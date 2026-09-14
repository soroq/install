package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"soroq/backend/internal/catalogv2"
	"soroq/backend/internal/signing"
)

// v2 catalog consumption (OPT-IN). See internal/catalogv2 for the wire format and why selection is by
// exact Flutter revision rather than by version string.
//
// THE FALLBACK RULE IS THE SECURITY-RELEVANT PART OF THIS FILE.
//
// v1 remains the default. A client uses v2 only when it explicitly opts in, and even then it may fall
// back to v1 in exactly ONE circumstance: v2 is genuinely not published, proven by a 404 on the
// document route. Every other v2 outcome — a bad signature, a wrong schema, a malformed body, a 500, a
// connection failure — is a REFUSAL.
//
// The reason is that fallback-on-any-error is a downgrade attack in disguise: anyone able to corrupt or
// block the v2 response could silently move a client back to v1's single global pair, which is the very
// thing v2 exists to stop being the only option. "Absent" is a publisher's state; "invalid" is an
// attacker's or an outage's, and the two must not share a code path.

// errCatalogV2NotPublished reports the ONLY condition under which a v1 fallback is permitted.
var errCatalogV2NotPublished = errors.New("no v2 catalog is published")

// httpGetBytesStatus is httpGetBytes with the status code preserved. The shared helper folds every
// non-2xx into one opaque error, which cannot express "404 specifically" — and the fallback rule needs
// exactly that distinction, so v2 reads through this instead.
func httpGetBytesStatus(rawURL string) ([]byte, int, error) {
	resp, err := http.Get(rawURL)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, resp.StatusCode, fmt.Errorf("GET %s: %s", rawURL, msg)
	}
	return body, resp.StatusCode, nil
}

// fetchVerifiedCatalogV2 fetches, VERIFIES and schema-gates the v2 document.
//
// It returns errCatalogV2NotPublished ONLY when the document route answers 404. Anything else that goes
// wrong returns a refusal, so a caller that falls back on errCatalogV2NotPublished alone cannot be
// downgraded by a failure that is not a clean absence.
func fetchVerifiedCatalogV2(base string) (catalogv2.Doc, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}

	catalogBytes, status, err := httpGetBytesStatus(base + "/v2/catalog")
	if status == http.StatusNotFound {
		return catalogv2.Doc{}, errCatalogV2NotPublished
	}
	if err != nil {
		return catalogv2.Doc{}, fmt.Errorf("REFUSED: fetch v2 catalog: %w", err)
	}

	// A 404 on the SIGNATURE while the document exists is NOT an absence — it is a half-published or
	// tampered state, and falling back from it would accept exactly the situation a signature exists to
	// catch. It refuses.
	sigBytes, _, err := httpGetBytesStatus(base + "/v2/catalog.sig")
	if err != nil {
		return catalogv2.Doc{}, fmt.Errorf("REFUSED: fetch v2 catalog signature: %w", err)
	}
	sigHex := strings.TrimSpace(string(sigBytes))

	// Verify the RAW bytes before parsing, against the SAME pinned toolchain key v1 uses. v2 introduces
	// no new trust anchor: one key, one place to rotate.
	if err := signing.VerifyToolchainManifestSignature(catalogBytes, sigHex, pinnedToolchainPublicKeyHex()); err != nil {
		return catalogv2.Doc{}, fmt.Errorf("REFUSED: v2 catalog signature: %w", err)
	}

	doc, err := parseVerifiedCatalogV2(catalogBytes)
	if err != nil {
		return catalogv2.Doc{}, err
	}
	return doc, nil
}

// parseVerifiedCatalogV2 parses bytes whose signature has ALREADY been verified, then applies the full
// structural contract. Split out so the schema and ambiguity gates are unit-testable without a network.
//
// The raw-bytes signature check has no domain separation: a validly-signed toolchain manifest or a
// validly-signed v1 catalog would pass it. The schema tag is what makes this the v2 route's boundary.
func parseVerifiedCatalogV2(catalogBytes []byte) (catalogv2.Doc, error) {
	var doc catalogv2.Doc
	dec := json.NewDecoder(bytes.NewReader(catalogBytes))
	// Unknown fields are REFUSED rather than ignored: a document carrying a field this build does not
	// understand may be relying on it for meaning (a cohort, an expiry), and silently dropping it would
	// resolve a pair the publisher did not intend.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return catalogv2.Doc{}, fmt.Errorf("REFUSED: parse v2 catalog: %w", err)
	}
	// A second JSON value after the document is refused: the signature covers the whole byte string, so
	// stopping at the first value would accept bytes whose tail was never examined.
	// A SECOND Decode must report io.EOF.
	//
	// dec.More() is NOT sufficient here: it answers "is there another VALUE in the current array or
	// object", and returns false for stray tokens such as a bare `}` or `]`. Those bytes are still
	// covered by the signature, so a parser that stopped at the first value would accept a document
	// whose tail it never examined. Requiring io.EOF from a second Decode is the only check that the
	// signed byte string contains exactly one document and nothing else.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return catalogv2.Doc{}, fmt.Errorf("REFUSED: parse v2 catalog: trailing data after the document")
	}
	if err := doc.Validate(); err != nil {
		return catalogv2.Doc{}, err
	}
	return doc, nil
}

// catalogSelection is the resolved pair plus the route that produced it, so a caller (and a receipt)
// can state which catalog answered rather than inferring it.
type catalogSelection struct {
	Platform         string
	FrontendVersion  string
	ToolchainVersion string
	// FlutterRevision and FlutterVersion are populated from v2 only; v1 carries neither.
	FlutterRevision string
	FlutterVersion  string
	// Source is "v2", "v1" (direct, not opted in) or "v1-fallback" (v2 was absent). Direct v1 use is
	// NOT labelled a fallback: nothing fell back, and mislabelling it would make audit output claim a
	// downgrade that never happened.
	Source string
}

// resolveCatalogPair resolves the pair to install for platform.
//
//   - useV2 false: v1 exactly as before. Existing clients are not switched by this change.
//   - useV2 true: v2 selected by the EXACT flutterRevision; v1 only if v2 is genuinely not published.
//
// flutterRevision is required when useV2 is set, because v2's whole purpose is that a platform has more
// than one pair and only the caller knows which revision it is building. Defaulting it would reintroduce
// the guess v2 exists to remove.
func resolveCatalogPair(base, platform, flutterRevision string, useV2 bool) (catalogSelection, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if !useV2 {
		// Not opted in: plain v1, exactly as before. requireRevision is empty, so no revision gate is
		// applied — this is the historical behaviour and the label says "v1", not "v1-fallback",
		// because nothing fell back.
		return resolveCatalogPairV1(base, platform, "", "v1")
	}
	if strings.TrimSpace(flutterRevision) == "" {
		return catalogSelection{}, fmt.Errorf("REFUSED: v2 catalog selection requires an exact flutter revision")
	}

	doc, err := fetchVerifiedCatalogV2(base)
	switch {
	case err == nil:
		entry, selErr := doc.Select(platform, flutterRevision)
		if selErr != nil {
			// An unknown revision is a REFUSAL, not a reason to try v1. v1 would answer with its single
			// global pair, which is precisely the wrong engine for the revision being built.
			return catalogSelection{}, selErr
		}
		return catalogSelection{
			Platform:         platform,
			FrontendVersion:  entry.FrontendVersion,
			ToolchainVersion: entry.ToolchainVersion,
			FlutterRevision:  entry.FlutterRevision,
			FlutterVersion:   entry.FlutterVersion,
			Source:           "v2",
		}, nil
	case errors.Is(err, errCatalogV2NotPublished):
		// The one permitted fallback — and even here the v1 pair must PROVE it is for the requested
		// revision before it is accepted. See resolveCatalogPairV1.
		return resolveCatalogPairV1(base, platform, flutterRevision, "v1-fallback")
	default:
		// Invalid v2 response: refuse. Never fall back past a signature, schema or transport failure.
		return catalogSelection{}, err
	}
}

// resolveCatalogPairV1 resolves through v1.
//
// requireRevision is the SAFETY GATE ON FALLBACK, and it is the difference between a fallback and a
// downgrade. v1 pins exactly one pair per platform with no revision dimension, so it will happily answer
// a request made for ANY revision — including one its pair was not built from. Installing that pair
// would put an engine on a device that nobody built or qualified for the revision being compiled.
//
// So when a revision is required (the v2-404 fallback path), the v1 pair is not trusted on the catalog's
// word: both signed manifests are fetched and their flutter_revision must EQUAL the requested revision.
// A 3.44.9 request therefore refuses today's 3.44.2 v1 pair rather than installing it.
//
// requireRevision is empty only on the non-opted-in path, which is the pre-existing v1 behaviour and is
// left exactly as it was.
func resolveCatalogPairV1(base, platform, requireRevision, sourceLabel string) (catalogSelection, error) {
	doc, err := fetchVerifiedCatalog(base)
	if err != nil {
		return catalogSelection{}, err
	}
	entry, err := doc.entryForPlatform(platform)
	if err != nil {
		return catalogSelection{}, err
	}

	sel := catalogSelection{
		Platform:         platform,
		FrontendVersion:  entry.FrontendVersion,
		ToolchainVersion: entry.ToolchainVersion,
		Source:           sourceLabel,
	}
	requireRevision = strings.ToLower(strings.TrimSpace(requireRevision))
	if requireRevision == "" {
		return sel, nil
	}

	frontend, err := fetchCatalogFrontendManifest(base, entry.FrontendVersion)
	if err != nil {
		return catalogSelection{}, fmt.Errorf("REFUSED: v1 fallback frontend %q: %w", entry.FrontendVersion, err)
	}
	toolchain, err := fetchCatalogToolchainManifest(base, entry.ToolchainVersion)
	if err != nil {
		return catalogSelection{}, fmt.Errorf("REFUSED: v1 fallback toolchain %q: %w", entry.ToolchainVersion, err)
	}
	if got := strings.ToLower(strings.TrimSpace(frontend.FlutterRevision)); got != requireRevision {
		return catalogSelection{}, fmt.Errorf(
			"REFUSED: v1 fallback declines to serve flutter revision %s: the v1 frontend %q is built from %s. v1 pins one pair per platform and cannot express the requested revision; publish a v2 catalog entry for it",
			shortRev(requireRevision), entry.FrontendVersion, shortRev(got))
	}
	if got := strings.ToLower(strings.TrimSpace(toolchain.FlutterRevision)); got != requireRevision {
		return catalogSelection{}, fmt.Errorf(
			"REFUSED: v1 fallback declines to serve flutter revision %s: the v1 toolchain %q is built from %s",
			shortRev(requireRevision), entry.ToolchainVersion, shortRev(got))
	}
	sel.FlutterRevision = requireRevision
	sel.FlutterVersion = strings.TrimSpace(toolchain.FlutterVersion)
	return sel, nil
}

// preflightSelectedPair verifies ONE already-resolved pair before any installer runs: both manifests are
// fetched and signature-checked, the toolchain's platform is confirmed, the pair's mutual compatibility
// is confirmed, and both archives are probed.
//
// When the selection carries a revision (every v2 selection, and a revision-gated v1 fallback), the
// manifests must AGREE with it. The resolver already checked this for the fallback path, and the v2
// publisher checked it at publish time — re-checking here is deliberate: it is the last point before
// bytes land on disk, and it costs one comparison against manifests already being fetched.
// selectedPairPreflightFn is the single seam for the v2 install-time preflight, mirroring
// catalogReferencePreflightFn on the v1 path. Production always uses the real verifier; tests replace it
// to exercise runSetup's resolve -> install -> record path without standing up a full artifact registry.
var selectedPairPreflightFn = preflightSelectedPair

func preflightSelectedPair(base string, sel catalogSelection) (catalogPlatformPreflight, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}
	frontend, err := fetchCatalogFrontendManifest(base, sel.FrontendVersion)
	if err != nil {
		return catalogPlatformPreflight{}, fmt.Errorf("REFUSED: %s frontend %q: %w", sel.Platform, sel.FrontendVersion, err)
	}
	toolchain, err := fetchCatalogToolchainManifest(base, sel.ToolchainVersion)
	if err != nil {
		return catalogPlatformPreflight{}, fmt.Errorf("REFUSED: %s toolchain %q: %w", sel.Platform, sel.ToolchainVersion, err)
	}
	if err := validatePairIdentity(sel.Platform, sel.FlutterRevision, sel.FlutterVersion,
		sel.FrontendVersion, sel.ToolchainVersion, frontend, toolchain); err != nil {
		return catalogPlatformPreflight{}, fmt.Errorf("REFUSED: %s pair identity: %w", sel.Platform, err)
	}
	if err := probeSignedArchive(frontend.Archive.URL, frontend.Archive.CompressedBytes); err != nil {
		return catalogPlatformPreflight{}, fmt.Errorf("REFUSED: %s frontend archive: %w", sel.Platform, err)
	}
	if err := probeSignedArchive(toolchain.Archive.URL, toolchain.Archive.CompressedBytes); err != nil {
		return catalogPlatformPreflight{}, fmt.Errorf("REFUSED: %s toolchain archive: %w", sel.Platform, err)
	}
	return catalogPlatformPreflight{
		Platform: sel.Platform,
		Entry:    catalogPlatform{FrontendVersion: sel.FrontendVersion, ToolchainVersion: sel.ToolchainVersion},
		Frontend: frontend, Toolchain: toolchain,
	}, nil
}

// shortRev abbreviates a revision for human-readable errors. Lives on the untagged client side because
// both the public consumer path and the operator publisher path format revisions the same way.
func shortRev(rev string) string {
	if len(rev) >= 12 {
		return rev[:12]
	}
	return rev
}

// validatePairIdentity is THE pair-identity contract for v2, shared by the publisher (publish time) and
// by preflightSelectedPair (install time) so the two cannot drift.
//
// Publish-time checking alone is not sufficient. The catalog is signed, but the MANIFESTS it points at
// are fetched separately at install time, and a pair that was correct when published can stop being so
// if a version string is reused or a manifest is replaced. Re-checking here costs one comparison
// against manifests already being fetched, and it is the last point before bytes reach the disk.
//
// flutterVersion may be empty (a v1 fallback carries no readable metadata); when present it must agree.
func validatePairIdentity(platform, flutterRevision, flutterVersion, frontendVersion, toolchainVersion string,
	frontend frontendManifest, toolchain cliManifest) error {

	if got := strings.ToLower(strings.TrimSpace(toolchain.Platform)); got != platform {
		return fmt.Errorf("toolchain %q declares platform %q, not %q", toolchainVersion, got, platform)
	}

	flutterRevision = strings.ToLower(strings.TrimSpace(flutterRevision))
	if flutterRevision != "" {
		if got := strings.ToLower(strings.TrimSpace(frontend.FlutterRevision)); got != flutterRevision {
			return fmt.Errorf("frontend %q is built from flutter %s, not the selected %s",
				frontendVersion, shortRev(got), shortRev(flutterRevision))
		}
		if got := strings.ToLower(strings.TrimSpace(toolchain.FlutterRevision)); got != flutterRevision {
			return fmt.Errorf("toolchain %q is built from flutter %s, not the selected %s",
				toolchainVersion, shortRev(got), shortRev(flutterRevision))
		}
	}

	// A pair can share a Flutter revision and still be built against different Dart SDKs, producing a
	// frontend whose analyzer and a toolchain whose compiler disagree — which surfaces as confusing
	// compile errors rather than as a bad install.
	feDart := strings.ToLower(strings.TrimSpace(frontend.DartRevision))
	tcDart := strings.ToLower(strings.TrimSpace(toolchain.DartRevision))
	if feDart == "" || tcDart == "" {
		return fmt.Errorf("frontend %q and toolchain %q must both declare dart_revision", frontendVersion, toolchainVersion)
	}
	if feDart != tcDart && !allowsLegacyDartRevisionMismatch(frontendVersion, toolchainVersion, feDart, tcDart) {
		return fmt.Errorf("dart_revision mismatch: frontend %q declares %s, toolchain %q declares %s",
			frontendVersion, shortRev(feDart), toolchainVersion, shortRev(tcDart))
	}

	if !containsExact(frontend.CompatibleToolchainIDs, toolchainVersion) {
		return fmt.Errorf("pair is not bound: frontend %q does not declare toolchain %q compatible",
			frontendVersion, toolchainVersion)
	}

	// The readable metadata is not a selector, but it IS what a human reads when deciding what to
	// qualify, so a document claiming "3.44.9" over a 3.44.2 toolchain would mislead exactly the person
	// the field exists for.
	// For a v2 entry the readable version must be PRESENT on both sides and identical. An empty
	// toolchain flutter_version used to pass, which meant the one field a human reads when deciding what
	// to qualify could be unverifiable — the catalog would assert "3.44.9" with nothing signed behind it.
	// flutterVersion is empty only on the v1 fallback path, which carries no such metadata.
	if want := strings.TrimSpace(flutterVersion); want != "" {
		got := strings.TrimSpace(toolchain.FlutterVersion)
		if got == "" {
			return fmt.Errorf("toolchain %q declares no flutter_version, so the catalog's %q cannot be verified",
				toolchainVersion, want)
		}
		if got != want {
			return fmt.Errorf("flutter_version metadata %q does not match toolchain %q which declares %q",
				want, toolchainVersion, got)
		}
	}
	return nil
}

// resolveCatalogPairsFromOneSnapshot resolves EVERY requested platform from a SINGLE verified v2
// document.
//
// Resolving platform-by-platform re-fetched the catalog per platform, so an android pair and an ios
// pair could come from DIFFERENT catalog generations if a publish landed between the two requests —
// a mixed set nobody ever published together and nobody reviewed as a whole. One fetch, one document,
// one set of selections.
//
// The fallback rule is unchanged: only a document-route 404 permits v1, and the v1 pair still has to
// prove it is for the requested revision.
func resolveCatalogPairsFromOneSnapshot(base string, platforms []string, flutterRevision string) (map[string]catalogSelection, error) {
	if strings.TrimSpace(flutterRevision) == "" {
		return nil, fmt.Errorf("REFUSED: v2 catalog selection requires an exact flutter revision")
	}
	out := make(map[string]catalogSelection, len(platforms))

	doc, err := fetchVerifiedCatalogV2(base)
	switch {
	case err == nil:
		for _, platform := range platforms {
			platform = strings.ToLower(strings.TrimSpace(platform))
			entry, selErr := doc.Select(platform, flutterRevision)
			if selErr != nil {
				return nil, selErr
			}
			out[platform] = catalogSelection{
				Platform:         platform,
				FrontendVersion:  entry.FrontendVersion,
				ToolchainVersion: entry.ToolchainVersion,
				FlutterRevision:  entry.FlutterRevision,
				FlutterVersion:   entry.FlutterVersion,
				Source:           "v2",
			}
		}
		return out, nil
	case errors.Is(err, errCatalogV2NotPublished):
		for _, platform := range platforms {
			platform = strings.ToLower(strings.TrimSpace(platform))
			sel, fbErr := resolveCatalogPairV1(base, platform, flutterRevision, "v1-fallback")
			if fbErr != nil {
				return nil, fbErr
			}
			out[platform] = sel
		}
		return out, nil
	default:
		return nil, err
	}
}
