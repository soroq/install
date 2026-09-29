package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---- map parsing ------------------------------------------------------------------------------------

func TestDependencyMap_ParseDedupesAndSorts(t *testing.T) {
	raw := "inline\t1\tpackage:a/a.dart::B::g\tpackage:a/a.dart::A::f\n" +
		"result\t0\tpackage:a/a.dart::::main\tpackage:a/a.dart::A::f\n" +
		"inline\t1\tpackage:a/a.dart::B::g\tpackage:a/a.dart::A::f\n" // duplicate
	edges, err := parseFreehandDependencyMap([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("duplicates must be deduplicated, got %d edges: %+v", len(edges), edges)
	}
	want := "inline\t1\tpackage:a/a.dart::B::g\tpackage:a/a.dart::A::f\n" +
		"result\t0\tpackage:a/a.dart::::main\tpackage:a/a.dart::A::f\n"
	if got := string(renderFreehandDependencyMap(edges)); got != want {
		t.Fatalf("canonical form:\n%q\nwant\n%q", got, want)
	}
	// Canonical form is a fixed point.
	again, err := parseFreehandDependencyMap([]byte(want))
	if err != nil || string(renderFreehandDependencyMap(again)) != want {
		t.Fatalf("canonical form must re-parse to itself: %v", err)
	}
	// An empty map is zero bytes and zero edges.
	if e, err := parseFreehandDependencyMap(nil); err != nil || len(e) != 0 || len(renderFreehandDependencyMap(e)) != 0 {
		t.Fatalf("empty map: %v %v", e, err)
	}
}

func TestDependencyMap_MalformedIsRefused(t *testing.T) {
	const a, b = "package:a/a.dart::A::f", "package:a/a.dart::B::g"
	for name, raw := range map[string]string{
		"three fields":        "inline\t1\t" + a + "\n",
		"five fields":         "inline\t1\t" + b + "\t" + a + "\tx\n",
		"bad why":             "inlined\t1\t" + b + "\t" + a + "\n",
		"bad flag":            "inline\t2\t" + b + "\t" + a + "\n",
		"empty flag":          "inline\t\t" + b + "\t" + a + "\n",
		"no trailing newline": "inline\t1\t" + b + "\t" + a,
		"carriage return":     "inline\t1\t" + b + "\t" + a + "\r\n",
		"blank line":          "inline\t1\t" + b + "\t" + a + "\n\n",
		"two-segment callee":  "inline\t1\t" + b + "\tpackage:a/a.dart::f\n",
		"empty member":        "inline\t1\t" + b + "\tpackage:a/a.dart::A::\n",
		"empty library":       "inline\t1\t" + b + "\t::A::f\n",
		"space in identity":   "inline\t1\t" + b + "\tpackage:a/a.dart::A::f g\n",
		"self edge":           "inline\t1\t" + a + "\t" + a + "\n",
		"conflicting caller flag": "inline\t1\t" + b + "\t" + a + "\n" +
			"result\t0\t" + b + "\tpackage:a/a.dart::C::h\n",
	} {
		if _, err := parseFreehandDependencyMap([]byte(raw)); err == nil {
			t.Errorf("%s: a malformed map was accepted", name)
		}
	}
}

func TestDependencyMap_IdentitiesMustAgreeWithManifest(t *testing.T) {
	manifest := []byte("pkg::A::f\npkg::B::g\n")
	ok := []freehandDependencyEdge{
		{Why: "inline", CallerPatchable: true, Caller: "pkg::B::g", Callee: "pkg::A::f"},
		{Why: "result", CallerPatchable: false, Caller: "pkg::C::h", Callee: "pkg::A::f"},
	}
	if err := checkDependencyMapAgainstManifest(ok, manifest); err != nil {
		t.Fatalf("an agreeing map was refused: %v", err)
	}
	for name, e := range map[string]freehandDependencyEdge{
		"callee not in manifest":               {Why: "inline", CallerPatchable: true, Caller: "pkg::B::g", Callee: "pkg::Z::z"},
		"patchable caller not in manifest":     {Why: "inline", CallerPatchable: true, Caller: "pkg::Z::z", Callee: "pkg::A::f"},
		"unpatchable caller that IS patchable": {Why: "inline", CallerPatchable: false, Caller: "pkg::B::g", Callee: "pkg::A::f"},
	} {
		if err := checkDependencyMapAgainstManifest([]freehandDependencyEdge{e}, manifest); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// ---- closure --------------------------------------------------------------------------------------

func depEdges(t *testing.T, lines ...string) []freehandDependencyEdge {
	t.Helper()
	e, err := parseFreehandDependencyMap([]byte(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func callerIDs(cs []FreehandDependencyMapCaller) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Identity)
	}
	return out
}

func TestDependencyClosure_IsTransitive(t *testing.T) {
	edges := depEdges(t,
		"inline\t1\tp::B::b\tp::A::a",
		"result\t1\tp::C::c\tp::B::b",
		"inline\t1\tp::D::d\tp::C::c",
		"inline\t1\tp::X::x\tp::Y::y", // unrelated
	)
	added, refusals := freehandDependencyClosure(edges, []string{"p::A::a"})
	if len(refusals) != 0 {
		t.Fatal(refusals)
	}
	if got := callerIDs(added); !reflect.DeepEqual(got, []string{"p::B::b", "p::C::c", "p::D::d"}) {
		t.Fatalf("closure = %v", got)
	}
	if added[1].Absorbed[0] != (FreehandDependencyAbsorb{Why: "result", Callee: "p::B::b"}) {
		t.Fatalf("the edge that pulled C in is not recorded: %+v", added[1])
	}
	// A caller that is itself changed is not "added".
	added, _ = freehandDependencyClosure(edges, []string{"p::A::a", "p::C::c"})
	if got := callerIDs(added); !reflect.DeepEqual(got, []string{"p::B::b", "p::D::d"}) {
		t.Fatalf("closure with C changed = %v", got)
	}
	// Nothing absorbed the changed code: nothing added.
	if added, r := freehandDependencyClosure(edges, []string{"p::D::d"}); len(added) != 0 || len(r) != 0 {
		t.Fatalf("a changed leaf must add nothing: %v %v", added, r)
	}
}

func TestDependencyClosure_TerminatesOnCycles(t *testing.T) {
	edges := depEdges(t,
		"inline\t1\tp::B::b\tp::A::a",
		"inline\t1\tp::A::a\tp::B::b",
		"inline\t1\tp::C::c\tp::B::b",
		"inline\t1\tp::B::b\tp::C::c",
	)
	added, refusals := freehandDependencyClosure(edges, []string{"p::A::a"})
	if len(refusals) != 0 {
		t.Fatal(refusals)
	}
	if got := callerIDs(added); !reflect.DeepEqual(got, []string{"p::B::b", "p::C::c"}) {
		t.Fatalf("closure over a cycle = %v", got)
	}
	// B absorbed both A and C: both edges are recorded.
	if len(added[0].Absorbed) != 2 {
		t.Fatalf("B's absorbed edges = %+v", added[0].Absorbed)
	}
}

func TestDependencyClosure_NonPatchableCallerRefuses(t *testing.T) {
	edges := depEdges(t,
		"inline\t1\tp::B::b\tp::A::a",
		"inline\t0\tpackage:flutter/src/x.dart::W::build\tp::B::b",
		"result\t0\tp::::_helper@123\tp::A::a",
	)
	_, refusals := freehandDependencyClosure(edges, []string{"p::A::a"})
	want := []string{
		"p::A::a's result was folded into p::::_helper@123, which OTA cannot replace; ship a store release",
		"p::B::b was inlined into package:flutter/src/x.dart::W::build, which OTA cannot replace; ship a store release (reached from the changed p::A::a)",
	}
	if !reflect.DeepEqual(refusals, want) {
		t.Fatalf("refusals:\n%s\nwant\n%s", strings.Join(refusals, "\n"), strings.Join(want, "\n"))
	}
}

// ---- baseline binding -----------------------------------------------------------------------------

const depMapTestLib = "package:app/main.dart"

func depMapManifest() string {
	return depMapTestLib + "::A::f\n" + depMapTestLib + "::B::g\n" + depMapTestLib + "::C::h\n" + depMapTestLib + "::K::K.\n"
}

func depMapCanonical(t *testing.T, lines ...string) *freehandDependencyMapCapture {
	t.Helper()
	if len(lines) == 0 {
		return &freehandDependencyMapCapture{Canonical: []byte{}, Edges: 0}
	}
	edges := depEdges(t, lines...)
	return &freehandDependencyMapCapture{Canonical: renderFreehandDependencyMap(edges), Edges: len(edges)}
}

// seedDepMapHome installs a fixture engine bundle for fullMeta().EngineRev declaring (or not) the
// dependency-map capability, so persistFreehandBaseline derives it exactly as a release would.
func seedDepMapHome(t *testing.T, withCapability bool) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	caps := `"private_enclosing_class_identity_v1"`
	if withCapability {
		caps += `,"` + freehandDependencyMapCapability + `"`
	}
	writeEngineBundle(t, filepath.Join(home, ".soroq", "toolchains"), "tc-r8", fullMeta().EngineRev,
		`{"honoured_kinds":["function","method","static-method","getter","setter","operator"],"identity_capabilities":[`+caps+`]}`)
}

func seedDepMapFixture(t *testing.T) (proj, dill, srcDill, man, graph string) {
	t.Helper()
	proj, dill, srcDill, _, graph = seedFixture(t)
	man = writeTmp(t, proj, "manifest.txt", depMapManifest())
	return
}

func TestDependencyMapBaseline_CapabilityRequiresTheMap(t *testing.T) {
	seedDepMapHome(t, true)
	proj, dill, srcDill, man, graph := seedDepMapFixture(t)
	if _, err := persistFreehandBaselineWithDependencyMap(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "", nil, nil); err == nil ||
		!strings.Contains(err.Error(), freehandDependencyMapCapability) {
		t.Fatalf("a base whose engine declares the capability must not persist without its map: %v", err)
	}
	dm := depMapCanonical(t, "inline\t1\t"+depMapTestLib+"::B::g\t"+depMapTestLib+"::A::f")
	relDir, err := persistFreehandBaselineWithDependencyMap(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "", nil, dm)
	if err != nil {
		t.Fatal(err)
	}
	m, err := verifyExistingBaseline(relDir)
	if err != nil {
		t.Fatalf("a fresh R8 baseline must verify: %v", err)
	}
	if m.DependencyMapSchema != freehandDependencyMapSchema || m.DependencyMapSHA256 != freehandSHA256Bytes(dm.Canonical) || m.DependencyMapEdges != 1 {
		t.Fatalf("binding not recorded: %+v", m)
	}
	onDisk, _ := os.ReadFile(filepath.Join(relDir, freehandDependencyMapFile))
	if !bytes.Equal(onDisk, dm.Canonical) {
		t.Fatal("the persisted map is not the canonical capture")
	}
	// Idempotent for the identical map; a different map under the same runtime id is a different base.
	if _, err := persistFreehandBaselineWithDependencyMap(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "", nil, dm); err != nil {
		t.Fatalf("identical re-persist must be idempotent: %v", err)
	}
	other := depMapCanonical(t, "inline\t1\t"+depMapTestLib+"::C::h\t"+depMapTestLib+"::A::f")
	if _, err := persistFreehandBaselineWithDependencyMap(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "", nil, other); err == nil {
		t.Fatal("a different dependency map is a different immutable base")
	}
	// Tamper (even a self-consistent duplicate line) and removal are detected.
	p := filepath.Join(relDir, freehandDependencyMapFile)
	os.WriteFile(p, append(append([]byte{}, dm.Canonical...), dm.Canonical...), 0o600)
	if _, err := verifyExistingBaseline(relDir); err == nil {
		t.Fatal("a tampered map must fail verification")
	}
	os.Remove(p)
	if _, err := verifyExistingBaseline(relDir); err == nil {
		t.Fatal("an R8 baseline without its map must fail verification")
	}
}

func TestDependencyMapBaseline_EmptyMapIsAValidRecord(t *testing.T) {
	seedDepMapHome(t, true)
	proj, dill, srcDill, man, graph := seedDepMapFixture(t)
	relDir, err := persistFreehandBaselineWithDependencyMap(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "", nil, depMapCanonical(t))
	if err != nil {
		t.Fatal(err)
	}
	m, err := verifyExistingBaseline(relDir)
	if err != nil {
		t.Fatal(err)
	}
	if m.DependencyMapSchema != freehandDependencyMapSchema || m.DependencyMapSHA256 != freehandSHA256Bytes(nil) {
		t.Fatalf("empty map binding: %+v", m)
	}
	if fi, err := os.Stat(filepath.Join(relDir, freehandDependencyMapFile)); err != nil || fi.Size() != 0 {
		t.Fatalf("the empty map must be persisted as a zero-byte file: %v", err)
	}
}

func TestDependencyMapBaseline_RefusesAMapTheManifestDisagreesWith(t *testing.T) {
	seedDepMapHome(t, true)
	proj, dill, srcDill, man, graph := seedDepMapFixture(t)
	dm := depMapCanonical(t, "inline\t1\t"+depMapTestLib+"::B::g\tpackage:app/other.dart::Z::z")
	if _, err := persistFreehandBaselineWithDependencyMap(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "", nil, dm); err == nil ||
		!strings.Contains(err.Error(), "not in the patchable manifest") {
		t.Fatalf("a callee outside the manifest means engine and analyzer disagree: %v", err)
	}
}

func TestDependencyMapBaseline_NoCapabilityForbidsTheMap(t *testing.T) {
	seedDepMapHome(t, false)
	proj, dill, srcDill, man, graph := seedDepMapFixture(t)
	dm := depMapCanonical(t, "inline\t1\t"+depMapTestLib+"::B::g\t"+depMapTestLib+"::A::f")
	if _, err := persistFreehandBaselineWithDependencyMap(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "", nil, dm); err == nil {
		t.Fatal("a map from an engine that never declared the capability has no authority")
	}
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	var probe map[string]json.RawMessage
	_ = json.Unmarshal(raw, &probe)
	for _, k := range []string{"dependency_map_schema", "dependency_map_sha256", "dependency_map_edges"} {
		if _, ok := probe[k]; ok {
			t.Fatalf("a baseline without the capability must not gain %q", k)
		}
	}
	if _, err := verifyExistingBaseline(relDir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(relDir, freehandDependencyMapFile), dm.Canonical, 0o600)
	if _, err := verifyExistingBaseline(relDir); err == nil {
		t.Fatal("a stray dependency map beside a baseline without the capability must be refused, not read")
	}
}

// ---- patch-side expansion -------------------------------------------------------------------------

func depKey(kind, cls, member string) string {
	return "v1|" + depMapTestLib + "|" + kind + "|" + cls + "|" + member + "|0000abcd"
}

// r8Base persists an R8 base carrying the given map and returns its verified record.
func r8Base(t *testing.T, lines ...string) (string, *FreehandBaselineMeta) {
	t.Helper()
	seedDepMapHome(t, true)
	proj, dill, srcDill, man, graph := seedDepMapFixture(t)
	relDir, err := persistFreehandBaselineWithDependencyMap(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "", nil, depMapCanonical(t, lines...))
	if err != nil {
		t.Fatal(err)
	}
	m, err := verifyExistingBaseline(relDir)
	if err != nil {
		t.Fatal(err)
	}
	return relDir, m
}

// writeDiff writes a freehand_diff.json with one changed patchable declaration (A.f) plus a field the Go
// struct does not model, and returns the parsed report.
func writeDiff(t *testing.T) (string, *FreehandDiffReport, []byte) {
	t.Helper()
	doc := map[string]any{
		"schema": "soroq.freehand.diff.v1", "identitySchema": freehandIdentitySchema,
		"supported": true, "noOp": false, "blockers": []any{},
		"changed": []any{map[string]any{"key": depKey("method", "A", "f"), "kind": "method",
			"manifestLine": depMapTestLib + "::A::f", "bodyDigest": "bd", "patchable": true}},
		"changedPatchable": []any{depMapTestLib + "::A::f"},
		"newCodeClosure":   []any{}, "signatureChanged": []any{}, "deleted": []any{},
		"unsupportedChanged": []any{}, "unresolvedEdges": []any{},
		"shapeChanged": []any{}, // not modelled by FreehandDiffReport; must survive the rewrite
		"counts":       map[string]any{"changed": 1, "changedPatchable": 1},
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	p := filepath.Join(t.TempDir(), "freehand_diff.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var rep FreehandDiffReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	return p, &rep, raw
}

// fakeSymbols is a source-kernel symbol graph for depMapTestLib: every manifest identity as a patchable
// symbol with the key depKey gives it, plus class B's field (optionally retyped, to plant a layout change).
func fakeSymbols(fieldType string) []freehandKernelSymbol {
	sym := func(kind, cls, member, vm string, patchable bool) freehandKernelSymbol {
		return freehandKernelSymbol{Key: depKey(kind, cls, member), LibURI: depMapTestLib, Class: cls, Member: member,
			Kind: kind, SignatureDigest: "0000abcd", ManifestLine: depMapTestLib + "::" + cls + "::" + vm, Patchable: patchable}
	}
	f := sym("field-initializer", "B", "count", "init:count", false)
	f.SignatureDigest = fieldType
	return []freehandKernelSymbol{
		sym("method", "A", "f", "f", true),
		sym("method", "B", "g", "g", true),
		sym("method", "C", "h", "h", true),
		sym("constructor", "K", "", "K.", true),
		f,
	}
}

type kernelCalls struct{ candidate, base int }

func fakeKernels(calls *kernelCalls, baseField, candField string) freehandExpansionKernels {
	return freehandExpansionKernels{
		Candidate: func() ([]freehandKernelSymbol, error) { calls.candidate++; return fakeSymbols(candField), nil },
		Base:      func() ([]freehandKernelSymbol, error) { calls.base++; return fakeSymbols(baseField), nil },
	}
}

func TestDependencyExpansion_ReachesModuleGeneration(t *testing.T) {
	relDir, base := r8Base(t,
		"inline\t1\t"+depMapTestLib+"::B::g\t"+depMapTestLib+"::A::f",
		"result\t1\t"+depMapTestLib+"::C::h\t"+depMapTestLib+"::B::g",
	)
	diffPath, rep, _ := writeDiff(t)
	var calls kernelCalls
	added, err := expandFreehandChangedSetOverDependencyMap(base, relDir, rep, diffPath, fakeKernels(&calls, "int", "int"))
	if err != nil {
		t.Fatal(err)
	}
	if calls.candidate != 1 || calls.base != 1 || !reflect.DeepEqual(callerIDs(added), []string{depMapTestLib + "::B::g", depMapTestLib + "::C::h"}) {
		t.Fatalf("expansion = %+v (kernel analyses %+v)", added, calls)
	}
	wantPatchable := []string{depMapTestLib + "::A::f", depMapTestLib + "::B::g", depMapTestLib + "::C::h"}
	if !reflect.DeepEqual(rep.ChangedPatchable, wantPatchable) {
		t.Fatalf("changedPatchable = %v", rep.ChangedPatchable)
	}
	// EVERY gate reads the changed set through changedDeclsFromDiff: the capability gate, the fold
	// check, and the ABI bijection. The expanded callers must be there, as ordinary patchable decls.
	decls, err := changedDeclsFromDiff(rep.Changed)
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 3 {
		t.Fatalf("gates would see %d decls, want 3: %+v", len(decls), decls)
	}

	// THE SYNTHESIZER'S INPUT. `soroq_kernel_analyze --synthesize --diff-json <path>` reads exactly this
	// file and extracts every `changed` entry with patchable=true (key/kind/manifestLine). It must now
	// carry the callers, and keep every analyzer field the Go struct does not model.
	onDisk, err := os.ReadFile(diffPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(onDisk, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["shapeChanged"]; !ok {
		t.Fatal("the rewrite dropped an analyzer field")
	}
	var synthChanged []map[string]any
	if err := json.Unmarshal(doc["changed"], &synthChanged); err != nil {
		t.Fatal(err)
	}
	gotKeys := []string{}
	for _, c := range synthChanged {
		if c["patchable"] == true {
			gotKeys = append(gotKeys, c["key"].(string))
		}
	}
	wantKeys := []string{depKey("method", "A", "f"), depKey("method", "B", "g"), depKey("method", "C", "h")}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("the synthesizer would extract %v, want %v", gotKeys, wantKeys)
	}

	// The module the synthesizer returns must REDIRECT the callers too: an ABI covering only the
	// originally changed declaration is refused, one covering all three is accepted.
	decl := func(cls, m string) abiDecl {
		return abiDecl{manifestLine: depMapTestLib + "::" + cls + "::" + m, stableKey: depKey("method", cls, m),
			class: cls, member: m, kind: "instance-member"}
	}
	onlyChanged := t.TempDir()
	buildFreehandArtifactFrom(t, onlyChanged, []abiDecl{decl("A", "f")})
	mOnly, _ := readArtifactManifestAndDiff(t, onlyChanged)
	if _, err := parseAndValidateModuleManifest(mOnly, rep.Changed, testDependencyDescriptor().DescriptorDigest); err == nil ||
		!strings.Contains(err.Error(), "has no replacement_abi entry") {
		t.Fatalf("a module that does not replace the absorbing callers must be refused: %v", err)
	}
	full := t.TempDir()
	buildFreehandArtifactFrom(t, full, []abiDecl{decl("A", "f"), decl("B", "g"), decl("C", "h")})
	mFull, _ := readArtifactManifestAndDiff(t, full)
	if _, err := parseAndValidateModuleManifest(mFull, rep.Changed, testDependencyDescriptor().DescriptorDigest); err != nil {
		t.Fatalf("a module replacing changed + absorbing callers must validate: %v", err)
	}

	// The plan records why each caller is there, and that record changes the plan digest.
	plan := FreehandPatchPlan{Diff: rep, DependencyMapCallers: added}
	pb, _ := json.Marshal(plan)
	if !strings.Contains(string(pb), `"dependency_map_callers"`) {
		t.Fatal("the plan must record the expansion")
	}
}

func TestDependencyExpansion_CallersFaceTheCapabilityGate(t *testing.T) {
	// K's constructor absorbed A.f; this base's engine does not honour constructor redirects, so the
	// expanded caller -- not just the changed function -- must be refused by the same gate.
	relDir, base := r8Base(t, "inline\t1\t"+depMapTestLib+"::K::K.\t"+depMapTestLib+"::A::f")
	diffPath, rep, _ := writeDiff(t)
	var calls kernelCalls
	if _, err := expandFreehandChangedSetOverDependencyMap(base, relDir, rep, diffPath, fakeKernels(&calls, "int", "int")); err != nil {
		t.Fatal(err)
	}
	decls, err := changedDeclsFromDiff(rep.Changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := assertFreehandRedirectCapability(base, decls); err == nil || !strings.Contains(err.Error(), "K::K.") {
		t.Fatalf("an expanded constructor caller must face the capability gate: %v", err)
	}
	// And the fold check sees it: with no object graph in the fixture base the check fails closed
	// for the WHOLE expanded set rather than silently skipping the callers.
	if err := assertFreehandNoFoldedValueForBase(base, relDir, decls); err == nil {
		t.Fatal("the fold check must run over the expanded set (and fail closed without evidence)")
	}
}

func TestDependencyExpansion_NonPatchableAbsorberRefusesThePatch(t *testing.T) {
	relDir, base := r8Base(t,
		"inline\t1\t"+depMapTestLib+"::B::g\t"+depMapTestLib+"::A::f",
		"inline\t0\tpackage:flutter/src/widgets/framework.dart::State::build\t"+depMapTestLib+"::B::g",
	)
	diffPath, rep, before := writeDiff(t)
	var calls kernelCalls
	_, err := expandFreehandChangedSetOverDependencyMap(base, relDir, rep, diffPath, fakeKernels(&calls, "int", "int"))
	if err == nil {
		t.Fatal("a change absorbed by non-patchable code must be refused")
	}
	want := depMapTestLib + "::B::g was inlined into package:flutter/src/widgets/framework.dart::State::build, which OTA cannot replace; ship a store release (reached from the changed " + depMapTestLib + "::A::f)"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal must name the changed function and the absorbing code:\n%v", err)
	}
	if calls != (kernelCalls{}) {
		t.Fatal("nothing may be analyzed or compiled for a refused patch")
	}
	after, _ := os.ReadFile(diffPath)
	if !bytes.Equal(before, after) {
		t.Fatal("a refused expansion must leave the diff untouched")
	}
}

func TestDependencyExpansion_UnresolvableCallerFailsClosed(t *testing.T) {
	relDir, base := r8Base(t, "inline\t1\t"+depMapTestLib+"::B::g\t"+depMapTestLib+"::A::f")
	diffPath, rep, _ := writeDiff(t)
	otherLib := func() ([]freehandKernelSymbol, error) {
		syms := fakeSymbols("int")
		syms[1].Key = "v1|package:other/x.dart|method|B|g|0"
		return syms, nil
	}
	none := func() ([]freehandKernelSymbol, error) { return nil, nil }
	notPatchable := func() ([]freehandKernelSymbol, error) {
		syms := fakeSymbols("int")
		syms[1].Patchable = false
		return syms, nil
	}
	for name, cand := range map[string]func() ([]freehandKernelSymbol, error){
		"key naming another library": otherLib, "caller absent": none, "caller not patchable": notPatchable,
	} {
		if _, err := expandFreehandChangedSetOverDependencyMap(base, relDir, rep, diffPath, freehandExpansionKernels{Candidate: cand, Base: none}); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

func TestDependencyExpansion_ReceiverCallerRefusedWhenAClassLayoutMoved(t *testing.T) {
	relDir, base := r8Base(t, "inline\t1\t"+depMapTestLib+"::B::g\t"+depMapTestLib+"::A::f")
	diffPath, rep, before := writeDiff(t)
	var calls kernelCalls
	_, err := expandFreehandChangedSetOverDependencyMap(base, relDir, rep, diffPath, fakeKernels(&calls, "int", "String"))
	if err == nil || !strings.Contains(err.Error(), depMapTestLib+"::B") || !strings.Contains(err.Error(), "base receiver") {
		t.Fatalf("an added instance-member caller over a moved class layout must be refused: %v", err)
	}
	after, _ := os.ReadFile(diffPath)
	if !bytes.Equal(before, after) {
		t.Fatal("a refused expansion must leave the diff untouched")
	}
}

func TestDependencyExpansion_BaseWithoutCapabilityIsUntouched(t *testing.T) {
	seedDepMapHome(t, false)
	proj, dill, srcDill, man, graph := seedDepMapFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "")
	if err != nil {
		t.Fatal(err)
	}
	base, err := verifyExistingBaseline(relDir)
	if err != nil {
		t.Fatal(err)
	}
	diffPath, rep, before := writeDiff(t)
	var repBefore FreehandDiffReport
	_ = json.Unmarshal(before, &repBefore)
	var calls kernelCalls
	added, err := expandFreehandChangedSetOverDependencyMap(base, relDir, rep, diffPath, fakeKernels(&calls, "int", "String"))
	if err != nil || added != nil || calls != (kernelCalls{}) {
		t.Fatalf("a base without the capability must not expand: %v %v %+v", added, err, calls)
	}
	after, _ := os.ReadFile(diffPath)
	if !bytes.Equal(before, after) {
		t.Fatal("the diff must stay byte-for-byte what the analyzer wrote")
	}
	if !reflect.DeepEqual(*rep, repBefore) {
		t.Fatal("the in-memory report must be untouched")
	}
	// The plan serializes exactly as before: no new key.
	pb, _ := json.Marshal(FreehandPatchPlan{Diff: rep})
	if strings.Contains(string(pb), "dependency_map") {
		t.Fatalf("a plan against a base without the capability gained a field: %s", pb)
	}
}

func TestDependencyExpansion_R8BaseWithNoAbsorbersIsUntouched(t *testing.T) {
	relDir, base := r8Base(t, "inline\t1\t"+depMapTestLib+"::C::h\t"+depMapTestLib+"::B::g")
	diffPath, rep, before := writeDiff(t) // only A.f changed; nothing absorbed it
	var calls kernelCalls
	added, err := expandFreehandChangedSetOverDependencyMap(base, relDir, rep, diffPath, fakeKernels(&calls, "int", "String"))
	if err != nil || added != nil || calls != (kernelCalls{}) {
		t.Fatalf("%v %v %+v", added, err, calls)
	}
	after, _ := os.ReadFile(diffPath)
	if !bytes.Equal(before, after) {
		t.Fatal("nothing absorbed the change: the diff must be untouched")
	}
}

// ---- candidate identity resolution -------------------------------------------------------------------

func TestResolveIdentitiesFromSymbolGraph(t *testing.T) {
	graph := `{"schema":"soroq.freehand.identity.v1","symbols":[
	 {"key":"v1|L|method|B|g|1","kind":"method","manifestLine":"L::B::g","patchable":true},
	 {"key":"v1|L|method|C|h|1","kind":"method","manifestLine":"L::C::h","patchable":false},
	 {"key":"v1|L|getter|D|x|1","kind":"getter","manifestLine":"L::D::get:x","patchable":true},
	 {"key":"v1|L|getter|D|x|2","kind":"getter","manifestLine":"L::D::get:x","patchable":true}]}`
	syms, err := parseFreehandSymbolGraph([]byte(graph))
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveIdentities(syms, []string{"L::B::g"})
	if err != nil || got["L::B::g"].Key != "v1|L|method|B|g|1" {
		t.Fatalf("%v %v", got, err)
	}
	for _, l := range []string{"L::C::h", "L::D::get:x", "L::Z::z"} {
		if _, err := resolveIdentities(syms, []string{l}); err == nil {
			t.Errorf("%s must not resolve (non-patchable, ambiguous or absent)", l)
		}
	}
	if _, err := parseFreehandSymbolGraph([]byte(`{"schema":"other","symbols":[]}`)); err == nil {
		t.Error("a symbol graph of another schema must be refused")
	}
}

// ---- release side ---------------------------------------------------------------------------------

func TestDependencyMapRelease_EngineDeclaration(t *testing.T) {
	dir := t.TempDir()
	write := func(doc string) string {
		p := filepath.Join(dir, "engine.json")
		os.WriteFile(p, []byte(doc), 0o600)
		return p
	}
	if ok, err := engineBundleDeclaresDependencyMap(write(`{"soroq_engine_revision":"x"}`)); ok || err != nil {
		t.Fatalf("no declaration: %v %v", ok, err)
	}
	if ok, err := engineBundleDeclaresDependencyMap(write(`{"soroq_freehand_redirect_capabilities":{"honoured_kinds":["method"],"identity_capabilities":["private_enclosing_class_identity_v1"]}}`)); ok || err != nil {
		t.Fatalf("R7-shaped declaration: %v %v", ok, err)
	}
	if ok, err := engineBundleDeclaresDependencyMap(write(`{"soroq_freehand_redirect_capabilities":{"honoured_kinds":["method"],"identity_capabilities":["soroq_dependency_map_v1"]}}`)); !ok || err != nil {
		t.Fatalf("R8 declaration: %v %v", ok, err)
	}
	if _, err := engineBundleDeclaresDependencyMap(write(`{"soroq_freehand_redirect_capabilities":{"honoured_kinds":["method"],"identity_capabilities":["soroq_dependency_map_v2"]}}`)); err == nil {
		t.Fatal("an unknown capability is a malformed declaration")
	}
}

func TestDependencyMapRelease_PrepareRemovesStaleMapAndRefusesUserFlag(t *testing.T) {
	proj := t.TempDir()
	p, err := prepareFreehandDependencyMap(proj)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p) || !strings.HasPrefix(p, filepath.Join(proj, ".soroq", "build")) {
		t.Fatalf("map path %q must be absolute under the project's .soroq/build", p)
	}
	os.WriteFile(p, []byte("stale\n"), 0o600)
	if _, err := prepareFreehandDependencyMap(proj); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("a stale map must be removed before the build")
	}
	if !freehandUserSetsDependencyMap([]string{"--extra-gen-snapshot-options=--soroq_dependency_map=/x"}) {
		t.Fatal("a developer-supplied flag must be detected")
	}
	if freehandUserSetsDependencyMap([]string{"--extra-gen-snapshot-options=--write_v8_snapshot_profile_to=/x"}) {
		t.Fatal("false positive")
	}
	comma := filepath.Join(t.TempDir(), "a,b")
	os.MkdirAll(comma, 0o755)
	if _, err := freehandDependencyMapBuildPath(comma); err == nil {
		t.Fatal("a path with a comma cannot be passed through --extra-gen-snapshot-options")
	}
}

func writeBuildLog(t *testing.T, proj, content string) {
	t.Helper()
	dir := filepath.Join(proj, ".soroq", "logs")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, time.Now().UTC().Format("20060102T150405.000000000Z")+"-ios-build.log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDependencyMapRelease_Collect(t *testing.T) {
	proj := t.TempDir()
	p, err := prepareFreehandDependencyMap(proj)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()

	// Missing file, no evidence: fail closed.
	if _, err := collectFreehandDependencyMap(proj, p, start); err == nil {
		t.Fatal("no map and no evidence the flag reached gen_snapshot must fail the release")
	}
	// The log HEADER (the flutter command line) is not evidence that gen_snapshot received the flag.
	writeBuildLog(t, proj, "$ flutter build ios --extra-gen-snapshot-options="+freehandDependencyMapFlag+"="+p+"\n\nRunning Xcode build...\n")
	if _, err := collectFreehandDependencyMap(proj, p, start); err == nil {
		t.Fatal("Flutter's own argv is not evidence that gen_snapshot ran with the flag")
	}
	// A longer path sharing the prefix is not the flag either.
	writeBuildLog(t, proj, "[ +3 ms] executing: /x/gen_snapshot_arm64 --deterministic "+freehandDependencyMapFlag+"="+p+".old --snapshot_kind=app-aot-assembly\n")
	if _, err := collectFreehandDependencyMap(proj, p, start); err == nil {
		t.Fatal("a different path must not count as evidence")
	}
	// A gen_snapshot command line carrying the exact flag: an empty map.
	time.Sleep(10 * time.Millisecond)
	writeBuildLog(t, proj, "[ +3 ms] executing: /x/gen_snapshot_arm64 --deterministic "+freehandDependencyMapFlag+"="+p+" --snapshot_kind=app-aot-assembly\n")
	got, err := collectFreehandDependencyMap(proj, p, start)
	if err != nil || got.Edges != 0 || len(got.Canonical) != 0 {
		t.Fatalf("evidence of the flag with no file must be an empty map: %+v %v", got, err)
	}

	// A written map is parsed strictly and canonicalized.
	os.WriteFile(p, []byte("result\t1\tp::B::b\tp::A::a\ninline\t1\tp::B::b\tp::A::a\ninline\t1\tp::B::b\tp::A::a\n"), 0o600)
	got, err = collectFreehandDependencyMap(proj, p, start)
	if err != nil || got.Edges != 2 || string(got.Canonical) != "inline\t1\tp::B::b\tp::A::a\nresult\t1\tp::B::b\tp::A::a\n" {
		t.Fatalf("%+v %v", got, err)
	}
	os.WriteFile(p, []byte("inline\t1\tp::B::b\n"), 0o600)
	if _, err := collectFreehandDependencyMap(proj, p, start); err == nil {
		t.Fatal("a malformed map must fail the release")
	}
	// A map older than the build is stale.
	os.WriteFile(p, []byte("inline\t1\tp::B::b\tp::A::a\n"), 0o600)
	old := start.Add(-time.Hour)
	os.Chtimes(p, old, old)
	if _, err := collectFreehandDependencyMap(proj, p, start); err == nil {
		t.Fatal("a map predating the build must not be adopted")
	}
}

func TestDependencyMapRelease_FlagReachesTheBuildOnlyWhenDeclared(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".soroq", "toolchains")
	writeEngineBundle(t, root, "tc-r7", "rev-r7", `{"honoured_kinds":["method"],"identity_capabilities":["private_enclosing_class_identity_v1"]}`)
	writeEngineBundle(t, root, "tc-r8", "rev-r8", `{"honoured_kinds":["method"],"identity_capabilities":["`+freehandDependencyMapCapability+`"]}`)
	proj := t.TempDir()
	in := []string{"--extra-front-end-options=--dynamic-interface=/x"}

	// R7-shaped toolchain: arguments byte-for-byte unchanged, no staging created.
	out, path, declared, err := withFreehandDependencyMap(proj, "tc-r7", in)
	if err != nil || declared || path != "" || !reflect.DeepEqual(out, in) {
		t.Fatalf("a toolchain without the capability must not change the build: %v %q %v %v", out, path, declared, err)
	}
	// Uninstalled toolchain: the build refuses it itself; nothing is added here.
	if out, _, declared, err := withFreehandDependencyMap(proj, "tc-missing", in); err != nil || declared || !reflect.DeepEqual(out, in) {
		t.Fatalf("%v %v %v", out, declared, err)
	}

	// R8: exactly one flag, naming an absolute path under the project's .soroq build staging, stale file gone.
	stale, _ := freehandDependencyMapBuildPath(proj)
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("stale\n"), 0o600)
	out, path, declared, err = withFreehandDependencyMap(proj, "tc-r8", in)
	if err != nil || !declared {
		t.Fatalf("%v %v", declared, err)
	}
	if want := append(append([]string(nil), in...), "--extra-gen-snapshot-options=--soroq_dependency_map="+path); !reflect.DeepEqual(out, want) {
		t.Fatalf("build args = %v, want %v", out, want)
	}
	if !filepath.IsAbs(path) || path != stale {
		t.Fatalf("map path %q", path)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("the stale map must be removed before the build")
	}
	// A developer-supplied flag is refused on every toolchain.
	for _, tc := range []string{"tc-r7", "tc-r8"} {
		if _, _, _, err := withFreehandDependencyMap(proj, tc, append(in, "--extra-gen-snapshot-options=--soroq_dependency_map=/tmp/x")); err == nil {
			t.Fatalf("%s: a developer-supplied --soroq_dependency_map must be refused", tc)
		}
	}
}
