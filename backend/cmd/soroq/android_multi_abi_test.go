package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"soroq/backend/internal/androidrelease"
)

var allAndroidPlatforms = []string{"android-arm", "android-arm64", "android-x64"}

// installMultiABISeedTestToolchain is installSeedTestToolchain plus soroq_android_abis.
func installMultiABISeedTestToolchain(t *testing.T, abis []string) string {
	t.Helper()
	installSeedTestToolchain(t, []string{androidObfuscationSeedCapability}, "--load-obfuscation-map")
	dir, err := androidCachedToolchainBundleDir(seedTestToolchain)
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := json.Marshal(map[string]any{
		"schema":                     "soroq.android_engine.v1",
		"soroq_android_capabilities": []string{androidObfuscationSeedCapability},
		"soroq_android_abis":         abis,
	})
	if err := os.WriteFile(filepath.Join(dir, "engine.json"), engine, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func stubDefaultTargetPlatforms(t *testing.T, platforms []string) {
	t.Helper()
	prev := androidDefaultTargetPlatformsFn
	androidDefaultTargetPlatformsFn = func(string) ([]string, error) { return platforms, nil }
	t.Cleanup(func() { androidDefaultTargetPlatformsFn = prev })
}

func TestAndroidToolchainABIs(t *testing.T) {
	dir := t.TempDir()
	if got, err := androidToolchainABIs(dir); err != nil || !reflect.DeepEqual(got, []string{"arm64-v8a"}) {
		t.Fatalf("no engine.json: %v %v", got, err)
	}
	write := func(abis []string) {
		raw, _ := json.Marshal(map[string]any{"soroq_android_abis": abis})
		if err := os.WriteFile(filepath.Join(dir, "engine.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(nil)
	if got, _ := androidToolchainABIs(dir); !reflect.DeepEqual(got, []string{"arm64-v8a"}) {
		t.Fatalf("arm64-only toolchain: %v", got)
	}
	write([]string{"x86_64", "arm64-v8a", "armeabi-v7a"})
	if got, _ := androidToolchainABIs(dir); !reflect.DeepEqual(got, []string{"armeabi-v7a", "arm64-v8a", "x86_64"}) {
		t.Fatalf("declared order is not table order: %v", got)
	}
	write([]string{"armeabi-v7a"})
	if _, err := androidToolchainABIs(dir); err == nil || !strings.Contains(err.Error(), "does not include arm64-v8a") {
		t.Fatalf("no primary: %v", err)
	}
	write([]string{"arm64-v8a", "mips"})
	if _, err := androidToolchainABIs(dir); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported: %v", err)
	}
}

// fakeFrontend returns a bin/flutter whose root does or does not declare multi_abi_v1.
func fakeFrontend(t *testing.T, multiABI bool) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin", "flutter")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if multiABI {
		marker := filepath.Join(root, "bin", "cache", "soroq", androidFrontendMultiABIMarker)
		if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte(`{"schema":"soroq.frontend_capability.multi_abi.v1"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

func TestResolveAndroidTargetPlatformArgs(t *testing.T) {
	multi := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"soroq_android_abis": []string{"arm64-v8a", "armeabi-v7a", "x86_64"}})
	if err := os.WriteFile(filepath.Join(multi, "engine.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	arm64Only := t.TempDir()
	cached := func(dir string) androidEngineSource {
		return androidEngineSource{Kind: androidEngineSourceCachedToolchain, BundleDir: dir}
	}
	withMarker, withoutMarker := fakeFrontend(t, true), fakeFrontend(t, false)

	// multi toolchain + multi frontend, nothing named: every ABI
	got, err := resolveAndroidTargetPlatformArgs([]string{"--obfuscate"}, cached(multi), withMarker)
	if err != nil || !reflect.DeepEqual(got, []string{"--obfuscate", "--target-platform", "android-arm,android-arm64,android-x64"}) {
		t.Fatalf("multi default: %v %v", got, err)
	}
	// either half missing: the historical arm64 default is left to the helper (args untouched)
	for name, c := range map[string]struct {
		dir, bin string
	}{"arm64-only toolchain": {arm64Only, withMarker}, "frontend without marker": {multi, withoutMarker}} {
		got, err := resolveAndroidTargetPlatformArgs([]string{"--obfuscate"}, cached(c.dir), c.bin)
		if err != nil || !reflect.DeepEqual(got, []string{"--obfuscate"}) {
			t.Fatalf("%s: %v %v", name, got, err)
		}
		if soroqAndroidABIsForTargetPlatforms(soroqAndroidBuildExtraArgsForSource(got, c.bin, cached(c.dir))) != "arm64-v8a" {
			t.Fatalf("%s: effective build is not arm64-only", name)
		}
	}
	// a named subset the stack has: kept
	if got, err := resolveAndroidTargetPlatformArgs([]string{"--target-platform=android-arm64,android-arm"}, cached(multi), withMarker); err != nil || len(got) != 1 {
		t.Fatalf("named subset: %v %v", got, err)
	}
	// a named ABI the stack lacks: refused, never silently arm64
	if _, err := resolveAndroidTargetPlatformArgs([]string{"--target-platform", "android-arm"}, cached(arm64Only), withMarker); err == nil || !strings.Contains(err.Error(), "can build only android-arm64") {
		t.Fatalf("unbuildable ABI: %v", err)
	}
	// an explicit --local-engine source is the operator's own and is not second-guessed
	if got, err := resolveAndroidTargetPlatformArgs([]string{"--target-platform", "android-arm"}, androidEngineSource{Kind: androidEngineSourceAdvancedLocalEngine}, withMarker); err != nil || len(got) != 2 {
		t.Fatalf("legacy: %v %v", got, err)
	}
}

func TestWithBaseTargetPlatforms(t *testing.T) {
	got, err := withBaseTargetPlatforms([]string{"--obfuscate"}, []string{"x86_64", "armeabi-v7a", "arm64-v8a"})
	if err != nil || !reflect.DeepEqual(got, []string{"--obfuscate", "--target-platform", "android-arm,android-arm64,android-x64"}) {
		t.Fatalf("multi base: %v %v", got, err)
	}
	got, err = withBaseTargetPlatforms(nil, []string{"arm64-v8a"})
	if err != nil || !reflect.DeepEqual(got, []string{"--target-platform", "android-arm64"}) {
		t.Fatalf("arm64 base: %v %v", got, err)
	}
	if _, err := withBaseTargetPlatforms([]string{"--target-platform", "android-arm64"}, []string{"arm64-v8a", "armeabi-v7a"}); err == nil || !strings.Contains(err.Error(), "exactly the base's ABIs") {
		t.Fatalf("named set differing from base: %v", err)
	}
	if got, err := withBaseTargetPlatforms([]string{"--target-platform=android-arm64,android-arm"}, []string{"armeabi-v7a", "arm64-v8a"}); err != nil || len(got) != 1 {
		t.Fatalf("named set equal to base: %v %v", got, err)
	}
}

func TestMaterializeAndroidExtraABIs(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"soroq_android_abis": []string{"arm64-v8a", "armeabi-v7a", "x86_64"}})
	if err := os.WriteFile(filepath.Join(dir, "engine.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, abi := range []string{"armeabi-v7a", "x86_64"} {
		s, u, g := androidToolchainABIArtifacts(abi)
		for _, name := range []string{s, u, g} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := materializeAndroidExtraABIs(dir); err != nil {
		t.Fatal(err)
	}
	if err := materializeAndroidExtraABIMaven(dir, "1.0.0-abc"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ abi, cpu, maven string }{{"armeabi-v7a", "arm", "armeabi_v7a"}, {"x86_64", "x64", "x86_64"}} {
		out := filepath.Join(dir, "out", "android_release_"+c.cpu)
		s, u, g := androidToolchainABIArtifacts(c.abi)
		for rel, want := range map[string]string{"lib.stripped/libflutter.so": s, "libflutter.so": u, "universal/gen_snapshot": g} {
			got, err := os.ReadFile(filepath.Join(out, rel))
			if err != nil || string(got) != want {
				t.Fatalf("%s %s: %q %v", c.abi, rel, got, err)
			}
		}
		zr, err := zip.OpenReader(filepath.Join(out, c.maven+"_release.jar"))
		if err != nil {
			t.Fatal(err)
		}
		if len(zr.File) != 1 || zr.File[0].Name != "lib/"+c.abi+"/libflutter.so" {
			t.Fatalf("%s jar holds %v", c.abi, zr.File[0].Name)
		}
		zr.Close()
		for _, f := range []string{c.maven + "_release.pom", c.maven + "_release.maven-metadata.xml"} {
			b, err := os.ReadFile(filepath.Join(out, f))
			if err != nil || !strings.Contains(string(b), "1.0.0-abc") {
				t.Fatalf("%s: %v", f, err)
			}
		}
	}
	// a declared ABI whose artifacts are missing is refused
	s, _, _ := androidToolchainABIArtifacts("x86_64")
	if err := os.Remove(filepath.Join(dir, s)); err != nil {
		t.Fatal(err)
	}
	if err := materializeAndroidExtraABIs(dir); err == nil || !strings.Contains(err.Error(), "declares x86_64") {
		t.Fatalf("missing artifact: %v", err)
	}
}

// The real finding behind per-ABI maps: two ABIs' gen_snapshots name the same identifiers differently.
func TestMultiABIObfuscationKeepsOneMapPerABI(t *testing.T) {
	installMultiABISeedTestToolchain(t, []string{"arm64-v8a", "armeabi-v7a", "x86_64"})
	stubDefaultTargetPlatforms(t, allAndroidPlatforms)
	project := t.TempDir()
	obf := []string{"--obfuscate", "--split-debug-info=build/symbols"}

	rel, err := planAndroidReleaseObfuscation(project, seedTestToolchain, obf)
	if err != nil {
		t.Fatal(err)
	}
	defer rel.cleanup()
	want := []string{"--target-platform", "android-arm,android-arm64,android-x64",
		"--extra-gen-snapshot-options=--save-obfuscation-map=" + filepath.Join(filepath.Dir(rel.SavedMapPath), "map.{soroq_target_platform}.json")}
	if !reflect.DeepEqual(rel.ExtraArgs, want) {
		t.Fatalf("release args %v, want %v", rel.ExtraArgs, want)
	}
	// every ABI must have written its map
	writeMap(t, expandObfuscationPlatform(rel.SavedMapPath, "android-arm64"), "Foo", "a", "lib:x", "b")
	writeMap(t, expandObfuscationPlatform(rel.SavedMapPath, "android-arm"), "Foo", "a", "lib:x", "c")
	if _, err := commitAndroidReleaseObfuscation(project, "multi", seedTestToolchain, rel); err == nil || !strings.Contains(err.Error(), "android-x64") {
		t.Fatalf("release with a missing ABI map registered: %v", err)
	}
	writeMap(t, expandObfuscationPlatform(rel.SavedMapPath, "android-x64"), "Foo", "a", "lib:x", "d")
	rec, err := commitAndroidReleaseObfuscation(project, "multi", seedTestToolchain, rel)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Schema != androidObfuscationRecordSchemaV2 || len(rec.Maps) != 3 || !reflect.DeepEqual(rec.platforms(), allAndroidPlatforms) {
		t.Fatalf("record %+v", rec)
	}
	got, mapPath, err := loadAndroidReleaseObfuscation(project, "multi")
	if err != nil || got == nil || filepath.Base(mapPath) != androidObfuscationMapPattern {
		t.Fatalf("load: %+v %s %v", got, mapPath, err)
	}

	// the patch must build exactly the base's ABIs, each seeded from its own map
	if _, err := planAndroidPatchObfuscation(project, "multi", seedTestToolchain, seedObfArgs); err == nil || !strings.Contains(err.Error(), "exactly those ABIs") {
		t.Fatalf("arm64-only patch over a multi-ABI base: %v", err)
	}
	patchArgs, err := withBaseTargetPlatforms(obf, []string{"arm64-v8a", "armeabi-v7a", "x86_64"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planAndroidPatchObfuscation(project, "multi", seedTestToolchain, patchArgs)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.cleanup()
	wantPatch := "--extra-gen-snapshot-options=--load-obfuscation-map=" + mapPath + ",--save-obfuscation-map=" + plan.SavedMapPath
	if len(plan.ExtraArgs) != 1 || plan.ExtraArgs[0] != wantPatch || !strings.Contains(plan.SavedMapPath, androidObfuscationPlatformToken) {
		t.Fatalf("patch args %v", plan.ExtraArgs)
	}
	writeMap(t, expandObfuscationPlatform(plan.SavedMapPath, "android-arm64"), "Foo", "a", "lib:x", "b", "New", "e")
	writeMap(t, expandObfuscationPlatform(plan.SavedMapPath, "android-arm"), "Foo", "a", "lib:x", "c", "New", "e")
	writeMap(t, expandObfuscationPlatform(plan.SavedMapPath, "android-x64"), "Foo", "a", "lib:x", "d", "New", "e")
	v, err := verifyAndroidSeededCandidate(plan)
	if err != nil || v.ABIs != 3 || v.BaseEntries != 2 || v.NewEntries != 1 {
		t.Fatalf("seeded multi-ABI candidate: %+v %v", v, err)
	}
	// one ABI renamed a base name: refused, naming that ABI
	writeMap(t, expandObfuscationPlatform(plan.SavedMapPath, "android-arm"), "Foo", "a", "lib:x", "b", "New", "e")
	if _, err := verifyAndroidSeededCandidate(plan); err == nil || !strings.Contains(err.Error(), "android-arm:") {
		t.Fatalf("a renamed name in one ABI: %v", err)
	}

	// a tampered ABI map is refused at load, never read as plain
	if err := os.WriteFile(expandObfuscationPlatform(mapPath, "android-x64"), []byte(`["Foo","z"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadAndroidReleaseObfuscation(project, "multi"); err == nil || !strings.Contains(err.Error(), "does not match its recorded digest") {
		t.Fatalf("tampered x64 map: %v", err)
	}
}

func TestMultiABIObfuscationNeedsEveryABIsSeededGenSnapshot(t *testing.T) {
	installMultiABISeedTestToolchain(t, []string{"arm64-v8a", "armeabi-v7a"})
	prev := androidGenSnapshotHelpFn
	androidGenSnapshotHelpFn = func(path string) (string, error) {
		if strings.HasSuffix(path, "gen_snapshot_armeabi-v7a") {
			return "stock usage", nil
		}
		return "--load-obfuscation-map", nil
	}
	t.Cleanup(func() { androidGenSnapshotHelpFn = prev })
	args := []string{"--obfuscate", "--split-debug-info=x", "--target-platform", "android-arm,android-arm64"}
	if _, err := planAndroidReleaseObfuscation(t.TempDir(), seedTestToolchain, args); err == nil || !strings.Contains(err.Error(), "armeabi-v7a gen_snapshot") {
		t.Fatalf("stock v7a gen_snapshot accepted: %v", err)
	}
}

// An arm64-only stack keeps the historical v1 record, byte for byte in shape.
func TestSingleABIObfuscationKeepsTheV1Record(t *testing.T) {
	installSeedTestToolchain(t, []string{androidObfuscationSeedCapability}, "--load-obfuscation-map")
	stubDefaultTargetPlatforms(t, []string{"android-arm64"})
	project := t.TempDir()
	plan, err := planAndroidReleaseObfuscation(project, seedTestToolchain, []string{"--obfuscate", "--split-debug-info=x"})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.cleanup()
	if filepath.Base(plan.SavedMapPath) != "map.json" || plan.ExtraArgs[len(plan.ExtraArgs)-1] != "--extra-gen-snapshot-options=--save-obfuscation-map="+plan.SavedMapPath {
		t.Fatalf("single-ABI plan %+v", plan)
	}
	writeMap(t, plan.SavedMapPath, "Foo", "a")
	rec, err := commitAndroidReleaseObfuscation(project, "one", seedTestToolchain, plan)
	if err != nil || rec.Schema != androidObfuscationRecordSchema || rec.Maps != nil || rec.MapFile != androidObfuscationMapFile {
		t.Fatalf("single-ABI record %+v %v", rec, err)
	}
	raw, _ := os.ReadFile(filepath.Join(project, ".soroq", "releases", "one", androidObfuscationRecordFile))
	if strings.Contains(string(raw), "maps") {
		t.Fatalf("v1 record grew a maps key: %s", raw)
	}
}

// A single non-arm64 ABI (an armeabi-v7a-only app) is obfuscated through the per-ABI record.
func TestSingleNonArm64ABIObfuscationUsesThePerABIRecord(t *testing.T) {
	installMultiABISeedTestToolchain(t, []string{"arm64-v8a", "armeabi-v7a"})
	project := t.TempDir()
	args := []string{"--obfuscate", "--split-debug-info=x", "--target-platform", "android-arm"}
	rel, err := planAndroidReleaseObfuscation(project, seedTestToolchain, args)
	if err != nil {
		t.Fatal(err)
	}
	defer rel.cleanup()
	if !strings.Contains(rel.SavedMapPath, androidObfuscationPlatformToken) || !reflect.DeepEqual(rel.TargetPlatforms, []string{"android-arm"}) {
		t.Fatalf("v7a-only plan %+v", rel)
	}
	writeMap(t, expandObfuscationPlatform(rel.SavedMapPath, "android-arm"), "Foo", "a")
	rec, err := commitAndroidReleaseObfuscation(project, "v7a", seedTestToolchain, rel)
	if err != nil || rec.Schema != androidObfuscationRecordSchemaV2 || !reflect.DeepEqual(rec.platforms(), []string{"android-arm"}) {
		t.Fatalf("v7a-only record %+v %v", rec, err)
	}
	if _, _, err := loadAndroidReleaseObfuscation(project, "v7a"); err != nil {
		t.Fatal(err)
	}
	patchArgs, err := withBaseTargetPlatforms([]string{"--obfuscate", "--split-debug-info=x"}, []string{"armeabi-v7a"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planAndroidPatchObfuscation(project, "v7a", seedTestToolchain, patchArgs)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.cleanup()
	writeMap(t, expandObfuscationPlatform(plan.SavedMapPath, "android-arm"), "Foo", "a", "New", "b")
	if v, err := verifyAndroidSeededCandidate(plan); err != nil || v.NewEntries != 1 {
		t.Fatalf("v7a-only seeded patch: %+v %v", v, err)
	}
}

func TestIncompleteAndroidABIs(t *testing.T) {
	snap := func(paths ...string) *androidrelease.Snapshot {
		s := &androidrelease.Snapshot{}
		for _, p := range paths {
			s.NativeLibs = append(s.NativeLibs, androidrelease.EntryDigest{Path: p})
		}
		return s
	}
	// what every arm64-only Soroq release shipped: plugin libraries for ABIs with no engine
	shipped := snap("lib/arm64-v8a/libapp.so", "lib/arm64-v8a/libflutter.so", "lib/arm64-v8a/libdartjni.so",
		"lib/armeabi-v7a/libdartjni.so", "lib/x86_64/libdartjni.so")
	if got := incompleteAndroidABIs(shipped); !reflect.DeepEqual(got, []string{"armeabi-v7a", "x86_64"}) {
		t.Fatalf("got %v", got)
	}
	if err := guardIncompleteAndroidABIs(shipped, true); err == nil || !strings.Contains(err.Error(), "armeabi-v7a, x86_64") {
		t.Fatalf("strict: %v", err)
	}
	if err := guardIncompleteAndroidABIs(shipped, false); err != nil {
		t.Fatalf("lenient must only warn: %v", err)
	}
	complete := snap("lib/arm64-v8a/libapp.so", "lib/arm64-v8a/libflutter.so", "lib/armeabi-v7a/libapp.so", "lib/armeabi-v7a/libflutter.so")
	if got := incompleteAndroidABIs(complete); len(got) != 0 {
		t.Fatalf("complete: %v", got)
	}
}
