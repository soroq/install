package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// CANONICAL CODE FINGERPRINTS (engine capability soroq_code_fingerprints_v1).
//
// A patch redirects every declaration whose source changed, plus every caller the R8 dependency map says
// absorbed one. Many of those compile to exactly the machine code the base already ships -- an operand
// reordered, a formatting change, a caller whose inlined copy did not change -- and interpreting them costs
// 10-100x for nothing. Measured on an iPhone 15 Pro: a patched 2M-iteration loop ran 821 ms interpreted;
// Shorebird, which links such code back to the base, ran it natively in 8 ms.
//
// gen_snapshot (--soroq_code_fingerprints) writes, per patchable function, a fingerprint of its machine
// code in which every object-pool reference is replaced by a canonical description of the object it loads,
// every call by its target's identity, and every closure the code creates by that closure's own
// fingerprint (see precompiler.cc). The base's fingerprints are persisted in its baseline; at patch time
// the candidate is built with the same toolchain, contract and flags, and a changed declaration whose
// candidate fingerprint equals the base's keeps running the base's native code. A function the engine
// cannot describe exactly is absent from the file, which keeps its redirect: unequal or missing
// fingerprints only ever cost speed, never correctness.

const (
	freehandCodeFingerprintsCapability = "soroq_code_fingerprints_v1"
	freehandCodeFingerprintsFlag       = "--soroq_code_fingerprints"
	freehandCodeFingerprintsFile       = "soroq_code_fingerprints.tsv"
	freehandCodeFingerprintsSchema     = "soroq.code_fingerprints.v1"
)

var codeFingerprintRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// parseFreehandCodeFingerprints reads gen_snapshot's file (or its canonical rendering). Comment lines start
// with '#'; every other line is `<manifest identity>\t<32 hex>`, each identity at most once.
func parseFreehandCodeFingerprints(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 || parts[0] == "" || !codeFingerprintRe.MatchString(parts[1]) {
			return nil, fmt.Errorf("code fingerprints line %d is malformed: %q", i+1, line)
		}
		if _, dup := out[parts[0]]; dup {
			return nil, fmt.Errorf("code fingerprints list %s twice", parts[0])
		}
		out[parts[0]] = parts[1]
	}
	return out, nil
}

// renderFreehandCodeFingerprints is the canonical form persisted in a baseline: schema line, then the
// entries sorted by identity.
func renderFreehandCodeFingerprints(m map[string]string) []byte {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b bytes.Buffer
	b.WriteString("# " + freehandCodeFingerprintsSchema + "\n")
	for _, id := range ids {
		b.WriteString(id + "\t" + m[id] + "\n")
	}
	return b.Bytes()
}

// engineBundleDeclaresIdentityCapability reads one capability out of an engine.json.
func engineBundleDeclaresIdentityCapability(enginePath, capability string) (bool, error) {
	raw, err := os.ReadFile(enginePath)
	if err != nil {
		return false, fmt.Errorf("read engine bundle %s: %w", enginePath, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, fmt.Errorf("engine bundle %s is not valid JSON: %w", enginePath, err)
	}
	decl, ok := doc[freehandEngineCapabilityKey]
	if !ok {
		return false, nil
	}
	_, caps, err := decodeEngineCapabilityDeclaration(decl)
	if err != nil {
		return false, fmt.Errorf("engine bundle %s declares a malformed %s: %w", enginePath, freehandEngineCapabilityKey, err)
	}
	for _, c := range caps {
		if c == capability {
			return true, nil
		}
	}
	return false, nil
}

func toolchainDeclaresCodeFingerprints(toolchain string) (bool, error) {
	if strings.TrimSpace(toolchain) == "" {
		return false, nil
	}
	bundleDir, err := iosCachedToolchainBundleDir(toolchain)
	if err != nil {
		return false, nil
	}
	enginePath := filepath.Join(bundleDir, "engine.json")
	if _, err := os.Stat(enginePath); os.IsNotExist(err) {
		return false, nil
	}
	return engineBundleDeclaresIdentityCapability(enginePath, freehandCodeFingerprintsCapability)
}

// freehandCodeFingerprintsStagingPath is an ABSOLUTE path in the project's .soroq build staging, removed
// first so a previous build's file can never be adopted. The path travels through Flutter's comma-separated
// --extra-gen-snapshot-options, so a comma is refused.
func freehandCodeFingerprintsStagingPath(projectDir, name string) (string, error) {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}
	p := filepath.Join(abs, ".soroq", "build", name)
	if strings.ContainsAny(p, ",") {
		return "", fmt.Errorf("the code-fingerprints path %s contains a comma, which gen_snapshot's option list cannot carry; move the project to a path without commas", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove the stale code fingerprints %s: %w", p, err)
	}
	return p, nil
}

// withFreehandCodeFingerprints is the release-side wiring: on a toolchain that declares the capability it
// appends --extra-gen-snapshot-options=--soroq_code_fingerprints=<abs path>.
func withFreehandCodeFingerprints(projectDir, toolchain string, passthrough []string) ([]string, string, bool, error) {
	if strings.Contains(strings.Join(passthrough, " "), "soroq_code_fingerprints") {
		return nil, "", false, fmt.Errorf("the build arguments already pass %s; Soroq sets it itself", freehandCodeFingerprintsFlag)
	}
	declared, err := toolchainDeclaresCodeFingerprints(toolchain)
	if err != nil {
		return nil, "", false, fmt.Errorf("read the toolchain's code-fingerprint capability: %w", err)
	}
	if !declared {
		return passthrough, "", false, nil
	}
	path, err := freehandCodeFingerprintsStagingPath(projectDir, freehandCodeFingerprintsFile)
	if err != nil {
		return nil, "", false, err
	}
	out := append(append([]string(nil), passthrough...), "--extra-gen-snapshot-options="+freehandCodeFingerprintsFlag+"="+path)
	return out, path, true, nil
}

// collectFreehandCodeFingerprints reads the file THIS build's gen_snapshot wrote (the engine always writes
// it, with a schema line, when given the flag) and returns its canonical rendering.
func collectFreehandCodeFingerprints(path string, buildStart time.Time) ([]byte, map[string]string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("the build produced no code fingerprints at %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("the code fingerprints at %s are not a regular file", path)
	}
	if fi.ModTime().Before(buildStart.Add(-2 * time.Second)) {
		return nil, nil, fmt.Errorf("the code fingerprints at %s predate this build; refusing to adopt a stale file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if !bytes.HasPrefix(raw, []byte("# "+freehandCodeFingerprintsSchema+"\n")) {
		return nil, nil, fmt.Errorf("the code fingerprints at %s do not carry the %s schema line", path, freehandCodeFingerprintsSchema)
	}
	fps, err := parseFreehandCodeFingerprints(raw)
	if err != nil {
		return nil, nil, err
	}
	return renderFreehandCodeFingerprints(fps), fps, nil
}

// loadVerifiedBaselineCodeFingerprints checks the file in BOTH directions, like the dependency map: required
// (canonical, hashing to the recorded digest) iff the base's engine declared the capability, forbidden
// otherwise. Returns nil for a base without the capability.
func loadVerifiedBaselineCodeFingerprints(relDir string, m *FreehandBaselineMeta) (map[string]string, error) {
	caps, err := baseRedirectCapabilities(m)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(relDir, freehandCodeFingerprintsFile)
	if !caps.hasIdentityCapability(freehandCodeFingerprintsCapability) {
		if m.CodeFingerprintsSchema != "" || m.CodeFingerprintsSHA256 != "" || m.CodeFingerprints != 0 {
			return nil, fmt.Errorf("baseline %s records code fingerprints but its engine does not declare %s", relDir, freehandCodeFingerprintsCapability)
		}
		if _, err := os.Lstat(p); err == nil {
			return nil, fmt.Errorf("baseline %s contains %s but its engine does not declare %s", relDir, freehandCodeFingerprintsFile, freehandCodeFingerprintsCapability)
		}
		return nil, nil
	}
	if m.CodeFingerprintsSchema != freehandCodeFingerprintsSchema || !sha256HexRe.MatchString(m.CodeFingerprintsSHA256) {
		return nil, fmt.Errorf("baseline %s was built by an engine declaring %s but records no valid code fingerprints", relDir, freehandCodeFingerprintsCapability)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("baseline built with %s is missing %s: %w", freehandCodeFingerprintsCapability, freehandCodeFingerprintsFile, err)
	}
	if got := freehandSHA256Bytes(raw); got != m.CodeFingerprintsSHA256 {
		return nil, fmt.Errorf("baseline %s hash mismatch: %s != recorded %s", freehandCodeFingerprintsFile, got, m.CodeFingerprintsSHA256)
	}
	fps, err := parseFreehandCodeFingerprints(raw)
	if err != nil {
		return nil, fmt.Errorf("baseline %s: %w", freehandCodeFingerprintsFile, err)
	}
	if !bytes.Equal(renderFreehandCodeFingerprints(fps), raw) || len(fps) != m.CodeFingerprints {
		return nil, fmt.Errorf("baseline %s is not in canonical form or does not match its recorded count", freehandCodeFingerprintsFile)
	}
	return fps, nil
}

// freehandPlanOptions carries what only the patch command knows. Without it (tests, other callers) no
// candidate is built and nothing is pruned.
type freehandPlanOptions struct {
	toolchain   string
	passthrough []string
	// buildCandidateFingerprints is injectable for tests; nil means the real candidate build.
	buildCandidateFingerprints func(projectDir, toolchain, relDir string, passthrough []string) (map[string]string, error)
	// buildCandidateApp builds the candidate app without fingerprints (the icon-glyph check needs only
	// its assets); injectable for tests, nil means the real candidate build.
	buildCandidateApp func(projectDir, toolchain, relDir string, passthrough []string) error
}

// stripObfuscationArgs removes obfuscation flags: the fingerprints are taken before gen_snapshot renames
// anything, so the candidate needs no obfuscation (and must not produce or consume a map).
func stripObfuscationArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--obfuscate" || strings.HasPrefix(a, "--split-debug-info") ||
			(strings.HasPrefix(a, "--extra-gen-snapshot-options") && strings.Contains(a, "obfuscation")) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// freehandBuildCandidateFingerprints builds the candidate exactly like the base -- same toolchain, the
// base's OWN contract as the dynamic interface, the freehand manifest, the dependency-map flag (an R8+
// engine inlines patchable code only with it) and the generated bootstrap -- and returns the fingerprints
// its gen_snapshot wrote. Only build outputs change; the baseline is untouched.
func freehandBuildCandidateFingerprints(projectDir, toolchain, relDir string, passthrough []string) (map[string]string, error) {
	return freehandBuildCandidate(projectDir, toolchain, relDir, passthrough, true)
}

// freehandBuildCandidateApp builds the candidate the same way without asking gen_snapshot for
// fingerprints -- for checks that need only the built app (the icon-glyph check), on engines with or
// without the fingerprint capability.
func freehandBuildCandidateApp(projectDir, toolchain, relDir string, passthrough []string) error {
	_, err := freehandBuildCandidate(projectDir, toolchain, relDir, passthrough, false)
	return err
}

func freehandBuildCandidate(projectDir, toolchain, relDir string, passthrough []string, withFingerprints bool) (map[string]string, error) {
	contract, err := os.ReadFile(filepath.Join(relDir, freehandBaseContractFile))
	if err != nil {
		return nil, fmt.Errorf("read the base contract: %w", err)
	}
	di, err := freehandCodeFingerprintsStagingPath(projectDir, "candidate_dynamic_interface.yaml")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(di, contract, 0o600); err != nil {
		return nil, err
	}
	pinnedKeyHex, err := ensureProjectManifestSigningKey(projectDir)
	if err != nil {
		return nil, fmt.Errorf("resolve iOS manifest-signing key: %w", err)
	}
	if _, err := prepareSoroqBuildResolution(projectDir); err != nil {
		return nil, fmt.Errorf("prepare Soroq build resolution: %w", err)
	}
	if err := writeFreehandConfigAtomic(projectDir); err != nil {
		return nil, fmt.Errorf("write freehand config: %w", err)
	}
	defer removeFreehandConfig(projectDir)
	pt := iosEngineBuildPassthrough(di, stripObfuscationArgs(passthrough))
	pt, _, _, err = withFreehandDependencyMap(projectDir, toolchain, pt)
	if err != nil {
		return nil, err
	}
	fpPath, err := freehandCodeFingerprintsStagingPath(projectDir, "candidate_code_fingerprints.tsv")
	if err != nil {
		return nil, err
	}
	if withFingerprints {
		pt = append(pt, "--extra-gen-snapshot-options="+freehandCodeFingerprintsFlag+"="+fpPath)
	}
	bootstrapRel, err := prepareFreehandZeroTouch(projectDir, pinnedKeyHex, pt)
	if err != nil {
		return nil, fmt.Errorf("generate zero-touch freehand runtime wiring: %w", err)
	}
	pt = withFreehandBootstrapEntrypoint(bootstrapRel, pt)
	if err := ensureFreehandAnalysisCacheIntegrity(projectDir); err != nil {
		return nil, fmt.Errorf("verify cached freehand analysis: %w", err)
	}
	start := time.Now()
	if !withFingerprints {
		fmt.Fprintln(os.Stderr, "soroq patch ios --engine (freehand): building the candidate to check the icons it draws")
		if _, err := buildIOSAppDill(projectDir, toolchain, pt); err != nil {
			return nil, fmt.Errorf("candidate build for the icon check failed: %w", err)
		}
		return nil, nil
	}
	fmt.Fprintln(os.Stderr, "soroq patch ios --engine (freehand): building the candidate to find changes that compile to the base's own machine code")
	candAOT, err := buildIOSAppDill(projectDir, toolchain, pt)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errFreehandMachineCodeUnavailable, err)
	}
	// Engine R12 compares the candidate's type-flow facts with the base's: keep its AOT kernel.
	if freehandR12.CallGraphPath != "" {
		if err := stageFreehandCandidateAOTKernel(projectDir, candAOT); err != nil {
			return nil, err
		}
	}
	_, fps, err := collectFreehandCodeFingerprints(fpPath, start)
	return fps, err
}

// pruneFreehandUnchangedMachineCode drops, from the (dependency-map expanded) changed set, every
// declaration whose candidate machine code is fingerprint-identical to the base's, and rewrites the diff
// so synthesis and the ABI see the same set. It returns the pruned identities. A base without fingerprints,
// or a candidate whose class layouts moved, prunes nothing.
func pruneFreehandUnchangedMachineCode(projectDir, relDir string, base *FreehandBaselineMeta, rep *FreehandDiffReport,
	diffJSONPath string, kernels freehandExpansionKernels, opts freehandPlanOptions) ([]string, error) {
	baseFPs, err := loadVerifiedBaselineCodeFingerprints(relDir, base)
	if err != nil || baseFPs == nil || !isScopedContractSchema(base.ContractSchema) {
		return nil, err
	}
	build := opts.buildCandidateFingerprints
	if build == nil {
		build = freehandBuildCandidateFingerprints
	}
	candFPs, err := build(projectDir, opts.toolchain, relDir, opts.passthrough)
	if errors.Is(err, errFreehandMachineCodeUnavailable) {
		// Pruning only REMOVES redirects whose machine code is unchanged; without the comparison every
		// changed declaration stays redirected, which is correct and only slower. Never refuse a patch
		// over an optimization.
		fmt.Fprintf(os.Stderr, "machine-code comparison unavailable, nothing pruned: %v\n", err)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	same := freehandSameMachineCode(rep.ChangedPatchable, baseFPs, candFPs)
	// A declaration the analyzer FORCED into the patch (a referrer of changed private code, an accessor
	// of module-owned storage, a constructor running a changed field initializer) is there because of
	// what it LOADS, not how it compiles: identical machine code names the same field while that field's
	// initial value changed. Pruning it would ship nothing.
	for _, c := range rep.Changed {
		if forced, _ := c["forced"].(bool); forced {
			if ml, _ := c["manifestLine"].(string); ml != "" {
				delete(same, ml)
			}
		}
	}
	if len(same) == 0 {
		return nil, nil
	}
	// Identical machine code proves identical behaviour only against the same object layouts.
	var receivers []string
	for id := range same {
		if _, cls, _, _ := splitIdentity(id); cls != "" {
			receivers = append(receivers, id)
		}
	}
	if len(receivers) > 0 {
		baseSymbols, err := kernels.Base()
		if err != nil {
			return nil, err
		}
		candSymbols, err := kernels.Candidate()
		if err != nil {
			return nil, err
		}
		if lerr := assertNoClassLayoutChange(baseSymbols, candSymbols, receivers); lerr != nil {
			fmt.Fprintf(os.Stderr, "machine-code comparison: not pruning, a class layout changed (%v)\n", lerr)
			return nil, nil
		}
	}
	kept := rep.Changed[:0]
	for _, c := range rep.Changed {
		if ml, _ := c["manifestLine"].(string); same[ml] {
			continue
		}
		kept = append(kept, c)
	}
	rep.Changed = kept
	cp := make([]string, 0, len(rep.ChangedPatchable))
	for _, id := range rep.ChangedPatchable {
		if !same[id] {
			cp = append(cp, id)
		}
	}
	rep.ChangedPatchable = cp
	if rep.Counts == nil {
		rep.Counts = map[string]int{}
	}
	rep.Counts["changed"] = len(rep.Changed)
	rep.Counts["changedPatchable"] = len(cp)
	rep.Counts["prunedIdenticalMachineCode"] = len(same)
	pruned := make([]string, 0, len(same))
	for id := range same {
		pruned = append(pruned, id)
	}
	sort.Strings(pruned)
	if len(cp) == 0 {
		return pruned, errFreehandIdenticalMachineCode
	}
	if err := rewriteFreehandDiffJSON(diffJSONPath, rep); err != nil {
		return nil, fmt.Errorf("rewrite the diff after machine-code pruning: %w", err)
	}
	fmt.Fprintf(os.Stdout, "machine-code comparison: %d changed declaration(s) compile to the base's own code and stay native: %s\n",
		len(pruned), strings.Join(pruned, ", "))
	return pruned, nil
}

// freehandSameMachineCode is the pruning decision: a changed identity whose fingerprint is present in BOTH
// builds and equal. Absent on either side (the engine could not describe it exactly) means keep it.
func freehandSameMachineCode(changed []string, baseFPs, candFPs map[string]string) map[string]bool {
	same := map[string]bool{}
	for _, id := range changed {
		if b, ok := baseFPs[id]; ok {
			if c, ok := candFPs[id]; ok && b == c {
				same[id] = true
			}
		}
	}
	return same
}

var errFreehandIdenticalMachineCode = errors.New("every changed declaration compiles to exactly the machine code the base already ships; there is nothing to patch")

// errFreehandMachineCodeUnavailable marks a failed candidate build for the machine-code comparison.
var errFreehandMachineCodeUnavailable = errors.New("candidate build for machine-code comparison failed")
