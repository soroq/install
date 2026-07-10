package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	androidrelease "soroq/backend/internal/androidrelease"
)

// soroqBundledMetadataAsset is the Flutter asset path (relative to the project root) for the bundled
// Soroq metadata the Android release validator requires. Flutter packages it into
// assets/flutter_assets/soroq/soroq_metadata.json, which androidrelease.readBundledMetadataFromZip
// looks for.
const soroqBundledMetadataAsset = "soroq/soroq_metadata.json"

// CANONICAL SOURCE OF TRUTH IS THE FORK FRONTEND — NOT THIS FILE.
//
// The Soroq Flutter fork's asset bundler (packages/flutter_tools/lib/src/soroq_metadata.dart,
// tracked as forks/flutter-sdk/patches/0010-soroq-android-bundled-metadata.patch) WRITES the shipped
// assets/flutter_assets/soroq/soroq_metadata.json during every real Android build and OVERRIDES any
// pubspec-declared soroq_metadata.json asset. That generator — never this Go code — decides the
// runtime_id that ends up in the APK/AAB.
//
// This Go generator exists only as a PREVIEW/FALLBACK: it lets `soroq` write a plausible
// soroq/soroq_metadata.json before a build so a project isn't missing the asset, and it lets the CLI
// compute the expected runtime_id for cross-checking against the real (fork-produced) artifact. It is
// therefore held BYTE-EXACT to the fork's derivation: same manifest_trust_fingerprint canonicalization
// and the same version-INCLUSIVE runtime_id JSON, so the CLI never disagrees with the fork on identity.
//
// runtime_id is version-INCLUSIVE by design (Decision A): it is sha256 over an ordered JSON document
// that includes app_version/build_name/build_number. Bumping the pubspec `version:` therefore changes
// the runtime_id, which is exactly what makes an old patch (built for the old runtime_id) get REJECTED
// against a new base build. Do NOT reintroduce a version-exclusive runtime_id.

// generateSoroqBundledMetadata reads soroq.yaml + pubspec.yaml from projectDir and writes
// soroq/soroq_metadata.json — the preview/fallback asset. The real build path lets the fork frontend
// OVERRIDE this file; the bytes here are kept identical to the fork's jsonEncode output (compact, no
// HTML escaping, null app/soroq fields omitted) so the preview matches the shipped asset.
func generateSoroqBundledMetadata(projectDir string) error {
	configBytes, err := os.ReadFile(filepath.Join(projectDir, "soroq.yaml"))
	if err != nil {
		return err
	}
	pubspecBytes, err := os.ReadFile(filepath.Join(projectDir, "pubspec.yaml"))
	if err != nil {
		return err
	}
	metadata, err := buildSoroqBundledMetadata(configBytes, pubspecBytes)
	if err != nil {
		return err
	}
	out, err := renderSoroqBundledMetadataJSON(metadata)
	if err != nil {
		return err
	}
	metadataPath := filepath.Join(projectDir, filepath.FromSlash(soroqBundledMetadataAsset))
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(metadataPath, out, 0o644)
}

// regenerateSoroqBundledMetadataForBuild refreshes soroq/soroq_metadata.json right before an Android
// build, so the shipped asset is always present and identical between the base build and later
// candidate/patch builds. It is best-effort on a project that has not been `soroq init`ed: with no
// soroq.yaml there is nothing to generate and the downstream release step fails clearly with the
// missing-metadata error. A malformed soroq.yaml/pubspec.yaml is a hard error.
//
// NOTE: on the real build path the fork frontend re-generates and OVERRIDES this file from the same
// soroq.yaml + pubspec.yaml, so the fork always wins. This regen keeps the preview honest and ensures
// the asset exists even when the fork wiring is not exercised (e.g. inspection before a build).
func regenerateSoroqBundledMetadataForBuild(projectDir string) error {
	if _, err := os.Stat(filepath.Join(projectDir, "soroq.yaml")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return generateSoroqBundledMetadata(projectDir)
}

func buildSoroqBundledMetadata(soroqConfig []byte, pubspec []byte) (androidrelease.BundledMetadata, error) {
	values := parseTopLevelYaml(soroqConfig)
	appID := strings.TrimSpace(values["app_id"])
	channel := strings.TrimSpace(values["channel"])
	strategy := strings.TrimSpace(values["runtime_id_strategy"])
	if appID == "" {
		return androidrelease.BundledMetadata{}, errors.New("soroq.yaml is missing app_id; run `soroq init`")
	}
	if channel == "" {
		return androidrelease.BundledMetadata{}, errors.New("soroq.yaml is missing channel; run `soroq init`")
	}
	if strategy == "" {
		strategy = "manifest_trust_v1"
	}

	pub := parseTopLevelYaml(pubspec)
	appName := strings.TrimSpace(pub["name"])
	if appName == "" {
		return androidrelease.BundledMetadata{}, errors.New("pubspec.yaml is missing a top-level name")
	}
	// Match the fork's FlutterManifest semantics exactly (flutter_manifest.dart):
	//   appVersion  = the pubspec `version:` string (pub_semver-normalized; raw is byte-exact for any
	//                 parseable version such as 1.0.0+1 / 1.0.1+2 — the only cases `flutter create` emits)
	//   buildName   = part before '+', or the whole version when there is no '+'
	//   buildNumber = part after '+', or NULL when there is no '+'
	appVersion, buildName, buildNumber := soroqPubspecVersionParts(strings.TrimSpace(pub["version"]))

	soroq := androidrelease.BundledSoroqMetadata{
		AppID:             appID,
		Channel:           channel,
		RuntimeIDStrategy: soroqStringPtr(strategy),
	}

	switch strategy {
	case "manifest_trust_v1":
		trust, err := parseSoroqManifestTrust(soroqConfig)
		if err != nil {
			return androidrelease.BundledMetadata{}, err
		}
		if trust == nil || len(trust.Keys) == 0 {
			return androidrelease.BundledMetadata{}, errors.New("soroq.yaml runtime_id_strategy manifest_trust_v1 requires manifest_trust keys; run `soroq init --force`")
		}
		fingerprint, err := soroqManifestTrustFingerprint(trust)
		if err != nil {
			return androidrelease.BundledMetadata{}, err
		}
		soroq.ManifestTrust = trust
		soroq.ManifestTrustFingerprint = soroqStringPtr(fingerprint)
		runtimeID, err := soroqManifestTrustRuntimeID(appID, channel, appVersion, buildName, buildNumber, fingerprint)
		if err != nil {
			return androidrelease.BundledMetadata{}, err
		}
		soroq.RuntimeID = runtimeID
	case "manual":
		// The fork uses the literal soroq.yaml `runtime_id:` verbatim for the manual strategy.
		runtimeID := strings.TrimSpace(values["runtime_id"])
		if runtimeID == "" {
			return androidrelease.BundledMetadata{}, errors.New("soroq.yaml runtime_id_strategy manual requires a runtime_id value")
		}
		soroq.RuntimeID = runtimeID
	default:
		return androidrelease.BundledMetadata{}, fmt.Errorf("soroq.yaml has unsupported runtime_id_strategy %q", strategy)
	}

	metadata := androidrelease.BundledMetadata{
		SchemaVersion: 1,
		App: androidrelease.BundledAppMetadata{
			Name:        appName,
			Version:     appVersion,
			BuildName:   buildName,
			BuildNumber: buildNumber,
		},
		Soroq: soroq,
	}
	if err := metadata.Validate(); err != nil {
		return androidrelease.BundledMetadata{}, fmt.Errorf("generated Soroq bundled metadata is invalid: %w", err)
	}
	return metadata, nil
}

// soroqPubspecVersionParts splits a pubspec `version:` string into (appVersion, buildName, buildNumber)
// pointers, matching FlutterManifest.appVersion/buildName/buildNumber. An empty version yields three
// nils; a version without '+' yields appVersion==buildName and a nil buildNumber.
func soroqPubspecVersionParts(version string) (appVersion, buildName, buildNumber *string) {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, nil, nil
	}
	v := version
	if name, number, ok := strings.Cut(version, "+"); ok {
		n := name
		num := number
		return &v, &n, &num
	}
	return &v, &v, nil
}

// soroqManifestTrustFingerprint mirrors soroq_metadata.dart:_manifestTrustFingerprint byte-for-byte.
// fingerprint = hex(sha256(utf8(compactJSON))) where compactJSON is the Dart jsonEncode of the ordered
// map { keyset_version?: <int>, keys: [ {id, public_key} ... sorted by id asc ] }. Keys are normalized
// to ONLY {id, public_key} in that field order. Empty keys is an error.
func soroqManifestTrustFingerprint(trust *androidrelease.ManifestTrust) (string, error) {
	if trust == nil || len(trust.Keys) == 0 {
		return "", errors.New("soroq manifest trust keys must not be empty")
	}
	type fpKey struct {
		ID        string `json:"id"`
		PublicKey string `json:"public_key"`
	}
	keys := make([]fpKey, 0, len(trust.Keys))
	for _, k := range trust.Keys {
		id := ""
		if k.ID != nil {
			id = *k.ID
		}
		if strings.TrimSpace(id) == "" || strings.TrimSpace(k.PublicKey) == "" {
			return "", errors.New(`soroq manifest trust keys require non-empty "id" and "public_key" values`)
		}
		keys = append(keys, fpKey{ID: id, PublicKey: k.PublicKey})
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })

	type fpDoc struct {
		KeysetVersion *int    `json:"keyset_version,omitempty"`
		Keys          []fpKey `json:"keys"`
	}
	canonical, err := encodeCompactJSON(fpDoc{KeysetVersion: trust.KeysetVersion, Keys: keys})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// soroqManifestTrustRuntimeID mirrors soroq_metadata.dart:_deriveManifestTrustRuntimeId byte-for-byte.
// runtime_id = hex(sha256(utf8(compactJSON))) where compactJSON is the Dart jsonEncode of the ordered
// map { strategy, app_id, channel, app_version, build_name, build_number, manifest_trust_fingerprint }.
// app_version/build_name/build_number are written as JSON null when absent (NOT omitted) — Dart's
// jsonEncode emits null for null map values.
func soroqManifestTrustRuntimeID(appID, channel string, appVersion, buildName, buildNumber *string, fingerprint string) (string, error) {
	type ridDoc struct {
		Strategy                 string  `json:"strategy"`
		AppID                    string  `json:"app_id"`
		Channel                  string  `json:"channel"`
		AppVersion               *string `json:"app_version"`
		BuildName                *string `json:"build_name"`
		BuildNumber              *string `json:"build_number"`
		ManifestTrustFingerprint string  `json:"manifest_trust_fingerprint"`
	}
	canonical, err := encodeCompactJSON(ridDoc{
		Strategy:                 "manifest_trust_v1",
		AppID:                    appID,
		Channel:                  channel,
		AppVersion:               appVersion,
		BuildName:                buildName,
		BuildNumber:              buildNumber,
		ManifestTrustFingerprint: fingerprint,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// renderSoroqBundledMetadataJSON serializes the preview/fallback asset byte-exactly to the fork's
// jsonEncode(metadata): compact (no spaces), HTML escaping DISABLED (Dart does not escape <>&), and the
// null app/soroq optional fields OMITTED (Dart writes them with `if (x != null)`). Only the runtime_id
// derivation map above writes explicit nulls; the asset itself omits them.
func renderSoroqBundledMetadataJSON(metadata androidrelease.BundledMetadata) ([]byte, error) {
	type metaFileApp struct {
		Name        string  `json:"name"`
		Version     *string `json:"version,omitempty"`
		BuildName   *string `json:"build_name,omitempty"`
		BuildNumber *string `json:"build_number,omitempty"`
	}
	type metaFileSoroq struct {
		AppID                    string                        `json:"app_id"`
		Channel                  string                        `json:"channel"`
		RuntimeID                string                        `json:"runtime_id"`
		RuntimeIDStrategy        string                        `json:"runtime_id_strategy"`
		ManifestTrust            *androidrelease.ManifestTrust `json:"manifest_trust,omitempty"`
		ManifestTrustFingerprint *string                       `json:"manifest_trust_fingerprint,omitempty"`
	}
	type metaFileDoc struct {
		SchemaVersion int           `json:"schema_version"`
		App           metaFileApp   `json:"app"`
		Soroq         metaFileSoroq `json:"soroq"`
	}
	strategy := ""
	if metadata.Soroq.RuntimeIDStrategy != nil {
		strategy = *metadata.Soroq.RuntimeIDStrategy
	}
	doc := metaFileDoc{
		SchemaVersion: metadata.SchemaVersion,
		App: metaFileApp{
			Name:        metadata.App.Name,
			Version:     metadata.App.Version,
			BuildName:   metadata.App.BuildName,
			BuildNumber: metadata.App.BuildNumber,
		},
		Soroq: metaFileSoroq{
			AppID:                    metadata.Soroq.AppID,
			Channel:                  metadata.Soroq.Channel,
			RuntimeID:                metadata.Soroq.RuntimeID,
			RuntimeIDStrategy:        strategy,
			ManifestTrust:            metadata.Soroq.ManifestTrust,
			ManifestTrustFingerprint: metadata.Soroq.ManifestTrustFingerprint,
		},
	}
	return encodeCompactJSON(doc)
}

// encodeCompactJSON renders v the way Dart's jsonEncode does: compact separators and NO HTML escaping
// (Go's default json.Marshal escapes <, >, & — Dart does not). The trailing newline json.Encoder adds
// is stripped so the bytes match jsonEncode exactly.
func encodeCompactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// parseSoroqManifestTrust extracts the top-level manifest_trust block from a soroq.yaml body. It
// understands the deterministic layout written by renderSoroqConfig:
//
//	manifest_trust:
//	  keyset_version: 1
//	  keys:
//	    - id: prod-primary
//	      public_key: <base64>
//
// Returns nil (no error) when there is no manifest_trust block.
func parseSoroqManifestTrust(soroqConfig []byte) (*androidrelease.ManifestTrust, error) {
	lines := strings.Split(string(soroqConfig), "\n")
	start := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == "manifest_trust:" && !startsWithIndent(line) {
			start = index
			break
		}
	}
	if start == -1 {
		return nil, nil
	}

	trust := &androidrelease.ManifestTrust{}
	var current *androidrelease.ManifestTrustKey
	inKeys := false
	flush := func() {
		if current != nil {
			trust.Keys = append(trust.Keys, *current)
			current = nil
		}
	}
	for index := start + 1; index < len(lines); index++ {
		raw := lines[index]
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !startsWithIndent(raw) {
			break // dedent to a new top-level key ends the block
		}
		if strings.HasPrefix(trimmed, "keyset_version:") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "keyset_version:"))
			if n, err := strconv.Atoi(value); err == nil {
				trust.KeysetVersion = &n
			}
			continue
		}
		if trimmed == "keys:" {
			inKeys = true
			continue
		}
		if !inKeys {
			continue
		}
		if strings.HasPrefix(trimmed, "-") {
			flush()
			current = &androidrelease.ManifestTrustKey{}
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			if trimmed == "" {
				continue
			}
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch key {
		case "id":
			id := value
			current.ID = &id
		case "public_key":
			current.PublicKey = value
		}
	}
	flush()
	return trust, nil
}

func startsWithIndent(line string) bool {
	return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
}

func soroqStringPtr(value string) *string {
	return &value
}
