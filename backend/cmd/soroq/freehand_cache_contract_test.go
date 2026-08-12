package main

// THE CACHE CONTRACT between Flutter's build DAG and baseline persistence.
//
// SoroqFreehandAnalysis is a cached Flutter target. Baseline persistence does NOT trust that cache: it
// recomputes a content address from live inputs (app.dill, the analyzer snapshot, package_config.json,
// the schema and the config digest) and demands the immutable outputs at exactly that address.
//
// The two keyings can disagree — the target looks up to date while the outputs for the recomputed
// address are absent. That is what produced "no immutable analysis for recomputed content address
// 50e7d818…" on a second release, whose only known cure was `flutter clean`.
//
// The underlying cause was Soroq's own doing: it mutated the customer's pubspec and re-ran pub get, so
// package_config.json — an input to the address — changed between runs. That is fixed at the source
// (see soroq_package_config.go). These tests pin the DEFENSIVE half: when the outputs are missing for
// any reason, the build fails closed and invalidates the cache entry so a plain re-run regenerates it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingReceiptInvalidatesTheAnalysisStampAndFailsClosed(t *testing.T) {
	buildDir := t.TempDir()
	appDill := filepath.Join(buildDir, "app.dill")
	mustWriteFile(t, appDill, "not a real dill, only its bytes matter")
	mustWriteFile(t, filepath.Join(buildDir, "soroq_freehand_analysis.stamp"), "{}")

	projectDir := t.TempDir()
	mustWriteFile(t, filepath.Join(projectDir, ".dart_tool", "package_config.json"), `{"configVersion":2}`)

	// No soroq_freehand/<addr>/soroq_analysis_receipt.json exists at all.
	_, _, _, err := verifyFreehandStagingStrict(projectDir, appDill, "analyzer-sha")
	if err == nil {
		t.Fatal("a missing analysis receipt was accepted; a baseline would be persisted from unverified outputs")
	}
	msg := err.Error()
	for _, want := range []string{"no verified freehand analysis", "invalidated", "Re-run", "do NOT `flutter clean`"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is not actionable, missing %q:\n%s", want, msg)
		}
	}
	// THE RECOVERY: the stamp is gone, so the next build re-runs the analysis without a clean.
	if _, statErr := os.Stat(filepath.Join(buildDir, "soroq_freehand_analysis.stamp")); !os.IsNotExist(statErr) {
		t.Error("the analysis stamp survived; the next build would hit the same cached-but-missing state")
	}
}

// Invalidation must be safe and idempotent when there is no stamp to remove.
func TestInvalidationIsSafeWhenNoStampExists(t *testing.T) {
	buildDir := t.TempDir()
	appDill := filepath.Join(buildDir, "app.dill")
	got := invalidateFreehandAnalysisStamp(appDill)
	if !strings.Contains(got, "No analysis cache entry") {
		t.Errorf("unexpected description: %q", got)
	}
	// Second call must behave identically rather than erroring.
	if second := invalidateFreehandAnalysisStamp(appDill); second != got {
		t.Errorf("invalidation is not idempotent: %q then %q", got, second)
	}
}

// A TAMPERED receipt must be refused too — a receipt that parses but does not match is not weaker than
// a missing one, and must never be accepted just because a file is present.
func TestTamperedReceiptIsRefused(t *testing.T) {
	buildDir := t.TempDir()
	appDill := filepath.Join(buildDir, "app.dill")
	mustWriteFile(t, appDill, "dill bytes")
	projectDir := t.TempDir()
	mustWriteFile(t, filepath.Join(projectDir, ".dart_tool", "package_config.json"), `{"configVersion":2}`)

	// Compute the address the verifier will look for, then plant a receipt with the WRONG contents there.
	appDillSha, err := sha256OfPath(appDill)
	if err != nil {
		t.Fatal(err)
	}
	pkgSha, err := sha256OfPath(filepath.Join(projectDir, ".dart_tool", "package_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	addr := freehandContentAddr(appDillSha, "analyzer-sha", pkgSha, freehandIdentitySchema, freehandConfigDigest())
	dir := filepath.Join(buildDir, "soroq_freehand", addr)
	mustWriteFile(t, filepath.Join(dir, "soroq_analysis_receipt.json"),
		`{"schema":"WRONG","mode":"WRONG","identity_schema":"x","analysis_id":"x","app_dill_sha256":"x",`+
			`"manifest_sha256":"x","symbol_graph_sha256":"x","analyzer_snapshot_sha256":"x",`+
			`"package_config_sha256":"x","config_digest":"x"}`)

	if _, _, _, err := verifyFreehandStagingStrict(projectDir, appDill, "analyzer-sha"); err == nil {
		t.Fatal("a tampered receipt was accepted")
	}
}
