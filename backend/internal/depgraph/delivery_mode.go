package depgraph

// DELIVERY MODE — how a runtime dependency may participate in a code-only OTA patch.
//
// The previous model had ONE axis (eligible / not), which conflated two genuinely different questions:
//
//	"may this package's Dart source be TRANSPORTED in the module?"   -- carriage
//	"may patched code REFERENCE this package at all?"                -- reference
//
// A native plugin already embedded in the immutable store base answers no to the first and yes to the
// second: its Dart interface is compiled into the base the device is already running, so referencing it
// ships nothing new. Conflating the two forced an all-or-nothing choice, and the only escape would have
// been a package-name exemption -- which is exactly what must not exist, because it would silently
// widen to any package that happened to be spelled the same way.
//
// The three modes are decided ONLY from evidence already bound into the graph (identity, source kind,
// checksums, capability classification). No package name is ever consulted.

import (
	"fmt"
	"sort"
	"strings"
)

// DeliveryMode is the schema-bound classification of one runtime dependency.
type DeliveryMode string

const (
	// ModeCarriable: an ADDED or UPGRADED eligible Dart-only package. Its source may be carried into
	// the module and it may be referenced.
	ModeCarriable DeliveryMode = "carriable"

	// ModeBaseReferenceOnly: a package whose identity is byte-for-byte the base's. Patched code may
	// REFERENCE it -- the base already contains it -- but its source must never be carried or
	// upgraded. This is what lets an unchanged native plugin coexist with a Dart-only patch without
	// naming it.
	ModeBaseReferenceOnly DeliveryMode = "base_reference_only"

	// ModeForbidden: the change cannot be delivered by a code-only patch at all. Requires a new store
	// release.
	ModeForbidden DeliveryMode = "forbidden"
)

// Valid reports whether m is one of the three defined modes. Anything else is a decode failure, not a
// default: an unknown mode must never be silently treated as permissive.
func (m DeliveryMode) Valid() bool {
	switch m {
	case ModeCarriable, ModeBaseReferenceOnly, ModeForbidden:
		return true
	}
	return false
}

// PackageDelivery is the per-package delivery decision, bound into the descriptor and the artifact
// identity so a forged mode cannot survive verification.
type PackageDelivery struct {
	Name string       `json:"name"`
	Mode DeliveryMode `json:"mode"`

	// Identity is the exact package pin the mode was decided against: source kind, source id and
	// content/tree hash. A base_reference_only claim is only meaningful against a specific base
	// identity, so the identity travels with the mode rather than being re-derived by the reader.
	Version    string `json:"version"`
	Source     string `json:"source"`
	SourceID   string `json:"source_id"`
	IdentityID string `json:"identity_hash"`

	// CapabilityID pins the capability classification the decision used, so flipping a capability
	// without flipping the mode is detectable.
	CapabilityID string `json:"capability_digest"`

	// Reason is human-facing and store-release-actionable for ModeForbidden; empty otherwise.
	Reason string `json:"reason,omitempty"`
}

// digestString is the canonical, order-stable serialisation bound into the descriptor digest.
func (p PackageDelivery) digestString() string {
	return fmt.Sprintf("%s|mode=%s|ver=%s|src=%s|srcid=%s|id=%s|cap=%s|reason=%s",
		p.Name, p.Mode, p.Version, p.Source, p.SourceID, p.IdentityID, p.CapabilityID, p.Reason)
}

// classifyDelivery assigns a mode to every package in the union of base and candidate.
//
// The rules are deliberately exhaustive over the delta, not over package properties: a package is
// classified by WHAT HAPPENED TO IT between base and candidate, and only then by whether that change is
// deliverable. That is why an unchanged native plugin lands in base_reference_only without anything
// knowing it is a plugin, and why a plugin that moved hosted->path lands in forbidden without anything
// knowing its name.
func classifyDelivery(base, candidate Graph) []PackageDelivery {
	out := make([]PackageDelivery, 0, len(candidate.Packages)+len(base.Packages))

	rec := func(name string, p Package, mode DeliveryMode, reason string) {
		out = append(out, PackageDelivery{
			Name: name, Mode: mode, Version: p.Version,
			Source: string(p.Source), SourceID: p.SourceID,
			IdentityID: p.identityHash(), CapabilityID: capabilityDigest(p.Capability),
			Reason: reason,
		})
	}

	for _, name := range sortedKeys(candidate.Packages) {
		cp := candidate.Packages[name]
		bp, inBase := base.Packages[name]

		switch {
		case !inBase:
			// ADDED. Deliverable only if it is code-only.
			if cp.Capability.Eligible {
				rec(name, cp, ModeCarriable, "")
			} else {
				rec(name, cp, ModeForbidden, actionableMessage(cp))
			}

		case packageIdentityChanged(bp, cp):
			// UPGRADED -- and "upgraded" includes a hosted->path repoint at the same version, a
			// source-id change, a checksum/tree-hash change, a pubspec change and a dependency-edge
			// change, because packageIdentityChanged covers all of them.
			if cp.Capability.Eligible {
				rec(name, cp, ModeCarriable, "")
			} else {
				rec(name, cp, ModeForbidden, actionableMessage(cp))
			}

		default:
			// UNCHANGED. The base already ships this exact package, so its interface is present in the
			// immutable base kernel. It may be referenced, never carried -- regardless of whether it is
			// a native plugin.
			rec(name, cp, ModeBaseReferenceOnly, "")
		}
	}

	// REMOVED. The installed base still contains and may reference the code, so removal is not
	// patchable at all.
	for _, name := range sortedKeys(base.Packages) {
		if _, ok := candidate.Packages[name]; !ok {
			bp := base.Packages[name]
			rec(name, bp, ModeForbidden,
				fmt.Sprintf("%s %s was removed from the runtime dependency graph; the installed base still "+
					"contains and may reference its code, so removal is not patchable — a new App Store/Play "+
					"Store release is required.", name, bp.Version))
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CarriablePackages returns the names whose source may be transported. Everything else must be either
// referenced from the base or refused; the synthesiser uses this as its allowed carriage set.
func (d Descriptor) CarriablePackages() []string {
	var out []string
	for _, p := range d.Delivery {
		if p.Mode == ModeCarriable {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out
}

// BaseReferencePackages returns the names that may be referenced but never carried.
func (d Descriptor) BaseReferencePackages() []string {
	var out []string
	for _, p := range d.Delivery {
		if p.Mode == ModeBaseReferenceOnly {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out
}

// validateDelivery is the SEMANTIC check run on read-back. It exists because a fully rebound tamper --
// one that edits a mode and recomputes every hash and the artifact id -- produces a document whose
// digests all agree. Only re-deciding the modes from the graphs the descriptor itself names can catch
// that, so this recomputes the classification and requires an exact match.
func validateDelivery(d Descriptor) error {
	seen := map[string]bool{}
	for _, p := range d.Delivery {
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("delivery entry with an empty package name")
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate delivery entry for %q", p.Name)
		}
		seen[p.Name] = true
		if !p.Mode.Valid() {
			return fmt.Errorf("package %q has unknown delivery mode %q", p.Name, p.Mode)
		}
		if p.Mode == ModeForbidden && strings.TrimSpace(p.Reason) == "" {
			return fmt.Errorf("package %q is forbidden but carries no actionable reason", p.Name)
		}
		if p.Mode != ModeForbidden && strings.TrimSpace(p.Reason) != "" {
			return fmt.Errorf("package %q is %s but carries a refusal reason", p.Name, p.Mode)
		}
	}
	// Cross-check against the delta the descriptor itself declares. A mode that disagrees with the
	// Added/Upgraded/Removed/Unchanged lists is a forged classification, whatever its digest says.
	byName := map[string]PackageDelivery{}
	for _, p := range d.Delivery {
		byName[p.Name] = p
	}
	requireMode := func(name string, want DeliveryMode, why string) error {
		got, ok := byName[name]
		if !ok {
			return fmt.Errorf("%s %q has no delivery entry", why, name)
		}
		if want == ModeBaseReferenceOnly && got.Mode != ModeBaseReferenceOnly {
			return fmt.Errorf("%s %q must be %s, got %s", why, name, want, got.Mode)
		}
		if want == ModeForbidden && got.Mode != ModeForbidden {
			return fmt.Errorf("%s %q must be %s, got %s", why, name, want, got.Mode)
		}
		if want == ModeCarriable && got.Mode == ModeBaseReferenceOnly {
			return fmt.Errorf("%s %q changed, so it cannot be %s", why, name, ModeBaseReferenceOnly)
		}
		return nil
	}
	for _, n := range d.Unchanged {
		if err := requireMode(n, ModeBaseReferenceOnly, "unchanged package"); err != nil {
			return err
		}
	}
	for _, p := range d.Added {
		if err := requireMode(p.Name, ModeCarriable, "added package"); err != nil {
			return err
		}
	}
	for _, u := range d.Upgraded {
		if err := requireMode(u.Name, ModeCarriable, "upgraded package"); err != nil {
			return err
		}
	}
	for _, p := range d.Removed {
		if err := requireMode(p.Name, ModeForbidden, "removed package"); err != nil {
			return err
		}
	}
	// EXACT SET EQUALITY, both directions. Checking only that each delta entry HAS a mode leaves the
	// converse open: an extra entry for a package in neither graph would be accepted, and a reader that
	// trusts the delivery list (rather than re-deriving it) could be steered by it.
	expected := map[string]bool{}
	for _, n := range d.Unchanged {
		expected[n] = true
	}
	for _, p := range d.UnchangedPackages {
		expected[p.Name] = true
	}
	for _, p := range d.Added {
		expected[p.Name] = true
	}
	for _, u := range d.Upgraded {
		expected[u.Name] = true
	}
	for _, p := range d.Removed {
		expected[p.Name] = true
	}
	for _, p := range d.Delivery {
		if !expected[p.Name] {
			return fmt.Errorf("delivery entry %q names a package that is not in the added/upgraded/"+
				"removed/unchanged delta; the classification does not describe this descriptor", p.Name)
		}
	}
	if len(d.Delivery) != len(expected) {
		return fmt.Errorf("delivery has %d entries but the delta names %d packages",
			len(d.Delivery), len(expected))
	}

	// FIELD-LEVEL SEMANTIC CROSS-CHECK.
	//
	// The identity fields are digest-bound, which stops a naive edit -- but a FULLY REBOUND tamper
	// recomputes the digest too, and then nothing disagrees. Each field must therefore be compared
	// against its AUTHORITATIVE record in the delta, so a rewritten version/source/identity is
	// contradicted by the very document that carries it.
	//
	//   added     -> the candidate Package record
	//   removed   -> the base Package record
	//   upgraded  -> the candidate-side identity recorded in the Upgrade
	//   unchanged -> the preserved UnchangedPackages identity
	checkAgainst := func(what, name, field, got, want string) error {
		if got != want {
			return fmt.Errorf("%s %q delivery %s %q does not match its %s record (%q); the "+
				"classification does not describe this package", what, name, field, got, what, want)
		}
		return nil
	}
	checkPkg := func(what string, p Package) error {
		e, ok := byName[p.Name]
		if !ok {
			return fmt.Errorf("%s %q has no delivery entry", what, p.Name)
		}
		for _, c := range []struct{ field, got, want string }{
			{"version", e.Version, p.Version},
			{"source", e.Source, string(p.Source)},
			{"source_id", e.SourceID, p.SourceID},
			{"identity_hash", e.IdentityID, p.identityHash()},
			{"capability_digest", e.CapabilityID, capabilityDigest(p.Capability)},
		} {
			if err := checkAgainst(what, p.Name, c.field, c.got, c.want); err != nil {
				return err
			}
		}
		return nil
	}
	for _, p := range d.Added {
		if err := checkPkg("added package", p); err != nil {
			return err
		}
	}
	for _, p := range d.Removed {
		if err := checkPkg("removed package", p); err != nil {
			return err
		}
	}
	for _, p := range d.UnchangedPackages {
		if err := checkPkg("unchanged package", p); err != nil {
			return err
		}
	}
	for _, u := range d.Upgraded {
		e, ok := byName[u.Name]
		if !ok {
			return fmt.Errorf("upgraded package %q has no delivery entry", u.Name)
		}
		for _, c := range []struct{ field, got, want string }{
			{"version", e.Version, u.ToVer},
			{"source", e.Source, u.ToSource},
			{"source_id", e.SourceID, u.ToSourceID},
			{"identity_hash", e.IdentityID, u.ToContent},
			{"capability_digest", e.CapabilityID, u.ToCapabilityID},
		} {
			if err := checkAgainst("upgraded package", u.Name, c.field, c.got, c.want); err != nil {
				return err
			}
		}
	}
	// The unchanged NAME list and the unchanged IDENTITY list must describe the same set, or one of
	// them was edited.
	if len(d.Unchanged) != len(d.UnchangedPackages) {
		return fmt.Errorf("unchanged names (%d) and unchanged package identities (%d) disagree",
			len(d.Unchanged), len(d.UnchangedPackages))
	}
	for i, n := range d.Unchanged {
		if d.UnchangedPackages[i].Name != n {
			return fmt.Errorf("unchanged entry %d names %q but its identity record names %q",
				i, n, d.UnchangedPackages[i].Name)
		}
	}

	// Every ineligible package must be forbidden: an ineligible package classified carriable would let
	// a native/asset/build-hook change ride a code-only patch.
	for _, ip := range d.Ineligible {
		got, ok := byName[ip.Name]
		if !ok {
			return fmt.Errorf("ineligible package %q has no delivery entry", ip.Name)
		}
		if got.Mode != ModeForbidden {
			return fmt.Errorf("ineligible package %q is classified %s; it must be %s",
				ip.Name, got.Mode, ModeForbidden)
		}
	}
	return nil
}
