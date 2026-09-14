package main

// dart_revision_compat_test.go — the legacy binding, using the EXACT live 3.44.2 identities and values.
//
// The value of these tests is that they pin the exemption to real, immutable identities. If someone
// later widens the table into a pattern, the "unrecognised mismatch" cases below start passing and say
// so.

import "testing"

const (
	liveFE3442     = "soroq-flutter-frontend-f74781f6-7277aaec-1a113cf9-clean-r5"
	liveIOSTC3442  = "soroq-ios-3.44.2-production-f74781f6-3499c008-clean-r5"
	liveANDTC3442  = "soroq-android-3.44.2-release-f74781f6-clean-r14"
	liveDartString = "3.13.0-103.1.beta"
	liveIOSDartSHA = "9576691c37d84d3b66a9722e4fadacc764f04b21"
	r5FE           = "soroq-flutter-frontend-6b182d2c-376d5a48-d055c615-private-state-r5"
	r5TC           = "soroq-ios-3.44.9-release-6b182d2c_5a2a6a42-private_state-r5"
	r5Dart         = "d684a576a6aa954ae107a03b2b4e1d61c3bebe93"
)

// The live 3.44.2 frontend may declare its version-string dart_revision.
func TestLiveFrontendVersionStringDartIsAccepted(t *testing.T) {
	if !allowsLegacyNonSHADartRevision(liveFE3442, liveDartString) {
		t.Fatal("the live 3.44.2 frontend is not accepted; `soroq setup ios` stays broken")
	}
	if err := checkFrontendIdentity(frontendManifest{
		SoroqFrontendVersion:   liveFE3442,
		FlutterRevision:        "f74781f6213447540225edae307acb48bbaaaf34",
		DartRevision:           liveDartString,
		EngineRevision:         "3499c0081904b3796be1ba0bf266d99f9ba04399",
		CompatibleToolchainIDs: []string{liveIOSTC3442},
	}); err != nil {
		t.Fatalf("live frontend manifest shape refused: %v", err)
	}
}

// PLANTED: the exemption is bound to the EXACT value, not to "any non-SHA string".
func TestOtherNonSHADartValuesAreStillRefused(t *testing.T) {
	for _, bad := range []string{"3.13.0-103.2.beta", "3.14.0", "whatever", ""} {
		if allowsLegacyNonSHADartRevision(liveFE3442, bad) {
			t.Errorf("the live frontend was allowed to declare %q", bad)
		}
	}
}

// PLANTED: the exemption is bound to the EXACT artifact, not to "any frontend".
func TestOtherFrontendsCannotUseTheLegacyForm(t *testing.T) {
	for _, v := range []string{r5FE, "soroq-flutter-frontend-someone-else", ""} {
		if allowsLegacyNonSHADartRevision(v, liveDartString) {
			t.Errorf("frontend %q was allowed the legacy dart form", v)
		}
	}
	// and a non-SHA dart on an unlisted frontend still fails the identity check
	if err := checkFrontendIdentity(frontendManifest{
		SoroqFrontendVersion:   r5FE,
		FlutterRevision:        "6b182d2c7585eba26d4edce0f97630effd256c33",
		DartRevision:           liveDartString,
		EngineRevision:         "5a2a6a42cce67f965cf540fcecf616faca624aa1",
		CompatibleToolchainIDs: []string{r5TC},
	}); err == nil {
		t.Error("an unlisted frontend declaring the legacy dart form was accepted")
	}
}

// The EXACT live ios 3.44.2 pairing may mismatch in form; nothing else may.
func TestLegacyPairMismatchIsBoundToTheExactPair(t *testing.T) {
	if !allowsLegacyDartRevisionMismatch(liveFE3442, liveIOSTC3442, liveDartString, liveIOSDartSHA) {
		t.Fatal("the live ios 3.44.2 pairing is not accepted")
	}
	for _, tc := range []struct {
		name                    string
		fe, tc_, feDart, tcDart string
	}{
		{"different toolchain", liveFE3442, r5TC, liveDartString, liveIOSDartSHA},
		{"different frontend", r5FE, liveIOSTC3442, liveDartString, liveIOSDartSHA},
		{"right pair, different frontend dart", liveFE3442, liveIOSTC3442, "3.13.0-103.2.beta", liveIOSDartSHA},
		{"right pair, different toolchain dart", liveFE3442, liveIOSTC3442, liveDartString, r5Dart},
		{"android toolchain substituted", liveFE3442, liveANDTC3442, liveDartString, liveIOSDartSHA},
	} {
		if allowsLegacyDartRevisionMismatch(tc.fe, tc.tc_, tc.feDart, tc.tcDart) {
			t.Errorf("%s: an unrecognised mismatch was accepted", tc.name)
		}
	}
}

// R5 KEEPS EXACT SHA-TO-SHA ENFORCEMENT: it never consults the table, and a mismatch is refused.
func TestR5StillRequiresExactShaToShaAgreement(t *testing.T) {
	fe := frontendManifest{FlutterRevision: "6b182d2c7585eba26d4edce0f97630effd256c33", DartRevision: r5Dart,
		CompatibleToolchainIDs: []string{r5TC}}
	tc := cliManifest{Platform: "ios", FlutterRevision: "6b182d2c7585eba26d4edce0f97630effd256c33",
		DartRevision: r5Dart, FlutterVersion: "3.44.9"}
	if err := validatePairIdentity("ios", "6b182d2c7585eba26d4edce0f97630effd256c33", "3.44.9", r5FE, r5TC, fe, tc); err != nil {
		t.Fatalf("the matching r5 pair was refused: %v", err)
	}
	tc.DartRevision = liveIOSDartSHA // a DIFFERENT Dart commit
	if err := validatePairIdentity("ios", "6b182d2c7585eba26d4edce0f97630effd256c33", "3.44.9", r5FE, r5TC, fe, tc); err == nil {
		t.Fatal("r5 accepted a dart_revision mismatch; SHA-to-SHA enforcement was lost")
	}
}

// The live ios 3.44.2 pair passes the whole pair contract, including the legacy dart exemption.
func TestLiveIOS3442PairPassesPairIdentity(t *testing.T) {
	fe := frontendManifest{FlutterRevision: "f74781f6213447540225edae307acb48bbaaaf34", DartRevision: liveDartString,
		CompatibleToolchainIDs: []string{liveANDTC3442, liveIOSTC3442}}
	tc := cliManifest{Platform: "ios", FlutterRevision: "f74781f6213447540225edae307acb48bbaaaf34",
		DartRevision: liveIOSDartSHA, FlutterVersion: "3.44.2"}
	if err := validatePairIdentity("ios", "f74781f6213447540225edae307acb48bbaaaf34", "3.44.2",
		liveFE3442, liveIOSTC3442, fe, tc); err != nil {
		t.Fatalf("live ios 3.44.2 pair refused: %v", err)
	}
}

// ITEM 3: a v2 entry requires the toolchain to DECLARE flutter_version, and to match.
func TestV2EntryRequiresToolchainFlutterVersion(t *testing.T) {
	fe := frontendManifest{FlutterRevision: "6b182d2c7585eba26d4edce0f97630effd256c33", DartRevision: r5Dart,
		CompatibleToolchainIDs: []string{r5TC}}
	base := cliManifest{Platform: "ios", FlutterRevision: "6b182d2c7585eba26d4edce0f97630effd256c33", DartRevision: r5Dart}

	missing := base // no FlutterVersion at all
	if err := validatePairIdentity("ios", "6b182d2c7585eba26d4edce0f97630effd256c33", "3.44.9", r5FE, r5TC, fe, missing); err == nil {
		t.Error("a toolchain declaring no flutter_version was accepted for a v2 entry")
	}
	wrong := base
	wrong.FlutterVersion = "3.44.2"
	if err := validatePairIdentity("ios", "6b182d2c7585eba26d4edce0f97630effd256c33", "3.44.9", r5FE, r5TC, fe, wrong); err == nil {
		t.Error("a contradictory flutter_version was accepted")
	}
	ok := base
	ok.FlutterVersion = "3.44.9"
	if err := validatePairIdentity("ios", "6b182d2c7585eba26d4edce0f97630effd256c33", "3.44.9", r5FE, r5TC, fe, ok); err != nil {
		t.Errorf("a matching flutter_version was refused: %v", err)
	}
}
