package main

// toolchain_active.go — record + read the per-platform active toolchain selection made by `soroq setup`.
//
// This mirrors recordActiveFrontend/activeFrontend (frontend_cmd.go:81-88,175) but is PER-PLATFORM so
// `soroq setup android` and `soroq setup ios` can both be recorded without overwriting each other. It is
// a NEW pointer at ~/.soroq/toolchains/active.json (a regular file; every toolchains-dir walk skips
// non-dirs, so it never mis-lists as a version). It records ONLY the active toolchain pointer — it does
// NOT write soroq.lock / soroq.yaml toolchain pins (that is D004's territory; D004 READS this pointer).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// activeToolchains is the recorded per-platform active toolchain selection
// (~/.soroq/toolchains/active.json).
type activeToolchains struct {
	Platforms map[string]activeToolchainEntry `json:"platforms"`
}

// activeToolchainEntry is the chosen versions for one platform, recorded after BOTH the frontend and the
// toolchain install for that platform succeeded.
type activeToolchainEntry struct {
	ToolchainVersion string    `json:"toolchain_version"`
	FrontendVersion  string    `json:"frontend_version"`
	RecordedAt       time.Time `json:"recorded_at"`
}

func activeToolchainsPath() (string, error) {
	root, err := toolchainsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "active.json"), nil
}

// loadActiveToolchains reads the per-platform active toolchain pointer, returning an empty (non-nil-map)
// value when the file does not exist yet.
func loadActiveToolchains() (activeToolchains, error) {
	path, err := activeToolchainsPath()
	if err != nil {
		return activeToolchains{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return activeToolchains{Platforms: map[string]activeToolchainEntry{}}, nil
	}
	if err != nil {
		return activeToolchains{}, err
	}
	var a activeToolchains
	if err := json.Unmarshal(b, &a); err != nil {
		return activeToolchains{}, err
	}
	if a.Platforms == nil {
		a.Platforms = map[string]activeToolchainEntry{}
	}
	return a, nil
}

// recordActiveToolchain records the chosen versions for one platform, MERGING with any already-recorded
// platforms (so android + ios coexist). It read-modify-writes atomically (temp + rename). Writes ONLY
// under ~/.soroq/toolchains/.
func recordActiveToolchain(platform string, entry activeToolchainEntry) error {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform == "" {
		return errors.New("recordActiveToolchain: empty platform")
	}
	current, err := loadActiveToolchains()
	if err != nil {
		return err
	}
	if current.Platforms == nil {
		current.Platforms = map[string]activeToolchainEntry{}
	}
	current.Platforms[platform] = entry

	path, err := activeToolchainsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(current, "", "  ")
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
