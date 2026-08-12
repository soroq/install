package main

// Dynamic-module contract diagnostics.
//
// Eligibility for an automatic dependency OTA has three dimensions, not two:
//
//   native code                -> the dependency descriptor's capability classifier
//   undeliverable assets       -> the descriptor + the build-output comparison
//   the dynamic-module contract -> THIS file
//
// The third one is a property of what the carried code DOES TO BASE TYPES, not of which package it came
// from: a dynamic module may not extend, implement, mix in, or override base/SDK members unless the
// dynamic interface permits it. A package whose reachable code does that cannot be carried today.
//
// Without this translation the violation surfaces either as a raw front-end diagnostic pointing into a
// pub-cache file, or — if the module is produced without validation — as a device-side load failure
// (`runtime/vm/object.cc: expected: is_finalized()`). Both are useless to the person who just added a
// dependency. This turns it into a refusal that names the package, the library and the member.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// dynamicModuleViolationRe matches the front-end's dynamic-module contract diagnostics. The message
// wording is stable across the three forms it emits (extend/implement/mix-in, override, invoke).
var dynamicModuleViolationRe = regexp.MustCompile(`Cannot (extend, implement or mix-in class|override member|invoke constructor|invoke member|use member) '([^']+)'[^\n]*in a dynamic module`)

// carriedFileRe recovers the carried package + library from a module-local source path
// (`_pkg/<package>/<rest>.dart`), which is where a carried dependency's code lives in the module tree.
var carriedFileRe = regexp.MustCompile(`_pkg/([^/]+)/([^\s:]+\.dart)`)

// contractViolation is one distinct (package, library, member) the carried code is not allowed to touch.
type contractViolation struct {
	Package string
	Library string
	Kind    string
	Member  string
}

// parseDynamicModuleViolations extracts the contract violations from a dart2bytecode failure. It returns
// nil when the failure was something else, so the caller can fall back to the raw compiler output rather
// than mis-attributing an unrelated error to the dependency.
func parseDynamicModuleViolations(compilerOutput string) []contractViolation {
	lines := strings.Split(compilerOutput, "\n")
	seen := map[string]contractViolation{}
	for i, line := range lines {
		m := dynamicModuleViolationRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		kind, member := m[1], m[2]
		// The file:line prefix is on the same diagnostic line; when the offending code is carried
		// dependency source it is a module-local `_pkg/<package>/...` path. Some diagnostics point at an
		// SDK patch file instead (the base member being overridden), in which case look back for the
		// nearest carried-file reference so the refusal still names the responsible package.
		pkg, lib := "", ""
		for j := i; j >= 0 && j > i-6; j-- {
			if f := carriedFileRe.FindStringSubmatch(lines[j]); f != nil {
				pkg, lib = f[1], f[2]
				break
			}
		}
		v := contractViolation{Package: pkg, Library: lib, Kind: kind, Member: member}
		seen[fmt.Sprintf("%s|%s|%s|%s", pkg, lib, kind, member)] = v
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]contractViolation, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Package != out[j].Package {
			return out[i].Package < out[j].Package
		}
		if out[i].Member != out[j].Member {
			return out[i].Member < out[j].Member
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// explainDynamicModuleViolations renders the actionable refusal. It deliberately does NOT suggest editing
// the dependency: the remedy is either a new store release (which ships the package normally) or widening
// the dynamic interface in the toolchain — one generic change that unlocks every package in this class.
func explainDynamicModuleViolations(vs []contractViolation) string {
	var b strings.Builder
	pkgs := map[string]bool{}
	for _, v := range vs {
		if v.Package != "" {
			pkgs[v.Package] = true
		}
	}
	named := make([]string, 0, len(pkgs))
	for p := range pkgs {
		named = append(named, p)
	}
	sort.Strings(named)

	if len(named) > 0 {
		fmt.Fprintf(&b, "dependency change is not deliverable via a code-only OTA patch: %s reach base/SDK types in a way the dynamic-module contract does not allow.\n",
			"package(s) ["+strings.Join(named, ", ")+"]")
	} else {
		b.WriteString("dependency change is not deliverable via a code-only OTA patch: the carried code reaches base/SDK types in a way the dynamic-module contract does not allow.\n")
	}
	for _, v := range vs {
		where := v.Package
		if v.Library != "" {
			where = v.Package + " (" + v.Library + ")"
		}
		if where == "" {
			where = "carried dependency code"
		}
		fmt.Fprintf(&b, "  - %s: cannot %s '%s'\n", where, v.Kind, v.Member)
	}
	b.WriteString("  A dynamic module may not extend, implement, mix in or override base/SDK members unless the\n")
	b.WriteString("  dynamic interface marks them as permitted. This is a property of what the code does to base\n")
	b.WriteString("  types, not of the package's size — a small package doing the same thing is refused identically.\n")
	b.WriteString("  Remedies: ship this dependency in a new App Store/Play Store release, or widen the toolchain's\n")
	b.WriteString("  dynamic interface to permit these members (one change that unlocks every package in this class).")
	return b.String()
}
