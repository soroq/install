package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlutterModuleProjectsAreDetected(t *testing.T) {
	module := []byte("name: my_module\nflutter:\n  module:\n    androidX: true\n")
	if !flutterProjectIsModule(module) {
		t.Error("a pubspec declaring module: under flutter: is an add-to-app project")
	}
	full := []byte("name: my_app\nflutter:\n  uses-material-design: true\n  assets:\n    - assets/\n")
	if flutterProjectIsModule(full) {
		t.Error("a standard application must not be mistaken for a module")
	}
	// `module:` under some OTHER top-level key is not add-to-app.
	elsewhere := []byte("name: my_app\ndependencies:\n  module: ^1.0.0\nflutter:\n  uses-material-design: true\n")
	if flutterProjectIsModule(elsewhere) {
		t.Error("a dependency named module must not trip add-to-app detection")
	}
}

func TestAddToAppProjectIsRefusedBeforeRelease(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pubspec.yaml"),
		[]byte("name: my_module\nflutter:\n  module:\n    androidX: true\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := guardSupportedApplicationShape(dir)
	if err == nil {
		t.Fatal("an add-to-app project must be refused, not released against an identity its host does not share")
	}
	for _, want := range []string{"add-to-app", "does not support", "standard full Flutter application"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q:\n%s", want, err)
		}
	}
}

func TestStandardApplicationIsUnaffected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pubspec.yaml"),
		[]byte("name: my_app\nflutter:\n  uses-material-design: true\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := guardSupportedApplicationShape(dir); err != nil {
		t.Fatalf("a standard Flutter application must be unaffected: %v", err)
	}
}

func TestAddToAppOptInIsHonoured(t *testing.T) {
	t.Setenv("SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS", "1")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pubspec.yaml"),
		[]byte("name: my_module\nflutter:\n  module: {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := guardSupportedApplicationShape(dir); err != nil {
		t.Fatalf("the documented opt-in must allow it: %v", err)
	}
}
