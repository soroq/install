package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"soroq/backend/internal/domain"
	"soroq/backend/internal/signing"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "release":
		exitIfErr(runRelease(os.Args[2:]))
	case "patch":
		exitIfErr(runPatch(os.Args[2:]))
	case "rollback":
		exitIfErr(runRollback(os.Args[2:]))
	case "create-app":
		exitIfErr(runCreateApp(os.Args[2:]))
	case "create-release":
		exitIfErr(runCreateRelease(os.Args[2:]))
	case "capture-android-release-snapshot":
		exitIfErr(runCaptureAndroidReleaseSnapshot(os.Args[2:]))
	case "compare-android-release-snapshots":
		exitIfErr(runCompareAndroidReleaseSnapshots(os.Args[2:]))
	case "prepare-android-patch-plan":
		exitIfErr(runPrepareAndroidPatchPlan(os.Args[2:]))
	case "prepare-android-code-patch-plan":
		exitIfErr(runPrepareAndroidCodePatchPlan(os.Args[2:]))
	case "build-android-code-patch":
		exitIfErr(runBuildAndroidCodePatch(os.Args[2:]))
	case "benchmark-android-code-delta":
		exitIfErr(runBenchmarkAndroidCodeDelta(os.Args[2:]))
	case "build-android-asset-patch":
		exitIfErr(runBuildAndroidAssetPatch(os.Args[2:]))
	case "build-ios-runtime-managed-dart-patch":
		exitIfErr(runBuildIOSRuntimeManagedDartPatch(os.Args[2:]))
	case "create-patch":
		exitIfErr(runCreatePatch(os.Args[2:]))
	case "upload-patch-bundle":
		exitIfErr(runUploadPatchBundle(os.Args[2:]))
	case "rollback-patch":
		exitIfErr(runRollbackPatch(os.Args[2:]))
	case "patch-check":
		exitIfErr(runPatchCheck(os.Args[2:]))
	case "report-boot":
		exitIfErr(runReportBoot(os.Args[2:]))
	case "patch-health":
		exitIfErr(runPatchHealth(os.Args[2:]))
	case "manifest-keygen":
		exitIfErr(runManifestKeygen())
	case "manifest-sign":
		exitIfErr(runManifestSign(os.Args[2:]))
	case "manifest-keyring-public":
		exitIfErr(runManifestKeyringPublic(os.Args[2:]))
	case "verify-engine-bundle":
		exitIfErr(runVerifyEngineBundleCmd(os.Args[2:]))
	case "-h", "--help", "help":
		// Explicit help exits 0 consistently on every platform (a bare invocation still
		// exits nonzero as a usage error). Keeps CI harnesses simple + cross-platform.
		usage()
		os.Exit(0)
	default:
		usage()
		os.Exit(2)
	}
}

func runCreateApp(args []string) error {
	fs := flag.NewFlagSet("create-app", flag.ContinueOnError)
	apiBase := fs.String("api", "http://localhost:8080", "control plane base URL")
	id := fs.String("id", "", "app id")
	name := fs.String("name", "", "display name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *name == "" {
		return errors.New("--id and --name are required")
	}
	_, err := postJSON(*apiBase+"/v1/apps", domain.CreateAppRequest{
		ID:          *id,
		DisplayName: *name,
	})
	return err
}

func runCreateRelease(args []string) error {
	fs := flag.NewFlagSet("create-release", flag.ContinueOnError)
	apiBase := fs.String("api", "http://localhost:8080", "control plane base URL")
	id := fs.String("id", "", "release id")
	appID := fs.String("app-id", "", "app id")
	runtimeID := fs.String("runtime-id", "", "runtime id")
	version := fs.String("version", "", "release version")
	platform := fs.String("platform", "android", "platform")
	arch := fs.String("arch", "arm64-v8a", "architecture")
	channel := fs.String("channel", "stable", "channel")
	manifestSigningKeyID := fs.String("manifest-key-id", "", "manifest signing key id for this release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *appID == "" || *runtimeID == "" || *version == "" {
		return errors.New("--id, --app-id, --runtime-id, and --version are required")
	}
	_, err := postJSON(*apiBase+"/v1/releases", domain.CreateReleaseRequest{
		ID:                   *id,
		AppID:                *appID,
		RuntimeID:            *runtimeID,
		Version:              *version,
		Platform:             *platform,
		Arch:                 *arch,
		Channel:              *channel,
		ManifestSigningKeyID: *manifestSigningKeyID,
	})
	return err
}

func runCreatePatch(args []string) error {
	fs := flag.NewFlagSet("create-patch", flag.ContinueOnError)
	apiBase := fs.String("api", "http://localhost:8080", "control plane base URL")
	id := fs.String("id", "", "patch id")
	appID := fs.String("app-id", "", "app id")
	releaseID := fs.String("release-id", "", "release id")
	runtimeID := fs.String("runtime-id", "", "runtime id")
	channel := fs.String("channel", "stable", "channel")
	kind := fs.String("kind", string(domain.PatchKindExperimentalNativeAOT), "patch kind")
	activation := fs.String("activation", string(domain.ActivationNextColdStart), "activation mode")
	manifestURL := fs.String("manifest-url", "", "manifest URL")
	bundleURLFlag := fs.String("bundle-url", "", "bundle URL")
	bundlePath := fs.String("bundle", "", "local patch bundle path to upload")
	rollout := fs.Int("rollout", 100, "rollout percentage")
	manifestSigningKeyID := fs.String("manifest-key-id", "", "override manifest signing key id for this patch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *appID == "" || *releaseID == "" || *runtimeID == "" {
		return errors.New("--id, --app-id, --release-id, and --runtime-id are required")
	}

	hostedPatchURL := strings.TrimRight(*apiBase, "/") + "/v1/patches/" + *id
	bundleURL := strings.TrimSpace(*bundleURLFlag)
	if *bundlePath != "" {
		if bundleURL == "" {
			bundleURL = hostedPatchURL + "/bundle"
		}
		if *manifestURL == "" {
			*manifestURL = hostedPatchURL + "/manifest"
		}
	}
	if *manifestURL == "" {
		return errors.New("--manifest-url is required when --bundle is not provided")
	}
	_, err := postJSON(*apiBase+"/v1/patches", domain.CreatePatchRequest{
		ID:                   *id,
		AppID:                *appID,
		ReleaseID:            *releaseID,
		RuntimeID:            *runtimeID,
		Channel:              *channel,
		Kind:                 domain.PatchKind(*kind),
		ActivationMode:       domain.ActivationMode(*activation),
		ManifestURL:          *manifestURL,
		BundleURL:            bundleURL,
		RolloutPercent:       *rollout,
		ManifestSigningKeyID: *manifestSigningKeyID,
	})
	if err != nil {
		return err
	}

	if *bundlePath == "" {
		return nil
	}

	return uploadPatchBundle(*apiBase, *id, *bundlePath)
}

func runUploadPatchBundle(args []string) error {
	fs := flag.NewFlagSet("upload-patch-bundle", flag.ContinueOnError)
	apiBase := fs.String("api", "http://localhost:8080", "control plane base URL")
	patchID := fs.String("patch-id", "", "patch id")
	bundlePath := fs.String("bundle", "", "local patch bundle path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *patchID == "" || *bundlePath == "" {
		return errors.New("--patch-id and --bundle are required")
	}

	return uploadPatchBundle(*apiBase, *patchID, *bundlePath)
}

func runRollbackPatch(args []string) error {
	fs := flag.NewFlagSet("rollback-patch", flag.ContinueOnError)
	apiBase := fs.String("api", "http://localhost:8080", "control plane base URL")
	patchID := fs.String("patch-id", "", "patch id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *patchID == "" {
		return errors.New("--patch-id is required")
	}

	req, err := http.NewRequest(http.MethodPost, *apiBase+"/v1/patches/"+*patchID+"/rollback", nil)
	if err != nil {
		return err
	}
	applyOperatorHeaders(req)
	return doRequest(req)
}

func runPatchCheck(args []string) error {
	fs := flag.NewFlagSet("patch-check", flag.ContinueOnError)
	apiBase := fs.String("api", "http://localhost:8080", "control plane base URL")
	appID := fs.String("app-id", "", "app id")
	runtimeID := fs.String("runtime-id", "", "runtime id")
	channel := fs.String("channel", "stable", "channel")
	currentPatch := fs.Int("current-patch", 0, "current patch number")
	clientID := fs.String("client-id", "local-cli", "client id")
	kind := fs.String("kind", "", "patch kind lane; use config for config OTA checks, omit for native/runtime-stageable checks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *appID == "" || *runtimeID == "" {
		return errors.New("--app-id and --runtime-id are required")
	}
	_, err := postJSON(*apiBase+"/v1/patch-check", domain.PatchCheckRequest{
		AppID:              *appID,
		RuntimeID:          *runtimeID,
		Channel:            *channel,
		CurrentPatchNumber: *currentPatch,
		ClientID:           *clientID,
		Kind:               domain.PatchKind(*kind),
	})
	return err
}

func runReportBoot(args []string) error {
	fs := flag.NewFlagSet("report-boot", flag.ContinueOnError)
	apiBase := fs.String("api", "http://localhost:8080", "control plane base URL")
	appID := fs.String("app-id", "", "app id")
	runtimeID := fs.String("runtime-id", "", "runtime id")
	channel := fs.String("channel", "stable", "channel")
	clientID := fs.String("client-id", "local-cli", "client id")
	activePatch := fs.Int("active-patch", -1, "active patch number")
	eventKind := fs.String("event-kind", "", "runtime event kind")
	patchNumber := fs.Int("patch-number", -1, "single patch number for success/failure events")
	patchNumbers := fs.String("patch-numbers", "", "comma separated patch numbers for multi-patch events")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *appID == "" || *runtimeID == "" || *eventKind == "" {
		return errors.New("--app-id, --runtime-id, and --event-kind are required")
	}

	event := domain.RuntimeEvent{
		Kind: domain.RuntimeEventKind(*eventKind),
	}
	if *patchNumber >= 0 {
		value := *patchNumber
		event.PatchNumber = &value
	}
	if *patchNumbers != "" {
		values, err := parseIntCSV(*patchNumbers)
		if err != nil {
			return err
		}
		event.PatchNumbers = values
	}

	var activePatchNumber *int
	if *activePatch >= 0 {
		value := *activePatch
		activePatchNumber = &value
	}

	_, err := postJSON(*apiBase+"/v1/boot-reports", domain.BootReportRequest{
		AppID:             *appID,
		RuntimeID:         *runtimeID,
		Channel:           *channel,
		ClientID:          *clientID,
		ActivePatchNumber: activePatchNumber,
		Events:            []domain.RuntimeEvent{event},
	})
	return err
}

func runPatchHealth(args []string) error {
	fs := flag.NewFlagSet("patch-health", flag.ContinueOnError)
	apiBase := fs.String("api", "http://localhost:8080", "control plane base URL")
	patchID := fs.String("patch-id", "", "patch id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *patchID == "" {
		return errors.New("--patch-id is required")
	}

	req, err := http.NewRequest(http.MethodGet, *apiBase+"/v1/patches/"+*patchID+"/health", nil)
	if err != nil {
		return err
	}
	applyOperatorHeaders(req)
	return doRequest(req)
}

func runManifestKeygen() error {
	privateSeedBase64, publicKeyBase64, keyID, err := signing.GenerateManifestKeyPair()
	if err != nil {
		return err
	}

	output := map[string]string{
		"algorithm":            signing.ManifestSignatureScheme,
		"key_id":               keyID,
		"private_seed_base64":  privateSeedBase64,
		"public_key_base64":    publicKeyBase64,
		"public_key_size_hint": strconv.Itoa(ed25519.PublicKeySize),
	}
	encoded, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func runManifestSign(args []string) error {
	fs := flag.NewFlagSet("manifest-sign", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "path to manifest JSON file")
	seedBase64 := fs.String("seed-base64", "", "manifest signing private seed in base64url format")
	keyID := fs.String("key-id", "", "optional manifest signing key id override")
	outputPath := fs.String("out", "", "optional output path; defaults to in-place")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*manifestPath) == "" || strings.TrimSpace(*seedBase64) == "" {
		return errors.New("--manifest and --seed-base64 are required")
	}

	manifestBytes, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}

	var manifest domain.PatchManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("decode manifest JSON: %w", err)
	}

	signer, err := signing.NewManifestSignerFromSeedBase64(*seedBase64, *keyID)
	if err != nil {
		return err
	}

	signature, err := signer.SignManifest(manifest)
	if err != nil {
		return err
	}
	resolvedKeyID := signer.KeyID()
	manifest.SignatureKeyID = &resolvedKeyID
	manifest.Signature = &signature

	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	targetPath := strings.TrimSpace(*outputPath)
	if targetPath == "" {
		targetPath = *manifestPath
	}
	if err := os.WriteFile(targetPath, encoded, 0o644); err != nil {
		return err
	}

	fmt.Printf(
		"{\"manifest\":\"%s\",\"signature_key_id\":\"%s\"}\n",
		targetPath,
		resolvedKeyID,
	)
	return nil
}

func runManifestKeyringPublic(args []string) error {
	fs := flag.NewFlagSet("manifest-keyring-public", flag.ContinueOnError)
	keysFile := fs.String("keys-file", "", "path to manifest signing keyring JSON file")
	keysetVersion := fs.Int("keyset-version", 0, "optional manifest trust keyset version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*keysFile) == "" {
		return errors.New("--keys-file is required")
	}

	signerSet, err := signing.LoadManifestSignerSetFromFile(*keysFile)
	if err != nil {
		return err
	}

	var keysetVersionPtr *int
	if *keysetVersion > 0 {
		keysetVersionPtr = keysetVersion
	}

	output := map[string]any{
		"default_key_id": signerSet.DefaultKeyID(),
		"manifest_trust": signerSet.ManifestTrustConfig(keysetVersionPtr),
	}
	encoded, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func postJSON(url string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyOperatorHeaders(req)
	return nil, doRequest(req)
}

func uploadPatchBundle(apiBase, patchID, bundlePath string) error {
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		return err
	}

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
	return doRequest(req)
}

func doRequest(req *http.Request) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, string(body))
	}

	fmt.Println(string(body))
	return nil
}

func applyOperatorHeaders(req *http.Request) {
	token := firstNonEmptyEnv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "SOROQ_OPERATOR_TOKEN")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if email := strings.TrimSpace(os.Getenv("SOROQ_OPERATOR_EMAIL")); email != "" {
		req.Header.Set("X-Soroq-Operator-Email", email)
	}
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		value := strings.TrimSpace(os.Getenv(name))
		if value != "" {
			return value
		}
	}
	return ""
}

func exitIfErr(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: soroqctl <command> [flags]

commands:
  release ios
  release ios-engine
  patch ios
  patch ios-engine
  rollback ios-engine
  create-app
  create-release
  capture-android-release-snapshot
  compare-android-release-snapshots
  prepare-android-patch-plan
  prepare-android-code-patch-plan
  build-android-code-patch
  benchmark-android-code-delta
  build-android-asset-patch
  build-ios-runtime-managed-dart-patch
  create-patch
  upload-patch-bundle
  rollback-patch
  patch-check
  report-boot
  patch-health
  manifest-keygen
  manifest-sign
  manifest-keyring-public`)
}

func parseIntCSV(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	values := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		value, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid patch number %q: %w", part, err)
		}
		values = append(values, value)
	}
	return values, nil
}
