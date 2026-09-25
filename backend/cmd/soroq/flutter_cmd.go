package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// soroq flutter — the catalog-driven Flutter version-manager surface.
//
// WHY THIS IS CATALOG-DRIVEN AND NOT A TABLE IN THIS FILE. The supported Flutter matrix is produced
// elsewhere and published as signed artifacts. If this command carried a hardcoded list, every new
// matrix entry would need a CLI release; read from the catalog, a new entry needs only data
// publication. That is the whole design constraint, so there is deliberately NO version literal here.
//
// WHY EXACT VERSIONS ONLY. Soroq patches are compiled against one engine and one Dart revision. A
// semver range would advertise compatibility the product cannot honour, so `versions list` prints only
// exactly what is published and `use` accepts only an exact match.

// flutterVersionEntry is one supported Flutter version, resolved from the SIGNED toolchain manifest
// behind a catalog entry rather than from the catalog text itself.
type flutterVersionEntry struct {
	FlutterVersion   string `json:"flutter_version"`
	FlutterRevision  string `json:"flutter_revision"`
	DartRevision     string `json:"dart_revision"`
	EngineRevision   string `json:"soroq_engine_revision"`
	ToolchainVersion string `json:"toolchain_version"`
	FrontendVersion  string `json:"frontend_version"`
	Platform         string `json:"platform"`
	Tier             string `json:"tier"`
	BuildMode        string `json:"build_mode"`
}

func runFlutter(args []string) error {
	if len(args) == 0 {
		flutterUsage()
		return fmt.Errorf("soroq flutter needs a subcommand")
	}
	switch args[0] {
	case "versions":
		return runFlutterVersions(args[1:])
	case "use":
		return runFlutterUse(args[1:])
	case "-h", "--help", "help":
		flutterUsage()
		return nil
	default:
		flutterUsage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func flutterUsage() {
	fmt.Fprintln(os.Stderr, `usage: soroq flutter <subcommand> [flags]

subcommands:
  versions list        show every Flutter version Soroq supports right now
  use <version>        pin this project to one exact supported Flutter version

flags:
  --api <url>          control plane to read the supported list from
  --project-dir <dir>  project to pin (defaults to the current directory)
  --json               machine-readable output

Soroq supports exact Flutter versions, not ranges: a patch is compiled against one
engine and one Dart revision. Run "soroq flutter versions list" to see what is
published, then "soroq flutter use <version>" to pin this project to one.`)
}

// resolveSupportedFlutterVersions reads the SIGNED catalog and resolves each platform entry to the
// exact Flutter identity recorded in its signed toolchain manifest.
//
// It reads whatever the catalog publishes. Today catalog.v1 carries one toolchain per platform, so this
// returns one entry per platform; when the matrix publishes more, this returns more without a code
// change. Nothing here caps, filters or invents a version.
func resolveSupportedFlutterVersions(api string) ([]flutterVersionEntry, error) {
	doc, err := fetchVerifiedCatalog(api)
	if err != nil {
		return nil, fmt.Errorf("read the supported Flutter list: %w", err)
	}
	// AN EMPTY LIST IS `[]`, NOT `null`. A nil slice marshals to `null`, and a consumer that does
	// `for x in result` then fails on a machine where nothing is installed -- the exact machine a
	// first run happens on. The command declares JSON; an empty array is what "no entries" looks like.
	out := []flutterVersionEntry{}
	for _, platform := range doc.platformNames() {
		entry, err := doc.entryForPlatform(platform)
		if err != nil {
			continue
		}
		manifest, err := fetchCatalogToolchainManifest(api, entry.ToolchainVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve toolchain %s: %w", entry.ToolchainVersion, err)
		}
		out = append(out, flutterVersionEntry{
			FlutterVersion:   manifest.FlutterVersion,
			FlutterRevision:  manifest.FlutterRevision,
			DartRevision:     manifest.DartRevision,
			EngineRevision:   manifest.SoroqEngineRevision,
			ToolchainVersion: manifest.SoroqToolchainVersion,
			FrontendVersion:  entry.FrontendVersion,
			Platform:         platform,
			Tier:             manifest.Tier,
			BuildMode:        manifest.BuildMode,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FlutterVersion != out[j].FlutterVersion {
			return out[i].FlutterVersion < out[j].FlutterVersion
		}
		return out[i].Platform < out[j].Platform
	})
	return out, nil
}

func runFlutterVersions(args []string) error {
	fs := flag.NewFlagSet("flutter versions", flag.ContinueOnError)
	api := fs.String("api", defaultAPIBase(), "control plane base url")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// VALIDATE COUNT AS WELL AS MEANING. This checked rest[0] and ignored everything after it, so
	// `soroq flutter versions list ios` printed every platform and exited 0 -- a reader who asked for
	// one platform got them all, with nothing to say the word had been dropped.
	rest := fs.Args()
	if len(rest) > 0 && rest[0] != "list" {
		return fmt.Errorf("unknown subcommand %q (did you mean: soroq flutter versions list)", rest[0])
	}
	if len(rest) > 1 {
		if err := refuseUnconsumedArguments("flutter versions list", rest[1:], []string{
			"no further arguments -- it lists every supported version",
			"`--json` for machine-readable output",
		}); err != nil {
			return err
		}
	}

	entries, err := resolveSupportedFlutterVersions(*api)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"schema":   "soroq.flutter_versions.v1",
			"api":      *api,
			"versions": entries,
		})
	}
	if len(entries) == 0 {
		fmt.Println("No Flutter versions are published for this control plane yet.")
		return nil
	}
	fmt.Println("Flutter versions Soroq supports right now:")
	fmt.Println()
	for _, e := range entries {
		fmt.Printf("  %-10s %-8s engine %s\n", e.FlutterVersion, e.Platform, shortRevision(e.EngineRevision))
		fmt.Printf("             %-8s dart %s, toolchain %s\n", "", shortRevision(e.DartRevision), e.ToolchainVersion)
	}
	fmt.Println()
	fmt.Printf("Pin this project with:  soroq flutter use %s\n", entries[0].FlutterVersion)
	return nil
}

func runFlutterUse(args []string) error {
	// FLAGS MAY COME BEFORE OR AFTER THE VERSION. Go's flag package stops at the first non-flag
	// argument, so `use 3.44.2 --project-dir .` silently ignored the flag -- the most natural order a
	// person types. The positional is extracted first so both orders behave the same.
	var positional []string
	var flags []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") &&
				(strings.HasSuffix(a, "api") || strings.HasSuffix(a, "project-dir")) {
				i++
				flags = append(flags, args[i])
			}
		default:
			positional = append(positional, a)
		}
	}
	args = append(flags, positional...)

	fs := flag.NewFlagSet("flutter use", flag.ContinueOnError)
	api := fs.String("api", defaultAPIBase(), "control plane base url")
	projectDir := fs.String("project-dir", ".", "project directory")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: soroq flutter use <exact-version>  (run: soroq flutter versions list)")
	}
	want := strings.TrimSpace(rest[0])

	entries, err := resolveSupportedFlutterVersions(*api)
	if err != nil {
		return err
	}

	// REFUSE BEFORE ANY EXPENSIVE WORK. Selecting an unsupported version must fail here, in a network
	// round trip, not thirty minutes into a build that cannot produce a patchable artifact.
	var matched []flutterVersionEntry
	for _, e := range entries {
		if e.FlutterVersion == want {
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 {
		var available []string
		seen := map[string]bool{}
		for _, e := range entries {
			if !seen[e.FlutterVersion] {
				seen[e.FlutterVersion] = true
				available = append(available, e.FlutterVersion)
			}
		}
		if len(available) == 0 {
			return fmt.Errorf("Flutter %s is not supported, and this control plane publishes no versions yet", want)
		}
		return fmt.Errorf("Flutter %s is not supported. Soroq supports exactly: %s\n"+
			"Soroq pins exact versions, not ranges: a patch is compiled against one engine and one Dart revision",
			want, strings.Join(available, ", "))
	}

	lock, err := loadSoroqLock(*projectDir)
	if err != nil {
		return err
	}
	if lock.Platforms == nil {
		lock.Platforms = map[string]soroqLockPin{}
	}
	for _, e := range matched {
		// Only the toolchain/frontend change; the pinned release's identity (id, version, flavor) is kept.
		// Per-flavor pins (<platform>@<flavor>) record which toolchain built each flavor's release and
		// are deliberately left as they are.
		lock.Platforms[e.Platform] = soroqLockPin{
			ReleaseID:        lock.Platforms[e.Platform].ReleaseID,
			Version:          lock.Platforms[e.Platform].Version,
			ToolchainVersion: e.ToolchainVersion,
			FrontendVersion:  e.FrontendVersion,
			Flavor:           lock.Platforms[e.Platform].Flavor,
			RecordedAt:       time.Now().UTC(),
		}
	}
	if err := saveSoroqLock(*projectDir, lock); err != nil {
		return err
	}

	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"schema":          "soroq.flutter_use.v1",
			"flutter_version": want,
			"pinned":          matched,
			"lock":            soroqLockPath(*projectDir),
		})
	}
	fmt.Printf("Pinned this project to Flutter %s.\n\n", want)
	for _, e := range matched {
		fmt.Printf("  %-8s toolchain %s\n", e.Platform, e.ToolchainVersion)
		fmt.Printf("  %-8s engine    %s\n", "", shortRevision(e.EngineRevision))
	}
	fmt.Printf("\nWritten to %s\n", soroqLockPath(*projectDir))
	fmt.Printf("Next:  soroq doctor --all\n")
	return nil
}

// shortRevision abbreviates a 40-char git revision and leaves everything else alone.
//
// It used to cut every string at 16 characters, which turned the engine revision
// "soroq.android_engine.f74781f6..." into "soroq.android_en" -- a truncation mid-word that reads like
// corruption rather than an abbreviation. Only hex looks like a revision worth shortening.
func shortRevision(rev string) string {
	if len(rev) >= 32 && isHexString(rev) {
		return rev[:12]
	}
	return rev
}

func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}
