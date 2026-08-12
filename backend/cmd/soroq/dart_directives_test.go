package main

// TARGET-AWARE classification — the point is what must SURVIVE, not only what must go.
//
// The previous classifier collected every quoted URI in a conditional import, so on iOS
//
//	import 'client_stub.dart'
//	    if (dart.library.js_interop) 'browser_client.dart'
//	    if (dart.library.io) 'io_client.dart';
//
// tainted the importer with a branch iOS never compiles. That removed package:http from the contract,
// and then everything importing http — including Soroq's own runtime package. Broad exclusion is not an
// acceptable fix; resolving the directive the way the compiler does is.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDart(t *testing.T, dir, rel, body string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The real package:http shape: a stub default, a web branch and an io branch.
const httpClientDirective = `
import 'client_stub.dart'
    if (dart.library.js_interop) 'browser_client.dart'
    if (dart.library.io) 'io_client.dart';
class Client {}
`

func TestConditionalImportResolvesToTheTargetBranch(t *testing.T) {
	ds := parseDartDirectives(httpClientDirective)
	if len(ds) != 1 {
		t.Fatalf("expected 1 directive, got %d", len(ds))
	}
	if got := ds[0].resolve(iosTargetEnvironment()); got != "io_client.dart" {
		t.Errorf("iOS selected %q, want io_client.dart (the js_interop branch must NOT win)", got)
	}
}

// Branch ORDER matters: the first true condition wins, not the last.
func TestFirstTrueConditionWins(t *testing.T) {
	ds := parseDartDirectives(`
import 'stub.dart'
    if (dart.library.io) 'first_io.dart'
    if (dart.library.async) 'second_async.dart';
`)
	if got := ds[0].resolve(iosTargetEnvironment()); got != "first_io.dart" {
		t.Errorf("selected %q, want first_io.dart", got)
	}
}

func TestNoMatchingConditionFallsBackToTheDefault(t *testing.T) {
	ds := parseDartDirectives(`import 'stub.dart' if (dart.library.js_interop) 'browser.dart';`)
	if got := ds[0].resolve(iosTargetEnvironment()); got != "stub.dart" {
		t.Errorf("selected %q, want the default stub.dart", got)
	}
}

// A dart: URI in a comment or a string literal is not a directive.
func TestCommentsAndStringsCannotFakeADirective(t *testing.T) {
	src := `
// import 'dart:js_interop';
/* import 'dart:html'; */
const s = "import 'dart:js_util';";
import 'dart:async';
`
	ds := parseDartDirectives(src)
	if len(ds) != 1 || ds[0].defaultURI != "dart:async" {
		t.Fatalf("expected only the real dart:async directive, got %+v", ds)
	}
}

// THE REGRESSION THIS EXISTS FOR: an http-shaped package keeps its target-compatible libraries, its
// browser library is excluded, and a package importing http is NOT tainted.
func TestHttpKeepsIoClientAndDoesNotTaintImporters(t *testing.T) {
	dir := t.TempDir()
	libPaths := map[string]string{
		"package:http/http.dart":               writeDart(t, dir, "http/http.dart", "export 'src/client.dart';\n"),
		"package:http/src/client.dart":         writeDart(t, dir, "http/src/client.dart", httpClientDirective),
		"package:http/src/client_stub.dart":    writeDart(t, dir, "http/src/client_stub.dart", "class Stub {}\n"),
		"package:http/src/io_client.dart":      writeDart(t, dir, "http/src/io_client.dart", "import 'dart:io';\nclass IOClient {}\n"),
		"package:http/src/browser_client.dart": writeDart(t, dir, "http/src/browser_client.dart", "import 'package:web/web.dart';\n"),
		"package:http/browser_client.dart":     writeDart(t, dir, "http/browser_client.dart", "export 'src/browser_client.dart';\n"),
		"package:web/web.dart":                 writeDart(t, dir, "web/web.dart", "export 'src/dom.dart';\n"),
		"package:web/src/dom.dart":             writeDart(t, dir, "web/src/dom.dart", "import 'dart:js_interop';\n"),
		// Soroq's runtime package imports http, exactly like the real one.
		"package:soroq_flutter/soroq_flutter.dart": writeDart(t, dir, "sf/soroq_flutter.dart",
			"import 'package:http/http.dart';\nclass Ota {}\n"),
		"package:app/main.dart": writeDart(t, dir, "app/main.dart", "import 'package:soroq_flutter/soroq_flutter.dart';\n"),
	}

	bad, reasons := targetIneligibleLibraries(libPaths, iosTargetEnvironment())

	mustSurvive := []string{
		"package:http/http.dart",
		"package:http/src/client.dart",
		"package:http/src/io_client.dart",
		"package:soroq_flutter/soroq_flutter.dart",
		"package:app/main.dart",
	}
	for _, u := range mustSurvive {
		if bad[u] {
			t.Errorf("%s was excluded on iOS; a target-compatible library must survive.\nreasons: %s",
				u, strings.Join(reasons, "\n  "))
		}
	}
	mustGo := []string{
		"package:web/src/dom.dart",
		"package:web/web.dart",
		"package:http/src/browser_client.dart",
		"package:http/browser_client.dart",
	}
	for _, u := range mustGo {
		if !bad[u] {
			t.Errorf("%s was NOT excluded; it cannot compile for iOS", u)
		}
	}
	if len(reasons) == 0 {
		t.Error("exclusions were recorded with no reasons; a narrowed contract must never be silent")
	}
}

// Renaming every package must not change a single decision.
func TestClassificationIsIndependentOfPackageNames(t *testing.T) {
	dir := t.TempDir()
	libPaths := map[string]string{
		"package:zz1/api.dart":             writeDart(t, dir, "a/api.dart", "export 'src/impl.dart';\n"),
		"package:zz1/src/impl.dart":        writeDart(t, dir, "a/src/impl.dart", httpClientDirective),
		"package:zz1/src/client_stub.dart": writeDart(t, dir, "a/src/client_stub.dart", "class S {}\n"),
		"package:zz1/src/io_client.dart":   writeDart(t, dir, "a/src/io_client.dart", "import 'dart:io';\n"),
		"package:zz1/src/browser_client.dart": writeDart(t, dir, "a/src/browser_client.dart",
			"import 'dart:js_interop';\n"),
		"package:zz2/user.dart": writeDart(t, dir, "b/user.dart", "import 'package:zz1/api.dart';\n"),
	}
	bad, _ := targetIneligibleLibraries(libPaths, iosTargetEnvironment())
	if bad["package:zz1/api.dart"] || bad["package:zz2/user.dart"] {
		t.Error("opaquely-named packages were excluded through an UNSELECTED web branch")
	}
	if !bad["package:zz1/src/browser_client.dart"] {
		t.Error("the genuinely web-only library survived")
	}
}

// FAIL CLOSED: an unreadable library must be excluded WITH a reason, never optimistically kept.
func TestUnreadableSourceFailsClosed(t *testing.T) {
	libPaths := map[string]string{
		"package:a/missing.dart": filepath.Join(t.TempDir(), "nope.dart"),
	}
	bad, reasons := targetIneligibleLibraries(libPaths, iosTargetEnvironment())
	if !bad["package:a/missing.dart"] {
		t.Fatal("an unreadable library was retained; it cannot be shown to compile for the target")
	}
	if len(reasons) == 0 || !strings.Contains(strings.Join(reasons, " "), "unreadable") {
		t.Errorf("exclusion reason must say the source was unreadable; got %v", reasons)
	}
}

// A cycle must terminate, and must not spuriously taint portable libraries.
func TestCyclesTerminateAndDoNotOverTaint(t *testing.T) {
	dir := t.TempDir()
	libPaths := map[string]string{
		"package:c/a.dart": writeDart(t, dir, "c/a.dart", "import 'b.dart';\n"),
		"package:c/b.dart": writeDart(t, dir, "c/b.dart", "import 'a.dart';\nimport 'dart:async';\n"),
	}
	bad, _ := targetIneligibleLibraries(libPaths, iosTargetEnvironment())
	if bad["package:c/a.dart"] || bad["package:c/b.dart"] {
		t.Error("a portable cycle was excluded")
	}

	libPaths["package:c/b.dart"] = writeDart(t, dir, "c/b.dart", "import 'a.dart';\nimport 'dart:html';\n")
	bad, _ = targetIneligibleLibraries(libPaths, iosTargetEnvironment())
	if !bad["package:c/a.dart"] || !bad["package:c/b.dart"] {
		t.Error("a cycle reaching a web-only dart: library was not fully excluded")
	}
}

// Re-exports propagate, since a re-exporting library compiles its target.
func TestReExportOfAnIneligibleLibraryPropagates(t *testing.T) {
	dir := t.TempDir()
	libPaths := map[string]string{
		"package:p/pub.dart":   writeDart(t, dir, "p/pub.dart", "export 'src/w.dart';\n"),
		"package:p/src/w.dart": writeDart(t, dir, "p/src/w.dart", "import 'dart:js_interop';\n"),
		"package:p/ok.dart":    writeDart(t, dir, "p/ok.dart", "import 'dart:convert';\n"),
	}
	bad, _ := targetIneligibleLibraries(libPaths, iosTargetEnvironment())
	if !bad["package:p/pub.dart"] {
		t.Error("a library re-exporting a web-only library was not excluded")
	}
	if bad["package:p/ok.dart"] {
		t.Error("an unrelated portable library in the same package was excluded")
	}
}

// `export` is a directive too, and `reexport` is not.
func TestDirectiveKeywordBoundaries(t *testing.T) {
	ds := parseDartDirectives("export 'a.dart';\nvar reexport = 'b.dart';\nimport 'c.dart';\n")
	var got []string
	for _, d := range ds {
		got = append(got, d.defaultURI)
	}
	if len(got) != 2 || got[0] != "a.dart" || got[1] != "c.dart" {
		t.Errorf("parsed %v, want [a.dart c.dart] (a variable named reexport is not a directive)", got)
	}
}

// RAW AND TRIPLE-QUOTED STRINGS. These are where a naive scanner produces FALSE directives: a raw
// string ignores escapes, and a triple-quoted string may contain newlines, quotes and whole
// import-looking lines. A false directive here would taint a portable library and silently narrow the
// contract, which is the failure mode this whole file exists to prevent.
func TestRawAndTripleQuotedStringsDoNotProduceDirectives(t *testing.T) {
	cases := map[string]string{
		"triple single": "const s = '''\nimport 'dart:js_interop';\nexport 'dart:html';\n''';\nimport 'dart:async';\n",
		"triple double": "const s = \"\"\"\nimport 'dart:js_interop';\n\"\"\";\nimport 'dart:async';\n",
		"raw single":    "const s = r'import \\'dart:js_interop\\';';\nimport 'dart:async';\n",
		"raw double":    "const s = r\"import 'dart:html';\";\nimport 'dart:async';\n",
		"adjacent":      "const s = 'import ' 'dart:js_util' ';';\nimport 'dart:async';\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, d := range parseDartDirectives(src) {
				got = append(got, d.defaultURI)
			}
			for _, u := range got {
				if strings.HasPrefix(u, "dart:") && u != "dart:async" {
					t.Errorf("a string literal produced a FALSE directive %q (parsed %v)", u, got)
				}
			}
		})
	}
}

// The same shapes must not cause a portable library to be excluded.
func TestStringLiteralsDoNotTaintAPortableLibrary(t *testing.T) {
	dir := t.TempDir()
	libPaths := map[string]string{
		"package:s/a.dart": writeDart(t, dir, "s/a.dart",
			"const doc = '''\nimport 'dart:js_interop';\n''';\nimport 'dart:convert';\n"),
	}
	bad, reasons := targetIneligibleLibraries(libPaths, iosTargetEnvironment())
	if bad["package:s/a.dart"] {
		t.Errorf("a portable library was excluded because of a string literal: %v", reasons)
	}
}
