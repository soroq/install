package domain

import "time"

type PatchKind string

const (
	PatchKindConfig                         PatchKind = "config"
	PatchKindAsset                          PatchKind = "asset"
	PatchKindExperimentalNativeAOT          PatchKind = "experimental_native_aot"
	PatchKindAssetPlusExperimentalNativeAOT PatchKind = "asset_plus_experimental_native_aot"
	PatchKindRuntimeManagedDart             PatchKind = "runtime_managed_dart"
	PatchKindAssetPlusRuntimeManagedDart    PatchKind = "asset_plus_runtime_managed_dart"
	LegacyPatchKindDartCode                 PatchKind = "dart_code"
	LegacyPatchKindAssetPlusDartCode        PatchKind = "asset_plus_dart_code"
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
		PatchKindAssetPlusRuntimeManagedDart:
		return true
	default:
		return false
	}
}

func (k PatchKind) Normalized() PatchKind {
	return NormalizePatchKind(k)
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

type App struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	OwnerEmail  string    `json:"owner_email,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Release struct {
	ID                   string    `json:"id"`
	AppID                string    `json:"app_id"`
	RuntimeID            string    `json:"runtime_id"`
	Version              string    `json:"version"`
	Platform             string    `json:"platform"`
	Arch                 string    `json:"arch"`
	Channel              string    `json:"channel"`
	ManifestSigningKeyID string    `json:"manifest_signing_key_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

type Patch struct {
	ID                   string         `json:"id"`
	AppID                string         `json:"app_id"`
	ReleaseID            string         `json:"release_id"`
	RuntimeID            string         `json:"runtime_id"`
	Number               int            `json:"number"`
	Channel              string         `json:"channel"`
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
}

type CreatePatchRequest struct {
	ID                   string         `json:"id"`
	AppID                string         `json:"app_id"`
	ReleaseID            string         `json:"release_id"`
	RuntimeID            string         `json:"runtime_id"`
	Channel              string         `json:"channel"`
	Kind                 PatchKind      `json:"kind"`
	ActivationMode       ActivationMode `json:"activation_mode"`
	ManifestURL          string         `json:"manifest_url"`
	BundleURL            string         `json:"bundle_url,omitempty"`
	RolloutPercent       int            `json:"rollout_percent"`
	ManifestSigningKeyID string         `json:"manifest_signing_key_id,omitempty"`
}

type PatchDescriptor struct {
	ID             string         `json:"id"`
	Number         int            `json:"number"`
	ManifestURL    string         `json:"manifest_url"`
	BundleURL      string         `json:"bundle_url,omitempty"`
	ActivationMode ActivationMode `json:"activation_mode"`
	Kind           PatchKind      `json:"kind"`
}

type PatchCheckRequest struct {
	AppID              string    `json:"app_id"`
	RuntimeID          string    `json:"runtime_id"`
	Channel            string    `json:"channel"`
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
	RuntimeID         string         `json:"runtime_id"`
	Channel           string         `json:"channel"`
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
