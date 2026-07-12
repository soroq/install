package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/signing"
)

// buildFixtureToolchainArchive builds a minimal but STRUCTURALLY VALID toolchain archive (tar.gz) whose
// bundle subdir is ios/, containing engine.json + the 5 iOS artifacts. Enough for the extract step's
// engine.json presence check; the verifyEngineBundle gate is exercised via the runVerifyEngineBundle seam.
func buildFixtureToolchainArchive(t *testing.T) []byte {
	t.Helper()
	files := map[string]string{
		"ios/engine.json":       `{"schema":"soroq.ios_engine.v1"}`,
		"ios/flutter_framework": "fixture-flutter-framework-bytes",
		"ios/dart2bytecode":     "fixture-dart2bytecode-bytes",
		"ios/gen_snapshot":      "fixture-gen-snapshot-bytes",
		"ios/vm_platform":       "fixture-vm-platform-bytes",
		"ios/dartaotruntime":    "fixture-dartaotruntime-bytes",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestToolchainInstallVerifyBeforeSwap is the STOP-IF regression guard: it proves that when the UNCHANGED
// verifyEngineBundle FAILS on a new install, a PRE-EXISTING versionDir (the last-working toolchain) is
// left INTACT — the swap into the cache happens only AFTER verify passes. It also proves the clean path
// swaps the freshly-verified bundle into place and leaves no transient temp dir behind.
func TestToolchainInstallVerifyBeforeSwap(t *testing.T) {
	seedB64 := usePinnedToolchainKey(t)
	seed, err := signing.DecodeToolchainSeed(seedB64)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := signing.NewToolchainSignerFromSeed(seed, toolchainPinnedKeyID)
	if err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)

	const version = "soroq-toolchain-fixture-safety"
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
				URL:             srv.URL + "/archive",
				SHA256:          archiveSHA,
				CompressedBytes: int64(len(archive)),
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

	versionDir, err := toolchainVersionDir(version)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(versionDir)

	// Pre-create a "last-working toolchain" sentinel so we can prove a verify-FAIL install leaves it intact.
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(versionDir, "LAST_WORKING_SENTINEL")
	if err := os.WriteFile(sentinel, []byte("do not delete"), 0o644); err != nil {
		t.Fatal(err)
	}

	// countTempDirs reports lingering .tmp-install-* extract dirs (a leaked partially-trusted cache entry).
	countTempDirs := func() int {
		entries, _ := os.ReadDir(root)
		n := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-install-") {
				n++
			}
		}
		return n
	}

	origVerify := runVerifyEngineBundle
	t.Cleanup(func() { runVerifyEngineBundle = origVerify })

	// (1) verifyEngineBundle FAILS => install REFUSED AFTER extract, the pre-existing versionDir (sentinel)
	//     is left UNTOUCHED, and no transient temp extract dir is leaked.
	runVerifyEngineBundle = func(platform, bundleDir string) (string, error) {
		return "simulated verify output", fmt.Errorf("simulated verifyEngineBundle failure")
	}
	err = runToolchainInstall([]string{version, "--api", srv.URL, "--force"})
	if err == nil {
		t.Fatal("expected REFUSED on verifyEngineBundle failure, got nil")
	}
	if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "verifyEngineBundle") {
		t.Fatalf("expected a verifyEngineBundle REFUSED, got: %v", err)
	}
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Fatalf("last-working toolchain was destroyed by a failed install (STOP-IF regression): %v", statErr)
	}
	if n := countTempDirs(); n != 0 {
		t.Fatalf("a failed install leaked %d transient .tmp-install-* dir(s)", n)
	}

	// (2) verifyEngineBundle PASSES => install succeeds, the verified bundle is swapped into versionDir
	//     (engine.json present), the stale sentinel is gone, and no temp dir lingers.
	runVerifyEngineBundle = func(platform, bundleDir string) (string, error) { return "verify ok", nil }
	if err := runToolchainInstall([]string{version, "--api", srv.URL, "--force"}); err != nil {
		t.Fatalf("clean install failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(versionDir, "ios", "engine.json")); err != nil {
		t.Fatalf("installed bundle missing ios/engine.json after swap: %v", err)
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatal("clean install should have replaced the prior dir (stale sentinel still present)")
	}
	if n := countTempDirs(); n != 0 {
		t.Fatalf("a clean install leaked %d transient .tmp-install-* dir(s)", n)
	}
}
