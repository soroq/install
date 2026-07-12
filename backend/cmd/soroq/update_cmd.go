package main

// update_cmd.go — `soroq update`.
//
// Deliberately NOT a self-updater. Soroq CLI binaries are distributed as signed
// release archives installed by the canonical installer (soroq/install's
// install.sh). This command therefore does NOT download or replace any binary in
// place; it reports the running version and points the developer at the one
// canonical, checksum-verifying install command. There is no releases API in the
// Go tree to query, so the latest-version check is intentionally a no-op here and
// the installer (which resolves "latest") is the source of truth.

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

// canonicalInstallCommand is the exact one-liner published by the soroq/install
// README (verified against raw.githubusercontent.com/soroq/install/main). It
// verifies the download checksum and installs to ~/.soroq/bin by default.
const canonicalInstallCommand = "curl --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/soroq/install/main/install.sh -sSf | bash"

func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq update [--json]

Reports the running soroq CLI version and the canonical install command. soroq
does NOT self-replace its binary; re-run the installer below to upgrade (it
downloads, checksum-verifies, and installs the latest signed release).`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if *jsonOut {
		return writeJSON(os.Stdout, map[string]any{
			"current_version": buildVersion,
			"self_update":     false,
			"install_command": canonicalInstallCommand,
		})
	}

	fmt.Fprintf(os.Stdout, "soroq %s\n", buildVersion)
	fmt.Fprintln(os.Stdout, "soroq does not self-update its binary.")
	fmt.Fprintln(os.Stdout, "To install or upgrade to the latest signed release, run:")
	fmt.Fprintf(os.Stdout, "  %s\n", canonicalInstallCommand)
	return nil
}
