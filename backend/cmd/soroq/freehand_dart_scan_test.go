package main

import (
	"sort"
	"strings"
	"testing"
)

const scanFixture = `// a comment with 'quotes' and { braces
/* block /* nested */ comment } */
@pragma('vm:entry-point')
class Widgets extends Base with Mixin implements Iface {
  static const String kTitle = 'Title';
  final String a = 'A', b = "B";
  @override
  @Deprecated('use other')
  Widget build(BuildContext context) {
    return Text('Hello ${name ?? 'anon'} and $other', key: Key(r'raw\n'));
  }
  String get label => 'L' 'abel';
  set label(String v) { _x = v; }
  Widgets.named(this.a) : super('named-super');
  Widgets();
  factory Widgets.make() => Widgets.named('made');
  bool operator ==(Object other) => other is Widgets && 'eq' == 'eq';
  void operator []=(int i, String v) { _m['k'] = v; }
  String multi() => '''
first line\t'tab'
''';
  String esc() => 'it\'s \u{1F600} \x41';
}

enum Kind { one, two; String describe() => 'kind'; }

extension ResponsiveExtensions on BuildContext {
  double responsiveSize(double v) => v * 1.0;
}

String topLevel() => "top";
final String topField = 'tf';
`

func declKeys(f *dartFile, scope string) []string {
	var out []string
	for _, d := range f.decls {
		if d.scope == scope {
			out = append(out, strings.Join(d.keys, "|"))
		}
	}
	return out
}

func litsOf(t *testing.T, f *dartFile, scope, key string) []string {
	t.Helper()
	ds := f.find(scope, key)
	if len(ds) == 0 {
		t.Fatalf("%s::%s not found; scope has %v", scope, key, declKeys(f, scope))
	}
	set := map[string]bool{}
	for _, d := range ds {
		for l := range f.literals(d.start, d.end) {
			set[l] = true
		}
	}
	var out []string
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func TestDartScanFindsDeclarationsAndLiterals(t *testing.T) {
	f := indexDartFile(scanFixture)
	cases := []struct {
		scope, key string
		want       []string
	}{
		{"Widgets", "build", []string{"Hello ", " and ", "", "anon", "raw\\n", "use other"}},
		{"Widgets", "get:label", []string{"L", "abel", "Label"}},
		{"Widgets", "set:label", nil},
		{"Widgets", "new Widgets.named", []string{"named-super"}},
		{"Widgets", "new Widgets.", nil},
		{"Widgets", "new Widgets.make", []string{"made"}},
		{"Widgets", "==", []string{"eq"}},
		{"Widgets", "[]=", []string{"k"}},
		{"Widgets", "multi", []string{"first line\t'tab'\n"}},
		{"Widgets", "esc", []string{"it's \U0001F600 A"}},
		{"Widgets", "init:b", []string{"A", "B"}},
		{"Widgets", "get:kTitle", []string{"Title"}},
		{"Kind", "describe", []string{"kind"}},
		{"ResponsiveExtensions", "responsiveSize", nil},
		{"", "topLevel", []string{"top"}},
		{"", "init:topField", []string{"tf"}},
	}
	for _, c := range cases {
		got := litsOf(t, f, c.scope, c.key)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		for _, w := range want {
			found := false
			for _, g := range got {
				if g == w {
					found = true
				}
			}
			if !found {
				t.Errorf("%s::%s: literal %q missing from %q", c.scope, c.key, w, got)
			}
		}
	}
	// An interpolated identifier is never read as a declaration.
	if len(f.find("Widgets", "other")) != 0 || len(f.find("Widgets", "name")) != 0 {
		t.Fatal("identifiers inside a string interpolation were indexed as declarations")
	}
	// Annotations before a class do not hide it.
	if len(declKeys(f, "Widgets")) == 0 {
		t.Fatal("an annotated class was not recognised")
	}
	consts := f.constLiterals()
	if !consts["kTitle"]["Title"] {
		t.Fatalf("const initializer not indexed: %v", consts)
	}
}

func TestDartDeclTargetNames(t *testing.T) {
	cases := []struct{ owner, fn, scope, key string }{
		{"_PredictAttendanceSheetState@1420201418", "_buildActions@1420201418", "_PredictAttendanceSheetState", "_buildActions"},
		{"::", "otaValue", "", "otaValue"},
		{"::", "ResponsiveExtensions|responsiveSize", "ResponsiveExtensions", "responsiveSize"},
		{"::", "ResponsiveExtensions|get#responsiveSize", "ResponsiveExtensions", "responsiveSize"},
		{"EagerProviderProbe", "new EagerProviderProbe.", "EagerProviderProbe", "new EagerProviderProbe."},
		{"_Foo@12", "new _Foo@12.named", "_Foo", "new _Foo.named"},
		{"Widgets", "dyn:get:label", "Widgets", "get:label"},
	}
	for _, c := range cases {
		scope, key, ok := dartDeclTarget(c.owner, c.fn)
		if !ok || scope != c.scope || key != c.key {
			t.Errorf("%s / %s -> (%q, %q, %v), want (%q, %q)", c.owner, c.fn, scope, key, ok, c.scope, c.key)
		}
	}
	for _, generated := range []string{"<anonymous closure>", "[tear-off] build"} {
		if _, _, ok := dartDeclTarget("C", generated); ok {
			t.Errorf("%q has no source declaration but was mapped to one", generated)
		}
	}
}

func TestUTF16Offset(t *testing.T) {
	src := []byte("aé\U0001F600b")
	if got := utf16Offset(src, len(src)); got != 5 { // a, é, surrogate pair, b
		t.Fatalf("utf16 length %d, want 5", got)
	}
	if got := utf16Offset(src, 1); got != 1 {
		t.Fatalf("utf16 offset %d, want 1", got)
	}
}
