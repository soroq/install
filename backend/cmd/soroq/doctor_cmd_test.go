package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// doctorIsolateAuth clears env tokens and returns a non-existent config path so the
// control-plane auth check is deterministically "not logged in" regardless of the dev's
// local ~/.soroq/config.json.
func doctorIsolateAuth(t *testing.T) string {
	t.Helper()
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "")
	t.Setenv("SOROQ_OPERATOR_TOKEN", "")
	return filepath.Join(t.TempDir(), "no-config.json")
}

func TestRunDoctorReportsProjectChecks(t *testing.T) {
	dir := t.TempDir()
	writeSoroqFlutterPubspec(t, dir)
	writeFile(t, filepath.Join(dir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	cfg := doctorIsolateAuth(t)

	out := captureStdout(t, func() {
		if err := runDoctor([]string{"--project-dir", dir, "--config", cfg, "--offline"}); err != nil &&
			!errors.Is(err, errAlreadyPrinted) {
			t.Fatalf("runDoctor() unexpected error = %v", err)
		}
	})
	for _, want := range []string{
		"Flutter project", "soroq.yaml", "app_id=com.example.app",
		"soroq_flutter dependency", "Control-plane auth", "not logged in",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q\n%s", want, out)
		}
	}
}

func TestRunDoctorMissingSoroqYAMLErrors(t *testing.T) {
	dir := t.TempDir()
	writeSoroqFlutterPubspec(t, dir) // pubspec present, soroq.yaml absent
	cfg := doctorIsolateAuth(t)

	var derr error
	out := captureStdout(t, func() {
		derr = runDoctor([]string{"--project-dir", dir, "--config", cfg, "--offline"})
	})
	if !errors.Is(derr, errAlreadyPrinted) {
		t.Fatalf("expected errAlreadyPrinted (non-zero exit) for missing soroq.yaml, got %v", derr)
	}
	if !strings.Contains(out, "soroq.yaml: missing") || !strings.Contains(out, "soroq init") {
		t.Fatalf("expected missing-soroq.yaml error + fix, got:\n%s", out)
	}
}

func TestRunDoctorJSON(t *testing.T) {
	dir := t.TempDir()
	writeSoroqFlutterPubspec(t, dir)
	writeFile(t, filepath.Join(dir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	cfg := doctorIsolateAuth(t)

	out := captureStdout(t, func() {
		_ = runDoctor([]string{"--project-dir", dir, "--config", cfg, "--offline", "--json"})
	})
	var report doctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, out)
	}
	if len(report.Checks) == 0 {
		t.Fatalf("expected checks in JSON report")
	}
	found := false
	for _, c := range report.Checks {
		if c.Name == "soroq.yaml" && c.Status == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected soroq.yaml ok check in JSON, got %+v", report.Checks)
	}
}

func TestRunDoctorControlPlaneAuthVerified(t *testing.T) {
	dir := t.TempDir()
	writeSoroqFlutterPubspec(t, dir)
	writeFile(t, filepath.Join(dir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/apps"):
			_, _ = w.Write([]byte(`[{"id":"com.example.app"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/releases"):
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "test-token")
	t.Setenv("SOROQ_OPERATOR_TOKEN", "")

	out := captureStdout(t, func() {
		if err := runDoctor([]string{"--project-dir", dir, "--api", server.URL}); err != nil &&
			!errors.Is(err, errAlreadyPrinted) {
			t.Fatalf("runDoctor() unexpected error = %v", err)
		}
	})
	if !strings.Contains(out, "Control-plane auth") || !strings.Contains(out, "verified") {
		t.Fatalf("expected verified control-plane auth, got:\n%s", out)
	}
}

func TestRunDoctorRegisteredRelease(t *testing.T) {
	dir := t.TempDir()
	writeSoroqFlutterPubspec(t, dir)
	writeFile(t, filepath.Join(dir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	cfg := doctorIsolateAuth(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/releases") {
			if got := r.URL.Query().Get("app_id"); got != "com.example.app" {
				t.Errorf("expected app_id filter, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"rel-1","app_id":"com.example.app","version":"1.2.3+45","channel":"stable","created_at":"2026-01-01T00:00:00Z"}]`))
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	defer server.Close()

	out := captureStdout(t, func() {
		if err := runDoctor([]string{"--project-dir", dir, "--config", cfg, "--api", server.URL}); err != nil &&
			!errors.Is(err, errAlreadyPrinted) {
			t.Fatalf("runDoctor() unexpected error = %v", err)
		}
	})
	if !strings.Contains(out, "Registered release") || !strings.Contains(out, "1.2.3+45") {
		t.Fatalf("expected registered release 1.2.3+45, got:\n%s", out)
	}
}
