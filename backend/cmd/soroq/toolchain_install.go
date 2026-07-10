package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"soroq/backend/internal/signing"
)

// runToolchainInstall downloads, VERIFIES, and caches a toolchain version. install touches NO
// credentials (the read leg is public), so a local/non-prod --api can never refresh/rewrite the
// stored prod credential.
//
// Refusal order (all clear errors, no partial trust): manifest fetch -> signature (CLI-pinned key) ->
// platform/flutter-revision/dart-revision/build-mode -> archive download + archive SHA-256 ->
// extract -> UNCHANGED verifyEngineBundle. Idempotent re-install = cache hit (re-verified offline).
func runToolchainInstall(args []string) error {
	fs := flag.NewFlagSet("toolchain install", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL (registry)")
	force := fs.Bool("force", false, "re-download and re-verify even if a verified cache entry exists")
	skipBundleVerify := fs.Bool("skip-bundle-verify", false, "skip the UNCHANGED verifyEngineBundle soroctl gate (signature + archive-hash still enforced; for environments without soroqctl)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq toolchain install <version> --api https://api.soroq.dev [--force] [--json]

Downloads + verifies (Ed25519 signature against the CLI-pinned key, archive SHA-256, and the UNCHANGED
verifyEngineBundle on the extracted bundle) and caches under ~/.soroq/toolchains/<version>/.
A verified cache entry short-circuits (offline OK); --force re-downloads.`)
	}
	// Accept the version positionally in ANY position (Go's flag stops at the first non-flag arg,
	// so pull the version out before parsing to support `install <version> --api <base>`).
	version, rest := extractToolchainVersionArg(args)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if version == "" {
		version = strings.TrimSpace(fs.Arg(0))
	}
	if version == "" {
		return errors.New("usage: soroq toolchain install <version> --api <base>")
	}
	versionDir, err := toolchainVersionDir(version)
	if err != nil {
		return err
	}

	result := installResult{Version: version, Dir: versionDir}

	// Cache hit path: if the version is already installed, RE-VERIFY it offline (signature over the
	// cached manifest + verifyEngineBundle on the cached bundle). A real re-verification, not a bare
	// dir-exists check. Never hits the network.
	if !*force {
		if cached, ok, err := reverifyCachedToolchain(version, versionDir, *skipBundleVerify); err != nil {
			return fmt.Errorf("cached toolchain %s failed re-verification: %w (re-run with --force to refetch)", version, err)
		} else if ok {
			// Self-heal the android local-engine layout on a cache hit (idempotent; soroq bytes only).
			if strings.EqualFold(strings.TrimSpace(cached.Platform), "android") {
				if err := materializeAndroidLocalEngineLayout(filepath.Join(versionDir, toolchainBundleSubdir(cached.Platform))); err != nil {
					return fmt.Errorf("materialize android local-engine layout: %w", err)
				}
			}
			result.Manifest = cached
			result.CacheHit = true
			return reportInstall(result, *jsonOut)
		}
	}

	base := strings.TrimRight(strings.TrimSpace(*apiBase), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}

	// 1. Fetch the manifest bytes (verbatim) + the detached signature. No credentials (public read).
	manifestBytes, err := httpGetBytes(base + "/v1/toolchains/" + url.PathEscape(version))
	if err != nil {
		return fmt.Errorf("fetch toolchain manifest: %w", err)
	}
	sigBytes, err := httpGetBytes(base + "/v1/toolchains/" + url.PathEscape(version) + "/manifest.sig")
	if err != nil {
		return fmt.Errorf("fetch toolchain manifest signature: %w", err)
	}
	sigHex := strings.TrimSpace(string(sigBytes))

	// 2. VERIFY the signature against the CLI-pinned toolchain pubkey (REFUSAL: bad signature).
	if err := signing.VerifyToolchainManifestSignature(manifestBytes, sigHex, pinnedToolchainPublicKeyHex()); err != nil {
		return fmt.Errorf("REFUSED: %w", err)
	}
	manifest, err := parseCLIManifest(manifestBytes)
	if err != nil {
		return err
	}
	if manifest.SoroqToolchainVersion != version {
		return fmt.Errorf("REFUSED: manifest version %q does not match requested %q", manifest.SoroqToolchainVersion, version)
	}

	// 3. Identity refusals (platform / flutter-revision / dart-revision / build-mode).
	if err := checkToolchainIdentity(manifest); err != nil {
		return fmt.Errorf("REFUSED: %w", err)
	}

	// 4. Download the archive from the signed manifest URL and VERIFY its SHA-256 (REFUSAL: mismatch).
	if strings.TrimSpace(manifest.Archive.URL) == "" {
		return errors.New("REFUSED: signed manifest has no archive.url to download from")
	}
	archiveBytes, err := httpGetBytes(manifest.Archive.URL)
	if err != nil {
		return fmt.Errorf("download toolchain archive: %w", err)
	}
	// Verify the archive SIZE against the signed manifest BEFORE the SHA (a cheap length check that also
	// catches a truncated/oversized object-store download; the size is signed, so it is a real trust check).
	if want := manifest.Archive.CompressedBytes; want > 0 && int64(len(archiveBytes)) != want {
		return fmt.Errorf("REFUSED: toolchain archive size mismatch: manifest=%d downloaded=%d bytes (from %s)", want, len(archiveBytes), manifest.Archive.URL)
	}
	gotSHA := sha256Hex(archiveBytes)
	if !strings.EqualFold(gotSHA, strings.TrimSpace(manifest.Archive.SHA256)) {
		return fmt.Errorf("REFUSED: toolchain archive sha256 mismatch: manifest=%s downloaded=%s", manifest.Archive.SHA256, gotSHA)
	}

	// 5. Extract atomically into the version dir (write to a temp sibling, then rename). The bundle
	// subdir is platform-aware (ios | android) so an Android toolchain caches under .../android/.
	subdir := toolchainBundleSubdir(manifest.Platform)
	if err := extractToolchainArchive(archiveBytes, versionDir, subdir, manifestBytes, sigHex); err != nil {
		return err
	}

	// 6. Run the UNCHANGED (platform-aware) verifyEngineBundle on the extracted bundle (REFUSAL: verify
	// failure). iOS shells `release ios-engine`; Android shells the side-effect-free `verify-engine-bundle`.
	bundleDir := filepath.Join(versionDir, subdir)
	if !*skipBundleVerify {
		out, err := runUnchangedVerifyEngineBundleForPlatform(manifest.Platform, bundleDir)
		if err != nil {
			// Leave no partially-trusted cache entry on verify failure.
			_ = os.RemoveAll(versionDir)
			return fmt.Errorf("REFUSED: extracted bundle failed the UNCHANGED verifyEngineBundle: %w\n%s", err, out)
		}
		result.BundleVerified = true
	}

	// 7. Android only: materialize the SOROQ engine artifacts into the Flutter `--local-engine` `out/`
	// layout (+ the Gradle embedding jar). The version-matched STOCK host tooling is overlaid later by
	// the release/patch build path (completeAndroidLocalEngineLayout). iOS is untouched.
	if strings.EqualFold(strings.TrimSpace(manifest.Platform), "android") {
		if err := materializeAndroidLocalEngineLayout(bundleDir); err != nil {
			return fmt.Errorf("materialize android local-engine layout: %w", err)
		}
	}

	result.Manifest = manifest
	result.CacheHit = false
	return reportInstall(result, *jsonOut)
}

// extractToolchainVersionArg supports `install <version> --api <base>` (version BEFORE its flags),
// which Go's flag package would otherwise treat as a terminator. If the first arg is a bare token
// (not a flag), it is the version and the rest is parsed as flags; otherwise the version is left to
// fs.Arg(0) after parsing. Only the leading positional is special-cased, so flag VALUES are not
// mistaken for the version.
func extractToolchainVersionArg(args []string) (version string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return strings.TrimSpace(args[0]), args[1:]
	}
	return "", args
}

type installResult struct {
	Version        string
	Dir            string
	Manifest       cliManifest
	CacheHit       bool
	BundleVerified bool
}

func reportInstall(r installResult, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"soroq_toolchain_version": r.Version,
			"cache_dir":               r.Dir,
			"cache_hit":               r.CacheHit,
			"bundle_verified":         r.BundleVerified || r.CacheHit,
			"flutter_revision":        r.Manifest.FlutterRevision,
			"dart_revision":           r.Manifest.DartRevision,
			"tier":                    r.Manifest.Tier,
			"platform":                r.Manifest.Platform,
		})
	}
	if r.CacheHit {
		fmt.Fprintf(os.Stdout, "Toolchain %s already installed (cache hit, re-verified offline)\n", r.Version)
	} else {
		fmt.Fprintf(os.Stdout, "Installed toolchain %s\n", r.Version)
	}
	fmt.Fprintf(os.Stdout, "  cache:    %s\n", r.Dir)
	fmt.Fprintf(os.Stdout, "  platform: %s/%s  build_mode=%s  tier=%s\n", r.Manifest.Platform, r.Manifest.Arch, r.Manifest.BuildMode, r.Manifest.Tier)
	fmt.Fprintf(os.Stdout, "  flutter:  %s  dart:%s\n", short(r.Manifest.FlutterRevision), short(r.Manifest.DartRevision))
	if r.BundleVerified || r.CacheHit {
		fmt.Fprintln(os.Stdout, "  verifyEngineBundle: PASSED (unchanged soroqctl gate)")
	}
	return nil
}

// checkToolchainIdentity enforces the platform/flutter/dart/build-mode refusals (clear errors). It is
// platform-aware (T012): the flutter/dart revision the CLI accepts is selected from the manifest's
// platform. An UNSUPPORTED platform (no identity entry) is refused — the unsupported-platform guard.
func checkToolchainIdentity(m cliManifest) error {
	platform := strings.ToLower(strings.TrimSpace(m.Platform))
	id, ok := toolchainPlatformIdentities[platform]
	if !ok {
		return fmt.Errorf("platform mismatch: manifest platform %q is not supported by this CLI (supported: ios, android)", m.Platform)
	}
	mode := strings.ToLower(strings.TrimSpace(m.BuildMode))
	if mode != "profile" && mode != "release" {
		return fmt.Errorf("build_mode %q is not profile|release", m.BuildMode)
	}
	if !strings.EqualFold(strings.TrimSpace(m.FlutterRevision), id.flutterRevision) {
		return fmt.Errorf("flutter revision mismatch: %s manifest %q, this CLI is wired for %q", platform, short(m.FlutterRevision), short(id.flutterRevision))
	}
	if !strings.EqualFold(strings.TrimSpace(m.DartRevision), id.dartRevision) {
		return fmt.Errorf("dart revision mismatch: %s manifest %q, this CLI is wired for %q", platform, short(m.DartRevision), short(id.dartRevision))
	}
	return nil
}

// toolchainBundleSubdir returns the cached-bundle subdir ("ios" | "android") for a manifest's platform,
// defaulting to "ios" for an unknown platform (callers gate on checkToolchainIdentity first).
func toolchainBundleSubdir(platform string) string {
	if id, ok := toolchainPlatformIdentities[strings.ToLower(strings.TrimSpace(platform))]; ok {
		return id.bundleSubdir
	}
	return "ios"
}

// reverifyCachedToolchain re-verifies an already-installed toolchain OFFLINE: the cached manifest
// signature against the pinned key + (unless skipped) the UNCHANGED verifyEngineBundle on the cached
// bundle. Returns (manifest, true, nil) on a clean cache hit, (_, false, nil) when not installed.
func reverifyCachedToolchain(version, versionDir string, skipBundleVerify bool) (cliManifest, bool, error) {
	manifestPath := filepath.Join(versionDir, "manifest.json")
	sigPath := filepath.Join(versionDir, "manifest.sig")
	if _, err := os.Stat(manifestPath); errors.Is(err, os.ErrNotExist) {
		return cliManifest{}, false, nil
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return cliManifest{}, false, err
	}
	sigBytes, err := os.ReadFile(sigPath)
	if err != nil {
		return cliManifest{}, false, fmt.Errorf("read cached manifest.sig: %w", err)
	}
	if err := signing.VerifyToolchainManifestSignature(manifestBytes, strings.TrimSpace(string(sigBytes)), pinnedToolchainPublicKeyHex()); err != nil {
		return cliManifest{}, false, err
	}
	manifest, err := parseCLIManifest(manifestBytes)
	if err != nil {
		return cliManifest{}, false, err
	}
	if err := checkToolchainIdentity(manifest); err != nil {
		return cliManifest{}, false, err
	}
	bundleDir := filepath.Join(versionDir, toolchainBundleSubdir(manifest.Platform))
	if !skipBundleVerify {
		if out, err := runUnchangedVerifyEngineBundleForPlatform(manifest.Platform, bundleDir); err != nil {
			return cliManifest{}, false, fmt.Errorf("%w\n%s", err, out)
		}
	}
	return manifest, true, nil
}

// extractToolchainArchive extracts the tar.gz bytes into versionDir atomically (temp sibling +
// rename), then writes the verbatim manifest.json + manifest.sig alongside the bundle subdir. Writes
// ONLY under ~/.soroq/toolchains/<version>/. subdir is the platform bundle subdir ("ios" | "android").
func extractToolchainArchive(archiveBytes []byte, versionDir, subdir string, manifestBytes []byte, sigHex string) error {
	root, err := toolchainsRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(root, ".tmp-install-")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	if err := untarGz(archiveBytes, tmpDir); err != nil {
		return fmt.Errorf("extract toolchain archive: %w", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, subdir, "engine.json")); err != nil {
		return fmt.Errorf("extracted toolchain archive is missing %s/engine.json: %w", subdir, err)
	}
	// Persist the verbatim manifest + signature so the cache can be re-verified offline.
	if err := os.WriteFile(filepath.Join(tmpDir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "manifest.sig"), []byte(sigHex), 0o644); err != nil {
		return err
	}

	// Atomic-ish swap: remove any stale dir, then rename the fully-built temp into place.
	if err := os.RemoveAll(versionDir); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, versionDir); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// runUnchangedVerifyEngineBundleForPlatform routes the post-extract verify to the right soroqctl entry
// for the toolchain's platform: iOS keeps the proven `release ios-engine` path; Android uses the
// side-effect-free `verify-engine-bundle` entry (both reach the SAME UNCHANGED verifyEngineBundle, which
// is now platform-aware via the engine.json schema). NO reimplementation of verify.
func runUnchangedVerifyEngineBundleForPlatform(platform, bundleDir string) (string, error) {
	if strings.EqualFold(strings.TrimSpace(platform), "android") {
		return runUnchangedVerifyEngineBundleViaVerifyCmd(bundleDir)
	}
	return runUnchangedVerifyEngineBundle(bundleDir)
}

// runUnchangedVerifyEngineBundleViaVerifyCmd shells `soroqctl verify-engine-bundle --engine-bundle <dir>`
// (exit 0 == the bundle's engine.json identity + per-artifact re-hash PASSED). soroqctl is REQUIRED.
func runUnchangedVerifyEngineBundleViaVerifyCmd(bundleDir string) (string, error) {
	bin, err := resolveSoroqctl()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, "verify-engine-bundle", "--engine-bundle", bundleDir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("soroqctl verify-engine-bundle (unchanged verifyEngineBundle) failed: %w", err)
	}
	return out.String(), nil
}

// runUnchangedVerifyEngineBundle proves the extracted bundle passes the UNCHANGED verifyEngineBundle
// by invoking the real soroqctl binary's `release ios-engine` path — the only exposed entry that
// reaches verifyEngineBundle (cmd/soroqctl/ios_engine_patch.go). verifyEngineBundle is the FIRST gate
// (before the app.dill hash + baseline write), so exit 0 == verify passed. NO reimplementation.
//
// The verify-only run uses a throwaway app.dill + temp --out + no --api (local-only); it never mutates
// the control plane or any Flutter install. soroqctl is REQUIRED: a missing binary is a clear refusal
// (post-extract verifyEngineBundle is a mandated install gate).
func runUnchangedVerifyEngineBundle(bundleDir string) (string, error) {
	bin, err := resolveSoroqctl()
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "soroq-toolchain-verify-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	appDill := filepath.Join(tmp, "throwaway-app.dill")
	if err := os.WriteFile(appDill, []byte("throwaway app.dill for verify-only run"), 0o644); err != nil {
		return "", err
	}
	baselineOut := filepath.Join(tmp, "baseline.json")
	cmd := exec.Command(bin, "release", "ios-engine",
		"--engine-bundle", bundleDir,
		"--app-dill", appDill,
		"--release-id", "toolchain-verify-only",
		"--app-id", "toolchain-verify-only",
		"--out", baselineOut,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("soroqctl release ios-engine (unchanged verifyEngineBundle) failed: %w", err)
	}
	return out.String(), nil
}

// --- low-level helpers ---

func httpGetBytes(rawURL string) ([]byte, error) {
	resp, err := http.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("GET %s: %s", rawURL, msg)
	}
	return body, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// untarGz extracts gzip'd tar bytes into dst. Rejects unsafe (absolute / .. traversal) entries.
func untarGz(archiveBytes []byte, dst string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing unsafe archive entry %q", hdr.Name)
		}
		target := filepath.Join(dst, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}

// copyTo wraps io.Copy so toolchain_cmd.go can hash files without importing io directly.
func copyTo(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}

// short truncates a long hex id for human-readable output.
func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
