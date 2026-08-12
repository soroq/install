package main

// Soroq freehand — CLI wiring for `soroq release ios --engine --build` in FREEHAND mode.
//
// Freehand mode is selected when soroq.yaml enables the iOS engine lane but lists NO
// ios_engine.patchable entries. It requires no patchable function list and generates no customer
// lib/ files: the patched frontend's build-DAG target (patch 0011) auto-discovers the patchable set
// from the compiled app.dill and passes the manifest to the unchanged gen_snapshot. This CLI only:
//   - installs the bundled analyzer at the fixed {FLUTTER_ROOT}/bin/cache/soroq/soroq_kernel_analyze.dill
//   - writes .soroq/freehand_config.json atomically (and removes it after the build — fail-safe, so a
//     later ordinary `flutter build ios` is never accidentally in freehand mode)
//   - after a fully successful build, snapshots the immutable baseline via persistFreehandBaseline
//     from the EXACT kernel the freehand analysis + gen_snapshot consumed.
//
// Indexed/manual mode (soroq.yaml with ios_engine.patchable entries) is unchanged.

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
	"strings"
)

// collectFrontendProvenance reads the ACTUAL installed frontend/toolchain revisions (no hardcoding):
// frontend = patched Flutter fork HEAD; framework/dart/engine = the toolchain's engine.json.
func collectFrontendProvenance(flutterRoot, toolchainVersion string) (frontend, framework, dart, engine string) {
	if out, err := exec.Command("git", "-C", flutterRoot, "rev-parse", "HEAD").Output(); err == nil {
		frontend = strings.TrimSpace(string(out))
	}
	if bundleDir, err := iosCachedToolchainBundleDir(toolchainVersion); err == nil {
		if b, err := os.ReadFile(filepath.Join(bundleDir, "engine.json")); err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				framework, _ = m["flutter_commit"].(string)
				dart, _ = m["dart_revision"].(string)
				engine, _ = m["soroq_engine_revision"].(string)
			}
		}
	}
	return
}

// freehandReleaseDelegate is the release-registration delegate and freehandPersistFn is the baseline
// persister — both are seams so tests can prove the failed-build path invokes NEITHER, and the happy
// path invokes the delegate exactly once. Production uses the real functions verbatim.
var (
	freehandReleaseDelegate = runEngineLaneDelegate
	freehandPersistFn       = persistFreehandBaselineFromBuild
)

// frontendPatchsetSHA derives the frontend patchset identity. Git HEAD alone is insufficient: multiple
// patchsets share the same base commit (f74781f6).
//
//   - Installed signed frontend (product path): the AUTHORITATIVE identity is the pack-time
//     patchset_sha256 recorded in the signed manifest.json (verified offline against manifest.sig +
//     the pinned key). The installed archive STRIPS most Soroq frontend source — the freehand target
//     lives only in the compiled tool snapshot — so re-hashing source would miss the patchset entirely.
//   - Development fork (no signed manifest): fall back to a deterministic source hash. A dev fork ships
//     FULL source, so hashing every Soroq-marked flutter_tools file captures the patchset completely.
func frontendPatchsetSHA(flutterRoot string) (string, error) {
	if sha, ok, err := installedFrontendPatchsetSHA(flutterRoot); err != nil {
		return "", err
	} else if ok {
		return sha, nil
	}
	return devForkFrontendSourceSHA(flutterRoot)
}

// installedFrontendPatchsetSHA returns the signed manifest patchset_sha256 when flutterRoot is an
// installed frontend (a signed manifest sits beside its subdir), else ok=false (a dev fork).
func installedFrontendPatchsetSHA(flutterRoot string) (string, bool, error) {
	versionDir := filepath.Dir(filepath.Clean(flutterRoot))
	if _, err := os.Stat(filepath.Join(versionDir, "manifest.json")); err != nil {
		return "", false, nil // not an installed frontend (no manifest) -> dev-fork fallback
	}
	m, ok, err := reverifyInstalledFrontend("", versionDir) // verifies manifest.sig vs pinned key, offline
	if err != nil {
		return "", false, fmt.Errorf("verify installed frontend manifest: %w", err)
	}
	if !ok {
		return "", false, nil
	}
	// The subdir the manifest names must be exactly the flutterRoot we are pinning.
	if filepath.Clean(filepath.Join(versionDir, m.subdir())) != filepath.Clean(flutterRoot) {
		return "", false, nil
	}
	sha := strings.TrimSpace(m.PatchsetSHA256)
	if sha == "" {
		return "", false, errors.New("installed frontend manifest has empty patchset_sha256")
	}
	return sha, true, nil
}

// devForkFrontendSourceSHA hashes every Soroq-marked flutter_tools source file — build_system/targets/
// *.dart carrying the "soroq" marker (empirically: assets.dart, ios.dart, soroq_freehand.dart) plus
// soroq_metadata.dart — sorted as (relpath \0 len \0 bytes). Complete for a full-source dev fork; a
// missing expected file fails closed.
func devForkFrontendSourceSHA(flutterRoot string) (string, error) {
	targetsDir := filepath.Join(flutterRoot, "packages", "flutter_tools", "lib", "src", "build_system", "targets")
	entries, err := os.ReadDir(targetsDir)
	if err != nil {
		return "", fmt.Errorf("read frontend targets dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".dart") {
			continue
		}
		p := filepath.Join(targetsDir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", p, err)
		}
		if bytes.Contains(bytes.ToLower(b), []byte("soroq")) {
			files = append(files, p)
		}
	}
	meta := filepath.Join(flutterRoot, "packages", "flutter_tools", "lib", "src", "soroq_metadata.dart")
	if !fileExists(meta) {
		return "", fmt.Errorf("expected frontend patch file missing: %s", meta)
	}
	files = append(files, meta)
	sort.Strings(files)
	if len(files) == 0 {
		return "", errors.New("no soroq frontend patch files found")
	}
	var buf bytes.Buffer
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(flutterRoot, p)
		if err != nil {
			rel = p
		}
		fmt.Fprintf(&buf, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
		buf.Write(b)
	}
	return freehandSHA256Bytes(buf.Bytes()), nil
}

// isFreehandIOSBuild reports whether the project is a freehand base (engine enabled, no patchable list).
func isFreehandIOSBuild(projectDir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		return false, fmt.Errorf("read soroq.yaml: %w", err)
	}
	enabled, items, err := parseIOSEnginePatchable(b)
	if err != nil {
		return false, err
	}
	return enabled && len(items) == 0, nil
}

const freehandAnalyzerRelPath = "bin/cache/soroq/soroq_kernel_analyze.dill"

// pubspecVersion extracts the top-level `version:` from pubspec.yaml (best-effort, for baseline meta).
func pubspecVersion(projectDir string) string {
	b, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(line, "version:") {
			return strings.TrimSpace(strings.TrimPrefix(t, "version:"))
		}
	}
	return ""
}

// resolveBundledAnalyzerSource finds the analyzer snapshot to install: SOROQ_FREEHAND_ANALYZER, else
// soroq_kernel_analyze.dill next to the running soroq executable.
func resolveBundledAnalyzerSource() (string, error) {
	if p := strings.TrimSpace(os.Getenv("SOROQ_FREEHAND_ANALYZER")); p != "" {
		if fileExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("SOROQ_FREEHAND_ANALYZER=%s does not exist", p)
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "soroq_kernel_analyze.dill")
		if fileExists(cand) {
			return cand, nil
		}
	}
	// The INSTALLED FRONTEND bundles the analyzer it was built against, at a fixed path. Resolving it
	// here is what makes the zero-touch workflow actually zero-touch: a fresh developer running
	// `soroq init` + `soroq release --platforms=ios` has no reason to know that an analyzer snapshot
	// exists, let alone to set an environment variable naming one. Preferring the frontend's own copy
	// is also the CORRECT binding -- it is the analyzer whose sha the frontend's manifest advertises,
	// so the analysis receipt and the frontend agree by construction.
	if bin, err := resolveInstalledFrontendFlutterBin(); err == nil && strings.TrimSpace(bin) != "" {
		// <frontend>/bin/flutter -> <frontend>/bin/cache/soroq/soroq_kernel_analyze.dill
		cand := filepath.Join(filepath.Dir(bin), "cache", "soroq", "soroq_kernel_analyze.dill")
		if fileExists(cand) {
			return cand, nil
		}
	}
	return "", errors.New("freehand analyzer snapshot not found: the active Soroq frontend does not bundle " +
		"one (bin/cache/soroq/soroq_kernel_analyze.dill), no soroq_kernel_analyze.dill sits next to the " +
		"soroq binary, and SOROQ_FREEHAND_ANALYZER is unset")
}

// installFreehandAnalyzer copies the bundled analyzer to the fixed frontend path and returns its
// installed path + real sha (idempotent: skips copy when bytes already match).
func installFreehandAnalyzer(flutterRoot string) (string, string, error) {
	src, err := resolveBundledAnalyzerSource()
	if err != nil {
		return "", "", err
	}
	srcSha, err := sha256OfPath(src)
	if err != nil {
		return "", "", err
	}
	dst := filepath.Join(flutterRoot, filepath.FromSlash(freehandAnalyzerRelPath))
	if fileExists(dst) {
		if dstSha, err := sha256OfPath(dst); err == nil && dstSha == srcSha {
			return dst, dstSha, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", "", err
	}
	tmp := dst + ".tmp"
	if err := copyFileVerifiedSync(src, tmp, srcSha, 0o644); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	dstSha, err := sha256OfPath(dst)
	if err != nil || dstSha != srcSha {
		return "", "", fmt.Errorf("analyzer install verification failed: %s != %s", dstSha, srcSha)
	}
	return dst, dstSha, nil
}

const freehandConfigRelPath = ".soroq/freehand_config.json"

// writeFreehandConfigAtomic writes .soroq/freehand_config.json (temp + rename).
func writeFreehandConfigAtomic(projectDir string) error {
	cfg := map[string]string{
		"schema":         "soroq.freehand.config.v1",
		"mode":           "freehand",
		"identitySchema": "soroq.freehand.identity.v1",
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(projectDir, ".soroq")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".freehand_config.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(projectDir, filepath.FromSlash(freehandConfigRelPath)))
}

func removeFreehandConfig(projectDir string) {
	os.Remove(filepath.Join(projectDir, filepath.FromSlash(freehandConfigRelPath)))
}

// Freehand analysis constants — MUST match the frontend (soroq_freehand.dart) exactly.
const (
	freehandIdentitySchema  = "soroq.freehand.identity.v1"
	freehandReceiptSchema   = "soroq.freehand.receipt.v1"
	freehandAnalysisAddrTag = "soroq.freehand.analysis.v1"
	freehandConfigMode      = "freehand"
)

// contentAddrRe matches a 64-char lowercase-hex SHA-256 content address (a safe single path segment).
var contentAddrRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// freehandConfigDigest independently recomputes the config digest the frontend binds into the content
// address: sha256("<mode>|<identitySchema>").
func freehandConfigDigest() string {
	return freehandSHA256Bytes([]byte(freehandConfigMode + "|" + freehandIdentitySchema))
}

// freehandContentAddr recomputes the frontend's content address (soroqAnalysisContentAddr) from inputs.
func freehandContentAddr(appDillSha, analyzerSha, pkgConfigSha, identitySchema, configDigest string) string {
	return freehandSHA256Bytes([]byte(strings.Join([]string{
		freehandAnalysisAddrTag, appDillSha, analyzerSha, pkgConfigSha, identitySchema, configDigest,
	}, "|")))
}

// freehandAnalysisIndex mirrors analysis_index.json (strict; no unknown fields).
type freehandAnalysisIndex struct {
	Schema              string `json:"schema"`
	Mode                string `json:"mode"`
	AnalysisID          string `json:"analysis_id"`
	AppDillSHA256       string `json:"app_dill_sha256"`
	AnalyzerSnapshotSHA string `json:"analyzer_snapshot_sha256"`
	PackageConfigSHA256 string `json:"package_config_sha256"`
	IdentitySchema      string `json:"identity_schema"`
	ConfigDigest        string `json:"config_digest"`
}

// freehandAnalysisReceipt mirrors soroq_analysis_receipt.json (strict; no unknown fields).
type freehandAnalysisReceipt struct {
	Schema              string `json:"schema"`
	Mode                string `json:"mode"`
	IdentitySchema      string `json:"identity_schema"`
	AnalysisID          string `json:"analysis_id"`
	AppDillSHA256       string `json:"app_dill_sha256"`
	ManifestSHA256      string `json:"manifest_sha256"`
	SymbolGraphSHA256   string `json:"symbol_graph_sha256"`
	AnalyzerSnapshotSHA string `json:"analyzer_snapshot_sha256"`
	PackageConfigSHA256 string `json:"package_config_sha256"`
	ConfigDigest        string `json:"config_digest"`
}

// strictDecodeJSON decodes exactly one JSON value into v, rejecting unknown fields and trailing bytes.
func strictDecodeJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing JSON content")
	}
	return nil
}

// verifyFreehandStagingStrict strictly parses + revalidates the freehand staging produced by the
// build-DAG target immediately before baseline persistence. It fails closed on any unknown/missing/
// wrongly-typed field, an unsafe analysis_id, or a mismatched hash/digest/schema/content-address, and
// returns the verified analysis dir + manifest/graph paths whose bytes were re-hashed and matched
// (the exact bytes gen_snapshot consumed via --soroq_manifest).
func verifyFreehandStagingStrict(projectDir, appDill, analyzerSha string) (analysisDir, manifestPath, graphPath string, err error) {
	root := filepath.Join(filepath.Dir(appDill), "soroq_freehand")

	// Live inputs — recomputed, never trusted from the receipt.
	appDillSha, err := sha256OfPath(appDill)
	if err != nil {
		return "", "", "", fmt.Errorf("hash app.dill: %w", err)
	}
	pkgConfigSha, err := sha256OfPath(filepath.Join(projectDir, ".dart_tool", "package_config.json"))
	if err != nil {
		return "", "", "", fmt.Errorf("hash package_config.json: %w", err)
	}
	configDigest := freehandConfigDigest()
	wantAddr := freehandContentAddr(appDillSha, analyzerSha, pkgConfigSha, freehandIdentitySchema, configDigest)
	// wantAddr is a SHA-256 (always a safe 64-hex segment); assert defensively before path construction.
	if !contentAddrRe.MatchString(wantAddr) {
		return "", "", "", fmt.Errorf("internal: recomputed content address %q is not 64-hex", wantAddr)
	}

	// Locate the analysis dir by the content address recomputed from LIVE inputs — exactly as the
	// frontend's AOT wiring (soroqFreehandVerifyAndWire) does. analysis_index.json is a MUTABLE cache
	// fingerprint the frontend documents as NOT authoritative: the build system caches the analysis
	// target, so after an app.dill change the index can still point at a STALE prior analysis while
	// gen_snapshot actually consumed the manifest under root/<recomputed-addr>. Trusting the index here
	// would either persist the wrong manifest or fail on a stale pointer; recomputing is both correct
	// and the stronger content-addressed binding.
	analysisDir = filepath.Join(root, wantAddr)

	// Strict receipt at the recomputed dir.
	recBytes, err := os.ReadFile(filepath.Join(analysisDir, "soroq_analysis_receipt.json"))
	if err != nil {
		// THE CACHE CONTRACT. Flutter caches SoroqFreehandAnalysis on its own inputs, but baseline
		// persistence needs the immutable outputs at the address recomputed from LIVE inputs. When the
		// target is considered up to date and those outputs are absent — a deleted or tampered receipt,
		// or any input the two keyings disagree about — the build cannot proceed, and the raw
		// "no such file" it used to print named neither the cause nor a way out (`flutter clean` was the
		// only thing that worked, which throws away every other cached target).
		//
		// So invalidate the analysis target's stamp: the very next build re-runs the analysis and
		// regenerates the outputs, with no clean. This fails closed — it never persists a baseline from
		// outputs it could not verify — while making the recovery a re-run.
		invalidated := invalidateFreehandAnalysisStamp(appDill)
		return "", "", "", fmt.Errorf(
			"no verified freehand analysis for content address %s.\n"+
				"  The analysis target was cached but its immutable outputs are missing at that address,\n"+
				"  so nothing here can be trusted to persist as a baseline (read receipt: %v).\n"+
				"  %s\n"+
				"  Re-run the same command — do NOT `flutter clean`; the rest of the build cache is intact.",
			wantAddr, err, invalidated)
	}
	var rec freehandAnalysisReceipt
	if err := strictDecodeJSON(recBytes, &rec); err != nil {
		return "", "", "", fmt.Errorf("strict parse analysis receipt: %w", err)
	}
	for name, v := range map[string]string{
		"schema": rec.Schema, "mode": rec.Mode, "identity_schema": rec.IdentitySchema,
		"analysis_id": rec.AnalysisID, "app_dill_sha256": rec.AppDillSHA256,
		"manifest_sha256": rec.ManifestSHA256, "symbol_graph_sha256": rec.SymbolGraphSHA256,
		"analyzer_snapshot_sha256": rec.AnalyzerSnapshotSHA, "package_config_sha256": rec.PackageConfigSHA256,
		"config_digest": rec.ConfigDigest,
	} {
		if strings.TrimSpace(v) == "" {
			return "", "", "", fmt.Errorf("receipt field %q missing/empty", name)
		}
	}
	if rec.Schema != freehandReceiptSchema || rec.Mode != freehandConfigMode {
		return "", "", "", fmt.Errorf("receipt schema/mode invalid: %q/%q", rec.Schema, rec.Mode)
	}
	if rec.IdentitySchema != freehandIdentitySchema {
		return "", "", "", fmt.Errorf("receipt identity_schema %q != %q", rec.IdentitySchema, freehandIdentitySchema)
	}

	// Bind every hash/digest/id to LIVE inputs (recomputed).
	if rec.AppDillSHA256 != appDillSha {
		return "", "", "", fmt.Errorf("app.dill sha mismatch: receipt %s != live %s", rec.AppDillSHA256, appDillSha)
	}
	if rec.AnalyzerSnapshotSHA != analyzerSha {
		return "", "", "", fmt.Errorf("analyzer sha mismatch: receipt %s != installed %s", rec.AnalyzerSnapshotSHA, analyzerSha)
	}
	if rec.PackageConfigSHA256 != pkgConfigSha {
		return "", "", "", fmt.Errorf("package_config sha mismatch: receipt %s != live %s", rec.PackageConfigSHA256, pkgConfigSha)
	}
	if rec.ConfigDigest != configDigest {
		return "", "", "", fmt.Errorf("config digest mismatch: receipt %s != recomputed %s", rec.ConfigDigest, configDigest)
	}
	if !contentAddrRe.MatchString(rec.AnalysisID) {
		return "", "", "", fmt.Errorf("receipt analysis_id %q is not a 64-hex content address", rec.AnalysisID)
	}
	if rec.AnalysisID != wantAddr {
		return "", "", "", fmt.Errorf("receipt analysis_id %s != content address %s", rec.AnalysisID, wantAddr)
	}

	// The index is a non-authoritative cache hint. Strict-parse it for hygiene; a stale index (pointing
	// at a prior app.dill) is EXPECTED and tolerated. Only when the index is FRESH — its analysis_id
	// equals the recomputed address — do we cross-check its bound fields against the receipt (a fresh
	// index that disagrees signals tampering).
	idxBytes, err := os.ReadFile(filepath.Join(root, "analysis_index.json"))
	if err != nil {
		return "", "", "", fmt.Errorf("read analysis_index.json: %w", err)
	}
	var idx freehandAnalysisIndex
	if err := strictDecodeJSON(idxBytes, &idx); err != nil {
		return "", "", "", fmt.Errorf("strict parse analysis_index.json: %w", err)
	}
	// NOTE: no idx.Mode hard-fail here. The index is non-authoritative and may be a stale leftover from a
	// prior disabled/stock build; the receipt at the recomputed dir already proves mode==freehand. Only a
	// FRESH index (analysis_id == recomputed address) is cross-checked below.
	if idx.AnalysisID == wantAddr {
		if idx.AppDillSHA256 != rec.AppDillSHA256 || idx.AnalyzerSnapshotSHA != rec.AnalyzerSnapshotSHA ||
			idx.PackageConfigSHA256 != rec.PackageConfigSHA256 || idx.IdentitySchema != rec.IdentitySchema ||
			idx.ConfigDigest != rec.ConfigDigest {
			return "", "", "", errors.New("fresh analysis_index and receipt disagree on a bound field")
		}
	}

	// Rehash the persisted manifest + graph and compare to the receipt.
	manifestPath = filepath.Join(analysisDir, "soroq_app_manifest.txt")
	graphPath = filepath.Join(analysisDir, "symbol_graph.json")
	manifestSha, err := sha256OfPath(manifestPath)
	if err != nil {
		return "", "", "", fmt.Errorf("hash manifest: %w", err)
	}
	if manifestSha != rec.ManifestSHA256 {
		return "", "", "", fmt.Errorf("manifest bytes do not match receipt: %s != %s", manifestSha, rec.ManifestSHA256)
	}
	graphSha, err := sha256OfPath(graphPath)
	if err != nil {
		return "", "", "", fmt.Errorf("hash symbol graph: %w", err)
	}
	if graphSha != rec.SymbolGraphSHA256 {
		return "", "", "", fmt.Errorf("symbol graph bytes do not match receipt: %s != %s", graphSha, rec.SymbolGraphSHA256)
	}
	return analysisDir, manifestPath, graphPath, nil
}

// persistFreehandBaselineFromBuild snapshots the immutable baseline from the freehand staging after a
// successful build. It STRICTLY revalidates the analysis staging (verifyFreehandStagingStrict) — every
// receipt/index field, all hashes, the config digest, and the content address are recomputed from live
// inputs and must match — then persists exactly the verified manifest/graph and the EXACT kernel that
// gen_snapshot consumed.
func persistFreehandBaselineFromBuild(projectDir, appDill, analyzerSha, flutterRoot, toolchainVersion, preBuildSourceDigest string) (string, error) {
	analysisDir, manifestPath, graphPath, err := verifyFreehandStagingStrict(projectDir, appDill, analyzerSha)
	if err != nil {
		return "", fmt.Errorf("freehand staging revalidation failed: %w", err)
	}
	// TOCTOU: the source kernel MUST be from the same source as the AOT app.dill. Re-capture the source
	// state and fail BEFORE persistence if it drifted since the pre-build capture.
	if strings.TrimSpace(preBuildSourceDigest) != "" {
		postDigest, err := captureCompilationInputDigest(projectDir)
		if err != nil {
			return "", fmt.Errorf("re-capture compilation input digest: %w", err)
		}
		if postDigest != preBuildSourceDigest {
			return "", fmt.Errorf("source/config changed during the build (TOCTOU): pre=%s post=%s; refusing to persist a baseline whose source kernel may not match the AOT app.dill", preBuildSourceDigest[:12], postDigest[:12])
		}
	}
	soroqConfig, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		return "", err
	}
	pubspec, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		return "", err
	}
	meta, err := buildSoroqBundledMetadata(soroqConfig, pubspec)
	if err != nil {
		return "", fmt.Errorf("compute runtime identity: %w", err)
	}
	pkgConfigSha, err := sha256OfPath(filepath.Join(projectDir, ".dart_tool", "package_config.json"))
	if err != nil {
		return "", fmt.Errorf("hash package_config.json: %w", err)
	}
	// Dual-kernel v2: build the source-kernel recipe + generate the non-AOT source-fidelity companion.
	recipe, err := buildFreehandSourceKernelRecipe(projectDir, flutterRoot)
	if err != nil {
		return "", fmt.Errorf("build source-kernel recipe: %w", err)
	}
	sourceKernel, err := os.CreateTemp("", "soroq-source-kernel-*.dill")
	if err != nil {
		return "", err
	}
	sourceKernelPath := sourceKernel.Name()
	sourceKernel.Close()
	defer os.Remove(sourceKernelPath)
	if _, err := generateFreehandSourceKernel(projectDir, flutterRoot, recipe, sourceKernelPath); err != nil {
		return "", fmt.Errorf("generate source-fidelity kernel: %w", err)
	}
	frontendPatchset, err := frontendPatchsetSHA(flutterRoot)
	if err != nil {
		return "", fmt.Errorf("frontend patchset digest: %w", err)
	}
	// Resolve the IMMUTABLE base runtime dependency graph. This is what a later patch's dependency
	// descriptor is anchored against, so it is captured from the same settled source state as the kernels
	// and persisted verbatim beside the baseline. A graph that cannot be resolved (an unresolved runtime
	// edge, an unpinned dependency) fails the release closed rather than producing a base that can never
	// accept a dependency patch.
	baseDepGraph, err := resolveRuntimeGraphPinned(projectDir)
	if err != nil {
		return "", fmt.Errorf("resolve base runtime dependency graph: %w", err)
	}
	// Re-derive the freehand base contract from THIS project so its identity is bound into the immutable
	// baseline. It is the same derivation the build used, so the digest describes the surface the base
	// actually retained.
	contract, err := generateFreehandBaseContract(projectDir, nil, nil)
	if err != nil {
		return "", fmt.Errorf("derive freehand base contract: %w", err)
	}
	frontendRev, frameworkRev, dartRev, engineRev := collectFrontendProvenance(flutterRoot, toolchainVersion)
	bl := FreehandBaselineMeta{
		ContractSchema:      contract.Schema,
		ContractDigest:      contract.Digest,
		IdentitySchema:      freehandIdentitySchema,
		AnalyzerVersion:     analyzerSha,
		SourceKernelRecipe:  &recipe,
		PackageConfigSHA256: pkgConfigSha,
		FrontendRev:         frontendRev,
		FrontendPatchsetSHA: frontendPatchset,
		FrameworkRev:        frameworkRev,
		DartRev:             dartRev,
		EngineRev:           engineRev,
		AppID:               meta.Soroq.AppID,
		Version:             pubspecVersion(projectDir),
		RuntimeID:           meta.Soroq.RuntimeID,
		Arch:                "arm64",
		Channel:             meta.Soroq.Channel,
		// Retention VERIFIED: reaching here means verifyFreehandStagingStrict validated the analysis
		// staging (the build-DAG target ran discovery + injected --soroq_manifest into the gen_snapshot
		// that produced this exact app.dill). persistFreehandBaseline derives the count/hashes and fails
		// closed unless this evidence is complete.
		Retention: &FreehandRetentionEvidence{
			Verified:   true,
			AnalysisID: filepath.Base(analysisDir),
		},
	}
	return persistFreehandBaseline(projectDir, bl, appDill, sourceKernelPath, manifestPath, graphPath, baseDepGraph)
}

// freehandFinalizeBuild persists the immutable baseline and registers the release AFTER a fully
// successful build. Fail-build atomicity for the product release command: any non-nil buildErr aborts
// with NO baseline persisted and NO release delegate invoked — nothing partial is left behind.
func freehandFinalizeBuild(head []string, projectDir, appDill string, buildErr error, analyzerSha, flutterRoot, toolchain, preBuildSourceDigest string) error {
	if buildErr != nil {
		return fmt.Errorf("freehand build failed; no baseline persisted and no release registered: %w", buildErr)
	}
	if strings.TrimSpace(appDill) == "" {
		return errors.New("freehand build reported success but produced no app.dill")
	}
	relDir, err := freehandPersistFn(projectDir, appDill, analyzerSha, flutterRoot, toolchain, preBuildSourceDigest)
	if err != nil {
		return fmt.Errorf("persist freehand baseline: %w", err)
	}
	fmt.Fprintf(os.Stderr, "freehand immutable baseline persisted -> %s\n", relDir)

	// Delegate release registration with the freehand-generated manifest from the immutable baseline.
	delegateArgs := stripFlag(head, "engine", true)
	delegateArgs = stripFlag(delegateArgs, "build", true)
	delegateArgs = stripFlag(delegateArgs, "project-dir", false)
	absDill, err := filepath.Abs(appDill)
	if err != nil {
		return err
	}
	delegateArgs = append(delegateArgs, "--app-dill", absDill,
		"--patchable-manifest", filepath.Join(relDir, "soroq_app_manifest.txt"))

	// Forward the identity the CLI already knows. Without this the unified
	// `soroq release --platforms=ios` reaches the delegate and stops with
	// "--app-dill, --release-id and --app-id are required" -- asking the developer for values that are
	// sitting in soroq.yaml and in the baseline directory name. A zero-touch command must not hand back
	// homework it can do itself.
	if v := freehandProjectAppID(projectDir); v != "" && !hasFlag(delegateArgs, "app-id") {
		delegateArgs = append(delegateArgs, "--app-id", v)
	}
	// The immutable baseline directory IS the runtime id.
	if rid := filepath.Base(relDir); rid != "" && !hasFlag(delegateArgs, "runtime-id") {
		delegateArgs = append(delegateArgs, "--runtime-id", rid)
	}
	if v := freehandProjectVersion(projectDir); v != "" && !hasFlag(delegateArgs, "version") {
		delegateArgs = append(delegateArgs, "--version", v)
	}
	return freehandReleaseDelegate("release", delegateArgs)
}

// runReleaseIOSEngineBuildFreehand is the freehand build+persist flow. No patchable list, no lib/ files.
func runReleaseIOSEngineBuildFreehand(head, passthrough []string, projectDir, toolchain string) error {
	if _, err := ensureManifestTrust(projectDir); err != nil {
		return err
	}
	// iOS must pin the public half of the SAME project seed used by hosted freehand publish. The
	// manifest_trust key is a separate trust/runtime-identity domain and is not guaranteed to match.
	pinnedKeyHex, err := ensureProjectManifestSigningKey(projectDir)
	if err != nil {
		return fmt.Errorf("resolve iOS manifest-signing key: %w", err)
	}
	// Resolve Soroq's own packages in an isolated workspace. The customer's pubspec.yaml and
	// pubspec.lock are never touched; only .dart_tool/package_config.json (build output) is written.
	if _, err := prepareSoroqBuildResolution(projectDir); err != nil {
		return fmt.Errorf("prepare Soroq build resolution: %w", err)
	}
	dynamicInterfacePath, err := generateIOSEngineDynamicInterface(projectDir)
	if err != nil {
		return fmt.Errorf("generate iOS engine dynamic interface: %w", err)
	}
	flutterBin, err := resolveSoroqFlutterBin()
	if err != nil {
		return err
	}
	flutterRoot, err := flutterRootFromBin(flutterBin)
	if err != nil {
		return err
	}
	_, analyzerSha, err := installFreehandAnalyzer(flutterRoot)
	if err != nil {
		return fmt.Errorf("install freehand analyzer: %w", err)
	}
	if err := writeFreehandConfigAtomic(projectDir); err != nil {
		return fmt.Errorf("write freehand config: %w", err)
	}
	// Fail-safe: freehand activation must not persist past this command.
	defer removeFreehandConfig(projectDir)

	fmt.Fprintf(os.Stderr, "soroq release ios --engine --build (freehand): analyzer %s, no ios_engine.patchable required\n", analyzerSha[:12])
	passthrough = iosEngineBuildPassthrough(dynamicInterfacePath, passthrough)

	// Zero-touch runtime wiring: generate the dual-interface activator + bootstrap entrypoint under
	// .soroq/generated/ and redirect the build entrypoint to the bootstrap, so the compiled app.dill
	// contains the activator and auto-starts the controller with NO lib/ edits. The baseline and every
	// candidate patch build share this exact entrypoint (identical identity graph).
	// Declaring the dependency is not enough: the RESOLVED soroq_flutter must provide the APIs this
	// CLI's generated scaffolding calls. Checked here, on the path that actually builds, so an old
	// runtime is refused BEFORE generation instead of failing deep in the Dart front end with
	// "Method not found" against soroq_bootstrap.g.dart -- which surfaces to the developer as an
	// Xcode/codesign failure and is not actionable.
	if err := verifyFreehandRuntimeCompatibility(projectDir, flutterRoot); err != nil {
		return err
	}

	bootstrapRel, err := prepareFreehandZeroTouch(projectDir, pinnedKeyHex, passthrough)
	if err != nil {
		return fmt.Errorf("generate zero-touch freehand runtime wiring: %w", err)
	}
	passthrough = withFreehandBootstrapEntrypoint(bootstrapRel, passthrough)
	fmt.Fprintf(os.Stderr, "soroq release ios --engine --build (freehand): zero-touch entrypoint -> %s\n", bootstrapRel)

	// Repair a damaged cached analysis BEFORE building. The frontend's AOT target fails closed on one,
	// but cannot regenerate it — the analysis target's own stamp is still valid, so without this the
	// only escape was `flutter clean`.
	if err := ensureFreehandAnalysisCacheIntegrity(projectDir); err != nil {
		return fmt.Errorf("verify cached freehand analysis: %w", err)
	}
	// Re-resolve AFTER the generated wiring exists, so package_config.json is settled before the input
	// digest is captured. This writes only .dart_tool/ — pubspec.yaml and pubspec.lock stay byte-identical,
	// which is why the build below runs with --no-pub (Flutter's own pub get would rewrite the lock with
	// the frontend's newer SDK and leave the developer unable to `pub get` their own project).
	if _, perr := prepareSoroqBuildResolution(projectDir); perr != nil {
		return fmt.Errorf("resolve dependencies before release: %w", perr)
	}
	// TOCTOU: capture the COMPLETE resolved compilation-input digest (app + path-package sources,
	// pubspec.lock, package_config, generated Dart) BEFORE the (long) AOT build so the post-build
	// source-kernel compile can prove the source did not drift (else the source kernel would not match
	// the AOT app.dill and the diff would be against the wrong base). Malformed config fails closed.
	preBuildSourceDigest, err := captureCompilationInputDigest(projectDir)
	if err != nil {
		return fmt.Errorf("capture pre-build compilation input digest: %w", err)
	}

	// Fail-build atomicity: buildIOSAppDill's result is finalized as a PURE TAIL — freehandFinalizeBuild
	// is the only post-build path, so a failed build persists no baseline and calls no release delegate.
	appDill, buildErr := buildIOSAppDill(projectDir, toolchain, passthrough)
	return freehandFinalizeBuild(head, projectDir, appDill, buildErr, analyzerSha, flutterRoot, toolchain, preBuildSourceDigest)
}

// freehandProjectAppID reads app_id from the project's soroq.yaml.
func freehandProjectAppID(projectDir string) string {
	b, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parseTopLevelYaml(b)["app_id"])
}

// freehandProjectVersion reads the app version from pubspec.yaml, which is the version a store release
// would carry. Absent or unparsable yields "", and the delegate then reports the missing flag itself.
func freehandProjectVersion(projectDir string) string {
	b, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parseTopLevelYaml(b)["version"])
}

// invalidateFreehandAnalysisStamp removes the Flutter build stamp for SoroqFreehandAnalysis so the next
// build re-runs it. Returns a sentence describing what happened, for the caller's error message.
//
// Removing a stamp is safe by construction: a stamp's only role is to let Flutter skip a target it
// believes is up to date. Deleting one can cause redundant work, never incorrect work.
func invalidateFreehandAnalysisStamp(appDill string) string {
	stamp := filepath.Join(filepath.Dir(appDill), "soroq_freehand_analysis.stamp")
	switch err := os.Remove(stamp); {
	case err == nil:
		return "The analysis cache entry has been invalidated, so the next build regenerates it."
	case os.IsNotExist(err):
		return "No analysis cache entry was present to invalidate."
	default:
		return fmt.Sprintf("Could not invalidate the analysis cache entry at %s (%v); remove it by hand.", stamp, err)
	}
}

// ensureFreehandAnalysisCacheIntegrity runs BEFORE the build and repairs a cached analysis whose
// immutable outputs are incomplete.
//
// WHY IT MUST BE PRE-BUILD. The frontend's AOT target already fails closed on a damaged analysis dir:
//
//	Target aot_assembly_profile failed: [soroq-freehand] analysis dir …/<addr> missing soroq_analysis_receipt.json
//
// but failing is all it can do. SoroqFreehandAnalysis is a separate, still-valid cached target, so the
// analysis never re-runs and every subsequent build fails the same way — `flutter clean` was the only
// escape, which discards every other cached target and costs a full rebuild.
//
// A directory under soroq_freehand/ is only ever written as a complete set. One that is missing a member
// is therefore damaged, not in-progress: something outside the build removed or truncated it. Deleting
// the damaged directory and the analysis stamp makes the next target run regenerate both — turning an
// unrecoverable state into an ordinary rebuild of one target.
func ensureFreehandAnalysisCacheIntegrity(projectDir string) error {
	buildRoot := filepath.Join(projectDir, ".dart_tool", "flutter_build")
	buildDirs, err := filepath.Glob(filepath.Join(buildRoot, "*"))
	if err != nil || len(buildDirs) == 0 {
		return nil // nothing cached yet
	}
	// Every member the AOT wiring and baseline persistence require.
	required := []string{"soroq_analysis_receipt.json", "soroq_app_manifest.txt", "soroq_symbol_graph.json"}

	for _, buildDir := range buildDirs {
		analysisRoot := filepath.Join(buildDir, "soroq_freehand")
		entries, err := os.ReadDir(analysisRoot)
		if err != nil {
			continue
		}
		damaged := false
		for _, entry := range entries {
			if !entry.IsDir() || !contentAddrRe.MatchString(entry.Name()) {
				continue
			}
			dir := filepath.Join(analysisRoot, entry.Name())
			for _, member := range required {
				if info, err := os.Stat(filepath.Join(dir, member)); err != nil || info.Size() == 0 {
					fmt.Fprintf(os.Stderr,
						"soroq: cached freehand analysis %s is incomplete (%s missing or empty); regenerating it\n",
						entry.Name()[:12], member)
					if rmErr := os.RemoveAll(dir); rmErr != nil {
						return fmt.Errorf("remove damaged analysis dir %s: %w", dir, rmErr)
					}
					damaged = true
					break
				}
			}
		}
		if damaged {
			// Drop the stamp so the analysis target actually re-runs rather than being skipped.
			invalidateFreehandAnalysisStamp(filepath.Join(buildDir, "app.dill"))
		}
	}
	return nil
}
