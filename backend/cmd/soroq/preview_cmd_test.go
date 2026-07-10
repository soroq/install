package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"soroq/backend/internal/domain"
)

func TestRunPreviewAndroidDownloadsReleaseAndChecksPatch(t *testing.T) {
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "test-token")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "owner@example.com")

	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	releaseArtifactPath := filepath.Join(t.TempDir(), "base.aab")
	writeArtifactZip(t, releaseArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("base"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	releaseArtifactBytes, err := os.ReadFile(releaseArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile(release artifact) error = %v", err)
	}
	releaseArtifactSHA := sha256.Sum256(releaseArtifactBytes)

	var capturedPatchCheck domain.PatchCheckRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Fatalf("expected release list to use operator auth, got %q", r.Header.Get("Authorization"))
			}
			if r.URL.Query().Get("app_id") != "com.example.app" {
				t.Fatalf("expected app_id query, got %q", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]domain.Release{
				{
					ID:        "older-release",
					AppID:     "com.example.app",
					RuntimeID: "runtime-old",
					Version:   "1.0.0+1",
					Platform:  "android",
					Arch:      "universal",
					Channel:   "stable",
					CreatedAt: time.Now().Add(-time.Hour),
				},
				{
					ID:        "release-1",
					AppID:     "com.example.app",
					RuntimeID: "runtime-1",
					Version:   "1.2.3+45",
					Platform:  "android",
					Arch:      "universal",
					Channel:   "stable",
					CreatedAt: time.Now(),
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-1/artifact":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Fatalf("expected artifact download to use operator auth, got %q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Disposition", `attachment; filename="playstore-base.aab"`)
			w.Header().Set("X-Soroq-Artifact-SHA256", fmt.Sprintf("%x", releaseArtifactSHA[:]))
			_, _ = w.Write(releaseArtifactBytes)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patch-check":
			if auth := r.Header.Get("Authorization"); auth != "" {
				t.Fatalf("runtime patch-check must not require operator auth, got %q", auth)
			}
			if err := json.NewDecoder(r.Body).Decode(&capturedPatchCheck); err != nil {
				t.Fatalf("Decode(patch-check) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.PatchCheckResponse{
				PatchAvailable: true,
				Patch: &domain.PatchDescriptor{
					ID:             "patch-1",
					Number:         2,
					Track:          capturedPatchCheck.Track,
					ManifestURL:    serverURL(r) + "/manifest.json",
					BundleURL:      serverURL(r) + "/bundle.zip",
					ActivationMode: domain.ActivationNextColdStart,
					Kind:           domain.PatchKindAsset,
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/manifest.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.PatchManifest{
				PatchID:        "patch-1",
				PatchNumber:    2,
				RuntimeID:      "runtime-1",
				ReleaseID:      "release-1",
				Channel:        "stable",
				Kind:           domain.PatchKindAsset,
				ActivationMode: domain.ActivationNextColdStart,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/bundle.zip":
			_, _ = w.Write([]byte("bundle-bytes"))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPreviewAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--release-version", "latest",
			"--client-id", "device-a",
			"--current-patch-number", "1",
			"--track", "staging",
			"--kind", "asset",
			"--download-patch",
		})
		if err != nil {
			t.Fatalf("runPreviewAndroid() error = %v", err)
		}
	})

	if capturedPatchCheck.AppID != "com.example.app" {
		t.Fatalf("unexpected patch-check app id %q", capturedPatchCheck.AppID)
	}
	if capturedPatchCheck.ReleaseID != "release-1" {
		t.Fatalf("unexpected patch-check release id %q", capturedPatchCheck.ReleaseID)
	}
	if capturedPatchCheck.ReleaseVersion != "1.2.3+45" {
		t.Fatalf("unexpected patch-check release version %q", capturedPatchCheck.ReleaseVersion)
	}
	if capturedPatchCheck.RuntimeID != "runtime-1" {
		t.Fatalf("unexpected patch-check runtime id %q", capturedPatchCheck.RuntimeID)
	}
	if capturedPatchCheck.Channel != "stable" {
		t.Fatalf("unexpected patch-check channel %q", capturedPatchCheck.Channel)
	}
	if capturedPatchCheck.Track != "staging" {
		t.Fatalf("unexpected patch-check track %q", capturedPatchCheck.Track)
	}
	if capturedPatchCheck.CurrentPatchNumber != 1 {
		t.Fatalf("unexpected current patch number %d", capturedPatchCheck.CurrentPatchNumber)
	}
	if capturedPatchCheck.ClientID != "device-a" {
		t.Fatalf("unexpected client id %q", capturedPatchCheck.ClientID)
	}
	if capturedPatchCheck.Kind != domain.PatchKindAsset {
		t.Fatalf("unexpected patch kind %q", capturedPatchCheck.Kind)
	}
	if !strings.Contains(stdout, "Soroq Android preview") {
		t.Fatalf("expected preview heading, got %q", stdout)
	}
	if !strings.Contains(stdout, "patch_available: yes") {
		t.Fatalf("expected patch availability, got %q", stdout)
	}
	if !strings.Contains(stdout, "patch_id: patch-1") {
		t.Fatalf("expected patch id, got %q", stdout)
	}
	if !strings.Contains(stdout, "patch_track: staging") {
		t.Fatalf("expected patch track, got %q", stdout)
	}
	if !strings.Contains(stdout, "downloaded_manifest:") || !strings.Contains(stdout, "downloaded_bundle:") {
		t.Fatalf("expected downloaded artifacts in output, got %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".soroq", "previews", "release-1", "patch-1-manifest.json")); err != nil {
		t.Fatalf("expected downloaded manifest, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".soroq", "previews", "release-1", "patch-1-bundle.zip")); err != nil {
		t.Fatalf("expected downloaded bundle, stat error = %v", err)
	}
}

func TestRunPreviewAndroidDefaultsToRememberedRelease(t *testing.T) {
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "test-token")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "owner@example.com")

	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	if err := saveProjectCLIState(projectDir, projectCLIState{
		SchemaVersion: 1,
		LastAndroidRelease: &androidReleaseState{
			UpdatedAt:    time.Now().UTC(),
			APIBase:      "https://state-api.example",
			AppID:        "com.example.app",
			Channel:      "stable",
			ReleaseID:    "remembered-release",
			RuntimeID:    "runtime-remembered",
			Version:      "1.2.3+45",
			Arch:         "universal",
			ArtifactPath: filepath.Join(projectDir, ".soroq", "releases", "remembered-release", "base.aab"),
		},
	}); err != nil {
		t.Fatalf("saveProjectCLIState() error = %v", err)
	}

	releaseArtifactPath := filepath.Join(t.TempDir(), "remembered-base.aab")
	writeArtifactZip(t, releaseArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-remembered", "1.2.3+45")),
		"base/lib/arm64-v8a/libapp.so":                         []byte("remembered-app"),
	})
	releaseArtifactBytes, err := os.ReadFile(releaseArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile(release artifact) error = %v", err)
	}
	var capturedPatchCheck domain.PatchCheckRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases":
			t.Fatalf("preview should use remembered release instead of listing latest releases")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/remembered-release":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Release{
				ID:        "remembered-release",
				AppID:     "com.example.app",
				RuntimeID: "runtime-remembered",
				Version:   "1.2.3+45",
				Platform:  "android",
				Arch:      "universal",
				Channel:   "stable",
				CreatedAt: time.Now().Add(-time.Hour),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/remembered-release/artifact":
			w.Header().Set("Content-Disposition", `attachment; filename="remembered-base.aab"`)
			_, _ = w.Write(releaseArtifactBytes)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patch-check":
			if err := json.NewDecoder(r.Body).Decode(&capturedPatchCheck); err != nil {
				t.Fatalf("Decode(patch-check) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.PatchCheckResponse{})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPreviewAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--track", "staging",
		})
		if err != nil {
			t.Fatalf("runPreviewAndroid() error = %v", err)
		}
	})

	if capturedPatchCheck.RuntimeID != "runtime-remembered" {
		t.Fatalf("expected remembered runtime in patch-check, got %q", capturedPatchCheck.RuntimeID)
	}
	if capturedPatchCheck.ReleaseID != "remembered-release" {
		t.Fatalf("expected remembered release in patch-check, got %q", capturedPatchCheck.ReleaseID)
	}
	if capturedPatchCheck.ReleaseVersion != "1.2.3+45" {
		t.Fatalf("expected remembered release version in patch-check, got %q", capturedPatchCheck.ReleaseVersion)
	}
	if !strings.Contains(stdout, "release_id: remembered-release") {
		t.Fatalf("expected remembered release in output, got %q", stdout)
	}
}

func TestRunPreviewAndroidRejectsReleaseArtifactMismatch(t *testing.T) {
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "test-token")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "owner@example.com")

	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	releaseArtifactPath := filepath.Join(t.TempDir(), "base.aab")
	writeArtifactZip(t, releaseArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-other", "1.2.3+45")),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	releaseArtifactBytes, err := os.ReadFile(releaseArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile(release artifact) error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-1":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Release{
				ID:        "release-1",
				AppID:     "com.example.app",
				RuntimeID: "runtime-1",
				Version:   "1.2.3+45",
				Platform:  "android",
				Arch:      "universal",
				Channel:   "stable",
				CreatedAt: time.Now(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-1/artifact":
			w.Header().Set("Content-Disposition", `attachment; filename="playstore-base.aab"`)
			_, _ = w.Write(releaseArtifactBytes)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patch-check":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.PatchCheckResponse{})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	err = runPreviewAndroid([]string{
		"--project-dir", projectDir,
		"--api", server.URL,
		"--release-id", "release-1",
	})
	if err == nil {
		t.Fatalf("expected release artifact mismatch")
	}
	if !strings.Contains(err.Error(), "runtime_id") {
		t.Fatalf("expected runtime mismatch error, got %v", err)
	}
}

func TestRunPreviewAndroidInstallsAndLaunchesAPK(t *testing.T) {
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "test-token")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "owner@example.com")

	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	releaseArtifactPath := filepath.Join(t.TempDir(), "base.apk")
	writeArtifactZip(t, releaseArtifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	releaseArtifactBytes, err := os.ReadFile(releaseArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile(release artifact) error = %v", err)
	}

	adbLogPath := filepath.Join(t.TempDir(), "adb.log")
	fakeADBPath := filepath.Join(t.TempDir(), "adb")
	writeFile(t, fakeADBPath, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$ADB_LOG\"\n")
	if err := os.Chmod(fakeADBPath, 0o755); err != nil {
		t.Fatalf("Chmod(fake adb) error = %v", err)
	}
	t.Setenv("ADB_LOG", adbLogPath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-1":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Release{
				ID:        "release-1",
				AppID:     "com.example.app",
				RuntimeID: "runtime-1",
				Version:   "1.2.3+45",
				Platform:  "android",
				Arch:      "arm64-v8a",
				Channel:   "stable",
				CreatedAt: time.Now(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-1/artifact":
			w.Header().Set("Content-Disposition", `attachment; filename="playstore-base.apk"`)
			_, _ = w.Write(releaseArtifactBytes)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patch-check":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.PatchCheckResponse{})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPreviewAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--release-id", "release-1",
			"--install",
			"--launch",
			"--adb", fakeADBPath,
			"--device-id", "emulator-5554",
		})
		if err != nil {
			t.Fatalf("runPreviewAndroid() error = %v", err)
		}
	})

	adbLogBytes, err := os.ReadFile(adbLogPath)
	if err != nil {
		t.Fatalf("ReadFile(adb log) error = %v", err)
	}
	adbLog := string(adbLogBytes)
	if !strings.Contains(adbLog, "-s emulator-5554 install -r ") {
		t.Fatalf("expected install adb command, got %q", adbLog)
	}
	if !strings.Contains(adbLog, "-s emulator-5554 shell monkey -p com.example.app -c android.intent.category.LAUNCHER 1") {
		t.Fatalf("expected launch adb command, got %q", adbLog)
	}
	if !strings.Contains(stdout, "installed: yes") || !strings.Contains(stdout, "launched: yes") {
		t.Fatalf("expected install/launch output, got %q", stdout)
	}
}

func TestRunPreviewAndroidInstallsHostedAABWithBundletool(t *testing.T) {
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "test-token")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "owner@example.com")

	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	releaseArtifactPath := filepath.Join(t.TempDir(), "base.aab")
	writeArtifactZip(t, releaseArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	releaseArtifactBytes, err := os.ReadFile(releaseArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile(release artifact) error = %v", err)
	}
	bundletoolLogPath := filepath.Join(t.TempDir(), "bundletool.log")
	fakeBundletoolPath := filepath.Join(t.TempDir(), "bundletool")
	writeFile(t, fakeBundletoolPath, `#!/bin/sh
printf '%s\n' "$*" >> "$BUNDLETOOL_LOG"
for arg in "$@"; do
  case "$arg" in
    --output=*) output="${arg#--output=}" ;;
  esac
done
if [ -n "$output" ]; then
  mkdir -p "$(dirname "$output")"
  touch "$output"
fi
`)
	if err := os.Chmod(fakeBundletoolPath, 0o755); err != nil {
		t.Fatalf("Chmod(fake bundletool) error = %v", err)
	}
	t.Setenv("BUNDLETOOL_LOG", bundletoolLogPath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-1":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Release{
				ID:        "release-1",
				AppID:     "com.example.app",
				RuntimeID: "runtime-1",
				Version:   "1.2.3+45",
				Platform:  "android",
				Arch:      "universal",
				Channel:   "stable",
				CreatedAt: time.Now(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-1/artifact":
			w.Header().Set("Content-Disposition", `attachment; filename="playstore-base.aab"`)
			_, _ = w.Write(releaseArtifactBytes)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patch-check":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.PatchCheckResponse{})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	err = runPreviewAndroid([]string{
		"--project-dir", projectDir,
		"--api", server.URL,
		"--release-id", "release-1",
		"--install",
		"--bundletool", fakeBundletoolPath,
		"--device-id", "emulator-5554",
	})
	if err != nil {
		t.Fatalf("runPreviewAndroid() error = %v", err)
	}
	bundletoolLogBytes, err := os.ReadFile(bundletoolLogPath)
	if err != nil {
		t.Fatalf("ReadFile(bundletool log) error = %v", err)
	}
	bundletoolLog := string(bundletoolLogBytes)
	if !strings.Contains(bundletoolLog, "build-apks --bundle=") || !strings.Contains(bundletoolLog, "--mode=universal") {
		t.Fatalf("expected build-apks command, got %q", bundletoolLog)
	}
	if !strings.Contains(bundletoolLog, "install-apks --apks=") || !strings.Contains(bundletoolLog, "--device-id=emulator-5554") {
		t.Fatalf("expected install-apks command, got %q", bundletoolLog)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
