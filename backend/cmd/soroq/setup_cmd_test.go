package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/signing"
)

// setupTestSigner installs an EPHEMERAL keypair as the in-process pinned key and returns a signer for it,
// so the test signs catalogs that verify against the freshly-pinned key (never the committed prod key).
func setupTestSigner(t *testing.T) *signing.ToolchainManifestSigner {
	t.Helper()
	seedB64 := usePinnedToolchainKey(t)
	seed, err := signing.DecodeToolchainSeed(seedB64)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := signing.NewToolchainSignerFromSeed(seed, toolchainPinnedKeyID)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// catalogTestServer serves an arbitrary body at /v1/catalog and a signature at /v1/catalog.sig. The
// tamper closure lets a test corrupt the served bytes/sig AFTER signing to prove the signature refusal.
func catalogTestServer(t *testing.T, body []byte, sigHex string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/catalog.sig"):
			w.Write([]byte(sigHex))
		case strings.HasSuffix(r.URL.Path, "/catalog"):
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func signBytes(t *testing.T, signer *signing.ToolchainManifestSigner, b []byte) string {
	t.Helper()
	sig, err := signer.SignToolchainManifest(b)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// stubInstalls replaces the install seams with recorders that succeed without touching the network or
// soroqctl, capturing the args each was called with. This proves the verify -> resolve -> record path of
// W-B; the REAL install boundary is runFrontendInstall / runToolchainInstall (exercised by their own
// safety tests). Restored on cleanup.
func stubInstalls(t *testing.T) (frontendCalls *[][]string, toolchainCalls *[][]string) {
	t.Helper()
	var fcalls, tcalls [][]string
	prevF, prevT := installFrontend, installToolchain
	prevPreflight := catalogReferencePreflightFn
	installFrontend = func(args []string) error { fcalls = append(fcalls, args); return nil }
	installToolchain = func(args []string) error { tcalls = append(tcalls, args); return nil }
	catalogReferencePreflightFn = func(_ string, doc catalogDoc, platforms []string) (map[string]catalogPlatformPreflight, error) {
		out := make(map[string]catalogPlatformPreflight, len(platforms))
		for _, platform := range platforms {
			entry, err := doc.entryForPlatform(platform)
			if err != nil {
				return nil, err
			}
			out[platform] = catalogPlatformPreflight{
				Platform: platform,
				Entry:    entry,
				Toolchain: cliManifest{
					Platform: platform, BuildMode: "release", Tier: "production",
				},
			}
		}
		return out, nil
	}
	t.Cleanup(func() {
		installFrontend = prevF
		installToolchain = prevT
		catalogReferencePreflightFn = prevPreflight
	})
	return &fcalls, &tcalls
}

func TestSetupPreflightsEveryRequestedPairBeforeAnyInstall(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir())
	doc := catalogDoc{
		Schema: catalogSchema,
		Platforms: map[string]catalogPlatform{
			"android": {FrontendVersion: "frontend-A", ToolchainVersion: "toolchain-A"},
			"ios":     {FrontendVersion: "frontend-I", ToolchainVersion: "missing-toolchain-I"},
		},
	}
	body, _ := json.MarshalIndent(doc, "", "  ")
	srv := catalogTestServer(t, body, signBytes(t, signer, body))

	var fcalls, tcalls int
	prevF, prevT, prevPreflight := installFrontend, installToolchain, catalogReferencePreflightFn
	installFrontend = func([]string) error { fcalls++; return nil }
	installToolchain = func([]string) error { tcalls++; return nil }
	catalogReferencePreflightFn = func(_ string, _ catalogDoc, platforms []string) (map[string]catalogPlatformPreflight, error) {
		if len(platforms) != 2 || platforms[0] != "android" || platforms[1] != "ios" {
			t.Fatalf("preflight platforms = %v", platforms)
		}
		return nil, errors.New("REFUSED: catalog ios toolchain missing-toolchain-I: manifest fetch: 404")
	}
	t.Cleanup(func() {
		installFrontend, installToolchain, catalogReferencePreflightFn = prevF, prevT, prevPreflight
	})

	err := runSetup([]string{"--platforms", "android,ios", "--api", srv.URL})
	if err == nil || !strings.Contains(err.Error(), "catalog artifact preflight") || !strings.Contains(err.Error(), "missing-toolchain-I") {
		t.Fatalf("expected exact preflight refusal, got %v", err)
	}
	if fcalls != 0 || tcalls != 0 {
		t.Fatalf("install side effects before all-pair preflight completed: frontend=%d toolchain=%d", fcalls, tcalls)
	}
	// Zero installs is necessary but not sufficient: the ACTIVE POINTER must also be untouched, or a
	// refused setup would still repoint the toolchain a later build resolves. active.json is written
	// per-platform after both installs succeed, so a partial write here would mean the android leg
	// committed state before the ios leg was known to be broken.
	if _, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".soroq", "toolchains", "active.json")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused preflight left an active-toolchain pointer behind (stat err = %v)", statErr)
	}
}

// (a) setup android: verifies -> resolves -> installs -> writes active.json with the right toolchain.
func TestSetupAndroidResolvesAndRecords(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir())

	doc := catalogDoc{
		Schema: catalogSchema,
		Platforms: map[string]catalogPlatform{
			"android": {FrontendVersion: "frontend-A", ToolchainVersion: "toolchain-A"},
			"ios":     {FrontendVersion: "frontend-I", ToolchainVersion: "toolchain-I"},
		},
	}
	body, _ := json.MarshalIndent(doc, "", "  ")
	srv := catalogTestServer(t, body, signBytes(t, signer, body))
	fcalls, tcalls := stubInstalls(t)

	if err := runSetup([]string{"android", "--api", srv.URL}); err != nil {
		t.Fatalf("setup android failed: %v", err)
	}

	// The existing installers were called (as libraries) with the RESOLVED versions from the catalog.
	if len(*fcalls) != 1 || (*fcalls)[0][0] != "frontend-A" {
		t.Fatalf("frontend install not called with resolved version: %v", *fcalls)
	}
	if len(*tcalls) != 1 || (*tcalls)[0][0] != "toolchain-A" {
		t.Fatalf("toolchain install not called with resolved version: %v", *tcalls)
	}

	// active.json records the android toolchain (and NOT ios, which was not requested).
	active, err := loadActiveToolchains()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := active.Platforms["android"]
	if !ok {
		t.Fatalf("android not recorded in active.json: %+v", active)
	}
	if got.ToolchainVersion != "toolchain-A" || got.FrontendVersion != "frontend-A" {
		t.Fatalf("android active entry wrong: %+v", got)
	}
	if _, ok := active.Platforms["ios"]; ok {
		t.Fatalf("ios must not be recorded when only android was requested: %+v", active)
	}
}

// --platforms android,ios records BOTH per-platform without overwriting.
func TestSetupBothPlatformsRecordedSeparately(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir())

	doc := catalogDoc{
		Schema: catalogSchema,
		Platforms: map[string]catalogPlatform{
			"android": {FrontendVersion: "frontend-A", ToolchainVersion: "toolchain-A"},
			"ios":     {FrontendVersion: "frontend-I", ToolchainVersion: "toolchain-I"},
		},
	}
	body, _ := json.MarshalIndent(doc, "", "  ")
	srv := catalogTestServer(t, body, signBytes(t, signer, body))
	stubInstalls(t)

	if err := runSetup([]string{"--platforms", "android,ios", "--api", srv.URL}); err != nil {
		t.Fatalf("setup --platforms android,ios failed: %v", err)
	}
	active, err := loadActiveToolchains()
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := active.Platforms["android"]; !ok || a.ToolchainVersion != "toolchain-A" {
		t.Fatalf("android not recorded correctly: %+v", active)
	}
	if i, ok := active.Platforms["ios"]; !ok || i.ToolchainVersion != "toolchain-I" {
		t.Fatalf("ios not recorded correctly: %+v", active)
	}
}

// (b) REFUSES a tampered signature (bytes changed after signing -> signature no longer matches).
func TestSetupRefusesTamperedSignature(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir())

	doc := catalogDoc{
		Schema:    catalogSchema,
		Platforms: map[string]catalogPlatform{"android": {FrontendVersion: "frontend-A", ToolchainVersion: "toolchain-A"}},
	}
	body, _ := json.MarshalIndent(doc, "", "  ")
	sig := signBytes(t, signer, body)
	// Tamper: serve a body that differs from what was signed. The verify over raw bytes must fail.
	tampered := append([]byte(nil), body...)
	tampered = append(tampered, ' ')
	srv := catalogTestServer(t, tampered, sig)
	fcalls, tcalls := stubInstalls(t)

	err := runSetup([]string{"android", "--api", srv.URL})
	if err == nil {
		t.Fatal("expected REFUSED on tampered signature, got nil")
	}
	if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(strings.ToLower(err.Error()), "signature") {
		t.Fatalf("expected a signature REFUSED, got: %v", err)
	}
	if len(*fcalls) != 0 || len(*tcalls) != 0 {
		t.Fatal("a refused catalog must not trigger any install")
	}
	if active, _ := loadActiveToolchains(); len(active.Platforms) != 0 {
		t.Fatal("a refused catalog must not record anything")
	}
}

// (c) REFUSES a validly-signed but WRONG-SCHEMA doc: a signed TOOLCHAIN manifest served at the catalog
// route. The signature verifies (same pinned key) — the SCHEMA-GATE is what refuses it.
func TestSetupRefusesWrongSchemaSignedDoc(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir())

	// A real, VALIDLY-SIGNED soroq.toolchain.v1 manifest (wrong schema for the catalog route).
	decoy := cliManifest{
		Schema:                toolchainManifestSchema,
		SoroqToolchainVersion: "toolchain-decoy",
		Platform:              "android",
	}
	body, _ := json.MarshalIndent(decoy, "", "  ")
	srv := catalogTestServer(t, body, signBytes(t, signer, body)) // signature is GENUINE
	fcalls, tcalls := stubInstalls(t)

	err := runSetup([]string{"android", "--api", srv.URL})
	if err == nil {
		t.Fatal("expected REFUSED on wrong-schema doc, got nil")
	}
	if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("expected a schema REFUSED (not a signature error), got: %v", err)
	}
	if len(*fcalls) != 0 || len(*tcalls) != 0 {
		t.Fatal("a wrong-schema catalog must not trigger any install")
	}
}

// (d) REFUSES an absent platform entry (catalog signed + right schema, but no ios entry).
func TestSetupRefusesAbsentPlatform(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir())

	doc := catalogDoc{
		Schema:    catalogSchema,
		Platforms: map[string]catalogPlatform{"android": {FrontendVersion: "frontend-A", ToolchainVersion: "toolchain-A"}},
	}
	body, _ := json.MarshalIndent(doc, "", "  ")
	srv := catalogTestServer(t, body, signBytes(t, signer, body))
	fcalls, tcalls := stubInstalls(t)

	err := runSetup([]string{"ios", "--api", srv.URL})
	if err == nil {
		t.Fatal("expected REFUSED on absent platform, got nil")
	}
	if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "no entry for platform") {
		t.Fatalf("expected an absent-platform REFUSED, got: %v", err)
	}
	if len(*fcalls) != 0 || len(*tcalls) != 0 {
		t.Fatal("an absent platform must not trigger any install")
	}
}
