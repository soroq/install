package main

// REPUBLISHING BYTE-IDENTICAL CONTENT.
//
// The defect: `resolveFreehandPublishParams` set `Version = 1` and derived the default patch id from
// it (`freehand-<artifact12>-v1`); the caller assigned the real monotonic version only afterwards,
// which did not rebuild the id. So republishing unchanged content regenerated an IDENTICAL id and the
// control plane rejected it on `patches_pkey` -- even though freehand_manifest_version.go documents
// the opposite intent:
//
//	CONTENT identity      the artifact id / payload sha -- immutable and content-addressed
//	DEPLOYMENT identity   the manifest version -- monotonic, and what ordering decisions use
//
// These tests pin the separation: same bytes keep their content identity forever, while each
// deployment takes a distinct version AND a distinct default patch id.

import (
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

const republishArtifact = "d2608faa1f55aa0011223344556677889900aabbccddeeff00112233445566"

func republishHead(extra ...string) []string {
	return append([]string{"--api", "https://cp.example", "--release-id", "rel-1", "--channel", "stable"}, extra...)
}

// THE REGRESSION. Two deployments of the SAME artifact must not collide.
func TestByteIdenticalContentGetsDistinctDeploymentIDs(t *testing.T) {
	v1, err := resolveFreehandPublishParams(republishHead(), "app", "runtime", "stable", republishArtifact, 1)
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2, err := resolveFreehandPublishParams(republishHead(), "app", "runtime", "stable", republishArtifact, 2)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v1.PatchID == v2.PatchID {
		t.Fatalf("identical content produced the same patch id %q for versions 1 and 2; "+
			"this is the patches_pkey collision", v1.PatchID)
	}
	if v1.Version != 1 || v2.Version != 2 {
		t.Fatalf("deployment versions not threaded through: v1=%d v2=%d", v1.Version, v2.Version)
	}
	// The id must still be derived from the CONTENT, so a deployment is traceable to its artifact.
	for _, p := range []freehandPublishParams{v1, v2} {
		if !strings.Contains(p.PatchID, republishArtifact[:12]) {
			t.Errorf("patch id %q no longer identifies its artifact", p.PatchID)
		}
	}
}

// A rollback consumes a version, so the republish after it must land above the rollback, not reuse
// the pre-rollback number.
func TestRepublishAfterRollbackTakesTheNextVersion(t *testing.T) {
	scope := []domain.Patch{
		{AppID: "app", Channel: "stable", ReleaseID: "rel-1", Number: 1}, // original
		{AppID: "app", Channel: "stable", ReleaseID: "rel-1", Number: 2}, // rollback row
	}
	next := nextManifestVersion(scope)
	if next != 3 {
		t.Fatalf("next version after a rollback = %d, want 3", next)
	}
	if err := validateManifestVersion(next, scope); err != nil {
		t.Fatalf("republishing identical content at the next version must be allowed: %v", err)
	}
	orig, _ := resolveFreehandPublishParams(republishHead(), "app", "runtime", "stable", republishArtifact, 1)
	re, _ := resolveFreehandPublishParams(republishHead(), "app", "runtime", "stable", republishArtifact, next)
	if orig.PatchID == re.PatchID {
		t.Fatal("the post-rollback republish reused the original patch id")
	}
}

// Content identity is NOT a function of the deployment version: the same bytes keep one logical
// artifact id and one payload sha across every redeployment.
func TestContentIdentityIsStableAcrossDeployments(t *testing.T) {
	ids := map[string]bool{}
	for v := 1; v <= 5; v++ {
		p, err := resolveFreehandPublishParams(republishHead(), "app", "runtime", "stable", republishArtifact, v)
		if err != nil {
			t.Fatal(err)
		}
		if ids[p.PatchID] {
			t.Fatalf("deployment %d reused patch id %q", v, p.PatchID)
		}
		ids[p.PatchID] = true
		// The artifact prefix -- the CONTENT identity -- is the same every time.
		if !strings.HasPrefix(p.PatchID, "freehand-"+republishArtifact[:12]) {
			t.Fatalf("deployment %d changed the content identity portion: %q", v, p.PatchID)
		}
	}
	if len(ids) != 5 {
		t.Fatalf("expected 5 distinct deployment ids, got %d", len(ids))
	}
}

// An explicit --patch-id must still win, unchanged, so an operator keeps full control of naming.
func TestExplicitPatchIDIsPreservedVerbatim(t *testing.T) {
	p, err := resolveFreehandPublishParams(
		republishHead("--patch-id", "my-own-id"), "app", "runtime", "stable", republishArtifact, 7)
	if err != nil {
		t.Fatal(err)
	}
	if p.PatchID != "my-own-id" {
		t.Fatalf("explicit --patch-id was overridden: %q", p.PatchID)
	}
	if p.Version != 7 {
		t.Fatalf("explicit id must not affect the deployment version, got %d", p.Version)
	}
}

// The version is the caller's to resolve. A non-positive value is a wiring bug, not something to
// paper over with a default -- defaulting is exactly what produced the collision.
func TestDeploymentVersionMustBeSuppliedByTheCaller(t *testing.T) {
	for _, v := range []int{0, -1} {
		if _, err := resolveFreehandPublishParams(republishHead(), "app", "runtime", "stable", republishArtifact, v); err == nil {
			t.Errorf("version %d was accepted; the id would silently be derived from a placeholder", v)
		}
	}
}

// CONCURRENCY. Two publishers predicting the same version now also predict the same id, so the loser
// must receive an ACTIONABLE refusal, not a raw database error. The signed-vs-allocated gate is the
// one that tells the operator what to do.
func TestConcurrentLoserGetsAnActionableRefusal(t *testing.T) {
	err := assertAllocatedVersionMatches(3, domain.Patch{Number: 4})
	if err == nil {
		t.Fatal("a mismatched allocation must fail closed")
	}
	for _, want := range []string{"signed for version 3", "allocated number 4", "re-run the publish"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must explain %q; got: %v", want, err)
		}
	}
}
