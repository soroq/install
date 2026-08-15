package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CUSTOM ENTRYPOINTS.
//
// An earlier "custom entrypoint" case created no alternate file and passed no target, so it proved
// only that an ordinary project builds. These create a real lib/alternate.dart and drive both flag
// forms, then exercise every rejection the wrapper depends on.
//
// Why the rejections matter: the generated bootstrap imports the entrypoint by package: URI and calls
// `app.main()` with no arguments. An entrypoint outside lib/ has no package: URI, and a main() taking
// a required argument cannot be invoked that way -- so each of these would fail deep in the Dart front
// end with a message pointing at generated code the developer never wrote.

func entrypointProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: example_app\nversion: 1.0.0+1\n")
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatalf("mkdir lib: %v", err)
	}
	writeFile(t, filepath.Join(dir, "lib", "main.dart"), "void main() { runApp(); }\n")
	writeFile(t, filepath.Join(dir, "lib", "alternate.dart"), "void main() { runApp(); }\n")
	return dir
}

func TestOrdinaryEntrypointResolvesToItsPackageURI(t *testing.T) {
	dir := entrypointProject(t)
	got, err := resolveFreehandEntrypointImport(dir, "example_app", "")
	if err != nil {
		t.Fatalf("the default entrypoint must resolve: %v", err)
	}
	if got != "package:example_app/main.dart" {
		t.Fatalf("got %q, want package:example_app/main.dart", got)
	}
}

// A REAL custom entrypoint, through both flag spellings the developer may use.
func TestCustomEntrypointResolvesThroughBothFlagForms(t *testing.T) {
	dir := entrypointProject(t)
	for _, args := range [][]string{
		{"-t", "lib/alternate.dart"},
		{"--target", "lib/alternate.dart"},
		{"-t=lib/alternate.dart"},
		{"--target=lib/alternate.dart"},
	} {
		target, _ := flagValue(args, "t")
		if strings.TrimSpace(target) == "" {
			if v, _ := flagValue(args, "target"); strings.TrimSpace(v) != "" {
				target = v
			}
		}
		if target != "lib/alternate.dart" {
			t.Fatalf("args %v: parsed target %q", args, target)
		}
		got, err := resolveFreehandEntrypointImport(dir, "example_app", target)
		if err != nil {
			t.Fatalf("args %v: custom entrypoint must resolve: %v", args, err)
		}
		if got != "package:example_app/alternate.dart" {
			t.Fatalf("args %v: got %q", args, got)
		}
	}
}

// The generated bootstrap must import the SELECTED entrypoint, not the default. Getting this wrong
// would silently wrap a different main() than the developer chose.
func TestGeneratedBootstrapImportsTheSelectedEntrypoint(t *testing.T) {
	src := freehandBootstrapSource(freehandBootstrapConfig{
		AppID: "com.example.app", RuntimeID: "runtime-1", Channel: "stable",
		ControlPlaneBaseURL: "https://api.example.test", PinnedEnginePubKeyHex: strings.Repeat("a", 64),
		EntrypointImport: "package:example_app/alternate.dart",
	})
	if !strings.Contains(src, "import 'package:example_app/alternate.dart' as app;") {
		t.Fatal("the bootstrap must import the selected custom entrypoint")
	}
	if strings.Contains(src, "package:example_app/main.dart") {
		t.Fatal("the bootstrap must not fall back to the default entrypoint")
	}
}

func TestUnsupportedEntrypointsAreRefused(t *testing.T) {
	dir := entrypointProject(t)
	writeFile(t, filepath.Join(dir, "lib", "args_main.dart"), "void main(List<String> args) {}\n")
	writeFile(t, filepath.Join(dir, "lib", "no_main.dart"), "int helper() => 1;\n")
	writeFile(t, filepath.Join(dir, "outside.dart"), "void main() {}\n")
	writeFile(t, filepath.Join(dir, "lib", "notdart.txt"), "void main() {}\n")

	for name, tc := range map[string]struct{ target, wantSubstring string }{
		"missing file":      {"lib/does_not_exist.dart", "read entrypoint"},
		"outside lib":       {"outside.dart", "under lib/"},
		"absolute path":     {"/etc/passwd.dart", "project-relative"},
		"traversal":         {"lib/../../escape.dart", "project-relative"},
		"non-dart":          {"lib/notdart.txt", ".dart file"},
		"required arg main": {"lib/args_main.dart", "required argument"},
		"no main at all":    {"lib/no_main.dart", "declares no top-level main"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveFreehandEntrypointImport(dir, "example_app", tc.target)
			if err == nil {
				t.Fatalf("target %q must be refused", tc.target)
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Errorf("refusal should explain %q, got: %v", tc.wantSubstring, err)
			}
		})
	}
}

// All-optional parameters are legitimately wrappable and must NOT be refused.
func TestOptionalParameterMainsAreAccepted(t *testing.T) {
	dir := entrypointProject(t)
	writeFile(t, filepath.Join(dir, "lib", "opt_positional.dart"), "void main([List<String>? a]) {}\n")
	writeFile(t, filepath.Join(dir, "lib", "opt_named.dart"), "void main({List<String>? a}) {}\n")
	for _, target := range []string{"lib/opt_positional.dart", "lib/opt_named.dart"} {
		if _, err := resolveFreehandEntrypointImport(dir, "example_app", target); err != nil {
			t.Errorf("%s has an all-optional main and must be accepted: %v", target, err)
		}
	}
}

// A refused entrypoint must not leave generated files behind.
func TestRefusedEntrypointWritesNothing(t *testing.T) {
	dir := entrypointProject(t)
	before, _ := filepath.Glob(filepath.Join(dir, ".soroq", "generated", "*"))
	if _, err := resolveFreehandEntrypointImport(dir, "example_app", "outside.dart"); err == nil {
		t.Fatal("expected refusal")
	}
	after, _ := filepath.Glob(filepath.Join(dir, ".soroq", "generated", "*"))
	if len(after) != len(before) {
		t.Fatalf("a refused entrypoint generated files: %v", after)
	}
}

// ---------------------------------------------------------------------------
// COMMAND LEVEL: the developer's -t must survive the top-level router.
//
// Everything above tests the resolver and the generator directly. That leaves the
// part most likely to break untested: argv. `soroq release ios --engine --build -- -t lib/alternate.dart`
// has to parse the head, split on `--`, and hand the developer's flags to the freehand path
// unmangled. A resolver test cannot fail if the router drops, reorders or swallows `-t`.
//
// BOUNDARY, stated plainly: these drive runRelease -> the real router -> the freehand boundary, and
// run the REAL resolver and the REAL entrypoint rewrite on the passthrough the router actually
// produced. They do NOT run the Flutter build, which needs an installed frontend and a device
// toolchain. So this proves argv delivery and entrypoint selection through the command, not that the
// resulting app.dill compiled.

// entrypointRouterProject is a freehand-shaped project (no patchable list) with a real alternate entrypoint.
func entrypointRouterProject(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: example_app\nversion: 1.0.0+1\n")
	writeFile(t, filepath.Join(dir, "soroq.yaml"), engineFreehandConfig)
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatalf("mkdir lib: %v", err)
	}
	writeFile(t, filepath.Join(dir, "lib", "main.dart"), "void main() { runApp(); }\n")
	writeFile(t, filepath.Join(dir, "lib", "alternate.dart"), "void main() { runApp(); }\n")
	return dir
}

// captureRouterEntrypoint runs the command and returns what the freehand boundary received, plus the
// entrypoint the real resolver selected from it and the real rewritten build passthrough.
func captureRouterEntrypoint(t *testing.T, dir string, developerArgs ...string) (delivered, rewritten []string, importURI string, err error) {
	t.Helper()
	prev := engineLaneFreehandFn
	t.Cleanup(func() { engineLaneFreehandFn = prev })
	engineLaneFreehandFn = func(_, passthrough []string, projectDir, _ string) error {
		delivered = append([]string(nil), passthrough...)
		target, _ := flagValue(passthrough, "t")
		if strings.TrimSpace(target) == "" {
			if v, _ := flagValue(passthrough, "target"); strings.TrimSpace(v) != "" {
				target = v
			}
		}
		uri, rerr := resolveFreehandEntrypointImport(projectDir, "example_app", target)
		if rerr != nil {
			return rerr
		}
		importURI = uri
		rewritten = withFreehandBootstrapEntrypoint(freehandBootstrapRelPath, passthrough)
		return nil
	}
	args := append([]string{"ios", "--engine", "--build", "--project-dir", dir, "--toolchain", "tc-1", "--"}, developerArgs...)
	err = runRelease(args)
	return delivered, rewritten, importURI, err
}

// Every spelling a developer may reasonably use, through the actual command.
func TestRouterCarriesCustomEntrypointThroughEverySupportedSpelling(t *testing.T) {
	for _, spelling := range [][]string{
		{"-t", "lib/alternate.dart"},
		{"--target", "lib/alternate.dart"},
		{"-t=lib/alternate.dart"},
		{"--target=lib/alternate.dart"},
	} {
		t.Run(strings.Join(spelling, " "), func(t *testing.T) {
			dir := entrypointRouterProject(t)
			delivered, rewritten, importURI, err := captureRouterEntrypoint(t, dir, spelling...)
			if err != nil {
				t.Fatalf("the command must accept %v: %v", spelling, err)
			}
			// The developer's tokens must arrive VERBATIM. Checking only the resolved import would let a
			// router that rewrote `--target=X` into `-t X` (or reordered tokens) pass, even though the
			// freehand path's own flag precedence would then be reading different input than the
			// developer typed.
			if strings.Join(delivered, " ") != strings.Join(spelling, " ") {
				t.Fatalf("the router altered the developer's flags: sent %v, delivered %v", spelling, delivered)
			}
			if importURI != "package:example_app/alternate.dart" {
				t.Fatalf("the command selected %q; the developer asked for alternate.dart", importURI)
			}
			// The default must not be silently substituted -- that would wrap a different main().
			if strings.Contains(importURI, "main.dart") {
				t.Fatalf("the command fell back to the default entrypoint: %q", importURI)
			}
			// Soroq's bootstrap must be the build entrypoint, and the developer's -t must be gone:
			// two -t flags would leave the winner up to Flutter's arg parser.
			if len(rewritten) < 2 || rewritten[0] != "-t" || rewritten[1] != freehandBootstrapRelPath {
				t.Fatalf("the bootstrap must be the build entrypoint, got %v", rewritten)
			}
			for i, a := range rewritten[2:] {
				if a == "-t" || a == "--target" || strings.HasPrefix(a, "-t=") || strings.HasPrefix(a, "--target=") {
					t.Fatalf("the developer's own entrypoint flag survived at index %d: %v", i+2, rewritten)
				}
			}
		})
	}
}

// With no -t at all the command must select the default, not fail.
func TestRouterSelectsDefaultEntrypointWhenNoTargetGiven(t *testing.T) {
	dir := entrypointRouterProject(t)
	_, _, importURI, err := captureRouterEntrypoint(t, dir)
	if err != nil {
		t.Fatalf("an ordinary build must not be refused: %v", err)
	}
	if importURI != "package:example_app/main.dart" {
		t.Fatalf("default entrypoint should be main.dart, got %q", importURI)
	}
}

// An unsupported entrypoint must be refused THROUGH THE COMMAND, and generate nothing.
func TestRouterRefusesUnsupportedEntrypointAndGeneratesNothing(t *testing.T) {
	for name, tc := range map[string]struct{ target, want string }{
		"outside lib": {"outside.dart", "under lib/"},
		"missing":     {"lib/nope.dart", "read entrypoint"},
		"traversal":   {"lib/../../escape.dart", "project-relative"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := entrypointRouterProject(t)
			writeFile(t, filepath.Join(dir, "outside.dart"), "void main() {}\n")
			_, _, _, err := captureRouterEntrypoint(t, dir, "-t", tc.target)
			if err == nil {
				t.Fatalf("the command must refuse target %q", tc.target)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal should explain %q, got: %v", tc.want, err)
			}
			if got, _ := filepath.Glob(filepath.Join(dir, ".soroq", "generated", "*")); len(got) != 0 {
				t.Errorf("a refused entrypoint generated files: %v", got)
			}
		})
	}
}
