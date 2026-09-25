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

// freehandObfuscatedDeviceContract is the VERSIONED contract for an obfuscated base.
//
// It exists so an OLD CLIENT FAILS CLOSED. A pre-R6 controller routes on the contract string and
// recognises only freehand_identity_v1; handed this one it matches nothing, takes no path, and stages
// nothing -- which is the correct outcome, because it would otherwise read the source-level
// base_identity fields and install redirects that resolve to nothing against an obfuscated base.
// Silently doing nothing WHILE REPORTING SUCCESS is the exact failure this whole lane removes, so the
// discriminator has to be in the field every client already branches on.
const freehandObfuscatedDeviceContract = "freehand_identity_obfuscated_v2"

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
	// The RUNTIME projection, present only under freehandObfuscatedDeviceContract. The source-level
	// fields above are KEPT as they are -- they are what review, semantic diffing and every existing
	// verification path read, and a device that is told to use the runtime fields still reports the
	// source identity when something goes wrong.
	RuntimeBaseIdentity string `json:"runtime_base_identity,omitempty"`
	RuntimeModuleClass  string `json:"runtime_module_class,omitempty"`
	RuntimeModuleMember string `json:"runtime_module_member,omitempty"`
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
	// ObfuscationBindingDigest / TranslationReceiptSha256 put the obfuscated-base binding INSIDE the
	// signed bytes, so the device can check that the ABI it is about to install belongs to the base map
	// and the receipt this patch was compiled against -- not merely that someone signed something.
	// Both are absent for a non-obfuscated base, and an old client ignores them and still fails closed
	// on the contract string.
	ObfuscationBindingDigest string `json:"obfuscationBindingDigest,omitempty"`
	TranslationReceiptSha256 string `json:"translationReceiptSha256,omitempty"`
	// BaseObfuscationMapSha256 is the digest of the BASE's captured obfuscation map. It is the value
	// the device can actually check: the app carries the same digest in its embedded base identity,
	// delivered at release time, and refuses a patch whose manifest names a different one. The map
	// itself never leaves the release machine.
	BaseObfuscationMapSha256 string `json:"baseObfuscationMapSha256,omitempty"`
	// BaseIdentity is the COMPLETE rich identity of the base this patch was compiled against, inside the
	// signed bytes. The top-level RuntimeID above is version-derived — appId | channel | appVersion |
	// buildName | buildNumber | trustFingerprint — and describes nothing about the app binary, so two
	// structurally different bases sharing all of those have the SAME runtime_id and each passes the
	// other's runtime check. This block is what tells them apart; `SoroqEngineLaneController.
	// rejectForeignArtifact` compares it field by field and re-derives its digest before anything is
	// staged.
	BaseIdentity *FreehandRichBaseIdentity `json:"baseIdentity"`
}

// requireTranslatedABICoverage proves a device ABI was actually translated, from the PERSISTED,
// VERIFIED projection rather than from what the strings happen to look like.
//
// The previous check asked whether any runtime identity differed from its source identity, and refused
// a manifest where none did. That is wrong: the obfuscator PROTECTS some names -- PreventRenaming
// writes them into the map as name -> name -- so a legitimate one-entry patch on `noSuchMethod`, `==`
// or `main` translates to itself in every field and was refused. The evidence that translation
// happened is that every entry is covered by the receipt-derived projection, which
// verifyExistingPatchArtifact re-derives from the durable ABI and the authenticated base map.
func requireTranslatedABICoverage(entries []freehandDeviceABIEntry, translated []FreehandTranslatedABIEntry) error {
	byStable := make(map[string]FreehandTranslatedABIEntry, len(translated))
	for _, t := range translated {
		byStable[t.StableIdentity] = t
	}
	for _, e := range entries {
		t, ok := byStable[e.StableIdentity]
		if !ok {
			return fmt.Errorf("replacement-ABI entry %s is not covered by the verified translation projection", e.BaseIdentity)
		}
		if t.BaseIdentity != e.BaseIdentity || t.ModuleClass != e.ModuleClass || t.ModuleMember != e.ModuleMember {
			return fmt.Errorf("the verified projection for %s describes a different source identity", e.StableIdentity)
		}
		if t.RuntimeBaseIdentity != e.RuntimeBaseIdentity ||
			t.RuntimeModuleClass != e.RuntimeModuleClass ||
			t.RuntimeModuleMember != e.RuntimeModuleMember {
			return fmt.Errorf("replacement-ABI entry %s carries a runtime identity the verified projection does not produce", e.BaseIdentity)
		}
	}
	if len(entries) != len(translated) {
		return fmt.Errorf("the verified projection covers %d identities but the ABI carries %d", len(translated), len(entries))
	}
	return nil
}

// obfuscationMapDigestOf is the base map digest, or "" when the base is not obfuscated.
func obfuscationMapDigestOf(b *FreehandObfuscationBinding) string {
	if !b.isEnabled() {
		return ""
	}
	return b.MapSHA256
}

// contractFor picks the entrypoint contract. A non-obfuscated patch keeps the exact v1 string it has
// always had, so nothing about the existing lane moves.
func contractFor(obfuscated bool) string {
	if obfuscated {
		return freehandObfuscatedDeviceContract
	}
	return freehandDeviceContract
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
	// The runtime projection, keyed by the frozen stable identity. Built at PATCH time from the
	// compiler's receipt and carried in the artifact, so publishing never has to re-derive an identity.
	translated := make(map[string]FreehandTranslatedABIEntry, len(meta.TranslatedABI))
	for _, t := range meta.TranslatedABI {
		translated[t.StableIdentity] = t
	}
	obfuscated := meta.Obfuscation.isEnabled()
	if obfuscated {
		if err := meta.Obfuscation.validate(); err != nil {
			return zero, nil, fmt.Errorf("refusing to publish an artifact with an invalid obfuscation binding: %w", err)
		}
		if meta.ObfuscationBindingDigest == "" || meta.TranslationReceiptSHA256 == "" {
			return zero, nil, errors.New("refusing to publish: the artifact declares an obfuscated base but binds no receipt digest")
		}
		if len(translated) != len(durable.ReplacementABI) {
			return zero, nil, fmt.Errorf("refusing to publish: %d of %d replacement-ABI entries have no runtime projection",
				len(durable.ReplacementABI)-len(translated), len(durable.ReplacementABI))
		}
	} else if len(meta.TranslatedABI) > 0 {
		// A translated ABI on a base that is not obfuscated is a forged pairing, not a harmless extra.
		return zero, nil, errors.New("refusing to publish: the artifact carries a translated ABI but records no obfuscated base")
	}
	abi := make([]freehandDeviceABIEntry, 0, len(durable.ReplacementABI))
	for _, e := range durable.ReplacementABI {
		entry := freehandDeviceABIEntry{
			BaseIdentity:   e.BaseIdentity,
			StableIdentity: e.StableIdentity,
			ModuleLibrary:  e.ModuleLibrary,
			ModuleClass:    e.ModuleClass,
			ModuleMember:   e.ModuleMember,
			Kind:           e.Kind,
		}
		if obfuscated {
			t, ok := translated[e.StableIdentity]
			if !ok {
				return zero, nil, fmt.Errorf("refusing to publish: replacement-ABI entry %s has no runtime projection", e.BaseIdentity)
			}
			entry.RuntimeBaseIdentity = t.RuntimeBaseIdentity
			entry.RuntimeModuleClass = t.RuntimeModuleClass
			entry.RuntimeModuleMember = t.RuntimeModuleMember
		}
		abi = append(abi, entry)
	}
	if obfuscated {
		// Coverage against the VERIFIED projection -- verifyExistingPatchArtifact above re-derived it
		// from this artifact's own receipt and durable ABI -- rather than any judgement about whether
		// the strings differ. A protected name legitimately translates to itself.
		if err := requireTranslatedABICoverage(abi, meta.TranslatedABI); err != nil {
			return zero, nil, fmt.Errorf("refusing to publish: %w", err)
		}
	}

	// FAIL CLOSED. An artifact with no rich identity can only be bound by runtime_id, so publishing it
	// would produce a manifest that an identity-aware device must refuse anyway — and that a
	// pre-identity device would accept from the wrong base. Refuse here, where the message can say what
	// to do about it, rather than shipping a payload nothing can use.
	if meta.BaseIdentity == nil {
		return zero, nil, errors.New("refusing to publish: the patch artifact records no rich base identity (it predates the four-field identity) — rebuild the patch")
	}
	if err := meta.BaseIdentity.validate(); err != nil {
		return zero, nil, fmt.Errorf("refusing to publish an artifact with an invalid base identity: %w", err)
	}
	if meta.BaseIdentity.RuntimeID != meta.RuntimeID {
		return zero, nil, fmt.Errorf("refusing to publish: base identity runtime_id %q != artifact runtime_id %q", meta.BaseIdentity.RuntimeID, meta.RuntimeID)
	}

	m := FreehandDeviceManifest{
		Version:              version,
		RuntimeID:            meta.RuntimeID,
		BaseIdentity:         meta.BaseIdentity,
		BytecodeSha256:       bytecodeSHA,
		EntrypointContract:   contractFor(obfuscated),
		Patches:              []freehandDevicePatch{{Bytecode: bytecodeName}},
		ReplacementABI:       abi,
		LogicalArtifactID:    meta.ArtifactID,
		ModuleBytecodeSha256: bytecodeSHA,
		PayloadSha256:        bytecodeSHA,
		// Bound from the artifact record, which verifyExistingPatchArtifact above already re-derived from
		// the persisted descriptor file and cross-checked against patch_plan.json.
		DependencyDescriptorDigest: meta.DependencyDescriptorDigest,
		ObfuscationBindingDigest:   meta.ObfuscationBindingDigest,
		TranslationReceiptSha256:   meta.TranslationReceiptSHA256,
		BaseObfuscationMapSha256:   obfuscationMapDigestOf(meta.Obfuscation),
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
	// EXACTLY ONE of the two contracts, and each one implies its own obligations. A v1 manifest that
	// carries obfuscation fields, or a v2 that does not, is a mismatched pairing rather than a
	// tolerable extra: the device branches on this string and would then honour the wrong half.
	switch m.EntrypointContract {
	case freehandDeviceContract:
		if m.BaseObfuscationMapSha256 != "" {
			return errors.New("a freehand_identity_v1 manifest must carry no base obfuscation map digest")
		}
		if m.ObfuscationBindingDigest != "" || m.TranslationReceiptSha256 != "" {
			return errors.New("a freehand_identity_v1 manifest must carry no obfuscation binding")
		}
		for _, e := range m.ReplacementABI {
			if e.RuntimeBaseIdentity != "" || e.RuntimeModuleClass != "" || e.RuntimeModuleMember != "" {
				return fmt.Errorf("a freehand_identity_v1 manifest must carry no runtime projection (entry %s)", e.BaseIdentity)
			}
		}
	case freehandObfuscatedDeviceContract:
		if !sha256HexRe.MatchString(m.BaseObfuscationMapSha256) {
			return errors.New("an obfuscated freehand manifest must bind a well-formed baseObfuscationMapSha256")
		}
		if !sha256HexRe.MatchString(m.ObfuscationBindingDigest) {
			return errors.New("an obfuscated freehand manifest must bind a well-formed obfuscationBindingDigest")
		}
		if !sha256HexRe.MatchString(m.TranslationReceiptSha256) {
			return errors.New("an obfuscated freehand manifest must bind a well-formed translationReceiptSha256")
		}
		for _, e := range m.ReplacementABI {
			if e.RuntimeBaseIdentity == "" || e.RuntimeModuleMember == "" {
				return fmt.Errorf("obfuscated replacement-ABI entry %s has no runtime projection", e.BaseIdentity)
			}
			// The runtime identity must be a well-formed triple. Whether it DIFFERS from the source
			// one proves nothing either way -- a protected name legitimately translates to itself --
			// so coverage against the verified projection is what establishes translation, in
			// verifyExistingPatchArtifact and requireTranslatedABICoverage.
			if _, _, _, err := splitBaseIdentity(e.RuntimeBaseIdentity); err != nil {
				return fmt.Errorf("obfuscated replacement-ABI entry %s has a malformed runtime identity: %w", e.BaseIdentity, err)
			}
		}
	default:
		return fmt.Errorf("freehand device manifest has wrong entrypointContract %q", m.EntrypointContract)
	}
	if m.Version <= 0 {
		return fmt.Errorf("freehand device manifest version must be > 0")
	}
	if strings.TrimSpace(m.RuntimeID) == "" {
		return errors.New("freehand device manifest missing runtime_id")
	}
	// A manifest for an identity-aware base that is missing ANY identity field, or whose digest does not
	// recompute from its own fields, is refused before it can be signed — the producer half of the gate
	// the device runs before staging. Neither the signature nor the payload hash says anything about
	// WHICH BASE a module was compiled against, so this is the only check that does.
	if m.BaseIdentity == nil {
		return errors.New("freehand device manifest is missing the baseIdentity block; runtime_id alone cannot distinguish two bases that share app/channel/version/trust")
	}
	if err := m.BaseIdentity.validate(); err != nil {
		return fmt.Errorf("freehand device manifest base identity: %w", err)
	}
	if m.BaseIdentity.RuntimeID != m.RuntimeID {
		return fmt.Errorf("freehand device manifest base identity runtime_id %q != manifest runtime_id %q", m.BaseIdentity.RuntimeID, m.RuntimeID)
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
