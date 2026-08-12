package main

// dynamic_modules distribution for the public `soroq` CLI (D-iOS-freshening).
//
// The iOS hard-OTA activator imports `package:dynamic_modules/dynamic_modules.dart` — a thin wrapper
// over `dart:_internal` (loadDynamicModule + the soroq redirect/rollback primitives). That package is
// NOT on pub.dev (publish_to: none) and only compiles against the SOROQ-patched dart-sdk the hosted
// toolchain provides. To make a fresh developer buildable WITHOUT a repo checkout or a repo-relative
// path dependency, the CLI ships the package's load-bearing source EMBEDDED in the binary and extracts
// it to a stable per-user path (~/.soroq/dynamic_modules), content-addressed by the embedded source sha.
//
// It is made RESOLVABLE by soroq_package_config.go, which resolves it in a throwaway workspace and
// installs the result as build output. This file used to wire it into the customer's pubspec.yaml
// instead; that put a machine-specific absolute path into a committed file and rewrote pubspec.lock,
// which left the developer's own Flutter unable to resolve the project. Nothing here touches the
// customer's pubspec any more — pubspecWithPathDependency is applied only to the workspace COPY.
//
// Embed scope: only lib/dynamic_modules.dart (the sole consumed source; the package's test/ + bin/
// trees are build-time fixtures for the package itself and are intentionally excluded to keep the CLI
// binary small). embedded_dynamic_modules_test.go asserts the embedded copy is byte-identical to
// packages/dynamic_modules/lib/dynamic_modules.dart so the mirror can never silently drift.

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"os"
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
