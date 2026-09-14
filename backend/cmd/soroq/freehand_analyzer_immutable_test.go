package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A CANDIDATE FRONTEND'S ANALYZER BYTES CANNOT BE REPLACED BY THE INSTALLED FRONTEND'S.
//
// installFreehandAnalyzer writes into whatever frontend root it is handed, and its source is
// resolveBundledAnalyzerSource, which prefers the INSTALLED frontend. Building against a candidate
// therefore overwrote the candidate's own analyzer with a different one, so the candidate stopped
// matching the sha its manifest advertises and `soroq frontend use-candidate` refused it -- the bytes
// under test were destroyed by the act of testing them.
func TestInstallAnalyzerRefusesToOverwriteADeclaredFrontend(t *testing.T) {
	root := t.TempDir()
	flutterRoot := filepath.Join(root, "flutter-sdk-src")
	dst := filepath.Join(flutterRoot, filepath.FromSlash(freehandAnalyzerRelPath))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	candidateBytes := []byte("CANDIDATE-ANALYZER-BYTES")
	if err := os.WriteFile(dst, candidateBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	candSha, err := sha256OfPath(dst)
	if err != nil {
		t.Fatal(err)
	}
	// The frontend advertises exactly these bytes, which is what makes it immutable.
	if err := os.WriteFile(filepath.Join(root, "soroq_frontend_metadata.json"),
		[]byte(`{"analyzer_snapshot_sha256":"`+candSha+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A DIFFERENT analyzer, standing in for the installed frontend's copy.
	other := filepath.Join(t.TempDir(), "soroq_kernel_analyze.dill")
	if err := os.WriteFile(other, []byte("INSTALLED-FRONTEND-ANALYZER"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOROQ_FREEHAND_ANALYZER", other)

	_, _, err = installFreehandAnalyzer(flutterRoot)
	if err == nil {
		t.Fatal("install SUCCEEDED against a frontend that declares its analyzer; the candidate was mutated")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite the analyzer") {
		t.Errorf("refusal does not explain itself: %v", err)
	}
	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(candidateBytes) {
		t.Errorf("candidate analyzer bytes were replaced: got %q", got)
	}
}

// The guard must not fire on an ordinary SDK checkout, which advertises nothing and has nothing to
// protect -- otherwise every normal build would refuse.
func TestInstallAnalyzerStillInstallsWhenTheFrontendDeclaresNothing(t *testing.T) {
	root := t.TempDir()
	flutterRoot := filepath.Join(root, "flutter-sdk-src")
	dst := filepath.Join(flutterRoot, filepath.FromSlash(freehandAnalyzerRelPath))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "soroq_kernel_analyze.dill")
	if err := os.WriteFile(other, []byte("INTENDED-ANALYZER"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOROQ_FREEHAND_ANALYZER", other)

	if _, _, err := installFreehandAnalyzer(flutterRoot); err != nil {
		t.Fatalf("install refused on an undeclared frontend: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "INTENDED-ANALYZER" {
		t.Errorf("analyzer was not installed: %q", got)
	}
}

// THE PUBLISHED-FRONTEND LAYOUT ARMS THE SAME GUARD.
//
// This is the case the first version of the guard could not fire on. A published manifest carries no
// analyzer field; the digest appears only as the 8-hex trailing segment of soroq_frontend_version.
// The guard matched the full 64-hex digest only, returned false, and installFreehandAnalyzer
// overwrote the published frontend's analyzer -- which is exactly what happened on this machine to
// soroq-flutter-frontend-f74781f6-7277aaec-70bae0fd.
//
// Planted failure: shorten the id's trailing segment or drop the prefix arm and this test fails.
func TestInstallAnalyzerRefusesToOverwriteAPublishedFrontend(t *testing.T) {
	root := t.TempDir()
	flutterRoot := filepath.Join(root, "flutter-sdk-src")
	dst := filepath.Join(flutterRoot, filepath.FromSlash(freehandAnalyzerRelPath))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("PUBLISHED-ANALYZER-BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	pubSha, err := sha256OfPath(dst)
	if err != nil {
		t.Fatal(err)
	}
	// A REAL published manifest: no analyzer field anywhere, digest only as the id suffix.
	manifest := `{"schema":"soroq.frontend.v1",` +
		`"soroq_frontend_version":"soroq-flutter-frontend-f74781f6-7277aaec-` + pubSha[:8] + `",` +
		`"flutter_revision":"f74781f6213447540225edae307acb48bbaaaf34"}`
	if strings.Contains(manifest, pubSha) {
		t.Fatalf("test is not exercising the prefix arm: manifest carries the full digest")
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if !frontendDeclaresAnalyzerSha(flutterRoot, pubSha) {
		t.Fatalf("guard is inert on the published layout: it would let a build overwrite %s", pubSha[:12])
	}

	other := filepath.Join(t.TempDir(), "soroq_kernel_analyze.dill")
	if err := os.WriteFile(other, []byte("SOME-OTHER-ANALYZER"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOROQ_FREEHAND_ANALYZER", other)
	if _, _, err := installFreehandAnalyzer(flutterRoot); err == nil {
		t.Fatalf("installFreehandAnalyzer overwrote a published frontend's analyzer")
	} else if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("wrong refusal: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "PUBLISHED-ANALYZER-BYTES" {
		t.Fatalf("published analyzer bytes were replaced: %q", got)
	}
}

// NEGATIVE CONTROL: a near-miss id suffix must NOT arm the guard.
//
// Without this, a guard that returned true unconditionally would pass every assertion above.
func TestPublishedGuardIgnoresANonMatchingIDSuffix(t *testing.T) {
	root := t.TempDir()
	flutterRoot := filepath.Join(root, "flutter-sdk-src")
	if err := os.MkdirAll(flutterRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// The id declares SOME analyzer, just not this one.
	manifest := `{"soroq_frontend_version":"soroq-flutter-frontend-f74781f6-7277aaec-deadbeef"}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	mine := strings.Repeat("a", 64)
	if frontendDeclaresAnalyzerSha(flutterRoot, mine) {
		t.Fatalf("guard armed on a frontend that declares a DIFFERENT analyzer")
	}
	// Positive control on the same fixture: the declared one does arm it.
	theirs := "deadbeef" + strings.Repeat("0", 56)
	if !frontendDeclaresAnalyzerSha(flutterRoot, theirs) {
		t.Fatalf("guard failed to arm on the analyzer the id actually declares")
	}
}

// NEGATIVE CONTROL: the prefix must be anchored to the version string, not found loose in the file.
func TestPublishedGuardDoesNotArmOnALooseHexRun(t *testing.T) {
	root := t.TempDir()
	flutterRoot := filepath.Join(root, "flutter-sdk-src")
	if err := os.MkdirAll(flutterRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// "cafebabe" appears, but as an unrelated field -- not as the id's analyzer segment.
	manifest := `{"soroq_frontend_version":"soroq-flutter-frontend-f74781f6-7277aaec-deadbeef",` +
		`"engine_revision":"cafebabe1904b3796be1ba0bf266d99f9ba04399"}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if frontendDeclaresAnalyzerSha(flutterRoot, "cafebabe"+strings.Repeat("0", 56)) {
		t.Fatalf("guard armed on a hex run outside the declared version")
	}
}
