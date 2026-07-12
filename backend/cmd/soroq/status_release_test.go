package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRunStatusPrintsRecordedRelease proves status enriches its output with release_id / version /
// channel / toolchain_version sourced from LOCAL data (cli-state + soroq.lock) with no network call.
func TestRunStatusPrintsRecordedRelease(t *testing.T) {
	// A bogus SOROQ_API would fail loudly if status ever made a network call.
	t.Setenv("SOROQ_API", "http://127.0.0.1:0")
	dir := t.TempDir()
	writeReadyProject(t, dir)
	if err := saveProjectCLIState(dir, projectCLIState{
		SchemaVersion: 1,
		LastAndroidRelease: &androidReleaseState{
			UpdatedAt: time.Now().UTC(),
			AppID:     "com.example.app",
			Channel:   "stable",
			ReleaseID: "release-42",
			Version:   "1.2.3+45",
		},
	}); err != nil {
		t.Fatalf("saveProjectCLIState() error = %v", err)
	}
	if err := recordSoroqLockPin(dir, "android", soroqLockPin{
		ReleaseID:        "release-42",
		Version:          "1.2.3+45",
		ToolchainVersion: "soroq-android-r7",
	}); err != nil {
		t.Fatalf("recordSoroqLockPin() error = %v", err)
	}

	out := captureStdout(t, func() {
		if err := runStatus([]string{"--project-dir", dir}); err != nil {
			t.Fatalf("runStatus() error = %v", err)
		}
	})
	for _, want := range []string{
		"release_id: release-42",
		"version: 1.2.3+45",
		"release channel: stable",
		"toolchain_version: soroq-android-r7",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}

	// JSON: additive fields present, existing shape (e.g. "ready") preserved.
	outJSON := captureStdout(t, func() {
		if err := runStatus([]string{"--project-dir", dir, "--json"}); err != nil {
			t.Fatalf("runStatus(--json) error = %v", err)
		}
	})
	var payload map[string]any
	if err := json.Unmarshal([]byte(outJSON), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; out=%s", err, outJSON)
	}
	if payload["release_id"] != "release-42" {
		t.Fatalf("json release_id = %v, want release-42", payload["release_id"])
	}
	if payload["toolchain_version"] != "soroq-android-r7" {
		t.Fatalf("json toolchain_version = %v, want soroq-android-r7", payload["toolchain_version"])
	}
	if _, ok := payload["ready"]; !ok {
		t.Fatalf("json must preserve the existing 'ready' field:\n%s", outJSON)
	}
	if payload["channel"] != "stable" {
		t.Fatalf("json must preserve the existing soroq.yaml 'channel' field, got %v", payload["channel"])
	}
}

// TestRunStatusNoReleaseRecorded proves the clear fallback line when no release is recorded and that
// the additive JSON fields are omitted.
func TestRunStatusNoReleaseRecorded(t *testing.T) {
	dir := t.TempDir()
	writeReadyProject(t, dir)

	out := captureStdout(t, func() {
		if err := runStatus([]string{"--project-dir", dir}); err != nil {
			t.Fatalf("runStatus() error = %v", err)
		}
	})
	if !strings.Contains(out, "release: no release recorded") {
		t.Fatalf("expected no-release-recorded line:\n%s", out)
	}

	outJSON := captureStdout(t, func() {
		if err := runStatus([]string{"--project-dir", dir, "--json"}); err != nil {
			t.Fatalf("runStatus(--json) error = %v", err)
		}
	})
	if strings.Contains(outJSON, "\"release_id\"") || strings.Contains(outJSON, "\"toolchain_version\"") {
		t.Fatalf("unrecorded release must omit additive JSON fields:\n%s", outJSON)
	}
}
