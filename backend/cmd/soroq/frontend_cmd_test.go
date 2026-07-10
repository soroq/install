package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeExecutable creates an executable stub file at path (with its parent dirs).
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestResolveSoroqFlutterBinOrder pins the D1.2 resolution order:
// SOROQ_FLUTTER_BIN -> recorded frontend install -> legacy ~/development fallback -> error.
func TestResolveSoroqFlutterBinOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SOROQ_FLUTTER_BIN", "")
	// Ensure no `soroq-flutter` on PATH interferes with the ordering assertions.
	t.Setenv("PATH", filepath.Join(home, "empty-bin"))

	// Stage a recorded frontend install and a legacy ~/development checkout.
	version := "soroq-flutter-frontend-test"
	installedBin := filepath.Join(home, ".soroq", "frontends", version, defaultFrontendSubdir, "bin", "flutter")
	writeExecutable(t, installedBin)
	legacyBin := filepath.Join(home, "development", "soroq-forks", "flutter-sdk-src", "bin", "flutter")
	writeExecutable(t, legacyBin)

	// (a) With nothing recorded, the legacy fallback wins over "not found".
	if got, err := resolveSoroqFlutterBin(); err != nil || got != legacyBin {
		t.Fatalf("legacy fallback: got %q err %v, want %q", got, err, legacyBin)
	}

	// (b) Record an active frontend install: it now takes precedence over the legacy checkout.
	if err := recordActiveFrontend(activeFrontend{Version: version, FlutterBin: installedBin}); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveSoroqFlutterBin(); err != nil || got != installedBin {
		t.Fatalf("recorded install should win: got %q err %v, want %q", got, err, installedBin)
	}

	// (c) SOROQ_FLUTTER_BIN overrides everything.
	override := filepath.Join(home, "override", "flutter")
	writeExecutable(t, override)
	t.Setenv("SOROQ_FLUTTER_BIN", override)
	if got, err := resolveSoroqFlutterBin(); err != nil || got != override {
		t.Fatalf("env override should win: got %q err %v, want %q", got, err, override)
	}
	t.Setenv("SOROQ_FLUTTER_BIN", "")

	// (d) Remove both the recorded install target and the legacy checkout: clear error.
	if err := os.RemoveAll(filepath.Join(home, ".soroq")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, "development")); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveSoroqFlutterBin(); err == nil {
		t.Fatalf("expected an error when nothing resolves, got %q", got)
	}
}

func TestParseFrontendManifest(t *testing.T) {
	good := []byte(`{
	  "schema": "soroq.frontend.v1",
	  "soroq_frontend_version": "soroq-flutter-frontend-abc-def",
	  "flutter_revision": "` + expectedFlutterRevision + `",
	  "dart_revision": "3.13.0-103.1.beta",
	  "archive": {"url": "https://x/y.tar.gz", "sha256": "` + repeatHex(64) + `", "compressed_bytes": 10, "uncompressed_bytes": 20}
	}`)
	m, err := parseFrontendManifest(good)
	if err != nil {
		t.Fatalf("parse good manifest: %v", err)
	}
	if m.subdir() != defaultFrontendSubdir {
		t.Fatalf("default subdir: got %q", m.subdir())
	}
	if err := checkFrontendIdentity(m); err != nil {
		t.Fatalf("identity should pass for the pinned revision: %v", err)
	}

	// Wrong schema is refused.
	if _, err := parseFrontendManifest([]byte(`{"schema":"nope","soroq_frontend_version":"v"}`)); err == nil {
		t.Fatal("expected wrong-schema refusal")
	}
	// Wrong flutter revision is refused by the identity check.
	m.FlutterRevision = "0000000000000000000000000000000000000000"
	if err := checkFrontendIdentity(m); err == nil {
		t.Fatal("expected flutter-revision-mismatch refusal")
	}
}

func repeatHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
