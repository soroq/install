package signing

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"soroq/backend/internal/domain"
)

func testManifest() domain.PatchManifest {
	return domain.PatchManifest{
		PatchID:        "patch-1",
		PatchNumber:    1,
		RuntimeID:      "runtime-1",
		ReleaseID:      "release-1",
		Channel:        "stable",
		Kind:           domain.PatchKindAssetPlusExperimentalNativeAOT,
		ActivationMode: domain.ActivationNextColdStart,
		Artifact: domain.PatchArtifact{
			URL:       "file://demo/patch-1.bin",
			SHA256:    "abc123",
			SizeBytes: 42,
		},
		Signature: nil,
	}
}

func TestManifestSigningPayloadIgnoresSignatureField(t *testing.T) {
	manifest := testManifest()
	unsignedPayload := ManifestSigningPayload(manifest)

	signature := "signed"
	manifest.Signature = &signature
	signedPayload := ManifestSigningPayload(manifest)

	if !bytes.Equal(unsignedPayload, signedPayload) {
		t.Fatalf("expected signing payload to ignore manifest signature field")
	}
}

func TestManifestSignerRoundTrip(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, 32)
	signer, err := NewManifestSignerFromSeed(seed, "")
	if err != nil {
		t.Fatalf("NewManifestSignerFromSeed() error = %v", err)
	}

	manifest := testManifest()
	signature, err := signer.SignManifest(manifest)
	if err != nil {
		t.Fatalf("SignManifest() error = %v", err)
	}
	manifest.Signature = &signature

	if err := VerifyManifestSignature(manifest, signer.PublicKeyBase64()); err != nil {
		t.Fatalf("VerifyManifestSignature() error = %v", err)
	}
	if signer.KeyID() == "" {
		t.Fatalf("expected signer key id to be populated")
	}
}

func TestVerifyManifestSignatureRejectsTampering(t *testing.T) {
	seed := bytes.Repeat([]byte{9}, 32)
	signer, err := NewManifestSignerFromSeed(seed, "")
	if err != nil {
		t.Fatalf("NewManifestSignerFromSeed() error = %v", err)
	}

	manifest := testManifest()
	signature, err := signer.SignManifest(manifest)
	if err != nil {
		t.Fatalf("SignManifest() error = %v", err)
	}
	manifest.Signature = &signature
	manifest.Artifact.SHA256 = "different"

	if err := VerifyManifestSignature(manifest, signer.PublicKeyBase64()); err == nil {
		t.Fatalf("expected tampered manifest verification to fail")
	}
}

func TestManifestSignerSetResolvesDefaultAndProducesTrustConfig(t *testing.T) {
	signerA, err := NewManifestSignerFromSeed(bytes.Repeat([]byte{1}, 32), "prod-a")
	if err != nil {
		t.Fatalf("NewManifestSignerFromSeed(prod-a) error = %v", err)
	}
	signerB, err := NewManifestSignerFromSeed(bytes.Repeat([]byte{2}, 32), "prod-b")
	if err != nil {
		t.Fatalf("NewManifestSignerFromSeed(prod-b) error = %v", err)
	}

	set, err := NewManifestSignerSet([]*ManifestSigner{signerA, signerB}, "prod-a")
	if err != nil {
		t.Fatalf("NewManifestSignerSet() error = %v", err)
	}

	resolvedDefault, err := set.ResolveKeyID("")
	if err != nil {
		t.Fatalf("ResolveKeyID(default) error = %v", err)
	}
	if resolvedDefault != "prod-a" {
		t.Fatalf("expected default key id prod-a, got %q", resolvedDefault)
	}

	keysetVersion := 2
	trustConfig := set.ManifestTrustConfig(&keysetVersion)
	if trustConfig.KeysetVersion == nil || *trustConfig.KeysetVersion != 2 {
		t.Fatalf("expected keyset version 2, got %#v", trustConfig.KeysetVersion)
	}
	if len(trustConfig.Keys) != 2 {
		t.Fatalf("expected 2 trust keys, got %#v", trustConfig.Keys)
	}
	if trustConfig.Keys[0].ID != "prod-a" || trustConfig.Keys[1].ID != "prod-b" {
		t.Fatalf("expected sorted trust keys, got %#v", trustConfig.Keys)
	}
}

func TestLoadManifestSignerSetFromFile(t *testing.T) {
	dir := t.TempDir()
	keyringPath := filepath.Join(dir, "manifest-keyring.json")
	encoded, err := json.Marshal(ManifestKeyringConfig{
		DefaultKeyID: "prod-a",
		Keys: []ManifestKeyringEntry{
			{
				KeyID:             "prod-a",
				PrivateSeedBase64: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
			},
			{
				KeyID:             "prod-b",
				PrivateSeedBase64: "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI",
			},
		},
	})
	if err != nil {
		t.Fatalf("Marshal(keyring) error = %v", err)
	}
	if err := os.WriteFile(keyringPath, encoded, 0o644); err != nil {
		t.Fatalf("WriteFile(keyring) error = %v", err)
	}

	set, err := LoadManifestSignerSetFromFile(keyringPath)
	if err != nil {
		t.Fatalf("LoadManifestSignerSetFromFile() error = %v", err)
	}
	if set.DefaultKeyID() != "prod-a" {
		t.Fatalf("expected default key id prod-a, got %q", set.DefaultKeyID())
	}
	if _, err := set.SignerForKeyID("prod-b"); err != nil {
		t.Fatalf("SignerForKeyID(prod-b) error = %v", err)
	}
}
