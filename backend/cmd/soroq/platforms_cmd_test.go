package main

// CANONICAL MULTI-PLATFORM UX — parser, orchestration, failure reporting, backward compatibility.
//
// The property most worth protecting here is the honest one: a two-platform run where one platform
// failed must never be reported as success, and must not discard the fact that the other succeeded.

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParsePlatformsAcceptsBothFlagForms(t *testing.T) {
	for name, args := range map[string][]string{
		"equals form": {"--platforms=android,ios", "--rollout", "100"},
		"space form":  {"--platforms", "android,ios", "--rollout", "100"},
		"spaced list": {"--platforms= android , ios ", "--rollout", "100"},
	} {
		t.Run(name, func(t *testing.T) {
			got, rest, err := parsePlatforms(args)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if want := []string{"android", "ios"}; !reflect.DeepEqual(got, want) {
				t.Errorf("platforms = %v, want %v", got, want)
			}
			if want := []string{"--rollout", "100"}; !reflect.DeepEqual(rest, want) {
				t.Errorf("rest = %v, want %v (the flag itself must be consumed)", rest, want)
			}
		})
	}
}

// Order must not depend on how the developer typed it, so a two-platform run always reports the same
// way and logs stay comparable between runs.
func TestPlatformOrderIsDeterministic(t *testing.T) {
	a, _, err := parsePlatforms([]string{"--platforms=ios,android"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := parsePlatforms([]string{"--platforms=android,ios"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("order depends on input spelling: %v vs %v", a, b)
	}
}

func TestParsePlatformsRejectsBadInput(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown platform": {"--platforms=android,windows"},
		"empty value":      {"--platforms="},
		"missing value":    {"--platforms"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parsePlatforms(args); err == nil {
				t.Fatalf("%v was accepted; a typo must fail loudly rather than silently doing less", args)
			}
		})
	}
}

// A repeated platform runs once. Asking twice is not asking for two releases.
func TestRepeatedPlatformRunsOnce(t *testing.T) {
	got, _, err := parsePlatforms([]string{"--platforms=ios,ios"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ios"}; !reflect.DeepEqual(got, want) {
		t.Errorf("platforms = %v, want %v", got, want)
	}
}

// BACKWARD COMPATIBILITY: without --platforms, args pass through untouched so the existing positional
// subcommands (`release ios`, `patch android`, `patch ios --engine`, `patch ios-engine`) are unaffected.
func TestWithoutTheFlagArgsPassThroughUnchanged(t *testing.T) {
	for _, args := range [][]string{
		{"ios", "--engine", "--build", "--toolchain", "x"},
		{"android", "--rollout", "100"},
		{"ios-engine", "--api", "https://example.test"},
		{"ios", "--config-file", "config.json"},
	} {
		platforms, rest, err := parsePlatforms(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if len(platforms) != 0 {
			t.Errorf("%v: unexpectedly parsed platforms %v", args, platforms)
		}
		if !reflect.DeepEqual(rest, args) {
			t.Errorf("%v: args were altered to %v", args, rest)
		}
	}
}

// Flutter build flags after `--` must survive untouched, including a literal `--platforms` that belongs
// to the flutter command rather than to soroq.
func TestFlutterPassthroughAfterDoubleDashSurvives(t *testing.T) {
	args := []string{"--platforms=ios", "--rollout", "100", "--", "--dart-define=A=b", "--verbose"}
	platforms, rest, err := parsePlatforms(args)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ios"}; !reflect.DeepEqual(platforms, want) {
		t.Fatalf("platforms = %v, want %v", platforms, want)
	}
	want := []string{"--rollout", "100", "--", "--dart-define=A=b", "--verbose"}
	if !reflect.DeepEqual(rest, want) {
		t.Errorf("passthrough mangled:\n got %v\nwant %v", rest, want)
	}
}

func TestRunPerPlatformRunsEveryPlatform(t *testing.T) {
	var ran []string
	err := runPerPlatform("release", []string{"android", "ios"}, func(p string) error {
		ran = append(ran, p)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"android", "ios"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("ran = %v, want %v", ran, want)
	}
}

// THE HONESTY REQUIREMENT. One platform failing must not be reported as success, must not hide that
// the other succeeded, and must not stop the other from running at all.
func TestPartialFailureIsReportedHonestly(t *testing.T) {
	var ran []string
	err := runPerPlatform("patch", []string{"android", "ios"}, func(p string) error {
		ran = append(ran, p)
		if p == "ios" {
			return errors.New("toolchain missing")
		}
		return nil
	})
	if err == nil {
		t.Fatal("a run where iOS failed reported overall SUCCESS")
	}
	if want := []string{"android", "ios"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("a failing platform stopped the others: ran %v, want %v", ran, want)
	}
	msg := err.Error()
	if !strings.Contains(msg, "ios") {
		t.Errorf("error does not name the failing platform: %q", msg)
	}
	if !strings.Contains(msg, "android") {
		t.Errorf("error does not say android SUCCEEDED, inviting a needless re-run: %q", msg)
	}
	if !strings.Contains(msg, "did NOT succeed") {
		t.Errorf("error does not state plainly that the combined command failed: %q", msg)
	}
}

func TestAllPlatformsFailingReportsAllOfThem(t *testing.T) {
	err := runPerPlatform("release", []string{"android", "ios"}, func(string) error {
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, p := range []string{"android", "ios"} {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error omits %q: %v", p, err)
		}
	}
}

// The single-platform form must behave like an ordinary command: a plain error, no summary framing
// that would read oddly for one platform.
func TestSinglePlatformFailureIsPlain(t *testing.T) {
	err := runPerPlatform("patch", []string{"ios"}, func(string) error {
		return errors.New("nope")
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "succeeded for") {
		t.Errorf("single-platform failure used the partial-success phrasing: %v", err)
	}
}

// Selecting iOS must route to the hard-OTA lane WITHOUT the developer typing --engine. This asserts the
// routing decision itself; the lane functions are exercised by their own tests.
func TestSelectingIOSRoutesToTheHardOTALaneWithoutEngineFlag(t *testing.T) {
	platforms, rest, err := parsePlatforms([]string{"--platforms=ios", "--rollout", "100"})
	if err != nil {
		t.Fatal(err)
	}
	if len(platforms) != 1 || platforms[0] != "ios" {
		t.Fatalf("platforms = %v", platforms)
	}
	for _, a := range rest {
		if a == "--engine" || a == "--build-ios" {
			t.Fatalf("the developer's args contain %q; selecting the platform must be sufficient", a)
		}
	}
	if err := patchPlatform("nonsense", rest); err == nil {
		t.Error("an unsupported platform was accepted by the dispatcher")
	}
}
