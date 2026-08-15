package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// MULTI-ENGINE / MULTI-WINDOW iOS HOSTS, on BOTH hard-OTA routes.
//
// Two separate properties are at stake and they are not equally cheap to get wrong:
//
//  1. An ORDINARY single-engine Flutter application must keep working. A guard that refuses the
//     standard template breaks every user of the product at once, and the standard template is full of
//     things that LOOK like evidence (FlutterAppDelegate, a FlutterViewController cast,
//     GeneratedPluginRegistrant). The anti-false-positive tests below are the important ones.
//
//  2. A host that really does run several engines must be refused BEFORE the project is touched, on
//     the release route AND the patch route, on the freehand branch AND the scaffolded branch.
//
// The refusal tests reuse engine_lane_refusal_test.go's harness so "untouched" keeps meaning the same
// thing: no file created or modified, and no counted production boundary reached.

// ---------------------------------------------------------------------------------------------------
// FIXTURES

// The stock Flutter iOS host, verbatim in spirit: FlutterAppDelegate, GeneratedPluginRegistrant, a
// rootViewController cast to FlutterViewController, a method channel. Nothing here may trip the guard.
const iosOrdinaryAppDelegate = `import Flutter
import UIKit

@main
@objc class AppDelegate: FlutterAppDelegate {
  override func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?
  ) -> Bool {
    GeneratedPluginRegistrant.register(with: self)
    let controller = window?.rootViewController as! FlutterViewController
    let channel = FlutterMethodChannel(name: "example/battery",
                                       binaryMessenger: controller.binaryMessenger)
    channel.setMethodCallHandler { call, result in result(nil) }
    return super.application(application, didFinishLaunchingWithOptions: launchOptions)
  }
}
`

// A host that creates exactly ONE engine explicitly. Still supported: one engine is one identity.
const iosSingleExplicitEngineAppDelegate = `import Flutter
import UIKit

@main
@objc class AppDelegate: FlutterAppDelegate {
  var engine: FlutterEngine?

  override func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?
  ) -> Bool {
    let engine = FlutterEngine(name: "main")
    engine.run()
    self.engine = engine
    GeneratedPluginRegistrant.register(with: self)
    return super.application(application, didFinishLaunchingWithOptions: launchOptions)
  }
}
`

const iosEngineGroupAppDelegate = `import Flutter
import UIKit

@main
@objc class AppDelegate: FlutterAppDelegate {
  lazy var engines = FlutterEngineGroup(name: "multi-engine", project: nil)

  override func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?
  ) -> Bool {
    GeneratedPluginRegistrant.register(with: self)
    return super.application(application, didFinishLaunchingWithOptions: launchOptions)
  }
}
`

const iosTwoEnginesAppDelegate = `import Flutter
import UIKit

@main
@objc class AppDelegate: FlutterAppDelegate {
  var primary: FlutterEngine?
  var secondary: FlutterEngine?

  override func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?
  ) -> Bool {
    primary = FlutterEngine(name: "primary")
    secondary = FlutterEngine(name: "secondary")
    primary?.run()
    secondary?.run(withEntrypoint: "secondaryMain")
    GeneratedPluginRegistrant.register(with: self)
    return super.application(application, didFinishLaunchingWithOptions: launchOptions)
  }
}
`

const iosTwoEnginesObjC = `#import "AppDelegate.h"
#import "GeneratedPluginRegistrant.h"

@implementation AppDelegate
- (BOOL)application:(UIApplication *)application
    didFinishLaunchingWithOptions:(NSDictionary *)launchOptions {
  FlutterEngine *primary = [[FlutterEngine alloc] initWithName:@"primary" project:nil];
  FlutterEngine *secondary = [[FlutterEngine alloc] initWithName:@"secondary" project:nil];
  [primary run];
  [secondary runWithEntrypoint:@"secondaryMain"];
  [GeneratedPluginRegistrant registerWithRegistry:self];
  return [super application:application didFinishLaunchingWithOptions:launchOptions];
}
@end
`

// FlutterEngineGroup named only in comments and string literals — including one inside an ESCAPED
// quote, which a naive quote-toggling stripper would hand back as code.
const iosCommentAndStringOnlyAppDelegate = `import Flutter
import UIKit

// This app deliberately does NOT use FlutterEngineGroup: one engine, one identity.
/*
  An earlier prototype called FlutterEngineGroup(name: "multi") here and
  also did secondary = FlutterEngine(name: "secondary"). Both were removed.
*/
@main
@objc class AppDelegate: FlutterAppDelegate {
  override func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?
  ) -> Bool {
    let note = "FlutterEngineGroup"
    let quoted = "the reviewer asked \"why not FlutterEngineGroup\" and we said no"
    let alsoNot = "FlutterEngine(name: \"a\") + FlutterEngine(name: \"b\")"
    NSLog("%@ %@ %@", note, quoted, alsoNot)
    GeneratedPluginRegistrant.register(with: self)
    return super.application(application, didFinishLaunchingWithOptions: launchOptions)
  }
}
`

const iosOrdinaryInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>example</string>
	<key>UILaunchStoryboardName</key>
	<string>LaunchScreen</string>
</dict>
</plist>
`

const iosMultipleScenesInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>example</string>
	<key>UIApplicationSceneManifest</key>
	<dict>
		<key>UIApplicationSupportsMultipleScenes</key>
		<true/>
		<key>UISceneConfigurations</key>
		<dict/>
	</dict>
</dict>
</plist>
`

const iosSingleSceneInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>UIApplicationSceneManifest</key>
	<dict>
		<key>UIApplicationSupportsMultipleScenes</key>
		<false/>
	</dict>
</dict>
</plist>
`

// ordinaryIOSHost is the supported baseline every anti-false-positive test starts from.
func ordinaryIOSHost() map[string]string {
	return map[string]string{
		"AppDelegate.swift": iosOrdinaryAppDelegate,
		"Info.plist":        iosOrdinaryInfoPlist,
	}
}

func writeIOSHost(t *testing.T, projectDir string, files map[string]string) {
	t.Helper()
	if files == nil {
		return // "no ios/ directory at all" — not even an empty Runner
	}
	runner := filepath.Join(projectDir, "ios", "Runner")
	if err := os.MkdirAll(runner, 0o755); err != nil {
		t.Fatalf("mkdir ios/Runner: %v", err)
	}
	for name, body := range files {
		writeFile(t, filepath.Join(runner, name), body)
	}
}

// ---------------------------------------------------------------------------------------------------
// HARNESS
//
// newMultiEngineProject is engine_lane_refusal_test.go's project plus an iOS host and one extra stub.
//
// The extra stub is not optional. The patch route's freehand branch calls engineLanePatchFreehandFn,
// and the real runPatchIOSEngineFreehand installs an analyzer and compiles; a positive control that
// reached it would run a real toolchain, and a refusal test could not tell "guard fired" from "the
// branch was never reachable". It increments the SAME freehand counter, so assertUntouched covers the
// patch route verbatim.
//
// SCOPE, stated honestly, in the spirit of the httpRequests note in engine_lane_refusal_test.go: of
// the seven counters assertUntouched checks, `manifestTrust` and `build` are boundaries the PATCH
// route never has — it neither self-heals a signing key nor builds an app.dill. Their zero-call
// assertions on patch-route refusals are therefore trivially true and prove nothing there; they are
// proven reachable by the RELEASE-route positive controls (TestEngineLaneScaffoldedBoundariesRegister)
// instead. The four counters that the patch route can reach — resolution, scaffold, delegate,
// freehand — are each shown non-zero by the patch positive controls at the bottom of this file.
func newMultiEngineProject(t *testing.T, soroqYAML string, host map[string]string) *engineLaneProject {
	t.Helper()
	p := newEngineLaneProjectWithConfig(t, engineOrdinaryPubspec, soroqYAML)
	writeIOSHost(t, p.dir, host)

	prevPatchFreehand := engineLanePatchFreehandFn
	engineLanePatchFreehandFn = func([]string, []string, string) error {
		atomic.AddInt32(&p.counters.freehand, 1)
		return nil
	}
	t.Cleanup(func() { engineLanePatchFreehandFn = prevPatchFreehand })
	return p
}

// runEngineLanePatch drives the TOP-LEVEL patch router (`soroq patch ios --engine`), the same dispatch
// a developer hits, so the guards are exercised through real routing rather than a direct call.
func runEngineLanePatch(p *engineLaneProject, extra ...string) error {
	args := append([]string{"ios", "--engine",
		"--project-dir", p.dir, "--toolchain", "tc-1", "--api", p.apiURL}, extra...)
	return runPatch(args)
}

// engineLaneRoutes: the two hard-OTA entry points a shape refusal must hold on. Before this, the patch
// route ran none of the shape guards at all: the same project was refused by `release` and accepted by
// `patch`, after `patch` had already rewritten .dart_tool/package_config.json.
var engineLaneRoutes = map[string]func(*engineLaneProject, ...string) error{
	"release": runEngineLaneBuild,
	"patch":   runEngineLanePatch,
}

// ---------------------------------------------------------------------------------------------------
// (a) ANTI-FALSE-POSITIVE: the ordinary application must survive, on every route and branch.

func TestOrdinarySingleEngineHostIsNotRefused(t *testing.T) {
	hosts := map[string]map[string]string{
		"stock template":        ordinaryIOSHost(),
		"one explicit engine":   {"AppDelegate.swift": iosSingleExplicitEngineAppDelegate, "Info.plist": iosOrdinaryInfoPlist},
		"comments and strings":  {"AppDelegate.swift": iosCommentAndStringOnlyAppDelegate, "Info.plist": iosOrdinaryInfoPlist},
		"scenes, but single":    {"AppDelegate.swift": iosOrdinaryAppDelegate, "Info.plist": iosSingleSceneInfoPlist},
		"no ios directory":      nil,
		"host with no sources":  {},
		"unrelated swift files": {"AppDelegate.swift": iosOrdinaryAppDelegate, "SettingsView.swift": "import SwiftUI\nstruct SettingsView: View { var body: some View { Text(\"FlutterEngineGroup\") } }\n"},
	}
	for hostName, host := range hosts {
		for routeName, run := range engineLaneRoutes {
			for cfgName, cfg := range engineLaneRouteConfigs {
				t.Run(hostName+"/"+routeName+"/"+cfgName, func(t *testing.T) {
					p := newMultiEngineProject(t, cfg, host)
					// The scaffolded branch may still fail further down on this stub fixture (the
					// release route generates a dynamic interface for real), so the assertion is that
					// it was not refused as an unsupported SHAPE, not that it succeeded outright.
					err := run(p)
					if err != nil {
						for _, forbidden := range []string{"single-engine", "FlutterEngineGroup", "UIApplicationSceneManifest"} {
							if strings.Contains(err.Error(), forbidden) {
								t.Fatalf("an ordinary single-engine app was refused as a multi-engine host: %v", err)
							}
						}
					}
					// Non-vacuity: prove the run actually got PAST the guards rather than failing
					// earlier for some unrelated reason, which would make the check above meaningless.
					if cfg == engineFreehandConfig {
						if got := atomic.LoadInt32(&p.counters.freehand); got != 1 {
							t.Fatalf("supported shape never reached the freehand branch (%d); the not-refused assertion above is vacuous. err=%v", got, err)
						}
					} else if got := atomic.LoadInt32(&p.counters.resolution) + atomic.LoadInt32(&p.counters.manifestTrust); got == 0 {
						t.Fatalf("supported shape never reached a post-guard boundary; the not-refused assertion above is vacuous. err=%v", err)
					}
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// (b)+(c) REFUSALS, on both routes and both branches, before any project mutation.

func TestEngineLaneRefusesMultiEngineHostsBeforeAnyProjectMutation(t *testing.T) {
	cases := map[string]struct {
		host map[string]string
		want []string
	}{
		"FlutterEngineGroup": {
			host: map[string]string{"AppDelegate.swift": iosEngineGroupAppDelegate, "Info.plist": iosOrdinaryInfoPlist},
			want: []string{"single-engine", "ios/Runner/AppDelegate.swift", "FlutterEngineGroup"},
		},
		"two Swift engines": {
			host: map[string]string{"AppDelegate.swift": iosTwoEnginesAppDelegate, "Info.plist": iosOrdinaryInfoPlist},
			want: []string{"single-engine", "ios/Runner/AppDelegate.swift", `constructs "FlutterEngine" 2 times`},
		},
		"two Objective-C engines": {
			host: map[string]string{"AppDelegate.m": iosTwoEnginesObjC, "Info.plist": iosOrdinaryInfoPlist},
			want: []string{"single-engine", "ios/Runner/AppDelegate.m", `constructs "FlutterEngine" 2 times`},
		},
		"multiple scenes": {
			host: map[string]string{"AppDelegate.swift": iosOrdinaryAppDelegate, "Info.plist": iosMultipleScenesInfoPlist},
			want: []string{"single-engine", "ios/Runner/Info.plist", "UIApplicationSceneManifest"},
		},
	}
	for shapeName, tc := range cases {
		for routeName, run := range engineLaneRoutes {
			for cfgName, cfg := range engineLaneRouteConfigs {
				t.Run(shapeName+"/"+routeName+"/"+cfgName, func(t *testing.T) {
					p := newMultiEngineProject(t, cfg, tc.host)
					before := p.snapshot(t)
					err := run(p)
					if err == nil {
						t.Fatalf("the hard-OTA %s route must refuse a multi-engine host", routeName)
					}
					for _, want := range tc.want {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("refusal should name %q:\n%s", want, err)
						}
					}
					// The opt-in must be discoverable from the refusal itself.
					if !strings.Contains(err.Error(), unverifiedBuildFlagsOptInEnv) {
						t.Errorf("refusal should name the %s opt-in:\n%s", unverifiedBuildFlagsOptInEnv, err)
					}
					p.assertUntouched(t, before)
				})
			}
		}
	}
}

// A refused shape stays refused for the right REASON: an add-to-app project whose host also has two
// engines must still report add-to-app, because that is the fact the developer has to act on first.
func TestModuleRefusalStillWinsOverMultiEngine(t *testing.T) {
	p := newEngineLaneProjectWithConfig(t, engineModulePubspec, engineFreehandConfig)
	writeIOSHost(t, p.dir, map[string]string{"AppDelegate.swift": iosTwoEnginesAppDelegate})
	before := p.snapshot(t)
	err := runEngineLaneBuild(p)
	if err == nil || !strings.Contains(err.Error(), "add-to-app") {
		t.Fatalf("a module project must still be refused as add-to-app, got: %v", err)
	}
	p.assertUntouched(t, before)
}

// The documented opt-in is the escape hatch for someone who accepts an unverified result.
func TestMultiEngineOptInIsHonoured(t *testing.T) {
	t.Setenv(unverifiedBuildFlagsOptInEnv, "1")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), engineOrdinaryPubspec)
	writeIOSHost(t, dir, map[string]string{"AppDelegate.swift": iosEngineGroupAppDelegate})
	if err := guardSupportedApplicationShape(dir); err != nil {
		t.Fatalf("the documented opt-in must allow it: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------------
// (e) UNIT-LEVEL: comments, string literals and identifier boundaries.
//
// These pin the exact reasons the route-level anti-false-positive test passes, so a regression says
// WHICH property broke instead of only that some project stopped building.

func TestMultiEngineDetectionIgnoresCommentsAndStrings(t *testing.T) {
	code := stripSourceCommentsAndStrings(iosCommentAndStringOnlyAppDelegate)
	if strings.Contains(code, "FlutterEngineGroup") {
		t.Errorf("comment/string occurrences must be blanked before matching:\n%s", code)
	}
	if n := countFlutterEngineConstructions(code); n != 0 {
		t.Errorf("commented-out and quoted constructions must not count, got %d", n)
	}
	if len(code) != len(iosCommentAndStringOnlyAppDelegate) {
		t.Errorf("stripping must preserve offsets: %d != %d", len(code), len(iosCommentAndStringOnlyAppDelegate))
	}
}

func TestStringEscapeDoesNotLeakCode(t *testing.T) {
	// A quote-toggling stripper ends the literal at \" and hands "FlutterEngineGroup" back as code.
	src := `let s = "he said \"FlutterEngineGroup\" once"` + "\n"
	if got := stripSourceCommentsAndStrings(src); strings.Contains(got, "FlutterEngineGroup") {
		t.Errorf("an escaped quote must not end the string literal:\n%s", got)
	}
	// An UNTERMINATED literal must end at the newline rather than swallowing the rest of the file.
	unterminated := "let s = \"oops\nlet g = FlutterEngineGroup(name: \"x\")\n"
	if got := stripSourceCommentsAndStrings(unterminated); !strings.Contains(got, "FlutterEngineGroup") {
		t.Errorf("an unterminated literal must not swallow the following code:\n%s", got)
	}
}

func TestEngineConstructionCountingIsIdentifierExact(t *testing.T) {
	for name, tc := range map[string]struct {
		code string
		want int
	}{
		"declaration only":      {"var engine: FlutterEngine?\n", 0},
		"cast only":             {"let e = something as! FlutterEngine\n", 0},
		"one swift":             {"let e = FlutterEngine(name: \"a\")\n", 1},
		"one objc":              {"FlutterEngine *e = [[FlutterEngine alloc] initWithName:@\"a\"];\n", 1},
		"two swift":             {"let a = FlutterEngine(name: \"a\")\nlet b = FlutterEngine(name: \"b\")\n", 2},
		"engine group is not a": {"let g = FlutterEngineGroup(name: \"g\")\n", 0},
		"longer identifier":     {"let x = MyFlutterEngine(name: \"a\")\n", 0},
	} {
		if got := countFlutterEngineConstructions(tc.code); got != tc.want {
			t.Errorf("%s: counted %d constructions, want %d", name, got, tc.want)
		}
	}
	// FlutterEngineGroup must match as a whole identifier only.
	if len(identifierOffsets("let g = MyFlutterEngineGroupHelper()\n", "FlutterEngineGroup")) != 0 {
		t.Error("a longer identifier containing FlutterEngineGroup must not count as one")
	}
	if len(identifierOffsets("let g = FlutterEngineGroup(name: \"g\")\n", "FlutterEngineGroup")) != 1 {
		t.Error("a real FlutterEngineGroup use must be found")
	}
}

func TestSceneManifestDetectionIsConservative(t *testing.T) {
	if !plistDeclaresMultipleScenes([]byte(iosMultipleScenesInfoPlist)) {
		t.Error("UIApplicationSupportsMultipleScenes <true/> is a multi-window host")
	}
	for name, plist := range map[string]string{
		"no scene manifest":      iosOrdinaryInfoPlist,
		"explicitly false":       iosSingleSceneInfoPlist,
		"manifest but no key":    "<plist><dict><key>UIApplicationSceneManifest</key><dict/></dict></plist>",
		"key without a manifest": "<plist><dict><key>UIApplicationSupportsMultipleScenes</key><true/></dict></plist>",
		"binary plist":           "bplist00\x00\x01UIApplicationSceneManifest",
	} {
		if plistDeclaresMultipleScenes([]byte(plist)) {
			t.Errorf("%s must not be read as a multi-window host", name)
		}
	}
}

// A project with no ios/ directory at all (or an empty one) must produce no evidence and no refusal.
func TestNoIOSHostProducesNoEvidence(t *testing.T) {
	dir := t.TempDir()
	if shape := detectUnsupportedIOSHostShape(dir); shape != nil {
		t.Errorf("a project with no ios/ directory must yield no evidence, got %+v", shape)
	}
	writeIOSHost(t, dir, map[string]string{})
	if shape := detectUnsupportedIOSHostShape(dir); shape != nil {
		t.Errorf("an empty ios/Runner must yield no evidence, got %+v", shape)
	}
}

// ---------------------------------------------------------------------------------------------------
// (d) POSITIVE CONTROLS FOR THE PATCH ROUTE.
//
// Every zero-call assertion on a patch-route refusal above is worthless unless the same harness can be
// shown to drive those counters NON-zero on a supported shape. The patch route had no positive control
// of any kind before this — it called the raw functions, so no counter could ever fire on it and its
// refusal assertions would have passed even if the route were dead code.
//
// Both branches are driven, because the freehand branch RETURNS before the scaffolded boundaries exist.

func TestEngineLanePatchFreehandBoundaryRegisters(t *testing.T) {
	p := newMultiEngineProject(t, engineFreehandConfig, ordinaryIOSHost())
	if err := runEngineLanePatch(p); err != nil {
		t.Fatalf("a supported freehand shape must not be refused on the patch route: %v", err)
	}
	if got := atomic.LoadInt32(&p.counters.freehand); got != 1 {
		t.Fatalf("patch freehand boundary registered %d times on a supported shape; the counter is not "+
			"wired to the patch route, so its zero-call assertions are vacuous", got)
	}
	if got := atomic.LoadInt32(&p.counters.resolution); got != 1 {
		t.Fatalf("dependency preparation registered %d times on a supported freehand patch; its "+
			"zero-call assertion is vacuous", got)
	}
}

func TestEngineLanePatchScaffoldedBoundariesRegister(t *testing.T) {
	p := newMultiEngineProject(t, engineScaffoldedConfig, ordinaryIOSHost())
	if err := runEngineLanePatch(p); err != nil {
		t.Fatalf("a supported scaffolded shape must not be refused on the patch route: %v", err)
	}
	if got := atomic.LoadInt32(&p.counters.freehand); got != 0 {
		t.Fatalf("a patchable list must not take the freehand branch on the patch route (got %d)", got)
	}
	for name, got := range map[string]int32{
		"dependency preparation": atomic.LoadInt32(&p.counters.resolution),
		"generated wiring":       atomic.LoadInt32(&p.counters.scaffold),
		"delegate":               atomic.LoadInt32(&p.counters.delegate),
	} {
		if got == 0 {
			t.Errorf("%s was never reached on a SUPPORTED shape via the patch route; its zero-call "+
				"assertion in the refusal tests proves nothing", name)
		}
	}
}

// THE SECOND DISPATCHER.
//
// runPatchIOSEngineScaffolded is reachable from two places: `soroq patch ios --engine` (patch_cmd.go,
// which strips the routing flag) and `soroq patch --platforms=ios` (platforms_cmd.go, which passes
// derived args with no --engine at all). The guards live inside the function precisely so both are
// covered — but "inside the function" only helps if the args still carry a `--` separator by the time
// splitFlutterPassthrough sees them. applyPlatformScopedOverrides and withDerivedFlags rewrite that
// list, and if either dropped or moved the separator the flavor/obfuscation guards would silently
// become no-ops on this path while every test above kept passing.
func TestPlatformsDispatcherReachesTheSameGuards(t *testing.T) {
	for cfgName, cfg := range engineLaneRouteConfigs {
		t.Run("flavor/"+cfgName, func(t *testing.T) {
			p := newMultiEngineProject(t, cfg, ordinaryIOSHost())
			before := p.snapshot(t)
			err := patchPlatform("ios", []string{
				"--project-dir", p.dir, "--toolchain", "tc-1", "--api", p.apiURL, "--", "--flavor", "prod"})
			if err == nil || !strings.Contains(err.Error(), "no flavor support") {
				t.Fatalf("`patch --platforms=ios` must refuse a flavored build too, got: %v", err)
			}
			p.assertUntouched(t, before)
		})
		t.Run("multi-engine/"+cfgName, func(t *testing.T) {
			p := newMultiEngineProject(t, cfg, map[string]string{
				"AppDelegate.swift": iosEngineGroupAppDelegate, "Info.plist": iosOrdinaryInfoPlist})
			before := p.snapshot(t)
			err := patchPlatform("ios", []string{
				"--project-dir", p.dir, "--toolchain", "tc-1", "--api", p.apiURL})
			if err == nil || !strings.Contains(err.Error(), "single-engine") {
				t.Fatalf("`patch --platforms=ios` must refuse a multi-engine host too, got: %v", err)
			}
			p.assertUntouched(t, before)
		})
	}
}

// The patch route must refuse the flags the release route refuses. Same project, same command shape,
// two different verdicts was the actual defect: `release` said no, `patch` said yes and had already
// rewritten package_config.json by the time anything checked.
func TestEngineLanePatchRefusesUnverifiedBuildFlags(t *testing.T) {
	for cfgName, cfg := range engineLaneRouteConfigs {
		t.Run("flavor/"+cfgName, func(t *testing.T) {
			p := newMultiEngineProject(t, cfg, ordinaryIOSHost())
			before := p.snapshot(t)
			err := runEngineLanePatch(p, "--", "--flavor", "prod")
			if err == nil || !strings.Contains(err.Error(), "no flavor support") {
				t.Fatalf("the patch route must refuse a flavored build, got: %v", err)
			}
			p.assertUntouched(t, before)
		})
		t.Run("obfuscation/"+cfgName, func(t *testing.T) {
			p := newMultiEngineProject(t, cfg, ordinaryIOSHost())
			before := p.snapshot(t)
			err := runEngineLanePatch(p, "--", "--obfuscate", "--split-debug-info=build/sym")
			if err == nil || !strings.Contains(err.Error(), "not verified") {
				t.Fatalf("the patch route must refuse an obfuscated build, got: %v", err)
			}
			p.assertUntouched(t, before)
		})
		t.Run("add-to-app/"+cfgName, func(t *testing.T) {
			p := newEngineLaneProjectWithConfig(t, engineModulePubspec, cfg)
			prev := engineLanePatchFreehandFn
			engineLanePatchFreehandFn = func([]string, []string, string) error {
				atomic.AddInt32(&p.counters.freehand, 1)
				return nil
			}
			t.Cleanup(func() { engineLanePatchFreehandFn = prev })
			before := p.snapshot(t)
			err := runEngineLanePatch(p)
			if err == nil || !strings.Contains(err.Error(), "add-to-app") {
				t.Fatalf("the patch route must refuse an add-to-app project, got: %v", err)
			}
			p.assertUntouched(t, before)
		})
	}
}

// ---------------------------------------------------------------------------
// PLATFORM SPLIT: an iOS host shape must never refuse an ANDROID build.
//
// The multi-engine detector reads ios/Runner sources. If it hung off the shared, platform-neutral
// shape guard, `soroq release android` on a project whose iOS host happens to use an engine group
// would fail with a message naming ios/Runner/AppDelegate.swift -- a file with nothing to do with the
// APK being built. A refusal a developer cannot act on is worse than no refusal: it teaches them to
// reach for SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS, which then also disables the checks that DO apply.
//
// Add-to-app stays platform-neutral, because its reason is: the host application was built by someone
// else's toolchain, so there is no Soroq-registered base artifact to bind to, on any platform.

func TestIOSHostShapeDoesNotRefuseNonIOSBuilds(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ios", "Runner"), 0o755); err != nil {
		t.Fatalf("mkdir ios/Runner: %v", err)
	}
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), engineOrdinaryPubspec)
	writeFile(t, filepath.Join(dir, "ios", "Runner", "AppDelegate.swift"), iosEngineGroupAppDelegate)
	writeFile(t, filepath.Join(dir, "ios", "Runner", "Info.plist"), iosOrdinaryInfoPlist)

	// Positive control: the iOS guard MUST refuse this project, or the negative below proves nothing.
	if err := guardSupportedIOSApplicationShape(dir); err == nil {
		t.Fatal("precondition: the iOS guard must refuse an engine-group host; without this the " +
			"assertion below would pass even if the detector were broken")
	}
	// The platform-neutral guard must NOT, because no Android artifact is affected by an iOS host.
	if err := guardSupportedApplicationShape(dir); err != nil {
		t.Fatalf("an iOS host shape must not refuse a platform-neutral build: %v", err)
	}
}

// Add-to-app remains refused on BOTH, because its reason is platform-neutral.
func TestAddToAppIsRefusedOnBothTheNeutralAndIOSGuards(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pubspec.yaml"), engineModulePubspec)
	for name, guard := range map[string]func(string) error{
		"platform-neutral": guardSupportedApplicationShape,
		"iOS":              guardSupportedIOSApplicationShape,
	} {
		if err := guard(dir); err == nil || !strings.Contains(err.Error(), "add-to-app") {
			t.Errorf("the %s guard must refuse a Flutter module, got: %v", name, err)
		}
	}
}
