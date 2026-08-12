package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func testContract() FreehandBaseContract {
	c := FreehandBaseContract{
		Schema:    freehandContractSchema,
		Libraries: []string{"dart:collection", "dart:core", "package:app/main.dart"},
		Sections:  contractSections,
	}
	c.Digest = freehandContractDigest(c)
	return c
}

var testRetained = []string{
	"dart:collection::LinkedList::add",
	"dart:collection::LinkedList::get:add",
	"dart:core::Object::toString",
	"package:app/main.dart::::main",
}

func TestSurfaceBindsBothHalves(t *testing.T) {
	s := buildFreehandBaseSurface(testContract(), testRetained)
	if err := s.Validate(); err != nil {
		t.Fatalf("fresh surface must validate: %v", err)
	}
	if s.RetainedCount != len(testRetained) {
		t.Fatalf("retained count = %d, want %d", s.RetainedCount, len(testRetained))
	}

	// Changing EITHER half must change the surface identity, or the two could drift undetected.
	c2 := testContract()
	c2.Libraries = append(c2.Libraries, "dart:async")
	c2.Digest = freehandContractDigest(c2)
	if buildFreehandBaseSurface(c2, testRetained).SurfaceDigest == s.SurfaceDigest {
		t.Error("widening the contract did not change the surface digest")
	}
	if buildFreehandBaseSurface(testContract(), append(testRetained, "dart:core::int::+")).SurfaceDigest == s.SurfaceDigest {
		t.Error("changing retention did not change the surface digest")
	}
}

func TestSurfaceIsOrderIndependent(t *testing.T) {
	shuffled := []string{testRetained[3], testRetained[1], testRetained[0], testRetained[2]}
	a := buildFreehandBaseSurface(testContract(), testRetained)
	b := buildFreehandBaseSurface(testContract(), shuffled)
	if a.SurfaceDigest != b.SurfaceDigest {
		t.Fatal("surface digest depends on identity order; it must be canonical (the profile's node order is not stable)")
	}
	// Duplicates must not inflate the count either.
	if c := buildFreehandBaseSurface(testContract(), append(append([]string{}, testRetained...), testRetained[0])); c.SurfaceDigest != a.SurfaceDigest {
		t.Fatal("duplicate identities changed the surface digest")
	}
}

// Contract/retention DRIFT: the surface record and the identity list live in separate files, so editing
// either independently must be caught.
func TestSurfaceDetectsRetentionDrift(t *testing.T) {
	s := buildFreehandBaseSurface(testContract(), testRetained)
	if err := s.AssertMatchesIdentities(testRetained); err != nil {
		t.Fatalf("matching identities must pass: %v", err)
	}
	// An identity list that gained an entry.
	err := s.AssertMatchesIdentities(append(append([]string{}, testRetained...), "dart:core::int::+"))
	if err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatalf("added identity not detected: %v", err)
	}
	// Same COUNT, different content — only the digest can catch this.
	swapped := append([]string{}, testRetained...)
	swapped[0] = "dart:collection::LinkedList::remove"
	err = s.AssertMatchesIdentities(swapped)
	if err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("same-count content swap not detected: %v", err)
	}
}

func TestSurfaceDetectsContractDrift(t *testing.T) {
	s := buildFreehandBaseSurface(testContract(), testRetained)
	if err := s.AssertMatchesContract(testContract()); err != nil {
		t.Fatalf("same contract must pass: %v", err)
	}
	c2 := testContract()
	c2.Libraries = append(c2.Libraries, "package:flutter/widgets.dart")
	c2.Digest = freehandContractDigest(c2)
	if err := s.AssertMatchesContract(c2); err == nil {
		t.Fatal("a base built under a different contract was accepted")
	}
}

func TestSurfaceStrictDecodeRejectsTampering(t *testing.T) {
	s := buildFreehandBaseSurface(testContract(), testRetained)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeFreehandBaseSurfaceStrict(raw); err != nil {
		t.Fatalf("round-trip must decode: %v", err)
	}

	for name, mutate := range map[string]func(m map[string]any){
		"retained digest rebound": func(m map[string]any) {
			m["retained_identity_digest"] = strings.Repeat("a", 64)
		},
		"count inflated": func(m map[string]any) { m["retained_identity_count"] = 99999 },
		"contract swapped": func(m map[string]any) {
			m["contract_digest"] = strings.Repeat("b", 64)
		},
		"library injected": func(m map[string]any) {
			m["contract_libraries"] = []any{"dart:collection", "dart:core", "dart:io", "package:app/main.dart"}
		},
		"zero retention": func(m map[string]any) { m["retained_identity_count"] = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			mutate(m)
			b, _ := json.Marshal(m)
			if _, err := DecodeFreehandBaseSurfaceStrict(b); err == nil {
				t.Fatalf("tampered surface (%s) was accepted", name)
			}
		})
	}
}

func TestSurfaceStrictDecodeRejectsUnknownAndTrailing(t *testing.T) {
	s := buildFreehandBaseSurface(testContract(), testRetained)
	raw, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["surprise"] = 1
	b, _ := json.Marshal(m)
	if _, err := DecodeFreehandBaseSurfaceStrict(b); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := DecodeFreehandBaseSurfaceStrict(append(raw, []byte("{}")...)); err == nil {
		t.Error("trailing JSON accepted")
	}
}

func TestSurfaceRejectsUnsortedLibraries(t *testing.T) {
	s := buildFreehandBaseSurface(testContract(), testRetained)
	s.Libraries = []string{"dart:core", "dart:collection", "package:app/main.dart"}
	s.SurfaceDigest = s.computeDigest() // self-consistent but NOT canonical
	if err := s.Validate(); err == nil {
		t.Fatal("unsorted contract libraries accepted; the digest would depend on emission order")
	}
}

func TestIdentityListRoundTrip(t *testing.T) {
	b := renderIdentityList([]string{"b::C::x", "a::B::y", "b::C::x", "  "})
	got := parseIdentityList(b)
	if len(got) != 2 || got[0] != "a::B::y" || got[1] != "b::C::x" {
		t.Fatalf("identity list did not canonicalize: %q", got)
	}
}
