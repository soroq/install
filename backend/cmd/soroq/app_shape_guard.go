package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Application shape: Soroq supports a standard full Flutter application.
//
// An add-to-app / hybrid project is a different product shape, not a variation of the same one. Its
// Dart code runs inside a host app that Soroq never built, its engine is created and owned by host
// code (possibly several engines via FlutterEngineGroup), and there is no Soroq-registered base
// artifact whose identity a patch can be bound to. Patch activation on a clean start also means
// something different when the host process outlives every engine.
//
// None of that is verified, and until this guard existed none of it was even detected: a module
// project would release and patch exactly like a full app, producing a release bound to an identity
// the host application does not share. Refuse it explicitly instead.

// flutterProjectIsModule reports whether pubspec.yaml declares a Flutter MODULE (add-to-app), which is
// the `module:` key nested under the top-level `flutter:` key.
func flutterProjectIsModule(pubspec []byte) bool {
	inFlutter := false
	for _, raw := range strings.Split(string(pubspec), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !startsWithIndent(line) {
			// A new top-level key ends any previous block.
			inFlutter = strings.HasPrefix(trimmed, "flutter:")
			continue
		}
		if inFlutter && (trimmed == "module:" || strings.HasPrefix(trimmed, "module:")) {
			return true
		}
	}
	return false
}

// guardSupportedApplicationShape refuses shapes Soroq has no evidence for, before a release is built
// or registered.
// guardSupportedApplicationShape is the PLATFORM-NEUTRAL shape check. Add-to-app is refused on every
// route because the reason is platform-neutral: the host application was built by someone else's
// toolchain, so there is no Soroq-registered base artifact for a patch to bind to.
//
// It deliberately does NOT include the iOS host inspection. That check reads ios/Runner sources, and
// running it on an Android build would refuse an Android release with a message naming
// ios/Runner/AppDelegate.swift — a file that has nothing to do with the artifact being built. A
// refusal a developer cannot act on is worse than no refusal, because it teaches them to reach for
// the opt-in escape hatch.
func guardSupportedApplicationShape(projectDir string) error {
	return guardNotAFlutterModule(projectDir)
}

// guardSupportedIOSApplicationShape is the iOS-route check: the platform-neutral rules plus the iOS
// host inspection. Wired only into routes that actually build or patch an iOS artifact.
func guardSupportedIOSApplicationShape(projectDir string) error {
	if err := guardSupportedApplicationShape(projectDir); err != nil {
		return err
	}
	return guardSingleEngineIOSHost(projectDir)
}

// guardNotAFlutterModule is the original add-to-app refusal. It runs FIRST so a module project keeps
// producing the add-to-app message even when its host sources would also trip the multi-engine check.
func guardNotAFlutterModule(projectDir string) error {
	pubspec, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		// A missing pubspec is reported by the caller's own project validation.
		return nil
	}
	if !flutterProjectIsModule(pubspec) {
		return nil
	}
	if os.Getenv(unverifiedBuildFlagsOptInEnv) == "1" {
		fmt.Fprintf(os.Stderr,
			"warning: this is a Flutter MODULE (add-to-app). Soroq has no acceptance evidence for hybrid\n"+
				"  applications; you have opted in via %s.\n", unverifiedBuildFlagsOptInEnv)
		return nil
	}
	return fmt.Errorf(`this project is a Flutter module (add-to-app), which Soroq does not support

%s/pubspec.yaml declares "module:" under "flutter:", so this Dart code runs inside a host application
that Soroq did not build. Soroq binds a patch to the identity of a base artifact it registered, and in
an add-to-app project there is no such artifact: the host app is built and shipped by its own
toolchain, its engine is created by host code (possibly several via FlutterEngineGroup), and
"activate on next clean start" does not mean the same thing when the host process outlives the engine.

None of that is verified, so releasing here would bind a release to an identity the host app does not
share. Supported today: a standard full Flutter application.

If you want to experiment anyway, accepting that the result is unverified:

    %s=1 soroq <your command>`, strings.TrimRight(projectDir, "/"), unverifiedBuildFlagsOptInEnv)
}

// ---------------------------------------------------------------------------------------------------
// MULTI-ENGINE / MULTI-WINDOW iOS HOSTS
//
// A full Flutter application can still be an unsupported shape: nothing stops its iOS host from
// creating a second FlutterEngine, using a FlutterEngineGroup, or opting into UIScene multi-window.
// pubspec.yaml says nothing about any of that, so the add-to-app guard above sees an ordinary app and
// lets it through.
//
// The evidence lives in the iOS host sources, so that is where this looks. Parsing ios/Runner is not a
// layering violation here: it is the only place the fact is written down.
//
// What goes wrong with several engines is the same thing that goes wrong add-to-app, for the same
// reason. Soroq registers ONE base artifact, binds a patch to that identity, and activates it on a
// clean start. With several engines each one loads its own isolate, they start and stop at different
// times, and the HOST decides which of them runs which entrypoint. Which engine a patch lands in,
// whether an engine that was already running keeps executing base code, and what "next clean start"
// means when the process outlives every engine are all unanswered — and a wrong answer does not
// announce itself, it just runs the wrong code in one window.
//
// LIMITS, stated plainly: this is a static read of ios/Runner. An engine created at runtime behind a
// condition, by a plugin, or from a target this does not glob is NOT detected. Absence of a refusal is
// therefore not a certificate of single-engine-ness; it only means Soroq found no evidence to the
// contrary. Detection deliberately fails OPEN so an ordinary single-engine app is never blocked.

// unsupportedIOSHostShape is one concrete piece of evidence: the file it was read from (project
// relative) and the exact token found there, so a refusal can name both.
type unsupportedIOSHostShape struct {
	kind  string // "engine-group" | "multiple-engines" | "multiple-scenes"
	file  string
	token string
	count int // constructions found, for the multiple-engines case
}

// iosHostSourceFiles returns the iOS host sources worth parsing, in a stable order so the refusal text
// never depends on directory iteration order.
func iosHostSourceFiles(projectDir string) []string {
	runner := filepath.Join(projectDir, "ios", "Runner")
	var out []string
	for _, pattern := range []string{"*.swift", "*.m", "*.mm"} {
		matches, err := filepath.Glob(filepath.Join(runner, pattern))
		if err != nil {
			continue
		}
		out = append(out, matches...)
	}
	sort.Strings(out)
	return out
}

// isSourceIdentifierByte reports whether b can appear inside a Swift/Objective-C identifier.
func isSourceIdentifierByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// stripSourceCommentsAndStrings blanks out `//` line comments, `/* */` block comments (nesting, as
// Swift allows) and string literals, replacing them with spaces so byte offsets are preserved.
//
// This is what keeps the guard from firing on a project that only MENTIONS FlutterEngineGroup — in a
// comment explaining why it does not use one, or in a log message. Escapes inside string literals are
// honoured, because a naive quote toggle would end the literal at \" and hand the rest of it back as
// code, which is the one way this could fail CLOSED and refuse an ordinary app.
//
// Exotic Swift literal forms (multi-line """, raw #"..."#, \(interpolation)) are not modelled
// precisely; they degrade toward blanking more text, i.e. toward NOT refusing.
func stripSourceCommentsAndStrings(src string) string {
	out := []byte(src)
	blank := func(from, to int) {
		for i := from; i < to && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				blank(i, len(src))
				return string(out)
			}
			blank(i, i+end)
			i += end
		case strings.HasPrefix(src[i:], "/*"):
			depth, j := 1, i+2
			for j < len(src) && depth > 0 {
				switch {
				case strings.HasPrefix(src[j:], "/*"):
					depth++
					j += 2
				case strings.HasPrefix(src[j:], "*/"):
					depth--
					j += 2
				default:
					j++
				}
			}
			blank(i, j)
			i = j
		case strings.HasPrefix(src[i:], `"""`):
			j := i + 3
			for j < len(src) {
				if src[j] == '\\' {
					j += 2
					continue
				}
				if strings.HasPrefix(src[j:], `"""`) {
					j += 3
					break
				}
				j++
			}
			blank(i, j)
			i = j
		case src[i] == '"':
			j := i + 1
			for j < len(src) {
				if src[j] == '\\' {
					j += 2
					continue
				}
				if src[j] == '"' {
					j++
					break
				}
				if src[j] == '\n' {
					break // an unterminated literal ends at the newline; do not swallow the file
				}
				j++
			}
			blank(i, j)
			i = j
		default:
			i++
		}
	}
	return string(out)
}

// identifierOffsets returns every offset in code where name appears as a WHOLE identifier, so
// FlutterEngineGroup does not count as FlutterEngine and MyFlutterEngineGroupHelper does not count as
// FlutterEngineGroup.
func identifierOffsets(code, name string) []int {
	var found []int
	for i := 0; ; {
		idx := strings.Index(code[i:], name)
		if idx < 0 {
			return found
		}
		at := i + idx
		end := at + len(name)
		beforeOK := at == 0 || !isSourceIdentifierByte(code[at-1])
		afterOK := end >= len(code) || !isSourceIdentifierByte(code[end])
		if beforeOK && afterOK {
			found = append(found, at)
		}
		i = end
	}
}

// countFlutterEngineConstructions counts engine CONSTRUCTIONS, not mentions: `FlutterEngine(...)` in
// Swift and `[FlutterEngine alloc]` in Objective-C. A stored `var engine: FlutterEngine?` or a cast is
// a mention and must not count, or every single-engine host would look like two.
func countFlutterEngineConstructions(code string) int {
	n := 0
	for _, at := range identifierOffsets(code, "FlutterEngine") {
		rest := strings.TrimLeft(code[at+len("FlutterEngine"):], " \t\r\n")
		if strings.HasPrefix(rest, "(") || strings.HasPrefix(rest, "alloc") {
			n++
		}
	}
	return n
}

// plistDeclaresMultipleScenes reports whether an Info.plist opts the app into UIScene multi-window:
// a UIApplicationSceneManifest whose UIApplicationSupportsMultipleScenes is <true/>.
//
// Deliberately literal about the XML plist Xcode writes. A <false/>, an absent key (Apple defaults it
// false) and a BINARY plist all read as "no evidence" — fail open, never refuse on a guess.
func plistDeclaresMultipleScenes(plist []byte) bool {
	s := string(plist)
	if !strings.Contains(s, "<key>UIApplicationSceneManifest</key>") {
		return false
	}
	const key = "<key>UIApplicationSupportsMultipleScenes</key>"
	idx := strings.Index(s, key)
	if idx < 0 {
		return false
	}
	rest := strings.TrimSpace(s[idx+len(key):])
	return strings.HasPrefix(rest, "<true/>") || strings.HasPrefix(rest, "<true />")
}

// detectUnsupportedIOSHostShape returns the first piece of multi-engine/multi-window evidence in the
// iOS host, or nil. Order is fixed (engine group, then engine count, then scenes) and files are walked
// sorted, so the same project always produces the same refusal.
func detectUnsupportedIOSHostShape(projectDir string) *unsupportedIOSHostShape {
	sources := iosHostSourceFiles(projectDir)
	code := make(map[string]string, len(sources))
	for _, path := range sources {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		code[path] = stripSourceCommentsAndStrings(string(raw))
	}
	rel := func(path string) string {
		if r, err := filepath.Rel(projectDir, path); err == nil {
			return filepath.ToSlash(r)
		}
		return filepath.ToSlash(path)
	}

	for _, path := range sources {
		if len(identifierOffsets(code[path], "FlutterEngineGroup")) > 0 {
			return &unsupportedIOSHostShape{kind: "engine-group", file: rel(path), token: "FlutterEngineGroup"}
		}
	}

	total, firstFile := 0, ""
	for _, path := range sources {
		if n := countFlutterEngineConstructions(code[path]); n > 0 {
			total += n
			if firstFile == "" {
				firstFile = rel(path)
			}
		}
	}
	if total >= 2 {
		return &unsupportedIOSHostShape{kind: "multiple-engines", file: firstFile, token: "FlutterEngine", count: total}
	}

	plistPath := filepath.Join(projectDir, "ios", "Runner", "Info.plist")
	if raw, err := os.ReadFile(plistPath); err == nil && plistDeclaresMultipleScenes(raw) {
		return &unsupportedIOSHostShape{kind: "multiple-scenes", file: rel(plistPath), token: "UIApplicationSceneManifest"}
	}
	return nil
}

// guardSingleEngineIOSHost refuses a host that shows evidence of more than one Flutter engine, or of a
// scene-based multi-window app, before a release is built or a patch is compiled.
func guardSingleEngineIOSHost(projectDir string) error {
	shape := detectUnsupportedIOSHostShape(projectDir)
	if shape == nil {
		return nil
	}
	if os.Getenv(unverifiedBuildFlagsOptInEnv) == "1" {
		fmt.Fprintf(os.Stderr,
			"warning: %s names %q, so this host is not the single-engine application Soroq has evidence\n"+
				"  for; you have opted in via %s.\n", shape.file, shape.token, unverifiedBuildFlagsOptInEnv)
		return nil
	}

	var evidence, mechanism string
	switch shape.kind {
	case "engine-group":
		evidence = fmt.Sprintf(`%s uses %q, which exists for exactly one purpose: running SEVERAL
Flutter engines in one process.`, shape.file, shape.token)
		mechanism = `Soroq registers ONE base artifact, binds a patch to that identity, and activates it on a clean
start. With an engine group each engine loads its own isolate from its own entrypoint, the engines
start and stop at different times, and the host — not Soroq — decides which of them runs which code.
Which engine a patch lands in, whether an engine that was already running keeps executing base code,
and what "activate on next clean start" means when the process outlives every engine are all
unanswered here.`
	case "multiple-engines":
		evidence = fmt.Sprintf(`%s constructs %q %d times — Swift "FlutterEngine(...)" or Objective-C
"[FlutterEngine alloc]" — so this host runs more than one Flutter engine.`,
			shape.file, shape.token, shape.count)
		mechanism = `Soroq registers ONE base artifact, binds a patch to that identity, and activates it on a clean
start. With several engines each one loads its own isolate from its own entrypoint, they start and
stop at different times, and the host — not Soroq — decides which of them runs which code. Which
engine a patch lands in, whether an engine that was already running keeps executing base code, and
what "activate on next clean start" means when the process outlives every engine are all unanswered
here.`
	default:
		evidence = fmt.Sprintf(`%s declares %q with UIApplicationSupportsMultipleScenes
set true, so iOS may run several windows of this app at once — each one its own Flutter engine.`,
			shape.file, shape.token)
		mechanism = `Soroq registers ONE base artifact, binds a patch to that identity, and activates it on a clean
start. Scenes are created and destroyed independently of the process, so a patch staged while one
window is open faces engines at different ages: some started before it, some after. Which window runs
the patched code, and what "activate on next clean start" means when the process outlives every
scene, are unanswered here.`
	}

	return fmt.Errorf(`this iOS host is not a single-engine application, which is the only shape Soroq supports

%s

%s

None of that is verified, so releasing or patching here would bind a patch to an identity only part of
the running app shares. Supported today: a standard full Flutter application with ONE Flutter engine.

If you want to experiment anyway, accepting that the result is unverified:

    %s=1 soroq <your command>`, evidence, mechanism, unverifiedBuildFlagsOptInEnv)
}
