package main

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

// Dart obfuscation renames declarations. Soroq's iOS lane binds redirects BY DECLARATION IDENTITY and
// its Android lane swaps AOT artifacts built from a specific symbol mapping, so an obfuscated build is
// exactly the case where "it produced a patch" and "the patch is correct" come apart -- and neither the
// release path nor the patch path looked at these flags at all.
//
// Nothing here claims obfuscation cannot work. It claims it is UNVERIFIED, and that shipping an
// unverified binding silently is worse than refusing it with a way forward.

func TestObfuscationFlagsAreDetected(t *testing.T) {
	for _, args := range [][]string{
		{"--obfuscate"},
		{"--obfuscate", "--split-debug-info=build/symbols"},
		{"--split-debug-info=build/symbols"},
		{"--dart-define=x=1", "--obfuscate"},
		{"--split-debug-info", "build/symbols"},
	} {
		if got := detectObfuscationFlags(args); len(got) == 0 {
			t.Errorf("detectObfuscationFlags(%v) found nothing; an obfuscated build must be recognised", args)
		}
	}
}

func TestOrdinaryBuildFlagsAreNotMistakenForObfuscation(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--release"},
		{"--dart-define=API=https://example.com"},
		{"--target", "lib/main_dev.dart"},
		{"--no-tree-shake-icons"},
		// Substrings must not trip it: a define whose VALUE mentions the word is still not obfuscation.
		{"--dart-define=NOTE=obfuscate-later"},
	} {
		if got := detectObfuscationFlags(args); len(got) != 0 {
			t.Errorf("detectObfuscationFlags(%v) = %v; ordinary flags must pass through", args, got)
		}
	}
}

func TestObfuscatedBuildIsRefusedWithAWayForward(t *testing.T) {
	err := guardUnverifiedBuildFlags([]string{"--obfuscate", "--split-debug-info=build/symbols"}, nil)
	if err == nil {
		t.Fatal("an obfuscated build must be refused rather than silently producing a patch of unknown correctness")
	}
	msg := err.Error()
	// The refusal's REASON changed with R6 and the assertion follows it. Before, obfuscated OTA was
	// unverified everywhere. Now it is implemented, and what a given command lacks is a toolchain whose
	// dart2bytecode can translate identities -- so the refusal must name that capability and how to get
	// past it, not merely say "unverified".
	for _, want := range []string{
		"--obfuscate",                                   // names what was seen
		"cannot bind an obfuscated base",                // states the real status
		freehandObfuscatedIdentityTranslationCapability, // names what is missing
		"SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS",            // gives the opt-in
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should mention %q:\n%s", want, msg)
		}
	}
}

// The opt-in NO LONGER applies to obfuscation, and that is the point.
//
// It used to let an unauthorized obfuscated release continue. The result was worse than a refusal:
// the release then recorded no binding and no captured map, and persisted an actually obfuscated base
// as an ordinary one. Every later patch against that baseline compiled, signed and installed cleanly
// and resolved nothing. An experiment must not be able to leave a reusable baseline behind.
func TestObfuscationOptInIsNoLongerHonoured(t *testing.T) {
	t.Setenv("SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS", "1")
	err := guardUnverifiedBuildFlags([]string{"--obfuscate"}, nil)
	if err == nil {
		t.Fatal("the override must not let an unauthorized obfuscated build proceed")
	}
	if !strings.Contains(err.Error(), "does NOT apply here") {
		t.Fatalf("the refusal should say the override no longer applies: %v", err)
	}
	// --flavor no longer needs the opt-in on its supported routes (flavor.go), and the opt-in does not
	// unlock the routes that still refuse it; see flavor_test.go.
}

func TestUnobfuscatedBuildIsUnaffected(t *testing.T) {
	if err := guardUnverifiedBuildFlags([]string{"--dart-define=A=b"}, nil); err != nil {
		t.Fatalf("an ordinary build must not be blocked: %v", err)
	}
}

// End to end: an obfuscated release must be refused with ZERO build invocations, so the developer
// learns about it immediately instead of after a full Gradle cycle.
func TestReleaseAndroidRefusesObfuscatedBuildWithoutBuilding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	buildCalls := 0
	prevBuild, prevGuard := androidReleaseBuildFn, androidReleaseEnvGuardFn
	androidReleaseBuildFn = func(string, string, string, []string) error { buildCalls++; return nil }
	androidReleaseEnvGuardFn = func(string, []string) error { return nil }
	t.Cleanup(func() { androidReleaseBuildFn, androidReleaseEnvGuardFn = prevBuild, prevGuard })

	err := runReleaseAndroid([]string{
		"--project-dir", projectDir, "--api", "http://127.0.0.1:1",
		"--", "--obfuscate", "--split-debug-info=build/symbols", "--target-platform", "android-arm64",
	})
	if err == nil {
		t.Fatal("an obfuscated release must be refused")
	}
	// [soroq] Option A: Android obfuscation is permitted only with a toolchain that can seed patches
	// from the base's map; with none resolved, the refusal names that capability.
	if !strings.Contains(err.Error(), androidObfuscationSeedCapability) {
		t.Fatalf("expected the missing-capability refusal, got: %v", err)
	}
	if buildCalls != 0 {
		t.Fatalf("refused after %d build(s); the guard must run before any build", buildCalls)
	}
}

// The explicit-artifact path must keep working with no flavor declared: an undeclared flavored path
// only warns and records the release as unflavored (so a later flavored patch is refused, not
// silently accepted). The flag-driven path is covered in flavor_test.go.
// END-TO-END: the documented flavor workaround must actually register a release.
//
// A refusal is only honest if the alternative it names works. This builds an artifact at the exact
// path a flavored Flutter build produces and registers it with --build=false --artifact, which is the
// command the refusal tells the developer to run.
func TestFlavoredArtifactRegistersViaTheDocumentedExplicitPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	// Exactly where `flutter build apk --release --flavor prod` puts it.
	flavored := filepath.Join(projectDir, "build", "app", "outputs", "apk", "prod", "release", "app-prod-release.apk")
	if err := os.MkdirAll(filepath.Dir(flavored), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeArtifactZip(t, flavored, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-flavor", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("flavored-app"),
	})

	var registered *domain.CreateReleaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]domain.Release{})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			var req domain.CreateReleaseRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			registered = &req
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Release{
				ID: req.ID, AppID: req.AppID, RuntimeID: req.RuntimeID, Version: req.Version,
				Platform: req.Platform, Arch: req.Arch, Channel: req.Channel,
			})
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.ReleaseArtifact{ReleaseID: "r", SHA256: "x", SizeBytes: 1})
		}
	}))
	t.Cleanup(server.Close)

	err := runReleaseAndroid([]string{
		"--project-dir", projectDir, "--api", server.URL,
		"--build=false", "--artifact", flavored,
		"--release-id", "com-example-app-prod-1-2-3-45", "--arch", "arm64-v8a",
	})
	if err != nil {
		t.Fatalf("the documented flavor workaround must work, got: %v", err)
	}
	if registered == nil {
		t.Fatal("no release was registered")
	}
	if registered.RuntimeID != "runtime-flavor" {
		t.Fatalf("registered the wrong artifact's identity: runtime_id=%q", registered.RuntimeID)
	}
}
