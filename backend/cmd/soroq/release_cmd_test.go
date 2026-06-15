package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/domain"
)

func TestInspectAndroidArtifactReadsBundledMetadata(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "app-release.aab")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
		"base/lib/arm64-v8a/libflutter.so":                     []byte("flutter"),
	})

	inspection, err := inspectAndroidArtifact(artifactPath)
	if err != nil {
		t.Fatalf("inspectAndroidArtifact() error = %v", err)
	}
	if inspection.Artifact.BundledMetadataZipPath != "assets/flutter_assets/soroq/soroq_metadata.json" {
		t.Fatalf("expected normalized metadata path, got %q", inspection.Artifact.BundledMetadataZipPath)
	}
	if inspection.Metadata.Soroq.AppID != "com.example.app" {
		t.Fatalf("expected app_id, got %q", inspection.Metadata.Soroq.AppID)
	}
	abis := androidrelease.DeriveABIs(inspection)
	if len(abis) != 1 || abis[0] != "arm64-v8a" {
		t.Fatalf("expected inferred arm64-v8a ABI, got %v", abis)
	}
}

func TestRunReleaseAndroidRegistersRelease(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")

	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})

	var captured domain.CreateReleaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/releases" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Release{
			ID:        captured.ID,
			AppID:     captured.AppID,
			RuntimeID: captured.RuntimeID,
			Version:   captured.Version,
			Platform:  captured.Platform,
			Arch:      captured.Arch,
			Channel:   captured.Channel,
		}); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runReleaseAndroid([]string{
			"--project-dir", projectDir,
			"--artifact", artifactPath,
			"--api", server.URL,
			"--release-id", "release-1",
		})
		if err != nil {
			t.Fatalf("runReleaseAndroid() error = %v", err)
		}
	})

	if captured.ID != "release-1" {
		t.Fatalf("expected release id release-1, got %q", captured.ID)
	}
	if captured.AppID != "com.example.app" {
		t.Fatalf("expected app id, got %q", captured.AppID)
	}
	if captured.RuntimeID != "runtime-1" {
		t.Fatalf("expected runtime id, got %q", captured.RuntimeID)
	}
	if captured.Version != "1.2.3+45" {
		t.Fatalf("expected inferred version, got %q", captured.Version)
	}
	if captured.Arch != "arm64-v8a" {
		t.Fatalf("expected inferred arch, got %q", captured.Arch)
	}
	if !strings.Contains(stdout, "Registered Android release release-1") {
		t.Fatalf("expected registration output, got %q", stdout)
	}
}

func TestRunReleaseAndroidDefaultsToNewestArtifactAndRecordsState(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")
	releaseCandidatesDir := filepath.Join(projectDir, "release-candidates")
	if err := os.MkdirAll(releaseCandidatesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	artifactPath := filepath.Join(releaseCandidatesDir, "app-play-release.aab")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "play-internal", "runtime-1", "1.2.3+45")),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app-arm64"),
		"base/lib/x86_64/libapp.so":                            []byte("app-x64"),
	})

	var captured domain.CreateReleaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/releases" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Release{
			ID:                   captured.ID,
			AppID:                captured.AppID,
			RuntimeID:            captured.RuntimeID,
			Version:              captured.Version,
			Platform:             captured.Platform,
			Arch:                 captured.Arch,
			Channel:              captured.Channel,
			ManifestSigningKeyID: captured.ManifestSigningKeyID,
		}); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runReleaseAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--release-id", "release-1",
			"--build=false",
		})
		if err != nil {
			t.Fatalf("runReleaseAndroid() error = %v", err)
		}
	})

	if captured.Channel != "play-internal" {
		t.Fatalf("expected artifact channel to win by default, got %q", captured.Channel)
	}
	if captured.Arch != "universal" {
		t.Fatalf("expected universal arch for multi-ABI AAB, got %q", captured.Arch)
	}
	if captured.ManifestSigningKeyID != "prod-primary" {
		t.Fatalf("expected bundled manifest key id, got %q", captured.ManifestSigningKeyID)
	}
	if !strings.Contains(stdout, "artifact: "+artifactPath) {
		t.Fatalf("expected discovered artifact in stdout, got %q", stdout)
	}
	state, err := loadProjectCLIState(projectDir)
	if err != nil {
		t.Fatalf("loadProjectCLIState() error = %v", err)
	}
	if state.LastAndroidRelease == nil {
		t.Fatalf("expected last Android release state")
	}
	if state.LastAndroidRelease.ReleaseID != "release-1" {
		t.Fatalf("expected release id in state, got %+v", state.LastAndroidRelease)
	}
	if state.LastAndroidRelease.ArtifactPath == artifactPath {
		t.Fatalf("expected stashed immutable release artifact path, got source path %+v", state.LastAndroidRelease)
	}
	expectedReleaseDir := filepath.Join(projectDir, ".soroq", "releases", "release-1") + string(filepath.Separator)
	if !strings.HasPrefix(state.LastAndroidRelease.ArtifactPath, expectedReleaseDir) {
		t.Fatalf("expected stashed artifact under %s, got %+v", expectedReleaseDir, state.LastAndroidRelease)
	}
	sourceBytes, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("ReadFile(source artifact) error = %v", err)
	}
	stashedBytes, err := os.ReadFile(state.LastAndroidRelease.ArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile(stashed artifact) error = %v", err)
	}
	if !bytes.Equal(sourceBytes, stashedBytes) {
		t.Fatalf("expected stashed artifact bytes to match source")
	}
}

func TestRunReleaseListPrintsReleases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/releases" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("app_id") != "com.example.app" {
			t.Fatalf("expected app_id query, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.Release{
			{
				ID:        "release-1",
				AppID:     "com.example.app",
				RuntimeID: "runtime-1",
				Version:   "1.2.3+45",
				Platform:  "android",
				Arch:      "arm64-v8a",
				Channel:   "stable",
			},
		}); err != nil {
			t.Fatalf("Encode(releases) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runReleaseList([]string{
			"--api", server.URL,
			"--app-id", "com.example.app",
		})
		if err != nil {
			t.Fatalf("runReleaseList() error = %v", err)
		}
	})

	if !strings.Contains(stdout, "Soroq releases: 1") {
		t.Fatalf("expected release count, got %q", stdout)
	}
	if !strings.Contains(stdout, "release-1") {
		t.Fatalf("expected release id, got %q", stdout)
	}
}

func TestRunReleaseStatusPrintsRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/releases/release-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Release{
			ID:        "release-1",
			AppID:     "com.example.app",
			RuntimeID: "runtime-1",
			Version:   "1.2.3+45",
			Platform:  "android",
			Arch:      "arm64-v8a",
			Channel:   "stable",
		}); err != nil {
			t.Fatalf("Encode(release) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runReleaseStatus([]string{
			"--api", server.URL,
			"--release-id", "release-1",
		})
		if err != nil {
			t.Fatalf("runReleaseStatus() error = %v", err)
		}
	})

	for _, expected := range []string{
		"Soroq release release-1",
		"app_id: com.example.app",
		"runtime_id: runtime-1",
		"version: 1.2.3+45",
		"arch: arm64-v8a",
		"channel: stable",
	} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("expected %q in output, got %q", expected, stdout)
		}
	}
}

func TestRunReleaseAndroidRequiresArchForMultiABI(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")

	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
		"lib/x86_64/libapp.so":                            []byte("app"),
	})

	err := runReleaseAndroid([]string{
		"--project-dir", projectDir,
		"--artifact", artifactPath,
	})
	if err == nil {
		t.Fatalf("expected error for multi-ABI artifact without --arch")
	}
	if !strings.Contains(err.Error(), "pass --arch explicitly") {
		t.Fatalf("expected explicit arch guidance, got %v", err)
	}
}

func TestRunReleaseAndroidSuggestsAppCreateWhenAppUnknown(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")

	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/releases" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		http.Error(w, `{"error":"unknown app \"com.example.app\""}`, http.StatusBadRequest)
	}))
	defer server.Close()

	err := runReleaseAndroid([]string{
		"--project-dir", projectDir,
		"--artifact", artifactPath,
		"--api", server.URL,
		"--release-id", "release-1",
	})
	if err == nil {
		t.Fatalf("expected unknown app error")
	}
	if !strings.Contains(err.Error(), "soroq app create") {
		t.Fatalf("expected app create guidance, got %v", err)
	}
	if !strings.Contains(err.Error(), "com.example.app") {
		t.Fatalf("expected app id in guidance, got %v", err)
	}
}

func TestRunReleaseAndroidRejectsInvalidProjectConfigBeforeArtifactInspection(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: Demo App\nchannel: stable\n")

	err := runReleaseAndroid([]string{
		"--project-dir", projectDir,
		"--artifact", filepath.Join(t.TempDir(), "missing-release.aab"),
	})
	if err == nil {
		t.Fatalf("expected invalid project config error")
	}
	if !strings.Contains(err.Error(), "stable Soroq app id") {
		t.Fatalf("expected app_id shape guidance, got %v", err)
	}
}

func writeArtifactZip(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create(%s) error = %v", path, err)
	}
	defer file.Close()

	writer := zip.NewWriter(file)
	for name, payload := range entries {
		entryWriter, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%s) error = %v", name, err)
		}
		if _, err := entryWriter.Write(payload); err != nil {
			t.Fatalf("Write(%s) error = %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
}

func testBundledMetadataJSON(appID, channel, runtimeID, version string) string {
	return `{
  "schema_version": 1,
  "app": {
    "name": "Example",
    "version": "` + version + `",
    "build_name": "1.2.3",
    "build_number": "45"
  },
  "soroq": {
    "app_id": "` + appID + `",
    "channel": "` + channel + `",
    "runtime_id": "` + runtimeID + `",
    "runtime_id_strategy": "manifest_trust_v1",
    "manifest_trust": {
      "keys": [
        { "id": "prod-primary", "public_key": "abc" }
      ]
    },
    "manifest_trust_fingerprint": "fingerprint-1"
  }
}`
}
