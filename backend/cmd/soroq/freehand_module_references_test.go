package main

import (
	"os"
	"strings"
	"testing"
)

func TestExtractModuleReferencesSeparatesModuleFromBase(t *testing.T) {
	blob := []byte("3CBD\x00\x00soroq-freehand:///abc/_pkg/matrix_core/matrix_core.dart" +
		"dart:core_StringBase_interpolatedart:collectionLinkedListaddpackage:app/values.dart")
	candidates := []string{"dart:core", "dart:collection", "dart:io", "package:app/values.dart", "soroq-freehand:///abc/_pkg/matrix_core/matrix_core.dart"}
	refs := extractModuleReferences(blob, candidates)
	for _, want := range []string{"dart:core", "dart:collection", "package:app/values.dart"} {
		if !contains(refs.BaseLibraries, want) {
			t.Errorf("base library %q not extracted; got %v", want, refs.BaseLibraries)
		}
	}
	for _, lib := range refs.BaseLibraries {
		if strings.HasPrefix(lib, moduleLibraryScheme) {
			t.Errorf("module-internal library %q reported as a base reference", lib)
		}
	}
	for _, want := range []string{"_StringBase", "LinkedList"} {
		if !refs.namesIdentifier(want) {
			t.Errorf("identifier %q not found in the string pool", want)
		}
	}
	// A library the module does NOT reference must not be reported.
	if contains(refs.BaseLibraries, "dart:io") {
		t.Error("reported a library the module never names")
	}
}

func TestLooksLikeClassName(t *testing.T) {
	for _, id := range []string{"LinkedList", "_StringBase", "Object", "_GrowableList"} {
		if !looksLikeClassName(id) {
			t.Errorf("%q should look like a class name", id)
		}
	}
	for _, id := range []string{"toString", "_interpolate", "add", "get", "_"} {
		if looksLikeClassName(id) {
			t.Errorf("%q should NOT look like a class name", id)
		}
	}
}

// THE REFUTATION, PINNED AS A TEST.
//
// Module `52030e79` loaded and executed on a physical iPhone against base `b100368f`, and its string pool
// names `_StringBase` and `_GrowableList`. Any future gate that refuses a module merely for naming a
// private SDK implementation class would refuse this one, so this test exists to make that regression
// impossible to reintroduce silently.
func TestNamingPrivateSDKClassIsNotEvidenceOfFailure(t *testing.T) {
	const modulePath = "../../../handoff/freehand/evidence/dependency-ota/ios-widened-sequence/retention-profile/device-proven-module.bytecode"
	blob, err := os.ReadFile(modulePath)
	if err != nil {
		t.Skipf("evidence module not present: %v", err)
	}
	if !containsModuleMagic(blob) {
		t.Fatalf("%s is not a dart2bytecode container", modulePath)
	}
	refs := extractModuleReferences(blob, []string{"dart:core", "dart:collection"})
	if !refs.namesIdentifier("_StringBase") || !refs.namesIdentifier("_GrowableList") {
		t.Fatal("wrong evidence file: the device-proven module should name both private SDK classes")
	}
	// It ran on real hardware. Naming these classes therefore carries no verdict, and nothing in this
	// package may treat it as one.
	t.Log("device-proven module names _StringBase and _GrowableList and executed on a physical iPhone")
}
