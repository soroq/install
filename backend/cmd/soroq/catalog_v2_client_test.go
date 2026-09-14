package main

// catalog_v2_client_test.go — the OPT-IN and FALLBACK contract.
//
// The fallback rule is the security-relevant behaviour in the v2 client, so it is tested as a rule
// rather than as a happy path: v1 is reachable from v2 through EXACTLY ONE door (a genuine 404 on the
// document route), and every other v2 outcome must refuse. Fallback-on-any-error would let anyone who
// can corrupt or block the v2 response silently downgrade a client to v1's single global pair.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	cliRev3442 = "f74781f6213447540225edae307acb48bbaaaf34"
	cliRev3449 = "6b182d2c7585eba26d4edce0f97630effd256c33"
)

func v2Body() []byte {
	return []byte(`{
  "schema": "soroq.catalog.v2",
  "generated_at": "2026-09-13T00:00:00Z",
  "signing_key_id": "soroq-toolchain-kid-v1",
  "platforms": {
    "ios": {
      "entries": [
        {"flutter_revision": "` + cliRev3442 + `", "flutter_version": "3.44.2",
         "frontend_version": "fe-3.44.2", "toolchain_version": "tc-3.44.2"},
        {"flutter_revision": "` + cliRev3449 + `", "flutter_version": "3.44.9",
         "frontend_version": "fe-3.44.9-r5", "toolchain_version": "tc-3.44.9-r5"}
      ]
    }
  }
}`)
}

func v1Body() []byte {
	return []byte(`{
  "schema": "soroq.catalog.v1",
  "generated_at": "2026-08-27T18:28:07Z",
  "signing_key_id": "soroq-toolchain-kid-v1",
  "platforms": {"ios": {"frontend_version": "fe-v1", "toolchain_version": "tc-v1"}}
}`)
}

// catalogBothVersionsServer serves v1 always, and v2 according to v2Status/v2Payload. A v2Status of 404
// models "never published"; anything else models a failure that must NOT produce a fallback.
func catalogBothVersionsServer(t *testing.T, v1Bytes, v1Sig string, v2Status int, v2Bytes, v2Sig string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/catalog":
			w.Write([]byte(v1Bytes))
		case "/v1/catalog.sig":
			w.Write([]byte(v1Sig))
		case "/v2/catalog":
			if v2Status != http.StatusOK {
				w.WriteHeader(v2Status)
				w.Write([]byte(`{"error":"synthetic"}`))
				return
			}
			w.Write([]byte(v2Bytes))
		case "/v2/catalog.sig":
			if v2Status != http.StatusOK {
				w.WriteHeader(v2Status)
				return
			}
			w.Write([]byte(v2Sig))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// Opted in, v2 published: both revisions resolve independently through the catalog.
func TestCatalogV2ResolvesEachRevisionIndependently(t *testing.T) {
	signer := setupTestSigner(t)
	v2 := v2Body()
	v2Sig, err := signer.SignToolchainManifest(v2)
	if err != nil {
		t.Fatal(err)
	}
	v1 := v1Body()
	v1Sig, _ := signer.SignToolchainManifest(v1)
	srv := catalogBothVersionsServer(t, string(v1), v1Sig, http.StatusOK, string(v2), v2Sig)
	defer srv.Close()

	for _, tc := range []struct{ rev, wantTC string }{
		{cliRev3442, "tc-3.44.2"},
		{cliRev3449, "tc-3.44.9-r5"},
	} {
		got, err := resolveCatalogPair(srv.URL, "ios", tc.rev, true)
		if err != nil {
			t.Fatalf("resolve %s: %v", tc.rev[:12], err)
		}
		if got.ToolchainVersion != tc.wantTC {
			t.Errorf("resolve %s = %q; want %q", tc.rev[:12], got.ToolchainVersion, tc.wantTC)
		}
		if got.Source != "v2" {
			t.Errorf("resolve %s source = %q; want v2", tc.rev[:12], got.Source)
		}
	}
}

// NOT opted in: v1 is used and v2 is never consulted. Existing clients are not switched by this change.
func TestWithoutOptInV1IsUsed(t *testing.T) {
	signer := setupTestSigner(t)
	v1 := v1Body()
	v1Sig, _ := signer.SignToolchainManifest(v1)
	v2 := v2Body()
	v2Sig, _ := signer.SignToolchainManifest(v2)
	srv := catalogBothVersionsServer(t, string(v1), v1Sig, http.StatusOK, string(v2), v2Sig)
	defer srv.Close()

	got, err := resolveCatalogPair(srv.URL, "ios", cliRev3449, false)
	if err != nil {
		t.Fatalf("v1 path: %v", err)
	}
	if got.ToolchainVersion != "tc-v1" {
		t.Errorf("without opt-in got %q; a non-opted-in client must keep resolving v1", got.ToolchainVersion)
	}
}

// THE ONLY PERMITTED FALLBACK: a genuine 404 on the v2 document — AND the v1 pair must prove it is
// for the requested revision.
//
// This test used to assert that a 404 alone was enough. That was the unsafe behaviour review caught:
// v1 has no revision dimension, so it will answer a 3.44.9 request with its 3.44.2 pair and the client
// would install an engine built for a different revision. The fallback now fetches v1's signed
// manifests and requires their flutter_revision to match.
func TestFallsBackToV1OnlyOnGenuine404AndMatchingRevision(t *testing.T) {
	signer := setupTestSigner(t)

	// v1's pair is built from 3.44.2, and 3.44.2 is what is requested: the fallback is allowed.
	srv := revisionAwareServer(t, signer, v1CatalogBytes(), nil, cliRev3442)
	got, err := resolveCatalogPair(srv.URL, "ios", cliRev3442, true)
	srv.Close()
	if err != nil {
		t.Fatalf("matching-revision fallback should succeed: %v", err)
	}
	if got.ToolchainVersion != "tc-v1-3442" || got.Source != "v1-fallback" {
		t.Errorf("got %q via %q; want tc-v1-3442 via v1-fallback", got.ToolchainVersion, got.Source)
	}

	// Same 404, but 3.44.9 is requested against a 3.44.2 v1 pair: REFUSED.
	srv2 := revisionAwareServer(t, signer, v1CatalogBytes(), nil, cliRev3442)
	defer srv2.Close()
	bad, err := resolveCatalogPair(srv2.URL, "ios", cliRev3449, true)
	if err == nil {
		t.Fatalf("3.44.9 was satisfied from the v1 3.44.2 pair (%q); it must refuse", bad.ToolchainVersion)
	}
	if !strings.Contains(err.Error(), "REFUSED") {
		t.Errorf("want an explicit refusal, got: %v", err)
	}
}

// Direct v1 use (not opted in) is labelled "v1", NOT "v1-fallback": nothing fell back, and mislabelling
// it would make audit output claim a downgrade that never happened.
func TestDirectV1UseIsNotLabelledFallback(t *testing.T) {
	signer := setupTestSigner(t)
	v1 := v1Body()
	v1Sig, _ := signer.SignToolchainManifest(v1)
	srv := catalogBothVersionsServer(t, string(v1), v1Sig, http.StatusOK, "", "")
	defer srv.Close()
	got, err := resolveCatalogPair(srv.URL, "ios", "", false)
	if err != nil {
		t.Fatalf("direct v1: %v", err)
	}
	if got.Source != "v1" {
		t.Errorf("direct v1 use reported source %q; want %q", got.Source, "v1")
	}
}

// PLANTED, the important half: every NON-404 v2 failure must REFUSE, never fall back.
func TestNeverFallsBackAfterAnInvalidV2Response(t *testing.T) {
	signer := setupTestSigner(t)
	v1 := v1Body()
	v1Sig, _ := signer.SignToolchainManifest(v1)
	goodV2 := v2Body()
	goodSig, _ := signer.SignToolchainManifest(goodV2)

	cases := []struct {
		name   string
		status int
		body   string
		sig    string
	}{
		{"server error", http.StatusInternalServerError, "", ""},
		{"bad gateway", http.StatusBadGateway, "", ""},
		{"forbidden", http.StatusForbidden, "", ""},
		// Served 200 but the signature does not match the bytes.
		{"bad signature", http.StatusOK, string(goodV2), strings.Repeat("00", 64)},
		// Validly signed, but it is a v1 document at the v2 route (no domain separation in raw verify).
		{"wrong schema", http.StatusOK, "", ""},
		// Validly signed but structurally ambiguous.
		{"duplicate revision", http.StatusOK, "", ""},
	}
	for _, tc := range cases {
		body, sig := tc.body, tc.sig
		switch tc.name {
		case "wrong schema":
			body = string(v1Body())
			sig, _ = signer.SignToolchainManifest([]byte(body))
		case "duplicate revision":
			dup := `{"schema":"soroq.catalog.v2","platforms":{"ios":{"entries":[` +
				`{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"a","toolchain_version":"b"},` +
				`{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"c","toolchain_version":"d"}]}}}`
			body = dup
			sig, _ = signer.SignToolchainManifest([]byte(dup))
		}
		srv := catalogBothVersionsServer(t, string(v1), v1Sig, tc.status, body, sig)
		got, err := resolveCatalogPair(srv.URL, "ios", cliRev3449, true)
		srv.Close()
		if err == nil {
			t.Errorf("%s: resolved to %q via %q; an invalid v2 response must REFUSE, not fall back",
				tc.name, got.ToolchainVersion, got.Source)
			continue
		}
		if got.Source == "v1-fallback" {
			t.Errorf("%s: fell back to v1 after an invalid v2 response", tc.name)
		}
	}
	_ = goodSig
}

// PLANTED: an unknown revision must refuse even though v1 could answer — v1's single global pair is
// precisely the wrong engine for a revision v2 does not pin.
func TestUnknownRevisionRefusesAndDoesNotFallBack(t *testing.T) {
	signer := setupTestSigner(t)
	v1 := v1Body()
	v1Sig, _ := signer.SignToolchainManifest(v1)
	v2 := v2Body()
	v2Sig, _ := signer.SignToolchainManifest(v2)
	srv := catalogBothVersionsServer(t, string(v1), v1Sig, http.StatusOK, string(v2), v2Sig)
	defer srv.Close()

	got, err := resolveCatalogPair(srv.URL, "ios", "0123456789abcdef0123456789abcdef01234567", true)
	if err == nil {
		t.Fatalf("unknown revision resolved to %q via %q; it must refuse", got.ToolchainVersion, got.Source)
	}
	if got.Source == "v1-fallback" {
		t.Error("unknown revision fell back to v1")
	}
}

// Opting in without naming a revision refuses: v2 exists because a platform has more than one pair, and
// only the caller knows which one it is building.
func TestOptInWithoutRevisionRefuses(t *testing.T) {
	if _, err := resolveCatalogPair("http://127.0.0.1:1", "ios", "", true); err == nil {
		t.Fatal("opted in with no revision resolved; it must refuse")
	}
}

// PLANTED: an unknown field must be refused rather than ignored, since it may carry meaning this build
// does not implement.
func TestUnknownFieldInV2IsRefused(t *testing.T) {
	body := `{"schema":"soroq.catalog.v2","cohort":"canary","platforms":{"ios":{"entries":[` +
		`{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"a","toolchain_version":"b"}]}}}`
	if _, err := parseVerifiedCatalogV2([]byte(body)); err == nil {
		t.Fatal("a document with an unknown top-level field was accepted")
	}
}

var _ = json.Marshal

// trailingCases are the second-value shapes a signed byte string must never be allowed to carry.
//
// dec.More() accepts several of these: it asks "is there another VALUE in the current array/object",
// so a bare `}` or `]` slips through. The signature covers the whole byte string, so anything after the
// document is signed-but-unexamined content.
var trailingCases = []struct{ name, tail string }{
	{"another object", `{"schema":"soroq.catalog.v2"}`},
	{"stray close brace", `}`},
	{"stray close bracket", `]`},
	{"null", `null`},
	{"number", `42`},
	{"string", `"x"`},
	{"array", `[1,2]`},
	{"true", `true`},
}

// REGRESSION 3 (client): a valid document followed by ANY second JSON value is refused.
func TestClientRefusesTrailingDataAfterDocument(t *testing.T) {
	good := `{"schema":"soroq.catalog.v2","platforms":{"ios":{"entries":[` +
		`{"flutter_revision":"` + cliRev3449 + `","flutter_version":"3.44.9","frontend_version":"f","toolchain_version":"t"}]}}}`
	// the clean document must parse
	if _, err := parseVerifiedCatalogV2([]byte(good)); err != nil {
		t.Fatalf("the clean document must parse: %v", err)
	}
	for _, tc := range trailingCases {
		if _, err := parseVerifiedCatalogV2([]byte(good + tc.tail)); err == nil {
			t.Errorf("client accepted trailing %s", tc.name)
		}
	}
	// whitespace-only tails are fine: they are not a second value
	for _, ws := range []string{"\n", "  ", "\t\n "} {
		if _, err := parseVerifiedCatalogV2([]byte(good + ws)); err != nil {
			t.Errorf("client refused a whitespace-only tail %q: %v", ws, err)
		}
	}
}
