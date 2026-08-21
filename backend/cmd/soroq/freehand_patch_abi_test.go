package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The six T002 direct-redirection targets, exactly as the analyzer emits them for the real fixture:
// one top-level field initializer, one static field initializer, an unnamed and a named generative
// constructor, a factory, and a plain function. This is the shape the whole route has to survive.
func t002Decls() []abiDecl {
	const lib = "package:app/main.dart"
	return []abiDecl{
		{manifestLine: lib + "::::init:gTopLevelFinal",
			stableKey: "v1|" + lib + "|field-initializer||gTopLevelFinal|ce90b6f0",
			class:     "", member: "init:gTopLevelFinal", kind: "function"},
		{manifestLine: lib + "::StaticInitProbe::init:value",
			stableKey: "v1|" + lib + "|field-initializer|StaticInitProbe|value|ce90b6f0",
			class:     "StaticInitProbe", member: "init:value", kind: "static-method"},
		{manifestLine: lib + "::EagerProviderProbe::EagerProviderProbe.",
			stableKey: "v1|" + lib + "|constructor|EagerProviderProbe||9e7dc67f",
			class:     "EagerProviderProbe", member: "EagerProviderProbe.", kind: "instance-member"},
		{manifestLine: lib + "::NamedCtorProbe::NamedCtorProbe.seeded",
			stableKey: "v1|" + lib + "|constructor|NamedCtorProbe|seeded|4a26b989",
			class:     "NamedCtorProbe", member: "NamedCtorProbe.seeded", kind: "instance-member"},
		{manifestLine: lib + "::FactoryCtorProbe::FactoryCtorProbe.make",
			stableKey: "v1|" + lib + "|factory|FactoryCtorProbe|make|27480176",
			class:     "FactoryCtorProbe", member: "FactoryCtorProbe.make", kind: "static-method"},
		{manifestLine: lib + "::::otaValue",
			stableKey: "v1|" + lib + "|function||otaValue|c55a32db",
			class:     "", member: "otaValue", kind: "function"},
	}
}

func t002ManifestAndDiff(t *testing.T) ([]byte, []map[string]any) {
	t.Helper()
	dir := t.TempDir()
	buildFreehandArtifactFrom(t, dir, t002Decls())
	return readArtifactManifestAndDiff(t, dir)
}

// TestABI_T002Shapes_Accepted is the GREEN control: without it every refusal below could be produced by
// a validator that rejects the whole shape, which would look like coverage and be none.
func TestABI_T002Shapes_Accepted(t *testing.T) {
	manifest, diff := t002ManifestAndDiff(t)
	if _, err := parseAndValidateModuleManifest(manifest, diff, testDependencyDescriptor().DescriptorDigest); err != nil {
		t.Fatalf("the six valid direct-redirection shapes were REJECTED: %v", err)
	}
}

// abiIndexOf finds the replacement_abi entry whose base_identity ends with the given suffix.
func abiIndexOf(t *testing.T, m map[string]any, suffix string) map[string]any {
	t.Helper()
	for _, raw := range m["replacement_abi"].([]any) {
		e := raw.(map[string]any)
		if strings.HasSuffix(e["base_identity"].(string), suffix) {
			return e
		}
	}
	t.Fatalf("no replacement_abi entry ending %q", suffix)
	return nil
}

// TestABI_PlantedFailures plants ONE defect at a time and requires the validator to refuse it, with the
// refusal text naming the reason. A planted failure that still validates is the finding, not a pass.
func TestABI_PlantedFailures(t *testing.T) {
	manifest, diff := t002ManifestAndDiff(t)

	cases := []struct {
		name    string
		wantMsg string // substring the refusal must contain, so a refusal for the WRONG reason still fails
		edit    func(m map[string]any)
		// diffEdit rewrites the plan's changed identities TOO. Mutating only the manifest makes the entry
		// unresolvable against the diff, so the bijection check fires and the VM-name check is never
		// reached; that would look like coverage of a rule the test never exercised. These cases plant the
		// real defect instead: an analyzer that emits the WRONG VM name consistently at both ends -- which
		// is precisely the T001 defect this validator exists to catch.
		diffEdit func(d []map[string]any)
	}{
		{
			// A generative constructor is a NON-static VM function. Labelling it static-method makes the
			// engine's `base.is_static() != patch.is_static()` gate throw and commit nothing.
			name: "constructor-labelled-static", wantMsg: "is built by the VM as",
			edit: func(m map[string]any) { abiIndexOf(t, m, "::EagerProviderProbe.")["kind"] = "static-method" },
		},
		{
			// A factory IS static. Labelling it instance-member is the mirror error.
			name: "factory-labelled-instance", wantMsg: "is built by the VM as",
			edit: func(m map[string]any) { abiIndexOf(t, m, "::FactoryCtorProbe.make")["kind"] = "instance-member" },
		},
		{
			name: "toplevel-field-initializer-labelled-static-method", wantMsg: "inconsistent with class",
			edit: func(m map[string]any) { abiIndexOf(t, m, "::init:gTopLevelFinal")["kind"] = "static-method" },
		},
		{
			name: "static-field-initializer-labelled-function", wantMsg: "inconsistent with class",
			edit: func(m map[string]any) { abiIndexOf(t, m, "StaticInitProbe::init:value")["kind"] = "function" },
		},
		{
			// THE DOT. `Foo` is not the VM's name for the unnamed constructor and matches nothing.
			name: "module-member-missing-constructor-dot", wantMsg: "!= the VM name",
			edit: func(m map[string]any) {
				abiIndexOf(t, m, "::EagerProviderProbe.")["module_member"] = "EagerProviderProbe"
			},
		},
		{
			// The whole T001 defect in one case: the analyzer names the unnamed constructor `Foo` at BOTH
			// ends, consistently, so every bijection check passes and the identity matches nothing on device.
			name: "analyzer-emits-dotless-constructor-everywhere", wantMsg: "matches nothing at runtime",
			edit: func(m map[string]any) {
				e := abiIndexOf(t, m, "::EagerProviderProbe.")
				e["base_identity"] = strings.TrimSuffix(e["base_identity"].(string), ".")
				// module_member stays CORRECT on purpose: the base-side VM name is a separate rule, and
				// this case has to REACH it rather than be short-circuited by the module-side one.
			},
			diffEdit: func(d []map[string]any) {
				for _, c := range d {
					if ml, _ := c["manifestLine"].(string); strings.HasSuffix(ml, "::EagerProviderProbe.") {
						c["manifestLine"] = strings.TrimSuffix(ml, ".")
					}
				}
			},
		},
		{
			name: "module-member-missing-init-prefix", wantMsg: "!= the VM name",
			edit: func(m map[string]any) { abiIndexOf(t, m, "::init:gTopLevelFinal")["module_member"] = "gTopLevelFinal" },
		},
		{
			name: "analyzer-emits-init-identity-without-the-prefix", wantMsg: "matches nothing at runtime",
			edit: func(m map[string]any) {
				e := abiIndexOf(t, m, "::init:gTopLevelFinal")
				e["base_identity"] = "package:app/main.dart::::gTopLevelFinal"
				// module_member stays CORRECT on purpose: the base-side VM name is a separate rule, and
				// this case has to REACH it rather than be short-circuited by the module-side one.
			},
			diffEdit: func(d []map[string]any) {
				for _, c := range d {
					if ml, _ := c["manifestLine"].(string); strings.HasSuffix(ml, "::init:gTopLevelFinal") {
						c["manifestLine"] = "package:app/main.dart::::gTopLevelFinal"
					}
				}
			},
		},
		{
			// A semantic kind must never reach the ABI: SoroqKindConsistent throws on it.
			name: "semantic-kind-in-abi", wantMsg: "unknown kind",
			edit: func(m map[string]any) { abiIndexOf(t, m, "::EagerProviderProbe.")["kind"] = "constructor" },
		},
		{
			name: "field-initializer-kind-in-abi", wantMsg: "unknown kind",
			edit: func(m map[string]any) { abiIndexOf(t, m, "::init:value")["kind"] = "field-initializer" },
		},
		{
			name: "signature-mismatch-malformed", wantMsg: "malformed signature_sha256",
			edit: func(m map[string]any) { abiIndexOf(t, m, "::NamedCtorProbe.seeded")["signature_sha256"] = "not-a-hash" },
		},
		{
			name: "missing-entry", wantMsg: "has no replacement_abi entry",
			edit: func(m map[string]any) {
				abi := m["replacement_abi"].([]any)
				m["replacement_abi"] = abi[1:]
			},
		},
		{
			name: "extra-entry", wantMsg: "not a changed identity",
			edit: func(m map[string]any) {
				abi := m["replacement_abi"].([]any)
				extra := map[string]any{}
				for k, v := range abi[0].(map[string]any) {
					extra[k] = v
				}
				extra["base_identity"] = "package:app/main.dart::Ghost::Ghost."
				extra["stable_identity"] = "v1|package:app/main.dart|constructor|Ghost||deadbeef"
				extra["module_class"] = "Ghost"
				extra["module_member"] = "Ghost."
				extra["kind"] = "instance-member"
				m["replacement_abi"] = append(abi, extra)
			},
		},
		{
			// The two constructors have the same shape, so ONLY the (base,stable) pairing catches a swap.
			name: "swapped-stable-identities", wantMsg: "stable_identity",
			edit: func(m map[string]any) {
				a := abiIndexOf(t, m, "::EagerProviderProbe.")
				b := abiIndexOf(t, m, "::NamedCtorProbe.seeded")
				a["stable_identity"], b["stable_identity"] = b["stable_identity"], a["stable_identity"]
			},
		},
		{
			name: "duplicate-entry", wantMsg: "duplicate replacement_abi",
			edit: func(m map[string]any) {
				abi := m["replacement_abi"].([]any)
				m["replacement_abi"] = append(abi, abi[0])
			},
		},
		{
			name: "module-class-does-not-match-frozen-key", wantMsg: "module_class",
			edit: func(m map[string]any) { abiIndexOf(t, m, "::NamedCtorProbe.seeded")["module_class"] = "EagerProviderProbe" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := mutateJSONBytes(t, manifest, tc.edit)
			useDiff := diff
			if tc.diffEdit != nil {
				useDiff = cloneDiff(t, diff)
				tc.diffEdit(useDiff)
			}
			_, err := parseAndValidateModuleManifest(mutated, useDiff, testDependencyDescriptor().DescriptorDigest)
			if err == nil {
				t.Fatalf("PLANTED FAILURE %q was ACCEPTED — the validator cannot detect it", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("refused %q for the WRONG reason: want a message containing %q, got %v", tc.name, tc.wantMsg, err)
			}
		})
	}
}

// TestExpectABI_UnknownSemanticKindFailsClosed: a frozen kind whose VM shape nobody measured must be an
// error, never a pass-through. A silent default is how an unsupported shape reaches the device.
func TestExpectABI_UnknownSemanticKindFailsClosed(t *testing.T) {
	for _, kind := range []string{"field", "extension-member", "mixin-member", ""} {
		if _, err := expectABI(changedDecl{keyKind: kind, keyClass: "C", keyMember: "m"}); err == nil {
			t.Fatalf("expectABI accepted unmeasured frozen kind %q", kind)
		}
	}
}

// TestExpectABI_MeasuredShapes pins the mapping itself, so a future edit that "simplifies" a
// constructor into a static-method has to change a test that says why it is wrong.
func TestExpectABI_MeasuredShapes(t *testing.T) {
	cases := []struct {
		d      changedDecl
		vmName string
		kind   string
	}{
		{changedDecl{keyKind: "constructor", keyClass: "Foo", keyMember: ""}, "Foo.", "instance-member"},
		{changedDecl{keyKind: "constructor", keyClass: "Foo", keyMember: "named"}, "Foo.named", "instance-member"},
		{changedDecl{keyKind: "factory", keyClass: "Foo", keyMember: "make"}, "Foo.make", "static-method"},
		{changedDecl{keyKind: "field-initializer", keyClass: "", keyMember: "g"}, "init:g", "function"},
		{changedDecl{keyKind: "field-initializer", keyClass: "C", keyMember: "v"}, "init:v", "static-method"},
		{changedDecl{keyKind: "getter", keyClass: "C", keyMember: "text"}, "get:text", "instance-member"},
		{changedDecl{keyKind: "setter", keyClass: "C", keyMember: "text"}, "set:text", "instance-member"},
		{changedDecl{keyKind: "function", keyClass: "", keyMember: "otaValue"}, "otaValue", "function"},
	}
	for _, c := range cases {
		exp, err := expectABI(c.d)
		if err != nil {
			t.Fatalf("expectABI(%+v): %v", c.d, err)
		}
		if exp.vmName != c.vmName {
			t.Errorf("expectABI(%s %s.%s) vmName = %q, want %q", c.d.keyKind, c.d.keyClass, c.d.keyMember, exp.vmName, c.vmName)
		}
		if !exp.kinds[c.kind] {
			t.Errorf("expectABI(%s %s.%s) kinds = %s, want to include %q", c.d.keyKind, c.d.keyClass, c.d.keyMember, exp.kindList(), c.kind)
		}
		for k := range exp.kinds {
			if !abiKinds[k] {
				t.Errorf("expectABI(%s) allows %q, which the runtime does not accept", c.d.keyKind, k)
			}
		}
	}
}

// readArtifactManifestAndDiff reads the module manifest bytes and the plan's diff.changed from a built
// artifact directory.
func readArtifactManifestAndDiff(t *testing.T, dir string) ([]byte, []map[string]any) {
	t.Helper()
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

// cloneDiff deep-copies the plan's changed identities so a planted edit cannot leak into another case.
func cloneDiff(t *testing.T, in []map[string]any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
