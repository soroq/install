package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// THE REAL HARD-OTA ROUTE.
//
// `soroq release ios --engine --build` routes through runReleaseIOSEngineBuild ->
// runReleaseIOSEngineBuildFreehand. Guarding runReleaseIOS (the CONFIG lane) does not protect it, so
// these drive the actual router and assert that a refused shape leaves the project untouched.
//
// "Untouched" is measured against the specific mutations this route performs, in the order it would
// perform them: a signing key written into soroq.yaml, rewritten dependency resolution, generated
// scaffold files, an installed analyzer, a build, a delegate invocation, and any persisted artifact.

// engineLaneCounters counts every production boundary a refusal must not reach. The filesystem
// snapshot alone cannot see a network call, a delegate invocation or an analyzer install, so each is
// counted directly.
type engineLaneCounters struct {
	manifestTrust int32
	resolution    int32
	scaffold      int32
	build         int32
	delegate      int32
	freehand      int32
	httpRequests  int32
}

type engineLaneProject struct {
	dir      string
	counters *engineLaneCounters
	apiURL   string
}

func newEngineLaneProject(t *testing.T, pubspec string) *engineLaneProject {
	return newEngineLaneProjectWithConfig(t, pubspec, engineFreehandConfig)
}

// A `patchable:` list with at least one entry selects the SCAFFOLDED route; an empty/absent list is
// freehand (isFreehandIOSBuild). Both must be reachable here or half the counters stay unproven.
const engineFreehandConfig = "app_id: com.example.app\nchannel: stable\nios_engine:\n  enabled: true\n"
const engineScaffoldedConfig = "app_id: com.example.app\nchannel: stable\nios_engine:\n  enabled: true\n  patchable:\n    - lib/main.dart\n"

func newEngineLaneProjectWithConfig(t *testing.T, pubspec, soroqYAML string) *engineLaneProject {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), pubspec)
	// iOS engine lane enabled, and deliberately NO manifest_trust: if a guard ran too late,
	// ensureManifestTrust would write a signing key here and the snapshot below would catch it.
	writeFile(t, filepath.Join(dir, "soroq.yaml"), soroqYAML)
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatalf("mkdir lib: %v", err)
	}
	writeFile(t, filepath.Join(dir, "lib", "main.dart"), "void main() {}\n")

	c := &engineLaneCounters{}
	prevTrust, prevRes, prevScaffold := engineLaneEnsureManifestTrustFn, engineLanePrepareResolutionFn, engineLaneGenerateScaffoldFn
	prevBuild, prevDelegate, prevFreehand := engineLaneBuildAppDillFn, engineLaneDelegateFn, engineLaneFreehandFn
	engineLaneEnsureManifestTrustFn = func(string) (string, error) { atomic.AddInt32(&c.manifestTrust, 1); return "", nil }
	engineLanePrepareResolutionFn = func(string) (string, error) { atomic.AddInt32(&c.resolution, 1); return "", nil }
	engineLaneGenerateScaffoldFn = func(string) (string, error) { atomic.AddInt32(&c.scaffold, 1); return "manifest", nil }
	// Must return a NON-EMPTY app.dill path: the route legitimately returns early when the build
	// produces nothing, which would make the delegate boundary unreachable and its zero-call
	// assertion vacuous. The path need not exist; only filepath.Abs is applied to it.
	engineLaneBuildAppDillFn = func(string, string, []string) (string, error) {
		atomic.AddInt32(&c.build, 1)
		return filepath.Join(dir, "build", "app.dill"), nil
	}
	engineLaneDelegateFn = func(string, []string) error { atomic.AddInt32(&c.delegate, 1); return nil }
	engineLaneFreehandFn = func([]string, []string, string, string) error { atomic.AddInt32(&c.freehand, 1); return nil }
	t.Cleanup(func() {
		engineLaneEnsureManifestTrustFn, engineLanePrepareResolutionFn, engineLaneGenerateScaffoldFn = prevTrust, prevRes, prevScaffold
		engineLaneBuildAppDillFn, engineLaneDelegateFn, engineLaneFreehandFn = prevBuild, prevDelegate, prevFreehand
	})

	// Control-plane access. SCOPE, stated honestly: the boundaries that would issue a real HTTP call
	// (delegate, freehand) are stubbed here, so this counter cannot be shown non-zero by any test in
	// this file and is NOT a proven-reachable boundary like the six above. What it does prove is that
	// the code which is NOT stubbed -- argument parsing, the guards, project-shape inspection, the
	// branch decision -- reaches no network at all before refusing, and it is a live tripwire if
	// anything later adds a call there.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&c.httpRequests, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return &engineLaneProject{dir: dir, counters: c, apiURL: server.URL}
}

// snapshot records every path under the project so any created or modified file is detectable.
func (p *engineLaneProject) snapshot(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(p.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(p.dir, path)
		out[rel] = string(b)
		return nil
	})
	return out
}

func (p *engineLaneProject) assertUntouched(t *testing.T, before map[string]string) {
	t.Helper()
	after := p.snapshot(t)
	for path, content := range after {
		prev, existed := before[path]
		if !existed {
			t.Errorf("refusal CREATED %s; it must land before any project mutation", path)
			continue
		}
		if prev != content {
			t.Errorf("refusal MODIFIED %s; it must land before any project mutation", path)
		}
	}
	for path := range before {
		if _, still := after[path]; !still {
			t.Errorf("refusal REMOVED %s", path)
		}
	}
	// The specific artefacts this route would produce, named so a failure says which stage ran.
	for _, forbidden := range []string{
		".soroq/manifest_signing_key.seed",        // project key creation
		".dart_tool/package_config.json",          // dependency-resolution mutation
		"lib/soroq_patch_table.g.dart",            // generated scaffold
		"lib/soroq_activator.dart",                // generated scaffold
		".soroq/generated/soroq_bootstrap.g.dart", // freehand bootstrap
	} {
		if _, err := os.Stat(filepath.Join(p.dir, forbidden)); err == nil {
			t.Errorf("refusal produced %s", forbidden)
		}
	}
	if entries, _ := filepath.Glob(filepath.Join(p.dir, ".soroq", "releases", "*")); len(entries) != 0 {
		t.Errorf("refusal persisted release artifacts: %v", entries)
	}

	// Boundaries the filesystem cannot see.
	for name, got := range map[string]int32{
		"manifest/key mutation":  atomic.LoadInt32(&p.counters.manifestTrust),
		"dependency preparation": atomic.LoadInt32(&p.counters.resolution),
		"generated wiring":       atomic.LoadInt32(&p.counters.scaffold),
		"build":                  atomic.LoadInt32(&p.counters.build),
		"delegate":               atomic.LoadInt32(&p.counters.delegate),
		"freehand route":         atomic.LoadInt32(&p.counters.freehand),
		"control-plane access":   atomic.LoadInt32(&p.counters.httpRequests), // tripwire; see scope note above
	} {
		if got != 0 {
			t.Errorf("refusal reached %s %d time(s); it must land before every side effect", name, got)
		}
	}
}

const engineOrdinaryPubspec = "name: example_app\nversion: 1.0.0+1\nflutter:\n  uses-material-design: true\n"
const engineModulePubspec = "name: example_module\nversion: 1.0.0+1\nflutter:\n  module:\n    androidX: true\n"

// runEngineLaneBuild drives the TOP-LEVEL router (`soroq release ios --engine --build`), so the test
// exercises the same dispatch a developer hits rather than an internal entry point.
func runEngineLaneBuild(project *engineLaneProject, extra ...string) error {
	args := append([]string{"ios", "--engine", "--build",
		"--project-dir", project.dir, "--toolchain", "tc-1", "--api", project.apiURL}, extra...)
	return runRelease(args)
}

// engineLaneRouteConfigs: a refusal must hold on BOTH branches of the route.
//
// Running these only on the freehand config would be a subtler version of the vacuity the positive
// controls exist to catch: on a freehand project the route returns at the branch, so the five
// scaffolded counters are unreachable whether or not a guard fires, and asserting them zero there
// proves nothing. Driving both configs gives every counter a live assertion on the branch that can
// actually reach it.
var engineLaneRouteConfigs = map[string]string{
	"freehand":   engineFreehandConfig,
	"scaffolded": engineScaffoldedConfig,
}

func TestEngineLaneRefusesFlavorBeforeAnyProjectMutation(t *testing.T) {
	for route, cfg := range engineLaneRouteConfigs {
		t.Run(route, func(t *testing.T) {
			p := newEngineLaneProjectWithConfig(t, engineOrdinaryPubspec, cfg)
			before := p.snapshot(t)
			err := runEngineLaneBuild(p, "--", "--flavor", "prod")
			if err == nil || !strings.Contains(err.Error(), "no flavor support") {
				t.Fatalf("the hard-OTA route must refuse a flavored build, got: %v", err)
			}
			p.assertUntouched(t, before)
		})
	}
}

func TestEngineLaneRefusesObfuscationBeforeAnyProjectMutation(t *testing.T) {
	for route, cfg := range engineLaneRouteConfigs {
		t.Run(route, func(t *testing.T) {
			p := newEngineLaneProjectWithConfig(t, engineOrdinaryPubspec, cfg)
			before := p.snapshot(t)
			err := runEngineLaneBuild(p, "--", "--obfuscate", "--split-debug-info=build/sym")
			if err == nil || !strings.Contains(err.Error(), "not verified") {
				t.Fatalf("the hard-OTA route must refuse an obfuscated build, got: %v", err)
			}
			p.assertUntouched(t, before)
		})
	}
}

func TestEngineLaneRefusesAddToAppBeforeAnyProjectMutation(t *testing.T) {
	for route, cfg := range engineLaneRouteConfigs {
		t.Run(route, func(t *testing.T) {
			p := newEngineLaneProjectWithConfig(t, engineModulePubspec, cfg)
			before := p.snapshot(t)
			err := runEngineLaneBuild(p)
			if err == nil || !strings.Contains(err.Error(), "add-to-app") {
				t.Fatalf("the hard-OTA route must refuse an add-to-app project, got: %v", err)
			}
			p.assertUntouched(t, before)
		})
	}
}

// When a command is BOTH malformed and an unsupported shape, either refusal is acceptable -- what
// must hold is that neither path mutates the project.
//
// An earlier version of this test asserted the shape refusal had to win. That was an expectation I
// invented rather than a product requirement: the --app-dill check is a side-effect-free argument
// validation about whether the COMMAND is well-formed, and ordering it after a project inspection
// would be a UX preference, not a correctness fix. The property that actually matters is asserted
// instead.
func TestEngineLaneRefusesMalformedAndUnsupportedWithoutMutating(t *testing.T) {
	p := newEngineLaneProject(t, engineModulePubspec)
	before := p.snapshot(t)
	err := runReleaseIOSEngineBuild([]string{
		"--project-dir", p.dir, "--toolchain", "tc-1", "--app-dill", "/tmp/app.dill",
	})
	if err == nil {
		t.Fatal("a malformed, unsupported-shape command must be refused")
	}
	// Either refusal is fine; a silent success or a mutation is not.
	if !strings.Contains(err.Error(), "add-to-app") && !strings.Contains(err.Error(), "app.dill") {
		t.Fatalf("expected a shape or argument refusal, got: %v", err)
	}
	p.assertUntouched(t, before)
}

// A missing --toolchain is likewise refused without touching the project.
func TestEngineLaneMissingToolchainDoesNotMutate(t *testing.T) {
	p := newEngineLaneProject(t, engineOrdinaryPubspec)
	before := p.snapshot(t)
	if err := runReleaseIOSEngineBuild([]string{"--project-dir", p.dir}); err == nil {
		t.Fatal("--toolchain is required")
	}
	p.assertUntouched(t, before)
}

// POSITIVE CONTROLS for the counters above.
//
// Asserting "zero calls" proves nothing unless the same harness can be shown to produce NON-zero
// calls at EACH boundary. If the router refused every command for an unrelated reason -- an unknown
// flag, a missing file -- every counter would read zero and the refusal tests would pass while
// proving nothing.
//
// One run is not enough: the route splits, and the freehand branch RETURNS before the scaffolded
// boundaries exist. A single supported-shape run therefore leaves five of the six counters untouched
// and just as vacuous as before. Both branches are driven below, and each boundary is asserted by
// name rather than as a sum.

func TestEngineLaneFreehandBoundaryRegisters(t *testing.T) {
	p := newEngineLaneProjectWithConfig(t, engineOrdinaryPubspec, engineFreehandConfig)
	if err := runEngineLaneBuild(p); err != nil {
		t.Fatalf("a supported freehand shape must not be refused: %v", err)
	}
	if got := atomic.LoadInt32(&p.counters.freehand); got != 1 {
		t.Fatalf("freehand boundary registered %d times on a supported shape; the counter is not wired "+
			"to the route, so the zero-call assertions for it are vacuous", got)
	}
}

func TestEngineLaneScaffoldedBoundariesRegister(t *testing.T) {
	p := newEngineLaneProjectWithConfig(t, engineOrdinaryPubspec, engineScaffoldedConfig)
	// The scaffolded route may still fail further down on this stub fixture; what matters is that it
	// REACHED each boundary, because that is exactly what a refusal must prevent.
	_ = runEngineLaneBuild(p)
	if got := atomic.LoadInt32(&p.counters.freehand); got != 0 {
		t.Fatalf("a patchable list must not take the freehand branch (got %d)", got)
	}
	for name, got := range map[string]int32{
		"manifest/key mutation":  atomic.LoadInt32(&p.counters.manifestTrust),
		"dependency preparation": atomic.LoadInt32(&p.counters.resolution),
		"generated wiring":       atomic.LoadInt32(&p.counters.scaffold),
		"build":                  atomic.LoadInt32(&p.counters.build),
		"delegate":               atomic.LoadInt32(&p.counters.delegate),
	} {
		if got == 0 {
			t.Errorf("%s was never reached on a SUPPORTED shape; its zero-call assertion in the "+
				"refusal tests proves nothing", name)
		}
	}
}
