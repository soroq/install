package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A SUPPORT BUNDLE IS TESTED BY PLANTING SECRETS IN IT.
//
// The only way to know redaction works is to put a secret where the bundle will find it and require
// that it does not come out. Every case below plants one, in a different shape and a different place.
//
// The positive control matters as much: a bundle that redacted EVERYTHING would pass all of these and
// be useless, so the last test requires the diagnostic content to survive.

// planted values. Each is distinctive enough that finding it in the output is unambiguous.
const (
	plantedToken = "sk-live-9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c"
	// gluedVendorToken is reachable ONLY by its vendor prefix: its body is letters, so no entropy rule
	// can see it, and it carries no key word beside it.
	gluedVendorToken = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh"
	plantedPassword  = "hunter2-correct-horse-battery"
	plantedSeed      = "MC4CAQAwBQYDK2VwBCIEIHhkZmFzZGZhc2RmYXNkZmFzZGZhc2RmYXNkZmFzZGZh"
	plantedHex       = "b57f04c6e3e6d3c98b1c2a7f8e9d0c1b2a3f4e5d6c7b8a9f0e1d2c3b4a5f6e7d"
)

func TestRedactionRemovesEverySecretShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  string
		secret string
		keeps  string // context that must SURVIVE, so redaction stays diagnosable
	}{
		{
			name:   "a database URL with a password in it",
			input:  "postgresql://neondb_owner:" + plantedPassword + "@ep-dry-lake.aws.neon.tech/neondb",
			secret: plantedPassword,
			keeps:  "postgresql://",
		},
		{
			name:   "an authorization header",
			input:  "Authorization: Bearer " + plantedToken,
			secret: plantedToken,
			keeps:  "Authorization",
		},
		{
			name:   "a token assignment in a config file",
			input:  "operator_token: " + plantedToken,
			secret: plantedToken,
			keeps:  "operator_token",
		},
		{
			name:   "an api key with a hyphenated name",
			input:  "api-key = " + plantedToken,
			secret: plantedToken,
			keeps:  "api-key",
		},
		{
			name:   "a signed artifact ticket in a URL",
			input:  "https://api.soroq.dev/v1/patches/p1/bundle?ticket=" + plantedToken,
			secret: plantedToken,
			keeps:  "ticket=",
		},
		{
			name:   "a PEM private key",
			input:  "-----BEGIN PRIVATE KEY-----\n" + plantedSeed + "\n-----END PRIVATE KEY-----",
			secret: plantedSeed,
			keeps:  "",
		},
		{
			name:   "a bare base64 seed with no label at all",
			input:  "value " + plantedSeed,
			secret: plantedSeed,
			keeps:  "value",
		},
		{
			name:   "a bare hex key with no label at all",
			input:  "signing " + plantedHex,
			secret: plantedHex,
			keeps:  "signing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.input)
			if strings.Contains(got, tc.secret) {
				t.Errorf("the secret survived redaction:\n  in:  %s\n  out: %s", tc.input, got)
			}
			if !strings.Contains(got, REDACTED) {
				t.Errorf("nothing was marked as redacted, so a reader cannot tell a removed value "+
					"from one that was never set:\n  out: %s", got)
			}
			if tc.keeps != "" && !strings.Contains(got, tc.keeps) {
				t.Errorf("redaction removed the context too, leaving the bundle undiagnosable:\n"+
					"  wanted to keep %q\n  out: %s", tc.keeps, got)
			}
		})
	}
}

// THE POSITIVE CONTROL. A redactor that returned "[REDACTED]" for everything would pass every test
// above. Ordinary diagnostic content must come through untouched.
func TestRedactionLeavesDiagnosticContentAlone(t *testing.T) {
	input := strings.Join([]string{
		"app_id: com.example.myapp",
		"channel: stable",
		"platform: android",
		"toolchain: soroq-android-3.44.2-release-12d3315131f5",
		"flutter_version: 3.44.2",
		"api: https://api.soroq.dev",
	}, "\n")

	got := redactSecrets(input)
	if got != input {
		t.Errorf("redaction altered content that holds no secret:\n  in:\n%s\n  out:\n%s", input, got)
	}
}

// THE WHOLE BUNDLE, WITH SECRETS PLANTED IN EVERY PLACE IT READS FROM.
func TestTheBundleCarriesNoPlantedSecret(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()

	// 1. a secret in the project config
	config := strings.Join([]string{
		"app_id: com.example.support",
		"channel: stable",
		"operator_token: " + plantedToken,
		"manifest_trust:",
		"  - key_id: prod-primary",
		"    private_key: " + plantedSeed,
	}, "\n")
	if err := os.WriteFile(filepath.Join(project, "soroq.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// 2. a secret in the lockfile
	lock := "android:\n  toolchain: soroq-android-3.44.2-release-abc\n  ticket: " + plantedToken + "\n"
	if err := os.WriteFile(filepath.Join(project, "soroq.lock"), []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	// 3. secrets in the environment, both in a reported setting and in a name-only one
	t.Setenv("HOME", home)
	t.Setenv("SOROQ_API", "https://operator:"+plantedPassword+"@api.soroq.dev")
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", plantedToken)
	t.Setenv("DATABASE_URL", "postgresql://user:"+plantedPassword+"@host/db")

	out := filepath.Join(t.TempDir(), "bundle.json")
	if err := runSupportBundle([]string{"--project-dir", project, "--out", out}); err != nil {
		t.Fatalf("support-bundle: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	bundle := string(raw)

	for _, secret := range []string{plantedToken, plantedPassword, plantedSeed} {
		if strings.Contains(bundle, secret) {
			t.Errorf("the bundle carries a planted secret (%s...)", secret[:12])
		}
	}

	// AND IT IS STILL USEFUL. A bundle with nothing in it would pass the assertions above.
	for _, wanted := range []string{
		"com.example.support", // the app id, which is what support needs to look anything up
		"cli_version",
		"platform",
		"has_soroq_yaml",
		"SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", // named, so "is it set?" can be answered
	} {
		if !strings.Contains(bundle, wanted) {
			t.Errorf("the bundle does not contain %q, so it cannot be used to diagnose anything", wanted)
		}
	}
	// The presence of a secret setting is reported WITHOUT its value.
	if !strings.Contains(bundle, "set (value not collected)") {
		t.Error("the bundle does not record whether the operator token is set; 'missing' and 'wrong' " +
			"are different problems and the first is the commonest support request")
	}

	// The file is written for the operator only.
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the bundle is world- or group-readable: %v", info.Mode())
	}
}

// A BUNDLE FROM A CLEAN HOME SAYS WHAT IS MISSING rather than producing an empty document.
func TestTheBundleFromACleanHomeSaysWhatIsMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := filepath.Join(t.TempDir(), "bundle.json")

	if err := runSupportBundle([]string{"--project-dir", t.TempDir(), "--out", out}); err != nil {
		t.Fatalf("support-bundle: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	bundle := string(raw)
	if !strings.Contains(bundle, "no soroq.yaml in this directory") {
		t.Errorf("a bundle from a directory with no project does not say so:\n%s", bundle)
	}
	if !strings.Contains(bundle, "soroq setup") {
		t.Errorf("a bundle with no toolchain installed does not name the command that installs one")
	}
}

// EVERY FREE-TEXT COLLECTION SITE REMOVES AN UNLABELLED SECRET.
//
// WHY THIS IS THE CLAIM, AND NOT "THE WHOLE-DOCUMENT PASS CATCHES THESE". An earlier version of this
// test asserted the second thing, and it was wrong in a way that took an independent round to find.
// The final pass over the encoded document is a CREDENTIAL backstop: it cannot apply the entropy rules,
// because the bundle legitimately carries 64-hex public identifiers and a full-strength pass destroys
// them. One rule over one document cannot both keep `runtime_id` and remove a bare hex key.
//
// So the entropy rules live at the collection sites, and this test is one row per site. A site added
// without a redactor fails here, and the failure names the site rather than a pass that never covered
// it. THE MECHANISM THE TEST NAMES IS THE MECHANISM THAT DOES THE WORK -- that is the point.
//
// Each row plants TWO shapes: one that carries a label (caught by credentialPatterns) and one that is
// a bare hex run with nothing to key on (caught only by entropyPatterns). A site wired to
// redactCredentials instead of redactSecrets passes the first and fails the second.
func TestEveryFreeTextCollectionSiteRemovesAnUnlabelledSecret(t *testing.T) {
	sites := []struct {
		site string
		// plant arranges for the secret to reach the named site, and returns the project directory
		// the command should be pointed at.
		plant func(t *testing.T, home string, secret string) string
	}{
		{
			site: "caches.toolchains -- a cache directory name",
			plant: func(t *testing.T, home, secret string) string {
				dir := filepath.Join(home, ".soroq", "toolchains", secret)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return t.TempDir()
			},
		},
		{
			site: "caches.frontends -- a cache directory name",
			plant: func(t *testing.T, home, secret string) string {
				dir := filepath.Join(home, ".soroq", "frontends", secret)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return t.TempDir()
			},
		},
		{
			site: "project.directory -- the project path itself",
			plant: func(t *testing.T, home, secret string) string {
				dir := filepath.Join(t.TempDir(), secret)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
		{
			site: "environment -- a reported setting's value",
			plant: func(t *testing.T, home, secret string) string {
				t.Setenv("SOROQ_API", "https://example.invalid/"+secret)
				return t.TempDir()
			},
		},
	}

	// A LABELLED shape and a BARE shape. The bare one is what separates redactSecrets from
	// redactCredentials; without it every row would pass on the credential rules alone.
	shapes := []struct {
		name string
		// secret is planted at the site; needle is what must not appear in the output.
		//
		// THE NEEDLE IS PER-SHAPE FOR A REASON. The first version of the glued-token row planted
		// `proj_` + plantedToken and asserted on plantedToken, and it passed with the defect restored
		// -- because that token's tail is 32 hex characters and the ENTROPY rule caught it. The row
		// tested a rule it was not about. A shape must be removed by the rule it is for, so each one
		// now carries a value no other rule in the set can see.
		secret string
		needle string
	}{
		{"labelled", "toolchain-token=" + plantedToken, plantedToken},
		{"bare hex, nothing to key on", plantedHex, plantedHex},
		// THE SHAPE THAT WAS INVISIBLE AT EVERY SITE AT ONCE.
		//
		// A vendor token GLUED to a preceding word character. `\b` is a boundary between a word
		// character and a non-word one, so in `proj_ghp_0123...` -- `j` then `g`, both word characters
		// -- the boundary never occurs and the pattern never fires. An audit found this by putting a
		// token in a directory NAME, and it shipped through the compiled command while the site was
		// classified "free text: redactSecrets" and this test passed. Classification says which rule
		// runs; it cannot say the rule works.
		// A GitHub token whose body is pure alphabet, so no entropy rule can reach it: 32/48/64-hex do
		// not match letters, the 40-char base64 rule needs exactly 40 of [A-Za-z0-9/+] and this has an
		// underscore, and the 60+ rule needs 60. Only the vendor prefix identifies it -- which is the
		// point.
		{"vendor token glued to a preceding word", "proj_" + gluedVendorToken, gluedVendorToken},
		// AND THE OTHER SIDE. The fix for the row above stripped the LEADING word boundary from every
		// vendor pattern and left the trailing one, so a token followed by `_anything` still shipped
		// -- the same defect, one side over, and it survived a round because the fix only looked at
		// the side the audit had named. The proof it is the boundary and not the shape: the same token
		// was redacted when followed by `/` and shipped when followed by `_`.
		{"vendor token glued on the RIGHT", gluedVendorToken + "_prod", gluedVendorToken},
	}

	for _, site := range sites {
		for _, shape := range shapes {
			t.Run(site.site+"/"+shape.name, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				project := site.plant(t, home, shape.secret)

				out := filepath.Join(t.TempDir(), "bundle.json")
				if err := runSupportBundle([]string{"--project-dir", project, "--out", out}); err != nil {
					t.Fatalf("support-bundle: %v", err)
				}
				raw, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				bundle := string(raw)

				// IT MUST STILL BE JSON. This one assertion is what five rounds of substring checks
				// never made, and two real defects shipped through the gap: a redaction pass over the
				// ENCODED document ate a closing quote, and the same pass ran through a `\n` escape
				// and destroyed the `app_id` key. Both left a file that "contains" every expected
				// substring, so every test stayed green while `soroq support-bundle` emitted a
				// document its own --help calls "plain JSON on purpose".
				var parsed map[string]any
				if err := json.Unmarshal(raw, &parsed); err != nil {
					t.Fatalf("%s: the bundle is not parseable JSON (%v); the command promises plain "+
						"JSON and a reader cannot use this:\n%s", site.site, err, bundle)
				}
				// AND A KEY SURVIVES THE ROUND TRIP. Valid JSON that lost a field would satisfy the
				// check above; the silent-loss defect produced exactly that.
				if _, ok := parsed["cli_version"]; !ok {
					t.Errorf("%s: the bundle parsed but lost cli_version:\n%s", site.site, bundle)
				}

				// The secret itself must be gone. plantedToken carries a label; plantedHex does not,
				// so a site wired to redactCredentials ships it and this fails naming the site.
				if strings.Contains(bundle, shape.needle) {
					t.Errorf("%s shipped an unredacted secret (%s...):\n"+
						"this collection site is not passing its value through redactSecrets, or the "+
						"rule for this shape does not fire where the value actually sits",
						site.site, shape.needle[:12])
				}
				// AND THE SECTION IS STILL THERE. A redactor that deleted the field would satisfy
				// the assertion above while destroying the diagnostic.
				if !strings.Contains(bundle, REDACTED) {
					t.Errorf("%s removed the value without leaving %s; a silently dropped field is "+
						"indistinguishable from one that was never set:\n%s", site.site, REDACTED, bundle)
				}
			})
		}
	}
}

// THE TRADE-OFF, AS A CONTROL. A cache named by nothing but a digest IS redacted, and its diagnostic
// value is lost. That is a decision, not an accident: a bare hex run cannot be told apart from a key.
//
// This test exists so the decision cannot be reversed silently. If a future change makes digest-named
// caches survive, this fails and whoever changed it has to argue for it in the open.
func TestADigestNamedCacheIsRedactedAndThatCostIsAccepted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	digest := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	labelled := "soroq-android-3.44.2-release-12d3315131f5"
	for _, name := range []string{digest, labelled} {
		if err := os.MkdirAll(filepath.Join(home, ".soroq", "toolchains", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(t.TempDir(), "bundle.json")
	if err := runSupportBundle([]string{"--project-dir", t.TempDir(), "--out", out}); err != nil {
		t.Fatalf("support-bundle: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	bundle := string(raw)

	if strings.Contains(bundle, digest) {
		t.Errorf("a digest-named cache survived; the stated trade-off is that it does not, so either "+
			"the code changed or this control is now the thing that is wrong:\n%s", bundle)
	}
	// THE POSITIVE HALF. Without it, a redactor that ate every cache name would pass the above, and
	// the "cost is bounded" claim in the code comment would be false with nothing to catch it.
	if !strings.Contains(bundle, labelled) {
		t.Errorf("a REAL toolchain name was redacted too; the cost is supposed to fall only on names "+
			"the toolchain does not produce, and %s is one it does:\n%s", labelled, bundle)
	}
}

// RECOGNISABLE THIRD-PARTY CREDENTIALS, EVERY SHAPE AN INDEPENDENT AUDIT FOUND SHIPPING.
//
// These carry their own prefix and need no key word beside them, and every one of them is shorter than
// the generic high-entropy rules can see. They shipped verbatim through a bundle whose other rules
// worked perfectly, which is the failure mode a shape-based redactor has: the list is incomplete rather
// than the mechanism broken. Each is listed here so a future gap is a test that fails, not an audit.
// THE FIXTURE VALUES ARE ASSEMBLED, NOT WRITTEN WHOLE, and that is not cosmetic.
//
// This file is part of the PUBLIC CLI export, and a contiguous vendor-shaped literal in a public
// repository trips GitHub's push protection: the v0.3.0 mirror push was blocked on the Slack line
// before this change. The SHAPES cannot be weakened -- the redactor is the thing under test, and a
// fixture that no longer looks like a credential would prove nothing -- so each one is built from
// fragments instead. The runtime values are byte-identical to what they always were; there is simply
// no literal left for a scanner to match.
//
// The same technique scripts/deploy_soroqd_to_fly.sh already uses to name SOROQ_S3_SECRET_ACCESS_KEY
// without writing it out.
func TestVendorCredentialShapesAreRedacted(t *testing.T) {
	for _, tc := range []struct{ name, secret string }{
		{"a GitHub personal access token", "ghp" + "_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
		{"a GitHub OAuth token", "gho" + "_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
		{"an AWS access key id", "AKIA" + "IOSFODNN7EXAMPLE"},
		{"an AWS secret access key", "wJalrXUtnFEMI" + "KbPxRfiCYEXAMPLEKEYabcdefgh"},
		{"a Slack bot token", "xoxb" + "-123456789012-123456789012-AbCdEfGhIjKlMnOpQrSt"},
		{"a Stripe live secret key", "sk_live" + "_51H8xKzABCDEFGHIJKLMNOPQR"},
		{"an OpenAI project key", "sk-proj" + "-AbCdEfGhIjKlMnOpQrStUvWxYz012345"},
		{"a GitLab personal access token", "glpat" + "-AbCdEfGhIjKlMnOpQrSt"},
		{"a Neon database password", "npg" + "_tEJbg0Tyi5hkABCDEFGH"},
		{"a JWT", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9" + ".eyJzdWIiOiIxMjM0NTY3ODkwIn0" + ".dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// In a bare line with NO key word: the hardest case, and the one the audit used.
			got := redactSecrets("value " + tc.secret)
			if strings.Contains(got, tc.secret) {
				t.Errorf("%s shipped verbatim: %s", tc.name, got)
			}
			// And inside a JSON document, which is the shape the bundle actually writes.
			inJSON := redactSecrets(`{"note": "` + tc.secret + `"}`)
			if strings.Contains(inJSON, tc.secret) {
				t.Errorf("%s shipped verbatim inside JSON: %s", tc.name, inJSON)
			}
		})
	}
}

// AND THE ORDINARY CONTENT STILL SURVIVES. Adding shape rules is how a redactor starts eating the
// diagnostics it exists to preserve, so the positive control is repeated against the new patterns.
func TestTheNewShapeRulesDoNotEatOrdinaryContent(t *testing.T) {
	input := strings.Join([]string{
		"app_id: com.example.myapp",
		"channel: stable",
		"flutter_version: 3.44.2",
		"toolchain: soroq-android-3.44.2-release-12d3315131f5",
		"platform: darwin/arm64",
		"cli_version: v0.2.9",
		"state_dir: /Users/someone/.soroq",
		"generated_at: 2026-08-31T09:00:00Z",
	}, "\n")
	if got := redactSecrets(input); got != input {
		t.Errorf("the new rules altered content that holds no secret:\n  in:\n%s\n  out:\n%s", input, got)
	}
}

// AN ALLOWLIST SURVIVES A FORMAT NOBODY HAS HEARD OF. This is the test the shape list cannot pass.
//
// Two independent rounds of auditing found secret formats the pattern list did not cover -- seven, then
// eight more after the first fix. That is the failure mode of a denylist: it ships what it has not
// heard of, and every round of patching leaves the next format uncovered. For a CONFIG FILE the
// question is invertible, because the keys are ours, so an unknown key is DROPPED.
//
// Every value below is a shape redactSecrets does NOT match. None may appear in the bundle.
func TestUnknownConfigKeysAreOmittedWhateverTheirShape(t *testing.T) {
	// VALUES THE SHAPE RULES CANNOT SEE. Not vendor formats -- those are all matched now, and planting
	// them would make this test pass on the denylist's coverage rather than on the allowlist.
	//
	// These are what a secret looks like when it has no recognisable form at all: a short opaque
	// string, a passphrase, a made-up scheme. No pattern can be written for the class, which is the
	// whole reason the config path is an allowlist.
	unlisted := map[string]string{
		"internal_shared_secret": "hunter2",
		"vendor_x_credential":    "correct-horse-battery-staple",
		"support_pin":            "8391",
		"something_invented":     "whatever-a-future-tool-decides-to-put-here-2026",
		"legacy_field":           "Zm9vYmFyYmF6",
		"partner_handshake":      "acme:prod:7f3a",
		"future_format":          "v3$scrypt$16384$8$1$d2hhdGV2ZXI",
	}

	var config strings.Builder
	config.WriteString("app_id: com.example.allowlist\nchannel: stable\n")
	for key, value := range unlisted {
		config.WriteString(key + ": " + value + "\n")
	}

	// FIRST: confirm the shape rules genuinely do NOT catch these, or this test proves nothing about
	// the allowlist -- it would be passing on the denylist's coverage.
	shapeOnly := redactSecrets(config.String())
	uncaught := 0
	for _, value := range unlisted {
		if strings.Contains(shapeOnly, value) {
			uncaught++
		}
	}
	// A MAJORITY, NOT ONE. This guard started as "at least one" and decayed to exactly one as vendor
	// patterns were added -- at which point the test was demonstrating its own premise with a single
	// value and passing on the denylist for the rest. The bar is now most of them.
	if uncaught*2 <= len(unlisted) {
		t.Fatalf("only %d of %d planted values bypass the shape rules; this test is drifting toward "+
			"proving the denylist instead of the allowlist. Plant values with no recognisable form.",
			uncaught, len(unlisted))
	}
	t.Logf("%d of %d planted formats are NOT matched by any shape rule", uncaught, len(unlisted))

	// NOW the allowlist, which does not need to recognise any of them.
	got := redactConfig(config.String())
	for key, value := range unlisted {
		if strings.Contains(got, value) {
			t.Errorf("the value of unlisted key %q survived: %s", key, got)
		}
		if !strings.Contains(got, key+": "+OMITTED) {
			t.Errorf("key %q was dropped silently; the key must remain so a reader can see something "+
				"was there:\n%s", key, got)
		}
	}
	// AND THE BUNDLE IS STILL USEFUL.
	if !strings.Contains(got, "app_id: com.example.allowlist") {
		t.Errorf("the allowlist dropped the app id, which is what support needs to look anything up:\n%s", got)
	}
	if !strings.Contains(got, "channel: stable") {
		t.Errorf("the allowlist dropped the channel:\n%s", got)
	}
	if !strings.Contains(got, "omitted: not on the support-bundle allowlist") {
		t.Errorf("the bundle does not say that anything was omitted:\n%s", got)
	}
}

// A REAL soroq.yaml SURVIVES USEFULLY. The allowlist is only worth having if what it keeps is enough to
// diagnose with; an allowlist that dropped everything would pass the test above.
func TestARealProjectConfigKeepsWhatSupportNeeds(t *testing.T) {
	real := strings.Join([]string{
		"app_id: com.example.keys",
		"channel: stable",
		"runtime_id_strategy: manifest_trust_v1",
		"ios_engine:",
		"  enabled: true",
		"manifest_trust:",
		"  keyset_version: 1",
		"  keys:",
		"    - id: com.example.keys-project-signing",
		"      public_key: 9SGAgLdg4-hAN6sVXZxzAfZV4J5pmGyE10mROetLKqU",
	}, "\n")

	got := redactConfig(real)
	for _, needed := range []string{
		"app_id: com.example.keys",
		"channel: stable",
		"runtime_id_strategy: manifest_trust_v1",
		"keyset_version: 1",
		"id: com.example.keys-project-signing",
		// A PUBLIC key is public, and a trust mismatch is one of the commonest reasons a device
		// refuses a patch. Dropping it would remove the answer to the question being asked.
		"public_key: 9SGAgLdg4-hAN6sVXZxzAfZV4J5pmGyE10mROetLKqU",
	} {
		if !strings.Contains(got, needed) {
			t.Errorf("the allowlist dropped %q, which support needs:\n%s", needed, got)
		}
	}
}

// AND A SECRET UNDER AN ALLOWLISTED KEY IS STILL CAUGHT. The two rules compose: the allowlist decides
// which keys survive, the shape rules still clean what those keys carry.
func TestAnAllowlistedKeyCarryingACredentialIsStillRedacted(t *testing.T) {
	got := redactConfig("app_id: com.example.app\ntoolchain: https://user:" + plantedPassword + "@host/tc\n")
	if strings.Contains(got, plantedPassword) {
		t.Errorf("a credential under an allowlisted key survived: %s", got)
	}
	if !strings.Contains(got, "app_id: com.example.app") {
		t.Errorf("the app id was lost: %s", got)
	}
}

// A PUBLIC IDENTIFIER SURVIVES IN A CONFIG AND IS REDACTED IN FREE TEXT.
//
// A runtime id is 64 hex characters and a signing key id is a hash, so the bare-entropy rules cannot
// tell either from a secret. Broadening the denylist to catch more vendor formats replaced `runtime_id`
// -- the single most useful value in a support bundle -- with [REDACTED]. The rules are therefore split:
// where the meaning of a field is KNOWN, only the credential shapes apply; where nothing is known about
// a value, everything applies.
func TestPublicIdentifiersSurviveWhereTheirMeaningIsKnown(t *testing.T) {
	config := strings.Join([]string{
		"app_id: com.example.identifiers",
		"runtime_id: 8a257787bd96d40b2360d97fdb813525b7e59ed2a174c1328de8206356b0eafe",
		"key_id: soroq-kid-c081686e4e53a71b",
		"release_id: my-release-1",
	}, "\n")

	kept := redactConfig(config)
	for _, identifier := range []string{
		"8a257787bd96d40b2360d97fdb813525b7e59ed2a174c1328de8206356b0eafe",
		"soroq-kid-c081686e4e53a71b",
		"my-release-1",
	} {
		if !strings.Contains(kept, identifier) {
			t.Errorf("a public identifier was destroyed; a bundle without it cannot be used to look "+
				"anything up:\n  wanted %s\n  got:\n%s", identifier, kept)
		}
	}

	// AND THROUGH THE REAL COMMAND, which is the assertion that was missing.
	//
	// This test passed on redactConfig in isolation while `soroq support-bundle` destroyed runtime_id,
	// because the command ran ONE MORE pass -- redactSecrets over the whole encoded document -- that
	// re-applied the entropy rules the split exists to withhold. An independent audit found it. A
	// tested helper is not a tested command, and this file had already said so about App.tsx.
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(project, "soroq.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "bundle.json")
	if err := runSupportBundle([]string{"--project-dir", project, "--out", out}); err != nil {
		t.Fatalf("support-bundle: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	bundle := string(raw)
	for _, identifier := range []string{
		"8a257787bd96d40b2360d97fdb813525b7e59ed2a174c1328de8206356b0eafe",
		"soroq-kid-c081686e4e53a71b",
		"my-release-1",
	} {
		if !strings.Contains(bundle, identifier) {
			t.Errorf("the REAL command destroyed the public identifier %s; the helper keeps it and the "+
				"command does not, which is the gap a helper-only test cannot see:\n%s", identifier, bundle)
		}
	}

	// THE OTHER HALF. In free text nothing is known about what a 64-hex run means, so it goes.
	free := redactSecrets("some log line 8a257787bd96d40b2360d97fdb813525b7e59ed2a174c1328de8206356b0eafe")
	if strings.Contains(free, "8a257787bd96d40b2360d97fdb813525b7e59ed2a174c1328de8206356b0eafe") {
		t.Error("a bare high-entropy run survived in free text, where nothing identifies it")
	}

	// AND A CREDENTIAL UNDER AN IDENTIFIER KEY IS STILL CAUGHT.
	leaky := redactConfig("runtime_id: https://user:" + plantedPassword + "@host/x\n")
	if strings.Contains(leaky, plantedPassword) {
		t.Errorf("a credential under an identifier key survived: %s", leaky)
	}
}

// A LOCKFILE SURVIVES THE REAL COMMAND WITH ITS SHAPE AND ITS PINS INTACT.
//
// An audit found three separate mutilations in one bundle: runtime_id redacted by a pass that should
// not have run, `platforms:` and `android:` marked [OMITTED] though a structural header carries no
// value at all, and toolchain_version omitted because it was not on the allowlist. Each destroyed the
// answer to a question support asks first.
func TestTheRealCommandKeepsTheLockfileReadable(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	lock := strings.Join([]string{
		"# soroq.lock",
		"platforms:",
		"  android:",
		"    release_id: rel-a",
		"    version: 1.0.0+1",
		"    toolchain_version: soroq-android-3.44.2-release-abc",
		"    recorded_at: 2026-08-31T00:00:00Z",
	}, "\n")
	if err := os.WriteFile(filepath.Join(project, "soroq.lock"), []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "soroq.yaml"),
		[]byte("app_id: com.example.lock\nchannel: stable\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "bundle.json")
	if err := runSupportBundle([]string{"--project-dir", project, "--out", out}); err != nil {
		t.Fatalf("support-bundle: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	bundle := string(raw)

	for _, needed := range []string{
		"platforms:",        // the structure
		"android:",          // the platform, also a structural header
		"release_id: rel-a", // which base the patch was built against
		"toolchain_version: soroq-android-3.44.2", // the commonest cause of a broken patch
		"recorded_at: 2026-08-31",
	} {
		if !strings.Contains(bundle, needed) {
			t.Errorf("the lockfile lost %q; a bundle that cannot show which toolchain built the base "+
				"cannot answer the question it is collected for:\n%s", needed, bundle)
		}
	}
	// And nothing was marked omitted, because every line in that lockfile is either a structural
	// header or an allowlisted key.
	if strings.Contains(bundle, "soroq_lock") && strings.Contains(bundle, OMITTED) {
		t.Errorf("the lockfile reported omissions it should not have:\n%s", bundle)
	}
}

// EVERY STRING THAT REACHES THE BUNDLE HAS A NAMED COLLECTION SITE THAT REDACTS IT.
//
// WHY THIS EXISTS. The bundle used to end with a redaction pass over the whole encoded document, sold
// as belt and braces for "a field this code does not know about". It was not braces; it was a second
// language. Its patterns are written for raw text, and over JSON they ate a closing quote and ran
// through a newline escape -- two shipped defects, one leak fixed by narrowing it and re-opened by the
// narrowing. It is gone.
//
// What replaces it is this: a field with no named site is a TEST failure, not a value quietly passed
// through a weaker rule. The check is deliberately manual -- reflection over the struct could confirm
// a field appears in a list while the code behind it ships raw, which is a checker that cannot fail.
// Adding a field to supportBundle means adding a row here and saying which rule redacts it.
func TestEveryStringInTheBundleIsClassified(t *testing.T) {
	classified := map[string]string{
		"generated_at":         "code-authored: time.Now().Format, no external input",
		"cli_version":          "code-authored: buildVersion, a build-time constant",
		"platform":             "code-authored: runtime.GOOS + runtime.GOARCH",
		"environment":          "free text: redactSecrets at the collection site",
		"environment_presence": "code-authored: the literals \"set\" / \"not set\"; the value is never read",
		"project":              "see the project rows below",
		"caches":               "see the caches rows below",
		"notes":                "code-authored: fixed advice strings, no interpolation",
	}
	projectClassified := map[string]string{
		"directory":      "free text: redactSecrets -- a path carries whatever it was named",
		"has_soroq_yaml": "bool",
		"has_soroq_lock": "bool",
		"soroq_yaml":     "known keys: redactConfig's allowlist; comments get redactSecrets",
		"soroq_lock":     "known keys: redactConfig's allowlist; comments get redactSecrets",
	}
	cachesClassified := map[string]string{
		"state_dir":  "free text: redactSecrets",
		"frontends":  "free text: redactSecrets per name",
		"toolchains": "free text: redactSecrets per name",
	}

	// Generate a real bundle and compare its actual keys against the classification.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".soroq", "toolchains", "tc"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "soroq.yaml"),
		[]byte("app_id: demo\nchannel: stable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "soroq.lock"),
		[]byte("platforms:\n  android:\n    release_id: r1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "bundle.json")
	if err := runSupportBundle([]string{"--project-dir", project, "--out", out}); err != nil {
		t.Fatalf("support-bundle: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the bundle is not parseable JSON: %v\n%s", err, raw)
	}

	check := func(section string, got map[string]any, want map[string]string) {
		for key := range got {
			if _, ok := want[key]; !ok {
				t.Errorf("%s.%s reaches the bundle with NO classification. Say which rule redacts it "+
					"-- free text takes redactSecrets, a value under a known key takes redactConfig, a "+
					"code-authored constant takes neither -- and add a row here. There is no "+
					"whole-document pass to fall back on, and there must not be one.", section, key)
			}
		}
		for key := range want {
			if _, ok := got[key]; !ok && section == "bundle" {
				t.Errorf("%s.%s is classified but never appears in a generated bundle; the "+
					"classification has drifted from the struct", section, key)
			}
		}
	}
	check("bundle", parsed, classified)
	if project, ok := parsed["project"].(map[string]any); ok {
		check("project", project, projectClassified)
	} else {
		t.Error("the bundle has no project section")
	}
	if caches, ok := parsed["caches"].(map[string]any); ok {
		check("caches", caches, cachesClassified)
	} else {
		t.Error("the bundle has no caches section")
	}
}

// A SECRET IN A YAML COMMENT IS STILL A SECRET.
//
// The comment line was the one shape redactConfig passed through verbatim, on the reasoning that a
// comment carries no value. It carries whatever a person typed. An audit put a bare 64-hex key in one
// and watched it ship through the compiled command.
func TestASecretInAConfigCommentIsRemoved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()

	config := "# ops note: raw key " + plantedHex + "\n" +
		"# Authorization: Bearer " + plantedToken + "\n" +
		"app_id: com.example.comment\n" +
		"channel: stable\n" +
		"runtime_id: " + plantedHex + "\n"
	if err := os.WriteFile(filepath.Join(project, "soroq.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "bundle.json")
	if err := runSupportBundle([]string{"--project-dir", project, "--out", out}); err != nil {
		t.Fatalf("support-bundle: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the bundle is not parseable JSON: %v\n%s", err, raw)
	}
	yaml, _ := parsed["project"].(map[string]any)["soroq_yaml"].(string)

	// The comment must be scrubbed...
	commentLine := strings.SplitN(yaml, "\n", 2)[0]
	if strings.Contains(commentLine, plantedHex) {
		t.Errorf("a bare high-entropy key in a COMMENT shipped verbatim:\n%s", yaml)
	}
	if strings.Contains(yaml, plantedToken) {
		t.Errorf("a labelled token in a COMMENT shipped verbatim:\n%s", yaml)
	}
	// ...AND THE KEPT KEYS MUST SURVIVE. Without this, a redactor that scrubbed the whole document
	// would pass the assertions above -- and that redactor is exactly what was just removed, because
	// it destroyed runtime_id and, over encoded JSON, the app_id key itself.
	if !strings.Contains(yaml, "app_id: com.example.comment") {
		t.Errorf("app_id did not survive; scrubbing the comment must not damage the rest:\n%s", yaml)
	}
	if !strings.Contains(yaml, "runtime_id: "+plantedHex) {
		t.Errorf("runtime_id lost its value; the SAME 64-hex string is a secret in a comment and a "+
			"public identifier under runtime_id, and knowing which is the entire point:\n%s", yaml)
	}
}

// A REDACTION PASS OVER THE ENCODED DOCUMENT CORRUPTS IT. THIS IS THE TEST THAT SAYS SO.
//
// WHY IT NEEDS ITS OWN CASE. Restoring the old whole-document pass does not fail the other tests: none
// of their fixtures contain a value whose redaction reaches a JSON delimiter. Both shipped defects
// needed a specific shape, and without those shapes the mutation looks harmless.
//
//	a query-string signature -> the value class `[^&\s]+` does not stop at `"`, so the closing quote
//	                            is eaten and the document is no longer JSON
//	a bearer token before a  -> in encoded text a newline is the two characters `\` and `n`, so `\S+`
//	newline in the config       runs through it and destroys the NEXT key
//
// If someone adds a pass over `encoded` again, this fails and the message says why. It is the guard on
// a design decision, not on a value.
func TestNoPassRunsOverTheEncodedDocument(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A signature in a setting's value: its redaction ends at a JSON delimiter.
	t.Setenv("SOROQ_INSTALL_DIR", "https://cdn.example/artifact?sig=abcdefghijklmnopqrst")

	project := t.TempDir()
	// A bearer token on the line BEFORE a key that must survive.
	config := "# Authorization: Bearer " + plantedToken + "\n" +
		"app_id: com.example.encoded\n" +
		"channel: stable\n"
	if err := os.WriteFile(filepath.Join(project, "soroq.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "bundle.json")
	if err := runSupportBundle([]string{"--project-dir", project, "--out", out}); err != nil {
		t.Fatalf("support-bundle: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the bundle is not parseable JSON (%v).\n"+
			"A redaction pass is running over the ENCODED document and has eaten a delimiter. "+
			"Redact each value BEFORE encoding; a pattern cannot eat a delimiter it never sees:\n%s",
			err, raw)
	}

	project_, _ := parsed["project"].(map[string]any)
	yaml, _ := project_["soroq_yaml"].(string)
	if !strings.Contains(yaml, "app_id: com.example.encoded") {
		t.Errorf("app_id was destroyed. A pattern matched across the `\\n` ESCAPE in the encoded "+
			"document -- `\\S+` sees two ordinary characters there, not whitespace -- and swallowed the "+
			"next key. Silent loss is worse than a visible one: a reader cannot tell it happened:\n%s",
			yaml)
	}
	if !strings.Contains(yaml, "channel: stable") {
		t.Errorf("channel was destroyed by the same over-long match:\n%s", yaml)
	}
	// THE POSITIVE HALF. Both secrets must still be gone, or a command that redacted nothing at all
	// would satisfy every assertion above.
	if strings.Contains(string(raw), plantedToken) {
		t.Errorf("the bearer token shipped:\n%s", raw)
	}
	if strings.Contains(string(raw), "sig=abcdefghijklmnopqrst") {
		t.Errorf("the query-string signature shipped:\n%s", raw)
	}
}

// A MULTI-LINE SECRET IN A CONFIG COMMENT.
//
// The PEM rule is the only pattern in the set that spans lines, and redactConfig iterates line by
// line, so it could never fire there. An audit put a private key in a soroq.yaml comment with
// 44-character body lines -- short enough that no entropy rule matched either -- and every line
// shipped verbatim. Normal 64-character PEM wrapping only died by accident, on the 60+ base64 rule.
//
// The 44-character body is the whole point of this fixture: at 64 the test would pass for the wrong
// reason, and it would keep passing with the multi-line pass deleted.
func TestAMultiLineSecretInAConfigCommentIsRemoved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()

	body := strings.Repeat("A", 44)
	config := "app_id: com.example.pem\n" +
		"runtime_id: " + plantedHex + "\n" +
		"# -----BEGIN RSA PRIVATE KEY-----\n" +
		"# " + body + "\n# " + body + "\n# " + body + "\n" +
		"# -----END RSA PRIVATE KEY-----\n"
	if err := os.WriteFile(filepath.Join(project, "soroq.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "bundle.json")
	if err := runSupportBundle([]string{"--project-dir", project, "--out", out}); err != nil {
		t.Fatalf("support-bundle: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the bundle is not parseable JSON: %v\n%s", err, raw)
	}
	yaml, _ := parsed["project"].(map[string]any)["soroq_yaml"].(string)

	if strings.Contains(yaml, body) {
		t.Errorf("a PEM private key shipped out of a config comment. A line-oriented redactor cannot "+
			"see a pattern that spans lines, so the multi-line rules must run over the whole text "+
			"first:\n%s", yaml)
	}
	// THE POSITIVE HALF. A pass that scrubbed the whole document would satisfy the check above, and
	// that is precisely the pass this file removed for destroying runtime_id.
	if !strings.Contains(yaml, "runtime_id: "+plantedHex) {
		t.Errorf("runtime_id did not survive; removing a multi-line secret must not damage the rest "+
			"of the file:\n%s", yaml)
	}
	if !strings.Contains(yaml, "app_id: com.example.pem") {
		t.Errorf("app_id did not survive:\n%s", yaml)
	}
}

// THE VENDOR FORMATS AN AUDIT NAMED, each with its own row.
//
// They shipped with no control at all: deleting all of them together left `go test ./cmd/soroq` green.
// A pattern nothing exercises is one a careless edit can drop unnoticed.
//
// WHAT THIS TEST CLAIMS, PRECISELY. It asserts each FORMAT is removed -- not that a particular pattern
// exists. The distinction is not pedantry: a later audit deleted only the `sk-ant-` rule and this test
// still passed, because the general `sk-` rule already covers that shape. The row was not proving what
// its name suggested. The redundant rule is gone and the claim is now the one the test can support.
//
// A row here that cannot fail on its own is worth finding: it means either the format is covered
// elsewhere (delete the duplicate rule, as happened) or the row is testing nothing.
func TestTheNamedVendorFormatsAreRemoved(t *testing.T) {
	// Assembled from fragments for the same reason as TestVendorCredentialShapesAreRedacted above:
	// GitHub's push protection rejected the v0.3.0 mirror on the DigitalOcean and Shopify rows here.
	// Runtime values are byte-identical, so the formats under test are exactly what they were.
	for _, secret := range []string{
		"dop" + "_v1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"sk-ant" + "-api03-QRSTUVWXYZabcdefghijklmnop",
		"shpat" + "_0123456789abcdef0123456789abcdef",
		"dckr" + "_pat_QRSTUVWXYZabcdefghijklmnop",
		"figd" + "_QRSTUVWXYZabcdefghijklmnop",
	} {
		t.Run(strings.SplitN(secret, "_", 2)[0], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			project := t.TempDir()
			// In a COMMENT, which is free text and takes the full rule set, and glued on BOTH sides
			// so a boundary regression fails here too.
			config := "app_id: com.example.vendor\n# note x" + secret + "_prod\n"
			if err := os.WriteFile(filepath.Join(project, "soroq.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "bundle.json")
			if err := runSupportBundle([]string{"--project-dir", project, "--out", out}); err != nil {
				t.Fatalf("support-bundle: %v", err)
			}
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), secret) {
				t.Errorf("%s shipped verbatim", secret)
			}
			if !strings.Contains(string(raw), "app_id") {
				t.Errorf("the bundle lost app_id; the rule removed more than the secret")
			}
		})
	}
}
