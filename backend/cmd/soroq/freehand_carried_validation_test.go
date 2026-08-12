package main

// STRICT CARRIED-LIBRARY VALIDATION — rejection tests.
//
// Every case here is a FULLY REBOUND tamper in the sense that matters: nothing relies on a hash
// mismatch. The mapping is internally self-consistent and still wrong, so only semantic validation can
// reject it. The previous traversal "test" merely logged whichever way it went, which proves nothing.

import (
	"strings"
	"testing"
)

const vGraph = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func vMain() string { return freehandURIPrefix + vGraph + "/soroq_freehand_module.dart" }

func vCarried(pkg, path string) freehandCarriedLibrary {
	return freehandCarriedLibrary{
		PackageURI:    pkg,
		ModulePath:    path,
		ModuleLibrary: freehandURIPrefix + vGraph + "/" + path,
		SHA256:        strings.Repeat("b", 64),
	}
}

func vTree(cls ...freehandCarriedLibrary) map[string]string {
	m := map[string]string{}
	for _, c := range cls {
		m[c.ModulePath] = c.SHA256
	}
	return m
}

func TestValidCarriedMappingIsAccepted(t *testing.T) {
	a := vCarried("package:a/a.dart", "_pkg/a/a.dart")
	b := vCarried("package:b/b.dart", "_pkg/b/b.dart")
	if err := validateCarriedLibraries(vGraph, vMain(), []freehandCarriedLibrary{a, b}, vTree(a, b)); err != nil {
		t.Fatalf("a canonical mapping must be accepted: %v", err)
	}
}

// The MAIN uri used to return early, so a manifest could declare a main library from another namespace
// and nothing objected.
func TestMainLibraryMustBeCanonicalForItsGraph(t *testing.T) {
	for name, main := range map[string]string{
		"foreign namespace": freehandURIPrefix + strings.Repeat("c", 64) + "/soroq_freehand_module.dart",
		"wrong basename":    freehandURIPrefix + vGraph + "/other.dart",
		"no prefix":         "soroq.freehand.module",
		"empty":             "",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCarriedLibraries(vGraph, main, nil, nil); err == nil {
				t.Fatalf("main library %q was accepted", main)
			}
		})
	}
}

// TRAVERSAL AND MALFORMED PATHS — an actual rejection test, not a log.
func TestMalformedCarriedPathsAreRejected(t *testing.T) {
	for name, path := range map[string]string{
		"traversal":       "../../etc/passwd",
		"absolute":        "/etc/passwd",
		"dot segment":     "_pkg/./a.dart",
		"empty segment":   "_pkg//a.dart",
		"backslash":       `_pkg\a\a.dart`,
		"empty":           "",
		"trailing parent": "_pkg/a/..",
	} {
		t.Run(name, func(t *testing.T) {
			cl := freehandCarriedLibrary{
				PackageURI:    "package:a/a.dart",
				ModulePath:    path,
				ModuleLibrary: freehandURIPrefix + vGraph + "/" + path,
				SHA256:        strings.Repeat("b", 64),
			}
			err := validateCarriedLibraries(vGraph, vMain(), []freehandCarriedLibrary{cl}, nil)
			if err == nil {
				t.Fatalf("module path %q was ACCEPTED; traversal and malformed paths must be refused", path)
			}
		})
	}
}

func TestCarriedURIMustMatchItsOwnModulePath(t *testing.T) {
	cl := vCarried("package:a/a.dart", "_pkg/a/a.dart")
	cl.ModuleLibrary = freehandURIPrefix + vGraph + "/_pkg/b/b.dart" // points elsewhere
	if err := validateCarriedLibraries(vGraph, vMain(), []freehandCarriedLibrary{cl}, nil); err == nil {
		t.Fatal("a carried URI inconsistent with its own module_path was accepted")
	}
}

func TestDuplicatesInAnyDimensionAreRejected(t *testing.T) {
	a := vCarried("package:a/a.dart", "_pkg/a/a.dart")
	cases := map[string][]freehandCarriedLibrary{
		"duplicate package_uri":    {a, vCarried("package:a/a.dart", "_pkg/z/z.dart")},
		"duplicate module_path":    {a, {PackageURI: "package:b/b.dart", ModulePath: a.ModulePath, ModuleLibrary: a.ModuleLibrary, SHA256: a.SHA256}},
		"duplicate module_library": {a, {PackageURI: "package:b/b.dart", ModulePath: "_pkg/b/b.dart", ModuleLibrary: a.ModuleLibrary, SHA256: a.SHA256}},
	}
	for name, cls := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateCarriedLibraries(vGraph, vMain(), cls, nil); err == nil {
				t.Fatalf("%s was accepted; the mapping must be a bijection", name)
			}
		})
	}
}

func TestCarriedMappingMustAgreeWithTheHashedSourceTree(t *testing.T) {
	a := vCarried("package:a/a.dart", "_pkg/a/a.dart")

	t.Run("absent from the tree", func(t *testing.T) {
		if err := validateCarriedLibraries(vGraph, vMain(), []freehandCarriedLibrary{a}, map[string]string{}); err == nil {
			t.Fatal("a carried library that the source tree never hashed was accepted")
		}
	})
	t.Run("sha disagrees with the tree", func(t *testing.T) {
		tree := map[string]string{a.ModulePath: strings.Repeat("d", 64)}
		if err := validateCarriedLibraries(vGraph, vMain(), []freehandCarriedLibrary{a}, tree); err == nil {
			t.Fatal("a carried library whose sha disagrees with the hashed tree was accepted")
		}
	})
}

func TestUnsortedCarriedMappingIsRejected(t *testing.T) {
	a := vCarried("package:a/a.dart", "_pkg/a/a.dart")
	b := vCarried("package:b/b.dart", "_pkg/b/b.dart")
	if err := validateCarriedLibraries(vGraph, vMain(), []freehandCarriedLibrary{b, a}, vTree(a, b)); err == nil {
		t.Fatal("an unsorted mapping was accepted; the same set must not produce two orderings")
	}
}

func TestNonLowercaseHashesAreRejected(t *testing.T) {
	a := vCarried("package:a/a.dart", "_pkg/a/a.dart")
	a.SHA256 = strings.ToUpper(a.SHA256)
	if err := validateCarriedLibraries(vGraph, vMain(), []freehandCarriedLibrary{a}, nil); err == nil {
		t.Fatal("an uppercase sha was accepted")
	}
	if err := validateCarriedLibraries(strings.ToUpper(vGraph), vMain(), nil, nil); err == nil {
		t.Fatal("an uppercase graph digest was accepted")
	}
}

// THE MOST DANGEROUS SHAPE: every hash verifies, the mapping is canonical, and the patch still
// redirects one package's declaration into a DIFFERENT package's carried library.
func TestABIMayNotRedirectOnePackageIntoAnotherPackagesLibrary(t *testing.T) {
	a := vCarried("package:a/a.dart", "_pkg/a/a.dart")
	b := vCarried("package:b/b.dart", "_pkg/b/b.dart")

	good := []freehandReplacementEntry{{
		BaseIdentity: "package:a/a.dart::::fn", StableIdentity: "k1",
		ModuleLibrary: a.ModuleLibrary, ModuleMember: "fn", Kind: "function",
	}}
	if err := validateABIPackageAgreement(good, vMain(), []freehandCarriedLibrary{a, b}); err != nil {
		t.Fatalf("a matching package/library pair must be accepted: %v", err)
	}

	crossed := []freehandReplacementEntry{{
		BaseIdentity: "package:a/a.dart::::fn", StableIdentity: "k1",
		ModuleLibrary: b.ModuleLibrary, // package a's identity -> package b's library
		ModuleMember:  "fn", Kind: "function",
	}}
	err := validateABIPackageAgreement(crossed, vMain(), []freehandCarriedLibrary{a, b})
	if err == nil {
		t.Fatal("an entry redirecting package a's declaration into package b's library was ACCEPTED; " +
			"every hash would still verify, so only this check can catch it")
	}
	if !strings.Contains(err.Error(), "another package's library") {
		t.Errorf("refusal must name the cross-package redirect; got: %v", err)
	}
}

// An entry naming the MAIN module is always allowed: extracted declarations legitimately live there
// regardless of which package they came from.
func TestMainModuleEntriesAreNotPackageConstrained(t *testing.T) {
	a := vCarried("package:a/a.dart", "_pkg/a/a.dart")
	entries := []freehandReplacementEntry{{
		BaseIdentity: "package:anything/x.dart::::fn", StableIdentity: "k",
		ModuleLibrary: vMain(), ModuleMember: "fn", Kind: "function",
	}}
	if err := validateABIPackageAgreement(entries, vMain(), []freehandCarriedLibrary{a}); err != nil {
		t.Fatalf("a main-module entry must not be package-constrained: %v", err)
	}
}
