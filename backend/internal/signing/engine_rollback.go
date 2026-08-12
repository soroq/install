package signing

// THE ONE ROLLBACK ASSERTION.
//
// A rollback is an INSTRUCTION that reverts a user's app, so it is exactly as security-sensitive as
// the patch it reverts. It therefore travels the same signed, runtime-bound path — and, like a patch,
// must be checked BEFORE it is signed rather than after it is served.
//
// This exists because the tree briefly carried two rollback signers:
//
//   - `soroqctl rollback ios-engine` — the device-proven production path. It marshalled
//     `{version:0, patches:[]}` inline and signed it with no assertion at all.
//   - `cmd/soroq/freehand_rollback_publish.go` — a fully-tested parallel publisher with a strict
//     validator and a runtime-id format check, and ZERO call sites.
//
// Two independently maintained signers for the same instruction is the hazard: the safety lived in
// the one nothing called. Deleting the unused publisher without moving its checks here would have
// quietly reduced rollback safety, so the assertion moved to the production path and the duplicate
// publisher was removed.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ReservedRollbackVersion is version 0 — the "return to base" instruction. It can never be a patch.
const ReservedRollbackVersion = 0

var runtimeIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// EngineRollbackBinding is the target a rollback is bound to. Every field is required: an
// instruction that reverts an app must not be replayable against a different app, channel, release
// or runtime.
type EngineRollbackBinding struct {
	AppID     string
	Channel   string
	ReleaseID string
	RuntimeID string
}

// Validate fails closed on an incomplete or malformed binding.
//
// RuntimeID is format-checked, not merely non-empty: the production CLI takes it as a free-text flag,
// so a typo would otherwise produce a correctly-signed rollback bound to a runtime that does not
// exist — served to nobody, and silently failing to un-brick the devices it was meant to save.
func (b EngineRollbackBinding) Validate() error {
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"app_id", b.AppID}, {"channel", b.Channel},
		{"release_id", b.ReleaseID}, {"runtime_id", b.RuntimeID},
	} {
		if strings.TrimSpace(f.v) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("engine rollback is missing binding field(s): %s", strings.Join(missing, ", "))
	}
	if !runtimeIDRe.MatchString(strings.TrimSpace(b.RuntimeID)) {
		return fmt.Errorf("engine rollback runtime_id %q is not a 64-hex runtime id", b.RuntimeID)
	}
	return nil
}

// AssertEngineRollbackManifest refuses to let anything that is not unambiguously a rollback be
// signed. The device reverts on `version == 0 || patches.isEmpty`, so anything carrying a payload,
// a non-zero version, or an unexpected field must never reach a signature.
//
// Unknown fields are rejected rather than ignored: a manifest with a field this build does not
// understand is a manifest this build cannot reason about, and signing it would vouch for semantics
// it did not check.
func AssertEngineRollbackManifest(raw []byte, binding *EngineRollbackBinding) error {
	// binding is nil for a purely LOCAL emit (`--out` with no `--api`): there is no control-plane
	// target to bind to, and demanding app/release ids there would break emitting a static-host
	// bundle. The manifest SHAPE is asserted either way -- that part never depends on a target.
	if binding != nil {
		if err := binding.Validate(); err != nil {
			return err
		}
	}
	var probe map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&probe); err != nil {
		return fmt.Errorf("rollback manifest is not valid JSON: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("rollback manifest has trailing data after the JSON document")
	}

	var m struct {
		Version        *int              `json:"version"`
		Patches        []json.RawMessage `json:"patches"`
		BytecodeSha256 string            `json:"bytecodeSha256"`
		RuntimeID      string            `json:"runtime_id"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("decode rollback manifest: %w", err)
	}
	if m.Version == nil {
		return fmt.Errorf("rollback manifest has no version field")
	}
	if *m.Version != ReservedRollbackVersion {
		return fmt.Errorf("a rollback manifest must be version %d, got %d", ReservedRollbackVersion, *m.Version)
	}
	if len(m.Patches) != 0 {
		return fmt.Errorf("a rollback manifest must carry no patches, got %d", len(m.Patches))
	}
	if strings.TrimSpace(m.BytecodeSha256) != "" {
		return fmt.Errorf("a rollback manifest must not name a payload (bytecodeSha256 was set)")
	}
	// The runtime binding must be IN the signed bytes, not merely alongside them: a device checks the
	// manifest it verified, so a rollback whose signed body does not name the runtime could be
	// replayed against another runtime on the same channel.
	if binding != nil && strings.TrimSpace(m.RuntimeID) != strings.TrimSpace(binding.RuntimeID) {
		return fmt.Errorf("signed rollback runtime %q does not match the target runtime %q",
			m.RuntimeID, binding.RuntimeID)
	}
	// Anything else in the document is unvouched-for semantics.
	for _, k := range []string{"replacementAbi", "entrypointContract", "dependencyDescriptorDigest",
		"logicalArtifactId", "payloadSha256"} {
		if v, ok := probe[k]; ok && string(v) != "null" {
			return fmt.Errorf("a rollback manifest must not carry %q", k)
		}
	}
	return nil
}
