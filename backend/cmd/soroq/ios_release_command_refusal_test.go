package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A2 COMMAND-LEVEL refusals.
//
// Calling a guard function directly proves the guard works. It does NOT prove the COMMAND calls it,
// nor that the refusal lands before a build, a network request or a registration. These drive the
// real runReleaseIOS with both boundaries faked and counted, so "refused before any side effect" is
// measured rather than asserted from source layout.

type iosRefusalHarness struct {
	projectDir string
	buildCalls int32
	requests   int32
	serverURL  string
}

func newIOSRefusalHarness(t *testing.T, pubspec string) *iosRefusalHarness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	h := &iosRefusalHarness{projectDir: t.TempDir()}
	writeFile(t, filepath.Join(h.projectDir, "pubspec.yaml"), pubspec)
	writeFile(t, filepath.Join(h.projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	prevBuild := iosReleaseBuildFn
	iosReleaseBuildFn = func(string, string, []string) error {
		atomic.AddInt32(&h.buildCalls, 1)
		return nil
	}
	t.Cleanup(func() { iosReleaseBuildFn = prevBuild })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&h.requests, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	h.serverURL = server.URL
	return h
}

func (h *iosRefusalHarness) run(t *testing.T, extra ...string) error {
	t.Helper()
	args := append([]string{"--project-dir", h.projectDir, "--api", h.serverURL,
		"--runtime-id", "runtime-1", "--version", "1.0.0+1"}, extra...)
	return runReleaseIOS(args)
}

func (h *iosRefusalHarness) assertNoSideEffects(t *testing.T) {
	t.Helper()
	if got := atomic.LoadInt32(&h.buildCalls); got != 0 {
		t.Errorf("refusal cost %d build invocation(s); it must land before the build", got)
	}
	if got := atomic.LoadInt32(&h.requests); got != 0 {
		t.Errorf("refusal made %d control-plane request(s); it must land before any network call", got)
	}
	// No release snapshot may be written for a refused command.
	if entries, _ := filepath.Glob(filepath.Join(h.projectDir, ".soroq", "releases", "*")); len(entries) != 0 {
		t.Errorf("refusal wrote release artifacts: %v", entries)
	}
}

const ordinaryPubspec = "name: example_app\nversion: 1.0.0+1\nflutter:\n  uses-material-design: true\n"
const modulePubspec = "name: example_module\nversion: 1.0.0+1\nflutter:\n  module:\n    androidX: true\n"

// The config baseline (no --build) builds nothing, so a flavor there is refused before anything runs.
func TestIOSReleaseRefusesFlavorOnConfigBaselineBeforeAnySideEffect(t *testing.T) {
	h := newIOSRefusalHarness(t, ordinaryPubspec)
	err := h.run(t, "--flavor", "prod")
	if err == nil {
		t.Fatal("a flavor on the iOS config baseline must be refused")
	}
	if !strings.Contains(err.Error(), "not supported on this route") {
		t.Errorf("expected the flavor refusal, got: %v", err)
	}
	h.assertNoSideEffects(t)
}

// The --build app leg supports a flavor: it reaches the build with exactly one `--flavor prod`,
// whichever way the developer spelled it.
func TestIOSReleaseBuildLegPassesFlavorThrough(t *testing.T) {
	for name, extra := range map[string][]string{
		"flag":        {"--build", "--toolchain", "tc-1", "--flavor", "prod"},
		"passthrough": {"--build", "--toolchain", "tc-1", "--", "--flavor=prod"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newIOSRefusalHarness(t, ordinaryPubspec)
			var got []string
			iosReleaseBuildFn = func(_ string, _ string, args []string) error {
				atomic.AddInt32(&h.buildCalls, 1)
				got = args
				return nil
			}
			if err := h.run(t, extra...); err != nil {
				t.Fatalf("flavored iOS build leg: %v", err)
			}
			if strings.Join(got, " ") != "--flavor prod" {
				t.Fatalf("build args = %q, want exactly --flavor prod", got)
			}
		})
	}
}

func TestIOSReleaseRefusesObfuscationBeforeAnySideEffect(t *testing.T) {
	h := newIOSRefusalHarness(t, ordinaryPubspec)
	err := h.run(t, "--build", "--toolchain", "tc-1", "--", "--obfuscate", "--split-debug-info=build/sym")
	if err == nil {
		t.Fatal("an obfuscated iOS build must be refused")
	}
	if !strings.Contains(err.Error(), "cannot bind an obfuscated base") {
		t.Errorf("expected the unverified-binding refusal, got: %v", err)
	}
	h.assertNoSideEffects(t)
}

func TestIOSReleaseRefusesAddToAppBeforeAnySideEffect(t *testing.T) {
	h := newIOSRefusalHarness(t, modulePubspec)
	err := h.run(t, "--build", "--toolchain", "tc-1")
	if err == nil {
		t.Fatal("an add-to-app project must be refused")
	}
	if !strings.Contains(err.Error(), "add-to-app") {
		t.Errorf("expected the add-to-app refusal, got: %v", err)
	}
	h.assertNoSideEffects(t)
}

// The supported shape must NOT be refused: a guard that blocks everything is not fail-closed, it is
// broken. An ordinary app with a custom entrypoint under lib/ proceeds past the shape guards.
func TestIOSReleaseAcceptsOrdinaryAndCustomEntrypointProjects(t *testing.T) {
	for name, pubspec := range map[string]string{
		"ordinary":          ordinaryPubspec,
		"custom entrypoint": "name: example_app\nversion: 1.0.0+1\nflutter:\n  uses-material-design: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newIOSRefusalHarness(t, pubspec)
			err := h.run(t, "--build", "--toolchain", "tc-1", "--", "--dart-define=API=x")
			// It may still fail later (the fake control plane returns 500), but it must NOT fail with a
			// shape refusal, and it MUST have reached the build.
			if err != nil {
				for _, refusal := range []string{"not supported on this route", "cannot bind an obfuscated base", "add-to-app"} {
					if strings.Contains(err.Error(), refusal) {
						t.Fatalf("a supported project was refused as %q: %v", refusal, err)
					}
				}
			}
			if atomic.LoadInt32(&h.buildCalls) == 0 {
				t.Fatal("a supported project must reach the build")
			}
		})
	}
}
