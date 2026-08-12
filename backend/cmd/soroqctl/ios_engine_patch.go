package main

// iOS ENGINE lane (Phase 9 / T009 Slice C). DISTINCT from the config_ota_only iOS lane in
// ios_release_patch.go: this hot-patches DART CODE via the soroq interpreter-in-engine
// (dart2bytecode bytecode loaded by the patched engine + redirected over the AOT function),
// grounded in the 2026-06-21 device-proven flow (docs/goals/ios-engine-hardlane/notes/phase9-cli.md).
//
// Scope/claims boundary (T045 follow-up Judge): NO arbitrary-Dart / parity / App-Store /
// production-readiness claim. The currently-proven optimized-PROFILE engine is an EXPERIMENTAL
// proof tier only — never a production/App-Store release engine. Never silently falls back to
// config OTA. Signing reuses --seed-base64 / --manifest-key-id; the private seed is NEVER persisted
// to state/logs/bundles/git (only the key id + public-key fingerprint are recorded).

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"soroq/backend/internal/domain"
	"soroq/backend/internal/serving"
	"soroq/backend/internal/signing"
)

// --- engine bundle (--engine-bundle) immutable descriptor (engine.json) ---

// engineBundleManifest is the immutable engine.json inside an --engine-bundle dir. Every value is
// verified before a release is registered; missing/mismatched/debug/unoptimized engines are refused.
type engineBundleManifest struct {
	Schema           string            `json:"schema"`             // soroq.ios_engine.v1 | soroq.ios_engine.v2
	FlutterCommit    string            `json:"flutter_commit"`     // pinned flutter/flutter sha
	DartRevision     string            `json:"dart_revision"`      // pinned dart sha
	SoroqPatchHashes map[string]string `json:"soroq_patch_hashes"` // patch-file -> sha256
	Arch             string            `json:"arch"`               // arm64
	BuildMode        string            `json:"build_mode"`         // profile | release (NOT debug)
	IsDebug          bool              `json:"is_debug"`           // MUST be false
	Tier             string            `json:"tier"`               // "experimental_profile" | "production"
	// path -> sha256, all relative to the bundle root; each is hashed + compared on verify.
	Artifacts map[string]string `json:"artifacts"` // flutter_framework, dart2bytecode, flutter_compile_interface, gen_snapshot, vm_platform, dartaotruntime
}

const engineBundleSchemaV1 = "soroq.ios_engine.v1"
const engineBundleSchema = "soroq.ios_engine.v2"
const androidEngineBundleSchema = "soroq.android_engine.v1"
const engineLaneBaselineSchemaV1 = "soroq.ios_engine_baseline.v1"
const engineLaneBaselineSchema = "soroq.ios_engine_baseline.v2"
const engineLaneBundleDescriptorSchema = "soroq.ios_engine.v1"

// engineLaneSpec parameterizes verifyEngineBundle per platform: the accepted engine.json schema + the
// minimal required-artifact set. iOS is the proven default; Android (T012) reuses the SAME verify logic
// (re-hash every declared artifact vs engine.json, refuse debug/non-profile-or-release/missing/mismatch)
// with libflutter.so in place of flutter_framework. The iOS spec is byte-identical to the prior behavior.
type engineLaneSpec struct {
	schema            string
	requiredArtifacts []string
	frameworkArtifact string // the primary build artifact whose sha is surfaced as FrameworkSHA
}

var iosEngineLaneSpec = engineLaneSpec{
	schema:            engineBundleSchema,
	requiredArtifacts: requiredEngineArtifacts,
	frameworkArtifact: "flutter_framework",
}

var iosEngineLaneSpecV1 = engineLaneSpec{
	schema:            engineBundleSchemaV1,
	requiredArtifacts: requiredEngineArtifactsV1,
	frameworkArtifact: "flutter_framework",
}

var androidEngineLaneSpec = engineLaneSpec{
	schema:            androidEngineBundleSchema,
	requiredArtifacts: requiredAndroidEngineArtifacts,
	frameworkArtifact: "libflutter.so",
}

// engineLaneSpecForSchema picks the verify spec from the engine.json schema (so install/self-verify can
// verify EITHER an iOS or an Android cached bundle without the caller pre-declaring the platform).
func engineLaneSpecForSchema(schema string) (engineLaneSpec, error) {
	switch strings.TrimSpace(schema) {
	case engineBundleSchemaV1:
		return iosEngineLaneSpecV1, nil
	case engineBundleSchema:
		return iosEngineLaneSpec, nil
	case androidEngineBundleSchema:
		return androidEngineLaneSpec, nil
	default:
		return engineLaneSpec{}, fmt.Errorf("engine bundle schema %q is not recognized (want %q, %q, or %q)", schema, engineBundleSchemaV1, engineBundleSchema, androidEngineBundleSchema)
	}
}

// resolveEngineBundleDir resolves the engine-bundle directory the lane operates on from EITHER a
// hand-specified --engine-bundle path (advanced mode) OR a cached toolchain version installed by
// `soroq toolchain install` (the product-shaped path). Exactly one must be supplied.
//
// A cached toolchain installed by T005 lives at ~/.soroq/toolchains/<version>/ios/ and contains the
// verbatim engine.json + the 5 flat artifacts — i.e. it IS a valid --engine-bundle dir that the
// UNCHANGED verifyEngineBundle accepts. So `--toolchain <version>` simply points the lane at that
// cached bundle, eliminating the hand-path / local repo engine checkout. (The toolchainsRoot helper
// lives in the `soroq` package, which soroqctl cannot import; the 3-line path is duplicated here on
// purpose rather than reaching for a shared package outside this task's allowed files.)
func resolveEngineBundleDir(engineBundle, toolchainVersion string) (string, error) {
	engineBundle = strings.TrimSpace(engineBundle)
	toolchainVersion = strings.TrimSpace(toolchainVersion)
	switch {
	case engineBundle != "" && toolchainVersion != "":
		return "", errors.New("provide either --engine-bundle (advanced: hand-specified bundle) or --toolchain <version> (resolve the cached toolchain), not both")
	case engineBundle != "":
		return engineBundle, nil
	case toolchainVersion != "":
		dir, err := cachedToolchainBundleDir(toolchainVersion)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(dir, "engine.json")); err != nil {
			return "", fmt.Errorf("toolchain %q is not installed (no engine.json under %s); run `soroq toolchain install %s --api <base>` first: %w", toolchainVersion, dir, toolchainVersion, err)
		}
		return dir, nil
	default:
		return "", errors.New("provide --toolchain <version> (resolve the cached toolchain installed by `soroq toolchain install`) or --engine-bundle <dir> (advanced)")
	}
}

// cachedToolchainBundleDir returns ~/.soroq/toolchains/<version>/ios (the cached engine-bundle dir).
// It mirrors the cache layout written by `soroq toolchain install` (backend/cmd/soroq/toolchain_*).
func cachedToolchainBundleDir(version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" || strings.Contains(version, "/") || strings.Contains(version, "..") {
		return "", fmt.Errorf("invalid toolchain version %q", version)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".soroq", "toolchains", version, "ios"), nil
}

// v1 is the device-proven R3 bundle and remains installable/usable for legacy
// pure-Dart patches. v2 adds the version-matched Flutter compile interface.
var requiredEngineArtifactsV1 = []string{
	"flutter_framework", "dart2bytecode", "gen_snapshot", "vm_platform", "dartaotruntime",
}

// requiredEngineArtifacts must all be present + hash-matched for a usable v2 iOS engine bundle.
// flutter_compile_interface is the version-matched CFE API summary that lets a
// patch import Flutter's public foundation/widgets/material/cupertino barrels
// without recompiling framework source against a tree-shaken app.dill.
var requiredEngineArtifacts = []string{
	"flutter_framework", "dart2bytecode", "flutter_compile_interface", "gen_snapshot", "vm_platform", "dartaotruntime",
}

// requiredAndroidEngineArtifacts is the Android-lane minimal hostable set (T012). It MUST equal the
// packer's requiredAndroidArtifacts. libflutter.so (the STRIPPED device runtime lib) replaces iOS's
// flutter_framework; the dart2bytecode/dartaotruntime/vm_platform patch trio mirrors iOS exactly.
// T017 adds flutter_embedding_release.jar: the SOROQ Android embedding jar (setSoroq* FlutterLoader
// overrides) so the --local-engine build links the SOROQ embedding and the on-device staged-patch apply
// honors the asset/kernel override (the missing piece behind the T015 asset_bundle_override_api_missing).
var requiredAndroidEngineArtifacts = []string{
	"libflutter.so", "gen_snapshot", "dart2bytecode", "dartaotruntime", "vm_platform", "flutter_embedding_release.jar",
}

// verifiedEngine is the result of a successful verifyEngineBundle.
type verifiedEngine struct {
	BundleDir    string
	Manifest     engineBundleManifest
	FrameworkSHA string // = artifacts["flutter_framework"]
	ToolPath     func(name string) string
	Experimental bool
}

// verifyEngineBundle parses + verifies the engine bundle at dir. It REFUSES: unrecognized schema, debug
// or non-profile/release build, missing required artifacts, and any artifact whose on-disk sha256 does
// not match engine.json. The required-artifact set + framework artifact are chosen from the engine.json
// schema (iOS v1/v2 OR Android v1), so the SAME refusal logic guards
// both lanes. Returns the verified engine or a precise error (no partial trust).
func verifyEngineBundle(dir string) (*verifiedEngine, error) {
	dir = filepath.Clean(dir)
	jsonPath := filepath.Join(dir, "engine.json")
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("read engine.json: %w", err)
	}
	var m engineBundleManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse engine.json: %w", err)
	}
	spec, err := engineLaneSpecForSchema(m.Schema)
	if err != nil {
		return nil, err
	}
	if m.IsDebug {
		return nil, errors.New("refusing debug engine: is_debug=true (the unopt engine aborts on the vsync DCHECK on device)")
	}
	mode := strings.ToLower(strings.TrimSpace(m.BuildMode))
	if mode != "profile" && mode != "release" {
		return nil, fmt.Errorf("refusing engine build_mode %q (must be profile or release)", m.BuildMode)
	}
	if strings.TrimSpace(m.FlutterCommit) == "" || strings.TrimSpace(m.DartRevision) == "" {
		return nil, errors.New("engine.json missing flutter_commit / dart_revision")
	}
	if len(m.SoroqPatchHashes) == 0 {
		return nil, errors.New("engine.json missing soroq_patch_hashes (the engine must be soroq-patched)")
	}
	for _, name := range spec.requiredArtifacts {
		want, ok := m.Artifacts[name]
		if !ok || strings.TrimSpace(want) == "" {
			return nil, fmt.Errorf("engine.json missing required artifact %q (sha256)", name)
		}
	}
	// hash every declared artifact and compare (refuse mismatch / missing file).
	for rel, want := range m.Artifacts {
		got, err := sha256File(filepath.Join(dir, rel))
		if err != nil {
			return nil, fmt.Errorf("artifact %q: %w", rel, err)
		}
		if !strings.EqualFold(got, strings.TrimSpace(want)) {
			return nil, fmt.Errorf("artifact %q sha256 mismatch: engine.json=%s on-disk=%s", rel, want, got)
		}
	}
	tier := strings.ToLower(strings.TrimSpace(m.Tier))
	experimental := tier != "production"
	ve := &verifiedEngine{
		BundleDir:    dir,
		Manifest:     m,
		FrameworkSHA: m.Artifacts[spec.frameworkArtifact],
		Experimental: experimental,
		ToolPath:     func(name string) string { return filepath.Join(dir, name) },
	}
	return ve, nil
}

// runVerifyEngineBundleCmd is a thin, side-effect-free entry that runs the UNCHANGED verifyEngineBundle
// on a bundle dir and reports pass/fail. It is the verify-only gate the toolchain install (Android
// branch) + the packer --self-verify shell to: no baseline write, no app.dill, no control-plane call —
// exit 0 == the bundle's engine.json identity + per-artifact sha256 re-hash PASSED. Works for either an
// iOS (soroq.ios_engine.v1) or Android (soroq.android_engine.v1) bundle (the schema selects the spec).
func runVerifyEngineBundleCmd(args []string) error {
	fs := flag.NewFlagSet("verify-engine-bundle", flag.ContinueOnError)
	engineBundle := fs.String("engine-bundle", "", "path to the assembled engine bundle dir (engine.json + flat artifacts)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*engineBundle) == "" {
		return errors.New("--engine-bundle <dir> is required")
	}
	ve, err := verifyEngineBundle(strings.TrimSpace(*engineBundle))
	if err != nil {
		return fmt.Errorf("engine bundle rejected: %w", err)
	}
	fmt.Printf("verifyEngineBundle PASSED: schema=%s build_mode=%s tier=%s framework=%s (%d artifacts re-hashed vs engine.json)\n",
		ve.Manifest.Schema, ve.Manifest.BuildMode, tierLabel(ve.Experimental), short(ve.FrameworkSHA), len(ve.Manifest.Artifacts))
	return nil
}

// --- engine-lane signed manifest (matches the device app soroq_ota.dart verifier) ---

// engineLaneManifest is the EXACT JSON the device app parses: top-level bytecodeSha256 + patches[].
type engineLaneManifest struct {
	Version            int               `json:"version"`
	RuntimeID          string            `json:"runtime_id"`
	BytecodeSha256     string            `json:"bytecodeSha256"`
	EntrypointContract string            `json:"entrypointContract,omitempty"`
	Patches            []engineLanePatch `json:"patches"`
}
type engineLanePatch struct {
	Index    int    `json:"index"`
	Bytecode string `json:"bytecode"`
}

// signEngineManifest signs the EXACT manifest bytes with Ed25519 (matching the in-app pinned-key
// verifier). Uses the --seed-base64 custody; returns the signature hex + the public-key fingerprint.
// The seed is NEVER returned/persisted.
func signEngineManifest(manifestBytes []byte, seedBase64 string) (sigHex string, pubFingerprint string, err error) {
	seed, err := decodeSeed(seedBase64)
	if err != nil {
		return "", "", err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	sig := ed25519.Sign(priv, manifestBytes)
	pub := priv.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sig), hex.EncodeToString(sum[:])[:16], nil
}

func decodeSeed(seedBase64 string) ([]byte, error) {
	s := strings.TrimSpace(seedBase64)
	if s == "" {
		return nil, errors.New("--seed-base64 is required (Ed25519 manifest signing seed; never persisted)")
	}
	for _, dec := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding} {
		if b, e := dec.DecodeString(s); e == nil && len(b) == ed25519.SeedSize {
			return b, nil
		}
	}
	return nil, fmt.Errorf("--seed-base64 must decode to %d bytes (Ed25519 seed)", ed25519.SeedSize)
}

// --- dispatch (wired from runRelease/runPatch/runRollback in main/ios_release_patch) ---

func runReleaseIOSEngine(args []string) error {
	fs := flag.NewFlagSet("release ios-engine", flag.ContinueOnError)
	engineBundle := fs.String("engine-bundle", "", "ADVANCED: path to a hand-specified verified soroq iOS engine bundle (immutable engine.json). Prefer --toolchain.")
	toolchainVersion := fs.String("toolchain", "", "resolve the engine bundle from the cached toolchain at ~/.soroq/toolchains/<version>/ios (installed by `soroq toolchain install`); no hand-path / repo checkout")
	appDill := fs.String("app-dill", "", "path to the deployed app.dill that patches compile against")
	patchable := fs.String("patchable-manifest", "", "path to the --soroq_manifest patchable-fn list used to build the app")
	releaseID := fs.String("release-id", "", "engine-lane release id")
	appID := fs.String("app-id", "", "app id")
	out := fs.String("out", "", "path to write the immutable engine-lane baseline json")
	apiBase := fs.String("api", "", "Soroq API base; idempotently ensures the control-plane app+release exist for this engine lane (turnkey setup before patch ios-engine --api)")
	appName := fs.String("app-name", "", "display name when creating the control-plane app (defaults to --app-id)")
	runtimeID := fs.String("runtime-id", "", "engine runtime compatibility id for the control-plane release (required with --api)")
	version := fs.String("version", "", "release version for the control-plane release (required with --api)")
	channel := fs.String("channel", "stable", "release channel for the control-plane release")
	archiveOut := fs.String("archive-out", "", "path to write the Xcode archive/IPA handoff descriptor (records the engine identity + the owner/Xcode-gated build step); does NOT fake a build")
	signingIdentity := fs.String("signing-identity", "", "Apple code-signing identity for the archive/IPA build (e.g. 'Apple Distribution: …'); when present with --archive-out the build step is attempted, else it is GATED with the exact missing input")
	xcodeProject := fs.String("xcode-project", "", "path to the Flutter app's ios/ Runner.xcworkspace for the archive build (required alongside --signing-identity)")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*appDill) == "" || strings.TrimSpace(*releaseID) == "" || strings.TrimSpace(*appID) == "" {
		return errors.New("--app-dill, --release-id and --app-id are required")
	}
	resolvedBundle, err := resolveEngineBundleDir(*engineBundle, *toolchainVersion)
	if err != nil {
		return err
	}
	useAPI := strings.TrimSpace(*apiBase) != ""
	if useAPI && (strings.TrimSpace(*runtimeID) == "" || strings.TrimSpace(*version) == "") {
		return errors.New("--api requires --runtime-id and --version (the control-plane release identity)")
	}
	ve, err := verifyEngineBundle(resolvedBundle)
	if err != nil {
		return fmt.Errorf("engine bundle rejected: %w", err)
	}
	appDillSHA, err := sha256File(*appDill)
	if err != nil {
		return fmt.Errorf("--app-dill: %w", err)
	}
	var patchableSHA string
	if p := strings.TrimSpace(*patchable); p != "" {
		if patchableSHA, err = sha256File(p); err != nil {
			return fmt.Errorf("--patchable-manifest: %w", err)
		}
	}
	baseline := engineLaneBaseline{
		Schema:              engineLaneBaselineSchema,
		EngineBundleSchema:  ve.Manifest.Schema,
		ReleaseID:           strings.TrimSpace(*releaseID),
		AppID:               strings.TrimSpace(*appID),
		FlutterCommit:       ve.Manifest.FlutterCommit,
		DartRevision:        ve.Manifest.DartRevision,
		FrameworkSHA256:     ve.FrameworkSHA,
		Dart2bytecodeSHA:    ve.Manifest.Artifacts["dart2bytecode"],
		CompileInterfaceSHA: ve.Manifest.Artifacts["flutter_compile_interface"],
		GenSnapshotSHA:      ve.Manifest.Artifacts["gen_snapshot"],
		AppDillSHA256:       appDillSHA,
		PatchableSHA256:     patchableSHA,
		Arch:                normalizedDefaultString(ve.Manifest.Arch, "arm64"),
		BuildMode:           ve.Manifest.BuildMode,
		ToolchainVersion:    strings.TrimSpace(*toolchainVersion),
		Experimental:        ve.Experimental,
	}
	if ve.Manifest.Schema == engineBundleSchemaV1 {
		baseline.Schema = engineLaneBaselineSchemaV1
	}
	outPath := strings.TrimSpace(*out)
	if outPath == "" {
		outPath = "soroq-ios-engine-baseline.json"
	}
	// IMMUTABLE: refuse to overwrite a DIFFERENT baseline for the same release id.
	if existing, err := os.ReadFile(outPath); err == nil {
		var prev engineLaneBaseline
		if json.Unmarshal(existing, &prev) == nil && prev.ReleaseID == baseline.ReleaseID && !prev.equals(baseline) {
			return fmt.Errorf("refusing to mutate the immutable baseline for release %q at %s (engine/app.dill changed); use a new --release-id", baseline.ReleaseID, outPath)
		}
	}
	if err := writeJSONOutput(baseline, outPath); err != nil {
		return err
	}
	// Archive/IPA handoff: record the engine identity bound to this release + the handoff structure.
	// The ACTUAL Xcode archive + signing is owner/Xcode-gated; we GATE that step with the exact
	// missing input (signing identity / Xcode / project) and NEVER fake a build.
	var handoff *engineLaneArchiveHandoff
	if strings.TrimSpace(*archiveOut) != "" {
		h, err := writeEngineArchiveHandoff(*archiveOut, resolvedBundle, ve, baseline,
			strings.TrimSpace(*signingIdentity), strings.TrimSpace(*xcodeProject))
		if err != nil {
			return err
		}
		handoff = h
	}
	// --api: idempotently ensure the control-plane app + release exist so the lane is ready for
	// `patch ios-engine --api` without separate create-app/create-release steps.
	if useAPI {
		if err := ensureControlPlaneSetup(*apiBase, baseline.AppID, *appName, baseline.ReleaseID, *runtimeID, *version, *channel); err != nil {
			return err
		}
	}
	if ve.Experimental {
		fmt.Fprintln(os.Stderr, "WARNING: experimental optimized-profile engine tier — NOT a production/App-Store release engine.")
	}
	if *format == "json" {
		return writeJSONOutput(baseline, "")
	}
	fmt.Printf("registered engine-lane baseline release=%s app=%s framework=%s app.dill=%s tier=%s",
		baseline.ReleaseID, baseline.AppID, short(baseline.FrameworkSHA256), short(baseline.AppDillSHA256), tierLabel(ve.Experimental))
	if baseline.ToolchainVersion != "" {
		fmt.Printf(" toolchain=%s", baseline.ToolchainVersion)
	}
	fmt.Println()
	if handoff != nil {
		fmt.Printf("archive handoff written to %s (build_status=%s)\n", strings.TrimSpace(*archiveOut), handoff.BuildStatus)
		if handoff.BuildStatus == "gated" {
			fmt.Fprintf(os.Stderr, "GATED: Xcode archive/IPA build not performed — %s\n", handoff.GatedOn)
		}
	}
	if useAPI {
		// The operator base registers; a DEVICE reads from the device-serve host, which is a different
		// origin on the hosted deployment and 401s if the two are conflated.
		fmt.Printf("control-plane app+release ensured on %s (device kHostBase -> %s)\n",
			strings.TrimRight(*apiBase, "/"),
			serving.EngineChannelURL(*apiBase, url.PathEscape(baseline.AppID), url.PathEscape(normalizedDefaultString(*channel, "stable"))))
	}
	return nil
}

// engineLaneArchiveHandoff is the archive/IPA handoff structure `release ios-engine --archive-out`
// writes. It binds the EXACT engine identity (framework + gen_snapshot + toolchain version + the
// patch-compile target app.dill) to the release and records the owner/Xcode-gated archive build step.
// The actual archive + code-signing is NEVER faked here: when the signing identity / Xcode project is
// absent the build_status is "gated" with the precise missing input.
type engineLaneArchiveHandoff struct {
	Schema              string `json:"schema"` // soroq.ios_engine_archive_handoff.v1
	Kind                string `json:"kind"`   // ios_engine
	ReleaseID           string `json:"release_id"`
	AppID               string `json:"app_id"`
	FlutterCommit       string `json:"flutter_commit"`
	DartRevision        string `json:"dart_revision"`
	FrameworkSHA256     string `json:"framework_sha256"`
	CompileInterfaceSHA string `json:"flutter_compile_interface_sha256"`
	GenSnapshotSHA      string `json:"gen_snapshot_sha256"`
	AppDillSHA256       string `json:"app_dill_sha256"`
	ToolchainVersion    string `json:"toolchain_version,omitempty"`
	EngineBundleDir     string `json:"engine_bundle_dir"`
	BuildMode           string `json:"build_mode"`
	Tier                string `json:"tier"`
	Experimental        bool   `json:"experimental"`
	// The owner/Xcode-gated build. build_status ∈ {gated, ready}. We do NOT execute xcodebuild here.
	BuildStatus      string `json:"build_status"`
	GatedOn          string `json:"gated_on,omitempty"`
	SigningIdentity  string `json:"signing_identity,omitempty"`
	XcodeProject     string `json:"xcode_project,omitempty"`
	BuildCommandHint string `json:"build_command_hint"`
}

// writeEngineArchiveHandoff writes the archive/IPA handoff descriptor and computes its build_status.
// The actual Xcode archive build is owner-gated; this records the handoff and the EXACT missing input
// (signing identity + Xcode project) without ever faking a build.
func writeEngineArchiveHandoff(path, bundleDir string, ve *verifiedEngine, b engineLaneBaseline, signingIdentity, xcodeProject string) (*engineLaneArchiveHandoff, error) {
	h := engineLaneArchiveHandoff{
		Schema:              "soroq.ios_engine_archive_handoff.v1",
		Kind:                "ios_engine",
		ReleaseID:           b.ReleaseID,
		AppID:               b.AppID,
		FlutterCommit:       b.FlutterCommit,
		DartRevision:        b.DartRevision,
		FrameworkSHA256:     b.FrameworkSHA256,
		CompileInterfaceSHA: b.CompileInterfaceSHA,
		GenSnapshotSHA:      b.GenSnapshotSHA,
		AppDillSHA256:       b.AppDillSHA256,
		ToolchainVersion:    b.ToolchainVersion,
		EngineBundleDir:     bundleDir,
		BuildMode:           b.BuildMode,
		Tier:                tierLabel(ve.Experimental),
		Experimental:        ve.Experimental,
		SigningIdentity:     signingIdentity,
		XcodeProject:        xcodeProject,
		BuildCommandHint:    "xcodebuild -workspace <ios/Runner.xcworkspace> -scheme Runner -configuration Release -archivePath <out>.xcarchive archive CODE_SIGN_IDENTITY=<signing-identity>; then -exportArchive to an IPA. The custom Flutter.framework comes from the resolved engine bundle (EngineBundleDir).",
	}
	// The build is GATED unless BOTH the signing identity and the Xcode project are present AND
	// xcodebuild exists on this host. We never run it here — it is owner/Xcode-gated by policy — but
	// we record precisely why so the operator knows the one missing input.
	var missing []string
	if signingIdentity == "" {
		missing = append(missing, "Apple code-signing identity (--signing-identity)")
	}
	if xcodeProject == "" {
		missing = append(missing, "Xcode project/workspace (--xcode-project ios/Runner.xcworkspace)")
	}
	if _, err := exec.LookPath("xcodebuild"); err != nil {
		missing = append(missing, "xcodebuild (Xcode command-line tools) on this host")
	}
	if len(missing) > 0 {
		h.BuildStatus = "gated"
		h.GatedOn = "owner/Xcode-gated archive+signing; missing: " + strings.Join(missing, "; ")
	} else {
		// All inputs present, but the actual archive is owner-gated by policy (no on-host signing in
		// this lane). Mark "ready" — inputs satisfied — without executing/faking the build.
		h.BuildStatus = "ready"
	}
	if err := writeJSONOutput(h, path); err != nil {
		return nil, err
	}
	return &h, nil
}

// ensureControlPlaneSetup idempotently creates the control-plane app + release for an engine lane.
// An "already exists" outcome is treated as success so the command is safe to re-run — across BOTH
// the file store ("... already exists") and the Postgres backend ("duplicate key value violates
// unique constraint", SQLSTATE 23505).
func ensureControlPlaneSetup(apiBase, appID, appName, releaseID, runtimeID, version, channel string) error {
	apiRoot := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if err := postJSONDecode(apiRoot+"/v1/apps", domain.CreateAppRequest{
		ID:          appID,
		DisplayName: firstNonEmpty(appName, appID),
	}, nil); err != nil && !isAlreadyExistsErr(err) {
		return fmt.Errorf("ensure control-plane app: %w", err)
	}
	ch := normalizedDefaultString(channel, "stable")
	if err := postJSONDecode(apiRoot+"/v1/releases", domain.CreateReleaseRequest{
		ID:        releaseID,
		AppID:     appID,
		RuntimeID: runtimeID,
		Version:   version,
		Platform:  "ios",
		Arch:      "arm64",
		Channel:   ch,
	}, nil); err != nil {
		// The (app, version, platform, arch, channel) tuple is UNIQUE server-side. Ignoring a create
		// error is ONLY safe when THIS release-id already exists (idempotent re-register). A duplicate-key
		// with a DIFFERENT release-id is a COLLISION: masking it (the prior bug — isAlreadyExistsErr treated
		// the 23505 uniqueness violation as "already exists") left a phantom release the patch lane 404s on.
		// So verify the id actually exists before tolerating; otherwise fail with an actionable message.
		switch {
		case releaseExistsByID(apiRoot, releaseID):
			// idempotent: the SAME release-id is already registered — safe to proceed.
		case isUniqueViolationErr(err):
			return fmt.Errorf("REFUSED: a release for app=%s version=%s ios/arm64 channel=%s already exists "+
				"with a DIFFERENT release-id; this build's release-id %q was NOT created (a masked collision "+
				"would 404 at patch time). Reuse the existing release-id, or register this build under a "+
				"distinct --version or --channel", appID, version, ch, releaseID)
		default:
			return fmt.Errorf("ensure control-plane release: %w", err)
		}
	}
	return nil
}

// releaseExistsByID reports whether an engine-lane release with exactly this id is already registered
// (GET /v1/releases/<id> -> 200). Used to distinguish an idempotent same-id re-register from a
// different-id collision on the unique (app, version, platform, arch, channel) tuple.
func releaseExistsByID(apiRoot, releaseID string) bool {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(apiRoot, "/")+"/v1/releases/"+url.PathEscape(strings.TrimSpace(releaseID)), nil)
	if err != nil {
		return false
	}
	applyOperatorHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// isUniqueViolationErr matches the server-side unique-constraint violation (the (app,version,platform,
// arch,channel) collision). Narrower than isAlreadyExistsErr, which also tolerates same-id re-creates.
func isUniqueViolationErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "duplicate key") || strings.Contains(m, "23505") || strings.Contains(m, "unique constraint")
}

// isAlreadyExistsErr reports whether a create error means the resource already exists, tolerating both
// store backends (file store + Postgres) so --api setup is idempotent on prod.
func isAlreadyExistsErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "already exists") ||
		strings.Contains(m, "duplicate key") ||
		strings.Contains(m, "23505")
}

// engineLaneBaseline is the immutable record `release ios-engine` writes (engine + app.dill identity).
type engineLaneBaseline struct {
	Schema              string `json:"schema"`
	EngineBundleSchema  string `json:"engine_bundle_schema,omitempty"`
	ReleaseID           string `json:"release_id"`
	AppID               string `json:"app_id"`
	FlutterCommit       string `json:"flutter_commit"`
	DartRevision        string `json:"dart_revision"`
	FrameworkSHA256     string `json:"framework_sha256"`
	Dart2bytecodeSHA    string `json:"dart2bytecode_sha256"`
	CompileInterfaceSHA string `json:"flutter_compile_interface_sha256"`
	GenSnapshotSHA      string `json:"gen_snapshot_sha256"`
	AppDillSHA256       string `json:"app_dill_sha256"`
	PatchableSHA256     string `json:"patchable_manifest_sha256,omitempty"`
	Arch                string `json:"arch"`
	BuildMode           string `json:"build_mode"`
	ToolchainVersion    string `json:"toolchain_version,omitempty"`
	Experimental        bool   `json:"experimental"`
}

func (b engineLaneBaseline) equals(o engineLaneBaseline) bool {
	bEngineSchema := normalizedDefaultString(b.EngineBundleSchema, engineBundleSchemaV1)
	oEngineSchema := normalizedDefaultString(o.EngineBundleSchema, engineBundleSchemaV1)
	return b.Schema == o.Schema && bEngineSchema == oEngineSchema &&
		b.ReleaseID == o.ReleaseID && b.AppID == o.AppID &&
		b.FlutterCommit == o.FlutterCommit && b.DartRevision == o.DartRevision &&
		strings.EqualFold(b.FrameworkSHA256, o.FrameworkSHA256) &&
		strings.EqualFold(b.Dart2bytecodeSHA, o.Dart2bytecodeSHA) &&
		strings.EqualFold(b.CompileInterfaceSHA, o.CompileInterfaceSHA) &&
		strings.EqualFold(b.GenSnapshotSHA, o.GenSnapshotSHA) &&
		strings.EqualFold(b.AppDillSHA256, o.AppDillSHA256) &&
		strings.EqualFold(b.PatchableSHA256, o.PatchableSHA256) &&
		b.Arch == o.Arch && b.BuildMode == o.BuildMode &&
		b.ToolchainVersion == o.ToolchainVersion && b.Experimental == o.Experimental
}

// --- patch ios-engine: compile a Dart patch + emit the device-proven static-host bundle ---

// engineLaneBundleDescriptor is the EXPLICIT ios_engine bundle kind/schema written alongside the
// device-proven manifest.json/manifest.sig/<bytecode> for provenance. It is NOT consumed by the
// device app (the app fetches the raw manifest.json/manifest.sig/bytecode); it records the engine +
// app.dill identity the patch was built against, so the bundle is unambiguously an engine-lane patch.
type engineLaneBundleDescriptor struct {
	Schema              string `json:"schema"` // provenance descriptor schema; device does not consume it
	Kind                string `json:"kind"`   // ios_engine (explicit; distinct from config_ota_only / runtime_managed_dart)
	ReleaseID           string `json:"release_id"`
	AppID               string `json:"app_id"`
	Version             int    `json:"version"`
	Index               int    `json:"index"`
	Indices             []int  `json:"indices,omitempty"`
	EntrypointContract  string `json:"entrypoint_contract,omitempty"`
	BytecodeFile        string `json:"bytecode_file"`
	BytecodeSHA256      string `json:"bytecode_sha256"`
	FrameworkSHA256     string `json:"framework_sha256"`
	CompileInterfaceSHA string `json:"flutter_compile_interface_sha256"`
	AppDillSHA256       string `json:"app_dill_sha256"`
	PubFingerprint      string `json:"public_key_fingerprint"`
	Experimental        bool   `json:"experimental"`
	HostURL             string `json:"host_url,omitempty"` // device kHostBase when published via --api
}

func runPatchIOSEngine(args []string) error {
	fs := flag.NewFlagSet("patch ios-engine", flag.ContinueOnError)
	baselinePath := fs.String("baseline", "", "path to the immutable engine-lane baseline json written by release ios-engine")
	engineBundle := fs.String("engine-bundle", "", "ADVANCED: path to a hand-specified verified soroq iOS engine bundle (must match the baseline). Prefer --toolchain.")
	toolchainVersion := fs.String("toolchain", "", "resolve the engine bundle from the cached toolchain at ~/.soroq/toolchains/<version>/ios (defaults to the toolchain_version recorded in the baseline); no hand-path / repo checkout")
	patchDart := fs.String("patch", "", "path to the Dart patch source compiled against the registered app.dill")
	appDill := fs.String("app-dill", "", "path to the deployed app.dill (must match the baseline app_dill_sha256)")
	packageConfig := fs.String("package-config", "", "path to .dart_tool/package_config.json for dart2bytecode --packages")
	patchableManifest := fs.String("patchable-manifest", "", "optional --soroq_manifest patchable-fn list; verified against the baseline when provided")
	index := fs.Int("index", 0, "patch index within the module (resolved to soroqPatchTable[index] on device)")
	indicesCSV := fs.String("indices", "", "ADVANCED: comma-separated patch indices from one source library; when neither --index nor --indices is given, every patchable function in --patch is selected")
	version := fs.Int("version", 0, "manifest version (use a FRESH number per patch; modules load once per process)")
	bytecodeName := fs.String("bytecode-name", "pv_patch.bytecode", "output bytecode filename inside the bundle")
	seedBase64 := fs.String("seed-base64", "", "Ed25519 manifest signing seed (base64; NEVER persisted to state/logs/bundles/git)")
	out := fs.String("out", "", "output dir for the static-host bundle (manifest.json, manifest.sig, <bytecode>); optional when --api is used")
	allowMissing := fs.Bool("allow-experimental-missing", false, "DEPRECATED diagnostic-mode flag; never bypasses compiler errors and never proves that the immutable base retained a symbol")
	apiBase := fs.String("api", "", "Soroq API base; publishes the engine bundle through the control plane (kind=ios_engine, served at /v1/engine/{app}/{channel})")
	runtimeID := fs.String("runtime-id", "", "engine runtime compatibility id bound into the signed manifest (required)")
	channel := fs.String("channel", "stable", "release channel for the control-plane patch")
	patchID := fs.String("patch-id", "", "control-plane patch id (required with --api)")
	rollout := fs.Int("rollout", 100, "initial rollout percentage (1-100) for staged/canary delivery; 100 = all devices, partial buckets by device client id (to pause a published patch to 0%, use the rollout command)")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	indexExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "index" {
			indexExplicit = true
		}
	})
	if indexExplicit && strings.TrimSpace(*indicesCSV) != "" {
		return errors.New("use either --index or --indices, not both")
	}
	required := []struct {
		name, val string
	}{
		{"--baseline", *baselinePath}, {"--patch", *patchDart},
		{"--app-dill", *appDill}, {"--package-config", *packageConfig}, {"--seed-base64", *seedBase64},
	}
	for _, r := range required {
		if strings.TrimSpace(r.val) == "" {
			return fmt.Errorf("%s is required", r.name)
		}
	}
	if *version <= 0 {
		return errors.New("--version must be a positive integer (use a FRESH number per patch; the device app caches a module once per process)")
	}
	useAPI := strings.TrimSpace(*apiBase) != ""
	if !useAPI && strings.TrimSpace(*out) == "" {
		return errors.New("provide --out (static-host bundle) and/or --api (publish through the Soroq control plane)")
	}
	if strings.TrimSpace(*runtimeID) == "" {
		return errors.New("--runtime-id is required so the signed patch is bound to one base runtime")
	}
	if useAPI && strings.TrimSpace(*patchID) == "" {
		return errors.New("--api requires --runtime-id and --patch-id (the control-plane patch identity)")
	}
	if *rollout < 1 || *rollout > 100 {
		return errors.New("--rollout must be between 1 and 100 at publish (canary start); to pause a published patch to 0% use the rollout command")
	}

	// 1. load the immutable baseline registered by `release ios-engine`.
	baseline, err := loadEngineLaneBaseline(*baselinePath)
	if err != nil {
		return err
	}
	// 2. resolve the engine bundle: --engine-bundle (advanced) OR --toolchain <version>; when neither
	// is given, fall back to the toolchain_version recorded in the baseline (turnkey: the patch
	// resolves the same cached toolchain the release was bound to).
	resolvedToolchain := strings.TrimSpace(*toolchainVersion)
	if resolvedToolchain == "" && strings.TrimSpace(*engineBundle) == "" {
		resolvedToolchain = strings.TrimSpace(baseline.ToolchainVersion)
	}
	resolvedBundle, err := resolveEngineBundleDir(*engineBundle, resolvedToolchain)
	if err != nil {
		return err
	}
	// re-verify the engine bundle AND require it to match the registered baseline.
	ve, err := verifyEngineBundle(resolvedBundle)
	if err != nil {
		return fmt.Errorf("engine bundle rejected: %w", err)
	}
	if err := engineMatchesBaseline(ve, baseline); err != nil {
		return err
	}
	// 3. the patch must compile against the EXACT deployed app.dill.
	appDillSHA, err := sha256File(*appDill)
	if err != nil {
		return fmt.Errorf("--app-dill: %w", err)
	}
	if !strings.EqualFold(appDillSHA, baseline.AppDillSHA256) {
		return fmt.Errorf("--app-dill sha256 %s does not match the registered baseline %s (the patch must compile against the deployed app.dill)", short(appDillSHA), short(baseline.AppDillSHA256))
	}
	// 4. optional patchable-fn manifest must match the one the app was built with.
	if p := strings.TrimSpace(*patchableManifest); p != "" {
		ps, err := sha256File(p)
		if err != nil {
			return fmt.Errorf("--patchable-manifest: %w", err)
		}
		if baseline.PatchableSHA256 != "" && !strings.EqualFold(ps, baseline.PatchableSHA256) {
			return fmt.Errorf("--patchable-manifest sha256 %s does not match the registered baseline %s", short(ps), short(baseline.PatchableSHA256))
		}
	}

	// 5. compile the Dart patch to bytecode with the bundle's fixed dart2bytecode toolchain.
	// Work dir = --out when given, else a temp dir (so --api-only runs need no static output dir).
	workDir := strings.TrimSpace(*out)
	if workDir == "" {
		tmp, err := os.MkdirTemp("", "soroq-engine-patch-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		workDir = tmp
	}
	workDir = filepath.Clean(workDir)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	bytecodeFile := filepath.Base(strings.TrimSpace(*bytecodeName))
	bytecodePath := filepath.Join(workDir, bytecodeFile)
	prepared, err := prepareRuntimePatchSource(runtimePatchSourceRequest{
		PatchDart:         *patchDart,
		PackageConfig:     *packageConfig,
		PatchableManifest: *patchableManifest,
		Index:             *index,
		IndexExplicit:     indexExplicit,
		IndicesCSV:        *indicesCSV,
	})
	if err != nil {
		return err
	}
	if prepared.Cleanup != nil {
		defer prepared.Cleanup()
	}
	if err := compilePatchBytecode(ve, *appDill, prepared, bytecodePath, *allowMissing); err != nil {
		return err
	}
	bytecodeSHA, err := sha256File(bytecodePath)
	if err != nil {
		return err
	}
	bytecodeBytes, err := os.ReadFile(bytecodePath)
	if err != nil {
		return err
	}

	// 6. build + sign the manifest the device app verifies (top-level bytecodeSha256 + patches[]).
	manifestPatches := make([]engineLanePatch, 0, len(prepared.Indices))
	for _, patchIndex := range prepared.Indices {
		manifestPatches = append(manifestPatches, engineLanePatch{Index: patchIndex, Bytecode: bytecodeFile})
	}
	manifest := engineLaneManifest{
		Version:            *version,
		RuntimeID:          strings.TrimSpace(*runtimeID),
		BytecodeSha256:     bytecodeSHA,
		EntrypointContract: prepared.EntrypointContract,
		Patches:            manifestPatches,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	sigHex, pubFP, err := signEngineManifest(manifestBytes, *seedBase64)
	if err != nil {
		return err
	}

	descriptor := engineLaneBundleDescriptor{
		Schema:              engineLaneBundleDescriptorSchema,
		Kind:                "ios_engine",
		ReleaseID:           baseline.ReleaseID,
		AppID:               baseline.AppID,
		Version:             *version,
		Index:               prepared.Indices[0],
		Indices:             prepared.Indices,
		EntrypointContract:  prepared.EntrypointContract,
		BytecodeFile:        bytecodeFile,
		BytecodeSHA256:      bytecodeSHA,
		FrameworkSHA256:     baseline.FrameworkSHA256,
		CompileInterfaceSHA: baseline.CompileInterfaceSHA,
		AppDillSHA256:       baseline.AppDillSHA256,
		PubFingerprint:      pubFP,
		Experimental:        ve.Experimental,
	}

	// 7a. static-host bundle (--out): Ed25519-hex sig over the EXACT manifest bytes.
	if strings.TrimSpace(*out) != "" {
		if err := os.WriteFile(filepath.Join(workDir, "manifest.json"), manifestBytes, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(workDir, "manifest.sig"), []byte(sigHex), 0o644); err != nil {
			return err
		}
		if err := writeJSONOutput(descriptor, filepath.Join(workDir, "soroq-ios-engine-bundle.json")); err != nil {
			return err
		}
	}

	// 7b. production delivery (--api): publish through the Soroq control plane (kind=ios_engine).
	var hostURL string
	if useAPI {
		bundleZip, err := buildEngineBundleZip(manifestBytes, []byte(sigHex), bytecodeFile, bytecodeBytes)
		if err != nil {
			return err
		}
		hostURL, err = publishEngineBundleViaAPI(*apiBase, *patchID, baseline.AppID, baseline.ReleaseID, *runtimeID, *channel, *rollout, bundleZip)
		if err != nil {
			return err
		}
		descriptor.HostURL = hostURL
	}

	if ve.Experimental {
		fmt.Fprintln(os.Stderr, "WARNING: experimental optimized-profile engine tier — NOT a production/App-Store release engine.")
	}
	if *format == "json" {
		return writeJSONOutput(descriptor, "")
	}
	if useAPI {
		fmt.Printf("published engine-lane patch release=%s version=%d index=%d bytecode=%s sha=%s -> %s (device kHostBase) pubkey=%s tier=%s\n",
			baseline.ReleaseID, *version, *index, bytecodeFile, short(bytecodeSHA), hostURL, pubFP, tierLabel(ve.Experimental))
	} else {
		fmt.Printf("compiled engine-lane patch release=%s version=%d index=%d bytecode=%s sha=%s -> %s (host these statically; the device run is owner-visual) pubkey=%s tier=%s\n",
			baseline.ReleaseID, *version, *index, bytecodeFile, short(bytecodeSHA), workDir, pubFP, tierLabel(ve.Experimental))
	}
	return nil
}

// runRollbackIOSEngine publishes a signed version-0 manifest (device app: version 0 / empty patches
// -> suppress all redirects + return to base).
func runRollbackIOSEngine(args []string) error {
	fs := flag.NewFlagSet("rollback ios-engine", flag.ContinueOnError)
	seedBase64 := fs.String("seed-base64", "", "Ed25519 manifest signing seed (base64; NEVER persisted)")
	out := fs.String("out", "", "output dir for the signed version-0 rollback bundle (manifest.json, manifest.sig); optional when --api is used")
	apiBase := fs.String("api", "", "Soroq API base; publishes the signed version-0 rollback through the control plane (kind=ios_engine)")
	appID := fs.String("app-id", "", "app id (required with --api)")
	releaseID := fs.String("release-id", "", "control-plane release id (required with --api)")
	runtimeID := fs.String("runtime-id", "", "engine runtime compatibility id (required with --api)")
	channel := fs.String("channel", "stable", "release channel for the control-plane rollback patch")
	patchID := fs.String("patch-id", "", "control-plane patch id for the rollback (required with --api)")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// SEED FROM THE ENVIRONMENT, NOT ARGV. A signing seed passed as a flag is readable by any process
	// on the machine via `ps`, and lands in shell history. The canonical `soroq rollback ios` therefore
	// hands it over through SOROQ_ENGINE_SIGNING_SEED instead. The flag stays supported for existing
	// callers, but the environment is preferred when the flag is absent.
	if strings.TrimSpace(*seedBase64) == "" {
		*seedBase64 = strings.TrimSpace(os.Getenv("SOROQ_ENGINE_SIGNING_SEED"))
	}
	if strings.TrimSpace(*seedBase64) == "" {
		return errors.New("no signing seed: set SOROQ_ENGINE_SIGNING_SEED or pass --seed-base64")
	}
	useAPI := strings.TrimSpace(*apiBase) != ""
	if !useAPI && strings.TrimSpace(*out) == "" {
		return errors.New("provide --out (static-host bundle) and/or --api (publish through the Soroq control plane)")
	}
	if strings.TrimSpace(*runtimeID) == "" {
		return errors.New("--runtime-id is required so the signed rollback is bound to one base runtime")
	}
	if useAPI && (strings.TrimSpace(*appID) == "" || strings.TrimSpace(*releaseID) == "" || strings.TrimSpace(*patchID) == "") {
		return errors.New("--api requires --app-id, --release-id, --runtime-id and --patch-id")
	}
	// version 0 / empty patches = rollback to base (matches soroq_ota.dart).
	manifest := engineLaneManifest{Version: 0, RuntimeID: strings.TrimSpace(*runtimeID), BytecodeSha256: "", Patches: []engineLanePatch{}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	// ASSERT BEFORE SIGNING. A rollback reverts a user's app, so it is as security-sensitive as the
	// patch it reverts and must be checked before a signature vouches for it -- not after it is
	// served. This is the single shared assertion (internal/signing); the tree previously had a
	// second, unused rollback signer that owned these checks while the production path had none.
	// The binding exists only when publishing through the control plane; a local `--out` emit has no
	// target to bind to. The manifest SHAPE is asserted in both modes.
	var rollbackBinding *signing.EngineRollbackBinding
	if useAPI {
		rollbackBinding = &signing.EngineRollbackBinding{
			AppID:     strings.TrimSpace(*appID),
			Channel:   normalizedDefaultString(*channel, "stable"),
			ReleaseID: strings.TrimSpace(*releaseID),
			RuntimeID: strings.TrimSpace(*runtimeID),
		}
	}
	if err := signing.AssertEngineRollbackManifest(manifestBytes, rollbackBinding); err != nil {
		return fmt.Errorf("refusing to sign a malformed rollback: %w", err)
	}
	sigHex, pubFP, err := signEngineManifest(manifestBytes, *seedBase64)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*out) != "" {
		outDir := filepath.Clean(*out)
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), manifestBytes, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, "manifest.sig"), []byte(sigHex), 0o644); err != nil {
			return err
		}
	}
	var hostURL string
	if useAPI {
		bundleZip, err := buildEngineBundleZip(manifestBytes, []byte(sigHex), "", nil)
		if err != nil {
			return err
		}
		hostURL, err = publishEngineBundleViaAPI(*apiBase, *patchID, *appID, *releaseID, *runtimeID, *channel, 100, bundleZip)
		if err != nil {
			return err
		}
	}
	result := map[string]any{"version": 0, "rollback": true, "public_key_fingerprint": pubFP}
	if strings.TrimSpace(*out) != "" {
		result["manifest_dir"] = filepath.Clean(*out)
	}
	if hostURL != "" {
		result["host_url"] = hostURL
	}
	if *format == "json" {
		return writeJSONOutput(result, "")
	}
	if useAPI {
		fmt.Printf("published signed engine-lane ROLLBACK (version 0 -> base) through the control plane -> %s pubkey=%s\n", hostURL, pubFP)
	} else {
		fmt.Printf("published signed engine-lane ROLLBACK (version 0 -> base) to %s pubkey=%s\n", filepath.Clean(*out), pubFP)
	}
	return nil
}

// buildEngineBundleZip packs the device-format engine artifacts into the upload bundle the control
// plane stores VERBATIM: manifest.json (exact signed bytes) + manifest.sig (Ed25519 hex) + the
// bytecode. A version-0 rollback carries no bytecode (empty bytecodeName).
func buildEngineBundleZip(manifestBytes, sigBytes []byte, bytecodeName string, bytecodeBytes []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := []struct {
		name string
		data []byte
	}{
		{"manifest.json", manifestBytes},
		{"manifest.sig", sigBytes},
	}
	if strings.TrimSpace(bytecodeName) != "" && bytecodeBytes != nil {
		entries = append(entries, struct {
			name string
			data []byte
		}{filepath.Base(bytecodeName), bytecodeBytes})
	}
	for _, e := range entries {
		entry, err := zw.Create(e.name)
		if err != nil {
			return nil, err
		}
		if _, err := entry.Write(e.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// publishEngineBundleViaAPI creates the control-plane patch (kind=ios_engine) and uploads the engine
// bundle through the existing authenticated path. Returns the device kHostBase URL the app points at.
func publishEngineBundleViaAPI(apiBase, patchID, appID, releaseID, runtimeID, channel string, rolloutPercent int, bundleZip []byte) (string, error) {
	apiRoot := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	resolvedChannel := normalizedDefaultString(channel, "stable")
	hostedPatchURL := apiRoot + "/v1/patches/" + patchID
	var patch domain.Patch
	if err := postJSONDecode(apiRoot+"/v1/patches", domain.CreatePatchRequest{
		ID:             patchID,
		AppID:          appID,
		ReleaseID:      releaseID,
		RuntimeID:      runtimeID,
		Channel:        resolvedChannel,
		Kind:           domain.PatchKindIOSEngine,
		ActivationMode: domain.ActivationNextColdStart,
		ManifestURL:    hostedPatchURL + "/manifest",
		BundleURL:      hostedPatchURL + "/bundle",
		RolloutPercent: rolloutPercent,
	}, &patch); err != nil {
		return "", fmt.Errorf("create engine patch: %w", err)
	}
	if err := uploadPatchBundleBytes(apiRoot, patch.ID, bundleZip); err != nil {
		return "", fmt.Errorf("upload engine bundle: %w", err)
	}
	// apiRoot is the operator/write host. What a DEVICE fetches is the device-serve host, which is a
	// separate origin on the hosted deployment; returning apiRoot here would advertise a URL that 401s.
	return serving.EngineChannelURL(apiRoot, url.PathEscape(appID), url.PathEscape(resolvedChannel)), nil
}

// loadEngineLaneBaseline reads + validates the immutable baseline written by release ios-engine.
func loadEngineLaneBaseline(path string) (engineLaneBaseline, error) {
	var b engineLaneBaseline
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, fmt.Errorf("--baseline: %w", err)
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, fmt.Errorf("--baseline: parse: %w", err)
	}
	if strings.TrimSpace(b.ReleaseID) == "" || strings.TrimSpace(b.AppDillSHA256) == "" ||
		strings.TrimSpace(b.FrameworkSHA256) == "" {
		return b, errors.New("--baseline is missing release_id / framework_sha256 / app_dill_sha256 (regenerate with release ios-engine)")
	}
	if b.EngineBundleSchema == "" {
		b.EngineBundleSchema = engineBundleSchemaV1
	}
	if b.Schema == engineLaneBaselineSchema && strings.TrimSpace(b.CompileInterfaceSHA) == "" {
		return b, errors.New("v2 --baseline is missing flutter_compile_interface_sha256 (regenerate with release ios-engine)")
	}
	return b, nil
}

// engineMatchesBaseline refuses to patch unless the engine bundle is byte-identical (by hash) to the
// one the baseline was registered against — engine SHA skew is rejected, not silently tolerated.
func engineMatchesBaseline(ve *verifiedEngine, b engineLaneBaseline) error {
	wantSchema := strings.TrimSpace(b.EngineBundleSchema)
	if wantSchema == "" {
		wantSchema = engineBundleSchemaV1
	}
	if ve.Manifest.Schema != wantSchema {
		return fmt.Errorf("engine bundle schema %q does not match registered baseline schema %q", ve.Manifest.Schema, wantSchema)
	}
	checks := []struct {
		field, got, want string
	}{
		{"flutter framework", ve.FrameworkSHA, b.FrameworkSHA256},
		{"dart2bytecode", ve.Manifest.Artifacts["dart2bytecode"], b.Dart2bytecodeSHA},
		{"gen_snapshot", ve.Manifest.Artifacts["gen_snapshot"], b.GenSnapshotSHA},
	}
	if wantSchema == engineBundleSchema {
		checks = append(checks, struct{ field, got, want string }{
			"flutter compile interface", ve.Manifest.Artifacts["flutter_compile_interface"], b.CompileInterfaceSHA,
		})
	}
	for _, c := range checks {
		if !strings.EqualFold(strings.TrimSpace(c.got), strings.TrimSpace(c.want)) {
			return fmt.Errorf("engine bundle %s sha256 %s does not match the registered baseline %s (engine skew rejected; re-register with release ios-engine or supply the matching engine)", c.field, short(c.got), short(c.want))
		}
	}
	return nil
}

type runtimePatchSourceRequest struct {
	PatchDart         string
	PackageConfig     string
	PatchableManifest string
	Index             int
	IndexExplicit     bool
	IndicesCSV        string
}

type runtimePatchIdentity struct {
	Index      int
	LibraryURI string
	Class      string
	Symbol     string
	SourcePath string
}

type preparedRuntimePatchSource struct {
	SourcePath         string
	CompilerInput      string
	PackageConfig      string
	FileSystemScheme   string
	FileSystemRoots    []string
	Indices            []int
	EntrypointContract string
	Cleanup            func()
}

const indexedSelectorEntrypointContract = "indexed_selector_v1"

type dartPackageConfig struct {
	Packages []struct {
		Name       string `json:"name"`
		RootURI    string `json:"rootUri"`
		PackageURI string `json:"packageUri"`
	} `json:"packages"`
}

// prepareRuntimePatchSource turns a normal customer patch library into a loadable dynamic module
// without modifying the customer's file. The VM loader requires a no-arg, named
// `dynamicModuleEntrypoint`; ordinary app code does not contain one. We append a generated entrypoint
// to a temporary multi-root overlay under a distinct synthetic library URI, returning a direct
// index->tear-off selector. The immutable base library stays loaded exactly once and the project
// checkout remains byte-for-byte untouched.
func prepareRuntimePatchSource(req runtimePatchSourceRequest) (preparedRuntimePatchSource, error) {
	var zero preparedRuntimePatchSource
	patchPath, err := filepath.Abs(strings.TrimSpace(req.PatchDart))
	if err != nil {
		return zero, err
	}
	packageConfigPath, err := filepath.Abs(strings.TrimSpace(req.PackageConfig))
	if err != nil {
		return zero, err
	}
	patchBytes, err := os.ReadFile(patchPath)
	if err != nil {
		return zero, fmt.Errorf("read --patch: %w", err)
	}
	if _, err := os.Stat(packageConfigPath); err != nil {
		return zero, fmt.Errorf("read --package-config: %w", err)
	}

	// Advanced legacy modules that already own the runtime entrypoint remain supported. The public
	// product path never asks a developer to add it; it is synthesized below.
	hasEntrypoint := strings.Contains(string(patchBytes), "dynamicModuleEntrypoint")
	manifestPath := strings.TrimSpace(req.PatchableManifest)
	if manifestPath == "" {
		if !hasEntrypoint {
			return zero, errors.New("the patch has no dynamicModuleEntrypoint and no --patchable-manifest was provided; use the public `soroq patch ios --engine` flow so the CLI can generate the runtime entrypoint")
		}
		return preparedRuntimePatchSource{
			SourcePath: patchPath, CompilerInput: patchPath, PackageConfig: packageConfigPath,
			Indices: []int{req.Index},
		}, nil
	}

	identities, projectRoot, err := readRuntimePatchIdentities(manifestPath, packageConfigPath)
	if err != nil {
		return zero, err
	}
	selected, err := selectRuntimePatchIdentities(identities, patchPath, req)
	if err != nil {
		return zero, err
	}
	indices := make([]int, 0, len(selected))
	for _, identity := range selected {
		indices = append(indices, identity.Index)
	}
	libraryURI := selected[0].LibraryURI
	for _, identity := range selected[1:] {
		if identity.LibraryURI != libraryURI {
			return zero, errors.New("one runtime module can currently replace multiple functions only when they are declared in the same Dart library; publish each changed library as a separate patch")
		}
	}

	overlayRoot, err := os.MkdirTemp("", "soroq-runtime-patch-overlay-")
	if err != nil {
		return zero, err
	}
	cleanup := func() { _ = os.RemoveAll(overlayRoot) }
	relSource, err := filepath.Rel(projectRoot, patchPath)
	if err != nil || relSource == "." || strings.HasPrefix(relSource, ".."+string(filepath.Separator)) || relSource == ".." {
		cleanup()
		return zero, fmt.Errorf("patch source %s is outside the package-config project root %s", patchPath, projectRoot)
	}
	// Root the generated overlay under a synthetic segment that no package in the
	// package_config maps to. This makes the emitted module a LOOSE library whose
	// URI differs from the base's already-in-dill `package:<app>/<lib>.dart`. Without
	// the prefix the overlay sits at the package's own lib/ path, so the CFE tries to
	// recompile the in-dill library (crashing on the double definition) and, even if
	// it compiled, loadDynamicModule would refuse a module that redeclares an
	// already-loaded package URI. relSource is preserved under the prefix so the
	// file's own layout (same-file helpers, parts) resolves consistently.
	const syntheticSegment = "_soroq_dynamic_module"
	moduleRel := filepath.Join(syntheticSegment, relSource)
	overlaySource := filepath.Join(overlayRoot, moduleRel)
	if err := os.MkdirAll(filepath.Dir(overlaySource), 0o755); err != nil {
		cleanup()
		return zero, err
	}
	generated := patchBytes
	if !hasEntrypoint {
		generated, err = appendRuntimeEntrypoint(patchBytes, selected)
		if err != nil {
			cleanup()
			return zero, err
		}
	}
	if err := os.WriteFile(overlaySource, generated, 0o600); err != nil {
		cleanup()
		return zero, err
	}
	packageConfigRel, err := filepath.Rel(projectRoot, packageConfigPath)
	if err != nil || strings.HasPrefix(packageConfigRel, ".."+string(filepath.Separator)) || packageConfigRel == ".." {
		cleanup()
		return zero, fmt.Errorf("package config %s is outside project root %s", packageConfigPath, projectRoot)
	}
	const scheme = "soroq-patch"
	virtualPackages := scheme + ":///" + filepath.ToSlash(packageConfigRel)
	// Compile the replacement functions under a DISTINCT synthetic module library
	// identity instead of the base's real `package:<app>/<lib>.dart` URI. The base
	// library is already loaded into the running isolate from --import-dill; a
	// dynamic module that redeclares that exact URI is refused by the VM's
	// loadDynamicModule ("library '<uri>' is already loaded"). Redirection is keyed
	// by the stable soroqPatchTable index (see the app-side activator + selector
	// contract), so the replacement library's identity is intentionally independent
	// of the base-function identity — a device-proven property (the single-function
	// standalone-library control loaded and redirected on-device). Emitting under
	// `soroq-patch:///<relSource>` keeps the module's imports/typing resolved against
	// the same package_config + import-dill while giving it a URI absent from the
	// immutable base, so the loader accepts it.
	syntheticModuleURI := scheme + ":///" + filepath.ToSlash(moduleRel)
	return preparedRuntimePatchSource{
		SourcePath:       overlaySource,
		CompilerInput:    syntheticModuleURI,
		PackageConfig:    virtualPackages,
		FileSystemScheme: scheme,
		FileSystemRoots:  []string{overlayRoot, projectRoot},
		Indices:          indices,
		EntrypointContract: func() string {
			if hasEntrypoint {
				return ""
			}
			return indexedSelectorEntrypointContract
		}(),
		Cleanup: cleanup,
	}, nil
}

func readRuntimePatchIdentities(manifestPath, packageConfigPath string) ([]runtimePatchIdentity, string, error) {
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, "", fmt.Errorf("read --patchable-manifest: %w", err)
	}
	configBytes, err := os.ReadFile(packageConfigPath)
	if err != nil {
		return nil, "", fmt.Errorf("read --package-config: %w", err)
	}
	var cfg dartPackageConfig
	if err := json.Unmarshal(configBytes, &cfg); err != nil {
		return nil, "", fmt.Errorf("parse --package-config: %w", err)
	}
	configDir := filepath.Dir(packageConfigPath)
	projectRoot := filepath.Dir(configDir)
	var identities []runtimePatchIdentity
	for _, line := range strings.Split(string(manifestBytes), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "::")
		if len(parts) != 3 || !strings.HasPrefix(parts[0], "package:") ||
			(parts[1] != "" && !validPatchIdentifier(parts[1])) || !validPatchIdentifier(parts[2]) {
			return nil, "", fmt.Errorf("invalid patchable identity %q", line)
		}
		libraryURI := parts[0]
		withoutScheme := strings.TrimPrefix(libraryURI, "package:")
		packageName, packagePath, ok := strings.Cut(withoutScheme, "/")
		if !ok || packageName == "" || packagePath == "" || strings.Contains(packagePath, "..") {
			return nil, "", fmt.Errorf("invalid package library URI %q", libraryURI)
		}
		var sourcePath string
		for _, pkg := range cfg.Packages {
			if pkg.Name != packageName {
				continue
			}
			root, err := resolvePackageConfigPath(configDir, pkg.RootURI)
			if err != nil {
				return nil, "", err
			}
			packageDir := filepath.Join(root, filepath.FromSlash(pkg.PackageURI))
			sourcePath = filepath.Clean(filepath.Join(packageDir, filepath.FromSlash(packagePath)))
			break
		}
		if sourcePath == "" {
			return nil, "", fmt.Errorf("patchable identity %q names package %q, which is absent from --package-config", line, packageName)
		}
		identities = append(identities, runtimePatchIdentity{
			Index: len(identities), LibraryURI: libraryURI, Class: parts[1], Symbol: parts[2], SourcePath: sourcePath,
		})
	}
	if len(identities) == 0 {
		return nil, "", errors.New("--patchable-manifest contains no identities")
	}
	return identities, projectRoot, nil
}

func resolvePackageConfigPath(configDir, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid package rootUri %q: %w", raw, err)
	}
	if u.IsAbs() {
		if u.Scheme != "file" {
			return "", fmt.Errorf("unsupported package rootUri scheme %q", u.Scheme)
		}
		return filepath.FromSlash(u.Path), nil
	}
	return filepath.Clean(filepath.Join(configDir, filepath.FromSlash(raw))), nil
}

func selectRuntimePatchIdentities(all []runtimePatchIdentity, patchPath string, req runtimePatchSourceRequest) ([]runtimePatchIdentity, error) {
	var requested []int
	if strings.TrimSpace(req.IndicesCSV) != "" {
		seen := map[int]bool{}
		for _, raw := range strings.Split(req.IndicesCSV, ",") {
			i, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || i < 0 {
				return nil, fmt.Errorf("invalid --indices value %q", raw)
			}
			if !seen[i] {
				requested = append(requested, i)
				seen[i] = true
			}
		}
	} else if req.IndexExplicit {
		requested = []int{req.Index}
	} else {
		for _, identity := range all {
			if sameFile(identity.SourcePath, patchPath) {
				requested = append(requested, identity.Index)
			}
		}
	}
	if len(requested) == 0 {
		return nil, fmt.Errorf("--patch %s does not contain any function listed in --patchable-manifest", patchPath)
	}
	selected := make([]runtimePatchIdentity, 0, len(requested))
	for _, i := range requested {
		if i < 0 || i >= len(all) {
			return nil, fmt.Errorf("patch index %d is outside the patchable table (0..%d)", i, len(all)-1)
		}
		if !sameFile(all[i].SourcePath, patchPath) {
			return nil, fmt.Errorf("patch index %d refers to %s, not --patch %s", i, all[i].SourcePath, patchPath)
		}
		selected = append(selected, all[i])
	}
	return selected, nil
}

func sameFile(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	return errA == nil && errB == nil && filepath.Clean(aa) == filepath.Clean(bb)
}

func validPatchIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || r == '$' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func appendRuntimeEntrypoint(source []byte, selected []runtimePatchIdentity) ([]byte, error) {
	if len(selected) == 0 {
		return nil, errors.New("cannot generate a runtime entrypoint without a selected patchable function")
	}
	var b strings.Builder
	b.Write(source)
	if len(source) > 0 && source[len(source)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString("\n// GENERATED BY SOROQ IN A TEMPORARY COMPILER OVERLAY; the customer source is untouched.\n")
	b.WriteString("@pragma('vm:entry-point')\n@pragma('dyn-module:entry-point')\n")
	// Return a FUNCTION directly. A collection literal here is not safe: a
	// tree-shaken AOT base may not have allocate-finalized the dynamic module's
	// exact generic Map/List instantiation, causing loadModule to fail before the
	// controller can inspect it. The direct selector tear-off uses the same
	// runtime representation as the already device-proven one-function contract.
	b.WriteString("Object? dynamicModuleEntrypoint() => _soroqReplacementForIndex;\n\n")
	b.WriteString("Object? _soroqReplacementForIndex(int index) {\n")
	b.WriteString("  switch (index) {\n")
	for _, identity := range selected {
		ref := identity.Symbol
		if identity.Class != "" {
			ref = identity.Class + "." + identity.Symbol
		}
		fmt.Fprintf(&b, "    case %d:\n      return %s;\n", identity.Index, ref)
	}
	// No exception allocation here: the immutable base may not retain an
	// otherwise-unused error constructor. The controller treats null as a
	// fail-closed selector miss.
	b.WriteString("    default:\n      return null;\n")
	b.WriteString("  }\n}\n")
	return []byte(b.String()), nil
}

// compilePatchBytecode runs the bundle's fixed dart2bytecode against the deployed app.dill. Compiler
// diagnostics are rejected. The device loader remains the authoritative retained-symbol gate and must
// activate transactionally; a compile-time interface can describe APIs that a particular base AOT did
// not retain, so successful compilation is never claimed as runtime-retention proof.
func compilePatchBytecode(ve *verifiedEngine, appDill string, patch preparedRuntimePatchSource, outBytecode string, allowMissing bool) error {
	dartaotruntime := ve.ToolPath("dartaotruntime")
	dart2bytecode := ve.ToolPath("dart2bytecode")
	compileInterface := ve.ToolPath("flutter_compile_interface")
	vmPlatform := ve.ToolPath("vm_platform")
	inputs := []string{dartaotruntime, dart2bytecode, vmPlatform, appDill, patch.SourcePath}
	if ve.Manifest.Schema == engineBundleSchema {
		inputs = append(inputs, compileInterface)
	} else {
		compileInterface = ""
	}
	for _, p := range inputs {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing input %s: %w", filepath.Base(p), err)
		}
	}
	cmd := exec.Command(dartaotruntime, patchCompileArgs(
		dart2bytecode, vmPlatform, appDill, compileInterface, patch, outBytecode,
	)...)
	// dart2bytecode/front_end diagnostics are not guaranteed to use stderr.
	// Capture both streams so a compiler refusal never degrades into an empty
	// `exit status 254` message for the developer.
	var diagnostics bytes.Buffer
	cmd.Stdout = &diagnostics
	cmd.Stderr = &diagnostics
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(diagnostics.String())
		if isMissingSymbolError(msg) && !allowMissing {
			return fmt.Errorf("patch compiler could not resolve a referenced symbol from the supplied base/interface; compilation was refused. "+
				"This is distinct from the runtime loader's retained-symbol gate. dart2bytecode: %s", msg)
		}
		return fmt.Errorf("dart2bytecode failed: %v: %s", err, msg)
	}
	return nil
}

// runtimePatchImportPrefix is the slash-separated prefix dart2bytecode adds to the
// import-dill (base) library URIs inside the compiled module, so module references
// to base libraries stay distinct from the base's already-loaded libraries at
// load/canonicalization time. Matches the device-proven soroq_redirect harness.
const runtimePatchImportPrefix = "import/prefix"

func patchCompileArgs(dart2bytecode, vmPlatform, appDill, compileInterface string, patch preparedRuntimePatchSource, outBytecode string) []string {
	args := []string{dart2bytecode,
		"--platform", vmPlatform,
		"--import-dill", appDill,
	}
	if strings.TrimSpace(compileInterface) != "" {
		args = append(args, "--compile-interface", compileInterface)
	}
	if patch.FileSystemScheme != "" {
		args = append(args, "--filesystem-scheme", patch.FileSystemScheme)
		for _, root := range patch.FileSystemRoots {
			args = append(args, "--filesystem-root", root)
		}
	}
	// [soroq] Prefix import-dill library URIs in the emitted module so its imported
	// base-library namespace cannot collide with libraries already loaded in the
	// isolate. This prevents repeat/in-process module loads from being rejected as
	// "already loaded". It is an identity/isolation rule, not a const-object hack:
	// real Flutter nested const widgets are delivered once the immutable base is
	// built with the matching --dynamic-interface retention contract.
	args = append(args, "--prefix-library-uris", runtimePatchImportPrefix)
	return append(args, "--packages", patch.PackageConfig, "-o", outBytecode, patch.CompilerInput)
}

func isMissingSymbolError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "method not found") ||
		strings.Contains(m, "getter not found") ||
		strings.Contains(m, "isn't defined") ||
		strings.Contains(m, "undefined name") ||
		strings.Contains(m, "not found:")
}

// --- helpers ---

func sha256File(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func tierLabel(experimental bool) string {
	if experimental {
		return "experimental_profile"
	}
	return "production"
}
