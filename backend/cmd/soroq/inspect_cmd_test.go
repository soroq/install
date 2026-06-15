package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInspectAndroidPrintsBundledMetadata(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})

	stdout := captureStdout(t, func() {
		err := runInspectAndroid([]string{"--artifact", artifactPath})
		if err != nil {
			t.Fatalf("runInspectAndroid() error = %v", err)
		}
	})

	for _, expected := range []string{
		"Android artifact:",
		"app_id: com.example.app",
		"runtime_id: runtime-1",
		"channel: stable",
		"abis: arm64-v8a",
	} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("expected %q in output, got %q", expected, stdout)
		}
	}
}

func TestRunInspectAndroidJSONIncludesABIs(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})

	stdout := captureStdout(t, func() {
		err := runInspectAndroid([]string{"--artifact", artifactPath, "--json"})
		if err != nil {
			t.Fatalf("runInspectAndroid() error = %v", err)
		}
	})

	var summary inspectAndroidSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("Unmarshal(summary) error = %v; stdout=%q", err, stdout)
	}
	if len(summary.ABIs) != 1 || summary.ABIs[0] != "arm64-v8a" {
		t.Fatalf("expected arm64-v8a ABI, got %v", summary.ABIs)
	}
	if summary.Snapshot.Metadata.Soroq.AppID != "com.example.app" {
		t.Fatalf("expected app id, got %+v", summary.Snapshot.Metadata.Soroq)
	}
}
