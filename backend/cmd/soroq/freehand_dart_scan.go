package main

// [soroq] A deliberately small Dart scanner for ONE question: which string literals does a given
// declaration's own source text contain?
//
// It is not a parser. It tokenizes (comments, all string forms including raw, triple-quoted and nested
// interpolation), tracks bracket depth, and splits each class body and the top level into member
// "chunks" -- a chunk ends at a `;` or at the close of a `{` opened at member depth. A chunk's header
// names the declaration. Anything it cannot place is reported as NOT FOUND, and the fold check treats
// a declaration it cannot find as unexplained, so a scanner miss costs a refusal, never a pass.

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type dartTokKind byte

const (
	dtIdent dartTokKind = iota + 1
	dtString
	dtPunct
	dtNumber
)

type dartTok struct {
	kind  dartTokKind
	text  string
	depth int // bracket depth: before an opener, after a closer
	// frags are the literal text pieces of a string token (between interpolations), escapes decoded.
	frags []string
	// pos and end are the token's byte range in the source.
	pos, end int
	// simple marks a string token with no interpolation: adjacent simple strings are concatenated by
	// the compiler into one constant, so the scanner records the concatenation too.
	simple bool
}

type dartScanner struct {
	src  string
	pos  int
	toks []dartTok
	// stack of open brackets: '(', '[', '{'
	stack []byte
	// interp counts enclosing `${...}` interpolations. Tokens inside one are reported far deeper than
	// any real member depth, so an identifier inside a string can never be read as a declaration.
	interp int
}

func isDartIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isDartIdentPart(c byte) bool { return isDartIdentStart(c) || (c >= '0' && c <= '9') }

// scanDart tokenizes the whole source. It never fails: unterminated constructs end at EOF.
func scanDart(src string) []dartTok {
	s := &dartScanner{src: src}
	s.scanUntil(0)
	return s.toks
}

// scanUntil scans tokens until EOF, or -- when stopAtBrace > 0 -- until the `}` that closes an
// interpolation opened at bracket stack height stopAtBrace-1.
func (s *dartScanner) scanUntil(stopAtBrace int) {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			s.pos++
		case c == '/' && s.pos+1 < len(s.src) && s.src[s.pos+1] == '/':
			for s.pos < len(s.src) && s.src[s.pos] != '\n' {
				s.pos++
			}
		case c == '/' && s.pos+1 < len(s.src) && s.src[s.pos+1] == '*':
			s.skipBlockComment()
		case (c == 'r' || c == 'R') && s.pos+1 < len(s.src) && (s.src[s.pos+1] == '\'' || s.src[s.pos+1] == '"'):
			s.pos++
			s.scanString(true)
		case c == '\'' || c == '"':
			s.scanString(false)
		case isDartIdentStart(c):
			st := s.pos
			for s.pos < len(s.src) && isDartIdentPart(s.src[s.pos]) {
				s.pos++
			}
			s.emit(dartTok{kind: dtIdent, text: s.src[st:s.pos], pos: st, end: s.pos})
		case c >= '0' && c <= '9':
			st := s.pos
			for s.pos < len(s.src) && (isDartIdentPart(s.src[s.pos]) || s.src[s.pos] == '.') {
				// stop before a `..` cascade or a `.method` on an int literal
				if s.src[s.pos] == '.' && (s.pos+1 >= len(s.src) || !(s.src[s.pos+1] >= '0' && s.src[s.pos+1] <= '9')) {
					break
				}
				s.pos++
			}
			s.emit(dartTok{kind: dtNumber, text: s.src[st:s.pos], pos: st, end: s.pos})
		case c == '(' || c == '[' || c == '{':
			s.emit(dartTok{kind: dtPunct, text: string(c), pos: s.pos, end: s.pos + 1})
			s.stack = append(s.stack, c)
			s.pos++
		case c == ')' || c == ']' || c == '}':
			if c == '}' && stopAtBrace > 0 && len(s.stack) == stopAtBrace-1 {
				s.pos++
				return
			}
			if len(s.stack) > 0 {
				s.stack = s.stack[:len(s.stack)-1]
			}
			s.emit(dartTok{kind: dtPunct, text: string(c), pos: s.pos, end: s.pos + 1})
			s.pos++
		case strings.IndexByte("=!<>+-*/%&|^?~", c) >= 0:
			st := s.pos
			// `<` and `>` stand alone (generics); everything else takes a maximal operator run.
			if c == '<' || c == '>' {
				s.pos++
				if s.pos < len(s.src) && s.src[s.pos] == '=' {
					s.pos++
				}
			} else {
				for s.pos < len(s.src) && strings.IndexByte("=!+-*/%&|^?~>", s.src[s.pos]) >= 0 {
					if s.pos > st && s.src[s.pos] == '/' && s.pos+1 < len(s.src) && (s.src[s.pos+1] == '/' || s.src[s.pos+1] == '*') {
						break
					}
					s.pos++
					if s.src[s.pos-1] == '>' { // `=>`, `->` end the run
						break
					}
				}
			}
			s.emit(dartTok{kind: dtPunct, text: s.src[st:s.pos], pos: st, end: s.pos})
		default:
			// `.`, `,`, `;`, `:`, `@`, `#` and anything else: one character (a whole rune).
			_, w := utf8.DecodeRuneInString(s.src[s.pos:])
			s.emit(dartTok{kind: dtPunct, text: s.src[s.pos : s.pos+w], pos: s.pos, end: s.pos + w})
			s.pos += w
		}
	}
}

func (s *dartScanner) emit(t dartTok) {
	t.depth = len(s.stack) + 1000*s.interp
	s.toks = append(s.toks, t)
}

func (s *dartScanner) skipBlockComment() {
	depth := 0
	for s.pos < len(s.src) {
		if strings.HasPrefix(s.src[s.pos:], "/*") {
			depth++
			s.pos += 2
			continue
		}
		if strings.HasPrefix(s.src[s.pos:], "*/") {
			depth--
			s.pos += 2
			if depth == 0 {
				return
			}
			continue
		}
		s.pos++
	}
}

func (s *dartScanner) scanString(raw bool) {
	strStart := s.pos
	if raw {
		strStart--
	}
	q := s.src[s.pos]
	triple := strings.HasPrefix(s.src[s.pos:], strings.Repeat(string(q), 3))
	if triple {
		s.pos += 3
		// A triple-quoted string drops its first line when that line is only whitespace.
		j := s.pos
		for j < len(s.src) && (s.src[j] == ' ' || s.src[j] == '\t') {
			j++
		}
		if j < len(s.src) && s.src[j] == '\n' {
			s.pos = j + 1
		} else if j+1 < len(s.src) && s.src[j] == '\r' && s.src[j+1] == '\n' {
			s.pos = j + 2
		}
	} else {
		s.pos++
	}
	var frags []string
	var cur strings.Builder
	interpolated := false
	flush := func() {
		frags = append(frags, cur.String())
		cur.Reset()
	}
	closed := func() bool {
		if triple {
			return strings.HasPrefix(s.src[s.pos:], strings.Repeat(string(q), 3))
		}
		return s.src[s.pos] == q
	}
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		if closed() {
			if triple {
				s.pos += 3
			} else {
				s.pos++
			}
			break
		}
		if !triple && c == '\n' { // unterminated single-line string
			break
		}
		if !raw && c == '\\' && s.pos+1 < len(s.src) {
			s.pos++
			cur.WriteString(s.decodeEscape())
			continue
		}
		if !raw && c == '$' && s.pos+1 < len(s.src) {
			n := s.src[s.pos+1]
			if n == '{' {
				interpolated = true
				flush()
				s.pos += 2
				// Scan the embedded expression as ordinary tokens so its identifiers and nested
				// strings are seen; stop at the matching `}`.
				s.interp++
				s.scanUntil(len(s.stack) + 1)
				s.interp--
				continue
			}
			if isDartIdentStart(n) && n != '$' {
				interpolated = true
				flush()
				s.pos++
				st := s.pos
				for s.pos < len(s.src) && isDartIdentPart(s.src[s.pos]) && s.src[s.pos] != '$' {
					s.pos++
				}
				s.interp++
				s.emit(dartTok{kind: dtIdent, text: s.src[st:s.pos], pos: st, end: s.pos})
				s.interp--
				continue
			}
		}
		cur.WriteByte(c)
		s.pos++
	}
	flush()
	s.emit(dartTok{kind: dtString, frags: frags, simple: !interpolated, pos: strStart, end: s.pos})
}

func (s *dartScanner) decodeEscape() string {
	c := s.src[s.pos]
	s.pos++
	switch c {
	case 'n':
		return "\n"
	case 'r':
		return "\r"
	case 't':
		return "\t"
	case 'b':
		return "\b"
	case 'f':
		return "\f"
	case 'v':
		return "\v"
	case 'x':
		if s.pos+2 <= len(s.src) {
			if v, err := strconv.ParseUint(s.src[s.pos:s.pos+2], 16, 32); err == nil {
				s.pos += 2
				return string(rune(v))
			}
		}
		return "x"
	case 'u':
		if s.pos < len(s.src) && s.src[s.pos] == '{' {
			end := strings.IndexByte(s.src[s.pos:], '}')
			if end > 0 {
				if v, err := strconv.ParseUint(s.src[s.pos+1:s.pos+end], 16, 32); err == nil {
					s.pos += end + 1
					return string(rune(v))
				}
			}
			return "u"
		}
		if s.pos+4 <= len(s.src) {
			if v, err := strconv.ParseUint(s.src[s.pos:s.pos+4], 16, 32); err == nil {
				s.pos += 4
				return string(rune(v))
			}
		}
		return "u"
	case '\n':
		return "\n"
	default:
		return string(c)
	}
}

// dartDecl is one member chunk: which scope it is in, the names it declares, and its token range.
type dartDecl struct {
	scope      string   // enclosing class/mixin/enum/extension name, "" at top level
	keys       []string // normalized declaration keys, see dartDeclKeys
	isConst    bool
	start, end int // token indexes, inclusive start, exclusive end
}

// dartFile is a scanned source file, indexed by declaration.
type dartFile struct {
	toks  []dartTok
	decls []dartDecl
}

func indexDartFile(src string) *dartFile {
	f := &dartFile{toks: scanDart(src)}
	f.index()
	return f
}

type dartScope struct {
	name  string
	depth int // bracket depth at which this scope's members live
}

func (f *dartFile) index() {
	scopes := []dartScope{{name: "", depth: 0}}
	chunkStart := 0
	bodyOpen := false // a `{` opened at member depth whose `}` has not been seen yet
	for i := 0; i < len(f.toks); i++ {
		t := f.toks[i]
		sc := scopes[len(scopes)-1]
		if t.kind == dtPunct && t.text == "}" && t.depth == sc.depth-1 && len(scopes) > 1 {
			// the class body closes: end any dangling chunk (an enum's value list), pop the scope
			f.emitChunk(sc.name, sc.depth, chunkStart, i)
			scopes = scopes[:len(scopes)-1]
			chunkStart = i + 1
			bodyOpen = false
			continue
		}
		if t.depth != sc.depth || t.kind != dtPunct {
			continue
		}
		switch t.text {
		case ";":
			f.emitChunk(sc.name, sc.depth, chunkStart, i+1)
			chunkStart = i + 1
		case "{":
			if name, ok := f.classHeader(sc.depth, chunkStart, i); ok {
				scopes = append(scopes, dartScope{name: name, depth: sc.depth + 1})
				chunkStart = i + 1
				bodyOpen = false
				continue
			}
			bodyOpen = true
		case "}":
			// the closer of a body opened at member depth carries member depth (depth after close)
			if bodyOpen {
				f.emitChunk(sc.name, sc.depth, chunkStart, i+1)
				chunkStart = i + 1
				bodyOpen = false
			}
		}
	}
	if chunkStart < len(f.toks) {
		sc := scopes[len(scopes)-1]
		f.emitChunk(sc.name, sc.depth, chunkStart, len(f.toks))
	}
}

// headerTokens returns the indexes of the tokens at `depth` in [from, to), with annotations
// (`@name(.name)*(args)?`) removed. Annotation arguments sit one level deeper; only their parentheses
// are at this depth.
func (f *dartFile) headerTokens(depth, from, to int) []int {
	var out []int
	for j := from; j < to; j++ {
		t := f.toks[j]
		if t.depth != depth {
			continue
		}
		if t.kind == dtPunct && t.text == "@" {
			j++ // the annotation name
			for j+2 < to && f.toks[j+1].depth == depth && f.toks[j+1].text == "." {
				j += 2
			}
			if j+1 < to && f.toks[j+1].depth == depth && f.toks[j+1].text == "(" {
				j++
				for j+1 < to && !(f.toks[j+1].depth == depth && f.toks[j+1].text == ")") {
					j++
				}
				j++
			}
			continue
		}
		out = append(out, j)
	}
	return out
}

// classHeader reports whether the member-depth tokens before a `{` declare a class-like body.
func (f *dartFile) classHeader(depth, from, brace int) (string, bool) {
	hdr := f.headerTokens(depth, from, brace)
	for hi, j := range hdr {
		t := f.toks[j]
		if t.kind != dtIdent {
			continue
		}
		switch t.text {
		case "class", "mixin", "enum", "extension":
			// A `(`/`=>`/`=` before the keyword means this is a member header, not a class header.
			for _, k := range hdr[:hi] {
				if f.toks[k].kind == dtPunct && (f.toks[k].text == "(" || f.toks[k].text == "=>" || f.toks[k].text == "=") {
					return "", false
				}
			}
			// name: the next identifier that is not a modifier keyword
			for _, k := range hdr[hi+1:] {
				n := f.toks[k]
				if n.kind == dtIdent && n.text == "on" { // unnamed extension
					return "", true
				}
				if n.kind == dtIdent && n.text != "class" && n.text != "type" {
					return n.text, true
				}
			}
			return "", true
		}
	}
	return "", false
}

var dartOperatorNames = map[string]bool{
	"==": true, "[]": true, "[]=": true, "+": true, "-": true, "*": true, "/": true, "~/": true, "%": true,
	"<": true, ">": true, "<=": true, ">=": true, "&": true, "|": true, "^": true, "~": true, "<<": true,
	">>": true, ">>>": true, "unary-": true,
}

func (f *dartFile) emitChunk(scope string, depth, start, end int) {
	if start >= end {
		return
	}
	for j := start; j < end; j++ {
		if f.toks[j].depth != depth {
			continue
		}
		switch f.toks[j].text {
		case "import", "export", "part", "library", "typedef":
			return
		}
		break
	}
	keys, isConst := f.chunkKeys(scope, depth, start, end)
	if len(keys) == 0 {
		return
	}
	f.decls = append(f.decls, dartDecl{scope: scope, keys: keys, isConst: isConst, start: start, end: end})
}

// chunkKeys names a member chunk the way the VM names the Function(s) it produces:
//
//	method/function     foo
//	getter / setter     get:foo / set:foo
//	field               foo, get:foo, set:foo, init:foo
//	constructor         new C. / new C.named   (factory and redirecting forms too)
//	operator            ==, [], unary-, ...
func (f *dartFile) chunkKeys(scope string, depth, start, end int) ([]string, bool) {
	var hdr []dartTok // member-depth tokens, annotations skipped
	stop := -1
	isConst := false
	sawOperator := false
	for _, j := range f.headerTokens(depth, start, end) {
		t := f.toks[j]
		if t.kind == dtIdent && t.text == "operator" {
			sawOperator = true
		}
		if t.kind == dtPunct && (t.text == "(" || (!sawOperator && (t.text == "=>" || t.text == "{" || t.text == "=" || t.text == ";" || t.text == ","))) {
			stop = j
			break
		}
		if t.kind == dtIdent && t.text == "const" {
			isConst = true
		}
		hdr = append(hdr, t)
	}
	if stop < 0 || len(hdr) == 0 {
		return nil, isConst
	}
	st := f.toks[stop]
	for i, t := range hdr {
		if t.kind == dtIdent && t.text == "operator" && st.text == "(" {
			var b strings.Builder
			for _, o := range hdr[i+1:] {
				b.WriteString(o.text)
			}
			op := b.String()
			if op == "-" {
				return []string{"-", "unary-"}, isConst
			}
			return []string{op}, isConst
		}
	}
	last := hdr[len(hdr)-1]
	if last.kind != dtIdent {
		return nil, isConst
	}
	prevIs := func(k int, text string) bool {
		return len(hdr)-1-k >= 0 && hdr[len(hdr)-1-k].text == text
	}
	switch st.text {
	case "(":
		if prevIs(1, "get") {
			return []string{"get:" + last.text}, isConst
		}
		if prevIs(1, "set") {
			return []string{"set:" + last.text}, isConst
		}
		if prevIs(1, ".") && len(hdr) >= 3 && hdr[len(hdr)-3].text == scope {
			return []string{"new " + scope + "." + last.text}, isConst
		}
		if last.text == scope {
			return []string{"new " + scope + "."}, isConst
		}
		return []string{last.text}, isConst
	case "=>", "{":
		if prevIs(1, "get") {
			return []string{"get:" + last.text}, isConst
		}
		return nil, isConst
	default: // "=", ";", ",": a field (or top-level variable) declaration, possibly several
		names := []string{}
		add := func(n string) {
			names = append(names, n, "get:"+n, "set:"+n, "init:"+n)
		}
		add(last.text)
		// further declarators: `, name (= expr)?` at member depth
		j := stop
		for j < end {
			t := f.toks[j]
			if t.depth == depth && t.kind == dtPunct && t.text == "=" {
				for j < end && !(f.toks[j].depth == depth && (f.toks[j].text == "," || f.toks[j].text == ";")) {
					j++
				}
				continue
			}
			if t.depth == depth && t.kind == dtPunct && t.text == "," && j+1 < end && f.toks[j+1].kind == dtIdent && f.toks[j+1].depth == depth {
				add(f.toks[j+1].text)
			}
			j++
		}
		return names, isConst
	}
}

// literals returns every literal fragment in the token range, plus the concatenation of each run of
// adjacent simple string literals (the compiler joins `'a' 'b'` into one constant).
func (f *dartFile) literals(start, end int) map[string]bool {
	out := map[string]bool{}
	var run strings.Builder
	inRun := false
	for j := start; j < end; j++ {
		t := f.toks[j]
		if t.kind != dtString {
			if inRun {
				out[run.String()] = true
				run.Reset()
				inRun = false
			}
			continue
		}
		for _, fr := range t.frags {
			out[fr] = true
		}
		if t.simple && len(t.frags) == 1 {
			run.WriteString(t.frags[0])
			inRun = true
		} else if inRun {
			out[run.String()] = true
			run.Reset()
			inRun = false
		}
	}
	if inRun {
		out[run.String()] = true
	}
	return out
}

func (f *dartFile) idents(start, end int) map[string]bool {
	out := map[string]bool{}
	for j := start; j < end; j++ {
		if f.toks[j].kind == dtIdent {
			out[f.toks[j].text] = true
		}
	}
	return out
}

// span is the byte range a declaration chunk covers.
func (f *dartFile) span(d dartDecl) (int, int) {
	return f.toks[d.start].pos, f.toks[d.end-1].end
}

// find returns the chunks in scope that declare key.
func (f *dartFile) find(scope, key string) []dartDecl {
	var out []dartDecl
	for _, d := range f.decls {
		if d.scope != scope {
			continue
		}
		for _, k := range d.keys {
			if k == key {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// constLiterals maps each const declaration's names to the literals of its initializer chunk.
func (f *dartFile) constLiterals() map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, d := range f.decls {
		if !d.isConst {
			continue
		}
		lits := f.literals(d.start, d.end)
		for _, k := range d.keys {
			if strings.Contains(k, ":") {
				continue
			}
			if out[k] == nil {
				out[k] = map[string]bool{}
			}
			for l := range lits {
				out[k][l] = true
			}
		}
	}
	return out
}

var vmPrivateSuffix = regexp.MustCompile(`@[0-9]+`)

// unmangleVMName removes the VM's library-private suffixes (`_Foo@1420201418` -> `_Foo`).
func unmangleVMName(s string) string { return vmPrivateSuffix.ReplaceAllString(s, "") }

// dartDeclTarget turns a gen_snapshot Function display name and its owner class into the (scope, key)
// the scanner indexes by. ok is false for names that do not correspond to a source declaration.
func dartDeclTarget(ownerClass, fnName string) (scope, key string, ok bool) {
	fn := unmangleVMName(fnName)
	scope = unmangleVMName(ownerClass)
	if scope == "::" {
		scope = ""
	}
	for strings.HasPrefix(fn, "dyn:") {
		fn = strings.TrimPrefix(fn, "dyn:")
	}
	if strings.HasPrefix(fn, "[") || strings.HasPrefix(fn, "<") {
		return "", "", false
	}
	// Extension members are top-level functions named `Ext|member` (tear-off helpers `Ext|get#m`).
	if bar := strings.IndexByte(fn, '|'); bar > 0 && scope == "" {
		scope = fn[:bar]
		fn = fn[bar+1:]
		fn = strings.TrimPrefix(fn, "get#")
		fn = strings.TrimPrefix(fn, "set#")
	}
	if strings.HasPrefix(fn, "new ") {
		// `new C.` / `new C.named`; the class part must be the owner
		rest := strings.TrimPrefix(fn, "new ")
		dot := strings.IndexByte(rest, '.')
		if dot < 0 {
			return "", "", false
		}
		return scope, "new " + scope + "." + rest[dot+1:], true
	}
	return scope, fn, true
}
