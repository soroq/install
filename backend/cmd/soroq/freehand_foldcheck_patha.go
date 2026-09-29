package main

// [soroq] The value-propagation check, made precise for bases built by a Path A engine.
//
// READ freehand_foldcheck.go FIRST. The rule there -- "a constant in the changed declaration's pool
// that is also in another Code's pool was propagated" -- was measured on the t002 fixture and is kept,
// unchanged, for every base this file cannot prove was built by a Path A engine.
//
// WHAT PATH A CLOSES. Every Soroq engine patch set listed in freehandPathAEngineRevisions carries, for
// each function in the base's patchable manifest (tooling/flutter_matrix/patches/<label>/dart.patch):
//
//	inliner.cc                 a patchable callee is never inlined
//	kernel_binary_flowgraph.cc a patchable function exposes no unchecked entry
//	kernel_to_il.cc            InstanceCall / StaticCall do NOT take TFA's inferred constant / result
//	                           type when the (interface) target is patchable
//	kernel_translation_helper  a patchable function's unboxing metadata is reset
//
// So the direct route -- a caller replacing `F()` with the constant F returns, or inlining F's body --
// is closed for a patchable F. Path A predates t002 (T033, June; the production compiler patch 0005
// carried it), so the t002 folds were NOT that route: gMainCaptured = otaValue() was a real call and
// took the patched value. The five constructs that stayed BASE were read through the routes below.
//
// WHAT PATH A DOES NOT CLOSE. Type-flow analysis (pkg/vm/lib/transformations/type_flow) runs in the
// FRONTEND, before gen_snapshot, and no Soroq patch touches it. TFA does not rewrite values into
// constants in kernel (it only replaces unreachable code with throws); it ANNOTATES kernel
// (vm.inferred-type / vm.inferred-arg-type / vm.inferred-return-type metadata), and gen_snapshot's IL
// builders turn an annotated constant into a Constant instruction. Path A gates exactly two of those
// consumers. TFA itself does not know which members are patchable, so F's base value still flows
// through its summaries and every other consumer folds it:
//
//   - fields: a field whose every store is F's value is inferred single-valued; StaticGet without a
//     getter (BuildStaticGet) and InstanceGet of the field or its implicit getter (a non-patchable
//     interface target) load the constant. This is the t002 measurement: main's pool held
//     'BASE-VALUE' once per construct it captured.
//   - static final fields with a literal initializer are folded by gen_snapshot itself
//     (has_trivial_initializer), with no annotation at all.
//   - parameters: an argument that is the same constant at every call site becomes a constant inside
//     the callee (scope_builder.cc, inferred_arg_type); an implicit setter takes its field's type.
//   - captured variables: a local captured by a closure carries its own annotation.
//   - closure calls: ClosureCall takes the inferred result type ungated (base_flow_graph_builder.cc).
//   - calls through a NON-patchable interface target whose only reachable implementation is F: the
//     gate tests the interface target, not the implementation.
//   - const constructors / enum values / const fields: CFE evaluates them before TFA runs; no redirect
//     can change them, which is why the analyzer refuses changes to them rather than this check.
//   - closures created inside F: their Code is F's own; the patched F creates the module's closures,
//     so a base closure holding F's literal is not a propagated site.
//
// THE PRECISE RULE (Path A bases only). A CanonicalString S in the changed declaration F's pool that
// also sits in another Code G's pool is attributed to F UNLESS one of these holds. Every input is the
// base's own immutable output: the object graph, and app.dill -- the kernel gen_snapshot compiled, which
// embeds the base source of every library and carries TFA's annotations.
//
//  1. OWN CODE. G is a closure whose outermost parent is F, or F's tear-off.
//  2. NOT A DART VALUE. S is not in the kernel's string table. TFA constants come only from kernel
//     string literals and constants, so a string gen_snapshot synthesized (" in type cast", a folded
//     interpolation) cannot leave F, which is never inlined.
//  3. NO SINK F CAN FEED (F not a field initializer). A constant leaves F only through F's own return,
//     an argument F passes, or a field F stores; every later hop starts at one of those. So S can have
//     left F only if some annotation carrying S is F's own return annotation, a member parameter whose
//     member F names, a constructor parameter of a class F names, or a field F names. Operators, `call`
//     and anything unclassified always count. Field initializers are excluded from this rule because of
//     the trivial-initializer fold above.
//  4. NOT A FOLD IN G. No annotation carrying S lies inside G's declaration or inside any function
//     gen_snapshot inlined into G (inlined_id_to_function_). Whatever put S in G's pool, it was not a
//     type-flow fold, so it was not F.
//  5. G WROTE IT. G's base source (or a function inlined into G, or a const G names from its file, its
//     library or a library it imports) contains S as a string literal.
//
// Anything else is refused, including every G whose declaration cannot be located in the base source
// (it is then taken to be its whole file for rule 4 and to contain nothing for rule 5). If app.dill is
// missing or its metadata cannot be decoded, the check falls back to freehand_foldcheck.go's rule, or
// drops rules 3 and 4.
//
// RESIDUAL RISK, stated plainly.
//   - Rule 5 cannot tell "G holds S because G wrote S" from "G wrote S AND also received F's S
//     through a field or parameter": the pool entry is canonical, so both uses share one slot. It
//     needs a route above AND an identical literal in the receiving declaration.
//     TestPathAResidualSharedLiteralIsAccepted pins that shape; it is what a device proof should run.
//   - Rule 3 is name-based: a callee reached without its name appearing in F's source (a callable
//     object's `call` and operators are handled; a member reached only through a tear-off stored
//     elsewhere is not F's argument) would be missed.
//   - Out of scope, and not caught by either rule: a PATCH that newly passes a different value to a
//     base member whose parameter TFA pinned to a constant from its base callers; and a base call site
//     TFA pruned as unreachable because F never returned normally in the base.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// freehandPathAEngineRevisions maps each engine revision whose gen_snapshot carries the full Path A
// compiler patch to the matrix label it was built from. A revision missing here keeps the conservative
// rule. TestPathAEngineRevisionsCarryPathA pins every entry to its committed dart.patch.
var freehandPathAEngineRevisions = map[string]string{
	"soroq.ios_engine.6b182d2c_5a2a6a42.private_state.r6_obfuscation": "3.44.9-private-state-r6-obfuscation",
	"soroq.ios_engine.6b182d2c_5a2a6a42.private_state.r7_obfuscation": "3.44.9-private-state-r7-obfuscation",
}

// freehandDependencyMapEngineRevisions are engines that record a dependency map (R8) instead of banning
// inlining and static-call result propagation outright. The precise rule is sound for them because every
// caller that absorbed a changed declaration is replaced by the same patch (rule 0 in
// analyzeValuePropagationPathA); instance-call results keep the Path A ban. TestR8EngineRevisionsRecord
// pins each entry to its committed dart.patch.
var freehandDependencyMapEngineRevisions = map[string]string{
	"soroq.ios_engine.6b182d2c_5a2a6a42.private_state.r8_obfuscation": "3.44.9-private-state-r8-obfuscation",
}

// freehandBasePathA reports whether the base was built by an engine known to carry Path A. Both the
// baseline's own engine_revision and, when present, its recorded capability record must name it.
func freehandBasePathA(base *FreehandBaselineMeta) (string, bool) {
	if base == nil {
		return "", false
	}
	rev := strings.TrimSpace(base.EngineRev)
	_, pathA := freehandPathAEngineRevisions[rev]
	_, r8 := freehandDependencyMapEngineRevisions[rev]
	if !pathA && !r8 {
		return rev, false
	}
	if base.RedirectCapabilities != nil && base.RedirectCapabilities.EngineRevision != rev {
		return rev, false
	}
	return rev, true
}

// PathAQuery is a PropagationQuery that also names the declaring library, so a same-named member of an
// unrelated class in another library can never be mistaken for the changed declaration.
type PathAQuery struct {
	PropagationQuery
	LibURI string
}

type pathAGraph struct {
	p         *snapshotProfile
	n         int
	edgeStart []int32
	rev       [][]int32
}

func newPathAGraph(p *snapshotProfile) (*pathAGraph, error) {
	if p.iEdgeName < 0 || p.iEdgeType < 0 || len(p.edgeTypes) == 0 {
		return nil, fmt.Errorf("object graph carries no edge names")
	}
	n := p.count()
	g := &pathAGraph{p: p, n: n, edgeStart: make([]int32, n+1), rev: make([][]int32, n)}
	var running int32
	for i := 0; i < n; i++ {
		g.edgeStart[i] = running
		running += p.nodes[i*p.nodeFieldCount+p.iEdgeCount]
	}
	g.edgeStart[n] = running
	if int(running)*p.edgeFieldCount > len(p.edges) {
		return nil, fmt.Errorf("object graph edge counts overrun the edge array")
	}
	for i := 0; i < n; i++ {
		for e := g.edgeStart[i]; e < g.edgeStart[i+1]; e++ {
			if t := g.to(e); t >= 0 && t < n {
				g.rev[t] = append(g.rev[t], int32(i))
			}
		}
	}
	return g, nil
}

func (g *pathAGraph) to(e int32) int {
	return int(g.p.edges[int(e)*g.p.edgeFieldCount+g.p.iEdgeTo]) / g.p.nodeFieldCount
}

// prop follows the named property edge of node i, or returns -1.
func (g *pathAGraph) prop(i int, key string) int {
	if i < 0 {
		return -1
	}
	p := g.p
	for e := g.edgeStart[i]; e < g.edgeStart[i+1]; e++ {
		rec := int(e) * p.edgeFieldCount
		et := int(p.edges[rec+p.iEdgeType])
		if et < 0 || et >= len(p.edgeTypes) {
			continue
		}
		switch p.edgeTypes[et] {
		case "property", "internal", "context":
		default:
			continue
		}
		ni := int(p.edges[rec+p.iEdgeName])
		if ni >= 0 && ni < len(p.strings) && p.strings[ni] == key {
			if t := g.to(e); t >= 0 && t < g.n {
				return t
			}
		}
	}
	return -1
}

// out lists every target of node i.
func (g *pathAGraph) out(i int) []int {
	res := make([]int, 0, int(g.edgeStart[i+1]-g.edgeStart[i]))
	for e := g.edgeStart[i]; e < g.edgeStart[i+1]; e++ {
		if t := g.to(e); t >= 0 && t < g.n {
			res = append(res, t)
		}
	}
	return res
}

// poolFunction returns the Code and Function that own an ObjectPool.
func (g *pathAGraph) poolFunction(pool int) (code, fn int) {
	for _, up := range g.rev[pool] {
		if g.p.typ(int(up)) != "Code" {
			continue
		}
		c := int(up)
		if f := g.prop(c, "owner_"); f >= 0 && g.p.typ(f) == "Function" {
			return c, f
		}
		for _, fnNode := range g.rev[c] {
			if g.p.typ(int(fnNode)) == "Function" {
				return c, int(fnNode)
			}
		}
	}
	return -1, -1
}

// outer walks a closure to the declaration it was written in, and a tear-off to its target.
func (g *pathAGraph) outer(fn int) int {
	for guard := 0; fn >= 0 && guard < 64; guard++ {
		data := g.prop(fn, "data_")
		if data >= 0 && g.p.typ(data) == "ClosureData" {
			if parent := g.prop(data, "parent_function_"); parent >= 0 {
				fn = parent
				continue
			}
		}
		if data >= 0 && g.p.typ(data) == "Function" && strings.HasPrefix(g.p.name(fn), "[tear-off] ") {
			fn = data
			continue
		}
		break
	}
	return fn
}

type pathAOwner struct {
	class   string // owner class name as the VM prints it ("::" for a library's top level)
	library string // library URI
	script  string // script (file) URI the function is declared in
}

func (g *pathAGraph) owner(fn int) (pathAOwner, bool) {
	ow := g.prop(fn, "owner_")
	if ow < 0 {
		return pathAOwner{}, false
	}
	var o pathAOwner
	cls := ow
	if g.p.typ(ow) == "PatchClass" {
		if s := g.prop(ow, "script_"); s >= 0 {
			o.script = g.p.name(s)
		}
		cls = g.prop(ow, "wrapped_class_")
		if cls < 0 {
			return pathAOwner{}, false
		}
	}
	if g.p.typ(cls) != "Class" {
		return pathAOwner{}, false
	}
	o.class = g.p.name(cls)
	if l := g.prop(cls, "library_"); l >= 0 {
		o.library = g.p.name(l)
	}
	if o.script == "" {
		if s := g.prop(cls, "script_"); s >= 0 {
			o.script = g.p.name(s)
		}
	}
	return o, o.library != ""
}

func classMatches(profileClass, queryClass string) bool {
	pc := unmangleVMName(profileClass)
	if pc == "::" {
		pc = ""
	}
	return pc == unmangleVMName(queryClass)
}

// pathASourceIndex answers "does this declaration's own base source contain S".
type pathASourceIndex struct {
	kernel  *freehandKernel
	imports map[string][]string // library URI -> imported library URIs
	files   map[string]*dartFile
	consts  map[string]map[string]map[string]bool
}

func (x *pathASourceIndex) file(uri string) *dartFile {
	if f, ok := x.files[uri]; ok {
		return f
	}
	var f *dartFile
	if src, ok := x.kernel.sources[uri]; ok {
		f = indexDartFile(string(src))
	}
	x.files[uri] = f
	return f
}

func (x *pathASourceIndex) constLits(uri string) map[string]map[string]bool {
	if c, ok := x.consts[uri]; ok {
		return c
	}
	var c map[string]map[string]bool
	if f := x.file(uri); f != nil {
		c = f.constLiterals()
	}
	x.consts[uri] = c
	return c
}

// declSource is what the scanner found for one declaration.
type declSource struct {
	found  bool
	why    string
	lits   map[string]bool
	idents map[string]bool
	owner  pathAOwner
	// spans is where the declaration's code comes from, in kernel file offsets (UTF-16). When the
	// declaration could not be located it is the whole file; when even the file is unknown it is nil,
	// which annotatedWith treats as "may overlap anything".
	spans []kernelSpan
}

func (x *pathASourceIndex) declaration(g *pathAGraph, fn int) declSource {
	o, ok := g.owner(fn)
	if !ok {
		return declSource{why: "its owner class could not be resolved in the object graph"}
	}
	wholeFile := []kernelSpan{}
	for _, uri := range []string{o.script, o.library} {
		if uri != "" {
			wholeFile = append(wholeFile, kernelSpan{uri: uri, whole: true})
		}
	}
	scope, key, ok := dartDeclTarget(o.class, g.p.name(fn))
	if !ok {
		return declSource{owner: o, spans: wholeFile,
			why: fmt.Sprintf("%q is compiler-generated and has no source declaration", g.p.name(fn))}
	}
	var decls []dartDecl
	var file *dartFile
	var fileURI string
	for _, uri := range []string{o.script, o.library} {
		if uri == "" {
			continue
		}
		if f := x.file(uri); f != nil {
			if d := f.find(scope, key); len(d) > 0 {
				decls, file, fileURI = d, f, uri
				break
			}
		}
	}
	if file == nil {
		if x.file(o.script) == nil && x.file(o.library) == nil {
			return declSource{owner: o, spans: wholeFile, why: fmt.Sprintf("the base kernel embeds no source for %s", o.script)}
		}
		return declSource{owner: o, spans: wholeFile, why: fmt.Sprintf("its declaration was not found in the base source of %s", o.script)}
	}
	// Type-flow analysis moves instance field initializers into every generative constructor, so a
	// constructor's code also carries its class's field initializers.
	if strings.HasPrefix(key, "new ") {
		for _, d := range file.decls {
			if d.scope != scope {
				continue
			}
			for _, k := range d.keys {
				if strings.HasPrefix(k, "init:") {
					decls = append(decls, d)
					break
				}
			}
		}
	}
	ds := declSource{found: true, owner: o, lits: map[string]bool{}, idents: map[string]bool{}}
	src := x.kernel.sources[fileURI]
	for _, d := range decls {
		for l := range file.literals(d.start, d.end) {
			ds.lits[l] = true
		}
		for id := range file.idents(d.start, d.end) {
			ds.idents[id] = true
		}
		b0, b1 := file.span(d)
		ds.spans = append(ds.spans, kernelSpan{uri: fileURI, start: utf16Offset(src, b0), end: utf16Offset(src, b1)})
	}
	return ds
}

// utf16Offset converts a byte offset in UTF-8 source into the UTF-16 code-unit offset kernel uses.
func utf16Offset(src []byte, bytePos int) int {
	if bytePos > len(src) {
		bytePos = len(src)
	}
	n := 0
	for i := 0; i < bytePos; {
		c := src[i]
		switch {
		case c < 0x80:
			i++
			n++
		case c < 0xE0:
			i += 2
			n++
		case c < 0xF0:
			i += 3
			n++
		default:
			i += 4
			n += 2 // a supplementary code point is a surrogate pair
		}
	}
	return n
}

// mayLeave reports whether the constant s can leave declaration f through a type-flow sink.
//
// A constant leaves a declaration only through its own return value, an argument it passes, or a
// field it stores; every later hop starts at one of those first sinks. So s can have left f only if
// some sink annotated with s is f's own return, or is a parameter / field / constructor that f names
// in its source (a call, a store, a constructor invocation is always by name). Operators and `call`
// can be invoked without their name appearing, so they always count. Anything unclassified counts.
func (x *pathASourceIndex) mayLeave(f declSource, kind, s string) bool {
	if !f.found {
		return true
	}
	isCtor := kind == "constructor" || kind == "factory"
	for _, sink := range x.kernel.tfaSinks[s] {
		switch sink.kind {
		case sinkReturn:
			for _, sp := range f.spans {
				if sink.span.uri == "" || sink.span.whole || (sink.span.uri == sp.uri && sink.span.start < sp.end && sp.start < sink.span.end) {
					return true
				}
			}
		case sinkParam:
			name := sink.name
			if bar := strings.LastIndexByte(name, '|'); bar >= 0 {
				name = strings.TrimPrefix(strings.TrimPrefix(name[bar+1:], "get#"), "set#")
			}
			if name == "" || name == "call" || !isDartIdentifier(name) || f.idents[name] {
				return true
			}
		case sinkCtorParam:
			if sink.name == "" || f.idents[sink.name] || (isCtor && (f.idents["super"] || f.idents["this"])) {
				return true
			}
		case sinkField:
			if f.idents[sink.name] {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func isDartIdentifier(s string) bool {
	if s == "" || !isDartIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isDartIdentPart(s[i]) {
			return false
		}
	}
	return true
}

// annotatedWith reports whether any type-flow annotation carrying s may lie inside the declaration.
func (x *pathASourceIndex) annotatedWith(ds declSource, s string) bool {
	if len(ds.spans) == 0 {
		return true
	}
	for _, a := range x.kernel.tfaSpans[s] {
		for _, d := range ds.spans {
			if a.uri == "" || d.uri == "" {
				return true
			}
			if a.uri != d.uri {
				continue
			}
			if a.whole || d.whole || (a.start < d.end && d.start < a.end) {
				return true
			}
		}
	}
	return false
}

// constExplains reports whether a const named in the declaration carries S.
func (x *pathASourceIndex) constExplains(ds declSource, s string) bool {
	uris := []string{ds.owner.script, ds.owner.library}
	uris = append(uris, x.imports[ds.owner.library]...)
	seen := map[string]bool{}
	for _, uri := range uris {
		if uri == "" || seen[uri] {
			continue
		}
		seen[uri] = true
		cl := x.constLits(uri)
		if cl == nil {
			continue
		}
		for id := range ds.idents {
			if cl[id][s] {
				return true
			}
		}
	}
	return false
}

type pathAVerdict struct {
	explained bool
	why       string
}

// loadLibraryImports reads the import edges the analyzer recorded for the base (symbol_graph.json).
// Missing or unreadable means no imported consts are consulted, which only ever explains LESS.
func loadLibraryImports(path string) map[string][]string {
	out := map[string][]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	var doc struct {
		LibraryGraph []struct {
			Library string   `json:"library"`
			Imports []string `json:"imports"`
		} `json:"libraryGraph"`
	}
	if json.NewDecoder(f).Decode(&doc) != nil {
		return map[string][]string{}
	}
	for _, l := range doc.LibraryGraph {
		out[l.Library] = l.Imports
	}
	return out
}

// analyzeValuePropagationPathA is the precise analysis. It needs the base's object graph and its
// app.dill; symbol_graph.json only widens rule 5 to consts of imported libraries.
// replaced names every declaration this patch replaces (its changed set, including callers an R8
// dependency map added), as pathAReplacedKey identities. A constant that sits in the pool of replaced
// code cannot be a silent no-op: that code is itself redirected to the patch module.
func analyzeValuePropagationPathA(graphPath, kernelPath, symbolGraphPath string, queries []PathAQuery, replaced map[string]bool) (*FreehandValuePropagation, error) {
	out := &FreehandValuePropagation{Schema: freehandValuePropagationSchema}
	if len(queries) == 0 {
		return out, nil
	}
	kernel, err := loadFreehandKernel(kernelPath)
	if err != nil {
		return nil, fmt.Errorf("read the base kernel %s: %w", kernelPath, err)
	}
	p, err := loadSnapshotProfile(graphPath)
	if err != nil {
		return nil, err
	}
	g, err := newPathAGraph(p)
	if err != nil {
		return nil, err
	}
	idx := &pathASourceIndex{
		kernel:  kernel,
		imports: loadLibraryImports(symbolGraphPath),
		files:   map[string]*dartFile{},
		consts:  map[string]map[string]map[string]bool{},
	}

	constantPools := map[int][]int{}
	poolConstants := map[int][]int{}
	for i := 0; i < g.n; i++ {
		if p.typ(i) != "ObjectPool" {
			continue
		}
		seen := map[int]bool{}
		for _, t := range g.out(i) {
			if p.typ(t) == constantNodeTypeName && !seen[t] {
				seen[t] = true
				constantPools[t] = append(constantPools[t], i)
				poolConstants[i] = append(poolConstants[i], t)
			}
		}
	}

	type fPool struct {
		q    PathAQuery
		pool int
		fn   int
	}
	var fpools []fPool
	for pool := range poolConstants {
		_, fn := g.poolFunction(pool)
		if fn < 0 {
			continue
		}
		name := p.name(fn)
		for _, q := range queries {
			if unmangleVMName(name) != unmangleVMName(snapshotFunctionNameFor(q.Kind, q.Class, q.VMName)) {
				continue
			}
			o, ok := g.owner(fn)
			if !ok || o.library != q.LibURI || !classMatches(o.class, q.Class) {
				continue
			}
			fpools = append(fpools, fPool{q: q, pool: pool, fn: fn})
		}
	}

	declCache := map[int]declSource{}
	decl := func(fn int) declSource {
		if d, ok := declCache[fn]; ok {
			return d
		}
		d := idx.declaration(g, fn)
		declCache[fn] = d
		return d
	}
	explains := func(ds declSource, s string) bool {
		return ds.found && (ds.lits[s] || idx.constExplains(ds, s))
	}
	label := func(fn, outerFn int) string {
		o, _ := g.owner(outerFn)
		l := declLabel(poolDecl{fn: unmangleVMName(p.name(outerFn)), owner: unmangleVMName(o.class)})
		if fn != outerFn {
			l += " (closure)"
		}
		return l
	}

	for _, fp := range fpools {
		fOuter := g.outer(fp.fn)
		fDecl := decl(fOuter)
		for _, c := range poolConstants[fp.pool] {
			s := p.name(c)
			if !kernel.hasString(s) {
				continue // rule 2: not a Dart value at all
			}
			// rule 3: s can only have left F through a type-flow sink F feeds. Not for a field
			// initializer: a static final field with a literal initializer is folded by gen_snapshot
			// itself (BuildStaticGet, has_trivial_initializer) with no annotation.
			if fp.q.Kind != "field-initializer" && !kernel.tfaInferredErr() {
				if !kernel.tfaInferred(s) || !idx.mayLeave(fDecl, fp.q.Kind, s) {
					continue
				}
			}
			type hit struct{ label, why string }
			var hits []hit
			for _, other := range constantPools[c] {
				if other == fp.pool {
					continue
				}
				code, gfn := g.poolFunction(other)
				if gfn < 0 {
					continue
				}
				gOuter := g.outer(gfn)
				if gOuter == fOuter {
					continue // rule 1: own code
				}
				ds := decl(gOuter)
				// rule 0 (R8): G is replaced by this very patch -- the dependency map added it because it
				// absorbed changed code -- so the stale constant in G's pool is never executed again.
				if o, ok := g.owner(gOuter); ok && replaced[pathAReplacedKey(o.library, o.class, g.p.name(gOuter))] {
					continue
				}
				var inlined []declSource
				if arr := g.prop(code, "inlined_id_to_function_"); arr >= 0 {
					for _, inl := range g.out(arr) {
						if p.typ(inl) != "Function" {
							continue
						}
						if io := g.outer(inl); io != gOuter {
							inlined = append(inlined, decl(io))
						}
					}
				}
				// rule 4: no annotation carrying s lies in G or in anything inlined into G, so G's
				// copy of s is not a type-flow fold -- whatever put it there, it was not F.
				if !kernel.tfaInferredErr() {
					annotated := idx.annotatedWith(ds, s)
					for _, in := range inlined {
						annotated = annotated || idx.annotatedWith(in, s)
					}
					if !annotated {
						continue
					}
				}
				// rule 5: G (or a function inlined into G, or a const G names) writes s itself.
				if explains(ds, s) {
					continue
				}
				inlinedExplains := false
				for _, in := range inlined {
					if explains(in, s) {
						inlinedExplains = true
						break
					}
				}
				if inlinedExplains {
					continue
				}
				why := "its own source does not contain this literal"
				if !ds.found {
					why = ds.why
				}
				hits = append(hits, hit{label: label(gfn, gOuter), why: why})
			}
			if len(hits) == 0 {
				continue
			}
			sort.Slice(hits, func(i, j int) bool { return hits[i].label < hits[j].label })
			reached := make([]string, 0, len(hits))
			whyByLabel := map[string]string{}
			for _, h := range hits {
				if _, dup := whyByLabel[h.label]; dup {
					continue
				}
				whyByLabel[h.label] = h.why
				reached = append(reached, h.label)
			}
			total := len(reached)
			if len(reached) > maxReachedListed {
				reached = reached[:maxReachedListed]
			}
			whys := make([]string, 0, len(reached))
			for _, r := range reached {
				whys = append(whys, r+": "+whyByLabel[r])
			}
			out.Propagated = append(out.Propagated, FreehandPropagatedValue{
				Function:     snapshotFunctionNameFor(fp.q.Kind, fp.q.Class, fp.q.VMName),
				Owner:        fp.q.Class,
				Constant:     truncateConstant(s),
				Reached:      reached,
				ReachedTotal: total,
				Why:          strings.Join(whys, "; "),
				PathA:        true,
			})
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

// assertFreehandNoFoldedValueForBase is the gate `soroq patch` runs. It uses the precise rule only when
// the base's engine is known to carry Path A and the base kernel can be read; otherwise it is exactly
// assertFreehandNoFoldedValue.
func assertFreehandNoFoldedValueForBase(base *FreehandBaselineMeta, relDir string, decls []changedDecl) error {
	rev, pathA := freehandBasePathA(base)
	graph := filepath.Join(relDir, freehandObjectGraphName)
	kernelPath := filepath.Join(relDir, "app.dill")
	if !pathA {
		return assertFreehandNoFoldedValue(relDir, decls)
	}
	if _, err := os.Stat(graph); err != nil {
		return assertFreehandNoFoldedValue(relDir, decls) // fails closed with the named remedy
	}
	if _, err := os.Stat(kernelPath); err != nil {
		fmt.Fprintf(os.Stderr, "NOTICE: base %s has no app.dill; using the conservative constant-propagation rule\n", rev)
		return assertFreehandNoFoldedValue(relDir, decls)
	}
	queries := make([]PathAQuery, 0, len(decls))
	for _, d := range decls {
		lib, _, vmName, err := splitIdentity(d.manifestLine)
		if err != nil {
			continue
		}
		queries = append(queries, PathAQuery{
			PropagationQuery: PropagationQuery{Kind: d.keyKind, Class: d.keyClass, VMName: vmName},
			LibURI:           lib,
		})
	}
	replaced := map[string]bool{}
	for _, d := range decls {
		if lib, class, vmName, err := splitIdentity(d.manifestLine); err == nil {
			replaced[pathAReplacedKey(lib, class, vmName)] = true
		}
	}
	vp, err := analyzeValuePropagationPathA(graph, kernelPath, filepath.Join(relDir, "symbol_graph.json"), queries, replaced)
	if err != nil {
		// Precision is an optimisation; the conservative rule is the floor.
		fmt.Fprintf(os.Stderr, "NOTICE: precise constant-propagation analysis unavailable (%v); using the conservative rule\n", err)
		return assertFreehandNoFoldedValue(relDir, decls)
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

// pathAReplacedKey normalizes a declaration to `<library>::<class>::<member>` so a snapshot-profile
// function (class `::` for top level, `@<key>` private suffix, `new ` constructor prefix) and a
// manifest identity compare equal.
func pathAReplacedKey(library, class, name string) string {
	if class == "::" {
		class = ""
	}
	if i := strings.IndexByte(class, '@'); i > 0 {
		class = class[:i]
	}
	name = strings.TrimPrefix(name, "new ")
	return library + "::" + class + "::" + name
}
