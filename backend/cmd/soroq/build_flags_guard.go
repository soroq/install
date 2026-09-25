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
// SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS=1 no longer applies to --flavor either: flavors are supported on
// the Android release/patch routes and the iOS app-build leg (flavor.go), and refused outright on the
// routes that would persist a flavor-unaware baseline. It is NOT honoured for obfuscation: an obfuscated build that proceeds without
// capability authorization produces a baseline with no map and no binding, and every patch against it
// then installs and resolves nothing. An experiment must never leave a reusable baseline, artifact or
// publication behind, so obfuscation is refused outright rather than warned about.

const unverifiedBuildFlagsOptInEnv = "SOROQ_ALLOW_UNVERIFIED_BUILD_FLAGS"

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
//
// [auth] is the TOOLCHAIN's answer to "can I translate identities into an obfuscated base's
// namespace?", resolved from the engine bundle's own capability declaration. A nil auth -- which is
// what every route that has not been wired passes -- means no toolchain was consulted, and the build
// stays refused. That direction is the safe one: a route that forgot to resolve authorization refuses
// rather than permits.
//
// NOTE what this does NOT do. Authorization permits the obfuscated BUILD; it does not by itself make
// the patch bind. The binding is completed by the captured map, the translating compilation and the
// translated ABI, each of which fails closed on its own. An R6-capable toolchain with any of those
// missing still produces a refusal, which is what the "obfuscated command still refused despite an
// R6-capable toolchain" control asserts.
func guardUnverifiedBuildFlags(args []string, auth *freehandObfuscationAuthorization) error {
	flags := detectObfuscationFlags(args)
	if len(flags) == 0 {
		return nil
	}
	if auth != nil && auth.Allowed {
		fmt.Fprintf(os.Stderr,
			"soroq: building with %s. The toolchain declares %s (%s), so this base's identities will be\n"+
				"  captured into a release-side obfuscation map and every patch against it will be translated\n"+
				"  through that map.\n",
			strings.Join(dedupeStrings(flags), " "),
			freehandObfuscatedIdentityTranslationCapability, auth.Reason)
		return nil
	}
	reason := "no toolchain capability was resolved for this command"
	if auth != nil && auth.Reason != "" {
		reason = auth.Reason
	}
	return fmt.Errorf(`refusing to build with %s: this toolchain cannot bind an obfuscated base

Dart obfuscation renames declarations, and Soroq binds an iOS patch BY DECLARATION IDENTITY. A patch
carrying source-level names misses an obfuscated base entirely, and misses silently — it runs nothing
and reports success.

Translating a module's identities into the base's namespace needs a toolchain whose dart2bytecode
declares %s. This one does not:

  %s

Choose one:

  1. Build without %s (supported and covered by acceptance tests).
  2. Install a toolchain that declares the capability, and rebuild the BASE with it — an existing base
     has no captured obfuscation map, so no patch can be translated against it.
%s does NOT apply here. It used to let this command continue, and the result was worse than a
refusal: the release recorded no obfuscation binding and no captured map, then persisted an actually
obfuscated base as if it were an ordinary one. Every later patch against that baseline compiled,
signed and installed cleanly and resolved nothing. An experiment must not be able to leave a reusable
baseline, patch artifact or publication behind, so the override is refused on this route.`,
		strings.Join(dedupeStrings(flags), " "),
		freehandObfuscatedIdentityTranslationCapability,
		reason,
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
