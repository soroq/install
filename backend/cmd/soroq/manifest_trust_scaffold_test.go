package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/signing"
)

// bareSoroqYAML is a minimal soroq.yaml with runtime_id_strategy: manifest_trust_v1 but NO
// manifest_trust block — exactly the fresh-user shape that makes the fork throw
// `Expected soroq.yaml to define "manifest_trust"`.
const bareSoroqYAML = "app_id: com.example.fresh\nchannel: stable\nruntime_id_strategy: manifest_trust_v1\n"

func TestEnsureManifestTrustScaffoldsWhenAbsent(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), bareSoroqYAML)

	pubHex, err := ensureManifestTrust(projectDir)
	if err != nil {
		t.Fatalf("ensureManifestTrust() error = %v", err)
	}
	if pubHex == "" {
		t.Fatalf("expected a non-empty public key hex")
	}

	// soroq.yaml gained a valid manifest_trust block.
	yamlBytes, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		t.Fatalf("read soroq.yaml: %v", err)
	}
	trust, err := parseSoroqManifestTrust(yamlBytes)
	if err != nil {
		t.Fatalf("parseSoroqManifestTrust: %v", err)
	}
	if trust == nil {
		t.Fatalf("expected a manifest_trust block, got none:\n%s", yamlBytes)
	}
	if trust.KeysetVersion == nil || *trust.KeysetVersion != 1 {
		t.Fatalf("expected keyset_version 1, got %v", trust.KeysetVersion)
	}
	if len(trust.Keys) != 1 {
		t.Fatalf("expected exactly one key, got %d", len(trust.Keys))
	}
	if trust.Keys[0].ID == nil || strings.TrimSpace(*trust.Keys[0].ID) == "" {
		t.Fatalf("expected a non-empty key id, got %v", trust.Keys[0].ID)
	}
	if want := "com.example.fresh-signing"; *trust.Keys[0].ID != want {
		t.Fatalf("key id = %q, want %q", *trust.Keys[0].ID, want)
	}
	pubBytes, derr := base64.RawURLEncoding.DecodeString(trust.Keys[0].PublicKey)
	if derr != nil {
		t.Fatalf("public_key not decodable base64url: %v", derr)
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		t.Fatalf("public_key decodes to %d bytes, want %d", len(pubBytes), ed25519.PublicKeySize)
	}

	// The seed exists, is mode 0600, and is a valid Ed25519 seed.
	seedPath := filepath.Join(projectDir, ".soroq", "manifest_signing_key.seed")
	info, err := os.Stat(seedPath)
	if err != nil {
		t.Fatalf("stat seed: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("seed perms = %o, want 0600", perm)
	}
	seedRaw, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	seedBase64 := strings.TrimSpace(string(seedRaw))
	seed, derr := base64.RawURLEncoding.DecodeString(seedBase64)
	if derr != nil {
		t.Fatalf("seed not decodable base64url: %v", derr)
	}
	if len(seed) != ed25519.SeedSize {
		t.Fatalf("seed decodes to %d bytes, want %d", len(seed), ed25519.SeedSize)
	}

	// The generated public_key in the yaml matches the seed's real Ed25519 pubkey (catches base64
	// padding/encoding bugs).
	derivedPub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	derivedPubBase64 := base64.RawURLEncoding.EncodeToString(derivedPub)
	if derivedPubBase64 != trust.Keys[0].PublicKey {
		t.Fatalf("yaml public_key %q != seed-derived pubkey %q", trust.Keys[0].PublicKey, derivedPubBase64)
	}

	// .gitignore ignores .soroq/.
	gitignore, err := os.ReadFile(filepath.Join(projectDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(gitignore), ".soroq/") {
		t.Fatalf(".gitignore missing .soroq/ line:\n%s", gitignore)
	}

	// soroq.yaml MUST NOT contain any private seed material.
	if strings.Contains(string(yamlBytes), seedBase64) {
		t.Fatalf("soroq.yaml leaked the private seed material")
	}
}

func TestEnsureManifestTrustPreservesValidExisting(t *testing.T) {
	projectDir := t.TempDir()
	// A valid, real Ed25519 key so decodability holds.
	seedBase64, pubBase64, _, err := signing.GenerateManifestKeyPair()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	_ = seedBase64
	original := "app_id: com.example.keep\nchannel: stable\nruntime_id_strategy: manifest_trust_v1\n" +
		"manifest_trust:\n  keyset_version: 3\n  keys:\n    - id: soroq-keep-real\n      public_key: " + pubBase64 + "\n"
	soroqPath := filepath.Join(projectDir, "soroq.yaml")
	writeFile(t, soroqPath, original)

	pubHex, err := ensureManifestTrust(projectDir)
	if err != nil {
		t.Fatalf("ensureManifestTrust() error = %v", err)
	}
	// Returns the existing key's pubkey hex.
	pubBytes, _ := base64.RawURLEncoding.DecodeString(pubBase64)
	if want := hex.EncodeToString(pubBytes); pubHex != want {
		t.Fatalf("pubHex = %q, want %q", pubHex, want)
	}
	// File is byte-for-byte unchanged.
	after, err := os.ReadFile(soroqPath)
	if err != nil {
		t.Fatalf("read soroq.yaml: %v", err)
	}
	if string(after) != original {
		t.Fatalf("soroq.yaml was modified:\nbefore:\n%s\nafter:\n%s", original, after)
	}
	// No .soroq/ seed was created for the preserve path.
	if _, err := os.Stat(filepath.Join(projectDir, ".soroq", "manifest_signing_key.seed")); !os.IsNotExist(err) {
		t.Fatalf("expected no seed file on the preserve path, got err=%v", err)
	}
}

func TestEnsureManifestTrustRejectsInvalidExisting(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantSubs []string
	}{
		{
			name: "missing public_key",
			yaml: "app_id: a\nchannel: stable\nruntime_id_strategy: manifest_trust_v1\n" +
				"manifest_trust:\n  keyset_version: 1\n  keys:\n    - id: only-id\n",
			wantSubs: []string{"manifest_trust.keys[0]", "public_key"},
		},
		{
			name: "missing keyset_version",
			yaml: "app_id: a\nchannel: stable\nruntime_id_strategy: manifest_trust_v1\n" +
				"manifest_trust:\n  keys:\n    - id: only-id\n      public_key: dGVzdA\n",
			wantSubs: []string{"keyset_version"},
		},
		{
			name: "missing id",
			yaml: "app_id: a\nchannel: stable\nruntime_id_strategy: manifest_trust_v1\n" +
				"manifest_trust:\n  keyset_version: 1\n  keys:\n    - public_key: dGVzdA\n",
			wantSubs: []string{"manifest_trust.keys[0]", `"id"`},
		},
		{
			name: "undecodable public_key",
			yaml: "app_id: a\nchannel: stable\nruntime_id_strategy: manifest_trust_v1\n" +
				"manifest_trust:\n  keyset_version: 1\n  keys:\n    - id: only-id\n      public_key: not+valid/base64url==\n",
			wantSubs: []string{"manifest_trust.keys[0]", "base64url"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projectDir := t.TempDir()
			soroqPath := filepath.Join(projectDir, "soroq.yaml")
			writeFile(t, soroqPath, tc.yaml)

			_, err := ensureManifestTrust(projectDir)
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("error %q missing expected substring %q", err.Error(), sub)
				}
			}
			// File must be unchanged.
			after, rerr := os.ReadFile(soroqPath)
			if rerr != nil {
				t.Fatalf("read soroq.yaml: %v", rerr)
			}
			if string(after) != tc.yaml {
				t.Fatalf("soroq.yaml was modified on the invalid path")
			}
		})
	}
}

func TestEnsureManifestTrustIsIdempotent(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), bareSoroqYAML)

	firstHex, err := ensureManifestTrust(projectDir)
	if err != nil {
		t.Fatalf("first ensureManifestTrust() error = %v", err)
	}
	afterFirst, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		t.Fatalf("read soroq.yaml: %v", err)
	}

	secondHex, err := ensureManifestTrust(projectDir)
	if err != nil {
		t.Fatalf("second ensureManifestTrust() error = %v", err)
	}
	afterSecond, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		t.Fatalf("read soroq.yaml: %v", err)
	}

	if firstHex != secondHex {
		t.Fatalf("idempotency: key changed between calls (%q -> %q)", firstHex, secondHex)
	}
	if string(afterFirst) != string(afterSecond) {
		t.Fatalf("idempotency: soroq.yaml was rewritten on the second call")
	}
}
