package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	androidrelease "soroq/backend/internal/androidrelease"
)

// Flutter caches assets/flutter_assets, so a build can ship a previously generated soroq_metadata.json
// even after soroq.yaml changed. Registering that release binds a runtime id to an artifact whose
// embedded trust anchor is not the project's, and the mismatch only surfaces later, on a device.

func projectForMetadataTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: example_app\nversion: 1.2.3+45\n")
	writeFile(t, filepath.Join(dir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	return dir
}

func expectedMetadataFor(t *testing.T, dir string) androidrelease.BundledMetadata {
	t.Helper()
	cfg := readFileForTest(t, filepath.Join(dir, "soroq.yaml"))
	pub := readFileForTest(t, filepath.Join(dir, "pubspec.yaml"))
	md, err := buildSoroqBundledMetadata(cfg, pub)
	if err != nil {
		t.Fatalf("buildSoroqBundledMetadata() error = %v", err)
	}
	return md
}

func TestVerifyArtifactMetadataAcceptsMatchingBuild(t *testing.T) {
	dir := projectForMetadataTest(t)
	want := expectedMetadataFor(t, dir)
	snapshot := &androidrelease.Snapshot{Metadata: want}
	snapshot.Artifact.Path = "/tmp/app-release.aab"
	if err := verifyArtifactMetadataMatchesProject(dir, snapshot); err != nil {
		t.Fatalf("an artifact built from the current project must verify: %v", err)
	}
}

func TestVerifyArtifactMetadataRejectsStaleRuntimeID(t *testing.T) {
	dir := projectForMetadataTest(t)
	stale := expectedMetadataFor(t, dir)
	stale.Soroq.RuntimeID = "0000000000000000000000000000000000000000000000000000000000000000"
	snapshot := &androidrelease.Snapshot{Metadata: stale}
	snapshot.Artifact.Path = "/tmp/app-release.aab"

	err := verifyArtifactMetadataMatchesProject(dir, snapshot)
	if err == nil {
		t.Fatal("a stale embedded runtime_id must block registration, not ship silently")
	}
	for _, want := range []string{"runtime_id", "flutter clean", "Nothing was registered"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q:\n%s", want, err)
		}
	}
}

func TestVerifyArtifactMetadataRejectsStaleTrustFingerprint(t *testing.T) {
	dir := projectForMetadataTest(t)
	stale := expectedMetadataFor(t, dir)
	if stale.Soroq.ManifestTrustFingerprint == nil {
		t.Skip("fixture project pins no manifest_trust fingerprint")
	}
	bogus := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	stale.Soroq.ManifestTrustFingerprint = &bogus
	snapshot := &androidrelease.Snapshot{Metadata: stale}
	snapshot.Artifact.Path = "/tmp/app-release.aab"

	err := verifyArtifactMetadataMatchesProject(dir, snapshot)
	if err == nil {
		t.Fatal("an artifact carrying a different trust anchor than the project must be refused")
	}
	if !strings.Contains(err.Error(), "manifest_trust_fingerprint") {
		t.Errorf("error should name the diverging field:\n%s", err)
	}
}

func readFileForTest(t *testing.T, path string) []byte {
	t.Helper()
	b, err := osReadFileForTest(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func osReadFileForTest(path string) ([]byte, error) { return os.ReadFile(path) }
