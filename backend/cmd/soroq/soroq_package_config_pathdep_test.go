package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RELATIVE PATH DEPENDENCIES IN THE ISOLATED WORKSPACE.
//
// The workspace is a temp directory and the pubspec is copied into it verbatim, so
// `path: ../../packages/soroq_flutter` resolved against the TEMP dir and pub refused with
// "could not find package soroq_flutter at \"../../packages/soroq_flutter\"" — naming a path that
// resolves perfectly well in the developer's own tree. Every monorepo and every locally-developed
// plugin hits this.
func TestRelativePathDependenciesBecomeAbsolute(t *testing.T) {
	t.Parallel()

	in := strings.Join([]string{
		"name: demo",
		"dependencies:",
		"  flutter:",
		"    sdk: flutter",
		"  soroq_flutter:",
		"    path: ../../packages/soroq_flutter",
		"dev_dependencies:",
		"  helper:",
		`    path: "../helper"`,
	}, "\n")

	out := pubspecWithAbsolutePathDependencies(in, "/repo/examples/app")

	want := filepath.Clean("/repo/packages/soroq_flutter")
	if !strings.Contains(out, "path: "+want) {
		t.Fatalf("dependency path not made absolute:\n%s", out)
	}
	// Quoted forms are legal YAML and must be handled too.
	if !strings.Contains(out, "path: "+filepath.Clean("/repo/examples/helper")) {
		t.Fatalf("quoted dev_dependency path not made absolute:\n%s", out)
	}
	if strings.Contains(out, "../") {
		t.Fatalf("a relative segment survived:\n%s", out)
	}
}

// An ALREADY-absolute path must be left exactly alone — rewriting it would corrupt it by joining it
// onto the project directory.
func TestAbsolutePathDependenciesAreUntouched(t *testing.T) {
	t.Parallel()

	in := "dependencies:\n  pkg:\n    path: /opt/shared/pkg\n"
	out := pubspecWithAbsolutePathDependencies(in, "/repo/app")
	if !strings.Contains(out, "path: /opt/shared/pkg") {
		t.Fatalf("absolute path was rewritten:\n%s", out)
	}
}

// Indentation is structural in YAML: mangling it would silently move a dependency into another block.
func TestPathRewritePreservesIndentation(t *testing.T) {
	t.Parallel()

	in := "dependencies:\n  pkg:\n    path: ../sibling\n"
	out := pubspecWithAbsolutePathDependencies(in, "/repo/app")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "path:") && !strings.HasPrefix(line, "    ") {
			t.Fatalf("indentation changed: %q", line)
		}
	}
}

// A pubspec with no path dependencies at all must come back byte-identical, so this cannot perturb
// the ordinary pub.dev case.
func TestPubspecWithoutPathDependenciesIsUnchanged(t *testing.T) {
	t.Parallel()

	in := "name: demo\ndependencies:\n  http: ^1.0.0\n  flutter:\n    sdk: flutter\n"
	if out := pubspecWithAbsolutePathDependencies(in, "/repo/app"); out != in {
		t.Fatalf("unchanged pubspec was modified:\n%s", out)
	}
}

// THE GUARANTEE THE REWRITE MUST NOT BREAK.
//
// pubspecWithAbsolutePathDependencies exists to mutate the WORKSPACE copy. If it ever ran against
// the developer's own file the damage would be silent and permanent: their pubspec would acquire
// absolute paths from this machine and stop resolving on anyone else's.
//
// The generic byte-identity test elsewhere uses a project with no path dependency, so it cannot
// catch a regression in this specific code path. This one carries a relative path dependency —
// exactly the input the rewrite acts on.
func TestPathDependencyProjectPubspecIsNeverModified(t *testing.T) {
	dir := t.TempDir()
	pubspec := strings.Join([]string{
		"name: demo",
		"environment:",
		"  sdk: '>=3.0.0 <4.0.0'",
		"dependencies:",
		"  soroq_flutter:",
		"    path: ../../packages/soroq_flutter",
		"",
	}, "\n")
	path := filepath.Join(dir, "pubspec.yaml")
	if err := os.WriteFile(path, []byte(pubspec), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The rewrite returns a NEW string; it must not touch the file it was derived from.
	out := pubspecWithAbsolutePathDependencies(string(before), dir)
	if out == string(before) {
		t.Fatal("the rewrite did nothing on a relative path dependency")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the developer's pubspec.yaml was modified:\n--- before ---\n%s\n--- after ---\n%s",
			before, after)
	}
}

// THE PACKAGE NAMED `path` IS NOT A PATH DEPENDENCY.
//
// This is a real defect found by building a shipping app: `path` is one of the most widely used
// packages on pub.dev, and declaring it the ordinary way produced
//
//	Error on line 40, column 9 of pubspec.yaml: Invalid version constraint:
//	Could not parse version "/Users/shrey/calorie_tracker_tf/^1.9.1".
//
// because the rewriter matched on the line PREFIX `path:` and could not tell a dependency NAME from a
// `path:` FIELD nested under one. Any app depending on `path` could not build through the CLI at all.
//
// Indentation is what separates the two cases: a dependency name sits directly under `dependencies:`,
// while a `path:` field sits one level deeper, under the dependency it belongs to.
func TestPackageNamedPathIsNotRewritten(t *testing.T) {
	in := `name: app
dependencies:
  flutter:
    sdk: flutter
  path: ^1.9.1
  soroq_flutter:
    path: ../packages/soroq_flutter
`
	out := pubspecWithAbsolutePathDependencies(in, "/repo/app")
	if !strings.Contains(out, "  path: ^1.9.1") {
		t.Errorf("the pub package named `path` was rewritten; got:\n%s", out)
	}
	// The genuine path dependency alongside it must still be made absolute.
	if !strings.Contains(out, "path: /repo/packages/soroq_flutter") {
		t.Errorf("the real path dependency was not absolutised; got:\n%s", out)
	}
}

// Version constraints take several shapes, and none of them is a filesystem path.
func TestVersionConstraintsForPackageNamedPathSurvive(t *testing.T) {
	for _, constraint := range []string{"^1.9.1", "any", ">=1.0.0 <2.0.0", "1.9.1"} {
		in := "name: app\ndependencies:\n  path: " + constraint + "\n"
		out := pubspecWithAbsolutePathDependencies(in, "/repo/app")
		if out != in {
			t.Errorf("constraint %q was rewritten:\n%s", constraint, out)
		}
	}
}

// A dev_dependencies entry named `path` is the same case one block down.
func TestPackageNamedPathInDevDependencies(t *testing.T) {
	in := "name: app\ndev_dependencies:\n  path: ^1.9.1\n"
	if out := pubspecWithAbsolutePathDependencies(in, "/repo/app"); out != in {
		t.Errorf("dev_dependencies `path` was rewritten:\n%s", out)
	}
}
