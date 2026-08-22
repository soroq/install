package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testIdentity(t *testing.T) FreehandRichBaseIdentity {
	t.Helper()
	id, err := newFreehandRichBaseIdentity("runtime-A", "dill-A", "contract-A", "retention-A")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// GREEN CONTROL. The asset must be exactly what the device re-derives, or every launch refuses.
func TestBaseIdentityAssetRoundTrips(t *testing.T) {
	id := testIdentity(t)
	raw, err := freehandBaseIdentityAssetBytes(id)
	if err != nil {
		t.Fatal(err)
	}
	var got freehandBaseIdentityAsset
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != freehandBaseIdentityAssetSchema {
		t.Fatalf("schema = %q", got.Schema)
	}
	if got.RuntimeID != id.RuntimeID || got.BaseFingerprint != id.BaseFingerprint ||
		got.ContractDigest != id.ContractDigest || got.RetentionDigest != id.RetentionDigest {
		t.Fatalf("fields did not round-trip: %+v", got)
	}
	want := freehandBaseIdentityDigest(id.RuntimeID, id.BaseFingerprint, id.ContractDigest, id.RetentionDigest)
	if got.Digest != want {
		t.Fatalf("digest = %q, want %q", got.Digest, want)
	}
}

// PLANTED FAILURE. A digest that does not describe its own fields would be refused on every device,
// so it must be refused here, where an operator can see it.
func TestBaseIdentityAssetRefusesInconsistentDigest(t *testing.T) {
	id := testIdentity(t)
	id.BaseFingerprint = "edited-after-the-digest-was-taken"
	if _, err := freehandBaseIdentityAssetBytes(id); err == nil {
		t.Fatal("an inconsistent digest was written instead of refused")
	}
}

func TestBaseIdentityAssetRefusesEmptyDigest(t *testing.T) {
	id := testIdentity(t)
	id.Digest = ""
	if _, err := freehandBaseIdentityAssetBytes(id); err == nil {
		t.Fatal("an identity with no digest was written")
	}
}

// The asset must land in EVERY built bundle. A build that produces both a Profile and a device bundle
// and gets the identity into only one ships an app that refuses every patch depending on which one was
// packaged.
func TestBaseIdentityAssetWritesEveryBundle(t *testing.T) {
	proj := t.TempDir()
	bundles := []string{
		filepath.Join(proj, "build", "ios", "iphoneos", "Runner.app"),
		filepath.Join(proj, "build", "ios", "Profile-iphoneos", "Runner.app"),
	}
	// A nested bundle inside another one is an app extension, not a target for this.
	nested := filepath.Join(bundles[0], "PlugIns", "Widget.appex.app")
	for _, d := range append(append([]string{}, bundles...), nested) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	written, err := writeFreehandBaseIdentityAsset(proj, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != len(bundles) {
		t.Fatalf("wrote %d assets, want %d: %v", len(written), len(bundles), written)
	}
	for _, b := range bundles {
		if _, err := os.Stat(filepath.Join(b, freehandBaseIdentityAssetName)); err != nil {
			t.Fatalf("no identity asset in %s: %v", b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(nested, freehandBaseIdentityAssetName)); err == nil {
		t.Fatal("descended into a nested bundle")
	}
}

// FAIL CLOSED. Printing success while delivering no identity produces an app that refuses every patch,
// and the operator finds out from a phone.
func TestBaseIdentityAssetRefusesWhenThereIsNoBundle(t *testing.T) {
	proj := t.TempDir()
	_, err := writeFreehandBaseIdentityAsset(proj, testIdentity(t))
	if err == nil {
		t.Fatal("a release with no app bundle reported success")
	}
	if !strings.Contains(err.Error(), "nowhere to go") {
		t.Fatalf("refusal does not explain itself: %v", err)
	}
}

// The two sides of the asset are a WIRE CONTRACT: this writes the file, Dart reads it. A schema or
// filename changed on one side only produces an app that reports "no identity asset" and refuses every
// patch, with nothing in either test suite failing. So both strings are asserted against the Dart
// source, the same way the identity digest is pinned on both sides.
func TestBaseIdentityAssetContractMatchesDart(t *testing.T) {
	src, err := os.ReadFile("../../packages/soroq_flutter/lib/src/base_identity.dart")
	if err != nil {
		t.Skipf("dart source not available: %v", err)
	}
	for _, want := range []string{
		"soroqBaseIdentityAssetSchema = '" + freehandBaseIdentityAssetSchema + "'",
		"soroqBaseIdentityAssetName = '" + freehandBaseIdentityAssetName + "'",
	} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("base_identity.dart does not declare %q; the producer and the reader have drifted", want)
		}
	}
	// Every key this writes must be read on the other side.
	for _, key := range []string{"schema", "runtime_id", "base_fingerprint", "contract_digest", "retention_digest", "digest"} {
		if !strings.Contains(string(src), "'"+key+"'") {
			t.Fatalf("base_identity.dart never reads the %q key", key)
		}
	}
}
