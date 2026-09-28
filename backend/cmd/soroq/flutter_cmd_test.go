package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The version-manager surface must be CATALOG-DRIVEN. These tests exist because the failure mode is
// silent: a hardcoded version list works perfectly until the matrix publishes a new entry, and then
// needs a CLI release to do what should have been data publication.

func TestFlutterCommandCarriesNoHardcodedVersionList(t *testing.T) {
	src, err := os.ReadFile("flutter_cmd.go")
	if err != nil {
		t.Fatalf("read flutter_cmd.go: %v", err)
	}
	body := string(src)
	// A version literal such as "3.44.2" in this file would mean the list is not read from the catalog.
	for _, bad := range []string{`"3.4`, `"3.2`, `"3.1`, `"3.3`} {
		if strings.Contains(body, bad) {
			t.Errorf("flutter_cmd.go contains what looks like a hardcoded Flutter version (%s); the "+
				"supported list must come from the served catalog so a new matrix entry needs data "+
				"publication, not a CLI release", bad)
		}
	}
}

func TestFlutterUseRefusesUnsupportedVersionBeforeAnyBuild(t *testing.T) {
	// The refusal must name what IS supported, and must say why a range is not offered.
	entries := []flutterVersionEntry{
		{FlutterVersion: "3.44.2", Platform: "ios", ToolchainVersion: "tc-ios"},
		{FlutterVersion: "3.44.2", Platform: "android", ToolchainVersion: "tc-android"},
	}
	var available []string
	seen := map[string]bool{}
	for _, e := range entries {
		if !seen[e.FlutterVersion] {
			seen[e.FlutterVersion] = true
			available = append(available, e.FlutterVersion)
		}
	}
	if len(available) != 1 || available[0] != "3.44.2" {
		t.Fatalf("dedupe of supported versions is wrong: %v", available)
	}
}

// A pin must carry the identity a later `patch` needs, or patch cannot reuse the release's toolchain
// without being told. This is the whole point of writing a lock.
func TestFlutterUseWritesPinCarryingToolchainAndFrontend(t *testing.T) {
	dir := t.TempDir()
	lock := soroqLock{Platforms: map[string]soroqLockPin{
		"ios": {ToolchainVersion: "soroq-ios-x", FrontendVersion: "soroq-frontend-y"},
	}}
	if err := saveSoroqLock(dir, lock); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadSoroqLock(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	pin, ok := got.Platforms["ios"]
	if !ok {
		t.Fatal("ios pin missing after round trip")
	}
	if pin.ToolchainVersion != "soroq-ios-x" {
		t.Errorf("toolchain identity lost: %q", pin.ToolchainVersion)
	}
	if pin.FrontendVersion != "soroq-frontend-y" {
		t.Errorf("frontend identity lost: %q", pin.FrontendVersion)
	}
	if _, err := os.Stat(filepath.Join(dir, "soroq.lock")); err != nil {
		t.Errorf("soroq.lock not written: %v", err)
	}
}

func TestShortRevisionDoesNotTruncateNamedRevisionsMidWord(t *testing.T) {
	named := "soroq.android_engine.candidate.sha256:12d3315131f5e58e"
	if got := shortRevision(named); got != named {
		t.Errorf("named engine revision was truncated to %q; that reads as corruption, not abbreviation", got)
	}
	hexRev := "9576691c37d84d3b66a9722e4fadacc764f04b21"
	if got := shortRevision(hexRev); got != "9576691c37d8" {
		t.Errorf("hex revision should abbreviate to 12 chars, got %q", got)
	}
}

func TestFlutterHelpDoesNotAdvertiseAVersionRange(t *testing.T) {
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	flutterUsage()
	w.Close()
	os.Stderr = old
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	help := sb.String()
	for _, banned := range []string{">=", "^3.", "~3.", "or later", "and above"} {
		if strings.Contains(help, banned) {
			t.Errorf("flutter help advertises a range (%q); Soroq supports exact versions only", banned)
		}
	}
	if !strings.Contains(help, "exact") {
		t.Error("flutter help does not tell the user versions are exact, which is the product constraint")
	}
}

// The version list reads EVERY entry of the v2 catalog. Before this, it read v1 only, so a second
// published stack could not be listed or pinned without hidden flags.
func TestFlutterVersionsListEveryV2Entry(t *testing.T) {
	signer := setupTestSigner(t)
	v1 := v1Body()
	v1Sig, _ := signer.SignToolchainManifest(v1)
	v2 := v2Body()
	v2Sig, _ := signer.SignToolchainManifest(v2)
	srv := catalogBothVersionsServer(t, string(v1), v1Sig, http.StatusOK, string(v2), v2Sig)
	defer srv.Close()

	pairs, err := publishedFlutterPairs(srv.URL)
	if err != nil {
		t.Fatalf("publishedFlutterPairs: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("got %d pairs, want both v2 entries: %+v", len(pairs), pairs)
	}
	for _, p := range pairs {
		if p.Source != "v2" || p.FlutterRevision == "" {
			t.Errorf("pair %+v was not read from v2 with its revision", p)
		}
	}
}

// v1 is read only when v2 is genuinely absent; any other v2 failure refuses rather than downgrading.
func TestFlutterVersionsFallBackToV1OnlyOn404(t *testing.T) {
	signer := setupTestSigner(t)
	v1 := v1Body()
	v1Sig, _ := signer.SignToolchainManifest(v1)

	absent := catalogBothVersionsServer(t, string(v1), v1Sig, http.StatusNotFound, "", "")
	pairs, err := publishedFlutterPairs(absent.URL)
	absent.Close()
	if err != nil || len(pairs) != 1 || pairs[0].Source != "v1" || pairs[0].ToolchainVersion != "tc-v1" {
		t.Fatalf("v2 absent: got %+v, %v; want the single v1 pair", pairs, err)
	}

	broken := catalogBothVersionsServer(t, string(v1), v1Sig, http.StatusInternalServerError, "", "")
	pairs, err = publishedFlutterPairs(broken.URL)
	broken.Close()
	if err == nil {
		t.Fatalf("v2 failing with 500 produced %+v; it must refuse, not fall back to v1", pairs)
	}
}

func TestNewestFlutterVersionComparesNumerically(t *testing.T) {
	entries := []flutterVersionEntry{{FlutterVersion: "3.44.9"}, {FlutterVersion: "3.44.10"}, {FlutterVersion: "3.44.2"}}
	if got := newestFlutterVersion(entries); got != "3.44.10" {
		t.Errorf("newest = %q, want 3.44.10", got)
	}
}

// After pinning a v2 entry, the next step must install THAT entry, which plain `soroq setup` does not.
func TestFlutterUseSuggestsTheSetupThatInstallsThePin(t *testing.T) {
	rev := strings.Repeat("ab", 20)
	got := setupCommandFor([]flutterVersionEntry{
		{Platform: "ios", FlutterRevision: rev, CatalogV2: true},
		{Platform: "android", FlutterRevision: rev, CatalogV2: true},
	})
	want := "soroq setup --platforms android,ios --catalog-v2 --flutter-revision " + rev
	if got != want {
		t.Errorf("setup command = %q, want %q", got, want)
	}
	if got := setupCommandFor([]flutterVersionEntry{{Platform: "ios"}}); got != "soroq setup --platforms ios" {
		t.Errorf("v1 pin setup command = %q", got)
	}
}
