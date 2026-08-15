package depgraph

// Build-output comparison — the second, independent native/asset signal.
//
// Package metadata (capability.go) says what a package DECLARES and CONTAINS. It cannot see what the
// build actually produced: a transitive change can add a plugin registrant entry, a new .so, or a new
// bundled asset without any single package's pubspec changing in a way the static scan flags. So the base
// and candidate BUILD OUTPUTS are compared as well, and a code-only patch is refused whenever the shipped
// binary composition changed.
//
// Both a directory (an .app bundle, an exploded APK) and a zip/apk/aar archive are accepted.

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"soroq/backend/internal/nativeelf"
)

// OutputCategory classifies one entry of a build output.
type OutputCategory string

const (
	CatNativeLib   OutputCategory = "native-library"    // .so/.dylib/.a/.framework/.jar/.aar members
	CatRegistrant  OutputCategory = "plugin-registrant" // GeneratedPluginRegistrant.*
	CatAsset       OutputCategory = "asset"             // flutter_assets content (real assets)
	CatLicenseMeta OutputCategory = "license-metadata"  // NOTICES / NOTICES.Z
	CatDartCode    OutputCategory = "dart-code"         // kernel_blob.bin / app.dill / isolate snapshots
	CatOther       OutputCategory = "other"
)

// OutputEntry is one file in a build output.
type OutputEntry struct {
	Path     string
	Category OutputCategory
	SHA256   string
	Size     int64

	// CompareDigest is what native-library DRIFT is judged on; SHA256 remains the file's true
	// content hash and keeps its meaning everywhere else (identity, manifests, reporting).
	//
	// They differ for exactly one reason. A linker stamps every ELF with an NT_GNU_BUILD_ID note
	// derived partly from the link environment, so compiling identical sources at a different
	// absolute path yields a byte-different .so containing identical code. Comparing raw SHA256
	// made `soroq patch android --code` refuse a build-id-only difference in libdartjni.so and
	// report blocked_native_drift — which any consumer of a plugin with a native library hits, for
	// no defect at all.
	//
	// For anything that is not an ELF carrying such a note, this is the plain SHA256, so the
	// comparison is unchanged. Fail-closed by construction: an unparseable or truncated library
	// yields no normalised digest and falls back to strict equality rather than to a match.
	CompareDigest string
}

// compareDigest is the value native-library equality is tested on. Non-native entries and anything
// the normaliser cannot parse fall back to the raw hash.
func (e OutputEntry) compareDigest() string {
	if e.CompareDigest != "" {
		return e.CompareDigest
	}
	return e.SHA256
}

// nativeCompareDigest normalises a native library for comparison, returning "" when the bytes are
// not an ELF with a build-id note — in which case the caller keeps using the raw SHA256.
func nativeCompareDigest(category OutputCategory, data []byte) string {
	if category != CatNativeLib {
		return ""
	}
	digest, ok := nativeelf.ComparisonDigest(data)
	if !ok {
		return ""
	}
	return digest
}

// OutputDiff is the categorized base→candidate difference of two build outputs.
type OutputDiff struct {
	AddedNativeLibs    []string
	ChangedNativeLibs  []string
	RemovedNativeLibs  []string
	ChangedRegistrant  []string
	AddedAssets        []string
	ChangedAssets      []string
	RemovedAssets      []string
	ChangedLicenseMeta []string
}

// HasNativeOrAssetDrift reports whether the shipped binary composition changed in a way a code-only OTA
// patch cannot deliver. License metadata is deliberately NOT counted here — it is handled explicitly by
// the caller (delivered, recorded, or refused), never silently ignored.
func (d OutputDiff) HasNativeOrAssetDrift() bool {
	return len(d.AddedNativeLibs)+len(d.ChangedNativeLibs)+len(d.RemovedNativeLibs)+
		len(d.ChangedRegistrant)+len(d.AddedAssets)+len(d.ChangedAssets)+len(d.RemovedAssets) > 0
}

// Explain renders an actionable, store-release-oriented refusal message.
func (d OutputDiff) Explain() string {
	var b strings.Builder
	add := func(label string, xs []string) {
		if len(xs) == 0 {
			return
		}
		fmt.Fprintf(&b, "  %s (%d): %s\n", label, len(xs), strings.Join(cap10(xs), ", "))
	}
	add("new native libraries", d.AddedNativeLibs)
	add("changed native libraries", d.ChangedNativeLibs)
	add("removed native libraries", d.RemovedNativeLibs)
	add("changed plugin registrant", d.ChangedRegistrant)
	add("new assets", d.AddedAssets)
	add("changed assets", d.ChangedAssets)
	add("removed assets", d.RemovedAssets)
	return b.String()
}

func cap10(xs []string) []string {
	if len(xs) <= 10 {
		return xs
	}
	return append(append([]string(nil), xs[:10]...), fmt.Sprintf("… and %d more", len(xs)-10))
}

// CompareBuildOutputs scans two build outputs and returns their categorized difference.
func CompareBuildOutputs(basePath, candidatePath string) (OutputDiff, error) {
	base, err := ScanBuildOutput(basePath)
	if err != nil {
		return OutputDiff{}, fmt.Errorf("scan base build output: %w", err)
	}
	cand, err := ScanBuildOutput(candidatePath)
	if err != nil {
		return OutputDiff{}, fmt.Errorf("scan candidate build output: %w", err)
	}
	return DiffBuildOutputs(base, cand), nil
}

// DiffBuildOutputs categorizes the difference between two scanned outputs.
func DiffBuildOutputs(base, cand map[string]OutputEntry) OutputDiff {
	var d OutputDiff
	for _, p := range sortedKeys(cand) {
		c := cand[p]
		b, inBase := base[p]
		switch c.Category {
		case CatNativeLib:
			if !inBase {
				d.AddedNativeLibs = append(d.AddedNativeLibs, p)
			} else if b.compareDigest() != c.compareDigest() {
				d.ChangedNativeLibs = append(d.ChangedNativeLibs, p)
			}
		case CatRegistrant:
			if !inBase || b.SHA256 != c.SHA256 {
				d.ChangedRegistrant = append(d.ChangedRegistrant, p)
			}
		case CatAsset:
			if !inBase {
				d.AddedAssets = append(d.AddedAssets, p)
			} else if b.SHA256 != c.SHA256 {
				d.ChangedAssets = append(d.ChangedAssets, p)
			}
		case CatLicenseMeta:
			if !inBase || b.SHA256 != c.SHA256 {
				d.ChangedLicenseMeta = append(d.ChangedLicenseMeta, p)
			}
		}
	}
	for _, p := range sortedKeys(base) {
		if _, ok := cand[p]; ok {
			continue
		}
		switch base[p].Category {
		case CatNativeLib:
			d.RemovedNativeLibs = append(d.RemovedNativeLibs, p)
		case CatAsset:
			d.RemovedAssets = append(d.RemovedAssets, p)
		}
	}
	for _, xs := range [][]string{
		d.AddedNativeLibs, d.ChangedNativeLibs, d.RemovedNativeLibs, d.ChangedRegistrant,
		d.AddedAssets, d.ChangedAssets, d.RemovedAssets, d.ChangedLicenseMeta,
	} {
		sort.Strings(xs)
	}
	return d
}

// ScanBuildOutput reads a build output (zip/apk/aar/ipa archive, or a directory such as an .app bundle or
// an exploded APK) and returns its categorized entries keyed by normalized path.
func ScanBuildOutput(path string) (map[string]OutputEntry, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return scanBuildDir(path)
	}
	return scanBuildArchive(path)
}

func scanBuildDir(root string) (map[string]OutputEntry, error) {
	out := map[string]OutputEntry{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		relSlash := filepath.ToSlash(rel)
		sha, herr := sha256File(p)
		if herr != nil {
			return herr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		category := categorizeOutputPath(relSlash)
		var compare string
		if category == CatNativeLib {
			// Read in full only for native libraries; every other category still streams its hash.
			if data, rerr := os.ReadFile(p); rerr == nil {
				compare = nativeCompareDigest(category, data)
			}
		}
		out[relSlash] = OutputEntry{Path: relSlash, Category: category, SHA256: sha, Size: info.Size(), CompareDigest: compare}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func scanBuildArchive(path string) (map[string]OutputEntry, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out := map[string]OutputEntry{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.ToSlash(f.Name)
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		category := categorizeOutputPath(name)
		out[name] = OutputEntry{
			Path:          name,
			Category:      category,
			SHA256:        sha256Hex(data),
			Size:          int64(len(data)),
			CompareDigest: nativeCompareDigest(category, data),
		}
	}
	return out, nil
}

// dartCodeBasenames are the Dart code payloads a code-only patch is ALLOWED to change — they are the
// patch itself, not drift.
//
// libapp.so is the important one: on Android release builds the compiled Dart AOT snapshot ships as
// lib/<abi>/libapp.so, and the Android code lane replaces exactly that file. It has a .so extension but
// it is NOT a native library — classifying it as one would make HasNativeOrAssetDrift fire on every
// legitimate Dart-only patch. The engine's own libflutter.so is a real native library and stays so.
var dartCodeBasenames = map[string]bool{
	"kernel_blob.bin": true, "app.dill": true, "isolate_snapshot_data": true,
	"vm_snapshot_data": true, "isolate_snapshot_instr": true, "vm_snapshot_instr": true,
	"soroq_metadata.json": true, "App": true, "libapp.so": true,
}

func categorizeOutputPath(p string) OutputCategory {
	base := filepath.Base(p)
	lower := strings.ToLower(p)
	ext := strings.ToLower(filepath.Ext(base))

	if base == "NOTICES" || base == "NOTICES.Z" {
		return CatLicenseMeta
	}
	if strings.HasPrefix(base, "GeneratedPluginRegistrant") {
		return CatRegistrant
	}
	if dartCodeBasenames[base] {
		return CatDartCode
	}
	if nativeBinaryExts[ext] || strings.Contains(lower, ".framework/") || strings.Contains(lower, ".xcframework/") {
		return CatNativeLib
	}
	if strings.Contains(lower, "flutter_assets/") || strings.HasPrefix(lower, "assets/") {
		return CatAsset
	}
	return CatOther
}
