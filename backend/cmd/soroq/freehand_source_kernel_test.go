package main

import "testing"

func v2Recipe() FreehandSourceKernelRecipe {
	return FreehandSourceKernelRecipe{
		Schema:           freehandRecipeSchemaV2,
		Entrypoint:       "lib/main.dart",
		Target:           "flutter",
		BuildMode:        "profile",
		Flavor:           "",
		PlatformDillRel:  "bin/cache/.../platform_strong.dill",
		PlatformDillSHA:  "aaaa",
		GenKernelSHA:     "bbbb",
		DartDefines:      []string{},
		Experiments:      []string{},
		PackageConfigSHA: "base-pkgcfg-sha",
	}
}

// v2: the toolchain recipe digest is INVARIANT to package_config (dependency) changes so a
// dependency-bearing patch is not refused before compilation.
func TestRecipeDigestV2_ExcludesPackageConfig(t *testing.T) {
	base := v2Recipe()
	cand := v2Recipe()
	cand.PackageConfigSHA = "candidate-pkgcfg-with-riverpod"
	bd, err := base.recipeDigest()
	if err != nil {
		t.Fatal(err)
	}
	cd, err := cand.recipeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if bd != cd {
		t.Fatalf("v2 recipe digest must be invariant to package_config change, got base=%s cand=%s", bd, cd)
	}
}

// v2: every immutable toolchain/compiler/build-mode field MUST still change the digest.
func TestRecipeDigestV2_ToolchainFieldsStillBind(t *testing.T) {
	bd, _ := v2Recipe().recipeDigest()
	mutations := map[string]func(*FreehandSourceKernelRecipe){
		"schema":            func(r *FreehandSourceKernelRecipe) { r.Schema = "soroq.freehand.source_kernel_recipe.vX" },
		"platform_dill_sha": func(r *FreehandSourceKernelRecipe) { r.PlatformDillSHA = "different" },
		"gen_kernel_sha":    func(r *FreehandSourceKernelRecipe) { r.GenKernelSHA = "different" },
		"build_mode":        func(r *FreehandSourceKernelRecipe) { r.BuildMode = "release" },
		"flavor":            func(r *FreehandSourceKernelRecipe) { r.Flavor = "prod" },
		"entrypoint":        func(r *FreehandSourceKernelRecipe) { r.Entrypoint = "lib/other.dart" },
		"target":            func(r *FreehandSourceKernelRecipe) { r.Target = "vm" },
		"dart_defines":      func(r *FreehandSourceKernelRecipe) { r.DartDefines = []string{"X=1"} },
		"experiments":       func(r *FreehandSourceKernelRecipe) { r.Experiments = []string{"records"} },
		"platform_dill_rel": func(r *FreehandSourceKernelRecipe) { r.PlatformDillRel = "other/path.dill" },
	}
	for name, mut := range mutations {
		r := v2Recipe()
		mut(&r)
		d, err := r.recipeDigest()
		if err != nil {
			// schema mutation to an UNKNOWN value must be an error, not a silent same-digest.
			if name == "schema" {
				continue
			}
			t.Fatalf("recipeDigest(%s) error: %v", name, err)
		}
		if d == bd {
			t.Fatalf("mutating %s must change the v2 recipe digest (toolchain immutability)", name)
		}
	}
}

// v1 (legacy): the digest INCLUDES package_config, so a dependency change changes it (v1 bases cannot
// accept dependency patches — they must be rebuilt to v2). This preserves existing v1-baseline
// verification byte-for-byte.
func TestRecipeDigestV1_IncludesPackageConfig(t *testing.T) {
	base := v2Recipe()
	base.Schema = freehandRecipeSchemaV1
	cand := base
	cand.PackageConfigSHA = "candidate-pkgcfg-with-riverpod"
	bd, _ := base.recipeDigest()
	cd, _ := cand.recipeDigest()
	if bd == cd {
		t.Fatalf("v1 (legacy) recipe digest MUST include package_config (change it), but it was invariant")
	}
	// Empty schema behaves as v1 (legacy).
	empty := base
	empty.Schema = ""
	ed, _ := empty.recipeDigest()
	emptyCand := empty
	emptyCand.PackageConfigSHA = "x"
	ecd, _ := emptyCand.recipeDigest()
	if ed == ecd {
		t.Fatalf("empty-schema recipe must use legacy (package_config-inclusive) digest")
	}
}

// v2 and v1 digests of the same fields differ (distinct binding), and v2 excludes package_config while
// v1 includes it — so a v1 baseline is never mistaken for a v2 one.
func TestRecipeDigest_V1V2Distinct(t *testing.T) {
	v2 := v2Recipe()
	v1 := v2
	v1.Schema = freehandRecipeSchemaV1
	d2, _ := v2.recipeDigest()
	d1, _ := v1.recipeDigest()
	if d1 == d2 {
		t.Fatal("v1 and v2 recipe digests must differ")
	}
}

// An unknown recipe schema is rejected (fail-closed), not silently digested.
func TestRecipeDigest_UnknownSchemaRejected(t *testing.T) {
	r := v2Recipe()
	r.Schema = "soroq.freehand.source_kernel_recipe.v999"
	if _, err := r.recipeDigest(); err == nil {
		t.Fatal("unknown recipe schema must be rejected")
	}
}
