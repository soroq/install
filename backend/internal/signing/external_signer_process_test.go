package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"soroq/backend/internal/domain"
)

// D3 evidence: the external signing seam driven by a REAL separate process.
//
// The existing external_signer_test.go replaces `runner`, so it proves the contract without ever
// spawning anything. This file does not: it compiles a standalone program (soroq-kms-shim), lets THAT
// program generate and hold an Ed25519 key that this process never receives, and drives the seam
// through its default runner -- a real fork/exec, real stdin, real stdout, real argv, real stderr.
//
// BOUNDARY, stated here so it cannot be misread from the test names: this is a real external signing
// process, invoked exactly as a KMS/HSM shim would be. It is NOT a cloud KMS. No AWS/GCP/Azure/Vault
// provider is contacted, configured or claimed; that needs operator credentials which do not exist on
// this machine. What is proven is the contract every such provider would have to satisfy.

// ---------------------------------------------------------------------------------------------
// The signer program.
//
// Kept as source in this test file so the exact program under test is reproducible from the repo,
// and written to a temp dir + `go build` at test time so what runs is a compiled binary rather than
// an interpreted stand-in. It is stdlib-only.
//
// Its key discipline is the point of the exercise:
//   - the key is CREATED by the shim, inside the shim's own key directory, mode 0600;
//   - only the PUBLIC key is ever printed (mode=keygen);
//   - argv carries a key REFERENCE (a directory and an alias, KMS-ARN shaped), never key material;
//   - the bytes to sign arrive only on stdin.
const kmsShimSource = `package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// soroq-kms-shim: a stand-in for an external signing service (KMS/HSM/signing daemon).
// The key material lives on THIS side of the pipe and is never printed, never in argv, never returned.

type record struct {
	Argv        []string ` + "`json:\"argv\"`" + `
	Env         []string ` + "`json:\"env\"`" + `
	StdinLen    int      ` + "`json:\"stdin_len\"`" + `
	StdinSHA256 string   ` + "`json:\"stdin_sha256\"`" + `
	StdinBase64 string   ` + "`json:\"stdin_base64\"`" + `
}

func main() {
	mode := flag.String("mode", "sign", "keygen or sign")
	keyDir := flag.String("key-dir", "", "directory holding key files; a reference, not key material")
	keyRef := flag.String("key-ref", "", "key alias within key-dir; a reference, not key material")
	recordPath := flag.String("record", "", "optional path to record what this process actually received")
	substitute := flag.String("substitute-file", "", "optional alternate payload to sign instead of stdin")
	fault := flag.String("fault", "none", "fault injection mode")
	flag.Parse()

	if err := run(*mode, *keyDir, *keyRef, *recordPath, *substitute, *fault); err != nil {
		fmt.Fprintln(os.Stderr, "soroq-kms-shim: "+err.Error())
		os.Exit(2)
	}
}

func keyPath(dir, ref string) string { return filepath.Join(dir, ref+".key") }

func loadOrCreateKey(dir, ref string) (ed25519.PrivateKey, error) {
	path := keyPath(dir, ref)
	raw, err := os.ReadFile(path)
	if err == nil {
		seed, decodeErr := base64.RawURLEncoding.DecodeString(string(raw))
		if decodeErr != nil {
			return nil, decodeErr
		}
		if len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("stored seed for %q is %d bytes, want %d", ref, len(seed), ed25519.SeedSize)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.RawURLEncoding.EncodeToString(seed)), 0o600); err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func run(mode, keyDir, keyRef, recordPath, substitutePath, fault string) error {
	if keyDir == "" || keyRef == "" {
		return fmt.Errorf("--key-dir and --key-ref are required")
	}
	key, err := loadOrCreateKey(keyDir, keyRef)
	if err != nil {
		return err
	}

	if mode == "keygen" {
		// ONLY the public half ever crosses back.
		fmt.Print(base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
		return nil
	}
	if mode != "sign" {
		return fmt.Errorf("unknown mode %q", mode)
	}

	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	if recordPath != "" {
		sum := sha256.Sum256(payload)
		entry := record{
			Argv:        os.Args,
			Env:         os.Environ(),
			StdinLen:    len(payload),
			StdinSHA256: hex.EncodeToString(sum[:]),
			StdinBase64: base64.RawURLEncoding.EncodeToString(payload),
		}
		encoded, err := json.MarshalIndent(entry, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(recordPath, encoded, 0o600); err != nil {
			return err
		}
	}

	switch fault {
	case "none":
	case "empty":
		// A helper that succeeds and says nothing.
		return nil
	case "truncated":
		sig := ed25519.Sign(key, payload)
		fmt.Print(base64.RawURLEncoding.EncodeToString(sig[:ed25519.SignatureSize-1]))
		return nil
	case "garbage":
		fmt.Print("this is not a base64url signature!!")
		return nil
	case "exit-nonzero":
		fmt.Fprintln(os.Stderr, "soroq-kms-shim: upstream signing service refused")
		os.Exit(3)
	case "substitute-trailing-newline":
		// Valid signature, CORRECT key, DIFFERENT bytes: the canonical payload plus a trailing
		// newline, which is what a helper that pipes through a line-oriented tool would produce.
		fmt.Print(base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append(append([]byte{}, payload...), '\n'))))
		return nil
	case "substitute-payload":
		// Valid signature, CORRECT key, an ENTIRELY DIFFERENT manifest payload.
		other, err := os.ReadFile(substitutePath)
		if err != nil {
			return err
		}
		fmt.Print(base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, other)))
		return nil
	case "leak-stderr":
		// The failure mode this seam's redaction exists for: a misconfigured helper printing its
		// credentials on its error path. It prints the REAL seed, labelled.
		seed := base64.RawURLEncoding.EncodeToString(key.Seed())
		fmt.Fprintln(os.Stderr, "soroq-kms-shim: request failed")
		fmt.Fprintln(os.Stderr, "  Authorization: Bearer sq_live_tok_D3PROOF_qX7fLm2v")
		fmt.Fprintln(os.Stderr, "  seed="+seed)
		fmt.Fprintln(os.Stderr, "  endpoint=https://signer.invalid/sign?token=sq_qs_D3PROOF_9aZ")
		os.Exit(4)
	case "leak-stderr-bare":
		// The STATED limit: an unlabelled opaque string carries no shape to key a redactor on.
		fmt.Fprintln(os.Stderr, "soroq-kms-shim: "+base64.RawURLEncoding.EncodeToString(key.Seed()))
		os.Exit(5)
	default:
		return fmt.Errorf("unknown fault %q", fault)
	}

	fmt.Print(base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)))
	return nil
}
`

// ---------------------------------------------------------------------------------------------
// Harness

type shimRecord struct {
	Argv        []string `json:"argv"`
	Env         []string `json:"env"`
	StdinLen    int      `json:"stdin_len"`
	StdinSHA256 string   `json:"stdin_sha256"`
	StdinBase64 string   `json:"stdin_base64"`
}

func buildKMSShim(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("the go toolchain is required to build the external signer program: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(kmsShimSource), 0o644); err != nil {
		t.Fatalf("write shim source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module soroqkmsshim\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("write shim go.mod: %v", err)
	}
	binary := filepath.Join(dir, "soroq-kms-shim")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = dir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shim: %v\n%s", err, output)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("shim binary missing after build: %v", err)
	}
	// So an owner can hold the artifact that was actually exercised rather than take its
	// description on trust: SOROQ_D3_SHIM_OUT=<dir> go test ... keeps the source and the binary.
	if keep := strings.TrimSpace(os.Getenv("SOROQ_D3_SHIM_OUT")); keep != "" {
		if err := os.MkdirAll(keep, 0o755); err != nil {
			t.Fatalf("SOROQ_D3_SHIM_OUT: %v", err)
		}
		for _, name := range []string{"main.go", "go.mod", "soroq-kms-shim"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("SOROQ_D3_SHIM_OUT read %s: %v", name, err)
			}
			mode := os.FileMode(0o644)
			if name == "soroq-kms-shim" {
				mode = 0o755
			}
			if err := os.WriteFile(filepath.Join(keep, name), data, mode); err != nil {
				t.Fatalf("SOROQ_D3_SHIM_OUT write %s: %v", name, err)
			}
		}
		t.Logf("external signer program kept at %s", keep)
	}
	return binary
}

// shimKeygen asks the shim to mint a key on ITS side and hand back only the public half.
func shimKeygen(t *testing.T, shim, keyDir, keyRef string) string {
	t.Helper()
	cmd := exec.Command(shim, "-mode=keygen", "-key-dir="+keyDir, "-key-ref="+keyRef)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("shim keygen %s: %v\n%s", keyRef, err, stderr.String())
	}
	publicKey := strings.TrimSpace(stdout.String())
	raw, err := base64.RawURLEncoding.DecodeString(publicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		t.Fatalf("shim keygen %s returned %q, want an unpadded base64url ed25519 public key", keyRef, publicKey)
	}
	return publicKey
}

// readShimSeedForAssertionsOnly reads the shim's private seed straight off disk.
//
// This is the TEST HARNESS reaching around the seam, after the fact, so that "the seed never appears
// in argv / in the caller's error output" can be asserted against the real distinctive value rather
// than against a pattern. ExternalManifestSigner never reads this file and is never told about it.
func readShimSeedForAssertionsOnly(t *testing.T, keyDir, keyRef string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(keyDir, keyRef+".key"))
	if err != nil {
		t.Fatalf("read shim seed (harness-only): %v", err)
	}
	seed := strings.TrimSpace(string(raw))
	if decoded, err := base64.RawURLEncoding.DecodeString(seed); err != nil || len(decoded) != ed25519.SeedSize {
		t.Fatalf("shim seed file is not a %d-byte base64url seed", ed25519.SeedSize)
	}
	return seed
}

const d3KeyID = "soroq-kms-key-alpha"

func newProcessBackedSigner(t *testing.T, shim, publicKey string, args ...string) *ExternalManifestSigner {
	t.Helper()
	signer, err := NewExternalManifestSigner(shim, args, publicKey, d3KeyID, 30*time.Second)
	if err != nil {
		t.Fatalf("NewExternalManifestSigner: %v", err)
	}
	// Deliberately NOT overriding signer.runner: this must fork/exec the real binary.
	return signer
}

func d3Manifest() domain.PatchManifest {
	return domain.PatchManifest{
		PatchID:        "patch-d3-9f2a41c7",
		PatchNumber:    7,
		RuntimeID:      "runtime-d3-5b8e2210",
		ReleaseID:      "release-d3-1c4d77a9",
		Channel:        "stable",
		Kind:           domain.PatchKindExperimentalNativeAOT,
		ActivationMode: domain.ActivationNextColdStart,
		Artifact: domain.PatchArtifact{
			URL:       "https://artifacts.invalid/d3/aa11bb22cc33.zip",
			SHA256:    "3d5f2ac1b9e04477a1c6f0d83b2e5591cc7710ee44d2b30f9a6c1e5578f4a2b0",
			SizeBytes: 424242,
		},
	}
}

// publishSignedBundle stands in for the publication step, in the ORDER OF OPERATIONS that
// store.normalizePatchBundleForPatch uses: sign first, attach, and only then emit the artifact.
//
// It is typed to ManifestSignerBackend, which is the seam. SCOPE, and this is load-bearing for how
// property 3 may be read: what this file proves is that the SEAM refuses before anything is emitted.
// It is a stand-in for publication, not a production publish path.
//
// The production paths (normalizePatchBundleForPatch, FileStore, PostgresStore) were typed to the
// concrete *ManifestSigner when this file was written, so an external signer could not be reached
// from a deployment at all. They now accept ManifestSignerBackend and soroqd can select an external
// signer from the environment; the evidence that a REAL publish path is driven by one, and writes
// nothing when it refuses, is internal/store/external_signer_publish_test.go. Neither file should be
// read as a cloud-KMS integration.
func publishSignedBundle(outputDir string, manifest domain.PatchManifest, backend ManifestSignerBackend) (string, error) {
	signature, err := backend.SignManifest(manifest)
	if err != nil {
		return "", fmt.Errorf("sign patch manifest: %w", err)
	}
	keyID := backend.KeyID()
	manifest.SignatureKeyID = &keyID
	manifest.Signature = &signature
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(outputDir, "manifest.json")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func assertNothingPublished(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read publication dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("refused signing still produced output in %s: %v", dir, names)
	}
}

// ---------------------------------------------------------------------------------------------

func TestExternalSignerAgainstARealSignerProcess(t *testing.T) {
	shim := buildKMSShim(t)
	keyDir := t.TempDir()

	// Two keys, both minted inside the shim. Alpha is the configured key; beta is the "helper is
	// wired to the wrong key" control. This process sees only their public halves.
	alphaPublicKey := shimKeygen(t, shim, keyDir, "alpha")
	betaPublicKey := shimKeygen(t, shim, keyDir, "beta")
	if alphaPublicKey == betaPublicKey {
		t.Fatalf("the wrong-key control needs two distinct keys")
	}
	// Harness-only, for the leak assertions. Never handed to the signer.
	alphaSeed := readShimSeedForAssertionsOnly(t, keyDir, "alpha")

	manifest := d3Manifest()
	payload := ManifestSigningPayload(manifest)

	// -----------------------------------------------------------------------------------------
	// Property 1: the canonical bytes arrive on stdin; argv carries a key reference and nothing else.
	t.Run("canonical_bytes_arrive_on_stdin_not_argv", func(t *testing.T) {
		recordPath := filepath.Join(t.TempDir(), "received.json")
		signer := newProcessBackedSigner(t, shim, alphaPublicKey,
			"-mode=sign", "-key-dir="+keyDir, "-key-ref=alpha", "-record="+recordPath)

		if _, err := signer.SignManifest(manifest); err != nil {
			t.Fatalf("SignManifest: %v", err)
		}

		raw, err := os.ReadFile(recordPath)
		if err != nil {
			t.Fatalf("the signer process recorded nothing, so nothing here is evidence: %v", err)
		}
		var received shimRecord
		if err := json.Unmarshal(raw, &received); err != nil {
			t.Fatalf("decode record: %v", err)
		}

		// POSITIVE: byte-for-byte identity with what the caller intended to sign.
		got, err := base64.RawURLEncoding.DecodeString(received.StdinBase64)
		if err != nil {
			t.Fatalf("decode recorded stdin: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("the signer process received different bytes than the caller signed:\n got %q\nwant %q", got, payload)
		}
		wantSum := sha256.Sum256(payload)
		if received.StdinSHA256 != hex.EncodeToString(wantSum[:]) {
			t.Fatalf("stdin digest %s != canonical payload digest %s", received.StdinSHA256, hex.EncodeToString(wantSum[:]))
		}
		if received.StdinLen != len(payload) {
			t.Fatalf("stdin length %d != %d", received.StdinLen, len(payload))
		}

		// SENSITIVITY CHECK (not the control): the comparison discriminates. One changed manifest
		// field must make the same assertion fail.
		adjacent := manifest
		adjacent.Artifact.SizeBytes = manifest.Artifact.SizeBytes + 1
		if bytes.Equal(got, ManifestSigningPayload(adjacent)) {
			t.Fatalf("payload comparison is not discriminating: a changed manifest produced identical bytes")
		}

		// NEGATIVE CONTROL for "not argv": the real argv the child was exec'd with.
		joinedArgv := strings.Join(received.Argv, "\x00")
		if strings.Contains(joinedArgv, string(payload)) {
			t.Fatalf("the payload was passed in argv: %v", received.Argv)
		}
		for _, line := range strings.Split(string(payload), "\n") {
			if strings.Contains(joinedArgv, line) {
				t.Fatalf("payload line %q leaked into argv: %v", line, received.Argv)
			}
		}
		if strings.Contains(joinedArgv, alphaSeed) {
			t.Fatalf("the signing seed leaked into argv")
		}
		// POSITIVE ANCHOR: argv really was read, and really does carry the key REFERENCE. Without
		// this the "absent from argv" assertions could pass on an empty slice.
		if !strings.Contains(joinedArgv, "-key-ref=alpha") || !strings.Contains(joinedArgv, "-mode=sign") {
			t.Fatalf("argv anchor missing; the recorded argv is not the seam's argv: %v", received.Argv)
		}
		if len(received.Argv) != 5 {
			t.Fatalf("expected argv0 plus the four seam args, got %v", received.Argv)
		}
		t.Logf("EVIDENCE child argv (the real os.Args of the signer process): %q", received.Argv)
		t.Logf("EVIDENCE stdin: %d bytes, sha256=%s", received.StdinLen, received.StdinSHA256)

		// Same question of the environment, which exec inherits.
		joinedEnv := strings.Join(received.Env, "\x00")
		if strings.Contains(joinedEnv, alphaSeed) || strings.Contains(joinedEnv, string(payload)) {
			t.Fatalf("key material or payload reached the child through the environment")
		}
	})

	// -----------------------------------------------------------------------------------------
	// Property 2: the returned signature verifies against the CONFIGURED key, over the CANONICAL bytes.
	t.Run("signature_verifies_over_the_canonical_bytes", func(t *testing.T) {
		signer := newProcessBackedSigner(t, shim, alphaPublicKey,
			"-mode=sign", "-key-dir="+keyDir, "-key-ref=alpha")

		signature, err := signer.SignManifest(manifest)
		if err != nil {
			t.Fatalf("SignManifest: %v", err)
		}

		// POSITIVE: the manifest verifies through the ordinary verifier, exactly as a locally
		// signed one would.
		signed := manifest
		signed.Signature = &signature
		if err := VerifyManifestSignature(signed, signer.PublicKeyBase64()); err != nil {
			t.Fatalf("externally signed manifest must verify: %v", err)
		}
		if signer.PublicKeyBase64() != alphaPublicKey {
			t.Fatalf("configured public key changed under us")
		}

		signatureBytes, err := base64.RawURLEncoding.DecodeString(signature)
		if err != nil {
			t.Fatalf("decode signature: %v", err)
		}
		alphaPublicKeyBytes, err := base64.RawURLEncoding.DecodeString(alphaPublicKey)
		if err != nil {
			t.Fatalf("decode alpha public key: %v", err)
		}
		if !ed25519.Verify(alphaPublicKeyBytes, payload, signatureBytes) {
			t.Fatalf("signature does not verify over the canonical payload")
		}

		// NEGATIVE CONTROLS: the signature is bound to the canonical bytes, not to something
		// adjacent to them. Trailing newline is the realistic confusion, since SignManifest
		// TrimSpaces the helper's stdout and the payload itself has no trailing newline.
		adjacentPayloads := map[string][]byte{
			"payload with a trailing newline": append(append([]byte{}, payload...), '\n'),
			"payload with a leading newline":  append([]byte{'\n'}, payload...),
			"manifest as JSON":                mustJSON(t, manifest),
			"payload with one changed field":  ManifestSigningPayload(withChangedArtifact(manifest)),
		}
		for name, adjacent := range adjacentPayloads {
			if bytes.Equal(adjacent, payload) {
				t.Fatalf("adjacency control %q is not actually different from the canonical payload", name)
			}
			if ed25519.Verify(alphaPublicKeyBytes, adjacent, signatureBytes) {
				t.Fatalf("the signature also verifies over %q, so it is not bound to the canonical bytes", name)
			}
		}

		// And a wrong-key control on the verification side itself.
		betaPublicKeyBytes, err := base64.RawURLEncoding.DecodeString(betaPublicKey)
		if err != nil {
			t.Fatalf("decode beta public key: %v", err)
		}
		if ed25519.Verify(betaPublicKeyBytes, payload, signatureBytes) {
			t.Fatalf("the signature verifies under the wrong public key")
		}
	})

	// -----------------------------------------------------------------------------------------
	// Property 3: a helper signing with a DIFFERENT key is refused before anything is emitted.
	t.Run("wrong_key_is_refused_before_publication", func(t *testing.T) {
		// POSITIVE CONTROL first: the same harness, the right key, really does publish. Without
		// this the negative case below could pass because the harness never publishes anything.
		goodDir := t.TempDir()
		goodSigner := newProcessBackedSigner(t, shim, alphaPublicKey,
			"-mode=sign", "-key-dir="+keyDir, "-key-ref=alpha")
		publishedPath, err := publishSignedBundle(goodDir, manifest, goodSigner)
		if err != nil {
			t.Fatalf("positive control: publication must succeed with the configured key: %v", err)
		}
		publishedBytes, err := os.ReadFile(publishedPath)
		if err != nil {
			t.Fatalf("positive control: published manifest unreadable: %v", err)
		}
		var published domain.PatchManifest
		if err := json.Unmarshal(publishedBytes, &published); err != nil {
			t.Fatalf("positive control: published manifest undecodable: %v", err)
		}
		if err := VerifyManifestSignature(published, alphaPublicKey); err != nil {
			t.Fatalf("positive control: published manifest must verify: %v", err)
		}
		if published.SignatureKeyID == nil || *published.SignatureKeyID != d3KeyID {
			t.Fatalf("positive control: published manifest must carry the configured key id")
		}

		// NEGATIVE: the helper is pointed at key beta while the caller is configured with alpha.
		badDir := t.TempDir()
		wrongKeySigner := newProcessBackedSigner(t, shim, alphaPublicKey,
			"-mode=sign", "-key-dir="+keyDir, "-key-ref=beta")
		signature, err := wrongKeySigner.SignManifest(manifest)
		if err == nil {
			t.Fatalf("a helper signing with a different key must be refused")
		}
		if signature != "" {
			t.Fatalf("a refused signing must return no signature, got %q", signature)
		}
		if !strings.Contains(err.Error(), "does not verify against the configured public key") {
			t.Fatalf("refusal must name the cause, got: %v", err)
		}
		t.Logf("EVIDENCE wrong-key refusal: %v", err)
		// The absence of output, asserted rather than assumed.
		if _, err := publishSignedBundle(badDir, manifest, wrongKeySigner); err == nil {
			t.Fatalf("publication must fail when the signer is refused")
		}
		assertNothingPublished(t, badDir)

		// The bad signer's bytes are not merely unverifiable, they are a real signature under the
		// OTHER key -- so the refusal is a key check, not an accident of malformed output.
		betaOutput := runShimDirectly(t, shim, payload,
			"-mode=sign", "-key-dir="+keyDir, "-key-ref=beta")
		betaSignatureBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(betaOutput))
		if err != nil || len(betaSignatureBytes) != ed25519.SignatureSize {
			t.Fatalf("the wrong-key helper did not even return a well-formed signature; the control is weaker than intended")
		}
		betaPublicKeyBytes, err := base64.RawURLEncoding.DecodeString(betaPublicKey)
		if err != nil {
			t.Fatalf("decode beta public key: %v", err)
		}
		if !ed25519.Verify(betaPublicKeyBytes, payload, betaSignatureBytes) {
			t.Fatalf("the wrong-key helper's output must be a genuine signature under its own key")
		}
	})

	// -----------------------------------------------------------------------------------------
	// Property 4: malformed and dishonest responses fail closed.
	t.Run("malformed_responses_fail_closed", func(t *testing.T) {
		substitutePath := filepath.Join(t.TempDir(), "other-payload.bin")
		otherManifest := withChangedArtifact(manifest)
		otherPayload := ManifestSigningPayload(otherManifest)
		if err := os.WriteFile(substitutePath, otherPayload, 0o600); err != nil {
			t.Fatalf("write substitute payload: %v", err)
		}

		cases := []struct {
			name      string
			fault     string
			wantError string
		}{
			{"empty output", "empty", "returned no signature"},
			{"truncated signature", "truncated", "63-byte signature"},
			{"non base64url garbage", "garbage", "not unpadded base64url"},
			{"helper exits non-zero", "exit-nonzero", "failed"},
			{"valid signature over the payload plus a newline", "substitute-trailing-newline", "does not verify against the configured public key"},
			{"valid signature over a DIFFERENT manifest", "substitute-payload", "does not verify against the configured public key"},
		}
		for _, testCase := range cases {
			t.Run(testCase.fault, func(t *testing.T) {
				outputDir := t.TempDir()
				signer := newProcessBackedSigner(t, shim, alphaPublicKey,
					"-mode=sign", "-key-dir="+keyDir, "-key-ref=alpha",
					"-substitute-file="+substitutePath, "-fault="+testCase.fault)
				signature, err := signer.SignManifest(manifest)
				if err == nil {
					t.Fatalf("%s must be refused, got signature %q", testCase.name, signature)
				}
				if signature != "" {
					t.Fatalf("%s returned a signature %q alongside its error", testCase.name, signature)
				}
				if !strings.Contains(err.Error(), testCase.wantError) {
					t.Fatalf("%s: error %q does not contain %q", testCase.name, err, testCase.wantError)
				}
				if _, err := publishSignedBundle(outputDir, manifest, signer); err == nil {
					t.Fatalf("%s must not publish", testCase.name)
				}
				assertNothingPublished(t, outputDir)
			})
		}

		// The interesting case, made explicit: the substitution faults are exactly what a naive
		// "did we get 64 well-formed bytes back?" check would ACCEPT. Both are demonstrated to pass
		// that naive check and to verify under the configured key over the WRONG bytes.
		for _, substitution := range []struct {
			fault  string
			signed []byte
		}{
			{"substitute-trailing-newline", append(append([]byte{}, payload...), '\n')},
			{"substitute-payload", otherPayload},
		} {
			output := runShimDirectly(t, shim, payload,
				"-mode=sign", "-key-dir="+keyDir, "-key-ref=alpha",
				"-substitute-file="+substitutePath, "-fault="+substitution.fault)
			signatureBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(output))
			if err != nil {
				t.Fatalf("%s: helper output is not base64url, so it would not fool a naive check: %v", substitution.fault, err)
			}
			if len(signatureBytes) != ed25519.SignatureSize {
				t.Fatalf("%s: helper output is %d bytes; a naive length check would already have caught it", substitution.fault, len(signatureBytes))
			}
			alphaPublicKeyBytes, err := base64.RawURLEncoding.DecodeString(alphaPublicKey)
			if err != nil {
				t.Fatalf("decode alpha public key: %v", err)
			}
			if !ed25519.Verify(alphaPublicKeyBytes, substitution.signed, signatureBytes) {
				t.Fatalf("%s: expected a genuine signature under the configured key over the substituted bytes", substitution.fault)
			}
			if ed25519.Verify(alphaPublicKeyBytes, payload, signatureBytes) {
				t.Fatalf("%s: the substituted signature must NOT verify over the canonical payload", substitution.fault)
			}
		}
	})

	// -----------------------------------------------------------------------------------------
	// Property 5: no key material leaks into argv, the returned error, or surfaced stderr.
	t.Run("no_key_material_leaks", func(t *testing.T) {
		outputDir := t.TempDir()
		signer := newProcessBackedSigner(t, shim, alphaPublicKey,
			"-mode=sign", "-key-dir="+keyDir, "-key-ref=alpha", "-fault=leak-stderr")
		signature, err := signer.SignManifest(manifest)
		if err == nil {
			t.Fatalf("a helper that exits non-zero must be refused")
		}
		if signature != "" {
			t.Fatalf("refusal returned a signature: %q", signature)
		}
		if _, publishErr := publishSignedBundle(outputDir, manifest, signer); publishErr == nil {
			t.Fatalf("a leaking, failing helper must not publish")
		}
		assertNothingPublished(t, outputDir)

		message := err.Error()

		// NEGATIVE: none of the three distinct secrets the helper printed survive into the caller's
		// only output surface. The seed is a random 32-byte value the caller never held.
		for name, secret := range map[string]string{
			"the signing seed":       alphaSeed,
			"the bearer token":       "sq_live_tok_D3PROOF_qX7fLm2v",
			"the query-string token": "sq_qs_D3PROOF_9aZ",
		} {
			if strings.Contains(message, secret) {
				t.Fatalf("%s leaked into the caller's error:\n%s", name, message)
			}
		}
		// POSITIVE ANCHOR: the helper's stderr really was surfaced and really was redacted, so the
		// assertions above cannot pass merely because stderr was dropped on the floor.
		if !strings.Contains(message, "[REDACTED]") {
			t.Fatalf("expected redacted helper diagnostics in:\n%s", message)
		}
		if strings.Count(message, "[REDACTED]") < 3 {
			t.Fatalf("expected all three credential shapes to be redacted, got:\n%s", message)
		}
		if !strings.Contains(message, "soroq-kms-shim: request failed") {
			t.Fatalf("the helper's non-secret diagnostic must survive so operators can debug:\n%s", message)
		}
		if !strings.Contains(message, shim) {
			t.Fatalf("the error must name the helper command:\n%s", message)
		}
		t.Logf("EVIDENCE redacted failure surfaced to the caller:\n%s", message)

		// POSITIVE ANCHOR on identity: a wrong-key refusal names the configured KEY ID, which is
		// the operator's handle on the key, while still carrying no key material.
		wrongKeySigner := newProcessBackedSigner(t, shim, alphaPublicKey,
			"-mode=sign", "-key-dir="+keyDir, "-key-ref=beta")
		if _, wrongErr := wrongKeySigner.SignManifest(manifest); wrongErr == nil {
			t.Fatalf("wrong-key anchor: expected refusal")
		} else {
			if !strings.Contains(wrongErr.Error(), d3KeyID) {
				t.Fatalf("wrong-key refusal must name the configured key id %q: %v", d3KeyID, wrongErr)
			}
			if strings.Contains(wrongErr.Error(), alphaSeed) ||
				strings.Contains(wrongErr.Error(), readShimSeedForAssertionsOnly(t, keyDir, "beta")) {
				t.Fatalf("a refusal must not print key material: %v", wrongErr)
			}
		}

		// THE STATED LIMIT, asserted rather than implied. Redaction here is shape-based
		// (external_signer_patterns.go), so an UNLABELLED opaque string is not detectable and IS
		// surfaced. Same limit already documented for the audit redactor
		// (backend/internal/audit/redact.go:29). The defence that holds regardless is structural:
		// the seam never possesses key material, so only a helper that volunteers it can expose it.
		bareSigner := newProcessBackedSigner(t, shim, alphaPublicKey,
			"-mode=sign", "-key-dir="+keyDir, "-key-ref=alpha", "-fault=leak-stderr-bare")
		_, bareErr := bareSigner.SignManifest(manifest)
		if bareErr == nil {
			t.Fatalf("expected the bare-leak helper to fail")
		}
		if !strings.Contains(bareErr.Error(), alphaSeed) {
			t.Fatalf("the documented limit no longer holds as described; re-state it rather than deleting this case:\n%s", bareErr)
		}
	})
}

func withChangedArtifact(manifest domain.PatchManifest) domain.PatchManifest {
	changed := manifest
	changed.Artifact.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	changed.Artifact.SizeBytes = manifest.Artifact.SizeBytes + 1
	changed.Artifact.URL = "https://artifacts.invalid/d3/attacker.zip"
	return changed
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

// runShimDirectly invokes the signer program outside the seam, so a test can look at the RAW bytes
// the seam was handed before it decided what to do with them.
func runShimDirectly(t *testing.T, shim string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command(shim, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run shim directly: %v\n%s", err, stderr.String())
	}
	return stdout.String()
}
