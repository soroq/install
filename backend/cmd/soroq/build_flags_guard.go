package main

import (
	"fmt"
	"os"
	"strings"
)

// Build flags whose interaction with OTA is not verified.
//
// Dart obfuscation (`--obfuscate`, which Flutter requires be paired with `--split-debug-info`) renames
// declarations at compile time. That collides with how Soroq binds a patch:
//
//   - the iOS freehand lane installs redirects BY DECLARATION IDENTITY, so base and candidate must
//     agree on what a declaration is called;
//   - the Android native-AOT lane swaps whole AOT artifacts, which carry their own symbol mapping.
//
// Whether a patch built from an obfuscated candidate binds correctly against an obfuscated base
// depends on whether the two compilations produced the same mapping — which nothing in Soroq
// currently pins, measures or tests. Both outcomes are possible and they are not distinguishable from
// the outside: a wrong binding does not announce itself, it just runs the wrong code or silently fails
// to take effect on a user's device.
//
// So this is deliberately NOT a claim that obfuscation is broken. It is a refusal to ship a binding
// nobody has verified. When someone proves the mapping is stable across the two builds and adds the
// acceptance evidence, this guard should be replaced by that proof rather than merely deleted.
//
// SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS=1 is the explicit opt-in for someone who wants to experiment and
// accepts that the result is unverified.

const unverifiedBuildFlagsOptInEnv = "SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS"

// detectFlavorFlags returns the flavor/custom-target flags present in a passthrough arg list.
func detectFlavorFlags(args []string) []string {
	var found []string
	for _, raw := range args {
		arg := strings.TrimSpace(raw)
		name := arg
		if idx := strings.Index(arg, "="); idx >= 0 {
			name = arg[:idx]
		}
		// Both `--flavor prod` and `--flavor=prod` count; the value is irrelevant to the decision.
		if name == "--flavor" {
			found = append(found, "--flavor")
		}
	}
	return found
}

// guardFlavoredBuild refuses to BUILD a flavored app, because Soroq would then look for the artifact
// in the wrong place.
//
// Flutter writes a flavored build to build/app/outputs/apk/<flavor>/release/ (and the analogous
// bundle path). Soroq's artifact discovery scans only the unflavored locations, so after a flavored
// build it finds nothing from that build -- and, if an older unflavored artifact is still on disk,
// silently returns THAT instead. The stale-artifact guard now catches the dangerous half of this, but
// the honest answer is that Soroq has no flavor-aware release identity at all: it cannot tell two
// flavors apart, so two flavors of one version would collide on the same release id.
//
// There IS a supported path, and it is exercised by tests: build the flavor yourself and hand Soroq
// the exact artifact. That keeps flavor selection where it already works -- in your build -- and keeps
// Soroq bound to a file the developer named on purpose.
func guardFlavoredBuild(args []string) error {
	if len(detectFlavorFlags(args)) == 0 {
		return nil
	}
	if os.Getenv(unverifiedBuildFlagsOptInEnv) == "1" {
		fmt.Fprintf(os.Stderr,
			"warning: building with --flavor. Soroq does not scan flavored output paths and has no\n"+
				"  flavor-aware release identity; you have opted in via %s.\n"+
				"  Verify the registered artifact is the one your flavored build produced.\n",
			unverifiedBuildFlagsOptInEnv)
		return nil
	}
	return fmt.Errorf(`refusing to build with --flavor: Soroq has no flavor support

Flutter writes a flavored build to build/app/outputs/apk/<flavor>/release/, which Soroq's artifact
discovery does not scan. Soroq also has no flavor-aware release identity, so two flavors of the same
version would collide on one release id.

Build the flavor yourself and hand Soroq the exact artifact -- this path is supported and tested:

    flutter build apk --release --flavor <name>
    soroq release android --build=false --artifact build/app/outputs/apk/<name>/release/app-<name>-release.apk

Then patch the same way, with --build=false --candidate-artifact <path>.`)
}

// detectObfuscationFlags returns the obfuscation-related flags present in a passthrough arg list.
//
// Matching is on the FLAG token itself, never on a substring of the whole command line: a
// `--dart-define` whose value happens to contain the word must not trip the guard.
func detectObfuscationFlags(args []string) []string {
	var found []string
	for _, raw := range args {
		arg := strings.TrimSpace(raw)
		name := arg
		if idx := strings.Index(arg, "="); idx >= 0 {
			name = arg[:idx]
		}
		switch name {
		case "--obfuscate", "--split-debug-info":
			found = append(found, name)
		}
	}
	return found
}

// guardUnverifiedBuildFlags refuses a build whose flags put the patch binding outside what Soroq has
// evidence for. It is called before any build starts, so a refusal costs no compile time.
func guardUnverifiedBuildFlags(args []string) error {
	flags := detectObfuscationFlags(args)
	if len(flags) == 0 {
		return nil
	}
	if os.Getenv(unverifiedBuildFlagsOptInEnv) == "1" {
		fmt.Fprintf(os.Stderr,
			"warning: building with %s. Soroq has no acceptance evidence that an obfuscated patch binds\n"+
				"  correctly to an obfuscated base; you have opted in via %s.\n"+
				"  Verify on a real device before shipping this to users.\n",
			strings.Join(dedupeStrings(flags), " "), unverifiedBuildFlagsOptInEnv)
		return nil
	}
	return fmt.Errorf(`refusing to build with %s: Soroq has not verified obfuscated OTA

Dart obfuscation renames declarations. Soroq binds an iOS patch by declaration identity, and an
Android patch carries its own AOT symbol mapping, so a patch built from an obfuscated candidate is
only correct if both compilations produced the same mapping. Nothing in Soroq pins or checks that
today, and a wrong binding does not announce itself — it runs the wrong code, or silently does
nothing, on a user's device.

This is a missing proof, not a known failure. Choose one:

  1. Build without %s (supported and covered by acceptance tests).
  2. Experiment anyway, accepting that the result is unverified:
       %s=1 soroq <your command>
     and confirm the patch actually takes effect on a real device before shipping it.`,
		strings.Join(dedupeStrings(flags), " "),
		strings.Join(dedupeStrings(flags), "/"),
		unverifiedBuildFlagsOptInEnv)
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
