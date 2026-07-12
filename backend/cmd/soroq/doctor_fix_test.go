package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInferableProject lays down a Flutter project WITHOUT soroq.yaml but WITH an Android
// applicationId (so doctor --fix can infer an app id offline) and no soroq_flutter dependency (so
// the pub-add fix stays advisory — proof --fix never auto-runs `flutter pub add`).
func writeInferableProject(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: fixapp\nflutter:\n  assets:\n    - soroq.yaml\n")
	if err := os.MkdirAll(filepath.Join(dir, "android", "app"), 0o755); err != nil {
		t.Fatalf("MkdirAll(android/app) error = %v", err)
	}
	writeFile(t, filepath.Join(dir, "android", "app", "build.gradle.kts"),
		"android {\n    defaultConfig {\n        applicationId = \"com.example.fixapp\"\n    }\n}\n")
}

func TestRunDoctorFixScaffoldsSoroqYAMLOfflineIdempotently(t *testing.T) {
	dir := t.TempDir()
	writeInferableProject(t, dir)
	cfg := doctorIsolateAuth(t)

	soroqPath := filepath.Join(dir, "soroq.yaml")
	if _, err := os.Stat(soroqPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition: soroq.yaml should be absent, stat err = %v", err)
	}

	// First --fix run: scaffolds soroq.yaml + manifest_trust offline.
	out1 := captureStdout(t, func() {
		if err := runDoctor([]string{"--project-dir", dir, "--config", cfg, "--offline", "--fix"}); err != nil &&
			!errors.Is(err, errAlreadyPrinted) {
			t.Fatalf("runDoctor(--fix) unexpected error = %v", err)
		}
	})
	if !strings.Contains(out1, "auto-applied offline fixes") || !strings.Contains(out1, "scaffolded soroq.yaml") {
		t.Fatalf("expected soroq.yaml scaffold in output:\n%s", out1)
	}
	if !strings.Contains(out1, "scaffolded manifest_trust") {
		t.Fatalf("expected manifest_trust scaffold in output:\n%s", out1)
	}
	body, err := os.ReadFile(soroqPath)
	if err != nil {
		t.Fatalf("soroq.yaml not written: %v", err)
	}
	if !strings.Contains(string(body), "app_id: com.example.fixapp") || !strings.Contains(string(body), "manifest_trust:") {
		t.Fatalf("scaffolded soroq.yaml missing expected content:\n%s", body)
	}

	// --fix must NOT auto-run network/pub fixes: soroq_flutter dep stays advisory, auth not logged in.
	if !strings.Contains(out1, "still advisory") {
		t.Fatalf("expected a still-advisory section:\n%s", out1)
	}
	if !strings.Contains(out1, "flutter pub add soroq_flutter") {
		t.Fatalf("expected soroq_flutter dep to remain advisory (pub not auto-run):\n%s", out1)
	}
	if !strings.Contains(out1, "not logged in") {
		t.Fatalf("expected control-plane auth to remain advisory (login not auto-run):\n%s", out1)
	}

	// Second --fix run: idempotent no-op.
	out2 := captureStdout(t, func() {
		if err := runDoctor([]string{"--project-dir", dir, "--config", cfg, "--offline", "--fix"}); err != nil &&
			!errors.Is(err, errAlreadyPrinted) {
			t.Fatalf("runDoctor(--fix) rerun unexpected error = %v", err)
		}
	})
	if !strings.Contains(out2, "no offline auto-fixes were applicable") {
		t.Fatalf("expected second --fix run to be a no-op:\n%s", out2)
	}
	body2, err := os.ReadFile(soroqPath)
	if err != nil {
		t.Fatalf("soroq.yaml missing after rerun: %v", err)
	}
	if string(body) != string(body2) {
		t.Fatalf("soroq.yaml changed on idempotent rerun:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}
}

// TestRunDoctorWithoutFixOmitsFixesField guards the additive JSON shape: without --fix the report
// carries no "fixes" key.
func TestRunDoctorWithoutFixOmitsFixesField(t *testing.T) {
	dir := t.TempDir()
	writeSoroqFlutterPubspec(t, dir)
	writeFile(t, filepath.Join(dir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	cfg := doctorIsolateAuth(t)

	out := captureStdout(t, func() {
		if err := runDoctor([]string{"--project-dir", dir, "--config", cfg, "--offline", "--json"}); err != nil &&
			!errors.Is(err, errAlreadyPrinted) {
			t.Fatalf("runDoctor(--json) unexpected error = %v", err)
		}
	})
	if strings.Contains(out, "\"fixes\"") {
		t.Fatalf("default doctor --json must not include a fixes field:\n%s", out)
	}
}
