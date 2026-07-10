package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/signing"
)

// TestToolchainPinnedKey_IsRotatedProdKey_NotCommittedTestSeed is the FLIPPED guard (T008/WS2 rotation
// DONE). It was previously TestToolchainPinnedKey_IsCommittedTestSeed_ProdMustRotate, which asserted the
// pinned key WAS the committed test seed's pubkey and served as the tripwire demanding rotation.
//
// Rotation has now happened: toolchainPinnedPublicKeyHex is a real PRODUCTION public key whose seed was
// minted via `soroq toolchain keygen`, kept out-of-band, and is NOT committed. This guard now asserts the
// OPPOSITE — the production pinned const NO LONGER matches the (retired but still-present) committed test
// seed testToolchainSeedB64 — so a regression that re-pinned the committed seed would fail here. It checks
// the const directly (the production trust anchor), NOT the test-overridable accessor.
// See docs/toolchain-signing-key-rotation.md.
func TestToolchainPinnedKey_IsRotatedProdKey_NotCommittedTestSeed(t *testing.T) {
	signer, err := signing.NewToolchainSignerFromSeedString(testToolchainSeedB64, toolchainPinnedKeyID)
	if err != nil {
		t.Fatalf("derive pubkey from the committed test seed: %v", err)
	}
	if strings.EqualFold(signer.PublicKeyHex(), toolchainPinnedPublicKeyHex) {
		t.Fatalf("REGRESSION: the production pinned toolchain key is the COMMITTED test seed's pubkey "+
			"again — that seed is in the repo, so this is NOT production-safe. Re-pin to a rotated key "+
			"whose seed is out-of-band (see docs/toolchain-signing-key-rotation.md).\n  pinned=%s\n  test-seed-pubkey=%s",
			toolchainPinnedPublicKeyHex, signer.PublicKeyHex())
	}
	t.Logf("ROTATED: toolchainPinnedPublicKeyHex (%s) is a production key whose seed is NOT committed; "+
		"it no longer matches the retired committed test seed (pubkey %s).",
		toolchainPinnedPublicKeyHex, signer.PublicKeyHex())
}

// TestToolchainKeygen_WritesSeed0600_NeverPrintsSeed proves the operator keygen entrypoint:
//   - writes the PRIVATE seed to the --out file with 0600 perms,
//   - the seed value NEVER appears on stdout (the human-readable form),
//   - the printed public key hex matches the seed actually written to disk.
func TestToolchainKeygen_WritesSeed0600_NeverPrintsSeed(t *testing.T) {
	seedPath := filepath.Join(t.TempDir(), "toolchain.seed")

	var out bytes.Buffer
	pubHex, keyID, err := keygenToolchain(&out, seedPath, false, false)
	if err != nil {
		t.Fatalf("keygenToolchain: %v", err)
	}
	if keyID != toolchainPinnedKeyID {
		t.Fatalf("key id = %q, want %q", keyID, toolchainPinnedKeyID)
	}

	// File perms must be exactly 0600.
	info, err := os.Stat(seedPath)
	if err != nil {
		t.Fatalf("stat seed file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("seed file perms = %o, want 0600", perm)
	}

	// Read the seed back and assert it NEVER appears on stdout (by value, not by eye).
	seedRaw, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("read seed file: %v", err)
	}
	seedStr := strings.TrimSpace(string(seedRaw))
	if seedStr == "" {
		t.Fatal("seed file is empty")
	}
	if strings.Contains(out.String(), seedStr) {
		t.Fatalf("SEED LEAKED: the private seed appeared on keygen stdout output")
	}

	// The printed public key must correspond to the seed actually written to disk.
	fromFile, err := signing.NewToolchainSignerFromSeedString(seedStr, toolchainPinnedKeyID)
	if err != nil {
		t.Fatalf("decode written seed: %v", err)
	}
	if !strings.EqualFold(fromFile.PublicKeyHex(), pubHex) {
		t.Fatalf("printed pubkey %q != pubkey of the written seed %q", pubHex, fromFile.PublicKeyHex())
	}

	// stdout must carry the pubkey + key id (the operator-facing, public-only output).
	if !strings.Contains(out.String(), pubHex) || !strings.Contains(out.String(), keyID) {
		t.Fatalf("keygen stdout missing pubkey/keyid:\n%s", out.String())
	}
}

// TestToolchainKeygen_RefusesClobberWithoutForce proves the seed file is not silently overwritten.
func TestToolchainKeygen_RefusesClobberWithoutForce(t *testing.T) {
	seedPath := filepath.Join(t.TempDir(), "toolchain.seed")

	var out bytes.Buffer
	if _, _, err := keygenToolchain(&out, seedPath, false, false); err != nil {
		t.Fatalf("first keygen: %v", err)
	}
	first, _ := os.ReadFile(seedPath)

	// Second run without --force must refuse and leave the existing seed untouched.
	out.Reset()
	if _, _, err := keygenToolchain(&out, seedPath, false, false); err == nil {
		t.Fatal("expected refusal to overwrite existing seed without --force")
	}
	after, _ := os.ReadFile(seedPath)
	if !bytes.Equal(first, after) {
		t.Fatal("existing seed was modified despite no --force")
	}

	// With --force, a fresh keypair replaces it.
	out.Reset()
	if _, _, err := keygenToolchain(&out, seedPath, true, false); err != nil {
		t.Fatalf("forced keygen: %v", err)
	}
	forced, _ := os.ReadFile(seedPath)
	if bytes.Equal(first, forced) {
		t.Fatal("--force did not regenerate the seed")
	}
	if perm := mustPerm(t, seedPath); perm != 0o600 {
		t.Fatalf("forced seed file perms = %o, want 0600", perm)
	}
}

func mustPerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
