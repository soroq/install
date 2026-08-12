package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"soroq/backend/internal/depgraph"
)

func writeTmp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func testRecipe() FreehandSourceKernelRecipe {
	return FreehandSourceKernelRecipe{
		Schema:           "soroq.freehand.source_kernel_recipe.v1",
		Entrypoint:       "lib/main.dart",
		Target:           "flutter",
		BuildMode:        "profile",
		PlatformDillRel:  "bin/cache/artifacts/engine/common/flutter_patched_sdk_product/platform_strong.dill",
		PlatformDillSHA:  "plat-sha",
		GenKernelSHA:     "genk-sha",
		DartDefines:      []string{},
		Experiments:      []string{},
		PackageConfigSHA: "cfg-sha",
	}
}

// testDepGraph is a minimal but FULLY VALID immutable base runtime dependency graph: it must satisfy
// exactly the same strict validation (schema, generator version, hash shapes, content pins, edge
// resolvability, canonical digest) that every production reader applies.
func testDepGraph() depgraph.Graph {
	pkg := func(name string, deps ...string) depgraph.Package {
		if deps == nil {
			deps = []string{}
		}
		return depgraph.Package{
			Name: name, Version: "1.0.0", Source: depgraph.SourceHosted,
			SourceID: "hosted:pub.dev/" + name, ContentHash: strings.Repeat("c", 64),
			PubspecSHA: strings.Repeat("d", 64), Dependencies: deps,
			Capability: depgraph.Capability{Eligible: true},
		}
	}
	g := depgraph.Graph{
		Schema:           depgraph.GraphSchema,
		GeneratorVersion: depgraph.GeneratorVersion,
		PubspecLockSHA:   strings.Repeat("1", 64),
		PackageConfigSHA: strings.Repeat("2", 64),
		RootPackage:      "app",
		Roots:            []string{"alpha"},
		Packages: map[string]depgraph.Package{
			"alpha": pkg("alpha", "beta"),
			"beta":  pkg("beta"),
		},
	}
	g.GraphDigest = g.RecomputeDigest()
	return g
}

func fullMeta() FreehandBaselineMeta {
	r := testRecipe()
	return FreehandBaselineMeta{
		IdentitySchema:      "soroq.freehand.identity.v1",
		AnalyzerVersion:     "analyzer-abc123",
		SourceKernelRecipe:  &r,
		PackageConfigSHA256: "cfg-sha",
		FrontendRev:         "fe-1",
		FrontendPatchsetSHA: "fps-1",
		FrameworkRev:        "fw-1",
		DartRev:             "dart-1",
		EngineRev:           "eng-1",
		RuntimeID:           "rt-aaaa",
		AppID:               "com.example.app",
		Version:             "1.0.0+1",
		Arch:                "arm64",
		Channel:             "stable",
		PatchableCount:      3,
		// A real freehand build reaches persistFreehandBaseline only after verifyFreehandStagingStrict
		// validated the analysis staging; the fixture mirrors that verified-retention evidence. AnalysisID
		// is the 64-hex content address of the verified staging (the persist path derives the count and
		// manifest/symbol-graph SHAs from the validated inputs).
		Retention: &FreehandRetentionEvidence{Verified: true, AnalysisID: strings.Repeat("a", 64)},
	}
}

// seedFixture returns (proj, appDill, sourceDill, manifest, graph). The source dill is the non-AOT
// companion required by the v2 dual-kernel baseline.
func seedFixture(t *testing.T) (proj, dill, srcDill, man, graph string) {
	t.Helper()
	proj = t.TempDir()
	dill = writeTmp(t, proj, "app.dill", "KERNEL-BYTES-A")
	srcDill = writeTmp(t, proj, "source_app.dill", "SOURCE-KERNEL-BYTES-A")
	man = writeTmp(t, proj, "manifest.txt", "pkg::Cls::m\n")
	graph = writeTmp(t, proj, "graph.json", `{"schema":"soroq.freehand.identity.v1"}`)
	return
}

func TestFreehandBaseline_PersistProvenanceAndPerms(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"app.dill", "soroq_app_manifest.txt", "symbol_graph.json", "baseline.json"} {
		if _, err := os.Stat(filepath.Join(relDir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	// private perms: dir 0700, app.dill (customer code) 0600
	di, _ := os.Stat(relDir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("release dir perm = %o, want 0700", di.Mode().Perm())
	}
	fiDill, _ := os.Stat(filepath.Join(relDir, "app.dill"))
	if fiDill.Mode().Perm() != 0o600 {
		t.Fatalf("app.dill perm = %o, want 0600", fiDill.Mode().Perm())
	}
	// baseline.json holds no secret fields
	raw, _ := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	var probe map[string]json.RawMessage
	_ = json.Unmarshal(raw, &probe)
	for _, bad := range []string{"seed", "seed_base64", "token", "operator_token", "manifest_signing_key"} {
		if _, ok := probe[bad]; ok {
			t.Fatalf("baseline.json must not contain secret field %q", bad)
		}
	}
}

func TestFreehandBaseline_IdempotentIdentical(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	d1, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	d2, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatalf("identical re-run must be idempotent: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("idempotent dir mismatch: %s != %s", d1, d2)
	}
}

// Any differing immutable input under the SAME runtime-id must fail closed.
func TestFreehandBaseline_DiffersOnAnyImmutableInput(t *testing.T) {
	mutate := map[string]func(*FreehandBaselineMeta){
		"analyzer":         func(m *FreehandBaselineMeta) { m.AnalyzerVersion = "different" },
		"pkgconfig":        func(m *FreehandBaselineMeta) { m.PackageConfigSHA256 = "different" },
		"frontendRev":      func(m *FreehandBaselineMeta) { m.FrontendRev = "different" },
		"frontendPatchset": func(m *FreehandBaselineMeta) { m.FrontendPatchsetSHA = "different-patchset" },
		"frameworkRev":     func(m *FreehandBaselineMeta) { m.FrameworkRev = "different" },
		"dartRev":          func(m *FreehandBaselineMeta) { m.DartRev = "different" },
		"engineRev":        func(m *FreehandBaselineMeta) { m.EngineRev = "different" },
		"version":          func(m *FreehandBaselineMeta) { m.Version = "9.9.9+9" },
		"channel":          func(m *FreehandBaselineMeta) { m.Channel = "beta" },
		"arch":             func(m *FreehandBaselineMeta) { m.Arch = "x64" },
		"identSchema":      func(m *FreehandBaselineMeta) { m.IdentitySchema = "soroq.freehand.identity.v2" },
	}
	for name, mut := range mutate {
		t.Run(name, func(t *testing.T) {
			proj, dill, srcDill, man, graph := seedFixture(t)
			if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph()); err != nil {
				t.Fatal(err)
			}
			m := fullMeta()
			mut(&m)
			if _, err := persistFreehandBaseline(proj, m, dill, srcDill, man, graph, testDepGraph()); err == nil {
				t.Fatalf("differing %s under same runtime-id must fail closed", name)
			}
		})
	}
	// differing manifest content (hash) also fails
	t.Run("manifest-content", func(t *testing.T) {
		proj, dill, srcDill, man, graph := seedFixture(t)
		if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph()); err != nil {
			t.Fatal(err)
		}
		man2 := writeTmp(t, proj, "manifest2.txt", "pkg::Cls::DIFFERENT\n")
		if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man2, graph, testDepGraph()); err == nil {
			t.Fatal("differing manifest content must fail closed")
		}
	})
}

func TestFreehandBaseline_OverwriteRefusedKeepsOriginal(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph()); err != nil {
		t.Fatal(err)
	}
	dillB := writeTmp(t, proj, "appB.dill", "KERNEL-BYTES-B-DIFFERENT")
	if _, err := persistFreehandBaseline(proj, fullMeta(), dillB, srcDill, man, graph, testDepGraph()); err == nil {
		t.Fatal("differing app.dill must be refused")
	}
	got, _ := os.ReadFile(filepath.Join(freehandReleaseDir(proj, "rt-aaaa"), "app.dill"))
	if string(got) != "KERNEL-BYTES-A" {
		t.Fatalf("original baseline damaged: %q", got)
	}
}

func TestFreehandBaseline_MismatchedKernelRefused(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	m := fullMeta()
	m.AppDillSHA256 = "deadbeef"
	if _, err := persistFreehandBaseline(proj, m, dill, srcDill, man, graph, testDepGraph()); err == nil {
		t.Fatal("mismatched-kernel must be refused")
	}
	if _, err := os.Stat(freehandReleaseDir(proj, "rt-aaaa")); err == nil {
		t.Fatal("no baseline dir on mismatched-kernel")
	}
}

// A corrupt / incomplete / tampered existing baseline must ERROR on reuse, never idempotent-succeed.
func TestFreehandBaseline_CompleteValidationOfExisting(t *testing.T) {
	corrupt := map[string]func(relDir string){
		"corrupt-json":     func(d string) { os.WriteFile(filepath.Join(d, "baseline.json"), []byte("{not json"), 0o600) },
		"missing-appdill":  func(d string) { os.Remove(filepath.Join(d, "app.dill")) },
		"missing-manifest": func(d string) { os.Remove(filepath.Join(d, "soroq_app_manifest.txt")) },
		"tampered-appdill": func(d string) { os.WriteFile(filepath.Join(d, "app.dill"), []byte("TAMPERED"), 0o600) },
		"symlink-appdill": func(d string) {
			os.Remove(filepath.Join(d, "app.dill"))
			os.Symlink("/etc/hosts", filepath.Join(d, "app.dill"))
		},
		"unknown-field": func(d string) {
			raw, _ := os.ReadFile(filepath.Join(d, "baseline.json"))
			var m map[string]any
			json.Unmarshal(raw, &m)
			m["evil_extra"] = "x"
			b, _ := json.Marshal(m)
			os.WriteFile(filepath.Join(d, "baseline.json"), b, 0o600)
		},
	}
	for name, breakIt := range corrupt {
		t.Run(name, func(t *testing.T) {
			proj, dill, srcDill, man, graph := seedFixture(t)
			relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
			if err != nil {
				t.Fatal(err)
			}
			breakIt(relDir)
			if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph()); err == nil {
				t.Fatalf("%s: corrupt existing baseline must ERROR, not idempotent-succeed", name)
			}
		})
	}
}

func TestFreehandBaseline_RuntimeIDPathSafety(t *testing.T) {
	bad := []string{"", "..", "../evil", "a/b", `a\b`, "/abs", ".hidden", "a/../../../etc", "a b", string(make([]byte, 200))}
	for _, id := range bad {
		if err := validateRuntimeID(id); err == nil {
			t.Fatalf("runtime-id %q must be rejected", id)
		}
	}
	for _, id := range []string{"rt-aaaa", "6e665fb0e10b", "v1.0.0_1"} {
		if err := validateRuntimeID(id); err != nil {
			t.Fatalf("safe runtime-id %q rejected: %v", id, err)
		}
	}
	// a traversal id must never let persist escape .soroq/releases
	proj, dill, srcDill, man, graph := seedFixture(t)
	m := fullMeta()
	m.RuntimeID = "../../escape"
	if _, err := persistFreehandBaseline(proj, m, dill, srcDill, man, graph, testDepGraph()); err == nil {
		t.Fatal("traversal runtime-id must be refused by persist")
	}
	if _, err := os.Stat(filepath.Join(proj, "escape")); err == nil {
		t.Fatal("persist escaped .soroq/releases via traversal")
	}
}

// Genuine fault injection after each write stage: no visible final baseline, temp cleaned,
// and any pre-existing valid baseline stays byte-unchanged.
func TestFreehandBaseline_FaultInjectionAtomicity(t *testing.T) {
	stages := []string{"after-appdill", "after-source-appdill", "after-manifest", "after-graph", "before-rename"}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			proj, dill, srcDill, man, graph := seedFixture(t)
			// pre-existing DIFFERENT-runtime baseline that must remain untouched
			pre := fullMeta()
			pre.RuntimeID = "rt-preexisting"
			if _, err := persistFreehandBaseline(proj, pre, dill, srcDill, man, graph, testDepGraph()); err != nil {
				t.Fatal(err)
			}
			preDill, _ := os.ReadFile(filepath.Join(freehandReleaseDir(proj, "rt-preexisting"), "app.dill"))

			freehandFaultInjection = func(s string) error {
				if s == stage {
					return os.ErrClosed
				}
				return nil
			}
			defer func() { freehandFaultInjection = nil }()

			if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph()); err == nil {
				t.Fatalf("fault at %s must fail", stage)
			}
			// no visible final baseline for the faulted runtime-id
			if _, err := os.Stat(freehandReleaseDir(proj, "rt-aaaa")); err == nil {
				t.Fatalf("fault at %s left a visible final baseline", stage)
			}
			// no leftover temp dirs
			entries, _ := os.ReadDir(filepath.Join(proj, ".soroq", "releases"))
			for _, e := range entries {
				if len(e.Name()) > 4 && e.Name()[:4] == ".tmp" {
					t.Fatalf("fault at %s leaked temp dir %s", stage, e.Name())
				}
			}
			// pre-existing baseline unchanged
			postDill, _ := os.ReadFile(filepath.Join(freehandReleaseDir(proj, "rt-preexisting"), "app.dill"))
			if string(preDill) != string(postDill) {
				t.Fatalf("fault at %s damaged an unrelated baseline", stage)
			}
		})
	}
}

func TestFreehandBaseline_ConcurrentWritersSameContent(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	dirs := map[string]int{}
	errs := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
			mu.Lock()
			if err != nil {
				errs++
			} else {
				dirs[d]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if errs != 0 {
		t.Fatalf("identical concurrent writers must all succeed, got %d errors", errs)
	}
	if len(dirs) != 1 {
		t.Fatalf("identical concurrent writers must converge on one dir, got %v", dirs)
	}
}

func TestFreehandBaseline_ConcurrentWritersDifferentContent(t *testing.T) {
	proj := t.TempDir()
	man := writeTmp(t, proj, "manifest.txt", "m\n")
	graph := writeTmp(t, proj, "graph.json", "{}")
	srcDill := writeTmp(t, proj, "source_app.dill", "SOURCE-KERNEL-SHARED")
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, collide := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// each writer has a DIFFERENT app.dill but the SAME runtime-id
			d := writeTmp(t, proj, "app.dill."+string(rune('a'+i)), "KERNEL-VARIANT-"+string(rune('a'+i)))
			_, err := persistFreehandBaseline(proj, fullMeta(), d, srcDill, man, graph, testDepGraph())
			mu.Lock()
			if err == nil {
				ok++
			} else {
				collide++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("exactly one differing concurrent writer may win, got %d wins", ok)
	}
	if collide != n-1 {
		t.Fatalf("other differing writers must collide-error, got %d", collide)
	}
}

func TestFreehandBaseline_PatchableCountDerivedAndVerified(t *testing.T) {
	proj := t.TempDir()
	dill := writeTmp(t, proj, "app.dill", "KERNEL-A")
	srcDill := writeTmp(t, proj, "source_app.dill", "SOURCE-KERNEL-A")
	man := writeTmp(t, proj, "manifest.txt", "a::b::c\nd::e::f\ng::h::i\n") // 3 entries
	graph := writeTmp(t, proj, "graph.json", "{}")
	m := fullMeta()
	m.PatchableCount = 0 // caller supplies zero -> MUST be derived from the manifest (3)
	relDir, err := persistFreehandBaseline(proj, m, dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	var got FreehandBaselineMeta
	b, _ := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.PatchableCount != 3 {
		t.Fatalf("derived patchable_symbols=%d, want 3", got.PatchableCount)
	}
	// tamper the recorded count -> reuse/verify must fail closed
	got.PatchableCount = 99
	nb, _ := json.MarshalIndent(got, "", "  ")
	os.WriteFile(filepath.Join(relDir, "baseline.json"), nb, 0o600)
	if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph()); err == nil {
		t.Fatal("tampered patchable_symbols count must be refused")
	}
}

func TestFreehandBaseline_EmptyManifestRefused(t *testing.T) {
	proj := t.TempDir()
	dill := writeTmp(t, proj, "app.dill", "K")
	srcDill := writeTmp(t, proj, "source_app.dill", "SOURCE-K")
	man := writeTmp(t, proj, "manifest.txt", "\n   \n") // 0 entries
	graph := writeTmp(t, proj, "graph.json", "{}")
	if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph()); err == nil {
		t.Fatal("zero patchable symbols must be refused (provenance requires >0)")
	}
}

func TestFreehandBaseline_ProvenanceRequiredOnWrite(t *testing.T) {
	fields := map[string]func(*FreehandBaselineMeta){
		"frontend":         func(m *FreehandBaselineMeta) { m.FrontendRev = "" },
		"frontendPatchset": func(m *FreehandBaselineMeta) { m.FrontendPatchsetSHA = "" },
		"framework":        func(m *FreehandBaselineMeta) { m.FrameworkRev = "" },
		"dart":             func(m *FreehandBaselineMeta) { m.DartRev = "" },
		"engine":           func(m *FreehandBaselineMeta) { m.EngineRev = "" },
		"pkgconfig":        func(m *FreehandBaselineMeta) { m.PackageConfigSHA256 = "" },
		"analyzer":         func(m *FreehandBaselineMeta) { m.AnalyzerVersion = "" },
		"channel":          func(m *FreehandBaselineMeta) { m.Channel = "" },
		"version":          func(m *FreehandBaselineMeta) { m.Version = "" },
	}
	for name, mut := range fields {
		t.Run(name, func(t *testing.T) {
			proj := t.TempDir()
			dill := writeTmp(t, proj, "app.dill", "K")
			srcDill := writeTmp(t, proj, "source_app.dill", "SOURCE-K")
			man := writeTmp(t, proj, "manifest.txt", "x::y::z\n")
			graph := writeTmp(t, proj, "graph.json", "{}")
			m := fullMeta()
			mut(&m)
			if _, err := persistFreehandBaseline(proj, m, dill, srcDill, man, graph, testDepGraph()); err == nil {
				t.Fatalf("missing %s provenance must be refused", name)
			}
		})
	}
}

func TestFreehandBaseline_TamperedProvenanceRefusedOnReuse(t *testing.T) {
	proj := t.TempDir()
	dill := writeTmp(t, proj, "app.dill", "K")
	srcDill := writeTmp(t, proj, "source_app.dill", "SOURCE-K")
	man := writeTmp(t, proj, "manifest.txt", "x::y::z\n")
	graph := writeTmp(t, proj, "graph.json", "{}")
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	// empty a provenance field in the persisted baseline -> reuse must fail closed
	var got map[string]any
	b, _ := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	json.Unmarshal(b, &got)
	got["framework_revision"] = ""
	nb, _ := json.Marshal(got)
	os.WriteFile(filepath.Join(relDir, "baseline.json"), nb, 0o600)
	if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph()); err == nil {
		t.Fatal("emptied provenance in an existing baseline must fail closed")
	}
}

// v2 dual-kernel: source_app.dill is persisted, hash-verified, and part of the immutable input set.
func TestFreehandBaseline_DualKernelPersistedAndImmutable(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	// both kernels persisted, distinct bytes
	aot, _ := os.ReadFile(filepath.Join(relDir, "app.dill"))
	src, _ := os.ReadFile(filepath.Join(relDir, "source_app.dill"))
	if string(aot) != "KERNEL-BYTES-A" || string(src) != "SOURCE-KERNEL-BYTES-A" {
		t.Fatalf("dual kernels not persisted verbatim: aot=%q src=%q", aot, src)
	}
	// baseline records v2 schema + source fields + recipe digest
	var m FreehandBaselineMeta
	b, _ := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Schema != freehandBaselineSchemaV2 {
		t.Fatalf("schema = %q, want %q", m.Schema, freehandBaselineSchemaV2)
	}
	if m.SourceAppDillSHA256 == "" || m.SourceRecipeDigest == "" || m.SourceKernelRecipe == nil {
		t.Fatal("v2 baseline missing source kernel provenance")
	}
	// a DIFFERENT source kernel under the same runtime-id must fail closed (immutable input)
	srcDillB := writeTmp(t, proj, "source_appB.dill", "SOURCE-KERNEL-DIFFERENT")
	if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDillB, man, graph, testDepGraph()); err == nil {
		t.Fatal("differing source_app.dill under same runtime-id must be refused")
	}
}

// A v1 (pre-dual-kernel) baseline must be refused on reuse with a clear "create a new base release".
func TestFreehandBaseline_V1WithoutSourceKernelRefused(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	// rewrite the on-disk baseline to look like a legacy v1 (schema v1, no source fields) + drop source_app.dill
	var m map[string]any
	b, _ := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	json.Unmarshal(b, &m)
	m["schema"] = "soroq.freehand.baseline.v1"
	delete(m, "source_app_dill_sha256")
	delete(m, "source_kernel_recipe")
	delete(m, "source_kernel_recipe_digest")
	nb, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(relDir, "baseline.json"), nb, 0o600)
	os.Remove(filepath.Join(relDir, "source_app.dill"))
	_, err = verifyExistingBaseline(relDir)
	if err == nil {
		t.Fatal("v1 baseline without source kernel must be refused")
	}
	if !strings.Contains(err.Error(), "create a new base release") {
		t.Fatalf("v1 refusal must direct to a new base release, got: %v", err)
	}
}

// A tampered source_app.dill (bytes changed after persistence) must fail existing-baseline verification.
func TestFreehandBaseline_TamperedSourceKernelRefused(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(relDir, "source_app.dill"), []byte("TAMPERED-SOURCE"), 0o600)
	if _, err := verifyExistingBaseline(relDir); err == nil {
		t.Fatal("tampered source_app.dill must be refused")
	}
}

// A tampered source_kernel_recipe (recipe field changed without updating the digest) must be refused.
func TestFreehandBaseline_TamperedRecipeRefused(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	b, _ := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	json.Unmarshal(b, &m)
	// mutate the recipe entrypoint but leave the recorded digest -> digest recompute mismatch
	if r, ok := m["source_kernel_recipe"].(map[string]any); ok {
		r["entrypoint"] = "lib/evil.dart"
	}
	nb, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(relDir, "baseline.json"), nb, 0o600)
	if _, err := verifyExistingBaseline(relDir); err == nil {
		t.Fatal("tampered source_kernel_recipe (digest mismatch) must be refused")
	}
}

// ---- Immutable base runtime dependency graph (dependency-OTA anchor) ----

func TestFreehandBaseline_PersistsAndBindsBaseDependencyGraph(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	want := testDepGraph()
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, want)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(relDir, dependencyGraphFile))
	if err != nil {
		t.Fatalf("the base dependency graph must be persisted beside the baseline: %v", err)
	}
	// It must contain no developer-local absolute path.
	for _, bad := range []string{"/Users/", "/home/", proj} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("persisted base dependency graph leaks a developer-local path %q", bad)
		}
	}
	m, err := verifyExistingBaseline(relDir)
	if err != nil {
		t.Fatalf("baseline with a dependency graph must verify: %v", err)
	}
	if m.DependencyGraphDigest != want.GraphDigest {
		t.Fatalf("baseline must record the graph's own digest: %s != %s", m.DependencyGraphDigest, want.GraphDigest)
	}
	if m.DependencyLockSHA256 != want.PubspecLockSHA || m.DependencyPackageConfigSHA256 != want.PackageConfigSHA {
		t.Fatal("baseline must record the graph's resolution inputs")
	}
	got, err := baseDependencyGraph(relDir, m)
	if err != nil {
		t.Fatal(err)
	}
	if got.GraphDigest != want.GraphDigest || len(got.Packages) != len(want.Packages) {
		t.Fatalf("reloaded base graph differs: %+v", got)
	}
}

func TestFreehandBaseline_RefusesInvalidBaseDependencyGraph(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	bad := testDepGraph()
	bad.GraphDigest = strings.Repeat("0", 64) // digest does not match content
	if _, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, bad); err == nil {
		t.Fatal("a baseline must never be written with an invalid base dependency graph")
	}
}

func TestFreehandBaseline_TamperedDependencyGraphFileRefused(t *testing.T) {
	for _, tc := range []struct{ name, mutate string }{
		{"content edited (sha + digest both break)", `{"schema":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proj, dill, srcDill, man, graph := seedFixture(t)
			relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(relDir, dependencyGraphFile)
			_ = os.Chmod(p, 0o600)
			if err := os.WriteFile(p, []byte(tc.mutate), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyExistingBaseline(relDir); err == nil {
				t.Fatal("a tampered dependency_graph.json must fail baseline verification")
			}
		})
	}
}

// A fully REBOUND tamper: rewrite the graph file AND fix up baseline.json's recorded sha/digest/inputs so
// every hash is internally consistent. It must still be refused, because the graph's own strict validation
// (here: a dangling runtime edge) is semantic, not a hash comparison.
func TestFreehandBaseline_ReboundDependencyGraphTamperStillRefused(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	forged := testDepGraph()
	alpha := forged.Packages["alpha"]
	alpha.Dependencies = []string{"never_resolved"} // semantic break
	forged.Packages["alpha"] = alpha
	forged.GraphDigest = forged.RecomputeDigest() // rebind the graph's own digest
	forgedBytes, err := json.MarshalIndent(forged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(relDir, dependencyGraphFile), 0o600)
	if err := os.WriteFile(filepath.Join(relDir, dependencyGraphFile), forgedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// Rebind baseline.json so the outer sha/digest agree with the forged file.
	blPath := filepath.Join(relDir, "baseline.json")
	blRaw, err := os.ReadFile(blPath)
	if err != nil {
		t.Fatal(err)
	}
	var m FreehandBaselineMeta
	if err := json.Unmarshal(blRaw, &m); err != nil {
		t.Fatal(err)
	}
	m.DependencyGraphSHA256 = freehandSHA256Bytes(forgedBytes)
	m.DependencyGraphDigest = forged.GraphDigest
	out, _ := json.MarshalIndent(m, "", "  ")
	_ = os.Chmod(blPath, 0o600)
	if err := os.WriteFile(blPath, out, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = verifyExistingBaseline(relDir)
	if err == nil || !strings.Contains(err.Error(), "dangling") {
		t.Fatalf("a fully rebound dependency-graph tamper must still be refused on semantic grounds, got %v", err)
	}
}

func TestFreehandBaseline_PreDependencyOTABaselineGetsActionableMessage(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a baseline written before dependency-OTA support: strip every dependency_* field.
	blPath := filepath.Join(relDir, "baseline.json")
	raw, err := os.ReadFile(blPath)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	for k := range probe {
		if strings.HasPrefix(k, "dependency_") {
			delete(probe, k)
		}
	}
	out, _ := json.MarshalIndent(probe, "", "  ")
	_ = os.Chmod(blPath, 0o600)
	if err := os.WriteFile(blPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(relDir, dependencyGraphFile))

	_, err = verifyExistingBaseline(relDir)
	if err == nil {
		t.Fatal("a pre-dependency-OTA baseline must be refused")
	}
	if !strings.Contains(err.Error(), "predates dependency-OTA support") || !strings.Contains(err.Error(), "soroq release ios --engine --build") {
		t.Fatalf("the refusal must be actionable (name the fix), got: %v", err)
	}
}
