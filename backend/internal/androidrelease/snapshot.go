package androidrelease

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Snapshot struct {
	SchemaVersion   int                         `json:"schema_version"`
	CapturedAt      time.Time                   `json:"captured_at"`
	Artifact        ArtifactDescriptor          `json:"artifact"`
	Metadata        BundledMetadata             `json:"metadata"`
	NativeLibs      []EntryDigest               `json:"native_libs"`
	AOTLinkMetadata []AOTLinkMetadataDescriptor `json:"aot_link_metadata,omitempty"`
}

type ArtifactDescriptor struct {
	Type                   string `json:"type"`
	Path                   string `json:"path"`
	Source                 string `json:"source,omitempty"`
	SHA256                 string `json:"sha256"`
	SizeBytes              uint64 `json:"size_bytes"`
	BundledMetadataZipPath string `json:"bundled_metadata_zip_path"`
}

type EntryDigest struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes uint64 `json:"size_bytes"`
}

type AOTLinkMetadataDescriptor struct {
	Snapshot   string                    `json:"snapshot"`
	Path       string                    `json:"path"`
	Source     string                    `json:"source,omitempty"`
	SHA256     string                    `json:"sha256"`
	SizeBytes  uint64                    `json:"size_bytes"`
	Symbol     *AOTSnapshotSymbol        `json:"symbol,omitempty"`
	KeyColumns []string                  `json:"key_columns,omitempty"`
	Notes      []string                  `json:"notes,omitempty"`
	Extra      map[string]map[string]any `json:"extra,omitempty"`
}

type AOTSnapshotSymbol struct {
	Name       string `json:"name"`
	FileOffset uint64 `json:"file_offset"`
	SizeBytes  uint64 `json:"size_bytes"`
}

type BundledMetadata struct {
	SchemaVersion int                  `json:"schema_version"`
	App           BundledAppMetadata   `json:"app"`
	Soroq         BundledSoroqMetadata `json:"soroq"`
}

type BundledAppMetadata struct {
	Name        string  `json:"name"`
	Version     *string `json:"version"`
	BuildName   *string `json:"build_name"`
	BuildNumber *string `json:"build_number"`
}

type BundledSoroqMetadata struct {
	AppID                    string         `json:"app_id"`
	Channel                  string         `json:"channel"`
	RuntimeID                string         `json:"runtime_id"`
	RuntimeIDStrategy        *string        `json:"runtime_id_strategy"`
	ManifestTrust            *ManifestTrust `json:"manifest_trust"`
	ManifestTrustFingerprint *string        `json:"manifest_trust_fingerprint"`
}

type ManifestTrust struct {
	KeysetVersion *int               `json:"keyset_version,omitempty"`
	Keys          []ManifestTrustKey `json:"keys"`
}

type ManifestTrustKey struct {
	ID        *string `json:"id,omitempty"`
	PublicKey string  `json:"public_key"`
}

type ComparisonReport struct {
	SchemaVersion int               `json:"schema_version"`
	Compatible    bool              `json:"compatible"`
	Base          SnapshotSummary   `json:"base"`
	Candidate     SnapshotSummary   `json:"candidate"`
	Checks        []ComparisonCheck `json:"checks"`
}

type SnapshotSummary struct {
	AppID                    string   `json:"app_id"`
	Channel                  string   `json:"channel"`
	RuntimeID                string   `json:"runtime_id"`
	RuntimeIDStrategy        string   `json:"runtime_id_strategy"`
	Version                  *string  `json:"version,omitempty"`
	BuildName                *string  `json:"build_name,omitempty"`
	BuildNumber              *string  `json:"build_number,omitempty"`
	ManifestTrustFingerprint *string  `json:"manifest_trust_fingerprint,omitempty"`
	NativeLibPaths           []string `json:"native_lib_paths"`
}

type ComparisonCheck struct {
	ID     string `json:"id"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

func CaptureSnapshot(artifactPath string) (*Snapshot, error) {
	artifactPath = filepath.Clean(artifactPath)
	artifactType, err := detectArtifactType(artifactPath)
	if err != nil {
		return nil, err
	}

	artifactBytes, err := os.ReadFile(artifactPath)
	if err != nil {
		return nil, err
	}

	artifactInfo, err := os.Stat(artifactPath)
	if err != nil {
		return nil, err
	}

	reader, err := zip.OpenReader(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("open %s as zip: %w", artifactType, err)
	}
	defer reader.Close()

	metadataZipPath, metadata, err := readBundledMetadataFromZip(reader.File)
	if err != nil {
		return nil, err
	}

	nativeLibs, err := readNativeLibrariesFromZip(reader.File)
	if err != nil {
		return nil, err
	}

	sort.Slice(nativeLibs, func(i, j int) bool {
		return nativeLibs[i].Path < nativeLibs[j].Path
	})

	return &Snapshot{
		SchemaVersion: 1,
		CapturedAt:    time.Now().UTC(),
		Artifact: ArtifactDescriptor{
			Type:                   artifactType,
			Path:                   artifactPath,
			SHA256:                 sha256Hex(artifactBytes),
			SizeBytes:              uint64(artifactInfo.Size()),
			BundledMetadataZipPath: metadataZipPath,
		},
		Metadata:   *metadata,
		NativeLibs: nativeLibs,
	}, nil
}

func LoadSnapshot(path string) (*Snapshot, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(bytes, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported snapshot schema version %d", snapshot.SchemaVersion)
	}
	if err := snapshot.Metadata.Validate(); err != nil {
		return nil, fmt.Errorf("invalid bundled metadata: %w", err)
	}
	for index, descriptor := range snapshot.AOTLinkMetadata {
		if err := validateAOTLinkMetadataDescriptor(descriptor); err != nil {
			return nil, fmt.Errorf("invalid aot_link_metadata[%d]: %w", index, err)
		}
	}
	return &snapshot, nil
}

func AddAOTLinkMetadataFromFile(
	snapshot *Snapshot,
	path string,
	snapshotName string,
	source string,
) error {
	path = filepath.Clean(path)
	bytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read AOT link metadata: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat AOT link metadata: %w", err)
	}
	descriptor := AOTLinkMetadataDescriptor{
		Snapshot:  normalizedDefaultString(snapshotName, "isolate"),
		Path:      path,
		Source:    normalizedDefaultString(source, "release_retained"),
		SHA256:    sha256Hex(bytes),
		SizeBytes: uint64(info.Size()),
	}
	if err := validateAOTLinkMetadataDescriptor(descriptor); err != nil {
		return err
	}
	snapshot.AOTLinkMetadata = append(snapshot.AOTLinkMetadata, descriptor)
	sort.Slice(snapshot.AOTLinkMetadata, func(i, j int) bool {
		if snapshot.AOTLinkMetadata[i].Snapshot == snapshot.AOTLinkMetadata[j].Snapshot {
			return snapshot.AOTLinkMetadata[i].Path < snapshot.AOTLinkMetadata[j].Path
		}
		return snapshot.AOTLinkMetadata[i].Snapshot < snapshot.AOTLinkMetadata[j].Snapshot
	})
	return nil
}

func CompareSnapshots(base *Snapshot, candidate *Snapshot) ComparisonReport {
	checks := make([]ComparisonCheck, 0, 8)
	addCheck := func(id string, passed bool, detail string) {
		checks = append(checks, ComparisonCheck{
			ID:     id,
			Passed: passed,
			Detail: detail,
		})
	}

	baseStrategy := base.Metadata.RuntimeIDStrategy()
	candidateStrategy := candidate.Metadata.RuntimeIDStrategy()

	addCheck(
		"app_id",
		base.Metadata.Soroq.AppID == candidate.Metadata.Soroq.AppID,
		fmt.Sprintf("base=%s candidate=%s", base.Metadata.Soroq.AppID, candidate.Metadata.Soroq.AppID),
	)
	addCheck(
		"channel",
		base.Metadata.Soroq.Channel == candidate.Metadata.Soroq.Channel,
		fmt.Sprintf("base=%s candidate=%s", base.Metadata.Soroq.Channel, candidate.Metadata.Soroq.Channel),
	)
	addCheck(
		"runtime_id",
		base.Metadata.Soroq.RuntimeID == candidate.Metadata.Soroq.RuntimeID,
		fmt.Sprintf("base=%s candidate=%s", base.Metadata.Soroq.RuntimeID, candidate.Metadata.Soroq.RuntimeID),
	)
	addCheck(
		"runtime_id_strategy",
		baseStrategy == candidateStrategy,
		fmt.Sprintf("base=%s candidate=%s", baseStrategy, candidateStrategy),
	)
	addCheck(
		"build_name",
		normalizedOptionalString(base.Metadata.App.BuildName) == normalizedOptionalString(candidate.Metadata.App.BuildName),
		fmt.Sprintf(
			"base=%s candidate=%s",
			normalizedOptionalString(base.Metadata.App.BuildName),
			normalizedOptionalString(candidate.Metadata.App.BuildName),
		),
	)
	addCheck(
		"build_number",
		normalizedOptionalString(base.Metadata.App.BuildNumber) == normalizedOptionalString(candidate.Metadata.App.BuildNumber),
		fmt.Sprintf(
			"base=%s candidate=%s",
			normalizedOptionalString(base.Metadata.App.BuildNumber),
			normalizedOptionalString(candidate.Metadata.App.BuildNumber),
		),
	)
	addCheck(
		"manifest_trust_fingerprint",
		normalizedOptionalString(base.Metadata.Soroq.ManifestTrustFingerprint) ==
			normalizedOptionalString(candidate.Metadata.Soroq.ManifestTrustFingerprint),
		fmt.Sprintf(
			"base=%s candidate=%s",
			normalizedOptionalString(base.Metadata.Soroq.ManifestTrustFingerprint),
			normalizedOptionalString(candidate.Metadata.Soroq.ManifestTrustFingerprint),
		),
	)

	baseNative := nativeLibraryMap(base.NativeLibs)
	candidateNative := nativeLibraryMap(candidate.NativeLibs)
	addCheck(
		"native_libraries",
		compareNativeLibraryMaps(baseNative, candidateNative),
		fmt.Sprintf(
			"base=%s candidate=%s",
			strings.Join(sortedMapKeys(baseNative), ","),
			strings.Join(sortedMapKeys(candidateNative), ","),
		),
	)

	compatible := true
	for _, check := range checks {
		if !check.Passed {
			compatible = false
			break
		}
	}

	return ComparisonReport{
		SchemaVersion: 1,
		Compatible:    compatible,
		Base:          snapshotSummary(base),
		Candidate:     snapshotSummary(candidate),
		Checks:        checks,
	}
}

func DeriveABIs(snapshot *Snapshot) []string {
	seen := make(map[string]struct{})
	for _, entry := range snapshot.NativeLibs {
		parts := strings.Split(entry.Path, "/")
		if len(parts) < 3 || parts[0] != "lib" {
			continue
		}
		abi := strings.TrimSpace(parts[1])
		if abi == "" {
			continue
		}
		seen[abi] = struct{}{}
	}
	return sortedMapKeys(seen)
}

func (metadata BundledMetadata) Validate() error {
	if metadata.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema version %d", metadata.SchemaVersion)
	}
	if strings.TrimSpace(metadata.App.Name) == "" {
		return errors.New("app.name must be non-empty")
	}
	if strings.TrimSpace(metadata.Soroq.AppID) == "" {
		return errors.New("soroq.app_id must be non-empty")
	}
	if strings.TrimSpace(metadata.Soroq.Channel) == "" {
		return errors.New("soroq.channel must be non-empty")
	}
	if strings.TrimSpace(metadata.Soroq.RuntimeID) == "" {
		return errors.New("soroq.runtime_id must be non-empty")
	}
	switch metadata.RuntimeIDStrategy() {
	case "manual":
		return nil
	case "manifest_trust_v1":
		if metadata.Soroq.ManifestTrust == nil || len(metadata.Soroq.ManifestTrust.Keys) == 0 {
			return errors.New("soroq.manifest_trust is required when runtime_id_strategy is manifest_trust_v1")
		}
		if strings.TrimSpace(normalizedOptionalString(metadata.Soroq.ManifestTrustFingerprint)) == "" {
			return errors.New("soroq.manifest_trust_fingerprint is required when runtime_id_strategy is manifest_trust_v1")
		}
		return nil
	default:
		return fmt.Errorf("unsupported runtime_id_strategy %q", metadata.RuntimeIDStrategy())
	}
}

func (metadata BundledMetadata) RuntimeIDStrategy() string {
	if metadata.Soroq.RuntimeIDStrategy == nil || strings.TrimSpace(*metadata.Soroq.RuntimeIDStrategy) == "" {
		if strings.TrimSpace(metadata.Soroq.RuntimeID) != "" {
			return "manual"
		}
		return "manifest_trust_v1"
	}
	return strings.TrimSpace(*metadata.Soroq.RuntimeIDStrategy)
}

func validateAOTLinkMetadataDescriptor(descriptor AOTLinkMetadataDescriptor) error {
	if strings.TrimSpace(descriptor.Snapshot) == "" {
		return errors.New("snapshot must be non-empty")
	}
	if strings.TrimSpace(descriptor.Path) == "" {
		return errors.New("path must be non-empty")
	}
	if strings.TrimSpace(descriptor.SHA256) == "" {
		return errors.New("sha256 must be non-empty")
	}
	if descriptor.SizeBytes == 0 {
		return errors.New("size_bytes must be greater than zero")
	}
	if descriptor.Symbol != nil {
		if strings.TrimSpace(descriptor.Symbol.Name) == "" {
			return errors.New("symbol.name must be non-empty")
		}
		if descriptor.Symbol.SizeBytes == 0 {
			return errors.New("symbol.size_bytes must be greater than zero")
		}
	}
	return nil
}

func normalizedDefaultString(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func detectArtifactType(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".apk":
		return "apk", nil
	case ".aab":
		return "aab", nil
	default:
		return "", fmt.Errorf("unsupported Android artifact extension for %s", path)
	}
}

func readBundledMetadataFromZip(files []*zip.File) (string, *BundledMetadata, error) {
	var match *zip.File
	for _, file := range files {
		if normalizeMetadataZipPath(file.Name) != "" {
			if match != nil {
				return "", nil, fmt.Errorf("multiple bundled metadata assets found: %s and %s", match.Name, file.Name)
			}
			match = file
		}
	}
	if match == nil {
		return "", nil, errors.New("bundled metadata asset not found in Android artifact")
	}

	bytes, err := readZipFileBytes(match)
	if err != nil {
		return "", nil, fmt.Errorf("read bundled metadata %s: %w", match.Name, err)
	}

	var metadata BundledMetadata
	if err := json.Unmarshal(bytes, &metadata); err != nil {
		return "", nil, fmt.Errorf("decode bundled metadata %s: %w", match.Name, err)
	}
	if err := metadata.Validate(); err != nil {
		return "", nil, fmt.Errorf("validate bundled metadata %s: %w", match.Name, err)
	}

	return normalizeMetadataZipPath(match.Name), &metadata, nil
}

func readNativeLibrariesFromZip(files []*zip.File) ([]EntryDigest, error) {
	entries := make([]EntryDigest, 0)
	for _, file := range files {
		path := normalizeNativeLibraryZipPath(file.Name)
		if path == "" {
			continue
		}
		bytes, err := readZipFileBytes(file)
		if err != nil {
			return nil, fmt.Errorf("read native library %s: %w", file.Name, err)
		}
		entries = append(entries, EntryDigest{
			Path:      path,
			SHA256:    sha256Hex(bytes),
			SizeBytes: uint64(len(bytes)),
		})
	}
	if len(entries) == 0 {
		return nil, errors.New("no native libraries found in Android artifact")
	}
	return entries, nil
}

func snapshotSummary(snapshot *Snapshot) SnapshotSummary {
	return SnapshotSummary{
		AppID:                    snapshot.Metadata.Soroq.AppID,
		Channel:                  snapshot.Metadata.Soroq.Channel,
		RuntimeID:                snapshot.Metadata.Soroq.RuntimeID,
		RuntimeIDStrategy:        snapshot.Metadata.RuntimeIDStrategy(),
		Version:                  snapshot.Metadata.App.Version,
		BuildName:                snapshot.Metadata.App.BuildName,
		BuildNumber:              snapshot.Metadata.App.BuildNumber,
		ManifestTrustFingerprint: snapshot.Metadata.Soroq.ManifestTrustFingerprint,
		NativeLibPaths:           sortedMapKeys(nativeLibraryMap(snapshot.NativeLibs)),
	}
}

func nativeLibraryMap(entries []EntryDigest) map[string]EntryDigest {
	result := make(map[string]EntryDigest, len(entries))
	for _, entry := range entries {
		result[entry.Path] = entry
	}
	return result
}

func compareNativeLibraryMaps(base map[string]EntryDigest, candidate map[string]EntryDigest) bool {
	if len(base) != len(candidate) {
		return false
	}
	for path, baseEntry := range base {
		candidateEntry, ok := candidate[path]
		if !ok {
			return false
		}
		if baseEntry.SHA256 != candidateEntry.SHA256 || baseEntry.SizeBytes != candidateEntry.SizeBytes {
			return false
		}
	}
	return true
}

func sortedMapKeys[T any](items map[string]T) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func normalizedOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func normalizeMetadataZipPath(path string) string {
	const suffix = "assets/flutter_assets/soroq/soroq_metadata.json"
	if path == suffix {
		return suffix
	}
	if strings.HasSuffix(path, "/"+suffix) {
		return suffix
	}
	return ""
}

func normalizeNativeLibraryZipPath(path string) string {
	parts := strings.Split(path, "/")
	for index := range parts {
		if parts[index] != "lib" {
			continue
		}
		if len(parts) <= index+2 {
			return ""
		}
		relative := strings.Join(parts[index:], "/")
		if !strings.HasSuffix(relative, ".so") {
			return ""
		}
		return relative
	}
	return ""
}

func readZipFileBytes(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func sha256Hex(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
}
