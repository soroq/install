package main

// The compatibility check is decided by the Dart FRONT END, so these tests pin two things it cannot:
// that the probe really exercises each API through its owner and signature (so a text-only match
// cannot satisfy it), and that a compiler refusal is turned into an actionable message.
//
// The end-to-end negatives -- published 0.2.4 refused, candidate 0.2.5 accepted -- are proven against
// real resolutions and recorded under handoff/freehand/evidence/frontend/runtime-compat/.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeExercisesEveryRequiredAPIThroughOwnerAndSignature(t *testing.T) {
	src := freehandRuntimeProbeSource()

	for _, api := range freehandRuntimeAPIs() {
		if !strings.Contains(src, api.stmt) {
			t.Errorf("probe does not exercise %s (missing %q)", api.symbol, api.stmt)
		}
	}
	// The owner must be pinned: restorePrepare is called ON the controller, so a same-named top-level
	// function in some other library cannot satisfy it.
	if !strings.Contains(src, "c.restorePrepare()") {
		t.Error("restorePrepare must be called on SoroqEngineLaneController, or the check would accept " +
			"the right name with the wrong owner")
	}
	// Signature is pinned by the call shape, not by a name match.
	if !strings.Contains(src, "soroqCommitStableOnHealthyFrame(c, 1)") {
		t.Error("soroqCommitStableOnHealthyFrame must be called with its real argument shape")
	}
	// The probe must import the PUBLIC library, so a declaration hidden in src/ and never exported
	// cannot satisfy it.
	if !strings.Contains(src, "import 'package:soroq_flutter/soroq_flutter.dart';") {
		t.Error("probe must import the package's public library so unexported declarations fail")
	}
	// It must never be written into the customer's project.
	if !strings.Contains(src, "never written into the project's lib/") {
		t.Error("probe should state that it is not shipped")
	}
}

// A private declaration cannot satisfy the probe: `_restorePrepare` is not `restorePrepare`, and Dart
// will not resolve it from another library. This pins that the probe uses the PUBLIC name only.
func TestProbeUsesPublicNamesOnly(t *testing.T) {
	src := freehandRuntimeProbeSource()
	for _, api := range freehandRuntimeAPIs() {
		if strings.Contains(src, "_"+api.symbol) {
			t.Errorf("probe references a private form of %s", api.symbol)
		}
	}
}

// A compiler refusal must name the APIs involved and stay actionable.
func TestIncompatibilityMessageAttributesTheMissingAPIs(t *testing.T) {
	out := `probe.dart:6:11: Error: Method not found: 'soroqCommitStableOnHealthyFrame'.
probe.dart:5:13: Error: The method 'restorePrepare' isn't defined for the type 'SoroqEngineLaneController'.`
	msg := freehandRuntimeIncompatibilityMessage(t.TempDir(), out)

	for _, want := range []string{"soroqCommitStableOnHealthyFrame", "restorePrepare"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not attribute %q: %s", want, msg)
		}
	}
	// An API the compiler did NOT complain about must not be listed as unsatisfied.
	if strings.Contains(msg, "- SoroqFreehandActivator") {
		t.Errorf("message lists an API the compiler accepted: %s", msg)
	}
	for _, want := range []string{"Compiler output:", "Fix:", "not actionable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
}

// Resolution must come from package_config.json -- what the compiler uses -- not pubspec.yaml, which
// states a constraint rather than a resolution.
func TestUnresolvedProjectIsRefusedClearly(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "pubspec.yaml"), "name: app\ndependencies:\n  soroq_flutter: ^0.2.4\n")
	err := verifyFreehandRuntimeCompatibility(dir, t.TempDir())
	if err == nil {
		t.Fatal("a project with no package_config.json was accepted")
	}
	if !strings.Contains(err.Error(), "flutter pub get") {
		t.Errorf("refusal should tell the developer to resolve dependencies: %v", err)
	}
}

func TestResolvedPackageLibDirReadsTheResolution(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".dart_tool", "package_config.json"),
		`{"configVersion":2,"packages":[{"name":"soroq_flutter","rootUri":"file:///tmp/sf","packageUri":"lib/"}]}`)
	got, err := resolvedPackageLibDir(dir, "soroq_flutter")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean("/tmp/sf/lib") {
		t.Errorf("resolved %q, want /tmp/sf/lib", got)
	}
	if _, err := resolvedPackageLibDir(dir, "absent"); err == nil {
		t.Error("a package missing from the resolution was accepted")
	}
}
