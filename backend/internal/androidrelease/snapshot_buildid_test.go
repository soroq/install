package androidrelease

import (
	"bytes"
	"debug/elf"
	"encoding/json"
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

// END TO END THROUGH THE SHAPE THE SHIPPED ASSET LANE ACTUALLY USES.
//
// The tests above build EntryDigest maps by hand, which proves the comparison but NOT that
// readNativeLibrariesFromZip records the digest, and not that it survives the JSON round trip the
// real lane performs — patch_cmd.go writes base-snapshot.json and PreparePlan reloads it. A digest
// computed but never persisted would pass every test above and fail in production.
//
// So this drives the real path: capture two APK zips -> marshal -> LoadSnapshot -> CompareSnapshots.
func TestCaptureCompareRoundTripAcceptsBuildIDOnlyNativeDrift(t *testing.T) {
	t.Parallel()

	capture := func(lib []byte, dart []byte) *Snapshot {
		path := filepath.Join(t.TempDir(), "app-release.apk")
		writeArtifactZip(t, path, map[string][]byte{
			"assets/flutter_assets/soroq/soroq_metadata.json": buildIDRoundTripMetadata,
			"lib/arm64-v8a/libdartjni.so":                     lib,
			"lib/arm64-v8a/libapp.so":                         dart,
		})
		snapshot, err := CaptureSnapshot(path)
		if err != nil {
			t.Fatalf("CaptureSnapshot: %v", err)
		}
		// The round trip the shipped lane performs: snapshots are written to disk and reloaded.
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		reloadPath := filepath.Join(t.TempDir(), "snapshot.json")
		if err := os.WriteFile(reloadPath, encoded, 0o644); err != nil {
			t.Fatalf("write snapshot: %v", err)
		}
		reloaded, err := LoadSnapshot(reloadPath)
		if err != nil {
			t.Fatalf("LoadSnapshot: %v", err)
		}
		return reloaded
	}

	// ONE VARIABLE: the build-id of libdartjni.so. libapp.so is held IDENTICAL on purpose.
	//
	// The first version of this test also varied libapp.so and failed — correctly. Unlike depgraph,
	// androidrelease treats every lib/**/*.so as a native library, libapp.so included, and this
	// comparison guards the ASSET lane, where the Dart payload is precisely what must NOT change.
	// Varying both conflated a legitimate refusal with the defect under test.
	dart := []byte("dart payload, identical on both sides")
	base := capture(readBuildIDFixture(t, "arm64-v8a", "a"), dart)
	candidate := capture(readBuildIDFixture(t, "arm64-v8a", "b"), dart)

	// The digest must have SURVIVED serialization, or the rest of this is accidental.
	//
	// Looked up BY PATH, not by index. NativeLibs is sorted, so index 0 is libapp.so -- the Dart
	// payload, which is not an ELF and correctly carries no digest. Indexing blindly made this
	// assertion fail against a working implementation.
	var dartjni EntryDigest
	for _, entry := range base.NativeLibs {
		if entry.Path == "lib/arm64-v8a/libdartjni.so" {
			dartjni = entry
		}
	}
	if dartjni.Path == "" {
		t.Fatal("libdartjni.so is missing from the captured snapshot")
	}
	if dartjni.CompareDigest == "" {
		t.Fatal("compare_digest did not survive the JSON round trip; the shipped lane reloads " +
			"snapshots from disk, so a digest that is computed but not persisted buys nothing")
	}

	report := CompareSnapshots(base, candidate)
	for _, check := range report.Checks {
		if check.ID == "native_libraries" && !check.Passed {
			t.Fatalf("native_libraries check failed on a build-id-only difference: %s", check.Detail)
		}
	}
	if !report.Compatible {
		t.Fatalf("CompareSnapshots reported incompatible on a build-id-only difference; "+
			"`soroq patch android --kind asset` would refuse this: %+v", report.Checks)
	}
}

// CONTROL for the round trip: real code drift must still be refused all the way through.
func TestCaptureCompareRoundTripStillRefusesRealCodeDrift(t *testing.T) {
	t.Parallel()

	raw := readBuildIDFixture(t, "arm64-v8a", "a")
	file, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse ELF: %v", err)
	}
	text := file.Section(".text")
	mutated := make([]byte, len(raw))
	copy(mutated, raw)
	mutated[text.Offset+text.Size/2] ^= 0xff

	capture := func(lib []byte) *Snapshot {
		path := filepath.Join(t.TempDir(), "app-release.apk")
		writeArtifactZip(t, path, map[string][]byte{
			"assets/flutter_assets/soroq/soroq_metadata.json": buildIDRoundTripMetadata,
			"lib/arm64-v8a/libdartjni.so":                     lib,
			"lib/arm64-v8a/libapp.so":                         []byte("dart"),
		})
		snapshot, err := CaptureSnapshot(path)
		if err != nil {
			t.Fatalf("CaptureSnapshot: %v", err)
		}
		return snapshot
	}

	report := CompareSnapshots(capture(raw), capture(mutated))
	if report.Compatible {
		t.Fatal("a real .text change was reported compatible end to end; normalisation is masking " +
			"genuine native drift on the asset lane")
	}
}

var buildIDRoundTripMetadata = []byte(`{
  "schema_version": 1,
  "app": { "name": "Example", "version": "1.2.3+45", "build_name": "1.2.3", "build_number": "45" },
  "soroq": {
    "app_id": "com.example.app",
    "channel": "stable",
    "runtime_id": "runtime-1",
    "runtime_id_strategy": "manifest_trust_v1",
    "manifest_trust": { "keys": [ { "id": "prod-primary", "public_key": "abc" } ] },
    "manifest_trust_fingerprint": "fingerprint-1"
  }
}`)
