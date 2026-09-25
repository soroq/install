package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const seedTestToolchain = "soroq-android-3.44.9-test-obfseed"

// installSeedTestToolchain lays out ~/.soroq/toolchains/<v>/android under a temp HOME.
func installSeedTestToolchain(t *testing.T, capabilities []string, helpText string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".soroq", "toolchains", seedTestToolchain, "android")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	engine, _ := json.Marshal(map[string]any{
		"schema":                     "soroq.android_engine.v1",
		"soroq_android_capabilities": capabilities,
	})
	if err := os.WriteFile(filepath.Join(dir, "engine.json"), engine, 0o644); err != nil {
		t.Fatal(err)
	}
	prev := androidGenSnapshotHelpFn
	androidGenSnapshotHelpFn = func(string) (string, error) { return helpText, nil }
	t.Cleanup(func() { androidGenSnapshotHelpFn = prev })
}

var seedObfArgs = []string{"--obfuscate", "--split-debug-info=build/symbols", "--target-platform", "android-arm64"}

func writeMap(t *testing.T, path string, flat ...string) {
	t.Helper()
	raw, _ := json.Marshal(flat)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAndroidObfuscationArgsValidation(t *testing.T) {
	cases := []struct {
		args []string
		want string // "" = accepted
	}{
		{seedObfArgs, ""},
		{[]string{"--obfuscate", "--split-debug-info=x", "--target-platform=android-arm64"}, ""},
		{[]string{"--obfuscate", "--target-platform=android-arm64"}, "both --obfuscate and --split-debug-info"},
		{[]string{"--split-debug-info=x", "--target-platform=android-arm64"}, "both --obfuscate and --split-debug-info"},
		{[]string{"--obfuscate", "--split-debug-info=x"}, "must target exactly android-arm64"},
		{[]string{"--obfuscate", "--split-debug-info=x", "--target-platform=android-arm64,android-arm"}, "must target exactly android-arm64"},
		{append(append([]string{}, seedObfArgs...), "--extra-gen-snapshot-options=--foo"), "--extra-gen-snapshot-options cannot be combined"},
	}
	for _, c := range cases {
		err := validateAndroidObfuscationArgs(c.args)
		if c.want == "" && err != nil {
			t.Errorf("%v: unexpected refusal: %v", c.args, err)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%v: want refusal %q, got %v", c.args, c.want, err)
		}
	}
}

func TestAndroidToolchainSeedSupportNeedsDeclarationAndBinary(t *testing.T) {
	const good = "... [--load-obfuscation-map=<map-filename>] ..."
	installSeedTestToolchain(t, []string{androidObfuscationSeedCapability}, good)
	if err := androidToolchainSupportsObfuscationSeed(seedTestToolchain); err != nil {
		t.Fatalf("declared + supporting binary refused: %v", err)
	}
	installSeedTestToolchain(t, nil, good)
	if err := androidToolchainSupportsObfuscationSeed(seedTestToolchain); err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("undeclared capability accepted: %v", err)
	}
	installSeedTestToolchain(t, []string{androidObfuscationSeedCapability}, "stock usage text")
	if err := androidToolchainSupportsObfuscationSeed(seedTestToolchain); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("declaration over a stock gen_snapshot accepted: %v", err)
	}
	if err := androidToolchainSupportsObfuscationSeed(""); err == nil {
		t.Fatal("obfuscated build with no toolchain accepted")
	}
}

func TestAndroidReleaseObfuscationCapturesAndCommitsMap(t *testing.T) {
	installSeedTestToolchain(t, []string{androidObfuscationSeedCapability}, "--load-obfuscation-map")
	project := t.TempDir()

	plain, err := planAndroidReleaseObfuscation(project, seedTestToolchain, []string{"--release"})
	if err != nil || plain.Obfuscated || len(plain.ExtraArgs) != 0 {
		t.Fatalf("plain release should be untouched: %+v %v", plain, err)
	}
	if rec, err := commitAndroidReleaseObfuscation(project, "r1", seedTestToolchain, plain); rec != nil || err != nil {
		t.Fatalf("plain release committed an obfuscation record: %+v %v", rec, err)
	}

	plan, err := planAndroidReleaseObfuscation(project, seedTestToolchain, seedObfArgs)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.cleanup()
	if len(plan.ExtraArgs) != 1 || plan.ExtraArgs[0] != "--extra-gen-snapshot-options=--save-obfuscation-map="+plan.SavedMapPath {
		t.Fatalf("unexpected extra args %v", plan.ExtraArgs)
	}
	if strings.HasPrefix(plan.SavedMapPath, filepath.Join(project, "build")) {
		t.Fatalf("map would be written inside the Flutter build tree: %s", plan.SavedMapPath)
	}
	// A build that wrote no map must not register.
	if _, err := commitAndroidReleaseObfuscation(project, "r1", seedTestToolchain, plan); err == nil {
		t.Fatal("release committed without a captured map")
	}
	writeMap(t, plan.SavedMapPath, "", "", "Foo", "a", "bar", "b")
	rec, err := commitAndroidReleaseObfuscation(project, "r1", seedTestToolchain, plan)
	if err != nil {
		t.Fatal(err)
	}
	if rec.MapEntries != 3 || len(rec.MapSHA256) != 64 || rec.ToolchainVersion != seedTestToolchain {
		t.Fatalf("bad record %+v", rec)
	}
	for _, f := range []string{androidObfuscationMapFile, androidObfuscationRecordFile} {
		info, err := os.Stat(filepath.Join(project, ".soroq", "releases", "r1", f))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v, want 0600", f, info.Mode().Perm())
		}
	}
	got, mapPath, err := loadAndroidReleaseObfuscation(project, "r1")
	if err != nil || got == nil || got.MapSHA256 != rec.MapSHA256 || !strings.HasSuffix(mapPath, androidObfuscationMapFile) {
		t.Fatalf("record did not round-trip: %+v %s %v", got, mapPath, err)
	}
}

func TestAndroidPatchObfuscationFollowsTheBase(t *testing.T) {
	installSeedTestToolchain(t, []string{androidObfuscationSeedCapability}, "--load-obfuscation-map")
	project := t.TempDir()

	// Plain base: a plain patch is untouched, an obfuscated one refused.
	if p, err := planAndroidPatchObfuscation(project, "plain", seedTestToolchain, []string{"--release"}); err != nil || p.Obfuscated {
		t.Fatalf("plain/plain: %+v %v", p, err)
	}
	if _, err := planAndroidPatchObfuscation(project, "plain", seedTestToolchain, seedObfArgs); err == nil || !strings.Contains(err.Error(), "was not built with --obfuscate") {
		t.Fatalf("obfuscated patch over a plain base accepted: %v", err)
	}

	// Obfuscated base.
	rel, _ := planAndroidReleaseObfuscation(project, seedTestToolchain, seedObfArgs)
	writeMap(t, rel.SavedMapPath, "Foo", "a", "bar", "b")
	if _, err := commitAndroidReleaseObfuscation(project, "obf", seedTestToolchain, rel); err != nil {
		t.Fatal(err)
	}
	rel.cleanup()
	if _, err := planAndroidPatchObfuscation(project, "obf", seedTestToolchain, []string{"--release"}); err == nil || !strings.Contains(err.Error(), "the patch must be too") {
		t.Fatalf("plain patch over an obfuscated base accepted: %v", err)
	}
	plan, err := planAndroidPatchObfuscation(project, "obf", seedTestToolchain, seedObfArgs)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.cleanup()
	want := "--extra-gen-snapshot-options=--load-obfuscation-map=" + plan.BaseMapPath + ",--save-obfuscation-map=" + plan.SavedMapPath
	if len(plan.ExtraArgs) != 1 || plan.ExtraArgs[0] != want {
		t.Fatalf("extra args %v, want %s", plan.ExtraArgs, want)
	}

	// A base map that no longer matches its digest is refused, never treated as plain.
	if err := os.WriteFile(plan.BaseMapPath, []byte(`["Foo","z","bar","b"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := planAndroidPatchObfuscation(project, "obf", seedTestToolchain, seedObfArgs); err == nil || !strings.Contains(err.Error(), "does not match its recorded digest") {
		t.Fatalf("tampered base map accepted: %v", err)
	}
	// A map without a record is refused too.
	if err := os.Remove(filepath.Join(project, ".soroq", "releases", "obf", androidObfuscationRecordFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := planAndroidPatchObfuscation(project, "obf", seedTestToolchain, []string{"--release"}); err == nil || !strings.Contains(err.Error(), "no record") {
		t.Fatalf("map without record treated as plain: %v", err)
	}
}

func TestAndroidSeededCandidateVerification(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	cand := filepath.Join(dir, "cand.json")
	plan := &androidObfuscationPlan{Obfuscated: true, BaseMapPath: base, SavedMapPath: cand}
	writeMap(t, base, "", "", "main", "main", "Foo", "a", "_bar@1", "_b@1")

	writeMap(t, cand, "", "", "main", "main", "Foo", "a", "_bar@1", "_b@1", "New", "c", "_fresh@1", "_d@1")
	v, err := verifyAndroidSeededCandidate(plan)
	if err != nil || v.BaseEntries != 4 || v.NewEntries != 2 {
		t.Fatalf("seeded superset refused: %+v %v", v, err)
	}
	for name, flat := range map[string][]string{
		"dropped":   {"", "", "main", "main", "_bar@1", "_b@1"},
		"renamed":   {"", "", "main", "main", "Foo", "z", "_bar@1", "_b@1"},
		"collision": {"", "", "main", "main", "Foo", "a", "_bar@1", "_b@1", "New", "a"},
	} {
		writeMap(t, cand, flat...)
		if _, err := verifyAndroidSeededCandidate(plan); err == nil {
			t.Errorf("%s candidate accepted", name)
		}
	}
	if err := os.Remove(cand); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyAndroidSeededCandidate(plan); err == nil {
		t.Error("candidate with no saved map accepted")
	}
	if v, err := verifyAndroidSeededCandidate(&androidObfuscationPlan{}); v != nil || err != nil {
		t.Errorf("plain plan should skip verification: %+v %v", v, err)
	}
}

// TestAndroidSeededCandidateVerificationOnRealMaps runs the publication gate over maps produced by the
// real Option A gen_snapshot (tooling/obfuscation-seed/seed_proof.sh), and over the stock-R5 pair it
// must refuse.
func TestAndroidSeededCandidateVerificationOnRealMaps(t *testing.T) {
	dir := os.Getenv("SOROQ_SEED_PROOF_DIR")
	if dir == "" {
		t.Skip("SOROQ_SEED_PROOF_DIR not set")
	}
	seeded := &androidObfuscationPlan{Obfuscated: true,
		BaseMapPath:  filepath.Join(dir, "new-base.map.json"),
		SavedMapPath: filepath.Join(dir, "seeded-candidate.map.json")}
	v, err := verifyAndroidSeededCandidate(seeded)
	if err != nil {
		t.Fatalf("real seeded candidate refused: %v", err)
	}
	t.Logf("real seeded candidate: %+v", *v)
	stock := &androidObfuscationPlan{Obfuscated: true,
		BaseMapPath:  filepath.Join(dir, "r5-base.map.json"),
		SavedMapPath: filepath.Join(dir, "r5-candidate.map.json")}
	if _, err := verifyAndroidSeededCandidate(stock); err == nil {
		t.Fatal("a stock-R5 (unseeded) candidate passed the publication gate")
	} else {
		t.Logf("stock R5 candidate refused as expected: %v", err)
	}
}
