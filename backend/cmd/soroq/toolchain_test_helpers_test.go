package main

// Shared toolchain-key test helpers. Deliberately UNTAGGED (present in both the default public build and
// the `soroq_operator` build) because they are used by public-build tests (frontend_install_safety_test.go,
// toolchain_keygen_test.go) AND the operator-only publish tests (toolchain_cmd_test.go). They depend only
// on internal/signing (no api/store/artifacts), so keeping them in the default build is boundary-safe.

import (
	"testing"

	"soroq/backend/internal/signing"
)

// testToolchainSeedB64 is the RETIRED genesis/test seed. Its pubkey was the pinned key BEFORE the T008
// production rotation; it no longer matches toolchainPinnedPublicKeyHex. It is kept ONLY so the negative
// tests can prove the rotated pinned key REFUSES it (publish self-check + the flipped guard). Accept-path
// tests no longer sign with it — they use usePinnedToolchainKey (an ephemeral, never-committed keypair).
const testToolchainSeedB64 = "vUpLfa4YNYorUr10lgWYXFZ-6lvW_rYf0-gnF8jUK6I"

// usePinnedToolchainKey installs an EPHEMERAL toolchain keypair as the in-process pinned key for the
// duration of one test (restored on cleanup), exercising the production accessor seam
// (pinnedToolchainPublicKeyHex). It returns the ephemeral seed (base64, held in memory only — never
// committed) so the test can sign manifests that verify against this freshly-pinned key. This keeps the
// accept-path tests OFF the retired committed seed: the production const stays the rotated prod pubkey.
func usePinnedToolchainKey(t *testing.T) (seedB64 string) {
	t.Helper()
	seed, pubHex, _, err := signing.GenerateToolchainKeyPair()
	if err != nil {
		t.Fatalf("GenerateToolchainKeyPair: %v", err)
	}
	prev := toolchainPinnedPublicKeyHexOverride
	toolchainPinnedPublicKeyHexOverride = pubHex
	t.Cleanup(func() { toolchainPinnedPublicKeyHexOverride = prev })
	return seed
}
