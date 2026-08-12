package signing

import (
	"encoding/json"
	"strings"
	"testing"
)

func okBindingPtr() *EngineRollbackBinding { b := okBinding(); return &b }

func okBinding() EngineRollbackBinding {
	return EngineRollbackBinding{
		AppID:     "dev.soroq.app",
		Channel:   "stable",
		ReleaseID: "rel-1",
		RuntimeID: strings.Repeat("a", 64),
	}
}

func rollbackBytes(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	m := map[string]any{
		"version":        0,
		"runtime_id":     strings.Repeat("a", 64),
		"bytecodeSha256": "",
		"patches":        []any{},
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The happy path: a genuine rollback signs.
func TestGenuineRollbackIsAccepted(t *testing.T) {
	if err := AssertEngineRollbackManifest(rollbackBytes(t, nil), okBindingPtr()); err != nil {
		t.Fatalf("a real rollback must be signable: %v", err)
	}
}

// Anything not unambiguously a rollback must never reach a signature.
func TestMalformedRollbackIsRefusedBeforeSigning(t *testing.T) {
	cases := map[string]func(map[string]any){
		"non-zero version":   func(m map[string]any) { m["version"] = 1 },
		"missing version":    func(m map[string]any) { delete(m, "version") },
		"carries a patch":    func(m map[string]any) { m["patches"] = []any{map[string]any{"bytecode": "x"}} },
		"names a payload":    func(m map[string]any) { m["bytecodeSha256"] = strings.Repeat("b", 64) },
		"carries an ABI":     func(m map[string]any) { m["replacementAbi"] = []any{map[string]any{"base_identity": "a"}} },
		"carries a contract": func(m map[string]any) { m["entrypointContract"] = "freehand_identity_v1" },
		"dependency digest":  func(m map[string]any) { m["dependencyDescriptorDigest"] = strings.Repeat("c", 64) },
		"foreign runtime":    func(m map[string]any) { m["runtime_id"] = strings.Repeat("f", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := AssertEngineRollbackManifest(rollbackBytes(t, mutate), okBindingPtr()); err == nil {
				t.Fatalf("malformed rollback (%s) would have been signed", name)
			}
		})
	}
}

func TestTrailingDataIsRefused(t *testing.T) {
	raw := append(rollbackBytes(t, nil), []byte("{}")...)
	if err := AssertEngineRollbackManifest(raw, okBindingPtr()); err == nil {
		t.Error("trailing JSON after the manifest was accepted")
	}
}

// RUNTIME BINDING. The production CLI takes --runtime-id as free text, so a typo must be caught
// here: a correctly-signed rollback bound to a nonexistent runtime is served to nobody and silently
// fails to un-brick the devices it was meant to save.
func TestRollbackBindingFailsClosed(t *testing.T) {
	for name, mut := range map[string]func(*EngineRollbackBinding){
		"no app":            func(b *EngineRollbackBinding) { b.AppID = "" },
		"no channel":        func(b *EngineRollbackBinding) { b.Channel = " " },
		"no release":        func(b *EngineRollbackBinding) { b.ReleaseID = "" },
		"no runtime":        func(b *EngineRollbackBinding) { b.RuntimeID = "" },
		"malformed runtime": func(b *EngineRollbackBinding) { b.RuntimeID = "not-a-runtime-id" },
		"uppercase runtime": func(b *EngineRollbackBinding) { b.RuntimeID = strings.Repeat("A", 64) },
		"short runtime":     func(b *EngineRollbackBinding) { b.RuntimeID = strings.Repeat("a", 63) },
	} {
		t.Run(name, func(t *testing.T) {
			b := okBinding()
			mut(&b)
			if err := AssertEngineRollbackManifest(rollbackBytes(t, nil), &b); err == nil {
				t.Fatalf("binding %q was accepted; the rollback must fail closed", name)
			}
		})
	}
}

// A LOCAL emit (`--out` with no `--api`) has no control-plane target, so it must still assert the
// manifest shape but must not demand app/release ids that do not exist in that mode.
func TestLocalEmitAssertsShapeWithoutABinding(t *testing.T) {
	if err := AssertEngineRollbackManifest(rollbackBytes(t, nil), nil); err != nil {
		t.Fatalf("a local rollback emit must be allowed without a binding: %v", err)
	}
	bad := rollbackBytes(t, func(m map[string]any) { m["version"] = 2 })
	if err := AssertEngineRollbackManifest(bad, nil); err == nil {
		t.Error("shape assertions must still apply to a local emit")
	}
}
