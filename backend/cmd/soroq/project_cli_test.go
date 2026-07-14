package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

func TestInspectProjectReady(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	status, err := inspectProject(projectDir)
	if err != nil {
		t.Fatalf("inspectProject() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("expected project to be ready, got %+v", status)
	}
	if !status.ReleaseReady {
		t.Fatalf("expected project to be release ready, got %+v", status)
	}
	if !status.PatchReady {
		t.Fatalf("expected project to be patch ready, got %+v", status)
	}
	if status.AppID != "com.example.app" {
		t.Fatalf("expected app_id, got %q", status.AppID)
	}
	if status.Channel != "stable" {
		t.Fatalf("expected channel, got %q", status.Channel)
	}
	if !status.AppIDLooksValid {
		t.Fatalf("expected app_id to look valid, got %+v", status)
	}
	if !status.ChannelLooksValid {
		t.Fatalf("expected channel to look valid, got %+v", status)
	}
}

func TestInspectProjectAcceptsLogicalSoroqAppID(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("sample-release-aot-cli-proof", "stable"))

	status, err := inspectProject(projectDir)
	if err != nil {
		t.Fatalf("inspectProject() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("expected logical Soroq app id to be ready, got %+v", status)
	}
	if !status.AppIDLooksValid {
		t.Fatalf("expected app_id to look valid, got %+v", status)
	}
}

func TestInspectProjectWarnsWhenDependencyMissing(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "dependencies:\n  flutter:\n    sdk: flutter\n")
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	status, err := inspectProject(projectDir)
	if err != nil {
		t.Fatalf("inspectProject() error = %v", err)
	}
	if status.Ready {
		t.Fatalf("expected project to be not ready")
	}
	if len(status.Warnings) == 0 {
		t.Fatalf("expected warnings, got none")
	}
	if !strings.Contains(strings.Join(status.Warnings, "\n"), "soroq_flutter") {
		t.Fatalf("expected dependency warning, got %v", status.Warnings)
	}
}

func TestInspectProjectWarnsWhenAutoUpdateConfigMissing(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "dependencies:\n  soroq_flutter: any\nflutter:\n  assets:\n    - soroq.yaml\n")
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	status, err := inspectProject(projectDir)
	if err != nil {
		t.Fatalf("inspectProject() error = %v", err)
	}
	if status.Ready {
		t.Fatalf("expected project to be not ready")
	}
	warnings := strings.Join(status.Warnings, "\n")
	if !strings.Contains(warnings, "auto-update config") {
		t.Fatalf("expected auto-update config warning, got %v", status.Warnings)
	}
	if !strings.Contains(warnings, "auto_update_config.json") {
		t.Fatalf("expected packaged asset warning, got %v", status.Warnings)
	}
}

func TestInspectProjectWarnsWhenConfigShapeInvalid(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: Demo App\nchannel: Canary Track\n")

	status, err := inspectProject(projectDir)
	if err != nil {
		t.Fatalf("inspectProject() error = %v", err)
	}
	if status.Ready {
		t.Fatalf("expected malformed config to be not ready")
	}
	if status.ReleaseReady {
		t.Fatalf("expected malformed config to be not release ready")
	}
	if status.PatchReady {
		t.Fatalf("expected malformed config to be not patch ready")
	}
	warnings := strings.Join(status.Warnings, "\n")
	if !strings.Contains(warnings, "stable Soroq app id") {
		t.Fatalf("expected app_id shape warning, got %v", status.Warnings)
	}
	if !strings.Contains(warnings, "stable slug") {
		t.Fatalf("expected channel shape warning, got %v", status.Warnings)
	}
}

func TestRunInitWritesSoroqYaml(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")
	server := newInitTestServer(t, nil)
	defer server.Close()

	stdout := captureStdout(t, func() {
		if err := runInit([]string{"--project-dir", projectDir, "--api", server.URL, "--app-id", "com.example.demo", "--add-dependency=false"}); err != nil {
			t.Fatalf("runInit() error = %v", err)
		}
	})

	content, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(soroq.yaml) error = %v", err)
	}
	text := string(content)
	if !strings.Contains(text, "app_id: com.example.demo") {
		t.Fatalf("expected app_id in file, got %q", text)
	}
	if !strings.Contains(text, "channel: stable") {
		t.Fatalf("expected channel in file, got %q", text)
	}
	if !strings.Contains(text, "runtime_id_strategy: manifest_trust_v1") {
		t.Fatalf("expected runtime_id_strategy in file, got %q", text)
	}
	if !strings.Contains(text, "public_key: test-public-key") {
		t.Fatalf("expected hosted manifest trust in file, got %q", text)
	}
	pubspecBytes, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(pubspec.yaml) error = %v", err)
	}
	if !strings.Contains(string(pubspecBytes), "- soroq.yaml") {
		t.Fatalf("expected soroq.yaml asset in pubspec, got %q", string(pubspecBytes))
	}
	if !strings.Contains(string(pubspecBytes), "- soroq/auto_update_config.json") {
		t.Fatalf("expected auto-update config asset in pubspec, got %q", string(pubspecBytes))
	}
	autoUpdateBytes, err := os.ReadFile(filepath.Join(projectDir, "soroq", "auto_update_config.json"))
	if err != nil {
		t.Fatalf("ReadFile(auto_update_config.json) error = %v", err)
	}
	if !strings.Contains(string(autoUpdateBytes), `"base_url": "`+server.URL+`"`) {
		t.Fatalf("expected api base_url in auto-update config, got %q", string(autoUpdateBytes))
	}
	if !strings.Contains(stdout, "Wrote ") {
		t.Fatalf("expected write confirmation, got %q", stdout)
	}
}

func TestRunInitAddsSoroqFlutterDependencyByDefault(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")
	if err := os.MkdirAll(filepath.Join(projectDir, "android", "app"), 0o755); err != nil {
		t.Fatalf("MkdirAll(android/app) error = %v", err)
	}
	writeFile(t, filepath.Join(projectDir, "android", "app", "build.gradle.kts"), `android {
    namespace = "com.example.demo"
    defaultConfig {
        applicationId = "com.example.demo"
    }
}
`)
	server := newInitTestServer(t, nil)
	defer server.Close()
	pubAddCalls := stubFlutterPubAddSoroqFlutter(t)

	stdout := captureStdout(t, func() {
		if err := runInit([]string{"--project-dir", projectDir, "--api", server.URL}); err != nil {
			t.Fatalf("runInit() error = %v", err)
		}
	})

	if *pubAddCalls != 1 {
		t.Fatalf("expected one flutter pub add call, got %d", *pubAddCalls)
	}
	pubspecBytes, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(pubspec.yaml) error = %v", err)
	}
	pubspecText := string(pubspecBytes)
	if !strings.Contains(pubspecText, "soroq_flutter: ^0.1.13") {
		t.Fatalf("expected soroq_flutter dependency, got %q", pubspecText)
	}
	if !strings.Contains(pubspecText, "- soroq.yaml") {
		t.Fatalf("expected soroq.yaml asset, got %q", pubspecText)
	}
	if !strings.Contains(pubspecText, "- soroq/auto_update_config.json") {
		t.Fatalf("expected auto-update config asset, got %q", pubspecText)
	}
	if !strings.Contains(stdout, "Added soroq_flutter dependency") {
		t.Fatalf("expected dependency output, got %q", stdout)
	}
}

func TestRunInitAppliesAndroidProjectFixes(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")
	if err := os.MkdirAll(filepath.Join(projectDir, "android", "app", "src", "main"), 0o755); err != nil {
		t.Fatalf("MkdirAll(android manifest dir) error = %v", err)
	}
	writeFile(t, filepath.Join(projectDir, "android", "app", "src", "main", "AndroidManifest.xml"), `<manifest xmlns:android="http://schemas.android.com/apk/res/android">
    <application android:label="demo" />
</manifest>
`)
	writeFile(t, filepath.Join(projectDir, "android", "app", "build.gradle.kts"), `android {
    namespace = "com.example.demo"
    ndkVersion = flutter.ndkVersion
    defaultConfig {
        applicationId = "com.example.demo"
    }
}
`)
	server := newInitTestServer(t, nil)
	defer server.Close()

	stdout := captureStdout(t, func() {
		if err := runInit([]string{"--project-dir", projectDir, "--api", server.URL, "--add-dependency=false"}); err != nil {
			t.Fatalf("runInit() error = %v", err)
		}
	})

	manifestBytes, err := os.ReadFile(filepath.Join(projectDir, "android", "app", "src", "main", "AndroidManifest.xml"))
	if err != nil {
		t.Fatalf("ReadFile(AndroidManifest.xml) error = %v", err)
	}
	if !strings.Contains(string(manifestBytes), `android.permission.INTERNET`) {
		t.Fatalf("expected INTERNET permission, got %q", string(manifestBytes))
	}
	gradleBytes, err := os.ReadFile(filepath.Join(projectDir, "android", "app", "build.gradle.kts"))
	if err != nil {
		t.Fatalf("ReadFile(build.gradle.kts) error = %v", err)
	}
	gradleText := string(gradleBytes)
	if !strings.Contains(gradleText, `ndkVersion = "`+soroqAndroidNDKVersion+`"`) {
		t.Fatalf("expected Soroq NDK pin, got %q", gradleText)
	}
	if strings.Contains(gradleText, "flutter.ndkVersion") {
		t.Fatalf("expected flutter.ndkVersion to be replaced, got %q", gradleText)
	}
	if !strings.Contains(stdout, "android.permission.INTERNET") {
		t.Fatalf("expected manifest fix output, got %q", stdout)
	}
	if !strings.Contains(stdout, soroqAndroidNDKVersion) {
		t.Fatalf("expected NDK fix output, got %q", stdout)
	}
}

func TestRunInitInfersAndroidAppIDAndCreatesHostedApp(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")
	if err := os.MkdirAll(filepath.Join(projectDir, "android", "app"), 0o755); err != nil {
		t.Fatalf("MkdirAll(android/app) error = %v", err)
	}
	writeFile(t, filepath.Join(projectDir, "android", "app", "build.gradle.kts"), `android {
    namespace = "com.example.demo"
    defaultConfig {
        applicationId = "com.example.demo"
    }
}
`)

	var captured domain.CreateAppRequest
	server := newInitTestServer(t, &captured)
	defer server.Close()

	stdout := captureStdout(t, func() {
		if err := runInit([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--display-name", "Demo App",
			"--add-dependency=false",
			"--json",
		}); err != nil {
			t.Fatalf("runInit() error = %v", err)
		}
	})

	if captured.ID != "com.example.demo" {
		t.Fatalf("expected inferred app id, got %q", captured.ID)
	}
	if captured.DisplayName != "Demo App" {
		t.Fatalf("expected display name, got %q", captured.DisplayName)
	}

	content, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(soroq.yaml) error = %v", err)
	}
	if !strings.Contains(string(content), "app_id: "+captured.ID) {
		t.Fatalf("expected generated app id in soroq.yaml, got %q", string(content))
	}

	var summary initSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("json.Unmarshal(summary) error = %v; stdout=%q", err, stdout)
	}
	if summary.AppID != captured.ID {
		t.Fatalf("expected summary app id %q, got %q", captured.ID, summary.AppID)
	}
	if summary.RuntimeIDStrategy != "manifest_trust_v1" {
		t.Fatalf("expected manifest trust strategy in summary, got %+v", summary)
	}
	if !summary.PubspecUpdated {
		t.Fatalf("expected pubspec update in summary, got %+v", summary)
	}
	if !summary.AutoUpdateConfigWritten {
		t.Fatalf("expected auto-update config write in summary, got %+v", summary)
	}
	if summary.AutoUpdateBaseURL != server.URL {
		t.Fatalf("expected summary auto-update base URL %q, got %+v", server.URL, summary)
	}
	if summary.HostedApp == nil || summary.HostedApp.ID != captured.ID {
		t.Fatalf("expected hosted app in summary, got %+v", summary.HostedApp)
	}
}

func TestRunInitInfersIOSBundleIdentifier(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: ios_demo\n")
	xcodeProjectDir := filepath.Join(projectDir, "ios", "Runner.xcodeproj")
	if err := os.MkdirAll(xcodeProjectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(Runner.xcodeproj) error = %v", err)
	}
	writeFile(t, filepath.Join(xcodeProjectDir, "project.pbxproj"), `
/* Begin XCBuildConfiguration section */
		1D6058900D05DD3D006BFB54 /* Debug */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				PRODUCT_BUNDLE_IDENTIFIER = dev.soroq.ios_demo;
			};
			name = Debug;
		};
		1D6058910D05DD3D006BFB54 /* RunnerTests */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				PRODUCT_BUNDLE_IDENTIFIER = $(PRODUCT_BUNDLE_IDENTIFIER).RunnerTests;
			};
			name = RunnerTests;
		};
/* End XCBuildConfiguration section */
`)

	var captured domain.CreateAppRequest
	server := newInitTestServer(t, &captured)
	defer server.Close()

	stdout := captureStdout(t, func() {
		if err := runInit([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--display-name", "iOS Demo",
			"--add-dependency=false",
			"--json",
		}); err != nil {
			t.Fatalf("runInit() error = %v", err)
		}
	})

	if captured.ID != "dev.soroq.ios_demo" {
		t.Fatalf("expected inferred iOS app id, got %q", captured.ID)
	}
	content, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(soroq.yaml) error = %v", err)
	}
	if !strings.Contains(string(content), "app_id: "+captured.ID) {
		t.Fatalf("expected generated iOS app id in soroq.yaml, got %q", string(content))
	}
	var summary initSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("json.Unmarshal(summary) error = %v; stdout=%q", err, stdout)
	}
	if summary.AppID != captured.ID {
		t.Fatalf("expected summary app id %q, got %q", captured.ID, summary.AppID)
	}
}

func TestRunInitCreateAppDefaultsDisplayNameFromPubspec(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: hello_world\n")

	var captured domain.CreateAppRequest
	server := newInitTestServer(t, &captured)
	defer server.Close()

	if err := runInit([]string{
		"--project-dir", projectDir,
		"--api", server.URL,
		"--app-id", "com.example.hello",
		"--create-app",
		"--add-dependency=false",
	}); err != nil {
		t.Fatalf("runInit() error = %v", err)
	}

	if captured.ID != "com.example.hello" {
		t.Fatalf("expected explicit app id, got %q", captured.ID)
	}
	if captured.DisplayName != "Hello World" {
		t.Fatalf("expected pubspec-derived display name, got %q", captured.DisplayName)
	}
}

func TestRunInitRejectsInvalidAppID(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")

	err := runInit([]string{"--project-dir", projectDir, "--app-id", "Demo App"})
	if err == nil {
		t.Fatalf("expected invalid app id error")
	}
	if !strings.Contains(err.Error(), "stable Soroq app id") {
		t.Fatalf("expected app id shape guidance, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, "soroq.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("expected soroq.yaml not to be written, stat error = %v", statErr)
	}
}

func TestRunInitRejectsInvalidChannel(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")

	err := runInit([]string{"--project-dir", projectDir, "--app-id", "com.example.demo", "--channel", "Canary Track"})
	if err == nil {
		t.Fatalf("expected invalid channel error")
	}
	if !strings.Contains(err.Error(), "stable slug") {
		t.Fatalf("expected channel shape guidance, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, "soroq.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("expected soroq.yaml not to be written, stat error = %v", statErr)
	}
}

func TestRunStatusJSON(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "beta"))

	stdout := captureStdout(t, func() {
		if err := runStatus([]string{"--project-dir", projectDir, "--json"}); err != nil {
			t.Fatalf("runStatus() error = %v", err)
		}
	})

	var payload projectStatus
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; stdout=%q", err, stdout)
	}
	if payload.Channel != "beta" {
		t.Fatalf("expected beta channel, got %q", payload.Channel)
	}
	if !payload.Ready {
		t.Fatalf("expected ready payload, got %+v", payload)
	}
	if !payload.ReleaseReady {
		t.Fatalf("expected release-ready payload, got %+v", payload)
	}
	if !payload.PatchReady {
		t.Fatalf("expected patch-ready payload, got %+v", payload)
	}
}

func TestRunStatusCheckPassesWhenReady(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	stdout := captureStdout(t, func() {
		if err := runStatus([]string{"--project-dir", projectDir, "--check"}); err != nil {
			t.Fatalf("runStatus(--check) error = %v", err)
		}
	})

	if !strings.Contains(stdout, "ready: yes") {
		t.Fatalf("expected ready status output, got %q", stdout)
	}
}

func TestRunStatusCheckFailsWhenNotReady(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "dependencies:\n  flutter:\n    sdk: flutter\n")
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	var statusErr error
	stdout := captureStdout(t, func() {
		statusErr = runStatus([]string{"--project-dir", projectDir, "--check"})
	})

	if statusErr == nil {
		t.Fatalf("expected --check to fail for not-ready project")
	}
	if !strings.Contains(stdout, "ready: no") {
		t.Fatalf("expected not-ready status output, got %q", stdout)
	}
	if !strings.Contains(stdout, "soroq_flutter") {
		t.Fatalf("expected dependency warning, got %q", stdout)
	}
}

func TestRunFlutterAndroidReleaseBuildUsesProjectSoroqBuildHelper(t *testing.T) {
	projectDir := t.TempDir()
	t.Setenv("SOROQ_ANDROID_ABIS", "")
	scriptsDir := filepath.Join(projectDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}
	writeFile(t, filepath.Join(scriptsDir, "build_soroq_local_engine_aab.sh"), `#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$APP_DIR/build/app/outputs/bundle/release"
{
  printf 'APP_DIR=%s\n' "$APP_DIR"
  printf 'BUILD_MODE=%s\n' "$BUILD_MODE"
  printf 'FLUTTER_EXTRA_ARGS=%s\n' "${FLUTTER_EXTRA_ARGS:-}"
  printf 'SOROQ_BUILD_RUST_JNI=%s\n' "${SOROQ_BUILD_RUST_JNI:-}"
  printf 'SOROQ_ANDROID_ABIS=%s\n' "${SOROQ_ANDROID_ABIS:-}"
} > "$APP_DIR/build-helper.env"
touch "$APP_DIR/build/app/outputs/bundle/release/app-release.aab"
`)

	if err := runFlutterAndroidReleaseBuild(projectDir, "aab", "", []string{"--dart-define=API_ENV=prod"}); err != nil {
		t.Fatalf("runFlutterAndroidReleaseBuild() error = %v", err)
	}

	envBytes, err := os.ReadFile(filepath.Join(projectDir, "build-helper.env"))
	if err != nil {
		t.Fatalf("ReadFile(build-helper.env) error = %v", err)
	}
	envText := string(envBytes)
	if !strings.Contains(envText, "APP_DIR="+projectDir) {
		t.Fatalf("expected helper APP_DIR, got %q", envText)
	}
	if !strings.Contains(envText, "BUILD_MODE=release") {
		t.Fatalf("expected release build mode, got %q", envText)
	}
	if !strings.Contains(envText, "FLUTTER_EXTRA_ARGS=--dart-define=API_ENV=prod --no-tree-shake-icons --target-platform android-arm64") {
		t.Fatalf("expected passthrough Flutter args, got %q", envText)
	}
	if !strings.Contains(envText, "SOROQ_BUILD_RUST_JNI=1") {
		t.Fatalf("expected Soroq runtime JNI default, got %q", envText)
	}
	if !strings.Contains(envText, "SOROQ_ANDROID_ABIS=arm64-v8a") {
		t.Fatalf("expected appbundle helper to match default Flutter target ABI, got %q", envText)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "build", "app", "outputs", "bundle", "release", "app-release.aab")); err != nil {
		t.Fatalf("expected helper-built AAB, stat error = %v", err)
	}
}

func TestSummarizeSoroqBuildOutputKeepsSuccessArtifactAndHidesFlutterNoise(t *testing.T) {
	output := []byte(`Resolving dependencies...
Warning: Flutter support for your project's Gradle version (8.12.0) will soon be dropped.
Potential fix: Your project's gradle version is typically defined in the gradle wrapper file.
Project does not support Flutter build mode: debug, skipping adding Flutter dependencies
Font asset "MaterialIcons-Regular.otf" was tree-shaken, reducing it from 1645184 to 1368 bytes (99.9% reduction).
Running Gradle task 'assembleRelease'...
Built build/app/outputs/flutter-apk/app-release.apk (16.5MB)
`)

	lines := summarizeSoroqBuildOutput(output, true)
	if len(lines) != 1 {
		t.Fatalf("expected one success line, got %#v", lines)
	}
	if lines[0] != "Built build/app/outputs/flutter-apk/app-release.apk (16.5MB)" {
		t.Fatalf("unexpected success summary: %#v", lines)
	}
}

func TestSummarizeSoroqBuildOutputFailureFiltersKnownFlutterNoise(t *testing.T) {
	output := []byte(`Warning: Flutter support for your project's Kotlin version (2.1.0) will soon be dropped.
Running Gradle task 'assembleRelease'...
FAILURE: Build failed with an exception.
* What went wrong:
Execution failed for task ':app:compileReleaseKotlin'.
> Compilation error. See log for more details.
`)

	lines := summarizeSoroqBuildOutput(output, false)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "soon be dropped") || strings.Contains(joined, "Running Gradle task") {
		t.Fatalf("expected Flutter noise to be filtered, got %q", joined)
	}
	if !strings.Contains(joined, "FAILURE: Build failed with an exception.") {
		t.Fatalf("expected failure headline, got %q", joined)
	}
}

func TestRunFlutterAndroidReleaseBuildMapsMultiTargetPlatformsToSoroqABIs(t *testing.T) {
	projectDir := t.TempDir()
	t.Setenv("SOROQ_ANDROID_ABIS", "")
	scriptsDir := filepath.Join(projectDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}
	writeFile(t, filepath.Join(scriptsDir, "build_soroq_local_engine_aab.sh"), `#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$APP_DIR/build/app/outputs/bundle/release"
{
  printf 'FLUTTER_EXTRA_ARGS=%s\n' "${FLUTTER_EXTRA_ARGS:-}"
  printf 'SOROQ_ANDROID_ABIS=%s\n' "${SOROQ_ANDROID_ABIS:-}"
} > "$APP_DIR/build-helper.env"
touch "$APP_DIR/build/app/outputs/bundle/release/app-release.aab"
`)

	if err := runFlutterAndroidReleaseBuild(projectDir, "aab", "", []string{"--target-platform=android-arm,android-arm64,android-x64"}); err != nil {
		t.Fatalf("runFlutterAndroidReleaseBuild() error = %v", err)
	}

	envBytes, err := os.ReadFile(filepath.Join(projectDir, "build-helper.env"))
	if err != nil {
		t.Fatalf("ReadFile(build-helper.env) error = %v", err)
	}
	envText := string(envBytes)
	if !strings.Contains(envText, "FLUTTER_EXTRA_ARGS=--target-platform=android-arm,android-arm64,android-x64") {
		t.Fatalf("expected explicit Flutter target platforms, got %q", envText)
	}
	if !strings.Contains(envText, "SOROQ_ANDROID_ABIS=armeabi-v7a,arm64-v8a,x86_64") {
		t.Fatalf("expected target-matched Soroq ABIs, got %q", envText)
	}
}

func TestRunFlutterAndroidReleaseBuildUsesProjectSoroqApkBuildHelper(t *testing.T) {
	projectDir := t.TempDir()
	t.Setenv("SOROQ_ANDROID_ABIS", "")
	scriptsDir := filepath.Join(projectDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}
	writeFile(t, filepath.Join(scriptsDir, "build_soroq_local_engine_apk.sh"), `#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$APP_DIR/build/app/outputs/flutter-apk"
{
  printf 'APP_DIR=%s\n' "$APP_DIR"
  printf 'BUILD_MODE=%s\n' "$BUILD_MODE"
  printf 'FLUTTER_EXTRA_ARGS=%s\n' "${FLUTTER_EXTRA_ARGS:-}"
  printf 'SOROQ_BUILD_RUST_JNI=%s\n' "${SOROQ_BUILD_RUST_JNI:-}"
  printf 'SOROQ_ANDROID_ABIS=%s\n' "${SOROQ_ANDROID_ABIS:-}"
} > "$APP_DIR/build-helper.env"
touch "$APP_DIR/build/app/outputs/flutter-apk/app-release.apk"
`)

	if err := runFlutterAndroidReleaseBuild(projectDir, "apk", "", []string{"--dart-define=API_ENV=prod"}); err != nil {
		t.Fatalf("runFlutterAndroidReleaseBuild() error = %v", err)
	}

	envBytes, err := os.ReadFile(filepath.Join(projectDir, "build-helper.env"))
	if err != nil {
		t.Fatalf("ReadFile(build-helper.env) error = %v", err)
	}
	envText := string(envBytes)
	if !strings.Contains(envText, "APP_DIR="+projectDir) {
		t.Fatalf("expected helper APP_DIR, got %q", envText)
	}
	if !strings.Contains(envText, "BUILD_MODE=release") {
		t.Fatalf("expected release build mode, got %q", envText)
	}
	if !strings.Contains(envText, "FLUTTER_EXTRA_ARGS=--dart-define=API_ENV=prod --no-tree-shake-icons --target-platform android-arm64") {
		t.Fatalf("expected passthrough Flutter args, got %q", envText)
	}
	if !strings.Contains(envText, "SOROQ_BUILD_RUST_JNI=1") {
		t.Fatalf("expected Soroq runtime JNI default, got %q", envText)
	}
	if !strings.Contains(envText, "SOROQ_ANDROID_ABIS=arm64-v8a") {
		t.Fatalf("expected APK helper ABI default, got %q", envText)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "build", "app", "outputs", "flutter-apk", "app-release.apk")); err != nil {
		t.Fatalf("expected helper-built APK, stat error = %v", err)
	}
}

func TestSoroqAndroidFallbackBuildExtraArgsAddsLocalEngineSrcPath(t *testing.T) {
	t.Setenv("SOROQ_ENGINE_SRC", "")
	t.Setenv("FLUTTER_ENGINE", "")
	flutterRoot := filepath.Join(t.TempDir(), "soroq-forks", "flutter-sdk-src")
	engineSrc := filepath.Join(flutterRoot, "engine", "src")
	if err := os.MkdirAll(filepath.Join(engineSrc, "out"), 0o755); err != nil {
		t.Fatalf("MkdirAll(engine out) error = %v", err)
	}

	args := soroqAndroidFallbackBuildExtraArgs(nil, filepath.Join(flutterRoot, "bin", "flutter"))
	argsText := strings.Join(args, "\n")
	for _, expected := range []string{
		"--target-platform\nandroid-arm64",
		"--local-engine\nandroid_release_arm64",
		"--local-engine-host\nhost_release_arm64",
		"--local-engine-src-path\n" + engineSrc,
	} {
		if !strings.Contains(argsText, expected) {
			t.Fatalf("expected fallback args to contain %q, got %#v", expected, args)
		}
	}
}

func TestSoroqAndroidFallbackBuildExtraArgsPreservesExplicitLocalEngineSrcPath(t *testing.T) {
	t.Setenv("SOROQ_ENGINE_SRC", "")
	t.Setenv("FLUTTER_ENGINE", "")
	explicit := filepath.Join(t.TempDir(), "engine", "src")

	args := soroqAndroidFallbackBuildExtraArgs([]string{"--local-engine-src-path", explicit}, filepath.Join(t.TempDir(), "bin", "flutter"))
	count := 0
	for _, arg := range args {
		if arg == "--local-engine-src-path" || strings.HasPrefix(arg, "--local-engine-src-path=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected one local engine source flag, got %d in %#v", count, args)
	}
	if !strings.Contains(strings.Join(args, "\n"), "--local-engine-src-path\n"+explicit) {
		t.Fatalf("expected explicit local engine source path to be preserved, got %#v", args)
	}
}

// Fix A: patchable Soroq Android release builds must force --no-tree-shake-icons so the base APK
// ships the FULL MaterialIcons font (any icon a later native-AOT code patch introduces already has
// its glyph). soroqAndroidBuildHelperExtraArgs is the shared choke point for the direct-flutter path
// (via soroqAndroidBuildExtraArgsForSource / soroqAndroidFallbackBuildExtraArgs) and the custom
// build-script path (which forwards these args through FLUTTER_EXTRA_ARGS).
func TestSoroqAndroidBuildHelperExtraArgsForcesNoTreeShakeIcons(t *testing.T) {
	args := soroqAndroidBuildHelperExtraArgs(nil)
	if !hasFlutterFlag(args, "--no-tree-shake-icons") {
		t.Fatalf("expected --no-tree-shake-icons to be injected, got %#v", args)
	}
}

func TestSoroqAndroidFallbackBuildExtraArgsForcesNoTreeShakeIcons(t *testing.T) {
	t.Setenv("SOROQ_ENGINE_SRC", "")
	t.Setenv("FLUTTER_ENGINE", "")
	args := soroqAndroidFallbackBuildExtraArgs(nil, filepath.Join(t.TempDir(), "bin", "flutter"))
	if !hasFlutterFlag(args, "--no-tree-shake-icons") {
		t.Fatalf("expected fallback args to force --no-tree-shake-icons, got %#v", args)
	}
}

// Dedup: if the caller already passed --no-tree-shake-icons, do not duplicate it.
func TestSoroqAndroidBuildHelperExtraArgsDedupExplicitNoTreeShake(t *testing.T) {
	args := soroqAndroidBuildHelperExtraArgs([]string{"--no-tree-shake-icons"})
	count := 0
	for _, arg := range args {
		if arg == "--no-tree-shake-icons" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one --no-tree-shake-icons, got %d in %#v", count, args)
	}
}

// Dedup / no-conflict: if the caller explicitly opted INTO tree-shaking (--tree-shake-icons), respect
// it and do NOT inject the conflicting --no-tree-shake-icons.
func TestSoroqAndroidBuildHelperExtraArgsRespectsExplicitTreeShake(t *testing.T) {
	args := soroqAndroidBuildHelperExtraArgs([]string{"--tree-shake-icons"})
	if hasFlutterFlag(args, "--no-tree-shake-icons") {
		t.Fatalf("expected explicit --tree-shake-icons to be respected (no --no-tree-shake-icons), got %#v", args)
	}
	if !hasFlutterFlag(args, "--tree-shake-icons") {
		t.Fatalf("expected explicit --tree-shake-icons to be preserved, got %#v", args)
	}
}

func newInitTestServer(t *testing.T, captured *domain.CreateAppRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/manifest-trust":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{
				"runtime_id_strategy": "manifest_trust_v1",
				"manifest_trust": map[string]any{
					"keyset_version": 1,
					"keys": []map[string]string{
						{"id": "prod-primary", "public_key": "test-public-key"},
					},
				},
			}); err != nil {
				t.Fatalf("Encode(manifest trust) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps":
			var req domain.CreateAppRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("Decode(create app) error = %v", err)
			}
			if captured != nil {
				*captured = req
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.App{
				ID:          req.ID,
				DisplayName: req.DisplayName,
			}); err != nil {
				t.Fatalf("Encode(app) error = %v", err)
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func writeFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func stubFlutterPubAddSoroqFlutter(t *testing.T) *int {
	t.Helper()
	original := runFlutterPubAddSoroqFlutter
	calls := 0
	runFlutterPubAddSoroqFlutter = func(projectDir string) error {
		calls++
		pubspecPath := filepath.Join(projectDir, "pubspec.yaml")
		bytes, err := os.ReadFile(pubspecPath)
		if err != nil {
			return err
		}
		text := string(bytes)
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if !strings.Contains(text, "\ndependencies:") && !strings.HasPrefix(text, "dependencies:") {
			text += "\ndependencies:\n"
		}
		text += "  soroq_flutter: ^0.1.13\n"
		return os.WriteFile(pubspecPath, []byte(text), 0o644)
	}
	t.Cleanup(func() {
		runFlutterPubAddSoroqFlutter = original
	})
	return &calls
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = original
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	return buf.String()
}
