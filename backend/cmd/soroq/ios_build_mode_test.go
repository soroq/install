package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEngineJSON(t *testing.T, dir, buildMode, tier string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"schema": "soroq.ios_engine.v2", "arch": "arm64"}
	if buildMode != "" {
		m["build_mode"] = buildMode
	}
	if tier != "" {
		m["tier"] = tier
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "engine.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// THE DEFECT THIS SUITE EXISTS FOR. `flutter build ios --profile` was hard-coded, so a production
// release-mode toolchain was consumed by a PROFILE app build. A profile build embeds the VM service
// and is not an App-Store release, so the catalog would have advertised "production" for something a
// developer could not ship. The directory names cannot settle it either: the packed layout keeps the
// historical `ios_profile` name whatever the engine inside it is.
func TestIOSBuildModeFollowsTheEnginesOwnDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name, buildMode, tier, wantFlag, wantErr string
	}{
		{"production release builds release", "release", "production", "--release", ""},
		{"experimental release builds release", "release", "experimental_release", "--release", ""},
		{"profile toolchain keeps building profile", "profile", "experimental_profile", "--profile", ""},
		{"a PRODUCTION tier may never be profile", "profile", "production", "", "must be a release engine"},
		{"an unknown mode is refused, not guessed", "jit", "production", "", "unknown build_mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := iosFlutterBuildModeFlag(tc.buildMode, tc.tier)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected a refusal, got %q", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("refusal does not explain itself: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.wantFlag {
				t.Fatalf("build mode flag = %q, want %q", got, tc.wantFlag)
			}
		})
	}
}

// NO PROFILE FALLBACK. A release toolchain that somehow reaches the profile branch must fail rather
// than build something shippable-looking in the wrong mode.
func TestIOSBuildModeHasNoProfileFallback(t *testing.T) {
	if _, err := iosFlutterBuildModeFlag("", "production"); err == nil {
		t.Fatal("an empty build mode resolved to a flag instead of refusing")
	}
}

// The mode comes from engine.json, and a bundle that declares none is refused rather than defaulted.
func TestIOSToolchainEngineModeReadsTheBundle(t *testing.T) {
	dir := t.TempDir()
	writeEngineJSON(t, dir, "release", "production")
	mode, tier, err := iosToolchainEngineMode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "release" || tier != "production" {
		t.Fatalf("read %q/%q, want release/production", mode, tier)
	}

	bare := t.TempDir()
	writeEngineJSON(t, bare, "", "production")
	if _, _, err := iosToolchainEngineMode(bare); err == nil {
		t.Fatal("a bundle declaring no build_mode was given one")
	} else if !strings.Contains(err.Error(), "no build_mode") {
		t.Fatalf("refusal does not explain itself: %v", err)
	}

	if _, _, err := iosToolchainEngineMode(t.TempDir()); err == nil {
		t.Fatal("a bundle with no engine.json resolved a mode")
	}
}

// The framework plist must state the engine's real mode. It was the constant "profile", so a release
// engine described itself as a profile one to anything that read it.
func TestIOSFrameworkPlistStatesTheRealBuildMode(t *testing.T) {
	rel := iosFrameworkInfoPlistFor("release")
	if !strings.Contains(rel, "<key>BuildMode</key>\n  <string>release</string>") {
		t.Fatalf("release plist does not declare release:\n%s", rel)
	}
	if strings.Contains(rel, "<string>profile</string>") {
		t.Fatal("the release plist still contains the hard-coded profile value")
	}
	prof := iosFrameworkInfoPlistFor("profile")
	if !strings.Contains(prof, "<key>BuildMode</key>\n  <string>profile</string>") {
		t.Fatalf("profile plist regressed:\n%s", prof)
	}
}

// THE REAL BUNDLE, when it is present: the transferred production engine must resolve to --release.
// Skipped rather than faked when the packet is not on this machine.
func TestIOSBuildModeAgainstTheTransferredProductionBundle(t *testing.T) {
	bundle := "/Users/shrey/soroq-prod-r1/verify/ios"
	if _, err := os.Stat(filepath.Join(bundle, "engine.json")); err != nil {
		t.Skip("the transferred production bundle is not on this machine")
	}
	mode, tier, err := iosToolchainEngineMode(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "release" || tier != "production" {
		t.Fatalf("the production bundle reads %q/%q", mode, tier)
	}
	flag, err := iosFlutterBuildModeFlag(mode, tier)
	if err != nil {
		t.Fatal(err)
	}
	if flag != "--release" {
		t.Fatalf("the production bundle would be built with %q", flag)
	}
}
