package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal kernel component writer, shaped by pkg/kernel/binary.md (format 130), producing exactly
// the sections loadFreehandKernel reads: libraries with their class / procedure indexes, the source
// table, the constant table and its index, inferred-type metadata, and the string table. Everything
// else a real component carries is absent, which the reader never looks at.

type kfProc struct {
	name       string
	uri        string // import URI of the file it is declared in
	start, end int    // UTF-16 file offsets of the declaration
}

type kfField struct {
	name string
	uri  string
}

type kfClass struct {
	name       string
	uri        string
	start, end int
	fields     []kfField
	procs      []kfProc
}

type kfLib struct {
	uri     string
	classes []kfClass
	procs   []kfProc
}

// kfMeta annotates one node with a string constant. target names the node:
//
//	"proc:<name>"           the procedure itself            (use with the return repository)
//	"param:<name>"          a parameter inside that procedure (arg repository)
//	"expr:<name>"           an expression inside that procedure (type repository)
//	"field:<class>.<name>"  the field node                  (type repository)
//	"ctorparam:<class>"     a constructor parameter          (arg repository)
type kfMeta struct {
	repo   string // "type", "arg", "return"
	target string
	value  string
}

type kernelFixture struct {
	sources map[string]string
	libs    []kfLib
	meta    []kfMeta
	extra   []string // strings present in the string table without being annotated
}

type kbuf struct{ b []byte }

func (k *kbuf) byte1(v byte) { k.b = append(k.b, v) }
func (k *kbuf) u32(v int)    { k.b = binary.BigEndian.AppendUint32(k.b, uint32(v)) }
func (k *kbuf) uint(v int) {
	switch {
	case v < 0x80:
		k.b = append(k.b, byte(v))
	case v < 0x4000:
		k.b = append(k.b, byte(0x80|v>>8), byte(v))
	default:
		k.b = append(k.b, byte(0xC0|v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
}
func (k *kbuf) bytes(p []byte) { k.uint(len(p)); k.b = append(k.b, p...) }

func (f *kernelFixture) write(t *testing.T) string {
	t.Helper()
	var strs []string
	strIdx := map[string]int{}
	str := func(s string) int {
		if i, ok := strIdx[s]; ok {
			return i
		}
		strIdx[s] = len(strs)
		strs = append(strs, s)
		return strIdx[s]
	}
	var uris []string
	uriIdx := map[string]int{}
	for u := range f.sources {
		uriIdx[u] = len(uris)
		uris = append(uris, u)
	}
	uriOf := func(u string) int {
		if i, ok := uriIdx[u]; ok {
			return i
		}
		t.Fatalf("fixture uri %q has no source", u)
		return 0
	}
	for _, m := range f.meta {
		str(m.value)
	}
	for _, e := range f.extra {
		str(e)
	}

	out := &kbuf{}
	out.u32(kernelMagic)
	out.u32(kernelFormatVersion)
	nodes := map[string]int{} // target -> component-relative offset
	procNode := func(p kfProc) {
		lo := len(out.b)
		nodes["proc:"+p.name] = lo
		out.byte1(6)
		out.uint(0) // canonical name
		out.uint(uriOf(p.uri))
		out.uint(p.start + 1)
		out.uint(p.start + 1)
		out.uint(p.end + 1)
		out.byte1(0) // kind
		out.byte1(0) // stub kind
		out.uint(0)  // flags
		out.uint(str(p.name))
		nodes["param:"+p.name] = len(out.b)
		out.byte1(0x55) // opaque body bytes stand in for the function node
		nodes["expr:"+p.name] = len(out.b)
		out.byte1(0x55)
		out.byte1(0x55)
	}
	libOffsets := []int{}
	for _, l := range f.libs {
		libOffsets = append(libOffsets, len(out.b))
		out.byte1(0) // flags
		out.uint(3)
		out.uint(0)
		out.uint(0) // canonical name
		out.uint(str(l.uri))
		out.uint(uriOf(l.uri))
		classOffsets := []int{}
		for _, c := range l.classes {
			classOffsets = append(classOffsets, len(out.b))
			out.byte1(2)
			out.uint(0)
			out.uint(uriOf(c.uri))
			out.uint(c.start + 1)
			out.uint(c.start + 1)
			out.uint(c.end + 1)
			out.uint(0)
			out.uint(str(c.name))
			nodes["ctorparam:"+c.name] = len(out.b)
			out.byte1(0x55)
			for _, fl := range c.fields {
				nodes["field:"+c.name+"."+fl.name] = len(out.b)
				out.byte1(4)
				out.uint(0)
				out.uint(0)
				out.uint(0)
				out.uint(uriOf(fl.uri))
				out.uint(1)
				out.uint(1)
				out.uint(0)
				out.uint(str(fl.name))
			}
			procOffsets := []int{}
			for _, p := range c.procs {
				procOffsets = append(procOffsets, len(out.b))
				procNode(p)
			}
			procOffsets = append(procOffsets, len(out.b))
			for _, o := range procOffsets {
				out.u32(o)
			}
			out.u32(len(c.procs))
		}
		classOffsets = append(classOffsets, len(out.b))
		procOffsets := []int{}
		for _, p := range l.procs {
			procOffsets = append(procOffsets, len(out.b))
			procNode(p)
		}
		procOffsets = append(procOffsets, len(out.b))
		out.u32(0) // sourceReferencesOffset
		for _, o := range classOffsets {
			out.u32(o)
		}
		out.u32(len(l.classes))
		for _, o := range procOffsets {
			out.u32(o)
		}
		out.u32(len(l.procs))
	}
	libOffsets = append(libOffsets, len(out.b))

	srcOff := len(out.b)
	out.u32(len(uris))
	for _, u := range uris {
		out.bytes([]byte("file:///fixture/" + u))
		out.bytes([]byte(f.sources[u]))
		out.uint(0)
		out.bytes([]byte(u))
		out.uint(0)
	}

	constOff := len(out.b)
	var constIdx []int
	constOf := map[string]int{}
	var values []string
	for _, m := range f.meta {
		if _, ok := constOf[m.value]; !ok {
			constOf[m.value] = len(values)
			values = append(values, m.value)
		}
	}
	out.uint(len(values))
	for _, v := range values {
		constIdx = append(constIdx, len(out.b)-constOff)
		out.byte1(4)
		out.uint(str(v))
	}
	constIdxOff := len(out.b)
	for _, o := range constIdx {
		out.u32(o)
	}
	out.u32(len(constIdx))
	cnOff := len(out.b)
	out.uint(0)

	payloadOff := len(out.b)
	repos := map[string]string{"type": "vm.inferred-type.metadata", "arg": "vm.inferred-arg-type.metadata", "return": "vm.inferred-return-type.metadata"}
	type pair struct{ node, payload int }
	byRepo := map[string][]pair{}
	for _, m := range f.meta {
		node, ok := nodes[m.target]
		if !ok {
			t.Fatalf("fixture metadata target %q does not exist", m.target)
		}
		rel := len(out.b) - payloadOff
		out.uint(inferredFlagConstant)
		out.uint(0)
		out.uint(constOf[m.value])
		byRepo[m.repo] = append(byRepo[m.repo], pair{node, rel})
	}
	mappingOff := len(out.b)
	nMappings := 0
	for _, r := range []string{"type", "arg", "return"} {
		ps := byRepo[r]
		out.u32(str(repos[r]))
		for _, p := range ps {
			out.u32(p.node)
			out.u32(p.payload)
		}
		out.u32(len(ps))
		nMappings++
	}
	out.u32(nMappings)

	strOff := len(out.b)
	out.uint(len(strs))
	end := 0
	for _, s := range strs {
		end += len(s)
		out.uint(end)
	}
	for _, s := range strs {
		out.b = append(out.b, s...)
	}
	idxStart := len(out.b)
	for _, v := range []int{srcOff, constOff, constIdxOff, cnOff, payloadOff, mappingOff, strOff, idxStart, 0} {
		out.u32(v)
	}
	for _, o := range libOffsets {
		out.u32(o)
	}
	out.u32(len(f.libs))
	out.u32(len(out.b) + 4)

	path := filepath.Join(t.TempDir(), "app.dill")
	if err := os.WriteFile(path, out.b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// span finds the UTF-16 offsets of a snippet in a fixture source, for kfProc / kfClass ranges.
func span(t *testing.T, src, snippet string) (int, int) {
	t.Helper()
	i := strings.Index(src, snippet)
	if i < 0 {
		t.Fatalf("snippet %q not in fixture source", snippet)
	}
	return utf16Offset([]byte(src), i), utf16Offset([]byte(src), i+len(snippet))
}

func TestKernelFixtureRoundTrip(t *testing.T) {
	src := "class A {\n  String f() => 'x';\n}\n"
	s0, s1 := span(t, src, "String f() => 'x';")
	c0, c1 := span(t, src, src[:len(src)-1])
	fx := &kernelFixture{
		sources: map[string]string{"package:app/a.dart": src},
		libs: []kfLib{{uri: "package:app/a.dart", classes: []kfClass{{
			name: "A", uri: "package:app/a.dart", start: c0, end: c1,
			fields: []kfField{{name: "label", uri: "package:app/a.dart"}},
			procs:  []kfProc{{name: "f", uri: "package:app/a.dart", start: s0, end: s1}},
		}}}},
		meta: []kfMeta{
			{repo: "return", target: "proc:f", value: "x"},
			{repo: "type", target: "field:A.label", value: "y"},
			{repo: "type", target: "expr:f", value: "z"},
		},
		extra: []string{"only-in-string-table"},
	}
	k, err := loadFreehandKernel(fx.write(t))
	if err != nil {
		t.Fatal(err)
	}
	if k.tfaErr != nil {
		t.Fatal(k.tfaErr)
	}
	if string(k.sources["package:app/a.dart"]) != src {
		t.Fatal("source table did not round-trip")
	}
	if !k.hasString("only-in-string-table") || k.tfaInferred("only-in-string-table") {
		t.Fatal("string table / inferred set mixed up")
	}
	for _, want := range []struct {
		value string
		kind  tfaSinkKind
		name  string
	}{{"x", sinkReturn, "f"}, {"y", sinkField, "label"}} {
		sinks := k.tfaSinks[want.value]
		if len(sinks) != 1 || sinks[0].kind != want.kind || sinks[0].name != want.name {
			t.Fatalf("%q: sinks %+v", want.value, sinks)
		}
	}
	if len(k.tfaSinks["z"]) != 0 || !k.tfaInferred("z") {
		t.Fatalf("an expression annotation is a consumer, not a sink: %+v", k.tfaSinks["z"])
	}
	if sp := k.tfaSpans["z"]; len(sp) != 1 || sp[0].start != s0 || sp[0].end != s1 {
		t.Fatalf("expression annotation not mapped to its procedure span: %+v", sp)
	}
}

func TestKernelReaderRejectsGarbage(t *testing.T) {
	if _, err := parseFreehandKernel([]byte("not a kernel at all")); err == nil {
		t.Fatal("garbage parsed as kernel")
	}
}
