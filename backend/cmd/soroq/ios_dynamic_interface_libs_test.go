package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A project whose dependencies cannot be resolved must REFUSE the build, not produce a base that
// silently cannot be patched.
//
// This is not a hypothetical. On a physical iPhone, a project missing pubspec.lock built fine, the
// patch published fine, and every device then quarantined it while reporting
// `fetch=- sig=- hash=- err=null` -- indistinguishable from no patch existing. The old code swallowed
// the resolve failure on the reasoning that a narrower contract "can only refuse more". It refuses
// more, silently, forever, with no diagnosable signal. That is worse than a build error.
func TestUnresolvableProjectRefusesRatherThanShippingAnUnpatchableBase(t *testing.T) {
	dir := t.TempDir()
	// A pubspec with no lock file and no .dart_tool: exactly what `flutter pub get` not being run
	// looks like, which is the mundane cause this refusal exists for.
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: unresolvable_app\nversion: 1.0.0+1\n")

	_, _, err := contractProjectLibraries(dir)
	if err == nil {
		t.Fatal("an unresolvable project must refuse: a base built without its own libraries in the " +
			"retention contract publishes patches that every device silently quarantines")
	}
	// The message has to be actionable, or the refusal just moves the mystery earlier.
	for _, want := range []string{"pub get", "quarantine", "retention contract"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q so a developer knows what to do; got: %v", want, err)
		}
	}
}

// POSITIVE CONTROL: a resolvable project still succeeds, so the refusal above is attributable to the
// resolve failure and not to the function having become unconditionally broken.
func TestResolvableProjectStillProducesContractLibraries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: ok_app\nversion: 1.0.0+1\n")
	writeFile(t, filepath.Join(dir, "pubspec.lock"), "packages: {}\nsdks:\n  dart: \">=3.0.0 <4.0.0\"\n")
	if err := os.MkdirAll(filepath.Join(dir, ".dart_tool"), 0o755); err != nil {
		t.Fatalf("mkdir .dart_tool: %v", err)
	}
	writeFile(t, filepath.Join(dir, ".dart_tool", "package_config.json"),
		`{"configVersion":2,"packages":[{"name":"ok_app","rootUri":"../","packageUri":"lib/","languageVersion":"3.0"}]}`)
	if _, _, err := contractProjectLibraries(dir); err != nil {
		t.Fatalf("a resolvable project must not be refused: %v", err)
	}
}

// Only libraries the entrypoint reaches may be in the contract, and never a `part of` file. Naming any
// other library crashed the front end on the Campus app ("The library ... has not been indexed").
func TestContractKeepsOnlyLibrariesReachableFromMain(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: reach_app\nversion: 1.0.0+1\n")
	writeFile(t, filepath.Join(dir, "pubspec.lock"), "packages: {}\nsdks:\n  dart: \">=3.0.0 <4.0.0\"\n")
	if err := os.MkdirAll(filepath.Join(dir, ".dart_tool"), 0o755); err != nil {
		t.Fatalf("mkdir .dart_tool: %v", err)
	}
	writeFile(t, filepath.Join(dir, ".dart_tool", "package_config.json"),
		`{"configVersion":2,"packages":[{"name":"reach_app","rootUri":"../","packageUri":"lib/","languageVersion":"3.0"}]}`)
	writeDart(t, dir, "lib/main.dart", "import 'screens/home.dart';\nvoid main() {}\n")
	writeDart(t, dir, "lib/screens/home.dart", "export 'widgets.dart';\npart 'home.g.dart';\n")
	writeDart(t, dir, "lib/screens/widgets.dart", "class W {}\n")
	writeDart(t, dir, "lib/screens/home.g.dart", "// generated\npart of 'home.dart';\n")
	writeDart(t, dir, "lib/dead_code.dart", "class Dead {}\n")

	appLibs, _, err := contractProjectLibraries(dir)
	if err != nil {
		t.Fatalf("contractProjectLibraries: %v", err)
	}
	got := map[string]bool{}
	for _, l := range appLibs {
		got[l] = true
	}
	for _, want := range []string{"package:reach_app/main.dart", "package:reach_app/screens/home.dart", "package:reach_app/screens/widgets.dart"} {
		if !got[want] {
			t.Errorf("%s is reached from main.dart but missing from the contract (got %v)", want, appLibs)
		}
	}
	for _, bad := range []string{"package:reach_app/dead_code.dart", "package:reach_app/screens/home.g.dart"} {
		if got[bad] {
			t.Errorf("%s is in the contract; it is not a library the app compiles", bad)
		}
	}
}
