package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// When SOROQ ran the build, the artifact it then discovers must be one THIS build produced.
//
// Artifact discovery globs a fixed set of output paths and returns the newest match. That is correct
// for the ordinary single-flavor project, and silently wrong the moment the build writes somewhere the
// globs do not cover -- most obviously `flutter build apk --flavor prod`, which emits
// build/app/outputs/apk/prod/release/ rather than the apk/release/ path Soroq looks in. Discovery then
// finds nothing from this build and happily returns a LEFTOVER artifact from an earlier, unrelated
// build. The release registers, the digests are internally consistent, and the shipped code is not the
// code the developer just built.
//
// The check is a timestamp, not a path heuristic, so it holds for any reason the build failed to land
// where Soroq looks: flavors, custom output dirs, a build that silently produced nothing.
//
// It applies ONLY when Soroq ran the build. An explicit --artifact, or --build=false, is the developer
// naming the file on purpose and is left alone.
func guardStaleDiscoveredArtifact(artifactPath string, buildStartedAt time.Time) error {
	if buildStartedAt.IsZero() || strings.TrimSpace(artifactPath) == "" {
		return nil
	}
	info, err := os.Stat(artifactPath)
	if err != nil {
		return nil // a missing artifact is reported by the caller's own not-found path
	}
	// Filesystem timestamp granularity can be as coarse as a second; allow a small margin so a
	// genuinely fresh artifact is never rejected for rounding.
	if !info.ModTime().Before(buildStartedAt.Add(-2 * time.Second)) {
		return nil
	}
	return fmt.Errorf(`the artifact Soroq found is older than the build it just ran

  found:        %s
  last written: %s
  build began:  %s

Soroq built, then discovered an artifact that predates that build — so this file is left over from an
earlier build and is NOT the code you just compiled. Registering it would ship the wrong code under a
release that looks internally consistent.

The usual cause is a build whose output lands outside the paths Soroq scans, most commonly a flavored
build (--flavor <name> writes build/app/outputs/apk/<flavor>/release/). Soroq has no flavor support
and does not guess.

Point Soroq at the file your build actually produced:

    soroq release android --build=false --artifact <path-to-your-artifact>`,
		artifactPath,
		info.ModTime().UTC().Format(time.RFC3339),
		buildStartedAt.UTC().Format(time.RFC3339))
}
