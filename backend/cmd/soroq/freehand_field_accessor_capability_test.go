package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A COMMITTED REDIRECT IS NOT A FUNCTIONAL PASS.
//
// The first real device session on soroq.ios_engine.6b182d2c_5a2a6a42.private_state.r1 resolved the
// private identity, loaded the module, ran the transition, and reported
//
//	SOROQ_OTA transition.result version=1 committed=1 required=1
//	SOROQ_OTA active version=1 state=patched
//
// and then threw on the first frame:
//
//	NoSuchMethodError: Class '_HomePageState' has no instance getter 'builds'
//
// Module bytecode reaches a field by SELECTOR, and the base AOT build had dropped the implicit accessors
// because the only access was inside the very method the patch replaced. Whether a base survives that is
// a property of the engine that built it, so it is recorded as a per-base capability. These tests pin
// the closed set, the fail-closed default, and the fact that the old engine cannot claim the new
// behaviour by accident.

func decodeCaps(t *testing.T, body string) ([]string, []string, error) {
	t.Helper()
	return decodeEngineCapabilityDeclaration(json.RawMessage(body))
}

func TestPublishedR2RetentionCapabilityRemainsReadableButDoesNotClaimDispatch(t *testing.T) {
	_, caps, err := decodeCaps(t, `{"honoured_kinds":["method"],
		"identity_capabilities":["public_instance_field_accessor_retention_v1"]}`)
	if err != nil {
		t.Fatalf("the immutable r2 capability must remain readable: %v", err)
	}
	if len(caps) != 1 || caps[0] != freehandPublicFieldAccessorRetentionCapability {
		t.Fatalf("capability not carried through: %v", caps)
	}
	rec := &FreehandRedirectCapabilities{IdentityCapabilities: caps}
	if rec.hasIdentityCapability(freehandPublicFieldAccessorDynamicDispatchCapability) {
		t.Fatal("r2 retention was upgraded into runtime selector-dispatch proof")
	}
}

// The r3 engine preserves both historical capabilities and adds the device-driven dispatch fix.
func TestR3IdentityCapabilitiesCanBeDeclaredTogether(t *testing.T) {
	_, caps, err := decodeCaps(t, `{"honoured_kinds":["method","getter","setter"],
		"identity_capabilities":["private_enclosing_class_identity_v1","public_instance_field_accessor_retention_v1","public_instance_field_accessor_dynamic_dispatch_v1"]}`)
	if err != nil {
		t.Fatalf("the r3 capability set was refused: %v", err)
	}
	if len(caps) != 3 {
		t.Fatalf("expected all three capabilities, got %v", caps)
	}
	rec := &FreehandRedirectCapabilities{IdentityCapabilities: caps}
	if !rec.hasIdentityCapability(freehandPublicFieldAccessorDynamicDispatchCapability) {
		t.Fatalf("r3 dynamic-dispatch capability was not carried through: %v", caps)
	}
}

// THE FAIL-CLOSED DEFAULT. r1 declares only the private-class capability, and must not be readable as
// supporting field access -- that is exactly the claim the device disproved.
func TestR1CapabilitySetDoesNotClaimFieldAccess(t *testing.T) {
	_, caps, err := decodeCaps(t, `{"honoured_kinds":["method"],
		"identity_capabilities":["private_enclosing_class_identity_v1"]}`)
	if err != nil {
		t.Fatal(err)
	}
	rec := &FreehandRedirectCapabilities{IdentityCapabilities: caps}
	if rec.hasIdentityCapability(freehandPublicFieldAccessorRetentionCapability) {
		t.Fatal("an r1 base claims field-accessor retention it does not have")
	}
	if !rec.hasIdentityCapability(freehandPrivateEnclosingClassCapability) {
		t.Fatal("the r1 base lost the capability it does have")
	}
}

// An engine that declares nothing claims nothing.
func TestAbsentIdentityCapabilitiesClaimNothing(t *testing.T) {
	_, caps, err := decodeCaps(t, `{"honoured_kinds":["method"]}`)
	if err != nil {
		t.Fatal(err)
	}
	rec := &FreehandRedirectCapabilities{IdentityCapabilities: caps}
	for _, c := range []string{
		freehandPublicFieldAccessorRetentionCapability,
		freehandPublicFieldAccessorDynamicDispatchCapability,
		freehandPrivateEnclosingClassCapability,
	} {
		if rec.hasIdentityCapability(c) {
			t.Fatalf("an engine declaring no identity capabilities claims %q", c)
		}
	}
}

// The set stays CLOSED. A near-miss name is the realistic mistake -- a typo in a canonical json that
// would otherwise ride through as an opaque string some future guard matches by accident.
func TestUnknownOrNearMissCapabilityIsRefused(t *testing.T) {
	for _, name := range []string{
		"public_instance_field_accessor_retention",           // no version
		"public_instance_field_accessor_retention_v2",        // not yet a thing
		"public_instance_field_accessor_dynamic_dispatch",    // no version
		"public_instance_field_accessor_dynamic_dispatch_v2", // not yet a thing
		"public_field_accessor_retention_v1",                 // dropped a word
		"private_field_accessor_retention_v1",                // the widening we refuse
		"",
	} {
		body := `{"honoured_kinds":["method"],"identity_capabilities":["` + name + `"]}`
		if _, _, err := decodeCaps(t, body); err == nil {
			t.Fatalf("capability %q was accepted; the set is not closed", name)
		}
	}
}

func TestDuplicateFieldAccessorCapabilityIsRefused(t *testing.T) {
	body := `{"honoured_kinds":["method"],"identity_capabilities":[
		"public_instance_field_accessor_retention_v1","public_instance_field_accessor_retention_v1"]}`
	_, _, err := decodeCaps(t, body)
	if err == nil {
		t.Fatal("a duplicated capability was accepted")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Fatalf("the refusal does not say what was wrong: %v", err)
	}
}
