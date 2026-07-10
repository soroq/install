package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"soroq/backend/internal/domain"
	"soroq/backend/internal/signing"
)

var (
	errAlreadyPrinted   = errors.New("message already printed")
	androidAppIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	soroqAppIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	channelSlugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

const soroqAndroidNDKVersion = "29.0.13599879"
const soroqAutoUpdateConfigAsset = "soroq/auto_update_config.json"

type projectStatus struct {
	ProjectDir                string   `json:"project_dir"`
	PubspecPath               string   `json:"pubspec_path"`
	SoroqConfigPath           string   `json:"soroq_config_path"`
	AutoUpdateConfigPath      string   `json:"auto_update_config_path"`
	HasPubspec                bool     `json:"has_pubspec"`
	HasSoroqConfig            bool     `json:"has_soroq_config"`
	HasSoroqFlutterDependency bool     `json:"has_soroq_flutter_dependency"`
	HasAutoUpdateConfig       bool     `json:"has_auto_update_config"`
	PubspecHasAutoUpdateAsset bool     `json:"pubspec_has_auto_update_asset"`
	HasManifestTrust          bool     `json:"has_manifest_trust"`
	HasAndroidManifest        bool     `json:"has_android_manifest"`
	HasAndroidInternet        bool     `json:"has_android_internet"`
	AndroidNDKVersion         string   `json:"android_ndk_version,omitempty"`
	AndroidNDKCompatible      bool     `json:"android_ndk_compatible"`
	AppID                     string   `json:"app_id,omitempty"`
	Channel                   string   `json:"channel,omitempty"`
	RuntimeIDStrategy         string   `json:"runtime_id_strategy,omitempty"`
	AutoUpdateBaseURL         string   `json:"auto_update_base_url,omitempty"`
	AutoUpdateTrack           string   `json:"auto_update_track,omitempty"`
	AppIDLooksValid           bool     `json:"app_id_looks_valid"`
	ChannelLooksValid         bool     `json:"channel_looks_valid"`
	ReleaseReady              bool     `json:"release_ready"`
	PatchReady                bool     `json:"patch_ready"`
	Ready                     bool     `json:"ready"`
	Warnings                  []string `json:"warnings,omitempty"`
}

type projectCommandConfig struct {
	AppID   string
	Channel string
}

type hostedManifestTrustResponse struct {
	RuntimeIDStrategy string                      `json:"runtime_id_strategy"`
	ManifestTrust     signing.ManifestTrustConfig `json:"manifest_trust"`
}

type androidProjectFixes struct {
	ManifestInternetUpdated bool `json:"manifest_internet_updated"`
	NDKVersionUpdated       bool `json:"ndk_version_updated"`
}

type autoUpdateConfigFile struct {
	BaseURL string `json:"base_url"`
	Track   string `json:"track"`
	Enabled bool   `json:"enabled"`
}

var runFlutterPubAddSoroqFlutter = func(projectDir string) error {
	flutterBin, err := exec.LookPath("flutter")
	if err != nil {
		return errors.New("flutter was not found on PATH; install Flutter first or run `flutter pub add soroq_flutter` manually before `soroq init`")
	}
	cmd := exec.Command(flutterBin, "pub", "add", "soroq_flutter")
	cmd.Dir = projectDir
	if soroqVerboseBuildOutput() {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("flutter pub add soroq_flutter failed: %w", err)
		}
		return nil
	}
	// Quiet by default: capture output and surface it only on failure (use --verbose for raw logs).
	output, err := cmd.CombinedOutput()
	if err != nil {
		if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
			fmt.Fprintln(os.Stderr, trimmed)
		}
		return fmt.Errorf("flutter pub add soroq_flutter failed: %w", err)
	}
	return nil
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	appID := fs.String("app-id", "", "application identifier to store in soroq.yaml (defaults to the Android applicationId)")
	displayName := fs.String("display-name", "", "display name to register in the hosted control plane")
	name := fs.String("name", "", "alias for --display-name")
	channel := fs.String("channel", "stable", "default rollout channel")
	createApp := fs.Bool("create-app", true, "register the app in the hosted control plane during init")
	ifNotExists := fs.Bool("if-not-exists", true, "succeed when the hosted app already exists")
	addDependency := fs.Bool("add-dependency", true, "add soroq_flutter to pubspec.yaml when it is missing")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	force := fs.Bool("force", false, "overwrite an existing soroq.yaml")
	verbose := fs.Bool("verbose", false, "stream raw flutter output (default: quiet; shown on failure)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq init [--app-id com.example.app] [--display-name "My App"] [--channel stable] [--project-dir .] [--api https://api.soroq.dev] [--create-app=false] [--if-not-exists=false] [--add-dependency=false] [--json] [--verbose] [--force]`)
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

	resolvedAppID := strings.TrimSpace(*appID)
	resolvedDisplayName := strings.TrimSpace(*displayName)
	if resolvedDisplayName == "" {
		resolvedDisplayName = strings.TrimSpace(*name)
	}
	resolvedChannel := strings.TrimSpace(*channel)
	if resolvedChannel == "" {
		return errors.New("--channel is required")
	}
	if !looksLikeChannel(resolvedChannel) {
		return fmt.Errorf("--channel %q should be a stable slug such as stable, beta, or production", resolvedChannel)
	}

	status, err := inspectProject(*projectDir)
	if err != nil {
		return err
	}
	if !status.HasPubspec {
		return fmt.Errorf("pubspec.yaml not found in %s", status.ProjectDir)
	}
	if status.HasSoroqConfig && !*force {
		return fmt.Errorf("soroq.yaml already exists at %s (rerun with --force to overwrite)", status.SoroqConfigPath)
	}

	shouldCreateApp := *createApp || resolvedDisplayName != ""
	if resolvedAppID == "" {
		var err error
		resolvedAppID, err = inferAndroidApplicationID(status.ProjectDir)
		if err != nil {
			resolvedAppID, err = inferIOSBundleIdentifier(status.ProjectDir)
			if err != nil {
				return fmt.Errorf("could not infer Android applicationId or iOS bundle identifier; pass --app-id explicitly: %w", err)
			}
		}
	}
	if !looksLikeSoroqAppID(resolvedAppID) {
		return fmt.Errorf("--app-id %q should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens", resolvedAppID)
	}
	if shouldCreateApp && resolvedDisplayName == "" {
		resolvedDisplayName = defaultDisplayNameFromPubspec(pubspecBytesOrNil(status.PubspecPath))
	}
	if shouldCreateApp && resolvedDisplayName == "" {
		return errors.New("--display-name is required when --create-app is used and pubspec.yaml has no top-level name")
	}
	trust, err := fetchHostedManifestTrust(strings.TrimRight(*apiBase, "/"))
	if err != nil {
		return err
	}

	var app *domain.App
	if shouldCreateApp {
		createdApp, err := createHostedApp(strings.TrimRight(*apiBase, "/"), resolvedAppID, resolvedDisplayName, *ifNotExists)
		if err != nil {
			return err
		}
		app = &createdApp
	}

	content := renderSoroqConfig(resolvedAppID, resolvedChannel, trust)
	if err := os.WriteFile(status.SoroqConfigPath, []byte(content), 0o644); err != nil {
		return err
	}
	// Guarantee a build-ready manifest_trust: validate the block just written (or auto-scaffold an
	// app-owned key if hosted trust yielded none), so a fresh init never dead-ends on the fork's
	// `Expected soroq.yaml to define "manifest_trust"` error. No-op when the hosted keys are valid.
	if _, err := ensureManifestTrust(status.ProjectDir); err != nil {
		return err
	}
	dependencyAdded := false
	if !status.HasSoroqFlutterDependency && *addDependency {
		if err := runFlutterPubAddSoroqFlutter(status.ProjectDir); err != nil {
			return err
		}
		dependencyAdded = true
		status.HasSoroqFlutterDependency = true
	}
	autoUpdateConfigWritten, err := ensureAutoUpdateConfig(status.ProjectDir, strings.TrimRight(*apiBase, "/"), *force)
	if err != nil {
		return err
	}
	pubspecUpdated, err := ensurePubspecContainsSoroqAssets(status.PubspecPath)
	if err != nil {
		return err
	}
	if err := generateSoroqBundledMetadata(status.ProjectDir); err != nil {
		return fmt.Errorf("generate Soroq bundled metadata: %w", err)
	}
	androidFixes, err := ensureAndroidProjectDefaults(status.ProjectDir)
	if err != nil {
		return err
	}

	if *jsonOut {
		summary := initSummary{
			ProjectDir:                  status.ProjectDir,
			SoroqConfigPath:             status.SoroqConfigPath,
			AppID:                       resolvedAppID,
			Channel:                     resolvedChannel,
			RuntimeIDStrategy:           trust.RuntimeIDStrategy,
			DependencyAdded:             dependencyAdded,
			AutoUpdateConfigPath:        filepath.Join(status.ProjectDir, soroqAutoUpdateConfigAsset),
			AutoUpdateBaseURL:           strings.TrimRight(*apiBase, "/"),
			AutoUpdateConfigWritten:     autoUpdateConfigWritten,
			PubspecUpdated:              pubspecUpdated,
			ManifestInternetUpdated:     androidFixes.ManifestInternetUpdated,
			AndroidNDKVersionUpdated:    androidFixes.NDKVersionUpdated,
			AndroidCompatibleNDKVersion: soroqAndroidNDKVersion,
			HostedApp:                   app,
			HostedAppCreated:            shouldCreateApp,
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Wrote %s\n", status.SoroqConfigPath)
	if dependencyAdded {
		fmt.Fprintln(os.Stdout, "Added soroq_flutter dependency to pubspec.yaml")
	}
	if autoUpdateConfigWritten {
		fmt.Fprintf(os.Stdout, "Wrote %s\n", filepath.Join(status.ProjectDir, soroqAutoUpdateConfigAsset))
	}
	if pubspecUpdated {
		fmt.Fprintf(os.Stdout, "Updated %s to include Soroq runtime assets\n", status.PubspecPath)
	}
	fmt.Fprintf(os.Stdout, "Wrote %s\n", filepath.Join(status.ProjectDir, filepath.FromSlash(soroqBundledMetadataAsset)))
	if androidFixes.ManifestInternetUpdated {
		fmt.Fprintln(os.Stdout, "Updated AndroidManifest.xml to include android.permission.INTERNET")
	}
	if androidFixes.NDKVersionUpdated {
		fmt.Fprintf(os.Stdout, "Updated Android Gradle ndkVersion to %s for Soroq runtime compatibility\n", soroqAndroidNDKVersion)
	}
	if app != nil {
		fmt.Fprintf(os.Stdout, "Registered Soroq app %s\n", app.ID)
		fmt.Fprintf(os.Stdout, "display_name: %s\n", app.DisplayName)
	}
	if !status.HasSoroqFlutterDependency {
		fmt.Fprintln(os.Stdout, "Next step: add soroq_flutter to your pubspec.yaml dependencies.")
	}
	fmt.Fprintln(os.Stdout, "Next step: run `soroq release android` or `soroq release ios`.")
	return nil
}

type initSummary struct {
	ProjectDir                  string      `json:"project_dir"`
	SoroqConfigPath             string      `json:"soroq_config_path"`
	AppID                       string      `json:"app_id"`
	Channel                     string      `json:"channel"`
	RuntimeIDStrategy           string      `json:"runtime_id_strategy"`
	DependencyAdded             bool        `json:"dependency_added"`
	AutoUpdateConfigPath        string      `json:"auto_update_config_path"`
	AutoUpdateBaseURL           string      `json:"auto_update_base_url"`
	AutoUpdateConfigWritten     bool        `json:"auto_update_config_written"`
	PubspecUpdated              bool        `json:"pubspec_updated"`
	ManifestInternetUpdated     bool        `json:"manifest_internet_updated"`
	AndroidNDKVersionUpdated    bool        `json:"android_ndk_version_updated"`
	AndroidCompatibleNDKVersion string      `json:"android_compatible_ndk_version"`
	HostedApp                   *domain.App `json:"hosted_app,omitempty"`
	HostedAppCreated            bool        `json:"hosted_app_created"`
}

func generateSoroqAppID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate app id: %w", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32]), nil
}

func pubspecBytesOrNil(path string) []byte {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return bytes
}

func readFirstExisting(paths ...string) ([]byte, error) {
	for _, path := range paths {
		bytes, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return bytes, err
	}
	return nil, os.ErrNotExist
}

func defaultDisplayNameFromPubspec(pubspecBytes []byte) string {
	name := strings.TrimSpace(parseTopLevelYaml(pubspecBytes)["name"])
	if name == "" {
		return ""
	}
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "-", " ")
	parts := strings.Fields(name)
	for index, part := range parts {
		if part == "" {
			continue
		}
		parts[index] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func createHostedApp(apiBase string, appID string, displayName string, ifNotExists bool) (domain.App, error) {
	req := domain.CreateAppRequest{
		ID:          appID,
		DisplayName: displayName,
	}
	app, err := postJSONDecode[domain.App](apiBase+"/v1/apps", req)
	if err == nil {
		return app, nil
	}
	if !ifNotExists {
		return domain.App{}, err
	}
	return getJSONDecode[domain.App](appURL(apiBase, appID))
}

func fetchHostedManifestTrust(apiBase string) (hostedManifestTrustResponse, error) {
	trust, err := getJSONDecode[hostedManifestTrustResponse](strings.TrimRight(apiBase, "/") + "/v1/manifest-trust")
	if err != nil {
		return hostedManifestTrustResponse{}, fmt.Errorf("fetch hosted manifest trust: %w", err)
	}
	if strings.TrimSpace(trust.RuntimeIDStrategy) == "" {
		trust.RuntimeIDStrategy = "manifest_trust_v1"
	}
	if trust.RuntimeIDStrategy != "manifest_trust_v1" {
		return hostedManifestTrustResponse{}, fmt.Errorf("hosted manifest trust returned unsupported runtime_id_strategy %q", trust.RuntimeIDStrategy)
	}
	if len(trust.ManifestTrust.Keys) == 0 {
		return hostedManifestTrustResponse{}, errors.New("hosted manifest trust did not include any public signing keys")
	}
	for _, key := range trust.ManifestTrust.Keys {
		if strings.TrimSpace(key.ID) == "" {
			return hostedManifestTrustResponse{}, errors.New("hosted manifest trust includes a key without id")
		}
		if strings.TrimSpace(key.PublicKey) == "" {
			return hostedManifestTrustResponse{}, fmt.Errorf("hosted manifest trust key %q has no public_key", key.ID)
		}
	}
	return trust, nil
}

func renderSoroqConfig(appID string, channel string, trust hostedManifestTrustResponse) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "app_id: %s\n", strings.TrimSpace(appID))
	fmt.Fprintf(&builder, "channel: %s\n", strings.TrimSpace(channel))
	fmt.Fprintf(&builder, "runtime_id_strategy: %s\n", strings.TrimSpace(trust.RuntimeIDStrategy))
	builder.WriteString("manifest_trust:\n")
	if trust.ManifestTrust.KeysetVersion != nil {
		fmt.Fprintf(&builder, "  keyset_version: %d\n", *trust.ManifestTrust.KeysetVersion)
	}
	builder.WriteString("  keys:\n")
	for _, key := range trust.ManifestTrust.Keys {
		fmt.Fprintf(&builder, "    - id: %s\n", strings.TrimSpace(key.ID))
		fmt.Fprintf(&builder, "      public_key: %s\n", strings.TrimSpace(key.PublicKey))
	}
	return builder.String()
}

func inferAndroidApplicationID(projectDir string) (string, error) {
	paths := []string{
		filepath.Join(projectDir, "android", "app", "build.gradle.kts"),
		filepath.Join(projectDir, "android", "app", "build.gradle"),
	}
	for _, path := range paths {
		bytes, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		text := string(bytes)
		for _, pattern := range []*regexp.Regexp{
			regexp.MustCompile(`(?m)\bapplicationId\s*=\s*"([^"]+)"`),
			regexp.MustCompile(`(?m)\bapplicationId\s+["']([^"']+)["']`),
			regexp.MustCompile(`(?m)\bnamespace\s*=\s*"([^"]+)"`),
			regexp.MustCompile(`(?m)\bnamespace\s+["']([^"']+)["']`),
		} {
			matches := pattern.FindStringSubmatch(text)
			if len(matches) == 2 && looksLikeAndroidAppID(matches[1]) {
				return matches[1], nil
			}
		}
	}
	return "", errors.New("android/app/build.gradle(.kts) did not contain a literal applicationId")
}

func inferIOSBundleIdentifier(projectDir string) (string, error) {
	paths := []string{
		filepath.Join(projectDir, "ios", "Runner.xcodeproj", "project.pbxproj"),
	}
	for _, path := range paths {
		bytes, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		text := string(bytes)
		pattern := regexp.MustCompile(`(?m)\bPRODUCT_BUNDLE_IDENTIFIER\s*=\s*([^;]+);`)
		matches := pattern.FindAllStringSubmatch(text, -1)
		for _, match := range matches {
			if len(match) != 2 {
				continue
			}
			bundleID := strings.Trim(strings.TrimSpace(match[1]), `"`)
			if strings.Contains(bundleID, "$(") ||
				strings.Contains(bundleID, ".RunnerTests") ||
				strings.HasSuffix(bundleID, "Tests") {
				continue
			}
			if looksLikeAndroidAppID(bundleID) {
				return bundleID, nil
			}
		}
	}
	return "", errors.New("ios/Runner.xcodeproj/project.pbxproj did not contain a literal app bundle identifier")
}

func ensureAutoUpdateConfig(projectDir string, apiBase string, force bool) (bool, error) {
	configPath := filepath.Join(projectDir, soroqAutoUpdateConfigAsset)
	if _, err := os.Stat(configPath); err == nil && !force {
		return false, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return false, err
	}
	config := autoUpdateConfigFile{
		BaseURL: strings.TrimRight(apiBase, "/"),
		Track:   "stable",
		Enabled: true,
	}
	bytes, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return false, err
	}
	bytes = append(bytes, '\n')
	if err := os.WriteFile(configPath, bytes, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func ensurePubspecContainsSoroqAssets(pubspecPath string) (bool, error) {
	updatedConfig, err := ensurePubspecContainsFlutterAsset(pubspecPath, "soroq.yaml")
	if err != nil {
		return false, err
	}
	updatedAutoUpdateConfig, err := ensurePubspecContainsFlutterAsset(pubspecPath, soroqAutoUpdateConfigAsset)
	if err != nil {
		return false, err
	}
	updatedBundledMetadata, err := ensurePubspecContainsFlutterAsset(pubspecPath, soroqBundledMetadataAsset)
	if err != nil {
		return false, err
	}
	return updatedConfig || updatedAutoUpdateConfig || updatedBundledMetadata, nil
}

func ensurePubspecContainsFlutterAsset(pubspecPath string, assetPath string) (bool, error) {
	bytes, err := os.ReadFile(pubspecPath)
	if err != nil {
		return false, err
	}
	text := string(bytes)
	if pubspecContainsFlutterAsset(text, assetPath) {
		return false, nil
	}
	lines := strings.Split(text, "\n")
	flutterIndex := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == "flutter:" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			flutterIndex = index
			break
		}
	}
	if flutterIndex == -1 {
		if strings.TrimSpace(text) != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "\nflutter:\n  assets:\n    - " + assetPath + "\n"
		if err := os.WriteFile(pubspecPath, []byte(text), 0o644); err != nil {
			return false, err
		}
		return true, nil
	}

	flutterEnd := len(lines)
	for index := flutterIndex + 1; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			flutterEnd = index
			break
		}
	}

	assetsIndex := -1
	for index := flutterIndex + 1; index < flutterEnd; index++ {
		if strings.TrimSpace(lines[index]) == "assets:" {
			assetsIndex = index
			break
		}
	}

	insertAt := flutterIndex + 1
	insertLines := []string{"  assets:", "    - " + assetPath}
	if assetsIndex != -1 {
		insertAt = assetsIndex + 1
		insertLines = []string{"    - " + assetPath}
	}

	updated := make([]string, 0, len(lines)+len(insertLines))
	updated = append(updated, lines[:insertAt]...)
	updated = append(updated, insertLines...)
	updated = append(updated, lines[insertAt:]...)
	updatedText := strings.Join(updated, "\n")
	if !strings.HasSuffix(updatedText, "\n") {
		updatedText += "\n"
	}
	if err := os.WriteFile(pubspecPath, []byte(updatedText), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func pubspecContainsFlutterAsset(pubspecText string, assetPath string) bool {
	assetPath = strings.TrimSpace(assetPath)
	for _, line := range strings.Split(pubspecText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			value = strings.Trim(value, `"'`)
			if value == assetPath {
				return true
			}
		}
	}
	return false
}

func ensureAndroidProjectDefaults(projectDir string) (androidProjectFixes, error) {
	var fixes androidProjectFixes
	manifestUpdated, err := ensureAndroidManifestInternetPermission(projectDir)
	if err != nil {
		return fixes, err
	}
	fixes.ManifestInternetUpdated = manifestUpdated

	ndkUpdated, err := ensureAndroidGradleNDKVersion(projectDir)
	if err != nil {
		return fixes, err
	}
	fixes.NDKVersionUpdated = ndkUpdated
	return fixes, nil
}

func ensureAndroidManifestInternetPermission(projectDir string) (bool, error) {
	manifestPath := filepath.Join(projectDir, "android", "app", "src", "main", "AndroidManifest.xml")
	bytes, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	text := string(bytes)
	if strings.Contains(text, `android.permission.INTERNET`) {
		return false, nil
	}
	insertAt, err := openingXMLTagEnd(text, "manifest")
	if err != nil {
		return false, fmt.Errorf("update AndroidManifest.xml: %w", err)
	}
	updated := text[:insertAt] + "\n    <uses-permission android:name=\"android.permission.INTERNET\" />" + text[insertAt:]
	if err := os.WriteFile(manifestPath, []byte(updated), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func ensureAndroidGradleNDKVersion(projectDir string) (bool, error) {
	paths := []string{
		filepath.Join(projectDir, "android", "app", "build.gradle.kts"),
		filepath.Join(projectDir, "android", "app", "build.gradle"),
	}
	for _, path := range paths {
		bytes, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		updated, changed := ensureGradleTextUsesSoroqNDK(string(bytes), strings.HasSuffix(path, ".kts"))
		if !changed {
			return false, nil
		}
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func ensureGradleTextUsesSoroqNDK(text string, kotlinDSL bool) (string, bool) {
	required := soroqAndroidNDKVersion
	if version, ok := findGradleNDKVersion(text); ok && compareDottedVersion(version, required) >= 0 {
		return text, false
	}

	replacements := []struct {
		pattern *regexp.Regexp
		value   string
	}{
		{regexp.MustCompile(`(?m)^(\s*)ndkVersion\s*=\s*flutter\.ndkVersion\s*$`), `${1}ndkVersion = "` + required + `"`},
		{regexp.MustCompile(`(?m)^(\s*)ndkVersion\s*=\s*"[^"]+"\s*$`), `${1}ndkVersion = "` + required + `"`},
		{regexp.MustCompile(`(?m)^(\s*)ndkVersion\s+flutter\.ndkVersion\s*$`), `${1}ndkVersion "` + required + `"`},
		{regexp.MustCompile(`(?m)^(\s*)ndkVersion\s+["'][^"']+["']\s*$`), `${1}ndkVersion "` + required + `"`},
	}
	for _, replacement := range replacements {
		if replacement.pattern.MatchString(text) {
			return replacement.pattern.ReplaceAllString(text, replacement.value), true
		}
	}

	androidBlock := regexp.MustCompile(`(?m)^(\s*)android\s*\{\s*$`)
	matches := androidBlock.FindStringSubmatchIndex(text)
	if len(matches) == 0 {
		return text, false
	}
	lineEnd := strings.IndexByte(text[matches[1]:], '\n')
	if lineEnd == -1 {
		lineEnd = len(text)
	} else {
		lineEnd += matches[1] + 1
	}
	line := `    ndkVersion "` + required + `"`
	if kotlinDSL {
		line = `    ndkVersion = "` + required + `"`
	}
	return text[:lineEnd] + line + "\n" + text[lineEnd:], true
}

func openingXMLTagEnd(text string, tag string) (int, error) {
	start := strings.Index(text, "<"+tag)
	if start == -1 {
		return 0, fmt.Errorf("<%s> tag not found", tag)
	}
	inQuote := byte(0)
	for index := start; index < len(text); index++ {
		ch := text[index]
		switch ch {
		case '\'', '"':
			if inQuote == 0 {
				inQuote = ch
			} else if inQuote == ch {
				inQuote = 0
			}
		case '>':
			if inQuote == 0 {
				return index + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("<%s> tag was not closed", tag)
}

func findGradleNDKVersion(text string) (string, bool) {
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)\bndkVersion\s*=\s*"([^"]+)"`),
		regexp.MustCompile(`(?m)\bndkVersion\s+["']([^"']+)["']`),
	} {
		matches := pattern.FindStringSubmatch(text)
		if len(matches) == 2 {
			return strings.TrimSpace(matches[1]), true
		}
	}
	return "", false
}

func compareDottedVersion(left string, right string) int {
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	length := len(leftParts)
	if len(rightParts) > length {
		length = len(rightParts)
	}
	for index := 0; index < length; index++ {
		leftValue := versionPart(leftParts, index)
		rightValue := versionPart(rightParts, index)
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}

func versionPart(parts []string, index int) int {
	if index >= len(parts) {
		return 0
	}
	value := 0
	for _, ch := range parts[index] {
		if ch < '0' || ch > '9' {
			break
		}
		value = value*10 + int(ch-'0')
	}
	return value
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	check := fs.Bool("check", false, "exit non-zero when the project is not ready")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq status [--project-dir .] [--json] [--check]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	status, err := inspectProject(*projectDir)
	if err != nil {
		return err
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(status); err != nil {
			return err
		}
		if *check && !status.Ready {
			return errors.New("project is not Soroq-ready; review status JSON for warnings")
		}
		return nil
	}

	fmt.Fprintf(os.Stdout, "Project: %s\n", status.ProjectDir)
	fmt.Fprintf(os.Stdout, "pubspec.yaml: %s\n", yesNo(status.HasPubspec))
	fmt.Fprintf(os.Stdout, "soroq.yaml: %s\n", yesNo(status.HasSoroqConfig))
	fmt.Fprintf(os.Stdout, "soroq_flutter dependency: %s\n", yesNo(status.HasSoroqFlutterDependency))
	fmt.Fprintf(os.Stdout, "auto-update config: %s\n", yesNo(status.HasAutoUpdateConfig))
	fmt.Fprintf(os.Stdout, "auto-update asset: %s\n", yesNo(status.PubspecHasAutoUpdateAsset))
	if status.AppID != "" {
		fmt.Fprintf(os.Stdout, "app_id: %s\n", status.AppID)
		fmt.Fprintf(os.Stdout, "app_id valid: %s\n", yesNo(status.AppIDLooksValid))
	}
	if status.Channel != "" {
		fmt.Fprintf(os.Stdout, "channel: %s\n", status.Channel)
		fmt.Fprintf(os.Stdout, "channel valid: %s\n", yesNo(status.ChannelLooksValid))
	}
	if status.RuntimeIDStrategy != "" {
		fmt.Fprintf(os.Stdout, "runtime_id_strategy: %s\n", status.RuntimeIDStrategy)
	}
	if status.AutoUpdateBaseURL != "" {
		fmt.Fprintf(os.Stdout, "auto-update base_url: %s\n", status.AutoUpdateBaseURL)
	}
	if status.AutoUpdateTrack != "" {
		fmt.Fprintf(os.Stdout, "auto-update track: %s\n", status.AutoUpdateTrack)
	}
	fmt.Fprintf(os.Stdout, "manifest_trust: %s\n", yesNo(status.HasManifestTrust))
	if status.HasAndroidManifest {
		fmt.Fprintf(os.Stdout, "android internet permission: %s\n", yesNo(status.HasAndroidInternet))
	}
	if status.AndroidNDKVersion != "" {
		fmt.Fprintf(os.Stdout, "android ndkVersion: %s\n", status.AndroidNDKVersion)
	}
	fmt.Fprintf(os.Stdout, "release ready: %s\n", yesNo(status.ReleaseReady))
	fmt.Fprintf(os.Stdout, "patch ready: %s\n", yesNo(status.PatchReady))
	fmt.Fprintf(os.Stdout, "ready: %s\n", yesNo(status.Ready))
	if len(status.Warnings) > 0 {
		fmt.Fprintln(os.Stdout, "warnings:")
		for _, warning := range status.Warnings {
			fmt.Fprintf(os.Stdout, "- %s\n", warning)
		}
	}
	if *check && !status.Ready {
		return errors.New("project is not Soroq-ready; review warnings above")
	}
	return nil
}

func inspectProject(projectDir string) (projectStatus, error) {
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return projectStatus{}, err
	}
	status := projectStatus{
		ProjectDir:           absDir,
		PubspecPath:          filepath.Join(absDir, "pubspec.yaml"),
		SoroqConfigPath:      filepath.Join(absDir, "soroq.yaml"),
		AutoUpdateConfigPath: filepath.Join(absDir, soroqAutoUpdateConfigAsset),
	}

	pubspecBytes, pubspecErr := os.ReadFile(status.PubspecPath)
	if pubspecErr == nil {
		status.HasPubspec = true
		status.HasSoroqFlutterDependency = hasYamlKey(pubspecBytes, "soroq_flutter")
		status.PubspecHasAutoUpdateAsset = pubspecContainsFlutterAsset(string(pubspecBytes), soroqAutoUpdateConfigAsset)
	} else if !errors.Is(pubspecErr, os.ErrNotExist) {
		return projectStatus{}, pubspecErr
	}

	configBytes, configErr := os.ReadFile(status.SoroqConfigPath)
	if configErr == nil {
		status.HasSoroqConfig = true
		values := parseTopLevelYaml(configBytes)
		status.AppID = values["app_id"]
		status.Channel = values["channel"]
		status.RuntimeIDStrategy = values["runtime_id_strategy"]
		status.HasManifestTrust = hasYamlKey(configBytes, "manifest_trust")
	} else if !errors.Is(configErr, os.ErrNotExist) {
		return projectStatus{}, configErr
	}
	if autoUpdateConfigBytes, err := os.ReadFile(status.AutoUpdateConfigPath); err == nil {
		var config autoUpdateConfigFile
		if json.Unmarshal(autoUpdateConfigBytes, &config) == nil {
			status.AutoUpdateBaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
			status.AutoUpdateTrack = strings.TrimSpace(config.Track)
			status.HasAutoUpdateConfig = status.AutoUpdateBaseURL != ""
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return projectStatus{}, err
	}
	if manifestBytes, err := os.ReadFile(filepath.Join(absDir, "android", "app", "src", "main", "AndroidManifest.xml")); err == nil {
		status.HasAndroidManifest = true
		status.HasAndroidInternet = strings.Contains(string(manifestBytes), "android.permission.INTERNET")
	} else if !errors.Is(err, os.ErrNotExist) {
		return projectStatus{}, err
	}
	if gradleBytes, err := readFirstExisting(filepath.Join(absDir, "android", "app", "build.gradle.kts"), filepath.Join(absDir, "android", "app", "build.gradle")); err == nil {
		if ndkVersion, ok := findGradleNDKVersion(string(gradleBytes)); ok {
			status.AndroidNDKVersion = ndkVersion
			status.AndroidNDKCompatible = compareDottedVersion(ndkVersion, soroqAndroidNDKVersion) >= 0
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return projectStatus{}, err
	}

	status.AppIDLooksValid = status.AppID != "" && looksLikeSoroqAppID(status.AppID)
	status.ChannelLooksValid = status.Channel != "" && looksLikeChannel(status.Channel)

	if !status.HasPubspec {
		status.Warnings = append(status.Warnings, "pubspec.yaml not found; run this inside a Flutter app directory.")
	}
	if !status.HasSoroqConfig {
		status.Warnings = append(status.Warnings, "soroq.yaml is missing; run `soroq init --app-id <your.app.id>`.")
	}
	if status.HasSoroqConfig && status.AppID == "" {
		status.Warnings = append(status.Warnings, "soroq.yaml is missing a top-level app_id value.")
	}
	if status.HasSoroqConfig && status.Channel == "" {
		status.Warnings = append(status.Warnings, "soroq.yaml is missing a top-level channel value.")
	}
	if status.HasSoroqConfig && status.AppID != "" && !status.AppIDLooksValid {
		status.Warnings = append(status.Warnings, "soroq.yaml app_id should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens.")
	}
	if status.HasSoroqConfig && status.Channel != "" && !status.ChannelLooksValid {
		status.Warnings = append(status.Warnings, "soroq.yaml channel should be a stable slug such as stable, beta, or production.")
	}
	if status.HasSoroqConfig && status.RuntimeIDStrategy != "manifest_trust_v1" {
		status.Warnings = append(status.Warnings, "soroq.yaml should include runtime_id_strategy: manifest_trust_v1; rerun `soroq init --force` to refresh hosted trust.")
	}
	if status.HasSoroqConfig && !status.HasManifestTrust {
		status.Warnings = append(status.Warnings, "soroq.yaml is missing hosted manifest_trust public keys; rerun `soroq init --force`.")
	}
	if status.HasPubspec && !status.HasSoroqFlutterDependency {
		status.Warnings = append(status.Warnings, "pubspec.yaml does not declare a soroq_flutter dependency.")
	}
	if status.HasSoroqConfig && !status.HasAutoUpdateConfig {
		status.Warnings = append(status.Warnings, "Soroq auto-update config is missing or has no base_url; rerun `soroq init --force`.")
	}
	if status.HasPubspec && !status.PubspecHasAutoUpdateAsset {
		status.Warnings = append(status.Warnings, "pubspec.yaml does not package soroq/auto_update_config.json; rerun `soroq init --force`.")
	}
	if status.HasAndroidManifest && !status.HasAndroidInternet {
		status.Warnings = append(status.Warnings, "AndroidManifest.xml is missing android.permission.INTERNET; rerun `soroq init --force`.")
	}
	if status.AndroidNDKVersion != "" && !status.AndroidNDKCompatible {
		status.Warnings = append(status.Warnings, fmt.Sprintf("Android Gradle ndkVersion %s is lower than Soroq's supported %s; rerun `soroq init --force`.", status.AndroidNDKVersion, soroqAndroidNDKVersion))
	}

	status.Ready = status.HasPubspec &&
		status.HasSoroqConfig &&
		status.HasSoroqFlutterDependency &&
		status.AppIDLooksValid &&
		status.ChannelLooksValid &&
		status.RuntimeIDStrategy == "manifest_trust_v1" &&
		status.HasManifestTrust &&
		status.HasAutoUpdateConfig &&
		status.PubspecHasAutoUpdateAsset &&
		(!status.HasAndroidManifest || status.HasAndroidInternet) &&
		(status.AndroidNDKVersion == "" || status.AndroidNDKCompatible)
	status.ReleaseReady = status.Ready
	status.PatchReady = status.Ready
	return status, nil
}

func resolveProjectCommandConfig(status projectStatus, channelOverride string) (projectCommandConfig, error) {
	if !status.HasPubspec {
		return projectCommandConfig{}, fmt.Errorf("pubspec.yaml not found in %s", status.ProjectDir)
	}
	if !status.HasSoroqConfig {
		return projectCommandConfig{}, fmt.Errorf("soroq.yaml not found in %s; run `soroq init` first", status.ProjectDir)
	}
	if !status.HasSoroqFlutterDependency {
		return projectCommandConfig{}, fmt.Errorf("pubspec.yaml at %s does not declare a soroq_flutter dependency; run `flutter pub add soroq_flutter`", status.PubspecPath)
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
	if !status.HasAutoUpdateConfig {
		return projectCommandConfig{}, fmt.Errorf("Soroq auto-update config is missing or has no base_url; run `soroq init --force`")
	}
	if !status.PubspecHasAutoUpdateAsset {
		return projectCommandConfig{}, fmt.Errorf("pubspec.yaml at %s does not package %s; run `soroq init --force`", status.PubspecPath, soroqAutoUpdateConfigAsset)
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

func looksLikeAndroidAppID(appID string) bool {
	return androidAppIDPattern.MatchString(appID)
}

func looksLikeSoroqAppID(appID string) bool {
	return soroqAppIDPattern.MatchString(appID)
}

func looksLikeChannel(channel string) bool {
	return channelSlugPattern.MatchString(channel)
}

func parseTopLevelYaml(data []byte) map[string]string {
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		values[key] = value
	}
	return values
}

func hasYamlKey(data []byte, key string) bool {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, key+":") {
			return true
		}
	}
	return false
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
