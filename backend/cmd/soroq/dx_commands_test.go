package main

// dx_commands_test.go — tests for the P4 beginner commands: update / uninstall /
// cache (all four in one file, sharing the temp-HOME cache fixtures). These use a
// temp HOME so os.UserHomeDir() resolves under t.TempDir(); nothing touches the
// real ~/.soroq.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureCmdStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote. Self-contained (does not depend on any other test file's helper).
func captureCmdStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf.String()
}

// writeCacheVersion creates ~/.soroq/<kind>/<version>/ with a file of nBytes so the
// directory has a measurable size.
func writeCacheVersion(t *testing.T, home, kind, version string, nBytes int) {
	t.Helper()
	dir := filepath.Join(home, ".soroq", kind, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob"), bytes.Repeat([]byte("x"), nBytes), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeActiveFrontend records ~/.soroq/frontends/active.json pointing at version.
func writeActiveFrontend(t *testing.T, home, version string) {
	t.Helper()
	path := filepath.Join(home, ".soroq", "frontends", "active.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"version": version, "flutter_bin": "/x", "archive_sha256": "y"})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeActiveToolchain records ~/.soroq/toolchains/active.json pinning one platform.
func writeActiveToolchain(t *testing.T, home, platform, toolchainVersion, frontendVersion string) {
	t.Helper()
	path := filepath.Join(home, ".soroq", "toolchains", "active.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"platforms": map[string]any{
			platform: map[string]any{"toolchain_version": toolchainVersion, "frontend_version": frontendVersion},
		},
	})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- cache list ---

func TestCacheList_MarksActiveAndSizes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCacheVersion(t, home, "frontends", "fe-active", 100)
	writeCacheVersion(t, home, "frontends", "fe-stale", 50)
	writeCacheVersion(t, home, "toolchains", "tc-active", 200)
	writeCacheVersion(t, home, "toolchains", "tc-stale", 30)
	writeActiveFrontend(t, home, "fe-active")
	writeActiveToolchain(t, home, "android", "tc-active", "fe-active")

	out := captureCmdStdout(t, func() {
		if err := runCacheList([]string{}); err != nil {
			t.Fatalf("cache list: %v", err)
		}
	})
	// active versions marked with '*', stale not.
	for _, want := range []string{"fe-active", "fe-stale", "tc-active", "tc-stale", "Total cached"} {
		if !strings.Contains(out, want) {
			t.Fatalf("cache list missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "* ") {
		t.Fatalf("expected an active marker in:\n%s", out)
	}

	// JSON surface: active flags correct + non-zero total.
	jsonOut := captureCmdStdout(t, func() {
		if err := runCacheList([]string{"--json"}); err != nil {
			t.Fatalf("cache list --json: %v", err)
		}
	})
	var parsed struct {
		Frontends  []cacheEntry `json:"frontends"`
		Toolchains []cacheEntry `json:"toolchains"`
		TotalBytes int64        `json:"total_bytes"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil {
		t.Fatalf("parse json %q: %v", jsonOut, err)
	}
	if parsed.TotalBytes != 380 {
		t.Fatalf("total_bytes = %d, want 380", parsed.TotalBytes)
	}
	assertActive(t, parsed.Frontends, "fe-active", true)
	assertActive(t, parsed.Frontends, "fe-stale", false)
	assertActive(t, parsed.Toolchains, "tc-active", true)
	assertActive(t, parsed.Toolchains, "tc-stale", false)
}

func assertActive(t *testing.T, entries []cacheEntry, version string, want bool) {
	t.Helper()
	for _, e := range entries {
		if e.Version == version {
			if e.Active != want {
				t.Fatalf("%s active = %v, want %v", version, e.Active, want)
			}
			return
		}
	}
	t.Fatalf("version %s not found in %+v", version, entries)
}

// --- cache clean ---

func TestCacheClean_DryRunKeepsActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCacheVersion(t, home, "frontends", "fe-active", 10)
	writeCacheVersion(t, home, "frontends", "fe-stale", 10)
	writeCacheVersion(t, home, "toolchains", "tc-active", 10)
	writeCacheVersion(t, home, "toolchains", "tc-stale", 10)
	writeActiveFrontend(t, home, "fe-active")
	writeActiveToolchain(t, home, "android", "tc-active", "fe-active")

	out := captureCmdStdout(t, func() {
		if err := runCacheClean([]string{}); err != nil {
			t.Fatalf("cache clean: %v", err)
		}
	})
	if strings.Contains(out, "fe-active") || strings.Contains(out, "tc-active") {
		t.Fatalf("dry-run listed an ACTIVE version for removal:\n%s", out)
	}
	if !strings.Contains(out, "fe-stale") || !strings.Contains(out, "tc-stale") {
		t.Fatalf("dry-run should list stale versions:\n%s", out)
	}
	// Nothing deleted on dry run.
	for _, v := range []string{"fe-active", "fe-stale", "tc-active", "tc-stale"} {
		kind := "frontends"
		if strings.HasPrefix(v, "tc") {
			kind = "toolchains"
		}
		if _, err := os.Stat(filepath.Join(home, ".soroq", kind, v)); err != nil {
			t.Fatalf("dry run deleted %s: %v", v, err)
		}
	}
}

func TestCacheClean_DeleteRemovesOnlyUnreferenced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCacheVersion(t, home, "frontends", "fe-active", 10)
	writeCacheVersion(t, home, "frontends", "fe-stale", 10)
	writeCacheVersion(t, home, "toolchains", "tc-active", 10)
	writeCacheVersion(t, home, "toolchains", "tc-stale", 10)
	writeActiveFrontend(t, home, "fe-active")
	writeActiveToolchain(t, home, "android", "tc-active", "fe-active")

	if err := runCacheClean([]string{"--delete"}); err != nil {
		t.Fatalf("cache clean --delete: %v", err)
	}
	// Active kept.
	if _, err := os.Stat(filepath.Join(home, ".soroq", "frontends", "fe-active")); err != nil {
		t.Fatalf("clean --delete removed the ACTIVE frontend: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".soroq", "toolchains", "tc-active")); err != nil {
		t.Fatalf("clean --delete removed the ACTIVE toolchain: %v", err)
	}
	// Stale removed.
	if _, err := os.Stat(filepath.Join(home, ".soroq", "frontends", "fe-stale")); !os.IsNotExist(err) {
		t.Fatalf("clean --delete kept the stale frontend (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".soroq", "toolchains", "tc-stale")); !os.IsNotExist(err) {
		t.Fatalf("clean --delete kept the stale toolchain (err=%v)", err)
	}
}

func TestCacheClean_SoroqLockPinPreserved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A version referenced by NO active.json, only by a project soroq.lock.
	writeCacheVersion(t, home, "toolchains", "tc-pinned", 10)
	writeCacheVersion(t, home, "toolchains", "tc-stale", 10)

	projectDir := t.TempDir()
	lock := "platforms:\n  android:\n    release_id: r1\n    version: 1.0\n    toolchain_version: tc-pinned\n"
	if err := os.WriteFile(filepath.Join(projectDir, "soroq.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runCacheClean([]string{"--delete", "--project-dir", projectDir}); err != nil {
		t.Fatalf("cache clean --delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".soroq", "toolchains", "tc-pinned")); err != nil {
		t.Fatalf("clean --delete removed a soroq.lock-PINNED toolchain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".soroq", "toolchains", "tc-stale")); !os.IsNotExist(err) {
		t.Fatalf("clean --delete kept the truly-stale toolchain (err=%v)", err)
	}
}

// update tests moved to update_cmd_test.go (P4 stub → production-safe self-updater).

// --- uninstall ---

func TestUninstall_WithoutYesAbortsAndDeletesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCacheVersion(t, home, "frontends", "fe1", 10)

	out := captureCmdStdout(t, func() {
		err := runUninstall([]string{})
		if err != errAlreadyPrinted {
			t.Fatalf("uninstall without --yes should abort with errAlreadyPrinted, got %v", err)
		}
	})
	if !strings.Contains(out, "would remove") {
		t.Fatalf("uninstall should print the plan:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".soroq", "frontends", "fe1")); err != nil {
		t.Fatalf("uninstall without --yes deleted state: %v", err)
	}
}

func TestUninstall_YesRemovesStateAndBinaryOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCacheVersion(t, home, "frontends", "fe1", 10)

	// External install dir (outside ~/.soroq) with a fake soroq binary + a sentinel
	// that must NOT be touched.
	installDir := t.TempDir()
	t.Setenv("SOROQ_INSTALL_DIR", installDir)
	binPath := filepath.Join(installDir, "soroq")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(installDir, "other-tool")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := runUninstall([]string{"--yes"}); err != nil {
		t.Fatalf("uninstall --yes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".soroq")); !os.IsNotExist(err) {
		t.Fatalf("uninstall --yes left ~/.soroq (err=%v)", err)
	}
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Fatalf("uninstall --yes left the soroq binary (err=%v)", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("uninstall --yes deleted an unrelated file in the install dir: %v", err)
	}
	if _, err := os.Stat(installDir); err != nil {
		t.Fatalf("uninstall --yes removed the shared install dir itself: %v", err)
	}
}
