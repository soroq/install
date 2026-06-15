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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	Patches   []domain.Patch `json:"patches"`
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
	case "health":
		return runPatchHealth(args[1:])
	case "list":
		return runPatchList(args[1:])
	case "status":
		return runPatchStatus(args[1:])
	case "-h", "--help", "help":
		patchUsage()
		return nil
	default:
		printUnknownSubcommand(os.Stderr, "patch", args[0], []string{"android", "config", "health", "list", "status"})
		return errAlreadyPrinted
	}
}

func patchUsage() {
	printCommandUsage(os.Stdout,
		"Soroq Patches",
		"Publish and inspect hosted OTA patches.",
		"soroq patch <target> [flags]",
		[]usageSection{{
			Title: "Targets",
			Rows: []usageRow{
				{Name: "android", Description: "Publish a hosted Android patch from base and candidate artifacts."},
				{Name: "config", Description: "Publish a hosted JSON config patch for an existing release."},
				{Name: "health", Description: "Inspect patch install health and rollback state."},
				{Name: "list", Description: "List patches in the control plane."},
				{Name: "status", Description: "Inspect a patch record in the control plane."},
			},
		}},
		[]string{
			"soroq patch android --release-id rel_123 --artifact patch.zip",
			"soroq patch list --app-id com.example.app",
		},
	)
}

func runPatchList(args []string) error {
	fs := flag.NewFlagSet("patch list", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	appID := fs.String("app-id", "", "optional app id filter")
	releaseID := fs.String("release-id", "", "optional release id filter")
	runtimeID := fs.String("runtime-id", "", "optional runtime id filter")
	channel := fs.String("channel", "", "optional channel filter")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq patch list [--api https://soroq-control-plane.fly.dev] [--app-id com.example.app] [--release-id release-123] [--runtime-id runtime-123] [--channel stable] [--json]`)
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
		Patches:   patches,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Soroq patches: %d\n", len(patches))
	for _, patch := range patches {
		fmt.Fprintf(os.Stdout, "- %s\t#%d\t%s\t%s\t%s\trolled_back=%s\n", patch.ID, patch.Number, patch.AppID, patch.ReleaseID, patch.Channel, yesNo(patch.RolledBack))
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
		fmt.Fprintln(os.Stdout, `usage: soroq patch status --patch-id patch-123 [--api https://soroq-control-plane.fly.dev] [--json]`)
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
		fmt.Fprintln(os.Stdout, `usage: soroq patch health --patch-id patch-123 [--api https://soroq-control-plane.fly.dev] [--json]`)
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
	releaseID := fs.String("release-id", "", "existing release id that this patch targets")
	patchID := fs.String("patch-id", "", "patch id override")
	channel := fs.String("channel", "", "channel override (defaults to soroq.yaml)")
	rollout := fs.Int("rollout", 100, "rollout percentage")
	activation := fs.String("activation", string(domain.ActivationNextColdStart), "activation mode")
	manifestKeyID := fs.String("manifest-key-id", "", "optional server-side manifest signing key id for this patch")
	patchKindRaw := fs.String("kind", "auto", "patch kind: auto, asset, code, or experimental_native_aot")
	codeDeltaStrategy := fs.String("code-delta-strategy", "default", "code delta strategy for code patches: default or v15")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	allowEmpty := fs.Bool("allow-empty", false, "publish an empty patch when no overlay asset changes are detected")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq patch android [--base-artifact .soroq/releases/my-release/app-release.aab] [--candidate-artifact build/app/outputs/bundle/release/app-release.aab] [--release-id my-release] [--build=false] [--artifact-type aab|apk] [--project-dir .] [--api https://soroq-control-plane.fly.dev] [--patch-id my-patch] [--channel stable] [--kind auto|asset|code] [--rollout 100] [--activation next_cold_start] [--manifest-key-id prod-primary] [--allow-empty] [--json] [-- <flutter build flags>]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	flutterBuildArgs := fs.Args()
	if *rollout < 1 || *rollout > 100 {
		return errors.New("--rollout must be between 1 and 100")
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
	resolvedBaseArtifactPath := strings.TrimSpace(*baseArtifactPath)
	if resolvedBaseArtifactPath == "" && lastRelease != nil {
		resolvedBaseArtifactPath = lastRelease.ArtifactPath
	}
	if resolvedBaseArtifactPath == "" {
		return errors.New("--base-artifact is required unless `soroq release android` has already recorded a release")
	}
	resolvedReleaseID := strings.TrimSpace(*releaseID)
	if resolvedReleaseID == "" && lastRelease != nil {
		resolvedReleaseID = lastRelease.ReleaseID
	}
	if resolvedReleaseID == "" {
		return errors.New("--release-id is required unless `soroq release android` has already recorded a release")
	}
	channelOverride := *channel
	if !flagWasSet(fs, "channel") && lastRelease != nil && strings.TrimSpace(lastRelease.Channel) != "" {
		channelOverride = lastRelease.Channel
	}
	projectConfig, err := resolveProjectCommandConfig(status, channelOverride)
	if err != nil {
		return err
	}

	workDir, err := os.MkdirTemp("", "soroq-patch-android-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

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
	resolvedCandidateArtifactPath := strings.TrimSpace(*candidateArtifactPath)
	candidateFromLastRelease := false
	if len(flutterBuildArgs) > 0 && (resolvedCandidateArtifactPath != "" || !*buildBeforeDiscover) {
		return errors.New("Flutter build passthrough args require automatic build; omit --candidate-artifact and keep --build=true")
	}
	if resolvedCandidateArtifactPath == "" && *buildBeforeDiscover {
		if err := runFlutterAndroidReleaseBuild(status.ProjectDir, *buildArtifactType, flutterBuildArgs); err != nil {
			return err
		}
	}
	if resolvedCandidateArtifactPath == "" {
		resolvedCandidateArtifactPath, err = discoverCompatibleCandidateArtifact(status.ProjectDir, baseSnapshot)
		if errors.Is(err, os.ErrNotExist) {
			if !*allowEmpty {
				return errors.New("no compatible candidate Android artifact found; build a candidate APK/AAB, pass --candidate-artifact, or use --allow-empty for an explicit empty patch")
			}
			resolvedCandidateArtifactPath = resolvedBaseArtifactPath
			candidateFromLastRelease = true
		} else if err != nil {
			return err
		}
	}
	resolvedManifestKeyID := strings.TrimSpace(*manifestKeyID)
	if resolvedManifestKeyID == "" && lastRelease != nil {
		resolvedManifestKeyID = strings.TrimSpace(lastRelease.ManifestSigningKeyID)
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
			RolloutPercent:        *rollout,
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
		Kind:                 domain.PatchKindAsset,
		ActivationMode:       domain.ActivationMode(strings.TrimSpace(*activation)),
		ManifestURL:          patchEndpointBase + "/manifest",
		BundleURL:            patchEndpointBase + "/bundle",
		RolloutPercent:       *rollout,
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
	fmt.Fprintf(os.Stdout, "kind: %s\n", patch.Kind)
	fmt.Fprintf(os.Stdout, "rollout_percent: %d\n", patch.RolloutPercent)
	fmt.Fprintf(os.Stdout, "base_artifact: %s\n", summary.BaseArtifact)
	fmt.Fprintf(os.Stdout, "candidate_artifact: %s\n", summary.CandidateArtifact)
	fmt.Fprintf(os.Stdout, "overlay_files: %d\n", len(report.OverlayEntries))
	if resolvedAllowEmpty && len(report.OverlayEntries) == 0 {
		fmt.Fprintln(os.Stdout, "empty_patch: yes")
	}
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
	return nil
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
		parts = append(parts, fmt.Sprintf("%s: %s", blocker.ID, blocker.Detail))
	}
	return strings.Join(parts, "; ")
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
	rollout := fs.Int("rollout", 100, "rollout percentage")
	activation := fs.String("activation", string(domain.ActivationDownloadOnly), "activation mode")
	manifestKeyID := fs.String("manifest-key-id", "", "optional server-side manifest signing key id for this patch")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq patch config --config-file config.json --release-id my-release [--project-dir .] [--api https://soroq-control-plane.fly.dev] [--patch-id my-config-patch] [--channel stable] [--rollout 100] [--activation download_only] [--manifest-key-id prod-primary] [--json]`)
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
	if *rollout < 1 || *rollout > 100 {
		return errors.New("--rollout must be between 1 and 100")
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
		Kind:                 domain.PatchKindConfig,
		ActivationMode:       domain.ActivationMode(strings.TrimSpace(*activation)),
		ManifestURL:          patchEndpointBase + "/manifest",
		BundleURL:            patchEndpointBase + "/bundle",
		RolloutPercent:       *rollout,
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
