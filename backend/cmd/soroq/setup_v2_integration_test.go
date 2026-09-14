package main

// setup_v2_integration_test.go — the REAL `soroq setup` path under --catalog-v2.
//
// These go through runSetup itself (flag parsing, resolution, the install seams, and the active-toolchain
// record), not through helper functions, because the gap being closed was precisely that v2 had been
// proven only in helpers.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stubV2Installs records install calls and satisfies the v2 preflight seam without a registry.
func stubV2Installs(t *testing.T) (frontendCalls, toolchainCalls *[][]string) {
	t.Helper()
	var fcalls, tcalls [][]string
	prevF, prevT, prevPre := installFrontend, installToolchain, selectedPairPreflightFn
	installFrontend = func(args []string) error { fcalls = append(fcalls, args); return nil }
	installToolchain = func(args []string) error { tcalls = append(tcalls, args); return nil }
	selectedPairPreflightFn = func(_ string, sel catalogSelection) (catalogPlatformPreflight, error) {
		return catalogPlatformPreflight{
			Platform:  sel.Platform,
			Entry:     catalogPlatform{FrontendVersion: sel.FrontendVersion, ToolchainVersion: sel.ToolchainVersion},
			Toolchain: cliManifest{Platform: sel.Platform, BuildMode: "release", Tier: "production"},
		}, nil
	}
	t.Cleanup(func() {
		installFrontend, installToolchain, selectedPairPreflightFn = prevF, prevT, prevPre
	})
	return &fcalls, &tcalls
}

// bothCatalogServer serves a signed v1 and (optionally) a signed v2.
func bothCatalogServer(t *testing.T, signer interface {
	SignToolchainManifest([]byte) (string, error)
}, v1 []byte, v2 []byte) *httptest.Server {
	t.Helper()
	v1Sig, err := signer.SignToolchainManifest(v1)
	if err != nil {
		t.Fatal(err)
	}
	var v2Sig string
	if v2 != nil {
		if v2Sig, err = signer.SignToolchainManifest(v2); err != nil {
			t.Fatal(err)
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/catalog":
			w.Write(v1)
		case "/v1/catalog.sig":
			w.Write([]byte(v1Sig))
		case "/v2/catalog":
			if v2 == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(v2)
		case "/v2/catalog.sig":
			if v2 == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write([]byte(v2Sig))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func v1CatalogBytes() []byte {
	return []byte(`{"schema":"soroq.catalog.v1","generated_at":"2026-08-27T18:28:07Z","signing_key_id":"soroq-toolchain-kid-v1",
"platforms":{"ios":{"frontend_version":"fe-v1-3442","toolchain_version":"tc-v1-3442"}}}`)
}

func v2MatrixBytes() []byte {
	return []byte(`{"schema":"soroq.catalog.v2","generated_at":"2026-09-13T00:00:00Z","signing_key_id":"soroq-toolchain-kid-v1",
"platforms":{"ios":{"entries":[
 {"flutter_revision":"` + cliRev3442 + `","flutter_version":"3.44.2","frontend_version":"fe-3442","toolchain_version":"tc-3442"},
 {"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"fe-3449-r5","toolchain_version":"tc-3449-r5"}
]}}}`)
}

func activeToolchainFor(t *testing.T, home, platform string) activeToolchainEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".soroq", "toolchains", "active.json"))
	if err != nil {
		t.Fatalf("no active toolchains record: %v", err)
	}
	var doc activeToolchains
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse active record: %v", err)
	}
	e, ok := doc.Platforms[platform]
	if !ok {
		t.Fatalf("active record has no %s entry (have %v)", platform, doc.Platforms)
	}
	return e
}

// REQUIRED: real `soroq setup` resolves 3.44.2 and 3.44.9 INDEPENDENTLY through v2.
func TestRealSetupResolvesBothRevisionsIndependently(t *testing.T) {
	for _, tc := range []struct{ rev, wantFE, wantTC string }{
		{cliRev3442, "fe-3442", "tc-3442"},
		{cliRev3449, "fe-3449-r5", "tc-3449-r5"},
	} {
		t.Run(tc.rev[:12], func(t *testing.T) {
			signer := setupTestSigner(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			fcalls, tcalls := stubV2Installs(t)
			srv := bothCatalogServer(t, signer, v1CatalogBytes(), v2MatrixBytes())
			defer srv.Close()

			if err := runSetup([]string{"ios", "--api", srv.URL, "--catalog-v2", "--flutter-revision", tc.rev}); err != nil {
				t.Fatalf("runSetup: %v", err)
			}
			if len(*fcalls) != 1 || (*fcalls)[0][0] != tc.wantFE {
				t.Fatalf("frontend install args = %v; want first arg %q", *fcalls, tc.wantFE)
			}
			if len(*tcalls) != 1 || (*tcalls)[0][0] != tc.wantTC {
				t.Fatalf("toolchain install args = %v; want first arg %q", *tcalls, tc.wantTC)
			}
			// The ACTIVE pointer must record the revision-matched pair, not v1's.
			active := activeToolchainFor(t, home, "ios")
			if active.ToolchainVersion != tc.wantTC || active.FrontendVersion != tc.wantFE {
				t.Fatalf("active record = %s/%s; want %s/%s",
					active.FrontendVersion, active.ToolchainVersion, tc.wantFE, tc.wantTC)
			}
		})
	}
}

// v1 REMAINS THE DEFAULT: without --catalog-v2 the same server yields v1's pair.
func TestRealSetupDefaultsToV1(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir())
	fcalls, tcalls := stubInstalls(t)
	srv := bothCatalogServer(t, signer, v1CatalogBytes(), v2MatrixBytes())
	defer srv.Close()

	if err := runSetup([]string{"ios", "--api", srv.URL}); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	if len(*fcalls) != 1 || (*fcalls)[0][0] != "fe-v1-3442" {
		t.Fatalf("default setup used %v; v1 must remain the default", *fcalls)
	}
	if len(*tcalls) != 1 || (*tcalls)[0][0] != "tc-v1-3442" {
		t.Fatalf("default setup toolchain %v; want tc-v1-3442", *tcalls)
	}
}

// REQUIRED: a 3.44.9 request must NOT fall back to the 3.44.2 v1 pair when v2 is absent.
func TestSetupRefusesV1FallbackForMismatchedRevision(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir())
	fcalls, tcalls := stubV2Installs(t)

	// v2 is genuinely absent (404) — the one fallback door — and v1 pins a 3.44.2 pair whose manifests
	// declare the 3.44.2 revision. The requested 3.44.9 must be refused, not silently downgraded.
	srv := revisionAwareServer(t, signer, v1CatalogBytes(), nil, cliRev3442)
	defer srv.Close()

	err := runSetup([]string{"ios", "--api", srv.URL, "--catalog-v2", "--flutter-revision", cliRev3449})
	if err == nil {
		t.Fatal("3.44.9 request was satisfied from the v1 3.44.2 pair; it must refuse")
	}
	if !strings.Contains(err.Error(), "REFUSED") {
		t.Errorf("error should be an explicit refusal, got: %v", err)
	}
	if len(*fcalls) != 0 || len(*tcalls) != 0 {
		t.Errorf("installers ran despite the refusal: fe=%v tc=%v", *fcalls, *tcalls)
	}
}

// ...and the same fallback SUCCEEDS when v1's pair genuinely is the requested revision.
func TestSetupAllowsV1FallbackWhenRevisionMatches(t *testing.T) {
	signer := setupTestSigner(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	fcalls, _ := stubV2Installs(t)
	srv := revisionAwareServer(t, signer, v1CatalogBytes(), nil, cliRev3442)
	defer srv.Close()

	if err := runSetup([]string{"ios", "--api", srv.URL, "--catalog-v2", "--flutter-revision", cliRev3442}); err != nil {
		t.Fatalf("matching-revision fallback should succeed: %v", err)
	}
	if len(*fcalls) != 1 || (*fcalls)[0][0] != "fe-v1-3442" {
		t.Fatalf("fallback installed %v; want the v1 pair", *fcalls)
	}
}

// --flutter-revision without --catalog-v2 must refuse rather than be silently ignored.
func TestRevisionWithoutV2FlagRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := runSetup([]string{"ios", "--api", "http://127.0.0.1:1", "--flutter-revision", cliRev3449}); err == nil {
		t.Fatal("--flutter-revision without --catalog-v2 was accepted; it must refuse")
	}
}

// --catalog-v2 without a revision must refuse: the matrix has more than one pair per platform.
func TestV2FlagWithoutRevisionRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := runSetup([]string{"ios", "--api", "http://127.0.0.1:1", "--catalog-v2"}); err == nil {
		t.Fatal("--catalog-v2 without --flutter-revision was accepted; it must refuse")
	}
}

// revisionAwareServer serves a signed v1 catalog, optionally a signed v2, AND the signed frontend +
// toolchain manifests the v1 fallback's revision gate fetches. manifestRev is the revision those
// manifests declare, which is what lets a test prove the gate refuses a mismatch.
func revisionAwareServer(t *testing.T, signer interface {
	SignToolchainManifest([]byte) (string, error)
}, v1 []byte, v2 []byte, manifestRev string) *httptest.Server {
	t.Helper()
	sign := func(b []byte) string {
		s, err := signer.SignToolchainManifest(b)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	const fe, tc = "fe-v1-3442", "tc-v1-3442"
	feManifest := []byte(`{"schema":"soroq.frontend.v1","soroq_frontend_version":"` + fe + `",
"flutter_revision":"` + manifestRev + `","dart_revision":"` + strings.Repeat("d", 40) + `",
"flutter_version":"3.44.2","engine_revision":"` + strings.Repeat("e", 40) + `",
"patchset_sha256":"` + strings.Repeat("f", 64) + `","analyzer_snapshot_sha256":"` + strings.Repeat("9", 64) + `",
"dart_version":"3.12.2","compatible_toolchain_ids":["` + tc + `"],
"archive":{"url":"https://example.invalid/fe.tar.gz","sha256":"` + strings.Repeat("a", 64) + `","compressed_bytes":1,"uncompressed_bytes":1},
"frontend_subdir":"flutter-sdk-src"}`)
	tcManifest := []byte(`{"schema":"soroq.toolchain.v1","soroq_toolchain_version":"` + tc + `","platform":"ios",
"arch":"arm64","build_mode":"release","tier":"production","flutter_version":"3.44.2",
"flutter_revision":"` + manifestRev + `","dart_revision":"` + strings.Repeat("d", 40) + `",
"soroq_engine_revision":"soroq.ios_engine.test.v1","artifacts":[],"archive":{"url":"https://example.invalid/tc.tar.gz","sha256":"` + strings.Repeat("b", 64) + `","compressed_bytes":1}}`)

	v1Sig := sign(v1)
	var v2Sig string
	if v2 != nil {
		v2Sig = sign(v2)
	}
	feSig, tcSig := sign(feManifest), sign(tcManifest)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/catalog":
			w.Write(v1)
		case "/v1/catalog.sig":
			w.Write([]byte(v1Sig))
		case "/v2/catalog", "/v2/catalog.sig":
			if v2 == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.URL.Path == "/v2/catalog" {
				w.Write(v2)
			} else {
				w.Write([]byte(v2Sig))
			}
		case "/v1/frontends/" + fe:
			w.Write(feManifest)
		case "/v1/frontends/" + fe + "/manifest.sig":
			w.Write([]byte(feSig))
		case "/v1/toolchains/" + tc:
			w.Write(tcManifest)
		case "/v1/toolchains/" + tc + "/manifest.sig":
			w.Write([]byte(tcSig))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// REGRESSION 1: v2 setup must be genuinely phased. A two-platform setup whose SECOND platform fails
// preflight must install NOTHING and record NOTHING — not even for the first platform, which would
// otherwise already be on disk with its active pointer written by the time the second was checked.
func TestSetupV2InstallsNothingWhenASecondPlatformFailsPreflight(t *testing.T) {
	signer := setupTestSigner(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	var fcalls, tcalls [][]string
	prevF, prevT, prevPre := installFrontend, installToolchain, selectedPairPreflightFn
	installFrontend = func(args []string) error { fcalls = append(fcalls, args); return nil }
	installToolchain = func(args []string) error { tcalls = append(tcalls, args); return nil }
	// android preflights fine; ios fails. Ordering in the command is "android ios".
	selectedPairPreflightFn = func(_ string, sel catalogSelection) (catalogPlatformPreflight, error) {
		if sel.Platform == "ios" {
			return catalogPlatformPreflight{}, errors.New("REFUSED: synthetic ios preflight failure")
		}
		return catalogPlatformPreflight{
			Platform:  sel.Platform,
			Entry:     catalogPlatform{FrontendVersion: sel.FrontendVersion, ToolchainVersion: sel.ToolchainVersion},
			Toolchain: cliManifest{Platform: sel.Platform, BuildMode: "release", Tier: "production"},
		}, nil
	}
	t.Cleanup(func() { installFrontend, installToolchain, selectedPairPreflightFn = prevF, prevT, prevPre })

	twoPlatform := []byte(`{"schema":"soroq.catalog.v2","platforms":{
"android":{"entries":[{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"fe-a","toolchain_version":"tc-a"}]},
"ios":{"entries":[{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"fe-i","toolchain_version":"tc-i"}]}}}`)
	srv := bothCatalogServer(t, signer, v1CatalogBytes(), twoPlatform)
	defer srv.Close()

	err := runSetup([]string{"--platforms", "android,ios", "--api", srv.URL, "--catalog-v2", "--flutter-revision", cliRev3449})
	if err == nil {
		t.Fatal("setup succeeded despite a failing ios preflight")
	}
	if len(fcalls) != 0 {
		t.Errorf("frontend installs ran before every preflight passed: %v", fcalls)
	}
	if len(tcalls) != 0 {
		t.Errorf("toolchain installs ran before every preflight passed: %v", tcalls)
	}
	// No active record for ANY platform: the file should not exist at all.
	if _, statErr := os.Stat(filepath.Join(home, ".soroq", "toolchains", "active.json")); statErr == nil {
		t.Error("an active-toolchain record was written despite the aborted setup")
	}
}

// REGRESSION 2: the install-time pair-identity contract refuses a DART-REVISION mismatch, before any
// installation. Publish-time checking is not enough: manifests are fetched again at install time.
func TestSetupV2RefusesDartRevisionMismatchBeforeInstalling(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir())

	var fcalls, tcalls [][]string
	prevF, prevT := installFrontend, installToolchain
	installFrontend = func(args []string) error { fcalls = append(fcalls, args); return nil }
	installToolchain = func(args []string) error { tcalls = append(tcalls, args); return nil }
	t.Cleanup(func() { installFrontend, installToolchain = prevF, prevT })
	// NOTE: the real preflightSelectedPair runs here (no seam stub), which is the point.

	srv := mismatchedDartServer(t, signer)
	defer srv.Close()

	err := runSetup([]string{"ios", "--api", srv.URL, "--catalog-v2", "--flutter-revision", cliRev3449})
	if err == nil {
		t.Fatal("a dart_revision mismatch was installed")
	}
	if !strings.Contains(err.Error(), "dart_revision") {
		t.Errorf("error should name dart_revision, got: %v", err)
	}
	if len(fcalls) != 0 || len(tcalls) != 0 {
		t.Errorf("installers ran despite the refusal: fe=%v tc=%v", fcalls, tcalls)
	}
}

// mismatchedDartServer serves a v2 matrix whose iOS pair shares a Flutter revision but disagrees on
// dart_revision, plus the archives the preflight probes.
func mismatchedDartServer(t *testing.T, signer interface {
	SignToolchainManifest([]byte) (string, error)
}) *httptest.Server {
	t.Helper()
	sign := func(b []byte) string {
		s, err := signer.SignToolchainManifest(b)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	v2 := []byte(`{"schema":"soroq.catalog.v2","platforms":{"ios":{"entries":[
{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"fe-dm","toolchain_version":"tc-dm"}]}}}`)
	v2Sig := sign(v2)
	var fe, tc []byte
	var feSig, tcSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/catalog":
			w.Write(v2)
		case "/v2/catalog.sig":
			w.Write([]byte(v2Sig))
		case "/v1/frontends/fe-dm":
			w.Write(fe)
		case "/v1/frontends/fe-dm/manifest.sig":
			w.Write([]byte(feSig))
		case "/v1/toolchains/tc-dm":
			w.Write(tc)
		case "/v1/toolchains/tc-dm/manifest.sig":
			w.Write([]byte(tcSig))
		case "/archives/fe.tar.gz", "/archives/tc.tar.gz":
			w.Header().Set("Content-Length", "1")
			w.Write([]byte("x"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	fe = []byte(`{"schema":"soroq.frontend.v1","soroq_frontend_version":"fe-dm","flutter_revision":"` + cliRev3449 + `",
"dart_revision":"` + strings.Repeat("1", 40) + `","engine_revision":"` + strings.Repeat("e", 40) + `","flutter_version":"3.44.9",
"patchset_sha256":"` + strings.Repeat("f", 64) + `","analyzer_snapshot_sha256":"` + strings.Repeat("9", 64) + `",
"dart_version":"3.12.2","compatible_toolchain_ids":["tc-dm"],
"archive":{"url":"` + srv.URL + `/archives/fe.tar.gz","sha256":"` + strings.Repeat("a", 64) + `","compressed_bytes":1,"uncompressed_bytes":1},
"frontend_subdir":"flutter-sdk-src"}`)
	tc = []byte(`{"schema":"soroq.toolchain.v1","soroq_toolchain_version":"tc-dm","platform":"ios","arch":"arm64",
"build_mode":"release","tier":"production","flutter_version":"3.44.9","flutter_revision":"` + cliRev3449 + `",
"dart_revision":"` + strings.Repeat("2", 40) + `","soroq_engine_revision":"soroq.ios_engine.test.dm","artifacts":[],
"archive":{"url":"` + srv.URL + `/archives/tc.tar.gz","sha256":"` + strings.Repeat("b", 64) + `","compressed_bytes":1}}`)
	feSig, tcSig = sign(fe), sign(tc)
	return srv
}

// ITEM 2: a setup must resolve every platform from ONE verified catalog snapshot.
//
// The server below CHANGES the document it serves after the first fetch, modelling a publish landing
// mid-setup. Resolving per-platform would take android from generation A and ios from generation B —
// a combination nobody published together and nobody reviewed as a whole. One fetch makes that
// impossible, so both platforms must come from generation A.
func TestSetupV2ResolvesAllPlatformsFromOneSnapshot(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir())
	fcalls, tcalls := stubV2Installs(t)

	genA := []byte(`{"schema":"soroq.catalog.v2","platforms":{
"android":{"entries":[{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"fe-A","toolchain_version":"tc-A"}]},
"ios":{"entries":[{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"fe-A","toolchain_version":"tc-A-ios"}]}}}`)
	genB := []byte(`{"schema":"soroq.catalog.v2","platforms":{
"android":{"entries":[{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"fe-B","toolchain_version":"tc-B"}]},
"ios":{"entries":[{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"fe-B","toolchain_version":"tc-B-ios"}]}}}`)
	sigA, _ := signer.SignToolchainManifest(genA)
	sigB, _ := signer.SignToolchainManifest(genB)

	var docFetches int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v2/catalog":
			docFetches++
			if docFetches == 1 {
				w.Write(genA)
			} else {
				w.Write(genB) // a "publish" landed
			}
		case "/v2/catalog.sig":
			if docFetches <= 1 {
				w.Write([]byte(sigA))
			} else {
				w.Write([]byte(sigB))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	if err := runSetup([]string{"--platforms", "android,ios", "--api", srv.URL,
		"--catalog-v2", "--flutter-revision", cliRev3449}); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	mu.Lock()
	fetches := docFetches
	mu.Unlock()
	if fetches != 1 {
		t.Errorf("the v2 document was fetched %d times; one setup must use ONE snapshot", fetches)
	}
	// Every installed version must come from generation A. A "-B" anywhere means two generations were mixed.
	for _, calls := range [][][]string{*fcalls, *tcalls} {
		for _, args := range calls {
			if strings.Contains(args[0], "-B") {
				t.Errorf("installed %q from a LATER catalog generation; the snapshot was not held", args[0])
			}
		}
	}
	if len(*fcalls) != 2 || len(*tcalls) != 2 {
		t.Errorf("expected both platforms installed, got fe=%v tc=%v", *fcalls, *tcalls)
	}
}
