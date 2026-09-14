// Package catalogv2 is the single source of truth for the soroq.catalog.v2 wire format and its
// selection rules. It is a leaf package importing ONLY the standard library, so the server
// (internal/api, internal/store) and the client CLI (cmd/soroq) share one definition rather than two
// copies that drift.
//
// WHY v2 EXISTS. v1 pins exactly one {frontend, toolchain} pair per platform:
//
//	platforms.ios = {frontend_version, toolchain_version}
//
// and the consumer resolves by platform id alone. That makes two Flutter versions mutually exclusive
// for a platform: publishing a 3.44.9 pair for iOS necessarily replaces the 3.44.2 pair for every
// consumer. Qualifying a new engine therefore could not be scoped — it was a global production flip
// or nothing.
//
// v2 adds the missing dimension and nothing else: a platform holds a LIST of entries, each selected by
// an EXACT Flutter revision. 3.44.2 and 3.44.9 coexist, and a consumer gets the pair matching the
// revision it is actually building with.
//
// WHAT v2 DOES NOT DO. It does not touch v1. v1 keeps its bytes, its key, its route and its clients.
// v2 is served at its own route from its own object, and no client consumes it without opting in.
//
// SELECTION IS BY REVISION, NEVER BY VERSION. flutter_version ("3.44.9") is carried for humans reading
// the document and for error messages. It is NOT a selector: version strings are ambiguous (two builds
// of "3.44.9" can differ), and selecting on them would silently hand a consumer the wrong engine. The
// 40-hex commit is exact, which is the only property worth trusting here.
package catalogv2

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Schema is the v2 schema tag. A document that does not carry exactly this string is refused, which is
// what stops a v1 document (or a toolchain manifest) being accepted at the v2 route.
const Schema = "soroq.catalog.v2"

// revisionPattern is the exact shape of a git commit revision: 40 lower-case hex characters. Anything
// else is refused rather than normalized, so "F74781F6…" or an abbreviated "f74781f6" cannot silently
// match or silently fail to match a full revision.
var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Entry is one selectable {frontend, toolchain} pair for a platform.
type Entry struct {
	// FlutterRevision is the SELECTOR: the exact 40-hex Flutter commit this pair is for.
	FlutterRevision string `json:"flutter_revision"`
	// FlutterVersion is readable metadata ("3.44.9"). Never used for selection — see the package doc.
	FlutterVersion string `json:"flutter_version"`
	// FrontendVersion and ToolchainVersion reference the EXISTING immutable registry artifacts by
	// version string, exactly as v1 does. v2 introduces no new artifact format.
	FrontendVersion  string `json:"frontend_version"`
	ToolchainVersion string `json:"toolchain_version"`
}

// Platform holds every entry published for one platform id.
type Platform struct {
	Entries []Entry `json:"entries"`
}

// Doc is the v2 document.
type Doc struct {
	Schema       string              `json:"schema"`
	GeneratedAt  string              `json:"generated_at"`
	SigningKeyID string              `json:"signing_key_id"`
	Platforms    map[string]Platform `json:"platforms"`
}

// Validate enforces every structural rule v2 relies on. It is called on BOTH sides — the server
// schema-gates a PUT with it, and the client gates a fetched document with it — so a malformed
// document cannot be stored and cannot be consumed even if it somehow was.
//
// EVERY failure is a refusal with a named cause. There is no normalization, no "best effort" entry,
// and no partial document: a document with one bad entry is rejected whole, because a consumer that
// silently skipped the bad entry could fall through to a different pair than the publisher intended.
func (d Doc) Validate() error {
	if d.Schema != Schema {
		return fmt.Errorf("REFUSED: catalog schema %q != %q", d.Schema, Schema)
	}
	if len(d.Platforms) == 0 {
		return fmt.Errorf("REFUSED: catalog has no platform entries")
	}
	for _, platform := range sortedKeys(d.Platforms) {
		entries := d.Platforms[platform].Entries
		if strings.TrimSpace(platform) == "" {
			return fmt.Errorf("REFUSED: catalog has an empty platform id")
		}
		if platform != strings.ToLower(platform) {
			return fmt.Errorf("REFUSED: platform id %q must be lower-case", platform)
		}
		if len(entries) == 0 {
			return fmt.Errorf("REFUSED: platform %q has no entries", platform)
		}
		// AMBIGUITY IS A REFUSAL, NOT A TIE-BREAK. Two entries for one revision give the consumer no
		// defensible choice, and any rule ("first wins", "last wins") would make the served bytes
		// depend on map/slice order rather than on what the publisher meant.
		seen := make(map[string]int, len(entries))
		for i, e := range entries {
			if !revisionPattern.MatchString(e.FlutterRevision) {
				return fmt.Errorf("REFUSED: platform %q entry %d: flutter_revision %q is not 40 lower-case hex characters",
					platform, i, e.FlutterRevision)
			}
			if strings.TrimSpace(e.FlutterVersion) == "" {
				return fmt.Errorf("REFUSED: platform %q entry %d (%s): flutter_version is required as readable metadata",
					platform, i, short(e.FlutterRevision))
			}
			if strings.TrimSpace(e.FrontendVersion) == "" {
				return fmt.Errorf("REFUSED: platform %q entry %d (%s): frontend_version is required",
					platform, i, short(e.FlutterRevision))
			}
			if strings.TrimSpace(e.ToolchainVersion) == "" {
				return fmt.Errorf("REFUSED: platform %q entry %d (%s): toolchain_version is required",
					platform, i, short(e.FlutterRevision))
			}
			if prev, dup := seen[e.FlutterRevision]; dup {
				return fmt.Errorf("REFUSED: platform %q has duplicate entries for flutter_revision %s (entries %d and %d); the selection would be ambiguous",
					platform, short(e.FlutterRevision), prev, i)
			}
			seen[e.FlutterRevision] = i
		}
	}
	return nil
}

// Select returns the single entry for platform at the exact flutterRevision.
//
// It REFUSES an unknown revision rather than falling back to a default, the newest entry, or the only
// entry. A consumer building at a revision the catalog does not pin has no correct pair to install,
// and guessing one is how a device ends up running an engine nobody qualified. The error names the
// revisions that ARE available, because the common cause is a stale catalog rather than a typo.
func (d Doc) Select(platform, flutterRevision string) (Entry, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	flutterRevision = strings.ToLower(strings.TrimSpace(flutterRevision))
	if !revisionPattern.MatchString(flutterRevision) {
		return Entry{}, fmt.Errorf("REFUSED: flutter revision %q is not 40 lower-case hex characters", flutterRevision)
	}
	p, ok := d.Platforms[platform]
	if !ok {
		return Entry{}, fmt.Errorf("REFUSED: catalog has no entry for platform %q (available: %s)",
			platform, strings.Join(sortedKeys(d.Platforms), ", "))
	}
	for _, e := range p.Entries {
		if e.FlutterRevision == flutterRevision {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("REFUSED: catalog pins no %s pair for flutter revision %s (pinned: %s)",
		platform, short(flutterRevision), strings.Join(p.revisionSummary(), ", "))
}

// revisionSummary renders each pinned revision with its readable version, for error messages only.
func (p Platform) revisionSummary() []string {
	out := make([]string, 0, len(p.Entries))
	for _, e := range p.Entries {
		out = append(out, fmt.Sprintf("%s (%s)", short(e.FlutterRevision), e.FlutterVersion))
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]Platform) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func short(rev string) string {
	if len(rev) >= 12 {
		return rev[:12]
	}
	return rev
}
