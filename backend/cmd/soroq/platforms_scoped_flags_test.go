package main

// AMBIGUOUS COMBINED FLAGS.
//
// `soroq release --platforms=android,ios --release-id foo` cannot be honoured. A release record stores
// exactly one platform and is keyed by its id, so one explicit id cannot identify two records — it is
// the same collision the derived per-platform ids exist to prevent, only supplied by hand. The same
// holds for --toolchain, which names one installed per-platform toolchain.
//
// The refusal must land BEFORE either lane runs. A command rejected halfway would leave one platform
// built and possibly registered, which is strictly worse than refusing.

import (
	"strings"
	"testing"
)

func TestCombinedRunRefusesAnAmbiguousReleaseID(t *testing.T) {
	err := validateCombinedPlatformFlags("release", []string{"android", "ios"},
		[]string{"--release-id", "foo"})
	if err == nil {
		t.Fatal("two platforms accepted a single --release-id; that cannot identify two release records")
	}
	msg := err.Error()
	for _, want := range []string{
		"--release-id", "android", "ios",
		"--release-id-android", "--release-id-ios", // a real alternative, not just a refusal
		"Nothing has been built or registered",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, msg)
		}
	}
}

func TestCombinedRunRefusesAnAmbiguousToolchain(t *testing.T) {
	err := validateCombinedPlatformFlags("patch", []string{"android", "ios"},
		[]string{"--toolchain=soroq-ios-3.44.2"})
	if err == nil {
		t.Fatal("two platforms accepted a single --toolchain; toolchains are per-platform")
	}
	if !strings.Contains(err.Error(), "--toolchain-ios") {
		t.Errorf("refusal does not offer the per-platform override:\n%v", err)
	}
}

// A SINGLE platform is unambiguous — the existing contract must not regress.
func TestSinglePlatformStillAcceptsExplicitScopedFlags(t *testing.T) {
	for _, platform := range []string{"android", "ios"} {
		if err := validateCombinedPlatformFlags("release", []string{platform},
			[]string{"--release-id", "foo", "--toolchain", "tc"}); err != nil {
			t.Errorf("%s alone was refused: %v", platform, err)
		}
	}
}

// No scoped flag at all: derivation handles it, nothing to refuse.
func TestCombinedRunWithoutScopedFlagsIsAccepted(t *testing.T) {
	if err := validateCombinedPlatformFlags("release", []string{"android", "ios"},
		[]string{"--rollout", "100"}); err != nil {
		t.Errorf("an unambiguous combined run was refused: %v", err)
	}
}

// ZERO SIDE EFFECTS: a rejected combined command must not invoke ANY lane. Proven with a counter that
// must remain at zero — the refusal happens during validation, before runPerPlatform is reached.
func TestRejectedCombinedCommandNeverInvokesALane(t *testing.T) {
	lanesRun := 0
	run := func(platforms []string, args []string) error {
		if err := validateCombinedPlatformFlags("release", platforms, args); err != nil {
			return err
		}
		return runPerPlatform("release", platforms, func(string) error {
			lanesRun++
			return nil
		})
	}
	if err := run([]string{"android", "ios"}, []string{"--release-id", "foo"}); err == nil {
		t.Fatal("expected refusal")
	}
	if lanesRun != 0 {
		t.Fatalf("%d lane(s) ran despite the refusal; a rejected command must have no side effects", lanesRun)
	}
}

// PER-PLATFORM OVERRIDES are unambiguous and must reach only their own lane.
func TestPerPlatformOverridesReachOnlyTheirOwnLane(t *testing.T) {
	args := []string{
		"--release-id-android", "droid-1",
		"--release-id-ios", "ios-1",
		"--rollout", "100",
	}

	android := applyPlatformScopedOverrides("android", args)
	ios := applyPlatformScopedOverrides("ios", args)

	if v, _ := flagValue(android, "release-id"); v != "droid-1" {
		t.Errorf("android received release-id %q, want droid-1", v)
	}
	if v, _ := flagValue(ios, "release-id"); v != "ios-1" {
		t.Errorf("ios received release-id %q, want ios-1", v)
	}
	// Neither lane may see the other platform's override, nor any scoped form.
	for name, out := range map[string][]string{"android": android, "ios": ios} {
		joined := strings.Join(out, " ")
		for _, leaked := range []string{"--release-id-android", "--release-id-ios"} {
			if strings.Contains(joined, leaked) {
				t.Errorf("%s lane still carries %s: %v", name, leaked, out)
			}
		}
		if v, _ := flagValue(out, "rollout"); v != "100" {
			t.Errorf("%s lane lost an unrelated flag: %v", name, out)
		}
	}
	if strings.Contains(strings.Join(android, " "), "ios-1") {
		t.Error("the android lane received the iOS release id")
	}
	if strings.Contains(strings.Join(ios, " "), "droid-1") {
		t.Error("the ios lane received the Android release id")
	}
}

// An override must survive the derivation step: explicit beats derived.
func TestPerPlatformOverrideBeatsDerivation(t *testing.T) {
	dir := projectWithIdentity(t, "dev.soroq.canonapp", "1.0.0+1")
	args := applyPlatformScopedOverrides("ios", pinnedToolchain("--release-id-ios", "chosen-by-hand"))
	out, err := withDerivedFlags("ios", dir, args)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := flagValue(out, "release-id"); v != "chosen-by-hand" {
		t.Errorf("derivation overrode the explicit per-platform id: %q", v)
	}
	if strings.Count(strings.Join(out, " "), "--release-id") != 1 {
		t.Errorf("--release-id appears more than once: %v", out)
	}
}

// A combined run using ONLY per-platform overrides is accepted — that is the whole point of offering them.
func TestCombinedRunWithPerPlatformOverridesIsAccepted(t *testing.T) {
	args := []string{"--release-id-android", "a", "--release-id-ios", "i"}
	if err := validateCombinedPlatformFlags("release", []string{"android", "ios"}, args); err != nil {
		t.Fatalf("per-platform overrides were refused: %v", err)
	}
	a := applyPlatformScopedOverrides("android", args)
	i := applyPlatformScopedOverrides("ios", args)
	av, _ := flagValue(a, "release-id")
	iv, _ := flagValue(i, "release-id")
	if av == iv {
		t.Fatalf("both platforms resolved to release id %q", av)
	}
}

// removeFlag must handle both spellings and leave everything else alone.
func TestRemoveFlagHandlesBothSpellings(t *testing.T) {
	got := removeFlag([]string{"--a", "1", "--b=2", "--keep", "3"}, "a")
	if strings.Join(got, " ") != "--b=2 --keep 3" {
		t.Errorf("space form not removed cleanly: %v", got)
	}
	got = removeFlag([]string{"--a", "1", "--b=2", "--keep", "3"}, "b")
	if strings.Join(got, " ") != "--a 1 --keep 3" {
		t.Errorf("equals form not removed cleanly: %v", got)
	}
}
