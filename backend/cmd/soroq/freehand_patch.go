package main

// Soroq freehand — candidate patch flow for `soroq patch ios --engine` (no ios_engine.patchable list).
//
// Contract (Step #4):
//   1. Load + strictly verify the immutable dual-kernel v2 baseline at .soroq/releases/<runtime-id>/.
//   2. Compile the developer's CURRENT project to a source-fidelity kernel using the baseline's recorded
//      recipe (identical entrypoint/target/platform/package_config/defines/experiments).
//   3. Diff source_app.dill (baseline) vs the candidate source kernel by the frozen identity schema v1.
//   4. Bind both baseline kernel hashes + the candidate source-kernel hash into the patch plan/metadata.
//   5. Fail closed (persist/register nothing) on an incompatible baseline, tampered provenance, or any
//      unsupported change (signature change, deletion, unsupported declaration, unresolved new code).
//
// The AOT app.dill remains the dart2bytecode --import-dill for module compilation (Phase E); the diff
// runs ONLY against the source-fidelity kernel.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"soroq/backend/internal/depgraph"
)

// FreehandDiffReport mirrors the analyzer's freehand_diff.json (schema soroq.freehand.diff.v1).
type FreehandDiffReport struct {
	Schema           string              `json:"schema"`
	IdentitySchema   string              `json:"identitySchema"`
	Supported        bool                `json:"supported"`
	NoOp             bool                `json:"noOp"`
	Blockers         []string            `json:"blockers"`
	Changed          []map[string]any    `json:"changed"`
	ChangedPatchable []string            `json:"changedPatchable"`
	NewCodeClosure   []map[string]any    `json:"newCodeClosure"`
	SignatureChanged []map[string]string `json:"signatureChanged"`
	Deleted          []map[string]any    `json:"deleted"`
	UnsupportedCh    []map[string]string `json:"unsupportedChanged"`
	UnresolvedEdges  []string            `json:"unresolvedEdges"`
	Counts           map[string]int      `json:"counts"`
}

// FreehandPatchPlan is the deterministic, fully-bound plan the module generator (Phase E) consumes and
// that patch metadata is derived from. It never contains secrets.
type FreehandPatchPlan struct {
	Schema                 string              `json:"schema"` // "soroq.freehand.patch_plan.v1"
	RuntimeID              string              `json:"runtime_id"`
	AppID                  string              `json:"app_id"`
	Channel                string              `json:"channel"`
	Version                string              `json:"version"`
	IdentitySchema         string              `json:"identity_schema"`
	BaseAppDillSHA256      string              `json:"base_app_dill_sha256"`      // AOT import-dill
	BaseSourceKernelSHA256 string              `json:"base_source_kernel_sha256"` // non-AOT diff baseline
	CandSourceKernelSHA256 string              `json:"candidate_source_kernel_sha256"`
	SourceRecipeDigest     string              `json:"source_kernel_recipe_digest"`
	ChangedPatchable       []string            `json:"changed_patchable"`
	NewCodeClosure         []string            `json:"new_code_closure"`
	Diff                   *FreehandDiffReport `json:"diff"`

	// BaseIdentity is the COMPLETE rich identity of the base this patch was diffed against, derived from
	// the verified baseline struct (never from a re-read of baseline.json). It travels the whole way —
	// plan -> artifact metadata -> signed device manifest — because runtime_id alone is version-derived
	// and two structurally different bases sharing app/channel/version/trust collide on it, so a patch
	// bound only by runtime_id is deliverable to a base it was never compiled for.
	BaseIdentity *FreehandRichBaseIdentity `json:"base_identity_record"`

	// DependencyDescriptor is the base→candidate RUNTIME dependency delta, already assessed as
	// code-only deliverable and anchored to the immutable base graph recorded in the release baseline.
	// Its digest is bound into the artifact identity, the module manifest and the signed metadata.
	DependencyDescriptor       *depgraph.Descriptor `json:"dependency_descriptor"`
	DependencyDescriptorDigest string               `json:"dependency_descriptor_digest"`

	// Internal (not serialized): paths retained for the module-synthesis step. The caller must clean up.
	candidateKernelPath string                     `json:"-"`
	diffJSONPath        string                     `json:"-"`
	recipe              FreehandSourceKernelRecipe `json:"-"`
	relDir              string                     `json:"-"`
	inputDigest         string                     `json:"-"`
	capabilityMapPath   string                     `json:"-"`
}

// cleanup removes the plan's transient artifacts (candidate kernel + diff dir).
func (p *FreehandPatchPlan) cleanup() {
	if p == nil {
		return
	}
	if p.candidateKernelPath != "" {
		os.Remove(p.candidateKernelPath)
	}
	if p.diffJSONPath != "" {
		os.RemoveAll(filepath.Dir(p.diffJSONPath))
	}
	if p.capabilityMapPath != "" {
		os.Remove(p.capabilityMapPath)
	}
}

// newPackageNames is the comma-separated list of packages ADDED by this patch. The synthesizer carries
// their reachable declarations into the module instead of importing them.
// carriablePackageNames is what the analyzer receives as --new-packages: every package whose SOURCE may
// be transported into the module.
//
// It used to return d.Added only. An eligible pure-Dart UPGRADE was therefore never passed, so its
// source was not carried and the device kept executing the BASE's older copy of that package -- a
// silent wrong-code bug, not a refusal. The descriptor's carriable set is the authority and includes
// added and upgraded alike.
func carriablePackageNames(d *depgraph.Descriptor) string {
	if d == nil {
		return ""
	}
	return strings.Join(d.CarriablePackages(), ",")
}

// writeCapabilityMap materializes the candidate graph's per-package OTA capability as the JSON map the
// analyzer consumes via --capability-map, so BOTH the diff and the synthesis stages classify dependency
// packages by real capability instead of the coarse "imports Flutter ⇒ reject" rule. The map is derived
// from the candidate graph only — never hand-written — so it cannot disagree with the descriptor.
func writeCapabilityMap(g depgraph.Graph, d *depgraph.Descriptor) (string, error) {
	type capEntry struct {
		// Eligible is retained for the analyzer's existing contract; it now means "may this package's
		// source be carried", which is exactly ModeCarriable.
		Eligible bool   `json:"eligible"`
		Mode     string `json:"mode"`
		Reason   string `json:"reason,omitempty"`
	}
	m := make(map[string]capEntry, len(g.Packages))
	if d != nil && len(d.Delivery) > 0 {
		// Derive from the VALIDATED descriptor, not from raw Capability.Eligible. Raw eligibility cannot
		// express base_reference_only: an unchanged native plugin is ineligible-to-carry yet legal to
		// reference, and feeding the analyzer a bare `eligible:false` would make it refuse a reference
		// the base already satisfies.
		for _, pd := range d.Delivery {
			e := capEntry{Mode: string(pd.Mode), Eligible: pd.Mode == depgraph.ModeCarriable}
			if pd.Mode != depgraph.ModeCarriable {
				e.Reason = pd.Reason
			}
			if e.Reason == "" && pd.Mode == depgraph.ModeBaseReferenceOnly {
				e.Reason = "unchanged in the immutable base: may be referenced, never carried"
			}
			m[pd.Name] = e
		}
	} else {
		for name, p := range g.Packages {
			e := capEntry{Eligible: p.Capability.Eligible, Mode: string(depgraph.ModeCarriable)}
			if !e.Eligible {
				e.Mode = string(depgraph.ModeForbidden)
				e.Reason = strings.Join(p.Capability.Reasons, "; ")
			}
			m[name] = e
		}
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "soroq-capability-map-*.json")
	if err != nil {
		return "", err
	}
	path := f.Name()
	f.Close()
	if err := writeFileSync(path, raw, 0o600); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// runFreehandAnalyzerDiff runs the installed analyzer in --diff mode (baseline source kernel vs candidate
// source kernel) and returns the parsed report. The analyzer .dill is run by the fork's dart.
func runFreehandAnalyzerDiff(flutterRoot, baselineSourceDill, candidateSourceDill, packageConfig, outDir, capabilityMapPath string) (*FreehandDiffReport, error) {
	dart := filepath.Join(flutterRoot, "bin", "cache", "dart-sdk", "bin", "dart")
	analyzer := filepath.Join(flutterRoot, "bin", "cache", "soroq", "soroq_kernel_analyze.dill")
	for _, p := range []string{dart, analyzer, baselineSourceDill, candidateSourceDill, packageConfig} {
		if !fileExists(p) {
			return nil, fmt.Errorf("freehand diff input missing: %s", p)
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	args := []string{analyzer, "--diff",
		"--baseline-dill", baselineSourceDill,
		"--dill", candidateSourceDill,
		"--package-config", packageConfig,
		"--out", outDir,
	}
	// The capability map must reach the DIFF stage too, not just synthesis: reachability into a newly
	// added dependency is decided here, and without it the analyzer falls back to rejecting every
	// Flutter-importing package.
	if capabilityMapPath != "" {
		args = append(args, "--capability-map", capabilityMapPath)
	}
	cmd := exec.Command(dart, args...)
	out, err := cmd.CombinedOutput()
	// exit 4 = blockers present (a valid, reportable outcome); other non-zero = real failure.
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 4 {
			return nil, fmt.Errorf("freehand analyzer diff failed: %w\n%s", err, string(out))
		}
	}
	raw, err := os.ReadFile(filepath.Join(outDir, "freehand_diff.json"))
	if err != nil {
		return nil, fmt.Errorf("read freehand_diff.json: %w\n%s", err, string(out))
	}
	var rep FreehandDiffReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("parse freehand_diff.json: %w", err)
	}
	return &rep, nil
}

// computeFreehandPatchPlan loads+verifies the v2 baseline, compiles the candidate source kernel via the
// recorded recipe, diffs, and returns a fully-bound plan. Fails closed on incompatible/tampered baseline
// or any unsupported change. Produces NOTHING persistent (the caller decides on module-gen/registration).
func computeFreehandPatchPlan(projectDir, flutterRoot string) (*FreehandPatchPlan, error) {
	soroqConfig, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		return nil, err
	}
	pubspec, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		return nil, err
	}
	meta, err := buildSoroqBundledMetadata(soroqConfig, pubspec)
	if err != nil {
		return nil, fmt.Errorf("compute runtime identity: %w", err)
	}
	runtimeID := meta.Soroq.RuntimeID
	relDir := freehandReleaseDir(projectDir, runtimeID)
	if _, err := os.Stat(filepath.Join(relDir, "baseline.json")); err != nil {
		return nil, fmt.Errorf("no freehand base release for runtime-id %s at %s; run `soroq release ios --engine --build` to create the base first", runtimeID, relDir)
	}
	// Strict v2 verification (rehash both kernels, recipe digest, provenance). A v1 baseline is refused
	// here with the "create a new base release" message.
	base, err := verifyExistingBaseline(relDir)
	if err != nil {
		return nil, fmt.Errorf("baseline verification failed (freehand patch refused): %w", err)
	}
	// Refuse to patch a base that was NOT built with verified freehand retention: without --soroq_manifest
	// retention its identities are not resolvable on device, so every by-identity redirect would fail at
	// runtime ("new base identity not found"). A plain/reused Flutter build carries no retention evidence.
	if err := requireFreehandRetention(base); err != nil {
		return nil, fmt.Errorf("freehand patch refused — base is not a verified-retention freehand build: %w", err)
	}

	// Source-integrity: snapshot the COMPLETE resolved compilation input set (app + path-package sources,
	// pubspec.lock, package_config, generated sources) AFTER resolution has settled. It is re-verified
	// immediately before module generation so a concurrent source/config edit fails the patch closed.
	inputDigest, err := captureCompilationInputDigest(projectDir)
	if err != nil {
		return nil, fmt.Errorf("capture compilation input digest: %w", err)
	}

	// ---- Dependency delta: assessed BEFORE anything is compiled ----
	//
	// Load the immutable base runtime dependency graph recorded in the release baseline (the base-side
	// anchor), re-resolve the candidate graph from the current project, and build the descriptor. The
	// candidate side is recomputed from each real pubspec.yaml and package contents, so a claim of
	// eligibility cannot be forged; the base side cannot be swapped because it lives inside the
	// separately hash-verified baseline.
	baseGraph, err := baseDependencyGraph(relDir, base)
	if err != nil {
		return nil, fmt.Errorf("load the base release's dependency graph: %w", err)
	}
	candGraph, err := resolveRuntimeGraphPinned(projectDir)
	if err != nil {
		return nil, fmt.Errorf("resolve the candidate runtime dependency graph: %w", err)
	}
	descriptor := depgraph.BuildDescriptor(baseGraph, candGraph)
	if err := descriptor.Validate(); err != nil {
		return nil, fmt.Errorf("dependency descriptor is invalid: %w", err)
	}
	if err := descriptor.AssertMatchesBase(base.DependencyGraphDigest, base.DependencyLockSHA256, base.DependencyPackageConfigSHA256); err != nil {
		return nil, err
	}
	// Refuse native/plugin/asset/removal incompatibilities NOW, naming the exact packages, rather than
	// after spending a full candidate compile on a patch that can never ship.
	if err := descriptor.Assess(); err != nil {
		return nil, fmt.Errorf("freehand patch refused — %w", err)
	}
	if descriptor.Changed() {
		fmt.Fprintf(os.Stdout, "dependency change accepted for code-only OTA:\n%s", descriptor.Summary())
	}
	// The capability map is derived from the candidate graph and passed to BOTH analyzer stages.
	capMapPath, err := writeCapabilityMap(candGraph, &descriptor)
	if err != nil {
		return nil, fmt.Errorf("write capability map: %w", err)
	}

	// Compile the candidate source kernel with the baseline's EXACT recipe (reproducibility). Re-verify
	// the recipe's tool/config inputs still match; a drift (e.g. a different frontend/platform/pkgconfig)
	// means the candidate would be compiled differently than the base — refuse.
	recipe := *base.SourceKernelRecipe
	if err := assertRecipeReproducible(projectDir, flutterRoot, recipe); err != nil {
		return nil, fmt.Errorf("candidate source-kernel recipe is not reproducible against this baseline: %w", err)
	}
	candKernel, err := os.CreateTemp("", "soroq-cand-source-*.dill")
	if err != nil {
		return nil, err
	}
	candPath := candKernel.Name()
	candKernel.Close()
	candSHA, err := generateFreehandSourceKernel(projectDir, flutterRoot, recipe, candPath)
	if err != nil {
		os.Remove(candPath)
		return nil, fmt.Errorf("compile candidate source kernel: %w", err)
	}

	diffOut, err := os.MkdirTemp("", "soroq-freehand-diff-*")
	if err != nil {
		os.Remove(candPath)
		return nil, err
	}
	rep, err := runFreehandAnalyzerDiff(
		flutterRoot,
		filepath.Join(relDir, "source_app.dill"),
		candPath,
		filepath.Join(projectDir, ".dart_tool", "package_config.json"),
		diffOut,
		capMapPath,
	)
	if err != nil {
		os.Remove(candPath)
		os.RemoveAll(diffOut)
		os.Remove(capMapPath)
		return nil, err
	}
	if rep.IdentitySchema != base.IdentitySchema {
		return nil, fmt.Errorf("diff identity schema %q != baseline %q", rep.IdentitySchema, base.IdentitySchema)
	}
	if len(rep.Blockers) > 0 {
		return nil, fmt.Errorf("freehand patch refused — unsupported change(s):\n  - %s", joinLines(rep.Blockers))
	}
	if rep.NoOp {
		return nil, errFreehandNoOp
	}
	if !rep.Supported {
		return nil, fmt.Errorf("freehand diff did not produce a supported patch (no changed patchable declarations)")
	}
	// CAPABILITY GATE — the diff has produced changed-patchable declarations, and nothing has been
	// synthesised yet. Every identity here is about to become a redirect on the device, so this is the
	// last point at which "the base's engine cannot honour this kind" is a message to a developer rather
	// than a patch that installs cleanly and does nothing. It runs BEFORE module synthesis so the refusal
	// costs no compile, and it consults the base's recorded capability data rather than any list here.
	changedDecls, err := changedDeclsFromDiff(rep.Changed)
	if err != nil {
		return nil, fmt.Errorf("freehand patch refused — %w", err)
	}
	if err := assertFreehandRedirectCapability(base, changedDecls); err != nil {
		return nil, fmt.Errorf("freehand patch refused — %w", err)
	}
	// CONSTANT-PROPAGATION GATE. The capability gate above asks whether the engine can honour this KIND
	// of identity. This one asks a question no runtime can answer: whether the base's own compiler
	// already replaced the calls with the value, in which case a perfectly committed redirect changes
	// nothing. See freehand_foldcheck.go for the measurement this rule is built on.
	if err := assertFreehandNoFoldedValue(relDir, changedDecls); err != nil {
		return nil, fmt.Errorf("freehand patch refused — %w", err)
	}

	closure := make([]string, 0, len(rep.NewCodeClosure))
	for _, c := range rep.NewCodeClosure {
		if ml, ok := c["manifestLine"].(string); ok {
			closure = append(closure, ml)
		}
	}
	// Derived from the VERIFIED baseline struct above. A base that cannot produce a complete identity
	// cannot produce a deliverable patch either, so this fails the patch here rather than emitting an
	// artifact that the publish gate would refuse after the compile has already been paid for.
	baseIdentity, err := richBaseIdentityFromBaseline(base)
	if err != nil {
		return nil, fmt.Errorf("freehand patch refused — %w", err)
	}
	return &FreehandPatchPlan{
		Schema:                     "soroq.freehand.patch_plan.v1",
		RuntimeID:                  runtimeID,
		AppID:                      base.AppID,
		Channel:                    base.Channel,
		Version:                    base.Version,
		IdentitySchema:             base.IdentitySchema,
		BaseAppDillSHA256:          base.AppDillSHA256,
		BaseIdentity:               &baseIdentity,
		BaseSourceKernelSHA256:     base.SourceAppDillSHA256,
		CandSourceKernelSHA256:     candSHA,
		SourceRecipeDigest:         base.SourceRecipeDigest,
		ChangedPatchable:           rep.ChangedPatchable,
		NewCodeClosure:             closure,
		Diff:                       rep,
		DependencyDescriptor:       &descriptor,
		DependencyDescriptorDigest: descriptor.DescriptorDigest,
		candidateKernelPath:        candPath,
		diffJSONPath:               filepath.Join(diffOut, "freehand_diff.json"),
		recipe:                     recipe,
		relDir:                     relDir,
		inputDigest:                inputDigest,
		capabilityMapPath:          capMapPath,
	}, nil
}

// errFreehandNoOp signals a clean no-op (no semantic change) so the caller can report it distinctly.
var errFreehandNoOp = fmt.Errorf("no patchable change detected between the base release and the current source (clean no-op)")

// assertRecipeReproducible re-derives the recipe from the CURRENT project + flutterRoot and confirms its
// digest matches the baseline recipe — i.e. the candidate would be compiled with the same frontend,
// platform, package_config, defines, and experiments. A mismatch (changed compile option / package
// config / defines / frontend) is refused BEFORE compiling the candidate.
func assertRecipeReproducible(projectDir, flutterRoot string, want FreehandSourceKernelRecipe) error {
	got, err := buildFreehandSourceKernelRecipe(projectDir, flutterRoot)
	if err != nil {
		return err
	}
	// Compare under the BASE recipe's schema semantics. A v1 base binds package_config into its digest
	// (legacy), so v1 bases are compared whole-struct; a v2 base binds only the toolchain, so a
	// dependency change leaves the v2 digest invariant and is allowed here (the dependency delta is
	// gated separately by the dependency descriptor).
	got.Schema = want.Schema
	wd, err := want.recipeDigest()
	if err != nil {
		return err
	}
	gd, err := got.recipeDigest()
	if err != nil {
		return err
	}
	if wd != gd {
		if want.isV1() {
			return fmt.Errorf("this freehand base was built before dependency-OTA support (source-kernel recipe %s binds the exact dependency graph into its digest, so a dependency/compile-input change is not patchable on it). Rebuild the base with `soroq release ios --engine --build` to produce a %s recipe that supports dependency patches. (base=%s current=%s)",
				func() string {
					if want.Schema == "" {
						return freehandRecipeSchemaV1
					}
					return want.Schema
				}(), freehandRecipeSchemaV2, wd[:12], gd[:12])
		}
		return fmt.Errorf("immutable toolchain recipe digest changed vs base (Flutter/Dart/frontend/platform/build-mode/defines/experiments differ — these cannot change in a patch): base=%s current=%s", wd[:12], gd[:12])
	}
	return nil
}

// FreehandToolchainBinding pins the exact tools + schema that produced a patch artifact. Bound into the
// artifact identity so a different toolchain (even with the same source/plan) yields a distinct immutable
// artifact, and recorded in metadata for verification.
type FreehandToolchainBinding struct {
	ToolchainVersion       string `json:"toolchain_version"`
	Dart2BytecodeSHA256    string `json:"dart2bytecode_sha256"`
	DartAotRuntimeSHA256   string `json:"dartaotruntime_sha256"`
	PlatformDillSHA256     string `json:"platform_dill_sha256"`
	CompileInterfaceSHA256 string `json:"compile_interface_sha256"` // "" for a pure-Dart (vm-target) module
	AnalyzerSnapshotSHA256 string `json:"analyzer_snapshot_sha256"`
	ModuleSchema           string `json:"module_schema"`
}

func (b FreehandToolchainBinding) digest() (string, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return freehandSHA256Bytes(raw), nil
}

// computeToolchainBinding hashes the exact tools that will produce the artifact (dart2bytecode,
// dartaotruntime, the target platform dill, the flutter compile interface when Flutter-imported, and the
// analyzer snapshot).
func computeToolchainBinding(flutterRoot, bundleDir, toolchain string, needsFlutter bool) (FreehandToolchainBinding, error) {
	b := FreehandToolchainBinding{ToolchainVersion: toolchain, ModuleSchema: "soroq.freehand.module.v2"}
	shaOf := func(p string) (string, error) {
		if !fileExists(p) {
			return "", fmt.Errorf("toolchain binding input missing: %s", p)
		}
		return sha256OfPath(p)
	}
	var err error
	if b.Dart2BytecodeSHA256, err = shaOf(filepath.Join(bundleDir, "dart2bytecode")); err != nil {
		return b, err
	}
	if b.DartAotRuntimeSHA256, err = shaOf(filepath.Join(bundleDir, "dartaotruntime")); err != nil {
		return b, err
	}
	if b.AnalyzerSnapshotSHA256, err = shaOf(filepath.Join(flutterRoot, "bin", "cache", "soroq", "soroq_kernel_analyze.dill")); err != nil {
		return b, err
	}
	if needsFlutter {
		platform, perr := flutterProfilePlatformDillFromToolchain(bundleDir)
		if perr != nil {
			return b, perr
		}
		if b.PlatformDillSHA256, err = shaOf(platform); err != nil {
			return b, err
		}
		if b.CompileInterfaceSHA256, err = shaOf(filepath.Join(bundleDir, "flutter_compile_interface")); err != nil {
			return b, err
		}
	} else {
		if b.PlatformDillSHA256, err = shaOf(filepath.Join(bundleDir, "vm_platform")); err != nil {
			return b, err
		}
	}
	return b, nil
}

// FreehandPatchArtifactMeta is the immutable, bound metadata for a generated freehand patch module.
type FreehandPatchArtifactMeta struct {
	Schema            string `json:"schema"` // freehandPatchArtifactSchema
	RuntimeID         string `json:"runtime_id"`
	IdentitySchema    string `json:"identity_schema"`
	AppID             string `json:"app_id"`
	Version           string `json:"version"`
	Channel           string `json:"channel"`
	BaseAppDillSHA256 string `json:"base_app_dill_sha256"`
	// BaseIdentity is the rich base identity carried verbatim from the plan. `base_app_dill_sha256`
	// above is the same value as its BaseFingerprint; both are kept because the former is what the
	// existing diff/verification paths read and the latter is what the device compares.
	BaseIdentity           *FreehandRichBaseIdentity `json:"base_identity_record"`
	BaseSourceKernelSHA256 string                    `json:"base_source_kernel_sha256"`
	CandSourceKernelSHA256 string                    `json:"candidate_source_kernel_sha256"`
	SourceRecipeDigest     string                    `json:"source_kernel_recipe_digest"`
	PatchPlanSHA256        string                    `json:"patch_plan_sha256"`
	ModuleSourceSHA256     string                    `json:"module_source_sha256"`
	ModuleGraphDigest      string                    `json:"module_graph_digest"`
	CarriedLibraries       []freehandCarriedLibrary  `json:"carried_libraries"`
	ModuleBytecodeSHA256   string                    `json:"module_bytecode_sha256"`
	ModuleManifestSHA256   string                    `json:"module_manifest_sha256"` // sha of soroq_freehand_module_manifest.json (the durable replacement ABI)
	ModuleLibrary          string                    `json:"module_library"`         // synthetic module-library identity for runtime lookup
	// (carried_libraries above maps every carried package library to its own synthetic URI)
	NeedsFlutterTarget     bool                      `json:"needs_flutter_target"`
	ToolchainBinding       *FreehandToolchainBinding `json:"toolchain_binding"`
	ToolchainBindingDigest string                    `json:"toolchain_binding_digest"`
	ArtifactID             string                    `json:"artifact_id"` // sha(plan_sha | toolchain_binding_digest | module_manifest_sha | dependency_descriptor_digest)
	CompilationInputDigest string                    `json:"compilation_input_digest"`
	ChangedIdentities      []string                  `json:"changed_identities"`
	ClosureIdentities      []string                  `json:"closure_identities"`
	// DependencyDescriptorDigest is bound into the artifact id: a different dependency delta produces a
	// distinct immutable artifact. The full descriptor is persisted verbatim beside it.
	DependencyDescriptorDigest string `json:"dependency_descriptor_digest"`
	DependencyDescriptorSHA256 string `json:"dependency_descriptor_sha256"`
	BaseDependencyGraphDigest  string `json:"base_dependency_graph_digest"`
}

// freehandDependencyDescriptorFile is the verbatim descriptor persisted inside every patch artifact.
const freehandDependencyDescriptorFile = "dependency_descriptor.json"

// bindDependencyDigestIntoManifest sets `dependency_descriptor_digest` on the synthesized module manifest
// and re-serializes it. Every other member is carried through as raw JSON, so nothing the analyzer emitted
// is reinterpreted or lost; only the one key is added. The result is what the CLI hashes into
// module_manifest_sha256 and persists, which is in turn bound into the artifact id — so the module can
// never be paired with a dependency descriptor other than the one it was generated under.
//
// This binding lives on the Go side on purpose: it must hold regardless of which analyzer snapshot is
// installed, and the CLI is the component that actually validated the descriptor against the base graph.
func bindDependencyDigestIntoManifest(manifestBytes []byte, digest string) ([]byte, error) {
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(manifestBytes))
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("decode synthesized module manifest: %w", err)
	}
	if dec.More() {
		return nil, errors.New("trailing data after synthesized module manifest JSON")
	}
	if existing, ok := fields["dependency_descriptor_digest"]; ok {
		var got string
		if err := json.Unmarshal(existing, &got); err != nil {
			return nil, fmt.Errorf("synthesized manifest has a malformed dependency_descriptor_digest: %w", err)
		}
		if got != "" && got != digest {
			return nil, fmt.Errorf("synthesized manifest already carries a different dependency_descriptor_digest %q (want %q)", got, digest)
		}
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		return nil, err
	}
	fields["dependency_descriptor_digest"] = encoded
	// map[string]json.RawMessage marshals with sorted keys and verbatim values ⇒ deterministic output.
	return json.MarshalIndent(fields, "", "  ")
}

// computeFreehandArtifactID derives the immutable artifact identity from every component that changes
// what the artifact IS: the plan, the exact toolchain, the durable replacement ABI, and the dependency
// delta. It is recomputed from re-derived inputs on every verification — nothing stored is trusted.
func computeFreehandArtifactID(planSHA, bindingDigest, manifestSHA, dependencyDigest string) string {
	return freehandSHA256Bytes([]byte(planSHA + "|" + bindingDigest + "|" + manifestSHA + "|" + dependencyDigest))
}

// revalidateDependencyDescriptor re-resolves the candidate runtime dependency graph from disk and
// requires the plan's already-validated descriptor to still match it, then re-runs Assess. It is called
// before module generation, before publication, and whenever a persisted artifact is verified — so a
// dependency change slipped in mid-flight cannot reach a device.
func revalidateDependencyDescriptor(projectDir string, plan *FreehandPatchPlan, stage string) error {
	if plan.DependencyDescriptor == nil {
		return fmt.Errorf("patch plan carries no dependency descriptor (refusing to proceed to %s)", stage)
	}
	if err := plan.DependencyDescriptor.Validate(); err != nil {
		return fmt.Errorf("dependency descriptor became invalid before %s: %w", stage, err)
	}
	if plan.DependencyDescriptorDigest != plan.DependencyDescriptor.DescriptorDigest {
		return fmt.Errorf("patch plan dependency_descriptor_digest %s != the descriptor's own digest %s",
			plan.DependencyDescriptorDigest, plan.DependencyDescriptor.DescriptorDigest)
	}
	fresh, err := resolveRuntimeGraphPinned(projectDir)
	if err != nil {
		return fmt.Errorf("re-resolve the candidate runtime dependency graph before %s: %w", stage, err)
	}
	if err := plan.DependencyDescriptor.AssertMatchesCandidate(fresh); err != nil {
		return fmt.Errorf("dependencies changed before %s: %w", stage, err)
	}
	if err := plan.DependencyDescriptor.Assess(); err != nil {
		return fmt.Errorf("freehand patch refused before %s — %w", stage, err)
	}
	return nil
}

// freehandModuleManifest is the DURABLE replacement ABI persisted alongside the module. The WHOLE schema is
// modeled so a strict (DisallowUnknownFields) decode rejects any unknown/renamed field at every level; the
// file is also SHA-bound into patch_artifact.json + the artifact id. Go independently re-validates the ABI
// against patch_plan.json's diff (manifestLine + frozen v1 key per changed decl) — it does not trust a
// single field. The analyzer owns deep Dart resolvability; Go owns the manifest schema + the ABI bijection.
const freehandModuleSchema = "soroq.freehand.module.v2"

// freehandPatchArtifactSchema is bumped alongside the module schema because the artifact now REQUIRES
// module_graph_digest and carried_libraries. An older record cannot express them, so it is refused
// rather than read with zero values.
const freehandPatchArtifactSchema = "soroq.freehand.patch_artifact.v2"

type freehandModuleManifest struct {
	Schema               string          `json:"schema"`
	ModuleSource         json.RawMessage `json:"module_source,omitempty"` // [step5 acceptance] tolerate the installed analyzer's supplemental source field
	ModuleSourceBasename string          `json:"module_source_basename"`
	ModuleLibrary        string          `json:"module_library"`
	ModuleSourceSHA256   string          `json:"module_source_sha256"`
	// The namespace every synthetic URI in this artifact derives from -- covering the WHOLE module
	// graph, not just the main source file, so an upgrade touching only a carried library still moves
	// the namespace and cannot collide with an already-loaded library.
	ModuleGraphDigest string `json:"module_graph_digest"`
	// Strict one-to-one mapping of every carried package library to its synthetic URI. A replacement
	// ABI entry may reference the main module library OR exactly one of these.
	CarriedLibraries           []freehandCarriedLibrary `json:"carried_libraries"`
	NeedsFlutterTarget         bool                     `json:"needs_flutter_target"`
	Imports                    []string                 `json:"imports"`
	ExtractedTopLevelFunctions []string                 `json:"extracted_top_level_functions"`
	ExtractedClasses           []string                 `json:"extracted_classes"`
	CarriedChangedMembers      []string                 `json:"carried_changed_members"`
	ValueExercised             []string                 `json:"value_exercised"`
	// Declarations that are CARRIED but deliberately not exercised, keyed by stable identity, each with
	// its reason. Present so "the entrypoint exercised nothing" is always distinguishable from "nothing
	// needed exercising" -- a silent gap otherwise reads as coverage.
	ExerciseSkipped            map[string]string          `json:"exercise_skipped"`
	HostInvokedInstanceMethods []string                   `json:"host_invoked_instance_methods"`
	ReplacementABI             []freehandReplacementEntry `json:"replacement_abi"`
	CarriedNewCode             []freehandCarriedEntry     `json:"carried_new_code"`
	// Declarations carried BY VALUE from packages that are NEW in this patch. They are inlined into the
	// module library rather than imported, because the installed base contains no such library.
	CarriedPackageFiles     []string `json:"carried_package_files"`
	CarriedPackageLibraries []string `json:"carried_package_libraries"`
	// ModuleSourceTree is the canonical manifest of every generated module-local source file.
	ModuleSourceTree       []freehandModuleTreeEntry `json:"module_source_tree"`
	ModuleSourceTreeDigest string                    `json:"module_source_tree_digest"`
	// DependencyDescriptorDigest is the dependency delta this module was synthesized under. Because the
	// manifest's SHA is bound into the artifact id, this makes the dependency descriptor part of the
	// artifact's identity: swapping in a different descriptor changes the artifact.
	DependencyDescriptorDigest string `json:"dependency_descriptor_digest"`
}

// freehandModuleTreeEntry is one generated module-local source file: its module-relative path and the
// SHA-256 of its contents. The set is bound into the module graph digest and thence the artifact id.
type freehandModuleTreeEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

type freehandReplacementEntry struct {
	BaseIdentity    string `json:"base_identity"`
	StableIdentity  string `json:"stable_identity"`
	ModuleLibrary   string `json:"module_library"`
	ModuleClass     string `json:"module_class"`
	ModuleMember    string `json:"module_member"`
	Kind            string `json:"kind"`
	SignatureSHA256 string `json:"signature_sha256"`
	HostInvocable   bool   `json:"host_invocable"`
}

type freehandCarriedEntry struct {
	Identity       string `json:"identity"`
	StableIdentity string `json:"stable_identity"`
	ModuleLibrary  string `json:"module_library"`
	ModuleClass    string `json:"module_class"`
	ModuleMember   string `json:"module_member"`
	Kind           string `json:"kind"`
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// abiKinds is the exact set of replacement_abi `kind` values the analyzer emits.
var abiKinds = map[string]bool{"function": true, "static-method": true, "instance-member": true}

// changedDecl is the authoritative identity of one changed-patchable declaration, taken from the SHA-bound
// patch_plan.json diff: its manifestLine (libUri::class::vmName) AND its frozen identity-schema-v1 key.
type changedDecl struct {
	manifestLine string // == replacement_abi.base_identity
	stableKey    string // == replacement_abi.stable_identity ("v1|libUri|kind|class|member|sigShort")
	keyKind      string // SEMANTIC kind segment parsed from stableKey (never an ABI shape label)
	keyClass     string // class segment parsed from stableKey
	keyMember    string // member segment parsed from stableKey
	keyIsFunc    bool   // stableKey kind segment == "function"
}

// freehandSemanticKinds is the universe of frozen identity kinds — the third segment of a v1 stable
// identity. expectABI below is the authority on what each one means, so this set is exactly the set of
// cases in its switch; TestSemanticKindUniverseMatchesExpectABI holds the two in agreement, so a kind
// added there can never become a kind a base's capability record is unable to name.
var freehandSemanticKinds = map[string]bool{
	"function": true, "method": true, "static-method": true,
	"getter": true, "setter": true, "operator": true,
	"constructor": true, "factory": true, "field-initializer": true,
}

func freehandSemanticKindList() string {
	out := make([]string, 0, len(freehandSemanticKinds))
	for k := range freehandSemanticKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// assertFreehandRedirectCapability refuses, BEFORE any module is synthesised, a changed identity whose
// semantic kind the base's engine has not been shown to honour.
//
// THE FAILURE THIS EXISTS FOR. Three constructor identities were published, verified, staged and
// committed on a real iPhone (`transition.result committed=4`) and changed nothing observable. Every
// producer- and transport-side guard passed, correctly: the names were the VM's own, the ABI was a valid
// bijection with measured shapes, the signature verified, the engine resolved both ends and set the
// slot. Setting a slot the running code never consults is indistinguishable, from every one of those
// vantage points, from success. This is the only place that asks whether the base's ENGINE can honour a
// redirect on that KIND of identity rather than merely resolve one.
//
// It is ADDITIVE: it relaxes nothing in parseAndValidateModuleManifest, and an entry it admits still
// faces every ABI, bijection and shape check unchanged. And it decides from the base's RECORDED data,
// so an engine build that declares constructor support unlocks constructors through this same code.
func assertFreehandRedirectCapability(base *FreehandBaselineMeta, decls []changedDecl) error {
	caps, err := baseRedirectCapabilities(base)
	if err != nil {
		return fmt.Errorf("cannot decide what this base's engine can honour: %w", err)
	}
	honoured := caps.kindSet()
	refused := make([]string, 0, len(decls))
	kinds := make([]string, 0, len(decls))
	seenKind := map[string]bool{}
	for _, d := range decls {
		if honoured[d.keyKind] {
			continue
		}
		refused = append(refused, fmt.Sprintf("kind %q: %s   (frozen identity %s)", d.keyKind, d.manifestLine, d.stableKey))
		if !seenKind[d.keyKind] {
			seenKind[d.keyKind] = true
			kinds = append(kinds, d.keyKind)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	sort.Strings(kinds)
	return fmt.Errorf("this base's engine does not honour redirects on %s identities, so the patch would install, commit and change NOTHING on device:\n  - %s\n"+
		"  base engine %s honours: [%s] (source: %s)\n"+
		"  why: %s\n"+
		"  A redirect on an unhonoured kind resolves, passes every ABI check and commits — and the running code never consults the slot,\n"+
		"  so the app keeps executing base code while every layer reports success. Refusing here makes that silence loud.\n"+
		"  To ship it: release a new base built on an engine whose bundle declares %s including %s — no producer change is needed, the\n"+
		"  capability is read from the base.",
		strings.Join(kinds, "/"), joinLines(refused),
		caps.EngineRevision, caps.kindList(), caps.Source, caps.Note,
		freehandEngineCapabilityKey, strings.Join(kinds, "/"))
}

// abiExpectation is what a frozen identity requires of its replacement_abi entry: the VM name the
// identity must carry at BOTH ends, and the shape labels the runtime will accept for it.
type abiExpectation struct {
	vmName string
	kinds  map[string]bool
}

func (e abiExpectation) kindList() string {
	out := make([]string, 0, len(e.kinds))
	for k := range e.kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, "|")
}

// expectABI derives, from the frozen identity ALONE, the VM name the identity must carry and the ABI
// shape labels that are legal for it.
//
// SEMANTIC KINDS ARE NOT ABI KINDS. The engine's SoroqKindConsistent accepts exactly
// function / static-method / instance-member and returns false — which throws — for anything else, so
// a semantic kind like "constructor" reaching the ABI is a device-side crash rather than a
// producer-side refusal. Each semantic kind is mapped onto the shape the VM actually builds:
//
//	generative constructor  -> a NON-static VM function (kernel_loader.cc builds kConstructor with
//	                           is_static false; object.cc:6708 asserts it)      -> instance-member
//	factory                 -> a STATIC constructor (object.cc:6711 asserts it) -> static-method
//	top-level init:<field>  -> a static function of the library toplevel class  -> function
//	static class init:<f>   -> a static method of that class                    -> static-method
//
// An unrecognized semantic kind is an ERROR, not a pass-through: a kind this function does not model
// is a kind whose VM shape nobody has measured.
func expectABI(d changedDecl) (abiExpectation, error) {
	set := func(k ...string) map[string]bool {
		m := make(map[string]bool, len(k))
		for _, s := range k {
			m[s] = true
		}
		return m
	}
	hasClass := d.keyClass != ""
	switch d.keyKind {
	case "function":
		if hasClass {
			return abiExpectation{}, fmt.Errorf("frozen kind \"function\" carries class %q", d.keyClass)
		}
		return abiExpectation{vmName: d.keyMember, kinds: set("function")}, nil
	case "method":
		if !hasClass {
			return abiExpectation{}, errors.New("frozen kind \"method\" carries no class")
		}
		return abiExpectation{vmName: d.keyMember, kinds: set("instance-member")}, nil
	case "static-method":
		if !hasClass {
			return abiExpectation{}, errors.New("frozen kind \"static-method\" carries no class")
		}
		return abiExpectation{vmName: d.keyMember, kinds: set("static-method")}, nil
	case "getter", "setter", "operator":
		// The VM names an accessor with its prefix; the bare source name resolves to nothing.
		vm := d.keyMember
		if d.keyKind == "getter" {
			vm = "get:" + d.keyMember
		} else if d.keyKind == "setter" {
			vm = "set:" + d.keyMember
		}
		if !hasClass {
			return abiExpectation{vmName: vm, kinds: set("function")}, nil
		}
		// Staticness is measured by the analyzer from the kernel; the frozen key does not carry it.
		return abiExpectation{vmName: vm, kinds: set("instance-member", "static-method")}, nil
	case "constructor":
		if !hasClass {
			return abiExpectation{}, errors.New("frozen kind \"constructor\" carries no class")
		}
		// THE DOT IS ALWAYS PRESENT. An unnamed constructor is `Foo.`, never `Foo`.
		return abiExpectation{vmName: d.keyClass + "." + d.keyMember, kinds: set("instance-member")}, nil
	case "factory":
		if !hasClass {
			return abiExpectation{}, errors.New("frozen kind \"factory\" carries no class")
		}
		return abiExpectation{vmName: d.keyClass + "." + d.keyMember, kinds: set("static-method")}, nil
	case "field-initializer":
		if d.keyMember == "" {
			return abiExpectation{}, errors.New("field-initializer identity carries no field name")
		}
		if hasClass {
			return abiExpectation{vmName: "init:" + d.keyMember, kinds: set("static-method")}, nil
		}
		return abiExpectation{vmName: "init:" + d.keyMember, kinds: set("function")}, nil
	default:
		return abiExpectation{}, fmt.Errorf("frozen identity kind %q has no measured VM shape; refusing "+
			"to emit an ABI entry for a shape nobody has measured", d.keyKind)
	}
}

// splitIdentity splits `<libUri>::<class>::<vmName>` into its three segments. Library URIs contain
// single colons but never "::", so the split is unambiguous.
func splitIdentity(id string) (lib, class, vmName string, err error) {
	parts := strings.Split(id, "::")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("identity %q is not <libUri>::<class>::<vmName>", id)
	}
	return parts[0], parts[1], parts[2], nil
}

// changedDeclsFromDiff extracts the changed-patchable declarations from a diff report's `changed` array
// (each entry carries both `manifestLine` and the frozen v1 `key`). Fails closed on a malformed/duplicate key.
func changedDeclsFromDiff(diffChanged []map[string]any) ([]changedDecl, error) {
	out := make([]changedDecl, 0, len(diffChanged))
	for _, c := range diffChanged {
		patchable, _ := c["patchable"].(bool)
		if !patchable {
			continue
		}
		ml, _ := c["manifestLine"].(string)
		key, _ := c["key"].(string)
		if ml == "" || key == "" {
			return nil, fmt.Errorf("diff changed entry missing manifestLine/key: %+v", c)
		}
		parts := strings.Split(key, "|")
		if len(parts) != 6 || parts[0] != "v1" {
			return nil, fmt.Errorf("malformed frozen identity key %q", key)
		}
		out = append(out, changedDecl{
			manifestLine: ml,
			stableKey:    key,
			keyKind:      parts[2],
			keyClass:     parts[3],
			keyMember:    parts[4],
			keyIsFunc:    parts[2] == "function",
		})
	}
	return out, nil
}

// parseAndValidateModuleManifest strictly decodes the manifest (whole schema modeled → unknown/trailing JSON
// refused) and enforces the durable-ABI contract against the plan's changed-patchable declarations:
//   - schema == soroq.freehand.module.v2; non-empty module_library
//   - replacement_abi is a BIJECTION with the changed decls by BOTH base_identity (manifestLine) AND
//     stable_identity (frozen v1 key) — no missing/duplicate/extra/swapped entry
//   - each entry: per-entry module_library == the top-level module_library; kind ∈ {function,static-method,
//     instance-member} and shape-consistent (function ⟺ empty class); module_class/module_member equal the
//     class/member parsed from the frozen key; signature_sha256 is 64-hex
//   - carried_new_code entries are well-formed, share the module_library, and never collide with a changed id
//
// Returns the validated top-level module_library (bound into patch_artifact.json).
func parseAndValidateModuleManifest(manifestBytes []byte, diffChanged []map[string]any, wantDependencyDigest string) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(manifestBytes))
	dec.DisallowUnknownFields()
	var m freehandModuleManifest
	if err := dec.Decode(&m); err != nil {
		return "", fmt.Errorf("decode module manifest: %w", err)
	}
	if dec.More() {
		return "", errors.New("trailing data after module manifest JSON")
	}
	if m.Schema != freehandModuleSchema {
		return "", fmt.Errorf("unexpected module manifest schema %q (want %q)", m.Schema, freehandModuleSchema)
	}
	if strings.TrimSpace(m.ModuleLibrary) == "" {
		return "", errors.New("module manifest missing module_library identity")
	}
	// The module must have been synthesized under the SAME dependency descriptor the plan validated.
	if m.DependencyDescriptorDigest != wantDependencyDigest {
		return "", fmt.Errorf("module manifest dependency_descriptor_digest %q != the validated descriptor %q — the module was synthesized under a different dependency delta",
			m.DependencyDescriptorDigest, wantDependencyDigest)
	}

	decls, err := changedDeclsFromDiff(diffChanged)
	if err != nil {
		return "", err
	}
	// Expected bijection: manifestLine -> decl, and stableKey -> decl (the PAIR must match to catch swaps).
	wantByLine := make(map[string]changedDecl, len(decls))
	wantByStable := make(map[string]changedDecl, len(decls))
	for _, d := range decls {
		if _, dup := wantByLine[d.manifestLine]; dup {
			return "", fmt.Errorf("diff has duplicate changed manifestLine %s", d.manifestLine)
		}
		if _, dup := wantByStable[d.stableKey]; dup {
			return "", fmt.Errorf("diff has duplicate changed stable key %s", d.stableKey)
		}
		wantByLine[d.manifestLine] = d
		wantByStable[d.stableKey] = d
	}

	changedIDs := make(map[string]bool, len(decls)) // manifestLines, to reject a closure entry that shadows a change
	for _, d := range decls {
		changedIDs[d.manifestLine] = true
	}

	// STRICT carried-library validation, before any entry is resolved against it: canonical URIs,
	// lowercase hashes, sorted unique paths, and exact agreement with the module source tree that was
	// hashed. Recording a mapping is not validating it.
	treeSHA := map[string]string{}
	for _, e := range m.ModuleSourceTree {
		treeSHA[e.Path] = e.SHA256
	}
	if m.ModuleGraphDigest != "" || len(m.CarriedLibraries) > 0 {
		if err := validateCarriedLibraries(m.ModuleGraphDigest, m.ModuleLibrary, m.CarriedLibraries, treeSHA); err != nil {
			return "", fmt.Errorf("carried-library mapping is invalid: %w", err)
		}
	}
	// And no entry may redirect one package's identity into another package's carried library.
	if err := validateABIPackageAgreement(m.ReplacementABI, m.ModuleLibrary, m.CarriedLibraries); err != nil {
		return "", err
	}
	seenLine := make(map[string]bool, len(m.ReplacementABI))
	seenStable := make(map[string]bool, len(m.ReplacementABI))
	for _, e := range m.ReplacementABI {
		if e.BaseIdentity == "" || e.StableIdentity == "" || e.ModuleMember == "" || e.Kind == "" {
			return "", fmt.Errorf("replacement_abi entry missing required field: %+v", e)
		}
		// An ABI entry may name the MAIN module library, or exactly one strictly-recorded carried
		// library. Requiring equality with the main module was wrong once a changed declaration could
		// live inside a carried package library (a dependency UPGRADE), and it is the assumption that
		// forced such identities to be dropped.
		if err := m.resolveEntryLibrary(e.ModuleLibrary, e.BaseIdentity); err != nil {
			return "", err
		}
		if !abiKinds[e.Kind] {
			return "", fmt.Errorf("replacement_abi entry %s has unknown kind %q (the runtime accepts only "+
				"function, static-method and instance-member and throws on anything else)", e.BaseIdentity, e.Kind)
		}
		if !sha256HexRe.MatchString(e.SignatureSHA256) {
			return "", fmt.Errorf("replacement_abi entry %s has malformed signature_sha256", e.BaseIdentity)
		}
		if seenLine[e.BaseIdentity] {
			return "", fmt.Errorf("duplicate replacement_abi base_identity %s", e.BaseIdentity)
		}
		if seenStable[e.StableIdentity] {
			return "", fmt.Errorf("duplicate replacement_abi stable_identity %s", e.StableIdentity)
		}
		seenLine[e.BaseIdentity] = true
		seenStable[e.StableIdentity] = true

		d, ok := wantByLine[e.BaseIdentity]
		if !ok {
			return "", fmt.Errorf("replacement_abi base_identity %s is not a changed identity (extra/unresolvable)", e.BaseIdentity)
		}
		// The (base_identity, stable_identity) PAIR must match the SAME changed decl — catches a swapped
		// stable_identity taken from a different (or nonexistent) declaration.
		if e.StableIdentity != d.stableKey {
			return "", fmt.Errorf("replacement_abi entry %s stable_identity %q != frozen key %q", e.BaseIdentity, e.StableIdentity, d.stableKey)
		}
		// module_class / module_member must equal the class/member parsed from the frozen key.
		if e.ModuleClass != d.keyClass {
			return "", fmt.Errorf("replacement_abi entry %s module_class %q != frozen key class %q", e.BaseIdentity, e.ModuleClass, d.keyClass)
		}
		// kind shape must be internally consistent: `function` ⟺ empty class.
		abiIsFunc := e.Kind == "function"
		if abiIsFunc != (e.ModuleClass == "") {
			return "", fmt.Errorf("replacement_abi entry %s kind %q is inconsistent with class %q", e.BaseIdentity, e.Kind, e.ModuleClass)
		}
		// The frozen SEMANTIC kind decides both the legal shape label and the VM name, and the VM name is
		// checked at BOTH ends. The base identity is what the runtime looks the base function up by; the
		// module member is what it looks the replacement up by; they go through the SAME lookup, so a name
		// that is right at one end and wrong at the other resolves to nothing and throws on device.
		exp, err := expectABI(d)
		if err != nil {
			return "", fmt.Errorf("replacement_abi entry %s: %w", e.BaseIdentity, err)
		}
		if !exp.kinds[e.Kind] {
			return "", fmt.Errorf("replacement_abi entry %s has kind %q but frozen kind %q is built by the VM as %s",
				e.BaseIdentity, e.Kind, d.keyKind, exp.kindList())
		}
		if e.ModuleMember != exp.vmName {
			return "", fmt.Errorf("replacement_abi entry %s module_member %q != the VM name %q for frozen kind %q",
				e.BaseIdentity, e.ModuleMember, exp.vmName, d.keyKind)
		}
		_, baseClass, baseVMName, err := splitIdentity(e.BaseIdentity)
		if err != nil {
			return "", fmt.Errorf("replacement_abi entry: %w", err)
		}
		if baseClass != d.keyClass {
			return "", fmt.Errorf("replacement_abi base_identity %s names class %q but the frozen key names %q",
				e.BaseIdentity, baseClass, d.keyClass)
		}
		if baseVMName != exp.vmName {
			return "", fmt.Errorf("replacement_abi base_identity %s carries VM name %q but frozen kind %q requires %q "+
				"(a missing constructor dot or accessor prefix matches nothing at runtime)",
				e.BaseIdentity, baseVMName, d.keyKind, exp.vmName)
		}
	}
	// No missing: every changed decl must have exactly one entry (bijection completeness).
	for _, d := range decls {
		if !seenLine[d.manifestLine] {
			return "", fmt.Errorf("changed identity %s has no replacement_abi entry", d.manifestLine)
		}
		if !seenStable[d.stableKey] {
			return "", fmt.Errorf("changed frozen key %s has no replacement_abi entry", d.stableKey)
		}
	}

	// carried_new_code: well-formed, shares the module_library, and must NOT shadow a changed identity.
	for _, c := range m.CarriedNewCode {
		if c.Identity == "" || c.StableIdentity == "" || c.Kind == "" {
			return "", fmt.Errorf("carried_new_code entry missing required field: %+v", c)
		}
		if err := m.resolveEntryLibrary(c.ModuleLibrary, c.Identity); err != nil {
			return "", err
		}
		if changedIDs[c.Identity] {
			return "", fmt.Errorf("carried_new_code entry %s collides with a changed identity", c.Identity)
		}
	}
	return m.ModuleLibrary, nil
}

// artifactFiles is the exact set every immutable patch artifact must contain (for strict reuse checks).
var artifactFiles = []string{
	"soroq_freehand_module.dart",
	"soroq_freehand_module.bytecode",
	"soroq_freehand_module_manifest.json",
	"patch_plan.json",
	"patch_artifact.json",
	freehandDependencyDescriptorFile,
}

// verifyExistingPatchArtifact strictly re-verifies an on-disk artifact from ITS OWN FILES before reuse.
// It trusts no single stored field: it re-derives patch_plan_sha256, the toolchain-binding digest, the
// module-manifest SHA, and the artifact id from the actual bytes on disk and requires all of them to agree
// with patch_artifact.json AND the caller's expected identity. Every artifact member is rehashed; the
// replacement ABI is validated as a bijection with the recorded changed identities. Any deviation — a
// tampered file, an edited metadata field, a symlink, unknown/trailing JSON — is an error (never silent reuse).
func verifyExistingPatchArtifact(dir, expectArtifactID string) error {
	for _, f := range artifactFiles {
		fi, err := os.Lstat(filepath.Join(dir, f))
		if err != nil {
			return fmt.Errorf("artifact missing %s: %w", f, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact %s is a symlink", f)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("artifact %s is not a regular file", f)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "patch_artifact.json"))
	if err != nil {
		return err
	}
	var m FreehandPatchArtifactMeta
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("corrupt/unknown-field patch_artifact.json: %w", err)
	}
	if dec.More() {
		return errors.New("trailing data after patch_artifact.json")
	}
	if m.Schema != freehandPatchArtifactSchema {
		return fmt.Errorf("unexpected patch_artifact schema %q (want %q)", m.Schema, freehandPatchArtifactSchema)
	}
	// The v2 graph fields are REQUIRED, not optional-with-a-zero-default. An artifact that records no
	// namespace has nothing to cross-check its ABI's synthetic URIs against, so it is refused rather
	// than accepted as "probably fine".
	if !sha256HexRe.MatchString(m.ModuleGraphDigest) {
		return fmt.Errorf("artifact records no valid module_graph_digest (%q); it predates per-graph "+
			"module namespaces — rebuild the patch", m.ModuleGraphDigest)
	}
	if err := validateCarriedLibraries(m.ModuleGraphDigest, m.ModuleLibrary, m.CarriedLibraries, nil); err != nil {
		return fmt.Errorf("artifact carried-library mapping is invalid: %w", err)
	}
	// The rich base identity is REQUIRED, not optional-with-a-zero-default. An artifact that records no
	// identity can only be bound by runtime_id, which is version-derived and shared by every base with
	// the same app/channel/version/trust — so it is deliverable to a base it was never compiled for.
	if m.BaseIdentity == nil {
		return errors.New("artifact records no rich base identity; it predates the four-field base identity — rebuild the patch")
	}
	if err := m.BaseIdentity.validate(); err != nil {
		return fmt.Errorf("artifact base identity is invalid: %w", err)
	}
	// Cross-check against the artifact's OWN records. A swapped identity block would otherwise be
	// perfectly self-consistent — it recomputes its own digest — while describing a different base than
	// the one the module was actually diffed against.
	if m.BaseIdentity.RuntimeID != m.RuntimeID {
		return fmt.Errorf("artifact base identity runtime_id %q != artifact runtime_id %q", m.BaseIdentity.RuntimeID, m.RuntimeID)
	}
	if m.BaseIdentity.BaseFingerprint != m.BaseAppDillSHA256 {
		return fmt.Errorf("artifact base identity base_fingerprint %s != artifact base_app_dill_sha256 %s",
			short12(m.BaseIdentity.BaseFingerprint), short12(m.BaseAppDillSHA256))
	}
	// Rehash the module source + bytecode against the recorded values.
	if got, err := sha256OfPath(filepath.Join(dir, "soroq_freehand_module.dart")); err != nil || got != m.ModuleSourceSHA256 {
		return fmt.Errorf("artifact module source hash mismatch: %s != %s", got, m.ModuleSourceSHA256)
	}
	if got, err := sha256OfPath(filepath.Join(dir, "soroq_freehand_module.bytecode")); err != nil || got != m.ModuleBytecodeSHA256 {
		return fmt.Errorf("artifact module bytecode hash mismatch: %s != %s", got, m.ModuleBytecodeSHA256)
	}
	// Re-derive the patch-plan SHA from patch_plan.json (previously only presence-checked).
	planRaw, err := os.ReadFile(filepath.Join(dir, "patch_plan.json"))
	if err != nil {
		return err
	}
	planSHA := freehandSHA256Bytes(planRaw)
	if planSHA != m.PatchPlanSHA256 {
		return fmt.Errorf("patch_plan.json hash mismatch: %s != %s", planSHA, m.PatchPlanSHA256)
	}
	// Cross-check the artifact metadata against the SHA-BOUND module manifest, so the two cannot drift:
	// the manifest is the thing the ABI lives in, and the metadata is what verification reads.
	if manRaw, err := os.ReadFile(filepath.Join(dir, "soroq_freehand_module_manifest.json")); err == nil {
		var mm struct {
			ModuleLibrary     string                   `json:"module_library"`
			ModuleGraphDigest string                   `json:"module_graph_digest"`
			CarriedLibraries  []freehandCarriedLibrary `json:"carried_libraries"`
		}
		if err := json.Unmarshal(manRaw, &mm); err != nil {
			return fmt.Errorf("corrupt module manifest: %w", err)
		}
		if mm.ModuleGraphDigest != m.ModuleGraphDigest {
			return fmt.Errorf("artifact module_graph_digest %s != module manifest %s",
				short(m.ModuleGraphDigest), short(mm.ModuleGraphDigest))
		}
		if mm.ModuleLibrary != m.ModuleLibrary {
			return fmt.Errorf("artifact module_library %q != module manifest %q", m.ModuleLibrary, mm.ModuleLibrary)
		}
		if len(mm.CarriedLibraries) != len(m.CarriedLibraries) {
			return fmt.Errorf("artifact records %d carried libraries but the module manifest records %d",
				len(m.CarriedLibraries), len(mm.CarriedLibraries))
		}
		for i := range mm.CarriedLibraries {
			if mm.CarriedLibraries[i] != m.CarriedLibraries[i] {
				return fmt.Errorf("carried library %d disagrees between the artifact and the module manifest",
					i)
			}
		}
	}
	// Decode the (now SHA-verified) plan for its diff — the authoritative source of the changed decls'
	// manifestLines + frozen identity keys that the replacement ABI must match exactly.
	var plan FreehandPatchPlan
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		return fmt.Errorf("decode patch_plan.json: %w", err)
	}
	if plan.Diff == nil {
		return errors.New("patch_plan.json missing diff report")
	}
	// Re-derive the module-manifest SHA and validate the durable replacement ABI against the recorded
	// changed identities (the analyzer owns deep resolvability; Go enforces SHA binding + bijection).
	manifestRaw, err := os.ReadFile(filepath.Join(dir, "soroq_freehand_module_manifest.json"))
	if err != nil {
		return err
	}
	manifestSHA := freehandSHA256Bytes(manifestRaw)
	if manifestSHA != m.ModuleManifestSHA256 {
		return fmt.Errorf("module manifest hash mismatch: %s != %s", manifestSHA, m.ModuleManifestSHA256)
	}
	// The artifact's advertised changed_identities must equal the plan diff's changed-patchable manifestLines
	// (else a tampered patch_artifact.json changed_identities field would diverge from the SHA-bound plan).
	planChanged, err := changedDeclsFromDiff(plan.Diff.Changed)
	if err != nil {
		return fmt.Errorf("patch_plan.json diff invalid: %w", err)
	}
	wantChanged := make(map[string]bool, len(planChanged))
	for _, d := range planChanged {
		wantChanged[d.manifestLine] = true
	}
	if len(wantChanged) != len(m.ChangedIdentities) {
		return fmt.Errorf("changed_identities count %d != plan changed-patchable count %d", len(m.ChangedIdentities), len(wantChanged))
	}
	for _, id := range m.ChangedIdentities {
		if !wantChanged[id] {
			return fmt.Errorf("changed_identities entry %s is not a plan changed-patchable identity", id)
		}
	}
	// Re-derive the dependency descriptor from ITS OWN FILE and require it to agree with the SHA-bound
	// plan AND the artifact record. DecodeStrict rejects unknown fields, trailing JSON, malformed hashes,
	// a digest that does not match the content, and any internally contradictory record set — so a
	// fully-rebound forgery (outer hashes recomputed) is still caught on semantic grounds.
	descRaw, err := os.ReadFile(filepath.Join(dir, freehandDependencyDescriptorFile))
	if err != nil {
		return err
	}
	descSHA := freehandSHA256Bytes(descRaw)
	if descSHA != m.DependencyDescriptorSHA256 {
		return fmt.Errorf("dependency descriptor hash mismatch: %s != %s", descSHA, m.DependencyDescriptorSHA256)
	}
	desc, err := depgraph.DecodeStrict(descRaw)
	if err != nil {
		return fmt.Errorf("artifact dependency descriptor is invalid: %w", err)
	}
	if desc.DescriptorDigest != m.DependencyDescriptorDigest {
		return fmt.Errorf("artifact dependency_descriptor_digest %s != the descriptor's own digest %s", m.DependencyDescriptorDigest, desc.DescriptorDigest)
	}
	if plan.DependencyDescriptorDigest != desc.DescriptorDigest {
		return fmt.Errorf("patch_plan.json dependency_descriptor_digest %s != the persisted descriptor %s", plan.DependencyDescriptorDigest, desc.DescriptorDigest)
	}
	if desc.BaseGraphDigest != m.BaseDependencyGraphDigest {
		return fmt.Errorf("artifact base_dependency_graph_digest %s != the descriptor's base anchor %s", m.BaseDependencyGraphDigest, desc.BaseGraphDigest)
	}
	// Semantic re-assessment: a descriptor that is not code-only deliverable can never be a valid
	// artifact, no matter how consistent its hashes are.
	if err := desc.Assess(); err != nil {
		return fmt.Errorf("artifact carries a dependency change that is not deliverable: %w", err)
	}

	moduleLib, err := parseAndValidateModuleManifest(manifestRaw, plan.Diff.Changed, desc.DescriptorDigest)
	if err != nil {
		return fmt.Errorf("invalid durable replacement ABI: %w", err)
	}
	if moduleLib != m.ModuleLibrary {
		return fmt.Errorf("module_library mismatch: manifest %q != artifact %q", moduleLib, m.ModuleLibrary)
	}
	// Re-derive the toolchain-binding digest from the recorded binding.
	if m.ToolchainBinding == nil {
		return errors.New("artifact missing toolchain_binding")
	}
	bindingDigest, err := m.ToolchainBinding.digest()
	if err != nil {
		return err
	}
	if bindingDigest != m.ToolchainBindingDigest {
		return fmt.Errorf("toolchain_binding_digest mismatch: %s != %s", m.ToolchainBindingDigest, bindingDigest)
	}
	// Recompute the artifact identity from the re-derived components — trust nothing stored — and require it
	// to equal BOTH the recorded id and the caller's expected id.
	artifactID := computeFreehandArtifactID(planSHA, bindingDigest, manifestSHA, desc.DescriptorDigest)
	if artifactID != m.ArtifactID {
		return fmt.Errorf("recomputed artifact_id %s != recorded %s", artifactID, m.ArtifactID)
	}
	if artifactID != expectArtifactID {
		return fmt.Errorf("artifact_id mismatch: recomputed %s != expected %s", artifactID, expectArtifactID)
	}
	return nil
}

// runPatchIOSEngineFreehand is the `soroq patch ios --engine` entry for a freehand base (no patchable
// list). It runs the FULL pipeline: verified baseline -> candidate source kernel -> semantic diff ->
// closure -> AUTOMATIC module synthesis -> dart2bytecode compilation -> immutable artifact + bound
// metadata under .soroq/patches/<runtime-id>/<plan-sha>/. Fails closed (persists nothing) on any
// unsupported change or compile failure.
func runPatchIOSEngineFreehand(head, passthrough []string, projectDir string) error {
	flutterBin, err := resolveSoroqFlutterBin()
	if err != nil {
		return err
	}
	flutterRoot, err := flutterRootFromBin(flutterBin)
	if err != nil {
		return err
	}
	toolchain, _ := flagValue(head, "toolchain")
	if strings.TrimSpace(toolchain) == "" {
		return errors.New("`soroq patch ios --engine` (freehand) requires --toolchain <version> (the cached iOS toolchain that provides dart2bytecode + the flutter compile interface)")
	}
	if _, _, err := installFreehandAnalyzer(flutterRoot); err != nil {
		return fmt.Errorf("install freehand analyzer: %w", err)
	}
	plan, err := computeFreehandPatchPlan(projectDir, flutterRoot)
	if err != nil {
		if err == errFreehandNoOp {
			fmt.Fprintln(os.Stdout, "freehand: no patchable change detected — nothing to patch (clean no-op).")
			return nil
		}
		return err
	}
	defer plan.cleanup()

	relDir, err := generateAndPersistFreehandModule(projectDir, flutterRoot, toolchain, plan)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "freehand patch module generated -> %s\n", relDir)
	fmt.Fprintf(os.Stdout, "  runtime_id:              %s\n", plan.RuntimeID)
	fmt.Fprintf(os.Stdout, "  changed patchable (%d):  %s\n", len(plan.ChangedPatchable), strings.Join(plan.ChangedPatchable, ", "))
	fmt.Fprintf(os.Stdout, "  new-code closure (%d)\n", len(plan.NewCodeClosure))

	// Signed freehand delivery (freehand_identity_v1). Two triggers, sharing one custody-signed manifest:
	//   --emit-signed-manifest <dir>   LOCAL device-verifiable payload directory (no publish).
	//   --rollout N (or --publish)     HOSTED publish through the iOS engine control-plane lane.
	// Signing custody is the PROJECT key (.soroq/manifest_signing_key.seed) by default — the beginner
	// never passes --seed-base64 (kept only as a CI override).
	emitDir, _ := flagValue(head, "emit-signed-manifest")
	wantEmit := strings.TrimSpace(emitDir) != ""
	wantPublish := freehandPublishRequested(head)
	// AUTHENTICATE BEFORE BUILDING. Publishing needs operator credentials, and the patch build that
	// precedes it takes minutes. Discovering "no usable credential" only at the upload step wastes that
	// entire build and reports an auth problem as if the patch had failed. This resolves nothing
	// secret -- it only proves a usable credential exists for the target control plane.
	if wantPublish {
		if _, err := requireOperatorCredentials("", freehandPublishAPIBase(head), "publishing a hosted patch"); err != nil {
			return err
		}
	}
	if wantEmit || wantPublish {
		// Final dependency revalidation immediately before anything is signed or published: the
		// descriptor must still match the project's freshly re-resolved runtime graph and still assess as
		// code-only deliverable. buildFreehandDeviceManifest independently re-verifies the artifact
		// (including its persisted descriptor) before signing.
		if err := revalidateDependencyDescriptor(projectDir, plan, "signing/publish"); err != nil {
			return err
		}
		// DEPLOYMENT VERSION. This was hardcoded to 1, which meant every patch shipped as version 1 and
		// the controller silently ignored every patch after a device's first (see
		// freehand_manifest_version.go). It is now the next monotonic number for this
		// (app, channel, runtime) scope, resolved from the control plane before signing.
		// THE SAME control plane the publish will write to, not the raw --api flag. When --api was
		// omitted -- the ordinary case, since the default is the hosted API -- this resolved to the
		// empty string, existingScopedPatches returned "no patches" without asking anything, and every
		// patch was signed as version 1. The second patch for a runtime then aborted with
		// "concurrent publication detected", naming a race that had not happened; and had the server
		// agreed, the controller would have ignored the patch as an already-seen version.
		verAPI := freehandPublishAPIBase(head)
		// Scoped by RUNTIME, matching how the control plane allocates patch.Number. Passing the
		// release id here made the client and server disagree whenever one runtime carried more than
		// one release, and the publish aborted reporting a race that had not happened.
		existingScoped, versionErr := existingScopedPatches(verAPI, plan.AppID, plan.Channel, plan.RuntimeID)
		if versionErr != nil {
			return fmt.Errorf("resolve the next deployment version: %w", versionErr)
		}
		version := nextManifestVersion(existingScoped)
		if vs, _ := flagValue(head, "patch-version"); strings.TrimSpace(vs) != "" {
			v, err := strconv.Atoi(strings.TrimSpace(vs))
			if err != nil || v <= 0 {
				return fmt.Errorf("--patch-version must be a positive integer, got %q", vs)
			}
			version = v
		}
		// Applies to the derived value AND to an explicit override: publishing a stale version would
		// reproduce the original silent no-op.
		if err := validateManifestVersion(version, existingScoped); err != nil {
			return err
		}
		seedFlag, _ := flagValue(head, "seed-base64")
		seed, err := resolveFreehandSigningSeed(projectDir, seedFlag)
		if err != nil {
			return err
		}
		bytecodeName := fmt.Sprintf("soroq_freehand_v%d.bytecode", version)
		m, bytecodeBytes, err := buildFreehandDeviceManifest(relDir, version, bytecodeName)
		if err != nil {
			return fmt.Errorf("build signed freehand manifest: %w", err)
		}
		manifestBytes, err := json.Marshal(m)
		if err != nil {
			return err
		}
		sigHex, err := signFreehandManifest(manifestBytes, seed)
		if err != nil {
			return fmt.Errorf("sign freehand manifest: %w", err)
		}

		if wantEmit {
			if err := emitFreehandPayload(emitDir, m, manifestBytes, sigHex, bytecodeBytes); err != nil {
				return fmt.Errorf("emit freehand payload: %w", err)
			}
			fmt.Fprintf(os.Stdout, "signed freehand payload (freehand_identity_v1, v%d) -> %s\n", version, emitDir)
			fmt.Fprintf(os.Stdout, "  entrypoint_contract:  %s\n", m.EntrypointContract)
			fmt.Fprintf(os.Stdout, "  logical_artifact_id:  %s\n", m.LogicalArtifactID)
			fmt.Fprintf(os.Stdout, "  payload_sha256:       %s\n", m.PayloadSha256)
			fmt.Fprintf(os.Stdout, "  replacement_abi (%d)\n", len(m.ReplacementABI))
		}

		if wantPublish {
			// Pass the RESOLVED deployment version so the default patch id is derived from it. It used
			// to be built while Version was still the placeholder 1, and assigning params.Version
			// afterwards did not rebuild the id -- so identical content always produced the same id.
			params, err := resolveFreehandPublishParams(head, plan.AppID, plan.RuntimeID, plan.Channel, m.LogicalArtifactID, version)
			if err != nil {
				return err
			}
			patch, hostBase, err := publishFreehandEngineBundle(params, manifestBytes, sigHex, bytecodeName, bytecodeBytes)
			if err == nil {
				// CONCURRENCY GATE: the manifest was signed for a PREDICTED version. If a parallel publish
				// won the race the control plane allocated a different number, and shipping a manifest whose
				// signed version disagrees with its allocated number would recreate the duplicate-version
				// defect. Fail closed and let the operator re-sign.
				if vErr := assertAllocatedVersionMatches(version, patch); vErr != nil {
					return vErr
				}
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "hosted freehand patch published (freehand_identity_v1, v%d)\n", version)
			fmt.Fprintf(os.Stdout, "  patch_id:             %s\n", patch.ID)
			fmt.Fprintf(os.Stdout, "  rollout_percent:      %d\n", params.Rollout)
			fmt.Fprintf(os.Stdout, "  device_serve_base:    %s\n", hostBase)
			fmt.Fprintf(os.Stdout, "  payload_sha256:       %s\n", m.PayloadSha256)
			return nil
		}
	}
	if !wantPublish {
		fmt.Fprintln(os.Stderr, "note: hosted publish is opt-in (`--rollout N`); physical-device delivery is owner-gated.")
	}
	return nil
}

// generateAndPersistFreehandModule runs synthesis + dart2bytecode compilation and atomically persists the
// immutable artifact + bound metadata. Returns the artifact dir. Persists nothing on failure.
func generateAndPersistFreehandModule(projectDir, flutterRoot, toolchain string, plan *FreehandPatchPlan) (string, error) {
	// Source-integrity TOCTOU: re-verify the complete compilation input set is unchanged since the plan
	// was computed; a concurrent source/config edit means the candidate kernel + diff no longer match the
	// current source — refuse to synthesize a stale module.
	if plan.inputDigest != "" {
		now, err := captureCompilationInputDigest(projectDir)
		if err != nil {
			return "", fmt.Errorf("re-capture compilation input digest: %w", err)
		}
		if now != plan.inputDigest {
			return "", fmt.Errorf("source/config changed during patch generation (TOCTOU): %s != %s; refusing to synthesize a module from a stale diff", plan.inputDigest[:12], now[:12])
		}
	}
	// Dependency TOCTOU: re-resolve the candidate runtime graph from disk and require it to still be the
	// one the descriptor was built from. A `pub get` (or an edited pubspec) between planning and
	// generation would otherwise let a module be built against dependencies nobody assessed.
	if err := revalidateDependencyDescriptor(projectDir, plan, "module generation"); err != nil {
		return "", err
	}
	// Plan digest (bound into metadata + used as the immutable artifact dir name).
	planBytes, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	planSHA := freehandSHA256Bytes(planBytes)

	// 1. Synthesize the module source (kernel-offset extraction) into a temp dir.
	synthOut, err := os.MkdirTemp("", "soroq-freehand-synth-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(synthOut)
	dart := filepath.Join(flutterRoot, "bin", "cache", "dart-sdk", "bin", "dart")
	analyzer := filepath.Join(flutterRoot, "bin", "cache", "soroq", "soroq_kernel_analyze.dill")
	pkgConfig := filepath.Join(projectDir, ".dart_tool", "package_config.json")
	synthArgs := []string{analyzer, "--synthesize",
		"--diff-json", plan.diffJSONPath,
		"--dill", plan.candidateKernelPath,
		"--package-config", pkgConfig,
		"--out", synthOut,
	}
	if plan.capabilityMapPath != "" {
		synthArgs = append(synthArgs, "--capability-map", plan.capabilityMapPath)
	}
	// Packages NEW in this patch must be carried by value, not imported: the installed base has no such
	// library, and importing one compiles but crashes the VM at load.
	if np := carriablePackageNames(plan.DependencyDescriptor); np != "" {
		synthArgs = append(synthArgs, "--new-packages", np)
	}
	synth := exec.Command(dart, synthArgs...)
	// Surface the synthesizer's diagnostics even on SUCCESS: what it decided to carry (and what it did
	// not) is the single most useful signal when a module later fails to compile, and discarding it on
	// the happy path made that invisible.
	synthLog, synthErr := synth.CombinedOutput()
	for _, line := range strings.Split(string(synthLog), "\n") {
		if strings.HasPrefix(line, "[soroq-synth]") {
			fmt.Fprintln(os.Stderr, line)
		}
	}
	if synthErr != nil {
		return "", fmt.Errorf("freehand module synthesis failed: %w\n%s", synthErr, string(synthLog))
	}
	moduleSrc := filepath.Join(synthOut, "soroq_freehand_module.dart")
	synthManifestSrc := filepath.Join(synthOut, "soroq_freehand_module_manifest.json")
	synthManifestBytes, err := os.ReadFile(synthManifestSrc)
	if err != nil {
		return "", fmt.Errorf("read synth manifest: %w", err)
	}
	var synthManifest struct {
		ModuleSourceSHA256 string `json:"module_source_sha256"`
		ModuleGraphDigest  string `json:"module_graph_digest"`
		NeedsFlutterTarget bool   `json:"needs_flutter_target"`
		ModuleSourceTree   []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"module_source_tree"`
		CarriedLibraries []freehandCarriedLibrary `json:"carried_libraries"`
	}
	if err := json.Unmarshal(synthManifestBytes, &synthManifest); err != nil {
		return "", err
	}
	// Bind the validated dependency descriptor into the durable ABI manifest. This is done HERE rather
	// than inside the analyzer deliberately: the binding must not depend on which analyzer snapshot is
	// installed, and the manifest bytes the CLI hashes and persists are the same bytes it augments — so
	// the digest is covered by module_manifest_sha256 and therefore by the artifact id.
	synthManifestBytes, err = bindDependencyDigestIntoManifest(synthManifestBytes, plan.DependencyDescriptorDigest)
	if err != nil {
		return "", fmt.Errorf("bind the dependency descriptor into the module manifest: %w", err)
	}
	// SHA of the durable ABI manifest (over the exact file bytes we will persist verbatim) + strict
	// validation that its replacement ABI is a bijection with the changed identities. Fails closed here so
	// no artifact is ever produced with a missing/duplicate/extra/malformed ABI entry.
	manifestSHA := freehandSHA256Bytes(synthManifestBytes)
	if plan.Diff == nil {
		return "", errors.New("patch plan is missing its diff report")
	}
	moduleLibrary, err := parseAndValidateModuleManifest(synthManifestBytes, plan.Diff.Changed, plan.DependencyDescriptorDigest)
	if err != nil {
		return "", fmt.Errorf("durable replacement ABI invalid: %w", err)
	}
	// The descriptor persisted verbatim in the artifact must itself survive the production strict decoder.
	descriptorBytes, err := json.MarshalIndent(plan.DependencyDescriptor, "", "  ")
	if err != nil {
		return "", err
	}
	if _, err := depgraph.DecodeStrict(descriptorBytes); err != nil {
		return "", fmt.Errorf("refusing to persist an artifact with an invalid dependency descriptor: %w", err)
	}
	descriptorSHA := freehandSHA256Bytes(descriptorBytes)
	moduleSrcSHA, err := sha256OfPath(moduleSrc)
	if err != nil {
		return "", err
	}
	if moduleSrcSHA != synthManifest.ModuleSourceSHA256 {
		return "", fmt.Errorf("synth module source sha mismatch: %s != %s", moduleSrcSHA, synthManifest.ModuleSourceSHA256)
	}

	// THE COMPILATION NAMESPACE.
	//
	// The synthetic library URIs in the replacement ABI are derived from the module GRAPH digest, so the
	// bytecode must be compiled under that same namespace or the runtime registers the libraries under
	// one name while the signed ABI asks transitionByIdentity to find them under another. Staging under
	// the main-source sha (as this did) produced exactly that split: a manifest internally consistent
	// with itself and inconsistent with the bytecode beside it.
	//
	// Recomputed here from the ACTUAL emitted sources rather than trusted from the manifest, so a
	// manifest whose digest does not describe its own tree cannot steer compilation.
	graphDigest := strings.ToLower(strings.TrimSpace(synthManifest.ModuleGraphDigest))
	if !sha256HexRe.MatchString(graphDigest) {
		return "", fmt.Errorf("synth manifest module_graph_digest %q is not lowercase 64-hex; this "+
			"analyzer predates per-graph module namespaces — rebuild it", synthManifest.ModuleGraphDigest)
	}
	recomputed, err := recomputeModuleGraphDigest(moduleSrcSHA, synthOut, synthManifest.ModuleSourceTree)
	if err != nil {
		return "", err
	}
	if recomputed != graphDigest {
		return "", fmt.Errorf("module_graph_digest %s does not match the digest recomputed from the "+
			"emitted sources (%s); the manifest does not describe its own module tree",
			short(graphDigest), short(recomputed))
	}

	// 2. Compile the module to bytecode via the R4g dart2bytecode path (flutter platform + compile
	//    interface when Flutter-imported; base AOT app.dill as --import-dill for on-device correctness).
	bundleDir, err := iosCachedToolchainBundleDir(toolchain)
	if err != nil {
		return "", err
	}
	bytecodePath := filepath.Join(synthOut, "soroq_freehand_module.bytecode")
	if err := compileFreehandModuleBytecode(projectDir, flutterRoot, bundleDir, plan.relDir, moduleSrc, graphDigest, moduleSrcSHA, bytecodePath, synthManifest.NeedsFlutterTarget); err != nil {
		return "", fmt.Errorf("compile freehand module: %w", err)
	}
	bytecodeSHA, err := sha256OfPath(bytecodePath)
	if err != nil {
		return "", err
	}

	// 2b. Post-synthesis/compile input recheck: the source must not have drifted while we synthesized +
	//     compiled (TOCTOU). Refuse before publication.
	if plan.inputDigest != "" {
		nowDigest, err := captureCompilationInputDigest(projectDir)
		if err != nil {
			return "", fmt.Errorf("post-compile source recheck: %w", err)
		}
		if nowDigest != plan.inputDigest {
			return "", fmt.Errorf("source/config changed during synthesis/compilation (TOCTOU): refusing to publish a stale artifact")
		}
	}
	// Second dependency revalidation: immediately before the artifact becomes publishable.
	if err := revalidateDependencyDescriptor(projectDir, plan, "publication"); err != nil {
		return "", err
	}

	// 3. Toolchain binding + artifact identity. The artifact id binds the plan AND the exact tools, so a
	//    toolchain change (same source/plan) yields a DISTINCT immutable artifact.
	binding, err := computeToolchainBinding(flutterRoot, bundleDir, toolchain, synthManifest.NeedsFlutterTarget)
	if err != nil {
		return "", err
	}
	bindingDigest, err := binding.digest()
	if err != nil {
		return "", err
	}
	// The artifact id binds the plan, the exact tools, AND the durable replacement ABI (module manifest):
	// any change to source/plan, toolchain, or the ABI yields a DISTINCT immutable artifact.
	artifactID := computeFreehandArtifactID(planSHA, bindingDigest, manifestSHA, plan.DependencyDescriptorDigest)
	meta := FreehandPatchArtifactMeta{
		Schema:                 freehandPatchArtifactSchema,
		RuntimeID:              plan.RuntimeID,
		IdentitySchema:         plan.IdentitySchema,
		AppID:                  plan.AppID,
		Version:                plan.Version,
		Channel:                plan.Channel,
		BaseAppDillSHA256:      plan.BaseAppDillSHA256,
		BaseIdentity:           plan.BaseIdentity,
		BaseSourceKernelSHA256: plan.BaseSourceKernelSHA256,
		CandSourceKernelSHA256: plan.CandSourceKernelSHA256,
		SourceRecipeDigest:     plan.SourceRecipeDigest,
		PatchPlanSHA256:        planSHA,
		ModuleSourceSHA256:     moduleSrcSHA,
		// Populated from the STRICTLY DECODED + independently recomputed manifest. These were declared
		// and left zero-valued, so the artifact recorded no namespace at all and verification had
		// nothing to cross-check the ABI's synthetic URIs against.
		ModuleGraphDigest:          graphDigest,
		CarriedLibraries:           append([]freehandCarriedLibrary(nil), synthManifest.CarriedLibraries...),
		ModuleBytecodeSHA256:       bytecodeSHA,
		ModuleManifestSHA256:       manifestSHA,
		ModuleLibrary:              moduleLibrary,
		NeedsFlutterTarget:         synthManifest.NeedsFlutterTarget,
		ToolchainBinding:           &binding,
		ToolchainBindingDigest:     bindingDigest,
		ArtifactID:                 artifactID,
		CompilationInputDigest:     plan.inputDigest,
		ChangedIdentities:          append([]string(nil), plan.ChangedPatchable...),
		ClosureIdentities:          append([]string(nil), plan.NewCodeClosure...),
		DependencyDescriptorDigest: plan.DependencyDescriptorDigest,
		DependencyDescriptorSHA256: descriptorSHA,
		BaseDependencyGraphDigest:  plan.DependencyDescriptor.BaseGraphDigest,
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", err
	}

	// 4. Atomic temp -> verify -> rename into the immutable artifact dir .soroq/patches/<rt>/<artifactID>/.
	patchesRoot := filepath.Join(projectDir, ".soroq", "patches", plan.RuntimeID)
	finalDir := filepath.Join(patchesRoot, artifactID)
	if _, err := os.Stat(filepath.Join(finalDir, "patch_artifact.json")); err == nil {
		// Existing artifact: STRICTLY re-verify before reuse (rehash all files; refuse tampered/incomplete).
		if verr := verifyExistingPatchArtifact(finalDir, artifactID); verr != nil {
			return "", fmt.Errorf("existing freehand patch artifact at %s is invalid: %w", finalDir, verr)
		}
		return finalDir, nil
	}
	if err := os.MkdirAll(patchesRoot, 0o700); err != nil {
		return "", err
	}
	tmpDir, err := os.MkdirTemp(patchesRoot, ".tmp-"+artifactID+"-")
	if err != nil {
		return "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(tmpDir)
		}
	}()
	if err := copyFileVerifiedSync(moduleSrc, filepath.Join(tmpDir, "soroq_freehand_module.dart"), moduleSrcSHA, 0o600); err != nil {
		return "", err
	}
	if err := copyFileVerifiedSync(bytecodePath, filepath.Join(tmpDir, "soroq_freehand_module.bytecode"), bytecodeSHA, 0o600); err != nil {
		return "", err
	}
	// Persist the durable replacement ABI. These are the AUGMENTED bytes (the dependency descriptor digest
	// bound in above) -- the same bytes manifestSHA was computed over and that the artifact id binds, NOT
	// the analyzer's original file, whose hash would no longer match.
	if err := writeFileSync(filepath.Join(tmpDir, "soroq_freehand_module_manifest.json"), synthManifestBytes, 0o600); err != nil {
		return "", err
	}
	if err := writeFileSync(filepath.Join(tmpDir, freehandDependencyDescriptorFile), descriptorBytes, 0o600); err != nil {
		return "", err
	}
	if err := writeFileSync(filepath.Join(tmpDir, "patch_plan.json"), planBytes, 0o600); err != nil {
		return "", err
	}
	if err := writeFileSync(filepath.Join(tmpDir, "patch_artifact.json"), metaBytes, 0o600); err != nil {
		return "", err
	}
	syncDir(tmpDir)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		if _, statErr := os.Stat(filepath.Join(finalDir, "patch_artifact.json")); statErr == nil {
			// A concurrent writer won: STRICTLY verify its artifact before accepting it.
			if verr := verifyExistingPatchArtifact(finalDir, artifactID); verr != nil {
				return "", fmt.Errorf("concurrent-writer left an invalid freehand patch artifact: %w", verr)
			}
			return finalDir, nil
		}
		return "", fmt.Errorf("atomic publish freehand patch artifact: %w", err)
	}
	cleanup = false
	syncDir(patchesRoot)
	return finalDir, nil
}

// compileFreehandModuleBytecode compiles the synthesized module to bytecode via the toolchain's
// dart2bytecode. Uses the flutter platform + compile interface when the module imports Flutter; the base
// AOT app.dill as --import-dill (device correctness).
//
// The module is compiled through a MULTI-ROOT FILESYSTEM SCHEME so its VM library URI is DETERMINISTIC and
// machine-independent — NOT the random absolute temp path that `--prefix-library-uris` alone would embed
// (which was also a source of bytecode non-determinism). The module file is staged at
// <root>/<moduleGraphDigest>/soroq_freehand_module.dart and passed as the URI
// `soroq-freehand:///<moduleGraphDigest>/soroq_freehand_module.dart`; with `--prefix-library-uris import/prefix`
// the resulting VM library URI is exactly:
//
//	soroq-freehand:///import/prefix/<moduleGraphDigest>/soroq_freehand_module.dart
//
// which is what the analyzer records as `module_library` and what the runtime looks up after load. The
// <moduleGraphDigest> segment makes the URI per-module-GRAPH-unique so two loaded modules never collide on
// the VM -- the main source sha alone was NOT sufficient, because an upgrade can change only a carried
// library while soroq_freehand_module.dart stays byte-identical
// loader's "library already loaded" check (bytecode_reader.cc), while keeping the URI + bytecode reproducible.
func compileFreehandModuleBytecode(projectDir, flutterRoot, bundleDir, baseRelDir, moduleSrc, moduleGraphDigest, mainSourceSHA, out string, needsFlutter bool) error {
	dartaot := filepath.Join(bundleDir, "dartaotruntime")
	dart2bc := filepath.Join(bundleDir, "dart2bytecode")
	// Import-dill = the NON-tree-shaken base SOURCE kernel: it carries the COMPLETE API of every base
	// library (flutter, provider, ...), so the module's external references resolve cleanly instead of
	// failing against the tree-shaken AOT app.dill. Symbol identities match the AOT app.dill (same
	// source), so the compiled references still resolve at device load.
	baseAppDill := filepath.Join(baseRelDir, "source_app.dill")
	pkgConfig := filepath.Join(projectDir, ".dart_tool", "package_config.json")
	for _, p := range []string{dartaot, dart2bc, baseAppDill} {
		if !fileExists(p) {
			return fmt.Errorf("module compile input missing: %s", p)
		}
	}
	// Stage the module under a deterministic, sha-named root so the scheme URI is content-addressed.
	fsRoot, err := os.MkdirTemp("", "soroq-freehand-fsroot-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(fsRoot)
	stagedDir := filepath.Join(fsRoot, moduleGraphDigest)
	if err := os.MkdirAll(stagedDir, 0o700); err != nil {
		return err
	}
	if err := copyFileVerifiedSync(moduleSrc, filepath.Join(stagedDir, "soroq_freehand_module.dart"), mainSourceSHA, 0o600); err != nil {
		return err
	}
	// Stage the MODULE-LOCAL SOURCE TREE alongside it. The module imports each carried dependency library
	// by relative path, so those files must exist under the same filesystem root or every carried symbol
	// is "Method not found". Paths are re-validated here (no absolute, no traversal, no symlink) because
	// this is what actually lands on disk for the compiler.
	moduleDir := filepath.Dir(moduleSrc)
	if err := filepath.WalkDir(moduleDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(moduleDir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == "soroq_freehand_module.dart" || !strings.HasSuffix(rel, ".dart") {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("module-local source %q is not a regular file", rel)
		}
		if strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
			return fmt.Errorf("unsafe module-local source path %q", rel)
		}
		dst := filepath.Join(stagedDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return copyFileVerifiedSync(p, dst, "", 0o600)
	}); err != nil {
		return fmt.Errorf("stage module-local source tree: %w", err)
	}
	moduleURI := "soroq-freehand:///" + moduleGraphDigest + "/soroq_freehand_module.dart"

	args := []string{dart2bc, "--target"}
	if needsFlutter {
		platform, perr := flutterProfilePlatformDillFromToolchain(bundleDir)
		if perr != nil {
			return perr
		}
		compileInterface := filepath.Join(bundleDir, "flutter_compile_interface")
		if !fileExists(compileInterface) {
			return fmt.Errorf("flutter compile interface missing: %s", compileInterface)
		}
		args = append(args, "flutter", "--platform", platform, "--compile-interface", compileInterface)
	} else {
		args = append(args, "vm", "--platform", filepath.Join(bundleDir, "vm_platform"))
	}
	args = append(args,
		"--packages", pkgConfig,
		"--import-dill", baseAppDill,
		"--filesystem-scheme", "soroq-freehand",
		"--filesystem-root", fsRoot,
		"--prefix-library-uris", "import/prefix",
		"-o", out, moduleURI,
	)
	cmd := exec.Command(dartaot, args...)
	cmd.Dir = projectDir
	if o, err := cmd.CombinedOutput(); err != nil {
		// A dynamic-module CONTRACT violation is a dependency-eligibility answer, not a compiler crash:
		// translate it into a refusal that names the package, library and member instead of dumping a
		// front-end diagnostic that points into a pub-cache file.
		if vs := parseDynamicModuleViolations(string(o)); len(vs) > 0 {
			return fmt.Errorf("%s", explainDynamicModuleViolations(vs))
		}
		return fmt.Errorf("dart2bytecode failed: %w\n%s", err, string(o))
	}
	if !fileExists(out) {
		return fmt.Errorf("dart2bytecode produced no output at %s", out)
	}
	return nil
}

// flutterProfilePlatformDillFromToolchain finds the flutter platform_strong.dill within the iOS toolchain
// bundle (the fix for the Flutter-import compile: --target flutter needs the flutter platform, not vm).
func flutterProfilePlatformDillFromToolchain(bundleDir string) (string, error) {
	for _, rel := range []string{
		filepath.Join("build_lane", "ios_profile", "flutter_patched_sdk", "platform_strong.dill"),
		filepath.Join("build_lane", "host_profile_unopt", "flutter_patched_sdk", "platform_strong.dill"),
	} {
		p := filepath.Join(bundleDir, rel)
		if fileExists(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no flutter platform_strong.dill under %s/build_lane/*/flutter_patched_sdk/", bundleDir)
}

func joinLines(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "\n  - "
		}
		out += x
	}
	return out
}

// freehandCarriedLibrary is one carried package library: where it came from, where it lives in the
// module tree, and the synthetic VM URI it will be registered under.
type freehandCarriedLibrary struct {
	PackageURI    string `json:"package_uri"`
	ModulePath    string `json:"module_path"`
	ModuleLibrary string `json:"module_library"`
	SHA256        string `json:"sha256"`
}

// resolveEntryLibrary accepts a per-entry module_library and fails closed unless it is either the main
// synthetic module or exactly one strictly-recorded carried library.
//
// Every accepted URI must also live in THIS artifact's namespace. Without that check an entry could
// name a correctly-shaped URI from a different module graph, and the runtime would resolve it against
// whatever happened to be loaded under that name.
func (m *freehandModuleManifest) resolveEntryLibrary(uri, who string) error {
	if uri == "" {
		return fmt.Errorf("entry %s has an empty module_library", who)
	}
	// The MAIN module library is self-identifying: it is the manifest's own declared library, so it
	// needs no namespace cross-check.
	if uri == m.ModuleLibrary {
		return nil
	}
	// Anything else must be a recorded CARRIED library, and those only exist in artifacts that declare
	// a module-graph namespace. An older artifact cannot express them, so it is refused rather than
	// guessed compatible.
	if m.ModuleGraphDigest == "" {
		return fmt.Errorf("entry %s names module_library %q but this artifact declares no "+
			"module_graph_digest; it predates per-library module URIs and cannot be verified — rebuild "+
			"the patch with the current toolchain", who, uri)
	}
	if !strings.Contains(uri, "/"+m.ModuleGraphDigest+"/") {
		return fmt.Errorf("entry %s module_library %q is outside this artifact's module-graph namespace %s",
			who, uri, short(m.ModuleGraphDigest))
	}
	matches := 0
	for _, cl := range m.CarriedLibraries {
		if cl.ModuleLibrary == uri {
			matches++
		}
	}
	switch matches {
	case 1:
		return nil
	case 0:
		return fmt.Errorf("entry %s names module_library %q, which is neither the main module nor any "+
			"recorded carried library; an unrecorded library URI cannot be verified", who, uri)
	default:
		return fmt.Errorf("entry %s names module_library %q, which maps to %d carried libraries; the "+
			"mapping must be one-to-one", who, uri, matches)
	}
}

// recomputeModuleGraphDigest independently re-derives the module-graph namespace from the sources the
// analyzer actually emitted, so a manifest whose digest does not describe its own tree cannot steer
// compilation into the wrong namespace.
//
// It mirrors the analyzer's construction exactly: a schema tag, the main source sha, then every carried
// file as path=sha in the manifest's recorded order (which the analyzer sorts). Each entry's sha is
// re-hashed from the file on disk rather than trusted, so a manifest that lists a file with a
// convenient hash is rejected.
func recomputeModuleGraphDigest(mainSHA, synthDir string, tree []struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}) (string, error) {
	lines := []string{"soroq.freehand.module_graph.v1", "main=" + mainSHA}
	seen := map[string]bool{}
	var order []string
	for _, e := range tree {
		rel := strings.TrimSpace(e.Path)
		if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
			return "", fmt.Errorf("module source tree entry %q is not a safe relative path", e.Path)
		}
		for _, seg := range strings.Split(rel, "/") {
			if seg == ".." {
				return "", fmt.Errorf("module source tree entry %q escapes the module tree", e.Path)
			}
		}
		onDisk, err := sha256OfPath(filepath.Join(synthDir, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("hash emitted module source %q: %w", rel, err)
		}
		if !strings.EqualFold(onDisk, strings.TrimSpace(e.SHA256)) {
			return "", fmt.Errorf("module source tree entry %q records sha %s but the emitted file hashes to %s",
				rel, short(e.SHA256), short(onDisk))
		}
		if seen[rel] {
			return "", fmt.Errorf("module source tree lists %q more than once; the tree must be a "+
				"canonical set, or two entries could describe the same file differently", rel)
		}
		seen[rel] = true
		order = append(order, rel)
		lines = append(lines, "file="+rel+"="+onDisk)
	}
	// The analyzer emits a sorted tree; accepting an arbitrary order would let the same set of files
	// produce different digests, so ordering is required rather than assumed.
	if !sort.StringsAreSorted(order) {
		return "", fmt.Errorf("module source tree is not sorted by path; the digest would not be canonical")
	}
	return freehandSHA256Bytes([]byte(strings.Join(lines, "\n"))), nil
}
