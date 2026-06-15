package androidpatch

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kr/binarydist"

	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/domain"
	"soroq/backend/internal/signing"
)

const (
	codeDeltaMagicV15    = "SRQCDL15"
	codeDeltaStrategyV15 = "bsdiff_bzip2_v15"
)

type CodePatchPlanOptions struct {
	BaseSnapshotPath         string
	CandidateSnapshotPath    string
	CandidateArtifactPath    string
	CandidateSnapshotOutPath string
	ReleaseID                string
	ActivationMode           string
	WorkspaceOut             string
	Strict                   bool
}

type CodePatchPlan struct {
	SchemaVersion            int                                        `json:"schema_version"`
	GeneratedAt              time.Time                                  `json:"generated_at"`
	Ready                    bool                                       `json:"ready"`
	Strict                   bool                                       `json:"strict"`
	BaseSnapshotPath         string                                     `json:"base_snapshot_path"`
	CandidateSnapshotPath    *string                                    `json:"candidate_snapshot_path,omitempty"`
	WorkspaceRoot            *string                                    `json:"workspace_root,omitempty"`
	Target                   Target                                     `json:"target"`
	BaseArtifact             androidrelease.ArtifactDescriptor          `json:"base_artifact"`
	CandidateArtifact        androidrelease.ArtifactDescriptor          `json:"candidate_artifact"`
	BaseAOTLinkMetadata      []androidrelease.AOTLinkMetadataDescriptor `json:"base_aot_link_metadata,omitempty"`
	CandidateAOTLinkMetadata []androidrelease.AOTLinkMetadataDescriptor `json:"candidate_aot_link_metadata,omitempty"`
	IdentityChecks           []androidrelease.ComparisonCheck           `json:"identity_checks"`
	CodePayloads             []CodePayload                              `json:"code_payloads"`
	Blockers                 []CodePatchBlocker                         `json:"blockers"`
	Notes                    []string                                   `json:"notes,omitempty"`
}

type CodePayload struct {
	ABI                    string  `json:"abi"`
	Path                   string  `json:"path"`
	BaseSHA256             string  `json:"base_sha256"`
	CandidateSHA256        string  `json:"candidate_sha256"`
	BaseSizeBytes          uint64  `json:"base_size_bytes"`
	CandidateSizeBytes     uint64  `json:"candidate_size_bytes"`
	BaseWorkspacePath      *string `json:"base_workspace_path,omitempty"`
	CandidateWorkspacePath *string `json:"candidate_workspace_path,omitempty"`
}

type CodePatchBlocker struct {
	ID                 string  `json:"id"`
	Path               string  `json:"path,omitempty"`
	Detail             string  `json:"detail"`
	BaseSHA256         *string `json:"base_sha256,omitempty"`
	CandidateSHA256    *string `json:"candidate_sha256,omitempty"`
	BaseSizeBytes      *uint64 `json:"base_size_bytes,omitempty"`
	CandidateSizeBytes *uint64 `json:"candidate_size_bytes,omitempty"`
}

type CodePatchBuildOptions struct {
	CodePlanPath      string
	PatchID           string
	PatchNumber       uint32
	ReleaseID         string
	ArtifactURL       string
	OutputPath        string
	ReportOutPath     string
	SeedBase64        string
	KeyID             string
	CodeDeltaStrategy string
}

type CodePatchBundleReport struct {
	SchemaVersion            int                                        `json:"schema_version"`
	GeneratedAt              time.Time                                  `json:"generated_at"`
	Ready                    bool                                       `json:"ready"`
	CodePlanPath             string                                     `json:"code_plan_path"`
	BaseArtifactPath         string                                     `json:"base_artifact_path"`
	CandidateArtifactPath    string                                     `json:"candidate_artifact_path"`
	BaseAOTLinkMetadata      []androidrelease.AOTLinkMetadataDescriptor `json:"base_aot_link_metadata,omitempty"`
	CandidateAOTLinkMetadata []androidrelease.AOTLinkMetadataDescriptor `json:"candidate_aot_link_metadata,omitempty"`
	BundlePath               *string                                    `json:"bundle_path,omitempty"`
	BundleSHA256             *string                                    `json:"bundle_sha256,omitempty"`
	BundleSizeBytes          *uint64                                    `json:"bundle_size_bytes,omitempty"`
	ArtifactSHA256           *string                                    `json:"artifact_sha256,omitempty"`
	ArtifactSizeBytes        *uint64                                    `json:"artifact_size_bytes,omitempty"`
	Target                   Target                                     `json:"target"`
	PatchID                  string                                     `json:"patch_id"`
	PatchNumber              uint32                                     `json:"patch_number"`
	ReleaseID                string                                     `json:"release_id"`
	ManifestSigned           bool                                       `json:"manifest_signed"`
	ManifestKeyID            *string                                    `json:"manifest_key_id,omitempty"`
	Payloads                 []CodeDeltaPayloadReport                   `json:"payloads"`
	Notes                    []string                                   `json:"notes,omitempty"`
}

type CodeDeltaPayloadReport struct {
	ABI                string `json:"abi"`
	Path               string `json:"path"`
	Strategy           string `json:"strategy"`
	DeltaPath          string `json:"delta_path"`
	BaseSHA256         string `json:"base_sha256"`
	CandidateSHA256    string `json:"candidate_sha256"`
	DeltaSHA256        string `json:"delta_sha256"`
	BaseSizeBytes      uint64 `json:"base_size_bytes"`
	CandidateSizeBytes uint64 `json:"candidate_size_bytes"`
	DeltaSizeBytes     uint64 `json:"delta_size_bytes"`
	OpCount            int    `json:"op_count"`
	CopyOps            int    `json:"copy_ops"`
	InsertOps          int    `json:"insert_ops"`
	CopiedBytes        uint64 `json:"copied_bytes"`
	InsertedBytes      uint64 `json:"inserted_bytes"`
	SkippedBaseBytes   uint64 `json:"skipped_base_bytes"`
	Verified           bool   `json:"verified"`
}

type codeDeltaSummary struct {
	Strategy         string
	OpCount          int
	CopyOps          int
	InsertOps        int
	CopiedBytes      uint64
	InsertedBytes    uint64
	SkippedBaseBytes uint64
}

type codeArtifactMetadata struct {
	SchemaVersion            int                                        `json:"schema_version"`
	GeneratedAt              time.Time                                  `json:"generated_at"`
	Strategy                 string                                     `json:"strategy"`
	BaseAOTLinkMetadata      []androidrelease.AOTLinkMetadataDescriptor `json:"base_aot_link_metadata,omitempty"`
	CandidateAOTLinkMetadata []androidrelease.AOTLinkMetadataDescriptor `json:"candidate_aot_link_metadata,omitempty"`
	Payloads                 []codeArtifactPayloadRecord                `json:"payloads"`
}

type codeArtifactPayloadRecord struct {
	ABI                string `json:"abi"`
	Path               string `json:"path"`
	DeltaPath          string `json:"delta_path"`
	BaseSHA256         string `json:"base_sha256"`
	CandidateSHA256    string `json:"candidate_sha256"`
	DeltaSHA256        string `json:"delta_sha256"`
	BaseSizeBytes      uint64 `json:"base_size_bytes"`
	CandidateSizeBytes uint64 `json:"candidate_size_bytes"`
	DeltaSizeBytes     uint64 `json:"delta_size_bytes"`
	Strategy           string `json:"strategy"`
	OpCount            int    `json:"op_count"`
	CopyOps            int    `json:"copy_ops"`
	InsertOps          int    `json:"insert_ops"`
	CopiedBytes        uint64 `json:"copied_bytes"`
	InsertedBytes      uint64 `json:"inserted_bytes"`
	SkippedBaseBytes   uint64 `json:"skipped_base_bytes"`
}

func PrepareCodePatchPlan(options CodePatchPlanOptions) (*CodePatchPlan, error) {
	if strings.TrimSpace(options.BaseSnapshotPath) == "" {
		return nil, errors.New("--base-snapshot is required")
	}
	if strings.TrimSpace(options.WorkspaceOut) == "" {
		return nil, errors.New("--workspace-out is required")
	}

	hasCandidateSnapshot := strings.TrimSpace(options.CandidateSnapshotPath) != ""
	hasCandidateArtifact := strings.TrimSpace(options.CandidateArtifactPath) != ""
	switch {
	case hasCandidateSnapshot == hasCandidateArtifact:
		return nil, errors.New("exactly one of --candidate-snapshot or --candidate-artifact is required")
	case hasCandidateSnapshot && strings.TrimSpace(options.CandidateSnapshotOutPath) != "":
		return nil, errors.New("--candidate-snapshot-out can only be used with --candidate-artifact")
	}

	baseSnapshotPath := filepath.Clean(options.BaseSnapshotPath)
	baseSnapshot, err := androidrelease.LoadSnapshot(baseSnapshotPath)
	if err != nil {
		return nil, fmt.Errorf("load base snapshot: %w", err)
	}

	var (
		candidateSnapshot     *androidrelease.Snapshot
		candidateSnapshotPath *string
	)
	if hasCandidateSnapshot {
		cleanPath := filepath.Clean(options.CandidateSnapshotPath)
		candidateSnapshot, err = androidrelease.LoadSnapshot(cleanPath)
		if err != nil {
			return nil, fmt.Errorf("load candidate snapshot: %w", err)
		}
		candidateSnapshotPath = &cleanPath
	} else {
		candidateSnapshot, err = androidrelease.CaptureSnapshot(options.CandidateArtifactPath)
		if err != nil {
			return nil, fmt.Errorf("capture candidate snapshot: %w", err)
		}
		if strings.TrimSpace(options.CandidateSnapshotOutPath) != "" {
			cleanPath := filepath.Clean(options.CandidateSnapshotOutPath)
			if err := writeJSONOutput(candidateSnapshot, cleanPath); err != nil {
				return nil, fmt.Errorf("write candidate snapshot: %w", err)
			}
			candidateSnapshotPath = &cleanPath
		}
	}

	comparison := androidrelease.CompareSnapshots(baseSnapshot, candidateSnapshot)
	identityChecks := make([]androidrelease.ComparisonCheck, 0, len(comparison.Checks))
	blockers := make([]CodePatchBlocker, 0)
	if options.Strict && !isExactAndroidBaseSource(baseSnapshot.Artifact.Source) {
		blockers = append(blockers, CodePatchBlocker{
			ID: "base_snapshot_not_exact",
			Detail: fmt.Sprintf(
				"strict Android native code patch planning requires a base snapshot captured from an installed or release artifact; got source=%q",
				normalizedAndroidArtifactSource(baseSnapshot.Artifact.Source),
			),
		})
	}
	for _, check := range comparison.Checks {
		if check.ID == "native_libraries" {
			continue
		}
		identityChecks = append(identityChecks, check)
		if !check.Passed {
			blockers = append(blockers, CodePatchBlocker{
				ID:     check.ID,
				Detail: check.Detail,
			})
		}
	}

	workspaceRoot := filepath.Clean(options.WorkspaceOut)
	workspaceRootPtr := &workspaceRoot
	target := Target{
		Platform:                 "android",
		AppID:                    baseSnapshot.Metadata.Soroq.AppID,
		Channel:                  baseSnapshot.Metadata.Soroq.Channel,
		RuntimeID:                baseSnapshot.Metadata.Soroq.RuntimeID,
		RuntimeIDStrategy:        baseSnapshot.Metadata.RuntimeIDStrategy(),
		Version:                  baseSnapshot.Metadata.App.Version,
		BuildName:                baseSnapshot.Metadata.App.BuildName,
		BuildNumber:              baseSnapshot.Metadata.App.BuildNumber,
		ManifestTrustFingerprint: baseSnapshot.Metadata.Soroq.ManifestTrustFingerprint,
		PatchKind:                string(domain.PatchKindExperimentalNativeAOT),
		ActivationMode:           normalizedDefaultString(options.ActivationMode, string(domain.ActivationNextColdStart)),
		ABIs:                     androidrelease.DeriveABIs(baseSnapshot),
	}
	if trimmedReleaseID := strings.TrimSpace(options.ReleaseID); trimmedReleaseID != "" {
		target.ReleaseID = &trimmedReleaseID
	}

	codePayloads, nativeBlockers, err := extractAndroidCodePayloads(baseSnapshot, candidateSnapshot, workspaceRoot)
	if err != nil {
		return nil, err
	}
	blockers = append(blockers, nativeBlockers...)

	notes := make([]string, 0, 4)
	if candidateSnapshotPath != nil {
		notes = append(notes, "candidate snapshot is persisted and can be reused by later code-diff tooling")
	} else {
		notes = append(notes, "candidate snapshot was captured in-memory from the provided Android artifact")
	}
	if len(codePayloads) == 0 {
		blockers = append(blockers, CodePatchBlocker{
			ID:     "no_code_payload_changes",
			Detail: "no libapp.so changes were detected between the base and candidate artifacts",
		})
	} else {
		notes = append(notes, fmt.Sprintf("extracted %d changed libapp.so payload(s) into the workspace", len(codePayloads)))
	}
	if len(nativeBlockers) > 0 {
		notes = append(notes, "non-libapp native drift is still blocked for code-patch planning")
	} else {
		notes = append(notes, "only libapp.so changed across the compared native payloads")
	}

	ready := len(blockers) == 0
	if !ready {
		notes = append(notes, fmt.Sprintf("code patch planning is currently blocked by %d issue(s)", len(blockers)))
	}

	return &CodePatchPlan{
		SchemaVersion:            1,
		GeneratedAt:              time.Now().UTC(),
		Ready:                    ready,
		Strict:                   options.Strict,
		BaseSnapshotPath:         baseSnapshotPath,
		CandidateSnapshotPath:    candidateSnapshotPath,
		WorkspaceRoot:            workspaceRootPtr,
		Target:                   target,
		BaseArtifact:             baseSnapshot.Artifact,
		CandidateArtifact:        candidateSnapshot.Artifact,
		BaseAOTLinkMetadata:      baseSnapshot.AOTLinkMetadata,
		CandidateAOTLinkMetadata: candidateSnapshot.AOTLinkMetadata,
		IdentityChecks:           identityChecks,
		CodePayloads:             codePayloads,
		Blockers:                 blockers,
		Notes:                    notes,
	}, nil
}

func BuildCodePatchBundle(
	options CodePatchBuildOptions,
) (*CodePatchBundleReport, []byte, error) {
	codePlanPath := filepath.Clean(options.CodePlanPath)
	plan, err := loadCodePatchPlan(codePlanPath)
	if err != nil {
		return nil, nil, err
	}

	releaseID := strings.TrimSpace(options.ReleaseID)
	if releaseID == "" && plan.Target.ReleaseID != nil {
		releaseID = strings.TrimSpace(*plan.Target.ReleaseID)
	}

	report := &CodePatchBundleReport{
		SchemaVersion:            1,
		GeneratedAt:              time.Now().UTC(),
		Ready:                    false,
		CodePlanPath:             codePlanPath,
		BaseArtifactPath:         plan.BaseArtifact.Path,
		CandidateArtifactPath:    plan.CandidateArtifact.Path,
		BaseAOTLinkMetadata:      plan.BaseAOTLinkMetadata,
		CandidateAOTLinkMetadata: plan.CandidateAOTLinkMetadata,
		Target:                   plan.Target,
		PatchID:                  options.PatchID,
		PatchNumber:              options.PatchNumber,
		ReleaseID:                releaseID,
		Payloads:                 []CodeDeltaPayloadReport{},
	}
	if err := ensureCodeDeltaStrategySupported(options.CodeDeltaStrategy); err != nil {
		return report, nil, err
	}
	if !plan.Ready {
		report.Notes = append(report.Notes, "code patch plan is already blocked; fix the compatibility blockers before generating a code patch bundle")
		return report, nil, errors.New("android code patch plan is not ready")
	}
	if domain.NormalizePatchKind(domain.PatchKind(plan.Target.PatchKind)) != domain.PatchKindExperimentalNativeAOT {
		report.Notes = append(report.Notes, "this command currently builds experimental_native_aot bundles only; rerun code patch planning for native AOT payload drift")
		return report, nil, fmt.Errorf("android code patch plan target patch_kind must be %q, got %q", domain.PatchKindExperimentalNativeAOT, plan.Target.PatchKind)
	}
	if releaseID == "" {
		return report, nil, errors.New("release id is required; pass --release-id or include it in the code plan target")
	}
	if len(plan.CodePayloads) == 0 {
		report.Notes = append(report.Notes, "the code plan did not contain any extracted libapp.so payloads")
		return report, nil, errors.New("android code patch plan contains no code payloads")
	}

	artifactRecords := make([]codeArtifactPayloadRecord, 0, len(plan.CodePayloads))
	deltaFiles := make(map[string][]byte, len(plan.CodePayloads))
	for _, payload := range plan.CodePayloads {
		if payload.BaseWorkspacePath == nil || payload.CandidateWorkspacePath == nil {
			return report, nil, fmt.Errorf("code payload %s is missing extracted workspace paths", payload.Path)
		}
		baseBytes, err := os.ReadFile(filepath.Clean(*payload.BaseWorkspacePath))
		if err != nil {
			return report, nil, fmt.Errorf("read base payload %s: %w", payload.Path, err)
		}
		candidateBytes, err := os.ReadFile(filepath.Clean(*payload.CandidateWorkspacePath))
		if err != nil {
			return report, nil, fmt.Errorf("read candidate payload %s: %w", payload.Path, err)
		}

		deltaBytes, summary, err := buildCodeDeltaV15(baseBytes, candidateBytes)
		if err != nil {
			return report, nil, fmt.Errorf("build code delta %s: %w", payload.Path, err)
		}
		reconstructed, err := applyCodeDeltaV15(baseBytes, deltaBytes)
		if err != nil {
			return report, nil, fmt.Errorf("apply code delta %s: %w", payload.Path, err)
		}
		if !bytes.Equal(reconstructed, candidateBytes) {
			return report, nil, fmt.Errorf("delta verification failed for %s", payload.Path)
		}

		deltaPath := filepath.ToSlash(filepath.Join("deltas", payload.Path)) + ".sqd"
		deltaSHA := sha256Hex(deltaBytes)
		deltaSize := uint64(len(deltaBytes))
		report.Payloads = append(report.Payloads, CodeDeltaPayloadReport{
			ABI:                payload.ABI,
			Path:               payload.Path,
			Strategy:           summary.Strategy,
			DeltaPath:          deltaPath,
			BaseSHA256:         payload.BaseSHA256,
			CandidateSHA256:    payload.CandidateSHA256,
			DeltaSHA256:        deltaSHA,
			BaseSizeBytes:      payload.BaseSizeBytes,
			CandidateSizeBytes: payload.CandidateSizeBytes,
			DeltaSizeBytes:     deltaSize,
			OpCount:            summary.OpCount,
			CopyOps:            summary.CopyOps,
			InsertOps:          summary.InsertOps,
			CopiedBytes:        summary.CopiedBytes,
			InsertedBytes:      summary.InsertedBytes,
			SkippedBaseBytes:   summary.SkippedBaseBytes,
			Verified:           true,
		})
		artifactRecords = append(artifactRecords, codeArtifactPayloadRecord{
			ABI:                payload.ABI,
			Path:               payload.Path,
			DeltaPath:          deltaPath,
			BaseSHA256:         payload.BaseSHA256,
			CandidateSHA256:    payload.CandidateSHA256,
			DeltaSHA256:        deltaSHA,
			BaseSizeBytes:      payload.BaseSizeBytes,
			CandidateSizeBytes: payload.CandidateSizeBytes,
			DeltaSizeBytes:     deltaSize,
			Strategy:           summary.Strategy,
			OpCount:            summary.OpCount,
			CopyOps:            summary.CopyOps,
			InsertOps:          summary.InsertOps,
			CopiedBytes:        summary.CopiedBytes,
			InsertedBytes:      summary.InsertedBytes,
			SkippedBaseBytes:   summary.SkippedBaseBytes,
		})
		deltaFiles[deltaPath] = deltaBytes
	}

	artifactBytes, err := buildCodePatchArtifact(
		codeDeltaStrategyV15,
		plan.BaseAOTLinkMetadata,
		plan.CandidateAOTLinkMetadata,
		artifactRecords,
		deltaFiles,
	)
	if err != nil {
		return report, nil, err
	}
	artifactSHA := sha256Hex(artifactBytes)
	artifactSize := uint64(len(artifactBytes))
	report.ArtifactSHA256 = &artifactSHA
	report.ArtifactSizeBytes = &artifactSize

	artifactURL := strings.TrimSpace(options.ArtifactURL)
	if artifactURL == "" {
		artifactURL = fmt.Sprintf("file://local/%s.bin", options.PatchID)
	}

	manifest := domain.PatchManifest{
		PatchID:        options.PatchID,
		PatchNumber:    int(options.PatchNumber),
		RuntimeID:      plan.Target.RuntimeID,
		ReleaseID:      releaseID,
		Channel:        plan.Target.Channel,
		Kind:           domain.PatchKindExperimentalNativeAOT,
		ActivationMode: domain.ActivationMode(plan.Target.ActivationMode),
		Artifact: domain.PatchArtifact{
			URL:       artifactURL,
			SHA256:    artifactSHA,
			SizeBytes: artifactSize,
		},
	}

	if strings.TrimSpace(options.SeedBase64) != "" {
		signer, err := signing.NewManifestSignerFromSeedBase64(options.SeedBase64, options.KeyID)
		if err != nil {
			return report, nil, fmt.Errorf("create manifest signer: %w", err)
		}
		signature, err := signer.SignManifest(manifest)
		if err != nil {
			return report, nil, fmt.Errorf("sign code patch manifest: %w", err)
		}
		keyID := signer.KeyID()
		manifest.SignatureKeyID = &keyID
		manifest.Signature = &signature
		report.ManifestSigned = true
		report.ManifestKeyID = &keyID
	}

	bundleBytes, err := buildPatchBundleArchive(manifest, artifactBytes, nil)
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
	report.Notes = append(report.Notes,
		fmt.Sprintf("bundle contains %d verified per-ABI code delta payload(s)", len(report.Payloads)),
		"artifact.bin carries a code delta archive for release/AOT application",
	)
	if strings.TrimSpace(options.ReportOutPath) != "" {
		if err := writeJSONOutput(report, options.ReportOutPath); err != nil {
			return report, nil, err
		}
	}
	return report, bundleBytes, nil
}

func loadCodePatchPlan(path string) (*CodePatchPlan, error) {
	bytes, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var plan CodePatchPlan
	if err := json.Unmarshal(bytes, &plan); err != nil {
		return nil, err
	}
	if plan.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported android code patch plan schema version %d", plan.SchemaVersion)
	}
	return &plan, nil
}

func extractAndroidCodePayloads(
	base *androidrelease.Snapshot,
	candidate *androidrelease.Snapshot,
	workspaceRoot string,
) ([]CodePayload, []CodePatchBlocker, error) {
	baseFiles, err := readNativeLibraryEntriesFromArtifact(base.Artifact.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("read base native libraries: %w", err)
	}
	candidateFiles, err := readNativeLibraryEntriesFromArtifact(candidate.Artifact.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("read candidate native libraries: %w", err)
	}

	allPaths := make(map[string]struct{}, len(baseFiles)+len(candidateFiles))
	for path := range baseFiles {
		allPaths[path] = struct{}{}
	}
	for path := range candidateFiles {
		allPaths[path] = struct{}{}
	}

	sortedPaths := sortedPathKeys(allPaths)
	payloadCandidates := make([]CodePayload, 0)
	blockers := make([]CodePatchBlocker, 0)
	for _, path := range sortedPaths {
		baseFile, baseOK := baseFiles[path]
		candidateFile, candidateOK := candidateFiles[path]
		if !baseOK || !candidateOK {
			blockers = append(blockers, nativeDriftBlocker(path, baseFile, baseOK, candidateFile, candidateOK, "native library was added or removed"))
			continue
		}
		if baseFile.SHA256 == candidateFile.SHA256 {
			continue
		}
		if !isLibappPath(path) {
			blockers = append(blockers, nativeDriftBlocker(path, baseFile, true, candidateFile, true, "only libapp.so may change in the current code patch lane"))
			continue
		}

		baseWorkspacePath := filepath.Join(workspaceRoot, "base", filepath.FromSlash(path))
		candidateWorkspacePath := filepath.Join(workspaceRoot, "candidate", filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(baseWorkspacePath), 0o755); err != nil {
			return nil, nil, err
		}
		if err := os.MkdirAll(filepath.Dir(candidateWorkspacePath), 0o755); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(baseWorkspacePath, baseFile.Bytes, 0o644); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(candidateWorkspacePath, candidateFile.Bytes, 0o644); err != nil {
			return nil, nil, err
		}

		payloadCandidates = append(payloadCandidates, CodePayload{
			ABI:                    androidABIFromNativePath(path),
			Path:                   path,
			BaseSHA256:             baseFile.SHA256,
			CandidateSHA256:        candidateFile.SHA256,
			BaseSizeBytes:          baseFile.SizeBytes,
			CandidateSizeBytes:     candidateFile.SizeBytes,
			BaseWorkspacePath:      &baseWorkspacePath,
			CandidateWorkspacePath: &candidateWorkspacePath,
		})
	}

	return payloadCandidates, blockers, nil
}

func readNativeLibraryEntriesFromArtifact(artifactPath string) (map[string]artifactFile, error) {
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
		normalizedPath := normalizeNativeLibraryZipPath(file.Name)
		if normalizedPath == "" {
			continue
		}
		bytes, err := readZipFileBytes(file)
		if err != nil {
			return nil, fmt.Errorf("read native library %s: %w", file.Name, err)
		}
		entries[normalizedPath] = artifactFile{
			Path:      normalizedPath,
			Bytes:     bytes,
			SHA256:    sha256Hex(bytes),
			SizeBytes: uint64(len(bytes)),
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("no native libraries were found in Android artifact")
	}
	return entries, nil
}

func normalizeNativeLibraryZipPath(path string) string {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
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

func nativeDriftBlocker(
	path string,
	baseFile artifactFile,
	baseOK bool,
	candidateFile artifactFile,
	candidateOK bool,
	detail string,
) CodePatchBlocker {
	blocker := CodePatchBlocker{
		ID:     "unsupported_native_drift",
		Path:   path,
		Detail: detail,
	}
	if baseOK {
		blocker.BaseSHA256 = &baseFile.SHA256
		blocker.BaseSizeBytes = &baseFile.SizeBytes
	}
	if candidateOK {
		blocker.CandidateSHA256 = &candidateFile.SHA256
		blocker.CandidateSizeBytes = &candidateFile.SizeBytes
	}
	return blocker
}

func isLibappPath(path string) bool {
	return strings.HasSuffix(filepath.ToSlash(path), "/libapp.so")
}

func androidABIFromNativePath(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) >= 3 && parts[0] == "lib" {
		return parts[1]
	}
	return ""
}

func isExactAndroidBaseSource(source string) bool {
	switch normalizedAndroidArtifactSource(source) {
	case "installed", "release":
		return true
	default:
		return false
	}
}

func normalizedAndroidArtifactSource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		return "unknown"
	}
	return source
}

func ensureCodeDeltaStrategySupported(raw string) error {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "default", "v15", codeDeltaStrategyV15:
		return nil
	default:
		return fmt.Errorf("unsupported code delta strategy %q; soroq patch android currently supports default/v15", raw)
	}
}

func buildCodeDeltaV15(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	var bsdiffPatch bytes.Buffer
	if err := binarydist.Diff(bytes.NewReader(baseBytes), bytes.NewReader(candidateBytes), &bsdiffPatch); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("build bsdiff delta: %w", err)
	}
	var output bytes.Buffer
	if _, err := output.Write([]byte(codeDeltaMagicV15)); err != nil {
		return nil, codeDeltaSummary{}, err
	}
	if _, err := output.Write(bsdiffPatch.Bytes()); err != nil {
		return nil, codeDeltaSummary{}, err
	}
	return output.Bytes(), codeDeltaSummary{
		Strategy:      codeDeltaStrategyV15,
		OpCount:       1,
		InsertOps:     1,
		InsertedBytes: uint64(bsdiffPatch.Len()),
	}, nil
}

func applyCodeDeltaV15(baseBytes []byte, deltaBytes []byte) ([]byte, error) {
	if !bytes.HasPrefix(deltaBytes, []byte(codeDeltaMagicV15)) {
		return nil, errors.New("code delta does not use SRQCDL15 magic")
	}
	var reconstructed bytes.Buffer
	if err := binarydist.Patch(bytes.NewReader(baseBytes), &reconstructed, bytes.NewReader(deltaBytes[len(codeDeltaMagicV15):])); err != nil {
		return nil, err
	}
	return reconstructed.Bytes(), nil
}

func buildCodePatchArtifact(
	strategy string,
	baseAOTLinkMetadata []androidrelease.AOTLinkMetadataDescriptor,
	candidateAOTLinkMetadata []androidrelease.AOTLinkMetadataDescriptor,
	records []codeArtifactPayloadRecord,
	deltaFiles map[string][]byte,
) ([]byte, error) {
	metadata := codeArtifactMetadata{
		SchemaVersion:            1,
		GeneratedAt:              time.Now().UTC(),
		Strategy:                 strategy,
		BaseAOTLinkMetadata:      baseAOTLinkMetadata,
		CandidateAOTLinkMetadata: candidateAOTLinkMetadata,
		Payloads:                 records,
	}
	metadataBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, err
	}
	metadataBytes = append(metadataBytes, '\n')

	var output bytes.Buffer
	writer := newBestCompressionZipWriter(&output)
	if err := writeCodeArtifactEntry(writer, "metadata.json", metadataBytes); err != nil {
		_ = writer.Close()
		return nil, err
	}
	deltaPaths := make([]string, 0, len(deltaFiles))
	for path := range deltaFiles {
		deltaPaths = append(deltaPaths, path)
	}
	sort.Strings(deltaPaths)
	for _, path := range deltaPaths {
		if err := writeCodeArtifactEntry(writer, path, deltaFiles[path]); err != nil {
			_ = writer.Close()
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func newBestCompressionZipWriter(output *bytes.Buffer) *zip.Writer {
	writer := zip.NewWriter(output)
	writer.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestCompression)
	})
	return writer
}

func writeCodeArtifactEntry(writer *zip.Writer, name string, bytes []byte) error {
	header := &zip.FileHeader{
		Name:   filepath.ToSlash(name),
		Method: zip.Deflate,
	}
	header.SetModTime(time.Unix(0, 0).UTC())
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = entry.Write(bytes)
	return err
}
