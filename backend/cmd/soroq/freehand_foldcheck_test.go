package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A synthetic gen_snapshot v8 profile, small enough to read and shaped exactly like the real one:
//
//	Class --> Array --> Function --> Code --> ObjectPool --> CanonicalString
//
// Every case below is built from this one shape, so a test that passes for the wrong reason has to
// pass through the same walk the production code does.
type profBuilder struct {
	types   []string
	strings []string
	nodes   [][5]int // type, name, id, self_size, edge_count
	edges   [][]int  // per node, list of target node indexes
}

func newProfBuilder() *profBuilder {
	return &profBuilder{types: []string{
		"ArtificialRoot", "Class", "Code", "Function", "ObjectPool", "CanonicalString", "Array",
	}}
}

func (b *profBuilder) str(s string) int {
	for i, existing := range b.strings {
		if existing == s {
			return i
		}
	}
	b.strings = append(b.strings, s)
	return len(b.strings) - 1
}

func (b *profBuilder) node(typ, name string) int {
	ti := -1
	for i, t := range b.types {
		if t == typ {
			ti = i
		}
	}
	if ti < 0 {
		panic("unknown node type " + typ)
	}
	b.nodes = append(b.nodes, [5]int{ti, b.str(name), len(b.nodes), 8, 0})
	b.edges = append(b.edges, nil)
	return len(b.nodes) - 1
}

func (b *profBuilder) edge(from, to int) {
	b.edges[from] = append(b.edges[from], to)
	b.nodes[from][4] = len(b.edges[from])
}

// decl wires one declaration and returns its ObjectPool node.
func (b *profBuilder) decl(class, fnName string) int {
	fn := b.node("Function", fnName)
	code := b.node("Code", "[Optimized] "+fnName)
	pool := b.node("ObjectPool", "Unnamed [ObjectPool] (nil)")
	b.edge(fn, code)
	b.edge(code, pool)
	if class != "" {
		arr := b.node("Array", "Unnamed [Array] (nil)")
		cls := b.node("Class", class)
		b.edge(cls, arr)
		b.edge(arr, fn)
	}
	return pool
}

func (b *profBuilder) constant(value string) int { return b.node("CanonicalString", value) }

func (b *profBuilder) write(t *testing.T) string {
	t.Helper()
	const NF = 5
	flatNodes := make([]int, 0, len(b.nodes)*NF)
	for _, n := range b.nodes {
		flatNodes = append(flatNodes, n[0], n[1], n[2], n[3], n[4])
	}
	flatEdges := make([]int, 0)
	for _, targets := range b.edges {
		for _, to := range targets {
			flatEdges = append(flatEdges, 1, 0, to*NF)
		}
	}
	doc := map[string]any{
		"snapshot": map[string]any{"meta": map[string]any{
			"node_fields": []string{"type", "name", "id", "self_size", "edge_count"},
			"node_types":  []any{b.types, "string", "number", "number", "number"},
			"edge_fields": []string{"type", "name_or_index", "to_node"},
			"edge_types":  []any{[]string{"context"}, "string_or_number", "node"},
		}},
		"nodes":   flatNodes,
		"edges":   flatEdges,
		"strings": b.strings,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "graph.heapsnapshot")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// GREEN CONTROL. The constant lives in exactly one declaration's pool, which is what a value the
// compiler did NOT propagate looks like. If this ever reports a refusal, every red below is worthless.
func TestValuePropagationGreenControl(t *testing.T) {
	b := newProfBuilder()
	pool := b.decl("", "otaValue")
	k := b.constant("BASE-VALUE")
	b.edge(pool, k)
	other := b.decl("", "main")
	b.edge(other, b.constant("something else entirely"))

	vp, err := analyzeValuePropagationFor(b.write(t), []PropagationQuery{{Kind: "function", VMName: "otaValue"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(vp.Propagated) != 0 {
		t.Fatalf("green control refused: %+v", vp.Propagated)
	}
	if msg := valuePropagationRefusal(vp, "function", "", "otaValue"); msg != "" {
		t.Fatalf("green control produced a refusal: %s", msg)
	}
}

// PLANTED FAILURE 1 — the exact defect measured on device: the constant is in the declaration's own
// pool AND in main's.
func TestValuePropagationDetectsFoldedTopLevelFunction(t *testing.T) {
	b := newProfBuilder()
	pool := b.decl("", "otaValue")
	mainPool := b.decl("", "main")
	k := b.constant("BASE-VALUE")
	b.edge(pool, k)
	b.edge(mainPool, k)

	vp, err := analyzeValuePropagationFor(b.write(t), []PropagationQuery{{Kind: "function", VMName: "otaValue"}})
	if err != nil {
		t.Fatal(err)
	}
	msg := valuePropagationRefusal(vp, "function", "", "otaValue")
	if msg == "" {
		t.Fatal("planted folded value was not detected")
	}
	for _, want := range []string{"otaValue", "main", "BASE-VALUE", "silent"} {
		if want == "silent" {
			continue
		}
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal does not name %q: %s", want, msg)
		}
	}
}

// PLANTED FAILURE 2 — a constructor. Its VM name is `C.` and gen_snapshot prints `new C.`; a check that
// compared the two directly would silently never fire for any constructor.
func TestValuePropagationDetectsFoldedConstructor(t *testing.T) {
	b := newProfBuilder()
	ctorPool := b.decl("EagerProviderProbe", "new EagerProviderProbe.")
	mainPool := b.decl("", "main")
	k := b.constant("BASE-VALUE")
	b.edge(ctorPool, k)
	b.edge(mainPool, k)

	q := []PropagationQuery{{Kind: "constructor", Class: "EagerProviderProbe", VMName: "EagerProviderProbe."}}
	vp, err := analyzeValuePropagationFor(b.write(t), q)
	if err != nil {
		t.Fatal(err)
	}
	if msg := valuePropagationRefusal(vp, "constructor", "EagerProviderProbe", "EagerProviderProbe."); msg == "" {
		t.Fatalf("planted folded constructor was not detected: %+v", vp.Propagated)
	}
}

// PLANTED FAILURE 3 — two classes declare `init:value`. Only the queried one may be flagged, or the
// check would refuse patches on an unrelated class that happens to share a member name.
func TestValuePropagationDisambiguatesByOwnerClass(t *testing.T) {
	b := newProfBuilder()
	hot := b.decl("StaticInitProbe", "init:value")
	cold := b.decl("UnrelatedProbe", "init:value")
	mainPool := b.decl("", "main")
	k := b.constant("BASE-VALUE")
	b.edge(hot, k)
	b.edge(mainPool, k)
	b.edge(cold, b.constant("cold constant"))

	q := []PropagationQuery{
		{Kind: "field-initializer", Class: "StaticInitProbe", VMName: "init:value"},
		{Kind: "field-initializer", Class: "UnrelatedProbe", VMName: "init:value"},
	}
	vp, err := analyzeValuePropagationFor(b.write(t), q)
	if err != nil {
		t.Fatal(err)
	}
	if msg := valuePropagationRefusal(vp, "field-initializer", "StaticInitProbe", "init:value"); msg == "" {
		t.Fatal("the folded StaticInitProbe.init:value was not detected")
	}
	if msg := valuePropagationRefusal(vp, "field-initializer", "UnrelatedProbe", "init:value"); msg != "" {
		t.Fatalf("UnrelatedProbe.init:value was refused for a constant it does not hold: %s", msg)
	}
}

// The gate itself, not just the analysis: a changed declaration whose value was folded must refuse.
func TestAssertFreehandNoFoldedValueRefusesAndPasses(t *testing.T) {
	b := newProfBuilder()
	pool := b.decl("", "otaValue")
	mainPool := b.decl("", "main")
	k := b.constant("BASE-VALUE")
	b.edge(pool, k)
	b.edge(mainPool, k)
	graph := b.write(t)

	relDir := t.TempDir()
	raw, err := os.ReadFile(graph)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relDir, freehandObjectGraphName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	folded := []changedDecl{{
		manifestLine: "package:app/values.dart::::otaValue",
		keyKind:      "function",
	}}
	if err := assertFreehandNoFoldedValue(relDir, folded); err == nil {
		t.Fatal("gate accepted a folded declaration")
	} else if !strings.Contains(err.Error(), "silent no-op") {
		t.Fatalf("refusal does not explain itself: %v", err)
	}

	// GREEN: a different declaration in the same baseline is untouched by the rule.
	clean := []changedDecl{{
		manifestLine: "package:app/values.dart::::somethingElse",
		keyKind:      "function",
	}}
	if err := assertFreehandNoFoldedValue(relDir, clean); err != nil {
		t.Fatalf("gate refused a declaration whose value was not propagated: %v", err)
	}
}

// A baseline with no object graph must REFUSE, not pass.
//
// This test asserted the opposite until a review pointed at the consequence: release 5d86e606, the
// pre-fix base whose patch was the original six-way silent no-op, carries no object graph, so that
// exact patch would have published green with the whole suite passing. Absent evidence is not clean
// evidence.
func TestAssertFreehandNoFoldedValueWithoutGraph(t *testing.T) {
	err := assertFreehandNoFoldedValue(t.TempDir(), []changedDecl{{
		manifestLine: "package:app/values.dart::::otaValue", keyKind: "function",
	}})
	if err == nil {
		t.Fatal("a baseline with no object graph was treated as checked")
	}
	if !strings.Contains(err.Error(), "Cut a new base release") {
		t.Fatalf("the refusal does not name the remedy: %v", err)
	}
}

// THE GAP, asserted as a shape so it runs on every `go test` rather than only when an operator points
// SOROQ_PROFILE at a 95 MB graph.
//
// A constructor that stores a value it obtained from somewhere else holds no constant in its own pool.
// The caller does. The rule keys on the declaration's own pool, so it cannot see this, and that is
// exactly how the two generative constructors and the two init: identities passed the gate on a base
// where all six were silent no-ops. If this test ever starts failing, the rule has grown a new
// capability and ROOT-CAUSE.md's table is out of date.
func TestValuePropagationGapIsReal(t *testing.T) {
	b := newProfBuilder()
	ctorPool := b.decl("EagerProviderProbe", "new EagerProviderProbe.")
	mainPool := b.decl("", "main")
	k := b.constant("BASE-VALUE")
	// Only the CALLER holds the folded constant. The constructor's pool holds something else entirely.
	b.edge(mainPool, k)
	b.edge(ctorPool, b.constant("#EPP"))
	graph := b.write(t)

	q := []PropagationQuery{{Kind: "constructor", Class: "EagerProviderProbe", VMName: "EagerProviderProbe."}}
	vp, err := analyzeValuePropagationFor(graph, q)
	if err != nil {
		t.Fatal(err)
	}
	if msg := valuePropagationRefusal(vp, "constructor", "EagerProviderProbe", "EagerProviderProbe."); msg != "" {
		t.Fatalf("the documented gap has closed -- update ROOT-CAUSE.md and the header of "+
			"freehand_foldcheck.go, which both state this case is NOT detected: %s", msg)
	}
}

// A corrupt graph must be an error, never a quiet pass. A checker that treats unreadable evidence as
// clean evidence cannot fail.
func TestAssertFreehandNoFoldedValueRejectsCorruptGraph(t *testing.T) {
	relDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(relDir, freehandObjectGraphName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := assertFreehandNoFoldedValue(relDir, []changedDecl{{
		manifestLine: "package:app/values.dart::::otaValue", keyKind: "function",
	}})
	if err == nil {
		t.Fatal("a corrupt object graph was treated as clean")
	}
}

// REAL-DATA CONTROL, opt-in — and it records the gate's GAP, not only its hit.
//
// Synthetic graphs prove the walk; only a real gen_snapshot profile proves the rule survives contact
// with a 1.1-million-node graph and the VM's actual naming. Run it as:
//
//	SOROQ_PROFILE=<a real .heapsnapshot> SOROQ_PROFILE_EXPECT=prefix|clean go test ./cmd/soroq \
//	  -run TestValuePropagationOnRealProfile -v
//
// `prefix` is the pre-fix t002 base build, where all six identities were silent no-ops on the device.
// The expectations below say that the rule catches exactly TWO of them. That is deliberate: the rule
// keys on a constant in the declaration's OWN pool, so it sees a declaration that RETURNS the constant
// and is blind to one that stores a value obtained elsewhere. Writing the four negatives down as
// assertions means the gap cannot quietly widen, and cannot be quietly claimed away either. See
// t002-root-cause/ROOT-CAUSE.md.
func TestValuePropagationOnRealProfile(t *testing.T) {
	path := os.Getenv("SOROQ_PROFILE")
	if path == "" {
		t.Skip("set SOROQ_PROFILE to a real gen_snapshot v8 profile to run this control")
	}
	cases := []struct {
		kind, class, vmName string
		refusedInPrefix     bool
	}{
		{"function", "", "otaValue", true},
		{"factory", "FactoryCtorProbe", "FactoryCtorProbe.make", true},
		// KNOWN GAP, measured: these four were silent no-ops on the device and the rule does not see
		// them, because none of them holds the constant in its own pool.
		{"constructor", "EagerProviderProbe", "EagerProviderProbe.", false},
		{"constructor", "NamedCtorProbe", "NamedCtorProbe.seeded", false},
		{"field-initializer", "", "init:gTopLevelFinal", false},
		{"field-initializer", "StaticInitProbe", "init:value", false},
	}
	queries := make([]PropagationQuery, 0, len(cases))
	for _, c := range cases {
		queries = append(queries, PropagationQuery{Kind: c.kind, Class: c.class, VMName: c.vmName})
	}
	vp, err := analyzeValuePropagationFor(path, queries)
	if err != nil {
		t.Fatal(err)
	}
	expect := os.Getenv("SOROQ_PROFILE_EXPECT")
	for _, c := range cases {
		msg := valuePropagationRefusal(vp, c.kind, c.class, c.vmName)
		switch expect {
		case "prefix":
			if c.refusedInPrefix && msg == "" {
				t.Errorf("%s%s: expected a refusal in the pre-fix profile", c.class, c.vmName)
			}
			if !c.refusedInPrefix && msg != "" {
				t.Errorf("%s%s: the known gap has closed — update this table and ROOT-CAUSE.md: %s",
					c.class, c.vmName, msg)
			}
		case "clean":
			if msg != "" {
				t.Errorf("%s%s: expected no refusal in the fixed profile, got: %s", c.class, c.vmName, msg)
			}
		default:
			t.Fatalf("set SOROQ_PROFILE_EXPECT to prefix or clean")
		}
	}
}
