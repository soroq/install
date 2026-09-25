package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A TAR ENTRY TYPE MUST MEAN WHAT IT MEANS, AND NO ENTRY MAY WRITE OUTSIDE THE EXTRACTION ROOT.
//
// Two extractors disagreed. The frontend's refused every non-regular entry, so a correctly signed Flutter
// 3.44.9 frontend died after a full 566 MiB download on
//
//	refusing unsupported archive entry type 50 for "flutter-sdk-src/engine/src/flutter/lib/web_ui/…"
//
// -- type 50 is a symlink, and 3.44.9 ships engine/src, which has them. The toolchain's did the opposite
// and wrote ANY non-directory entry as a regular file, so a symlink silently became a file containing its
// own target path. Both now share one implementation, and symlinks are the one entry type that can escape
// the root without a ".." in its own name, so containment is asserted here in both link forms.

type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
	mode     int64
}

func tarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: mode, Linkname: e.linkname}
		if e.typeflag == tar.TypeXGlobalHeader {
			// The writer accepts nothing but PAXRecords on a global header.
			h = &tar.Header{Name: e.name, Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "fixture"}}
		}
		if e.typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func extract(t *testing.T, b []byte) (string, error) {
	t.Helper()
	dir := t.TempDir()
	return dir, untarGzReader(bytes.NewReader(b), dir)
}

// --- A SYMLINK EXTRACTS AS A SYMLINK ---------------------------------------------------------------

func TestSymlinkEntryIsExtractedAsALink(t *testing.T) {
	dir, err := extract(t, tarGz(t,
		tarEntry{name: "sdk/real/target.dart", typeflag: tar.TypeReg, body: "// real\n"},
		tarEntry{name: "sdk/tests/link.dart", typeflag: tar.TypeSymlink, linkname: "../real/target.dart"},
	))
	if err != nil {
		t.Fatalf("a symlink entry was refused: %v", err)
	}
	link := filepath.Join(dir, "sdk", "tests", "link.dart")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		// This is the toolchain extractor's old behaviour: a link written as a regular file.
		body, _ := os.ReadFile(link)
		t.Fatalf("the symlink was written as a regular file containing %q", string(body))
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("the link does not resolve: %v", err)
	}
	if string(got) != "// real\n" {
		t.Fatalf("the link resolves to the wrong content: %q", string(got))
	}
}

// The exact shape that refused the real frontend: a relative link inside a deep engine/src tree.
func TestFlutterEngineSrcSymlinkShapeIsAccepted(t *testing.T) {
	if _, err := extract(t, tarGz(t,
		tarEntry{name: "flutter-sdk-src/engine/src/flutter/lib/web_ui/dev/target.dart", typeflag: tar.TypeReg, body: "x"},
		tarEntry{name: "flutter-sdk-src/engine/src/flutter/lib/web_ui/test/ui/paragraph_performance_test.dart",
			typeflag: tar.TypeSymlink, linkname: "../../dev/target.dart"},
	)); err != nil {
		t.Fatalf("the 3.44.9 engine/src symlink shape was refused: %v", err)
	}
}

func TestHardlinkEntryIsExtracted(t *testing.T) {
	dir, err := extract(t, tarGz(t,
		tarEntry{name: "sdk/a.txt", typeflag: tar.TypeReg, body: "shared"},
		tarEntry{name: "sdk/b.txt", typeflag: tar.TypeLink, linkname: "sdk/a.txt"},
	))
	if err != nil {
		t.Fatalf("a hardlink entry was refused: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "sdk", "b.txt"))
	if err != nil || string(b) != "shared" {
		t.Fatalf("the hardlink does not carry the linked content: %q %v", string(b), err)
	}
}

// --- NOTHING MAY WRITE OUTSIDE THE ROOT -----------------------------------------------------------

func TestLinkEscapingTheRootIsRefused(t *testing.T) {
	for name, entries := range map[string][]tarEntry{
		"symlink to an absolute path": {
			{name: "sdk/evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		},
		"symlink climbing out": {
			{name: "sdk/evil", typeflag: tar.TypeSymlink, linkname: "../../../../etc"},
		},
		"symlink climbing out from a deep entry": {
			{name: "a/b/c/evil", typeflag: tar.TypeSymlink, linkname: "../../../../outside"},
		},
		"hardlink climbing out": {
			{name: "sdk/evil", typeflag: tar.TypeLink, linkname: "../../../../etc/passwd"},
		},
		"hardlink to an absolute path": {
			{name: "sdk/evil", typeflag: tar.TypeLink, linkname: "/etc/passwd"},
		},
	} {
		if _, err := extract(t, tarGz(t, entries...)); err == nil {
			t.Fatalf("%s: an entry resolving outside the extraction root was accepted", name)
		}
	}
}

// The attack the containment check exists for: a link out, then a write THROUGH it. The link must be
// refused, so the payload entry never gets a path to travel.
func TestWriteThroughAnEscapingSymlinkIsRefused(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := extract(t, tarGz(t,
		tarEntry{name: "sdk/escape", typeflag: tar.TypeSymlink, linkname: "../../../../../../../../" + strings.TrimPrefix(outside, "/")},
		tarEntry{name: "sdk/escape/victim.txt", typeflag: tar.TypeReg, body: "OVERWRITTEN"},
	))
	if err == nil {
		t.Fatal("an escaping symlink was accepted, so a later entry could write through it")
	}
	body, rerr := os.ReadFile(victim)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(body) != "original" {
		t.Fatalf("a file outside the extraction root was overwritten: %q", string(body))
	}
}

// A link that stays inside must still work after the escape checks, or they'd be satisfied by refusing
// every link -- which is the defect this replaced.
func TestALinkThatStaysInsideIsStillAccepted(t *testing.T) {
	if _, err := extract(t, tarGz(t,
		tarEntry{name: "sdk/deep/nested/file.txt", typeflag: tar.TypeReg, body: "ok"},
		tarEntry{name: "sdk/deep/other/link.txt", typeflag: tar.TypeSymlink, linkname: "../nested/file.txt"},
	)); err != nil {
		t.Fatalf("an in-root link was refused: %v", err)
	}
}

// --- THE PRE-EXISTING GUARANTEES ARE UNCHANGED ---------------------------------------------------

func TestPathTraversalAndUnsupportedTypesStillRefused(t *testing.T) {
	if _, err := extract(t, tarGz(t, tarEntry{name: "../escape.txt", typeflag: tar.TypeReg, body: "x"})); err == nil {
		t.Fatal("a .. traversal entry was accepted")
	}
	if _, err := extract(t, tarGz(t, tarEntry{name: "sdk/dev", typeflag: tar.TypeChar})); err == nil {
		t.Fatal("a character-device entry was accepted")
	}
}

// Archive-level PAX metadata describes the stream, not a file, and must not be mistaken for either.
func TestGlobalPaxHeaderIsSkipped(t *testing.T) {
	dir, err := extract(t, tarGz(t,
		tarEntry{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader},
		tarEntry{name: "sdk/file.txt", typeflag: tar.TypeReg, body: "ok"},
	))
	if err != nil {
		t.Fatalf("a global pax header was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pax_global_header")); err == nil {
		t.Fatal("the pax header was materialised as a file")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sdk", "file.txt")); string(b) != "ok" {
		t.Fatal("the entry after the pax header was lost")
	}
}

// Both installs go through one extractor now; if they ever diverge again, this fails.
func TestToolchainAndFrontendShareOneExtractor(t *testing.T) {
	b := tarGz(t,
		tarEntry{name: "sdk/real.txt", typeflag: tar.TypeReg, body: "x"},
		tarEntry{name: "sdk/link.txt", typeflag: tar.TypeSymlink, linkname: "real.txt"},
	)
	viaToolchain := t.TempDir()
	if err := untarGz(bytes.NewReader(b), viaToolchain); err != nil {
		t.Fatalf("the toolchain extractor refused a symlink: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(viaToolchain, "sdk", "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the toolchain extractor still writes a symlink as a regular file")
	}
}
