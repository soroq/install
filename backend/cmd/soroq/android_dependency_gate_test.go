package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateProject builds a minimal resolvable runtime-graph project for the gate to classify.
func gateProject(t *testing.T, extraDep string, depPubspecExtra string) string {
	t.Helper()
	dir := t.TempDir()
	deps := "  pure: any\n"
	if extraDep != "" {
		deps += "  " + extraDep + ": any\n"
	}
	mustWrite(t, filepath.Join(dir, "pubspec.yaml"), "name: app\ndependencies:\n"+deps)

	lock := "packages:\n"
	var cfg []string
	addPkg := func(name, extra string) {
		mustWrite(t, filepath.Join(dir, "pkgs", name, "pubspec.yaml"), "name: "+name+"\n"+extra)
		mustWrite(t, filepath.Join(dir, "pkgs", name, "lib", name+".dart"), "int f() => 1;")
		lock += "  " + name + ":\n    dependency: direct main\n    source: hosted\n    version: \"1.0.0\"\n" +
			"    description:\n      name: " + name + "\n      url: \"https://pub.dev\"\n      sha256: \"" + strings.Repeat("a", 64) + "\"\n"
		cfg = append(cfg, `{"name":"`+name+`","rootUri":"../pkgs/`+name+`","packageUri":"lib/"}`)
	}
	addPkg("pure", "")
	if extraDep != "" {
		addPkg(extraDep, depPubspecExtra)
	}
	cfg = append(cfg, `{"name":"app","rootUri":"../","packageUri":"lib/"}`)
	mustWrite(t, filepath.Join(dir, "pubspec.lock"), lock)
	mustWrite(t, filepath.Join(dir, ".dart_tool", "package_config.json"),
		`{"configVersion":2,"packages":[`+strings.Join(cfg, ",")+`]}`)
	return dir
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gateAPK(t *testing.T, path string, files map[string]string) string {
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

// The exact real-world shape: only libapp.so and NOTICES.Z move. Must be ACCEPTED, with the license
// delta surfaced rather than silently dropped.
func TestAndroidGate_DartOnlyDependencyAdd_AcceptedWithLicenseWarning(t *testing.T) {
	dir := t.TempDir()
	base := gateAPK(t, filepath.Join(dir, "base.apk"), map[string]string{
		"lib/arm64-v8a/libapp.so":         "DART-AOT-BASE",
		"lib/arm64-v8a/libflutter.so":     "ENGINE",
		"assets/flutter_assets/NOTICES.Z": "licenses-without-riverpod",
	})
	cand := gateAPK(t, filepath.Join(dir, "cand.apk"), map[string]string{
		"lib/arm64-v8a/libapp.so":         "DART-AOT-WITH-RIVERPOD",
		"lib/arm64-v8a/libflutter.so":     "ENGINE",
		"assets/flutter_assets/NOTICES.Z": "licenses-with-riverpod",
	})
	delta, err := assertAndroidDependencyDeliverable(gateProject(t, "riverpod", ""), base, cand)
	if err != nil {
		t.Fatalf("a Dart-only dependency add must be accepted: %v", err)
	}
	if !delta.Changed || len(delta.Paths) != 1 {
		t.Fatalf("the license delta must be surfaced, got %+v", delta)
	}
	w := delta.Warning()
	for _, want := range []string{"NOTICES.Z", "license screen", "store release", "No runtime behaviour is affected"} {
		if !strings.Contains(w, want) {
			t.Fatalf("warning must mention %q, got:\n%s", want, w)
		}
	}
}

func TestAndroidGate_NewNativeLibrary_RefusedWithAttribution(t *testing.T) {
	dir := t.TempDir()
	base := gateAPK(t, filepath.Join(dir, "base.apk"), map[string]string{
		"lib/arm64-v8a/libapp.so": "DART-AOT-BASE",
	})
	cand := gateAPK(t, filepath.Join(dir, "cand.apk"), map[string]string{
		"lib/arm64-v8a/libapp.so":    "DART-AOT-CAND",
		"lib/arm64-v8a/libcamera.so": "NATIVE",
	})
	proj := gateProject(t, "camera", "flutter:\n  plugin:\n    platforms:\n      android:\n        pluginClass: CameraPlugin\n")
	_, err := assertAndroidDependencyDeliverable(proj, base, cand)
	if err == nil {
		t.Fatal("a new native library must refuse the code patch")
	}
	for _, want := range []string{"libcamera.so", "camera", "Play Store release"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must mention %q, got: %v", want, err)
		}
	}
}

func TestAndroidGate_RealAssetAndRegistrant_Refused(t *testing.T) {
	dir := t.TempDir()
	base := gateAPK(t, filepath.Join(dir, "base.apk"), map[string]string{
		"lib/arm64-v8a/libapp.so":                            "A",
		"io/flutter/plugins/GeneratedPluginRegistrant.class": "v1",
	})
	cand := gateAPK(t, filepath.Join(dir, "cand.apk"), map[string]string{
		"lib/arm64-v8a/libapp.so":                            "B",
		"io/flutter/plugins/GeneratedPluginRegistrant.class": "v2",
		"assets/flutter_assets/packages/icons/a.png":         "PNG",
	})
	if _, err := assertAndroidDependencyDeliverable(gateProject(t, "", ""), base, cand); err == nil {
		t.Fatal("a changed plugin registrant and a new real asset must refuse the code patch")
	}
}

func TestAndroidGate_UnresolvableRuntimeGraph_FailsClosed(t *testing.T) {
	dir := t.TempDir()
	base := gateAPK(t, filepath.Join(dir, "base.apk"), map[string]string{"lib/arm64-v8a/libapp.so": "A"})
	cand := gateAPK(t, filepath.Join(dir, "cand.apk"), map[string]string{"lib/arm64-v8a/libapp.so": "B"})
	// A project that HAS been pub-resolved but whose runtime graph does not resolve (a runtime edge to a
	// package absent from the lock) is a real integrity failure and must fail closed.
	broken := t.TempDir()
	mustWrite(t, filepath.Join(broken, "pubspec.yaml"), "name: app\ndependencies:\n  ghost: any\n")
	mustWrite(t, filepath.Join(broken, "pubspec.lock"), "packages:\n")
	mustWrite(t, filepath.Join(broken, ".dart_tool", "package_config.json"), `{"configVersion":2,"packages":[]}`)
	_, err := assertAndroidDependencyDeliverable(broken, base, cand)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("an unresolvable runtime dependency graph in a pub-resolved project must fail closed, got %v", err)
	}

	// A project that was never pub-resolved has no dependency metadata to read; the artifact comparison
	// alone governs, and it must NOT lose the gate.
	unresolved := t.TempDir()
	mustWrite(t, filepath.Join(unresolved, "pubspec.yaml"), "name: app\n")
	if _, err := assertAndroidDependencyDeliverable(unresolved, base, cand); err != nil {
		t.Fatalf("a project with no pub resolution must still pass the artifact gate, got %v", err)
	}
	withNative := gateAPK(t, filepath.Join(dir, "native.apk"), map[string]string{
		"lib/arm64-v8a/libapp.so": "B", "lib/arm64-v8a/libx.so": "NATIVE",
	})
	if _, err := assertAndroidDependencyDeliverable(unresolved, base, withNative); err == nil {
		t.Fatal("the artifact gate must still refuse native drift without dependency metadata")
	}
}
