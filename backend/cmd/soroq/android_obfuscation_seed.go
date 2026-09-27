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
	// v2 is a multi-ABI base: one map per ABI (androidObfuscationMapPattern). A CLI that knows only v1
	// refuses it as unreadable rather than seeding every ABI from one of them.
	androidObfuscationRecordSchemaV2 = "soroq.android_obfuscation.v2"
	androidObfuscationPlatformToken  = "{soroq_target_platform}"
	androidObfuscationMapPattern     = "android_base_obfuscation_map." + androidObfuscationPlatformToken + ".json"
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
	// Maps is the v2 (multi-ABI) record: each ABI's own map, by --target-platform. Each ABI's
	// gen_snapshot names identifiers in its own order (measured: 439 of 16,737 library names differ
	// between arm and arm64), and a device only ever runs its own ABI, so each ABI's patches are seeded
	// from, and checked against, that ABI's map. v1 records carry the single arm64 map in MapFile.
	Maps map[string]androidObfuscationMapDigest `json:"maps,omitempty"`
}

type androidObfuscationMapDigest struct {
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Entries int    `json:"entries"`
}

// platforms lists a record's ABIs in table order.
func (r *androidObfuscationRecord) platforms() []string {
	if r.Schema != androidObfuscationRecordSchemaV2 {
		return []string{androidObfuscationTargetPlatform}
	}
	var out []string
	for _, info := range androidABITable {
		if _, ok := r.Maps[info.TargetPlatform]; ok {
			out = append(out, info.TargetPlatform)
		}
	}
	return out
}

// androidObfuscationPlan is what an obfuscated build needs: the extra Flutter args that drive the
// seed, and where this build's own map lands.
type androidObfuscationPlan struct {
	Obfuscated   bool
	ExtraArgs    []string
	SavedMapPath string
	BaseMapPath  string
	scratchDir   string
	// TargetPlatforms is what a multi-ABI build compiles; nil for the single android-arm64 build, whose
	// map paths carry no platform. On a multi-ABI build SavedMapPath (and a patch's BaseMapPath) hold
	// androidObfuscationPlatformToken, which the multi-ABI frontend expands per ABI.
	TargetPlatforms []string
}

func expandObfuscationPlatform(path, platform string) string {
	return strings.ReplaceAll(path, androidObfuscationPlatformToken, platform)
}

// savedMaps returns this build's map per ABI ("" for the single arm64 build).
func (p *androidObfuscationPlan) savedMaps() (map[string][]byte, error) {
	if len(p.TargetPlatforms) == 0 {
		raw, err := os.ReadFile(p.SavedMapPath)
		if err != nil {
			return nil, err
		}
		return map[string][]byte{"": raw}, nil
	}
	out := make(map[string][]byte, len(p.TargetPlatforms))
	for _, platform := range p.TargetPlatforms {
		raw, err := os.ReadFile(expandObfuscationPlatform(p.SavedMapPath, platform))
		if err != nil {
			return nil, fmt.Errorf("no obfuscation map for %s: %w", platform, err)
		}
		out[platform] = raw
	}
	return out, nil
}

// androidDefaultTargetPlatformsFn is what a build with no --target-platform compiles for this
// toolchain and the active frontend. A variable so tests need no installed frontend.
var androidDefaultTargetPlatformsFn = func(toolchainVersion string) ([]string, error) {
	dir, err := androidCachedToolchainBundleDir(strings.TrimSpace(toolchainVersion))
	if err != nil {
		return nil, err
	}
	flutterBin, err := resolveSoroqFlutterBin()
	if err != nil {
		return nil, err
	}
	return androidBuildablePlatforms(androidEngineSource{Kind: androidEngineSourceCachedToolchain, BundleDir: dir}, flutterBin)
}

// withObfuscationTargetPlatforms makes an obfuscated build's ABIs explicit, so the map plan and the
// build cannot disagree about them. It returns the args to validate, the platforms, and the
// --target-platform args to add (none when the caller named them).
func withObfuscationTargetPlatforms(toolchainVersion string, args []string) ([]string, []string, []string, error) {
	if platforms := explicitAndroidTargetPlatforms(args); len(platforms) > 0 {
		return args, platforms, nil, nil
	}
	platforms, err := androidDefaultTargetPlatformsFn(toolchainVersion)
	if err != nil {
		return nil, nil, nil, err
	}
	added := []string{"--target-platform", strings.Join(platforms, ",")}
	return append(append([]string{}, args...), added...), platforms, added, nil
}

// obfuscationMapArgs is the save-map half of the gen_snapshot options: one map.json for the single
// arm64 build (unchanged), or one map per ABI through the multi-ABI frontend's placeholder.
func (p *androidObfuscationPlan) obfuscationMapArgs(dir string, platforms []string) string {
	if len(platforms) == 1 && platforms[0] == androidObfuscationTargetPlatform {
		p.SavedMapPath = filepath.Join(dir, "map.json")
		return "--save-obfuscation-map=" + p.SavedMapPath
	}
	p.TargetPlatforms = append([]string{}, platforms...)
	p.SavedMapPath = filepath.Join(dir, "map."+androidObfuscationPlatformToken+".json")
	return "--save-obfuscation-map=" + p.SavedMapPath
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

// androidToolchainSupportsObfuscationSeedFor extends the check to every ABI the build compiles: each
// extra ABI's own gen_snapshot must support the seed too.
func androidToolchainSupportsObfuscationSeedFor(toolchainVersion string, platforms []string) error {
	if err := androidToolchainSupportsObfuscationSeed(toolchainVersion); err != nil {
		return err
	}
	dir, err := androidCachedToolchainBundleDir(strings.TrimSpace(toolchainVersion))
	if err != nil {
		return err
	}
	for _, platform := range platforms {
		info, ok := androidABIInfoForPlatform(platform)
		if !ok || info.ABI == androidPrimaryABI {
			continue
		}
		_, _, genSnapshot := androidToolchainABIArtifacts(info.ABI)
		help, err := androidGenSnapshotHelpFn(filepath.Join(dir, genSnapshot))
		if err != nil {
			return fmt.Errorf("android toolchain %q: cannot run the %s gen_snapshot: %w", toolchainVersion, info.ABI, err)
		}
		if !strings.Contains(help, "--load-obfuscation-map") {
			return fmt.Errorf("android toolchain %q: the %s gen_snapshot does not support --load-obfuscation-map", toolchainVersion, info.ABI)
		}
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
	// Each ABI's gen_snapshot writes its own map (through the multi-ABI frontend's per-platform
	// placeholder, unless the ABI is android-arm64 alone), and each ABI's patches are seeded from it.
	if len(platforms) == 0 {
		return fmt.Errorf("an obfuscated Soroq Android build must name its ABIs (pass --target-platform %s)", androidObfuscationTargetPlatform)
	}
	seen := map[string]bool{}
	for _, platform := range platforms {
		platform = strings.TrimSpace(platform)
		if _, ok := androidABIInfoForPlatform(platform); !ok || seen[platform] {
			return fmt.Errorf("an obfuscated Soroq Android build cannot target %q twice or an unsupported platform (supported: android-arm, android-arm64, android-x64)", platform)
		}
		seen[platform] = true
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
	effective, platforms, added, err := withObfuscationTargetPlatforms(toolchainVersion, args)
	if err != nil {
		return nil, err
	}
	if err := validateAndroidObfuscationArgs(effective); err != nil {
		return nil, err
	}
	if err := androidToolchainSupportsObfuscationSeedFor(toolchainVersion, platforms); err != nil {
		return nil, err
	}
	dir, err := newAndroidObfuscationScratch(projectDir)
	if err != nil {
		return nil, err
	}
	plan := &androidObfuscationPlan{Obfuscated: true, scratchDir: dir}
	plan.ExtraArgs = append(append([]string{}, added...), "--extra-gen-snapshot-options="+plan.obfuscationMapArgs(dir, platforms))
	return plan, nil
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

// commitAndroidReleaseObfuscation moves this build's map(s) into the release's state and records them.
func commitAndroidReleaseObfuscation(projectDir, releaseID, toolchainVersion string, plan *androidObfuscationPlan) (*androidObfuscationRecord, error) {
	if plan == nil || !plan.Obfuscated {
		return nil, nil
	}
	maps, err := plan.savedMaps()
	if err != nil {
		return nil, fmt.Errorf("the obfuscated build wrote no obfuscation map (%s): %w; refusing to register a base no patch could be seeded from", plan.SavedMapPath, err)
	}
	parsed := make(map[string]int, len(maps))
	for platform, raw := range maps {
		pairs, err := parseAndroidObfuscationMap(raw)
		if err != nil {
			if platform != "" {
				return nil, fmt.Errorf("%s: %w", platform, err)
			}
			return nil, err
		}
		parsed[platform] = len(pairs)
	}
	dir, err := androidReleaseObfuscationDir(projectDir, releaseID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	rec := &androidObfuscationRecord{
		Schema:           androidObfuscationRecordSchema,
		MapFile:          androidObfuscationMapFile,
		ToolchainVersion: strings.TrimSpace(toolchainVersion),
	}
	if len(plan.TargetPlatforms) == 0 {
		raw := maps[""]
		sum := sha256.Sum256(raw)
		rec.MapSHA256, rec.MapEntries = hex.EncodeToString(sum[:]), parsed[""]
		if err := writePrivateFileAtomic(filepath.Join(dir, androidObfuscationMapFile), raw); err != nil {
			return nil, err
		}
	} else {
		rec.Schema, rec.MapFile = androidObfuscationRecordSchemaV2, androidObfuscationMapPattern
		rec.Maps = make(map[string]androidObfuscationMapDigest, len(maps))
		for _, platform := range plan.TargetPlatforms {
			raw := maps[platform]
			sum := sha256.Sum256(raw)
			file := expandObfuscationPlatform(androidObfuscationMapPattern, platform)
			if err := writePrivateFileAtomic(filepath.Join(dir, file), raw); err != nil {
				return nil, err
			}
			rec.Maps[platform] = androidObfuscationMapDigest{File: file, SHA256: hex.EncodeToString(sum[:]), Entries: parsed[platform]}
			if platform == androidObfuscationTargetPlatform || rec.MapEntries == 0 {
				rec.MapSHA256, rec.MapEntries = hex.EncodeToString(sum[:]), parsed[platform]
			}
		}
	}
	recBytes, _ := json.MarshalIndent(rec, "", "  ")
	if err := writePrivateFileAtomic(filepath.Join(dir, androidObfuscationRecordFile), append(recBytes, '\n')); err != nil {
		return nil, err
	}
	return rec, nil
}

// loadAndroidReleaseObfuscation returns the release's record, or nil if the base was not obfuscated,
// and the base map path: a plain path for a v1 (arm64) base, or a path holding
// androidObfuscationPlatformToken for a v2 (multi-ABI) base. A record whose map is missing or does not
// hash to the recorded digest is an error, never "plain".
func loadAndroidReleaseObfuscation(projectDir, releaseID string) (*androidObfuscationRecord, string, error) {
	dir, err := androidReleaseObfuscationDir(projectDir, releaseID)
	if err != nil {
		return nil, "", err
	}
	recRaw, err := os.ReadFile(filepath.Join(dir, androidObfuscationRecordFile))
	mapPath := filepath.Join(dir, androidObfuscationMapFile)
	if errors.Is(err, os.ErrNotExist) {
		stray, _ := filepath.Glob(filepath.Join(dir, "android_base_obfuscation_map*.json"))
		if len(stray) > 0 {
			return nil, "", fmt.Errorf("release %s has an obfuscation map but no record of it; refusing to guess whether the base is obfuscated", releaseID)
		}
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var rec androidObfuscationRecord
	if err := json.Unmarshal(recRaw, &rec); err != nil ||
		(rec.Schema != androidObfuscationRecordSchema && rec.Schema != androidObfuscationRecordSchemaV2) {
		return nil, "", fmt.Errorf("release %s: unreadable obfuscation record", releaseID)
	}
	check := func(path, want string) error {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("release %s is obfuscated but its base obfuscation map is missing (%s): no patch can keep its names", releaseID, path)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != want {
			return fmt.Errorf("release %s: base obfuscation map %s does not match its recorded digest", releaseID, filepath.Base(path))
		}
		return nil
	}
	if rec.Schema == androidObfuscationRecordSchema {
		if err := check(mapPath, rec.MapSHA256); err != nil {
			return nil, "", err
		}
		return &rec, mapPath, nil
	}
	platforms := rec.platforms()
	if len(platforms) == 0 || len(platforms) != len(rec.Maps) {
		return nil, "", fmt.Errorf("release %s: per-ABI obfuscation record names no ABIs or unsupported ones", releaseID)
	}
	for _, platform := range platforms {
		entry := rec.Maps[platform]
		if entry.File != expandObfuscationPlatform(androidObfuscationMapPattern, platform) {
			return nil, "", fmt.Errorf("release %s: obfuscation record names an unexpected map file %q for %s", releaseID, entry.File, platform)
		}
		if err := check(filepath.Join(dir, entry.File), entry.SHA256); err != nil {
			return nil, "", err
		}
	}
	return &rec, filepath.Join(dir, androidObfuscationMapPattern), nil
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
	// The base's ABIs decide: each ABI is seeded from its own base map.
	basePlatforms := rec.platforms()
	platforms := explicitAndroidTargetPlatforms(args)
	if !sameStringSet(platforms, basePlatforms) {
		return nil, fmt.Errorf("release %s was obfuscated for %s, so the patch must build exactly those ABIs (got --target-platform %s)", releaseID, strings.Join(basePlatforms, ","), strings.Join(platforms, ","))
	}
	if err := androidToolchainSupportsObfuscationSeedFor(toolchainVersion, basePlatforms); err != nil {
		return nil, err
	}
	if strings.Contains(baseMap, ",") {
		return nil, fmt.Errorf("base map path %q contains a comma, which gen_snapshot's option list cannot carry", baseMap)
	}
	dir, err := newAndroidObfuscationScratch(projectDir)
	if err != nil {
		return nil, err
	}
	plan := &androidObfuscationPlan{Obfuscated: true, BaseMapPath: baseMap, scratchDir: dir}
	plan.ExtraArgs = []string{"--extra-gen-snapshot-options=--load-obfuscation-map=" + baseMap + "," + plan.obfuscationMapArgs(dir, basePlatforms)}
	return plan, nil
}

// androidSeedVerification summarises the candidate map check.
type androidSeedVerification struct {
	BaseEntries      int
	CandidateEntries int
	NewEntries       int
	// ABIs is how many ABIs were checked (0 = the single arm64 build); the counts are arm64's.
	ABIs int
}

// verifyAndroidSeededCandidate is the gate between an obfuscated patch build and publication: the
// candidate must keep every base pair exactly and must not map two identifiers to one name. A
// multi-ABI patch passes only if every ABI's candidate map passes against that ABI's base map.
func verifyAndroidSeededCandidate(plan *androidObfuscationPlan) (*androidSeedVerification, error) {
	if plan == nil || !plan.Obfuscated {
		return nil, nil
	}
	cands, err := plan.savedMaps()
	if err != nil {
		return nil, fmt.Errorf("the obfuscated patch build wrote no obfuscation map, so its names cannot be shown to match the base's: %w", err)
	}
	if len(plan.TargetPlatforms) == 0 {
		return verifyAndroidSeededMaps(plan.BaseMapPath, cands[""])
	}
	var primary *androidSeedVerification
	for _, platform := range plan.TargetPlatforms {
		v, err := verifyAndroidSeededMaps(expandObfuscationPlatform(plan.BaseMapPath, platform), cands[platform])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", platform, err)
		}
		if primary == nil || platform == androidObfuscationTargetPlatform {
			primary = v
		}
	}
	primary.ABIs = len(plan.TargetPlatforms)
	return primary, nil
}

func verifyAndroidSeededMaps(baseMapPath string, candRaw []byte) (*androidSeedVerification, error) {
	baseRaw, err := os.ReadFile(baseMapPath)
	if err != nil {
		return nil, err
	}
	base, err := parseAndroidObfuscationMap(baseRaw)
	if err != nil {
		return nil, fmt.Errorf("base map: %w", err)
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
