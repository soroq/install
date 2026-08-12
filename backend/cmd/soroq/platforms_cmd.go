package main

// CANONICAL MULTI-PLATFORM UX — `soroq release --platforms=android,ios` and
// `soroq patch --platforms=android,ios --rollout 100`.
//
// The zero-touch workflow is meant to be three commands:
//
//	soroq init
//	soroq release --platforms=android,ios
//	# ...edit ordinary Flutter/Dart code...
//	soroq patch --platforms=android,ios --rollout 100
//
// Before this, reaching the iOS hard-OTA lane meant knowing to type `patch ios --engine`, which is an
// internal routing detail: "engine" names the delivery mechanism, not anything the developer chose.
// Selecting the iOS platform IS the choice, so `--platforms=ios` selects the hard-OTA path itself.
//
// Two properties this must not get wrong:
//
//   - BACKWARD COMPATIBILITY. `soroq release ios`, `soroq patch android`, `patch ios --engine` and
//     `patch ios-engine` all keep working unchanged; this is a new entry point, not a replacement. It
//     dispatches to exactly the same functions, so there is no second implementation to drift.
//   - PER-PLATFORM HONESTY. Running two platforms means two independent outcomes. A run where Android
//     succeeded and iOS failed is NOT a success, and it is also not a plain failure -- reporting it as
//     either loses information the developer needs. Every platform's result is reported separately and
//     the exit status is non-zero if ANY platform failed.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// knownPlatforms is the closed set. Anything else is a typo, and a typo must fail loudly rather than
// silently doing less than the developer asked for.
var knownPlatforms = map[string]bool{"android": true, "ios": true}

// parsePlatforms extracts and validates `--platforms=a,b` / `--platforms a,b`, returning the requested
// platforms in a stable order plus the remaining args with the flag removed.
func parsePlatforms(args []string) (platforms []string, rest []string, err error) {
	var raw string
	found := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--platforms="):
			raw, found = strings.TrimPrefix(a, "--platforms="), true
		case a == "--platforms":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--platforms requires a value, e.g. --platforms=android,ios")
			}
			raw, found = args[i+1], true
			i++
		default:
			rest = append(rest, a)
			continue
		}
	}
	if !found {
		return nil, args, nil
	}

	seen := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if !knownPlatforms[p] {
			return nil, nil, fmt.Errorf("unknown platform %q in --platforms; supported: android, ios", p)
		}
		if seen[p] {
			continue // a repeated platform is harmless; run it once
		}
		seen[p] = true
		platforms = append(platforms, p)
	}
	if len(platforms) == 0 {
		return nil, nil, fmt.Errorf("--platforms was given no platforms; expected e.g. --platforms=android,ios")
	}
	// Deterministic order, so a two-platform run always reports in the same sequence.
	sort.Strings(platforms)
	return platforms, rest, nil
}

// platformScopedFlags are the flags whose value identifies ONE platform's artifacts. A single value for
// two platforms is not a preference the CLI can honour — it is a contradiction:
//
//   - --release-id names one control-plane record, and a release record stores exactly one platform. Two
//     platforms sharing an id is the collision deriveReleaseIDForPlatform exists to prevent; accepting it
//     from the developer would reintroduce it by hand.
//   - --toolchain names one installed toolchain, and toolchains are per-platform (an iOS toolchain cannot
//     build an Android app bundle).
//
// Each entry carries the per-platform override that IS unambiguous, so the refusal names a real
// alternative instead of only saying no.
var platformScopedFlags = []string{"release-id", "toolchain"}

// platformScopedOverride is the per-platform form of a scoped flag, e.g. --release-id-ios.
func platformScopedOverride(flag, platform string) string {
	return flag + "-" + platform
}

// validateCombinedPlatformFlags refuses a multi-platform run whose flags cannot describe both platforms.
//
// It runs BEFORE any lane is invoked, so a rejected command has zero side effects: nothing is built, no
// pubspec is touched, no control-plane record is created, and the platform that would have gone first
// is not left half-registered for the other to trip over.
func validateCombinedPlatformFlags(verb string, platforms []string, args []string) error {
	if len(platforms) < 2 {
		return nil // a single platform is unambiguous by construction
	}
	for _, flag := range platformScopedFlags {
		if _, ok := flagValue(args, flag); !ok {
			continue
		}
		var overrides []string
		for _, p := range platforms {
			overrides = append(overrides, "--"+platformScopedOverride(flag, p)+" <value>")
		}
		return fmt.Errorf(
			"--%s applies to ONE platform, but this command targets %s.\n"+
				"  A %s identifies a single platform's artifacts, so one value cannot describe both — and\n"+
				"  reusing it across platforms is exactly the collision the derived per-platform ids prevent.\n"+
				"  Either drop --%s and let `soroq %s --platforms=%s` derive one per platform, or pass the\n"+
				"  per-platform overrides: %s.\n"+
				"  Nothing has been built or registered.",
			flag, strings.Join(platforms, " and "), flag, flag, verb,
			strings.Join(platforms, ","), strings.Join(overrides, " "))
	}
	return nil
}

// applyPlatformScopedOverrides rewrites `--release-id-ios v` into `--release-id v` for THIS platform and
// strips every other platform's overrides, so a lane only ever sees flags meant for it.
func applyPlatformScopedOverrides(platform string, args []string) []string {
	out := append([]string(nil), args...)
	for _, flag := range platformScopedFlags {
		for _, p := range sortedKnownPlatforms() {
			scoped := platformScopedOverride(flag, p)
			value, ok := flagValue(out, scoped)
			out = removeFlag(out, scoped)
			if !ok || p != platform {
				continue
			}
			// An explicit per-platform override wins over derivation, and over a value already present.
			out = removeFlag(out, flag)
			out = append(out, "--"+flag, value)
		}
	}
	return out
}

func sortedKnownPlatforms() []string {
	out := make([]string, 0, len(knownPlatforms))
	for p := range knownPlatforms {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// removeFlag drops `--name value` / `--name=value` from args, leaving everything else untouched.
func removeFlag(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--"+name || a == "-"+name:
			i++ // skip its value
		case strings.HasPrefix(a, "--"+name+"=") || strings.HasPrefix(a, "-"+name+"="):
		default:
			out = append(out, a)
		}
	}
	return out
}

// platformResult is one platform's independent outcome.
type platformResult struct {
	Platform string
	Err      error
}

// runPerPlatform executes fn for each platform and reports each outcome separately.
//
// It deliberately does NOT stop at the first failure. If a developer asks for android+ios and Android
// succeeds, they should get their Android patch; hiding it because iOS failed helps nobody. What must
// never happen is the combined command REPORTING success when a platform failed.
func runPerPlatform(verb string, platforms []string, fn func(platform string) error) error {
	results := make([]platformResult, 0, len(platforms))
	for _, p := range platforms {
		if len(platforms) > 1 {
			fmt.Fprintf(os.Stdout, "\n=== soroq %s: %s ===\n", verb, p)
		}
		results = append(results, platformResult{Platform: p, Err: fn(p)})
	}

	var failed []string
	if len(platforms) > 1 {
		fmt.Fprintf(os.Stdout, "\n=== soroq %s summary ===\n", verb)
	}
	for _, r := range results {
		if r.Err != nil {
			failed = append(failed, r.Platform)
			if len(platforms) > 1 {
				fmt.Fprintf(os.Stdout, "  %-8s FAILED  %v\n", r.Platform, r.Err)
			}
			continue
		}
		if len(platforms) > 1 {
			fmt.Fprintf(os.Stdout, "  %-8s ok\n", r.Platform)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	// Name the platforms that succeeded too. "ios failed" without "android succeeded" invites a
	// developer to re-run everything and re-publish an Android patch that already shipped.
	var ok []string
	for _, r := range results {
		if r.Err == nil {
			ok = append(ok, r.Platform)
		}
	}
	// Carry the underlying causes. Summarising N outcomes must not DISCARD them: a bare
	// "release failed for ios" tells a developer nothing they can act on, and for a single platform it
	// is strictly worse than the error the lane already produced.
	var causes []string
	for _, r := range results {
		if r.Err != nil {
			causes = append(causes, fmt.Sprintf("%s: %v", r.Platform, r.Err))
		}
	}
	if len(ok) > 0 {
		return fmt.Errorf("%s failed for %s (succeeded for %s); the combined command did NOT succeed\n  %s",
			verb, strings.Join(failed, ", "), strings.Join(ok, ", "), strings.Join(causes, "\n  "))
	}
	if len(results) == 1 {
		// Single platform: behave exactly like the underlying command.
		return results[0].Err
	}
	return fmt.Errorf("%s failed for %s\n  %s", verb, strings.Join(failed, ", "), strings.Join(causes, "\n  "))
}

// releasePlatform runs the release lane for one platform. iOS selects the hard-OTA (engine/freehand)
// lane, which is what choosing iOS means -- the developer never types --engine.
func releasePlatform(platform string, rest []string) error {
	rest = applyPlatformScopedOverrides(platform, rest)
	rest, err := withDerivedFlags(platform, releaseProjectDir(rest), rest)
	if err != nil {
		return err
	}
	switch platform {
	case "android":
		return runReleaseAndroid(rest)
	case "ios":
		// The UNIFIED fresh-dev path: generate the scaffold, build app.dill and register the
		// baseline in one command. This is what `release ios --engine --build` does, and it is
		// what choosing the iOS platform is asking for.
		return runReleaseIOSEngineBuild(rest)
	}
	return fmt.Errorf("unsupported platform %q", platform)
}

// patchPlatform runs the patch lane for one platform, with the same iOS routing rule.
func patchPlatform(platform string, rest []string) error {
	rest = applyPlatformScopedOverrides(platform, rest)
	rest, err := withDerivedFlags(platform, releaseProjectDir(rest), rest)
	if err != nil {
		return err
	}
	switch platform {
	case "android":
		return runPatchAndroid(rest)
	case "ios":
		return runPatchIOSEngineScaffolded(rest)
	}
	return fmt.Errorf("unsupported platform %q", platform)
}

// ---------------------------------------------------------------------------------------------------
// ZERO-TOUCH DERIVATION
//
// The canonical contract is `soroq init` then `soroq release --platforms=…`, so the canonical path must
// not ask the developer for values the CLI can determine. Everything below is derived ONLY for the
// --platforms path; the platform-specific subcommands keep their existing explicit flags.
//
// What is derived, and from where:
//
//	--toolchain    the installed toolchain the ACTIVE frontend declares itself compatible with
//	--release-id   app id + app version, which is what a store release is actually identified by
//	--api          already defaulted by defaultAPIBase()
//
// Deriving is not guessing: each value has exactly one correct source, and when that source is missing
// the command says which single setup action is required rather than surfacing an internal flag.

// installedToolchains lists toolchain versions under ~/.soroq/toolchains.
func installedToolchains() []string {
	root, err := toolchainsRoot()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "soroq-") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// deriveToolchain picks the toolchain for a platform.
//
// The ACTIVE frontend's manifest lists the toolchains it was built to work with, so the intersection of
// that list with what is installed is the answer — not "the newest directory", which would happily pair
// a frontend with a toolchain it was never verified against.
func deriveToolchain(platform string) (string, error) {
	installed := installedToolchains()
	prefix := "soroq-" + platform + "-"

	var candidates []string
	for _, t := range installed {
		if strings.HasPrefix(t, prefix) {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no %s toolchain is installed.\n"+
			"  Run this once:  soroq toolchain install <version> --api %s\n"+
			"  (`soroq toolchain list --api %s` shows what is available)",
			platform, defaultAPIBase(), defaultAPIBase())
	}

	// Prefer one the active frontend declares compatibility with.
	if compat := activeFrontendCompatibleToolchains(); len(compat) > 0 {
		allowed := map[string]bool{}
		for _, c := range compat {
			allowed[c] = true
		}
		var matched []string
		for _, c := range candidates {
			if allowed[c] {
				matched = append(matched, c)
			}
		}
		if len(matched) > 0 {
			return matched[len(matched)-1], nil
		}
	}
	// No compatibility list (an unsigned candidate may omit it): use the single installed toolchain, or
	// refuse to choose between several rather than pairing arbitrarily.
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return "", fmt.Errorf("several %s toolchains are installed and the active frontend declares no "+
		"compatibility list, so the correct pairing cannot be derived: %s\n"+
		"  Pass one explicitly with --toolchain <version>",
		platform, strings.Join(candidates, ", "))
}

// activeFrontendCompatibleToolchains reads compatible_toolchain_ids from the ACTIVE frontend's manifest.
func activeFrontendCompatibleToolchains() []string {
	bin, err := resolveInstalledFrontendFlutterBin()
	if err != nil || strings.TrimSpace(bin) == "" {
		return nil
	}
	// <version>/flutter-sdk-src/bin/flutter -> <version>/manifest.json
	dir := filepath.Dir(filepath.Dir(filepath.Dir(bin)))
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil
	}
	var m struct {
		CompatibleToolchainIDs []string `json:"compatible_toolchain_ids"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m.CompatibleToolchainIDs
}

// deriveReleaseIDForPlatform builds a stable release id from the project's own identity: the app id,
// the app version, and the PLATFORM. App id plus version is what a store release IS identified by, so
// two builds of the same app version get the same release id and a version bump gets a new one —
// without the developer inventing a label.
//
// THE PLATFORM COMPONENT IS LOAD-BEARING, not decoration. `--platforms=android,ios` dispatches the same
// derived flags to two lanes, but a release record holds exactly ONE platform and is keyed by its id, at
// three independent layers:
//
//   - the store rejects a duplicate id outright ("release %q already exists");
//   - the server's (app, version, platform, arch, channel) tuple is UNIQUE, and soroqctl deliberately
//     REFUSES a duplicate-key whose id differs rather than masking it;
//   - locally, baselines are stashed under .soroq/releases/<release-id>/, so one id means the second
//     platform's baseline lands on top of the first's.
//
// So an unqualified id does not merely look wrong — it makes the combined command impossible: whichever
// platform ran second could not register at all, while sharing the first's on-disk baseline directory.
// Qualifying by platform is what lets the two lanes coexist.
//
// Only the `--platforms` path derives ids (withDerivedFlags is its sole caller). The platform-specific
// commands — `soroq release ios`, `soroq patch android`, `patch ios --engine` — derive their own ids
// independently and are unaffected, so already-registered releases are never orphaned.
func deriveReleaseIDForPlatform(projectDir, platform string) string {
	appID := freehandProjectAppID(projectDir)
	if strings.TrimSpace(appID) == "" {
		return ""
	}
	short := appID
	if i := strings.LastIndex(appID, "."); i >= 0 && i+1 < len(appID) {
		short = appID[i+1:]
	}
	version := freehandProjectVersion(projectDir)
	if v := strings.SplitN(version, "+", 2); len(v) > 0 && strings.TrimSpace(v[0]) != "" {
		version = strings.TrimSpace(v[0])
	} else {
		version = "0"
	}
	base := fmt.Sprintf("%s-%s", strings.ToLower(short), strings.ReplaceAll(version, ".", "-"))
	if p := strings.ToLower(strings.TrimSpace(platform)); p != "" {
		base += "-" + p
	}
	return base
}

// withDerivedFlags appends the flags the canonical path derives, leaving anything the developer passed
// explicitly untouched.
func withDerivedFlags(platform, projectDir string, args []string) ([]string, error) {
	out := append([]string(nil), args...)
	if !hasFlag(out, "toolchain") {
		tc, err := deriveToolchain(platform)
		if err != nil {
			return nil, err
		}
		out = append(out, "--toolchain", tc)
	}
	if !hasFlag(out, "release-id") {
		if rid := deriveReleaseIDForPlatform(projectDir, platform); rid != "" {
			out = append(out, "--release-id", rid)
		}
	}
	// --api MUST be forwarded, not left to the delegate's own default.
	//
	// `soroq release --platforms=ios` delegates registration to soroqctl, whose `-api` defaults to
	// http://localhost:8080. Without an explicit value the control-plane app+release was never created:
	// the local baseline persisted, "registered engine-lane baseline" was printed, and the command
	// exited 0 — so it read as success. The next `soroq patch` then correctly resolved the real control
	// plane and failed with 404 "unknown release", pointing at the patch rather than the release that
	// silently did half its job.
	if !hasFlag(out, "api") {
		out = append(out, "--api", defaultAPIBase())
	}
	return out, nil
}

// releaseProjectDir resolves the project directory the same way the lanes do: --project-dir when given,
// otherwise the working directory.
func releaseProjectDir(args []string) string {
	if v, ok := flagValue(args, "project-dir"); ok && strings.TrimSpace(v) != "" {
		return v
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
