package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func markToolchainInstalled(t *testing.T, home, version string) {
	t.Helper()
	dir := filepath.Join(home, ".soroq", "toolchains", version, "android")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "engine.json"), "{}")
}

// With several flavors the plain "android" pin is only the LATEST release. An older flavor's release
// must still be patched with the toolchain that built it (its per-flavor pin), and a patch that asks for
// another toolchain is refused exactly as for the latest release.
func TestSoroqLockKeepsAToolchainPinPerFlavor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	markToolchainInstalled(t, home, "tc-prod")
	markToolchainInstalled(t, home, "tc-dev")
	dir := t.TempDir()
	prod := soroqLockPin{ReleaseID: "rel-prod", Version: "1.0.0", ToolchainVersion: "tc-prod", Flavor: "prod"}
	dev := soroqLockPin{ReleaseID: "rel-dev", Version: "1.0.0", ToolchainVersion: "tc-dev", Flavor: "freeTier"}
	// What two flavored releases write, prod first: the plain pin ends up being dev's.
	for _, p := range []soroqLockPin{prod, dev} {
		if err := recordSoroqLockPin(dir, "android", p); err != nil {
			t.Fatal(err)
		}
		if err := recordSoroqLockPin(dir, soroqLockFlavorKey("android", p.Flavor), p); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(soroqLockPath(dir))
	for _, want := range []string{"  android:\n    release_id: rel-dev\n", "  android@prod:\n    release_id: rel-prod\n", "  android@freetier:\n"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("soroq.lock lacks %q:\n%s", want, raw)
		}
	}

	if got, err := resolveAndroidPatchToolchain(dir, "rel-prod", "", nil); err != nil || got != "tc-prod" {
		t.Fatalf("older flavor's base resolved to %q, %v; want its own pin tc-prod", got, err)
	}
	if _, err := resolveAndroidPatchToolchain(dir, "rel-prod", "tc-dev", nil); err == nil {
		t.Fatal("patching the prod base with the dev toolchain must be refused")
	}
	if got, err := resolveAndroidPatchToolchain(dir, "rel-dev", "", nil); err != nil || got != "tc-dev" {
		t.Fatalf("latest release: %q %v", got, err)
	}
	if p, ok := loadSoroqLockFlavorPin(dir, "android", "freeTier"); !ok || p.ReleaseID != "rel-dev" {
		t.Fatalf("flavor pin lookup is case-insensitive on the key: %+v %v", p, ok)
	}
	if _, ok := loadSoroqLockFlavorPin(dir, "android", "staging"); ok {
		t.Fatal("pin found for an unreleased flavor")
	}
	// Flavor keys are not platforms: rollback's platform inference still sees exactly one.
	writeFile(t, filepath.Join(dir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	if p, err := soleReleasedPlatform(dir); err != nil || p != "android" {
		t.Fatalf("rollback platform inference = %q, %v", p, err)
	}
}
