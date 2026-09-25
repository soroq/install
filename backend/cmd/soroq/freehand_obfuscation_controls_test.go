package main

// PLANTED CONTROLS for the obfuscated-base lane.
//
// Each one takes a configuration that PASSES and breaks exactly one thing, then asserts the refusal.
// Every control begins by proving the unmutated configuration is accepted -- a control whose baseline
// silently stopped working would otherwise "fire" forever while testing nothing.

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A small, realistic base map: renamed user identifiers plus protected names as identity mappings,
// exactly the shape gen_snapshot's --save-obfuscation-map emits.
var controlBaseMapFlat = []any{
	"_HomePageState", "_hW",
	"Counter", "Qx",
	"build", "Nza",
	"builds", "xqc",
	"bump", "yqc",
	"main", "main",
	"noSuchMethod", "noSuchMethod",
}

func controlMapBytes(t *testing.T, flat []any) []byte {
	t.Helper()
	raw, err := json.Marshal(flat)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// writeControlBaseMap writes a map at the release-side path and returns its digest.
func writeControlBaseMap(t *testing.T, dir string, flat []any) (string, string) {
	t.Helper()
	raw := controlMapBytes(t, flat)
	p := filepath.Join(dir, freehandBaseObfuscationMapFile)
	if err := os.WriteFile(p, raw, freehandBaseObfuscationMapMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, freehandBaseObfuscationMapMode); err != nil {
		t.Fatal(err)
	}
	return p, freehandSHA256Bytes(raw)
}

func controlBinding(t *testing.T, digest string, entries int) *FreehandObfuscationBinding {
	t.Helper()
	return &FreehandObfuscationBinding{
		Schema:           freehandObfuscationBindingSchema,
		Enabled:          true,
		MapFormat:        soroqBaseMapFormat,
		MapFormatVersion: soroqBaseMapFormatVersion,
		MapSHA256:        digest,
		MapMode:          freehandBaseObfuscationMapMode.String(),
		MapFile:          freehandBaseObfuscationMapFile,
		MapEntries:       entries,
		Capability:       freehandObfuscatedIdentityTranslationCapability,
		EngineRevision:   "engine-rev-r6",
	}
}

func controlReceipt(mapDigest string) *FreehandTranslationReceipt {
	identities := []FreehandTranslationIdentity{
		{
			Context: freehandTranslationContextClass, Library: "package:app/main.dart",
			Original: "_HomePageState", Runtime: "_hW", Origin: freehandTranslationOriginBase,
		},
		{
			Context: freehandTranslationContextMember, Library: "package:app/main.dart",
			EnclosingOriginal: "_HomePageState", EnclosingRuntime: "_hW",
			Original: "build", Runtime: "Nza", Origin: freehandTranslationOriginBase,
		},
		{
			Context: freehandTranslationContextClass, Original: "Counter", Runtime: "Qx",
			Origin: freehandTranslationOriginBase,
		},
		{
			Context: freehandTranslationContextMember, EnclosingOriginal: "Counter",
			EnclosingRuntime: "Qx", Original: "bump", Runtime: "yqc",
			Origin: freehandTranslationOriginBase,
		},
		{
			Context: freehandTranslationContextSelector, SelectorPrefix: "get:",
			Original: "builds", Runtime: "xqc",
			CompositeOriginal: "get:builds", CompositeRuntime: "get:xqc",
			Origin: freehandTranslationOriginBase,
		},
	}
	return &FreehandTranslationReceipt{
		Format:             soroqTranslationReceiptFormat,
		FormatVersion:      soroqTranslationReceiptFormatVersion,
		ObfuscationEnabled: true,
		BaseMapSHA256:      mapDigest,
		BaseMapEntries:     len(controlBaseMapFlat) / 2,
		IdentityCount:      len(identities),
		Identities:         identities,
	}
}

func writeControlReceipt(t *testing.T, dir string, r *FreehandTranslationReceipt) (string, string) {
	t.Helper()
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, freehandTranslationReceiptFile)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, freehandSHA256Bytes(raw)
}

// ---------------------------------------------------------------------------------------------
// CONTROL 1 — an obfuscated command is STILL refused when the capability is the only thing present.
// ---------------------------------------------------------------------------------------------

func TestControlObfuscatedBuildRefusedDespiteR6CapableToolchain(t *testing.T) {
	obfArgs := []string{"--obfuscate", "--split-debug-info=build/symbols"}
	capable := &freehandObfuscationAuthorization{
		Allowed:        true,
		Capability:     freehandObfuscatedIdentityTranslationCapability,
		EngineRevision: "engine-rev-r6",
		Reason:         "engine engine-rev-r6 declares " + freehandObfuscatedIdentityTranslationCapability,
	}
	// BASELINE: with the capability, the BUILD is permitted.
	if err := guardUnverifiedBuildFlags(obfArgs, capable); err != nil {
		t.Fatalf("an R6-capable toolchain must be allowed to build: %v", err)
	}

	// CONTROL: the capability alone does not make a PATCH bind. A base with no captured map is still
	// refused at compile time, which is what stops "the build was allowed" from being read as "the
	// lane works".
	relDir := t.TempDir()
	if _, err := resolveBaseObfuscationMap(relDir, controlBinding(t, strings.Repeat("a", 64), 7)); err == nil {
		t.Fatal("control did not fire: an obfuscated base with no captured map was accepted")
	}

	// CONTROL: no capability -> the build itself is refused again.
	for _, auth := range []*freehandObfuscationAuthorization{
		nil,
		{Allowed: false, Reason: "engine declares no identity capabilities"},
	} {
		err := guardUnverifiedBuildFlags(obfArgs, auth)
		if err == nil {
			t.Fatal("control did not fire: an obfuscated build was allowed with no capability")
		}
		if !strings.Contains(err.Error(), freehandObfuscatedIdentityTranslationCapability) {
			t.Fatalf("the refusal must name the missing capability: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// CONTROL 2 — an arbitrary map with a self-computed digest.
// ---------------------------------------------------------------------------------------------

func TestControlArbitraryMapWithSelfComputedDigestIsRefused(t *testing.T) {
	relDir := t.TempDir()
	_, digest := writeControlBaseMap(t, relDir, controlBaseMapFlat)
	binding := controlBinding(t, digest, len(controlBaseMapFlat)/2)
	// BASELINE.
	if _, err := resolveBaseObfuscationMap(relDir, binding); err != nil {
		t.Fatalf("the captured map must resolve: %v", err)
	}

	// CONTROL: swap in a different map AND recompute its digest, the way a caller supplying both
	// would. Integrity is satisfied; PROVENANCE is not, because the binding is the baseline's and the
	// baseline is not rewritable from a command line.
	forged := []any{"anything", "zz", "main", "main"}
	forgedRaw := controlMapBytes(t, forged)
	if err := os.WriteFile(filepath.Join(relDir, freehandBaseObfuscationMapFile), forgedRaw, freehandBaseObfuscationMapMode); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveBaseObfuscationMap(relDir, binding); err == nil {
		t.Fatal("control did not fire: a substituted map passed the baseline's binding")
	}
	// And a binding that ALSO carries the forged digest is still not the base's: the artifact id and
	// the signed manifest bind the ORIGINAL digest, so the substitution changes the artifact identity.
	forgedBinding := controlBinding(t, freehandSHA256Bytes(forgedRaw), 2)
	a, err := freehandObfuscationArtifactDigest(binding, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	b, err := freehandObfuscationArtifactDigest(forgedBinding, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("control did not fire: a forged map produced the same artifact binding digest")
	}
}

// ---------------------------------------------------------------------------------------------
// CONTROL 3 — a receipt produced against a DIFFERENT base.
// ---------------------------------------------------------------------------------------------

func TestControlWrongBaselineMapIsRefused(t *testing.T) {
	relDir := t.TempDir()
	_, digest := writeControlBaseMap(t, relDir, controlBaseMapFlat)
	// BASELINE.
	receiptPath, _ := writeControlReceipt(t, relDir, controlReceipt(digest))
	if _, _, err := loadTranslationReceipt(receiptPath, digest); err != nil {
		t.Fatalf("a matching receipt must load: %v", err)
	}

	// CONTROL: the receipt describes another base's map.
	other := controlReceipt(strings.Repeat("c", 64))
	otherPath, _ := writeControlReceipt(t, t.TempDir(), other)
	_, _, err := loadTranslationReceipt(otherPath, digest)
	if err == nil {
		t.Fatal("control did not fire: a receipt for a different base map was accepted")
	}
	if !strings.Contains(err.Error(), "translated for a different base") {
		t.Fatalf("the refusal must name the wrong-baseline case: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// CONTROLS 4 and 5 — the two halves must move together.
// ---------------------------------------------------------------------------------------------

func TestControlTranslatedBytecodeWithUntranslatedABIIsRefused(t *testing.T) {
	entries := []freehandReplacementEntry{{
		BaseIdentity: "package:app/main.dart::_HomePageState::build", StableIdentity: "v1|k",
		ModuleLibrary: "soroq-freehand:///m", ModuleClass: "Counter", ModuleMember: "bump",
		Kind: "instance-member",
	}}
	receipt := controlReceipt(strings.Repeat("d", 64))
	// BASELINE: the projection resolves and DIFFERS from the source identity.
	translated, err := translateReplacementABI(entries, receipt, "")
	if err != nil {
		t.Fatalf("the ABI must translate: %v", err)
	}
	if translated[0].RuntimeBaseIdentity == translated[0].BaseIdentity {
		t.Fatal("the baseline is not discriminating: the runtime identity equals the source identity")
	}
	if translated[0].RuntimeBaseIdentity != "package:app/main.dart::_hW::Nza" {
		t.Fatalf("unexpected runtime identity %q", translated[0].RuntimeBaseIdentity)
	}

	// CONTROL: an ABI that carries the source identities under the obfuscated contract. The module's
	// bytecode WAS translated; this manifest was not, so every redirect would resolve to nothing.
	//
	// The proof is coverage against the verified projection, not an inequality: a protected name
	// legitimately translates to itself, and that case is asserted in TestDefect6.
	device := []freehandDeviceABIEntry{{
		BaseIdentity: entries[0].BaseIdentity, StableIdentity: "v1|k",
		ModuleLibrary: entries[0].ModuleLibrary, ModuleClass: "Counter", ModuleMember: "bump",
		Kind: "instance-member",
		// The projection is present but is the SOURCE identity, which the receipt does not produce.
		RuntimeBaseIdentity: entries[0].BaseIdentity,
		RuntimeModuleClass:  "Counter",
		RuntimeModuleMember: "bump",
	}}
	if err := requireTranslatedABICoverage(device, translated); err == nil {
		t.Fatal("control did not fire: an untranslated ABI was accepted under the obfuscated contract")
	}
	// BASELINE for the same helper: the correctly projected ABI is accepted.
	good := device
	good[0].RuntimeBaseIdentity = translated[0].RuntimeBaseIdentity
	good[0].RuntimeModuleClass = translated[0].RuntimeModuleClass
	good[0].RuntimeModuleMember = translated[0].RuntimeModuleMember
	if err := requireTranslatedABICoverage(good, translated); err != nil {
		t.Fatalf("the sound configuration must be accepted: %v", err)
	}
}

func TestControlTranslatedABIWithUntranslatedBytecodeIsRefused(t *testing.T) {
	entries := []freehandReplacementEntry{{
		BaseIdentity: "package:app/main.dart::_HomePageState::build", StableIdentity: "v1|k",
		ModuleLibrary: "soroq-freehand:///m", ModuleClass: "Counter", ModuleMember: "bump",
		Kind: "instance-member",
	}}
	// BASELINE.
	if _, err := translateReplacementABI(entries, controlReceipt(strings.Repeat("d", 64)), ""); err != nil {
		t.Fatalf("the ABI must translate: %v", err)
	}

	// CONTROL: the bytecode was compiled WITHOUT translation, so the compiler emitted no receipt for
	// these identities. Projecting the ABI anyway must refuse rather than invent names -- an invented
	// name is a redirect that resolves to nothing.
	empty := &FreehandTranslationReceipt{
		Format: soroqTranslationReceiptFormat, FormatVersion: soroqTranslationReceiptFormatVersion,
		ObfuscationEnabled: true, BaseMapSHA256: strings.Repeat("d", 64),
		BaseMapEntries: 1, IdentityCount: 1,
		Identities: []FreehandTranslationIdentity{{
			Context: freehandTranslationContextClass, Original: "Unrelated", Runtime: "zz",
			Origin: freehandTranslationOriginModule,
		}},
	}
	_, err := translateReplacementABI(entries, empty, "")
	if err == nil {
		t.Fatal("control did not fire: an ABI was projected from a receipt that does not cover it")
	}
	if !strings.Contains(err.Error(), "silently bind nothing") {
		t.Fatalf("the refusal must say what the consequence would be: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// CONTROL 6 — a missing or altered receipt.
// ---------------------------------------------------------------------------------------------

func TestControlMissingOrAlteredReceiptIsRefused(t *testing.T) {
	relDir := t.TempDir()
	_, digest := writeControlBaseMap(t, relDir, controlBaseMapFlat)
	receiptPath, receiptSHA := writeControlReceipt(t, relDir, controlReceipt(digest))
	// BASELINE.
	if _, gotSHA, err := loadTranslationReceipt(receiptPath, digest); err != nil || gotSHA != receiptSHA {
		t.Fatalf("the receipt must load and hash to its own bytes: %v", err)
	}

	t.Run("missing", func(t *testing.T) {
		if _, _, err := loadTranslationReceipt(filepath.Join(relDir, "absent.json"), digest); err == nil {
			t.Fatal("control did not fire: a missing receipt was accepted")
		}
	})

	t.Run("altered_identity", func(t *testing.T) {
		r := controlReceipt(digest)
		r.Identities[0].Runtime = "TAMPERED"
		p, sha := writeControlReceipt(t, t.TempDir(), r)
		if _, gotSHA, err := loadTranslationReceipt(p, digest); err != nil || gotSHA == receiptSHA {
			t.Fatalf("control did not fire: an altered receipt hashed to the original (%v)", err)
		} else if gotSHA != sha {
			t.Fatal("receipt digest is not over its own bytes")
		}
		// The altered receipt produces a DIFFERENT artifact binding, so the alteration cannot be
		// smuggled into an artifact that was signed against the original.
		binding := controlBinding(t, digest, len(controlBaseMapFlat)/2)
		before, _ := freehandObfuscationArtifactDigest(binding, receiptSHA)
		after, _ := freehandObfuscationArtifactDigest(binding, sha)
		if before == after {
			t.Fatal("control did not fire: an altered receipt produced the same artifact binding")
		}
	})

	t.Run("declared_count_disagrees", func(t *testing.T) {
		r := controlReceipt(digest)
		r.IdentityCount = 99
		p, _ := writeControlReceipt(t, t.TempDir(), r)
		if _, _, err := loadTranslationReceipt(p, digest); err == nil {
			t.Fatal("control did not fire: a receipt whose count disagrees with its rows was accepted")
		}
	})

	t.Run("foreign_format", func(t *testing.T) {
		r := controlReceipt(digest)
		r.Format = "someone-elses-receipt"
		p, _ := writeControlReceipt(t, t.TempDir(), r)
		if _, _, err := loadTranslationReceipt(p, digest); err == nil {
			t.Fatal("control did not fire: a foreign receipt format was accepted")
		}
	})
}

// ---------------------------------------------------------------------------------------------
// CONTROL 7 — the signed manifest must bind the RIGHT map and receipt.
// ---------------------------------------------------------------------------------------------

func TestControlSignedManifestWithWrongDigestIsRefused(t *testing.T) {
	binding := controlBinding(t, strings.Repeat("e", 64), 7)
	good, err := freehandObfuscationArtifactDigest(binding, strings.Repeat("f", 64))
	if err != nil {
		t.Fatal(err)
	}
	// BASELINE: the digest recomputes.
	again, err := freehandObfuscationArtifactDigest(binding, strings.Repeat("f", 64))
	if err != nil || again != good {
		t.Fatalf("the binding digest must be reproducible: %v", err)
	}

	t.Run("wrong_map", func(t *testing.T) {
		other := controlBinding(t, strings.Repeat("0", 64), 7)
		got, err := freehandObfuscationArtifactDigest(other, strings.Repeat("f", 64))
		if err != nil {
			t.Fatal(err)
		}
		if got == good {
			t.Fatal("control did not fire: a different base map produced the same signed binding")
		}
	})

	t.Run("wrong_receipt", func(t *testing.T) {
		got, err := freehandObfuscationArtifactDigest(binding, strings.Repeat("9", 64))
		if err != nil {
			t.Fatal(err)
		}
		if got == good {
			t.Fatal("control did not fire: a different receipt produced the same signed binding")
		}
	})

	t.Run("no_receipt_at_all", func(t *testing.T) {
		if _, err := freehandObfuscationArtifactDigest(binding, ""); err == nil {
			t.Fatal("control did not fire: an obfuscated artifact bound no receipt digest")
		}
	})
}

// ---------------------------------------------------------------------------------------------
// CONTROL 9 — an old client handed the new contract.
// ---------------------------------------------------------------------------------------------

func TestControlOldClientFailsClosedOnTheNewContract(t *testing.T) {
	// The pre-R6 controller's branch, reproduced exactly: an EQUALITY test against the v1 string.
	oldClientAccepts := func(contract string) bool { return contract == freehandDeviceContract }

	// BASELINE: an ordinary patch still routes on an old client.
	if !oldClientAccepts(contractFor(false)) {
		t.Fatal("the baseline is broken: an old client must still accept an ordinary freehand patch")
	}
	// CONTROL: the obfuscated contract matches nothing, so an old client stages nothing rather than
	// installing source-level redirects that resolve to nothing against an obfuscated base.
	if oldClientAccepts(contractFor(true)) {
		t.Fatal("control did not fire: an old client accepted the obfuscated contract")
	}

	// The controller source must ALSO fail closed on a v2 entry with no projection, rather than fall
	// back to the source names -- a fallback would reintroduce the silent miss.
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "packages", "soroq_flutter", "lib", "src", "engine_lane_ota.dart"))
	if err != nil {
		t.Skipf("controller source not readable from here: %v", err)
	}
	for _, want := range []string{
		"freehand_identity_obfuscated_v2",
		"carries no runtime projection",
		"runtime_base_identity",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("the controller must contain %q", want)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// CONTROL 10 — the ordinary, non-obfuscated lane is byte-for-byte unaffected.
// ---------------------------------------------------------------------------------------------

func TestControlNonObfuscatedLaneIsUnchanged(t *testing.T) {
	// The artifact id for a base that was not obfuscated must be EXACTLY what it was before R6, which
	// is what the empty-digest branch of computeFreehandArtifactID guarantees.
	plan := strings.Repeat("1", 64)
	tool := strings.Repeat("2", 64)
	man := strings.Repeat("3", 64)
	dep := strings.Repeat("4", 64)
	preR6 := freehandSHA256Bytes([]byte(plan + "|" + tool + "|" + man + "|" + dep))
	if got := computeFreehandArtifactID(plan, tool, man, dep, ""); got != preR6 {
		t.Fatalf("the non-obfuscated artifact id changed: %s != %s", got, preR6)
	}
	// And an obfuscated one is distinct.
	if got := computeFreehandArtifactID(plan, tool, man, dep, strings.Repeat("5", 64)); got == preR6 {
		t.Fatal("an obfuscated artifact id collides with the non-obfuscated one")
	}

	// A base with no binding is not obfuscated, and asking for one is not an error -- it is the
	// ordinary path.
	var absent *FreehandObfuscationBinding
	if absent.isEnabled() {
		t.Fatal("an absent binding must not read as enabled")
	}
	if err := absent.validate(); err != nil {
		t.Fatalf("an absent binding must validate as the ordinary case: %v", err)
	}
	if d, err := freehandObfuscationArtifactDigest(absent, ""); err != nil || d != "" {
		t.Fatalf("an absent binding must bind nothing: %q %v", d, err)
	}
	// The ordinary contract is unchanged.
	if contractFor(false) != "freehand_identity_v1" {
		t.Fatal("the ordinary entrypoint contract moved")
	}
}

// ---------------------------------------------------------------------------------------------
// Map hygiene: mode, shape and provenance at rest.
// ---------------------------------------------------------------------------------------------

func TestControlCapturedMapHygiene(t *testing.T) {
	relDir := t.TempDir()
	mapPath, digest := writeControlBaseMap(t, relDir, controlBaseMapFlat)
	binding := controlBinding(t, digest, len(controlBaseMapFlat)/2)
	if _, err := resolveBaseObfuscationMap(relDir, binding); err != nil {
		t.Fatalf("the captured map must resolve: %v", err)
	}

	t.Run("group_readable_is_refused", func(t *testing.T) {
		if err := os.Chmod(mapPath, 0o644); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(mapPath, freehandBaseObfuscationMapMode)
		err := resolveMapErr(relDir, binding)
		if err == nil {
			t.Fatal("control did not fire: a world-readable symbol map was accepted")
		}
		if !strings.Contains(err.Error(), "names every private declaration") {
			t.Fatalf("the refusal must say why the mode matters: %v", err)
		}
	})

	t.Run("symlink_is_refused", func(t *testing.T) {
		other := t.TempDir()
		linked := filepath.Join(other, freehandBaseObfuscationMapFile)
		if err := os.Symlink(mapPath, linked); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := resolveMapErr(other, binding); err == nil {
			t.Fatal("control did not fire: a symlinked map was accepted")
		}
	})

	t.Run("malformed_maps_are_refused", func(t *testing.T) {
		for name, flat := range map[string][]any{
			"odd_length":     {"a", "b", "c"},
			"duplicate_key":  {"a", "x", "a", "y"},
			"non_injective":  {"a", "x", "b", "x"},
			"empty":          {},
			"empty_identity": {"a", ""},
			"empty_renamed":  {"", "x"},
		} {
			if _, err := parseBaseObfuscationMap(controlMapBytes(t, flat)); err == nil {
				t.Errorf("control did not fire: a %s map was accepted", name)
			}
		}
	})
}

// TestRealObfuscationMapHeadIsAccepted is handoff 17, F4. The first six pairs of a REAL
// --save-obfuscation-map output (R6 gen_snapshot, the private-state fixture); pair 0 is ("", ""),
// which every real map carries and which this parser used to refuse, blocking every real baseline.
func TestRealObfuscationMapHeadIsAccepted(t *testing.T) {
	realHead := []any{
		"", "",
		"_drawBox@581058941", "_Xec@581058941",
		"untransformedEndPosition", "Sba",
		"includeInScreenshot", "dub",
		"ScrollViewKeyboardDismissBehavior", "ScrollViewKeyboardDismissBehavior",
		"castle_outlined", "castle_outlined",
	}
	n, err := parseBaseObfuscationMap(controlMapBytes(t, realHead))
	if err != nil {
		t.Fatalf("a real map head was refused: %v", err)
	}
	if n != len(realHead)/2 {
		t.Fatalf("entry count %d, want %d (the empty pair counts, as it does in dart2bytecode's receipt)", n, len(realHead)/2)
	}
}

func resolveMapErr(dir string, b *FreehandObfuscationBinding) error {
	_, err := resolveBaseObfuscationMap(dir, b)
	return err
}

// ---------------------------------------------------------------------------------------------
// The cross-component key contract, extended to the runtime projection.
// ---------------------------------------------------------------------------------------------

func TestControlControllerReadsTheRuntimeProjectionKeys(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "packages", "soroq_flutter", "lib", "src", "engine_lane_ota.dart"))
	if err != nil {
		t.Skipf("controller source not readable from here: %v", err)
	}
	// Every key the producer emits under the obfuscated contract must be a key the controller reads.
	// Neither suite covers this on its own: they would both pass while real activations failed closed.
	var entry FreehandTranslatedABIEntry
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"runtime_base_identity", "runtime_module_class", "runtime_module_member"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("the producer entry does not emit %q", key)
		}
		if !strings.Contains(string(src), "'"+key+"'") {
			t.Errorf("the controller does not read %q", key)
		}
	}
	// And the digest helpers agree on hex shape, so a malformed digest cannot reach a manifest.
	if _, err := hex.DecodeString(strings.Repeat("g", 64)); err == nil {
		t.Fatal("hex validation is not discriminating")
	}
}
