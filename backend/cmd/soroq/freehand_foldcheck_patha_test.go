package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the Path A form of the value-propagation check (freehand_foldcheck_patha.go).
//
// Each scenario is a synthetic BASE: a gen_snapshot object graph with named edges, shaped like the real
// one (Function -owner_-> Class -library_/script_-> Library/Script, Function -code_-> Code -object_pool_->
// ObjectPool -> CanonicalString, closures through ClosureData.parent_function_), plus an app.dill with
// embedded source and type-flow annotations written by freehand_kernel_fixture_test.go. The two Campus
// refusals are reproduced from their measured shapes; the refusal shapes are the t002 routes.

const pathATestEngine = "soroq.ios_engine.6b182d2c_5a2a6a42.private_state.r6_obfuscation"

type namedProf struct {
	types   []string
	strings []string
	nodes   [][5]int
	edges   [][]struct{ to, name int }
	libs    map[string]int
	scripts map[string]int
	consts  map[string]int
}

func newNamedProf() *namedProf {
	return &namedProf{
		types: []string{"ArtificialRoot", "Class", "Code", "Function", "ObjectPool", "CanonicalString", "Array",
			"Library", "Script", "ClosureData", "PatchClass"},
		libs: map[string]int{}, scripts: map[string]int{}, consts: map[string]int{},
	}
}

func (b *namedProf) str(s string) int {
	for i, e := range b.strings {
		if e == s {
			return i
		}
	}
	b.strings = append(b.strings, s)
	return len(b.strings) - 1
}

func (b *namedProf) node(typ, name string) int {
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

func (b *namedProf) edge(from, to int, name string) {
	b.edges[from] = append(b.edges[from], struct{ to, name int }{to, b.str(name)})
	b.nodes[from][4] = len(b.edges[from])
}

// class makes (or reuses) the library and script for uri and returns a Class in it. The VM names a
// library's top level "::".
func (b *namedProf) class(uri, name string) int {
	lib, ok := b.libs[uri]
	if !ok {
		lib = b.node("Library", uri)
		b.libs[uri] = lib
		b.scripts[uri] = b.node("Script", uri)
	}
	c := b.node("Class", name)
	b.edge(c, lib, "library_")
	b.edge(c, b.scripts[uri], "script_")
	return c
}

// fn declares a function on cls and returns its Function, Code and ObjectPool.
func (b *namedProf) fn(cls int, name string) (fn, code, pool int) {
	fn = b.node("Function", name)
	code = b.node("Code", "[Optimized] "+name)
	pool = b.node("ObjectPool", "Unnamed [ObjectPool] (nil)")
	b.edge(fn, cls, "owner_")
	b.edge(fn, code, "code_")
	b.edge(code, pool, "object_pool_")
	b.edge(code, fn, "owner_")
	return
}

func (b *namedProf) closure(parent, cls int, name string) (fn, code, pool int) {
	fn, code, pool = b.fn(cls, name)
	data := b.node("ClosureData", "Unnamed [ClosureData] (nil)")
	b.edge(fn, data, "data_")
	b.edge(data, parent, "parent_function_")
	return
}

func (b *namedProf) holds(pool int, values ...string) {
	for _, v := range values {
		c, ok := b.consts[v]
		if !ok {
			c = b.node("CanonicalString", v)
			b.consts[v] = c
		}
		b.edge(pool, c, "")
	}
}

func (b *namedProf) write(t *testing.T, path string) {
	t.Helper()
	const NF = 5
	flatNodes := make([]int, 0, len(b.nodes)*NF)
	for _, n := range b.nodes {
		flatNodes = append(flatNodes, n[0], n[1], n[2], n[3], n[4])
	}
	flatEdges := []int{}
	for _, es := range b.edges {
		for _, e := range es {
			flatEdges = append(flatEdges, 2, e.name, e.to*NF) // 2 = "property"
		}
	}
	doc := map[string]any{
		"snapshot": map[string]any{"meta": map[string]any{
			"node_fields": []string{"type", "name", "id", "self_size", "edge_count"},
			"node_types":  []any{b.types, "string", "number", "number", "number"},
			"edge_fields": []string{"type", "name_or_index", "to_node"},
			"edge_types":  []any{[]string{"context", "element", "property", "internal"}, "string_or_number", "node"},
		}},
		"nodes":   flatNodes,
		"edges":   flatEdges,
		"strings": b.strings,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// pathABase writes a baseline directory holding the graph and the kernel.
func pathABase(t *testing.T, b *namedProf, k *kernelFixture, imports map[string][]string) string {
	t.Helper()
	dir := t.TempDir()
	b.write(t, filepath.Join(dir, freehandObjectGraphName))
	if k != nil {
		raw, err := os.ReadFile(k.write(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "app.dill"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if imports != nil {
		type lib struct {
			Library string   `json:"library"`
			Imports []string `json:"imports"`
		}
		doc := struct {
			LibraryGraph []lib `json:"libraryGraph"`
		}{}
		for l, im := range imports {
			doc.LibraryGraph = append(doc.LibraryGraph, lib{l, im})
		}
		raw, _ := json.Marshal(doc)
		if err := os.WriteFile(filepath.Join(dir, "symbol_graph.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func pathAMeta(rev string) *FreehandBaselineMeta { return &FreehandBaselineMeta{EngineRev: rev} }

func decl(kind, manifestLine string) []changedDecl {
	_, class, _, _ := splitIdentity(manifestLine)
	return []changedDecl{{manifestLine: manifestLine, keyKind: kind, keyClass: class}}
}

// proc builds a kfProc spanning snippet in src.
func proc(t *testing.T, name, uri, src, snippet string) kfProc {
	s0, s1 := span(t, src, snippet)
	return kfProc{name: name, uri: uri, start: s0, end: s1}
}

func cls(t *testing.T, name, uri, src, snippet string, fields []kfField, procs ...kfProc) kfClass {
	s0, s1 := span(t, src, snippet)
	return kfClass{name: name, uri: uri, start: s0, end: s1, fields: fields, procs: procs}
}

// ---------------------------------------------------------------------------------------------
// Campus refusal #2: UpdateDialog.build shares the literal "Update" with an unrelated widget method in
// another library, which writes its own "Update". build returns a Widget, never "Update".

const updateDialogURI = "package:campus_app/widgets/common/update_dialog.dart"
const updateDialogSrc = `class UpdateDialog extends StatelessWidget {
  final String latestVersion;
  const UpdateDialog({required this.latestVersion});

  @override
  Widget build(BuildContext context) {
    return Column(children: [
      Text('Version $latestVersion'),
      TextButton(onPressed: () => log('tap'), child: Text('Update')),
    ]);
  }
}

void log(String message) {}
`
const predictSheetURI = "package:campus_app/widgets/attendance_widgets/predict_attendance_sheet.dart"
const predictSheetSrc = `class _PredictAttendanceSheetState extends State<PredictAttendanceSheet> {
  @override
  Widget build(BuildContext context) => _buildActions(context, false);

  Widget _buildActions(BuildContext context, bool applied) {
    return Text('${applied ? 'Update' : 'Preview'} prediction');
  }
}
`
const textURI = "package:flutter/src/widgets/text.dart"
const textSrc = `class Text extends StatelessWidget {
  const Text(this.data, {super.key});
  final String? data;
}
`

// updateDialogBase builds the Campus shape. withSink controls where TFA recorded "Update": as a
// constructor parameter of Text (which build names, so the constant MAY have left build) or not at all.
func updateDialogBase(t *testing.T, textSink bool) string {
	b := newNamedProf()
	ud := b.class(updateDialogURI, "UpdateDialog")
	buildFn, _, buildPool := b.fn(ud, "build")
	b.holds(buildPool, "Update", "Version ")
	_, _, tapPool := b.closure(buildFn, ud, "<anonymous closure @412>")
	b.holds(tapPool, "tap")
	ps := b.class(predictSheetURI, "_PredictAttendanceSheetState@1420201418")
	_, _, actionsPool := b.fn(ps, "_buildActions@1420201418")
	b.holds(actionsPool, "Update", "Preview", " prediction")

	k := &kernelFixture{
		sources: map[string]string{updateDialogURI: updateDialogSrc, predictSheetURI: predictSheetSrc, textURI: textSrc},
		libs: []kfLib{
			{uri: updateDialogURI, classes: []kfClass{cls(t, "UpdateDialog", updateDialogURI, updateDialogSrc,
				"class UpdateDialog extends StatelessWidget {", nil,
				proc(t, "build", updateDialogURI, updateDialogSrc, "Widget build(BuildContext context) {"))},
				procs: []kfProc{proc(t, "log", updateDialogURI, updateDialogSrc, "void log(String message) {}")}},
			{uri: predictSheetURI, classes: []kfClass{cls(t, "_PredictAttendanceSheetState", predictSheetURI, predictSheetSrc,
				predictSheetSrc[:len(predictSheetSrc)-1], nil,
				proc(t, "_buildActions", predictSheetURI, predictSheetSrc, "Widget _buildActions(BuildContext context, bool applied) {"))}},
			{uri: textURI, classes: []kfClass{cls(t, "Text", textURI, textSrc, textSrc[:len(textSrc)-1], nil)}},
		},
		extra: []string{"Update", "Version ", "Preview", " prediction"},
		// "tap" really leaves build: it is the only value ever passed to log. Its only other holder is
		// build's own closure.
		meta: []kfMeta{{repo: "arg", target: "param:log", value: "tap"}},
	}
	if textSink {
		k.meta = append(k.meta,
			kfMeta{repo: "arg", target: "ctorparam:Text", value: "Update"},
			kfMeta{repo: "type", target: "expr:_buildActions", value: "Update"})
	}
	return pathABase(t, b, k, nil)
}

func TestPathACampusUpdateDialogSharedLiteralAccepted(t *testing.T) {
	for _, textSink := range []bool{false, true} {
		dir := updateDialogBase(t, textSink)
		d := decl("method", updateDialogURI+"::UpdateDialog::build")
		// CONTROL: the conservative rule refuses exactly this, as it did on the pilot.
		if err := assertFreehandNoFoldedValue(dir, d); err == nil || !strings.Contains(err.Error(), "_buildActions") {
			t.Fatalf("textSink=%v: the conservative rule no longer reproduces the Campus refusal: %v", textSink, err)
		}
		if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, d); err != nil {
			t.Fatalf("textSink=%v: a literal the other method writes itself was attributed to build: %v", textSink, err)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// Campus refusal #1: VersionCheckService.checkForUpdate shares "" (from `?? ''`), a URL constant,
// "update_url" and the VM-synthesized " in type cast" with unrelated code. "" really IS a type-flow
// constant in the app -- a payment SDK's never-set `_primaryFont = ""` is folded into its getter --
// but checkForUpdate feeds no sink that carries it.

const versionCheckURI = "package:campus_app/services/version_check_service.dart"
const versionCheckSrc = `class VersionCheckService {
  static const String _baseUrl = 'https://campusapi.fly.dev/api';

  static Future<VersionCheckResult> checkForUpdate() async {
    final response = await http.get(Uri.parse('$_baseUrl/version/check/ios'));
    final jsonBody = jsonDecode(response.body) as Map<String, dynamic>;
    final updateUrl = jsonBody['update_url'] as String? ?? '';
    return VersionCheckResult.fromJson({...jsonBody, 'update_url': updateUrl});
  }
}

class VersionCheckResult {
  final String updateUrl;
  VersionCheckResult({required this.updateUrl});
  factory VersionCheckResult.fromJson(Map<String, dynamic> json) =>
      VersionCheckResult(updateUrl: json['update_url'] as String);
}
`
const cfThemeURI = "package:flutter_cashfree_pg_sdk/api/cftheme/cftheme.dart"
const cfThemeSrc = `class CFThemeBuilder {
  String _primaryFont = "";
  String getPrimaryFont() {
    return _primaryFont;
  }
}
`
const timetableURI = "package:campus_app/notifiers/timetable_notifier.dart"
const timetableSrc = `class TimetableNotifier {
  Future<void> _fetchTimetable() async {
    await http.get(Uri.parse('$baseUrl/timetable'));
  }
}
`

func versionCheckBase(t *testing.T) string {
	b := newNamedProf()
	vc := b.class(versionCheckURI, "VersionCheckService")
	_, _, fPool := b.fn(vc, "checkForUpdate")
	b.holds(fPool, "https://campusapi.fly.dev/api", "/version/check/ios", "update_url", "", " in type cast")
	vr := b.class(versionCheckURI, "VersionCheckResult")
	_, _, fromJSON := b.fn(vr, "new VersionCheckResult.fromJson")
	b.holds(fromJSON, "update_url", " in type cast")
	cf := b.class(cfThemeURI, "CFThemeBuilder")
	_, _, fontPool := b.fn(cf, "getPrimaryFont")
	b.holds(fontPool, "")
	tt := b.class(timetableURI, "TimetableNotifier@1086034154")
	_, _, ttPool := b.fn(tt, "_fetchTimetable@1086034154")
	b.holds(ttPool, "https://campusapi.fly.dev/api", " in type cast")
	// A same-named method of an unrelated class: the conservative rule's owner walk could not tell it
	// apart; the Path A rule matches on library and class.
	lane := b.class("package:soroq_flutter/src/engine_lane_ota.dart", "SoroqEngineLaneController")
	_, _, lanePool := b.fn(lane, "checkForUpdate")
	b.holds(lanePool, "base", "")

	k := &kernelFixture{
		sources: map[string]string{versionCheckURI: versionCheckSrc, cfThemeURI: cfThemeSrc, timetableURI: timetableSrc},
		libs: []kfLib{
			{uri: versionCheckURI, classes: []kfClass{
				cls(t, "VersionCheckService", versionCheckURI, versionCheckSrc, "class VersionCheckService {", nil,
					proc(t, "checkForUpdate", versionCheckURI, versionCheckSrc, "static Future<VersionCheckResult> checkForUpdate() async {")),
			}},
			{uri: cfThemeURI, classes: []kfClass{cls(t, "CFThemeBuilder", cfThemeURI, cfThemeSrc, cfThemeSrc[:len(cfThemeSrc)-1],
				[]kfField{{name: "_primaryFont", uri: cfThemeURI}},
				proc(t, "getPrimaryFont", cfThemeURI, cfThemeSrc, "String getPrimaryFont() {"))}},
			{uri: timetableURI, classes: []kfClass{cls(t, "TimetableNotifier", timetableURI, timetableSrc, timetableSrc[:len(timetableSrc)-1], nil,
				proc(t, "_fetchTimetable", timetableURI, timetableSrc, "Future<void> _fetchTimetable() async {"))}},
		},
		meta: []kfMeta{
			// the SDK's never-set field, folded into its getter
			{repo: "type", target: "field:CFThemeBuilder._primaryFont", value: ""},
			{repo: "type", target: "expr:getPrimaryFont", value: ""},
			// the URL is a type-flow constant through an unrelated top-level const's reader
			{repo: "type", target: "expr:_fetchTimetable", value: "https://campusapi.fly.dev/api"},
		},
		extra: []string{"/version/check/ios", "update_url"},
	}
	return pathABase(t, b, k, nil)
}

func TestPathACampusVersionCheckAccepted(t *testing.T) {
	dir := versionCheckBase(t)
	d := decl("static-method", versionCheckURI+"::VersionCheckService::checkForUpdate")
	if err := assertFreehandNoFoldedValue(dir, d); err == nil {
		t.Fatal("CONTROL: the conservative rule no longer reproduces the Campus refusal")
	}
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, d); err != nil {
		t.Fatalf("checkForUpdate feeds no sink carrying any shared constant, yet was refused: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// REFUSALS: the routes Path A leaves open must still refuse.

// t002, the original: otaValue returns its literal, TFA records that as its return, and main folds it
// through the field it captured into.
func TestPathARefusesT002ReturnPropagation(t *testing.T) {
	const valuesURI = "package:app/values.dart"
	const valuesSrc = "String otaValue() => 'BASE-VALUE';\n"
	const mainURI = "package:app/main.dart"
	const mainSrc = "final String gTopLevelFinal = otaValue();\nvoid main() {\n  gTopLevelFinalCaptured = gTopLevelFinal;\n}\n"
	b := newNamedProf()
	_, _, fPool := b.fn(b.class(valuesURI, "::"), "otaValue")
	b.holds(fPool, "BASE-VALUE")
	_, _, mainPool := b.fn(b.class(mainURI, "::"), "main")
	b.holds(mainPool, "BASE-VALUE")
	k := &kernelFixture{
		sources: map[string]string{valuesURI: valuesSrc, mainURI: mainSrc},
		libs: []kfLib{
			{uri: valuesURI, procs: []kfProc{proc(t, "otaValue", valuesURI, valuesSrc, "String otaValue() => 'BASE-VALUE';")}},
			{uri: mainURI, procs: []kfProc{proc(t, "main", mainURI, mainSrc, "void main() {\n  gTopLevelFinalCaptured = gTopLevelFinal;\n}")}},
		},
		meta: []kfMeta{
			{repo: "return", target: "proc:otaValue", value: "BASE-VALUE"},
			{repo: "type", target: "expr:main", value: "BASE-VALUE"},
		},
	}
	dir := pathABase(t, b, k, nil)
	err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, decl("function", valuesURI+"::::otaValue"))
	if err == nil {
		t.Fatal("the t002 shape was accepted on a Path A base")
	}
	for _, want := range []string{"main", "BASE-VALUE", "does not contain", "silent no-op"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
}

// Field route: F stores a literal into a field TFA proves single-valued; another class's method reads
// the field and folds it.
const storeURI = "package:app/store.dart"
const storeSrc = `class LabelStore {
  String label = 'unset';
  void seed() {
    label = 'SEEDED';
  }
}

class Banner {
  String show(LabelStore s) => s.label;
}
`

func fieldRouteBase(t *testing.T, readerSrc string) (string, []changedDecl) {
	b := newNamedProf()
	ls := b.class(storeURI, "LabelStore")
	_, _, fPool := b.fn(ls, "seed")
	b.holds(fPool, "SEEDED")
	bn := b.class(storeURI, "Banner")
	_, _, gPool := b.fn(bn, "show")
	b.holds(gPool, "SEEDED")
	src := strings.Replace(storeSrc, "  String show(LabelStore s) => s.label;", readerSrc, 1)
	k := &kernelFixture{
		sources: map[string]string{storeURI: src},
		libs: []kfLib{{uri: storeURI, classes: []kfClass{
			cls(t, "LabelStore", storeURI, src, "class LabelStore {", []kfField{{name: "label", uri: storeURI}},
				proc(t, "seed", storeURI, src, "void seed() {")),
			cls(t, "Banner", storeURI, src, "class Banner {", nil, proc(t, "show", storeURI, src, strings.TrimSpace(readerSrc))),
		}}},
		meta: []kfMeta{
			{repo: "type", target: "field:LabelStore.label", value: "SEEDED"},
			{repo: "type", target: "expr:show", value: "SEEDED"},
		},
	}
	return pathABase(t, b, k, nil), decl("method", storeURI+"::LabelStore::seed")
}

func TestPathARefusesFieldRoute(t *testing.T) {
	dir, d := fieldRouteBase(t, "  String show(LabelStore s) => s.label;")
	err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, d)
	if err == nil || !strings.Contains(err.Error(), "Banner.show") {
		t.Fatalf("a field-propagated constant was accepted: %v", err)
	}
}

// RESIDUAL, pinned: the same field route, but the reader also writes the identical literal itself.
// The pool slot is shared, so the rule cannot tell the two uses apart and accepts. If this test ever
// starts failing, the rule has grown a capability and the header of freehand_foldcheck_patha.go is out
// of date. This is the shape a device proof of the residual should run.
func TestPathAResidualSharedLiteralIsAccepted(t *testing.T) {
	dir, d := fieldRouteBase(t, "  String show(LabelStore s) => s.label == 'SEEDED' ? 'SEEDED' : s.label;")
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, d); err != nil {
		t.Fatalf("the documented residual has closed -- update the header of freehand_foldcheck_patha.go: %v", err)
	}
}

// Parameter route: F passes a literal to a constructor whose every caller passes that value; the
// class's own method folds the parameter it stored.
func TestPathARefusesParameterRoute(t *testing.T) {
	const uri = "package:app/title.dart"
	const src = `class Title extends StatelessWidget {
  const Title(this.text);
  final String text;
  Widget build(BuildContext context) => Text(text);
}

class Home extends StatelessWidget {
  Widget build(BuildContext context) => Title('Hello');
}
`
	b := newNamedProf()
	home := b.class(uri, "Home")
	_, _, fPool := b.fn(home, "build")
	b.holds(fPool, "Hello")
	title := b.class(uri, "Title")
	_, _, gPool := b.fn(title, "build")
	b.holds(gPool, "Hello")
	k := &kernelFixture{
		sources: map[string]string{uri: src},
		libs: []kfLib{{uri: uri, classes: []kfClass{
			cls(t, "Title", uri, src, "class Title extends StatelessWidget {", nil,
				proc(t, "build", uri, src, "Widget build(BuildContext context) => Text(text);")),
			cls(t, "Home", uri, src, "class Home extends StatelessWidget {", nil,
				kfProc{name: "homeBuild", uri: uri}),
		}}},
		meta: []kfMeta{
			{repo: "arg", target: "ctorparam:Title", value: "Hello"},
			{repo: "type", target: "expr:build", value: "Hello"},
		},
	}
	// The Home.build procedure span must be real for its own annotations; place it.
	k.libs[0].classes[1].procs[0] = proc(t, "homeBuild", uri, src, "Widget build(BuildContext context) => Title('Hello');")
	dir := pathABase(t, b, k, nil)
	err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, decl("method", uri+"::Home::build"))
	if err == nil || !strings.Contains(err.Error(), "Title.build") {
		t.Fatalf("a parameter-propagated constant was accepted: %v", err)
	}
}

// A site whose declaration cannot be located is refused, and says why.
func TestPathARefusesUnlocatableSite(t *testing.T) {
	const valuesURI = "package:app/values.dart"
	const valuesSrc = "String otaValue() => 'BASE-VALUE';\n"
	b := newNamedProf()
	top := b.class(valuesURI, "::")
	_, _, fPool := b.fn(top, "otaValue")
	b.holds(fPool, "BASE-VALUE")
	_, _, gPool := b.fn(top, "<synthetic forwarder>")
	b.holds(gPool, "BASE-VALUE")
	k := &kernelFixture{
		sources: map[string]string{valuesURI: valuesSrc},
		libs:    []kfLib{{uri: valuesURI, procs: []kfProc{proc(t, "otaValue", valuesURI, valuesSrc, "String otaValue() => 'BASE-VALUE';")}}},
		meta:    []kfMeta{{repo: "return", target: "proc:otaValue", value: "BASE-VALUE"}},
	}
	dir := pathABase(t, b, k, nil)
	err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, decl("function", valuesURI+"::::otaValue"))
	if err == nil || !strings.Contains(err.Error(), "compiler-generated") {
		t.Fatalf("an unlocatable site was not refused with its reason: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// ACCEPTS that are only safe because of Path A.

// A closure created inside F is F's own code: "tap" provably leaves build (it is log's only
// argument), and its only other holder is build's closure, so there is nothing to refuse.
func TestPathAOwnClosureIsNotPropagation(t *testing.T) {
	dir := updateDialogBase(t, false)
	vp, err := analyzeValuePropagationPathA(filepath.Join(dir, freehandObjectGraphName), filepath.Join(dir, "app.dill"),
		filepath.Join(dir, "symbol_graph.json"), []PathAQuery{{PropagationQuery: PropagationQuery{Kind: "method", Class: "UpdateDialog", VMName: "build"}, LibURI: updateDialogURI}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(vp.Propagated) != 0 {
		t.Fatalf("own closure or explained literal counted as propagation: %+v", vp.Propagated)
	}
	// And the closure is really in the graph as another Code: move "tap" into an unrelated function
	// and it must be refused, which proves rule 1 -- not an empty graph -- is what accepted it.
	b := newNamedProf()
	ud := b.class(updateDialogURI, "UpdateDialog")
	_, _, buildPool := b.fn(ud, "build")
	b.holds(buildPool, "tap")
	_, _, otherPool := b.fn(b.class(predictSheetURI, "_PredictAttendanceSheetState@1420201418"), "_buildActions@1420201418")
	b.holds(otherPool, "tap")
	k := &kernelFixture{
		sources: map[string]string{updateDialogURI: updateDialogSrc, predictSheetURI: predictSheetSrc},
		libs: []kfLib{
			{uri: updateDialogURI, classes: []kfClass{cls(t, "UpdateDialog", updateDialogURI, updateDialogSrc, "class UpdateDialog extends StatelessWidget {", nil,
				proc(t, "build", updateDialogURI, updateDialogSrc, "Widget build(BuildContext context) {"))},
				procs: []kfProc{proc(t, "log", updateDialogURI, updateDialogSrc, "void log(String message) {}")}},
			{uri: predictSheetURI, classes: []kfClass{cls(t, "_PredictAttendanceSheetState", predictSheetURI, predictSheetSrc, "class _PredictAttendanceSheetState", nil,
				proc(t, "_buildActions", predictSheetURI, predictSheetSrc, "Widget _buildActions(BuildContext context, bool applied) {"))}},
		},
		meta: []kfMeta{{repo: "arg", target: "param:log", value: "tap"}, {repo: "type", target: "expr:_buildActions", value: "tap"}},
	}
	other := pathABase(t, b, k, nil)
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), other, decl("method", updateDialogURI+"::UpdateDialog::build")); err == nil {
		t.Fatal("CONTROL: the same constant in a foreign function was accepted")
	}
}

// A const the site names, declared in a library it imports, explains its copy.
func TestPathAConstInImportedLibraryExplains(t *testing.T) {
	const constsURI = "package:app/strings.dart"
	const constsSrc = "class AppStrings {\n  static const update = 'Update';\n}\n"
	const uri = "package:app/banner.dart"
	const src = "class UpdateBanner {\n  Widget build(BuildContext c) => Text(AppStrings.update);\n}\n"
	const fURI = "package:app/dialog.dart"
	const fSrc = "class Dialog {\n  Widget build(BuildContext c) => Text('Update');\n}\n"
	b := newNamedProf()
	_, _, fPool := b.fn(b.class(fURI, "Dialog"), "build")
	b.holds(fPool, "Update")
	_, _, gPool := b.fn(b.class(uri, "UpdateBanner"), "build")
	b.holds(gPool, "Update")
	k := &kernelFixture{
		sources: map[string]string{constsURI: constsSrc, uri: src, fURI: fSrc},
		libs: []kfLib{
			{uri: fURI, classes: []kfClass{cls(t, "Dialog", fURI, fSrc, fSrc[:len(fSrc)-1], nil,
				proc(t, "build", fURI, fSrc, "Widget build(BuildContext c) => Text('Update');"))}},
			{uri: uri, classes: []kfClass{cls(t, "UpdateBanner", uri, src, src[:len(src)-1], nil,
				proc(t, "bannerBuild", uri, src, "Widget build(BuildContext c) => Text(AppStrings.update);"))}},
			{uri: constsURI},
		},
		meta: []kfMeta{
			{repo: "arg", target: "ctorparam:Dialog", value: "Update"}, // stands in for Text's parameter, which build names
			{repo: "type", target: "expr:bannerBuild", value: "Update"},
		},
	}
	// Dialog.build names `Text`, not `Dialog`; make the sink one it names.
	k.libs[0].classes[0].name = "Text"
	k.meta[0].target = "ctorparam:Text"
	d := decl("method", fURI+"::Dialog::build")

	withoutImports := pathABase(t, b, k, nil)
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), withoutImports, d); err == nil {
		t.Fatal("CONTROL: without the import edge the const is invisible, so this must refuse")
	}
	withImports := pathABase(t, b, k, map[string][]string{uri: {constsURI}})
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), withImports, d); err != nil {
		t.Fatalf("a const the site names from an imported library did not explain its copy: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// GATING: nothing is relaxed unless the base is known to carry Path A and its kernel is readable.

func TestPathAOnlyForKnownEngines(t *testing.T) {
	dir := updateDialogBase(t, true)
	d := decl("method", updateDialogURI+"::UpdateDialog::build")
	for _, rev := range []string{"", "soroq.ios_engine.6b182d2c_5a2a6a42.private_state.r5", "soroq.ios_engine.future.rN"} {
		if err := assertFreehandNoFoldedValueForBase(pathAMeta(rev), dir, d); err == nil {
			t.Fatalf("engine %q is not known to carry Path A, yet the precise rule ran", rev)
		}
	}
	// A capability record naming a different engine than the baseline's own is not trusted either.
	m := pathAMeta(pathATestEngine)
	m.RedirectCapabilities = &FreehandRedirectCapabilities{EngineRevision: "soroq.ios_engine.other"}
	if err := assertFreehandNoFoldedValueForBase(m, dir, d); err == nil {
		t.Fatal("an inconsistent engine record enabled the precise rule")
	}
}

func TestPathAFallsBackWithoutKernel(t *testing.T) {
	dir := updateDialogBase(t, true)
	d := decl("method", updateDialogURI+"::UpdateDialog::build")
	if err := os.Remove(filepath.Join(dir, "app.dill")); err != nil {
		t.Fatal(err)
	}
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, d); err == nil {
		t.Fatal("no app.dill, yet the conservative rule was not applied")
	}
	if err := os.WriteFile(filepath.Join(dir, "app.dill"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, d); err == nil {
		t.Fatal("a corrupt app.dill was treated as evidence")
	}
	if err := os.Remove(filepath.Join(dir, freehandObjectGraphName)); err != nil {
		t.Fatal(err)
	}
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, d); err == nil ||
		!strings.Contains(err.Error(), "Cut a new base release") {
		t.Fatalf("a Path A base with no object graph must still fail closed: %v", err)
	}
}

// Every revision allowed the precise rule must have been built from a patch set that carries all four
// Path A compiler hunks. Skips in the public export, which carries no tooling/.
func TestPathAEngineRevisionsCarryPathA(t *testing.T) {
	root := filepath.Join("..", "..", "..", "tooling", "flutter_matrix", "patches")
	if _, err := os.Stat(root); err != nil {
		t.Skip("tooling/flutter_matrix/patches is not in this tree")
	}
	hunks := []string{
		`return InliningDecision::No("soroq-patchable");`,
		`if (dart_function.SoroqIsPatchable()) {` + "\n+    return UncheckedEntryPointStyle::kNone;",
		"!interface_target.IsNull() && interface_target.SoroqIsPatchable();",
		"!soroq_patchable_callee) {",
		"const bool soroq_patchable_target = target.SoroqIsPatchable();",
		"!soroq_patchable_target) {",
	}
	for rev, label := range freehandPathAEngineRevisions {
		raw, err := os.ReadFile(filepath.Join(root, label, "dart.patch"))
		if err != nil {
			t.Fatalf("%s -> %s: %v", rev, label, err)
		}
		for _, h := range hunks {
			if !strings.Contains(string(raw), h) {
				t.Errorf("%s (%s) is allowed the precise rule but its dart.patch lacks %q", rev, label, h)
			}
		}
	}
}

// Rule 1 decisive: the value reaches build's closure through a type-flow fold INSIDE build (no
// literal anywhere in build's source), so only "the closure is build's own code" accepts it.
func TestPathAOwnClosureWithoutLiteralIsNotPropagation(t *testing.T) {
	const uri = "package:app/tap.dart"
	const src = `class TapRow extends StatelessWidget {
  Widget build(BuildContext context) {
    final label = tapLabel();
    return Button(onPressed: () => log(label));
  }
}

void log(String message) {}
`
	b := newNamedProf()
	row := b.class(uri, "TapRow")
	buildFn, _, buildPool := b.fn(row, "build")
	b.holds(buildPool, "tap")
	_, _, closurePool := b.closure(buildFn, row, "<anonymous closure @120>")
	b.holds(closurePool, "tap")
	k := &kernelFixture{
		sources: map[string]string{uri: src},
		libs: []kfLib{{uri: uri,
			classes: []kfClass{cls(t, "TapRow", uri, src, "class TapRow extends StatelessWidget {", nil,
				proc(t, "build", uri, src, "Widget build(BuildContext context) {\n    final label = tapLabel();\n    return Button(onPressed: () => log(label));\n  }"))},
			procs: []kfProc{proc(t, "log", uri, src, "void log(String message) {}")}}},
		meta: []kfMeta{
			{repo: "arg", target: "param:log", value: "tap"},   // leaves build: log's only argument
			{repo: "type", target: "expr:build", value: "tap"}, // the captured `label`, folded
		},
	}
	dir := pathABase(t, b, k, nil)
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, decl("method", uri+"::TapRow::build")); err != nil {
		t.Fatalf("build's own closure was treated as a propagated site: %v", err)
	}
}

// Rule 4 decisive: F's constant really can leave F (it is a constructor argument F passes), but the
// other holder carries no type-flow annotation for it at all -- a runtime-library function that uses
// the same string for its own reasons. Its copy is not a fold, so it is not F's.
func TestPathASiteWithoutAnnotationIsNotAFold(t *testing.T) {
	const fURI = "package:app/dialog.dart"
	const fSrc = "class Dialog {\n  Widget build(BuildContext c) => Text('Update');\n}\n"
	const gURI = "dart:core-patch/errors_patch.dart"
	const gSrc = "class ArgumentError {\n  String toString() => message;\n}\n"
	b := newNamedProf()
	_, _, fPool := b.fn(b.class(fURI, "Dialog"), "build")
	b.holds(fPool, "Update")
	_, _, gPool := b.fn(b.class(gURI, "ArgumentError"), "toString")
	b.holds(gPool, "Update")
	k := &kernelFixture{
		sources: map[string]string{fURI: fSrc, gURI: gSrc},
		libs: []kfLib{
			{uri: fURI, classes: []kfClass{cls(t, "Text", fURI, fSrc, "class Dialog {", nil,
				proc(t, "build", fURI, fSrc, "Widget build(BuildContext c) => Text('Update');"))}},
			{uri: gURI, classes: []kfClass{cls(t, "ArgumentError", gURI, gSrc, "class ArgumentError {", nil,
				proc(t, "toString", gURI, gSrc, "String toString() => message;"))}},
		},
		meta: []kfMeta{{repo: "arg", target: "ctorparam:Text", value: "Update"}},
	}
	dir := pathABase(t, b, k, nil)
	d := decl("method", fURI+"::Dialog::build")
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), dir, d); err != nil {
		t.Fatalf("a holder with no annotation for the constant was attributed to F: %v", err)
	}
	// CONTROL: annotate the same holder and it must refuse, so the absence is what accepted it.
	k.meta = append(k.meta, kfMeta{repo: "type", target: "expr:toString", value: "Update"})
	if err := assertFreehandNoFoldedValueForBase(pathAMeta(pathATestEngine), pathABase(t, b, k, nil), d); err == nil {
		t.Fatal("CONTROL: an annotated holder without the literal was accepted")
	}
}

// R8 is allowed the precise rule only because it records what it inlines/propagates; pin that.
func TestR8EngineRevisionsRecord(t *testing.T) {
	root := filepath.Join("..", "..", "..", "tooling", "flutter_matrix", "patches")
	if _, err := os.Stat(root); err != nil {
		t.Skip("tooling/flutter_matrix/patches is not in this tree")
	}
	hunks := []string{
		`caller_graph_->function().SoroqRecordDependency(function, "inline");`,
		`parsed_function_->function().SoroqRecordDependency(target, "result");`,
		"!interface_target.IsNull() && interface_target.SoroqIsPatchable();",
		"!soroq_patchable_callee) {",
		`if (dart_function.SoroqIsPatchable()) {` + "\n+    return UncheckedEntryPointStyle::kNone;",
	}
	for rev, label := range freehandDependencyMapEngineRevisions {
		raw, err := os.ReadFile(filepath.Join(root, label, "dart.patch"))
		if err != nil {
			t.Fatalf("%s -> %s: %v", rev, label, err)
		}
		for _, h := range hunks {
			if !strings.Contains(string(raw), h) {
				t.Errorf("%s (%s) lacks %q", rev, label, h)
			}
		}
	}
}

func TestPathAReplacedKeyNormalizes(t *testing.T) {
	if a, b := pathAReplacedKey("package:a/a.dart", "::", "top"), pathAReplacedKey("package:a/a.dart", "", "top"); a != b {
		t.Fatalf("top-level forms differ: %q vs %q", a, b)
	}
	if a, b := pathAReplacedKey("package:a/a.dart", "_Home@123", "build"), pathAReplacedKey("package:a/a.dart", "_Home", "build"); a != b {
		t.Fatalf("private class forms differ: %q vs %q", a, b)
	}
	if a, b := pathAReplacedKey("package:a/a.dart", "C", "new C."), pathAReplacedKey("package:a/a.dart", "C", "C."); a != b {
		t.Fatalf("constructor forms differ: %q vs %q", a, b)
	}
}
