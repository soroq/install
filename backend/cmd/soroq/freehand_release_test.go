package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const freehandYaml = `
ios_engine:
  enabled: true
`
const indexedYaml = `
ios_engine:
  enabled: true
  patchable:
    - lib/soroq_r4_acceptance.dart#r4Label
`
const disabledYaml = `
ios_engine:
  enabled: false
`

func projectWith(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "soroq.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestIsFreehandIOSBuild_ModeSelection(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want bool
	}{
		"freehand (enabled, no patchable)": {freehandYaml, true},
		"indexed (enabled + patchable)":    {indexedYaml, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := isFreehandIOSBuild(projectWith(t, c.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("freehand=%v, want %v", got, c.want)
			}
		})
	}
	// disabled engine lane must not be treated as freehand
	if got, _ := isFreehandIOSBuild(projectWith(t, disabledYaml)); got {
		t.Fatal("disabled engine lane must not be freehand")
	}
}

func TestFreehandConfig_WriteRemove(t *testing.T) {
	proj := t.TempDir()
	if err := writeFreehandConfigAtomic(proj); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(proj, ".soroq", "freehand_config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["schema"] != "soroq.freehand.config.v1" || m["mode"] != "freehand" ||
		m["identitySchema"] != "soroq.freehand.identity.v1" {
		t.Fatalf("bad config: %v", m)
	}
	// fail-safe cleanup
	removeFreehandConfig(proj)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("freehand config must be removed after cleanup")
	}
}

func TestInstallFreehandAnalyzer_PlacementAndVerify(t *testing.T) {
	// bundled analyzer source (resolved via SOROQ_FREEHAND_ANALYZER)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "soroq_kernel_analyze.dill")
	if err := os.WriteFile(src, []byte("ANALYZER-BYTES-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOROQ_FREEHAND_ANALYZER", src)

	flutterRoot := t.TempDir()
	dst, sha, err := installFreehandAnalyzer(flutterRoot)
	if err != nil {
		t.Fatal(err)
	}
	// installed at the FIXED path
	wantDst := filepath.Join(flutterRoot, "bin", "cache", "soroq", "soroq_kernel_analyze.dill")
	if dst != wantDst {
		t.Fatalf("installed at %s, want %s", dst, wantDst)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "ANALYZER-BYTES-v1" {
		t.Fatalf("analyzer bytes not copied: %q", got)
	}
	// real-byte sha matches the source
	wantSha, _ := sha256OfPath(src)
	if sha != wantSha {
		t.Fatalf("returned sha %s != %s", sha, wantSha)
	}
	// idempotent: second install returns same sha, no error
	if _, sha2, err := installFreehandAnalyzer(flutterRoot); err != nil || sha2 != sha {
		t.Fatalf("idempotent install failed: sha2=%s err=%v", sha2, err)
	}
}

func TestInstallFreehandAnalyzer_MissingSourceFails(t *testing.T) {
	t.Setenv("SOROQ_FREEHAND_ANALYZER", filepath.Join(t.TempDir(), "nope.dill"))
	if _, _, err := installFreehandAnalyzer(t.TempDir()); err == nil {
		t.Fatal("missing analyzer source must fail")
	}
}

func TestPubspecVersion(t *testing.T) {
	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "pubspec.yaml"), []byte("name: app\nversion: 1.2.3+9\n"), 0o644)
	if v := pubspecVersion(proj); v != "1.2.3+9" {
		t.Fatalf("version=%q, want 1.2.3+9", v)
	}
}

// writeValidStaging builds a FULLY-VALID freehand staging (correct content address, matching receipt +
// index, real manifest/graph hashes) exactly as the build-DAG target would. Each tamper test mutates
// ONE binding of a fresh valid staging so a refusal proves the specific binding, not a missing field.
func writeValidStaging(t *testing.T, analyzerSha string) (projectDir, appDill, root, analysisDir, addr string) {
	t.Helper()
	projectDir = t.TempDir()
	pkgConfig := filepath.Join(projectDir, ".dart_tool", "package_config.json")
	if err := os.MkdirAll(filepath.Dir(pkgConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkgConfig, []byte(`{"configVersion":2,"packages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	buildDir := filepath.Join(projectDir, ".dart_tool", "flutter_build", "cfg")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	appDill = filepath.Join(buildDir, "app.dill")
	if err := os.WriteFile(appDill, []byte("VALID-KERNEL-BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}

	appDillSha, _ := sha256OfPath(appDill)
	pkgConfigSha, _ := sha256OfPath(pkgConfig)
	configDigest := freehandConfigDigest()
	addr = freehandContentAddr(appDillSha, analyzerSha, pkgConfigSha, freehandIdentitySchema, configDigest)

	root = filepath.Join(buildDir, "soroq_freehand")
	analysisDir = filepath.Join(root, addr)
	if err := os.MkdirAll(analysisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(analysisDir, "soroq_app_manifest.txt")
	graph := filepath.Join(analysisDir, "symbol_graph.json")
	os.WriteFile(manifest, []byte("package:app/main.dart::MyWidget::build\n"), 0o644)
	os.WriteFile(graph, []byte(`{"schema":"soroq.freehand.identity.v1","symbols":[]}`), 0o644)
	manifestSha, _ := sha256OfPath(manifest)
	graphSha, _ := sha256OfPath(graph)

	rec := freehandAnalysisReceipt{
		Schema: freehandReceiptSchema, Mode: freehandConfigMode, IdentitySchema: freehandIdentitySchema,
		AnalysisID: addr, AppDillSHA256: appDillSha, ManifestSHA256: manifestSha, SymbolGraphSHA256: graphSha,
		AnalyzerSnapshotSHA: analyzerSha, PackageConfigSHA256: pkgConfigSha, ConfigDigest: configDigest,
	}
	rb, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(analysisDir, "soroq_analysis_receipt.json"), rb, 0o644)

	idx := freehandAnalysisIndex{
		Schema: "soroq.freehand.index.v1", Mode: freehandConfigMode, AnalysisID: addr,
		AppDillSHA256: appDillSha, AnalyzerSnapshotSHA: analyzerSha, PackageConfigSHA256: pkgConfigSha,
		IdentitySchema: freehandIdentitySchema, ConfigDigest: configDigest,
	}
	ib, _ := json.Marshal(idx)
	os.WriteFile(filepath.Join(root, "analysis_index.json"), ib, 0o644)
	return
}

func rewriteReceipt(t *testing.T, analysisDir string, mut func(*freehandAnalysisReceipt)) {
	t.Helper()
	p := filepath.Join(analysisDir, "soroq_analysis_receipt.json")
	b, _ := os.ReadFile(p)
	var r freehandAnalysisReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	mut(&r)
	nb, _ := json.Marshal(r)
	os.WriteFile(p, nb, 0o644)
}

func rewriteIndexRaw(t *testing.T, root string, m map[string]any) {
	t.Helper()
	b, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(root, "analysis_index.json"), b, 0o644)
}

// Happy path: a fully-valid staging passes strict revalidation.
func TestVerifyFreehandStagingStrict_HappyPath(t *testing.T) {
	proj, appDill, _, analysisDir, _ := writeValidStaging(t, "analyzer-sha-1")
	gotDir, man, graph, err := verifyFreehandStagingStrict(proj, appDill, "analyzer-sha-1")
	if err != nil {
		t.Fatalf("valid staging must pass: %v", err)
	}
	if gotDir != analysisDir {
		t.Fatalf("analysis dir = %s, want %s", gotDir, analysisDir)
	}
	if filepath.Dir(man) != analysisDir || filepath.Dir(graph) != analysisDir {
		t.Fatalf("manifest/graph must come from the verified dir")
	}
}

// A STALE analysis_index.json (pointing at a prior app.dill's analysis, as happens when the build
// system caches the analysis target across an app.dill change) must be TOLERATED: the analysis dir is
// located by the content address recomputed from live inputs — exactly as the frontend's AOT wiring
// does — not by the mutable index. This is the exact scenario the real CalorieNote build hit.
func TestVerifyFreehandStagingStrict_StaleIndexTolerated(t *testing.T) {
	proj, appDill, root, analysisDir, _ := writeValidStaging(t, "analyzer-sha-1")
	// Point the index at a DIFFERENT (stale) content address, leaving the real analysis dir intact.
	rewriteIndexRaw(t, root, map[string]any{
		"schema": "soroq.freehand.index.v1", "mode": "freehand",
		"analysis_id": "0000000000000000000000000000000000000000000000000000000000000000",
	})
	gotDir, _, _, err := verifyFreehandStagingStrict(proj, appDill, "analyzer-sha-1")
	if err != nil {
		t.Fatalf("a stale index must be tolerated (locate by recomputed address): %v", err)
	}
	if gotDir != analysisDir {
		t.Fatalf("stale index: located %s, want recomputed dir %s", gotDir, analysisDir)
	}
}

// Each binding, tampered in isolation, must fail closed.
func TestVerifyFreehandStagingStrict_TamperNegatives(t *testing.T) {
	const aSha = "analyzer-sha-1"
	cases := map[string]func(t *testing.T, proj, appDill, root, analysisDir, addr string){
		"tampered-manifest": func(t *testing.T, _, _, _, analysisDir, _ string) {
			os.WriteFile(filepath.Join(analysisDir, "soroq_app_manifest.txt"), []byte("TAMPERED\n"), 0o644)
		},
		"tampered-graph": func(t *testing.T, _, _, _, analysisDir, _ string) {
			os.WriteFile(filepath.Join(analysisDir, "symbol_graph.json"), []byte(`{"x":1}`), 0o644)
		},
		"tampered-appdill": func(t *testing.T, _, appDill, _, _, _ string) {
			os.WriteFile(appDill, []byte("DIFFERENT-KERNEL"), 0o644) // kernel mismatch
		},
		"tampered-pkgconfig": func(t *testing.T, proj, _, _, _, _ string) {
			os.WriteFile(filepath.Join(proj, ".dart_tool", "package_config.json"),
				[]byte(`{"configVersion":2,"packages":[{"name":"x"}]}`), 0o644)
		},
		"tampered-receipt-configdigest": func(t *testing.T, _, _, _, analysisDir, _ string) {
			rewriteReceipt(t, analysisDir, func(r *freehandAnalysisReceipt) { r.ConfigDigest = "deadbeef" })
		},
		"tampered-receipt-identityschema": func(t *testing.T, _, _, _, analysisDir, _ string) {
			rewriteReceipt(t, analysisDir, func(r *freehandAnalysisReceipt) { r.IdentitySchema = "soroq.freehand.identity.v2" })
		},
		"tampered-receipt-appdillsha": func(t *testing.T, _, _, _, analysisDir, _ string) {
			rewriteReceipt(t, analysisDir, func(r *freehandAnalysisReceipt) { r.AppDillSHA256 = "deadbeef" })
		},
		"tampered-receipt-manifestsha": func(t *testing.T, _, _, _, analysisDir, _ string) {
			rewriteReceipt(t, analysisDir, func(r *freehandAnalysisReceipt) { r.ManifestSHA256 = "deadbeef" })
		},
		"unknown-receipt-field": func(t *testing.T, _, _, _, analysisDir, _ string) {
			p := filepath.Join(analysisDir, "soroq_analysis_receipt.json")
			b, _ := os.ReadFile(p)
			var m map[string]any
			json.Unmarshal(b, &m)
			m["evil"] = 1
			nb, _ := json.Marshal(m)
			os.WriteFile(p, nb, 0o644)
		},
		"unknown-index-field": func(t *testing.T, _, _, root, _, addr string) {
			rewriteIndexRaw(t, root, map[string]any{
				"schema": "soroq.freehand.index.v1", "mode": "freehand", "analysis_id": addr, "evil": 1,
			})
		},
		"non-hex-receipt-analysis-id": func(t *testing.T, _, _, _, analysisDir, _ string) {
			// analysis_id must be a 64-hex content address; a traversal/garbage value is refused.
			rewriteReceipt(t, analysisDir, func(r *freehandAnalysisReceipt) { r.AnalysisID = "../../../etc/passwd" })
		},
		"stale-index-tolerated-but-mismatched-appdill-still-fails": func(t *testing.T, _, appDill, _, _, _ string) {
			// A stale index is fine, but the recomputed address must still find a matching analysis dir;
			// changing app.dill makes the recomputed dir nonexistent -> refused (not silently accepted).
			os.WriteFile(appDill, []byte("KERNEL-CHANGED-AFTER-ANALYSIS"), 0o644)
		},
		"index-receipt-disagree": func(t *testing.T, _, _, root, _, addr string) {
			// index keeps a valid (matching) analysis_id but flips a shared bound field
			rewriteIndexRaw(t, root, map[string]any{
				"schema": "soroq.freehand.index.v1", "mode": "freehand", "analysis_id": addr,
				"app_dill_sha256": "x", "analyzer_snapshot_sha256": aSha, "package_config_sha256": "y",
				"identity_schema": freehandIdentitySchema, "config_digest": "z",
			})
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			proj, appDill, root, analysisDir, addr := writeValidStaging(t, aSha)
			tamper(t, proj, appDill, root, analysisDir, addr)
			if _, _, _, err := verifyFreehandStagingStrict(proj, appDill, aSha); err == nil {
				t.Fatalf("%s: tampered staging must be refused", name)
			}
		})
	}
}

// Fail-build atomicity: a non-nil buildErr persists NO baseline and calls NO release delegate.
func TestFreehandFinalizeBuild_FailedBuildIsAtomic(t *testing.T) {
	proj := t.TempDir()
	persistCalls, delegateCalls := 0, 0
	origPersist, origDelegate := freehandPersistFn, freehandReleaseDelegate
	defer func() { freehandPersistFn, freehandReleaseDelegate = origPersist, origDelegate }()
	freehandPersistFn = func(projectDir, appDill, analyzerSha, flutterRoot, toolchain, preBuildSourceDigest string) (string, error) {
		persistCalls++
		return "", errors.New("persist must never be called on a failed build")
	}
	freehandReleaseDelegate = func(verb string, args []string) error {
		delegateCalls++
		return nil
	}

	err := freehandFinalizeBuild(nil, proj, filepath.Join(proj, "app.dill"),
		errors.New("gen_snapshot/kernel build failed"), "analyzer-sha", t.TempDir(), "toolchain-x", "")
	if err == nil {
		t.Fatal("failed build must return a non-nil error")
	}
	if persistCalls != 0 {
		t.Fatalf("failed build must not persist a baseline (got %d persist calls)", persistCalls)
	}
	if delegateCalls != 0 {
		t.Fatalf("failed build must not invoke the release delegate (got %d calls)", delegateCalls)
	}
	if _, statErr := os.Stat(filepath.Join(proj, ".soroq", "releases")); statErr == nil {
		t.Fatal("failed build must leave no .soroq/releases directory")
	}
}

// Positive sibling: on a successful build the delegate IS invoked exactly once (so the zero-count in the
// failed-build test is meaningful — the seam is wired).
func TestFreehandFinalizeBuild_SuccessInvokesDelegateOnce(t *testing.T) {
	proj := t.TempDir()
	relDir := filepath.Join(proj, ".soroq", "releases", "rt-xyz")
	os.MkdirAll(relDir, 0o755)
	appDill := filepath.Join(proj, "app.dill")
	os.WriteFile(appDill, []byte("K"), 0o644)

	delegateCalls := 0
	var gotVerb string
	var gotArgs []string
	origPersist, origDelegate := freehandPersistFn, freehandReleaseDelegate
	defer func() { freehandPersistFn, freehandReleaseDelegate = origPersist, origDelegate }()
	freehandPersistFn = func(projectDir, ad, analyzerSha, flutterRoot, toolchain, preBuildSourceDigest string) (string, error) {
		return relDir, nil
	}
	freehandReleaseDelegate = func(verb string, args []string) error {
		delegateCalls++
		gotVerb, gotArgs = verb, args
		return nil
	}

	if err := freehandFinalizeBuild([]string{"ios", "--engine", "--build"}, proj, appDill, nil,
		"analyzer-sha", t.TempDir(), "toolchain-x", ""); err != nil {
		t.Fatalf("successful finalize must not error: %v", err)
	}
	if delegateCalls != 1 {
		t.Fatalf("successful build must invoke the release delegate exactly once, got %d", delegateCalls)
	}
	if gotVerb != "release" {
		t.Fatalf("delegate verb = %q, want release", gotVerb)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--app-dill") || !strings.Contains(joined, "--patchable-manifest") {
		t.Fatalf("delegate args missing --app-dill/--patchable-manifest: %v", gotArgs)
	}
}

// An installed frontend (a manifest sits beside the subdir) whose signature does NOT verify must fail
// closed — never silently fall back to the source hash (which would bypass the authoritative identity).
func TestFrontendPatchsetSHA_InstalledManifestBadSigFailsClosed(t *testing.T) {
	base := t.TempDir()
	versionDir := filepath.Join(base, "soroq-flutter-frontend-x")
	root := filepath.Join(versionDir, "flutter-sdk-src")
	targets := filepath.Join(root, "packages", "flutter_tools", "lib", "src", "build_system", "targets")
	os.MkdirAll(targets, 0o755)
	// full source present so the fallback WOULD succeed if (wrongly) taken
	os.WriteFile(filepath.Join(targets, "ios.dart"), []byte("// soroq\n"), 0o644)
	os.WriteFile(filepath.Join(root, "packages", "flutter_tools", "lib", "src", "soroq_metadata.dart"), []byte("// soroq\n"), 0o644)
	// a manifest marks this as an installed frontend, but the signature is bogus
	os.WriteFile(filepath.Join(versionDir, "manifest.json"),
		[]byte(`{"schema":"soroq.frontend.v1","soroq_frontend_version":"x","flutter_revision":"f74781f6","patchset_sha256":"deadbeef","frontend_subdir":"flutter-sdk-src"}`), 0o644)
	os.WriteFile(filepath.Join(versionDir, "manifest.sig"), []byte("00"), 0o644)
	if _, err := frontendPatchsetSHA(root); err == nil {
		t.Fatal("installed frontend with an unverifiable manifest signature must fail closed (no source-hash fallback)")
	}
}

// frontend_patchset_sha256 is deterministic and changes when any Soroq frontend file changes.
func TestFrontendPatchsetSHA_DeterministicAndSensitive(t *testing.T) {
	root := t.TempDir()
	targets := filepath.Join(root, "packages", "flutter_tools", "lib", "src", "build_system", "targets")
	os.MkdirAll(targets, 0o755)
	os.WriteFile(filepath.Join(targets, "ios.dart"), []byte("// soroq patch A\n"), 0o644)
	os.WriteFile(filepath.Join(targets, "soroq_freehand.dart"), []byte("// soroq freehand\n"), 0o644)
	os.WriteFile(filepath.Join(targets, "unrelated.dart"), []byte("// plain flutter file\n"), 0o644)
	metaPath := filepath.Join(root, "packages", "flutter_tools", "lib", "src", "soroq_metadata.dart")
	os.WriteFile(metaPath, []byte("// soroq metadata v1\n"), 0o644)

	a, err := frontendPatchsetSHA(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := frontendPatchsetSHA(root)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("frontend patchset digest must be deterministic")
	}
	// changing a Soroq frontend file changes the digest (distinguishes patchsets sharing the base commit)
	os.WriteFile(filepath.Join(targets, "soroq_freehand.dart"), []byte("// soroq freehand PATCHSET-B\n"), 0o644)
	c, err := frontendPatchsetSHA(root)
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Fatal("a changed Soroq frontend file must change the patchset digest")
	}
	// a missing expected file fails closed
	os.Remove(metaPath)
	if _, err := frontendPatchsetSHA(root); err == nil {
		t.Fatal("missing soroq_metadata.dart must fail closed")
	}
}

// The full compilation-input digest covers app lib (incl. generated *.g.dart), pubspec.lock,
// package_config, and local/path-package sources; any drift changes it; pub-cache deps are excluded.
func TestCaptureCompilationInputDigest(t *testing.T) {
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, "lib"), 0o755)
	os.MkdirAll(filepath.Join(proj, ".dart_tool"), 0o755)
	os.WriteFile(filepath.Join(proj, "lib", "main.dart"), []byte("void main(){}"), 0o644)
	os.WriteFile(filepath.Join(proj, "lib", "gen.g.dart"), []byte("// generated"), 0o644)
	os.WriteFile(filepath.Join(proj, "pubspec.yaml"), []byte("name: app\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "pubspec.lock"), []byte("packages: {}\n"), 0o644)
	// a local path-package dependency (mutable) + a pub-cache dep (immutable, excluded)
	pathPkg := t.TempDir()
	os.MkdirAll(filepath.Join(pathPkg, "lib"), 0o755)
	os.WriteFile(filepath.Join(pathPkg, "lib", "dep.dart"), []byte("int dep() => 1;"), 0o644)
	os.WriteFile(filepath.Join(proj, ".dart_tool", "package_config.json"), []byte(`{"configVersion":2,"packages":[
	 {"name":"app","rootUri":"../"},
	 {"name":"local","rootUri":"file://`+pathPkg+`"},
	 {"name":"cached","rootUri":"file:///Users/x/.pub-cache/hosted/pub.dev/cached-1.0.0"}
	]}`), 0o644)

	d1, err := captureCompilationInputDigest(proj)
	if err != nil {
		t.Fatal(err)
	}
	if d1 == "" {
		t.Fatal("empty digest")
	}
	// deterministic: same inputs -> same digest
	d2, _ := captureCompilationInputDigest(proj)
	if d1 != d2 {
		t.Fatalf("non-deterministic: %s != %s", d1, d2)
	}
	// generated *.g.dart change -> digest changes
	os.WriteFile(filepath.Join(proj, "lib", "gen.g.dart"), []byte("// generated v2"), 0o644)
	if d3, _ := captureCompilationInputDigest(proj); d3 == d1 {
		t.Fatal("generated source change must change the digest")
	}
	os.WriteFile(filepath.Join(proj, "lib", "gen.g.dart"), []byte("// generated"), 0o644)
	// path-package source change -> digest changes
	os.WriteFile(filepath.Join(pathPkg, "lib", "dep.dart"), []byte("int dep() => 2;"), 0o644)
	if d4, _ := captureCompilationInputDigest(proj); d4 == d1 {
		t.Fatal("path-package source change must change the digest")
	}
	os.WriteFile(filepath.Join(pathPkg, "lib", "dep.dart"), []byte("int dep() => 1;"), 0o644)
	// pubspec.lock change -> digest changes
	os.WriteFile(filepath.Join(proj, "pubspec.lock"), []byte("packages: {a: 1}\n"), 0o644)
	if d5, _ := captureCompilationInputDigest(proj); d5 == d1 {
		t.Fatal("pubspec.lock change must change the digest")
	}
}

// Toolchain binding: same plan, different tool bytes -> different artifact id (distinct immutable dir).
func TestFreehandToolchainBinding_ArtifactIdentity(t *testing.T) {
	b1 := FreehandToolchainBinding{ToolchainVersion: "tc-1", Dart2BytecodeSHA256: "aaa", DartAotRuntimeSHA256: "bbb", PlatformDillSHA256: "ccc", AnalyzerSnapshotSHA256: "ddd", ModuleSchema: "soroq.freehand.module.v1"}
	b2 := b1
	b2.Dart2BytecodeSHA256 = "CHANGED"
	d1, _ := b1.digest()
	d2, _ := b2.digest()
	if d1 == d2 {
		t.Fatal("a changed tool sha must change the binding digest")
	}
	id1 := freehandSHA256Bytes([]byte("plan|" + d1))
	id2 := freehandSHA256Bytes([]byte("plan|" + d2))
	if id1 == id2 {
		t.Fatal("same plan + different toolchain binding must yield a different artifact id")
	}
}

// NOTE: strict existing-artifact reuse + full tamper matrix (every member + every bound metadata field,
// incl. the durable ABI manifest) now lives in freehand_patch_tamper_test.go under the v1 manifest-bound
// contract. The older, pre-manifest happy-path test was removed to avoid a stale duplicate contract.

// Malformed / missing package config fails the compilation-input digest closed.
func TestCompilationInputDigest_MalformedConfigFailsClosed(t *testing.T) {
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, "lib"), 0o755)
	os.MkdirAll(filepath.Join(proj, ".dart_tool"), 0o755)
	os.WriteFile(filepath.Join(proj, "lib", "main.dart"), []byte("void main(){}"), 0o644)
	os.WriteFile(filepath.Join(proj, "pubspec.yaml"), []byte("name: app\n"), 0o644)
	// missing package_config
	if _, err := captureCompilationInputDigest(proj); err == nil {
		t.Fatal("missing package_config must fail closed")
	}
	// malformed package_config
	os.WriteFile(filepath.Join(proj, ".dart_tool", "package_config.json"), []byte("{not json"), 0o644)
	if _, err := captureCompilationInputDigest(proj); err == nil {
		t.Fatal("malformed package_config must fail closed")
	}
}

// Concurrent identical artifact producers converge on ONE valid immutable dir (strict-verify accepts the
// winner); an interrupted producer (incomplete dir) is refused, never accepted as final.
func TestPatchArtifact_ConcurrentIdenticalAndInterrupted(t *testing.T) {
	dir := t.TempDir()
	binding := FreehandToolchainBinding{ToolchainVersion: "tc", Dart2BytecodeSHA256: "x", DartAotRuntimeSHA256: "y", PlatformDillSHA256: "z", AnalyzerSnapshotSHA256: "a", ModuleSchema: "soroq.freehand.module.v1"}
	bd, _ := binding.digest()
	// v1 manifest-bound contract: a valid (empty-change) durable ABI + plan, and artifact_id = sha(plan|binding|manifest).
	descriptor := testDependencyDescriptor()
	descriptorBytes, _ := json.MarshalIndent(descriptor, "", "  ")
	manifestBytes, _ := json.MarshalIndent(map[string]any{
		"schema":              "soroq.freehand.module.v2",
		"module_library":      "soroq-freehand:///import/prefix/1111111111111111111111111111111111111111111111111111111111111111/soroq_freehand_module.dart",
		"module_graph_digest": "1111111111111111111111111111111111111111111111111111111111111111", "carried_libraries": []any{},
		"replacement_abi": []any{}, "carried_new_code": []any{},
		"dependency_descriptor_digest": descriptor.DescriptorDigest,
	}, "", "  ")
	manifestSHA := freehandSHA256Bytes(manifestBytes)
	planBytes, _ := json.MarshalIndent(map[string]any{
		"schema": "soroq.freehand.patch_plan.v1", "changed_patchable": []any{},
		"diff":                         map[string]any{"changed": []any{}, "changedPatchable": []any{}, "newCodeClosure": []any{}},
		"dependency_descriptor_digest": descriptor.DescriptorDigest,
	}, "", "  ")
	planSHA := freehandSHA256Bytes(planBytes)
	moduleSrcBytes := []byte("// m")
	moduleBCBytes := []byte("BC")
	aid := computeFreehandArtifactID(planSHA, bd, manifestSHA, descriptor.DescriptorDigest)
	write := func(d string, complete bool) {
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "soroq_freehand_module.dart"), moduleSrcBytes, 0o600)
		os.WriteFile(filepath.Join(d, "soroq_freehand_module.bytecode"), moduleBCBytes, 0o600)
		os.WriteFile(filepath.Join(d, "soroq_freehand_module_manifest.json"), manifestBytes, 0o600)
		os.WriteFile(filepath.Join(d, "patch_plan.json"), planBytes, 0o600)
		os.WriteFile(filepath.Join(d, freehandDependencyDescriptorFile), descriptorBytes, 0o600)
		if !complete {
			return // interrupted: no patch_artifact.json
		}
		m := FreehandPatchArtifactMeta{
			Schema: "soroq.freehand.patch_artifact.v2", ArtifactID: aid,
			PatchPlanSHA256: planSHA, ModuleManifestSHA256: manifestSHA,
			ModuleLibrary:      "soroq-freehand:///import/prefix/1111111111111111111111111111111111111111111111111111111111111111/soroq_freehand_module.dart",
			ModuleGraphDigest:  "1111111111111111111111111111111111111111111111111111111111111111",
			ModuleSourceSHA256: freehandSHA256Bytes(moduleSrcBytes), ModuleBytecodeSHA256: freehandSHA256Bytes(moduleBCBytes),
			ToolchainBinding: &binding, ToolchainBindingDigest: bd, ChangedIdentities: []string{},
			DependencyDescriptorDigest: descriptor.DescriptorDigest,
			DependencyDescriptorSHA256: freehandSHA256Bytes(descriptorBytes),
			BaseDependencyGraphDigest:  descriptor.BaseGraphDigest,
		}
		mb, _ := json.MarshalIndent(m, "", "  ")
		os.WriteFile(filepath.Join(d, "patch_artifact.json"), mb, 0o600)
	}
	// interrupted producer -> incomplete dir must be refused (never treated as a valid artifact)
	inter := filepath.Join(dir, aid+"-interrupted")
	write(inter, false)
	if err := verifyExistingPatchArtifact(inter, aid); err == nil {
		t.Fatal("interrupted (incomplete) artifact must be refused")
	}
	// two identical concurrent producers -> both verify the same complete winner
	winner := filepath.Join(dir, aid)
	write(winner, true)
	for i := 0; i < 2; i++ {
		if err := verifyExistingPatchArtifact(winner, aid); err != nil {
			t.Fatalf("identical concurrent producer %d must accept the winner: %v", i, err)
		}
	}
}
