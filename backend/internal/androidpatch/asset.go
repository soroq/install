package androidpatch

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"soroq/backend/internal/domain"
	"soroq/backend/internal/signing"
)

const (
	flutterAssetsZipPrefix           = "assets/flutter_assets/"
	soroqBundledMetadataRelativePath = "soroq/soroq_metadata.json"
)

type AssetPatchBuildOptions struct {
	PatchPlanPath    string
	PatchID          string
	PatchNumber      uint32
	ReleaseID        string
	ArtifactURL      string
	OutputPath       string
	ReportOutPath    string
	SeedBase64       string
	KeyID            string
	AllowEmpty       bool
	IgnoreKernelBlob bool
}

type AssetDiffEntry struct {
	Path      string `json:"path"`
	Change    string `json:"change"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes uint64 `json:"size_bytes,omitempty"`
}

type AssetPatchBundleReport struct {
	SchemaVersion         int              `json:"schema_version"`
	GeneratedAt           time.Time        `json:"generated_at"`
	Ready                 bool             `json:"ready"`
	PatchPlanPath         string           `json:"patch_plan_path"`
	BaseArtifactPath      string           `json:"base_artifact_path"`
	CandidateArtifactPath string           `json:"candidate_artifact_path"`
	BundlePath            *string          `json:"bundle_path,omitempty"`
	BundleSHA256          *string          `json:"bundle_sha256,omitempty"`
	BundleSizeBytes       *uint64          `json:"bundle_size_bytes,omitempty"`
	Target                Target           `json:"target"`
	PatchID               string           `json:"patch_id"`
	PatchNumber           uint32           `json:"patch_number"`
	ReleaseID             string           `json:"release_id"`
	ManifestSigned        bool             `json:"manifest_signed"`
	ManifestKeyID         *string          `json:"manifest_key_id,omitempty"`
	OverlayEntries        []AssetDiffEntry `json:"overlay_entries"`
	RemovedOverlayEntries []string         `json:"removed_overlay_entries,omitempty"`
	UnsupportedChanges    []AssetDiffEntry `json:"unsupported_changes,omitempty"`
	IgnoredChanges        []string         `json:"ignored_changes,omitempty"`
	Notes                 []string         `json:"notes,omitempty"`
}

type artifactFile struct {
	Path      string
	Bytes     []byte
	SHA256    string
	SizeBytes uint64
}

type assetDiffResult struct {
	overlayFiles          map[string]artifactFile
	overlayEntries        []AssetDiffEntry
	removedOverlayEntries []string
	unsupportedChanges    []AssetDiffEntry
	ignoredChanges        []string
}

func BuildAssetPatchBundle(
	options AssetPatchBuildOptions,
) (*AssetPatchBundleReport, []byte, error) {
	planPath := filepath.Clean(options.PatchPlanPath)
	plan, err := LoadPlan(planPath)
	if err != nil {
		return nil, nil, err
	}

	releaseID := strings.TrimSpace(options.ReleaseID)
	if releaseID == "" && plan.Target.ReleaseID != nil {
		releaseID = strings.TrimSpace(*plan.Target.ReleaseID)
	}

	report := &AssetPatchBundleReport{
		SchemaVersion:         1,
		GeneratedAt:           time.Now().UTC(),
		Ready:                 false,
		PatchPlanPath:         planPath,
		BaseArtifactPath:      plan.BaseArtifact.Path,
		CandidateArtifactPath: plan.CandidateArtifact.Path,
		Target:                plan.Target,
		PatchID:               options.PatchID,
		PatchNumber:           options.PatchNumber,
		ReleaseID:             releaseID,
		OverlayEntries:        []AssetDiffEntry{},
	}

	if !plan.Ready {
		report.Notes = append(report.Notes, "patch plan is already blocked; fix the compatibility blockers before generating an asset patch bundle")
		return report, nil, errors.New("android patch plan is not ready")
	}
	if plan.Target.PatchKind != string(domain.PatchKindAsset) {
		report.Notes = append(report.Notes, "this command currently builds asset-only bundles; rerun prepare-android-patch-plan with --patch-kind asset")
		return report, nil, fmt.Errorf("android patch plan target patch_kind must be %q, got %q", domain.PatchKindAsset, plan.Target.PatchKind)
	}
	if releaseID == "" {
		return report, nil, errors.New("release id is required; pass --release-id or include it in the patch plan target")
	}
	if strings.TrimSpace(options.KeyID) != "" && strings.TrimSpace(options.SeedBase64) == "" {
		return report, nil, errors.New("--key-id requires --seed-base64")
	}

	baseAssets, err := readFlutterAssetEntriesFromArtifact(plan.BaseArtifact.Path)
	if err != nil {
		return report, nil, fmt.Errorf("read base flutter assets: %w", err)
	}
	candidateAssets, err := readFlutterAssetEntriesFromArtifact(plan.CandidateArtifact.Path)
	if err != nil {
		return report, nil, fmt.Errorf("read candidate flutter assets: %w", err)
	}

	diff := diffAndroidFlutterAssets(baseAssets, candidateAssets)
	if options.IgnoreKernelBlob {
		diff = ignoreKernelBlobUnsupportedChange(diff)
	}
	report.OverlayEntries = diff.overlayEntries
	report.RemovedOverlayEntries = diff.removedOverlayEntries
	report.UnsupportedChanges = diff.unsupportedChanges
	report.IgnoredChanges = diff.ignoredChanges

	if len(diff.unsupportedChanges) > 0 {
		report.Notes = append(report.Notes, "candidate artifact changed flutter runtime files that are not yet supported by the asset-only generator")
		return report, nil, fmt.Errorf("unsupported flutter asset changes detected (%d)", len(diff.unsupportedChanges))
	}
	if options.IgnoreKernelBlob {
		report.Notes = append(report.Notes, "kernel_blob.bin drift was ignored explicitly for this asset-only proof")
	}
	if len(diff.overlayEntries) == 0 && !options.AllowEmpty {
		report.Notes = append(report.Notes, "no overlay file changes were detected; pass --allow-empty if you intentionally want an empty asset patch")
		return report, nil, errors.New("no overlay asset changes detected")
	}

	artifactURL := strings.TrimSpace(options.ArtifactURL)
	if artifactURL == "" {
		artifactURL = fmt.Sprintf("file://local/%s.bin", options.PatchID)
	}

	artifactBytes := []byte{}
	manifest := domain.PatchManifest{
		PatchID:        options.PatchID,
		PatchNumber:    int(options.PatchNumber),
		RuntimeID:      plan.Target.RuntimeID,
		ReleaseID:      releaseID,
		Channel:        plan.Target.Channel,
		Kind:           domain.PatchKindAsset,
		ActivationMode: domain.ActivationMode(plan.Target.ActivationMode),
		Artifact: domain.PatchArtifact{
			URL:       artifactURL,
			SHA256:    sha256Hex(artifactBytes),
			SizeBytes: uint64(len(artifactBytes)),
		},
	}

	if strings.TrimSpace(options.SeedBase64) != "" {
		signer, err := signing.NewManifestSignerFromSeedBase64(options.SeedBase64, options.KeyID)
		if err != nil {
			return report, nil, fmt.Errorf("create manifest signer: %w", err)
		}
		signature, err := signer.SignManifest(manifest)
		if err != nil {
			return report, nil, fmt.Errorf("sign asset patch manifest: %w", err)
		}
		keyID := signer.KeyID()
		manifest.SignatureKeyID = &keyID
		manifest.Signature = &signature
		report.ManifestSigned = true
		report.ManifestKeyID = &keyID
	}

	bundleBytes, err := buildPatchBundleArchive(manifest, artifactBytes, diff.overlayFiles)
	if err != nil {
		return report, nil, err
	}

	outputPathClean := filepath.Clean(options.OutputPath)
	bundleSHA := sha256Hex(bundleBytes)
	bundleSize := uint64(len(bundleBytes))
	report.BundlePath = &outputPathClean
	report.BundleSHA256 = &bundleSHA
	report.BundleSizeBytes = &bundleSize
	report.Ready = true
	report.Notes = append(report.Notes, fmt.Sprintf("bundle contains %d overlay file(s)", len(diff.overlayEntries)))
	if len(diff.removedOverlayEntries) > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("%d overlay-eligible file(s) were removed from the candidate build and are represented only through updated manifests/metadata", len(diff.removedOverlayEntries)))
	}
	return report, bundleBytes, nil
}

func readFlutterAssetEntriesFromArtifact(artifactPath string) (map[string]artifactFile, error) {
	reader, err := zip.OpenReader(filepath.Clean(artifactPath))
	if err != nil {
		return nil, fmt.Errorf("open Android artifact zip: %w", err)
	}
	defer reader.Close()

	entries := make(map[string]artifactFile)
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		normalizedPath := normalizeFlutterAssetZipPath(file.Name)
		if normalizedPath == "" {
			continue
		}
		bytes, err := readZipFileBytes(file)
		if err != nil {
			return nil, fmt.Errorf("read flutter asset %s: %w", file.Name, err)
		}
		entries[normalizedPath] = artifactFile{
			Path:      normalizedPath,
			Bytes:     bytes,
			SHA256:    sha256Hex(bytes),
			SizeBytes: uint64(len(bytes)),
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("no flutter assets were found in Android artifact")
	}
	return entries, nil
}

func normalizeFlutterAssetZipPath(path string) string {
	path = filepath.ToSlash(filepath.Clean(path))
	if strings.HasPrefix(path, "base/"+flutterAssetsZipPrefix) {
		return strings.TrimPrefix(path, "base/"+flutterAssetsZipPrefix)
	}
	if strings.HasPrefix(path, flutterAssetsZipPrefix) {
		return strings.TrimPrefix(path, flutterAssetsZipPrefix)
	}
	return ""
}

func diffAndroidFlutterAssets(base map[string]artifactFile, candidate map[string]artifactFile) assetDiffResult {
	allPaths := make(map[string]struct{}, len(base)+len(candidate))
	for path := range base {
		allPaths[path] = struct{}{}
	}
	for path := range candidate {
		allPaths[path] = struct{}{}
	}

	sortedPaths := sortedPathKeys(allPaths)
	result := assetDiffResult{
		overlayFiles: map[string]artifactFile{},
	}
	for _, path := range sortedPaths {
		baseEntry, baseOK := base[path]
		candidateEntry, candidateOK := candidate[path]
		if baseOK && candidateOK && baseEntry.SHA256 == candidateEntry.SHA256 && baseEntry.SizeBytes == candidateEntry.SizeBytes {
			continue
		}

		changeKind := "modified"
		switch {
		case !baseOK && candidateOK:
			changeKind = "added"
		case baseOK && !candidateOK:
			changeKind = "removed"
		}

		if isIgnoredFlutterAssetPath(path) {
			result.ignoredChanges = append(result.ignoredChanges, path)
			continue
		}

		entryForReport := baseEntry
		if candidateOK {
			entryForReport = candidateEntry
		}
		diffEntry := AssetDiffEntry{
			Path:      path,
			Change:    changeKind,
			SHA256:    entryForReport.SHA256,
			SizeBytes: entryForReport.SizeBytes,
		}

		if isOverlayEligibleFlutterAssetPath(path) {
			if candidateOK {
				result.overlayFiles[path] = candidateEntry
				result.overlayEntries = append(result.overlayEntries, diffEntry)
			} else {
				result.removedOverlayEntries = append(result.removedOverlayEntries, path)
			}
			continue
		}

		result.unsupportedChanges = append(result.unsupportedChanges, diffEntry)
	}

	sort.Slice(result.overlayEntries, func(i, j int) bool {
		return result.overlayEntries[i].Path < result.overlayEntries[j].Path
	})
	sort.Strings(result.removedOverlayEntries)
	sort.Slice(result.unsupportedChanges, func(i, j int) bool {
		return result.unsupportedChanges[i].Path < result.unsupportedChanges[j].Path
	})
	sort.Strings(result.ignoredChanges)
	return result
}

func isIgnoredFlutterAssetPath(path string) bool {
	return path == soroqBundledMetadataRelativePath
}

func isOverlayEligibleFlutterAssetPath(path string) bool {
	switch path {
	case "AssetManifest.bin", "AssetManifest.bin.json", "AssetManifest.json", "FontManifest.json", "NOTICES.Z":
		return true
	}
	return strings.HasPrefix(path, "assets/") || strings.HasPrefix(path, "packages/")
}

func ignoreKernelBlobUnsupportedChange(diff assetDiffResult) assetDiffResult {
	if len(diff.unsupportedChanges) == 0 {
		return diff
	}

	filteredUnsupported := make([]AssetDiffEntry, 0, len(diff.unsupportedChanges))
	for _, change := range diff.unsupportedChanges {
		if change.Path == "kernel_blob.bin" {
			diff.ignoredChanges = append(diff.ignoredChanges, change.Path)
			continue
		}
		filteredUnsupported = append(filteredUnsupported, change)
	}
	diff.unsupportedChanges = filteredUnsupported
	sort.Strings(diff.ignoredChanges)
	return diff
}

func buildPatchBundleArchive(manifest domain.PatchManifest, artifactBytes []byte, overlayFiles map[string]artifactFile) ([]byte, error) {
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode asset patch manifest: %w", err)
	}

	var output bytes.Buffer
	writer := zip.NewWriter(&output)

	writeEntry := func(path string, bytes []byte) error {
		entry, err := writer.Create(path)
		if err != nil {
			return fmt.Errorf("create patch bundle entry %s: %w", path, err)
		}
		if _, err := entry.Write(bytes); err != nil {
			return fmt.Errorf("write patch bundle entry %s: %w", path, err)
		}
		return nil
	}

	if err := writeEntry("manifest.json", manifestBytes); err != nil {
		return nil, err
	}
	if err := writeEntry("artifact.bin", artifactBytes); err != nil {
		return nil, err
	}

	overlayPaths := sortedOverlayKeys(overlayFiles)
	for _, overlayPath := range overlayPaths {
		entry := overlayFiles[overlayPath]
		if err := writeEntry("overlay/"+entry.Path, entry.Bytes); err != nil {
			return nil, err
		}
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finalize asset patch bundle archive: %w", err)
	}
	return output.Bytes(), nil
}
