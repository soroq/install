package main

// catalog_client_test.go — client-side (soroq setup) required P2 coverage, exercising catalog_client.go's
// OWN functions (fetchVerifiedCatalog / parseVerifiedCatalog / entryForPlatform) directly. These are named
// TestCatalog* so the `go test -run Catalog` gate discovers them; the runSetup-level TestSetup* tests in
// setup_cmd_test.go exercise a DIFFERENT entrypoint and are outside that filter.
//
// Every case routes a signed body through a local catalogTestServer against the CLI's verify -> parse ->
// schema-gate -> resolve pipeline, using an EPHEMERAL in-process pinned key (setupTestSigner, never the prod
// seed). The load-bearing distinctions are asserted explicitly:
//   - bad signature is refused at VERIFY (error names "signature"), BEFORE any parse,
//   - malformed bytes that VERIFY are refused at PARSE (error names "parse", not "signature"),
//   - a validly-signed WRONG-SCHEMA doc is refused by the SCHEMA-GATE (error names "schema", not "signature")
//     — this is the domain separation raw-bytes Ed25519 verify does not provide.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCatalogClientResolvesValidEntry (case 1) — a properly-signed soroq.catalog.v1 catalog verifies, parses,
// and resolves a per-platform {frontend_version, toolchain_version} pair via the CLI's own client pipeline.
func TestCatalogClientResolvesValidEntry(t *testing.T) {
	signer := setupTestSigner(t)

	doc := catalogDoc{
		Schema: catalogSchema,
		Platforms: map[string]catalogPlatform{
			"android": {FrontendVersion: "frontend-A", ToolchainVersion: "toolchain-A"},
			"ios":     {FrontendVersion: "frontend-I", ToolchainVersion: "toolchain-I"},
		},
	}
	body, _ := json.MarshalIndent(doc, "", "  ")
	srv := catalogTestServer(t, body, signBytes(t, signer, body))

	got, err := fetchVerifiedCatalog(srv.URL)
	if err != nil {
		t.Fatalf("valid signed catalog must verify + parse: %v", err)
	}
	entry, err := got.entryForPlatform("android")
	if err != nil {
		t.Fatalf("valid catalog must resolve the android entry: %v", err)
	}
	if entry.FrontendVersion != "frontend-A" || entry.ToolchainVersion != "toolchain-A" {
		t.Fatalf("resolved wrong versions: %+v", entry)
	}
}

// TestCatalogClientRefusesMalformedSignedBytes (case 2) — bytes that VERIFY (correctly signed) but are NOT
// valid soroq.catalog.v1 JSON are REFUSED at PARSE. The error must name "parse" (not "signature"), proving
// the signature check passed and it was the parse step that refused.
func TestCatalogClientRefusesMalformedSignedBytes(t *testing.T) {
	signer := setupTestSigner(t)

	// Garbage bytes, but signed with the pinned key so the verify step passes cleanly.
	malformed := []byte("{ this is not valid soroq.catalog.v1 json @@@")
	srv := catalogTestServer(t, malformed, signBytes(t, signer, malformed))

	_, err := fetchVerifiedCatalog(srv.URL)
	if err == nil {
		t.Fatal("expected REFUSED on malformed (but signed) catalog bytes, got nil")
	}
	if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected a parse REFUSED, got: %v", err)
	}
	// The bytes were validly signed: this must be a parse refusal, NOT a signature refusal.
	if strings.Contains(strings.ToLower(err.Error()), "signature") {
		t.Fatalf("malformed-but-signed bytes must be refused at PARSE, not as a signature error: %v", err)
	}
}

// TestCatalogClientRefusesBadSignature (case 3) — valid soroq.catalog.v1 JSON with a tampered (1-byte-flipped)
// signature is REFUSED at VERIFY, BEFORE any parse. The error must name "signature".
func TestCatalogClientRefusesBadSignature(t *testing.T) {
	signer := setupTestSigner(t)

	doc := catalogDoc{
		Schema:    catalogSchema,
		Platforms: map[string]catalogPlatform{"android": {FrontendVersion: "frontend-A", ToolchainVersion: "toolchain-A"}},
	}
	body, _ := json.MarshalIndent(doc, "", "  ")
	sig := []byte(signBytes(t, signer, body))
	// Flip one hex nibble of the detached signature: still well-formed hex, but no longer a valid signature.
	if last := len(sig) - 1; sig[last] == '0' {
		sig[last] = '1'
	} else {
		sig[last] = '0'
	}
	srv := catalogTestServer(t, body, string(sig))

	got, err := fetchVerifiedCatalog(srv.URL)
	if err == nil {
		t.Fatal("expected REFUSED on a tampered signature, got nil")
	}
	if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(strings.ToLower(err.Error()), "signature") {
		t.Fatalf("expected a signature REFUSED (verify before parse), got: %v", err)
	}
	if len(got.Platforms) != 0 {
		t.Fatal("a signature-refused catalog must not yield a parsed document")
	}
}

// TestCatalogClientRefusesUnknownPlatform (case 4) — a validly-signed, right-schema catalog that has no entry
// for the requested platform REFUSES with a clear "no entry for platform" message.
func TestCatalogClientRefusesUnknownPlatform(t *testing.T) {
	signer := setupTestSigner(t)

	doc := catalogDoc{
		Schema:    catalogSchema,
		Platforms: map[string]catalogPlatform{"android": {FrontendVersion: "frontend-A", ToolchainVersion: "toolchain-A"}},
	}
	body, _ := json.MarshalIndent(doc, "", "  ")
	srv := catalogTestServer(t, body, signBytes(t, signer, body))

	got, err := fetchVerifiedCatalog(srv.URL)
	if err != nil {
		t.Fatalf("catalog must verify + parse: %v", err)
	}
	// The catalog is valid; the requested platform is simply absent.
	if _, err := got.entryForPlatform("ios"); err == nil {
		t.Fatal("expected REFUSED for an absent platform, got nil")
	} else if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "no entry for platform") {
		t.Fatalf("expected an absent-platform REFUSED, got: %v", err)
	}
}

// TestCatalogClientMissingArtifact404 (case 5) — the catalog resolves the pinned frontend version, but the
// (test) registry serves NO artifact for it, so the resolve -> install path fails clearly with a 404. This
// proves the failure surfaces at the artifact download, AFTER a clean catalog resolve.
func TestCatalogClientMissingArtifact404(t *testing.T) {
	signer := setupTestSigner(t)
	t.Setenv("HOME", t.TempDir()) // installer writes under ~/.soroq; default-build-safe (not tempHome).

	doc := catalogDoc{
		Schema:    catalogSchema,
		Platforms: map[string]catalogPlatform{"android": {FrontendVersion: "frontend-A", ToolchainVersion: "toolchain-A"}},
	}
	body, _ := json.MarshalIndent(doc, "", "  ")
	// catalogTestServer serves /catalog + /catalog.sig and 404s everything else (incl. /v1/frontends/...).
	srv := catalogTestServer(t, body, signBytes(t, signer, body))

	got, err := fetchVerifiedCatalog(srv.URL)
	if err != nil {
		t.Fatalf("catalog must resolve: %v", err)
	}
	entry, err := got.entryForPlatform("android")
	if err != nil || entry.FrontendVersion != "frontend-A" {
		t.Fatalf("catalog must resolve the pinned frontend version, got entry=%+v err=%v", entry, err)
	}

	// The REAL frontend installer (called as a library) now 404s because the registry serves no such artifact.
	err = runFrontendInstall([]string{entry.FrontendVersion, "--api", srv.URL})
	if err == nil {
		t.Fatal("expected the install to fail when the pinned artifact is not served, got nil")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "frontend manifest") {
		t.Fatalf("expected a clear 404 on the frontend artifact fetch, got: %v", err)
	}
}

// TestCatalogClientRefusesWrongSchemaSignedDoc (case 6) — the load-bearing DOMAIN-SEPARATION case: a validly
// signed document whose schema is NOT soroq.catalog.v1 (a soroq.toolchain.v1 doc served at the catalog route)
// passes the raw-bytes signature verify (same pinned key) and is REFUSED by the SCHEMA-GATE. The error must
// name "schema" (NOT "signature") — proving the schema check, not the signature, is the domain boundary.
func TestCatalogClientRefusesWrongSchemaSignedDoc(t *testing.T) {
	signer := setupTestSigner(t)

	// A validly-signed doc with a DIFFERENT schema tag (as if a toolchain manifest were served here).
	wrongSchema := catalogDoc{
		Schema:    "soroq.toolchain.v1",
		Platforms: map[string]catalogPlatform{"android": {FrontendVersion: "frontend-A", ToolchainVersion: "toolchain-A"}},
	}
	body, _ := json.MarshalIndent(wrongSchema, "", "  ")
	srv := catalogTestServer(t, body, signBytes(t, signer, body)) // signature is GENUINE

	_, err := fetchVerifiedCatalog(srv.URL)
	if err == nil {
		t.Fatal("expected REFUSED on a validly-signed wrong-schema doc, got nil")
	}
	if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("expected a schema REFUSED, got: %v", err)
	}
	// The signature verified — the refusal must be the schema-gate, NOT a signature error.
	if strings.Contains(strings.ToLower(err.Error()), "signature") {
		t.Fatalf("a validly-signed wrong-schema doc must be refused by the SCHEMA-GATE, not as a signature error: %v", err)
	}
}
