package main

import (
	"strings"
	"testing"
)

func TestCodeFingerprints_ParseRenderCanonical(t *testing.T) {
	raw := "# soroq.code_fingerprints.v1\npackage:a/a.dart::B::m\t0123456789abcdef0123456789abcdef\npackage:a/a.dart::::f\tffffffffffffffffffffffffffffffff\n# written=2 unhashable=0\n"
	m, err := parseFreehandCodeFingerprints([]byte(raw))
	if err != nil || len(m) != 2 {
		t.Fatalf("parse: %v %v", m, err)
	}
	want := "# soroq.code_fingerprints.v1\npackage:a/a.dart::::f\tffffffffffffffffffffffffffffffff\npackage:a/a.dart::B::m\t0123456789abcdef0123456789abcdef\n"
	if got := string(renderFreehandCodeFingerprints(m)); got != want {
		t.Fatalf("render:\n%s\nwant:\n%s", got, want)
	}
	for name, bad := range map[string]string{
		"short hash": "x::y::z\tabc\n",
		"no tab":     "x::y::z 0123456789abcdef0123456789abcdef\n",
		"duplicate":  "x::y::z\t0123456789abcdef0123456789abcdef\nx::y::z\t0123456789abcdef0123456789abcdef\n",
		"upper hex":  "x::y::z\t0123456789ABCDEF0123456789abcdef\n",
	} {
		if _, err := parseFreehandCodeFingerprints([]byte(bad)); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestCodeFingerprints_PruneOnlyWhatIsEqualOnBothSides(t *testing.T) {
	h := func(c string) string { return strings.Repeat(c, 32) }
	base := map[string]string{"L::::loop": h("a"), "L::Rect::area": h("b"), "L::Circle::area": h("c"), "L::Vec::plus": h("d")}
	cand := map[string]string{"L::::loop": h("a"), "L::Rect::area": h("b"), "L::Circle::area": h("e")}
	got := freehandSameMachineCode([]string{"L::::loop", "L::Rect::area", "L::Circle::area", "L::Vec::plus", "L::New::f"}, base, cand)
	if len(got) != 2 || !got["L::::loop"] || !got["L::Rect::area"] {
		t.Fatalf("pruned %v; want only the loop and Rect.area (Circle differs, Vec.plus unhashable on the candidate, New absent)", got)
	}
}

func TestCodeFingerprints_CandidateBuildDropsObfuscation(t *testing.T) {
	got := stripObfuscationArgs([]string{"--obfuscate", "--split-debug-info=build/sym", "--dart-define=A=1",
		"--extra-gen-snapshot-options=--save-obfuscation-map=x", "--flavor", "prod"})
	if strings.Join(got, " ") != "--dart-define=A=1 --flavor prod" {
		t.Fatalf("stripObfuscationArgs = %v", got)
	}
}
