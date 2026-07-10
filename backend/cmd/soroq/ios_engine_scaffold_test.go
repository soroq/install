package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempIOSEngineProject lays out a minimal Flutter app (pubspec + soroq.yaml + lib source) with the
// given soroq.yaml patchable list and Dart source, and returns the project dir.
func writeTempIOSEngineProject(t *testing.T, patchableYAML, dartSrc string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pubspec.yaml"), []byte("name: demo_app\n\ndependencies:\n  flutter:\n    sdk: flutter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	soroq := "app_id: com.example.demo\nchannel: stable\nios_engine:\n  enabled: true\n  patchable:\n" + patchableYAML
	if err := os.WriteFile(filepath.Join(dir, "soroq.yaml"), []byte(soroq), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "ota_demo.dart"), []byte(dartSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const validDartSrc = `String demoLabel() => 'v1';

class AppInfo {
  static String channel() => 'stable';
  String instanceOnly() => 'no';
  static int get count => 3;
  static T identity<T>(T x) => x;
}

int topGetterOnly = 0;
`

func TestResolvePatchableEntry_Identities(t *testing.T) {
	dir := writeTempIOSEngineProject(t, "    - lib/ota_demo.dart#demoLabel\n", validDartSrc)

	top, err := resolvePatchableEntry(dir, "demo_app", "lib/ota_demo.dart#demoLabel")
	if err != nil {
		t.Fatalf("top-level: %v", err)
	}
	if top.Identity != "package:demo_app/ota_demo.dart::::demoLabel" {
		t.Fatalf("top-level identity = %q", top.Identity)
	}
	if top.TableRef != "demoLabel" || top.ImportPath != "ota_demo.dart" {
		t.Fatalf("top-level ref=%q import=%q", top.TableRef, top.ImportPath)
	}

	static, err := resolvePatchableEntry(dir, "demo_app", "lib/ota_demo.dart#AppInfo.channel")
	if err != nil {
		t.Fatalf("static: %v", err)
	}
	if static.Identity != "package:demo_app/ota_demo.dart::AppInfo::channel" {
		t.Fatalf("static identity = %q", static.Identity)
	}
	if static.TableRef != "AppInfo.channel" {
		t.Fatalf("static ref = %q", static.TableRef)
	}
}

func TestResolvePatchableEntry_StripsLibFromNestedPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pubspec.yaml"), []byte("name: demo_app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "lib", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "sub", "f.dart"), []byte("void go() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := resolvePatchableEntry(dir, "demo_app", "lib/sub/f.dart#go")
	if err != nil {
		t.Fatal(err)
	}
	if e.Identity != "package:demo_app/sub/f.dart::::go" {
		t.Fatalf("nested identity = %q", e.Identity)
	}
	if e.ImportPath != "sub/f.dart" {
		t.Fatalf("nested import = %q", e.ImportPath)
	}
}

func TestResolvePatchableEntry_Rejections(t *testing.T) {
	dir := writeTempIOSEngineProject(t, "    - lib/ota_demo.dart#demoLabel\n", validDartSrc)
	cases := []struct {
		name  string
		entry string
		want  string
	}{
		{"instance method", "lib/ota_demo.dart#AppInfo.instanceOnly", "instance method"},
		{"static getter", "lib/ota_demo.dart#AppInfo.count", "getter/setter"},
		{"generic method", "lib/ota_demo.dart#AppInfo.identity", "generic"},
		{"missing file", "lib/nope.dart#foo", "cannot read"},
		{"missing top-level symbol", "lib/ota_demo.dart#doesNotExist", "no top-level function"},
		{"missing class", "lib/ota_demo.dart#Nope.method", "no class"},
		{"malformed", "lib/ota_demo.dart", "must be of the form"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolvePatchableEntry(dir, "demo_app", tc.entry)
			if err == nil {
				t.Fatalf("expected error for %q", tc.entry)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestResolvePatchableEntry_TopLevelGetterAndClosureRejected(t *testing.T) {
	src := `int get demoLabel => 3;
final greet = (String n) => 'hi $n';
`
	dir := writeTempIOSEngineProject(t, "    - lib/ota_demo.dart#demoLabel\n", src)
	if _, err := resolvePatchableEntry(dir, "demo_app", "lib/ota_demo.dart#demoLabel"); err == nil || !strings.Contains(err.Error(), "getter/setter") {
		t.Fatalf("top-level getter should be rejected: %v", err)
	}
	if _, err := resolvePatchableEntry(dir, "demo_app", "lib/ota_demo.dart#greet"); err == nil || !strings.Contains(err.Error(), "no top-level function") {
		t.Fatalf("top-level closure var should be rejected: %v", err)
	}
}

func TestResolvePatchableEntries_DuplicateRejected(t *testing.T) {
	dir := writeTempIOSEngineProject(t, "", validDartSrc)
	_, err := resolvePatchableEntries(dir, "demo_app", []string{
		"lib/ota_demo.dart#demoLabel",
		"lib/ota_demo.dart#demoLabel",
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate should be rejected: %v", err)
	}
}

func TestGenerateIOSEngineScaffold_OrderAndContents(t *testing.T) {
	patchable := "    - lib/ota_demo.dart#demoLabel\n    - lib/ota_demo.dart#AppInfo.channel\n"
	dir := writeTempIOSEngineProject(t, patchable, validDartSrc)

	manifestPath, err := generateIOSEngineScaffold(dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	manifest, _ := os.ReadFile(manifestPath)
	wantManifest := "package:demo_app/ota_demo.dart::::demoLabel\npackage:demo_app/ota_demo.dart::AppInfo::channel\n"
	if string(manifest) != wantManifest {
		t.Fatalf("manifest =\n%q\nwant\n%q", string(manifest), wantManifest)
	}

	table, _ := os.ReadFile(filepath.Join(dir, "lib", "soroq_patch_table.g.dart"))
	ts := string(table)
	if !strings.Contains(ts, "GENERATED — do not edit") {
		t.Fatalf("table missing generated header")
	}
	if !strings.Contains(ts, "import 'ota_demo.dart';") {
		t.Fatalf("table missing direct import: %s", ts)
	}
	// Direct refs in soroq.yaml order.
	iDemo := strings.Index(ts, "\n  demoLabel,")
	iChannel := strings.Index(ts, "\n  AppInfo.channel,")
	if iDemo < 0 || iChannel < 0 || iDemo > iChannel {
		t.Fatalf("table refs missing or out of order (demo=%d channel=%d):\n%s", iDemo, iChannel, ts)
	}

	activator, _ := os.ReadFile(filepath.Join(dir, "lib", "soroq_activator.dart"))
	// Check the CODE only (strip comments + string bodies) so the descriptive header — which names the
	// policies the activator deliberately omits — cannot false-positive. The activator must carry ZERO
	// OTA policy: no network / verify / manifest / rollout / quarantine / hash / signature decisions.
	// (The rollback PRIMITIVE soroqRollbackPatch is mechanism, not policy, and is expected.)
	code := strings.ToLower(stripDartCommentsAndStrings(string(activator)))
	for _, forbidden := range []string{"http", "verify", "signature", "sha256", "rollout", "quarantine", "manifest", "fetch(", "hashcheck"} {
		if strings.Contains(code, forbidden) {
			t.Fatalf("activator CODE contains forbidden OTA-policy token %q", forbidden)
		}
	}
	if !strings.Contains(string(activator), "class EngineActivator implements SoroqEngineActivator") {
		t.Fatalf("activator missing EngineActivator class")
	}
}

func TestGenerateIOSEngineScaffold_RequiresEnabled(t *testing.T) {
	dir := writeTempIOSEngineProject(t, "    - lib/ota_demo.dart#demoLabel\n", validDartSrc)
	// Overwrite soroq.yaml with enabled:false.
	soroq := "app_id: com.example.demo\nchannel: stable\nios_engine:\n  enabled: false\n  patchable:\n    - lib/ota_demo.dart#demoLabel\n"
	if err := os.WriteFile(filepath.Join(dir, "soroq.yaml"), []byte(soroq), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := generateIOSEngineScaffold(dir); err == nil || !strings.Contains(err.Error(), "enabled") {
		t.Fatalf("expected enabled error: %v", err)
	}
}

func TestParseIOSEnginePatchable(t *testing.T) {
	yaml := `app_id: com.example.demo
channel: stable
ios_engine:
  enabled: true
  patchable:
    - lib/a.dart#foo
    - "lib/b.dart#Cls.bar"
other_key: value
`
	enabled, items, err := parseIOSEnginePatchable([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatalf("expected enabled")
	}
	want := []string{"lib/a.dart#foo", "lib/b.dart#Cls.bar"}
	if strings.Join(items, "|") != strings.Join(want, "|") {
		t.Fatalf("items = %v want %v", items, want)
	}
}
