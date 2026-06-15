package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	androidrelease "soroq/backend/internal/androidrelease"
)

const defaultControlPlaneAPI = "https://soroq-control-plane.fly.dev"
const defaultHostedSurfaceURL = "https://soroq-hosted-surface.vercel.app"

type projectCLIState struct {
	SchemaVersion      int                  `json:"schema_version"`
	LastAndroidRelease *androidReleaseState `json:"last_android_release,omitempty"`
}

type androidReleaseState struct {
	UpdatedAt            time.Time `json:"updated_at"`
	APIBase              string    `json:"api_base"`
	AppID                string    `json:"app_id"`
	Channel              string    `json:"channel"`
	ReleaseID            string    `json:"release_id"`
	RuntimeID            string    `json:"runtime_id"`
	Version              string    `json:"version"`
	Arch                 string    `json:"arch"`
	ArtifactPath         string    `json:"artifact_path"`
	ManifestSigningKeyID string    `json:"manifest_signing_key_id,omitempty"`
}

type discoveredArtifact struct {
	Path    string
	ModTime time.Time
	Size    int64
}

func defaultAPIBase() string {
	value := strings.TrimSpace(os.Getenv("SOROQ_API"))
	if value != "" {
		return value
	}
	if creds, err := currentOperatorCredentials(""); err == nil && strings.TrimSpace(creds.APIBase) != "" {
		return strings.TrimRight(strings.TrimSpace(creds.APIBase), "/")
	}
	return defaultControlPlaneAPI
}

func runFlutterAndroidReleaseBuild(projectDir string, artifactType string, extraArgs []string) error {
	target, err := normalizeAndroidBuildArtifactType(artifactType)
	if err != nil {
		return err
	}
	flutterBin := strings.TrimSpace(os.Getenv("SOROQ_FLUTTER_BIN"))
	if flutterBin == "" {
		flutterBin = "flutter"
	}
	args := append([]string{"build", target, "--release"}, extraArgs...)
	cmd := exec.Command(flutterBin, args...)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return errors.New("flutter " + strings.Join(args, " ") + " failed: " + err.Error())
	}
	return nil
}

func normalizeAndroidBuildArtifactType(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "aab", "appbundle", "bundle":
		return "appbundle", nil
	case "apk":
		return "apk", nil
	default:
		return "", errors.New("--artifact-type must be aab or apk; got " + strconv.Quote(raw))
	}
}

func validateProjectIdentity(status projectStatus) error {
	if !status.HasPubspec {
		return errors.New("pubspec.yaml not found in " + status.ProjectDir)
	}
	if !status.HasSoroqConfig {
		return errors.New("soroq.yaml not found in " + status.ProjectDir)
	}
	if !status.HasSoroqFlutterDependency {
		return errors.New("pubspec.yaml at " + status.PubspecPath + " does not declare a soroq_flutter dependency; run `flutter pub add soroq_flutter`")
	}
	if strings.TrimSpace(status.AppID) == "" {
		return errors.New("soroq.yaml at " + status.SoroqConfigPath + " is missing app_id")
	}
	if !status.AppIDLooksValid {
		return errors.New("soroq.yaml app_id " + strconv.Quote(status.AppID) + " should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens")
	}
	return nil
}

func projectStatePath(projectDir string) string {
	return filepath.Join(projectDir, ".soroq", "cli-state.json")
}

func projectReleaseArtifactPath(projectDir, releaseID, artifactPath string) string {
	releaseDir := filepath.Join(projectDir, ".soroq", "releases", slugifyReleaseID(releaseID))
	fileName := filepath.Base(filepath.Clean(artifactPath))
	if strings.TrimSpace(fileName) == "" || fileName == "." || fileName == string(filepath.Separator) {
		fileName = "android-release" + filepath.Ext(artifactPath)
	}
	return filepath.Join(releaseDir, fileName)
}

func stashAndroidReleaseArtifact(projectDir string, releaseID string, artifactPath string) (string, error) {
	resolvedPath := filepath.Clean(artifactPath)
	stashedPath := projectReleaseArtifactPath(projectDir, releaseID, resolvedPath)
	if filepath.Clean(stashedPath) == resolvedPath {
		return stashedPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(stashedPath), 0o755); err != nil {
		return "", err
	}
	source, err := os.Open(resolvedPath)
	if err != nil {
		return "", err
	}
	defer source.Close()

	tmpPath := stashedPath + ".tmp"
	target, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := target.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, stashedPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return stashedPath, nil
}

func loadProjectCLIState(projectDir string) (projectCLIState, error) {
	statePath := projectStatePath(projectDir)
	bytes, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return projectCLIState{SchemaVersion: 1}, nil
	}
	if err != nil {
		return projectCLIState{}, err
	}
	var state projectCLIState
	if err := json.Unmarshal(bytes, &state); err != nil {
		return projectCLIState{}, err
	}
	if state.SchemaVersion == 0 {
		state.SchemaVersion = 1
	}
	return state, nil
}

func saveProjectCLIState(projectDir string, state projectCLIState) error {
	if state.SchemaVersion == 0 {
		state.SchemaVersion = 1
	}
	stateDir := filepath.Dir(projectStatePath(projectDir))
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	bytes, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	bytes = append(bytes, '\n')
	statePath := projectStatePath(projectDir)
	tmpPath := statePath + ".tmp"
	if err := os.WriteFile(tmpPath, bytes, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, statePath)
}

func discoverDefaultAndroidArtifact(projectDir string) (string, error) {
	artifacts, err := discoverAndroidArtifacts(projectDir)
	if err != nil {
		return "", err
	}
	if len(artifacts) == 0 {
		return "", os.ErrNotExist
	}
	return artifacts[0].Path, nil
}

func discoverCompatibleCandidateArtifact(projectDir string, baseSnapshot *androidrelease.Snapshot) (string, error) {
	artifacts, err := discoverAndroidArtifacts(projectDir)
	if err != nil {
		return "", err
	}
	basePath := filepath.Clean(baseSnapshot.Artifact.Path)
	for _, artifact := range artifacts {
		if filepath.Clean(artifact.Path) == basePath {
			continue
		}
		candidateSnapshot, err := androidrelease.CaptureSnapshot(artifact.Path)
		if err != nil {
			continue
		}
		report := androidrelease.CompareSnapshots(baseSnapshot, candidateSnapshot)
		if report.Compatible || releaseIdentityMatchesIgnoringNativeLibraries(report) {
			return artifact.Path, nil
		}
	}
	return "", os.ErrNotExist
}

func releaseIdentityMatchesIgnoringNativeLibraries(report androidrelease.ComparisonReport) bool {
	for _, check := range report.Checks {
		if check.ID == "native_libraries" {
			continue
		}
		if !check.Passed {
			return false
		}
	}
	return true
}

func discoverAndroidArtifacts(projectDir string) ([]discoveredArtifact, error) {
	patterns := []string{
		filepath.Join(projectDir, "release-candidates", "*.aab"),
		filepath.Join(projectDir, "release-candidates", "*.apk"),
		filepath.Join(projectDir, "build", "app", "outputs", "bundle", "release", "*.aab"),
		filepath.Join(projectDir, "build", "app", "outputs", "apk", "release", "*.apk"),
		filepath.Join(projectDir, "build", "app", "outputs", "flutter-apk", "app-release.apk"),
	}
	byPath := map[string]discoveredArtifact{}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			cleanPath := filepath.Clean(match)
			info, err := os.Stat(cleanPath)
			if err != nil || info.IsDir() {
				continue
			}
			byPath[cleanPath] = discoveredArtifact{
				Path:    cleanPath,
				ModTime: info.ModTime(),
				Size:    info.Size(),
			}
		}
	}
	artifacts := make([]discoveredArtifact, 0, len(byPath))
	for _, artifact := range byPath {
		artifacts = append(artifacts, artifact)
	}
	sort.Slice(artifacts, func(i, j int) bool {
		if artifacts[i].ModTime.Equal(artifacts[j].ModTime) {
			if filepath.Ext(artifacts[i].Path) != filepath.Ext(artifacts[j].Path) {
				return filepath.Ext(artifacts[i].Path) == ".aab"
			}
			if artifacts[i].Size != artifacts[j].Size {
				return artifacts[i].Size > artifacts[j].Size
			}
			return artifacts[i].Path < artifacts[j].Path
		}
		return artifacts[i].ModTime.After(artifacts[j].ModTime)
	})
	return artifacts, nil
}

func firstManifestSigningKeyID(metadata androidrelease.BundledMetadata) string {
	if metadata.Soroq.ManifestTrust == nil {
		return ""
	}
	for _, key := range metadata.Soroq.ManifestTrust.Keys {
		if key.ID == nil {
			continue
		}
		if keyID := strings.TrimSpace(*key.ID); keyID != "" {
			return keyID
		}
	}
	return ""
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	wasSet := false
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			wasSet = true
		}
	})
	return wasSet
}
