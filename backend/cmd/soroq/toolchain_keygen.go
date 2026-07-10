package main

// soroq toolchain keygen — OPERATOR entrypoint that mints a fresh Ed25519 toolchain signing keypair
// for the toolchain trust domain (T003). It mirrors `soroqctl manifest-keygen` but for the SEPARATE
// toolchain domain (key id soroq-toolchain-kid-v1).
//
// Trust model (see toolchain_cmd.go): the CLI PINS the toolchain PUBLIC key (toolchainPinnedPublicKeyHex);
// the PRIVATE seed is OPERATOR-held and supplied at publish time via SOROQ_TOOLCHAIN_SIGNING_SEED /
// --signing-key-file. This command therefore:
//   - prints ONLY the PUBLIC key hex + the key id (the value an operator commits into the pinned const);
//   - writes the PRIVATE seed to a --out file with 0600 perms (operator custody) and NEVER prints it;
//   - refuses to clobber an existing --out (use --force) so an operator's only seed copy is not lost.
//
// The seed MUST then go to a secret store (Fly secret / GitHub Actions secret / password manager) and the
// on-disk 0600 copy deleted once stored. The seed is NEVER committed to the repo. The actual production
// re-pin of toolchainPinnedPublicKeyHex is OWNER-GATED — see docs/toolchain-signing-key-rotation.md.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"soroq/backend/internal/signing"
)

// runToolchainKeygen parses flags for `soroq toolchain keygen` and emits the keypair via os.Stdout.
func runToolchainKeygen(args []string) error {
	fs := flag.NewFlagSet("toolchain keygen", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	out := fs.String("out", "", "REQUIRED: path to write the PRIVATE seed (created 0600); the seed is NEVER printed")
	force := fs.Bool("force", false, "overwrite --out if it already exists (default: refuse, to protect an existing seed)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON (public key + key id only; never the seed)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq toolchain keygen --out ./toolchain.seed [--force] [--json]

Generates a fresh Ed25519 TOOLCHAIN signing keypair (key id soroq-toolchain-kid-v1).
  - The PUBLIC key hex + key id are printed (commit the pubkey into toolchainPinnedPublicKeyHex).
  - The PRIVATE seed is written to --out with 0600 perms and is NEVER printed to stdout/logs.

The seed MUST then move to a secret store (Fly secret / GitHub Actions secret / password manager)
and the 0600 file deleted once stored. NEVER commit the seed to the repo. The production re-pin of
toolchainPinnedPublicKeyHex is owner-gated — see docs/toolchain-signing-key-rotation.md.`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(*out) == "" {
		return errors.New("--out is required: keygen writes the PRIVATE seed to a 0600 file (it is never printed). Pass a path on a trusted disk, then move the seed to a secret store and delete the file. See docs/toolchain-signing-key-rotation.md")
	}
	_, _, err := keygenToolchain(os.Stdout, *out, *force, *jsonOut)
	return err
}

// keygenToolchain mints a fresh toolchain keypair, persists the seed to outPath (0600), and writes the
// PUBLIC-only summary to w. It returns the public key hex + key id. The seed is written to disk only and
// is never returned to or printed by callers (so it cannot leak via stdout/logs).
func keygenToolchain(w io.Writer, outPath string, force, jsonOut bool) (publicKeyHex, keyID string, err error) {
	seedBase64, publicKeyHex, keyID, err := signing.GenerateToolchainKeyPair()
	if err != nil {
		return "", "", err
	}

	// Write the seed FIRST, with exclusive-create so we never silently overwrite an operator's only seed
	// copy. --force opts into overwrite. Chmod after write guarantees 0600 regardless of umask.
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(outPath, flags, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return "", "", fmt.Errorf("--out %q already exists; refusing to overwrite a seed file (pass --force to replace it)", outPath)
		}
		return "", "", fmt.Errorf("create seed file: %w", err)
	}
	if _, werr := io.WriteString(f, seedBase64+"\n"); werr != nil {
		_ = f.Close()
		return "", "", fmt.Errorf("write seed file: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		return "", "", fmt.Errorf("close seed file: %w", cerr)
	}
	if cerr := os.Chmod(outPath, 0o600); cerr != nil {
		return "", "", fmt.Errorf("chmod seed file 0600: %w", cerr)
	}

	if jsonOut {
		summary := map[string]any{
			"algorithm":      signing.ToolchainSignatureScheme,
			"key_id":         keyID,
			"public_key_hex": publicKeyHex,
			"seed_path":      outPath,
			"seed_perms":     "0600",
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return publicKeyHex, keyID, enc.Encode(summary)
	}

	fmt.Fprintln(w, "Generated a fresh Soroq TOOLCHAIN signing keypair.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  public_key_hex: %s\n", publicKeyHex)
	fmt.Fprintf(w, "  key_id:         %s\n", keyID)
	fmt.Fprintf(w, "  seed_written:   %s  (0600, PRIVATE — not printed)\n", outPath)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "NEXT STEPS (the private seed is NEVER printed or committed):")
	fmt.Fprintln(w, "  1. Move the seed file to a secret store (Fly secret / GitHub Actions secret /")
	fmt.Fprintln(w, "     password manager), then delete the on-disk 0600 copy once it is stored.")
	fmt.Fprintln(w, "  2. To re-pin: set toolchainPinnedPublicKeyHex in cmd/soroq/toolchain_cmd.go to the")
	fmt.Fprintln(w, "     public_key_hex above (the key id stays soroq-toolchain-kid-v1).")
	fmt.Fprintln(w, "  3. At publish time supply the seed via SOROQ_TOOLCHAIN_SIGNING_SEED or")
	fmt.Fprintln(w, "     --signing-key-file; publish self-checks the seed's pubkey == the pinned key.")
	fmt.Fprintln(w, "  See docs/toolchain-signing-key-rotation.md for the full (owner-gated) procedure.")
	return publicKeyHex, keyID, nil
}
