package main

// dynamic_modules distribution for the public `soroq` CLI (D-iOS-freshening).
//
// The iOS hard-OTA activator imports `package:dynamic_modules/dynamic_modules.dart` — a thin wrapper
// over `dart:_internal` (loadDynamicModule + the soroq redirect/rollback primitives). That package is
// NOT on pub.dev (publish_to: none) and only compiles against the SOROQ-patched dart-sdk the hosted
// toolchain provides. To make a fresh developer buildable WITHOUT a repo checkout or a repo-relative
// path dependency, the CLI ships the package's load-bearing source EMBEDDED in the binary and extracts
// it to a stable per-user absolute path (~/.soroq/dynamic_modules), then wires it into the app's
// pubspec.yaml as a PLAIN dependency (never a dependency_override; absolute path so it resolves from
// any working directory). The developer never configures this.
//
// Embed scope: only lib/dynamic_modules.dart (the sole consumed source; the package's test/ + bin/
// trees are build-time fixtures for the package itself and are intentionally excluded to keep the CLI
// binary small). embedded_dynamic_modules_test.go asserts the embedded copy is byte-identical to
// packages/dynamic_modules/lib/dynamic_modules.dart so the mirror can never silently drift.

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed embedded/dynamic_modules/lib/dynamic_modules.dart
var embeddedDynamicModulesFS embed.FS

const embeddedDynamicModulesLibPath = "embedded/dynamic_modules/lib/dynamic_modules.dart"

// sanitizedDynamicModulesPubspec is the pubspec written to the EXTRACTED copy. The source package
// pubspec carries `resolution: workspace` (only valid inside the mono-repo workspace) and test-only
// dev_dependencies (front_end/kernel/vm/...). A standalone path dependency needs neither — the
// consumed library imports only dart:typed_data + dart:_internal — so the extracted package gets a
// minimal, self-contained pubspec. The load-bearing lib/ source is copied verbatim (sha-checked).
const sanitizedDynamicModulesPubspec = `name: dynamic_modules
# Extracted by the soroq CLI from its embedded copy. This package is not intended
# for consumption on pub.dev. DO NOT publish.
publish_to: none

environment:
  sdk: '^3.12.0-0'
`

// runFlutterPubGet runs `flutter pub get` in projectDir. Overridable in tests.
var runFlutterPubGet = func(projectDir string) error {
	flutterBin, err := resolveSoroqFlutterBin()
	if err != nil {
		if path, lookErr := exec.LookPath("flutter"); lookErr == nil {
			flutterBin = path
		} else {
			return fmt.Errorf("flutter not found for `flutter pub get`; install Flutter or run it manually in %s: %w", projectDir, err)
		}
	}
	cmd := exec.Command(flutterBin, "pub", "get")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func embeddedDynamicModulesLib() ([]byte, error) {
	return embeddedDynamicModulesFS.ReadFile(embeddedDynamicModulesLibPath)
}

func embeddedDynamicModulesLibSHA256() (string, error) {
	data, err := embeddedDynamicModulesLib()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// dynamicModulesInstallDir returns ~/.soroq/dynamic_modules (the per-user extracted package root).
func dynamicModulesInstallDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".soroq", "dynamic_modules"), nil
}

// ensureDynamicModulesInstalled extracts the embedded dynamic_modules package to ~/.soroq/dynamic_modules
// (idempotent: skipped when the on-disk version stamp already matches the embedded lib sha) and ensures
// the app pubspec at projectDir has a PLAIN `dependencies: dynamic_modules: {path: <abs>}` entry, then
// runs `flutter pub get` when anything changed. Returns the absolute install dir.
func ensureDynamicModulesInstalled(projectDir string) (string, error) {
	installDir, err := dynamicModulesInstallDir()
	if err != nil {
		return "", err
	}
	embeddedSHA, err := embeddedDynamicModulesLibSHA256()
	if err != nil {
		return "", err
	}

	extracted, err := extractDynamicModulesIfStale(installDir, embeddedSHA)
	if err != nil {
		return "", err
	}

	pubspecPath := filepath.Join(projectDir, "pubspec.yaml")
	changed, err := ensurePubspecPathDependency(pubspecPath, "dynamic_modules", installDir)
	if err != nil {
		return "", err
	}

	if extracted || changed {
		if err := runFlutterPubGet(projectDir); err != nil {
			return "", fmt.Errorf("flutter pub get after wiring dynamic_modules: %w", err)
		}
	}
	return installDir, nil
}

// extractDynamicModulesIfStale writes the embedded package to installDir when the version stamp is
// missing or does not match embeddedSHA. Returns true when it (re)extracted.
func extractDynamicModulesIfStale(installDir, embeddedSHA string) (bool, error) {
	stampPath := filepath.Join(installDir, ".soroq-version")
	if existing, err := os.ReadFile(stampPath); err == nil && strings.TrimSpace(string(existing)) == embeddedSHA {
		// Already installed at this version; still confirm the lib file is actually present.
		if _, statErr := os.Stat(filepath.Join(installDir, "lib", "dynamic_modules.dart")); statErr == nil {
			return false, nil
		}
	}

	lib, err := embeddedDynamicModulesLib()
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Join(installDir, "lib"), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(installDir, "lib", "dynamic_modules.dart"), lib, 0o644); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(installDir, "pubspec.yaml"), []byte(sanitizedDynamicModulesPubspec), 0o644); err != nil {
		return false, err
	}
	if err := os.WriteFile(stampPath, []byte(embeddedSHA+"\n"), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// ensurePubspecPathDependency guarantees the app pubspec has a PLAIN path dependency
// `<depName>:\n    path: <absPath>` under the top-level `dependencies:` map (never under
// dependency_overrides). Idempotent: returns changed=false when it is already present with the same
// absolute path. It edits the raw text to preserve the rest of the pubspec verbatim.
func ensurePubspecPathDependency(pubspecPath, depName, absPath string) (bool, error) {
	absPath, err := filepath.Abs(absPath)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(pubspecPath)
	if err != nil {
		return false, fmt.Errorf("read pubspec.yaml at %s: %w", pubspecPath, err)
	}
	text := string(raw)
	newText, changed, err := pubspecWithPathDependency(text, depName, absPath)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := os.WriteFile(pubspecPath, []byte(newText), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// pubspecWithPathDependency returns the pubspec text with a plain path dependency inserted/updated
// under the top-level `dependencies:` block. Pure (string-in/string-out) so it is unit-testable.
func pubspecWithPathDependency(text, depName, absPath string) (string, bool, error) {
	lines := strings.Split(text, "\n")
	depBlockStart := -1
	for i, line := range lines {
		if line == "dependencies:" || strings.HasPrefix(line, "dependencies:") && !strings.HasPrefix(line, "dependencies_") {
			trimmed := strings.TrimRight(line, " \t")
			if trimmed == "dependencies:" {
				depBlockStart = i
				break
			}
		}
	}

	desired := []string{
		"  " + depName + ":",
		"    path: " + absPath,
	}

	if depBlockStart == -1 {
		// No top-level dependencies block: append a fresh one (unusual for a Flutter app, but safe).
		block := append([]string{"", "dependencies:"}, desired...)
		if len(text) > 0 && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		return text + strings.Join(block, "\n") + "\n", true, nil
	}

	// Find the extent of the dependencies block: from the line after `dependencies:` until the next
	// top-level (column-0, non-blank, non-comment) key.
	blockEnd := len(lines)
	for i := depBlockStart + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			blockEnd = i
			break
		}
	}

	// Is depName already present in the block?
	depKeyPrefixes := []string{"  " + depName + ":", "  " + depName + " :"}
	for i := depBlockStart + 1; i < blockEnd; i++ {
		line := lines[i]
		matched := false
		for _, p := range depKeyPrefixes {
			if strings.HasPrefix(line, p) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// Determine the existing entry's extent (its more-indented children).
		entryEnd := blockEnd
		for j := i + 1; j < blockEnd; j++ {
			child := lines[j]
			if strings.TrimSpace(child) == "" {
				continue
			}
			// child must be indented deeper than the 2-space dep key to belong to it.
			if len(child)-len(strings.TrimLeft(child, " \t")) <= 2 {
				entryEnd = j
				break
			}
		}
		// Already correct?
		existing := strings.Join(lines[i:entryEnd], "\n")
		if strings.Contains(existing, "path:") && strings.Contains(existing, absPath) {
			return text, false, nil
		}
		// Replace the existing entry with the desired plain path dependency.
		out := append([]string{}, lines[:i]...)
		out = append(out, desired...)
		out = append(out, lines[entryEnd:]...)
		return strings.Join(out, "\n"), true, nil
	}

	// Not present: insert right after the `dependencies:` line.
	out := append([]string{}, lines[:depBlockStart+1]...)
	out = append(out, desired...)
	out = append(out, lines[depBlockStart+1:]...)
	return strings.Join(out, "\n"), true, nil
}
