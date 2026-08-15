package nativeelf

import (
	"bytes"
	"debug/elf"
	"os"
	"path/filepath"
	"testing"
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

		baseDigest, baseOK := ComparisonDigest(base)
		candidateDigest, candidateOK := ComparisonDigest(candidate)
		if !baseOK || !candidateOK {
			t.Fatalf("%s: ComparisonDigest() failed to parse a real NDK library", abi)
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

		baseDigest, baseOK := ComparisonDigest(base)
		mutatedDigest, mutatedOK := ComparisonDigest(mutated)
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

		baseDigest, baseOK := ComparisonDigest(base)
		mutatedDigest, mutatedOK := ComparisonDigest(mutated)
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

	baseDigest, baseOK := ComparisonDigest(base)
	mutatedDigest, mutatedOK := ComparisonDigest(mutated)
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
		if _, ok := ComparisonDigest(raw); ok {
			t.Fatalf("%s: ComparisonDigest() reported a usable digest for unparseable input", name)
		}
	}
}

// TestNativeComparisonDigestFailsClosedOnCorruptNoteRegion covers an ELF whose
// headers parse but whose note walk cannot complete: namesz is inflated so the
// first note overruns its region.
func TestNativeComparisonDigestFailsClosedOnCorruptNoteRegion(t *testing.T) {
	t.Parallel()

	raw := readNativeBuildIDFixture(t, "arm64-v8a", "a")
	if _, ok := ComparisonDigest(raw); !ok {
		t.Fatalf("expected the unmodified fixture to parse")
	}

	identOffset, _ := sectionFileRange(t, raw, ".note.android.ident")
	corrupt := make([]byte, len(raw))
	copy(corrupt, raw)
	// Inflate namesz of the first note so the walk runs past the region end.
	corrupt[identOffset] = 0xff
	corrupt[identOffset+1] = 0xff

	if _, ok := ComparisonDigest(corrupt); ok {
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

		digest, ok := ComparisonDigest(raw)
		if !ok {
			t.Fatalf("%s: expected a digest for the shipped library", abi)
		}
		if digest != sha256Hex(raw) {
			t.Fatalf("%s: normalisation changed a library that has no build-id note", abi)
		}

		textOffset, textSize := sectionFileRange(t, raw, ".text")
		mutated := flipByteAt(raw, textOffset+textSize/2)
		mutatedDigest, ok := ComparisonDigest(mutated)
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
