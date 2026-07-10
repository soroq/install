package main

// iOS ENGINE lane wiring for the top-level `soroq` product CLI (Phase 9 / T009 Slice C).
//
// The engine lane (hot-patching Dart CODE via the soroq interpreter-in-engine) is DISTINCT from the
// config_ota_only iOS lane (`soroq release/patch ios`). Its safety-critical compile/sign/verify logic
// lives in `soroqctl` (cmd/soroqctl/ios_engine_patch.go) and operates on local operator inputs — a
// verified engine bundle, the deployed app.dill, dart2bytecode — that the API-oriented `soroq` project
// flow does not manage. Rather than duplicate that logic here (a maintenance + drift hazard) or invent
// a shared package (outside this task's allowed files), the top-level commands DELEGATE to soroqctl so
// `soroq <verb> ios-engine ...` is genuinely runnable and discoverable from the product CLI.
//
// Scope/claims boundary: EXPERIMENTAL operator lane. NO parity / arbitrary-Dart / App-Store /
// production-readiness claim. Never silently falls back to the config lane.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// patchIOSEngineRequested reports whether `soroq patch ios ...` asked for the iOS ENGINE (hard-OTA
// Dart-code) lane rather than the default config/data lane. The config lane is `--config-file ...`;
// an explicit `--engine` OR a `--toolchain <ver>` (which the config lane never takes) routes to the
// hard engine lane. Explicit split so the two lanes never silently mix.
func patchIOSEngineRequested(args []string) bool {
	for _, a := range args {
		switch {
		case a == "--engine" || a == "-engine":
			return true
		case a == "--toolchain" || a == "-toolchain":
			return true
		case strings.HasPrefix(a, "--toolchain=") || strings.HasPrefix(a, "-toolchain="):
			return true
		}
	}
	return false
}

// releaseIOSEngineRequested reports whether `soroq release ios ...` asked for the ENGINE baseline
// (hard-OTA) delegate. Only an explicit `--engine` routes here: `--toolchain` on `release ios` is
// already claimed by the existing `--build --toolchain` app-build leg, so it must NOT also trigger
// the engine delegate.
func releaseIOSEngineRequested(args []string) bool {
	for _, a := range args {
		if a == "--engine" || a == "-engine" {
			return true
		}
	}
	return false
}

// stripEngineRoutingFlag removes the routing-only `--engine` flag; the delegated `soroqctl <verb>
// ios-engine` subcommand does not accept it. `--toolchain` and every other flag pass through.
func stripEngineRoutingFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--engine" || a == "-engine" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// hasFlag reports whether args contains the given boolean flag (`--name` / `-name` / `--name=...`).
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--"+name || a == "-"+name ||
			strings.HasPrefix(a, "--"+name+"=") || strings.HasPrefix(a, "-"+name+"=") {
			return true
		}
	}
	return false
}

// flagValue extracts the value of a `--name value` / `--name=value` flag from args. Returns ("", false)
// when absent.
func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == "--"+name || a == "-"+name {
			if i+1 < len(args) {
				return strings.TrimSpace(args[i+1]), true
			}
			return "", true
		}
		for _, pfx := range []string{"--" + name + "=", "-" + name + "="} {
			if strings.HasPrefix(a, pfx) {
				return strings.TrimSpace(strings.TrimPrefix(a, pfx)), true
			}
		}
	}
	return "", false
}

// stripFlag removes a flag (and its separate value token, when valued) from args. When bool=true the
// flag takes no value token.
func stripFlag(args []string, name string, boolFlag bool) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--"+name || a == "-"+name {
			if !boolFlag && i+1 < len(args) {
				i++ // skip the value token too
			}
			continue
		}
		if strings.HasPrefix(a, "--"+name+"=") || strings.HasPrefix(a, "-"+name+"=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// splitFlutterPassthrough splits args at a lone `--` separator into (soroq/delegate args, flutter build
// passthrough args). When no `--` is present the second slice is empty.
func splitFlutterPassthrough(args []string) (head []string, passthrough []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// runReleaseIOSEngineBuild implements the UNIFIED `soroq release ios --engine --build`: generate the
// engine-lane scaffold, build app.dill from the cached toolchain with the patchable manifest, then
// register the immutable engine-lane baseline through the soroqctl delegate (passing the built app.dill
// + the generated patchable manifest, so the baseline persists the manifest sha). Fresh-developer path:
// no repo checkout, no dependency_overrides, no hand-paths.
func runReleaseIOSEngineBuild(args []string) error {
	head, passthrough := splitFlutterPassthrough(args)
	projectDir, _ := flagValue(head, "project-dir")
	if strings.TrimSpace(projectDir) == "" {
		projectDir = "."
	}
	toolchain, _ := flagValue(head, "toolchain")
	if strings.TrimSpace(toolchain) == "" {
		return errors.New("`soroq release ios --engine --build` requires --toolchain <version> (the cached iOS toolchain installed by `soroq toolchain install`)")
	}
	if _, ok := flagValue(head, "app-dill"); ok {
		return errors.New("with --build, app.dill is produced by the build; do not also pass --app-dill")
	}
	if _, ok := flagValue(head, "patchable-manifest"); ok {
		return errors.New("with --engine --build, the patchable manifest is generated from soroq.yaml; do not also pass --patchable-manifest")
	}

	if err := requireIOSEngineEnabled(projectDir); err != nil {
		return err
	}
	// Self-heal a missing manifest_trust before scaffolding/building so a fresh iOS engine-lane build
	// never fails with the fork's `Expected soroq.yaml to define "manifest_trust"`. Idempotent: a valid
	// existing block is preserved untouched; an invalid one surfaces an actionable error.
	if _, err := ensureManifestTrust(projectDir); err != nil {
		return err
	}
	if _, err := ensureDynamicModulesInstalled(projectDir); err != nil {
		return fmt.Errorf("install dynamic_modules: %w", err)
	}
	manifestPath, err := generateIOSEngineScaffold(projectDir)
	if err != nil {
		return fmt.Errorf("generate iOS engine scaffold: %w", err)
	}
	fmt.Fprintf(os.Stderr, "soroq release ios --engine --build: generated %s + lib/soroq_patch_table.g.dart + lib/soroq_activator.dart\n", manifestPath)

	appDill, buildErr := buildIOSAppDill(projectDir, toolchain, passthrough)
	if appDill == "" {
		return buildErr
	}
	if buildErr != nil {
		// app.dill was produced before a tail (Xcode/codesign) failure; it is valid for baseline
		// registration. Surface the tail failure as a note and proceed — a signed IPA is owner-gated.
		fmt.Fprintf(os.Stderr, "note: %v — registering the baseline against the produced app.dill anyway (signing is owner-gated).\n", buildErr)
	}

	// Build the delegate args: forward the original head EXCEPT the routing/build-only flags, then add
	// the built app.dill + generated patchable manifest. --project-dir is not a soroqctl flag.
	delegateArgs := stripFlag(head, "engine", true)
	delegateArgs = stripFlag(delegateArgs, "build", true)
	delegateArgs = stripFlag(delegateArgs, "project-dir", false)
	absDill, err := filepath.Abs(appDill)
	if err != nil {
		return err
	}
	delegateArgs = append(delegateArgs, "--app-dill", absDill, "--patchable-manifest", manifestPath)
	return runEngineLaneDelegate("release", delegateArgs)
}

// runPatchIOSEngineScaffolded implements `soroq patch ios --engine`: regenerate the patchable manifest
// from the CURRENT soroq.yaml, REQUIRE it byte-matches the base release's manifest (recorded as
// patchable_manifest_sha256 in the baseline), then delegate to soroqctl forwarding the regenerated
// manifest. A changed/reordered patchable set fails clearly here (a new base release is required).
func runPatchIOSEngineScaffolded(args []string) error {
	head, passthrough := splitFlutterPassthrough(args)
	projectDir, _ := flagValue(head, "project-dir")
	if strings.TrimSpace(projectDir) == "" {
		projectDir = "."
	}
	if err := requireIOSEngineEnabled(projectDir); err != nil {
		return err
	}
	if _, err := ensureDynamicModulesInstalled(projectDir); err != nil {
		return fmt.Errorf("install dynamic_modules: %w", err)
	}
	manifestPath, err := generateIOSEngineScaffold(projectDir)
	if err != nil {
		return fmt.Errorf("generate iOS engine scaffold: %w", err)
	}

	// Guard: the regenerated manifest MUST byte-match the base release's manifest. The baseline json
	// (soroqctl --baseline) records patchable_manifest_sha256; compare against the regenerated sha.
	if baselinePath, ok := flagValue(head, "baseline"); ok && strings.TrimSpace(baselinePath) != "" {
		if err := assertManifestMatchesBaseline(manifestPath, baselinePath); err != nil {
			return err
		}
	}

	// Forward everything except routing/project-dir; inject the regenerated manifest when the caller did
	// not already pass one (ours is authoritative).
	delegateArgs := stripFlag(head, "engine", true)
	delegateArgs = stripFlag(delegateArgs, "project-dir", false)
	if _, ok := flagValue(delegateArgs, "patchable-manifest"); !ok {
		delegateArgs = append(delegateArgs, "--patchable-manifest", manifestPath)
	}
	if len(passthrough) > 0 {
		delegateArgs = append(delegateArgs, append([]string{"--"}, passthrough...)...)
	}
	return runEngineLaneDelegate("patch", delegateArgs)
}

// requireIOSEngineEnabled fails unless soroq.yaml at projectDir enables ios_engine.
func requireIOSEngineEnabled(projectDir string) error {
	soroqPath := filepath.Join(projectDir, "soroq.yaml")
	b, err := os.ReadFile(soroqPath)
	if err != nil {
		return fmt.Errorf("read soroq.yaml at %s (run `soroq init` first): %w", soroqPath, err)
	}
	enabled, _, err := parseIOSEnginePatchable(b)
	if err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("soroq.yaml at %s does not enable the iOS engine lane; set ios_engine.enabled: true and list ios_engine.patchable entries", soroqPath)
	}
	return nil
}

// assertManifestMatchesBaseline fails CLEARLY when the regenerated manifest's sha differs from the base
// release's recorded patchable_manifest_sha256 — i.e. the patchable set changed or was reordered.
func assertManifestMatchesBaseline(manifestPath, baselinePath string) error {
	raw, err := os.ReadFile(baselinePath)
	if err != nil {
		return fmt.Errorf("read engine-lane baseline %s: %w", baselinePath, err)
	}
	var base struct {
		ReleaseID       string `json:"release_id"`
		PatchableSHA256 string `json:"patchable_manifest_sha256"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		return fmt.Errorf("parse engine-lane baseline %s: %w", baselinePath, err)
	}
	if strings.TrimSpace(base.PatchableSHA256) == "" {
		// Base did not record a patchable manifest; nothing to enforce here (soroqctl still verifies).
		return nil
	}
	got, err := manifestSHA256(manifestPath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, base.PatchableSHA256) {
		return fmt.Errorf("patchable set changed vs base release %q (regenerated manifest sha %s != baseline %s); a new base release is required — run `soroq release ios --engine --build` with the updated soroq.yaml",
			base.ReleaseID, got[:12], base.PatchableSHA256[:12])
	}
	return nil
}

// runEngineLaneDelegate runs `soroqctl <verb> ios-engine <args...>`, streaming its output. verb is one
// of release/patch/rollback. It resolves the soroqctl binary next to the running `soroq` executable
// first, then on PATH; if neither is found it returns a precise, actionable error (never a silent
// no-op or a fallback to the config lane).
// engineLaneDelegateEnv builds the child soroqctl environment, injecting the operator credential that
// `soroq login` stored (a per-user cli_token or the operator token) so the delegated soroqctl
// authenticates WITHOUT a manual SOROQ_CONTROL_PLANE_OPERATOR_TOKEN export. An explicit env token (set
// by the caller) always wins — this only fills the gap when nothing is exported.
func engineLaneDelegateEnv(args []string) []string {
	env := os.Environ()
	if firstNonEmptyEnv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "SOROQ_OPERATOR_TOKEN") != "" {
		return env
	}
	creds, err := currentOperatorCredentialsForRequest("", apiFlagFromArgs(args))
	if err != nil || strings.TrimSpace(creds.Token) == "" {
		return env
	}
	env = append(env, "SOROQ_CONTROL_PLANE_OPERATOR_TOKEN="+strings.TrimSpace(creds.Token))
	// Only the static operator_token uses the email header; a cli_token's email is bound to the token
	// server-side, so we never forward a (possibly mismatched) email for it.
	if creds.Email != "" && normalizeCredentialKind(creds.CredentialKind, creds.Token) == credentialKindOperatorToken {
		env = append(env, "SOROQ_OPERATOR_EMAIL="+strings.TrimSpace(creds.Email))
	}
	return env
}

// apiFlagFromArgs extracts --api <val> / --api=<val> from the delegate args (default: the standard
// control-plane base) so the correct per-api stored credential is resolved.
func apiFlagFromArgs(args []string) string {
	for i, a := range args {
		if a == "--api" && i+1 < len(args) {
			return strings.TrimSpace(args[i+1])
		}
		if strings.HasPrefix(a, "--api=") {
			return strings.TrimSpace(strings.TrimPrefix(a, "--api="))
		}
	}
	return defaultControlPlaneAPI
}

func runEngineLaneDelegate(verb string, args []string) error {
	bin, err := resolveSoroqctl()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "soroq %s ios-engine: experimental engine lane (Dart-code OTA via the soroq interpreter-in-engine), delegating to soroqctl.\n", verb)
	fmt.Fprintln(os.Stderr, "  product path: pass --toolchain <version> to resolve the cached engine bundle from `soroq toolchain install` (no hand-path / repo checkout).")
	fmt.Fprintln(os.Stderr, "  advanced mode: `soroqctl "+verb+" ios-engine --engine-bundle <dir> ...` runs the same lane against a hand-specified bundle.")
	fullArgs := append([]string{verb, "ios-engine"}, args...)
	cmd := exec.Command(bin, fullArgs...)
	cmd.Env = engineLaneDelegateEnv(args)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// soroqctl already printed the diagnostic; surface a non-zero exit without double-printing.
			return errAlreadyPrinted
		}
		return fmt.Errorf("invoke soroqctl: %w", err)
	}
	return nil
}

// resolveSoroqctl finds the soroqctl binary next to the running soroq executable, else on PATH.
func resolveSoroqctl() (string, error) {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "soroqctl")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath("soroqctl"); err == nil {
		return path, nil
	}
	return "", errors.New("engine lane requires the soroqctl binary (not found next to soroq or on PATH); " +
		"build it (go build ./backend/cmd/soroqctl) and run `soroqctl <release|patch|rollback> ios-engine ...` directly, " +
		"or place soroqctl alongside the soroq executable")
}
