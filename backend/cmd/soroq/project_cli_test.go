package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectProjectReady(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")

	status, err := inspectProject(projectDir)
	if err != nil {
		t.Fatalf("inspectProject() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("expected project to be ready, got %+v", status)
	}
	if !status.ReleaseReady {
		t.Fatalf("expected project to be release ready, got %+v", status)
	}
	if !status.PatchReady {
		t.Fatalf("expected project to be patch ready, got %+v", status)
	}
	if status.AppID != "com.example.app" {
		t.Fatalf("expected app_id, got %q", status.AppID)
	}
	if status.Channel != "stable" {
		t.Fatalf("expected channel, got %q", status.Channel)
	}
	if !status.AppIDLooksValid {
		t.Fatalf("expected app_id to look valid, got %+v", status)
	}
	if !status.ChannelLooksValid {
		t.Fatalf("expected channel to look valid, got %+v", status)
	}
}

func TestInspectProjectAcceptsLogicalSoroqAppID(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: sample-release-aot-cli-proof\nchannel: stable\n")

	status, err := inspectProject(projectDir)
	if err != nil {
		t.Fatalf("inspectProject() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("expected logical Soroq app id to be ready, got %+v", status)
	}
	if !status.AppIDLooksValid {
		t.Fatalf("expected app_id to look valid, got %+v", status)
	}
}

func TestInspectProjectWarnsWhenDependencyMissing(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "dependencies:\n  flutter:\n    sdk: flutter\n")
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")

	status, err := inspectProject(projectDir)
	if err != nil {
		t.Fatalf("inspectProject() error = %v", err)
	}
	if status.Ready {
		t.Fatalf("expected project to be not ready")
	}
	if len(status.Warnings) == 0 {
		t.Fatalf("expected warnings, got none")
	}
	if !strings.Contains(strings.Join(status.Warnings, "\n"), "soroq_flutter") {
		t.Fatalf("expected dependency warning, got %v", status.Warnings)
	}
}

func TestInspectProjectWarnsWhenConfigShapeInvalid(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: Demo App\nchannel: Canary Track\n")

	status, err := inspectProject(projectDir)
	if err != nil {
		t.Fatalf("inspectProject() error = %v", err)
	}
	if status.Ready {
		t.Fatalf("expected malformed config to be not ready")
	}
	if status.ReleaseReady {
		t.Fatalf("expected malformed config to be not release ready")
	}
	if status.PatchReady {
		t.Fatalf("expected malformed config to be not patch ready")
	}
	warnings := strings.Join(status.Warnings, "\n")
	if !strings.Contains(warnings, "stable Soroq app id") {
		t.Fatalf("expected app_id shape warning, got %v", status.Warnings)
	}
	if !strings.Contains(warnings, "stable slug") {
		t.Fatalf("expected channel shape warning, got %v", status.Warnings)
	}
}

func TestRunInitWritesSoroqYaml(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")

	stdout := captureStdout(t, func() {
		if err := runInit([]string{"--project-dir", projectDir, "--app-id", "com.example.demo"}); err != nil {
			t.Fatalf("runInit() error = %v", err)
		}
	})

	content, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(soroq.yaml) error = %v", err)
	}
	text := string(content)
	if !strings.Contains(text, "app_id: com.example.demo") {
		t.Fatalf("expected app_id in file, got %q", text)
	}
	if !strings.Contains(text, "channel: stable") {
		t.Fatalf("expected channel in file, got %q", text)
	}
	if !strings.Contains(stdout, "Wrote ") {
		t.Fatalf("expected write confirmation, got %q", stdout)
	}
}

func TestRunInitRejectsInvalidAppID(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")

	err := runInit([]string{"--project-dir", projectDir, "--app-id", "Demo App"})
	if err == nil {
		t.Fatalf("expected invalid app id error")
	}
	if !strings.Contains(err.Error(), "stable Soroq app id") {
		t.Fatalf("expected app id shape guidance, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, "soroq.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("expected soroq.yaml not to be written, stat error = %v", statErr)
	}
}

func TestRunInitRejectsInvalidChannel(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")

	err := runInit([]string{"--project-dir", projectDir, "--app-id", "com.example.demo", "--channel", "Canary Track"})
	if err == nil {
		t.Fatalf("expected invalid channel error")
	}
	if !strings.Contains(err.Error(), "stable slug") {
		t.Fatalf("expected channel shape guidance, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, "soroq.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("expected soroq.yaml not to be written, stat error = %v", statErr)
	}
}

func TestRunStatusJSON(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: beta\n")

	stdout := captureStdout(t, func() {
		if err := runStatus([]string{"--project-dir", projectDir, "--json"}); err != nil {
			t.Fatalf("runStatus() error = %v", err)
		}
	})

	var payload projectStatus
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; stdout=%q", err, stdout)
	}
	if payload.Channel != "beta" {
		t.Fatalf("expected beta channel, got %q", payload.Channel)
	}
	if !payload.Ready {
		t.Fatalf("expected ready payload, got %+v", payload)
	}
	if !payload.ReleaseReady {
		t.Fatalf("expected release-ready payload, got %+v", payload)
	}
	if !payload.PatchReady {
		t.Fatalf("expected patch-ready payload, got %+v", payload)
	}
}

func TestRunStatusCheckPassesWhenReady(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")

	stdout := captureStdout(t, func() {
		if err := runStatus([]string{"--project-dir", projectDir, "--check"}); err != nil {
			t.Fatalf("runStatus(--check) error = %v", err)
		}
	})

	if !strings.Contains(stdout, "ready: yes") {
		t.Fatalf("expected ready status output, got %q", stdout)
	}
}

func TestRunStatusCheckFailsWhenNotReady(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "dependencies:\n  flutter:\n    sdk: flutter\n")
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")

	var statusErr error
	stdout := captureStdout(t, func() {
		statusErr = runStatus([]string{"--project-dir", projectDir, "--check"})
	})

	if statusErr == nil {
		t.Fatalf("expected --check to fail for not-ready project")
	}
	if !strings.Contains(stdout, "ready: no") {
		t.Fatalf("expected not-ready status output, got %q", stdout)
	}
	if !strings.Contains(stdout, "soroq_flutter") {
		t.Fatalf("expected dependency warning, got %q", stdout)
	}
}

func writeFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = original
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	return buf.String()
}
