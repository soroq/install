package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"soroq/backend/internal/domain"
)

// Duplicate-release detection BEFORE the build, not after it.
//
// A release is immutable: its id binds one app+runtime+version+arch to one artifact forever. Re-running
// `soroq release android` for a version that already shipped therefore cannot succeed -- but until now
// the collision surfaced only when the create call ran, which is AFTER a Gradle build that routinely
// takes ten to fifteen minutes. The user paid the entire build cost to be told the release already
// existed, and the message did not say which of the two ways out to take.
//
// The prospective release identity is knowable before compiling anything: the app id and channel come
// from soroq.yaml, and the version comes from pubspec.yaml (the same value the build will stamp).
// Checking it costs one GET. The one thing that is NOT knowable pre-build is the runtime id, which is
// derived from the built artifact -- so this preflight never claims identity it cannot see. It either
// proves an idempotent re-run from the immutable local snapshot's digest, proves a conflict, or stands
// aside and lets the existing post-build path decide.

type releasePreflightVerdict string

const (
	// No release claims this identity: build.
	releasePreflightClear releasePreflightVerdict = "clear"
	// The release exists AND the local immutable snapshot proves the same artifact: skip the build.
	releasePreflightIdempotent releasePreflightVerdict = "idempotent"
	// The release exists and cannot accept this build: fail now, before the build.
	releasePreflightConflict releasePreflightVerdict = "conflict"
	// Something is unknown (offline, no artifact record). Build and let the existing path decide.
	releasePreflightUnknown releasePreflightVerdict = "unknown"
)

type releasePreflightResult struct {
	Verdict  releasePreflightVerdict
	Existing *domain.Release
	// Detail explains the verdict in one line, for the phase timeline.
	Detail string
}

// releasePreflightInputs is everything knowable before a build.
type releasePreflightInputs struct {
	AppID     string
	Channel   string
	Platform  string
	Version   string // from pubspec.yaml; empty when it cannot be inferred
	Arch      string // only when explicitly requested; empty is fine
	ReleaseID string // only when explicitly requested via --release-id
}

// releasePreflightDeps isolates I/O so the decision logic is testable without a network or a filesystem.
type releasePreflightDeps struct {
	// ListReleases returns every release registered for the app.
	ListReleases func(appID string) ([]domain.Release, error)
	// HostedArtifactDigest returns the sha256 recorded for a release's uploaded artifact, or "".
	HostedArtifactDigest func(releaseID string) string
	// LocalSnapshotDigest returns the sha256 of the immutable local snapshot for a release, or "".
	LocalSnapshotDigest func(releaseID string) string
}

// preflightRelease decides whether building is worthwhile.
//
// It deliberately does NOT compare runtime ids: those exist only after a build, and a preflight that
// pretended otherwise would either block legitimate rebuilds or wave through real conflicts.
func preflightRelease(in releasePreflightInputs, deps releasePreflightDeps) releasePreflightResult {
	if strings.TrimSpace(in.AppID) == "" || deps.ListReleases == nil {
		return releasePreflightResult{Verdict: releasePreflightUnknown, Detail: "no app id to check"}
	}
	releases, err := deps.ListReleases(in.AppID)
	if err != nil {
		// The control plane may be unreachable, or the app may not be registered yet. Neither is a
		// reason to refuse to build; the post-build path already handles both.
		return releasePreflightResult{Verdict: releasePreflightUnknown, Detail: "control plane not reachable for preflight"}
	}

	match := findReleaseClaimingIdentity(releases, in)
	if match == nil {
		return releasePreflightResult{Verdict: releasePreflightClear, Detail: "no existing release claims this version"}
	}

	// The release exists. If the immutable local snapshot carries the same bytes the control plane
	// already holds, this invocation has nothing left to do -- that is an idempotent re-run, and
	// rebuilding would produce the same artifact at the cost of a full Gradle cycle.
	if deps.LocalSnapshotDigest != nil && deps.HostedArtifactDigest != nil {
		local := strings.TrimSpace(deps.LocalSnapshotDigest(match.ID))
		hosted := strings.TrimSpace(deps.HostedArtifactDigest(match.ID))
		if local != "" && hosted != "" {
			if strings.EqualFold(local, hosted) {
				return releasePreflightResult{
					Verdict:  releasePreflightIdempotent,
					Existing: match,
					Detail:   fmt.Sprintf("release %s already holds this exact artifact", match.ID),
				}
			}
			return releasePreflightResult{
				Verdict:  releasePreflightConflict,
				Existing: match,
				Detail: fmt.Sprintf("release %s exists with a DIFFERENT artifact (hosted %s, local %s)",
					match.ID, shortDigest(hosted), shortDigest(local)),
			}
		}
	}

	return releasePreflightResult{
		Verdict:  releasePreflightConflict,
		Existing: match,
		Detail:   fmt.Sprintf("release %s already exists for version %s", match.ID, match.Version),
	}
}

// findReleaseClaimingIdentity picks the release this build would collide with, if any.
//
// An explicit --release-id is matched exactly. Otherwise the collision is on the tuple a store build
// actually reuses: app + platform + channel + version (and arch when the caller pinned one), because
// that is what `defaultReleaseID` derives its id from.
func findReleaseClaimingIdentity(releases []domain.Release, in releasePreflightInputs) *domain.Release {
	wantID := strings.TrimSpace(in.ReleaseID)
	candidates := make([]domain.Release, 0, len(releases))
	for _, r := range releases {
		if !strings.EqualFold(r.AppID, in.AppID) {
			continue
		}
		if wantID != "" {
			if r.ID == wantID {
				found := r
				return &found
			}
			continue
		}
		if in.Platform != "" && !strings.EqualFold(r.Platform, in.Platform) {
			continue
		}
		if in.Channel != "" && !strings.EqualFold(r.Channel, in.Channel) {
			continue
		}
		if in.Version == "" || r.Version != in.Version {
			continue
		}
		if in.Arch != "" && !strings.EqualFold(r.Arch, in.Arch) {
			continue
		}
		candidates = append(candidates, r)
	}
	if len(candidates) == 0 {
		return nil
	}
	// Deterministic pick so the message is stable across runs.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	found := candidates[0]
	return &found
}

// releaseConflictError is the pre-build failure, written so the reader knows both ways out.
func releaseConflictError(result releasePreflightResult, projectDir string) error {
	existingID, existingVersion := "", ""
	if result.Existing != nil {
		existingID, existingVersion = result.Existing.ID, result.Existing.Version
	}
	return fmt.Errorf(`release %s already exists (version %s) and releases are immutable

%s

Nothing was built; no time was spent compiling. Choose one:

  1. Reuse the existing release (no rebuild needed):
       soroq patch android --release-id %s

  2. Ship a new release by incrementing the version in %s/pubspec.yaml:
       version: <bump the +build number>
     then re-run this command`,
		existingID, existingVersion, result.Detail, existingID, strings.TrimRight(projectDir, "/"))
}

// channelOverrideForPreflight resolves the channel using only what is knowable before a build.
//
// The post-build path prefers the channel stamped into the artifact's bundled metadata; that metadata
// does not exist yet here, so an explicit --channel wins and otherwise soroq.yaml decides.
func channelOverrideForPreflight(fs *flag.FlagSet, channelFlag string) string {
	if flagWasSet(fs, "channel") {
		return channelFlag
	}
	return ""
}

// reportIdempotentRelease states plainly that the run was a no-op, and why that is correct.
func reportIdempotentRelease(result releasePreflightResult, projectDir string, jsonOut bool) error {
	existing := domain.Release{}
	if result.Existing != nil {
		existing = *result.Existing
	}
	if jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(map[string]any{
			"status":     "idempotent",
			"reason":     result.Detail,
			"release":    existing,
			"built":      false,
			"artifact":   filepath.Join(strings.TrimRight(projectDir, "/"), ".soroq", "releases", existing.ID),
			"next":       "soroq patch android --release-id " + existing.ID,
			"schema":     "soroq.release.idempotent.v1",
			"api_action": "none",
		})
	}
	fmt.Fprintf(os.Stdout, `Release %s is already registered with this exact artifact — nothing to do.

  app_id:     %s
  runtime_id: %s
  version:    %s (%s)
  channel:    %s
  snapshot:   %s

No build ran: the control plane already holds these bytes, so rebuilding could only reproduce them.

Next: soroq patch android --release-id %s
`,
		existing.ID, existing.AppID, existing.RuntimeID, existing.Version, existing.Arch, existing.Channel,
		filepath.Join(strings.TrimRight(projectDir, "/"), ".soroq", "releases", existing.ID),
		existing.ID)
	return nil
}

func shortDigest(hex string) string {
	h := strings.TrimSpace(hex)
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// localReleaseSnapshotDigest hashes the immutable artifact Soroq stashed under
// .soroq/releases/<release-id>/ at release time. Absent or unreadable is "" -- not an error, because
// the preflight treats "cannot prove identity" as a reason to stand aside, never as a verdict.
func localReleaseSnapshotDigest(projectDir string, releaseID string) string {
	dir := filepath.Join(strings.TrimRight(projectDir, "/"), ".soroq", "releases", releaseID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".aab", ".apk":
			digest, hashErr := sha256File(filepath.Join(dir, entry.Name()))
			if hashErr != nil {
				return ""
			}
			return digest
		}
	}
	return ""
}

// hostedReleaseArtifactDigest reads the sha256 the control plane recorded for a release's artifact.
// A missing record is not an error here: it just means the preflight cannot prove identity.
func hostedReleaseArtifactDigest(apiBase string, releaseID string) string {
	artifact, err := getJSONDecode[domain.ReleaseArtifact](
		strings.TrimRight(apiBase, "/") + "/v1/releases/" + url.PathEscape(releaseID) + "/artifact")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(artifact.SHA256)
}
