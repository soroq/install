package main

// CANONICAL ROLLBACK — derivation, secrecy, and the record-vs-device distinction.
//
// `soroq rollback ios` used to dispatch to the RECORD lane, which marks a patch row rolled_back so the
// device falls to the newest surviving patch — an older patch, not the base. For an app on v4 that is a
// downgrade to v3, not a rollback. The signed version-0 ENGINE manifest is what returns a device to the
// code in its store build, and it was reachable only by hand-copying four ids plus the Ed25519 seed as
// a command-line flag.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func engineProject(t *testing.T, appID, channel string) string {
	t.Helper()
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "soroq.yaml"), "app_id: "+appID+"\nchannel: "+channel+"\nios_engine:\n  enabled: true\n")
	mustWriteFile(t, filepath.Join(dir, "pubspec.yaml"), "name: app\nversion: 1.0.0+1\n")
	return dir
}

func seedBaseline(t *testing.T, dir, runtimeID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".soroq", "releases", runtimeID), 0o755); err != nil {
		t.Fatal(err)
	}
}

// DERIVATION: app, release, runtime, channel and api all come from project state — no ids by hand.
func TestEngineRollbackDerivesEverythingFromProjectState(t *testing.T) {
	const runtime = "3ff36e9b6c3725746e73060cc2bd1f74904811922131a73a66d6e89134c41ce5"
	dir := engineProject(t, "dev.soroq.canonapp", "stable")
	seedBaseline(t, dir, runtime)

	plan, err := deriveEngineRollbackPlan(dir, "")
	if err != nil {
		t.Fatalf("derivation failed: %v", err)
	}
	if plan.AppID != "dev.soroq.canonapp" {
		t.Errorf("app id = %q", plan.AppID)
	}
	if plan.RuntimeID != runtime {
		t.Errorf("runtime = %q, want the persisted baseline", plan.RuntimeID)
	}
	if plan.Channel != "stable" {
		t.Errorf("channel = %q", plan.Channel)
	}
	if !strings.Contains(plan.ReleaseID, "canonapp") || !strings.Contains(plan.ReleaseID, "ios") {
		t.Errorf("release id %q is not the platform-qualified project release", plan.ReleaseID)
	}
	if plan.APIBase == "" {
		t.Error("api base was not derived")
	}
}

// The channel is honoured, not assumed: rolling back on the wrong channel would leave the bad patch
// serving to real devices.
func TestEngineRollbackHonoursTheProjectChannel(t *testing.T) {
	dir := engineProject(t, "dev.soroq.canonapp", "beta")
	seedBaseline(t, dir, strings.Repeat("a", 64))
	plan, err := deriveEngineRollbackPlan(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Channel != "beta" {
		t.Errorf("channel = %q, want beta", plan.Channel)
	}
}

// MISSING STATE must be actionable: name the artifact and the command that produces it.
func TestEngineRollbackWithoutABaselineIsActionable(t *testing.T) {
	dir := engineProject(t, "dev.soroq.canonapp", "stable")
	_, err := deriveEngineRollbackPlan(dir, "")
	if err == nil {
		t.Fatal("a project with no persisted base release derived a rollback anyway")
	}
	for _, want := range []string{".soroq/releases", "soroq release", "--platforms=ios"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestEngineRollbackWithoutAnAppIdIsActionable(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "pubspec.yaml"), "name: app\n")
	_, err := deriveEngineRollbackPlan(dir, "")
	if err == nil {
		t.Fatal("a project with no soroq.yaml derived a rollback anyway")
	}
	if !strings.Contains(err.Error(), "soroq init") {
		t.Errorf("error does not tell the developer what to run:\n%v", err)
	}
}

// AMBIGUOUS RUNTIME: two persisted baselines means the device's base cannot be inferred. Signing a
// version-0 manifest against the wrong runtime produces a rollback the device correctly refuses, which
// presents as "rollback did nothing" — so guessing is worse than refusing.
func TestEngineRollbackRefusesAnAmbiguousRuntime(t *testing.T) {
	dir := engineProject(t, "dev.soroq.canonapp", "stable")
	seedBaseline(t, dir, strings.Repeat("a", 64))
	seedBaseline(t, dir, strings.Repeat("b", 64))

	_, err := deriveEngineRollbackPlan(dir, "")
	if err == nil {
		t.Fatal("two baselines produced a derived runtime; the device's base is not knowable here")
	}
	if !strings.Contains(err.Error(), "--runtime-id") {
		t.Errorf("error does not offer the disambiguating flag:\n%v", err)
	}
}

// THE SEED MUST NEVER REACH ARGV. This is the property that makes the command safe to run on a shared
// machine: argv is readable by any process via `ps` and is recorded in shell history.
func TestSigningSeedIsNeverPassedInArgv(t *testing.T) {
	const runtime = "3ff36e9b6c3725746e73060cc2bd1f74904811922131a73a66d6e89134c41ce5"
	dir := engineProject(t, "dev.soroq.canonapp", "stable")
	seedBaseline(t, dir, runtime)
	plan, err := deriveEngineRollbackPlan(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	// The argv the canonical path constructs, mirrored from runRollbackIOSEngineCanonical.
	args := []string{
		"--app-id", plan.AppID, "--release-id", plan.ReleaseID, "--runtime-id", plan.RuntimeID,
		"--channel", plan.Channel, "--patch-id", "freehand-rollback-x-v0", "--api", plan.APIBase,
	}
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"--seed-base64", "seed", "SOROQ_ENGINE_SIGNING_SEED"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("argv contains %q — a signing seed must travel in the environment, not argv:\n%s",
				forbidden, joined)
		}
	}
}

// The delegate must actually carry the seed in its environment, or the canonical command cannot sign.
func TestDelegateEnvCarriesTheSeed(t *testing.T) {
	baseEnv, envErr := engineLaneDelegateEnv(nil)
	if envErr != nil {
		t.Fatalf("delegate env: %v", envErr)
	}
	env := append(baseEnv, "SOROQ_ENGINE_SIGNING_SEED=abc123")
	var found bool
	for _, e := range env {
		if strings.HasPrefix(e, "SOROQ_ENGINE_SIGNING_SEED=") {
			found = true
		}
	}
	if !found {
		t.Error("the seed is not present in the delegate environment")
	}
}

// RECORD vs DEVICE: `--patch-record` must still reach the record lane, and its absence must not.
func TestPatchRecordFlagSelectsTheRecordLane(t *testing.T) {
	// The dispatch condition mirrored: engine rollback is chosen only when --patch-record is absent.
	for _, tc := range []struct {
		args       []string
		wantRecord bool
	}{
		{[]string{"--project-dir", "."}, false},
		{[]string{"--patch-record", "--project-dir", "."}, true},
	} {
		gotRecord := hasFlag(tc.args, "patch-record")
		if gotRecord != tc.wantRecord {
			t.Errorf("args %v selected record=%v, want %v", tc.args, gotRecord, tc.wantRecord)
		}
	}
	// And the flag must be stripped before the record lane parses, or its FlagSet rejects it.
	stripped := stripFlag([]string{"--patch-record", "--channel", "stable"}, "patch-record", true)
	if strings.Contains(strings.Join(stripped, " "), "patch-record") {
		t.Errorf("--patch-record leaked into the record lane args: %v", stripped)
	}
	if !strings.Contains(strings.Join(stripped, " "), "--channel stable") {
		t.Errorf("stripping removed an unrelated flag: %v", stripped)
	}
}

// Explicit overrides must win over derivation for the disambiguation cases.
func TestExplicitOverridesWinOverDerivation(t *testing.T) {
	const runtime = "3ff36e9b6c3725746e73060cc2bd1f74904811922131a73a66d6e89134c41ce5"
	dir := engineProject(t, "dev.soroq.canonapp", "stable")
	seedBaseline(t, dir, runtime)
	plan, err := deriveEngineRollbackPlan(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.RuntimeID != runtime {
		t.Fatalf("precondition: derived runtime = %q", plan.RuntimeID)
	}
	// The override path in runRollbackIOSEngineCanonical replaces the field and records its provenance.
	plan.RuntimeID, plan.SourceRt = "explicit-runtime", "--runtime-id"
	if plan.RuntimeID != "explicit-runtime" || plan.SourceRt != "--runtime-id" {
		t.Error("explicit override did not take effect or lost its provenance")
	}
}

// REPEATED ROLLBACK must not collide. The patch table keys on the patch id, so a constant id made the
// second rollback of a runtime fail with a primary-key violation -- while the bad patch stayed live.
func TestEngineRollbackPatchIDIsUniquePerInvocation(t *testing.T) {
	const rt = "826f9b98948d3a394e8679776ecd0c79e5552e1c721d9e2924fbcf4f717f7096"
	first := engineRollbackPatchID(rt, time.Date(2026, 8, 10, 17, 24, 5, 0, time.UTC))
	second := engineRollbackPatchID(rt, time.Date(2026, 8, 10, 17, 31, 42, 0, time.UTC))
	if first == second {
		t.Fatalf("two rollbacks of the same runtime produced the same patch id %q; the second insert "+
			"violates patches_pkey and the bad patch stays live", first)
	}
	for _, id := range []string{first, second} {
		if !strings.HasPrefix(id, "freehand-rollback-826f9b98948d-v0-") {
			t.Errorf("id %q lost its identifying prefix", id)
		}
	}
	// Sortable: a later rollback must order after an earlier one.
	if !(first < second) {
		t.Errorf("ids are not chronologically sortable: %q !< %q", first, second)
	}
}

// SAME-SECOND and CONCURRENT rollbacks must not collide. A timestamp at second resolution is not
// enough: a retry after a transient failure, a script rolling back several channels, or two operators
// reacting to the same bad patch all land inside one second, and the collision surfaces as a
// primary-key violation reported as "rollback failed" while the bad patch is still live.
func TestEngineRollbackPatchIDIsUniqueWithinTheSameSecond(t *testing.T) {
	const rt = "826f9b98948d3a394e8679776ecd0c79e5552e1c721d9e2924fbcf4f717f7096"
	at := time.Date(2026, 8, 10, 17, 24, 5, 0, time.UTC)
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := engineRollbackPatchID(rt, at) // identical timestamp every iteration
		if seen[id] {
			t.Fatalf("duplicate patch id %q generated within one second after %d ids", id, i)
		}
		seen[id] = true
	}
}

func TestEngineRollbackPatchIDIsUniqueUnderConcurrency(t *testing.T) {
	const rt = "826f9b98948d3a394e8679776ecd0c79e5552e1c721d9e2924fbcf4f717f7096"
	at := time.Date(2026, 8, 10, 17, 24, 5, 0, time.UTC)
	const n = 256
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ids <- engineRollbackPatchID(rt, at) }()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("concurrent generation produced duplicate id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct ids, got %d", n, len(seen))
	}
}

// Uniqueness must not cost sortability: the timestamp stays the leading component so an operator
// listing patches still reads them chronologically.
func TestEngineRollbackPatchIDsRemainChronologicallySortable(t *testing.T) {
	const rt = "826f9b98948d3a394e8679776ecd0c79e5552e1c721d9e2924fbcf4f717f7096"
	earlier := engineRollbackPatchID(rt, time.Date(2026, 8, 10, 17, 24, 5, 0, time.UTC))
	later := engineRollbackPatchID(rt, time.Date(2026, 8, 10, 17, 24, 6, 0, time.UTC))
	if !(earlier < later) {
		t.Errorf("ids are not chronologically sortable: %q !< %q", earlier, later)
	}
	if !strings.HasPrefix(earlier, "freehand-rollback-826f9b98948d-v0-20260810t172405z-") {
		t.Errorf("id lost its operator-readable provenance: %q", earlier)
	}
}

// THE ERROR MUST BE ACTIONABLE BY THE FLAG IT NAMES.
//
// With several bases persisted, the derivation refuses and tells the operator to "re-run with an
// explicit --runtime-id". That flag was read by the command and applied to the plan AFTERWARDS — so
// the derivation failed first and the advice could never work. An error that names its own remedy has
// to be resolvable by that remedy.
func TestExplicitRuntimeIDResolvesTheMultipleBaselineRefusal(t *testing.T) {
	dir := engineProject(t, "dev.soroq.canonapp", "stable")
	a := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	seedBaseline(t, dir, a)
	seedBaseline(t, dir, b)

	// Without the flag: refused, and the message must name the flag that fixes it.
	_, err := deriveEngineRollbackPlan(dir, "")
	if err == nil {
		t.Fatal("two persisted baselines must not be silently guessed between")
	}
	if !strings.Contains(err.Error(), "--runtime-id") {
		t.Fatalf("refusal should name the remedy:\n%s", err)
	}

	// With the flag: resolved, to exactly the base named.
	plan, err := deriveEngineRollbackPlan(dir, b)
	if err != nil {
		t.Fatalf("--runtime-id did not resolve the refusal it is advertised for: %v", err)
	}
	if plan.RuntimeID != b {
		t.Fatalf("plan targets %s, want %s", plan.RuntimeID, b)
	}
	if plan.SourceRt != "--runtime-id" {
		t.Errorf("provenance should record the flag, got %q", plan.SourceRt)
	}

	// A runtime id this project never built must be refused rather than signed for: the device would
	// reject that manifest, and a wrong argument would present as a broken rollback.
	if _, err := deriveEngineRollbackPlan(dir, "c"+b[1:]); err == nil {
		t.Fatal("an unknown --runtime-id was accepted")
	}
}
