package androidpatch

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
)

// A linker stamps every ELF it produces with an NT_GNU_BUILD_ID note. The
// descriptor of that note is derived from the link inputs *and* from build
// configuration that has nothing to do with the emitted code: AGP, for
// instance, feeds the CMake output directory into the configuration hash, so
// building the same sources from two different directories yields two
// libraries whose only difference is those few bytes.
//
// A build-id is therefore not a code change, and refusing an OTA code patch
// because of one refuses a patch that is in fact deliverable. Everything else
// in the file still has to match exactly: a native library whose code really
// changed cannot be delivered by a patch that only replaces Dart code.
const (
	// NT_GNU_BUILD_ID. Not exported by debug/elf.
	gnuBuildIDNoteType uint32 = 3

	// ELF note fields are 4-byte aligned in every producer that matters here
	// (Android LLD, GNU ld, gold and the Go linker all emit 4-byte alignment
	// in 32-bit and 64-bit objects alike; the fixtures and the shipped
	// jniLibs all report Addralign=4). Deriving the value from sh_addralign
	// and then overriding it in most cases would be less honest than a
	// constant. A note region that really used 8-byte alignment would
	// mis-step the walk, trip the bounds guard below, and fall back to the
	// strict raw comparison -- fail closed, never open.
	elfNoteAlignment uint64 = 4

	// Size of the fixed namesz/descsz/type header that precedes every note.
	elfNoteHeaderSize uint64 = 12
)

// gnuNoteName is the NUL-terminated owner string of a GNU note; namesz counts
// the terminator, so it is exactly 4 for "GNU".
var gnuNoteName = []byte("GNU\x00")

// normalizeNativeLibraryForComparison returns a copy of raw in which the
// descriptor of every NT_GNU_BUILD_ID note has been zeroed, and nothing else
// has been touched.
//
// Deliberately NOT normalised:
//   - the 12-byte note header (namesz, descsz, type) and the owner name bytes,
//     so a descriptor that changes length still changes the compared bytes;
//   - every other note, including .note.android.ident;
//   - every section and every byte of code and data.
//
// The second return value is false when the input cannot be parsed and walked
// as an ELF file. Callers must treat that as "cannot normalise" and fall back
// to the strict raw comparison; an unparseable library must never be treated
// as unchanged.
func normalizeNativeLibraryForComparison(raw []byte) ([]byte, bool) {
	descriptors, ok := gnuBuildIDDescriptorRanges(raw)
	if !ok {
		return nil, false
	}

	normalized := make([]byte, len(raw))
	copy(normalized, raw)
	for _, descriptor := range descriptors {
		for offset := descriptor.offset; offset < descriptor.offset+descriptor.size; offset++ {
			normalized[offset] = 0
		}
	}
	return normalized, true
}

// nativeComparisonDigest hashes a native library with the GNU build-id note
// descriptor normalised away. It reports false when the library could not be
// parsed, in which case there is no digest and the caller must stay strict.
func nativeComparisonDigest(raw []byte) (string, bool) {
	normalized, ok := normalizeNativeLibraryForComparison(raw)
	if !ok {
		return "", false
	}
	return sha256Hex(normalized), true
}

// NativeLibrariesMatchIgnoringBuildID reports whether two native libraries are
// identical once the GNU build-id note descriptor is normalised.
//
// This is the single implementation shared by both code-patch planners: the
// shipped `soroq patch android --code` path (PrepareCodePatchPlan, below) and
// the internal soroqctl planner. A second copy would drift, and a drifting copy
// is how the defect this fixes reached users on one path and not the other.
//
// It fails closed in every direction: differing sizes, an unparseable ELF on
// either side, or any surviving byte difference all report false, which leaves
// the caller's strict verdict standing.
func NativeLibrariesMatchIgnoringBuildID(base []byte, candidate []byte) bool {
	if len(base) != len(candidate) {
		return false
	}
	baseDigest, baseOK := nativeComparisonDigest(base)
	if !baseOK {
		return false
	}
	candidateDigest, candidateOK := nativeComparisonDigest(candidate)
	if !candidateOK {
		return false
	}
	return baseDigest == candidateDigest
}

// buildIDNoteDescriptor is the file range covered by one build-id descriptor.
type buildIDNoteDescriptor struct {
	offset uint64
	size   uint64
}

// gnuBuildIDDescriptorRanges locates every NT_GNU_BUILD_ID descriptor in raw by
// parsing the ELF note regions properly.
//
// Both SHT_NOTE sections and PT_NOTE segments are scanned because neither view
// is guaranteed to cover the note: in the Android NDK fixtures PT_NOTE spans
// .note.gnu.build-id, while in a Go-linked ELF PT_NOTE covers only
// .note.go.buildid and the build-id note is reachable through the section
// table alone. The two views usually describe the same note, so identical
// ranges are reported once.
func gnuBuildIDDescriptorRanges(raw []byte) ([]buildIDNoteDescriptor, bool) {
	file, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}

	type noteRegion struct {
		offset uint64
		size   uint64
	}
	regions := make([]noteRegion, 0, len(file.Sections)+len(file.Progs))
	for _, section := range file.Sections {
		if section.Type != elf.SHT_NOTE || section.Size == 0 {
			continue
		}
		regions = append(regions, noteRegion{offset: section.Offset, size: section.Size})
	}
	for _, prog := range file.Progs {
		if prog.Type != elf.PT_NOTE || prog.Filesz == 0 {
			continue
		}
		regions = append(regions, noteRegion{offset: prog.Off, size: prog.Filesz})
	}

	descriptors := make([]buildIDNoteDescriptor, 0, 1)
	seen := make(map[buildIDNoteDescriptor]struct{}, 1)
	for _, region := range regions {
		found, ok := gnuBuildIDDescriptorsInRegion(raw, region.offset, region.size, file.ByteOrder)
		if !ok {
			return nil, false
		}
		for _, descriptor := range found {
			if _, duplicate := seen[descriptor]; duplicate {
				continue
			}
			seen[descriptor] = struct{}{}
			descriptors = append(descriptors, descriptor)
		}
	}
	return descriptors, true
}

// gnuBuildIDDescriptorsInRegion walks one note region. Any malformation -- a
// region outside the file, a note that overruns the region, a trailing stub too
// short to be a note header -- reports false so the caller stays strict.
func gnuBuildIDDescriptorsInRegion(
	raw []byte,
	offset uint64,
	size uint64,
	order binary.ByteOrder,
) ([]buildIDNoteDescriptor, bool) {
	total := uint64(len(raw))
	if offset > total || size > total-offset {
		return nil, false
	}

	descriptors := make([]buildIDNoteDescriptor, 0, 1)
	end := offset + size
	for cursor := offset; cursor < end; {
		if end-cursor < elfNoteHeaderSize {
			return nil, false
		}
		nameSize := uint64(order.Uint32(raw[cursor : cursor+4]))
		descriptorSize := uint64(order.Uint32(raw[cursor+4 : cursor+8]))
		noteType := order.Uint32(raw[cursor+8 : cursor+12])

		nameOffset := cursor + elfNoteHeaderSize
		descriptorOffset := nameOffset + alignUpNoteField(nameSize)
		next := descriptorOffset + alignUpNoteField(descriptorSize)
		if descriptorOffset < nameOffset || next < descriptorOffset || next > end {
			return nil, false
		}

		if noteType == gnuBuildIDNoteType &&
			nameSize == uint64(len(gnuNoteName)) &&
			bytes.Equal(raw[nameOffset:nameOffset+nameSize], gnuNoteName) {
			descriptors = append(descriptors, buildIDNoteDescriptor{
				offset: descriptorOffset,
				size:   descriptorSize,
			})
		}
		cursor = next
	}
	return descriptors, true
}

// alignUpNoteField rounds a note name or descriptor length up to the note
// alignment. Saturating on overflow keeps the caller's bounds guard in charge.
func alignUpNoteField(length uint64) uint64 {
	aligned := length + (elfNoteAlignment - 1)
	if aligned < length {
		return ^uint64(0)
	}
	return aligned &^ (elfNoteAlignment - 1)
}

// BuildIDNormalizedDriftNote records, in the plan, that a native library was
// treated as unchanged despite differing raw bytes.
func BuildIDNormalizedDriftNote(path string, baseSHA256 string, candidateSHA256 string) string {
	return fmt.Sprintf(
		"native library %s differs only in its GNU build-id note and was treated as unchanged (base sha256=%s, candidate sha256=%s)",
		path,
		baseSHA256,
		candidateSHA256,
	)
}
