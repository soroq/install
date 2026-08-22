package main

import "testing"

// PATCHABLE IDENTITY GRAMMAR.
//
// Two entries that Soroq's OWN analyzer emits were rejected here, and each aborted a 5395-entry
// manifest with an error naming something the developer never wrote:
//
//	file:///…/.soroq/generated/soroq_bootstrap.g.dart::::main      (not addressable; must be SKIPPED)
//	package:characters/…::StringCharacters::get:iterator           (a getter; must be VALID)
func TestValidPatchIdentifierAcceptsAccessors(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"iterator", "get:iterator", "set:value", "_private", "$sym", "a1"} {
		if !validPatchIdentifier(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
}

// The prefix allowance must not become a hole: a stray colon anywhere else is still corruption.
func TestValidPatchIdentifierStillRejectsStrayColons(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "get:", "set:", "a:b", "get:a:b", "1leading", "has space", "get::x"} {
		if validPatchIdentifier(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

// ONLY Soroq's own generated files may be skipped. A non-package entry that is NOT generated means
// developer code is being dropped from the patchable surface, and silently skipping it would hide
// exactly the defect worth catching.
func TestOnlyGeneratedNonPackageURIsAreSkippable(t *testing.T) {
	t.Parallel()

	skippable := []string{
		"file:///Users/dev/app/.soroq/generated/soroq_bootstrap.g.dart",
		"file:///w/x/.soroq/generated/soroq_freehand_activator.g.dart",
	}
	for _, uri := range skippable {
		if !isSoroqGeneratedLibraryURI(uri) {
			t.Errorf("%q should be recognised as Soroq-generated", uri)
		}
	}

	notSkippable := []string{
		"file:///Users/dev/app/lib/main.dart",                    // developer source
		"file:///Users/dev/app/.soroq/generated/notes.txt",       // generated dir, not a .g.dart library
		"file:///Users/dev/app/generated/soroq_bootstrap.g.dart", // .g.dart but NOT under .soroq/generated
		"dart:core",
		"package:foo/foo.dart",
	}
	for _, uri := range notSkippable {
		if isSoroqGeneratedLibraryURI(uri) {
			t.Errorf("%q must NOT be skippable", uri)
		}
	}
}
