package signing

import (
	"strings"
	"testing"
)

func TestToolchainSignVerifyRoundTrip(t *testing.T) {
	seedB64, pubHex, keyID, err := GenerateToolchainKeyPair()
	if err != nil {
		t.Fatalf("GenerateToolchainKeyPair: %v", err)
	}
	if keyID != DefaultToolchainKeyID {
		t.Fatalf("keyID = %q, want %q", keyID, DefaultToolchainKeyID)
	}

	signer, err := NewToolchainSignerFromSeedString(seedB64, "")
	if err != nil {
		t.Fatalf("NewToolchainSignerFromSeedString: %v", err)
	}
	if signer.PublicKeyHex() != pubHex {
		t.Fatalf("PublicKeyHex mismatch: signer=%s generated=%s", signer.PublicKeyHex(), pubHex)
	}

	manifest := []byte(`{"schema":"soroq.toolchain.v1","soroq_toolchain_version":"v-test"}`)
	sigHex, err := signer.SignToolchainManifest(manifest)
	if err != nil {
		t.Fatalf("SignToolchainManifest: %v", err)
	}
	if err := VerifyToolchainManifestSignature(manifest, sigHex, pubHex); err != nil {
		t.Fatalf("VerifyToolchainManifestSignature (valid): %v", err)
	}
}

func TestToolchainVerifyRejectsTamperedManifest(t *testing.T) {
	seedB64, pubHex, _, _ := GenerateToolchainKeyPair()
	signer, _ := NewToolchainSignerFromSeedString(seedB64, "")
	manifest := []byte(`{"schema":"soroq.toolchain.v1","v":1}`)
	sigHex, _ := signer.SignToolchainManifest(manifest)

	tampered := []byte(`{"schema":"soroq.toolchain.v1","v":2}`)
	if err := VerifyToolchainManifestSignature(tampered, sigHex, pubHex); err == nil {
		t.Fatal("expected verification to fail for a tampered manifest, got nil")
	}
}

func TestToolchainVerifyRejectsWrongKey(t *testing.T) {
	seedB64A, _, _, _ := GenerateToolchainKeyPair()
	signerA, _ := NewToolchainSignerFromSeedString(seedB64A, "")
	manifest := []byte(`{"schema":"soroq.toolchain.v1"}`)
	sigHex, _ := signerA.SignToolchainManifest(manifest)

	// A signature from key A must NOT verify against key B's pinned pubkey.
	_, pubHexB, _, _ := GenerateToolchainKeyPair()
	if err := VerifyToolchainManifestSignature(manifest, sigHex, pubHexB); err == nil {
		t.Fatal("expected verification to fail against the wrong pinned public key, got nil")
	}
}

func TestToolchainSeedDecodeFormats(t *testing.T) {
	seedB64, _, _, _ := GenerateToolchainKeyPair()
	// raw-url base64 (the generated form)
	if _, err := DecodeToolchainSeed(seedB64); err != nil {
		t.Fatalf("decode raw-url base64 seed: %v", err)
	}
	// hex form of the same seed must also decode
	raw, _ := DecodeToolchainSeed(seedB64)
	hexSeed := toHex(raw)
	if _, err := DecodeToolchainSeed(hexSeed); err != nil {
		t.Fatalf("decode hex seed: %v", err)
	}
	// empty + garbage are rejected
	if _, err := DecodeToolchainSeed(""); err == nil {
		t.Fatal("expected empty seed to be rejected")
	}
	if _, err := DecodeToolchainSeed("not-a-valid-seed"); err == nil {
		t.Fatal("expected garbage seed to be rejected")
	}
}

func TestToolchainSchemeDistinctFromPatchScheme(t *testing.T) {
	if ToolchainSignatureScheme == ManifestSignatureScheme {
		t.Fatal("toolchain signature scheme must be a separate trust domain from the patch manifest scheme")
	}
	if !strings.Contains(ToolchainSignatureScheme, "toolchain") {
		t.Fatalf("toolchain scheme %q should be self-identifying", ToolchainSignatureScheme)
	}
}

func toHex(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0x0f]
	}
	return string(out)
}
