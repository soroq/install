package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmbeddedDynamicModulesMatchesSource asserts the embedded copy of lib/dynamic_modules.dart is
// byte-identical to the canonical packages/dynamic_modules source, so the CLI mirror can never drift.
func TestEmbeddedDynamicModulesMatchesSource(t *testing.T) {
	embedded, err := embeddedDynamicModulesLib()
	if err != nil {
		t.Fatal(err)
	}
	// cmd/soroq -> ../../../packages/dynamic_modules/lib/dynamic_modules.dart (repo layout).
	source := filepath.Join("..", "..", "..", "packages", "dynamic_modules", "lib", "dynamic_modules.dart")
	src, err := os.ReadFile(source)
	if err != nil {
		t.Skipf("canonical source not present at %s: %v", source, err)
	}
	if sha256.Sum256(embedded) != sha256.Sum256(src) {
		t.Fatalf("embedded dynamic_modules.dart (%s) drifted from source %s",
			hexSHA(embedded), hexSHA(src))
	}
}

func hexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestEnsurePubspecPathDependency_AddsPlainDependency asserts a plain `dependencies:` path entry is
// added (never under dependency_overrides).
func TestEnsurePubspecPathDependency_AddsPlainDependency(t *testing.T) {
	dir := t.TempDir()
	pubspec := filepath.Join(dir, "pubspec.yaml")
	original := `name: demo_app

dependencies:
  flutter:
    sdk: flutter
  cupertino_icons: ^1.0.0

dependency_overrides:
  meta: 1.9.0

flutter:
  uses-material-design: true
`
	if err := os.WriteFile(pubspec, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(dir, ".soroq", "dynamic_modules")

	changed, err := ensurePubspecPathDependency(pubspec, "dynamic_modules", installDir)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatalf("expected changed=true on first insert")
	}
	got, _ := os.ReadFile(pubspec)
	text := string(got)

	absInstall, _ := filepath.Abs(installDir)
	depBlock := text[strings.Index(text, "dependencies:"):strings.Index(text, "dependency_overrides:")]
	if !strings.Contains(depBlock, "dynamic_modules:") || !strings.Contains(depBlock, "path: "+absInstall) {
		t.Fatalf("plain dependency not added under dependencies:\n%s", text)
	}
	// It must NOT appear in dependency_overrides.
	overridesBlock := text[strings.Index(text, "dependency_overrides:"):]
	if strings.Contains(overridesBlock, "dynamic_modules") {
		t.Fatalf("dynamic_modules leaked into dependency_overrides:\n%s", text)
	}

	// Idempotent: a second call is a no-op.
	changed2, err := ensurePubspecPathDependency(pubspec, "dynamic_modules", installDir)
	if err != nil {
		t.Fatal(err)
	}
	if changed2 {
		t.Fatalf("expected changed=false on idempotent second call")
	}
}

// TestAssertManifestMatchesBaseline_Mismatch verifies the patch-vs-base manifest guard fails clearly
// when the regenerated manifest's sha differs from the baseline record.
func TestAssertManifestMatchesBaseline_Mismatch(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "soroq_app_manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("package:demo_app/a.dart::::foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baselinePath := filepath.Join(dir, "baseline.json")
	baseline := map[string]any{
		"release_id":                "demo-v1",
		"patchable_manifest_sha256": "deadbeef00000000000000000000000000000000000000000000000000000000",
	}
	b, _ := json.Marshal(baseline)
	if err := os.WriteFile(baselinePath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	err := assertManifestMatchesBaseline(manifestPath, baselinePath)
	if err == nil {
		t.Fatalf("expected mismatch error")
	}
	for _, want := range []string{"patchable set changed", "demo-v1", "new base release is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestAssertManifestMatchesBaseline_Match verifies a matching manifest passes.
func TestAssertManifestMatchesBaseline_Match(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "soroq_app_manifest.txt")
	content := []byte("package:demo_app/a.dart::::foo\n")
	if err := os.WriteFile(manifestPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	baselinePath := filepath.Join(dir, "baseline.json")
	baseline := map[string]any{
		"release_id":                "demo-v1",
		"patchable_manifest_sha256": hex.EncodeToString(sum[:]),
	}
	b, _ := json.Marshal(baseline)
	if err := os.WriteFile(baselinePath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := assertManifestMatchesBaseline(manifestPath, baselinePath); err != nil {
		t.Fatalf("expected match, got %v", err)
	}
}

// TestEnsureDynamicModulesInstalled_ExtractsAndWires drives the full installer with a stubbed pub get.
func TestEnsureDynamicModulesInstalled_ExtractsAndWires(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "pubspec.yaml"),
		[]byte("name: demo_app\n\ndependencies:\n  flutter:\n    sdk: flutter\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pubGetCalls := 0
	orig := runFlutterPubGet
	runFlutterPubGet = func(dir string) error { pubGetCalls++; return nil }
	defer func() { runFlutterPubGet = orig }()

	installDir, err := ensureDynamicModulesInstalled(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "lib", "dynamic_modules.dart")); err != nil {
		t.Fatalf("extracted lib missing: %v", err)
	}
	pubspec, _ := os.ReadFile(filepath.Join(installDir, "pubspec.yaml"))
	if strings.Contains(string(pubspec), "resolution: workspace") {
		t.Fatalf("extracted pubspec should be sanitized (no workspace resolution)")
	}
	if pubGetCalls != 1 {
		t.Fatalf("expected 1 pub get on first install, got %d", pubGetCalls)
	}

	// Idempotent second run: version stamp matches, pubspec already wired -> no pub get.
	if _, err := ensureDynamicModulesInstalled(project); err != nil {
		t.Fatal(err)
	}
	if pubGetCalls != 1 {
		t.Fatalf("expected no additional pub get on idempotent run, got %d", pubGetCalls)
	}
}
