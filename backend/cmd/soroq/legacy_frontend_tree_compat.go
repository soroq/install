package main

import "strings"

// Legacy frontend TREE layouts, bound to EXACT immutable artifacts.
//
// verifyFrontendTreeMatchesManifest proves an extracted frontend is the tree its signed manifest
// describes, by reading three markers: the SDK's git HEAD, bin/cache/dart-sdk/revision, and
// bin/internal/engine.version. The live 3.44.2 frontend -- the one catalog v1 serves to every default
// `soroq setup` -- predates that convention:
//
//   - it has no bin/internal/engine.version; its engine revision is in bin/cache/engine.stamp;
//   - its manifest declares the Dart VERSION STRING "3.13.0-103.1.beta" while its tree carries the Dart
//     COMMIT 9576691c… (the same Dart; see dart_revision_compat.go for why that frontend's manifest uses
//     the version form).
//
// Without this table the strict check refused that frontend after a full 1.08 GiB download, so any CLI
// carrying the check could not complete the production default install.
//
// The exemption is NOT a skipped check. Every marker is still read from the tree and compared, only at
// the location/form this artifact actually uses, and every expected value is pinned here. It is keyed on
// the frontend version AND the exact signed archive SHA-256, so the same version with different bytes, or
// any other frontend, gets the strict check. It is backward compatibility for one existing artifact, not a
// mechanism for new ones.
type legacyFrontendTreeLayout struct {
	archiveSHA256    string // the signed archive this layout describes
	manifestDart     string // the non-SHA dart_revision the manifest declares
	treeDartRevision string // what bin/cache/dart-sdk/revision must contain
	engineMarker     string // tree-relative path holding the engine revision
}

var legacyFrontendTreeLayouts = map[string]legacyFrontendTreeLayout{
	"soroq-flutter-frontend-f74781f6-7277aaec-1a113cf9-clean-r5": {
		archiveSHA256:    "de993bb264f2cafac60c06fe0dc4786afcc64059f2eaee0e352cae0496882d6a",
		manifestDart:     "3.13.0-103.1.beta",
		treeDartRevision: "9576691c37d84d3b66a9722e4fadacc764f04b21",
		engineMarker:     "bin/cache/engine.stamp",
	},
}

// legacyTreeLayoutFor returns the enumerated layout only when version, archive and declared Dart all match.
func legacyTreeLayoutFor(m frontendManifest) (legacyFrontendTreeLayout, bool) {
	l, ok := legacyFrontendTreeLayouts[strings.TrimSpace(m.SoroqFrontendVersion)]
	if !ok {
		return legacyFrontendTreeLayout{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(m.Archive.SHA256), l.archiveSHA256) ||
		strings.TrimSpace(m.DartRevision) != l.manifestDart {
		return legacyFrontendTreeLayout{}, false
	}
	return l, true
}
