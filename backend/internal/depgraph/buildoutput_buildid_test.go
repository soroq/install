package depgraph

import (
	"archive/zip"
	"bytes"
	"debug/elf"
	"os"
	"path/filepath"
	"testing"
)

// BUILD-ID-ONLY NATIVE DRIFT, at the build-output comparison.
//
// This was the THIRD copy of the same raw-SHA comparison. The first fix landed on soroqctl's
// internal planner, the second on androidpatch.PrepareCodePatchPlan — and `soroq patch android
// --code` STILL refused a build-id-only libdartjni.so, because it also calls
// assertAndroidDependencyDeliverable, which reaches CompareBuildOutputs down here.
//
// So these tests are written against the real consequence, not against the helper: a pair of build
// outputs differing only in a linker build-id must produce NO native drift, and a pair differing in
// actual code must still produce it. The second half is what stops this from being a hole.

// The fixtures are the ELF pairs built for the normaliser: identical sources linked with two
// different explicit --build-id values, one pair per shipped ABI. Shared rather than duplicated —
// a second copy would drift from the implementation it is meant to pin.
const buildIDFixtureRoot = "../nativeelf/testdata/native_build_id"

func readBuildIDFixture(t *testing.T, abi string, variant string) []byte {
	t.Helper()

	path := filepath.Join(buildIDFixtureRoot, abi, "buildid_"+variant+".so")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %q: %v", path, err)
	}
	return raw
}

// writeAPK builds a minimal zip whose entries are placed at paths that categorizeOutputPath
// classifies as native libraries, so the comparison under test is genuinely the native one.
func writeAPK(t *testing.T, dir string, name string, libs map[string][]byte) string {
	t.Helper()

	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for entry, data := range libs {
		w, err := zw.Create(entry)
		if err != nil {
			t.Fatalf("zip create %s: %v", entry, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip write %s: %v", entry, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return path
}

func nativeLibEntry(abi string) string {
	return "lib/" + abi + "/libdartjni.so"
}

// Guards the premise. If these paths ever stopped classifying as native libraries, every assertion
// below would pass by comparing nothing at all.
func TestBuildIDFixturePathsClassifyAsNativeLibraries(t *testing.T) {
	t.Parallel()

	for _, abi := range []string{"arm64-v8a", "armeabi-v7a", "x86_64"} {
		if got := categorizeOutputPath(nativeLibEntry(abi)); got != CatNativeLib {
			t.Fatalf("%s classified as %q, want %q — the drift tests would be vacuous",
				nativeLibEntry(abi), got, CatNativeLib)
		}
	}
}

// THE DEFECT. Identical code, different linker build-id, across all three shipped ABIs.
func TestCompareBuildOutputsIgnoresBuildIDOnlyNativeDrift(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	base := map[string][]byte{}
	cand := map[string][]byte{}
	for _, abi := range []string{"arm64-v8a", "armeabi-v7a", "x86_64"} {
		base[nativeLibEntry(abi)] = readBuildIDFixture(t, abi, "a")
		cand[nativeLibEntry(abi)] = readBuildIDFixture(t, abi, "b")
	}

	// Premise check: the files really ARE byte-different, or this proves nothing.
	for entry := range base {
		if bytes.Equal(base[entry], cand[entry]) {
			t.Fatalf("%s: fixtures are byte-identical, so the test cannot detect a regression", entry)
		}
	}

	diff, err := CompareBuildOutputs(
		writeAPK(t, dir, "base.apk", base),
		writeAPK(t, dir, "cand.apk", cand),
	)
	if err != nil {
		t.Fatalf("CompareBuildOutputs: %v", err)
	}
	if len(diff.ChangedNativeLibs) != 0 {
		t.Fatalf("a build-id-only difference was reported as native drift: %v.\n"+
			"This is what made `soroq patch android --code` refuse a deliverable patch for every "+
			"consumer of a plugin with a native library.", diff.ChangedNativeLibs)
	}
	if diff.HasNativeOrAssetDrift() {
		t.Fatalf("HasNativeOrAssetDrift() is true on a build-id-only difference: %s", diff.Explain())
	}
}

// THE CONTROL. Real code drift must still be refused, or the fix above is a hole rather than a fix.
func TestCompareBuildOutputsStillBlocksRealNativeCodeDrift(t *testing.T) {
	t.Parallel()

	for _, abi := range []string{"arm64-v8a", "armeabi-v7a", "x86_64"} {
		t.Run(abi, func(t *testing.T) {
			t.Parallel()

			raw := readBuildIDFixture(t, abi, "a")
			// Flip a byte in the middle of .text: same size, same build-id, different CODE.
			file, err := elf.NewFile(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("parse ELF: %v", err)
			}
			text := file.Section(".text")
			if text == nil {
				t.Fatal(".text not found")
			}
			mutated := make([]byte, len(raw))
			copy(mutated, raw)
			mutated[text.Offset+text.Size/2] ^= 0xff

			dir := t.TempDir()
			diff, err := CompareBuildOutputs(
				writeAPK(t, dir, "base.apk", map[string][]byte{nativeLibEntry(abi): raw}),
				writeAPK(t, dir, "cand.apk", map[string][]byte{nativeLibEntry(abi): mutated}),
			)
			if err != nil {
				t.Fatalf("CompareBuildOutputs: %v", err)
			}
			if len(diff.ChangedNativeLibs) != 1 {
				t.Fatalf("a real .text change was NOT reported as native drift (%v); normalisation "+
					"is masking code changes, which would ship an undeliverable patch",
					diff.ChangedNativeLibs)
			}
		})
	}
}

// A library that cannot be parsed must fall back to strict comparison, never to "unchanged".
func TestCompareBuildOutputsFailsClosedOnUnparseableNativeLibrary(t *testing.T) {
	t.Parallel()

	raw := readBuildIDFixture(t, "arm64-v8a", "a")
	truncated := raw[:len(raw)/3]

	dir := t.TempDir()
	entry := nativeLibEntry("arm64-v8a")
	diff, err := CompareBuildOutputs(
		writeAPK(t, dir, "base.apk", map[string][]byte{entry: raw}),
		writeAPK(t, dir, "cand.apk", map[string][]byte{entry: truncated}),
	)
	if err != nil {
		t.Fatalf("CompareBuildOutputs: %v", err)
	}
	if len(diff.ChangedNativeLibs) != 1 {
		t.Fatalf("a truncated, unparseable library was not reported as drift: %v", diff.ChangedNativeLibs)
	}
}

// A non-ELF file classified as a native library keeps the pre-existing strict behaviour: the
// normaliser is ELF-only by construction, which is what bounds the blast radius of this change for
// iOS Mach-O members that share CatNativeLib.
func TestCompareBuildOutputsKeepsNonELFNativeLibrariesStrict(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	entry := "lib/arm64-v8a/libnotanelf.so"
	if got := categorizeOutputPath(entry); got != CatNativeLib {
		t.Fatalf("%s classified as %q, want %q", entry, got, CatNativeLib)
	}
	diff, err := CompareBuildOutputs(
		writeAPK(t, dir, "base.apk", map[string][]byte{entry: []byte("definitely not an ELF, v1")}),
		writeAPK(t, dir, "cand.apk", map[string][]byte{entry: []byte("definitely not an ELF, v2")}),
	)
	if err != nil {
		t.Fatalf("CompareBuildOutputs: %v", err)
	}
	if len(diff.ChangedNativeLibs) != 1 {
		t.Fatalf("a changed non-ELF native library was not reported as drift: %v", diff.ChangedNativeLibs)
	}
}

// Directory scanning is a SEPARATE code path from archive scanning (scanBuildDir vs
// scanBuildArchive), and only one of them had the bytes already in hand. Wiring one and not the
// other is precisely the class of mistake that produced three divergent copies in the first place.
func TestScanBuildDirAppliesTheSameNormalisation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	baseDir := filepath.Join(root, "base", "lib", "arm64-v8a")
	candDir := filepath.Join(root, "cand", "lib", "arm64-v8a")
	for _, d := range []string{baseDir, candDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(baseDir, "libdartjni.so"), readBuildIDFixture(t, "arm64-v8a", "a"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(candDir, "libdartjni.so"), readBuildIDFixture(t, "arm64-v8a", "b"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	diff, err := CompareBuildOutputs(filepath.Join(root, "base"), filepath.Join(root, "cand"))
	if err != nil {
		t.Fatalf("CompareBuildOutputs: %v", err)
	}
	if len(diff.ChangedNativeLibs) != 0 {
		t.Fatalf("scanBuildDir still reports build-id-only drift: %v", diff.ChangedNativeLibs)
	}
}
