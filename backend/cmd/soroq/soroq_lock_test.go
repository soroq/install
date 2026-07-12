package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"soroq/backend/internal/domain"
)

// --- schema round-trip + absent-file behavior ---------------------------------------------------

func TestSoroqLockRoundTripAndAbsent(t *testing.T) {
	projectDir := t.TempDir()

	// Absent file loads as an empty (non-nil-map) value, not an error.
	lock, err := loadSoroqLock(projectDir)
	if err != nil {
		t.Fatalf("loadSoroqLock(absent) error = %v", err)
	}
	if lock.Platforms == nil || len(lock.Platforms) != 0 {
		t.Fatalf("expected empty non-nil platforms map, got %+v", lock.Platforms)
	}

	when := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	if err := recordSoroqLockPin(projectDir, "android", soroqLockPin{
		ReleaseID:        "rel-1",
		Version:          "1.2.3+45",
		ToolchainVersion: "tc-1",
		FrontendVersion:  "fe-1",
		RecordedAt:       when,
	}); err != nil {
		t.Fatalf("recordSoroqLockPin() error = %v", err)
	}

	got, err := loadSoroqLock(projectDir)
	if err != nil {
		t.Fatalf("loadSoroqLock() error = %v", err)
	}
	pin, ok := got.Platforms["android"]
	if !ok {
		t.Fatalf("android pin missing: %+v", got)
	}
	if pin.ReleaseID != "rel-1" || pin.Version != "1.2.3+45" || pin.ToolchainVersion != "tc-1" || pin.FrontendVersion != "fe-1" {
		t.Fatalf("round-trip pin mismatch: %+v", pin)
	}
	if !pin.RecordedAt.Equal(when) {
		t.Fatalf("recorded_at mismatch: got %v want %v", pin.RecordedAt, when)
	}
}

// (a) release build WITHOUT --toolchain pins the active.json toolchain into soroq.lock at project ROOT.
func TestReleaseAndroidWithoutToolchainPinsActiveToolchainAtProjectRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	if err := recordActiveToolchain("android", activeToolchainEntry{
		ToolchainVersion: "toolchain-A",
		FrontendVersion:  "frontend-A",
		RecordedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("recordActiveToolchain() error = %v", err)
	}

	// Stub the SOROQ build so no real Flutter run happens; it writes a discoverable candidate artifact
	// and captures the toolchain it was invoked with (must be the active.json default).
	var builtWithToolchain string
	prevBuild := androidReleaseBuildFn
	androidReleaseBuildFn = func(pd string, artifactType string, toolchainVersion string, extraArgs []string) error {
		builtWithToolchain = toolchainVersion
		outDir := filepath.Join(pd, "release-candidates")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
		writeArtifactZip(t, filepath.Join(outDir, "app-release.aab"), map[string][]byte{
			"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
			"base/lib/arm64-v8a/libapp.so":                         []byte("app-arm64"),
		})
		return nil
	}
	t.Cleanup(func() { androidReleaseBuildFn = prevBuild })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			var req domain.CreateReleaseRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Release{
				ID: req.ID, AppID: req.AppID, RuntimeID: req.RuntimeID, Version: req.Version,
				Platform: req.Platform, Arch: req.Arch, Channel: req.Channel, ManifestSigningKeyID: req.ManifestSigningKeyID,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases/release-1/artifact":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.ReleaseArtifact{ReleaseID: "release-1", FileName: "app-release.aab", SizeBytes: 1, SHA256: "sha"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	captureStdout(t, func() {
		if err := runReleaseAndroid([]string{"--project-dir", projectDir, "--api", server.URL, "--release-id", "release-1"}); err != nil {
			t.Fatalf("runReleaseAndroid() error = %v", err)
		}
	})

	if builtWithToolchain != "toolchain-A" {
		t.Fatalf("expected build to default toolchain from active.json, got %q", builtWithToolchain)
	}

	// The pin must be at <projectDir>/soroq.lock — project ROOT, NOT under the git-ignored .soroq/.
	lockPath := soroqLockPath(projectDir)
	if lockPath != filepath.Join(projectDir, "soroq.lock") {
		t.Fatalf("soroq.lock path is not project root: %q", lockPath)
	}
	if strings.Contains(lockPath, string(filepath.Separator)+".soroq"+string(filepath.Separator)) {
		t.Fatalf("soroq.lock must not live under .soroq/: %q", lockPath)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected soroq.lock at project root: %v", err)
	}
	lock, err := loadSoroqLock(projectDir)
	if err != nil {
		t.Fatalf("loadSoroqLock() error = %v", err)
	}
	pin, ok := lock.Platforms["android"]
	if !ok {
		t.Fatalf("android pin missing after release: %+v", lock)
	}
	if pin.ToolchainVersion != "toolchain-A" {
		t.Fatalf("expected pinned toolchain toolchain-A, got %q", pin.ToolchainVersion)
	}
	if pin.FrontendVersion != "frontend-A" {
		t.Fatalf("expected pinned frontend frontend-A, got %q", pin.FrontendVersion)
	}
	if pin.ReleaseID != "release-1" {
		t.Fatalf("expected pinned release id release-1, got %q", pin.ReleaseID)
	}
}

// (b) release --artifact bypass (soroq did NOT build) writes NO pin, even with active.json present.
func TestReleaseAndroidArtifactBypassWritesNoPin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	if err := recordActiveToolchain("android", activeToolchainEntry{ToolchainVersion: "toolchain-A", FrontendVersion: "frontend-A"}); err != nil {
		t.Fatalf("recordActiveToolchain() error = %v", err)
	}

	artifactPath := filepath.Join(projectDir, "app-release.aab")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", "runtime-1", "1.2.3+45")),
		"base/lib/arm64-v8a/libapp.so":                         []byte("app-arm64"),
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			var req domain.CreateReleaseRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Release{ID: req.ID, AppID: req.AppID, RuntimeID: req.RuntimeID, Version: req.Version, Platform: req.Platform, Arch: req.Arch, Channel: req.Channel})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/artifact"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.ReleaseArtifact{ReleaseID: "release-1", FileName: "app-release.aab", SizeBytes: 1, SHA256: "sha"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	captureStdout(t, func() {
		if err := runReleaseAndroid([]string{"--project-dir", projectDir, "--api", server.URL, "--release-id", "release-1", "--artifact", artifactPath}); err != nil {
			t.Fatalf("runReleaseAndroid() error = %v", err)
		}
	})

	if _, err := os.Stat(soroqLockPath(projectDir)); !os.IsNotExist(err) {
		t.Fatalf("expected NO soroq.lock on the --artifact bypass, stat err = %v", err)
	}
}

// (c) patch with --toolchain != pin is REFUSED, naming both versions and the base release.
func TestResolveAndroidPatchToolchainRefusesConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	if err := recordSoroqLockPin(projectDir, "android", soroqLockPin{ReleaseID: "rel-1", Version: "1.0.0", ToolchainVersion: "tc-base"}); err != nil {
		t.Fatalf("recordSoroqLockPin() error = %v", err)
	}

	_, err := resolveAndroidPatchToolchain(projectDir, "rel-1", "tc-other", nil)
	if err == nil {
		t.Fatalf("expected refusal when --toolchain != pin")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tc-other") || !strings.Contains(msg, "tc-base") || !strings.Contains(msg, "rel-1") {
		t.Fatalf("refusal must name both toolchains and the base release, got: %s", msg)
	}
}

// (d) patch with no --toolchain auto-selects the pinned toolchain (when it is installed).
func TestResolveAndroidPatchToolchainAutoSelectsPin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projectDir := t.TempDir()
	if err := recordSoroqLockPin(projectDir, "android", soroqLockPin{ReleaseID: "rel-1", Version: "1.0.0", ToolchainVersion: "tc-base"}); err != nil {
		t.Fatalf("recordSoroqLockPin() error = %v", err)
	}
	// Mark tc-base as installed (engine.json under ~/.soroq/toolchains/tc-base/android/).
	bundleDir := filepath.Join(home, ".soroq", "toolchains", "tc-base", "android")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeFile(t, filepath.Join(bundleDir, "engine.json"), "{}")

	got, err := resolveAndroidPatchToolchain(projectDir, "rel-1", "", nil)
	if err != nil {
		t.Fatalf("resolveAndroidPatchToolchain() error = %v", err)
	}
	if got != "tc-base" {
		t.Fatalf("expected auto-selected pinned toolchain tc-base, got %q", got)
	}

	// No pin for the base (e.g. an --artifact release) -> honest fallback to the user's toolchain.
	fallback, err := resolveAndroidPatchToolchain(projectDir, "other-release", "user-tc", nil)
	if err != nil {
		t.Fatalf("fallback error = %v", err)
	}
	if fallback != "user-tc" {
		t.Fatalf("expected fallback to user toolchain, got %q", fallback)
	}
}

// (e) pin-not-installed prints the EXACT `soroq toolchain install <v> --api <base>` recovery.
func TestResolveAndroidPatchToolchainNotInstalledRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // empty: pinned toolchain is NOT installed
	projectDir := t.TempDir()
	if err := recordSoroqLockPin(projectDir, "android", soroqLockPin{ReleaseID: "rel-1", Version: "1.0.0", ToolchainVersion: "tc-base"}); err != nil {
		t.Fatalf("recordSoroqLockPin() error = %v", err)
	}

	_, err := resolveAndroidPatchToolchain(projectDir, "rel-1", "", nil)
	if err == nil {
		t.Fatalf("expected not-installed refusal")
	}
	if !strings.Contains(err.Error(), "soroq toolchain install tc-base --api <base>") {
		t.Fatalf("expected exact toolchain-install recovery, got: %s", err.Error())
	}
}

// Fresh-clone guarantee: soroq.lock is committed but the git-ignored cli-state.json is absent, so
// `soroq patch android` (no --release-id/--release-version) takes the early auto-infer build path.
// A --toolchain that differs from the committed pin must STILL be refused there — the enforcement is
// wired into the early path, not only the record path.
func TestRunPatchAndroidFreshCloneRefusesConflictingToolchain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	// Committed pin; deliberately NO .soroq/cli-state.json (as on a fresh clone / CI).
	if err := recordSoroqLockPin(projectDir, "android", soroqLockPin{ReleaseID: "release-1", Version: "1.2.3+45", ToolchainVersion: "tc-base"}); err != nil {
		t.Fatalf("recordSoroqLockPin() error = %v", err)
	}

	err := runPatchAndroid([]string{"--project-dir", projectDir, "--api", "http://127.0.0.1:0", "--toolchain", "tc-other"})
	if err == nil {
		t.Fatalf("expected fresh-clone patch to refuse a toolchain != the committed pin")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tc-other") || !strings.Contains(msg, "tc-base") || !strings.Contains(msg, "release-1") {
		t.Fatalf("early-path refusal must name both toolchains and the pinned release, got: %s", msg)
	}
}

// (f) soroq.lock is at the project ROOT and is NOT matched by the repo .gitignore.
func TestSoroqLockIsCommittableNotGitignored(t *testing.T) {
	dir := t.TempDir()
	if got := soroqLockPath(dir); got != filepath.Join(dir, "soroq.lock") {
		t.Fatalf("soroq.lock must be at project root, got %q", got)
	}

	// Walk up from the test cwd (backend/cmd/soroq) to the repo .gitignore (the one that ignores .soroq/).
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	gitignorePath := ""
	for d := cwd; ; {
		candidate := filepath.Join(d, ".gitignore")
		if b, err := os.ReadFile(candidate); err == nil && strings.Contains(string(b), ".soroq/") {
			gitignorePath = candidate
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if gitignorePath == "" {
		t.Fatalf("could not locate repo .gitignore containing .soroq/ above %s", cwd)
	}
	body, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", gitignorePath, err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "soroq.lock", "*.lock", "/soroq.lock":
			t.Fatalf("%s ignores soroq.lock via %q; it must be committable", gitignorePath, trimmed)
		}
	}
}
