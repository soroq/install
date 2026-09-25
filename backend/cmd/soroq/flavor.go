package main

// Flutter build-flavor support.
//
// WHAT IS SUPPORTED
//
//   - `soroq release android` and `soroq patch android` accept `--flavor <name>` (or `--flavor=<name>`,
//     or the same flag after `--` as a Flutter passthrough). When Soroq builds, it passes `--flavor` to
//     `flutter build` and discovers the artifact ONLY in that flavor's output locations, so an
//     unflavored or other-flavor leftover can never be picked up.
//   - `pubspec.yaml` `flutter: default-flavor: <name>` is honoured exactly as Flutter honours it: a
//     build with no `--flavor` IS a build of the default flavor, so Soroq treats it as one.
//   - The flavor a release was built from is RECORDED (per-release sidecar under
//     .soroq/releases/<release-id>/, the CLI state, and the committed soroq.lock pin), and a patch whose
//     flavor differs from its base release's is REFUSED.
//   - `soroq release ios --build` (the plain app-build leg, which registers nothing) passes the flavor
//     through; Flutter maps it to the Xcode scheme and copies the product to build/ios/iphoneos/.
//
// WHAT IS STILL REFUSED, AND WHY
//
//   - The iOS ENGINE lanes (`release ios --engine --build`, `patch ios --engine`, `--platforms=ios`).
//     Their baselines are bound by a source-kernel recipe that does not yet reproduce the
//     FLUTTER_APP_FLAVOR define a flavored Flutter build injects, and the base-identity asset is written
//     into every .app under build/ios, which would stamp a leftover other-flavor bundle too. Neither is
//     verified on a device, so these routes fail closed.
//   - `release ios` without `--build` (config baseline): it builds nothing, and its runtime id does not
//     depend on the flavor, so a flavor there would be a label with no effect.
//
// THE SERVING LIMIT (why a runtime-id collision is refused)
//
// The control plane selects a patch by (app_id, runtime_id, channel). The runtime_id a device reports is
// derived by the Soroq Flutter frontend from soroq.yaml + the pubspec version, NOT from the flavor. Two
// flavors built from one project at one version therefore report the SAME runtime_id, and a patch
// registered against the prod release would be offered to dev devices. Soroq cannot change that from
// the CLI (the derivation lives in the frontend), so a flavored release or patch is refused whenever
// another release shares its runtime_id and is not provably the same flavor.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"soroq/backend/internal/domain"
)

// flavorNamePattern is deliberately narrower than what Flutter accepts: it is a single safe path
// segment (no separators, no dots, no spaces) and a valid Gradle product-flavor identifier.
var flavorNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

func validateFlavorName(name string) error {
	if !flavorNamePattern.MatchString(name) {
		return fmt.Errorf("invalid flavor %q: a flavor must start with a letter and contain only letters, digits or underscores (at most 64 characters)", name)
	}
	return nil
}

// extractPassthroughFlavor returns the flavor named by `--flavor X` / `--flavor=X` in a Flutter
// passthrough list, and the list with those tokens removed. Repeating the flag with different values,
// or leaving it without a value, is an error rather than a last-one-wins guess.
func extractPassthroughFlavor(args []string) (flavor string, found bool, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		var value string
		switch {
		case arg == "--flavor":
			if i+1 >= len(args) || strings.HasPrefix(strings.TrimSpace(args[i+1]), "-") {
				return "", false, nil, errors.New("--flavor requires a value")
			}
			i++
			value = strings.TrimSpace(args[i])
		case strings.HasPrefix(arg, "--flavor="):
			value = strings.TrimSpace(strings.TrimPrefix(arg, "--flavor="))
			if value == "" {
				return "", false, nil, errors.New("--flavor requires a value")
			}
		default:
			rest = append(rest, args[i])
			continue
		}
		if found && value != flavor {
			return "", false, nil, fmt.Errorf("--flavor given twice with different values (%q and %q)", flavor, value)
		}
		flavor, found = value, true
	}
	return flavor, found, rest, nil
}

// pubspecDefaultFlavor reads `flutter: default-flavor:` from pubspec.yaml — the value Flutter applies
// when a build names no flavor (flutter_manifest.dart defaultFlavor). A missing file or key is "".
func pubspecDefaultFlavor(projectDir string) string {
	data, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		return ""
	}
	inFlutter := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		raw := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		if indent == 0 {
			inFlutter = trimmed == "flutter:" || strings.HasPrefix(trimmed, "flutter: #")
			continue
		}
		if !inFlutter {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok || strings.TrimSpace(key) != "default-flavor" {
			continue
		}
		value = strings.TrimSpace(value)
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		return strings.Trim(value, `"'`)
	}
	return ""
}

// resolvedFlavor is the single answer to "which flavor is this command building/registering?".
type resolvedFlavor struct {
	Name   string // "" = unflavored
	Source string // "flag", "passthrough", "pubspec default-flavor", or ""
}

// resolveCommandFlavor reconciles the soroq `--flavor` flag, a passthrough `--flavor`, and the pubspec
// default-flavor. It returns the passthrough with any `--flavor` removed; buildArgsWithFlavor puts the
// one resolved value back so Flutter is always told explicitly.
func resolveCommandFlavor(projectDir, flagValue string, passthrough []string) (resolvedFlavor, []string, error) {
	fromPass, passFound, rest, err := extractPassthroughFlavor(passthrough)
	if err != nil {
		return resolvedFlavor{}, nil, err
	}
	flagValue = strings.TrimSpace(flagValue)
	var out resolvedFlavor
	switch {
	case flagValue != "" && passFound && flagValue != fromPass:
		return resolvedFlavor{}, nil, fmt.Errorf("--flavor %q conflicts with the passthrough --flavor %q; pass it once", flagValue, fromPass)
	case flagValue != "":
		out = resolvedFlavor{Name: flagValue, Source: "flag"}
	case passFound:
		out = resolvedFlavor{Name: fromPass, Source: "passthrough"}
	default:
		if d := pubspecDefaultFlavor(projectDir); d != "" {
			out = resolvedFlavor{Name: d, Source: "pubspec default-flavor"}
		}
	}
	if out.Name != "" {
		if err := validateFlavorName(out.Name); err != nil {
			return resolvedFlavor{}, nil, err
		}
	}
	return out, rest, nil
}

// buildArgsWithFlavor appends the resolved flavor to Flutter build args. Unflavored => args unchanged.
func buildArgsWithFlavor(args []string, flavor string) []string {
	out := append([]string(nil), args...)
	if flavor == "" {
		return out
	}
	return append(out, "--flavor", flavor)
}

// ---------------------------------------------------------------------------------------------------
// Android output locations.
//
// Taken from the Soroq Flutter frontend's own resolver (packages/flutter_tools/lib/src/android/
// gradle.dart): flutter-apk/app-<lowercased flavor>-release.apk (and split-per-abi
// app-<abi>-<flavor>-release.apk), the Gradle apk/<flavor>/release/ directory, and the bundle
// directory whose lowercased name is <lowercased flavor>release. Directory matching is
// case-insensitive, as Flutter's is, because AGP keeps the flavor's declared casing.

func flavoredAndroidArtifactCandidates(projectDir, flavor string) ([]string, error) {
	outputs := filepath.Join(projectDir, "build", "app", "outputs")
	lower := strings.ToLower(flavor)
	var paths []string
	add := func(pattern string) error {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		paths = append(paths, matches...)
		return nil
	}
	// flutter-apk: the copy Flutter itself reports.
	if err := add(filepath.Join(outputs, "flutter-apk", "app-"+lower+"-release.apk")); err != nil {
		return nil, err
	}
	if err := add(filepath.Join(outputs, "flutter-apk", "app-*-"+lower+"-release.apk")); err != nil {
		return nil, err
	}
	// Gradle apk/<flavor>/release/ and bundle/<flavor>Release/, matched case-insensitively.
	for _, sub := range []struct {
		dir  string
		want func(name string) bool
		ext  string
		tail []string
	}{
		{"apk", func(n string) bool { return strings.ToLower(n) == lower }, "*.apk", []string{"release"}},
		{"bundle", func(n string) bool { return strings.ToLower(n) == lower+"release" }, "*.aab", nil},
	} {
		entries, err := os.ReadDir(filepath.Join(outputs, sub.dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !sub.want(e.Name()) {
				continue
			}
			parts := append([]string{outputs, sub.dir, e.Name()}, sub.tail...)
			parts = append(parts, sub.ext)
			if err := add(filepath.Join(parts...)); err != nil {
				return nil, err
			}
		}
	}
	return paths, nil
}

// discoverAndroidArtifactsForFlavor returns the build outputs for ONE flavor (newest first), or the
// unflavored set when flavor is "". A flavored lookup never returns an unflavored or other-flavor file,
// and never looks in release-candidates/, which carries no flavor.
func discoverAndroidArtifactsForFlavor(projectDir, flavor string) ([]discoveredArtifact, error) {
	if flavor == "" {
		return discoverAndroidArtifacts(projectDir)
	}
	paths, err := flavoredAndroidArtifactCandidates(projectDir, flavor)
	if err != nil {
		return nil, err
	}
	return sortDiscoveredArtifacts(paths), nil
}

func discoverDefaultAndroidArtifactForFlavor(projectDir, flavor string) (string, error) {
	artifacts, err := discoverAndroidArtifactsForFlavor(projectDir, flavor)
	if err != nil {
		return "", err
	}
	if len(artifacts) == 0 {
		return "", os.ErrNotExist
	}
	return artifacts[0].Path, nil
}

// flavorFromAndroidArtifactPath recognises a path Flutter/Gradle writes for a flavored build. It is
// only ever used to CATCH a contradiction or warn — never to silently adopt a flavor.
func flavorFromAndroidArtifactPath(path string) string {
	clean := "/" + strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "/")
	const marker = "/build/app/outputs/"
	idx := strings.LastIndex(clean, marker)
	if idx < 0 {
		return ""
	}
	rel := clean[idx+len(marker):]
	parts := strings.Split(rel, "/")
	switch {
	case len(parts) == 4 && parts[0] == "apk" && parts[2] == "release" && parts[1] != "release":
		return parts[1]
	case len(parts) == 3 && parts[0] == "bundle" && strings.HasSuffix(parts[1], "Release") && parts[1] != "Release":
		return strings.TrimSuffix(parts[1], "Release")
	case len(parts) == 2 && parts[0] == "flutter-apk":
		name := strings.TrimSuffix(parts[1], ".apk")
		rest, ok := strings.CutPrefix(name, "app-")
		if !ok {
			return ""
		}
		mid, ok := strings.CutSuffix(rest, "-release")
		if !ok || mid == "" {
			return ""
		}
		segs := strings.Split(mid, "-")
		last := segs[len(segs)-1]
		if isAndroidABIName(strings.Join(segs, "-")) || isAndroidABIName(last) {
			return "" // split-per-abi unflavored (app-arm64-v8a-release.apk)
		}
		return last
	}
	return ""
}

func isAndroidABIName(s string) bool {
	switch s {
	case "arm64-v8a", "armeabi-v7a", "x86_64", "x86", "v8a", "v7a":
		return true
	}
	return false
}

func sortDiscoveredArtifacts(paths []string) []discoveredArtifact {
	byPath := map[string]discoveredArtifact{}
	for _, p := range paths {
		clean := filepath.Clean(p)
		info, err := os.Stat(clean)
		if err != nil || info.IsDir() {
			continue
		}
		byPath[clean] = discoveredArtifact{Path: clean, ModTime: info.ModTime(), Size: info.Size()}
	}
	out := make([]discoveredArtifact, 0, len(byPath))
	for _, a := range byPath {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModTime.Equal(out[j].ModTime) {
			if filepath.Ext(out[i].Path) != filepath.Ext(out[j].Path) {
				return filepath.Ext(out[i].Path) == ".aab"
			}
			return out[i].Path < out[j].Path
		}
		return out[i].ModTime.After(out[j].ModTime)
	})
	return out
}

// ---------------------------------------------------------------------------------------------------
// The persisted flavor record.

const releaseFlavorRecordFile = "soroq-flavor.json"
const releaseFlavorRecordSchema = "soroq.release_flavor.v1"

// releaseFlavorRecord is written beside the stashed release artifact in .soroq/releases/<release-id>/.
// Its presence is what makes a release's flavor KNOWN; "flavor": "" then means "known unflavored".
type releaseFlavorRecord struct {
	Schema     string    `json:"schema"`
	Platform   string    `json:"platform"`
	ReleaseID  string    `json:"release_id"`
	RuntimeID  string    `json:"runtime_id"`
	Flavor     string    `json:"flavor"`
	RecordedAt time.Time `json:"recorded_at"`
}

func releaseFlavorRecordPath(projectDir, releaseID string) string {
	return filepath.Join(projectDir, ".soroq", "releases", slugifyReleaseID(releaseID), releaseFlavorRecordFile)
}

func writeReleaseFlavorRecord(projectDir string, rec releaseFlavorRecord) error {
	rec.Schema = releaseFlavorRecordSchema
	if rec.RecordedAt.IsZero() {
		rec.RecordedAt = time.Now().UTC()
	}
	path := releaseFlavorRecordPath(projectDir, rec.ReleaseID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readReleaseFlavorRecord returns (record, true, nil) when a valid record exists for releaseID. A
// record that exists but is malformed or names another release is an ERROR, not "unknown": a corrupt
// identity record must not quietly downgrade to the permissive path.
func readReleaseFlavorRecord(projectDir, releaseID string) (releaseFlavorRecord, bool, error) {
	path := releaseFlavorRecordPath(projectDir, releaseID)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return releaseFlavorRecord{}, false, nil
	}
	if err != nil {
		return releaseFlavorRecord{}, false, err
	}
	var rec releaseFlavorRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return releaseFlavorRecord{}, false, fmt.Errorf("release flavor record %s is malformed: %w", path, err)
	}
	if rec.Schema != releaseFlavorRecordSchema || rec.ReleaseID != releaseID {
		return releaseFlavorRecord{}, false, fmt.Errorf("release flavor record %s does not describe release %q (schema %q, release %q)", path, releaseID, rec.Schema, rec.ReleaseID)
	}
	if rec.Flavor != "" {
		if err := validateFlavorName(rec.Flavor); err != nil {
			return releaseFlavorRecord{}, false, fmt.Errorf("release flavor record %s: %w", path, err)
		}
	}
	return rec, true, nil
}

// knownReleaseFlavor resolves a release's recorded flavor from, in order: the per-release record, the
// CLI state, and the committed soroq.lock pin. known=false means no source recorded it (a release made
// before flavor support, or on another machine without a soroq.lock flavor line).
func knownReleaseFlavor(projectDir, platform, releaseID string) (flavor string, known bool, err error) {
	releaseID = strings.TrimSpace(releaseID)
	if releaseID == "" {
		return "", false, nil
	}
	rec, ok, err := readReleaseFlavorRecord(projectDir, releaseID)
	if err != nil {
		return "", false, err
	}
	if ok {
		return rec.Flavor, true, nil
	}
	if platform == "android" {
		if state, err := loadProjectCLIState(projectDir); err == nil && state.LastAndroidRelease != nil &&
			state.LastAndroidRelease.ReleaseID == releaseID && state.LastAndroidRelease.Flavor != nil {
			return *state.LastAndroidRelease.Flavor, true, nil
		}
	}
	if lock, err := loadSoroqLock(projectDir); err == nil {
		if pin, ok := lock.Platforms[platform]; ok && strings.TrimSpace(pin.ReleaseID) == releaseID && pin.Flavor != "" {
			return pin.Flavor, true, nil
		}
		// The plain pin names only the LATEST release; an older flavor's release is pinned under
		// <platform>@<flavor>. .soroq/ is not committed, so on a fresh clone this is the only record.
		for key, pin := range lock.Platforms {
			if isSoroqLockFlavorKeyOf(key, platform) && strings.TrimSpace(pin.ReleaseID) == releaseID && pin.Flavor != "" {
				return pin.Flavor, true, nil
			}
		}
	}
	return "", false, nil
}

func describeFlavor(f string) string {
	if f == "" {
		return "no flavor"
	}
	return fmt.Sprintf("flavor %q", f)
}

// guardPatchFlavorMatchesBase is the rule a patch must never break: a patch built from one flavor is
// never offered to a release built from another.
//
//   - base flavor known, differs from the patch's            -> refuse
//   - base flavor unknown, patch flavored                    -> refuse (cannot prove the match)
//   - base flavor unknown, patch unflavored                  -> allow  (pre-flavor behaviour unchanged)
func guardPatchFlavorMatchesBase(releaseID, baseFlavor string, baseKnown bool, patchFlavor string) error {
	if baseKnown {
		if baseFlavor == patchFlavor {
			return nil
		}
		return fmt.Errorf(`refusing to patch: flavor mismatch

  base release %s was built with %s
  this patch is built with       %s

A patch compiled from one flavor carries that flavor's code, resources and configuration. Offering it
to devices running another flavor would ship the wrong app to them. Build the patch with the base
release's flavor%s, or target a release of this flavor.`,
			releaseID, describeFlavor(baseFlavor), describeFlavor(patchFlavor), flavorFlagHint(baseFlavor))
	}
	if patchFlavor == "" {
		return nil
	}
	return fmt.Errorf(`refusing to patch with --flavor %s: the flavor of base release %s is not recorded

Soroq records a release's flavor when it registers it (under .soroq/releases/<release-id>/ and in
soroq.lock). This release has no such record — it was registered before flavor support, or on another
machine — so Soroq cannot prove the patch and the base are the same flavor.

If this release really is the %q flavor, re-register the SAME artifact with its flavor declared (the
release is idempotent for identical bytes, and this only adds the record):

    soroq release android --build=false --artifact <base artifact> --release-id %s --flavor %s`,
		patchFlavor, releaseID, patchFlavor, releaseID, patchFlavor)
}

func flavorFlagHint(f string) string {
	if f == "" {
		return " (no --flavor, and no pubspec default-flavor)"
	}
	return " (--flavor " + f + ")"
}

// guardFlavorRuntimeCollision refuses when a flavored release (or a release sharing a runtime_id with a
// flavored one) would be indistinguishable to the control plane. See THE SERVING LIMIT above.
//
// A colliding release is tolerated only when it is provably the SAME flavor — e.g. the arm64 and armv7
// releases of one flavor, which legitimately share a runtime_id and are separated by architecture.
func guardFlavorRuntimeCollision(projectDir, platform, ownReleaseID, runtimeID, flavor string, releases []domain.Release) error {
	var conflicts []string
	for _, r := range releases {
		if r.ID == ownReleaseID || r.RuntimeID != runtimeID || r.Platform != platform {
			continue
		}
		other, known, err := knownReleaseFlavor(projectDir, platform, r.ID)
		if err != nil {
			return err
		}
		if known && other == flavor {
			continue
		}
		if !known && flavor == "" {
			// Neither side is known to be flavored: the pre-flavor behaviour, unchanged.
			continue
		}
		desc := "unrecorded flavor"
		if known {
			desc = describeFlavor(other)
		}
		conflicts = append(conflicts, fmt.Sprintf("%s (%s, arch %s)", r.ID, desc, r.Arch))
	}
	if len(conflicts) == 0 {
		return nil
	}
	sort.Strings(conflicts)
	return fmt.Errorf(`refusing: %s would share runtime_id %s with %s

The control plane chooses a patch by app id, channel and runtime_id, and a device's runtime_id is
derived from soroq.yaml and the pubspec version — not from the build flavor. Releases of different
flavors at the same version therefore look identical to it, and a patch for one would be offered to
devices running the other.

Soroq cannot tell these releases apart, so it will not register or patch across them. Ship only one
Soroq-enabled flavor per app id / channel / version, or give the flavors distinct runtime identities
(a separate app_id or channel) before using Soroq with more than one of them.`,
		describeFlavor(flavor), runtimeID, strings.Join(conflicts, ", "))
}

// ---------------------------------------------------------------------------------------------------
// Routes that remain refused.

// guardUnsupportedFlavorRoute refuses a flavor on a route that does not support it yet. Unlike the old
// blanket guard, SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS does NOT bypass it: on the iOS engine lanes a
// flavored build proceeding "unverified" would persist a baseline whose recipe records no flavor — a
// reusable, wrong baseline — which is exactly what the obfuscation guard refuses to allow as well.
func guardUnsupportedFlavorRoute(route, why string, flavor string) error {
	if flavor == "" {
		return nil
	}
	return fmt.Errorf(`refusing --flavor %s on %s: flavored builds are not supported on this route

%s

Supported flavor routes: `+"`soroq release android`"+`, `+"`soroq patch android`"+`, the plain iOS app
build `+"`soroq release ios --build`"+`, and the freehand iOS engine lane with a flavor declared in
soroq.yaml `+"`flavors:`"+`. %s does not override this refusal.`,
		flavor, route, why, unverifiedBuildFlagsOptInEnv)
}

const iosEngineFlavorRefusalReason = `The iOS engine lane supports flavors only on the freehand lane and only for a flavor declared in
soroq.yaml ` + "`flavors:`" + ` with its own channel (ios_engine_flavor.go): the source-kernel recipe binds the
flavor and FLUTTER_APP_FLAVOR, the identity asset goes only into that flavor's bundles, and the
channel keeps each flavor's runtime identity apart.`

// flavorFromRouteArgs resolves the flavor a manually-parsed route (iOS engine lanes, --platforms) asks for, from the head
// flags (`--flavor`), the passthrough, or the pubspec default-flavor, without mutating anything.
func flavorFromRouteArgs(projectDir string, head, passthrough []string) (string, error) {
	flagVal, _ := flagValue(head, "flavor")
	if hasFlag(head, "flavor") && strings.TrimSpace(flagVal) == "" {
		return "", errors.New("--flavor requires a value")
	}
	rf, _, err := resolveCommandFlavor(projectDir, flagVal, passthrough)
	if err != nil {
		return "", err
	}
	return rf.Name, nil
}

// ---------------------------------------------------------------------------------------------------
// Android command helpers.

// defaultAndroidReleaseID qualifies the default id by flavor so two flavors of one version never claim
// the same release id. Unflavored ids are exactly what they were before flavor support.
func defaultAndroidReleaseID(appID, version, arch, flavor string) string {
	if flavor == "" {
		return defaultReleaseID(appID, version, arch)
	}
	return defaultReleaseID(appID, version, strings.ToLower(flavor)+"-"+arch)
}

// checkExplicitArtifactFlavor catches an explicitly named artifact whose Flutter output path says it is
// a different flavor than the command declares. It never ADOPTS a flavor from a path: an undeclared
// flavored path only produces a warning, and the release is recorded as unflavored (so a later
// flavored patch is refused rather than silently accepted).
func checkExplicitArtifactFlavor(path, flavor, flagName string) error {
	inferred := flavorFromAndroidArtifactPath(path)
	if inferred == "" {
		return nil
	}
	if flavor == "" {
		fmt.Fprintf(os.Stderr, "warning: %s %s is in a flavored Flutter output location (flavor %q), but no --flavor was given; it will be recorded as unflavored. Pass --flavor %s to record it.\n", flagName, path, inferred, inferred)
		return nil
	}
	if !strings.EqualFold(inferred, flavor) {
		return fmt.Errorf("%s %s is a Flutter output of flavor %q, but this command is for %s", flagName, path, inferred, describeFlavor(flavor))
	}
	return nil
}

// anyFlavoredReleaseRecorded reports whether this project has recorded any flavored release. An
// unflavored command only needs the runtime-id collision check when one exists, which keeps a project
// that never used flavors on exactly its pre-flavor code path (no extra control-plane request).
func anyFlavoredReleaseRecorded(projectDir string) bool {
	matches, _ := filepath.Glob(filepath.Join(projectDir, ".soroq", "releases", "*", releaseFlavorRecordFile))
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var rec releaseFlavorRecord
		if json.Unmarshal(b, &rec) == nil && rec.Flavor != "" {
			return true
		}
	}
	if lock, err := loadSoroqLock(projectDir); err == nil {
		for _, pin := range lock.Platforms {
			if pin.Flavor != "" {
				return true
			}
		}
	}
	return false
}

// listAndroidReleasesFn is indirected for tests.
var listAndroidReleasesFn = func(apiBase, appID string) ([]domain.Release, error) {
	return getJSONDecode[[]domain.Release](strings.TrimRight(apiBase, "/") + "/v1/releases?app_id=" + url.QueryEscape(appID))
}

// guardAndroidReleaseFlavorCollision applies guardFlavorRuntimeCollision against the control plane's
// release list. A flavored command that cannot read the list fails closed; an unflavored command in a
// project with no flavored release does not look at all (behaviour unchanged).
func guardAndroidReleaseFlavorCollision(projectDir, apiBase, appID, ownReleaseID, runtimeID, flavor string) error {
	if flavor == "" && !anyFlavoredReleaseRecorded(projectDir) {
		return nil
	}
	releases, err := listAndroidReleasesFn(apiBase, appID)
	if err != nil {
		return fmt.Errorf("cannot verify that %s does not share runtime_id %s with another flavor's release (listing releases failed): %w", describeFlavor(flavor), runtimeID, err)
	}
	return guardFlavorRuntimeCollision(projectDir, "android", ownReleaseID, runtimeID, flavor, releases)
}

// flavorFromPlatformArgs resolves the flavor of a `--platforms=` invocation's remaining args (soroq
// `--flavor` before `--`, a passthrough `--flavor` after it, or the pubspec default-flavor).
func flavorFromPlatformArgs(projectDir string, args []string) (string, error) {
	head, passthrough := splitFlutterPassthrough(args)
	return flavorFromRouteArgs(projectDir, head, passthrough)
}

// refuseFlavorOnUnsupportedPlatforms refuses a flavored `--platforms=` command that includes a platform
// whose lane does not support flavors, BEFORE any lane runs — so `--platforms=android,ios --flavor prod`
// cannot register the Android release and then fail on iOS.
func refuseFlavorOnUnsupportedPlatforms(verb string, platforms []string, rest []string) error {
	projectDir := releaseProjectDir(rest)
	flavor, err := flavorFromPlatformArgs(projectDir, rest)
	if err != nil || flavor == "" {
		return err
	}
	for _, p := range platforms {
		if p == "ios" {
			// The iOS engine lane's own rule (freehand + a declared per-flavor channel), applied here,
			// before any lane runs.
			head, passthrough := splitFlutterPassthrough(rest)
			route := fmt.Sprintf("`soroq %s --platforms=%s` (the iOS engine lane)", verb, strings.Join(platforms, ","))
			if _, err := resolveIOSEngineFlavorRoute(route, projectDir, head, passthrough); err != nil {
				return fmt.Errorf("%w\n\nNothing has been built or registered for any platform.", err)
			}
		}
	}
	return nil
}
