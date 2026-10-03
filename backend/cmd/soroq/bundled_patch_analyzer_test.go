package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/patchanalyzer"
)

// The CLI's own analyzer serves a project whose frontend runs the Dart SDK it was compiled by, and only
// that one: a different kernel format would not load.
func TestBundledPatchAnalyzerUsesTheEmbeddedCopyForTheMatchingSDK(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	sdk := filepath.Join(root, "bin", "cache", "dart-sdk")
	if err := os.MkdirAll(sdk, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(v string) {
		if err := os.WriteFile(filepath.Join(sdk, "version"), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(patchanalyzer.DartVersion())
	p, ok := bundledPatchAnalyzerFor(root)
	if !ok || !strings.Contains(p, filepath.Join(".soroq", "analyzers")) {
		t.Fatalf("matching SDK: got %q ok=%v, want the extracted embedded analyzer", p, ok)
	}
	write("0.0.1")
	if p, ok := bundledPatchAnalyzerFor(root); ok {
		t.Fatalf("a different SDK must not get the embedded analyzer, got %q", p)
	}
}
