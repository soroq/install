package androidrelease

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureSnapshotReadsBundledMetadataAndABIs(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "app-release.aab")
	writeArtifactZip(t, artifactPath, map[string][]byte{
		"base/assets/flutter_assets/soroq/soroq_metadata.json": []byte(`{
  "schema_version": 1,
  "app": {
    "name": "Example",
    "version": "1.2.3+45",
    "build_name": "1.2.3",
    "build_number": "45"
  },
  "soroq": {
    "app_id": "com.example.app",
    "channel": "stable",
    "runtime_id": "runtime-1",
    "runtime_id_strategy": "manifest_trust_v1",
    "manifest_trust": {
      "keys": [
        { "id": "prod-primary", "public_key": "abc" }
      ]
    },
    "manifest_trust_fingerprint": "fingerprint-1"
  }
}`),
		"base/lib/arm64-v8a/libapp.so": []byte("app"),
		"base/lib/x86_64/libapp.so":    []byte("app"),
	})

	snapshot, err := CaptureSnapshot(artifactPath)
	if err != nil {
		t.Fatalf("CaptureSnapshot() error = %v", err)
	}
	if snapshot.Artifact.Type != "aab" {
		t.Fatalf("expected aab type, got %q", snapshot.Artifact.Type)
	}
	if snapshot.Metadata.Soroq.AppID != "com.example.app" {
		t.Fatalf("expected app id, got %q", snapshot.Metadata.Soroq.AppID)
	}
	abis := DeriveABIs(snapshot)
	if len(abis) != 2 || abis[0] != "arm64-v8a" || abis[1] != "x86_64" {
		t.Fatalf("unexpected ABIs %v", abis)
	}
}

func TestDeriveABIsPrefersLibappOverPackagedRuntimeLibraries(t *testing.T) {
	snapshot := &Snapshot{
		NativeLibs: []EntryDigest{
			{Path: "lib/arm64-v8a/libsoroq_runtime_jni.so", SHA256: "runtime-arm64", SizeBytes: 1},
			{Path: "lib/armeabi-v7a/libsoroq_runtime_jni.so", SHA256: "runtime-arm", SizeBytes: 1},
			{Path: "lib/x86_64/libapp.so", SHA256: "app-x64", SizeBytes: 1},
			{Path: "lib/x86_64/libsoroq_runtime_jni.so", SHA256: "runtime-x64", SizeBytes: 1},
		},
	}

	abis := DeriveABIs(snapshot)
	if len(abis) != 1 || abis[0] != "x86_64" {
		t.Fatalf("expected libapp ABI only, got %v", abis)
	}
}

func TestCompareSnapshotsFlagsRuntimeMismatch(t *testing.T) {
	base := &Snapshot{
		SchemaVersion: 1,
		Metadata: BundledMetadata{
			SchemaVersion: 1,
			App:           BundledAppMetadata{Name: "Example"},
			Soroq: BundledSoroqMetadata{
				AppID:     "com.example.app",
				Channel:   "stable",
				RuntimeID: "runtime-1",
			},
		},
		NativeLibs: []EntryDigest{{Path: "lib/arm64-v8a/libapp.so", SHA256: "a", SizeBytes: 1}},
	}
	candidate := &Snapshot{
		SchemaVersion: 1,
		Metadata: BundledMetadata{
			SchemaVersion: 1,
			App:           BundledAppMetadata{Name: "Example"},
			Soroq: BundledSoroqMetadata{
				AppID:     "com.example.app",
				Channel:   "stable",
				RuntimeID: "runtime-2",
			},
		},
		NativeLibs: []EntryDigest{{Path: "lib/arm64-v8a/libapp.so", SHA256: "a", SizeBytes: 1}},
	}

	report := CompareSnapshots(base, candidate)
	if report.Compatible {
		t.Fatalf("expected incompatible report")
	}
	found := false
	for _, check := range report.Checks {
		if check.ID == "runtime_id" && !check.Passed {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected runtime_id mismatch check, got %+v", report.Checks)
	}
}

func TestAddAOTLinkMetadataFromFileRecordsRetainedDescriptor(t *testing.T) {
	tempDir := t.TempDir()
	linkMetadataPath := filepath.Join(tempDir, "link-metadata.tsv")
	linkMetadataBytes := []byte("schema_version\tsnapshot\n1\tisolate\n")
	if err := os.WriteFile(linkMetadataPath, linkMetadataBytes, 0o644); err != nil {
		t.Fatalf("WriteFile(link metadata) error = %v", err)
	}
	snapshot := &Snapshot{
		SchemaVersion: 1,
		Metadata: BundledMetadata{
			SchemaVersion: 1,
			App:           BundledAppMetadata{Name: "Example"},
			Soroq: BundledSoroqMetadata{
				AppID:     "com.example.app",
				Channel:   "stable",
				RuntimeID: "runtime-1",
			},
		},
		NativeLibs: []EntryDigest{{Path: "lib/arm64-v8a/libapp.so", SHA256: "a", SizeBytes: 1}},
	}

	if err := AddAOTLinkMetadataFromFile(snapshot, linkMetadataPath, "isolate", "release_retained"); err != nil {
		t.Fatalf("AddAOTLinkMetadataFromFile() error = %v", err)
	}
	if len(snapshot.AOTLinkMetadata) != 1 {
		t.Fatalf("expected one descriptor, got %#v", snapshot.AOTLinkMetadata)
	}
	descriptor := snapshot.AOTLinkMetadata[0]
	if descriptor.Snapshot != "isolate" || descriptor.Source != "release_retained" {
		t.Fatalf("unexpected descriptor identity: %#v", descriptor)
	}
	if descriptor.SHA256 != sha256Hex(linkMetadataBytes) {
		t.Fatalf("unexpected descriptor sha256: %#v", descriptor)
	}
	if descriptor.SizeBytes != uint64(len(linkMetadataBytes)) {
		t.Fatalf("unexpected descriptor size: %#v", descriptor)
	}

	snapshotPath := filepath.Join(tempDir, "snapshot.json")
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal(snapshot) error = %v", err)
	}
	if err := os.WriteFile(snapshotPath, encoded, 0o644); err != nil {
		t.Fatalf("WriteFile(snapshot) error = %v", err)
	}
	loaded, err := LoadSnapshot(snapshotPath)
	if err != nil {
		t.Fatalf("LoadSnapshot() error = %v", err)
	}
	if len(loaded.AOTLinkMetadata) != 1 || loaded.AOTLinkMetadata[0].SHA256 != descriptor.SHA256 {
		t.Fatalf("expected descriptor to round trip, got %#v", loaded.AOTLinkMetadata)
	}
}

func writeArtifactZip(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create(%s) error = %v", path, err)
	}
	defer file.Close()

	writer := zip.NewWriter(file)
	for name, payload := range entries {
		entryWriter, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%s) error = %v", name, err)
		}
		if _, err := entryWriter.Write(payload); err != nil {
			t.Fatalf("Write(%s) error = %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
}
