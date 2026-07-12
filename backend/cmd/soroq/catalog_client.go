package main

// catalog_client.go — fetch + VERIFY + schema-gate the hosted platform catalog for `soroq setup`.
//
// The catalog (soroq.catalog.v1) maps each supported platform to the {frontend_version, toolchain_version}
// pair the CLI should install, so a developer runs `soroq setup android` with NO long version IDs. It is
// signed with the SAME operator toolchain key + the SAME raw-bytes anchor as every frontend/toolchain
// manifest (signing.VerifyToolchainManifestSignature against the CLI-pinned toolchain pubkey — no new
// trust anchor).
//
// SECURITY-CRITICAL ordering (mirrors frontend_cmd.go: verify@306 -> parse -> schema-gate@225):
//   1. fetch the RAW catalog bytes + detached signature,
//   2. VERIFY the raw bytes against the pinned key BEFORE parsing anything,
//   3. ONLY THEN parse and enforce schema == "soroq.catalog.v1".
// The schema-gate is LOAD-BEARING: raw-bytes Ed25519 verify has NO domain separation, so a validly-signed
// TOOLCHAIN or FRONTEND manifest served at the catalog route would pass step 2. The schema check in step 3
// is what REFUSES it (its schema is soroq.toolchain.v1 / soroq.frontend.v1, not soroq.catalog.v1). There is
// NO unsigned fallback anywhere.

import (
	"encoding/json"
	"fmt"
	"strings"

	"soroq/backend/internal/signing"
)

// catalogSchema is the required schema tag. A signed document whose schema differs is REFUSED even though
// its signature verifies — this is the domain separation raw-bytes verify does not provide.
const catalogSchema = "soroq.catalog.v1"

// catalogDoc is the CLI view of the signed soroq.catalog.v1 document. platforms maps a platform id
// ("android" | "ios") to the versions setup should install for it.
type catalogDoc struct {
	Schema    string                     `json:"schema"`
	Platforms map[string]catalogPlatform `json:"platforms"`
}

// catalogPlatform is a single platform entry: the frontend + toolchain versions to resolve + install.
type catalogPlatform struct {
	FrontendVersion  string `json:"frontend_version"`
	ToolchainVersion string `json:"toolchain_version"`
}

// fetchVerifiedCatalog fetches, VERIFIES, and schema-gates the catalog from base (the api). It returns the
// parsed doc ONLY after the signature verifies against the pinned key AND the schema is exactly
// soroq.catalog.v1. Every failure path REFUSES (a clear error, no partial trust, no unsigned fallback).
func fetchVerifiedCatalog(base string) (catalogDoc, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}

	// 1. Fetch the RAW catalog bytes (verbatim) + the detached signature. No credentials (public read).
	catalogBytes, err := httpGetBytes(base + "/v1/catalog")
	if err != nil {
		return catalogDoc{}, fmt.Errorf("fetch catalog: %w", err)
	}
	sigBytes, err := httpGetBytes(base + "/v1/catalog.sig")
	if err != nil {
		return catalogDoc{}, fmt.Errorf("fetch catalog signature: %w", err)
	}
	sigHex := strings.TrimSpace(string(sigBytes))

	// 2. VERIFY the RAW bytes against the pinned toolchain pubkey BEFORE parsing (REFUSAL: bad signature).
	//    Same anchor + pinned key as the frontend/toolchain installs (frontend_cmd.go:306).
	if err := signing.VerifyToolchainManifestSignature(catalogBytes, sigHex, pinnedToolchainPublicKeyHex()); err != nil {
		return catalogDoc{}, fmt.Errorf("REFUSED: catalog signature: %w", err)
	}

	// 3. ONLY AFTER verify passes: parse + schema-gate. This is what refuses a validly-signed but
	//    WRONG-SCHEMA document (e.g. a toolchain/frontend manifest served here) — the raw-bytes verify
	//    above has no domain separation, so the schema check is the domain boundary.
	return parseVerifiedCatalog(catalogBytes)
}

// parseVerifiedCatalog parses catalog bytes whose signature has ALREADY been verified, and enforces the
// schema tag. It refuses a missing/wrong schema or an empty platforms map. Split out so the schema-gate
// is unit-testable independent of the network.
func parseVerifiedCatalog(catalogBytes []byte) (catalogDoc, error) {
	var doc catalogDoc
	if err := json.Unmarshal(catalogBytes, &doc); err != nil {
		return catalogDoc{}, fmt.Errorf("REFUSED: parse catalog: %w", err)
	}
	if doc.Schema != catalogSchema {
		return catalogDoc{}, fmt.Errorf("REFUSED: catalog schema %q != %q (wrong document served at the catalog route)", doc.Schema, catalogSchema)
	}
	if len(doc.Platforms) == 0 {
		return catalogDoc{}, fmt.Errorf("REFUSED: catalog has no platform entries")
	}
	return doc, nil
}

// entryForPlatform returns the catalog entry for platform, REFUSING an absent platform or an entry missing
// either version. platform is normalized to lower-case.
func (d catalogDoc) entryForPlatform(platform string) (catalogPlatform, error) {
	p := strings.ToLower(strings.TrimSpace(platform))
	entry, ok := d.Platforms[p]
	if !ok {
		return catalogPlatform{}, fmt.Errorf("REFUSED: catalog has no entry for platform %q (available: %s)", p, strings.Join(d.platformNames(), ", "))
	}
	if strings.TrimSpace(entry.FrontendVersion) == "" {
		return catalogPlatform{}, fmt.Errorf("REFUSED: catalog entry for %q is missing frontend_version", p)
	}
	if strings.TrimSpace(entry.ToolchainVersion) == "" {
		return catalogPlatform{}, fmt.Errorf("REFUSED: catalog entry for %q is missing toolchain_version", p)
	}
	return entry, nil
}

func (d catalogDoc) platformNames() []string {
	names := make([]string, 0, len(d.Platforms))
	for k := range d.Platforms {
		names = append(names, k)
	}
	return names
}
