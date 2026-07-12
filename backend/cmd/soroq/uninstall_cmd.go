package main

// uninstall_cmd.go — `soroq uninstall`.
//
// Removes Soroq's own footprint and NOTHING else: the ~/.soroq state/cache
// directory (frontends, toolchains, config, tokens) plus the installed `soroq` and
// `soroqctl` binaries. It NEVER touches project files (soroq.yaml/soroq.lock, a
// Flutter app, the cwd). It REQUIRES --yes; without it the command prints exactly
// what it would remove and aborts having deleted nothing.
//
// The binary install dir honours the same SOROQ_INSTALL_DIR the installer uses
// (default ~/.soroq/bin). Only the two named binaries are removed — the install
// dir itself is never rmdir'd, since SOROQ_INSTALL_DIR may point at a shared
// location like /usr/local/bin. When the install dir is inside ~/.soroq (the
// default) the directory removal already covers the binaries.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// soroqStateDir returns ~/.soroq (the CLI's state + cache root).
func soroqStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".soroq"), nil
}

// binaryInstallDir mirrors install.sh: SOROQ_INSTALL_DIR or ~/.soroq/bin.
func binaryInstallDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("SOROQ_INSTALL_DIR")); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".soroq", "bin"), nil
}

// isWithin reports whether child is the same path as, or nested under, parent.
func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func runUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	yes := fs.Bool("yes", false, "actually remove (required; without it uninstall only prints the plan and aborts)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq uninstall --yes [--json]

Removes ~/.soroq (state, cache, config, tokens) and the installed soroq + soroqctl
binaries. Never touches project files. Requires --yes; without it the plan is
printed and nothing is deleted.`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	stateDir, err := soroqStateDir()
	if err != nil {
		return err
	}
	installDir, err := binaryInstallDir()
	if err != nil {
		return err
	}

	// Build the exact removal plan. Only the two named binaries are candidates,
	// and only when they live OUTSIDE ~/.soroq (otherwise the state-dir removal
	// already covers them — never double-handle, never rmdir a shared install dir).
	var targets []string
	if _, err := os.Stat(stateDir); err == nil {
		targets = append(targets, stateDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !isWithin(installDir, stateDir) {
		for _, name := range []string{"soroq", "soroqctl"} {
			p := filepath.Join(installDir, name)
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				targets = append(targets, p)
			}
		}
	}

	// Safety invariant: never remove anything outside ~/.soroq or the install dir.
	for _, t := range targets {
		if !isWithin(t, stateDir) && !isWithin(t, installDir) {
			return fmt.Errorf("refusing to remove %s: outside ~/.soroq and the binary install dir", t)
		}
	}

	if !*yes {
		if *jsonOut {
			if err := writeJSON(os.Stdout, map[string]any{
				"removed":      false,
				"confirm":      false,
				"would_remove": targetsOrEmpty(targets),
				"install_dir":  installDir,
				"state_dir":    stateDir,
				"hint":         "re-run with --yes to remove",
			}); err != nil {
				return err
			}
			return errAlreadyPrinted
		}
		if len(targets) == 0 {
			fmt.Fprintf(os.Stdout, "Nothing to uninstall (no %s and no installed binaries in %s).\n", stateDir, installDir)
			return nil
		}
		fmt.Fprintln(os.Stdout, "soroq uninstall would remove:")
		for _, t := range targets {
			fmt.Fprintf(os.Stdout, "  %s\n", t)
		}
		fmt.Fprintln(os.Stderr, "\nAborted: nothing was deleted. Re-run with --yes to proceed.")
		return errAlreadyPrinted
	}

	if len(targets) == 0 {
		if *jsonOut {
			return writeJSON(os.Stdout, map[string]any{"removed": true, "removed_paths": []string{}})
		}
		fmt.Fprintf(os.Stdout, "Nothing to uninstall (no %s and no installed binaries in %s).\n", stateDir, installDir)
		return nil
	}

	var removed []string
	for _, t := range targets {
		if err := os.RemoveAll(t); err != nil {
			return fmt.Errorf("remove %s: %w", t, err)
		}
		removed = append(removed, t)
	}

	if *jsonOut {
		return writeJSON(os.Stdout, map[string]any{"removed": true, "removed_paths": removed})
	}
	fmt.Fprintln(os.Stdout, "Uninstalled soroq. Removed:")
	for _, t := range removed {
		fmt.Fprintf(os.Stdout, "  %s\n", t)
	}
	return nil
}

// targetsOrEmpty returns a non-nil slice so --json always emits [] not null.
func targetsOrEmpty(t []string) []string {
	if t == nil {
		return []string{}
	}
	return t
}
