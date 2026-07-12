package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"soroq/backend/internal/signing"
)

// captureStdoutStderr redirects os.Stdout + os.Stderr to separate temp files for the duration of fn,
// returning their contents SEPARATELY. This is how we prove no banner/progress bytes interleave with the
// --json report on STDOUT.
func captureStdoutStderr(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	outF, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	errF, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outF, errF
	defer func() {
		os.Stdout, os.Stderr = origOut, origErr
		outF.Close()
		errF.Close()
	}()
	fn()
	os.Stdout, os.Stderr = origOut, origErr
	ob, _ := os.ReadFile(outF.Name())
	eb, _ := os.ReadFile(errF.Name())
	return string(ob), string(eb)
}

func newToolchainSigner(t *testing.T) *signing.ToolchainManifestSigner {
	t.Helper()
	seedB64 := usePinnedToolchainKey(t)
	seed, err := signing.DecodeToolchainSeed(seedB64)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := signing.NewToolchainSignerFromSeed(seed, toolchainPinnedKeyID)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// TestFrontendInstallJSONByteStability proves `frontend install --json` emits ONLY the JSON report on
// STDOUT — no banner/progress bytes interleave — while the preflight + download banner land on STDERR.
func TestFrontendInstallJSONByteStability(t *testing.T) {
	signer := newToolchainSigner(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SOROQ_FLUTTER_BIN", "")

	const version = "soroq-frontend-json-stability"
	archive := buildFixtureFrontendArchive(t)
	sum := sha256.Sum256(archive)
	archiveSHA := hex.EncodeToString(sum[:])

	var srv *httptest.Server
	buildManifest := func() ([]byte, string) {
		m := frontendManifest{
			Schema:               frontendManifestSchema,
			SoroqFrontendVersion: version,
			FlutterRevision:      expectedFlutterRevision,
			DartRevision:         "3.13.0-103.1.beta",
			EngineRevision:       "engine-rev-fixture",
			SigningKeyID:         toolchainPinnedKeyID,
			Archive: frontendManifestArchive{
				URL:               srv.URL + "/archive",
				SHA256:            archiveSHA,
				CompressedBytes:   int64(len(archive)),
				UncompressedBytes: int64(len(archive) * 2),
			},
		}
		mb, _ := json.MarshalIndent(m, "", "  ")
		sig, err := signer.SignToolchainManifest(mb)
		if err != nil {
			t.Fatal(err)
		}
		return mb, sig
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifest.sig"):
			_, sig := buildManifest()
			fmt.Fprint(w, sig)
		case strings.HasSuffix(r.URL.Path, "/archive"):
			w.Write(archive)
		default:
			mb, _ := buildManifest()
			w.Write(mb)
		}
	}))
	defer srv.Close()

	var runErr error
	stdout, stderr := captureStdoutStderr(t, func() {
		runErr = runFrontendInstall([]string{version, "--api", srv.URL, "--force", "--json"})
	})
	if runErr != nil {
		t.Fatalf("frontend install --json failed: %v", runErr)
	}
	assertPureJSONStdout(t, stdout, "soroq_frontend_version", version)
	assertPreflightAndProgressOnStderr(t, stderr)
}

// TestToolchainInstallJSONByteStability proves `toolchain install --json` emits ONLY the JSON report on
// STDOUT. The verifyEngineBundle gate is stubbed to pass (fixtures can't clear the real soroqctl gate).
func TestToolchainInstallJSONByteStability(t *testing.T) {
	signer := newToolchainSigner(t)
	t.Setenv("HOME", t.TempDir())

	origVerify := runVerifyEngineBundle
	t.Cleanup(func() { runVerifyEngineBundle = origVerify })
	runVerifyEngineBundle = func(platform, bundleDir string) (string, error) { return "verify ok", nil }

	const version = "soroq-toolchain-json-stability"
	archive := buildFixtureToolchainArchive(t)
	sum := sha256.Sum256(archive)
	archiveSHA := hex.EncodeToString(sum[:])

	var srv *httptest.Server
	buildManifest := func() ([]byte, string) {
		m := cliManifest{
			Schema:                toolchainManifestSchema,
			SoroqToolchainVersion: version,
			Platform:              "ios",
			Arch:                  "arm64",
			BuildMode:             "profile",
			Tier:                  "experimental_profile",
			FlutterRevision:       expectedFlutterRevision,
			DartRevision:          expectedDartRevision,
			Archive: cliManifestArchive{
				URL:               srv.URL + "/archive",
				SHA256:            archiveSHA,
				CompressedBytes:   int64(len(archive)),
				UncompressedBytes: int64(len(archive) * 2),
			},
		}
		mb, _ := json.MarshalIndent(m, "", "  ")
		sig, err := signer.SignToolchainManifest(mb)
		if err != nil {
			t.Fatal(err)
		}
		return mb, sig
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifest.sig"):
			_, sig := buildManifest()
			fmt.Fprint(w, sig)
		case strings.HasSuffix(r.URL.Path, "/archive"):
			w.Write(archive)
		default:
			mb, _ := buildManifest()
			w.Write(mb)
		}
	}))
	defer srv.Close()

	var runErr error
	stdout, stderr := captureStdoutStderr(t, func() {
		runErr = runToolchainInstall([]string{version, "--api", srv.URL, "--force", "--json"})
	})
	if runErr != nil {
		t.Fatalf("toolchain install --json failed: %v", runErr)
	}
	assertPureJSONStdout(t, stdout, "soroq_toolchain_version", version)
	assertPreflightAndProgressOnStderr(t, stderr)
}

func assertPureJSONStdout(t *testing.T, stdout, versionKey, version string) {
	t.Helper()
	trimmed := strings.TrimSpace(stdout)
	var report map[string]any
	if err := json.Unmarshal([]byte(trimmed), &report); err != nil {
		t.Fatalf("stdout is not pure JSON (banner/progress leaked?): %v\n--- stdout ---\n%s", err, stdout)
	}
	if got, _ := report[versionKey].(string); got != version {
		t.Fatalf("json report %s = %q, want %q", versionKey, got, version)
	}
	// The load-bearing assertion: NO banner/progress bytes anywhere on STDOUT.
	for _, needle := range []string{"\r", "preflight", "Downloading", "downloading", "downloaded", "free:"} {
		if strings.Contains(stdout, needle) {
			t.Fatalf("stdout contains banner/progress marker %q (must be STDERR-only):\n%s", needle, stdout)
		}
	}
}

func assertPreflightAndProgressOnStderr(t *testing.T, stderr string) {
	t.Helper()
	for _, needle := range []string{"install preflight:", "free:", "Downloading", "downloaded"} {
		if !strings.Contains(stderr, needle) {
			t.Fatalf("expected STDERR to contain %q, got:\n%s", needle, stderr)
		}
	}
}

// TestFrontendInstallFreeDiskAbort proves the preflight ABORTS before any archive download when the target
// filesystem reports less free space than the peak footprint — and that NO archive bytes are fetched.
func TestFrontendInstallFreeDiskAbort(t *testing.T) {
	signer := newToolchainSigner(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SOROQ_FLUTTER_BIN", "")

	// Inject a tiny available-bytes value so the peak-footprint check fails. RESTORE via Cleanup so a
	// leaked value can't spuriously fail other tests (e.g. the verify-before-swap regression guard).
	origAvail := availableBytesFn
	t.Cleanup(func() { availableBytesFn = origAvail })
	availableBytesFn = func(string) (int64, error) { return 1, nil }

	const version = "soroq-frontend-diskabort"
	archive := buildFixtureFrontendArchive(t)
	sum := sha256.Sum256(archive)
	archiveSHA := hex.EncodeToString(sum[:])

	var archiveHits int32
	var srv *httptest.Server
	buildManifest := func() ([]byte, string) {
		m := frontendManifest{
			Schema:               frontendManifestSchema,
			SoroqFrontendVersion: version,
			FlutterRevision:      expectedFlutterRevision,
			DartRevision:         "3.13.0-103.1.beta",
			SigningKeyID:         toolchainPinnedKeyID,
			Archive: frontendManifestArchive{
				URL:               srv.URL + "/archive",
				SHA256:            archiveSHA,
				CompressedBytes:   int64(len(archive)),
				UncompressedBytes: int64(len(archive) * 2),
			},
		}
		mb, _ := json.MarshalIndent(m, "", "  ")
		sig, err := signer.SignToolchainManifest(mb)
		if err != nil {
			t.Fatal(err)
		}
		return mb, sig
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifest.sig"):
			_, sig := buildManifest()
			fmt.Fprint(w, sig)
		case strings.HasSuffix(r.URL.Path, "/archive"):
			atomic.AddInt32(&archiveHits, 1)
			w.Write(archive)
		default:
			mb, _ := buildManifest()
			w.Write(mb)
		}
	}))
	defer srv.Close()

	err := runFrontendInstall([]string{version, "--api", srv.URL, "--force"})
	if err == nil {
		t.Fatal("expected an insufficient-disk abort, got nil")
	}
	if !strings.Contains(err.Error(), "insufficient disk space") || !strings.Contains(err.Error(), "need ~") {
		t.Fatalf("expected a clear free-disk abort message, got: %v", err)
	}
	if n := atomic.LoadInt32(&archiveHits); n != 0 {
		t.Fatalf("the archive was downloaded (%d hits) despite a free-disk abort; the check must run BEFORE the download", n)
	}
}

// TestRequiredPeakBytes documents the PEAK-footprint math: compressed+uncompressed when both are known,
// and a conservative 3x compressed fallback when the manifest exposes no uncompressed size.
func TestRequiredPeakBytes(t *testing.T) {
	if got := requiredPeakBytes(100, 400); got != 500 {
		t.Fatalf("compressed+uncompressed = %d, want 500", got)
	}
	if got := requiredPeakBytes(100, 0); got != 300 {
		t.Fatalf("3x fallback = %d, want 300", got)
	}
	if got := requiredPeakBytes(0, 0); got != 0 {
		t.Fatalf("unknown compressed = %d, want 0 (skip check)", got)
	}
}
