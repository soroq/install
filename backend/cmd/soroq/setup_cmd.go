package main

// setup_cmd.go — `soroq setup <platform>` (and `soroq setup --platforms android,ios`): the one-shot,
// no-long-IDs onboarding path. It fetches + verifies the signed platform catalog, resolves the
// {frontend_version, toolchain_version} for each requested platform, invokes the EXISTING frontend-install
// and toolchain-install functions (as libraries — no reimplementation of download/verify/cache), and
// records the per-platform active toolchain pointer.
//
// No long IDs, no mandatory --api (defaults to defaultAPIBase() = api.soroq.dev; --api is an optional
// advanced override). No unsigned fallback (fetchVerifiedCatalog REFUSES a bad signature / wrong schema /
// absent platform). The manual `frontend install` / `toolchain install` commands remain the UNCHANGED
// advanced path.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// installFrontend / installToolchain are seams onto the EXISTING install functions (called as libraries).
// Production points them at the real installers; tests override them to prove the verify -> resolve ->
// record path without real network artifacts / soroqctl. Overriding these is NOT editing the install
// files — the install functions themselves are unchanged.
var (
	installFrontend  = runFrontendInstall
	installToolchain = runToolchainInstall
)

// runSetup is the `soroq setup` entrypoint.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	platformsFlag := fs.String("platforms", "", "comma-separated platforms to set up (e.g. android,ios)")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL (advanced override; defaults to "+defaultControlPlaneAPI+")")
	force := fs.Bool("force", false, "force a clean reinstall even if a verified install exists")
	// V2 IS OPT-IN AND STAYS OPT-IN. Without --catalog-v2 this command behaves exactly as it did: the
	// v1 catalog, resolved by platform id. The flag exists for qualification of a specific engine
	// revision, which v1 cannot express at all.
	catalogV2 := fs.Bool("catalog-v2", false, "resolve through the signed soroq.catalog.v2 version matrix (opt-in; requires --flutter-revision)")
	flutterRevision := fs.String("flutter-revision", "", "exact 40-hex Flutter revision to resolve with --catalog-v2")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq setup <platform> [--force]
       soroq setup --platforms android,ios [--force]

Fetches + verifies the signed Soroq platform catalog, then for each requested platform installs the
matching Soroq Flutter frontend and build-time toolchain (no long version IDs, no --api required).
The catalog signature is verified against the pinned key and the schema is enforced before any install;
a bad signature, wrong schema, or absent platform is REFUSED. --api is an advanced override.

QUALIFICATION (opt-in, v2 version matrix):
       soroq setup ios --catalog-v2 --flutter-revision <40-hex>

v1 pins ONE pair per platform, so it cannot express two Flutter versions at once. --catalog-v2
resolves the pair for an EXACT Flutter revision from the signed soroq.catalog.v2 document. v1 remains
the default; v2 is used only when this flag is given. If v2 is not published the command falls back to
v1 ONLY when v1's own signed manifests are built from the requested revision — otherwise it REFUSES
rather than install an engine built for a different revision.`)
	}
	// Pull the leading positional platform args (e.g. `setup android ios --force`) out BEFORE flag parsing:
	// Go's flag package stops at the first non-flag arg, so a positional before its flags would otherwise
	// hide `--api`/`--force` (mirrors extractToolchainVersionArg in toolchain_install.go).
	positional, rest := extractLeadingPositionals(args)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// Any positionals after the flags (e.g. `setup --force android`) are also accepted.
	positional = append(positional, fs.Args()...)

	platforms, err := parseSetupPlatforms(positional, *platformsFlag)
	if err != nil {
		return err
	}

	base := strings.TrimRight(strings.TrimSpace(*apiBase), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}

	if *catalogV2 {
		return runSetupV2(base, platforms, strings.TrimSpace(*flutterRevision), *force)
	}
	if strings.TrimSpace(*flutterRevision) != "" {
		// --flutter-revision without --catalog-v2 would be silently ignored, and the caller would believe
		// they had pinned a revision when they had not. Refuse instead.
		return errors.New("--flutter-revision requires --catalog-v2 (v1 has no revision dimension)")
	}

	// Fetch + VERIFY + schema-gate the catalog ONCE (same document for every platform). No unsigned fallback.
	catalog, err := fetchVerifiedCatalog(base)
	if err != nil {
		return err
	}
	// Resolve and verify EVERY requested pair before the first installer runs. This preserves the
	// no-side-effects promise when (for example) android is healthy but a later iOS catalog reference is
	// missing, and prevents a large frontend download before discovering an absent toolchain.
	preflight, err := catalogReferencePreflightFn(base, catalog, platforms)
	if err != nil {
		return fmt.Errorf("catalog artifact preflight: %w", err)
	}

	for _, platform := range platforms {
		verified, ok := preflight[platform]
		if !ok {
			return fmt.Errorf("catalog artifact preflight returned no verified pair for %s", platform)
		}
		if err := setupPlatform(platform, catalog, verified, base, *force); err != nil {
			// Fail-fast: never record a partial set. A failure on one platform stops setup with a clear error.
			return fmt.Errorf("setup %s: %w", platform, err)
		}
	}
	return nil
}

// runSetupV2 is the opt-in qualification path: resolve each platform's pair for an EXACT Flutter
// revision, then install through the SAME installers and record the SAME active pointer as v1. Only the
// resolution differs; nothing about installation or recording is special-cased for v2.
func runSetupV2(base string, platforms []string, flutterRevision string, force bool) error {
	if flutterRevision == "" {
		return errors.New("--catalog-v2 requires --flutter-revision <40-hex> (the matrix has more than one pair per platform; only you know which revision you are building)")
	}

	// PHASE 1: resolve EVERY platform. Three phases — resolve all, preflight all, install all — so the
	// v2 path keeps the same no-side-effects promise the v1 path makes.
	// ONE verified document for the whole invocation, so android and ios cannot come from different
	// catalog generations if a publish lands mid-setup.
	selections, err := resolveCatalogPairsFromOneSnapshot(base, platforms, flutterRevision)
	if err != nil {
		return err
	}

	// PHASE 2: preflight EVERY selected pair, storing the verified results. Nothing is installed here.
	//
	// This is separated from installation deliberately. Interleaving them means a two-platform setup
	// whose SECOND platform fails preflight has already installed the first and written its active
	// pointer — a partial state the v1 path does not produce, and the exact asymmetry that makes a
	// failed setup hard to reason about afterwards.
	verifiedPairs := make(map[string]catalogPlatformPreflight, len(platforms))
	for _, platform := range platforms {
		verified, err := selectedPairPreflightFn(base, selections[platform])
		if err != nil {
			return fmt.Errorf("setup %s: catalog artifact preflight: %w", platform, err)
		}
		verifiedPairs[platform] = verified
	}

	// PHASE 3: only now, with every pair verified, does anything get installed.
	for _, platform := range platforms {
		if err := setupResolvedPair(selections[platform], verifiedPairs[platform], base, force); err != nil {
			return fmt.Errorf("setup %s: %w", platform, err)
		}
	}
	return nil
}

// extractLeadingPositionals returns the leading run of non-flag args (the platforms) and the remaining
// args (starting at the first flag) for flag parsing.
func extractLeadingPositionals(args []string) (positional, rest []string) {
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		i++
	}
	return args[:i], args[i:]
}

// parseSetupPlatforms merges positional platform args with the --platforms list, normalizing + de-duping.
func parseSetupPlatforms(positional []string, platformsFlag string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, a := range positional {
		add(a)
	}
	for _, a := range strings.Split(platformsFlag, ",") {
		add(a)
	}
	if len(out) == 0 {
		return nil, errors.New("usage: soroq setup <platform> | soroq setup --platforms android,ios")
	}
	return out, nil
}

// setupPlatform resolves the catalog entry for one platform and installs its frontend + toolchain, then
// records the active toolchain. The active pointer is written ONLY after BOTH installs succeed, so it
// never points at a partial/failed install.
func setupPlatform(platform string, catalog catalogDoc, verified catalogPlatformPreflight, base string, force bool) error {
	entry, err := catalog.entryForPlatform(platform)
	if err != nil {
		return err
	}

	return setupResolvedPair(catalogSelection{
		Platform:         platform,
		FrontendVersion:  entry.FrontendVersion,
		ToolchainVersion: entry.ToolchainVersion,
		Source:           "v1",
	}, verified, base, force)
}

// setupResolvedPair installs a RESOLVED pair and records it active. Both the v1 and v2 paths funnel
// through this single function, so the install gates and the active-pointer write cannot diverge
// between them — the only thing v2 changes is how the pair was chosen.
func setupResolvedPair(sel catalogSelection, verified catalogPlatformPreflight, base string, force bool) error {
	platform := sel.Platform
	entry := catalogPlatform{FrontendVersion: sel.FrontendVersion, ToolchainVersion: sel.ToolchainVersion}

	if sel.FlutterRevision != "" {
		fmt.Fprintf(os.Stdout, "Setting up %s via %s: flutter %s (%s)\n", platform, sel.Source, sel.FlutterVersion, shortRev(sel.FlutterRevision))
	}
	fmt.Fprintf(os.Stdout, "Setting up %s: frontend %s, toolchain %s\n", platform, entry.FrontendVersion, entry.ToolchainVersion)
	fmt.Fprintf(os.Stdout, "  verified pair: build_mode=%s tier=%s\n", verified.Toolchain.BuildMode, verified.Toolchain.Tier)
	if strings.EqualFold(platform, "ios") &&
		(!strings.EqualFold(strings.TrimSpace(verified.Toolchain.BuildMode), "release") ||
			!strings.EqualFold(strings.TrimSpace(verified.Toolchain.Tier), "production")) {
		fmt.Fprintln(os.Stderr, "WARNING: selected iOS toolchain is experimental and is NOT an App-Store-production engine; device lifecycle success does not imply Apple approval.")
	}

	// Frontend install (existing installer, called as a library). It re-verifies signature + archive hash
	// and caches under ~/.soroq/frontends/.
	if err := installFrontend(frontendInstallArgs(entry.FrontendVersion, base, force)); err != nil {
		return fmt.Errorf("frontend install: %w", err)
	}
	// Toolchain install (existing installer, called as a library). Full verify gate kept (NO
	// --skip-bundle-verify). Caches under ~/.soroq/toolchains/.
	if err := installToolchain(toolchainInstallArgs(entry.ToolchainVersion, base, force)); err != nil {
		return fmt.Errorf("toolchain install: %w", err)
	}

	// Record the active toolchain for this platform ONLY after both installs succeeded.
	if err := recordActiveToolchain(platform, activeToolchainEntry{
		ToolchainVersion: entry.ToolchainVersion,
		FrontendVersion:  entry.FrontendVersion,
		RecordedAt:       time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("record active toolchain: %w", err)
	}
	fmt.Fprintf(os.Stdout, "  %s ready (toolchain %s active)\n", platform, entry.ToolchainVersion)
	return nil
}

func frontendInstallArgs(version, base string, force bool) []string {
	args := []string{version, "--api", base}
	if force {
		args = append(args, "--force")
	}
	return args
}

func toolchainInstallArgs(version, base string, force bool) []string {
	args := []string{version, "--api", base}
	if force {
		args = append(args, "--force")
	}
	return args
}
