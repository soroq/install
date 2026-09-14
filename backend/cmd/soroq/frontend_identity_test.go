package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE CLI MUST INSTALL ANY CORRECTLY SIGNED FRONTEND, AND MUST PROVE THE TREE MATCHES THE SIGNATURE.
//
// `frontend install` compared every signed frontend against expectedFlutterRevision, a constant compiled
// into the CLI at 3.44.2. A correctly signed Flutter 3.44.9 frontend was therefore unreachable without a
// CLI rebuild and release -- doctor included. The signature authorises the identity; what the CLI still
// owes is that the identity is WELL-FORMED, and that the tree it just extracted is the tree that identity
// describes. Removing the pin without adding the second half would let a signed manifest front any
// archive, so both halves are pinned here, along with the pair binding that keeps a frontend from being
// built against a toolchain its publisher never declared.

func frontendFor(flutter, dart, engine string, compat ...string) frontendManifest {
	return frontendManifest{
		Schema:                 frontendManifestSchema,
		SoroqFrontendVersion:   "soroq-flutter-frontend-fixture",
		FlutterRevision:        flutter,
		DartRevision:           dart,
		EngineRevision:         engine,
		PatchsetSHA256:         strings.Repeat("a", 64),
		CompatibleToolchainIDs: compat,
	}
}

const psToolchain = "soroq-ios-3.44.9-release-6b182d2c_5a2a6a42-private_state-r1"

// writeFrontendTree lays down the three markers verifyFrontendTreeMatchesManifest reads: the dart-sdk
// revision, bin/internal/engine.version, and a git HEAD resolving to the framework revision. Enough git
// metadata for rev-parse, which is all the production archive is relied on to carry.
func writeFrontendTree(t *testing.T, flutter, dart, engine string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		filepath.Join("bin", "cache", "dart-sdk", "revision"): dart,
		filepath.Join("bin", "internal", "engine.version"):    engine,
		filepath.Join(".git", "HEAD"):                         "ref: refs/heads/main",
		filepath.Join(".git", "config"):                       "[core]\n\trepositoryformatversion = 0",
		filepath.Join(".git", "refs", "heads", "main"):        flutter,
		filepath.Join(".git", "objects", "info", "packs"):     "",
	} {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// --- THE PIN IS GONE -----------------------------------------------------------------------------

// The exact frontend the old constant refused.
func TestSignedFlutter3449FrontendIdentityIsAccepted(t *testing.T) {
	m := frontendFor(psFlutter, psDart, psEngine, psToolchain)
	if err := checkFrontendIdentity(m); err != nil {
		t.Fatalf("the signed Flutter 3.44.9 frontend identity was refused: %v", err)
	}
	if err := verifyFrontendTreeMatchesManifest(m, writeFrontendTree(t, psFlutter, psDart, psEngine)); err != nil {
		t.Fatalf("a tree matching its signed manifest was refused: %v", err)
	}
}

// A FUTURE signed frontend must install with no CLI edit. Asserted with revisions that appear nowhere in
// this repository, so the check cannot be satisfied by a widened allow-list.
func TestUnknownFutureFrontendInstallsWithoutACLIEdit(t *testing.T) {
	f, d := strings.Repeat("1", 40), strings.Repeat("2", 40)
	m := frontendFor(f, d, "engine-of-the-future", "soroq-ios-9.9.9-release-future")
	if err := checkFrontendIdentity(m); err != nil {
		t.Fatalf("a future signed frontend was refused: %v", err)
	}
	if err := verifyFrontendTreeMatchesManifest(m, writeFrontendTree(t, f, d, "engine-of-the-future")); err != nil {
		t.Fatalf("a matching future tree was refused: %v", err)
	}
}

// PLANTED: the defect itself. The old check was EqualFold(m.FlutterRevision, expectedFlutterRevision).
// If that comparison is ever reintroduced, the signed 3.44.9 frontend above stops installing -- so pin
// that the constant does NOT gate identity, using the constant itself.
func TestIdentityDoesNotGateOnTheCompileTimeConstant(t *testing.T) {
	m := frontendFor(psFlutter, psDart, psEngine, psToolchain)
	if strings.EqualFold(m.FlutterRevision, expectedFlutterRevision) {
		t.Fatal("the fixture accidentally matches the constant; this control proves nothing")
	}
	if err := checkFrontendIdentity(m); err != nil {
		t.Fatalf("identity is still gated on expectedFlutterRevision: %v", err)
	}
}

// --- MALFORMED IDENTITY IS STILL REFUSED ----------------------------------------------------------

func TestMalformedFrontendIdentityIsRefused(t *testing.T) {
	for name, m := range map[string]frontendManifest{
		"no flutter revision":  frontendFor("", psDart, psEngine, psToolchain),
		"short flutter":        frontendFor("6b182d2c", psDart, psEngine, psToolchain),
		"non-hex flutter":      frontendFor(strings.Repeat("z", 40), psDart, psEngine, psToolchain),
		"no dart revision":     frontendFor(psFlutter, "", psEngine, psToolchain),
		"non-hex dart":         frontendFor(psFlutter, strings.Repeat("q", 40), psEngine, psToolchain),
		"no engine revision":   frontendFor(psFlutter, psDart, "  ", psToolchain),
		"no compatible pair":   frontendFor(psFlutter, psDart, psEngine),
		"empty compatible id":  frontendFor(psFlutter, psDart, psEngine, "   "),
		"duplicate compatible": frontendFor(psFlutter, psDart, psEngine, psToolchain, psToolchain),
	} {
		if err := checkFrontendIdentity(m); err == nil {
			t.Fatalf("%s: a malformed frontend identity was accepted", name)
		}
	}
}

// --- MANIFEST / TREE DISAGREEMENT MUST REFUSE BEFORE THE CACHE SWAP -------------------------------

func TestFrontendTreeDisagreeingWithItsManifestIsRefused(t *testing.T) {
	m := frontendFor(psFlutter, psDart, psEngine, psToolchain)
	for name, tree := range map[string]string{
		"tree carries another flutter": writeFrontendTree(t, r2Flutter, psDart, psEngine),
		"tree carries another dart":    writeFrontendTree(t, psFlutter, r2Dart, psEngine),
		"tree carries another engine":  writeFrontendTree(t, psFlutter, psDart, r2Engine),
	} {
		if err := verifyFrontendTreeMatchesManifest(m, tree); err == nil {
			t.Fatalf("%s: a tree disagreeing with its signed manifest was accepted", name)
		}
	}
}

// A tree missing a marker cannot be cross-checked, and must not pass by default.
func TestFrontendTreeMissingItsRevisionMarkersIsRefused(t *testing.T) {
	m := frontendFor(psFlutter, psDart, psEngine, psToolchain)
	full := writeFrontendTree(t, psFlutter, psDart, psEngine)
	for _, rel := range []string{
		filepath.Join("bin", "cache", "dart-sdk", "revision"),
		filepath.Join("bin", "internal", "engine.version"),
	} {
		dir := writeFrontendTree(t, psFlutter, psDart, psEngine)
		if err := os.Remove(filepath.Join(dir, rel)); err != nil {
			t.Fatal(err)
		}
		if err := verifyFrontendTreeMatchesManifest(m, dir); err == nil {
			t.Fatalf("a tree with no %s was accepted", rel)
		}
	}
	// And the positive case still passes, or the checks above would be met by refusing everything.
	if err := verifyFrontendTreeMatchesManifest(m, full); err != nil {
		t.Fatalf("the complete tree was refused: %v", err)
	}
}

// PAIR BINDING is deliberately NOT retested here. This branch keeps the product CLI's own pair checks
// rather than importing the r5 line's assertFrontendToolchainPair helper: the v1 path already refuses an
// unbound pair (TestCatalogReferencePreflightRefusesUnboundPair) and the v2 path refuses a disagreeing
// one inside validatePairIdentity (TestPublishV2RefusesDartRevisionMismatch). Adding a second helper
// that no production path called would have been dead code carrying a test that proved nothing shipped.
