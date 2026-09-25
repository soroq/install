package androidpatch

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestObfuscationSeedDeltaSizes measures, with the production SRQCDL15 codec, the Android patch a
// candidate needs against its base when obfuscation is fresh (stock R5 gen_snapshot) versus seeded
// from the base's map (Option A, handoff 14). It runs only against a seed_proof.sh output directory
// (SOROQ_SEED_PROOF_DIR) and writes delta-sizes.json there for seed_gates.py.
func TestObfuscationSeedDeltaSizes(t *testing.T) {
	dir := os.Getenv("SOROQ_SEED_PROOF_DIR")
	if dir == "" {
		t.Skip("SOROQ_SEED_PROOF_DIR not set")
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	measure := func(base, cand string) int {
		baseBytes, candBytes := read(base), read(cand)
		delta, _, err := buildCodeDeltaV15(baseBytes, candBytes)
		if err != nil {
			t.Fatalf("%s -> %s: %v", base, cand, err)
		}
		rebuilt, err := applyCodeDeltaV15(baseBytes, delta)
		if err != nil || !bytes.Equal(rebuilt, candBytes) {
			t.Fatalf("%s -> %s: delta does not reconstruct the candidate (err=%v)", base, cand, err)
		}
		return len(delta)
	}
	sizes := map[string]int{
		"stock_r5": measure("r5-base.stripped.so", "r5-candidate.stripped.so"),
		"seeded":   measure("new-base.stripped.so", "seeded-candidate.stripped.so"),
	}
	out, _ := json.MarshalIndent(sizes, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "delta-sizes.json"), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("delta sizes: %s", out)
}
