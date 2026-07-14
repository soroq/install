package main

// update_cmd_test.go — failure-safety tests for the production-safe `soroq update`
// self-updater (selfupdate.go). Every test fakes the GitHub releases API + the
// asset/checksum downloads with httptest and uses a throwaway "install dir" holding
// fake soroq/soroqctl scripts, so no real install or network is touched. The core
// invariant asserted after EVERY failing-update case: the install dir's soroq and
// soroqctl are byte-for-byte the ORIGINALS (the CLI stays usable).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const (
	origVersion   = "v0.2.3"
	stableVersion = "v0.2.4"
)

type updOpts struct {
	current              string
	goos, goarch         string
	stableTag            string
	newSoroqVersion      string // version the NEW soroq script prints (default stableTag)
	includePrerelease    bool
	includeSoroqctl      bool
	omitArchiveAsset     bool
	omitChecksums        bool
	badChecksum          bool
	missingChecksumEntry bool
	malformedArchive     bool
	shortDownload        bool
	failReleases         bool
	installSpace         bool
}

type updFixture struct {
	t          *testing.T
	cfg        selfUpdateConfig
	out        *bytes.Buffer
	installDir string
	soroqPath  string
	ctlPath    string
	origSoroq  []byte
	origCtl    []byte
	server     *httptest.Server
}

func defaultUpdOpts() updOpts {
	return updOpts{
		current:         origVersion,
		goos:            runtime.GOOS,
		goarch:          runtime.GOARCH,
		stableTag:       stableVersion,
		includeSoroqctl: true,
	}
}

func makeArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		content := []byte(body)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func execScript(version, tool string) string {
	// Prints e.g. "soroq v0.2.4" for any args, so `<bin> version` verification works.
	return "#!/bin/sh\necho \"" + tool + " " + version + "\"\n"
}

func setupUpdate(t *testing.T, opts updOpts) *updFixture {
	t.Helper()
	if opts.newSoroqVersion == "" {
		opts.newSoroqVersion = opts.stableTag
	}

	root := t.TempDir()
	installName := "bin"
	if opts.installSpace {
		installName = "soroq bin dir" // path with spaces (test 15)
	}
	installDir := filepath.Join(root, installName)
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}

	origSoroq := []byte(execScript(origVersion, "soroq"))
	origCtl := []byte(execScript(origVersion, "soroqctl"))
	soroqPath := filepath.Join(installDir, "soroq")
	ctlPath := filepath.Join(installDir, "soroqctl")
	if err := os.WriteFile(soroqPath, origSoroq, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctlPath, origCtl, 0o755); err != nil {
		t.Fatal(err)
	}

	assetName := fmt.Sprintf("soroq_%s_%s.tar.gz", opts.goos, opts.goarch)

	var archive []byte
	if opts.malformedArchive {
		archive = []byte("this is definitely not a gzip archive")
	} else {
		files := map[string]string{"soroq": execScript(opts.newSoroqVersion, "soroq")}
		if opts.includeSoroqctl {
			files["soroqctl"] = execScript(opts.newSoroqVersion, "soroqctl")
		}
		archive = makeArchive(t, files)
	}

	var checksums string
	switch {
	case opts.missingChecksumEntry:
		checksums = sha256Hex([]byte("unrelated")) + "  some-other-file.tar.gz\n"
	case opts.badChecksum:
		checksums = strings.Repeat("0", 64) + "  " + assetName + "\n"
	default:
		checksums = sha256Hex(archive) + "  " + assetName + "\n"
	}

	assets := map[string][]byte{
		assetName:       archive,
		"checksums.txt": []byte(checksums),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/soroq/install/releases", func(w http.ResponseWriter, r *http.Request) {
		if opts.failReleases {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		base := "http://" + r.Host
		var rels []map[string]any
		stableAssets := []map[string]any{}
		if !opts.omitArchiveAsset {
			stableAssets = append(stableAssets, map[string]any{"name": assetName, "browser_download_url": base + "/dl/" + assetName})
		}
		if !opts.omitChecksums {
			stableAssets = append(stableAssets, map[string]any{"name": "checksums.txt", "browser_download_url": base + "/dl/checksums.txt"})
		}
		rels = append(rels, map[string]any{"tag_name": opts.stableTag, "draft": false, "prerelease": false, "assets": stableAssets})
		if opts.includePrerelease {
			// A NEWER prerelease (v0.3.0) that MUST be ignored on the stable channel.
			rels = append(rels, map[string]any{
				"tag_name": "v0.3.0", "draft": false, "prerelease": true,
				"assets": []map[string]any{{"name": assetName, "browser_download_url": base + "/dl/" + assetName}},
			})
		}
		_ = json.NewEncoder(w).Encode(rels)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		data, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if opts.shortDownload && name == assetName {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)+4096))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data[:len(data)/2]) // truncate → client sees ErrUnexpectedEOF
			return
		}
		_, _ = w.Write(data)
	})

	server := httptest.NewServer(mux)
	if opts.failReleases {
		server.Close() // network error: connection refused
	}
	t.Cleanup(server.Close)

	out := &bytes.Buffer{}
	f := &updFixture{
		t: t, out: out, installDir: installDir,
		soroqPath: soroqPath, ctlPath: ctlPath,
		origSoroq: origSoroq, origCtl: origCtl, server: server,
		cfg: selfUpdateConfig{
			apiBase:        server.URL,
			installRepo:    "soroq/install",
			installDir:     installDir,
			goos:           opts.goos,
			goarch:         opts.goarch,
			currentVersion: opts.current,
			stdout:         out,
			httpClient:     server.Client(),
		},
	}
	return f
}

// assertOriginal proves both installed binaries are byte-identical to the originals
// (the CLI stays usable after a failed update).
func (f *updFixture) assertOriginal() {
	f.t.Helper()
	gotSoroq, err := os.ReadFile(f.soroqPath)
	if err != nil {
		f.t.Fatalf("read soroq: %v", err)
	}
	gotCtl, err := os.ReadFile(f.ctlPath)
	if err != nil {
		f.t.Fatalf("read soroqctl: %v", err)
	}
	if !bytes.Equal(gotSoroq, f.origSoroq) {
		f.t.Fatalf("soroq was modified after a failed update:\n%s", gotSoroq)
	}
	if !bytes.Equal(gotCtl, f.origCtl) {
		f.t.Fatalf("soroqctl was modified after a failed update:\n%s", gotCtl)
	}
	// No staged/backup leftovers.
	for _, n := range []string{".soroq.new", ".soroqctl.new", ".soroq.bak", ".soroqctl.bak"} {
		if _, err := os.Stat(filepath.Join(f.installDir, n)); err == nil {
			f.t.Fatalf("leftover transactional file %s", n)
		}
	}
}

// --- 1: already on latest → no-op ---

func TestUpdate_AlreadyLatest(t *testing.T) {
	o := defaultUpdOpts()
	o.current = stableVersion
	f := setupUpdate(t, o)
	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(f.out.String(), "already up to date") {
		t.Fatalf("expected up-to-date message, got:\n%s", f.out.String())
	}
	f.assertOriginal()
}

// --- 2: newer stable available → updates (happy path) ---

func TestUpdate_NewerStableInstalls(t *testing.T) {
	f := setupUpdate(t, defaultUpdOpts())
	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("update: %v", err)
	}
	out := f.out.String()
	for _, want := range []string{
		"Current version: v0.2.3",
		"Latest version:  v0.2.4",
		"Downloading Soroq v0.2.4...",
		"Verifying checksum...",
		"Updating soroq and soroqctl...",
		"Updated successfully to v0.2.4.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
	t.Logf("happy-path output:\n%s", out)
	// Both binaries are the NEW ones and the installed soroq runs + reports v0.2.4.
	newSoroq, _ := os.ReadFile(f.soroqPath)
	if bytes.Equal(newSoroq, f.origSoroq) {
		t.Fatal("soroq was not replaced")
	}
	newCtl, _ := os.ReadFile(f.ctlPath)
	if bytes.Equal(newCtl, f.origCtl) {
		t.Fatal("soroqctl was not replaced")
	}
	if err := verifyInstalledVersion(f.soroqPath, stableVersion); err != nil {
		t.Fatalf("installed soroq does not report %s: %v", stableVersion, err)
	}
}

// --- 3: unsupported OS/arch → clear error, no change ---

func TestUpdate_UnsupportedPlatform(t *testing.T) {
	o := defaultUpdOpts()
	o.goos = "plan9"
	f := setupUpdate(t, o)
	err := performSelfUpdate(f.cfg)
	if err == nil || !strings.Contains(err.Error(), "unsupported platform") {
		t.Fatalf("expected unsupported-platform error, got %v", err)
	}
	f.assertOriginal()
}

// --- 4: release endpoint unavailable → network error, install intact ---

func TestUpdate_ReleaseEndpointUnavailable(t *testing.T) {
	o := defaultUpdOpts()
	o.failReleases = true
	f := setupUpdate(t, o)
	err := performSelfUpdate(f.cfg)
	if err == nil {
		t.Fatal("expected a network/endpoint error")
	}
	f.assertOriginal()
}

// --- 5: interrupted / short download → abort, intact ---

func TestUpdate_ShortDownloadAborts(t *testing.T) {
	o := defaultUpdOpts()
	o.shortDownload = true
	f := setupUpdate(t, o)
	if err := performSelfUpdate(f.cfg); err == nil {
		t.Fatal("expected a truncated-download error")
	}
	f.assertOriginal()
}

// --- 6: missing archive asset → abort ---

func TestUpdate_MissingArchiveAsset(t *testing.T) {
	o := defaultUpdOpts()
	o.omitArchiveAsset = true
	f := setupUpdate(t, o)
	err := performSelfUpdate(f.cfg)
	if err == nil || !strings.Contains(err.Error(), "no asset") {
		t.Fatalf("expected missing-asset error, got %v", err)
	}
	f.assertOriginal()
}

// --- 7: missing checksum entry for the archive → abort ---

func TestUpdate_MissingChecksumEntry(t *testing.T) {
	o := defaultUpdOpts()
	o.missingChecksumEntry = true
	f := setupUpdate(t, o)
	err := performSelfUpdate(f.cfg)
	if err == nil || !strings.Contains(err.Error(), "no checksum entry") {
		t.Fatalf("expected missing-checksum-entry error, got %v", err)
	}
	f.assertOriginal()
}

// --- 8: incorrect checksum → abort, no extract, intact ---

func TestUpdate_IncorrectChecksum(t *testing.T) {
	o := defaultUpdOpts()
	o.badChecksum = true
	f := setupUpdate(t, o)
	err := performSelfUpdate(f.cfg)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum-mismatch error, got %v", err)
	}
	f.assertOriginal()
}

// --- 9: malformed archive → abort ---

func TestUpdate_MalformedArchive(t *testing.T) {
	o := defaultUpdOpts()
	o.malformedArchive = true
	f := setupUpdate(t, o)
	if err := performSelfUpdate(f.cfg); err == nil {
		t.Fatal("expected an extract error for a malformed archive")
	}
	f.assertOriginal()
}

// --- 10: archive missing soroqctl → abort, neither replaced ---

func TestUpdate_ArchiveMissingSoroqctl(t *testing.T) {
	o := defaultUpdOpts()
	o.includeSoroqctl = false
	f := setupUpdate(t, o)
	err := performSelfUpdate(f.cfg)
	if err == nil || !strings.Contains(err.Error(), "does not contain soroqctl") {
		t.Fatalf("expected missing-soroqctl error, got %v", err)
	}
	f.assertOriginal()
}

// --- 11: failure replacing the SECOND binary → first restored (both-or-neither) ---

func TestUpdate_SecondRenameFailsRollsBackBoth(t *testing.T) {
	f := setupUpdate(t, defaultUpdOpts())
	saved := osRename
	osRename = func(oldpath, newpath string) error {
		if strings.HasSuffix(oldpath, ".soroqctl.new") { // the SECOND into-place rename
			return fmt.Errorf("injected rename failure")
		}
		return saved(oldpath, newpath)
	}
	defer func() { osRename = saved }()

	err := performSelfUpdate(f.cfg)
	if err == nil || !strings.Contains(err.Error(), "both binaries") {
		t.Fatalf("expected both-or-neither rollback error, got %v", err)
	}
	f.assertOriginal() // BOTH must be the originals
}

// --- 12: post-install version mismatch → rollback restores old binaries that run ---

func TestUpdate_VersionMismatchRollsBack(t *testing.T) {
	o := defaultUpdOpts()
	o.newSoroqVersion = "v0.0.0-wrong" // installed soroq reports the wrong version
	f := setupUpdate(t, o)
	err := performSelfUpdate(f.cfg)
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("expected post-install verification failure, got %v", err)
	}
	f.assertOriginal()
	// The restored old soroq still runs and reports the old version.
	if err := verifyInstalledVersion(f.soroqPath, origVersion); err != nil {
		t.Fatalf("restored soroq is not runnable/old: %v", err)
	}
}

// --- 13: --check makes NO filesystem changes ---

func TestUpdate_CheckMakesNoFilesystemChanges(t *testing.T) {
	f := setupUpdate(t, defaultUpdOpts())
	f.cfg.checkOnly = true

	before := snapshotInstallDir(t, f.installDir)
	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("update --check: %v", err)
	}
	after := snapshotInstallDir(t, f.installDir)
	if before != after {
		t.Fatalf("--check changed the filesystem:\nbefore=%s\nafter=%s", before, after)
	}
	out := f.out.String()
	if !strings.Contains(out, "Latest version:  v0.2.4") || strings.Contains(out, "Updated successfully") {
		t.Fatalf("--check output wrong:\n%s", out)
	}
	f.assertOriginal()
}

// snapshotDir returns a stable string of every entry's name, size, mode, mtime and
// content hash under dir.
func snapshotInstallDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		fmt.Fprintf(&sb, "%s|%d|%v|%d|%s\n", e.Name(), info.Size(), info.Mode(), info.ModTime().UnixNano(), sha256Hex(data))
	}
	return sb.String()
}

// --- 14: prerelease ignored on the stable channel ---

func TestUpdate_PrereleaseIgnored(t *testing.T) {
	o := defaultUpdOpts()
	o.includePrerelease = true // a NEWER v0.3.0 prerelease also exists
	f := setupUpdate(t, o)
	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("update: %v", err)
	}
	out := f.out.String()
	if !strings.Contains(out, "Updated successfully to v0.2.4.") {
		t.Fatalf("stable update should pick v0.2.4, not the v0.3.0 prerelease:\n%s", out)
	}
	if strings.Contains(out, "v0.3.0") {
		t.Fatalf("prerelease v0.3.0 leaked into a stable update:\n%s", out)
	}
	if err := verifyInstalledVersion(f.soroqPath, stableVersion); err != nil {
		t.Fatalf("installed soroq should be v0.2.4: %v", err)
	}
}

// --- 15: install dir path containing spaces works ---

func TestUpdate_InstallDirWithSpaces(t *testing.T) {
	o := defaultUpdOpts()
	o.installSpace = true
	f := setupUpdate(t, o)
	if !strings.Contains(f.installDir, " ") {
		t.Fatalf("expected a spaced install dir, got %q", f.installDir)
	}
	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("update in spaced dir: %v", err)
	}
	if err := verifyInstalledVersion(f.soroqPath, stableVersion); err != nil {
		t.Fatalf("installed soroq in spaced dir not runnable: %v", err)
	}
}
