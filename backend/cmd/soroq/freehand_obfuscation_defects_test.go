package main

// REPRODUCTIONS. Each test here states a defect found at 009b986f and fails while it is present.
// After the fix they are the load-bearing controls: each one begins by proving the sound
// configuration is accepted, then breaks exactly one thing.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func obfMeta(t *testing.T, digest string, entries int) FreehandBaselineMeta {
	t.Helper()
	m := fullMeta()
	m.Obfuscation = controlBinding(t, digest, entries)
	return m
}

// DEFECT 1 — the map is adopted AFTER the baseline is published, so a crash in between leaves
// baseline.json declaring a map that is absent, and nothing rejects it afterwards.
func TestDefect1_BaselineDeclaringAnAbsentMapIsRejected(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	mapRaw := controlMapBytes(t, controlBaseMapFlat)
	src := filepath.Join(proj, "produced_map.json")
	if err := os.WriteFile(src, mapRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := obfMeta(t, freehandSHA256Bytes(mapRaw), len(controlBaseMapFlat)/2)

	// SOUND: the map travels with the baseline, inside the same transaction.
	relDir, err := persistFreehandBaseline(proj, meta, dill, srcDill, man, graph, testDepGraph(), src)
	if err != nil {
		t.Fatalf("an obfuscated baseline must persist with its map: %v", err)
	}
	captured := filepath.Join(relDir, freehandBaseObfuscationMapFile)
	if _, err := os.Stat(captured); err != nil {
		t.Fatalf("the published baseline must contain its map: %v", err)
	}
	if _, err := verifyExistingBaseline(relDir); err != nil {
		t.Fatalf("a complete obfuscated baseline must verify: %v", err)
	}

	// CONTROL: remove the map. A baseline that DECLARES one and does not have it must be rejected.
	if err := os.Remove(captured); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyExistingBaseline(relDir); err == nil {
		t.Fatal("control did not fire: a baseline declaring an absent obfuscation map was accepted")
	}
}

// DEFECT 1b — a fault between the two operations must leave NO final baseline at all.
func TestDefect1_FaultBeforeRenameLeavesNoBaseline(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	mapRaw := controlMapBytes(t, controlBaseMapFlat)
	src := filepath.Join(proj, "produced_map.json")
	if err := os.WriteFile(src, mapRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := obfMeta(t, freehandSHA256Bytes(mapRaw), len(controlBaseMapFlat)/2)

	prev := freehandFaultInjection
	t.Cleanup(func() { freehandFaultInjection = prev })
	// The map must be written INSIDE the transaction, so a fault after it and before the rename
	// leaves nothing published.
	freehandFaultInjection = func(stage string) error {
		if stage == "before-rename" {
			return os.ErrClosed
		}
		return nil
	}
	if _, err := persistFreehandBaseline(proj, meta, dill, srcDill, man, graph, testDepGraph(), src); err == nil {
		t.Fatal("the injected fault must fail the persist")
	}
	relDir := filepath.Join(proj, ".soroq", "releases", meta.RuntimeID)
	if _, err := os.Stat(relDir); err == nil {
		t.Fatal("control did not fire: a fault before the rename left a published baseline")
	}
	// And no temporary directory survives.
	entries, err := os.ReadDir(filepath.Join(proj, ".soroq", "releases"))
	if err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				t.Fatalf("a scratch directory survived the fault: %s", e.Name())
			}
		}
	}
}

// DEFECT 1c — the map must be verified AT REST by the same verifier that verifies everything else.
func TestDefect1_MapTamperingAtRestIsRejected(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	mapRaw := controlMapBytes(t, controlBaseMapFlat)
	src := filepath.Join(proj, "produced_map.json")
	if err := os.WriteFile(src, mapRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := obfMeta(t, freehandSHA256Bytes(mapRaw), len(controlBaseMapFlat)/2)
	relDir, err := persistFreehandBaseline(proj, meta, dill, srcDill, man, graph, testDepGraph(), src)
	if err != nil {
		t.Fatal(err)
	}
	captured := filepath.Join(relDir, freehandBaseObfuscationMapFile)

	restore := func() {
		os.Remove(captured)
		if err := os.WriteFile(captured, mapRaw, freehandBaseObfuscationMapMode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(captured, freehandBaseObfuscationMapMode)
		if _, err := verifyExistingBaseline(relDir); err != nil {
			t.Fatalf("the restored baseline must verify: %v", err)
		}
	}
	restore()

	t.Run("altered_bytes", func(t *testing.T) {
		defer restore()
		if err := os.WriteFile(captured, controlMapBytes(t, []any{"x", "y"}), freehandBaseObfuscationMapMode); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyExistingBaseline(relDir); err == nil {
			t.Fatal("control did not fire: an altered captured map was accepted")
		}
	})

	t.Run("wrong_mode", func(t *testing.T) {
		defer restore()
		if err := os.Chmod(captured, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyExistingBaseline(relDir); err == nil {
			t.Fatal("control did not fire: a world-readable captured map was accepted")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		defer restore()
		elsewhere := filepath.Join(t.TempDir(), "map.json")
		if err := os.WriteFile(elsewhere, mapRaw, 0o600); err != nil {
			t.Fatal(err)
		}
		os.Remove(captured)
		if err := os.Symlink(elsewhere, captured); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := verifyExistingBaseline(relDir); err == nil {
			t.Fatal("control did not fire: a symlinked captured map was accepted")
		}
	})
}

// DEFECT 1d — a map with NO binding is a stray file inside an immutable directory.
func TestDefect1_StrayMapWithoutBindingIsRejected(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph(), "")
	if err != nil {
		t.Fatal(err)
	}
	// SOUND: an ordinary baseline with no binding and no map.
	if _, err := verifyExistingBaseline(relDir); err != nil {
		t.Fatalf("an ordinary baseline must verify: %v", err)
	}
	// CONTROL.
	stray := filepath.Join(relDir, freehandBaseObfuscationMapFile)
	if err := os.WriteFile(stray, controlMapBytes(t, controlBaseMapFlat), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyExistingBaseline(relDir); err == nil {
		t.Fatal("control did not fire: a stray obfuscation map with no binding was accepted")
	}
}

// DEFECT 1e — immutableInputsEqual ignored the binding, so two bases differing only in their
// obfuscation map compared equal and the second reused the first idempotently.
func TestDefect1_ImmutableInputsIncludeTheObfuscationBinding(t *testing.T) {
	a := obfMeta(t, strings.Repeat("a", 64), 7)
	b := obfMeta(t, strings.Repeat("a", 64), 7)
	if !immutableInputsEqual(&a, &b) {
		t.Fatal("identical bindings must compare equal")
	}
	c := obfMeta(t, strings.Repeat("b", 64), 7)
	if immutableInputsEqual(&a, &c) {
		t.Fatal("control did not fire: two different base maps compared equal")
	}
	d := fullMeta()
	if immutableInputsEqual(&a, &d) {
		t.Fatal("control did not fire: an obfuscated base compared equal to a non-obfuscated one")
	}
}

// DEFECT 2 — one fixed scratch path, shared by every concurrent release.
func TestDefect2_ScratchMapPathIsPerInvocation(t *testing.T) {
	proj := t.TempDir()
	a, cleanupA, err := freehandProducedObfuscationMapPath(proj)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanupA() }()
	b, cleanupB, err := freehandProducedObfuscationMapPath(proj)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanupB() }()
	if a == b {
		t.Fatal("control did not fire: two invocations were handed the same scratch map path")
	}
	// Each cleanup removes only its OWN directory.
	if err := os.WriteFile(a, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupA(); err != nil {
		t.Fatalf("cleanup refused a clean allocation: %v", err)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatalf("one invocation's cleanup removed another's scratch: %v", err)
	}
	if _, err := os.Stat(a); err == nil {
		t.Fatal("cleanup did not remove its own scratch")
	}
}

// DEFECT 5 — the experiment override let an obfuscated release proceed WITHOUT authorization, after
// which the release recorded no binding and persisted an actually-obfuscated base as an ordinary one.
func TestDefect5_OverrideCannotProduceAReusableBaseline(t *testing.T) {
	obfArgs := []string{"--obfuscate", "--split-debug-info=build/symbols"}
	// SOUND: an ordinary build is unaffected by the override.
	t.Setenv(unverifiedBuildFlagsOptInEnv, "1")
	if err := guardUnverifiedBuildFlags([]string{"--dart-define=A=b"}, nil); err != nil {
		t.Fatalf("an ordinary build must not be blocked: %v", err)
	}
	// CONTROL: the override must NOT let an unauthorized obfuscated release through.
	err := guardUnverifiedBuildFlags(obfArgs, nil)
	if err == nil {
		t.Fatal("control did not fire: the experiment override allowed an unauthorized obfuscated release")
	}
	if !strings.Contains(err.Error(), unverifiedBuildFlagsOptInEnv) {
		t.Fatalf("the refusal should explain the override no longer applies here: %v", err)
	}
}

// DEFECT 6 — a legitimate one-entry patch on a PROTECTED name (which the map records as name->name)
// was rejected by an inequality heuristic.
func TestDefect6_IdentityMappingsAreLegitimate(t *testing.T) {
	// `noSuchMethod` is protected: PreventRenaming writes it as name -> name.
	entries := []freehandReplacementEntry{{
		BaseIdentity: "package:app/main.dart::Counter::noSuchMethod", StableIdentity: "v1|k",
		ModuleLibrary: "soroq-freehand:///m", ModuleClass: "Counter", ModuleMember: "noSuchMethod",
		Kind: "instance-member",
	}}
	receipt := &FreehandTranslationReceipt{
		Format: soroqTranslationReceiptFormat, FormatVersion: soroqTranslationReceiptFormatVersion,
		ObfuscationEnabled: true, BaseMapSHA256: strings.Repeat("d", 64), BaseMapEntries: 2,
		IdentityCount: 2,
		Identities: []FreehandTranslationIdentity{
			{Context: freehandTranslationContextClass, Original: "Counter", Runtime: "Counter", Origin: freehandTranslationOriginBase},
			{Context: freehandTranslationContextMember, EnclosingOriginal: "Counter", EnclosingRuntime: "Counter",
				Original: "noSuchMethod", Runtime: "noSuchMethod", Origin: freehandTranslationOriginBase},
		},
	}
	translated, err := translateReplacementABI(entries, receipt, "")
	if err != nil {
		t.Fatalf("a protected-name patch must translate: %v", err)
	}
	if translated[0].RuntimeBaseIdentity != entries[0].BaseIdentity {
		t.Fatalf("a protected name must translate to itself, got %q", translated[0].RuntimeBaseIdentity)
	}
	// SOUND: the manifest must ACCEPT it. Proof of translation comes from the verified receipt, not
	// from the runtime identity happening to differ from the source one.
	device := []freehandDeviceABIEntry{{
		BaseIdentity: entries[0].BaseIdentity, StableIdentity: "v1|k",
		ModuleLibrary: entries[0].ModuleLibrary, ModuleClass: "Counter", ModuleMember: "noSuchMethod",
		Kind:                "instance-member",
		RuntimeBaseIdentity: translated[0].RuntimeBaseIdentity,
		RuntimeModuleClass:  translated[0].RuntimeModuleClass,
		RuntimeModuleMember: translated[0].RuntimeModuleMember,
	}}
	if err := requireTranslatedABICoverage(device, translated); err != nil {
		t.Fatalf("control fired wrongly: a valid identity-mapping patch was refused: %v", err)
	}

	// CONTROL: translation genuinely skipped -- the ABI carries identities the receipt never produced.
	skipped := []freehandDeviceABIEntry{{
		BaseIdentity: "package:app/main.dart::Counter::bump", StableIdentity: "v1|other",
		ModuleLibrary: entries[0].ModuleLibrary, ModuleClass: "Counter", ModuleMember: "bump",
		Kind:                "instance-member",
		RuntimeBaseIdentity: "package:app/main.dart::Counter::bump",
		RuntimeModuleClass:  "Counter",
		RuntimeModuleMember: "bump",
	}}
	if err := requireTranslatedABICoverage(skipped, translated); err == nil {
		t.Fatal("control did not fire: an ABI the receipt does not cover was accepted")
	}
}

// DEFECT 4 — the controller never read the obfuscation binding.
func TestDefect4_ControllerReadsTheBaseMapBinding(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "packages", "soroq_flutter", "lib", "src", "engine_lane_ota.dart"))
	if err != nil {
		t.Skipf("controller source not readable from here: %v", err)
	}
	for _, want := range []string{"baseObfuscationMapSha256", "obfuscationBindingDigest"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("the controller must read %q from the signed manifest", want)
		}
	}
}

// DEFECT 3 — the artifact carried only a digest, so verification trusted metadata it could not
// re-derive.
func TestDefect3_ArtifactCarriesTheReceiptItself(t *testing.T) {
	found := false
	for _, f := range artifactFiles {
		if f == freehandTranslationReceiptFile {
			found = true
		}
	}
	if found {
		t.Fatal("the receipt must be conditional on obfuscation, not in the unconditional artifact set")
	}
	if !listHas(obfuscatedArtifactFiles(), freehandTranslationReceiptFile) {
		t.Fatal("control did not fire: an obfuscated artifact does not require the receipt file")
	}
	// A JSON document with trailing data must be refused by the strict decoder.
	raw := append(controlMapBytes(t, []any{}), []byte("{}")...)
	var v any
	if err := decodeStrictJSONWithEOF(raw, &v); err == nil {
		t.Fatal("control did not fire: trailing data after a JSON document was accepted")
	}
}

func listHas(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

var _ = json.Marshal

// CONCURRENCY. Two identical writers may publish one fully verified baseline between them, and two
// concurrent scratch allocations may never see each other's files.
func TestDefect1_ConcurrentIdenticalWritersPublishOneVerifiedBaseline(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	mapRaw := controlMapBytes(t, controlBaseMapFlat)
	meta := obfMeta(t, freehandSHA256Bytes(mapRaw), len(controlBaseMapFlat)/2)

	const writers = 8
	type outcome struct {
		dir string
		err error
	}
	results := make(chan outcome, writers)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < writers; i++ {
		go func(i int) {
			// Each writer has its OWN scratch copy of the map, exactly as two concurrent releases would.
			src := filepath.Join(t.TempDir(), "produced.json")
			if err := os.WriteFile(src, mapRaw, 0o600); err != nil {
				results <- outcome{err: err}
				return
			}
			start.Wait()
			dir, err := persistFreehandBaseline(proj, meta, dill, srcDill, man, graph, testDepGraph(), src)
			results <- outcome{dir: dir, err: err}
		}(i)
	}
	start.Done()

	published := map[string]int{}
	for i := 0; i < writers; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("an identical concurrent writer must not fail: %v", r.err)
		}
		published[r.dir]++
	}
	if len(published) != 1 {
		t.Fatalf("concurrent identical writers published %d distinct baselines: %v", len(published), published)
	}
	// The one published result must be COMPLETE: baseline, map, and a verifier that accepts both.
	relDir := filepath.Join(proj, ".soroq", "releases", meta.RuntimeID)
	if _, err := verifyExistingBaseline(relDir); err != nil {
		t.Fatalf("the surviving baseline must verify: %v", err)
	}
	// And no scratch directory is left behind under the releases root.
	entries, err := os.ReadDir(filepath.Join(proj, ".soroq", "releases"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("a scratch directory survived: %s", e.Name())
		}
	}
}

func TestDefect2_ConcurrentScratchAllocationsDoNotCrossAdopt(t *testing.T) {
	proj := t.TempDir()
	const n = 16
	type alloc struct {
		path    string
		cleanup func() error
	}
	allocs := make(chan alloc, n)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		go func(i int) {
			start.Wait()
			// Half allocate a release map, half a patch receipt: both share the scratch root.
			var p string
			var c func() error
			var err error
			if i%2 == 0 {
				p, c, err = freehandProducedObfuscationMapPath(proj)
			} else {
				p, c, err = freehandPatchReceiptScratch(proj)
			}
			if err != nil {
				allocs <- alloc{}
				return
			}
			// Each writes a distinguishable body; cross-adoption would make two agree.
			_ = os.WriteFile(p, []byte(strings.Repeat("x", i+1)), 0o600)
			allocs <- alloc{path: p, cleanup: c}
		}(i)
	}
	start.Done()

	seen := map[string]bool{}
	bodies := map[string]bool{}
	got := make([]alloc, 0, n)
	for i := 0; i < n; i++ {
		a := <-allocs
		if a.path == "" {
			t.Fatal("a concurrent allocation failed")
		}
		if seen[a.path] {
			t.Fatalf("two concurrent allocations were handed the same path: %s", a.path)
		}
		seen[a.path] = true
		body, err := os.ReadFile(a.path)
		if err != nil {
			t.Fatalf("an allocation's own file vanished: %v", err)
		}
		if bodies[string(body)] {
			t.Fatal("control did not fire: two allocations hold identical content, so one adopted the other's")
		}
		bodies[string(body)] = true
		got = append(got, a)
	}
	// Cleaning up one must leave every other intact.
	if err := got[0].cleanup(); err != nil {
		t.Fatalf("a clean allocation must clean up without complaint: %v", err)
	}
	for _, a := range got[1:] {
		if _, err := os.Stat(a.path); err != nil {
			t.Fatalf("one cleanup removed another allocation's scratch: %v", err)
		}
	}
	for _, a := range got[1:] {
		if err := a.cleanup(); err != nil {
			t.Fatalf("cleanup refused a clean allocation: %v", err)
		}
	}
}

// CONTROL: cleanup refuses a path outside the fixed scratch root, however it is spelled.
func TestDefect2_CleanupRefusesAnythingOutsideTheScratchRoot(t *testing.T) {
	proj := t.TempDir()
	root := freehandScratchRoot(proj)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// BASELINE: a genuine allocation is removed.
	p, cleanup, err := freehandProducedObfuscationMapPath(proj)
	if err != nil {
		t.Fatal(err)
	}
	own := filepath.Dir(p)
	if err := os.WriteFile(p, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("a clean allocation must clean up: %v", err)
	}
	if _, err := os.Stat(own); err == nil {
		t.Fatal("the baseline cleanup did not remove its own directory")
	}

	// CONTROLS: every one of these must be left alone.
	victim := filepath.Join(proj, "precious")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"the root itself":       root,
		"a sibling of the root": victim,
		"a dot-dot escape":      filepath.Join(root, "..", "precious"),
		"a nested grandchild":   filepath.Join(root, "map-x", "deeper"),
		"an unprefixed child":   filepath.Join(root, "not-ours"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
		if err := freehandScratchCleanup(root, dir, freehandBaseObfuscationMapFile)(); err == nil {
			t.Fatalf("control did not fire: cleanup accepted %s (%s)", name, dir)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("control did not fire: cleanup removed %s (%s)", name, dir)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("control did not fire: a sibling directory was removed: %v", err)
	}
}
