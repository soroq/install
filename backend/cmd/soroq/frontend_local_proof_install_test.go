package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInstallUnsignedFrontendForLocalProof installs an UNSIGNED frontend archive into $HOME through the
// CLI's own extractFrontendArchive (AppleDouble skipping, subdir checks, tree-vs-manifest revision
// check), so a pairing can be proven end to end BEFORE anyone signs it (handoff 27). It is test-only and
// runs only when both env vars are set; the recorded manifest.sig says it is unsigned, so nothing that
// verifies signatures will ever accept this install as published.
func TestInstallUnsignedFrontendForLocalProof(t *testing.T) {
	archive := os.Getenv("SOROQ_LOCAL_FRONTEND_PROOF_ARCHIVE")
	manifestPath := os.Getenv("SOROQ_LOCAL_FRONTEND_PROOF_MANIFEST")
	if archive == "" || manifestPath == "" {
		t.Skip("SOROQ_LOCAL_FRONTEND_PROOF_ARCHIVE / _MANIFEST not set")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m frontendManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != strings.ToLower(m.Archive.SHA256) || n != m.Archive.CompressedBytes {
		t.Fatalf("archive %s / %d B does not match the manifest %s / %d B", got, n, m.Archive.SHA256, m.Archive.CompressedBytes)
	}
	versionDir, err := frontendVersionDir(m.SoroqFrontendVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(versionDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractFrontendArchive(archive, versionDir, m, raw, "UNSIGNED-LOCAL-PROOF"); err != nil {
		t.Fatal(err)
	}
	if err := recordActiveFrontend(activeFrontend{
		Version:       m.SoroqFrontendVersion,
		FlutterBin:    filepath.Join(versionDir, m.subdir(), "bin", "flutter"),
		ArchiveSHA256: strings.ToLower(m.Archive.SHA256),
		InstalledAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("installed %s (unsigned, local proof) at %s", m.SoroqFrontendVersion, versionDir)
}
