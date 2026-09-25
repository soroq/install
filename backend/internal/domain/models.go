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
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	OwnerEmail  string `json:"owner_email,omitempty"`
	// OrgID is empty for a personal-workspace app, which is what every app created before
	// organizations existed is. It is never a replacement for OwnerEmail; it is an addition.
	OrgID     string    `json:"org_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
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
	// OrgID creates the app inside an organization instead of the caller's personal workspace. Empty
	// keeps the historical behaviour, which is what every existing caller wants.
	OrgID string `json:"org_id,omitempty"`
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
	Track string `json:"track"`
	// NO omitempty. `omitempty` drops a zero, so `set-track --rollout 0` never reached the wire at all
	// and the server saw an absent field -- which it then defaulted to 100, publishing to every client
	// the operator was trying to exclude. Zero is a value this field must be able to carry.
	RolloutPercent int `json:"rollout_percent"`
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
	// Arch is the client's ABI (e.g. arm64-v8a). It is OPTIONAL for backward compatibility with
	// already-shipped clients, but it is what makes patch selection architecture-safe: runtime_id is
	// derived from trust + version and does NOT encode architecture, so without this an arm64 payload
	// can be offered to an armeabi-v7a device. When it is absent and the runtime id spans more than one
	// architecture, selection fails closed rather than guessing.
	Arch string `json:"arch,omitempty"`
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

	// FailureClass says WHY an install failed. Gate B2 asks for the signature/hash refusal rate, and
	// until this existed every failure arrived as an undifferentiated patch_install_failure — the
	// numerator that metric names could not be extracted from what production recorded, so the honest
	// answer was "cannot be measured" rather than a guess.
	//
	// `omitempty` and free-form on purpose: an older client sends nothing and still reports a failure
	// exactly as before, and a client that learns a new refusal reason does not need a server release
	// to report it. Unknown values are counted under their own key rather than dropped — a taxonomy
	// that silently discards what it does not recognise is how a gap like this one reappears.
	FailureClass string `json:"failure_class,omitempty"`

	// EventID and ReportedAt exist for ONE purpose: deciding whether an event may drive an AUTOMATIC
	// ROLLBACK. They are optional, and an event without them is still counted exactly as before -- a
	// fielded client that has never heard of them keeps reporting, because losing telemetry to tighten
	// a control would be trading the thing being measured for the measurement.
	//
	// EventID is the reporter's own identifier for this event. A second event carrying an id already
	// seen for this patch inside the freshness window is a REPLAY and cannot count again.
	EventID string `json:"event_id,omitempty"`
	// ReportedAt is when the reporter observed the event. An event older than the freshness window is
	// STALE: it may describe a patch that has since been superseded, and acting on it would withdraw
	// code from a fleet on the strength of something that stopped being true.
	ReportedAt *time.Time `json:"reported_at,omitempty"`
}

// Failure classes the runtime reports today. These are the values Soroq's own client emits; the field
// accepts others so a newer client is never silenced by an older server.
const (
	// The two B2 actually asks about.
	FailureClassSignatureRefused = "signature_refused"
	FailureClassHashMismatch     = "hash_mismatch"

	FailureClassDownloadFailed   = "download_failed"
	FailureClassBytecodeRejected = "bytecode_rejected"
	FailureClassActivationFailed = "activation_failed"
	// Reported when a client knows only that the install failed.
	FailureClassUnspecified = "unspecified"
)

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

	// Verified is set by the HTTP boundary from the CREDENTIAL on the request, never from the body.
	// `json:"-"` is the whole guarantee: a client that posts {"verified": true} cannot set it, because
	// the field does not participate in decoding at all.
	Verified bool `json:"-"`
}

type BootReportResponse struct {
	RolledBackPatchNumbers []int `json:"rolled_back_patch_numbers"`
}

type PatchHealth struct {
	PatchID             string   `json:"patch_id"`
	PatchNumber         int      `json:"patch_number"`
	SuccessCount        int      `json:"success_count"`
	FailureCount        int      `json:"failure_count"`
	SuccessfulClientIDs []string `json:"successful_client_ids"`
	FailedClientIDs     []string `json:"failed_client_ids"`
	// FailureClasses is the B2 histogram: failure class -> distinct clients reporting it. Counted by
	// CLIENT, not by event, to match FailureCount directly above — a device that retries and fails
	// five times is one failing device, and counting events would inflate a refusal rate by exactly
	// the retry behaviour that a refusal causes.
	FailureClasses map[string]int `json:"failure_classes,omitempty"`

	// PROVENANCE. POST /v1/boot-reports carries no device credential -- fielded apps have none to give
	// -- so anyone who knows an app_id, runtime_id and channel can post one. That is tolerable for
	// counting and NOT tolerable for deployment decisions: three anonymous failure reports used to be
	// enough to withdraw a production patch from every device.
	//
	// These are SUBSETS of the sets above, holding the clients whose report arrived with an operator
	// credential authorized for the app. Every report is still recorded -- losing telemetry is worse
	// than holding unverified telemetry -- but a deployment can now decide what it acts on.
	VerifiedSuccessfulClientIDs []string `json:"verified_successful_client_ids,omitempty"`
	VerifiedFailedClientIDs     []string `json:"verified_failed_client_ids,omitempty"`

	// ROLLBACK ELIGIBILITY IS NARROWER THAN PROVENANCE, and the two are kept apart on purpose.
	//
	// Verified means only "this report arrived with an operator credential authorized for the app".
	// Eligible means verified AND bound to the right release and version AND carrying an unseen event
	// id AND fresh. A credentialed but stale report is still VERIFIED -- saying otherwise would make
	// the analytics surface report a false thing about where the report came from -- and it is not
	// eligible to withdraw code from a fleet.
	RollbackEligibleSuccessfulClientIDs []string `json:"rollback_eligible_successful_client_ids,omitempty"`
	RollbackEligibleFailedClientIDs     []string `json:"rollback_eligible_failed_client_ids,omitempty"`

	// SeenEvents holds the event ids already counted for this patch, with when they were reported, so
	// a replay can be recognised. It is pruned to the freshness window rather than to a fixed length:
	// a count cap would let a busy patch evict a recent id, and a replay of that id would then pass
	// both the replay check and the freshness check. Pruning by the same window the freshness check
	// uses makes the two compose with no gap between them.
	SeenEvents []SeenRuntimeEvent `json:"seen_events,omitempty"`

	// Observed says whether ANY report has ever arrived for this patch. Zero successes and no reports
	// at all are different facts, and rendering the second as "0% adoption" is a delivery claim about
	// devices that were never heard from.
	Observed bool `json:"observed"`

	LastEventKind RuntimeEventKind `json:"last_event_kind,omitempty"`
	LastEventAt   time.Time        `json:"last_event_at,omitempty"`
	RolledBack    bool             `json:"rolled_back"`
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

// StagedToolchainArchive is a toolchain archive the control plane has received IN FULL and verified
// (size and SHA-256 both equal to what the operator declared) BEFORE any manifest names it. It lives at a
// content-addressed key and is publicly readable at /v1/toolchain-archives/sha256/<sha256>. Staging never
// makes a toolchain version visible; only a manifest publication naming this exact digest and size does.
type StagedToolchainArchive struct {
	SHA256      string `json:"sha256"`
	SizeBytes   uint64 `json:"size_bytes"`
	ObjectKey   string `json:"object_key,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// StagedArchive is a frontend archive the control plane holds at a content-addressed key after verifying
// its whole body against the declared size and SHA-256. It is publicly readable at
// /v1/frontend-archives/sha256/<sha256>; staging never makes a frontend version visible.
type StagedArchive struct {
	SHA256      string `json:"sha256"`
	SizeBytes   uint64 `json:"size_bytes"`
	ObjectKey   string `json:"object_key,omitempty"`
	ContentType string `json:"content_type,omitempty"`
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

// Role is a member's authority inside an organization.
//
// The order matters and is checked with AtLeast rather than by comparing strings at each call site: a
// permission test written as `role == "admin" || role == "owner"` silently omits whichever role is added
// next, which is how a viewer eventually gets to publish.
type Role string

const (
	RoleViewer    Role = "viewer"
	RoleDeveloper Role = "developer"
	RoleAdmin     Role = "admin"
	RoleOwner     Role = "owner"
)

func (r Role) rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleDeveloper:
		return 2
	case RoleAdmin:
		return 3
	case RoleOwner:
		return 4
	default:
		return 0 // an unknown role has NO authority; it must never outrank viewer
	}
}

// AtLeast reports whether r carries at least the authority of want. An unrecognised role is refused,
// so a typo or a future role added to the database cannot accidentally grant access.
func (r Role) AtLeast(want Role) bool {
	if r.rank() == 0 || want.rank() == 0 {
		return false
	}
	return r.rank() >= want.rank()
}

func (r Role) Valid() bool { return r.rank() > 0 }

// Organization owns apps on behalf of more than one person.
type Organization struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by,omitempty"`
}

// SeenRuntimeEvent is one event id already counted toward rollback eligibility, with its report time.
type SeenRuntimeEvent struct {
	EventID    string    `json:"event_id"`
	ReportedAt time.Time `json:"reported_at"`
}

// Membership is one person's role in one organization. Email is the identity used everywhere else in
// this schema, so it is the identity here too.
type Membership struct {
	OrgID     string    `json:"org_id"`
	Email     string    `json:"email"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
