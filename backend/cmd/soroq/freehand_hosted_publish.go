package main

// Soroq freehand — hosted delivery through the EXISTING iOS engine control-plane lane.
//
// `soroq patch ios --engine --rollout N` (freehand mode) publishes the immutable freehand artifact
// as a Kind=ios_engine patch: it signs the exact freehand_identity_v1 device manifest with the
// PROJECT's established signing key (the gitignored .soroq/manifest_signing_key.seed created by the
// trust flow — the public half is the app's pinned engine key), packs the VERBATIM
// manifest.json + manifest.sig + bytecode into the engine bundle, registers the patch
// (POST /v1/patches) and uploads the bundle (POST /v1/patches/{id}/bundle). The store keeps an
// ios_engine bundle byte-identical and serves manifest/sig/bytecode verbatim to the device from
// /v1/engine/{app}/{channel}. Signing custody is NEVER surfaced in the beginner command;
// --seed-base64 remains only as a CI override.
//
// This file contains SOURCE + is exercised by LOCAL integration tests (freehand_hosted_publish_test.go
// boots the real backend store+router over httptest). It performs NO production deploy on its own.

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"soroq/backend/internal/domain"
	"soroq/backend/internal/serving"
)

// freehandSigningSeedRelPath is the project's established engine signing seed (same file the manifest
// trust flow scaffolds; its public half is the app's pinnedEnginePublicKeyHex).
const freehandSigningSeedRelPath = ".soroq/manifest_signing_key.seed"

// resolveFreehandSigningSeed returns the base64 Ed25519 seed used to sign the device manifest, WITHOUT
// exposing custody in the beginner command. Precedence: explicit --seed-base64 (CI override) > env
// SOROQ_ENGINE_SIGNING_SEED (CI override) > the project's .soroq/manifest_signing_key.seed (default,
// gitignored, mode 0600). Fails clearly when none is available.
func resolveFreehandSigningSeed(projectDir, seedFlag string) (string, error) {
	if s := strings.TrimSpace(seedFlag); s != "" {
		return s, nil
	}
	if s := strings.TrimSpace(os.Getenv("SOROQ_ENGINE_SIGNING_SEED")); s != "" {
		return s, nil
	}
	seedPath := filepath.Join(projectDir, filepath.FromSlash(freehandSigningSeedRelPath))
	b, err := os.ReadFile(seedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no engine signing key at %s; run `soroq release ios --engine --build` once (it scaffolds the project signing key), or set SOROQ_ENGINE_SIGNING_SEED / --seed-base64 for CI", seedPath)
		}
		return "", fmt.Errorf("read engine signing seed %s: %w", seedPath, err)
	}
	seed := strings.TrimSpace(string(b))
	if seed == "" {
		return "", fmt.Errorf("engine signing seed at %s is empty", seedPath)
	}
	return seed, nil
}

// buildFreehandEngineBundleZip packs the VERBATIM signed payload into the engine-lane bundle the store
// accepts + serves byte-identically: manifest.json (exact signed bytes), manifest.sig (hex),
// and the named bytecode file. Deterministic (fixed order, no timestamps beyond zip defaults).
func buildFreehandEngineBundleZip(manifestBytes []byte, sigHex, bytecodeName string, bytecodeBytes []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := []struct {
		name string
		data []byte
	}{
		{"manifest.json", manifestBytes},
		{"manifest.sig", []byte(sigHex)},
		{bytecodeName, bytecodeBytes},
	}
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(e.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// freehandPublishParams are the resolved inputs to a hosted freehand publish.
type freehandPublishParams struct {
	APIBase   string
	AppID     string
	ReleaseID string
	RuntimeID string
	Channel   string
	Track     string
	PatchID   string
	Rollout   int
	Version   int
}

// publishFreehandEngineBundle registers the ios_engine patch and uploads the verbatim bundle through
// the control-plane HTTP lane, authenticated with the stored operator credentials. Returns the created
// patch + the device host base (/v1/engine/{app}/{channel}).
func publishFreehandEngineBundle(p freehandPublishParams, manifestBytes []byte, sigHex, bytecodeName string, bytecodeBytes []byte) (domain.Patch, string, error) {
	var zero domain.Patch
	zipBytes, err := buildFreehandEngineBundleZip(manifestBytes, sigHex, bytecodeName, bytecodeBytes)
	if err != nil {
		return zero, "", fmt.Errorf("build engine bundle: %w", err)
	}
	// Shared gate: refuses missing/unusable credentials and refuses to send a stored credential to a
	// control plane it was not issued for. Registration and bundle upload both go through this, so the
	// publish path cannot drift from how `whoami` and the release lane authenticate.
	creds, err := requireOperatorCredentials("", p.APIBase, "publishing a hosted patch")
	if err != nil {
		return zero, "", fmt.Errorf("load operator credentials (run `soroq login`): %w", err)
	}

	base := strings.TrimRight(p.APIBase, "/")
	createReq := domain.CreatePatchRequest{
		ID:             p.PatchID,
		AppID:          p.AppID,
		ReleaseID:      p.ReleaseID,
		RuntimeID:      p.RuntimeID,
		Channel:        p.Channel,
		Track:          p.Track,
		Kind:           domain.PatchKindIOSEngine,
		ActivationMode: domain.ActivationNextColdStart,
		ManifestURL:    base + "/v1/patches/" + p.PatchID + "/manifest",
		BundleURL:      base + "/v1/patches/" + p.PatchID + "/bundle",
		RolloutPercent: p.Rollout,
	}
	patch, err := postJSONOperator(base+"/v1/patches", creds, createReq)
	if err != nil {
		return zero, "", fmt.Errorf("register ios_engine patch: %w", err)
	}
	if err := uploadEngineBundleOperator(base+"/v1/patches/"+patch.ID+"/bundle", creds, zipBytes); err != nil {
		return zero, "", fmt.Errorf("upload ios_engine bundle: %w", err)
	}
	// The DEVICE base is not the operator base. `base` here is the authenticated write host; a device
	// reading manifest.json from it gets 401. Report what a device actually fetches.
	return patch, serving.EngineChannelURL(base, p.AppID, p.Channel), nil
}

// postJSONOperator POSTs a JSON body with operator auth and decodes the created domain.Patch.
func postJSONOperator(url string, creds operatorCredentials, body any) (domain.Patch, error) {
	var zero domain.Patch
	raw, err := json.Marshal(body)
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyCredentialsHeaders(req, creds)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		if authErr := authFailureError(resp.StatusCode, creds, url, "registering the patch"); authErr != nil {
			return zero, authErr
		}
		return zero, fmt.Errorf("control plane returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var patch domain.Patch
	if err := json.Unmarshal(respBody, &patch); err != nil {
		return zero, fmt.Errorf("decode patch response: %w", err)
	}
	return patch, nil
}

// uploadEngineBundleOperator PUTs/POSTs the raw zip bundle with operator auth.
func uploadEngineBundleOperator(url string, creds operatorCredentials, zipBytes []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(zipBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/zip")
	applyCredentialsHeaders(req, creds)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		if authErr := authFailureError(resp.StatusCode, creds, url, "uploading the engine bundle"); authErr != nil {
			return authErr
		}
		return fmt.Errorf("bundle upload returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// freehandPublishRequested reports whether the freehand patch command asked to publish (an explicit
// --rollout or --publish). The default freehand flow stops at the immutable artifact.
func freehandPublishRequested(head []string) bool {
	if _, ok := flagValue(head, "rollout"); ok {
		return true
	}
	if _, ok := flagValue(head, "publish"); ok {
		return true
	}
	return false
}

// resolveFreehandPublishParams gathers + validates the publish inputs from the command flags + the
// computed plan. patch-id is auto-derived from the artifact when the beginner omits it.
// deploymentVersion is the MONOTONIC deployment number the caller already resolved against the
// control plane (see freehand_manifest_version.go). It must be passed in rather than defaulted here,
// because the default patch ID is derived from it: previously this function set Version = 1 and built
// `freehand-<artifact12>-v1`, and the caller only overwrote Version afterwards. Republishing
// byte-identical content therefore regenerated an IDENTICAL patch ID and collided on `patches_pkey` —
// even though the documented contract is that identical CONTENT may be redeployed at a new DEPLOYMENT
// version. Content identity (logical artifact id + payload sha) is unchanged by this; only the
// deployment identity advances.
func resolveFreehandPublishParams(head []string, appID, runtimeID, defaultChannel, artifactID string, deploymentVersion int) (freehandPublishParams, error) {
	var p freehandPublishParams
	p.APIBase = strings.TrimSpace(firstNonEmptyFlag(head, "api"))
	if p.APIBase == "" {
		p.APIBase = defaultAPIBase()
	}
	p.AppID = appID
	p.RuntimeID = runtimeID
	p.ReleaseID = strings.TrimSpace(firstNonEmptyFlag(head, "release-id"))
	if p.ReleaseID == "" {
		return p, errors.New("hosted freehand publish requires --release-id (the iOS engine-lane release the base app was registered as)")
	}
	p.Channel = strings.TrimSpace(firstNonEmptyFlag(head, "channel"))
	if p.Channel == "" {
		p.Channel = strings.TrimSpace(defaultChannel)
	}
	if p.Channel == "" {
		p.Channel = "stable"
	}
	p.Track = strings.TrimSpace(firstNonEmptyFlag(head, "track"))

	// The caller owns version resolution: it queries the control plane for the scope's existing
	// patches, applies --patch-version if given, and runs validateManifestVersion. Re-deriving it here
	// would give two sources of truth for the number the patch ID is built from.
	if deploymentVersion <= 0 {
		return p, fmt.Errorf("internal: deployment version must be positive, got %d", deploymentVersion)
	}
	p.Version = deploymentVersion

	p.Rollout = 100
	if rs := strings.TrimSpace(firstNonEmptyFlag(head, "rollout")); rs != "" {
		r, err := strconv.Atoi(rs)
		if err != nil || r < 1 || r > 100 {
			return p, fmt.Errorf("--rollout must be an integer 1..100, got %q", rs)
		}
		p.Rollout = r
	}

	p.PatchID = strings.TrimSpace(firstNonEmptyFlag(head, "patch-id"))
	if p.PatchID == "" {
		short := artifactID
		if len(short) > 12 {
			short = short[:12]
		}
		p.PatchID = fmt.Sprintf("freehand-%s-v%d", short, p.Version)
	}
	return p, nil
}

// firstNonEmptyFlag returns the value of the named flag if present, else "".
func firstNonEmptyFlag(args []string, name string) string {
	if v, ok := flagValue(args, name); ok {
		return v
	}
	return ""
}

// freehandPublishAPIBase resolves the control plane a publish will target, using exactly the same rule
// as resolveFreehandPublishParams. It exists so the pre-build credential gate checks the SAME host the
// upload will use — a gate that checked a different host would pass and then fail at upload.
func freehandPublishAPIBase(head []string) string {
	if v := strings.TrimSpace(firstNonEmptyFlag(head, "api")); v != "" {
		return v
	}
	return defaultAPIBase()
}
