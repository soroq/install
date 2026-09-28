package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A platform's build must use the frontend soroq.lock pins for THAT platform, not whichever one
// `soroq setup` activated last. The Campus app's first Android release was built with the iOS frontend
// that way and came out arm64-only with engine-less 32-bit directories.
func TestBuildUsesTheFrontendPinnedForItsPlatform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SOROQ_FLUTTER_BIN", "")
	install := func(version string) string {
		bin := filepath.Join(home, ".soroq", "frontends", version, defaultFrontendSubdir, "bin", "flutter")
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return bin
	}
	androidBin := install("fe-android-multiabi")
	iosBin := install("fe-ios-r6")
	writeActiveFrontend(t, home, "fe-ios-r6") // setup installed iOS last

	project := t.TempDir()
	if err := saveSoroqLock(project, soroqLock{Platforms: map[string]soroqLockPin{
		"android": {FrontendVersion: "fe-android-multiabi", ToolchainVersion: "tc-a"},
		"ios":     {FrontendVersion: "fe-ios-r6", ToolchainVersion: "tc-i"},
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { platformFrontendPin = soroqFrontendChoice{} })

	pinFrontendForPlatform(project, "android")
	got, err := resolveSoroqFlutterFrontend()
	if err != nil || got.Bin != androidBin {
		t.Fatalf("android build resolved %q (%v); want the android pin %q", got.Bin, err, androidBin)
	}
	if !strings.Contains(got.Provenance, "soroq.lock pin for android") {
		t.Errorf("provenance %q does not say where the choice came from", got.Provenance)
	}

	pinFrontendForPlatform(project, "ios")
	if got, _ := resolveSoroqFlutterFrontend(); got.Bin != iosBin {
		t.Fatalf("ios build resolved %q; want the ios pin %q", got.Bin, iosBin)
	}

	// An explicit SOROQ_FLUTTER_BIN still wins.
	t.Setenv("SOROQ_FLUTTER_BIN", "/explicit/flutter")
	pinFrontendForPlatform(project, "android")
	if got, _ := resolveSoroqFlutterFrontend(); got.Bin != "/explicit/flutter" {
		t.Errorf("explicit SOROQ_FLUTTER_BIN was overridden by the lock pin: %q", got.Bin)
	}
	t.Setenv("SOROQ_FLUTTER_BIN", "")

	// A pin that is not installed leaves the active frontend in charge (with a warning).
	if err := saveSoroqLock(project, soroqLock{Platforms: map[string]soroqLockPin{
		"android": {FrontendVersion: "fe-not-installed"},
	}}); err != nil {
		t.Fatal(err)
	}
	pinFrontendForPlatform(project, "android")
	if platformFrontendPin.Bin != "" {
		t.Errorf("an uninstalled pin was selected: %+v", platformFrontendPin)
	}
}
