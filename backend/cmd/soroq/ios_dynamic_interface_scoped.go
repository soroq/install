package main

// THE USAGE-SCOPED BASE CONTRACT (soroq.freehand.base_contract.v2).
//
// WHY. The v1 contract (ios_dynamic_interface.go) lists every public dart: SDK library the curated set
// names and the whole Flutter framework as WHOLE LIBRARIES under callable / extendable /
// can-be-used-as-type. Every public member of those libraries must then stay callable by a future patch
// module, so the AOT tree shaker keeps the entire framework. Measured on a small benchmark app:
// App.framework __text 14.8 MB against 1.7 MB for stock Flutter 3.44.9, and 2.2 MB with the SDK/Flutter
// entries removed. The retention manifest and Path A contribute ~0.3 MB; the interface is the bloat.
//
// WHAT CHANGES. For the SCOPED DOMAIN -- exactly the SDK libraries v1 listed, dart:ui, and every
// package:flutter/ library -- v2 lists only what the base's own code uses, at CLASS granularity:
//
//   - every public class the base references (call target's enclosing class, constructed/constant class,
//     type, super/mixin/interface of a base class), plus that class's whole supertype closure. A class
//     entry makes every public member of that class callable, so a hotfix may use any constructor,
//     method, getter or field of a widget/SDK type the base already uses;
//   - the classes in a used member's signature (one hop), so a patch can hold what a used API returns;
//   - individually: used top-level functions/getters/setters/fields, named extensions, extension types.
//
// Usage is computed by the Soroq kernel analyzer (`--interface-usage`, tools/soroq_kernel_analyze/bin/
// interface_usage.dart) from a non-AOT kernel of the EXACT build entrypoint (the generated bootstrap), so
// Soroq's bootstrap/activator and the soroq_flutter runtime count as users too. Every entry names the
// DECLARING library, which is what the front end's LibraryIndex resolves (an export library would not).
//
// What does NOT change: the application's libraries and eligible Dart-only dependency libraries are still
// WHOLE-library entries (the same derivation as v1, contractProjectLibraries), except that a library which
// RE-EXPORTS scoped declarations is listed by its OWN declarations. A whole-library entry follows
// additionalExports, so `export 'package:flutter/material.dart'` in any dependency would otherwise put the
// whole framework back -- and let a patch validate a framework reference the base never retained.
// can-be-overridden and dynamically-callable stay excluded for the reasons recorded in v1.
//
// THE PATCH SIDE. A class the base never used is not retained, so a module referencing it would abort in
// the VM's bytecode reader on a device. At persist time the base records an INTERFACE VALIDATION SPEC
// (renderInterfaceValidationYAML) beside the baseline, and `soroq patch` runs the front end's own
// dynamic-module validator (`dart2bytecode --validate`) against it before compiling. The spec covers every
// non-scoped library whole (so nothing outside the narrowed domain is refused that was accepted before)
// and the scoped domain exactly as the base contract does, so the only interface refusal it can produce
// is "this patch uses a framework/SDK declaration the installed base does not expose" -- a named refusal
// on the operator's machine instead of a crash on a phone (freehand_interface_validation.go).
//
// COMPATIBILITY. v1 bases keep their contract byte for byte: this file never rewrites a v1 contract and
// v1's digest is unchanged. The schema string differs, so a v1 and a v2 base are never interchangeable.
// A frontend whose analyzer predates `--interface-usage` builds a v1 base, with a notice; so does
// SOROQ_IOS_BASE_CONTRACT=v1 (an explicit escape hatch).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"soroq/backend/internal/depgraph"
)

const freehandContractSchemaV2 = "soroq.freehand.base_contract.v2"

// freehandContractSchemaV3 is v2 with the pure SDK libraries' EXTENDABLE surface usage-scoped too
// (FreehandBaseContract.ExtendableScopedLibraries). v2 listed dart:core, dart:collection, ... whole under
// extendable, which marks List, Map, Set, Iterable, Iterator, Future, Stream, num and every other public
// SDK class as possibly subclassed by a module. The AOT compiler then gives up class-id range type checks,
// CHA devirtualization and unboxed calling conventions for all of them, in every base. Measured on the
// benchmark workloads (same kernel, only the interface differs): collection-heavy code ~10% faster and
// JSON ~9% faster with v3. What a module can still extend: every SDK class the base's own (consumer)
// classes extend, implement or mix in -- exactly what a module that re-declares or carries a base class
// needs -- plus every public exception and error type and Comparable (which pure-Dart packages a patch may
// newly add, such as package:decimal, implement). Callable and can-be-used-as-type stay whole: a patch may
// still CALL or NAME anything in the pure SDK. A patch that declares a NEW subtype of anything else (a
// custom List, Stream, Codec ...) is refused at patch build time by the interface validation, by name.
const freehandContractSchemaV3 = "soroq.freehand.base_contract.v3"

// freehandContractSchemaV4 is v3 plus every INSTANCE-CLOSED scoped class: each public SDK/Flutter class
// the base's own AOT build can instantiate (a live constructor), with its supertypes, whole for callable
// and can-be-used-as-type, and extendable when abstract. Their code is in the snapshot anyway because
// the base uses them; v4 only keeps their public surface reachable, so a patch can call any API of a
// class the base has (a class it cannot instantiate is carried by the patch instead). Built in two
// passes: the first build's AOT kernel says which classes those are. Opt-in: SOROQ_IOS_BASE_CONTRACT=v4.
const freehandContractSchemaV4 = "soroq.freehand.base_contract.v4"

// sdkExtendableSchema is what the analyzer's --interface-sdk-extendable-schema probe prints.
const sdkExtendableSchema = "soroq.freehand.sdk_extendable.v1"

// isScopedContractSchema reports whether a base contract is usage-scoped (v2 or v3): such a base carries a
// contract YAML and a patch-side interface validation spec in its baseline.
func isScopedContractSchema(schema string) bool {
	return schema == freehandContractSchemaV2 || schema == freehandContractSchemaV3 || schema == freehandContractSchemaV4
}

// instanceClosedContractRequested reports whether this release builds a v4 (instance-closed) base.
func instanceClosedContractRequested() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SOROQ_IOS_BASE_CONTRACT")), "v4")
}

// extendContractInstanceClosed turns the contract the first build used into v4: the instance-closed
// classes of that build's AOT kernel are added as class entries (and extendable when abstract), and the
// contract is reinstalled for the second build.
func extendContractInstanceClosed(projectDir, flutterRoot, appDill string) (FreehandBaseContract, error) {
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return FreehandBaseContract{}, err
	}
	c, ok, err := loadBuiltFreehandBaseContract(absDir)
	if err != nil {
		return FreehandBaseContract{}, err
	}
	if !ok {
		return FreehandBaseContract{}, errors.New("a v4 base extends the usage-scoped contract, and this build has none")
	}
	raw, err := os.ReadFile(scopedContractRecordPath(absDir))
	if err != nil {
		return FreehandBaseContract{}, err
	}
	var rec scopedContractRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return FreehandBaseContract{}, err
	}
	analyzer, err := freehandPatchAnalyzer(flutterRoot)
	if err != nil {
		return FreehandBaseContract{}, err
	}
	dart := filepath.Join(flutterRoot, "bin", "cache", "dart-sdk", "bin", "dart")
	out, err := exec.Command(dart, analyzer, "--instance-closed", appDill, "--scoped-packages", strings.Join(c.ScopedPackages, ",")).Output()
	if err != nil {
		return FreehandBaseContract{}, fmt.Errorf("compute the instance-closed classes (the analyzer must support --instance-closed): %w", err)
	}
	var ic struct {
		Schema  string `json:"schema"`
		Classes []struct {
			Library  string `json:"library"`
			Class    string `json:"class"`
			Abstract bool   `json:"abstract"`
		} `json:"classes"`
	}
	if err := json.Unmarshal(out, &ic); err != nil || ic.Schema != "soroq.freehand.instance_closed.v1" {
		return FreehandBaseContract{}, fmt.Errorf("unreadable instance-closed report: %v", err)
	}
	entries := append([]ContractEntry(nil), c.Entries...)
	for _, k := range ic.Classes {
		entries = append(entries, ContractEntry{Library: k.Library, Kind: "class", Name: k.Class})
		if k.Abstract {
			entries = append(entries, ContractEntry{Library: k.Library, Kind: "extendable", Name: k.Class})
		}
	}
	c.Entries = sortedUniqueEntries(entries)
	for _, e := range c.Entries {
		if err := e.validate(); err != nil {
			return FreehandBaseContract{}, err
		}
	}
	c.Schema = freehandContractSchemaV4
	c.Digest = freehandContractDigest(c)
	c, err = installScopedFreehandBaseContract(absDir, c, rec.AnalyzerSHA, rec.UsageEntries)
	if err != nil {
		return FreehandBaseContract{}, err
	}
	fmt.Fprintf(os.Stderr, "soroq contract: %s (instance-closed) -- %d instance-closed classes added; rebuilding\n", c.Schema, len(ic.Classes))
	return c, nil
}

// pureSDKContractLibraries are the SDK libraries a scoped contract lists whole for callable and
// can-be-used-as-type: sdkContractLibraries minus the scoped ones. Sorted.
func pureSDKContractLibraries() []string {
	var out []string
	for _, l := range sdkContractLibraries {
		scoped := false
		for _, s := range scopedSDKLibraries {
			if s == l {
				scoped = true
			}
		}
		if !scoped {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// interfaceUsageSchema is what the analyzer's --interface-usage mode emits (and --interface-usage-schema
// prints).
const interfaceUsageSchema = "soroq.freehand.interface_usage.v1"

// scopedContractRecordSchema versions the record the build writes beside the YAML.
const scopedContractRecordSchema = "soroq.freehand.base_contract_record.v1"

// scopedSDKLibraries MUST equal the analyzer's scopedSdkLibraries; the parsed usage is refused otherwise,
// so the two can never silently disagree about which declarations were narrowed.
var scopedSDKLibraries = []string{
	"dart:developer",
	"dart:ui",
}

const scopedFlutterPrefix = "package:flutter/"

// scopedDomain is the set of libraries a v2 contract narrows: scopedSDKLibraries, package:flutter/, and
// the packages the Flutter SDK itself pins that the application does not declare as its own dependency
// (flutterPinnedScopedPackages). It is recorded in the contract so the patch side narrows the same set.
type scopedDomain struct {
	packages []string // sorted package names in addition to flutter
}

func newScopedDomain(packages []string) scopedDomain {
	p := append([]string(nil), packages...)
	sort.Strings(p)
	return scopedDomain{packages: p}
}

func (d scopedDomain) prefixes() []string {
	out := []string{scopedFlutterPrefix}
	for _, p := range d.packages {
		out = append(out, "package:"+p+"/")
	}
	sort.Strings(out)
	return out
}

func (d scopedDomain) isScoped(uri string) bool {
	for _, p := range d.prefixes() {
		if strings.HasPrefix(uri, p) {
			return true
		}
	}
	for _, l := range scopedSDKLibraries {
		if l == uri {
			return true
		}
	}
	return false
}

// ContractEntry is one declaration-level dynamic-interface entry.
type ContractEntry struct {
	Library string `json:"library"`
	// Kind is class | extendable | extension | extension_type | member. A class entry makes the class
	// callable and usable as a type; an extendable entry (a class the base extends, implements or mixes
	// in) additionally lets a module class extend it.
	Kind string `json:"kind"`
	// Name is the declaration name; for a top-level member it is the LibraryIndex name (get:x / set:x for
	// a getter / setter procedure).
	Name string `json:"name"`
}

func (e ContractEntry) key() string { return e.Library + "\x00" + e.Kind + "\x00" + e.Name }

var (
	contractNameRe = regexp.MustCompile(`^(get:|set:)?[A-Za-z_$][A-Za-z0-9_$]*$`)
	contractLibRe  = regexp.MustCompile(`^(dart:[A-Za-z_][A-Za-z0-9_]*|package:[A-Za-z0-9_.\-]+/[A-Za-z0-9_./\-]+\.dart)$`)
)

func (e ContractEntry) validate() error {
	if !contractLibRe.MatchString(e.Library) {
		return fmt.Errorf("contract entry library %q is not a dart:/package: library URI", e.Library)
	}
	switch e.Kind {
	case "class", "extendable", "extension", "extension_type":
		if strings.Contains(e.Name, ":") || !contractNameRe.MatchString(e.Name) || strings.HasPrefix(e.Name, "_") {
			return fmt.Errorf("contract entry %s %q in %s is not a public identifier", e.Kind, e.Name, e.Library)
		}
	case "member":
		if !contractNameRe.MatchString(e.Name) || strings.HasPrefix(strings.TrimPrefix(strings.TrimPrefix(e.Name, "get:"), "set:"), "_") {
			return fmt.Errorf("contract entry member %q in %s is not a public member name", e.Name, e.Library)
		}
	default:
		return fmt.Errorf("contract entry kind %q is not one of class/extension/extension_type/member", e.Kind)
	}
	return nil
}

// interfaceUsage is the parsed output of the analyzer's --interface-usage mode.
type interfaceUsage struct {
	Schema            string          `json:"schema"`
	ScopedSDK         []string        `json:"scoped_sdk_libraries"`
	ScopedPrefixes    []string        `json:"scoped_prefixes"`
	ConsumerPackages  []string        `json:"consumer_packages"`
	ConsumerLibraries int             `json:"consumer_libraries"`
	Entries           []ContractEntry `json:"entries"`
	// PureSDK and SDKExtendable are present only when the analyzer ran with --interface-sdk-extendable:
	// the pure SDK classes the base's consumer classes extend / implement / mix in, plus every public
	// exception and error type. nil means the analyzer was not asked (a v2 base).
	PureSDK       []string         `json:"pure_sdk_libraries,omitempty"`
	SDKExtendable *[]ContractEntry `json:"sdk_extendable,omitempty"`
	Libraries     []usageLibrary   `json:"libraries"`
}

type usageLibrary struct {
	URI             string          `json:"uri"`
	Scoped          bool            `json:"scoped"`
	ReexportsScoped bool            `json:"reexports_scoped,omitempty"`
	Own             []ContractEntry `json:"own,omitempty"`
	// Consts are the public compile-time constant fields of a scoped library: `name` (top level) or
	// `Class.name` (static). The front end folds a constant into the patch module, so reading one never
	// calls into the base; the patch-side spec therefore allows them all (they cost the base nothing).
	Consts []string `json:"consts,omitempty"`
}

// parseInterfaceUsage decodes and validates analyzer output against the domain the CLI asked for.
// Anything unexpected is refused: a usage set the CLI cannot vouch for must not decide what a base
// retains.
func parseInterfaceUsage(raw []byte, domain scopedDomain) (interfaceUsage, error) {
	var u interfaceUsage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&u); err != nil {
		return interfaceUsage{}, fmt.Errorf("decode interface usage: %w", err)
	}
	if u.Schema != interfaceUsageSchema {
		return interfaceUsage{}, fmt.Errorf("interface usage schema %q != %q", u.Schema, interfaceUsageSchema)
	}
	gotSDK := append([]string(nil), u.ScopedSDK...)
	sort.Strings(gotSDK)
	wantSDK := append([]string(nil), scopedSDKLibraries...)
	sort.Strings(wantSDK)
	gotPrefixes := append([]string(nil), u.ScopedPrefixes...)
	sort.Strings(gotPrefixes)
	if strings.Join(gotSDK, ",") != strings.Join(wantSDK, ",") ||
		strings.Join(gotPrefixes, ",") != strings.Join(domain.prefixes(), ",") {
		return interfaceUsage{}, fmt.Errorf("the analyzer's scoped domain (%v + %v) differs from this CLI's (%v + %v); the analyzer and the CLI must agree on what is narrowed",
			u.ScopedSDK, u.ScopedPrefixes, scopedSDKLibraries, domain.prefixes())
	}
	for _, e := range u.Entries {
		if err := e.validate(); err != nil {
			return interfaceUsage{}, err
		}
		if !domain.isScoped(e.Library) {
			return interfaceUsage{}, fmt.Errorf("usage entry %s %s is outside the scoped domain", e.Library, e.Name)
		}
	}
	if u.SDKExtendable != nil {
		got := append([]string(nil), u.PureSDK...)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(pureSDKContractLibraries(), ",") {
			return interfaceUsage{}, fmt.Errorf("the analyzer's pure SDK libraries %v differ from this CLI's %v", got, pureSDKContractLibraries())
		}
		pure := map[string]bool{}
		for _, l := range got {
			pure[l] = true
		}
		for _, e := range *u.SDKExtendable {
			if err := e.validate(); err != nil {
				return interfaceUsage{}, err
			}
			if e.Kind != "extendable" || !pure[e.Library] {
				return interfaceUsage{}, fmt.Errorf("sdk_extendable entry %s %s %s is not an extendable pure-SDK class", e.Library, e.Kind, e.Name)
			}
		}
	} else if u.PureSDK != nil {
		return interfaceUsage{}, errors.New("interface usage lists pure SDK libraries without sdk_extendable")
	}
	for _, l := range u.Libraries {
		// Library URIs are emitted verbatim into single-quoted YAML scalars.
		if l.URI == "" || strings.ContainsAny(l.URI, "'\n\r\\") {
			return interfaceUsage{}, fmt.Errorf("usage library URI %q cannot be written into a dynamic interface", l.URI)
		}
		if l.Scoped != domain.isScoped(l.URI) {
			return interfaceUsage{}, fmt.Errorf("usage library %s scoped=%v disagrees with this CLI", l.URI, l.Scoped)
		}
		for _, e := range l.Own {
			if err := e.validate(); err != nil {
				return interfaceUsage{}, err
			}
			if e.Library != l.URI {
				return interfaceUsage{}, fmt.Errorf("own declaration %s of %s names library %s", e.Name, l.URI, e.Library)
			}
		}
	}
	return u, nil
}

// ownWithExtendable turns a library's own declarations into whole-library-equivalent entries: each own
// class is also extendable, exactly as a `library:` entry would make it.
func ownWithExtendable(own []ContractEntry) []ContractEntry {
	out := append([]ContractEntry(nil), own...)
	for _, e := range own {
		if e.Kind == "class" {
			out = append(out, ContractEntry{Library: e.Library, Kind: "extendable", Name: e.Name})
		}
	}
	return out
}

func sortedUniqueEntries(in []ContractEntry) []ContractEntry {
	seen := map[string]ContractEntry{}
	for _, e := range in {
		seen[e.key()] = e
	}
	out := make([]ContractEntry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// buildScopedFreehandBaseContract derives the v2 contract: app + eligible dependency libraries whole
// (as v1) -- except a re-exporter of scoped declarations, which is listed by its own declarations -- plus
// the scoped declarations the base uses.
func buildScopedFreehandBaseContract(appLibraries, depLibraries []string, domain scopedDomain, usage interfaceUsage) (FreehandBaseContract, error) {
	if len(usage.Entries) == 0 {
		// Every Flutter app uses dart:core and the framework. An empty set means the analysis saw no
		// consumer code, and building on it would retain nothing a patch could call.
		return FreehandBaseContract{}, errors.New("interface usage is empty: the analyzer found no SDK/Flutter use in the base kernel, which is impossible for a real app; refusing to build a base on it")
	}
	table := map[string]usageLibrary{}
	for _, l := range usage.Libraries {
		table[l.URI] = l
	}
	whole := map[string]bool{}
	entries := append([]ContractEntry(nil), usage.Entries...)
	// Soroq's own generated patch-module code is a consumer too: the module synthesizer
	// (soroq_kernel_analyze --synthesize) annotates every module entry point with
	// @pragma('vm:entry-point') and @pragma('dyn-module:entry-point'). The class is always present in
	// the runtime (the SDK itself uses it), so exposing it costs the base nothing; without it EVERY patch
	// against a base whose own code never wrote @pragma is refused.
	for _, e := range soroqModuleSynthesizerEntries {
		if domain.isScoped(e.Library) { // a whole library already exposes it
			entries = append(entries, e)
		}
	}
	for _, lib := range append(append([]string(nil), appLibraries...), depLibraries...) {
		if domain.isScoped(lib) {
			continue // never whole: that is precisely what v2 removes
		}
		if t, ok := table[lib]; ok && t.ReexportsScoped {
			entries = append(entries, ownWithExtendable(t.Own)...)
			continue
		}
		whole[lib] = true
	}
	// The pure Dart SDK libraries stay WHOLE, as in v1. A patch may carry a NEW pure-Dart dependency, and
	// new dependency code uses SDK classes the app itself never touched (measured on the device lane's
	// dependency gate: BigInt, RegExp and 200+ members). Their unused parts cost little size and no call
	// speed (the core types the app uses were exposed whole-class anyway); scoping them would make "add a
	// dependency in a patch" -- which Shorebird allows -- a store release. dart:ui and dart:developer stay
	// scoped: they are engine bindings a pure-Dart dependency has no business reaching.
	for _, lib := range sdkContractLibraries {
		if !domain.isScoped(lib) {
			whole[lib] = true
		}
	}
	libs := make([]string, 0, len(whole))
	for l := range whole {
		libs = append(libs, l)
	}
	sort.Strings(libs)
	schema := freehandContractSchemaV2
	var extScoped []string
	if usage.SDKExtendable != nil {
		// v3: the pure SDK libraries stay whole for callable / can-be-used-as-type, but only the usage-
		// scoped set is extendable.
		schema = freehandContractSchemaV3
		for _, l := range pureSDKContractLibraries() {
			if whole[l] {
				extScoped = append(extScoped, l)
			}
		}
		entries = append(entries, *usage.SDKExtendable...)
	}
	c := FreehandBaseContract{
		Schema:                    schema,
		ExtendableScopedLibraries: extScoped,
		Libraries:                 libs,
		Entries:                   sortedUniqueEntries(entries),
		ScopedPackages:            append([]string(nil), domain.packages...),
		Sections:                  contractSections,
	}
	for _, e := range c.Entries {
		if err := e.validate(); err != nil {
			return FreehandBaseContract{}, err
		}
	}
	c.Digest = freehandContractDigest(c)
	return c, nil
}

// sectionKinds is which declaration kinds each section accepts (the front end's parser rejects the
// others: extendable takes classes only, can-be-used-as-type classes and extension types).
var sectionKinds = map[string]map[string]bool{
	"callable":            {"class": true, "extension": true, "extension_type": true, "member": true},
	"extendable":          {"extendable": true},
	"can-be-used-as-type": {"class": true, "extension_type": true},
	"can-be-overridden":   {},
}

// writeInterfaceSection emits one section: whole libraries first, then declaration entries grouped by
// declaring library in canonical order. Lists use the parser's `class: [..]` form.
func writeInterfaceSection(b *strings.Builder, section string, wholeLibs []string, entries []ContractEntry) {
	fmt.Fprintf(b, "%s:\n", section)
	for _, l := range wholeLibs {
		fmt.Fprintf(b, "  - library: '%s'\n", l)
	}
	kinds := sectionKinds[section]
	type group struct {
		classes, extensions, extTypes, members []string
	}
	order := []string{}
	groups := map[string]*group{}
	for _, e := range entries {
		if !kinds[e.Kind] {
			continue
		}
		g := groups[e.Library]
		if g == nil {
			g = &group{}
			groups[e.Library] = g
			order = append(order, e.Library)
		}
		switch e.Kind {
		case "class", "extendable":
			g.classes = append(g.classes, e.Name)
		case "extension":
			g.extensions = append(g.extensions, e.Name)
		case "extension_type":
			g.extTypes = append(g.extTypes, e.Name)
		case "member":
			g.members = append(g.members, e.Name)
		}
	}
	sort.Strings(order)
	list := func(names []string) string {
		q := make([]string, len(names))
		for i, n := range names {
			q[i] = "'" + n + "'"
		}
		return "[" + strings.Join(q, ", ") + "]"
	}
	for _, lib := range order {
		g := groups[lib]
		if len(g.classes) > 0 {
			fmt.Fprintf(b, "  - library: '%s'\n    class: %s\n", lib, list(g.classes))
		}
		if len(g.extensions) > 0 {
			fmt.Fprintf(b, "  - library: '%s'\n    extension: %s\n", lib, list(g.extensions))
		}
		if len(g.extTypes) > 0 {
			fmt.Fprintf(b, "  - library: '%s'\n    extension_type: %s\n", lib, list(g.extTypes))
		}
		for _, m := range g.members {
			fmt.Fprintf(b, "  - library: '%s'\n    member: '%s'\n", lib, m)
		}
	}
}

// renderScopedFreehandContractYAML produces the exact v2 bytes handed to the base build. Deterministic.
func renderScopedFreehandContractYAML(c FreehandBaseContract) string {
	var b strings.Builder
	b.WriteString("# Generated by Soroq. Do not edit.\n")
	fmt.Fprintf(&b, "# schema: %s\n", c.Schema)
	b.WriteString("# Usage-scoped: application and eligible Dart-only dependency libraries whole; SDK and\n")
	b.WriteString("# Flutter declarations only where this base uses them (class granularity).\n")
	if c.Schema == freehandContractSchemaV4 {
		b.WriteString("# v4: plus every class the base can instantiate (and its supertypes), whole; extendable when abstract.\n")
	}
	if c.Schema == freehandContractSchemaV3 || c.Schema == freehandContractSchemaV4 {
		b.WriteString("# v3: pure SDK libraries are whole for callable / can-be-used-as-type; extendable lists only\n")
		b.WriteString("# the SDK classes this base's own classes extend, plus exception and error types.\n")
	}
	for _, section := range c.Sections {
		writeInterfaceSection(&b, section, sectionWholeLibraries(section, c.Libraries, c.ExtendableScopedLibraries), c.Entries)
	}
	return b.String()
}

// sectionWholeLibraries is the whole-library list of one section: every whole library, except that the
// extendable section of a v3 contract leaves out the libraries whose extendable surface is usage-scoped
// (their extendable classes are declaration entries instead).
func sectionWholeLibraries(section string, whole, extendableScoped []string) []string {
	if section != "extendable" || len(extendableScoped) == 0 {
		return whole
	}
	drop := map[string]bool{}
	for _, l := range extendableScoped {
		drop[l] = true
	}
	var out []string
	for _, l := range whole {
		if !drop[l] {
			out = append(out, l)
		}
	}
	return out
}

// renderInterfaceValidationYAML builds the PATCH-SIDE validation spec for a v2 base from the base's
// contract and the library table of the base SOURCE kernel (the --import-dill every patch compiles
// against). It is never used to build anything; it only decides what `dart2bytecode --validate` accepts.
//
//   - every non-scoped library of the source kernel is whole in every section, so nothing outside the
//     narrowed domain is refused that the lane accepted before (a re-exporter of scoped declarations is
//     listed by its own declarations, so the re-export cannot validate an unretained framework class);
//   - scoped declarations appear exactly as in the base contract (restricted to libraries the source
//     kernel contains, since the parser resolves every entry against the compile's component);
//   - can-be-overridden lists every library: the lane's module classes are shape carriers that re-declare
//     overriding members, and override checking has never been part of this lane's contract.
//
// Dynamic-call diagnostics are not interface questions and are ignored by the caller.
func renderInterfaceValidationYAML(c FreehandBaseContract, sourceKernel interfaceUsage) (string, error) {
	if !isScopedContractSchema(c.Schema) {
		return "", fmt.Errorf("an interface validation spec is only derived for %s / %s bases, not %s", freehandContractSchemaV2, freehandContractSchemaV3, c.Schema)
	}
	domain := newScopedDomain(c.ScopedPackages)
	present := map[string]bool{}
	var whole, all []string
	var own []ContractEntry
	for _, l := range sourceKernel.Libraries {
		present[l.URI] = true
		all = append(all, l.URI)
		switch {
		case l.Scoped:
		case l.ReexportsScoped:
			own = append(own, ownWithExtendable(l.Own)...)
		default:
			whole = append(whole, l.URI)
		}
	}
	if len(all) == 0 {
		return "", errors.New("the base source kernel lists no libraries")
	}
	sort.Strings(whole)
	sort.Strings(all)
	entries := own
	extScoped := map[string]bool{}
	for _, l := range c.ExtendableScopedLibraries {
		extScoped[l] = true
	}
	for _, e := range c.Entries {
		if (domain.isScoped(e.Library) || extScoped[e.Library]) && present[e.Library] {
			entries = append(entries, e)
		}
	}
	entries = sortedUniqueEntries(entries)
	var b strings.Builder
	b.WriteString("# Generated by Soroq. Do not edit.\n")
	b.WriteString("# PATCH-SIDE validation spec for a usage-scoped base. Not a build input.\n")
	fmt.Fprintf(&b, "# base contract: %s %s\n", c.Schema, c.Digest)
	for _, section := range c.Sections {
		writeInterfaceSection(&b, section, sectionWholeLibraries(section, whole, c.ExtendableScopedLibraries), entries)
		if section == "callable" {
			writeConstEntries(&b, sourceKernel.Libraries, present)
		}
	}
	writeInterfaceSection(&b, "can-be-overridden", all, nil)
	return b.String(), nil
}

// scopedContractRecord is written beside the YAML so baseline persistence binds the contract THIS build
// used instead of re-deriving it (a re-derivation would need the usage kernel again, and could differ).
type scopedContractRecord struct {
	Schema       string               `json:"schema"`
	Contract     FreehandBaseContract `json:"contract"`
	YAMLSHA256   string               `json:"yaml_sha256"`
	AnalyzerSHA  string               `json:"usage_analyzer_sha256"`
	UsageEntries int                  `json:"usage_entries"`
}

func scopedContractRecordPath(absProjectDir string) string {
	return filepath.Join(absProjectDir, ".soroq", "generated", "ios_dynamic_interface.json")
}

func iosDynamicInterfacePath(absProjectDir string) string {
	return filepath.Join(absProjectDir, ".soroq", "generated", "ios_dynamic_interface.yaml")
}

// installScopedFreehandBaseContract writes the v2 YAML (at the same path the build already references)
// and its record.
func installScopedFreehandBaseContract(absProjectDir string, c FreehandBaseContract, analyzerSHA string, usageEntries int) (FreehandBaseContract, error) {
	path := iosDynamicInterfacePath(absProjectDir)
	yaml := renderFreehandContractYAML(c)
	sum := sha256.Sum256([]byte(yaml))
	rec := scopedContractRecord{
		Schema:       scopedContractRecordSchema,
		Contract:     c,
		YAMLSHA256:   hex.EncodeToString(sum[:]),
		AnalyzerSHA:  analyzerSHA,
		UsageEntries: usageEntries,
	}
	recBytes, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		return FreehandBaseContract{}, err
	}
	// Record first, YAML second: a crash between the two leaves a record whose YAML hash does not match,
	// which loadBuiltFreehandBaseContract refuses, rather than a v2 YAML with no record (which persistence
	// would silently treat as v1).
	if err := writeFileAtomic(scopedContractRecordPath(absProjectDir), recBytes); err != nil {
		return FreehandBaseContract{}, fmt.Errorf("write scoped contract record: %w", err)
	}
	if err := writeFileAtomic(path, []byte(yaml)); err != nil {
		return FreehandBaseContract{}, fmt.Errorf("install scoped iOS dynamic interface: %w", err)
	}
	c.Path = path
	return c, nil
}

// loadBuiltFreehandBaseContract returns the v2 contract this build installed, or ok=false when the build
// used a v1 contract (no record). A record that does not describe the YAML on disk byte for byte is an
// error, never a fallback.
func loadBuiltFreehandBaseContract(projectDir string) (FreehandBaseContract, bool, error) {
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return FreehandBaseContract{}, false, err
	}
	raw, err := os.ReadFile(scopedContractRecordPath(absDir))
	if os.IsNotExist(err) {
		return FreehandBaseContract{}, false, nil
	}
	if err != nil {
		return FreehandBaseContract{}, false, err
	}
	var rec scopedContractRecord
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return FreehandBaseContract{}, false, fmt.Errorf("decode scoped contract record: %w", err)
	}
	c := rec.Contract
	if rec.Schema != scopedContractRecordSchema || !isScopedContractSchema(c.Schema) {
		return FreehandBaseContract{}, false, fmt.Errorf("scoped contract record has schema %q/%q", rec.Schema, c.Schema)
	}
	if got := freehandContractDigest(c); got != c.Digest {
		return FreehandBaseContract{}, false, fmt.Errorf("scoped contract record digest mismatch: %s != recomputed %s", short12(c.Digest), short12(got))
	}
	yaml, err := os.ReadFile(iosDynamicInterfacePath(absDir))
	if err != nil {
		return FreehandBaseContract{}, false, fmt.Errorf("read the scoped dynamic interface the build used: %w", err)
	}
	sum := sha256.Sum256(yaml)
	if hex.EncodeToString(sum[:]) != rec.YAMLSHA256 || string(yaml) != renderFreehandContractYAML(c) {
		return FreehandBaseContract{}, false, errors.New("the dynamic interface on disk is not the one the scoped contract record describes; it changed during the build")
	}
	c.Path = iosDynamicInterfacePath(absDir)
	return c, true, nil
}

// ---- usage computation ------------------------------------------------------------------------------

// interfaceUsageAnalyzer resolves the analyzer used for --interface-usage. SOROQ_INTERFACE_ANALYZER names
// one explicitly (for evaluating a new analyzer without rebuilding a frontend); otherwise it is the
// analyzer this build installed, i.e. the one the frontend declares.
func interfaceUsageAnalyzer(installed string) string {
	if p := strings.TrimSpace(os.Getenv("SOROQ_INTERFACE_ANALYZER")); p != "" {
		return p
	}
	return installed
}

// analyzerSupportsInterfaceUsage asks the analyzer, explicitly, whether it has the mode.
func analyzerSupportsInterfaceUsage(dart, analyzer string) bool {
	out, err := exec.Command(dart, analyzer, "--interface-usage-schema").Output()
	return err == nil && strings.TrimSpace(string(out)) == interfaceUsageSchema
}

// analyzerSupportsSDKExtendable asks the analyzer whether it can usage-scope the pure SDK's extendable
// surface (--interface-sdk-extendable). An analyzer that predates it answers with an error or other text.
func analyzerSupportsSDKExtendable(dart, analyzer string) bool {
	out, err := exec.Command(dart, analyzer, "--interface-sdk-extendable-schema").Output()
	return err == nil && strings.TrimSpace(string(out)) == sdkExtendableSchema
}

// consumers names the packages whose code counts as use (nil: every non-scoped package; used when only
// the library table is wanted).
func runInterfaceUsageAnalyzer(dart, analyzer, dill string, domain scopedDomain, consumers []string, sdkExtendable bool) (interfaceUsage, error) {
	outFile, err := os.CreateTemp("", "soroq-interface-usage-*.json")
	if err != nil {
		return interfaceUsage{}, err
	}
	outPath := outFile.Name()
	outFile.Close()
	defer os.Remove(outPath)
	args := []string{analyzer, "--interface-usage", dill, "--interface-usage-out", outPath}
	if len(domain.packages) > 0 {
		args = append(args, "--interface-scoped-packages", strings.Join(domain.packages, ","))
	}
	if consumers != nil {
		if len(consumers) == 0 {
			return interfaceUsage{}, errors.New("no consumer packages: the application's own package is always one")
		}
		args = append(args, "--interface-consumer-packages", strings.Join(consumers, ","))
	}
	if sdkExtendable {
		args = append(args, "--interface-sdk-extendable")
	}
	cmd := exec.Command(dart, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return interfaceUsage{}, fmt.Errorf("analyzer --interface-usage failed: %w\n%s", err, string(out))
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		return interfaceUsage{}, err
	}
	u, err := parseInterfaceUsage(raw, domain)
	if err != nil {
		return interfaceUsage{}, err
	}
	if sdkExtendable != (u.SDKExtendable != nil) {
		return interfaceUsage{}, fmt.Errorf("the analyzer was asked for sdk_extendable=%v but returned it=%v", sdkExtendable, u.SDKExtendable != nil)
	}
	if consumers != nil {
		got := append([]string(nil), u.ConsumerPackages...)
		sort.Strings(got)
		want := append([]string(nil), consumers...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return interfaceUsage{}, fmt.Errorf("the analyzer measured use by %v, not the requested consumers %v", got, want)
		}
	}
	return u, nil
}

// iosToolchainPlatformDill is the platform the BASE BUILD compiles against: the toolchain's own patched
// SDK (it carries Soroq's dart:_internal / dynamic_modules additions, which the frontend's does not).
func iosToolchainPlatformDill(toolchain string) (string, error) {
	bundle, err := iosCachedToolchainBundleDir(toolchain)
	if err != nil {
		return "", err
	}
	for _, rel := range []string{
		filepath.Join("out", iosLocalEngineTargetName, "flutter_patched_sdk", "platform_strong.dill"),
		filepath.Join("out", iosLocalEngineTargetName, "flutter_patched_sdk_product", "platform_strong.dill"),
		filepath.Join("out", iosLocalEngineHostName, "flutter_patched_sdk", "platform_strong.dill"),
	} {
		if p := filepath.Join(bundle, rel); fileExists(p) {
			return p, nil
		}
	}
	// A freshly installed toolchain has not had its local-engine layout materialized yet (that happens in
	// the build this contract is derived for). The layout's patched SDK platform IS the bundle's own
	// `platform_strong` artifact (materializeIOSSoroqPatchedSDK links it in), so use it directly --
	// hash-checked against engine.json exactly as the materializer checks it.
	flat := filepath.Join(bundle, "platform_strong")
	if fileExists(flat) {
		declared, err := iosToolchainDeclaredArtifactSHA(bundle, "platform_strong")
		if err != nil {
			return "", err
		}
		got, _, err := sha256OfFile(flat)
		if err != nil {
			return "", err
		}
		if declared == "" {
			return "", fmt.Errorf("toolchain %s engine.json declares no platform_strong artifact", bundle)
		}
		if !strings.EqualFold(got, declared) {
			return "", fmt.Errorf("toolchain %s platform_strong is %s but engine.json declares %s", bundle, short12(got), short12(declared))
		}
		return flat, nil
	}
	return "", fmt.Errorf("no flutter_patched_sdk platform_strong.dill in toolchain %s", bundle)
}

// buildDartDefines extracts --dart-define values from Flutter passthrough so the usage kernel sees the
// same environment constants. (A mismatch can only make the usage set differ, and a missed use is a
// refusal at patch time, never a crash; this is fidelity, not safety.)
func buildDartDefines(passthrough []string) []string {
	var out []string
	for i := 0; i < len(passthrough); i++ {
		a := passthrough[i]
		switch {
		case strings.HasPrefix(a, "--dart-define="):
			out = append(out, strings.TrimPrefix(a, "--dart-define="))
		case a == "--dart-define" && i+1 < len(passthrough):
			out = append(out, passthrough[i+1])
			i++
		}
	}
	return out
}

// compileInterfaceUsageKernel compiles a NON-AOT kernel of the build entrypoint against the toolchain
// platform with the frontend's gen_kernel. Nothing is tree-shaken, so every use the base code makes is
// visible.
func compileInterfaceUsageKernel(projectDir, flutterRoot, toolchain, entryRel string, defines []string, outPath string) error {
	platform, err := iosToolchainPlatformDill(toolchain)
	if err != nil {
		return err
	}
	aot := flutterDartAotRuntime(flutterRoot)
	genKernel := flutterGenKernelSnapshot(flutterRoot)
	for _, p := range []string{aot, genKernel} {
		if !fileExists(p) {
			return fmt.Errorf("usage-kernel compiler input missing: %s", p)
		}
	}
	entry := entryRel
	if !filepath.IsAbs(entry) {
		entry = filepath.Join(projectDir, entryRel)
	}
	args := []string{
		"--disable-dart-dev", genKernel,
		"--target", "flutter",
		"--platform", platform,
		"--packages", filepath.Join(projectDir, ".dart_tool", "package_config.json"),
		"--no-aot", "--no-embed-sources",
		"-Ddart.vm.profile=true", "-Ddart.vm.product=false",
		"--output", outPath,
	}
	for _, d := range defines {
		args = append(args, "-D"+d)
	}
	args = append(args, entry)
	cmd := exec.Command(aot, args...)
	cmd.Dir = projectDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("compile the interface-usage kernel: %w\n%s", err, string(out))
	}
	if !fileExists(outPath) {
		return fmt.Errorf("gen_kernel produced no usage kernel at %s", outPath)
	}
	return nil
}

// scopedContractDisabled is the explicit escape hatch back to the v1 whole-library contract.
func scopedContractDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SOROQ_IOS_BASE_CONTRACT")), "v1")
}

// upgradeFreehandBaseContractToScoped replaces the v1 contract written earlier in the build with the
// usage-scoped v2 contract. It returns ok=false (leaving v1 in place, with a notice) only when v2 is
// explicitly disabled or the analyzer predates the mode. Once the analyzer claims the mode, every failure
// is an error: a partially computed usage set must never be built on.
func upgradeFreehandBaseContractToScoped(projectDir, flutterRoot, toolchain, installedAnalyzer, entryRel string, passthrough []string) (FreehandBaseContract, bool, error) {
	var zero FreehandBaseContract
	if scopedContractDisabled() {
		fmt.Fprintln(os.Stderr, "soroq contract: SOROQ_IOS_BASE_CONTRACT=v1 -> building a whole-library (v1) base")
		return zero, false, nil
	}
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return zero, false, err
	}
	dart := filepath.Join(flutterRoot, "bin", "cache", "dart-sdk", "bin", "dart")
	analyzer := interfaceUsageAnalyzer(installedAnalyzer)
	if !fileExists(dart) || !fileExists(analyzer) {
		return zero, false, fmt.Errorf("cannot compute the usage-scoped contract: dart %s / analyzer %s missing", dart, analyzer)
	}
	if !analyzerSupportsInterfaceUsage(dart, analyzer) {
		fmt.Fprintf(os.Stderr, "NOTICE: this frontend's analyzer (%s) predates usage-scoped base contracts; building a whole-library (%s) base, which retains the entire Flutter framework. A frontend carrying an analyzer with --interface-usage builds the smaller %s base.\n",
			filepath.Base(filepath.Dir(analyzer)), freehandContractSchema, freehandContractSchemaV2)
		return zero, false, nil
	}
	analyzerSHA, err := sha256OfPath(analyzer)
	if err != nil {
		return zero, false, err
	}
	tmp, err := os.MkdirTemp("", "soroq-interface-usage-")
	if err != nil {
		return zero, false, err
	}
	defer os.RemoveAll(tmp)
	dill := filepath.Join(tmp, "usage.dill")
	defines := append(flavorDartDefines(freehandBuildFlavor(passthrough)), buildDartDefines(passthrough)...)
	if err := compileInterfaceUsageKernel(absDir, flutterRoot, toolchain, entryRel, defines, dill); err != nil {
		return zero, false, err
	}
	pinned, err := flutterPinnedScopedPackages(absDir, flutterRoot)
	if err != nil {
		return zero, false, err
	}
	appLibs, depLibs, err := contractProjectLibraries(absDir)
	if err != nil {
		return zero, false, err
	}
	// Dependency packages are scoped by usage too, not kept whole. Measured on a real app (Campus,
	// Flutter 3.44.9): with dependencies whole the base still retained 3.7 MB of package:win32 (Windows
	// FFI bindings that can never run on iOS), 2.0 MB of package:image, 0.95 MB of package:pdf and more
	// that the app never calls -- the stock tree shaker drops all of it. Only the APPLICATION's use of a
	// dependency is what a patch module can need from the base: a patch that changes dependency code
	// carries that code in the module, and anything it then needs that the base does not expose is
	// refused at patch build time by the interface validation, never on a device.
	domain := newScopedDomain(append(pinned, dependencyPackagesOf(depLibs, appLibs)...))
	consumers := interfaceConsumerPackages(appLibs, depLibs, domain)
	usage, err := runInterfaceUsageAnalyzer(dart, analyzer, dill, domain, consumers, analyzerSupportsSDKExtendable(dart, analyzer))
	if err != nil {
		return zero, false, err
	}
	c, err := buildScopedFreehandBaseContract(appLibs, depLibs, domain, usage)
	if err != nil {
		return zero, false, err
	}
	c, err = installScopedFreehandBaseContract(absDir, c, analyzerSHA, len(usage.Entries))
	if err != nil {
		return zero, false, err
	}
	libs := map[string]bool{}
	for _, e := range usage.Entries {
		libs[e.Library] = true
	}
	sdkExt := 0
	if usage.SDKExtendable != nil {
		sdkExt = len(*usage.SDKExtendable)
	}
	fmt.Fprintf(os.Stderr, "soroq contract: %s (usage-scoped) -- %d SDK/Flutter declarations in %d declaring libraries (framework-pinned packages scoped too: %v), %d extendable pure-SDK classes, %d whole app/dependency libraries, digest %s\n",
		c.Schema, len(usage.Entries), len(libs), domain.packages, sdkExt, len(c.Libraries), short12(c.Digest))
	return c, true, nil
}

// deriveInterfaceValidationSpec renders the patch-side validation spec for a v2 base from its source
// kernel, using the same analyzer that computed the base's usage.
func deriveInterfaceValidationSpec(c FreehandBaseContract, flutterRoot, installedAnalyzer, sourceKernel string) (string, error) {
	dart := filepath.Join(flutterRoot, "bin", "cache", "dart-sdk", "bin", "dart")
	analyzer := interfaceUsageAnalyzer(installedAnalyzer)
	if !analyzerSupportsInterfaceUsage(dart, analyzer) {
		return "", fmt.Errorf("the analyzer %s cannot describe the base source kernel (--interface-usage), so a %s base's patch-side validation spec cannot be derived", analyzer, c.Schema)
	}
	table, err := runInterfaceUsageAnalyzer(dart, analyzer, sourceKernel, newScopedDomain(c.ScopedPackages), nil, false)
	if err != nil {
		return "", err
	}
	return renderInterfaceValidationYAML(c, table)
}

// interfaceConsumerPackages are the packages whose code a patch module can contain: the application and
// its eligible Dart-only dependencies (the contract's whole-library set). Their use of the scoped domain
// is what a patch needs; see interface_usage.dart for why Soroq's runtime and native-plugin code are not
// consumers. Generated (non-package) libraries are always consumers in the analyzer.
func interfaceConsumerPackages(appLibs, depLibs []string, domain scopedDomain) []string {
	set := map[string]bool{}
	for _, l := range append(append([]string(nil), appLibs...), depLibs...) {
		if pkg, _, ok := splitPackageURI(l); ok && !domain.isScoped(l) {
			set[pkg] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// flutterPinnedScopedPackages returns the packages the Flutter SDK pins EXACTLY in its own pubspec
// (characters, collection, material_color_utilities, meta, vector_math on 3.44) that the application
// does NOT declare as a direct dependency.
//
// These are framework surface under another name: the app never chose them, pub cannot resolve them to a
// version other than the SDK's pin, so they change only with the Flutter SDK -- which already requires a
// new base -- and are never carried by a dependency patch. v1 exposed them whole only because
// reachability walks through the framework's imports into them. A package the application DOES declare
// is its own surface and stays whole, exactly as before.
func flutterPinnedScopedPackages(projectDir, flutterRoot string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(flutterRoot, "packages", "flutter", "pubspec.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read the Flutter SDK's own pubspec to find its pinned packages: %w", err)
	}
	pinned := parseExactPinnedDependencies(string(raw))
	g, err := depgraph.Resolve(projectDir)
	if err != nil {
		return nil, fmt.Errorf("resolve the application's direct dependencies: %w", err)
	}
	direct := map[string]bool{}
	for _, r := range g.Roots {
		direct[r] = true
	}
	var out []string
	for _, p := range pinned {
		if !direct[p] && p != g.RootPackage {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

var exactPinRe = regexp.MustCompile(`^  ([a-z0-9_]+):\s*([0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+\-]*)\s*(#.*)?$`)

// parseExactPinnedDependencies returns the names under `dependencies:` whose constraint is an exact
// version (`name: 1.2.3`). SDK and ranged dependencies are not pins and are ignored.
func parseExactPinnedDependencies(pubspec string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(pubspec, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, " ") && strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			in = strings.TrimSpace(line) == "dependencies:"
			continue
		}
		if !in {
			continue
		}
		if m := exactPinRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// dependencyPackagesOf returns the package names of the dependency libraries that are not also
// application packages, sorted and de-duplicated. They join the scoped domain, so only the application's
// use of them is exposed to patch modules.
func dependencyPackagesOf(depLibs, appLibs []string) []string {
	app := map[string]bool{}
	for _, l := range appLibs {
		if pkg, _, ok := splitPackageURI(l); ok {
			app[pkg] = true
		}
	}
	set := map[string]bool{}
	for _, l := range depLibs {
		if pkg, _, ok := splitPackageURI(l); ok && !app[pkg] {
			set[pkg] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// writeConstEntries appends, under the current (callable) section, every public compile-time constant of
// the scoped libraries in the base source kernel. PATCH-SIDE ONLY: the base's own interface never lists
// them, so they cost the installed app nothing, while a patch that says `@override`, `Colors.black` or
// `Icons.add` is no longer refused for "using" something the base never needs to run.
func writeConstEntries(b *strings.Builder, libs []usageLibrary, present map[string]bool) {
	for _, l := range libs {
		if !l.Scoped || !present[l.URI] || !contractLibRe.MatchString(l.URI) {
			continue
		}
		for _, name := range l.Consts {
			cls, member, isStatic := strings.Cut(name, ".")
			if !isStatic {
				member, cls = cls, ""
			}
			if !contractNameRe.MatchString(member) || strings.HasPrefix(member, "_") ||
				(cls != "" && (!contractNameRe.MatchString(cls) || strings.HasPrefix(cls, "_"))) {
				continue
			}
			if cls == "" {
				fmt.Fprintf(b, "  - library: '%s'\n    member: '%s'\n", l.URI, member)
			} else {
				fmt.Fprintf(b, "  - library: '%s'\n    class: '%s'\n    member: '%s'\n", l.URI, cls, member)
			}
		}
	}
}

// soroqModuleSynthesizerEntries are the SDK declarations Soroq's generated patch-module source uses on its
// own behalf. Keep in sync with the synthesizer in tools/soroq_kernel_analyze/bin/analyze.dart.
var soroqModuleSynthesizerEntries = []ContractEntry{
	{Library: "dart:core", Kind: "class", Name: "pragma"},
}
