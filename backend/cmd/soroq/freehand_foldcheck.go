package main

// [soroq] Detect the silent no-op: a patchable declaration whose VALUE the precompiler already
// propagated into its callers.
//
// WHY THIS EXISTS. `soroq patch` can publish a manifest, the engine can commit every redirect, the
// module bytecode can run — and the app can still render the old value, because the AOT compiler
// replaced the call with the constant it returns. Nothing at runtime can detect that; it was decided
// when the base was built.
//
// This was not a theory. The first t002 device lane spent six launches and a full engine diagnostic
// cycle on it. The base build's own gen_snapshot object graph showed the literal 'BASE-VALUE' sitting
// FIVE times in the object pool of `[Optimized] main`, one per construct main() captured, so main
// never read the fields it appeared to read. See t002-root-cause/ROOT-CAUSE.md.
//
// THE RULE, and it is deliberately a rule about MEASURED OUTPUT rather than about source shape:
//
//	a constant that sits in the object pool of a declaration's own Code, and ALSO sits in the object
//	pool of some other Code, was propagated out of that declaration.
//
// Source-shape predicates were tried and rejected as unsound. `body is a literal` misses the case
// measured next: returning a mutable top-level variable assigned exactly once folds identically,
// because type-flow analysis infers the single value. Only the compiler's own output knows.
//
// SCOPE, measured rather than assumed. The rule keys on a constant in the declaration's OWN pool, so
// it sees a declaration that RETURNS the folded constant and is BLIND to one that stores a value it
// obtained from somewhere else. Against the pre-fix t002 base, where all six identities were silent
// no-ops on the device, it refuses `otaValue` and `FactoryCtorProbe.make` and lets the two generative
// constructors and the two `init:<field>` identities through.
//
// Those four negatives are asserted two ways, and the difference matters. TestValuePropagationGapIsReal
// encodes the SHAPE and runs on every `go test`. TestValuePropagationOnRealProfile checks the same
// verdicts against a real gen_snapshot graph but is opt-in (`SOROQ_PROFILE`) and SKIPS by default,
// because the graphs are ~95 MB and carry unrelated application strings; it is a control an operator
// runs, not a gate CI enforces. An earlier version of this comment claimed the real-profile test made
// the gap impossible to widen. It does not, on its own.
//
// The "two of six" figure is a property of ONE base build -- the pre-fix t002 base -- not of the rule
// in general. Intermediate builds of the same fixture, where the constant reached every caller's pool,
// are flagged six of six.
//
// The profile cannot close that gap on its own: a callee's Code is not referenced from a caller's
// object pool -- AOT direct calls are relative branches -- so nothing in the graph ties a caller's
// folded constant back to the declaration that produced it. Closing it needs the analyzer, which knows
// both the declaration's value and the field it writes. What limits the damage today is that the gate
// refuses the PATCH, not the identity: a patch that also changes the folded value-returning function
// is refused outright.
//
// CONSERVATISM, stated plainly. The rule can flag a declaration whose pool merely happens to share a
// constant with another Code. That costs a refusal on a declaration a developer is actively patching,
// with a named remedy. The alternative costs a patch that publishes green and does nothing. The
// refusal is the cheaper error, and it only ever runs over the CHANGED set — never the whole app.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const freehandValuePropagationSchema = "soroq.freehand.value_propagation.v1"

// FreehandPropagatedValue names one declaration whose constant reached other code.
type FreehandPropagatedValue struct {
	// Function is the display name gen_snapshot gave the declaration: `otaValue`,
	// `new EagerProviderProbe.`, `init:gTopLevelFinal`.
	Function string `json:"function"`
	// Owner is the declaring class name, or "" for a top-level declaration. Two classes can both
	// declare `init:value`, so the name alone is not an identity.
	Owner string `json:"owner"`
	// Constant is the propagated value, truncated. Diagnostic only; never matched on.
	Constant string `json:"constant"`
	// Reached names the other declarations whose pools carry the same constant. Bounded.
	Reached []string `json:"reached"`
	// ReachedTotal is the true count before Reached was bounded, so a truncated list never reads as
	// a complete one.
	ReachedTotal int `json:"reached_total"`
}

// FreehandValuePropagation is what the baseline carries. The 90+ MB profile it was computed from is
// NOT kept: it contains unrelated application strings.
type FreehandValuePropagation struct {
	Schema     string                    `json:"schema"`
	Propagated []FreehandPropagatedValue `json:"propagated"`
}

const (
	maxReachedListed     = 8
	maxConstantChars     = 96
	maxPropagatedListed  = 512
	constantNodeTypeName = "CanonicalString"
)

// snapshotProfile is the subset of a gen_snapshot v8 heap-snapshot profile this check needs.
type snapshotProfile struct {
	nodeTypes []string
	strings   []string
	// flat node records, nodeFieldCount wide
	nodes                    []int32
	edges                    []int32
	nodeFieldCount           int
	edgeFieldCount           int
	iName, iType, iEdgeCount int
	iEdgeTo                  int
}

func (p *snapshotProfile) count() int { return len(p.nodes) / p.nodeFieldCount }

func (p *snapshotProfile) name(i int) string {
	idx := int(p.nodes[i*p.nodeFieldCount+p.iName])
	if idx < 0 || idx >= len(p.strings) {
		return ""
	}
	return p.strings[idx]
}

func (p *snapshotProfile) typ(i int) string {
	idx := int(p.nodes[i*p.nodeFieldCount+p.iType])
	if idx < 0 || idx >= len(p.nodeTypes) {
		return ""
	}
	return p.nodeTypes[idx]
}

// loadSnapshotProfile streams the profile. `nodes` and `edges` are multi-million-element integer
// arrays; decoding them into []any would cost an order of magnitude more memory than the file.
func loadSnapshotProfile(path string) (*snapshotProfile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("snapshot profile: expected a JSON object")
	}
	p := &snapshotProfile{}
	var nodeFields, edgeFields []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyTok.(string)
		switch key {
		case "snapshot":
			var snap struct {
				Meta struct {
					NodeFields []string          `json:"node_fields"`
					NodeTypes  []json.RawMessage `json:"node_types"`
					EdgeFields []string          `json:"edge_fields"`
				} `json:"meta"`
			}
			if err := dec.Decode(&snap); err != nil {
				return nil, err
			}
			nodeFields = snap.Meta.NodeFields
			edgeFields = snap.Meta.EdgeFields
			// node_types is a heterogeneous array: the first element is the list of node type names,
			// the rest are field-type tags such as "string" or "number". Take the first element that
			// really is a list of names.
			for _, raw := range snap.Meta.NodeTypes {
				var names []string
				if err := json.Unmarshal(raw, &names); err == nil {
					p.nodeTypes = names
					break
				}
			}
		case "nodes":
			if p.nodes, err = decodeInt32Array(dec); err != nil {
				return nil, fmt.Errorf("decode nodes: %w", err)
			}
		case "edges":
			if p.edges, err = decodeInt32Array(dec); err != nil {
				return nil, fmt.Errorf("decode edges: %w", err)
			}
		case "strings":
			if err := dec.Decode(&p.strings); err != nil {
				return nil, fmt.Errorf("decode strings: %w", err)
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
		}
	}
	idx := func(fields []string, want string) int {
		for i, f := range fields {
			if f == want {
				return i
			}
		}
		return -1
	}
	p.nodeFieldCount = len(nodeFields)
	p.edgeFieldCount = len(edgeFields)
	p.iName = idx(nodeFields, "name")
	p.iType = idx(nodeFields, "type")
	p.iEdgeCount = idx(nodeFields, "edge_count")
	p.iEdgeTo = idx(edgeFields, "to_node")
	if p.nodeFieldCount == 0 || p.edgeFieldCount == 0 ||
		p.iName < 0 || p.iType < 0 || p.iEdgeCount < 0 || p.iEdgeTo < 0 {
		return nil, fmt.Errorf("snapshot profile: missing required meta fields")
	}
	if len(p.nodes)%p.nodeFieldCount != 0 || len(p.edges)%p.edgeFieldCount != 0 {
		return nil, fmt.Errorf("snapshot profile: node/edge array is not a whole number of records")
	}
	return p, nil
}

func decodeInt32Array(dec *json.Decoder) ([]int32, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("expected an array")
	}
	out := make([]int32, 0, 1<<20)
	for dec.More() {
		var v int64
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, int32(v))
	}
	if _, err := dec.Token(); err != nil && err != io.EOF { // closing ']'
		return nil, err
	}
	return out, nil
}

// PropagationQuery is one identity the caller wants a verdict for.
type PropagationQuery struct {
	Kind   string // Soroq semantic kind: function, static-method, constructor, factory, field-initializer, ...
	Class  string // declaring class, "" for a top-level declaration
	VMName string // the VM member name, e.g. otaValue, EagerProviderProbe., init:gTopLevelFinal
}

// analyzeValuePropagationFor answers, for a SPECIFIC set of identities, whether a constant in that
// declaration's own object pool also appears in some other declaration's object pool.
//
// Targeted on purpose. A global sweep over the whole snapshot returns hundreds of entries, because
// unrelated declarations legitimately share common strings; that noise says nothing about any
// particular patch. The changed set of a patch is a handful of identities, and asking only about
// those is both cheaper and the only form of the question with a defensible answer.
func analyzeValuePropagationFor(path string, queries []PropagationQuery) (*FreehandValuePropagation, error) {
	if len(queries) == 0 {
		return &FreehandValuePropagation{Schema: freehandValuePropagationSchema}, nil
	}
	p, err := loadSnapshotProfile(path)
	if err != nil {
		return nil, err
	}
	n := p.count()

	// Edge records are stored in node order: node i owns edge_count[i] consecutive records.
	edgeStart := make([]int32, n+1)
	var running int32
	for i := 0; i < n; i++ {
		edgeStart[i] = running
		running += p.nodes[i*p.nodeFieldCount+p.iEdgeCount]
	}
	edgeStart[n] = running
	to := func(e int32) int { return int(p.edges[int(e)*p.edgeFieldCount+p.iEdgeTo]) / p.nodeFieldCount }

	rev := make([][]int32, n)
	for i := 0; i < n; i++ {
		for e := edgeStart[i]; e < edgeStart[i+1]; e++ {
			if t := to(e); t >= 0 && t < n {
				rev[t] = append(rev[t], int32(i))
			}
		}
	}

	// Every pool that holds each constant, and every constant each pool holds.
	constantPools := map[int][]int{}
	poolConstants := map[int][]int{}
	for i := 0; i < n; i++ {
		if p.typ(i) != "ObjectPool" {
			continue
		}
		for e := edgeStart[i]; e < edgeStart[i+1]; e++ {
			t := to(e)
			if t >= 0 && t < n && p.typ(t) == constantNodeTypeName {
				constantPools[t] = append(constantPools[t], i)
				poolConstants[i] = append(poolConstants[i], t)
			}
		}
	}

	// The pools owned by each queried identity. A Code display name is "[Optimized] <name>", and a
	// constructor's display name is "new C.name" where the VM name is "C.name".
	wanted := map[string][]int{} // key: owner\x00vmName
	for _, q := range queries {
		wanted[q.Class+"\x00"+q.VMName] = nil
	}
	for i := 0; i < n; i++ {
		if p.typ(i) != "ObjectPool" {
			continue
		}
		d, ok := declForPool(p, rev, i)
		if !ok {
			continue
		}
		for _, q := range queries {
			if d.fn != snapshotFunctionNameFor(q.Kind, q.Class, q.VMName) {
				continue
			}
			if q.Class != "" && d.owner != "" && d.owner != q.Class {
				continue
			}
			key := q.Class + "\x00" + q.VMName
			wanted[key] = append(wanted[key], i)
		}
	}

	out := &FreehandValuePropagation{Schema: freehandValuePropagationSchema}
	for _, q := range queries {
		key := q.Class + "\x00" + q.VMName
		for _, pool := range wanted[key] {
			for _, c := range poolConstants[pool] {
				others := make([]string, 0, 4)
				for _, other := range constantPools[c] {
					if other == pool {
						continue
					}
					od, ok := declForPool(p, rev, other)
					if !ok {
						continue
					}
					others = append(others, declLabel(od))
				}
				if len(others) == 0 {
					continue
				}
				sort.Strings(others)
				others = dedupeStrings(others)
				total := len(others)
				if len(others) > maxReachedListed {
					others = others[:maxReachedListed]
				}
				out.Propagated = append(out.Propagated, FreehandPropagatedValue{
					Function:     snapshotFunctionNameFor(q.Kind, q.Class, q.VMName),
					Owner:        q.Class,
					Constant:     truncateConstant(p.name(c)),
					Reached:      others,
					ReachedTotal: total,
				})
			}
		}
	}
	sort.Slice(out.Propagated, func(i, j int) bool {
		a, b := out.Propagated[i], out.Propagated[j]
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		if a.Function != b.Function {
			return a.Function < b.Function
		}
		return a.Constant < b.Constant
	})
	return out, nil
}

type poolDecl struct{ fn, owner string }

// declForPool walks ObjectPool -> Code -> Function -> (functions Array) -> Class.
func declForPool(p *snapshotProfile, rev [][]int32, pool int) (poolDecl, bool) {
	for _, up := range rev[pool] {
		if p.typ(int(up)) != "Code" {
			continue
		}
		for _, fnNode := range rev[int(up)] {
			if p.typ(int(fnNode)) != "Function" {
				continue
			}
			owner := ""
			for _, arr := range rev[int(fnNode)] {
				if p.typ(int(arr)) != "Array" {
					continue
				}
				for _, cls := range rev[int(arr)] {
					if p.typ(int(cls)) == "Class" {
						owner = p.name(int(cls))
						break
					}
				}
				if owner != "" {
					break
				}
			}
			return poolDecl{fn: p.name(int(fnNode)), owner: owner}, true
		}
	}
	return poolDecl{}, false
}

// declLabel prints a declaration the way a developer would recognise it. A constructor's own name
// already carries its class, and the VM's name for a top-level owner is "::", so neither is prefixed.
func declLabel(d poolDecl) string {
	if d.owner == "" || d.owner == "::" || strings.HasPrefix(d.fn, "new ") {
		return d.fn
	}
	return d.owner + "." + d.fn
}

func truncateConstant(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > maxConstantChars {
		return s[:maxConstantChars] + "…"
	}
	return s
}

// snapshotFunctionNameFor maps a Soroq identity to the display name gen_snapshot uses.
//
// The VM name and the profile name are NOT the same string for constructors: the VM calls the
// unnamed generative constructor of `C` exactly `C.`, and gen_snapshot prints `new C.`.
func snapshotFunctionNameFor(kind, class, vmName string) string {
	switch kind {
	case "constructor", "factory":
		return "new " + vmName
	default:
		return vmName
	}
}

// valuePropagationRefusal returns a non-empty message when publishing a redirect for this identity
// would be a no-op at the sites the compiler already folded.
func valuePropagationRefusal(vp *FreehandValuePropagation, kind, class, vmName string) string {
	if vp == nil {
		return ""
	}
	want := snapshotFunctionNameFor(kind, class, vmName)
	for _, e := range vp.Propagated {
		if e.Function != want {
			continue
		}
		// A top-level declaration has no owner class on either side; a member must match its class.
		if class != "" && e.Owner != "" && e.Owner != class {
			continue
		}
		reached := strings.Join(e.Reached, ", ")
		more := ""
		if e.ReachedTotal > len(e.Reached) {
			more = fmt.Sprintf(" (+%d more)", e.ReachedTotal-len(e.Reached))
		}
		return fmt.Sprintf(
			"%s%s%s returns a value the precompiler already propagated into %s%s. "+
				"Redirecting it would publish green and change nothing at those sites, because they "+
				"load the constant %q instead of calling. Make the value non-constant (return it "+
				"through state the compiler cannot prove single-valued) and cut a new base release.",
			class, dotIf(class), vmName, reached, more, e.Constant)
	}
	return ""
}

func dotIf(s string) string {
	if s == "" {
		return ""
	}
	return "."
}

// freehandObjectGraphName is the base build's gen_snapshot object graph, kept inside the immutable
// baseline directory. It is LOCAL ONLY and is never uploaded: it carries unrelated application
// strings.
const freehandObjectGraphName = "aot_object_graph.heapsnapshot"

// assertFreehandNoFoldedValue refuses a patch whose changed declaration returns a value the base's
// precompiler already propagated into other code. Runs after the capability gate and before any module
// is synthesised, so the refusal costs no compile.
func assertFreehandNoFoldedValue(relDir string, decls []changedDecl) error {
	graph := filepath.Join(relDir, freehandObjectGraphName)
	if _, err := os.Stat(graph); err != nil {
		// FAIL CLOSED on missing evidence.
		//
		// This used to print a NOTICE and return nil, and a review showed exactly what that bought: the
		// pre-fix baseline whose patch was the original six-way silent no-op carries no object graph, so
		// that patch would still have published green with the whole suite passing. A check that treats
		// absent evidence as clean evidence is not a check.
		//
		// The remedy is cheap and is named in the message: `soroq release ios --engine --build` writes
		// the graph into the baseline, so cutting a new base release enables it.
		return fmt.Errorf(
			"this baseline carries no %s, so whether the precompiler already folded these values into "+
				"their callers cannot be determined. A redirect on a folded value publishes green and "+
				"changes nothing, and no runtime check can see it. Cut a new base release (which writes "+
				"the graph) and patch against that",
			freehandObjectGraphName)
	}
	queries := make([]PropagationQuery, 0, len(decls))
	for _, d := range decls {
		_, _, vmName, err := splitIdentity(d.manifestLine)
		if err != nil {
			continue
		}
		queries = append(queries, PropagationQuery{Kind: d.keyKind, Class: d.keyClass, VMName: vmName})
	}
	vp, err := analyzeValuePropagationFor(graph, queries)
	if err != nil {
		return fmt.Errorf("read the base object graph %s: %w", graph, err)
	}
	var refusals []string
	for _, d := range decls {
		_, _, vmName, err := splitIdentity(d.manifestLine)
		if err != nil {
			continue
		}
		if msg := valuePropagationRefusal(vp, d.keyKind, d.keyClass, vmName); msg != "" {
			refusals = append(refusals, msg)
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	return fmt.Errorf("the redirect would be a silent no-op:\n  - %s", strings.Join(refusals, "\n  - "))
}
