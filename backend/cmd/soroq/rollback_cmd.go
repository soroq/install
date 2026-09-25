package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"soroq/backend/internal/domain"
)

type rollbackSummary struct {
	Patch        domain.Patch                `json:"patch"`
	Verification *rollbackVerificationResult `json:"verification,omitempty"`
}

type rollbackVerificationResult struct {
	ClientID                string                    `json:"client_id"`
	CurrentPatchNumber      int                       `json:"current_patch_number"`
	PatchCheck              domain.PatchCheckResponse `json:"patch_check"`
	RolledBackNumberPresent bool                      `json:"rolled_back_number_present"`
	Verified                bool                      `json:"verified"`
}

func runRollback(args []string) error {
	// engine-lane rollback (version-0 signed manifest) is a distinct target; delegate to soroqctl.
	// This branch MUST stay byte-for-byte unchanged — it is the proven engine-lane internal.
	if len(args) > 0 && args[0] == "ios-engine" {
		return runEngineLaneDelegate("rollback", args[1:])
	}
	// Config-lane rollback wrapper (symmetric with `release`/`patch`): `soroq rollback android|ios`
	// resolves the newest rollback-able patch from local project config + recorded state, so a user
	// never needs to hand-copy a --patch-id. `rollback ios-engine` (above) stays delegated.
	if len(args) > 0 && (args[0] == "android" || args[0] == "ios") {
		// iOS freehand/engine projects: the developer-facing meaning of "roll back" is RETURN TO BASE,
		// which is the signed version-0 engine manifest — not the record rollback, which merely exposes
		// an older patch. Record rollback stays reachable through the explicit --patch-record flag.
		if args[0] == "ios" && !hasFlag(args[1:], "patch-record") {
			dir := releaseProjectDir(args[1:])
			if fh, _ := isFreehandIOSBuild(dir); fh {
				rel, _ := flagValue(args[1:], "release-id")
				rt, _ := flagValue(args[1:], "runtime-id")
				api, _ := flagValue(args[1:], "api")
				return runRollbackIOSEngineCanonical(dir, rel, rt, api, hasFlag(args[1:], "json"))
			}
		}
		return runRollbackConfigLane(args[0], stripFlag(args[1:], "patch-record", true))
	}
	// A WORD THAT IS NOT A PLATFORM IS A MISTAKE, NOT A NO-OP.
	//
	// `soroq release` and `soroq patch` both end their positional switch with a default arm that
	// prints usage. `rollback` had no switch: anything that was not android/ios/ios-engine fell
	// through to flag.Parse, which leaves a leading non-flag argument in fs.Args() and never reads it.
	// So `soroq rollback androd` DISCARDED the word and rolled back whatever platform the lockfile
	// held. On an iOS-only project a mistyped `androd` rolled back iOS -- a typo acting on a different
	// fleet, silently, with an exit code of zero.
	//
	// An audit proved it at the boundary rather than by reading: `rollback`, `rollback bogusx` and
	// `rollback android` produced byte-identical output.
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	patchID := fs.String("patch-id", "", "patch id to roll back")
	verify := fs.Bool("verify", false, "verify runtime patch-check no longer offers the rolled-back patch")
	verifyClientID := fs.String("verify-client-id", "soroq-rollback-verify", "client id to use for rollback patch-check verification")
	verifyCurrentPatchNumber := fs.Int("verify-current-patch-number", 0, "current patch number to report during rollback verification")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq rollback --patch-id patch-123 [--api https://api.soroq.dev] [--verify] [--verify-client-id device-123] [--json]
   or: soroq rollback android|ios [--patch-id patch-123] [--release-id release-123] [--channel stable] [--api ...] [--verify] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// Anything left here is a word the command parsed and would never read. See
	// refuseUnconsumedArguments for why this is checked AFTER the parse rather than at args[0].
	//
	// The hint names the ORDER, not just the words. `soroq rollback --api URL android` is refused
	// because the platform is routed by the first argument, and a message that only listed the valid
	// platforms would tell a reader their correctly-spelled word was invalid.
	if leftover := fs.Args(); len(leftover) > 0 {
		if err := refuseUnconsumedArguments("rollback", leftover, []string{
			"a platform as the FIRST argument: `soroq rollback android [flags]`",
			"`soroq rollback ios`",
			"`soroq rollback ios-engine`",
			"or no platform at all, to roll back the one this project has released",
		}); err != nil {
			return err
		}
	}
	if strings.TrimSpace(*patchID) == "" {
		// PROJECT-AWARE BY DEFAULT. The quickstart tells a beginner to run `soroq rollback`, and until
		// now that answered "--patch-id is required" -- demanding the raw identifier the product exists
		// to keep people away from. When the current directory is a Soroq project that has released
		// exactly one platform, that platform is the answer, and the platform-aware path already knows
		// how to find the newest rollback-able patch.
		//
		// An independent audit found this: the documentation checker verified the command and its flags
		// EXIST and never ran it, so a documented beginner step errored out while the docs scored clean.
		if platform, err := soleReleasedPlatform(releaseProjectDir(args)); err == nil {
			return runRollbackConfigLane(platform, args)
		} else if err != errNotASoroqProject {
			return err
		}
		return errors.New(
			"--patch-id is required outside a Soroq project.\n" +
				"Inside one, `soroq rollback` finds the patch for you; or name the platform: " +
				"`soroq rollback android` / `soroq rollback ios`")
	}
	if *verifyCurrentPatchNumber < 0 {
		return errors.New("--verify-current-patch-number must be zero or greater")
	}

	return performRollback(strings.TrimRight(*apiBase, "/"), strings.TrimSpace(*patchID), *verify, strings.TrimSpace(*verifyClientID), *verifyCurrentPatchNumber, *jsonOut)
}

// performRollback issues the rollback POST (existing path), optionally verifies the runtime
// patch-check, and prints the identical JSON/text output for both the advanced `--patch-id` path
// and the `rollback android|ios` wrapper.
func performRollback(resolvedAPIBase, patchID string, verify bool, verifyClientID string, verifyCurrentPatchNumber int, jsonOut bool) error {
	patch, err := postNoBodyDecode[domain.Patch](http.MethodPost, resolvedAPIBase+"/v1/patches/"+url.PathEscape(patchID)+"/rollback")
	if err != nil {
		return err
	}
	var verification *rollbackVerificationResult
	if verify {
		result, err := verifyRollbackPatchCheck(resolvedAPIBase, patch, verifyClientID, verifyCurrentPatchNumber)
		if err != nil {
			return err
		}
		verification = &result
	}

	if jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if verification != nil {
			return encoder.Encode(rollbackSummary{Patch: patch, Verification: verification})
		}
		return encoder.Encode(patch)
	}

	fmt.Fprintf(os.Stdout, "Rolled back patch %s\n", patch.ID)
	fmt.Fprintf(os.Stdout, "patch_number: %d\n", patch.Number)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", patch.AppID)
	fmt.Fprintf(os.Stdout, "release_id: %s\n", patch.ReleaseID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", patch.RuntimeID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", patch.Channel)
	fmt.Fprintf(os.Stdout, "rolled_back: %s\n", yesNo(patch.RolledBack))
	if verification != nil {
		fmt.Fprintf(os.Stdout, "rollback_verified: %s\n", yesNo(verification.Verified))
		fmt.Fprintf(os.Stdout, "patch_check_patch_available: %s\n", yesNo(verification.PatchCheck.PatchAvailable && verification.PatchCheck.Patch != nil))
		fmt.Fprintf(os.Stdout, "rolled_back_number_present: %s\n", yesNo(verification.RolledBackNumberPresent))
	}
	return nil
}

// recordedRelease is the platform-agnostic view of the last release `soroq release` recorded in
// .soroq/cli-state.json (LastAndroidRelease / LastIOSRelease share these fields).
type recordedRelease struct {
	APIBase   string
	AppID     string
	Channel   string
	ReleaseID string
}

func recordedReleaseFor(platform string, state projectCLIState) *recordedRelease {
	switch platform {
	case "android":
		if r := state.LastAndroidRelease; r != nil {
			return &recordedRelease{APIBase: r.APIBase, AppID: r.AppID, Channel: r.Channel, ReleaseID: r.ReleaseID}
		}
	case "ios":
		if r := state.LastIOSRelease; r != nil {
			return &recordedRelease{APIBase: r.APIBase, AppID: r.AppID, Channel: r.Channel, ReleaseID: r.ReleaseID}
		}
	}
	return nil
}

// runRollbackConfigLane implements `soroq rollback android|ios`. It resolves WHAT to roll back from
// local data (soroq.yaml + soroq.lock + cli-state), then reuses the existing rollback POST path.
//
// Resolution precedence:
//   - patch id : --patch-id (advanced override, verbatim) > newest non-rolled-back patch from
//     GET /v1/patches?app_id&channel&release_id
//   - app_id   : soroq.yaml (via resolveProjectCommandConfig)
//   - channel  : --channel > recorded release (cli-state) > soroq.yaml
//   - release  : --release-id > soroq.lock pin > cli-state Last{Android,IOS}Release
//   - api base : --api > recorded release api_base > defaultAPIBase()
func runRollbackConfigLane(platform string, args []string) error {
	fs := flag.NewFlagSet("rollback "+platform, flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	patchID := fs.String("patch-id", "", "advanced override: roll back this exact patch id instead of the resolved newest patch")
	releaseID := fs.String("release-id", "", "release id override (defaults to soroq.lock pin or the recorded release)")
	channelOverride := fs.String("channel", "", "channel override (defaults to soroq.yaml / the recorded release)")
	verify := fs.Bool("verify", false, "verify runtime patch-check no longer offers the rolled-back patch")
	verifyClientID := fs.String("verify-client-id", "soroq-rollback-verify", "client id to use for rollback patch-check verification")
	verifyCurrentPatchNumber := fs.Int("verify-current-patch-number", 0, "current patch number to report during rollback verification")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	flavorFlag := fs.String("flavor", "", "android only: the Flutter build flavor whose release to roll back; a flavor declared in soroq.yaml `flavors:` uses its own channel. Defaults to pubspec.yaml flutter.default-flavor.")
	fs.Usage = func() {
		// --runtime-id is listed because the engine lane's multiple-baseline refusal tells the operator
		// to use it; a remedy named in an error has to be discoverable in the usage text too.
		fmt.Fprintf(os.Stdout, "usage: soroq rollback %s [--patch-id patch-123] [--release-id release-123] [--runtime-id <64-hex base runtime>] [--channel stable] [--project-dir .] [--api https://api.soroq.dev] [--verify] [--verify-client-id device-123] [--json]\n", platform)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *verifyCurrentPatchNumber < 0 {
		return errors.New("--verify-current-patch-number must be zero or greater")
	}

	resolvedAPIBase := strings.TrimRight(*apiBase, "/")

	// Advanced override: an explicit --patch-id skips resolution and rolls back exactly that patch
	// (current behavior of `soroq rollback --patch-id`).
	if id := strings.TrimSpace(*patchID); id != "" {
		return performRollback(resolvedAPIBase, id, *verify, strings.TrimSpace(*verifyClientID), *verifyCurrentPatchNumber, *jsonOut)
	}

	status, err := inspectProject(*projectDir)
	if err != nil {
		return err
	}
	state, err := loadProjectCLIState(status.ProjectDir)
	if err != nil {
		return err
	}
	lastRelease := recordedReleaseFor(platform, state)

	if !flagWasSet(fs, "api") && lastRelease != nil && strings.TrimSpace(lastRelease.APIBase) != "" {
		resolvedAPIBase = strings.TrimRight(lastRelease.APIBase, "/")
	}
	flavorName := ""
	if platform == "android" {
		rf, _, err := resolveCommandFlavor(status.ProjectDir, *flavorFlag, nil)
		if err != nil {
			return err
		}
		flavorName = rf.Name
	} else if strings.TrimSpace(*flavorFlag) != "" {
		return fmt.Errorf("--flavor is supported for `soroq rollback android` only")
	}
	flavorChannel, flavorChannelDeclared, err := resolveReleaseFlavorChannel(fs, status.ProjectDir, flavorName, *channelOverride)
	if err != nil {
		return err
	}
	channel := *channelOverride
	if flavorChannelDeclared {
		channel = flavorChannel
		// A recorded release on another flavor's channel is not this flavor's release.
		if lastRelease != nil && strings.TrimSpace(lastRelease.Channel) != flavorChannel {
			lastRelease = nil
		}
	} else if !flagWasSet(fs, "channel") && lastRelease != nil && strings.TrimSpace(lastRelease.Channel) != "" {
		channel = lastRelease.Channel
	}
	projectConfig, err := resolveProjectCommandConfig(status, channel)
	if err != nil {
		return err
	}

	resolvedReleaseID := strings.TrimSpace(*releaseID)
	if resolvedReleaseID == "" {
		if fp, ok := loadSoroqLockFlavorPin(status.ProjectDir, platform, flavorName); ok && flavorChannelDeclared {
			resolvedReleaseID = strings.TrimSpace(fp.ReleaseID)
		} else if pin, ok := loadSoroqLockPin(status.ProjectDir, platform, ""); ok && strings.TrimSpace(pin.ReleaseID) != "" &&
			(!flavorChannelDeclared || releaseRecordedAsFlavor(status.ProjectDir, platform, strings.TrimSpace(pin.ReleaseID), flavorName)) {
			resolvedReleaseID = strings.TrimSpace(pin.ReleaseID)
		} else if lastRelease != nil {
			resolvedReleaseID = strings.TrimSpace(lastRelease.ReleaseID)
		} else if flavorChannelDeclared {
			if id, ok := latestRecordedFlavorRelease(status.ProjectDir, platform, flavorName); ok {
				resolvedReleaseID = id
			}
		}
	}
	if resolvedReleaseID == "" {
		return fmt.Errorf("could not resolve a %s release to roll back; run `soroq release %s` first, or pass --release-id / --patch-id", platform, platform)
	}

	resolvedPatchID, err := resolveNewestRollbackablePatch(resolvedAPIBase, projectConfig.AppID, projectConfig.Channel, resolvedReleaseID)
	if err != nil {
		return err
	}
	return performRollback(resolvedAPIBase, resolvedPatchID, *verify, strings.TrimSpace(*verifyClientID), *verifyCurrentPatchNumber, *jsonOut)
}

// resolveNewestRollbackablePatch lists patches for (app, channel, release) via the existing
// GET /v1/patches client and returns the id of the NEWEST patch that is not already rolled back.
// FileStore returns patches ascending by CreatedAt, but this does not rely on server order: it
// selects the maximum CreatedAt client-side (tie-break: higher patch number).
func resolveNewestRollbackablePatch(apiBase, appID, channel, releaseID string) (string, error) {
	query := url.Values{}
	if strings.TrimSpace(appID) != "" {
		query.Set("app_id", strings.TrimSpace(appID))
	}
	if strings.TrimSpace(channel) != "" {
		query.Set("channel", strings.TrimSpace(channel))
	}
	if strings.TrimSpace(releaseID) != "" {
		query.Set("release_id", strings.TrimSpace(releaseID))
	}
	listURL := strings.TrimRight(apiBase, "/") + "/v1/patches"
	if encoded := query.Encode(); encoded != "" {
		listURL += "?" + encoded
	}
	patches, err := getJSONDecode[[]domain.Patch](listURL)
	if err != nil {
		return "", err
	}
	var newest *domain.Patch
	for i := range patches {
		p := patches[i]
		if p.RolledBack {
			continue
		}
		if newest == nil ||
			p.CreatedAt.After(newest.CreatedAt) ||
			(p.CreatedAt.Equal(newest.CreatedAt) && p.Number > newest.Number) {
			selected := p
			newest = &selected
		}
	}
	if newest == nil {
		return "", fmt.Errorf("no rollback-able patch found for app %q channel %q release %q (none exist, or all are already rolled back)", appID, channel, releaseID)
	}
	return newest.ID, nil
}

func verifyRollbackPatchCheck(apiBase string, patch domain.Patch, clientID string, currentPatchNumber int) (rollbackVerificationResult, error) {
	if strings.TrimSpace(clientID) == "" {
		return rollbackVerificationResult{}, errors.New("--verify-client-id is required when --verify is used")
	}
	response, err := postRuntimeJSONDecode[domain.PatchCheckResponse](strings.TrimRight(apiBase, "/")+"/v1/patch-check", domain.PatchCheckRequest{
		AppID:              patch.AppID,
		ReleaseID:          patch.ReleaseID,
		RuntimeID:          patch.RuntimeID,
		Channel:            patch.Channel,
		Track:              patch.Track,
		CurrentPatchNumber: currentPatchNumber,
		ClientID:           clientID,
		Kind:               patch.Kind,
	})
	if err != nil {
		return rollbackVerificationResult{}, err
	}
	result := rollbackVerificationResult{
		ClientID:                clientID,
		CurrentPatchNumber:      currentPatchNumber,
		PatchCheck:              response,
		RolledBackNumberPresent: containsInt(response.RolledBackPatchNumbers, patch.Number),
	}
	if response.Patch != nil && response.Patch.ID == patch.ID {
		return result, fmt.Errorf("rollback verification failed: patch-check still offers rolled-back patch %s", patch.ID)
	}
	if !result.RolledBackNumberPresent {
		return result, fmt.Errorf("rollback verification failed: patch-check did not report rolled-back patch number %d", patch.Number)
	}
	result.Verified = true
	return result, nil
}

func containsInt(values []int, value int) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// errNotASoroqProject says the current directory is not a Soroq project, so nothing could be inferred
// from it. It is distinguished from every other failure because the caller falls back rather than
// stopping: running `soroq rollback --patch-id X` from anywhere at all is still supported.
var errNotASoroqProject = errors.New("not a soroq project")

// soleReleasedPlatform returns the one platform this project has released, when there is exactly one.
//
// EXACTLY ONE, on purpose. A project that has released both Android and iOS has two possible answers,
// and picking one would roll back a fleet the developer was not talking about. Ambiguity is reported
// with both names rather than resolved.
func soleReleasedPlatform(projectDir string) (string, error) {
	status, err := inspectProject(projectDir)
	if err != nil || !status.HasSoroqConfig {
		return "", errNotASoroqProject
	}
	lock, lockErr := loadSoroqLock(projectDir)
	if lockErr != nil {
		return "", errNotASoroqProject
	}
	var released []string
	for platform, pin := range lock.Platforms {
		if isSoroqLockFlavorKey(platform) {
			continue // a per-flavor pin, not a platform
		}
		if strings.TrimSpace(pin.ReleaseID) != "" {
			released = append(released, platform)
		}
	}
	sort.Strings(released)
	switch len(released) {
	case 1:
		return released[0], nil
	case 0:
		return "", fmt.Errorf(
			"this project has no registered release to roll back; run `soroq release android` " +
				"(or `soroq release ios`) first")
	default:
		return "", fmt.Errorf(
			"this project has released %s; say which one: `soroq rollback %s`",
			strings.Join(released, " and "), released[0])
	}
}
