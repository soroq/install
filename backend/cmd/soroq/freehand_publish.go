package main

// Soroq freehand — Part 5 signed delivery contract (freehand_identity_v1).
//
// Turns the immutable Step-4 artifact (source + bytecode + durable ABI + metadata) into a
// DEVICE-VERIFIABLE, Ed25519-signed engine-lane manifest + payload the soroq_flutter controller consumes
// via its freehand identity path. Local/mock emission only — NO production publish here.
//
// Schema note (deliberate, do not "fix" into consistency): the TOP-LEVEL manifest keys are camelCase to
// match the existing indexed engine-lane manifest the store already serves verbatim (version,
// bytecodeSha256, entrypointContract, patches). The replacement_abi ENTRY keys are snake_case to match the
// durable ABI (soroq_freehand_module_manifest.json) AND the controller's `_flatSpecsFromAbi` parser, so the
// entries are copied over the SAME keys the controller reads — no rename, no drift.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// freehandDeviceContract is the entrypoint-contract discriminator. Only this value routes to the freehand
// identity path in the controller; nothing else does, and an indexed manifest never carries it.
const freehandDeviceContract = "freehand_identity_v1"

// freehandDeviceABIEntry is a device-facing replacement-ABI entry — a strict projection over the SAME keys
// the durable ABI emits and the controller parses. `signature_sha256` is intentionally omitted (supplemental
// per Step 4: the frozen `stable_identity` is authoritative and the value is not device-recomputable).
type freehandDeviceABIEntry struct {
	BaseIdentity   string `json:"base_identity"`
	StableIdentity string `json:"stable_identity"`
	ModuleLibrary  string `json:"module_library"`
	ModuleClass    string `json:"module_class"`
	ModuleMember   string `json:"module_member"`
	Kind           string `json:"kind"`
}

// freehandDevicePatch carries the shared bytecode filename with NO numeric index — so an old INDEXED client
// that ignores entrypointContract and reaches its numeric-index path fails closed (index missing) instead of
// misapplying a freehand payload as an indexed one.
type freehandDevicePatch struct {
	Bytecode string `json:"bytecode"`
}

// FreehandDeviceManifest is the signed device manifest for the freehand engine lane.
type FreehandDeviceManifest struct {
	Version              int                      `json:"version"`
	RuntimeID            string                   `json:"runtime_id"`
	BytecodeSha256       string                   `json:"bytecodeSha256"` // the payload SHA the signature binds + the controller hash-checks
	EntrypointContract   string                   `json:"entrypointContract"`
	Patches              []freehandDevicePatch    `json:"patches"`
	ReplacementABI       []freehandDeviceABIEntry `json:"replacementAbi"`
	LogicalArtifactID    string                   `json:"logicalArtifactId"`    // reproducible SOURCE identity (artifact_id)
	ModuleBytecodeSha256 string                   `json:"moduleBytecodeSha256"` // == bytecodeSha256 (contract alias)
	PayloadSha256        string                   `json:"payloadSha256"`        // == bytecodeSha256 (contract alias; bytecode IS the payload)
	// DependencyDescriptorDigest binds the RUNTIME dependency delta this patch was built under into the
	// SIGNED metadata, so the dependency change is covered by the Ed25519 signature rather than being a
	// build-time-only claim. Empty for a patch that changed no dependencies. The on-device controller
	// decodes non-strictly, so an older client simply ignores the field and still fails closed on the
	// checks it does understand.
	DependencyDescriptorDigest string `json:"dependencyDescriptorDigest,omitempty"`
}

// buildFreehandDeviceManifest projects an immutable Step-4 artifact into a device manifest. It re-derives
// the bytecode SHA from the actual file, copies the durable ABI entries VERBATIM (same keys), and binds the
// logical (source) artifact id. Fails closed on a missing/inconsistent artifact.
func buildFreehandDeviceManifest(artifactDir string, version int, bytecodeName string) (FreehandDeviceManifest, []byte, error) {
	var zero FreehandDeviceManifest
	if version <= 0 {
		return zero, nil, fmt.Errorf("freehand device manifest version must be > 0, got %d", version)
	}
	// Strictly re-verify the artifact first (all files present, hashes + ABI bijection consistent).
	metaRaw, err := os.ReadFile(filepath.Join(artifactDir, "patch_artifact.json"))
	if err != nil {
		return zero, nil, err
	}
	var meta FreehandPatchArtifactMeta
	dec := json.NewDecoder(bytes.NewReader(metaRaw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil {
		return zero, nil, fmt.Errorf("decode patch_artifact.json: %w", err)
	}
	if err := verifyExistingPatchArtifact(artifactDir, meta.ArtifactID); err != nil {
		return zero, nil, fmt.Errorf("refusing to publish an invalid artifact: %w", err)
	}
	bytecodePath := filepath.Join(artifactDir, "soroq_freehand_module.bytecode")
	bytecodeBytes, err := os.ReadFile(bytecodePath)
	if err != nil {
		return zero, nil, err
	}
	sum := sha256.Sum256(bytecodeBytes)
	bytecodeSHA := hex.EncodeToString(sum[:])
	if bytecodeSHA != meta.ModuleBytecodeSHA256 {
		return zero, nil, fmt.Errorf("bytecode sha %s != artifact record %s", bytecodeSHA, meta.ModuleBytecodeSHA256)
	}

	// Project the durable ABI entries verbatim (same keys the controller parses).
	manifestRaw, err := os.ReadFile(filepath.Join(artifactDir, "soroq_freehand_module_manifest.json"))
	if err != nil {
		return zero, nil, err
	}
	var durable freehandModuleManifest
	if err := json.Unmarshal(manifestRaw, &durable); err != nil {
		return zero, nil, fmt.Errorf("decode durable module manifest: %w", err)
	}
	if len(durable.ReplacementABI) == 0 {
		return zero, nil, errors.New("durable manifest has an empty replacement ABI")
	}
	abi := make([]freehandDeviceABIEntry, 0, len(durable.ReplacementABI))
	for _, e := range durable.ReplacementABI {
		abi = append(abi, freehandDeviceABIEntry{
			BaseIdentity:   e.BaseIdentity,
			StableIdentity: e.StableIdentity,
			ModuleLibrary:  e.ModuleLibrary,
			ModuleClass:    e.ModuleClass,
			ModuleMember:   e.ModuleMember,
			Kind:           e.Kind,
		})
	}

	m := FreehandDeviceManifest{
		Version:              version,
		RuntimeID:            meta.RuntimeID,
		BytecodeSha256:       bytecodeSHA,
		EntrypointContract:   freehandDeviceContract,
		Patches:              []freehandDevicePatch{{Bytecode: bytecodeName}},
		ReplacementABI:       abi,
		LogicalArtifactID:    meta.ArtifactID,
		ModuleBytecodeSha256: bytecodeSHA,
		PayloadSha256:        bytecodeSHA,
		// Bound from the artifact record, which verifyExistingPatchArtifact above already re-derived from
		// the persisted descriptor file and cross-checked against patch_plan.json.
		DependencyDescriptorDigest: meta.DependencyDescriptorDigest,
	}
	// Validate the manifest is internally consistent + device-strict before signing.
	manifestBytes, err := json.Marshal(m)
	if err != nil {
		return zero, nil, err
	}
	if err := validateFreehandDeviceManifest(manifestBytes); err != nil {
		return zero, nil, err
	}
	return m, bytecodeBytes, nil
}

// validateFreehandDeviceManifest strictly decodes a device manifest (STRICT: unknown/trailing JSON refused
// at every level, incl. ABI entries) and enforces the contract: correct discriminator, one bytecode with no
// index, non-empty well-formed ABI, and the bytecode/payload SHA aliases all equal the bound bytecodeSha256.
// This is the PRODUCER/verifier strictness (item 4); the on-device controller decode stays non-strict so an
// OLD client ignores unknown fields and fails closed rather than hard-erroring.
func validateFreehandDeviceManifest(manifestBytes []byte) error {
	dec := json.NewDecoder(bytes.NewReader(manifestBytes))
	dec.DisallowUnknownFields()
	var m FreehandDeviceManifest
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("strict device manifest decode: %w", err)
	}
	if dec.More() {
		return errors.New("trailing data after device manifest JSON")
	}
	if m.EntrypointContract != freehandDeviceContract {
		return fmt.Errorf("freehand device manifest has wrong entrypointContract %q", m.EntrypointContract)
	}
	if m.Version <= 0 {
		return fmt.Errorf("freehand device manifest version must be > 0")
	}
	if strings.TrimSpace(m.RuntimeID) == "" {
		return errors.New("freehand device manifest missing runtime_id")
	}
	if !sha256HexRe.MatchString(m.BytecodeSha256) {
		return errors.New("freehand device manifest bytecodeSha256 malformed")
	}
	if m.ModuleBytecodeSha256 != m.BytecodeSha256 || m.PayloadSha256 != m.BytecodeSha256 {
		return errors.New("freehand device manifest bytecode/payload SHA aliases must equal bytecodeSha256")
	}
	if m.LogicalArtifactID == "" {
		return errors.New("freehand device manifest missing logicalArtifactId")
	}
	if len(m.Patches) != 1 || m.Patches[0].Bytecode == "" {
		return errors.New("freehand device manifest must carry exactly one named bytecode patch")
	}
	if len(m.ReplacementABI) == 0 {
		return errors.New("freehand device manifest has an empty replacementAbi")
	}
	// Re-decode the ABI entries STRICTLY (unknown entry fields refused). The top-level DisallowUnknownFields
	// already covers this, but re-decode each entry raw to also reject a non-object / extra key defensively.
	var raw struct {
		ReplacementABI []json.RawMessage `json:"replacementAbi"`
	}
	_ = json.Unmarshal(manifestBytes, &raw)
	for _, er := range raw.ReplacementABI {
		ed := json.NewDecoder(bytes.NewReader(er))
		ed.DisallowUnknownFields()
		var e freehandDeviceABIEntry
		if err := ed.Decode(&e); err != nil {
			return fmt.Errorf("strict replacement_abi entry: %w", err)
		}
		if e.BaseIdentity == "" || e.StableIdentity == "" || e.ModuleLibrary == "" || e.ModuleMember == "" || e.Kind == "" {
			return fmt.Errorf("replacement_abi entry missing required field: %+v", e)
		}
	}
	return nil
}

// signFreehandManifest signs the EXACT manifest bytes with Ed25519 (matching the in-app pinned-key verifier).
// The seed comes from the caller's --seed-base64 custody and is NEVER persisted. Returns signature hex.
func signFreehandManifest(manifestBytes []byte, seedBase64 string) (string, error) {
	seed, err := decodeFreehandSeed(seedBase64)
	if err != nil {
		return "", err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return hex.EncodeToString(ed25519.Sign(priv, manifestBytes)), nil
}

func decodeFreehandSeed(seedBase64 string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(seedBase64); err == nil && len(b) == ed25519.SeedSize {
			return b, nil
		}
	}
	return nil, fmt.Errorf("seed must decode (base64) to %d bytes", ed25519.SeedSize)
}

// emitFreehandPayload writes the local, device-verifiable payload: manifest.json (the EXACT signed bytes),
// manifest.sig (hex), and the bytecode file under its manifest name. NO production publish.
func emitFreehandPayload(outDir string, m FreehandDeviceManifest, manifestBytes []byte, sigHex string, bytecodeBytes []byte) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.sig"), []byte(sigHex), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, m.Patches[0].Bytecode), bytecodeBytes, 0o644); err != nil {
		return err
	}
	return nil
}
