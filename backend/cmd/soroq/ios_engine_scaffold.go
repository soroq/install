package main

// iOS engine-lane build scaffold generator (D-iOS-freshening).
//
// Turns the owner-facing soroq.yaml `ios_engine.patchable` list into the three build artifacts the
// iOS hard-OTA lane needs, so a fresh developer never hand-writes them:
//
//   soroq_app_manifest.txt        stable patchable-fn identities, in soroq.yaml order, one per line.
//                                 Passed to the patched gen_snapshot as --soroq_manifest at build time
//                                 and recorded (by sha) into the immutable engine-lane baseline.
//   lib/soroq_patch_table.g.dart  `final List<Function> soroqPatchTable` with DIRECT references to the
//                                 patchable functions (so tree-shaking keeps them), same order.
//   lib/soroq_activator.dart      the thin EngineActivator binding (dynamic_modules primitive + the
//                                 generated table). It carries ZERO OTA policy.
//
// Stable identity format (must match the patched gen_snapshot's --soroq_manifest reader exactly):
//   top-level  lib/f.dart#fn        -> package:<pubspec-name>/f.dart::::fn
//   static     lib/f.dart#Cls.m     -> package:<pubspec-name>/f.dart::Cls::m
// The leading `lib/` segment is stripped from the package URI path.
//
// Validation is deliberately strict: only top-level functions and static methods are patchable. Instance
// methods, getters/setters, closures, and generic/specialized functions are rejected per-entry with a
// clear error, because the redirect primitive binds a plain tear-off by stable identity.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// patchableEntry is one resolved soroq.yaml ios_engine.patchable item.
type patchableEntry struct {
	Raw        string // as written in soroq.yaml, e.g. "lib/ota_demo.dart#AppInfo.channel"
	RelPath    string // "lib/ota_demo.dart"
	ImportPath string // relative-to-lib import for the generated table, e.g. "ota_demo.dart"
	Class      string // "" for top-level, else "AppInfo"
	Symbol     string // "demoLabel" or "channel"
	Identity   string // stable manifest identity
	TableRef   string // "demoLabel" or "AppInfo.channel"
}

// generateIOSEngineScaffold reads soroq.yaml ios_engine.patchable, validates every entry against the
// project's Dart source, and writes soroq_app_manifest.txt, lib/soroq_patch_table.g.dart and
// lib/soroq_activator.dart. It returns the absolute path to the written manifest.
func generateIOSEngineScaffold(projectDir string) (string, error) {
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}
	pubspecPath := filepath.Join(absDir, "pubspec.yaml")
	pubBytes, err := os.ReadFile(pubspecPath)
	if err != nil {
		return "", fmt.Errorf("read pubspec.yaml at %s: %w", pubspecPath, err)
	}
	packageName := strings.TrimSpace(parseTopLevelYaml(pubBytes)["name"])
	if packageName == "" {
		return "", fmt.Errorf("pubspec.yaml at %s is missing a top-level package name", pubspecPath)
	}

	soroqPath := filepath.Join(absDir, "soroq.yaml")
	soroqBytes, err := os.ReadFile(soroqPath)
	if err != nil {
		return "", fmt.Errorf("read soroq.yaml at %s: %w", soroqPath, err)
	}
	enabled, rawItems, err := parseIOSEnginePatchable(soroqBytes)
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", fmt.Errorf("soroq.yaml at %s does not enable ios_engine (set ios_engine.enabled: true)", soroqPath)
	}
	if len(rawItems) == 0 {
		return "", fmt.Errorf("soroq.yaml ios_engine.patchable is empty; list at least one `lib/f.dart#fn` entry")
	}

	entries, err := resolvePatchableEntries(absDir, packageName, rawItems)
	if err != nil {
		return "", err
	}

	manifestPath := filepath.Join(absDir, "soroq_app_manifest.txt")
	if err := writeManifestFile(manifestPath, entries); err != nil {
		return "", err
	}
	if err := writePatchTableFile(filepath.Join(absDir, "lib", "soroq_patch_table.g.dart"), entries); err != nil {
		return "", err
	}
	if err := writeActivatorFile(filepath.Join(absDir, "lib", "soroq_activator.dart")); err != nil {
		return "", err
	}

	// Cross-check: table length == manifest length, order matches, every entry present.
	if err := crossCheckScaffold(manifestPath, entries); err != nil {
		return "", err
	}
	return manifestPath, nil
}

// resolvePatchableEntries validates and maps each raw `lib/f.dart#Sym` item to a patchableEntry,
// preserving soroq.yaml order and rejecting duplicates.
func resolvePatchableEntries(projectDir, packageName string, rawItems []string) ([]patchableEntry, error) {
	entries := make([]patchableEntry, 0, len(rawItems))
	seen := map[string]string{}
	for _, raw := range rawItems {
		entry, err := resolvePatchableEntry(projectDir, packageName, raw)
		if err != nil {
			return nil, err
		}
		if prev, ok := seen[entry.Identity]; ok {
			return nil, fmt.Errorf("duplicate patchable identity %q (from %q and %q); each function may appear once", entry.Identity, prev, raw)
		}
		seen[entry.Identity] = raw
		entries = append(entries, entry)
	}
	return entries, nil
}

func resolvePatchableEntry(projectDir, packageName, raw string) (patchableEntry, error) {
	var zero patchableEntry
	trimmed := strings.TrimSpace(raw)
	relPath, symbolSpec, ok := strings.Cut(trimmed, "#")
	if !ok {
		return zero, fmt.Errorf("patchable entry %q must be of the form lib/file.dart#Symbol or lib/file.dart#Class.method", raw)
	}
	relPath = strings.TrimSpace(relPath)
	symbolSpec = strings.TrimSpace(symbolSpec)
	if relPath == "" || symbolSpec == "" {
		return zero, fmt.Errorf("patchable entry %q must name a Dart file and a symbol", raw)
	}
	if filepath.IsAbs(relPath) || strings.Contains(relPath, "..") {
		return zero, fmt.Errorf("patchable entry %q must use a project-relative path under lib/", raw)
	}
	if !strings.HasSuffix(relPath, ".dart") {
		return zero, fmt.Errorf("patchable entry %q must reference a .dart file", raw)
	}
	relSlash := filepath.ToSlash(relPath)
	if !strings.HasPrefix(relSlash, "lib/") {
		return zero, fmt.Errorf("patchable entry %q must reference a file under lib/ (got %q)", raw, relPath)
	}

	absFile := filepath.Join(projectDir, filepath.FromSlash(relSlash))
	src, err := os.ReadFile(absFile)
	if err != nil {
		return zero, fmt.Errorf("patchable entry %q: cannot read %s: %w", raw, absFile, err)
	}

	className, symbol := "", symbolSpec
	if strings.Contains(symbolSpec, ".") {
		parts := strings.Split(symbolSpec, ".")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return zero, fmt.Errorf("patchable entry %q: static symbol must be Class.method", raw)
		}
		className, symbol = parts[0], parts[1]
	}
	if !isSimpleDartIdentifier(symbol) || (className != "" && !isSimpleDartIdentifier(className)) {
		return zero, fmt.Errorf("patchable entry %q: symbol/class must be a plain Dart identifier", raw)
	}

	// The package URI path strips the leading lib/.
	uriPath := strings.TrimPrefix(relSlash, "lib/")
	importPath := uriPath

	var identity, tableRef string
	if className == "" {
		if err := validateTopLevelFunction(string(src), symbol, raw); err != nil {
			return zero, err
		}
		identity = fmt.Sprintf("package:%s/%s::::%s", packageName, uriPath, symbol)
		tableRef = symbol
	} else {
		if err := validateStaticMethod(string(src), className, symbol, raw); err != nil {
			return zero, err
		}
		identity = fmt.Sprintf("package:%s/%s::%s::%s", packageName, uriPath, className, symbol)
		tableRef = className + "." + symbol
	}

	return patchableEntry{
		Raw:        raw,
		RelPath:    relSlash,
		ImportPath: importPath,
		Class:      className,
		Symbol:     symbol,
		Identity:   identity,
		TableRef:   tableRef,
	}, nil
}

var simpleDartIdentifierRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func isSimpleDartIdentifier(s string) bool {
	return simpleDartIdentifierRE.MatchString(s)
}

// stripDartCommentsAndStrings removes // and /* */ comments and string literal bodies so the focused
// declaration parser cannot be fooled by a symbol name that only appears in a comment or a string. It
// preserves newlines so line-anchored regexes still see top-level (column-0) structure.
func stripDartCommentsAndStrings(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	runes := []rune(src)
	i := 0
	n := len(runes)
	for i < n {
		c := runes[i]
		// Line comment.
		if c == '/' && i+1 < n && runes[i+1] == '/' {
			for i < n && runes[i] != '\n' {
				i++
			}
			continue
		}
		// Block comment.
		if c == '/' && i+1 < n && runes[i+1] == '*' {
			i += 2
			for i < n && !(runes[i] == '*' && i+1 < n && runes[i+1] == '/') {
				if runes[i] == '\n' {
					b.WriteRune('\n')
				}
				i++
			}
			i += 2
			continue
		}
		// String literal (single/double, incl. simple raw). Body is replaced with spaces.
		if c == '\'' || c == '"' {
			quote := c
			b.WriteRune(' ')
			i++
			for i < n && runes[i] != quote {
				if runes[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if runes[i] == '\n' {
					b.WriteRune('\n')
				}
				i++
			}
			i++ // closing quote
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(c)
		i++
	}
	return b.String()
}

// validateTopLevelFunction confirms symbol is declared as a top-level (column-0) function in src, and
// is NOT a getter/setter and NOT generic. Returns a clear per-entry error otherwise.
func validateTopLevelFunction(src, symbol, raw string) error {
	clean := stripDartCommentsAndStrings(src)
	// Getter/setter at top level: `... get symbol` / `... set symbol(`.
	getterRE := regexp.MustCompile(`(?m)^[^\s].*\bget\s+` + regexp.QuoteMeta(symbol) + `\b`)
	setterRE := regexp.MustCompile(`(?m)^[^\s].*\bset\s+` + regexp.QuoteMeta(symbol) + `\b`)
	if getterRE.MatchString(clean) || setterRE.MatchString(clean) {
		return fmt.Errorf("patchable entry %q: %q is a top-level getter/setter, which is not patchable (only plain top-level functions and static methods are)", raw, symbol)
	}
	// Generic top-level function: `symbol<...>(`.
	genericRE := regexp.MustCompile(`(?m)^(?:[\w$<>,.?\[\] ]+\s+)?` + regexp.QuoteMeta(symbol) + `\s*<[^>]*>\s*\(`)
	if genericRE.MatchString(clean) {
		return fmt.Errorf("patchable entry %q: top-level function %q is generic/specialized, which is not patchable", raw, symbol)
	}
	// Plain top-level function definition: column 0, optional return type, name, `(`.
	fnRE := regexp.MustCompile(`(?m)^(?:[\w$<>,.?\[\] ]+\s+)?` + regexp.QuoteMeta(symbol) + `\s*\(`)
	loc := fnRE.FindStringIndex(clean)
	if loc == nil {
		return fmt.Errorf("patchable entry %q: no top-level function %q found in %s", raw, symbol, raw[:strings.Index(raw, "#")])
	}
	// Guard: the matched line must not be a getter/setter/keyword-led non-definition. Reject if the
	// token immediately before the name is `get`/`set` (already handled) or if it looks like a class.
	line := lineContaining(clean, loc[0])
	if regexp.MustCompile(`^\s*(class|enum|mixin|extension|typedef)\b`).MatchString(line) {
		return fmt.Errorf("patchable entry %q: %q is not a top-level function", raw, symbol)
	}
	return nil
}

// validateStaticMethod confirms className is a class in src and symbol is a STATIC method of it (not an
// instance method, getter/setter, or generic method).
func validateStaticMethod(src, className, symbol, raw string) error {
	clean := stripDartCommentsAndStrings(src)
	body, ok := classBody(clean, className)
	if !ok {
		return fmt.Errorf("patchable entry %q: no class %q found in %s", raw, className, raw[:strings.Index(raw, "#")])
	}
	q := regexp.QuoteMeta(symbol)
	// Static getter/setter -> reject.
	if regexp.MustCompile(`(?m)^\s*static\s+.*\bget\s+`+q+`\b`).MatchString(body) ||
		regexp.MustCompile(`(?m)^\s*static\s+.*\bset\s+`+q+`\b`).MatchString(body) {
		return fmt.Errorf("patchable entry %q: %s.%s is a static getter/setter, which is not patchable", raw, className, symbol)
	}
	// Static generic method -> reject.
	if regexp.MustCompile(`(?m)^\s*static\s+(?:[\w$<>,.?\[\] ]+\s+)?` + q + `\s*<[^>]*>\s*\(`).MatchString(body) {
		return fmt.Errorf("patchable entry %q: static method %s.%s is generic/specialized, which is not patchable", raw, className, symbol)
	}
	// Static plain method.
	if regexp.MustCompile(`(?m)^\s*static\s+(?:[\w$<>,.?\[\] ]+\s+)?` + q + `\s*\(`).MatchString(body) {
		return nil
	}
	// Present but NOT static -> instance method (explicit rejection).
	instanceRE := regexp.MustCompile(`(?m)^\s*(?:[\w$<>,.?\[\] @]+\s+)?` + q + `\s*(?:<[^>]*>\s*)?\(`)
	if instanceRE.MatchString(body) {
		return fmt.Errorf("patchable entry %q: %s.%s is an instance method, which is not patchable (mark it `static` or use a top-level function)", raw, className, symbol)
	}
	// Getter/setter without static.
	if regexp.MustCompile(`(?m)^\s*(?:[\w$<>,.?\[\] ]+\s+)?get\s+`+q+`\b`).MatchString(body) ||
		regexp.MustCompile(`(?m)^\s*(?:[\w$<>,.?\[\] ]+\s+)?set\s+`+q+`\b`).MatchString(body) {
		return fmt.Errorf("patchable entry %q: %s.%s is a getter/setter, which is not patchable", raw, className, symbol)
	}
	return fmt.Errorf("patchable entry %q: no static method %q found in class %s", raw, symbol, className)
}

// classBody returns the brace-balanced body of `class <name>` (excluding the outer braces).
func classBody(clean, name string) (string, bool) {
	re := regexp.MustCompile(`(?m)^\s*(?:abstract\s+|base\s+|final\s+|sealed\s+|interface\s+|mixin\s+)*class\s+` + regexp.QuoteMeta(name) + `\b`)
	loc := re.FindStringIndex(clean)
	if loc == nil {
		return "", false
	}
	open := strings.IndexByte(clean[loc[1]:], '{')
	if open < 0 {
		return "", false
	}
	start := loc[1] + open
	depth := 0
	for i := start; i < len(clean); i++ {
		switch clean[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return clean[start+1 : i], true
			}
		}
	}
	return "", false
}

func lineContaining(s string, idx int) string {
	start := strings.LastIndexByte(s[:idx], '\n') + 1
	end := strings.IndexByte(s[idx:], '\n')
	if end < 0 {
		return s[start:]
	}
	return s[start : idx+end]
}

// parseIOSEnginePatchable extracts `ios_engine.enabled` and the ordered `ios_engine.patchable` list
// from soroq.yaml. It is a focused (indentation-aware) parser for exactly the documented shape:
//
//	ios_engine:
//	  enabled: true
//	  patchable:
//	    - lib/ota_demo.dart#demoLabel
//	    - lib/ota_demo.dart#AppInfo.channel
func parseIOSEnginePatchable(soroqBytes []byte) (enabled bool, items []string, err error) {
	lines := strings.Split(string(soroqBytes), "\n")
	inBlock := false
	blockIndent := 0
	inPatchable := false
	patchableIndent := -1
	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		if !inBlock {
			if trimmed == "ios_engine:" && indent == 0 {
				inBlock = true
				blockIndent = indent
			}
			continue
		}

		// A key at or below the block indent (and not the block itself) ends the block.
		if indent <= blockIndent {
			break
		}

		if inPatchable {
			if indent > patchableIndent && strings.HasPrefix(trimmed, "-") {
				item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
				item = strings.Trim(item, `"'`)
				if item != "" {
					items = append(items, item)
				}
				continue
			}
			// Any non-list line at/under the patchable key indent ends the patchable list.
			inPatchable = false
		}

		key, value, _ := strings.Cut(trimmed, ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `"'`))
		switch key {
		case "enabled":
			enabled = strings.EqualFold(value, "true")
		case "patchable":
			inPatchable = true
			patchableIndent = indent
		}
	}
	return enabled, items, nil
}

func writeManifestFile(path string, entries []patchableEntry) error {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Identity)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writePatchTableFile(path string, entries []patchableEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Unique imports in first-seen order.
	var imports []string
	seenImport := map[string]bool{}
	for _, e := range entries {
		if !seenImport[e.ImportPath] {
			seenImport[e.ImportPath] = true
			imports = append(imports, e.ImportPath)
		}
	}
	sort.Strings(imports)

	var b strings.Builder
	b.WriteString("// GENERATED — do not edit. Produced by `soroq release/patch ios --engine`.\n")
	b.WriteString("// The patch table references each patchable function DIRECTLY so tree-shaking retains it.\n")
	b.WriteString("// Order MUST match soroq_app_manifest.txt (soroq.yaml ios_engine.patchable order).\n")
	b.WriteString("// ignore_for_file: directives_ordering, unused_import\n\n")
	for _, imp := range imports {
		b.WriteString(fmt.Sprintf("import '%s';\n", imp))
	}
	b.WriteString("\nfinal List<Function> soroqPatchTable = <Function>[\n")
	for _, e := range entries {
		b.WriteString("  " + e.TableRef + ",\n")
	}
	b.WriteString("];\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// generatedActivatorSource is the thin EngineActivator binding, emitted verbatim (with a generated
// header). It contains ZERO OTA policy: no network, no signature/hash checks, no manifest parsing, no
// rollout/rollback-decision logic. It only binds the dynamic_modules primitive + the generated table.
const generatedActivatorSource = `// GENERATED — do not edit. Produced by ` + "`soroq release/patch ios --engine`" + `.
// The thin iOS engine-binding activator — the ONLY app-side apply code. It contains ZERO OTA policy:
// no network, no signature/hash checks, no manifest parsing, no rollout/rollback/quarantine decisions,
// no URLs/keys. It only binds the SDK-bundled dynamic_modules primitive + the app's generated
// soroqPatchTable to the package's SoroqEngineActivator interface.
import 'dart:typed_data';

import 'package:dynamic_modules/dynamic_modules.dart'
    show loadModuleFromBytes, soroqRedirectToPatch, soroqRollbackPatch;
import 'package:soroq_flutter/soroq_flutter.dart' show SoroqEngineActivator;

import 'soroq_patch_table.g.dart'; // build-generated: List<Function> soroqPatchTable

class EngineActivator implements SoroqEngineActivator {
  final Object _owner = Object();

  @override
  Future<Object?> loadModule(Uint8List bytecode) =>
      loadModuleFromBytes(bytecode);

  @override
  void redirect(int index, Object? module) =>
      soroqRedirectToPatch(_owner, soroqPatchTable[index], module!);

  @override
  void rollbackToBase() {
    for (final fn in soroqPatchTable) {
      soroqRollbackPatch(_owner, fn);
    }
  }
}
`

func writeActivatorFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(generatedActivatorSource), 0o644)
}

// crossCheckScaffold re-reads the written manifest and asserts table length == manifest length, order
// matches, and every resolved entry is present — a belt-and-braces guard against generator drift.
func crossCheckScaffold(manifestPath string, entries []patchableEntry) error {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifestLines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			manifestLines = append(manifestLines, strings.TrimSpace(l))
		}
	}
	if len(manifestLines) != len(entries) {
		return fmt.Errorf("scaffold cross-check failed: manifest has %d identities but %d patchable entries were resolved", len(manifestLines), len(entries))
	}
	for i, e := range entries {
		if manifestLines[i] != e.Identity {
			return fmt.Errorf("scaffold cross-check failed at index %d: manifest %q != entry %q", i, manifestLines[i], e.Identity)
		}
	}
	return nil
}

// manifestSHA256 returns the hex sha256 of the manifest file bytes (matches soroqctl's sha256File).
func manifestSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
