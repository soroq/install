package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/domain"
	"soroq/backend/internal/signing"
)

func TestBuildIOSRuntimeManagedDartPatchGeneratesSignedCandidateKernelBundle(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	kernelPath := writeTestIOSKernelBlob(t, tempDir, []byte("candidate-ios-kernel-v1"))
	seedBase64 := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32))

	report, bundleBytes, err := buildIOSRuntimeManagedDartPatchBundle(iosRuntimeManagedDartPatchBuildOptions{
		KernelBlobPath: kernelPath,
		PatchID:        "ios-runtime-dart-1",
		PatchNumber:    12,
		RuntimeID:      "ios-runtime-1",
		ReleaseID:      "ios-release-1",
		Channel:        "stable",
		ArtifactURL:    "https://cdn.example.com/ios-runtime-dart-1/artifact.bin",
		OutputPath:     filepath.Join(tempDir, "ios-runtime-dart.zip"),
		SeedBase64:     seedBase64,
		KeyID:          "dev-ios-runtime-dart",
	})
	if err != nil {
		t.Fatalf("buildIOSRuntimeManagedDartPatchBundle() error = %v", err)
	}
	if report == nil || !report.Ready {
		t.Fatalf("expected ready report, got %#v", report)
	}
	if !report.ManifestSigned {
		t.Fatalf("expected manifest signing to be enabled: %#v", report)
	}
	if report.Platform != "ios" {
		t.Fatalf("expected ios platform, got %q", report.Platform)
	}
	if report.Kind != domain.PatchKindRuntimeManagedDart {
		t.Fatalf("expected runtime_managed_dart report kind, got %q", report.Kind)
	}
	if report.KernelBlobPath != filepath.Clean(kernelPath) {
		t.Fatalf("expected report to bind candidate kernel path, got %q", report.KernelBlobPath)
	}
	if report.KernelBlobSHA256 != sha256Hex([]byte("candidate-ios-kernel-v1")) {
		t.Fatalf("unexpected candidate kernel sha: %#v", report)
	}
	if !report.NativeAudit.Checked {
		t.Fatalf("expected native payload audit in report: %#v", report.NativeAudit)
	}

	manifest, artifactBytes, overlayFiles := parseBuiltPatchBundle(t, bundleBytes)
	if len(overlayFiles) != 0 {
		t.Fatalf("expected no overlay files, got %#v", overlayFiles)
	}
	if manifest.Kind != domain.PatchKindRuntimeManagedDart {
		t.Fatalf("expected runtime_managed_dart manifest kind, got %q", manifest.Kind)
	}
	if manifest.ActivationMode != domain.ActivationNextColdStart {
		t.Fatalf("expected next_cold_start activation, got %q", manifest.ActivationMode)
	}
	if manifest.Signature == nil || strings.TrimSpace(*manifest.Signature) == "" {
		t.Fatalf("expected signed manifest: %#v", manifest)
	}
	if manifest.SignatureKeyID == nil || *manifest.SignatureKeyID != "dev-ios-runtime-dart" {
		t.Fatalf("unexpected signature key id: %#v", manifest.SignatureKeyID)
	}
	if err := signing.VerifyManifestSignature(manifest, report.ManifestPublicKeyBase64); err != nil {
		t.Fatalf("VerifyManifestSignature() error = %v", err)
	}
	if manifest.Artifact.SHA256 != sha256Hex(artifactBytes) {
		t.Fatalf("expected manifest artifact sha to match artifact.bin")
	}
	if manifest.Artifact.SizeBytes != uint64(len(artifactBytes)) {
		t.Fatalf("expected manifest artifact size to match artifact.bin")
	}
	if report.ArtifactSHA256 != manifest.Artifact.SHA256 {
		t.Fatalf("expected report artifact sha to match manifest")
	}
	if report.BundleSHA256 != sha256Hex(bundleBytes) {
		t.Fatalf("expected report bundle sha to match output bytes")
	}

	metadata, payloads := parseRuntimeManagedDartArtifact(t, artifactBytes)
	if metadata.ArtifactType != iosRuntimeManagedDartArtifactType {
		t.Fatalf("unexpected artifact type: %#v", metadata)
	}
	if metadata.SchemaVersion != 1 {
		t.Fatalf("unexpected schema version: %#v", metadata)
	}
	if metadata.Format != iosRuntimeManagedDartFormat {
		t.Fatalf("unexpected artifact format: %#v", metadata)
	}
	if metadata.EntrypointPath != iosRuntimeManagedDartEntrypoint {
		t.Fatalf("unexpected entrypoint: %#v", metadata)
	}
	if len(metadata.Files) != 1 {
		t.Fatalf("expected one payload metadata entry, got %#v", metadata.Files)
	}
	file := metadata.Files[0]
	if file.Path != iosRuntimeManagedDartEntrypoint {
		t.Fatalf("unexpected payload path: %#v", file)
	}
	if got := string(payloads[iosRuntimeManagedDartEntrypoint]); got != "candidate-ios-kernel-v1" {
		t.Fatalf("unexpected runtime-managed Dart payload: %q", got)
	}
	if file.SHA256 != sha256Hex(payloads[iosRuntimeManagedDartEntrypoint]) {
		t.Fatalf("expected payload sha metadata to match candidate kernel")
	}
	if file.SizeBytes != uint64(len(payloads[iosRuntimeManagedDartEntrypoint])) {
		t.Fatalf("expected payload size metadata to match candidate kernel")
	}
}

func TestRunBuildIOSRuntimeManagedDartPatchWritesBundleAndReport(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	kernelPath := writeTestIOSKernelBlob(t, tempDir, []byte("candidate-ios-kernel-cli"))
	outPath := filepath.Join(tempDir, "patch.zip")
	reportPath := filepath.Join(tempDir, "report.json")

	if err := runBuildIOSRuntimeManagedDartPatch([]string{
		"--kernel-blob", kernelPath,
		"--patch-id", "ios-runtime-dart-cli",
		"--patch-number", "3",
		"--runtime-id", "ios-runtime-cli",
		"--release-id", "ios-release-cli",
		"--out", outPath,
		"--report-out", reportPath,
		"--seed-base64", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{21}, 32)),
		"--key-id", "dev-ios-cli",
	}); err != nil {
		t.Fatalf("runBuildIOSRuntimeManagedDartPatch() error = %v", err)
	}

	bundleBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(outPath) error = %v", err)
	}
	manifest, artifactBytes, _ := parseBuiltPatchBundle(t, bundleBytes)
	if manifest.Kind != domain.PatchKindRuntimeManagedDart {
		t.Fatalf("expected runtime_managed_dart manifest kind, got %q", manifest.Kind)
	}
	metadata, payloads := parseRuntimeManagedDartArtifact(t, artifactBytes)
	if metadata.EntrypointPath != iosRuntimeManagedDartEntrypoint {
		t.Fatalf("unexpected artifact metadata: %#v", metadata)
	}
	if string(payloads[iosRuntimeManagedDartEntrypoint]) != "candidate-ios-kernel-cli" {
		t.Fatalf("unexpected artifact payload")
	}

	var report iosRuntimeManagedDartPatchBuildReport
	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("ReadFile(reportPath) error = %v", err)
	}
	if err := json.Unmarshal(reportBytes, &report); err != nil {
		t.Fatalf("Unmarshal(report) error = %v", err)
	}
	if report.OutputPath != filepath.Clean(outPath) {
		t.Fatalf("expected report output path %q, got %q", filepath.Clean(outPath), report.OutputPath)
	}
	if report.ManifestSignatureKeyID != "dev-ios-cli" {
		t.Fatalf("unexpected report key id: %#v", report)
	}
}

func TestBuildIOSRuntimeManagedDartPatchCanGenerateBaseRelativeDelta(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseBytes := bytes.Repeat([]byte("base-ios-kernel-block-"), 512)
	candidateBytes := append([]byte{}, baseBytes...)
	copy(candidateBytes[4096:], []byte("candidate-runtime-managed-dart-change"))
	candidateBytes = append(candidateBytes, []byte("-candidate-tail")...)
	baseKernelPath := writeTestIOSKernelBlobForApp(t, tempDir, "base_app", baseBytes)
	candidateKernelPath := writeTestIOSKernelBlobForApp(t, tempDir, "candidate_app", candidateBytes)

	report, bundleBytes, err := buildIOSRuntimeManagedDartPatchBundle(iosRuntimeManagedDartPatchBuildOptions{
		KernelBlobPath:     candidateKernelPath,
		BaseKernelBlobPath: baseKernelPath,
		PatchID:            "ios-runtime-dart-delta",
		PatchNumber:        13,
		RuntimeID:          "ios-runtime-1",
		ReleaseID:          "ios-release-1",
		Channel:            "stable",
		ArtifactURL:        "https://cdn.example.com/ios-runtime-dart-delta/artifact.bin",
		OutputPath:         filepath.Join(tempDir, "ios-runtime-dart-delta.zip"),
		SeedBase64:         base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{23}, 32)),
		KeyID:              "dev-ios-runtime-dart",
	})
	if err != nil {
		t.Fatalf("buildIOSRuntimeManagedDartPatchBundle() error = %v", err)
	}
	if report.TransportMode != "base_xor_zlib_delta" {
		t.Fatalf("expected delta transport, got %#v", report)
	}
	if report.BaseKernelBlobSHA256 != sha256Hex(baseBytes) {
		t.Fatalf("expected report base sha to match base kernel")
	}
	if report.KernelBlobSHA256 != sha256Hex(candidateBytes) {
		t.Fatalf("expected report candidate sha to match candidate kernel")
	}
	if report.DeltaPath != iosRuntimeManagedDartDeltaPath || report.DeltaSizeBytes == 0 {
		t.Fatalf("expected delta report fields, got %#v", report)
	}

	_, artifactBytes, _ := parseBuiltPatchBundle(t, bundleBytes)
	metadata, payloads, deltas := parseRuntimeManagedDartArtifactWithDeltas(t, artifactBytes)
	if metadata.Format != iosRuntimeManagedDartDeltaFormat {
		t.Fatalf("expected delta artifact format, got %#v", metadata)
	}
	if metadata.Base == nil || metadata.Base.SHA256 != sha256Hex(baseBytes) {
		t.Fatalf("expected base metadata to bind base kernel, got %#v", metadata.Base)
	}
	if metadata.Delta == nil || metadata.Delta.Path != iosRuntimeManagedDartDeltaPath {
		t.Fatalf("expected delta metadata, got %#v", metadata.Delta)
	}
	if len(payloads) != 0 {
		t.Fatalf("delta artifact must not ship full payload/app.dill, got payloads %#v", payloads)
	}
	deltaBytes := deltas[iosRuntimeManagedDartDeltaPath]
	if len(deltaBytes) == 0 {
		t.Fatalf("expected compressed delta bytes")
	}
	if metadata.Delta.SHA256 != sha256Hex(deltaBytes) {
		t.Fatalf("expected delta sha metadata to match compressed delta")
	}
	if metadata.Files[0].SHA256 != sha256Hex(candidateBytes) {
		t.Fatalf("expected target file metadata to bind candidate kernel")
	}
}

func TestBuildIOSRuntimeManagedDartPatchRejectsUnsignedManifest(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	kernelPath := writeTestIOSKernelBlob(t, tempDir, []byte("candidate-ios-kernel-v1"))
	_, _, err := buildIOSRuntimeManagedDartPatchBundle(iosRuntimeManagedDartPatchBuildOptions{
		KernelBlobPath: kernelPath,
		PatchID:        "ios-runtime-dart-unsigned",
		PatchNumber:    1,
		RuntimeID:      "ios-runtime-1",
		ReleaseID:      "ios-release-1",
		OutputPath:     filepath.Join(tempDir, "patch.zip"),
	})
	if err == nil || !strings.Contains(err.Error(), "--seed-base64 is required") {
		t.Fatalf("expected mandatory signing error, got %v", err)
	}
}

func TestBuildIOSRuntimeManagedDartPatchRejectsNonIOSKernelPath(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	kernelPath := filepath.Join(tempDir, "flutter_assets", "kernel_blob.bin")
	if err := os.MkdirAll(filepath.Dir(kernelPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(kernel parent) error = %v", err)
	}
	if err := os.WriteFile(kernelPath, []byte("candidate"), 0o644); err != nil {
		t.Fatalf("WriteFile(kernel) error = %v", err)
	}
	_, _, err := buildIOSRuntimeManagedDartPatchBundle(validIOSRuntimeManagedDartOptions(t, tempDir, kernelPath))
	if err == nil || !strings.Contains(err.Error(), "App.framework/flutter_assets/kernel_blob.bin") {
		t.Fatalf("expected iOS kernel path error, got %v", err)
	}
}

func TestBuildIOSRuntimeManagedDartPatchRejectsNativeLookingPayloadPaths(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	kernelPath := writeTestIOSKernelBlob(t, tempDir, []byte("candidate-ios-kernel-v1"))
	for _, path := range []string{
		"payload/libPatch.dylib",
		"payload/Foo.framework/App",
		"payload/libfoo.so",
		"payload/libfoo.a",
		"payload/Assets.bundle/config",
		"payload/Extension.appex/App",
		"payload/code.mach-o",
		"payload/kernel.vmcode",
	} {
		t.Run(path, func(t *testing.T) {
			options := validIOSRuntimeManagedDartOptions(t, tempDir, kernelPath)
			options.EntrypointPath = path
			_, _, err := buildIOSRuntimeManagedDartPatchBundle(options)
			if err == nil || !strings.Contains(err.Error(), "native executable content") {
				t.Fatalf("expected native-looking path rejection for %q, got %v", path, err)
			}
		})
	}
}

func TestBuildIOSRuntimeManagedDartPatchRejectsMachOPayloadBytes(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	kernelPath := writeTestIOSKernelBlob(t, tempDir, []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 1, 2, 3})
	_, _, err := buildIOSRuntimeManagedDartPatchBundle(validIOSRuntimeManagedDartOptions(t, tempDir, kernelPath))
	if err == nil || !strings.Contains(err.Error(), "Mach-O/native executable") {
		t.Fatalf("expected Mach-O payload rejection, got %v", err)
	}
}

func validIOSRuntimeManagedDartOptions(
	t *testing.T,
	tempDir string,
	kernelPath string,
) iosRuntimeManagedDartPatchBuildOptions {
	t.Helper()
	return iosRuntimeManagedDartPatchBuildOptions{
		KernelBlobPath: kernelPath,
		EntrypointPath: iosRuntimeManagedDartEntrypoint,
		PatchID:        "ios-runtime-dart-valid",
		PatchNumber:    9,
		RuntimeID:      "ios-runtime-valid",
		ReleaseID:      "ios-release-valid",
		OutputPath:     filepath.Join(tempDir, "valid.zip"),
		SeedBase64:     base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{19}, 32)),
		KeyID:          "dev-ios-valid",
	}
}

func writeTestIOSKernelBlob(t *testing.T, root string, bytes []byte) string {
	t.Helper()
	return writeTestIOSKernelBlobForApp(t, root, "candidate_app", bytes)
}

func writeTestIOSKernelBlobForApp(t *testing.T, root string, appDirName string, bytes []byte) string {
	t.Helper()
	kernelPath := filepath.Join(
		root,
		appDirName,
		"build",
		"ios",
		"Debug-iphonesimulator",
		"App.framework",
		"flutter_assets",
		"kernel_blob.bin",
	)
	if err := os.MkdirAll(filepath.Dir(kernelPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(kernel parent) error = %v", err)
	}
	if err := os.WriteFile(kernelPath, bytes, 0o644); err != nil {
		t.Fatalf("WriteFile(kernel) error = %v", err)
	}
	return kernelPath
}

func parseRuntimeManagedDartArtifact(
	t *testing.T,
	artifactBytes []byte,
) (iosRuntimeDartMetadata, map[string][]byte) {
	t.Helper()
	metadata, payloads, _ := parseRuntimeManagedDartArtifactWithDeltas(t, artifactBytes)
	return metadata, payloads
}

func parseRuntimeManagedDartArtifactWithDeltas(
	t *testing.T,
	artifactBytes []byte,
) (iosRuntimeDartMetadata, map[string][]byte, map[string][]byte) {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(artifactBytes), int64(len(artifactBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader(artifact) error = %v", err)
	}

	var metadata iosRuntimeDartMetadata
	payloads := map[string][]byte{}
	deltas := map[string][]byte{}
	for _, file := range reader.File {
		bytes, err := readZipFileBytes(file)
		if err != nil {
			t.Fatalf("readZipFileBytes(%q) error = %v", file.Name, err)
		}
		switch {
		case file.Name == "metadata.json":
			if err := json.Unmarshal(bytes, &metadata); err != nil {
				t.Fatalf("Unmarshal(runtime metadata) error = %v", err)
			}
		case strings.HasPrefix(file.Name, "payload/"):
			payloads[file.Name] = bytes
		case strings.HasPrefix(file.Name, "delta/"):
			deltas[file.Name] = bytes
		default:
			t.Fatalf("unexpected runtime-managed Dart artifact entry %q", file.Name)
		}
	}
	return metadata, payloads, deltas
}
