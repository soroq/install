package signing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"soroq/backend/internal/domain"
)

// External signing seam.
//
// Some operators cannot let a private key exist as a file on a build machine: the key lives in a KMS,
// an HSM, or behind a signing service. This is the provider-independent seam for that, and it is
// deliberately NOT an integration with any named provider -- a `soroq_kms` claim is only honest once
// that provider is individually wired and tested.
//
// The contract is narrow on purpose:
//
//   - Soroq hands the helper the EXACT bytes to sign on stdin. Those bytes are ManifestSigningPayload,
//     the same deterministic encoding the local signer uses, so a manifest signed externally verifies
//     identically to one signed locally. Nothing about the manifest is re-encoded on the way out.
//   - The helper returns ONLY an unpadded base64url Ed25519 signature on stdout. Anything else is a
//     refusal, never a best-effort parse.
//   - No seed, key material or signature is ever passed in argv, because argv is world-readable on
//     most systems (`ps`) and routinely captured by CI logs.
//   - The helper's stderr is captured for diagnosis and REDACTED before it is surfaced, since a
//     misconfigured helper is exactly the thing that prints a credential in its error path.
//   - The signature is verified against the configured public key before it is returned. A helper that
//     signs with the wrong key fails here rather than shipping a manifest no device will accept.

// ManifestSignerBackend is what the publish paths actually depend on. The local seed signer and the
// external helper both satisfy it, so callers do not branch on which one is in use.
type ManifestSignerBackend interface {
	SignManifest(manifest domain.PatchManifest) (string, error)
	PublicKeyBase64() string
	KeyID() string
}

// compile-time proof that the existing local signer already satisfies the seam unchanged.
var _ ManifestSignerBackend = (*ManifestSigner)(nil)

const defaultExternalSignerTimeout = 30 * time.Second

// ExternalManifestSigner signs by invoking a helper command that holds the key material.
type ExternalManifestSigner struct {
	command   string
	args      []string
	publicKey ed25519.PublicKey
	keyID     string
	timeout   time.Duration
	// runner is indirected so tests can drive the contract without spawning processes.
	runner func(ctx context.Context, command string, args []string, stdin []byte) (stdout []byte, stderr []byte, err error)
}

// NewExternalManifestSigner builds a signer around a helper command and the public key it is expected
// to sign with. The public key is REQUIRED: without it there is nothing to check the helper against.
func NewExternalManifestSigner(command string, args []string, publicKeyBase64 string, keyID string, timeout time.Duration) (*ExternalManifestSigner, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("external signer command is empty")
	}
	publicKeyBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(publicKeyBase64))
	if err != nil {
		return nil, fmt.Errorf("decode external signer public key: %w", err)
	}
	if len(publicKeyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("external signer public key must be %d bytes, got %d", ed25519.PublicKeySize, len(publicKeyBytes))
	}
	if timeout <= 0 {
		timeout = defaultExternalSignerTimeout
	}
	return &ExternalManifestSigner{
		command:   command,
		args:      append([]string(nil), args...),
		publicKey: ed25519.PublicKey(publicKeyBytes),
		keyID:     normalizeManifestKeyID(keyID, ed25519.PublicKey(publicKeyBytes)),
		timeout:   timeout,
		runner:    runExternalSignerCommand,
	}, nil
}

func (s *ExternalManifestSigner) PublicKeyBase64() string {
	return base64.RawURLEncoding.EncodeToString(s.publicKey)
}

func (s *ExternalManifestSigner) KeyID() string { return s.keyID }

// SignManifest asks the helper to sign the canonical payload, then verifies what it returned.
func (s *ExternalManifestSigner) SignManifest(manifest domain.PatchManifest) (string, error) {
	payload := ManifestSigningPayload(manifest)

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	stdout, stderr, err := s.runner(ctx, s.command, s.args, payload)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("external signer %q timed out after %s%s", s.command, s.timeout, formatSignerStderr(stderr))
		}
		return "", fmt.Errorf("external signer %q failed: %v%s", s.command, err, formatSignerStderr(stderr))
	}

	signature := strings.TrimSpace(string(stdout))
	if signature == "" {
		return "", fmt.Errorf("external signer %q returned no signature%s", s.command, formatSignerStderr(stderr))
	}
	signatureBytes, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return "", fmt.Errorf("external signer %q returned a signature that is not unpadded base64url%s", s.command, formatSignerStderr(stderr))
	}
	if len(signatureBytes) != ed25519.SignatureSize {
		return "", fmt.Errorf("external signer %q returned a %d-byte signature, want %d%s",
			s.command, len(signatureBytes), ed25519.SignatureSize, formatSignerStderr(stderr))
	}
	// The helper may hold a different key than the operator configured. Catch that here, where the
	// message can say so, rather than on a device that simply refuses the patch.
	if !ed25519.Verify(s.publicKey, payload, signatureBytes) {
		return "", fmt.Errorf("external signer %q produced a signature that does not verify against the configured public key "+
			"(key id %s); the helper is signing with a different key%s", s.command, s.keyID, formatSignerStderr(stderr))
	}
	return signature, nil
}

// formatSignerStderr appends redacted helper diagnostics, or nothing when it said nothing.
func formatSignerStderr(stderr []byte) string {
	text := strings.TrimSpace(string(stderr))
	if text == "" {
		return ""
	}
	return "\n  signer stderr: " + RedactSignerOutput(text)
}

// RedactSignerOutput strips credential-shaped values from helper diagnostics. A misconfigured signing
// helper is precisely the kind of program that prints a token or a key in its error path.
func RedactSignerOutput(text string) string {
	redacted := text
	for _, pattern := range signerSecretPatterns {
		redacted = pattern.ReplaceAllString(redacted, "${1}[REDACTED]")
	}
	if len(redacted) > 2000 {
		redacted = redacted[:2000] + "…"
	}
	return redacted
}

func runExternalSignerCommand(ctx context.Context, command string, args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
