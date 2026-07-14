package main

// update_cmd.go — `soroq update` (production-safe self-updater) + `soroq update
// --check`.
//
// This REPLACES the P4 installer-pointer stub with a real, safety-first self
// updater. It resolves the latest STABLE public release of soroq/install via the
// public GitHub releases API (NO auth), downloads the exact archive for this
// OS/arch plus checksums.txt, verifies the archive SHA-256 BEFORE extraction,
// confirms the archive carries BOTH `soroq` and `soroqctl`, and replaces the two
// installed binaries TRANSACTIONALLY (both-or-neither) in the directory holding the
// currently-running soroq. Any failure at any step restores the previous binaries,
// so the existing installation always stays usable. See selfupdate.go for the
// engine. `--check` reports whether a newer stable release exists and makes ZERO
// filesystem changes.

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	checkOnly := fs.Bool("check", false, "check for a newer stable release without installing (no filesystem changes)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq update [--check]

Updates the installed soroq and soroqctl binaries to the latest stable release.

soroq update downloads the release archive for this OS/arch from the public
soroq/install releases (no GitHub auth), verifies its SHA-256 checksum BEFORE
extracting, and replaces both binaries atomically (both-or-neither). If anything
fails, the previous binaries are restored and the install stays usable.

  --check   report whether a newer stable release exists; makes NO changes`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	// Resolve the install dir = the directory holding the CURRENTLY-RUNNING soroq
	// binary (typically ~/.soroq/bin). We deliberately do NOT fall back to any
	// other directory: replacing the wrong binary would silently corrupt PATH.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate the running soroq binary: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}

	cfg := selfUpdateConfig{
		apiBase:        githubAPIBase,
		installRepo:    "soroq/install",
		installDir:     filepath.Dir(exe),
		goos:           runtime.GOOS,
		goarch:         runtime.GOARCH,
		currentVersion: buildVersion,
		checkOnly:      *checkOnly,
		stdout:         os.Stdout,
		httpClient:     &http.Client{Timeout: 120 * time.Second},
	}
	return performSelfUpdate(cfg)
}
