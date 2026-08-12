package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	androidrelease "soroq/backend/internal/androidrelease"
)

// The trust anchor a device enforces is the one COMPILED INTO the artifact, not the one in soroq.yaml.
// Those can silently diverge: Flutter caches assets/flutter_assets, so a build can ship a previously
// generated soroq_metadata.json even after soroq.yaml changed. That is not hypothetical -- a release
// built minutes after a manifest_trust edit shipped the OLD trust set, including a key whose private
// half had been regenerated away, and nothing in the release path noticed.
//
// Registering that release would bind a runtime id to an artifact whose embedded identity disagrees
// with the project. Every later patch is verified against the SHIPPED anchor, so the divergence
// surfaces on the device as an unexplained refusal, long after the cause.
//
// This compares the built artifact's embedded metadata against what the project would generate right
// now, and refuses to register on a mismatch.

type artifactMetadataMismatch struct {
	Field      string
	InArtifact string
	InProject  string
}

// verifyArtifactMetadataMatchesProject recomputes the bundled metadata from the CURRENT soroq.yaml and
// pubspec.yaml and compares the identity-bearing fields against what the artifact actually carries.
func verifyArtifactMetadataMatchesProject(projectDir string, snapshot *androidrelease.Snapshot) error {
	if snapshot == nil {
		return nil
	}
	configBytes, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		// No soroq.yaml is a different, already-reported failure; nothing to compare against here.
		return nil
	}
	pubspecBytes, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		return nil
	}
	expected, err := buildSoroqBundledMetadata(configBytes, pubspecBytes)
	if err != nil {
		// A malformed project is reported by the generator itself, with a better message than a diff.
		return nil
	}

	var mismatches []artifactMetadataMismatch
	add := func(field, inArtifact, inProject string) {
		if strings.TrimSpace(inProject) == "" {
			// The project cannot pin this field (e.g. no manifest_trust); nothing to enforce.
			return
		}
		if strings.TrimSpace(inArtifact) != strings.TrimSpace(inProject) {
			mismatches = append(mismatches, artifactMetadataMismatch{field, inArtifact, inProject})
		}
	}

	add("runtime_id", snapshot.Metadata.Soroq.RuntimeID, expected.Soroq.RuntimeID)
	add("manifest_trust_fingerprint",
		derefOrEmpty(snapshot.Metadata.Soroq.ManifestTrustFingerprint),
		derefOrEmpty(expected.Soroq.ManifestTrustFingerprint))

	if len(mismatches) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "the built artifact's embedded Soroq metadata does not match this project\n\n")
	fmt.Fprintf(&b, "  artifact: %s\n\n", snapshot.Artifact.Path)
	for _, m := range mismatches {
		fmt.Fprintf(&b, "  %s\n      in artifact: %s\n      in project:  %s\n",
			m.Field, displayOrNone(m.InArtifact), displayOrNone(m.InProject))
	}
	b.WriteString(`
The device enforces the anchor compiled into the artifact, so registering this build would bind a
release to an identity the project no longer has -- and later patches would be refused on the device
for reasons that point nowhere.

This is almost always a stale Flutter asset cache. Rebuild from clean:

    flutter clean && soroq release android

Nothing was registered.`)
	return fmt.Errorf("%s", b.String())
}

func derefOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func displayOrNone(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(absent)"
	}
	return v
}
