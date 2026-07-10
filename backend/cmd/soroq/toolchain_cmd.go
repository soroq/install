package main

// soroq toolchain — hosted build-time engine TOOLCHAIN lifecycle (T005).
//
// This is a NEW, separate trust domain from the device engine-lane pinned key and the backend
// PatchManifest signer (T002 §4). The CLI PINS the toolchain PUBLIC key (toolchainPinnedPublicKeyHex
// below); the PRIVATE key is OPERATOR-held and supplied at publish time via SOROQ_TOOLCHAIN_SIGNING_SEED
// or --signing-key-file. The private key is NEVER committed, printed, or persisted.
//
// Lifecycle:
//   - publish (operator): Ed25519-sign the EXACT (augmented) manifest bytes, PUT manifest+sig to the
//     registry (/v1/toolchains/{version}, operator-auth). By DEFAULT it also uploads the archive bytes to
//     the control plane (/v1/toolchains/{version}/archive, operator-auth) and points archive.url at that
//     route; --archive-url overrides with an external host (no upload). The signed manifest pins
//     archive.sha256 + archive.url; the archive is fetched from that URL at install time.
//   - install <version> --api <base>: GET manifest + manifest.sig, VERIFY the Ed25519 signature against
//     the CLI-pinned pubkey, download the archive, VERIFY archive SHA-256 == manifest's, extract under
//     ~/.soroq/toolchains/<version>/, run the UNCHANGED verifyEngineBundle (via soroqctl) on the
//     extracted bundle, cache. Idempotent re-install = cache hit (offline OK).
//   - list: installed cached versions.
//   - doctor: availability + package/CLI-version compatibility + identity vs the committed canonical.
//
// Refusals (clear errors, no partial trust): bad signature, archive-hash mismatch, platform mismatch,
// flutter-version/revision mismatch, post-extract verifyEngineBundle failure. Never mutates unrelated
// Flutter installs (writes only under ~/.soroq/toolchains/). install touches NO credentials, so a
// local/non-prod --api can never refresh/rewrite the stored prod credential.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// toolchainPinnedPublicKeyHex is the CLI-pinned PUBLIC key for the toolchain trust domain
// (key id soroq-toolchain-kid-v1). The matching operator-held seed is supplied at publish time via
// SOROQ_TOOLCHAIN_SIGNING_SEED / --signing-key-file and is NEVER committed. install verifies every
// downloaded toolchain manifest signature against THIS key; an unknown/wrong key is refused.
//
// PRODUCTION KEY (rotated, T008/WS2). This is now a real production public key: its matching Ed25519
// seed was minted with `soroq toolchain keygen`, kept OUT-OF-BAND (a 0600 file moved to a secret store),
// and is NOT committed anywhere in this repo. The previously-pinned value was the pubkey of the COMMITTED
// test fixture testToolchainSeedB64 (toolchain_cmd_test.go) and was therefore NOT production-safe; it has
// been retired. The old test seed no longer matches this key, so publish refuses it (publish self-check,
// toolchain_publish.go) and install refuses any manifest signed by it. See
// docs/toolchain-signing-key-rotation.md.
const toolchainPinnedPublicKeyHex = "0c13a40b064f549f817c58a8d4a22b28a38cd0bde7133e73db0040071d770ca1"

// toolchainPinnedPublicKeyHexOverride is a TEST-ONLY seam. Production code reads the pinned toolchain
// public key through pinnedToolchainPublicKeyHex(), which returns this override when a test has installed
// an EPHEMERAL keypair (so accept-path tests sign + verify against a key whose seed is never committed),
// and otherwise returns the production const above. It is empty (and thus inert) in all production builds;
// nothing outside _test.go ever sets it. The production trust anchor stays the const — this is not a
// mutable production key.
var toolchainPinnedPublicKeyHexOverride string

// pinnedToolchainPublicKeyHex returns the in-process pinned toolchain public key: the production const,
// unless a test has installed an ephemeral override via toolchainPinnedPublicKeyHexOverride.
func pinnedToolchainPublicKeyHex() string {
	if toolchainPinnedPublicKeyHexOverride != "" {
		return toolchainPinnedPublicKeyHexOverride
	}
	return toolchainPinnedPublicKeyHex
}

// toolchainPinnedKeyID is the well-known key id paired with the pinned public key above.
const toolchainPinnedKeyID = "soroq-toolchain-kid-v1"

const toolchainManifestSchema = "soroq.toolchain.v1"

// Expected build-time identity the CLI is wired for. These mirror the committed canonical
// soroq.ios_engine.v1 engine.json (tools/soroq_toolchain_packer/canonical/). doctor + install use them
// to refuse a flutter/dart revision mismatch up front (a clear refusal before any extraction).
const (
	// iOS toolchain identity — the R3 matched revision (dart_dynamic_modules=true), device-proven
	// 2026-07-03 (T037). Superseded the stale c9a6c484/d684a576 patch-lane-only identity when the
	// revision-matched SOROQ iOS engine + build-lane at f74781f6/3499c008 was shipped and the full
	// hard-OTA flip (apply/rollback/tamper-refuse) was proven on a physical iPhone.
	expectedFlutterRevision = "f74781f6213447540225edae307acb48bbaaaf34"
	expectedDartRevision    = "9576691c37d84d3b66a9722e4fadacc764f04b21"
	expectedPlatform        = "ios"
)

// Expected Android build-time identity (T012), mirroring the committed soroq.android_engine.v1 canonical
// engine.json. The Android toolchain is CANDIDATE: flutter_revision (the engine head f74781f6…) is the
// one real upstream anchor; dart_revision is the dart-sdk VERSION STRING (no git SHA was supplied in the
// handoff), so the Android identity check accepts that literal value rather than demanding a 40-hex SHA.
const (
	expectedAndroidFlutterRevision = "f74781f6213447540225edae307acb48bbaaaf34"
	expectedAndroidDartRevision    = "3.13.0-103.1.beta"
	expectedAndroidArch            = "arm64-v8a"
)

// toolchainPlatformIdentity is the per-platform build-time identity the CLI accepts. install + doctor
// refuse a manifest whose flutter/dart revision does not match its platform's entry. iOS is the proven
// default; Android (candidate) is added without touching the iOS values. An UNKNOWN platform has no
// entry and is refused (the unsupported-platform guard).
type toolchainPlatformIdentity struct {
	flutterRevision string
	dartRevision    string
	bundleSubdir    string // cached-bundle subdir + tar prefix: "ios" | "android"
}

var toolchainPlatformIdentities = map[string]toolchainPlatformIdentity{
	"ios":     {flutterRevision: expectedFlutterRevision, dartRevision: expectedDartRevision, bundleSubdir: "ios"},
	"android": {flutterRevision: expectedAndroidFlutterRevision, dartRevision: expectedAndroidDartRevision, bundleSubdir: "android"},
}

// cliManifest is the CLI-side view of the packer's FLAT soroq.toolchain.v1 manifest. It matches the
// packer's schema (cmd/soroq-toolchain-pack/manifest.go) — NOT domain.ToolchainVersion (whose nested
// platforms[] would make these checks read empty strings and silently mis-pass).
type cliManifest struct {
	Schema                string                `json:"schema"`
	SoroqToolchainVersion string                `json:"soroq_toolchain_version"`
	Platform              string                `json:"platform"`
	Arch                  string                `json:"arch"`
	Mode                  string                `json:"mode"`
	Tier                  string                `json:"tier"`
	BuildMode             string                `json:"build_mode"`
	FlutterVersion        string                `json:"flutter_version"`
	FlutterRevision       string                `json:"flutter_revision"`
	DartRevision          string                `json:"dart_revision"`
	SoroqEngineRevision   string                `json:"soroq_engine_revision"`
	SoroqPatchHashes      map[string]string     `json:"soroq_patch_hashes"`
	Artifacts             []cliManifestArtifact `json:"artifacts"`
	Archive               cliManifestArchive    `json:"archive"`
	Compatibility         cliManifestCompat     `json:"compatibility"`
	Signatures            cliManifestSignatures `json:"signatures"`
}

type cliManifestArtifact struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Role   string `json:"role"`
	Kind   string `json:"kind"`
}

type cliManifestArchive struct {
	Path              string `json:"path"`
	URL               string `json:"url"`
	SHA256            string `json:"sha256"`
	CompressedBytes   int64  `json:"compressed_bytes"`
	UncompressedBytes int64  `json:"uncompressed_bytes"`
}

type cliManifestCompat struct {
	Platform        string `json:"platform"`
	Arch            string `json:"arch"`
	FlutterRevision string `json:"flutter_revision"`
	DartRevision    string `json:"dart_revision"`
	MinHostArch     string `json:"min_host_arch"`
}

type cliManifestSignatures struct {
	SigningKeyID string `json:"signing_key_id"`
	ManifestSig  string `json:"manifest_sig"`
}

// runToolchain is the `soroq toolchain <subcommand>` dispatcher.
func runToolchain(args []string) error {
	if len(args) == 0 {
		toolchainUsage()
		return errAlreadyPrinted
	}
	switch args[0] {
	case "keygen":
		return runToolchainKeygen(args[1:])
	case "publish":
		return runToolchainPublish(args[1:])
	case "install":
		return runToolchainInstall(args[1:])
	case "list":
		return runToolchainList(args[1:])
	case "doctor":
		return runToolchainDoctor(args[1:])
	case "-h", "--help", "help":
		toolchainUsage()
		return nil
	default:
		toolchainUsage()
		return errAlreadyPrinted
	}
}

func toolchainUsage() {
	fmt.Fprintln(os.Stderr, `usage: soroq toolchain <subcommand> [flags]

subcommands:
  keygen   operator: mint a fresh toolchain signing keypair (prints pubkey+keyid; seed -> 0600 file)
  publish  operator: sign + PUT a packer-produced toolchain manifest to the registry
  install  download, verify (signature + archive hash + verifyEngineBundle), and cache a toolchain
  list     list installed (cached) toolchain versions under ~/.soroq/toolchains/
  doctor   report toolchain availability + package/CLI-version compatibility`)
}

// toolchainsRoot returns ~/.soroq/toolchains (honoring SOROQ_CONFIG's HOME via os.UserHomeDir; a temp
// HOME in tests is respected). It NEVER touches any unrelated Flutter install.
func toolchainsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".soroq", "toolchains"), nil
}

func toolchainVersionDir(version string) (string, error) {
	if strings.TrimSpace(version) == "" || strings.Contains(version, "/") || strings.Contains(version, "..") {
		return "", fmt.Errorf("invalid toolchain version %q", version)
	}
	root, err := toolchainsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, version), nil
}

// parseCLIManifest unmarshals + sanity-checks the flat soroq.toolchain.v1 manifest bytes.
func parseCLIManifest(b []byte) (cliManifest, error) {
	var m cliManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse toolchain manifest: %w", err)
	}
	if m.Schema != toolchainManifestSchema {
		return m, fmt.Errorf("toolchain manifest schema %q != %q", m.Schema, toolchainManifestSchema)
	}
	if strings.TrimSpace(m.SoroqToolchainVersion) == "" {
		return m, fmt.Errorf("toolchain manifest missing soroq_toolchain_version")
	}
	return m, nil
}

func sha256OfFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := copyTo(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
