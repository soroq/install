package main

// STRICT CARRIED-LIBRARY VALIDATION.
//
// Recording a carried-library mapping is not the same as validating it. The first version accepted an
// entry if some recorded entry happened to share its module_library string, which leaves several ways
// for a forged artifact to point the runtime somewhere it was never verified:
//
//   - the MAIN uri returned early, so it was never checked to be inside its own graph namespace;
//   - a carried uri only had to appear in the list, not to be well-formed or canonical;
//   - nothing tied a carried entry back to the module source tree that was actually hashed;
//   - nothing stopped one package's identity being redirected into a DIFFERENT package's library.
//
// The last one matters most: base_identity says which base declaration is being replaced, and
// module_library says where the replacement lives. If those may disagree about the package, a patch
// can silently redirect package A's function to package B's code while every hash still verifies.

import (
	"fmt"
	"sort"
	"strings"
)

const freehandURIPrefix = "soroq-freehand:///import/prefix/"

// canonicalModulePath rejects anything that is not a plain, relative, forward-slashed path.
func canonicalModulePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty module path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("module path %q is absolute", p)
	}
	if strings.Contains(p, `\`) {
		return fmt.Errorf("module path %q contains a backslash", p)
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return fmt.Errorf("module path %q has an empty segment", p)
		case ".", "..":
			return fmt.Errorf("module path %q contains a %q segment", p, seg)
		}
	}
	return nil
}

func lowerHex64(s string) bool { return sha256HexRe.MatchString(s) }

// validateCarriedLibraries proves the carried mapping is an exact, canonical bijection and that it
// agrees with the module source tree the artifact hashed.
//
// treeSHAByPath is the canonical module_source_tree (path -> sha). The carried map must be a SUBSET of
// it with identical hashes: a carried library the tree never hashed is a library nothing verified.
func validateCarriedLibraries(graphDigest, mainLibrary string, carried []freehandCarriedLibrary, treeSHAByPath map[string]string) error {
	if !lowerHex64(graphDigest) {
		return fmt.Errorf("module_graph_digest %q is not lowercase 64-hex", graphDigest)
	}
	// The MAIN uri is checked exactly, not merely accepted. It returned early before, so a manifest
	// could declare a main library from another namespace and nothing objected.
	wantMain := freehandURIPrefix + graphDigest + "/soroq_freehand_module.dart"
	if mainLibrary != wantMain {
		return fmt.Errorf("module_library %q is not the canonical main URI for this graph (%q)",
			mainLibrary, wantMain)
	}

	seenPkg := map[string]bool{}
	seenPath := map[string]bool{}
	seenURI := map[string]bool{}
	var paths []string

	for i, cl := range carried {
		if strings.TrimSpace(cl.PackageURI) == "" {
			return fmt.Errorf("carried library %d has an empty package_uri", i)
		}
		if !strings.HasPrefix(cl.PackageURI, "package:") {
			return fmt.Errorf("carried library %q package_uri is not a package: URI", cl.PackageURI)
		}
		if err := canonicalModulePath(cl.ModulePath); err != nil {
			return fmt.Errorf("carried library %q: %w", cl.PackageURI, err)
		}
		if !lowerHex64(cl.SHA256) {
			return fmt.Errorf("carried library %q sha256 %q is not lowercase 64-hex", cl.PackageURI, cl.SHA256)
		}
		wantURI := freehandURIPrefix + graphDigest + "/" + cl.ModulePath
		if cl.ModuleLibrary != wantURI {
			return fmt.Errorf("carried library %q module_library %q is not the canonical URI for its "+
				"module path in this graph (%q)", cl.PackageURI, cl.ModuleLibrary, wantURI)
		}
		if seenPkg[cl.PackageURI] {
			return fmt.Errorf("carried library package_uri %q appears more than once", cl.PackageURI)
		}
		if seenPath[cl.ModulePath] {
			return fmt.Errorf("carried library module_path %q appears more than once", cl.ModulePath)
		}
		if seenURI[cl.ModuleLibrary] {
			return fmt.Errorf("carried library module_library %q appears more than once", cl.ModuleLibrary)
		}
		seenPkg[cl.PackageURI], seenPath[cl.ModulePath], seenURI[cl.ModuleLibrary] = true, true, true
		paths = append(paths, cl.ModulePath)

		// The carried entry must be one of the files the artifact actually hashed, with the same hash.
		if treeSHAByPath != nil {
			treeSHA, ok := treeSHAByPath[cl.ModulePath]
			if !ok {
				return fmt.Errorf("carried library %q names module path %q, which is absent from the "+
					"module source tree; nothing hashed that file", cl.PackageURI, cl.ModulePath)
			}
			if !strings.EqualFold(treeSHA, cl.SHA256) {
				return fmt.Errorf("carried library %q sha %s disagrees with the module source tree (%s)",
					cl.PackageURI, short(cl.SHA256), short(treeSHA))
			}
		}
	}
	if !sort.StringsAreSorted(paths) {
		return fmt.Errorf("carried libraries are not sorted by module path; the mapping must be canonical")
	}
	return nil
}

// validateABIPackageAgreement proves no ABI entry redirects one package's identity into a DIFFERENT
// package's carried library.
//
// base_identity is `libUri::class::vmName`, so its package URI is the library part. For an entry that
// targets a carried library, that library's package_uri must be the same library. Without this an
// artifact could route package A's declaration to package B's code and every hash would still verify.
func validateABIPackageAgreement(entries []freehandReplacementEntry, mainLibrary string, carried []freehandCarriedLibrary) error {
	byURI := map[string]freehandCarriedLibrary{}
	for _, cl := range carried {
		byURI[cl.ModuleLibrary] = cl
	}
	for _, e := range entries {
		if e.ModuleLibrary == mainLibrary {
			continue // the main module legitimately hosts extracted declarations from anywhere
		}
		cl, ok := byURI[e.ModuleLibrary]
		if !ok {
			return fmt.Errorf("replacement_abi entry %s names module_library %q, which is not a recorded "+
				"carried library", e.BaseIdentity, e.ModuleLibrary)
		}
		sep := strings.LastIndex(e.BaseIdentity, "::")
		if sep <= 0 {
			return fmt.Errorf("replacement_abi entry has a malformed base_identity %q", e.BaseIdentity)
		}
		head := e.BaseIdentity[:sep]
		if s := strings.LastIndex(head, "::"); s > 0 {
			head = head[:s]
		}
		if head != cl.PackageURI {
			return fmt.Errorf("replacement_abi entry %s targets carried library %q (package %q), but its "+
				"base identity belongs to %q; a patch must not redirect one package's declaration into "+
				"another package's library", e.BaseIdentity, cl.ModulePath, cl.PackageURI, head)
		}
	}
	return nil
}
