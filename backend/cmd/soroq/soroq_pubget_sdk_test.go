package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// HOSTILE CONTROL: an incompatible Flutter first in PATH must never be used for the isolated-workspace
// resolve.
//
// runFlutterPubGetIn used to fall back to exec.LookPath("flutter") whenever the Soroq frontend could
// not be resolved. On a developer machine that is typically a stable-channel install with an older
// Dart, and the resolve then failed against the WRONG SDK:
//
//	The current Dart SDK version is 3.9.2.
//	Because <app> depends on dynamic_modules from path which requires SDK version ^3.12.0-0,
//	version solving failed.
//
// which reads as a defect in the app's pubspec. The app's SDK constraint is correct and must not be
// weakened to accommodate a wrong SDK; the resolver must refuse instead.
func TestPubGetNeverFallsBackToAFlutterOnPath(t *testing.T) {
	dir := t.TempDir()
	// A hostile `flutter` that would "succeed" if it were ever chosen, so a fallback cannot hide.
	fake := filepath.Join(dir, "flutter")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho HOSTILE-FLUTTER-WAS-USED >&2\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Prove the hostile binary really is first in PATH, so a passing test cannot be a lookup miss.
	if got, err := exec.LookPath("flutter"); err != nil || got != fake {
		t.Fatalf("hostile flutter is not first in PATH (got %q, err %v); this control would prove nothing", got, err)
	}
	// No frontend available: an empty HOME has no store, so resolveSoroqFlutterBin must fail.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SOROQ_FLUTTER_BIN", "")

	err := runFlutterPubGetIn(t.TempDir())
	if err == nil {
		t.Fatal("resolution SUCCEEDED with no Soroq frontend; it must have used the Flutter on PATH")
	}
	if !strings.Contains(err.Error(), "no Soroq frontend Flutter is available") {
		t.Errorf("refusal does not name the frontend problem: %v", err)
	}
	if strings.Contains(err.Error(), "HOSTILE") {
		t.Errorf("the hostile flutter on PATH was executed: %v", err)
	}
}
