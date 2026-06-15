package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	errAlreadyPrinted   = errors.New("message already printed")
	androidAppIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	soroqAppIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	channelSlugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

type projectStatus struct {
	ProjectDir                string   `json:"project_dir"`
	PubspecPath               string   `json:"pubspec_path"`
	SoroqConfigPath           string   `json:"soroq_config_path"`
	HasPubspec                bool     `json:"has_pubspec"`
	HasSoroqConfig            bool     `json:"has_soroq_config"`
	HasSoroqFlutterDependency bool     `json:"has_soroq_flutter_dependency"`
	AppID                     string   `json:"app_id,omitempty"`
	Channel                   string   `json:"channel,omitempty"`
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

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	appID := fs.String("app-id", "", "application identifier to store in soroq.yaml")
	channel := fs.String("channel", "stable", "default rollout channel")
	force := fs.Bool("force", false, "overwrite an existing soroq.yaml")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq init --app-id com.example.app [--channel stable] [--project-dir .] [--force]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	resolvedAppID := strings.TrimSpace(*appID)
	resolvedChannel := strings.TrimSpace(*channel)
	if resolvedAppID == "" {
		return errors.New("--app-id is required")
	}
	if !looksLikeSoroqAppID(resolvedAppID) {
		return fmt.Errorf("--app-id %q should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens", resolvedAppID)
	}
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

	content := fmt.Sprintf("app_id: %s\nchannel: %s\n", resolvedAppID, resolvedChannel)
	if err := os.WriteFile(status.SoroqConfigPath, []byte(content), 0o644); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "Wrote %s\n", status.SoroqConfigPath)
	if !status.HasSoroqFlutterDependency {
		fmt.Fprintln(os.Stdout, "Next step: add soroq_flutter to your pubspec.yaml dependencies.")
	}
	fmt.Fprintln(os.Stdout, "Next step: build with a supported Soroq-compatible Flutter toolchain.")
	return nil
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
	if status.AppID != "" {
		fmt.Fprintf(os.Stdout, "app_id: %s\n", status.AppID)
		fmt.Fprintf(os.Stdout, "app_id valid: %s\n", yesNo(status.AppIDLooksValid))
	}
	if status.Channel != "" {
		fmt.Fprintf(os.Stdout, "channel: %s\n", status.Channel)
		fmt.Fprintf(os.Stdout, "channel valid: %s\n", yesNo(status.ChannelLooksValid))
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
		ProjectDir:      absDir,
		PubspecPath:     filepath.Join(absDir, "pubspec.yaml"),
		SoroqConfigPath: filepath.Join(absDir, "soroq.yaml"),
	}

	pubspecBytes, pubspecErr := os.ReadFile(status.PubspecPath)
	if pubspecErr == nil {
		status.HasPubspec = true
		status.HasSoroqFlutterDependency = hasYamlKey(pubspecBytes, "soroq_flutter")
	} else if !errors.Is(pubspecErr, os.ErrNotExist) {
		return projectStatus{}, pubspecErr
	}

	configBytes, configErr := os.ReadFile(status.SoroqConfigPath)
	if configErr == nil {
		status.HasSoroqConfig = true
		values := parseTopLevelYaml(configBytes)
		status.AppID = values["app_id"]
		status.Channel = values["channel"]
	} else if !errors.Is(configErr, os.ErrNotExist) {
		return projectStatus{}, configErr
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
	if status.HasPubspec && !status.HasSoroqFlutterDependency {
		status.Warnings = append(status.Warnings, "pubspec.yaml does not declare a soroq_flutter dependency.")
	}

	status.Ready = status.HasPubspec &&
		status.HasSoroqConfig &&
		status.HasSoroqFlutterDependency &&
		status.AppIDLooksValid &&
		status.ChannelLooksValid
	status.ReleaseReady = status.Ready
	status.PatchReady = status.Ready
	return status, nil
}

func resolveProjectCommandConfig(status projectStatus, channelOverride string) (projectCommandConfig, error) {
	if !status.HasPubspec {
		return projectCommandConfig{}, fmt.Errorf("pubspec.yaml not found in %s", status.ProjectDir)
	}
	if !status.HasSoroqConfig {
		return projectCommandConfig{}, fmt.Errorf("soroq.yaml not found in %s", status.ProjectDir)
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
