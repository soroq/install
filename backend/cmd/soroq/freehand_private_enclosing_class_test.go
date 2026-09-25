package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- private ENCLOSING CLASS identity, capability-gated -------------------------------------------
//
// The shape this is about is the most idiomatic one in Flutter:
//
//	class _HomePageState extends State<HomePage> {
//	  @override
//	  Widget build(BuildContext context) { ... }
//	}
//
// The VM mangles the private class name, so gen_snapshot compares `_HomePageState@57413802::build`
// against a manifest carrying `_HomePageState::build` and reports hit=0. An engine that canonicalizes
// the class name before comparing can match it; whether a given base's engine does is recorded per-base
// as private_enclosing_class_identity_v1, which is what these tests pin.

// THE REGRESSION FIXTURE, kept verbatim from the reproduced defect. A private enclosing class must be
// classified as such -- separately from a private member, which no capability lifts.
func TestPrivateEnclosingClassIsClassifiedApartFromPrivateMembers(t *testing.T) {
	privateClass, privateMember := freehandPrivateIdentitySplit([]string{
		"package:ioslane/main.dart::_CleanHomeState::build",
		"package:ioslane/main.dart::_HomePageState::build",
		"package:ioslane/main.dart::CleanApp::_privateMethod",
		"package:ioslane/main.dart::::_privateTopLevel",
		"package:ioslane/main.dart::CleanApp::build",
	})
	wantClass := []string{
		"package:ioslane/main.dart::_CleanHomeState::build",
		"package:ioslane/main.dart::_HomePageState::build",
	}
	wantMember := []string{
		"package:ioslane/main.dart::CleanApp::_privateMethod",
		"package:ioslane/main.dart::::_privateTopLevel",
	}
	if strings.Join(privateClass, "|") != strings.Join(wantClass, "|") {
		t.Fatalf("private enclosing classes = %v, want %v", privateClass, wantClass)
	}
	if strings.Join(privateMember, "|") != strings.Join(wantMember, "|") {
		t.Fatalf("private members = %v, want %v", privateMember, wantMember)
	}
}

// PUBLIC IDENTITY BEHAVIOUR IS UNCHANGED. Nothing about this capability touches a public class, and a
// batch of them must classify as neither kind of private.
func TestPublicIdentitiesAreUnchangedByTheCapability(t *testing.T) {
	public := []string{
		"package:ioslane/main.dart::::headline",
		"package:ioslane/main.dart::CleanApp::build",
		"package:ioslane/main.dart::CleanHome::createState",
		"package:ioslane/main.dart::CleanApp::get:label",
	}
	privateClass, privateMember := freehandPrivateIdentitySplit(public)
	if len(privateClass) != 0 || len(privateMember) != 0 {
		t.Fatalf("public identities were classified private: class=%v member=%v", privateClass, privateMember)
	}
	if got := freehandPrivateIdentities(public); len(got) != 0 {
		t.Fatalf("public identities were refused: %v", got)
	}
}

// A PRIVATE MEMBER OF A PRIVATE CLASS is a private member. The capability lifts the class, never the
// member, and reporting it as a class case would make a refusal message claim a capability could fix it.
func TestPrivateMemberOfPrivateClassStaysAMemberRefusal(t *testing.T) {
	privateClass, privateMember := freehandPrivateIdentitySplit([]string{
		"package:ioslane/main.dart::_HomePageState::_helper",
	})
	if len(privateClass) != 0 {
		t.Fatalf("a private member was reported as a private-class case: %v", privateClass)
	}
	if len(privateMember) != 1 {
		t.Fatalf("private member not refused: %v", privateMember)
	}
}

// An accessor wears its name behind a `get:`/`set:` prefix. Without looking past the prefix, `get:_value`
// reads as public and the capability would appear to lift a private member.
func TestPrivateAccessorsAreMemberRefusalsDespiteTheirPrefix(t *testing.T) {
	for _, id := range []string{
		"package:ioslane/main.dart::_HomePageState::get:_value",
		"package:ioslane/main.dart::CleanApp::set:_value",
	} {
		privateClass, privateMember := freehandPrivateIdentitySplit([]string{id})
		if len(privateMember) != 1 || len(privateClass) != 0 {
			t.Fatalf("%s: class=%v member=%v, want it refused as a private member", id, privateClass, privateMember)
		}
	}
}

// --- the capability record itself ----------------------------------------------------------------

// AN OLD TOOLCHAIN MUST KEEP REFUSING. A base whose engine declared nothing carries no identity
// capability, and the accessor must answer false rather than defaulting to permissive.
func TestOldToolchainDeclaresNoIdentityCapability(t *testing.T) {
	legacy := legacyDefaultRedirectCapabilities("engine-abc123", "no engine bundle declared anything")
	if legacy.hasIdentityCapability(freehandPrivateEnclosingClassCapability) {
		t.Fatal("a legacy-default base claimed a capability no engine declared")
	}
	if len(legacy.IdentityCapabilities) != 0 {
		t.Fatalf("legacy default carries identity capabilities: %v", legacy.IdentityCapabilities)
	}
}

// A NIL RECORD IS NOT A CAPABILITY. Reading "no record" as permissive is the exact failure the whole
// capability guard exists to prevent.
func TestNilCapabilityRecordRefuses(t *testing.T) {
	var none *FreehandRedirectCapabilities
	if none.hasIdentityCapability(freehandPrivateEnclosingClassCapability) {
		t.Fatal("a nil capability record answered true")
	}
}

// A FALSE/ABSENT DECLARATION REFUSES, a present one accepts. Both halves, because a check that only
// proves acceptance would pass with the guard deleted.
func TestIdentityCapabilityAcceptanceAndRefusal(t *testing.T) {
	with := &FreehandRedirectCapabilities{
		IdentityCapabilities: []string{freehandPrivateEnclosingClassCapability},
	}
	if !with.hasIdentityCapability(freehandPrivateEnclosingClassCapability) {
		t.Fatal("a declared capability was not honoured")
	}
	without := &FreehandRedirectCapabilities{IdentityCapabilities: []string{}}
	if without.hasIdentityCapability(freehandPrivateEnclosingClassCapability) {
		t.Fatal("an empty capability list answered true")
	}
	other := &FreehandRedirectCapabilities{IdentityCapabilities: []string{"some_other_capability_v9"}}
	if other.hasIdentityCapability(freehandPrivateEnclosingClassCapability) {
		t.Fatal("a different capability satisfied this one")
	}
}

// --- what an engine bundle is allowed to declare --------------------------------------------------

func TestEngineDeclarationCarriesIdentityCapabilities(t *testing.T) {
	raw := json.RawMessage(`{"honoured_kinds":["method"],"identity_capabilities":["private_enclosing_class_identity_v1"]}`)
	kinds, caps, err := decodeEngineCapabilityDeclaration(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(kinds) != 1 || kinds[0] != "method" {
		t.Fatalf("kinds = %v", kinds)
	}
	if len(caps) != 1 || caps[0] != freehandPrivateEnclosingClassCapability {
		t.Fatalf("caps = %v", caps)
	}
}

// An engine that predates identity capabilities declares only honoured_kinds. That must decode, and
// must yield NO capability -- this is the old-toolchain path.
func TestEngineDeclarationWithoutIdentityCapabilitiesYieldsNone(t *testing.T) {
	kinds, caps, err := decodeEngineCapabilityDeclaration(json.RawMessage(`{"honoured_kinds":["method"]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(kinds) != 1 {
		t.Fatalf("kinds = %v", kinds)
	}
	if len(caps) != 0 {
		t.Fatalf("an engine that declared no identity capabilities produced %v", caps)
	}
}

// AN UNKNOWN CAPABILITY NAME FAILS CLOSED. Carrying it through as an opaque string is how a future
// guard matches something no engine demonstrated.
func TestUnknownIdentityCapabilityIsRefused(t *testing.T) {
	_, _, err := decodeEngineCapabilityDeclaration(
		json.RawMessage(`{"honoured_kinds":["method"],"identity_capabilities":["private_everything_v99"]}`))
	if err == nil {
		t.Fatal("an unrecognized identity capability was accepted")
	}
	if !strings.Contains(err.Error(), "private_everything_v99") {
		t.Fatalf("the refusal does not name the offending capability: %v", err)
	}
}

func TestDuplicateIdentityCapabilityIsRefused(t *testing.T) {
	_, _, err := decodeEngineCapabilityDeclaration(json.RawMessage(
		`{"honoured_kinds":["method"],"identity_capabilities":["private_enclosing_class_identity_v1","private_enclosing_class_identity_v1"]}`))
	if err == nil {
		t.Fatal("a duplicated identity capability was accepted")
	}
}
