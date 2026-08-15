package androidpatch

import (
	"bytes"
	"debug/elf"
	"os"
	"path/filepath"
	"strings"
	"testing"

	androidrelease "soroq/backend/internal/androidrelease"
)

// Fixtures: real Android NDK shared objects, one pair per shipped ABI, linked
// from identical sources with two different explicit GNU build-ids. See
// testdata/native_build_id/README.md for the exact link commands.
//
// They live here, next to the single implementation, because both code-patch
// planners share it: the shipped `soroq patch android --code` path in this
// package and the internal soroqctl planner, whose plan-level tests reach the
// fixtures through a relative path.
const nativeBuildIDFixtureRoot = "testdata/native_build_id"

// The shipped runtime libraries carry .note.android.ident but no
// NT_GNU_BUILD_ID note. They are the "no build-id" control.
const shippedRuntimeJNIRoot = "../../../packages/soroq_flutter/android/src/main/jniLibs"

var nativeBuildIDFixtureABIs = []string{"arm64-v8a", "armeabi-v7a", "x86_64"}

// expectedBuildIDDescriptorOffsets are the file offsets at which the linker
// placed the build-id descriptor in each fixture. They are recorded here
// because they are the reason this code parses ELF notes instead of masking a
// byte range: the offsets differ by ABI, and they are exactly the offsets the
// A3 evidence reported for libdartjni.so.
var expectedBuildIDDescriptorOffsets = map[string]uint64{
	"arm64-v8a":   0x2e0,
	"armeabi-v7a": 0x1fc,
	"x86_64":      0x2e0,
}

func readNativeBuildIDFixture(t *testing.T, abi string, variant string) []byte {
	t.Helper()

	path := filepath.Join(nativeBuildIDFixtureRoot, abi, "buildid_"+variant+".so")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read native build-id fixture %q: %v", path, err)
	}
	return raw
}

// readShippedRuntimeJNI loads a REAL shipped runtime library. Those live outside this Go module, in
// the Flutter package, and that is deliberate: an assertion about normalisation is only worth making
// against the artefact we actually ship, not against a synthetic fixture built to satisfy it.
//
// It also means the file is absent in the public CLI mirror, where the Flutter package does not
// exist — so the export's standalone `go test` failed with a bare "no such file or directory".
//
// Skip on ABSENCE only. A blanket `if err != nil { t.Skip() }` would convert a corrupt, truncated or
// unreadable artefact — precisely the thing worth catching — into a silent pass. os.IsNotExist is the
// one condition that actually means "different repository". TestShippedRuntimeLibrariesArePresent
// below then makes sure this skip can never fire unnoticed in the monorepo.
func readShippedRuntimeJNI(t *testing.T, abi string) []byte {
	t.Helper()

	path := filepath.Join(shippedRuntimeJNIRoot, abi, "libsoroq_runtime_jni.so")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("shipped runtime library %q is absent, so this package is being tested outside the "+
			"monorepo (the public CLI mirror carries no Flutter package)", path)
	}
	if err != nil {
		t.Fatalf("read shipped runtime jni library %q: %v", path, err)
	}
	return raw
}

// TestShippedRuntimeLibrariesArePresent is the CONTROL for the skip above.
//
// Without it, deleting or moving the shipped libraries would turn every real-artefact assertion in
// this file into a green skip, and the suite would still report success. A skip that nobody can
// observe is indistinguishable from coverage that was never there.
//
// It anchors on a monorepo marker rather than on the libraries themselves — otherwise it would skip
// for exactly the same reason it is meant to detect.
func TestShippedRuntimeLibrariesArePresent(t *testing.T) {
	t.Parallel()

	marker := filepath.Join("..", "..", "..", "packages", "soroq_flutter", "pubspec.yaml")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Skip("not the monorepo (no packages/soroq_flutter): the real-artefact skips are expected here")
	}

	for _, abi := range nativeBuildIDFixtureABIs {
		path := filepath.Join(shippedRuntimeJNIRoot, abi, "libsoroq_runtime_jni.so")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the monorepo is missing %s, so every real-artefact assertion in this file is "+
				"silently skipping rather than running: %v", path, err)
		}
	}
}

func sectionFileRange(t *testing.T, raw []byte, name string) (uint64, uint64) {
	t.Helper()

	file, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse ELF: %v", err)
	}
	section := file.Section(name)
	if section == nil {
		t.Fatalf("expected section %q to be present", name)
	}
	if section.Type == elf.SHT_NOBITS || section.Size == 0 {
		t.Fatalf("section %q occupies no file bytes", name)
	}
	return section.Offset, section.Size
}

func flipByteAt(raw []byte, offset uint64) []byte {
	mutated := make([]byte, len(raw))
	copy(mutated, raw)
	mutated[offset] ^= 0xff
	return mutated
}

func differingByteOffsets(left []byte, right []byte) []uint64 {
	if len(left) != len(right) {
		return nil
	}
	offsets := make([]uint64, 0, 32)
	for index := range left {
		if left[index] != right[index] {
			offsets = append(offsets, uint64(index))
		}
	}
	return offsets
}

// TestNativeBuildIDFixturesDifferOnlyInBuildIDDescriptor pins the property the
// rest of this file relies on: each fixture pair differs in exactly the twenty
// bytes of the build-id descriptor, at an offset the parser discovers, and
// those offsets are NOT the same across ABIs.
func TestNativeBuildIDFixturesDifferOnlyInBuildIDDescriptor(t *testing.T) {
	t.Parallel()

	observed := make(map[string]uint64, len(nativeBuildIDFixtureABIs))
	for _, abi := range nativeBuildIDFixtureABIs {
		base := readNativeBuildIDFixture(t, abi, "a")
		candidate := readNativeBuildIDFixture(t, abi, "b")

		descriptors, ok := gnuBuildIDDescriptorRanges(base)
		if !ok {
			t.Fatalf("%s: gnuBuildIDDescriptorRanges() reported the fixture unparseable", abi)
		}
		if len(descriptors) != 1 {
			t.Fatalf("%s: expected exactly 1 build-id descriptor, got %d", abi, len(descriptors))
		}
		if got, want := descriptors[0].offset, expectedBuildIDDescriptorOffsets[abi]; got != want {
			t.Fatalf("%s: build-id descriptor offset = %#x, want %#x", abi, got, want)
		}
		if descriptors[0].size != 20 {
			t.Fatalf("%s: build-id descriptor size = %d, want 20", abi, descriptors[0].size)
		}
		observed[abi] = descriptors[0].offset

		diffs := differingByteOffsets(base, candidate)
		if len(diffs) != 20 {
			t.Fatalf("%s: fixtures differ in %d bytes, want exactly 20", abi, len(diffs))
		}
		for _, offset := range diffs {
			if offset < descriptors[0].offset || offset >= descriptors[0].offset+descriptors[0].size {
				t.Fatalf("%s: fixtures differ at %#x, outside the build-id descriptor", abi, offset)
			}
		}
	}

	if observed["arm64-v8a"] == observed["armeabi-v7a"] {
		t.Fatalf("fixtures do not exercise ABI-varying offsets: both at %#x", observed["arm64-v8a"])
	}
}

// TestNativeComparisonDigestIgnoresGNUBuildID is the positive case: libraries
// that differ only in their linker-assigned build-id compare as equal.
func TestNativeComparisonDigestIgnoresGNUBuildID(t *testing.T) {
	t.Parallel()

	for _, abi := range nativeBuildIDFixtureABIs {
		base := readNativeBuildIDFixture(t, abi, "a")
		candidate := readNativeBuildIDFixture(t, abi, "b")

		if sha256Hex(base) == sha256Hex(candidate) {
			t.Fatalf("%s: fixtures are byte-identical, the test would prove nothing", abi)
		}

		baseDigest, baseOK := nativeComparisonDigest(base)
		candidateDigest, candidateOK := nativeComparisonDigest(candidate)
		if !baseOK || !candidateOK {
			t.Fatalf("%s: nativeComparisonDigest() failed to parse a real NDK library", abi)
		}
		if baseDigest != candidateDigest {
			t.Fatalf("%s: build-id-only difference was reported as drift", abi)
		}
	}
}

// TestNativeComparisonDigestBlocksCodeChangeWithDifferentBuildID is the
// negative control that matters: the code differs AND the build-id differs.
// An implementation that ignores too much passes the positive test and fails
// this one.
func TestNativeComparisonDigestBlocksCodeChangeWithDifferentBuildID(t *testing.T) {
	t.Parallel()

	for _, abi := range nativeBuildIDFixtureABIs {
		base := readNativeBuildIDFixture(t, abi, "a")
		candidate := readNativeBuildIDFixture(t, abi, "b")

		textOffset, textSize := sectionFileRange(t, candidate, ".text")
		mutated := flipByteAt(candidate, textOffset+textSize/2)

		baseDigest, baseOK := nativeComparisonDigest(base)
		mutatedDigest, mutatedOK := nativeComparisonDigest(mutated)
		if !baseOK || !mutatedOK {
			t.Fatalf("%s: expected both libraries to parse", abi)
		}
		if baseDigest == mutatedDigest {
			t.Fatalf("%s: a byte changed inside .text was masked by build-id normalisation", abi)
		}
	}
}

// TestNativeComparisonDigestBlocksOtherNoteChanges keeps the normalisation
// narrow. .note.android.ident lives in the same SHT_NOTE/PT_NOTE regions as
// the build-id note, so an implementation that zeroes note regions wholesale
// would still pass the .text control above but fails here.
func TestNativeComparisonDigestBlocksOtherNoteChanges(t *testing.T) {
	t.Parallel()

	for _, abi := range nativeBuildIDFixtureABIs {
		base := readNativeBuildIDFixture(t, abi, "a")
		candidate := readNativeBuildIDFixture(t, abi, "b")

		// namesz(4) + descsz(4) + type(4) + "Android\0" padded to 8 = 20.
		identOffset, identSize := sectionFileRange(t, candidate, ".note.android.ident")
		descriptorOffset := identOffset + 20
		if descriptorOffset >= identOffset+identSize {
			t.Fatalf("%s: .note.android.ident is too small to hold a descriptor", abi)
		}
		mutated := flipByteAt(candidate, descriptorOffset)

		baseDigest, baseOK := nativeComparisonDigest(base)
		mutatedDigest, mutatedOK := nativeComparisonDigest(mutated)
		if !baseOK || !mutatedOK {
			t.Fatalf("%s: expected both libraries to parse", abi)
		}
		if baseDigest == mutatedDigest {
			t.Fatalf("%s: a change inside .note.android.ident was masked", abi)
		}
	}
}

// TestNativeComparisonDigestBlocksBuildIDNoteHeaderChanges shows that only the
// descriptor payload is normalised. The note header is compared, so a
// descriptor that changes length changes the compared bytes.
func TestNativeComparisonDigestBlocksBuildIDNoteHeaderChanges(t *testing.T) {
	t.Parallel()

	abi := "arm64-v8a"
	base := readNativeBuildIDFixture(t, abi, "a")
	descriptors, ok := gnuBuildIDDescriptorRanges(base)
	if !ok || len(descriptors) != 1 {
		t.Fatalf("expected exactly one parseable build-id descriptor")
	}

	// descsz sits 16 bytes before the descriptor: header(12) + "GNU\0"(4).
	mutated := flipByteAt(base, descriptors[0].offset-16)

	baseDigest, baseOK := nativeComparisonDigest(base)
	mutatedDigest, mutatedOK := nativeComparisonDigest(mutated)
	if !baseOK {
		t.Fatalf("expected the base fixture to parse")
	}
	if mutatedOK && baseDigest == mutatedDigest {
		t.Fatalf("a change to the build-id note header was masked")
	}
}

// TestNativeComparisonDigestFailsClosedOnUnparseableELF: unparseable input has
// no normalised form, so callers must keep the strict raw verdict.
func TestNativeComparisonDigestFailsClosedOnUnparseableELF(t *testing.T) {
	t.Parallel()

	real := readNativeBuildIDFixture(t, "arm64-v8a", "a")
	cases := map[string][]byte{
		"empty":            {},
		"not_elf":          []byte("this is not an ELF file at all, not even close"),
		"header_only":      real[:64],
		"truncated_middle": real[:len(real)/2],
		"truncated_by_one": real[:len(real)-1],
	}

	for name, raw := range cases {
		if _, ok := nativeComparisonDigest(raw); ok {
			t.Fatalf("%s: nativeComparisonDigest() reported a usable digest for unparseable input", name)
		}
	}
}

// TestNativeComparisonDigestFailsClosedOnCorruptNoteRegion covers an ELF whose
// headers parse but whose note walk cannot complete: namesz is inflated so the
// first note overruns its region.
func TestNativeComparisonDigestFailsClosedOnCorruptNoteRegion(t *testing.T) {
	t.Parallel()

	raw := readNativeBuildIDFixture(t, "arm64-v8a", "a")
	if _, ok := nativeComparisonDigest(raw); !ok {
		t.Fatalf("expected the unmodified fixture to parse")
	}

	identOffset, _ := sectionFileRange(t, raw, ".note.android.ident")
	corrupt := make([]byte, len(raw))
	copy(corrupt, raw)
	// Inflate namesz of the first note so the walk runs past the region end.
	corrupt[identOffset] = 0xff
	corrupt[identOffset+1] = 0xff

	if _, ok := nativeComparisonDigest(corrupt); ok {
		t.Fatalf("expected a corrupt note region to fail closed")
	}
}

// TestNativeComparisonDigestIsIdentityWithoutBuildIDNote uses the real shipped
// runtime libraries, which have no NT_GNU_BUILD_ID note. Normalisation must be
// a no-op, and content differences must still be visible.
func TestNativeComparisonDigestIsIdentityWithoutBuildIDNote(t *testing.T) {
	t.Parallel()

	for _, abi := range nativeBuildIDFixtureABIs {
		raw := readShippedRuntimeJNI(t, abi)

		descriptors, ok := gnuBuildIDDescriptorRanges(raw)
		if !ok {
			t.Fatalf("%s: expected the shipped library to parse", abi)
		}
		if len(descriptors) != 0 {
			t.Fatalf("%s: expected no build-id note, found %d", abi, len(descriptors))
		}

		digest, ok := nativeComparisonDigest(raw)
		if !ok {
			t.Fatalf("%s: expected a digest for the shipped library", abi)
		}
		if digest != sha256Hex(raw) {
			t.Fatalf("%s: normalisation changed a library that has no build-id note", abi)
		}

		textOffset, textSize := sectionFileRange(t, raw, ".text")
		mutated := flipByteAt(raw, textOffset+textSize/2)
		mutatedDigest, ok := nativeComparisonDigest(mutated)
		if !ok {
			t.Fatalf("%s: expected the mutated library to parse", abi)
		}
		if mutatedDigest == digest {
			t.Fatalf("%s: a .text change was masked in a library with no build-id note", abi)
		}
	}
}

func TestNativeLibrariesMatchIgnoringBuildIDRejectsSizeMismatch(t *testing.T) {
	t.Parallel()

	base := readNativeBuildIDFixture(t, "arm64-v8a", "a")
	candidate := readNativeBuildIDFixture(t, "arm64-v8a", "b")

	if !NativeLibrariesMatchIgnoringBuildID(base, candidate) {
		t.Fatalf("expected build-id-only fixtures to match")
	}

	padded := append(append([]byte(nil), candidate...), 0x00)
	if NativeLibrariesMatchIgnoringBuildID(base, padded) {
		t.Fatalf("expected differing sizes to be reported as drift")
	}
}

// --- shipped-path plan behaviour ---------------------------------------------
//
// Everything below runs through PrepareCodePatchPlan, which is what
// `soroq patch android --code` calls (cmd/soroq/patch_cmd.go). The unit tests
// above prove the helper; these prove the helper is actually wired into the
// command real users run, and that the guard around it still refuses.

func writeBuildIDPlanArtifacts(
	t *testing.T,
	tempDir string,
	baseNative map[string][]byte,
	candidateNative map[string][]byte,
) (string, string) {
	t.Helper()

	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "1.2.3+45")
	baseFiles := map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "base-libapp-arm64",
	}
	candidateFiles := map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         "candidate-libapp-arm64",
	}
	for path, raw := range baseNative {
		baseFiles[path] = string(raw)
	}
	for path, raw := range candidateNative {
		candidateFiles[path] = string(raw)
	}

	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	writeArtifactZip(t, baseArtifactPath, baseFiles)
	writeArtifactZip(t, candidateArtifactPath, candidateFiles)
	return baseArtifactPath, candidateArtifactPath
}

func prepareBuildIDCodePatchPlan(
	t *testing.T,
	tempDir string,
	baseArtifactPath string,
	candidateArtifactPath string,
) *CodePatchPlan {
	t.Helper()

	baseSnapshot, err := androidrelease.CaptureSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("androidrelease.CaptureSnapshot(base) error = %v", err)
	}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeJSONFile(t, baseSnapshotPath, baseSnapshot)

	plan, err := PrepareCodePatchPlan(CodePatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: candidateArtifactPath,
		ReleaseID:             "release-android-1",
		WorkspaceOut:          filepath.Join(tempDir, "workspace"),
	})
	if err != nil {
		t.Fatalf("PrepareCodePatchPlan() error = %v", err)
	}
	return plan
}

func codePlanHasBlocker(plan *CodePatchPlan, id string, path string) bool {
	for _, blocker := range plan.Blockers {
		if blocker.ID == id && blocker.Path == path {
			return true
		}
	}
	return false
}

// TestPrepareCodePatchPlanAcceptsBuildIDOnlyNativeDrift reproduces the A3
// refusal on the SHIPPED path: libdartjni.so present in both artifacts,
// differing only in its build-id because base and candidate were compiled at
// different paths. Before the shared helper was wired in here, this refused.
func TestPrepareCodePatchPlanAcceptsBuildIDOnlyNativeDrift(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseNative := map[string][]byte{}
	candidateNative := map[string][]byte{}
	for _, abi := range nativeBuildIDFixtureABIs {
		baseNative["lib/"+abi+"/libdartjni.so"] = readNativeBuildIDFixture(t, abi, "a")
		candidateNative["lib/"+abi+"/libdartjni.so"] = readNativeBuildIDFixture(t, abi, "b")
	}

	baseArtifactPath, candidateArtifactPath := writeBuildIDPlanArtifacts(t, tempDir, baseNative, candidateNative)
	plan := prepareBuildIDCodePatchPlan(t, tempDir, baseArtifactPath, candidateArtifactPath)

	if !plan.Ready {
		t.Fatalf("expected plan to be ready, blockers = %#v", plan.Blockers)
	}
	if len(plan.Blockers) != 0 {
		t.Fatalf("expected no blockers, got %#v", plan.Blockers)
	}
	if len(plan.CodePayloads) != 1 {
		t.Fatalf("expected the libapp payload to be extracted, got %#v", plan.CodePayloads)
	}

	joinedNotes := strings.Join(plan.Notes, "\n")
	for _, abi := range nativeBuildIDFixtureABIs {
		want := "native library lib/" + abi + "/libdartjni.so differs only in its GNU build-id note"
		if !strings.Contains(joinedNotes, want) {
			t.Fatalf("expected the plan to record the tolerated %s drift, notes = %q", abi, joinedNotes)
		}
	}
	if !strings.Contains(joinedNotes, "ignoring 3 GNU build-id-only difference(s)") {
		t.Fatalf("expected the summary note to count the ignored differences, notes = %q", joinedNotes)
	}
}

// TestPrepareCodePatchPlanBlocksNativeCodeDrift is the plan-level negative
// control on the shipped path: the same build-id difference, plus one byte
// changed inside .text, must still refuse. This is the test that proves the
// guard survives the move -- a helper that ignores too much fails here.
func TestPrepareCodePatchPlanBlocksNativeCodeDrift(t *testing.T) {
	t.Parallel()

	for _, abi := range nativeBuildIDFixtureABIs {
		tempDir := t.TempDir()
		candidate := readNativeBuildIDFixture(t, abi, "b")
		textOffset, textSize := sectionFileRange(t, candidate, ".text")

		baseArtifactPath, candidateArtifactPath := writeBuildIDPlanArtifacts(
			t,
			tempDir,
			map[string][]byte{"lib/" + abi + "/libdartjni.so": readNativeBuildIDFixture(t, abi, "a")},
			map[string][]byte{"lib/" + abi + "/libdartjni.so": flipByteAt(candidate, textOffset+textSize/2)},
		)
		plan := prepareBuildIDCodePatchPlan(t, tempDir, baseArtifactPath, candidateArtifactPath)

		if plan.Ready {
			t.Fatalf("%s: expected changed native code to block the plan", abi)
		}
		if !codePlanHasBlocker(plan, "unsupported_native_drift", "lib/"+abi+"/libdartjni.so") {
			t.Fatalf("%s: expected unsupported_native_drift, got %#v", abi, plan.Blockers)
		}
	}
}

// TestPrepareCodePatchPlanBlocksUnparseableNativeDrift: a truncated library
// cannot be normalised, so the strict raw comparison stands.
func TestPrepareCodePatchPlanBlocksUnparseableNativeDrift(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	base := readNativeBuildIDFixture(t, "arm64-v8a", "a")
	truncated := make([]byte, len(base))
	copy(truncated, base)
	truncated = truncated[:len(truncated)-64]
	truncated = append(truncated, make([]byte, 64)...)

	baseArtifactPath, candidateArtifactPath := writeBuildIDPlanArtifacts(
		t,
		tempDir,
		map[string][]byte{"lib/arm64-v8a/libdartjni.so": base},
		map[string][]byte{"lib/arm64-v8a/libdartjni.so": truncated},
	)
	plan := prepareBuildIDCodePatchPlan(t, tempDir, baseArtifactPath, candidateArtifactPath)

	if plan.Ready {
		t.Fatalf("expected an unparseable native library to block the plan")
	}
	if !codePlanHasBlocker(plan, "unsupported_native_drift", "lib/arm64-v8a/libdartjni.so") {
		t.Fatalf("expected unsupported_native_drift, got %#v", plan.Blockers)
	}
}

// TestPrepareCodePatchPlanBlocksNonELFNativeDrift keeps the pre-existing
// behaviour for native entries that were never ELF at all.
func TestPrepareCodePatchPlanBlocksNonELFNativeDrift(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath, candidateArtifactPath := writeBuildIDPlanArtifacts(
		t,
		tempDir,
		map[string][]byte{"lib/arm64-v8a/libflutter.so": []byte("base-flutter")},
		map[string][]byte{"lib/arm64-v8a/libflutter.so": []byte("cand-flutter")},
	)
	plan := prepareBuildIDCodePatchPlan(t, tempDir, baseArtifactPath, candidateArtifactPath)

	if plan.Ready {
		t.Fatalf("expected non-ELF native drift to block the plan")
	}
	if !codePlanHasBlocker(plan, "unsupported_native_drift", "lib/arm64-v8a/libflutter.so") {
		t.Fatalf("expected unsupported_native_drift, got %#v", plan.Blockers)
	}
}

// TestPrepareCodePatchPlanKeepsLibappStrict pins the boundary of the
// normalisation: libapp.so is the payload being replaced, not a guard, so a
// build-id-only difference in it must still be extracted as a payload rather
// than dismissed as "unchanged". Structurally the check sits inside
// !isLibappPath, and this is what holds that structure in place.
func TestPrepareCodePatchPlanKeepsLibappStrict(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	base := readNativeBuildIDFixture(t, "arm64-v8a", "a")
	candidate := readNativeBuildIDFixture(t, "arm64-v8a", "b")
	if !NativeLibrariesMatchIgnoringBuildID(base, candidate) {
		t.Fatalf("fixtures must differ only in the build-id for this test to mean anything")
	}

	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "1.2.3+45")
	baseArtifactPath := filepath.Join(tempDir, "base.apk")
	candidateArtifactPath := filepath.Join(tempDir, "candidate.apk")
	writeArtifactZip(t, baseArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         string(base),
	})
	writeArtifactZip(t, candidateArtifactPath, map[string]string{
		"assets/flutter_assets/soroq/soroq_metadata.json": metadata,
		"lib/arm64-v8a/libapp.so":                         string(candidate),
	})

	plan := prepareBuildIDCodePatchPlan(t, tempDir, baseArtifactPath, candidateArtifactPath)

	if !plan.Ready {
		t.Fatalf("expected the plan to be ready, blockers = %#v", plan.Blockers)
	}
	if len(plan.CodePayloads) != 1 {
		t.Fatalf("expected libapp.so to still be extracted as a payload, got %#v", plan.CodePayloads)
	}
	if plan.CodePayloads[0].BaseSHA256 == plan.CodePayloads[0].CandidateSHA256 {
		t.Fatalf("expected the libapp payload to keep its exact raw digests")
	}
	if got, want := plan.CodePayloads[0].BaseSHA256, sha256Hex(base); got != want {
		t.Fatalf("libapp base digest = %q, want the raw digest %q", got, want)
	}
	if got, want := plan.CodePayloads[0].CandidateSHA256, sha256Hex(candidate); got != want {
		t.Fatalf("libapp candidate digest = %q, want the raw digest %q", got, want)
	}
	for _, note := range plan.Notes {
		if strings.Contains(note, "libapp.so differs only in its GNU build-id note") {
			t.Fatalf("libapp.so must never be normalised away, notes = %q", plan.Notes)
		}
	}
}
