package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

// --- engine bundle fixtures ---

// writeEngineBundle writes a verifiable engine bundle dir whose engine.json hashes match the on-disk
// artifacts. mutate lets a test corrupt the manifest before it is written.
func writeEngineBundle(t *testing.T, mutate func(m *engineBundleManifest)) string {
	t.Helper()
	dir := t.TempDir()
	artifacts := map[string]string{}
	for _, name := range requiredEngineArtifacts {
		content := []byte("artifact-" + name + "-bytes")
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatalf("write artifact %s: %v", name, err)
		}
		sum := sha256.Sum256(content)
		artifacts[name] = hex.EncodeToString(sum[:])
	}
	m := engineBundleManifest{
		Schema:           engineBundleSchema,
		FlutterCommit:    "c9a6c48423",
		DartRevision:     "d684a576a6",
		SoroqPatchHashes: map[string]string{"0003-soroq-vm.patch": "deadbeef"},
		Arch:             "arm64",
		BuildMode:        "profile",
		IsDebug:          false,
		Tier:             "experimental_profile",
		Artifacts:        artifacts,
	}
	if mutate != nil {
		mutate(&m)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal engine.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "engine.json"), raw, 0o644); err != nil {
		t.Fatalf("write engine.json: %v", err)
	}
	return dir
}

func TestVerifyEngineBundleAcceptsValid(t *testing.T) {
	dir := writeEngineBundle(t, nil)
	ve, err := verifyEngineBundle(dir)
	if err != nil {
		t.Fatalf("expected valid bundle, got error: %v", err)
	}
	if !ve.Experimental {
		t.Errorf("expected experimental tier for experimental_profile")
	}
	if ve.FrameworkSHA != ve.Manifest.Artifacts["flutter_framework"] {
		t.Errorf("framework sha not surfaced")
	}
}

func TestVerifyEngineBundleRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m *engineBundleManifest)
		want   string
	}{
		{"wrong schema", func(m *engineBundleManifest) { m.Schema = "bogus" }, "schema"},
		{"debug engine", func(m *engineBundleManifest) { m.IsDebug = true }, "debug"},
		{"non profile/release", func(m *engineBundleManifest) { m.BuildMode = "jit_release" }, "build_mode"},
		{"missing flutter commit", func(m *engineBundleManifest) { m.FlutterCommit = "" }, "flutter_commit"},
		{"empty soroq patches", func(m *engineBundleManifest) { m.SoroqPatchHashes = nil }, "soroq_patch_hashes"},
		{"missing required artifact", func(m *engineBundleManifest) { delete(m.Artifacts, "dart2bytecode") }, "dart2bytecode"},
		{"artifact sha mismatch", func(m *engineBundleManifest) { m.Artifacts["gen_snapshot"] = "00ff" }, "mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeEngineBundle(t, tc.mutate)
			_, err := verifyEngineBundle(dir)
			if err == nil {
				t.Fatalf("expected refusal containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyEngineBundleRefusesDebugEvenIfReleaseMode(t *testing.T) {
	// is_debug=true must be refused regardless of build_mode (the unopt vsync DCHECK aborts on device).
	dir := writeEngineBundle(t, func(m *engineBundleManifest) {
		m.BuildMode = "release"
		m.IsDebug = true
	})
	if _, err := verifyEngineBundle(dir); err == nil || !strings.Contains(err.Error(), "debug") {
		t.Fatalf("expected debug refusal, got %v", err)
	}
}

// --- signing ---

func TestSignEngineManifestVerifiesWithPinnedKeyAndHidesSeed(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	seedB64 := base64.RawURLEncoding.EncodeToString(seed)
	manifest := engineLaneManifest{Version: 5, BytecodeSha256: "abc123", Patches: []engineLanePatch{{Index: 0, Bytecode: "pv.bytecode"}}}
	manifestBytes, _ := json.Marshal(manifest)

	sigHex, fp, err := signEngineManifest(manifestBytes, seedB64)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		t.Fatalf("sig not hex: %v", err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, manifestBytes, sig) {
		t.Fatalf("signature does not verify against the derived public key")
	}
	// fingerprint = first 16 hex chars of sha256(pub); must be stable + never the raw seed.
	sum := sha256.Sum256(pub)
	if fp != hex.EncodeToString(sum[:])[:16] {
		t.Fatalf("unexpected fingerprint %q", fp)
	}
	if strings.Contains(sigHex, seedB64) || strings.Contains(fp, seedB64) {
		t.Fatalf("seed leaked into signing output")
	}
}

func TestDecodeSeedRejectsBadInput(t *testing.T) {
	if _, err := decodeSeed(""); err == nil {
		t.Fatalf("expected error for empty seed")
	}
	if _, err := decodeSeed(base64.RawURLEncoding.EncodeToString([]byte("too-short"))); err == nil {
		t.Fatalf("expected error for wrong-length seed")
	}
	good := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize))
	if _, err := decodeSeed(good); err != nil {
		t.Fatalf("expected valid std-base64 seed to decode, got %v", err)
	}
}

// --- baseline release + immutability ---

func writeAppDill(t *testing.T) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "app.dill")
	content := []byte("deployed-app-dill-bytes")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("write app.dill: %v", err)
	}
	sum := sha256.Sum256(content)
	return p, hex.EncodeToString(sum[:])
}

func TestReleaseIOSEngineWritesImmutableBaselineAndRefusesMutation(t *testing.T) {
	dir := writeEngineBundle(t, nil)
	appDill, appDillSHA := writeAppDill(t)
	out := filepath.Join(t.TempDir(), "baseline.json")

	args := []string{"--engine-bundle", dir, "--app-dill", appDill, "--release-id", "rel-1", "--app-id", "com.example.app", "--out", out}
	if err := runReleaseIOSEngine(args); err != nil {
		t.Fatalf("release: %v", err)
	}
	var b engineLaneBaseline
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	if b.AppDillSHA256 != appDillSHA || b.ReleaseID != "rel-1" || !b.Experimental {
		t.Fatalf("unexpected baseline: %+v", b)
	}

	// re-running with the SAME engine+app is idempotent (equal baseline).
	if err := runReleaseIOSEngine(args); err != nil {
		t.Fatalf("idempotent re-release should succeed, got %v", err)
	}
	// re-running with a DIFFERENT app.dill for the same release id must be refused (immutable).
	appDill2, _ := writeAppDill(t)
	if err := os.WriteFile(appDill2, []byte("different-app-dill"), 0o644); err != nil {
		t.Fatal(err)
	}
	args2 := []string{"--engine-bundle", dir, "--app-dill", appDill2, "--release-id", "rel-1", "--app-id", "com.example.app", "--out", out}
	if err := runReleaseIOSEngine(args2); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("expected immutable-baseline refusal, got %v", err)
	}
}

func TestReleaseIOSEngineAPIRequiresRuntimeAndVersion(t *testing.T) {
	dir := writeEngineBundle(t, nil)
	appDill, _ := writeAppDill(t)
	out := filepath.Join(t.TempDir(), "baseline.json")
	// --api without --runtime-id/--version must be rejected before any network call.
	err := runReleaseIOSEngine([]string{
		"--engine-bundle", dir, "--app-dill", appDill, "--release-id", "rel-1", "--app-id", "com.example.app",
		"--out", out, "--api", "http://localhost:8080",
	})
	if err == nil || !strings.Contains(err.Error(), "--runtime-id") {
		t.Fatalf("expected --api identifier requirement, got %v", err)
	}
}

func TestEngineMatchesBaselineRejectsSkew(t *testing.T) {
	dir := writeEngineBundle(t, nil)
	ve, err := verifyEngineBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	good := engineLaneBaseline{
		FrameworkSHA256:  ve.FrameworkSHA,
		Dart2bytecodeSHA: ve.Manifest.Artifacts["dart2bytecode"],
		GenSnapshotSHA:   ve.Manifest.Artifacts["gen_snapshot"],
	}
	if err := engineMatchesBaseline(ve, good); err != nil {
		t.Fatalf("matching baseline should pass, got %v", err)
	}
	skew := good
	skew.FrameworkSHA256 = "00deadbeef"
	if err := engineMatchesBaseline(ve, skew); err == nil || !strings.Contains(err.Error(), "framework") {
		t.Fatalf("expected framework skew rejection, got %v", err)
	}
}

func TestLoadEngineLaneBaselineRejectsMissingFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(p, []byte(`{"release_id":"r"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEngineLaneBaseline(p); err == nil {
		t.Fatalf("expected missing-field error")
	}
}

// --- rollback bundle is device-verifiable ---

func TestRollbackIOSEngineEmitsSignedVersion0(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 9
	seedB64 := base64.RawURLEncoding.EncodeToString(seed)
	out := t.TempDir()
	if err := runRollbackIOSEngine([]string{"--seed-base64", seedB64, "--out", out}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if got := string(manifestBytes); got != `{"version":0,"bytecodeSha256":"","patches":[]}` {
		t.Fatalf("unexpected rollback manifest: %s", got)
	}
	sigHex, err := os.ReadFile(filepath.Join(out, "manifest.sig"))
	if err != nil {
		t.Fatalf("read sig: %v", err)
	}
	sig, err := hex.DecodeString(strings.TrimSpace(string(sigHex)))
	if err != nil {
		t.Fatalf("sig not hex: %v", err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, manifestBytes, sig) {
		t.Fatalf("rollback signature does not verify (device app would reject)")
	}
}

// --- API delivery: the engine bundle zip is well-formed; flag validation is enforced ---

func TestBuildEngineBundleZipRoundTrips(t *testing.T) {
	manifest := []byte(`{"version":2,"bytecodeSha256":"abc","patches":[{"index":0,"bytecode":"pv.bytecode"}]}`)
	sig := []byte("deadbeef")
	bytecode := []byte("compiled-bytes")
	zipBytes, err := buildEngineBundleZip(manifest, sig, "pv.bytecode", bytecode)
	if err != nil {
		t.Fatalf("build zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	got := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = b
	}
	// manifest bytes must round-trip BYTE-FOR-BYTE (the device verifies the signature over them).
	if !bytes.Equal(got["manifest.json"], manifest) {
		t.Fatalf("manifest not verbatim: %q", got["manifest.json"])
	}
	if !bytes.Equal(got["manifest.sig"], sig) {
		t.Fatalf("sig not verbatim")
	}
	if !bytes.Equal(got["pv.bytecode"], bytecode) {
		t.Fatalf("bytecode not verbatim")
	}
}

// TestPublishEngineBundleSendsRollout proves the CLI forwards the requested rollout percentage into
// the control-plane create-patch request (kind=ios_engine), e.g. --rollout 50 -> RolloutPercent=50.
func TestPublishEngineBundleSendsRollout(t *testing.T) {
	var gotRollout int
	var gotKind string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/patches" {
			var req domain.CreatePatchRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode create-patch: %v", err)
			}
			gotRollout = req.RolloutPercent
			gotKind = string(req.Kind)
			json.NewEncoder(w).Encode(domain.Patch{ID: req.ID, Number: 1, AppID: req.AppID, ReleaseID: req.ReleaseID, RuntimeID: req.RuntimeID, Channel: req.Channel, Kind: req.Kind})
			return
		}
		w.WriteHeader(http.StatusOK) // bundle upload
	}))
	defer srv.Close()

	host, err := publishEngineBundleViaAPI(srv.URL, "patch-x", "com.example.app", "rel-1", "rt-1", "stable", 50, []byte("zip"))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if gotRollout != 50 {
		t.Fatalf("expected RolloutPercent=50 in create-patch, got %d", gotRollout)
	}
	if gotKind != string(domain.PatchKindIOSEngine) {
		t.Fatalf("expected kind ios_engine, got %q", gotKind)
	}
	if !strings.Contains(host, "/v1/engine/com.example.app/stable") {
		t.Fatalf("unexpected device host url %q", host)
	}
}

func TestIsAlreadyExistsErrToleratesBothStores(t *testing.T) {
	for _, msg := range []string{
		`app "x" already exists`,                                                 // file store
		`400 Bad Request: {"error":"create app: ERROR: duplicate key value violates unique constraint \"apps_pkey\" (SQLSTATE 23505)"}`, // postgres
	} {
		if !isAlreadyExistsErr(errors.New(msg)) {
			t.Fatalf("expected already-exists tolerance for %q", msg)
		}
	}
	if isAlreadyExistsErr(errors.New("500 Internal Server Error: boom")) {
		t.Fatalf("must NOT treat an unrelated error as already-exists")
	}
}

func TestRollbackEngineBundleZipHasNoBytecode(t *testing.T) {
	zipBytes, err := buildEngineBundleZip([]byte(`{"version":0,"bytecodeSha256":"","patches":[]}`), []byte("sig"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if len(zr.File) != 2 {
		t.Fatalf("rollback bundle must have exactly manifest.json + manifest.sig, got %d entries", len(zr.File))
	}
}

func TestPatchIOSEngineAPIRequiresIdentifiers(t *testing.T) {
	dir := writeEngineBundle(t, nil)
	appDill, _ := writeAppDill(t)
	baseline := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(baseline, []byte(`{"release_id":"r","framework_sha256":"x","app_dill_sha256":"y"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	seedB64 := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SeedSize))
	// --api without --runtime-id/--patch-id must be rejected (no network call attempted).
	err := runPatchIOSEngine([]string{
		"--baseline", baseline, "--engine-bundle", dir, "--patch", "p.dart",
		"--app-dill", appDill, "--package-config", "pc.json", "--version", "2",
		"--seed-base64", seedB64, "--api", "http://localhost:8080",
	})
	if err == nil || !strings.Contains(err.Error(), "--runtime-id") {
		t.Fatalf("expected --api identifier requirement, got %v", err)
	}
}

func TestPatchIOSEngineRequiresOutOrAPI(t *testing.T) {
	dir := writeEngineBundle(t, nil)
	appDill, _ := writeAppDill(t)
	baseline := filepath.Join(t.TempDir(), "b.json")
	os.WriteFile(baseline, []byte(`{"release_id":"r","framework_sha256":"x","app_dill_sha256":"y"}`), 0o644)
	seedB64 := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SeedSize))
	err := runPatchIOSEngine([]string{
		"--baseline", baseline, "--engine-bundle", dir, "--patch", "p.dart",
		"--app-dill", appDill, "--package-config", "pc.json", "--version", "2",
		"--seed-base64", seedB64,
	})
	if err == nil || !strings.Contains(err.Error(), "--out") {
		t.Fatalf("expected --out/--api requirement, got %v", err)
	}
}

func TestPatchIOSEngineRequiresFreshVersion(t *testing.T) {
	dir := writeEngineBundle(t, nil)
	appDill, _ := writeAppDill(t)
	baseline := filepath.Join(t.TempDir(), "b.json")
	os.WriteFile(baseline, []byte(`{"release_id":"r","framework_sha256":"x","app_dill_sha256":"y"}`), 0o644)
	seedB64 := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SeedSize))
	err := runPatchIOSEngine([]string{
		"--baseline", baseline, "--engine-bundle", dir, "--patch", "p.dart",
		"--app-dill", appDill, "--package-config", "pc.json", "--version", "0",
		"--seed-base64", seedB64, "--out", t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "FRESH") {
		t.Fatalf("expected fresh-version error, got %v", err)
	}
}

// TestEnsureControlPlaneSetupCollisionVsIdempotent locks the release-registration fix: a create error
// is tolerated ONLY as a verified same-id re-register; a (app,version,platform,arch,channel) unique
// collision under a DIFFERENT release-id must FAIL CLEARLY (the prior bug masked it -> phantom release
// -> the patch lane 404'd). Regression for "register baseline; immediately patch; lookup succeeds".
func TestEnsureControlPlaneSetupCollisionVsIdempotent(t *testing.T) {
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "t")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "op@example.com")
	const dupBody = `{"error":"create release: ERROR: duplicate key value violates unique constraint \"releases_app_version_platform_arch_channel_uidx\" (SQLSTATE 23505)"}`

	newSrv := func(releasePOST func(w http.ResponseWriter), releaseGET int) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/apps", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		mux.HandleFunc("POST /v1/releases", func(w http.ResponseWriter, r *http.Request) { releasePOST(w) })
		mux.HandleFunc("GET /v1/releases/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(releaseGET) })
		return httptest.NewServer(mux)
	}

	t.Run("fresh release registers", func(t *testing.T) {
		srv := newSrv(func(w http.ResponseWriter) { w.WriteHeader(200); _, _ = w.Write([]byte(`{}`)) }, 200)
		defer srv.Close()
		if err := ensureControlPlaneSetup(srv.URL, "com.x", "X", "rel-1", "rt", "1.0.0", "stable"); err != nil {
			t.Fatalf("fresh release should register, got: %v", err)
		}
	})

	t.Run("same-id re-register is idempotent", func(t *testing.T) {
		srv := newSrv(func(w http.ResponseWriter) { w.WriteHeader(400); _, _ = w.Write([]byte(dupBody)) }, 200) // GET id -> 200 exists
		defer srv.Close()
		if err := ensureControlPlaneSetup(srv.URL, "com.x", "X", "rel-1", "rt", "1.0.0", "stable"); err != nil {
			t.Fatalf("same-id re-register should be idempotent, got: %v", err)
		}
	})

	t.Run("different-id collision fails clearly, not masked", func(t *testing.T) {
		srv := newSrv(func(w http.ResponseWriter) { w.WriteHeader(400); _, _ = w.Write([]byte(dupBody)) }, 404) // GET id -> 404 not this id
		defer srv.Close()
		err := ensureControlPlaneSetup(srv.URL, "com.x", "X", "rel-NEW", "rt", "1.0.0", "stable")
		if err == nil {
			t.Fatal("collision under a different release-id must FAIL, not be masked")
		}
		if !strings.Contains(err.Error(), "REFUSED") {
			t.Fatalf("collision error should be actionable (REFUSED...), got: %v", err)
		}
	})
}
