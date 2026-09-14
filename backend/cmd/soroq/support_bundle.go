package main

// support_bundle.go -- `soroq support-bundle`: everything an operator needs to hand over, and nothing
// they would regret handing over.
//
// WHY THIS EXISTS. When something breaks, the fastest path to help is "send me your setup". Without a
// command that does it, people paste their whole config, their environment, or a terminal scrollback
// into a chat window -- and an operator token, a database URL with a password in it, or a signed
// artifact ticket goes with it. A support bundle is not a convenience; it is the safe alternative to
// what people do anyway.
//
// THE RULE: REDACT ON THE WAY IN, NOT ON THE WAY OUT. Every value is passed through redactSecrets
// before it is written, and the redaction is keyed on the SHAPE of a secret rather than on a list of
// field names. A field-name allowlist fails the first time a new field appears, and it fails silently.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// REDACTED is what replaces a secret. It is deliberately loud: a bundle that quietly dropped a value
// would be indistinguishable from one where the value was never set.
const REDACTED = "[REDACTED]"

// OMITTED marks a config value dropped because its key is not on the allowlist. It is a different word
// from REDACTED on purpose: REDACTED means "this looked like a secret", OMITTED means "nobody said this
// was safe to include", and an operator reading the bundle should be able to tell those apart.
const OMITTED = "[OMITTED]"

// secretPatterns match the SHAPES secrets take, not the names people give them.
//
// Ordered longest-context-first so a URL credential is caught as a whole before its parts are.
// credentialPatterns match values that ANNOUNCE themselves: a vendor prefix, a key word beside them, a
// URL that carries a password. They are safe to apply everywhere, because nothing that looks like this
// is a diagnostic.
var credentialPatterns = []*regexp.Regexp{
	// A URL carrying credentials: postgres://user:password@host/db
	regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s:/@]+:[^\s@]+@`),
	// An Authorization header, to the end of the line. It has to swallow the WHOLE value: matching
	// only the next non-space run turned "Authorization: Bearer <token>" into
	// "Authorization[REDACTED] <token>" -- redacting the scheme word and shipping the secret.
	regexp.MustCompile(`(?im)^(\s*authorization)\s*[:=].*$`),
	// A bearer credential anywhere else, including inline in prose or a curl command.
	regexp.MustCompile(`(?i)\b(bearer)\s+\S+`),
	// A signed artifact ticket in a query string.
	regexp.MustCompile(`(?i)([?&](?:ticket|signature|sig|x-amz-signature)=)[^&\s]+`),
	// key = value where the key names something secret. Covers snake, kebab and camel spellings.
	//
	// The value class excludes `[` so this cannot re-match a value an EARLIER pattern already replaced.
	// Without that, the query-string rule turned "?ticket=X" into "?ticket=[REDACTED]" and this rule
	// then rewrote it to "ticket[REDACTED]", destroying the context that says what was removed.
	regexp.MustCompile(`(?i)\b([a-z0-9_-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|seed|ticket|signature)[a-z0-9_-]*)\s*[:=]\s*"?[^"\s,}\[]+`),
	// A PEM block header is enough to know the whole value must go.
	regexp.MustCompile(`(?s)-----BEGIN [^-]+-----.*?-----END [^-]+-----`),
	// RECOGNISABLE THIRD-PARTY CREDENTIALS. These carry their own prefix, so they need no key word
	// beside them and they are far shorter than the generic high-entropy rules below can see. An
	// independent audit found all seven of these shipping verbatim through a bundle whose other rules
	// worked perfectly -- the pattern list was incomplete, which is the failure mode a shape-based
	// redactor has instead of an allowlist's.
	//
	// NO WORD BOUNDARY ON EITHER SIDE OF THESE, AND THAT IS DELIBERATE. A later audit put a token in a directory NAME --
	// `proj_ghp_0123456789abcdefghij` -- and it shipped verbatim. `\b` is a boundary between a word
	// character and a non-word character, and `j` before `g` is two word characters, so the boundary
	// never occurs and the pattern never matches. Any token glued to a preceding word character was
	// invisible: `proj_ghp_...`, `myAKIA...`, `v2sk-live-...`.
	//
	// THE TRAILING BOUNDARY HAD THE SAME DEFECT, ONE SIDE OVER, and it survived a round because the
	// fix only looked at the side the audit had named. `_` is a word character but appears in none of
	// the token character classes, so `ghp_7777777777abcdefghij_prod` never reaches a boundary after
	// the token and never matched. The proof it was the boundary and not the shape: the SAME token was
	// redacted when followed by `/` and shipped when followed by `_`.
	//
	// Removing the trailing boundary costs nothing, because the character class already stops at the
	// end of the credential -- the match ends there and `_prod` is left in place.
	//
	// Dropping the boundaries is safe HERE and only here. Each prefix below identifies itself --
	// `ghp_`, `AKIA`, `xox`, `glpat-` are not sequences ordinary text produces -- so a match inside a
	// longer string is still a credential, and over-matching costs a diagnostic rather than leaking one.
	// The entropy rules below KEEP their boundaries: `[0-9a-fA-F]{32}` unanchored would match inside a
	// 64-hex `runtime_id` and destroy the single most useful value in the bundle. The distinction is the
	// same one this whole file rests on -- a self-identifying shape can be matched anywhere, a bare
	// high-entropy run cannot.
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),                                // GitHub tokens
	regexp.MustCompile(`(?:AKIA|ASIA|AROA|AIDA|ANPA|ANVA|AIPA)[A-Z0-9]{12,}`),       // AWS access key ids
	regexp.MustCompile(`sk-(?:proj-|live-|test-)?[A-Za-z0-9_-]{16,}`),               // OpenAI / Stripe secret keys
	regexp.MustCompile(`[psrw]k_(?:live|test)_[A-Za-z0-9]{16,}`),                    // Stripe publishable/secret
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),                              // Slack tokens
	regexp.MustCompile(`ey[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), // JWTs
	regexp.MustCompile(`glpat-[A-Za-z0-9_-]{16,}`),                                  // GitLab
	regexp.MustCompile(`npg_[A-Za-z0-9]{16,}`),                                      // Neon
	regexp.MustCompile(`AIza[A-Za-z0-9_\-]{20,}`),                                   // Google API keys
	regexp.MustCompile(`AC[0-9a-fA-F]{30,}`),                                        // Twilio account SIDs
	regexp.MustCompile(`SG\.[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}`),              // SendGrid
	regexp.MustCompile(`pypi-[A-Za-z0-9_\-]{16,}`),                                  // PyPI
	regexp.MustCompile(`(?i)AccountKey\s*=\s*[^;\s]+`),                              // Azure connection strings
	regexp.MustCompile(`1//0[A-Za-z0-9_\-]{20,}`),
	// ADDED AFTER AN AUDIT LISTED THEM, and added without claiming the list is now complete. Each
	// carries a self-identifying prefix, so the leading boundary is omitted here as it is above.
	regexp.MustCompile(`dop_v1_[a-f0-9]{32,}`), // DigitalOcean
	// (no sk-ant- rule: the `sk-` rule above already matches it, and a second copy would be
	// a pattern no test could distinguish from its absence.)
	regexp.MustCompile(`shpat_[a-f0-9]{16,}`),             // Shopify
	regexp.MustCompile(`(?i)dckr_pat_[A-Za-z0-9_-]{16,}`), // Docker Hub
	regexp.MustCompile(`figd_[A-Za-z0-9_-]{16,}`),         // Figma                                   // Google OAuth refresh tokens
	// A 32- or 48-hex run: an API secret or a truncated digest. Shorter than the 64-hex rule, and long
	// enough that ordinary prose does not produce one by accident.
	// An AWS SECRET access key has no prefix at all: it is 40 characters of base64 alphabet. Matched
	// only when it stands alone as a word, because a shorter bound here would eat ordinary text.
	// Long base64/hex runs: the shape a raw key or seed takes when it has no label at all.
}

// multiLinePatterns are the patterns that SPAN lines. They are named separately because any
// line-oriented pass is blind to them, and redactConfig is line-oriented by necessity.
var multiLinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [^-]+-----.*?-----END [^-]+-----`),
}

// entropyPatterns match a bare run of base64 or hex with NOTHING to identify it. They are the last
// resort for a secret that carries no label at all -- and they are dangerous, because Soroq's own
// public identifiers have exactly this shape: a runtime id is 64 hex characters and a signing key id
// is a hash. Applied to free text only; never to a value under a key known to hold an identifier.
var entropyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b[0-9a-fA-F]{32}\b`),
	regexp.MustCompile(`\b[0-9a-fA-F]{48}\b`),
	regexp.MustCompile(`\b[A-Za-z0-9/+]{40}\b`),
	regexp.MustCompile(`\b[A-Za-z0-9+/]{60,}={0,2}\b`),
	regexp.MustCompile(`\b[0-9a-fA-F]{64,}\b`),
}

// secretPatterns is both sets, for free text where nothing is known about what a value means.
var secretPatterns = append(append([]*regexp.Regexp{}, credentialPatterns...), entropyPatterns...)

// environmentKeysToReport are the settings that change behaviour and are safe to name. Their VALUES
// still go through redaction: SOROQ_API is a URL, and a URL can carry a credential.
var environmentKeysToReport = []string{
	"SOROQ_API",
	"SOROQ_INSTALL_DIR",
	"SOROQ_FLUTTER_BIN",
	"SOROQ_PROJECT_DIR",
	"SOROQ_AUTO_ROLLBACK_POLICY",
	"NO_COLOR",
}

// environmentKeysToNameOnly are known to hold secrets. The bundle records WHETHER each is set, because
// "the token is missing" and "the token is wrong" are different problems and the first one is the most
// common cause of a support request. The value is never read into the bundle.
var environmentKeysToNameOnly = []string{
	"SOROQ_CONTROL_PLANE_OPERATOR_TOKEN",
	"SOROQ_OPERATOR_TOKEN",
	"SOROQ_OPERATOR_EMAIL",
	"SOROQ_GITHUB_TOKEN",
	"GITHUB_TOKEN",
	"DATABASE_URL",
}

// configKeysWorthKeeping are the soroq.yaml / soroq.lock keys a maintainer needs in order to help.
//
// AN ALLOWLIST, NOT A DENYLIST, AND THAT IS THE WHOLE POINT. redactSecrets matches the SHAPES secrets
// take, and a shape list is never finished: an independent audit found seven vendor formats shipping
// verbatim, and a second pass found eight more the first fix had not listed. Each round adds patterns
// and leaves the next format uncovered, because the failure mode of a denylist is to ship what it has
// not heard of.
//
// For a CONFIG FILE the question can be inverted, because the keys are ours. An unknown key is DROPPED
// rather than emitted, so a field added tomorrow -- by us, by a plugin, by a user pasting something in
// -- is omitted by default. What is lost is a key nobody listed; what is gained is that no future
// secret format can ship through this path at all.
//
// Free-text fields the bundle also collects (cache directory names, environment values) cannot be
// allowlisted this way and still go through redactSecrets.
var configKeysWorthKeeping = map[string]bool{
	"app_id":              true,
	"channel":             true,
	"runtime_id_strategy": true,
	"ios_engine":          true,
	"enabled":             true,
	"manifest_trust":      true,
	"keyset_version":      true,
	"keys":                true,
	// A key ID names which public key a device verifies against. It is an identifier, not a secret, and
	// devices already receive it in every manifest.
	"id": true,
	// PUBLIC keys are public. Keeping them is what lets a maintainer check a trust mismatch, which is
	// one of the commonest reasons a patch is refused on a device.
	"public_key": true,
	"platform":   true,
	"toolchain":  true,
	"frontend":   true,
	"version":    true,
	"release_id": true,
	// soroq.lock records these, and each is what a maintainer asks for first when a patch turns out to
	// have been built against the wrong toolchain.
	"toolchain_version": true,
	"frontend_version":  true,
	"recorded_at":       true,
	"platforms":         true,
	"runtime_id":        true,
	"patch_id":          true,
	"key_id":            true,
	"arch":              true,
	"track":             true,
}

// configKeyLine matches `  key: value` and `  - key: value` in the YAML these files use.
var configKeyLine = regexp.MustCompile(`^(\s*(?:-\s*)?)([A-Za-z_][A-Za-z0-9_-]*)\s*:\s*(.*)$`)

// redactConfig keeps the keys a maintainer needs and OMITS every other value.
//
// The key name is kept even when its value is dropped, so a reader can see that something was there.
// A line that is not a key/value pair at all is dropped rather than guessed at.
func redactConfig(text string) string {
	// A MULTI-LINE SECRET DIES BEFORE THE LINE LOOP, BECAUSE A LINE LOOP CANNOT SEE IT.
	//
	// The PEM rule is the only pattern in the set that spans lines, and this function iterates line by
	// line, so it could never fire here. An audit put a private key in a soroq.yaml COMMENT with
	// 44-character body lines -- short enough that no entropy rule matched either -- and all five lines
	// shipped verbatim. The usual 64-character wrapping only died by accident, on the 60+ base64 rule.
	//
	// This is NOT the whole-document pass that was removed. That one ran raw-text patterns over
	// ENCODED JSON, where `"` and `\n` are different characters than the patterns assume. This runs
	// over the raw YAML this function was handed, before any encoding exists, so none of that hazard
	// applies. The distinction is the text's language, not the size of the window.
	for _, pattern := range multiLinePatterns {
		text = pattern.ReplaceAllString(text, REDACTED)
	}

	var out []string
	omitted := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			out = append(out, line)
			continue
		}
		// A COMMENT IS FREE TEXT, and it was the one line shape that passed through verbatim.
		//
		// An audit put `# note from ops: raw key <64 hex>` in a soroq.yaml and watched it ship intact.
		// The reasoning that let it through was that a comment carries no value -- but a comment carries
		// whatever a person typed, which is the definition of free text and the case the entropy rules
		// exist for. Nothing is known about it, so the widest rule set applies.
		if strings.HasPrefix(trimmed, "#") {
			out = append(out, redactSecrets(line))
			continue
		}
		match := configKeyLine.FindStringSubmatch(line)
		if match == nil {
			// A bare list item or a continuation. Its key is unknown, so its value is unknown too.
			omitted++
			continue
		}
		indent, key, value := match[1], match[2], match[3]
		// A key with an EMPTY value is a structural header -- `platforms:`, `android:`, `keys:` -- and
		// carries nothing that could leak. Marking it [OMITTED] destroyed the shape of the file and told
		// a reader that a value had been removed where none existed.
		if strings.TrimSpace(value) == "" {
			out = append(out, line)
			continue
		}
		if !configKeysWorthKeeping[key] {
			omitted++
			out = append(out, indent+key+": "+OMITTED)
			continue
		}
		// A kept key still goes through the CREDENTIAL rules -- a URL carrying a password is a secret
		// whatever key it arrived under -- but not the entropy rules, which cannot tell a runtime id
		// from a key.
		out = append(out, indent+key+": "+redactCredentials(value))
	}
	if omitted > 0 {
		out = append(out, fmt.Sprintf("# %d value(s) omitted: not on the support-bundle allowlist", omitted))
	}
	return strings.Join(out, "\n")
}

// redactSecrets removes anything shaped like a secret from free text: both credential shapes and bare
// high-entropy runs.
func redactSecrets(text string) string {
	return applyPatterns(text, secretPatterns)
}

// redactCredentials is the narrower pass, for a value under a key KNOWN to hold a public identifier.
//
// A runtime id is 64 hex characters and a signing key id is a hash; the entropy rules cannot tell those
// from a secret, and applying them here replaced `runtime_id` -- the single most useful value in a
// support bundle -- with [REDACTED]. Broadening a denylist is how a redactor starts eating the
// diagnostics it exists to preserve, so the entropy rules stop where the meaning of a field is known.
func redactCredentials(text string) string {
	return applyPatterns(text, credentialPatterns)
}

func applyPatterns(text string, patterns []*regexp.Regexp) string {
	for _, pattern := range patterns {
		text = pattern.ReplaceAllStringFunc(text, func(match string) string {
			// Keep the part that identifies WHAT was redacted, drop the part that is the secret. A
			// pattern with no capture group (a PEM block, a bare key) has no context worth keeping, and
			// asking for group 1 there used to panic -- taking the whole command down rather than
			// producing a bundle.
			groups := pattern.FindStringSubmatch(match)
			for _, group := range groups[min(1, len(groups)):] {
				if group != "" {
					return group + REDACTED
				}
			}
			return REDACTED
		})
	}
	return text
}

type supportBundle struct {
	GeneratedAt string            `json:"generated_at"`
	CLIVersion  string            `json:"cli_version"`
	Platform    string            `json:"platform"`
	Environment map[string]string `json:"environment"`
	EnvPresence map[string]string `json:"environment_presence"`
	Project     supportProject    `json:"project"`
	Caches      supportCaches     `json:"caches"`
	Notes       []string          `json:"notes"`
}

type supportProject struct {
	Directory   string `json:"directory"`
	HasConfig   bool   `json:"has_soroq_yaml"`
	HasLockfile bool   `json:"has_soroq_lock"`
	Config      string `json:"soroq_yaml,omitempty"`
	Lockfile    string `json:"soroq_lock,omitempty"`
}

type supportCaches struct {
	StateDir   string   `json:"state_dir"`
	Frontends  []string `json:"frontends"`
	Toolchains []string `json:"toolchains"`
}

func runSupportBundle(args []string) error {
	fs := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "project to describe")
	output := fs.String("out", "", "write the bundle to this file instead of stdout")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq support-bundle [--project-dir .] [--out bundle.json]

Collects the information a Soroq maintainer needs to help you, with secrets removed.

What it includes: CLI version, platform, which Soroq settings are set, your soroq.yaml and soroq.lock,
and the names of cached frontends and toolchains.

What it removes: every value whose key is not on the support-bundle allowlist is replaced with
`+"`[OMITTED]`"+`, and settings known to hold secrets are recorded only as set or unset. Free text --
cache names, paths, comments, environment values -- additionally has credentials of known shapes and
bare high-entropy runs replaced with `+"`[REDACTED]`"+`.

What that does NOT promise: the shape list is a denylist, so a credential format it has not been
taught, sitting in free text, can still get through. The allowlist covers your config and lockfile;
free text is best-effort. Read the file before you send it -- which is why it is plain JSON.

Read the file before you send it. It is plain JSON on purpose.`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// VALIDATE BEFORE ANY SIDE EFFECT. Go's flag package stops at the first non-flag
	// argument and leaves the rest in fs.Args(). A command that never reads them accepts
	// any number of words and silently ignores them -- and, worse, every flag AFTER such a
	// word is never parsed at all.
	if err := refuseUnconsumedArguments("support-bundle", fs.Args(), nil); err != nil {
		return err
	}

	bundle := supportBundle{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		CLIVersion:  buildVersion,
		Platform:    runtime.GOOS + "/" + runtime.GOARCH,
		Environment: map[string]string{},
		EnvPresence: map[string]string{},
		Notes:       []string{},
	}

	for _, key := range environmentKeysToReport {
		if value, ok := os.LookupEnv(key); ok {
			// Redacted even so: SOROQ_API is a URL and a URL can carry a credential.
			bundle.Environment[key] = redactSecrets(value)
		}
	}
	for _, key := range environmentKeysToNameOnly {
		if _, ok := os.LookupEnv(key); ok {
			bundle.EnvPresence[key] = "set (value not collected)"
		} else {
			bundle.EnvPresence[key] = "not set"
		}
	}

	resolvedProject, err := filepath.Abs(*projectDir)
	if err != nil {
		return err
	}
	// FREE TEXT. A directory path carries whatever the operator named it, including a token.
	bundle.Project.Directory = redactSecrets(resolvedProject)
	if raw, err := os.ReadFile(filepath.Join(resolvedProject, "soroq.yaml")); err == nil {
		bundle.Project.HasConfig = true
		bundle.Project.Config = redactConfig(string(raw))
	}
	if raw, err := os.ReadFile(filepath.Join(resolvedProject, "soroq.lock")); err == nil {
		bundle.Project.HasLockfile = true
		bundle.Project.Lockfile = redactConfig(string(raw))
	}

	stateDir, err := soroqStateDir()
	if err == nil {
		bundle.Caches.StateDir = redactSecrets(stateDir)
		bundle.Caches.Frontends = listCacheNames(filepath.Join(stateDir, "frontends"))
		bundle.Caches.Toolchains = listCacheNames(filepath.Join(stateDir, "toolchains"))
	}

	if !bundle.Project.HasConfig {
		bundle.Notes = append(bundle.Notes,
			"no soroq.yaml in this directory: run `soroq init` or pass --project-dir")
	}
	if len(bundle.Caches.Frontends) == 0 && len(bundle.Caches.Toolchains) == 0 {
		bundle.Notes = append(bundle.Notes,
			"no frontend or toolchain is installed: run `soroq setup <platform>`")
	}

	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	// THERE IS NO WHOLE-DOCUMENT PASS, AND THERE MUST NOT BE ONE. Read this before adding one back.
	//
	// Every value above is redacted BEFORE it is encoded, by the rule that fits what that value means:
	// free text gets redactSecrets, a value under a known key gets redactConfig's allowlist, and a
	// code-authored constant needs neither. That is the whole design, and it is what lets `runtime_id`
	// keep its 64 hex characters while a 64-hex cache name is removed -- no single rule has to do both,
	// because at the value level the meaning is known.
	//
	// A pass over the ENCODED document cannot work, and two rounds of evidence say so. Its patterns are
	// written for raw text, and in encoded text the delimiters are different characters:
	//
	//   - `"` ends a JSON string but is an ordinary character to `\S+`. The query-string rule turned
	//     `"...?sig=abcdefghijklmnop"` into `"...?sig=[REDACTED]` -- it ate the closing quote, and the
	//     command's own --help promises "It is plain JSON on purpose". It was not parseable JSON.
	//   - A newline is the two characters `\` and `n`, not whitespace. So `\S+` runs straight through
	//     it: `# Authorization: Bearer sekrit\napp_id: demo` became `Bearer[REDACTED] demo`, and the
	//     `app_id` key was silently destroyed. Silent loss is worse than a leak in one way -- a reader
	//     cannot tell it happened.
	//
	// Both defects have one cause: regexes that were correct over raw text applied to a different
	// language. Narrowing the pass again does not fix the class; removing it does. No pattern can eat a
	// delimiter it never sees.
	//
	// TestEveryStringInTheBundleIsClassified is the enforcement point: it fails if a field reaches this
	// document without a named collection site to redact it. Add a field, name its site.
	encodedText := string(encoded)

	if *output == "" {
		fmt.Fprintln(os.Stdout, encodedText)
		return nil
	}
	if err := os.WriteFile(*output, []byte(encodedText+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Wrote %s\nRead it before you send it.\n", *output)
	return nil
}

// listCacheNames reports the directory NAMES under a cache, never their contents.
//
// A cache directory name is FREE TEXT: nothing constrains what an operator, or their tooling, called
// it. So each name goes through the full redactSecrets, entropy rules included.
//
// THE TRADE-OFF, STATED. A cache named by nothing but a digest -- 32, 48 or 64 hex characters and no
// label -- is redacted, and its diagnostic value is lost. That is deliberate: a bare hex run is not
// distinguishable from a key, and shipping a key is worse than losing one line of diagnostics. Real
// Soroq cache names are not this shape (`soroq-android-3.44.2-release-12d3315131f5` carries a 12-hex
// chunk inside a labelled name), so the cost falls only on names the toolchain does not produce.
// A planted digest-named cache is a control in the test file; if that trade-off is ever reversed, the
// control is the thing that must change first.
func listCacheNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, redactSecrets(entry.Name()))
		}
	}
	sort.Strings(names)
	return names
}

var _ = strings.TrimSpace
