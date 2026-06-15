package androidpatch

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/domain"
)

func TestPreparePlanAndBuildAssetPatchBundle(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "1.2.3+45")
	writeArtifactZip(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/assets/patch_probe.txt":    "bundled-base-v1",
		"lib/arm64-v8a/libapp.so":                         "shared-libapp",
	})
	writeArtifactZip(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/assets/patch_probe.txt":    "patched-asset:asset-only-v1",
		"lib/arm64-v8a/libapp.so":                         "shared-libapp",
	})

	baseSnapshot, err := androidrelease.CaptureSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("CaptureSnapshot(base) error = %v", err)
	}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeJSONFile(t, baseSnapshotPath, baseSnapshot)

	plan, err := PreparePlan(PlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: candidateArtifactPath,
		ReleaseID:             "release-android-1",
		PatchKind:             string(domain.PatchKindAsset),
		Strict:                true,
	})
	if err != nil {
		t.Fatalf("PreparePlan() error = %v", err)
	}
	if !plan.Ready {
		t.Fatalf("expected plan to be ready: %#v", plan)
	}

	planPath := filepath.Join(tempDir, "plan.json")
	writeJSONFile(t, planPath, plan)

	report, bundleBytes, err := BuildAssetPatchBundle(AssetPatchBuildOptions{
		PatchPlanPath: planPath,
		PatchID:       "asset-patch-1",
		PatchNumber:   1,
		OutputPath:    filepath.Join(tempDir, "asset-patch.zip"),
		SeedBase64:    base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
		KeyID:         "dev-asset",
	})
	if err != nil {
		t.Fatalf("BuildAssetPatchBundle() error = %v", err)
	}
	if !report.Ready {
		t.Fatalf("expected report to be ready: %#v", report)
	}
	if len(report.OverlayEntries) != 1 || report.OverlayEntries[0].Path != "assets/patch_probe.txt" {
		t.Fatalf("unexpected overlay entries: %#v", report.OverlayEntries)
	}

	manifest, overlayFiles := parseBuiltBundle(t, bundleBytes)
	if manifest.Kind != domain.PatchKindAsset {
		t.Fatalf("expected asset manifest kind, got %q", manifest.Kind)
	}
	if manifest.Signature == nil || *manifest.Signature == "" {
		t.Fatalf("expected manifest signature, got %#v", manifest)
	}
	if got := string(overlayFiles["assets/patch_probe.txt"]); got != "patched-asset:asset-only-v1" {
		t.Fatalf("unexpected overlay payload %q", got)
	}
}

func parseBuiltBundle(t *testing.T, bundleBytes []byte) (domain.PatchManifest, map[string][]byte) {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(bundleBytes), int64(len(bundleBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}

	var manifest domain.PatchManifest
	overlayFiles := map[string][]byte{}
	for _, file := range reader.File {
		bytes, err := readZipBytes(file)
		if err != nil {
			t.Fatalf("readZipBytes(%q) error = %v", file.Name, err)
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

func readZipBytes(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func writeArtifactZip(t *testing.T, path string, entries map[string]string) {
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
		if _, err := entryWriter.Write([]byte(payload)); err != nil {
			t.Fatalf("Write(%s) error = %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()

	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent() error = %v", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%s) error = %v", path, err)
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
