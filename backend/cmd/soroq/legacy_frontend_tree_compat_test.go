package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLegacyFrontendTree lays down the live 3.44.2 frontend's layout: git HEAD, bin/cache/dart-sdk/revision
// holding a Dart COMMIT, the engine revision in bin/cache/engine.stamp, and NO bin/internal/engine.version.
func writeLegacyFrontendTree(t *testing.T, flutter, treeDart, engineStamp string) string {
	t.Helper()
	dir := writeFrontendTree(t, flutter, treeDart, "unused")
	if err := os.Remove(filepath.Join(dir, "bin", "internal", "engine.version")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "cache", "engine.stamp"), []byte(engineStamp+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const (
	legacyFV      = "soroq-flutter-frontend-f74781f6-7277aaec-1a113cf9-clean-r5"
	legacyArchive = "de993bb264f2cafac60c06fe0dc4786afcc64059f2eaee0e352cae0496882d6a"
	legacyEngine  = "3499c0081904b3796be1ba0bf266d99f9ba04399"
	legacyTreeDrt = "9576691c37d84d3b66a9722e4fadacc764f04b21"
)

func legacyManifest() frontendManifest {
	m := frontendFor(expectedFlutterRevision, "3.13.0-103.1.beta", legacyEngine, "soroq-ios-3.44.2-production-f74781f6-3499c008-clean-r5")
	m.SoroqFrontendVersion = legacyFV
	m.Archive.SHA256 = legacyArchive
	return m
}

// The live production default frontend installs: every marker is checked at its legacy location/form.
func TestLegacyProductionFrontendTreeIsVerified(t *testing.T) {
	m := legacyManifest()
	if err := verifyFrontendTreeMatchesManifest(m, writeLegacyFrontendTree(t, expectedFlutterRevision, legacyTreeDrt, legacyEngine)); err != nil {
		t.Fatalf("the live 3.44.2 production frontend tree was refused: %v", err)
	}
}

// Planted negatives: the legacy mapping still checks every marker, and never applies to anything but the
// exact enumerated artifact.
func TestLegacyFrontendTreeExemptionIsExact(t *testing.T) {
	good := func() string { return writeLegacyFrontendTree(t, expectedFlutterRevision, legacyTreeDrt, legacyEngine) }
	cases := map[string]struct {
		m    frontendManifest
		tree string
		want string
	}{
		"different archive bytes -> strict check": {func() frontendManifest { m := legacyManifest(); m.Archive.SHA256 = strings.Repeat("a", 64); return m }(), good(), "engine.version"},
		"different version -> strict check": {func() frontendManifest {
			m := legacyManifest()
			m.SoroqFrontendVersion = "soroq-flutter-frontend-other"
			return m
		}(), good(), "engine.version"},
		"different declared dart -> strict check": {func() frontendManifest { m := legacyManifest(); m.DartRevision = "3.13.0-103.2.beta"; return m }(), good(), "engine.version"},
		"wrong engine.stamp":                      {legacyManifest(), writeLegacyFrontendTree(t, expectedFlutterRevision, legacyTreeDrt, strings.Repeat("b", 40)), "engine revision"},
		"wrong tree dart":                         {legacyManifest(), writeLegacyFrontendTree(t, expectedFlutterRevision, strings.Repeat("c", 40), legacyEngine), "dart revision"},
		"wrong flutter HEAD":                      {legacyManifest(), writeLegacyFrontendTree(t, strings.Repeat("d", 40), legacyTreeDrt, legacyEngine), "flutter revision"},
	}
	for name, c := range cases {
		err := verifyFrontendTreeMatchesManifest(c.m, c.tree)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want refusal mentioning %q, got %v", name, c.want, err)
		}
	}
}

// The single enumerated entry is exactly the live artifact (manifest fetched 2026-09-25; tree markers
// read from the streamed archive, sha256 de993bb2…).
func TestLegacyFrontendTreeTableIsExactlyTheLiveArtifact(t *testing.T) {
	if len(legacyFrontendTreeLayouts) != 1 {
		t.Fatalf("the legacy tree table must hold exactly one artifact, has %d", len(legacyFrontendTreeLayouts))
	}
	l := legacyFrontendTreeLayouts[legacyFV]
	if l.archiveSHA256 != legacyArchive || l.manifestDart != "3.13.0-103.1.beta" || l.treeDartRevision != legacyTreeDrt || l.engineMarker != "bin/cache/engine.stamp" {
		t.Fatalf("legacy entry drifted: %+v", l)
	}
}
