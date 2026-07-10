package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"soroq/backend/internal/domain"
)

func TestRunReleaseIOSCreatesIOSRelease(t *testing.T) {
	var captured domain.CreateReleaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/releases" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode(release) error = %v", err)
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
			t.Fatalf("Encode(release) error = %v", err)
		}
	}))
	defer server.Close()

	if err := runRelease([]string{
		"ios",
		"--api", server.URL,
		"--release-id", "ios-release-1",
		"--app-id", "demo.app",
		"--runtime-id", "ios-runtime-1",
		"--version", "1.2.3",
	}); err != nil {
		t.Fatalf("runRelease(ios) error = %v", err)
	}

	if captured.ID != "ios-release-1" {
		t.Fatalf("expected release id alias to be used, got %q", captured.ID)
	}
	if captured.Platform != "ios" {
		t.Fatalf("expected platform ios, got %q", captured.Platform)
	}
	if captured.Arch != "arm64" {
		t.Fatalf("expected default arch arm64, got %q", captured.Arch)
	}
	if captured.Channel != "stable" {
		t.Fatalf("expected default channel stable, got %q", captured.Channel)
	}
}

func TestRunPatchIOSCreatesRuntimeManagedDartPatchAndUploadsServerNumberedBundle(t *testing.T) {
	tempDir := t.TempDir()
	baseKernelPath := writeTestIOSKernelBlobForApp(t, tempDir, "base_app", []byte("base-ios-kernel-server-number"))
	kernelPath := writeTestIOSKernelBlobForApp(t, tempDir, "candidate_app", []byte("candidate-ios-kernel-server-number"))
	outPath := filepath.Join(tempDir, "ios-runtime.zip")
	reportPath := filepath.Join(tempDir, "ios-runtime-report.json")
	seedBase64 := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{23}, 32))

	var capturedCreate domain.CreatePatchRequest
	var uploadedBundle []byte
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
				Number:         42,
				Channel:        capturedCreate.Channel,
				Kind:           capturedCreate.Kind,
				ActivationMode: capturedCreate.ActivationMode,
				ManifestURL:    capturedCreate.ManifestURL,
				BundleURL:      capturedCreate.BundleURL,
				RolloutPercent: capturedCreate.RolloutPercent,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches/ios-runtime-patch/bundle":
			bytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(bundle upload) error = %v", err)
			}
			uploadedBundle = bytes
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	if err := runPatch([]string{
		"ios",
		"--api", server.URL,
		"--id", "ios-runtime-patch",
		"--app-id", "demo.app",
		"--release-id", "ios-release-1",
		"--runtime-id", "ios-runtime-1",
		"--kernel-blob", kernelPath,
		"--base-kernel-blob", baseKernelPath,
		"--out", outPath,
		"--report-out", reportPath,
		"--seed-base64", seedBase64,
		"--key-id", "dev-ios-runtime",
	}); err != nil {
		t.Fatalf("runPatch(ios) error = %v", err)
	}

	if capturedCreate.Kind != domain.PatchKindRuntimeManagedDart {
		t.Fatalf("expected runtime_managed_dart kind, got %q", capturedCreate.Kind)
	}
	if capturedCreate.ActivationMode != domain.ActivationNextColdStart {
		t.Fatalf("expected next_cold_start activation, got %q", capturedCreate.ActivationMode)
	}
	if capturedCreate.ManifestURL != server.URL+"/v1/patches/ios-runtime-patch/manifest" {
		t.Fatalf("unexpected manifest URL %q", capturedCreate.ManifestURL)
	}
	if capturedCreate.BundleURL != server.URL+"/v1/patches/ios-runtime-patch/bundle" {
		t.Fatalf("unexpected bundle URL %q", capturedCreate.BundleURL)
	}
	if len(uploadedBundle) == 0 {
		t.Fatalf("expected bundle upload")
	}

	manifest, artifactBytes, _ := parseBuiltPatchBundle(t, uploadedBundle)
	if manifest.PatchNumber != 42 {
		t.Fatalf("expected uploaded bundle manifest patch number from server, got %d", manifest.PatchNumber)
	}
	if manifest.PatchID != "ios-runtime-patch" {
		t.Fatalf("unexpected uploaded bundle patch id %q", manifest.PatchID)
	}
	if manifest.Artifact.URL != server.URL+"/v1/patches/ios-runtime-patch/bundle" {
		t.Fatalf("expected hosted bundle URL in manifest artifact, got %q", manifest.Artifact.URL)
	}
	metadata, payloads, deltas := parseRuntimeManagedDartArtifactWithDeltas(t, artifactBytes)
	if metadata.Format != iosRuntimeManagedDartDeltaFormat {
		t.Fatalf("expected delta artifact format, got %#v", metadata)
	}
	if len(payloads) != 0 {
		t.Fatalf("delta artifact must not upload full payload/app.dill")
	}
	if len(deltas[iosRuntimeManagedDartDeltaPath]) == 0 {
		t.Fatalf("expected uploaded delta bytes")
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("expected local bundle output: %v", err)
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("expected report output: %v", err)
	}
}

func TestRunCreatePatchWithBundleUsesHostedManifestAndBundleEndpoints(t *testing.T) {
	bundlePath := filepath.Join(t.TempDir(), "patch.zip")
	if err := os.WriteFile(bundlePath, []byte("patch-bundle"), 0o644); err != nil {
		t.Fatalf("WriteFile(bundlePath) error = %v", err)
	}

	var capturedCreate domain.CreatePatchRequest
	var uploadedBundle []byte
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
				Number:         1,
				Channel:        capturedCreate.Channel,
				Kind:           capturedCreate.Kind,
				ActivationMode: capturedCreate.ActivationMode,
				ManifestURL:    capturedCreate.ManifestURL,
				BundleURL:      capturedCreate.BundleURL,
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
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	if err := runCreatePatch([]string{
		"--api", server.URL,
		"--id", "patch-1",
		"--app-id", "demo.app",
		"--release-id", "release-1",
		"--runtime-id", "runtime-1",
		"--bundle", bundlePath,
	}); err != nil {
		t.Fatalf("runCreatePatch() error = %v", err)
	}

	if capturedCreate.ManifestURL != server.URL+"/v1/patches/patch-1/manifest" {
		t.Fatalf("unexpected manifest URL %q", capturedCreate.ManifestURL)
	}
	if capturedCreate.BundleURL != server.URL+"/v1/patches/patch-1/bundle" {
		t.Fatalf("unexpected bundle URL %q", capturedCreate.BundleURL)
	}
	if string(uploadedBundle) != "patch-bundle" {
		t.Fatalf("unexpected uploaded bundle %q", string(uploadedBundle))
	}
}

func TestRunCreatePatchWithIOSRuntimeManagedDartBundleSendsKind(t *testing.T) {
	bundlePath := filepath.Join(t.TempDir(), "ios-runtime.zip")
	if err := os.WriteFile(bundlePath, []byte("ios-runtime-bundle"), 0o644); err != nil {
		t.Fatalf("WriteFile(bundlePath) error = %v", err)
	}

	var capturedCreate domain.CreatePatchRequest
	var uploadedBundle []byte
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
				Number:               1,
				Channel:              capturedCreate.Channel,
				Kind:                 capturedCreate.Kind,
				ActivationMode:       capturedCreate.ActivationMode,
				ManifestURL:          capturedCreate.ManifestURL,
				BundleURL:            capturedCreate.BundleURL,
				RolloutPercent:       capturedCreate.RolloutPercent,
				ManifestSigningKeyID: capturedCreate.ManifestSigningKeyID,
			}); err != nil {
				t.Fatalf("Encode(create patch response) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches/ios-runtime-patch/bundle":
			bytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(bundle upload) error = %v", err)
			}
			uploadedBundle = bytes
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	if err := runCreatePatch([]string{
		"--api", server.URL,
		"--id", "ios-runtime-patch",
		"--app-id", "demo.app",
		"--release-id", "ios-release-1",
		"--runtime-id", "ios-runtime-1",
		"--kind", "runtime_managed_dart",
		"--activation", "next_cold_start",
		"--manifest-key-id", "ios-runtime",
		"--bundle", bundlePath,
	}); err != nil {
		t.Fatalf("runCreatePatch() error = %v", err)
	}

	if capturedCreate.Kind != domain.PatchKindRuntimeManagedDart {
		t.Fatalf("expected runtime_managed_dart kind, got %q", capturedCreate.Kind)
	}
	if capturedCreate.ManifestSigningKeyID != "ios-runtime" {
		t.Fatalf("expected manifest signing key id ios-runtime, got %q", capturedCreate.ManifestSigningKeyID)
	}
	if capturedCreate.ManifestURL != server.URL+"/v1/patches/ios-runtime-patch/manifest" {
		t.Fatalf("unexpected manifest URL %q", capturedCreate.ManifestURL)
	}
	if capturedCreate.BundleURL != server.URL+"/v1/patches/ios-runtime-patch/bundle" {
		t.Fatalf("unexpected bundle URL %q", capturedCreate.BundleURL)
	}
	if string(uploadedBundle) != "ios-runtime-bundle" {
		t.Fatalf("unexpected uploaded bundle %q", string(uploadedBundle))
	}
}

func TestRunCreateAppSendsOperatorHeadersFromEnvironment(t *testing.T) {
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "ctl-secret")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "owner@example.com")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ctl-secret" {
			t.Fatalf("expected operator Authorization header, got %q", got)
		}
		if got := r.Header.Get("X-Soroq-Operator-Email"); got != "owner@example.com" {
			t.Fatalf("expected operator email header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.App{
			ID:          "demo.app",
			DisplayName: "Demo",
			OwnerEmail:  "owner@example.com",
		}); err != nil {
			t.Fatalf("Encode(app) error = %v", err)
		}
	}))
	defer server.Close()

	if err := runCreateApp([]string{
		"--api", server.URL,
		"--id", "demo.app",
		"--name", "Demo",
	}); err != nil {
		t.Fatalf("runCreateApp() error = %v", err)
	}
}

func TestRunCreatePatchAcceptsExplicitBundleURL(t *testing.T) {
	var capturedCreate domain.CreatePatchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/patches" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&capturedCreate); err != nil {
			t.Fatalf("Decode(create patch) error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Patch{
			ID:             capturedCreate.ID,
			AppID:          capturedCreate.AppID,
			ReleaseID:      capturedCreate.ReleaseID,
			RuntimeID:      capturedCreate.RuntimeID,
			Number:         1,
			Channel:        capturedCreate.Channel,
			Kind:           capturedCreate.Kind,
			ActivationMode: capturedCreate.ActivationMode,
			ManifestURL:    capturedCreate.ManifestURL,
			BundleURL:      capturedCreate.BundleURL,
			RolloutPercent: capturedCreate.RolloutPercent,
		}); err != nil {
			t.Fatalf("Encode(create patch response) error = %v", err)
		}
	}))
	defer server.Close()

	if err := runCreatePatch([]string{
		"--api", server.URL,
		"--id", "patch-1",
		"--app-id", "demo.app",
		"--release-id", "release-1",
		"--runtime-id", "runtime-1",
		"--manifest-url", "https://cdn.example.com/patch-1.json",
		"--bundle-url", "https://cdn.example.com/patch-1.zip",
	}); err != nil {
		t.Fatalf("runCreatePatch() error = %v", err)
	}

	if capturedCreate.ManifestURL != "https://cdn.example.com/patch-1.json" {
		t.Fatalf("unexpected manifest URL %q", capturedCreate.ManifestURL)
	}
	if capturedCreate.BundleURL != "https://cdn.example.com/patch-1.zip" {
		t.Fatalf("unexpected bundle URL %q", capturedCreate.BundleURL)
	}
}

func TestRunPatchCheckSendsKind(t *testing.T) {
	var captured domain.PatchCheckRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/patch-check" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode(patch check) error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.PatchCheckResponse{
			PatchAvailable:         false,
			RolledBackPatchNumbers: []int{},
		}); err != nil {
			t.Fatalf("Encode(patch check response) error = %v", err)
		}
	}))
	defer server.Close()

	if err := runPatchCheck([]string{
		"--api", server.URL,
		"--app-id", "demo.app",
		"--runtime-id", "runtime-1",
		"--channel", "beta",
		"--current-patch", "7",
		"--client-id", "operator-device",
		"--kind", "config",
	}); err != nil {
		t.Fatalf("runPatchCheck() error = %v", err)
	}

	if captured.AppID != "demo.app" {
		t.Fatalf("expected app id demo.app, got %q", captured.AppID)
	}
	if captured.RuntimeID != "runtime-1" {
		t.Fatalf("expected runtime id runtime-1, got %q", captured.RuntimeID)
	}
	if captured.Channel != "beta" {
		t.Fatalf("expected channel beta, got %q", captured.Channel)
	}
	if captured.CurrentPatchNumber != 7 {
		t.Fatalf("expected current patch 7, got %d", captured.CurrentPatchNumber)
	}
	if captured.ClientID != "operator-device" {
		t.Fatalf("expected client id operator-device, got %q", captured.ClientID)
	}
	if captured.Kind != domain.PatchKindConfig {
		t.Fatalf("expected config kind, got %q", captured.Kind)
	}
}
