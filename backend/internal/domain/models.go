package domain

import (
	"strings"
	"time"
)

type PatchKind string

const (
	PatchKindConfig                         PatchKind = "config"
	PatchKindAsset                          PatchKind = "asset"
	PatchKindExperimentalNativeAOT          PatchKind = "experimental_native_aot"
	PatchKindAssetPlusExperimentalNativeAOT PatchKind = "asset_plus_experimental_native_aot"
	PatchKindRuntimeManagedDart             PatchKind = "runtime_managed_dart"
	PatchKindAssetPlusRuntimeManagedDart    PatchKind = "asset_plus_runtime_managed_dart"
	// PatchKindIOSEngine is the iOS ENGINE lane: hot-patching Dart CODE via the soroq
	// interpreter-in-engine. Its bundle carries the device-format signed artifacts
	// (manifest.json + manifest.sig + bytecode) which the control plane stores + serves
	// VERBATIM (never re-signed server-side; the pinned-key Ed25519 signature is made by the
	// operator/CLI). Distinct from the config_ota_only and runtime_managed_dart lanes.
	PatchKindIOSEngine               PatchKind = "ios_engine"
	LegacyPatchKindDartCode          PatchKind = "dart_code"
	LegacyPatchKindAssetPlusDartCode PatchKind = "asset_plus_dart_code"
)

func NormalizePatchKind(kind PatchKind) PatchKind {
	switch kind {
	case LegacyPatchKindDartCode:
		return PatchKindExperimentalNativeAOT
	case LegacyPatchKindAssetPlusDartCode:
		return PatchKindAssetPlusExperimentalNativeAOT
	default:
		return kind
	}
}

func IsKnownPatchKind(kind PatchKind) bool {
	switch NormalizePatchKind(kind) {
	case PatchKindConfig,
		PatchKindAsset,
		PatchKindExperimentalNativeAOT,
		PatchKindAssetPlusExperimentalNativeAOT,
		PatchKindRuntimeManagedDart,
		PatchKindAssetPlusRuntimeManagedDart,
		PatchKindIOSEngine:
		return true
	default:
		return false
	}
}

func (k PatchKind) Normalized() PatchKind {
	return NormalizePatchKind(k)
}

// IsIOSEngine reports whether this is the iOS ENGINE lane, whose bundle is stored + served
// verbatim (device-format signed artifacts) rather than re-signed through the PatchManifest path.
func (k PatchKind) IsIOSEngine() bool {
	return NormalizePatchKind(k) == PatchKindIOSEngine
}

type ActivationMode string

const (
	ActivationNextColdStart ActivationMode = "next_cold_start"
	ActivationAppControlled ActivationMode = "app_controlled_restart"
	ActivationSafeBoundary  ActivationMode = "safe_boundary_restart"
	ActivationDownloadOnly  ActivationMode = "download_only"
)

func IsKnownActivationMode(mode ActivationMode) bool {
	switch mode {
	case ActivationNextColdStart,
		ActivationAppControlled,
		ActivationSafeBoundary,
		ActivationDownloadOnly:
		return true
	default:
		return false
	}
}

const DefaultPatchTrack = "stable"

func NormalizePatchTrack(track string) string {
	track = strings.ToLower(strings.TrimSpace(track))
	switch track {
	case "", "production":
		return DefaultPatchTrack
	case "staged":
		return "staging"
	default:
		return track
	}
}

func IsKnownPatchTrack(track string) bool {
	track = NormalizePatchTrack(track)
	if track == DefaultPatchTrack || track == "staging" {
		return true
	}
	if len(track) > 64 {
		return false
	}
	for _, char := range track {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= '0' && char <= '9':
		case char == '-' || char == '_' || char == '.':
		default:
			return false
		}
	}
	return track != ""
}

type App struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	OwnerEmail  string    `json:"owner_email,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Release struct {
	ID                   string `json:"id"`
	AppID                string `json:"app_id"`
	RuntimeID            string `json:"runtime_id"`
	Version              string `json:"version"`
	Platform             string `json:"platform"`
	Arch                 string `json:"arch"`
	Channel              string `json:"channel"`
	ManifestSigningKeyID string `json:"manifest_signing_key_id,omitempty"`
	// Toolchain identity binding (additive, T004). These pin a release to the EXACT toolchain that
	// built it so a patch compiles against the right hosted artifacts. They are advisory metadata on
	// the release record; the immutable engineLaneBaseline + engineMatchesBaseline (soroqctl) remain
	// the sole identity ENFORCER — the registry never moves that check server-side.
	FlutterRevision     string `json:"flutter_revision,omitempty"`
	DartRevision        string `json:"dart_revision,omitempty"`
	SoroqEngineRevision string `json:"soroq_engine_revision,omitempty"`
	// ToolchainID binds this release to a toolchain_versions.soroq_toolchain_version row.
	ToolchainID string    `json:"toolchain_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type ReleaseArtifact struct {
	ReleaseID   string    `json:"release_id"`
	FileName    string    `json:"file_name,omitempty"`
	SHA256      string    `json:"sha256"`
	SizeBytes   uint64    `json:"size_bytes"`
	ContentType string    `json:"content_type,omitempty"`
	UploadedAt  time.Time `json:"uploaded_at"`
}

type Patch struct {
	ID                   string         `json:"id"`
	AppID                string         `json:"app_id"`
	ReleaseID            string         `json:"release_id"`
	RuntimeID            string         `json:"runtime_id"`
	Number               int            `json:"number"`
	Channel              string         `json:"channel"`
	Track                string         `json:"track,omitempty"`
	Kind                 PatchKind      `json:"kind"`
	ActivationMode       ActivationMode `json:"activation_mode"`
	ManifestURL          string         `json:"manifest_url"`
	BundleURL            string         `json:"bundle_url,omitempty"`
	RolloutPercent       int            `json:"rollout_percent"`
	ManifestSigningKeyID string         `json:"manifest_signing_key_id,omitempty"`
	RolledBack           bool           `json:"rolled_back"`
	CreatedAt            time.Time      `json:"created_at"`
}

type PatchArtifact struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes uint64 `json:"size_bytes"`
}

type PatchManifest struct {
	PatchID        string         `json:"patch_id"`
	PatchNumber    int            `json:"patch_number"`
	RuntimeID      string         `json:"runtime_id"`
	ReleaseID      string         `json:"release_id"`
	Channel        string         `json:"channel"`
	Kind           PatchKind      `json:"kind"`
	ActivationMode ActivationMode `json:"activation_mode"`
	Artifact       PatchArtifact  `json:"artifact"`
	SignatureKeyID *string        `json:"signature_key_id,omitempty"`
	Signature      *string        `json:"signature"`
}

type CreateAppRequest struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	OwnerEmail  string `json:"owner_email,omitempty"`
}

type CreateReleaseRequest struct {
	ID                   string `json:"id"`
	AppID                string `json:"app_id"`
	RuntimeID            string `json:"runtime_id"`
	Version              string `json:"version"`
	Platform             string `json:"platform"`
	Arch                 string `json:"arch"`
	Channel              string `json:"channel"`
	ManifestSigningKeyID string `json:"manifest_signing_key_id,omitempty"`
	// Optional toolchain identity binding (additive, T004) carried through to the release record.
	FlutterRevision     string `json:"flutter_revision,omitempty"`
	DartRevision        string `json:"dart_revision,omitempty"`
	SoroqEngineRevision string `json:"soroq_engine_revision,omitempty"`
	ToolchainID         string `json:"toolchain_id,omitempty"`
}

type CreatePatchRequest struct {
	ID                   string         `json:"id"`
	AppID                string         `json:"app_id"`
	ReleaseID            string         `json:"release_id"`
	RuntimeID            string         `json:"runtime_id"`
	Channel              string         `json:"channel"`
	Track                string         `json:"track,omitempty"`
	Kind                 PatchKind      `json:"kind"`
	ActivationMode       ActivationMode `json:"activation_mode"`
	ManifestURL          string         `json:"manifest_url"`
	BundleURL            string         `json:"bundle_url,omitempty"`
	RolloutPercent       int            `json:"rollout_percent"`
	ManifestSigningKeyID string         `json:"manifest_signing_key_id,omitempty"`
}

type UpdatePatchRolloutRequest struct {
	RolloutPercent int `json:"rollout_percent"`
}

type UpdatePatchTrackRequest struct {
	Track          string `json:"track"`
	RolloutPercent int    `json:"rollout_percent,omitempty"`
}

type PatchDescriptor struct {
	ID             string         `json:"id"`
	Number         int            `json:"number"`
	ReleaseID      string         `json:"release_id,omitempty"`
	RuntimeID      string         `json:"runtime_id,omitempty"`
	Channel        string         `json:"channel,omitempty"`
	Track          string         `json:"track,omitempty"`
	ManifestURL    string         `json:"manifest_url"`
	BundleURL      string         `json:"bundle_url,omitempty"`
	ActivationMode ActivationMode `json:"activation_mode"`
	Kind           PatchKind      `json:"kind"`
}

type PatchCheckRequest struct {
	AppID              string    `json:"app_id"`
	ReleaseID          string    `json:"release_id,omitempty"`
	ReleaseVersion     string    `json:"release_version,omitempty"`
	RuntimeID          string    `json:"runtime_id"`
	Channel            string    `json:"channel"`
	Track              string    `json:"track,omitempty"`
	CurrentPatchNumber int       `json:"current_patch_number"`
	ClientID           string    `json:"client_id"`
	Kind               PatchKind `json:"kind,omitempty"`
}

type PatchCheckResponse struct {
	PatchAvailable         bool             `json:"patch_available"`
	Patch                  *PatchDescriptor `json:"patch,omitempty"`
	RolledBackPatchNumbers []int            `json:"rolled_back_patch_numbers"`
}

type RuntimeEventKind string

const (
	RuntimeEventPatchInstallSuccess RuntimeEventKind = "patch_install_success"
	RuntimeEventPatchInstallFailure RuntimeEventKind = "patch_install_failure"
	RuntimeEventServerRollback      RuntimeEventKind = "server_rollback_applied"
)

type RuntimeEvent struct {
	Kind         RuntimeEventKind `json:"kind"`
	PatchNumber  *int             `json:"patch_number,omitempty"`
	PatchNumbers []int            `json:"patch_numbers,omitempty"`
}

type BootReportRequest struct {
	AppID             string         `json:"app_id"`
	ReleaseID         string         `json:"release_id,omitempty"`
	ReleaseVersion    string         `json:"release_version,omitempty"`
	RuntimeID         string         `json:"runtime_id"`
	Channel           string         `json:"channel"`
	Track             string         `json:"track,omitempty"`
	ClientID          string         `json:"client_id"`
	ActivePatchNumber *int           `json:"active_patch_number,omitempty"`
	Events            []RuntimeEvent `json:"events"`
}

type BootReportResponse struct {
	RolledBackPatchNumbers []int `json:"rolled_back_patch_numbers"`
}

type PatchHealth struct {
	PatchID             string           `json:"patch_id"`
	PatchNumber         int              `json:"patch_number"`
	SuccessCount        int              `json:"success_count"`
	FailureCount        int              `json:"failure_count"`
	SuccessfulClientIDs []string         `json:"successful_client_ids"`
	FailedClientIDs     []string         `json:"failed_client_ids"`
	LastEventKind       RuntimeEventKind `json:"last_event_kind,omitempty"`
	LastEventAt         time.Time        `json:"last_event_at,omitempty"`
	RolledBack          bool             `json:"rolled_back"`
}

// ToolchainArtifact is one hosted toolchain file (an archive/role inside a platform set), matching the
// T002 §7 per-artifact schema: name/url/sha256/size + role (build|patch) + kind (e.g. xcframework,
// gen_snapshot, dart2bytecode, dartaotruntime, vm_platform, archive). Hashes/sizes are transport
// integrity; the per-file uncompressed SHAs are re-verified by the CLI's unchanged verifyEngineBundle
// after extraction.
type ToolchainArtifact struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes uint64 `json:"size"`
	Role      string `json:"role"` // build | patch
	Kind      string `json:"kind"` // xcframework | gen_snapshot | dart2bytecode | dartaotruntime | vm_platform | archive
}

// ToolchainPlatform is the per-platform artifact set within a toolchain manifest, with the
// verifyEngineBundle-required arch + build_mode plus an honesty tier (tier != "production" => the
// toolchain is experimental; the proven engine is tier="experimental_profile").
type ToolchainPlatform struct {
	Platform   string              `json:"platform"`   // ios | android
	Arch       string              `json:"arch"`       // arm64, etc.
	BuildMode  string              `json:"build_mode"` // profile | release | experimental
	Tier       string              `json:"tier"`       // production | experimental_profile | ...
	EngineJSON string              `json:"engine_json,omitempty"`
	Artifacts  []ToolchainArtifact `json:"artifacts"`
}

// ToolchainVersion is the T002 §7 hosted toolchain manifest: a signed, version-keyed description of the
// build-time engine artifacts a developer must fetch. It is a NEW, separate trust domain (signed with a
// new toolchain key id, §4) — distinct from the device engine-lane pinned key and the backend
// PatchManifest signer. The registry stores the SIGNED manifest bytes VERBATIM in the object store and
// returns them byte-for-byte; this struct is the indexed projection for the toolchain_versions table.
type ToolchainVersion struct {
	SoroqToolchainVersion string              `json:"soroq_toolchain_version"`
	Platform              string              `json:"platform"` // primary platform (ios) — Platforms carries the full set
	Mode                  string              `json:"mode"`     // profile | release | experimental
	FlutterVersion        string              `json:"flutter_version"`
	FlutterRevision       string              `json:"flutter_revision"`
	DartRevision          string              `json:"dart_revision"`
	SoroqEngineRevision   string              `json:"soroq_engine_revision"`
	SigningKeyID          string              `json:"signing_key_id"`
	Platforms             []ToolchainPlatform `json:"platforms"`
	// ManifestObjectKey + ManifestSig are persistence/registry fields (not part of the signed body);
	// they are populated by the store on read so callers can locate + verify the bytes.
	ManifestObjectKey string    `json:"manifest_object_key,omitempty"`
	ManifestSig       string    `json:"manifest_sig,omitempty"`
	CreatedAt         time.Time `json:"created_at,omitempty"`
}

// PutToolchainRequest is the operator PUT envelope: the VERBATIM signed manifest bytes plus the
// detached Ed25519 hex signature over those exact bytes. The store persists ManifestBytes byte-for-byte
// (never re-marshaled) so the CLI's signature verification over the served bytes holds.
type PutToolchainRequest struct {
	ManifestBytes []byte `json:"manifest"`
	SignatureHex  string `json:"signature"`
}

// FrontendVersion is the indexed projection for the hosted FRONTEND registry (D1.2, additive). The hosted
// frontend manifest describes a prebuilt Soroq Flutter FRONTEND archive (the fork's flutter-sdk-src tree)
// a developer fetches so `resolveSoroqFlutterBin` no longer needs a manual SOROQ_FLUTTER_BIN. It reuses the
// SAME operator TOOLCHAIN signing key (no new trust anchor): the manifest is Ed25519-signed and the archive
// is SHA-256 + size pinned. The registry stores the SIGNED manifest bytes VERBATIM in the object store and
// returns them byte-for-byte; this struct is the indexed projection for the frontend_versions table. It is
// distinct from ToolchainVersion (whose schema is coupled to platform in {ios,android} engine bundles).
type FrontendVersion struct {
	SoroqFrontendVersion string `json:"soroq_frontend_version"`
	FlutterRevision      string `json:"flutter_revision"`
	DartRevision         string `json:"dart_revision"`
	EngineRevision       string `json:"engine_revision"`
	PatchsetSHA256       string `json:"patchset_sha256"`
	SigningKeyID         string `json:"signing_key_id"`
	// ManifestObjectKey + ManifestSig are persistence/registry fields (not part of the signed body);
	// they are populated by the store on read so callers can locate + verify the bytes.
	ManifestObjectKey string    `json:"manifest_object_key,omitempty"`
	ManifestSig       string    `json:"manifest_sig,omitempty"`
	CreatedAt         time.Time `json:"created_at,omitempty"`
}

// PutFrontendRequest is the operator PUT envelope for a frontend manifest: the VERBATIM signed manifest
// bytes plus the detached Ed25519 hex signature over those exact bytes. The store persists ManifestBytes
// byte-for-byte (never re-marshaled) so the CLI's signature verification over the served bytes holds.
type PutFrontendRequest struct {
	ManifestBytes []byte `json:"manifest"`
	SignatureHex  string `json:"signature"`
}

// FrontendArchive is the persistence metadata for a hosted FRONTEND ARCHIVE (D1.2, additive). The ~1 GB
// archive bytes are uploaded DIRECTLY to object storage (chunked) and finalized through the control plane;
// the public archive GET streams them back (chunk-by-chunk) or presign-redirects. The trust anchor stays
// the SIGNED manifest's archive.sha256 + size (verified by the CLI at install time); SHA256 here is
// transport metadata for the public GET.
type FrontendArchive struct {
	SoroqFrontendVersion string    `json:"soroq_frontend_version"`
	ObjectKey            string    `json:"object_key,omitempty"`
	SHA256               string    `json:"sha256"`
	SizeBytes            uint64    `json:"size_bytes"`
	ContentType          string    `json:"content_type,omitempty"`
	UploadedAt           time.Time `json:"uploaded_at,omitempty"`
}

// ToolchainArchive is the persistence metadata for a hosted toolchain ARCHIVE (T011, additive). The
// control plane stores the ~20MB archive bytes VERBATIM keyed by version (toolchains/<version>/archive.tar.gz)
// — exactly like the proven verbatim engine-artifact serve — so a fresh `soroq toolchain install` can fetch
// it from the control plane instead of an external host. The trust anchor stays the SIGNED manifest's
// archive.sha256 (verified by the CLI at install time); SHA256 here is transport metadata for the public GET.
type ToolchainArchive struct {
	SoroqToolchainVersion string    `json:"soroq_toolchain_version"`
	ObjectKey             string    `json:"object_key,omitempty"`
	SHA256                string    `json:"sha256"`
	SizeBytes             uint64    `json:"size_bytes"`
	ContentType           string    `json:"content_type,omitempty"`
	UploadedAt            time.Time `json:"uploaded_at,omitempty"`
}

// CLIAuthCode is a one-time PKCE authorization code minted by the website->backend authorize endpoint
// (Deliverable 2, browser-based CLI login). SECURITY: only the sha256 (hex) of the raw code is ever
// persisted — the raw code is returned to the caller once and never stored. The record is single-use
// (Used) with a short expiry (ExpiresAt = now+5min) and binds the PKCE code_challenge, opaque state,
// verified operator email, and the loopback redirect_uri the CLI listens on.
type CLIAuthCode struct {
	CodeSHA256    string
	CodeChallenge string
	State         string
	Email         string
	RedirectURI   string
	Scopes        []string
	ExpiresAt     time.Time
	Used          bool
}

// CLIToken is a per-user CLI bearer token minted by the exchange endpoint after a successful PKCE
// verification. SECURITY: only the sha256 (hex) of the raw token is ever persisted; the raw token is
// returned to the CLI once. A nil RevokedAt means active; a non-nil RevokedAt means revoked (rejected
// by whoami + requireOperator). requireOperator accepts EITHER the static operator token OR a valid,
// non-revoked CLIToken whose Email passes the same allowed/admin operator-eligibility checks.
type CLIToken struct {
	TokenSHA256 string
	Email       string
	Scopes      []string
	CreatedAt   time.Time
	RevokedAt   *time.Time
}
