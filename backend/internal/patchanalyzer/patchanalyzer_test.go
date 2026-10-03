package patchanalyzer

import (
	"os"
	"testing"
)

func TestExtractWritesOnceAndIsStable(t *testing.T) {
	if !Available() {
		t.Fatal("the CLI must carry a patch analyzer")
	}
	root := t.TempDir()
	p1, err := Extract(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p1)
	if err != nil || info.Size() < 1<<20 {
		t.Fatalf("extracted analyzer missing or truncated: %v %v", info, err)
	}
	p2, err := Extract(root)
	if err != nil || p2 != p1 {
		t.Fatalf("second extract = %q, %v; want the same file", p2, err)
	}
	if DartVersion() == "" {
		t.Fatal("dart_version must name the SDK the analyzer was compiled by")
	}
}
