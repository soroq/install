package fontglyphs

import (
	"encoding/binary"
	"reflect"
	"testing"
)

// sfnt wraps one cmap table (with the given subtables, each tagged platform/encoding) into a minimal font.
func sfnt(subtables ...[]byte) []byte {
	type rec struct {
		platform, encoding uint16
		body               []byte
	}
	var recs []rec
	for _, s := range subtables {
		recs = append(recs, rec{binary.BigEndian.Uint16(s[0:2]), binary.BigEndian.Uint16(s[2:4]), s[4:]})
	}
	cmap := make([]byte, 4+8*len(recs))
	binary.BigEndian.PutUint16(cmap[2:], uint16(len(recs)))
	for i, r := range recs {
		binary.BigEndian.PutUint16(cmap[4+8*i:], r.platform)
		binary.BigEndian.PutUint16(cmap[6+8*i:], r.encoding)
		binary.BigEndian.PutUint32(cmap[8+8*i:], uint32(len(cmap)))
		cmap = append(cmap, r.body...)
	}
	font := make([]byte, 12+16)
	binary.BigEndian.PutUint32(font[0:], 0x00010000)
	binary.BigEndian.PutUint16(font[4:], 1)
	copy(font[12:], "cmap")
	binary.BigEndian.PutUint32(font[20:], uint32(len(font)))
	binary.BigEndian.PutUint32(font[24:], uint32(len(cmap)))
	return append(font, cmap...)
}

func u16(v ...int) []byte {
	b := make([]byte, 2*len(v))
	for i, x := range v {
		binary.BigEndian.PutUint16(b[2*i:], uint16(x))
	}
	return b
}

func u32(v ...uint32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.BigEndian.PutUint32(b[4*i:], x)
	}
	return b
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// format4 with two real segments: 0xE000-0xE002 by delta (glyphs 1..3), and 0xE145-0xE146 through the
// glyph index array, where 0xE146 maps to glyph 0 (not drawn); plus the mandatory 0xFFFF sentinel.
func format4() []byte {
	segs := 3
	head := u16(3, 1) // platform 3, encoding 1
	body := cat(
		u16(4, 0, 0, 2*segs, 0, 0, 0),
		u16(0xE002, 0xE146, 0xFFFF),  // endCode
		u16(0),                       // reservedPad
		u16(0xE000, 0xE145, 0xFFFF),  // startCode
		u16((1-0xE000)&0xFFFF, 0, 1), // idDelta
		u16(0, 4, 0),                 // idRangeOffset: segment 1 points 4 bytes ahead, at glyphIdArray[0]
		u16(7, 0),                    // glyphIdArray: 0xE145 -> 7, 0xE146 -> 0
	)
	binary.BigEndian.PutUint16(body[2:], uint16(len(body)))
	return cat(head, body)
}

func format12() []byte {
	head := u16(0, 4) // platform 0 (Unicode), encoding 4
	body := cat(u16(12, 0), u32(0, 0, 2), u32(0xF0000, 0xF0001, 10), u32(0x1F600, 0x1F600, 20))
	binary.BigEndian.PutUint32(body[4:], uint32(len(body)))
	return cat(head, body)
}

func TestCodepointsReadsFormat4DeltasRangeOffsetsAndSkipsNotdef(t *testing.T) {
	got, err := Codepoints(sfnt(format4()))
	if err != nil {
		t.Fatal(err)
	}
	want := map[rune]bool{0xE000: true, 0xE001: true, 0xE002: true, 0xE145: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCodepointsUnitesEveryUnicodeSubtable(t *testing.T) {
	got, err := Codepoints(sfnt(format4(), format12()))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []rune{0xE000, 0xE145, 0xF0000, 0xF0001, 0x1F600} {
		if !got[c] {
			t.Errorf("U+%04X missing from %v", c, got)
		}
	}
	if got[0xE146] {
		t.Error("a code point mapped to glyph 0 must not count as drawn")
	}
}

func TestCodepointsRefusesWhatItCannotFullyRead(t *testing.T) {
	for name, font := range map[string][]byte{
		"empty":        nil,
		"no cmap":      func() []byte { f := sfnt(format4()); copy(f[12:], "glyf"); return f }(),
		"format 14":    sfnt(cat(u16(0, 5), u16(14, 0), u32(10, 0))),
		"no unicode":   sfnt(cat(u16(1, 0), u16(6, 0, 0, 0, 0))),
		"truncated f4": sfnt(format4()[:20]),
	} {
		if _, err := Codepoints(font); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestMissingAndFormat(t *testing.T) {
	base := map[rune]bool{0xE000: true, 0xE001: true}
	cand := map[rune]bool{0xE001: true, 0xF04B: true, 0xE145: true}
	missing := Missing(base, cand)
	if !reflect.DeepEqual(missing, []rune{0xE145, 0xF04B}) {
		t.Fatalf("missing = %v", missing)
	}
	if got := FormatCodepoints(missing, 1); got != "U+E145 and 1 more" {
		t.Fatalf("format = %q", got)
	}
	if len(Missing(cand, map[rune]bool{0xE001: true})) != 0 {
		t.Fatal("a candidate that draws fewer glyphs is missing nothing")
	}
}
