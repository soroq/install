package main

// DEPLOYMENT VERSIONING — the number a device uses to decide "is this newer than what I run?".
//
// The defect this fixes, observed on a physical iPhone: the device manifest version was hardcoded
//
//	version := 1        // freehand_patch.go, unless --patch-version was passed
//
// so EVERY freehand patch shipped as version 1 (every published patch id ends `-v1`). The controller
// treats `version == _activeVersion && activeBytecode exists` as already-applied and reactivates the
// bytecode it already has, so the second and every later patch was silently a no-op. Patch A rendered;
// patch B was published as a distinct artifact with a distinct payload, was served, and the device kept
// showing A. B only applied after a clean reinstall reset the active version to 0.
//
// Consequences: only the first patch a device ever received could apply, and the v2 stale-clear gate was
// unreachable because a v2 could not be delivered at all.
//
// Two identities are deliberately kept apart here:
//
//	CONTENT identity      the artifact id / payload sha -- immutable, content-addressed, and unchanged
//	                      by this file. Byte-identical content keeps its identity forever.
//	DEPLOYMENT identity   the manifest version -- monotonic per (app, channel, release/runtime), and
//	                      what ordering decisions are made on.
//
// Republishing byte-identical content is therefore legal: same content identity, new deployment version.

import (
	"fmt"
	"net/url"
	"strings"

	"soroq/backend/internal/domain"
)

// reservedRollbackVersion is version 0. It is the signed "return to base" instruction -- built and
// asserted by the single rollback signer in internal/signing (used by `soroqctl rollback ios-engine`)
// -- and must never be allocated to a patch.
const reservedRollbackVersion = 0

// nextManifestVersion returns the deployment version to publish next for a scope, given every patch
// already registered in it.
//
// It is the maximum existing number plus one, so it is strictly increasing even when earlier patches were
// rolled back — a rolled-back patch still consumed its version, and reusing it would make a device that
// saw the original treat the new one as already-applied. That is precisely the bug being fixed, so
// rolled-back patches are counted, not skipped.
func nextManifestVersion(existing []domain.Patch) int {
	max := 0
	for _, p := range existing {
		if p.Number > max {
			max = p.Number
		}
	}
	return max + 1
}

// scopedPatches filters a patch list to one deployment scope. Versions are monotonic PER
// (app, channel, release) so parallel channels or releases never collide with each other.
func scopedPatches(all []domain.Patch, appID, channel, releaseID string) []domain.Patch {
	appID, channel, releaseID = strings.TrimSpace(appID), strings.TrimSpace(channel), strings.TrimSpace(releaseID)
	out := make([]domain.Patch, 0, len(all))
	for _, p := range all {
		if p.AppID != appID || p.Channel != channel {
			continue
		}
		if releaseID != "" && p.ReleaseID != releaseID {
			continue
		}
		out = append(out, p)
	}
	return out
}

// validateManifestVersion is the fail-closed gate applied to any version about to be signed, whether it
// was derived automatically or supplied via --patch-version.
func validateManifestVersion(version int, existing []domain.Patch) error {
	if version == reservedRollbackVersion {
		return fmt.Errorf("manifest version 0 is reserved for the signed rollback instruction and cannot be used for a patch")
	}
	if version < 0 {
		return fmt.Errorf("manifest version must be positive, got %d", version)
	}
	next := nextManifestVersion(existing)
	if version < next {
		// Publishing a stale version is the exact shape of the original defect: the device would compare
		// it against what it already runs and silently do nothing. Refuse loudly instead.
		return fmt.Errorf("manifest version %d is not greater than the highest already published (%d) for this app/channel/release; "+
			"a device already running v%d would treat it as already-applied and silently ignore it — use %d or higher",
			version, next-1, next-1, next)
	}
	return nil
}

// assertAllocatedVersionMatches is the CONCURRENCY gate, run after the control plane has registered the
// patch and assigned its number.
//
// The manifest is signed BEFORE registration, so the version has to be predicted. If two operators
// publish into the same scope at once they can predict the same number, and exactly one will be assigned
// it. The loser must fail closed and retry rather than ship a manifest whose signed version disagrees
// with its allocated number — a duplicate deployment version is the defect this whole file exists to
// prevent.
func assertAllocatedVersionMatches(signedVersion int, allocated domain.Patch) error {
	if allocated.Number == signedVersion {
		return nil
	}
	return fmt.Errorf("concurrent publication detected: this manifest was signed for version %d but the control plane allocated number %d "+
		"(another publish won the race). Nothing was served with a mismatched version; re-run the publish to sign version %d",
		signedVersion, allocated.Number, allocated.Number)
}

// existingScopedPatches fetches every patch already registered in a deployment scope, so the next
// version can be derived before anything is signed.
//
// An empty api base means an offline emit (`--emit-signed-manifest`) with no control plane to consult; it
// returns no patches, so the derived version is 1. That is correct for an artifact that is not being
// deployed, and `validateManifestVersion` still refuses version 0.
func existingScopedPatches(apiBase, appID, channel, releaseID string) ([]domain.Patch, error) {
	apiBase = strings.TrimSpace(apiBase)
	if apiBase == "" {
		return nil, nil
	}
	q := url.Values{}
	if s := strings.TrimSpace(appID); s != "" {
		q.Set("app_id", s)
	}
	if s := strings.TrimSpace(channel); s != "" {
		q.Set("channel", s)
	}
	if s := strings.TrimSpace(releaseID); s != "" {
		q.Set("release_id", s)
	}
	listURL := strings.TrimRight(apiBase, "/") + "/v1/patches"
	if enc := q.Encode(); enc != "" {
		listURL += "?" + enc
	}
	all, err := getJSONDecode[[]domain.Patch](listURL)
	if err != nil {
		return nil, err
	}
	// Filter again client-side: the server query is a convenience, but the version invariant is per
	// (app, channel, release) and must not depend on server-side filter semantics.
	return scopedPatches(all, appID, channel, releaseID), nil
}
