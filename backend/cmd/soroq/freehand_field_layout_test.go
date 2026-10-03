package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const genSnapshotFieldLayout = "# soroq.field_layout.v1\n" +
	"package:app/main.dart::Counter::\t303\t0\tclass\t0\t1\t0\n" +
	"package:app/main.dart::Counter::_n\t303\t8\ti64\t0\t1\t0\n" +
	"dart:ui::Offset::_dx\t120\t8\tf64\t0\t0\t0\n" +
	"package:app/main.dart::Counter::_label\t303\t16\ttagged\t1\t1\t0\n" +
	"@const\t77\t303\tC303{8=d4010000000000000}\n" +
	"@const\t77\t303\tC303{8=d4010000000000000}\n" +
	"# constants=2\n" +
	"# written=4\n"

func TestFieldLayoutCanonicalForm(t *testing.T) {
	m, err := parseFreehandFieldLayout([]byte(genSnapshotFieldLayout))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Slots) != 4 || len(m.Consts) != 1 || !m.Slots["package:app/main.dart::Counter::"].Named || m.Slots["dart:ui::Offset::_dx"] != (freehandFieldSlot{ClassID: 120, Offset: 8, Rep: "f64"}) || !m.Slots["package:app/main.dart::Counter::_label"].Late ||
		!m.Slots["package:app/main.dart::Counter::_n"].Named {
		t.Fatalf("parsed %v", m)
	}
	canon := renderFreehandFieldLayout(m)
	want := "# soroq.field_layout.v1\n" +
		"dart:ui::Offset::_dx\t120\t8\tf64\t0\t0\t0\n" +
		"package:app/main.dart::Counter::\t303\t0\tclass\t0\t1\t0\n" +
		"package:app/main.dart::Counter::_label\t303\t16\ttagged\t1\t1\t0\n" +
		"package:app/main.dart::Counter::_n\t303\t8\ti64\t0\t1\t0\n" +
		"@const\t77\t303\tC303{8=d4010000000000000}\n"
	if string(canon) != want {
		t.Fatalf("canonical form:\n%s\nwant:\n%s", canon, want)
	}
	again, err := parseFreehandFieldLayout(canon)
	if err != nil || !bytes.Equal(renderFreehandFieldLayout(again), canon) {
		t.Fatalf("canonical form does not round-trip: %v", err)
	}
}

func TestFieldLayoutRefusesMalformed(t *testing.T) {
	for name, raw := range map[string]string{
		"short line":        "a::B::c\t1\t8\ttagged\t0\t1\n",
		"no class":          "a::c\t1\t8\ttagged\t0\t1\t0\n",
		"zero cid":          "a::B::c\t0\t8\ttagged\t0\t1\t0\n",
		"unknown rep":       "a::B::c\t1\t8\tsimd\t0\t1\t0\n",
		"bad named":         "a::B::c\t1\t8\ttagged\t0\t2\t0\n",
		"class row offset":  "a::B::\t1\t8\tclass\t0\t1\t0\n",
		"field as class":    "a::B::c\t1\t0\tclass\t0\t1\t0\n",
		"class as field":    "a::B::\t1\t8\ttagged\t0\t1\t0\n",
		"const bad path":    "@const\tx.1\t3\tC3{}\n",
		"const bad cid":     "@const\t1\t0\tC3{}\n",
		"const short":       "@const\t1\t3\n",
		"conflicting slots": "a::B::c\t1\t8\ttagged\t0\t1\na::B::c\t1\t16\ttagged\t0\t1\t0\n",
		"after trailer":     "# written=1\na::B::c\t1\t8\ttagged\t0\t1\t0\n",
		"trailer short":     "a::B::c\t1\t8\ttagged\t0\t1\na::B::d\t1\t16\ttagged\t0\t1\n# written=1\n",
	} {
		if _, err := parseFreehandFieldLayout([]byte(raw)); err == nil {
			t.Errorf("%s: accepted %q", name, raw)
		}
	}
}

func TestCollectFieldLayoutRequiresThisBuildsFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, freehandFieldLayoutFile)
	if _, err := collectFreehandFieldLayout(p, time.Now()); err == nil {
		t.Fatal("collected a missing file")
	}
	if err := os.WriteFile(p, []byte(genSnapshotFieldLayout), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := collectFreehandFieldLayout(p, time.Now()); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("adopted a stale file: %v", err)
	}
	if err := os.Chtimes(p, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	canon, err := collectFreehandFieldLayout(p, time.Now().Add(-time.Second))
	if err != nil || !bytes.HasPrefix(canon, []byte("# soroq.field_layout.v1\ndart:ui::Offset::_dx")) {
		t.Fatalf("collect: %v\n%s", err, canon)
	}
}

func TestBindFieldLayoutBothDirections(t *testing.T) {
	m, _ := parseFreehandFieldLayout([]byte(genSnapshotFieldLayout))
	canon := renderFreehandFieldLayout(m)
	with := &FreehandRedirectCapabilities{IdentityCapabilities: []string{freehandFieldLayoutCapability}}
	without := &FreehandRedirectCapabilities{}

	var meta FreehandBaselineMeta
	if err := bindFreehandFieldLayout(&meta, with, nil); err == nil {
		t.Fatal("an engine declaring the capability persisted a baseline without its layout")
	}
	if err := bindFreehandFieldLayout(&meta, without, canon); err == nil {
		t.Fatal("a layout was persisted into a baseline whose engine does not declare it")
	}
	if err := bindFreehandFieldLayout(&meta, with, []byte(genSnapshotFieldLayout)); err == nil {
		t.Fatal("a non-canonical layout was persisted")
	}
	if err := bindFreehandFieldLayout(&meta, with, canon); err != nil {
		t.Fatal(err)
	}
	if meta.FieldLayoutSchema != freehandFieldLayoutSchema || meta.FieldLayoutEntries != 5 || meta.FieldLayoutSHA256 != freehandSHA256Bytes(canon) {
		t.Fatalf("bound %+v", meta)
	}
	if err := bindFreehandFieldLayout(&meta, without, nil); err != nil || meta.FieldLayoutSHA256 != "" {
		t.Fatalf("an older engine's baseline must stay without a layout: %v %+v", err, meta)
	}
}

func TestWithFieldReadFallbackPassesTheLayout(t *testing.T) {
	defer func(p string, f bool) { freehandFieldLayoutPath, freehandFieldReadFallback = p, f }(freehandFieldLayoutPath, freehandFieldReadFallback)
	freehandFieldLayoutPath, freehandFieldReadFallback = "/r/soroq_field_layout.tsv", true
	if got := strings.Join(withFieldReadFallback([]string{"x"}), " "); got != "x --field-layout /r/soroq_field_layout.tsv" {
		t.Fatalf("got %q", got)
	}
	freehandFieldLayoutPath, freehandFieldReadFallback = "", false
	if got := withFieldReadFallback([]string{"x"}); len(got) != 1 {
		t.Fatalf("an R10 base got analyzer flags: %v", got)
	}
}

func TestFieldLayoutStaticFields(t *testing.T) {
	raw := "# soroq.field_layout.v1\n" +
		"package:app/main.dart::Counter::_n\t303\t8\ti64\t0\t1\t0\n" +
		"@static\tpackage:flutter/fw.dart::::boxes\t854\t1201\t10\n" +
		"@static\tpackage:app/main.dart::Counter::count\t12\t-1\t0\n" +
		"# statics=2\n" +
		"# written=1\n"
	m, err := parseFreehandFieldLayout([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !m.StaticsRecorded || len(m.Statics) != 2 ||
		m.Statics["package:flutter/fw.dart::::boxes"] != (freehandStaticField{FieldID: 854, PoolIndex: 1201, Flags: 10}) ||
		m.entries() != 3 {
		t.Fatalf("parsed %+v", m)
	}
	canon := renderFreehandFieldLayout(m)
	want := "# soroq.field_layout.v1\n" +
		"package:app/main.dart::Counter::_n\t303\t8\ti64\t0\t1\t0\n" +
		"@static\tpackage:app/main.dart::Counter::count\t12\t-1\t0\n" +
		"@static\tpackage:flutter/fw.dart::::boxes\t854\t1201\t10\n" +
		"# statics=2\n"
	if string(canon) != want {
		t.Fatalf("canonical form:\n%s\nwant:\n%s", canon, want)
	}
	again, err := parseFreehandFieldLayout(canon)
	if err != nil || !again.StaticsRecorded || !bytes.Equal(renderFreehandFieldLayout(again), canon) {
		t.Fatalf("canonical form does not round-trip: %v", err)
	}
	// An engine that records statics but found none still says so: absence then means "no base storage".
	none, err := parseFreehandFieldLayout([]byte("# soroq.field_layout.v1\na::B::c\t1\t8\ttagged\t0\t1\t0\n# statics=0\n"))
	if err != nil || !none.StaticsRecorded || !strings.Contains(string(renderFreehandFieldLayout(none)), "# statics=0\n") {
		t.Fatalf("an empty statics record must survive canonicalization: %v", err)
	}
	for name, bad := range map[string]string{
		"short":       "@static\ta::B::c\t1\t-1\n# statics=1\n",
		"no class":    "@static\ta::c\t1\t-1\t0\n# statics=1\n",
		"pool":        "@static\ta::B::c\t1\t-2\t0\n# statics=1\n",
		"flags":       "@static\ta::B::c\t1\t-1\t16\n# statics=1\n",
		"count":       "@static\ta::B::c\t1\t-1\t0\n# statics=2\n",
		"no count":    "@static\ta::B::c\t1\t-1\t0\n",
		"conflicting": "@static\ta::B::c\t1\t-1\t0\n@static\ta::B::c\t2\t-1\t0\n# statics=1\n",
	} {
		if _, err := parseFreehandFieldLayout([]byte(bad)); err == nil {
			t.Errorf("%s: accepted %q", name, bad)
		}
	}
}
