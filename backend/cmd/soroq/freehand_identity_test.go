package main

// THE RICH BASE IDENTITY — producer-side tests.
//
// The device half lives in packages/soroq_flutter/test/cross_base_isolation_test.dart. The two halves
// share ONE pinned digest vector, because each side recomputing and checking its own output would prove
// nothing about drift between them: a change to either encoding that is not made to the other has to
// fail a test, or the first symptom is every device refusing every artifact.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedBaseIdentityDigest is the SAME hex literal asserted by the 'canonical rich-identity digest'
// group in packages/soroq_flutter/test/cross_base_isolation_test.dart, for the same four inputs.
const pinnedBaseIdentityDigest = "a00d4ccfca7deb36a8b9f382d2ee661d5f470a2c95f33eb6accb55028ffba550"

func TestFreehandBaseIdentityDigest_PinnedVector(t *testing.T) {
	got := freehandBaseIdentityDigest("runtime-A", "base-A", "contract-A", "retention-A")
	if got != pinnedBaseIdentityDigest {
		t.Fatalf("canonical digest drifted from the pinned vector.\n got  %s\n want %s\n"+
			"If this change is intended, the SAME value must be updated in "+
			"packages/soroq_flutter/test/cross_base_isolation_test.dart or Go and Dart will disagree "+
			"and every device will refuse every artifact.", got, pinnedBaseIdentityDigest)
	}
}

// NEGATIVE CONTROL for the pin. A digest that ignored three of its four inputs would satisfy the pin
// above and defeat the entire point of a rich identity, so every field must move it.
func TestFreehandBaseIdentityDigest_EveryFieldParticipates(t *testing.T) {
	base := []string{"runtime-A", "base-A", "contract-A", "retention-A"}
	for i, name := range []string{"runtime_id", "base_fingerprint", "contract_digest", "retention_digest"} {
		changed := append([]string(nil), base...)
		changed[i] += "-DIFFERENT"
		if got := freehandBaseIdentityDigest(changed[0], changed[1], changed[2], changed[3]); got == pinnedBaseIdentityDigest {
			t.Errorf("changing %s did not change the digest; the field is not bound", name)
		}
	}
}

// The encoding must be INJECTIVE. A separator-only encoding is not: moving a character across a field
// boundary produces identical bytes, so two structurally different bases would collide again — through
// the very mechanism meant to tell them apart.
func TestFreehandBaseIdentityDigest_IsInjectiveAcrossFieldBoundaries(t *testing.T) {
	a := freehandBaseIdentityDigest("r", "ab", "c", "d")
	b := freehandBaseIdentityDigest("r", "a", "bc", "d")
	if a == b {
		t.Fatal("a field-boundary shift produced the same digest; the encoding is not injective")
	}
}

func TestNewFreehandRichBaseIdentity_RefusesAPartialIdentity(t *testing.T) {
	full := []string{"rt", "fp", "cd", "rd"}
	for i, name := range []string{"runtime_id", "base_fingerprint", "contract_digest", "retention_digest"} {
		f := append([]string(nil), full...)
		f[i] = "  "
		_, err := newFreehandRichBaseIdentity(f[0], f[1], f[2], f[3])
		if err == nil {
			t.Fatalf("a rich identity missing %s must be refused: empty fields compare equal to empty "+
				"fields, so two bases differing only there would be judged the same base", name)
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal must name the missing field %s, got %v", name, err)
		}
	}
	// GREEN CONTROL: a complete identity must still be accepted, or the check above is satisfied by a
	// constructor that refuses everything.
	if _, err := newFreehandRichBaseIdentity(full[0], full[1], full[2], full[3]); err != nil {
		t.Fatalf("a complete identity must be accepted: %v", err)
	}
}

func TestRichBaseIdentity_Validate_RefusesADigestThatDoesNotRecompute(t *testing.T) {
	id, err := newFreehandRichBaseIdentity("rt", "fp", "cd", "rd")
	if err != nil {
		t.Fatal(err)
	}
	if err := id.validate(); err != nil {
		t.Fatalf("GREEN control: a self-consistent identity must validate: %v", err)
	}
	// PLANTED FAILURE: a digest that selects one base while the fields describe another. Without the
	// re-derivation the two gates would disagree and whichever ran second would be the only real one.
	forged := id
	forged.Digest = strings.Repeat("f", 64)
	err = forged.validate()
	if err == nil {
		t.Fatal("a digest that does not recompute from its own fields must be refused")
	}
	if !strings.Contains(err.Error(), "does not recompute") {
		t.Errorf("the refusal must say the digest does not recompute, got %v", err)
	}

	// PLANTED FAILURE, the other direction: fields edited under a digest that still matches the OLD
	// tuple. The digest is stale, so it must not validate either.
	swapped := id
	swapped.ContractDigest = "cd-OTHER"
	if err := swapped.validate(); err == nil {
		t.Fatal("editing a field without recomputing the digest must be refused")
	}
}

// richBaseIdentityFromBaseline must read the VERIFIED struct and must refuse a base that cannot produce
// a complete identity, rather than emitting a partial one that would silently distinguish nothing.
func TestRichBaseIdentityFromBaseline(t *testing.T) {
	good := func() *FreehandBaselineMeta {
		return &FreehandBaselineMeta{
			RuntimeID:      "rt-1",
			AppDillSHA256:  freehandSHA256Bytes([]byte("appdill")),
			ManifestSHA256: freehandSHA256Bytes([]byte("manifest")),
			GraphSHA256:    freehandSHA256Bytes([]byte("graph")),
			ContractDigest: freehandSHA256Bytes([]byte("contract")),
			PatchableCount: 3,
			Retention: &FreehandRetentionEvidence{
				Verified:           true,
				RetainedIdentities: 3,
				ManifestSHA256:     freehandSHA256Bytes([]byte("manifest")),
				SymbolGraphSHA256:  freehandSHA256Bytes([]byte("graph")),
				AnalysisID:         freehandSHA256Bytes([]byte("analysis")),
			},
		}
	}

	id, err := richBaseIdentityFromBaseline(good())
	if err != nil {
		t.Fatalf("GREEN control: a complete verified baseline must produce an identity: %v", err)
	}
	// NON-CIRCULAR: every field is an immutable input the baseline already records, never a hash of the
	// record that carries it.
	if id.BaseFingerprint != good().AppDillSHA256 {
		t.Errorf("base_fingerprint must be the shipped base artifact's sha, got %s", id.BaseFingerprint)
	}
	if id.RetentionDigest != good().Retention.ManifestSHA256 {
		t.Errorf("retention_digest must be the retained-identity manifest sha, got %s", id.RetentionDigest)
	}
	if id.ContractDigest != good().ContractDigest {
		t.Errorf("contract_digest must be the base contract digest, got %s", id.ContractDigest)
	}
	if err := id.validate(); err != nil {
		t.Errorf("a derived identity must be self-consistent: %v", err)
	}

	// A base with no contract digest cannot be told apart from a sibling built against a different
	// dynamic interface, which is one of the two collisions this identity exists to close.
	noContract := good()
	noContract.ContractDigest = ""
	if _, err := richBaseIdentityFromBaseline(noContract); err == nil {
		t.Error("a baseline with no contract digest must not yield an identity")
	}
	// Retention evidence is what proves the identity set survived tree-shaking; without it the
	// retention digest would be a claim about nothing.
	noRetention := good()
	noRetention.Retention = nil
	if _, err := richBaseIdentityFromBaseline(noRetention); err == nil {
		t.Error("a baseline with no retention evidence must not yield an identity")
	}
	if _, err := richBaseIdentityFromBaseline(nil); err == nil {
		t.Error("a nil baseline must not yield an identity")
	}
}

// TWO BASES SHARING app/channel/version/trust.
//
// runtime_id is soroqManifestTrustRuntimeID(appID, channel, appVersion, buildName, buildNumber,
// fingerprint) and describes nothing about the app binary, so these two baselines produce the SAME
// runtime id. One case per differing field, so a guard that only checks one of them fails the others.
func TestRichBaseIdentity_CollidingRuntimesAreStillDistinct(t *testing.T) {
	base := func() *FreehandBaselineMeta {
		return &FreehandBaselineMeta{
			RuntimeID:      "same-runtime-id",
			AppDillSHA256:  freehandSHA256Bytes([]byte("appdill-A")),
			ManifestSHA256: freehandSHA256Bytes([]byte("manifest-A")),
			GraphSHA256:    freehandSHA256Bytes([]byte("graph")),
			ContractDigest: freehandSHA256Bytes([]byte("contract-A")),
			PatchableCount: 1,
			Retention: &FreehandRetentionEvidence{
				Verified: true, RetainedIdentities: 1,
				ManifestSHA256:    freehandSHA256Bytes([]byte("manifest-A")),
				SymbolGraphSHA256: freehandSHA256Bytes([]byte("graph")),
				AnalysisID:        freehandSHA256Bytes([]byte("analysis")),
			},
		}
	}
	a, err := richBaseIdentityFromBaseline(base())
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		apply func(*FreehandBaselineMeta)
	}{
		{"contract_digest", func(m *FreehandBaselineMeta) {
			m.ContractDigest = freehandSHA256Bytes([]byte("contract-B"))
		}},
		{"retention_digest", func(m *FreehandBaselineMeta) {
			sha := freehandSHA256Bytes([]byte("manifest-B"))
			m.ManifestSHA256 = sha
			m.Retention.ManifestSHA256 = sha
		}},
		{"base_fingerprint", func(m *FreehandBaselineMeta) {
			m.AppDillSHA256 = freehandSHA256Bytes([]byte("appdill-B"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.apply(m)
			b, err := richBaseIdentityFromBaseline(m)
			if err != nil {
				t.Fatal(err)
			}
			if b.RuntimeID != a.RuntimeID {
				t.Fatalf("the premise of this test is that the runtime ids COLLIDE; they did not")
			}
			if b.Digest == a.Digest {
				t.Fatalf("two bases differing in %s share a rich identity digest; selection on that "+
					"digest would serve each of them the other's artifact", tc.name)
			}
		})
	}

	// GREEN CONTROL: an identical baseline must produce an identical digest, or "different" is
	// meaningless and every legitimate patch would be refused.
	same, err := richBaseIdentityFromBaseline(base())
	if err != nil {
		t.Fatal(err)
	}
	if same.Digest != a.Digest {
		t.Fatal("the same baseline must produce the same digest")
	}
}

// The SIGNED DEVICE MANIFEST must carry the identity and must fail closed on a missing field or a
// digest that does not recompute — the producer half of the gate the device runs before staging.
func TestFreehandDeviceManifest_BaseIdentityIsMandatoryAndSelfConsistent(t *testing.T) {
	id, err := newFreehandRichBaseIdentity("rt-1",
		freehandSHA256Bytes([]byte("appdill")),
		freehandSHA256Bytes([]byte("contract")),
		freehandSHA256Bytes([]byte("retention")))
	if err != nil {
		t.Fatal(err)
	}
	sha := freehandSHA256Bytes([]byte("bytecode"))
	valid := FreehandDeviceManifest{
		Version:            1,
		RuntimeID:          "rt-1",
		BaseIdentity:       &id,
		BytecodeSha256:     sha,
		EntrypointContract: freehandDeviceContract,
		Patches:            []freehandDevicePatch{{Bytecode: "m.bytecode"}},
		ReplacementABI: []freehandDeviceABIEntry{{
			BaseIdentity: "package:app/main.dart::::greeting", StableIdentity: "k",
			ModuleLibrary: "soroq-freehand:///m.dart", ModuleMember: "greeting", Kind: "function",
		}},
		LogicalArtifactID:    "artifact-1",
		ModuleBytecodeSha256: sha,
		PayloadSha256:        sha,
	}
	mustMarshal := func(m FreehandDeviceManifest) []byte {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// GREEN CONTROL first, so the refusals below cannot be satisfied by a validator that rejects
	// everything it is shown.
	if err := validateFreehandDeviceManifest(mustMarshal(valid)); err != nil {
		t.Fatalf("a complete manifest must validate: %v", err)
	}

	// PLANTED FAILURE: no identity block at all. Signature and hash both still pass — they prove the
	// manifest is authentic and intact, never WHICH BASE it was compiled against.
	absent := valid
	absent.BaseIdentity = nil
	err = validateFreehandDeviceManifest(mustMarshal(absent))
	if err == nil {
		t.Fatal("a manifest with no baseIdentity block must be refused")
	}
	if !strings.Contains(err.Error(), "missing the baseIdentity block") {
		t.Errorf("the refusal must name the missing block, got %v", err)
	}

	// PLANTED FAILURE, one per field: a manifest for an identity-aware base missing ANY field.
	for _, field := range []string{"runtime_id", "base_fingerprint", "contract_digest", "retention_digest"} {
		raw := map[string]any{}
		if err := json.Unmarshal(mustMarshal(valid), &raw); err != nil {
			t.Fatal(err)
		}
		block := raw["baseIdentity"].(map[string]any)
		delete(block, field)
		b, _ := json.Marshal(raw)
		err := validateFreehandDeviceManifest(b)
		if err == nil {
			t.Fatalf("a manifest missing baseIdentity.%s must be refused", field)
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("the refusal for a missing %s must name it, got %v", field, err)
		}
	}

	// PLANTED FAILURE: a digest that does not recompute from its own fields.
	forgedID := id
	forgedID.Digest = strings.Repeat("a", 64)
	forged := valid
	forged.BaseIdentity = &forgedID
	err = validateFreehandDeviceManifest(mustMarshal(forged))
	if err == nil {
		t.Fatal("a manifest whose identity digest does not recompute must be refused")
	}
	if !strings.Contains(err.Error(), "does not recompute") {
		t.Errorf("the refusal must say the digest does not recompute, got %v", err)
	}

	// PLANTED FAILURE: an identity block for a DIFFERENT runtime than the manifest it travels on. The
	// block is internally perfect; it simply describes another base.
	otherID, err := newFreehandRichBaseIdentity("rt-OTHER",
		freehandSHA256Bytes([]byte("appdill")),
		freehandSHA256Bytes([]byte("contract")),
		freehandSHA256Bytes([]byte("retention")))
	if err != nil {
		t.Fatal(err)
	}
	crossed := valid
	crossed.BaseIdentity = &otherID
	err = validateFreehandDeviceManifest(mustMarshal(crossed))
	if err == nil {
		t.Fatal("an identity block for a different runtime must be refused")
	}
	if !strings.Contains(err.Error(), "runtime_id") {
		t.Errorf("the refusal must name runtime_id, got %v", err)
	}

	// The strict decode must NOT have been loosened to make room for the new block.
	withUnknown := map[string]any{}
	if err := json.Unmarshal(mustMarshal(valid), &withUnknown); err != nil {
		t.Fatal(err)
	}
	withUnknown["baseIdentity"].(map[string]any)["injected"] = true
	b, _ := json.Marshal(withUnknown)
	if err := validateFreehandDeviceManifest(b); err == nil {
		t.Fatal("an unknown field inside the identity block must still be refused by the strict decode")
	}
}

// The PUBLISH gate refuses an artifact that records no rich identity, so a payload that no
// identity-aware device could ever accept is never emitted.
func TestBuildFreehandDeviceManifest_RefusesAnArtifactWithNoIdentity(t *testing.T) {
	dir := t.TempDir()
	_, meta, _ := buildValidFreehandArtifact(t, dir)

	// GREEN CONTROL: the untouched artifact publishes, and the manifest carries the identity.
	m, _, err := buildFreehandDeviceManifest(dir, 1, "soroq_freehand_v1.bytecode")
	if err != nil {
		t.Fatalf("a valid artifact must publish: %v", err)
	}
	if m.BaseIdentity == nil {
		t.Fatal("the device manifest must carry the rich base identity")
	}
	if m.BaseIdentity.Digest != meta.BaseIdentity.Digest {
		t.Errorf("the manifest identity must be the artifact's, got %s want %s",
			m.BaseIdentity.Digest, meta.BaseIdentity.Digest)
	}

	// PLANTED FAILURE: strip the identity from the persisted artifact. Everything else — hashes, ABI
	// bijection, dependency binding — stays exactly as it was.
	p := filepath.Join(dir, "patch_artifact.json")
	raw := map[string]any{}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "base_identity_record")
	stripped, _ := json.MarshalIndent(raw, "", "  ")
	if err := os.WriteFile(p, stripped, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = buildFreehandDeviceManifest(dir, 1, "soroq_freehand_v1.bytecode")
	if err == nil {
		t.Fatal("publishing an artifact with no rich base identity must be refused")
	}
	if !strings.Contains(err.Error(), "rich base identity") {
		t.Errorf("the refusal must say what is missing, got %v", err)
	}
}

// The ARTIFACT verification refuses an identity that is internally perfect but describes a different
// base than the artifact's own records — the swap a self-consistent block would otherwise survive.
func TestVerifyExistingPatchArtifact_RefusesASwappedBaseIdentity(t *testing.T) {
	dir := t.TempDir()
	id, _, _ := buildValidFreehandArtifact(t, dir)
	if err := verifyExistingPatchArtifact(dir, id); err != nil {
		t.Fatalf("GREEN control: the untouched artifact must verify: %v", err)
	}

	other, err := newFreehandRichBaseIdentity("rt-1",
		freehandSHA256Bytes([]byte("a DIFFERENT base app.dill")),
		freehandSHA256Bytes([]byte("contract")),
		freehandSHA256Bytes([]byte("retention")))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "patch_artifact.json")
	raw := map[string]any{}
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	blk, _ := json.Marshal(other)
	var blkMap map[string]any
	if err := json.Unmarshal(blk, &blkMap); err != nil {
		t.Fatal(err)
	}
	raw["base_identity_record"] = blkMap
	swapped, _ := json.MarshalIndent(raw, "", "  ")
	if err := os.WriteFile(p, swapped, 0o600); err != nil {
		t.Fatal(err)
	}

	err = verifyExistingPatchArtifact(dir, id)
	if err == nil {
		t.Fatal("an identity describing a different base app.dill must be refused")
	}
	if !strings.Contains(err.Error(), "base_fingerprint") {
		t.Errorf("the refusal must name base_fingerprint, got %v", err)
	}
}
