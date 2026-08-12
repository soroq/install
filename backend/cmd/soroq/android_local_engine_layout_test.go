package main

// THE LAYOUT HALF of the Android debug-symbol fix.
//
// Requiring libflutter_unstripped.so in the toolchain is not enough on its own: the flat packed bundle
// is expanded into the `out/` engine-source tree the local-engine build consumes, and that expansion
// used to copy the STRIPPED library into every position — including the Gradle embedding jar, which is
// what AGP merges, strips and extracts libflutter.so.sym from. That is how three byte-identical
// pre-stripped copies reached the installed toolchain and why every release AAB was rejected.
//
// So the symbol-bearing library must reach two specific places: out/<target>/libflutter.so and the
// embedding jar. lib.stripped/ keeps the stripped copy.

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func writeAndroidFlatBundle(t *testing.T, dir string, withUnstripped bool) {
	t.Helper()
	mustWriteFile(t, filepath.Join(dir, "libflutter.so"), "STRIPPED-LIBFLUTTER")
	mustWriteFile(t, filepath.Join(dir, "gen_snapshot"), "GEN-SNAPSHOT")
	// A real (empty) zip — the layout step reads it as a jar.
	jar, err := os.Create(filepath.Join(dir, "flutter_embedding_release.jar"))
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(jar)
	w, err := zw.Create("io/flutter/embedding/engine/loader/FlutterLoader.class")
	if err != nil {
		t.Fatal(err)
	}
	// The layout step also asserts this is the SOROQ override embedding, not a stock one.
	if _, err := w.Write([]byte("fake class setSoroqStagedAssetBundlePath")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	jar.Close()
	if withUnstripped {
		mustWriteFile(t, filepath.Join(dir, "libflutter_unstripped.so"), "UNSTRIPPED-LIBFLUTTER-WITH-SYMBOLS")
	}
}

func TestUnstrippedLibraryReachesTheAGPReadPositions(t *testing.T) {
	bundle := t.TempDir()
	writeAndroidFlatBundle(t, bundle, true)

	if err := materializeAndroidLocalEngineLayout(bundle); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	out := filepath.Join(bundle, "out", androidLocalEngineTargetName)

	// out/<target>/libflutter.so is the unstripped position AGP extracts debug metadata from.
	got, err := os.ReadFile(filepath.Join(out, "libflutter.so"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "UNSTRIPPED-LIBFLUTTER-WITH-SYMBOLS" {
		t.Errorf("out/%s/libflutter.so carries %q; AGP would have no symbols to extract",
			androidLocalEngineTargetName, got)
	}

	// lib.stripped/ keeps the stripped device runtime copy.
	stripped, err := os.ReadFile(filepath.Join(out, "lib.stripped", "libflutter.so"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stripped) != "STRIPPED-LIBFLUTTER" {
		t.Errorf("lib.stripped/libflutter.so carries %q, want the stripped copy", stripped)
	}

	// THE ONE THAT ACTUALLY DECIDES IT: the embedding jar Gradle resolves.
	jarPath := filepath.Join(out, androidLocalEngineABI+"_release.jar")
	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		t.Fatalf("open embedding jar: %v", err)
	}
	defer zr.Close()
	found := false
	for _, f := range zr.File {
		if filepath.Base(f.Name) != "libflutter.so" {
			continue
		}
		found = true
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		n, _ := rc.Read(buf)
		rc.Close()
		if string(buf[:n]) != "UNSTRIPPED-LIBFLUTTER-WITH-SYMBOLS" {
			t.Errorf("the embedding jar carries %q; AGP merges THIS library, so a stripped copy here "+
				"means no libflutter.so.sym and a rejected AAB", buf[:n])
		}
	}
	if !found {
		t.Fatalf("embedding jar %s contains no libflutter.so", jarPath)
	}
}

// The three positions must not all be identical — that is the published defect, restated as a layout
// property so it cannot silently return.
func TestLayoutPositionsAreNotAllIdentical(t *testing.T) {
	bundle := t.TempDir()
	writeAndroidFlatBundle(t, bundle, true)
	if err := materializeAndroidLocalEngineLayout(bundle); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(bundle, "out", androidLocalEngineTargetName)
	unstripped, _ := os.ReadFile(filepath.Join(out, "libflutter.so"))
	stripped, _ := os.ReadFile(filepath.Join(out, "lib.stripped", "libflutter.so"))
	if string(unstripped) == string(stripped) {
		t.Error("out/libflutter.so and lib.stripped/libflutter.so are identical — the published defect")
	}
}

// BACKWARD COMPATIBILITY: an old toolchain with no unstripped library must still lay out (its release
// build fails at Flutter's verification, which is the toolchain's fault, not a reason to break every
// other Android flow) — and must say so.
func TestOldToolchainWithoutUnstrippedStillLaysOut(t *testing.T) {
	bundle := t.TempDir()
	writeAndroidFlatBundle(t, bundle, false)

	if err := materializeAndroidLocalEngineLayout(bundle); err != nil {
		t.Fatalf("an old toolchain must still lay out, got: %v", err)
	}
	out := filepath.Join(bundle, "out", androidLocalEngineTargetName)
	got, err := os.ReadFile(filepath.Join(out, "libflutter.so"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "STRIPPED-LIBFLUTTER" {
		t.Errorf("fallback did not use the only library available: %q", got)
	}
}
