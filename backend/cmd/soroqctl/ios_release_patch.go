package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"soroq/backend/internal/domain"
)

type iosPatchWorkflowResult struct {
	PatchID       string `json:"patch_id"`
	PatchNumber   int    `json:"patch_number"`
	ReleaseID     string `json:"release_id"`
	RuntimeID     string `json:"runtime_id"`
	Channel       string `json:"channel"`
	ManifestURL   string `json:"manifest_url"`
	BundleURL     string `json:"bundle_url"`
	BundlePath    string `json:"bundle_path"`
	ReportOutPath string `json:"report_out_path,omitempty"`
}

func runRelease(args []string) error {
	if len(args) == 0 {
		return errors.New("internal experimental usage: soroqctl release ios|ios-engine [flags]")
	}
	switch args[0] {
	case "ios":
		return runReleaseIOS(args[1:])
	case "ios-engine":
		return runReleaseIOSEngine(args[1:])
	default:
		return errors.New("internal experimental usage: soroqctl release ios|ios-engine [flags]")
	}
}

func runPatch(args []string) error {
	if len(args) == 0 {
		return errors.New("internal experimental usage: soroqctl patch ios|ios-engine [flags]")
	}
	switch args[0] {
	case "ios":
		return runPatchIOS(args[1:])
	case "ios-engine":
		return runPatchIOSEngine(args[1:])
	default:
		return errors.New("internal experimental usage: soroqctl patch ios|ios-engine [flags]")
	}
}

// runRollback routes the engine-lane rollback (distinct from rollback-patch, which is the
// config/runtime-managed control-plane rollback).
func runRollback(args []string) error {
	if len(args) == 0 || args[0] != "ios-engine" {
		return errors.New("internal experimental usage: soroqctl rollback ios-engine [flags]")
	}
	return runRollbackIOSEngine(args[1:])
}

func runReleaseIOS(args []string) error {
	fs := flag.NewFlagSet("release ios", flag.ContinueOnError)
	apiBase := fs.String("api", "http://localhost:8080", "control plane base URL")
	id := fs.String("id", "", "iOS release id")
	releaseID := fs.String("release-id", "", "iOS release id alias")
	appID := fs.String("app-id", "", "app id")
	runtimeID := fs.String("runtime-id", "", "iOS runtime compatibility id")
	version := fs.String("version", "", "release version")
	arch := fs.String("arch", "arm64", "iOS architecture")
	channel := fs.String("channel", "stable", "channel")
	manifestSigningKeyID := fs.String("manifest-key-id", "", "manifest signing key id for this release")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolvedID := firstNonEmpty(*id, *releaseID)
	if resolvedID == "" || strings.TrimSpace(*appID) == "" || strings.TrimSpace(*runtimeID) == "" || strings.TrimSpace(*version) == "" {
		return errors.New("--id or --release-id, --app-id, --runtime-id, and --version are required")
	}

	var release domain.Release
	if err := postJSONDecode(strings.TrimRight(*apiBase, "/")+"/v1/releases", domain.CreateReleaseRequest{
		ID:                   resolvedID,
		AppID:                strings.TrimSpace(*appID),
		RuntimeID:            strings.TrimSpace(*runtimeID),
		Version:              strings.TrimSpace(*version),
		Platform:             "ios",
		Arch:                 normalizedDefaultString(*arch, "arm64"),
		Channel:              normalizedDefaultString(*channel, "stable"),
		ManifestSigningKeyID: strings.TrimSpace(*manifestSigningKeyID),
	}, &release); err != nil {
		return err
	}

	if *format == "json" {
		return writeJSONOutput(release, "")
	}
	fmt.Printf("created internal experimental iOS release %s platform=ios arch=%s channel=%s\n", release.ID, release.Arch, release.Channel)
	return nil
}

func runPatchIOS(args []string) error {
	fs := flag.NewFlagSet("patch ios", flag.ContinueOnError)
	apiBase := fs.String("api", "http://localhost:8080", "control plane base URL")
	id := fs.String("id", "", "patch id")
	appID := fs.String("app-id", "", "app id")
	releaseID := fs.String("release-id", "", "release id")
	runtimeID := fs.String("runtime-id", "", "iOS runtime compatibility id")
	channel := fs.String("channel", "stable", "channel")
	kernelBlobPath := fs.String("kernel-blob", "", "candidate iOS build App.framework/flutter_assets/kernel_blob.bin")
	baseKernelBlobPath := fs.String("base-kernel-blob", "", "optional base iOS build App.framework/flutter_assets/kernel_blob.bin for data-only delta transport")
	outPath := fs.String("out", "", "path to write the signed runtime-managed Dart patch bundle zip")
	reportOutPath := fs.String("report-out", "", "optional path for the build report json")
	rollout := fs.Int("rollout", 100, "rollout percentage")
	seedBase64 := fs.String("seed-base64", "", "required manifest signing private seed in base64url format")
	keyID := fs.String("key-id", "", "optional manifest signing key id override")
	manifestSigningKeyID := fs.String("manifest-key-id", "", "override manifest signing key id for this patch")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if strings.TrimSpace(*id) == "" || strings.TrimSpace(*appID) == "" || strings.TrimSpace(*releaseID) == "" || strings.TrimSpace(*runtimeID) == "" {
		return errors.New("--id, --app-id, --release-id, and --runtime-id are required")
	}
	if strings.TrimSpace(*kernelBlobPath) == "" || strings.TrimSpace(*outPath) == "" || strings.TrimSpace(*seedBase64) == "" {
		return errors.New("--kernel-blob, --out, and --seed-base64 are required")
	}

	apiRoot := strings.TrimRight(*apiBase, "/")
	hostedPatchURL := apiRoot + "/v1/patches/" + strings.TrimSpace(*id)
	manifestURL := hostedPatchURL + "/manifest"
	bundleURL := hostedPatchURL + "/bundle"

	var patch domain.Patch
	if err := postJSONDecode(apiRoot+"/v1/patches", domain.CreatePatchRequest{
		ID:                   strings.TrimSpace(*id),
		AppID:                strings.TrimSpace(*appID),
		ReleaseID:            strings.TrimSpace(*releaseID),
		RuntimeID:            strings.TrimSpace(*runtimeID),
		Channel:              normalizedDefaultString(*channel, "stable"),
		Kind:                 domain.PatchKindRuntimeManagedDart,
		ActivationMode:       domain.ActivationNextColdStart,
		ManifestURL:          manifestURL,
		BundleURL:            bundleURL,
		RolloutPercent:       *rollout,
		ManifestSigningKeyID: strings.TrimSpace(*manifestSigningKeyID),
	}, &patch); err != nil {
		return err
	}
	if patch.Number <= 0 {
		return fmt.Errorf("server returned invalid patch number %d", patch.Number)
	}

	report, bundleBytes, err := buildIOSRuntimeManagedDartPatchBundle(iosRuntimeManagedDartPatchBuildOptions{
		KernelBlobPath:     *kernelBlobPath,
		BaseKernelBlobPath: *baseKernelBlobPath,
		PatchID:            patch.ID,
		PatchNumber:        uint32(patch.Number),
		RuntimeID:          patch.RuntimeID,
		ReleaseID:          patch.ReleaseID,
		Channel:            patch.Channel,
		ArtifactURL:        patch.BundleURL,
		OutputPath:         *outPath,
		ReportOutPath:      *reportOutPath,
		SeedBase64:         *seedBase64,
		KeyID:              *keyID,
	})
	if err != nil {
		return err
	}

	outputPathClean := filepath.Clean(*outPath)
	if err := os.MkdirAll(filepath.Dir(outputPathClean), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(outputPathClean, bundleBytes, 0o644); err != nil {
		return fmt.Errorf("write iOS runtime-managed Dart patch bundle: %w", err)
	}
	if strings.TrimSpace(*reportOutPath) != "" {
		if err := writeJSONOutput(report, *reportOutPath); err != nil {
			return err
		}
	}
	if err := uploadPatchBundleBytes(apiRoot, patch.ID, bundleBytes); err != nil {
		return err
	}

	result := iosPatchWorkflowResult{
		PatchID:       patch.ID,
		PatchNumber:   patch.Number,
		ReleaseID:     patch.ReleaseID,
		RuntimeID:     patch.RuntimeID,
		Channel:       patch.Channel,
		ManifestURL:   patch.ManifestURL,
		BundleURL:     patch.BundleURL,
		BundlePath:    outputPathClean,
		ReportOutPath: strings.TrimSpace(*reportOutPath),
	}
	if *format == "json" {
		return writeJSONOutput(result, "")
	}
	fmt.Printf("created internal experimental iOS runtime-managed Dart patch %s number=%d activation=next_cold_start bundle=%s\n", result.PatchID, result.PatchNumber, result.BundlePath)
	return nil
}

func postJSONDecode(url string, payload any, output any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	applyOperatorHeaders(req)
	respBody, err := doRequestBody(req)
	if err != nil {
		return err
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(respBody, output)
}

func uploadPatchBundleBytes(apiBase, patchID string, bundleBytes []byte) error {
	req, err := http.NewRequest(
		http.MethodPost,
		strings.TrimRight(apiBase, "/")+"/v1/patches/"+patchID+"/bundle",
		bytes.NewReader(bundleBytes),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/zip")
	applyOperatorHeaders(req)
	_, err = doRequestBody(req)
	return err
}

func doRequestBody(req *http.Request) ([]byte, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	return body, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
