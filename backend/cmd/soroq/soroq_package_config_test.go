package main

// THE CUSTOMER'S PROJECT FILES ARE NOT SOROQ'S TO EDIT.
//
// The previous installer wrote `dynamic_modules: {path: /Users/<dev>/.soroq/dynamic_modules}` into the
// customer's pubspec.yaml and let a frontend `pub get` rewrite pubspec.lock. Both are committed files,
// the path is machine-specific, and after the lock rewrite the developer's own Flutter could no longer
// resolve the project. These tests pin the replacement: resolution happens in a throwaway workspace and
// only build output is installed.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeResolvedProject writes a minimal project and stubs the workspace pub get so the test exercises
// the real copy/rewrite/install logic without invoking Flutter.
func fakeResolvedProject(t *testing.T) (projectDir string, restore func()) {
	t.Helper()
	projectDir = t.TempDir()
	mustWriteFile(t, filepath.Join(projectDir, "pubspec.yaml"),
		"name: demo_app\n\ndependencies:\n  flutter:\n    sdk: flutter\n")
	mustWriteFile(t, filepath.Join(projectDir, "pubspec.lock"), "# developer lock\npackages: {}\n")

	orig := runFlutterPubGetIn
	runFlutterPubGetIn = func(dir string) error {
		// Emulate pub: emit a package_config with a RELATIVE root package and an absolute foreign one.
		cfg := map[string]any{
			"configVersion": 2,
			"packages": []any{
				map[string]any{"name": "demo_app", "rootUri": "../", "packageUri": "lib/"},
				map[string]any{"name": "dynamic_modules", "rootUri": "file:///opt/dm", "packageUri": "lib/"},
				map[string]any{"name": "flutter", "rootUri": "../../frontend/packages/flutter", "packageUri": "lib/"},
			},
		}
		raw, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.MkdirAll(filepath.Join(dir, ".dart_tool"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".dart_tool", "package_config.json"), raw, 0o644)
	}
	return projectDir, func() { runFlutterPubGetIn = orig }
}

// THE HEADLINE PROPERTY: the customer's tracked files are byte-identical afterwards.
func TestResolutionLeavesPubspecAndLockByteIdentical(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir, restore := fakeResolvedProject(t)
	defer restore()

	before := map[string][]byte{}
	for _, name := range []string{"pubspec.yaml", "pubspec.lock"} {
		raw, err := os.ReadFile(filepath.Join(projectDir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = raw
	}

	if _, err := prepareSoroqBuildResolution(projectDir); err != nil {
		t.Fatalf("prepareSoroqBuildResolution: %v", err)
	}

	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(projectDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s was modified:\n--- before ---\n%s\n--- after ---\n%s", name, want, got)
		}
	}
	// And no override file was left behind.
	if _, err := os.Stat(filepath.Join(projectDir, "pubspec_overrides.yaml")); err == nil {
		t.Error("a pubspec_overrides.yaml was left in the project")
	}
}

// The package IS resolvable afterwards — the point of the exercise.
func TestInstalledPackageConfigResolvesDynamicModules(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir, restore := fakeResolvedProject(t)
	defer restore()

	if _, err := prepareSoroqBuildResolution(projectDir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(projectDir, ".dart_tool", "package_config.json"))
	if err != nil {
		t.Fatalf("no package_config.json installed: %v", err)
	}
	var cfg struct {
		Packages []struct {
			Name    string `json:"name"`
			RootURI string `json:"rootUri"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	roots := map[string]string{}
	for _, p := range cfg.Packages {
		roots[p.Name] = p.RootURI
	}
	if _, ok := roots["dynamic_modules"]; !ok {
		t.Fatal("dynamic_modules is not resolvable from the installed package config")
	}
	// THE SILENT-WRONG-BUILD GUARD: the root package must point at the real project, never the workspace.
	if want := "file://" + projectDir; roots["demo_app"] != want {
		t.Errorf("root package points at %q, want %q — the build would compile the wrong sources",
			roots["demo_app"], want)
	}
	// Every rootUri must be absolute; a relative one would resolve against the project's .dart_tool.
	for name, uri := range roots {
		if !strings.HasPrefix(uri, "file:///") {
			t.Errorf("package %q has a non-absolute rootUri %q", name, uri)
		}
	}
}

// NO TEMPORARY STATE SURVIVES, on success or failure. A workspace left behind would accumulate; a temp
// package_config left in .dart_tool would confuse the next build.
func TestWorkspaceAndTempFilesAreRemovedOnSuccessAndFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	countWorkspaces := func() int {
		entries, _ := filepath.Glob(filepath.Join(os.TempDir(), "soroq-resolve-*"))
		return len(entries)
	}
	baseline := countWorkspaces()

	// Success path.
	projectDir, restore := fakeResolvedProject(t)
	if _, err := prepareSoroqBuildResolution(projectDir); err != nil {
		t.Fatal(err)
	}
	restore()
	if got := countWorkspaces(); got != baseline {
		t.Errorf("success left %d workspace(s) behind", got-baseline)
	}

	// Failure path: resolution fails mid-way.
	failDir, restore2 := fakeResolvedProject(t)
	orig := runFlutterPubGetIn
	runFlutterPubGetIn = func(string) error { return errFakeResolveFailed }
	_, err := prepareSoroqBuildResolution(failDir)
	runFlutterPubGetIn = orig
	restore2()
	if err == nil {
		t.Fatal("a failing resolve reported success")
	}
	if got := countWorkspaces(); got != baseline {
		t.Errorf("failure left %d workspace(s) behind", got-baseline)
	}
	// A failed resolve must not leave a half-written config or temp file in the project.
	leftovers, _ := filepath.Glob(filepath.Join(failDir, ".dart_tool", "package_config.json.soroq-*"))
	if len(leftovers) != 0 {
		t.Errorf("failure left temp files behind: %v", leftovers)
	}
	// And it must not have touched the customer's files.
	raw, _ := os.ReadFile(filepath.Join(failDir, "pubspec.yaml"))
	if strings.Contains(string(raw), "dynamic_modules") {
		t.Error("a FAILED resolve still mutated the customer pubspec")
	}
}

var errFakeResolveFailed = &fakeErr{"isolated resolve failed"}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }

// INTERRUPTION: if the process dies after the workspace is created but before install, the project must
// be untouched. Simulated by failing immediately after resolution produced its output.
func TestInterruptionBeforeInstallLeavesTheProjectUntouched(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir, restore := fakeResolvedProject(t)
	defer restore()

	before, _ := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	lockBefore, _ := os.ReadFile(filepath.Join(projectDir, "pubspec.lock"))

	orig := runFlutterPubGetIn
	calls := 0
	runFlutterPubGetIn = func(dir string) error {
		calls++
		if calls == 1 {
			return nil // plugin injection in the project succeeds
		}
		// The WORKSPACE resolve produces output, then the process dies before install.
		if err := orig(dir); err != nil {
			return err
		}
		return errFakeResolveFailed
	}
	_, err := prepareSoroqBuildResolution(projectDir)
	runFlutterPubGetIn = orig
	if err == nil {
		t.Fatal("expected the interrupted resolve to fail")
	}

	after, _ := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	lockAfter, _ := os.ReadFile(filepath.Join(projectDir, "pubspec.lock"))
	if string(before) != string(after) || string(lockBefore) != string(lockAfter) {
		t.Error("an interrupted resolve modified the customer's project files")
	}
	// Plugin injection legitimately writes .dart_tool, so the assertion is not "no config exists" but
	// "the Soroq OVERLAY was not installed" — an interrupted resolve must not leave the build believing
	// dynamic_modules is resolvable when the resolution never completed.
	if raw, err := os.ReadFile(filepath.Join(projectDir, ".dart_tool", "package_config.json")); err == nil {
		if strings.Contains(string(raw), "dynamic_modules") {
			t.Error("an interrupted resolve installed the Soroq overlay anyway")
		}
	}
}

// TWO PARALLEL PROJECT BUILDS must not share or overwrite an overlay.
func TestParallelProjectBuildsDoNotShareAnOverlay(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	names := []string{"alpha_app", "beta_app"}
	dirs := make([]string, len(names))
	for i, name := range names {
		dirs[i] = t.TempDir()
		mustWriteFile(t, filepath.Join(dirs[i], "pubspec.yaml"),
			"name: "+name+"\n\ndependencies:\n  flutter:\n    sdk: flutter\n")
	}

	orig := runFlutterPubGetIn
	runFlutterPubGetIn = func(dir string) error {
		raw, err := os.ReadFile(filepath.Join(dir, "pubspec.yaml"))
		if err != nil {
			return err
		}
		name := strings.TrimSpace(parseTopLevelYaml(raw)["name"])
		cfg := map[string]any{
			"configVersion": 2,
			"packages": []any{
				map[string]any{"name": name, "rootUri": "../", "packageUri": "lib/"},
				map[string]any{"name": "dynamic_modules", "rootUri": "file:///opt/dm", "packageUri": "lib/"},
			},
		}
		out, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.MkdirAll(filepath.Join(dir, ".dart_tool"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".dart_tool", "package_config.json"), out, 0o644)
	}
	defer func() { runFlutterPubGetIn = orig }()

	var wg sync.WaitGroup
	errs := make([]error, len(dirs))
	for i := range dirs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = prepareSoroqBuildResolution(dirs[i])
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("%s failed: %v", names[i], err)
		}
	}
	// Each project must have ITS OWN root package, not the other's.
	for i, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, ".dart_tool", "package_config.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"`+names[i]+`"`) {
			t.Errorf("%s's config does not contain its own package", names[i])
		}
		other := names[(i+1)%len(names)]
		if strings.Contains(string(raw), `"`+other+`"`) {
			t.Errorf("%s's config leaked %s — the overlays were shared", names[i], other)
		}
	}
}

// A resolution whose output omits the project package must FAIL rather than install a config that would
// compile the wrong sources.
func TestMissingRootPackageFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	mustWriteFile(t, filepath.Join(projectDir, "pubspec.yaml"),
		"name: demo_app\n\ndependencies:\n  flutter:\n    sdk: flutter\n")

	orig := runFlutterPubGetIn
	runFlutterPubGetIn = func(dir string) error {
		cfg := `{"configVersion":2,"packages":[{"name":"dynamic_modules","rootUri":"file:///opt/dm"}]}`
		if err := os.MkdirAll(filepath.Join(dir, ".dart_tool"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".dart_tool", "package_config.json"), []byte(cfg), 0o644)
	}
	defer func() { runFlutterPubGetIn = orig }()

	if _, err := prepareSoroqBuildResolution(projectDir); err == nil {
		t.Fatal("a package config without the project package was accepted")
	} else if !strings.Contains(err.Error(), "demo_app") {
		t.Errorf("error does not name the missing package: %v", err)
	}
}

// Relative rootUri values must become absolute against the WORKSPACE's .dart_tool, not the project's.
func TestRelativeRootsResolveAgainstTheWorkspace(t *testing.T) {
	raw := []byte(`{"configVersion":2,"packages":[
		{"name":"demo_app","rootUri":"../"},
		{"name":"foo","rootUri":"../../cache/foo-1.0.0"}
	]}`)
	out, err := rewritePackageConfigRoots(raw, "demo_app", "/real/project", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Packages []struct{ Name, RootURI string } `json:"packages"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range cfg.Packages {
		switch p.Name {
		case "demo_app":
			if p.RootURI != "file:///real/project" {
				t.Errorf("root package = %q", p.RootURI)
			}
		case "foo":
			if p.RootURI != "file:///tmp/cache/foo-1.0.0" {
				t.Errorf("foo resolved to %q, want file:///tmp/cache/foo-1.0.0", p.RootURI)
			}
		}
	}
}

// CRASH SAFETY OF THE LOCK GUARD.
//
// Plugin injection must run `flutter pub get` in the project, which rewrites pubspec.lock. That is
// tolerable only because the rewrite is undone — and only if the undo survives the process dying
// mid-way. A plain save/restore does not: a crash between them loses the developer's lock permanently.
// The sidecar makes recovery automatic on the NEXT run, which is what these tests pin.

func TestLockIsRestoredAfterPluginInjection(t *testing.T) {
	projectDir := t.TempDir()
	const devLock = "# the developer's lock\npackages:\n  foo: 1.0.0\n"
	mustWriteFile(t, filepath.Join(projectDir, "pubspec.lock"), devLock)

	orig := runFlutterPubGetIn
	runFlutterPubGetIn = func(dir string) error {
		// pub rewrites the lock, exactly as the frontend SDK does.
		return os.WriteFile(filepath.Join(dir, "pubspec.lock"), []byte("# REWRITTEN by pub\n"), 0o644)
	}
	defer func() { runFlutterPubGetIn = orig }()

	if err := runPluginInjection(projectDir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(projectDir, "pubspec.lock"))
	if string(got) != devLock {
		t.Errorf("lock not restored:\n%s", got)
	}
	if _, err := os.Stat(lockGuardPath(projectDir)); !os.IsNotExist(err) {
		t.Error("the sidecar was left behind after a successful run")
	}
}

// THE CRASH: the process dies after pub rewrote the lock and before the restore. The next run must
// repair it automatically, with no developer action.
func TestAnInterruptedInjectionIsRepairedOnTheNextRun(t *testing.T) {
	projectDir := t.TempDir()
	const devLock = "# the developer's lock\npackages:\n  foo: 1.0.0\n"
	lockPath := filepath.Join(projectDir, "pubspec.lock")
	mustWriteFile(t, lockPath, devLock)

	// Simulate the crash: sidecar written, lock rewritten, restore never reached.
	guard := lockGuardPath(projectDir)
	if err := os.MkdirAll(filepath.Dir(guard), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, guard, devLock)
	mustWriteFile(t, lockPath, "# REWRITTEN by pub, process died here\n")

	if err := restoreInterruptedLock(projectDir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(lockPath)
	if string(got) != devLock {
		t.Errorf("an interrupted run was not repaired:\n%s", got)
	}
	if _, err := os.Stat(guard); !os.IsNotExist(err) {
		t.Error("the sidecar survived recovery and would fire again")
	}
}

// Recovery must be idempotent: a sidecar identical to the current lock is simply cleared.
func TestRecoveryIsIdempotent(t *testing.T) {
	projectDir := t.TempDir()
	const devLock = "# lock\n"
	mustWriteFile(t, filepath.Join(projectDir, "pubspec.lock"), devLock)
	guard := lockGuardPath(projectDir)
	if err := os.MkdirAll(filepath.Dir(guard), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, guard, devLock)

	for i := 0; i < 2; i++ {
		if err := restoreInterruptedLock(projectDir); err != nil {
			t.Fatalf("recovery %d: %v", i, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(projectDir, "pubspec.lock"))
	if string(got) != devLock {
		t.Errorf("repeated recovery corrupted the lock: %q", got)
	}
}

// A FAILING injection must still restore the lock — the failure is the build's, not the developer's.
func TestLockIsRestoredEvenWhenInjectionFails(t *testing.T) {
	projectDir := t.TempDir()
	const devLock = "# the developer's lock\n"
	mustWriteFile(t, filepath.Join(projectDir, "pubspec.lock"), devLock)

	orig := runFlutterPubGetIn
	runFlutterPubGetIn = func(dir string) error {
		_ = os.WriteFile(filepath.Join(dir, "pubspec.lock"), []byte("# half-written\n"), 0o644)
		return errFakeResolveFailed
	}
	defer func() { runFlutterPubGetIn = orig }()

	if err := runPluginInjection(projectDir); err == nil {
		t.Fatal("a failing injection reported success")
	}
	got, _ := os.ReadFile(filepath.Join(projectDir, "pubspec.lock"))
	if string(got) != devLock {
		t.Errorf("a failed injection left the lock rewritten:\n%s", got)
	}
}

// A project with NO lock must not gain one: pub creates it, and it is removed again.
func TestAProjectWithoutALockDoesNotGainOne(t *testing.T) {
	projectDir := t.TempDir()
	orig := runFlutterPubGetIn
	runFlutterPubGetIn = func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "pubspec.lock"), []byte("# created by pub\n"), 0o644)
	}
	defer func() { runFlutterPubGetIn = orig }()

	if err := runPluginInjection(projectDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "pubspec.lock")); !os.IsNotExist(err) {
		t.Error("Soroq left a pubspec.lock in a project that had none")
	}
}

// SAME-PROJECT CONCURRENCY. Two builds of one project share the lock sidecar and the installed
// package_config. Without serialisation they interleave destructively: B parks A's already-rewritten
// lock over A's saved original, then "restores" the rewritten bytes — the developer's lock is gone with
// no error anywhere. The per-build workspaces do NOT protect against this; only the project lock does.
func TestConcurrentBuildsOfTheSameProjectDoNotLoseTheLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	const devLock = "# the developer's lock\npackages:\n  foo: 1.0.0\n"
	mustWriteFile(t, filepath.Join(projectDir, "pubspec.yaml"),
		"name: demo_app\n\ndependencies:\n  flutter:\n    sdk: flutter\n")
	mustWriteFile(t, filepath.Join(projectDir, "pubspec.lock"), devLock)

	orig := runFlutterPubGetIn
	runFlutterPubGetIn = func(dir string) error {
		// Every pub get rewrites the lock in whatever directory it runs in, and takes long enough for a
		// racing run to interleave if nothing serialises them.
		_ = os.WriteFile(filepath.Join(dir, "pubspec.lock"), []byte("# REWRITTEN by pub\n"), 0o644)
		time.Sleep(5 * time.Millisecond)
		cfg := `{"configVersion":2,"packages":[` +
			`{"name":"demo_app","rootUri":"../","packageUri":"lib/"},` +
			`{"name":"dynamic_modules","rootUri":"file:///opt/dm","packageUri":"lib/"}]}`
		if err := os.MkdirAll(filepath.Join(dir, ".dart_tool"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".dart_tool", "package_config.json"), []byte(cfg), 0o644)
	}
	defer func() { runFlutterPubGetIn = orig }()

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = prepareSoroqBuildResolution(projectDir)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("build %d failed: %v", i, err)
		}
	}

	got, err := os.ReadFile(filepath.Join(projectDir, "pubspec.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != devLock {
		t.Errorf("concurrent same-project builds LOST the developer's lock:\n%s", got)
	}
	if _, err := os.Stat(lockGuardPath(projectDir)); !os.IsNotExist(err) {
		t.Error("a sidecar survived; the next run would restore stale bytes")
	}
	// The overlay must still be installed and complete.
	raw, err := os.ReadFile(filepath.Join(projectDir, ".dart_tool", "package_config.json"))
	if err != nil || !strings.Contains(string(raw), "dynamic_modules") {
		t.Errorf("the overlay is missing or incomplete after concurrent builds: %v", err)
	}
}

// A CHANGED INPUT MUST CHANGE THE ADDRESS. Cache reuse is only safe if every input that can alter the
// analysis is bound into the content address; an input that does not move it would silently serve a
// stale analysis for changed code.
func TestEveryContentAddressInputChangesTheAddress(t *testing.T) {
	base := freehandContentAddr("dill", "analyzer", "pkgconfig", freehandIdentitySchema, "config")
	variants := map[string]string{
		"app.dill":        freehandContentAddr("DILL2", "analyzer", "pkgconfig", freehandIdentitySchema, "config"),
		"analyzer":        freehandContentAddr("dill", "ANALYZER2", "pkgconfig", freehandIdentitySchema, "config"),
		"package_config":  freehandContentAddr("dill", "analyzer", "PKGCONFIG2", freehandIdentitySchema, "config"),
		"identity schema": freehandContentAddr("dill", "analyzer", "pkgconfig", freehandIdentitySchema+"-v2", "config"),
		"config digest":   freehandContentAddr("dill", "analyzer", "pkgconfig", freehandIdentitySchema, "CONFIG2"),
	}
	seen := map[string]string{base: "baseline"}
	for name, addr := range variants {
		if addr == base {
			t.Errorf("changing %s did NOT change the content address; a stale analysis would be reused", name)
		}
		if other, clash := seen[addr]; clash {
			t.Errorf("changing %s collides with %s (%s)", name, other, addr)
		}
		seen[addr] = name
	}
	// Determinism: identical inputs must always give the identical address, or nothing could ever hit.
	if again := freehandContentAddr("dill", "analyzer", "pkgconfig", freehandIdentitySchema, "config"); again != base {
		t.Errorf("the address is not deterministic: %s vs %s", base, again)
	}
}
