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
	"strings"
	"time"
	"unicode"

	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/domain"
)

type releaseAndroidSummary struct {
	ProjectDir string                      `json:"project_dir"`
	Snapshot   *androidrelease.Snapshot    `json:"snapshot"`
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
	case "list":
		return runReleaseList(args[1:])
	case "status":
		return runReleaseStatus(args[1:])
	case "-h", "--help", "help":
		releaseUsage()
		return nil
	default:
		printUnknownSubcommand(os.Stderr, "release", args[0], []string{"android", "list", "status"})
		return errAlreadyPrinted
	}
}

func releaseUsage() {
	printCommandUsage(os.Stdout,
		"Soroq Releases",
		"Register Android baselines with the hosted control plane.",
		"soroq release <platform> [flags]",
		[]usageSection{{
			Title: "Platforms",
			Rows: []usageRow{
				{Name: "android", Description: "Register a built Android APK/AAB as a Soroq release."},
				{Name: "list", Description: "List registered releases in the control plane."},
				{Name: "status", Description: "Inspect a registered release in the control plane."},
			},
		}},
		[]string{
			"soroq release android --artifact build/app/outputs/bundle/release/app-release.aab",
			"soroq release list --app-id com.example.app",
		},
	)
}

func runReleaseList(args []string) error {
	fs := flag.NewFlagSet("release list", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	appID := fs.String("app-id", "", "optional app id filter")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq release list [--api https://soroq-control-plane.fly.dev] [--app-id com.example.app] [--json]`)
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
		fmt.Fprintln(os.Stdout, `usage: soroq release status --release-id release-123 [--api https://soroq-control-plane.fly.dev] [--json]`)
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
	releaseID := fs.String("release-id", "", "release id override")
	version := fs.String("version", "", "release version override")
	arch := fs.String("arch", "", "ABI override when the artifact contains multiple ABIs")
	channel := fs.String("channel", "", "channel override (defaults to soroq.yaml)")
	manifestKeyID := fs.String("manifest-key-id", "", "optional manifest signing key id for this release")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq release android [--artifact build/app/outputs/bundle/release/app-release.aab] [--build=false] [--artifact-type aab|apk] [--project-dir .] [--api https://soroq-control-plane.fly.dev] [--release-id my-release] [--version 1.2.3+45] [--arch arm64-v8a] [--channel stable] [--manifest-key-id prod-primary] [--json] [-- <flutter build flags>]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
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
	if resolvedArtifactPath == "" && *buildBeforeDiscover {
		if err := runFlutterAndroidReleaseBuild(status.ProjectDir, *buildArtifactType, flutterBuildArgs); err != nil {
			return err
		}
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

	release, err := postJSONDecode[domain.Release](strings.TrimRight(*apiBase, "/")+"/v1/releases", req)
	if err != nil {
		if strings.Contains(err.Error(), "unknown app") {
			return addAppCreateHint(err, projectConfig.AppID)
		}
		existing, statusErr := getJSONDecode[domain.Release](strings.TrimRight(*apiBase, "/") + "/v1/releases/" + url.PathEscape(resolvedReleaseID))
		if statusErr != nil || !releaseMatchesRequest(existing, req) {
			return addAppCreateHint(err, projectConfig.AppID)
		}
		release = existing
	}
	if err := rememberAndroidRelease(status.ProjectDir, *apiBase, snapshot, release, resolvedManifestKeyID); err != nil {
		return err
	}

	summary := releaseAndroidSummary{
		ProjectDir: status.ProjectDir,
		Snapshot:   snapshot,
		Request:    req,
		Response:   release,
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
	fmt.Fprintf(os.Stdout, "bundled metadata: %s\n", snapshot.Artifact.BundledMetadataZipPath)
	return nil
}

func inspectAndroidArtifact(artifactPath string) (*androidrelease.Snapshot, error) {
	return androidrelease.CaptureSnapshot(artifactPath)
}

func resolveReleaseVersion(metadata androidrelease.BundledMetadata, override string) (string, error) {
	override = strings.TrimSpace(override)
	if override != "" {
		return override, nil
	}
	if metadata.App.Version != nil && strings.TrimSpace(*metadata.App.Version) != "" {
		return strings.TrimSpace(*metadata.App.Version), nil
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
		return buildName + "+" + buildNumber, nil
	case buildName != "":
		return buildName, nil
	default:
		return "", errors.New("release version could not be inferred from bundled metadata; pass --version explicitly")
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
		return "", fmt.Errorf("artifact contains multiple ABIs (%s); pass --arch explicitly", strings.Join(abis, ", "))
	}
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
	creds, err := currentOperatorCredentialsForRequest("")
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
