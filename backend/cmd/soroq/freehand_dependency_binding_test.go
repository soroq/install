package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/depgraph"
)

// depPkg builds a strictly-valid dependency-graph package record.
func depPkg(name, ver string, eligible bool, deps ...string) depgraph.Package {
	if deps == nil {
		deps = []string{}
	}
	cap := depgraph.Capability{Eligible: true}
	if !eligible {
		cap = depgraph.Capability{
			Eligible: false, HasNativePlugin: true,
			NativeDetail: "plugin.platforms: ios.pluginClass",
			Reasons:      []string{"declares native platform plugin code"},
		}
	}
	return depgraph.Package{
		Name: name, Version: ver, Source: depgraph.SourceHosted,
		SourceID: "hosted:pub.dev/" + name, ContentHash: strings.Repeat("c", 64),
		PubspecSHA: strings.Repeat("d", 64), Dependencies: deps, Capability: cap,
	}
}

func depGraphOf(pkgs ...depgraph.Package) depgraph.Graph {
	m := map[string]depgraph.Package{}
	var roots []string
	for _, p := range pkgs {
		m[p.Name] = p
		roots = append(roots, p.Name)
	}
	g := depgraph.Graph{
		Schema:           depgraph.GraphSchema,
		GeneratorVersion: depgraph.GeneratorVersion,
		PubspecLockSHA:   strings.Repeat("1", 64),
		PackageConfigSHA: strings.Repeat("2", 64),
		RootPackage:      "app",
		Roots:            roots,
		Packages:         m,
	}
	g.GraphDigest = g.RecomputeDigest()
	return g
}

// testDependencyDescriptor is the canonical "added one Dart-only package" descriptor used by the
// artifact fixtures. It is a REAL descriptor: it passes DecodeStrict and Assess.
func testDependencyDescriptor() depgraph.Descriptor {
	base := depGraphOf(depPkg("meta", "1.0.0", true))
	cand := depGraphOf(depPkg("meta", "1.0.0", true), depPkg("riverpod", "2.6.1", true))
	return depgraph.BuildDescriptor(base, cand)
}

func TestArtifact_DependencyDescriptorIsBoundIntoIdentity(t *testing.T) {
	dir := t.TempDir()
	id, meta, _ := buildValidFreehandArtifact(t, dir)
	if err := verifyExistingPatchArtifact(dir, id); err != nil {
		t.Fatalf("a valid artifact with a dependency descriptor must verify: %v", err)
	}
	if meta.DependencyDescriptorDigest == "" || meta.BaseDependencyGraphDigest == "" {
		t.Fatal("artifact metadata must record the descriptor digest and the base graph anchor")
	}
	// A DIFFERENT dependency delta must yield a DIFFERENT artifact id, with everything else identical.
	other := depgraph.BuildDescriptor(
		depGraphOf(depPkg("meta", "1.0.0", true)),
		depGraphOf(depPkg("meta", "1.0.0", true), depPkg("collection", "1.19.1", true)),
	)
	if other.DescriptorDigest == meta.DependencyDescriptorDigest {
		t.Fatal("test setup: the two descriptors should differ")
	}
	same := computeFreehandArtifactID(meta.PatchPlanSHA256, meta.ToolchainBindingDigest, meta.ModuleManifestSHA256, meta.DependencyDescriptorDigest)
	diff := computeFreehandArtifactID(meta.PatchPlanSHA256, meta.ToolchainBindingDigest, meta.ModuleManifestSHA256, other.DescriptorDigest)
	if same != id {
		t.Fatalf("artifact id must be reproducible: %s != %s", same, id)
	}
	if diff == id {
		t.Fatal("a different dependency descriptor must produce a distinct artifact id")
	}
}

// editArtifactDescriptor rewrites dependency_descriptor.json and, when rebind is true, recomputes EVERY
// outer hash that depends on it (its sha, patch_artifact.json's recorded digests, and the artifact id) so
// the artifact is fully self-consistent. This is the strong tamper model: hash checks alone cannot catch it.
func editArtifactDescriptor(t *testing.T, dir string, mutate func(*depgraph.Descriptor), rebind bool) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, freehandDependencyDescriptorFile))
	if err != nil {
		t.Fatal(err)
	}
	var d depgraph.Descriptor
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	mutate(&d)
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, freehandDependencyDescriptorFile), out, 0o600); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(dir, "patch_artifact.json")
	metaRaw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta FreehandPatchArtifactMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}
	if rebind {
		meta.DependencyDescriptorSHA256 = freehandSHA256Bytes(out)
		meta.DependencyDescriptorDigest = d.DescriptorDigest
		meta.BaseDependencyGraphDigest = d.BaseGraphDigest
		meta.ArtifactID = computeFreehandArtifactID(meta.PatchPlanSHA256, meta.ToolchainBindingDigest, meta.ModuleManifestSHA256, d.DescriptorDigest)
	}
	newMeta, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, newMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	return meta.ArtifactID
}

func TestArtifact_DescriptorTamper_NotRebound_Refused(t *testing.T) {
	dir := t.TempDir()
	id, _, _ := buildValidFreehandArtifact(t, dir)
	editArtifactDescriptor(t, dir, func(d *depgraph.Descriptor) {
		d.Added[0].Version = "9.9.9"
	}, false)
	if err := verifyExistingPatchArtifact(dir, id); err == nil {
		t.Fatal("an edited dependency descriptor must fail artifact verification")
	}
}

// The core requirement: FULLY REBOUND tamper. The attacker edits the descriptor, recomputes its own
// digest, its file sha, the artifact metadata digests and the artifact id — so every hash agrees — and it
// must STILL be refused, on semantic grounds.
func TestArtifact_DescriptorTamper_FullyRebound_StillRefused(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*depgraph.Descriptor)
		wantMsg string
	}{
		{
			// Sneak a native plugin in while deleting the refusal record and recomputing everything.
			name: "suppressed native-plugin refusal",
			mutate: func(d *depgraph.Descriptor) {
				np := depPkg("camera", "0.10.0", false)
				d.Added = append(d.Added, np)
				d.Ineligible = nil // suppress the refusal
				d.DescriptorDigest = ""
			},
			wantMsg: "refusal was suppressed",
		},
		{
			// Forge the eligible flag but leave the ineligible entry: contradictory records.
			name: "forged eligible flag",
			mutate: func(d *depgraph.Descriptor) {
				np := depPkg("camera", "0.10.0", false)
				d.Added = append(d.Added, np)
				d.Ineligible = []depgraph.IneligiblePackage{{
					Name: "camera", Version: "0.10.0",
					Reasons: []string{"native"}, Message: "camera requires a new App Store/Play Store release",
				}}
				d.Added[len(d.Added)-1].Capability.Eligible = true
				d.DescriptorDigest = ""
			},
			wantMsg: "contradicts itself",
		},
		{
			// Swap the base anchor to a graph this patch was never built against.
			name: "swapped base graph anchor",
			mutate: func(d *depgraph.Descriptor) {
				d.BaseGraphDigest = strings.Repeat("9", 64)
				d.DescriptorDigest = ""
			},
			wantMsg: "", // any refusal is acceptable; the module manifest digest no longer matches
		},
		{
			// Claim a package is both added and unchanged.
			name: "package in two categories",
			mutate: func(d *depgraph.Descriptor) {
				d.Unchanged = append(d.Unchanged, d.Added[0].Name)
				d.DescriptorDigest = ""
			},
			wantMsg: "both",
		},
		{
			// Leak a developer-local absolute path into the source identity.
			name: "developer-local path",
			mutate: func(d *depgraph.Descriptor) {
				d.Added[0].SourceID = "path:/Users/attacker/riverpod"
				d.DescriptorDigest = ""
			},
			wantMsg: "developer-local",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			buildValidFreehandArtifact(t, dir)
			newID := editArtifactDescriptor(t, dir, func(d *depgraph.Descriptor) {
				tc.mutate(d)
				if d.DescriptorDigest == "" {
					// Rebind the descriptor's OWN digest so it is internally consistent.
					d.DescriptorDigest = depgraph.RecomputeDescriptorDigest(*d)
				}
			}, true)
			err := verifyExistingPatchArtifact(dir, newID)
			if err == nil {
				t.Fatal("a fully rebound descriptor tamper must still be refused")
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("expected refusal mentioning %q, got: %v", tc.wantMsg, err)
			}
		})
	}
}

func TestArtifact_ModuleManifestPairedWithDifferentDescriptor_Refused(t *testing.T) {
	dir := t.TempDir()
	id, _, _ := buildValidFreehandArtifact(t, dir)
	// Repoint ONLY the module manifest's recorded descriptor digest, then rebind the manifest sha and the
	// artifact id so every hash is consistent. The module must not be pairable with another delta.
	mPath := filepath.Join(dir, "soroq_freehand_module_manifest.json")
	raw, err := os.ReadFile(mPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["dependency_descriptor_digest"] = strings.Repeat("7", 64)
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(mPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(dir, "patch_artifact.json")
	metaRaw, _ := os.ReadFile(metaPath)
	var meta FreehandPatchArtifactMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}
	meta.ModuleManifestSHA256 = freehandSHA256Bytes(out)
	meta.ArtifactID = computeFreehandArtifactID(meta.PatchPlanSHA256, meta.ToolchainBindingDigest, meta.ModuleManifestSHA256, meta.DependencyDescriptorDigest)
	newMeta, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(metaPath, newMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = id
	err = verifyExistingPatchArtifact(dir, meta.ArtifactID)
	if err == nil || !strings.Contains(err.Error(), "synthesized under a different dependency delta") {
		t.Fatalf("a module manifest paired with a different descriptor must be refused, got %v", err)
	}
}

func TestArtifact_MissingDescriptorFile_Refused(t *testing.T) {
	dir := t.TempDir()
	id, _, _ := buildValidFreehandArtifact(t, dir)
	if err := os.Remove(filepath.Join(dir, freehandDependencyDescriptorFile)); err != nil {
		t.Fatal(err)
	}
	if err := verifyExistingPatchArtifact(dir, id); err == nil {
		t.Fatal("an artifact missing its dependency descriptor must be refused")
	}
}

func TestSignedDeviceManifest_BindsDependencyDescriptorDigest(t *testing.T) {
	dir := t.TempDir()
	_, meta, _ := buildValidFreehandArtifact(t, dir)
	m, _, err := buildFreehandDeviceManifest(dir, 1, "soroq_freehand_v1.bytecode")
	if err != nil {
		t.Fatalf("device manifest build failed: %v", err)
	}
	if m.DependencyDescriptorDigest != meta.DependencyDescriptorDigest {
		t.Fatalf("the SIGNED manifest must bind the dependency descriptor digest: %q != %q",
			m.DependencyDescriptorDigest, meta.DependencyDescriptorDigest)
	}
	// It must survive the producer-strict validator (i.e. it is a recognised, not an unknown, field).
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFreehandDeviceManifest(raw); err != nil {
		t.Fatalf("signed manifest with a descriptor digest must validate: %v", err)
	}
	if !strings.Contains(string(raw), meta.DependencyDescriptorDigest) {
		t.Fatal("the descriptor digest must be inside the bytes the signature covers")
	}
}

func TestBindDependencyDigestIntoManifest(t *testing.T) {
	digest := strings.Repeat("a", 64)
	orig := []byte(`{"schema":"soroq.freehand.module.v2","module_library":"lib","replacement_abi":[],"carried_new_code":[],"needs_flutter_target":true}`)

	out, err := bindDependencyDigestIntoManifest(orig, digest)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["dependency_descriptor_digest"] != digest {
		t.Fatalf("digest not bound: %v", got["dependency_descriptor_digest"])
	}
	// Every original member must survive untouched.
	for k, want := range map[string]any{
		"schema": "soroq.freehand.module.v2", "module_library": "lib", "needs_flutter_target": true,
	} {
		if got[k] != want {
			t.Fatalf("field %q changed: %v != %v", k, got[k], want)
		}
	}
	// Deterministic: the same input must produce byte-identical output (the SHA is bound into the id).
	again, err := bindDependencyDigestIntoManifest(orig, digest)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Fatal("manifest binding must be byte-deterministic")
	}
	if _, err := bindDependencyDigestIntoManifest(append(orig, '{', '}'), digest); err == nil {
		t.Fatal("trailing JSON in the synthesized manifest must be refused")
	}
	// A manifest that already claims a DIFFERENT digest must be refused, not silently overwritten.
	conflicting := []byte(`{"schema":"s","dependency_descriptor_digest":"` + strings.Repeat("b", 64) + `"}`)
	if _, err := bindDependencyDigestIntoManifest(conflicting, digest); err == nil {
		t.Fatal("a conflicting pre-existing digest must be refused")
	}
}
