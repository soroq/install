package androidpatch

import (
	"path/filepath"
	"strings"
	"testing"
)

// metadataForGuard is the deterministic Soroq metadata asset that is identical between base and
// candidate builds (regenerated offline), so it must never count as drift.
func metadataForGuard() string {
	return testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "1.2.3+45")
}

// Test 1(a) / Fix C: a base whose tree-shaken MaterialIcons subset lacks 0xE15A + a candidate whose
// font GAINS the glyph (font bytes change) is drift the code-only patch cannot deliver => the guard
// must report drift so auto-mode fail-closes rather than emit a silent code-only patch.
func TestDetectCodePatchAssetDrift_FontGainsGlyph_Drifts(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	base := filepath.Join(tempDir, "base.apk")
	candidate := filepath.Join(tempDir, "candidate.apk")
	metadata := metadataForGuard()
	writeArtifactZip(t, base, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json":       metadata,
		"assets/flutter_assets/fonts/MaterialIcons-Regular.otf": "tree-shaken-subset-9-codepoints",
		"assets/flutter_assets/FontManifest.json":               `[{"family":"MaterialIcons"}]`,
		"lib/arm64-v8a/libapp.so":                               "base-libapp",
	})
	writeArtifactZip(t, candidate, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json":       metadata,
		"assets/flutter_assets/fonts/MaterialIcons-Regular.otf": "full-font-with-0xE15A-glyph",
		"assets/flutter_assets/FontManifest.json":               `[{"family":"MaterialIcons"}]`,
		"lib/arm64-v8a/libapp.so":                               "candidate-libapp-references-0xE15A",
	})

	drift, err := DetectCodePatchAssetDrift(base, candidate)
	if err != nil {
		t.Fatalf("DetectCodePatchAssetDrift() error = %v", err)
	}
	if !drift.HasDrift() {
		t.Fatalf("expected font drift to be detected, got none")
	}
	if !containsPath(drift.Paths(), "fonts/MaterialIcons-Regular.otf") {
		t.Fatalf("expected the changed font in the drift paths, got %v", drift.Paths())
	}
}

// Test 1(b) / Fix A interaction: when the base already ships the FULL font (Fix A forces
// --no-tree-shake-icons), an icon-only code change leaves the font bytes IDENTICAL, so only
// libapp.so differs and the guard reports NO drift (no false-trip on icon introduction).
func TestDetectCodePatchAssetDrift_FullFontUnchanged_NoDrift(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	base := filepath.Join(tempDir, "base.apk")
	candidate := filepath.Join(tempDir, "candidate.apk")
	metadata := metadataForGuard()
	fullFont := "full-materialicons-font-8667-codepoints-including-0xE15A"
	writeArtifactZip(t, base, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json":       metadata,
		"assets/flutter_assets/fonts/MaterialIcons-Regular.otf": fullFont,
		"lib/arm64-v8a/libapp.so":                               "base-libapp",
	})
	writeArtifactZip(t, candidate, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json":       metadata,
		"assets/flutter_assets/fonts/MaterialIcons-Regular.otf": fullFont,
		"lib/arm64-v8a/libapp.so":                               "candidate-libapp-references-0xE15A",
	})

	drift, err := DetectCodePatchAssetDrift(base, candidate)
	if err != nil {
		t.Fatalf("DetectCodePatchAssetDrift() error = %v", err)
	}
	if drift.HasDrift() {
		t.Fatalf("expected no drift for identical full font, got %v", drift.Paths())
	}
}

// Test 3 / Fix C: a non-icon flutter_asset drift (changed image + changed AssetManifest) between base
// and candidate must be reported as drift so auto-mode fail-closes with an actionable error.
func TestDetectCodePatchAssetDrift_ImageAndManifestDrift_Drifts(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	base := filepath.Join(tempDir, "base.apk")
	candidate := filepath.Join(tempDir, "candidate.apk")
	metadata := metadataForGuard()
	writeArtifactZip(t, base, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/assets/logo.png":           "old-image-bytes",
		"assets/flutter_assets/AssetManifest.json":        `{"assets/logo.png":["assets/logo.png"]}`,
		"lib/arm64-v8a/libapp.so":                         "shared-libapp",
	})
	writeArtifactZip(t, candidate, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/assets/logo.png":           "new-image-bytes",
		"assets/flutter_assets/AssetManifest.json":        `{"assets/logo.png":["assets/logo.png"],"assets/new.png":["assets/new.png"]}`,
		"assets/flutter_assets/assets/new.png":            "brand-new-image",
		"lib/arm64-v8a/libapp.so":                         "shared-libapp",
	})

	drift, err := DetectCodePatchAssetDrift(base, candidate)
	if err != nil {
		t.Fatalf("DetectCodePatchAssetDrift() error = %v", err)
	}
	if !drift.HasDrift() {
		t.Fatalf("expected image/manifest drift to be detected, got none")
	}
	for _, want := range []string{"assets/logo.png", "AssetManifest.json", "assets/new.png"} {
		if !containsPath(drift.Paths(), want) {
			t.Fatalf("expected drifted asset %q in %v", want, drift.Paths())
		}
	}
}

// Regression guard: a PURE Dart-logic change alters only libapp.so (and possibly kernel_blob.bin);
// the Soroq metadata asset is regenerated deterministically identical. The guard must NOT false-trip
// on these, or it would refuse EVERY legitimate code patch and break the whole native-AOT lane.
func TestDetectCodePatchAssetDrift_PureCodeChange_NoDrift(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	base := filepath.Join(tempDir, "base.apk")
	candidate := filepath.Join(tempDir, "candidate.apk")
	metadata := metadataForGuard()
	writeArtifactZip(t, base, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/kernel_blob.bin":           "base-kernel",
		"assets/flutter_assets/assets/logo.png":           "same-image",
		"lib/arm64-v8a/libapp.so":                         "base-libapp",
	})
	writeArtifactZip(t, candidate, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"assets/flutter_assets/kernel_blob.bin":           "candidate-kernel-changed",
		"assets/flutter_assets/assets/logo.png":           "same-image",
		"lib/arm64-v8a/libapp.so":                         "candidate-libapp",
	})

	drift, err := DetectCodePatchAssetDrift(base, candidate)
	if err != nil {
		t.Fatalf("DetectCodePatchAssetDrift() error = %v", err)
	}
	if drift.HasDrift() {
		t.Fatalf("expected no drift for a pure code change (only libapp.so/kernel_blob differ), got %v", drift.Paths())
	}
}

func containsPath(paths []string, substr string) bool {
	for _, p := range paths {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}
