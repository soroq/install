package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"soroq/backend/internal/domain"
)

// ---------------------------------------------------------------------------------------------------
// Flag parsing.

func TestFlavorPassthroughParsingAcceptsBothFormsAndStripsThem(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		rest []string
	}{
		{[]string{"--flavor", "prod"}, "prod", []string{}},
		{[]string{"--flavor=prod"}, "prod", []string{}},
		{[]string{"--dart-define=A=b", "--flavor", "dev", "--no-pub"}, "dev", []string{"--dart-define=A=b", "--no-pub"}},
		{[]string{"--flavor=prod", "--flavor", "prod"}, "prod", []string{}}, // same value twice is fine
		{[]string{"--dart-define=FLAVOR=prod"}, "", []string{"--dart-define=FLAVOR=prod"}},
	} {
		got, found, rest, err := extractPassthroughFlavor(tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got != tc.want || found != (tc.want != "") || strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
			t.Errorf("%v => (%q, %v, %v), want (%q, %v)", tc.args, got, found, rest, tc.want, tc.rest)
		}
	}
	for _, bad := range [][]string{
		{"--flavor"},
		{"--flavor="},
		{"--flavor", "--release"},
		{"--flavor", "prod", "--flavor=dev"},
	} {
		if _, _, _, err := extractPassthroughFlavor(bad); err == nil {
			t.Errorf("%v must be rejected", bad)
		}
	}
}

func TestFlavorNameMustBeASafePathSegment(t *testing.T) {
	for _, ok := range []string{"prod", "dev", "freeTier", "free_tier", "P1"} {
		if err := validateFlavorName(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../prod", "a/b", "a\\b", ".", "..", "1prod", "pro d", "prod-1", "_x", strings.Repeat("a", 65)} {
		if err := validateFlavorName(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if _, _, err := resolveCommandFlavor(t.TempDir(), "../../etc", nil); err == nil {
		t.Fatal("an unsafe --flavor must be refused before it reaches any path")
	}
}

func TestResolveCommandFlavorPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: a\nflutter:\n  uses-material-design: true\n  default-flavor: staging # comment\n")

	rf, rest, err := resolveCommandFlavor(dir, "", []string{"--dart-define=X=1"})
	if err != nil || rf.Name != "staging" || rf.Source != "pubspec default-flavor" || len(rest) != 1 {
		t.Fatalf("default-flavor: %+v %v %v", rf, rest, err)
	}
	rf, _, err = resolveCommandFlavor(dir, "prod", nil)
	if err != nil || rf.Name != "prod" || rf.Source != "flag" {
		t.Fatalf("flag must win over default-flavor: %+v %v", rf, err)
	}
	rf, rest, err = resolveCommandFlavor(dir, "", []string{"--flavor", "dev"})
	if err != nil || rf.Name != "dev" || rf.Source != "passthrough" || len(rest) != 0 {
		t.Fatalf("passthrough must win over default-flavor: %+v %v %v", rf, rest, err)
	}
	if _, _, err := resolveCommandFlavor(dir, "prod", []string{"--flavor=dev"}); err == nil {
		t.Fatal("a flag and a passthrough naming different flavors must be refused")
	}
	if rf, _, err := resolveCommandFlavor(dir, "prod", []string{"--flavor=prod"}); err != nil || rf.Name != "prod" {
		t.Fatalf("the same flavor given both ways is fine: %+v %v", rf, err)
	}
	// No flavor anywhere: unflavored, args untouched.
	plain := t.TempDir()
	writeFile(t, filepath.Join(plain, "pubspec.yaml"), testSoroqFlutterPubspec)
	rf, rest, err = resolveCommandFlavor(plain, "", []string{"--dart-define=A=b"})
	if err != nil || rf.Name != "" || strings.Join(rest, " ") != "--dart-define=A=b" {
		t.Fatalf("unflavored: %+v %v %v", rf, rest, err)
	}
	if got := buildArgsWithFlavor(rest, ""); strings.Join(got, " ") != "--dart-define=A=b" {
		t.Fatalf("unflavored build args must be unchanged: %v", got)
	}
	if got := buildArgsWithFlavor(rest, "prod"); strings.Join(got, " ") != "--dart-define=A=b --flavor prod" {
		t.Fatalf("flavored build args: %v", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// Output-path resolution.

func touch(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestAndroidFlavoredDiscoveryNeverReturnsAnotherFlavorOrUnflavoredOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "build", "app", "outputs")
	now := time.Now()
	// Newer unflavored and other-flavor leftovers: must never be picked for "prod".
	touch(t, filepath.Join(out, "bundle", "release", "app-release.aab"), now.Add(time.Hour))
	touch(t, filepath.Join(out, "flutter-apk", "app-release.apk"), now.Add(time.Hour))
	touch(t, filepath.Join(out, "flutter-apk", "app-dev-release.apk"), now.Add(time.Hour))
	touch(t, filepath.Join(out, "bundle", "devRelease", "app-dev-release.aab"), now.Add(time.Hour))
	touch(t, filepath.Join(dir, "release-candidates", "x.aab"), now.Add(time.Hour))
	// The prod outputs, in every location Flutter/Gradle writes them.
	prodBundle := filepath.Join(out, "bundle", "prodRelease", "app-prod-release.aab")
	prodAPK := filepath.Join(out, "apk", "prod", "release", "app-prod-release.apk")
	prodFlutterAPK := filepath.Join(out, "flutter-apk", "app-prod-release.apk")
	prodSplit := filepath.Join(out, "flutter-apk", "app-arm64-v8a-prod-release.apk")
	touch(t, prodBundle, now)
	touch(t, prodAPK, now.Add(-time.Minute))
	touch(t, prodFlutterAPK, now.Add(-2*time.Minute))
	touch(t, prodSplit, now.Add(-3*time.Minute))

	got, err := discoverAndroidArtifactsForFlavor(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, a := range got {
		paths = append(paths, a.Path)
	}
	want := []string{prodBundle, prodAPK, prodFlutterAPK, prodSplit}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("prod discovery =\n%s\nwant\n%s", strings.Join(paths, "\n"), strings.Join(want, "\n"))
	}
	if first, _ := discoverDefaultAndroidArtifactForFlavor(dir, "prod"); first != prodBundle {
		t.Fatalf("newest prod artifact = %s, want %s", first, prodBundle)
	}

	// Unflavored discovery is unchanged: it never returns a flavored output.
	plain, err := discoverAndroidArtifactsForFlavor(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plain {
		if strings.Contains(a.Path, "prod") || strings.Contains(a.Path, "dev") {
			t.Errorf("unflavored discovery returned a flavored output: %s", a.Path)
		}
	}
}

// AGP keeps the flavor's declared casing in directories, Flutter lowercases it in flutter-apk names.
func TestAndroidFlavoredDiscoveryIsCaseInsensitiveLikeFlutter(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "build", "app", "outputs")
	bundle := filepath.Join(out, "bundle", "freeTierRelease", "app-freeTier-release.aab")
	apk := filepath.Join(out, "flutter-apk", "app-freetier-release.apk")
	touch(t, bundle, time.Now())
	touch(t, apk, time.Now().Add(-time.Minute))
	got, err := discoverAndroidArtifactsForFlavor(dir, "freeTier")
	if err != nil || len(got) != 2 || got[0].Path != bundle || got[1].Path != apk {
		t.Fatalf("freeTier discovery = %+v, %v", got, err)
	}
	// "free" is a prefix of "freeTier" and must not match its outputs.
	if got, _ := discoverAndroidArtifactsForFlavor(dir, "free"); len(got) != 0 {
		t.Fatalf("flavor free matched freeTier outputs: %+v", got)
	}
}

func TestFlavorFromAndroidArtifactPath(t *testing.T) {
	for path, want := range map[string]string{
		"/p/build/app/outputs/apk/prod/release/app-prod-release.apk":        "prod",
		"build/app/outputs/bundle/freeTierRelease/app-freeTier-release.aab": "freeTier",
		"/p/build/app/outputs/flutter-apk/app-prod-release.apk":             "prod",
		"/p/build/app/outputs/flutter-apk/app-arm64-v8a-prod-release.apk":   "prod",
		"/p/build/app/outputs/flutter-apk/app-release.apk":                  "",
		"/p/build/app/outputs/flutter-apk/app-arm64-v8a-release.apk":        "",
		"/p/build/app/outputs/apk/release/app-release.apk":                  "",
		"/p/build/app/outputs/bundle/release/app-release.aab":               "",
		"/p/release-candidates/app.aab":                                     "",
	} {
		if got := flavorFromAndroidArtifactPath(path); got != want {
			t.Errorf("flavorFromAndroidArtifactPath(%s) = %q, want %q", path, got, want)
		}
	}
	if err := checkExplicitArtifactFlavor("/p/build/app/outputs/apk/prod/release/app-prod-release.apk", "dev", "--artifact"); err == nil {
		t.Fatal("an explicit prod artifact registered as dev must be refused")
	}
	if err := checkExplicitArtifactFlavor("/p/build/app/outputs/flutter-apk/app-prod-release.apk", "prod", "--artifact"); err != nil {
		t.Fatalf("matching flavor must pass: %v", err)
	}
	if err := checkExplicitArtifactFlavor("/somewhere/app.apk", "prod", "--artifact"); err != nil {
		t.Fatalf("a path that says nothing about flavor must pass: %v", err)
	}
}

// Flutter rsyncs a flavored iOS product from build/ios/<Config>-<flavor>-iphoneos/ to build/ios/iphoneos/
// (flutter_tools ios/mac.dart: outputDir = TARGET_BUILD_DIR.replaceFirst('/$configuration-', '/')), so
// the canonical copy must win over the per-configuration directory.
func TestIOSFlavoredAppResolvesToTheCanonicalIphoneosCopy(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{
		filepath.Join(dir, "build", "ios", "Release-prod-iphoneos", "Runner.app"),
		filepath.Join(dir, "build", "ios", "iphoneos", "Runner.app"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := locateBuiltIOSApp(dir)
	if err != nil || got != filepath.Join(dir, "build", "ios", "iphoneos", "Runner.app") {
		t.Fatalf("locateBuiltIOSApp = %q, %v", got, err)
	}
}

// ---------------------------------------------------------------------------------------------------
// Persisted record round-trip.

func TestReleaseFlavorRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for _, flavor := range []string{"prod", ""} {
		id := "rel-" + flavor + "x"
		if err := writeReleaseFlavorRecord(dir, releaseFlavorRecord{Platform: "android", ReleaseID: id, RuntimeID: "rt", Flavor: flavor}); err != nil {
			t.Fatal(err)
		}
		got, known, err := knownReleaseFlavor(dir, "android", id)
		if err != nil || !known || got != flavor {
			t.Fatalf("round trip %q: got %q known=%v err=%v", flavor, got, known, err)
		}
	}
	if _, known, err := knownReleaseFlavor(dir, "android", "never-recorded"); err != nil || known {
		t.Fatalf("an unrecorded release must be unknown, not unflavored: known=%v err=%v", known, err)
	}
	// A corrupt record is an error, never a silent downgrade to "unknown".
	bad := releaseFlavorRecordPath(dir, "corrupt")
	_ = os.MkdirAll(filepath.Dir(bad), 0o755)
	_ = os.MkdirAll(filepath.Dir(releaseFlavorRecordPath(dir, "other")), 0o755)
	writeFile(t, bad, "{not json")
	if _, _, err := knownReleaseFlavor(dir, "android", "corrupt"); err == nil {
		t.Fatal("a malformed record must be an error")
	}
	writeFile(t, releaseFlavorRecordPath(dir, "other"), `{"schema":"soroq.release_flavor.v1","release_id":"someone-else","flavor":"prod"}`)
	if _, _, err := knownReleaseFlavor(dir, "android", "other"); err == nil {
		t.Fatal("a record naming another release must be an error")
	}
}

func TestReleaseFlavorFallsBackToCLIStateAndSoroqLock(t *testing.T) {
	dir := t.TempDir()
	prod := "prod"
	if err := saveProjectCLIState(dir, projectCLIState{LastAndroidRelease: &androidReleaseState{ReleaseID: "r-state", Flavor: &prod}}); err != nil {
		t.Fatal(err)
	}
	if f, known, _ := knownReleaseFlavor(dir, "android", "r-state"); !known || f != "prod" {
		t.Fatalf("cli-state flavor: %q %v", f, known)
	}
	// A legacy state entry (no flavor field) is unknown.
	if err := saveProjectCLIState(dir, projectCLIState{LastAndroidRelease: &androidReleaseState{ReleaseID: "r-legacy"}}); err != nil {
		t.Fatal(err)
	}
	if _, known, _ := knownReleaseFlavor(dir, "android", "r-legacy"); known {
		t.Fatal("a legacy cli-state entry must be unknown")
	}
	// soroq.lock (committed, survives a fresh clone).
	if err := recordSoroqLockPin(dir, "android", soroqLockPin{ReleaseID: "r-lock", Version: "1.0.0+1", ToolchainVersion: "tc", Flavor: "dev"}); err != nil {
		t.Fatal(err)
	}
	if f, known, _ := knownReleaseFlavor(dir, "android", "r-lock"); !known || f != "dev" {
		t.Fatalf("soroq.lock flavor: %q %v", f, known)
	}
}

func TestSoroqLockFlavorLineIsWrittenOnlyWhenFlavored(t *testing.T) {
	plain := renderSoroqLock(soroqLock{Platforms: map[string]soroqLockPin{"android": {ReleaseID: "r", Version: "1", ToolchainVersion: "t", RecordedAt: time.Unix(0, 0)}}})
	if strings.Contains(plain, "flavor") {
		t.Fatalf("an unflavored soroq.lock must be unchanged:\n%s", plain)
	}
	flav := renderSoroqLock(soroqLock{Platforms: map[string]soroqLockPin{"android": {ReleaseID: "r", Version: "1", ToolchainVersion: "t", Flavor: "prod", RecordedAt: time.Unix(0, 0)}}})
	if got := parseSoroqLock([]byte(flav)).Platforms["android"].Flavor; got != "prod" {
		t.Fatalf("flavor did not round-trip through soroq.lock: %q\n%s", got, flav)
	}
}

// ---------------------------------------------------------------------------------------------------
// Mismatch refusal.

func TestPatchFlavorMustMatchBase(t *testing.T) {
	for _, tc := range []struct {
		name        string
		base        string
		known       bool
		patch       string
		wantRefused bool
	}{
		{"same flavor", "prod", true, "prod", false},
		{"both unflavored", "", true, "", false},
		{"prod patch onto dev base", "dev", true, "prod", true},
		{"flavored patch onto unflavored base", "", true, "prod", true},
		{"unflavored patch onto flavored base", "prod", true, "", true},
		{"flavored patch onto unrecorded base", "", false, "prod", true},
		{"unflavored patch onto unrecorded base (pre-flavor behaviour)", "", false, "", false},
	} {
		err := guardPatchFlavorMatchesBase("rel-1", tc.base, tc.known, tc.patch)
		if (err != nil) != tc.wantRefused {
			t.Errorf("%s: err=%v, wantRefused=%v", tc.name, err, tc.wantRefused)
		}
	}
}

// Command level: a prod patch against a recorded dev base is refused before any build, download or
// control-plane request.
func TestPatchAndroidRefusesFlavorMismatchBeforeAnySideEffect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeSoroqFlutterPubspec(t, dir)
	writeFile(t, filepath.Join(dir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	dev := "dev"
	if err := saveProjectCLIState(dir, projectCLIState{LastAndroidRelease: &androidReleaseState{
		ReleaseID: "rel-dev", AppID: "com.example.app", Channel: "stable", Flavor: &dev,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeReleaseFlavorRecord(dir, releaseFlavorRecord{Platform: "android", ReleaseID: "rel-dev", Flavor: "dev"}); err != nil {
		t.Fatal(err)
	}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	candidate := filepath.Join(dir, "build", "app", "outputs", "flutter-apk", "app-prod-release.apk")
	touch(t, candidate, time.Now())
	for _, args := range [][]string{
		{"--flavor", "prod", "--build=false", "--candidate-artifact", candidate},
		{"--build=false", "--candidate-artifact", filepath.Join(dir, "some.apk")}, // unflavored onto dev
	} {
		err := runPatchAndroid(append([]string{"--project-dir", dir, "--api", server.URL, "--release-id", "rel-dev"}, args...))
		if err == nil || !strings.Contains(err.Error(), "flavor mismatch") {
			t.Fatalf("%v: expected the flavor-mismatch refusal, got %v", args, err)
		}
	}
	if requests != 0 {
		t.Fatalf("the refusal made %d control-plane request(s)", requests)
	}
}

// ---------------------------------------------------------------------------------------------------
// Release, end to end with the build stubbed.

type fakeReleasePlane struct {
	mu         sync.Mutex
	releases   []domain.Release
	registered []domain.CreateReleaseRequest
	server     *httptest.Server
}

func newFakeReleasePlane(t *testing.T, existing ...domain.Release) *fakeReleasePlane {
	f := &fakeReleasePlane{releases: existing}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/releases":
			_ = json.NewEncoder(w).Encode(f.releases)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
			var req domain.CreateReleaseRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.registered = append(f.registered, req)
			rel := domain.Release{ID: req.ID, AppID: req.AppID, RuntimeID: req.RuntimeID, Version: req.Version,
				Platform: req.Platform, Arch: req.Arch, Channel: req.Channel}
			f.releases = append(f.releases, rel)
			_ = json.NewEncoder(w).Encode(rel)
		default:
			_ = json.NewEncoder(w).Encode(domain.ReleaseArtifact{ReleaseID: "r", SHA256: "x", SizeBytes: 1})
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func writeFlavorTestArtifact(t *testing.T, path, runtimeID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeArtifactZip(t, path, map[string][]byte{
		"assets/flutter_assets/soroq/soroq_metadata.json": []byte(testBundledMetadataJSON("com.example.app", "stable", runtimeID, "1.2.3+45")),
		"lib/arm64-v8a/libapp.so":                         []byte("app-" + path),
	})
}

func newFlavorReleaseProject(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeSoroqFlutterPubspec(t, dir)
	writeFile(t, filepath.Join(dir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
	return dir
}

func stubAndroidReleaseBuild(t *testing.T, build func(args []string) error) {
	t.Helper()
	prevBuild, prevGuard := androidReleaseBuildFn, androidReleaseEnvGuardFn
	androidReleaseBuildFn = func(_ string, _ string, _ string, args []string) error { return build(args) }
	androidReleaseEnvGuardFn = func(string, []string) error { return nil }
	t.Cleanup(func() { androidReleaseBuildFn, androidReleaseEnvGuardFn = prevBuild, prevGuard })
}

func TestReleaseAndroidBuildsDiscoversAndRecordsTheFlavor(t *testing.T) {
	for name, extra := range map[string][]string{
		"flag":        {"--flavor", "prod"},
		"passthrough": {"--", "--flavor=prod"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := newFlavorReleaseProject(t)
			plane := newFakeReleasePlane(t)
			// A NEWER unflavored leftover: the old discovery would have registered this.
			leftover := filepath.Join(dir, "build", "app", "outputs", "bundle", "release", "app-release.aab")
			writeFlavorTestArtifact(t, leftover, "runtime-leftover")
			_ = os.Chtimes(leftover, time.Now().Add(time.Hour), time.Now().Add(time.Hour))

			prodAAB := filepath.Join(dir, "build", "app", "outputs", "bundle", "prodRelease", "app-prod-release.aab")
			var buildArgs []string
			stubAndroidReleaseBuild(t, func(args []string) error {
				buildArgs = args
				writeFlavorTestArtifact(t, prodAAB, "runtime-prod")
				return nil
			})
			args := append([]string{"--project-dir", dir, "--api", plane.server.URL, "--arch", "arm64-v8a"}, extra...)
			if err := runReleaseAndroid(args); err != nil {
				t.Fatalf("flavored release: %v", err)
			}
			if strings.Join(buildArgs, " ") != "--flavor prod" {
				t.Fatalf("flutter build args = %q, want exactly --flavor prod", buildArgs)
			}
			if len(plane.registered) != 1 || plane.registered[0].RuntimeID != "runtime-prod" {
				t.Fatalf("registered %+v; must be the prod artifact, never the unflavored leftover", plane.registered)
			}
			id := plane.registered[0].ID
			if !strings.Contains(id, "prod") {
				t.Fatalf("default release id %q must be flavor-qualified", id)
			}
			if f, known, err := knownReleaseFlavor(dir, "android", id); err != nil || !known || f != "prod" {
				t.Fatalf("recorded flavor = %q known=%v err=%v", f, known, err)
			}
			state, _ := loadProjectCLIState(dir)
			if state.LastAndroidRelease == nil || state.LastAndroidRelease.Flavor == nil || *state.LastAndroidRelease.Flavor != "prod" {
				t.Fatalf("cli-state must record the flavor: %+v", state.LastAndroidRelease)
			}
		})
	}
}

// The stale guard still holds on the flavored path: a flavored build that wrote nothing must not
// register an older flavored file that happens to be in the right place.
func TestReleaseAndroidFlavoredStaleArtifactIsRefused(t *testing.T) {
	dir := newFlavorReleaseProject(t)
	plane := newFakeReleasePlane(t)
	old := filepath.Join(dir, "build", "app", "outputs", "flutter-apk", "app-prod-release.apk")
	writeFlavorTestArtifact(t, old, "runtime-old")
	_ = os.Chtimes(old, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	stubAndroidReleaseBuild(t, func([]string) error { return nil })

	err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane.server.URL, "--flavor", "prod"})
	if err == nil || !strings.Contains(err.Error(), "older than the build") {
		t.Fatalf("expected the stale-artifact refusal, got %v", err)
	}
	if len(plane.registered) != 0 {
		t.Fatalf("nothing may be registered: %+v", plane.registered)
	}
}

func TestReleaseAndroidFlavoredBuildWithNoOutputSaysWhereItLooked(t *testing.T) {
	dir := newFlavorReleaseProject(t)
	plane := newFakeReleasePlane(t)
	// Only an unflavored artifact exists: it must NOT be used for a prod release.
	writeFlavorTestArtifact(t, filepath.Join(dir, "build", "app", "outputs", "flutter-apk", "app-release.apk"), "runtime-plain")
	stubAndroidReleaseBuild(t, func([]string) error { return nil })
	err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane.server.URL, "--flavor", "prod"})
	if err == nil || !strings.Contains(err.Error(), `flavor "prod"`) || !strings.Contains(err.Error(), "bundle/prodRelease/") {
		t.Fatalf("expected a flavored not-found error naming the locations, got %v", err)
	}
}

// Two flavors at one version share a runtime_id; the second must be refused before registration.
func TestReleaseAndroidRefusesRuntimeCollisionWithAnotherFlavor(t *testing.T) {
	dir := newFlavorReleaseProject(t)
	plane := newFakeReleasePlane(t, domain.Release{ID: "rel-dev", AppID: "com.example.app", RuntimeID: "runtime-shared", Platform: "android", Arch: "arm64-v8a"})
	if err := writeReleaseFlavorRecord(dir, releaseFlavorRecord{Platform: "android", ReleaseID: "rel-dev", RuntimeID: "runtime-shared", Flavor: "dev"}); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(dir, "build", "app", "outputs", "flutter-apk", "app-prod-release.apk")
	writeFlavorTestArtifact(t, artifact, "runtime-shared")
	err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane.server.URL, "--build=false", "--artifact", artifact, "--flavor", "prod"})
	if err == nil || !strings.Contains(err.Error(), "would share runtime_id") || !strings.Contains(err.Error(), "rel-dev") {
		t.Fatalf("expected the runtime-id collision refusal, got %v", err)
	}
	if len(plane.registered) != 0 {
		t.Fatalf("nothing may be registered: %+v", plane.registered)
	}

	// An UNRECORDED colliding release is not provably the same flavor either.
	plane2 := newFakeReleasePlane(t, domain.Release{ID: "rel-unknown", AppID: "com.example.app", RuntimeID: "runtime-shared", Platform: "android"})
	if err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane2.server.URL, "--build=false", "--artifact", artifact, "--flavor", "prod"}); err == nil {
		t.Fatal("a collision with an unrecorded release must be refused for a flavored release")
	}

	// The SAME flavor at another arch legitimately shares the runtime_id.
	plane3 := newFakeReleasePlane(t, domain.Release{ID: "rel-prod-v7", AppID: "com.example.app", RuntimeID: "runtime-shared", Platform: "android", Arch: "armeabi-v7a"})
	if err := writeReleaseFlavorRecord(dir, releaseFlavorRecord{Platform: "android", ReleaseID: "rel-prod-v7", Flavor: "prod"}); err != nil {
		t.Fatal(err)
	}
	if err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane3.server.URL, "--build=false", "--artifact", artifact, "--flavor", "prod"}); err != nil {
		t.Fatalf("the same flavor at another arch must be allowed: %v", err)
	}
}

// No flavor anywhere: the release goes down exactly the pre-flavor path — unflavored discovery, an
// unqualified id, no collision lookup, no soroq.lock flavor line — and the record says "unflavored".
func TestReleaseAndroidWithoutFlavorIsUnchanged(t *testing.T) {
	dir := newFlavorReleaseProject(t)
	var gets int
	plane := newFakeReleasePlane(t)
	inner := plane.server.Config.Handler
	plane.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/releases" {
			gets++
		}
		inner.ServeHTTP(w, r)
	})
	// A flavored output must not be picked by an unflavored release.
	writeFlavorTestArtifact(t, filepath.Join(dir, "build", "app", "outputs", "flutter-apk", "app-prod-release.apk"), "runtime-prod")
	plainAAB := filepath.Join(dir, "build", "app", "outputs", "bundle", "release", "app-release.aab")
	var buildArgs []string
	stubAndroidReleaseBuild(t, func(args []string) error {
		buildArgs = args
		writeFlavorTestArtifact(t, plainAAB, "runtime-plain")
		return nil
	})
	if err := runReleaseAndroid([]string{"--project-dir", dir, "--api", plane.server.URL, "--arch", "arm64-v8a", "--", "--dart-define=A=b"}); err != nil {
		t.Fatalf("unflavored release: %v", err)
	}
	if strings.Join(buildArgs, " ") != "--dart-define=A=b" {
		t.Fatalf("unflavored build args changed: %q", buildArgs)
	}
	if len(plane.registered) != 1 || plane.registered[0].RuntimeID != "runtime-plain" {
		t.Fatalf("registered %+v", plane.registered)
	}
	if want := defaultReleaseID("com.example.app", "1.2.3+45", "arm64-v8a"); plane.registered[0].ID != want {
		t.Fatalf("unflavored default id = %q, want %q", plane.registered[0].ID, want)
	}
	if gets != 1 { // the pre-existing duplicate-release preflight only; no collision lookup
		t.Fatalf("unflavored release made %d release-list requests, want 1 (preflight only)", gets)
	}
	if f, known, _ := knownReleaseFlavor(dir, "android", plane.registered[0].ID); !known || f != "" {
		t.Fatalf("an unflavored release must be recorded as known-unflavored: %q %v", f, known)
	}
	if b, _ := os.ReadFile(soroqLockPath(dir)); strings.Contains(string(b), "flavor") {
		t.Fatalf("an unflavored soroq.lock must not gain a flavor line:\n%s", b)
	}
}

// ---------------------------------------------------------------------------------------------------
// Routes that stay refused.

func TestUnsupportedFlavorRoutesRefuseEvenWithTheOptIn(t *testing.T) {
	t.Setenv(unverifiedBuildFlagsOptInEnv, "1")
	err := guardUnsupportedFlavorRoute("the iOS engine release lane", iosEngineFlavorRefusalReason, "prod")
	if err == nil {
		t.Fatal("the opt-in must not unlock a flavor-unaware baseline")
	}
	for _, want := range []string{"FLUTTER_APP_FLAVOR", "soroq release android", unverifiedBuildFlagsOptInEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q:\n%s", want, err)
		}
	}
	if err := guardUnsupportedFlavorRoute("x", "y", ""); err != nil {
		t.Fatalf("no flavor, no refusal: %v", err)
	}
}

// A pubspec default-flavor makes every `flutter build` flavored, so the iOS engine lane must see it
// even when no --flavor was typed.
func TestIOSEngineLaneSeesThePubspecDefaultFlavor(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: a\nflutter:\n  default-flavor: prod\n")
	if f, err := flavorFromRouteArgs(dir, nil, nil); err != nil || f != "prod" {
		t.Fatalf("default-flavor = %q, %v", f, err)
	}
	if f, err := flavorFromRouteArgs(t.TempDir(), []string{"--flavor", "dev"}, nil); err != nil || f != "dev" {
		t.Fatalf("head --flavor = %q, %v", f, err)
	}
}

func TestPlatformsWithIOSRefuseAFlavorBeforeAnyLane(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), testSoroqFlutterPubspec)
	rest := []string{"--project-dir", dir, "--flavor", "prod"}
	if err := refuseFlavorOnUnsupportedPlatforms("release", []string{"android", "ios"}, rest); err == nil ||
		!strings.Contains(err.Error(), "Nothing has been built") {
		t.Fatalf("android,ios with a flavor must be refused up front, got %v", err)
	}
	if err := refuseFlavorOnUnsupportedPlatforms("release", []string{"android"}, rest); err != nil {
		t.Fatalf("android alone supports a flavor: %v", err)
	}
	if err := refuseFlavorOnUnsupportedPlatforms("release", []string{"android", "ios"}, []string{"--project-dir", dir}); err != nil {
		t.Fatalf("no flavor, no refusal: %v", err)
	}
}

// Patch, fresh-clone path (no recorded release): the candidate is built with --flavor, discovered only
// in that flavor's locations, and a leftover flavored file older than the build is refused.
func TestPatchAndroidFlavoredCandidateBuildRefusesStaleOutput(t *testing.T) {
	dir := newFlavorReleaseProject(t)
	old := filepath.Join(dir, "build", "app", "outputs", "bundle", "prodRelease", "app-prod-release.aab")
	writeFlavorTestArtifact(t, old, "runtime-old")
	_ = os.Chtimes(old, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	var buildArgs []string
	prev := androidPatchBuildFn
	androidPatchBuildFn = func(_ string, _ string, _ string, args []string) error { buildArgs = args; return nil }
	t.Cleanup(func() { androidPatchBuildFn = prev })

	err := runPatchAndroid([]string{"--project-dir", dir, "--api", "http://127.0.0.1:1", "--flavor", "prod"})
	if err == nil || !strings.Contains(err.Error(), "older than the build") {
		t.Fatalf("expected the stale-candidate refusal on the flavored path, got %v", err)
	}
	if strings.Join(buildArgs, " ") != "--flavor prod" {
		t.Fatalf("candidate build args = %q, want --flavor prod", buildArgs)
	}
}

func TestPlatformsDerivedReleaseIDIsFlavorQualifiedOnlyWhenFlavored(t *testing.T) {
	dir := newFlavorReleaseProject(t)
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), "name: a\nversion: 1.2.3+4\n")
	base := []string{"--toolchain", "tc", "--api", "http://x"}
	plain, err := withDerivedFlags("android", dir, base)
	if err != nil {
		t.Fatal(err)
	}
	flav, err := withDerivedFlags("android", dir, append([]string{"--flavor", "Prod"}, base...))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := flagValue(plain, "release-id")
	f, _ := flagValue(flav, "release-id")
	if p == "" || strings.Contains(p, "prod") || f != p+"-prod" {
		t.Fatalf("derived ids: unflavored %q, flavored %q", p, f)
	}
}
