package main

// T006 — Android on the CACHED TOOLCHAIN + the honest missing-artifact gate.
//
// These tests prove the gap T006 closes (the resolution + gate LOGIC; the real Android build is GATED
// in this checkout — no Android engine artifacts exist):
//   1. resolveAndroidEngineSource PREFERS a cached android toolchain (--toolchain <v>) over any repo
//      checkout: ~/.soroq/toolchains/<v>/android/.
//   2. With NEITHER a cached toolchain NOR an explicit --local-engine, the build path BLOCKS with the
//      EXACT missing Android artifact list (not a cryptic --local-engine / Flutter-not-found failure).
//   3. The ADVANCED --local-engine opt-in still works when explicitly given.
//   4. A clean valid project running `soroq release android` (build default) BLOCKS with the
//      missing-artifact list BEFORE any flutter-bin lookup and BEFORE the control-plane POST.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

// materializeCachedAndroidToolchain writes a minimal cached Android toolchain into a temp HOME laid out
// like `soroq toolchain install` would: ~/.soroq/toolchains/<version>/android/engine.json (+ would-be
// flat artifacts). engine.json presence is what resolveAndroidEngineSource keys on. Returns the version.
func materializeCachedAndroidToolchain(t *testing.T, version string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows parity; harmless on unix
	androidDir := filepath.Join(home, ".soroq", "toolchains", version, "android")
	if err := os.MkdirAll(androidDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	// engine.json analog (soroq.android_engine.v1) — its mere presence marks the toolchain installed.
	if err := os.WriteFile(filepath.Join(androidDir, "engine.json"), []byte(`{"schema":"soroq.android_engine.v1"}`), 0o644); err != nil {
		t.Fatalf("write android engine.json: %v", err)
	}
	return version
}

// TestResolveAndroidEngineSourcePrefersCachedToolchain proves --toolchain <v> resolves the cached
// Android toolchain (no repo engine checkout, no --local-engine).
func TestResolveAndroidEngineSourcePrefersCachedToolchain(t *testing.T) {
	version := materializeCachedAndroidToolchain(t, "2026.06.28-android-experimental")

	source, err := resolveAndroidEngineSource(version, nil)
	if err != nil {
		t.Fatalf("resolveAndroidEngineSource(--toolchain) error = %v", err)
	}
	if source.Kind != androidEngineSourceCachedToolchain {
		t.Fatalf("expected cached-toolchain source, got kind %d", source.Kind)
	}
	if source.ToolchainVersion != version {
		t.Fatalf("expected toolchain version %q recorded, got %q", version, source.ToolchainVersion)
	}
	wantDir := filepath.Join(os.Getenv("HOME"), ".soroq", "toolchains", version, "android")
	if source.BundleDir != wantDir {
		t.Fatalf("expected cached bundle dir %q, got %q", wantDir, source.BundleDir)
	}
	// The cache must WIN over an explicit --local-engine when --toolchain is given (explicit selection).
	source2, err := resolveAndroidEngineSource(version, []string{"--local-engine", "android_release_arm64"})
	if err != nil {
		t.Fatalf("resolveAndroidEngineSource(--toolchain + --local-engine) error = %v", err)
	}
	if source2.Kind != androidEngineSourceCachedToolchain {
		t.Fatalf("expected cached toolchain to win over --local-engine, got kind %d", source2.Kind)
	}
}

// TestResolveAndroidEngineSourceBlocksWithExactMissingList proves the silent hard-require is GONE: with
// no cached toolchain and no --local-engine, the resolver BLOCKS listing the EXACT missing artifacts.
func TestResolveAndroidEngineSourceBlocksWithExactMissingList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	_, err := resolveAndroidEngineSource("", nil)
	if err == nil {
		t.Fatalf("expected BLOCK with no cache and no --local-engine, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Android build BLOCKED") {
		t.Fatalf("expected a clear BLOCK message, got %q", msg)
	}
	// It must NOT be a cryptic --local-engine / Flutter-not-found failure.
	if strings.Contains(strings.ToLower(msg), "soroq-compatible flutter toolchain was not found") {
		t.Fatalf("block must not be the cryptic Flutter-not-found failure, got %q", msg)
	}
	// EVERY exact missing artifact (T002 §2) must be named.
	for _, want := range androidMissingArtifacts {
		if !strings.Contains(msg, want) {
			t.Fatalf("block message missing exact artifact %q\nfull message:\n%s", want, msg)
		}
	}
	// It must point at how to install a toolchain.
	if !strings.Contains(msg, "soroq toolchain install") {
		t.Fatalf("block should tell the operator how to install a toolchain, got %q", msg)
	}
}

// TestResolveAndroidEngineSourceAdvancedLocalEngineStillWorks proves the ADVANCED opt-in is preserved:
// an explicit --local-engine resolves to the advanced source (owner-accepted Android not broken).
func TestResolveAndroidEngineSourceAdvancedLocalEngineStillWorks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	source, err := resolveAndroidEngineSource("", []string{"--local-engine", "android_release_arm64", "--local-engine-host", "host_release_arm64"})
	if err != nil {
		t.Fatalf("resolveAndroidEngineSource(--local-engine) error = %v", err)
	}
	if source.Kind != androidEngineSourceAdvancedLocalEngine {
		t.Fatalf("expected advanced --local-engine source, got kind %d", source.Kind)
	}
	// The --local-engine=value form (single arg) must also be accepted as the explicit opt-in.
	source2, err := resolveAndroidEngineSource("", []string{"--local-engine=android_release_arm64"})
	if err != nil {
		t.Fatalf("resolveAndroidEngineSource(--local-engine=...) error = %v", err)
	}
	if source2.Kind != androidEngineSourceAdvancedLocalEngine {
		t.Fatalf("expected advanced source for --local-engine=..., got kind %d", source2.Kind)
	}
}

// TestResolveAndroidEngineSourceUninstalledToolchainBlocks proves a --toolchain pointing at an
// uninstalled version is a clear "not installed" refusal, not a silent fallback to the repo checkout.
func TestResolveAndroidEngineSourceUninstalledToolchainBlocks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	_, err := resolveAndroidEngineSource("not-installed", nil)
	if err == nil || !strings.Contains(err.Error(), "is not installed") {
		t.Fatalf("expected 'not installed' refusal for an uninstalled --toolchain, got %v", err)
	}
}

// TestRunReleaseAndroidBlocksCleanEnvBeforeFlutterAndAPI is acceptance #3: a clean valid project (no
// build script, no cached toolchain, no --local-engine, --build default) running `soroq release
// android` BLOCKS with the EXACT missing-artifact list BEFORE any flutter-bin lookup AND BEFORE the
// control-plane POST. The test server FAILS the test if any HTTP request reaches it.
func TestRunReleaseAndroidBlocksCleanEnvBeforeFlutterAndAPI(t *testing.T) {
	// Clean env: empty HOME with no cached toolchain; no Soroq Flutter on PATH.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SOROQ_FLUTTER_BIN", "") // force the resolver, not a real flutter
	t.Setenv("SOROQ_ENGINE_SRC", "")
	t.Setenv("FLUTTER_ENGINE", "")

	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("Android build must BLOCK before any control-plane request, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	// No --artifact, --build defaults true -> reaches the fallback build path -> BLOCK.
	err := runReleaseAndroid([]string{
		"--project-dir", projectDir,
		"--api", server.URL,
		"--release-id", "release-1",
	})
	if err == nil {
		t.Fatalf("expected missing-artifact BLOCK in a clean env, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Android build BLOCKED") {
		t.Fatalf("expected the clear missing-artifact block, got %q", msg)
	}
	// Not the cryptic Flutter-not-found failure (proves the gate fires BEFORE resolveSoroqFlutterBin).
	if strings.Contains(strings.ToLower(msg), "soroq-compatible flutter toolchain was not found") {
		t.Fatalf("block must precede the Flutter-bin lookup, got %q", msg)
	}
	for _, want := range androidMissingArtifacts {
		if !strings.Contains(msg, want) {
			t.Fatalf("clean-env block missing exact artifact %q\nfull message:\n%s", want, msg)
		}
	}
}

// TestRunPatchAndroidBlocksCleanEnvBeforeFlutterAndAPI is the patch-side of acceptance #2: a clean
// valid project running `soroq patch android` (no --release-id/--release-version, no recorded release,
// --build default) reaches the candidate build through resolveCandidateArtifactForReleaseSelection and
// BLOCKS with the EXACT missing-artifact list BEFORE any flutter-bin lookup and BEFORE any
// control-plane request (in this clean branch inspectProject/loadProjectCLIState/resolveProjectCommandConfig
// are all local, so no GET/POST precedes the build). The server FAILS the test if any request arrives.
func TestRunPatchAndroidBlocksCleanEnvBeforeFlutterAndAPI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // empty: no cached toolchain
	t.Setenv("USERPROFILE", home)
	t.Setenv("SOROQ_FLUTTER_BIN", "")
	t.Setenv("SOROQ_ENGINE_SRC", "")
	t.Setenv("FLUTTER_ENGINE", "")

	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("patch android must BLOCK before any control-plane request in the clean branch, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	// No --release-id, no --release-version, no recorded release, --build defaults true -> the candidate
	// build is reached via resolveCandidateArtifactForReleaseSelection -> BLOCK.
	err := runPatchAndroid([]string{
		"--project-dir", projectDir,
		"--api", server.URL,
	})
	if err == nil {
		t.Fatalf("expected missing-artifact BLOCK in a clean env, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Android build BLOCKED") {
		t.Fatalf("expected the clear missing-artifact block, got %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "soroq-compatible flutter toolchain was not found") {
		t.Fatalf("block must precede the Flutter-bin lookup, got %q", msg)
	}
	for _, want := range androidMissingArtifacts {
		if !strings.Contains(msg, want) {
			t.Fatalf("clean-env patch block missing exact artifact %q\nfull message:\n%s", want, msg)
		}
	}
}

// TestRunReleaseAndroidPrebuiltArtifactPathUnaffectedByGate proves the gate fires ONLY on the build
// path: `release android --build=false --artifact <x>` still registers exactly as before (owner-accepted
// behavior preserved; no missing-artifact block on a pre-built artifact).
func TestRunReleaseAndroidPrebuiltArtifactPathUnaffectedByGate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // empty: no cached toolchain
	t.Setenv("USERPROFILE", home)

	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})

	var captured domain.CreateReleaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			_ = json.NewDecoder(r.Body).Decode(&captured)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Release{
				ID: captured.ID, AppID: captured.AppID, RuntimeID: captured.RuntimeID,
				Version: captured.Version, Platform: captured.Platform, Arch: captured.Arch,
				Channel: captured.Channel,
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/artifact"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.ReleaseArtifact{
				ReleaseID: "release-1", FileName: "app-release.apk", SizeBytes: 1, SHA256: "sha",
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	if err := runReleaseAndroid([]string{
		"--project-dir", projectDir,
		"--artifact", artifactPath,
		"--api", server.URL,
		"--release-id", "release-1",
	}); err != nil {
		t.Fatalf("pre-built --artifact path must still register (no gate), got error = %v", err)
	}
	if captured.ID != "release-1" {
		t.Fatalf("expected release registration on the pre-built path, got %+v", captured)
	}
}
