package depgraph

// The dependency descriptor is the base→candidate runtime-dependency delta that is bound into the patch
// plan, the module manifest, the artifact identity, the signed metadata, the publish request, and the
// persisted-artifact verification. Everything that reads one back MUST go through DecodeStrict.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Upgrade records a package whose version or source identity changed between base and candidate.
type Upgrade struct {
	Name           string `json:"name"`
	FromVer        string `json:"from_version"`
	ToVer          string `json:"to_version"`
	FromSource     string `json:"from_source"`
	ToSource       string `json:"to_source"`
	FromSourceID   string `json:"from_source_id"`
	ToSourceID     string `json:"to_source_id"`
	FromContent    string `json:"from_content_hash,omitempty"`
	ToContent      string `json:"to_content_hash,omitempty"`
	ToEligible     bool   `json:"to_eligible"`
	ToCapabilityID string `json:"to_capability_digest"` // sha256 of the candidate capability classification

	// packageIdentityChanged also treats a changed pubspec hash or a changed dependency edge set as an
	// upgrade. Those two discriminators used to be detected but NOT recorded, which made the descriptor
	// unable to express a change its own builder had found: the record showed identical versions,
	// sources and content hashes, and DecodeStrict then rejected the whole descriptor with
	// "lists %q as upgraded but nothing about it changed". An ordinary `flutter pub get` that shifted a
	// transitive edge was enough to block EVERY subsequent patch for the project.
	//
	// A dependency descriptor is a security record: dropping the reason a package changed is exactly the
	// information a reviewer needs. These carry it.
	FromPubspecSHA string   `json:"from_pubspec_sha,omitempty"`
	ToPubspecSHA   string   `json:"to_pubspec_sha,omitempty"`
	FromDeps       []string `json:"from_dependencies,omitempty"`
	ToDeps         []string `json:"to_dependencies,omitempty"`
}

// Descriptor is the strict, immutable base→candidate dependency delta.
type Descriptor struct {
	Schema           string `json:"schema"`
	GeneratorVersion string `json:"generator_version"`

	RootPackage string `json:"root_package"`

	BasePubspecLockSHA      string `json:"base_pubspec_lock_sha256"`
	CandidatePubspecLockSHA string `json:"candidate_pubspec_lock_sha256"`
	BasePackageConfigSHA    string `json:"base_package_config_sha256"`
	CandPackageConfigSHA    string `json:"candidate_package_config_sha256"`
	BaseGraphDigest         string `json:"base_graph_digest"`
	CandidateGraphDigest    string `json:"candidate_graph_digest"`

	Added    []Package `json:"added"`
	Removed  []Package `json:"removed"`
	Upgraded []Upgrade `json:"upgraded"`

	// Unchanged is the NAME list, kept for readers and summaries.
	Unchanged []string `json:"unchanged_names"`

	// UnchangedPackages carries the immutable identity of each unchanged package.
	//
	// Names alone were not enough: a base_reference_only mode asserts "this is byte-for-byte the base's
	// package", and validating that claim requires the identity to compare against. With only a name
	// recorded, a fully rebound tamper could rewrite a delivery entry's version/source/identity_hash and
	// nothing in the descriptor could contradict it.
	UnchangedPackages []Package `json:"unchanged_packages"`

	// Ineligible lists added/upgraded packages that are NOT deliverable via a code-only OTA patch.
	Ineligible []IneligiblePackage `json:"ineligible"`

	// Delivery is the per-package DELIVERY MODE (carriable / base_reference_only / forbidden). It
	// separates "may this source be transported" from "may patched code reference this", which the
	// single eligible flag conflated. Bound into the descriptor digest and re-decided on read-back.
	Delivery []PackageDelivery `json:"delivery"`

	DescriptorDigest string `json:"descriptor_digest"` // sha256 over the canonical descriptor (excl. this field)
}

// IneligiblePackage is a refusal record: a changed package the code lane cannot deliver.
type IneligiblePackage struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Reasons []string `json:"reasons"`
	Message string   `json:"message"` // human-facing, store-release-actionable
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// BuildDescriptor computes the base→candidate delta and classifies the eligibility of every added or
// upgraded package. It does NOT decide acceptance — call Assess for that.
func BuildDescriptor(base, candidate Graph) Descriptor {
	d := Descriptor{
		Schema:                  DescriptorSchema,
		GeneratorVersion:        GeneratorVersion,
		RootPackage:             candidate.RootPackage,
		BasePubspecLockSHA:      base.PubspecLockSHA,
		CandidatePubspecLockSHA: candidate.PubspecLockSHA,
		BasePackageConfigSHA:    base.PackageConfigSHA,
		CandPackageConfigSHA:    candidate.PackageConfigSHA,
		BaseGraphDigest:         base.GraphDigest,
		CandidateGraphDigest:    candidate.GraphDigest,
	}
	for _, name := range sortedKeys(candidate.Packages) {
		cp := candidate.Packages[name]
		bp, inBase := base.Packages[name]
		switch {
		case !inBase:
			d.Added = append(d.Added, cp)
		case packageIdentityChanged(bp, cp):
			d.Upgraded = append(d.Upgraded, Upgrade{
				Name: name, FromVer: bp.Version, ToVer: cp.Version,
				FromSource: string(bp.Source), ToSource: string(cp.Source),
				FromSourceID: bp.SourceID, ToSourceID: cp.SourceID,
				FromContent: bp.identityHash(), ToContent: cp.identityHash(),
				FromPubspecSHA: bp.PubspecSHA, ToPubspecSHA: cp.PubspecSHA,
				FromDeps: bp.Dependencies, ToDeps: cp.Dependencies,
				ToEligible: cp.Capability.Eligible, ToCapabilityID: capabilityDigest(cp.Capability),
			})
		default:
			d.Unchanged = append(d.Unchanged, name)
			d.UnchangedPackages = append(d.UnchangedPackages, cp)
		}
	}
	for _, name := range sortedKeys(base.Packages) {
		if _, ok := candidate.Packages[name]; !ok {
			d.Removed = append(d.Removed, base.Packages[name])
		}
	}
	// Eligibility: every ADDED or UPGRADED package must be code-only deliverable.
	changed := map[string]Package{}
	for _, p := range d.Added {
		changed[p.Name] = p
	}
	for _, u := range d.Upgraded {
		changed[u.Name] = candidate.Packages[u.Name]
	}
	for _, name := range sortedKeys(changed) {
		p := changed[name]
		if !p.Capability.Eligible {
			d.Ineligible = append(d.Ineligible, IneligiblePackage{
				Name:    p.Name,
				Version: p.Version,
				Reasons: p.Capability.Reasons,
				Message: actionableMessage(p),
			})
		}
	}
	d.Delivery = classifyDelivery(base, candidate)
	sortDescriptor(&d)
	d.DescriptorDigest = d.computeDigest()
	return d
}

// packageIdentityChanged reports whether the SHIPPED identity of a package differs — not merely its
// version string. A repointed path/git package at the same version is still a change.
func packageIdentityChanged(a, b Package) bool {
	return a.Version != b.Version || a.Source != b.Source || a.SourceID != b.SourceID ||
		a.identityHash() != b.identityHash() || a.PubspecSHA != b.PubspecSHA ||
		strings.Join(a.Dependencies, ",") != strings.Join(b.Dependencies, ",")
}

// identityHash is a package's content pin: the hosted archive checksum, or the source-tree hash for
// path/git sources (which the lock does not check-sum).
func (p Package) identityHash() string {
	if p.ContentHash != "" {
		return p.ContentHash
	}
	return p.TreeHash
}

func capabilityDigest(c Capability) string {
	s := sha256.Sum256([]byte(c.digestString()))
	return hex.EncodeToString(s[:])
}

func actionableMessage(p Package) string {
	switch {
	case p.Capability.MetadataInconsistent:
		return fmt.Sprintf("%s %s has plugin metadata inconsistent with its contents (%s); it cannot be classified safely and requires a new App Store/Play Store release.", p.Name, p.Version, p.Capability.InconsistencyDetail)
	case p.Capability.HasBuildHook:
		return fmt.Sprintf("%s %s ships a native/FFI build hook (%s) that compiles native artifacts at build time; a code-only OTA patch cannot deliver those and it requires a new App Store/Play Store release.", p.Name, p.Version, p.Capability.BuildHookDetail)
	case p.Capability.HasNativePlugin:
		return fmt.Sprintf("%s %s introduces %s native platform plugin code and requires a new App Store/Play Store release (a code-only OTA patch cannot ship native binaries).", p.Name, p.Version, p.Capability.NativeDetail)
	case p.Capability.HasAssets:
		return fmt.Sprintf("%s %s ships packaged Flutter assets (%s) that a code-only OTA patch cannot deliver; it requires a new App Store/Play Store release that bundles them.", p.Name, p.Version, strings.Join(p.Capability.AssetDetail, ", "))
	default:
		return fmt.Sprintf("%s %s is not deliverable via a code-only OTA patch: %s. A new App Store/Play Store release is required.", p.Name, p.Version, strings.Join(p.Capability.Reasons, "; "))
	}
}

// Assess is THE authoritative deliverability gate. It is implemented over the delivery classification
// so that "is this patchable" has exactly one answer: a second entry point would eventually disagree
// with this one, and the disagreement would be silent.
//
// The delivery modes already cover every refusal the previous implementation enumerated separately --
// ineligible added/upgraded packages and removals are all ModeForbidden with the same actionable
// message -- so nothing is lost by routing through them.
func (d Descriptor) Assess() error {
	var lines []string
	for _, p := range d.Delivery {
		if p.Mode == ModeForbidden {
			lines = append(lines, "  - "+p.Reason)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("dependency change is not deliverable via a code-only OTA patch:\n%s",
		strings.Join(lines, "\n"))
}

// Changed reports whether the descriptor represents any dependency change at all.
func (d Descriptor) Changed() bool {
	return len(d.Added) > 0 || len(d.Removed) > 0 || len(d.Upgraded) > 0
}

// Summary is a short human summary of the change (for `soroq patch` output).
func (d Descriptor) Summary() string {
	var b strings.Builder
	for _, p := range d.Added {
		verdict := "Dart-only, eligible for OTA"
		if !p.Capability.Eligible {
			verdict = "NOT eligible — " + strings.Join(p.Capability.Reasons, "; ")
		}
		fmt.Fprintf(&b, "  + %s %s (%s): %s\n", p.Name, p.Version, p.Source, verdict)
	}
	for _, u := range d.Upgraded {
		fmt.Fprintf(&b, "  ^ %s %s -> %s\n", u.Name, u.FromVer, u.ToVer)
	}
	for _, p := range d.Removed {
		fmt.Fprintf(&b, "  - %s %s (removed)\n", p.Name, p.Version)
	}
	return b.String()
}

// DecodeStrict is the ONE production parser for a serialized descriptor. Every read site — patch plan,
// module manifest, artifact metadata, publish request, activation verification — must use it. It rejects
// unknown fields, trailing JSON, a wrong schema, malformed hashes, a digest that does not match the
// content, and any internally contradictory record set.
func DecodeStrict(raw []byte) (Descriptor, error) {
	var d Descriptor
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Descriptor{}, fmt.Errorf("decode dependency descriptor: %w", err)
	}
	if dec.More() {
		return Descriptor{}, errors.New("trailing data after dependency descriptor JSON")
	}
	if err := d.Validate(); err != nil {
		return Descriptor{}, err
	}
	return d, nil
}

// Validate rejects a descriptor that is malformed, mis-hashed, or self-contradictory.
func (d Descriptor) Validate() error {
	if d.Schema != DescriptorSchema {
		return fmt.Errorf("dependency descriptor schema %q != %q", d.Schema, DescriptorSchema)
	}
	if d.GeneratorVersion != GeneratorVersion {
		return fmt.Errorf("dependency descriptor generator_version %q != %q (a descriptor is only comparable within one generator version)", d.GeneratorVersion, GeneratorVersion)
	}
	if strings.TrimSpace(d.RootPackage) == "" {
		return errors.New("dependency descriptor missing root_package")
	}
	for name, h := range map[string]string{
		"base_pubspec_lock_sha256":        d.BasePubspecLockSHA,
		"candidate_pubspec_lock_sha256":   d.CandidatePubspecLockSHA,
		"base_package_config_sha256":      d.BasePackageConfigSHA,
		"candidate_package_config_sha256": d.CandPackageConfigSHA,
		"base_graph_digest":               d.BaseGraphDigest,
		"candidate_graph_digest":          d.CandidateGraphDigest,
		"descriptor_digest":               d.DescriptorDigest,
	} {
		if !sha256Re.MatchString(h) {
			return fmt.Errorf("dependency descriptor field %s is not a 64-hex sha256: %q", name, h)
		}
	}
	if err := d.validateRecords(); err != nil {
		return err
	}
	if got := d.computeDigest(); got != d.DescriptorDigest {
		return fmt.Errorf("dependency descriptor digest mismatch (tampered?): recorded %s != recomputed %s", short(d.DescriptorDigest), short(got))
	}
	// SEMANTIC validation, deliberately AFTER the digest check and independent of it. A fully rebound
	// tamper -- one that flips a delivery mode and recomputes every hash including the descriptor digest
	// and the artifact id -- produces a document whose hashes all agree. Only re-deciding the modes from
	// the delta the descriptor itself declares can catch that.
	if err := validateDelivery(d); err != nil {
		return fmt.Errorf("dependency descriptor delivery classification is invalid: %w", err)
	}
	return nil
}

// validateRecords rejects duplicate, missing, swapped and contradictory package records: a name may
// appear in exactly one category; every package record must be well-formed and hash-shaped; and the
// ineligible list must be exactly the set of added/upgraded packages whose OWN recorded classification
// says they are ineligible. That last check is what makes a partially-forged descriptor detectable:
// flipping `eligible` to true without deleting the ineligible entry (or vice versa) is a contradiction.
func (d Descriptor) validateRecords() error {
	category := map[string]string{}
	claim := func(name, cat string) error {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("dependency descriptor has an empty package name in %s", cat)
		}
		if prev, dup := category[name]; dup {
			if prev == cat {
				return fmt.Errorf("dependency descriptor lists %q twice in %s", name, cat)
			}
			return fmt.Errorf("dependency descriptor contradicts itself: %q appears in both %s and %s", name, prev, cat)
		}
		category[name] = cat
		return nil
	}
	validatePkg := func(p Package, cat string) error {
		if err := claim(p.Name, cat); err != nil {
			return err
		}
		if strings.TrimSpace(p.Version) == "" {
			return fmt.Errorf("dependency descriptor %s package %q has no version", cat, p.Name)
		}
		if strings.TrimSpace(p.SourceID) == "" {
			return fmt.Errorf("dependency descriptor %s package %q has no source_id", cat, p.Name)
		}
		if strings.Contains(p.SourceID, "/Users/") || strings.Contains(p.SourceID, "/home/") || strings.Contains(p.SourceID, `C:\`) {
			return fmt.Errorf("dependency descriptor %s package %q embeds a developer-local absolute path in source_id (%q)", cat, p.Name, p.SourceID)
		}
		for label, h := range map[string]string{"content_hash": p.ContentHash, "tree_hash": p.TreeHash, "pubspec_sha256": p.PubspecSHA} {
			if h != "" && !sha256Re.MatchString(h) {
				return fmt.Errorf("dependency descriptor %s package %q has a malformed %s: %q", cat, p.Name, label, h)
			}
		}
		if p.Source != SourceSDK && p.identityHash() == "" {
			return fmt.Errorf("dependency descriptor %s package %q has no content pin (neither a hosted content_hash nor a tree_hash)", cat, p.Name)
		}
		for _, e := range p.Dependencies {
			if strings.TrimSpace(e) == "" {
				return fmt.Errorf("dependency descriptor %s package %q has an empty dependency edge", cat, p.Name)
			}
		}
		if !sort.StringsAreSorted(p.Dependencies) {
			return fmt.Errorf("dependency descriptor %s package %q has non-canonical (unsorted) dependency edges", cat, p.Name)
		}
		return nil
	}

	for _, p := range d.Added {
		if err := validatePkg(p, "added"); err != nil {
			return err
		}
	}
	for _, p := range d.Removed {
		if err := validatePkg(p, "removed"); err != nil {
			return err
		}
	}
	for _, u := range d.Upgraded {
		if err := claim(u.Name, "upgraded"); err != nil {
			return err
		}
		if strings.TrimSpace(u.FromVer) == "" || strings.TrimSpace(u.ToVer) == "" {
			return fmt.Errorf("dependency descriptor upgrade %q is missing a version", u.Name)
		}
		// The no-op check must consider EVERY discriminator packageIdentityChanged uses. Checking a
		// subset rejected legitimate upgrades whose only change was a pubspec hash or a dependency edge.
		if u.FromVer == u.ToVer && u.FromSource == u.ToSource && u.FromSourceID == u.ToSourceID &&
			u.FromContent == u.ToContent && u.FromPubspecSHA == u.ToPubspecSHA &&
			strings.Join(u.FromDeps, ",") == strings.Join(u.ToDeps, ",") {
			return fmt.Errorf("dependency descriptor lists %q as upgraded but nothing about it changed", u.Name)
		}
		if u.ToCapabilityID != "" && !sha256Re.MatchString(u.ToCapabilityID) {
			return fmt.Errorf("dependency descriptor upgrade %q has a malformed to_capability_digest", u.Name)
		}
	}
	for _, n := range d.Unchanged {
		if err := claim(n, "unchanged"); err != nil {
			return err
		}
	}

	// Ineligible must be EXACTLY the added/upgraded packages whose own record says they are ineligible.
	wantIneligible := map[string]bool{}
	for _, p := range d.Added {
		if !p.Capability.Eligible {
			wantIneligible[p.Name] = true
		}
	}
	for _, u := range d.Upgraded {
		if !u.ToEligible {
			wantIneligible[u.Name] = true
		}
	}
	seen := map[string]bool{}
	for _, ip := range d.Ineligible {
		if seen[ip.Name] {
			return fmt.Errorf("dependency descriptor lists %q twice in ineligible", ip.Name)
		}
		seen[ip.Name] = true
		cat := category[ip.Name]
		if cat != "added" && cat != "upgraded" {
			return fmt.Errorf("dependency descriptor marks %q ineligible but it is not an added or upgraded package (category %q)", ip.Name, cat)
		}
		if !wantIneligible[ip.Name] {
			return fmt.Errorf("dependency descriptor contradicts itself: %q is listed ineligible but its own capability record says it is eligible", ip.Name)
		}
		if strings.TrimSpace(ip.Message) == "" {
			return fmt.Errorf("dependency descriptor ineligible entry %q has no actionable message", ip.Name)
		}
	}
	for name := range wantIneligible {
		if !seen[name] {
			return fmt.Errorf("dependency descriptor contradicts itself: %q is classified ineligible but is missing from the ineligible list (a refusal was suppressed)", name)
		}
	}
	return nil
}

// AssertMatchesCandidate verifies the descriptor was generated from THIS candidate project's freshly
// re-resolved runtime graph — the TOCTOU guard run before module generation, before publish, and again
// before an artifact is accepted. Because the graph is re-resolved from disk (capability recomputed from
// each real pubspec.yaml and package contents), a forged "eligible" claim cannot survive this check.
func (d Descriptor) AssertMatchesCandidate(candidate Graph) error {
	if d.RootPackage != candidate.RootPackage {
		return fmt.Errorf("descriptor/candidate root package mismatch: descriptor %q != actual %q", d.RootPackage, candidate.RootPackage)
	}
	if d.CandidatePubspecLockSHA != candidate.PubspecLockSHA {
		return fmt.Errorf("descriptor/candidate pubspec.lock mismatch (dependencies changed during the patch): descriptor %s != actual %s", short(d.CandidatePubspecLockSHA), short(candidate.PubspecLockSHA))
	}
	if d.CandPackageConfigSHA != candidate.PackageConfigSHA {
		return fmt.Errorf("descriptor/candidate package_config mismatch (TOCTOU): descriptor %s != actual %s", short(d.CandPackageConfigSHA), short(candidate.PackageConfigSHA))
	}
	if d.CandidateGraphDigest != candidate.GraphDigest {
		return fmt.Errorf("descriptor/candidate runtime dependency-graph digest mismatch (TOCTOU): descriptor %s != actual %s", short(d.CandidateGraphDigest), short(candidate.GraphDigest))
	}
	if recomputed := candidate.RecomputeDigest(); recomputed != candidate.GraphDigest {
		return fmt.Errorf("candidate graph digest is internally inconsistent: recorded %s != recomputed %s", short(candidate.GraphDigest), short(recomputed))
	}
	return nil
}

// AssertMatchesBase verifies the descriptor's base side against the immutable base graph recorded in the
// release baseline. This is the second, independent semantic anchor: without it, a fully-rebound
// descriptor (digest and all outer hashes recomputed) would be self-consistent and undetectable.
func (d Descriptor) AssertMatchesBase(baseGraphDigest, basePubspecLockSHA, basePackageConfigSHA string) error {
	if d.BaseGraphDigest != baseGraphDigest {
		return fmt.Errorf("descriptor base graph digest %s does not match the immutable base release's recorded dependency graph %s — the descriptor was not generated against this base", short(d.BaseGraphDigest), short(baseGraphDigest))
	}
	if d.BasePubspecLockSHA != basePubspecLockSHA {
		return fmt.Errorf("descriptor base pubspec.lock %s does not match the base release's recorded %s", short(d.BasePubspecLockSHA), short(basePubspecLockSHA))
	}
	if d.BasePackageConfigSHA != basePackageConfigSHA {
		return fmt.Errorf("descriptor base package_config %s does not match the base release's recorded %s", short(d.BasePackageConfigSHA), short(basePackageConfigSHA))
	}
	return nil
}

// RecomputeDescriptorDigest re-derives a descriptor's canonical digest from its content. It exists so
// verification code (and adversarial tests that model a FULLY REBOUND forgery) can recompute the digest
// the same way the builder does — the digest is a consistency check, never the security boundary. The
// real boundaries are Validate's contradiction checks plus the base/candidate anchors.
func RecomputeDescriptorDigest(d Descriptor) string { return d.computeDigest() }

// computeDigest is an explicit canonical serialization — not a struct marshal — so the digest does not
// silently change when a field is reordered, and so every semantically load-bearing value is covered.
func (d Descriptor) computeDigest() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n", d.Schema, d.GeneratorVersion, d.RootPackage)
	fmt.Fprintf(h, "base\x00%s\x00%s\x00%s\n", d.BasePubspecLockSHA, d.BasePackageConfigSHA, d.BaseGraphDigest)
	fmt.Fprintf(h, "cand\x00%s\x00%s\x00%s\n", d.CandidatePubspecLockSHA, d.CandPackageConfigSHA, d.CandidateGraphDigest)
	writePkg := func(tag string, p Package) {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\n",
			tag, p.Name, p.Version, p.Source, p.SourceID, p.ContentHash, p.GitCommit, p.TreeHash,
			strings.Join(p.Dependencies, ","), p.Capability.digestString())
	}
	for _, p := range d.Added {
		writePkg("added", p)
	}
	for _, p := range d.Removed {
		writePkg("removed", p)
	}
	for _, u := range d.Upgraded {
		fmt.Fprintf(h, "upgraded\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%t\x00%s\n",
			u.Name, u.FromVer, u.ToVer, u.FromSource, u.ToSource, u.FromSourceID, u.ToSourceID,
			u.FromContent, u.ToContent, u.ToEligible, u.ToCapabilityID)
	}
	fmt.Fprintf(h, "unchanged\x00%s\n", strings.Join(d.Unchanged, ","))
	for _, p := range d.UnchangedPackages {
		writePkg("unchanged_pkg", p)
	}
	for _, ip := range d.Ineligible {
		fmt.Fprintf(h, "ineligible\x00%s\x00%s\x00%s\x00%s\n", ip.Name, ip.Version, strings.Join(ip.Reasons, ";"), ip.Message)
	}
	// Delivery modes are digest-bound: editing a mode without editing the digest is detectable by hash,
	// and editing both is detectable by validateDelivery re-deciding from the delta.
	for _, pd := range d.Delivery {
		fmt.Fprintf(h, "delivery\x00%s\n", pd.digestString())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortDescriptor(d *Descriptor) {
	sort.Slice(d.Delivery, func(i, j int) bool { return d.Delivery[i].Name < d.Delivery[j].Name })
	sort.Slice(d.UnchangedPackages, func(i, j int) bool { return d.UnchangedPackages[i].Name < d.UnchangedPackages[j].Name })
	sort.Slice(d.Added, func(i, j int) bool { return d.Added[i].Name < d.Added[j].Name })
	sort.Slice(d.Removed, func(i, j int) bool { return d.Removed[i].Name < d.Removed[j].Name })
	sort.Slice(d.Upgraded, func(i, j int) bool { return d.Upgraded[i].Name < d.Upgraded[j].Name })
	sort.Strings(d.Unchanged)
	sort.Slice(d.Ineligible, func(i, j int) bool { return d.Ineligible[i].Name < d.Ineligible[j].Name })
}

func short(h string) string {
	if len(h) >= 12 {
		return h[:12]
	}
	return h
}
