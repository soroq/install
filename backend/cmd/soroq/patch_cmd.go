package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	androidpatch "soroq/backend/internal/androidpatch"
	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/domain"
)

type patchAndroidSummary struct {
	ProjectDir        string                               `json:"project_dir"`
	BaseArtifact      string                               `json:"base_artifact"`
	CandidateArtifact string                               `json:"candidate_artifact"`
	Kind              domain.PatchKind                     `json:"kind"`
	Plan              *androidpatch.Plan                   `json:"plan,omitempty"`
	Report            *androidpatch.AssetPatchBundleReport `json:"report,omitempty"`
	CodePlan          *androidpatch.CodePatchPlan          `json:"code_plan,omitempty"`
	CodeReport        *androidpatch.CodePatchBundleReport  `json:"code_report,omitempty"`
	Patch             domain.Patch                         `json:"patch"`
}

type patchConfigSummary struct {
	ProjectDir    string         `json:"project_dir"`
	ConfigFile    string         `json:"config_file"`
	ConfigSHA256  string         `json:"config_sha256"`
	ConfigBytes   int            `json:"config_bytes"`
	TargetRelease domain.Release `json:"target_release"`
	Patch         domain.Patch   `json:"patch"`
}

type patchListSummary struct {
	Count     int            `json:"count"`
	AppID     string         `json:"app_id,omitempty"`
	ReleaseID string         `json:"release_id,omitempty"`
	RuntimeID string         `json:"runtime_id,omitempty"`
	Channel   string         `json:"channel,omitempty"`
	Track     string         `json:"track,omitempty"`
	Patches   []domain.Patch `json:"patches"`
}

type patchRolloutSummary struct {
	Patch          domain.Patch `json:"patch"`
	RolloutPercent int          `json:"rollout_percent"`
}

type patchTrackSummary struct {
	Patch          domain.Patch `json:"patch"`
	Track          string       `json:"track"`
	RolloutPercent int          `json:"rollout_percent"`
}

func runPatch(args []string) error {
	if len(args) == 0 {
		patchUsage()
		return errAlreadyPrinted
	}

	switch args[0] {
	case "android":
		return runPatchAndroid(args[1:])
	case "config":
		return runPatchConfig(args[1:])
	case "ios":
		// Unified iOS UX: `patch ios --config-file ...` = config/data lane (default);
		// `patch ios --engine` / `patch ios --toolchain <ios-r3> ...` = hard engine lane
		// (delegates to soroqctl). Never silently mixes the two.
		if patchIOSEngineRequested(args[1:]) {
			return runPatchIOSEngineScaffolded(stripEngineRoutingFlag(args[1:]))
		}
		return runPatchIOS(args[1:])
	case "ios-engine":
		return runEngineLaneDelegate("patch", args[1:])
	case "health":
		return runPatchHealth(args[1:])
	case "list":
		return runPatchList(args[1:])
	case "promote":
		return runPatchRollout(args[1:], 100, "promote")
	case "rollout":
		return runPatchRollout(args[1:], -1, "rollout")
	case "set-track":
		return runPatchSetTrack(args[1:])
	case "status":
		return runPatchStatus(args[1:])
	case "-h", "--help", "help":
		patchUsage()
		return nil
	default:
		patchUsage()
		return errAlreadyPrinted
	}
}

// iosPatchLaneNote returns an honest, per-patch description. The same `patch ios` command can
// carry dart_eval bytecode inside the signed config, so when code_evc_base64 is present this is
// the patch-point lane (NOT App-Store-safe / NOT Shorebird parity); otherwise it is ordinary
// remote config. Either way, no native code / dylib / Mach-O / engine / JIT is downloaded.
func iosPatchLaneNote(carriesCode bool) string {
	if carriesCode {
		return "note: this config carries dart_eval bytecode (code_evc_base64) — the patch-point lane, run by the bundled runtime. NOT Shorebird parity; NOT App-Store-safe until an App Review pilot. No native code, dylib, Mach-O, replacement engine, or JIT is downloaded."
	}
	return "note: signed JSON config/data OTA (no executable code) — ordinary remote config. No native code, dylib, Mach-O, replacement engine, or JIT is downloaded."
}

func patchUsage() {
	fmt.Fprintln(os.Stdout, `usage: soroq patch <target> [flags]

targets:
  android  publish a hosted Android patch from a shipped base artifact and a local candidate artifact
  config   publish a hosted JSON config patch for an existing release
  ios      config/data lane: --config-file ... (signed JSON config OTA). Hard ENGINE lane:
           add --engine or --toolchain <ios-r3> to publish an experimental Dart-code engine
           patch (delegates to soroqctl; same as patch ios-engine).
  ios-engine  compile + sign an experimental engine-lane Dart-code patch (delegates to soroqctl)
  health   inspect patch install health and rollback state
  list     list patches in the control plane
  promote  promote a patch to 100 percent rollout
  rollout  update a patch rollout percentage
  set-track set a patch track such as stable, staging, or beta
  status   inspect a patch record in the control plane`)
}

func runPatchSetTrack(args []string) error {
	fs := flag.NewFlagSet("patches set-track", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	patchID := fs.String("patch-id", "", "patch id to update")
	track := fs.String("track", "", "track to set, such as stable, staging, or beta")
	rollout := fs.Int("rollout", 100, "rollout percentage for the target track")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq patches set-track --patch-id patch-123 --track stable [--rollout 100] [--api https://api.soroq.dev] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	resolvedPatchID := strings.TrimSpace(*patchID)
	if resolvedPatchID == "" {
		return errors.New("--patch-id is required")
	}
	resolvedTrack := domain.NormalizePatchTrack(*track)
	if resolvedTrack == "" {
		return errors.New("--track is required")
	}
	if !domain.IsKnownPatchTrack(resolvedTrack) {
		return fmt.Errorf("--track should be a slug such as stable, staging, or beta; got %q", *track)
	}
	if *rollout < 0 || *rollout > 100 {
		return errors.New("--rollout must be between 0 and 100")
	}

	patch, err := postJSONDecode[domain.Patch](
		strings.TrimRight(*apiBase, "/")+"/v1/patches/"+url.PathEscape(resolvedPatchID)+"/track",
		domain.UpdatePatchTrackRequest{Track: resolvedTrack, RolloutPercent: *rollout},
	)
	if err != nil {
		return err
	}

	summary := patchTrackSummary{
		Patch:          patch,
		Track:          resolvedTrack,
		RolloutPercent: patch.RolloutPercent,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Set patch %s track to %s\n", patch.ID, resolvedTrack)
	fmt.Fprintf(os.Stdout, "patch_number: %d\n", patch.Number)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", patch.AppID)
	fmt.Fprintf(os.Stdout, "release_id: %s\n", patch.ReleaseID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", patch.RuntimeID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", patch.Channel)
	fmt.Fprintf(os.Stdout, "track: %s\n", domain.NormalizePatchTrack(patch.Track))
	fmt.Fprintf(os.Stdout, "rollout_percent: %d\n", patch.RolloutPercent)
	return nil
}

func runPatchRollout(args []string, defaultRollout int, commandName string) error {
	fs := flag.NewFlagSet("patch "+commandName, flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	patchID := fs.String("patch-id", "", "patch id to update")
	rollout := fs.Int("rollout", defaultRollout, "rollout percentage to set")
	percent := fs.Int("percent", defaultRollout, "rollout percentage to set")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		if commandName == "promote" {
			fmt.Fprintln(os.Stdout, `usage: soroq patch promote --patch-id patch-123 [--api https://api.soroq.dev] [--json]`)
			return
		}
		fmt.Fprintln(os.Stdout, `usage: soroq patch rollout --patch-id patch-123 --percent 25 [--api https://api.soroq.dev] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	resolvedPatchID := strings.TrimSpace(*patchID)
	if resolvedPatchID == "" {
		return errors.New("--patch-id is required")
	}
	resolvedRollout := *rollout
	if flagWasSet(fs, "percent") {
		resolvedRollout = *percent
	}
	if resolvedRollout < 0 || resolvedRollout > 100 {
		return errors.New("--percent/--rollout must be between 0 and 100")
	}

	patch, err := postJSONDecode[domain.Patch](
		strings.TrimRight(*apiBase, "/")+"/v1/patches/"+url.PathEscape(resolvedPatchID)+"/rollout",
		domain.UpdatePatchRolloutRequest{RolloutPercent: resolvedRollout},
	)
	if err != nil {
		return err
	}

	summary := patchRolloutSummary{
		Patch:          patch,
		RolloutPercent: patch.RolloutPercent,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	if commandName == "promote" && patch.RolloutPercent == 100 {
		fmt.Fprintf(os.Stdout, "Promoted patch %s to stable rollout\n", patch.ID)
	} else {
		fmt.Fprintf(os.Stdout, "Updated patch %s rollout\n", patch.ID)
	}
	fmt.Fprintf(os.Stdout, "patch_number: %d\n", patch.Number)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", patch.AppID)
	fmt.Fprintf(os.Stdout, "release_id: %s\n", patch.ReleaseID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", patch.RuntimeID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", patch.Channel)
	fmt.Fprintf(os.Stdout, "rollout_percent: %d\n", patch.RolloutPercent)
	return nil
}

func resolvePatchTrackAndRollout(rawTrack string, rollout int, rolloutWasSet bool) (string, int, error) {
	resolvedTrack := domain.NormalizePatchTrack(rawTrack)
	if !domain.IsKnownPatchTrack(resolvedTrack) {
		return "", 0, fmt.Errorf("--track should be a slug such as stable, staging, or beta; got %q", rawTrack)
	}
	resolvedRollout := rollout
	_ = rolloutWasSet
	if resolvedRollout < 0 || resolvedRollout > 100 {
		return "", 0, errors.New("--rollout must be between 0 and 100")
	}
	return resolvedTrack, resolvedRollout, nil
}

func runPatchList(args []string) error {
	fs := flag.NewFlagSet("patch list", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	appID := fs.String("app-id", "", "optional app id filter")
	releaseID := fs.String("release-id", "", "optional release id filter")
	runtimeID := fs.String("runtime-id", "", "optional runtime id filter")
	channel := fs.String("channel", "", "optional channel filter")
	track := fs.String("track", "", "optional patch track filter, such as stable, staging, or beta")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq patch list [--api https://api.soroq.dev] [--app-id com.example.app] [--release-id release-123] [--runtime-id runtime-123] [--channel stable] [--track stable|staging|beta] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	query := url.Values{}
	resolvedAppID := strings.TrimSpace(*appID)
	if resolvedAppID != "" {
		if !looksLikeSoroqAppID(resolvedAppID) {
			return fmt.Errorf("--app-id %q should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens", resolvedAppID)
		}
		query.Set("app_id", resolvedAppID)
	}
	resolvedReleaseID := strings.TrimSpace(*releaseID)
	if resolvedReleaseID != "" {
		query.Set("release_id", resolvedReleaseID)
	}
	resolvedRuntimeID := strings.TrimSpace(*runtimeID)
	if resolvedRuntimeID != "" {
		query.Set("runtime_id", resolvedRuntimeID)
	}
	resolvedChannel := strings.TrimSpace(*channel)
	if resolvedChannel != "" {
		if !looksLikeChannel(resolvedChannel) {
			return fmt.Errorf("--channel %q should be a stable slug such as stable, beta, or production", resolvedChannel)
		}
		query.Set("channel", resolvedChannel)
	}
	resolvedTrack := strings.TrimSpace(*track)
	if resolvedTrack != "" {
		resolvedTrack = domain.NormalizePatchTrack(resolvedTrack)
		if !domain.IsKnownPatchTrack(resolvedTrack) {
			return fmt.Errorf("--track should be a slug such as stable, staging, or beta; got %q", *track)
		}
		query.Set("track", resolvedTrack)
	}
	listURL := strings.TrimRight(*apiBase, "/") + "/v1/patches"
	if encodedQuery := query.Encode(); encodedQuery != "" {
		listURL += "?" + encodedQuery
	}
	patches, err := getJSONDecode[[]domain.Patch](listURL)
	if err != nil {
		return err
	}

	summary := patchListSummary{
		Count:     len(patches),
		AppID:     resolvedAppID,
		ReleaseID: resolvedReleaseID,
		RuntimeID: resolvedRuntimeID,
		Channel:   resolvedChannel,
		Track:     resolvedTrack,
		Patches:   patches,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Soroq patches: %d\n", len(patches))
	for _, patch := range patches {
		fmt.Fprintf(os.Stdout, "- %s\t#%d\t%s\t%s\t%s\t%s\trolled_back=%s\n", patch.ID, patch.Number, patch.AppID, patch.ReleaseID, patch.Channel, domain.NormalizePatchTrack(patch.Track), yesNo(patch.RolledBack))
	}
	return nil
}

func runPatchStatus(args []string) error {
	fs := flag.NewFlagSet("patch status", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	patchID := fs.String("patch-id", "", "patch id to inspect")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq patch status --patch-id patch-123 [--api https://api.soroq.dev] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	resolvedPatchID := strings.TrimSpace(*patchID)
	if resolvedPatchID == "" {
		return errors.New("--patch-id is required")
	}

	patch, err := getJSONDecode[domain.Patch](strings.TrimRight(*apiBase, "/") + "/v1/patches/" + url.PathEscape(resolvedPatchID))
	if err != nil {
		return err
	}

	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(patch)
	}

	fmt.Fprintf(os.Stdout, "Soroq patch %s\n", patch.ID)
	fmt.Fprintf(os.Stdout, "patch_number: %d\n", patch.Number)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", patch.AppID)
	fmt.Fprintf(os.Stdout, "release_id: %s\n", patch.ReleaseID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", patch.RuntimeID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", patch.Channel)
	fmt.Fprintf(os.Stdout, "track: %s\n", domain.NormalizePatchTrack(patch.Track))
	fmt.Fprintf(os.Stdout, "kind: %s\n", patch.Kind)
	fmt.Fprintf(os.Stdout, "activation_mode: %s\n", patch.ActivationMode)
	fmt.Fprintf(os.Stdout, "rollout_percent: %d\n", patch.RolloutPercent)
	fmt.Fprintf(os.Stdout, "rolled_back: %s\n", yesNo(patch.RolledBack))
	return nil
}

func runPatchHealth(args []string) error {
	fs := flag.NewFlagSet("patch health", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	patchID := fs.String("patch-id", "", "patch id to inspect")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq patch health --patch-id patch-123 [--api https://api.soroq.dev] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	resolvedPatchID := strings.TrimSpace(*patchID)
	if resolvedPatchID == "" {
		return errors.New("--patch-id is required")
	}

	health, err := getJSONDecode[domain.PatchHealth](strings.TrimRight(*apiBase, "/") + "/v1/patches/" + url.PathEscape(resolvedPatchID) + "/health")
	if err != nil {
		return err
	}

	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(health)
	}

	fmt.Fprintf(os.Stdout, "Patch health %s\n", health.PatchID)
	fmt.Fprintf(os.Stdout, "patch_number: %d\n", health.PatchNumber)
	fmt.Fprintf(os.Stdout, "success_count: %d\n", health.SuccessCount)
	fmt.Fprintf(os.Stdout, "failure_count: %d\n", health.FailureCount)
	fmt.Fprintf(os.Stdout, "last_event_kind: %s\n", health.LastEventKind)
	fmt.Fprintf(os.Stdout, "rolled_back: %s\n", yesNo(health.RolledBack))
	return nil
}

func runPatchAndroid(args []string) error {
	fs := flag.NewFlagSet("patch android", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	baseArtifactPath := fs.String("base-artifact", "", "path to the shipped Android APK or AAB")
	candidateArtifactPath := fs.String("candidate-artifact", "", "path to the candidate Android APK or AAB")
	buildBeforeDiscover := fs.Bool("build", true, "run flutter build before discovering the candidate Android artifact when --candidate-artifact is omitted")
	buildArtifactType := fs.String("artifact-type", "aab", "artifact type to build when --candidate-artifact is omitted: aab or apk")
	toolchainVersion := fs.String("toolchain", "", "resolve the Android engine from the cached toolchain at ~/.soroq/toolchains/<version>/android (installed by `soroq toolchain install`); replaces the local repo engine checkout. Consistent with `patch ios-engine --toolchain`.")
	releaseID := fs.String("release-id", "", "existing release id that this patch targets")
	releaseVersion := fs.String("release-version", "", "release version to patch; use latest for the newest Android release in the channel")
	patchID := fs.String("patch-id", "", "patch id override")
	channel := fs.String("channel", "", "channel override (defaults to soroq.yaml)")
	track := fs.String("track", "", "patch track: stable or staging")
	rollout := fs.Int("rollout", 100, "rollout percentage")
	activation := fs.String("activation", string(domain.ActivationNextColdStart), "activation mode")
	manifestKeyID := fs.String("manifest-key-id", "", "optional server-side manifest signing key id for this patch")
	patchKindRaw := fs.String("kind", "auto", "patch kind: auto, asset, code, or experimental_native_aot")
	codeDeltaStrategy := fs.String("code-delta-strategy", "default", "code delta strategy for code patches: default or v15")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	allowEmpty := fs.Bool("allow-empty", false, "publish an empty patch when no overlay asset changes are detected")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq patch android [--release-version latest|1.2.3+45] [--base-artifact .soroq/releases/my-release/app-release.aab] [--candidate-artifact build/app/outputs/bundle/release/app-release.aab] [--release-id my-release] [--build=false] [--artifact-type aab|apk] [--toolchain <version>] [--project-dir .] [--api https://api.soroq.dev] [--patch-id my-patch] [--channel stable] [--track stable|staging|beta] [--kind auto|asset|code] [--rollout 100] [--activation next_cold_start] [--manifest-key-id prod-primary] [--allow-empty] [--json] [-- <flutter build flags>]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	flutterBuildArgs := fs.Args()
	resolvedTrack, resolvedRollout, err := resolvePatchTrackAndRollout(*track, *rollout, flagWasSet(fs, "rollout"))
	if err != nil {
		return err
	}
	requestedPatchKind, err := normalizeAndroidPatchKindFlag(*patchKindRaw)
	if err != nil {
		return err
	}

	status, err := inspectProject(*projectDir)
	if err != nil {
		return err
	}
	state, err := loadProjectCLIState(status.ProjectDir)
	if err != nil {
		return err
	}
	lastRelease := state.LastAndroidRelease
	resolvedAPIBase := strings.TrimRight(*apiBase, "/")
	if !flagWasSet(fs, "api") && lastRelease != nil && strings.TrimSpace(lastRelease.APIBase) != "" {
		resolvedAPIBase = strings.TrimRight(lastRelease.APIBase, "/")
	}
	channelOverride := *channel
	if !flagWasSet(fs, "channel") && lastRelease != nil && strings.TrimSpace(lastRelease.Channel) != "" {
		channelOverride = lastRelease.Channel
	}
	projectConfig, err := resolveProjectCommandConfig(status, channelOverride)
	if err != nil {
		return err
	}

	resolvedCandidateArtifactPath := strings.TrimSpace(*candidateArtifactPath)
	candidateArtifactResolvedEarly := false
	resolvedReleaseID := strings.TrimSpace(*releaseID)
	resolvedReleaseVersion := strings.TrimSpace(*releaseVersion)
	if resolvedReleaseID != "" && resolvedReleaseVersion != "" {
		return errors.New("use either --release-id or --release-version, not both")
	}
	var selectedHostedRelease *domain.Release
	if resolvedReleaseID == "" && resolvedReleaseVersion == "" && lastRelease == nil {
		resolvedCandidateArtifactPath, err = resolveCandidateArtifactForReleaseSelection(
			status.ProjectDir,
			resolvedCandidateArtifactPath,
			*buildBeforeDiscover,
			*buildArtifactType,
			strings.TrimSpace(*toolchainVersion),
			flutterBuildArgs,
		)
		if err != nil {
			return err
		}
		candidateArtifactResolvedEarly = true
		candidateSnapshot, err := inspectAndroidArtifact(resolvedCandidateArtifactPath)
		if err != nil {
			return err
		}
		inferredVersion, err := resolveReleaseVersion(candidateSnapshot.Metadata, "")
		if err != nil {
			return fmt.Errorf("release version could not be inferred from the candidate artifact; pass --release-version or --release-id: %w", err)
		}
		resolvedReleaseVersion = inferredVersion
	}
	if resolvedReleaseVersion != "" {
		selectedRelease, err := selectAndroidReleaseForPatch(
			resolvedAPIBase,
			projectConfig.AppID,
			projectConfig.Channel,
			resolvedReleaseVersion,
		)
		if err != nil {
			return err
		}
		selectedHostedRelease = &selectedRelease
		resolvedReleaseID = selectedRelease.ID
	} else if resolvedReleaseID == "" && lastRelease != nil {
		resolvedReleaseID = lastRelease.ReleaseID
	}
	if resolvedReleaseID == "" {
		return errors.New("--release-id or --release-version is required unless `soroq release android` has already recorded a release")
	}
	resolvedBaseArtifactPath := strings.TrimSpace(*baseArtifactPath)
	if resolvedBaseArtifactPath == "" && lastRelease != nil && lastRelease.ReleaseID == resolvedReleaseID {
		resolvedBaseArtifactPath = lastRelease.ArtifactPath
	}
	if resolvedBaseArtifactPath == "" || (!flagWasSet(fs, "base-artifact") && !fileExists(resolvedBaseArtifactPath)) {
		downloadedPath, err := downloadReleaseArtifact(resolvedAPIBase, resolvedReleaseID, status.ProjectDir)
		if err != nil {
			if resolvedBaseArtifactPath == "" {
				return fmt.Errorf("--base-artifact is required unless `soroq release android` has recorded or uploaded a release artifact: %w", err)
			}
			return fmt.Errorf("local base artifact %s is missing and hosted release artifact download failed: %w", resolvedBaseArtifactPath, err)
		}
		resolvedBaseArtifactPath = downloadedPath
		if lastRelease != nil {
			lastRelease.ArtifactPath = downloadedPath
			state.LastAndroidRelease = lastRelease
			_ = saveProjectCLIState(status.ProjectDir, state)
		}
	}
	if resolvedBaseArtifactPath == "" {
		return errors.New("--base-artifact is required unless `soroq release android` has already recorded a release")
	}

	workDir, err := os.MkdirTemp("", "soroq-patch-android-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	if resolvedCandidateArtifactPath == "" && *buildBeforeDiscover {
		stashedBaseArtifactPath, err := stashAndroidReleaseArtifact(status.ProjectDir, resolvedReleaseID, resolvedBaseArtifactPath)
		if err != nil {
			return err
		}
		if filepath.Clean(stashedBaseArtifactPath) != filepath.Clean(resolvedBaseArtifactPath) {
			resolvedBaseArtifactPath = stashedBaseArtifactPath
			if lastRelease != nil && lastRelease.ReleaseID == resolvedReleaseID && !flagWasSet(fs, "base-artifact") {
				lastRelease.ArtifactPath = stashedBaseArtifactPath
				state.LastAndroidRelease = lastRelease
				if err := saveProjectCLIState(status.ProjectDir, state); err != nil {
					return err
				}
			}
		}
	}

	baseSnapshot, err := androidrelease.CaptureSnapshot(resolvedBaseArtifactPath)
	if err != nil {
		return err
	}
	baseSnapshot.Artifact.Source = "release"
	if baseSnapshot.Metadata.Soroq.AppID == "" {
		return fmt.Errorf("base artifact %s is missing bundled soroq.app_id metadata", baseSnapshot.Artifact.Path)
	}
	if baseSnapshot.Metadata.Soroq.AppID != projectConfig.AppID {
		return fmt.Errorf("base artifact app_id %q does not match soroq.yaml app_id %q", baseSnapshot.Metadata.Soroq.AppID, projectConfig.AppID)
	}
	if baseSnapshot.Metadata.Soroq.Channel != "" && baseSnapshot.Metadata.Soroq.Channel != projectConfig.Channel {
		return fmt.Errorf("base artifact channel %q does not match requested channel %q", baseSnapshot.Metadata.Soroq.Channel, projectConfig.Channel)
	}
	if strings.TrimSpace(baseSnapshot.Metadata.Soroq.RuntimeID) == "" {
		return fmt.Errorf("base artifact %s is missing bundled soroq.runtime_id metadata", baseSnapshot.Artifact.Path)
	}
	candidateFromLastRelease := false
	candidateBuildRan := false
	if !candidateArtifactResolvedEarly && len(flutterBuildArgs) > 0 && (resolvedCandidateArtifactPath != "" || !*buildBeforeDiscover) {
		return errors.New("Flutter build passthrough args require automatic build; omit --candidate-artifact and keep --build=true")
	}
	if resolvedCandidateArtifactPath == "" && *buildBeforeDiscover {
		if err := runFlutterAndroidReleaseBuild(status.ProjectDir, *buildArtifactType, strings.TrimSpace(*toolchainVersion), flutterBuildArgs); err != nil {
			return err
		}
		candidateBuildRan = true
	}
	if resolvedCandidateArtifactPath == "" {
		resolvedCandidateArtifactPath, err = discoverCompatibleCandidateArtifact(status.ProjectDir, baseSnapshot)
		if errors.Is(err, os.ErrNotExist) {
			if candidateBuildRan {
				resolvedCandidateArtifactPath, err = discoverSamePathCandidateArtifactAfterBuild(status.ProjectDir, baseSnapshot)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if resolvedCandidateArtifactPath == "" && !*allowEmpty {
				return errors.New("no compatible candidate Android artifact found; build a candidate APK/AAB, pass --candidate-artifact, or use --allow-empty for an explicit empty patch")
			}
			if resolvedCandidateArtifactPath == "" {
				resolvedCandidateArtifactPath = resolvedBaseArtifactPath
				candidateFromLastRelease = true
			}
		} else if err != nil {
			return err
		}
	}
	resolvedManifestKeyID := strings.TrimSpace(*manifestKeyID)
	if resolvedManifestKeyID == "" && lastRelease != nil && lastRelease.ReleaseID == resolvedReleaseID {
		resolvedManifestKeyID = strings.TrimSpace(lastRelease.ManifestSigningKeyID)
	}
	if resolvedManifestKeyID == "" && selectedHostedRelease != nil {
		resolvedManifestKeyID = strings.TrimSpace(selectedHostedRelease.ManifestSigningKeyID)
	}
	if resolvedManifestKeyID == "" {
		resolvedManifestKeyID = firstManifestSigningKeyID(baseSnapshot.Metadata)
	}
	resolvedAllowEmpty := *allowEmpty || (candidateFromLastRelease && !flagWasSet(fs, "candidate-artifact"))

	baseSnapshotPath := filepath.Join(workDir, "base-snapshot.json")
	if err := writeJSONFile(baseSnapshotPath, baseSnapshot); err != nil {
		return err
	}

	codePlan, err := androidpatch.PrepareCodePatchPlan(androidpatch.CodePatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: resolvedCandidateArtifactPath,
		ReleaseID:             resolvedReleaseID,
		ActivationMode:        strings.TrimSpace(*activation),
		WorkspaceOut:          filepath.Join(workDir, "code-workspace"),
		Strict:                true,
	})
	if err != nil {
		return err
	}
	codePlanPath := filepath.Join(workDir, "code-plan.json")
	if err := writeJSONFile(codePlanPath, codePlan); err != nil {
		return err
	}
	if requestedPatchKind == domain.PatchKindExperimentalNativeAOT || (requestedPatchKind == "" && codePlan.Ready) {
		return publishAndroidCodePatch(publishAndroidCodePatchOptions{
			ProjectDir:            status.ProjectDir,
			APIBase:               resolvedAPIBase,
			BaseArtifactPath:      resolvedBaseArtifactPath,
			CandidateArtifactPath: resolvedCandidateArtifactPath,
			ReleaseID:             resolvedReleaseID,
			PatchID:               strings.TrimSpace(*patchID),
			AppID:                 projectConfig.AppID,
			Channel:               projectConfig.Channel,
			RuntimeID:             strings.TrimSpace(baseSnapshot.Metadata.Soroq.RuntimeID),
			ActivationMode:        domain.ActivationMode(strings.TrimSpace(*activation)),
			Track:                 resolvedTrack,
			RolloutPercent:        resolvedRollout,
			ManifestSigningKeyID:  resolvedManifestKeyID,
			CodePlan:              codePlan,
			CodePlanPath:          codePlanPath,
			WorkDir:               workDir,
			CodeDeltaStrategy:     *codeDeltaStrategy,
			JSONOut:               *jsonOut,
		})
	}
	if requestedPatchKind == domain.PatchKindExperimentalNativeAOT && !codePlan.Ready {
		return fmt.Errorf("android code patch plan is blocked: %s", summarizeCodePatchBlockers(codePlan.Blockers))
	}

	plan, err := androidpatch.PreparePlan(androidpatch.PlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: resolvedCandidateArtifactPath,
		ReleaseID:             resolvedReleaseID,
		PatchKind:             string(domain.PatchKindAsset),
		ActivationMode:        strings.TrimSpace(*activation),
		Strict:                true,
	})
	if err != nil {
		return err
	}
	if requestedPatchKind == domain.PatchKindAsset && !plan.Ready {
		return fmt.Errorf("android asset patch plan is blocked: %s", summarizeAssetPatchBlockers(plan.Blockers))
	}
	if requestedPatchKind == "" && !plan.Ready {
		return fmt.Errorf(
			"android patch auto-detection is blocked; code lane: %s; asset lane: %s",
			summarizeCodePatchBlockers(codePlan.Blockers),
			summarizeAssetPatchBlockers(plan.Blockers),
		)
	}
	planPath := filepath.Join(workDir, "asset-plan.json")
	if err := writeJSONFile(planPath, plan); err != nil {
		return err
	}

	preflightPatchID := strings.TrimSpace(*patchID)
	if preflightPatchID == "" {
		preflightPatchID = "preflight"
	}
	if _, _, err := androidpatch.BuildAssetPatchBundle(androidpatch.AssetPatchBuildOptions{
		PatchPlanPath: planPath,
		PatchID:       preflightPatchID,
		PatchNumber:   1,
		ReleaseID:     resolvedReleaseID,
		OutputPath:    filepath.Join(workDir, "preflight.zip"),
		AllowEmpty:    resolvedAllowEmpty,
	}); err != nil {
		if requestedPatchKind == "" && strings.Contains(err.Error(), "no overlay asset changes detected") {
			return fmt.Errorf(
				"no patchable Android changes detected; code lane: %s; asset lane: %s",
				summarizeCodePatchBlockers(codePlan.Blockers),
				err,
			)
		}
		return err
	}

	resolvedPatchID := strings.TrimSpace(*patchID)
	if resolvedPatchID == "" {
		resolvedPatchID = defaultPatchID(projectConfig.AppID, projectConfig.Channel)
	}
	patchEndpointBase := resolvedAPIBase + "/v1/patches/" + resolvedPatchID
	patch, err := postJSONDecode[domain.Patch](resolvedAPIBase+"/v1/patches", domain.CreatePatchRequest{
		ID:                   resolvedPatchID,
		AppID:                projectConfig.AppID,
		ReleaseID:            resolvedReleaseID,
		RuntimeID:            strings.TrimSpace(baseSnapshot.Metadata.Soroq.RuntimeID),
		Channel:              projectConfig.Channel,
		Track:                resolvedTrack,
		Kind:                 domain.PatchKindAsset,
		ActivationMode:       domain.ActivationMode(strings.TrimSpace(*activation)),
		ManifestURL:          patchEndpointBase + "/manifest",
		BundleURL:            patchEndpointBase + "/bundle",
		RolloutPercent:       resolvedRollout,
		ManifestSigningKeyID: resolvedManifestKeyID,
	})
	if err != nil {
		return err
	}

	report, bundleBytes, err := androidpatch.BuildAssetPatchBundle(androidpatch.AssetPatchBuildOptions{
		PatchPlanPath: planPath,
		PatchID:       patch.ID,
		PatchNumber:   uint32(patch.Number),
		ReleaseID:     patch.ReleaseID,
		OutputPath:    filepath.Join(workDir, patch.ID+".zip"),
		AllowEmpty:    resolvedAllowEmpty,
	})
	if err != nil {
		return err
	}
	if err := uploadPatchBundleBytes(patch.BundleURL, bundleBytes); err != nil {
		return err
	}

	summary := patchAndroidSummary{
		ProjectDir:        status.ProjectDir,
		BaseArtifact:      filepath.Clean(resolvedBaseArtifactPath),
		CandidateArtifact: filepath.Clean(resolvedCandidateArtifactPath),
		Kind:              domain.PatchKindAsset,
		Plan:              plan,
		Report:            report,
		Patch:             patch,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Published Android asset patch %s\n", patch.ID)
	fmt.Fprintf(os.Stdout, "patch_number: %d\n", patch.Number)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", patch.AppID)
	fmt.Fprintf(os.Stdout, "release_id: %s\n", patch.ReleaseID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", patch.RuntimeID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", patch.Channel)
	fmt.Fprintf(os.Stdout, "track: %s\n", domain.NormalizePatchTrack(patch.Track))
	fmt.Fprintf(os.Stdout, "kind: %s\n", patch.Kind)
	fmt.Fprintf(os.Stdout, "rollout_percent: %d\n", patch.RolloutPercent)
	fmt.Fprintf(os.Stdout, "base_artifact: %s\n", summary.BaseArtifact)
	fmt.Fprintf(os.Stdout, "candidate_artifact: %s\n", summary.CandidateArtifact)
	fmt.Fprintf(os.Stdout, "overlay_files: %d\n", len(report.OverlayEntries))
	if resolvedAllowEmpty && len(report.OverlayEntries) == 0 {
		fmt.Fprintln(os.Stdout, "empty_patch: yes")
	}
	printAndroidPatchNextSteps(patch.ID)
	return nil
}

type publishAndroidCodePatchOptions struct {
	ProjectDir            string
	APIBase               string
	BaseArtifactPath      string
	CandidateArtifactPath string
	ReleaseID             string
	PatchID               string
	AppID                 string
	Channel               string
	RuntimeID             string
	ActivationMode        domain.ActivationMode
	Track                 string
	RolloutPercent        int
	ManifestSigningKeyID  string
	CodePlan              *androidpatch.CodePatchPlan
	CodePlanPath          string
	WorkDir               string
	CodeDeltaStrategy     string
	JSONOut               bool
}

func publishAndroidCodePatch(options publishAndroidCodePatchOptions) error {
	if options.CodePlan == nil {
		return errors.New("android code patch plan is required")
	}
	if !options.CodePlan.Ready {
		return fmt.Errorf("android code patch plan is blocked: %s", summarizeCodePatchBlockers(options.CodePlan.Blockers))
	}

	preflightPatchID := strings.TrimSpace(options.PatchID)
	if preflightPatchID == "" {
		preflightPatchID = "preflight"
	}
	if _, _, err := androidpatch.BuildCodePatchBundle(androidpatch.CodePatchBuildOptions{
		CodePlanPath:      options.CodePlanPath,
		PatchID:           preflightPatchID,
		PatchNumber:       1,
		ReleaseID:         options.ReleaseID,
		OutputPath:        filepath.Join(options.WorkDir, "preflight-code.zip"),
		CodeDeltaStrategy: options.CodeDeltaStrategy,
	}); err != nil {
		return err
	}

	resolvedPatchID := strings.TrimSpace(options.PatchID)
	if resolvedPatchID == "" {
		resolvedPatchID = defaultPatchID(options.AppID, options.Channel)
	}
	patchEndpointBase := options.APIBase + "/v1/patches/" + resolvedPatchID
	patch, err := postJSONDecode[domain.Patch](options.APIBase+"/v1/patches", domain.CreatePatchRequest{
		ID:                   resolvedPatchID,
		AppID:                options.AppID,
		ReleaseID:            options.ReleaseID,
		RuntimeID:            options.RuntimeID,
		Channel:              options.Channel,
		Track:                options.Track,
		Kind:                 domain.PatchKindExperimentalNativeAOT,
		ActivationMode:       options.ActivationMode,
		ManifestURL:          patchEndpointBase + "/manifest",
		BundleURL:            patchEndpointBase + "/bundle",
		RolloutPercent:       options.RolloutPercent,
		ManifestSigningKeyID: options.ManifestSigningKeyID,
	})
	if err != nil {
		return err
	}

	report, bundleBytes, err := androidpatch.BuildCodePatchBundle(androidpatch.CodePatchBuildOptions{
		CodePlanPath:      options.CodePlanPath,
		PatchID:           patch.ID,
		PatchNumber:       uint32(patch.Number),
		ReleaseID:         patch.ReleaseID,
		OutputPath:        filepath.Join(options.WorkDir, patch.ID+"-code.zip"),
		CodeDeltaStrategy: options.CodeDeltaStrategy,
	})
	if err != nil {
		return err
	}
	if err := uploadPatchBundleBytes(patch.BundleURL, bundleBytes); err != nil {
		return err
	}

	summary := patchAndroidSummary{
		ProjectDir:        options.ProjectDir,
		BaseArtifact:      filepath.Clean(options.BaseArtifactPath),
		CandidateArtifact: filepath.Clean(options.CandidateArtifactPath),
		Kind:              domain.PatchKindExperimentalNativeAOT,
		CodePlan:          options.CodePlan,
		CodeReport:        report,
		Patch:             patch,
	}
	if options.JSONOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Published Android code patch %s\n", patch.ID)
	fmt.Fprintf(os.Stdout, "patch_number: %d\n", patch.Number)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", patch.AppID)
	fmt.Fprintf(os.Stdout, "release_id: %s\n", patch.ReleaseID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", patch.RuntimeID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", patch.Channel)
	fmt.Fprintf(os.Stdout, "track: %s\n", domain.NormalizePatchTrack(patch.Track))
	fmt.Fprintf(os.Stdout, "kind: %s\n", patch.Kind)
	fmt.Fprintf(os.Stdout, "rollout_percent: %d\n", patch.RolloutPercent)
	fmt.Fprintf(os.Stdout, "base_artifact: %s\n", summary.BaseArtifact)
	fmt.Fprintf(os.Stdout, "candidate_artifact: %s\n", summary.CandidateArtifact)
	fmt.Fprintf(os.Stdout, "code_payloads: %d\n", len(report.Payloads))
	if report.ArtifactSizeBytes != nil {
		fmt.Fprintf(os.Stdout, "code_artifact_bytes: %d\n", *report.ArtifactSizeBytes)
	}
	if report.BundleSizeBytes != nil {
		fmt.Fprintf(os.Stdout, "bundle_bytes: %d\n", *report.BundleSizeBytes)
	}
	printAndroidPatchNextSteps(patch.ID)
	return nil
}

func printAndroidPatchNextSteps(patchID string) {
	patchID = strings.TrimSpace(patchID)
	if patchID == "" {
		return
	}
	fmt.Fprintln(os.Stdout, "activation: clients download the patch in the background and activate it on the next clean app start.")
	fmt.Fprintf(os.Stdout, "next: verify health with `soroq patch health --patch-id %s`; adjust rollout with `soroq patch rollout --patch-id %s --percent <0-100>`.\n", patchID, patchID)
}

func selectAndroidReleaseForPatch(
	apiBase string,
	appID string,
	channel string,
	releaseVersion string,
) (domain.Release, error) {
	query := url.Values{}
	query.Set("app_id", strings.TrimSpace(appID))
	releases, err := getJSONDecode[[]domain.Release](
		strings.TrimRight(apiBase, "/") + "/v1/releases?" + query.Encode(),
	)
	if err != nil {
		return domain.Release{}, err
	}
	releaseVersion = strings.TrimSpace(releaseVersion)
	if releaseVersion == "" {
		return domain.Release{}, errors.New("--release-version is required")
	}

	matches := make([]domain.Release, 0, len(releases))
	for _, release := range releases {
		if release.AppID != appID || release.Platform != "android" || release.Channel != channel {
			continue
		}
		if releaseVersion != "latest" && release.Version != releaseVersion {
			continue
		}
		matches = append(matches, release)
	}
	if len(matches) == 0 {
		if releaseVersion == "latest" {
			return domain.Release{}, fmt.Errorf("no Android release found for app %q on channel %q", appID, channel)
		}
		return domain.Release{}, fmt.Errorf("no Android release version %q found for app %q on channel %q", releaseVersion, appID, channel)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
			return matches[i].ID < matches[j].ID
		}
		return matches[i].CreatedAt.Before(matches[j].CreatedAt)
	})
	return matches[len(matches)-1], nil
}

func resolveCandidateArtifactForReleaseSelection(
	projectDir string,
	candidateArtifactPath string,
	buildBeforeDiscover bool,
	buildArtifactType string,
	toolchainVersion string,
	flutterBuildArgs []string,
) (string, error) {
	candidateArtifactPath = strings.TrimSpace(candidateArtifactPath)
	if len(flutterBuildArgs) > 0 && (candidateArtifactPath != "" || !buildBeforeDiscover) {
		return "", errors.New("Flutter build passthrough args require automatic build; omit --candidate-artifact and keep --build=true")
	}
	if candidateArtifactPath == "" && buildBeforeDiscover {
		if err := runFlutterAndroidReleaseBuild(projectDir, buildArtifactType, toolchainVersion, flutterBuildArgs); err != nil {
			return "", err
		}
	}
	if candidateArtifactPath != "" {
		return candidateArtifactPath, nil
	}
	artifactPath, err := discoverDefaultAndroidArtifact(projectDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("no candidate Android artifact found to infer the release version; run `soroq patch android` with a working Flutter toolchain, pass --candidate-artifact, or pass --release-version")
	}
	if err != nil {
		return "", err
	}
	return artifactPath, nil
}

func downloadReleaseArtifact(apiBase string, releaseID string, projectDir string) (string, error) {
	req, err := http.NewRequest(
		http.MethodGet,
		strings.TrimRight(apiBase, "/")+"/v1/releases/"+url.PathEscape(releaseID)+"/artifact",
		nil,
	)
	if err != nil {
		return "", err
	}
	if err := applyOperatorHeaders(req); err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return "", err
		}
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return "", fmt.Errorf("request failed: %s", message)
	}

	fileName := releaseArtifactFileName(resp.Header.Get("Content-Disposition"))
	if strings.TrimSpace(fileName) == "" {
		fileName = "android-release.aab"
	}
	targetPath := projectReleaseArtifactPath(projectDir, releaseID, fileName)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return "", err
	}
	tmpPath := targetPath + ".tmp"
	target, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(target, hasher), resp.Body)
	closeErr := target.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", closeErr
	}
	if resp.ContentLength >= 0 && written != resp.ContentLength {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("hosted release artifact size mismatch: expected %d bytes, got %d bytes", resp.ContentLength, written)
	}
	if expectedSize := strings.TrimSpace(resp.Header.Get("X-Soroq-Artifact-Size-Bytes")); expectedSize != "" && expectedSize != fmt.Sprintf("%d", written) {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("hosted release artifact size mismatch: expected %s bytes, got %d bytes", expectedSize, written)
	}
	if expectedSHA := strings.TrimSpace(resp.Header.Get("X-Soroq-Artifact-SHA256")); expectedSHA != "" {
		actualSHA := fmt.Sprintf("%x", hasher.Sum(nil))
		if !strings.EqualFold(actualSHA, expectedSHA) {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("hosted release artifact sha256 mismatch: expected %s, got %s", expectedSHA, actualSHA)
		}
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return targetPath, nil
}

func releaseArtifactFileName(contentDisposition string) string {
	_, params, err := mime.ParseMediaType(contentDisposition)
	if err != nil {
		return ""
	}
	fileName := filepath.Base(filepath.Clean(strings.TrimSpace(params["filename"])))
	if fileName == "" || fileName == "." || fileName == string(filepath.Separator) {
		return ""
	}
	return fileName
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func normalizeAndroidPatchKindFlag(raw string) (domain.PatchKind, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return "", nil
	case "asset":
		return domain.PatchKindAsset, nil
	case "code", "dart", "dart_code", "aot", "native_aot", "experimental_native_aot":
		return domain.PatchKindExperimentalNativeAOT, nil
	default:
		return "", fmt.Errorf("--kind must be auto, asset, code, or experimental_native_aot; got %q", raw)
	}
}

func summarizeCodePatchBlockers(blockers []androidpatch.CodePatchBlocker) string {
	if len(blockers) == 0 {
		return "unknown blocker"
	}
	parts := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		if strings.TrimSpace(blocker.Path) != "" {
			parts = append(parts, fmt.Sprintf("%s(%s): %s", blocker.ID, blocker.Path, blocker.Detail))
		} else {
			parts = append(parts, fmt.Sprintf("%s: %s", blocker.ID, blocker.Detail))
		}
	}
	return strings.Join(parts, "; ")
}

func summarizeAssetPatchBlockers(blockers []androidpatch.Blocker) string {
	if len(blockers) == 0 {
		return "unknown blocker"
	}
	parts := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		if blocker.ID == "native_libraries" {
			parts = append(parts, "native_libraries: native .so files changed; asset patches can only change Flutter asset/config files")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", blocker.ID, blocker.Detail))
	}
	return strings.Join(parts, "; ")
}

func runPatchIOS(args []string) error {
	fs := flag.NewFlagSet("patch ios", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	configFilePath := fs.String("config-file", "", "path to a JSON config file to deliver OTA")
	releaseID := fs.String("release-id", "", "existing iOS release id that this config patch targets")
	releaseVersion := fs.String("release-version", "", "iOS release version to patch; use latest for the newest iOS release in the channel")
	patchID := fs.String("patch-id", "", "patch id override")
	channel := fs.String("channel", "", "channel override (defaults to soroq.yaml or the last iOS release)")
	track := fs.String("track", "", "patch track, such as stable, staging, or beta")
	rollout := fs.Int("rollout", 100, "rollout percentage")
	activation := fs.String("activation", string(domain.ActivationDownloadOnly), "activation mode")
	manifestKeyID := fs.String("manifest-key-id", "", "optional server-side manifest signing key id for this patch")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq patch ios --config-file config.json [--project-dir .] [--api https://api.soroq.dev] [--release-id ios-release] [--release-version latest|1.2.3+45] [--patch-id my-ios-config-patch] [--channel stable] [--track stable|staging|beta] [--rollout 100] [--activation download_only] [--manifest-key-id prod-primary] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(*configFilePath) == "" {
		return errors.New("--config-file is required")
	}
	if strings.TrimSpace(*releaseID) != "" && strings.TrimSpace(*releaseVersion) != "" {
		return errors.New("use either --release-id or --release-version, not both")
	}

	status, err := inspectProject(*projectDir)
	if err != nil {
		return err
	}
	state, err := loadProjectCLIState(status.ProjectDir)
	if err != nil {
		return err
	}
	lastRelease := state.LastIOSRelease
	resolvedAPIBase := strings.TrimRight(*apiBase, "/")
	if !flagWasSet(fs, "api") && lastRelease != nil && strings.TrimSpace(lastRelease.APIBase) != "" {
		resolvedAPIBase = strings.TrimRight(lastRelease.APIBase, "/")
	}
	channelOverride := *channel
	if !flagWasSet(fs, "channel") && lastRelease != nil && strings.TrimSpace(lastRelease.Channel) != "" {
		channelOverride = lastRelease.Channel
	}
	projectConfig, err := resolveIOSReleaseProjectConfig(status, channelOverride)
	if err != nil {
		return err
	}

	resolvedReleaseID := strings.TrimSpace(*releaseID)
	if resolvedReleaseID == "" {
		switch {
		case strings.TrimSpace(*releaseVersion) != "":
			selectedRelease, err := selectIOSReleaseForPatch(
				resolvedAPIBase,
				projectConfig.AppID,
				projectConfig.Channel,
				strings.TrimSpace(*releaseVersion),
			)
			if err != nil {
				return err
			}
			resolvedReleaseID = selectedRelease.ID
		case lastRelease != nil &&
			lastRelease.AppID == projectConfig.AppID &&
			lastRelease.Channel == projectConfig.Channel &&
			strings.TrimSpace(lastRelease.ReleaseID) != "":
			resolvedReleaseID = lastRelease.ReleaseID
		default:
			selectedRelease, err := selectIOSReleaseForPatch(
				resolvedAPIBase,
				projectConfig.AppID,
				projectConfig.Channel,
				"latest",
			)
			if err != nil {
				return err
			}
			resolvedReleaseID = selectedRelease.ID
		}
	}

	targetRelease, err := getJSONDecode[domain.Release](resolvedAPIBase + "/v1/releases/" + url.PathEscape(resolvedReleaseID))
	if err != nil {
		return err
	}
	if targetRelease.Platform != "ios" {
		return fmt.Errorf("release %s is platform %q, not ios", targetRelease.ID, targetRelease.Platform)
	}

	forwardedArgs := []string{
		"--project-dir", status.ProjectDir,
		"--api", resolvedAPIBase,
		"--config-file", *configFilePath,
		"--release-id", targetRelease.ID,
		"--channel", projectConfig.Channel,
	}
	if flagWasSet(fs, "patch-id") {
		forwardedArgs = append(forwardedArgs, "--patch-id", *patchID)
	}
	if flagWasSet(fs, "track") {
		forwardedArgs = append(forwardedArgs, "--track", *track)
	}
	if flagWasSet(fs, "rollout") {
		forwardedArgs = append(forwardedArgs, "--rollout", fmt.Sprintf("%d", *rollout))
	}
	if flagWasSet(fs, "activation") {
		forwardedArgs = append(forwardedArgs, "--activation", *activation)
	}
	if flagWasSet(fs, "manifest-key-id") {
		forwardedArgs = append(forwardedArgs, "--manifest-key-id", *manifestKeyID)
	}
	if *jsonOut {
		forwardedArgs = append(forwardedArgs, "--json")
	}

	if err := runPatchConfig(forwardedArgs); err != nil {
		return err
	}
	if !*jsonOut {
		fmt.Fprintln(os.Stdout, "ios_support: config_ota_only")
		// Be accurate per-patch: the SAME command can carry dart_eval bytecode inside the
		// signed config, so a blanket "config/data only, no executable code" note would be
		// false when code_evc_base64 is present.
		carriesCode := false
		if data, readErr := os.ReadFile(*configFilePath); readErr == nil {
			carriesCode = strings.Contains(string(data), "code_evc_base64")
		}
		fmt.Fprintln(os.Stdout, iosPatchLaneNote(carriesCode))
		verifyPatchID := strings.TrimSpace(*patchID)
		if verifyPatchID == "" {
			verifyPatchID = "<patch-id printed above>"
		}
		fmt.Fprintf(os.Stdout, "verify: `soroq patch status --patch-id %s` (or `soroq patch health --patch-id %s`), or open test_url below on device.\n", verifyPatchID, verifyPatchID)
		fmt.Fprintf(os.Stdout, "test_url: %s\n", iosConfigHarnessDeepLink("check", resolvedAPIBase, targetRelease, "ios-config-test"))
		fmt.Fprintf(os.Stdout, "reset_url: %s\n", iosConfigHarnessDeepLink("reset", resolvedAPIBase, targetRelease, "ios-config-test"))
	}
	return nil
}

func iosConfigHarnessDeepLink(action string, apiBase string, release domain.Release, clientID string) string {
	query := url.Values{}
	query.Set("api_base", strings.TrimRight(apiBase, "/"))
	query.Set("app_id", release.AppID)
	query.Set("runtime_id", release.RuntimeID)
	query.Set("release_id", release.ID)
	query.Set("channel", release.Channel)
	query.Set("client_id", strings.TrimSpace(clientID))
	return "soroq-ios-config://" + url.PathEscape(strings.TrimSpace(action)) + "?" + query.Encode()
}

func selectIOSReleaseForPatch(
	apiBase string,
	appID string,
	channel string,
	releaseVersion string,
) (domain.Release, error) {
	query := url.Values{}
	query.Set("app_id", strings.TrimSpace(appID))
	releases, err := getJSONDecode[[]domain.Release](
		strings.TrimRight(apiBase, "/") + "/v1/releases?" + query.Encode(),
	)
	if err != nil {
		return domain.Release{}, err
	}
	releaseVersion = strings.TrimSpace(releaseVersion)
	if releaseVersion == "" {
		return domain.Release{}, errors.New("--release-version is required")
	}

	matches := make([]domain.Release, 0, len(releases))
	for _, release := range releases {
		if release.AppID != appID || release.Platform != "ios" || release.Channel != channel {
			continue
		}
		if releaseVersion != "latest" && release.Version != releaseVersion {
			continue
		}
		matches = append(matches, release)
	}
	if len(matches) == 0 {
		if releaseVersion == "latest" {
			return domain.Release{}, fmt.Errorf("no iOS release found for app %q on channel %q", appID, channel)
		}
		return domain.Release{}, fmt.Errorf("no iOS release version %q found for app %q on channel %q", releaseVersion, appID, channel)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
			return matches[i].ID < matches[j].ID
		}
		return matches[i].CreatedAt.Before(matches[j].CreatedAt)
	})
	return matches[len(matches)-1], nil
}

func runPatchConfig(args []string) error {
	fs := flag.NewFlagSet("patch config", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	configFilePath := fs.String("config-file", "", "path to a JSON config file to deliver OTA")
	releaseID := fs.String("release-id", "", "existing release id that this config patch targets")
	patchID := fs.String("patch-id", "", "patch id override")
	channel := fs.String("channel", "", "channel override (defaults to soroq.yaml)")
	track := fs.String("track", "", "patch track, such as stable, staging, or beta")
	rollout := fs.Int("rollout", 100, "rollout percentage")
	activation := fs.String("activation", string(domain.ActivationDownloadOnly), "activation mode")
	manifestKeyID := fs.String("manifest-key-id", "", "optional server-side manifest signing key id for this patch")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq patch config --config-file config.json --release-id my-release [--project-dir .] [--api https://api.soroq.dev] [--patch-id my-config-patch] [--channel stable] [--track stable|staging|beta] [--rollout 100] [--activation download_only] [--manifest-key-id prod-primary] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(*configFilePath) == "" || strings.TrimSpace(*releaseID) == "" {
		return errors.New("--config-file and --release-id are required")
	}
	resolvedTrack, resolvedRollout, err := resolvePatchTrackAndRollout(*track, *rollout, flagWasSet(fs, "rollout"))
	if err != nil {
		return err
	}

	configBytes, configSHA, err := readConfigPatchFile(*configFilePath)
	if err != nil {
		return err
	}

	status, err := inspectProject(*projectDir)
	if err != nil {
		return err
	}
	projectConfig, err := resolveProjectCommandConfig(status, *channel)
	if err != nil {
		return err
	}

	targetRelease, err := getJSONDecode[domain.Release](strings.TrimRight(*apiBase, "/") + "/v1/releases/" + url.PathEscape(strings.TrimSpace(*releaseID)))
	if err != nil {
		return err
	}
	if targetRelease.AppID != projectConfig.AppID {
		return fmt.Errorf("release app_id %q does not match soroq.yaml app_id %q", targetRelease.AppID, projectConfig.AppID)
	}
	if targetRelease.Channel != projectConfig.Channel {
		return fmt.Errorf("release channel %q does not match requested channel %q", targetRelease.Channel, projectConfig.Channel)
	}
	if targetRelease.RuntimeID == "" {
		return fmt.Errorf("release %s is missing runtime_id", targetRelease.ID)
	}

	resolvedPatchID := strings.TrimSpace(*patchID)
	if resolvedPatchID == "" {
		resolvedPatchID = defaultPatchID(projectConfig.AppID, projectConfig.Channel)
	}
	patchEndpointBase := strings.TrimRight(*apiBase, "/") + "/v1/patches/" + resolvedPatchID
	patch, err := postJSONDecode[domain.Patch](strings.TrimRight(*apiBase, "/")+"/v1/patches", domain.CreatePatchRequest{
		ID:                   resolvedPatchID,
		AppID:                projectConfig.AppID,
		ReleaseID:            targetRelease.ID,
		RuntimeID:            targetRelease.RuntimeID,
		Channel:              projectConfig.Channel,
		Track:                resolvedTrack,
		Kind:                 domain.PatchKindConfig,
		ActivationMode:       domain.ActivationMode(strings.TrimSpace(*activation)),
		ManifestURL:          patchEndpointBase + "/manifest",
		BundleURL:            patchEndpointBase + "/bundle",
		RolloutPercent:       resolvedRollout,
		ManifestSigningKeyID: strings.TrimSpace(*manifestKeyID),
	})
	if err != nil {
		return err
	}

	bundleBytes, err := buildSimplePatchBundle(domain.PatchManifest{
		PatchID:        patch.ID,
		PatchNumber:    patch.Number,
		RuntimeID:      patch.RuntimeID,
		ReleaseID:      patch.ReleaseID,
		Channel:        patch.Channel,
		Kind:           domain.PatchKindConfig,
		ActivationMode: patch.ActivationMode,
		Artifact: domain.PatchArtifact{
			URL:       "file://soroq/config.json",
			SHA256:    configSHA,
			SizeBytes: uint64(len(configBytes)),
		},
		Signature: nil,
	}, configBytes)
	if err != nil {
		return err
	}
	if err := uploadPatchBundleBytes(patch.BundleURL, bundleBytes); err != nil {
		return err
	}

	summary := patchConfigSummary{
		ProjectDir:    status.ProjectDir,
		ConfigFile:    filepath.Clean(*configFilePath),
		ConfigSHA256:  configSHA,
		ConfigBytes:   len(configBytes),
		TargetRelease: targetRelease,
		Patch:         patch,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Published config patch %s\n", patch.ID)
	fmt.Fprintf(os.Stdout, "patch_number: %d\n", patch.Number)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", patch.AppID)
	fmt.Fprintf(os.Stdout, "release_id: %s\n", patch.ReleaseID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", patch.RuntimeID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", patch.Channel)
	fmt.Fprintf(os.Stdout, "track: %s\n", domain.NormalizePatchTrack(patch.Track))
	fmt.Fprintf(os.Stdout, "activation_mode: %s\n", patch.ActivationMode)
	fmt.Fprintf(os.Stdout, "rollout_percent: %d\n", patch.RolloutPercent)
	fmt.Fprintf(os.Stdout, "config_file: %s\n", summary.ConfigFile)
	fmt.Fprintf(os.Stdout, "config_sha256: %s\n", summary.ConfigSHA256)
	return nil
}

func defaultPatchID(appID, channel string) string {
	return slugifyReleaseID(fmt.Sprintf("%s-%s-patch-%d", appID, channel, time.Now().Unix()))
}

func readConfigPatchFile(path string) ([]byte, string, error) {
	configBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var payload any
	if err := json.Unmarshal(configBytes, &payload); err != nil {
		return nil, "", fmt.Errorf("config file must be valid JSON: %w", err)
	}
	if _, ok := payload.(map[string]any); !ok {
		return nil, "", errors.New("config file must be a JSON object")
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, configBytes); err != nil {
		return nil, "", err
	}
	normalizedBytes := append(compact.Bytes(), '\n')
	sum := sha256.Sum256(normalizedBytes)
	return normalizedBytes, fmt.Sprintf("%x", sum[:]), nil
}

func buildSimplePatchBundle(manifest domain.PatchManifest, artifactBytes []byte) ([]byte, error) {
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	for _, entry := range []struct {
		name  string
		bytes []byte
	}{
		{name: "manifest.json", bytes: manifestBytes},
		{name: "artifact.bin", bytes: artifactBytes},
	} {
		file, err := writer.Create(entry.name)
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		if _, err := file.Write(entry.bytes); err != nil {
			_ = writer.Close()
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func uploadPatchBundleBytes(url string, bundleBytes []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bundleBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/zip")
	if err := applyOperatorHeaders(req); err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(respBody))
		if message == "" {
			message = resp.Status
		}
		return fmt.Errorf("request failed: %s", message)
	}
	return nil
}

func writeJSONFile(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(path, encoded, 0o644)
}
