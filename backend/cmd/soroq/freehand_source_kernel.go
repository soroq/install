package main

// Soroq freehand — source-fidelity (non-AOT) companion kernel for the dual-kernel baseline (v2).
//
// The immutable baseline's app.dill is the AOT/runtime import-dill kernel: it is POST-TFA (whole-program
// tree-shaking + type-flow are baked into declaration bodies), so a semantic diff against it is noisy —
// one edit ripples TFA into hundreds of unrelated "changes". The reliable freehand diff therefore needs
// a NON-AOT, source-fidelity kernel of the exact same customer source + build configuration. This file
// deterministically produces that companion kernel with ONE kernel-only `gen_kernel --no-aot` compile
// (not a second full Flutter/Xcode build) and records the exact recipe so `soroq patch ios --engine` can
// reproduce the identical compilation for the candidate.
//
// TOCTOU: the source kernel MUST be from the same source/config as the AOT app.dill. We capture a digest
// of the customer source tree + config before the AOT build and re-verify it is byte-identical right
// before compiling the source kernel; any drift fails the release BEFORE persistence or registration.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// freehandRecipeSchemaV1 is the legacy source-kernel recipe. Its digest was computed over the WHOLE
	// struct — including package_config_sha256 — so a v1 baseline binds its exact dependency graph into
	// the toolchain digest and CANNOT accept a dependency-changing patch (that is why v1 bases must be
	// rebuilt to enable dependency OTA). v1 baselines still verify correctly under v1 semantics.
	freehandRecipeSchemaV1 = "soroq.freehand.source_kernel_recipe.v1"
	// freehandRecipeSchemaV2 splits the toolchain recipe from the dependency graph: the digest binds
	// ONLY the immutable toolchain/compiler/build-mode inputs (see toolchainRecipeInputsV2). The
	// dependency graph is validated + bound separately via FreehandDependencyDescriptor, so
	// adding/updating a Dart dependency keeps the toolchain recipe digest invariant.
	freehandRecipeSchemaV2 = "soroq.freehand.source_kernel_recipe.v2"
)

// FreehandSourceKernelRecipe is the exact, reproducible recipe for the non-AOT source kernel. Recorded in
// baseline.json so the candidate patch flow compiles its source kernel identically. Its canonical digest
// (recipeDigest) binds the immutable inputs; a candidate compiled with a different recipe is refused.
type FreehandSourceKernelRecipe struct {
	Schema           string   `json:"schema"` // freehandRecipeSchemaV1 / V2
	Entrypoint       string   `json:"entrypoint"`
	Target           string   `json:"target"`            // "flutter"
	BuildMode        string   `json:"build_mode"`        // "profile" (matches the freehand release build)
	Flavor           string   `json:"flavor"`            // "" when none
	PlatformDillRel  string   `json:"platform_dill_rel"` // path relative to flutterRoot
	PlatformDillSHA  string   `json:"platform_dill_sha256"`
	GenKernelSHA     string   `json:"gen_kernel_sha256"` // the gen_kernel (frontend compiler) snapshot bytes
	DartDefines      []string `json:"dart_defines"`      // sorted
	Experiments      []string `json:"experiments"`       // sorted
	PackageConfigSHA string   `json:"package_config_sha256"`
}

// toolchainRecipeInputsV2 is the EXPLICIT, closed set of immutable toolchain/compiler/build-mode inputs
// the v2 recipe digest binds. It deliberately does NOT contain package_config: the dependency graph is a
// separate, mutable input gated by FreehandDependencyDescriptor. Any change to Flutter/Dart/frontend
// (platform dill + gen_kernel snapshot bytes = the compiler revision), the build mode, entrypoint,
// target, flavor, dart-defines, or experiments changes this digest and is refused on patch.
type toolchainRecipeInputsV2 struct {
	Schema          string   `json:"schema"`
	Entrypoint      string   `json:"entrypoint"`
	Target          string   `json:"target"`
	BuildMode       string   `json:"build_mode"`
	Flavor          string   `json:"flavor"`
	PlatformDillRel string   `json:"platform_dill_rel"`
	PlatformDillSHA string   `json:"platform_dill_sha256"`
	GenKernelSHA    string   `json:"gen_kernel_sha256"`
	DartDefines     []string `json:"dart_defines"`
	Experiments     []string `json:"experiments"`
}

// recipeDigest binds the immutable recipe inputs. It dispatches on the recipe schema so existing v1
// baselines verify byte-for-byte under legacy (whole-struct, package_config-inclusive) semantics, while
// v2 baselines bind only toolchainRecipeInputsV2 (package_config excluded → dependency OTA is possible
// while Flutter/Dart/frontend/toolchain/build-mode stay immutable). An unknown schema is rejected.
func (r FreehandSourceKernelRecipe) recipeDigest() (string, error) {
	switch r.Schema {
	case freehandRecipeSchemaV2:
		v2 := toolchainRecipeInputsV2{
			Schema:          r.Schema,
			Entrypoint:      r.Entrypoint,
			Target:          r.Target,
			BuildMode:       r.BuildMode,
			Flavor:          r.Flavor,
			PlatformDillRel: r.PlatformDillRel,
			PlatformDillSHA: r.PlatformDillSHA,
			GenKernelSHA:    r.GenKernelSHA,
			DartDefines:     r.DartDefines,
			Experiments:     r.Experiments,
		}
		b, err := json.Marshal(v2)
		if err != nil {
			return "", err
		}
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:]), nil
	case freehandRecipeSchemaV1, "":
		// Legacy: digest the WHOLE struct (package_config included), exactly as v1 baselines recorded.
		b, err := json.Marshal(r)
		if err != nil {
			return "", err
		}
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:]), nil
	default:
		return "", fmt.Errorf("unknown freehand source-kernel recipe schema %q (expected %s or %s)", r.Schema, freehandRecipeSchemaV1, freehandRecipeSchemaV2)
	}
}

// isV1 reports whether this recipe uses the legacy package_config-inclusive digest (cannot accept a
// dependency-changing patch without a new base release).
func (r FreehandSourceKernelRecipe) isV1() bool {
	return r.Schema == freehandRecipeSchemaV1 || r.Schema == ""
}

// flutterGenKernelSnapshot / flutterVMPlatform / flutterProfilePlatformDill derive tool paths purely from
// flutterRoot so the recipe is reproducible from recorded provenance (no ambient state).
func flutterGenKernelSnapshot(flutterRoot string) string {
	return filepath.Join(flutterRoot, "bin", "cache", "dart-sdk", "bin", "snapshots", "gen_kernel_aot.dart.snapshot")
}
func flutterDartAotRuntime(flutterRoot string) string {
	return filepath.Join(flutterRoot, "bin", "cache", "dart-sdk", "bin", "dartaotruntime")
}

// flutterProfilePlatformDillRel returns the flutter platform_strong.dill (product variant, matching a
// --profile/--release engine) relative path under flutterRoot, preferring the product variant.
func flutterProfilePlatformDillRel(flutterRoot string) (string, error) {
	candidates := []string{
		filepath.Join("bin", "cache", "artifacts", "engine", "common", "flutter_patched_sdk_product", "platform_strong.dill"),
		filepath.Join("bin", "cache", "artifacts", "engine", "common", "flutter_patched_sdk", "platform_strong.dill"),
	}
	for _, rel := range candidates {
		if fileExists(filepath.Join(flutterRoot, rel)) {
			return rel, nil
		}
	}
	return "", fmt.Errorf("no flutter platform_strong.dill under %s (looked in flutter_patched_sdk_product/ and flutter_patched_sdk/)", flutterRoot)
}

// buildFreehandSourceKernelRecipe assembles the reproducible recipe from flutterRoot + the project's
// package_config. Entrypoint/target/build-mode mirror the freehand release build (flutter build ios
// --profile, entrypoint lib/main.dart, no dart-defines/experiments/flavor).
func buildFreehandSourceKernelRecipe(projectDir, flutterRoot string) (FreehandSourceKernelRecipe, error) {
	platRel, err := flutterProfilePlatformDillRel(flutterRoot)
	if err != nil {
		return FreehandSourceKernelRecipe{}, err
	}
	platSHA, err := sha256OfPath(filepath.Join(flutterRoot, platRel))
	if err != nil {
		return FreehandSourceKernelRecipe{}, fmt.Errorf("hash platform dill: %w", err)
	}
	genKernel := flutterGenKernelSnapshot(flutterRoot)
	if !fileExists(genKernel) {
		return FreehandSourceKernelRecipe{}, fmt.Errorf("gen_kernel snapshot missing: %s", genKernel)
	}
	genSHA, err := sha256OfPath(genKernel)
	if err != nil {
		return FreehandSourceKernelRecipe{}, err
	}
	pkgCfgSHA, err := sha256OfPath(filepath.Join(projectDir, ".dart_tool", "package_config.json"))
	if err != nil {
		return FreehandSourceKernelRecipe{}, fmt.Errorf("hash package_config.json: %w", err)
	}
	return FreehandSourceKernelRecipe{
		Schema:           freehandRecipeSchemaV2,
		Entrypoint:       "lib/main.dart",
		Target:           "flutter",
		BuildMode:        "profile",
		Flavor:           "",
		PlatformDillRel:  platRel,
		PlatformDillSHA:  platSHA,
		GenKernelSHA:     genSHA,
		DartDefines:      []string{},
		Experiments:      []string{},
		PackageConfigSHA: pkgCfgSHA,
	}, nil
}

// generateFreehandSourceKernel runs ONE deterministic `gen_kernel --no-aot` compilation per the recipe
// and writes the source-fidelity kernel to outPath. Returns its SHA-256. Deterministic: the same source +
// recipe yields a byte-identical kernel (proven by the acceptance test). No Xcode/gen_snapshot involved.
func generateFreehandSourceKernel(projectDir, flutterRoot string, recipe FreehandSourceKernelRecipe, outPath string) (string, error) {
	aot := flutterDartAotRuntime(flutterRoot)
	if !fileExists(aot) {
		return "", fmt.Errorf("dartaotruntime missing: %s", aot)
	}
	genKernel := flutterGenKernelSnapshot(flutterRoot)
	platform := filepath.Join(flutterRoot, recipe.PlatformDillRel)
	pkgConfig := filepath.Join(projectDir, ".dart_tool", "package_config.json")
	entrypoint := filepath.Join(projectDir, recipe.Entrypoint)
	if !fileExists(entrypoint) {
		return "", fmt.Errorf("entrypoint not found: %s", entrypoint)
	}
	args := []string{
		"--disable-dart-dev", genKernel,
		"--target", recipe.Target,
		"--platform", platform,
		"--packages", pkgConfig,
		"--no-aot", "--no-embed-sources",
		"-Ddart.vm.profile=true", "-Ddart.vm.product=false",
		"--output", outPath,
	}
	for _, d := range recipe.DartDefines {
		args = append(args, "-D"+d)
	}
	for _, e := range recipe.Experiments {
		args = append(args, "--enable-experiment="+e)
	}
	args = append(args, entrypoint)
	cmd := exec.Command(aot, args...)
	cmd.Dir = projectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gen_kernel --no-aot source kernel failed: %w\n%s", err, string(out))
	}
	if !fileExists(outPath) {
		return "", fmt.Errorf("gen_kernel produced no output at %s\n%s", outPath, string(out))
	}
	return sha256OfPath(outPath)
}

// captureSourceStateDigest hashes the DEVELOPER-EDITABLE customer source that determines the kernels:
// every lib/**/*.dart plus pubspec.yaml, sorted by relpath as (relpath \0 sha). It deliberately EXCLUDES
// build-managed artifacts (.dart_tool/package_config.json, pubspec.lock, .flutter-plugins*) which the
// Flutter build regenerates during compilation — including them would false-positive the TOCTOU guard.
// Purpose: prove the developer did not edit source DURING the (long) AOT build, so the source kernel
// (compiled after) matches the AOT app.dill. A mismatch fails the release before persistence.
func captureSourceStateDigest(projectDir string) (string, error) {
	var entries []string
	add := func(rel string) error {
		p := filepath.Join(projectDir, rel)
		if !fileExists(p) {
			return nil
		}
		sha, err := sha256OfPath(p)
		if err != nil {
			return err
		}
		entries = append(entries, rel+"\x00"+sha)
		return nil
	}
	libDir := filepath.Join(projectDir, "lib")
	if fi, err := os.Stat(libDir); err == nil && fi.IsDir() {
		err := filepath.Walk(libDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".dart") {
				return nil
			}
			rel, err := filepath.Rel(projectDir, path)
			if err != nil {
				return err
			}
			return add(rel)
		})
		if err != nil {
			return "", err
		}
	}
	if err := add("pubspec.yaml"); err != nil {
		return "", err
	}
	sort.Strings(entries)
	h := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(h[:]), nil
}

// captureCompilationInputDigest hashes the COMPLETE resolved compilation input set that determines the
// candidate kernel + synthesized module: every reachable app + local/path-package Dart source (incl.
// generated *.g.dart under lib/), pubspec.yaml, pubspec.lock, and .dart_tool/package_config.json. Unlike
// captureSourceStateDigest (the release-build TOCTOU guard, which excludes build-regenerated files), this
// is captured AFTER dependency resolution has settled (patch flow, no long build) and re-verified
// immediately before module generation; any drift fails the patch closed. Package sources whose rootUri
// is under /.pub-cache/ or a flutter SDK path are EXCLUDED (immutable, pinned by pubspec.lock).
func captureCompilationInputDigest(projectDir string) (string, error) {
	var entries []string
	addFile := func(absPath, label string) error {
		if !fileExists(absPath) {
			return nil
		}
		sha, err := sha256OfPath(absPath)
		if err != nil {
			return err
		}
		entries = append(entries, label+"\x00"+sha)
		return nil
	}
	hashDartTree := func(root, labelPrefix string) error {
		fi, err := os.Stat(root)
		if err != nil || !fi.IsDir() {
			return nil
		}
		return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".dart") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			return addFile(path, labelPrefix+"/"+filepath.ToSlash(rel))
		})
	}
	// 1. app lib/ (incl. generated *.g.dart) + pubspec.yaml/lock + package_config.json
	if err := hashDartTree(filepath.Join(projectDir, "lib"), "app:lib"); err != nil {
		return "", err
	}
	for _, rel := range []string{"pubspec.yaml", "pubspec.lock", filepath.Join(".dart_tool", "package_config.json")} {
		if err := addFile(filepath.Join(projectDir, rel), "cfg:"+rel); err != nil {
			return "", err
		}
	}
	// 2. local/path-package Dart sources (mutable dependencies) — exclude pub-cache + flutter SDK.
	// The package config is REQUIRED and must be well-formed: a missing/malformed config means the
	// resolved compilation inputs are unknown -> fail closed (never hash a partial input set).
	pkgCfg := filepath.Join(projectDir, ".dart_tool", "package_config.json")
	b, err := os.ReadFile(pkgCfg)
	if err != nil {
		return "", fmt.Errorf("package_config.json missing/unreadable at %s (run pub get): %w", pkgCfg, err)
	}
	{
		var pc struct {
			Packages []struct {
				Name    string `json:"name"`
				RootURI string `json:"rootUri"`
			} `json:"packages"`
		}
		if jerr := json.Unmarshal(b, &pc); jerr != nil {
			return "", fmt.Errorf("malformed package_config.json at %s: %w", pkgCfg, jerr)
		}
		if len(pc.Packages) == 0 {
			return "", fmt.Errorf("package_config.json at %s has no packages (unresolved project)", pkgCfg)
		}
		{
			for _, p := range pc.Packages {
				root := p.RootURI
				root = strings.TrimPrefix(root, "file://")
				if !filepath.IsAbs(root) {
					root = filepath.Clean(filepath.Join(filepath.Dir(pkgCfg), root))
				}
				if strings.Contains(root, "/.pub-cache/") || strings.Contains(root, "/flutter/packages/") ||
					strings.Contains(root, "/flutter-sdk") || strings.Contains(root, "/bin/cache/") {
					continue // immutable dependency, pinned by pubspec.lock
				}
				if filepath.Clean(root) == filepath.Clean(projectDir) {
					continue // the app itself, already hashed
				}
				if err := hashDartTree(filepath.Join(root, "lib"), "path:"+p.Name); err != nil {
					return "", err
				}
			}
		}
	}
	sort.Strings(entries)
	h := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(h[:]), nil
}
