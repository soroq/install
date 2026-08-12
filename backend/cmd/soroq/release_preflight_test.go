package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

// The point of the preflight is that a duplicate release costs ZERO build time. Every case below is
// decided from one list call plus two digest lookups -- no Gradle, no Xcode, no artifact discovery.

func releaseFixture(id, version, arch string) domain.Release {
	return domain.Release{
		ID: id, AppID: "com.example.app", Version: version, Platform: "android",
		Arch: arch, Channel: "stable", RuntimeID: "runtime-" + id,
	}
}

func staticList(releases ...domain.Release) func(string) ([]domain.Release, error) {
	return func(string) ([]domain.Release, error) { return releases, nil }
}

func TestPreflightReleaseClearWhenVersionUnseen(t *testing.T) {
	got := preflightRelease(
		releasePreflightInputs{AppID: "com.example.app", Channel: "stable", Platform: "android", Version: "1.0.1+2"},
		releasePreflightDeps{ListReleases: staticList(releaseFixture("r1", "1.0.0+1", "arm64-v8a"))})
	if got.Verdict != releasePreflightClear {
		t.Fatalf("a new version must be clear to build, got %q (%s)", got.Verdict, got.Detail)
	}
}

func TestPreflightReleaseConflictsOnDuplicateVersionBeforeAnyBuild(t *testing.T) {
	got := preflightRelease(
		releasePreflightInputs{AppID: "com.example.app", Channel: "stable", Platform: "android", Version: "1.0.0+1"},
		releasePreflightDeps{ListReleases: staticList(releaseFixture("r1", "1.0.0+1", "arm64-v8a"))})
	if got.Verdict != releasePreflightConflict {
		t.Fatalf("a duplicate version must conflict, got %q", got.Verdict)
	}
	// The message has to name both ways out, or the user is stuck exactly where they were before.
	msg := releaseConflictError(got, "/tmp/proj").Error()
	for _, want := range []string{"soroq patch android --release-id r1", "pubspec.yaml", "Nothing was built"} {
		if !strings.Contains(msg, want) {
			t.Errorf("conflict message is missing %q:\n%s", want, msg)
		}
	}
}

func TestPreflightReleaseIdempotentWhenArtifactBytesAlreadyHosted(t *testing.T) {
	const digest = "abc123def456abc123def456"
	got := preflightRelease(
		releasePreflightInputs{AppID: "com.example.app", Channel: "stable", Platform: "android", Version: "1.0.0+1"},
		releasePreflightDeps{
			ListReleases:         staticList(releaseFixture("r1", "1.0.0+1", "arm64-v8a")),
			HostedArtifactDigest: func(string) string { return digest },
			LocalSnapshotDigest:  func(string) string { return digest },
		})
	if got.Verdict != releasePreflightIdempotent {
		t.Fatalf("identical hosted+local bytes must be an idempotent no-op, got %q (%s)", got.Verdict, got.Detail)
	}
}

func TestPreflightReleaseConflictsWhenHostedArtifactDiffers(t *testing.T) {
	// Same id, different bytes: the release is immutable, so this can never be accepted.
	got := preflightRelease(
		releasePreflightInputs{AppID: "com.example.app", Channel: "stable", Platform: "android", Version: "1.0.0+1"},
		releasePreflightDeps{
			ListReleases:         staticList(releaseFixture("r1", "1.0.0+1", "arm64-v8a")),
			HostedArtifactDigest: func(string) string { return "1111111111111111" },
			LocalSnapshotDigest:  func(string) string { return "2222222222222222" },
		})
	if got.Verdict != releasePreflightConflict {
		t.Fatalf("a changed artifact under an existing release id must conflict, got %q", got.Verdict)
	}
	if !strings.Contains(got.Detail, "DIFFERENT artifact") {
		t.Errorf("detail should say the artifact differs, got %q", got.Detail)
	}
}

func TestPreflightReleaseStandsAsideWhenControlPlaneUnreachable(t *testing.T) {
	// Offline must not block a build: the post-build path already handles registration failures.
	got := preflightRelease(
		releasePreflightInputs{AppID: "com.example.app", Version: "1.0.0+1"},
		releasePreflightDeps{ListReleases: func(string) ([]domain.Release, error) {
			return nil, errors.New("dial tcp: connection refused")
		}})
	if got.Verdict != releasePreflightUnknown {
		t.Fatalf("an unreachable control plane must not decide the verdict, got %q", got.Verdict)
	}
}

func TestPreflightReleaseExplicitReleaseIDMatchesExactly(t *testing.T) {
	releases := []domain.Release{releaseFixture("chosen-id", "9.9.9+9", "arm64-v8a"), releaseFixture("other", "1.0.0+1", "arm64-v8a")}
	got := preflightRelease(
		releasePreflightInputs{AppID: "com.example.app", ReleaseID: "chosen-id", Version: "1.0.0+1"},
		releasePreflightDeps{ListReleases: staticList(releases...)})
	if got.Verdict != releasePreflightConflict || got.Existing == nil || got.Existing.ID != "chosen-id" {
		t.Fatalf("--release-id must match by id, not by version; got %q / %+v", got.Verdict, got.Existing)
	}
}

func TestPreflightReleaseIgnoresOtherChannelsAndPlatforms(t *testing.T) {
	other := releaseFixture("r-beta", "1.0.0+1", "arm64-v8a")
	other.Channel = "beta"
	ios := releaseFixture("r-ios", "1.0.0+1", "arm64")
	ios.Platform = "ios"
	got := preflightRelease(
		releasePreflightInputs{AppID: "com.example.app", Channel: "stable", Platform: "android", Version: "1.0.0+1"},
		releasePreflightDeps{ListReleases: staticList(other, ios)})
	if got.Verdict != releasePreflightClear {
		t.Fatalf("a different channel/platform is not a collision, got %q (%s)", got.Verdict, got.Detail)
	}
}

// The whole point, end to end: a duplicate release must cost ZERO build invocations. Before the
// preflight, this scenario ran a full Gradle cycle and only then reported the collision.
func TestReleaseAndroidDuplicateVersionRunsNoBuild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	buildCalls := 0
	prevGuard := androidReleaseEnvGuardFn
	androidReleaseEnvGuardFn = func(string, []string) error { return nil }
	t.Cleanup(func() { androidReleaseEnvGuardFn = prevGuard })
	prevBuild := androidReleaseBuildFn
	androidReleaseBuildFn = func(string, string, string, []string) error {
		buildCalls++
		return nil
	}
	t.Cleanup(func() { androidReleaseBuildFn = prevBuild })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/releases" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]domain.Release{{
				ID: "com-example-app-1-2-3-45-arm64-v8a", AppID: "com.example.app",
				Version: "1.2.3+45", Platform: "android", Arch: "arm64-v8a", Channel: "stable",
				RuntimeID: "runtime-1",
			}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	err := runReleaseAndroid([]string{
		"--project-dir", projectDir, "--api", server.URL, "--version", "1.2.3+45", "--arch", "arm64-v8a",
	})
	if err == nil {
		t.Fatal("a duplicate release must fail, not silently succeed")
	}
	if buildCalls != 0 {
		t.Fatalf("the duplicate was detected AFTER %d build(s); the preflight must run first", buildCalls)
	}
	for _, want := range []string{"already exists", "Nothing was built", "pubspec.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%s", want, err.Error())
		}
	}
}
