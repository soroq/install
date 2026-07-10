package main

// Android engine-source resolution for `soroq release/patch android` (T006).
//
// BEFORE T006 the Android build path SILENTLY hard-required a LOCAL REPO CHECKOUT: the fallback
// `flutter build` (project_state.go) injected `--local-engine android_release_arm64` /
// `--local-engine-host host_release_arm64` / `--local-engine-src-path <repo>/engine/src` and the
// project build-script path injected `LOCAL_ENGINE` + `SOROQ_REPO_ROOT`, all assuming the operator was
// inside the soroq monorepo with a built engine. That is the gap T006 closes.
//
// AFTER T006 the Android build resolves its engine source in ONE of three EXPLICIT ways:
//   1. cached toolchain (--toolchain <version>): ~/.soroq/toolchains/<version>/android/ installed by
//      `soroq toolchain install` — the product path, mirrors iOS (resolveEngineBundleDir / --toolchain).
//   2. ADVANCED opt-in: an explicit `--local-engine ...` passed through to flutter (kept working so
//      owner-accepted Android is NOT broken), OR a committed project build script
//      (scripts/build_soroq_local_engine_*.sh) which is itself a developer-authored opt-in.
//   3. NEITHER -> BLOCK with the EXACT missing Android artifact list (T002 §2), not a cryptic
//      --local-engine / Flutter-not-found failure.
//
// HONEST SCOPE: this checkout has NO Android engine outputs (it is iOS-only; the device proof lives on
// iOS, T007). So the ACTUAL Android build that consumes a resolved cached toolchain is GATED here. T006
// delivers the resolution + the missing-artifact gate + tests for that logic; it does NOT fake a build.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// androidEngineSourceKind enumerates how the Android build resolves its engine source.
type androidEngineSourceKind int

const (
	// androidEngineSourceCachedToolchain: resolved from ~/.soroq/toolchains/<version>/android/.
	androidEngineSourceCachedToolchain androidEngineSourceKind = iota
	// androidEngineSourceAdvancedLocalEngine: the operator passed an explicit --local-engine through.
	androidEngineSourceAdvancedLocalEngine
)

// androidEngineSource is the resolved, explicit engine source the Android build will consume.
type androidEngineSource struct {
	Kind androidEngineSourceKind
	// BundleDir is set for androidEngineSourceCachedToolchain: ~/.soroq/toolchains/<version>/android.
	BundleDir string
	// ToolchainVersion is set for androidEngineSourceCachedToolchain.
	ToolchainVersion string
}

// androidMissingArtifacts is the EXACT, honest missing-artifact list (T002 §2). It is duplicated
// VERBATIM from the packer's `androidMissingArtifacts`
// (backend/cmd/soroq-toolchain-pack/android.go) because that file is a different `main` package and is
// NOT in this task's allowed_files — it cannot be imported. The two lists MUST agree; this is the
// single source for the `soroq` CLI's Android block.
var androidMissingArtifacts = []string{
	"libflutter.so (per ABI; at minimum arm64-v8a) — Android release engine shared lib",
	"gen_snapshot (Android AOT host tool) — produces libapp.so for the release build",
	"dart2bytecode (Android patch lane host tool)",
	"dartaotruntime (Android patch lane host tool)",
	"vm_platform.dill (Android patch lane platform dill)",
	"flutter_patched_sdk / platform dill (Android) — if the patch lane needs it",
	"engine.json analog (schema soroq.android_engine.v1) with android soroq_patch_hashes + per-artifact SHAs",
}

// androidCachedToolchainBundleDir returns ~/.soroq/toolchains/<version>/android (the cached Android
// engine-bundle dir). It mirrors the cache layout written by `soroq toolchain install`
// (toolchainsRoot / toolchainVersionDir in this package) with the android platform subdir, exactly as
// iOS uses .../<version>/ios (cachedToolchainBundleDir in cmd/soroqctl/ios_engine_patch.go).
func androidCachedToolchainBundleDir(version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" || strings.Contains(version, "/") || strings.Contains(version, "..") {
		return "", fmt.Errorf("invalid toolchain version %q", version)
	}
	root, err := toolchainsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, version, "android"), nil
}

// resolveAndroidEngineSource decides — EXPLICITLY, no silent local-repo default — how the Android build
// gets its engine source. It is a PURE decision function (its only side effect is reading the cache dir
// for existence) so the resolution + gate can be tested directly, mirroring resolveEngineBundleDir.
//
//   - --toolchain <version>  -> RESOLVE ~/.soroq/toolchains/<version>/android/ (must be installed).
//   - explicit --local-engine in the flutter passthrough -> ADVANCED opt-in (kept working).
//   - NEITHER                -> BLOCK with the EXACT missing Android artifact list (no cryptic failure).
//
// A committed project build script (scripts/build_soroq_local_engine_*.sh) is ALSO an explicit opt-in,
// but that decision is made upstream (runProjectSoroqAndroidBuildScript runs before this is consulted),
// so this function is only reached on the fallback `flutter build` path.
func resolveAndroidEngineSource(toolchainVersion string, flutterPassthroughArgs []string) (androidEngineSource, error) {
	toolchainVersion = strings.TrimSpace(toolchainVersion)
	if toolchainVersion != "" {
		dir, err := androidCachedToolchainBundleDir(toolchainVersion)
		if err != nil {
			return androidEngineSource{}, err
		}
		if _, err := os.Stat(filepath.Join(dir, "engine.json")); err != nil {
			return androidEngineSource{}, fmt.Errorf("android toolchain %q is not installed (no engine.json under %s); run `soroq toolchain install %s --api <base>` first: %w", toolchainVersion, dir, toolchainVersion, err)
		}
		return androidEngineSource{
			Kind:             androidEngineSourceCachedToolchain,
			BundleDir:        dir,
			ToolchainVersion: toolchainVersion,
		}, nil
	}
	if hasFlutterFlag(flutterPassthroughArgs, "--local-engine") {
		return androidEngineSource{Kind: androidEngineSourceAdvancedLocalEngine}, nil
	}
	return androidEngineSource{}, blockAndroidEngineToolchain()
}

// blockAndroidEngineToolchain returns the CLEAR blocking error that REPLACES the old silent
// --local-engine / repo-checkout hard-require. It lists the EXACT missing Android artifacts (T002 §2)
// and how to install a toolchain — never a cryptic local-engine or Flutter-not-found failure.
func blockAndroidEngineToolchain() error {
	var b strings.Builder
	b.WriteString("Android build BLOCKED: no Soroq Android engine toolchain is available.\n")
	b.WriteString("This checkout has NO Android engine artifacts (the proven engine lane is iOS).\n")
	b.WriteString("Resolve one of the following, then re-run:\n")
	b.WriteString("  - PRODUCT: install a cached Android toolchain and pass --toolchain <version>:\n")
	b.WriteString("      soroq toolchain install <version> --api <base>   (caches ~/.soroq/toolchains/<version>/android/)\n")
	b.WriteString("  - ADVANCED: pass an explicit `--local-engine <name>` (and `--local-engine-host`/`--local-engine-src-path`)\n")
	b.WriteString("    through to flutter, or commit a project build script (scripts/build_soroq_local_engine_*.sh).\n")
	b.WriteString("A cached Android toolchain must carry ALL of the following (none exist here yet):\n")
	for _, m := range androidMissingArtifacts {
		fmt.Fprintf(&b, "  - %s\n", m)
	}
	b.WriteString("Until an Android toolchain is packed (T003 BlockAndroidPack lists the same set), Android stays design-on-paper; the end-to-end toolchain proof is iOS only (T007).")
	return fmt.Errorf("%s", b.String())
}

// androidEngineSrcPathFromBundleDir maps a cached Android toolchain bundle dir to the
// --local-engine-src-path Flutter expects: the dir whose child `out/{android_release_arm64,
// host_release_arm64}/...` holds the engine. `soroq toolchain install` materializes that `out/` tree
// under <version>/android/ from the packed soroq artifacts (materializeAndroidLocalEngineLayout), and the
// build path completes it with version-matched stock host tooling (completeAndroidLocalEngineLayout), so
// the cached bundle dir IS the engine-src-path. T015 proved a real APK + on-device render from this.
func androidEngineSrcPathFromBundleDir(bundleDir string) string {
	return strings.TrimSpace(bundleDir)
}
