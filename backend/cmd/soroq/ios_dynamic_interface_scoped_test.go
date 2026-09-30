package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v1 bases keep their contract byte for byte: the digest a v1 contract gets must be exactly the one the
// pre-v2 formula produced (reproduced verbatim here), and its rendering must not change shape.
func TestScopedContract_V1DigestAndYAMLUnchanged(t *testing.T) {
	c := buildFreehandBaseContract(nil, nil, []string{"package:app/a.dart"}, []string{"package:dep/z.dart"})
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", c.Schema)
	fmt.Fprintf(h, "sections:%s\n", strings.Join(c.Sections, ","))
	for _, l := range c.Libraries {
		fmt.Fprintf(h, "lib:%s\n", l)
	}
	if want := hex.EncodeToString(h.Sum(nil)); c.Digest != want {
		t.Fatalf("v1 digest changed: %s != %s", c.Digest, want)
	}
	if c.Schema != "soroq.freehand.base_contract.v1" || len(c.Entries) != 0 || len(c.ScopedPackages) != 0 {
		t.Fatalf("v1 contract gained v2 state: %+v", c)
	}
	y := renderFreehandContractYAML(c)
	if strings.Contains(y, "class:") || !strings.Contains(y, "  - library: 'package:flutter/material.dart'\n") {
		t.Fatalf("v1 rendering changed:\n%s", y)
	}
	// The v1 contract is serialized into base_surface/records; no new JSON keys may appear for it.
	raw, _ := json.Marshal(c)
	for _, k := range []string{"entries", "scoped_packages"} {
		if strings.Contains(string(raw), `"`+k+`"`) {
			t.Fatalf("v1 contract JSON gained %q: %s", k, raw)
		}
	}
}

func testUsage(domain scopedDomain, entries []ContractEntry, libs []usageLibrary) []byte {
	sdk := append([]string(nil), scopedSDKLibraries...)
	raw, _ := json.Marshal(map[string]any{
		"schema":               interfaceUsageSchema,
		"scoped_sdk_libraries": sdk,
		"scoped_prefixes":      domain.prefixes(),
		"consumer_packages":    []string{"app"},
		"consumer_libraries":   1,
		"entries":              entries,
		"libraries":            libs,
	})
	return raw
}

var testScopedEntries = []ContractEntry{
	{Library: "dart:ui", Kind: "class", Name: "Color"},
	{Library: "dart:ui", Kind: "class", Name: "Offset"},
	{Library: "dart:ui", Kind: "member", Name: "lerpDouble"},
	{Library: "dart:ui", Kind: "extension", Name: "EnumName"},
	{Library: "package:flutter/src/widgets/text.dart", Kind: "class", Name: "Text"},
	{Library: "package:flutter/src/widgets/framework.dart", Kind: "class", Name: "StatelessWidget"},
	{Library: "package:flutter/src/widgets/framework.dart", Kind: "extendable", Name: "StatelessWidget"},
	{Library: "package:flutter/src/foundation/print.dart", Kind: "member", Name: "get:debugPrint"},
}

func TestScopedContract_ParseRejectsWhatItCannotVouchFor(t *testing.T) {
	d := newScopedDomain([]string{"vector_math"})
	if _, err := parseInterfaceUsage(testUsage(d, testScopedEntries, nil), d); err != nil {
		t.Fatalf("well-formed usage refused: %v", err)
	}
	// Domain disagreement: the analyzer narrowed a different set than the CLI asked for.
	if _, err := parseInterfaceUsage(testUsage(newScopedDomain(nil), testScopedEntries, nil), d); err == nil {
		t.Fatal("a usage computed over a different scoped domain must be refused")
	}
	for name, bad := range map[string]ContractEntry{
		"outside domain":  {Library: "package:app/main.dart", Kind: "class", Name: "Home"},
		"private":         {Library: "dart:ui", Kind: "class", Name: "_GrowableList"},
		"unknown kind":    {Library: "dart:ui", Kind: "typedef", Name: "X"},
		"quote injection": {Library: "dart:ui", Kind: "class", Name: "A', 'B"},
		"export library":  {Library: "package:flutter/material dart", Kind: "class", Name: "Text"},
	} {
		if _, err := parseInterfaceUsage(testUsage(d, append([]ContractEntry{bad}, testScopedEntries...), nil), d); err == nil {
			t.Fatalf("%s entry must be refused: %+v", name, bad)
		}
	}
	if _, err := parseInterfaceUsage(testUsage(d, testScopedEntries, []usageLibrary{{URI: "package:x/it's.dart"}}), d); err == nil {
		t.Fatal("a library URI that cannot be quoted into YAML must be refused")
	}
	if _, err := parseInterfaceUsage(testUsage(d, testScopedEntries, []usageLibrary{{URI: "package:flutter/widgets.dart", Scoped: false}}), d); err == nil {
		t.Fatal("a library table that disagrees about what is scoped must be refused")
	}
}

func TestScopedContract_BuildKeepsAppAndDepsWholeAndNarrowsTheFramework(t *testing.T) {
	d := newScopedDomain([]string{"vector_math"})
	libs := []usageLibrary{
		{URI: "package:app/main.dart"},
		{URI: "package:dep/dep.dart"},
		// A dependency that re-exports framework declarations is listed by its OWN declarations.
		{URI: "package:uikit/uikit.dart", ReexportsScoped: true, Own: []ContractEntry{
			{Library: "package:uikit/uikit.dart", Kind: "class", Name: "Fancy"},
			{Library: "package:uikit/uikit.dart", Kind: "member", Name: "fancy"},
		}},
	}
	u, err := parseInterfaceUsage(testUsage(d, testScopedEntries, libs), d)
	if err != nil {
		t.Fatal(err)
	}
	c, err := buildScopedFreehandBaseContract(
		[]string{"package:app/main.dart"},
		[]string{"package:dep/dep.dart", "package:uikit/uikit.dart", "package:vector_math/vector_math_64.dart"}, d, u)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schema != freehandContractSchemaV2 || c.Digest != freehandContractDigest(c) {
		t.Fatalf("bad v2 identity: %+v", c)
	}
	if strings.Join(c.Libraries, ",") != "dart:async,dart:collection,dart:convert,dart:core,dart:math,dart:typed_data,package:app/main.dart,package:dep/dep.dart" {
		t.Fatalf("whole libraries = %v; want app + plain dep only (never the re-exporter, never a scoped package)", c.Libraries)
	}
	y := renderFreehandContractYAML(c)
	for _, forbidden := range []string{"package:flutter/material.dart", "vector_math_64", "can-be-overridden:"} {
		if strings.Contains(y, forbidden) {
			t.Fatalf("v2 YAML must not contain %q:\n%s", forbidden, y)
		}
	}
	for _, lib := range []string{"dart:ui", "package:uikit/uikit.dart", "package:flutter/src/widgets/text.dart"} {
		if yamlListsWhole(y, lib) {
			t.Fatalf("%s must never be a whole-library entry in v2:\n%s", lib, y)
		}
	}
	section := func(name string) string { return yamlSection(t, y, name) }
	callable, ext, typ := section("callable"), section("extendable"), section("can-be-used-as-type")
	for _, want := range []string{
		"  - library: 'dart:ui'\n    class: ['Color', 'Offset']\n",
		"  - library: 'dart:ui'\n    extension: ['EnumName']\n",
		"  - library: 'dart:ui'\n    member: 'lerpDouble'\n",
		"  - library: 'package:flutter/src/foundation/print.dart'\n    member: 'get:debugPrint'\n",
		"  - library: 'package:uikit/uikit.dart'\n    class: ['Fancy']\n",
		"  - library: 'package:uikit/uikit.dart'\n    member: 'fancy'\n",
		"  - library: 'package:app/main.dart'\n",
	} {
		if !strings.Contains(callable, want) {
			t.Fatalf("callable missing %q:\n%s", want, callable)
		}
	}
	// Extendable: only classes the base extends (plus a re-exporter's own classes), never members.
	if !strings.Contains(ext, "    class: ['StatelessWidget']\n") || !strings.Contains(ext, "    class: ['Fancy']\n") ||
		strings.Contains(ext, "'Text'") || strings.Contains(ext, "member:") || strings.Contains(ext, "extension:") {
		t.Fatalf("extendable section wrong:\n%s", ext)
	}
	if strings.Contains(typ, "member:") || strings.Contains(typ, "extension:") || !strings.Contains(typ, "'Text'") {
		t.Fatalf("can-be-used-as-type section wrong:\n%s", typ)
	}
	// Deterministic, and every entry is bound into the digest.
	c2, _ := buildScopedFreehandBaseContract([]string{"package:app/main.dart"}, []string{"package:vector_math/vector_math_64.dart", "package:uikit/uikit.dart", "package:dep/dep.dart"}, d, u)
	if c2.Digest != c.Digest || renderFreehandContractYAML(c2) != y {
		t.Fatal("input order must not change the v2 contract")
	}
	fewer := c
	fewer.Entries = fewer.Entries[1:]
	if freehandContractDigest(fewer) == c.Digest {
		t.Fatal("dropping a declaration entry must change the digest")
	}
	other := c
	other.ScopedPackages = nil
	if freehandContractDigest(other) == c.Digest {
		t.Fatal("the scoped package set must be bound into the digest")
	}
}

func TestScopedContract_EmptyUsageFailsClosed(t *testing.T) {
	d := newScopedDomain(nil)
	u, err := parseInterfaceUsage(testUsage(d, nil, nil), d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildScopedFreehandBaseContract([]string{"package:app/main.dart"}, nil, d, u); err == nil {
		t.Fatal("an empty usage set must never become a base")
	}
}

func TestScopedContract_ValidationSpecCoversEverythingButTheNarrowedDomain(t *testing.T) {
	d := newScopedDomain(nil)
	u, _ := parseInterfaceUsage(testUsage(d, testScopedEntries, nil), d)
	c, err := buildScopedFreehandBaseContract([]string{"package:app/main.dart"}, nil, d, u)
	if err != nil {
		t.Fatal(err)
	}
	table, err := parseInterfaceUsage(testUsage(d, nil, []usageLibrary{
		{URI: "dart:ui", Scoped: true},
		{URI: "dart:io"},
		{URI: "package:app/main.dart"},
		{URI: "package:app/src/impl.dart"},
		{URI: "package:flutter/src/widgets/framework.dart", Scoped: true},
		{URI: "package:plugin/plugin.dart", ReexportsScoped: true, Own: []ContractEntry{{Library: "package:plugin/plugin.dart", Kind: "class", Name: "P"}}},
	}), d)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := renderInterfaceValidationYAML(c, table)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  - library: 'dart:io'\n",                   // outside the domain: whole, as the lane always accepted
		"  - library: 'package:app/src/impl.dart'\n", // even src/ and plugin code
		"  - library: 'dart:ui'\n    class: ['Color', 'Offset']\n",
		"  - library: 'package:flutter/src/widgets/framework.dart'\n    class: ['StatelessWidget']\n",
		"  - library: 'package:plugin/plugin.dart'\n    class: ['P']\n",
		"can-be-overridden:\n",
	} {
		if !strings.Contains(spec, want) {
			t.Fatalf("validation spec missing %q:\n%s", want, spec)
		}
	}
	// Entries whose declaring library is not in the source kernel are dropped (the parser would throw).
	if strings.Contains(spec, "package:flutter/src/widgets/text.dart") || strings.Contains(spec, "foundation/print.dart") {
		t.Fatalf("validation spec names a library the source kernel lacks:\n%s", spec)
	}
	over := spec[strings.Index(spec, "can-be-overridden:\n"):]
	for _, lib := range []string{"dart:ui", "dart:io", "package:flutter/src/widgets/framework.dart", "package:plugin/plugin.dart"} {
		if !strings.Contains(over, "  - library: '"+lib+"'\n") {
			t.Fatalf("can-be-overridden must list %s whole", lib)
		}
	}
	scopedOnlyCallable := spec[:strings.Index(spec, "\nextendable:")+1]
	for _, lib := range []string{"dart:ui", "package:plugin/plugin.dart", "package:flutter/src/widgets/framework.dart"} {
		if yamlListsWhole(scopedOnlyCallable, lib) {
			t.Fatalf("%s (scoped or a re-exporter) must never be whole in callable:\n%s", lib, scopedOnlyCallable)
		}
	}
	if _, err := renderInterfaceValidationYAML(buildFreehandBaseContract(nil, nil, nil, nil), table); err == nil {
		t.Fatal("a v1 contract has no validation spec")
	}
}

func TestScopedContract_RecordRoundTripAndTamper(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".soroq", "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := loadBuiltFreehandBaseContract(dir); ok || err != nil {
		t.Fatalf("no record must mean v1 (ok=%v err=%v)", ok, err)
	}
	d := newScopedDomain(nil)
	u, _ := parseInterfaceUsage(testUsage(d, testScopedEntries, nil), d)
	c, _ := buildScopedFreehandBaseContract([]string{"package:app/main.dart"}, nil, d, u)
	if _, err := installScopedFreehandBaseContract(dir, c, strings.Repeat("a", 64), len(u.Entries)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := loadBuiltFreehandBaseContract(dir)
	if err != nil || !ok || got.Digest != c.Digest {
		t.Fatalf("round trip failed: ok=%v err=%v", ok, err)
	}
	yamlPath := iosDynamicInterfacePath(dir)
	raw, _ := os.ReadFile(yamlPath)
	if err := os.WriteFile(yamlPath, append(raw, []byte("  - library: 'package:flutter/material.dart'\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadBuiltFreehandBaseContract(dir); err == nil {
		t.Fatal("a YAML that no longer matches its record must be refused, never treated as v1")
	}
}

// A v1 build after a v2 build must not leave the v2 record behind for persistence to pick up.
func TestScopedContract_V1GenerationRemovesStaleRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".soroq", "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopedContractRecordPath(dir), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// generateFreehandBaseContract needs a resolvable project; the removal happens before resolution
	// errors are possible only after the path checks, so drive it with a minimal pubspec + lock.
	os.WriteFile(filepath.Join(dir, "pubspec.yaml"), []byte("name: app\nenvironment:\n  sdk: ^3.0.0\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pubspec.lock"), []byte("packages: {}\nsdks:\n  dart: \">=3.0.0 <4.0.0\"\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".dart_tool"), 0o755)
	os.WriteFile(filepath.Join(dir, ".dart_tool", "package_config.json"), []byte(`{"configVersion":2,"packages":[{"name":"app","rootUri":"../","packageUri":"lib/"}]}`), 0o644)
	os.MkdirAll(filepath.Join(dir, "lib"), 0o755)
	os.WriteFile(filepath.Join(dir, "lib", "main.dart"), []byte("void main() {}\n"), 0o644)
	if _, err := generateFreehandBaseContract(dir, nil, nil); err != nil {
		t.Skipf("minimal project not resolvable here: %v", err)
	}
	if _, err := os.Stat(scopedContractRecordPath(dir)); !os.IsNotExist(err) {
		t.Fatal("v1 generation must remove a stale scoped contract record")
	}
}

func TestScopedContract_FlutterPinnedPackagesAreExactPinsOnly(t *testing.T) {
	pub := `name: flutter
dependencies:
  # comment
  characters: 1.4.1
  collection: 1.19.1
  material_color_utilities: 0.13.0
  meta: 1.18.0
  vector_math: 2.2.0
  ranged: ^1.0.0
  sky_engine:
    sdk: flutter

dev_dependencies:
  fake_async: 1.3.1
`
	got := strings.Join(parseExactPinnedDependencies(pub), ",")
	if got != "characters,collection,material_color_utilities,meta,vector_math" {
		t.Fatalf("pinned = %s", got)
	}
}

func TestScopedContract_ConsumersAreAppAndEligibleDepsOnly(t *testing.T) {
	d := newScopedDomain([]string{"collection"})
	got := interfaceConsumerPackages(
		[]string{"package:app/main.dart", "package:app/b.dart"},
		[]string{"package:riverpod/riverpod.dart", "package:collection/collection.dart"}, d)
	if strings.Join(got, ",") != "app,riverpod" {
		t.Fatalf("consumers = %v", got)
	}
}

func TestScopedContract_DartDefinesFromPassthrough(t *testing.T) {
	got := buildDartDefines([]string{"--no-codesign", "--dart-define=A=1", "--dart-define", "B=2", "--obfuscate"})
	if strings.Join(got, ";") != "A=1;B=2" {
		t.Fatalf("defines = %v", got)
	}
}

// yamlSection returns the body of a top-level section (up to the next unindented line).
func yamlSection(t *testing.T, y, name string) string {
	t.Helper()
	i := strings.Index(y, "\n"+name+":\n")
	if i < 0 {
		t.Fatalf("missing section %s", name)
	}
	lines := strings.SplitAfter(y[i+len(name)+3:], "\n")
	var b strings.Builder
	for _, l := range lines {
		if l != "" && l[0] != ' ' {
			break
		}
		b.WriteString(l)
	}
	return b.String()
}

// yamlListsWhole reports whether a `- library: 'lib'` item appears with no class/member/extension key,
// i.e. as a whole-library entry.
func yamlListsWhole(y, lib string) bool {
	lines := strings.Split(y, "\n")
	for i, l := range lines {
		if l == "  - library: '"+lib+"'" && (i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "    ")) {
			return true
		}
	}
	return false
}

// Every v2 contract must expose what Soroq's own module synthesizer emits (@pragma), whatever the app uses.
func TestScopedContractAlwaysExposesSynthesizerPragma(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "tools", "soroq_kernel_analyze", "bin", "analyze.dart"))
	if err != nil {
		t.Skip("analyzer source not in tree")
	}
	if !strings.Contains(string(raw), "@pragma('vm:entry-point')") {
		t.Fatal("the synthesizer no longer emits @pragma; revisit soroqModuleSynthesizerEntries")
	}
	found := false
	for _, e := range soroqModuleSynthesizerEntries {
		if e.Library == "dart:core" && e.Kind == "class" && e.Name == "pragma" {
			found = true
		}
	}
	if !found {
		t.Fatal("dart:core pragma missing from soroqModuleSynthesizerEntries")
	}
}

func testUsageSDKExt(domain scopedDomain, entries []ContractEntry, libs []usageLibrary, sdkExt []ContractEntry, pure []string) []byte {
	var m map[string]any
	_ = json.Unmarshal(testUsage(domain, entries, libs), &m)
	m["pure_sdk_libraries"] = pure
	m["sdk_extendable"] = sdkExt
	raw, _ := json.Marshal(m)
	return raw
}

var testSDKExtendable = []ContractEntry{
	{Library: "dart:collection", Kind: "extendable", Name: "ListBase"},
	{Library: "dart:core", Kind: "extendable", Name: "Exception"},
	{Library: "dart:core", Kind: "extendable", Name: "StateError"},
}

// v3: the pure SDK stays whole for callable / can-be-used-as-type, but its EXTENDABLE surface is only
// what the analyzer listed -- List, Map, Iterable, Comparable, num ... are no longer extendable unless the
// base itself extends them.
func TestScopedContract_V3ScopesTheSDKExtendableSurface(t *testing.T) {
	d := newScopedDomain(nil)
	u, err := parseInterfaceUsage(testUsageSDKExt(d, testScopedEntries, nil, testSDKExtendable, pureSDKContractLibraries()), d)
	if err != nil {
		t.Fatalf("well-formed v3 usage refused: %v", err)
	}
	c, err := buildScopedFreehandBaseContract([]string{"package:app/main.dart"}, nil, d, u)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schema != freehandContractSchemaV3 || c.Digest != freehandContractDigest(c) {
		t.Fatalf("bad v3 identity: %+v", c)
	}
	if strings.Join(c.ExtendableScopedLibraries, ",") != strings.Join(pureSDKContractLibraries(), ",") {
		t.Fatalf("extendable-scoped libraries = %v", c.ExtendableScopedLibraries)
	}
	y := renderFreehandContractYAML(c)
	callable, ext, typ := yamlSection(t, y, "callable"), yamlSection(t, y, "extendable"), yamlSection(t, y, "can-be-used-as-type")
	for _, lib := range pureSDKContractLibraries() {
		if !yamlListsWhole(callable, lib) || !yamlListsWhole(typ, lib) {
			t.Fatalf("%s must stay whole for callable and can-be-used-as-type:\n%s", lib, y)
		}
		if yamlListsWhole(ext, lib) {
			t.Fatalf("%s must not be whole under extendable in v3:\n%s", lib, ext)
		}
	}
	for _, want := range []string{
		"  - library: 'dart:collection'\n    class: ['ListBase']\n",
		"  - library: 'dart:core'\n    class: ['Exception', 'StateError']\n",
		"  - library: 'package:app/main.dart'\n",
		"    class: ['StatelessWidget']\n",
	} {
		if !strings.Contains(ext, want) {
			t.Fatalf("v3 extendable missing %q:\n%s", want, ext)
		}
	}
	if strings.Contains(callable, "'ListBase'") || strings.Contains(typ, "'Exception'") {
		t.Fatalf("SDK extendable entries leaked into another section:\n%s", y)
	}
	// Bound into the digest, and a v3 record round-trips through the loader's schema check.
	v2 := c
	v2.ExtendableScopedLibraries = nil
	if freehandContractDigest(v2) == c.Digest {
		t.Fatal("the extendable-scoped library set must be bound into the digest")
	}

	// The patch side narrows extendable exactly like the base: whole for everything else, the SDK by entry.
	table, err := parseInterfaceUsage(testUsage(d, nil, []usageLibrary{
		{URI: "dart:core"}, {URI: "dart:collection"}, {URI: "dart:io"}, {URI: "dart:ui", Scoped: true},
		{URI: "package:app/main.dart"},
	}), d)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := renderInterfaceValidationYAML(c, table)
	if err != nil {
		t.Fatal(err)
	}
	sExt := yamlSection(t, spec, "extendable")
	if yamlListsWhole(sExt, "dart:core") || yamlListsWhole(sExt, "dart:collection") || !yamlListsWhole(sExt, "dart:io") || !yamlListsWhole(sExt, "package:app/main.dart") {
		t.Fatalf("validation spec extendable wrong:\n%s", sExt)
	}
	if !strings.Contains(sExt, "  - library: 'dart:core'\n    class: ['Exception', 'StateError']\n") || !strings.Contains(sExt, "  - library: 'dart:collection'\n    class: ['ListBase']\n") {
		t.Fatalf("validation spec must list the SDK extendable classes:\n%s", sExt)
	}
	if !yamlListsWhole(yamlSection(t, spec, "callable"), "dart:core") {
		t.Fatalf("validation spec must keep dart:core whole for callable:\n%s", spec)
	}
}

func TestScopedContract_V3ParseRefusesBadSDKExtendable(t *testing.T) {
	d := newScopedDomain(nil)
	pure := pureSDKContractLibraries()
	for name, bad := range map[string]ContractEntry{
		"not extendable kind": {Library: "dart:core", Kind: "class", Name: "List"},
		"scoped sdk library":  {Library: "dart:ui", Kind: "extendable", Name: "Color"},
		"package library":     {Library: "package:app/main.dart", Kind: "extendable", Name: "Home"},
		"private":             {Library: "dart:core", Kind: "extendable", Name: "_List"},
	} {
		if _, err := parseInterfaceUsage(testUsageSDKExt(d, testScopedEntries, nil, []ContractEntry{bad}, pure), d); err == nil {
			t.Fatalf("%s: sdk_extendable entry %+v must be refused", name, bad)
		}
	}
	if _, err := parseInterfaceUsage(testUsageSDKExt(d, testScopedEntries, nil, testSDKExtendable, []string{"dart:core"}), d); err == nil {
		t.Fatal("a pure SDK library set that differs from the CLI's must be refused")
	}
	// Without the analyzer flag the usage has neither key and the contract stays v2.
	u, err := parseInterfaceUsage(testUsage(d, testScopedEntries, nil), d)
	if err != nil {
		t.Fatal(err)
	}
	c, err := buildScopedFreehandBaseContract([]string{"package:app/main.dart"}, nil, d, u)
	if err != nil || c.Schema != freehandContractSchemaV2 || len(c.ExtendableScopedLibraries) != 0 {
		t.Fatalf("usage without sdk_extendable must build v2: %+v %v", c, err)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "extendable_scoped_libraries") {
		t.Fatalf("v2 contract JSON gained a v3 key: %s", raw)
	}
}
