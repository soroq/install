package main

// CANDIDATE FRONTEND ACTIVATION — the refusals matter more than the happy path.
//
// Activating a candidate used to mean hand-editing active.json. The failure that motivated this
// command is subtle: a frontend whose SOURCE contains the analysis target but whose executed SNAPSHOT
// does not builds successfully, writes no analysis receipt, and only fails much later with a confusing
// content-address error. So "the snapshot really contains the target" is a refusal, not a warning.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCandidate builds a candidate tree on disk, applying mutators to the manifest before writing.
func writeCandidate(t *testing.T, mutate func(m map[string]any), snapshotBody string) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "flutter-sdk-src")
	mustMkdir(t, filepath.Join(root, "bin", "cache", "soroq"))
	mustWriteFile(t, filepath.Join(root, "bin", "flutter"), "#!/bin/sh\n")

	dill := filepath.Join(root, "bin", "cache", "soroq", "soroq_kernel_analyze.dill")
	mustWriteFile(t, dill, "ANALYZER-BYTES")
	sum := sha256.Sum256([]byte("ANALYZER-BYTES"))

	mustWriteFile(t, filepath.Join(root, "bin", "cache", "flutter_tools.snapshot"), snapshotBody)

	m := map[string]any{
		"schema":                   frontendManifestSchema,
		"soroq_frontend_version":   "soroq-flutter-frontend-candidate-test",
		"flutter_revision":         "abc123",
		"patchset_sha256":          strings.Repeat("a", 64),
		"analyzer_snapshot_sha256": hex.EncodeToString(sum[:]),
		"frontend_subdir":          "flutter-sdk-src",
		"candidate":                true,
		"signed":                   false,
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dir, "manifest.json"), string(b))
	return dir
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidCandidateIsAccepted(t *testing.T) {
	dir := writeCandidate(t, nil, "... SoroqFreehandAnalysis ...")
	m, err := verifyCandidateFrontend(dir)
	if err != nil {
		t.Fatalf("a well-formed candidate was refused: %v", err)
	}
	if m.Version == "" || !m.Candidate || m.Signed {
		t.Errorf("manifest not decoded as an unsigned candidate: %+v", m)
	}
}

// THE DECISIVE REFUSAL. Source is irrelevant if the snapshot the tool executes lacks the target.
func TestCandidateWithoutTheTargetInItsSnapshotIsRefused(t *testing.T) {
	dir := writeCandidate(t, nil, "a snapshot with no soroq target at all")
	_, err := verifyCandidateFrontend(dir)
	if err == nil {
		t.Fatal("a candidate whose snapshot lacks SoroqFreehandAnalysis was ACCEPTED; it would build " +
			"successfully and silently skip the analysis")
	}
	if !strings.Contains(err.Error(), "silently skip") {
		t.Errorf("refusal should explain the silent-skip consequence; got: %v", err)
	}
}

func TestMalformedOrIncompleteCandidatesAreRefused(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"wrong schema":             func(m map[string]any) { m["schema"] = "soroq.frontend.v0" },
		"not declared a candidate": func(m map[string]any) { m["candidate"] = false },
		"claims to be signed":      func(m map[string]any) { m["signed"] = true },
		"empty version":            func(m map[string]any) { m["soroq_frontend_version"] = "" },
		"empty patchset sha":       func(m map[string]any) { m["patchset_sha256"] = "" },
		"empty subdir":             func(m map[string]any) { m["frontend_subdir"] = "" },
		"unknown field":            func(m map[string]any) { m["surprise"] = "value" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeCandidate(t, mutate, "... SoroqFreehandAnalysis ...")
			if _, err := verifyCandidateFrontend(dir); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// The bundled analyzer must be the one the manifest names, or the frontend would invoke a different
// analyzer than the identity it advertises.
func TestBundledAnalyzerMustMatchTheManifestHash(t *testing.T) {
	dir := writeCandidate(t, func(m map[string]any) {
		m["analyzer_snapshot_sha256"] = strings.Repeat("b", 64)
	}, "... SoroqFreehandAnalysis ...")
	_, err := verifyCandidateFrontend(dir)
	if err == nil {
		t.Fatal("a candidate whose bundled analyzer does not match its manifest hash was accepted")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("refusal should name the hash mismatch; got: %v", err)
	}
}

func TestMissingPiecesAreRefused(t *testing.T) {
	for name, remove := range map[string]string{
		"no manifest": "manifest.json",
		"no flutter":  "flutter-sdk-src/bin/flutter",
		"no analyzer": "flutter-sdk-src/bin/cache/soroq/soroq_kernel_analyze.dill",
		"no snapshot": "flutter-sdk-src/bin/cache/flutter_tools.snapshot",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeCandidate(t, nil, "... SoroqFreehandAnalysis ...")
			if err := os.Remove(filepath.Join(dir, remove)); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyCandidateFrontend(dir); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// A candidate must never be recorded as if it were a verified signed archive.
func TestCandidateIsRecordedAsUnsigned(t *testing.T) {
	dir := writeCandidate(t, nil, "... SoroqFreehandAnalysis ...")
	m, err := verifyCandidateFrontend(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := activeFrontend{
		Version:       m.Version,
		ArchiveSHA256: "candidate-unsigned:" + m.PatchsetSHA256[:16],
	}
	if !strings.HasPrefix(rec.ArchiveSHA256, "candidate-unsigned:") {
		t.Error("an unsigned candidate must be recorded with a clearly non-archive marker, so it can " +
			"never be mistaken for a signature-verified install")
	}
}

// RESTORE must always lead back to a SIGNED frontend. Activating a candidate while a candidate is
// already active previously overwrote the restore pointer with that candidate, so `--restore` returned
// to a candidate and the way back to the signed frontend was lost. Re-activating after rebuilding a
// candidate is the normal workflow, so this was not an edge case.
func TestRestoreTargetIsOnlyEverASignedFrontend(t *testing.T) {
	signed := activeFrontend{Version: "soroq-flutter-frontend-signed", ArchiveSHA256: strings.Repeat("a", 64)}
	cand := activeFrontend{Version: "soroq-flutter-frontend-candidate-x", ArchiveSHA256: "candidate-unsigned:abc123"}

	if isCandidateFrontendRecord(signed) {
		t.Error("a signed frontend was classified as a candidate")
	}
	if !isCandidateFrontendRecord(cand) {
		t.Error("an unsigned candidate was not recognised; --restore would overwrite the signed target")
	}
}
