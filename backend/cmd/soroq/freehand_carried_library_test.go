package main

// PER-ENTRY MODULE LIBRARY RESOLUTION.
//
// A replacement-ABI entry used to be required to equal the manifest's single module_library. That
// assumption is what forced a changed declaration inside a CARRIED package library to be dropped --
// which is how a pure-Dart dependency UPGRADE ended up crashing synthesis with a null assertion.
//
// An entry may now name the main module OR exactly one strictly-recorded carried library. These tests
// pin what "strictly" means, because the loose version of this rule is exactly how a forged entry
// would point the runtime at a library it never verified.

import (
	"strings"
	"testing"
)

const testGraphDigest = "1111111111111111111111111111111111111111111111111111111111111111"

func manifestWithCarried(carried ...freehandCarriedLibrary) *freehandModuleManifest {
	return &freehandModuleManifest{
		ModuleLibrary:     "soroq-freehand:///import/prefix/" + testGraphDigest + "/soroq_freehand_module.dart",
		ModuleGraphDigest: testGraphDigest,
		CarriedLibraries:  carried,
	}
}

func carriedLib(pkgURI, path string) freehandCarriedLibrary {
	return freehandCarriedLibrary{
		PackageURI:    pkgURI,
		ModulePath:    path,
		ModuleLibrary: "soroq-freehand:///import/prefix/" + testGraphDigest + "/" + path,
		SHA256:        strings.Repeat("a", 64),
	}
}

func TestEntryMayNameTheMainModuleOrARecordedCarriedLibrary(t *testing.T) {
	cl := carriedLib("package:upgradable/upgradable.dart", "_pkg/upgradable/upgradable.dart")
	m := manifestWithCarried(cl)

	if err := m.resolveEntryLibrary(m.ModuleLibrary, "main"); err != nil {
		t.Errorf("the main module library must resolve: %v", err)
	}
	if err := m.resolveEntryLibrary(cl.ModuleLibrary, "carried"); err != nil {
		t.Errorf("a recorded carried library must resolve: %v", err)
	}
}

// An UNRECORDED library URI cannot be verified against any source, so it must never resolve -- even
// when it is correctly shaped and inside the right namespace.
func TestUnrecordedLibraryURIIsRefused(t *testing.T) {
	m := manifestWithCarried(carriedLib("package:a/a.dart", "_pkg/a/a.dart"))
	ghost := "soroq-freehand:///import/prefix/" + testGraphDigest + "/_pkg/ghost/ghost.dart"
	err := m.resolveEntryLibrary(ghost, "entry")
	if err == nil {
		t.Fatal("an unrecorded library URI was accepted")
	}
	if !strings.Contains(err.Error(), "neither the main module nor any recorded carried library") {
		t.Errorf("refusal must say the URI is unrecorded; got: %v", err)
	}
}

// FOREIGN NAMESPACE. A correctly-shaped URI from a DIFFERENT module graph must be refused: at runtime
// it would resolve against whatever happened to be loaded under that name.
func TestForeignNamespaceURIIsRefused(t *testing.T) {
	cl := carriedLib("package:a/a.dart", "_pkg/a/a.dart")
	m := manifestWithCarried(cl)
	foreign := strings.Replace(cl.ModuleLibrary, testGraphDigest, strings.Repeat("2", 64), 1)
	err := m.resolveEntryLibrary(foreign, "entry")
	if err == nil {
		t.Fatal("a URI from a foreign module graph was accepted")
	}
	if !strings.Contains(err.Error(), "outside this artifact's module-graph namespace") {
		t.Errorf("refusal must name the namespace mismatch; got: %v", err)
	}
}

// The mapping must be ONE-TO-ONE. Two carried entries claiming the same URI make "which source is
// this?" ambiguous, and an ambiguous mapping cannot be verified.
func TestDuplicateCarriedLibraryMappingIsRefused(t *testing.T) {
	cl := carriedLib("package:a/a.dart", "_pkg/a/a.dart")
	dup := carriedLib("package:b/b.dart", "_pkg/a/a.dart") // different package, same URI
	m := manifestWithCarried(cl, dup)
	err := m.resolveEntryLibrary(cl.ModuleLibrary, "entry")
	if err == nil {
		t.Fatal("a duplicate carried-library mapping was accepted")
	}
	if !strings.Contains(err.Error(), "one-to-one") {
		t.Errorf("refusal must name the ambiguity; got: %v", err)
	}
}

// OLD-ARTIFACT REFUSAL. An artifact that predates per-library URIs cannot express carried libraries,
// so a carried reference in one must be refused rather than guessed compatible.
func TestArtifactWithoutGraphDigestRefusesCarriedReferences(t *testing.T) {
	m := &freehandModuleManifest{ModuleLibrary: "soroq.freehand.module"} // no graph digest
	if err := m.resolveEntryLibrary("soroq.freehand.module", "main"); err != nil {
		t.Errorf("its own main module must still resolve: %v", err)
	}
	err := m.resolveEntryLibrary("soroq-freehand:///import/prefix/x/_pkg/a/a.dart", "carried")
	if err == nil {
		t.Fatal("an old artifact accepted a carried-library reference")
	}
	if !strings.Contains(err.Error(), "rebuild") {
		t.Errorf("refusal must tell the operator to rebuild; got: %v", err)
	}
}

func TestEmptyModuleLibraryIsRefused(t *testing.T) {
	m := manifestWithCarried()
	if err := m.resolveEntryLibrary("", "entry"); err == nil {
		t.Fatal("an empty module_library was accepted")
	}
}

// NOTE: traversal/malformed-path rejection is asserted for real in
// freehand_carried_validation_test.go (TestMalformedCarriedPathsAreRejected). The stub that used to
// live here only LOGGED whichever way it went, which proved nothing.
