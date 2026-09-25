package main

// [soroq] Android obfuscated OTA -- Option A (handoff 14).
//
// An Android patch replaces the whole libapp.so. Under --obfuscate, a fresh gen_snapshot run assigns
// fresh names, so identifiers the running app has already persisted by name (the callback cache,
// anything keyed by runtimeType.toString(), restoration ids) stop resolving after the swap. Option A
// seeds the candidate's obfuscation from the base's own saved map: every name the base assigned is
// kept, and names for new identifiers are minted past the base's, so they cannot collide.
//
// The seed lives in the HOST tool only (gen_snapshot --load-obfuscation-map). Nothing reaches the
// device: no map, no names, no migration. What the CLI adds is the bookkeeping that makes the seed
// impossible to get wrong silently:
//
//   - release: an obfuscated build is permitted only with a toolchain that DECLARES the capability
//     (engine.json) AND whose gen_snapshot actually advertises --load-obfuscation-map. The base map is
//     captured outside the build tree and committed, with its digest, beside the release (0600).
//   - patch: the base's record decides. An obfuscated base requires an obfuscated, seeded candidate;
//     a plain base refuses an obfuscated one. After the build, the candidate's own map must contain
//     every base pair unchanged and stay injective, or nothing is published.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	androidObfuscationSeedCapability = "obfuscation_map_seed_v1"
	androidObfuscationMapFile        = "android_base_obfuscation_map.json"
	androidObfuscationRecordFile     = "android_obfuscation.json"
	androidObfuscationRecordSchema   = "soroq.android_obfuscation.v1"
	androidObfuscationTargetPlatform = "android-arm64"
)

// androidObfuscationRecord is what a release remembers about its obfuscation. The map itself sits
// beside it; the digest pins which map.
type androidObfuscationRecord struct {
	Schema           string `json:"schema"`
	MapFile          string `json:"map_file"`
	MapSHA256        string `json:"map_sha256"`
	MapEntries       int    `json:"map_entries"`
	ToolchainVersion string `json:"toolchain_version"`
}

// androidObfuscationPlan is what an obfuscated build needs: the extra Flutter args that drive the
// seed, and where this build's own map lands.
type androidObfuscationPlan struct {
	Obfuscated   bool
	ExtraArgs    []string
	SavedMapPath string
	BaseMapPath  string
	scratchDir   string
}

func (p *androidObfuscationPlan) cleanup() {
	if p != nil && p.scratchDir != "" {
		_ = os.RemoveAll(p.scratchDir)
	}
}

// androidGenSnapshotHelpFn returns gen_snapshot's --help text. A variable so tests need no binary.
var androidGenSnapshotHelpFn = func(genSnapshot string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// gen_snapshot prints usage for --help and exits non-zero on some builds; the text is what matters.
	out, err := exec.CommandContext(ctx, genSnapshot, "--help").CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s --help timed out", genSnapshot)
	}
	if len(out) == 0 && err != nil {
		return "", err
	}
	return string(out), nil
}

// androidToolchainSupportsObfuscationSeed requires BOTH the declaration and the binary. A declaration
// alone could be a packaging mistake over a stock gen_snapshot, which would ignore nothing -- it would
// reject the unknown flag -- but a binary alone is an undeclared capability nobody reviewed.
func androidToolchainSupportsObfuscationSeed(toolchainVersion string) error {
	version := strings.TrimSpace(toolchainVersion)
	if version == "" {
		return errors.New("no Android toolchain was resolved for this build; obfuscated Android OTA needs an installed toolchain that declares " + androidObfuscationSeedCapability + " (pass --toolchain <version>)")
	}
	dir, err := androidCachedToolchainBundleDir(version)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "engine.json"))
	if err != nil {
		return fmt.Errorf("android toolchain %q is not installed: %w", version, err)
	}
	var engine struct {
		Capabilities []string `json:"soroq_android_capabilities"`
	}
	if err := json.Unmarshal(raw, &engine); err != nil {
		return fmt.Errorf("android toolchain %q engine.json: %w", version, err)
	}
	declared := false
	for _, c := range engine.Capabilities {
		if c == androidObfuscationSeedCapability {
			declared = true
		}
	}
	if !declared {
		return fmt.Errorf("android toolchain %q does not declare %s: its gen_snapshot cannot keep an obfuscated base's names, so a patch would rename identifiers the running app has already persisted", version, androidObfuscationSeedCapability)
	}
	help, err := androidGenSnapshotHelpFn(filepath.Join(dir, "gen_snapshot"))
	if err != nil {
		return fmt.Errorf("android toolchain %q: cannot run gen_snapshot: %w", version, err)
	}
	if !strings.Contains(help, "--load-obfuscation-map") {
		return fmt.Errorf("android toolchain %q declares %s but its gen_snapshot does not support --load-obfuscation-map; refusing a toolchain whose declaration and binary disagree", version, androidObfuscationSeedCapability)
	}
	return nil
}

// validateAndroidObfuscationArgs checks the user's Flutter args for an obfuscated build.
func validateAndroidObfuscationArgs(args []string) error {
	flags := dedupeStrings(detectObfuscationFlags(args))
	hasObfuscate, hasSplit := false, false
	for _, f := range flags {
		switch f {
		case "--obfuscate":
			hasObfuscate = true
		case "--split-debug-info":
			hasSplit = true
		}
	}
	if !hasObfuscate || !hasSplit {
		return errors.New("an obfuscated Android build needs both --obfuscate and --split-debug-info=<dir> (Flutter requires the pair)")
	}
	platforms := []string{}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch {
		case strings.HasPrefix(arg, "--extra-gen-snapshot-options"):
			return errors.New("--extra-gen-snapshot-options cannot be combined with an obfuscated Soroq Android build: Soroq drives gen_snapshot's obfuscation-map options itself, and a second set would override them")
		case arg == "--target-platform" && i+1 < len(args):
			platforms = append(platforms, strings.Split(args[i+1], ",")...)
			i++
		case strings.HasPrefix(arg, "--target-platform="):
			platforms = append(platforms, strings.Split(strings.TrimPrefix(arg, "--target-platform="), ",")...)
		}
	}
	// One ABI, one gen_snapshot run, one map. Several ABIs would each write the map in turn.
	if len(platforms) != 1 || strings.TrimSpace(platforms[0]) != androidObfuscationTargetPlatform {
		return fmt.Errorf("an obfuscated Soroq Android build must target exactly %s (pass --target-platform %s); with several ABIs each gen_snapshot run would overwrite the captured map", androidObfuscationTargetPlatform, androidObfuscationTargetPlatform)
	}
	return nil
}

func newAndroidObfuscationScratch(projectDir string) (string, error) {
	base := filepath.Join(projectDir, ".soroq", "build")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(base, "android-obfuscation-*")
	if err != nil {
		return "", err
	}
	if strings.Contains(dir, ",") {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("project path %q contains a comma, which gen_snapshot's option list cannot carry", dir)
	}
	return dir, nil
}

// planAndroidReleaseObfuscation decides what an Android RELEASE build does about obfuscation.
func planAndroidReleaseObfuscation(projectDir, toolchainVersion string, args []string) (*androidObfuscationPlan, error) {
	if len(detectObfuscationFlags(args)) == 0 {
		return &androidObfuscationPlan{}, nil
	}
	if err := validateAndroidObfuscationArgs(args); err != nil {
		return nil, err
	}
	if err := androidToolchainSupportsObfuscationSeed(toolchainVersion); err != nil {
		return nil, err
	}
	dir, err := newAndroidObfuscationScratch(projectDir)
	if err != nil {
		return nil, err
	}
	saved := filepath.Join(dir, "map.json")
	return &androidObfuscationPlan{
		Obfuscated:   true,
		SavedMapPath: saved,
		ExtraArgs:    []string{"--extra-gen-snapshot-options=--save-obfuscation-map=" + saved},
		scratchDir:   dir,
	}, nil
}

func androidReleaseObfuscationDir(projectDir, releaseID string) (string, error) {
	id := strings.TrimSpace(releaseID)
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", fmt.Errorf("invalid release id %q", releaseID)
	}
	return filepath.Join(projectDir, ".soroq", "releases", id), nil
}

// parseAndroidObfuscationMap reads gen_snapshot's flat [name, renamed, ...] map.
func parseAndroidObfuscationMap(raw []byte) (map[string]string, error) {
	var flat []string
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("not an obfuscation map: %w", err)
	}
	if len(flat)%2 != 0 {
		return nil, fmt.Errorf("obfuscation map has an odd number of strings (%d)", len(flat))
	}
	pairs := make(map[string]string, len(flat)/2)
	for i := 0; i < len(flat); i += 2 {
		if _, dup := pairs[flat[i]]; dup {
			return nil, fmt.Errorf("obfuscation map names %q twice", flat[i])
		}
		pairs[flat[i]] = flat[i+1]
	}
	if len(pairs) == 0 {
		return nil, errors.New("obfuscation map is empty")
	}
	return pairs, nil
}

// commitAndroidReleaseObfuscation moves this build's map into the release's state and records it.
func commitAndroidReleaseObfuscation(projectDir, releaseID, toolchainVersion string, plan *androidObfuscationPlan) (*androidObfuscationRecord, error) {
	if plan == nil || !plan.Obfuscated {
		return nil, nil
	}
	raw, err := os.ReadFile(plan.SavedMapPath)
	if err != nil {
		return nil, fmt.Errorf("the obfuscated build wrote no obfuscation map (%s): %w; refusing to register a base no patch could be seeded from", plan.SavedMapPath, err)
	}
	pairs, err := parseAndroidObfuscationMap(raw)
	if err != nil {
		return nil, err
	}
	dir, err := androidReleaseObfuscationDir(projectDir, releaseID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	rec := &androidObfuscationRecord{
		Schema:           androidObfuscationRecordSchema,
		MapFile:          androidObfuscationMapFile,
		MapSHA256:        hex.EncodeToString(sum[:]),
		MapEntries:       len(pairs),
		ToolchainVersion: strings.TrimSpace(toolchainVersion),
	}
	if err := writePrivateFileAtomic(filepath.Join(dir, androidObfuscationMapFile), raw); err != nil {
		return nil, err
	}
	recBytes, _ := json.MarshalIndent(rec, "", "  ")
	if err := writePrivateFileAtomic(filepath.Join(dir, androidObfuscationRecordFile), append(recBytes, '\n')); err != nil {
		return nil, err
	}
	return rec, nil
}

// loadAndroidReleaseObfuscation returns the release's record, or nil if the base was not obfuscated.
// A record whose map is missing or does not hash to the recorded digest is an error, never "plain".
func loadAndroidReleaseObfuscation(projectDir, releaseID string) (*androidObfuscationRecord, string, error) {
	dir, err := androidReleaseObfuscationDir(projectDir, releaseID)
	if err != nil {
		return nil, "", err
	}
	recRaw, err := os.ReadFile(filepath.Join(dir, androidObfuscationRecordFile))
	mapPath := filepath.Join(dir, androidObfuscationMapFile)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(mapPath); statErr == nil {
			return nil, "", fmt.Errorf("release %s has an obfuscation map but no record of it; refusing to guess whether the base is obfuscated", releaseID)
		}
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var rec androidObfuscationRecord
	if err := json.Unmarshal(recRaw, &rec); err != nil || rec.Schema != androidObfuscationRecordSchema {
		return nil, "", fmt.Errorf("release %s: unreadable obfuscation record", releaseID)
	}
	raw, err := os.ReadFile(mapPath)
	if err != nil {
		return nil, "", fmt.Errorf("release %s is obfuscated but its base obfuscation map is missing (%s): no patch can keep its names", releaseID, mapPath)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != rec.MapSHA256 {
		return nil, "", fmt.Errorf("release %s: base obfuscation map does not match its recorded digest", releaseID)
	}
	return &rec, mapPath, nil
}

// planAndroidPatchObfuscation decides what an Android PATCH build does, from the base's record.
func planAndroidPatchObfuscation(projectDir, releaseID, toolchainVersion string, args []string) (*androidObfuscationPlan, error) {
	rec, baseMap, err := loadAndroidReleaseObfuscation(projectDir, releaseID)
	if err != nil {
		return nil, err
	}
	requested := len(detectObfuscationFlags(args)) > 0
	switch {
	case rec == nil && !requested:
		return &androidObfuscationPlan{}, nil
	case rec == nil && requested:
		return nil, fmt.Errorf("release %s was not built with --obfuscate (no captured map), so an obfuscated patch would rename every identifier the running app knows; build the patch without --obfuscate, or release an obfuscated base first", releaseID)
	case !requested:
		return nil, fmt.Errorf("release %s is obfuscated, so the patch must be too: pass --obfuscate --split-debug-info=<dir> --target-platform %s", releaseID, androidObfuscationTargetPlatform)
	}
	if err := validateAndroidObfuscationArgs(args); err != nil {
		return nil, err
	}
	if err := androidToolchainSupportsObfuscationSeed(toolchainVersion); err != nil {
		return nil, err
	}
	if strings.Contains(baseMap, ",") {
		return nil, fmt.Errorf("base map path %q contains a comma, which gen_snapshot's option list cannot carry", baseMap)
	}
	dir, err := newAndroidObfuscationScratch(projectDir)
	if err != nil {
		return nil, err
	}
	saved := filepath.Join(dir, "map.json")
	return &androidObfuscationPlan{
		Obfuscated:   true,
		BaseMapPath:  baseMap,
		SavedMapPath: saved,
		ExtraArgs: []string{"--extra-gen-snapshot-options=--load-obfuscation-map=" + baseMap +
			",--save-obfuscation-map=" + saved},
		scratchDir: dir,
	}, nil
}

// androidSeedVerification summarises the candidate map check.
type androidSeedVerification struct {
	BaseEntries      int
	CandidateEntries int
	NewEntries       int
}

// verifyAndroidSeededCandidate is the gate between an obfuscated patch build and publication: the
// candidate must keep every base pair exactly and must not map two identifiers to one name.
func verifyAndroidSeededCandidate(plan *androidObfuscationPlan) (*androidSeedVerification, error) {
	if plan == nil || !plan.Obfuscated {
		return nil, nil
	}
	baseRaw, err := os.ReadFile(plan.BaseMapPath)
	if err != nil {
		return nil, err
	}
	base, err := parseAndroidObfuscationMap(baseRaw)
	if err != nil {
		return nil, fmt.Errorf("base map: %w", err)
	}
	candRaw, err := os.ReadFile(plan.SavedMapPath)
	if err != nil {
		return nil, fmt.Errorf("the obfuscated patch build wrote no obfuscation map, so its names cannot be shown to match the base's: %w", err)
	}
	cand, err := parseAndroidObfuscationMap(candRaw)
	if err != nil {
		return nil, fmt.Errorf("candidate map: %w", err)
	}
	for name, renamed := range base {
		got, ok := cand[name]
		if !ok {
			return nil, fmt.Errorf("the candidate's obfuscation dropped %q, which the base renamed to %q; refusing to publish a patch that is not seeded from its base", name, renamed)
		}
		if got != renamed {
			return nil, fmt.Errorf("the candidate renamed %q to %q but the base uses %q; refusing to publish a patch whose names differ from the running app's", name, got, renamed)
		}
	}
	owner := make(map[string]string, len(cand))
	for name, renamed := range cand {
		if name == renamed {
			continue
		}
		if prev, taken := owner[renamed]; taken && prev != name {
			return nil, fmt.Errorf("the candidate's obfuscation maps both %q and %q to %q", prev, name, renamed)
		}
		owner[renamed] = name
	}
	return &androidSeedVerification{
		BaseEntries:      len(base),
		CandidateEntries: len(cand),
		NewEntries:       len(cand) - len(base),
	}, nil
}

// writePrivateFileAtomic writes a 0600 file via rename: the base map names every renamed identifier
// in the app, so it is never world-readable and never half-written.
func writePrivateFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
