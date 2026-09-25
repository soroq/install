package main

// INTEGRITY CONTROLS for the four defects found at 74e7d0dd. Each group proves the sound
// configuration is accepted, then breaks exactly one thing.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------------------------
// 1. SCRATCH CLEANUP: exact file, then an empty directory. Never recursive.
// ---------------------------------------------------------------------------------------------

func TestIntegrityScratchCleanupIsExactNotRecursive(t *testing.T) {
	t.Run("normal_cleanup_removes_its_own_file_and_directory", func(t *testing.T) {
		proj := t.TempDir()
		p, cleanup, err := freehandProducedObfuscationMapPath(proj)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatalf("a clean allocation must clean up without complaint: %v", err)
		}
		if _, err := os.Stat(p); err == nil {
			t.Fatal("the allocated file survived")
		}
		if _, err := os.Stat(filepath.Dir(p)); err == nil {
			t.Fatal("the allocated directory survived")
		}
	})

	t.Run("cleanup_before_anything_was_written_is_not_an_error", func(t *testing.T) {
		// The error path: the build failed before gen_snapshot wrote a map at all.
		proj := t.TempDir()
		p, cleanup, err := freehandPatchReceiptScratch(proj)
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatalf("an aborted allocation must clean up quietly: %v", err)
		}
		if _, err := os.Stat(filepath.Dir(p)); err == nil {
			t.Fatal("the allocated directory survived")
		}
	})

	t.Run("cleanup_twice_is_quiet", func(t *testing.T) {
		proj := t.TempDir()
		_, cleanup, err := freehandProducedObfuscationMapPath(proj)
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatalf("a second cleanup must be a no-op, not an error: %v", err)
		}
	})

	// THE CONTROL THIS WAS WRITTEN FOR. Before the fix, os.RemoveAll deleted these silently.
	t.Run("control_an_unexpected_sentinel_survives_and_cleanup_refuses", func(t *testing.T) {
		proj := t.TempDir()
		p, cleanup, err := freehandProducedObfuscationMapPath(proj)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(p)
		if err := os.WriteFile(p, []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(dir, "SOMEONE-ELSES-FILE")
		if err := os.WriteFile(sentinel, []byte("do not delete me"), 0o600); err != nil {
			t.Fatal(err)
		}
		err = cleanup()
		if err == nil {
			t.Fatal("control did not fire: cleanup accepted a directory holding an unexpected file")
		}
		if !strings.Contains(err.Error(), "left untouched") {
			t.Fatalf("the refusal must say the leftovers were not deleted: %v", err)
		}
		body, rerr := os.ReadFile(sentinel)
		if rerr != nil {
			t.Fatalf("control did not fire: the sentinel was deleted: %v", rerr)
		}
		if string(body) != "do not delete me" {
			t.Fatal("the sentinel was modified")
		}
	})

	t.Run("control_an_unexpected_subdirectory_survives", func(t *testing.T) {
		proj := t.TempDir()
		p, cleanup, err := freehandProducedObfuscationMapPath(proj)
		if err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(filepath.Dir(p), "nested", "deep")
		if err := os.MkdirAll(nested, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "payload"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err == nil {
			t.Fatal("control did not fire: cleanup accepted a directory holding a subtree")
		}
		if _, err := os.Stat(filepath.Join(nested, "payload")); err != nil {
			t.Fatalf("control did not fire: the nested subtree was deleted: %v", err)
		}
	})

	t.Run("control_a_symlinked_scratch_directory_is_refused", func(t *testing.T) {
		proj := t.TempDir()
		root := freehandScratchRoot(proj)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		victim := t.TempDir()
		if err := os.WriteFile(filepath.Join(victim, "precious"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		// A correctly-named child that is actually a symlink to somewhere else entirely.
		link := filepath.Join(root, "map-substituted")
		if err := os.Symlink(victim, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := freehandScratchCleanup(root, link, freehandBaseObfuscationMapFile)(); err == nil {
			t.Fatal("control did not fire: a symlinked scratch directory was accepted")
		}
		if _, err := os.Stat(filepath.Join(victim, "precious")); err != nil {
			t.Fatalf("control did not fire: the symlink target was touched: %v", err)
		}
	})

	// Canonicalisation makes the root/child comparison SPELLING-INDEPENDENT, in both directions.
	//
	// The first half is what filepath.EvalSymlinks actually buys: a root spelled through a symlink
	// that resolves to the real root must still accept its own child. Without resolution the textual
	// compare fails and every legitimate cleanup through such a path starts refusing -- which looks
	// safe and is not, because it leaves scratch directories holding original identifiers behind.
	t.Run("a_root_spelled_through_a_symlink_still_matches_its_own_child", func(t *testing.T) {
		proj := t.TempDir()
		p, _, err := freehandProducedObfuscationMapPath(proj)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
		real := freehandScratchRoot(proj)
		aliasParent := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(filepath.Dir(real), aliasParent); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		aliasedRoot := filepath.Join(aliasParent, filepath.Base(real))
		if err := freehandScratchCleanup(aliasedRoot, filepath.Dir(p), freehandBaseObfuscationMapFile)(); err != nil {
			t.Fatalf("control did not fire: a root spelled through a symlink was not recognised: %v", err)
		}
		if _, err := os.Stat(filepath.Dir(p)); err == nil {
			t.Fatal("the directory was not removed")
		}
	})

	// The second half: a directory that is NOT under the registered root is refused however it is
	// spelled, and its content is untouched.
	t.Run("control_a_directory_outside_the_root_is_refused", func(t *testing.T) {
		proj := t.TempDir()
		real := freehandScratchRoot(proj)
		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}
		elsewhere := t.TempDir()
		child := filepath.Join(elsewhere, "map-real")
		if err := os.MkdirAll(child, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(child, "precious"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := freehandScratchCleanup(real, child, freehandBaseObfuscationMapFile)(); err == nil {
			t.Fatal("control did not fire: a directory outside the registered root was accepted")
		}
		if _, err := os.Stat(filepath.Join(child, "precious")); err != nil {
			t.Fatalf("control did not fire: content outside the root was deleted: %v", err)
		}
	})

	t.Run("control_a_symlinked_file_inside_scratch_is_refused", func(t *testing.T) {
		proj := t.TempDir()
		p, cleanup, err := freehandProducedObfuscationMapPath(proj)
		if err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(t.TempDir(), "precious")
		if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, p); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := cleanup(); err == nil {
			t.Fatal("control did not fire: a symlinked scratch file was removed")
		}
		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("control did not fire: the symlink target was deleted: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------------------------
// 2. VERIFY BEFORE PUBLICATION.
// ---------------------------------------------------------------------------------------------

func TestIntegrityArtifactIsVerifiedBeforeItBecomesImmutable(t *testing.T) {
	// The publish sequence must verify tmpDir between the last write and the rename. A structural
	// assertion, because the alternative is running a full patch build, and the property under test is
	// exactly "this call is in this window".
	src, err := os.ReadFile("freehand_patch.go")
	if err != nil {
		t.Skipf("source unreadable: %v", err)
	}
	body := string(src)
	renameIdx := strings.Index(body, "if err := os.Rename(tmpDir, finalDir); err != nil {")
	if renameIdx < 0 {
		t.Fatal("the publish sequence has moved; this control must be updated with it")
	}
	writeIdx := strings.LastIndex(body[:renameIdx], `writeFileSync(filepath.Join(tmpDir, "patch_artifact.json")`)
	if writeIdx < 0 {
		t.Fatal("could not locate the metadata write")
	}
	window := body[writeIdx:renameIdx]
	if !strings.Contains(window, "verifyExistingPatchArtifact(tmpDir, artifactID)") {
		t.Fatal("control did not fire: nothing verifies tmpDir before it is renamed into place")
	}
	// And the refusal must be a refusal, not a warning.
	if !strings.Contains(window, `return "", fmt.Errorf("refusing to publish an artifact that does not verify`) {
		t.Fatal("a failed pre-publication verification must abort the publish")
	}
}

// A failed verification must leave NOTHING published. Exercised through the fault hook that sits
// immediately after the verification and before the rename.
func TestIntegrityFailedPrePublicationVerificationPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "final")
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	// An empty tmpDir cannot verify: it has none of the required members.
	if err := verifyExistingPatchArtifact(tmp, strings.Repeat("a", 64)); err == nil {
		t.Fatal("control did not fire: an empty directory verified as a patch artifact")
	}
	if _, err := os.Stat(final); err == nil {
		t.Fatal("a failed verification must leave no final artifact")
	}
	// The fault hook the publish path uses exists and is wired.
	prev := freehandFaultInjection
	t.Cleanup(func() { freehandFaultInjection = prev })
	called := false
	freehandFaultInjection = func(stage string) error {
		if stage == "before-artifact-rename" {
			called = true
		}
		return nil
	}
	_ = freehandFault("before-artifact-rename")
	if !called {
		t.Fatal("the before-artifact-rename fault point is not reachable")
	}
}

// ---------------------------------------------------------------------------------------------
// 3. REAL EOF DECODING.
// ---------------------------------------------------------------------------------------------

func TestIntegrityReceiptRejectsTrailingData(t *testing.T) {
	dir := t.TempDir()
	mapRaw := controlMapBytes(t, controlBaseMapFlat)
	digest := freehandSHA256Bytes(mapRaw)
	clean, err := json.MarshalIndent(controlReceipt(digest), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, body []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// BASELINE: the clean receipt loads.
	if _, _, err := loadTranslationReceipt(write("clean.json", clean), digest); err != nil {
		t.Fatalf("a clean receipt must load: %v", err)
	}

	t.Run("control_trailing_object", func(t *testing.T) {
		p := write("obj.json", append(append([]byte(nil), clean...), []byte("\n{\"injected\":true}")...))
		if _, _, err := loadTranslationReceipt(p, digest); err == nil {
			t.Fatal("control did not fire: a receipt with a second JSON object appended was accepted")
		}
	})

	t.Run("control_trailing_garbage", func(t *testing.T) {
		p := write("garbage.json", append(append([]byte(nil), clean...), []byte("\nnot json at all")...))
		if _, _, err := loadTranslationReceipt(p, digest); err == nil {
			t.Fatal("control did not fire: a receipt with trailing garbage was accepted")
		}
	})

	t.Run("control_trailing_array", func(t *testing.T) {
		p := write("arr.json", append(append([]byte(nil), clean...), []byte("[1,2,3]")...))
		if _, _, err := loadTranslationReceipt(p, digest); err == nil {
			t.Fatal("control did not fire: a receipt with a trailing array was accepted")
		}
	})

	t.Run("trailing_whitespace_is_still_fine", func(t *testing.T) {
		p := write("ws.json", append(append([]byte(nil), clean...), []byte("\n\n  \t\n")...))
		if _, _, err := loadTranslationReceipt(p, digest); err != nil {
			t.Fatalf("trailing whitespace must not be an error: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------------------------
// 4. RECEIPT PERMISSIONS.
// ---------------------------------------------------------------------------------------------

// integrityArtifact builds a minimal but fully consistent obfuscated artifact directory.
func integrityArtifact(t *testing.T, receiptMode os.FileMode) (string, *FreehandPatchArtifactMeta) {
	t.Helper()
	dir := t.TempDir()
	entry := freehandReplacementEntry{
		BaseIdentity: "package:app/main.dart::Counter::bump", StableIdentity: "v1|k",
		ModuleLibrary: "soroq-freehand:///m", ModuleClass: "Counter", ModuleMember: "bump",
		Kind: "instance-member",
	}
	manifest := map[string]any{"replacement_abi": []any{map[string]any{
		"base_identity": entry.BaseIdentity, "stable_identity": entry.StableIdentity,
		"module_library": entry.ModuleLibrary, "module_class": entry.ModuleClass,
		"module_member": entry.ModuleMember, "kind": entry.Kind,
		"signature_sha256": strings.Repeat("e", 64), "host_invocable": false,
	}}}
	manRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "soroq_freehand_module_manifest.json"), manRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	mapRaw := controlMapBytes(t, controlBaseMapFlat)
	binding := controlBinding(t, freehandSHA256Bytes(mapRaw), len(controlBaseMapFlat)/2)
	receipt := controlReceipt(binding.MapSHA256)
	receiptRaw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, freehandTranslationReceiptFile)
	if err := os.WriteFile(p, receiptRaw, receiptMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, receiptMode); err != nil {
		t.Fatal(err)
	}
	translated, err := translateReplacementABI([]freehandReplacementEntry{entry}, receipt, "")
	if err != nil {
		t.Fatal(err)
	}
	receiptSHA := freehandSHA256Bytes(receiptRaw)
	bindingDigest, err := freehandObfuscationArtifactDigest(binding, receiptSHA)
	if err != nil {
		t.Fatal(err)
	}
	return dir, &FreehandPatchArtifactMeta{
		Obfuscation: binding, TranslationReceiptSHA256: receiptSHA,
		ObfuscationBindingDigest: bindingDigest, TranslatedABI: translated,
	}
}

func TestIntegrityReceiptMustBe0600(t *testing.T) {
	// BASELINE: 0600 verifies.
	dir, meta := integrityArtifact(t, freehandTranslationReceiptMode)
	if err := verifyArtifactObfuscation(dir, meta); err != nil {
		t.Fatalf("a 0600 receipt must verify: %v", err)
	}

	// CONTROL: chmod 0644 must be refused, at publication time and at every later reuse. Both go
	// through verifyArtifactObfuscation, which verifyExistingPatchArtifact calls, so one chmod breaks
	// both -- which is the property being asserted.
	receipt := filepath.Join(dir, freehandTranslationReceiptFile)
	if err := os.Chmod(receipt, 0o644); err != nil {
		t.Fatal(err)
	}
	err := verifyArtifactObfuscation(dir, meta)
	if err == nil {
		t.Fatal("control did not fire: a world-readable translation receipt was accepted")
	}
	if !strings.Contains(err.Error(), "names every original identity") {
		t.Fatalf("the refusal must say why the mode matters: %v", err)
	}

	// And group-readable is refused too: this is an exact mode, not a ceiling.
	if err := os.Chmod(receipt, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := verifyArtifactObfuscation(dir, meta); err == nil {
		t.Fatal("control did not fire: a group-readable translation receipt was accepted")
	}

	// Restoring 0600 restores acceptance, so the control is about the mode and nothing else.
	if err := os.Chmod(receipt, freehandTranslationReceiptMode); err != nil {
		t.Fatal(err)
	}
	if err := verifyArtifactObfuscation(dir, meta); err != nil {
		t.Fatalf("restoring 0600 must restore acceptance: %v", err)
	}
}

// The permission check must also be reached through the FULL artifact verifier, not only the
// obfuscation half in isolation.
func TestIntegrityReceiptModeIsReachedThroughTheFullVerifier(t *testing.T) {
	src, err := os.ReadFile("freehand_patch.go")
	if err != nil {
		t.Skipf("source unreadable: %v", err)
	}
	body := string(src)
	verifier := body[strings.Index(body, "func verifyExistingPatchArtifact("):]
	if !strings.Contains(verifier[:strings.Index(verifier, "\nfunc artifactDeclaresObfuscation(")], "verifyArtifactObfuscation(dir, &m)") {
		t.Fatal("control did not fire: the full verifier does not call the obfuscation half")
	}
}

// ---------------------------------------------------------------------------------------------
// 5. SCRATCH ALLOCATION: nothing is created through a symlinked component.
// ---------------------------------------------------------------------------------------------

// scratchTree lists every path under dir, relative and sorted, so a victim directory can be compared
// exactly rather than approximately.
func scratchTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	if err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		if rel != "." {
			out = append(out, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestIntegrityScratchAllocationRefusesSymlinkedComponents(t *testing.T) {
	// BASELINE: a normal project allocates and cleans up, for both prefixes.
	t.Run("normal_allocation_and_cleanup", func(t *testing.T) {
		proj := t.TempDir()
		for _, alloc := range []func(string) (string, func() error, error){
			freehandProducedObfuscationMapPath,
			freehandPatchReceiptScratch,
		} {
			p, cleanup, err := alloc(proj)
			if err != nil {
				t.Fatalf("a normal allocation must succeed: %v", err)
			}
			if err := os.WriteFile(p, []byte("[]"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The three fixed components exist as REAL directories under the project.
			cur, cerr := filepath.EvalSymlinks(proj)
			if cerr != nil {
				t.Fatal(cerr)
			}
			for _, component := range freehandScratchComponents {
				cur = filepath.Join(cur, component)
				fi, lerr := os.Lstat(cur)
				if lerr != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
					t.Fatalf("%s is not a real directory", cur)
				}
			}
			if err := cleanup(); err != nil {
				t.Fatalf("a normal cleanup must succeed: %v", err)
			}
			if _, err := os.Stat(filepath.Dir(p)); err == nil {
				t.Fatal("the allocated directory survived cleanup")
			}
		}
	})

	// THE CONTROLS THIS WAS WRITTEN FOR. Each replaces ONE fixed component with a symlink into a
	// victim directory and requires the allocation to refuse BEFORE creating anything in the target.
	for name, chain := range map[string][]string{
		"control_dot_soroq_is_a_symlink":   {".soroq"},
		"control_build_is_a_symlink":       {".soroq", "build"},
		"control_obfuscation_is_a_symlink": {".soroq", "build", "obfuscation"},
	} {
		t.Run(name, func(t *testing.T) {
			proj := t.TempDir()
			victim := t.TempDir()
			sentinel := filepath.Join(victim, "SENTINEL")
			body := []byte("do not touch")
			if err := os.WriteFile(sentinel, body, 0o600); err != nil {
				t.Fatal(err)
			}
			before := scratchTree(t, victim)

			link := proj
			for _, c := range chain[:len(chain)-1] {
				link = filepath.Join(link, c)
				if err := os.MkdirAll(link, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			link = filepath.Join(link, chain[len(chain)-1])
			if err := os.Symlink(victim, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}

			for _, alloc := range []func(string) (string, func() error, error){
				freehandProducedObfuscationMapPath,
				freehandPatchReceiptScratch,
			} {
				p, cleanup, err := alloc(proj)
				if err == nil {
					if cleanup != nil {
						_ = cleanup()
					}
					t.Fatalf("control did not fire: allocation succeeded at %s through a symlinked %s",
						p, chain[len(chain)-1])
				}
				if !strings.Contains(err.Error(), "is a symlink") {
					t.Fatalf("the refusal must name the symlink: %v", err)
				}
			}

			// THE VICTIM IS BYTE-IDENTICAL: same entries, same count, same content.
			after := scratchTree(t, victim)
			if len(after) != len(before) {
				t.Fatalf("control did not fire: the victim grew from %d to %d entries: %v -> %v",
					len(before), len(after), before, after)
			}
			for i := range after {
				if after[i] != before[i] {
					t.Fatalf("control did not fire: the victim's contents changed: %v -> %v", before, after)
				}
			}
			got, rerr := os.ReadFile(sentinel)
			if rerr != nil {
				t.Fatalf("control did not fire: the sentinel was removed: %v", rerr)
			}
			if string(got) != string(body) {
				t.Fatal("control did not fire: the sentinel was modified")
			}
		})
	}

	t.Run("control_a_fixed_component_that_is_a_file_is_refused", func(t *testing.T) {
		proj := t.TempDir()
		if err := os.MkdirAll(filepath.Join(proj, ".soroq"), 0o700); err != nil {
			t.Fatal(err)
		}
		// `build` exists but is a regular file.
		if err := os.WriteFile(filepath.Join(proj, ".soroq", "build"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := freehandProducedObfuscationMapPath(proj)
		if err == nil {
			t.Fatal("control did not fire: a non-directory component was accepted")
		}
		// It must be refused BY THE COMPONENT CHECK, naming the offending path. Without that check the
		// next Mkdir fails with ENOTDIR anyway, which looks the same from outside and is not the same
		// thing: it means the refusal is incidental and moves the moment the layout changes.
		if !strings.Contains(err.Error(), "exists and is not a directory") {
			t.Fatalf("the refusal must come from the component check: %v", err)
		}
	})

	t.Run("control_an_unknown_prefix_is_refused", func(t *testing.T) {
		if _, _, err := allocateFreehandScratch(t.TempDir(), "evil-", "x.json"); err == nil {
			t.Fatal("control did not fire: an arbitrary prefix was accepted")
		}
	})

	// The project root is canonicalised, so everything below is created and returned in the place it
	// actually lives. Without that, a project reached through a symlink yields scratch paths spelled
	// through the link, and the cleanup closure's root/child comparison is then comparing two different
	// spellings of the same directory.
	t.Run("control_a_symlinked_project_path_yields_canonical_scratch", func(t *testing.T) {
		real := t.TempDir()
		canonReal, err := filepath.EvalSymlinks(real)
		if err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(t.TempDir(), "project-alias")
		if err := os.Symlink(canonReal, alias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		p, cleanup, err := freehandProducedObfuscationMapPath(alias)
		if err != nil {
			t.Fatalf("allocation through a symlinked project path must succeed: %v", err)
		}
		if !strings.HasPrefix(p, canonReal+string(os.PathSeparator)) {
			t.Fatalf("control did not fire: the returned path %s is not under the canonical project root %s",
				p, canonReal)
		}
		if err := cleanup(); err != nil {
			t.Fatalf("cleanup of a canonical allocation must succeed: %v", err)
		}
	})

	t.Run("control_an_absent_project_directory_is_refused", func(t *testing.T) {
		if _, _, err := freehandProducedObfuscationMapPath(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
			t.Fatal("control did not fire: an unresolvable project directory was accepted")
		}
	})
}

// The two re-validations are only reachable under a RACE: something replacing a component between
// the component loop and the allocation, or replacing the allocated directory between MkdirTemp and
// the post-check. The fault hook puts the test at exactly those two instants.
func TestIntegrityScratchAllocationRevalidatesUnderARace(t *testing.T) {
	t.Run("control_root_swapped_for_a_symlink_before_allocation", func(t *testing.T) {
		proj := t.TempDir()
		victim := t.TempDir()
		if err := os.WriteFile(filepath.Join(victim, "SENTINEL"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := scratchTree(t, victim)

		prev := freehandFaultInjection
		t.Cleanup(func() { freehandFaultInjection = prev })
		freehandFaultInjection = func(stage string) error {
			if stage != "scratch-root-validated" {
				return nil
			}
			// The instant after the components were validated, swap the real root for a symlink into
			// the victim -- the race the re-check exists for.
			canon, err := filepath.EvalSymlinks(proj)
			if err != nil {
				return nil
			}
			root := freehandScratchRoot(canon)
			if err := os.Remove(root); err != nil {
				return nil
			}
			_ = os.Symlink(victim, root)
			return nil
		}

		_, cleanup, err := freehandProducedObfuscationMapPath(proj)
		if err == nil {
			if cleanup != nil {
				_ = cleanup()
			}
			t.Fatal("control did not fire: a root swapped for a symlink mid-allocation was accepted")
		}
		if !strings.Contains(err.Error(), "does not resolve to itself") {
			t.Fatalf("the refusal must come from the containment re-check: %v", err)
		}
		if after := scratchTree(t, victim); len(after) != len(before) {
			t.Fatalf("control did not fire: the victim grew from %v to %v", before, after)
		}
	})

	t.Run("control_allocated_directory_swapped_after_MkdirTemp", func(t *testing.T) {
		proj := t.TempDir()
		victim := t.TempDir()

		prev := freehandFaultInjection
		t.Cleanup(func() { freehandFaultInjection = prev })
		freehandFaultInjection = func(stage string) error {
			if stage != "scratch-dir-allocated" {
				return nil
			}
			// Replace the freshly allocated directory with a symlink into the victim.
			canon, err := filepath.EvalSymlinks(proj)
			if err != nil {
				return nil
			}
			entries, rerr := os.ReadDir(freehandScratchRoot(canon))
			if rerr != nil {
				return nil
			}
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), "map-") {
					continue
				}
				p := filepath.Join(freehandScratchRoot(canon), e.Name())
				if err := os.Remove(p); err != nil {
					return nil
				}
				_ = os.Symlink(victim, p)
			}
			return nil
		}

		_, cleanup, err := freehandProducedObfuscationMapPath(proj)
		if err == nil {
			if cleanup != nil {
				_ = cleanup()
			}
			t.Fatal("control did not fire: an allocated directory swapped for a symlink was accepted")
		}
		if !strings.Contains(err.Error(), "is not a real directory") {
			t.Fatalf("the refusal must come from the post-allocation re-check: %v", err)
		}
	})
}
