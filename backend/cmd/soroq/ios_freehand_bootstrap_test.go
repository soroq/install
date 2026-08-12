package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// zeroTouchFixture writes a clean, disposable freehand base app (soroq_flutter declared, engine
// enabled with NO patchable list, a plain no-arg main under lib/) and returns its dir.
func zeroTouchFixture(t *testing.T, mainSrc string) string {
	t.Helper()
	dir := t.TempDir()
	writeFixtureFile(t, dir, "pubspec.yaml", "name: myapp\nversion: 1.0.0+1\n"+
		"environment:\n  sdk: '>=3.7.0-0 <4.0.0'\n"+
		"dependencies:\n  flutter:\n    sdk: flutter\n  soroq_flutter:\n    path: /opt/soroq_flutter\n")
	writeFixtureFile(t, dir, "soroq.yaml", "app_id: dev.soroq.myapp\nchannel: stable\n"+
		"runtime_id_strategy: manifest_trust_v1\n"+
		"manifest_trust:\n  keyset_version: 1\n  keys:\n    - id: dev-primary\n      public_key: zRVsQ98KecPqlQBhJsp7xbUgYy82pF6vDQUuKgP9Yds\n"+
		"ios_engine:\n  enabled: true\n")
	writeFixtureFile(t, dir, filepath.Join("lib", "main.dart"), mainSrc)
	return dir
}

func writeFixtureFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hashDirTree returns a stable digest of every file under root (relpath + bytes), for proving lib/ is
// byte-unchanged across a generation pass.
func hashDirTree(t *testing.T, root string) string {
	t.Helper()
	var entries []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, p)
		entries = append(entries, filepath.ToSlash(rel)+"\x00"+string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\x00\x00")))
	return hex.EncodeToString(sum[:])
}

const testPinnedKeyHex = "ab12cd34ef567890ab12cd34ef567890ab12cd34ef567890ab12cd34ef567890"

func TestFreehandZeroTouch_GeneratesGlueWithoutTouchingLib(t *testing.T) {
	dir := zeroTouchFixture(t, "import 'package:flutter/material.dart';\n\nvoid main() {\n  runApp(const SizedBox());\n}\n")
	libBefore := hashDirTree(t, filepath.Join(dir, "lib"))

	bootstrapRel, err := prepareFreehandZeroTouch(dir, testPinnedKeyHex, nil)
	if err != nil {
		t.Fatalf("prepareFreehandZeroTouch: %v", err)
	}
	if bootstrapRel != freehandBootstrapRelPath {
		t.Fatalf("bootstrap rel = %q, want %q", bootstrapRel, freehandBootstrapRelPath)
	}

	// lib/ must be byte-for-byte unchanged.
	if after := hashDirTree(t, filepath.Join(dir, "lib")); after != libBefore {
		t.Fatal("lib/ was modified by zero-touch generation (must be untouched)")
	}
	// Nothing indexed-scaffold-ish must appear (freehand needs none of it).
	for _, forbidden := range []string{
		filepath.Join("lib", "soroq_activator.dart"),
		filepath.Join("lib", "soroq_patch_table.g.dart"),
		"soroq_app_manifest.txt",
	} {
		if fileExists(filepath.Join(dir, forbidden)) {
			t.Fatalf("freehand zero-touch must not create %s", forbidden)
		}
	}

	activator := readFixture(t, dir, freehandActivatorRelPath)
	if !strings.Contains(activator, "implements SoroqEngineActivator, SoroqFreehandActivator") {
		t.Fatal("activator must implement BOTH interfaces")
	}
	for _, want := range []string{
		"loadModuleFromBytes(bytecode)",
		"soroqTransitionBatchByIdentity(newFlatSpecs, staleFlatBaseIds)",
		"bool redirect(int index, Object? replacement) => throw UnsupportedError",
		"void rollbackToBase() => throw UnsupportedError",
	} {
		if !strings.Contains(activator, want) {
			t.Fatalf("activator missing %q", want)
		}
	}

	bootstrap := readFixture(t, dir, freehandBootstrapRelPath)
	for _, want := range []string{
		"import 'package:myapp/main.dart' as app;",
		"import 'soroq_freehand_activator.g.dart';",
		"const String _soroqAppId = 'dev.soroq.myapp';",
		"const String _soroqChannel = 'stable';",
		"const String _soroqControlPlaneBaseUrl = 'https://api.soroq.dev';",
		"const String _soroqPinnedEnginePublicKeyHex = '" + testPinnedKeyHex + "';",
		"await Soroq.configure(",
		"activator: SoroqFreehandActivatorImpl(),",
		"await controller.restorePrepare();",
		// activation waits for the first RASTERIZED frame and returns a Future the update chains behind
		"soroqActivateRestoredAfterFirstFrame(c).then((_) {",
		// background update + post-apply-frame commit of the exact NEWLY-APPLIED version
		"return c.checkForUpdate().then((SoroqOtaStatus st) {",
		"if (st.error == null && st.isPatched && st.activeVersion != 0) {",
		"soroqCommitStableOnHealthyFrame(c, st.activeVersion);",
		"app.main();",
	} {
		if !strings.Contains(bootstrap, want) {
			t.Fatalf("bootstrap missing %q", want)
		}
	}
	// The old, buggy single first-frame markStable (that never committed a network patch) must be gone.
	if strings.Contains(bootstrap, "unawaited(controller.checkForUpdate())") {
		t.Fatal("bootstrap must not fire-and-forget checkForUpdate without a post-apply stability commit")
	}
	// The post-apply commit MUST go through the frame-requesting helper, never a bare
	// addPostFrameCallback (which does not schedule a frame and so silently drops the commit in a
	// fully static app). See soroqCommitStableOnHealthyFrame + its widget/scheduler test.
	if strings.Contains(bootstrap, "addPostFrameCallback") {
		t.Fatal("bootstrap must commit via soroqCommitStableOnHealthyFrame (frame-requesting), not a bare addPostFrameCallback")
	}

	// ORDERING IS THE FIX, AND IT IS ASSERTED AS A WHITELIST, NOT AS SUBSTRING POSITIONS.
	//
	// Position assertions on specific literals are not a regression guard. An adversarial pass proved it:
	// inserting `await controller.restore(); await controller.checkForUpdate();` immediately after
	// `restorePrepare()` -- the legacy complete-restore plus a network activation, both BEFORE
	// `app.main()` -- left every position assertion green. The anchors were `c.checkForUpdate()`, which
	// never matches `controller.checkForUpdate()`, and before `app.main()` only `controller` is in scope,
	// so a real regression is naturally written the exact way the anchor misses.
	//
	// So: enumerate every controller call that appears before `app.main();` and require it to be one of
	// the two that provably install no redirect. Anything else fails, on either variable name.
	appMain := strings.Index(bootstrap, "app.main();")
	activate := strings.Index(bootstrap, "soroqActivateRestoredAfterFirstFrame(c)")
	if appMain < 0 || activate < 0 {
		t.Fatalf("bootstrap is missing an ordering anchor (app.main=%d activate=%d)", appMain, activate)
	}
	if appMain > activate {
		t.Fatal("bootstrap must call app.main() BEFORE the OTA activation chain; the app has to start first")
	}

	preMain := bootstrap[:appMain]
	// The only controller work allowed before the app starts: creating the controller, and the state
	// phase, which by construction loads no module and installs no redirect.
	allowedPreMain := map[string]bool{"configure": true, "restorePrepare": true}
	callRe := regexp.MustCompile(`(?:controller|c)\??\.([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	for _, m := range callRe.FindAllStringSubmatch(preMain, -1) {
		if !allowedPreMain[m[1]] {
			t.Fatalf("bootstrap calls %q before app.main(); only %v may run pre-first-frame — "+
				"every other controller entry point can reach an activation site and install redirects "+
				"before a frame has been presented, which is the black-window defect",
				m[1], []string{"configure", "restorePrepare"})
		}
	}
	// And the hosted check must be chained behind the activation, on either variable name.
	for _, forbidden := range []string{"controller.checkForUpdate()", "controller.restore()", "controller.restoreActivate()"} {
		if strings.Contains(bootstrap, forbidden) {
			t.Fatalf("bootstrap must not call %s: the OTA chain runs on `c` behind the rasterized-frame barrier", forbidden)
		}
	}
	check := strings.Index(bootstrap, "c.checkForUpdate()")
	if check < 0 || check < activate {
		t.Fatal("bootstrap must not place checkForUpdate before the rasterized-frame activation: " +
			"checkForUpdate reaches the activation path itself and would install redirects pre-first-frame")
	}

	// Idempotent: a second pass reproduces byte-identical generated files.
	genBefore := hashDirTree(t, filepath.Join(dir, ".soroq", "generated"))
	if _, err := prepareFreehandZeroTouch(dir, testPinnedKeyHex, nil); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if genAfter := hashDirTree(t, filepath.Join(dir, ".soroq", "generated")); genAfter != genBefore {
		t.Fatal("generation is not idempotent (second pass changed the generated files)")
	}
}

func TestFreehandZeroTouch_CustomEntrypointUnderLib(t *testing.T) {
	dir := zeroTouchFixture(t, "void main() {}\n")
	writeFixtureFile(t, dir, filepath.Join("lib", "main_dev.dart"), "void main() {}\n")
	if _, err := prepareFreehandZeroTouch(dir, testPinnedKeyHex, []string{"-t", "lib/main_dev.dart"}); err != nil {
		t.Fatalf("custom entrypoint under lib/: %v", err)
	}
	bootstrap := readFixture(t, dir, freehandBootstrapRelPath)
	if !strings.Contains(bootstrap, "import 'package:myapp/main_dev.dart' as app;") {
		t.Fatal("bootstrap must wrap the custom entrypoint's package URI")
	}
}

func TestFreehandZeroTouch_RejectsEntrypointOutsideLib(t *testing.T) {
	dir := zeroTouchFixture(t, "void main() {}\n")
	writeFixtureFile(t, dir, filepath.Join("bin", "tool.dart"), "void main() {}\n")
	_, err := prepareFreehandZeroTouch(dir, testPinnedKeyHex, []string{"--target", "bin/tool.dart"})
	if err == nil || !strings.Contains(err.Error(), "outside lib/") {
		t.Fatalf("expected outside-lib refusal, got %v", err)
	}
}

func TestFreehandZeroTouch_RejectsRequiredArgsMain(t *testing.T) {
	dir := zeroTouchFixture(t, "void main(List<String> args) {\n  print(args);\n}\n")
	_, err := prepareFreehandZeroTouch(dir, testPinnedKeyHex, nil)
	if err == nil || !strings.Contains(err.Error(), "required argument") {
		t.Fatalf("expected required-arg main refusal, got %v", err)
	}
}

func TestFreehandZeroTouch_AllOptionalArgsMainAccepted(t *testing.T) {
	dir := zeroTouchFixture(t, "void main([List<String>? args]) {}\n")
	if _, err := prepareFreehandZeroTouch(dir, testPinnedKeyHex, nil); err != nil {
		t.Fatalf("all-optional main should be accepted: %v", err)
	}
}

func TestFreehandZeroTouch_RequiresSoroqFlutterDependency(t *testing.T) {
	dir := zeroTouchFixture(t, "void main() {}\n")
	// Rewrite pubspec without the soroq_flutter dependency.
	writeFixtureFile(t, dir, "pubspec.yaml", "name: myapp\nversion: 1.0.0+1\ndependencies:\n  flutter:\n    sdk: flutter\n")
	_, err := prepareFreehandZeroTouch(dir, testPinnedKeyHex, nil)
	if err == nil || !strings.Contains(err.Error(), "flutter pub add soroq_flutter") {
		t.Fatalf("expected soroq_flutter dependency requirement, got %v", err)
	}
}

func TestFreehandZeroTouch_RejectsBadPinnedKey(t *testing.T) {
	dir := zeroTouchFixture(t, "void main() {}\n")
	_, err := prepareFreehandZeroTouch(dir, "not-hex", nil)
	if err == nil || !strings.Contains(err.Error(), "64 hex") {
		t.Fatalf("expected 64-hex key requirement, got %v", err)
	}
}

func TestWithFreehandBootstrapEntrypoint_StripsDeveloperTarget(t *testing.T) {
	// Developer passed their own -t; the Soroq bootstrap must win, developer's -t removed.
	got := withFreehandBootstrapEntrypoint(freehandBootstrapRelPath,
		[]string{"--extra-front-end-options=--dynamic-interface=x", "-t", "lib/main.dart", "--verbose"})
	want := []string{"-t", freehandBootstrapRelPath, "--extra-front-end-options=--dynamic-interface=x", "--verbose"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func readFixture(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
