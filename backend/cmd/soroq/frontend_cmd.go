package main

// soroq frontend — hosted prebuilt Soroq Flutter FRONTEND lifecycle (D1.2, Option B).
//
// The frontend is the Soroq Flutter fork's `flutter-sdk-src` tree (the canonical asset bundler for
// soroq/soroq_metadata.json + runtime_id, and the `bin/flutter` every Android/iOS build shells out to).
// Before D1.2 a developer had to set SOROQ_FLUTTER_BIN by hand or keep a ~/development checkout. Now:
//
//   - publish (operator): Ed25519-sign the EXACT (augmented) frontend manifest bytes with the SAME operator
//     TOOLCHAIN key (no new trust anchor), PUT manifest+sig to /v1/frontends/{version}, upload the ~1 GB
//     archive DIRECTLY to object storage (chunked) + finalize its metadata through the control plane.
//   - install <version> --api <base>: GET manifest + manifest.sig, VERIFY the Ed25519 signature against the
//     CLI-pinned toolchain pubkey, download the archive, VERIFY archive SHA-256 + size BEFORE extract,
//     atomically extract under ~/.soroq/frontends/<version>/, clear the macOS quarantine xattr on the
//     extracted bin, and record the active version. Idempotent re-install = verified cache hit.
//   - path / list / doctor: report + verify the installed frontend.
//
// Refusals (clear errors, no partial trust): bad signature, archive size/hash mismatch, flutter-revision
// mismatch, missing bin/flutter or soroq_metadata.dart after extract. Writes ONLY under ~/.soroq/frontends/.

import (
	"archive/tar"
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
	"runtime"
	"strings"
	"time"

	"soroq/backend/internal/signing"
)

const frontendManifestSchema = "soroq.frontend.v1"

// defaultFrontendSubdir is the top-level directory inside the frontend archive (the tar was built with
// `-C ~/development/soroq-forks flutter-sdk-src`, so every entry is under flutter-sdk-src/). bin/flutter
// therefore lives at <versionDir>/flutter-sdk-src/bin/flutter. A manifest may override via frontend_subdir.
const defaultFrontendSubdir = "flutter-sdk-src"

// frontendManifest is the CLI + operator view of the signed soroq.frontend.v1 manifest. The backend
// projects domain.FrontendVersion from the same bytes (matching json tags) for its indexed table.
type frontendManifest struct {
	Schema                 string                  `json:"schema"`
	SoroqFrontendVersion   string                  `json:"soroq_frontend_version"`
	FlutterRevision        string                  `json:"flutter_revision"`
	DartRevision           string                  `json:"dart_revision"`
	EngineRevision         string                  `json:"engine_revision"`
	PatchsetSHA256         string                  `json:"patchset_sha256"`
	Archive                frontendManifestArchive `json:"archive"`
	CreatedAt              string                  `json:"created_at"`
	SigningKeyID           string                  `json:"signing_key_id"`
	CompatibleToolchainIDs []string                `json:"compatible_toolchain_ids"`
	FrontendSubdir         string                  `json:"frontend_subdir,omitempty"`
}

type frontendManifestArchive struct {
	Path              string `json:"path,omitempty"`
	URL               string `json:"url"`
	SHA256            string `json:"sha256"`
	CompressedBytes   int64  `json:"compressed_bytes"`
	UncompressedBytes int64  `json:"uncompressed_bytes"`
}

func (m frontendManifest) subdir() string {
	if s := strings.TrimSpace(m.FrontendSubdir); s != "" {
		return s
	}
	return defaultFrontendSubdir
}

// activeFrontend is the recorded active frontend install (~/.soroq/frontends/active.json). FlutterBin is
// stored absolute so resolveSoroqFlutterBin is a trivial stat, with the version kept for reporting.
type activeFrontend struct {
	Version       string    `json:"version"`
	FlutterBin    string    `json:"flutter_bin"`
	ArchiveSHA256 string    `json:"archive_sha256"`
	InstalledAt   time.Time `json:"installed_at"`
}

// runFrontend is the `soroq frontend <subcommand>` dispatcher.
func runFrontend(args []string) error {
	if len(args) == 0 {
		frontendUsage()
		return errAlreadyPrinted
	}
	switch args[0] {
	case "install":
		return runFrontendInstall(args[1:])
	case "publish":
		return runFrontendPublish(args[1:])
	case "list":
		return runFrontendList(args[1:])
	case "path":
		return runFrontendPath(args[1:])
	case "doctor":
		return runFrontendDoctor(args[1:])
	case "-h", "--help", "help":
		frontendUsage()
		return nil
	default:
		frontendUsage()
		return errAlreadyPrinted
	}
}

func frontendUsage() {
	fmt.Fprintln(os.Stderr, `usage: soroq frontend <subcommand> [flags]

subcommands:
  install  download, verify (signature + archive sha256/size), and install a Soroq Flutter frontend
  publish  operator: sign + PUT a frontend manifest + upload the archive to the registry
  list     list installed frontends under ~/.soroq/frontends/
  path     print the resolved installed frontend bin/flutter
  doctor   verify the installed frontend (revision, soroq_metadata.dart, recorded hash)`)
}

// --- paths ---

func frontendsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".soroq", "frontends"), nil
}

func frontendVersionDir(version string) (string, error) {
	if strings.TrimSpace(version) == "" || strings.Contains(version, "/") || strings.Contains(version, "..") {
		return "", fmt.Errorf("invalid frontend version %q", version)
	}
	root, err := frontendsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, version), nil
}

func activeFrontendPath() (string, error) {
	root, err := frontendsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "active.json"), nil
}

func loadActiveFrontend() (activeFrontend, bool, error) {
	path, err := activeFrontendPath()
	if err != nil {
		return activeFrontend{}, false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return activeFrontend{}, false, nil
	}
	if err != nil {
		return activeFrontend{}, false, err
	}
	var a activeFrontend
	if err := json.Unmarshal(b, &a); err != nil {
		return activeFrontend{}, false, err
	}
	return a, strings.TrimSpace(a.Version) != "", nil
}

func recordActiveFrontend(a activeFrontend) error {
	path, err := activeFrontendPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// resolveInstalledFrontendFlutterBin returns the bin/flutter of the recorded active frontend install, or
// ("", nil) when none is installed. It re-derives the path from the version dir (robust to a moved bin) and
// falls back to the recorded absolute FlutterBin.
func resolveInstalledFrontendFlutterBin() (string, error) {
	active, ok, err := loadActiveFrontend()
	if err != nil || !ok {
		return "", err
	}
	candidates := []string{}
	if versionDir, err := frontendVersionDir(active.Version); err == nil {
		candidates = append(candidates,
			filepath.Join(versionDir, defaultFrontendSubdir, "bin", "flutter"))
	}
	if strings.TrimSpace(active.FlutterBin) != "" {
		candidates = append(candidates, active.FlutterBin)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", nil
}

// parseFrontendManifest unmarshals + sanity-checks the signed soroq.frontend.v1 manifest bytes.
func parseFrontendManifest(b []byte) (frontendManifest, error) {
	var m frontendManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse frontend manifest: %w", err)
	}
	if m.Schema != frontendManifestSchema {
		return m, fmt.Errorf("frontend manifest schema %q != %q", m.Schema, frontendManifestSchema)
	}
	if strings.TrimSpace(m.SoroqFrontendVersion) == "" {
		return m, fmt.Errorf("frontend manifest missing soroq_frontend_version")
	}
	return m, nil
}

// checkFrontendIdentity refuses a manifest whose flutter_revision does not match the revision this CLI is
// wired for (the strong upstream anchor; the frontend must match the toolchain/engine pair).
func checkFrontendIdentity(m frontendManifest) error {
	if !strings.EqualFold(strings.TrimSpace(m.FlutterRevision), expectedFlutterRevision) {
		return fmt.Errorf("flutter revision mismatch: manifest %q, this CLI is wired for %q",
			short(m.FlutterRevision), short(expectedFlutterRevision))
	}
	return nil
}

// --- install ---

func runFrontendInstall(args []string) error {
	fs := flag.NewFlagSet("frontend install", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL (registry)")
	force := fs.Bool("force", false, "clean reinstall even if a verified install exists")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq frontend install <version> --api https://api.soroq.dev [--force] [--json]

Downloads + verifies (Ed25519 signature against the pinned toolchain key, archive SHA-256 + size) and
installs the Soroq Flutter frontend under ~/.soroq/frontends/<version>/, then records it active so builds
auto-detect it (no SOROQ_FLUTTER_BIN). A verified existing install short-circuits; --force reinstalls.`)
	}
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
		return errors.New("usage: soroq frontend install <version> --api <base>")
	}
	versionDir, err := frontendVersionDir(version)
	if err != nil {
		return err
	}

	// Cache hit path: re-verify the cached manifest signature offline + confirm bin/flutter is present.
	if !*force {
		if m, ok, err := reverifyInstalledFrontend(version, versionDir); err != nil {
			return fmt.Errorf("existing frontend %s failed re-verification: %w (re-run with --force to refetch)", version, err)
		} else if ok {
			if err := ensureActiveFrontend(version, versionDir, m); err != nil {
				return err
			}
			return reportFrontendInstall(version, versionDir, m, true, *jsonOut)
		}
	}

	base := strings.TrimRight(strings.TrimSpace(*apiBase), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}

	// 1. Fetch the manifest bytes (verbatim) + the detached signature. No credentials (public read).
	manifestBytes, err := httpGetBytes(base + "/v1/frontends/" + url.PathEscape(version))
	if err != nil {
		return fmt.Errorf("fetch frontend manifest: %w", err)
	}
	sigBytes, err := httpGetBytes(base + "/v1/frontends/" + url.PathEscape(version) + "/manifest.sig")
	if err != nil {
		return fmt.Errorf("fetch frontend manifest signature: %w", err)
	}
	sigHex := strings.TrimSpace(string(sigBytes))

	// 2. VERIFY the signature against the pinned toolchain pubkey (REFUSAL: bad signature).
	if err := signing.VerifyToolchainManifestSignature(manifestBytes, sigHex, pinnedToolchainPublicKeyHex()); err != nil {
		return fmt.Errorf("REFUSED: %w", err)
	}
	manifest, err := parseFrontendManifest(manifestBytes)
	if err != nil {
		return err
	}
	if manifest.SoroqFrontendVersion != version {
		return fmt.Errorf("REFUSED: manifest version %q does not match requested %q", manifest.SoroqFrontendVersion, version)
	}
	if err := checkFrontendIdentity(manifest); err != nil {
		return fmt.Errorf("REFUSED: %w", err)
	}
	if strings.TrimSpace(manifest.Archive.URL) == "" {
		return errors.New("REFUSED: signed manifest has no archive.url to download from")
	}
	if len(manifest.Archive.SHA256) != 64 {
		return errors.New("REFUSED: signed manifest archive.sha256 is not a 64-hex digest")
	}

	// 3. Stream the archive to a temp file (no 512MB cap; bounded memory) and VERIFY size + SHA-256 BEFORE
	//    extract (REFUSAL: mismatch). The archive is ~1 GB, so it is never held whole in memory.
	root, err := frontendsRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

	// Preflight (STDERR): print size/dest and ABORT before downloading if the target filesystem lacks
	// the PEAK footprint (temp archive + extracted temp dir). Runs BEFORE any byte is fetched.
	if err := runInstallPreflight(preflightInfo{
		label:             "frontend",
		version:           version,
		destDir:           versionDir,
		checkDir:          root,
		compressedBytes:   manifest.Archive.CompressedBytes,
		uncompressedBytes: manifest.Archive.UncompressedBytes,
		force:             *force,
	}); err != nil {
		return err
	}

	tmpArchive, err := os.CreateTemp(root, ".frontend-archive-*.tar.gz")
	if err != nil {
		return err
	}
	tmpArchivePath := tmpArchive.Name()
	defer os.Remove(tmpArchivePath)
	// Banner + live progress go to STDERR only (never STDOUT), and progress is suppressed under --json or
	// a non-TTY so machine output on stdout is never corrupted.
	live := stderrIsTTY() && !*jsonOut
	fmt.Fprintf(os.Stderr, "Downloading frontend archive (%s, %s)…\n", manifest.SoroqFrontendVersion, humanBytes(manifest.Archive.CompressedBytes))
	prog := newProgressReporter(manifest.Archive.CompressedBytes, live)
	gotSHA, gotSize, err := streamDownloadToFile(manifest.Archive.URL, tmpArchive, prog)
	tmpArchive.Close()
	if err != nil {
		return fmt.Errorf("download frontend archive: %w", err)
	}
	prog.finish()
	if want := manifest.Archive.CompressedBytes; want > 0 && gotSize != want {
		return fmt.Errorf("REFUSED: frontend archive size mismatch: manifest=%d downloaded=%d bytes (from %s)", want, gotSize, manifest.Archive.URL)
	}
	if !strings.EqualFold(gotSHA, strings.TrimSpace(manifest.Archive.SHA256)) {
		return fmt.Errorf("REFUSED: frontend archive sha256 mismatch: manifest=%s downloaded=%s", manifest.Archive.SHA256, gotSHA)
	}

	// 4. Extract atomically (temp sibling dir -> verify -> rename). untarGzReader streams from the file.
	if err := extractFrontendArchive(tmpArchivePath, versionDir, manifest, manifestBytes, sigHex); err != nil {
		return err
	}

	// 5. Clear the macOS quarantine xattr on the extracted bin so the frontend's binaries run.
	clearQuarantine(filepath.Join(versionDir, manifest.subdir(), "bin"))

	// 6. Record active so resolveSoroqFlutterBin auto-detects it (no SOROQ_FLUTTER_BIN needed).
	if err := ensureActiveFrontend(version, versionDir, manifest); err != nil {
		return err
	}
	return reportFrontendInstall(version, versionDir, manifest, false, *jsonOut)
}

// reverifyInstalledFrontend re-verifies an already-installed frontend OFFLINE: the cached manifest signature
// against the pinned key + bin/flutter present. Returns (manifest, true, nil) on a clean hit.
func reverifyInstalledFrontend(version, versionDir string) (frontendManifest, bool, error) {
	manifestPath := filepath.Join(versionDir, "manifest.json")
	if _, err := os.Stat(manifestPath); errors.Is(err, os.ErrNotExist) {
		return frontendManifest{}, false, nil
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return frontendManifest{}, false, err
	}
	sigBytes, err := os.ReadFile(filepath.Join(versionDir, "manifest.sig"))
	if err != nil {
		return frontendManifest{}, false, fmt.Errorf("read cached manifest.sig: %w", err)
	}
	if err := signing.VerifyToolchainManifestSignature(manifestBytes, strings.TrimSpace(string(sigBytes)), pinnedToolchainPublicKeyHex()); err != nil {
		return frontendManifest{}, false, err
	}
	manifest, err := parseFrontendManifest(manifestBytes)
	if err != nil {
		return frontendManifest{}, false, err
	}
	binPath := filepath.Join(versionDir, manifest.subdir(), "bin", "flutter")
	if info, err := os.Stat(binPath); err != nil || info.IsDir() {
		return frontendManifest{}, false, fmt.Errorf("installed frontend missing %s", binPath)
	}
	return manifest, true, nil
}

func ensureActiveFrontend(version, versionDir string, m frontendManifest) error {
	return recordActiveFrontend(activeFrontend{
		Version:       version,
		FlutterBin:    filepath.Join(versionDir, m.subdir(), "bin", "flutter"),
		ArchiveSHA256: strings.ToLower(strings.TrimSpace(m.Archive.SHA256)),
		InstalledAt:   time.Now().UTC(),
	})
}

// extractFrontendArchive streams the tar.gz file into versionDir atomically (temp sibling + rename), then
// writes the verbatim manifest.json + manifest.sig alongside. Verifies bin/flutter + soroq_metadata.dart
// landed. Writes ONLY under ~/.soroq/frontends/<version>/.
func extractFrontendArchive(archivePath, versionDir string, m frontendManifest, manifestBytes []byte, sigHex string) error {
	root, err := frontendsRoot()
	if err != nil {
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

	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := untarGzReader(f, tmpDir); err != nil {
		return fmt.Errorf("extract frontend archive: %w", err)
	}

	subdir := m.subdir()
	binFlutter := filepath.Join(tmpDir, subdir, "bin", "flutter")
	if info, err := os.Stat(binFlutter); err != nil || info.IsDir() {
		return fmt.Errorf("extracted frontend archive is missing %s/bin/flutter", subdir)
	}
	metadataDart := filepath.Join(tmpDir, subdir, "packages", "flutter_tools", "lib", "src", "soroq_metadata.dart")
	if _, err := os.Stat(metadataDart); err != nil {
		return fmt.Errorf("extracted frontend archive is missing the Soroq asset bundler (%s/packages/flutter_tools/lib/src/soroq_metadata.dart): %w", subdir, err)
	}

	if err := os.WriteFile(filepath.Join(tmpDir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "manifest.sig"), []byte(sigHex), 0o644); err != nil {
		return err
	}

	if err := os.RemoveAll(versionDir); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, versionDir); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func reportFrontendInstall(version, versionDir string, m frontendManifest, cacheHit bool, jsonOut bool) error {
	binPath := filepath.Join(versionDir, m.subdir(), "bin", "flutter")
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"soroq_frontend_version": version,
			"install_dir":            versionDir,
			"flutter_bin":            binPath,
			"cache_hit":              cacheHit,
			"flutter_revision":       m.FlutterRevision,
			"dart_revision":          m.DartRevision,
			"engine_revision":        m.EngineRevision,
		})
	}
	if cacheHit {
		fmt.Fprintf(os.Stdout, "Frontend %s already installed (verified)\n", version)
	} else {
		fmt.Fprintf(os.Stdout, "Installed frontend %s\n", version)
	}
	fmt.Fprintf(os.Stdout, "  install:  %s\n", versionDir)
	fmt.Fprintf(os.Stdout, "  flutter:  %s\n", binPath)
	fmt.Fprintf(os.Stdout, "  revision: flutter=%s dart=%s engine=%s\n", short(m.FlutterRevision), m.DartRevision, short(m.EngineRevision))
	fmt.Fprintln(os.Stdout, "  active:   builds will auto-detect this frontend (no SOROQ_FLUTTER_BIN)")
	return nil
}

// --- path / list ---

func runFrontendPath(args []string) error {
	fs := flag.NewFlagSet("frontend path", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { fmt.Fprintln(os.Stdout, `usage: soroq frontend path`) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	bin, err := resolveInstalledFrontendFlutterBin()
	if err != nil {
		return err
	}
	if bin == "" {
		return errors.New("no frontend installed; run `soroq frontend install <version> --api <base>`")
	}
	fmt.Fprintln(os.Stdout, bin)
	return nil
}

func runFrontendList(args []string) error {
	fs := flag.NewFlagSet("frontend list", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() { fmt.Fprintln(os.Stdout, `usage: soroq frontend list [--json]`) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	root, err := frontendsRoot()
	if err != nil {
		return err
	}
	active, _, _ := loadActiveFrontend()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return err
	}
	type listed struct {
		Version        string `json:"version"`
		Dir            string `json:"dir"`
		Active         bool   `json:"active"`
		SignatureValid bool   `json:"signature_valid"`
		Note           string `json:"note,omitempty"`
	}
	var out []listed
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		versionDir := filepath.Join(root, e.Name())
		l := listed{Version: e.Name(), Dir: versionDir, Active: e.Name() == active.Version}
		if _, ok, err := reverifyInstalledFrontend(e.Name(), versionDir); err != nil {
			l.Note = err.Error()
		} else if ok {
			l.SignatureValid = true
		} else {
			l.Note = "incomplete install"
		}
		out = append(out, l)
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if len(out) == 0 {
		fmt.Fprintf(os.Stdout, "No frontends installed under %s\n", root)
		fmt.Fprintln(os.Stdout, "  install one: soroq frontend install <version> --api <base>")
		return nil
	}
	fmt.Fprintf(os.Stdout, "Installed frontends (%s):\n", root)
	for _, l := range out {
		marker := "✓"
		if !l.SignatureValid {
			marker = "✗"
		}
		active := ""
		if l.Active {
			active = "  (active)"
		}
		fmt.Fprintf(os.Stdout, "  %s %s%s\n", marker, l.Version, active)
		if l.Note != "" {
			fmt.Fprintf(os.Stdout, "      note: %s\n", l.Note)
		}
	}
	return nil
}

// --- doctor ---

func runFrontendDoctor(args []string) error {
	fs := flag.NewFlagSet("frontend doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() { fmt.Fprintln(os.Stdout, `usage: soroq frontend doctor [--json]`) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	report := doctorReport{}
	report.Checks = append(report.Checks, frontendInstalledCheck())

	okCount := 0
	for _, c := range report.Checks {
		switch c.Status {
		case "warn":
			report.Warnings++
		case "error":
			report.Errors++
		case "ok":
			okCount++
		}
	}
	report.OK = report.Errors == 0

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
		if report.Errors > 0 {
			return errAlreadyPrinted
		}
		return nil
	}
	fmt.Fprintln(os.Stdout, "soroq frontend doctor")
	fmt.Fprintln(os.Stdout)
	for _, c := range report.Checks {
		fmt.Fprintf(os.Stdout, "%s %s", doctorIcon(c.Status), c.Name)
		if c.Message != "" {
			fmt.Fprintf(os.Stdout, ": %s", c.Message)
		}
		fmt.Fprintln(os.Stdout)
		if c.Fix != "" {
			fmt.Fprintf(os.Stdout, "   → %s\n", c.Fix)
		}
	}
	fmt.Fprintf(os.Stdout, "\n%d ok, %d warning(s), %d error(s)\n", okCount, report.Warnings, report.Errors)
	if report.Errors > 0 {
		return errAlreadyPrinted
	}
	return nil
}

// frontendInstalledCheck HARD-FAILS when no frontend is installed, and verifies the installed one: cached
// signature, `bin/flutter --version` revision == pinned f74781f621, soroq_metadata.dart present, and the
// recorded active archive sha matches the cached manifest.
func frontendInstalledCheck() doctorCheck {
	active, ok, err := loadActiveFrontend()
	if err != nil {
		return doctorCheck{Name: "Soroq Flutter frontend", Status: "error", Message: err.Error()}
	}
	if !ok {
		return doctorCheck{
			Name:    "Soroq Flutter frontend",
			Status:  "error",
			Message: "no frontend installed",
			Fix:     "soroq frontend install <version> --api " + defaultControlPlaneAPI,
		}
	}
	versionDir, err := frontendVersionDir(active.Version)
	if err != nil {
		return doctorCheck{Name: "Soroq Flutter frontend", Status: "error", Message: err.Error()}
	}
	manifest, valid, err := reverifyInstalledFrontend(active.Version, versionDir)
	if err != nil {
		return doctorCheck{Name: "Soroq Flutter frontend", Status: "error", Message: "verification failed: " + err.Error(),
			Fix: "soroq frontend install " + active.Version + " --force --api " + defaultControlPlaneAPI}
	}
	if !valid {
		return doctorCheck{Name: "Soroq Flutter frontend", Status: "error", Message: "install incomplete",
			Fix: "soroq frontend install " + active.Version + " --force --api " + defaultControlPlaneAPI}
	}
	if !strings.EqualFold(strings.ToLower(strings.TrimSpace(manifest.Archive.SHA256)), strings.ToLower(strings.TrimSpace(active.ArchiveSHA256))) {
		return doctorCheck{Name: "Soroq Flutter frontend", Status: "error",
			Message: "recorded active archive sha does not match the cached manifest"}
	}
	binPath := filepath.Join(versionDir, manifest.subdir(), "bin", "flutter")
	metadataDart := filepath.Join(versionDir, manifest.subdir(), "packages", "flutter_tools", "lib", "src", "soroq_metadata.dart")
	if _, err := os.Stat(metadataDart); err != nil {
		return doctorCheck{Name: "Soroq Flutter frontend", Status: "error",
			Message: "missing the Soroq asset bundler soroq_metadata.dart"}
	}
	rev, err := flutterRevisionOf(binPath)
	if err != nil {
		return doctorCheck{Name: "Soroq Flutter frontend", Status: "error", Message: "bin/flutter --version failed: " + err.Error()}
	}
	if !strings.HasPrefix(strings.TrimSpace(rev), strings.TrimSpace(short(expectedFlutterRevision))) {
		return doctorCheck{Name: "Soroq Flutter frontend", Status: "error",
			Message: fmt.Sprintf("bin/flutter revision %s != pinned %s", short(rev), short(expectedFlutterRevision))}
	}
	return doctorCheck{Name: "Soroq Flutter frontend", Status: "ok",
		Message: fmt.Sprintf("%s installed + signature-valid + revision %s (%s)", active.Version, short(rev), binPath)}
}

// flutterRevisionOf runs `<binPath> --version --machine` and returns the frameworkRevision. Falls back to
// parsing the plain `--version` output. Never mutates any Flutter install.
func flutterRevisionOf(binPath string) (string, error) {
	cmd := exec.Command(binPath, "--version", "--machine")
	out, err := cmd.Output()
	if err == nil {
		var parsed struct {
			FrameworkRevision string `json:"frameworkRevision"`
		}
		if json.Unmarshal(out, &parsed) == nil && strings.TrimSpace(parsed.FrameworkRevision) != "" {
			return parsed.FrameworkRevision, nil
		}
	}
	// The installed frontend ships its .git, so the FULL revision is authoritative from git. Prefer it
	// over the plain `flutter --version` text below, which prints a SHORTENED revision (e.g. f74781f621)
	// and caused a one-time first-run false negative in `frontend doctor` right after install.
	if root := filepath.Dir(filepath.Dir(binPath)); strings.TrimSpace(root) != "" {
		if gitOut, gerr := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output(); gerr == nil {
			if rev := strings.TrimSpace(string(gitOut)); rev != "" {
				return rev, nil
			}
		}
	}
	cmd = exec.Command(binPath, "--version")
	out, err2 := cmd.CombinedOutput()
	if err2 != nil {
		if err != nil {
			return "", fmt.Errorf("%v; %v", err, err2)
		}
		return "", err2
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Framework") && strings.Contains(line, "revision") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "revision" && i+1 < len(fields) {
					return fields[i+1], nil
				}
			}
		}
	}
	return "", errors.New("could not parse flutter revision from --version output")
}

// --- low-level helpers ---

// streamDownloadToFile streams rawURL into dst (following redirects, no size cap), returning the sha256
// (hex) + byte count. Bytes are never buffered whole in memory. An optional progress sink (a
// *progressReporter, or nil) is added to the MultiWriter so callers can report live download progress to
// STDERR; it never affects the hash or the returned byte count.
func streamDownloadToFile(rawURL string, dst *os.File, progress io.Writer) (string, int64, error) {
	resp, err := http.Get(rawURL)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return "", 0, fmt.Errorf("GET %s: %s", rawURL, msg)
	}
	h := sha256.New()
	writers := []io.Writer{dst, h}
	if progress != nil {
		writers = append(writers, progress)
	}
	n, err := io.Copy(io.MultiWriter(writers...), resp.Body)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// untarGzReader extracts gzip'd tar bytes from r into dst. Rejects unsafe (absolute / .. traversal) entries.
// Streaming variant of untarGz (bytes) so a ~1 GB archive is never held whole in memory. The frontend
// archive contains only regular files + directories (no symlinks/hardlinks), so those are the handled types.
func untarGzReader(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
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
		// Skip macOS AppleDouble sidecars (._name). A frontend archive tarred on macOS without
		// COPYFILE_DISABLE carries a `._foo` metadata file next to every `foo`; git in particular chokes
		// on `.git/objects/pack/._pack-*.idx` (it parses the AppleDouble blob as a pack index and aborts),
		// which breaks git-dependent flutter operations (e.g. `flutter build ios`). These files are never
		// wanted, so drop them on extract — the frontend runs from a clean tree regardless of how it was packed.
		if strings.HasPrefix(filepath.Base(clean), "._") {
			continue
		}
		target := filepath.Join(dst, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
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
		default:
			return fmt.Errorf("refusing unsupported archive entry type %d for %q", hdr.Typeflag, hdr.Name)
		}
	}
}

// clearQuarantine best-effort removes the macOS com.apple.quarantine xattr recursively so extracted binaries
// run without a Gatekeeper prompt/SIGKILL. No-op on non-darwin.
func clearQuarantine(dir string) {
	if runtime.GOOS != "darwin" {
		return
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return
	}
	_ = exec.Command("xattr", "-dr", "com.apple.quarantine", dir).Run()
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "unknown size"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
