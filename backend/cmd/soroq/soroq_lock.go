package main

// soroq_lock.go — the committed, project-root `soroq.lock` toolchain pin.
//
// PURPOSE (P3): close the beginner loop `setup -> release -> patch`. `soroq release android` (having
// defaulted its toolchain from `soroq setup`'s ~/.soroq/toolchains/active.json) records, beside
// soroq.yaml, WHICH toolchain built WHICH release. `soroq patch android` then reads this pin and builds
// the candidate with the SAME toolchain as its base release — a patch can never build with a toolchain
// != base.
//
// It lives at <projectDir>/soroq.lock (project ROOT, a committable file — NOT under the git-ignored
// .soroq/). It is rendered/parsed by hand to mirror the existing soroq.yaml render (project_cli.go
// renderSoroqConfig) so the repo stays free of a YAML dependency. It stores ONLY version pointers
// (release id, version, toolchain/frontend versions, timestamp) — never a secret.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// soroqLock is the whole soroq.lock document: a per-platform map so android + ios pins coexist.
type soroqLock struct {
	Platforms map[string]soroqLockPin
}

// soroqLockPin is the toolchain pin recorded for one platform's most recent soroq-built release.
type soroqLockPin struct {
	ReleaseID        string
	Version          string
	ToolchainVersion string
	FrontendVersion  string // optional
	RecordedAt       time.Time
}

// soroqLockPath returns <projectDir>/soroq.lock (project ROOT, beside soroq.yaml).
func soroqLockPath(projectDir string) string {
	return filepath.Join(projectDir, "soroq.lock")
}

// loadSoroqLock reads soroq.lock, returning an empty (non-nil-map) value when the file is absent — an
// honest absent lock is NOT an error (patch falls back gracefully).
func loadSoroqLock(projectDir string) (soroqLock, error) {
	b, err := os.ReadFile(soroqLockPath(projectDir))
	if errors.Is(err, os.ErrNotExist) {
		return soroqLock{Platforms: map[string]soroqLockPin{}}, nil
	}
	if err != nil {
		return soroqLock{}, err
	}
	return parseSoroqLock(b), nil
}

// parseSoroqLock hand-parses the 2/4-space-indented soroq.lock body. Malformed or unknown lines are
// skipped (best effort) so a partially hand-edited lock never dead-ends a build.
func parseSoroqLock(data []byte) soroqLock {
	lock := soroqLock{Platforms: map[string]soroqLockPin{}}
	current := ""
	inPlatforms := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		raw := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		if indent == 0 {
			inPlatforms = trimmed == "platforms:"
			current = ""
			continue
		}
		if !inPlatforms {
			continue
		}
		if indent == 2 && strings.HasSuffix(trimmed, ":") {
			current = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(trimmed, ":")))
			if current != "" {
				if _, ok := lock.Platforms[current]; !ok {
					lock.Platforms[current] = soroqLockPin{}
				}
			}
			continue
		}
		if indent >= 4 && current != "" {
			key, value, ok := strings.Cut(trimmed, ":")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			pin := lock.Platforms[current]
			switch key {
			case "release_id":
				pin.ReleaseID = value
			case "version":
				pin.Version = value
			case "toolchain_version":
				pin.ToolchainVersion = value
			case "frontend_version":
				pin.FrontendVersion = value
			case "recorded_at":
				if t, err := time.Parse(time.RFC3339, value); err == nil {
					pin.RecordedAt = t
				}
			}
			lock.Platforms[current] = pin
		}
	}
	return lock
}

// renderSoroqLock renders soroq.lock by hand (mirrors renderSoroqConfig). Platform keys are sorted so
// the committed file is byte-stable regardless of which platform was released first.
func renderSoroqLock(lock soroqLock) string {
	var b strings.Builder
	b.WriteString("# soroq.lock — toolchain pins recorded by `soroq release`. Commit this file.\n")
	b.WriteString("# `soroq patch` builds a patch with the SAME toolchain that built its base release.\n")
	b.WriteString("platforms:\n")
	platforms := make([]string, 0, len(lock.Platforms))
	for platform := range lock.Platforms {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		pin := lock.Platforms[platform]
		fmt.Fprintf(&b, "  %s:\n", platform)
		fmt.Fprintf(&b, "    release_id: %s\n", strings.TrimSpace(pin.ReleaseID))
		fmt.Fprintf(&b, "    version: %s\n", strings.TrimSpace(pin.Version))
		fmt.Fprintf(&b, "    toolchain_version: %s\n", strings.TrimSpace(pin.ToolchainVersion))
		if strings.TrimSpace(pin.FrontendVersion) != "" {
			fmt.Fprintf(&b, "    frontend_version: %s\n", strings.TrimSpace(pin.FrontendVersion))
		}
		recordedAt := pin.RecordedAt
		if recordedAt.IsZero() {
			recordedAt = time.Now().UTC()
		}
		fmt.Fprintf(&b, "    recorded_at: %s\n", recordedAt.UTC().Format(time.RFC3339))
	}
	return b.String()
}

// saveSoroqLock writes soroq.lock atomically (temp + rename).
func saveSoroqLock(projectDir string, lock soroqLock) error {
	path := soroqLockPath(projectDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(renderSoroqLock(lock)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// recordSoroqLockPin merges one platform's pin into soroq.lock (so android + ios coexist) and writes it
// atomically. Called by `soroq release` ONLY when soroq ran the build.
func recordSoroqLockPin(projectDir string, platform string, pin soroqLockPin) error {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform == "" {
		return errors.New("recordSoroqLockPin: empty platform")
	}
	lock, err := loadSoroqLock(projectDir)
	if err != nil {
		return err
	}
	if lock.Platforms == nil {
		lock.Platforms = map[string]soroqLockPin{}
	}
	if pin.RecordedAt.IsZero() {
		pin.RecordedAt = time.Now().UTC()
	}
	lock.Platforms[platform] = pin
	return saveSoroqLock(projectDir, lock)
}

// loadSoroqLockPin returns the pin for (platform, releaseID) when soroq.lock pins that EXACT base
// release with a non-empty toolchain. A missing lock, a pin for a different (e.g. newer) release, or an
// empty toolchain_version all return found=false so patch falls back to its current behavior (honest
// fallback — e.g. an `--artifact` release wrote no pin at all).
func loadSoroqLockPin(projectDir string, platform string, releaseID string) (soroqLockPin, bool) {
	lock, err := loadSoroqLock(projectDir)
	if err != nil {
		return soroqLockPin{}, false
	}
	pin, ok := lock.Platforms[strings.ToLower(strings.TrimSpace(platform))]
	if !ok {
		return soroqLockPin{}, false
	}
	if strings.TrimSpace(pin.ToolchainVersion) == "" {
		return soroqLockPin{}, false
	}
	if releaseID = strings.TrimSpace(releaseID); releaseID != "" && strings.TrimSpace(pin.ReleaseID) != releaseID {
		return soroqLockPin{}, false
	}
	return pin, true
}
