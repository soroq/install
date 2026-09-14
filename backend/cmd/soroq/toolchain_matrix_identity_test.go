package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE CLI MUST INSTALL ANY CORRECTLY SIGNED TOOLCHAIN, NOT ONE HARDCODED REVISION.
//
// `install` used to demand flutter_revision == a constant compiled into the CLI. A production-published,
// correctly signed Flutter 3.44.9 toolchain was therefore refused on a clean HOME with
//
//	REFUSED: flutter revision mismatch: ios manifest "6b182d2c7585", this CLI is wired for "f74781f62134"
//
// and every future matrix version would have needed a CLI edit and release. The signature is what
// authorises an identity; the CLI's remaining jobs are to check the identity is WELL-FORMED and that the
// extracted bundle AGREES with the signed manifest. These tests pin both halves, and the refusals.

const (
	// The historical production lane.
	r2Flutter = "f74781f6213447540225edae307acb48bbaaaf34"
	r2Dart    = "9576691c37d84d3b66a9722e4fadacc764f04b21"
	r2Engine  = "soroq.ios_engine.f74781f6_3499c008.production.r2"
	// The signed Flutter 3.44.9 lane the old check refused.
	psFlutter = "6b182d2c7585eba26d4edce0f97630effd256c33"
	psDart    = "d684a576a6aa954ae107a03b2b4e1d61c3bebe93"
	psEngine  = "soroq.ios_engine.6b182d2c_5a2a6a42.private_state.r1"
)

func iosManifest(flutter, dart, engine string) cliManifest {
	return cliManifest{
		Schema:                toolchainManifestSchema,
		SoroqToolchainVersion: "fixture",
		Platform:              "ios",
		Arch:                  "arm64",
		BuildMode:             "release",
		Tier:                  "production",
		FlutterRevision:       flutter,
		DartRevision:          dart,
		SoroqEngineRevision:   engine,
		Artifacts: []cliManifestArtifact{
			{Name: "gen_snapshot", SHA256: "aa", Size: 1},
			{Name: "platform_strong", SHA256: "bb", Size: 2},
		},
	}
}

// writeBundle lays down an engine.json for the cross-check to read.
func writeBundle(t *testing.T, doc map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "engine.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func matchingBundle(t *testing.T, m cliManifest) string {
	t.Helper()
	arts := map[string]string{}
	for _, a := range m.Artifacts {
		arts[a.Name] = a.SHA256
	}
	return writeBundle(t, map[string]any{
		"schema":                "soroq.ios_engine.v2",
		"soroq_engine_revision": m.SoroqEngineRevision,
		"flutter_commit":        m.FlutterRevision,
		"dart_revision":         m.DartRevision,
		"artifacts":             arts,
		"soroq_freehand_redirect_capabilities": map[string]any{
			"honoured_kinds":        []string{"method"},
			"identity_capabilities": []string{"private_enclosing_class_identity_v1"},
		},
	})
}

// --- BOTH LANES MUST INSTALL -------------------------------------------------------------------

func TestOlderProductionToolchainIdentityIsAccepted(t *testing.T) {
	m := iosManifest(r2Flutter, r2Dart, r2Engine)
	if err := checkToolchainIdentity(m); err != nil {
		t.Fatalf("the historical production identity was refused: %v", err)
	}
	if err := verifyManifestMatchesEngine(m, matchingBundle(t, m)); err != nil {
		t.Fatalf("a matching r2 bundle was refused: %v", err)
	}
}

// The exact case the defect report names.
func TestSignedFlutter3449ToolchainIdentityIsAccepted(t *testing.T) {
	m := iosManifest(psFlutter, psDart, psEngine)
	if err := checkToolchainIdentity(m); err != nil {
		t.Fatalf("the signed Flutter 3.44.9 identity was refused: %v", err)
	}
	if err := verifyManifestMatchesEngine(m, matchingBundle(t, m)); err != nil {
		t.Fatalf("a matching 3.44.9 bundle was refused: %v", err)
	}
}

// A FUTURE matrix version must need no CLI edit. This is the property that keeps the version matrix
// catalog-driven, so it is asserted with a revision that appears nowhere in this repository.
func TestAnUnknownFutureRevisionInstallsWithoutACLIEdit(t *testing.T) {
	m := iosManifest("0123456789abcdef0123456789abcdef01234567",
		"fedcba9876543210fedcba9876543210fedcba98",
		"soroq.ios_engine.future.rN")
	if err := checkToolchainIdentity(m); err != nil {
		t.Fatalf("a future signed revision was refused: %v", err)
	}
	if err := verifyManifestMatchesEngine(m, matchingBundle(t, m)); err != nil {
		t.Fatalf("a matching future bundle was refused: %v", err)
	}
}

// --- MISSING OR MALFORMED IDENTITY --------------------------------------------------------------

func TestMissingIdentityIsRefused(t *testing.T) {
	for name, m := range map[string]cliManifest{
		"no flutter revision": iosManifest("", psDart, psEngine),
		"no dart revision":    iosManifest(psFlutter, "", psEngine),
		"no engine revision":  iosManifest(psFlutter, psDart, ""),
	} {
		if err := checkToolchainIdentity(m); err == nil {
			t.Fatalf("%s: a manifest identifying nothing was accepted", name)
		}
	}
}

func TestMalformedRevisionIsRefused(t *testing.T) {
	for name, m := range map[string]cliManifest{
		"short flutter":       iosManifest("6b182d2c", psDart, psEngine),
		"non-hex flutter":     iosManifest(strings.Repeat("z", 40), psDart, psEngine),
		"short dart":          iosManifest(psFlutter, "d684a576", psEngine),
		"non-hex dart":        iosManifest(psFlutter, strings.Repeat("q", 40), psEngine),
		"whitespace revision": iosManifest("   ", psDart, psEngine),
	} {
		if err := checkToolchainIdentity(m); err == nil {
			t.Fatalf("%s: a malformed revision was accepted", name)
		}
	}
}

// Android legitimately publishes a dart VERSION STRING, so hex must not be demanded there.
func TestAndroidDartVersionStringIsAccepted(t *testing.T) {
	m := iosManifest(psFlutter, "3.13.0-103.1.beta", "soroq.android_engine.candidate")
	m.Platform = "android"
	m.Arch = "arm64-v8a"
	if err := checkToolchainIdentity(m); err != nil {
		t.Fatalf("android's dart version string was refused: %v", err)
	}
}

func TestUnsupportedPlatformIsRefused(t *testing.T) {
	m := iosManifest(psFlutter, psDart, psEngine)
	m.Platform = "windows"
	if err := checkToolchainIdentity(m); err == nil {
		t.Fatal("an unsupported platform was accepted")
	}
}

func TestBadBuildModeIsRefused(t *testing.T) {
	m := iosManifest(psFlutter, psDart, psEngine)
	m.BuildMode = "debug"
	if err := checkToolchainIdentity(m); err == nil {
		t.Fatal("build_mode debug was accepted")
	}
}

// --- MANIFEST / ENGINE DISAGREEMENT -------------------------------------------------------------
//
// This is what replaces the hardcoded coupling: a correctly signed manifest must not be able to front a
// bundle built from something else.

func TestManifestEngineIdentityMismatchIsRefused(t *testing.T) {
	m := iosManifest(psFlutter, psDart, psEngine)
	arts := map[string]string{"gen_snapshot": "aa", "platform_strong": "bb"}

	cases := map[string]map[string]any{
		"different engine revision": {"soroq_engine_revision": r2Engine, "flutter_commit": psFlutter, "dart_revision": psDart, "artifacts": arts},
		"different flutter commit":  {"soroq_engine_revision": psEngine, "flutter_commit": r2Flutter, "dart_revision": psDart, "artifacts": arts},
		"different dart revision":   {"soroq_engine_revision": psEngine, "flutter_commit": psFlutter, "dart_revision": r2Dart, "artifacts": arts},
		"engine names no identity":  {"flutter_commit": psFlutter, "dart_revision": psDart, "artifacts": arts},
	}
	for name, doc := range cases {
		doc["schema"] = "soroq.ios_engine.v2"
		if err := verifyManifestMatchesEngine(m, writeBundle(t, doc)); err == nil {
			t.Fatalf("%s: a disagreeing bundle was accepted", name)
		}
	}
}

func TestArtifactHashDisagreementIsRefused(t *testing.T) {
	m := iosManifest(psFlutter, psDart, psEngine)
	base := map[string]any{
		"schema": "soroq.ios_engine.v2", "soroq_engine_revision": psEngine,
		"flutter_commit": psFlutter, "dart_revision": psDart,
	}

	// a substituted artifact
	d1 := map[string]any{}
	for k, v := range base {
		d1[k] = v
	}
	d1["artifacts"] = map[string]string{"gen_snapshot": "SUBSTITUTED", "platform_strong": "bb"}
	if err := verifyManifestMatchesEngine(m, writeBundle(t, d1)); err == nil {
		t.Fatal("a substituted artifact hash was accepted")
	}

	// an artifact the manifest never signed for
	d2 := map[string]any{}
	for k, v := range base {
		d2[k] = v
	}
	d2["artifacts"] = map[string]string{"gen_snapshot": "aa", "platform_strong": "bb", "smuggled": "cc"}
	if err := verifyManifestMatchesEngine(m, writeBundle(t, d2)); err == nil {
		t.Fatal("an artifact absent from the signed manifest was accepted")
	}

	// a missing artifact
	d3 := map[string]any{}
	for k, v := range base {
		d3[k] = v
	}
	d3["artifacts"] = map[string]string{"gen_snapshot": "aa"}
	if err := verifyManifestMatchesEngine(m, writeBundle(t, d3)); err == nil {
		t.Fatal("a bundle missing a signed artifact was accepted")
	}
}

// --- CAPABILITIES ARE PART OF THE IDENTITY ------------------------------------------------------

func TestMalformedOrDuplicateCapabilitiesAreRefused(t *testing.T) {
	m := iosManifest(psFlutter, psDart, psEngine)
	arts := map[string]string{"gen_snapshot": "aa", "platform_strong": "bb"}
	mk := func(caps map[string]any) string {
		return writeBundle(t, map[string]any{
			"schema": "soroq.ios_engine.v2", "soroq_engine_revision": psEngine,
			"flutter_commit": psFlutter, "dart_revision": psDart, "artifacts": arts,
			"soroq_freehand_redirect_capabilities": caps,
		})
	}
	for name, caps := range map[string]map[string]any{
		"duplicate identity capability": {"honoured_kinds": []string{"method"},
			"identity_capabilities": []string{"private_enclosing_class_identity_v1", "private_enclosing_class_identity_v1"}},
		"empty identity capability": {"honoured_kinds": []string{"method"},
			"identity_capabilities": []string{""}},
		"blank honoured kind": {"honoured_kinds": []string{"  "},
			"identity_capabilities": []string{}},
	} {
		if err := verifyManifestMatchesEngine(m, mk(caps)); err == nil {
			t.Fatalf("%s: was accepted", name)
		}
	}

	// The well-formed case must still pass, or the checks above would be satisfied by refusing everything.
	if err := verifyManifestMatchesEngine(m, mk(map[string]any{
		"honoured_kinds":        []string{"method", "setter"},
		"identity_capabilities": []string{"private_enclosing_class_identity_v1"},
	})); err != nil {
		t.Fatalf("a well-formed capability block was refused: %v", err)
	}
}

// A bundle with no engine.json at all cannot be cross-checked, and must not pass by default.
func TestMissingEngineJSONIsRefused(t *testing.T) {
	if err := verifyManifestMatchesEngine(iosManifest(psFlutter, psDart, psEngine), t.TempDir()); err == nil {
		t.Fatal("a bundle with no engine.json was accepted")
	}
}
