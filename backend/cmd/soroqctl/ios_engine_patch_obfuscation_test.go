package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CONTROL: the indexed engine-lane route must refuse an obfuscated base rather than produce a patch
// that installs and resolves nothing.
func TestControlLegacyRouteRefusesObfuscatedBase(t *testing.T) {
	dir := t.TempDir()
	baseline := filepath.Join(dir, "soroq-ios-engine-baseline.json")
	if err := os.WriteFile(baseline, []byte(`{"schema":"soroq.ios_engine_baseline.v2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// BASELINE: an ordinary base passes straight through.
	if err := refuseObfuscatedBaseOnLegacyRoute(baseline); err != nil {
		t.Fatalf("an ordinary base must not be refused: %v", err)
	}
	if err := refuseObfuscatedBaseOnLegacyRoute(""); err != nil {
		t.Fatalf("no baseline path must not be refused here: %v", err)
	}

	t.Run("captured_map_beside_the_baseline", func(t *testing.T) {
		p := filepath.Join(dir, "base_obfuscation_map.json")
		if err := os.WriteFile(p, []byte(`["a","b"]`), 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(p)
		err := refuseObfuscatedBaseOnLegacyRoute(baseline)
		if err == nil {
			t.Fatal("control did not fire: an obfuscated base was accepted on the indexed route")
		}
		if !strings.Contains(err.Error(), "soroq patch ios --engine") {
			t.Fatalf("the refusal must name the lane that does support it: %v", err)
		}
	})

	t.Run("freehand_baseline_declares_obfuscation", func(t *testing.T) {
		p := filepath.Join(dir, "baseline.json")
		if err := os.WriteFile(p, []byte(`{"schema":"soroq.freehand.baseline.v2","obfuscation":{"enabled":true}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(p)
		if err := refuseObfuscatedBaseOnLegacyRoute(baseline); err == nil {
			t.Fatal("control did not fire: a freehand baseline declaring obfuscation was accepted")
		}
	})
}
