package androidrelease

import (
	"bytes"
	"debug/elf"
	"os"
	"path/filepath"
	"testing"

	"soroq/backend/internal/nativeelf"
)

// THE FOURTH COPY of the GNU build-id comparison.
//
// Three earlier passes each fixed a native-library comparison and each was described as complete:
// soroqctl's planner, androidpatch.PrepareCodePatchPlan, then depgraph.DiffBuildOutputs. This one
// sits behind CompareSnapshots and reaches users through `soroq patch android --kind asset` and the
// auto-mode fallback, where plan.go turns comparison.Compatible into a hard refusal.
//
// It was structurally different from the other three, which is why it survived them: EntryDigest
// carries {Path, SHA256, SizeBytes} and no bytes, and a Snapshot is serialized to JSON and reloaded
// later — so there is nothing to normalise at the comparison site. The digest has to be captured at
// scan time, which is a schema addition rather than a call-site swap.

const buildIDFixtureRoot = "../nativeelf/testdata/native_build_id"

func readBuildIDFixture(t *testing.T, abi string, variant string) []byte {
	t.Helper()

	path := filepath.Join(buildIDFixtureRoot, abi, "buildid_"+variant+".so")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %q: %v", path, err)
	}
	return raw
}

func nativeEntry(t *testing.T, path string, raw []byte) EntryDigest {
	t.Helper()

	entry := EntryDigest{Path: path, SHA256: sha256Hex(raw), SizeBytes: uint64(len(raw))}
	// Built the way readNativeLibrariesFromZip builds it, so the test exercises the real shape.
	if digest, ok := nativeComparisonDigestForTest(raw); ok {
		entry.CompareDigest = digest
	}
	return entry
}

func TestCompareNativeLibraryMapsIgnoresBuildIDOnlyDifference(t *testing.T) {
	t.Parallel()

	base := map[string]EntryDigest{}
	candidate := map[string]EntryDigest{}
	for _, abi := range []string{"arm64-v8a", "armeabi-v7a", "x86_64"} {
		path := "lib/" + abi + "/libflutter.so"
		rawA := readBuildIDFixture(t, abi, "a")
		rawB := readBuildIDFixture(t, abi, "b")
		if bytes.Equal(rawA, rawB) {
			t.Fatalf("%s: fixtures are identical; the test would prove nothing", abi)
		}
		base[path] = nativeEntry(t, path, rawA)
		candidate[path] = nativeEntry(t, path, rawB)
	}

	if !compareNativeLibraryMaps(base, candidate) {
		t.Fatal("a build-id-only difference is still reported as native drift on the asset lane; " +
			"`soroq patch android --kind asset` refuses a patch that is in fact deliverable")
	}
}

// CONTROL. Real code drift must still be refused, or the change above is a hole.
func TestCompareNativeLibraryMapsStillBlocksRealCodeDrift(t *testing.T) {
	t.Parallel()

	for _, abi := range []string{"arm64-v8a", "armeabi-v7a", "x86_64"} {
		t.Run(abi, func(t *testing.T) {
			t.Parallel()

			path := "lib/" + abi + "/libflutter.so"
			raw := readBuildIDFixture(t, abi, "a")

			file, err := elf.NewFile(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("parse ELF: %v", err)
			}
			text := file.Section(".text")
			if text == nil {
				t.Fatal(".text not found")
			}
			mutated := make([]byte, len(raw))
			copy(mutated, raw)
			mutated[text.Offset+text.Size/2] ^= 0xff

			base := map[string]EntryDigest{path: nativeEntry(t, path, raw)}
			candidate := map[string]EntryDigest{path: nativeEntry(t, path, mutated)}

			if compareNativeLibraryMaps(base, candidate) {
				t.Fatal("a real .text change was accepted as unchanged; normalisation is masking " +
					"code drift and would ship an undeliverable patch")
			}
		})
	}
}

// BACKWARD COMPATIBILITY, which is the whole reason this is a field with `omitempty` rather than a
// schema version bump. A snapshot captured before the field existed must still load and compare, and
// it must compare STRICTLY — the fallback direction has to be the one that can only refuse.
func TestCompareNativeLibraryMapsFallsBackToStrictForOldSnapshots(t *testing.T) {
	t.Parallel()

	path := "lib/arm64-v8a/libflutter.so"
	rawA := readBuildIDFixture(t, "arm64-v8a", "a")
	rawB := readBuildIDFixture(t, "arm64-v8a", "b")

	withDigest := nativeEntry(t, path, rawB)
	if withDigest.CompareDigest == "" {
		t.Fatal("fixture produced no compare digest; the rest of this test would be vacuous")
	}

	// An OLD snapshot: same bytes, but written before the field existed.
	oldStyle := EntryDigest{Path: path, SHA256: sha256Hex(rawA), SizeBytes: uint64(len(rawA))}

	if compareNativeLibraryMaps(
		map[string]EntryDigest{path: oldStyle},
		map[string]EntryDigest{path: withDigest},
	) {
		t.Fatal("a snapshot with no compare_digest was treated as matching; a missing digest must " +
			"only ever cause a refusal, never an acceptance")
	}

	// And an old-style pair of IDENTICAL bytes must still compare equal, or old snapshots stop working.
	sameOld := EntryDigest{Path: path, SHA256: sha256Hex(rawA), SizeBytes: uint64(len(rawA))}
	if !compareNativeLibraryMaps(
		map[string]EntryDigest{path: oldStyle},
		map[string]EntryDigest{path: sameOld},
	) {
		t.Fatal("two pre-field snapshots of identical bytes no longer compare equal; this change " +
			"broke every snapshot captured before it")
	}
}

// A size difference must be refused before any digest is consulted.
func TestCompareNativeLibraryMapsRefusesSizeMismatch(t *testing.T) {
	t.Parallel()

	path := "lib/arm64-v8a/libflutter.so"
	raw := readBuildIDFixture(t, "arm64-v8a", "a")
	base := nativeEntry(t, path, raw)
	candidate := nativeEntry(t, path, raw)
	candidate.SizeBytes++

	if compareNativeLibraryMaps(
		map[string]EntryDigest{path: base},
		map[string]EntryDigest{path: candidate},
	) {
		t.Fatal("a size mismatch was accepted")
	}
}

// nativeComparisonDigestForTest mirrors what readNativeLibrariesFromZip does when capturing a
// snapshot. It is a thin indirection so the tests build entries the same way production does rather
// than hand-writing a digest that might not match.
func nativeComparisonDigestForTest(raw []byte) (string, bool) {
	return nativeelf.ComparisonDigest(raw)
}
