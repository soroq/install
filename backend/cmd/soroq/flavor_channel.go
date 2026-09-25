package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// PER-FLAVOR CHANNELS: how two flavors of one app can both use Soroq at the same version.
//
// A device's runtime_id is derived by the Soroq Flutter FRONTEND from soroq.yaml's top-level app_id and
// channel plus the pubspec version (soroq_metadata.dart; mirrored by soroqManifestTrustRuntimeID). The
// frontend is handed the build flavor but does not use it, so two flavors built from one soroq.yaml
// report the SAME runtime_id and channel, and the control plane -- which serves by (app_id, runtime_id,
// channel) -- cannot tell them apart. guardFlavorRuntimeCollision therefore allows one flavor.
//
// The channel is the lever that needs no toolchain change: it is part of the runtime_id AND of the serving
// key, and the Android runtime asks for patches with the channel from the bundled metadata. A project may
// declare
//
//	flavors:
//	  prod:
//	    channel: stable
//	  dev:
//	    channel: dev
//
// and a build of a DECLARED flavor then runs with soroq.yaml's top-level channel set to that flavor's
// channel. Registration, patch selection, publication and rollback all use it, so each flavor's releases
// and patches live on their own channel with their own runtime ids.
//
// The published frontend reads soroq.yaml from the project root and nowhere else, so the build sees an
// EFFECTIVE soroq.yaml: the original with exactly one top-level line changed. It is swapped in for the
// build only and restored byte-for-byte afterwards (also on failure). The original is first saved under
// .soroq/, so a process killed mid-build is repaired by the next soroq command (and a second, concurrent
// swap is refused rather than stacked). The preview asset the build regenerates is restored the same way.
//
// Undeclared flavors keep the existing behaviour (and its one-flavor collision guard). Declaring
// `flavors:` changes nothing for unflavored builds.

const flavorChannelSwapDir = "flavor-channel-swap"

var topLevelChannelLine = regexp.MustCompile(`(?m)^channel:[^\n]*$`)

// flavorChannels parses soroq.yaml's optional `flavors:` map. Absent => empty map, no error.
func flavorChannels(configBytes []byte) (map[string]string, error) {
	var doc struct {
		Channel string `yaml:"channel"`
		Flavors map[string]struct {
			Channel string `yaml:"channel"`
		} `yaml:"flavors"`
	}
	if err := yaml.Unmarshal(configBytes, &doc); err != nil {
		return nil, fmt.Errorf("soroq.yaml: %w", err)
	}
	out := map[string]string{}
	owner := map[string]string{}
	names := make([]string, 0, len(doc.Flavors))
	for name := range doc.Flavors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateFlavorName(name); err != nil {
			return nil, fmt.Errorf("soroq.yaml flavors: %w", err)
		}
		ch := strings.TrimSpace(doc.Flavors[name].Channel)
		if ch == "" {
			return nil, fmt.Errorf("soroq.yaml flavors.%s must set a channel", name)
		}
		if !looksLikeChannel(ch) {
			return nil, fmt.Errorf("soroq.yaml flavors.%s.channel %q is not a valid channel (lowercase letters, digits, '.', '_', '-')", name, ch)
		}
		// Two flavors on one channel would share runtime ids again: the isolation this exists for.
		if other, dup := owner[ch]; dup {
			return nil, fmt.Errorf("soroq.yaml flavors %q and %q both use channel %q; each flavor needs its own channel", other, name, ch)
		}
		owner[ch] = name
		out[name] = ch
	}
	return out, nil
}

// resolveFlavorChannel reports the channel declared for flavor, if any.
func resolveFlavorChannel(projectDir, flavor string) (string, bool, error) {
	if flavor == "" {
		return "", false, nil
	}
	configBytes, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	channels, err := flavorChannels(configBytes)
	if err != nil {
		return "", false, err
	}
	ch, ok := channels[flavor]
	return ch, ok, nil
}

// effectiveSoroqYAML returns original with its top-level channel set to channel. Everything else is
// byte-identical, and the result is re-parsed to prove that only the channel changed.
func effectiveSoroqYAML(original []byte, channel string) ([]byte, error) {
	if !looksLikeChannel(channel) {
		return nil, fmt.Errorf("invalid channel %q", channel)
	}
	line := "channel: " + channel
	var out []byte
	switch n := len(topLevelChannelLine.FindAllIndex(original, -1)); n {
	case 0:
		out = append([]byte{}, original...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, []byte(line+"\n")...)
	case 1:
		out = topLevelChannelLine.ReplaceAll(original, []byte(line))
	default:
		return nil, fmt.Errorf("soroq.yaml has %d top-level channel lines", n)
	}
	var before, after map[string]any
	if err := yaml.Unmarshal(original, &before); err != nil {
		return nil, fmt.Errorf("soroq.yaml: %w", err)
	}
	if err := yaml.Unmarshal(out, &after); err != nil {
		return nil, fmt.Errorf("effective soroq.yaml does not parse: %w", err)
	}
	if got, _ := after["channel"].(string); got != channel {
		return nil, fmt.Errorf("effective soroq.yaml channel is %q, want %q", got, channel)
	}
	delete(before, "channel")
	delete(after, "channel")
	if fmt.Sprint(before) != fmt.Sprint(after) {
		return nil, errors.New("setting the flavor channel would change soroq.yaml beyond its channel")
	}
	return out, nil
}

type flavorChannelSwapFile struct {
	rel     string // project-relative path
	saved   string // backup name under the swap dir
	existed bool
}

var flavorChannelSwapFiles = []flavorChannelSwapFile{
	{rel: "soroq.yaml", saved: "soroq.yaml.original"},
	{rel: soroqBundledMetadataAsset, saved: "soroq_metadata.json.original"},
}

func flavorChannelSwapPath(projectDir string) string {
	return filepath.Join(projectDir, ".soroq", flavorChannelSwapDir)
}

// The swap directory is also the LOCK: it is created with a plain (exclusive) Mkdir by the one process
// that swaps, and its `swap.json` -- written atomically and LAST, after every backup -- records that
// process and which files existed. Recovery therefore:
//   - leaves a swap owned by a live other process alone and refuses to read a swapped soroq.yaml;
//   - discards a dir without swap.json (killed while backing up: soroq.yaml was never changed);
//   - restores exactly the files swap.json lists, and removes a file it records as absent.
const flavorChannelSwapRecord = "swap.json"

type flavorChannelSwapState struct {
	PID   int             `json:"pid"`
	Files map[string]bool `json:"files"` // project-relative path -> existed before the swap
}

func readFlavorChannelSwapState(dir string) (*flavorChannelSwapState, error) {
	raw, err := os.ReadFile(filepath.Join(dir, flavorChannelSwapRecord))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st flavorChannelSwapState
	if err := json.Unmarshal(raw, &st); err != nil || st.Files == nil {
		return nil, fmt.Errorf("unreadable %s in %s", flavorChannelSwapRecord, dir)
	}
	return &st, nil
}

// processAlive reports whether pid is a running process (signal 0 probes without delivering).
var processAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// recoverInterruptedFlavorChannelSwap restores the project files a killed flavored build left swapped.
func recoverInterruptedFlavorChannelSwap(projectDir string) error {
	dir := flavorChannelSwapPath(projectDir)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	st, err := readFlavorChannelSwapState(dir)
	if err != nil {
		return fmt.Errorf("an interrupted flavored build left %s in an unknown state (%v); inspect soroq.yaml and remove the directory", dir, err)
	}
	if st == nil {
		// Killed while backing up: nothing on disk was swapped yet.
		return os.RemoveAll(dir)
	}
	if st.PID == os.Getpid() {
		return nil // our own swap, in progress
	}
	if processAlive(st.PID) {
		return fmt.Errorf("a flavored build (pid %d) is running in this project with soroq.yaml temporarily on its flavor channel; wait for it to finish", st.PID)
	}
	if err := restoreFlavorChannelSwap(projectDir, st); err != nil {
		return fmt.Errorf("an interrupted flavored build left soroq.yaml swapped and it could not be restored from %s: %w", dir, err)
	}
	fmt.Fprintf(os.Stderr, "soroq: restored soroq.yaml after an interrupted flavored build (%s)\n", dir)
	return nil
}

func restoreFlavorChannelSwap(projectDir string, st *flavorChannelSwapState) error {
	dir := flavorChannelSwapPath(projectDir)
	for _, f := range flavorChannelSwapFiles {
		existed, recorded := st.Files[f.rel]
		if !recorded {
			return fmt.Errorf("%s does not record %s", flavorChannelSwapRecord, f.rel)
		}
		target := filepath.Join(projectDir, filepath.FromSlash(f.rel))
		if !existed {
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, f.saved))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(target, data); err != nil {
			return err
		}
	}
	return os.RemoveAll(dir)
}

// NO FILE CHANGE WITH A FLAVOR-AWARE FRONTEND.
//
// The soroq CLI's own reads of soroq.yaml during a flavored command go through readProjectSoroqYAML,
// which applies the active flavor channel IN MEMORY. The only other reader is the Soroq Flutter
// frontend's asset bundler (soroq_metadata.dart), which writes the bundled channel/runtime_id. A frontend
// that ships frontendFlavorChannelsMarker resolves `flavors.<flavor>.channel` itself from the --flavor
// it is building, so nothing on disk changes. Only an older frontend still needs the swap below.
const (
	frontendFlavorChannelsMarker = "bin/cache/soroq/flavor_channels_v1.json"
	frontendFlavorChannelsSchema = "soroq.frontend_capability.flavor_channels.v1"
)

var (
	activeFlavorChannelsMu sync.Mutex
	activeFlavorChannels   = map[string]string{} // absolute project dir -> channel
)

// frontendSupportsFlavorChannelsFn reports whether the Soroq Flutter frontend a build will use resolves
// per-flavor channels itself. The marker is part of the signed frontend archive.
var frontendSupportsFlavorChannelsFn = func() bool {
	bin, err := resolveSoroqFlutterBin()
	if err != nil {
		return false
	}
	root, err := flutterRootFromBin(bin)
	if err != nil {
		return false
	}
	return frontendRootSupportsFlavorChannels(root)
}

func frontendRootSupportsFlavorChannels(flutterRoot string) bool {
	raw, err := os.ReadFile(filepath.Join(flutterRoot, filepath.FromSlash(frontendFlavorChannelsMarker)))
	if err != nil {
		return false
	}
	var doc struct {
		Schema string `json:"schema"`
	}
	return json.Unmarshal(raw, &doc) == nil && doc.Schema == frontendFlavorChannelsSchema
}

func absProjectKey(projectDir string) string {
	if abs, err := filepath.Abs(projectDir); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(projectDir)
}

// readProjectSoroqYAML is soroq.yaml as the CURRENT command sees it: the file on disk, with the active
// flavor's channel applied when a flavored command is running.
func readProjectSoroqYAML(projectDir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		return nil, err
	}
	activeFlavorChannelsMu.Lock()
	ch, ok := activeFlavorChannels[absProjectKey(projectDir)]
	activeFlavorChannelsMu.Unlock()
	if !ok {
		return raw, nil
	}
	return effectiveSoroqYAML(raw, ch)
}

// withFlavorChannel runs build as the flavor on channel: the CLI's reads see that channel in memory, and
// soroq.yaml on disk is rewritten (then restored byte-for-byte) ONLY for a frontend that cannot resolve
// flavor channels itself. A soroq.yaml already on that channel is not touched either way.
func withFlavorChannel(projectDir, channel string, build func() error) (err error) {
	configPath := filepath.Join(projectDir, "soroq.yaml")
	original, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(parseTopLevelYaml(original)["channel"]) == channel {
		return build()
	}
	if _, err := effectiveSoroqYAML(original, channel); err != nil {
		return err
	}
	key := absProjectKey(projectDir)
	activeFlavorChannelsMu.Lock()
	activeFlavorChannels[key] = channel
	activeFlavorChannelsMu.Unlock()
	defer func() {
		activeFlavorChannelsMu.Lock()
		delete(activeFlavorChannels, key)
		activeFlavorChannelsMu.Unlock()
	}()
	if frontendSupportsFlavorChannelsFn() {
		return build()
	}
	effective, err := effectiveSoroqYAML(original, channel)
	if err != nil {
		return err
	}
	dir := flavorChannelSwapPath(projectDir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	// Exclusive: exactly one process can create the swap dir, so it doubles as the lock.
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("another flavored build is using %s; wait for it, or run a soroq command after it exits to restore soroq.yaml", dir)
		}
		return err
	}
	st := &flavorChannelSwapState{PID: os.Getpid(), Files: map[string]bool{}}
	for _, f := range flavorChannelSwapFiles {
		data, readErr := os.ReadFile(filepath.Join(projectDir, filepath.FromSlash(f.rel)))
		if errors.Is(readErr, os.ErrNotExist) {
			st.Files[f.rel] = false
			continue
		}
		if readErr != nil {
			_ = os.RemoveAll(dir)
			return readErr
		}
		if writeErr := writeFileAtomic(filepath.Join(dir, f.saved), data); writeErr != nil {
			_ = os.RemoveAll(dir)
			return writeErr
		}
		st.Files[f.rel] = true
	}
	stateBytes, _ := json.Marshal(st)
	if writeErr := writeFileAtomic(filepath.Join(dir, flavorChannelSwapRecord), stateBytes); writeErr != nil {
		_ = os.RemoveAll(dir)
		return writeErr
	}
	defer func() {
		if restoreErr := restoreFlavorChannelSwap(projectDir, st); restoreErr != nil && err == nil {
			err = fmt.Errorf("restore soroq.yaml after the flavored build: %w (the original is in %s)", restoreErr, dir)
		}
	}()
	if err := writeFileAtomic(configPath, effective); err != nil {
		return err
	}
	return build()
}

// resolveReleaseFlavorChannel resolves a declared flavor channel and refuses an explicit --channel that
// disagrees with it.
func resolveReleaseFlavorChannel(fs *flag.FlagSet, projectDir, flavor, channelFlag string) (string, bool, error) {
	ch, declared, err := resolveFlavorChannel(projectDir, flavor)
	if err != nil || !declared {
		return "", false, err
	}
	if flagWasSet(fs, "channel") && strings.TrimSpace(channelFlag) != ch {
		return "", false, fmt.Errorf("--channel %q conflicts with soroq.yaml flavors.%s.channel %q; omit --channel for a declared flavor", strings.TrimSpace(channelFlag), flavor, ch)
	}
	return ch, true, nil
}

func declaredChannel(ch string, declared bool) string {
	if declared {
		return ch
	}
	return ""
}

// runWithFlavorChannel runs build under a declared flavor channel, or as-is when channel is empty.
func runWithFlavorChannel(projectDir, channel string, build func() error) error {
	if channel == "" {
		return build()
	}
	return withFlavorChannel(projectDir, channel, build)
}

// releaseRecordedAsFlavor reports whether releaseID is recorded (soroq-flavor.json) as flavor.
func releaseRecordedAsFlavor(projectDir, platform, releaseID, flavor string) bool {
	got, known, err := knownReleaseFlavor(projectDir, platform, releaseID)
	return err == nil && known && got == flavor
}

// latestRecordedFlavorRelease returns the most recently recorded release of platform built as flavor,
// from the soroq-flavor.json records. cli-state and soroq.lock hold only the LATEST release, which with
// several flavors is often another flavor's; this is how a declared flavor finds its own base.
func latestRecordedFlavorRelease(projectDir, platform, flavor string) (string, bool) {
	matches, _ := filepath.Glob(filepath.Join(projectDir, ".soroq", "releases", "*", releaseFlavorRecordFile))
	best, bestAt := "", time.Time{}
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var rec releaseFlavorRecord
		if json.Unmarshal(raw, &rec) != nil || rec.Schema != releaseFlavorRecordSchema {
			continue
		}
		if rec.Platform != platform || rec.Flavor != flavor || strings.TrimSpace(rec.ReleaseID) == "" {
			continue
		}
		// Only a record that sits where its own release id says it should (knownReleaseFlavor's rule).
		if filepath.Clean(m) != filepath.Clean(releaseFlavorRecordPath(projectDir, rec.ReleaseID)) {
			continue
		}
		if best == "" || rec.RecordedAt.After(bestAt) {
			best, bestAt = rec.ReleaseID, rec.RecordedAt
		}
	}
	return best, best != ""
}
