package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
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
		return runRollbackConfigLane(args[0], args[1:])
	}
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
	if strings.TrimSpace(*patchID) == "" {
		return errors.New("--patch-id is required")
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
	fs.Usage = func() {
		fmt.Fprintf(os.Stdout, "usage: soroq rollback %s [--patch-id patch-123] [--release-id release-123] [--channel stable] [--project-dir .] [--api https://api.soroq.dev] [--verify] [--verify-client-id device-123] [--json]\n", platform)
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
	channel := *channelOverride
	if !flagWasSet(fs, "channel") && lastRelease != nil && strings.TrimSpace(lastRelease.Channel) != "" {
		channel = lastRelease.Channel
	}
	projectConfig, err := resolveProjectCommandConfig(status, channel)
	if err != nil {
		return err
	}

	resolvedReleaseID := strings.TrimSpace(*releaseID)
	if resolvedReleaseID == "" {
		if pin, ok := loadSoroqLockPin(status.ProjectDir, platform, ""); ok && strings.TrimSpace(pin.ReleaseID) != "" {
			resolvedReleaseID = strings.TrimSpace(pin.ReleaseID)
		} else if lastRelease != nil {
			resolvedReleaseID = strings.TrimSpace(lastRelease.ReleaseID)
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
