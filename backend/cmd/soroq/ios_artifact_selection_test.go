package main

import (
	"strings"
	"testing"
)

// A2 — iOS explicit artifacts, flavors and schemes.
//
// The Android hazard was artifact DISCOVERY: globbing output paths and returning the newest match lets
// a flavored build (which writes elsewhere) leave a stale artifact to be registered as the code just
// built. iOS is structurally different and these tests pin why, so nobody "fixes" it into existence:
//
//   * `soroq release ios` takes its identity from EXPLICIT flags -- --runtime-id, --version, --arch --
//     rather than by inspecting a discovered file, so there is no newest-match to get wrong;
//   * there is no --artifact flag to aim at a stale file;
//   * the only glob on the iOS path repairs damaged cached analyses across ALL build dirs; it selects
//     nothing and therefore cannot select wrongly.
//
// What iOS shares with Android is the shape guards, and those are asserted on the iOS path itself
// rather than inferred from the Android tests.

func TestIOSReleaseTakesIdentityFromExplicitFlagsNotDiscovery(t *testing.T) {
	// The usage string is the contract a developer reads; it must offer identity flags and must NOT
	// offer an artifact path, because an artifact path is what creates the stale-selection hazard.
	usage := iosReleaseUsageForTest()
	for _, want := range []string{"--runtime-id", "--version", "--arch"} {
		if !strings.Contains(usage, want) {
			t.Errorf("iOS release must accept explicit identity flag %q, usage:\n%s", want, usage)
		}
	}
	if strings.Contains(usage, "--artifact ") {
		t.Error("iOS release must not take an --artifact path: identity comes from flags, which is what " +
			"makes stale-artifact selection structurally impossible here")
	}
}

// A flavored iOS build must be refused by the same guard as Android, on the iOS path.
func TestIOSFlavoredBuildIsRefused(t *testing.T) {
	if err := guardFlavoredBuild([]string{"--flavor", "prod"}); err == nil {
		t.Fatal("a flavored build must be refused on the iOS path too")
	}
	if err := guardFlavoredBuild([]string{"--dart-define=FLAVOR=prod"}); err != nil {
		t.Fatalf("a dart-define naming a flavor is not a flavored build: %v", err)
	}
}

// Obfuscated and add-to-app shapes must be refused before an iOS release registers.
func TestIOSUnsupportedShapesAreRefused(t *testing.T) {
	if err := guardUnverifiedBuildFlags([]string{"--obfuscate"}); err == nil {
		t.Error("an obfuscated iOS build must be refused")
	}
	dir := t.TempDir()
	writeFile(t, dir+"/pubspec.yaml", "name: m\nflutter:\n  module:\n    androidX: true\n")
	if err := guardSupportedApplicationShape(dir); err == nil {
		t.Error("an add-to-app project must be refused before an iOS release registers")
	}
}

// The iOS release command must wire all three guards, in front of the build.
func TestIOSReleasePathWiresEveryShapeGuard(t *testing.T) {
	src := iosReleaseSourceForTest(t)
	for _, want := range []string{
		"guardUnverifiedBuildFlags(flutterBuildArgs)",
		"guardFlavoredBuild(flutterBuildArgs)",
		// The iOS routes take the iOS-SPECIFIC variant: it adds the ios/Runner host inspection on top
		// of the platform-neutral add-to-app rule. The neutral one would silently drop the host check.
		"guardSupportedIOSApplicationShape(*projectDir)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("runReleaseIOS must call %s", want)
		}
	}
	// They must precede the build call, or a refused shape would still pay for a compile.
	// The build is invoked through iosReleaseBuildFn so command-level tests can count invocations;
	// ordering is additionally MEASURED in ios_release_command_refusal_test.go rather than only read
	// off the source here.
	buildIdx := strings.Index(src, "iosReleaseBuildFn(")
	for _, guard := range []string{"guardUnverifiedBuildFlags", "guardFlavoredBuild", "guardSupportedIOSApplicationShape"} {
		if idx := strings.Index(src, guard); idx < 0 || idx > buildIdx {
			t.Errorf("%s must run before the iOS build starts", guard)
		}
	}
}
