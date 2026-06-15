package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")

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
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")
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

func TestRunPatchAndroidRequiresCandidateUnlessAllowEmpty(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")
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
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")

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
}

func TestRunPatchAndroidDefaultsDiscoverCodeCandidateWhenLibappChanges(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")
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

func TestRunPatchConfigPublishesHostedConfigPatch(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")
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
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: com.example.app\nchannel: stable\n")

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
