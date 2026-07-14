package main

// selfupdate.go — engine for `soroq update` (see update_cmd.go for the CLI layer).
//
// Safety model (implemented EXACTLY, in order):
//  1. current version = buildVersion; target = latest STABLE soroq/install release.
//  2. map GOOS/GOARCH → soroq_<os>_<arch>.tar.gz (darwin|linux × amd64|arm64).
//  3. resolve latest via the PUBLIC GitHub releases API (NO auth), EXCLUDING
//     prereleases/drafts (this is the stable channel).
//  4. download the archive + checksums.txt into an os.MkdirTemp dir.
//  5. verify the archive SHA-256 against checksums.txt BEFORE extraction.
//  6. extract; require BOTH soroq and soroqctl; force 0755.
//  7. replace both binaries TRANSACTIONALLY in the install dir (stage → back up →
//     rename-into-place both; on the SECOND failure restore the FIRST → both-or
//     -neither), then exec the new soroq to confirm the expected version; on any
//     failure restore the previous binaries so the install stays usable.
//
// Only stdlib is used (net/http, archive/tar, compress/gzip, crypto/sha256, os,
// encoding/json, ...); no new go.mod dependency.

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// githubAPIBase is the public GitHub REST API root. Overridable in tests so the
// releases API + asset/checksum downloads can be faked with httptest.
var githubAPIBase = "https://api.github.com"

// osRename indirects os.Rename so a test can inject a failure of the SECOND
// binary's rename-into-place and assert both-or-neither rollback.
var osRename = os.Rename

const (
	soroqBinName    = "soroq"
	soroqctlBinName = "soroqctl"
	checksumsAsset  = "checksums.txt"
)

// selfUpdateConfig carries everything performSelfUpdate needs; every external
// dependency (API base, install dir, OS/arch, http client) is a field so the whole
// updater is testable without touching the real install or network.
type selfUpdateConfig struct {
	apiBase        string
	installRepo    string
	installDir     string
	goos, goarch   string
	currentVersion string
	checkOnly      bool
	stdout         io.Writer
	httpClient     *http.Client
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

func performSelfUpdate(c selfUpdateConfig) error {
	// Step 2: fail fast on an unsupported platform, before any network or FS work.
	assetName, err := c.assetName()
	if err != nil {
		return err
	}

	// Step 3: resolve the latest STABLE release (network only; no FS change).
	rel, err := c.resolveLatestStable()
	if err != nil {
		return err
	}
	latest := rel.TagName

	upToDate := versionAtLeast(c.currentVersion, latest)

	if c.checkOnly {
		// --check: report and make ZERO filesystem changes.
		if upToDate {
			fmt.Fprintf(c.stdout, "Soroq %s is already up to date.\n", displayVersion(latest))
			return nil
		}
		fmt.Fprintf(c.stdout, "Current version: %s\n", displayVersion(c.currentVersion))
		fmt.Fprintf(c.stdout, "Latest version:  %s\n", displayVersion(latest))
		fmt.Fprintf(c.stdout, "Run `soroq update` to install %s.\n", displayVersion(latest))
		return nil
	}

	if upToDate {
		fmt.Fprintf(c.stdout, "Soroq %s is already up to date.\n", displayVersion(latest))
		return nil
	}

	// Step 9 (early): the install dir must be writable. Report the EXACT path and an
	// actionable message; never silently fall back to another directory.
	if err := ensureWritableDir(c.installDir); err != nil {
		return err
	}

	fmt.Fprintf(c.stdout, "Current version: %s\n", displayVersion(c.currentVersion))
	fmt.Fprintf(c.stdout, "Latest version:  %s\n", displayVersion(latest))

	// Step 4: download the archive + checksums.txt into a temp dir.
	archiveURL := findAssetURL(rel, assetName)
	if archiveURL == "" {
		return fmt.Errorf("release %s has no asset %q for this platform; nothing changed", latest, assetName)
	}
	checksumsURL := findAssetURL(rel, checksumsAsset)
	if checksumsURL == "" {
		return fmt.Errorf("release %s has no %s; refusing to install unverified binaries; nothing changed", latest, checksumsAsset)
	}

	tmpDir, err := os.MkdirTemp("", "soroq-update-")
	if err != nil {
		return fmt.Errorf("cannot create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Fprintf(c.stdout, "Downloading Soroq %s...\n", displayVersion(latest))
	archivePath := filepath.Join(tmpDir, assetName)
	if err := c.download(archiveURL, archivePath); err != nil {
		return fmt.Errorf("downloading %s: %w; nothing changed", assetName, err)
	}
	checksumsPath := filepath.Join(tmpDir, checksumsAsset)
	if err := c.download(checksumsURL, checksumsPath); err != nil {
		return fmt.Errorf("downloading %s: %w; nothing changed", checksumsAsset, err)
	}

	// Step 5: verify the archive SHA-256 against checksums.txt BEFORE extraction.
	fmt.Fprintln(c.stdout, "Verifying checksum...")
	got, err := sha256File(archivePath)
	if err != nil {
		return fmt.Errorf("hashing %s: %w; nothing changed", assetName, err)
	}
	want, err := checksumFor(checksumsPath, assetName)
	if err != nil {
		return fmt.Errorf("%w; nothing changed", err)
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s (expected %s, got %s); refusing to install; nothing changed", assetName, want, got)
	}

	// Step 6: extract into the temp dir and require BOTH binaries (force 0755).
	extractDir := filepath.Join(tmpDir, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return err
	}
	found, err := extractBinaries(archivePath, extractDir)
	if err != nil {
		return fmt.Errorf("extracting %s: %w; nothing changed", assetName, err)
	}
	if !found[soroqBinName] {
		return fmt.Errorf("archive %s does not contain %s; nothing changed", assetName, soroqBinName)
	}
	if !found[soroqctlBinName] {
		return fmt.Errorf("archive %s does not contain %s; nothing changed", assetName, soroqctlBinName)
	}

	// Step 7 + 10 + 11: transactional both-binary replace + post-install version
	// verification, restoring the previous binaries on any failure.
	fmt.Fprintln(c.stdout, "Updating soroq and soroqctl...")
	if err := c.installBinaries(
		filepath.Join(extractDir, soroqBinName),
		filepath.Join(extractDir, soroqctlBinName),
		latest,
	); err != nil {
		return err
	}

	fmt.Fprintf(c.stdout, "Updated successfully to %s.\n", displayVersion(latest))
	return nil
}

// assetName maps this OS/arch to the release archive name, or a clear error for an
// unsupported combination.
func (c selfUpdateConfig) assetName() (string, error) {
	supportedOS := map[string]bool{"darwin": true, "linux": true}
	supportedArch := map[string]bool{"amd64": true, "arm64": true}
	if !supportedOS[c.goos] || !supportedArch[c.goarch] {
		return "", fmt.Errorf("unsupported platform %s/%s: soroq update supports darwin and linux on amd64 and arm64; reinstall from https://github.com/soroq/install for your platform", c.goos, c.goarch)
	}
	return fmt.Sprintf("soroq_%s_%s.tar.gz", c.goos, c.goarch), nil
}

// resolveLatestStable lists releases from the PUBLIC GitHub API (no auth) and picks
// the highest-versioned release that is NOT a draft or prerelease — the stable
// channel. Using the list (not /releases/latest) keeps the prerelease-exclusion
// logic here where it is tested.
func (c selfUpdateConfig) resolveLatestStable() (ghRelease, error) {
	url := strings.TrimRight(c.apiBase, "/") + "/repos/" + c.installRepo + "/releases?per_page=100"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return ghRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ghRelease{}, fmt.Errorf("cannot reach the release server (%s): %w; nothing changed", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ghRelease{}, fmt.Errorf("release server returned HTTP %d for %s; nothing changed", resp.StatusCode, url)
	}
	var releases []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return ghRelease{}, fmt.Errorf("cannot parse the release list: %w; nothing changed", err)
	}

	var best ghRelease
	var bestVer [3]int
	found := false
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue // stable channel only
		}
		v, ok := parseSemver(r.TagName)
		if !ok {
			continue
		}
		if !found || compareSemver(v, bestVer) > 0 {
			best, bestVer, found = r, v, true
		}
	}
	if !found {
		return ghRelease{}, fmt.Errorf("no stable release found for %s; nothing changed", c.installRepo)
	}
	return best, nil
}

func findAssetURL(rel ghRelease, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// download streams url into dest. A truncated/short response (Content-Length larger
// than the body) surfaces as io.ErrUnexpectedEOF, so an interrupted download aborts.
func (c selfUpdateConfig) download(url, dest string) error {
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checksumFor parses a `sha256sum`-style checksums.txt and returns the hash for
// assetName, or an error if there is no entry (missing entry → refuse to install).
func checksumFor(checksumsPath, assetName string) (string, error) {
	f, err := os.Open(checksumsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		// The last field is the filename; a leading '*' marks binary mode.
		name := filepath.Base(strings.TrimPrefix(fields[len(fields)-1], "*"))
		if name == assetName {
			return fields[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no checksum entry for %s in %s", assetName, checksumsAsset)
}

// extractBinaries walks the gzip'd tar and writes ONLY soroq/soroqctl (matched by
// base name) into destDir with 0755 perms. It returns which of the two were found.
// A malformed archive (bad gzip/tar) returns an error.
func extractBinaries(archivePath, destDir string) (map[string]bool, error) {
	found := map[string]bool{}
	f, err := os.Open(archivePath)
	if err != nil {
		return found, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return found, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return found, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if base != soroqBinName && base != soroqctlBinName {
			continue
		}
		out := filepath.Join(destDir, base)
		w, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return found, err
		}
		if _, err := io.Copy(w, tr); err != nil {
			w.Close()
			return found, err
		}
		if err := w.Close(); err != nil {
			return found, err
		}
		if err := os.Chmod(out, 0o755); err != nil { // step 8: preserve exec perms
			return found, err
		}
		found[base] = true
	}
	return found, nil
}

// installBinaries replaces both binaries transactionally in c.installDir:
// stage both (in-dir temp names, same filesystem) → back up both → rename-into
// -place both → exec the new soroq to confirm expectVersion. If the SECOND rename
// (or the version check) fails, both binaries are restored from backup, so the
// result is always both-new-or-both-old. On success the backups are removed.
func (c selfUpdateConfig) installBinaries(newSoroq, newSoroqctl, expectVersion string) error {
	dir := c.installDir
	dstSoroq := filepath.Join(dir, soroqBinName)
	dstSoroqctl := filepath.Join(dir, soroqctlBinName)
	stgSoroq := filepath.Join(dir, ".soroq.new")
	stgSoroqctl := filepath.Join(dir, ".soroqctl.new")
	bakSoroq := filepath.Join(dir, ".soroq.bak")
	bakSoroqctl := filepath.Join(dir, ".soroqctl.bak")

	// 1. Stage both new binaries in the install dir (same fs as the targets, so the
	//    later rename-into-place is atomic and never crosses filesystems).
	if err := copyFile(newSoroq, stgSoroq, 0o755); err != nil {
		os.Remove(stgSoroq)
		return fmt.Errorf("staging new soroq: %w; nothing changed", err)
	}
	if err := copyFile(newSoroqctl, stgSoroqctl, 0o755); err != nil {
		os.Remove(stgSoroq)
		os.Remove(stgSoroqctl)
		return fmt.Errorf("staging new soroqctl: %w; nothing changed", err)
	}

	// 2. Back up both existing binaries (if present).
	haveSoroq := fileExists(dstSoroq)
	haveSoroqctl := fileExists(dstSoroqctl)
	if haveSoroq {
		if err := osRename(dstSoroq, bakSoroq); err != nil {
			os.Remove(stgSoroq)
			os.Remove(stgSoroqctl)
			return fmt.Errorf("backing up soroq: %w; nothing changed", err)
		}
	}
	if haveSoroqctl {
		if err := osRename(dstSoroqctl, bakSoroqctl); err != nil {
			if haveSoroq {
				osRename(bakSoroq, dstSoroq)
			}
			os.Remove(stgSoroq)
			os.Remove(stgSoroqctl)
			return fmt.Errorf("backing up soroqctl: %w; nothing changed", err)
		}
	}

	// rollback restores BOTH previous binaries and clears staged/leftover files.
	rollback := func() {
		os.Remove(dstSoroq)
		os.Remove(dstSoroqctl)
		if haveSoroq {
			osRename(bakSoroq, dstSoroq)
		}
		if haveSoroqctl {
			osRename(bakSoroqctl, dstSoroqctl)
		}
		os.Remove(stgSoroq)
		os.Remove(stgSoroqctl)
	}

	// 3. Rename the first new binary into place.
	if err := osRename(stgSoroq, dstSoroq); err != nil {
		rollback()
		return fmt.Errorf("installing soroq: %w; previous version restored", err)
	}
	// 4. Rename the second new binary into place. If this fails, restore the first
	//    from backup so we never leave one updated and the other old.
	if err := osRename(stgSoroqctl, dstSoroqctl); err != nil {
		rollback()
		return fmt.Errorf("installing soroqctl: %w; previous version restored (both binaries)", err)
	}

	// 5. Verify the newly installed soroq reports the expected version; if not,
	//    roll BOTH back to the previous, known-good binaries.
	if err := verifyInstalledVersion(dstSoroq, expectVersion); err != nil {
		rollback()
		return fmt.Errorf("post-install verification failed: %w; previous version restored", err)
	}

	// 6. Success: remove backups (best-effort).
	os.Remove(bakSoroq)
	os.Remove(bakSoroqctl)
	return nil
}

// verifyInstalledVersion execs `<soroqPath> version` and confirms its output names
// the expected version.
func verifyInstalledVersion(soroqPath, expect string) error {
	out, err := exec.Command(soroqPath, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("running %s version: %w", soroqPath, err)
	}
	got := strings.TrimSpace(string(out))
	needle := strings.TrimPrefix(strings.TrimSpace(expect), "v")
	if needle == "" || !strings.Contains(got, needle) {
		return fmt.Errorf("installed soroq reports %q, expected version %s", got, expect)
	}
	return nil
}

// ensureWritableDir returns an actionable error (naming the exact path) when dir is
// not writable, without falling back to any other directory.
func ensureWritableDir(dir string) error {
	f, err := os.CreateTemp(dir, ".soroq-update-perm-")
	if err != nil {
		return fmt.Errorf("install directory %s is not writable: %v\nRe-run soroq update with permission to write there (fix its ownership/permissions, or run with elevated privileges); soroq will not install into any other directory", dir, err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// parseSemver parses a vX.Y.Z tag into [3]int, defensively dropping any
// prerelease/build suffix. Returns ok=false for non-numeric tags.
func parseSemver(tag string) ([3]int, bool) {
	var v [3]int
	s := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func compareSemver(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// versionAtLeast reports whether current is the same or newer than latest. When
// either side is not semver (e.g. a "dev" build), it falls back to a string compare
// so a non-semver build is treated as needing an update.
func versionAtLeast(current, latest string) bool {
	cv, cok := parseSemver(current)
	lv, lok := parseSemver(latest)
	if cok && lok {
		return compareSemver(cv, lv) >= 0
	}
	return strings.TrimPrefix(current, "v") == strings.TrimPrefix(latest, "v")
}

func displayVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "(unknown)"
	}
	if _, ok := parseSemver(v); ok && !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}
