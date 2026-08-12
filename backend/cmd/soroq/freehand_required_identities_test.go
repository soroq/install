package main

import (
	"strings"
	"testing"
)

func gateSurface() FreehandBaseSurface {
	return buildFreehandBaseSurface(testContract(), testRetained)
}

func TestRequiredIdentitiesPassWhenAllRetained(t *testing.T) {
	set := newRetainedIdentitySet(testRetained)
	err := assertRequiredBaseIdentitiesRetained(
		[]string{"dart:core::Object::toString", "package:app/main.dart::::main"}, set, gateSurface())
	if err != nil {
		t.Fatalf("all-retained requirement must pass: %v", err)
	}
	if err := assertRequiredBaseIdentitiesRetained(nil, set, gateSurface()); err != nil {
		t.Fatalf("empty requirement must pass: %v", err)
	}
}

// Accessor kinds are DISTINCT functions: LookupFunctionAllowPrivate takes a MemberKind, and real profiles
// contain `LinkedList::add` and `LinkedList::get:add` as separate entries. Collapsing them would let a
// required getter match a retained method and ship the crash the gate exists to prevent.
func TestRequiredIdentitiesDoesNotCollapseAccessorKinds(t *testing.T) {
	set := newRetainedIdentitySet([]string{"dart:collection::LinkedList::add"})
	if set.has("dart:collection::LinkedList::get:add") {
		t.Fatal("a retained method satisfied a required GETTER of the same name")
	}
	if set.has("dart:collection::LinkedList::set:add") {
		t.Fatal("a retained method satisfied a required SETTER of the same name")
	}
	err := assertRequiredBaseIdentitiesRetained([]string{"dart:collection::LinkedList::get:add"}, set, gateSurface())
	if err == nil {
		t.Fatal("required getter accepted against a retained method of the same name")
	}
	// ...but the class is not reported as empty, since it does retain something.
	if !strings.Contains(err.Error(), "retains 1 other function") {
		t.Errorf("refusal should say the class retains other functions; got:\n%s", err)
	}
}

func TestRequiredIdentitiesDedupesAndSorts(t *testing.T) {
	set := newRetainedIdentitySet(testRetained)
	err := assertRequiredBaseIdentitiesRetained([]string{
		"z:lib::C::b", "a:lib::C::a", "z:lib::C::b", "  ", "",
	}, set, gateSurface())
	if err == nil {
		t.Fatal("expected refusal")
	}
	msg := err.Error()
	if strings.Count(msg, "z:lib::C::b") != 1 {
		t.Error("duplicate requirement listed twice")
	}
	if strings.Index(msg, "a:lib::C::a") > strings.Index(msg, "z:lib::C::b") {
		t.Error("missing identities are not sorted; refusals must be deterministic")
	}
	if !strings.Contains(msg, "requires 2 base declaration(s)") {
		t.Errorf("blank/duplicate entries were counted; got:\n%s", msg)
	}
}

func TestRequiredIdentitiesCapsButReportsTotal(t *testing.T) {
	set := newRetainedIdentitySet(testRetained)
	var many []string
	for _, c := range "abcdefghijklmnopqrst" {
		many = append(many, "pkg:x/y.dart::C::"+string(c))
	}
	err := assertRequiredBaseIdentitiesRetained(many, set, gateSurface())
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), "and 8 more") {
		t.Errorf("truncation must disclose how many were omitted; got:\n%s", err)
	}
	if !strings.Contains(err.Error(), "requires 20 base declaration(s)") {
		t.Errorf("total must be reported even when the list is capped; got:\n%s", err)
	}
}
