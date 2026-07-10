package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"soroq/backend/internal/domain"
)

func TestRunPatchAndroidPublishesHostedAssetPatch(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	baseArtifactPath := filepath.Join(t.TempDir(), "base.apk")
	candidateArtifactPath := filepath.Join(t.TempDir(), "candidate.apk")
	baseMetadata := testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")
	writeArtifactZip(t, baseArtifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"assets/flutter_assets/assets/patch_probe.txt":    []byte("bundled-base-v1"),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	writeArtifactZip(t, candidateArtifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"assets/flutter_assets/assets/patch_probe.txt":    []byte("patched-asset:public-v1"),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})

	var (
		capturedCreate domain.CreatePatchRequest
		uploadedBundle []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:             capturedCreate.ID,
				AppID:          capturedCreate.AppID,
				ReleaseID:      capturedCreate.ReleaseID,
				RuntimeID:      capturedCreate.RuntimeID,
				Number:         7,
				Channel:        capturedCreate.Channel,
				Track:          capturedCreate.Track,
				Kind:           capturedCreate.Kind,
				ActivationMode: capturedCreate.ActivationMode,
				BundleURL:      capturedCreate.BundleURL,
				ManifestURL:    capturedCreate.ManifestURL,
				RolloutPercent: capturedCreate.RolloutPercent,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches/patch-1/bundle":
			bytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(bundle upload) error = %v", err)
			}
			uploadedBundle = bytes
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--base-artifact", baseArtifactPath,
			"--candidate-artifact", candidateArtifactPath,
			"--release-id", "release-1",
			"--patch-id", "patch-1",
			"--track", "beta",
			"--rollout", "25",
		})
		if err != nil {
			t.Fatalf("runPatchAndroid() error = %v", err)
		}
	})

	if capturedCreate.ID != "patch-1" {
		t.Fatalf("expected patch id patch-1, got %q", capturedCreate.ID)
	}
	if capturedCreate.ReleaseID != "release-1" {
		t.Fatalf("expected release id release-1, got %q", capturedCreate.ReleaseID)
	}
	if capturedCreate.RuntimeID != "runtime-1" {
		t.Fatalf("expected runtime id runtime-1, got %q", capturedCreate.RuntimeID)
	}
	if capturedCreate.Track != "beta" {
		t.Fatalf("expected beta track, got %q", capturedCreate.Track)
	}
	if capturedCreate.RolloutPercent != 25 {
		t.Fatalf("expected rollout 25, got %d", capturedCreate.RolloutPercent)
	}
	if capturedCreate.Kind != domain.PatchKindAsset {
		t.Fatalf("expected asset kind, got %q", capturedCreate.Kind)
	}
	if capturedCreate.BundleURL != server.URL+"/v1/patches/patch-1/bundle" {
		t.Fatalf("unexpected bundle url %q", capturedCreate.BundleURL)
	}
	if len(uploadedBundle) == 0 {
		t.Fatalf("expected uploaded bundle bytes")
	}

	manifest, overlayFiles := parseUploadedBundle(t, uploadedBundle)
	if manifest.PatchID != "patch-1" {
		t.Fatalf("expected patch id in manifest, got %q", manifest.PatchID)
	}
	if manifest.PatchNumber != 7 {
		t.Fatalf("expected patch number 7 in manifest, got %d", manifest.PatchNumber)
	}
	if manifest.ReleaseID != "release-1" {
		t.Fatalf("expected release id in manifest, got %q", manifest.ReleaseID)
	}
	if manifest.Kind != domain.PatchKindAsset {
		t.Fatalf("expected asset manifest kind, got %q", manifest.Kind)
	}
	if got := string(overlayFiles["assets/patch_probe.txt"]); got != "patched-asset:public-v1" {
		t.Fatalf("unexpected overlay payload %q", got)
	}
	if !strings.Contains(stdout, "Published Android asset patch patch-1") {
		t.Fatalf("expected publish headline, got %q", stdout)
	}
	if !strings.Contains(stdout, "overlay_files: 1") {
		t.Fatalf("expected overlay count, got %q", stdout)
	}
}

func TestRunPatchAndroidDefaultsFromRecordedRelease(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	releaseCandidatesDir := filepath.Join(projectDir, "release-candidates")
	if err := os.MkdirAll(releaseCandidatesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	baseArtifactPath := filepath.Join(releaseCandidatesDir, "base.aab")
	candidateArtifactPath := filepath.Join(releaseCandidatesDir, "candidate.aab")
	baseMetadata := testBundledMetadataJSON("com.example.app", "play-internal", "runtime-1", "1.2.3+45")
	writeArtifactZip(t, baseArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("base"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	writeArtifactZip(t, candidateArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("patched"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	futureTime := time.Now().Add(time.Minute)
	if err := os.Chtimes(candidateArtifactPath, futureTime, futureTime); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	if err := saveProjectCLIState(projectDir, projectCLIState{
		SchemaVersion: 1,
		LastAndroidRelease: &androidReleaseState{
			UpdatedAt:            time.Now().UTC(),
			APIBase:              "https://example.test",
			AppID:                "com.example.app",
			Channel:              "play-internal",
			ReleaseID:            "release-1",
			RuntimeID:            "runtime-1",
			Version:              "1.2.3+45",
			Arch:                 "universal",
			ArtifactPath:         baseArtifactPath,
			ManifestSigningKeyID: "prod-primary",
		},
	}); err != nil {
		t.Fatalf("saveProjectCLIState() error = %v", err)
	}

	var (
		capturedCreate domain.CreatePatchRequest
		uploadedBundle []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:                   capturedCreate.ID,
				AppID:                capturedCreate.AppID,
				ReleaseID:            capturedCreate.ReleaseID,
				RuntimeID:            capturedCreate.RuntimeID,
				Number:               9,
				Channel:              capturedCreate.Channel,
				Kind:                 capturedCreate.Kind,
				ActivationMode:       capturedCreate.ActivationMode,
				BundleURL:            capturedCreate.BundleURL,
				ManifestURL:          capturedCreate.ManifestURL,
				RolloutPercent:       capturedCreate.RolloutPercent,
				ManifestSigningKeyID: capturedCreate.ManifestSigningKeyID,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/bundle"):
			bytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(bundle upload) error = %v", err)
			}
			uploadedBundle = bytes
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--patch-id", "patch-1",
			"--build=false",
		})
		if err != nil {
			t.Fatalf("runPatchAndroid() error = %v", err)
		}
	})

	if capturedCreate.ReleaseID != "release-1" {
		t.Fatalf("expected release id from state, got %q", capturedCreate.ReleaseID)
	}
	if capturedCreate.Channel != "play-internal" {
		t.Fatalf("expected channel from state, got %q", capturedCreate.Channel)
	}
	if capturedCreate.ManifestSigningKeyID != "prod-primary" {
		t.Fatalf("expected manifest key from state, got %q", capturedCreate.ManifestSigningKeyID)
	}
	if len(uploadedBundle) == 0 {
		t.Fatalf("expected uploaded bundle")
	}
	_, overlayFiles := parseUploadedBundle(t, uploadedBundle)
	if got := string(overlayFiles["assets/patch_probe.txt"]); got != "patched" {
		t.Fatalf("unexpected overlay payload %q", got)
	}
	if !strings.Contains(stdout, "base_artifact: "+baseArtifactPath) {
		t.Fatalf("expected base artifact in stdout, got %q", stdout)
	}
	if !strings.Contains(stdout, "candidate_artifact: "+candidateArtifactPath) {
		t.Fatalf("expected candidate artifact in stdout, got %q", stdout)
	}
}

func TestRunPatchAndroidDownloadsHostedBaseArtifactWhenLocalStateIsMissing(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	releaseCandidatesDir := filepath.Join(projectDir, "release-candidates")
	if err := os.MkdirAll(releaseCandidatesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	baseArtifactPath := filepath.Join(t.TempDir(), "hosted-base.aab")
	candidateArtifactPath := filepath.Join(releaseCandidatesDir, "candidate.aab")
	baseMetadata := testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")
	writeArtifactZip(t, baseArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("base"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	writeArtifactZip(t, candidateArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("patched-from-hosted-base"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	futureTime := time.Now().Add(time.Minute)
	if err := os.Chtimes(candidateArtifactPath, futureTime, futureTime); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	missingLocalBase := filepath.Join(projectDir, ".soroq", "releases", "release-1", "missing.aab")
	if err := saveProjectCLIState(projectDir, projectCLIState{
		SchemaVersion: 1,
		LastAndroidRelease: &androidReleaseState{
			UpdatedAt:            time.Now().UTC(),
			APIBase:              "https://example.test",
			AppID:                "com.example.app",
			Channel:              "stable",
			ReleaseID:            "release-1",
			RuntimeID:            "runtime-1",
			Version:              "1.2.3+45",
			Arch:                 "universal",
			ArtifactPath:         missingLocalBase,
			ManifestSigningKeyID: "prod-primary",
		},
	}); err != nil {
		t.Fatalf("saveProjectCLIState() error = %v", err)
	}

	baseBytes, err := os.ReadFile(baseArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile(base artifact) error = %v", err)
	}
	var (
		capturedCreate domain.CreatePatchRequest
		uploadedBundle []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-1/artifact":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="hosted-base.aab"`)
			_, _ = w.Write(baseBytes)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:                   capturedCreate.ID,
				AppID:                capturedCreate.AppID,
				ReleaseID:            capturedCreate.ReleaseID,
				RuntimeID:            capturedCreate.RuntimeID,
				Number:               11,
				Channel:              capturedCreate.Channel,
				Kind:                 capturedCreate.Kind,
				ActivationMode:       capturedCreate.ActivationMode,
				BundleURL:            capturedCreate.BundleURL,
				ManifestURL:          capturedCreate.ManifestURL,
				RolloutPercent:       capturedCreate.RolloutPercent,
				ManifestSigningKeyID: capturedCreate.ManifestSigningKeyID,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/bundle"):
			bytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(bundle upload) error = %v", err)
			}
			uploadedBundle = bytes
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--patch-id", "patch-1",
			"--build=false",
		})
		if err != nil {
			t.Fatalf("runPatchAndroid() error = %v", err)
		}
	})

	downloadedPath := filepath.Join(projectDir, ".soroq", "releases", "release-1", "hosted-base.aab")
	if !strings.Contains(stdout, "base_artifact: "+downloadedPath) {
		t.Fatalf("expected downloaded base artifact in stdout, got %q", stdout)
	}
	downloadedBytes, err := os.ReadFile(downloadedPath)
	if err != nil {
		t.Fatalf("ReadFile(downloaded base) error = %v", err)
	}
	if !bytes.Equal(downloadedBytes, baseBytes) {
		t.Fatalf("expected downloaded base artifact bytes to match hosted artifact")
	}
	if len(uploadedBundle) == 0 {
		t.Fatalf("expected uploaded patch bundle")
	}
	_, overlayFiles := parseUploadedBundle(t, uploadedBundle)
	if got := string(overlayFiles["assets/patch_probe.txt"]); got != "patched-from-hosted-base" {
		t.Fatalf("unexpected overlay payload %q", got)
	}
}

func TestRunPatchAndroidSelectsLatestHostedReleaseVersion(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	releaseCandidatesDir := filepath.Join(projectDir, "release-candidates")
	if err := os.MkdirAll(releaseCandidatesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	baseArtifactPath := filepath.Join(t.TempDir(), "hosted-base.aab")
	candidateArtifactPath := filepath.Join(releaseCandidatesDir, "candidate.aab")
	baseMetadata := testBundledMetadataJSON("com.example.app", "stable", "runtime-new", "1.0.2+3")
	writeArtifactZip(t, baseArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("base"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	writeArtifactZip(t, candidateArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("patched-latest"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	futureTime := time.Now().Add(time.Minute)
	if err := os.Chtimes(candidateArtifactPath, futureTime, futureTime); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	baseBytes, err := os.ReadFile(baseArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile(base artifact) error = %v", err)
	}

	var capturedCreate domain.CreatePatchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases":
			if r.URL.Query().Get("app_id") != "com.example.app" {
				t.Fatalf("expected app_id filter, got %q", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]domain.Release{
				{
					ID:        "release-old",
					AppID:     "com.example.app",
					RuntimeID: "runtime-old",
					Version:   "1.0.1+2",
					Platform:  "android",
					Arch:      "arm64-v8a",
					Channel:   "stable",
					CreatedAt: time.Now().Add(-2 * time.Hour),
				},
				{
					ID:                   "release-new",
					AppID:                "com.example.app",
					RuntimeID:            "runtime-new",
					Version:              "1.0.2+3",
					Platform:             "android",
					Arch:                 "arm64-v8a",
					Channel:              "stable",
					ManifestSigningKeyID: "prod-primary",
					CreatedAt:            time.Now().Add(-time.Hour),
				},
			}); err != nil {
				t.Fatalf("Encode(releases) error = %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-new/artifact":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="hosted-base.aab"`)
			_, _ = w.Write(baseBytes)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:                   capturedCreate.ID,
				AppID:                capturedCreate.AppID,
				ReleaseID:            capturedCreate.ReleaseID,
				RuntimeID:            capturedCreate.RuntimeID,
				Number:               13,
				Channel:              capturedCreate.Channel,
				Kind:                 capturedCreate.Kind,
				ActivationMode:       capturedCreate.ActivationMode,
				BundleURL:            capturedCreate.BundleURL,
				ManifestURL:          capturedCreate.ManifestURL,
				RolloutPercent:       capturedCreate.RolloutPercent,
				ManifestSigningKeyID: capturedCreate.ManifestSigningKeyID,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/bundle"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--release-version", "latest",
			"--patch-id", "patch-1",
			"--build=false",
		})
		if err != nil {
			t.Fatalf("runPatchAndroid() error = %v", err)
		}
	})

	if capturedCreate.ReleaseID != "release-new" {
		t.Fatalf("expected latest release id, got %q", capturedCreate.ReleaseID)
	}
	if capturedCreate.RuntimeID != "runtime-new" {
		t.Fatalf("expected runtime from hosted base, got %q", capturedCreate.RuntimeID)
	}
	if capturedCreate.ManifestSigningKeyID != "prod-primary" {
		t.Fatalf("expected manifest key from selected release, got %q", capturedCreate.ManifestSigningKeyID)
	}
	if !strings.Contains(stdout, "release_id: release-new") {
		t.Fatalf("expected selected release in stdout, got %q", stdout)
	}
}

func TestRunPatchAndroidInfersHostedReleaseFromCandidateVersionWithoutLocalState(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	baseArtifactPath := filepath.Join(t.TempDir(), "hosted-base.aab")
	candidateArtifactPath := filepath.Join(t.TempDir(), "candidate.aab")
	baseMetadata := testBundledMetadataJSON("com.example.app", "stable", "runtime-2", "2.0.0+7")
	writeArtifactZip(t, baseArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("base"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	writeArtifactZip(t, candidateArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("patched-by-inferred-version"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	baseBytes, err := os.ReadFile(baseArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile(base artifact) error = %v", err)
	}

	var capturedCreate domain.CreatePatchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]domain.Release{
				{
					ID:        "release-other",
					AppID:     "com.example.app",
					RuntimeID: "runtime-other",
					Version:   "1.9.9+6",
					Platform:  "android",
					Arch:      "arm64-v8a",
					Channel:   "stable",
					CreatedAt: time.Now().Add(-2 * time.Hour),
				},
				{
					ID:                   "release-target",
					AppID:                "com.example.app",
					RuntimeID:            "runtime-2",
					Version:              "2.0.0+7",
					Platform:             "android",
					Arch:                 "arm64-v8a",
					Channel:              "stable",
					ManifestSigningKeyID: "prod-primary",
					CreatedAt:            time.Now().Add(-time.Hour),
				},
			}); err != nil {
				t.Fatalf("Encode(releases) error = %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-target/artifact":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="hosted-base.aab"`)
			_, _ = w.Write(baseBytes)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:                   capturedCreate.ID,
				AppID:                capturedCreate.AppID,
				ReleaseID:            capturedCreate.ReleaseID,
				RuntimeID:            capturedCreate.RuntimeID,
				Number:               14,
				Channel:              capturedCreate.Channel,
				Kind:                 capturedCreate.Kind,
				ActivationMode:       capturedCreate.ActivationMode,
				BundleURL:            capturedCreate.BundleURL,
				ManifestURL:          capturedCreate.ManifestURL,
				RolloutPercent:       capturedCreate.RolloutPercent,
				ManifestSigningKeyID: capturedCreate.ManifestSigningKeyID,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/bundle"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--candidate-artifact", candidateArtifactPath,
			"--patch-id", "patch-1",
			"--build=false",
		})
		if err != nil {
			t.Fatalf("runPatchAndroid() error = %v", err)
		}
	})

	if capturedCreate.ReleaseID != "release-target" {
		t.Fatalf("expected inferred release id, got %q", capturedCreate.ReleaseID)
	}
	if capturedCreate.ManifestSigningKeyID != "prod-primary" {
		t.Fatalf("expected manifest key from inferred release, got %q", capturedCreate.ManifestSigningKeyID)
	}
	if !strings.Contains(stdout, "release_id: release-target") {
		t.Fatalf("expected inferred release in stdout, got %q", stdout)
	}
}

func TestRunPatchPromoteUpdatesRollout(t *testing.T) {
	var captured domain.UpdatePatchRolloutRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/patches/patch-1/rollout" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode(rollout request) error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Patch{
			ID:             "patch-1",
			AppID:          "com.example.app",
			ReleaseID:      "release-1",
			RuntimeID:      "runtime-1",
			Number:         4,
			Channel:        "stable",
			RolloutPercent: captured.RolloutPercent,
		}); err != nil {
			t.Fatalf("Encode(patch response) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatch([]string{
			"promote",
			"--api", server.URL,
			"--patch-id", "patch-1",
		})
		if err != nil {
			t.Fatalf("runPatch(promote) error = %v", err)
		}
	})

	if captured.RolloutPercent != 100 {
		t.Fatalf("expected promote rollout 100, got %d", captured.RolloutPercent)
	}
	if !strings.Contains(stdout, "Promoted patch patch-1 to stable rollout") {
		t.Fatalf("expected promote headline, got %q", stdout)
	}
	if !strings.Contains(stdout, "rollout_percent: 100") {
		t.Fatalf("expected rollout output, got %q", stdout)
	}
}

func TestRunPatchRolloutUpdatesPercent(t *testing.T) {
	var captured domain.UpdatePatchRolloutRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/patches/patch-1/rollout" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode(rollout request) error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Patch{
			ID:             "patch-1",
			AppID:          "com.example.app",
			ReleaseID:      "release-1",
			RuntimeID:      "runtime-1",
			Number:         4,
			Channel:        "stable",
			RolloutPercent: captured.RolloutPercent,
		}); err != nil {
			t.Fatalf("Encode(patch response) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatch([]string{
			"rollout",
			"--api", server.URL,
			"--patch-id", "patch-1",
			"--percent", "25",
		})
		if err != nil {
			t.Fatalf("runPatch(rollout) error = %v", err)
		}
	})

	if captured.RolloutPercent != 25 {
		t.Fatalf("expected rollout 25, got %d", captured.RolloutPercent)
	}
	if !strings.Contains(stdout, "Updated patch patch-1 rollout") {
		t.Fatalf("expected rollout headline, got %q", stdout)
	}
	if !strings.Contains(stdout, "rollout_percent: 25") {
		t.Fatalf("expected rollout output, got %q", stdout)
	}
}

func TestRunPatchAndroidRequiresCandidateUnlessAllowEmpty(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	baseArtifactPath := filepath.Join(t.TempDir(), "base.aab")
	baseMetadata := testBundledMetadataJSON("com.example.app", "play-internal", "runtime-1", "1.2.3+45")
	writeArtifactZip(t, baseArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("base"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
	})
	if err := saveProjectCLIState(projectDir, projectCLIState{
		SchemaVersion: 1,
		LastAndroidRelease: &androidReleaseState{
			UpdatedAt:            time.Now().UTC(),
			APIBase:              "https://example.test",
			AppID:                "com.example.app",
			Channel:              "play-internal",
			ReleaseID:            "release-1",
			RuntimeID:            "runtime-1",
			Version:              "1.2.3+45",
			Arch:                 "universal",
			ArtifactPath:         baseArtifactPath,
			ManifestSigningKeyID: "prod-primary",
		},
	}); err != nil {
		t.Fatalf("saveProjectCLIState() error = %v", err)
	}

	err := runPatchAndroid([]string{
		"--project-dir", projectDir,
		"--patch-id", "empty-patch",
		"--build=false",
	})
	if err == nil || !strings.Contains(err.Error(), "no compatible candidate Android artifact found") {
		t.Fatalf("expected explicit no-candidate error, got %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			var captured domain.CreatePatchRequest
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:             captured.ID,
				AppID:          captured.AppID,
				ReleaseID:      captured.ReleaseID,
				RuntimeID:      captured.RuntimeID,
				Number:         10,
				Channel:        captured.Channel,
				Kind:           captured.Kind,
				ActivationMode: captured.ActivationMode,
				BundleURL:      captured.BundleURL,
				ManifestURL:    captured.ManifestURL,
				RolloutPercent: captured.RolloutPercent,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/bundle"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--patch-id", "empty-patch",
			"--build=false",
			"--allow-empty",
		})
		if err != nil {
			t.Fatalf("runPatchAndroid() error = %v", err)
		}
	})

	if !strings.Contains(stdout, "overlay_files: 0") {
		t.Fatalf("expected empty overlay count, got %q", stdout)
	}
	if !strings.Contains(stdout, "empty_patch: yes") {
		t.Fatalf("expected empty patch marker, got %q", stdout)
	}
}

func TestRunPatchAndroidAutoPublishesHostedCodePatchWhenLibappChanges(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	baseArtifactPath := filepath.Join(t.TempDir(), "base.apk")
	candidateArtifactPath := filepath.Join(t.TempDir(), "candidate.apk")
	baseMetadata := testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")
	writeArtifactZip(t, baseArtifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"assets/flutter_assets/assets/patch_probe.txt":    []byte("same-asset"),
		"lib/arm64-v8a/libapp.so":                         []byte("compiled-dart-code: Sample dashboard title"),
	})
	writeArtifactZip(t, candidateArtifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"assets/flutter_assets/assets/patch_probe.txt":    []byte("same-asset"),
		"lib/arm64-v8a/libapp.so":                         []byte("compiled-dart-code: Soroq dashboard title"),
	})

	var (
		capturedCreate domain.CreatePatchRequest
		uploadedBundle []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:             capturedCreate.ID,
				AppID:          capturedCreate.AppID,
				ReleaseID:      capturedCreate.ReleaseID,
				RuntimeID:      capturedCreate.RuntimeID,
				Number:         11,
				Channel:        capturedCreate.Channel,
				Kind:           capturedCreate.Kind,
				ActivationMode: capturedCreate.ActivationMode,
				BundleURL:      capturedCreate.BundleURL,
				ManifestURL:    capturedCreate.ManifestURL,
				RolloutPercent: capturedCreate.RolloutPercent,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches/code-patch/bundle":
			bytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(bundle upload) error = %v", err)
			}
			uploadedBundle = bytes
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--base-artifact", baseArtifactPath,
			"--candidate-artifact", candidateArtifactPath,
			"--release-id", "release-1",
			"--patch-id", "code-patch",
		})
		if err != nil {
			t.Fatalf("runPatchAndroid() error = %v", err)
		}
	})

	if capturedCreate.Kind != domain.PatchKindExperimentalNativeAOT {
		t.Fatalf("expected experimental_native_aot kind, got %q", capturedCreate.Kind)
	}
	if len(uploadedBundle) == 0 {
		t.Fatalf("expected uploaded code bundle")
	}
	manifest, artifactBytes := parseUploadedBundleArtifact(t, uploadedBundle)
	if manifest.Kind != domain.PatchKindExperimentalNativeAOT {
		t.Fatalf("expected code manifest kind, got %q", manifest.Kind)
	}
	if manifest.Artifact.SizeBytes != uint64(len(artifactBytes)) {
		t.Fatalf("expected artifact size %d, got %d", len(artifactBytes), manifest.Artifact.SizeBytes)
	}

	artifactReader, err := zip.NewReader(bytes.NewReader(artifactBytes), int64(len(artifactBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader(artifact.bin) error = %v", err)
	}
	artifactEntries := map[string][]byte{}
	for _, file := range artifactReader.File {
		artifactEntries[file.Name], err = readZipEntry(t, file)
		if err != nil {
			t.Fatalf("readZipEntry(%q) error = %v", file.Name, err)
		}
	}
	if _, ok := artifactEntries["metadata.json"]; !ok {
		t.Fatalf("expected metadata.json in code artifact")
	}
	deltaBytes := artifactEntries["deltas/lib/arm64-v8a/libapp.so.sqd"]
	if !bytes.HasPrefix(deltaBytes, []byte("SRQCDL15")) {
		previewLen := len(deltaBytes)
		if previewLen > 8 {
			previewLen = 8
		}
		t.Fatalf("expected SRQCDL15 delta, got %q", deltaBytes[:previewLen])
	}
	if !strings.Contains(stdout, "Published Android code patch code-patch") {
		t.Fatalf("expected code publish headline, got %q", stdout)
	}
	if !strings.Contains(stdout, "code_payloads: 1") {
		t.Fatalf("expected code payload count, got %q", stdout)
	}
	if !strings.Contains(stdout, "activation: clients download the patch in the background") {
		t.Fatalf("expected activation guidance, got %q", stdout)
	}
	if !strings.Contains(stdout, "soroq patch health --patch-id code-patch") {
		t.Fatalf("expected patch health next step, got %q", stdout)
	}
}

func TestRunPatchAndroidDefaultsDiscoverCodeCandidateWhenLibappChanges(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	releaseCandidatesDir := filepath.Join(projectDir, "release-candidates")
	if err := os.MkdirAll(releaseCandidatesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	baseArtifactPath := filepath.Join(releaseCandidatesDir, "base.apk")
	candidateArtifactPath := filepath.Join(releaseCandidatesDir, "candidate.apk")
	baseMetadata := testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")
	writeArtifactZip(t, baseArtifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"assets/flutter_assets/assets/patch_probe.txt":    []byte("same-asset"),
		"lib/arm64-v8a/libapp.so":                         []byte("compiled-dart-code: Sample dashboard title"),
	})
	writeArtifactZip(t, candidateArtifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"assets/flutter_assets/assets/patch_probe.txt":    []byte("same-asset"),
		"lib/arm64-v8a/libapp.so":                         []byte("compiled-dart-code: Soroq dashboard title"),
	})
	futureTime := time.Now().Add(time.Minute)
	if err := os.Chtimes(candidateArtifactPath, futureTime, futureTime); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	if err := saveProjectCLIState(projectDir, projectCLIState{
		SchemaVersion: 1,
		LastAndroidRelease: &androidReleaseState{
			UpdatedAt:            time.Now().UTC(),
			APIBase:              "https://example.test",
			AppID:                "com.example.app",
			Channel:              "stable",
			ReleaseID:            "release-1",
			RuntimeID:            "runtime-1",
			Version:              "1.2.3+45",
			Arch:                 "arm64-v8a",
			ArtifactPath:         baseArtifactPath,
			ManifestSigningKeyID: "prod-primary",
		},
	}); err != nil {
		t.Fatalf("saveProjectCLIState() error = %v", err)
	}

	var (
		capturedCreate domain.CreatePatchRequest
		uploadedBundle []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:                   capturedCreate.ID,
				AppID:                capturedCreate.AppID,
				ReleaseID:            capturedCreate.ReleaseID,
				RuntimeID:            capturedCreate.RuntimeID,
				Number:               12,
				Channel:              capturedCreate.Channel,
				Kind:                 capturedCreate.Kind,
				ActivationMode:       capturedCreate.ActivationMode,
				BundleURL:            capturedCreate.BundleURL,
				ManifestURL:          capturedCreate.ManifestURL,
				RolloutPercent:       capturedCreate.RolloutPercent,
				ManifestSigningKeyID: capturedCreate.ManifestSigningKeyID,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/bundle"):
			bytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(bundle upload) error = %v", err)
			}
			uploadedBundle = bytes
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	state, err := loadProjectCLIState(projectDir)
	if err != nil {
		t.Fatalf("loadProjectCLIState() error = %v", err)
	}
	state.LastAndroidRelease.APIBase = server.URL
	if err := saveProjectCLIState(projectDir, state); err != nil {
		t.Fatalf("saveProjectCLIState(server URL) error = %v", err)
	}

	stdout := captureStdout(t, func() {
		err := runPatchAndroid([]string{
			"--project-dir", projectDir,
			"--patch-id", "code-patch",
			"--kind", "code",
			"--build=false",
		})
		if err != nil {
			t.Fatalf("runPatchAndroid() error = %v", err)
		}
	})

	if capturedCreate.Kind != domain.PatchKindExperimentalNativeAOT {
		t.Fatalf("expected experimental_native_aot kind, got %q", capturedCreate.Kind)
	}
	if capturedCreate.ReleaseID != "release-1" {
		t.Fatalf("expected release id from state, got %q", capturedCreate.ReleaseID)
	}
	if capturedCreate.ManifestSigningKeyID != "prod-primary" {
		t.Fatalf("expected manifest key from state, got %q", capturedCreate.ManifestSigningKeyID)
	}
	if len(uploadedBundle) == 0 {
		t.Fatalf("expected uploaded code bundle")
	}
	if !strings.Contains(stdout, "candidate_artifact: "+candidateArtifactPath) {
		t.Fatalf("expected discovered code candidate in stdout, got %q", stdout)
	}
	if !strings.Contains(stdout, "code_payloads: 1") {
		t.Fatalf("expected code payload count, got %q", stdout)
	}
}

func TestRunPatchAndroidUsesSameBuildOutputAfterRebuildWhenReleaseStateIsLegacy(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	buildOutputDir := filepath.Join(projectDir, "build", "app", "outputs", "bundle", "release")
	if err := os.MkdirAll(buildOutputDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(build output) error = %v", err)
	}
	baseArtifactPath := filepath.Join(buildOutputDir, "app-release.aab")
	candidateTemplatePath := filepath.Join(t.TempDir(), "candidate.aab")
	baseMetadata := testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")
	writeArtifactZip(t, baseArtifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("same-asset"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("compiled-dart-code: Example dashboard title"),
	})
	writeArtifactZip(t, candidateTemplatePath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("same-asset"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("compiled-dart-code: Updated dashboard title"),
	})

	scriptsDir := filepath.Join(projectDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}
	writeFile(t, filepath.Join(scriptsDir, "build_soroq_local_engine_aab.sh"), `#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$APP_DIR/build/app/outputs/bundle/release"
cp "$SOROQ_TEST_CANDIDATE_AAB" "$APP_DIR/build/app/outputs/bundle/release/app-release.aab"
`)
	t.Setenv("SOROQ_TEST_CANDIDATE_AAB", candidateTemplatePath)

	var (
		capturedCreate domain.CreatePatchRequest
		uploadedBundle []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:                   capturedCreate.ID,
				AppID:                capturedCreate.AppID,
				ReleaseID:            capturedCreate.ReleaseID,
				RuntimeID:            capturedCreate.RuntimeID,
				Number:               15,
				Channel:              capturedCreate.Channel,
				Kind:                 capturedCreate.Kind,
				ActivationMode:       capturedCreate.ActivationMode,
				BundleURL:            capturedCreate.BundleURL,
				ManifestURL:          capturedCreate.ManifestURL,
				RolloutPercent:       capturedCreate.RolloutPercent,
				ManifestSigningKeyID: capturedCreate.ManifestSigningKeyID,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/bundle"):
			bytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(bundle upload) error = %v", err)
			}
			uploadedBundle = bytes
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	if err := saveProjectCLIState(projectDir, projectCLIState{
		SchemaVersion: 1,
		LastAndroidRelease: &androidReleaseState{
			UpdatedAt:            time.Now().UTC(),
			APIBase:              server.URL,
			AppID:                "com.example.app",
			Channel:              "stable",
			ReleaseID:            "release-1",
			RuntimeID:            "runtime-1",
			Version:              "1.2.3+45",
			Arch:                 "universal",
			ArtifactPath:         baseArtifactPath,
			ManifestSigningKeyID: "prod-primary",
		},
	}); err != nil {
		t.Fatalf("saveProjectCLIState() error = %v", err)
	}

	stdout := captureStdout(t, func() {
		err := runPatchAndroid([]string{
			"--project-dir", projectDir,
			"--patch-id", "same-output-code-patch",
			"--kind", "code",
		})
		if err != nil {
			t.Fatalf("runPatchAndroid() error = %v", err)
		}
	})

	if capturedCreate.Kind != domain.PatchKindExperimentalNativeAOT {
		t.Fatalf("expected experimental_native_aot kind, got %q", capturedCreate.Kind)
	}
	if capturedCreate.ReleaseID != "release-1" {
		t.Fatalf("expected release id from state, got %q", capturedCreate.ReleaseID)
	}
	if len(uploadedBundle) == 0 {
		t.Fatalf("expected uploaded code bundle")
	}
	if !strings.Contains(stdout, "candidate_artifact: "+baseArtifactPath) {
		t.Fatalf("expected same build output candidate in stdout, got %q", stdout)
	}
	if !strings.Contains(stdout, "code_payloads: 1") {
		t.Fatalf("expected code payload count, got %q", stdout)
	}
}

func TestRunPatchAndroidCodeNoChangesFailsBeforePublishWithBlocker(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	baseArtifactPath := filepath.Join(t.TempDir(), "base.aab")
	releaseCandidatesDir := filepath.Join(projectDir, "release-candidates")
	if err := os.MkdirAll(releaseCandidatesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(release-candidates) error = %v", err)
	}
	candidateArtifactPath := filepath.Join(releaseCandidatesDir, "candidate.aab")
	baseMetadata := testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")
	entries := map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(baseMetadata),
		"base/assets/flutter_assets/assets/patch_probe.txt":    []byte("same-asset"),
		"base/lib/arm64-v8a/libapp.so":                         []byte("compiled-dart-code: Example dashboard title"),
	}
	writeArtifactZip(t, baseArtifactPath, entries)
	writeArtifactZip(t, candidateArtifactPath, entries)
	if err := os.Chtimes(candidateArtifactPath, time.Now().Add(time.Minute), time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("Chtimes(candidate) error = %v", err)
	}
	if err := saveProjectCLIState(projectDir, projectCLIState{
		SchemaVersion: 1,
		LastAndroidRelease: &androidReleaseState{
			UpdatedAt:            time.Now().UTC(),
			APIBase:              "https://example.test",
			AppID:                "com.example.app",
			Channel:              "stable",
			ReleaseID:            "release-1",
			RuntimeID:            "runtime-1",
			Version:              "1.2.3+45",
			Arch:                 "universal",
			ArtifactPath:         baseArtifactPath,
			ManifestSigningKeyID: "prod-primary",
		},
	}); err != nil {
		t.Fatalf("saveProjectCLIState() error = %v", err)
	}

	err := runPatchAndroid([]string{
		"--project-dir", projectDir,
		"--kind", "code",
		"--build=false",
	})
	if err == nil {
		t.Fatalf("expected code no-change error")
	}
	if !strings.Contains(err.Error(), "android code patch plan is blocked") {
		t.Fatalf("expected code blocker headline, got %v", err)
	}
	if !strings.Contains(err.Error(), "no_code_payload_changes") {
		t.Fatalf("expected no_code_payload_changes detail, got %v", err)
	}
	if strings.Contains(err.Error(), "plan is not ready") {
		t.Fatalf("expected actionable blocker instead of generic plan-not-ready error, got %v", err)
	}
}

func TestDownloadReleaseArtifactStreamsAndVerifiesSHA(t *testing.T) {
	t.Setenv("SOROQ_OPERATOR_TOKEN", "test-token")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "owner@example.com")

	body := bytes.Repeat([]byte("target-aware-aab-chunk-"), 8192)
	sum := sha256.Sum256(body)
	expectedSHA := fmt.Sprintf("%x", sum[:])
	var sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/releases/release-1/artifact" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		sawAuth = strings.TrimSpace(r.Header.Get("Authorization")) != ""
		w.Header().Set("Content-Disposition", `attachment; filename="base-target-aware.aab"`)
		w.Header().Set("X-Soroq-Artifact-SHA256", expectedSHA)
		w.Header().Set("X-Soroq-Artifact-Size-Bytes", fmt.Sprintf("%d", len(body)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusOK)
		for offset := 0; offset < len(body); offset += 4096 {
			end := offset + 4096
			if end > len(body) {
				end = len(body)
			}
			if _, err := w.Write(body[offset:end]); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
		}
	}))
	defer server.Close()

	projectDir := t.TempDir()
	path, err := downloadReleaseArtifact(server.URL, "release-1", projectDir)
	if err != nil {
		t.Fatalf("downloadReleaseArtifact() error = %v", err)
	}
	if !sawAuth {
		t.Fatalf("expected operator authorization header")
	}
	wantPath := filepath.Join(projectDir, ".soroq", "releases", "release-1", "base-target-aware.aab")
	if path != wantPath {
		t.Fatalf("expected artifact path %s, got %s", wantPath, path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(downloaded artifact) error = %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("downloaded artifact bytes mismatch")
	}
}

func TestRunPatchConfigPublishesHostedConfigPatch(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, configPath, `{
  "feature": "fresh-copy",
  "enabled": true
}`)

	var (
		capturedCreate domain.CreatePatchRequest
		uploadedBundle []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-1":
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
				t.Fatalf("Encode(release response) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:             capturedCreate.ID,
				AppID:          capturedCreate.AppID,
				ReleaseID:      capturedCreate.ReleaseID,
				RuntimeID:      capturedCreate.RuntimeID,
				Number:         8,
				Channel:        capturedCreate.Channel,
				Kind:           capturedCreate.Kind,
				ActivationMode: capturedCreate.ActivationMode,
				BundleURL:      capturedCreate.BundleURL,
				ManifestURL:    capturedCreate.ManifestURL,
				RolloutPercent: capturedCreate.RolloutPercent,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches/config-patch-1/bundle":
			bytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(bundle upload) error = %v", err)
			}
			uploadedBundle = bytes
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchConfig([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--config-file", configPath,
			"--release-id", "release-1",
			"--patch-id", "config-patch-1",
		})
		if err != nil {
			t.Fatalf("runPatchConfig() error = %v", err)
		}
	})

	if capturedCreate.ID != "config-patch-1" {
		t.Fatalf("expected patch id config-patch-1, got %q", capturedCreate.ID)
	}
	if capturedCreate.ReleaseID != "release-1" {
		t.Fatalf("expected release id release-1, got %q", capturedCreate.ReleaseID)
	}
	if capturedCreate.RuntimeID != "runtime-1" {
		t.Fatalf("expected runtime id runtime-1, got %q", capturedCreate.RuntimeID)
	}
	if capturedCreate.Kind != domain.PatchKindConfig {
		t.Fatalf("expected config kind, got %q", capturedCreate.Kind)
	}
	if capturedCreate.ActivationMode != domain.ActivationDownloadOnly {
		t.Fatalf("expected download_only activation, got %q", capturedCreate.ActivationMode)
	}
	if capturedCreate.BundleURL != server.URL+"/v1/patches/config-patch-1/bundle" {
		t.Fatalf("unexpected bundle url %q", capturedCreate.BundleURL)
	}
	if len(uploadedBundle) == 0 {
		t.Fatalf("expected uploaded bundle bytes")
	}

	manifest, artifactBytes := parseUploadedBundleArtifact(t, uploadedBundle)
	if manifest.PatchID != "config-patch-1" {
		t.Fatalf("expected patch id in manifest, got %q", manifest.PatchID)
	}
	if manifest.PatchNumber != 8 {
		t.Fatalf("expected patch number 8 in manifest, got %d", manifest.PatchNumber)
	}
	if manifest.ReleaseID != "release-1" {
		t.Fatalf("expected release id in manifest, got %q", manifest.ReleaseID)
	}
	if manifest.Kind != domain.PatchKindConfig {
		t.Fatalf("expected config manifest kind, got %q", manifest.Kind)
	}
	if manifest.ActivationMode != domain.ActivationDownloadOnly {
		t.Fatalf("expected download_only manifest activation, got %q", manifest.ActivationMode)
	}
	if got := strings.TrimSpace(string(artifactBytes)); got != `{"feature":"fresh-copy","enabled":true}` {
		t.Fatalf("unexpected config payload %q", got)
	}
	if !strings.Contains(stdout, "Published config patch config-patch-1") {
		t.Fatalf("expected publish headline, got %q", stdout)
	}
	if !strings.Contains(stdout, "activation_mode: download_only") {
		t.Fatalf("expected activation mode, got %q", stdout)
	}
}

func TestRunPatchIOSPublishesConfigPatchFromRecordedRelease(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, configPath, `{"headline":"Friend-safe iOS config","enabled":true}`)
	if err := saveProjectCLIState(projectDir, projectCLIState{
		SchemaVersion: 1,
		LastIOSRelease: &iosReleaseState{
			UpdatedAt:            time.Now().UTC(),
			APIBase:              "https://example.test",
			AppID:                "com.example.app",
			Channel:              "stable",
			ReleaseID:            "ios-release-1",
			RuntimeID:            "ios-config-runtime-1",
			Version:              "1.2.3+45",
			Arch:                 "arm64",
			ManifestSigningKeyID: "prod-primary",
		},
	}); err != nil {
		t.Fatalf("saveProjectCLIState() error = %v", err)
	}

	var (
		capturedCreate domain.CreatePatchRequest
		uploadedBundle []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/ios-release-1":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Release{
				ID:                   "ios-release-1",
				AppID:                "com.example.app",
				RuntimeID:            "ios-config-runtime-1",
				Version:              "1.2.3+45",
				Platform:             "ios",
				Arch:                 "arm64",
				Channel:              "stable",
				ManifestSigningKeyID: "prod-primary",
			}); err != nil {
				t.Fatalf("Encode(release) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches":
			if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
				t.Fatalf("Decode(create patch) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Patch{
				ID:                   capturedCreate.ID,
				AppID:                capturedCreate.AppID,
				ReleaseID:            capturedCreate.ReleaseID,
				RuntimeID:            capturedCreate.RuntimeID,
				Number:               11,
				Channel:              capturedCreate.Channel,
				Track:                capturedCreate.Track,
				Kind:                 capturedCreate.Kind,
				ActivationMode:       capturedCreate.ActivationMode,
				ManifestURL:          capturedCreate.ManifestURL,
				BundleURL:            capturedCreate.BundleURL,
				RolloutPercent:       capturedCreate.RolloutPercent,
				ManifestSigningKeyID: "prod-primary",
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches/ios-config-patch/bundle":
			bytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(bundle upload) error = %v", err)
			}
			uploadedBundle = bytes
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchIOS([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--config-file", configPath,
			"--patch-id", "ios-config-patch",
			"--track", "beta",
			"--rollout", "50",
		})
		if err != nil {
			t.Fatalf("runPatchIOS() error = %v", err)
		}
	})

	if capturedCreate.ReleaseID != "ios-release-1" {
		t.Fatalf("expected release id from iOS state, got %q", capturedCreate.ReleaseID)
	}
	if capturedCreate.RuntimeID != "ios-config-runtime-1" {
		t.Fatalf("expected iOS runtime id from release, got %q", capturedCreate.RuntimeID)
	}
	if capturedCreate.Kind != domain.PatchKindConfig {
		t.Fatalf("expected config patch kind, got %q", capturedCreate.Kind)
	}
	if capturedCreate.ActivationMode != domain.ActivationDownloadOnly {
		t.Fatalf("expected download_only activation, got %q", capturedCreate.ActivationMode)
	}
	if capturedCreate.Track != "beta" || capturedCreate.RolloutPercent != 50 {
		t.Fatalf("expected beta rollout 50, got track=%q rollout=%d", capturedCreate.Track, capturedCreate.RolloutPercent)
	}
	if capturedCreate.ManifestSigningKeyID != "" {
		t.Fatalf("expected release default manifest key to be resolved by server, got explicit key %q", capturedCreate.ManifestSigningKeyID)
	}
	if len(uploadedBundle) == 0 {
		t.Fatalf("expected uploaded bundle bytes")
	}
	manifest, artifactBytes := parseUploadedBundleArtifact(t, uploadedBundle)
	if manifest.PatchID != "ios-config-patch" {
		t.Fatalf("expected patch id in manifest, got %q", manifest.PatchID)
	}
	if manifest.ReleaseID != "ios-release-1" {
		t.Fatalf("expected release id in manifest, got %q", manifest.ReleaseID)
	}
	if manifest.RuntimeID != "ios-config-runtime-1" {
		t.Fatalf("expected runtime id in manifest, got %q", manifest.RuntimeID)
	}
	if manifest.Kind != domain.PatchKindConfig {
		t.Fatalf("expected config manifest kind, got %q", manifest.Kind)
	}
	if got := strings.TrimSpace(string(artifactBytes)); got != `{"headline":"Friend-safe iOS config","enabled":true}` {
		t.Fatalf("unexpected config payload %q", got)
	}
	for _, expected := range []string{
		"Published config patch ios-config-patch",
		"ios_support: config_ota_only",
		"signed JSON config/data OTA (no executable code)", // plain config -> ordinary remote config note
		"No native code, dylib, Mach-O, replacement engine, or JIT is downloaded.",
		"verify: `soroq patch status --patch-id ios-config-patch`", // prints the verification command
		"test_url: soroq-ios-config://check",
		"api_base=" + url.QueryEscape(server.URL),
		"release_id=ios-release-1",
		"client_id=ios-config-test",
		"reset_url: soroq-ios-config://reset",
	} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("expected %q in output, got %q", expected, stdout)
		}
	}
}

func TestIOSPatchLaneNoteIsHonestPerPatch(t *testing.T) {
	code := iosPatchLaneNote(true)
	for _, want := range []string{"patch-point lane", "NOT App-Store-safe", "NOT Shorebird parity"} {
		if !strings.Contains(code, want) {
			t.Fatalf("bytecode-carrying note must flag the patch-point lane honestly (%q), got %q", want, code)
		}
	}
	plain := iosPatchLaneNote(false)
	if !strings.Contains(plain, "no executable code") {
		t.Fatalf("plain config note should say no executable code, got %q", plain)
	}
	for _, banned := range []string{"App-Review-safe", "App-Store-safe", "Shorebird parity"} {
		if strings.Contains(plain, banned) {
			t.Fatalf("plain config note must not make a %q claim, got %q", banned, plain)
		}
	}
}

func TestIOSConfigHarnessDeepLinkIncludesResolvedHostedTarget(t *testing.T) {
	link := iosConfigHarnessDeepLink("check", "https://api.example.test/", domain.Release{
		ID:        "ios-release-1",
		AppID:     "com.example.friend",
		RuntimeID: "ios runtime/one",
		Channel:   "stable",
	}, "friend iphone")

	uri, err := url.Parse(link)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", link, err)
	}
	if uri.Scheme != "soroq-ios-config" || uri.Host != "check" {
		t.Fatalf("unexpected deep link target: scheme=%q host=%q link=%q", uri.Scheme, uri.Host, link)
	}
	query := uri.Query()
	for key, expected := range map[string]string{
		"api_base":   "https://api.example.test",
		"app_id":     "com.example.friend",
		"runtime_id": "ios runtime/one",
		"release_id": "ios-release-1",
		"channel":    "stable",
		"client_id":  "friend iphone",
	} {
		if got := query.Get(key); got != expected {
			t.Fatalf("expected %s=%q, got %q in %q", key, expected, got, link)
		}
	}
}

func TestRunPatchIOSRejectsNonIOSRelease(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, configPath, `{"headline":"Wrong release","enabled":false}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/android-release-1":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Release{
				ID:        "android-release-1",
				AppID:     "com.example.app",
				RuntimeID: "android-runtime-1",
				Version:   "1.2.3+45",
				Platform:  "android",
				Arch:      "arm64-v8a",
				Channel:   "stable",
			}); err != nil {
				t.Fatalf("Encode(release) error = %v", err)
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	err := runPatchIOS([]string{
		"--project-dir", projectDir,
		"--api", server.URL,
		"--config-file", configPath,
		"--release-id", "android-release-1",
	})
	if err == nil {
		t.Fatalf("expected non-iOS release error")
	}
	if !strings.Contains(err.Error(), `platform "android", not ios`) {
		t.Fatalf("expected platform guidance, got %v", err)
	}
}

func TestRunPatchHealthPrintsPatchHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/patches/patch-1/health" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.PatchHealth{
			PatchID:      "patch-1",
			PatchNumber:  7,
			SuccessCount: 3,
			FailureCount: 1,
			RolledBack:   false,
		}); err != nil {
			t.Fatalf("Encode(health) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchHealth([]string{
			"--api", server.URL,
			"--patch-id", "patch-1",
		})
		if err != nil {
			t.Fatalf("runPatchHealth() error = %v", err)
		}
	})

	for _, expected := range []string{
		"Patch health patch-1",
		"patch_number: 7",
		"success_count: 3",
		"failure_count: 1",
		"rolled_back: no",
	} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("expected %q in output, got %q", expected, stdout)
		}
	}
}

func TestRunPatchListPrintsPatches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/patches" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("app_id") != "com.example.app" ||
			r.URL.Query().Get("runtime_id") != "runtime-1" ||
			r.URL.Query().Get("channel") != "stable" {
			t.Fatalf("unexpected query %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.Patch{
			{
				ID:             "patch-1",
				AppID:          "com.example.app",
				ReleaseID:      "release-1",
				RuntimeID:      "runtime-1",
				Number:         7,
				Channel:        "stable",
				Kind:           domain.PatchKindAsset,
				ActivationMode: domain.ActivationNextColdStart,
				RolloutPercent: 100,
			},
		}); err != nil {
			t.Fatalf("Encode(patches) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchList([]string{
			"--api", server.URL,
			"--app-id", "com.example.app",
			"--runtime-id", "runtime-1",
			"--channel", "stable",
		})
		if err != nil {
			t.Fatalf("runPatchList() error = %v", err)
		}
	})

	if !strings.Contains(stdout, "Soroq patches: 1") {
		t.Fatalf("expected patch count, got %q", stdout)
	}
	if !strings.Contains(stdout, "patch-1") {
		t.Fatalf("expected patch id, got %q", stdout)
	}
}

func TestRunPatchSetTrackMapsStableToFullRollout(t *testing.T) {
	var captured domain.UpdatePatchTrackRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/patches/patch-1/track" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode(track) error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Patch{
			ID:             "patch-1",
			AppID:          "com.example.app",
			ReleaseID:      "release-1",
			RuntimeID:      "runtime-1",
			Number:         7,
			Channel:        "stable",
			Track:          captured.Track,
			Kind:           domain.PatchKindAsset,
			ActivationMode: domain.ActivationNextColdStart,
			RolloutPercent: captured.RolloutPercent,
		}); err != nil {
			t.Fatalf("Encode(patch) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatch([]string{
			"set-track",
			"--api", server.URL,
			"--patch-id", "patch-1",
			"--track", "stable",
		})
		if err != nil {
			t.Fatalf("runPatch(set-track) error = %v", err)
		}
	})

	if captured.RolloutPercent != 100 {
		t.Fatalf("expected stable track to map to 100 rollout, got %d", captured.RolloutPercent)
	}
	if captured.Track != "stable" {
		t.Fatalf("expected stable track request, got %q", captured.Track)
	}
	if !strings.Contains(stdout, "Set patch patch-1 track to stable") {
		t.Fatalf("expected stable track output, got %q", stdout)
	}
	if !strings.Contains(stdout, "rollout_percent: 100") {
		t.Fatalf("expected rollout percent output, got %q", stdout)
	}
}

func TestRunPatchSetTrackMapsStagingToTrackEndpoint(t *testing.T) {
	var captured domain.UpdatePatchTrackRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/patches/patch-1/track" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode(track) error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Patch{
			ID:             "patch-1",
			AppID:          "com.example.app",
			ReleaseID:      "release-1",
			RuntimeID:      "runtime-1",
			Number:         7,
			Channel:        "stable",
			Track:          captured.Track,
			Kind:           domain.PatchKindAsset,
			ActivationMode: domain.ActivationNextColdStart,
			RolloutPercent: captured.RolloutPercent,
		}); err != nil {
			t.Fatalf("Encode(patch) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatch([]string{
			"set-track",
			"--api", server.URL,
			"--patch-id", "patch-1",
			"--track", "staging",
		})
		if err != nil {
			t.Fatalf("runPatch(set-track) error = %v", err)
		}
	})

	if captured.RolloutPercent != 100 {
		t.Fatalf("expected staging track to default to 100 rollout within that track, got %d", captured.RolloutPercent)
	}
	if captured.Track != "staging" {
		t.Fatalf("expected staging track request, got %q", captured.Track)
	}
	if !strings.Contains(stdout, "Set patch patch-1 track to staging") {
		t.Fatalf("expected staging track output, got %q", stdout)
	}
	if !strings.Contains(stdout, "rollout_percent: 100") {
		t.Fatalf("expected rollout percent output, got %q", stdout)
	}
}

func TestRunPatchStatusPrintsPatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/patches/patch-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Patch{
			ID:             "patch-1",
			AppID:          "com.example.app",
			ReleaseID:      "release-1",
			RuntimeID:      "runtime-1",
			Number:         7,
			Channel:        "stable",
			Kind:           domain.PatchKindAsset,
			ActivationMode: domain.ActivationNextColdStart,
			RolloutPercent: 25,
			RolledBack:     false,
		}); err != nil {
			t.Fatalf("Encode(patch) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runPatchStatus([]string{
			"--api", server.URL,
			"--patch-id", "patch-1",
		})
		if err != nil {
			t.Fatalf("runPatchStatus() error = %v", err)
		}
	})

	for _, expected := range []string{
		"Soroq patch patch-1",
		"patch_number: 7",
		"app_id: com.example.app",
		"release_id: release-1",
		"kind: asset",
		"rollout_percent: 25",
		"rolled_back: no",
	} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("expected %q in output, got %q", expected, stdout)
		}
	}
}

func TestRunPatchAndroidRejectsInvalidChannelBeforeArtifactInspection(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	err := runPatchAndroid([]string{
		"--project-dir", projectDir,
		"--base-artifact", filepath.Join(t.TempDir(), "missing-base.apk"),
		"--candidate-artifact", filepath.Join(t.TempDir(), "missing-candidate.apk"),
		"--release-id", "release-1",
		"--channel", "Canary Track",
	})
	if err == nil {
		t.Fatalf("expected invalid channel error")
	}
	if !strings.Contains(err.Error(), "stable slug") {
		t.Fatalf("expected channel shape guidance, got %v", err)
	}
}

func parseUploadedBundle(t *testing.T, bundleBytes []byte) (domain.PatchManifest, map[string][]byte) {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(bundleBytes), int64(len(bundleBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}

	var manifest domain.PatchManifest
	overlayFiles := map[string][]byte{}
	for _, file := range reader.File {
		bytes, err := readZipEntry(t, file)
		if err != nil {
			t.Fatalf("readZipEntry(%q) error = %v", file.Name, err)
		}
		switch file.Name {
		case "manifest.json":
			if err := json.Unmarshal(bytes, &manifest); err != nil {
				t.Fatalf("json.Unmarshal(manifest) error = %v", err)
			}
		default:
			if strings.HasPrefix(file.Name, "overlay/") {
				overlayFiles[strings.TrimPrefix(file.Name, "overlay/")] = bytes
			}
		}
	}
	return manifest, overlayFiles
}

func parseUploadedBundleArtifact(t *testing.T, bundleBytes []byte) (domain.PatchManifest, []byte) {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(bundleBytes), int64(len(bundleBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}

	var manifest domain.PatchManifest
	var artifactBytes []byte
	for _, file := range reader.File {
		bytes, err := readZipEntry(t, file)
		if err != nil {
			t.Fatalf("readZipEntry(%q) error = %v", file.Name, err)
		}
		switch file.Name {
		case "manifest.json":
			if err := json.Unmarshal(bytes, &manifest); err != nil {
				t.Fatalf("json.Unmarshal(manifest) error = %v", err)
			}
		case "artifact.bin":
			artifactBytes = bytes
		}
	}
	if len(artifactBytes) == 0 {
		t.Fatalf("expected artifact.bin bytes")
	}
	return manifest, artifactBytes
}

func readZipEntry(t *testing.T, file *zip.File) ([]byte, error) {
	t.Helper()
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}
