package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	androidrelease "soroq/backend/internal/androidrelease"
)

const testPubspecWithVersion = "name: fresh_demo_app\nversion: 2.3.4+7\ndependencies:\n  soroq_flutter: any\n"

func TestBuildSoroqBundledMetadataManifestTrustV1IsValidAndDeterministic(t *testing.T) {
	config := []byte(testSoroqYAML("com.example.demo", "stable"))
	pubspec := []byte(testPubspecWithVersion)

	metadata, err := buildSoroqBundledMetadata(config, pubspec)
	if err != nil {
		t.Fatalf("buildSoroqBundledMetadata() error = %v", err)
	}
	if err := metadata.Validate(); err != nil {
		t.Fatalf("generated metadata failed Validate(): %v", err)
	}
	if metadata.SchemaVersion != 1 {
		t.Fatalf("expected schema_version 1, got %d", metadata.SchemaVersion)
	}
	if metadata.App.Name != "fresh_demo_app" {
		t.Fatalf("expected app.name from pubspec, got %q", metadata.App.Name)
	}
	if metadata.App.Version == nil || *metadata.App.Version != "2.3.4+7" {
		t.Fatalf("expected app.version 2.3.4+7, got %v", metadata.App.Version)
	}
	if metadata.App.BuildName == nil || *metadata.App.BuildName != "2.3.4" {
		t.Fatalf("expected build_name 2.3.4, got %v", metadata.App.BuildName)
	}
	if metadata.App.BuildNumber == nil || *metadata.App.BuildNumber != "7" {
		t.Fatalf("expected build_number 7, got %v", metadata.App.BuildNumber)
	}
	if metadata.Soroq.AppID != "com.example.demo" || metadata.Soroq.Channel != "stable" {
		t.Fatalf("unexpected soroq identity: %+v", metadata.Soroq)
	}
	if metadata.RuntimeIDStrategy() != "manifest_trust_v1" {
		t.Fatalf("expected manifest_trust_v1 strategy, got %q", metadata.RuntimeIDStrategy())
	}
	if metadata.Soroq.ManifestTrust == nil || len(metadata.Soroq.ManifestTrust.Keys) != 1 {
		t.Fatalf("expected one manifest_trust key, got %+v", metadata.Soroq.ManifestTrust)
	}
	if metadata.Soroq.ManifestTrust.Keys[0].PublicKey != "test-public-key" {
		t.Fatalf("expected parsed public key, got %+v", metadata.Soroq.ManifestTrust.Keys[0])
	}
	if metadata.Soroq.ManifestTrustFingerprint == nil || *metadata.Soroq.ManifestTrustFingerprint == "" {
		t.Fatalf("expected non-empty fingerprint")
	}
	if metadata.Soroq.RuntimeID == "" {
		t.Fatalf("expected non-empty runtime_id")
	}

	// Determinism: same inputs -> byte-identical metadata.
	again, err := buildSoroqBundledMetadata(config, pubspec)
	if err != nil {
		t.Fatalf("buildSoroqBundledMetadata() second call error = %v", err)
	}
	if again.Soroq.RuntimeID != metadata.Soroq.RuntimeID {
		t.Fatalf("runtime_id not stable: %q vs %q", again.Soroq.RuntimeID, metadata.Soroq.RuntimeID)
	}
	if *again.Soroq.ManifestTrustFingerprint != *metadata.Soroq.ManifestTrustFingerprint {
		t.Fatalf("fingerprint not stable")
	}

	// Decision A: runtime_id is version-INCLUSIVE. Changing the pubspec version MUST change runtime_id
	// (so an old patch is rejected against a new base), while the manifest_trust_fingerprint — which
	// depends only on the trust keyset — stays UNCHANGED.
	otherPubspec := []byte("name: fresh_demo_app\nversion: 9.9.9+99\ndependencies:\n  soroq_flutter: any\n")
	other, err := buildSoroqBundledMetadata(config, otherPubspec)
	if err != nil {
		t.Fatalf("buildSoroqBundledMetadata() version-variant error = %v", err)
	}
	if other.Soroq.RuntimeID == metadata.Soroq.RuntimeID {
		t.Fatalf("runtime_id must change with version (version-inclusive), but stayed %q", metadata.Soroq.RuntimeID)
	}
	if *other.Soroq.ManifestTrustFingerprint != *metadata.Soroq.ManifestTrustFingerprint {
		t.Fatalf("fingerprint must NOT depend on version, but changed")
	}
}

// TestBuildSoroqBundledMetadataGoldenMatchesFork pins the CLI derivation to the fork's
// soroq_metadata.dart output on a fixed fixture. The expected fingerprint and runtime_id hex values
// are computed INDEPENDENTLY (by an out-of-band reference implementation of the exact fork formulas,
// see scripts/diff_soroq_metadata.sh) and hardcoded here, so a regression in the Go derivation — order,
// HTML escaping, null handling, or reverting to a version-exclusive runtime_id — fails this test.
func TestBuildSoroqBundledMetadataGoldenMatchesFork(t *testing.T) {
	config := []byte(testSoroqYAML("com.example.demo", "stable"))
	// pubspec version 1.0.0+1 (the `flutter create` default).
	pubspec := []byte("name: fresh_demo_app\nversion: 1.0.0+1\ndependencies:\n  soroq_flutter: any\n")

	const (
		wantFingerprint = "8ac0b6989dda9d510a336c649e826ec2b183b98eac5f7e652b1fc8903ed29cc3"
		wantRuntimeID   = "957d791ef92c720004a3c94a0f99282945616895f5e686591d9cf071f2eb842f"
	)

	metadata, err := buildSoroqBundledMetadata(config, pubspec)
	if err != nil {
		t.Fatalf("buildSoroqBundledMetadata() error = %v", err)
	}
	if metadata.Soroq.ManifestTrustFingerprint == nil || *metadata.Soroq.ManifestTrustFingerprint != wantFingerprint {
		t.Fatalf("manifest_trust_fingerprint = %v, want %s", metadata.Soroq.ManifestTrustFingerprint, wantFingerprint)
	}
	if metadata.Soroq.RuntimeID != wantRuntimeID {
		t.Fatalf("runtime_id = %s, want %s (fork-derived golden)", metadata.Soroq.RuntimeID, wantRuntimeID)
	}

	// A version bump must move the runtime_id to a specific, fork-matching value.
	const wantRuntimeIDBumped = "f11deaa21225aed38a83a26de66324428b4cde6a0de721769e7d52031add6e76"
	bumped, err := buildSoroqBundledMetadata(config, []byte("name: fresh_demo_app\nversion: 1.0.1+2\ndependencies:\n  soroq_flutter: any\n"))
	if err != nil {
		t.Fatalf("buildSoroqBundledMetadata() bumped error = %v", err)
	}
	if bumped.Soroq.RuntimeID != wantRuntimeIDBumped {
		t.Fatalf("bumped runtime_id = %s, want %s", bumped.Soroq.RuntimeID, wantRuntimeIDBumped)
	}
}

func TestBuildSoroqBundledMetadataDistinctAppsDifferentRuntimeID(t *testing.T) {
	pubspec := []byte(testPubspecWithVersion)
	a, err := buildSoroqBundledMetadata([]byte(testSoroqYAML("com.example.a", "stable")), pubspec)
	if err != nil {
		t.Fatalf("build a error = %v", err)
	}
	b, err := buildSoroqBundledMetadata([]byte(testSoroqYAML("com.example.b", "stable")), pubspec)
	if err != nil {
		t.Fatalf("build b error = %v", err)
	}
	if a.Soroq.RuntimeID == b.Soroq.RuntimeID {
		t.Fatalf("expected distinct runtime_ids for distinct app_ids")
	}
}

func TestGenerateSoroqBundledMetadataWritesValidAsset(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), testPubspecWithVersion)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.demo", "stable"))

	if err := generateSoroqBundledMetadata(projectDir); err != nil {
		t.Fatalf("generateSoroqBundledMetadata() error = %v", err)
	}
	bytes, err := os.ReadFile(filepath.Join(projectDir, "soroq", "soroq_metadata.json"))
	if err != nil {
		t.Fatalf("ReadFile(soroq_metadata.json) error = %v", err)
	}
	var metadata androidrelease.BundledMetadata
	if err := json.Unmarshal(bytes, &metadata); err != nil {
		t.Fatalf("unmarshal generated metadata error = %v", err)
	}
	if err := metadata.Validate(); err != nil {
		t.Fatalf("written metadata failed Validate(): %v", err)
	}
}

func TestRegenerateSoroqBundledMetadataForBuildSkipsWithoutConfig(t *testing.T) {
	projectDir := t.TempDir()
	if err := regenerateSoroqBundledMetadataForBuild(projectDir); err != nil {
		t.Fatalf("expected no error without soroq.yaml, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "soroq", "soroq_metadata.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no metadata written without soroq.yaml, stat err = %v", err)
	}
}
