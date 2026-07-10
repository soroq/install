package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	androidpatch "soroq/backend/internal/androidpatch"
	"soroq/backend/internal/domain"
)

type androidAssetPatchBuildOptions = androidpatch.AssetPatchBuildOptions
type androidAssetDiffEntry = androidpatch.AssetDiffEntry
type androidAssetPatchBundleReport = androidpatch.AssetPatchBundleReport

type androidArtifactFile struct {
	Path      string
	Bytes     []byte
	SHA256    string
	SizeBytes uint64
}

func runBuildAndroidAssetPatch(args []string) error {
	fs := flag.NewFlagSet("build-android-asset-patch", flag.ContinueOnError)
	patchPlanPath := fs.String("patch-plan", "", "path to android patch plan json")
	patchID := fs.String("patch-id", "", "patch id")
	patchNumber := fs.Uint("patch-number", 0, "patch number")
	releaseID := fs.String("release-id", "", "release id override (defaults to patch plan target release_id)")
	artifactURL := fs.String("artifact-url", "", "artifact url recorded in manifest (defaults to a local placeholder)")
	outputPath := fs.String("out", "", "path to write the asset patch bundle zip")
	reportOutPath := fs.String("report-out", "", "optional path for the asset patch build report json")
	seedBase64 := fs.String("seed-base64", "", "optional manifest signing private seed in base64url format")
	keyID := fs.String("key-id", "", "optional manifest signing key id override")
	allowEmpty := fs.Bool("allow-empty", false, "allow bundles with no overlay file payloads")
	ignoreKernelBlob := fs.Bool("ignore-kernel-blob", false, "ignore kernel_blob.bin drift for debug/local-engine asset proofs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if strings.TrimSpace(*patchPlanPath) == "" || strings.TrimSpace(*patchID) == "" || *patchNumber == 0 || strings.TrimSpace(*outputPath) == "" {
		return errors.New("--patch-plan, --patch-id, --patch-number, and --out are required")
	}
	if strings.TrimSpace(*keyID) != "" && strings.TrimSpace(*seedBase64) == "" {
		return errors.New("--key-id requires --seed-base64")
	}

	report, bundleBytes, err := buildAndroidAssetPatchBundle(androidAssetPatchBuildOptions{
		PatchPlanPath:    *patchPlanPath,
		PatchID:          *patchID,
		PatchNumber:      uint32(*patchNumber),
		ReleaseID:        *releaseID,
		ArtifactURL:      *artifactURL,
		OutputPath:       *outputPath,
		ReportOutPath:    *reportOutPath,
		SeedBase64:       *seedBase64,
		KeyID:            *keyID,
		AllowEmpty:       *allowEmpty,
		IgnoreKernelBlob: *ignoreKernelBlob,
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

func buildAndroidAssetPatchBundle(options androidAssetPatchBuildOptions) (*androidAssetPatchBundleReport, []byte, error) {
	return androidpatch.BuildAssetPatchBundle(options)
}

func loadAndroidPatchPlan(path string) (*androidPatchPlan, error) {
	return androidpatch.LoadPlan(path)
}

func buildPatchBundleArchive(
	manifest domain.PatchManifest,
	artifactBytes []byte,
	overlayFiles map[string]androidArtifactFile,
) ([]byte, error) {
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode asset patch manifest: %w", err)
	}

	var output bytes.Buffer
	writer := newBestCompressionZipWriter(&output)

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

	overlayPaths := make([]string, 0, len(overlayFiles))
	for overlayPath := range overlayFiles {
		overlayPaths = append(overlayPaths, overlayPath)
	}
	sort.Strings(overlayPaths)
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
