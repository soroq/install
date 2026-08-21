package main

// Soroq freehand — immutable, versioned release-baseline persistence.
//
// The mutable .dart_tool/flutter_build/<hash>/app.dill is NEVER the long-term baseline: a later
// build overwrites it (we lost a TestFlight baseline that way). After the exact app.dill that
// gen_snapshot consumed is known, we atomically snapshot it under a per-release directory:
//
//   .soroq/releases/<runtime-id>/
//     app.dill              byte copy of the exact analyzed/consumed kernel (customer code: 0600)
//     soroq_app_manifest.txt   auto-discovered patchable identities
//     symbol_graph.json     identity schema v1 graph
//     baseline.json         full provenance (schema/analyzer/hashes/revisions/ids) — NO secrets
//
// Writes are temp-dir -> private perms -> fsync -> atomic rename -> fsync parent. A baseline for a
// runtime-id is IMMUTABLE across EVERY immutable input (app.dill/manifest/graph/config hashes,
// identity-schema, analyzer, tool revisions, and app identity). An identical re-run is idempotent;
// any differing immutable input fails closed (a changed baseline needs a new runtime-id). Existing
// baselines are fully re-validated (strict JSON, required fields, rehash, no symlinks) before reuse.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"soroq/backend/internal/depgraph"
)

// FreehandBaselineMeta is the provenance recorded in baseline.json. It MUST NOT hold secrets
// (no signing seeds, no operator tokens).
// FreehandRetentionEvidence is the immutable proof that this base app was built with freehand
// symbol RETENTION — i.e. the patched frontend's build-DAG target ran automatic discovery, generated
// the manifest, and injected `--soroq_manifest=<path>` into the exact gen_snapshot that produced the
// AOT app.dill so the patchable identities survive tree-shaking and are resolvable at runtime by
// identity. Without this, on-device redirects fail with "new base identity not found". A plain/reused
// `flutter build` produces NO analysis staging, so this evidence cannot be forged: it is populated
// ONLY after verifyFreehandStagingStrict validates the staging against the live app.dill/analyzer/
// package-config, and the baseline write + patch flow REFUSE a base whose evidence is absent, unverified,
// empty, or inconsistent with the recorded manifest.
type FreehandRetentionEvidence struct {
	Verified           bool   `json:"verified"`            // staging validated against live inputs (build-DAG retention ran)
	RetainedIdentities int    `json:"retained_identities"` // count passed to --soroq_manifest (== patchable_symbols)
	ManifestSHA256     string `json:"manifest_sha256"`     // sha of the exact --soroq_manifest bytes gen_snapshot consumed
	SymbolGraphSHA256  string `json:"symbol_graph_sha256"` // sha of the analysis symbol graph
	AnalysisID         string `json:"analysis_id"`         // content-address of the verified analysis staging
}

type FreehandBaselineMeta struct {
	Schema          string `json:"schema"`           // "soroq.freehand.baseline.v2"
	IdentitySchema  string `json:"identity_schema"`  // "soroq.freehand.identity.v1"
	AnalyzerVersion string `json:"analyzer_version"` // analyzer snapshot sha/version
	AppDillSHA256   string `json:"app_dill_sha256"`
	// v2 dual-kernel: the non-AOT/source-fidelity companion used ONLY for semantic diff. app.dill above
	// remains the AOT runtime import-dill kernel.
	SourceAppDillSHA256 string                      `json:"source_app_dill_sha256"`
	SourceKernelRecipe  *FreehandSourceKernelRecipe `json:"source_kernel_recipe"`
	SourceRecipeDigest  string                      `json:"source_kernel_recipe_digest"`
	ManifestSHA256      string                      `json:"manifest_sha256"`
	GraphSHA256         string                      `json:"symbol_graph_sha256"`
	PackageConfigSHA256 string                      `json:"package_config_sha256"`
	FrontendRev         string                      `json:"frontend_revision"`
	FrontendPatchsetSHA string                      `json:"frontend_patchset_sha256"`
	FrameworkRev        string                      `json:"framework_revision"`
	DartRev             string                      `json:"dart_revision"`
	EngineRev           string                      `json:"engine_revision"`
	AppID               string                      `json:"app_id"`
	Version             string                      `json:"version"`
	RuntimeID           string                      `json:"runtime_id"`
	Arch                string                      `json:"arch"`
	Channel             string                      `json:"channel"`
	PatchableCount      int                         `json:"patchable_symbols"`
	// Dependency-OTA support: the IMMUTABLE runtime dependency graph this base shipped with, persisted
	// verbatim beside the baseline as dependency_graph.json. It is the base-side anchor a patch's
	// dependency descriptor is checked against — without it a fully-rebound descriptor would be
	// self-consistent and undetectable. The three fields bind, respectively, the format, the file bytes,
	// and the graph's own canonical content digest; all three are re-derived on every verification.
	DependencyGraphSchema string `json:"dependency_graph_schema"`
	DependencyGraphSHA256 string `json:"dependency_graph_sha256"`
	DependencyGraphDigest string `json:"dependency_graph_digest"`
	// DependencyLockSHA256 / DependencyPackageConfigSHA256 are the resolution inputs the base graph came
	// from, recorded so a descriptor's base side can be checked without re-reading the graph file.
	DependencyLockSHA256          string `json:"dependency_pubspec_lock_sha256"`
	DependencyPackageConfigSHA256 string `json:"dependency_package_config_sha256"`
	// The FREEHAND BASE CONTRACT this base was built with: the dynamic interface that decides what a
	// future patch module may call, extend and use as a type. A patch compiled against a different
	// contract than the base retained is not deliverable, so the schema + digest are bound here and
	// re-checked on every patch.
	ContractSchema string `json:"contract_schema"`
	ContractDigest string `json:"contract_digest"`
	// Retention is the load-bearing freehand-retention evidence; a base without it is refused at both
	// release registration and patch time (see requireFreehandRetention).
	Retention *FreehandRetentionEvidence `json:"retention"`
	// RedirectCapabilities is what THIS base's engine can honour end to end (see the block comment on
	// FreehandRedirectCapabilities). It is DERIVED at persist time from the engine bundle, never taken
	// from the caller, and it is deliberately absent from requiredBaselineFields and from
	// immutableInputsEqual: every baseline written before this guard existed has no such field, and a
	// baseline that is otherwise byte-identical must still re-register idempotently. Absent is read as
	// legacy-default, never as "allow every kind".
	RedirectCapabilities *FreehandRedirectCapabilities `json:"redirect_capabilities,omitempty"`
}

// ---------------------------------------------------------------------------------------------
// WHAT THIS BASE'S ENGINE CAN ACTUALLY HONOUR.
//
// A freehand patch carrying three constructor identities was published, downloaded, signature-verified,
// staged and committed on a real iPhone -- `transition.begin requested=4`, `transition.result
// committed=4`, `active state=patched` -- and every one of those constructor redirects changed nothing
// observable. The app rendered base values across three cold starts. Every layer reported success.
//
// Every existing guard passed, and each of them was RIGHT to pass: the analyzer emitted the VM's exact
// names, the strict ABI verifier accepted a bijection with measured shapes, the signature and payload
// hashes verified, and the engine resolved both ends and set the slot. Nothing anywhere asked the only
// question that decides whether the patch does anything: can this base's ENGINE honour a redirect on
// this KIND of identity -- not merely resolve one? The engine answers "I set the slot", never "the
// running code will consult it".
//
// A patch that installs cleanly and does nothing is the worst failure mode this system has. A crash is
// loud, a refusal is loud, a signature mismatch is loud; a silent no-op leaves the developer with no
// signal at all except that the bug they shipped a fix for is still there.
//
// So the capability is DATA CARRIED BY THE BASE, resolved from the engine bundle that built it. It is
// not a rule in code: an engine build that demonstrates constructor support declares it, the next base
// records the declaration, and the SAME producer code ships constructor identities with no change here.
// A hard-coded unsupported-kind list would have to be edited to unlock them, which is how a temporary
// measurement becomes a permanent limit.
// ---------------------------------------------------------------------------------------------

// freehandRedirectCapabilitiesSchema tags the recorded encoding so a future capability format cannot be
// misread as this one.
const freehandRedirectCapabilitiesSchema = "soroq.freehand.redirect_capabilities.v1"

// The two recognized provenances of a recorded capability set. Anything else is a malformed record and
// fails closed -- an unrecognized source would otherwise let a hand-written baseline assert capabilities
// no engine ever demonstrated.
const (
	freehandCapabilitySourceLegacy = "legacy-default"
	freehandCapabilitySourceEngine = "engine-declared"
)

// freehandEngineCapabilityKey is the engine.json key a FUTURE engine bundle declares its honoured
// redirect kinds under. No engine ships it today; that absence is exactly why legacy-default exists.
const freehandEngineCapabilityKey = "soroq_freehand_redirect_capabilities"

// legacyDefaultRedirectKinds are the kinds that were demonstrably shipping before this tranche: they are
// the method-shaped identities whose redirects have been observed to take effect on device. They are the
// VALUE recorded onto a base that predates any engine declaration -- not a rule applied at patch time.
// The guard reads the base's recorded set; this constant only seeds it.
//
// constructor, factory and field-initializer are absent because the device transcript above shows their
// redirects committing and doing nothing, and because SoroqResolveByIdentity cannot resolve
// `init:<field>` at all (a field's initializer is not a class member). An engine build that fixes either
// declares the kind and unlocks it without touching this file.
var legacyDefaultRedirectKinds = []string{"function", "getter", "method", "operator", "setter", "static-method"}

// FreehandRedirectCapabilities is the recorded, per-base answer to "which SEMANTIC identity kinds can
// this base's engine honour end to end?". Semantic kinds are the frozen-identity kinds (the third
// segment of a v1 stable identity), never the three ABI shape labels the runtime type-checks.
type FreehandRedirectCapabilities struct {
	Schema string `json:"schema"`
	// Source is where the set came from: an engine that declared it, or the pre-declaration default.
	Source string `json:"source"`
	// EngineRevision is the engine the set describes (the baseline's own engine_revision).
	EngineRevision string `json:"engine_revision"`
	// HonouredKinds is the set itself: sorted, de-duplicated, non-empty.
	HonouredKinds []string `json:"honoured_kinds"`
	// Note says IN THE RECORDED VALUE why the set is what it is, so someone reading a baseline.json years
	// from now learns whether an engine declared this or nobody had measured it yet.
	Note string `json:"note"`
}

// kindSet is the membership test the publish-time guard uses.
func (c *FreehandRedirectCapabilities) kindSet() map[string]bool {
	m := make(map[string]bool, len(c.HonouredKinds))
	for _, k := range c.HonouredKinds {
		m[k] = true
	}
	return m
}

func (c *FreehandRedirectCapabilities) kindList() string { return strings.Join(c.HonouredKinds, ", ") }

// legacyDefaultRedirectCapabilities builds the record for a base whose engine declared nothing. The
// reason is written INTO the value: a future reader must be able to tell "no engine had declared this
// yet" apart from "an engine measured exactly these kinds".
func legacyDefaultRedirectCapabilities(engineRev, why string) *FreehandRedirectCapabilities {
	kinds := append([]string(nil), legacyDefaultRedirectKinds...)
	sort.Strings(kinds)
	return &FreehandRedirectCapabilities{
		Schema:         freehandRedirectCapabilitiesSchema,
		Source:         freehandCapabilitySourceLegacy,
		EngineRevision: engineRev,
		HonouredKinds:  kinds,
		Note: "no engine declared " + freehandEngineCapabilityKey + " (" + why + "); recorded the kinds that " +
			"were demonstrably shipping before the capability guard existed. constructor/factory/field-initializer " +
			"redirects were observed to COMMIT and change nothing on device, so they are withheld until an engine " +
			"build declares them.",
	}
}

// validateFreehandRedirectCapabilities fails closed on any record that is not fully well-formed. A
// malformed capability record must never be read as permissive: a record nobody can parse is a base
// nobody has measured, and the whole point of the guard is that unmeasured means refused.
func validateFreehandRedirectCapabilities(c *FreehandRedirectCapabilities) error {
	if c == nil {
		return errors.New("no redirect-capability record")
	}
	if c.Schema != freehandRedirectCapabilitiesSchema {
		return fmt.Errorf("baseline redirect_capabilities has schema %q, want %q — refusing to read an unrecognized capability format as permissive", c.Schema, freehandRedirectCapabilitiesSchema)
	}
	if c.Source != freehandCapabilitySourceLegacy && c.Source != freehandCapabilitySourceEngine {
		return fmt.Errorf("baseline redirect_capabilities has unknown source %q (want %q or %q); a capability set with no stated provenance is not evidence of anything",
			c.Source, freehandCapabilitySourceEngine, freehandCapabilitySourceLegacy)
	}
	if strings.TrimSpace(c.EngineRevision) == "" {
		return errors.New("baseline redirect_capabilities names no engine_revision, so it describes no engine")
	}
	if len(c.HonouredKinds) == 0 {
		return errors.New("baseline redirect_capabilities declares ZERO honoured kinds; no patch could ever be published against this base")
	}
	seen := map[string]bool{}
	for _, k := range c.HonouredKinds {
		if !freehandSemanticKinds[k] {
			return fmt.Errorf("baseline redirect_capabilities declares kind %q, which is not a frozen semantic identity kind (%s)", k, freehandSemanticKindList())
		}
		if seen[k] {
			return fmt.Errorf("baseline redirect_capabilities lists kind %q twice", k)
		}
		seen[k] = true
	}
	return nil
}

// baseRedirectCapabilities is the ONLY reader of a base's capability set. Absent means legacy-default —
// every baseline written before this guard existed has no field, and reading absence as "allow all"
// would reinstate the silent no-op for exactly the bases that already shipped with it.
func baseRedirectCapabilities(m *FreehandBaselineMeta) (*FreehandRedirectCapabilities, error) {
	if m == nil {
		return nil, errors.New("no baseline: cannot read its redirect capabilities")
	}
	if m.RedirectCapabilities == nil {
		return legacyDefaultRedirectCapabilities(m.EngineRev, "baseline predates the capability record"), nil
	}
	if err := validateFreehandRedirectCapabilities(m.RedirectCapabilities); err != nil {
		return nil, err
	}
	return m.RedirectCapabilities, nil
}

// deriveFreehandRedirectCapabilities resolves the capability set from the ENGINE that built this base,
// by finding the installed engine bundle whose soroq_engine_revision equals the baseline's own.
//
// The revision is the join key because the release path cannot hand the toolchain version down here, and
// because the revision is precisely what identifies the engine binary: two toolchain directories built
// from the same engine must declare the same thing, and if they disagree we refuse rather than pick one.
//
// Resolution FAILURES degrade to legacy-default (no store, no match, unreadable directory) because that
// direction only ever allows FEWER kinds. A matching engine whose declaration is present but malformed
// is an ERROR: silently downgrading a declaration nobody could parse would hide the one case where an
// engine tried to say something and the producer misread it.
func deriveFreehandRedirectCapabilities(engineRev string) (*FreehandRedirectCapabilities, error) {
	root, err := toolchainsRoot()
	if err != nil {
		return legacyDefaultRedirectCapabilities(engineRev, "no toolchain store on this machine"), nil
	}
	return resolveRedirectCapabilitiesFromToolchains(root, engineRev)
}

// resolveRedirectCapabilitiesFromToolchains is deriveFreehandRedirectCapabilities with the store root
// injected, so the resolution can be exercised against a fixture store.
func resolveRedirectCapabilitiesFromToolchains(root, engineRev string) (*FreehandRedirectCapabilities, error) {
	if strings.TrimSpace(engineRev) == "" {
		return nil, errors.New("cannot resolve redirect capabilities: the baseline records no engine_revision")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return legacyDefaultRedirectCapabilities(engineRev, "toolchain store unreadable at "+root), nil
	}
	type found struct {
		dir   string
		kinds []string
	}
	matches := []found{}
	for _, e := range entries {
		enginePath := filepath.Join(root, e.Name(), "ios", "engine.json")
		raw, rerr := os.ReadFile(enginePath)
		if rerr != nil {
			continue
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(raw, &doc) != nil {
			continue // an unparseable bundle is not evidence about THIS engine
		}
		var rev string
		if r, ok := doc["soroq_engine_revision"]; ok {
			_ = json.Unmarshal(r, &rev)
		}
		if rev != engineRev {
			continue
		}
		decl, ok := doc[freehandEngineCapabilityKey]
		if !ok {
			matches = append(matches, found{dir: e.Name(), kinds: nil})
			continue
		}
		kinds, derr := decodeEngineCapabilityDeclaration(decl)
		if derr != nil {
			return nil, fmt.Errorf("engine bundle %s declares a malformed %s: %w", enginePath, freehandEngineCapabilityKey, derr)
		}
		matches = append(matches, found{dir: e.Name(), kinds: kinds})
	}
	if len(matches) == 0 {
		return legacyDefaultRedirectCapabilities(engineRev, "no installed engine bundle declares revision "+engineRev), nil
	}
	// Two bundles claiming the same engine revision must say the same thing about it. Picking the first
	// would make the recorded capability depend on directory iteration order.
	first := matches[0]
	for _, m := range matches[1:] {
		if strings.Join(m.kinds, ",") != strings.Join(first.kinds, ",") {
			return nil, fmt.Errorf("installed engine bundles %s and %s both claim engine revision %s but declare DIFFERENT honoured redirect kinds ([%s] vs [%s]); refusing to guess which engine this base was built with",
				first.dir, m.dir, engineRev, strings.Join(first.kinds, ", "), strings.Join(m.kinds, ", "))
		}
	}
	if first.kinds == nil {
		return legacyDefaultRedirectCapabilities(engineRev, "engine bundle "+first.dir+" declares no "+freehandEngineCapabilityKey), nil
	}
	c := &FreehandRedirectCapabilities{
		Schema:         freehandRedirectCapabilitiesSchema,
		Source:         freehandCapabilitySourceEngine,
		EngineRevision: engineRev,
		HonouredKinds:  first.kinds,
		Note:           "declared by the engine bundle itself (" + freehandEngineCapabilityKey + " in " + first.dir + "/ios/engine.json)",
	}
	if err := validateFreehandRedirectCapabilities(c); err != nil {
		return nil, err
	}
	return c, nil
}

// decodeEngineCapabilityDeclaration reads the engine's own declaration. It accepts the object form
//
//	"soroq_freehand_redirect_capabilities": {"honoured_kinds": ["constructor", ...]}
//
// and nothing else: a bare list or a stray string would be a different contract, and guessing which one
// an engine meant is how a producer ends up "unlocking" a kind the engine never claimed.
func decodeEngineCapabilityDeclaration(raw json.RawMessage) ([]string, error) {
	var decl struct {
		HonouredKinds []string `json:"honoured_kinds"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decl); err != nil {
		return nil, fmt.Errorf("not a {\"honoured_kinds\": [...]} object: %w", err)
	}
	if len(decl.HonouredKinds) == 0 {
		return nil, errors.New("honoured_kinds is empty; an engine that honours nothing cannot receive any patch")
	}
	seen := map[string]bool{}
	kinds := make([]string, 0, len(decl.HonouredKinds))
	for _, k := range decl.HonouredKinds {
		if !freehandSemanticKinds[k] {
			return nil, fmt.Errorf("honoured_kinds contains %q, which is not a frozen semantic identity kind (%s)", k, freehandSemanticKindList())
		}
		if seen[k] {
			return nil, fmt.Errorf("honoured_kinds lists %q twice", k)
		}
		seen[k] = true
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds, nil
}

// requireFreehandRetention fails closed unless the baseline carries verified, non-empty, consistent
// freehand-retention evidence. It is the single gate both the release path (before persisting/
// registering) and the patch path (before diffing a base) call, so a plain/reused Flutter build — which
// produces no analysis staging and hence no evidence — can never be accepted as a freehand base.
func requireFreehandRetention(meta *FreehandBaselineMeta) error {
	r := meta.Retention
	if r == nil {
		return errors.New("freehand base has NO retention evidence: it was not built via `soroq release ios --engine --build` freehand mode (no --soroq_manifest retention). A plain/reused `flutter build` is not a valid freehand base")
	}
	if !r.Verified {
		return errors.New("freehand retention evidence is present but not verified (analysis staging did not validate against the live app.dill)")
	}
	if r.RetainedIdentities <= 0 {
		return errors.New("freehand retention retained ZERO identities; nothing would be patchable by identity on device")
	}
	if r.ManifestSHA256 == "" || r.ManifestSHA256 != meta.ManifestSHA256 {
		return fmt.Errorf("freehand retention manifest sha %q is inconsistent with the baseline manifest sha %q", r.ManifestSHA256, meta.ManifestSHA256)
	}
	if r.SymbolGraphSHA256 == "" || r.SymbolGraphSHA256 != meta.GraphSHA256 {
		return fmt.Errorf("freehand retention symbol_graph sha %q is inconsistent with the baseline symbol_graph sha %q", r.SymbolGraphSHA256, meta.GraphSHA256)
	}
	if !contentAddrRe.MatchString(strings.TrimSpace(r.AnalysisID)) {
		return fmt.Errorf("freehand retention analysis_id %q is not a valid non-empty content address (64-hex sha256 of the verified analysis staging)", r.AnalysisID)
	}
	if r.RetainedIdentities != meta.PatchableCount {
		return fmt.Errorf("freehand retention count %d != baseline patchable_symbols %d", r.RetainedIdentities, meta.PatchableCount)
	}
	return nil
}

// retentionEqual is a nil-safe field-by-field comparison of the immutable retention evidence.
func retentionEqual(a, b *FreehandRetentionEvidence) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Verified == b.Verified &&
		a.RetainedIdentities == b.RetainedIdentities &&
		a.ManifestSHA256 == b.ManifestSHA256 &&
		a.SymbolGraphSHA256 == b.SymbolGraphSHA256 &&
		a.AnalysisID == b.AnalysisID
}

const freehandBaselineSchemaV2 = "soroq.freehand.baseline.v2"

// freehandFaultInjection is a TEST-ONLY hook (nil in production) to simulate interruption after a
// named write stage. Production builds never set it.
var freehandFaultInjection func(stage string) error

func freehandFault(stage string) error {
	if freehandFaultInjection != nil {
		return freehandFaultInjection(stage)
	}
	return nil
}

func freehandSHA256Bytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func sha256OfPath(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var runtimeIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// validateRuntimeID rejects anything unsafe as a single path segment (separators, traversal,
// absolute paths, empty/oversized, leading dot, unsafe chars).
func validateRuntimeID(id string) error {
	if id == "" {
		return errors.New("empty runtime-id")
	}
	if len(id) > 128 {
		return fmt.Errorf("runtime-id too long (%d > 128)", len(id))
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") || filepath.IsAbs(id) {
		return fmt.Errorf("runtime-id %q contains path separators or traversal", id)
	}
	if id != filepath.Base(id) || id == "." || id == ".." {
		return fmt.Errorf("runtime-id %q is not a single safe path segment", id)
	}
	if !runtimeIDRe.MatchString(id) {
		return fmt.Errorf("runtime-id %q has unsafe characters (allowed: [A-Za-z0-9._-], no leading dot)", id)
	}
	return nil
}

func writeFileSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// copyFileVerifiedSync copies src->dst with private perms, verifies the hash, and fsyncs the file.
func copyFileVerifiedSync(src, dst, wantSHA string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); wantSHA != "" && got != wantSHA {
		return fmt.Errorf("hash mismatch copying %s: got %s want %s", src, got, wantSHA)
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

func freehandReleaseDir(projectDir, runtimeID string) string {
	return filepath.Join(projectDir, ".soroq", "releases", runtimeID)
}

var requiredBaselineFields = []string{
	"schema", "identity_schema", "analyzer_version", "app_dill_sha256",
	"source_app_dill_sha256", "source_kernel_recipe", "source_kernel_recipe_digest",
	"manifest_sha256", "symbol_graph_sha256", "package_config_sha256",
	"frontend_revision", "frontend_patchset_sha256", "framework_revision", "dart_revision", "engine_revision",
	"app_id", "version", "runtime_id", "arch", "channel", "patchable_symbols", "retention",
	"dependency_graph_schema", "dependency_graph_sha256", "dependency_graph_digest",
	"dependency_pubspec_lock_sha256", "dependency_package_config_sha256",
	"contract_schema", "contract_digest",
}

// errBasePredatesGenericContract is the required, actionable refusal for a base built before the widened
// freehand base contract. Such a base retained only a narrow `callable:` surface, so a dependency that
// extends or uses base types as types can never be delivered onto it -- no amount of patching can widen a
// contract that is already baked into the installed app.
const errBasePredatesGenericContract = "This base predates generic Dart-dependency patching; create one new store release."

// dependencyGraphFile is the immutable base runtime dependency graph persisted beside the baseline.
const dependencyGraphFile = "dependency_graph.json"

// baseDependencyGraph loads and strictly re-verifies a baseline's immutable dependency graph from its own
// file: the file bytes must hash to the recorded sha, the graph must pass full strict decoding (schema,
// generator version, per-package hashes, dangling-edge and digest checks), and its canonical digest must
// equal the one recorded in baseline.json. Nothing stored in baseline.json is trusted on its own.
func baseDependencyGraph(relDir string, m *FreehandBaselineMeta) (depgraph.Graph, error) {
	p := filepath.Join(relDir, dependencyGraphFile)
	fi, err := os.Lstat(p)
	if err != nil {
		return depgraph.Graph{}, fmt.Errorf("baseline missing %s: %w", dependencyGraphFile, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return depgraph.Graph{}, fmt.Errorf("baseline %s is not a regular file", dependencyGraphFile)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return depgraph.Graph{}, err
	}
	if got := freehandSHA256Bytes(raw); got != m.DependencyGraphSHA256 {
		return depgraph.Graph{}, fmt.Errorf("baseline %s hash mismatch: %s != recorded %s", dependencyGraphFile, got, m.DependencyGraphSHA256)
	}
	g, err := depgraph.DecodeGraphStrict(raw)
	if err != nil {
		return depgraph.Graph{}, fmt.Errorf("baseline %s is invalid: %w", dependencyGraphFile, err)
	}
	if g.GraphDigest != m.DependencyGraphDigest {
		return depgraph.Graph{}, fmt.Errorf("baseline dependency_graph_digest %s != the graph's own digest %s", m.DependencyGraphDigest, g.GraphDigest)
	}
	if g.Schema != m.DependencyGraphSchema {
		return depgraph.Graph{}, fmt.Errorf("baseline dependency_graph_schema %q != the graph's own schema %q", m.DependencyGraphSchema, g.Schema)
	}
	if g.PubspecLockSHA != m.DependencyLockSHA256 || g.PackageConfigSHA != m.DependencyPackageConfigSHA256 {
		return depgraph.Graph{}, fmt.Errorf("baseline dependency resolution inputs disagree with the recorded graph (lock %s vs %s, package_config %s vs %s)",
			m.DependencyLockSHA256, g.PubspecLockSHA, m.DependencyPackageConfigSHA256, g.PackageConfigSHA)
	}
	return g, nil
}

// verifyExistingBaseline strictly validates an on-disk baseline: no dir/file symlinks, strict JSON
// (no unknown fields), all required fields present, and app.dill/manifest/graph re-hashed to match
// baseline.json. Any deviation is an error — never silent idempotent success.
func verifyExistingBaseline(relDir string) (*FreehandBaselineMeta, error) {
	fi, err := os.Lstat(relDir)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("baseline dir is a symlink: %s", relDir)
	}
	raw, err := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	if err != nil {
		return nil, fmt.Errorf("read baseline.json: %w", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("corrupt baseline.json: %w", err)
	}
	// Schema gate FIRST so a legacy v1 baseline gives the actionable "create a new base release" message
	// (before the v2 required-field presence check would report a missing source field).
	if sc, ok := probe["schema"]; ok {
		var schemaStr string
		_ = json.Unmarshal(sc, &schemaStr)
		if schemaStr == "soroq.freehand.baseline.v1" {
			return nil, fmt.Errorf("baseline %s is schema v1 (no source-fidelity kernel); the freehand diff requires a dual-kernel v2 baseline — create a new base release with `soroq release ios --engine --build`", relDir)
		}
		// The ENGINE-LANE baseline is a different artifact with a different schema. Landing one here
		// means `soroqctl release ios-engine --out` was pointed at a freehand release directory. Say
		// so, naming both schemas, instead of failing later on absent freehand fields.
		if strings.HasPrefix(schemaStr, "soroq.ios_engine_baseline.") {
			return nil, fmt.Errorf(
				"baseline %s/baseline.json declares schema %q — that is an ENGINE-LANE baseline, not a freehand baseline (%s).\n"+
					"  These are two different artifacts. The freehand baseline is written here by `soroq release ios --engine --build`;\n"+
					"  the engine-lane baseline is written by `soroqctl release ios-engine --out <file>` and must NOT be named baseline.json.",
				relDir, schemaStr, freehandBaselineSchemaV2)
		}
	}
	// A baseline written before dependency-OTA support has no recorded base dependency graph. It is
	// otherwise well-formed, so give it its own actionable message instead of a bare missing-field error:
	// without the base graph there is no anchor to check a patch's dependency descriptor against.
	if _, ok := probe["contract_digest"]; !ok {
		return nil, fmt.Errorf("%s", errBasePredatesGenericContract)
	}
	if _, ok := probe["dependency_graph_digest"]; !ok {
		return nil, fmt.Errorf("baseline %s predates dependency-OTA support: it records no immutable base dependency graph, so a dependency change cannot be verified against it. Create a new base release with `soroq release ios --engine --build` to produce a dependency-OTA-capable base", relDir)
	}
	for _, f := range requiredBaselineFields {
		if _, ok := probe[f]; !ok {
			return nil, fmt.Errorf("baseline.json missing required field %q", f)
		}
	}
	var m FreehandBaselineMeta
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("corrupt/unknown-field baseline.json: %w", err)
	}
	if m.Schema != freehandBaselineSchemaV2 {
		return nil, fmt.Errorf("unexpected baseline schema %q", m.Schema)
	}
	if m.AppDillSHA256 == "" || m.SourceAppDillSHA256 == "" || m.ManifestSHA256 == "" || m.GraphSHA256 == "" || m.RuntimeID == "" {
		return nil, errors.New("baseline.json has empty required hash/id")
	}
	if m.SourceKernelRecipe == nil {
		return nil, errors.New("baseline.json missing source_kernel_recipe")
	}
	// Recipe digest must match the recomputed digest of the recorded recipe (tamper-evident binding).
	if rd, err := m.SourceKernelRecipe.recipeDigest(); err != nil || rd != m.SourceRecipeDigest {
		return nil, fmt.Errorf("baseline source_kernel_recipe_digest mismatch: recorded %s != recomputed %s", m.SourceRecipeDigest, rd)
	}
	checks := map[string]string{
		"app.dill":               m.AppDillSHA256,
		"source_app.dill":        m.SourceAppDillSHA256,
		"soroq_app_manifest.txt": m.ManifestSHA256,
		"symbol_graph.json":      m.GraphSHA256,
	}
	for f, want := range checks {
		p := filepath.Join(relDir, f)
		lfi, err := os.Lstat(p)
		if err != nil {
			return nil, fmt.Errorf("baseline missing %s: %w", f, err)
		}
		if lfi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("baseline %s is a symlink", f)
		}
		got, err := sha256OfPath(p)
		if err != nil {
			return nil, err
		}
		if got != want {
			return nil, fmt.Errorf("baseline %s hash mismatch: %s != recorded %s", f, got, want)
		}
	}
	// Recorded patchable count must equal the manifest's actual non-empty entries.
	manifestBytes, err := os.ReadFile(filepath.Join(relDir, "soroq_app_manifest.txt"))
	if err != nil {
		return nil, err
	}
	if actual := countManifestEntries(manifestBytes); actual != m.PatchableCount {
		return nil, fmt.Errorf("baseline patchable_symbols %d != actual manifest entries %d", m.PatchableCount, actual)
	}
	// Provenance must be complete for an existing baseline to be trusted/reused.
	if err := requireProvenance(&m); err != nil {
		return nil, err
	}
	// Retention evidence must be present + internally consistent (verified, non-zero, manifest/symbol-graph
	// SHAs matching, content-addressed analysis_id) for an existing baseline to be reused or patched.
	if err := requireFreehandRetention(&m); err != nil {
		return nil, err
	}
	// The immutable base dependency graph must load, strictly validate, and agree with every value
	// recorded about it in baseline.json.
	if _, err := baseDependencyGraph(relDir, &m); err != nil {
		return nil, err
	}
	// A RECORDED redirect-capability set must be well-formed. Absent is fine (legacy-default is applied
	// when it is read); present-but-malformed is not, because the guard that consults it would then be
	// deciding what a base can honour from a record nobody can parse.
	if _, err := baseRedirectCapabilities(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// countManifestEntries counts non-empty manifest lines (the true patchable-symbol count).
func countManifestEntries(b []byte) int {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// requireProvenance fails closed unless every immutable provenance field is present + non-empty.
func requireProvenance(m *FreehandBaselineMeta) error {
	missing := []string{}
	chk := func(name, v string) {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	chk("identity_schema", m.IdentitySchema)
	chk("analyzer_version", m.AnalyzerVersion)
	chk("source_app_dill_sha256", m.SourceAppDillSHA256)
	chk("source_kernel_recipe_digest", m.SourceRecipeDigest)
	chk("package_config_sha256", m.PackageConfigSHA256)
	chk("frontend_revision", m.FrontendRev)
	chk("frontend_patchset_sha256", m.FrontendPatchsetSHA)
	chk("framework_revision", m.FrameworkRev)
	chk("dart_revision", m.DartRev)
	chk("engine_revision", m.EngineRev)
	chk("app_id", m.AppID)
	chk("version", m.Version)
	chk("runtime_id", m.RuntimeID)
	chk("arch", m.Arch)
	chk("channel", m.Channel)
	if m.PatchableCount <= 0 {
		missing = append(missing, "patchable_symbols(>0)")
	}
	if m.SourceKernelRecipe == nil {
		missing = append(missing, "source_kernel_recipe")
	}
	if len(missing) > 0 {
		return fmt.Errorf("baseline provenance incomplete: %v", missing)
	}
	return nil
}

// immutableInputsEqual compares every immutable input (not just app.dill).
func immutableInputsEqual(a, b *FreehandBaselineMeta) bool {
	return a.AppDillSHA256 == b.AppDillSHA256 &&
		a.SourceAppDillSHA256 == b.SourceAppDillSHA256 &&
		a.SourceRecipeDigest == b.SourceRecipeDigest &&
		a.ManifestSHA256 == b.ManifestSHA256 &&
		a.GraphSHA256 == b.GraphSHA256 &&
		a.PackageConfigSHA256 == b.PackageConfigSHA256 &&
		a.IdentitySchema == b.IdentitySchema &&
		a.AnalyzerVersion == b.AnalyzerVersion &&
		a.FrontendRev == b.FrontendRev &&
		a.FrontendPatchsetSHA == b.FrontendPatchsetSHA &&
		a.FrameworkRev == b.FrameworkRev &&
		a.DartRev == b.DartRev &&
		a.EngineRev == b.EngineRev &&
		a.AppID == b.AppID &&
		a.Version == b.Version &&
		a.RuntimeID == b.RuntimeID &&
		a.Arch == b.Arch &&
		a.Channel == b.Channel &&
		a.PatchableCount == b.PatchableCount &&
		a.DependencyGraphSchema == b.DependencyGraphSchema &&
		a.DependencyGraphSHA256 == b.DependencyGraphSHA256 &&
		a.DependencyGraphDigest == b.DependencyGraphDigest &&
		a.DependencyLockSHA256 == b.DependencyLockSHA256 &&
		a.DependencyPackageConfigSHA256 == b.DependencyPackageConfigSHA256 &&
		a.ContractSchema == b.ContractSchema &&
		a.ContractDigest == b.ContractDigest &&
		retentionEqual(a.Retention, b.Retention)
}

// persistFreehandBaseline atomically snapshots (appDill, manifest, graph) + baseline.json under
// .soroq/releases/<runtime-id>/, returning the release dir. Immutable across all inputs, idempotent
// for an identical re-run, hash-verified, path-safe, fault-atomic, and fsync-durable.
func persistFreehandBaseline(projectDir string, meta FreehandBaselineMeta, appDillPath, sourceDillPath, manifestPath, graphPath string, depGraph depgraph.Graph) (string, error) {
	if err := validateRuntimeID(meta.RuntimeID); err != nil {
		return "", err
	}
	// The base runtime dependency graph is canonicalized (which validates it) and its identity derived
	// here — never taken from the caller — so a baseline can only be written with a graph that passes the
	// same strict checks every reader applies.
	depGraphBytes, err := depGraph.MarshalCanonical()
	if err != nil {
		return "", fmt.Errorf("refusing to persist a baseline with an invalid base dependency graph: %w", err)
	}
	meta.DependencyGraphSchema = depGraph.Schema
	meta.DependencyGraphSHA256 = freehandSHA256Bytes(depGraphBytes)
	meta.DependencyGraphDigest = depGraph.GraphDigest
	meta.DependencyLockSHA256 = depGraph.PubspecLockSHA
	meta.DependencyPackageConfigSHA256 = depGraph.PackageConfigSHA
	actualDill, err := sha256OfPath(appDillPath)
	if err != nil {
		return "", fmt.Errorf("hash app.dill: %w", err)
	}
	if meta.AppDillSHA256 != "" && meta.AppDillSHA256 != actualDill {
		return "", fmt.Errorf("mismatched kernel: baseline records app.dill %s but file is %s", meta.AppDillSHA256, actualDill)
	}
	meta.AppDillSHA256 = actualDill
	// v2 dual-kernel: the non-AOT source-fidelity companion is required.
	if strings.TrimSpace(sourceDillPath) == "" {
		return "", errors.New("dual-kernel baseline requires a source_app.dill (non-AOT source-fidelity kernel)")
	}
	actualSourceDill, err := sha256OfPath(sourceDillPath)
	if err != nil {
		return "", fmt.Errorf("hash source_app.dill: %w", err)
	}
	if meta.SourceAppDillSHA256 != "" && meta.SourceAppDillSHA256 != actualSourceDill {
		return "", fmt.Errorf("mismatched source kernel: baseline records source_app.dill %s but file is %s", meta.SourceAppDillSHA256, actualSourceDill)
	}
	meta.SourceAppDillSHA256 = actualSourceDill
	if meta.SourceKernelRecipe == nil {
		return "", errors.New("dual-kernel baseline requires source_kernel_recipe")
	}
	recipeDigest, err := meta.SourceKernelRecipe.recipeDigest()
	if err != nil {
		return "", err
	}
	meta.SourceRecipeDigest = recipeDigest
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}
	graphBytes, err := os.ReadFile(graphPath)
	if err != nil {
		return "", fmt.Errorf("read symbol graph: %w", err)
	}
	meta.ManifestSHA256 = freehandSHA256Bytes(manifestBytes)
	meta.GraphSHA256 = freehandSHA256Bytes(graphBytes)
	meta.Schema = freehandBaselineSchemaV2
	// PatchableCount is DERIVED from the manifest — never trust a caller-supplied/zero count.
	meta.PatchableCount = countManifestEntries(manifestBytes)
	// Retention gate: derive the retention count/hashes from the just-validated manifest/graph; the
	// caller supplies Verified+AnalysisID ONLY after the analysis staging validated against the live
	// app.dill (verifyFreehandStagingStrict). Fail closed unless the evidence is complete — a plain/
	// reused Flutter build never reaches here with Retention.Verified set.
	if meta.Retention == nil {
		meta.Retention = &FreehandRetentionEvidence{}
	}
	meta.Retention.RetainedIdentities = meta.PatchableCount
	meta.Retention.ManifestSHA256 = meta.ManifestSHA256
	meta.Retention.SymbolGraphSHA256 = meta.GraphSHA256
	if err := requireFreehandRetention(&meta); err != nil {
		return "", fmt.Errorf("refusing to persist a freehand baseline without verified retention: %w", err)
	}
	// Provenance must be complete before an immutable baseline is written.
	if err := requireProvenance(&meta); err != nil {
		return "", err
	}
	// The engine's redirect capability is DERIVED here from the engine bundle that matches this
	// baseline's own engine_revision (requireProvenance above guarantees it is non-empty) — never taken
	// from the caller, for the same reason PatchableCount is derived: a value a caller could supply is a
	// value a caller could overstate, and overstating this one produces patches that install and do
	// nothing. A caller-set value is overwritten, not merged.
	capabilities, err := deriveFreehandRedirectCapabilities(meta.EngineRev)
	if err != nil {
		return "", fmt.Errorf("refusing to persist a baseline whose engine redirect capability cannot be resolved: %w", err)
	}
	meta.RedirectCapabilities = capabilities

	releasesRoot := filepath.Join(projectDir, ".soroq", "releases")
	relDir := filepath.Join(releasesRoot, meta.RuntimeID)
	absRoot, err := filepath.Abs(releasesRoot)
	if err != nil {
		return "", err
	}
	absRel, err := filepath.Abs(relDir)
	if err != nil {
		return "", err
	}
	if absRel != filepath.Join(absRoot, meta.RuntimeID) ||
		!strings.HasPrefix(absRel+string(os.PathSeparator), absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("release dir %q escapes releases root %q", absRel, absRoot)
	}

	// existing baseline: full validation + compare ALL immutable inputs.
	if _, statErr := os.Stat(filepath.Join(relDir, "baseline.json")); statErr == nil {
		prev, verr := verifyExistingBaseline(relDir)
		if verr != nil {
			return "", fmt.Errorf("existing baseline for runtime-id %s is invalid: %w", meta.RuntimeID, verr)
		}
		if immutableInputsEqual(prev, &meta) {
			return relDir, nil // idempotent: every immutable input identical
		}
		return "", fmt.Errorf("refusing to overwrite immutable baseline for runtime-id %s: an immutable input differs (app.dill/manifest/graph/config/identity/analyzer/revisions); a changed baseline requires a new runtime-id. The runtime id is derived from the app VERSION, not from the app.dill, so rebuilding changed source under the same pubspec.yaml version reuses it: bump `version:` in pubspec.yaml and build again", meta.RuntimeID)
	}

	if err := os.MkdirAll(releasesRoot, 0o700); err != nil {
		return "", err
	}
	tmpDir, err := os.MkdirTemp(releasesRoot, ".tmp-"+meta.RuntimeID+"-")
	if err != nil {
		return "", err
	}
	_ = os.Chmod(tmpDir, 0o700)
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(tmpDir)
		}
	}()

	if err := copyFileVerifiedSync(appDillPath, filepath.Join(tmpDir, "app.dill"), actualDill, 0o600); err != nil {
		return "", err
	}
	if err := freehandFault("after-appdill"); err != nil {
		return "", err
	}
	if err := copyFileVerifiedSync(sourceDillPath, filepath.Join(tmpDir, "source_app.dill"), actualSourceDill, 0o600); err != nil {
		return "", err
	}
	if err := freehandFault("after-source-appdill"); err != nil {
		return "", err
	}
	if err := writeFileSync(filepath.Join(tmpDir, "soroq_app_manifest.txt"), manifestBytes, 0o600); err != nil {
		return "", err
	}
	if err := freehandFault("after-manifest"); err != nil {
		return "", err
	}
	if err := writeFileSync(filepath.Join(tmpDir, "symbol_graph.json"), graphBytes, 0o600); err != nil {
		return "", err
	}
	if err := freehandFault("after-graph"); err != nil {
		return "", err
	}
	if err := writeFileSync(filepath.Join(tmpDir, dependencyGraphFile), depGraphBytes, 0o600); err != nil {
		return "", err
	}
	if err := freehandFault("after-dependency-graph"); err != nil {
		return "", err
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeFileSync(filepath.Join(tmpDir, "baseline.json"), metaBytes, 0o600); err != nil {
		return "", err
	}
	if err := freehandFault("before-rename"); err != nil {
		return "", err
	}
	syncDir(tmpDir)

	if err := os.Rename(tmpDir, relDir); err != nil {
		// Concurrent writer may have created relDir. Reopen, fully verify, compare all inputs.
		if _, statErr := os.Stat(filepath.Join(relDir, "baseline.json")); statErr == nil {
			prev, verr := verifyExistingBaseline(relDir)
			if verr != nil {
				return "", fmt.Errorf("concurrent writer left an invalid baseline for %s: %w", meta.RuntimeID, verr)
			}
			if immutableInputsEqual(prev, &meta) {
				return relDir, nil
			}
			return "", fmt.Errorf("concurrent-writer collision for runtime-id %s: winning baseline differs on an immutable input", meta.RuntimeID)
		}
		return "", fmt.Errorf("atomic publish baseline: %w", err)
	}
	cleanup = false
	syncDir(releasesRoot) // durability of the rename in the parent directory
	return relDir, nil
}

// ---------------------------------------------------------------------------------------------
// THE CANONICAL RICH BASE IDENTITY.
//
// Selection and matching used to key on runtime_id alone. That id is derived from
// soroqManifestTrustRuntimeID(appID, channel, appVersion, buildName, buildNumber, trustFingerprint)
// and contains NOTHING about the app binary, so two structurally different bases that share
// app/channel/version/trust produce the SAME id and each could be served the other's artifact. The
// rich identity binds the three things that actually decide whether a module is loadable — the shipped
// base artifact, the dynamic-interface contract it was built against, and the AOT-retained identity set
// — and collapses all four into one digest that selection can key on.
//
// NON-CIRCULAR BY CONSTRUCTION. Every field is read from an immutable input the baseline ALREADY
// records, never from a hash of the record that carries it. A fingerprint that hashed its own container
// would have no fixed point and could only ever be recomputed into agreement with itself.
// ---------------------------------------------------------------------------------------------

// freehandBaseIdentitySchema tags the encoding. It is the FIRST segment so a future v2 encoding cannot
// collide with a v1 one: without it, adding or reordering a field could reproduce the byte string of
// some v1 tuple and two different identities would share a digest.
const freehandBaseIdentitySchema = "soroq.base.identity.v1"

// FreehandRichBaseIdentity is the four-field base identity plus the digest derived from it. It is the
// producer-side mirror of SoroqBaseIdentity in packages/soroq_flutter/lib/src/base_identity.dart.
type FreehandRichBaseIdentity struct {
	RuntimeID       string `json:"runtime_id"`
	BaseFingerprint string `json:"base_fingerprint"`
	ContractDigest  string `json:"contract_digest"`
	RetentionDigest string `json:"retention_digest"`
	Digest          string `json:"digest"`
}

// freehandBaseIdentityDigest is byte-for-byte mirrored by soroqBaseIdentityDigest in
// packages/soroq_flutter/lib/src/base_identity.dart. Both sides pin the SAME expected digest for the
// SAME fixed input (TestFreehandBaseIdentityDigest_PinnedVector here, the 'canonical rich-identity
// digest' group there), so a change made to one and not the other fails a test instead of silently
// making every device refuse every artifact.
//
// The encoding is LENGTH-PREFIXED, not merely separated. A separator-only encoding is not injective: a
// field containing the separator lets two different tuples serialise to identical bytes, and two
// distinct bases would collide again — reintroducing the exact defect through the fix for it.
func freehandBaseIdentityDigest(runtimeID, baseFingerprint, contractDigest, retentionDigest string) string {
	seg := func(key, value string) string {
		return fmt.Sprintf("%s=%d:%s", key, len(value), value)
	}
	canonical := strings.Join([]string{
		"schema=" + freehandBaseIdentitySchema,
		seg("runtime_id", runtimeID),
		seg("base_fingerprint", baseFingerprint),
		seg("contract_digest", contractDigest),
		seg("retention_digest", retentionDigest),
	}, "\n")
	return freehandSHA256Bytes([]byte(canonical))
}

// newFreehandRichBaseIdentity builds an identity from explicit fields and fails closed on any empty
// one. A partial identity is worse than none: empty fields compare equal to empty fields, so two bases
// differing only in the omitted field would be judged the same base and the check would run while
// distinguishing nothing.
func newFreehandRichBaseIdentity(runtimeID, baseFingerprint, contractDigest, retentionDigest string) (FreehandRichBaseIdentity, error) {
	var zero FreehandRichBaseIdentity
	for _, f := range []struct{ name, value string }{
		{"runtime_id", runtimeID},
		{"base_fingerprint", baseFingerprint},
		{"contract_digest", contractDigest},
		{"retention_digest", retentionDigest},
	} {
		if strings.TrimSpace(f.value) == "" {
			return zero, fmt.Errorf("rich base identity is missing %s; a partial identity cannot distinguish two bases", f.name)
		}
	}
	return FreehandRichBaseIdentity{
		RuntimeID:       runtimeID,
		BaseFingerprint: baseFingerprint,
		ContractDigest:  contractDigest,
		RetentionDigest: retentionDigest,
		Digest:          freehandBaseIdentityDigest(runtimeID, baseFingerprint, contractDigest, retentionDigest),
	}, nil
}

// richBaseIdentityFromBaseline derives the identity from a VERIFIED baseline struct — never from a
// re-read of baseline.json, which would reintroduce the TOCTOU the strict verification exists to close.
//
// The mapping is deliberate and each field is an immutable input the baseline already holds:
//
//	runtime_id       = RuntimeID                  (the release identity)
//	base_fingerprint = AppDillSHA256              (the shipped base artifact)
//	contract_digest  = ContractDigest             (the dynamic interface the base was built against)
//	retention_digest = Retention.ManifestSHA256   (the retained-identity set IS the --soroq_manifest)
//
// Retention.ManifestSHA256 rather than the top-level ManifestSHA256 because requireFreehandRetention
// proves the retention copy is the sha of the EXACT bytes gen_snapshot consumed, and cross-checks it
// against the baseline's own — so it is the same value with an extra proof attached.
func richBaseIdentityFromBaseline(m *FreehandBaselineMeta) (FreehandRichBaseIdentity, error) {
	var zero FreehandRichBaseIdentity
	if m == nil {
		return zero, errors.New("no baseline: cannot derive a rich base identity")
	}
	if err := requireFreehandRetention(m); err != nil {
		return zero, fmt.Errorf("cannot derive a rich base identity: %w", err)
	}
	return newFreehandRichBaseIdentity(m.RuntimeID, m.AppDillSHA256, m.ContractDigest, m.Retention.ManifestSHA256)
}

// validate re-derives the digest from the identity's OWN fields and refuses a declared digest that does
// not reproduce. Accepting one would let a producer publish a digest that selects one base and fields
// that describe another; the two gates would then disagree and whichever ran second would be the only
// real one.
func (id FreehandRichBaseIdentity) validate() error {
	rebuilt, err := newFreehandRichBaseIdentity(id.RuntimeID, id.BaseFingerprint, id.ContractDigest, id.RetentionDigest)
	if err != nil {
		return err
	}
	if id.Digest != rebuilt.Digest {
		return fmt.Errorf("rich base identity digest %s does not recompute from its own fields (want %s)", short12(id.Digest), short12(rebuilt.Digest))
	}
	return nil
}
