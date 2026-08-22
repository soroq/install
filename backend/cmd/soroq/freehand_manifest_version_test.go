package main

import (
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

// The third argument is the RUNTIME id, which is the scope the control plane numbers by. Each patch
// also gets a release id derived from it, so a test that accidentally scoped by release would still
// see a coherent fixture rather than an empty one.
func mkPatch(app, channel, runtime string, number int, rolledBack bool) domain.Patch {
	return domain.Patch{
		AppID: app, Channel: channel, RuntimeID: runtime, ReleaseID: "rel-of-" + runtime,
		Number: number, RolledBack: rolledBack,
	}
}

// THE DEFECT: every patch shipped as version 1, so a device silently ignored every patch after its first.
func TestVersionIsMonotonicNotAlwaysOne(t *testing.T) {
	var scope []domain.Patch
	for want := 1; want <= 4; want++ {
		got := nextManifestVersion(scope)
		if got != want {
			t.Fatalf("publish #%d derived version %d, want %d (hardcoding 1 is the bug being fixed)", want, got, want)
		}
		scope = append(scope, mkPatch("app", "ch", "rel", got, false))
	}
}

// A rolled-back patch still CONSUMED its version. Reusing it would make a device that saw the original
// treat the replacement as already-applied -- exactly the silent no-op being fixed.
func TestRolledBackVersionsAreNotReused(t *testing.T) {
	scope := []domain.Patch{
		mkPatch("app", "ch", "rel", 1, true),
		mkPatch("app", "ch", "rel", 2, true),
	}
	if got := nextManifestVersion(scope); got != 3 {
		t.Fatalf("next version = %d, want 3; rolled-back versions must not be recycled", got)
	}
}

// Version 0 is the signed rollback instruction and can never be a patch.
func TestVersionZeroIsReservedForRollback(t *testing.T) {
	err := validateManifestVersion(reservedRollbackVersion, nil)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("version 0 must be refused as reserved, got %v", err)
	}
	if err := validateManifestVersion(-1, nil); err == nil {
		t.Error("negative version accepted")
	}
}

// A stale explicit --patch-version must be refused loudly, not silently ignored by the device.
func TestStaleVersionIsRefusedWithAnActionableMessage(t *testing.T) {
	scope := []domain.Patch{
		mkPatch("app", "ch", "rel", 1, false),
		mkPatch("app", "ch", "rel", 2, false),
	}
	err := validateManifestVersion(2, scope)
	if err == nil {
		t.Fatal("republishing version 2 over an existing version 2 was accepted; the device would ignore it")
	}
	for _, want := range []string{"already-applied", "use 3 or higher"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must explain %q; got: %v", want, err)
		}
	}
	if err := validateManifestVersion(3, scope); err != nil {
		t.Errorf("the next version must be accepted: %v", err)
	}
	if err := validateManifestVersion(9, scope); err != nil {
		t.Errorf("a higher-than-next version must be accepted: %v", err)
	}
}

// Versions are per (app, channel, runtime): parallel scopes must not collide or interfere.
func TestVersionScopeIsolation(t *testing.T) {
	all := []domain.Patch{
		mkPatch("app", "stable", "rt", 1, false),
		mkPatch("app", "stable", "rt", 2, false),
		mkPatch("app", "beta", "rt", 7, false),
		mkPatch("other", "stable", "rt", 9, false),
		mkPatch("app", "stable", "otherRt", 5, false),
	}
	if got := nextManifestVersion(scopedPatches(all, "app", "stable", "rt")); got != 3 {
		t.Errorf("app/stable/rt next = %d, want 3", got)
	}
	if got := nextManifestVersion(scopedPatches(all, "app", "beta", "rt")); got != 8 {
		t.Errorf("app/beta/rt next = %d, want 8 (a parallel channel must not be dragged along)", got)
	}
	if got := nextManifestVersion(scopedPatches(all, "app", "stable", "brandNewRt")); got != 1 {
		t.Errorf("a fresh runtime must start at 1, got %d", got)
	}
}

// THE SCOPE MUST BE THE SERVER'S SCOPE.
//
// The control plane numbers patches per (app_id, runtime_id, channel). The client used to resolve the
// version per (app, channel, RELEASE), and the two agree only while every runtime has exactly one
// release. The moment a second release is registered against the same runtime -- which is ordinary,
// since the runtime id is version-derived and does not change when only the app code changes -- the
// client signs version 1, the server allocates 2, and the publish aborts with
//
//	concurrent publication detected: ... another publish won the race
//
// naming a race that never happened. This pins the disagreement rather than the symptom: a fixture
// where one runtime carries two releases must resolve to the number the server would allocate.
func TestVersionScopeMatchesTheControlPlaneAllocationScope(t *testing.T) {
	// One runtime, two releases -- the exact shape that broke.
	all := []domain.Patch{
		{AppID: "app", Channel: "stable", RuntimeID: "rt1", ReleaseID: "release-A", Number: 1},
		{AppID: "app", Channel: "stable", RuntimeID: "rt1", ReleaseID: "release-A", Number: 2},
	}
	// Publishing under a NEW release id against the SAME runtime must continue the runtime's sequence.
	if got := nextManifestVersion(scopedPatches(all, "app", "stable", "rt1")); got != 3 {
		t.Fatalf("next version for runtime rt1 = %d, want 3; scoping by release would have said 1 and "+
			"the control plane would have allocated 3", got)
	}
	// And a genuinely different runtime still starts fresh.
	if got := nextManifestVersion(scopedPatches(all, "app", "stable", "rt2")); got != 1 {
		t.Fatalf("a different runtime must start at 1, got %d", got)
	}
}

// CONCURRENCY: the manifest is signed before the control plane allocates the number, so two parallel
// publishes can predict the same one. Exactly one wins; the loser must fail closed, never ship a manifest
// whose signed version disagrees with its allocated number.
func TestConcurrentPublicationCannotShipADuplicateVersion(t *testing.T) {
	if err := assertAllocatedVersionMatches(3, mkPatch("app", "ch", "rel", 3, false)); err != nil {
		t.Fatalf("matching allocation must pass: %v", err)
	}
	err := assertAllocatedVersionMatches(3, mkPatch("app", "ch", "rel", 4, false))
	if err == nil {
		t.Fatal("a mismatched allocation was accepted; two deployments could share a version")
	}
	for _, want := range []string{"signed for version 3", "allocated number 4", "re-run the publish"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message must explain %q; got: %v", want, err)
		}
	}
}

// IDEMPOTENT REPUBLISH: byte-identical CONTENT keeps its immutable identity; only the DEPLOYMENT version
// advances. Content identity is not touched by this file, so republishing must be legal.
func TestByteIdenticalContentCanBeRepublishedAtANewVersion(t *testing.T) {
	scope := []domain.Patch{mkPatch("app", "ch", "rel", 1, false)}
	next := nextManifestVersion(scope)
	if next != 2 {
		t.Fatalf("next = %d, want 2", next)
	}
	if err := validateManifestVersion(next, scope); err != nil {
		t.Fatalf("republishing identical content at the next version must be allowed: %v", err)
	}
}

func TestExistingScopedPatchesIsOfflineSafe(t *testing.T) {
	// Offline emit: no control plane to consult, so no patches and therefore version 1.
	got, err := existingScopedPatches("", "app", "ch", "rel")
	if err != nil {
		t.Fatalf("an empty api base must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no patches offline, got %d", len(got))
	}
	if v := nextManifestVersion(got); v != 1 {
		t.Errorf("offline derived version = %d, want 1", v)
	}
}

// TestPublishVersionQueryUsesThePublishControlPlane pins the trap that shipped: the deployment-version
// query read the RAW --api flag while the publish itself resolved the default hosted API. With --api
// omitted -- the ordinary invocation -- the query base was the empty string, existingScopedPatches
// took its offline-safe path above WITHOUT asking anything, and every patch was signed as version 1.
//
// The offline-safe path is correct for `--emit-signed-manifest` and is kept. What must never happen
// again is a PUBLISH reaching it, so this asserts the two halves resolve the same non-empty base.
func TestPublishVersionQueryUsesThePublishControlPlane(t *testing.T) {
	var noFlags []string
	raw, _ := flagValue(noFlags, "api")
	if raw != "" {
		t.Fatalf("precondition: with no --api the raw flag must be empty, got %q", raw)
	}
	resolved := freehandPublishAPIBase(noFlags)
	if strings.TrimSpace(resolved) == "" {
		t.Fatal("freehandPublishAPIBase returned an empty base with no --api; a publish would sign version 1 forever")
	}
	if resolved != defaultAPIBase() {
		t.Fatalf("freehandPublishAPIBase(no flags) = %q, want the default %q", resolved, defaultAPIBase())
	}
	// And the explicit flag still wins, so a publish against a non-default control plane asks THAT one.
	if got := freehandPublishAPIBase([]string{"--api", "https://example.invalid/api"}); got != "https://example.invalid/api" {
		t.Fatalf("explicit --api not honoured: %q", got)
	}
}
