package main

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"index/suffixarray"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kr/binarydist"

	"soroq/backend/internal/domain"
	"soroq/backend/internal/signing"
)

const (
	codeDeltaMagicV1                          = "SRQCDL1\x00"
	codeDeltaMagicV2                          = "SRQCDL2\x00"
	codeDeltaMagicV3                          = "SRQCDL3\x00"
	codeDeltaMagicV4                          = "SRQCDL4\x00"
	codeDeltaMagicV5                          = "SRQCDL5\x00"
	codeDeltaMagicV6                          = "SRQCDL6\x00"
	codeDeltaMagicV7                          = "SRQCDL7\x00"
	codeDeltaMagicV8                          = "SRQCDL8\x00"
	codeDeltaMagicV10                         = "SRQCDL10"
	codeDeltaMagicV11                         = "SRQCDL11"
	codeDeltaMagicV12                         = "SRQCDL12"
	codeDeltaMagicV13                         = "SRQCDL13"
	codeDeltaMagicV14                         = "SRQCDL14"
	codeDeltaMagicV15                         = "SRQCDL15"
	codeDeltaOpCopy                      byte = 1
	codeDeltaOpInsert                    byte = 2
	codeDeltaOpAdd                       byte = 3
	codeDeltaOpSparseAdd                 byte = 4
	codeDeltaOpOutputCopy                byte = 5
	codeDeltaAddTransformRaw             byte = 0
	codeDeltaAddTransformDelta           byte = 1
	codeDeltaAddTransformXOR             byte = 2
	codeDeltaAddTransformBitplaneZeroOne byte = 3
	codeDeltaStrategy                         = codeDeltaStrategyV15
	codeDeltaStrategyV1                       = "copy_insert_v1"
	codeDeltaStrategyV2                       = "copy_insert_v2"
	codeDeltaStrategyV3                       = "copy_insert_add_v3"
	codeDeltaStrategyV4                       = "copy_insert_sparse_add_v4"
	codeDeltaStrategyV5                       = "suffix_copy_add_v5"
	codeDeltaStrategyV6                       = "suffix_compact_add_v6"
	codeDeltaStrategyV7                       = "suffix_split_dict_add_v7"
	codeDeltaStrategyV8                       = "suffix_context_add_v8"
	codeDeltaStrategyV10                      = "output_copy_v10"
	codeDeltaStrategyV11                      = "indexed_output_copy_v11"
	codeDeltaStrategyV12                      = "sparse_indexed_output_copy_v12"
	codeDeltaStrategyV13                      = "bitplane_indexed_output_copy_v13"
	codeDeltaStrategyV14                      = "split_add_streams_v14"
	codeDeltaStrategyV15                      = "bsdiff_bzip2_v15"
	codeDeltaMinAnchorBytes                   = 64
	codeDeltaMinTailAnchorBytes               = 16
	codeDeltaAnchorLookahead                  = 16 * 1024
	codeDeltaV2AnchorBytes                    = 16
	codeDeltaV2AnchorStride                   = 8
	codeDeltaV2MinCopyBytes                   = 32
	codeDeltaV2MaxOffsetsPerAnchor            = 64
	codeDeltaV3MinAddBytes                    = 32
	codeDeltaV4MinSparseAddBytes              = 32
	codeDeltaV5SeedBytes                      = 16
	codeDeltaV5MinCopyBytes                   = 32
	codeDeltaV5MaxMatches                     = 256
	codeDeltaV6MaxMatches                     = codeDeltaV5MaxMatches
	codeDeltaV6AddDenseRadius                 = 512
	codeDeltaV6AddSparseRadius                = 16384
	codeDeltaV6AddSparseStride                = 32
	codeDeltaV6AddTopCandidates               = 32
	codeDeltaV8MinCopyBytes                   = 40
	codeDeltaV10SeedBytes                     = 16
	codeDeltaV10MinOutputCopyBytes            = 32
	codeDeltaV10MaxOutputCopyMatches          = 128
	codeDeltaV11MaxIndexedOffsetsPerSeed      = 128
)

type androidCodePatchBuildOptions struct {
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

type androidCodePatchBundleReport struct {
	SchemaVersion            int                                `json:"schema_version"`
	GeneratedAt              time.Time                          `json:"generated_at"`
	Ready                    bool                               `json:"ready"`
	CodePlanPath             string                             `json:"code_plan_path"`
	BaseArtifactPath         string                             `json:"base_artifact_path"`
	CandidateArtifactPath    string                             `json:"candidate_artifact_path"`
	BaseAOTLinkMetadata      []androidAOTLinkMetadataDescriptor `json:"base_aot_link_metadata,omitempty"`
	CandidateAOTLinkMetadata []androidAOTLinkMetadataDescriptor `json:"candidate_aot_link_metadata,omitempty"`
	BundlePath               *string                            `json:"bundle_path,omitempty"`
	BundleSHA256             *string                            `json:"bundle_sha256,omitempty"`
	BundleSizeBytes          *uint64                            `json:"bundle_size_bytes,omitempty"`
	ArtifactSHA256           *string                            `json:"artifact_sha256,omitempty"`
	ArtifactSizeBytes        *uint64                            `json:"artifact_size_bytes,omitempty"`
	Target                   androidPatchPlanTarget             `json:"target"`
	PatchID                  string                             `json:"patch_id"`
	PatchNumber              uint32                             `json:"patch_number"`
	ReleaseID                string                             `json:"release_id"`
	ManifestSigned           bool                               `json:"manifest_signed"`
	ManifestKeyID            *string                            `json:"manifest_key_id,omitempty"`
	Payloads                 []androidCodeDeltaPayloadReport    `json:"payloads"`
	Notes                    []string                           `json:"notes,omitempty"`
}

type androidCodeDeltaPayloadReport struct {
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
	AddOps             int    `json:"add_ops,omitempty"`
	SparseAddOps       int    `json:"sparse_add_ops,omitempty"`
	OutputCopyOps      int    `json:"output_copy_ops,omitempty"`
	CopiedBytes        uint64 `json:"copied_bytes"`
	InsertedBytes      uint64 `json:"inserted_bytes"`
	AddedBytes         uint64 `json:"added_bytes,omitempty"`
	SparseAddedBytes   uint64 `json:"sparse_added_bytes,omitempty"`
	OutputCopiedBytes  uint64 `json:"output_copied_bytes,omitempty"`
	SkippedBaseBytes   uint64 `json:"skipped_base_bytes"`
	Verified           bool   `json:"verified"`
}

type androidCodeArtifactMetadata struct {
	SchemaVersion            int                                `json:"schema_version"`
	GeneratedAt              time.Time                          `json:"generated_at"`
	Strategy                 string                             `json:"strategy"`
	BaseAOTLinkMetadata      []androidAOTLinkMetadataDescriptor `json:"base_aot_link_metadata,omitempty"`
	CandidateAOTLinkMetadata []androidAOTLinkMetadataDescriptor `json:"candidate_aot_link_metadata,omitempty"`
	Payloads                 []androidCodeArtifactPayloadRecord `json:"payloads"`
}

type androidCodeArtifactPayloadRecord struct {
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
	AddOps             int    `json:"add_ops,omitempty"`
	SparseAddOps       int    `json:"sparse_add_ops,omitempty"`
	OutputCopyOps      int    `json:"output_copy_ops,omitempty"`
	CopiedBytes        uint64 `json:"copied_bytes"`
	InsertedBytes      uint64 `json:"inserted_bytes"`
	AddedBytes         uint64 `json:"added_bytes,omitempty"`
	SparseAddedBytes   uint64 `json:"sparse_added_bytes,omitempty"`
	OutputCopiedBytes  uint64 `json:"output_copied_bytes,omitempty"`
	SkippedBaseBytes   uint64 `json:"skipped_base_bytes"`
}

type codeDeltaOp struct {
	Kind       byte
	BaseOffset uint64
	Length     uint64
	Literal    []byte
}

type codeDeltaSummary struct {
	Strategy          string `json:"strategy"`
	OpCount           int    `json:"op_count"`
	CopyOps           int    `json:"copy_ops"`
	InsertOps         int    `json:"insert_ops"`
	AddOps            int    `json:"add_ops"`
	SparseAddOps      int    `json:"sparse_add_ops"`
	OutputCopyOps     int    `json:"output_copy_ops"`
	CopiedBytes       uint64 `json:"copied_bytes"`
	InsertedBytes     uint64 `json:"inserted_bytes"`
	AddedBytes        uint64 `json:"added_bytes"`
	SparseAddedBytes  uint64 `json:"sparse_added_bytes"`
	OutputCopiedBytes uint64 `json:"output_copied_bytes"`
	SkippedBaseBytes  uint64 `json:"skipped_base_bytes"`
}

func runBuildAndroidCodePatch(args []string) error {
	fs := flag.NewFlagSet("build-android-code-patch", flag.ContinueOnError)
	codePlanPath := fs.String("code-plan", "", "path to android code patch plan json")
	patchID := fs.String("patch-id", "", "patch id")
	patchNumber := fs.Uint("patch-number", 0, "patch number")
	releaseID := fs.String("release-id", "", "release id override (defaults to code plan target release_id)")
	artifactURL := fs.String("artifact-url", "", "artifact url recorded in manifest (defaults to a local placeholder)")
	outputPath := fs.String("out", "", "path to write the code patch bundle zip")
	reportOutPath := fs.String("report-out", "", "optional path for the code patch build report json")
	seedBase64 := fs.String("seed-base64", "", "optional manifest signing private seed in base64url format")
	keyID := fs.String("key-id", "", "optional manifest signing key id override")
	codeDeltaStrategyRaw := fs.String("code-delta-strategy", "default", "code delta strategy to emit: default (v15), v8, v10, v11, v12, v13, v14, or v15")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if strings.TrimSpace(*codePlanPath) == "" || strings.TrimSpace(*patchID) == "" || *patchNumber == 0 || strings.TrimSpace(*outputPath) == "" {
		return errors.New("--code-plan, --patch-id, --patch-number, and --out are required")
	}
	if strings.TrimSpace(*keyID) != "" && strings.TrimSpace(*seedBase64) == "" {
		return errors.New("--key-id requires --seed-base64")
	}

	report, bundleBytes, err := buildAndroidCodePatchBundle(androidCodePatchBuildOptions{
		CodePlanPath:      *codePlanPath,
		PatchID:           *patchID,
		PatchNumber:       uint32(*patchNumber),
		ReleaseID:         *releaseID,
		ArtifactURL:       *artifactURL,
		OutputPath:        *outputPath,
		ReportOutPath:     *reportOutPath,
		SeedBase64:        *seedBase64,
		KeyID:             *keyID,
		CodeDeltaStrategy: *codeDeltaStrategyRaw,
	})
	if report != nil {
		if strings.TrimSpace(*reportOutPath) != "" {
			if writeErr := writeJSONOutput(report, *reportOutPath); writeErr != nil {
				return writeErr
			}
		} else {
			if writeErr := writeJSONOutput(report, ""); writeErr != nil {
				return writeErr
			}
		}
	}
	if err != nil {
		return err
	}

	outputPathClean := filepath.Clean(*outputPath)
	if err := os.MkdirAll(filepath.Dir(outputPathClean), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(outputPathClean, bundleBytes, 0o644); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}
	return nil
}

func buildAndroidCodePatchBundle(
	options androidCodePatchBuildOptions,
) (*androidCodePatchBundleReport, []byte, error) {
	codePlanPath := filepath.Clean(options.CodePlanPath)
	plan, err := loadAndroidCodePatchPlan(codePlanPath)
	if err != nil {
		return nil, nil, err
	}

	releaseID := strings.TrimSpace(options.ReleaseID)
	if releaseID == "" && plan.Target.ReleaseID != nil {
		releaseID = strings.TrimSpace(*plan.Target.ReleaseID)
	}

	report := &androidCodePatchBundleReport{
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
		Payloads:                 []androidCodeDeltaPayloadReport{},
	}
	codeDeltaBuilder, err := resolveCodeDeltaBuildStrategy(options.CodeDeltaStrategy)
	if err != nil {
		return report, nil, err
	}

	if !plan.Ready {
		report.Notes = append(report.Notes, "code patch plan is already blocked; fix the compatibility blockers before generating a code patch bundle")
		return report, nil, errors.New("android code patch plan is not ready")
	}
	if domain.NormalizePatchKind(domain.PatchKind(plan.Target.PatchKind)) != domain.PatchKindExperimentalNativeAOT {
		report.Notes = append(report.Notes, "this command currently builds experimental_native_aot bundles only; rerun prepare-android-code-patch-plan for native code payload drift")
		return report, nil, fmt.Errorf("android code patch plan target patch_kind must be %q, got %q", domain.PatchKindExperimentalNativeAOT, plan.Target.PatchKind)
	}
	if releaseID == "" {
		return report, nil, errors.New("release id is required; pass --release-id or include it in the code plan target")
	}
	if len(plan.CodePayloads) == 0 {
		report.Notes = append(report.Notes, "the code plan did not contain any extracted libapp.so payloads")
		return report, nil, errors.New("android code patch plan contains no code payloads")
	}

	artifactRecords := make([]androidCodeArtifactPayloadRecord, 0, len(plan.CodePayloads))
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

		deltaBytes, summary, err := codeDeltaBuilder.build(baseBytes, candidateBytes)
		if err != nil {
			return report, nil, fmt.Errorf("build code delta %s: %w", payload.Path, err)
		}
		reconstructed, err := applyCodeDelta(baseBytes, deltaBytes)
		if err != nil {
			return report, nil, fmt.Errorf("apply code delta %s: %w", payload.Path, err)
		}
		verified := bytes.Equal(reconstructed, candidateBytes)
		if !verified {
			return report, nil, fmt.Errorf("delta verification failed for %s", payload.Path)
		}

		deltaPath := filepath.ToSlash(filepath.Join("deltas", payload.Path)) + ".sqd"
		deltaSHA := sha256Hex(deltaBytes)
		deltaSize := uint64(len(deltaBytes))
		report.Payloads = append(report.Payloads, androidCodeDeltaPayloadReport{
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
			AddOps:             summary.AddOps,
			SparseAddOps:       summary.SparseAddOps,
			OutputCopyOps:      summary.OutputCopyOps,
			CopiedBytes:        summary.CopiedBytes,
			InsertedBytes:      summary.InsertedBytes,
			AddedBytes:         summary.AddedBytes,
			SparseAddedBytes:   summary.SparseAddedBytes,
			OutputCopiedBytes:  summary.OutputCopiedBytes,
			SkippedBaseBytes:   summary.SkippedBaseBytes,
			Verified:           true,
		})
		artifactRecords = append(artifactRecords, androidCodeArtifactPayloadRecord{
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
			AddOps:             summary.AddOps,
			SparseAddOps:       summary.SparseAddOps,
			OutputCopyOps:      summary.OutputCopyOps,
			CopiedBytes:        summary.CopiedBytes,
			InsertedBytes:      summary.InsertedBytes,
			AddedBytes:         summary.AddedBytes,
			SparseAddedBytes:   summary.SparseAddedBytes,
			OutputCopiedBytes:  summary.OutputCopiedBytes,
			SkippedBaseBytes:   summary.SkippedBaseBytes,
		})
		deltaFiles[deltaPath] = deltaBytes
	}

	artifactBytes, err := buildAndroidCodePatchArtifact(
		codeDeltaBuilder.name,
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
		"artifact.bin now carries a code delta archive for future release/AOT application work",
	)
	return report, bundleBytes, nil
}

func loadAndroidCodePatchPlan(path string) (*androidCodePatchPlan, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var plan androidCodePatchPlan
	if err := json.Unmarshal(bytes, &plan); err != nil {
		return nil, err
	}
	if plan.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported code patch plan schema version %d", plan.SchemaVersion)
	}
	return &plan, nil
}

func buildAndroidCodePatchArtifact(
	strategy string,
	baseAOTLinkMetadata []androidAOTLinkMetadataDescriptor,
	candidateAOTLinkMetadata []androidAOTLinkMetadataDescriptor,
	payloads []androidCodeArtifactPayloadRecord,
	deltaFiles map[string][]byte,
) ([]byte, error) {
	metadataBytes, err := json.MarshalIndent(androidCodeArtifactMetadata{
		SchemaVersion:            1,
		GeneratedAt:              time.Now().UTC(),
		Strategy:                 strategy,
		BaseAOTLinkMetadata:      baseAOTLinkMetadata,
		CandidateAOTLinkMetadata: candidateAOTLinkMetadata,
		Payloads:                 payloads,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode code patch artifact metadata: %w", err)
	}

	var output bytes.Buffer
	writer := newBestCompressionZipWriter(&output)
	writeEntry := func(path string, bytes []byte) error {
		entry, err := writer.Create(path)
		if err != nil {
			return fmt.Errorf("create code patch artifact entry %s: %w", path, err)
		}
		if _, err := entry.Write(bytes); err != nil {
			return fmt.Errorf("write code patch artifact entry %s: %w", path, err)
		}
		return nil
	}

	if err := writeEntry("metadata.json", metadataBytes); err != nil {
		return nil, err
	}
	for _, path := range sortedMapKeys(deltaFiles) {
		if err := writeEntry(path, deltaFiles[path]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finalize code patch artifact archive: %w", err)
	}
	return output.Bytes(), nil
}

func newBestCompressionZipWriter(output *bytes.Buffer) *zip.Writer {
	writer := zip.NewWriter(output)
	writer.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(w, flate.BestCompression)
	})
	return writer
}

func buildCodeDelta(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	return buildCodeDeltaV15(baseBytes, candidateBytes)
}

func resolveCodeDeltaBuildStrategy(raw string) (codeDeltaBenchmarkStrategyBuilder, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if key == "" || key == "default" {
		key = codeDeltaStrategy
	}
	strategy, ok := availableCodeDeltaBenchmarkStrategies()[key]
	if !ok {
		return codeDeltaBenchmarkStrategyBuilder{}, fmt.Errorf("unknown code delta strategy %q", raw)
	}
	return strategy, nil
}

func buildCodeDeltaV1(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOps(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV1
	return encodeCodeDelta(codeDeltaMagicV1, ops, summary)
}

func buildCodeDeltaV2(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV2(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV2
	return encodeCodeDelta(codeDeltaMagicV2, ops, summary)
}

func buildCodeDeltaV3(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV2(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV3(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV3
	return encodeCodeDelta(codeDeltaMagicV3, ops, summary)
}

func buildCodeDeltaV4(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV2(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV3(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV4(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV4
	return encodeCodeDelta(codeDeltaMagicV4, ops, summary)
}

func buildCodeDeltaV5(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV5(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV3(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV5
	return encodeCodeDelta(codeDeltaMagicV5, ops, summary)
}

func buildCodeDeltaV6(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV6(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV6Add(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV6
	return encodeCompactCodeDelta(codeDeltaMagicV6, ops, summary)
}

func buildCodeDeltaV7(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV6(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV6Add(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV7
	return encodeSplitDictionaryCodeDelta(codeDeltaMagicV7, baseBytes, ops, summary)
}

func buildCodeDeltaV8(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV8(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV6Add(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV8
	return encodeSplitContextCodeDelta(codeDeltaMagicV8, baseBytes, ops, summary)
}

func buildCodeDeltaV10(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV8(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV6Add(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV10OutputCopy(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV10
	return encodeSplitContextCodeDelta(codeDeltaMagicV10, baseBytes, ops, summary)
}

func buildCodeDeltaV11(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV8(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV6Add(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV11IndexedOutputCopy(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV11
	return encodeSplitContextCodeDelta(codeDeltaMagicV11, baseBytes, ops, summary)
}

func buildCodeDeltaV12(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV8(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV6Add(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV12SparseAdd(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV11IndexedOutputCopy(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV12
	return encodeSplitTransformedAddCodeDelta(codeDeltaMagicV12, baseBytes, ops, summary)
}

func buildCodeDeltaV13(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV8(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV6Add(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV11IndexedOutputCopy(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV13
	return encodeSplitTransformedAddCodeDeltaWithTransforms(
		codeDeltaMagicV13,
		baseBytes,
		ops,
		summary,
		[]byte{
			codeDeltaAddTransformRaw,
			codeDeltaAddTransformDelta,
			codeDeltaAddTransformXOR,
			codeDeltaAddTransformBitplaneZeroOne,
		},
	)
}

func buildCodeDeltaV14(baseBytes []byte, candidateBytes []byte) ([]byte, codeDeltaSummary, error) {
	ops := buildCodeDeltaOpsV8(baseBytes, candidateBytes)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV6Add(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	ops = buildCodeDeltaOpsV11IndexedOutputCopy(baseBytes, ops)
	ops = coalesceCodeDeltaOps(ops)
	summary := summarizeCodeDelta(baseBytes, ops)
	summary.Strategy = codeDeltaStrategyV14
	return encodeSplitOneOtherAddCodeDelta(codeDeltaMagicV14, baseBytes, ops, summary)
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

func encodeCodeDelta(magic string, ops []codeDeltaOp, summary codeDeltaSummary) ([]byte, codeDeltaSummary, error) {
	var output bytes.Buffer
	if _, err := output.Write([]byte(magic)); err != nil {
		return nil, codeDeltaSummary{}, err
	}
	if err := binary.Write(&output, binary.BigEndian, uint32(len(ops))); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("encode op count: %w", err)
	}
	for _, op := range ops {
		if err := output.WriteByte(op.Kind); err != nil {
			return nil, codeDeltaSummary{}, fmt.Errorf("encode op kind: %w", err)
		}
		if err := binary.Write(&output, binary.BigEndian, op.BaseOffset); err != nil {
			return nil, codeDeltaSummary{}, fmt.Errorf("encode op base offset: %w", err)
		}
		if err := binary.Write(&output, binary.BigEndian, op.Length); err != nil {
			return nil, codeDeltaSummary{}, fmt.Errorf("encode op length: %w", err)
		}
		if op.Kind == codeDeltaOpSparseAdd {
			if err := binary.Write(&output, binary.BigEndian, uint64(len(op.Literal))); err != nil {
				return nil, codeDeltaSummary{}, fmt.Errorf("encode sparse add payload length: %w", err)
			}
		}
		if op.Kind == codeDeltaOpInsert || op.Kind == codeDeltaOpAdd || op.Kind == codeDeltaOpSparseAdd {
			if _, err := output.Write(op.Literal); err != nil {
				return nil, codeDeltaSummary{}, fmt.Errorf("encode delta literal: %w", err)
			}
		}
	}
	return output.Bytes(), summary, nil
}

func encodeCompactCodeDelta(magic string, ops []codeDeltaOp, summary codeDeltaSummary) ([]byte, codeDeltaSummary, error) {
	var output bytes.Buffer
	if _, err := output.Write([]byte(magic)); err != nil {
		return nil, codeDeltaSummary{}, err
	}
	body, err := encodeCompactCodeDeltaBody(ops)
	if err != nil {
		return nil, codeDeltaSummary{}, err
	}
	if _, err := output.Write(body); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compact delta body: %w", err)
	}
	return output.Bytes(), summary, nil
}

func encodeDictionaryCompressedCodeDelta(magic string, baseBytes []byte, ops []codeDeltaOp, summary codeDeltaSummary) ([]byte, codeDeltaSummary, error) {
	body, err := encodeCompactCodeDeltaBody(ops)
	if err != nil {
		return nil, codeDeltaSummary{}, err
	}
	compressedBody, err := compressCodeDeltaBodyWithDict(body, baseBytes)
	if err != nil {
		return nil, codeDeltaSummary{}, err
	}
	var output bytes.Buffer
	if _, err := output.Write([]byte(magic)); err != nil {
		return nil, codeDeltaSummary{}, err
	}
	writeUvarint(&output, uint64(len(body)))
	if _, err := output.Write(compressedBody); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write dictionary-compressed delta body: %w", err)
	}
	return output.Bytes(), summary, nil
}

func encodeSplitDictionaryCodeDelta(magic string, baseBytes []byte, ops []codeDeltaOp, summary codeDeltaSummary) ([]byte, codeDeltaSummary, error) {
	controlStream, insertStream, addStream, err := encodeSplitCodeDeltaStreams(ops)
	if err != nil {
		return nil, codeDeltaSummary{}, err
	}
	compressedControl, err := compressCodeDeltaBodyWithDict(controlStream, nil)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress control stream: %w", err)
	}
	compressedInsert, err := compressCodeDeltaBodyWithDict(insertStream, codeDeltaCompressionDictionary(baseBytes))
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress insert stream: %w", err)
	}
	compressedAdd, err := compressCodeDeltaBodyWithDict(addStream, nil)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress add stream: %w", err)
	}

	var output bytes.Buffer
	if _, err := output.Write([]byte(magic)); err != nil {
		return nil, codeDeltaSummary{}, err
	}
	writeUvarint(&output, uint64(len(controlStream)))
	writeUvarint(&output, uint64(len(compressedControl)))
	writeUvarint(&output, uint64(len(insertStream)))
	writeUvarint(&output, uint64(len(compressedInsert)))
	writeUvarint(&output, uint64(len(addStream)))
	writeUvarint(&output, uint64(len(compressedAdd)))
	if _, err := output.Write(compressedControl); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed control stream: %w", err)
	}
	if _, err := output.Write(compressedInsert); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed insert stream: %w", err)
	}
	if _, err := output.Write(compressedAdd); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed add stream: %w", err)
	}
	return output.Bytes(), summary, nil
}

func encodeSplitContextCodeDelta(magic string, baseBytes []byte, ops []codeDeltaOp, summary codeDeltaSummary) ([]byte, codeDeltaSummary, error) {
	controlStream, insertStream, addStream, err := encodeSplitCodeDeltaStreams(ops)
	if err != nil {
		return nil, codeDeltaSummary{}, err
	}
	compressedControl, err := compressCodeDeltaBodyWithDict(controlStream, nil)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress control stream: %w", err)
	}
	insertDictionary, err := codeDeltaInsertContextDictionary(baseBytes, controlStream)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("build insert context dictionary: %w", err)
	}
	compressedInsert, err := compressCodeDeltaBodyWithDict(insertStream, insertDictionary)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress insert stream: %w", err)
	}
	compressedAdd, err := compressCodeDeltaBodyWithDict(addStream, nil)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress add stream: %w", err)
	}

	var output bytes.Buffer
	if _, err := output.Write([]byte(magic)); err != nil {
		return nil, codeDeltaSummary{}, err
	}
	writeUvarint(&output, uint64(len(controlStream)))
	writeUvarint(&output, uint64(len(compressedControl)))
	writeUvarint(&output, uint64(len(insertStream)))
	writeUvarint(&output, uint64(len(compressedInsert)))
	writeUvarint(&output, uint64(len(addStream)))
	writeUvarint(&output, uint64(len(compressedAdd)))
	if _, err := output.Write(compressedControl); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed control stream: %w", err)
	}
	if _, err := output.Write(compressedInsert); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed insert stream: %w", err)
	}
	if _, err := output.Write(compressedAdd); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed add stream: %w", err)
	}
	return output.Bytes(), summary, nil
}

func encodeSplitTransformedAddCodeDelta(magic string, baseBytes []byte, ops []codeDeltaOp, summary codeDeltaSummary) ([]byte, codeDeltaSummary, error) {
	return encodeSplitTransformedAddCodeDeltaWithTransforms(
		magic,
		baseBytes,
		ops,
		summary,
		[]byte{
			codeDeltaAddTransformRaw,
			codeDeltaAddTransformDelta,
			codeDeltaAddTransformXOR,
		},
	)
}

func encodeSplitTransformedAddCodeDeltaWithTransforms(magic string, baseBytes []byte, ops []codeDeltaOp, summary codeDeltaSummary, transforms []byte) ([]byte, codeDeltaSummary, error) {
	controlStream, insertStream, addStream, err := encodeSplitCodeDeltaStreams(ops)
	if err != nil {
		return nil, codeDeltaSummary{}, err
	}
	compressedControl, err := compressCodeDeltaBodyWithDict(controlStream, nil)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress control stream: %w", err)
	}
	insertDictionary, err := codeDeltaInsertContextDictionary(baseBytes, controlStream)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("build insert context dictionary: %w", err)
	}
	compressedInsert, err := compressCodeDeltaBodyWithDict(insertStream, insertDictionary)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress insert stream: %w", err)
	}
	addTransform, transformedAddStream, compressedAdd, err := bestCodeDeltaAddTransform(addStream, transforms)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress transformed add stream: %w", err)
	}

	var output bytes.Buffer
	if _, err := output.Write([]byte(magic)); err != nil {
		return nil, codeDeltaSummary{}, err
	}
	writeUvarint(&output, uint64(addTransform))
	writeUvarint(&output, uint64(len(controlStream)))
	writeUvarint(&output, uint64(len(compressedControl)))
	writeUvarint(&output, uint64(len(insertStream)))
	writeUvarint(&output, uint64(len(compressedInsert)))
	writeUvarint(&output, uint64(len(transformedAddStream)))
	writeUvarint(&output, uint64(len(compressedAdd)))
	if _, err := output.Write(compressedControl); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed control stream: %w", err)
	}
	if _, err := output.Write(compressedInsert); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed insert stream: %w", err)
	}
	if _, err := output.Write(compressedAdd); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed add stream: %w", err)
	}
	return output.Bytes(), summary, nil
}

func encodeSplitOneOtherAddCodeDelta(magic string, baseBytes []byte, ops []codeDeltaOp, summary codeDeltaSummary) ([]byte, codeDeltaSummary, error) {
	return encodeSplitOneOtherAddCodeDeltaWithDictionaryBuilder(magic, baseBytes, ops, summary, codeDeltaInsertContextDictionary)
}

func encodeSplitOneOtherAddCodeDeltaWithDictionaryBuilder(
	magic string,
	baseBytes []byte,
	ops []codeDeltaOp,
	summary codeDeltaSummary,
	dictionaryBuilder func([]byte, []byte) ([]byte, error),
) ([]byte, codeDeltaSummary, error) {
	controlStream, insertStream, addStream, err := encodeSplitCodeDeltaStreams(ops)
	if err != nil {
		return nil, codeDeltaSummary{}, err
	}
	compressedControl, err := compressCodeDeltaBodyWithDict(controlStream, nil)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress control stream: %w", err)
	}
	insertDictionary, err := dictionaryBuilder(baseBytes, controlStream)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("build insert context dictionary: %w", err)
	}
	compressedInsert, err := compressCodeDeltaBodyWithDict(insertStream, insertDictionary)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress insert stream: %w", err)
	}
	oneGapStream, otherGapStream, otherValueStream := transformCodeDeltaAddStreamOneOtherGaps(addStream)
	compressedOneGaps, err := compressCodeDeltaBodyWithDict(oneGapStream, nil)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress one-gap add stream: %w", err)
	}
	compressedOtherGaps, err := compressCodeDeltaBodyWithDict(otherGapStream, nil)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress other-gap add stream: %w", err)
	}
	compressedOtherValues, err := compressCodeDeltaBodyWithDict(otherValueStream, nil)
	if err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("compress other-value add stream: %w", err)
	}

	var output bytes.Buffer
	if _, err := output.Write([]byte(magic)); err != nil {
		return nil, codeDeltaSummary{}, err
	}
	writeUvarint(&output, uint64(len(controlStream)))
	writeUvarint(&output, uint64(len(compressedControl)))
	writeUvarint(&output, uint64(len(insertStream)))
	writeUvarint(&output, uint64(len(compressedInsert)))
	writeUvarint(&output, uint64(len(addStream)))
	writeUvarint(&output, uint64(len(oneGapStream)))
	writeUvarint(&output, uint64(len(compressedOneGaps)))
	writeUvarint(&output, uint64(len(otherGapStream)))
	writeUvarint(&output, uint64(len(compressedOtherGaps)))
	writeUvarint(&output, uint64(len(otherValueStream)))
	writeUvarint(&output, uint64(len(compressedOtherValues)))
	if _, err := output.Write(compressedControl); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed control stream: %w", err)
	}
	if _, err := output.Write(compressedInsert); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed insert stream: %w", err)
	}
	if _, err := output.Write(compressedOneGaps); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed one-gap add stream: %w", err)
	}
	if _, err := output.Write(compressedOtherGaps); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed other-gap add stream: %w", err)
	}
	if _, err := output.Write(compressedOtherValues); err != nil {
		return nil, codeDeltaSummary{}, fmt.Errorf("write compressed other-value add stream: %w", err)
	}
	return output.Bytes(), summary, nil
}

func transformCodeDeltaAddStreamOneOtherGaps(addStream []byte) ([]byte, []byte, []byte) {
	var oneGapStream bytes.Buffer
	var otherGapStream bytes.Buffer
	var otherValueStream bytes.Buffer
	previousOneIndex := -1
	previousOtherIndex := -1
	for index, value := range addStream {
		switch value {
		case 0:
			continue
		case 1:
			writeUvarint(&oneGapStream, uint64(index-previousOneIndex))
			previousOneIndex = index
		default:
			writeUvarint(&otherGapStream, uint64(index-previousOtherIndex))
			otherValueStream.WriteByte(value)
			previousOtherIndex = index
		}
	}
	return oneGapStream.Bytes(), otherGapStream.Bytes(), otherValueStream.Bytes()
}

func bestCodeDeltaAddTransform(addStream []byte, transforms []byte) (byte, []byte, []byte, error) {
	type candidate struct {
		id          byte
		transformed []byte
		compressed  []byte
	}
	candidates := make([]candidate, 0, len(transforms))
	for _, transform := range transforms {
		transformed, err := transformCodeDeltaAddStream(addStream, transform)
		if err != nil {
			return 0, nil, nil, err
		}
		compressed, err := compressCodeDeltaBodyWithDict(transformed, nil)
		if err != nil {
			return 0, nil, nil, err
		}
		candidates = append(candidates, candidate{
			id:          transform,
			transformed: transformed,
			compressed:  compressed,
		})
	}
	best := candidates[0]
	for _, current := range candidates[1:] {
		if len(current.compressed) < len(best.compressed) {
			best = current
		}
	}
	return best.id, best.transformed, best.compressed, nil
}

func transformCodeDeltaAddStream(addStream []byte, transform byte) ([]byte, error) {
	switch transform {
	case codeDeltaAddTransformRaw:
		return append([]byte(nil), addStream...), nil
	case codeDeltaAddTransformDelta:
		output := make([]byte, len(addStream))
		var previous byte
		for index, value := range addStream {
			output[index] = value - previous
			previous = value
		}
		return output, nil
	case codeDeltaAddTransformXOR:
		output := make([]byte, len(addStream))
		var previous byte
		for index, value := range addStream {
			output[index] = value ^ previous
			previous = value
		}
		return output, nil
	case codeDeltaAddTransformBitplaneZeroOne:
		return transformCodeDeltaAddStreamBitplaneZeroOne(addStream), nil
	default:
		return nil, fmt.Errorf("unsupported add transform %d", transform)
	}
}

func transformCodeDeltaAddStreamBitplaneZeroOne(addStream []byte) []byte {
	packedCodes := make([]byte, (len(addStream)+3)/4)
	rawFallback := make([]byte, 0)
	for index, value := range addStream {
		code := byte(2)
		switch value {
		case 0:
			code = 0
		case 1:
			code = 1
		default:
			rawFallback = append(rawFallback, value)
		}
		packedCodes[index/4] |= code << uint((index%4)*2)
	}

	var output bytes.Buffer
	writeUvarint(&output, uint64(len(addStream)))
	writeUvarint(&output, uint64(len(rawFallback)))
	output.Write(packedCodes)
	output.Write(rawFallback)
	return output.Bytes()
}

func restoreCodeDeltaAddStream(addStream []byte, transform byte) ([]byte, error) {
	switch transform {
	case codeDeltaAddTransformRaw:
		return addStream, nil
	case codeDeltaAddTransformDelta:
		output := make([]byte, len(addStream))
		var previous byte
		for index, value := range addStream {
			restored := previous + value
			output[index] = restored
			previous = restored
		}
		return output, nil
	case codeDeltaAddTransformXOR:
		output := make([]byte, len(addStream))
		var previous byte
		for index, value := range addStream {
			restored := value ^ previous
			output[index] = restored
			previous = restored
		}
		return output, nil
	case codeDeltaAddTransformBitplaneZeroOne:
		return restoreCodeDeltaAddStreamBitplaneZeroOne(addStream)
	default:
		return nil, fmt.Errorf("unsupported add transform %d", transform)
	}
}

func restoreCodeDeltaAddStreamBitplaneZeroOne(addStream []byte) ([]byte, error) {
	reader := bytes.NewReader(addStream)
	originalLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read bitplane add original length: %w", err)
	}
	rawFallbackLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read bitplane add raw fallback length: %w", err)
	}
	packedLength := (originalLength + 3) / 4
	packedCodes, err := readCodeDeltaStream(reader, packedLength)
	if err != nil {
		return nil, fmt.Errorf("read bitplane add codes: %w", err)
	}
	rawFallback, err := readCodeDeltaStream(reader, rawFallbackLength)
	if err != nil {
		return nil, fmt.Errorf("read bitplane add raw fallback original=%d raw=%d packed=%d remaining=%d: %w", originalLength, rawFallbackLength, packedLength, reader.Len(), err)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected bitplane add trailing bytes: %d", reader.Len())
	}

	output := make([]byte, int(originalLength))
	rawIndex := 0
	for index := range output {
		code := (packedCodes[index/4] >> uint((index%4)*2)) & 0x03
		switch code {
		case 0:
			output[index] = 0
		case 1:
			output[index] = 1
		case 2:
			if rawIndex >= len(rawFallback) {
				return nil, fmt.Errorf("bitplane add raw fallback exhausted at byte %d", index)
			}
			output[index] = rawFallback[rawIndex]
			rawIndex++
		default:
			return nil, fmt.Errorf("unsupported bitplane add code %d at byte %d", code, index)
		}
	}
	if rawIndex != len(rawFallback) {
		return nil, fmt.Errorf("unused bitplane add raw fallback bytes: %d", len(rawFallback)-rawIndex)
	}
	return output, nil
}

func encodeSplitCodeDeltaStreams(ops []codeDeltaOp) ([]byte, []byte, []byte, error) {
	var controlStream bytes.Buffer
	var insertStream bytes.Buffer
	var addStream bytes.Buffer
	writeUvarint(&controlStream, uint64(len(ops)))
	for _, op := range ops {
		if err := controlStream.WriteByte(op.Kind); err != nil {
			return nil, nil, nil, fmt.Errorf("encode split op kind: %w", err)
		}
		writeUvarint(&controlStream, op.BaseOffset)
		writeUvarint(&controlStream, op.Length)
		switch op.Kind {
		case codeDeltaOpInsert:
			if _, err := insertStream.Write(op.Literal); err != nil {
				return nil, nil, nil, fmt.Errorf("encode split insert literal: %w", err)
			}
		case codeDeltaOpAdd:
			if _, err := addStream.Write(op.Literal); err != nil {
				return nil, nil, nil, fmt.Errorf("encode split add literal: %w", err)
			}
		case codeDeltaOpSparseAdd:
			writeUvarint(&controlStream, uint64(len(op.Literal)))
			if _, err := addStream.Write(op.Literal); err != nil {
				return nil, nil, nil, fmt.Errorf("encode split sparse add payload: %w", err)
			}
		case codeDeltaOpOutputCopy:
			// Output-copy is fully described by the control stream.
		}
	}
	return controlStream.Bytes(), insertStream.Bytes(), addStream.Bytes(), nil
}

func encodeCompactCodeDeltaBody(ops []codeDeltaOp) ([]byte, error) {
	var output bytes.Buffer
	writeUvarint(&output, uint64(len(ops)))
	for _, op := range ops {
		if err := output.WriteByte(op.Kind); err != nil {
			return nil, fmt.Errorf("encode compact op kind: %w", err)
		}
		writeUvarint(&output, op.BaseOffset)
		writeUvarint(&output, op.Length)
		if op.Kind == codeDeltaOpSparseAdd {
			writeUvarint(&output, uint64(len(op.Literal)))
		}
		if op.Kind == codeDeltaOpInsert || op.Kind == codeDeltaOpAdd || op.Kind == codeDeltaOpSparseAdd {
			if _, err := output.Write(op.Literal); err != nil {
				return nil, fmt.Errorf("encode compact delta literal: %w", err)
			}
		}
	}
	return output.Bytes(), nil
}

func compressCodeDeltaBodyWithDict(body []byte, dict []byte) ([]byte, error) {
	var output bytes.Buffer
	var writer *flate.Writer
	var err error
	if len(dict) == 0 {
		writer, err = flate.NewWriter(&output, flate.BestCompression)
	} else {
		writer, err = flate.NewWriterDict(&output, flate.BestCompression, dict)
	}
	if err != nil {
		return nil, fmt.Errorf("create dictionary compressor: %w", err)
	}
	if _, err := writer.Write(body); err != nil {
		return nil, fmt.Errorf("write dictionary-compressed body: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finalize dictionary-compressed body: %w", err)
	}
	return output.Bytes(), nil
}

func codeDeltaCompressionDictionary(baseBytes []byte) []byte {
	return baseBytes[:minInt(len(baseBytes), 32*1024)]
}

func codeDeltaInsertContextDictionary(baseBytes []byte, controlStream []byte) ([]byte, error) {
	reader := bytes.NewReader(controlStream)
	opCount, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split delta op count: %w", err)
	}

	dictionary := make([]byte, 0, 32*1024)
	previousBaseEnd := 0
	for index := uint64(0); index < opCount; index++ {
		kind, err := reader.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read split delta op kind %d: %w", index, err)
		}
		baseOffset, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read split delta op base offset %d: %w", index, err)
		}
		length, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read split delta op length %d: %w", index, err)
		}
		switch kind {
		case codeDeltaOpCopy, codeDeltaOpAdd:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("split op %d exceeds base payload bounds", index)
			}
			previousBaseEnd = int(end)
		case codeDeltaOpSparseAdd:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("split sparse op %d exceeds base payload bounds", index)
			}
			if _, err := binary.ReadUvarint(reader); err != nil {
				return nil, fmt.Errorf("read split sparse add payload length %d: %w", index, err)
			}
			previousBaseEnd = int(end)
		case codeDeltaOpInsert:
			start := maxInt(0, previousBaseEnd-512)
			end := minInt(len(baseBytes), previousBaseEnd+512)
			dictionary = append(dictionary, baseBytes[start:end]...)
			if len(dictionary) >= 32*1024 {
				dictionary = dictionary[len(dictionary)-32*1024:]
			}
		case codeDeltaOpOutputCopy:
			// Output-copy does not consume base bytes, so the insert context stays anchored to the last base-backed op.
		default:
			return nil, fmt.Errorf("unsupported split delta op kind %d", kind)
		}
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected split control trailing bytes: %d", reader.Len())
	}
	return dictionary, nil
}

func applyCodeDelta(baseBytes []byte, deltaBytes []byte) ([]byte, error) {
	reader := bytes.NewReader(deltaBytes)
	magic := make([]byte, len(codeDeltaMagicV1))
	if _, err := reader.Read(magic); err != nil {
		return nil, fmt.Errorf("read delta header: %w", err)
	}
	switch string(magic) {
	case codeDeltaMagicV15:
		return applyBsdiffCodeDelta(baseBytes, reader)
	case codeDeltaMagicV14:
		return applySplitOneOtherAddCodeDelta(baseBytes, reader)
	case codeDeltaMagicV13:
		return applySplitTransformedAddCodeDelta(baseBytes, reader)
	case codeDeltaMagicV12:
		return applySplitTransformedAddCodeDelta(baseBytes, reader)
	case codeDeltaMagicV11:
		return applySplitContextCodeDelta(baseBytes, reader)
	case codeDeltaMagicV10:
		return applySplitContextCodeDelta(baseBytes, reader)
	case codeDeltaMagicV8:
		return applySplitContextCodeDelta(baseBytes, reader)
	case codeDeltaMagicV7:
		return applySplitDictionaryCodeDelta(baseBytes, reader)
	case codeDeltaMagicV6:
		return applyCompactCodeDelta(baseBytes, reader)
	case codeDeltaMagicV1, codeDeltaMagicV2, codeDeltaMagicV3, codeDeltaMagicV4, codeDeltaMagicV5:
	default:
		return nil, fmt.Errorf("unexpected code delta magic %q", string(magic))
	}

	var opCount uint32
	if err := binary.Read(reader, binary.BigEndian, &opCount); err != nil {
		return nil, fmt.Errorf("read delta op count: %w", err)
	}

	var output bytes.Buffer
	for i := uint32(0); i < opCount; i++ {
		kind, err := reader.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read delta op kind %d: %w", i, err)
		}
		var baseOffset uint64
		if err := binary.Read(reader, binary.BigEndian, &baseOffset); err != nil {
			return nil, fmt.Errorf("read delta op base offset %d: %w", i, err)
		}
		var length uint64
		if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
			return nil, fmt.Errorf("read delta op length %d: %w", i, err)
		}

		switch kind {
		case codeDeltaOpCopy:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("copy op %d exceeds base payload bounds", i)
			}
			if _, err := output.Write(baseBytes[int(baseOffset):int(end)]); err != nil {
				return nil, fmt.Errorf("apply copy op %d: %w", i, err)
			}
		case codeDeltaOpInsert:
			literal := make([]byte, int(length))
			if _, err := reader.Read(literal); err != nil {
				return nil, fmt.Errorf("read insert literal %d: %w", i, err)
			}
			if _, err := output.Write(literal); err != nil {
				return nil, fmt.Errorf("apply insert op %d: %w", i, err)
			}
		case codeDeltaOpAdd:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("add op %d exceeds base payload bounds", i)
			}
			diff := make([]byte, int(length))
			if _, err := reader.Read(diff); err != nil {
				return nil, fmt.Errorf("read add diff %d: %w", i, err)
			}
			for index, value := range diff {
				diff[index] = baseBytes[int(baseOffset)+index] + value
			}
			if _, err := output.Write(diff); err != nil {
				return nil, fmt.Errorf("apply add op %d: %w", i, err)
			}
		case codeDeltaOpSparseAdd:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("sparse add op %d exceeds base payload bounds", i)
			}
			var payloadLength uint64
			if err := binary.Read(reader, binary.BigEndian, &payloadLength); err != nil {
				return nil, fmt.Errorf("read sparse add payload length %d: %w", i, err)
			}
			if payloadLength > uint64(reader.Len()) {
				return nil, fmt.Errorf("sparse add payload %d exceeds remaining delta bytes", i)
			}
			payload := make([]byte, int(payloadLength))
			if _, err := reader.Read(payload); err != nil {
				return nil, fmt.Errorf("read sparse add payload %d: %w", i, err)
			}
			materialized, err := applyCodeDeltaSparseAdd(baseBytes[int(baseOffset):int(end)], payload)
			if err != nil {
				return nil, fmt.Errorf("apply sparse add op %d: %w", i, err)
			}
			if _, err := output.Write(materialized); err != nil {
				return nil, fmt.Errorf("write sparse add op %d: %w", i, err)
			}
		default:
			return nil, fmt.Errorf("unsupported delta op kind %d", kind)
		}
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected trailing delta bytes: %d", reader.Len())
	}
	return output.Bytes(), nil
}

func applyBsdiffCodeDelta(baseBytes []byte, reader *bytes.Reader) ([]byte, error) {
	var output bytes.Buffer
	if err := binarydist.Patch(bytes.NewReader(baseBytes), &output, reader); err != nil {
		return nil, fmt.Errorf("apply bsdiff delta: %w", err)
	}
	return output.Bytes(), nil
}

func applyDictionaryCompressedCodeDelta(baseBytes []byte, reader *bytes.Reader) ([]byte, error) {
	expectedLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read dictionary-compressed delta body length: %w", err)
	}
	compressedBody := make([]byte, reader.Len())
	if _, err := reader.Read(compressedBody); err != nil {
		return nil, fmt.Errorf("read dictionary-compressed delta body: %w", err)
	}
	dictReader := flate.NewReaderDict(bytes.NewReader(compressedBody), baseBytes)
	defer dictReader.Close()
	body, err := io.ReadAll(dictReader)
	if err != nil {
		return nil, fmt.Errorf("decompress dictionary-compressed delta body: %w", err)
	}
	if uint64(len(body)) != expectedLength {
		return nil, fmt.Errorf("dictionary-compressed delta body length mismatch: expected %d got %d", expectedLength, len(body))
	}
	return applyCompactCodeDelta(baseBytes, bytes.NewReader(body))
}

func applySplitDictionaryCodeDelta(baseBytes []byte, reader *bytes.Reader) ([]byte, error) {
	controlLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split control stream length: %w", err)
	}
	compressedControlLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split control stream length: %w", err)
	}
	insertLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split insert stream length: %w", err)
	}
	compressedInsertLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split insert stream length: %w", err)
	}
	addLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split add stream length: %w", err)
	}
	compressedAddLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split add stream length: %w", err)
	}

	compressedControl, err := readCodeDeltaStream(reader, compressedControlLength)
	if err != nil {
		return nil, fmt.Errorf("read split control stream: %w", err)
	}
	compressedInsert, err := readCodeDeltaStream(reader, compressedInsertLength)
	if err != nil {
		return nil, fmt.Errorf("read split insert stream: %w", err)
	}
	compressedAdd, err := readCodeDeltaStream(reader, compressedAddLength)
	if err != nil {
		return nil, fmt.Errorf("read split add stream: %w", err)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected split trailing delta bytes: %d", reader.Len())
	}

	controlStream, err := decompressCodeDeltaBodyWithDict(compressedControl, nil, controlLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split control stream: %w", err)
	}
	insertStream, err := decompressCodeDeltaBodyWithDict(compressedInsert, codeDeltaCompressionDictionary(baseBytes), insertLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split insert stream: %w", err)
	}
	addStream, err := decompressCodeDeltaBodyWithDict(compressedAdd, nil, addLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split add stream: %w", err)
	}
	return applySplitCodeDelta(baseBytes, bytes.NewReader(controlStream), bytes.NewReader(insertStream), bytes.NewReader(addStream))
}

func applySplitContextCodeDelta(baseBytes []byte, reader *bytes.Reader) ([]byte, error) {
	controlLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split control stream length: %w", err)
	}
	compressedControlLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split control stream length: %w", err)
	}
	insertLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split insert stream length: %w", err)
	}
	compressedInsertLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split insert stream length: %w", err)
	}
	addLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split add stream length: %w", err)
	}
	compressedAddLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split add stream length: %w", err)
	}

	compressedControl, err := readCodeDeltaStream(reader, compressedControlLength)
	if err != nil {
		return nil, fmt.Errorf("read split control stream: %w", err)
	}
	compressedInsert, err := readCodeDeltaStream(reader, compressedInsertLength)
	if err != nil {
		return nil, fmt.Errorf("read split insert stream: %w", err)
	}
	compressedAdd, err := readCodeDeltaStream(reader, compressedAddLength)
	if err != nil {
		return nil, fmt.Errorf("read split add stream: %w", err)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected split trailing delta bytes: %d", reader.Len())
	}

	controlStream, err := decompressCodeDeltaBodyWithDict(compressedControl, nil, controlLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split control stream: %w", err)
	}
	insertDictionary, err := codeDeltaInsertContextDictionary(baseBytes, controlStream)
	if err != nil {
		return nil, fmt.Errorf("build insert context dictionary: %w", err)
	}
	insertStream, err := decompressCodeDeltaBodyWithDict(compressedInsert, insertDictionary, insertLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split insert stream: %w", err)
	}
	addStream, err := decompressCodeDeltaBodyWithDict(compressedAdd, nil, addLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split add stream: %w", err)
	}
	return applySplitCodeDelta(baseBytes, bytes.NewReader(controlStream), bytes.NewReader(insertStream), bytes.NewReader(addStream))
}

func applySplitTransformedAddCodeDelta(baseBytes []byte, reader *bytes.Reader) ([]byte, error) {
	addTransform, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split add transform: %w", err)
	}
	controlLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split control stream length: %w", err)
	}
	compressedControlLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split control stream length: %w", err)
	}
	insertLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split insert stream length: %w", err)
	}
	compressedInsertLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split insert stream length: %w", err)
	}
	addLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split add stream length: %w", err)
	}
	compressedAddLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split add stream length: %w", err)
	}

	compressedControl, err := readCodeDeltaStream(reader, compressedControlLength)
	if err != nil {
		return nil, fmt.Errorf("read split control stream: %w", err)
	}
	compressedInsert, err := readCodeDeltaStream(reader, compressedInsertLength)
	if err != nil {
		return nil, fmt.Errorf("read split insert stream: %w", err)
	}
	compressedAdd, err := readCodeDeltaStream(reader, compressedAddLength)
	if err != nil {
		return nil, fmt.Errorf("read split add stream: %w", err)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected split trailing delta bytes: %d", reader.Len())
	}

	controlStream, err := decompressCodeDeltaBodyWithDict(compressedControl, nil, controlLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split control stream: %w", err)
	}
	insertDictionary, err := codeDeltaInsertContextDictionary(baseBytes, controlStream)
	if err != nil {
		return nil, fmt.Errorf("build insert context dictionary: %w", err)
	}
	insertStream, err := decompressCodeDeltaBodyWithDict(compressedInsert, insertDictionary, insertLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split insert stream: %w", err)
	}
	transformedAddStream, err := decompressCodeDeltaBodyWithDict(compressedAdd, nil, addLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split add stream: %w", err)
	}
	addStream, err := restoreCodeDeltaAddStream(transformedAddStream, byte(addTransform))
	if err != nil {
		return nil, err
	}
	return applySplitCodeDelta(baseBytes, bytes.NewReader(controlStream), bytes.NewReader(insertStream), bytes.NewReader(addStream))
}

func applySplitOneOtherAddCodeDelta(baseBytes []byte, reader *bytes.Reader) ([]byte, error) {
	return applySplitOneOtherAddCodeDeltaWithDictionaryBuilder(baseBytes, reader, codeDeltaInsertContextDictionary)
}

func applySplitOneOtherAddCodeDeltaWithDictionaryBuilder(
	baseBytes []byte,
	reader *bytes.Reader,
	dictionaryBuilder func([]byte, []byte) ([]byte, error),
) ([]byte, error) {
	controlLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split control stream length: %w", err)
	}
	compressedControlLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split control stream length: %w", err)
	}
	insertLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split insert stream length: %w", err)
	}
	compressedInsertLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split insert stream length: %w", err)
	}
	addLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split add stream length: %w", err)
	}
	oneGapLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split one-gap stream length: %w", err)
	}
	compressedOneGapLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split one-gap stream length: %w", err)
	}
	otherGapLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split other-gap stream length: %w", err)
	}
	compressedOtherGapLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split other-gap stream length: %w", err)
	}
	otherValueLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read split other-value stream length: %w", err)
	}
	compressedOtherValueLength, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compressed split other-value stream length: %w", err)
	}

	compressedControl, err := readCodeDeltaStream(reader, compressedControlLength)
	if err != nil {
		return nil, fmt.Errorf("read split control stream: %w", err)
	}
	compressedInsert, err := readCodeDeltaStream(reader, compressedInsertLength)
	if err != nil {
		return nil, fmt.Errorf("read split insert stream: %w", err)
	}
	compressedOneGaps, err := readCodeDeltaStream(reader, compressedOneGapLength)
	if err != nil {
		return nil, fmt.Errorf("read split one-gap stream: %w", err)
	}
	compressedOtherGaps, err := readCodeDeltaStream(reader, compressedOtherGapLength)
	if err != nil {
		return nil, fmt.Errorf("read split other-gap stream: %w", err)
	}
	compressedOtherValues, err := readCodeDeltaStream(reader, compressedOtherValueLength)
	if err != nil {
		return nil, fmt.Errorf("read split other-value stream: %w", err)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected split trailing delta bytes: %d", reader.Len())
	}

	controlStream, err := decompressCodeDeltaBodyWithDict(compressedControl, nil, controlLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split control stream: %w", err)
	}
	insertDictionary, err := dictionaryBuilder(baseBytes, controlStream)
	if err != nil {
		return nil, fmt.Errorf("build insert context dictionary: %w", err)
	}
	insertStream, err := decompressCodeDeltaBodyWithDict(compressedInsert, insertDictionary, insertLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split insert stream: %w", err)
	}
	oneGapStream, err := decompressCodeDeltaBodyWithDict(compressedOneGaps, nil, oneGapLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split one-gap stream: %w", err)
	}
	otherGapStream, err := decompressCodeDeltaBodyWithDict(compressedOtherGaps, nil, otherGapLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split other-gap stream: %w", err)
	}
	otherValueStream, err := decompressCodeDeltaBodyWithDict(compressedOtherValues, nil, otherValueLength)
	if err != nil {
		return nil, fmt.Errorf("decompress split other-value stream: %w", err)
	}
	addStream, err := restoreCodeDeltaAddStreamOneOtherGaps(addLength, oneGapStream, otherGapStream, otherValueStream)
	if err != nil {
		return nil, err
	}
	return applySplitCodeDelta(baseBytes, bytes.NewReader(controlStream), bytes.NewReader(insertStream), bytes.NewReader(addStream))
}

func restoreCodeDeltaAddStreamOneOtherGaps(addLength uint64, oneGapStream []byte, otherGapStream []byte, otherValueStream []byte) ([]byte, error) {
	addStream := make([]byte, int(addLength))
	oneReader := bytes.NewReader(oneGapStream)
	oneIndex := -1
	for oneReader.Len() > 0 {
		gap, err := binary.ReadUvarint(oneReader)
		if err != nil {
			return nil, fmt.Errorf("read one-gap add stream: %w", err)
		}
		oneIndex += int(gap)
		if oneIndex < 0 || oneIndex >= len(addStream) {
			return nil, fmt.Errorf("one-gap index %d exceeds add stream length %d", oneIndex, len(addStream))
		}
		addStream[oneIndex] = 1
	}

	otherGapReader := bytes.NewReader(otherGapStream)
	otherValueReader := bytes.NewReader(otherValueStream)
	otherIndex := -1
	for otherGapReader.Len() > 0 {
		gap, err := binary.ReadUvarint(otherGapReader)
		if err != nil {
			return nil, fmt.Errorf("read other-gap add stream: %w", err)
		}
		value, err := otherValueReader.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read other-value add stream: %w", err)
		}
		otherIndex += int(gap)
		if otherIndex < 0 || otherIndex >= len(addStream) {
			return nil, fmt.Errorf("other-pair index %d exceeds add stream length %d", otherIndex, len(addStream))
		}
		if addStream[otherIndex] != 0 {
			return nil, fmt.Errorf("conflicting add stream value at index %d", otherIndex)
		}
		addStream[otherIndex] = value
	}
	if otherValueReader.Len() != 0 {
		return nil, fmt.Errorf("unused other-value add bytes: %d", otherValueReader.Len())
	}
	return addStream, nil
}

func readCodeDeltaStream(reader *bytes.Reader, length uint64) ([]byte, error) {
	if length > uint64(reader.Len()) {
		return nil, fmt.Errorf("stream length %d exceeds remaining bytes %d", length, reader.Len())
	}
	if length == 0 {
		return []byte{}, nil
	}
	stream := make([]byte, int(length))
	if _, err := reader.Read(stream); err != nil {
		return nil, err
	}
	return stream, nil
}

func decompressCodeDeltaBodyWithDict(compressedBody []byte, dict []byte, expectedLength uint64) ([]byte, error) {
	var reader io.ReadCloser
	if len(dict) == 0 {
		reader = flate.NewReader(bytes.NewReader(compressedBody))
	} else {
		reader = flate.NewReaderDict(bytes.NewReader(compressedBody), dict)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if uint64(len(body)) != expectedLength {
		return nil, fmt.Errorf("decompressed stream length mismatch: expected %d got %d", expectedLength, len(body))
	}
	return body, nil
}

func applySplitCodeDelta(baseBytes []byte, controlReader *bytes.Reader, insertReader *bytes.Reader, addReader *bytes.Reader) ([]byte, error) {
	opCount, err := binary.ReadUvarint(controlReader)
	if err != nil {
		return nil, fmt.Errorf("read split delta op count: %w", err)
	}

	var output bytes.Buffer
	for i := uint64(0); i < opCount; i++ {
		kind, err := controlReader.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read split delta op kind %d: %w", i, err)
		}
		baseOffset, err := binary.ReadUvarint(controlReader)
		if err != nil {
			return nil, fmt.Errorf("read split delta op base offset %d: %w", i, err)
		}
		length, err := binary.ReadUvarint(controlReader)
		if err != nil {
			return nil, fmt.Errorf("read split delta op length %d: %w", i, err)
		}

		switch kind {
		case codeDeltaOpCopy:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("split copy op %d exceeds base payload bounds", i)
			}
			if _, err := output.Write(baseBytes[int(baseOffset):int(end)]); err != nil {
				return nil, fmt.Errorf("apply split copy op %d: %w", i, err)
			}
		case codeDeltaOpInsert:
			literal, err := readCodeDeltaStream(insertReader, length)
			if err != nil {
				return nil, fmt.Errorf("read split insert literal %d: %w", i, err)
			}
			if _, err := output.Write(literal); err != nil {
				return nil, fmt.Errorf("apply split insert op %d: %w", i, err)
			}
		case codeDeltaOpAdd:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("split add op %d exceeds base payload bounds", i)
			}
			diff, err := readCodeDeltaStream(addReader, length)
			if err != nil {
				return nil, fmt.Errorf("read split add diff %d: %w", i, err)
			}
			for index, value := range diff {
				diff[index] = baseBytes[int(baseOffset)+index] + value
			}
			if _, err := output.Write(diff); err != nil {
				return nil, fmt.Errorf("apply split add op %d: %w", i, err)
			}
		case codeDeltaOpSparseAdd:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("split sparse add op %d exceeds base payload bounds", i)
			}
			payloadLength, err := binary.ReadUvarint(controlReader)
			if err != nil {
				return nil, fmt.Errorf("read split sparse add payload length %d: %w", i, err)
			}
			payload, err := readCodeDeltaStream(addReader, payloadLength)
			if err != nil {
				return nil, fmt.Errorf("read split sparse add payload %d: %w", i, err)
			}
			materialized, err := applyCodeDeltaSparseAdd(baseBytes[int(baseOffset):int(end)], payload)
			if err != nil {
				return nil, fmt.Errorf("apply split sparse add op %d: %w", i, err)
			}
			if _, err := output.Write(materialized); err != nil {
				return nil, fmt.Errorf("write split sparse add op %d: %w", i, err)
			}
		case codeDeltaOpOutputCopy:
			end := baseOffset + length
			outputBytes := output.Bytes()
			if end > uint64(len(outputBytes)) {
				return nil, fmt.Errorf("split output-copy op %d exceeds materialized output bounds", i)
			}
			segment := append([]byte(nil), outputBytes[int(baseOffset):int(end)]...)
			if _, err := output.Write(segment); err != nil {
				return nil, fmt.Errorf("apply split output-copy op %d: %w", i, err)
			}
		default:
			return nil, fmt.Errorf("unsupported split delta op kind %d", kind)
		}
	}
	if controlReader.Len() != 0 || insertReader.Len() != 0 || addReader.Len() != 0 {
		return nil, fmt.Errorf("unexpected split delta trailing bytes: control=%d insert=%d add=%d", controlReader.Len(), insertReader.Len(), addReader.Len())
	}
	return output.Bytes(), nil
}

func applyCompactCodeDelta(baseBytes []byte, reader *bytes.Reader) ([]byte, error) {
	opCount, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("read compact delta op count: %w", err)
	}

	var output bytes.Buffer
	for i := uint64(0); i < opCount; i++ {
		kind, err := reader.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read compact delta op kind %d: %w", i, err)
		}
		baseOffset, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read compact delta op base offset %d: %w", i, err)
		}
		length, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read compact delta op length %d: %w", i, err)
		}

		switch kind {
		case codeDeltaOpCopy:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("compact copy op %d exceeds base payload bounds", i)
			}
			if _, err := output.Write(baseBytes[int(baseOffset):int(end)]); err != nil {
				return nil, fmt.Errorf("apply compact copy op %d: %w", i, err)
			}
		case codeDeltaOpInsert:
			if length > uint64(reader.Len()) {
				return nil, fmt.Errorf("compact insert op %d exceeds remaining delta bytes", i)
			}
			literal := make([]byte, int(length))
			if _, err := reader.Read(literal); err != nil {
				return nil, fmt.Errorf("read compact insert literal %d: %w", i, err)
			}
			if _, err := output.Write(literal); err != nil {
				return nil, fmt.Errorf("apply compact insert op %d: %w", i, err)
			}
		case codeDeltaOpAdd:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("compact add op %d exceeds base payload bounds", i)
			}
			if length > uint64(reader.Len()) {
				return nil, fmt.Errorf("compact add op %d exceeds remaining delta bytes", i)
			}
			diff := make([]byte, int(length))
			if _, err := reader.Read(diff); err != nil {
				return nil, fmt.Errorf("read compact add diff %d: %w", i, err)
			}
			for index, value := range diff {
				diff[index] = baseBytes[int(baseOffset)+index] + value
			}
			if _, err := output.Write(diff); err != nil {
				return nil, fmt.Errorf("apply compact add op %d: %w", i, err)
			}
		case codeDeltaOpSparseAdd:
			end := baseOffset + length
			if end > uint64(len(baseBytes)) {
				return nil, fmt.Errorf("compact sparse add op %d exceeds base payload bounds", i)
			}
			payloadLength, err := binary.ReadUvarint(reader)
			if err != nil {
				return nil, fmt.Errorf("read compact sparse add payload length %d: %w", i, err)
			}
			if payloadLength > uint64(reader.Len()) {
				return nil, fmt.Errorf("compact sparse add payload %d exceeds remaining delta bytes", i)
			}
			payload := make([]byte, int(payloadLength))
			if _, err := reader.Read(payload); err != nil {
				return nil, fmt.Errorf("read compact sparse add payload %d: %w", i, err)
			}
			materialized, err := applyCodeDeltaSparseAdd(baseBytes[int(baseOffset):int(end)], payload)
			if err != nil {
				return nil, fmt.Errorf("apply compact sparse add op %d: %w", i, err)
			}
			if _, err := output.Write(materialized); err != nil {
				return nil, fmt.Errorf("write compact sparse add op %d: %w", i, err)
			}
		default:
			return nil, fmt.Errorf("unsupported compact delta op kind %d", kind)
		}
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("unexpected compact trailing delta bytes: %d", reader.Len())
	}
	return output.Bytes(), nil
}

func buildCodeDeltaOps(baseBytes []byte, candidateBytes []byte) []codeDeltaOp {
	if len(candidateBytes) == 0 {
		return nil
	}

	ops := make([]codeDeltaOp, 0)
	baseIndex := 0
	candidateIndex := 0

	for candidateIndex < len(candidateBytes) {
		if baseIndex < len(baseBytes) && baseBytes[baseIndex] == candidateBytes[candidateIndex] {
			matchStartBase := baseIndex
			for baseIndex < len(baseBytes) && candidateIndex < len(candidateBytes) && baseBytes[baseIndex] == candidateBytes[candidateIndex] {
				baseIndex++
				candidateIndex++
			}
			ops = append(ops, codeDeltaOp{
				Kind:       codeDeltaOpCopy,
				BaseOffset: uint64(matchStartBase),
				Length:     uint64(baseIndex - matchStartBase),
			})
			continue
		}

		anchorBase, anchorCandidate, found := findCodeDeltaResync(baseBytes, candidateBytes, baseIndex, candidateIndex)
		if !found {
			ops = append(ops, codeDeltaOp{
				Kind:    codeDeltaOpInsert,
				Length:  uint64(len(candidateBytes) - candidateIndex),
				Literal: append([]byte(nil), candidateBytes[candidateIndex:]...),
			})
			break
		}

		if anchorCandidate > candidateIndex {
			ops = append(ops, codeDeltaOp{
				Kind:    codeDeltaOpInsert,
				Length:  uint64(anchorCandidate - candidateIndex),
				Literal: append([]byte(nil), candidateBytes[candidateIndex:anchorCandidate]...),
			})
		}
		baseIndex = anchorBase
		candidateIndex = anchorCandidate
	}

	return ops
}

func findCodeDeltaResync(baseBytes []byte, candidateBytes []byte, baseIndex int, candidateIndex int) (int, int, bool) {
	if baseIndex >= len(baseBytes) || candidateIndex >= len(candidateBytes) {
		return 0, 0, false
	}

	baseWindowEnd := minInt(len(baseBytes), baseIndex+codeDeltaAnchorLookahead)
	candidateWindowEnd := minInt(len(candidateBytes), candidateIndex+codeDeltaAnchorLookahead)

	anchorLength := minInt(codeDeltaMinAnchorBytes, minInt(baseWindowEnd-baseIndex, candidateWindowEnd-candidateIndex))
	if anchorLength < codeDeltaMinTailAnchorBytes {
		anchorLength = minInt(codeDeltaMinTailAnchorBytes, minInt(len(baseBytes)-baseIndex, len(candidateBytes)-candidateIndex))
	}
	if anchorLength < codeDeltaMinTailAnchorBytes {
		return 0, 0, false
	}

	for candidateAnchorStart := candidateIndex; candidateAnchorStart+anchorLength <= candidateWindowEnd; candidateAnchorStart++ {
		anchor := candidateBytes[candidateAnchorStart : candidateAnchorStart+anchorLength]
		if relativeBaseIndex := bytes.Index(baseBytes[baseIndex:baseWindowEnd], anchor); relativeBaseIndex >= 0 {
			return baseIndex + relativeBaseIndex, candidateAnchorStart, true
		}
	}
	return 0, 0, false
}

func buildCodeDeltaOpsV2(baseBytes []byte, candidateBytes []byte) []codeDeltaOp {
	if len(candidateBytes) == 0 {
		return nil
	}
	if len(baseBytes) < codeDeltaV2AnchorBytes || len(candidateBytes) < codeDeltaV2AnchorBytes {
		return []codeDeltaOp{{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len(candidateBytes)),
			Literal: append([]byte(nil), candidateBytes...),
		}}
	}

	index := buildCodeDeltaV2AnchorIndex(baseBytes)
	ops := make([]codeDeltaOp, 0)
	pendingInsert := make([]byte, 0)
	flushInsert := func() {
		if len(pendingInsert) == 0 {
			return
		}
		ops = append(ops, codeDeltaOp{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len(pendingInsert)),
			Literal: append([]byte(nil), pendingInsert...),
		})
		pendingInsert = pendingInsert[:0]
	}

	for candidateIndex := 0; candidateIndex < len(candidateBytes); {
		baseOffset, length, found := findCodeDeltaV2Copy(baseBytes, candidateBytes, index, candidateIndex)
		if found {
			flushInsert()
			ops = append(ops, codeDeltaOp{
				Kind:       codeDeltaOpCopy,
				BaseOffset: uint64(baseOffset),
				Length:     uint64(length),
			})
			candidateIndex += length
			continue
		}
		pendingInsert = append(pendingInsert, candidateBytes[candidateIndex])
		candidateIndex++
	}
	flushInsert()
	return ops
}

func buildCodeDeltaV2AnchorIndex(baseBytes []byte) map[[codeDeltaV2AnchorBytes]byte][]int {
	index := make(map[[codeDeltaV2AnchorBytes]byte][]int, len(baseBytes)/codeDeltaV2AnchorStride)
	for offset := 0; offset+codeDeltaV2AnchorBytes <= len(baseBytes); offset += codeDeltaV2AnchorStride {
		key := codeDeltaV2AnchorKey(baseBytes, offset)
		offsets := index[key]
		if len(offsets) >= codeDeltaV2MaxOffsetsPerAnchor {
			continue
		}
		index[key] = append(offsets, offset)
	}
	return index
}

func findCodeDeltaV2Copy(
	baseBytes []byte,
	candidateBytes []byte,
	index map[[codeDeltaV2AnchorBytes]byte][]int,
	candidateIndex int,
) (int, int, bool) {
	if candidateIndex+codeDeltaV2AnchorBytes > len(candidateBytes) {
		return 0, 0, false
	}
	offsets := index[codeDeltaV2AnchorKey(candidateBytes, candidateIndex)]
	if len(offsets) == 0 {
		return 0, 0, false
	}

	bestBaseOffset := 0
	bestLength := 0
	for _, baseOffset := range offsets {
		length := commonPrefixLength(baseBytes[baseOffset:], candidateBytes[candidateIndex:])
		if length > bestLength {
			bestLength = length
			bestBaseOffset = baseOffset
		}
	}
	if bestLength < codeDeltaV2MinCopyBytes {
		return 0, 0, false
	}
	return bestBaseOffset, bestLength, true
}

func buildCodeDeltaOpsV5(baseBytes []byte, candidateBytes []byte) []codeDeltaOp {
	if len(candidateBytes) == 0 {
		return nil
	}
	if len(baseBytes) < codeDeltaV5SeedBytes || len(candidateBytes) < codeDeltaV5SeedBytes {
		return []codeDeltaOp{{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len(candidateBytes)),
			Literal: append([]byte(nil), candidateBytes...),
		}}
	}

	index := suffixarray.New(baseBytes)
	ops := make([]codeDeltaOp, 0)
	pendingInsert := make([]byte, 0)
	flushInsert := func() {
		if len(pendingInsert) == 0 {
			return
		}
		ops = append(ops, codeDeltaOp{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len(pendingInsert)),
			Literal: append([]byte(nil), pendingInsert...),
		})
		pendingInsert = pendingInsert[:0]
	}

	for candidateIndex := 0; candidateIndex < len(candidateBytes); {
		baseOffset, length, found := findCodeDeltaV5Copy(baseBytes, candidateBytes, index, candidateIndex)
		if found {
			flushInsert()
			ops = append(ops, codeDeltaOp{
				Kind:       codeDeltaOpCopy,
				BaseOffset: uint64(baseOffset),
				Length:     uint64(length),
			})
			candidateIndex += length
			continue
		}
		pendingInsert = append(pendingInsert, candidateBytes[candidateIndex])
		candidateIndex++
	}
	flushInsert()
	return ops
}

func buildCodeDeltaOpsV6(baseBytes []byte, candidateBytes []byte) []codeDeltaOp {
	if len(candidateBytes) == 0 {
		return nil
	}
	if len(baseBytes) < codeDeltaV5SeedBytes || len(candidateBytes) < codeDeltaV5SeedBytes {
		return []codeDeltaOp{{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len(candidateBytes)),
			Literal: append([]byte(nil), candidateBytes...),
		}}
	}

	index := suffixarray.New(baseBytes)
	ops := make([]codeDeltaOp, 0)
	pendingInsert := make([]byte, 0)
	flushInsert := func() {
		if len(pendingInsert) == 0 {
			return
		}
		ops = append(ops, codeDeltaOp{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len(pendingInsert)),
			Literal: append([]byte(nil), pendingInsert...),
		})
		pendingInsert = pendingInsert[:0]
	}

	for candidateIndex := 0; candidateIndex < len(candidateBytes); {
		baseOffset, length, found := findCodeDeltaV6Copy(baseBytes, candidateBytes, index, candidateIndex)
		if found {
			matchedLength := length
			extension := codeDeltaBackwardCopyExtension(baseBytes, pendingInsert, baseOffset)
			if extension > 0 {
				baseOffset -= extension
				length += extension
				pendingInsert = pendingInsert[:len(pendingInsert)-extension]
			}
			flushInsert()
			ops = append(ops, codeDeltaOp{
				Kind:       codeDeltaOpCopy,
				BaseOffset: uint64(baseOffset),
				Length:     uint64(length),
			})
			candidateIndex += matchedLength
			continue
		}
		pendingInsert = append(pendingInsert, candidateBytes[candidateIndex])
		candidateIndex++
	}
	flushInsert()
	return ops
}

func buildCodeDeltaOpsV8(baseBytes []byte, candidateBytes []byte) []codeDeltaOp {
	if len(candidateBytes) == 0 {
		return nil
	}
	if len(baseBytes) < codeDeltaV5SeedBytes || len(candidateBytes) < codeDeltaV5SeedBytes {
		return []codeDeltaOp{{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len(candidateBytes)),
			Literal: append([]byte(nil), candidateBytes...),
		}}
	}

	index := suffixarray.New(baseBytes)
	ops := make([]codeDeltaOp, 0)
	pendingInsert := make([]byte, 0)
	flushInsert := func() {
		if len(pendingInsert) == 0 {
			return
		}
		ops = append(ops, codeDeltaOp{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len(pendingInsert)),
			Literal: append([]byte(nil), pendingInsert...),
		})
		pendingInsert = pendingInsert[:0]
	}

	for candidateIndex := 0; candidateIndex < len(candidateBytes); {
		baseOffset, length, found := findCodeDeltaV8Copy(baseBytes, candidateBytes, index, candidateIndex)
		if found {
			matchedLength := length
			extension := codeDeltaBackwardCopyExtension(baseBytes, pendingInsert, baseOffset)
			if extension > 0 {
				baseOffset -= extension
				length += extension
				pendingInsert = pendingInsert[:len(pendingInsert)-extension]
			}
			flushInsert()
			ops = append(ops, codeDeltaOp{
				Kind:       codeDeltaOpCopy,
				BaseOffset: uint64(baseOffset),
				Length:     uint64(length),
			})
			candidateIndex += matchedLength
			continue
		}
		pendingInsert = append(pendingInsert, candidateBytes[candidateIndex])
		candidateIndex++
	}
	flushInsert()
	return ops
}

func buildCodeDeltaOpsV10OutputCopy(baseBytes []byte, ops []codeDeltaOp) []codeDeltaOp {
	if len(ops) == 0 {
		return nil
	}

	outputOps := make([]codeDeltaOp, 0, len(ops))
	materialized := make([]byte, 0)
	for _, op := range ops {
		if op.Kind == codeDeltaOpInsert {
			outputOps = appendCodeDeltaV10OutputCopyOps(outputOps, &materialized, op.Literal)
			continue
		}

		outputOps = append(outputOps, op)
		switch op.Kind {
		case codeDeltaOpCopy:
			end := int(op.BaseOffset + op.Length)
			if end <= len(baseBytes) {
				materialized = append(materialized, baseBytes[int(op.BaseOffset):end]...)
			}
		case codeDeltaOpAdd:
			end := int(op.BaseOffset + op.Length)
			if end <= len(baseBytes) && len(op.Literal) == int(op.Length) {
				for index, diff := range op.Literal {
					materialized = append(materialized, baseBytes[int(op.BaseOffset)+index]+diff)
				}
			}
		case codeDeltaOpSparseAdd:
			end := int(op.BaseOffset + op.Length)
			if end <= len(baseBytes) {
				materializedOp, err := applyCodeDeltaSparseAdd(baseBytes[int(op.BaseOffset):end], op.Literal)
				if err == nil {
					materialized = append(materialized, materializedOp...)
				}
			}
		}
	}
	return outputOps
}

func appendCodeDeltaV10OutputCopyOps(outputOps []codeDeltaOp, materialized *[]byte, literal []byte) []codeDeltaOp {
	pendingInsert := make([]byte, 0)
	flushInsert := func() {
		if len(pendingInsert) == 0 {
			return
		}
		outputOps = append(outputOps, codeDeltaOp{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len(pendingInsert)),
			Literal: append([]byte(nil), pendingInsert...),
		})
		pendingInsert = pendingInsert[:0]
	}

	for literalIndex := 0; literalIndex < len(literal); {
		outputOffset, length, found := findCodeDeltaV10OutputCopy(*materialized, literal, literalIndex)
		if found {
			flushInsert()
			copiedBytes := append([]byte(nil), (*materialized)[outputOffset:outputOffset+length]...)
			outputOps = append(outputOps, codeDeltaOp{
				Kind:       codeDeltaOpOutputCopy,
				BaseOffset: uint64(outputOffset),
				Length:     uint64(length),
			})
			*materialized = append(*materialized, copiedBytes...)
			literalIndex += length
			continue
		}

		pendingInsert = append(pendingInsert, literal[literalIndex])
		*materialized = append(*materialized, literal[literalIndex])
		literalIndex++
	}
	flushInsert()
	return outputOps
}

func findCodeDeltaV10OutputCopy(materialized []byte, literal []byte, literalIndex int) (int, int, bool) {
	remaining := len(literal) - literalIndex
	if len(materialized) < codeDeltaV10MinOutputCopyBytes || remaining < codeDeltaV10MinOutputCopyBytes {
		return 0, 0, false
	}

	seedLength := minInt(codeDeltaV10SeedBytes, remaining)
	seed := literal[literalIndex : literalIndex+seedLength]
	bestOffset := 0
	bestLength := 0
	matches := 0
	searchStart := 0
	for searchStart+seedLength <= len(materialized) && matches < codeDeltaV10MaxOutputCopyMatches {
		relativeOffset := bytes.Index(materialized[searchStart:], seed)
		if relativeOffset < 0 {
			break
		}
		outputOffset := searchStart + relativeOffset
		length := commonPrefixLength(materialized[outputOffset:], literal[literalIndex:])
		if length > bestLength {
			bestOffset = outputOffset
			bestLength = length
		}
		searchStart = outputOffset + 1
		matches++
	}
	if bestLength < codeDeltaV10MinOutputCopyBytes {
		return 0, 0, false
	}
	return bestOffset, bestLength, true
}

type codeDeltaOutputCopyIndex struct {
	data         []byte
	offsets      map[[codeDeltaV10SeedBytes]byte][]int
	indexedUntil int
}

func newCodeDeltaOutputCopyIndex() *codeDeltaOutputCopyIndex {
	return &codeDeltaOutputCopyIndex{
		offsets: make(map[[codeDeltaV10SeedBytes]byte][]int),
	}
}

func (index *codeDeltaOutputCopyIndex) appendBytes(bytes []byte) {
	if len(bytes) == 0 {
		return
	}
	index.data = append(index.data, bytes...)
	index.indexNewSeeds()
}

func (index *codeDeltaOutputCopyIndex) indexNewSeeds() {
	for index.indexedUntil+codeDeltaV10SeedBytes <= len(index.data) {
		key := codeDeltaOutputCopySeedKey(index.data, index.indexedUntil)
		offsets := index.offsets[key]
		if len(offsets) >= codeDeltaV11MaxIndexedOffsetsPerSeed {
			copy(offsets, offsets[1:])
			offsets[len(offsets)-1] = index.indexedUntil
		} else {
			offsets = append(offsets, index.indexedUntil)
		}
		index.offsets[key] = offsets
		index.indexedUntil++
	}
}

func (index *codeDeltaOutputCopyIndex) find(candidateBytes []byte, candidateIndex int) (int, int, bool) {
	remaining := len(candidateBytes) - candidateIndex
	if len(index.data) < codeDeltaV10MinOutputCopyBytes || remaining < codeDeltaV10MinOutputCopyBytes {
		return 0, 0, false
	}

	key := codeDeltaOutputCopySeedKey(candidateBytes, candidateIndex)
	offsets := index.offsets[key]
	if len(offsets) == 0 {
		return 0, 0, false
	}

	bestOffset := 0
	bestLength := 0
	for offsetIndex := len(offsets) - 1; offsetIndex >= 0; offsetIndex-- {
		outputOffset := offsets[offsetIndex]
		length := commonPrefixLength(index.data[outputOffset:], candidateBytes[candidateIndex:])
		if length > bestLength {
			bestOffset = outputOffset
			bestLength = length
		}
	}
	if bestLength < codeDeltaV10MinOutputCopyBytes {
		return 0, 0, false
	}
	return bestOffset, bestLength, true
}

func codeDeltaOutputCopySeedKey(bytes []byte, offset int) [codeDeltaV10SeedBytes]byte {
	var key [codeDeltaV10SeedBytes]byte
	copy(key[:], bytes[offset:offset+codeDeltaV10SeedBytes])
	return key
}

func buildCodeDeltaOpsV11IndexedOutputCopy(baseBytes []byte, ops []codeDeltaOp) []codeDeltaOp {
	if len(ops) == 0 {
		return nil
	}

	outputOps := make([]codeDeltaOp, 0, len(ops))
	index := newCodeDeltaOutputCopyIndex()
	for _, op := range ops {
		switch op.Kind {
		case codeDeltaOpInsert:
			outputOps = appendCodeDeltaV11IndexedInsertOps(outputOps, index, op.Literal)
		case codeDeltaOpAdd:
			outputOps = appendCodeDeltaV11IndexedAddOps(outputOps, index, baseBytes, op)
		default:
			outputOps = append(outputOps, op)
			if materialized, ok := materializeCodeDeltaOpForOutputIndex(baseBytes, op); ok {
				index.appendBytes(materialized)
			}
		}
	}
	return outputOps
}

func buildCodeDeltaOpsV12SparseAdd(baseBytes []byte, ops []codeDeltaOp) []codeDeltaOp {
	if len(ops) == 0 {
		return nil
	}

	candidate := make([]codeDeltaOp, 0, len(ops))
	converted := false
	for _, op := range ops {
		if op.Kind != codeDeltaOpAdd || int(op.BaseOffset)+int(op.Length) > len(baseBytes) {
			candidate = append(candidate, op)
			continue
		}
		sparseOp, ok := codeDeltaV12SparseAddOp(op)
		if ok {
			candidate = append(candidate, sparseOp)
			converted = true
			continue
		}
		candidate = append(candidate, op)
	}
	if converted && scoreSplitCodeDeltaOps(candidate) < scoreSplitCodeDeltaOps(ops) {
		return candidate
	}
	return ops
}

func codeDeltaV12SparseAddOp(op codeDeltaOp) (codeDeltaOp, bool) {
	if op.Length < codeDeltaV4MinSparseAddBytes || len(op.Literal) != int(op.Length) {
		return codeDeltaOp{}, false
	}

	payload := encodeCodeDeltaSparseAddPayload(op.Literal)
	if len(payload) == 0 {
		return codeDeltaOp{
			Kind:       codeDeltaOpCopy,
			BaseOffset: op.BaseOffset,
			Length:     op.Length,
		}, true
	}
	return codeDeltaOp{
		Kind:       codeDeltaOpSparseAdd,
		BaseOffset: op.BaseOffset,
		Length:     op.Length,
		Literal:    payload,
	}, true
}

func appendCodeDeltaV11IndexedInsertOps(outputOps []codeDeltaOp, index *codeDeltaOutputCopyIndex, literal []byte) []codeDeltaOp {
	pendingInsert := make([]byte, 0)
	flushInsert := func() {
		if len(pendingInsert) == 0 {
			return
		}
		outputOps = append(outputOps, codeDeltaOp{
			Kind:    codeDeltaOpInsert,
			Length:  uint64(len(pendingInsert)),
			Literal: append([]byte(nil), pendingInsert...),
		})
		pendingInsert = pendingInsert[:0]
	}

	for literalIndex := 0; literalIndex < len(literal); {
		outputOffset, length, found := index.find(literal, literalIndex)
		if found {
			flushInsert()
			copiedBytes := append([]byte(nil), index.data[outputOffset:outputOffset+length]...)
			outputOps = append(outputOps, codeDeltaOp{
				Kind:       codeDeltaOpOutputCopy,
				BaseOffset: uint64(outputOffset),
				Length:     uint64(length),
			})
			index.appendBytes(copiedBytes)
			literalIndex += length
			continue
		}

		pendingInsert = append(pendingInsert, literal[literalIndex])
		index.appendBytes(literal[literalIndex : literalIndex+1])
		literalIndex++
	}
	flushInsert()
	return outputOps
}

func appendCodeDeltaV11IndexedAddOps(outputOps []codeDeltaOp, index *codeDeltaOutputCopyIndex, baseBytes []byte, op codeDeltaOp) []codeDeltaOp {
	baseOffset := int(op.BaseOffset)
	length := int(op.Length)
	if length == 0 || baseOffset < 0 || baseOffset+length > len(baseBytes) || len(op.Literal) != length {
		outputOps = append(outputOps, op)
		return outputOps
	}

	addBytes := make([]byte, length)
	for diffIndex, diff := range op.Literal {
		addBytes[diffIndex] = baseBytes[baseOffset+diffIndex] + diff
	}

	candidateOps := buildCodeDeltaV11IndexedAddCandidateOps(index, baseOffset, op.Literal, addBytes)
	if len(candidateOps) > 0 && scoreSplitCodeDeltaOps(candidateOps) < scoreSplitCodeDeltaOps([]codeDeltaOp{op}) {
		outputOps = append(outputOps, candidateOps...)
	} else {
		outputOps = append(outputOps, op)
	}
	index.appendBytes(addBytes)
	return outputOps
}

func buildCodeDeltaV11IndexedAddCandidateOps(index *codeDeltaOutputCopyIndex, baseOffset int, diff []byte, addBytes []byte) []codeDeltaOp {
	candidateOps := make([]codeDeltaOp, 0, 1)
	pendingStart := 0
	pendingDiff := make([]byte, 0)
	flushAdd := func() {
		if len(pendingDiff) == 0 {
			return
		}
		candidateOps = append(candidateOps, codeDeltaOp{
			Kind:       codeDeltaOpAdd,
			BaseOffset: uint64(baseOffset + pendingStart),
			Length:     uint64(len(pendingDiff)),
			Literal:    append([]byte(nil), pendingDiff...),
		})
		pendingDiff = pendingDiff[:0]
	}

	for addIndex := 0; addIndex < len(addBytes); {
		outputOffset, copyLength, found := index.find(addBytes, addIndex)
		if found {
			flushAdd()
			candidateOps = append(candidateOps, codeDeltaOp{
				Kind:       codeDeltaOpOutputCopy,
				BaseOffset: uint64(outputOffset),
				Length:     uint64(copyLength),
			})
			addIndex += copyLength
			pendingStart = addIndex
			continue
		}

		if len(pendingDiff) == 0 {
			pendingStart = addIndex
		}
		pendingDiff = append(pendingDiff, diff[addIndex])
		addIndex++
	}
	flushAdd()
	return candidateOps
}

func scoreSplitCodeDeltaOps(ops []codeDeltaOp) int {
	controlStream, insertStream, addStream, err := encodeSplitCodeDeltaStreams(ops)
	if err != nil {
		return 1 << 30
	}
	return compressedCodeDeltaBlockSize(controlStream) +
		compressedCodeDeltaBlockSize(insertStream) +
		compressedCodeDeltaBlockSize(addStream)
}

func materializeCodeDeltaOpForOutputIndex(baseBytes []byte, op codeDeltaOp) ([]byte, bool) {
	switch op.Kind {
	case codeDeltaOpCopy:
		end := int(op.BaseOffset + op.Length)
		if end > len(baseBytes) {
			return nil, false
		}
		return baseBytes[int(op.BaseOffset):end], true
	case codeDeltaOpAdd:
		end := int(op.BaseOffset + op.Length)
		if end > len(baseBytes) || len(op.Literal) != int(op.Length) {
			return nil, false
		}
		output := make([]byte, int(op.Length))
		for index, diff := range op.Literal {
			output[index] = baseBytes[int(op.BaseOffset)+index] + diff
		}
		return output, true
	case codeDeltaOpSparseAdd:
		end := int(op.BaseOffset + op.Length)
		if end > len(baseBytes) {
			return nil, false
		}
		output, err := applyCodeDeltaSparseAdd(baseBytes[int(op.BaseOffset):end], op.Literal)
		if err != nil {
			return nil, false
		}
		return output, true
	default:
		return nil, false
	}
}

func findCodeDeltaV5Copy(
	baseBytes []byte,
	candidateBytes []byte,
	index *suffixarray.Index,
	candidateIndex int,
) (int, int, bool) {
	if candidateIndex+codeDeltaV5SeedBytes > len(candidateBytes) {
		return 0, 0, false
	}

	seed := candidateBytes[candidateIndex : candidateIndex+codeDeltaV5SeedBytes]
	offsets := index.Lookup(seed, codeDeltaV5MaxMatches)
	if len(offsets) == 0 {
		return 0, 0, false
	}

	bestBaseOffset := 0
	bestLength := 0
	for _, baseOffset := range offsets {
		length := commonPrefixLength(baseBytes[baseOffset:], candidateBytes[candidateIndex:])
		if length > bestLength {
			bestLength = length
			bestBaseOffset = baseOffset
		}
	}
	if bestLength < codeDeltaV5MinCopyBytes {
		return 0, 0, false
	}
	return bestBaseOffset, bestLength, true
}

func findCodeDeltaV6Copy(
	baseBytes []byte,
	candidateBytes []byte,
	index *suffixarray.Index,
	candidateIndex int,
) (int, int, bool) {
	if candidateIndex+codeDeltaV5SeedBytes > len(candidateBytes) {
		return 0, 0, false
	}

	seed := candidateBytes[candidateIndex : candidateIndex+codeDeltaV5SeedBytes]
	offsets := index.Lookup(seed, codeDeltaV6MaxMatches)
	if len(offsets) == 0 {
		return 0, 0, false
	}

	bestBaseOffset := 0
	bestLength := 0
	for _, baseOffset := range offsets {
		length := commonPrefixLength(baseBytes[baseOffset:], candidateBytes[candidateIndex:])
		if length > bestLength {
			bestLength = length
			bestBaseOffset = baseOffset
		}
	}
	if bestLength < codeDeltaV2MinCopyBytes {
		return 0, 0, false
	}
	return bestBaseOffset, bestLength, true
}

func findCodeDeltaV8Copy(
	baseBytes []byte,
	candidateBytes []byte,
	index *suffixarray.Index,
	candidateIndex int,
) (int, int, bool) {
	baseOffset, length, found := findCodeDeltaV6Copy(baseBytes, candidateBytes, index, candidateIndex)
	if !found || length < codeDeltaV8MinCopyBytes {
		return 0, 0, false
	}
	return baseOffset, length, true
}

func codeDeltaBackwardCopyExtension(baseBytes []byte, pendingInsert []byte, baseOffset int) int {
	extension := 0
	for extension < len(pendingInsert) && extension < baseOffset {
		if baseBytes[baseOffset-extension-1] != pendingInsert[len(pendingInsert)-extension-1] {
			break
		}
		extension++
	}
	return extension
}

func buildCodeDeltaOpsV3(baseBytes []byte, ops []codeDeltaOp) []codeDeltaOp {
	if len(ops) == 0 {
		return nil
	}

	output := make([]codeDeltaOp, 0, len(ops))
	candidateOffset := 0
	previousBaseEnd := -1
	for index, op := range ops {
		switch op.Kind {
		case codeDeltaOpCopy:
			output = append(output, op)
			candidateOffset += int(op.Length)
			previousBaseEnd = int(op.BaseOffset + op.Length)
		case codeDeltaOpInsert:
			addOp, ok := bestCodeDeltaV3AddOp(baseBytes, op.Literal, candidateOffset, previousBaseEnd, nextCodeDeltaCopyBaseOffset(ops, index))
			if ok {
				output = append(output, addOp)
			} else {
				output = append(output, op)
			}
			candidateOffset += int(op.Length)
		default:
			output = append(output, op)
			candidateOffset += int(op.Length)
		}
	}
	return output
}

func buildCodeDeltaOpsV6Add(baseBytes []byte, ops []codeDeltaOp) []codeDeltaOp {
	if len(ops) == 0 {
		return nil
	}

	output := make([]codeDeltaOp, 0, len(ops))
	candidateOffset := 0
	previousBaseEnd := -1
	for index, op := range ops {
		switch op.Kind {
		case codeDeltaOpCopy:
			output = append(output, op)
			candidateOffset += int(op.Length)
			previousBaseEnd = int(op.BaseOffset + op.Length)
		case codeDeltaOpInsert:
			addOp, ok := bestCodeDeltaV6AddOp(baseBytes, op.Literal, candidateOffset, previousBaseEnd, nextCodeDeltaCopyBaseOffset(ops, index))
			if ok {
				output = append(output, addOp)
			} else {
				output = append(output, op)
			}
			candidateOffset += int(op.Length)
		default:
			output = append(output, op)
			candidateOffset += int(op.Length)
			if op.Kind == codeDeltaOpAdd || op.Kind == codeDeltaOpSparseAdd {
				previousBaseEnd = int(op.BaseOffset + op.Length)
			}
		}
	}
	return output
}

func nextCodeDeltaCopyBaseOffset(ops []codeDeltaOp, start int) int {
	for index := start + 1; index < len(ops); index++ {
		if ops[index].Kind == codeDeltaOpCopy {
			return int(ops[index].BaseOffset)
		}
	}
	return -1
}

func bestCodeDeltaV3AddOp(baseBytes []byte, literal []byte, candidateOffset int, previousBaseEnd int, nextBaseOffset int) (codeDeltaOp, bool) {
	if len(literal) < codeDeltaV3MinAddBytes {
		return codeDeltaOp{}, false
	}

	candidates := []int{candidateOffset}
	if previousBaseEnd >= 0 {
		candidates = append(candidates, previousBaseEnd)
	}
	if nextBaseOffset >= len(literal) {
		candidates = append(candidates, nextBaseOffset-len(literal))
	}

	literalScore := compressedCodeDeltaBlockSize(literal)
	bestScore := literalScore
	bestOffset := -1
	var bestDiff []byte
	seen := make(map[int]bool, len(candidates))
	for _, offset := range candidates {
		if seen[offset] || offset < 0 || offset+len(literal) > len(baseBytes) {
			continue
		}
		seen[offset] = true
		diff := buildCodeDeltaAddDiff(baseBytes[offset:offset+len(literal)], literal)
		score := compressedCodeDeltaBlockSize(diff)
		if score < bestScore {
			bestScore = score
			bestOffset = offset
			bestDiff = diff
		}
	}
	if bestOffset < 0 {
		return codeDeltaOp{}, false
	}
	return codeDeltaOp{
		Kind:       codeDeltaOpAdd,
		BaseOffset: uint64(bestOffset),
		Length:     uint64(len(bestDiff)),
		Literal:    bestDiff,
	}, true
}

func bestCodeDeltaV6AddOp(baseBytes []byte, literal []byte, candidateOffset int, previousBaseEnd int, nextBaseOffset int) (codeDeltaOp, bool) {
	if len(literal) < codeDeltaV3MinAddBytes {
		return codeDeltaOp{}, false
	}

	literalScore := compressedCodeDeltaBlockSize(literal)
	bestScore := literalScore
	bestOffset := -1
	var bestDiff []byte
	for _, offset := range bestCodeDeltaV6AddCandidateOffsets(baseBytes, literal, candidateOffset, previousBaseEnd, nextBaseOffset) {
		diff := buildCodeDeltaAddDiff(baseBytes[offset:offset+len(literal)], literal)
		score := compressedCodeDeltaBlockSize(diff)
		if score < bestScore {
			bestScore = score
			bestOffset = offset
			bestDiff = diff
		}
	}
	if bestOffset < 0 {
		return codeDeltaOp{}, false
	}
	return codeDeltaOp{
		Kind:       codeDeltaOpAdd,
		BaseOffset: uint64(bestOffset),
		Length:     uint64(len(bestDiff)),
		Literal:    bestDiff,
	}, true
}

func bestCodeDeltaV6AddCandidateOffsets(baseBytes []byte, literal []byte, candidateOffset int, previousBaseEnd int, nextBaseOffset int) []int {
	type scoredOffset struct {
		offset int
		score  int
	}

	anchors := []int{candidateOffset}
	if previousBaseEnd >= 0 {
		anchors = append(anchors, previousBaseEnd)
	}
	if nextBaseOffset >= len(literal) {
		anchors = append(anchors, nextBaseOffset-len(literal))
	}

	offsets := make([]int, 0, codeDeltaV6AddTopCandidates+len(anchors))
	top := make([]scoredOffset, 0, codeDeltaV6AddTopCandidates)
	seen := make(map[int]bool)
	addAnchor := func(offset int) {
		if seen[offset] || offset < 0 || offset+len(literal) > len(baseBytes) {
			return
		}
		seen[offset] = true
		offsets = append(offsets, offset)
	}
	consider := func(offset int) {
		if seen[offset] || offset < 0 || offset+len(literal) > len(baseBytes) {
			return
		}
		seen[offset] = true
		score := estimateCodeDeltaAddDiffScore(baseBytes[offset:offset+len(literal)], literal)
		if len(top) < codeDeltaV6AddTopCandidates {
			top = append(top, scoredOffset{offset: offset, score: score})
			return
		}

		worstIndex := 0
		for index := 1; index < len(top); index++ {
			if top[index].score > top[worstIndex].score {
				worstIndex = index
			}
		}
		if score < top[worstIndex].score {
			top[worstIndex] = scoredOffset{offset: offset, score: score}
		}
	}

	for _, anchor := range anchors {
		addAnchor(anchor)
	}
	for _, anchor := range anchors {
		for delta := -codeDeltaV6AddDenseRadius; delta <= codeDeltaV6AddDenseRadius; delta++ {
			consider(anchor + delta)
		}
		for delta := -codeDeltaV6AddSparseRadius; delta <= codeDeltaV6AddSparseRadius; delta += codeDeltaV6AddSparseStride {
			if absInt(delta) <= codeDeltaV6AddDenseRadius {
				continue
			}
			consider(anchor + delta)
		}
	}

	for _, candidate := range top {
		offsets = append(offsets, candidate.offset)
	}
	return offsets
}

func estimateCodeDeltaAddDiffScore(baseBytes []byte, candidateBytes []byte) int {
	nonZero := 0
	runs := 0
	inRun := false
	for index, candidateByte := range candidateBytes {
		if candidateByte == baseBytes[index] {
			inRun = false
			continue
		}
		nonZero++
		if !inRun {
			runs++
			inRun = true
		}
	}
	return nonZero*4 + runs*32
}

func buildCodeDeltaAddDiff(baseBytes []byte, candidateBytes []byte) []byte {
	diff := make([]byte, len(candidateBytes))
	for index := range candidateBytes {
		diff[index] = candidateBytes[index] - baseBytes[index]
	}
	return diff
}

func buildCodeDeltaOpsV4(baseBytes []byte, ops []codeDeltaOp) []codeDeltaOp {
	if len(ops) == 0 {
		return nil
	}

	output := make([]codeDeltaOp, 0, len(ops))
	for _, op := range ops {
		if op.Kind != codeDeltaOpAdd || int(op.BaseOffset)+int(op.Length) > len(baseBytes) {
			output = append(output, op)
			continue
		}
		sparseOp, ok := bestCodeDeltaV4SparseAddOp(op)
		if ok {
			output = append(output, sparseOp)
		} else {
			output = append(output, op)
		}
	}
	return output
}

func bestCodeDeltaV4SparseAddOp(op codeDeltaOp) (codeDeltaOp, bool) {
	if op.Length < codeDeltaV4MinSparseAddBytes || len(op.Literal) != int(op.Length) {
		return codeDeltaOp{}, false
	}

	payload := encodeCodeDeltaSparseAddPayload(op.Literal)
	if len(payload) == 0 {
		return codeDeltaOp{
			Kind:       codeDeltaOpCopy,
			BaseOffset: op.BaseOffset,
			Length:     op.Length,
		}, true
	}

	fullScore := compressedCodeDeltaBlockSize(op.Literal)
	sparseScore := compressedCodeDeltaBlockSize(payload) + 8
	if sparseScore >= fullScore {
		return codeDeltaOp{}, false
	}
	return codeDeltaOp{
		Kind:       codeDeltaOpSparseAdd,
		BaseOffset: op.BaseOffset,
		Length:     op.Length,
		Literal:    payload,
	}, true
}

func encodeCodeDeltaSparseAddPayload(diff []byte) []byte {
	var output bytes.Buffer
	for index := 0; index < len(diff); {
		for index < len(diff) && diff[index] == 0 {
			index++
		}
		if index >= len(diff) {
			break
		}
		runStart := index
		for index < len(diff) && diff[index] != 0 {
			index++
		}
		writeUvarint(&output, uint64(runStart))
		writeUvarint(&output, uint64(index-runStart))
		output.Write(diff[runStart:index])
	}
	return output.Bytes()
}

func applyCodeDeltaSparseAdd(baseBytes []byte, payload []byte) ([]byte, error) {
	output := append([]byte(nil), baseBytes...)
	reader := bytes.NewReader(payload)
	for reader.Len() > 0 {
		offset, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read sparse add offset: %w", err)
		}
		length, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("read sparse add length: %w", err)
		}
		end := offset + length
		if end > uint64(len(output)) {
			return nil, fmt.Errorf("sparse add run exceeds base payload bounds")
		}
		if length > uint64(reader.Len()) {
			return nil, fmt.Errorf("sparse add run exceeds remaining payload bytes")
		}
		for index := uint64(0); index < length; index++ {
			diff, err := reader.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("read sparse add diff byte: %w", err)
			}
			output[int(offset+index)] += diff
		}
	}
	return output, nil
}

func writeUvarint(output *bytes.Buffer, value uint64) {
	var encoded [binary.MaxVarintLen64]byte
	count := binary.PutUvarint(encoded[:], value)
	output.Write(encoded[:count])
}

func compressedCodeDeltaBlockSize(payload []byte) int {
	var output bytes.Buffer
	writer, err := flate.NewWriter(&output, flate.DefaultCompression)
	if err != nil {
		return len(payload)
	}
	if _, err := writer.Write(payload); err != nil {
		return len(payload)
	}
	if err := writer.Close(); err != nil {
		return len(payload)
	}
	return output.Len()
}

func codeDeltaV2AnchorKey(bytes []byte, offset int) [codeDeltaV2AnchorBytes]byte {
	var key [codeDeltaV2AnchorBytes]byte
	copy(key[:], bytes[offset:offset+codeDeltaV2AnchorBytes])
	return key
}

func commonPrefixLength(left []byte, right []byte) int {
	limit := minInt(len(left), len(right))
	for index := 0; index < limit; index++ {
		if left[index] != right[index] {
			return index
		}
	}
	return limit
}

func coalesceCodeDeltaOps(ops []codeDeltaOp) []codeDeltaOp {
	if len(ops) == 0 {
		return nil
	}

	merged := make([]codeDeltaOp, 0, len(ops))
	for _, op := range ops {
		if op.Length == 0 {
			continue
		}
		if len(merged) == 0 {
			merged = append(merged, op)
			continue
		}

		last := &merged[len(merged)-1]
		switch {
		case last.Kind == codeDeltaOpInsert && op.Kind == codeDeltaOpInsert:
			last.Literal = append(last.Literal, op.Literal...)
			last.Length = uint64(len(last.Literal))
		case last.Kind == codeDeltaOpAdd && op.Kind == codeDeltaOpAdd && last.BaseOffset+last.Length == op.BaseOffset:
			last.Literal = append(last.Literal, op.Literal...)
			last.Length = uint64(len(last.Literal))
		case last.Kind == codeDeltaOpCopy && op.Kind == codeDeltaOpCopy && last.BaseOffset+last.Length == op.BaseOffset:
			last.Length += op.Length
		default:
			merged = append(merged, op)
		}
	}
	return merged
}

func summarizeCodeDelta(baseBytes []byte, ops []codeDeltaOp) codeDeltaSummary {
	summary := codeDeltaSummary{
		OpCount: len(ops),
	}
	var consumedBase uint64
	for _, op := range ops {
		switch op.Kind {
		case codeDeltaOpCopy:
			summary.CopyOps++
			summary.CopiedBytes += op.Length
			if op.BaseOffset > consumedBase {
				summary.SkippedBaseBytes += op.BaseOffset - consumedBase
			}
			consumedBase = op.BaseOffset + op.Length
		case codeDeltaOpInsert:
			summary.InsertOps++
			summary.InsertedBytes += op.Length
		case codeDeltaOpAdd:
			summary.AddOps++
			summary.AddedBytes += op.Length
			if op.BaseOffset > consumedBase {
				summary.SkippedBaseBytes += op.BaseOffset - consumedBase
			}
			consumedBase = op.BaseOffset + op.Length
		case codeDeltaOpSparseAdd:
			summary.SparseAddOps++
			summary.SparseAddedBytes += op.Length
			if op.BaseOffset > consumedBase {
				summary.SkippedBaseBytes += op.BaseOffset - consumedBase
			}
			consumedBase = op.BaseOffset + op.Length
		case codeDeltaOpOutputCopy:
			summary.OutputCopyOps++
			summary.OutputCopiedBytes += op.Length
		}
	}
	if consumedBase < uint64(len(baseBytes)) {
		summary.SkippedBaseBytes += uint64(len(baseBytes)) - consumedBase
	}
	return summary
}

func minInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
