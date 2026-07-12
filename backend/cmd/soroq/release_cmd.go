package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/domain"
)

// androidReleaseBuildFn indirects runFlutterAndroidReleaseBuild (defined in project_state.go) so tests
// can stub the SOROQ build step and exercise the soroq.lock pin-write path without a real Flutter build.
var androidReleaseBuildFn = runFlutterAndroidReleaseBuild

type releaseAndroidSummary struct {
	ProjectDir      string                      `json:"project_dir"`
	Snapshot        *androidrelease.Snapshot    `json:"snapshot"`
	Request         domain.CreateReleaseRequest `json:"request"`
	Response        domain.Release              `json:"response"`
	ReleaseArtifact *domain.ReleaseArtifact     `json:"release_artifact,omitempty"`
}

type releaseIOSSummary struct {
	ProjectDir string                      `json:"project_dir"`
	Request    domain.CreateReleaseRequest `json:"request"`
	Response   domain.Release              `json:"response"`
}

type releaseListSummary struct {
	Count    int              `json:"count"`
	AppID    string           `json:"app_id,omitempty"`
	Releases []domain.Release `json:"releases"`
}

func runRelease(args []string) error {
	if len(args) == 0 {
		releaseUsage()
		return errAlreadyPrinted
	}

	switch args[0] {
	case "android":
		return runReleaseAndroid(args[1:])
	case "ios":
		// `release ios --build --toolchain <ios-r3>` = build the app + app.dill (config-lane
		// leg); `release ios --engine ...` = register the ENGINE-lane baseline (delegates to
		// soroqctl). Only --engine routes to the delegate — --toolchain belongs to --build.
		// `release ios --engine --build` = UNIFIED fresh-dev path: generate scaffold + build
		// app.dill + register baseline in one command.
		if releaseIOSEngineRequested(args[1:]) {
			if hasFlag(args[1:], "build") {
				return runReleaseIOSEngineBuild(args[1:])
			}
			return runEngineLaneDelegate("release", stripEngineRoutingFlag(args[1:]))
		}
		return runReleaseIOS(args[1:])
	case "ios-engine":
		return runEngineLaneDelegate("release", args[1:])
	case "list":
		return runReleaseList(args[1:])
	case "status":
		return runReleaseStatus(args[1:])
	case "-h", "--help", "help":
		releaseUsage()
		return nil
	default:
		releaseUsage()
		return errAlreadyPrinted
	}
}

func releaseUsage() {
	fmt.Fprintln(os.Stdout, `usage: soroq release <platform> [flags]

platforms:
  android  register a built Android APK/AAB as a Soroq release
  ios      register an App Store/TestFlight iOS baseline for config OTA
  ios-engine  register an experimental engine-lane baseline (Dart-code OTA; delegates to soroqctl)
  list     list registered releases in the control plane
  status   inspect a registered release in the control plane`)
}

func runReleaseList(args []string) error {
	fs := flag.NewFlagSet("release list", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	appID := fs.String("app-id", "", "optional app id filter")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq release list [--api https://api.soroq.dev] [--app-id com.example.app] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	query := url.Values{}
	resolvedAppID := strings.TrimSpace(*appID)
	if resolvedAppID != "" {
		if !looksLikeSoroqAppID(resolvedAppID) {
			return fmt.Errorf("--app-id %q should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens", resolvedAppID)
		}
		query.Set("app_id", resolvedAppID)
	}
	listURL := strings.TrimRight(*apiBase, "/") + "/v1/releases"
	if encodedQuery := query.Encode(); encodedQuery != "" {
		listURL += "?" + encodedQuery
	}
	releases, err := getJSONDecode[[]domain.Release](listURL)
	if err != nil {
		return err
	}

	summary := releaseListSummary{
		Count:    len(releases),
		AppID:    resolvedAppID,
		Releases: releases,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Soroq releases: %d\n", len(releases))
	for _, release := range releases {
		fmt.Fprintf(os.Stdout, "- %s\t%s\t%s\t%s\t%s\n", release.ID, release.AppID, release.Version, release.Channel, release.Arch)
	}
	return nil
}

func runReleaseStatus(args []string) error {
	fs := flag.NewFlagSet("release status", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	releaseID := fs.String("release-id", "", "release id to inspect")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq release status --release-id release-123 [--api https://api.soroq.dev] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	resolvedReleaseID := strings.TrimSpace(*releaseID)
	if resolvedReleaseID == "" {
		return errors.New("--release-id is required")
	}

	release, err := getJSONDecode[domain.Release](strings.TrimRight(*apiBase, "/") + "/v1/releases/" + url.PathEscape(resolvedReleaseID))
	if err != nil {
		return err
	}

	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(release)
	}

	fmt.Fprintf(os.Stdout, "Soroq release %s\n", release.ID)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", release.AppID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", release.RuntimeID)
	fmt.Fprintf(os.Stdout, "version: %s\n", release.Version)
	fmt.Fprintf(os.Stdout, "platform: %s\n", release.Platform)
	fmt.Fprintf(os.Stdout, "arch: %s\n", release.Arch)
	fmt.Fprintf(os.Stdout, "channel: %s\n", release.Channel)
	return nil
}

func runReleaseAndroid(args []string) error {
	fs := flag.NewFlagSet("release android", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	artifactPath := fs.String("artifact", "", "path to Android APK or AAB")
	buildBeforeDiscover := fs.Bool("build", true, "run flutter build before discovering the Android artifact when --artifact is omitted")
	buildArtifactType := fs.String("artifact-type", "aab", "artifact type to build when --artifact is omitted: aab or apk")
	toolchainVersion := fs.String("toolchain", "", "resolve the Android engine from the cached toolchain at ~/.soroq/toolchains/<version>/android (installed by `soroq toolchain install`); replaces the local repo engine checkout. Consistent with `release ios-engine --toolchain`.")
	releaseID := fs.String("release-id", "", "release id override")
	version := fs.String("version", "", "release version override")
	arch := fs.String("arch", "", "ABI override when the artifact contains multiple ABIs")
	channel := fs.String("channel", "", "channel override (defaults to soroq.yaml)")
	manifestKeyID := fs.String("manifest-key-id", "", "optional manifest signing key id for this release")
	uploadArtifact := fs.Bool("upload-artifact", true, "upload the APK/AAB to the control plane so future patch commands can run from hosted release state")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	verbose := fs.Bool("verbose", false, "stream raw flutter build output (default: quiet; summarized + logged to .soroq/logs)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq release android [--artifact build/app/outputs/bundle/release/app-release.aab] [--build=false] [--artifact-type aab|apk] [--toolchain <version>] [--project-dir .] [--api https://api.soroq.dev] [--release-id my-release] [--version 1.2.3+45] [--arch arm64-v8a] [--channel stable] [--manifest-key-id prod-primary] [--upload-artifact=true] [--json] [--verbose] [-- <flutter build flags>]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *verbose {
		cliVerboseRequested = true
	}
	flutterBuildArgs := fs.Args()

	status, err := inspectProject(*projectDir)
	if err != nil {
		return err
	}
	if err := validateProjectIdentity(status); err != nil {
		return err
	}
	resolvedArtifactPath := strings.TrimSpace(*artifactPath)
	if len(flutterBuildArgs) > 0 && (resolvedArtifactPath != "" || !*buildBeforeDiscover) {
		return errors.New("Flutter build passthrough args require automatic build; omit --artifact and keep --build=true")
	}
	// P3: when --toolchain is absent, default it from `soroq setup`'s active.json (the setup->release
	// link — this is the first reader of active.json). If neither the flag nor active.json provides one,
	// keep the current behavior (empty -> resolveAndroidEngineSource decides or blocks; don't invent).
	resolvedToolchainVersion := strings.TrimSpace(*toolchainVersion)
	resolvedFrontendVersion := ""
	if resolvedToolchainVersion == "" {
		if active, activeErr := loadActiveToolchains(); activeErr == nil {
			if entry, ok := active.Platforms["android"]; ok {
				resolvedToolchainVersion = strings.TrimSpace(entry.ToolchainVersion)
				resolvedFrontendVersion = strings.TrimSpace(entry.FrontendVersion)
			}
		}
	}
	// soroqBuilt is true ONLY when SOROQ ran the build below. On the --artifact bypass (soroq did NOT
	// build and resolves no toolchain) it stays false, so NO soroq.lock pin is written for that release.
	soroqBuilt := false
	if resolvedArtifactPath == "" && *buildBeforeDiscover {
		if err := androidReleaseBuildFn(status.ProjectDir, *buildArtifactType, resolvedToolchainVersion, flutterBuildArgs); err != nil {
			return err
		}
		soroqBuilt = true
	}
	if resolvedArtifactPath == "" {
		resolvedArtifactPath, err = discoverDefaultAndroidArtifact(status.ProjectDir)
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no Android release artifact found; run `soroq release android` with a working Flutter toolchain or pass --artifact")
		}
		if err != nil {
			return err
		}
	}

	snapshot, err := inspectAndroidArtifact(resolvedArtifactPath)
	if err != nil {
		return err
	}
	channelOverride := *channel
	if !flagWasSet(fs, "channel") && strings.TrimSpace(snapshot.Metadata.Soroq.Channel) != "" {
		channelOverride = snapshot.Metadata.Soroq.Channel
	}
	projectConfig, err := resolveProjectCommandConfig(status, channelOverride)
	if err != nil {
		return err
	}
	if snapshot.Metadata.Soroq.AppID == "" {
		return fmt.Errorf("artifact %s is missing bundled soroq.app_id metadata", snapshot.Artifact.Path)
	}
	if snapshot.Metadata.Soroq.AppID != projectConfig.AppID {
		return fmt.Errorf("artifact app_id %q does not match soroq.yaml app_id %q", snapshot.Metadata.Soroq.AppID, projectConfig.AppID)
	}
	if snapshot.Metadata.Soroq.Channel != "" && snapshot.Metadata.Soroq.Channel != projectConfig.Channel {
		return fmt.Errorf("artifact channel %q does not match requested channel %q", snapshot.Metadata.Soroq.Channel, projectConfig.Channel)
	}
	if strings.TrimSpace(snapshot.Metadata.Soroq.RuntimeID) == "" {
		return fmt.Errorf("artifact %s is missing bundled soroq.runtime_id metadata", snapshot.Artifact.Path)
	}

	resolvedVersion, err := resolveReleaseVersion(snapshot.Metadata, *version)
	if err != nil {
		return err
	}
	resolvedArch, err := resolveReleaseArchForArtifact(snapshot.Artifact.Type, androidrelease.DeriveABIs(snapshot), *arch)
	if err != nil {
		return err
	}
	resolvedReleaseID := strings.TrimSpace(*releaseID)
	if resolvedReleaseID == "" {
		resolvedReleaseID = defaultReleaseID(projectConfig.AppID, resolvedVersion, resolvedArch)
	}
	resolvedManifestKeyID := strings.TrimSpace(*manifestKeyID)
	if resolvedManifestKeyID == "" {
		resolvedManifestKeyID = firstManifestSigningKeyID(snapshot.Metadata)
	}

	req := domain.CreateReleaseRequest{
		ID:                   resolvedReleaseID,
		AppID:                projectConfig.AppID,
		RuntimeID:            strings.TrimSpace(snapshot.Metadata.Soroq.RuntimeID),
		Version:              resolvedVersion,
		Platform:             "android",
		Arch:                 resolvedArch,
		Channel:              projectConfig.Channel,
		ManifestSigningKeyID: resolvedManifestKeyID,
	}

	release, err := createRelease(*apiBase, req, projectConfig.AppID)
	if err != nil {
		return err
	}
	var releaseArtifact *domain.ReleaseArtifact
	if *uploadArtifact {
		artifact, err := uploadReleaseArtifact(strings.TrimRight(*apiBase, "/"), release.ID, snapshot.Artifact.Path)
		if err != nil {
			return err
		}
		releaseArtifact = &artifact
	}
	if err := rememberAndroidRelease(status.ProjectDir, *apiBase, snapshot, release, resolvedManifestKeyID); err != nil {
		return err
	}
	// P3: pin the toolchain that built this release into the committed project-root soroq.lock, but
	// ONLY when SOROQ ran the build. On the --artifact bypass (soroqBuilt == false) soroq did not
	// resolve a toolchain, so we write no pin — an honest absent pin lets `soroq patch` fall back.
	if soroqBuilt {
		if err := recordSoroqLockPin(status.ProjectDir, "android", soroqLockPin{
			ReleaseID:        release.ID,
			Version:          release.Version,
			ToolchainVersion: resolvedToolchainVersion,
			FrontendVersion:  resolvedFrontendVersion,
			RecordedAt:       time.Now().UTC(),
		}); err != nil {
			return err
		}
	}
	releaseArtifactPath := projectReleaseArtifactPath(status.ProjectDir, release.ID, snapshot.Artifact.Path)

	summary := releaseAndroidSummary{
		ProjectDir:      status.ProjectDir,
		Snapshot:        snapshot,
		Request:         req,
		Response:        release,
		ReleaseArtifact: releaseArtifact,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Registered Android release %s\n", release.ID)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", release.AppID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", release.RuntimeID)
	fmt.Fprintf(os.Stdout, "version: %s\n", release.Version)
	fmt.Fprintf(os.Stdout, "channel: %s\n", release.Channel)
	fmt.Fprintf(os.Stdout, "arch: %s\n", release.Arch)
	fmt.Fprintf(os.Stdout, "artifact: %s\n", snapshot.Artifact.Path)
	fmt.Fprintf(os.Stdout, "release_artifact: %s\n", releaseArtifactPath)
	if releaseArtifact != nil {
		fmt.Fprintf(os.Stdout, "uploaded_artifact_bytes: %d\n", releaseArtifact.SizeBytes)
		fmt.Fprintf(os.Stdout, "uploaded_artifact_sha256: %s\n", releaseArtifact.SHA256)
	}
	fmt.Fprintf(os.Stdout, "bundled metadata: %s\n", snapshot.Artifact.BundledMetadataZipPath)
	fmt.Fprintf(os.Stdout, "next: send release_artifact to testers or upload it to Play Store; after Dart changes run `soroq patch android --artifact-type %s`.\n", androidArtifactTypeForCommand(snapshot.Artifact.Path))
	return nil
}

func runReleaseIOS(args []string) error {
	fs := flag.NewFlagSet("release ios", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	releaseID := fs.String("release-id", "", "release id override")
	version := fs.String("version", "", "App Store/TestFlight version, such as 1.2.3+45")
	runtimeID := fs.String("runtime-id", "", "iOS runtime compatibility id for this shipped baseline")
	arch := fs.String("arch", "arm64", "iOS architecture label for the shipped baseline")
	channel := fs.String("channel", "", "channel override (defaults to soroq.yaml)")
	manifestKeyID := fs.String("manifest-key-id", "", "optional manifest signing key id for this release")
	build := fs.Bool("build", false, "build the iOS app (.app + app.dill) from the cached Soroq toolchain via flutter build ios --local-engine before/without config registration; requires --toolchain. Analog of `release android --build`.")
	toolchainVersion := fs.String("toolchain", "", "resolve the iOS engine from the cached toolchain at ~/.soroq/toolchains/<version>/ios (installed by `soroq toolchain install`); required with --build. Consistent with `patch ios-engine --toolchain`.")
	verbose := fs.Bool("verbose", false, "stream raw flutter build output (default: quiet; summarized + logged to .soroq/logs)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq release ios [--project-dir .] [--api https://api.soroq.dev] [--release-id my-ios-release] [--version 1.2.3+45] [--runtime-id ios-config-runtime] [--arch arm64] [--channel stable] [--manifest-key-id prod-primary] [--build --toolchain <version>] [--verbose] [--json] [-- <flutter build flags>]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *verbose {
		cliVerboseRequested = true
	}
	flutterBuildArgs := fs.Args()

	status, err := inspectProject(*projectDir)
	if err != nil {
		return err
	}

	// Build leg (T030): build the iOS app from the HOSTED Soroq toolchain (no engine source checkout) and
	// emit app.dill for the ios-engine patch lane. Decoupled from the config-lane control-plane
	// registration below so a fresh dev can produce app.dill without a control-plane round-trip.
	if *build {
		return runFlutterIOSReleaseBuild(status.ProjectDir, strings.TrimSpace(*toolchainVersion), flutterBuildArgs)
	}
	if len(flutterBuildArgs) > 0 {
		return errors.New("release ios does not build or upload an IPA without --build; remove passthrough build arguments or pass --build --toolchain <version>")
	}
	projectConfig, err := resolveIOSReleaseProjectConfig(status, *channel)
	if err != nil {
		return err
	}

	resolvedVersion := strings.TrimSpace(*version)
	if resolvedVersion == "" {
		resolvedVersion, err = inferFlutterProjectVersion(status.PubspecPath)
		if err != nil {
			return fmt.Errorf("could not infer iOS release version from pubspec.yaml; pass --version: %w", err)
		}
	}
	resolvedRuntimeID := strings.TrimSpace(*runtimeID)
	if resolvedRuntimeID == "" {
		resolvedRuntimeID = defaultIOSConfigRuntimeID(projectConfig.AppID, projectConfig.Channel)
	}
	resolvedArch := strings.TrimSpace(*arch)
	if resolvedArch == "" {
		return errors.New("--arch must not be empty")
	}
	resolvedReleaseID := strings.TrimSpace(*releaseID)
	if resolvedReleaseID == "" {
		resolvedReleaseID = defaultReleaseID(projectConfig.AppID, resolvedVersion, "ios-"+resolvedArch)
	}

	req := domain.CreateReleaseRequest{
		ID:                   resolvedReleaseID,
		AppID:                projectConfig.AppID,
		RuntimeID:            resolvedRuntimeID,
		Version:              resolvedVersion,
		Platform:             "ios",
		Arch:                 resolvedArch,
		Channel:              projectConfig.Channel,
		ManifestSigningKeyID: strings.TrimSpace(*manifestKeyID),
	}

	release, err := createRelease(*apiBase, req, projectConfig.AppID)
	if err != nil {
		return err
	}
	if err := rememberIOSRelease(status.ProjectDir, *apiBase, release, strings.TrimSpace(*manifestKeyID)); err != nil {
		return err
	}

	summary := releaseIOSSummary{
		ProjectDir: status.ProjectDir,
		Request:    req,
		Response:   release,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Registered iOS config baseline %s\n", release.ID)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", release.AppID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", release.RuntimeID)
	fmt.Fprintf(os.Stdout, "version: %s\n", release.Version)
	fmt.Fprintf(os.Stdout, "channel: %s\n", release.Channel)
	fmt.Fprintf(os.Stdout, "arch: %s\n", release.Arch)
	fmt.Fprintln(os.Stdout, "ios_support: config_ota_only")
	fmt.Fprintf(os.Stdout, "submit: ship this baseline's IPA (version %s) to TestFlight/App Store — build `flutter build ipa`, then upload via Xcode Organizer, Transporter, or `asc builds upload`. Reviewers run the signed bundled baseline; config patches ride OTA on top.\n", release.Version)
	fmt.Fprintf(os.Stdout, "next: publish signed JSON config with `soroq patch ios --config-file config.json`.\n")
	fmt.Fprintf(os.Stdout, "explicit: `soroq patch config --release-id %s --config-file config.json` also works.\n", release.ID)
	fmt.Fprintln(os.Stdout, "note: this does not enable iOS Dart-code OTA, native code patches, dylib downloads, or JIT.")
	return nil
}

func inferFlutterProjectVersion(pubspecPath string) (string, error) {
	bytes, err := os.ReadFile(pubspecPath)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(parseTopLevelYaml(bytes)["version"])
	if version == "" {
		return "", errors.New("top-level version is missing")
	}
	return version, nil
}

func defaultIOSConfigRuntimeID(appID string, channel string) string {
	return slugifyReleaseID("ios-config-v1-" + strings.TrimSpace(appID) + "-" + strings.TrimSpace(channel))
}

func rememberIOSRelease(projectDir string, apiBase string, release domain.Release, manifestKeyID string) error {
	state, err := loadProjectCLIState(projectDir)
	if err != nil {
		return err
	}
	state.LastIOSRelease = &iosReleaseState{
		UpdatedAt:            time.Now().UTC(),
		APIBase:              strings.TrimRight(apiBase, "/"),
		AppID:                release.AppID,
		Channel:              release.Channel,
		ReleaseID:            release.ID,
		RuntimeID:            release.RuntimeID,
		Version:              release.Version,
		Arch:                 release.Arch,
		ManifestSigningKeyID: manifestKeyID,
	}
	return saveProjectCLIState(projectDir, state)
}

func resolveIOSReleaseProjectConfig(status projectStatus, channelOverride string) (projectCommandConfig, error) {
	if !status.HasPubspec {
		return projectCommandConfig{}, fmt.Errorf("pubspec.yaml not found in %s", status.ProjectDir)
	}
	if !status.HasSoroqConfig {
		return projectCommandConfig{}, fmt.Errorf("soroq.yaml not found in %s; run `soroq init` first", status.ProjectDir)
	}
	if strings.TrimSpace(status.AppID) == "" {
		return projectCommandConfig{}, fmt.Errorf("soroq.yaml at %s is missing app_id", status.SoroqConfigPath)
	}
	if !status.AppIDLooksValid {
		return projectCommandConfig{}, fmt.Errorf("soroq.yaml app_id %q should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens", status.AppID)
	}
	if status.RuntimeIDStrategy != "manifest_trust_v1" || !status.HasManifestTrust {
		return projectCommandConfig{}, fmt.Errorf("soroq.yaml at %s is missing hosted manifest trust; run `soroq init --force` to refresh it", status.SoroqConfigPath)
	}

	resolvedChannel := strings.TrimSpace(channelOverride)
	if resolvedChannel == "" {
		resolvedChannel = strings.TrimSpace(status.Channel)
	}
	if resolvedChannel == "" {
		return projectCommandConfig{}, fmt.Errorf("soroq.yaml at %s is missing channel", status.SoroqConfigPath)
	}
	if !looksLikeChannel(resolvedChannel) {
		return projectCommandConfig{}, fmt.Errorf("channel %q should be a stable slug such as stable, beta, or production", resolvedChannel)
	}

	return projectCommandConfig{
		AppID:   status.AppID,
		Channel: resolvedChannel,
	}, nil
}

func inspectAndroidArtifact(artifactPath string) (*androidrelease.Snapshot, error) {
	return androidrelease.CaptureSnapshot(artifactPath)
}

func resolveReleaseVersion(metadata androidrelease.BundledMetadata, override string) (string, error) {
	override = strings.TrimSpace(override)
	bundledVersion := bundledReleaseVersion(metadata)
	if override != "" {
		if bundledVersion != "" && override != bundledVersion {
			return "", fmt.Errorf("--version %q does not match bundled artifact version %q; update the app version and rebuild the artifact instead of overriding release metadata", override, bundledVersion)
		}
		return override, nil
	}
	if bundledVersion != "" {
		return bundledVersion, nil
	}

	return "", errors.New("release version could not be inferred from bundled metadata; pass --version explicitly")
}

func bundledReleaseVersion(metadata androidrelease.BundledMetadata) string {
	if metadata.App.Version != nil && strings.TrimSpace(*metadata.App.Version) != "" {
		return strings.TrimSpace(*metadata.App.Version)
	}

	buildName := ""
	if metadata.App.BuildName != nil {
		buildName = strings.TrimSpace(*metadata.App.BuildName)
	}
	buildNumber := ""
	if metadata.App.BuildNumber != nil {
		buildNumber = strings.TrimSpace(*metadata.App.BuildNumber)
	}
	switch {
	case buildName != "" && buildNumber != "":
		return buildName + "+" + buildNumber
	case buildName != "":
		return buildName
	default:
		return ""
	}
}

func resolveReleaseArch(abis []string, override string) (string, error) {
	return resolveReleaseArchForArtifact("", abis, override)
}

func resolveReleaseArchForArtifact(artifactType string, abis []string, override string) (string, error) {
	override = strings.TrimSpace(override)
	if override != "" {
		if len(abis) > 0 {
			for _, abi := range abis {
				if abi == override {
					return override, nil
				}
			}
			return "", fmt.Errorf("requested --arch %q is not present in artifact ABIs %s", override, strings.Join(abis, ", "))
		}
		return override, nil
	}
	switch len(abis) {
	case 0:
		return "", errors.New("artifact ABI could not be inferred; pass --arch explicitly")
	case 1:
		return abis[0], nil
	default:
		if artifactType == "aab" {
			return "universal", nil
		}
		return preferredAndroidABI(abis), nil
	}
}

func preferredAndroidABI(abis []string) string {
	for _, preferred := range []string{"arm64-v8a", "x86_64", "armeabi-v7a"} {
		for _, abi := range abis {
			if abi == preferred {
				return preferred
			}
		}
	}
	return abis[0]
}

func rememberAndroidRelease(projectDir string, apiBase string, snapshot *androidrelease.Snapshot, release domain.Release, manifestKeyID string) error {
	state, err := loadProjectCLIState(projectDir)
	if err != nil {
		return err
	}
	stashedArtifactPath, err := stashAndroidReleaseArtifact(projectDir, release.ID, snapshot.Artifact.Path)
	if err != nil {
		return err
	}
	state.LastAndroidRelease = &androidReleaseState{
		UpdatedAt:            time.Now().UTC(),
		APIBase:              strings.TrimRight(apiBase, "/"),
		AppID:                release.AppID,
		Channel:              release.Channel,
		ReleaseID:            release.ID,
		RuntimeID:            release.RuntimeID,
		Version:              release.Version,
		Arch:                 release.Arch,
		ArtifactPath:         stashedArtifactPath,
		ManifestSigningKeyID: manifestKeyID,
	}
	return saveProjectCLIState(projectDir, state)
}

// createRelease registers a release in the control plane, closing the fresh-user gap where
// `soroq release` fails because the app was never registered. When the control plane reports
// the EXISTING "unknown app" sentinel (store file_store.go: `unknown app %q`, surfaced as a 400)
// AND the operator is logged in, it auto-registers the app on the same create+bind path
// `soroq app create` uses (ownership is bound server-side from the operator credential), then
// retries the release create EXACTLY ONCE.
//
// Safety (consumes the server's already-correct auth/store semantics, changes none of it):
//   - A foreign-owned app returns requireOperatorApp 403 errOperatorForbidden, which does NOT
//     contain "unknown app": it flows to the else branch below and is surfaced verbatim by
//     addAppCreateHint (no auto-create, no misleading "soroq app create" hint).
//   - No login creds -> fail with a clear "run soroq login" and DO NOT attempt registration.
//   - Registration is attempted at most once; on its failure the error is surfaced verbatim with
//     no retry loop.
func createRelease(apiBase string, req domain.CreateReleaseRequest, appID string) (domain.Release, error) {
	apiBaseURL := strings.TrimRight(apiBase, "/")
	release, err := postJSONDecode[domain.Release](apiBaseURL+"/v1/releases", req)
	if err == nil {
		return release, nil
	}

	// Reuse the EXACT sentinel condition release already keyed on; do NOT broaden the match.
	if strings.Contains(err.Error(), "unknown app") {
		creds, credErr := currentOperatorCredentialsForRequest("", apiBaseURL)
		if credErr != nil {
			return domain.Release{}, credErr
		}
		if strings.TrimSpace(creds.Token) == "" {
			return domain.Release{}, fmt.Errorf("app %q is not registered and no operator is logged in; run `soroq login` first, then re-run this release to auto-register it", appID)
		}
		// Auto-register on the same create+bind path `soroq app create` uses. Ownership is bound
		// server-side from the operator credential; a foreign-owned id is rejected there, not here.
		if _, regErr := createSoroqApp(apiBaseURL, domain.CreateAppRequest{ID: appID, DisplayName: appID}); regErr != nil {
			// Registration failed (e.g. an ownership conflict or unauthenticated): surface it
			// clearly and do NOT retry-loop or emit the create hint.
			return domain.Release{}, regErr
		}
		// Retry the release create EXACTLY ONCE now that the app is registered.
		release, err = postJSONDecode[domain.Release](apiBaseURL+"/v1/releases", req)
		if err != nil {
			return domain.Release{}, err
		}
		return release, nil
	}

	// Non-"unknown app" failure: preserve the idempotent existing-release fallback. A 403
	// errOperatorForbidden lands here and is returned verbatim by addAppCreateHint (no hint).
	existing, statusErr := getJSONDecode[domain.Release](apiBaseURL + "/v1/releases/" + url.PathEscape(req.ID))
	if statusErr != nil || !releaseMatchesRequest(existing, req) {
		return domain.Release{}, addAppCreateHint(err, appID)
	}
	return existing, nil
}

func releaseMatchesRequest(release domain.Release, req domain.CreateReleaseRequest) bool {
	return release.ID == req.ID &&
		release.AppID == req.AppID &&
		release.RuntimeID == req.RuntimeID &&
		release.Version == req.Version &&
		release.Platform == req.Platform &&
		release.Arch == req.Arch &&
		release.Channel == req.Channel
}

func defaultReleaseID(appID, version, arch string) string {
	return slugifyReleaseID(fmt.Sprintf("%s-%s-%s", appID, version, arch))
}

func uploadReleaseArtifact(apiBase string, releaseID string, artifactPath string) (domain.ReleaseArtifact, error) {
	var zero domain.ReleaseArtifact
	file, err := os.Open(artifactPath)
	if err != nil {
		return zero, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return zero, err
	}

	artifactURL := strings.TrimRight(apiBase, "/") +
		"/v1/releases/" +
		url.PathEscape(releaseID) +
		"/artifact?filename=" +
		url.QueryEscape(filepath.Base(filepath.Clean(artifactPath)))
	req, err := http.NewRequest(http.MethodPost, artifactURL, file)
	if err != nil {
		return zero, err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", androidArtifactContentType(artifactPath))
	req.Header.Set("X-Soroq-Artifact-Filename", filepath.Base(filepath.Clean(artifactPath)))
	if err := applyOperatorHeaders(req); err != nil {
		return zero, err
	}

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
		return zero, fmt.Errorf("release artifact upload failed: %s", message)
	}
	var artifact domain.ReleaseArtifact
	if err := json.Unmarshal(respBody, &artifact); err != nil {
		return zero, fmt.Errorf("decode release artifact response: %w", err)
	}
	return artifact, nil
}

func androidArtifactContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".apk":
		return "application/vnd.android.package-archive"
	case ".aab":
		return "application/vnd.android.aab"
	default:
		return "application/octet-stream"
	}
}

func slugifyReleaseID(raw string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(raw) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevDash = false
		case r == '.' || r == '_' || r == '-' || unicode.IsSpace(r) || r == '+':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "release"
	}
	return slug
}

func postJSONDecode[T any](url string, payload any) (T, error) {
	var zero T

	body, err := json.Marshal(payload)
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := applyOperatorHeaders(req); err != nil {
		return zero, err
	}

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
		return zero, fmt.Errorf("request failed: %s", message)
	}

	var out T
	if err := json.Unmarshal(respBody, &out); err != nil {
		return zero, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

func postNoBodyDecode[T any](method string, url string) (T, error) {
	var zero T

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return zero, err
	}
	if err := applyOperatorHeaders(req); err != nil {
		return zero, err
	}
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
		return zero, fmt.Errorf("request failed: %s", message)
	}

	var out T
	if err := json.Unmarshal(respBody, &out); err != nil {
		return zero, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

func getJSONDecode[T any](url string) (T, error) {
	return postNoBodyDecode[T](http.MethodGet, url)
}

func applyOperatorHeaders(req *http.Request) error {
	// Scope the credential refresh to the host this request actually targets so a
	// local/non-prod --api request never refreshes or rewrites the stored prod
	// credential. req.URL already carries the resolved control-plane base.
	targetAPIBase := ""
	if req != nil && req.URL != nil {
		targetAPIBase = req.URL.Scheme + "://" + req.URL.Host
	}
	creds, err := currentOperatorCredentialsForRequest("", targetAPIBase)
	if err != nil {
		return err
	}
	applyCredentialsHeaders(req, creds)
	return nil
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
