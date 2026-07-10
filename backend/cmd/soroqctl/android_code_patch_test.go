package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareAndroidCodePatchPlanExtractsChangedLibappWorkspace(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "base-libapp-arm64",
		"lib/arm64-v8a/libflutter.so":                     "shared-flutter",
		"lib/x86_64/libapp.so":                            "base-libapp-x86_64",
		"lib/x86_64/libflutter.so":                        "shared-flutter",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "candidate-libapp-arm64",
		"lib/arm64-v8a/libflutter.so":                     "shared-flutter",
		"lib/x86_64/libapp.so":                            "candidate-libapp-x86_64",
		"lib/x86_64/libflutter.so":                        "shared-flutter",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)

	workspaceOut := filepath.Join(tempDir, "workspace")
	plan, err := prepareAndroidCodePatchPlan(androidCodePatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: candidateArtifactPath,
		ReleaseID:             "release-android-1",
		WorkspaceOut:          workspaceOut,
	})
	if err != nil {
		t.Fatalf("prepareAndroidCodePatchPlan() error = %v", err)
	}
	if !plan.Ready {
		t.Fatalf("expected code patch plan to be ready: %#v", plan)
	}
	if len(plan.CodePayloads) != 2 {
		t.Fatalf("expected 2 code payloads, got %#v", plan.CodePayloads)
	}
	for _, payload := range plan.CodePayloads {
		if payload.BaseWorkspacePath == nil || payload.CandidateWorkspacePath == nil {
			t.Fatalf("expected workspace paths on payload: %#v", payload)
		}
		baseBytes, err := os.ReadFile(*payload.BaseWorkspacePath)
		if err != nil {
			t.Fatalf("os.ReadFile(base workspace) error = %v", err)
		}
		candidateBytes, err := os.ReadFile(*payload.CandidateWorkspacePath)
		if err != nil {
			t.Fatalf("os.ReadFile(candidate workspace) error = %v", err)
		}
		if string(baseBytes) == string(candidateBytes) {
			t.Fatalf("expected extracted payload bytes to differ for %#v", payload)
		}
	}
}

func TestPrepareAndroidCodePatchPlanCarriesAOTLinkMetadata(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	baseLinkMetadataPath := filepath.Join(tempDir, "base-link.tsv")
	candidateLinkMetadataPath := filepath.Join(tempDir, "candidate-link.tsv")
	baseLinkMetadata := []byte("schema_version\tsnapshot\n1\tisolate\n")
	candidateLinkMetadata := []byte("schema_version\tsnapshot\n1\tisolate\n")
	if err := os.WriteFile(baseLinkMetadataPath, baseLinkMetadata, 0o644); err != nil {
		t.Fatalf("WriteFile(base link metadata) error = %v", err)
	}
	if err := os.WriteFile(candidateLinkMetadataPath, candidateLinkMetadata, 0o644); err != nil {
		t.Fatalf("WriteFile(candidate link metadata) error = %v", err)
	}
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "base-libapp",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "candidate-libapp",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	candidateSnapshot, err := captureAndroidReleaseSnapshot(candidateArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(candidate) error = %v", err)
	}
	baseSnapshot.AOTLinkMetadata = []androidAOTLinkMetadataDescriptor{{
		Snapshot:  "isolate",
		Path:      baseLinkMetadataPath,
		Source:    "release_retained",
		SHA256:    sha256Hex(baseLinkMetadata),
		SizeBytes: uint64(len(baseLinkMetadata)),
	}}
	candidateSnapshot.AOTLinkMetadata = []androidAOTLinkMetadataDescriptor{{
		Snapshot:  "isolate",
		Path:      candidateLinkMetadataPath,
		Source:    "candidate_build",
		SHA256:    sha256Hex(candidateLinkMetadata),
		SizeBytes: uint64(len(candidateLinkMetadata)),
	}}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	candidateSnapshotPath := filepath.Join(tempDir, "candidate.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)
	writeTestJSONFile(t, candidateSnapshotPath, candidateSnapshot)

	plan, err := prepareAndroidCodePatchPlan(androidCodePatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateSnapshotPath: candidateSnapshotPath,
		ReleaseID:             "release-android-1",
		WorkspaceOut:          filepath.Join(tempDir, "workspace"),
	})
	if err != nil {
		t.Fatalf("prepareAndroidCodePatchPlan() error = %v", err)
	}
	if !plan.Ready {
		t.Fatalf("expected code patch plan to be ready: %#v", plan)
	}
	if len(plan.BaseAOTLinkMetadata) != 1 || plan.BaseAOTLinkMetadata[0].SHA256 != sha256Hex(baseLinkMetadata) {
		t.Fatalf("expected base AOT link metadata to carry through: %#v", plan.BaseAOTLinkMetadata)
	}
	if len(plan.CandidateAOTLinkMetadata) != 1 || plan.CandidateAOTLinkMetadata[0].Source != "candidate_build" {
		t.Fatalf("expected candidate AOT link metadata to carry through: %#v", plan.CandidateAOTLinkMetadata)
	}
}

func TestPrepareAndroidCodePatchPlanRejectsNonLibappNativeDrift(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
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
		"lib/arm64-v8a/libflutter.so":                     "candidate-flutter-drift",
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
		ReleaseID:             "release-android-1",
		WorkspaceOut:          filepath.Join(tempDir, "workspace"),
	})
	if err != nil {
		t.Fatalf("prepareAndroidCodePatchPlan() error = %v", err)
	}
	if plan.Ready {
		t.Fatalf("expected code patch plan to be blocked: %#v", plan)
	}
	blockerIDs := make([]string, 0, len(plan.Blockers))
	for _, blocker := range plan.Blockers {
		blockerIDs = append(blockerIDs, blocker.ID)
	}
	joined := strings.Join(blockerIDs, ",")
	if !strings.Contains(joined, "blocked_native_drift") {
		t.Fatalf("expected blocked_native_drift blocker, got %q", joined)
	}
}

func TestRunPrepareAndroidCodePatchPlanStrictFailsWhenBlocked(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
	writeTestAndroidArtifact(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "base-libapp",
		"lib/arm64-v8a/libflutter.so":                     "shared-flutter",
	})
	writeTestAndroidArtifact(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "base-libapp",
		"lib/arm64-v8a/libflutter.so":                     "shared-flutter",
	})

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)

	outputPath := filepath.Join(tempDir, "code-plan.json")
	err = runPrepareAndroidCodePatchPlan([]string{
		"--base-snapshot", baseSnapshotPath,
		"--candidate-artifact", candidateArtifactPath,
		"--workspace-out", filepath.Join(tempDir, "workspace"),
		"--out", outputPath,
		"--strict",
	})
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected strict mode error, got %v", err)
	}
}
