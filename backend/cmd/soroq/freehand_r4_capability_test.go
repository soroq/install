package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// r4 CHANGES HOW ACCESSORS ARE FOUND, NOT WHAT THE ENGINE PROMISES.
//
// r3 already declares public_instance_field_accessor_dynamic_dispatch_v1: public instance field
// accessors resolve through selector dispatch. r3 simply failed to deliver it -- it located accessors by
// building a name, missed every implicit getter, and the first patched frame took EXC_BAD_ACCESS at 0x20
// instead of rendering. r4 finds the same set through the VM's own accessor_field() association.
//
// The observable contract is identical, so r4 introduces NO new capability. A fourth name would claim a
// behaviour nobody can point at, and would let an r4 base be told apart from a correct r3 base by
// something other than whether it works.

func TestR4IntroducesNoNewCapability(t *testing.T) {
	// R6 added exactly one name, obfuscated_identity_translation_v1, for a behaviour that can be
	// pointed at: translating module identities into an obfuscated base's namespace. r4 still adds
	// none, which is what this asserts -- the set is the three r3 names plus that one, and nothing
	// else.
	if len(freehandKnownIdentityCapabilities) != 4 {
		t.Fatalf("the closed set has %d entries, want the 3 r3 names plus the r6 one: %v",
			len(freehandKnownIdentityCapabilities), freehandKnownIdentityCapabilities)
	}
	if !freehandKnownIdentityCapabilities[freehandObfuscatedIdentityTranslationCapability] {
		t.Fatalf("the closed set lost the r6 capability %q", freehandObfuscatedIdentityTranslationCapability)
	}
	for _, want := range []string{
		freehandPrivateEnclosingClassCapability,
		freehandPublicFieldAccessorRetentionCapability,
		freehandPublicFieldAccessorDynamicDispatchCapability,
	} {
		if !freehandKnownIdentityCapabilities[want] {
			t.Fatalf("the closed set lost %q", want)
		}
	}
}

// The r4 engine declares exactly what r3 declared, and a base built on it says so.
func TestR4CapabilityTripletIsAccepted(t *testing.T) {
	_, caps, err := decodeEngineCapabilityDeclaration([]byte(`{"honoured_kinds":["method","getter","setter"],
		"identity_capabilities":[
			"private_enclosing_class_identity_v1",
			"public_instance_field_accessor_retention_v1",
			"public_instance_field_accessor_dynamic_dispatch_v1"]}`))
	if err != nil {
		t.Fatalf("the r4 capability triplet was refused: %v", err)
	}
	if len(caps) != 3 {
		t.Fatalf("expected three capabilities, got %v", caps)
	}
	rec := &FreehandRedirectCapabilities{IdentityCapabilities: caps}
	for _, c := range []string{
		freehandPrivateEnclosingClassCapability,
		freehandPublicFieldAccessorRetentionCapability,
		freehandPublicFieldAccessorDynamicDispatchCapability,
	} {
		if !rec.hasIdentityCapability(c) {
			t.Fatalf("an r4 base does not carry %q", c)
		}
	}
}

// A speculative fourth name must stay refused. This is the realistic slip while iterating engines:
// inventing a capability per revision until the set stops meaning anything.
func TestSpeculativeR4CapabilityNamesAreRefused(t *testing.T) {
	for _, name := range []string{
		"public_instance_field_accessor_dynamic_dispatch_v2",
		"public_instance_field_accessor_association_v1",
		"accessor_field_association_v1",
		"public_instance_field_accessor_retention_v2",
	} {
		body := `{"honoured_kinds":["method"],"identity_capabilities":["` + name + `"]}`
		if _, _, err := decodeEngineCapabilityDeclaration([]byte(body)); err == nil {
			t.Fatalf("capability %q was accepted; the set is not closed", name)
		}
	}
}

// --- PATCH-SET IMMUTABILITY ---------------------------------------------------------------------
//
// Each earlier dart.patch is the exact file whose sha256 a published canonical records. Editing one in
// place would make a published, immutable artifact unreproducible from this tree while still claiming
// that hash -- and it would do it silently, because the canonical and the file drift together only if
// someone edits both.

func patchSHA(t *testing.T, dir string) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "tooling", "flutter_matrix", "patches")
	// The public CLI mirror (scripts/export-public-cli.sh) exports backend/ only, so the patch tree is
	// structurally absent there. Skip ONLY when the whole tree is missing; in the canonical repo a
	// missing individual patch is still a hard failure below.
	if _, err := os.Stat(root); os.IsNotExist(err) {
		t.Skip("canonical-repo check: tooling/flutter_matrix/patches is not part of the public mirror")
	}
	p := filepath.Join(root, dir, "dart.patch")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPublishedPatchSetsAreUnchanged(t *testing.T) {
	for dir, want := range map[string]string{
		"3.44.9":                  "2916ffa382248a3cba5811f5b9814982eca5257c449095d80d39f890a4fd2343",
		"3.44.9-private-state-r2": "a2b53b7688f4a844f6c1fa45403a0f15a9ae53f1268dbf0be72bee574efbb31b",
		"3.44.9-private-state-r3": "c898ac5818a61a076e8bf2627c26b8406b3473a045586a18f71cda772e7c37af",
		"3.44.9-private-state-r4": "f3118974fecedc34228154274076be5ce2fb5ec6ac4bd2ab3855dd46452b9185",
	} {
		if got := patchSHA(t, dir); got != want {
			t.Fatalf("%s/dart.patch changed: published canonical records %s, tree has %s", dir, want, got)
		}
	}
}

func TestR5KeepsTheR4CapabilityContract(t *testing.T) {
	// r5 still adds nothing. The set grew to four only in R6, and only by
	// obfuscated_identity_translation_v1 -- a behaviour that can be pointed at.
	if len(freehandKnownIdentityCapabilities) != 4 {
		t.Fatalf("the closed capability set is %v, want the 3 r3 names plus the r6 one", freehandKnownIdentityCapabilities)
	}
	if !freehandKnownIdentityCapabilities[freehandObfuscatedIdentityTranslationCapability] {
		t.Fatalf("the closed set lost the r6 capability %q", freehandObfuscatedIdentityTranslationCapability)
	}
	r5 := patchSHA(t, "3.44.9-private-state-r5")
	for _, prior := range []string{"3.44.9", "3.44.9-private-state-r2", "3.44.9-private-state-r3", "3.44.9-private-state-r4"} {
		if r5 == patchSHA(t, prior) {
			t.Fatalf("r5 is byte-identical to %s; the interpreter selector fix is absent", prior)
		}
	}
}

func TestR4IsADistinctPatchSet(t *testing.T) {
	r4 := patchSHA(t, "3.44.9-private-state-r4")
	for _, prior := range []string{"3.44.9", "3.44.9-private-state-r2", "3.44.9-private-state-r3"} {
		if r4 == patchSHA(t, prior) {
			t.Fatalf("r4 is byte-identical to %s; the accessor_field fix is not in it", prior)
		}
	}
}
