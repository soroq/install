package main

// Zero-touch freehand runtime wiring.
//
// A freehand base app carries the ENTIRE OTA state machine inside the soroq_flutter package; the
// only app-side apply code is a thin activator plus an entrypoint that starts the controller. To
// keep this ZERO-TOUCH — a fresh developer edits nothing under lib/ and never imports, instantiates,
// or calls Soroq runtime code — the CLI generates both under .soroq/generated/ and redirects the
// build entrypoint (`-t`) to the generated bootstrap:
//
//   .soroq/generated/soroq_freehand_activator.g.dart  a dual-interface activator implementing BOTH
//                                                      SoroqEngineActivator (loadModule) AND
//                                                      SoroqFreehandActivator (transitionByIdentity),
//                                                      bound to the dynamic_modules primitives. It
//                                                      carries ZERO OTA policy.
//   .soroq/generated/soroq_bootstrap.g.dart            an entrypoint that wraps the app's real main():
//                                                      configure -> restore (last-good patch live on
//                                                      the first frame, NO network) -> post-frame
//                                                      markStable -> background checkForUpdate ->
//                                                      run the app's main().
//
// The generated bootstrap becomes the build entrypoint for BOTH the baseline release build and the
// candidate patch build, so the compiled app.dill always contains the activator and the identity
// graph is identical across builds. lib/ is never touched; the developer only declares the
// soroq_flutter SDK dependency (a pubspec declaration, not a lib/ change), exactly as `soroq doctor`
// already directs.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	freehandBootstrapRelPath = ".soroq/generated/soroq_bootstrap.g.dart"
	freehandActivatorRelPath = ".soroq/generated/soroq_freehand_activator.g.dart"
)

// freehandActivatorSource is the dual-interface activator, emitted verbatim (with a generated
// header). It implements SoroqEngineActivator + SoroqFreehandActivator and binds the SDK-bundled
// dynamic_modules primitives. Freehand redirects flow through transitionByIdentity; the indexed
// numeric-slot methods are never used in freehand mode and throw if ever called.
const freehandActivatorSource = `// GENERATED — do not edit. Produced by ` + "`soroq release/patch ios --engine`" + ` (freehand).
// The freehand engine-binding activator — the ONLY app-side apply code. It implements BOTH the base
// engine interface (loadModule) AND the freehand identity capability (transitionByIdentity), binding
// the SDK-bundled dynamic_modules primitives. It carries ZERO OTA policy: no network, no signature/
// hash checks, no manifest parsing, no rollout/rollback/quarantine decisions, no URLs/keys. Freehand
// redirects flow through transitionByIdentity; the indexed numeric-slot methods are never used in
// freehand mode and throw if ever called.
import 'dart:typed_data';

import 'package:dynamic_modules/dynamic_modules.dart'
    show loadModuleFromBytes, soroqTransitionBatchByIdentity;
import 'package:soroq_flutter/soroq_flutter.dart'
    show SoroqEngineActivator, SoroqFreehandActivator;

class SoroqFreehandActivatorImpl
    implements SoroqEngineActivator, SoroqFreehandActivator {
  @override
  Future<Object?> loadModule(Uint8List bytecode) =>
      loadModuleFromBytes(bytecode);

  @override
  int transitionByIdentity(
    List<String> newFlatSpecs,
    List<String> staleFlatBaseIds,
  ) =>
      soroqTransitionBatchByIdentity(newFlatSpecs, staleFlatBaseIds);

  @override
  bool redirect(int index, Object? replacement) => throw UnsupportedError(
        'freehand activator applies patches by identity (transitionByIdentity), '
        'not by numeric index',
      );

  @override
  void rollbackToBase() => throw UnsupportedError(
        'freehand activator reverts by identity (transitionByIdentity with an '
        'empty new set), not by indexed rollbackToBase',
      );
}
`

// freehandBootstrapSource renders the zero-touch entrypoint. Every interpolated value is validated by
// the caller (safe charsets / 64-hex / parsed URL / package: URI) so it cannot break the Dart literal.
func freehandBootstrapSource(cfg freehandBootstrapConfig) string {
	return fmt.Sprintf(`// GENERATED — do not edit. Produced by `+"`soroq release/patch ios --engine`"+` (freehand).
// Zero-touch entrypoint. It wraps the app's real main() so a fresh developer never edits lib/ and
// never calls Soroq.configure(): configure the engine-lane controller, restore the last committed
// good patch BEFORE the first frame (NO network), then run the app. Stability is committed per exact
// version AFTER a healthy rendered frame — the restored version on its first frame, and any newly
// downloaded+applied version on its first post-apply frame — so a network patch is never left
// uncommitted (and thus never falsely quarantined), and a crashing patch stays pending. OTA wiring
// never blocks or fails app launch.
// ignore_for_file: directives_ordering, unawaited_futures

import 'package:flutter/widgets.dart';
import 'package:soroq_flutter/soroq_flutter.dart';

import '%s' as app;
import 'soroq_freehand_activator.g.dart';

const String _soroqAppId = '%s';
const String _soroqRuntimeId = '%s';
const String _soroqChannel = '%s';
const String _soroqControlPlaneBaseUrl = '%s';
const String _soroqPinnedEnginePublicKeyHex = '%s';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  SoroqEngineLaneController? controller;
  try {
    controller = await Soroq.configure(
      config: SoroqEngineLaneConfig(
        appId: _soroqAppId,
        runtimeId: _soroqRuntimeId,
        channel: _soroqChannel,
        controlPlaneBaseUrl: Uri.parse(_soroqControlPlaneBaseUrl),
        pinnedEnginePublicKeyHex: _soroqPinnedEnginePublicKeyHex,
      ),
      activator: SoroqFreehandActivatorImpl(),
    );
    // Cold start, PHASE 1+2 ONLY: restore persisted state and do crash-loop accounting, with NO
    // network and -- critically -- NO module load and NO redirect. Installing redirects before the
    // first frame was reproduced 3/3 on a physical iPhone to leave the window permanently BLACK while
    // the UI thread kept building frames and the redirected functions returned correct values. State
    // work stays here because it is what makes crash-loop quarantine correct; activation moves to the
    // first-frame callback below.
    await controller.restorePrepare();
  } catch (_) {
    // OTA wiring must never prevent the app from launching.
  }

  // THE APP STARTS FIRST. Everything OTA-related is chained behind Flutter's first RASTERIZED frame.
  //
  // The previous ordering ran checkForUpdate() before app.main(), and checkForUpdate() reached the
  // activation path itself -- so redirects were installed before the first frame and the later post-frame
  // activation found the work already done and no-opped. The split existed on paper only.
  app.main();

  final SoroqEngineLaneController? c = controller;
  if (c != null) {
    // Persisted-module activation waits for the frame to be RASTERIZED (not merely built), installs the
    // redirects, requests a frame so the patched values are presented, and commits stability only after a
    // healthy post-redirect frame. Only THEN does the hosted check run: checkForUpdate() must never race
    // ahead of activation, or it would activate the persisted module pre-frame itself. No timers, sleeps
    // or retries are involved -- this is ordering.
    soroqActivateRestoredAfterFirstFrame(c).then((_) {
      return c.checkForUpdate().then((SoroqOtaStatus st) {
        if (st.error == null && st.isPatched && st.activeVersion != 0) {
          soroqCommitStableOnHealthyFrame(c, st.activeVersion);
        }
      });
    }).catchError((Object _, StackTrace __) {
      // Network/apply error: do NOT mark stable; the retained (previous-good or base) state stays.
    });
  }
}
`, cfg.EntrypointImport, cfg.AppID, cfg.RuntimeID, cfg.Channel, cfg.ControlPlaneBaseURL, cfg.PinnedEnginePubKeyHex)
}

// freehandBootstrapConfig is the resolved zero-touch runtime config baked into the bootstrap.
type freehandBootstrapConfig struct {
	AppID                 string
	RuntimeID             string
	Channel               string
	ControlPlaneBaseURL   string
	PinnedEnginePubKeyHex string
	EntrypointImport      string // package:<pkg>/<path under lib/>
}

var (
	freehandSafeIDRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	freehandHex64RE        = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	freehandHTTPURLRE      = regexp.MustCompile(`^https?://[A-Za-z0-9._~:/?#\[\]@!$&()*+,;=%-]+$`)
	freehandSoroqFlutterRE = regexp.MustCompile(`(?m)^\s+soroq_flutter\s*:`)
	// mainDecl matches a COLUMN-0 top-level `main(` declaration and captures its parameter list.
	freehandMainDeclRE = regexp.MustCompile(`(?m)^(?:Future(?:<[^>]*>)?\s+|void\s+|dynamic\s+)?main\s*\(([^)]*)\)`)
)

// prepareFreehandZeroTouch resolves the runtime config, validates it, generates the dual-interface
// activator + bootstrap entrypoint under .soroq/generated/, and returns the project-relative path of
// the generated bootstrap (to be used as the build `-t`). pinnedKeyHex is the app's pinned engine
// public key (hex) returned by ensureManifestTrust. developerPassthrough is the raw `-- <flutter
// build flags>` slice, read (never mutated) to honor a custom `-t`/`--target` entrypoint.
func prepareFreehandZeroTouch(projectDir, pinnedKeyHex string, developerPassthrough []string) (bootstrapRel string, err error) {
	if err := ensureSoroqFlutterDependency(projectDir); err != nil {
		return "", err
	}

	pubBytes, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		return "", fmt.Errorf("read pubspec.yaml: %w", err)
	}
	packageName := strings.TrimSpace(parseTopLevelYaml(pubBytes)["name"])
	if packageName == "" {
		return "", errors.New("pubspec.yaml is missing a top-level package name")
	}

	soroqBytes, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		return "", fmt.Errorf("read soroq.yaml: %w", err)
	}
	meta, err := buildSoroqBundledMetadata(soroqBytes, pubBytes)
	if err != nil {
		return "", fmt.Errorf("compute runtime identity: %w", err)
	}
	appID := strings.TrimSpace(meta.Soroq.AppID)
	runtimeID := strings.TrimSpace(meta.Soroq.RuntimeID)
	channel := strings.TrimSpace(meta.Soroq.Channel)
	// The DEVICE fetches manifests from the device-serve base (api.soroq.dev), which is a DISTINCT
	// endpoint from the operator write/publish base (soroq.dev/api). We must NOT use defaultAPIBase()
	// here: with operator credentials present it returns the write endpoint, which 401s device reads.
	controlPlaneURL := defaultControlPlaneAPI

	// Validate every value before it is baked into the generated Dart (defends the string literal and
	// fails at build time rather than at runtime inside SoroqEngineLaneConfig).
	if !freehandSafeIDRE.MatchString(appID) {
		return "", fmt.Errorf("soroq.yaml app_id %q is not a safe identifier for zero-touch wiring", appID)
	}
	if !freehandHex64RE.MatchString(runtimeID) {
		return "", fmt.Errorf("computed runtime_id %q is not a 64-hex runtime identity", runtimeID)
	}
	if !freehandSafeIDRE.MatchString(channel) {
		return "", fmt.Errorf("soroq.yaml channel %q is not a safe identifier for zero-touch wiring", channel)
	}
	if !freehandHTTPURLRE.MatchString(controlPlaneURL) {
		return "", fmt.Errorf("control-plane base URL %q is not a valid http(s) URL", controlPlaneURL)
	}
	if !freehandHex64RE.MatchString(strings.TrimSpace(pinnedKeyHex)) {
		return "", fmt.Errorf("pinned engine public key must be 64 hex chars (a 32-byte Ed25519 key); got %q — check manifest_trust.keys[0] in soroq.yaml", pinnedKeyHex)
	}

	target, _ := flagValue(developerPassthrough, "t")
	if strings.TrimSpace(target) == "" {
		if v, _ := flagValue(developerPassthrough, "target"); strings.TrimSpace(v) != "" {
			target = v
		}
	}
	importURI, err := resolveFreehandEntrypointImport(projectDir, packageName, target)
	if err != nil {
		return "", err
	}

	cfg := freehandBootstrapConfig{
		AppID:                 appID,
		RuntimeID:             strings.ToLower(runtimeID),
		Channel:               channel,
		ControlPlaneBaseURL:   controlPlaneURL,
		PinnedEnginePubKeyHex: strings.ToLower(strings.TrimSpace(pinnedKeyHex)),
		EntrypointImport:      importURI,
	}
	if err := writeFreehandZeroTouchFiles(projectDir, cfg); err != nil {
		return "", err
	}
	return freehandBootstrapRelPath, nil
}

// ensureSoroqFlutterDependency fails clearly when the soroq_flutter SDK is not declared. This is the
// one thing the developer declares (a pubspec dependency, never a lib/ edit) — the same boundary
// `soroq doctor` enforces. dynamic_modules (the runtime primitive package) is auto-wired separately.
func ensureSoroqFlutterDependency(projectDir string) error {
	b, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		return fmt.Errorf("read pubspec.yaml: %w", err)
	}
	if !freehandSoroqFlutterRE.MatchString(string(b)) {
		return errors.New("zero-touch freehand requires the soroq_flutter SDK dependency; run `flutter pub add soroq_flutter` (a pubspec declaration, not a lib/ change)")
	}
	return nil
}

// resolveFreehandEntrypointImport maps the build entrypoint (default lib/main.dart, or a custom
// -t/--target) to its package: URI and validates that its main() is callable with no arguments.
// A custom entrypoint outside lib/ has no package: URI and is refused with a clear error.
func resolveFreehandEntrypointImport(projectDir, packageName, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		target = "lib/main.dart"
	}
	slash := filepath.ToSlash(target)
	if filepath.IsAbs(slash) || strings.Contains(slash, "..") {
		return "", fmt.Errorf("zero-touch freehand requires a project-relative entrypoint under lib/; got %q", target)
	}
	if !strings.HasSuffix(slash, ".dart") {
		return "", fmt.Errorf("entrypoint %q must be a .dart file", target)
	}
	if !strings.HasPrefix(slash, "lib/") {
		return "", fmt.Errorf("zero-touch freehand can only wrap an entrypoint under lib/ (got %q); a custom entrypoint outside lib/ has no package: URI and is unsupported — move it under lib/ or disable freehand for this build", target)
	}
	abs := filepath.Join(projectDir, filepath.FromSlash(slash))
	src, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read entrypoint %s: %w", abs, err)
	}
	if err := validateFreehandNoArgMain(string(src), target); err != nil {
		return "", err
	}
	return "package:" + packageName + "/" + strings.TrimPrefix(slash, "lib/"), nil
}

// validateFreehandNoArgMain confirms the entrypoint declares a top-level main() the wrapper can call
// with no arguments (bare `main()`, or all-optional `[..]`/`{..}` params). A required positional
// parameter (e.g. `main(List<String> args)`) is refused, because the generated wrapper invokes
// `app.main()` with no arguments.
func validateFreehandNoArgMain(src, target string) error {
	clean := stripDartCommentsAndStrings(src)
	m := freehandMainDeclRE.FindStringSubmatch(clean)
	if m == nil {
		return fmt.Errorf("entrypoint %q declares no top-level main() that zero-touch can wrap", target)
	}
	params := strings.TrimSpace(m[1])
	if params == "" || strings.HasPrefix(params, "[") || strings.HasPrefix(params, "{") {
		return nil
	}
	return fmt.Errorf("entrypoint %q main(%s) takes a required argument; zero-touch wrapping supports only a no-argument main() (or all-optional parameters) — provide `void main()`", target, params)
}

// writeFreehandZeroTouchFiles writes the activator + bootstrap atomically (temp + rename) under
// .soroq/generated/. It is idempotent: identical inputs reproduce byte-identical files.
func writeFreehandZeroTouchFiles(projectDir string, cfg freehandBootstrapConfig) error {
	genDir := filepath.Join(projectDir, ".soroq", "generated")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return fmt.Errorf("create .soroq/generated: %w", err)
	}
	// Acceptance lifecycle tracing is opt-in via SOROQ_FREEHAND_LIFECYCLE_TRACE=1 and rewrites nothing
	// when unset, so ordinary generation stays byte-identical (see freehand_lifecycle_trace.go).
	activatorSrc := injectActivatorLifecycleTrace(freehandActivatorSource, freehandPackageName(cfg.EntrypointImport))
	bootstrapSrc := injectBootstrapLifecycleTrace(freehandBootstrapSource(cfg), freehandPackageName(cfg.EntrypointImport))
	traceLib := filepath.Join(projectDir, "lib", "soroq_trace.dart")
	if freehandLifecycleTraceEnabled() {
		// The trace clock lives in the app package so the app's own value samples and the bootstrap's
		// lifecycle events share ONE monotonic source.
		if err := writeFileAtomic(traceLib, []byte(freehandLifecycleTraceLibrary())); err != nil {
			return fmt.Errorf("write lifecycle trace library: %w", err)
		}
	} else if b, err := os.ReadFile(traceLib); err == nil && strings.Contains(string(b), freehandLifecycleTraceEnv) {
		// REMOVE a leftover from a previous traced generation. Writing into the developer's lib/ and
		// never cleaning up would leave acceptance instrumentation in a normal build -- and lib/ is
		// precisely what zero-touch promises never to touch. Only a file this generator wrote (it
		// carries the env-var marker) is removed; a developer's own file of that name is left alone.
		if err := os.Remove(traceLib); err != nil {
			return fmt.Errorf("remove stale lifecycle trace library: %w", err)
		}
	}
	if err := writeFileAtomic(filepath.Join(projectDir, filepath.FromSlash(freehandActivatorRelPath)), []byte(activatorSrc)); err != nil {
		return fmt.Errorf("write freehand activator: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(projectDir, filepath.FromSlash(freehandBootstrapRelPath)), []byte(bootstrapSrc)); err != nil {
		return fmt.Errorf("write freehand bootstrap: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to path via a sibling temp file + rename (0644).
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// withFreehandBootstrapEntrypoint returns a copy of the developer's flutter build passthrough with any
// developer `-t`/`--target` removed and the generated bootstrap set as the entrypoint. The Soroq
// entrypoint MUST win so the compiled app.dill always contains the activator; the developer's chosen
// entrypoint is still honored — it is wrapped (its main() is invoked by the bootstrap).
func withFreehandBootstrapEntrypoint(bootstrapRel string, developerPassthrough []string) []string {
	stripped := stripFlag(developerPassthrough, "t", false)
	stripped = stripFlag(stripped, "target", false)
	out := make([]string, 0, len(stripped)+2)
	out = append(out, "-t", bootstrapRel)
	return append(out, stripped...)
}
