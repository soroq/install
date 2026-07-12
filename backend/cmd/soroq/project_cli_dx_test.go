package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// snapshotDir returns a map of project-relative path -> sha256 of every regular file under dir. It lets
// a test assert that a read-only path (--dry-run / --check) or a clean no-op wrote nothing.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		bytes, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(bytes)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotDir(%s) error = %v", dir, err)
	}
	return out
}

func assertSameSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("directory changed: %d files before, %d after\nbefore=%v\nafter=%v", len(before), len(after), keysOf(before), keysOf(after))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Fatalf("file %q changed (or vanished): before=%s after=%s", path, sum, after[path])
		}
	}
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writeMinimalAndroidProject(t *testing.T, projectDir, appID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(projectDir, "android", "app"), 0o755); err != nil {
		t.Fatalf("MkdirAll(android/app) error = %v", err)
	}
	writeFile(t, filepath.Join(projectDir, "android", "app", "build.gradle.kts"), `android {
    namespace = "`+appID+`"
    defaultConfig {
        applicationId = "`+appID+`"
    }
}
`)
}

// A fresh project: --dry-run must write NOTHING and exit 0, printing a plan.
func TestRunInitDryRunWritesNothing(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\ndependencies:\n  soroq_flutter: any\n")
	writeMinimalAndroidProject(t, projectDir, "com.example.demo")

	before := snapshotDir(t, projectDir)
	stdout := captureStdout(t, func() {
		if err := runInit([]string{"--project-dir", projectDir, "--dry-run"}); err != nil {
			t.Fatalf("runInit(--dry-run) error = %v", err)
		}
	})
	after := snapshotDir(t, projectDir)

	assertSameSnapshot(t, before, after)
	if _, err := os.Stat(filepath.Join(projectDir, "soroq.yaml")); !os.IsNotExist(err) {
		t.Fatalf("soroq.yaml must NOT exist after --dry-run; stat err = %v", err)
	}
	for _, want := range []string{"Dry run", "would create: soroq.yaml", "No files were written (--dry-run)."} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("expected dry-run plan to contain %q, got:\n%s", want, stdout)
		}
	}
}

// --check must fail on a fresh project and pass once initialized, without writing on the check path.
func TestRunInitCheckSemantics(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\ndependencies:\n  soroq_flutter: any\n")
	writeMinimalAndroidProject(t, projectDir, "com.example.demo")

	if err := runInit([]string{"--project-dir", projectDir, "--check"}); err == nil {
		t.Fatal("expected --check to fail on a fresh project")
	} else if !strings.Contains(err.Error(), "soroq init") {
		t.Fatalf("expected --check error to name `soroq init`, got %v", err)
	}

	server := newInitTestServer(t, nil)
	defer server.Close()
	captureStdout(t, func() {
		if err := runInit([]string{"--project-dir", projectDir, "--api", server.URL, "--add-dependency=false"}); err != nil {
			t.Fatalf("runInit() error = %v", err)
		}
	})

	before := snapshotDir(t, projectDir)
	if err := runInit([]string{"--project-dir", projectDir, "--check"}); err != nil {
		t.Fatalf("expected --check to pass after init, got %v", err)
	}
	after := snapshotDir(t, projectDir)
	assertSameSnapshot(t, before, after)
}

// Second `soroq init` (no --force) must be a clean no-op that writes nothing and exits 0.
func TestRunInitCleanNoOp(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\ndependencies:\n  soroq_flutter: any\n")
	writeMinimalAndroidProject(t, projectDir, "com.example.demo")
	server := newInitTestServer(t, nil)
	defer server.Close()

	stdout := captureStdout(t, func() {
		if err := runInit([]string{"--project-dir", projectDir, "--api", server.URL, "--add-dependency=false"}); err != nil {
			t.Fatalf("first runInit() error = %v", err)
		}
	})
	if !strings.Contains(stdout, "What Soroq set up and why:") {
		t.Fatalf("expected teaching explain on first init, got:\n%s", stdout)
	}

	before := snapshotDir(t, projectDir)
	stdout = captureStdout(t, func() {
		if err := runInit([]string{"--project-dir", projectDir, "--api", server.URL, "--add-dependency=false"}); err != nil {
			t.Fatalf("second runInit() (no --force) error = %v", err)
		}
	})
	after := snapshotDir(t, projectDir)

	assertSameSnapshot(t, before, after)
	if !strings.Contains(stdout, "Already initialized — nothing to do.") {
		t.Fatalf("expected clean no-op message on re-run, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "What Soroq set up and why:") {
		t.Fatalf("teaching explain should not print on a no-op, got:\n%s", stdout)
	}
}

// A re-run with a DIFFERENT explicit --app-id is a genuine conflict and must still require --force.
func TestRunInitConflictRequiresForce(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\ndependencies:\n  soroq_flutter: any\n")
	server := newInitTestServer(t, nil)
	defer server.Close()
	captureStdout(t, func() {
		if err := runInit([]string{"--project-dir", projectDir, "--api", server.URL, "--app-id", "com.example.demo", "--add-dependency=false"}); err != nil {
			t.Fatalf("runInit() error = %v", err)
		}
	})

	before := snapshotDir(t, projectDir)
	err := runInit([]string{"--project-dir", projectDir, "--api", server.URL, "--app-id", "com.example.other", "--add-dependency=false"})
	if err == nil {
		t.Fatal("expected conflict error when --app-id differs from stored app_id without --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected conflict error to mention --force, got %v", err)
	}
	after := snapshotDir(t, projectDir)
	assertSameSnapshot(t, before, after)
}

// The --json output must stay a stable, machine-only shape (no teaching / warning lines leaked in).
func TestRunInitJSONShapeStable(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\ndependencies:\n  soroq_flutter: any\n")
	server := newInitTestServer(t, nil)
	defer server.Close()

	stdout := captureStdout(t, func() {
		if err := runInit([]string{"--project-dir", projectDir, "--api", server.URL, "--app-id", "com.example.demo", "--add-dependency=false", "--json"}); err != nil {
			t.Fatalf("runInit(--json) error = %v", err)
		}
	})

	if !json.Valid([]byte(stdout)) {
		t.Fatalf("--json output is not valid JSON:\n%s", stdout)
	}
	if strings.Contains(stdout, "What Soroq set up") || strings.Contains(stdout, "trust anchor") || strings.Contains(stdout, "Next step") {
		t.Fatalf("teaching/human lines leaked into --json output:\n%s", stdout)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("Unmarshal(--json) error = %v", err)
	}
	wantKeys := []string{
		"project_dir", "soroq_config_path", "app_id", "channel", "runtime_id_strategy",
		"dependency_added", "auto_update_config_path", "auto_update_base_url", "auto_update_config_written",
		"pubspec_updated", "manifest_internet_updated", "android_ndk_version_updated",
		"android_compatible_ndk_version", "hosted_app_created",
	}
	gotKeys := map[string]bool{}
	for k := range got {
		gotKeys[k] = true
	}
	for _, k := range wantKeys {
		if !gotKeys[k] {
			t.Fatalf("--json missing expected key %q; got keys %v", k, keysOfAny(got))
		}
	}
	// hosted_app is the only omitempty key; anything else is an unexpected shape change.
	for k := range gotKeys {
		if !contains(wantKeys, k) && k != "hosted_app" {
			t.Fatalf("--json has unexpected key %q (shape changed); got %v", k, keysOfAny(got))
		}
	}
}

func TestResolveInitAppIDNamespaceFallbackWarns(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, "android", "app"), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	// namespace only, NO applicationId -> conservative inference must warn.
	writeFile(t, filepath.Join(projectDir, "android", "app", "build.gradle.kts"), `android {
    namespace = "com.example.fallback"
}
`)
	status := projectStatus{ProjectDir: projectDir}
	id, warnings, err := resolveInitAppID(projectDir, "", status)
	if err != nil {
		t.Fatalf("resolveInitAppID error = %v", err)
	}
	if id != "com.example.fallback" {
		t.Fatalf("expected inferred id com.example.fallback, got %q", id)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "namespace") {
		t.Fatalf("expected a namespace-fallback warning, got %v", warnings)
	}
}

func TestResolveInitAppIDMultipleIOSWarns(t *testing.T) {
	projectDir := t.TempDir()
	xcodeDir := filepath.Join(projectDir, "ios", "Runner.xcodeproj")
	if err := os.MkdirAll(xcodeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	writeFile(t, filepath.Join(xcodeDir, "project.pbxproj"), `
PRODUCT_BUNDLE_IDENTIFIER = com.example.one;
PRODUCT_BUNDLE_IDENTIFIER = com.example.two;
PRODUCT_BUNDLE_IDENTIFIER = com.example.oneTests;
`)
	status := projectStatus{ProjectDir: projectDir}
	id, warnings, err := resolveInitAppID(projectDir, "", status)
	if err != nil {
		t.Fatalf("resolveInitAppID error = %v", err)
	}
	if id != "com.example.one" {
		t.Fatalf("expected first candidate com.example.one, got %q", id)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "multiple iOS bundle identifiers") {
		t.Fatalf("expected a multiple-iOS-bundle-id warning, got %v", warnings)
	}
}

// The common real Flutter pbxproj repeats the SAME bundle id across Debug/Release/Profile plus a
// RunnerTests id. Conservative inference must collapse that to one candidate and NOT warn (req 5:
// keep the unambiguous happy path quiet).
func TestResolveInitAppIDSingleRepeatedIOSNoWarn(t *testing.T) {
	projectDir := t.TempDir()
	xcodeDir := filepath.Join(projectDir, "ios", "Runner.xcodeproj")
	if err := os.MkdirAll(xcodeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	writeFile(t, filepath.Join(xcodeDir, "project.pbxproj"), `
PRODUCT_BUNDLE_IDENTIFIER = com.example.app;
PRODUCT_BUNDLE_IDENTIFIER = com.example.app;
PRODUCT_BUNDLE_IDENTIFIER = com.example.app;
PRODUCT_BUNDLE_IDENTIFIER = com.example.app.RunnerTests;
`)
	status := projectStatus{ProjectDir: projectDir}
	id, warnings, err := resolveInitAppID(projectDir, "", status)
	if err != nil {
		t.Fatalf("resolveInitAppID error = %v", err)
	}
	if id != "com.example.app" {
		t.Fatalf("expected com.example.app, got %q", id)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected NO warning for a single repeated bundle id, got %v", warnings)
	}
}

func TestResolveInitAppIDNoFabrication(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")
	status := projectStatus{ProjectDir: projectDir}
	if id, _, err := resolveInitAppID(projectDir, "", status); err == nil {
		t.Fatalf("expected inference to fail (never fabricate), got id=%q", id)
	}
}

func keysOfAny(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
