package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/domain"
)

type previewAndroidSummary struct {
	ProjectDir         string                    `json:"project_dir"`
	AppID              string                    `json:"app_id"`
	Channel            string                    `json:"channel"`
	Track              string                    `json:"track"`
	Release            domain.Release            `json:"release"`
	Artifact           string                    `json:"artifact"`
	Snapshot           *androidrelease.Snapshot  `json:"snapshot"`
	ClientID           string                    `json:"client_id"`
	CurrentPatchNumber int                       `json:"current_patch_number"`
	PatchCheck         domain.PatchCheckResponse `json:"patch_check"`
	DownloadedManifest string                    `json:"downloaded_manifest,omitempty"`
	DownloadedBundle   string                    `json:"downloaded_bundle,omitempty"`
	Installed          bool                      `json:"installed"`
	Launched           bool                      `json:"launched"`
	DeviceID           string                    `json:"device_id,omitempty"`
	PackageName        string                    `json:"package_name,omitempty"`
}

func runPreview(args []string) error {
	if len(args) == 0 {
		previewUsage()
		return errAlreadyPrinted
	}

	switch args[0] {
	case "android":
		return runPreviewAndroid(args[1:])
	case "ios", "ios-engine":
		return runPreviewIOSEngine(args[1:])
	case "-h", "--help", "help":
		previewUsage()
		return nil
	default:
		previewUsage()
		return errAlreadyPrinted
	}
}

func previewUsage() {
	fmt.Fprintln(os.Stdout, `usage: soroq preview <platform> [flags]

platforms:
  android     preview the hosted Android release artifact and runtime patch response
  ios         preview the EXPERIMENTAL engine-lane device manifest (version/rollback/patch metadata)
  ios-engine  alias of preview ios`)
}

// previewIOSEngineSummary is the device-equivalent view of the engine lane: exactly what an iOS device
// fetches from /v1/engine/{app}/{channel}. It reports the served manifest version, whether it is a
// version-0 rollback, and (optionally) a device-equivalent signature verification.
type previewIOSEngineSummary struct {
	APIBase          string `json:"api_base"`
	AppID            string `json:"app_id"`
	Channel          string `json:"channel"`
	ClientID         string `json:"client_id,omitempty"`
	Track            string `json:"track,omitempty"`
	HostBase         string `json:"host_base"`
	PatchAvailable   bool   `json:"patch_available"`
	ManifestVersion  int    `json:"manifest_version"`
	IsRollback       bool   `json:"is_rollback"`
	BytecodeSha256   string `json:"bytecode_sha256,omitempty"`
	PatchCount       int    `json:"patch_count"`
	SignaturePresent bool   `json:"signature_present"`
	SignatureVerified *bool `json:"signature_verified,omitempty"`
	Experimental     bool   `json:"experimental"`
}

// runPreviewIOSEngine fetches the device-equivalent engine manifest (and its detached sig) and reports
// version/rollback/patch metadata. With --pubkey-hex it ALSO replicates the device's Ed25519 verify
// over the EXACT served manifest bytes (the same check the in-app pinned-key verifier runs). Provable
// against a local soroqd; no operator credentials are needed (the engine serve route is public read).
func runPreviewIOSEngine(args []string) error {
	fs := flag.NewFlagSet("preview ios-engine", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL (or a local soroqd)")
	appID := fs.String("app-id", "", "app id")
	channel := fs.String("channel", "stable", "release channel")
	clientID := fs.String("cid", "", "optional device client id for staged-rollout bucketing")
	track := fs.String("track", "", "optional patch track to preview")
	pubkeyHex := fs.String("pubkey-hex", "", "optional 32-byte Ed25519 public key (hex) to replicate the device manifest verify over the served bytes")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq preview ios-engine --app-id com.example.app [--channel stable] [--api http://localhost:8080] [--cid device-123] [--track stable] [--pubkey-hex <ed25519-pub-hex>] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	resolvedAppID := strings.TrimSpace(*appID)
	if resolvedAppID == "" {
		return errors.New("--app-id is required")
	}
	resolvedChannel := strings.TrimSpace(*channel)
	if resolvedChannel == "" {
		resolvedChannel = "stable"
	}
	hostBase := strings.TrimRight(*apiBase, "/") + "/v1/engine/" + url.PathEscape(resolvedAppID) + "/" + url.PathEscape(resolvedChannel)
	query := ""
	q := url.Values{}
	if strings.TrimSpace(*clientID) != "" {
		q.Set("cid", strings.TrimSpace(*clientID))
	}
	if strings.TrimSpace(*track) != "" {
		q.Set("track", strings.TrimSpace(*track))
	}
	if encoded := q.Encode(); encoded != "" {
		query = "?" + encoded
	}

	summary := previewIOSEngineSummary{
		APIBase:      strings.TrimRight(*apiBase, "/"),
		AppID:        resolvedAppID,
		Channel:      resolvedChannel,
		ClientID:     strings.TrimSpace(*clientID),
		Track:        strings.TrimSpace(*track),
		HostBase:     hostBase,
		Experimental: true,
	}

	manifestBytes, status, err := engineGet(hostBase + "/manifest.json" + query)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		// No active engine patch -> device stays on / returns to base. Coherent, not an error.
		summary.PatchAvailable = false
		return reportPreviewIOSEngine(summary, *jsonOut)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("engine manifest fetch failed: status %d: %s", status, strings.TrimSpace(string(manifestBytes)))
	}
	summary.PatchAvailable = true

	var m struct {
		Version        int    `json:"version"`
		BytecodeSha256 string `json:"bytecodeSha256"`
		Patches        []struct {
			Index    int    `json:"index"`
			Bytecode string `json:"bytecode"`
		} `json:"patches"`
	}
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return fmt.Errorf("parse served engine manifest: %w", err)
	}
	summary.ManifestVersion = m.Version
	summary.IsRollback = m.Version == 0 && len(m.Patches) == 0
	summary.BytecodeSha256 = m.BytecodeSha256
	summary.PatchCount = len(m.Patches)

	sigBytes, sigStatus, err := engineGet(hostBase + "/manifest.sig" + query)
	if err != nil {
		return err
	}
	summary.SignaturePresent = sigStatus >= 200 && sigStatus < 300 && len(bytes.TrimSpace(sigBytes)) > 0

	// Optional device-equivalent verify over the EXACT served manifest bytes.
	if pk := strings.TrimSpace(*pubkeyHex); pk != "" {
		if !summary.SignaturePresent {
			return errors.New("--pubkey-hex given but the engine serve returned no manifest.sig to verify")
		}
		pub, err := hex.DecodeString(pk)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return fmt.Errorf("--pubkey-hex must be a %d-byte Ed25519 public key in hex", ed25519.PublicKeySize)
		}
		sigRaw, err := hex.DecodeString(strings.TrimSpace(string(sigBytes)))
		if err != nil {
			return fmt.Errorf("served manifest.sig is not hex: %w", err)
		}
		verified := ed25519.Verify(ed25519.PublicKey(pub), manifestBytes, sigRaw)
		summary.SignatureVerified = &verified
		if !verified {
			return errors.New("device-equivalent verify FAILED: served manifest bytes do not verify against --pubkey-hex")
		}
	}
	return reportPreviewIOSEngine(summary, *jsonOut)
}

// engineGet performs a public (no-credential) GET against the engine serve route and returns the
// body + status. A 404 is returned as a status (not an error) so the caller can treat "no active
// engine patch" as a coherent state.
func engineGet(rawURL string) ([]byte, int, error) {
	resp, err := http.Get(rawURL)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func reportPreviewIOSEngine(summary previewIOSEngineSummary, jsonOut bool) error {
	if jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}
	fmt.Fprintln(os.Stdout, "Soroq iOS engine-lane preview (EXPERIMENTAL — Dart-code OTA via the soroq interpreter-in-engine)")
	fmt.Fprintf(os.Stdout, "app_id: %s\n", summary.AppID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", summary.Channel)
	fmt.Fprintf(os.Stdout, "host_base: %s\n", summary.HostBase)
	fmt.Fprintf(os.Stdout, "patch_available: %s\n", yesNo(summary.PatchAvailable))
	if !summary.PatchAvailable {
		fmt.Fprintln(os.Stdout, "state: no active engine patch -> device stays on / returns to base (coherent)")
		return nil
	}
	fmt.Fprintf(os.Stdout, "manifest_version: %d\n", summary.ManifestVersion)
	fmt.Fprintf(os.Stdout, "is_rollback: %s\n", yesNo(summary.IsRollback))
	fmt.Fprintf(os.Stdout, "patch_count: %d\n", summary.PatchCount)
	if summary.BytecodeSha256 != "" {
		fmt.Fprintf(os.Stdout, "bytecode_sha256: %s\n", summary.BytecodeSha256)
	}
	fmt.Fprintf(os.Stdout, "signature_present: %s\n", yesNo(summary.SignaturePresent))
	if summary.SignatureVerified != nil {
		fmt.Fprintf(os.Stdout, "signature_verified: %s (device-equivalent Ed25519 over served bytes)\n", yesNo(*summary.SignatureVerified))
	}
	fmt.Fprintln(os.Stdout, "note: this is the EXPERIMENTAL optimized-profile engine lane — NOT a production/App-Store release engine.")
	return nil
}

func runPreviewAndroid(args []string) error {
	fs := flag.NewFlagSet("preview android", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	appID := fs.String("app-id", "", "app id override (defaults to soroq.yaml)")
	channel := fs.String("channel", "", "channel override (defaults to soroq.yaml)")
	track := fs.String("track", "", "patch track to preview, such as stable, staging, or beta")
	releaseID := fs.String("release-id", "", "release id to preview")
	releaseVersion := fs.String("release-version", "latest", "release version to preview; use latest for the newest Android release in the channel")
	clientID := fs.String("client-id", "soroq-preview", "runtime client id to use for patch-check proof")
	currentPatchNumber := fs.Int("current-patch-number", 0, "current patch number reported to patch-check")
	patchKindRaw := fs.String("kind", "auto", "patch kind to request: auto, asset, code, or experimental_native_aot")
	downloadPatch := fs.Bool("download-patch", false, "download the available patch manifest and bundle to the preview output directory")
	outputDir := fs.String("output-dir", "", "directory for downloaded preview patch artifacts")
	install := fs.Bool("install", false, "install the selected release APK on a connected device or emulator")
	launch := fs.Bool("launch", false, "launch the installed app on a connected device or emulator")
	adbPath := fs.String("adb", "adb", "adb executable path")
	bundletoolPath := fs.String("bundletool", "bundletool", "bundletool executable or jar path for hosted AAB installs")
	deviceID := fs.String("device-id", "", "optional adb device serial")
	packageName := fs.String("package", "", "Android package name to launch; defaults to release app id when it is package-shaped")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq preview android [--release-version latest|1.2.3+45] [--release-id release-123] [--project-dir .] [--api https://api.soroq.dev] [--app-id com.example.app] [--channel stable] [--track stable|staging|beta] [--client-id device-123] [--current-patch-number 0] [--kind auto|asset|code] [--download-patch] [--output-dir .soroq/previews] [--install] [--launch] [--device-id emulator-5554] [--package com.example.app] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *currentPatchNumber < 0 {
		return errors.New("--current-patch-number must be zero or greater")
	}
	requestedPatchKind, err := normalizeAndroidPatchKindFlag(*patchKindRaw)
	if err != nil {
		return err
	}
	resolvedTrack := domain.NormalizePatchTrack(*track)
	if !domain.IsKnownPatchTrack(resolvedTrack) {
		return fmt.Errorf("--track should be a slug such as stable, staging, or beta; got %q", *track)
	}

	status, err := inspectProject(*projectDir)
	if err != nil {
		return err
	}
	state, err := loadProjectCLIState(status.ProjectDir)
	if err != nil {
		return err
	}
	lastRelease := state.LastAndroidRelease
	resolvedAPIBase := strings.TrimRight(*apiBase, "/")
	if !flagWasSet(fs, "api") && lastRelease != nil && strings.TrimSpace(lastRelease.APIBase) != "" {
		resolvedAPIBase = strings.TrimRight(lastRelease.APIBase, "/")
	}
	projectConfig, err := resolvePreviewProjectConfig(status, strings.TrimSpace(*appID), strings.TrimSpace(*channel))
	if err != nil {
		return err
	}

	resolvedReleaseID := strings.TrimSpace(*releaseID)
	resolvedReleaseVersion := strings.TrimSpace(*releaseVersion)
	if resolvedReleaseID != "" && flagWasSet(fs, "release-version") {
		return errors.New("use either --release-id or --release-version, not both")
	}
	if resolvedReleaseID == "" && !flagWasSet(fs, "release-version") && lastRelease != nil &&
		strings.TrimSpace(lastRelease.ReleaseID) != "" &&
		lastRelease.AppID == projectConfig.AppID &&
		lastRelease.Channel == projectConfig.Channel {
		resolvedReleaseID = lastRelease.ReleaseID
	}
	var release domain.Release
	if resolvedReleaseID != "" {
		release, err = getJSONDecode[domain.Release](resolvedAPIBase + "/v1/releases/" + url.PathEscape(resolvedReleaseID))
		if err != nil {
			return err
		}
	} else {
		if resolvedReleaseVersion == "" {
			resolvedReleaseVersion = "latest"
		}
		release, err = selectAndroidReleaseForPatch(
			resolvedAPIBase,
			projectConfig.AppID,
			projectConfig.Channel,
			resolvedReleaseVersion,
		)
		if err != nil {
			return err
		}
	}
	if err := validatePreviewRelease(release, projectConfig.AppID, projectConfig.Channel); err != nil {
		return err
	}

	artifactPath, err := downloadReleaseArtifact(resolvedAPIBase, release.ID, status.ProjectDir)
	if err != nil {
		return fmt.Errorf("download hosted release artifact: %w", err)
	}
	snapshot, err := inspectAndroidArtifact(artifactPath)
	if err != nil {
		return err
	}
	if err := validatePreviewSnapshot(snapshot, release); err != nil {
		return err
	}

	patchReq := domain.PatchCheckRequest{
		AppID:              release.AppID,
		ReleaseID:          release.ID,
		ReleaseVersion:     release.Version,
		RuntimeID:          release.RuntimeID,
		Channel:            release.Channel,
		Track:              resolvedTrack,
		CurrentPatchNumber: *currentPatchNumber,
		ClientID:           strings.TrimSpace(*clientID),
		Kind:               requestedPatchKind,
	}
	if patchReq.ClientID == "" {
		return errors.New("--client-id is required")
	}
	patchCheck, err := postRuntimeJSONDecode[domain.PatchCheckResponse](resolvedAPIBase+"/v1/patch-check", patchReq)
	if err != nil {
		return err
	}

	downloadedManifest := ""
	downloadedBundle := ""
	if *downloadPatch && patchCheck.Patch != nil {
		resolvedOutputDir := strings.TrimSpace(*outputDir)
		if resolvedOutputDir == "" {
			resolvedOutputDir = filepath.Join(status.ProjectDir, ".soroq", "previews", release.ID)
		}
		if !filepath.IsAbs(resolvedOutputDir) {
			resolvedOutputDir = filepath.Join(status.ProjectDir, resolvedOutputDir)
		}
		if strings.TrimSpace(patchCheck.Patch.ManifestURL) != "" {
			targetPath := filepath.Join(resolvedOutputDir, safePreviewFileName(patchCheck.Patch.ID)+"-manifest.json")
			if err := downloadURLToFile(patchCheck.Patch.ManifestURL, targetPath); err != nil {
				return err
			}
			downloadedManifest = targetPath
		}
		if strings.TrimSpace(patchCheck.Patch.BundleURL) != "" {
			targetPath := filepath.Join(resolvedOutputDir, safePreviewFileName(patchCheck.Patch.ID)+"-bundle.zip")
			if err := downloadURLToFile(patchCheck.Patch.BundleURL, targetPath); err != nil {
				return err
			}
			downloadedBundle = targetPath
		}
	}
	resolvedDeviceID := strings.TrimSpace(*deviceID)
	resolvedPackageName := strings.TrimSpace(*packageName)
	if resolvedPackageName == "" && looksLikeAndroidAppID(release.AppID) {
		resolvedPackageName = release.AppID
	}
	installed := false
	launched := false
	if *install {
		if err := installPreviewArtifact(previewInstallOptions{
			ArtifactPath:   artifactPath,
			ArtifactType:   snapshot.Artifact.Type,
			ReleaseID:      release.ID,
			OutputDir:      strings.TrimSpace(*outputDir),
			ProjectDir:     status.ProjectDir,
			DeviceID:       resolvedDeviceID,
			ADBPath:        *adbPath,
			BundletoolPath: *bundletoolPath,
		}); err != nil {
			return err
		}
		installed = true
	}
	if *launch {
		if resolvedPackageName == "" {
			return errors.New("--package is required for launch when release app_id is not an Android package id")
		}
		if err := runADBCommand(*adbPath, resolvedDeviceID, "shell", "monkey", "-p", resolvedPackageName, "-c", "android.intent.category.LAUNCHER", "1"); err != nil {
			return err
		}
		launched = true
	}

	summary := previewAndroidSummary{
		ProjectDir:         status.ProjectDir,
		AppID:              release.AppID,
		Channel:            release.Channel,
		Track:              patchReq.Track,
		Release:            release,
		Artifact:           artifactPath,
		Snapshot:           snapshot,
		ClientID:           patchReq.ClientID,
		CurrentPatchNumber: patchReq.CurrentPatchNumber,
		PatchCheck:         patchCheck,
		DownloadedManifest: downloadedManifest,
		DownloadedBundle:   downloadedBundle,
		Installed:          installed,
		Launched:           launched,
		DeviceID:           resolvedDeviceID,
		PackageName:        resolvedPackageName,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Soroq Android preview\n")
	fmt.Fprintf(os.Stdout, "app_id: %s\n", release.AppID)
	fmt.Fprintf(os.Stdout, "release_id: %s\n", release.ID)
	fmt.Fprintf(os.Stdout, "version: %s\n", release.Version)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", release.RuntimeID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", release.Channel)
	fmt.Fprintf(os.Stdout, "track: %s\n", patchReq.Track)
	fmt.Fprintf(os.Stdout, "artifact: %s\n", artifactPath)
	fmt.Fprintf(os.Stdout, "patch_available: %s\n", yesNo(patchCheck.PatchAvailable && patchCheck.Patch != nil))
	if patchCheck.Patch != nil {
		fmt.Fprintf(os.Stdout, "patch_id: %s\n", patchCheck.Patch.ID)
		fmt.Fprintf(os.Stdout, "patch_number: %d\n", patchCheck.Patch.Number)
		fmt.Fprintf(os.Stdout, "patch_track: %s\n", domain.NormalizePatchTrack(patchCheck.Patch.Track))
		fmt.Fprintf(os.Stdout, "patch_kind: %s\n", patchCheck.Patch.Kind)
		fmt.Fprintf(os.Stdout, "activation_mode: %s\n", patchCheck.Patch.ActivationMode)
	}
	if downloadedManifest != "" {
		fmt.Fprintf(os.Stdout, "downloaded_manifest: %s\n", downloadedManifest)
	}
	if downloadedBundle != "" {
		fmt.Fprintf(os.Stdout, "downloaded_bundle: %s\n", downloadedBundle)
	}
	if installed {
		fmt.Fprintf(os.Stdout, "installed: yes\n")
	}
	if launched {
		fmt.Fprintf(os.Stdout, "launched: yes\n")
		fmt.Fprintf(os.Stdout, "package: %s\n", resolvedPackageName)
	}
	return nil
}

func resolvePreviewProjectConfig(status projectStatus, appIDOverride string, channelOverride string) (projectCommandConfig, error) {
	if appIDOverride == "" {
		return resolveProjectCommandConfig(status, channelOverride)
	}
	if !status.HasPubspec {
		return projectCommandConfig{}, fmt.Errorf("pubspec.yaml not found in %s", status.ProjectDir)
	}
	if !status.HasSoroqFlutterDependency {
		return projectCommandConfig{}, fmt.Errorf("pubspec.yaml at %s does not declare a soroq_flutter dependency; run `flutter pub add soroq_flutter`", status.PubspecPath)
	}
	if !looksLikeSoroqAppID(appIDOverride) {
		return projectCommandConfig{}, fmt.Errorf("--app-id %q should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens", appIDOverride)
	}
	if status.HasSoroqConfig && strings.TrimSpace(status.AppID) != "" && strings.TrimSpace(status.AppID) != appIDOverride {
		return projectCommandConfig{}, fmt.Errorf("--app-id %q does not match soroq.yaml app_id %q", appIDOverride, status.AppID)
	}
	resolvedChannel := strings.TrimSpace(channelOverride)
	if resolvedChannel == "" {
		resolvedChannel = strings.TrimSpace(status.Channel)
	}
	if resolvedChannel == "" {
		return projectCommandConfig{}, errors.New("--channel is required when soroq.yaml has no channel")
	}
	if !looksLikeChannel(resolvedChannel) {
		return projectCommandConfig{}, fmt.Errorf("channel %q should be a stable slug such as stable, beta, or production", resolvedChannel)
	}
	return projectCommandConfig{AppID: appIDOverride, Channel: resolvedChannel}, nil
}

func validatePreviewRelease(release domain.Release, appID string, channel string) error {
	if release.AppID != appID {
		return fmt.Errorf("release app_id %q does not match requested app_id %q", release.AppID, appID)
	}
	if release.Platform != "android" {
		return fmt.Errorf("release platform %q is not android", release.Platform)
	}
	if release.Channel != channel {
		return fmt.Errorf("release channel %q does not match requested channel %q", release.Channel, channel)
	}
	if strings.TrimSpace(release.RuntimeID) == "" {
		return fmt.Errorf("release %s is missing runtime_id", release.ID)
	}
	return nil
}

func validatePreviewSnapshot(snapshot *androidrelease.Snapshot, release domain.Release) error {
	if snapshot == nil {
		return errors.New("release artifact snapshot is empty")
	}
	if snapshot.Metadata.Soroq.AppID == "" {
		return fmt.Errorf("release artifact %s is missing bundled soroq.app_id metadata", snapshot.Artifact.Path)
	}
	if snapshot.Metadata.Soroq.AppID != release.AppID {
		return fmt.Errorf("release artifact app_id %q does not match release app_id %q", snapshot.Metadata.Soroq.AppID, release.AppID)
	}
	if snapshot.Metadata.Soroq.Channel != "" && snapshot.Metadata.Soroq.Channel != release.Channel {
		return fmt.Errorf("release artifact channel %q does not match release channel %q", snapshot.Metadata.Soroq.Channel, release.Channel)
	}
	if strings.TrimSpace(snapshot.Metadata.Soroq.RuntimeID) == "" {
		return fmt.Errorf("release artifact %s is missing bundled soroq.runtime_id metadata", snapshot.Artifact.Path)
	}
	if snapshot.Metadata.Soroq.RuntimeID != release.RuntimeID {
		return fmt.Errorf("release artifact runtime_id %q does not match release runtime_id %q", snapshot.Metadata.Soroq.RuntimeID, release.RuntimeID)
	}
	return nil
}

func postRuntimeJSONDecode[T any](requestURL string, payload any) (T, error) {
	var zero T

	body, err := json.Marshal(payload)
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequest(http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(respBody))
		if message == "" {
			message = resp.Status
		}
		return zero, fmt.Errorf("runtime request failed: %s", message)
	}

	var out T
	if err := json.Unmarshal(respBody, &out); err != nil {
		return zero, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

func downloadURLToFile(rawURL string, targetPath string) error {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return fmt.Errorf("download %s failed: %s", rawURL, message)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	tmpPath := targetPath + ".tmp"
	if err := os.WriteFile(tmpPath, body, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

type previewInstallOptions struct {
	ArtifactPath   string
	ArtifactType   string
	ReleaseID      string
	OutputDir      string
	ProjectDir     string
	DeviceID       string
	ADBPath        string
	BundletoolPath string
}

func installPreviewArtifact(options previewInstallOptions) error {
	switch options.ArtifactType {
	case "apk":
		return runADBCommand(options.ADBPath, options.DeviceID, "install", "-r", options.ArtifactPath)
	case "aab":
		apksPath := previewAPKSPath(options.ProjectDir, options.OutputDir, options.ReleaseID)
		if err := os.MkdirAll(filepath.Dir(apksPath), 0o755); err != nil {
			return err
		}
		_ = os.Remove(apksPath)
		if err := runBundletoolCommand(
			options.BundletoolPath,
			"build-apks",
			"--bundle="+options.ArtifactPath,
			"--output="+apksPath,
			"--mode=universal",
		); err != nil {
			return err
		}
		installArgs := []string{
			"install-apks",
			"--apks=" + apksPath,
		}
		if strings.TrimSpace(options.DeviceID) != "" {
			installArgs = append(installArgs, "--device-id="+strings.TrimSpace(options.DeviceID))
		}
		return runBundletoolCommand(options.BundletoolPath, installArgs...)
	default:
		return fmt.Errorf("device preview does not support Android artifact type %q", options.ArtifactType)
	}
}

func previewAPKSPath(projectDir string, outputDir string, releaseID string) string {
	resolvedOutputDir := strings.TrimSpace(outputDir)
	if resolvedOutputDir == "" {
		resolvedOutputDir = filepath.Join(projectDir, ".soroq", "previews", releaseID)
	}
	if !filepath.IsAbs(resolvedOutputDir) {
		resolvedOutputDir = filepath.Join(projectDir, resolvedOutputDir)
	}
	return filepath.Join(resolvedOutputDir, safePreviewFileName(releaseID)+".apks")
}

func runBundletoolCommand(bundletoolPath string, args ...string) error {
	resolvedBundletoolPath := strings.TrimSpace(bundletoolPath)
	if resolvedBundletoolPath == "" {
		resolvedBundletoolPath = "bundletool"
	}
	var commandName string
	var commandArgs []string
	if strings.HasSuffix(strings.ToLower(resolvedBundletoolPath), ".jar") {
		javaPath, err := exec.LookPath("java")
		if err != nil {
			return errors.New("java executable not found; install Java or pass a bundletool executable via --bundletool")
		}
		commandName = javaPath
		commandArgs = append([]string{"-jar", resolvedBundletoolPath}, args...)
	} else {
		if !strings.ContainsRune(resolvedBundletoolPath, filepath.Separator) {
			lookedUp, err := exec.LookPath(resolvedBundletoolPath)
			if err != nil {
				return fmt.Errorf("bundletool executable %q not found; install bundletool, pass --bundletool, or register an APK release for device preview", resolvedBundletoolPath)
			}
			resolvedBundletoolPath = lookedUp
		}
		commandName = resolvedBundletoolPath
		commandArgs = args
	}
	cmd := exec.Command(commandName, commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("bundletool %s failed: %s", strings.Join(args, " "), message)
	}
	return nil
}

func runADBCommand(adbPath string, deviceID string, args ...string) error {
	resolvedADBPath := strings.TrimSpace(adbPath)
	if resolvedADBPath == "" {
		resolvedADBPath = "adb"
	}
	if !strings.ContainsRune(resolvedADBPath, filepath.Separator) {
		lookedUp, err := exec.LookPath(resolvedADBPath)
		if err != nil {
			return fmt.Errorf("adb executable %q not found; install Android platform-tools or pass --adb", resolvedADBPath)
		}
		resolvedADBPath = lookedUp
	}
	commandArgs := make([]string, 0, len(args)+2)
	if strings.TrimSpace(deviceID) != "" {
		commandArgs = append(commandArgs, "-s", strings.TrimSpace(deviceID))
	}
	commandArgs = append(commandArgs, args...)
	cmd := exec.Command(resolvedADBPath, commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("adb %s failed: %s", strings.Join(commandArgs, " "), message)
	}
	return nil
}

func safePreviewFileName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "patch"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "patch"
	}
	return out
}
