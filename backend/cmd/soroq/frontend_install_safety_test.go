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

// buildFixtureFrontendArchive builds a minimal but STRUCTURALLY VALID frontend archive (tar.gz) whose top
// entry is flutter-sdk-src/, containing an executable bin/flutter and the required soroq_metadata.dart.
func buildFixtureFrontendArchive(t *testing.T) []byte {
	t.Helper()
	files := []struct {
		name string
		mode int64
		body string
	}{
		{"flutter-sdk-src/bin/flutter", 0o755, "#!/bin/sh\necho fixture flutter\n"},
		{"flutter-sdk-src/packages/flutter_tools/lib/src/soroq_metadata.dart", 0o644, "// soroq asset bundler fixture\n"},
		{"flutter-sdk-src/.git/HEAD", 0o644, "ref: refs/heads/main\n"},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
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

// TestFrontendInstallSafety proves the safety-critical install invariants without touching prod:
//   - the signed manifest is verified against the pinned key;
//   - the archive SHA-256 + size are verified BEFORE extraction (a truncated download is REFUSED);
//   - a REFUSED install does NOT corrupt a prior install (atomic temp -> verify -> rename);
//   - a clean install records the active frontend; --force reinstalls.
func TestFrontendInstallSafety(t *testing.T) {
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
	t.Setenv("SOROQ_FLUTTER_BIN", "")

	const version = "soroq-flutter-frontend-fixture-safety"
	archive := buildFixtureFrontendArchive(t)
	sum := sha256.Sum256(archive)
	archiveSHA := hex.EncodeToString(sum[:])

	// A mutable server switch: serve either the FULL archive or a TRUNCATED one.
	serveTruncated := true
	var srv *httptest.Server
	buildManifest := func() ([]byte, string) {
		m := frontendManifest{
			Schema:               frontendManifestSchema,
			SoroqFrontendVersion: version,
			FlutterRevision:      expectedFlutterRevision,
			DartRevision:         "3.13.0-103.1.beta",
			SigningKeyID:         toolchainPinnedKeyID,
			Archive: frontendManifestArchive{
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
			if serveTruncated {
				w.Write(archive[:len(archive)/2]) // truncated => size mismatch BEFORE extract
				return
			}
			w.Write(archive)
		default:
			mb, _ := buildManifest()
			w.Write(mb)
		}
	}))
	defer srv.Close()

	versionDir, err := frontendVersionDir(version)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-create a "prior install" sentinel so we can prove a REFUSED install leaves it intact.
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(versionDir, "PRIOR_INSTALL_SENTINEL")
	if err := os.WriteFile(sentinel, []byte("do not delete"), 0o644); err != nil {
		t.Fatal(err)
	}

	// (1) Truncated archive => REFUSED before extract, prior install (sentinel) intact.
	serveTruncated = true
	err = runFrontendInstall([]string{version, "--api", srv.URL, "--force"})
	if err == nil {
		t.Fatal("expected REFUSED on truncated archive, got nil")
	}
	if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(strings.ToLower(err.Error()), "size mismatch") {
		t.Fatalf("expected a size-mismatch REFUSED, got: %v", err)
	}
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Fatalf("prior install was corrupted by a refused install: %v", statErr)
	}
	if _, ok, _ := loadActiveFrontend(); ok {
		t.Fatal("a refused install must not record an active frontend")
	}

	// (2) Full archive => install succeeds, active recorded, bin/flutter present, sentinel replaced.
	serveTruncated = false
	if err := runFrontendInstall([]string{version, "--api", srv.URL, "--force"}); err != nil {
		t.Fatalf("clean install failed: %v", err)
	}
	binPath := filepath.Join(versionDir, "flutter-sdk-src", "bin", "flutter")
	if info, err := os.Stat(binPath); err != nil || info.IsDir() {
		t.Fatalf("installed bin/flutter missing: %v", err)
	}
	active, ok, err := loadActiveFrontend()
	if err != nil || !ok || active.Version != version {
		t.Fatalf("active frontend not recorded: ok=%v err=%v active=%+v", ok, err, active)
	}
	if active.FlutterBin != binPath {
		t.Fatalf("active flutter bin = %q, want %q", active.FlutterBin, binPath)
	}
	// The atomic rename replaced the old dir, so the stale sentinel is gone.
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatal("clean install should have replaced the prior dir (stale sentinel still present)")
	}

	// (3) resolveSoroqFlutterBin now returns the installed frontend (no env, no ~/development here).
	if got, err := resolveSoroqFlutterBin(); err != nil || got != binPath {
		t.Fatalf("resolveSoroqFlutterBin = %q err %v, want installed %q", got, err, binPath)
	}

	// (4) Idempotent re-install (no --force) = verified cache hit (offline; server would 500 if hit).
	if err := runFrontendInstall([]string{version, "--api", "http://127.0.0.1:1"}); err != nil {
		t.Fatalf("verified cache-hit re-install failed: %v", err)
	}

	// (5) --force reinstalls cleanly against the server.
	serveTruncated = false
	if err := runFrontendInstall([]string{version, "--api", srv.URL, "--force"}); err != nil {
		t.Fatalf("--force reinstall failed: %v", err)
	}
}

// TestUntarGzReaderSkipsAppleDouble proves the extractor drops macOS AppleDouble ._* sidecars (a frontend
// tarred on macOS without COPYFILE_DISABLE carries ._pack-*.idx etc. that break git-dependent flutter ops).
func TestUntarGzReaderSkipsAppleDouble(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := map[string]string{
		"flutter-sdk-src/.git/objects/pack/pack-abc.idx":   "real index",
		"flutter-sdk-src/.git/objects/pack/._pack-abc.idx": "appledouble junk",
		"flutter-sdk-src/._shallow":                        "appledouble junk",
		"flutter-sdk-src/bin/flutter":                      "#!/bin/sh\n",
	}
	for name, body := range entries {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()

	dst := t.TempDir()
	if err := untarGzReader(bytes.NewReader(buf.Bytes()), dst); err != nil {
		t.Fatalf("untarGzReader: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "flutter-sdk-src/.git/objects/pack/pack-abc.idx")); err != nil {
		t.Fatalf("real index should be extracted: %v", err)
	}
	for _, junk := range []string{
		"flutter-sdk-src/.git/objects/pack/._pack-abc.idx",
		"flutter-sdk-src/._shallow",
	} {
		if _, err := os.Stat(filepath.Join(dst, junk)); err == nil {
			t.Fatalf("AppleDouble sidecar %q should have been skipped", junk)
		}
	}
}

// TestUntarGzReaderRejectsTraversal proves the streaming extractor refuses a path-traversal entry.
func TestUntarGzReaderRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("pwned")
	_ = tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()

	dst := t.TempDir()
	err := untarGzReader(bytes.NewReader(buf.Bytes()), dst)
	if err == nil || !strings.Contains(err.Error(), "unsafe archive entry") {
		t.Fatalf("expected unsafe-entry refusal, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dst), "escape.txt")); statErr == nil {
		t.Fatal("path traversal escaped the destination dir")
	}
}
