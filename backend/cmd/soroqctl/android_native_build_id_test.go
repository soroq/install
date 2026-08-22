package main

import (
	"bytes"
	"debug/elf"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The GNU build-id normalisation itself lives in
// backend/internal/androidpatch/native_build_id.go and is unit-tested there,
// next to the fixtures. What is left here is the soroqctl planner's own
// behaviour: that it calls the shared helper, and that its guard still refuses
// real native drift.
//
// Fixtures are shared with that package rather than duplicated: real Android
// NDK shared objects, one pair per shipped ABI, linked from identical sources
// with two different explicit GNU build-ids. See the directory's README.md.
const nativeBuildIDFixtureRoot = "../../internal/nativeelf/testdata/native_build_id"

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

func writeBuildIDPlanArtifacts(
	t *testing.T,
	tempDir string,
	baseNative map[string][]byte,
	candidateNative map[string][]byte,
) (string, string) {
	t.Helper()

	metadata := testBundledMetadataJSON("com.example.soroq", "stable", "runtime-123", "manual", "")
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
	writeTestAndroidArtifact(t, baseArtifactPath, baseFiles)
	writeTestAndroidArtifact(t, candidateArtifactPath, candidateFiles)
	return baseArtifactPath, candidateArtifactPath
}

func prepareBuildIDPlan(
	t *testing.T,
	tempDir string,
	baseArtifactPath string,
	candidateArtifactPath string,
) *androidCodePatchPlan {
	t.Helper()

	baseSnapshot, err := captureAndroidReleaseSnapshot(baseArtifactPath)
	if err != nil {
		t.Fatalf("captureAndroidReleaseSnapshot(base) error = %v", err)
	}
	baseSnapshotPath := filepath.Join(tempDir, "base.json")
	writeTestJSONFile(t, baseSnapshotPath, baseSnapshot)

	plan, err := prepareAndroidCodePatchPlan(androidCodePatchPlanOptions{
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateArtifactPath: candidateArtifactPath,
		ReleaseID:             "release-android-1",
		WorkspaceOut:          filepath.Join(tempDir, "workspace"),
	})
	if err != nil {
		t.Fatalf("prepareAndroidCodePatchPlan() error = %v", err)
	}
	return plan
}

// TestPrepareAndroidCodePatchPlanAcceptsBuildIDOnlyNativeDrift reproduces the
// A3 refusal: libdartjni.so present in both artifacts, differing only in its
// build-id because base and candidate were compiled at different paths.
func TestPrepareAndroidCodePatchPlanAcceptsBuildIDOnlyNativeDrift(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseNative := map[string][]byte{}
	candidateNative := map[string][]byte{}
	for _, abi := range nativeBuildIDFixtureABIs {
		baseNative["lib/"+abi+"/libdartjni.so"] = readNativeBuildIDFixture(t, abi, "a")
		candidateNative["lib/"+abi+"/libdartjni.so"] = readNativeBuildIDFixture(t, abi, "b")
	}

	baseArtifactPath, candidateArtifactPath := writeBuildIDPlanArtifacts(t, tempDir, baseNative, candidateNative)
	plan := prepareBuildIDPlan(t, tempDir, baseArtifactPath, candidateArtifactPath)

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
}

// TestPrepareAndroidCodePatchPlanBlocksNativeCodeDrift is the plan-level
// negative control: the same build-id difference, plus one byte changed inside
// .text, must still refuse.
func TestPrepareAndroidCodePatchPlanBlocksNativeCodeDrift(t *testing.T) {
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
		plan := prepareBuildIDPlan(t, tempDir, baseArtifactPath, candidateArtifactPath)

		if plan.Ready {
			t.Fatalf("%s: expected changed native code to block the plan", abi)
		}
		if !testPlanHasBlocker(plan, "blocked_native_drift", "lib/"+abi+"/libdartjni.so") {
			t.Fatalf("%s: expected blocked_native_drift, got %#v", abi, plan.Blockers)
		}
	}
}

// TestPrepareAndroidCodePatchPlanBlocksUnparseableNativeDrift: a truncated
// library cannot be normalised, so the strict raw comparison stands.
func TestPrepareAndroidCodePatchPlanBlocksUnparseableNativeDrift(t *testing.T) {
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
	plan := prepareBuildIDPlan(t, tempDir, baseArtifactPath, candidateArtifactPath)

	if plan.Ready {
		t.Fatalf("expected an unparseable native library to block the plan")
	}
	if !testPlanHasBlocker(plan, "blocked_native_drift", "lib/arm64-v8a/libdartjni.so") {
		t.Fatalf("expected blocked_native_drift, got %#v", plan.Blockers)
	}
}

// TestPrepareAndroidCodePatchPlanBlocksNonELFNativeDrift keeps the pre-existing
// behaviour for native entries that were never ELF at all.
func TestPrepareAndroidCodePatchPlanBlocksNonELFNativeDrift(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	baseArtifactPath, candidateArtifactPath := writeBuildIDPlanArtifacts(
		t,
		tempDir,
		map[string][]byte{"lib/arm64-v8a/libflutter.so": []byte("base-flutter")},
		map[string][]byte{"lib/arm64-v8a/libflutter.so": []byte("cand-flutter")},
	)
	plan := prepareBuildIDPlan(t, tempDir, baseArtifactPath, candidateArtifactPath)

	if plan.Ready {
		t.Fatalf("expected non-ELF native drift to block the plan")
	}
	if !testPlanHasBlocker(plan, "blocked_native_drift", "lib/arm64-v8a/libflutter.so") {
		t.Fatalf("expected blocked_native_drift, got %#v", plan.Blockers)
	}
}

func testPlanHasBlocker(plan *androidCodePatchPlan, id string, path string) bool {
	for _, blocker := range plan.Blockers {
		if blocker.ID == id && blocker.Path == path {
			return true
		}
	}
	return false
}
