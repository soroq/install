package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

const defaultControlPlaneAPI = "https://api.soroq.dev"
const defaultHostedSurfaceURL = "https://soroq.dev"

type projectCLIState struct {
	SchemaVersion      int                  `json:"schema_version"`
	LastAndroidRelease *androidReleaseState `json:"last_android_release,omitempty"`
	LastIOSRelease     *iosReleaseState     `json:"last_ios_release,omitempty"`
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

type iosReleaseState struct {
	UpdatedAt            time.Time `json:"updated_at"`
	APIBase              string    `json:"api_base"`
	AppID                string    `json:"app_id"`
	Channel              string    `json:"channel"`
	ReleaseID            string    `json:"release_id"`
	RuntimeID            string    `json:"runtime_id"`
	Version              string    `json:"version"`
	Arch                 string    `json:"arch"`
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

func runFlutterAndroidReleaseBuild(projectDir string, artifactType string, toolchainVersion string, extraArgs []string) error {
	target, err := normalizeAndroidBuildArtifactType(artifactType)
	if err != nil {
		return err
	}
	// A committed project build script is itself an explicit developer-authored opt-in: run it (and let
	// --toolchain prefer a cached toolchain over the legacy LOCAL_ENGINE/SOROQ_REPO_ROOT repo discovery).
	if ok, err := runProjectSoroqAndroidBuildScript(projectDir, target, toolchainVersion, extraArgs); ok || err != nil {
		return err
	}
	// No project build script. Resolve the engine source EXPLICITLY before any Flutter-bin lookup, so a
	// clean project (no cache, no --local-engine) BLOCKS with the exact missing-artifact list instead of
	// silently injecting --local-engine android_release_arm64 (the old repo-checkout hard-require) or a
	// cryptic "Soroq Flutter not found". This is the silent-default removal at the single choke point.
	source, err := resolveAndroidEngineSource(toolchainVersion, extraArgs)
	if err != nil {
		return err
	}
	flutterBin, err := resolveSoroqFlutterBin()
	if err != nil {
		return err
	}
	// Regenerate the bundled Soroq metadata asset from soroq.yaml (offline, no network) so the shipped
	// APK/AAB always carries a fresh, deterministic soroq/soroq_metadata.json — identical between the
	// base build and later candidate/patch builds. Runs only after the engine source + Flutter bin are
	// resolved, so a clean project still blocks first with the missing-artifact list. Best-effort on a
	// non-init'd project (no soroq.yaml). (The committed-build-script path is a legacy developer opt-in
	// that supplies its own metadata; the fresh-user flow goes through this fallback path.)
	if err := regenerateSoroqBundledMetadataForBuild(projectDir); err != nil {
		return err
	}
	// Cached toolchain: complete the minimal cached `out/` layout with the version-matched STOCK host
	// tooling + Gradle embedding from the resolved Soroq frontend, so `--local-engine` has a fully
	// Flutter-buildable engine-source tree. The SOROQ engine bytes (libflutter.so + gen_snapshot) are
	// NEVER replaced — they are the artifacts under test.
	if source.Kind == androidEngineSourceCachedToolchain {
		if err := completeAndroidLocalEngineLayout(source.BundleDir, flutterBin); err != nil {
			return fmt.Errorf("prepare cached Android toolchain local-engine layout: %w", err)
		}
	}
	effectiveExtraArgs := soroqAndroidBuildExtraArgsForSource(extraArgs, flutterBin, source)
	args := append([]string{"build", target, "--release"}, effectiveExtraArgs...)
	cmd := exec.Command(flutterBin, args...)
	cmd.Dir = projectDir
	cmd.Stdin = os.Stdin
	cmd.Env = soroqFlutterBuildEnv(os.Environ())
	cmd.Env = appendDefaultEnv(cmd.Env, "SOROQ_BUILD_RUST_JNI", "1")
	if androidABIs := soroqAndroidABIsForTargetPlatforms(effectiveExtraArgs); androidABIs != "" {
		cmd.Env = appendDefaultEnv(cmd.Env, "SOROQ_ANDROID_ABIS", androidABIs)
	}
	if err := runSoroqBuildCommand(cmd, projectDir, "Building "+androidBuildTargetLabel(target)+" with Soroq Flutter"); err != nil {
		return errors.New("flutter " + strings.Join(args, " ") + " failed: " + err.Error())
	}
	return nil
}

// resolveSoroqFlutterBin resolves the Soroq Flutter frontend's bin/flutter in this order (D1.2):
//  1. $SOROQ_FLUTTER_BIN — explicit dev override.
//  2. a `soroq-flutter` on PATH — packaged shim override.
//  3. the recorded frontend install (`soroq frontend install <version>` -> ~/.soroq/frontends/<active>/...)
//     — the normal fresh-user path, no env needed.
//  4. legacy ~/development/soroq-forks checkout — dev-only fallback.
//
// A clean machine with none of these gets a clear error telling the user to run `soroq frontend install`.
func resolveSoroqFlutterBin() (string, error) {
	if flutterBin := strings.TrimSpace(os.Getenv("SOROQ_FLUTTER_BIN")); flutterBin != "" {
		return flutterBin, nil
	}
	if flutterBin, err := exec.LookPath("soroq-flutter"); err == nil && strings.TrimSpace(flutterBin) != "" {
		return flutterBin, nil
	}
	// Recorded frontend install (Option B): the auto-detected, no-env path.
	if flutterBin, err := resolveInstalledFrontendFlutterBin(); err == nil && strings.TrimSpace(flutterBin) != "" {
		return flutterBin, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, "development", "soroq-forks", "flutter-sdk-src", "bin", "flutter"),
			filepath.Join(home, "development", "soroq-forks", "flutter-sdk-3.28-src", "bin", "flutter"),
		)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("Soroq-compatible Flutter frontend was not found; run `soroq frontend install <version> --api <base>` (or set SOROQ_FLUTTER_BIN to a Soroq Flutter fork's bin/flutter)")
}

func soroqFlutterBuildEnv(env []string) []string {
	if strings.TrimSpace(envValue(env, "JAVA_HOME")) == "" {
		androidStudioJava := "/Applications/Android Studio.app/Contents/jbr/Contents/Home"
		if info, err := os.Stat(androidStudioJava); err == nil && info.IsDir() {
			env = appendDefaultEnv(env, "JAVA_HOME", androidStudioJava)
			env = prependPath(env, filepath.Join(androidStudioJava, "bin"))
		}
	}
	if strings.TrimSpace(envValue(env, "ANDROID_HOME")) == "" {
		if home, _ := os.UserHomeDir(); home != "" {
			androidHome := filepath.Join(home, "Library", "Android", "sdk")
			if info, err := os.Stat(androidHome); err == nil && info.IsDir() {
				env = appendDefaultEnv(env, "ANDROID_HOME", androidHome)
			}
		}
	}
	return env
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func prependPath(env []string, dir string) []string {
	if strings.TrimSpace(dir) == "" {
		return env
	}
	for index, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[index] = "PATH=" + dir + string(os.PathListSeparator) + strings.TrimPrefix(entry, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+dir)
}

func runProjectSoroqAndroidBuildScript(projectDir string, target string, toolchainVersion string, extraArgs []string) (bool, error) {
	var buildScriptName string
	switch target {
	case "appbundle":
		buildScriptName = "build_soroq_local_engine_aab.sh"
	case "apk":
		buildScriptName = "build_soroq_local_engine_apk.sh"
	default:
		return false, nil
	}

	buildScriptPath := filepath.Join(projectDir, "scripts", buildScriptName)
	info, err := os.Stat(buildScriptPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if info.IsDir() {
		return true, fmt.Errorf("Soroq Android build helper is a directory: %s", buildScriptPath)
	}

	effectiveExtraArgs := soroqAndroidBuildHelperExtraArgs(extraArgs)
	cmd := exec.Command("bash", buildScriptPath)
	cmd.Dir = projectDir
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(),
		"APP_DIR="+projectDir,
		"BUILD_MODE=release",
		"FLUTTER_EXTRA_ARGS="+strings.Join(effectiveExtraArgs, " "),
	)
	cmd.Env = appendDefaultEnv(cmd.Env, "SOROQ_BUILD_RUST_JNI", "1")
	if androidABIs := soroqAndroidABIsForTargetPlatforms(effectiveExtraArgs); androidABIs != "" {
		cmd.Env = appendDefaultEnv(cmd.Env, "SOROQ_ANDROID_ABIS", androidABIs)
	}
	cmd.Env = appendDefaultEnv(cmd.Env, "LOCAL_ENGINE", "android_release_arm64")
	cmd.Env = appendDefaultEnv(cmd.Env, "LOCAL_ENGINE_HOST", "host_release_arm64")
	// Engine-source preference (T006): when a cached Android toolchain is selected (--toolchain),
	// point the build script at the cached bundle dir instead of discovering a local repo checkout via
	// SOROQ_REPO_ROOT. With no --toolchain the script keeps its existing developer-opt-in behavior
	// (its own SOROQ_REPO_ROOT discovery / committed engine). GATED here: no Android engine artifacts
	// exist in this checkout, so the cached-toolchain branch is wired but not empirically buildable.
	if version := strings.TrimSpace(toolchainVersion); version != "" {
		if dir, err := androidCachedToolchainBundleDir(version); err == nil {
			if _, statErr := os.Stat(filepath.Join(dir, "engine.json")); statErr != nil {
				return true, fmt.Errorf("android toolchain %q is not installed (no engine.json under %s); run `soroq toolchain install %s --api <base>` first: %w", version, dir, version, statErr)
			}
			cmd.Env = appendDefaultEnv(cmd.Env, "SOROQ_ANDROID_TOOLCHAIN_DIR", dir)
			cmd.Env = appendDefaultEnv(cmd.Env, "SOROQ_TOOLCHAIN_VERSION", version)
		}
	} else if repoRoot := discoverSoroqRepoRoot(projectDir); repoRoot != "" && strings.TrimSpace(os.Getenv("SOROQ_REPO_ROOT")) == "" {
		cmd.Env = append(cmd.Env, "SOROQ_REPO_ROOT="+repoRoot)
	}
	if err := runSoroqBuildCommand(cmd, projectDir, "Building "+androidBuildTargetLabel(target)+" with project helper"); err != nil {
		return true, errors.New(buildScriptPath + " failed: " + err.Error())
	}
	return true, nil
}

func androidBuildTargetLabel(target string) string {
	switch strings.TrimSpace(target) {
	case "apk":
		return "Android APK"
	case "appbundle":
		return "Android app bundle"
	default:
		return "Android release artifact"
	}
}

func androidArtifactTypeForCommand(artifactPath string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(strings.TrimSpace(artifactPath)), ".")) {
	case "apk":
		return "apk"
	case "aab":
		return "aab"
	default:
		return "aab"
	}
}

func runSoroqBuildCommand(cmd *exec.Cmd, projectDir string, label string) error {
	if soroqVerboseBuildOutput() {
		fmt.Fprintln(os.Stderr, label+"...")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	fmt.Fprintln(os.Stderr, label+"...")
	output, err := cmd.CombinedOutput()
	logPath, logErr := writeSoroqBuildLog(projectDir, cmd.Args, output)
	for _, line := range summarizeSoroqBuildOutput(output, err == nil) {
		fmt.Fprintln(os.Stderr, line)
	}
	if err == nil && logErr == nil && logPath != "" {
		fmt.Fprintf(os.Stderr, "Build log: %s\n", logPath)
	}
	if err != nil {
		if logErr == nil && logPath != "" {
			fmt.Fprintf(os.Stderr, "Full build log: %s\n", logPath)
		}
		return err
	}
	return nil
}

// cliVerboseRequested is set by a command's --verbose flag. It ORs with the SOROQ_VERBOSE
// env var so raw subprocess output (flutter / xcode / pub) is streamed either way; default
// is quiet (summarized + logged to .soroq/logs).
var cliVerboseRequested bool

func soroqVerboseBuildOutput() bool {
	if cliVerboseRequested {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOROQ_VERBOSE"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func writeSoroqBuildLog(projectDir string, args []string, output []byte) (string, error) {
	logsDir := filepath.Join(projectDir, ".soroq", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return "", err
	}
	logPath := filepath.Join(logsDir, time.Now().UTC().Format("20060102T150405Z")+"-android-build.log")
	var builder strings.Builder
	if len(args) > 0 {
		builder.WriteString("$ ")
		builder.WriteString(strings.Join(args, " "))
		builder.WriteString("\n\n")
	}
	builder.Write(output)
	if !strings.HasSuffix(builder.String(), "\n") {
		builder.WriteByte('\n')
	}
	return logPath, os.WriteFile(logPath, []byte(builder.String()), 0o644)
}

func summarizeSoroqBuildOutput(output []byte, success bool) []string {
	lines := strings.Split(string(output), "\n")
	if success {
		var built []string
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if isFlutterBuiltLine(trimmed) {
				built = append(built, normalizeFlutterBuiltLine(trimmed))
			}
		}
		if len(built) > 0 {
			return built
		}
		return []string{"Build finished."}
	}

	var important []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isNoisyFlutterBuildLine(trimmed) {
			continue
		}
		important = append(important, trimmed)
	}
	const maxLines = 24
	if len(important) > maxLines {
		important = important[len(important)-maxLines:]
	}
	if len(important) == 0 {
		return []string{"Build failed."}
	}
	return append([]string{"Build failed:"}, important...)
}

func isFlutterBuiltLine(line string) bool {
	if !strings.Contains(line, "Built ") || !strings.Contains(line, "build/") {
		return false
	}
	return strings.Contains(line, ".apk") || strings.Contains(line, ".aab")
}

func normalizeFlutterBuiltLine(line string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "✓ ")
	line = strings.TrimPrefix(line, "√ ")
	return line
}

func isNoisyFlutterBuildLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if lower == "" {
		return true
	}
	noisyPrefixes := []string{
		"warning: flutter support for your project's gradle version",
		"warning: flutter support for your project's android gradle plugin version",
		"warning: flutter support for your project's kotlin version",
		"warning: your android app project:",
		"warning: your app uses the following plugins",
		"potential fix:",
		"alternatively, use the flag",
		"for more information, see",
		"please migrate your app to built-in kotlin",
		"please check the changelogs",
		"if no such version exists",
		"if you are a plugin author",
		"future versions of flutter will fail",
		"project does not support flutter build mode:",
		"font asset ",
		"resolving dependencies",
		"downloading packages",
		"got dependencies",
		"changed ",
		"try `flutter pub outdated`",
		"running gradle task ",
	}
	for _, prefix := range noisyPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	noisyContains := []string{
		"packages have newer versions incompatible with dependency constraints",
		"applies the kotlin gradle plugin",
		"https://docs.flutter.dev/release/breaking-changes/migrate-to-built-in-kotlin",
		"https://docs.gradle.org/current/userguide/gradle_wrapper.html",
	}
	for _, fragment := range noisyContains {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

func soroqAndroidBuildHelperExtraArgs(extraArgs []string) []string {
	effectiveArgs := append([]string{}, extraArgs...)
	for _, arg := range effectiveArgs {
		if arg == "--target-platform" || strings.HasPrefix(arg, "--target-platform=") {
			return effectiveArgs
		}
	}
	return append(effectiveArgs, "--target-platform", "android-arm64")
}

// soroqAndroidBuildExtraArgsForSource builds the flutter passthrough args for the resolved engine
// source. For a CACHED TOOLCHAIN it points --local-engine-src-path at the cached bundle dir (no repo
// checkout, no SOROQ_REPO_ROOT discovery) instead of the legacy repo path. For the ADVANCED
// --local-engine opt-in it preserves the existing fallback behavior. The resolver upstream already
// guaranteed exactly one of these two sources (a clean project blocked before reaching here).
//
// NOTE: the cached-toolchain branch is GATED in this checkout — no Android engine.json layout exists to
// build against — so the src-path wiring is the documented seam, never a fabricated build.
func soroqAndroidBuildExtraArgsForSource(extraArgs []string, flutterBin string, source androidEngineSource) []string {
	if source.Kind == androidEngineSourceCachedToolchain {
		effectiveArgs := soroqAndroidBuildHelperExtraArgs(extraArgs)
		if !hasFlutterFlag(effectiveArgs, "--local-engine") {
			effectiveArgs = append(effectiveArgs, "--local-engine", "android_release_arm64")
		}
		if !hasFlutterFlag(effectiveArgs, "--local-engine-host") {
			effectiveArgs = append(effectiveArgs, "--local-engine-host", "host_release_arm64")
		}
		if !hasFlutterFlag(effectiveArgs, "--local-engine-src-path") {
			if engineSrc := androidEngineSrcPathFromBundleDir(source.BundleDir); engineSrc != "" {
				effectiveArgs = append(effectiveArgs, "--local-engine-src-path", engineSrc)
			}
		}
		return effectiveArgs
	}
	return soroqAndroidFallbackBuildExtraArgs(extraArgs, flutterBin)
}

func soroqAndroidFallbackBuildExtraArgs(extraArgs []string, flutterBin string) []string {
	effectiveArgs := soroqAndroidBuildHelperExtraArgs(extraArgs)
	if !hasFlutterFlag(effectiveArgs, "--local-engine") {
		effectiveArgs = append(effectiveArgs, "--local-engine", "android_release_arm64")
	}
	if !hasFlutterFlag(effectiveArgs, "--local-engine-host") {
		effectiveArgs = append(effectiveArgs, "--local-engine-host", "host_release_arm64")
	}
	if !hasFlutterFlag(effectiveArgs, "--local-engine-src-path") {
		if engineSrc := resolveSoroqLocalEngineSrcPath(flutterBin); engineSrc != "" {
			effectiveArgs = append(effectiveArgs, "--local-engine-src-path", engineSrc)
		}
	}
	return effectiveArgs
}

func hasFlutterFlag(args []string, flagName string) bool {
	for _, arg := range args {
		if arg == flagName || strings.HasPrefix(arg, flagName+"=") {
			return true
		}
	}
	return false
}

func resolveSoroqLocalEngineSrcPath(flutterBin string) string {
	var candidates []string
	for _, envKey := range []string{"SOROQ_ENGINE_SRC", "FLUTTER_ENGINE"} {
		if value := strings.TrimSpace(os.Getenv(envKey)); value != "" {
			candidates = append(candidates, soroqEngineSrcCandidates(value)...)
		}
	}
	if strings.TrimSpace(flutterBin) != "" {
		if resolved, err := filepath.EvalSymlinks(flutterBin); err == nil && strings.TrimSpace(resolved) != "" {
			candidates = append(candidates, soroqEngineSrcCandidatesFromFlutterBin(resolved)...)
		}
		candidates = append(candidates, soroqEngineSrcCandidatesFromFlutterBin(flutterBin)...)
	}
	if home, _ := os.UserHomeDir(); strings.TrimSpace(home) != "" {
		candidates = append(candidates,
			filepath.Join(home, "development", "soroq-forks", "flutter-sdk-src", "engine", "src"),
			filepath.Join(home, "development", "soroq-forks", "flutter-sdk-3.28-src", "engine", "src"),
			filepath.Join(home, "development", "soroq-forks", "src"),
		)
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		candidate = filepath.Clean(strings.TrimSpace(candidate))
		if candidate == "." || seen[candidate] {
			continue
		}
		seen[candidate] = true
		if isUsableSoroqLocalEngineSrcPath(candidate) {
			return candidate
		}
	}
	return ""
}

func soroqEngineSrcCandidatesFromFlutterBin(flutterBin string) []string {
	flutterBin = filepath.Clean(strings.TrimSpace(flutterBin))
	if flutterBin == "." {
		return nil
	}
	flutterRoot := filepath.Dir(filepath.Dir(flutterBin))
	return []string{
		filepath.Join(flutterRoot, "engine", "src"),
		filepath.Join(filepath.Dir(flutterRoot), "src"),
	}
}

func soroqEngineSrcCandidates(path string) []string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." {
		return nil
	}
	return []string{
		path,
		filepath.Join(path, "engine", "src"),
		filepath.Join(path, "src"),
		filepath.Join(path, "flutter-sdk-src", "engine", "src"),
		filepath.Join(path, "soroq-forks", "flutter-sdk-src", "engine", "src"),
		filepath.Join(path, "soroq-forks", "src"),
	}
}

func isUsableSoroqLocalEngineSrcPath(path string) bool {
	info, err := os.Stat(filepath.Join(path, "out"))
	return err == nil && info.IsDir()
}

func soroqAndroidABIsForTargetPlatforms(extraArgs []string) string {
	var targetPlatforms []string
	for index := 0; index < len(extraArgs); index++ {
		arg := strings.TrimSpace(extraArgs[index])
		if arg == "--target-platform" {
			if index+1 < len(extraArgs) {
				targetPlatforms = append(targetPlatforms, strings.Split(extraArgs[index+1], ",")...)
			}
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--target-platform="); ok {
			targetPlatforms = append(targetPlatforms, strings.Split(value, ",")...)
		}
	}

	var abis []string
	seen := map[string]bool{}
	for _, targetPlatform := range targetPlatforms {
		var abi string
		switch strings.TrimSpace(targetPlatform) {
		case "android-arm":
			abi = "armeabi-v7a"
		case "android-arm64":
			abi = "arm64-v8a"
		case "android-x64":
			abi = "x86_64"
		default:
			continue
		}
		if !seen[abi] {
			seen[abi] = true
			abis = append(abis, abi)
		}
	}
	return strings.Join(abis, ",")
}

func appendDefaultEnv(env []string, key string, value string) []string {
	prefix := key + "="
	for index, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if strings.TrimSpace(strings.TrimPrefix(entry, prefix)) == "" {
				env[index] = prefix + value
			}
			return env
		}
	}
	return append(env, prefix+value)
}

func discoverSoroqRepoRoot(projectDir string) string {
	dir := filepath.Clean(projectDir)
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "engine_env.sh")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
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
		return errors.New("soroq.yaml not found in " + status.ProjectDir + "; run `soroq init` first")
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
	if status.RuntimeIDStrategy != "manifest_trust_v1" || !status.HasManifestTrust {
		return errors.New("soroq.yaml at " + status.SoroqConfigPath + " is missing hosted manifest trust; run `soroq init --force` to refresh it")
	}
	if !status.HasAutoUpdateConfig {
		return errors.New("Soroq auto-update config is missing or has no base_url; run `soroq init --force`")
	}
	if !status.PubspecHasAutoUpdateAsset {
		return errors.New("pubspec.yaml at " + status.PubspecPath + " does not package " + soroqAutoUpdateConfigAsset + "; run `soroq init --force`")
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

func discoverSamePathCandidateArtifactAfterBuild(projectDir string, baseSnapshot *androidrelease.Snapshot) (string, error) {
	artifactPath, err := discoverDefaultAndroidArtifact(projectDir)
	if err != nil {
		return "", err
	}
	if filepath.Clean(artifactPath) != filepath.Clean(baseSnapshot.Artifact.Path) {
		return "", os.ErrNotExist
	}
	candidateSnapshot, err := androidrelease.CaptureSnapshot(artifactPath)
	if err != nil {
		return "", err
	}
	report := androidrelease.CompareSnapshots(baseSnapshot, candidateSnapshot)
	if report.Compatible || releaseIdentityMatchesIgnoringNativeLibraries(report) {
		return artifactPath, nil
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
