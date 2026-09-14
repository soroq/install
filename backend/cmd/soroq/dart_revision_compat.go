package main

import "strings"

// Legacy dart_revision compatibility, bound to EXACT immutable artifact identities.
//
// THE PROBLEM THIS SOLVES, precisely. The live 3.44.2 frontend is SHARED by android and ios and
// declares the Dart VERSION STRING "3.13.0-103.1.beta", because no Dart commit SHA was ever published
// for that Android-derived build. The android toolchain declares the same string; the ios toolchain
// declares the Dart commit 9576691c… for the same Dart. So one signed frontend legitimately carries a
// different FORM of the same identity from the ios toolchain it is paired with.
//
// Two checks objected, and `soroq setup ios` has been REFUSING the live catalog because of the first:
//
//	1. checkFrontendIdentity demands a 40-hex dart_revision from every frontend;
//	2. validatePairIdentity (v2) demands the pair's two dart_revisions be equal.
//
// WHY A TABLE RATHER THAN A RULE. The tempting fixes are both wrong. Dropping the SHA requirement
// would accept any string from any frontend forever. Accepting "version string vs SHA" as a general
// shape would let ANY mismatched pair through, which is exactly the class of error the check exists to
// catch — a frontend and toolchain built against genuinely different Dart SDKs.
//
// So the equivalence is enumerated, and each entry names BOTH the immutable version identity AND the
// exact value it may declare. Those version strings are immutable and signed, so an entry cannot be
// widened by re-publishing: a different artifact has a different version, and the same artifact
// declaring a different value no longer matches. Anything not enumerated is refused as before, and R5
// — whose frontend and toolchain both declare the same Dart commit — never consults this table at all.
//
// This buys backward compatibility for artifacts that already exist. It is deliberately NOT a
// forward-looking mechanism: a new pair must agree on dart_revision.

// legacyNonSHADartRevisions maps an immutable artifact version to the exact non-SHA dart_revision it is
// permitted to declare. Consulted ONLY when the value is not a 40-hex commit.
var legacyNonSHADartRevisions = map[string]string{
	// The 3.44.2 frontend shared by android and ios.
	"soroq-flutter-frontend-f74781f6-7277aaec-1a113cf9-clean-r5": "3.13.0-103.1.beta",
	// The android 3.44.2 toolchain, which legitimately publishes the same version string.
	"soroq-android-3.44.2-release-f74781f6-clean-r14": "3.13.0-103.1.beta",
}

// allowsLegacyNonSHADartRevision reports whether version may declare exactly this non-SHA value.
func allowsLegacyNonSHADartRevision(version, dartRevision string) bool {
	want, ok := legacyNonSHADartRevisions[strings.TrimSpace(version)]
	return ok && want == strings.TrimSpace(dartRevision)
}

// legacyDartPairKey identifies one exact {frontend, toolchain} pairing.
type legacyDartPairKey struct{ frontendVersion, toolchainVersion string }

// legacyDartEquivalentPairs enumerates pairs whose two dart_revisions differ in FORM while naming the
// same Dart. BOTH declared values are pinned, so the exemption covers one specific pairing of one
// specific pair of artifacts and nothing else.
var legacyDartEquivalentPairs = map[legacyDartPairKey][2]string{
	{
		frontendVersion:  "soroq-flutter-frontend-f74781f6-7277aaec-1a113cf9-clean-r5",
		toolchainVersion: "soroq-ios-3.44.2-production-f74781f6-3499c008-clean-r5",
	}: {"3.13.0-103.1.beta", "9576691c37d84d3b66a9722e4fadacc764f04b21"},
}

// allowsLegacyDartRevisionMismatch reports whether this EXACT pair may declare these EXACT two values.
//
// Every component is checked: both version identities and both declared revisions. A pair that merely
// resembles an enumerated one — same frontend, different toolchain, or the right pair with a different
// Dart — does not match and is refused.
func allowsLegacyDartRevisionMismatch(frontendVersion, toolchainVersion, frontendDart, toolchainDart string) bool {
	want, ok := legacyDartEquivalentPairs[legacyDartPairKey{
		frontendVersion:  strings.TrimSpace(frontendVersion),
		toolchainVersion: strings.TrimSpace(toolchainVersion),
	}]
	if !ok {
		return false
	}
	return want[0] == strings.TrimSpace(frontendDart) && want[1] == strings.TrimSpace(toolchainDart)
}
