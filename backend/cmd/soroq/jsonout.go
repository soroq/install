package main

// jsonout.go — one shared machine-readable JSON writer for the beginner-facing
// commands added in P4 (update / uninstall / cache). It mirrors the inline
// `json.NewEncoder(os.Stdout); enc.SetIndent("", "  ")` style already used across
// the CLI (e.g. frontend_cmd.go reportFrontendInstall). It is deliberately NOT a
// refactor of the existing per-command encoders — those keep their inline shapes.

import (
	"encoding/json"
	"io"
)

// writeJSON encodes v as two-space-indented JSON (trailing newline) to w, matching
// the CLI's existing inline encoder style. Used by the update/uninstall/cache
// commands so their --json output is byte-consistent.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
