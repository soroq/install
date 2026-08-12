package main

// COMBINED-PLATFORM RELEASE IDENTITY.
//
// `soroq release --platforms=android,ios` dispatches the SAME derived flags to two lanes. The backend
// Release model stores ONE platform per record and keys releases by a globally-unique ID, so a derived
// id without a platform component makes the two platforms fight over one record: whichever runs second
// either collides or silently rebinds the first platform's release to its own runtime.
//
// This is a correctness property of the identity itself, so it is asserted on the derivation rather
// than on a mocked server: if the two ids are equal, no amount of server behaviour makes the combined
// command safe.

import (
	"path/filepath"
	"strings"
	"testing"
)

// pinnedToolchain supplies an explicit --toolchain so these tests exercise RELEASE-ID derivation only.
// Without it, withDerivedFlags also derives a toolchain by reading ~/.soroq/toolchains, which makes the
// result depend on what the developer happens to have installed — and correctly refuses outright once
// two toolchains for a platform are installed and the active frontend declares no compatibility list.
func pinnedToolchain(extra ...string) []string {
	return append([]string{"--toolchain", "test-pinned-toolchain"}, extra...)
}

func projectWithIdentity(t *testing.T, appID, version string) string {
	t.Helper()
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "soroq.yaml"), "app_id: "+appID+"\nchannel: stable\n")
	mustWriteFile(t, filepath.Join(dir, "pubspec.yaml"), "name: app\nversion: "+version+"\n")
	return dir
}

// THE PREDICTED DEFECT: both platforms must not derive the same release id.
func TestCombinedPlatformsDeriveDistinctReleaseIDs(t *testing.T) {
	dir := projectWithIdentity(t, "dev.soroq.canonapp", "1.0.0+1")

	android, err := withDerivedFlags("android", dir, pinnedToolchain())
	if err != nil {
		t.Fatalf("android derivation: %v", err)
	}
	ios, err := withDerivedFlags("ios", dir, pinnedToolchain())
	if err != nil {
		t.Fatalf("ios derivation: %v", err)
	}

	aID, aOK := flagValue(android, "release-id")
	iID, iOK := flagValue(ios, "release-id")
	if !aOK || !iOK {
		t.Fatalf("release-id not derived (android=%v ios=%v)", aOK, iOK)
	}
	if aID == iID {
		t.Fatalf("Android and iOS derived the SAME release id %q. The backend keys releases by id and "+
			"stores one platform per record, so the second platform would collide with or silently "+
			"rebind the first platform's release.", aID)
	}
	for name, id := range map[string]string{"android": aID, "ios": iID} {
		if !strings.Contains(id, name) {
			t.Errorf("%s release id %q does not identify its platform", name, id)
		}
	}
}

// Determinism: the same project must always derive the same ids, so a re-run targets the same release
// instead of creating a new one each time.
func TestDerivedReleaseIDsAreDeterministic(t *testing.T) {
	dir := projectWithIdentity(t, "dev.soroq.canonapp", "1.0.0+1")
	for _, platform := range []string{"android", "ios"} {
		first, err := withDerivedFlags(platform, dir, pinnedToolchain())
		if err != nil {
			t.Fatal(err)
		}
		second, err := withDerivedFlags(platform, dir, pinnedToolchain())
		if err != nil {
			t.Fatal(err)
		}
		a, _ := flagValue(first, "release-id")
		b, _ := flagValue(second, "release-id")
		if a != b {
			t.Errorf("%s release id is not deterministic: %q vs %q", platform, a, b)
		}
	}
}

// A version bump must produce a NEW release id; the app version is what a store release is identified
// by, and reusing an id across versions would rebind an existing release.
func TestVersionBumpChangesTheReleaseID(t *testing.T) {
	oldDir := projectWithIdentity(t, "dev.soroq.canonapp", "1.0.0+1")
	newDir := projectWithIdentity(t, "dev.soroq.canonapp", "1.1.0+2")
	o, err := withDerivedFlags("ios", oldDir, pinnedToolchain())
	if err != nil {
		t.Fatal(err)
	}
	n, err := withDerivedFlags("ios", newDir, pinnedToolchain())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := flagValue(o, "release-id")
	b, _ := flagValue(n, "release-id")
	if a == b {
		t.Errorf("a version bump reused release id %q", a)
	}
}

// BACKWARD COMPATIBILITY: an explicit --release-id must be honoured verbatim for BOTH platforms, since
// that is the existing contract for platform-specific commands and CI.
func TestExplicitReleaseIDIsNeverOverridden(t *testing.T) {
	dir := projectWithIdentity(t, "dev.soroq.canonapp", "1.0.0+1")
	for _, platform := range []string{"android", "ios"} {
		out, err := withDerivedFlags(platform, dir, pinnedToolchain("--release-id", "my-own-id"))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := flagValue(out, "release-id")
		if got != "my-own-id" {
			t.Errorf("%s: explicit --release-id was changed to %q", platform, got)
		}
		if strings.Count(strings.Join(out, " "), "--release-id") != 1 {
			t.Errorf("%s: --release-id appears more than once: %v", platform, out)
		}
	}
}

// An app id with no dots, and a missing version, must still yield a usable platform-qualified id.
func TestDegenerateProjectIdentityStillQualifiesByPlatform(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "soroq.yaml"), "app_id: bare\nchannel: stable\n")
	mustWriteFile(t, filepath.Join(dir, "pubspec.yaml"), "name: app\n")
	a := deriveReleaseIDForPlatform(dir, "android")
	i := deriveReleaseIDForPlatform(dir, "ios")
	if a == "" || i == "" {
		t.Fatalf("empty release id (android=%q ios=%q)", a, i)
	}
	if a == i {
		t.Fatalf("degenerate identity collided across platforms: %q", a)
	}
}

// THE LOCAL CORRUPTION VECTOR. Release ids also key the on-disk baseline directory
// (.soroq/releases/<release-id>/). Distinct ids on the server are not enough: if the two platforms
// share a directory, the second platform's baseline lands on top of the first's, locally and silently,
// with no server involved.
func TestPlatformsGetDistinctOnDiskBaselineDirectories(t *testing.T) {
	dir := projectWithIdentity(t, "dev.soroq.canonapp", "1.0.0+1")
	seen := map[string]string{}
	for _, platform := range []string{"android", "ios"} {
		id := deriveReleaseIDForPlatform(dir, platform)
		stash := projectReleaseArtifactPath(dir, id, "app-release.aab")
		baselineDir := filepath.Dir(stash)
		if other, clash := seen[baselineDir]; clash {
			t.Fatalf("%s and %s share baseline directory %s; the second build would overwrite the first",
				other, platform, baselineDir)
		}
		seen[baselineDir] = platform
		if !strings.Contains(baselineDir, id) {
			t.Errorf("%s baseline dir %s is not keyed by its release id %s", platform, baselineDir, id)
		}
	}
}

// Project state must retain the release id INDEPENDENTLY per platform, so recording one platform's
// release never destroys the other's, and each platform resolves back to ITS OWN release and runtime.
// Written through the real persistence path, then re-read from disk.
func TestProjectStateRetainsBothPlatformReleaseIDsIndependently(t *testing.T) {
	dir := projectWithIdentity(t, "dev.soroq.canonapp", "1.0.0+1")
	androidID := deriveReleaseIDForPlatform(dir, "android")
	iosID := deriveReleaseIDForPlatform(dir, "ios")

	if err := saveProjectCLIState(dir, projectCLIState{
		SchemaVersion: 1,
		LastAndroidRelease: &androidReleaseState{
			AppID: "dev.soroq.canonapp", Channel: "stable", ReleaseID: androidID,
			RuntimeID: "rt-android", Version: "1.0.0", Arch: "arm64",
		},
		LastIOSRelease: &iosReleaseState{
			AppID: "dev.soroq.canonapp", Channel: "stable", ReleaseID: iosID,
			RuntimeID: "rt-ios", Version: "1.0.0", Arch: "arm64",
		},
	}); err != nil {
		t.Fatalf("save project state: %v", err)
	}

	state, err := loadProjectCLIState(dir)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if state.LastAndroidRelease == nil || state.LastIOSRelease == nil {
		t.Fatalf("a platform's release state was lost: android=%v ios=%v",
			state.LastAndroidRelease, state.LastIOSRelease)
	}
	if state.LastAndroidRelease.ReleaseID != androidID {
		t.Errorf("android release id is %q, want %q", state.LastAndroidRelease.ReleaseID, androidID)
	}
	if state.LastIOSRelease.ReleaseID != iosID {
		t.Errorf("ios release id is %q, want %q", state.LastIOSRelease.ReleaseID, iosID)
	}

	// Runtime identity stays isolated: the two platforms must not share a runtime id.
	if state.LastAndroidRelease.RuntimeID == state.LastIOSRelease.RuntimeID {
		t.Error("the two platforms recorded the SAME runtime id; runtime identity must stay isolated")
	}

	// CROSS-ATTACHMENT: the resolver each downstream command uses (rollback, patch) must hand each
	// platform its OWN release, never the other's.
	for platform, wantRelease := range map[string]string{"android": androidID, "ios": iosID} {
		rec := recordedReleaseFor(platform, state)
		if rec == nil {
			t.Errorf("%s resolved no recorded release", platform)
			continue
		}
		if rec.ReleaseID != wantRelease {
			t.Errorf("%s resolved release %q, want %q (a platform must never be attached to the other's release)",
				platform, rec.ReleaseID, wantRelease)
		}
	}
}
