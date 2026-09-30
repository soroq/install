// Package fontglyphs answers one question about a TrueType/OpenType font: which Unicode code points does
// it draw? It reads the font's character map ('cmap') and nothing else.
//
// Soroq store builds tree-shake icon fonts like any Flutter release build: the shipped MaterialIcons font
// keeps only the glyphs the app referenced. A patch cannot deliver a font, so before a patch is accepted
// the CLI checks that every glyph the patched app would draw is already in the store build's font. This
// package is that check's ground truth -- the font bytes the device actually has.
package fontglyphs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// Codepoints returns every code point the font's Unicode character maps assign a glyph (other than
// glyph 0, .notdef, which is what a missing character renders as).
func Codepoints(font []byte) (map[rune]bool, error) {
	if len(font) < 12 {
		return nil, errors.New("font too short for an sfnt header")
	}
	numTables := int(binary.BigEndian.Uint16(font[4:6]))
	var cmap []byte
	for i := 0; i < numTables; i++ {
		rec := 12 + 16*i
		if rec+16 > len(font) {
			return nil, errors.New("font table directory is truncated")
		}
		if string(font[rec:rec+4]) != "cmap" {
			continue
		}
		off := int(binary.BigEndian.Uint32(font[rec+8 : rec+12]))
		length := int(binary.BigEndian.Uint32(font[rec+12 : rec+16]))
		if off < 0 || length < 0 || off+length > len(font) {
			return nil, errors.New("cmap table lies outside the font")
		}
		cmap = font[off : off+length]
	}
	if cmap == nil {
		return nil, errors.New("font has no cmap table")
	}
	if len(cmap) < 4 {
		return nil, errors.New("cmap table is truncated")
	}
	out := map[rune]bool{}
	unicodeTables := 0
	n := int(binary.BigEndian.Uint16(cmap[2:4]))
	for i := 0; i < n; i++ {
		rec := 4 + 8*i
		if rec+8 > len(cmap) {
			return nil, errors.New("cmap encoding records are truncated")
		}
		platform := binary.BigEndian.Uint16(cmap[rec : rec+2])
		encoding := binary.BigEndian.Uint16(cmap[rec+2 : rec+4])
		if !(platform == 0 || (platform == 3 && (encoding == 1 || encoding == 10))) {
			continue // not a Unicode map
		}
		off := int(binary.BigEndian.Uint32(cmap[rec+4 : rec+8]))
		if off+2 > len(cmap) {
			return nil, errors.New("cmap subtable offset lies outside the table")
		}
		if err := readSubtable(cmap[off:], out); err != nil {
			return nil, fmt.Errorf("cmap subtable (platform %d, encoding %d): %w", platform, encoding, err)
		}
		unicodeTables++
	}
	if unicodeTables == 0 {
		return nil, errors.New("font has no Unicode character map")
	}
	return out, nil
}

func readSubtable(t []byte, out map[rune]bool) error {
	switch format := binary.BigEndian.Uint16(t[0:2]); format {
	case 0: // byte encoding table
		if len(t) < 6+256 {
			return errors.New("format 0 subtable is truncated")
		}
		for c := 0; c < 256; c++ {
			if t[6+c] != 0 {
				out[rune(c)] = true
			}
		}
	case 4: // segment mapping to delta values
		if len(t) < 14 {
			return errors.New("format 4 subtable is truncated")
		}
		segs := int(binary.BigEndian.Uint16(t[6:8])) / 2
		endAt, startAt := 14, 16+2*segs
		deltaAt, rangeAt := startAt+2*segs, startAt+4*segs
		if rangeAt+2*segs > len(t) {
			return errors.New("format 4 segment arrays are truncated")
		}
		for s := 0; s < segs; s++ {
			end := int(binary.BigEndian.Uint16(t[endAt+2*s:]))
			start := int(binary.BigEndian.Uint16(t[startAt+2*s:]))
			delta := int(binary.BigEndian.Uint16(t[deltaAt+2*s:]))
			rangeOffset := int(binary.BigEndian.Uint16(t[rangeAt+2*s:]))
			for c := start; c <= end && c != 0xFFFF; c++ {
				glyph := 0
				if rangeOffset == 0 {
					glyph = (c + delta) & 0xFFFF
				} else {
					at := rangeAt + 2*s + rangeOffset + 2*(c-start)
					if at+2 > len(t) {
						return errors.New("format 4 glyph index lies outside the subtable")
					}
					if g := int(binary.BigEndian.Uint16(t[at:])); g != 0 {
						glyph = (g + delta) & 0xFFFF
					}
				}
				if glyph != 0 {
					out[rune(c)] = true
				}
			}
		}
	case 6: // trimmed table mapping
		if len(t) < 10 {
			return errors.New("format 6 subtable is truncated")
		}
		first := int(binary.BigEndian.Uint16(t[6:8]))
		count := int(binary.BigEndian.Uint16(t[8:10]))
		if 10+2*count > len(t) {
			return errors.New("format 6 glyph array is truncated")
		}
		for i := 0; i < count; i++ {
			if binary.BigEndian.Uint16(t[10+2*i:]) != 0 {
				out[rune(first+i)] = true
			}
		}
	case 12, 13: // segmented coverage / many-to-one range mappings
		if len(t) < 16 {
			return fmt.Errorf("format %d subtable is truncated", format)
		}
		groups := int(binary.BigEndian.Uint32(t[12:16]))
		if 16+12*groups > len(t) {
			return fmt.Errorf("format %d groups are truncated", format)
		}
		for g := 0; g < groups; g++ {
			at := 16 + 12*g
			start := binary.BigEndian.Uint32(t[at:])
			end := binary.BigEndian.Uint32(t[at+4:])
			glyph := binary.BigEndian.Uint32(t[at+8:])
			if end < start || end > 0x10FFFF {
				return fmt.Errorf("format %d group %d has an invalid range", format, g)
			}
			for c := start; c <= end; c++ {
				// Format 12 assigns glyph+offset; format 13 assigns the same glyph to the whole range.
				assigned := glyph
				if format == 12 {
					assigned = glyph + (c - start)
				}
				if assigned != 0 {
					out[rune(c)] = true
				}
			}
		}
	default:
		// Formats 2, 8, 10 and 14 do not occur in the icon fonts Flutter builds or subsets. Refuse
		// rather than report a partial map: a missed code point would let a blank icon through.
		return fmt.Errorf("unsupported cmap subtable format %d", format)
	}
	return nil
}

// Missing returns the code points of candidate that base does not draw, sorted.
func Missing(base, candidate map[rune]bool) []rune {
	var out []rune
	for c := range candidate {
		if !base[c] {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// FormatCodepoints renders code points as "U+E145, U+F04B" for messages, at most limit of them.
func FormatCodepoints(cps []rune, limit int) string {
	s := ""
	for i, c := range cps {
		if i == limit {
			return s + fmt.Sprintf(" and %d more", len(cps)-limit)
		}
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("U+%04X", c)
	}
	return s
}
