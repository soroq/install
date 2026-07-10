package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	androidpatch "soroq/backend/internal/androidpatch"
	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/domain"
)

type androidReleaseSnapshot = androidrelease.Snapshot
type androidReleaseArtifactDescriptor = androidrelease.ArtifactDescriptor
type androidReleaseEntryDigest = androidrelease.EntryDigest
type androidAOTLinkMetadataDescriptor = androidrelease.AOTLinkMetadataDescriptor
type bundledSoroqMetadata = androidrelease.BundledMetadata
type bundledManifestTrust = androidrelease.ManifestTrust
type bundledManifestTrustKey = androidrelease.ManifestTrustKey
type androidReleaseComparisonReport = androidrelease.ComparisonReport
type androidReleaseSnapshotSummary = androidrelease.SnapshotSummary
type androidReleaseComparisonCheck = androidrelease.ComparisonCheck

type androidPatchPlanOptions = androidpatch.PlanOptions
type androidPatchPlan = androidpatch.Plan
type androidPatchPlanTarget = androidpatch.Target
type androidPatchPlanBlocker = androidpatch.Blocker

func runCaptureAndroidReleaseSnapshot(args []string) error {
	fs := flag.NewFlagSet("capture-android-release-snapshot", flag.ContinueOnError)
	artifactPath := fs.String("artifact", "", "path to Android APK or AAB")
	source := fs.String("source", "", "optional artifact source: installed, release, or rebuilt")
	aotLinkMetadataPath := fs.String("aot-link-metadata", "", "optional retained AOT link metadata TSV path to attach to this release snapshot")
	aotLinkMetadataSnapshot := fs.String("aot-link-metadata-snapshot", "isolate", "snapshot name for --aot-link-metadata")
	aotLinkMetadataSource := fs.String("aot-link-metadata-source", "release_retained", "source label for --aot-link-metadata")
	outputPath := fs.String("out", "", "optional path for snapshot JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*artifactPath) == "" {
		return errors.New("--artifact is required")
	}

	snapshot, err := captureAndroidReleaseSnapshot(*artifactPath)
	if err != nil {
		return err
	}
	if err := setAndroidReleaseSnapshotSource(snapshot, *source); err != nil {
		return err
	}
	if strings.TrimSpace(*aotLinkMetadataPath) != "" {
		if err := androidrelease.AddAOTLinkMetadataFromFile(
			snapshot,
			*aotLinkMetadataPath,
			*aotLinkMetadataSnapshot,
			*aotLinkMetadataSource,
		); err != nil {
			return err
		}
	}
	return writeJSONOutput(snapshot, *outputPath)
}

func runCompareAndroidReleaseSnapshots(args []string) error {
	fs := flag.NewFlagSet("compare-android-release-snapshots", flag.ContinueOnError)
	basePath := fs.String("base", "", "path to base release snapshot JSON")
	candidatePath := fs.String("candidate", "", "path to candidate release snapshot JSON")
	outputPath := fs.String("out", "", "optional path for comparison JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*basePath) == "" || strings.TrimSpace(*candidatePath) == "" {
		return errors.New("--base and --candidate are required")
	}

	baseSnapshot, err := loadAndroidReleaseSnapshot(*basePath)
	if err != nil {
		return fmt.Errorf("load base snapshot: %w", err)
	}
	candidateSnapshot, err := loadAndroidReleaseSnapshot(*candidatePath)
	if err != nil {
		return fmt.Errorf("load candidate snapshot: %w", err)
	}

	report := compareAndroidReleaseSnapshots(baseSnapshot, candidateSnapshot)
	return writeJSONOutput(report, *outputPath)
}

func runPrepareAndroidPatchPlan(args []string) error {
	fs := flag.NewFlagSet("prepare-android-patch-plan", flag.ContinueOnError)
	baseSnapshotPath := fs.String("base-snapshot", "", "path to base release snapshot JSON")
	candidateSnapshotPath := fs.String("candidate-snapshot", "", "path to candidate release snapshot JSON")
	candidateArtifactPath := fs.String("candidate-artifact", "", "path to candidate Android APK or AAB")
	candidateSnapshotOutPath := fs.String("candidate-snapshot-out", "", "optional path to persist a captured candidate snapshot")
	releaseID := fs.String("release-id", "", "optional release id for the planned patch target")
	patchKind := fs.String("patch-kind", string(domain.PatchKindExperimentalNativeAOT), "planned patch kind")
	activationMode := fs.String("activation", string(domain.ActivationNextColdStart), "planned activation mode")
	outputPath := fs.String("out", "", "optional path for patch plan JSON output")
	strict := fs.Bool("strict", false, "return an error when the candidate is not patch-ready")
	if err := fs.Parse(args); err != nil {
		return err
	}

	plan, err := prepareAndroidPatchPlan(androidPatchPlanOptions{
		BaseSnapshotPath:         *baseSnapshotPath,
		CandidateSnapshotPath:    *candidateSnapshotPath,
		CandidateArtifactPath:    *candidateArtifactPath,
		CandidateSnapshotOutPath: *candidateSnapshotOutPath,
		ReleaseID:                *releaseID,
		PatchKind:                *patchKind,
		ActivationMode:           *activationMode,
		Strict:                   *strict,
	})
	if err != nil {
		return err
	}
	if err := writeJSONOutput(plan, *outputPath); err != nil {
		return err
	}
	if plan.Strict && !plan.Ready {
		return errors.New("android patch plan is blocked; inspect blockers")
	}
	return nil
}

func prepareAndroidPatchPlan(options androidPatchPlanOptions) (*androidPatchPlan, error) {
	return androidpatch.PreparePlan(options)
}

func captureAndroidReleaseSnapshot(artifactPath string) (*androidReleaseSnapshot, error) {
	return androidrelease.CaptureSnapshot(artifactPath)
}

func setAndroidReleaseSnapshotSource(snapshot *androidReleaseSnapshot, source string) error {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		return nil
	}
	switch source {
	case "installed", "release", "rebuilt":
		snapshot.Artifact.Source = source
		return nil
	default:
		return fmt.Errorf("unsupported Android artifact source %q; expected installed, release, or rebuilt", source)
	}
}

func normalizeNativeLibraryZipPath(path string) string {
	parts := strings.Split(path, "/")
	for index := range parts {
		if parts[index] != "lib" {
			continue
		}
		if len(parts) <= index+2 {
			return ""
		}
		relative := strings.Join(parts[index:], "/")
		if !strings.HasSuffix(relative, ".so") {
			return ""
		}
		return relative
	}
	return ""
}

func readZipFileBytes(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func sha256Hex(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
}

func loadAndroidReleaseSnapshot(path string) (*androidReleaseSnapshot, error) {
	return androidrelease.LoadSnapshot(path)
}

func compareAndroidReleaseSnapshots(
	base *androidReleaseSnapshot,
	candidate *androidReleaseSnapshot,
) androidReleaseComparisonReport {
	return androidrelease.CompareSnapshots(base, candidate)
}

func sortedMapKeys[T any](items map[string]T) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func normalizedOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func normalizedDefaultString(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func deriveAndroidABIs(snapshot *androidReleaseSnapshot) []string {
	return androidrelease.DeriveABIs(snapshot)
}

func writeJSONOutput(value any, outputPath string) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if strings.TrimSpace(outputPath) == "" {
		fmt.Print(string(encoded))
		return nil
	}
	return os.WriteFile(outputPath, encoded, 0o644)
}
