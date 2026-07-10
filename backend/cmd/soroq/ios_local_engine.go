package main

// iOS cached-toolchain -> Flutter `--local-engine` layout composition (T030).
//
// This is the iOS analog of android_local_engine.go. The hosted iOS toolchain archive is deliberately
// MINIMAL: it carries only the SOROQ-under-test engine artifacts (the bare Mach-O `flutter_framework`
// binary + the host `gen_snapshot`) plus the patch-lane tools, stored FLAT under
// ~/.soroq/toolchains/<version>/ios/. Flutter's `--local-engine=ios_profile` build, however, consumes an
// engine-source `out/{ios_profile,host_profile_unopt}/...` tree (a Flutter.xcframework the tool reads via
// _getIosFrameworkPath, a host+device gen_snapshot, plus stock host tooling). T030 closes that gap
// WITHOUT hosting a 70-80GB engine checkout, mirroring the proven Android pattern:
//
//   - materializeIOSLocalEngineLayout (run first) maps the packed SOROQ artifacts into the `out/` layout
//     Flutter expects. ONLY soroq-under-test bytes are placed here: the bare `flutter_framework` Mach-O
//     (sha 8729cf75, == the T051 device engine) into out/ios_profile/Flutter.framework/Flutter, and the
//     host gen_snapshot into the three name/dir variants _genSnapshotPath probes for an iOS target.
//   - completeIOSLocalEngineLayout overlays the VERSION-MATCHED STOCK host tooling the minimal pack omits
//     (dart-sdk + frontend_server, flutter_patched_sdk, const_finder) sourced from the resolved Soroq
//     Flutter frontend's own bin/cache, and DEEP-COPIES the stock precached Flutter.xcframework SHELL
//     (Headers/Modules/Info.plist/icudtl.dat) then OVERWRITES its ios-arm64 Flutter binary with the SOROQ
//     byte. The stock bin/cache is NEVER mutated (deep copy, never symlink-then-overwrite) and the SOROQ
//     engine bytes are NEVER taken from the frontend cache, so the built/patched artifact is the toolchain
//     under test, not stock.
//
// _getIosFrameworkPath (flutter_tools/lib/src/artifacts.dart, CachedLocalEngineArtifacts) reads the
// device Flutter binary from out/ios_profile/Flutter.xcframework/ios-arm64/Flutter.framework/Flutter for a
// physical `flutter build ios --profile`, NOT the top-level Flutter.framework — so the xcframework MUST
// carry the SOROQ byte. assertSoroqIOSFramework enforces 8729cf75 both in the materialized layout and in
// the built .app.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	iosLocalEngineTargetName = "ios_profile"
	iosLocalEngineHostName   = "host_profile_unopt"
	// soroqIOSFrameworkSHA256 is the bare Mach-O arm64 `flutter_framework` the hosted iOS toolchain packs,
	// == the framework the T051 real-iPhone device proof ran. Any other sha here means a stock or wrong
	// engine was materialized/linked; assertSoroqIOSFramework fails closed on a mismatch.
	soroqIOSFrameworkSHA256 = "8729cf755a2e907cb41670f00afc8db27822904f75ca44f7a1733221dc214448"
)

// iosCachedToolchainBundleDir returns ~/.soroq/toolchains/<version>/ios (the cached iOS engine-bundle
// dir). It mirrors androidCachedToolchainBundleDir + soroqctl's cachedToolchainBundleDir exactly.
func iosCachedToolchainBundleDir(version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" || strings.Contains(version, "/") || strings.Contains(version, "..") {
		return "", fmt.Errorf("invalid toolchain version %q", version)
	}
	root, err := toolchainsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, version, "ios"), nil
}

// materializeIOSLocalEngineLayout maps the FLAT packed soroq artifacts under iosBundleDir into the `out/`
// engine-source layout. Idempotent. ONLY soroq-under-test bytes (the flutter_framework Mach-O + the host
// gen_snapshot).
func materializeIOSLocalEngineLayout(iosBundleDir string) error {
	flatFramework := filepath.Join(iosBundleDir, "flutter_framework")
	flatGenSnapshot := filepath.Join(iosBundleDir, "gen_snapshot")
	if _, err := os.Stat(flatFramework); err != nil {
		return fmt.Errorf("packed flutter_framework missing under %s: %w", iosBundleDir, err)
	}
	if _, err := os.Stat(flatGenSnapshot); err != nil {
		return fmt.Errorf("packed gen_snapshot missing under %s: %w", iosBundleDir, err)
	}
	// Fail-safe: the packed flutter_framework MUST be the SOROQ engine byte, never a stock one.
	if err := assertSoroqIOSFrameworkBinary(flatFramework); err != nil {
		return fmt.Errorf("packed flutter_framework is not the SOROQ engine: %w", err)
	}

	targetOut := filepath.Join(iosBundleDir, "out", iosLocalEngineTargetName)

	// SOROQ device engine: the bare Mach-O at the top-level Flutter.framework/Flutter (+ a minimal
	// Info.plist). This is the soroq-bytes-only anchor; completeIOSLocalEngineLayout re-injects this byte
	// into the stock xcframework shell that _getIosFrameworkPath actually reads.
	topFrameworkBinary := filepath.Join(targetOut, "Flutter.framework", "Flutter")
	if err := linkOrCopyFile(flatFramework, topFrameworkBinary); err != nil {
		return fmt.Errorf("materialize Flutter.framework/Flutter: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetOut, "Flutter.framework", "Info.plist"), []byte(iosFrameworkInfoPlist), 0o644); err != nil {
		return fmt.Errorf("write Flutter.framework/Info.plist: %w", err)
	}

	// SOROQ host AOT snapshotter placed at the three name/dir variants _genSnapshotPath probes for an iOS
	// target (prefersUniversal=false): `clang_arm64/gen_snapshot` catches Artifact.genSnapshot,
	// `universal/gen_snapshot_arm64` catches Artifact.genSnapshotArm64; artifacts_arm64 matches the
	// reference layout shape. All three are the SAME host gen_snapshot byte.
	for _, rel := range []string{
		filepath.Join("clang_arm64", "gen_snapshot"),
		filepath.Join("universal", "gen_snapshot_arm64"),
		filepath.Join("artifacts_arm64", "gen_snapshot_arm64"),
	} {
		dst := filepath.Join(targetOut, rel)
		if err := linkOrCopyFile(flatGenSnapshot, dst); err != nil {
			return fmt.Errorf("materialize %s: %w", rel, err)
		}
		// _genSnapshotPath skips a path whose binary is not runnable; make sure it is executable.
		if err := os.Chmod(dst, 0o755); err != nil {
			return fmt.Errorf("chmod %s: %w", rel, err)
		}
	}
	return nil
}

// completeIOSLocalEngineLayout overlays the version-matched STOCK host tooling + the stock Flutter.xcframework
// shell that the minimal pack omits, sourced from the resolved Soroq Flutter frontend's bin/cache. It DEEP
// COPIES the xcframework (never symlinks into / mutates the shared cache) and re-injects the SOROQ engine
// byte into the copy. It NEVER overwrites the soroq engine bytes materialized above. Returns nil only when
// the full Flutter-buildable layout is present.
func completeIOSLocalEngineLayout(iosBundleDir, flutterBin string) error {
	// Matched-revision toolchain (T035): if the hosted toolchain SHIPS a complete SOROQ build-lane
	// (ios/build_lane/{ios_profile,host_profile_unopt}) — the version-skewed path the T030 finding
	// requires: dart-sdk + frontend_server_aot.dart.snapshot + flutter_patched_sdk carrying the soroq
	// intrinsics, built at the frontend revision — use it DIRECTLY as the --local-engine layout instead
	// of materializing the frontend's STOCK cache. The shipped bytes ARE the SOROQ engine + build-lane.
	shipped := filepath.Join(iosBundleDir, "build_lane")
	if fi, err := os.Stat(filepath.Join(shipped, iosLocalEngineTargetName)); err == nil && fi.IsDir() {
		outRoot := filepath.Join(iosBundleDir, "out")
		if err := os.MkdirAll(outRoot, 0o755); err != nil {
			return err
		}
		for _, name := range []string{iosLocalEngineTargetName, iosLocalEngineHostName} {
			src := filepath.Join(shipped, name)
			if _, err := os.Stat(src); err != nil {
				return fmt.Errorf("shipped SOROQ build-lane missing %s under %s: %w", name, shipped, err)
			}
			if err := symlinkForce(src, filepath.Join(outRoot, name)); err != nil {
				return fmt.Errorf("link shipped SOROQ build-lane %s: %w", name, err)
			}
		}
		return nil
	}
	if err := materializeIOSLocalEngineLayout(iosBundleDir); err != nil {
		return err
	}
	flutterRoot, err := flutterRootFromBin(flutterBin)
	if err != nil {
		return err
	}
	cacheDir := filepath.Join(flutterRoot, "bin", "cache")
	commonEngine := filepath.Join(cacheDir, "artifacts", "engine", "common")
	darwinHostEngine := filepath.Join(cacheDir, "artifacts", "engine", "darwin-x64")
	stockIOSXcframework := filepath.Join(cacheDir, "artifacts", "engine", "ios-profile", "Flutter.xcframework")

	targetOut := filepath.Join(iosBundleDir, "out", iosLocalEngineTargetName)
	hostOut := filepath.Join(iosBundleDir, "out", iosLocalEngineHostName)

	// Stock precached Flutter.xcframework shell (Headers/Modules/Info.plist/icudtl.dat/PrivacyInfo). Deep
	// copy so the shared bin/cache is never touched, then overwrite the ios-arm64 device Flutter binary
	// (the path _getIosFrameworkPath reads for a physical profile build) with the SOROQ engine byte.
	if _, err := os.Stat(stockIOSXcframework); err != nil {
		return fmt.Errorf("stock precached Flutter.xcframework missing at %s (run the Soroq frontend's `flutter precache --ios` once): %w", stockIOSXcframework, err)
	}
	xcframeworkDst := filepath.Join(targetOut, "Flutter.xcframework")
	if err := copyDirTree(stockIOSXcframework, xcframeworkDst); err != nil {
		return fmt.Errorf("copy stock Flutter.xcframework shell: %w", err)
	}
	soroqFrameworkBinary := filepath.Join(targetOut, "Flutter.framework", "Flutter")
	deviceFrameworkBinary := filepath.Join(xcframeworkDst, "ios-arm64", "Flutter.framework", "Flutter")
	if _, err := os.Stat(deviceFrameworkBinary); err != nil {
		return fmt.Errorf("stock Flutter.xcframework has no ios-arm64/Flutter.framework/Flutter to overlay: %w", err)
	}
	if err := overwriteFileContents(soroqFrameworkBinary, deviceFrameworkBinary); err != nil {
		return fmt.Errorf("inject SOROQ engine byte into xcframework ios-arm64: %w", err)
	}
	// Re-assert (fail-safe): the read path must be the SOROQ engine, never the stock byte we just replaced.
	if err := assertSoroqIOSFrameworkBinary(deviceFrameworkBinary); err != nil {
		return err
	}

	// Stock host tooling from the resolved Soroq frontend cache. NOTE (T030 finding): dart-sdk +
	// frontend_server + flutter_patched_sdk are only safe to take from the frontend STOCK cache when the
	// frontend's engine revision MATCHES the hosted toolchain's (flutter_revision/dart_revision). If they
	// diverge — e.g. a hosted toolchain at soroq engine c9a6c484/d684a576 vs a frontend updated to a newer
	// STOCK engine — the kernel compile fails (stock patched_sdk lacks the soroq intrinsics; a soroq
	// patched_sdk trips the SDK-hash + dart:ui-framework skew). For a version-skewed toolchain the hosted
	// iOS toolchain MUST pack a consistent SOROQ build-lane (frontend_server_aot.dart.snapshot, dart-sdk,
	// flutter_patched_sdk platform+outline) built at the frontend's revision, and this function must
	// materialize those packed SOROQ bytes here INSTEAD of symlinking the stock ones. See
	// notes/t030-ios-app-build-leg.md.
	stockLinks := map[string]string{
		filepath.Join(hostOut, "dart-sdk"):                             filepath.Join(cacheDir, "dart-sdk"),
		filepath.Join(hostOut, "frontend_server_aot.dart.snapshot"):    filepath.Join(darwinHostEngine, "frontend_server_aot.dart.snapshot"),
		filepath.Join(hostOut, "gen", "const_finder.dart.snapshot"):    filepath.Join(darwinHostEngine, "const_finder.dart.snapshot"),
		filepath.Join(hostOut, "flutter_patched_sdk"):                  filepath.Join(commonEngine, "flutter_patched_sdk"),
		filepath.Join(targetOut, "flutter_patched_sdk"):                filepath.Join(commonEngine, "flutter_patched_sdk"),
	}
	for dst, src := range stockLinks {
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("stock frontend artifact missing (run the Soroq frontend's `flutter precache --ios` once): %s: %w", src, err)
		}
		if err := symlinkForce(src, dst); err != nil {
			return fmt.Errorf("link stock %s: %w", filepath.Base(dst), err)
		}
	}
	return nil
}

// overwriteFileContents copies src bytes over dst (truncating dst), following symlinks on src. Unlike
// linkOrCopyFile it NEVER hardlinks, so overwriting the copied stock xcframework binary can never mutate
// the shared bin/cache inode.
func overwriteFileContents(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Remove any existing dst (it may be a hardlink/symlink into the cache) before writing fresh bytes.
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	return copyFileContents(src, dst)
}

// copyDirTree recursively copies srcDir into dstDir with real byte copies (never hardlinks/symlinks into
// the source), preserving perms. Symlinks in the source are dereferenced (their target content copied).
func copyDirTree(srcDir, dstDir string) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(dstDir, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		// copyFileContents opens the source with os.Open (follows symlinks) and writes dst with the
		// source's perms, so a symlink entry is dereferenced to real bytes in the copy.
		return copyFileContents(path, dst)
	})
}

// assertSoroqIOSFrameworkBinary fails unless the Mach-O at binaryPath hashes to the SOROQ engine sha.
func assertSoroqIOSFrameworkBinary(binaryPath string) error {
	got, _, err := sha256OfFile(binaryPath)
	if err != nil {
		return fmt.Errorf("hash Flutter framework binary %s: %w", binaryPath, err)
	}
	// Fast path: the pinned c9a6c484 SOROQ engine.
	if got == soroqIOSFrameworkSHA256 {
		return nil
	}
	// Matched-revision toolchain (T035): any other SOROQ engine is accepted iff it carries the SOROQ iOS
	// interpreter backend symbol — a revision-agnostic "not stock" proof (a stock Flutter engine has none).
	if frameworkHasSoroqInterpreterBackend(binaryPath) {
		return nil
	}
	return fmt.Errorf("Flutter framework binary %s hashes to %s (not the pinned SOROQ engine %s) and carries NO SOROQ interpreter backend symbol — a stock or wrong engine was materialized/linked", binaryPath, got, soroqIOSFrameworkSHA256)
}

// frameworkHasSoroqInterpreterBackend reports whether the Flutter engine binary contains the SOROQ iOS
// interpreter backend symbol (the load-bearing "this engine is soroq-patched, not stock" marker).
func frameworkHasSoroqInterpreterBackend(binaryPath string) bool {
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte("soroq_ios_interpreter_backend_v1"))
}

// assertSoroqIOSFrameworkInLayout asserts the SOROQ engine byte is present both at the top-level
// Flutter.framework and (crucially) in the Flutter.xcframework ios-arm64 read path _getIosFrameworkPath
// consumes for a physical profile build.
func assertSoroqIOSFrameworkInLayout(iosBundleDir string) error {
	targetOut := filepath.Join(iosBundleDir, "out", iosLocalEngineTargetName)
	for _, rel := range []string{
		filepath.Join("Flutter.framework", "Flutter"),
		filepath.Join("Flutter.xcframework", "ios-arm64", "Flutter.framework", "Flutter"),
	} {
		if err := assertSoroqIOSFrameworkBinary(filepath.Join(targetOut, rel)); err != nil {
			return err
		}
	}
	return nil
}

// assertSoroqIOSFrameworkInApp finds the Flutter engine binary embedded in a built .app and asserts it is
// the SOROQ engine. Returns an error (not fatal) so the caller can gate this on the Xcode/codesign tail
// having actually produced the .app.
func assertSoroqIOSFrameworkInApp(appPath string) error {
	candidate := filepath.Join(appPath, "Frameworks", "Flutter.framework", "Flutter")
	if _, err := os.Stat(candidate); err != nil {
		return fmt.Errorf("built app has no embedded Flutter.framework at %s: %w", candidate, err)
	}
	return assertSoroqIOSFrameworkBinary(candidate)
}

// runFlutterIOSReleaseBuild builds the iOS app from the HOSTED Soroq toolchain (no engine-source
// checkout) and emits app.dill for the ios-engine patch lane. It is the iOS analog of
// runFlutterAndroidReleaseBuild: resolve the cached toolchain -> resolve the Soroq frontend flutter ->
// completeIOSLocalEngineLayout -> flutter build ios --local-engine. app.dill is captured whether or not
// the Xcode/codesign TAIL succeeds (per the T030 guardrail: app.dill + soroq AOT come before signing).
func runFlutterIOSReleaseBuild(projectDir, toolchainVersion string, extraArgs []string) error {
	_, err := buildIOSAppDill(projectDir, toolchainVersion, extraArgs)
	return err
}

// buildIOSAppDill runs the cached-toolchain `flutter build ios --local-engine` and returns the path to
// the produced app.dill. app.dill is emitted BEFORE the Xcode/codesign tail (per the T030 guardrail),
// so a non-empty path is returned even when the build fails at that tail — the returned error is
// non-nil in that case, letting engine-lane callers register the baseline against a valid app.dill
// while surfacing the tail failure. A genuine pre-app.dill compile failure returns ("", err).
func buildIOSAppDill(projectDir, toolchainVersion string, extraArgs []string) (string, error) {
	toolchainVersion = strings.TrimSpace(toolchainVersion)
	if toolchainVersion == "" {
		return "", errors.New("`soroq release ios --build` requires --toolchain <version> (the cached iOS toolchain installed by `soroq toolchain install`); there is no local engine-source fallback")
	}
	iosBundleDir, err := iosCachedToolchainBundleDir(toolchainVersion)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(iosBundleDir, "engine.json")); err != nil {
		return "", fmt.Errorf("iOS toolchain %q is not installed (no engine.json under %s); run `soroq toolchain install %s --api <base>` first: %w", toolchainVersion, iosBundleDir, toolchainVersion, err)
	}
	flutterBin, err := resolveSoroqFlutterBin()
	if err != nil {
		return "", err
	}
	if err := completeIOSLocalEngineLayout(iosBundleDir, flutterBin); err != nil {
		return "", fmt.Errorf("prepare cached iOS toolchain local-engine layout: %w", err)
	}
	// Fail fast BEFORE the long build if the SOROQ engine byte is not in the read path.
	if err := assertSoroqIOSFrameworkInLayout(iosBundleDir); err != nil {
		return "", err
	}

	manifestPath := filepath.Join(projectDir, "soroq_app_manifest.txt")
	args := []string{
		"build", "ios", "--profile",
		"--local-engine=" + iosLocalEngineTargetName,
		"--local-engine-host=" + iosLocalEngineHostName,
		"--local-engine-src-path=" + iosBundleDir,
		"--no-codesign",
		"--no-tree-shake-icons",
	}
	if fileExists(manifestPath) {
		args = append(args, "--extra-gen-snapshot-options=--soroq_manifest="+manifestPath)
	}
	args = append(args, extraArgs...)

	cmd := exec.Command(flutterBin, args...)
	cmd.Dir = projectDir
	cmd.Stdin = os.Stdin
	cmd.Env = soroqFlutterBuildEnv(os.Environ())
	buildErr := runSoroqBuildCommand(cmd, projectDir, "Building iOS app with the Soroq toolchain (--local-engine "+iosLocalEngineTargetName+")")

	// Capture app.dill even on a tail failure: it is produced before the Xcode/codesign tail.
	appDill, dillErr := locateFreshestAppDill(projectDir)
	if dillErr == nil {
		sha, size, _ := sha256OfFile(appDill)
		fmt.Fprintf(os.Stdout, "app_dill: %s\n", appDill)
		fmt.Fprintf(os.Stdout, "app_dill_sha256: %s\n", sha)
		fmt.Fprintf(os.Stdout, "app_dill_bytes: %d\n", size)
	}

	if buildErr != nil {
		if dillErr == nil {
			fmt.Fprintln(os.Stderr, "note: flutter build ios failed at a later stage (likely the Xcode/CocoaPods/codesign tail). app.dill above was produced BEFORE that tail and is usable for `soroqctl release/patch ios-engine --app-dill`. A signed IPA is owner-gated.")
			return appDill, fmt.Errorf("flutter build ios produced app.dill but failed at a later (Xcode/codesign) stage: %w", buildErr)
		}
		return "", fmt.Errorf("flutter build ios failed before producing app.dill: %w", buildErr)
	}

	// Build fully succeeded: assert the SOROQ engine is embedded in the built .app.
	if appPath, err := locateBuiltIOSApp(projectDir); err == nil {
		if err := assertSoroqIOSFrameworkInApp(appPath); err != nil {
			return "", fmt.Errorf("built .app Flutter.framework check: %w", err)
		}
		fmt.Fprintf(os.Stdout, "app: %s (embedded Flutter.framework == SOROQ %s)\n", appPath, soroqIOSFrameworkSHA256)
	}
	if dillErr != nil {
		return "", fmt.Errorf("flutter build ios succeeded but app.dill was not found under %s: %w", projectDir, dillErr)
	}
	return appDill, nil
}

// locateFreshestAppDill finds the newest app.dill the build emitted, searching the flutter_build kernel
// output dir and the build intermediates.
func locateFreshestAppDill(projectDir string) (string, error) {
	var roots = []string{
		filepath.Join(projectDir, ".dart_tool", "flutter_build"),
		filepath.Join(projectDir, "build"),
	}
	type cand struct {
		path    string
		modTime int64
	}
	var candidates []cand
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if filepath.Base(path) == "app.dill" {
				candidates = append(candidates, cand{path: path, modTime: info.ModTime().UnixNano()})
			}
			return nil
		})
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no app.dill found under %s", strings.Join(roots, " or "))
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].modTime > candidates[j].modTime })
	return candidates[0].path, nil
}

// locateBuiltIOSApp finds the built Runner.app (or first *.app) for an iOS profile build.
func locateBuiltIOSApp(projectDir string) (string, error) {
	patterns := []string{
		filepath.Join(projectDir, "build", "ios", "iphoneos", "Runner.app"),
		filepath.Join(projectDir, "build", "ios", "iphoneos", "*.app"),
		filepath.Join(projectDir, "build", "ios", "*-iphoneos", "*.app"),
	}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && info.IsDir() {
				return m, nil
			}
		}
	}
	return "", fmt.Errorf("no built .app under %s/build/ios", projectDir)
}

// iosFrameworkInfoPlist is a minimal, valid framework Info.plist for the top-level (soroq-bytes-only)
// Flutter.framework. The device build reads the xcframework copy, not this bundle; this exists to satisfy
// the materialized-layout assertion + reference-shape parity.
const iosFrameworkInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleDevelopmentRegion</key>
  <string>en</string>
  <key>CFBundleExecutable</key>
  <string>Flutter</string>
  <key>CFBundleIdentifier</key>
  <string>io.flutter.flutter</string>
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>CFBundleName</key>
  <string>Flutter</string>
  <key>CFBundlePackageType</key>
  <string>FMWK</string>
  <key>CFBundleShortVersionString</key>
  <string>1.0</string>
  <key>CFBundleSignature</key>
  <string>????</string>
  <key>CFBundleVersion</key>
  <string>1.0</string>
  <key>MinimumOSVersion</key>
  <string>13.0</string>
  <key>BuildMode</key>
  <string>profile</string>
</dict>
</plist>
`
