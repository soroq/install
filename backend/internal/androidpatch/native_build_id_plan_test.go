package androidpatch

import (
	"bytes"
	"debug/elf"
	"os"
	"path/filepath"
	"strings"
	"testing"

	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/nativeelf"
)

// PLAN-LEVEL tests for build-id-only native drift.
//
// The ELF normaliser itself lives in internal/nativeelf and is unit-tested
// there. What these prove is different and is the part that actually protects a
// user: that the normaliser is WIRED INTO PrepareCodePatchPlan, the function
// `soroq patch android --code` calls (cmd/soroq/patch_cmd.go), and that the
// guard around it still refuses real native drift.
//
// That distinction is not academic. The first version of this fix landed only
// on the internal soroqctl planner while the shipped command carried its own
// untouched copy of the comparison, and every unit test passed the whole time.

// Fixtures are shared with the nativeelf unit tests rather than duplicated. Two
// copies of the same pair of .so files would drift apart, and a fixture that no
// longer matches the one the implementation is tested against proves nothing.
const nativeBuildIDFixtureRoot = "../nativeelf/testdata/native_build_id"

var nativeBuildIDFixtureABIs = []string{"arm64-v8a", "armeabi-v7a", "x86_64"}

func readNativeBuildIDFixture(t *testing.T, abi string, variant string) []byte {
	t.Helper()

	path := filepath.Join(nativeBuildIDFixtureRoot, abi, "buildid_"+variant+".so")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read native build-id fixture %q: %v", path, err)
	}
	return raw
}

func sectionFileRange(t *testing.T, raw []byte, name string) (uint64, uint64) {
	t.Helper()

	file, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse ELF: %v", err)
	}
	section := file.Section(name)
	if section == nil {
		t.Fatalf("section %q not found", name)
	}
	return section.Offset, section.Size
}

func flipByteAt(raw []byte, offset uint64) []byte {
	out := make([]byte, len(raw))
	copy(out, raw)
	out[offset] ^= 0xff
	return out
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
	if !nativeelf.NativeLibrariesMatchIgnoringBuildID(base, candidate) {
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
