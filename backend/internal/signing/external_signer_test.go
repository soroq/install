package signing

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"soroq/backend/internal/domain"
)

// A disposable fake signer stands in for a KMS/HSM helper. No provider is integrated here, and none is
// claimed: this proves the SEAM, which is what a real integration would plug into.

func externalSeamManifest() domain.PatchManifest {
	return domain.PatchManifest{
		PatchID: "patch-1", PatchNumber: 1, RuntimeID: "runtime-1", ReleaseID: "release-1",
		Channel: "stable", Kind: domain.PatchKindExperimentalNativeAOT,
		ActivationMode: domain.ActivationNextColdStart,
	}
}

func newFakeExternalSigner(t *testing.T, respond func(payload []byte, priv ed25519.PrivateKey) (stdout, stderr []byte, err error)) (*ExternalManifestSigner, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	signer, err := NewExternalManifestSigner("fake-helper", []string{"--mode", "sign"},
		base64.RawURLEncoding.EncodeToString(pub), "kms-key-1", 2*time.Second)
	if err != nil {
		t.Fatalf("NewExternalManifestSigner: %v", err)
	}
	signer.runner = func(_ context.Context, _ string, _ []string, stdin []byte) ([]byte, []byte, error) {
		return respond(stdin, priv)
	}
	return signer, priv
}

// The happy path must produce a signature indistinguishable from a locally-signed one.
func TestExternalSignerProducesAVerifiableSignature(t *testing.T) {
	signer, _ := newFakeExternalSigner(t, func(payload []byte, priv ed25519.PrivateKey) ([]byte, []byte, error) {
		return []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload))), nil, nil
	})
	manifest := externalSeamManifest()
	sig, err := signer.SignManifest(manifest)
	if err != nil {
		t.Fatalf("SignManifest: %v", err)
	}
	manifest.Signature = &sig
	if err := VerifyManifestSignature(manifest, signer.PublicKeyBase64()); err != nil {
		t.Fatalf("an externally-signed manifest must verify exactly like a locally-signed one: %v", err)
	}
}

// The helper receives the SAME deterministic bytes the local signer uses, or the two paths would
// produce manifests that do not verify interchangeably.
func TestExternalSignerSendsTheCanonicalPayload(t *testing.T) {
	manifest := externalSeamManifest()
	want := ManifestSigningPayload(manifest)
	var got []byte
	signer, _ := newFakeExternalSigner(t, func(payload []byte, priv ed25519.PrivateKey) ([]byte, []byte, error) {
		got = append([]byte(nil), payload...)
		return []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload))), nil, nil
	})
	if _, err := signer.SignManifest(manifest); err != nil {
		t.Fatalf("SignManifest: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("helper received different bytes than the local signer would sign")
	}
}

// A helper holding the WRONG key must fail here, with a message that says so -- not on a device that
// silently refuses the patch later.
func TestExternalSignerRejectsAWrongKeySignature(t *testing.T) {
	signer, _ := newFakeExternalSigner(t, func(payload []byte, _ ed25519.PrivateKey) ([]byte, []byte, error) {
		_, otherPriv, _ := ed25519.GenerateKey(nil)
		return []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(otherPriv, payload))), nil, nil
	})
	_, err := signer.SignManifest(externalSeamManifest())
	if err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("a wrong-key signature must be refused with a clear message, got %v", err)
	}
}

func TestExternalSignerRejectsMalformedOutput(t *testing.T) {
	for name, out := range map[string]string{
		"not base64":   "!!!!not-base64!!!!",
		"wrong length": base64.RawURLEncoding.EncodeToString([]byte("too-short")),
		"empty":        "",
	} {
		t.Run(name, func(t *testing.T) {
			signer, _ := newFakeExternalSigner(t, func([]byte, ed25519.PrivateKey) ([]byte, []byte, error) {
				return []byte(out), nil, nil
			})
			if _, err := signer.SignManifest(externalSeamManifest()); err == nil {
				t.Fatal("malformed signer output must be refused, never best-effort parsed")
			}
		})
	}
}

func TestExternalSignerReportsTimeout(t *testing.T) {
	signer, _ := newFakeExternalSigner(t, func([]byte, ed25519.PrivateKey) ([]byte, []byte, error) {
		return nil, nil, context.DeadlineExceeded
	})
	signer.timeout = 10 * time.Millisecond
	signer.runner = func(ctx context.Context, _ string, _ []string, _ []byte) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	_, err := signer.SignManifest(externalSeamManifest())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("a hung helper must time out with a clear message, got %v", err)
	}
}

// A misconfigured helper is exactly the program that prints a credential on its error path.
func TestExternalSignerRedactsHelperStderr(t *testing.T) {
	const secret = "super-secret-token-value"
	signer, _ := newFakeExternalSigner(t, func([]byte, ed25519.PrivateKey) ([]byte, []byte, error) {
		return nil, []byte("auth failed\nAuthorization: Bearer " + secret + "\ntoken=" + secret), errors.New("exit status 1")
	})
	_, err := signer.SignManifest(externalSeamManifest())
	if err == nil {
		t.Fatal("a failing helper must surface an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("helper stderr leaked a credential into the error:\n%s", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("stderr should be surfaced redacted, got:\n%s", err)
	}
}

// No key material may reach argv: argv is world-readable via ps and is routinely captured by CI.
func TestExternalSignerPassesNoSecretsInArgv(t *testing.T) {
	signer, _ := newFakeExternalSigner(t, func(payload []byte, priv ed25519.PrivateKey) ([]byte, []byte, error) {
		return []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload))), nil, nil
	})
	var sawArgs []string
	inner := signer.runner
	signer.runner = func(ctx context.Context, cmd string, args []string, stdin []byte) ([]byte, []byte, error) {
		sawArgs = append([]string(nil), args...)
		return inner(ctx, cmd, args, stdin)
	}
	if _, err := signer.SignManifest(externalSeamManifest()); err != nil {
		t.Fatalf("SignManifest: %v", err)
	}
	for _, a := range sawArgs {
		if strings.Contains(a, "seed") || strings.Contains(a, "BEGIN") || len(a) > 120 {
			t.Fatalf("argv carried something key-shaped: %q", a)
		}
	}
	// The payload travels on stdin, never as an argument.
	for _, a := range sawArgs {
		if strings.Contains(a, "patch-1") {
			t.Fatalf("the signing payload must go on stdin, not argv: %q", a)
		}
	}
}

// The local seed signer must satisfy the same seam, unchanged, so callers never branch.
func TestLocalSignerSatisfiesTheSameSeam(t *testing.T) {
	seedB64, pubB64, keyID, err := GenerateManifestKeyPair()
	if err != nil {
		t.Fatalf("GenerateManifestKeyPair: %v", err)
	}
	local, err := NewManifestSignerFromSeedBase64(seedB64, keyID)
	if err != nil {
		t.Fatalf("NewManifestSignerFromSeedBase64: %v", err)
	}
	var backend ManifestSignerBackend = local
	manifest := externalSeamManifest()
	sig, err := backend.SignManifest(manifest)
	if err != nil {
		t.Fatalf("SignManifest: %v", err)
	}
	manifest.Signature = &sig
	if err := VerifyManifestSignature(manifest, pubB64); err != nil {
		t.Fatalf("local signer through the seam must still verify: %v", err)
	}
}
