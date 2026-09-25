package main

// Soroq freehand — OBFUSCATED BASE support.
//
// A patchable base built with `--obfuscate` presents renamed declarations. The iOS freehand lane binds
// a patch BY DECLARATION IDENTITY, comparing `<library-url>::<class>::<member>` as strings, so a patch
// carrying source-level names misses an obfuscated base entirely — and misses silently, which is the
// whole reason obfuscated builds were refused rather than merely warned about.
//
// The producer-side answer has four parts, and every one of them is derived, never asserted:
//
//  1. OBFUSCATION STATE COMES FROM THE VERIFIED BASELINE. Not from a CLI flag, not from the presence of
//     a map file on disk. A caller who could name a map could name any map; see authorizeObfuscation.
//  2. THE MAP IS CAPTURED FROM THE EXACT gen_snapshot INVOCATION that produced the shipped base, and
//     stored release-side at mode 0600. It is never committed, uploaded, logged or packaged.
//  3. dart2bytecode TRANSLATES the module's identities through that map and emits an EXACT receipt.
//  4. THE DEVICE ABI IS BUILT FROM THAT RECEIPT, so the runtime strings the engine compares are the
//     base's own names.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// freehandObfuscationBindingSchema tags the recorded encoding so a future format cannot be misread
	// as this one.
	freehandObfuscationBindingSchema = "soroq.freehand.obfuscation.v1"

	// The two formats this lane understands. A file declaring anything else is foreign and refused.
	soroqBaseMapFormat                   = "soroq-obfuscation-base-map"
	soroqBaseMapFormatVersion            = 1
	soroqTranslationReceiptFormat        = "soroq-obfuscation-translation-receipt"
	soroqTranslationReceiptFormatVersion = 1

	// freehandObfuscatedIdentityTranslationCapability is what an engine bundle declares when its
	// dart2bytecode can translate module identities into an obfuscated base's namespace (the R6
	// toolchain). An older toolchain declares nothing, and an obfuscated build stays refused.
	freehandObfuscatedIdentityTranslationCapability = "obfuscated_identity_translation_v1"

	// freehandBaseObfuscationMapFile is the captured map, release-side, beside baseline.json.
	freehandBaseObfuscationMapFile = "base_obfuscation_map.json"

	// freehandTranslationReceiptFile is the compiler's exact record of the translation. Release-side
	// only: it names every original identity in the module.
	freehandTranslationReceiptFile = "translation_receipt.json"

	// freehandTranslationReceiptMode is the ONLY mode the persisted receipt may have, for the same
	// reason the captured map has one: it carries the original names.
	freehandTranslationReceiptMode fs.FileMode = 0o600

	// freehandBaseObfuscationMapMode is the ONLY mode this file may have. It names every private
	// declaration in the app; group- or world-readable is a disclosure.
	freehandBaseObfuscationMapMode fs.FileMode = 0o600
)

// FreehandObfuscationBinding is the recorded, per-base answer to "was this base obfuscated, and which
// map is its ABI authority?". It lives in baseline.json and is copied into the patch artifact and the
// signed device manifest, so the binding a device honours is the one the base was built with.
type FreehandObfuscationBinding struct {
	Schema  string `json:"schema"`
	Enabled bool   `json:"enabled"`
	// MapFormat / MapFormatVersion identify the ENCODING of the captured map, so a future format is a
	// refusal rather than a misparse.
	MapFormat        string `json:"map_format"`
	MapFormatVersion int    `json:"map_format_version"`
	// MapSHA256 is the digest of the captured bytes. It is what dart2bytecode is required to match and
	// what the artifact and manifest bind.
	MapSHA256 string `json:"map_sha256"`
	// MapMode is the recorded permission of the file at rest, checked again at patch time.
	MapMode string `json:"map_mode"`
	// MapFile is the name of the captured map INSIDE the baseline directory. A bare name, never a
	// path: the map lives beside the baseline and nowhere else.
	MapFile string `json:"map_file"`
	// MapEntries is the pair count, recorded so a truncated map is visible without re-reading it.
	MapEntries int `json:"map_entries"`
	// Capability / EngineRevision record WHICH engine authorised this, so a base built by a toolchain
	// that could not translate can never claim it could.
	Capability     string `json:"capability"`
	EngineRevision string `json:"engine_revision"`
}

func (b *FreehandObfuscationBinding) digest() (string, error) {
	if b == nil {
		return "", errors.New("no obfuscation binding to digest")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return freehandSHA256Bytes(raw), nil
}

// isEnabled answers for a possibly-absent binding. A base with no record is NOT obfuscated: absence is
// never read as permission.
func (b *FreehandObfuscationBinding) isEnabled() bool { return b != nil && b.Enabled }

// validate re-derives everything checkable about a recorded binding. A hand-edited baseline that
// asserts obfuscation without a well-formed record is refused here rather than at compile time.
func (b *FreehandObfuscationBinding) validate() error {
	if b == nil {
		return nil
	}
	if b.Schema != freehandObfuscationBindingSchema {
		return fmt.Errorf("obfuscation binding declares schema %q, expected %q", b.Schema, freehandObfuscationBindingSchema)
	}
	if !b.Enabled {
		return errors.New("obfuscation binding is recorded but disabled; a disabled binding must be absent, not present")
	}
	if b.MapFormat != soroqBaseMapFormat || b.MapFormatVersion != soroqBaseMapFormatVersion {
		return fmt.Errorf("obfuscation binding declares a foreign map format %q v%d (this toolchain reads %q v%d)",
			b.MapFormat, b.MapFormatVersion, soroqBaseMapFormat, soroqBaseMapFormatVersion)
	}
	if !isHexSHA256(b.MapSHA256) {
		return fmt.Errorf("obfuscation binding map_sha256 is not a sha256: %q", b.MapSHA256)
	}
	if b.MapFile != freehandBaseObfuscationMapFile {
		return fmt.Errorf("obfuscation binding names map file %q; the captured map is always %q beside the baseline",
			b.MapFile, freehandBaseObfuscationMapFile)
	}
	if b.MapMode != freehandBaseObfuscationMapMode.String() {
		return fmt.Errorf("obfuscation binding records map mode %q, expected %q", b.MapMode, freehandBaseObfuscationMapMode.String())
	}
	if b.MapEntries <= 0 {
		return fmt.Errorf("obfuscation binding records %d map entries", b.MapEntries)
	}
	if b.Capability != freehandObfuscatedIdentityTranslationCapability {
		return fmt.Errorf("obfuscation binding names capability %q, expected %q", b.Capability, freehandObfuscatedIdentityTranslationCapability)
	}
	if strings.TrimSpace(b.EngineRevision) == "" {
		return errors.New("obfuscation binding records no engine revision")
	}
	return nil
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ---------------------------------------------------------------------------------------------
// AUTHORIZATION
// ---------------------------------------------------------------------------------------------

// freehandObfuscationAuthorization is the answer to "may THIS command build with --obfuscate?".
//
// It is resolved from the TOOLCHAIN that will do the building, exactly like every other identity
// capability: the engine bundle declares what it can honour, and an engine that declares nothing keeps
// the build refused. It is never derived from a command-line flag, because a flag proves only that
// someone typed it.
type freehandObfuscationAuthorization struct {
	Allowed        bool
	Capability     string
	EngineRevision string
	Reason         string
}

// authorizeObfuscationFromToolchain reads the installed iOS bundle's engine.json and reports whether it
// declares the translation capability. Every failure is a refusal that NAMES what was missing: a
// silent false here would be indistinguishable from an old toolchain, and the operator needs to know
// which one they have.
func authorizeObfuscationFromToolchain(iosBundleDir string) (*freehandObfuscationAuthorization, error) {
	enginePath := filepath.Join(iosBundleDir, "engine.json")
	raw, err := os.ReadFile(enginePath)
	if err != nil {
		return &freehandObfuscationAuthorization{
			Reason: fmt.Sprintf("no engine bundle declaration at %s", enginePath),
		}, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("engine bundle %s is unparseable: %w", enginePath, err)
	}
	var rev string
	if r, ok := doc["soroq_engine_revision"]; ok {
		_ = json.Unmarshal(r, &rev)
	}
	decl, ok := doc[freehandEngineCapabilityKey]
	if !ok {
		return &freehandObfuscationAuthorization{
			EngineRevision: rev,
			Reason:         fmt.Sprintf("engine bundle %s declares no %s", enginePath, freehandEngineCapabilityKey),
		}, nil
	}
	_, caps, derr := decodeEngineCapabilityDeclaration(decl)
	if derr != nil {
		// A malformed declaration is an error, not a downgrade: an engine tried to say something and
		// the producer could not read it.
		return nil, fmt.Errorf("engine bundle %s declares a malformed %s: %w", enginePath, freehandEngineCapabilityKey, derr)
	}
	for _, c := range caps {
		if c == freehandObfuscatedIdentityTranslationCapability {
			return &freehandObfuscationAuthorization{
				Allowed:        true,
				Capability:     freehandObfuscatedIdentityTranslationCapability,
				EngineRevision: rev,
				Reason:         fmt.Sprintf("engine %s declares %s", rev, freehandObfuscatedIdentityTranslationCapability),
			}, nil
		}
	}
	return &freehandObfuscationAuthorization{
		EngineRevision: rev,
		Reason: fmt.Sprintf("engine %s declares identity capabilities %v, which do not include %s",
			rev, caps, freehandObfuscatedIdentityTranslationCapability),
	}, nil
}

// authorizeObfuscationForToolchain resolves authorization from an installed toolchain VERSION. A
// version that is not installed is not an error here: the build itself reports that, in its own words,
// and this must not pre-empt it with a worse message.
func authorizeObfuscationForToolchain(toolchainVersion string) (*freehandObfuscationAuthorization, error) {
	toolchainVersion = strings.TrimSpace(toolchainVersion)
	if toolchainVersion == "" {
		return &freehandObfuscationAuthorization{Reason: "no --toolchain was given, so no capability could be resolved"}, nil
	}
	iosBundleDir, err := iosCachedToolchainBundleDir(toolchainVersion)
	if err != nil {
		return &freehandObfuscationAuthorization{
			Reason: fmt.Sprintf("toolchain %q is not resolvable on this machine", toolchainVersion),
		}, nil
	}
	return authorizeObfuscationFromToolchain(iosBundleDir)
}

// ---------------------------------------------------------------------------------------------
// THE CAPTURED MAP
// ---------------------------------------------------------------------------------------------

// parseBaseObfuscationMap validates a `--save-obfuscation-map` output and returns its pair count.
//
// It mirrors the checks dart2bytecode makes, deliberately. The compiler's refusal happens far from the
// person who ran the release; catching a malformed map at capture time names it while they are still
// looking at the build.
func parseBaseObfuscationMap(raw []byte) (int, error) {
	var flat []any
	if err := json.Unmarshal(raw, &flat); err != nil {
		return 0, fmt.Errorf("obfuscation map is not the flat JSON array --save-obfuscation-map produces: %w", err)
	}
	if len(flat) == 0 {
		return 0, errors.New("obfuscation map is empty; an obfuscated base always names at least its protected symbols")
	}
	if len(flat)%2 != 0 {
		return 0, fmt.Errorf("obfuscation map has an odd entry count (%d); it is not a flat key/value array", len(flat))
	}
	seenKey := make(map[string]string, len(flat)/2)
	seenValue := make(map[string]string, len(flat)/2)
	for i := 0; i < len(flat); i += 2 {
		key, kok := flat[i].(string)
		value, vok := flat[i+1].(string)
		if !kok || !vok {
			return 0, fmt.Errorf("obfuscation map entry %d is not a string pair", i)
		}
		// THE EMPTY NAME. Every real --save-obfuscation-map output carries ("", ""): the empty identifier
		// (an unnamed constructor's name, for one) is never renamed, and gen_snapshot records it like any
		// other protected name. Until R6's first real obfuscated release every map this parser had seen
		// was hand-made, so the pair was refused and no real obfuscated baseline could be persisted
		// (handoff 17, F4). Only that exact identity pair is legitimate; one empty side is malformed.
		if (key == "") != (value == "") {
			return 0, fmt.Errorf("obfuscation map entry %d maps %q to %q; only (\"\", \"\") may be empty", i, key, value)
		}
		if prev, dup := seenKey[key]; dup {
			return 0, fmt.Errorf("obfuscation map defines %q twice (-> %q and -> %q); the ABI would be ambiguous", key, prev, value)
		}
		if owner, dup := seenValue[value]; dup {
			return 0, fmt.Errorf("obfuscation map renames both %q and %q to %q; translation would not be injective", owner, key, value)
		}
		seenKey[key] = value
		seenValue[value] = key
	}
	return len(flat) / 2, nil
}

// freehandScratchComponents are the FIXED path segments, in order, between the project root and the
// scratch root. They are constants, never caller-supplied, and each one is created and checked
// individually by allocateFreehandScratch.
var freehandScratchComponents = []string{".soroq", "build", "obfuscation"}

// freehandScratchRoot is the textual scratch root. Use it for reporting and for the cleanup closure's
// shape check; use allocateFreehandScratch to actually create anything, because this function does no
// validation and cannot: it is pure path arithmetic.
func freehandScratchRoot(projectDir string) string {
	return filepath.Join(append([]string{projectDir}, freehandScratchComponents...)...)
}

// allocateFreehandScratch creates the scratch root COMPONENT BY COMPONENT and allocates one unique
// directory beneath it.
//
// WHY NOT os.MkdirAll. MkdirAll follows symlinks. With `.soroq`, `.soroq/build` or
// `.soroq/build/obfuscation` replaced by a symlink, it happily created the remaining components and
// then a `map-*` directory INSIDE the link's target -- and returned no error at all. Cleanup refused
// afterwards, which is too late: directories had already been created in someone else's tree, and the
// refusal left them there.
//
// So every fixed component is created with os.Mkdir (one level, never through a link) and Lstat'd:
// if it exists it must be a real directory, and a symlink or a non-directory is a refusal BEFORE
// anything further is touched. Containment is re-checked immediately before and after the temporary
// directory is created, and the result must be a real direct child of the canonical root carrying the
// prefix this package allocates.
func allocateFreehandScratch(projectDir, prefix, fileName string) (string, func() error, error) {
	if prefix != "map-" && prefix != "receipt-" {
		return "", nil, fmt.Errorf("refusing scratch allocation: %q is not a soroq scratch prefix", prefix)
	}
	// THE PROJECT ROOT is the caller's own directory and may legitimately be reached through a symlink
	// (every macOS temp dir is). It is canonicalised ONCE, and everything below is built from the
	// resolved form, so the components are checked in the place they actually live.
	canonProject, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return "", nil, fmt.Errorf("refusing scratch allocation: project directory %s is unresolvable: %w", projectDir, err)
	}
	if fi, serr := os.Lstat(canonProject); serr != nil || !fi.IsDir() {
		return "", nil, fmt.Errorf("refusing scratch allocation: project directory %s is not a directory", canonProject)
	}

	cur := canonProject
	for _, component := range freehandScratchComponents {
		cur = filepath.Join(cur, component)
		// Mkdir, not MkdirAll: exactly one level, and it fails rather than traversing a link.
		if merr := os.Mkdir(cur, 0o700); merr != nil && !os.IsExist(merr) {
			return "", nil, fmt.Errorf("refusing scratch allocation: cannot create %s: %w", cur, merr)
		}
		fi, lerr := os.Lstat(cur)
		if lerr != nil {
			return "", nil, fmt.Errorf("refusing scratch allocation: %s: %w", cur, lerr)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf(
				"refusing scratch allocation: %s is a symlink; soroq will not create scratch directories "+
					"through a link, because they would be created inside its target", cur)
		}
		if !fi.IsDir() {
			return "", nil, fmt.Errorf("refusing scratch allocation: %s exists and is not a directory", cur)
		}
	}
	root := cur

	// The component loop above already establishes both facts, so the two re-checks that follow are
	// only reachable when something CHANGES BETWEEN THEM -- another process swapping a component while
	// this one is mid-allocation. That is exactly why they were asked for "immediately before and
	// after", and the fault points are how the race is exercised rather than argued about.
	if err := freehandFault("scratch-root-validated"); err != nil {
		return "", nil, err
	}

	// CONTAINMENT, immediately before allocating: the root must BE the fixed path under the canonical
	// project root, and must resolve to itself.
	if expected := freehandScratchRoot(canonProject); root != expected {
		return "", nil, fmt.Errorf("refusing scratch allocation: resolved root %s is not %s", root, expected)
	}
	if resolved, rerr := filepath.EvalSymlinks(root); rerr != nil || resolved != root {
		return "", nil, fmt.Errorf("refusing scratch allocation: scratch root %s does not resolve to itself", root)
	}

	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		return "", nil, fmt.Errorf("allocate scratch directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", nil, err
	}

	if err := freehandFault("scratch-dir-allocated"); err != nil {
		return "", nil, err
	}

	// CONTAINMENT, immediately after: a real direct child of the same root, with our prefix.
	fi, lerr := os.Lstat(dir)
	if lerr != nil {
		return "", nil, fmt.Errorf("refusing scratch allocation: %s: %w", dir, lerr)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", nil, fmt.Errorf("refusing scratch allocation: %s is not a real directory", dir)
	}
	if filepath.Dir(dir) != root {
		return "", nil, fmt.Errorf("refusing scratch allocation: %s is not a direct child of %s", dir, root)
	}
	if !strings.HasPrefix(filepath.Base(dir), prefix) {
		return "", nil, fmt.Errorf("refusing scratch allocation: %s does not carry the %q prefix", dir, prefix)
	}
	return filepath.Join(dir, fileName), freehandScratchCleanup(root, dir, fileName), nil
}

// freehandProducedObfuscationMapPath allocates a UNIQUE scratch file for one build's obfuscation map
// and returns it with an exact cleanup.
//
// A fixed path per project let two releases delete and adopt each other's map; nothing about "clear
// the stale one first" fixes that, because both builds write the same name at the same time.
func freehandProducedObfuscationMapPath(projectDir string) (string, func() error, error) {
	return allocateFreehandScratch(projectDir, "map-", freehandBaseObfuscationMapFile)
}

// freehandPatchReceiptScratch is the same allocation for a patch's translation receipt. The receipt
// used to be written into the IMMUTABLE baseline directory, with each patch deleting the previous one
// first, so two patches against one base raced over a file nothing is allowed to touch.
func freehandPatchReceiptScratch(projectDir string) (string, func() error, error) {
	return allocateFreehandScratch(projectDir, "receipt-", freehandTranslationReceiptFile)
}

// freehandScratchCleanup returns a closure that removes ONE registered file and then the now-empty
// directory that held it.
//
// It used to call os.RemoveAll. That is a recursive delete of a path held in a variable, which is the
// shape this codebase forbids everywhere else, and it was not theoretical: a file or subtree that
// appeared inside a registered scratch directory -- another process's, a debugger's, a developer's --
// was deleted silently, and nothing said so.
//
// So the contract is now exact. The closure knows the ONE filename it allocated. It removes that file,
// then os.Remove (never RemoveAll) the directory, which succeeds only if the directory is otherwise
// empty. Anything unexpected inside SURVIVES and the cleanup REFUSES, reporting what it found. A
// refusal leaks one small directory; a recursive delete loses someone's data.
//
// The returned error is for controls and diagnostics. Callers use this in defer, where a leaked
// scratch directory is the correct outcome of a refusal.
func freehandScratchCleanup(root, dir, name string) func() error {
	return func() error {
		absRoot, err := canonicalScratchPath(root)
		if err != nil {
			return fmt.Errorf("refusing scratch cleanup: unresolvable root %s: %w", root, err)
		}
		absDir, err := canonicalScratchPath(dir)
		if err != nil {
			return fmt.Errorf("refusing scratch cleanup: unresolvable directory %s: %w", dir, err)
		}
		// SHAPE. Exactly one level under the fixed root, under a name this package allocates.
		if absDir == absRoot || filepath.Dir(absDir) != absRoot {
			return fmt.Errorf("refusing scratch cleanup: %s is not a direct child of %s", absDir, absRoot)
		}
		base := filepath.Base(absDir)
		if !strings.HasPrefix(base, "map-") && !strings.HasPrefix(base, "receipt-") {
			return fmt.Errorf("refusing scratch cleanup: %s is not a soroq scratch directory", absDir)
		}
		if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
			return fmt.Errorf("refusing scratch cleanup: %q is not a plain file name", name)
		}
		// The directory itself must be a real directory, not a symlink pointing somewhere else.
		fi, err := os.Lstat(absDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // already gone; nothing to do and nothing to complain about
			}
			return fmt.Errorf("refusing scratch cleanup: %w", err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("refusing scratch cleanup: %s is not a real directory", absDir)
		}
		// THE ONE FILE. Removed by exact name, and only when it is a regular file.
		target := filepath.Join(absDir, name)
		if tfi, terr := os.Lstat(target); terr == nil {
			if tfi.Mode()&os.ModeSymlink != 0 || !tfi.Mode().IsRegular() {
				return fmt.Errorf("refusing scratch cleanup: %s is not a regular file", target)
			}
			if rerr := os.Remove(target); rerr != nil {
				return fmt.Errorf("scratch cleanup could not remove %s: %w", target, rerr)
			}
		} else if !os.IsNotExist(terr) {
			return fmt.Errorf("refusing scratch cleanup: %w", terr)
		}
		// THE DIRECTORY. os.Remove, never RemoveAll: it fails if anything else is in there, and that
		// failure is the point.
		if rerr := os.Remove(absDir); rerr != nil {
			leftovers, _ := os.ReadDir(absDir)
			names := make([]string, 0, len(leftovers))
			for _, e := range leftovers {
				names = append(names, e.Name())
			}
			sort.Strings(names)
			if len(names) > 0 {
				return fmt.Errorf("refusing to remove scratch directory %s: it holds %d unexpected entry/entries (%s); they were left untouched",
					absDir, len(names), strings.Join(names, ", "))
			}
			return fmt.Errorf("scratch cleanup could not remove %s: %w", absDir, rerr)
		}
		return nil
	}
}

// canonicalScratchPath resolves a path for the shape checks above, refusing a symlinked component.
//
// The root is resolved through its real ancestors, so substituting a symlink for the scratch root --
// or for the directory itself -- cannot make an outside path look like a registered child.
func canonicalScratchPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(abs)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(abs)), nil
}

// describeProducedObfuscationMap validates the map the base build produced and returns the binding
// that will be recorded. It does NOT move anything: the baseline directory does not exist yet.
func describeProducedObfuscationMap(producedMapPath string, auth *freehandObfuscationAuthorization) (*FreehandObfuscationBinding, error) {
	if auth == nil || !auth.Allowed {
		return nil, errors.New("refusing to record an obfuscation map for a build that was not authorized to produce one")
	}
	raw, err := os.ReadFile(producedMapPath)
	if err != nil {
		return nil, fmt.Errorf("the obfuscated build produced no obfuscation map at %s: %w", producedMapPath, err)
	}
	entries, err := parseBaseObfuscationMap(raw)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &FreehandObfuscationBinding{
		Schema:           freehandObfuscationBindingSchema,
		Enabled:          true,
		MapFormat:        soroqBaseMapFormat,
		MapFormatVersion: soroqBaseMapFormatVersion,
		MapSHA256:        hex.EncodeToString(sum[:]),
		MapMode:          freehandBaseObfuscationMapMode.String(),
		MapFile:          freehandBaseObfuscationMapFile,
		MapEntries:       entries,
		Capability:       auth.Capability,
		EngineRevision:   auth.EngineRevision,
	}, nil
}

// publishObfuscationMapInto writes the map into a baseline's TEMPORARY directory and verifies it
// there, before baseline.json exists. The caller's rename then publishes both or neither.
func publishObfuscationMapInto(tmpDir, srcPath string, b *FreehandObfuscationBinding) error {
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read the produced obfuscation map: %w", err)
	}
	if got := freehandSHA256Bytes(raw); got != b.MapSHA256 {
		return fmt.Errorf("the obfuscation map changed between validation (%s) and publication (%s)", b.MapSHA256, got)
	}
	entries, err := parseBaseObfuscationMap(raw)
	if err != nil {
		return err
	}
	if entries != b.MapEntries {
		return fmt.Errorf("the obfuscation map has %d entries, the binding records %d", entries, b.MapEntries)
	}
	dst := filepath.Join(tmpDir, b.MapFile)
	if err := writeFileSync(dst, raw, freehandBaseObfuscationMapMode); err != nil {
		return fmt.Errorf("write the obfuscation map into the baseline transaction: %w", err)
	}
	// Explicit chmod: the create mode is subject to umask, and 0600 is a security property here.
	if err := os.Chmod(dst, freehandBaseObfuscationMapMode); err != nil {
		return err
	}
	// Verify AT REST, in the temporary directory, so a bad write never reaches the rename.
	check, err := readCapturedObfuscationMap(dst)
	if err != nil {
		return err
	}
	if freehandSHA256Bytes(check) != b.MapSHA256 {
		return errors.New("the obfuscation map did not read back as written")
	}
	return nil
}

// readCapturedObfuscationMap reads a captured map, refusing anything that is not a regular file at
// exactly mode 0600. It is the single place those two facts are checked, so the transaction and the
// verifier cannot drift apart.
func readCapturedObfuscationMap(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("the base declares obfuscation but its captured map is missing at %s: %w", p, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("captured obfuscation map is a symlink: %s", p)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("captured obfuscation map is not a regular file: %s", p)
	}
	if perm := fi.Mode().Perm(); perm != freehandBaseObfuscationMapMode {
		return nil, fmt.Errorf("captured obfuscation map %s is mode %s, expected %s; it names every private declaration in the app",
			p, perm.String(), freehandBaseObfuscationMapMode.String())
	}
	return os.ReadFile(p)
}

// resolveBaseObfuscationMap returns the captured map's path after re-checking everything the binding
// claims about it. Nothing recorded in baseline.json is trusted on its own.
func resolveBaseObfuscationMap(baselineDir string, b *FreehandObfuscationBinding) (string, error) {
	if err := b.validate(); err != nil {
		return "", err
	}
	if !b.isEnabled() {
		return "", errors.New("no obfuscation binding to resolve")
	}
	p := filepath.Join(baselineDir, b.MapFile)
	raw, err := readCapturedObfuscationMap(p)
	if err != nil {
		return "", err
	}
	if got := freehandSHA256Bytes(raw); got != b.MapSHA256 {
		return "", fmt.Errorf("captured obfuscation map %s hashes to %s, but the baseline binds %s", p, got, b.MapSHA256)
	}
	entries, err := parseBaseObfuscationMap(raw)
	if err != nil {
		return "", err
	}
	if entries != b.MapEntries {
		return "", fmt.Errorf("captured obfuscation map %s has %d entries, the baseline binds %d", p, entries, b.MapEntries)
	}
	return p, nil
}

// ---------------------------------------------------------------------------------------------
// THE TRANSLATION RECEIPT
// ---------------------------------------------------------------------------------------------

// FreehandTranslationIdentity is ONE translated identity, exactly as dart2bytecode recorded it.
type FreehandTranslationIdentity struct {
	Context           string `json:"context"`
	Library           string `json:"library,omitempty"`
	EnclosingOriginal string `json:"enclosing_original,omitempty"`
	EnclosingRuntime  string `json:"enclosing_runtime,omitempty"`
	SelectorPrefix    string `json:"selector_prefix,omitempty"`
	Original          string `json:"original"`
	Runtime           string `json:"runtime"`
	CompositeOriginal string `json:"composite_original,omitempty"`
	CompositeRuntime  string `json:"composite_runtime,omitempty"`
	Origin            string `json:"origin"`
}

// The three contexts and two origins dart2bytecode emits.
const (
	freehandTranslationContextClass    = "class"
	freehandTranslationContextMember   = "member"
	freehandTranslationContextSelector = "selector"
	freehandTranslationOriginBase      = "base"
	freehandTranslationOriginModule    = "module-local"
)

// FreehandTranslationReceipt is the compiler's exact record of what it translated. The device ABI is
// built FROM this, so it is validated strictly rather than read opportunistically.
type FreehandTranslationReceipt struct {
	Format             string                        `json:"format"`
	FormatVersion      int                           `json:"format_version"`
	ObfuscationEnabled bool                          `json:"obfuscation_enabled"`
	BaseMapSHA256      string                        `json:"base_map_sha256"`
	BaseMapEntries     int                           `json:"base_map_entries"`
	IdentityCount      int                           `json:"identity_count"`
	Identities         []FreehandTranslationIdentity `json:"identities"`
}

// loadTranslationReceipt reads, strictly decodes and fully validates a receipt, and returns it with the
// digest of its exact bytes — which is what the artifact and the signed manifest bind.
//
// expectedMapSHA is the baseline's map digest. A receipt produced against a DIFFERENT map is the
// wrong-baseline case, and it is caught here rather than on a device.
func loadTranslationReceipt(path, expectedMapSHA string) (*FreehandTranslationReceipt, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("the translating compilation produced no receipt at %s: %w", path, err)
	}
	var r FreehandTranslationReceipt
	// STRICT, WITH A REAL END OF INPUT. This decoded once and stopped, so anything appended after the
	// JSON document -- a second object, or plain garbage -- was accepted and never seen again. The
	// digest binding does not save you there: the artifact would simply bind the digest of a file with
	// an extra payload stapled to it. decodeStrictJSONWithEOF reads one more token and requires io.EOF.
	if err := decodeStrictJSONWithEOF(raw, &r); err != nil {
		return nil, "", fmt.Errorf("decode translation receipt %s: %w", path, err)
	}
	if r.Format != soroqTranslationReceiptFormat || r.FormatVersion != soroqTranslationReceiptFormatVersion {
		return nil, "", fmt.Errorf("translation receipt declares a foreign format %q v%d (this toolchain reads %q v%d)",
			r.Format, r.FormatVersion, soroqTranslationReceiptFormat, soroqTranslationReceiptFormatVersion)
	}
	if !r.ObfuscationEnabled {
		return nil, "", errors.New("translation receipt says obfuscation was not enabled, but the base declares it was")
	}
	if !isHexSHA256(r.BaseMapSHA256) {
		return nil, "", fmt.Errorf("translation receipt base_map_sha256 is not a sha256: %q", r.BaseMapSHA256)
	}
	if expectedMapSHA != "" && r.BaseMapSHA256 != expectedMapSHA {
		return nil, "", fmt.Errorf("translation receipt was produced against base map %s, but this base binds %s; "+
			"the module was translated for a different base", r.BaseMapSHA256, expectedMapSHA)
	}
	if r.IdentityCount != len(r.Identities) {
		return nil, "", fmt.Errorf("translation receipt declares %d identities but carries %d", r.IdentityCount, len(r.Identities))
	}
	if len(r.Identities) == 0 {
		return nil, "", errors.New("translation receipt records no identities; a translated module always translates at least one")
	}
	seen := make(map[string]string, len(r.Identities))
	for i, id := range r.Identities {
		switch id.Context {
		case freehandTranslationContextClass, freehandTranslationContextMember, freehandTranslationContextSelector:
		default:
			return nil, "", fmt.Errorf("translation receipt identity %d has unknown context %q", i, id.Context)
		}
		switch id.Origin {
		case freehandTranslationOriginBase, freehandTranslationOriginModule:
		default:
			return nil, "", fmt.Errorf("translation receipt identity %d has unknown origin %q", i, id.Origin)
		}
		if id.Original == "" || id.Runtime == "" {
			return nil, "", fmt.Errorf("translation receipt identity %d has an empty half", i)
		}
		// AN ORIGINAL NAME MUST NEVER SURVIVE AS ITS OWN RUNTIME NAME unless the obfuscator protected
		// it (PreventRenaming writes name -> name). We cannot tell those apart from the receipt alone,
		// so this is not an error here; the release lane's own control asserts it against the map.
		key := id.Context + "|" + id.Library + "|" + id.EnclosingOriginal + "|" + preferComposite(id.CompositeOriginal, id.Original)
		if prev, dup := seen[key]; dup && prev != id.Runtime {
			return nil, "", fmt.Errorf("translation receipt maps %q to both %q and %q", key, prev, id.Runtime)
		}
		seen[key] = id.Runtime
	}
	return &r, freehandSHA256Bytes(raw), nil
}

// preferComposite picks the composite (prefixed) form when there is one, falling back to the bare
// identifier. A selector's ABI string is the composite; a plain member's is the identifier itself.
func preferComposite(composite, bare string) string {
	if composite != "" {
		return composite
	}
	return bare
}

// freehandTranslationIndex is the receipt turned into the three lookups the ABI builder needs.
type freehandTranslationIndex struct {
	classes   map[string]string
	members   map[string]string
	selectors map[string]string
}

func classKey(library, name string) string { return library + "|" + name }
func memberKey(library, enclosing, mangled string) string {
	return library + "|" + enclosing + "|" + mangled
}

func (r *FreehandTranslationReceipt) index() *freehandTranslationIndex {
	ix := &freehandTranslationIndex{
		classes:   map[string]string{},
		members:   map[string]string{},
		selectors: map[string]string{},
	}
	for _, id := range r.Identities {
		switch id.Context {
		case freehandTranslationContextClass:
			ix.classes[classKey(id.Library, id.Original)] = id.Runtime
		case freehandTranslationContextMember:
			ix.members[memberKey(id.Library, id.EnclosingOriginal, preferComposite(id.CompositeOriginal, id.Original))] =
				preferComposite(id.CompositeRuntime, id.Runtime)
			if id.EnclosingOriginal != "" && id.EnclosingRuntime != "" {
				ix.classes[classKey(id.Library, id.EnclosingOriginal)] = id.EnclosingRuntime
			}
		case freehandTranslationContextSelector:
			ix.selectors[classKey(id.Library, preferComposite(id.CompositeOriginal, id.Original))] =
				preferComposite(id.CompositeRuntime, id.Runtime)
		}
	}
	return ix
}

// className resolves a class identity, trying the private (library-qualified) form first and then the
// public one. A miss is reported, never guessed: an unresolved identity would ship as a source name and
// silently fail to bind on a device.
func (ix *freehandTranslationIndex) className(library, name string) (string, bool) {
	if name == "" {
		return "", true // a top-level declaration has no class segment
	}
	if v, ok := ix.classes[classKey(library, name)]; ok {
		return v, true
	}
	if v, ok := ix.classes[classKey("", name)]; ok {
		return v, true
	}
	return "", false
}

// memberName resolves a member identity, falling back to the selector table for a name that was only
// ever seen at a call site.
func (ix *freehandTranslationIndex) memberName(library, enclosing, mangled string) (string, bool) {
	if v, ok := ix.members[memberKey(library, enclosing, mangled)]; ok {
		return v, true
	}
	if v, ok := ix.members[memberKey("", enclosing, mangled)]; ok {
		return v, true
	}
	if v, ok := ix.members[memberKey(library, "", mangled)]; ok {
		return v, true
	}
	if v, ok := ix.members[memberKey("", "", mangled)]; ok {
		return v, true
	}
	if v, ok := ix.selectors[classKey(library, mangled)]; ok {
		return v, true
	}
	if v, ok := ix.selectors[classKey("", mangled)]; ok {
		return v, true
	}
	return "", false
}

// sortedUnresolved renders a stable, bounded list of unresolved identities for a refusal message.
func sortedUnresolved(in map[string]bool) []string {
	out := make([]string, 0, len(in))
	for k := range in {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > 8 {
		out = append(out[:8], fmt.Sprintf("... and %d more", len(in)-8))
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// THE TRANSLATED ABI
// ---------------------------------------------------------------------------------------------

// FreehandTranslatedABIEntry is one replacement-ABI entry expressed in the identities the RUNTIME
// actually compares.
//
// The source-level fields stay where they are, in the durable ABI: they are what semantic diffing,
// review and every existing verification path read, and they must keep meaning what they meant. This
// is a SEPARATE, additional projection, keyed by the frozen stable identity, so the two never drift
// into one another.
type FreehandTranslatedABIEntry struct {
	// StableIdentity is the join key back to the durable ABI entry this translates.
	StableIdentity string `json:"stable_identity"`
	// BaseIdentity is the source-level `libUri::class::member`, repeated so an entry is readable alone.
	BaseIdentity string `json:"base_identity"`
	// RuntimeBaseIdentity is the same triple with the class and member segments translated into the
	// base snapshot's namespace. The LIBRARY URI is never translated: it is not an identifier, and
	// dart2bytecode does not translate it either.
	RuntimeBaseIdentity string `json:"runtime_base_identity"`
	// The module side, translated. Empty class means a top-level declaration, exactly as before.
	ModuleLibrary       string `json:"module_library"`
	ModuleClass         string `json:"module_class"`
	ModuleMember        string `json:"module_member"`
	RuntimeModuleClass  string `json:"runtime_module_class"`
	RuntimeModuleMember string `json:"runtime_module_member"`
	Kind                string `json:"kind"`
}

// splitBaseIdentity splits `libUri::class::member` from the RIGHT, because a library URI may itself
// contain "::". This mirrors the controller's own parser exactly; if the two ever disagree the device
// would resolve a different triple than the producer intended.
func splitBaseIdentity(identity string) (libURI, class, member string, err error) {
	lastSep := strings.LastIndex(identity, "::")
	if lastSep <= 0 {
		return "", "", "", fmt.Errorf("malformed base identity %q", identity)
	}
	member = identity[lastSep+2:]
	head := identity[:lastSep]
	midSep := strings.LastIndex(head, "::")
	if midSep < 0 {
		return "", "", "", fmt.Errorf("malformed base identity %q", identity)
	}
	libURI = head[:midSep]
	class = head[midSep+2:]
	if libURI == "" || member == "" {
		return "", "", "", fmt.Errorf("malformed base identity %q", identity)
	}
	return libURI, class, member, nil
}

// translateReplacementABI projects the durable ABI into runtime identities using the compiler's own
// receipt.
//
// EVERY identity must resolve. An unresolved one would ship as a source-level name against an
// obfuscated base, which is precisely the silent miss this whole lane exists to remove -- the redirect
// installs, the transition commits, and nothing changes. So a miss is a refusal that names what was
// missing, not a passthrough.
func translateReplacementABI(
	entries []freehandReplacementEntry,
	receipt *FreehandTranslationReceipt,
	moduleLibrary string,
) ([]FreehandTranslatedABIEntry, error) {
	if receipt == nil {
		return nil, errors.New("cannot translate a replacement ABI without a translation receipt")
	}
	ix := receipt.index()
	out := make([]FreehandTranslatedABIEntry, 0, len(entries))
	unresolved := map[string]bool{}
	for _, e := range entries {
		libURI, class, member, err := splitBaseIdentity(e.BaseIdentity)
		if err != nil {
			return nil, err
		}
		runtimeClass, okClass := ix.className(libURI, class)
		if !okClass {
			unresolved[fmt.Sprintf("base class %s::%s", libURI, class)] = true
		}
		runtimeMember, okMember := ix.memberName(libURI, class, member)
		if !okMember {
			unresolved[fmt.Sprintf("base member %s::%s::%s", libURI, class, member)] = true
		}
		runtimeModuleClass, okModClass := ix.className(moduleLibrary, e.ModuleClass)
		if !okModClass {
			unresolved[fmt.Sprintf("module class %s::%s", moduleLibrary, e.ModuleClass)] = true
		}
		runtimeModuleMember, okModMember := ix.memberName(moduleLibrary, e.ModuleClass, e.ModuleMember)
		if !okModMember {
			unresolved[fmt.Sprintf("module member %s::%s::%s", moduleLibrary, e.ModuleClass, e.ModuleMember)] = true
		}
		if !okClass || !okMember || !okModClass || !okModMember {
			continue
		}
		out = append(out, FreehandTranslatedABIEntry{
			StableIdentity:      e.StableIdentity,
			BaseIdentity:        e.BaseIdentity,
			RuntimeBaseIdentity: libURI + "::" + runtimeClass + "::" + runtimeMember,
			ModuleLibrary:       e.ModuleLibrary,
			ModuleClass:         e.ModuleClass,
			ModuleMember:        e.ModuleMember,
			RuntimeModuleClass:  runtimeModuleClass,
			RuntimeModuleMember: runtimeModuleMember,
			Kind:                e.Kind,
		})
	}
	if len(unresolved) > 0 {
		return nil, fmt.Errorf(
			"the translation receipt does not cover %d identity/identities this patch replaces, so the "+
				"patch would ship source-level names against an obfuscated base and silently bind nothing:\n  %s",
			len(unresolved), strings.Join(sortedUnresolved(unresolved), "\n  "))
	}
	if len(out) != len(entries) {
		return nil, fmt.Errorf("translated %d of %d replacement-ABI entries", len(out), len(entries))
	}
	return out, nil
}
