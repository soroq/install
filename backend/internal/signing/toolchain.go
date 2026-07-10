package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Toolchain manifest signing (T005) is a NEW, SEPARATE trust domain from BOTH the device engine-lane
// pinned key (signEngineManifest) AND the backend PatchManifest signer (soroq-ed25519-v1). It reuses
// only the MECHANISM (Ed25519 over exact manifest bytes); the key id and the keypair are distinct.
//
//   - The CLI PINS the toolchain PUBLIC key (a committed const in cmd/soroq).
//   - The PRIVATE key is OPERATOR-held: supplied at publish time from an env var or a key file path.
//     It is NEVER committed, NEVER printed, NEVER persisted to state/logs.
//   - Signatures are HEX-encoded (matching the registry's PutToolchainRequest.SignatureHex + the
//     GET manifest.sig convention + the device-engine hex convention) — NOT the base64 the
//     PatchManifest signer uses.
//
// The toolchain manifest layers ON TOP of the engine.json: after the CLI materializes the bundle,
// the UNCHANGED verifyEngineBundle re-verifies the per-file uncompressed SHAs. This signer covers
// only the integrity + authenticity of the hosted toolchain manifest at install time.

// ToolchainSignatureScheme is the scheme tag for the toolchain manifest trust domain. It is distinct
// from ManifestSignatureScheme (the PatchManifest signer) by design.
const ToolchainSignatureScheme = "soroq-toolchain-ed25519-v1"

// DefaultToolchainKeyID is the well-known key id for the genesis toolchain signing key. The CLI pins
// the matching PUBLIC key; the operator holds the matching seed.
const DefaultToolchainKeyID = "soroq-toolchain-kid-v1"

// ToolchainManifestSigner signs the EXACT toolchain manifest bytes with the operator-held private key.
// The seed is held only in memory for the lifetime of this signer and is never exported.
type ToolchainManifestSigner struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	keyID      string
}

// NewToolchainSignerFromSeed builds a signer from a 32-byte Ed25519 seed.
func NewToolchainSignerFromSeed(seed []byte, keyID string) (*ToolchainManifestSigner, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf(
			"toolchain signing seed must be %d bytes, got %d",
			ed25519.SeedSize,
			len(seed),
		)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &ToolchainManifestSigner{
		privateKey: priv,
		publicKey:  pub,
		keyID:      normalizeToolchainKeyID(keyID),
	}, nil
}

// NewToolchainSignerFromSeedString accepts a seed encoded as base64 (raw-url, std, or url) or hex,
// the conventional ways an operator supplies a key via env var or a key file. It NEVER prints the
// decoded value.
func NewToolchainSignerFromSeedString(seedStr string, keyID string) (*ToolchainManifestSigner, error) {
	seed, err := DecodeToolchainSeed(seedStr)
	if err != nil {
		return nil, err
	}
	return NewToolchainSignerFromSeed(seed, keyID)
}

// DecodeToolchainSeed decodes an operator-supplied Ed25519 seed from base64 (raw-url / url / std) or
// hex. The seed must decode to exactly ed25519.SeedSize bytes. The raw value is never logged.
func DecodeToolchainSeed(seedStr string) ([]byte, error) {
	s := strings.TrimSpace(seedStr)
	if s == "" {
		return nil, errors.New("toolchain signing seed is empty")
	}
	for _, dec := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if b, e := dec.DecodeString(s); e == nil && len(b) == ed25519.SeedSize {
			return b, nil
		}
	}
	if b, e := hex.DecodeString(s); e == nil && len(b) == ed25519.SeedSize {
		return b, nil
	}
	return nil, fmt.Errorf("toolchain signing seed must decode (base64 or hex) to %d bytes", ed25519.SeedSize)
}

// GenerateToolchainKeyPair generates a fresh toolchain keypair. Returns the seed (raw-url base64, for
// operator custody) + the public key (hex, for the CLI-pinned const) + the key id.
func GenerateToolchainKeyPair() (seedBase64 string, publicKeyHex string, keyID string, err error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", "", "", fmt.Errorf("generate toolchain signing seed: %w", err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return base64.RawURLEncoding.EncodeToString(seed),
		hex.EncodeToString(pub),
		DefaultToolchainKeyID,
		nil
}

// PublicKeyHex returns the signer's public key as hex (the form pinned in the CLI).
func (s *ToolchainManifestSigner) PublicKeyHex() string {
	return hex.EncodeToString(s.publicKey)
}

// KeyID returns the signer's key id.
func (s *ToolchainManifestSigner) KeyID() string {
	return s.keyID
}

// SignToolchainManifest signs the EXACT manifest bytes and returns a HEX-encoded detached signature.
// The caller MUST sign the same bytes it transmits/stores (no re-marshal between sign and PUT).
func (s *ToolchainManifestSigner) SignToolchainManifest(manifestBytes []byte) (string, error) {
	if len(manifestBytes) == 0 {
		return "", errors.New("toolchain manifest bytes are empty")
	}
	sig := ed25519.Sign(s.privateKey, manifestBytes)
	return hex.EncodeToString(sig), nil
}

// VerifyToolchainManifestSignature verifies a HEX detached signature over the EXACT manifest bytes
// against a pinned public key (HEX). It returns a precise error on any failure (no partial trust).
func VerifyToolchainManifestSignature(manifestBytes []byte, signatureHex string, pinnedPublicKeyHex string) error {
	if len(manifestBytes) == 0 {
		return errors.New("toolchain manifest bytes are empty")
	}
	pub, err := hex.DecodeString(strings.TrimSpace(pinnedPublicKeyHex))
	if err != nil {
		return fmt.Errorf("decode pinned toolchain public key: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf(
			"pinned toolchain public key must be %d bytes, got %d",
			ed25519.PublicKeySize,
			len(pub),
		)
	}
	sig, err := hex.DecodeString(strings.TrimSpace(signatureHex))
	if err != nil {
		return fmt.Errorf("decode toolchain manifest signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf(
			"toolchain manifest signature must be %d bytes, got %d",
			ed25519.SignatureSize,
			len(sig),
		)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), manifestBytes, sig) {
		return errors.New("toolchain manifest signature is invalid (does not match the pinned toolchain public key)")
	}
	return nil
}

func normalizeToolchainKeyID(keyID string) string {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return DefaultToolchainKeyID
	}
	return keyID
}
