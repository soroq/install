package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testIconFont builds a minimal font whose cmap (format 12) draws exactly the given code points.
func testIconFont(cps ...rune) []byte {
	be16 := func(v int) []byte { return []byte{byte(v >> 8), byte(v)} }
	be32 := func(v uint32) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }
	sub := append(append(be16(12), be16(0)...), be32(uint32(16+12*len(cps)))...)
	sub = append(append(sub, be32(0)...), be32(uint32(len(cps)))...)
	for i, c := range cps {
		sub = append(append(append(sub, be32(uint32(c))...), be32(uint32(c))...), be32(uint32(i+1))...)
	}
	cmap := append(append(append(be16(0), be16(1)...), append(be16(3), be16(10)...)...), be32(12)...)
	cmap = append(cmap, sub...)
	font := append(append(be32(0x00010000), be16(1)...), make([]byte, 6)...)
	font = append(append(font, []byte("cmap")...), be32(0)...)
	font = append(append(font, be32(28)...), be32(uint32(len(cmap)))...)
	return append(font, cmap...)
}

// writeBuiltIOSApp lays out build/ios/iphoneos/Runner.app with the given fonts, as a candidate build does.
func writeBuiltIOSApp(t *testing.T, projectDir string, fonts map[string][]byte) {
	t.Helper()
	fw := filepath.Join(projectDir, "build", "ios", "iphoneos", "Runner.app", "Frameworks", "App.framework")
	assets := filepath.Join(fw, "flutter_assets")
	os.RemoveAll(assets)
	for rel, b := range fonts {
		p := filepath.Join(assets, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fw, "App"), []byte("dart code"), 0o755); err != nil {
		t.Fatal(err)
	}
}

const iconFontRel = "fonts/MaterialIcons-Regular.otf"

func TestMissingIconGlyphs(t *testing.T) {
	base := map[string][]byte{iconFontRel: testIconFont(0xE09D, 0xE16A)}
	if m, err := missingIconGlyphs(base, map[string][]byte{iconFontRel: testIconFont(0xE09D)}); err != nil || len(m) != 0 {
		t.Fatalf("a dropped icon needs nothing new: %v, %v", m, err)
	}
	m, err := missingIconGlyphs(base, map[string][]byte{iconFontRel: testIconFont(0xE09D, 0xF04B)})
	if err != nil || len(m[iconFontRel]) != 1 || m[iconFontRel][0] != 0xF04B {
		t.Fatalf("a new icon must be missing: %v, %v", m, err)
	}
	m, err = missingIconGlyphs(base, map[string][]byte{"packages/cupertino_icons/assets/CupertinoIcons.ttf": testIconFont(0xF4D2)})
	if err != nil || len(m) != 1 {
		t.Fatalf("a font the store build does not ship is missing every glyph: %v, %v", m, err)
	}
	if _, err := missingIconGlyphs(base, map[string][]byte{iconFontRel: []byte("not a font")}); err == nil {
		t.Fatal("an unreadable font must fail the check, not pass it")
	}
}

func TestKeepFreehandBaseIconFontsCopiesTheShippedFonts(t *testing.T) {
	project, relDir := t.TempDir(), t.TempDir()
	writeBuiltIOSApp(t, project, map[string][]byte{iconFontRel: testIconFont(0xE16A), "AssetManifest.bin": []byte("x")})
	if err := keepFreehandBaseIconFonts(project, relDir); err != nil {
		t.Fatal(err)
	}
	got, err := readFontFiles(filepath.Join(relDir, freehandBaseIconFontsDir))
	if err != nil || len(got) != 1 || got[iconFontRel] == nil {
		t.Fatalf("kept fonts = %v, %v", got, err)
	}
}

func TestAssertFreehandIconGlyphs(t *testing.T) {
	opts := func(built *int, fonts map[string][]byte) []freehandPlanOptions {
		return []freehandPlanOptions{{toolchain: "tc", buildCandidateApp: func(projectDir, _, _ string, _ []string) error {
			*built++
			writeBuiltIOSApp(t, projectDir, fonts)
			return nil
		}}}
	}

	// A baseline from before tree-shaken fonts: nothing to check, nothing built.
	project, relDir := t.TempDir(), t.TempDir()
	built := 0
	if err := assertFreehandIconGlyphs(project, relDir, time.Now(), opts(&built, nil)); err != nil || built != 0 {
		t.Fatalf("old baseline: err=%v built=%d", err, built)
	}

	// A tree-shaken baseline.
	writeBuiltIOSApp(t, project, map[string][]byte{iconFontRel: testIconFont(0xE09D, 0xE16A)})
	if err := keepFreehandBaseIconFonts(project, relDir); err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	time.Sleep(10 * time.Millisecond)

	// No candidate built in this plan yet: it is built, and a new icon is refused by code point.
	err := assertFreehandIconGlyphs(project, relDir, since, opts(&built, map[string][]byte{iconFontRel: testIconFont(0xE09D, 0xF04B)}))
	if err == nil || !strings.Contains(err.Error(), "U+F04B") || built != 1 {
		t.Fatalf("new icon: err=%v built=%d", err, built)
	}

	// The plan already built the candidate (the machine-code comparison did): it is not rebuilt, and a
	// candidate that only dropped an icon passes.
	writeBuiltIOSApp(t, project, map[string][]byte{iconFontRel: testIconFont(0xE16A)})
	if err := assertFreehandIconGlyphs(project, relDir, since, opts(&built, nil)); err != nil || built != 1 {
		t.Fatalf("dropped icon: err=%v built=%d", err, built)
	}

	// Without a toolchain the candidate cannot be built, so the check refuses rather than passes.
	if err := assertFreehandIconGlyphs(project, relDir, time.Now().Add(time.Hour), nil); err == nil {
		t.Fatal("a tree-shaken baseline cannot be checked without a candidate build")
	}
}
