package main

// Android dependency gate for the native-AOT code lane.
//
// The Android lane replaces the whole compiled Dart payload (lib/<abi>/libapp.so), so newly added Dart
// package code is carried automatically — there is no module synthesis to extend. What the lane still
// needs is the REFUSAL half: a dependency change that pulls in native code, a plugin registration, or a
// real asset cannot be delivered by replacing libapp.so alone, and must be refused by name.
//
// Unlike the iOS freehand lane, the Android base is an APK rather than a recorded dependency graph, so
// the base side of the delta comes from the real base ARTIFACT: base and candidate build outputs are
// compared directly. That is a stronger signal than a recorded graph — it is what actually shipped —
// and it is combined with the candidate's package metadata so refusals can name the responsible package.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"soroq/backend/internal/depgraph"
)

// androidLicenseDelta records a license-metadata (NOTICES/NOTICES.Z) change that the code lane does NOT
// deliver. Empirically this is the ONLY flutter_assets entry a Dart-only dependency add moves.
type androidLicenseDelta struct {
	Changed bool
	Paths   []string
}

// Warning renders the operator-facing notice. The delta is permitted (refusing every dependency add over
// license text would make the feature useless, and a code patch physically cannot ship assets), but it is
// never silent: the operator is told exactly what the installed app's license screen will still show.
func (d androidLicenseDelta) Warning() string {
	if !d.Changed {
		return ""
	}
	return fmt.Sprintf(
		"warning: this patch changes aggregated license metadata (%s) that a code-only patch does NOT deliver.\n"+
			"  The patched app's Dart code is updated, but its license screen (showLicensePage) will keep\n"+
			"  showing the licenses bundled in the installed store release until a new store release ships.\n"+
			"  No runtime behaviour is affected. This delta is recorded with the patch.",
		strings.Join(d.Paths, ", "))
}

// assertAndroidDependencyDeliverable refuses an Android code patch whose dependency change is not
// deliverable by replacing libapp.so, and returns the license delta for recording + warning.
//
// It fails closed on any native/registrant/asset drift between the real base and candidate artifacts, and
// on a pub-resolved project whose runtime dependency graph does not resolve.
func assertAndroidDependencyDeliverable(projectDir, baseArtifact, candidateArtifact string) (androidLicenseDelta, error) {
	var zero androidLicenseDelta

	// The AUTHORITATIVE gate is the real base-vs-candidate build-output comparison: it is what actually
	// shipped, and it runs unconditionally.
	diff, err := depgraph.CompareBuildOutputs(baseArtifact, candidateArtifact)
	if err != nil {
		return zero, fmt.Errorf("compare base and candidate build outputs: %w", err)
	}

	// Candidate package metadata is used to ATTRIBUTE a refusal to the responsible package(s) — it is a
	// diagnostic, not the safety decision, so a project that was never pub-resolved (no pubspec.lock) does
	// not lose the artifact gate. A project that IS pub-resolved but whose runtime graph does not resolve
	// (an unresolved runtime edge, an unpinned dependency) is a real integrity failure and fails closed.
	candidate, graphErr := resolveRuntimeGraphPinned(projectDir)
	if graphErr != nil && projectIsPubResolved(projectDir) {
		return zero, fmt.Errorf("resolve the candidate runtime dependency graph: %w", graphErr)
	}

	if diff.HasNativeOrAssetDrift() {
		msg := &strings.Builder{}
		fmt.Fprintf(msg, "android code patch refused — the candidate build changed content a code-only patch cannot deliver:\n%s", diff.Explain())
		if attribution := ineligibleRuntimePackages(candidate); attribution != "" {
			fmt.Fprintf(msg, "  likely responsible dependenc(y/ies): %s\n", attribution)
		}
		fmt.Fprint(msg, "  A native-AOT code patch ships only the Dart payload (lib/<abi>/libapp.so), so this content would\n"+
			"  never reach the device. It requires a new Play Store release that bundles it.")
		return zero, fmt.Errorf("%s", msg.String())
	}

	return androidLicenseDelta{
		Changed: len(diff.ChangedLicenseMeta) > 0,
		Paths:   append([]string(nil), diff.ChangedLicenseMeta...),
	}, nil
}

// projectIsPubResolved reports whether the project has actually been through `pub get`. If it has, its
// runtime graph MUST resolve; if it has not, there is no dependency metadata to read and the artifact
// comparison alone governs.
func projectIsPubResolved(projectDir string) bool {
	return fileExists(filepath.Join(projectDir, "pubspec.lock")) &&
		fileExists(filepath.Join(projectDir, ".dart_tool", "package_config.json"))
}

// ineligibleRuntimePackages names the candidate's runtime packages that are NOT code-only deliverable,
// so a build-output refusal can point at the dependency that caused it instead of only the file paths.
func ineligibleRuntimePackages(g depgraph.Graph) string {
	var out []string
	for _, name := range g.PackageNames() {
		p := g.Packages[name]
		if p.Capability.Eligible {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s (%s)", p.Name, p.Version, strings.Join(p.Capability.Reasons, "; ")))
	}
	sort.Strings(out)
	if len(out) == 0 {
		return ""
	}
	if len(out) > 5 {
		out = append(out[:5], fmt.Sprintf("… and %d more", len(out)-5))
	}
	return strings.Join(out, ", ")
}
