package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/domain"
)

func TestInspectAndroidArtifactReadsBundledMetadata(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "app-release.aab")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app"),
		"base/lib/arm64-v8a/libflutter.so":                     []byte("flutter"),
	})

	inspection, err := inspectAndroidArtifact(artifactPath)
	if err != nil {
		t.Fatalf("inspectAndroidArtifact() error = %v", err)
	}
	if inspection.Artifact.BundledMetadataZipPath != "assets/flutter_assets/soroq/soroq_metadata.json" {
		t.Fatalf("expected normalized metadata path, got %q", inspection.Artifact.BundledMetadataZipPath)
	}
	if inspection.Metadata.Soroq.AppID != "com.example.app" {
		t.Fatalf("expected app_id, got %q", inspection.Metadata.Soroq.AppID)
	}
	abis := androidrelease.DeriveABIs(inspection)
	if len(abis) != 1 || abis[0] != "arm64-v8a" {
		t.Fatalf("expected inferred arm64-v8a ABI, got %v", abis)
	}
}

func TestResolveReleaseArchPrefersArm64ForMultiABIAPK(t *testing.T) {
	arch, err := resolveReleaseArchForArtifact(
		"apk",
		[]string{"armeabi-v7a", "arm64-v8a", "x86_64"},
		"",
	)
	if err != nil {
		t.Fatalf("resolveReleaseArchForArtifact() error = %v", err)
	}
	if arch != "arm64-v8a" {
		t.Fatalf("expected arm64-v8a, got %q", arch)
	}
}

func TestRunReleaseAndroidRegistersRelease(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})

	var (
		captured         domain.CreateReleaseRequest
		uploadedArtifact []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Release{
				ID:        captured.ID,
				AppID:     captured.AppID,
				RuntimeID: captured.RuntimeID,
				Version:   captured.Version,
				Platform:  captured.Platform,
				Arch:      captured.Arch,
				Channel:   captured.Channel,
			}); err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases/release-1/artifact":
			if got := r.Header.Get("Content-Type"); got != "application/vnd.android.package-archive" {
				t.Fatalf("expected APK content type, got %q", got)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(artifact) error = %v", err)
			}
			uploadedArtifact = body
			if r.URL.Query().Get("filename") != "app-release.apk" {
				t.Fatalf("expected artifact filename query, got %q", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.ReleaseArtifact{
				ReleaseID: "release-1",
				FileName:  "app-release.apk",
				SizeBytes: uint64(len(body)),
				SHA256:    "test-sha",
			}); err != nil {
				t.Fatalf("Encode(artifact response) error = %v", err)
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runReleaseAndroid([]string{
			"--project-dir", projectDir,
			"--artifact", artifactPath,
			"--api", server.URL,
			"--release-id", "release-1",
		})
		if err != nil {
			t.Fatalf("runReleaseAndroid() error = %v", err)
		}
	})

	if captured.ID != "release-1" {
		t.Fatalf("expected release id release-1, got %q", captured.ID)
	}
	if captured.AppID != "com.example.app" {
		t.Fatalf("expected app id, got %q", captured.AppID)
	}
	if captured.RuntimeID != "runtime-1" {
		t.Fatalf("expected runtime id, got %q", captured.RuntimeID)
	}
	if captured.Version != "1.2.3+45" {
		t.Fatalf("expected inferred version, got %q", captured.Version)
	}
	if captured.Arch != "arm64-v8a" {
		t.Fatalf("expected inferred arch, got %q", captured.Arch)
	}
	sourceBytes, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("ReadFile(artifact) error = %v", err)
	}
	if !bytes.Equal(uploadedArtifact, sourceBytes) {
		t.Fatalf("expected uploaded release artifact bytes to match source")
	}
	if !strings.Contains(stdout, "Registered Android release release-1") {
		t.Fatalf("expected registration output, got %q", stdout)
	}
	if !strings.Contains(stdout, "uploaded_artifact_bytes: ") {
		t.Fatalf("expected hosted release artifact output, got %q", stdout)
	}
}

func TestRunReleaseAndroidRejectsVersionOverrideMismatch(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("release mismatch should fail before HTTP request, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	err := runReleaseAndroid([]string{
		"--project-dir", projectDir,
		"--artifact", artifactPath,
		"--api", server.URL,
		"--release-id", "release-1",
		"--version", "1.2.3+46",
	})
	if err == nil {
		t.Fatalf("expected version override mismatch error")
	}
	if !strings.Contains(err.Error(), `--version "1.2.3+46" does not match bundled artifact version "1.2.3+45"`) {
		t.Fatalf("expected bundled version mismatch guidance, got %v", err)
	}
}

func TestRunReleaseIOSRegistersConfigBaseline(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	var captured domain.CreateReleaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Release{
				ID:                   captured.ID,
				AppID:                captured.AppID,
				RuntimeID:            captured.RuntimeID,
				Version:              captured.Version,
				Platform:             captured.Platform,
				Arch:                 captured.Arch,
				Channel:              captured.Channel,
				ManifestSigningKeyID: captured.ManifestSigningKeyID,
			}); err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runReleaseIOS([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--release-id", "ios-release-1",
			"--version", "1.2.3+45",
			"--runtime-id", "ios-runtime-1",
			"--manifest-key-id", "prod-primary",
		})
		if err != nil {
			t.Fatalf("runReleaseIOS() error = %v", err)
		}
	})

	if captured.ID != "ios-release-1" {
		t.Fatalf("expected release id, got %q", captured.ID)
	}
	if captured.Platform != "ios" {
		t.Fatalf("expected platform ios, got %q", captured.Platform)
	}
	if captured.Arch != "arm64" {
		t.Fatalf("expected default arch arm64, got %q", captured.Arch)
	}
	if captured.RuntimeID != "ios-runtime-1" {
		t.Fatalf("expected runtime id, got %q", captured.RuntimeID)
	}
	if captured.ManifestSigningKeyID != "prod-primary" {
		t.Fatalf("expected manifest key id, got %q", captured.ManifestSigningKeyID)
	}
	for _, expected := range []string{
		"Registered iOS config baseline ios-release-1",
		"ios_support: config_ota_only",
		"submit:",                 // names the App Store/TestFlight submission step
		"TestFlight/App Store",    // what to submit to
		"soroq patch ios --config-file config.json",
		"soroq patch config --release-id ios-release-1",
		"does not enable iOS Dart-code OTA",
	} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("expected %q in output, got %q", expected, stdout)
		}
	}
	state, err := loadProjectCLIState(projectDir)
	if err != nil {
		t.Fatalf("loadProjectCLIState() error = %v", err)
	}
	if state.LastIOSRelease == nil {
		t.Fatalf("expected last iOS release state")
	}
	if state.LastIOSRelease.ReleaseID != "ios-release-1" {
		t.Fatalf("expected iOS release id in state, got %+v", state.LastIOSRelease)
	}
	if state.LastIOSRelease.RuntimeID != "ios-runtime-1" {
		t.Fatalf("expected iOS runtime id in state, got %+v", state.LastIOSRelease)
	}
}

func TestRunReleaseIOSInfersVersionAndRuntimeID(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\nversion: 1.2.3+45\n")
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	var captured domain.CreateReleaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/releases" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Release{
			ID:        captured.ID,
			AppID:     captured.AppID,
			RuntimeID: captured.RuntimeID,
			Version:   captured.Version,
			Platform:  captured.Platform,
			Arch:      captured.Arch,
			Channel:   captured.Channel,
		}); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	defer server.Close()

	if err := runReleaseIOS([]string{"--project-dir", projectDir, "--api", server.URL}); err != nil {
		t.Fatalf("runReleaseIOS() error = %v", err)
	}
	if captured.Version != "1.2.3+45" {
		t.Fatalf("expected inferred version, got %q", captured.Version)
	}
	if captured.RuntimeID != defaultIOSConfigRuntimeID("com.example.app", "stable") {
		t.Fatalf("expected default iOS config runtime id, got %q", captured.RuntimeID)
	}
	if captured.Platform != "ios" {
		t.Fatalf("expected platform ios, got %q", captured.Platform)
	}
}

func TestRunReleaseIOSRequiresVersionWhenPubspecHasNone(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	err := runReleaseIOS([]string{"--project-dir", projectDir})
	if err == nil || !strings.Contains(err.Error(), "could not infer iOS release version") {
		t.Fatalf("expected version inference requirement, got %v", err)
	}
}

func TestRunReleaseAndroidDefaultsToNewestArtifactAndRecordsState(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	releaseCandidatesDir := filepath.Join(projectDir, "release-candidates")
	if err := os.MkdirAll(releaseCandidatesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	artifactPath := filepath.Join(releaseCandidatesDir, "app-play-release.aab")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "play-internal", "runtime-1", "1.2.3+45")),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app-arm64"),
		"base/lib/x86_64/libapp.so":                            []byte("app-x64"),
	})

	var (
		captured         domain.CreateReleaseRequest
		uploadedArtifact []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Release{
				ID:                   captured.ID,
				AppID:                captured.AppID,
				RuntimeID:            captured.RuntimeID,
				Version:              captured.Version,
				Platform:             captured.Platform,
				Arch:                 captured.Arch,
				Channel:              captured.Channel,
				ManifestSigningKeyID: captured.ManifestSigningKeyID,
			}); err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases/release-1/artifact":
			if got := r.Header.Get("Content-Type"); got != "application/vnd.android.aab" {
				t.Fatalf("expected AAB content type, got %q", got)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(artifact) error = %v", err)
			}
			uploadedArtifact = body
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.ReleaseArtifact{
				ReleaseID: "release-1",
				FileName:  "app-play-release.aab",
				SizeBytes: uint64(len(body)),
				SHA256:    "test-sha",
			}); err != nil {
				t.Fatalf("Encode(artifact response) error = %v", err)
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runReleaseAndroid([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--release-id", "release-1",
			"--build=false",
		})
		if err != nil {
			t.Fatalf("runReleaseAndroid() error = %v", err)
		}
	})

	if captured.Channel != "play-internal" {
		t.Fatalf("expected artifact channel to win by default, got %q", captured.Channel)
	}
	if captured.Arch != "universal" {
		t.Fatalf("expected universal arch for multi-ABI AAB, got %q", captured.Arch)
	}
	if captured.ManifestSigningKeyID != "prod-primary" {
		t.Fatalf("expected bundled manifest key id, got %q", captured.ManifestSigningKeyID)
	}
	if !strings.Contains(stdout, "artifact: "+artifactPath) {
		t.Fatalf("expected discovered artifact in stdout, got %q", stdout)
	}
	if !strings.Contains(stdout, "release_artifact: "+filepath.Join(projectDir, ".soroq", "releases", "release-1")) {
		t.Fatalf("expected immutable release artifact path in stdout, got %q", stdout)
	}
	if !strings.Contains(stdout, "next: send release_artifact to testers or upload it to Play Store") {
		t.Fatalf("expected release next-step guidance, got %q", stdout)
	}
	sourceBytes, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("ReadFile(source artifact) error = %v", err)
	}
	if !bytes.Equal(uploadedArtifact, sourceBytes) {
		t.Fatalf("expected uploaded artifact bytes to match source")
	}
	state, err := loadProjectCLIState(projectDir)
	if err != nil {
		t.Fatalf("loadProjectCLIState() error = %v", err)
	}
	if state.LastAndroidRelease == nil {
		t.Fatalf("expected last Android release state")
	}
	if state.LastAndroidRelease.ReleaseID != "release-1" {
		t.Fatalf("expected release id in state, got %+v", state.LastAndroidRelease)
	}
	if state.LastAndroidRelease.ArtifactPath == artifactPath {
		t.Fatalf("expected stashed immutable release artifact path, got source path %+v", state.LastAndroidRelease)
	}
	expectedReleaseDir := filepath.Join(projectDir, ".soroq", "releases", "release-1") + string(filepath.Separator)
	if !strings.HasPrefix(state.LastAndroidRelease.ArtifactPath, expectedReleaseDir) {
		t.Fatalf("expected stashed artifact under %s, got %+v", expectedReleaseDir, state.LastAndroidRelease)
	}
	stashedBytes, err := os.ReadFile(state.LastAndroidRelease.ArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile(stashed artifact) error = %v", err)
	}
	if !bytes.Equal(sourceBytes, stashedBytes) {
		t.Fatalf("expected stashed artifact bytes to match source")
	}
}

func TestRunReleaseListPrintsReleases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/releases" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("app_id") != "com.example.app" {
			t.Fatalf("expected app_id query, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.Release{
			{
				ID:        "release-1",
				AppID:     "com.example.app",
				RuntimeID: "runtime-1",
				Version:   "1.2.3+45",
				Platform:  "android",
				Arch:      "arm64-v8a",
				Channel:   "stable",
			},
		}); err != nil {
			t.Fatalf("Encode(releases) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runReleaseList([]string{
			"--api", server.URL,
			"--app-id", "com.example.app",
		})
		if err != nil {
			t.Fatalf("runReleaseList() error = %v", err)
		}
	})

	if !strings.Contains(stdout, "Soroq releases: 1") {
		t.Fatalf("expected release count, got %q", stdout)
	}
	if !strings.Contains(stdout, "release-1") {
		t.Fatalf("expected release id, got %q", stdout)
	}
}

func TestRunReleaseStatusPrintsRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/releases/release-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.Release{
			ID:        "release-1",
			AppID:     "com.example.app",
			RuntimeID: "runtime-1",
			Version:   "1.2.3+45",
			Platform:  "android",
			Arch:      "arm64-v8a",
			Channel:   "stable",
		}); err != nil {
			t.Fatalf("Encode(release) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runReleaseStatus([]string{
			"--api", server.URL,
			"--release-id", "release-1",
		})
		if err != nil {
			t.Fatalf("runReleaseStatus() error = %v", err)
		}
	})

	for _, expected := range []string{
		"Soroq release release-1",
		"app_id: com.example.app",
		"runtime_id: runtime-1",
		"version: 1.2.3+45",
		"arch: arm64-v8a",
		"channel: stable",
	} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("expected %q in output, got %q", expected, stdout)
		}
	}
}

func TestRunReleaseAndroidDefaultsMultiABIAPKToArm64(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
		"lib/x86_64/libapp.so":                            []byte("app"),
	})

	var captured domain.CreateReleaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("Decode(release) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.Release{
				ID:        captured.ID,
				AppID:     captured.AppID,
				RuntimeID: captured.RuntimeID,
				Version:   captured.Version,
				Platform:  captured.Platform,
				Arch:      captured.Arch,
				Channel:   captured.Channel,
			}); err != nil {
				t.Fatalf("Encode(release) error = %v", err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/artifact"):
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.ReleaseArtifact{
				ReleaseID: captured.ID,
				FileName:  "app-release.apk",
				SizeBytes: 1,
				SHA256:    "sha",
			}); err != nil {
				t.Fatalf("Encode(artifact) error = %v", err)
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	if err := runReleaseAndroid([]string{
		"--project-dir", projectDir,
		"--artifact", artifactPath,
		"--api", server.URL,
	}); err != nil {
		t.Fatalf("runReleaseAndroid() error = %v", err)
	}
	if captured.Arch != "arm64-v8a" {
		t.Fatalf("expected arm64-v8a default, got %q", captured.Arch)
	}
}

// isolateOperatorCredentials points credential resolution at an empty temp config and clears the
// environment operator tokens so a test never reads the developer's real ~/.soroq/config.json.
func isolateOperatorCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("SOROQ_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "")
	t.Setenv("SOROQ_OPERATOR_TOKEN", "")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "")
	t.Setenv("SOROQ_API", "")
}

func echoRelease(w http.ResponseWriter, req domain.CreateReleaseRequest) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(domain.Release{
		ID:        req.ID,
		AppID:     req.AppID,
		RuntimeID: req.RuntimeID,
		Version:   req.Version,
		Platform:  req.Platform,
		Arch:      req.Arch,
		Channel:   req.Channel,
	})
}

func testCreateReleaseRequest() domain.CreateReleaseRequest {
	return domain.CreateReleaseRequest{
		ID:        "release-1",
		AppID:     "com.example.app",
		RuntimeID: "runtime-1",
		Version:   "1.2.3+45",
		Platform:  "android",
		Arch:      "arm64-v8a",
		Channel:   "stable",
	}
}

// new-reg: the control plane reports the "unknown app" sentinel, the operator is logged in, so
// createRelease auto-registers the app (create+bind) then retries the release exactly once.
func TestCreateReleaseAutoRegistersUnknownAppThenRetries(t *testing.T) {
	isolateOperatorCredentials(t)
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "cli-secret")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "owner@example.com")

	var (
		releasePosts int
		appPosts     int
		appReq       domain.CreateAppRequest
		appAuth      string
		appEmail     string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			releasePosts++
			var req domain.CreateReleaseRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("Decode(release) error = %v", err)
			}
			if releasePosts == 1 {
				http.Error(w, `{"error":"unknown app \"com.example.app\""}`, http.StatusBadRequest)
				return
			}
			echoRelease(w, req)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps":
			appPosts++
			appAuth = r.Header.Get("Authorization")
			appEmail = r.Header.Get("X-Soroq-Operator-Email")
			if err := json.NewDecoder(r.Body).Decode(&appReq); err != nil {
				t.Fatalf("Decode(app) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.App{ID: appReq.ID, DisplayName: appReq.DisplayName, OwnerEmail: "owner@example.com"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	release, err := createRelease(server.URL, testCreateReleaseRequest(), "com.example.app")
	if err != nil {
		t.Fatalf("createRelease() error = %v", err)
	}
	if release.ID != "release-1" {
		t.Fatalf("expected retried release, got %+v", release)
	}
	if appPosts != 1 {
		t.Fatalf("expected exactly one app create, got %d", appPosts)
	}
	if releasePosts != 2 {
		t.Fatalf("expected release create then retry (2), got %d", releasePosts)
	}
	if appReq.ID != "com.example.app" {
		t.Fatalf("expected auto-created app id, got %q", appReq.ID)
	}
	// Binding to the logged-in operator: the create carries that operator credential, so the
	// server binds the new app to them (foreign-owned ids are rejected server-side, not hijacked).
	if appAuth != "Bearer cli-secret" {
		t.Fatalf("expected operator bearer on app create, got %q", appAuth)
	}
	if appEmail != "owner@example.com" {
		t.Fatalf("expected operator email on app create, got %q", appEmail)
	}
}

// existing-reg: the app already exists, so the first release create succeeds and NO app create
// is attempted (idempotent).
func TestCreateReleaseExistingAppMakesNoCreateAttempt(t *testing.T) {
	isolateOperatorCredentials(t)

	var releasePosts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			releasePosts++
			var req domain.CreateReleaseRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("Decode(release) error = %v", err)
			}
			echoRelease(w, req)
		case r.URL.Path == "/v1/apps":
			t.Fatalf("existing app must not trigger an app create")
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	release, err := createRelease(server.URL, testCreateReleaseRequest(), "com.example.app")
	if err != nil {
		t.Fatalf("createRelease() error = %v", err)
	}
	if release.ID != "release-1" {
		t.Fatalf("expected release, got %+v", release)
	}
	if releasePosts != 1 {
		t.Fatalf("expected a single release create, got %d", releasePosts)
	}
}

// ownership-conflict: a foreign-owned app returns 403 errOperatorForbidden (NOT the "unknown app"
// sentinel), so the release FAILS with the forbidden error verbatim, with NO app create attempt
// and WITHOUT the misleading "soroq app create" hint.
func TestCreateReleaseForbiddenAppSurfacesErrorWithoutCreateOrHint(t *testing.T) {
	isolateOperatorCredentials(t)
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "cli-secret")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "stranger@example.com")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/apps":
			t.Fatalf("a foreign-owned 403 must not trigger an app create")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			http.Error(w, `{"error":"operator is not allowed for this app: com.example.app"}`, http.StatusForbidden)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases/release-1":
			http.Error(w, `{"error":"operator is not allowed for this app: com.example.app"}`, http.StatusForbidden)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	_, err := createRelease(server.URL, testCreateReleaseRequest(), "com.example.app")
	if err == nil {
		t.Fatalf("expected forbidden error")
	}
	if !strings.Contains(err.Error(), "operator is not allowed for this app") {
		t.Fatalf("expected forbidden error verbatim, got %v", err)
	}
	if strings.Contains(err.Error(), "soroq app create") {
		t.Fatalf("forbidden error must not carry the app create hint, got %v", err)
	}
}

// unauthenticated: with no login creds, the "unknown app" sentinel must fail with a clear "run
// soroq login" and MUST NOT attempt registration.
func TestCreateReleaseUnauthenticatedRequiresLoginWithoutCreate(t *testing.T) {
	isolateOperatorCredentials(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/apps":
			t.Fatalf("an unauthenticated release must not attempt an app create")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			http.Error(w, `{"error":"unknown app \"com.example.app\""}`, http.StatusBadRequest)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	_, err := createRelease(server.URL, testCreateReleaseRequest(), "com.example.app")
	if err == nil {
		t.Fatalf("expected unauthenticated error")
	}
	if !strings.Contains(err.Error(), "soroq login") {
		t.Fatalf("expected run soroq login guidance, got %v", err)
	}
	if strings.Contains(err.Error(), "soroq app create") {
		t.Fatalf("unauthenticated error must not carry the app create hint, got %v", err)
	}
}

// End-to-end wiring: `soroq release android` against an unknown app auto-registers then registers
// the release, proving the helper is wired into the CLI path a fresh user hits.
func TestRunReleaseAndroidAutoRegistersUnknownApp(t *testing.T) {
	isolateOperatorCredentials(t)
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "cli-secret")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "owner@example.com")

	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	artifactPath := filepath.Join(t.TempDir(), "app-release.apk")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app"),
	})

	var releasePosts, appPosts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			releasePosts++
			var req domain.CreateReleaseRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("Decode(release) error = %v", err)
			}
			if releasePosts == 1 {
				http.Error(w, `{"error":"unknown app \"com.example.app\""}`, http.StatusBadRequest)
				return
			}
			echoRelease(w, req)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps":
			appPosts++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.App{ID: "com.example.app", DisplayName: "com.example.app", OwnerEmail: "owner@example.com"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runReleaseAndroid([]string{
			"--project-dir", projectDir,
			"--artifact", artifactPath,
			"--api", server.URL,
			"--release-id", "release-1",
			"--upload-artifact=false",
		})
		if err != nil {
			t.Fatalf("runReleaseAndroid() error = %v", err)
		}
	})

	if appPosts != 1 {
		t.Fatalf("expected exactly one app create, got %d", appPosts)
	}
	if releasePosts != 2 {
		t.Fatalf("expected release create then retry (2), got %d", releasePosts)
	}
	if !strings.Contains(stdout, "Registered Android release release-1") {
		t.Fatalf("expected registration output, got %q", stdout)
	}
}

func TestRunReleaseAndroidRejectsInvalidProjectConfigBeforeArtifactInspection(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), "app_id: Demo App\nchannel: stable\n")

	err := runReleaseAndroid([]string{
		"--project-dir", projectDir,
		"--artifact", filepath.Join(t.TempDir(), "missing-release.aab"),
	})
	if err == nil {
		t.Fatalf("expected invalid project config error")
	}
	if !strings.Contains(err.Error(), "stable Soroq app id") {
		t.Fatalf("expected app_id shape guidance, got %v", err)
	}
}

func writeArtifactZip(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create(%s) error = %v", path, err)
	}
	defer file.Close()

	writer := zip.NewWriter(file)
	for name, payload := range entries {
		entryWriter, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%s) error = %v", name, err)
		}
		if _, err := entryWriter.Write(payload); err != nil {
			t.Fatalf("Write(%s) error = %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
}

func testBundledMetadataJSON(appID, channel, runtimeID, version string) string {
	return `{
  "schema_version": 1,
  "app": {
    "name": "Example",
    "version": "` + version + `",
    "build_name": "1.2.3",
    "build_number": "45"
  },
  "soroq": {
    "app_id": "` + appID + `",
    "channel": "` + channel + `",
    "runtime_id": "` + runtimeID + `",
    "runtime_id_strategy": "manifest_trust_v1",
    "manifest_trust": {
      "keys": [
        { "id": "prod-primary", "public_key": "abc" }
      ]
    },
    "manifest_trust_fingerprint": "fingerprint-1"
  }
}`
}
