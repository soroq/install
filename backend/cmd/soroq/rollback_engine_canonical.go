package main

// CANONICAL ENGINE ROLLBACK — `soroq rollback ios` with no secrets and no ids to copy.
//
// Two different things were both called "rollback", and the convenient command did the wrong one:
//
//   - RECORD rollback marks a patch row rolled_back in the control plane. The device then sees the
//     newest surviving patch — which is an OLDER PATCH, not the base. For an app already running v4
//     that is not a rollback at all; it is a downgrade to v3.
//   - ENGINE rollback publishes a SIGNED version-0 manifest. Version 0 means "no patch": the device
//     transactionally clears every redirect and returns to the code inside the store build. That is
//     what a developer means by "roll back".
//
// `soroq rollback ios` dispatched to the record lane, so the safety valve for a bad patch silently did
// the wrong thing. The signed version-0 path existed only as `soroqctl rollback ios-engine`, which
// required --app-id, --release-id, --runtime-id, --patch-id AND --seed-base64 — every one of them
// copied by hand, with the Ed25519 signing seed passed IN ARGV, where any process on the machine can
// read it out of `ps`.
//
// This resolves all of that from project state and signs IN-PROCESS with the seed the project already
// owns, so the canonical command carries no ids and no secrets.

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// engineRollbackPlan is everything the signed version-0 rollback needs, with the provenance of each
// value so a derivation failure can say WHICH input was missing rather than "not found".
type engineRollbackPlan struct {
	AppID      string
	ReleaseID  string
	RuntimeID  string
	Channel    string
	APIBase    string
	PatchID    string
	SourceApp  string
	SourceRel  string
	SourceRt   string
	ProjectDir string
}

// deriveEngineRollbackPlan reconstructs the rollback target from the project, in the same order the
// release and patch lanes resolve their own identity — so a rollback always targets exactly what the
// last release registered.
//
// Every field reports where it came from: an actionable failure has to name the missing artifact, not
// just the missing value.
// explicitRuntimeID, when non-empty, SELECTS among the persisted baselines instead of deriving one.
// It is passed in rather than applied by the caller afterwards because the multiple-baseline case used
// to fail here, before any override could be considered -- so `--runtime-id`, the flag the error
// message tells you to use, could not actually resolve the error it was named in.
func deriveEngineRollbackPlan(projectDir, explicitRuntimeID string) (engineRollbackPlan, error) {
	p := engineRollbackPlan{ProjectDir: projectDir}

	p.AppID = freehandProjectAppID(projectDir)
	p.SourceApp = filepath.Join(projectDir, "soroq.yaml")
	if strings.TrimSpace(p.AppID) == "" {
		return p, fmt.Errorf(
			"cannot determine the app id: %s is missing or has no app_id.\n"+
				"  Run `soroq init` in this project, or pass --project-dir at the project root.", p.SourceApp)
	}

	// The runtime id is the identity of the BASE build the device is running. It comes from the
	// immutable baseline the release lane persisted, never from a guess: a version-0 manifest signed
	// against the wrong runtime is refused by the device, which looks like a broken rollback.
	baselineDir := filepath.Join(projectDir, ".soroq", "releases")
	entries, _ := os.ReadDir(baselineDir)
	var baselines []string
	for _, e := range entries {
		if e.IsDir() && contentAddrRe.MatchString(e.Name()) {
			baselines = append(baselines, e.Name())
		}
	}
	if want := strings.TrimSpace(explicitRuntimeID); want != "" {
		// Validated against what is actually persisted: a typo'd runtime id would otherwise sign a
		// version-0 manifest for a base that does not exist here, which the device refuses and which
		// then reads as a broken rollback rather than as a wrong argument.
		found := false
		for _, b := range baselines {
			if b == want {
				found = true
				break
			}
		}
		if !found {
			return p, fmt.Errorf(
				"--runtime-id %s is not among the base releases persisted under %s (%s).\n"+
					"  A signed version-0 rollback must be bound to a base this project actually built.",
				want, baselineDir, strings.Join(baselines, ", "))
		}
		p.RuntimeID = want
		p.SourceRt = "--runtime-id"
		baselines = []string{want}
	}

	switch len(baselines) {
	case 0:
		return p, fmt.Errorf(
			"no persisted base release found under %s.\n"+
				"  A signed version-0 rollback must be bound to the exact base runtime the device is\n"+
				"  running, and that identity is only recorded by a successful `soroq release\n"+
				"  --platforms=ios`. Run a release for this project first.", baselineDir)
	case 1:
		if p.RuntimeID == "" {
			p.RuntimeID = baselines[0]
		}
	default:
		return p, fmt.Errorf(
			"%d base releases are persisted under %s (%s).\n"+
				"  Which base the device is running cannot be derived. Re-run with an explicit\n"+
				"  --runtime-id for the base you are rolling back.",
			len(baselines), baselineDir, strings.Join(baselines, ", "))
	}
	if p.SourceRt == "" {
		p.SourceRt = baselineDir
	}

	// Release id and channel: prefer what the release lane actually recorded, then the project config.
	state, _ := loadProjectCLIState(projectDir)
	if rec := recordedReleaseFor("ios", state); rec != nil {
		p.ReleaseID = strings.TrimSpace(rec.ReleaseID)
		p.Channel = strings.TrimSpace(rec.Channel)
		p.APIBase = strings.TrimSpace(rec.APIBase)
		p.SourceRel = projectStatePath(projectDir)
	}
	if p.ReleaseID == "" {
		p.ReleaseID = deriveReleaseIDForPlatform(projectDir, "ios")
		p.SourceRel = "derived from app id + version"
	}
	if p.ReleaseID == "" {
		return p, fmt.Errorf(
			"cannot determine the release id from %s or from the project identity.\n"+
				"  Run `soroq release --platforms=ios`, or pass --release-id.", projectStatePath(projectDir))
	}
	if p.Channel == "" {
		p.Channel = freehandProjectChannelOrDefault(projectDir)
	}
	if p.APIBase == "" {
		p.APIBase = defaultAPIBase()
	}
	return p, nil
}

// freehandProjectChannelOrDefault reads soroq.yaml's channel, defaulting to stable.
func freehandProjectChannelOrDefault(projectDir string) string {
	raw, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		return "stable"
	}
	if ch := strings.TrimSpace(parseTopLevelYaml(raw)["channel"]); ch != "" {
		return ch
	}
	return "stable"
}

// runRollbackIOSEngineCanonical publishes the SIGNED version-0 engine rollback for this project.
//
// It DERIVES the ids and DELEGATES to the proven `soroqctl rollback ios-engine` lane rather than
// rebuilding the manifest here. That lane runs signing.AssertEngineRollbackManifest BEFORE signing —
// a security control on an operation that reverts a user's app — and its own source records that the
// tree once carried a second rollback signer which owned those checks while the production path had
// none. Re-deriving the manifest shape in this package would recreate exactly that split.
//
// The seed is read in-process from the project's own .soroq/manifest_signing_key.seed (the key hosted
// publication signs with, and the one d9b9838f pins into the device bootstrap) and handed over through
// the environment. It never appears in argv, so it is not visible in `ps` or shell history, and it is
// never logged or persisted.
func runRollbackIOSEngineCanonical(projectDir string, explicitReleaseID, explicitRuntimeID, explicitAPI string, jsonOut bool) error {
	plan, err := deriveEngineRollbackPlan(projectDir, explicitRuntimeID)
	if err != nil {
		return err
	}
	if v := strings.TrimSpace(explicitReleaseID); v != "" {
		plan.ReleaseID, plan.SourceRel = v, "--release-id"
	}
	if v := strings.TrimSpace(explicitRuntimeID); v != "" {
		plan.RuntimeID, plan.SourceRt = v, "--runtime-id"
	}
	if v := strings.TrimSpace(explicitAPI); v != "" {
		plan.APIBase = v
	}

	// Credentials before signing: publishing needs an operator, and discovering that after a manifest
	// has been signed wastes the work and reports an auth problem as a rollback failure.
	if _, err := requireOperatorCredentials("", plan.APIBase, "publishing a signed rollback"); err != nil {
		return err
	}

	seed, err := resolveFreehandSigningSeed(projectDir, "")
	if err != nil {
		return fmt.Errorf(
			"cannot sign the rollback: %w\n"+
				"  The signed version-0 manifest must be signed with the SAME project key the app pins\n"+
				"  (.soroq/manifest_signing_key.seed). Without it a device correctly refuses the rollback.", err)
	}

	args := []string{
		"--app-id", plan.AppID,
		"--release-id", plan.ReleaseID,
		"--runtime-id", plan.RuntimeID,
		"--channel", plan.Channel,
		"--patch-id", engineRollbackPatchID(plan.RuntimeID, time.Now().UTC()),
		"--api", plan.APIBase,
	}
	if jsonOut {
		args = append(args, "--format", "json")
	}
	fmt.Fprintf(os.Stderr,
		"soroq rollback ios: signed version-0 engine rollback\n"+
			"  app        %s   (%s)\n"+
			"  release    %s   (%s)\n"+
			"  runtime    %s   (%s)\n"+
			"  channel    %s\n"+
			"  api        %s\n",
		plan.AppID, plan.SourceApp, plan.ReleaseID, plan.SourceRel,
		shortHex(plan.RuntimeID, 16)+"…", plan.SourceRt, plan.Channel, plan.APIBase)

	// The seed travels in the delegate's environment, never its argv.
	return runEngineLaneDelegateWithEnv("rollback", append([]string{"ios-engine"}, args...),
		[]string{"SOROQ_ENGINE_SIGNING_SEED=" + seed})
}

// engineRollbackPatchID names the version-0 rollback record.
//
// The id was previously derived from the runtime alone, which made it CONSTANT for a given base. The
// control plane's patch table is append-only and keys on the patch id, so a second rollback of the same
// runtime failed with a primary-key violation:
//
//	insert patch: ERROR: duplicate key value violates unique constraint "patches_pkey"
//
// That made the safety valve single-use per runtime. Rolling back, shipping a fix, and rolling back
// again is the ordinary life of a bad patch -- and the second attempt failed with a database error at
// exactly the moment an operator is trying to pull a broken release, while the bad patch stayed live.
//
// A rollback is an EVENT, not a piece of state, so each invocation gets its own record.
//
// The timestamp alone is NOT sufficient for that. At second resolution two rollbacks in the same second
// collide, and that is not a hypothetical: a retry after a transient network failure, a script rolling
// back several channels, or two operators reacting to the same bad patch all land inside one second.
// The failure mode is the worst kind -- a primary-key violation surfacing as "rollback failed" while
// the bad patch is still live -- so the id carries 40 bits of entropy as well.
//
// Ordering and provenance are preserved deliberately: the timestamp stays the leading, sortable part so
// `soroq patches list` reads chronologically and an operator can see when a rollback happened, with the
// random suffix only breaking ties. Sortable AND unique, not one at the expense of the other.
func engineRollbackPatchID(runtimeID string, now time.Time) string {
	return fmt.Sprintf("freehand-rollback-%s-v0-%s-%s",
		shortHex(runtimeID, 12), now.Format("20060102t150405z"), randomIDSuffix())
}

// randomIDSuffix returns 8 lowercase base32 characters (40 bits) from crypto/rand.
//
// crypto/rand rather than math/rand: patch ids are published records in a shared control plane, and a
// seeded PRNG would repeat across processes started in the same instant -- exactly the concurrent case
// this suffix exists to survive. Base32 without padding keeps the id case-insensitive and free of
// characters that need quoting in a shell or a URL.
func randomIDSuffix() string {
	var b [5]byte // 5 bytes -> exactly 8 base32 characters, no padding
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not something to paper over with a weaker source: a duplicate id would
		// silently fail the publish later. Fall back to nanosecond precision, which still distinguishes
		// same-second invocations, and let a genuine duplicate surface as an error rather than a guess.
		return fmt.Sprintf("%08x", time.Now().UTC().Nanosecond())
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

func shortHex(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
