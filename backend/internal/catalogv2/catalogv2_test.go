package catalogv2

import "testing"

// The two revisions this whole change exists to let coexist.
const (
	rev3442 = "f74781f6213447540225edae307acb48bbaaaf34"
	rev3449 = "6b182d2c7585eba26d4edce0f97630effd256c33"
)

func iosMatrix() Doc {
	return Doc{
		Schema:       Schema,
		GeneratedAt:  "2026-09-13T00:00:00Z",
		SigningKeyID: "soroq-toolchain-kid-v1",
		Platforms: map[string]Platform{
			"ios": {Entries: []Entry{
				{FlutterRevision: rev3442, FlutterVersion: "3.44.2",
					FrontendVersion: "fe-3.44.2", ToolchainVersion: "tc-3.44.2"},
				{FlutterRevision: rev3449, FlutterVersion: "3.44.9",
					FrontendVersion: "fe-3.44.9-r5", ToolchainVersion: "tc-3.44.9-r5"},
			}},
		},
	}
}

// THE POINT OF v2: both Flutter versions resolve independently for the same platform, which v1 could
// not express at all.
func TestBothFlutterVersionsCoexistAndResolveIndependently(t *testing.T) {
	doc := iosMatrix()
	if err := doc.Validate(); err != nil {
		t.Fatalf("valid matrix refused: %v", err)
	}
	for _, tc := range []struct{ rev, wantFE, wantTC, wantVer string }{
		{rev3442, "fe-3.44.2", "tc-3.44.2", "3.44.2"},
		{rev3449, "fe-3.44.9-r5", "tc-3.44.9-r5", "3.44.9"},
	} {
		got, err := doc.Select("ios", tc.rev)
		if err != nil {
			t.Fatalf("select %s: %v", tc.rev[:12], err)
		}
		if got.FrontendVersion != tc.wantFE || got.ToolchainVersion != tc.wantTC {
			t.Errorf("select %s = %s/%s; want %s/%s", tc.rev[:12], got.FrontendVersion, got.ToolchainVersion, tc.wantFE, tc.wantTC)
		}
		if got.FlutterVersion != tc.wantVer {
			t.Errorf("select %s version = %q; want %q", tc.rev[:12], got.FlutterVersion, tc.wantVer)
		}
	}
}

// Selecting one revision must not perturb the other: the two entries are independent, not a sequence.
func TestSelectingOneRevisionDoesNotAffectTheOther(t *testing.T) {
	doc := iosMatrix()
	if _, err := doc.Select("ios", rev3449); err != nil {
		t.Fatalf("3.44.9: %v", err)
	}
	got, err := doc.Select("ios", rev3442)
	if err != nil {
		t.Fatalf("3.44.2 after selecting 3.44.9: %v", err)
	}
	if got.ToolchainVersion != "tc-3.44.2" {
		t.Errorf("3.44.2 resolved to %q after selecting 3.44.9", got.ToolchainVersion)
	}
}

// PLANTED: an unknown revision must REFUSE, never fall through to "the only entry" or "the newest".
func TestUnknownRevisionRefusesRatherThanGuessing(t *testing.T) {
	doc := iosMatrix()
	unknown := "0123456789abcdef0123456789abcdef01234567"
	if got, err := doc.Select("ios", unknown); err == nil {
		t.Fatalf("unknown revision resolved to %s/%s; it must refuse", got.FrontendVersion, got.ToolchainVersion)
	}
	// A single-entry platform is the tempting case to "just return the one" — it must still refuse.
	single := Doc{Schema: Schema, Platforms: map[string]Platform{
		"ios": {Entries: []Entry{{FlutterRevision: rev3442, FlutterVersion: "3.44.2", FrontendVersion: "fe", ToolchainVersion: "tc"}}},
	}}
	if _, err := single.Select("ios", unknown); err == nil {
		t.Error("unknown revision resolved against a single-entry platform; it must refuse")
	}
}

// PLANTED: duplicate revisions are ambiguous and must be refused at validation, not tie-broken.
func TestDuplicateRevisionIsRefused(t *testing.T) {
	doc := iosMatrix()
	ios := doc.Platforms["ios"]
	ios.Entries = append(ios.Entries, Entry{
		FlutterRevision: rev3449, FlutterVersion: "3.44.9",
		FrontendVersion: "fe-other", ToolchainVersion: "tc-other",
	})
	doc.Platforms["ios"] = ios
	if err := doc.Validate(); err == nil {
		t.Fatal("duplicate flutter_revision accepted; the selection would be ambiguous")
	}
}

// PLANTED: selection is by revision ONLY. Two entries may share a readable version string without being
// ambiguous, because the version is metadata and never a selector.
func TestDuplicateReadableVersionIsNotAmbiguous(t *testing.T) {
	doc := Doc{Schema: Schema, Platforms: map[string]Platform{
		"ios": {Entries: []Entry{
			{FlutterRevision: rev3442, FlutterVersion: "3.44.9", FrontendVersion: "fe-a", ToolchainVersion: "tc-a"},
			{FlutterRevision: rev3449, FlutterVersion: "3.44.9", FrontendVersion: "fe-b", ToolchainVersion: "tc-b"},
		}},
	}}
	if err := doc.Validate(); err != nil {
		t.Fatalf("same version on distinct revisions refused: %v", err)
	}
	got, err := doc.Select("ios", rev3449)
	if err != nil || got.FrontendVersion != "fe-b" {
		t.Fatalf("select by revision got %q (err %v); want fe-b", got.FrontendVersion, err)
	}
}

// PLANTED: malformed structure must be refused, each for its own named reason.
func TestMalformedDocumentsAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  Doc
	}{
		{"wrong schema", Doc{Schema: "soroq.catalog.v1", Platforms: map[string]Platform{
			"ios": {Entries: []Entry{{FlutterRevision: rev3442, FlutterVersion: "v", FrontendVersion: "f", ToolchainVersion: "t"}}}}}},
		{"no platforms", Doc{Schema: Schema, Platforms: map[string]Platform{}}},
		{"empty entries", Doc{Schema: Schema, Platforms: map[string]Platform{"ios": {Entries: nil}}}},
		{"short revision", Doc{Schema: Schema, Platforms: map[string]Platform{
			"ios": {Entries: []Entry{{FlutterRevision: "f74781f6", FlutterVersion: "v", FrontendVersion: "f", ToolchainVersion: "t"}}}}}},
		{"upper-case revision", Doc{Schema: Schema, Platforms: map[string]Platform{
			"ios": {Entries: []Entry{{FlutterRevision: "F74781F6213447540225EDAE307ACB48BBAAAF34", FlutterVersion: "v", FrontendVersion: "f", ToolchainVersion: "t"}}}}}},
		{"missing frontend", Doc{Schema: Schema, Platforms: map[string]Platform{
			"ios": {Entries: []Entry{{FlutterRevision: rev3442, FlutterVersion: "v", ToolchainVersion: "t"}}}}}},
		{"missing toolchain", Doc{Schema: Schema, Platforms: map[string]Platform{
			"ios": {Entries: []Entry{{FlutterRevision: rev3442, FlutterVersion: "v", FrontendVersion: "f"}}}}}},
		{"missing readable version", Doc{Schema: Schema, Platforms: map[string]Platform{
			"ios": {Entries: []Entry{{FlutterRevision: rev3442, FrontendVersion: "f", ToolchainVersion: "t"}}}}}},
		{"upper-case platform", Doc{Schema: Schema, Platforms: map[string]Platform{
			"IOS": {Entries: []Entry{{FlutterRevision: rev3442, FlutterVersion: "v", FrontendVersion: "f", ToolchainVersion: "t"}}}}}},
	} {
		if err := tc.doc.Validate(); err == nil {
			t.Errorf("%s: accepted; it must refuse", tc.name)
		}
	}
}

// An unknown platform refuses and names what IS available, because a stale catalog is the usual cause.
func TestUnknownPlatformRefuses(t *testing.T) {
	if _, err := iosMatrix().Select("android", rev3442); err == nil {
		t.Fatal("unknown platform resolved; it must refuse")
	}
}

// A malformed selector argument refuses rather than being normalized into a near-miss.
func TestMalformedSelectorRefuses(t *testing.T) {
	for _, bad := range []string{"", "f74781f6", "zzzz81f6213447540225edae307acb48bbaaaf34"} {
		if _, err := iosMatrix().Select("ios", bad); err == nil {
			t.Errorf("selector %q accepted; it must refuse", bad)
		}
	}
}
