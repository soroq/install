package depgraph

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- Capability classification (declaration signals) ----

func TestClassify_DartOnlyPackage_Eligible(t *testing.T) {
	cap := ClassifyPubspecForTest([]byte(`
name: riverpod
dependencies:
  collection: ^1.0.0
  meta: ^1.0.0
`))
	if !cap.Eligible || cap.HasNativePlugin || cap.HasAssets {
		t.Fatalf("riverpod must be Dart-only eligible, got %+v", cap)
	}
}

func TestClassify_FlutterDependentNoNative_Eligible(t *testing.T) {
	// flutter_riverpod imports Flutter but ships NO native code and NO assets ⇒ still eligible.
	cap := ClassifyPubspecForTest([]byte(`
name: flutter_riverpod
dependencies:
  flutter:
    sdk: flutter
  riverpod: ^2.0.0
`))
	if !cap.Eligible {
		t.Fatalf("flutter_riverpod imports Flutter but is Dart-only — must stay eligible, got %+v", cap)
	}
	if !cap.ImportsFlutter {
		t.Fatal("expected ImportsFlutter recorded")
	}
}

func TestClassify_NativePlugin_Ineligible(t *testing.T) {
	cap := ClassifyPubspecForTest([]byte(`
name: camera
flutter:
  plugin:
    platforms:
      android:
        package: com.example.camera
        pluginClass: CameraPlugin
      ios:
        pluginClass: CameraPlugin
`))
	if cap.Eligible || !cap.HasNativePlugin {
		t.Fatalf("camera declares native pluginClass — must be ineligible, got %+v", cap)
	}
	if !strings.Contains(cap.NativeDetail, "pluginClass") {
		t.Fatalf("native detail should name pluginClass, got %q", cap.NativeDetail)
	}
}

func TestClassify_FfiPlugin_Ineligible(t *testing.T) {
	cap := ClassifyPubspecForTest([]byte(`
name: some_ffi
flutter:
  plugin:
    platforms:
      android: {ffiPlugin: true}
      ios: {ffiPlugin: true}
`))
	if cap.Eligible || !cap.HasNativePlugin {
		t.Fatalf("ffiPlugin must be ineligible, got %+v", cap)
	}
}

func TestClassify_DartPluginClassOnly_Eligible(t *testing.T) {
	// A federated Dart-only implementation (real shape: path_provider_android 2.3.1).
	cap := ClassifyPubspecForTest([]byte(`
name: path_provider_android
flutter:
  plugin:
    implements: path_provider
    platforms:
      android:
        dartPluginClass: PathProviderAndroid
`))
	if !cap.Eligible || cap.HasNativePlugin {
		t.Fatalf("dartPluginClass-only must stay eligible, got %+v", cap)
	}
}

func TestClassify_LegacyPluginClass_Ineligible(t *testing.T) {
	cap := ClassifyPubspecForTest([]byte(`
name: old_plugin
flutter:
  plugin:
    androidPackage: com.example.old
    pluginClass: OldPlugin
`))
	if cap.Eligible || !cap.HasNativePlugin {
		t.Fatalf("legacy pluginClass must be ineligible, got %+v", cap)
	}
}

func TestClassify_PackageAssetsFontsShaders_Ineligible(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"assets", "flutter:\n  assets:\n    - assets/icon.png\n", "flutter.assets"},
		{"fonts", "flutter:\n  fonts:\n    - family: Custom\n      fonts:\n        - asset: fonts/C.ttf\n", "flutter.fonts"},
		{"shaders", "flutter:\n  shaders:\n    - shaders/ripple.frag\n", "flutter.shaders"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := ClassifyPubspecForTest([]byte("name: pack\n" + tc.body))
			if cap.Eligible || !cap.HasAssets {
				t.Fatalf("%s must be ineligible, got %+v", tc.name, cap)
			}
			if !strings.Contains(strings.Join(cap.AssetDetail, " "), tc.want) {
				t.Fatalf("asset detail should name %s, got %v", tc.want, cap.AssetDetail)
			}
		})
	}
}

// ---- Capability classification (on-disk content signals) ----

func writePkg(t *testing.T, dir, pubspec string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pubspec.yaml"), []byte(pubspec), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestClassify_NativeBuildHook_Ineligible(t *testing.T) {
	ps := "name: ffi_pkg\n"
	dir := writePkg(t, filepath.Join(t.TempDir(), "ffi_pkg"), ps, map[string]string{
		"hook/build.dart":  "void main() {}",
		"lib/ffi_pkg.dart": "int f() => 1;",
	})
	cap := classifyPackage([]byte(ps), dir, SourceHosted)
	if cap.Eligible || !cap.HasBuildHook {
		t.Fatalf("a package with hook/build.dart must be ineligible, got %+v", cap)
	}
	if !strings.Contains(cap.BuildHookDetail, "hook/build.dart") {
		t.Fatalf("build hook detail should name the hook, got %q", cap.BuildHookDetail)
	}
}

func TestClassify_UndeclaredNativeContent_IneligibleAndInconsistent(t *testing.T) {
	// Pure-looking pubspec, but the package actually ships a prebuilt .dylib and an ios/ dir.
	ps := "name: sneaky\n"
	dir := writePkg(t, filepath.Join(t.TempDir(), "sneaky"), ps, map[string]string{
		"lib/sneaky.dart":      "int f() => 1;",
		"lib/blobs/libx.dylib": "\x00binary",
		"ios/sneaky.podspec":   "Pod::Spec.new",
	})
	cap := classifyPackage([]byte(ps), dir, SourceHosted)
	if cap.Eligible {
		t.Fatalf("undeclared native content must be ineligible, got %+v", cap)
	}
	if !cap.MetadataInconsistent {
		t.Fatalf("undeclared native content must be flagged as metadata-inconsistent, got %+v", cap)
	}
	joined := strings.Join(cap.NativeArtifacts, " ")
	if !strings.Contains(joined, "libx.dylib") || !strings.Contains(joined, "ios/") {
		t.Fatalf("expected the .dylib and ios/ recorded as native artifacts, got %v", cap.NativeArtifacts)
	}
}

func TestClassify_DeclaredPluginWithoutNativeContent_Inconsistent(t *testing.T) {
	ps := "name: liar\nflutter:\n  plugin:\n    platforms:\n      ios:\n        pluginClass: LiarPlugin\n"
	dir := writePkg(t, filepath.Join(t.TempDir(), "liar"), ps, map[string]string{"lib/liar.dart": "int f() => 1;"})
	cap := classifyPackage([]byte(ps), dir, SourceHosted)
	if cap.Eligible || !cap.MetadataInconsistent {
		t.Fatalf("a declared native plugin shipping no native content must be flagged inconsistent, got %+v", cap)
	}
}

func TestClassify_ExampleAndTestDirsAreNotNativeEvidence(t *testing.T) {
	// Real pure-Dart packages routinely ship example/ios + test/. These must NOT make them native.
	ps := "name: pure\n"
	dir := writePkg(t, filepath.Join(t.TempDir(), "pure"), ps, map[string]string{
		"lib/pure.dart":                        "int f() => 1;",
		"example/ios/Runner/AppDelegate.swift": "import UIKit",
		"example/android/build.gradle":         "apply plugin: 'com.android.application'",
		"test/pure_test.dart":                  "void main() {}",
		"tool/gen.dart":                        "void main() {}",
	})
	cap := classifyPackage([]byte(ps), dir, SourceHosted)
	if !cap.Eligible {
		t.Fatalf("example/, test/ and tool/ content must not make a pure-Dart package native: %+v", cap)
	}
	if len(cap.NativeArtifacts) != 0 {
		t.Fatalf("no native artifacts expected, got %v", cap.NativeArtifacts)
	}
}

func TestClassify_SDKPackageNotContentScanned(t *testing.T) {
	// The flutter SDK package contains native engine sources; it is pinned by the toolchain recipe and
	// must not be content-scanned into a refusal.
	ps := "name: flutter\n"
	dir := writePkg(t, filepath.Join(t.TempDir(), "flutter"), ps, map[string]string{
		"lib/src/engine.cc": "int main(){}",
	})
	cap := classifyPackage([]byte(ps), dir, SourceSDK)
	if !cap.Eligible {
		t.Fatalf("sdk packages must stay eligible, got %+v", cap)
	}
}

// ---- Runtime graph resolution ----

// pkgSpec describes one package in a synthetic on-disk project.
type pkgSpec struct {
	deps    []string // runtime dependencies
	devDeps []string // dev_dependencies (must be excluded from the runtime graph)
	extra   string   // extra pubspec yaml (e.g. a flutter: section)
	files   map[string]string
	version string
	source  SourceType
	// omitFromLock / omitFromConfig simulate an unresolved runtime edge.
	omitFromLock   bool
	omitFromConfig bool
}

// project builds an on-disk project: a root pubspec plus packages, wired through pubspec.lock and
// .dart_tool/package_config.json exactly like `flutter pub get` does.
func project(t *testing.T, rootDeps []string, rootDevDeps []string, pkgs map[string]pkgSpec) string {
	t.Helper()
	dir := t.TempDir()
	root := "name: app\ndependencies:\n"
	for _, d := range rootDeps {
		root += "  " + d + ": any\n"
	}
	if len(rootDevDeps) > 0 {
		root += "dev_dependencies:\n"
		for _, d := range rootDevDeps {
			root += "  " + d + ": any\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "pubspec.yaml"), []byte(root), 0o644); err != nil {
		t.Fatal(err)
	}

	lock := "packages:\n"
	var cfgPkgs []map[string]string
	for _, name := range sortedKeys(pkgs) {
		spec := pkgs[name]
		ver := spec.version
		if ver == "" {
			ver = "1.0.0"
		}
		src := spec.source
		if src == "" {
			src = SourceHosted
		}
		body := "name: " + name + "\n"
		if len(spec.deps) > 0 {
			body += "dependencies:\n"
			for _, d := range spec.deps {
				body += "  " + d + ": any\n"
			}
		}
		if len(spec.devDeps) > 0 {
			body += "dev_dependencies:\n"
			for _, d := range spec.devDeps {
				body += "  " + d + ": any\n"
			}
		}
		body += spec.extra
		writePkg(t, filepath.Join(dir, "pkgs", name), body, spec.files)

		if !spec.omitFromLock {
			lock += "  " + name + ":\n    dependency: transitive\n    source: " + string(src) + "\n    version: \"" + ver + "\"\n"
			switch src {
			case SourceHosted:
				lock += "    description:\n      name: " + name + "\n      url: \"https://pub.dev\"\n      sha256: \"" + strings.Repeat("a", 64) + "\"\n"
			case SourcePath:
				lock += "    description:\n      path: \"pkgs/" + name + "\"\n      relative: true\n"
			case SourceGit:
				lock += "    description:\n      url: \"https://example.com/" + name + ".git\"\n      resolved-ref: \"" + strings.Repeat("b", 40) + "\"\n"
			case SourceSDK:
				lock += "    description: flutter\n"
			}
		}
		if !spec.omitFromConfig {
			cfgPkgs = append(cfgPkgs, map[string]string{"name": name, "rootUri": "../pkgs/" + name, "packageUri": "lib/"})
		}
	}
	cfgPkgs = append(cfgPkgs, map[string]string{"name": "app", "rootUri": "../", "packageUri": "lib/"})

	if err := os.WriteFile(filepath.Join(dir, "pubspec.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".dart_tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgBytes, _ := json.MarshalIndent(map[string]any{"configVersion": 2, "packages": cfgPkgs}, "", " ")
	if err := os.WriteFile(filepath.Join(dir, ".dart_tool", "package_config.json"), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolve_ExcludesDevDependencies(t *testing.T) {
	// build_runner is a dev dependency of the app AND `test` of a runtime package; neither may enter.
	dir := project(t, []string{"riverpod"}, []string{"build_runner"}, map[string]pkgSpec{
		"riverpod":     {deps: []string{"meta"}, devDeps: []string{"test"}},
		"meta":         {},
		"build_runner": {},
		"test":         {},
	})
	g, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Packages["build_runner"]; ok {
		t.Fatal("build_runner is a dev dependency and must NOT be a runtime package")
	}
	if _, ok := g.Packages["test"]; ok {
		t.Fatal("a package's dev_dependencies must NOT be walked into the runtime graph")
	}
	if _, ok := g.Packages["meta"]; !ok {
		t.Fatal("transitive runtime dependency meta must be present")
	}
	if len(g.Packages) != 2 {
		t.Fatalf("expected exactly {riverpod, meta}, got %v", g.PackageNames())
	}
}

func TestResolve_DevOnlyAssetPackageDoesNotRefuse(t *testing.T) {
	// A dev-only package declaring assets must not produce a spurious refusal: it never ships.
	dir := project(t, []string{"pure"}, []string{"golden_toolkit"}, map[string]pkgSpec{
		"pure":           {},
		"golden_toolkit": {extra: "flutter:\n  assets:\n    - assets/golden.png\n"},
	})
	g, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Packages["golden_toolkit"]; ok {
		t.Fatal("a dev-only asset-bearing package must not appear in the runtime graph")
	}
}

func TestResolve_RecursiveTransitiveClosure(t *testing.T) {
	dir := project(t, []string{"a"}, nil, map[string]pkgSpec{
		"a": {deps: []string{"b"}},
		"b": {deps: []string{"c"}},
		"c": {deps: []string{"d"}},
		"d": {},
	})
	g, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c", "d"} {
		if _, ok := g.Packages[n]; !ok {
			t.Fatalf("transitive package %q missing from the runtime closure %v", n, g.PackageNames())
		}
	}
	if got := g.Packages["b"].Dependencies; len(got) != 1 || got[0] != "c" {
		t.Fatalf("canonical edges for b should be [c], got %v", got)
	}
}

func TestResolve_CycleTerminates(t *testing.T) {
	dir := project(t, []string{"a"}, nil, map[string]pkgSpec{
		"a": {deps: []string{"b"}},
		"b": {deps: []string{"c"}},
		"c": {deps: []string{"a", "b"}}, // cycle back to both
	})
	g, err := Resolve(dir)
	if err != nil {
		t.Fatalf("a dependency cycle must be handled safely, got %v", err)
	}
	if len(g.Packages) != 3 {
		t.Fatalf("expected 3 packages in the cyclic graph, got %v", g.PackageNames())
	}
}

func TestResolve_UnresolvedRuntimeEdge_FailsClosed(t *testing.T) {
	t.Run("missing from lock", func(t *testing.T) {
		dir := project(t, []string{"a"}, nil, map[string]pkgSpec{
			"a":     {deps: []string{"ghost"}},
			"ghost": {omitFromLock: true},
		})
		_, err := Resolve(dir)
		if err == nil || !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "pubspec.lock") {
			t.Fatalf("an unresolved runtime edge must fail closed naming the package, got %v", err)
		}
		if !strings.Contains(err.Error(), `required by "a"`) {
			t.Fatalf("error should name the requester, got %v", err)
		}
	})
	t.Run("missing from package_config", func(t *testing.T) {
		dir := project(t, []string{"a"}, nil, map[string]pkgSpec{
			"a":     {deps: []string{"ghost"}},
			"ghost": {omitFromConfig: true},
		})
		_, err := Resolve(dir)
		if err == nil || !strings.Contains(err.Error(), "package_config.json") {
			t.Fatalf("a package absent from package_config must fail closed, got %v", err)
		}
	})
}

func TestResolve_NoDeveloperLocalPathsSerialized(t *testing.T) {
	dir := project(t, []string{"local"}, nil, map[string]pkgSpec{
		"local": {source: SourcePath, files: map[string]string{"lib/local.dart": "int f() => 1;"}},
	})
	g, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g.Packages["local"].TreeHash == "" {
		t.Fatal("a path package must be pinned by a deterministic source-tree hash")
	}
	raw, err := g.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{dir, "/Users/", "/home/"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("serialized graph leaks a developer-local absolute path %q", bad)
		}
	}
}

func TestResolve_TreeHashDetectsPathPackageEdit(t *testing.T) {
	dir := project(t, []string{"local"}, nil, map[string]pkgSpec{
		"local": {source: SourcePath, files: map[string]string{"lib/local.dart": "int f() => 1;"}},
	})
	before, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkgs", "local", "lib", "local.dart"), []byte("int f() => 2;"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Packages["local"].TreeHash == after.Packages["local"].TreeHash {
		t.Fatal("editing a path package's source must change its tree hash")
	}
	if before.GraphDigest == after.GraphDigest {
		t.Fatal("editing a path package's source must change the canonical graph digest")
	}
}

func TestGraphDigest_BindsCapabilityAndEdges(t *testing.T) {
	g, err := Resolve(project(t, []string{"a"}, nil, map[string]pkgSpec{"a": {deps: []string{"b"}}, "b": {}}))
	if err != nil {
		t.Fatal(err)
	}
	clone := func() Graph {
		c := g
		c.Packages = map[string]Package{}
		for k, v := range g.Packages {
			c.Packages[k] = v
		}
		return c
	}
	forged := clone()
	p := forged.Packages["a"]
	p.Capability.Eligible = false
	p.Capability.Reasons = []string{"forged"}
	forged.Packages["a"] = p
	if forged.RecomputeDigest() == g.GraphDigest {
		t.Fatal("capability must be bound into the graph digest")
	}

	edgeless := clone()
	p2 := edgeless.Packages["a"]
	p2.Dependencies = nil
	edgeless.Packages["a"] = p2
	if edgeless.RecomputeDigest() == g.GraphDigest {
		t.Fatal("canonical dependency edges must be bound into the graph digest")
	}
}

// ---- Strict graph decoding ----

func resolvedGraph(t *testing.T) Graph {
	t.Helper()
	g, err := Resolve(project(t, []string{"a"}, nil, map[string]pkgSpec{"a": {deps: []string{"b"}}, "b": {}}))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestDecodeGraphStrict_Roundtrip(t *testing.T) {
	g := resolvedGraph(t)
	raw, err := g.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeGraphStrict(raw)
	if err != nil {
		t.Fatalf("valid graph must roundtrip: %v", err)
	}
	if back.GraphDigest != g.GraphDigest {
		t.Fatal("digest changed across roundtrip")
	}
}

func TestDecodeGraphStrict_RejectsUnknownFieldAndTrailingJSON(t *testing.T) {
	g := resolvedGraph(t)
	raw, _ := g.MarshalCanonical()

	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["evil_extra"] = "x"
	withUnknown, _ := json.Marshal(m)
	if _, err := DecodeGraphStrict(withUnknown); err == nil {
		t.Fatal("unknown field must be rejected")
	}
	if _, err := DecodeGraphStrict(append(append([]byte{}, raw...), []byte(`{"more":1}`)...)); err == nil {
		t.Fatal("trailing JSON must be rejected")
	}
}

func TestDecodeGraphStrict_RejectsSwappedRecordDanglingEdgeAndTamper(t *testing.T) {
	g := resolvedGraph(t)
	clone := func() Graph {
		c := g
		c.Packages = map[string]Package{}
		for k, v := range g.Packages {
			c.Packages[k] = v
		}
		return c
	}
	t.Run("swapped map key", func(t *testing.T) {
		swapped := clone()
		a := swapped.Packages["a"]
		a.Name = "b" // key "a" now holds a record naming "b"
		swapped.Packages["a"] = a
		raw, _ := json.Marshal(swapped)
		if _, err := DecodeGraphStrict(raw); err == nil || !strings.Contains(err.Error(), "disagrees") {
			t.Fatalf("a swapped package record must be rejected, got %v", err)
		}
	})
	t.Run("dangling edge with rebound digest", func(t *testing.T) {
		dangling := clone()
		a := dangling.Packages["a"]
		a.Dependencies = []string{"nonexistent"}
		dangling.Packages["a"] = a
		dangling.GraphDigest = dangling.RecomputeDigest() // fully rebound
		raw, _ := json.Marshal(dangling)
		if _, err := DecodeGraphStrict(raw); err == nil || !strings.Contains(err.Error(), "dangling") {
			t.Fatalf("a dangling runtime edge must be rejected even with a rebound digest, got %v", err)
		}
	})
	t.Run("tampered digest", func(t *testing.T) {
		tampered := clone()
		tampered.PubspecLockSHA = strings.Repeat("f", 64)
		raw, _ := json.Marshal(tampered)
		if _, err := DecodeGraphStrict(raw); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("a tampered graph must fail the digest check, got %v", err)
		}
	})
}

// ---- Descriptor ----

func gWith(pkgs ...Package) Graph {
	m := map[string]Package{}
	for _, p := range pkgs {
		m[p.Name] = p
	}
	g := Graph{
		Schema:           GraphSchema,
		GeneratorVersion: GeneratorVersion,
		PubspecLockSHA:   strings.Repeat("a", 64),
		PackageConfigSHA: strings.Repeat("b", 64),
		RootPackage:      "app",
		Packages:         m,
	}
	g.GraphDigest = graphDigest(g)
	return g
}

func dartPkg(name, ver string) Package {
	return Package{
		Name: name, Version: ver, Source: SourceHosted,
		SourceID: "hosted:pub.dev/" + name, ContentHash: strings.Repeat("c", 64),
		PubspecSHA: strings.Repeat("d", 64), Dependencies: []string{},
		Capability: Capability{Eligible: true},
	}
}

func nativePkg(name, ver string) Package {
	p := dartPkg(name, ver)
	p.Capability = Capability{
		Eligible: false, HasNativePlugin: true,
		NativeDetail: "plugin.platforms: ios.pluginClass",
		Reasons:      []string{"declares native platform plugin code"},
	}
	return p
}

func TestDescriptor_AddDartOnly_Eligible(t *testing.T) {
	d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.0.0"), dartPkg("riverpod", "2.6.1")))
	if len(d.Added) != 1 || d.Added[0].Name != "riverpod" {
		t.Fatalf("expected riverpod added, got %+v", d.Added)
	}
	if err := d.Assess(); err != nil {
		t.Fatalf("Dart-only add must be accepted, got %v", err)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("descriptor must self-validate: %v", err)
	}
}

func TestDescriptor_AddNativePlugin_Refused(t *testing.T) {
	d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.0.0"), nativePkg("camera", "0.10.0")))
	err := d.Assess()
	if err == nil || !strings.Contains(err.Error(), "camera") || !strings.Contains(err.Error(), "Store release") {
		t.Fatalf("native plugin add must be refused with a store-release message, got %v", err)
	}
}

func TestDescriptor_RemovedRuntimeDep_Refused(t *testing.T) {
	d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0"), dartPkg("used", "1.0.0")), gWith(dartPkg("meta", "1.0.0")))
	if err := d.Assess(); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("removing a runtime dependency must be refused, got %v", err)
	}
}

func TestDescriptor_Upgrade_Detected(t *testing.T) {
	d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.1.0")))
	if len(d.Upgraded) != 1 || d.Upgraded[0].ToVer != "1.1.0" {
		t.Fatalf("expected meta upgrade, got %+v", d.Upgraded)
	}
	if err := d.Assess(); err != nil {
		t.Fatalf("Dart-only upgrade must be accepted, got %v", err)
	}
}

func TestDescriptor_SameVersionDifferentContent_IsAnUpgrade(t *testing.T) {
	base := dartPkg("local", "1.0.0")
	base.Source, base.SourceID, base.ContentHash, base.TreeHash = SourcePath, "path:./pkgs/local", "", strings.Repeat("1", 64)
	cand := base
	cand.TreeHash = strings.Repeat("2", 64)
	d := BuildDescriptor(gWith(base), gWith(cand))
	if len(d.Upgraded) != 1 {
		t.Fatalf("a repointed path package at the same version must register as changed, got %+v", d)
	}
}

func TestDescriptorDecodeStrict_RejectsUnknownFieldAndTrailingJSON(t *testing.T) {
	d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.0.0")))
	raw, _ := json.Marshal(d)
	if _, err := DecodeStrict(raw); err != nil {
		t.Fatalf("valid descriptor must decode: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["evil_extra"] = "x"
	withUnknown, _ := json.Marshal(m)
	if _, err := DecodeStrict(withUnknown); err == nil {
		t.Fatal("strict decode must reject an unknown descriptor field")
	}
	if _, err := DecodeStrict(append(append([]byte{}, raw...), '{', '}')); err == nil {
		t.Fatal("strict decode must reject trailing JSON")
	}
}

func TestDescriptorDecodeStrict_RejectsTamperedDigest(t *testing.T) {
	d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.0.0"), dartPkg("riverpod", "2.6.1")))
	d.Added[0].Version = "9.9.9" // tamper WITHOUT recomputing the digest
	raw, _ := json.Marshal(d)
	if _, err := DecodeStrict(raw); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered descriptor must fail the digest check, got %v", err)
	}
}

func TestDescriptorDecodeStrict_RejectsContradictoryRecords(t *testing.T) {
	t.Run("suppressed refusal (fully rebound)", func(t *testing.T) {
		// The strongest attack: delete the ineligible entry for a native package and recompute the
		// descriptor digest so every hash is internally consistent. The package's OWN capability record
		// still says ineligible, so the contradiction is detectable.
		d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.0.0"), nativePkg("camera", "0.10.0")))
		d.Ineligible = nil
		d.DescriptorDigest = d.computeDigest()
		raw, _ := json.Marshal(d)
		if _, err := DecodeStrict(raw); err == nil || !strings.Contains(err.Error(), "refusal was suppressed") {
			t.Fatalf("a suppressed refusal must be rejected even when fully rebound, got %v", err)
		}
	})
	t.Run("forged eligible flag with stale ineligible entry", func(t *testing.T) {
		d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.0.0"), nativePkg("camera", "0.10.0")))
		d.Added[0].Capability.Eligible = true
		d.DescriptorDigest = d.computeDigest()
		raw, _ := json.Marshal(d)
		if _, err := DecodeStrict(raw); err == nil || !strings.Contains(err.Error(), "contradicts itself") {
			t.Fatalf("a forged eligible flag must contradict the ineligible list, got %v", err)
		}
	})
	t.Run("same package in two categories", func(t *testing.T) {
		d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.0.0"), dartPkg("riverpod", "2.6.1")))
		d.Unchanged = append(d.Unchanged, "riverpod") // also listed as added
		d.DescriptorDigest = d.computeDigest()
		raw, _ := json.Marshal(d)
		if _, err := DecodeStrict(raw); err == nil || !strings.Contains(err.Error(), "both") {
			t.Fatalf("a package in two categories must be rejected, got %v", err)
		}
	})
	t.Run("duplicate added record", func(t *testing.T) {
		d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.0.0"), dartPkg("riverpod", "2.6.1")))
		d.Added = append(d.Added, d.Added[0])
		d.DescriptorDigest = d.computeDigest()
		raw, _ := json.Marshal(d)
		if _, err := DecodeStrict(raw); err == nil || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("a duplicate package record must be rejected, got %v", err)
		}
	})
	t.Run("no-op upgrade", func(t *testing.T) {
		d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.0.0")))
		d.Upgraded = []Upgrade{{Name: "meta", FromVer: "1.0.0", ToVer: "1.0.0", FromSourceID: "x", ToSourceID: "x", ToEligible: true}}
		d.Unchanged = nil
		d.DescriptorDigest = d.computeDigest()
		raw, _ := json.Marshal(d)
		if _, err := DecodeStrict(raw); err == nil || !strings.Contains(err.Error(), "nothing about it changed") {
			t.Fatalf("a no-op upgrade record must be rejected, got %v", err)
		}
	})
	t.Run("developer-local path in source_id", func(t *testing.T) {
		d := BuildDescriptor(gWith(dartPkg("meta", "1.0.0")), gWith(dartPkg("meta", "1.0.0"), dartPkg("riverpod", "2.6.1")))
		d.Added[0].SourceID = "path:/Users/someone/dev/riverpod"
		d.DescriptorDigest = d.computeDigest()
		raw, _ := json.Marshal(d)
		if _, err := DecodeStrict(raw); err == nil || !strings.Contains(err.Error(), "developer-local") {
			t.Fatalf("a developer-local path must be rejected, got %v", err)
		}
	})
}

func TestDescriptor_TOCTOUMismatch_Rejected(t *testing.T) {
	base := gWith(dartPkg("meta", "1.0.0"))
	cand := gWith(dartPkg("meta", "1.0.0"), dartPkg("riverpod", "2.6.1"))
	d := BuildDescriptor(base, cand)
	mutated := gWith(dartPkg("meta", "1.0.0"), dartPkg("riverpod", "2.6.1"), dartPkg("sneaky", "0.0.1"))
	if err := d.AssertMatchesCandidate(mutated); err == nil {
		t.Fatal("a candidate mutated after descriptor construction must be rejected")
	}
	if err := d.AssertMatchesCandidate(cand); err != nil {
		t.Fatalf("an unchanged candidate must match, got %v", err)
	}
}

func TestDescriptor_BaseAnchor_RejectsWrongBase(t *testing.T) {
	base := gWith(dartPkg("meta", "1.0.0"))
	cand := gWith(dartPkg("meta", "1.0.0"), dartPkg("riverpod", "2.6.1"))
	d := BuildDescriptor(base, cand)
	if err := d.AssertMatchesBase(base.GraphDigest, base.PubspecLockSHA, base.PackageConfigSHA); err != nil {
		t.Fatalf("the real base must match, got %v", err)
	}
	other := gWith(dartPkg("meta", "2.0.0"))
	if err := d.AssertMatchesBase(other.GraphDigest, other.PubspecLockSHA, other.PackageConfigSHA); err == nil {
		t.Fatal("a descriptor built against a different base must be rejected by the base anchor")
	}
}

// ---- Build-output comparison ----

func makeZip(t *testing.T, path string, files map[string]string) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildOutput_DartOnlyChange_NoDrift(t *testing.T) {
	dir := t.TempDir()
	base := makeZip(t, filepath.Join(dir, "base.apk"), map[string]string{
		"assets/flutter_assets/kernel_blob.bin": "OLD",
		"assets/flutter_assets/NOTICES.Z":       "licenses-v1",
		"lib/arm64-v8a/libflutter.so":           "engine",
	})
	cand := makeZip(t, filepath.Join(dir, "cand.apk"), map[string]string{
		"assets/flutter_assets/kernel_blob.bin": "NEW",
		"assets/flutter_assets/NOTICES.Z":       "licenses-v2",
		"lib/arm64-v8a/libflutter.so":           "engine",
	})
	diff, err := CompareBuildOutputs(base, cand)
	if err != nil {
		t.Fatal(err)
	}
	if diff.HasNativeOrAssetDrift() {
		t.Fatalf("a Dart-code-only change must not be native/asset drift: %s", diff.Explain())
	}
	if len(diff.ChangedLicenseMeta) != 1 {
		t.Fatalf("the license-metadata delta must be REPORTED (not silently dropped), got %v", diff.ChangedLicenseMeta)
	}
}

func TestBuildOutput_NewNativeLib_IsDrift(t *testing.T) {
	dir := t.TempDir()
	base := makeZip(t, filepath.Join(dir, "base.apk"), map[string]string{"lib/arm64-v8a/libflutter.so": "engine"})
	cand := makeZip(t, filepath.Join(dir, "cand.apk"), map[string]string{
		"lib/arm64-v8a/libflutter.so": "engine",
		"lib/arm64-v8a/libcamera.so":  "new-native",
	})
	diff, err := CompareBuildOutputs(base, cand)
	if err != nil {
		t.Fatal(err)
	}
	if !diff.HasNativeOrAssetDrift() || len(diff.AddedNativeLibs) != 1 {
		t.Fatalf("a new native library must be drift, got %+v", diff)
	}
}

func TestBuildOutput_RealAssetAndRegistrant_AreDrift(t *testing.T) {
	dir := t.TempDir()
	base := makeZip(t, filepath.Join(dir, "base.apk"), map[string]string{
		"assets/flutter_assets/AssetManifest.json":           "{}",
		"io/flutter/plugins/GeneratedPluginRegistrant.class": "v1",
	})
	cand := makeZip(t, filepath.Join(dir, "cand.apk"), map[string]string{
		"assets/flutter_assets/AssetManifest.json":           `{"a.png":["a.png"]}`,
		"assets/flutter_assets/packages/icons/a.png":         "PNG",
		"io/flutter/plugins/GeneratedPluginRegistrant.class": "v2",
	})
	diff, err := CompareBuildOutputs(base, cand)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.AddedAssets) != 1 || len(diff.ChangedAssets) != 1 || len(diff.ChangedRegistrant) != 1 {
		t.Fatalf("real assets and a changed plugin registrant must all be drift, got %+v", diff)
	}
}
