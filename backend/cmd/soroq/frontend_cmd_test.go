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

// TestResolveSoroqFlutterBinOrder pins the resolution order, which is now TWO sources and a refusal:
// SOROQ_FLUTTER_BIN -> the frontend recorded for THIS home -> error.
//
// The legacy `~/development/soroq-forks` fallback this used to pin is GONE, and case (a) below now
// asserts its absence. It could select a frontend the developer never chose, from a path that is not
// the frontend store, with nothing said in the output — the same class as the `soroq-flutter` PATH
// shim removed alongside it. An external frontend is still reachable, but only by naming it.
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

	// (a) With nothing recorded, a legacy ~/development checkout must NOT be selected. It is present
	//     on disk for this assertion precisely so its absence from the result is a measurement.
	if got, err := resolveSoroqFlutterBin(); err == nil {
		t.Fatalf("a ~/development checkout was selected with nothing recorded: %q", got)
	} else if got == legacyBin {
		t.Fatalf("the removed legacy fallback is back: %q", got)
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

	// (d) Remove the recorded install: clear error naming both routes.
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
	  "dart_revision": "` + expectedDartRevision + `",
	  "engine_revision": "` + repeatHex(40) + `",
	  "compatible_toolchain_ids": ["soroq-ios-3.44.2-production"],
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
	// A DIFFERENT but well-formed revision is now ACCEPTED, and that is the point of catalog v2: the
	// matrix pins 3.44.2 and 3.44.9 for iOS at once, so an equality check against one compiled-in
	// revision would refuse the other outright. The signature over the manifest is what authorises the
	// identity; what binds it to reality is verifyFrontendTreeMatchesManifest, which proves the extracted
	// tree declares these same revisions.
	m.FlutterRevision = "0000000000000000000000000000000000000000"
	if err := checkFrontendIdentity(m); err != nil {
		t.Fatalf("a well-formed non-default revision must be accepted under the version matrix: %v", err)
	}
	// What is still refused is a manifest that identifies NOTHING, or pairs with nothing.
	m.FlutterRevision = "not-a-revision"
	if err := checkFrontendIdentity(m); err == nil {
		t.Fatal("expected a malformed flutter_revision to be refused")
	}
	m.FlutterRevision = expectedFlutterRevision
	m.EngineRevision = ""
	if err := checkFrontendIdentity(m); err == nil {
		t.Fatal("expected a manifest with no engine_revision to be refused")
	}
	m.EngineRevision = repeatHex(40)
	m.CompatibleToolchainIDs = nil
	if err := checkFrontendIdentity(m); err == nil {
		t.Fatal("expected a frontend that pairs with no toolchain to be refused")
	}
}

func repeatHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
