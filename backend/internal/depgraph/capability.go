package depgraph

// Capability classification — "can this package be delivered by a code-only OTA patch?".
//
// Pubspec declarations ALONE are not sufficient: a package can ship a build hook, a prebuilt .framework,
// or an undeclared native platform directory without ever writing `flutter: plugin:`. So classification
// reads BOTH the package's declared metadata AND its actual on-disk contents, and fails closed when the
// two disagree. The complementary signal — what actually landed in the built app — is compared separately
// by the build-output comparison (see buildoutput.go), because a package can also acquire native content
// through a transitive change this static scan cannot see.
//
// The one thing that is explicitly NOT a rejection: importing Flutter. A Dart-only Flutter package such as
// flutter_riverpod ships no native code and no assets, and stays eligible.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Capability is the OTA-eligibility classification of one package.
type Capability struct {
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons,omitempty"`

	HasNativePlugin bool   `json:"has_native_plugin"`
	NativeDetail    string `json:"native_detail,omitempty"`

	HasAssets   bool     `json:"has_assets"`
	AssetDetail []string `json:"asset_detail,omitempty"`

	// HasBuildHook is a native/FFI build hook (hook/build.dart, hook/link.dart, legacy build.dart):
	// code that compiles native artifacts at pub-get/build time.
	HasBuildHook    bool   `json:"has_build_hook"`
	BuildHookDetail string `json:"build_hook_detail,omitempty"`

	// NativeArtifacts are package-relative paths of native evidence actually found on disk
	// (platform dirs, native build files, compiled libraries, native sources). Capped + sorted.
	NativeArtifacts []string `json:"native_artifacts,omitempty"`

	// MetadataInconsistent means the declared plugin metadata contradicts the on-disk contents
	// (undeclared native code, or a declared native plugin with no native sources). Fails closed.
	MetadataInconsistent bool   `json:"metadata_inconsistent"`
	InconsistencyDetail  string `json:"inconsistency_detail,omitempty"`

	// ImportsFlutter is recorded for transparency and is NEVER a rejection signal.
	ImportsFlutter bool `json:"imports_flutter"`
}

// digestString is the canonical, order-stable serialization of the FULL classification bound into the
// graph digest — so a forged "eligible: true" cannot survive a digest recomputation.
func (c Capability) digestString() string {
	return fmt.Sprintf("elig=%t|native=%t|%s|assets=%t|%s|hook=%t|%s|inconsistent=%t|%s|artifacts=%s|flutter=%t|reasons=%s",
		c.Eligible, c.HasNativePlugin, c.NativeDetail, c.HasAssets, strings.Join(c.AssetDetail, ";"),
		c.HasBuildHook, c.BuildHookDetail, c.MetadataInconsistent, c.InconsistencyDetail,
		strings.Join(c.NativeArtifacts, ";"), c.ImportsFlutter, strings.Join(c.Reasons, ";"))
}

type pubspecRoot struct {
	Dependencies map[string]yaml.Node `yaml:"dependencies"`
	Flutter      *flutterSection      `yaml:"flutter"`
}
type flutterSection struct {
	Plugin  *pluginSection `yaml:"plugin"`
	Assets  []yaml.Node    `yaml:"assets"`
	Fonts   []yaml.Node    `yaml:"fonts"`
	Shaders []yaml.Node    `yaml:"shaders"`
}
type pluginSection struct {
	Platforms map[string]map[string]yaml.Node `yaml:"platforms"`
	// legacy single-platform form:
	PluginClass    string `yaml:"pluginClass"`
	AndroidPackage string `yaml:"androidPackage"`
}

// nativePlatformDirs are package-root directories that exist to hold platform-native build content.
var nativePlatformDirs = []string{"android", "ios", "macos", "windows", "linux", "ohos"}

// nativeBuildFiles are package-root/basename markers of a native build system.
var nativeBuildFiles = map[string]bool{
	"build.gradle": true, "build.gradle.kts": true, "settings.gradle": true,
	"CMakeLists.txt": true, "Package.swift": true, "Cargo.toml": true,
	"Android.mk": true, "Application.mk": true, "Makefile": true,
}

// nativeBinaryExts are compiled/linkable artifacts a code-only patch can never deliver.
var nativeBinaryExts = map[string]bool{
	".so": true, ".dylib": true, ".a": true, ".o": true, ".dll": true, ".lib": true,
	".jar": true, ".aar": true, ".framework": true, ".xcframework": true, ".bundle": true,
}

// nativeSourceExts are native language sources; their presence in a shipped package means the package
// contributes compiled code to the app binary.
var nativeSourceExts = map[string]bool{
	".swift": true, ".m": true, ".mm": true, ".c": true, ".cc": true, ".cpp": true, ".cxx": true,
	".h": true, ".hpp": true, ".java": true, ".kt": true, ".kts": true, ".rs": true,
}

// scanSkipDirs are directories that are NOT shipped package content: an `example/ios/` folder or a
// package's own tests must not make a pure-Dart package look native.
var scanSkipDirs = map[string]bool{
	"example": true, "examples": true, "test": true, "tests": true, "doc": true, "docs": true,
	"benchmark": true, "benchmarks": true, "tool": true, "tools": true, "integration_test": true,
	".git": true, ".dart_tool": true, "build": true, ".github": true, ".idea": true, ".vscode": true,
	"node_modules": true,
}

const maxRecordedArtifacts = 12

// classifyPackage is the capability policy. A package is ELIGIBLE for a code-only OTA patch only when it
// declares AND contains no native platform code, no native/FFI build hook, and no packaged Flutter
// assets/fonts/shaders — and its declared metadata is consistent with its actual contents.
//
// pubspecBytes is authoritative for declarations; rootDir is scanned for the contents. rootDir may be ""
// (declaration-only classification, used by unit tests and by callers that only have the pubspec).
func classifyPackage(pubspecBytes []byte, rootDir string, source SourceType) Capability {
	var ps pubspecRoot
	c := Capability{Eligible: true}
	if err := yaml.Unmarshal(pubspecBytes, &ps); err != nil {
		return Capability{Eligible: false, Reasons: []string{"pubspec.yaml parse error — cannot verify the package is Dart-only"}}
	}
	if _, ok := ps.Dependencies["flutter"]; ok {
		c.ImportsFlutter = true
	}

	declaresNativePlugin := false
	if ps.Flutter != nil {
		if native, detail := pluginIsNative(ps.Flutter.Plugin); native {
			declaresNativePlugin = true
			c.HasNativePlugin = true
			c.NativeDetail = detail
			c.Eligible = false
			c.Reasons = append(c.Reasons, "declares native platform plugin code ("+detail+")")
		}
		for _, a := range []struct {
			label string
			nodes []yaml.Node
		}{
			{"flutter.assets", ps.Flutter.Assets},
			{"flutter.fonts", ps.Flutter.Fonts},
			{"flutter.shaders", ps.Flutter.Shaders},
		} {
			if len(a.nodes) > 0 {
				c.HasAssets = true
				c.AssetDetail = append(c.AssetDetail, fmt.Sprintf("%d %s entr(y/ies)", len(a.nodes), a.label))
				c.Eligible = false
				c.Reasons = append(c.Reasons, "declares packaged "+a.label)
			}
		}
	}

	// SDK packages (flutter, sky_engine) are part of the pinned toolchain, not deliverable content. The
	// toolchain itself is bound by the source-kernel recipe digest, so they need no content scan: scanning
	// them would flag the engine's own native sources and refuse every patch.
	if source == SourceSDK {
		c.Reasons = append(c.Reasons, "sdk package (pinned by the toolchain recipe digest; not deliverable content)")
		return c
	}

	if rootDir == "" {
		return c
	}

	scan := scanNativeContent(rootDir)
	c.NativeArtifacts = scan.artifacts
	if scan.buildHook != "" {
		c.HasBuildHook = true
		c.BuildHookDetail = scan.buildHook
		c.Eligible = false
		c.Reasons = append(c.Reasons, "ships a native/FFI build hook ("+scan.buildHook+") that compiles native artifacts at build time")
	}
	if len(scan.artifacts) > 0 && !c.HasNativePlugin {
		// Undeclared native content: the pubspec says pure Dart, the package says otherwise.
		c.HasNativePlugin = true
		c.NativeDetail = "undeclared native content: " + strings.Join(scan.artifacts, ", ")
		c.MetadataInconsistent = true
		c.InconsistencyDetail = "package contains native platform content but declares no `flutter: plugin:` section"
		c.Eligible = false
		c.Reasons = append(c.Reasons, "contains native platform content not declared in its pubspec ("+strings.Join(scan.artifacts, ", ")+")")
	}
	if declaresNativePlugin && len(scan.artifacts) == 0 {
		// Declared native plugin with no native content on disk: the metadata cannot be trusted either way.
		c.MetadataInconsistent = true
		c.InconsistencyDetail = "package declares a native `flutter: plugin:` implementation but ships no native platform content"
		c.Eligible = false
		c.Reasons = append(c.Reasons, "plugin metadata is inconsistent with package contents ("+c.InconsistencyDetail+")")
	}
	return c
}

// ClassifyPubspecForTest exposes declaration-only classification for tests and for callers that hold a
// pubspec without a resolved package directory.
func ClassifyPubspecForTest(pubspecBytes []byte) Capability {
	return classifyPackage(pubspecBytes, "", SourceHosted)
}

type nativeScan struct {
	artifacts []string
	buildHook string
}

// scanNativeContent walks a package's SHIPPED content looking for native platform evidence: platform
// directories, native build files, compiled libraries, native sources, and native/FFI build hooks.
// example/, test/ and tooling directories are excluded — they are not shipped as part of the package.
func scanNativeContent(rootDir string) nativeScan {
	var s nativeScan
	seen := map[string]bool{}
	add := func(rel string) {
		if seen[rel] || len(s.artifacts) >= maxRecordedArtifacts {
			return
		}
		seen[rel] = true
		s.artifacts = append(s.artifacts, rel)
	}

	// Native/FFI build hooks (package root only — that is where the SDK looks for them).
	for _, hook := range []string{
		filepath.Join("hook", "build.dart"),
		filepath.Join("hook", "link.dart"),
		"build.dart",
	} {
		if fi, err := os.Lstat(filepath.Join(rootDir, hook)); err == nil && fi.Mode().IsRegular() {
			s.buildHook = filepath.ToSlash(hook)
			break
		}
	}

	// Package-root native platform directories.
	for _, d := range nativePlatformDirs {
		p := filepath.Join(rootDir, d)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() && dirHasContent(p) {
			add(d + "/")
		}
	}

	_ = filepath.WalkDir(rootDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || len(s.artifacts) >= maxRecordedArtifacts {
			if len(s.artifacts) >= maxRecordedArtifacts {
				return filepath.SkipAll
			}
			return nil //nolint:nilerr // an unreadable subtree must not abort classification
		}
		rel, rerr := filepath.Rel(rootDir, p)
		if rerr != nil || rel == "." {
			return nil
		}
		if d.IsDir() {
			if scanSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		name := d.Name()
		ext := strings.ToLower(filepath.Ext(name))
		switch {
		case strings.HasSuffix(strings.ToLower(name), ".podspec"):
			add(relSlash)
		case nativeBuildFiles[name]:
			add(relSlash)
		case nativeBinaryExts[ext]:
			add(relSlash)
		case nativeSourceExts[ext]:
			add(relSlash)
		}
		return nil
	})

	sort.Strings(s.artifacts)
	return s
}

func dirHasContent(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(ents) > 0
}

// pluginIsNative reports whether a `flutter: plugin:` section declares NATIVE platform code. A platform
// entry that ONLY declares `dartPluginClass` (a federated Dart-only implementation) is NOT native.
func pluginIsNative(pl *pluginSection) (bool, string) {
	if pl == nil {
		return false, ""
	}
	// Legacy single-platform plugin form (pre-federated): pluginClass/androidPackage ⇒ native.
	if strings.TrimSpace(pl.PluginClass) != "" || strings.TrimSpace(pl.AndroidPackage) != "" {
		return true, "legacy plugin.pluginClass/androidPackage"
	}
	nativeFields := []string{"pluginClass", "ffiPlugin", "sharedDarwinSource"}
	var hits []string
	for _, plat := range sortedKeys(pl.Platforms) {
		entry := pl.Platforms[plat]
		for _, f := range nativeFields {
			if node, ok := entry[f]; ok {
				var v any
				_ = node.Decode(&v)
				if isNativeFieldTruthy(f, v) {
					hits = append(hits, plat+"."+f)
				}
			}
		}
	}
	if len(hits) > 0 {
		return true, "plugin.platforms: " + strings.Join(hits, ", ")
	}
	return false, ""
}

func isNativeFieldTruthy(field string, v any) bool {
	switch field {
	case "ffiPlugin", "sharedDarwinSource":
		b, ok := v.(bool)
		return ok && b
	default: // pluginClass: any non-empty string names a native class
		s, ok := v.(string)
		return ok && strings.TrimSpace(s) != ""
	}
}
