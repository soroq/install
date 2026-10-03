package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FIELD LAYOUT (engine capability soroq_field_layout_v1, engine R11).
//
// The precompiler drops the getter of an instance field nothing in the base calls by name, so a patch that
// reads such a field (Offset.dx from a new call site, a private field of a framework State) has no member
// to call. Instead of carrying a replacement class (app size, State identity), gen_snapshot
// (--soroq_field_layout) records where every instance field lives: `lib::Class::field`, class id, byte
// offset, representation, late, and whether the class stays resolvable by name in the shipped binary
// (a patch may then use it as a type). The patch reads the field through dart:_internal.soroqLoadField, which
// checks the receiver's class id and the offset before loading. The file is build metadata only: the
// snapshot is byte-identical with or without it, so the app does not grow.
const (
	freehandFieldLayoutCapability = "soroq_field_layout_v1"
	freehandFieldLayoutFlag       = "--soroq_field_layout"
	freehandFieldLayoutFile       = "soroq_field_layout.tsv"
	freehandFieldLayoutSchema     = "soroq.field_layout.v1"
)

var (
	fieldLayoutRepRe     = regexp.MustCompile(`^(tagged|f64|i64|unboxed-other|class)$`)
	fieldLayoutWrittenRe = regexp.MustCompile(`^# written=([0-9]+)$`)
	fieldLayoutStaticsRe = regexp.MustCompile(`^# statics=([0-9]+)$`)
)

// freehandFieldLayout is the parsed file: instance-field/class rows, the base constants
// (`@const <pool index>[.<offset>]* <class id> <key>`) a patch's equal constant must resolve to, and the
// static fields the base keeps storage for (`@static <key> <field id> <pool index> <flags>`).
type freehandFieldLayout struct {
	Slots  map[string]freehandFieldSlot
	Consts map[string]bool // the canonical `@const` lines (path, class id, key), deduplicated
	// Statics: the engine recorded static fields (a `# statics=N` trailer). Only then does a static field
	// WITHOUT a row mean "the base keeps no storage for it" to the patch tooling.
	StaticsRecorded bool
	Statics         map[string]freehandStaticField
}

type freehandStaticField struct {
	FieldID   int
	PoolIndex int // -1: no Field object reachable (the slot holds the value, or a late field has no initializer)
	Flags     int // 1 shared, 2 final, 4 late, 8 nontrivial initializer
}

func (l freehandFieldLayout) entries() int { return len(l.Slots) + len(l.Consts) + len(l.Statics) }

var fieldLayoutConstPathRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
var fieldLayoutConstKeyRe = regexp.MustCompile(`^[A-Za-z0-9{}=,.\-]+$`)

type freehandFieldSlot struct {
	ClassID int
	Offset  int
	Rep     string
	Late    bool
	Named   bool
	// FieldObject: the Field object survives in the shipped binary (resolvable by name).
	FieldObject bool
}

// parseFreehandFieldLayout reads gen_snapshot's file or its canonical rendering. Comment lines start with
// '#'; a `# written=N` trailer (gen_snapshot's) must match the entry count. A field listed twice with
// different slots is refused; an identical repeat is folded.
func parseFreehandFieldLayout(raw []byte) (freehandFieldLayout, error) {
	out := map[string]freehandFieldSlot{}
	consts := map[string]bool{}
	statics := map[string]freehandStaticField{}
	staticsRecorded := -1
	layout := func() freehandFieldLayout {
		return freehandFieldLayout{Slots: out, Consts: consts, StaticsRecorded: staticsRecorded >= 0, Statics: statics}
	}
	written := -1
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "@const\t") {
			parts := strings.Split(line, "\t")
			if len(parts) != 4 || !fieldLayoutConstPathRe.MatchString(parts[1]) ||
				!fieldLayoutConstKeyRe.MatchString(parts[3]) {
				return freehandFieldLayout{}, fmt.Errorf("field layout line %d is a malformed base constant: %q", i+1, line)
			}
			if cid, err := strconv.Atoi(parts[2]); err != nil || cid <= 0 {
				return freehandFieldLayout{}, fmt.Errorf("field layout line %d has an invalid class id: %q", i+1, line)
			}
			consts[line] = true
			continue
		}
		if strings.HasPrefix(line, "@static\t") {
			parts := strings.Split(line, "\t")
			if len(parts) != 5 || strings.Count(parts[1], "::") < 2 || strings.HasSuffix(parts[1], "::") {
				return freehandFieldLayout{}, fmt.Errorf("field layout line %d is a malformed static field: %q", i+1, line)
			}
			id, err1 := strconv.Atoi(parts[2])
			pool, err2 := strconv.Atoi(parts[3])
			flags, err3 := strconv.Atoi(parts[4])
			if err1 != nil || err2 != nil || err3 != nil || id < 0 || pool < -1 || flags < 0 || flags > 15 {
				return freehandFieldLayout{}, fmt.Errorf("field layout line %d has an invalid static field record: %q", i+1, line)
			}
			sf := freehandStaticField{FieldID: id, PoolIndex: pool, Flags: flags}
			if prev, dup := statics[parts[1]]; dup && prev != sf {
				return freehandFieldLayout{}, fmt.Errorf("field layout lists static %s twice with different records", parts[1])
			}
			statics[parts[1]] = sf
			continue
		}
		if strings.HasPrefix(line, "#") {
			if m := fieldLayoutWrittenRe.FindStringSubmatch(line); m != nil {
				written, _ = strconv.Atoi(m[1])
			}
			if m := fieldLayoutStaticsRe.FindStringSubmatch(line); m != nil {
				staticsRecorded, _ = strconv.Atoi(m[1])
			}
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 7 || strings.Count(parts[0], "::") < 2 || !fieldLayoutRepRe.MatchString(parts[3]) || !isBit(parts[4]) || !isBit(parts[5]) || !isBit(parts[6]) {
			return freehandFieldLayout{}, fmt.Errorf("field layout line %d is malformed: %q", i+1, line)
		}
		cid, err1 := strconv.Atoi(parts[1])
		off, err2 := strconv.Atoi(parts[2])
		// A class row (`lib::Class::`, representation `class`) has offset 0 and no field; a field row has both.
		classRow := parts[3] == "class"
		if classRow != strings.HasSuffix(parts[0], "::") || (classRow && (off != 0 || parts[4] != "0" || parts[6] != "0")) {
			return freehandFieldLayout{}, fmt.Errorf("field layout line %d mixes a class row and a field row: %q", i+1, line)
		}
		if err1 != nil || err2 != nil || cid <= 0 || (!classRow && off <= 0) {
			return freehandFieldLayout{}, fmt.Errorf("field layout line %d has an invalid class id or offset: %q", i+1, line)
		}
		slot := freehandFieldSlot{ClassID: cid, Offset: off, Rep: parts[3], Late: parts[4] == "1", Named: parts[5] == "1", FieldObject: parts[6] == "1"}
		if prev, dup := out[parts[0]]; dup && prev != slot {
			return freehandFieldLayout{}, fmt.Errorf("field layout lists %s twice with different slots", parts[0])
		}
		out[parts[0]] = slot
		if written >= 0 {
			// A trailer before the last entry would let entries escape the count.
			return freehandFieldLayout{}, fmt.Errorf("field layout line %d follows its written= trailer", i+1)
		}
	}
	if written >= 0 && written < len(out) {
		return freehandFieldLayout{}, fmt.Errorf("field layout trailer says %d entries but %d were read", written, len(out))
	}
	if staticsRecorded >= 0 && staticsRecorded != len(statics) {
		return freehandFieldLayout{}, fmt.Errorf("field layout says %d static fields but %d were read", staticsRecorded, len(statics))
	}
	if staticsRecorded < 0 && len(statics) > 0 {
		return freehandFieldLayout{}, errors.New("field layout lists static fields without its statics= count")
	}
	return layout(), nil
}

// renderFreehandFieldLayout is the canonical form persisted in a baseline: schema line, then the entries
// sorted by field key.
func renderFreehandFieldLayout(l freehandFieldLayout) []byte {
	m := l.Slots
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("# " + freehandFieldLayoutSchema + "\n")
	for _, k := range keys {
		s := m[k]
		fmt.Fprintf(&b, "%s\t%d\t%d\t%s\t%s\t%s\t%s\n", k, s.ClassID, s.Offset, s.Rep, bit(s.Late), bit(s.Named), bit(s.FieldObject))
	}
	consts := make([]string, 0, len(l.Consts))
	for c := range l.Consts {
		consts = append(consts, c)
	}
	sort.Strings(consts)
	for _, c := range consts {
		b.WriteString(c + "\n")
	}
	if l.StaticsRecorded {
		skeys := make([]string, 0, len(l.Statics))
		for k := range l.Statics {
			skeys = append(skeys, k)
		}
		sort.Strings(skeys)
		for _, k := range skeys {
			sf := l.Statics[k]
			fmt.Fprintf(&b, "@static\t%s\t%d\t%d\t%d\n", k, sf.FieldID, sf.PoolIndex, sf.Flags)
		}
		fmt.Fprintf(&b, "# statics=%d\n", len(l.Statics))
	}
	return b.Bytes()
}

func toolchainDeclaresFieldLayout(toolchain string) (bool, error) {
	if strings.TrimSpace(toolchain) == "" {
		return false, nil
	}
	bundleDir, err := iosCachedToolchainBundleDir(toolchain)
	if err != nil {
		return false, nil
	}
	enginePath := filepath.Join(bundleDir, "engine.json")
	if _, err := os.Stat(enginePath); os.IsNotExist(err) {
		return false, nil
	}
	return engineBundleDeclaresIdentityCapability(enginePath, freehandFieldLayoutCapability)
}

// withFreehandFieldLayout is the release-side wiring: on a toolchain that declares the capability it
// appends --extra-gen-snapshot-options=--soroq_field_layout=<abs path>.
func withFreehandFieldLayout(projectDir, toolchain string, passthrough []string) ([]string, string, bool, error) {
	if strings.Contains(strings.Join(passthrough, " "), "soroq_field_layout") {
		return nil, "", false, fmt.Errorf("the build arguments already pass %s; Soroq sets it itself", freehandFieldLayoutFlag)
	}
	declared, err := toolchainDeclaresFieldLayout(toolchain)
	if err != nil {
		return nil, "", false, fmt.Errorf("read the toolchain's field-layout capability: %w", err)
	}
	if !declared {
		return passthrough, "", false, nil
	}
	path, err := freehandCodeFingerprintsStagingPath(projectDir, freehandFieldLayoutFile)
	if err != nil {
		return nil, "", false, err
	}
	out := append(append([]string(nil), passthrough...), "--extra-gen-snapshot-options="+freehandFieldLayoutFlag+"="+path)
	return out, path, true, nil
}

// collectFreehandFieldLayout reads the file THIS build's gen_snapshot wrote and returns its canonical form.
func collectFreehandFieldLayout(path string, buildStart time.Time) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("the build produced no field layout at %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("the field layout at %s is not a regular file", path)
	}
	if fi.ModTime().Before(buildStart.Add(-2 * time.Second)) {
		return nil, fmt.Errorf("the field layout at %s predates this build; refusing to adopt a stale file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(raw, []byte("# "+freehandFieldLayoutSchema+"\n")) {
		return nil, fmt.Errorf("the field layout at %s does not start with the %s schema line", path, freehandFieldLayoutSchema)
	}
	m, err := parseFreehandFieldLayout(raw)
	if err != nil {
		return nil, err
	}
	if len(m.Slots) == 0 {
		return nil, fmt.Errorf("the field layout at %s lists no fields", path)
	}
	return renderFreehandFieldLayout(m), nil
}

// bindFreehandFieldLayout records the canonical layout onto meta, under the same both-directions rule as
// the code fingerprints: required iff the engine declares the capability, forbidden otherwise.
func bindFreehandFieldLayout(meta *FreehandBaselineMeta, capabilities *FreehandRedirectCapabilities, layout []byte) error {
	meta.FieldLayoutSchema, meta.FieldLayoutSHA256, meta.FieldLayoutEntries = "", "", 0
	want := capabilities.hasIdentityCapability(freehandFieldLayoutCapability)
	have := layout != nil
	if want && !have {
		return fmt.Errorf("refusing to persist a baseline built by engine %s, which declares %s, without the field layout its build recorded", meta.EngineRev, freehandFieldLayoutCapability)
	}
	if !want && have {
		return fmt.Errorf("refusing to persist a field layout into a baseline whose engine %s does not declare %s", meta.EngineRev, freehandFieldLayoutCapability)
	}
	if !have {
		return nil
	}
	m, err := parseFreehandFieldLayout(layout)
	if err != nil {
		return fmt.Errorf("refusing to persist a malformed field layout: %w", err)
	}
	if !bytes.Equal(renderFreehandFieldLayout(m), layout) {
		return errors.New("refusing to persist a field layout that is not in canonical form")
	}
	meta.FieldLayoutSchema = freehandFieldLayoutSchema
	meta.FieldLayoutSHA256 = freehandSHA256Bytes(layout)
	meta.FieldLayoutEntries = m.entries()
	return nil
}

// verifiedBaselineFieldLayoutPath checks the file in BOTH directions and returns its path, or "" for a
// base whose engine does not declare the capability.
func verifiedBaselineFieldLayoutPath(relDir string, m *FreehandBaselineMeta) (string, error) {
	caps, err := baseRedirectCapabilities(m)
	if err != nil {
		return "", err
	}
	p := filepath.Join(relDir, freehandFieldLayoutFile)
	if !caps.hasIdentityCapability(freehandFieldLayoutCapability) {
		if m.FieldLayoutSchema != "" || m.FieldLayoutSHA256 != "" || m.FieldLayoutEntries != 0 {
			return "", fmt.Errorf("baseline %s records a field layout but its engine does not declare %s", relDir, freehandFieldLayoutCapability)
		}
		if _, err := os.Lstat(p); err == nil {
			return "", fmt.Errorf("baseline %s contains %s but its engine does not declare %s", relDir, freehandFieldLayoutFile, freehandFieldLayoutCapability)
		}
		return "", nil
	}
	if m.FieldLayoutSchema != freehandFieldLayoutSchema || !sha256HexRe.MatchString(m.FieldLayoutSHA256) {
		return "", fmt.Errorf("baseline %s was built by an engine declaring %s but records no valid field layout", relDir, freehandFieldLayoutCapability)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("baseline built with %s is missing %s: %w", freehandFieldLayoutCapability, freehandFieldLayoutFile, err)
	}
	if got := freehandSHA256Bytes(raw); got != m.FieldLayoutSHA256 {
		return "", fmt.Errorf("baseline %s hash mismatch: %s != recorded %s", freehandFieldLayoutFile, got, m.FieldLayoutSHA256)
	}
	parsed, err := parseFreehandFieldLayout(raw)
	if err != nil {
		return "", fmt.Errorf("baseline %s: %w", freehandFieldLayoutFile, err)
	}
	if !bytes.Equal(renderFreehandFieldLayout(parsed), raw) || parsed.entries() != m.FieldLayoutEntries {
		return "", fmt.Errorf("baseline %s is not in canonical form or does not match its recorded count", freehandFieldLayoutFile)
	}
	return p, nil
}

func isBit(s string) bool { return s == "0" || s == "1" }

func bit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
