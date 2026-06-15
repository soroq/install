package main

import (
	"path/filepath"
	"testing"
)

const testSoroqFlutterPubspec = "dependencies:\n  soroq_flutter: any\n"

func writeSoroqFlutterPubspec(t *testing.T, projectDir string) {
	t.Helper()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), testSoroqFlutterPubspec)
}
