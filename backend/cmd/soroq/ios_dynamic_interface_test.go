package main

import (
	"strings"
	"testing"
)

func TestContract_IsDeterministicAndSorted(t *testing.T) {
	a := buildFreehandBaseContract(nil, nil, []string{"package:app/b.dart", "package:app/a.dart"}, []string{"package:dep/z.dart"})
	b := buildFreehandBaseContract(nil, nil, []string{"package:app/a.dart", "package:app/b.dart"}, []string{"package:dep/z.dart"})
	if a.Digest != b.Digest {
		t.Fatalf("input order must not change the contract: %s != %s", a.Digest, b.Digest)
	}
	if renderFreehandContractYAML(a) != renderFreehandContractYAML(b) {
		t.Fatal("rendered YAML must be byte-identical for the same inputs")
	}
	for i := 1; i < len(a.Libraries); i++ {
		if a.Libraries[i-1] >= a.Libraries[i] {
			t.Fatalf("libraries must be sorted and deduped: %v", a.Libraries)
		}
	}
}

// The sections are evidence-gated. can-be-overridden segfaults the VM at load and dynamically-callable
// crashes the front-end validator, so emitting either would trade a clear refusal for a crash.
func TestContract_OnlyEvidenceBackedSections(t *testing.T) {
	y := renderFreehandContractYAML(buildFreehandBaseContract(nil, nil, nil, nil))
	for _, want := range []string{"callable:", "extendable:", "can-be-used-as-type:"} {
		if !strings.Contains(y, want) {
			t.Fatalf("contract must emit %q", want)
		}
	}
	for _, forbidden := range []string{"can-be-overridden:", "dynamically-callable:"} {
		if strings.Contains(y, forbidden) {
			t.Fatalf("contract must NOT emit %q (unproven/crashing upstream)", forbidden)
		}
	}
}

// extendable is what lifts the ceiling: without it a dependency extending a base/SDK class is refused.
func TestContract_ExposesSdkAndFlutterSurfaceForExtension(t *testing.T) {
	c := buildFreehandBaseContract(nil, nil, nil, nil)
	got := map[string]bool{}
	for _, l := range c.Libraries {
		got[l] = true
	}
	for _, want := range []string{"dart:collection", "dart:core", "dart:async", "package:flutter/widgets.dart", "package:flutter/foundation.dart"} {
		if !got[want] {
			t.Fatalf("contract must expose %q so ordinary Dart-only packages can extend base types", want)
		}
	}
	y := renderFreehandContractYAML(c)
	ext := y[strings.Index(y, "extendable:"):]
	if !strings.Contains(ext[:strings.Index(ext, "can-be-used-as-type:")], "dart:collection") {
		t.Fatal("dart:collection must be extendable (LinkedListEntry case)")
	}
}

func TestContract_AvailabilityNarrowsNeverWidens(t *testing.T) {
	// A pinned platform that only provides dart:core must not yield a contract claiming dart:collection.
	c := buildFreehandBaseContract([]string{"dart:core"}, []string{"dart:ui"}, nil, nil)
	for _, l := range c.Libraries {
		if l == "dart:collection" {
			t.Fatal("contract must not claim a library the pinned platform does not provide")
		}
	}
}

func TestContract_DigestBindsSectionsAndLibraries(t *testing.T) {
	base := buildFreehandBaseContract(nil, nil, []string{"package:app/a.dart"}, nil)
	more := buildFreehandBaseContract(nil, nil, []string{"package:app/a.dart", "package:app/b.dart"}, nil)
	if base.Digest == more.Digest {
		t.Fatal("adding a library must change the contract digest")
	}
	tampered := base
	tampered.Sections = []string{"callable"}
	if freehandContractDigest(tampered) == base.Digest {
		t.Fatal("changing the section set must change the contract digest")
	}
}

func TestContract_NoPackageSpecificEntries(t *testing.T) {
	y := renderFreehandContractYAML(buildFreehandBaseContract(nil, nil, nil, nil))
	for _, banned := range []string{"riverpod", "state_notifier", "collection/", "provider", "bloc"} {
		if strings.Contains(y, banned) {
			t.Fatalf("contract must contain no package-specific entry, found %q", banned)
		}
	}
}
