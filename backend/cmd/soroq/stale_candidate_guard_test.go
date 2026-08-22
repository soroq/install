package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeArtifactWithModTime(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("artifact"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// A flavored build writes outside the globbed paths, so discovery returns a leftover artifact from an
// earlier build. Registering it ships code the developer did not just compile.
func TestStaleArtifactAfterOurOwnBuildIsRefused(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "app-release.apk")
	buildStart := time.Now()
	writeArtifactWithModTime(t, artifact, buildStart.Add(-45*time.Minute))

	err := guardStaleDiscoveredArtifact(artifact, buildStart)
	if err == nil {
		t.Fatal("an artifact older than the build Soroq just ran must be refused, not registered")
	}
	for _, want := range []string{"older than the build", "--flavor", "--build=false --artifact"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q:\n%s", want, err)
		}
	}
}

func TestFreshArtifactFromOurBuildIsAccepted(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "app-release.apk")
	buildStart := time.Now()
	writeArtifactWithModTime(t, artifact, buildStart.Add(30*time.Second))
	if err := guardStaleDiscoveredArtifact(artifact, buildStart); err != nil {
		t.Fatalf("an artifact written by this build must be accepted: %v", err)
	}
}

// Coarse filesystem timestamps must not reject a genuinely fresh artifact.
func TestArtifactWrittenAtBuildStartIsAccepted(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "app-release.apk")
	buildStart := time.Now()
	writeArtifactWithModTime(t, artifact, buildStart.Add(-1*time.Second))
	if err := guardStaleDiscoveredArtifact(artifact, buildStart); err != nil {
		t.Fatalf("sub-second timestamp granularity must not trip the guard: %v", err)
	}
}

// The developer naming a file explicitly (or --build=false) is not Soroq's build; leave it alone.
func TestGuardIsInertWhenSoroqDidNotBuild(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "app-release.apk")
	writeArtifactWithModTime(t, artifact, time.Now().Add(-72*time.Hour))
	if err := guardStaleDiscoveredArtifact(artifact, time.Time{}); err != nil {
		t.Fatalf("an explicitly supplied artifact must not be second-guessed: %v", err)
	}
}
