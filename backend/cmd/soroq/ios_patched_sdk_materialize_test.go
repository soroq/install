package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stock Flutter patched SDK declares none of the soroq dart:_internal intrinsics, so a lane built
// against it fails inside kernel_snapshot_program with "Method not found: 'soroqRedirectToPatch'" before
// any application Dart is compiled. materializeIOSSoroqPatchedSDK is what puts the packed SOROQ platform
// dill in the lane instead. Each test below is paired with the planted failure it must catch.

func writeIOSBundleForPatchedSDK(t *testing.T, platformStrong []byte, declaredSHA string) (bundleDir, commonEngine string) {
	t.Helper()
	root := t.TempDir()
	bundleDir = filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if platformStrong != nil {
		if err := os.WriteFile(filepath.Join(bundleDir, "platform_strong"), platformStrong, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	arts := map[string]string{}
	if declaredSHA != "" {
		arts["platform_strong"] = declaredSHA
	}
	raw, err := json.Marshal(map[string]any{"schema": "soroq.ios_engine.v2", "artifacts": arts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "engine.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// The stock cache the old code linked wholesale; only its outline is still used.
	commonEngine = filepath.Join(root, "cache", "artifacts", "engine", "common")
	stock := filepath.Join(commonEngine, "flutter_patched_sdk")
	if err := os.MkdirAll(stock, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"platform_strong.dill", "vm_outline_strong.dill"} {
		if err := os.WriteFile(filepath.Join(stock, n), []byte("STOCK-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return bundleDir, commonEngine
}

// POSITIVE CONTROL. A declared, hash-matching platform_strong must land in the lane, replacing the stock
// link. Without this the other tests could pass with a function that refuses everything.
func TestSoroqPatchedSDKReplacesTheStockPlatformDill(t *testing.T) {
	packed := []byte("SOROQ-platform-with-soroqRedirectToPatch")
	bundleDir, commonEngine := writeIOSBundleForPatchedSDK(t, packed, sha256Hex(packed))
	lane := filepath.Join(t.TempDir(), "ios_profile")
	// Reproduce the pre-fix state: the lane's patched sdk is a symlink into the stock cache.
	if err := os.MkdirAll(lane, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(commonEngine, "flutter_patched_sdk"), filepath.Join(lane, "flutter_patched_sdk")); err != nil {
		t.Fatal(err)
	}
	if err := materializeIOSSoroqPatchedSDK(bundleDir, commonEngine, lane); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(lane, "flutter_patched_sdk", "platform_strong.dill"))
	if err != nil {
		t.Fatalf("lane platform dill: %v", err)
	}
	if string(got) != string(packed) {
		t.Fatalf("lane carries %q, want the packed SOROQ dill", got)
	}
	// The stock cache must never be mutated by the overlay.
	stockDill, err := os.ReadFile(filepath.Join(commonEngine, "flutter_patched_sdk", "platform_strong.dill"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stockDill) != "STOCK-platform_strong.dill" {
		t.Fatalf("the shared stock cache was mutated: %q", stockDill)
	}
	// The outline stays stock: the bundle packs none, and the program snapshot does not read it.
	outline, err := os.ReadFile(filepath.Join(lane, "flutter_patched_sdk", "vm_outline_strong.dill"))
	if err != nil {
		t.Fatalf("lane outline: %v", err)
	}
	if string(outline) != "STOCK-vm_outline_strong.dill" {
		t.Fatalf("outline is %q, want the stock one", outline)
	}
}

// PLANTED FAILURE 1 — the bundle declares nothing. This is the published clean-r4 toolchain. It must be
// refused with a message that names the artifact, not left to fail later as three "Method not found"
// lines against a generated bootstrap file.
func TestUndeclaredPatchedSDKIsRefusedAndNamesTheArtifact(t *testing.T) {
	bundleDir, commonEngine := writeIOSBundleForPatchedSDK(t, nil, "")
	lane := filepath.Join(t.TempDir(), "ios_profile")
	err := materializeIOSSoroqPatchedSDK(bundleDir, commonEngine, lane)
	if err == nil {
		t.Fatal("a bundle that declares no platform_strong was accepted; it cannot build a Soroq app")
	}
	if !strings.Contains(err.Error(), "platform_strong") {
		t.Fatalf("refusal does not name the missing artifact: %v", err)
	}
}

// PLANTED FAILURE 2 — declared but absent. Falling back to the stock SDK here would reproduce the exact
// silent failure this function exists to end.
func TestDeclaredButAbsentPatchedSDKIsRefused(t *testing.T) {
	bundleDir, commonEngine := writeIOSBundleForPatchedSDK(t, nil, sha256Hex([]byte("anything")))
	lane := filepath.Join(t.TempDir(), "ios_profile")
	if err := materializeIOSSoroqPatchedSDK(bundleDir, commonEngine, lane); err == nil {
		t.Fatal("a bundle declaring platform_strong with no such file was accepted")
	}
}

// PLANTED FAILURE 3 — the file is present but is not the declared bytes.
func TestPatchedSDKHashMismatchIsRefused(t *testing.T) {
	bundleDir, commonEngine := writeIOSBundleForPatchedSDK(t, []byte("SOMETHING-ELSE"), sha256Hex([]byte("declared")))
	lane := filepath.Join(t.TempDir(), "ios_profile")
	err := materializeIOSSoroqPatchedSDK(bundleDir, commonEngine, lane)
	if err == nil {
		t.Fatal("a platform_strong whose sha256 differs from engine.json was accepted")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("refusal does not mention the hash comparison: %v", err)
	}
}

// The PATCH lane resolves the same dill, and its lookup list was build-lane-only. A bundle that packs
// the artifact flat -- the canonical v2 layout -- then failed with a message describing a directory the
// bundle was never meant to have.
func TestPatchLaneFindsTheFlatPackedPlatformDill(t *testing.T) {
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "platform_strong"), []byte("dill"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := flutterProfilePlatformDillFromToolchain(bundle)
	if err != nil {
		t.Fatalf("flat packed platform_strong not found: %v", err)
	}
	if got != filepath.Join(bundle, "platform_strong") {
		t.Fatalf("resolved %q, want the packed artifact", got)
	}
}

// The older build-lane layout must keep working, or this fix would break every previously published
// toolchain that nests the dill.
func TestPatchLaneStillFindsTheBuildLaneDill(t *testing.T) {
	bundle := t.TempDir()
	nested := filepath.Join(bundle, "build_lane", "ios_profile", "flutter_patched_sdk")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "platform_strong.dill"), []byte("dill"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := flutterProfilePlatformDillFromToolchain(bundle)
	if err != nil {
		t.Fatalf("build-lane platform dill not found: %v", err)
	}
	if got != filepath.Join(nested, "platform_strong.dill") {
		t.Fatalf("resolved %q, want the build-lane dill", got)
	}
}

// A bundle with neither must still be refused, or the lookup is a guard that cannot fail.
func TestPatchLaneRefusesABundleWithNoPlatformDill(t *testing.T) {
	if _, err := flutterProfilePlatformDillFromToolchain(t.TempDir()); err == nil {
		t.Fatal("a bundle with no platform dill at all was accepted")
	}
}
