package main

import (
	"archive/zip"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"soroq/backend/internal/domain"
)

type androidCodePatchPlanOptions struct {
	BaseSnapshotPath         string
	CandidateSnapshotPath    string
	CandidateArtifactPath    string
	CandidateSnapshotOutPath string
	ReleaseID                string
	ActivationMode           string
	WorkspaceOut             string
	Strict                   bool
}

type androidCodePatchPlan struct {
	SchemaVersion            int                                `json:"schema_version"`
	GeneratedAt              time.Time                          `json:"generated_at"`
	Ready                    bool                               `json:"ready"`
	Strict                   bool                               `json:"strict"`
	BaseSnapshotPath         string                             `json:"base_snapshot_path"`
	CandidateSnapshotPath    *string                            `json:"candidate_snapshot_path,omitempty"`
	WorkspaceRoot            *string                            `json:"workspace_root,omitempty"`
	Target                   androidPatchPlanTarget             `json:"target"`
	BaseArtifact             androidReleaseArtifactDescriptor   `json:"base_artifact"`
	CandidateArtifact        androidReleaseArtifactDescriptor   `json:"candidate_artifact"`
	BaseAOTLinkMetadata      []androidAOTLinkMetadataDescriptor `json:"base_aot_link_metadata,omitempty"`
	CandidateAOTLinkMetadata []androidAOTLinkMetadataDescriptor `json:"candidate_aot_link_metadata,omitempty"`
	IdentityChecks           []androidReleaseComparisonCheck    `json:"identity_checks"`
	CodePayloads             []androidCodePayload               `json:"code_payloads"`
	Blockers                 []androidCodePatchBlocker          `json:"blockers"`
	Notes                    []string                           `json:"notes,omitempty"`
}

type androidCodePayload struct {
	ABI                    string  `json:"abi"`
	Path                   string  `json:"path"`
	BaseSHA256             string  `json:"base_sha256"`
	CandidateSHA256        string  `json:"candidate_sha256"`
	BaseSizeBytes          uint64  `json:"base_size_bytes"`
	CandidateSizeBytes     uint64  `json:"candidate_size_bytes"`
	BaseWorkspacePath      *string `json:"base_workspace_path,omitempty"`
	CandidateWorkspacePath *string `json:"candidate_workspace_path,omitempty"`
}

type androidCodePatchBlocker struct {
	ID                 string  `json:"id"`
	Path               string  `json:"path,omitempty"`
	Detail             string  `json:"detail"`
	BaseSHA256         *string `json:"base_sha256,omitempty"`
	CandidateSHA256    *string `json:"candidate_sha256,omitempty"`
	BaseSizeBytes      *uint64 `json:"base_size_bytes,omitempty"`
	CandidateSizeBytes *uint64 `json:"candidate_size_bytes,omitempty"`
}

func runPrepareAndroidCodePatchPlan(args []string) error {
	fs := flag.NewFlagSet("prepare-android-code-patch-plan", flag.ContinueOnError)
	baseSnapshotPath := fs.String("base-snapshot", "", "path to base release snapshot JSON")
	candidateSnapshotPath := fs.String("candidate-snapshot", "", "path to candidate release snapshot JSON")
	candidateArtifactPath := fs.String("candidate-artifact", "", "path to candidate Android APK or AAB")
	candidateSnapshotOutPath := fs.String("candidate-snapshot-out", "", "optional path to persist a captured candidate snapshot")
	releaseID := fs.String("release-id", "", "optional release id for the planned patch target")
	activationMode := fs.String("activation", string(domain.ActivationNextColdStart), "planned activation mode")
	workspaceOut := fs.String("workspace-out", "", "directory where extracted code payloads should be written")
	outputPath := fs.String("out", "", "optional path for code patch plan JSON output")
	strict := fs.Bool("strict", false, "return an error when the candidate is not code-patch-ready")
	if err := fs.Parse(args); err != nil {
		return err
	}

	plan, err := prepareAndroidCodePatchPlan(androidCodePatchPlanOptions{
		BaseSnapshotPath:         *baseSnapshotPath,
		CandidateSnapshotPath:    *candidateSnapshotPath,
		CandidateArtifactPath:    *candidateArtifactPath,
		CandidateSnapshotOutPath: *candidateSnapshotOutPath,
		ReleaseID:                *releaseID,
		ActivationMode:           *activationMode,
		WorkspaceOut:             *workspaceOut,
		Strict:                   *strict,
	})
	if err != nil {
		return err
	}
	if err := writeJSONOutput(plan, *outputPath); err != nil {
		return err
	}
	if plan.Strict && !plan.Ready {
		return errors.New("android code patch plan is blocked; inspect blockers")
	}
	return nil
}

func prepareAndroidCodePatchPlan(options androidCodePatchPlanOptions) (*androidCodePatchPlan, error) {
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
	baseSnapshot, err := loadAndroidReleaseSnapshot(baseSnapshotPath)
	if err != nil {
		return nil, fmt.Errorf("load base snapshot: %w", err)
	}

	var (
		candidateSnapshot     *androidReleaseSnapshot
		candidateSnapshotPath *string
	)
	if hasCandidateSnapshot {
		cleanPath := filepath.Clean(options.CandidateSnapshotPath)
		candidateSnapshot, err = loadAndroidReleaseSnapshot(cleanPath)
		if err != nil {
			return nil, fmt.Errorf("load candidate snapshot: %w", err)
		}
		candidateSnapshotPath = &cleanPath
	} else {
		candidateSnapshot, err = captureAndroidReleaseSnapshot(options.CandidateArtifactPath)
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

	comparison := compareAndroidReleaseSnapshots(baseSnapshot, candidateSnapshot)
	identityChecks := make([]androidReleaseComparisonCheck, 0, len(comparison.Checks))
	blockers := make([]androidCodePatchBlocker, 0)
	if options.Strict && !isExactAndroidBaseSource(baseSnapshot.Artifact.Source) {
		blockers = append(blockers, androidCodePatchBlocker{
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
			blockers = append(blockers, androidCodePatchBlocker{
				ID:     check.ID,
				Detail: check.Detail,
			})
		}
	}

	workspaceRoot := filepath.Clean(options.WorkspaceOut)
	workspaceRootPtr := &workspaceRoot
	target := androidPatchPlanTarget{
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
		ABIs:                     deriveAndroidABIs(baseSnapshot),
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
		blockers = append(blockers, androidCodePatchBlocker{
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

	return &androidCodePatchPlan{
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

func extractAndroidCodePayloads(
	baseSnapshot *androidReleaseSnapshot,
	candidateSnapshot *androidReleaseSnapshot,
	workspaceRoot string,
) ([]androidCodePayload, []androidCodePatchBlocker, error) {
	baseNative, err := readNativeLibraryEntriesFromArtifact(baseSnapshot.Artifact.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("read base native libraries: %w", err)
	}
	candidateNative, err := readNativeLibraryEntriesFromArtifact(candidateSnapshot.Artifact.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("read candidate native libraries: %w", err)
	}

	allPaths := make(map[string]struct{}, len(baseNative)+len(candidateNative))
	for path := range baseNative {
		allPaths[path] = struct{}{}
	}
	for path := range candidateNative {
		allPaths[path] = struct{}{}
	}

	sortedPaths := sortedMapKeys(allPaths)
	blockers := make([]androidCodePatchBlocker, 0)
	payloadCandidates := make([]androidCodePayload, 0)

	if err := os.RemoveAll(workspaceRoot); err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("reset workspace root: %w", err)
	}

	for _, path := range sortedPaths {
		baseEntry, baseOK := baseNative[path]
		candidateEntry, candidateOK := candidateNative[path]
		if baseOK && candidateOK && baseEntry.SHA256 == candidateEntry.SHA256 && baseEntry.SizeBytes == candidateEntry.SizeBytes {
			continue
		}

		if !isLibappPath(path) {
			blockers = append(blockers, nativeDriftBlocker("blocked_native_drift", path, baseOK, candidateOK, baseEntry, candidateEntry))
			continue
		}
		if !baseOK || !candidateOK {
			blockers = append(blockers, nativeDriftBlocker("libapp_inventory_drift", path, baseOK, candidateOK, baseEntry, candidateEntry))
			continue
		}

		baseWorkspacePath := filepath.Join(workspaceRoot, "base", filepath.FromSlash(path))
		candidateWorkspacePath := filepath.Join(workspaceRoot, "candidate", filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(baseWorkspacePath), 0o755); err != nil {
			return nil, nil, fmt.Errorf("create base workspace dir: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(candidateWorkspacePath), 0o755); err != nil {
			return nil, nil, fmt.Errorf("create candidate workspace dir: %w", err)
		}
		if err := os.WriteFile(baseWorkspacePath, baseEntry.Bytes, 0o644); err != nil {
			return nil, nil, fmt.Errorf("write base payload %s: %w", path, err)
		}
		if err := os.WriteFile(candidateWorkspacePath, candidateEntry.Bytes, 0o644); err != nil {
			return nil, nil, fmt.Errorf("write candidate payload %s: %w", path, err)
		}

		baseWorkspacePathClean := filepath.Clean(baseWorkspacePath)
		candidateWorkspacePathClean := filepath.Clean(candidateWorkspacePath)
		payloadCandidates = append(payloadCandidates, androidCodePayload{
			ABI:                    androidABIFromNativePath(path),
			Path:                   path,
			BaseSHA256:             baseEntry.SHA256,
			CandidateSHA256:        candidateEntry.SHA256,
			BaseSizeBytes:          baseEntry.SizeBytes,
			CandidateSizeBytes:     candidateEntry.SizeBytes,
			BaseWorkspacePath:      &baseWorkspacePathClean,
			CandidateWorkspacePath: &candidateWorkspacePathClean,
		})
	}

	sort.Slice(payloadCandidates, func(i, j int) bool {
		if payloadCandidates[i].ABI == payloadCandidates[j].ABI {
			return payloadCandidates[i].Path < payloadCandidates[j].Path
		}
		return payloadCandidates[i].ABI < payloadCandidates[j].ABI
	})
	return payloadCandidates, blockers, nil
}

func readNativeLibraryEntriesFromArtifact(artifactPath string) (map[string]androidArtifactFile, error) {
	reader, err := zip.OpenReader(filepath.Clean(artifactPath))
	if err != nil {
		return nil, fmt.Errorf("open Android artifact zip: %w", err)
	}
	defer reader.Close()

	entries := make(map[string]androidArtifactFile)
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
		entries[normalizedPath] = androidArtifactFile{
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

func nativeDriftBlocker(
	id string,
	path string,
	baseOK bool,
	candidateOK bool,
	baseEntry androidArtifactFile,
	candidateEntry androidArtifactFile,
) androidCodePatchBlocker {
	detail := "native entry changed"
	switch {
	case !baseOK && candidateOK:
		detail = "native entry was added in the candidate artifact"
	case baseOK && !candidateOK:
		detail = "native entry was removed from the candidate artifact"
	}

	blocker := androidCodePatchBlocker{
		ID:     id,
		Path:   path,
		Detail: detail,
	}
	if baseOK {
		blocker.BaseSHA256 = &baseEntry.SHA256
		blocker.BaseSizeBytes = &baseEntry.SizeBytes
	}
	if candidateOK {
		blocker.CandidateSHA256 = &candidateEntry.SHA256
		blocker.CandidateSizeBytes = &candidateEntry.SizeBytes
	}
	return blocker
}

func isLibappPath(path string) bool {
	return strings.HasSuffix(path, "/libapp.so")
}

func androidABIFromNativePath(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
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
