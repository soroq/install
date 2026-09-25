package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const flavorChannelsYAMLTail = "flavors:\n  prod:\n    channel: stable\n  dev:\n    channel: dev\n"

// soroqYAMLWithRealTrust is testSoroqYAML with a real Ed25519 key, so the CLI's byte-exact mirror of the
// fork's runtime_id derivation (buildSoroqBundledMetadata) runs for real instead of bailing out.
func soroqYAMLWithRealTrust(t *testing.T, channel, tail string) string {
	t.Helper()
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return "app_id: com.example.app\n" +
		"channel: " + channel + "\n" +
		"runtime_id_strategy: manifest_trust_v1\n" +
		"manifest_trust:\n" +
		"  keyset_version: 1\n" +
		"  keys:\n" +
		"    - id: prod-primary\n" +
		"      public_key: " + base64.RawURLEncoding.EncodeToString(pub) + "\n" +
		tail
}

func TestFlavorChannelsParseAndValidate(t *testing.T) {
	got, err := flavorChannels([]byte(soroqYAMLWithRealTrust(t, "stable", flavorChannelsYAMLTail)))
	if err != nil || len(got) != 2 || got["prod"] != "stable" || got["dev"] != "dev" {
		t.Fatalf("flavors = %v, %v", got, err)
	}
	if got, err := flavorChannels([]byte(testSoroqYAML("com.example.app", "stable"))); err != nil || len(got) != 0 {
		t.Fatalf("no flavors block must mean none declared: %v %v", got, err)
	}
	for name, tail := range map[string]string{
		"missing channel":   "flavors:\n  prod: {}\n",
		"invalid channel":   "flavors:\n  prod:\n    channel: Prod Channel\n",
		"shared channel":    "flavors:\n  prod:\n    channel: x\n  dev:\n    channel: x\n",
		"invalid flavor":    "flavors:\n  ../prod:\n    channel: x\n",
		"flavors not a map": "flavors: prod\n",
	} {
		if _, err := flavorChannels([]byte(soroqYAMLWithRealTrust(t, "stable", tail))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEffectiveSoroqYAMLChangesOnlyTheChannelLine(t *testing.T) {
	orig := []byte("# comment\napp_id: a.b\nchannel: \"stable\" # main\nruntime_id_strategy: manifest_trust_v1\nflavors:\n  dev:\n    channel: dev\n")
	out, err := effectiveSoroqYAML(orig, "dev")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(orig), "channel: \"stable\" # main", "channel: dev", 1)
	if string(out) != want {
		t.Fatalf("effective:\n%s\nwant:\n%s", out, want)
	}
	// Nested `channel:` lines (the flavors block) are not top-level and are untouched.
	if !strings.Contains(string(out), "  dev:\n    channel: dev\n") {
		t.Fatal("nested flavor channel changed")
	}
	appended, err := effectiveSoroqYAML([]byte("app_id: a.b"), "dev")
	if err != nil || string(appended) != "app_id: a.b\nchannel: dev\n" {
		t.Fatalf("no channel line: %q %v", appended, err)
	}
	if _, err := effectiveSoroqYAML([]byte("app_id: a\nchannel: x\nchannel: y\n"), "dev"); err == nil {
		t.Fatal("two top-level channel lines accepted")
	}
	if _, err := effectiveSoroqYAML(orig, "Bad Channel"); err == nil {
		t.Fatal("invalid channel accepted")
	}
}

func snapshotFlavorSwapFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, f := range flavorChannelSwapFiles {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.rel))); err == nil {
			out[f.rel] = b
		}
	}
	return out
}

func assertFlavorSwapRestored(t *testing.T, dir string, before map[string][]byte) {
	t.Helper()
	after := snapshotFlavorSwapFiles(t, dir)
	if len(after) != len(before) {
		t.Fatalf("project files after the swap: %d, before: %d", len(after), len(before))
	}
	for k, v := range before {
		if !bytes.Equal(after[k], v) {
			t.Fatalf("%s not restored byte-for-byte:\n%s", k, after[k])
		}
	}
	if _, err := os.Stat(flavorChannelSwapPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("swap dir left behind: %v", err)
	}
}

func TestWithFlavorChannelSwapsForTheBuildAndRestoresExactly(t *testing.T) {
	for _, withPreview := range []bool{true, false} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "soroq.yaml"), soroqYAMLWithRealTrust(t, "stable", flavorChannelsYAMLTail))
		if err := os.MkdirAll(filepath.Join(dir, "soroq"), 0o755); err != nil {
			t.Fatal(err)
		}
		if withPreview {
			writeFile(t, filepath.Join(dir, "soroq", "soroq_metadata.json"), `{"preview":"stable"}`)
		}
		before := snapshotFlavorSwapFiles(t, dir)
		var seen string
		err := withFlavorChannel(dir, "dev", func() error {
			b, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml"))
			seen = parseTopLevelYaml(b)["channel"]
			// The real build regenerates the preview asset from the swapped soroq.yaml.
			writeFile(t, filepath.Join(dir, "soroq", "soroq_metadata.json"), `{"preview":"dev"}`)
			return nil
		})
		if err != nil || seen != "dev" {
			t.Fatalf("build saw channel %q, err %v", seen, err)
		}
		assertFlavorSwapRestored(t, dir, before)
	}
}

func TestWithFlavorChannelRestoresOnBuildFailure(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "soroq.yaml"), soroqYAMLWithRealTrust(t, "stable", flavorChannelsYAMLTail))
	before := snapshotFlavorSwapFiles(t, dir)
	err := withFlavorChannel(dir, "dev", func() error { return os.ErrPermission })
	if err != os.ErrPermission {
		t.Fatalf("build error not returned: %v", err)
	}
	assertFlavorSwapRestored(t, dir, before)
}

func TestInterruptedFlavorChannelSwapIsRecoveredAndNeverStacked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "soroq.yaml"), soroqYAMLWithRealTrust(t, "stable", flavorChannelsYAMLTail))
	before := snapshotFlavorSwapFiles(t, dir)

	// The state a process killed mid-build leaves: original saved under .soroq/, swapped file on disk,
	// preview regenerated from the swapped file (it did not exist before).
	if err := os.MkdirAll(flavorChannelSwapPath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(flavorChannelSwapPath(dir), "soroq.yaml.original"), string(before["soroq.yaml"]))
	// The swap record a killed process (pid 999999, not running) left, written last after the backups.
	prevAlive := processAlive
	processAlive = func(pid int) bool { return false }
	t.Cleanup(func() { processAlive = prevAlive })
	writeFile(t, filepath.Join(flavorChannelSwapPath(dir), flavorChannelSwapRecord),
		`{"pid":999999,"files":{"soroq.yaml":true,"soroq/soroq_metadata.json":false}}`)
	eff, err := effectiveSoroqYAML(before["soroq.yaml"], "dev")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "soroq.yaml"), string(eff))
	if err := os.MkdirAll(filepath.Join(dir, "soroq"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "soroq", "soroq_metadata.json"), `{"preview":"dev"}`)

	// While that state exists no second swap may start (it would save the SWAPPED file as "original").
	if err := withFlavorChannel(dir, "other", func() error { t.Fatal("stacked a second swap"); return nil }); err == nil ||
		!strings.Contains(err.Error(), "another flavored build") {
		t.Fatalf("second swap not refused: %v", err)
	}
	// Every flavored command recovers first.
	if err := recoverInterruptedFlavorChannelSwap(dir); err != nil {
		t.Fatal(err)
	}
	assertFlavorSwapRestored(t, dir, before)
	if err := recoverInterruptedFlavorChannelSwap(dir); err != nil {
		t.Fatalf("recovery with nothing to recover: %v", err)
	}
}

// forkLikeFlavorBuild emulates the fork frontend: the artifact's bundled metadata is derived from the
// soroq.yaml ON DISK AT BUILD TIME (the CLI's byte-exact mirror of the fork's derivation).
func forkLikeFlavorBuild(t *testing.T, dir string, out func(flavor string) string) func(args []string) error {
	return func(args []string) error {
		flavor := ""
		for i, a := range args {
			if a == "--flavor" && i+1 < len(args) {
				flavor = args[i+1]
			}
		}
		cfg, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml"))
		pub, _ := os.ReadFile(filepath.Join(dir, "pubspec.yaml"))
		meta, err := buildSoroqBundledMetadata(cfg, pub)
		if err != nil {
			t.Fatalf("fork-like metadata: %v", err)
		}
		raw, err := renderSoroqBundledMetadataJSON(meta)
		if err != nil {
			t.Fatal(err)
		}
		path := out(flavor)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeArtifactZip(t, path, map[string][]byte{
			"assets/flutter_assets/soroq/soroq_metadata.json": raw,
			"lib/arm64-v8a/libapp.so":                         []byte("app-" + flavor),
		})
		return nil
	}
}

// newForkLikeFlavorProject is a project whose pubspec carries the name and version the real metadata
// derivation requires.
func newForkLikeFlavorProject(t *testing.T, tail string) string {
	t.Helper()
	dir := newFlavorReleaseProject(t)
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: example\nversion: 1.2.3+45\n"+testSoroqFlutterPubspec)
	writeFile(t, filepath.Join(dir, "soroq.yaml"), soroqYAMLWithRealTrust(t, "stable", tail))
	return dir
}

func flavorAABPath(dir, flavor string) string {
	return filepath.Join(dir, "build", "app", "outputs", "bundle", flavor+"Release", "app-"+flavor+"-release.aab")
}

// Two declared flavors of ONE app at ONE version: each release is built with its own channel, so each
// carries its own channel and runtime_id, is registered on that channel, and neither trips the
// one-flavor collision guard. soroq.yaml is byte-identical afterwards.
func TestDeclaredFlavorsReleaseOnTheirOwnChannelsWithDistinctRuntimeIDs(t *testing.T) {
	dir := newForkLikeFlavorProject(t, flavorChannelsYAMLTail)
	origYAML, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml"))
	plane := newFakeReleasePlane(t)
	stubAndroidReleaseBuild(t, forkLikeFlavorBuild(t, dir, func(f string) string { return flavorAABPath(dir, f) }))

	for _, flavor := range []string{"prod", "dev"} {
		args := []string{"--project-dir", dir, "--api", plane.server.URL, "--arch", "arm64-v8a", "--flavor", flavor}
		if err := runReleaseAndroid(args); err != nil {
			t.Fatalf("release %s: %v", flavor, err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml")); !bytes.Equal(b, origYAML) {
			t.Fatalf("soroq.yaml changed after the %s release:\n%s", flavor, b)
		}
	}
	if len(plane.registered) != 2 {
		t.Fatalf("registered %d releases", len(plane.registered))
	}
	prod, dev := plane.registered[0], plane.registered[1]
	if prod.Channel != "stable" || dev.Channel != "dev" {
		t.Fatalf("channels: prod=%q dev=%q", prod.Channel, dev.Channel)
	}
	if prod.RuntimeID == dev.RuntimeID || prod.RuntimeID == "" {
		t.Fatalf("runtime ids must differ per flavor: %q %q", prod.RuntimeID, dev.RuntimeID)
	}
}

// Control: the SAME two flavors WITHOUT a `flavors:` declaration share a runtime_id, and the second is
// refused by the existing collision guard -- the limit per-flavor channels remove.
func TestUndeclaredFlavorsStillHitTheCollisionGuard(t *testing.T) {
	dir := newForkLikeFlavorProject(t, "")
	plane := newFakeReleasePlane(t)
	stubAndroidReleaseBuild(t, forkLikeFlavorBuild(t, dir, func(f string) string { return flavorAABPath(dir, f) }))
	if err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane.server.URL, "--arch", "arm64-v8a", "--flavor", "prod"}); err != nil {
		t.Fatalf("first flavor: %v", err)
	}
	err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane.server.URL, "--arch", "arm64-v8a", "--flavor", "dev"})
	if err == nil || len(plane.registered) != 1 {
		t.Fatalf("second undeclared flavor at one version must be refused: err=%v registered=%d", err, len(plane.registered))
	}
}

func TestDeclaredFlavorRefusesAConflictingChannelFlag(t *testing.T) {
	dir := newForkLikeFlavorProject(t, flavorChannelsYAMLTail)
	plane := newFakeReleasePlane(t)
	built := false
	stubAndroidReleaseBuild(t, func([]string) error { built = true; return nil })
	err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane.server.URL, "--flavor", "dev", "--channel", "stable"})
	if err == nil || !strings.Contains(err.Error(), "conflicts with soroq.yaml flavors.dev.channel") || built {
		t.Fatalf("conflicting --channel: err=%v built=%v", err, built)
	}
}

// An artifact built WITHOUT the swap (e.g. a stale build on the top-level channel) must not register
// as the declared flavor: the channel check proves the flavored build really used its channel.
func TestDeclaredFlavorRefusesAnArtifactOnTheWrongChannel(t *testing.T) {
	dir := newForkLikeFlavorProject(t, flavorChannelsYAMLTail)
	plane := newFakeReleasePlane(t)
	stubAndroidReleaseBuild(t, func(args []string) error {
		// Ignores the swap: derives from the ORIGINAL soroq.yaml, as a build bypassing it would.
		return forkLikeFlavorBuild(t, dir, func(f string) string { return flavorAABPath(dir, f) })(args)
	})
	// Break the swap for this build only by pre-restoring the original inside the build.
	orig, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml"))
	prev := androidReleaseBuildFn
	androidReleaseBuildFn = func(pd, at, tc string, args []string) error {
		swapped, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml"))
		writeFile(t, filepath.Join(dir, "soroq.yaml"), string(orig))
		err := prev(pd, at, tc, args)
		writeFile(t, filepath.Join(dir, "soroq.yaml"), string(swapped))
		return err
	}
	t.Cleanup(func() { androidReleaseBuildFn = prev })
	err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane.server.URL, "--arch", "arm64-v8a", "--flavor", "dev"})
	if err == nil || len(plane.registered) != 0 || !strings.Contains(err.Error(), `artifact channel "stable" does not match requested channel "dev"`) {
		t.Fatalf("artifact on the wrong channel must be refused by the channel check: err=%v registered=%+v", err, plane.registered)
	}
}

func TestLatestRecordedFlavorReleasePicksTheNewestOfThatFlavorOnly(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for i, r := range []struct{ id, flavor, platform string }{
		{"prod-old", "prod", "android"},
		{"dev-new", "dev", "android"},
		{"prod-new", "prod", "android"},
		{"prod-ios", "prod", "ios"},
	} {
		if err := writeReleaseFlavorRecord(dir, releaseFlavorRecord{Schema: releaseFlavorRecordSchema, Platform: r.platform,
			ReleaseID: r.id, RuntimeID: "rt-" + r.id, Flavor: r.flavor, RecordedAt: at.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if id, ok := latestRecordedFlavorRelease(dir, "android", "prod"); !ok || id != "prod-new" {
		t.Fatalf("prod = %q %v, want prod-new (not another flavor's newer release, not another platform's)", id, ok)
	}
	if id, ok := latestRecordedFlavorRelease(dir, "android", "dev"); !ok || id != "dev-new" {
		t.Fatalf("dev = %q %v", id, ok)
	}
	if _, ok := latestRecordedFlavorRelease(dir, "android", "staging"); ok {
		t.Fatal("found a release for a flavor never recorded")
	}
	// A record copied under another release's directory is not trusted.
	src := releaseFlavorRecordPath(dir, "prod-new")
	raw, _ := os.ReadFile(src)
	if err := os.MkdirAll(filepath.Join(dir, ".soroq", "releases", "zz-copied"), 0o755); err != nil {
		t.Fatal(err)
	}
	misplaced := strings.Replace(string(raw), "2026-09-25T14:00:00Z", "2027-01-01T00:00:00Z", 1)
	writeFile(t, filepath.Join(dir, ".soroq", "releases", "zz-copied", releaseFlavorRecordFile), misplaced)
	if id, _ := latestRecordedFlavorRelease(dir, "android", "prod"); id != "prod-new" {
		t.Fatalf("a misplaced record was trusted: %q", id)
	}
}

func stubFlavorAwareFrontend(t *testing.T, aware bool) {
	t.Helper()
	prev := frontendSupportsFlavorChannelsFn
	frontendSupportsFlavorChannelsFn = func() bool { return aware }
	t.Cleanup(func() { frontendSupportsFlavorChannelsFn = prev })
}

// With a flavor-aware frontend nothing on disk changes: the CLI's own reads see the flavor channel in
// memory, and only for the duration of the command.
func TestFlavorAwareFrontendNeedsNoFileChange(t *testing.T) {
	stubFlavorAwareFrontend(t, true)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "soroq.yaml"), soroqYAMLWithRealTrust(t, "stable", flavorChannelsYAMLTail))
	if err := os.MkdirAll(filepath.Join(dir, "soroq"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := snapshotFlavorSwapFiles(t, dir)
	var inMemory string
	err := withFlavorChannel(dir, "dev", func() error {
		if !bytes.Equal(snapshotFlavorSwapFiles(t, dir)["soroq.yaml"], before["soroq.yaml"]) {
			t.Fatal("soroq.yaml changed on disk during the build")
		}
		if _, err := os.Stat(flavorChannelSwapPath(dir)); !os.IsNotExist(err) {
			t.Fatal("a swap dir was created")
		}
		b, err := readProjectSoroqYAML(dir)
		if err != nil {
			return err
		}
		inMemory = parseTopLevelYaml(b)["channel"]
		return nil
	})
	if err != nil || inMemory != "dev" {
		t.Fatalf("in-memory channel %q, err %v", inMemory, err)
	}
	b, _ := readProjectSoroqYAML(dir)
	if got := parseTopLevelYaml(b)["channel"]; got != "stable" {
		t.Fatalf("the override outlived the command: %q", got)
	}
	assertFlavorSwapRestored(t, dir, before)
}

func TestFrontendFlavorChannelsMarker(t *testing.T) {
	root := t.TempDir()
	if frontendRootSupportsFlavorChannels(root) {
		t.Fatal("no marker must mean unsupported")
	}
	p := filepath.Join(root, filepath.FromSlash(frontendFlavorChannelsMarker))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, `{"schema":"something.else"}`)
	if frontendRootSupportsFlavorChannels(root) {
		t.Fatal("a marker with another schema must not count")
	}
	writeFile(t, p, `{"schema":"`+frontendFlavorChannelsSchema+`"}`)
	if !frontendRootSupportsFlavorChannels(root) {
		t.Fatal("the marker was not recognised")
	}
}

// newForkFrontendBuild emulates the FLAVOR-AWARE frontend (soroq_metadata.dart with
// _soroqFlavorChannel): it reads soroq.yaml from disk and applies flavors.<--flavor>.channel itself.
func newForkFrontendBuild(t *testing.T, dir string, original []byte) func(args []string) error {
	return func(args []string) error {
		cfg, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml"))
		if !bytes.Equal(cfg, original) {
			t.Fatal("soroq.yaml was modified on disk while a flavor-aware frontend built")
		}
		flavor := ""
		for i, a := range args {
			if a == "--flavor" && i+1 < len(args) {
				flavor = args[i+1]
			}
		}
		if chans, _ := flavorChannels(cfg); chans[flavor] != "" {
			cfg, _ = effectiveSoroqYAML(cfg, chans[flavor])
		}
		pub, _ := os.ReadFile(filepath.Join(dir, "pubspec.yaml"))
		meta, err := buildSoroqBundledMetadata(cfg, pub)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := renderSoroqBundledMetadataJSON(meta)
		if err := os.MkdirAll(filepath.Dir(flavorAABPath(dir, flavor)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeArtifactZip(t, flavorAABPath(dir, flavor), map[string][]byte{
			"assets/flutter_assets/soroq/soroq_metadata.json": raw,
			"lib/arm64-v8a/libapp.so":                         []byte("app-" + flavor),
		})
		return nil
	}
}

func TestDeclaredFlavorsWithAFlavorAwareFrontendNeverTouchSoroqYAML(t *testing.T) {
	stubFlavorAwareFrontend(t, true)
	dir := newForkLikeFlavorProject(t, flavorChannelsYAMLTail)
	orig, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml"))
	plane := newFakeReleasePlane(t)
	stubAndroidReleaseBuild(t, newForkFrontendBuild(t, dir, orig))
	for _, f := range []string{"prod", "dev"} {
		if err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane.server.URL, "--arch", "arm64-v8a", "--flavor", f}); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if len(plane.registered) != 2 || plane.registered[0].Channel != "stable" || plane.registered[1].Channel != "dev" ||
		plane.registered[0].RuntimeID == plane.registered[1].RuntimeID {
		t.Fatalf("registered %+v", plane.registered)
	}
}

// Killed while backing up (no swap record yet): soroq.yaml was never changed, so recovery must NOT
// restore anything -- a partial backup would otherwise overwrite an intact file.
func TestInterruptedBackupIsDiscardedNotRestored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "soroq.yaml"), soroqYAMLWithRealTrust(t, "stable", flavorChannelsYAMLTail))
	if err := os.MkdirAll(filepath.Join(dir, "soroq"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "soroq", "soroq_metadata.json"), `{"preview":"stable"}`)
	before := snapshotFlavorSwapFiles(t, dir)
	if err := os.MkdirAll(flavorChannelSwapPath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(flavorChannelSwapPath(dir), "soroq.yaml.original"), "app_id: trunc")
	if err := recoverInterruptedFlavorChannelSwap(dir); err != nil {
		t.Fatal(err)
	}
	assertFlavorSwapRestored(t, dir, before)
}

// A swap owned by another LIVE process is never recovered underneath it, and commands refuse to read
// its swapped soroq.yaml; the owning process itself is not blocked by its own swap.
func TestLiveSwapOfAnotherProcessIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "soroq.yaml"), soroqYAMLWithRealTrust(t, "dev", flavorChannelsYAMLTail))
	if err := os.MkdirAll(flavorChannelSwapPath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(flavorChannelSwapPath(dir), "soroq.yaml.original"), "original")
	writeFile(t, filepath.Join(flavorChannelSwapPath(dir), flavorChannelSwapRecord),
		`{"pid":424242,"files":{"soroq.yaml":true,"soroq/soroq_metadata.json":false}}`)
	prevAlive := processAlive
	processAlive = func(pid int) bool { return pid == 424242 }
	t.Cleanup(func() { processAlive = prevAlive })
	err := recoverInterruptedFlavorChannelSwap(dir)
	if err == nil || !strings.Contains(err.Error(), "pid 424242") {
		t.Fatalf("a live swap must be refused, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml")); strings.Contains(string(b), "original") {
		t.Fatal("recovered underneath a live build")
	}
	if _, err := inspectProject(dir); err == nil {
		t.Fatal("inspectProject must refuse while another process holds the swap")
	}
	writeFile(t, filepath.Join(flavorChannelSwapPath(dir), flavorChannelSwapRecord),
		fmt.Sprintf(`{"pid":%d,"files":{"soroq.yaml":true,"soroq/soroq_metadata.json":false}}`, os.Getpid()))
	if err := recoverInterruptedFlavorChannelSwap(dir); err != nil {
		t.Fatalf("own swap must be left alone: %v", err)
	}
}

// On a fresh clone (.soroq/ is not committed) an older flavor's release is known only through its
// <platform>@<flavor> lock pin.
func TestKnownReleaseFlavorReadsPerFlavorLockPins(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []soroqLockPin{{ReleaseID: "rel-prod", Version: "1", ToolchainVersion: "tc", Flavor: "prod"},
		{ReleaseID: "rel-dev", Version: "1", ToolchainVersion: "tc", Flavor: "dev"}} {
		if err := recordSoroqLockPin(dir, "android", p); err != nil {
			t.Fatal(err)
		}
		if err := recordSoroqLockPin(dir, soroqLockFlavorKey("android", p.Flavor), p); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{"rel-prod": "prod", "rel-dev": "dev"} {
		if f, known, err := knownReleaseFlavor(dir, "android", id); err != nil || !known || f != want {
			t.Fatalf("%s: flavor %q known=%v err=%v", id, f, known, err)
		}
	}
}
