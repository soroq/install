package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A baseline written before the widened contract must be refused with the EXACT actionable message.
func TestOldBaseWithoutContract_RefusedWithExactMessage(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	blPath := filepath.Join(relDir, "baseline.json")
	raw, err := os.ReadFile(blPath)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	delete(probe, "contract_schema")
	delete(probe, "contract_digest")
	out, _ := json.MarshalIndent(probe, "", "  ")
	_ = os.Chmod(blPath, 0o600)
	if err := os.WriteFile(blPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = verifyExistingBaseline(relDir)
	if err == nil {
		t.Fatal("a base with no recorded contract must be refused")
	}
	const want = "This base predates generic Dart-dependency patching; create one new store release."
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal must be exactly %q, got: %v", want, err)
	}
}
