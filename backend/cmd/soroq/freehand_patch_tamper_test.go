package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// abiDecl is one changed-patchable declaration used to build a self-consistent test artifact: the ABI entry,
// the plan diff entry, and changed_identities are all derived from it so the real cross-checks apply.
type abiDecl struct {
	manifestLine string // base_identity
	stableKey    string // stable_identity: "v1|libUri|frozenKind|class|member|sigShort"
	class        string // module_class (== key class segment)
	member       string // module_member (== key member segment)
	kind         string // ABI kind: function | static-method | instance-member
}

func defaultDecls() []abiDecl {
	return []abiDecl{{
		manifestLine: "package:app/main.dart::_DailySummary::build",
		stableKey:    "v1|package:app/main.dart|method|_DailySummary|build|797de60e",
		class:        "_DailySummary", member: "build", kind: "instance-member",
	}}
}

// buildFreehandArtifactFrom writes a fully-consistent immutable artifact for the given changed decls: the
// manifest ABI, the plan diff.changed, changed_identities, and every SHA/digest/id are derived from real
// bytes exactly as the generator does. Returns the artifact id + the valid metadata.
func buildFreehandArtifactFrom(t *testing.T, dir string, decls []abiDecl) (string, FreehandPatchArtifactMeta) {
	t.Helper()
	const moduleGraphDigest = "1111111111111111111111111111111111111111111111111111111111111111"
	const moduleLib = "soroq-freehand:///import/prefix/1111111111111111111111111111111111111111111111111111111111111111/soroq_freehand_module.dart"

	abi := make([]any, 0, len(decls))
	diffChanged := make([]any, 0, len(decls))
	changedLines := make([]string, 0, len(decls))
	changedPatchable := make([]any, 0, len(decls))
	for _, d := range decls {
		frozenKind := strings.Split(d.stableKey, "|")[2]
		abi = append(abi, map[string]any{
			"base_identity": d.manifestLine, "stable_identity": d.stableKey, "module_library": moduleLib,
			"module_class": d.class, "module_member": d.member, "kind": d.kind,
			"signature_sha256": freehandSHA256Bytes([]byte("sig-" + d.member)), "host_invocable": false,
		})
		diffChanged = append(diffChanged, map[string]any{
			"key": d.stableKey, "manifestLine": d.manifestLine, "kind": frozenKind,
			"bodyDigest": freehandSHA256Bytes([]byte("body-" + d.member)), "patchable": true,
		})
		changedLines = append(changedLines, d.manifestLine)
		changedPatchable = append(changedPatchable, d.manifestLine)
	}

	// Every artifact carries a real, strictly-valid dependency descriptor: it is bound into the module
	// manifest, patch_plan.json, patch_artifact.json AND the artifact id.
	descriptor := testDependencyDescriptor()
	descriptorBytes, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	descriptorSHA := freehandSHA256Bytes(descriptorBytes)

	moduleSrcBytes := []byte("// module\nObject? dynamicModuleEntrypoint() => 'ok';\n")
	moduleBytecodeBytes := []byte("\x00bytecode-bytes\x01")

	manifest := map[string]any{
		"schema": "soroq.freehand.module.v2", "module_source_basename": "soroq_freehand_module.dart",
		"module_library": moduleLib, "module_graph_digest": moduleGraphDigest,
		"carried_libraries": []any{}, "module_source_sha256": freehandSHA256Bytes(moduleSrcBytes),
		"needs_flutter_target": true, "imports": []any{},
		"extracted_top_level_functions": []any{}, "extracted_classes": []any{},
		"carried_changed_members": []any{}, "value_exercised": []any{},
		"host_invoked_instance_methods": []any{}, "replacement_abi": abi, "carried_new_code": []any{},
		"dependency_descriptor_digest": descriptor.DescriptorDigest,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	manifestSHA := freehandSHA256Bytes(manifestBytes)

	plan := map[string]any{
		"schema": "soroq.freehand.patch_plan.v1", "runtime_id": "rt-1",
		"changed_patchable": changedPatchable, "new_code_closure": []any{},
		"dependency_descriptor_digest": descriptor.DescriptorDigest,
		"diff": map[string]any{
			"schema": "soroq.freehand.diff.v1", "identitySchema": "v1", "supported": true,
			"changed": diffChanged, "changedPatchable": changedPatchable, "newCodeClosure": []any{},
		},
	}
	planBytes, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	planSHA := freehandSHA256Bytes(planBytes)

	binding := FreehandToolchainBinding{
		ToolchainVersion: "test-toolchain", Dart2BytecodeSHA256: freehandSHA256Bytes([]byte("d2b")),
		DartAotRuntimeSHA256: freehandSHA256Bytes([]byte("aot")), PlatformDillSHA256: freehandSHA256Bytes([]byte("plat")),
		AnalyzerSnapshotSHA256: freehandSHA256Bytes([]byte("analyzer")), ModuleSchema: "soroq.freehand.module.v1",
	}
	bindingDigest, err := binding.digest()
	if err != nil {
		t.Fatal(err)
	}
	artifactID := computeFreehandArtifactID(planSHA, bindingDigest, manifestSHA, descriptor.DescriptorDigest)

	// Every artifact carries a COMPLETE rich base identity, derived the way the real patch path derives
	// it: base_fingerprint is the artifact's own base app.dill sha, and the digest is recomputed rather
	// than written by hand, so a fixture can never encode a digest the production code would reject.
	baseIdentity, err := newFreehandRichBaseIdentity(
		"rt-1",
		freehandSHA256Bytes([]byte("baseapp")),
		freehandSHA256Bytes([]byte("contract")),
		freehandSHA256Bytes([]byte("retention")),
	)
	if err != nil {
		t.Fatal(err)
	}

	meta := FreehandPatchArtifactMeta{
		Schema: "soroq.freehand.patch_artifact.v2", RuntimeID: "rt-1", IdentitySchema: "v1",
		AppID: "com.shreyansh.calorietracker", Version: "1.0.0", Channel: "stable",
		BaseAppDillSHA256: freehandSHA256Bytes([]byte("baseapp")), BaseIdentity: &baseIdentity,
		BaseSourceKernelSHA256: freehandSHA256Bytes([]byte("basesrc")),
		CandSourceKernelSHA256: freehandSHA256Bytes([]byte("candsrc")), SourceRecipeDigest: freehandSHA256Bytes([]byte("recipe")),
		PatchPlanSHA256: planSHA, ModuleSourceSHA256: freehandSHA256Bytes(moduleSrcBytes),
		ModuleBytecodeSHA256: freehandSHA256Bytes(moduleBytecodeBytes), ModuleManifestSHA256: manifestSHA,
		ModuleLibrary: moduleLib, ModuleGraphDigest: moduleGraphDigest,
		CarriedLibraries:   []freehandCarriedLibrary{},
		NeedsFlutterTarget: true, ToolchainBinding: &binding,
		ToolchainBindingDigest: bindingDigest, ArtifactID: artifactID,
		CompilationInputDigest: freehandSHA256Bytes([]byte("input")), ChangedIdentities: changedLines,
		ClosureIdentities:          []string{},
		DependencyDescriptorDigest: descriptor.DescriptorDigest,
		DependencyDescriptorSHA256: descriptorSHA,
		BaseDependencyGraphDigest:  descriptor.BaseGraphDigest,
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("soroq_freehand_module.dart", moduleSrcBytes)
	write("soroq_freehand_module.bytecode", moduleBytecodeBytes)
	write("soroq_freehand_module_manifest.json", manifestBytes)
	write(freehandDependencyDescriptorFile, descriptorBytes)
	write("patch_plan.json", planBytes)
	write("patch_artifact.json", metaBytes)
	return artifactID, meta
}

func buildValidFreehandArtifact(t *testing.T, dir string) (string, FreehandPatchArtifactMeta, []byte) {
	t.Helper()
	id, meta := buildFreehandArtifactFrom(t, dir, defaultDecls())
	mb, _ := os.ReadFile(filepath.Join(dir, "patch_artifact.json"))
	return id, meta, mb
}

func TestVerifyExistingPatchArtifact_AcceptsValid(t *testing.T) {
	dir := t.TempDir()
	id, _, _ := buildValidFreehandArtifact(t, dir)
	if err := verifyExistingPatchArtifact(dir, id); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}
}

// TestVerifyExistingPatchArtifact_RejectsTamper mutates each artifact member + each bound metadata/ABI field
// of a freshly-built valid artifact and asserts verification refuses it. Rebuild-per-case keeps cases isolated.
func TestVerifyExistingPatchArtifact_RejectsTamper(t *testing.T) {
	editManifest := func(t *testing.T, dir string, edit func(m map[string]any)) {
		editJSONFile(t, filepath.Join(dir, "soroq_freehand_module_manifest.json"), edit)
	}
	editABI0 := func(t *testing.T, dir string, edit func(e map[string]any)) {
		editManifest(t, dir, func(m map[string]any) { edit(m["replacement_abi"].([]any)[0].(map[string]any)) })
	}
	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string, meta FreehandPatchArtifactMeta)
	}{
		{"module-source-byte", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			flipByte(t, filepath.Join(dir, "soroq_freehand_module.dart"))
		}},
		{"module-bytecode-byte", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			flipByte(t, filepath.Join(dir, "soroq_freehand_module.bytecode"))
		}},
		{"patch-plan-byte", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			flipByte(t, filepath.Join(dir, "patch_plan.json"))
		}},
		{"manifest-wrong-schema", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editManifest(t, dir, func(m map[string]any) { m["schema"] = "evil.schema" })
		}},
		{"manifest-trailing-json", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			p := filepath.Join(dir, "soroq_freehand_module_manifest.json")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, append(b, []byte("\n{\"x\":1}\n")...), 0o600)
		}},
		{"manifest-unknown-top-field", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editManifest(t, dir, func(m map[string]any) { m["injected_top"] = true })
		}},
		{"abi-unknown-field", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editABI0(t, dir, func(e map[string]any) { e["injected_abi"] = true })
		}},
		{"abi-wrong-stable-identity", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editABI0(t, dir, func(e map[string]any) {
				e["stable_identity"] = "v1|package:app/main.dart|method|_DailySummary|build|deadbeef"
			})
		}},
		{"abi-missing-stable-identity", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editABI0(t, dir, func(e map[string]any) { delete(e, "stable_identity") })
		}},
		{"abi-inconsistent-base-vs-stable", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			// base_identity stays valid but stable_identity is taken from a DIFFERENT declaration.
			editABI0(t, dir, func(e map[string]any) {
				e["stable_identity"] = "v1|package:app/main.dart|function||topHelper|abc12345"
			})
		}},
		{"abi-duplicate-stable-identity", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editManifest(t, dir, func(m map[string]any) {
				abiArr := m["replacement_abi"].([]any)
				dup := map[string]any{}
				for k, v := range abiArr[0].(map[string]any) {
					dup[k] = v
				}
				dup["base_identity"] = "package:app/main.dart::Other::x" // different base, SAME stable
				m["replacement_abi"] = append(abiArr, dup)
			})
		}},
		{"abi-wrong-per-entry-module-library", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editABI0(t, dir, func(e map[string]any) { e["module_library"] = "soroq.freehand.evil" })
		}},
		{"abi-wrong-module-class", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editABI0(t, dir, func(e map[string]any) { e["module_class"] = "_Evil" })
		}},
		{"abi-wrong-module-member", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editABI0(t, dir, func(e map[string]any) { e["module_member"] = "evilMember" })
		}},
		{"abi-inconsistent-kind", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editABI0(t, dir, func(e map[string]any) { e["kind"] = "function" }) // function but class non-empty
		}},
		{"abi-unknown-kind", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editABI0(t, dir, func(e map[string]any) { e["kind"] = "gadget" })
		}},
		{"abi-malformed-signature", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editABI0(t, dir, func(e map[string]any) { e["signature_sha256"] = "not-hex" })
		}},
		{"manifest-drop-abi-entry", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editManifest(t, dir, func(m map[string]any) { m["replacement_abi"] = []any{} })
		}},
		{"manifest-extra-abi-entry", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editManifest(t, dir, func(m map[string]any) {
				abiArr := m["replacement_abi"].([]any)
				extra := map[string]any{
					"base_identity": "package:app/main.dart::::ghost", "stable_identity": "v1|package:app/main.dart|function||ghost|deadbeef",
					"module_library": "soroq.freehand.module", "module_class": "", "module_member": "ghost",
					"kind": "function", "signature_sha256": freehandSHA256Bytes([]byte("g")), "host_invocable": false,
				}
				m["replacement_abi"] = append(abiArr, extra)
			})
		}},
		{"manifest-wrong-module-library", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			// top-level module_library changed → manifest hash mismatch (also != meta.ModuleLibrary).
			editManifest(t, dir, func(m map[string]any) { m["module_library"] = "soroq.freehand.evil" })
		}},
		{"artifact-plan-sha-field", func(t *testing.T, dir string, meta FreehandPatchArtifactMeta) {
			meta.PatchPlanSHA256 = freehandSHA256Bytes([]byte("wrong-plan"))
			rewriteMeta(t, dir, meta)
		}},
		{"artifact-manifest-sha-field", func(t *testing.T, dir string, meta FreehandPatchArtifactMeta) {
			meta.ModuleManifestSHA256 = freehandSHA256Bytes([]byte("wrong-manifest"))
			rewriteMeta(t, dir, meta)
		}},
		{"artifact-id-field", func(t *testing.T, dir string, meta FreehandPatchArtifactMeta) {
			meta.ArtifactID = freehandSHA256Bytes([]byte("wrong-id"))
			rewriteMeta(t, dir, meta)
		}},
		{"artifact-binding-digest-field", func(t *testing.T, dir string, meta FreehandPatchArtifactMeta) {
			meta.ToolchainBindingDigest = freehandSHA256Bytes([]byte("wrong-binding"))
			rewriteMeta(t, dir, meta)
		}},
		{"artifact-module-library-field", func(t *testing.T, dir string, meta FreehandPatchArtifactMeta) {
			meta.ModuleLibrary = "soroq.freehand.evil"
			rewriteMeta(t, dir, meta)
		}},
		{"artifact-changed-identity-field", func(t *testing.T, dir string, meta FreehandPatchArtifactMeta) {
			meta.ChangedIdentities = []string{"package:app/main.dart::_Evil::build"}
			rewriteMeta(t, dir, meta)
		}},
		{"artifact-binding-tools-field", func(t *testing.T, dir string, meta FreehandPatchArtifactMeta) {
			b := *meta.ToolchainBinding
			b.Dart2BytecodeSHA256 = freehandSHA256Bytes([]byte("swapped-tool"))
			meta.ToolchainBinding = &b
			rewriteMeta(t, dir, meta)
		}},
		{"artifact-unknown-field", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			editJSONFile(t, filepath.Join(dir, "patch_artifact.json"), func(m map[string]any) { m["injected_unknown"] = true })
		}},
		{"artifact-trailing-json", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			p := filepath.Join(dir, "patch_artifact.json")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, append(b, []byte("\n{\"x\":1}\n")...), 0o600)
		}},
		{"member-symlink", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			p := filepath.Join(dir, "soroq_freehand_module.bytecode")
			os.Remove(p)
			if err := os.Symlink("/etc/hosts", p); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing-member", func(t *testing.T, dir string, _ FreehandPatchArtifactMeta) {
			os.Remove(filepath.Join(dir, "soroq_freehand_module_manifest.json"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			id, meta, _ := buildValidFreehandArtifact(t, dir)
			if err := verifyExistingPatchArtifact(dir, id); err != nil {
				t.Fatalf("precondition: valid artifact must verify, got %v", err)
			}
			tc.mutate(t, dir, meta)
			if err := verifyExistingPatchArtifact(dir, id); err == nil {
				t.Fatalf("tamper %q was ACCEPTED (expected rejection)", tc.name)
			}
		})
	}
}

// TestVerify_SwappedStableIdentity: with TWO changed decls, swapping the two entries' stable_identity fields
// keeps every value individually valid but breaks the (base_identity, stable_identity) pairing → must reject.
func TestVerify_SwappedStableIdentity(t *testing.T) {
	dir := t.TempDir()
	decls := []abiDecl{
		{manifestLine: "package:app/main.dart::_DailySummary::build",
			stableKey: "v1|package:app/main.dart|method|_DailySummary|build|797de60e",
			class:     "_DailySummary", member: "build", kind: "instance-member"},
		{manifestLine: "package:app/main.dart::::topHelper",
			stableKey: "v1|package:app/main.dart|function||topHelper|abc12345",
			class:     "", member: "topHelper", kind: "function"},
	}
	id, _ := buildFreehandArtifactFrom(t, dir, decls)
	if err := verifyExistingPatchArtifact(dir, id); err != nil {
		t.Fatalf("precondition: 2-decl artifact must verify, got %v", err)
	}
	editJSONFile(t, filepath.Join(dir, "soroq_freehand_module_manifest.json"), func(m map[string]any) {
		abi := m["replacement_abi"].([]any)
		e0, e1 := abi[0].(map[string]any), abi[1].(map[string]any)
		e0["stable_identity"], e1["stable_identity"] = e1["stable_identity"], e0["stable_identity"]
	})
	// REBIND the manifest SHA + artifact id so the integrity layer is satisfied — the (base,stable) PAIR
	// mismatch, not the hash, must be the rejecter.
	newID := rebindArtifact(t, dir)
	if err := verifyExistingPatchArtifact(dir, newID); err == nil {
		t.Fatal("swapped stable_identity pairing was ACCEPTED (expected rejection by pair-mismatch, not SHA)")
	}
}

// validManifestAndDiff returns a valid manifest (bytes) + the plan diff.changed it must validate against —
// so parseAndValidateModuleManifest can be exercised DIRECTLY, independent of the artifact SHA binding.
func validManifestAndDiff(t *testing.T) ([]byte, []map[string]any) {
	t.Helper()
	dir := t.TempDir()
	buildFreehandArtifactFrom(t, dir, defaultDecls())
	mb, err := os.ReadFile(filepath.Join(dir, "soroq_freehand_module_manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	pb, err := os.ReadFile(filepath.Join(dir, "patch_plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Diff struct {
			Changed []map[string]any `json:"changed"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(pb, &plan); err != nil {
		t.Fatal(err)
	}
	return mb, plan.Diff.Changed
}

func mutateJSONBytes(t *testing.T, b []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestParseAndValidateModuleManifest_Direct exercises the validator FUNCTION directly (no SHA binding in the
// way) — this is the layer the artifact-SHA check would otherwise mask. It must accept a valid manifest and
// reject every semantic tamper: schema, stable_identity, per-entry module_library, unknown/trailing JSON, and
// base-vs-stable inconsistency (the exact acceptances the earlier weak validator let through).
func TestParseAndValidateModuleManifest_Direct(t *testing.T) {
	valid, diff := validManifestAndDiff(t)
	if _, err := parseAndValidateModuleManifest(valid, diff, testDependencyDescriptor().DescriptorDigest); err != nil {
		t.Fatalf("valid manifest rejected by direct validation: %v", err)
	}
	abi0 := func(m map[string]any) map[string]any { return m["replacement_abi"].([]any)[0].(map[string]any) }
	cases := []struct {
		name  string
		bytes func() []byte
	}{
		{"schema", func() []byte {
			return mutateJSONBytes(t, valid, func(m map[string]any) { m["schema"] = "evil.schema" })
		}},
		{"top-module-library", func() []byte {
			return mutateJSONBytes(t, valid, func(m map[string]any) { m["module_library"] = "" })
		}},
		{"wrong-stable-identity", func() []byte {
			return mutateJSONBytes(t, valid, func(m map[string]any) {
				abi0(m)["stable_identity"] = "v1|package:app/main.dart|method|_DailySummary|build|deadbeef"
			})
		}},
		{"missing-stable-identity", func() []byte {
			return mutateJSONBytes(t, valid, func(m map[string]any) { delete(abi0(m), "stable_identity") })
		}},
		{"per-entry-module-library", func() []byte {
			return mutateJSONBytes(t, valid, func(m map[string]any) { abi0(m)["module_library"] = "soroq.freehand.evil" })
		}},
		{"wrong-module-class", func() []byte {
			return mutateJSONBytes(t, valid, func(m map[string]any) { abi0(m)["module_class"] = "_Evil" })
		}},
		{"inconsistent-kind", func() []byte {
			return mutateJSONBytes(t, valid, func(m map[string]any) { abi0(m)["kind"] = "function" })
		}},
		{"unknown-abi-field", func() []byte {
			return mutateJSONBytes(t, valid, func(m map[string]any) { abi0(m)["injected"] = true })
		}},
		{"unknown-top-field", func() []byte {
			return mutateJSONBytes(t, valid, func(m map[string]any) { m["injected"] = true })
		}},
		{"trailing-json", func() []byte { return append(append([]byte{}, valid...), []byte("\n{\"x\":1}\n")...) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseAndValidateModuleManifest(tc.bytes(), diff, testDependencyDescriptor().DescriptorDigest); err == nil {
				t.Fatalf("direct validation ACCEPTED tampered manifest (%s)", tc.name)
			}
		})
	}
}

// rebindArtifact recomputes the manifest/plan SHAs + artifact id from the (possibly tampered) files and
// rewrites patch_artifact.json so the INTEGRITY layer is fully satisfied — forcing the SEMANTIC ABI
// validation to be the rejecter. Returns the new artifact id.
func rebindArtifact(t *testing.T, dir string) string {
	t.Helper()
	manifestRaw, _ := os.ReadFile(filepath.Join(dir, "soroq_freehand_module_manifest.json"))
	planRaw, _ := os.ReadFile(filepath.Join(dir, "patch_plan.json"))
	metaRaw, _ := os.ReadFile(filepath.Join(dir, "patch_artifact.json"))
	var meta FreehandPatchArtifactMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}
	manifestSHA := freehandSHA256Bytes(manifestRaw)
	planSHA := freehandSHA256Bytes(planRaw)
	bindingDigest, _ := meta.ToolchainBinding.digest()
	meta.ModuleManifestSHA256 = manifestSHA
	meta.PatchPlanSHA256 = planSHA
	meta.ArtifactID = freehandSHA256Bytes([]byte(planSHA + "|" + bindingDigest + "|" + manifestSHA))
	// align the meta-level module_library to the manifest top-level so THAT check passes and the deeper
	// per-entry / stable-identity semantics become the rejecter.
	var mm map[string]any
	if json.Unmarshal(manifestRaw, &mm) == nil {
		if lib, ok := mm["module_library"].(string); ok {
			meta.ModuleLibrary = lib
		}
	}
	rewriteMeta(t, dir, meta)
	return meta.ArtifactID
}

// TestVerify_ReboundManifestTamper proves the SEMANTIC validation (not just the manifest-SHA binding) rejects
// tampering: after mutating the manifest we REBIND every SHA + the artifact id so the integrity layer passes,
// yet verification must still refuse. Covers the three acceptances the user proved: schema, stable_identity,
// per-entry module_library.
func TestVerify_ReboundManifestTamper(t *testing.T) {
	abi0 := func(m map[string]any) map[string]any { return m["replacement_abi"].([]any)[0].(map[string]any) }
	cases := []struct {
		name string
		edit func(m map[string]any)
	}{
		{"schema", func(m map[string]any) { m["schema"] = "evil.schema" }},
		{"stable-identity", func(m map[string]any) {
			abi0(m)["stable_identity"] = "v1|package:app/main.dart|method|_DailySummary|build|deadbeef"
		}},
		{"per-entry-module-library", func(m map[string]any) { abi0(m)["module_library"] = "soroq.freehand.evil" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			buildValidFreehandArtifact(t, dir)
			editJSONFile(t, filepath.Join(dir, "soroq_freehand_module_manifest.json"), tc.edit)
			newID := rebindArtifact(t, dir) // integrity layer now consistent with the tampered manifest
			if err := verifyExistingPatchArtifact(dir, newID); err == nil {
				t.Fatalf("rebound tamper %q was ACCEPTED (semantic validation must reject even when SHAs are consistent)", tc.name)
			}
		})
	}
}

// TestVerifyExistingPatchArtifact_RejectsWrongExpectedID: an untampered artifact must still be refused when
// the caller's expected identity does not match (guards against serving artifact A for request B).
func TestVerifyExistingPatchArtifact_RejectsWrongExpectedID(t *testing.T) {
	dir := t.TempDir()
	buildValidFreehandArtifact(t, dir)
	if err := verifyExistingPatchArtifact(dir, freehandSHA256Bytes([]byte("some-other-expected-id"))); err == nil {
		t.Fatal("verification accepted a mismatched expected artifact id")
	}
}

func flipByte(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		b = []byte{0}
	}
	b[len(b)-1] ^= 0xFF
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func editJSONFile(t *testing.T, path string, edit func(map[string]any)) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rewriteMeta(t *testing.T, dir string, meta FreehandPatchArtifactMeta) {
	t.Helper()
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "patch_artifact.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}
