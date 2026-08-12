package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The EXACT set of replacement-ABI entry keys the soroq_flutter controller's `_flatSpecsFromAbi` reads.
// If the producer drifts from this set, real activations fail closed while both suites pass in isolation —
// this is the cross-component contract neither the Go nor the Dart suite otherwise covers.
var controllerParsedABIKeys = []string{
	"base_identity", "kind", "module_class", "module_library", "module_member", "stable_identity",
}

func newFreehandArtifact(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	buildFreehandArtifactFrom(t, dir, defaultDecls())
	return dir
}

func TestFreehandDeviceManifest_FieldsAndBinding(t *testing.T) {
	dir := newFreehandArtifact(t)
	m, bytecode, err := buildFreehandDeviceManifest(dir, 3, "soroq_freehand_v3.bytecode")
	if err != nil {
		t.Fatalf("build device manifest: %v", err)
	}
	// bytecode SHA is the payload SHA + both contract aliases.
	sum := sha256.Sum256(bytecode)
	want := hex.EncodeToString(sum[:])
	if m.BytecodeSha256 != want || m.PayloadSha256 != want || m.ModuleBytecodeSha256 != want {
		t.Fatalf("bytecode/payload SHA aliases not all equal to the bytecode SHA")
	}
	if m.EntrypointContract != freehandDeviceContract {
		t.Fatalf("wrong entrypointContract %q", m.EntrypointContract)
	}
	// logical id = the reproducible SOURCE artifact id.
	metaRaw, _ := os.ReadFile(filepath.Join(dir, "patch_artifact.json"))
	var meta FreehandPatchArtifactMeta
	json.Unmarshal(metaRaw, &meta)
	if m.LogicalArtifactID != meta.ArtifactID {
		t.Fatalf("logicalArtifactId %q != artifact_id %q", m.LogicalArtifactID, meta.ArtifactID)
	}
	if len(m.ReplacementABI) == 0 {
		t.Fatal("empty replacementAbi")
	}
}

// The producer must NOT put a numeric index in patches, so an OLD indexed client (which ignores
// entrypointContract) reaches its numeric-index path with no index and fails closed instead of misapplying.
func TestFreehandDeviceManifest_PatchesHaveNoIndex(t *testing.T) {
	dir := newFreehandArtifact(t)
	m, _, err := buildFreehandDeviceManifest(dir, 1, "m.bytecode")
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := json.Marshal(m)
	var raw map[string]any
	json.Unmarshal(mb, &raw)
	for _, p := range raw["patches"].([]any) {
		if _, hasIndex := p.(map[string]any)["index"]; hasIndex {
			t.Fatal("freehand patch entry must NOT carry a numeric index")
		}
	}
}

// Cross-component: the emitted replacement-ABI entry keys are EXACTLY the set the controller parses.
func TestFreehandDeviceManifest_ABIKeysMatchController(t *testing.T) {
	dir := newFreehandArtifact(t)
	m, _, err := buildFreehandDeviceManifest(dir, 1, "m.bytecode")
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := json.Marshal(m)
	var raw struct {
		ReplacementABI []map[string]any `json:"replacementAbi"`
	}
	json.Unmarshal(mb, &raw)
	for _, e := range raw.ReplacementABI {
		got := make([]string, 0, len(e))
		for k := range e {
			got = append(got, k)
		}
		sort.Strings(got)
		if len(got) != len(controllerParsedABIKeys) {
			t.Fatalf("ABI entry keys %v != controller-parsed %v", got, controllerParsedABIKeys)
		}
		for i := range got {
			if got[i] != controllerParsedABIKeys[i] {
				t.Fatalf("ABI entry keys %v != controller-parsed %v", got, controllerParsedABIKeys)
			}
		}
	}
}

// End-to-end contract: sign + serve on an in-process mock server; a device-equivalent client fetches +
// verifies Ed25519 (pinned key) over the exact manifest bytes + SHA-256 of the bytecode. No prod publish.
func TestFreehandDeviceManifest_SignServeVerifyRoundTrip(t *testing.T) {
	dir := newFreehandArtifact(t)
	m, bytecode, err := buildFreehandDeviceManifest(dir, 2, "soroq_freehand_v2.bytecode")
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, _ := json.Marshal(m)

	// ephemeral signing key (seed custody stays external; never persisted).
	_, priv, _ := ed25519.GenerateKey(nil)
	seed := priv.Seed()
	sigHex, err := signFreehandManifest(manifestBytes, encodeSeedRawURL(seed))
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)

	out := t.TempDir()
	if err := emitFreehandPayload(out, m, manifestBytes, sigHex, bytecode); err != nil {
		t.Fatal(err)
	}
	// serve the emitted payload verbatim (store-equivalent).
	srv := httptest.NewServer(http.FileServer(http.Dir(out)))
	defer srv.Close()
	get := func(name string) []byte {
		res, err := http.Get(srv.URL + "/" + name)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("GET %s: %v (%d)", name, err, res.StatusCode)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return b
	}
	// device-equivalent verification.
	servedManifest := get("manifest.json")
	servedSig, _ := hex.DecodeString(string(get("manifest.sig")))
	if !ed25519.Verify(pub, servedManifest, servedSig) {
		t.Fatal("device verify: signature over served manifest failed")
	}
	var served FreehandDeviceManifest
	json.Unmarshal(servedManifest, &served)
	servedBytecode := get(served.Patches[0].Bytecode)
	sum := sha256.Sum256(servedBytecode)
	if hex.EncodeToString(sum[:]) != served.BytecodeSha256 {
		t.Fatal("device verify: served bytecode SHA != manifest bytecodeSha256")
	}
	// tamper the served bytecode -> hash check must fail (fail-closed).
	if hex.EncodeToString(sum[:]) == served.BytecodeSha256 {
		bad := sha256.Sum256(append(servedBytecode, 0xFF))
		if hex.EncodeToString(bad[:]) == served.BytecodeSha256 {
			t.Fatal("tampered bytecode unexpectedly matched")
		}
	}
}

func TestValidateFreehandDeviceManifest_StrictRejects(t *testing.T) {
	dir := newFreehandArtifact(t)
	m, _, err := buildFreehandDeviceManifest(dir, 1, "m.bytecode")
	if err != nil {
		t.Fatal(err)
	}
	good, _ := json.Marshal(m)
	if err := validateFreehandDeviceManifest(good); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	edit := func(f func(map[string]any)) []byte {
		var mm map[string]any
		json.Unmarshal(good, &mm)
		f(mm)
		b, _ := json.Marshal(mm)
		return b
	}
	cases := map[string][]byte{
		"unknown top-level field": edit(func(mm map[string]any) { mm["evil"] = 1 }),
		"unknown ABI entry field": edit(func(mm map[string]any) { mm["replacementAbi"].([]any)[0].(map[string]any)["evil"] = 1 }),
		"wrong contract":          edit(func(mm map[string]any) { mm["entrypointContract"] = "indexed_selector_v1" }),
		"payload alias mismatch":  edit(func(mm map[string]any) { mm["payloadSha256"] = "0" }),
		"empty replacementAbi":    edit(func(mm map[string]any) { mm["replacementAbi"] = []any{} }),
		"trailing json":           append(append([]byte{}, good...), []byte("\n{}\n")...),
		"missing logical id":      edit(func(mm map[string]any) { delete(mm, "logicalArtifactId") }),
		"patch with index (indexed shape)": edit(func(mm map[string]any) {
			mm["patches"].([]any)[0].(map[string]any)["index"] = 0
		}),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateFreehandDeviceManifest(b); err == nil {
				t.Fatalf("strict validation ACCEPTED %q", name)
			}
		})
	}
}

func encodeSeedRawURL(seed []byte) string {
	return base64.RawURLEncoding.EncodeToString(seed)
}
