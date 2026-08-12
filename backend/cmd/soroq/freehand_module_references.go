package main

// MODULE REFERENCE EXTRACTION — what does this compiled module need from the base?
//
// The thing that aborts the app is the module's CONSTANT POOL: `BytecodeReaderHelper::ReadConstantPool`
// resolves each referenced base declaration through `Class::LookupFunctionAllowPrivate`, and a miss is a
// `FATAL` (bytecode_reader.cc:1199), i.e. SIGABRT on a user's device. So the gate has to reason about the
// COMPILED module, not the source: an AST walk cannot see the helper calls dart2bytecode synthesizes for
// string interpolation, list literals, super-constructor invocations or tear-offs — and those are exactly
// what crashed us.
//
// PRECISION LIMIT — read this before trusting the output.
//
// The exact route is to parse the module's object table: `pkg/dart2bytecode/lib/object_table.dart`
// provides `ObjectTable.read(BufferedReader)`, and `_MemberHandle.toString()` already renders precisely
// `library::class::member`. That requires reproducing the container's section layout to locate the object
// table, and there is no single `Component.read` to lean on — a real sub-project, and NOT done here.
//
// What IS implemented is a deliberately CONSERVATIVE approximation over the module's string pool, which
// is plain text and contains every library URI, class name and member name the module references. It
// cannot reconstruct which member belongs to which class, so it does not try. Instead it answers a
// narrower question that is sufficient for the failure class we actually hit and cannot produce a false
// PASS for it:
//
//	"does this module name a base class for which the base retains NO functions whatsoever?"
//
// A class with zero retained functions cannot satisfy ANY lookup against it, so naming one is fatal
// regardless of which member is wanted. That covers `_StringBase`, `_GrowableList` and every other
// private SDK implementation class AOT drops entirely.
//
// The tradeoff is deliberate and one-directional: this can over-refuse (a class name appearing in the
// pool only as an unrelated string), never under-refuse for this failure class. Over-refusal costs an
// operator a confusing message; under-refusal ships a crash to a user's phone.

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// moduleLibraryScheme marks the module's OWN libraries. Anything under it is carried inside the module
// and is not a base reference.
const moduleLibraryScheme = "soroq-freehand:"

// printableRunRe extracts maximal runs of printable ASCII from the binary, the same way `strings` does.
var printableRunRe = regexp.MustCompile(`[\x20-\x7E]{3,}`)

// modulePoolText returns the module's string pool as one searchable blob.
//
// The pool is a CONCATENATION with no separators, so it cannot be tokenized: `dart:core` immediately
// followed by the name `int` is indistinguishable from a library called `dart:coreint`, and any regex
// that tries to delimit URIs either over-runs into the next entry or truncates this one. Both failure
// modes are silent, and one of them turns this gate into a no-op.
//
// So we never tokenize. Every question is asked as CONTAINMENT against a candidate we already know the
// spelling of — the base's own library URIs and class names. That is exact for the membership question
// this gate actually asks.
func modulePoolText(bytecode []byte) string {
	var b strings.Builder
	for _, run := range printableRunRe.FindAll(bytecode, -1) {
		b.Write(run)
		b.WriteByte('\x00') // keep runs from fusing into spurious cross-run matches
	}
	return b.String()
}

// ModuleReferences is what a compiled module names, resolved against a known candidate set.
type ModuleReferences struct {
	// BaseLibraries are the non-module library URIs the module references.
	BaseLibraries []string
	// Pool is the raw string-pool text, for containment checks against class names.
	Pool string
}

// namesIdentifier reports whether the module's string pool contains this exact name.
func (r ModuleReferences) namesIdentifier(id string) bool { return strings.Contains(r.Pool, id) }

// extractModuleReferences resolves which of `candidateLibraries` a compiled module references.
// Candidates come from the target base itself (the distinct library URIs of its retained identities), so
// the spellings are exact and no tokenization is involved.
func extractModuleReferences(bytecode []byte, candidateLibraries []string) ModuleReferences {
	pool := modulePoolText(bytecode)
	var libs []string
	for _, lib := range candidateLibraries {
		if lib == "" || strings.HasPrefix(lib, moduleLibraryScheme) {
			continue
		}
		if strings.Contains(pool, lib) {
			libs = append(libs, lib)
		}
	}
	sort.Strings(libs)
	return ModuleReferences{BaseLibraries: libs, Pool: pool}
}

// baseLibrariesOf returns the distinct library URIs appearing in an identity set — the candidate set for
// extractModuleReferences.
func baseLibrariesOf(ids []string) []string {
	seen := map[string]bool{}
	for _, id := range ids {
		if i := strings.Index(id, "::"); i > 0 {
			seen[id[:i]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// looksLikeClassName keeps tokens shaped like Dart type names, including private ones (`_StringBase`).
// Members and locals are lowerCamelCase and are excluded; they are not what this check reasons about.
func looksLikeClassName(id string) bool {
	s := strings.TrimPrefix(id, "_")
	if s == "" {
		return false
	}
	return unicode.IsUpper(rune(s[0]))
}

// REFUTED HYPOTHESIS — deliberately NOT implemented as a gate.
//
// An earlier revision refused any module naming a private SDK implementation class (`_StringBase`,
// `_GrowableList`) whose class retains no functions in the base, on the theory that dart2bytecode lowers
// string interpolation and list literals to calls on them while AOT inlines the same constructs.
//
// The theory is wrong, and the counter-evidence is on real hardware. Base `b100368f` — the
// device-accepted dependency-OTA base — ran module `52030e79`, whose string pool contains BOTH
// `_StringBase` and `_GrowableList`, and it loaded and executed on a physical iPhone:
//
//	SOROQ_STEP5 headline="PKG:PKG-soroq-1! calls=2 box=<9> tag=g5"   (x20)
//
// So naming those classes is not fatal, the check would have refused a patch that demonstrably works,
// and shipping it would have broken every working patch in the name of safety. A gate that refuses
// everything is not fail-closed, it is broken.
//
// The string pool alone cannot decide this: it proves a NAME is present, not that a FUNCTION LOOKUP is
// performed against it. Only the object table distinguishes the two — see the precision note at the top
// of this file. Until that parser exists, the required-identity gate runs on identities the synthesizer
// DECLARES, and no inference is drawn from the string pool.

// containsModuleMagic reports whether the blob is a dart2bytecode container ("3CBD").
func containsModuleMagic(b []byte) bool { return bytes.HasPrefix(b, []byte("3CBD")) }
