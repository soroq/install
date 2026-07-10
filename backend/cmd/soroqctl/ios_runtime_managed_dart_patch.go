package main

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"soroq/backend/internal/domain"
	"soroq/backend/internal/signing"
)

const (
	iosRuntimeManagedDartArtifactType  = "runtime_managed_dart"
	iosRuntimeManagedDartFormat        = "flutter_dill_bundle_v1"
	iosRuntimeManagedDartDeltaFormat   = "flutter_dill_xor_delta_v1"
	iosRuntimeManagedDartEntrypoint    = "payload/app.dill"
	iosRuntimeManagedDartDeltaPath     = "delta/app.dill.xor.zlib"
	iosRuntimeManagedDartDeltaAlgo     = "xor_full_v1"
	iosRuntimeManagedDartDeltaCompress = "zlib"
)

type iosRuntimeManagedDartPatchBuildOptions struct {
	KernelBlobPath     string
	BaseKernelBlobPath string
	EntrypointPath     string
	PatchID            string
	PatchNumber        uint32
	RuntimeID          string
	ReleaseID          string
	Channel            string
	ArtifactURL        string
	OutputPath         string
	ReportOutPath      string
	SeedBase64         string
	KeyID              string
}

type iosRuntimeManagedDartPatchBuildReport struct {
	SchemaVersion           int                    `json:"schema_version"`
	Ready                   bool                   `json:"ready"`
	GeneratedAt             string                 `json:"generated_at"`
	Platform                string                 `json:"platform"`
	PatchID                 string                 `json:"patch_id"`
	PatchNumber             uint32                 `json:"patch_number"`
	RuntimeID               string                 `json:"runtime_id"`
	ReleaseID               string                 `json:"release_id"`
	Channel                 string                 `json:"channel"`
	Kind                    domain.PatchKind       `json:"kind"`
	ActivationMode          domain.ActivationMode  `json:"activation_mode"`
	OutputPath              string                 `json:"output_path"`
	KernelBlobPath          string                 `json:"kernel_blob_path"`
	KernelBlobSHA256        string                 `json:"kernel_blob_sha256"`
	KernelBlobSizeBytes     uint64                 `json:"kernel_blob_size_bytes"`
	BaseKernelBlobPath      string                 `json:"base_kernel_blob_path,omitempty"`
	BaseKernelBlobSHA256    string                 `json:"base_kernel_blob_sha256,omitempty"`
	BaseKernelBlobSizeBytes uint64                 `json:"base_kernel_blob_size_bytes,omitempty"`
	TransportMode           string                 `json:"transport_mode"`
	DeltaPath               string                 `json:"delta_path,omitempty"`
	DeltaSHA256             string                 `json:"delta_sha256,omitempty"`
	DeltaSizeBytes          uint64                 `json:"delta_size_bytes,omitempty"`
	DeltaUncompressedSHA256 string                 `json:"delta_uncompressed_sha256,omitempty"`
	DeltaUncompressedBytes  uint64                 `json:"delta_uncompressed_size_bytes,omitempty"`
	EntrypointPath          string                 `json:"entrypoint_path"`
	ArtifactURL             string                 `json:"artifact_url"`
	ArtifactSHA256          string                 `json:"artifact_sha256"`
	ArtifactSizeBytes       uint64                 `json:"artifact_size_bytes"`
	BundleSHA256            string                 `json:"bundle_sha256"`
	BundleSizeBytes         uint64                 `json:"bundle_size_bytes"`
	ManifestSigned          bool                   `json:"manifest_signed"`
	ManifestSignatureKeyID  string                 `json:"manifest_signature_key_id"`
	ManifestPublicKeyBase64 string                 `json:"manifest_public_key_base64"`
	NativeAudit             iosNativePayloadAudit  `json:"native_audit"`
	ArtifactMetadata        iosRuntimeDartMetadata `json:"artifact_metadata"`
}

type iosNativePayloadAudit struct {
	Checked             bool     `json:"checked"`
	RejectedExtensions  []string `json:"rejected_extensions"`
	RejectedMagicPrefix []string `json:"rejected_magic_prefix"`
}

type iosRuntimeDartMetadata struct {
	ArtifactType   string                       `json:"artifact_type"`
	SchemaVersion  int                          `json:"schema_version"`
	GeneratedAt    string                       `json:"generated_at"`
	Format         string                       `json:"format"`
	EntrypointPath string                       `json:"entrypoint_path"`
	Files          []iosRuntimeDartMetadataFile `json:"files"`
	Base           *iosRuntimeDartBase          `json:"base,omitempty"`
	Delta          *iosRuntimeDartDelta         `json:"delta,omitempty"`
}

type iosRuntimeDartMetadataFile struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes uint64 `json:"size_bytes"`
}

type iosRuntimeDartBase struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes uint64 `json:"size_bytes"`
}

type iosRuntimeDartDelta struct {
	Path                  string `json:"path"`
	Algorithm             string `json:"algorithm"`
	Compression           string `json:"compression"`
	SHA256                string `json:"sha256"`
	SizeBytes             uint64 `json:"size_bytes"`
	UncompressedSHA256    string `json:"uncompressed_sha256"`
	UncompressedSizeBytes uint64 `json:"uncompressed_size_bytes"`
}

func runBuildIOSRuntimeManagedDartPatch(args []string) error {
	fs := flag.NewFlagSet("build-ios-runtime-managed-dart-patch", flag.ContinueOnError)
	kernelBlobPath := fs.String("kernel-blob", "", "candidate iOS build App.framework/flutter_assets/kernel_blob.bin")
	baseKernelBlobPath := fs.String("base-kernel-blob", "", "optional base iOS build App.framework/flutter_assets/kernel_blob.bin for data-only delta transport")
	patchID := fs.String("patch-id", "", "patch id")
	patchNumber := fs.Uint("patch-number", 0, "patch number")
	runtimeID := fs.String("runtime-id", "", "iOS runtime compatibility id")
	releaseID := fs.String("release-id", "", "release id")
	channel := fs.String("channel", "stable", "channel")
	artifactURL := fs.String("artifact-url", "", "artifact url recorded in manifest")
	outputPath := fs.String("out", "", "path to write the signed runtime-managed Dart patch bundle zip")
	reportOutPath := fs.String("report-out", "", "optional path for the build report json")
	seedBase64 := fs.String("seed-base64", "", "required manifest signing private seed in base64url format")
	keyID := fs.String("key-id", "", "optional manifest signing key id override")
	entrypointPath := fs.String("entrypoint-path", iosRuntimeManagedDartEntrypoint, "runtime-managed Dart payload entrypoint; current iOS backend requires payload/app.dill")
	if err := fs.Parse(args); err != nil {
		return err
	}

	report, bundleBytes, err := buildIOSRuntimeManagedDartPatchBundle(iosRuntimeManagedDartPatchBuildOptions{
		KernelBlobPath:     *kernelBlobPath,
		BaseKernelBlobPath: *baseKernelBlobPath,
		EntrypointPath:     *entrypointPath,
		PatchID:            *patchID,
		PatchNumber:        uint32(*patchNumber),
		RuntimeID:          *runtimeID,
		ReleaseID:          *releaseID,
		Channel:            *channel,
		ArtifactURL:        *artifactURL,
		OutputPath:         *outputPath,
		ReportOutPath:      *reportOutPath,
		SeedBase64:         *seedBase64,
		KeyID:              *keyID,
	})
	if err != nil {
		return err
	}

	outputPathClean := filepath.Clean(*outputPath)
	if err := os.MkdirAll(filepath.Dir(outputPathClean), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(outputPathClean, bundleBytes, 0o644); err != nil {
		return fmt.Errorf("write iOS runtime-managed Dart patch bundle: %w", err)
	}

	if strings.TrimSpace(*reportOutPath) != "" {
		return writeJSONOutput(report, *reportOutPath)
	}
	return writeJSONOutput(report, "")
}

func buildIOSRuntimeManagedDartPatchBundle(
	options iosRuntimeManagedDartPatchBuildOptions,
) (*iosRuntimeManagedDartPatchBuildReport, []byte, error) {
	if err := validateIOSRuntimeManagedDartBuildOptions(options); err != nil {
		return nil, nil, err
	}

	kernelBlobPath := filepath.Clean(options.KernelBlobPath)
	kernelBytes, err := os.ReadFile(kernelBlobPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read candidate iOS kernel_blob.bin: %w", err)
	}
	if err := rejectNativeLookingPayloadBytes(kernelBytes); err != nil {
		return nil, nil, err
	}

	generatedAt := time.Now().UTC().Format(time.RFC3339)
	entrypointPath := normalizedDefaultString(options.EntrypointPath, iosRuntimeManagedDartEntrypoint)
	kernelSHA := sha256Hex(kernelBytes)
	kernelSize := uint64(len(kernelBytes))
	format := iosRuntimeManagedDartFormat
	transportMode := "full_kernel"
	artifactEntries := map[string][]byte{
		entrypointPath: kernelBytes,
	}
	var baseKernelPath string
	var baseKernelSHA string
	var baseKernelSize uint64
	var deltaPath string
	var deltaSHA string
	var deltaSize uint64
	var deltaUncompressedSHA string
	var deltaUncompressedSize uint64
	var metadataBase *iosRuntimeDartBase
	var metadataDelta *iosRuntimeDartDelta

	if strings.TrimSpace(options.BaseKernelBlobPath) != "" {
		baseKernelPath = filepath.Clean(options.BaseKernelBlobPath)
		baseKernelBytes, err := os.ReadFile(baseKernelPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read base iOS kernel_blob.bin: %w", err)
		}
		if err := rejectNativeLookingPayloadBytes(baseKernelBytes); err != nil {
			return nil, nil, err
		}
		residual := buildIOSRuntimeManagedDartXORResidual(baseKernelBytes, kernelBytes)
		compressedResidual, err := zlibCompressBest(residual)
		if err != nil {
			return nil, nil, err
		}
		baseKernelSHA = sha256Hex(baseKernelBytes)
		baseKernelSize = uint64(len(baseKernelBytes))
		deltaPath = iosRuntimeManagedDartDeltaPath
		deltaSHA = sha256Hex(compressedResidual)
		deltaSize = uint64(len(compressedResidual))
		deltaUncompressedSHA = sha256Hex(residual)
		deltaUncompressedSize = uint64(len(residual))
		format = iosRuntimeManagedDartDeltaFormat
		transportMode = "base_xor_zlib_delta"
		artifactEntries = map[string][]byte{
			iosRuntimeManagedDartDeltaPath: compressedResidual,
		}
		metadataBase = &iosRuntimeDartBase{
			Path:      entrypointPath,
			SHA256:    baseKernelSHA,
			SizeBytes: baseKernelSize,
		}
		metadataDelta = &iosRuntimeDartDelta{
			Path:                  iosRuntimeManagedDartDeltaPath,
			Algorithm:             iosRuntimeManagedDartDeltaAlgo,
			Compression:           iosRuntimeManagedDartDeltaCompress,
			SHA256:                deltaSHA,
			SizeBytes:             deltaSize,
			UncompressedSHA256:    deltaUncompressedSHA,
			UncompressedSizeBytes: deltaUncompressedSize,
		}
	}

	metadata := iosRuntimeDartMetadata{
		ArtifactType:   iosRuntimeManagedDartArtifactType,
		SchemaVersion:  1,
		GeneratedAt:    generatedAt,
		Format:         format,
		EntrypointPath: entrypointPath,
		Files: []iosRuntimeDartMetadataFile{{
			Path:      entrypointPath,
			SHA256:    kernelSHA,
			SizeBytes: kernelSize,
		}},
		Base:  metadataBase,
		Delta: metadataDelta,
	}

	artifactBytes, err := buildIOSRuntimeManagedDartArtifact(metadata, artifactEntries)
	if err != nil {
		return nil, nil, err
	}
	artifactSHA := sha256Hex(artifactBytes)
	artifactSize := uint64(len(artifactBytes))

	artifactURL := strings.TrimSpace(options.ArtifactURL)
	if artifactURL == "" {
		artifactURL = "file://soroq/ios/" + strings.TrimSpace(options.PatchID) + "/runtime-managed-dart-artifact.bin"
	}

	manifest := domain.PatchManifest{
		PatchID:        strings.TrimSpace(options.PatchID),
		PatchNumber:    int(options.PatchNumber),
		RuntimeID:      strings.TrimSpace(options.RuntimeID),
		ReleaseID:      strings.TrimSpace(options.ReleaseID),
		Channel:        normalizedDefaultString(options.Channel, "stable"),
		Kind:           domain.PatchKindRuntimeManagedDart,
		ActivationMode: domain.ActivationNextColdStart,
		Artifact: domain.PatchArtifact{
			URL:       artifactURL,
			SHA256:    artifactSHA,
			SizeBytes: artifactSize,
		},
		Signature: nil,
	}

	signer, err := signing.NewManifestSignerFromSeedBase64(options.SeedBase64, options.KeyID)
	if err != nil {
		return nil, nil, err
	}
	signature, err := signer.SignManifest(manifest)
	if err != nil {
		return nil, nil, err
	}
	signatureKeyID := signer.KeyID()
	manifest.SignatureKeyID = &signatureKeyID
	manifest.Signature = &signature

	bundleBytes, err := buildPatchBundleArchive(manifest, artifactBytes, nil)
	if err != nil {
		return nil, nil, err
	}

	report := &iosRuntimeManagedDartPatchBuildReport{
		SchemaVersion:           1,
		Ready:                   true,
		GeneratedAt:             generatedAt,
		Platform:                "ios",
		PatchID:                 manifest.PatchID,
		PatchNumber:             options.PatchNumber,
		RuntimeID:               manifest.RuntimeID,
		ReleaseID:               manifest.ReleaseID,
		Channel:                 manifest.Channel,
		Kind:                    manifest.Kind,
		ActivationMode:          manifest.ActivationMode,
		OutputPath:              filepath.Clean(options.OutputPath),
		KernelBlobPath:          kernelBlobPath,
		KernelBlobSHA256:        kernelSHA,
		KernelBlobSizeBytes:     kernelSize,
		BaseKernelBlobPath:      baseKernelPath,
		BaseKernelBlobSHA256:    baseKernelSHA,
		BaseKernelBlobSizeBytes: baseKernelSize,
		TransportMode:           transportMode,
		DeltaPath:               deltaPath,
		DeltaSHA256:             deltaSHA,
		DeltaSizeBytes:          deltaSize,
		DeltaUncompressedSHA256: deltaUncompressedSHA,
		DeltaUncompressedBytes:  deltaUncompressedSize,
		EntrypointPath:          entrypointPath,
		ArtifactURL:             artifactURL,
		ArtifactSHA256:          artifactSHA,
		ArtifactSizeBytes:       artifactSize,
		BundleSHA256:            sha256Hex(bundleBytes),
		BundleSizeBytes:         uint64(len(bundleBytes)),
		ManifestSigned:          true,
		ManifestSignatureKeyID:  signatureKeyID,
		ManifestPublicKeyBase64: signer.PublicKeyBase64(),
		NativeAudit: iosNativePayloadAudit{
			Checked: true,
			RejectedExtensions: []string{
				".dylib",
				".framework",
				".so",
				".a",
				".bundle",
				".appex",
				".mach-o",
				".vmcode",
			},
			RejectedMagicPrefix: []string{
				"mach-o",
				"fat_mach-o",
			},
		},
		ArtifactMetadata: metadata,
	}
	return report, bundleBytes, nil
}

func validateIOSRuntimeManagedDartBuildOptions(options iosRuntimeManagedDartPatchBuildOptions) error {
	required := []struct {
		flag  string
		value string
	}{
		{flag: "--kernel-blob", value: options.KernelBlobPath},
		{flag: "--patch-id", value: options.PatchID},
		{flag: "--runtime-id", value: options.RuntimeID},
		{flag: "--release-id", value: options.ReleaseID},
		{flag: "--out", value: options.OutputPath},
		{flag: "--seed-base64", value: options.SeedBase64},
	}
	for _, item := range required {
		if strings.TrimSpace(item.value) == "" {
			return fmt.Errorf("%s is required", item.flag)
		}
	}
	if options.PatchNumber == 0 {
		return errors.New("--patch-number is required")
	}
	if strings.TrimSpace(options.KeyID) != "" && strings.TrimSpace(options.SeedBase64) == "" {
		return errors.New("--key-id requires --seed-base64")
	}
	if err := validateIOSKernelBlobPath(options.KernelBlobPath); err != nil {
		return err
	}
	if strings.TrimSpace(options.BaseKernelBlobPath) != "" {
		if err := validateIOSKernelBlobPath(options.BaseKernelBlobPath); err != nil {
			return fmt.Errorf("--base-kernel-blob: %w", err)
		}
	}
	entrypointPath := strings.TrimSpace(options.EntrypointPath)
	if entrypointPath == "" {
		entrypointPath = iosRuntimeManagedDartEntrypoint
	}
	if err := validateIOSRuntimeManagedDartPayloadPath(entrypointPath); err != nil {
		return err
	}
	if entrypointPath != iosRuntimeManagedDartEntrypoint {
		return fmt.Errorf("current iOS runtime-managed Dart entrypoint must be %s, got %q", iosRuntimeManagedDartEntrypoint, entrypointPath)
	}
	return nil
}

func validateIOSKernelBlobPath(raw string) error {
	clean := filepath.Clean(strings.TrimSpace(raw))
	normalized := filepath.ToSlash(clean)
	if !strings.HasSuffix(normalized, "/App.framework/flutter_assets/kernel_blob.bin") {
		return fmt.Errorf("candidate iOS kernel blob must come from App.framework/flutter_assets/kernel_blob.bin, got %q", raw)
	}
	return nil
}

func validateIOSRuntimeManagedDartPayloadPath(raw string) error {
	path := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if path == "" {
		return errors.New("runtime-managed Dart payload path is required")
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, ":") {
		return fmt.Errorf("runtime-managed Dart payload path is unsafe: %q", raw)
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] != "payload" {
		return fmt.Errorf("runtime-managed Dart payload path must be under payload/: %q", raw)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("runtime-managed Dart payload path is unsafe: %q", raw)
		}
		if isNativeLookingPathComponent(part) {
			return fmt.Errorf("runtime-managed Dart payload path looks like native executable content: %q", raw)
		}
	}
	return nil
}

func isNativeLookingPathComponent(component string) bool {
	lower := strings.ToLower(component)
	for _, suffix := range []string{
		".dylib",
		".framework",
		".so",
		".a",
		".bundle",
		".appex",
		".mach-o",
		".vmcode",
	} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func rejectNativeLookingPayloadBytes(payload []byte) error {
	if len(payload) < 4 {
		return nil
	}
	if bytes.Equal(payload[:4], []byte{0xfe, 0xed, 0xfa, 0xce}) ||
		bytes.Equal(payload[:4], []byte{0xce, 0xfa, 0xed, 0xfe}) ||
		bytes.Equal(payload[:4], []byte{0xfe, 0xed, 0xfa, 0xcf}) ||
		bytes.Equal(payload[:4], []byte{0xcf, 0xfa, 0xed, 0xfe}) ||
		bytes.Equal(payload[:4], []byte{0xca, 0xfe, 0xba, 0xbe}) ||
		bytes.Equal(payload[:4], []byte{0xbe, 0xba, 0xfe, 0xca}) {
		return errors.New("candidate payload looks like a Mach-O/native executable, not a Dart kernel blob")
	}
	return nil
}

func buildIOSRuntimeManagedDartArtifact(
	metadata iosRuntimeDartMetadata,
	entries map[string][]byte,
) ([]byte, error) {
	metadataBytes, err := writeJSONToBytes(metadata)
	if err != nil {
		return nil, fmt.Errorf("encode runtime-managed Dart artifact metadata: %w", err)
	}

	var output bytes.Buffer
	writer := newBestCompressionZipWriter(&output)
	writeEntry := func(path string, bytes []byte) error {
		file, err := writer.Create(path)
		if err != nil {
			_ = writer.Close()
			return fmt.Errorf("create runtime-managed Dart artifact entry %s: %w", path, err)
		}
		if _, err := file.Write(bytes); err != nil {
			_ = writer.Close()
			return fmt.Errorf("write runtime-managed Dart artifact entry %s: %w", path, err)
		}
		return nil
	}
	if err := writeEntry("metadata.json", metadataBytes); err != nil {
		return nil, err
	}
	for _, path := range sortedMapKeys(entries) {
		if err := writeEntry(path, entries[path]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finalize runtime-managed Dart artifact: %w", err)
	}
	return output.Bytes(), nil
}

func buildIOSRuntimeManagedDartXORResidual(baseBytes, candidateBytes []byte) []byte {
	residual := make([]byte, len(candidateBytes))
	for index, candidateByte := range candidateBytes {
		var baseByte byte
		if index < len(baseBytes) {
			baseByte = baseBytes[index]
		}
		residual[index] = baseByte ^ candidateByte
	}
	return residual
}

func zlibCompressBest(input []byte) ([]byte, error) {
	var output bytes.Buffer
	writer, err := zlib.NewWriterLevel(&output, zlib.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("create zlib compressor: %w", err)
	}
	if _, err := writer.Write(input); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("write zlib body: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finalize zlib body: %w", err)
	}
	return output.Bytes(), nil
}

func writeJSONToBytes(value any) ([]byte, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
