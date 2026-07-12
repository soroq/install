package main

// cache_cmd.go — `soroq cache list` / `soroq cache clean`.
//
// The CLI caches large signed artifacts under ~/.soroq/frontends/<version>/ and
// ~/.soroq/toolchains/<version>/. `cache list` reports per-version sizes and marks
// which versions are ACTIVE. `cache clean` reclaims space by removing ONLY versions
// that no active pointer references — it is DRY-RUN by default and requires an
// explicit --delete to actually remove anything.
//
// A version is protected (never removable) when referenced by:
//   - frontends/active.json (the active frontend), or
//   - toolchains/active.json (any platform's toolchain_version OR frontend_version), or
//   - a soroq.lock in --project-dir/cwd (best effort; the global cache can't see
//     every project, so re-install remains the ultimate safety net).
// removable = installed − (active ∪ lock-pinned). The active/pinned version is
// structurally never in the delete set, and a pre-delete assertion re-checks it.

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func runCache(args []string) error {
	if len(args) == 0 {
		cacheUsage()
		return errAlreadyPrinted
	}
	switch args[0] {
	case "list":
		return runCacheList(args[1:])
	case "clean":
		return runCacheClean(args[1:])
	case "-h", "--help", "help":
		cacheUsage()
		return nil
	default:
		cacheUsage()
		return errAlreadyPrinted
	}
}

func cacheUsage() {
	fmt.Fprintln(os.Stderr, `usage: soroq cache <subcommand> [flags]

subcommands:
  list   list cached frontends + toolchains with per-version sizes; marks active
  clean  remove cached versions no active pointer references (dry-run by default; --delete to remove)`)
}

// cacheEntry is one cached version directory under frontends/ or toolchains/.
type cacheEntry struct {
	Version string `json:"version"`
	Dir     string `json:"dir"`
	Bytes   int64  `json:"bytes"`
	Active  bool   `json:"active"`
}

// activeCacheSets are the sets of frontend + toolchain versions any active.json
// pointer references (the protected-from-clean set, before soroq.lock pins).
func activeCacheSets() (frontends, toolchains map[string]bool) {
	frontends = map[string]bool{}
	toolchains = map[string]bool{}
	if af, ok, err := loadActiveFrontend(); err == nil && ok {
		if v := strings.TrimSpace(af.Version); v != "" {
			frontends[v] = true
		}
	}
	if at, err := loadActiveToolchains(); err == nil {
		for _, entry := range at.Platforms {
			if v := strings.TrimSpace(entry.ToolchainVersion); v != "" {
				toolchains[v] = true
			}
			if v := strings.TrimSpace(entry.FrontendVersion); v != "" {
				frontends[v] = true
			}
		}
	}
	return frontends, toolchains
}

// dirSize sums the byte size of every regular file under root.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// enumerateCache lists the version subdirectories of root with sizes, marking each
// active against activeSet. A missing root yields no entries (not an error).
func enumerateCache(root string, activeSet map[string]bool) ([]cacheEntry, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []cacheEntry
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		size, err := dirSize(dir)
		if err != nil {
			return nil, err
		}
		out = append(out, cacheEntry{Version: e.Name(), Dir: dir, Bytes: size, Active: activeSet[e.Name()]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func runCacheList(args []string) error {
	fs := flag.NewFlagSet("cache list", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() { fmt.Fprintln(os.Stdout, `usage: soroq cache list [--json]`) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	activeFrontends, activeToolchains := activeCacheSets()
	frontendsDir, err := frontendsRoot()
	if err != nil {
		return err
	}
	toolchainsDir, err := toolchainsRoot()
	if err != nil {
		return err
	}
	frontends, err := enumerateCache(frontendsDir, activeFrontends)
	if err != nil {
		return err
	}
	toolchains, err := enumerateCache(toolchainsDir, activeToolchains)
	if err != nil {
		return err
	}

	var total int64
	for _, e := range frontends {
		total += e.Bytes
	}
	for _, e := range toolchains {
		total += e.Bytes
	}

	if *jsonOut {
		return writeJSON(os.Stdout, map[string]any{
			"frontends":   cacheEntriesOrEmpty(frontends),
			"toolchains":  cacheEntriesOrEmpty(toolchains),
			"total_bytes": total,
		})
	}

	printCacheGroup("Frontends", frontendsDir, frontends)
	printCacheGroup("Toolchains", toolchainsDir, toolchains)
	fmt.Fprintf(os.Stdout, "\nTotal cached: %s\n", humanBytes(total))
	return nil
}

func printCacheGroup(title, root string, entries []cacheEntry) {
	fmt.Fprintf(os.Stdout, "%s (%s):\n", title, root)
	if len(entries) == 0 {
		fmt.Fprintln(os.Stdout, "  (none)")
		return
	}
	for _, e := range entries {
		marker := " "
		if e.Active {
			marker = "*"
		}
		fmt.Fprintf(os.Stdout, "  %s %-10s %s\n", marker, humanBytes(e.Bytes), e.Version)
	}
	fmt.Fprintln(os.Stdout, "  (* = active)")
}

func runCacheClean(args []string) error {
	fs := flag.NewFlagSet("cache clean", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	del := fs.Bool("delete", false, "actually delete the unreferenced versions (default is a dry run)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	projectDir := fs.String("project-dir", ".", "project dir whose soroq.lock pins are also protected (best effort)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq cache clean [--delete] [--project-dir <dir>] [--json]

Lists cached frontend + toolchain versions that no active pointer references and
would be removed. DRY RUN by default — pass --delete to actually remove them. The
active frontend/toolchain(s) and any soroq.lock-pinned version are always kept.`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	activeFrontends, activeToolchains := activeCacheSets()
	keepFrontends := cloneSet(activeFrontends)
	keepToolchains := cloneSet(activeToolchains)

	// Best-effort: also protect versions pinned by a soroq.lock in --project-dir.
	if lock, err := loadSoroqLock(strings.TrimSpace(*projectDir)); err == nil {
		for _, pin := range lock.Platforms {
			if v := strings.TrimSpace(pin.ToolchainVersion); v != "" {
				keepToolchains[v] = true
			}
			if v := strings.TrimSpace(pin.FrontendVersion); v != "" {
				keepFrontends[v] = true
			}
		}
	}

	frontendsDir, err := frontendsRoot()
	if err != nil {
		return err
	}
	toolchainsDir, err := toolchainsRoot()
	if err != nil {
		return err
	}
	frontends, err := enumerateCache(frontendsDir, activeFrontends)
	if err != nil {
		return err
	}
	toolchains, err := enumerateCache(toolchainsDir, activeToolchains)
	if err != nil {
		return err
	}

	removableFrontends := removable(frontends, keepFrontends)
	removableToolchains := removable(toolchains, keepToolchains)

	// Safety invariant: nothing kept (active or pinned) may reach the delete set.
	for _, e := range append(append([]cacheEntry{}, removableFrontends...), removableToolchains...) {
		if keepFrontends[e.Version] || keepToolchains[e.Version] {
			return fmt.Errorf("internal safety check: refusing to clean protected version %s", e.Version)
		}
	}

	var reclaimable int64
	for _, e := range removableFrontends {
		reclaimable += e.Bytes
	}
	for _, e := range removableToolchains {
		reclaimable += e.Bytes
	}

	deleted := false
	if *del {
		for _, e := range append(append([]cacheEntry{}, removableFrontends...), removableToolchains...) {
			if err := os.RemoveAll(e.Dir); err != nil {
				return fmt.Errorf("remove %s: %w", e.Dir, err)
			}
		}
		deleted = true
	}

	if *jsonOut {
		return writeJSON(os.Stdout, map[string]any{
			"dry_run":              !*del,
			"deleted":              deleted,
			"removable_frontends":  cacheEntriesOrEmpty(removableFrontends),
			"removable_toolchains": cacheEntriesOrEmpty(removableToolchains),
			"reclaimable_bytes":    reclaimable,
		})
	}

	if len(removableFrontends) == 0 && len(removableToolchains) == 0 {
		fmt.Fprintln(os.Stdout, "Nothing to clean; every cached version is active or pinned.")
		return nil
	}
	if deleted {
		fmt.Fprintln(os.Stdout, "Removed unreferenced cache versions:")
	} else {
		fmt.Fprintln(os.Stdout, "Would remove (dry run; pass --delete to remove):")
	}
	for _, e := range removableFrontends {
		fmt.Fprintf(os.Stdout, "  frontend  %-10s %s\n", humanBytes(e.Bytes), e.Version)
	}
	for _, e := range removableToolchains {
		fmt.Fprintf(os.Stdout, "  toolchain %-10s %s\n", humanBytes(e.Bytes), e.Version)
	}
	verb := "Reclaimable"
	if deleted {
		verb = "Reclaimed"
	}
	fmt.Fprintf(os.Stdout, "%s: %s\n", verb, humanBytes(reclaimable))
	return nil
}

func removable(entries []cacheEntry, keep map[string]bool) []cacheEntry {
	var out []cacheEntry
	for _, e := range entries {
		if !keep[e.Version] {
			out = append(out, e)
		}
	}
	return out
}

func cloneSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cacheEntriesOrEmpty(e []cacheEntry) []cacheEntry {
	if e == nil {
		return []cacheEntry{}
	}
	return e
}
