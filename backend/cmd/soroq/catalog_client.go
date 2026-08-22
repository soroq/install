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
	"io"
	"net/http"
	"net/url"
	"strconv"
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

// catalogPlatformPreflight is the verified, signed artifact pair behind one catalog entry. Keeping the
// parsed manifests lets setup report the selected build mode/tier without trusting unsigned catalog text.
type catalogPlatformPreflight struct {
	Platform  string
	Entry     catalogPlatform
	Frontend  frontendManifest
	Toolchain cliManifest
}

// catalogReferencePreflightFn is the single setup/operator-publish preflight seam. Production always uses
// the real verifier; tests may replace it to isolate catalog routing/signing from artifact-fixture setup.
var catalogReferencePreflightFn = preflightCatalogReferences

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

// preflightCatalogReferences verifies every requested frontend/toolchain pair and lightly probes both
// signed archive URLs BEFORE setup performs any install. This closes a distribution-integrity gap where a
// validly-signed catalog could name an absent artifact: cached machines continued to work while every fresh
// setup failed only after the first large install had already run.
func preflightCatalogReferences(base string, doc catalogDoc, platforms []string) (map[string]catalogPlatformPreflight, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}
	verified := make(map[string]catalogPlatformPreflight, len(platforms))
	for _, rawPlatform := range platforms {
		platform := strings.ToLower(strings.TrimSpace(rawPlatform))
		entry, err := doc.entryForPlatform(platform)
		if err != nil {
			return nil, err
		}

		frontend, err := fetchCatalogFrontendManifest(base, entry.FrontendVersion)
		if err != nil {
			return nil, fmt.Errorf("REFUSED: catalog %s frontend %q: %w", platform, entry.FrontendVersion, err)
		}
		toolchain, err := fetchCatalogToolchainManifest(base, entry.ToolchainVersion)
		if err != nil {
			return nil, fmt.Errorf("REFUSED: catalog %s toolchain %q: %w", platform, entry.ToolchainVersion, err)
		}
		if got := strings.ToLower(strings.TrimSpace(toolchain.Platform)); got != platform {
			return nil, fmt.Errorf("REFUSED: catalog %s toolchain %q declares platform %q", platform, entry.ToolchainVersion, toolchain.Platform)
		}
		if !containsExact(frontend.CompatibleToolchainIDs, entry.ToolchainVersion) {
			return nil, fmt.Errorf("REFUSED: catalog %s pair is not bound: frontend %q does not declare toolchain %q compatible", platform, entry.FrontendVersion, entry.ToolchainVersion)
		}
		if err := probeSignedArchive(frontend.Archive.URL, frontend.Archive.CompressedBytes); err != nil {
			return nil, fmt.Errorf("REFUSED: catalog %s frontend archive: %w", platform, err)
		}
		if err := probeSignedArchive(toolchain.Archive.URL, toolchain.Archive.CompressedBytes); err != nil {
			return nil, fmt.Errorf("REFUSED: catalog %s toolchain archive: %w", platform, err)
		}
		verified[platform] = catalogPlatformPreflight{
			Platform: platform, Entry: entry, Frontend: frontend, Toolchain: toolchain,
		}
	}
	return verified, nil
}

func fetchCatalogFrontendManifest(base, version string) (frontendManifest, error) {
	manifestBytes, err := httpGetBytes(base + "/v1/frontends/" + url.PathEscape(version))
	if err != nil {
		return frontendManifest{}, fmt.Errorf("manifest fetch: %w", err)
	}
	sigBytes, err := httpGetBytes(base + "/v1/frontends/" + url.PathEscape(version) + "/manifest.sig")
	if err != nil {
		return frontendManifest{}, fmt.Errorf("signature fetch: %w", err)
	}
	if err := signing.VerifyToolchainManifestSignature(manifestBytes, strings.TrimSpace(string(sigBytes)), pinnedToolchainPublicKeyHex()); err != nil {
		return frontendManifest{}, fmt.Errorf("signature verification: %w", err)
	}
	manifest, err := parseFrontendManifest(manifestBytes)
	if err != nil {
		return frontendManifest{}, err
	}
	if manifest.SoroqFrontendVersion != version {
		return frontendManifest{}, fmt.Errorf("manifest version %q does not match catalog version %q", manifest.SoroqFrontendVersion, version)
	}
	if err := checkFrontendIdentity(manifest); err != nil {
		return frontendManifest{}, err
	}
	if strings.TrimSpace(manifest.Archive.URL) == "" || len(strings.TrimSpace(manifest.Archive.SHA256)) != 64 || manifest.Archive.CompressedBytes <= 0 {
		return frontendManifest{}, fmt.Errorf("signed manifest has incomplete archive identity")
	}
	return manifest, nil
}

func fetchCatalogToolchainManifest(base, version string) (cliManifest, error) {
	manifestBytes, err := httpGetBytes(base + "/v1/toolchains/" + url.PathEscape(version))
	if err != nil {
		return cliManifest{}, fmt.Errorf("manifest fetch: %w", err)
	}
	sigBytes, err := httpGetBytes(base + "/v1/toolchains/" + url.PathEscape(version) + "/manifest.sig")
	if err != nil {
		return cliManifest{}, fmt.Errorf("signature fetch: %w", err)
	}
	if err := signing.VerifyToolchainManifestSignature(manifestBytes, strings.TrimSpace(string(sigBytes)), pinnedToolchainPublicKeyHex()); err != nil {
		return cliManifest{}, fmt.Errorf("signature verification: %w", err)
	}
	manifest, err := parseCLIManifest(manifestBytes)
	if err != nil {
		return cliManifest{}, err
	}
	if manifest.SoroqToolchainVersion != version {
		return cliManifest{}, fmt.Errorf("manifest version %q does not match catalog version %q", manifest.SoroqToolchainVersion, version)
	}
	if err := checkToolchainIdentity(manifest); err != nil {
		return cliManifest{}, err
	}
	if strings.TrimSpace(manifest.Archive.URL) == "" || len(strings.TrimSpace(manifest.Archive.SHA256)) != 64 || manifest.Archive.CompressedBytes <= 0 {
		return cliManifest{}, fmt.Errorf("signed manifest has incomplete archive identity")
	}
	return manifest, nil
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

// probeSignedArchive performs a one-byte range GET rather than downloading a multi-gigabyte archive.
// Some registry routes do not implement HEAD, so HEAD cannot distinguish an absent artifact from an
// unsupported method. A server that ignores Range is still bounded client-side: the body is closed after
// one byte. Full size and SHA verification remain the installer's responsibility.
func probeSignedArchive(rawURL string, expectedBytes int64) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("invalid signed archive URL %q", rawURL)
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_, _ = io.CopyN(io.Discard, resp.Body, 512)
		return fmt.Errorf("GET %s returned %s", u.Redacted(), resp.Status)
	}
	if expectedBytes > 0 {
		servedBytes, err := archiveResponseSize(resp)
		if err != nil {
			return fmt.Errorf("GET %s: %w", u.Redacted(), err)
		}
		if servedBytes != expectedBytes {
			return fmt.Errorf("GET %s archive size %d does not match signed size %d", u.Redacted(), servedBytes, expectedBytes)
		}
		var one [1]byte
		if n, readErr := resp.Body.Read(one[:]); n != 1 || (readErr != nil && readErr != io.EOF) {
			return fmt.Errorf("GET %s returned no archive bytes", u.Redacted())
		}
	}
	return nil
}

func archiveResponseSize(resp *http.Response) (int64, error) {
	if resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 {
		return resp.ContentLength, nil
	}
	if resp.StatusCode == http.StatusPartialContent {
		contentRange := strings.TrimSpace(resp.Header.Get("Content-Range"))
		slash := strings.LastIndexByte(contentRange, '/')
		if slash >= 0 && slash+1 < len(contentRange) {
			total, err := strconv.ParseInt(contentRange[slash+1:], 10, 64)
			if err == nil && total >= 0 {
				return total, nil
			}
		}
	}
	return 0, fmt.Errorf("archive response does not expose a usable total size")
}
