package main

import (
	"archive/zip"
	"bytes"
	"debug/elf"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE GATE USERS ACTUALLY HIT.
//
// The GNU build-id normalisation has now been fixed four times, in four different packages, and each
// of the first three was verified only in the package it lived in. Every one of those suites was
// green while `soroq patch android --code` still refused a deliverable patch, because the refusal
// came from the NEXT layer up.
//
// So this test deliberately sits at the command boundary — assertAndroidDependencyDeliverable, called
// from runPatchAndroid — rather than one layer below it. It is the only place where "the user's
// command accepts this" is the thing being asserted.

const gateBuildIDFixtureRoot = "../../internal/nativeelf/testdata/native_build_id"

func gateBuildIDFixture(t *testing.T, abi string, variant string) []byte {
	t.Helper()

	path := filepath.Join(gateBuildIDFixtureRoot, abi, "buildid_"+variant+".so")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %q: %v", path, err)
	}
	return raw
}

// writeGateAPK produces a minimal APK-shaped zip: a native library plus the Dart payload a code
// patch legitimately replaces, so the gate sees a realistic delta rather than a lone .so.
func writeGateAPK(t *testing.T, path string, nativeLib []byte, dartPayload []byte) string {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, data := range map[string][]byte{
		"lib/arm64-v8a/libdartjni.so": nativeLib,
		"lib/arm64-v8a/libapp.so":     dartPayload,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return path
}

// A build-id-only difference in a native library must NOT refuse the patch at the command boundary.
//
// This is the exact shape that blocked A3: libdartjni.so arrives with any consumer of path_provider,
// and base and candidate are compiled at different paths, so their build-ids differ and nothing else
// does.
func TestAssertAndroidDependencyDeliverableAcceptsBuildIDOnlyNativeDrift(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	baseLib := gateBuildIDFixture(t, "arm64-v8a", "a")
	candLib := gateBuildIDFixture(t, "arm64-v8a", "b")
	if bytes.Equal(baseLib, candLib) {
		t.Fatal("fixtures are identical; this test would prove nothing")
	}

	base := writeGateAPK(t, filepath.Join(dir, "base.apk"), baseLib, []byte("dart payload v1"))
	cand := writeGateAPK(t, filepath.Join(dir, "cand.apk"), candLib, []byte("dart payload v2"))

	// projectDir is deliberately a directory with no pubspec.lock: attribution is a diagnostic, and a
	// project that was never pub-resolved must not lose the artifact gate. That keeps this test about
	// the native comparison and nothing else.
	if _, err := assertAndroidDependencyDeliverable(dir, base, cand); err != nil {
		t.Fatalf("the command refused a build-id-only native difference: %v\n\n"+
			"This is the defect that was declared fixed three times while this gate still refused.", err)
	}
}

// CONTROL. Real native code drift must still be refused BY NAME at this boundary.
func TestAssertAndroidDependencyDeliverableStillRefusesRealNativeDrift(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	baseLib := gateBuildIDFixture(t, "arm64-v8a", "a")

	file, err := elf.NewFile(bytes.NewReader(baseLib))
	if err != nil {
		t.Fatalf("parse ELF: %v", err)
	}
	text := file.Section(".text")
	if text == nil {
		t.Fatal(".text not found")
	}
	candLib := make([]byte, len(baseLib))
	copy(candLib, baseLib)
	candLib[text.Offset+text.Size/2] ^= 0xff

	base := writeGateAPK(t, filepath.Join(dir, "base.apk"), baseLib, []byte("dart payload v1"))
	cand := writeGateAPK(t, filepath.Join(dir, "cand.apk"), candLib, []byte("dart payload v2"))

	_, err = assertAndroidDependencyDeliverable(dir, base, cand)
	if err == nil {
		t.Fatal("a real .text change was ACCEPTED at the command boundary; the normalisation is " +
			"masking genuine native drift and would ship an undeliverable patch")
	}
	// The refusal has to name the file, or an operator cannot act on it.
	if !strings.Contains(err.Error(), "libdartjni.so") {
		t.Fatalf("the refusal does not name the offending library: %v", err)
	}
}
