package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureAndroidReleaseSnapshotFromAPK(t *testing.T) {
	t.Parallel()

	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeTestAndroidArtifact(t, artifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": testBundledMetadataJSON(
			"com.example.soroq",
			"stable",
			"runtime-123",
			"manual",
			"",
		),
		"lib/arm64-v8a/libapp.so":     "app-so-bytes",
		"lib/arm64-v8a/libflutter.so": "flutter-so-bytes",
	})

	snapshot, err := captureAndroidReleaseSnapshot(artifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot() error = %v", err)
	}

	if snapshot.Artifact.Type != "apk" {
		t.Fatalf("expected artifact type apk, got %q", snapshot.Artifact.Type)
	}
	if snapshot.Artifact.BundledMetadataZipPath != "assets/flutter_assets/soroq/soroq_metadata.json" {
		t.Fatalf("unexpected metadata path: %q", snapshot.Artifact.BundledMetadataZipPath)
	}
	if snapshot.Metadata.Soroq.AppID != "com.example.soroq" {
		t.Fatalf("unexpected app id: %q", snapshot.Metadata.Soroq.AppID)
	}
	if snapshot.Metadata.Soroq.RuntimeID != "runtime-123" {
		t.Fatalf("unexpected runtime id: %q", snapshot.Metadata.Soroq.RuntimeID)
	}
	if len(snapshot.NativeLibs) != 2 {
		t.Fatalf("expected 2 native libs, got %d", len(snapshot.NativeLibs))
	}
	if snapshot.NativeLibs[0].Path != "lib/arm64-v8a/libapp.so" {
		t.Fatalf("unexpected first native lib path: %q", snapshot.NativeLibs[0].Path)
	}
}

func TestCaptureAndroidReleaseSnapshotFromAABNormalizesBasePrefix(t *testing.T) {
	t.Parallel()

	artifactPath := filepath.Join(t.TempDir(), "app-release.aab")
	writeTestAndroidArtifact(t, artifactPath, map[string]string{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": testBundledMetadataJSON(
			"com.example.soroq",
			"stable",
			"runtime-456",
			"manifest_trust_v1",
			"trust-fingerprint-1",
		),
		"base/lib/arm64-v8a/libapp.so": "aot-app-so",
	})

	snapshot, err := captureAndroidReleaseSnapshot(artifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot() error = %v", err)
	}

	if snapshot.Artifact.Type != "aab" {
		t.Fatalf("expected artifact type aab, got %q", snapshot.Artifact.Type)
	}
	if snapshot.Artifact.BundledMetadataZipPath != "assets/flutter_assets/soroq/soroq_metadata.json" {
		t.Fatalf("unexpected metadata path: %q", snapshot.Artifact.BundledMetadataZipPath)
	}
	if len(snapshot.NativeLibs) != 1 || snapshot.NativeLibs[0].Path != "lib/arm64-v8a/libapp.so" {
		t.Fatalf("unexpected native libs: %#v", snapshot.NativeLibs)
	}
	if snapshot.Metadata.RuntimeIDStrategy() != "manifest_trust_v1" {
		t.Fatalf("unexpected runtime id strategy: %q", snapshot.Metadata.RuntimeIDStrategy())
	}
}

func TestRunCaptureAndroidReleaseSnapshotRecordsArtifactSource(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	artifactPath := filepath.Join(tempDir, "installed-base.apk")
	outputPath := filepath.Join(tempDir, "snapshot.json")
	writeTestAndroidArtifact(t, artifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": testBundledMetadataJSON(
			"com.example.soroq",
			"stable",
			"runtime-123",
			"manual",
			"",
		),
		"lib/arm64-v8a/libapp.so": "installed-libapp",
	})

	if err := runCaptureAndroidReleaseSnapshot([]string{
		"--artifact", artifactPath,
		"--source", "installed",
		"--out", outputPath,
	}); err != nil {
		t.Fatalf("runCaptureAndroidReleaseSnapshot() error = %v", err)
	}

	snapshot, err := loadAndroidReleaseSnapshot(outputPath)
	if err != nil {
		t.Fatalf("loadAndroidReleaseSnapshot() error = %v", err)
	}
	if snapshot.Artifact.Source != "installed" {
		t.Fatalf("expected installed source, got %q", snapshot.Artifact.Source)
	}
}

func TestRunCaptureAndroidReleaseSnapshotRecordsAOTLinkMetadata(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	artifactPath := filepath.Join(tempDir, "release.apk")
	outputPath := filepath.Join(tempDir, "snapshot.json")
	linkMetadataPath := filepath.Join(tempDir, "link-metadata.tsv")
	linkMetadata := "schema_version\tsnapshot\n1\tisolate\n"
	if err := os.WriteFile(linkMetadataPath, []byte(linkMetadata), 0o644); err != nil {
		t.Fatalf("WriteFile(link metadata) error = %v", err)
	}
	writeTestAndroidArtifact(t, artifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": testBundledMetadataJSON(
			"com.example.soroq",
			"stable",
			"runtime-123",
			"manual",
			"",
		),
		"lib/arm64-v8a/libapp.so": "installed-libapp",
	})

	if err := runCaptureAndroidReleaseSnapshot([]string{
		"--artifact", artifactPath,
		"--aot-link-metadata", linkMetadataPath,
		"--aot-link-metadata-snapshot", "isolate",
		"--aot-link-metadata-source", "release_retained",
		"--out", outputPath,
	}); err != nil {
		t.Fatalf("runCaptureAndroidReleaseSnapshot() error = %v", err)
	}

	snapshot, err := loadAndroidReleaseSnapshot(outputPath)
	if err != nil {
		t.Fatalf("loadAndroidReleaseSnapshot() error = %v", err)
	}
	if len(snapshot.AOTLinkMetadata) != 1 {
		t.Fatalf("expected retained link metadata descriptor, got %#v", snapshot.AOTLinkMetadata)
	}
	descriptor := snapshot.AOTLinkMetadata[0]
	if descriptor.Path != linkMetadataPath {
		t.Fatalf("unexpected retained path: %#v", descriptor)
	}
	if descriptor.SHA256 != sha256Hex([]byte(linkMetadata)) {
		t.Fatalf("unexpected retained sha256: %#v", descriptor)
	}
}

func TestCompareAndroidReleaseSnapshotsDetectsNativeDrift(t *testing.T) {
	t.Parallel()

	basePath := filepath.Join(t.TempDir(), "base.apk")
	candidatePath := filepath.Join(t.TempDir(), "candidate.apk")

	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
	writeTestAndroidArtifact(t, basePath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "base-libapp",
	})
	writeTestAndroidArtifact(t, candidatePath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "candidate-libapp-changed",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(basePath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	candidateSnapshot, err := captureAndroidReleaseSnapshot(candidatePath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(candidate) error = %v", err)
	}

	report := compareAndroidReleaseSnapshots(baseSnapshot, candidateSnapshot)
	if report.Compatible {
		t.Fatalf("expected report to be incompatible: %#v", report)
	}

	foundNativeCheck := false
	for _, check := range report.Checks {
		if check.ID == "native_libraries" {
			foundNativeCheck = true
			if check.Passed {
				t.Fatalf("expected native_libraries check to fail: %#v", check)
			}
		}
	}
	if !foundNativeCheck {
		t.Fatalf("expected native_libraries check in %#v", report.Checks)
	}
}

func TestPrepareAndroidCodePatchPlanStrictBlocksUnknownBaseSource(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "rebuilt-base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "base-libapp",
		"lib/arm64-v8a/libflutter.so":                     "shared-flutter",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "candidate-libapp",
		"lib/arm64-v8a/libflutter.so":                     "shared-flutter",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)

	plan, err := prepareAndroidCodePatchPlan(androidCodePatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: candidateArtifactPath,
		WorkspaceOut:          filepath.Join(tempDir, "workspace"),
		Strict:                true,
	})
	if err != nil {
		t.Fatalf("prepareAndroidCodePatchPlan() error = %v", err)
	}
	if plan.Ready {
		t.Fatalf("expected strict code patch plan to be blocked: %#v", plan)
	}
	if !hasAndroidCodePatchBlocker(plan.Blockers, "base_snapshot_not_exact") {
		t.Fatalf("expected base_snapshot_not_exact blocker, got %#v", plan.Blockers)
	}
}

func TestPrepareAndroidCodePatchPlanStrictAllowsInstalledBaseSource(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "installed-base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "base-libapp",
		"lib/arm64-v8a/libflutter.so":                     "shared-flutter",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "candidate-libapp",
		"lib/arm64-v8a/libflutter.so":                     "shared-flutter",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	baseSnapshot.Artifact.Source = "installed"
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)

	plan, err := prepareAndroidCodePatchPlan(androidCodePatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: candidateArtifactPath,
		WorkspaceOut:          filepath.Join(tempDir, "workspace"),
		Strict:                true,
	})
	if err != nil {
		t.Fatalf("prepareAndroidCodePatchPlan() error = %v", err)
	}
	if !plan.Ready {
		t.Fatalf("expected strict code patch plan to be ready: %#v", plan)
	}
}

func TestPrepareAndroidPatchPlanCapturesCandidateArtifactAndWritesSnapshot(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "base-libapp",
		"lib/arm64-v8a/libflutter.so":                     "flutter-lib",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "base-libapp",
		"lib/arm64-v8a/libflutter.so":                     "flutter-lib",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)

	candidateSnapshotOutPath := filepath.Join(tempDir, "candidate.json")
	plan, err := prepareAndroidPatchPlan(androidPatchPlanOptions{
		BaseSnapshotPath:         baseSnapshotPath,
		CandidateArtifactPath:    candidateArtifactPath,
		CandidateSnapshotOutPath: candidateSnapshotOutPath,
		ReleaseID:                "release-android-1",
	})
	if err != nil {
		t.Fatalf("prepareAndroidPatchPlan() error = %v", err)
	}

	if !plan.Ready {
		t.Fatalf("expected patch plan to be ready: %#v", plan)
	}
	if plan.CandidateSnapshotPath == nil || *plan.CandidateSnapshotPath != candidateSnapshotOutPath {
		t.Fatalf("unexpected candidate snapshot path: %#v", plan.CandidateSnapshotPath)
	}
	if plan.Target.ReleaseID == nil || *plan.Target.ReleaseID != "release-android-1" {
		t.Fatalf("unexpected release id: %#v", plan.Target.ReleaseID)
	}
	if plan.Target.PatchKind != "experimental_native_aot" {
		t.Fatalf("unexpected patch kind: %q", plan.Target.PatchKind)
	}
	if plan.Target.ActivationMode != "next_cold_start" {
		t.Fatalf("unexpected activation mode: %q", plan.Target.ActivationMode)
	}
	if len(plan.Target.ABIs) != 1 || plan.Target.ABIs[0] != "arm64-v8a" {
		t.Fatalf("unexpected ABIs: %#v", plan.Target.ABIs)
	}
	if _, err := loadAndroidReleaseSnapshot(candidateSnapshotOutPath); err != nil {
		t.Fatalf("expected persisted candidate snapshot to load: %v", err)
	}
}

func TestPrepareAndroidPatchPlanReportsBlockersFromIncompatibleCandidate(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", ""),
		"lib/arm64-v8a/libapp.so":                         "shared-lib",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": testBundledMetadataJSON("com.example.soroq", "stable", "runtime-456", "manual", ""),
		"lib/arm64-v8a/libapp.so":                         "candidate-libapp-changed",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	candidateSnapshot, err := captureAndroidReleaseSnapshot(candidateArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(candidate) error = %v", err)
	}

	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	candidateSnapshotPath := filepath.Join(tempDir, "candidate.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)
	writeTestJSONFile(t, candidateSnapshotPath, candidateSnapshot)

	plan, err := prepareAndroidPatchPlan(androidPatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateSnapshotPath: candidateSnapshotPath,
	})
	if err != nil {
		t.Fatalf("prepareAndroidPatchPlan() error = %v", err)
	}
	if plan.Ready {
		t.Fatalf("expected patch plan to be blocked: %#v", plan)
	}

	blockerIDs := make([]string, 0, len(plan.Blockers))
	for _, blocker := range plan.Blockers {
		blockerIDs = append(blockerIDs, blocker.ID)
	}
	joinedBlockers := strings.Join(blockerIDs, ",")
	if !strings.Contains(joinedBlockers, "runtime_id") {
		t.Fatalf("expected runtime_id blocker, got %q", joinedBlockers)
	}
	if !strings.Contains(joinedBlockers, "native_libraries") {
		t.Fatalf("expected native_libraries blocker, got %q", joinedBlockers)
	}
}

func TestRunPrepareAndroidPatchPlanStrictFailsWhenBlocked(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", ""),
		"lib/arm64-v8a/libapp.so":                         "shared-lib",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": testBundledMetadataJSON("com.example.other", "stable", "runtime-123", "manual", ""),
		"lib/arm64-v8a/libapp.so":                         "shared-lib",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	candidateSnapshot, err := captureAndroidReleaseSnapshot(candidateArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(candidate) error = %v", err)
	}

	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	candidateSnapshotPath := filepath.Join(tempDir, "candidate.json")
	outputPath := filepath.Join(tempDir, "plan.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)
	writeTestJSONFile(t, candidateSnapshotPath, candidateSnapshot)

	err = runPrepareAndroidPatchPlan([]string{
		"--base-snapshot", baseSnapshotPath,
		"--candidate-snapshot", candidateSnapshotPath,
		"--out", outputPath,
		"--strict",
	})
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected strict mode error, got %v", err)
	}

	bytes, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", outputPath, readErr)
	}
	var plan androidPatchPlan
	if err := json.Unmarshal(bytes, &plan); err != nil {
		t.Fatalf("json.Unmarshal(plan) error = %v", err)
	}
	if plan.Ready {
		t.Fatalf("expected persisted plan to remain blocked: %#v", plan)
	}
}

func writeTestAndroidArtifact(t *testing.T, path string, files map[string]string) {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create(%q) error = %v", path, err)
	}
	defer file.Close()

	archive := zip.NewWriter(file)
	for name, contents := range files {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatalf("archive.Create(%q) error = %v", name, err)
		}
		if _, err := writer.Write([]byte(contents)); err != nil {
			t.Fatalf("writer.Write(%q) error = %v", name, err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("archive.Close() error = %v", err)
	}
}

func testBundledMetadataJSON(
	appID string,
	channel string,
	runtimeID string,
	runtimeIDStrategy string,
	manifestTrustFingerprint string,
) string {
	payload := map[string]any{
		"schema_version": 1,
		"app": map[string]any{
			"name":         "Example",
			"version":      "1.2.3+45",
			"build_name":   "1.2.3",
			"build_number": "45",
		},
		"soroq": map[string]any{
			"app_id":              appID,
			"channel":             channel,
			"runtime_id":          runtimeID,
			"runtime_id_strategy": runtimeIDStrategy,
		},
	}
	if manifestTrustFingerprint != "" {
		payload["soroq"].(map[string]any)["manifest_trust"] = map[string]any{
			"keyset_version": 1,
			"keys": []map[string]any{
				{
					"id":         "prod-primary",
					"public_key": "BASE64URL_ED25519_PUBLIC_KEY",
				},
			},
		}
		payload["soroq"].(map[string]any)["manifest_trust_fingerprint"] = manifestTrustFingerprint
	}
	bytes, _ := json.Marshal(payload)
	return string(bytes)
}

func writeTestJSONFile(t *testing.T, path string, value any) {
	t.Helper()

	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent() error = %v", err)
	}
	bytes = append(bytes, '\n')
	if err := os.WriteFile(path, bytes, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
}

func hasAndroidCodePatchBlocker(blockers []androidCodePatchBlocker, id string) bool {
	for _, blocker := range blockers {
		if blocker.ID == id {
			return true
		}
	}
	return false
}
