package main

// TARGET-AWARE Dart directive resolution.
//
// A base contract must list only libraries that can actually compile for the target. Deciding that
// requires following imports/exports — and Dart's conditional import makes the naive reading wrong:
//
//	import 'client_stub.dart'
//	    if (dart.library.js_interop) 'browser_client.dart'
//	    if (dart.library.io) 'io_client.dart';
//
// Collecting every quoted URI in that declaration taints the importer with `browser_client.dart`, which
// on iOS is a branch the compiler never selects. That is how `package:http` — and then anything
// importing it, including Soroq's own runtime package — got excluded from an iOS contract for a web
// branch that is not compiled on iOS. The fix is to resolve the directive the way the compiler does:
// evaluate the conditions against the TARGET and follow exactly one URI.
//
// WHY A DIRECTIVE PARSER RATHER THAN A FULL DART FRONT END. Resolution needs the import/export grammar
// and nothing else — no scopes, no types, no constant evaluation. So this parses that grammar
// properly: comments and string literals are removed by a scanner (so a `dart:js_interop` mentioned in
// a comment or inside a string cannot be mistaken for a directive), then each directive is parsed into
// a default URI plus an ordered list of (condition, uri) configurations, and resolved against a target
// environment. It is not quoted-string scraping, and its behaviour is pinned by tests including
// branch-ordering, multiline, re-export, cycle, comment and package-renaming cases. Using the real
// analyser would mean shipping and invoking a Dart process purely to read directives; if that becomes
// necessary for other reasons, resolveDirective is the single seam to swap.

import (
	"fmt"
	"path"
	"strings"
)

// targetEnvironment answers `dart.library.<name>` for a build target. Values follow the Dart SDK's
// per-platform library availability; only the libraries that actually gate real-world conditional
// imports need entries, and anything unknown is FALSE (a conditional branch is taken only when its
// condition is known-true, which is also how the compiler behaves for an unavailable library).
type targetEnvironment struct {
	name      string
	available map[string]bool
}

// iosTargetEnvironment is the mobile/native environment: dart:io and dart:ffi exist, every web-only
// library does not.
func iosTargetEnvironment() targetEnvironment {
	return targetEnvironment{
		name: "ios",
		available: map[string]bool{
			"dart.library.async":             true,
			"dart.library.collection":        true,
			"dart.library.convert":           true,
			"dart.library.core":              true,
			"dart.library.developer":         true,
			"dart.library.ffi":               true,
			"dart.library.io":                true,
			"dart.library.isolate":           true,
			"dart.library.math":              true,
			"dart.library.typed_data":        true,
			"dart.library.ui":                true,
			"dart.library.html":              false,
			"dart.library.indexed_db":        false,
			"dart.library.js":                false,
			"dart.library.js_interop":        false,
			"dart.library.js_interop_unsafe": false,
			"dart.library.js_util":           false,
			"dart.library.web_audio":         false,
			"dart.library.web_gl":            false,
		},
	}
}

// unavailableDartLibrary reports whether a `dart:` URI names a library this target does not have.
// Importing one is a hard compile failure ("Dart library 'dart:js_interop' is not available on this
// platform"), so a library that does it cannot be in the contract.
func (t targetEnvironment) unavailableDartLibrary(uri string) bool {
	if !strings.HasPrefix(uri, "dart:") {
		return false
	}
	name := "dart.library." + strings.TrimPrefix(uri, "dart:")
	avail, known := t.available[name]
	return known && !avail
}

// dartDirective is one parsed import/export.
type dartDirective struct {
	defaultURI string
	configs    []dartDirectiveConfig // in source order; first true condition wins
}

type dartDirectiveConfig struct {
	condition string // e.g. "dart.library.io"
	uri       string
}

// resolve returns the single URI the compiler would select for this target.
func (d dartDirective) resolve(t targetEnvironment) string {
	for _, c := range d.configs {
		if t.available[c.condition] {
			return c.uri
		}
	}
	return d.defaultURI
}

// stripCommentsAndStrings blanks comments and string literal CONTENTS, keeping the quote characters and
// the byte length. Blanking the contents is what stops `const s = "import 'dart:js_util';";` from
// parsing as a directive; keeping the length means offsets into the result still index the ORIGINAL
// source, which is where the real directive URIs are read from.
func stripCommentsAndStrings(src string) string {
	out := []byte(src)
	const (
		code = iota
		lineComment
		blockComment
		single
		double
	)
	state := code
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(out) && out[i+1] == '/':
				state = lineComment
				out[i], out[i+1] = ' ', ' '
				i++
			case c == '/' && i+1 < len(out) && out[i+1] == '*':
				state = blockComment
				out[i], out[i+1] = ' ', ' '
				i++
			case c == '\'':
				state = single
			case c == '"':
				state = double
			}
		case lineComment:
			if c == '\n' {
				state = code
			} else {
				out[i] = ' '
			}
		case blockComment:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				state = code
			} else if c != '\n' {
				out[i] = ' '
			}
		case single:
			if c == '\\' && i+1 < len(out) {
				out[i], out[i+1] = ' ', ' '
				i++
			} else if c == '\'' {
				state = code
			} else {
				out[i] = ' '
			}
		case double:
			if c == '\\' && i+1 < len(out) {
				out[i], out[i+1] = ' ', ' '
				i++
			} else if c == '"' {
				state = code
			} else {
				out[i] = ' '
			}
		}
	}
	return string(out)
}

// parseDartDirectives extracts every import/export declaration from Dart source, with its conditional
// configurations in source order. Comments and unrelated literals are removed first.
func parseDartDirectives(src string) []dartDirective {
	clean := stripCommentsAndStrings(src)
	if len(clean) != len(src) {
		return nil // defensive: offsets must stay aligned with the original source
	}
	var out []dartDirective

	for i := 0; i < len(clean); {
		if kw := directiveKeywordAt(clean, i); kw == "" {
			i++
			continue
		}
		end := strings.IndexByte(clean[i:], ';')
		if end < 0 {
			break
		}
		// Bounds come from the blanked copy (so a literal cannot masquerade as a directive); the URIs
		// themselves are read from the ORIGINAL source at the same offsets.
		stmt := src[i : i+end]
		i += end + 1

		uris := directiveURIs(stmt)
		if len(uris) == 0 {
			continue
		}
		d := dartDirective{defaultURI: uris[0]}
		// Each `if (<cond>) '<uri>'` contributes one configuration, in order.
		rest := stmt
		for u := 1; u < len(uris); u++ {
			cond := conditionBefore(rest, uris[u])
			if cond == "" {
				continue
			}
			d.configs = append(d.configs, dartDirectiveConfig{condition: cond, uri: uris[u]})
		}
		out = append(out, d)
	}
	return out
}

// directiveKeywordAt reports whether an `import`/`export` keyword starts at i on a token boundary.
func directiveKeywordAt(s string, i int) string {
	for _, kw := range []string{"import", "export"} {
		if !strings.HasPrefix(s[i:], kw) {
			continue
		}
		if i > 0 && isDartIdentChar(s[i-1]) {
			continue // part of a longer identifier, e.g. `reexport`
		}
		j := i + len(kw)
		if j < len(s) && isDartIdentChar(s[j]) {
			continue
		}
		return kw
	}
	return ""
}

func isDartIdentChar(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// directiveURIs returns the quoted URIs of a directive statement, in source order.
func directiveURIs(stmt string) []string {
	var out []string
	for i := 0; i < len(stmt); i++ {
		q := stmt[i]
		if q != '\'' && q != '"' {
			continue
		}
		j := strings.IndexByte(stmt[i+1:], q)
		if j < 0 {
			break
		}
		out = append(out, stmt[i+1:i+1+j])
		i += j + 1
	}
	return out
}

// conditionBefore returns the `dart.library.x` condition of the `if (...)` immediately preceding uri.
func conditionBefore(stmt, uri string) string {
	idx := strings.Index(stmt, uri)
	if idx < 0 {
		return ""
	}
	head := stmt[:idx]
	open := strings.LastIndex(head, "if")
	if open < 0 {
		return ""
	}
	lp := strings.Index(head[open:], "(")
	rp := strings.Index(head[open:], ")")
	if lp < 0 || rp < 0 || rp < lp {
		return ""
	}
	cond := strings.TrimSpace(head[open+lp+1 : open+rp])
	// `if (dart.library.io)` and `if (dart.library.io == 'true')` mean the same thing.
	if eq := strings.Index(cond, "=="); eq >= 0 {
		cond = strings.TrimSpace(cond[:eq])
	}
	return cond
}

// resolveDirective returns the library URI a directive selects for the target, resolved to an absolute
// `package:` URI when it is relative to the importing library. ok is false when the directive names
// something outside the library set (a dart: URI, or a package we do not track).
func resolveDirective(fromURI string, d dartDirective, t targetEnvironment) (uri string, isDart bool, ok bool) {
	sel := d.resolve(t)
	if sel == "" {
		return "", false, false
	}
	if strings.HasPrefix(sel, "dart:") {
		return sel, true, true
	}
	if strings.HasPrefix(sel, "package:") {
		return sel, false, true
	}
	pkg, rest, split := splitPackageURI(fromURI)
	if !split {
		return "", false, false
	}
	if !strings.HasSuffix(sel, ".dart") {
		return "", false, false
	}
	return "package:" + pkg + "/" + path.Clean(path.Join(path.Dir(rest), sel)), false, true
}

// describeTargetExclusion renders a stable, human-readable reason for excluding a library, so a
// narrowed contract is never silent about why.
func describeTargetExclusion(target, uri, cause string) string {
	return fmt.Sprintf("[%s] %s excluded: %s", target, uri, cause)
}
