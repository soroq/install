package main

// manifest_trust_scaffold.go closes the fresh-user residual where a clean build fails with
// `Expected soroq.yaml to define "manifest_trust"`. The fork's Android asset bundler
// (packages/flutter_tools/lib/src/soroq_metadata.dart) derives the Android runtime_id from the
// soroq.yaml `manifest_trust` block whenever runtime_id_strategy is manifest_trust_v1, and throws
// when that block is missing. ensureManifestTrust auto-scaffolds a valid block (with an app-owned
// Ed25519 key) so a clean `soroq init` / `soroq release ios --engine --build` never dead-ends.
//
// TRUST SCOPE: the scaffolded key is an app-owned manifest signing key used to derive the Android
// runtime_id. It is NOT the iOS engine-lane OTA trust anchor — iOS OTA verification uses the app's
// `pinnedEnginePublicKeyHex` (in lib/main.dart). The scaffolded key's PUBLIC half is printed as a hex
// convenience so a developer MAY pin it for iOS if they want a single key, but nothing here touches
// the iOS pinned-key, patch, or tamper logic.
//
// SECRET HANDLING: the PRIVATE Ed25519 seed is written to <projectDir>/.soroq/manifest_signing_key.seed
// (mode 0600) and `.soroq/` is added to .gitignore. Only the PUBLIC key ever lands in soroq.yaml. The
// private seed is NEVER printed to stdout/stderr and NEVER written to soroq.yaml.

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/signing"
)

// manifestSigningSeedRelPath is the project-relative path of the app-owned manifest signing seed.
const manifestSigningSeedRelPath = ".soroq/manifest_signing_key.seed"

// ensureManifestTrust guarantees <projectDir>/soroq.yaml has a valid `manifest_trust` block so the
// fork's Android metadata bundler can derive a runtime_id. It returns the first key's public key as
// hex (a convenience for pinning it as the iOS pinnedEnginePublicKeyHex).
//
//   - PRESENT + VALID  → returns the first key's pubkey hex; soroq.yaml is left byte-for-byte unchanged.
//   - PRESENT + INVALID → returns an actionable error naming the exact bad field; nothing is written.
//   - ABSENT           → generates (or reuses) an app-owned Ed25519 key, writes the seed 0600, gitignores
//     `.soroq/`, injects a manifest_trust block into soroq.yaml (preserving existing content), and
//     returns the new pubkey hex.
//
// It is idempotent: once a valid block exists, subsequent calls are a no-op that return the same key.
func ensureManifestTrust(projectDir string) (publicKeyHex string, err error) {
	soroqPath := filepath.Join(projectDir, "soroq.yaml")
	configBytes, err := os.ReadFile(soroqPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("soroq.yaml not found at %s; run `soroq init` first", soroqPath)
		}
		return "", err
	}

	// parseSoroqManifestTrust returns nil (no error) only when there is no top-level `manifest_trust:`
	// block, so a non-nil result means the block is PRESENT (possibly malformed) and must be validated
	// rather than overwritten.
	trust, err := parseSoroqManifestTrust(configBytes)
	if err != nil {
		return "", err
	}
	if trust != nil {
		return validateExistingManifestTrust(trust)
	}
	return scaffoldManifestTrust(projectDir, soroqPath, configBytes)
}

// validateExistingManifestTrust checks a present manifest_trust block WITHOUT modifying soroq.yaml. It
// returns an actionable error naming the exact missing/bad field. Validity requires an integer
// keyset_version and a non-empty keys list where every key has a non-empty id and a decodable
// (unpadded) base64url public_key. Length is intentionally NOT constrained to 32 bytes here so that
// pre-existing/manual keys are preserved as-is; the returned hex is hex(decoded) regardless.
func validateExistingManifestTrust(trust *androidrelease.ManifestTrust) (string, error) {
	const fix = `; fix the field in soroq.yaml, or run: soroq init --force`
	if trust.KeysetVersion == nil {
		return "", errors.New(`soroq.yaml manifest_trust is missing a valid integer "keyset_version"` + fix)
	}
	if len(trust.Keys) == 0 {
		return "", errors.New("soroq.yaml manifest_trust.keys is empty (needs at least one key)" + fix)
	}
	for i, k := range trust.Keys {
		id := ""
		if k.ID != nil {
			id = strings.TrimSpace(*k.ID)
		}
		if id == "" {
			return "", fmt.Errorf(`soroq.yaml manifest_trust.keys[%d] is missing "id"`+fix, i)
		}
		if strings.TrimSpace(k.PublicKey) == "" {
			return "", fmt.Errorf(`soroq.yaml manifest_trust.keys[%d] is missing "public_key"`+fix, i)
		}
		if _, derr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(k.PublicKey)); derr != nil {
			return "", fmt.Errorf(`soroq.yaml manifest_trust.keys[%d] "public_key" is not valid unpadded base64url: %v`+fix, i, derr)
		}
	}
	decoded, _ := base64.RawURLEncoding.DecodeString(strings.TrimSpace(trust.Keys[0].PublicKey))
	return hex.EncodeToString(decoded), nil
}

// scaffoldManifestTrust is the ABSENT branch: it materializes an app-owned Ed25519 manifest signing
// key (reusing an existing seed file if one is present, so a lost yaml block cannot orphan/destroy a
// private key), persists the seed 0600, gitignores `.soroq/`, and injects a manifest_trust block into
// soroq.yaml. Only the PUBLIC key is written to soroq.yaml; the private seed is never printed.
func scaffoldManifestTrust(projectDir, soroqPath string, configBytes []byte) (string, error) {
	seedPath := filepath.Join(projectDir, filepath.FromSlash(manifestSigningSeedRelPath))

	publicKeyBase64, err := loadOrGenerateManifestSeed(seedPath)
	if err != nil {
		return "", err
	}
	pubBytes, err := base64.RawURLEncoding.DecodeString(publicKeyBase64)
	if err != nil {
		return "", fmt.Errorf("decode generated manifest public key: %w", err)
	}
	publicKeyHex := hex.EncodeToString(pubBytes)

	if err := ensureGitignoreLine(projectDir, ".soroq/"); err != nil {
		return "", err
	}

	appID := strings.TrimSpace(parseTopLevelYaml(configBytes)["app_id"])
	if appID == "" {
		appID = "primary"
	}
	keyID := appID + "-signing"

	newContent := injectManifestTrust(string(configBytes), keyID, publicKeyBase64)
	if err := os.WriteFile(soroqPath, []byte(newContent), 0o644); err != nil {
		return "", fmt.Errorf("write soroq.yaml manifest_trust: %w", err)
	}

	fmt.Fprintf(os.Stderr, "soroq: scaffolded a manifest_trust block in %s\n", soroqPath)
	fmt.Fprintf(os.Stderr, "  manifest signing public key (hex): %s\n", publicKeyHex)
	fmt.Fprintln(os.Stderr, "    (optionally pin this as pinnedEnginePublicKeyHex in lib/main.dart for a single iOS engine-lane key)")
	fmt.Fprintf(os.Stderr, "  private signing seed written to %s (mode 0600, gitignored via .soroq/)\n", seedPath)
	fmt.Fprintln(os.Stderr, "  the private seed is NEVER committed and is not written to soroq.yaml.")
	return publicKeyHex, nil
}

// loadOrGenerateManifestSeed returns the base64url public key for the manifest signing seed at
// seedPath. If the seed file already exists it is reused (making scaffolding idempotent even if the
// soroq.yaml block was removed); otherwise a fresh Ed25519 keypair is generated and its seed persisted
// with mode 0600. The seed value is never returned to callers so it cannot leak via logs.
func loadOrGenerateManifestSeed(seedPath string) (publicKeyBase64 string, err error) {
	if existing, rerr := os.ReadFile(seedPath); rerr == nil {
		seedBase64 := strings.TrimSpace(string(existing))
		signer, serr := signing.NewManifestSignerFromSeedBase64(seedBase64, "")
		if serr != nil {
			return "", fmt.Errorf("existing manifest signing seed at %s is invalid (%w); delete it to regenerate", seedPath, serr)
		}
		return signer.PublicKeyBase64(), nil
	} else if !errors.Is(rerr, os.ErrNotExist) {
		return "", rerr
	}

	seedBase64, publicKeyBase64, _, gerr := signing.GenerateManifestKeyPair()
	if gerr != nil {
		return "", gerr
	}
	if err := os.MkdirAll(filepath.Dir(seedPath), 0o700); err != nil {
		return "", err
	}
	// O_EXCL so a concurrent writer cannot be clobbered; chmod re-asserts 0600 regardless of umask.
	f, err := os.OpenFile(seedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write manifest signing seed: %w", err)
	}
	if _, werr := f.WriteString(seedBase64 + "\n"); werr != nil {
		_ = f.Close()
		return "", fmt.Errorf("write manifest signing seed: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		return "", fmt.Errorf("close manifest signing seed: %w", cerr)
	}
	if cerr := os.Chmod(seedPath, 0o600); cerr != nil {
		return "", fmt.Errorf("chmod manifest signing seed 0600: %w", cerr)
	}
	return publicKeyBase64, nil
}

// injectManifestTrust appends a manifest_trust block to soroq.yaml content, preserving everything that
// is already there. It also sets `runtime_id_strategy: manifest_trust_v1` when that key is unset. The
// block layout matches renderSoroqConfig so parseSoroqManifestTrust reads it back identically.
func injectManifestTrust(content, keyID, publicKeyBase64 string) string {
	if strings.TrimSpace(parseTopLevelYaml([]byte(content))["runtime_id_strategy"]) == "" {
		content = ensureRuntimeIDStrategyLine(content)
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	var b strings.Builder
	b.WriteString(content)
	b.WriteString("manifest_trust:\n")
	b.WriteString("  keyset_version: 1\n")
	b.WriteString("  keys:\n")
	b.WriteString("    - id: " + keyID + "\n")
	b.WriteString("      public_key: " + publicKeyBase64 + "\n")
	return b.String()
}

// ensureRuntimeIDStrategyLine sets the top-level runtime_id_strategy to manifest_trust_v1. It replaces
// an existing (empty-valued) top-level runtime_id_strategy line in place if present, otherwise appends
// one. Only called when the value is currently unset.
func ensureRuntimeIDStrategyLine(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if startsWithIndent(line) {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "runtime_id_strategy:") {
			lines[i] = "runtime_id_strategy: manifest_trust_v1"
			return strings.Join(lines, "\n")
		}
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + "runtime_id_strategy: manifest_trust_v1\n"
}

// ensureGitignoreLine makes sure <projectDir>/.gitignore ignores the given path. A bare `.soroq`
// entry (no trailing slash) already satisfies a `.soroq/` requirement, so no near-duplicate is
// appended. The .gitignore is created when absent.
func ensureGitignoreLine(projectDir, line string) error {
	gitignorePath := filepath.Join(projectDir, ".gitignore")
	existing, err := os.ReadFile(gitignorePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	wanted := strings.TrimSuffix(line, "/")
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.TrimSuffix(strings.TrimSpace(l), "/") == wanted {
			return nil
		}
	}
	var out strings.Builder
	out.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		out.WriteString("\n")
	}
	out.WriteString(line + "\n")
	return os.WriteFile(gitignorePath, []byte(out.String()), 0o644)
}
