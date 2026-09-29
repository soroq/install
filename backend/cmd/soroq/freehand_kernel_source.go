package main

// [soroq] Read the two things the precise fold check needs out of the base's own kernel (app.dill):
// its STRING TABLE and its embedded SOURCE TABLE.
//
// app.dill is the exact kernel gen_snapshot compiled for the base (its sha256 is bound into
// baseline.json), and the frontend embeds the source text of every library and part it compiled,
// SDK patch files included. That makes it the only source text that is guaranteed to be the BASE
// version: the project's working tree already carries the patch being checked.
//
// Only the fixed-size component index, the source table and the string table are decoded. Nothing
// here walks the AST, so a kernel format change that moves those tables is caught by the magic /
// version / bounds checks below and turns into an error, which the caller treats as "cannot be
// precise" and falls back to the conservative rule.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
)

const (
	kernelMagic = 0x90ABCDEF
	// kernelFormatVersion is the only binary format whose index layout this reader has been checked
	// against (pkg/kernel/binary.md, BinaryFormatVersion = 130 for Dart d684a576 / Flutter 3.44.9).
	kernelFormatVersion = 130
)

// freehandKernel is the decoded view of a base app.dill.
type freehandKernel struct {
	strings map[string]struct{}
	// tfaStrings is every string that type-flow analysis inferred as the constant value of some
	// expression, field, parameter or return (vm.inferred-*-type.metadata), including strings nested
	// in an inferred composite constant. Every interprocedural constant gen_snapshot folds without
	// inlining comes from one of those annotations. tfaErr is non-nil when the metadata could not be
	// decoded completely; tfaStrings is then unusable and the caller must not rely on it.
	tfaStrings map[string]struct{}
	tfaErr     error
	// tfaSpans says WHERE each such string was inferred: the source span (UTF-16 offsets, as kernel
	// records them) of the member whose kernel holds the annotation. A span is the enclosing
	// procedure when there is one, else the enclosing class, else the whole library file -- always a
	// superset of the annotated node.
	tfaSpans map[string][]kernelSpan
	// tfaSinks are the annotations through which a constant can LEAVE the declaration that produced
	// it: a procedure's inferred return, a member parameter's inferred argument type, a field's
	// inferred type. Every other annotation consumes a value that entered through one of these.
	tfaSinks map[string][]tfaSink
	// sources maps the IMPORT URI of every embedded source (package:x/y.dart, dart:core,
	// dart:core-patch/string_patch.dart, ...) to its text. The gen_snapshot object graph names
	// scripts by exactly this URI.
	sources map[string][]byte
}

// tfaInferred reports whether s may be a TFA-inferred constant. Without decodable metadata every
// string may be.
func (k *freehandKernel) tfaInferred(s string) bool {
	if k.tfaErr != nil {
		return true
	}
	_, ok := k.tfaStrings[s]
	return ok
}

// tfaInferredErr reports whether the inferred-type metadata could not be decoded, in which case
// neither tfaInferred nor tfaSpans can narrow anything.
func (k *freehandKernel) tfaInferredErr() bool { return k.tfaErr != nil }

func (k *freehandKernel) hasString(s string) bool {
	_, ok := k.strings[s]
	return ok
}

type kernelReader struct {
	b   []byte
	off int
	err error
}

func (r *kernelReader) fail(format string, a ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(format, a...)
	}
}

func (r *kernelReader) uint() int {
	if r.err != nil {
		return 0
	}
	if r.off >= len(r.b) {
		r.fail("kernel: truncated UInt at %d", r.off)
		return 0
	}
	b0 := r.b[r.off]
	switch {
	case b0&0x80 == 0:
		r.off++
		return int(b0)
	case b0&0xC0 == 0x80:
		if r.off+2 > len(r.b) {
			r.fail("kernel: truncated UInt14 at %d", r.off)
			return 0
		}
		v := int(b0&0x3F)<<8 | int(r.b[r.off+1])
		r.off += 2
		return v
	default:
		if r.off+4 > len(r.b) {
			r.fail("kernel: truncated UInt30 at %d", r.off)
			return 0
		}
		v := int(b0&0x3F)<<24 | int(r.b[r.off+1])<<16 | int(r.b[r.off+2])<<8 | int(r.b[r.off+3])
		r.off += 4
		return v
	}
}

func (r *kernelReader) bytes() []byte {
	n := r.uint()
	if r.err != nil {
		return nil
	}
	if n < 0 || r.off+n > len(r.b) {
		r.fail("kernel: byte list of %d overruns at %d", n, r.off)
		return nil
	}
	out := r.b[r.off : r.off+n]
	r.off += n
	return out
}

// loadFreehandKernel decodes every component concatenated into the file (a dill may hold several; the
// component index is found from the END of each one).
func loadFreehandKernel(path string) (*freehandKernel, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseFreehandKernel(raw)
}

func parseFreehandKernel(raw []byte) (*freehandKernel, error) {
	k := &freehandKernel{strings: map[string]struct{}{}, sources: map[string][]byte{},
		tfaStrings: map[string]struct{}{}, tfaSpans: map[string][]kernelSpan{}, tfaSinks: map[string][]tfaSink{}}
	u32 := func(o int) (int, error) {
		if o < 0 || o+4 > len(raw) {
			return 0, fmt.Errorf("kernel: offset %d out of range", o)
		}
		return int(binary.BigEndian.Uint32(raw[o : o+4])), nil
	}
	end := len(raw)
	components := 0
	for end > 0 {
		size, err := u32(end - 4)
		if err != nil {
			return nil, err
		}
		start := end - size
		if size <= 0 || start < 0 {
			return nil, fmt.Errorf("kernel: component size %d does not fit a %d-byte file", size, end)
		}
		magic, err := u32(start)
		if err != nil {
			return nil, err
		}
		version, err := u32(start + 4)
		if err != nil {
			return nil, err
		}
		if magic != kernelMagic {
			return nil, fmt.Errorf("kernel: bad magic %#x", magic)
		}
		if version != kernelFormatVersion {
			return nil, fmt.Errorf("kernel: format version %d, this reader knows only %d", version, kernelFormatVersion)
		}
		libCount, err := u32(end - 8)
		if err != nil {
			return nil, err
		}
		// ComponentIndex: 8 fixed UInt32 offsets, mainMethodReference, libraryOffsets[libCount+1],
		// libraryCount, componentFileSizeInBytes.
		idx := end - (libCount+12)*4
		if idx < start {
			return nil, fmt.Errorf("kernel: component index underruns the component")
		}
		field := func(i int) (int, error) { return u32(idx + 4*i) }
		srcOff, err := field(0)
		if err != nil {
			return nil, err
		}
		constOff, err := field(1)
		if err != nil {
			return nil, err
		}
		constIdxOff, err := field(2)
		if err != nil {
			return nil, err
		}
		payloadOff, err := field(4)
		if err != nil {
			return nil, err
		}
		mappingOff, err := field(5)
		if err != nil {
			return nil, err
		}
		strOff, err := field(6)
		if err != nil {
			return nil, err
		}
		idxStart, err := field(7)
		if err != nil {
			return nil, err
		}
		if srcOff <= 0 || strOff <= srcOff || idxStart < strOff || start+idxStart > end {
			return nil, fmt.Errorf("kernel: inconsistent component index (source %d, strings %d, index %d)", srcOff, strOff, idxStart)
		}

		// Source table: UInt32 count, then SourceInfo records.
		n, err := u32(start + srcOff)
		if err != nil {
			return nil, err
		}
		r := &kernelReader{b: raw[:start+strOff], off: start + srcOff + 4}
		sourceImports := make([]string, 0, n)
		for i := 0; i < n && r.err == nil; i++ {
			_ = r.bytes() // uri
			src := r.bytes()
			lines := r.uint()
			for j := 0; j < lines && r.err == nil; j++ {
				r.uint()
			}
			importURI := r.bytes()
			ctors := r.uint()
			for j := 0; j < ctors && r.err == nil; j++ {
				r.uint()
			}
			sourceImports = append(sourceImports, string(importURI))
			if r.err == nil && len(importURI) > 0 && len(src) > 0 {
				k.sources[string(importURI)] = src
			}
		}
		if r.err != nil {
			return nil, fmt.Errorf("kernel source table: %w", r.err)
		}

		// String table: List<UInt> endOffsets, then the utf8 bytes.
		sr := &kernelReader{b: raw[:start+idxStart], off: start + strOff}
		count := sr.uint()
		ends := make([]int, 0, count)
		for i := 0; i < count && sr.err == nil; i++ {
			ends = append(ends, sr.uint())
		}
		if sr.err != nil {
			return nil, fmt.Errorf("kernel string table: %w", sr.err)
		}
		base := sr.off
		prev := 0
		for _, e := range ends {
			if e < prev || base+e > start+idxStart {
				return nil, errors.New("kernel string table: offsets out of order or out of range")
			}
			k.strings[string(raw[base+prev:base+e])] = struct{}{}
			prev = e
		}
		if k.tfaErr == nil {
			var stringsByIndex []string
			prev = 0
			for _, e := range ends {
				stringsByIndex = append(stringsByIndex, string(raw[base+prev:base+e]))
				prev = e
			}
			ranges, rerr := kernelMemberRanges(raw, start, idx, libCount, sourceImports, stringsByIndex)
			if rerr != nil {
				k.tfaErr = rerr
			} else {
				k.tfaErr = collectTFAStrings(raw, start, constOff, constIdxOff, payloadOff, mappingOff, strOff,
					stringsByIndex, ranges, k)
			}
		}
		components++
		end = start
	}
	if components == 0 {
		return nil, errors.New("kernel: no component found")
	}
	return k, nil
}

var tfaMetadataTags = map[string]bool{
	"vm.inferred-type.metadata":        true,
	"vm.inferred-arg-type.metadata":    true,
	"vm.inferred-return-type.metadata": true,
}

const (
	inferredFlagConstant  = 1 << 3
	inferredFlagExactType = 1 << 6
)

// collectTFAStrings decodes the three inferred-type metadata repositories of one component and adds
// every string reachable from an inferred constant to out. Any construct it does not understand is an
// error: the caller then treats every string as possibly inferred.
func collectTFAStrings(raw []byte, start, constOff, constIdxOff, payloadOff, mappingOff, strOff int, strs []string,
	ranges *kernelRanges, k *freehandKernel) error {
	out := map[int][]string{} // constant index -> strings reachable from it
	u32 := func(o int) (int, error) {
		if o < 0 || o+4 > len(raw) {
			return 0, fmt.Errorf("kernel metadata: offset %d out of range", o)
		}
		return int(binary.BigEndian.Uint32(raw[o : o+4])), nil
	}
	// The constant table index is UInt32[count] offsets relative to the constant table; the count is
	// read from the head of the constant table itself.
	cstart := start + constIdxOff
	ct := &kernelReader{b: raw, off: start + constOff}
	constCount := ct.uint()
	if ct.err != nil {
		return ct.err
	}
	constOffset := func(i int) (int, error) {
		if i < 0 || i >= constCount {
			return 0, fmt.Errorf("kernel metadata: constant index %d out of %d", i, constCount)
		}
		rel, err := u32(cstart + 4*i)
		if err != nil {
			return 0, err
		}
		return start + constOff + rel, nil
	}
	var visitConst func(i int, depth int) ([]string, error)
	visitConst = func(i int, depth int) ([]string, error) {
		if got, ok := out[i]; ok {
			return got, nil
		}
		out[i] = nil // cycle guard; constants form a DAG, so this is only defensive
		if depth > 256 {
			return nil, errors.New("kernel metadata: constant nesting too deep")
		}
		o, err := constOffset(i)
		if err != nil {
			return nil, err
		}
		r := &kernelReader{b: raw, off: o}
		if r.off >= len(raw) {
			return nil, errors.New("kernel metadata: constant out of range")
		}
		tag := raw[r.off]
		r.off++
		refs := []int{}
		switch tag {
		case 0, 1, 2, 3, 5, 9, 10, 11, 14, 15, 16:
			// null, bool, int, double, symbol, instantiation (of a tear-off), tear-offs, type literal:
			// none can carry a CanonicalString a pool would load.
			return nil, nil
		case 4: // StringConstant
			si := r.uint()
			if r.err != nil || si < 0 || si >= len(strs) {
				return nil, fmt.Errorf("kernel metadata: bad string reference in constant %d", i)
			}
			out[i] = []string{strs[si]}
			return out[i], nil
		case 6: // MapConstant: keyType, valueType, List<Pair<ref, ref>>
			skipDartType(r, 0)
			skipDartType(r, 0)
			n := r.uint()
			for j := 0; j < n && r.err == nil; j++ {
				refs = append(refs, r.uint(), r.uint())
			}
		case 7, 13: // ListConstant / SetConstant: type, List<ref>
			skipDartType(r, 0)
			n := r.uint()
			for j := 0; j < n && r.err == nil; j++ {
				refs = append(refs, r.uint())
			}
		case 8: // InstanceConstant: class, List<DartType>, List<Pair<FieldReference, ref>>
			r.uint()
			n := r.uint()
			for j := 0; j < n && r.err == nil; j++ {
				skipDartType(r, 0)
			}
			m := r.uint()
			for j := 0; j < m && r.err == nil; j++ {
				r.uint()
				refs = append(refs, r.uint())
			}
		case 17: // RecordConstant: List<ref>, List<Pair<StringReference, ref>>, type
			n := r.uint()
			for j := 0; j < n && r.err == nil; j++ {
				refs = append(refs, r.uint())
			}
			m := r.uint()
			for j := 0; j < m && r.err == nil; j++ {
				r.uint()
				refs = append(refs, r.uint())
			}
		default:
			return nil, fmt.Errorf("kernel metadata: constant tag %d is not understood", tag)
		}
		if r.err != nil {
			return nil, r.err
		}
		var acc []string
		for _, ref := range refs {
			sub, err := visitConst(ref, depth+1)
			if err != nil {
				return nil, err
			}
			acc = append(acc, sub...)
		}
		out[i] = acc
		return acc, nil
	}

	// Metadata mappings are an RList read from the end (see ast_from_binary _readMetadataMappings).
	endOffset := start + strOff
	count, err := u32(endOffset - 4)
	if err != nil {
		return err
	}
	endOffset -= 4
	for i := 0; i < count; i++ {
		mappingLength, err := u32(endOffset - 4)
		if err != nil {
			return err
		}
		mappingStart := (endOffset - 4) - 8*mappingLength
		if mappingStart-4 < start+mappingOff {
			return errors.New("kernel metadata: mapping overruns its section")
		}
		tagIdx, err := u32(mappingStart - 4)
		if err != nil {
			return err
		}
		if tagIdx < 0 || tagIdx >= len(strs) {
			return errors.New("kernel metadata: bad mapping tag")
		}
		if repo := strs[tagIdx]; tfaMetadataTags[repo] {
			for j := 0; j < mappingLength; j++ {
				nodeOff, err := u32(mappingStart + 8*j)
				if err != nil {
					return err
				}
				rel, err := u32(mappingStart + 8*j + 4)
				if err != nil {
					return err
				}
				r := &kernelReader{b: raw, off: start + payloadOff + rel}
				flags := r.uint()
				if flags&inferredFlagExactType != 0 {
					skipDartType(r, 0)
				} else {
					r.uint() // nullable concrete class reference
				}
				if r.err != nil {
					return r.err
				}
				if flags&inferredFlagConstant != 0 {
					ci := r.uint()
					if r.err != nil {
						return r.err
					}
					found, err := visitConst(ci, 0)
					if err != nil {
						return err
					}
					if len(found) > 0 {
						span := ranges.lookup(nodeOff)
						sink := classifyTFASink(raw, start, nodeOff, repo, ranges, strs)
						for _, str := range found {
							k.tfaStrings[str] = struct{}{}
							k.tfaSpans[str] = append(k.tfaSpans[str], span)
							if sink.kind != sinkConsumer {
								k.tfaSinks[str] = append(k.tfaSinks[str], sink)
							}
						}
					}
				}
			}
		}
		endOffset = mappingStart - 4
	}
	return nil
}

// skipDartType advances past one DartType (pkg/kernel/binary.md). An unknown tag, or a type
// parameter carrying annotations, sets r.err.
func skipDartType(r *kernelReader, depth int) {
	if r.err != nil {
		return
	}
	if depth > 64 {
		r.fail("kernel: DartType nesting too deep")
		return
	}
	if r.off >= len(r.b) {
		r.fail("kernel: truncated DartType")
		return
	}
	tag := r.b[r.off]
	r.off++
	byte1 := func() {
		if r.off >= len(r.b) {
			r.fail("kernel: truncated DartType")
			return
		}
		r.off++
	}
	list := func() {
		n := r.uint()
		for i := 0; i < n && r.err == nil; i++ {
			skipDartType(r, depth+1)
		}
	}
	named := func() {
		n := r.uint()
		for i := 0; i < n && r.err == nil; i++ {
			r.uint() // name
			skipDartType(r, depth+1)
			byte1() // flags
		}
	}
	switch tag {
	case 90, 91, 92, 152: // invalid, dynamic, void, Null
	case 98: // Never
		byte1()
	case 93, 87: // InterfaceType, TypedefType: nullability, reference, List<DartType>
		byte1()
		r.uint()
		list()
	case 96: // SimpleInterfaceType
		byte1()
		r.uint()
	case 94: // FunctionType
		byte1()
		n := r.uint() // List<TypeParameter>
		for i := 0; i < n && r.err == nil; i++ {
			byte1() // flags
			if ann := r.uint(); ann != 0 {
				r.fail("kernel: annotated type parameter in a DartType")
				return
			}
			byte1()  // variance
			r.uint() // name
			skipDartType(r, depth+1)
			skipDartType(r, depth+1)
		}
		r.uint() // requiredParameterCount
		r.uint() // totalParameterCount
		list()
		named()
		skipDartType(r, depth+1)
	case 97: // SimpleFunctionType
		byte1()
		list()
		skipDartType(r, depth+1)
	case 100: // RecordType
		byte1()
		list()
		named()
	case 95: // TypeParameterType
		byte1()
		r.uint()
	case 99: // IntersectionType
		skipDartType(r, depth+1)
		skipDartType(r, depth+1)
	case 103: // ExtensionType
		byte1()
		r.uint()
		list()
		skipDartType(r, depth+1)
	case 107: // FutureOrType
		byte1()
		skipDartType(r, depth+1)
	default:
		r.fail("kernel: DartType tag %d is not understood", tag)
	}
}

// kernelSpan is a source range in one file, in kernel file offsets (UTF-16 code units). whole means
// the entire file.
type kernelSpan struct {
	uri        string
	start, end int
	whole      bool
}

type kernelRange struct {
	lo, hi int // component-relative binary range [lo, hi)
	span   kernelSpan
	name   string // procedure or class name; "" for a library
}

// kernelRanges maps a component-relative node offset to the innermost member that contains it.
type kernelRanges struct {
	procs, classes, libs []kernelRange // each sorted by lo, non-overlapping within its level
}

func (k *kernelRanges) lookup(off int) kernelSpan {
	_, r, ok := k.innermost(off)
	if !ok {
		return kernelSpan{whole: true} // unknown file: overlaps everything
	}
	return r.span
}

// innermost returns the level ("procedure", "class", "library") and range containing off.
func (k *kernelRanges) innermost(off int) (string, kernelRange, bool) {
	for _, lv := range []struct {
		name string
		rs   []kernelRange
	}{{"procedure", k.procs}, {"class", k.classes}, {"library", k.libs}} {
		i := sort.Search(len(lv.rs), func(i int) bool { return lv.rs[i].hi > off })
		if i < len(lv.rs) && lv.rs[i].lo <= off {
			return lv.name, lv.rs[i], true
		}
	}
	return "", kernelRange{}, false
}

// kernelMemberRanges reads the library and class indexes (pkg/kernel/binary.md) and the fixed-size
// header of every class and procedure to learn which source span each binary range belongs to.
func kernelMemberRanges(raw []byte, start, idx, libCount int, sourceImports []string, strs []string) (*kernelRanges, error) {
	u32 := func(o int) (int, error) {
		if o < 0 || o+4 > len(raw) {
			return 0, fmt.Errorf("kernel index: offset %d out of range", o)
		}
		return int(binary.BigEndian.Uint32(raw[o : o+4])), nil
	}
	uriOf := func(i int) string {
		if i >= 0 && i < len(sourceImports) {
			return sourceImports[i]
		}
		return ""
	}
	// header reads tag, canonicalName, fileUri, startFileOffset, fileOffset, fileEndOffset.
	// header reads a class (tag 2) or procedure (tag 6) header up to its name.
	header := func(off int, wantTag byte) (kernelSpan, string, error) {
		if off < 0 || off >= len(raw) || raw[off] != wantTag {
			return kernelSpan{}, "", fmt.Errorf("kernel index: expected tag %d at %d", wantTag, off)
		}
		r := &kernelReader{b: raw, off: off + 1}
		r.uint() // canonical name
		uri := uriOf(r.uint())
		startOff := r.uint() - 1
		r.uint()
		endOff := r.uint() - 1
		if wantTag == 6 {
			r.off += 2 // kind, stubKind
		}
		r.uint() // flags
		nameIdx := r.uint()
		if r.err != nil {
			return kernelSpan{}, "", r.err
		}
		name := ""
		if nameIdx >= 0 && nameIdx < len(strs) {
			name = strs[nameIdx]
		}
		if startOff < 0 || endOff < startOff {
			return kernelSpan{uri: uri, whole: true}, name, nil
		}
		return kernelSpan{uri: uri, start: startOff, end: endOff}, name, nil
	}
	out := &kernelRanges{}
	libOffsets := make([]int, libCount+1)
	for i := range libOffsets {
		v, err := u32(idx + 4*(9+i))
		if err != nil {
			return nil, err
		}
		libOffsets[i] = v
	}
	for li := 0; li < libCount; li++ {
		ls, le := libOffsets[li], libOffsets[li+1]
		// library header: flags, languageVersionMajor, languageVersionMinor, canonicalName, name, fileUri
		lr := &kernelReader{b: raw, off: start + ls + 1}
		lr.uint()
		lr.uint()
		lr.uint()
		lr.uint()
		libURI := uriOf(lr.uint())
		if lr.err != nil {
			return nil, lr.err
		}
		out.libs = append(out.libs, kernelRange{lo: ls, hi: le, span: kernelSpan{uri: libURI, whole: true}})
		pc, err := u32(start + le - 4)
		if err != nil {
			return nil, err
		}
		pbase := start + le - 4 - (pc+1)*4
		cc, err := u32(pbase - 4)
		if err != nil {
			return nil, err
		}
		cbase := pbase - 4 - (cc+1)*4
		for i := 0; i < pc; i++ {
			lo, err1 := u32(pbase + 4*i)
			hi, err2 := u32(pbase + 4*(i+1))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("kernel index: library %d procedure %d", li, i)
			}
			sp, name, err := header(start+lo, 6)
			if err != nil {
				return nil, err
			}
			out.procs = append(out.procs, kernelRange{lo: lo, hi: hi, span: sp, name: name})
		}
		for i := 0; i < cc; i++ {
			lo, err1 := u32(cbase + 4*i)
			hi, err2 := u32(cbase + 4*(i+1))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("kernel index: library %d class %d", li, i)
			}
			sp, name, err := header(start+lo, 2)
			if err != nil {
				return nil, err
			}
			out.classes = append(out.classes, kernelRange{lo: lo, hi: hi, span: sp, name: name})
			// class procedure index at the class's end
			cpc, err := u32(start + hi - 4)
			if err != nil {
				return nil, err
			}
			cpbase := start + hi - 4 - (cpc+1)*4
			for j := 0; j < cpc; j++ {
				plo, e1 := u32(cpbase + 4*j)
				phi, e2 := u32(cpbase + 4*(j+1))
				if e1 != nil || e2 != nil {
					return nil, fmt.Errorf("kernel index: class procedure %d", j)
				}
				psp, pname, err := header(start+plo, 6)
				if err != nil {
					return nil, err
				}
				out.procs = append(out.procs, kernelRange{lo: plo, hi: phi, span: psp, name: pname})
			}
		}
	}
	for _, rs := range [][]kernelRange{out.procs, out.classes, out.libs} {
		sort.Slice(rs, func(i, j int) bool { return rs[i].lo < rs[j].lo })
	}
	return out, nil
}

type tfaSinkKind int

const (
	sinkConsumer  tfaSinkKind = iota // an expression or local: consumes a value, never a first hop
	sinkReturn                       // a procedure's inferred return value
	sinkParam                        // a procedure parameter
	sinkCtorParam                    // a constructor parameter (named by its class)
	sinkField                        // a field's inferred value
	sinkUnknown                      // could not be classified: treated as reachable from anything
)

type tfaSink struct {
	kind tfaSinkKind
	span kernelSpan // the member the annotation belongs to
	// name is what a declaration must mention to feed this sink: the procedure name for a return or
	// a parameter, the class name for a constructor parameter, the field name for a field.
	name string
}

// classifyTFASink decides what kind of node an inferred-type annotation sits on. It needs no AST
// walk: return annotations sit on procedures, argument annotations on member parameters, and a field
// is recognised by its tag outside any procedure. A misclassification can only ADD a spurious sink
// (a local taken for a field); it can never hide a real one, because every real field carries tag 4.
func classifyTFASink(raw []byte, start, nodeOff int, repo string, ranges *kernelRanges, strs []string) tfaSink {
	level, rng, ok := ranges.innermost(nodeOff)
	if !ok {
		return tfaSink{kind: sinkUnknown}
	}
	switch repo {
	case "vm.inferred-return-type.metadata":
		if level == "procedure" {
			return tfaSink{kind: sinkReturn, span: rng.span, name: rng.name}
		}
		return tfaSink{kind: sinkUnknown, span: rng.span}
	case "vm.inferred-arg-type.metadata":
		switch level {
		case "procedure":
			return tfaSink{kind: sinkParam, span: rng.span, name: rng.name}
		case "class":
			return tfaSink{kind: sinkCtorParam, span: rng.span, name: rng.name}
		}
		return tfaSink{kind: sinkUnknown, span: rng.span}
	default: // vm.inferred-type.metadata
		if level == "procedure" {
			return tfaSink{kind: sinkConsumer}
		}
		o := start + nodeOff
		if o < len(raw) && raw[o] == 4 {
			r := &kernelReader{b: raw, off: o + 1}
			r.uint() // canonicalNameField
			r.uint() // canonicalNameGetter
			r.uint() // canonicalNameSetter
			r.uint() // fileUri
			r.uint() // fileOffset
			r.uint() // fileEndOffset
			r.uint() // flags
			ni := r.uint()
			if r.err == nil && ni >= 0 && ni < len(strs) && strs[ni] != "" {
				return tfaSink{kind: sinkField, span: rng.span, name: strs[ni]}
			}
		}
		// Not a field: a captured local or an expression in a constructor or initializer; a consumer.
		return tfaSink{kind: sinkConsumer}
	}
}
