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
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq setup <platform> [--force]
       soroq setup --platforms android,ios [--force]

Fetches + verifies the signed Soroq platform catalog, then for each requested platform installs the
matching Soroq Flutter frontend and build-time toolchain (no long version IDs, no --api required).
The catalog signature is verified against the pinned key and the schema is enforced before any install;
a bad signature, wrong schema, or absent platform is REFUSED. --api is an advanced override.`)
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

	// Fetch + VERIFY + schema-gate the catalog ONCE (same document for every platform). No unsigned fallback.
	catalog, err := fetchVerifiedCatalog(base)
	if err != nil {
		return err
	}

	for _, platform := range platforms {
		if err := setupPlatform(platform, catalog, base, *force); err != nil {
			// Fail-fast: never record a partial set. A failure on one platform stops setup with a clear error.
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
func setupPlatform(platform string, catalog catalogDoc, base string, force bool) error {
	entry, err := catalog.entryForPlatform(platform)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "Setting up %s: frontend %s, toolchain %s\n", platform, entry.FrontendVersion, entry.ToolchainVersion)

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
