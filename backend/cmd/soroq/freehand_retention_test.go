package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Release path: persisting a freehand baseline WITHOUT verified retention evidence (as a plain/reused
// `flutter build` would produce — no analysis staging) must fail closed.
func TestRetention_MissingEvidenceRefusesRelease(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	m := fullMeta()
	m.Retention = nil
	_, err := persistFreehandBaseline(proj, m, dill, srcDill, man, graph, testDepGraph())
	if err == nil || !strings.Contains(err.Error(), "retention") {
		t.Fatalf("release must refuse a base without verified retention, got %v", err)
	}
}

// Release path: the only caller-supplied retention fields are Verified and AnalysisID (count and the
// manifest/symbol-graph SHAs are DERIVED from the validated inputs). Both must be sound or persist
// fails closed BEFORE the baseline is written.
func TestRetention_ReleaseRefusesTamperedCallerFields(t *testing.T) {
	t.Run("unverified", func(t *testing.T) {
		proj, dill, srcDill, man, graph := seedFixture(t)
		m := fullMeta()
		m.Retention = &FreehandRetentionEvidence{Verified: false, AnalysisID: strings.Repeat("a", 64)}
		if _, err := persistFreehandBaseline(proj, m, dill, srcDill, man, graph, testDepGraph()); err == nil || !strings.Contains(err.Error(), "not verified") {
			t.Fatalf("release must refuse unverified retention, got %v", err)
		}
	})
	t.Run("non-hex analysis_id", func(t *testing.T) {
		proj, dill, srcDill, man, graph := seedFixture(t)
		m := fullMeta()
		m.Retention = &FreehandRetentionEvidence{Verified: true, AnalysisID: "not-a-content-address"}
		if _, err := persistFreehandBaseline(proj, m, dill, srcDill, man, graph, testDepGraph()); err == nil || !strings.Contains(err.Error(), "analysis_id") {
			t.Fatalf("release must refuse a non-content-addressed analysis_id, got %v", err)
		}
	})
}

// The immutable baseline receipt records verified retention (count + manifest hash), derived — never
// caller-supplied — from the validated manifest.
func TestRetention_RecordedInBaselineReceipt(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got FreehandBaselineMeta
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Retention == nil || !got.Retention.Verified {
		t.Fatalf("baseline receipt must record verified retention, got %+v", got.Retention)
	}
	if got.Retention.RetainedIdentities != got.PatchableCount || got.Retention.RetainedIdentities <= 0 {
		t.Fatalf("recorded retained identities %d must equal patchable_symbols %d and be > 0", got.Retention.RetainedIdentities, got.PatchableCount)
	}
	if got.Retention.ManifestSHA256 != got.ManifestSHA256 || got.Retention.ManifestSHA256 == "" {
		t.Fatalf("recorded retention manifest sha must match + be non-empty")
	}
}

// validAddr is a well-formed 64-hex content address for the analysis staging.
const validAddr = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// goodRetention returns evidence internally consistent with base (manifest sha, symbol-graph sha,
// count) so a single field can be tampered per case.
func goodRetention() *FreehandRetentionEvidence {
	return &FreehandRetentionEvidence{
		Verified:           true,
		RetainedIdentities: 3,
		ManifestSHA256:     "abc",
		SymbolGraphSHA256:  "graph-sha",
		AnalysisID:         validAddr,
	}
}

// The single retention gate used by BOTH the release and patch paths. Every nested field is load-bearing.
func TestRetention_RequireGateCases(t *testing.T) {
	newBase := func() *FreehandBaselineMeta {
		return &FreehandBaselineMeta{ManifestSHA256: "abc", GraphSHA256: "graph-sha", PatchableCount: 3}
	}

	// nil evidence
	base := newBase()
	base.Retention = nil
	if err := requireFreehandRetention(base); err == nil {
		t.Fatal("nil retention must fail closed")
	}

	// unverified
	base = newBase()
	base.Retention = goodRetention()
	base.Retention.Verified = false
	if err := requireFreehandRetention(base); err == nil || !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("unverified retention must fail: %v", err)
	}

	// zero identities
	base = newBase()
	base.Retention = goodRetention()
	base.Retention.RetainedIdentities = 0
	if err := requireFreehandRetention(base); err == nil || !strings.Contains(err.Error(), "ZERO") {
		t.Fatalf("zero-identity retention must fail: %v", err)
	}

	// manifest sha mismatch
	base = newBase()
	base.Retention = goodRetention()
	base.Retention.ManifestSHA256 = "WRONG"
	if err := requireFreehandRetention(base); err == nil || !strings.Contains(err.Error(), "manifest sha") {
		t.Fatalf("manifest-sha-inconsistent retention must fail: %v", err)
	}

	// symbol-graph sha mismatch
	base = newBase()
	base.Retention = goodRetention()
	base.Retention.SymbolGraphSHA256 = "WRONG"
	if err := requireFreehandRetention(base); err == nil || !strings.Contains(err.Error(), "symbol_graph sha") {
		t.Fatalf("symbol-graph-sha-inconsistent retention must fail: %v", err)
	}

	// empty symbol-graph sha
	base = newBase()
	base.Retention = goodRetention()
	base.Retention.SymbolGraphSHA256 = ""
	if err := requireFreehandRetention(base); err == nil || !strings.Contains(err.Error(), "symbol_graph sha") {
		t.Fatalf("empty symbol-graph sha must fail: %v", err)
	}

	// non-content-addressed analysis_id (empty)
	base = newBase()
	base.Retention = goodRetention()
	base.Retention.AnalysisID = ""
	if err := requireFreehandRetention(base); err == nil || !strings.Contains(err.Error(), "analysis_id") {
		t.Fatalf("empty analysis_id must fail: %v", err)
	}

	// non-content-addressed analysis_id (not 64-hex)
	base = newBase()
	base.Retention = goodRetention()
	base.Retention.AnalysisID = "test-analysis"
	if err := requireFreehandRetention(base); err == nil || !strings.Contains(err.Error(), "analysis_id") {
		t.Fatalf("non-hex analysis_id must fail: %v", err)
	}

	// count != patchable_symbols
	base = newBase()
	base.Retention = goodRetention()
	base.Retention.RetainedIdentities = 2
	if err := requireFreehandRetention(base); err == nil || !strings.Contains(err.Error(), "!= baseline patchable_symbols") {
		t.Fatalf("count mismatch must fail: %v", err)
	}

	// complete + consistent passes
	base = newBase()
	base.Retention = goodRetention()
	if err := requireFreehandRetention(base); err != nil {
		t.Fatalf("complete, consistent retention must pass: %v", err)
	}
}

// Existing-baseline reuse (verifyExistingBaseline) must fail closed if retention is removed or ANY
// nested retention field is tampered — before any patch delegate runs.
func TestRetention_VerifyExistingBaselineFailsOnTamper(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	baselinePath := filepath.Join(relDir, "baseline.json")

	// A clean reuse succeeds.
	if _, err := verifyExistingBaseline(relDir); err != nil {
		t.Fatalf("clean baseline must reuse: %v", err)
	}

	// mutate rewrites baseline.json with a mutated retention block and asserts reuse fails.
	mutate := func(name string, f func(m *FreehandBaselineMeta)) {
		raw, err := os.ReadFile(baselinePath)
		if err != nil {
			t.Fatal(err)
		}
		var m FreehandBaselineMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		f(&m)
		out, err := json.Marshal(&m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(baselinePath, out, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyExistingBaseline(relDir); err == nil {
			t.Fatalf("%s: verifyExistingBaseline must refuse the tampered/absent retention", name)
		}
		// restore for the next case
		if err := os.WriteFile(baselinePath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mutate("retention removed", func(m *FreehandBaselineMeta) { m.Retention = nil })
	mutate("verified cleared", func(m *FreehandBaselineMeta) { m.Retention.Verified = false })
	mutate("count zeroed", func(m *FreehandBaselineMeta) { m.Retention.RetainedIdentities = 0 })
	mutate("count skewed", func(m *FreehandBaselineMeta) { m.Retention.RetainedIdentities++ })
	mutate("manifest sha tampered", func(m *FreehandBaselineMeta) { m.Retention.ManifestSHA256 = "deadbeef" })
	mutate("symbol-graph sha tampered", func(m *FreehandBaselineMeta) { m.Retention.SymbolGraphSHA256 = "deadbeef" })
	mutate("analysis_id cleared", func(m *FreehandBaselineMeta) { m.Retention.AnalysisID = "" })
	mutate("analysis_id non-hex", func(m *FreehandBaselineMeta) { m.Retention.AnalysisID = "not-a-content-address" })
}

// immutableInputsEqual must treat any nested retention change as a different immutable input, so a
// release re-registration with tampered retention can never be accepted as an idempotent re-run.
func TestRetention_ImmutableInputsIncludeEveryRetentionField(t *testing.T) {
	base := fullMeta()
	base.Retention = &FreehandRetentionEvidence{Verified: true, RetainedIdentities: 3, ManifestSHA256: "m", SymbolGraphSHA256: "g", AnalysisID: validAddr}

	if !immutableInputsEqual(&base, &base) {
		t.Fatal("identical inputs must compare equal")
	}

	cases := []struct {
		name string
		mut  func(r *FreehandRetentionEvidence)
	}{
		{"verified", func(r *FreehandRetentionEvidence) { r.Verified = false }},
		{"retained_identities", func(r *FreehandRetentionEvidence) { r.RetainedIdentities = 99 }},
		{"manifest_sha256", func(r *FreehandRetentionEvidence) { r.ManifestSHA256 = "x" }},
		{"symbol_graph_sha256", func(r *FreehandRetentionEvidence) { r.SymbolGraphSHA256 = "x" }},
		{"analysis_id", func(r *FreehandRetentionEvidence) { r.AnalysisID = strings.Repeat("b", 64) }},
	}
	for _, c := range cases {
		b := fullMeta()
		rr := *base.Retention
		c.mut(&rr)
		b.Retention = &rr
		if immutableInputsEqual(&base, &b) {
			t.Fatalf("tampering retention.%s must break immutable-input equality", c.name)
		}
	}

	// nil vs non-nil must differ; nil vs nil equal (handled by retentionEqual).
	b := base
	b.Retention = nil
	if immutableInputsEqual(&base, &b) {
		t.Fatal("nil vs non-nil retention must differ")
	}
}

// Patch path: a verified-retention base is patchable; a base whose evidence is absent is refused (the
// same guard computeFreehandPatchPlan applies right after verifyExistingBaseline).
func TestRetention_PatchRefusesBaseWithoutEvidence(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	base, err := verifyExistingBaseline(relDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireFreehandRetention(base); err != nil {
		t.Fatalf("a verified-retention base must be patchable: %v", err)
	}
	base.Retention = nil // a plain/reused base carries no evidence
	if err := requireFreehandRetention(base); err == nil {
		t.Fatal("patch must refuse a base without retention evidence")
	}
}
