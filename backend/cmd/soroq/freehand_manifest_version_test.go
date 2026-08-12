package main

import (
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

func mkPatch(app, channel, release string, number int, rolledBack bool) domain.Patch {
	return domain.Patch{
		AppID: app, Channel: channel, ReleaseID: release, Number: number, RolledBack: rolledBack,
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

// Versions are per (app, channel, release): parallel scopes must not collide or interfere.
func TestVersionScopeIsolation(t *testing.T) {
	all := []domain.Patch{
		mkPatch("app", "stable", "rel", 1, false),
		mkPatch("app", "stable", "rel", 2, false),
		mkPatch("app", "beta", "rel", 7, false),
		mkPatch("other", "stable", "rel", 9, false),
		mkPatch("app", "stable", "otherRel", 5, false),
	}
	if got := nextManifestVersion(scopedPatches(all, "app", "stable", "rel")); got != 3 {
		t.Errorf("app/stable/rel next = %d, want 3", got)
	}
	if got := nextManifestVersion(scopedPatches(all, "app", "beta", "rel")); got != 8 {
		t.Errorf("app/beta/rel next = %d, want 8 (a parallel channel must not be dragged along)", got)
	}
	if got := nextManifestVersion(scopedPatches(all, "app", "stable", "brandNew")); got != 1 {
		t.Errorf("a fresh release must start at 1, got %d", got)
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
