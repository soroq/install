package main

// Android multi-ABI builds.
//
// Soroq builds Android through Flutter's `--local-engine`, which stock Flutter limits to ONE ABI: the
// local engine's. The published toolchains were therefore arm64-v8a only, and every Soroq app shipped
// without armeabi-v7a and x86_64 — so Play never offered it to 32-bit ARM or x86_64 devices.
//
// A multi-ABI stack needs two things, and the CLI builds several ABIs only when it has both:
//
//   - a TOOLCHAIN that declares `soroq_android_abis` in its engine.json and carries, per extra ABI,
//     libflutter_<abi>.so, libflutter_unstripped_<abi>.so and gen_snapshot_<abi> (arm64 keeps the flat
//     names, so an arm64-only toolchain is read exactly as before);
//   - a FRONTEND whose flutter_tools resolves each extra ABI's engine jar and gen_snapshot from a
//     sibling `out/android_release_<cpu>` (declared by bin/cache/soroq/multi_abi_v1.json).
//
// With either missing, the build is arm64-v8a only, as it always was.
//
// A patch builds exactly its base release's ABIs, whatever the stack could do: a patch that adds or
// drops an ABI changes native libraries, which the code-patch lane refuses.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"soroq/backend/internal/androidrelease"
)

type androidABIInfo struct {
	ABI            string // the Android ABI name: arm64-v8a
	CPU            string // the engine out-dir suffix: android_release_<cpu>
	MavenABI       string // the Gradle engine artifact: io.flutter:<maven>_release
	TargetPlatform string // the flutter --target-platform value
}

// androidABITable is in Flutter's own --target-platform order.
var androidABITable = []androidABIInfo{
	{ABI: "armeabi-v7a", CPU: "arm", MavenABI: "armeabi_v7a", TargetPlatform: "android-arm"},
	{ABI: "arm64-v8a", CPU: "arm64", MavenABI: "arm64_v8a", TargetPlatform: "android-arm64"},
	{ABI: "x86_64", CPU: "x64", MavenABI: "x86_64", TargetPlatform: "android-x64"},
}

const (
	androidPrimaryABI             = "arm64-v8a"
	androidPrimaryTargetPlatform  = "android-arm64"
	androidFrontendMultiABIMarker = "multi_abi_v1.json"
)

func androidABIInfoFor(abi string) (androidABIInfo, bool) {
	for _, info := range androidABITable {
		if info.ABI == strings.TrimSpace(abi) {
			return info, true
		}
	}
	return androidABIInfo{}, false
}

func androidABIInfoForPlatform(targetPlatform string) (androidABIInfo, bool) {
	for _, info := range androidABITable {
		if info.TargetPlatform == strings.TrimSpace(targetPlatform) {
			return info, true
		}
	}
	return androidABIInfo{}, false
}

// androidToolchainABIArtifacts names an ABI's engine artifacts in the flat toolchain bundle.
func androidToolchainABIArtifacts(abi string) (stripped, unstripped, genSnapshot string) {
	if abi == androidPrimaryABI {
		return "libflutter.so", "libflutter_unstripped.so", "gen_snapshot"
	}
	return "libflutter_" + abi + ".so", "libflutter_unstripped_" + abi + ".so", "gen_snapshot_" + abi
}

// androidToolchainABIs reads the ABIs an installed Android toolchain bundle declares, in table order.
// A toolchain that declares none is the historical arm64-v8a-only toolchain.
func androidToolchainABIs(bundleDir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, "engine.json"))
	if errors.Is(err, os.ErrNotExist) {
		// No declaration to read: only the flat arm64 engine can be assumed.
		return []string{androidPrimaryABI}, nil
	}
	if err != nil {
		return nil, err
	}
	var engine struct {
		ABIs []string `json:"soroq_android_abis"`
	}
	if err := json.Unmarshal(raw, &engine); err != nil {
		return nil, fmt.Errorf("engine.json: %w", err)
	}
	if len(engine.ABIs) == 0 {
		return []string{androidPrimaryABI}, nil
	}
	declared := map[string]bool{}
	for _, abi := range engine.ABIs {
		if _, ok := androidABIInfoFor(abi); !ok {
			return nil, fmt.Errorf("engine.json declares unsupported Android ABI %q", abi)
		}
		declared[strings.TrimSpace(abi)] = true
	}
	if !declared[androidPrimaryABI] {
		return nil, fmt.Errorf("engine.json soroq_android_abis does not include %s", androidPrimaryABI)
	}
	var out []string
	for _, info := range androidABITable {
		if declared[info.ABI] {
			out = append(out, info.ABI)
		}
	}
	return out, nil
}

// frontendSupportsAndroidMultiABI reports whether the frontend's flutter_tools can build several ABIs
// against one local engine.
func frontendSupportsAndroidMultiABI(flutterBin string) bool {
	root, err := flutterRootFromBin(flutterBin)
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(root, "bin", "cache", "soroq", androidFrontendMultiABIMarker))
	if err != nil {
		return false
	}
	var marker struct {
		Schema string `json:"schema"`
	}
	return json.Unmarshal(raw, &marker) == nil && marker.Schema == "soroq.frontend_capability.multi_abi.v1"
}

// androidBuildablePlatforms is every --target-platform this engine source + frontend can build.
func androidBuildablePlatforms(source androidEngineSource, flutterBin string) ([]string, error) {
	if source.Kind != androidEngineSourceCachedToolchain {
		return []string{androidPrimaryTargetPlatform}, nil
	}
	abis, err := androidToolchainABIs(source.BundleDir)
	if err != nil {
		return nil, fmt.Errorf("android toolchain %s: %w", source.BundleDir, err)
	}
	if len(abis) == 1 || !frontendSupportsAndroidMultiABI(flutterBin) {
		return []string{androidPrimaryTargetPlatform}, nil
	}
	platforms := make([]string, 0, len(abis))
	for _, abi := range abis {
		info, _ := androidABIInfoFor(abi)
		platforms = append(platforms, info.TargetPlatform)
	}
	return platforms, nil
}

// explicitAndroidTargetPlatforms returns the --target-platform values the args name, or nil.
func explicitAndroidTargetPlatforms(args []string) []string {
	var platforms []string
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		var value string
		switch {
		case arg == "--target-platform" && i+1 < len(args):
			value = args[i+1]
			i++
		case strings.HasPrefix(arg, "--target-platform="):
			value = strings.TrimPrefix(arg, "--target-platform=")
		default:
			continue
		}
		for _, p := range strings.Split(value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				platforms = append(platforms, p)
			}
		}
	}
	return platforms
}

// resolveAndroidTargetPlatformArgs makes the build's ABIs explicit. With no --target-platform, a
// multi-ABI stack builds every ABI it has; an arm64-only stack is left to the historical arm64 default.
// A named ABI the stack cannot build is refused: stock --local-engine would silently build arm64 only.
func resolveAndroidTargetPlatformArgs(extraArgs []string, source androidEngineSource, flutterBin string) ([]string, error) {
	if source.Kind != androidEngineSourceCachedToolchain {
		return extraArgs, nil
	}
	buildable, err := androidBuildablePlatforms(source, flutterBin)
	if err != nil {
		return nil, err
	}
	explicit := explicitAndroidTargetPlatforms(extraArgs)
	if len(explicit) == 0 {
		if len(buildable) == 1 {
			return extraArgs, nil
		}
		return append(append([]string{}, extraArgs...), "--target-platform", strings.Join(buildable, ",")), nil
	}
	can := map[string]bool{}
	for _, p := range buildable {
		can[p] = true
	}
	for _, p := range explicit {
		if !can[p] {
			return nil, fmt.Errorf("--target-platform %s: this Android toolchain and Soroq Flutter frontend can build only %s", p, strings.Join(buildable, ","))
		}
	}
	return extraArgs, nil
}

// androidTargetPlatformsForABIs maps artifact ABIs (e.g. a base release's) to --target-platform values.
func androidTargetPlatformsForABIs(abis []string) ([]string, error) {
	have := map[string]bool{}
	for _, abi := range abis {
		if _, ok := androidABIInfoFor(abi); !ok {
			return nil, fmt.Errorf("unsupported Android ABI %q", abi)
		}
		have[strings.TrimSpace(abi)] = true
	}
	var out []string
	for _, info := range androidABITable {
		if have[info.ABI] {
			out = append(out, info.TargetPlatform)
		}
	}
	return out, nil
}

// withBaseTargetPlatforms pins a patch build to its base release's ABIs, unless the caller named
// them; a named set that differs from the base is refused here rather than by the native-drift check
// after a full build.
func withBaseTargetPlatforms(args []string, baseABIs []string) ([]string, error) {
	base, err := androidTargetPlatformsForABIs(baseABIs)
	if err != nil {
		return nil, fmt.Errorf("base release: %w", err)
	}
	if len(base) == 0 {
		return args, nil
	}
	explicit := explicitAndroidTargetPlatforms(args)
	if len(explicit) == 0 {
		return append(append([]string{}, args...), "--target-platform", strings.Join(base, ",")), nil
	}
	if !sameStringSet(explicit, base) {
		return nil, fmt.Errorf("--target-platform %s differs from the base release's ABIs (%s); a patch must build exactly the base's ABIs", strings.Join(explicit, ","), strings.Join(base, ","))
	}
	return args, nil
}

func sameStringSet(a, b []string) bool {
	set := map[string]int{}
	for _, v := range a {
		set[v]++
	}
	for _, v := range b {
		set[v]--
	}
	for _, n := range set {
		if n != 0 {
			return false
		}
	}
	return len(a) == len(b)
}

// materializeAndroidExtraABIs lays out each extra ABI's engine as a sibling local-engine out dir,
// the way the frontend's multi-ABI flutter_tools reads it: the stripped and unstripped libflutter.so,
// gen_snapshot, and the Gradle engine jar carrying the symbol-bearing library.
func materializeAndroidExtraABIs(androidBundleDir string) error {
	abis, err := androidToolchainABIs(androidBundleDir)
	if err != nil {
		return err
	}
	for _, abi := range abis {
		if abi == androidPrimaryABI {
			continue
		}
		info, _ := androidABIInfoFor(abi)
		stripped, unstripped, genSnapshot := androidToolchainABIArtifacts(abi)
		for _, name := range []string{stripped, unstripped, genSnapshot} {
			if _, err := os.Stat(filepath.Join(androidBundleDir, name)); err != nil {
				return fmt.Errorf("android toolchain declares %s but %s is missing: %w", abi, name, err)
			}
		}
		out := filepath.Join(androidBundleDir, "out", "android_release_"+info.CPU)
		if err := linkOrCopyFile(filepath.Join(androidBundleDir, stripped), filepath.Join(out, "lib.stripped", "libflutter.so")); err != nil {
			return fmt.Errorf("materialize %s lib.stripped/libflutter.so: %w", abi, err)
		}
		if err := linkOrCopyFile(filepath.Join(androidBundleDir, unstripped), filepath.Join(out, "libflutter.so")); err != nil {
			return fmt.Errorf("materialize %s libflutter.so: %w", abi, err)
		}
		if err := linkOrCopyFile(filepath.Join(androidBundleDir, genSnapshot), filepath.Join(out, "universal", "gen_snapshot")); err != nil {
			return fmt.Errorf("materialize %s universal/gen_snapshot: %w", abi, err)
		}
		if err := writeAndroidEngineJar(filepath.Join(androidBundleDir, unstripped), filepath.Join(out, info.MavenABI+"_release.jar"), abi); err != nil {
			return fmt.Errorf("materialize %s_release.jar: %w", info.MavenABI, err)
		}
	}
	return nil
}

// materializeAndroidExtraABIMaven writes each extra ABI's engine POM + maven-metadata beside its jar,
// at the same version as the primary's.
func materializeAndroidExtraABIMaven(androidBundleDir, version string) error {
	abis, err := androidToolchainABIs(androidBundleDir)
	if err != nil {
		return err
	}
	for _, abi := range abis {
		if abi == androidPrimaryABI {
			continue
		}
		info, _ := androidABIInfoFor(abi)
		out := filepath.Join(androidBundleDir, "out", "android_release_"+info.CPU)
		artifact := info.MavenABI + "_release"
		if err := os.WriteFile(filepath.Join(out, artifact+".pom"), []byte(androidEmbeddingPOM(artifact, version)), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, artifact+".maven-metadata.xml"), []byte(androidEmbeddingMavenMetadata(artifact, version)), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// incompleteAndroidABIs lists the ABIs an artifact ships native code for without the Flutter engine
// and the app (libflutter.so + libapp.so). Android picks an app's ABI from the lib/<abi>/ directories
// it contains, so a device of such an ABI installs the app and crashes at launch, and Play offers the
// app to it. A plugin's own library (e.g. libdartjni.so) is enough to create the directory.
func incompleteAndroidABIs(snapshot *androidrelease.Snapshot) []string {
	have := map[string]map[string]bool{}
	for _, entry := range snapshot.NativeLibs {
		parts := strings.Split(entry.Path, "/")
		if len(parts) != 3 || parts[0] != "lib" {
			continue
		}
		if have[parts[1]] == nil {
			have[parts[1]] = map[string]bool{}
		}
		have[parts[1]][parts[2]] = true
	}
	var out []string
	for abi, libs := range have {
		if !libs["libflutter.so"] || !libs["libapp.so"] {
			out = append(out, abi)
		}
	}
	sort.Strings(out)
	return out
}

// guardIncompleteAndroidABIs refuses (strict) or warns about an artifact with engine-less ABIs.
func guardIncompleteAndroidABIs(snapshot *androidrelease.Snapshot, strict bool) error {
	bad := incompleteAndroidABIs(snapshot)
	if len(bad) == 0 {
		return nil
	}
	msg := fmt.Sprintf("the artifact ships native libraries for %s without libflutter.so and libapp.so; a device of that ABI would install the app and crash at launch, and Play would offer it to such devices", strings.Join(bad, ", "))
	if strict {
		return fmt.Errorf("%s. Build every ABI (omit --target-platform) or check the project's abiFilters", msg)
	}
	fmt.Fprintf(os.Stderr, "warning: %s.\n  A Soroq Flutter frontend that declares multi_abi_v1 filters these out; this one does not.\n", msg)
	return nil
}
