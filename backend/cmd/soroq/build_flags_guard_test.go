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
	err := guardUnverifiedBuildFlags([]string{"--obfuscate", "--split-debug-info=build/symbols"})
	if err == nil {
		t.Fatal("an obfuscated build must be refused rather than silently producing a patch of unknown correctness")
	}
	msg := err.Error()
	for _, want := range []string{
		"--obfuscate",                        // names what was seen
		"not verified",                       // states the real status
		"SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS", // gives the opt-in
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should mention %q:\n%s", want, msg)
		}
	}
}

func TestObfuscationOptInIsHonoured(t *testing.T) {
	t.Setenv("SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS", "1")
	if err := guardUnverifiedBuildFlags([]string{"--obfuscate"}); err != nil {
		t.Fatalf("the documented opt-in must allow the build to proceed: %v", err)
	}
}

func TestUnobfuscatedBuildIsUnaffected(t *testing.T) {
	if err := guardUnverifiedBuildFlags([]string{"--dart-define=A=b"}); err != nil {
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
		"--", "--obfuscate", "--split-debug-info=build/symbols",
	})
	if err == nil {
		t.Fatal("an obfuscated release must be refused")
	}
	if !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("expected the unverified-binding refusal, got: %v", err)
	}
	if buildCalls != 0 {
		t.Fatalf("refused after %d build(s); the guard must run before any build", buildCalls)
	}
}

func TestFlavorFlagsAreDetectedInBothForms(t *testing.T) {
	for _, args := range [][]string{{"--flavor", "prod"}, {"--flavor=prod"}, {"--release", "--flavor", "dev"}} {
		if len(detectFlavorFlags(args)) == 0 {
			t.Errorf("detectFlavorFlags(%v) found nothing", args)
		}
	}
	for _, args := range [][]string{nil, {"--release"}, {"--dart-define=FLAVOR=prod"}} {
		if got := detectFlavorFlags(args); len(got) != 0 {
			t.Errorf("detectFlavorFlags(%v) = %v; only the --flavor flag itself counts", args, got)
		}
	}
}

func TestFlavoredBuildIsRefusedAndNamesTheSupportedPath(t *testing.T) {
	err := guardFlavoredBuild([]string{"--flavor", "prod"})
	if err == nil {
		t.Fatal("Soroq cannot find a flavored build's artifact, so building one must be refused")
	}
	for _, want := range []string{"no flavor support", "--build=false --artifact", "outputs/apk/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q:\n%s", want, err)
		}
	}
}

// The escape hatch must be real: supplying a flavored artifact explicitly is the SUPPORTED path, so it
// must not be blocked. A refusal with no working alternative would just be an outage.
func TestExplicitlySuppliedArtifactIsNeverBlockedByTheFlavorGuard(t *testing.T) {
	// The guard only ever inspects passthrough BUILD args; --build=false supplies none.
	if err := guardFlavoredBuild(nil); err != nil {
		t.Fatalf("the --build=false path passes no build args and must not be blocked: %v", err)
	}
}

func TestFlavorOptInIsHonoured(t *testing.T) {
	t.Setenv("SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS", "1")
	if err := guardFlavoredBuild([]string{"--flavor=prod"}); err != nil {
		t.Fatalf("the documented opt-in must allow the build to proceed: %v", err)
	}
}

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
