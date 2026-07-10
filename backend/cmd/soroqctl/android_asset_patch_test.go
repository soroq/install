package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

func TestBuildAndroidAssetPatchBundleGeneratesSignedOverlayBundle(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/assets/patch_probe.txt":    "bundled-base-v1",
		"lib/arm64-v8a/libapp.so":                         "shared-libapp",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/assets/patch_probe.txt":    "patched-asset:asset-only-v1",
		"lib/arm64-v8a/libapp.so":                         "shared-libapp",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)

	plan, err := prepareAndroidPatchPlan(androidPatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: candidateArtifactPath,
		ReleaseID:             "release-android-1",
		PatchKind:             string(domain.PatchKindAsset),
	})
	if err != nil {
		t.Fatalf("prepareAndroidPatchPlan() error = %v", err)
	}
	if !plan.Ready {
		t.Fatalf("expected patch plan to be ready: %#v", plan)
	}
	planPath := filepath.Join(tempDir, "plan.json")
	writeTestJSONFile(t, planPath, plan)

	report, bundleBytes, err := buildAndroidAssetPatchBundle(androidAssetPatchBuildOptions{
		PatchPlanPath: planPath,
		PatchID:       "asset-patch-1",
		PatchNumber:   1,
		OutputPath:    filepath.Join(tempDir, "asset-patch.zip"),
		SeedBase64:    base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
		KeyID:         "dev-asset",
	})
	if err != nil {
		t.Fatalf("buildAndroidAssetPatchBundle() error = %v", err)
	}
	if !report.Ready {
		t.Fatalf("expected report to be ready: %#v", report)
	}
	if !report.ManifestSigned {
		t.Fatalf("expected manifest signing to be enabled: %#v", report)
	}
	if len(report.OverlayEntries) != 1 || report.OverlayEntries[0].Path != "assets/patch_probe.txt" {
		t.Fatalf("unexpected overlay entries: %#v", report.OverlayEntries)
	}

	manifest, artifactBytes, overlayFiles := parseBuiltPatchBundle(t, bundleBytes)
	if manifest.Kind != domain.PatchKindAsset {
		t.Fatalf("expected asset patch kind, got %q", manifest.Kind)
	}
	if manifest.Signature == nil || strings.TrimSpace(*manifest.Signature) == "" {
		t.Fatalf("expected bundle manifest signature: %#v", manifest)
	}
	if len(artifactBytes) != 0 {
		t.Fatalf("expected empty artifact.bin, got %d bytes", len(artifactBytes))
	}
	if got := string(overlayFiles["assets/patch_probe.txt"]); got != "patched-asset:asset-only-v1" {
		t.Fatalf("unexpected overlay payload: %q", got)
	}
}

func TestBuildAndroidAssetPatchBundleRejectsUnsupportedRuntimeFileChange(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/kernel_blob.bin":           "base-kernel",
		"lib/arm64-v8a/libapp.so":                         "shared-libapp",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/kernel_blob.bin":           "candidate-kernel",
		"lib/arm64-v8a/libapp.so":                         "shared-libapp",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)

	plan, err := prepareAndroidPatchPlan(androidPatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: candidateArtifactPath,
		ReleaseID:             "release-android-1",
		PatchKind:             string(domain.PatchKindAsset),
	})
	if err != nil {
		t.Fatalf("prepareAndroidPatchPlan() error = %v", err)
	}
	planPath := filepath.Join(tempDir, "plan.json")
	writeTestJSONFile(t, planPath, plan)

	report, _, err := buildAndroidAssetPatchBundle(androidAssetPatchBuildOptions{
		PatchPlanPath: planPath,
		PatchID:       "asset-patch-2",
		PatchNumber:   2,
		OutputPath:    filepath.Join(tempDir, "asset-patch.zip"),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported flutter asset changes") {
		t.Fatalf("expected unsupported change error, got %v", err)
	}
	if report == nil || report.Ready {
		t.Fatalf("expected blocked report, got %#v", report)
	}
	if len(report.UnsupportedChanges) != 1 || report.UnsupportedChanges[0].Path != "kernel_blob.bin" {
		t.Fatalf("unexpected unsupported changes: %#v", report.UnsupportedChanges)
	}
}

func TestBuildAndroidAssetPatchBundleCanIgnoreKernelBlobDriftExplicitly(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/kernel_blob.bin":           "base-kernel",
		"assets/flutter_assets/assets/patch_probe.txt":    "bundled-base-v1",
		"lib/arm64-v8a/libapp.so":                         "shared-libapp",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/kernel_blob.bin":           "candidate-kernel",
		"assets/flutter_assets/assets/patch_probe.txt":    "patched-asset:asset-only-v1",
		"lib/arm64-v8a/libapp.so":                         "shared-libapp",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)

	plan, err := prepareAndroidPatchPlan(androidPatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: candidateArtifactPath,
		ReleaseID:             "release-android-1",
		PatchKind:             string(domain.PatchKindAsset),
	})
	if err != nil {
		t.Fatalf("prepareAndroidPatchPlan() error = %v", err)
	}
	planPath := filepath.Join(tempDir, "plan.json")
	writeTestJSONFile(t, planPath, plan)

	report, bundleBytes, err := buildAndroidAssetPatchBundle(androidAssetPatchBuildOptions{
		PatchPlanPath:    planPath,
		PatchID:          "asset-patch-3",
		PatchNumber:      3,
		OutputPath:       filepath.Join(tempDir, "asset-patch.zip"),
		IgnoreKernelBlob: true,
	})
	if err != nil {
		t.Fatalf("buildAndroidAssetPatchBundle() error = %v", err)
	}
	if report == nil || !report.Ready {
		t.Fatalf("expected ready report, got %#v", report)
	}
	if len(report.UnsupportedChanges) != 0 {
		t.Fatalf("expected no unsupported changes, got %#v", report.UnsupportedChanges)
	}
	if len(report.IgnoredChanges) != 2 && len(report.IgnoredChanges) != 1 {
		t.Fatalf("expected ignored changes to include kernel_blob.bin, got %#v", report.IgnoredChanges)
	}
	foundKernelBlob := false
	for _, path := range report.IgnoredChanges {
		if path == "kernel_blob.bin" {
			foundKernelBlob = true
			break
		}
	}
	if !foundKernelBlob {
		t.Fatalf("expected kernel_blob.bin in ignored changes, got %#v", report.IgnoredChanges)
	}
	manifest, artifactBytes, overlayFiles := parseBuiltPatchBundle(t, bundleBytes)
	if manifest.Kind != domain.PatchKindAsset {
		t.Fatalf("expected asset patch kind, got %q", manifest.Kind)
	}
	if len(artifactBytes) != 0 {
		t.Fatalf("expected empty artifact.bin, got %d bytes", len(artifactBytes))
	}
	if got := string(overlayFiles["assets/patch_probe.txt"]); got != "patched-asset:asset-only-v1" {
		t.Fatalf("unexpected overlay payload: %q", got)
	}
}

func parseBuiltPatchBundle(
	t *testing.T,
	bundleBytes []byte,
) (domain.PatchManifest, []byte, map[string][]byte) {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(bundleBytes), int64(len(bundleBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}

	var manifest domain.PatchManifest
	overlayFiles := map[string][]byte{}
	var artifactBytes []byte

	for _, file := range reader.File {
		bytes, err := readZipFileBytes(file)
		if err != nil {
			t.Fatalf("readZipFileBytes(%q) error = %v", file.Name, err)
		}
		switch file.Name {
		case "manifest.json":
			if err := json.Unmarshal(bytes, &manifest); err != nil {
				t.Fatalf("json.Unmarshal(manifest) error = %v", err)
			}
		case "artifact.bin":
			artifactBytes = bytes
		default:
			if strings.HasPrefix(file.Name, "overlay/") {
				overlayFiles[strings.TrimPrefix(file.Name, "overlay/")] = bytes
			}
		}
	}
	return manifest, artifactBytes, overlayFiles
}
