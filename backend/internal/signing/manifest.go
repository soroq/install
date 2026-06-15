package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"soroq/backend/internal/domain"
)

const ManifestSignatureScheme = "soroq-ed25519-v1"

type ManifestSigner struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	keyID      string
}

func NewManifestSignerFromSeedBase64(seedBase64 string, keyID string) (*ManifestSigner, error) {
	seedBase64 = strings.TrimSpace(seedBase64)
	if seedBase64 == "" {
		return nil, errors.New("manifest signing seed is empty")
	}

	seed, err := base64.RawURLEncoding.DecodeString(seedBase64)
	if err != nil {
		return nil, fmt.Errorf("decode manifest signing seed: %w", err)
	}
	return NewManifestSignerFromSeed(seed, keyID)
}

func NewManifestSignerFromSeed(seed []byte, keyID string) (*ManifestSigner, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf(
			"manifest signing seed must be %d bytes, got %d",
			ed25519.SeedSize,
			len(seed),
		)
	}

	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &ManifestSigner{
		privateKey: privateKey,
		publicKey:  publicKey,
		keyID:      normalizeManifestKeyID(keyID, publicKey),
	}, nil
}

func GenerateManifestKeyPair() (
	privateSeedBase64 string,
	publicKeyBase64 string,
	keyID string,
	err error,
) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", "", "", fmt.Errorf("generate manifest signing seed: %w", err)
	}

	publicKey := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	privateSeedBase64 = base64.RawURLEncoding.EncodeToString(seed)
	publicKeyBase64 = base64.RawURLEncoding.EncodeToString(publicKey)
	keyID = normalizeManifestKeyID("", publicKey)
	return privateSeedBase64, publicKeyBase64, keyID, nil
}

func (s *ManifestSigner) PublicKeyBase64() string {
	return base64.RawURLEncoding.EncodeToString(s.publicKey)
}

func (s *ManifestSigner) KeyID() string {
	return s.keyID
}

func (s *ManifestSigner) SignManifest(manifest domain.PatchManifest) (string, error) {
	signature := ed25519.Sign(s.privateKey, ManifestSigningPayload(manifest))
	return base64.RawURLEncoding.EncodeToString(signature), nil
}

func normalizeManifestKeyID(keyID string, publicKey ed25519.PublicKey) string {
	keyID = strings.TrimSpace(keyID)
	if keyID != "" {
		return keyID
	}

	sum := sha256.Sum256(publicKey)
	return "soroq-kid-" + hex.EncodeToString(sum[:8])
}

func VerifyManifestSignature(
	manifest domain.PatchManifest,
	publicKeyBase64 string,
) error {
	if manifest.Signature == nil || strings.TrimSpace(*manifest.Signature) == "" {
		return errors.New("manifest signature is missing")
	}

	publicKeyBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(publicKeyBase64))
	if err != nil {
		return fmt.Errorf("decode manifest public key: %w", err)
	}
	if len(publicKeyBytes) != ed25519.PublicKeySize {
		return fmt.Errorf(
			"manifest public key must be %d bytes, got %d",
			ed25519.PublicKeySize,
			len(publicKeyBytes),
		)
	}

	signatureBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(*manifest.Signature))
	if err != nil {
		return fmt.Errorf("decode manifest signature: %w", err)
	}
	if len(signatureBytes) != ed25519.SignatureSize {
		return fmt.Errorf(
			"manifest signature must be %d bytes, got %d",
			ed25519.SignatureSize,
			len(signatureBytes),
		)
	}

	if !ed25519.Verify(ed25519.PublicKey(publicKeyBytes), ManifestSigningPayload(manifest), signatureBytes) {
		return errors.New("manifest signature is invalid")
	}
	return nil
}

func ManifestSigningPayload(manifest domain.PatchManifest) []byte {
	lines := []string{
		ManifestSignatureScheme,
		"patch_id=" + manifest.PatchID,
		"patch_number=" + strconv.Itoa(manifest.PatchNumber),
		"runtime_id=" + manifest.RuntimeID,
		"release_id=" + manifest.ReleaseID,
		"channel=" + manifest.Channel,
		"kind=" + string(manifest.Kind),
		"activation_mode=" + string(manifest.ActivationMode),
		"artifact_url=" + manifest.Artifact.URL,
		"artifact_sha256=" + manifest.Artifact.SHA256,
		"artifact_size_bytes=" + strconv.FormatUint(manifest.Artifact.SizeBytes, 10),
	}
	return []byte(strings.Join(lines, "\n"))
}
