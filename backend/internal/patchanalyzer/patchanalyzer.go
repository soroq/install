// Package patchanalyzer carries the Soroq patch analyzer (the kernel-level module builder) inside the
// CLI binary. The analyzer is a Dart kernel program run by the project's own frontend `dart`, so it is
// only usable with the exact Dart SDK it was compiled by; DartVersion records that SDK.
//
// Shipping it inside the binary means the installer, `soroq update`, the release checksums and the build
// attestation all cover it with no extra asset. Extract writes it once per content digest under
// ~/.soroq/analyzers/.
package patchanalyzer

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

//go:embed soroq_patch_analyzer.dill.gz
var compressed []byte

//go:embed dart_version
var dartVersion string

// DartVersion is the Dart SDK version (bin/cache/dart-sdk/version) the analyzer was compiled by.
func DartVersion() string { return strings.TrimSpace(dartVersion) }

// Available reports whether this build carries an analyzer at all.
func Available() bool { return len(compressed) > 0 && DartVersion() != "" }

// Extract returns the path of the analyzer .dill under root (normally ~/.soroq/analyzers), writing it
// on first use. The directory is named by the content digest, so a new CLI never reuses an old file.
func Extract(root string) (string, error) {
	if !Available() {
		return "", fmt.Errorf("this soroq build carries no patch analyzer")
	}
	sum := sha256.Sum256(compressed)
	dir := filepath.Join(root, hex.EncodeToString(sum[:8]))
	dill := filepath.Join(dir, "soroq_patch_analyzer.dill")
	if info, err := os.Stat(dill); err == nil && info.Size() > 0 {
		return dill, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return "", fmt.Errorf("embedded patch analyzer is corrupt: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "soroq_patch_analyzer-*.tmp")
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(tmp, zr)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmp.Name())
		if copyErr != nil {
			return "", copyErr
		}
		return "", closeErr
	}
	if err := os.Rename(tmp.Name(), dill); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return dill, nil
}
