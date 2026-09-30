package depgraph

import "testing"

func TestDiffBuildOutputsJudgesFontsByTheGlyphsTheyDraw(t *testing.T) {
	font := func(glyphs ...rune) OutputEntry {
		g := map[rune]bool{}
		for _, c := range glyphs {
			g[c] = true
		}
		return OutputEntry{Category: CatAsset, SHA256: string(glyphs), Glyphs: g}
	}
	const path = "base/assets/flutter_assets/fonts/MaterialIcons-Regular.otf"
	base := map[string]OutputEntry{path: font(0xE09D, 0xE16A)}

	if d := DiffBuildOutputs(base, map[string]OutputEntry{path: font(0xE16A)}); d.HasNativeOrAssetDrift() {
		t.Fatalf("a font drawing a subset of the base's glyphs is not drift: %+v", d)
	}
	if d := DiffBuildOutputs(base, map[string]OutputEntry{path: font(0xE16A, 0xF04B)}); len(d.ChangedAssets) != 1 {
		t.Fatalf("a font drawing a new glyph is drift: %+v", d)
	}
	unreadable := OutputEntry{Category: CatAsset, SHA256: "other"}
	if d := DiffBuildOutputs(base, map[string]OutputEntry{path: unreadable}); len(d.ChangedAssets) != 1 {
		t.Fatalf("a font that cannot be read falls back to byte equality: %+v", d)
	}
}
