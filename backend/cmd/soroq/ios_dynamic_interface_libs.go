package main

// Derivation of the application + eligible-dependency library URIs the base contract exposes.
//
// This is where "generic" is enforced: the set comes from the project's own resolved runtime dependency
// graph and its lib/ tree. There is no package allowlist, no soroq.yaml declaration, and no per-package
// condition — a dependency is exposed because the classifier already judged it Dart-only, which is the
// same judgement that decides whether it is patchable at all.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"soroq/backend/internal/depgraph"
)

// contractProjectLibraries returns (application library URIs, eligible Dart-only dependency library URIs)
// for the project. Failures are non-fatal: a contract missing the project's own libraries is narrower,
// never wider, so it can only refuse more — it can never let something through unchecked.
func contractProjectLibraries(projectDir string) (appLibs []string, depLibs []string) {
	g, err := depgraph.Resolve(projectDir)
	if err != nil {
		return nil, nil
	}
	// Prefer the libraries actually present in the PINNED base kernel. Walking lib/ instead exposes
	// libraries that cannot compile for this target at all -- a package's web-only dart:js_interop
	// libraries being the concrete case that broke the build -- and forcing those into the retention
	// contract fails the build outright.
	kernelLibs := pinnedKernelLibraries(projectDir)
	if len(kernelLibs) > 0 {
		eligible := map[string]bool{g.RootPackage: true}
		for _, name := range g.PackageNames() {
			p := g.Packages[name]
			if p.Capability.Eligible && p.Source != depgraph.SourceSDK {
				eligible[name] = true
			}
		}
		for _, u := range kernelLibs {
			pkg, rest, ok := splitPackageURI(u)
			if !ok || !eligible[pkg] {
				continue
			}
			// lib/src is implementation detail: the contract describes public surface, and exposing src
			// would make it depend on a package's internal layout (and retain far more code).
			if strings.HasPrefix(rest, "src/") {
				continue
			}
			if pkg == g.RootPackage {
				appLibs = append(appLibs, u)
			} else {
				depLibs = append(depLibs, u)
			}
		}
		sort.Strings(appLibs)
		sort.Strings(depLibs)
		return appLibs, depLibs
	}
	libPaths := map[string]string{}
	appLibs = dartLibraryURIs(g.RootPackage, filepath.Join(projectDir, "lib"), libPaths)

	seen := map[string]bool{}
	for _, name := range g.PackageNames() {
		p := g.Packages[name]
		// ONLY Dart-only dependencies. A native-plugin or asset-bearing package is not exposed: it is
		// already refused by the descriptor, and exposing it would widen the contract for code that can
		// never be carried anyway.
		if !p.Capability.Eligible || p.Source == depgraph.SourceSDK {
			continue
		}
		root := p.RootDir()
		if root == "" {
			continue
		}
		for _, u := range dartLibraryURIs(name, filepath.Join(root, "lib"), libPaths) {
			if !seen[u] {
				seen[u] = true
				depLibs = append(depLibs, u)
			}
		}
	}
	// TARGET ELIGIBILITY. Drop libraries that cannot compile for this target.
	//
	// Forcing one into the retention contract makes the AOT compiler compile it, and the build dies
	// with "Dart library 'dart:js_interop' is not available on this platform". This fallback runs when
	// no pinned baseline exists yet -- a developer's FIRST release -- which is exactly where a fresh
	// project meets it.
	//
	// Conditional imports are resolved AGAINST THE TARGET, not by collecting every branch. On iOS
	// `import 'stub.dart' if (dart.library.js_interop) 'browser.dart' if (dart.library.io) 'io.dart';`
	// selects io.dart, so the importer is NOT tainted by the browser branch. Collecting all branches is
	// what wrongly excluded package:http and, transitively, Soroq's own runtime package.
	excluded, reasons := targetIneligibleLibraries(libPaths, iosTargetEnvironment())
	for _, r := range reasons {
		fmt.Fprintln(os.Stderr, "soroq contract: "+r)
	}
	appLibs = dropURIs(appLibs, excluded)
	depLibs = dropURIs(depLibs, excluded)

	sort.Strings(depLibs)
	return appLibs, depLibs
}

// dropURIs returns uris with every member of drop removed.
func dropURIs(uris []string, drop map[string]bool) []string {
	out := uris[:0]
	for _, u := range uris {
		if !drop[u] {
			out = append(out, u)
		}
	}
	return out
}

// targetIneligibleLibraries marks every library that cannot compile for the target, following ONLY the
// conditional-import branches the target selects, to a fixed point. It returns the exclusion set plus a
// reason per excluded library, because a silently narrowed contract is indistinguishable from a bug.
//
// FAIL CLOSED on unreadable source. A library whose bytes cannot be read cannot be shown to compile for
// the target, and keeping it risks a build-breaking entry in the contract; it is excluded with the
// reason recorded rather than optimistically retained.
func targetIneligibleLibraries(libPaths map[string]string, t targetEnvironment) (map[string]bool, []string) {
	bad := map[string]bool{}
	reasons := map[string]string{}
	refs := map[string][]string{}

	for uri, p := range libPaths {
		src, err := os.ReadFile(p)
		if err != nil {
			bad[uri] = true
			reasons[uri] = describeTargetExclusion(t.name, uri, "source unreadable ("+err.Error()+")")
			continue
		}
		for _, d := range parseDartDirectives(string(src)) {
			target, isDart, ok := resolveDirective(uri, d, t)
			if !ok {
				continue
			}
			if isDart {
				if t.unavailableDartLibrary(target) {
					bad[uri] = true
					reasons[uri] = describeTargetExclusion(t.name, uri,
						"imports "+target+", which does not exist on this target")
				}
				continue
			}
			if _, known := libPaths[target]; known {
				refs[uri] = append(refs[uri], target)
			}
		}
	}

	// Propagate: a library that selects (for THIS target) an ineligible library is itself ineligible.
	for changed := true; changed; {
		changed = false
		for uri, targets := range refs {
			if bad[uri] {
				continue
			}
			for _, dep := range targets {
				if bad[dep] {
					bad[uri] = true
					reasons[uri] = describeTargetExclusion(t.name, uri, "reaches "+dep)
					changed = true
					break
				}
			}
		}
	}

	out := make([]string, 0, len(reasons))
	for _, uri := range sortedStringKeys(reasons) {
		out = append(out, reasons[uri])
	}
	return bad, out
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// splitPackageURI splits "package:<name>/<rest>" into its parts.
func splitPackageURI(u string) (pkg, rest string, ok bool) {
	if !strings.HasPrefix(u, "package:") {
		return "", "", false
	}
	body := u[len("package:"):]
	i := strings.Index(body, "/")
	if i < 0 {
		return "", "", false
	}
	return body[:i], body[i+1:], true
}

// pinnedKernelLibraries asks the analyzer which libraries the pinned base kernel actually contains. It
// returns nil when there is no pinned base yet (the very first build), in which case the caller falls
// back to the filesystem walk.
func pinnedKernelLibraries(projectDir string) []string {
	relDir, dill := newestBaselineAppDill(projectDir)
	if dill == "" {
		return nil
	}
	_ = relDir
	analyzer := strings.TrimSpace(os.Getenv("SOROQ_FREEHAND_ANALYZER"))
	if analyzer == "" || !fileExists(analyzer) || !fileExists(dill) {
		return nil
	}
	dart := freehandHostDartForContract()
	if dart == "" {
		return nil
	}
	out, err := exec.Command(dart, analyzer, "--list-libraries", dill).Output()
	if err != nil {
		return nil
	}
	var parsed struct {
		Libraries []string `json:"libraries"`
	}
	if json.Unmarshal(out, &parsed) != nil {
		return nil
	}
	return parsed.Libraries
}

// newestBaselineAppDill finds the most recently written baseline's app.dill under .soroq/releases.
func newestBaselineAppDill(projectDir string) (string, string) {
	root := filepath.Join(projectDir, ".soroq", "releases")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", ""
	}
	var bestDir string
	var bestMod int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name(), "app.dill")
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.ModTime().Unix() >= bestMod {
			bestMod = fi.ModTime().Unix()
			bestDir = filepath.Join(root, e.Name())
		}
	}
	if bestDir == "" {
		return "", ""
	}
	return bestDir, filepath.Join(bestDir, "app.dill")
}

// freehandHostDartForContract resolves a host VM able to run the analyzer snapshot.
func freehandHostDartForContract() string {
	if p := strings.TrimSpace(os.Getenv("SOROQ_HOST_DARTVM")); p != "" && fileExists(p) {
		return p
	}
	if bin, err := resolveSoroqFlutterBin(); err == nil {
		if root, err := flutterRootFromBin(bin); err == nil {
			p := filepath.Join(root, "bin", "cache", "dart-sdk", "bin", "dart")
			if fileExists(p) {
				return p
			}
		}
	}
	return ""
}

// dartLibraryURIs enumerates the package: URIs of the public Dart libraries under a package's lib/.
// Files under lib/src/ are implementation detail and are NOT exposed: the dynamic interface describes a
// package's public surface, and exposing src/ would make the contract depend on internal layout.
func dartLibraryURIs(packageName, libDir string, libPaths map[string]string) []string {
	var out []string
	_ = filepath.WalkDir(libDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".dart") {
			return nil //nolint:nilerr // an unreadable subtree narrows the contract, never widens it
		}
		rel, rerr := filepath.Rel(libDir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		uri := "package:" + packageName + "/" + rel
		// Record EVERY library, including lib/src, for reachability analysis. src libraries are not part
		// of the contract, but they are exactly where a public library's real imports live: web.dart is
		// just `export 'src/dom.dart'`, so a closure that cannot see src/ can never discover that
		// web.dart is web-only. Excluding them from libPaths is what made the first filter miss.
		if libPaths != nil {
			libPaths[uri] = p
		}
		if strings.HasPrefix(rel, "src/") {
			// lib/src is implementation detail: the contract describes public surface, and exposing src
			// would tie it to a package's internal layout (and retain far more code).
			return nil
		}
		out = append(out, uri)
		return nil
	})
	sort.Strings(out)
	return out
}
