package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An iOS patch is built with the toolchain whose ENGINE built its base, read from the base's baseline,
// whatever the active frontend prefers (an R9 base on a machine whose active toolchain is R10).
func TestDeriveIOSPatchToolchainFromBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := zeroTouchFixture(t, "void main() {}\n")
	soroqBytes, err := readProjectSoroqYAML(dir)
	if err != nil {
		t.Fatal(err)
	}
	pubBytes, _ := os.ReadFile(filepath.Join(dir, "pubspec.yaml"))
	meta, err := buildSoroqBundledMetadata(soroqBytes, pubBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := deriveIOSPatchToolchainFromBase(dir); ok {
		t.Fatal("no baseline yet: nothing to derive")
	}
	writeFixtureFile(t, freehandReleaseDir(dir, strings.ToLower(meta.Soroq.RuntimeID)), "baseline.json",
		`{"engine_revision":"soroq.ios_engine.x.r9"}`)
	tool := func(name, rev string) {
		writeFixtureFile(t, filepath.Join(home, ".soroq", "toolchains", name, "ios"), "engine.json",
			`{"soroq_engine_revision":"`+rev+`"}`)
	}
	tool("soroq-ios-3.44.9-r10", "soroq.ios_engine.x.r10")
	tool("soroq-ios-3.44.9-r9", "soroq.ios_engine.x.r9")
	if got, ok := deriveIOSPatchToolchainFromBase(dir); !ok || got != "soroq-ios-3.44.9-r9" {
		t.Fatalf("derived %q (ok=%v), want the R9 toolchain that built the base", got, ok)
	}
	// Two directories claiming the base's engine: ambiguous, so the caller derives as before.
	tool("soroq-ios-3.44.9-r9-copy", "soroq.ios_engine.x.r9")
	if got, ok := deriveIOSPatchToolchainFromBase(dir); ok {
		t.Fatalf("ambiguous engine must not pick one, got %q", got)
	}
}
