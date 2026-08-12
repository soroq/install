package main

import (
	"strings"
	"testing"
)

// Regenerating the project seed used to leave TWO `<app>-project-signing` entries in soroq.yaml with
// different public keys -- the older one having no private half anywhere. The app then shipped
// trusting a key that could not sign, which only widens what the device accepts.

const trustYAMLOneKey = `app_id: com.example.app
channel: stable
runtime_id_strategy: manifest_trust_v1
manifest_trust:
  keyset_version: 1
  keys:
    - id: com.example.app-project-signing
      public_key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
    - id: hosted-key
      public_key: BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB
`

func TestAppendManifestTrustKeyReplacesSameIDInPlace(t *testing.T) {
	out := appendManifestTrustKey(trustYAMLOneKey, "com.example.app-project-signing", "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")
	if n := strings.Count(out, "id: com.example.app-project-signing"); n != 1 {
		t.Fatalf("regenerating the seed must REPLACE the entry, not add a second one; found %d entries:\n%s", n, out)
	}
	if strings.Contains(out, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") {
		t.Errorf("the superseded key must not remain trusted:\n%s", out)
	}
	if !strings.Contains(out, "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC") {
		t.Errorf("the new key must be present:\n%s", out)
	}
	// Unrelated keys are untouched.
	if !strings.Contains(out, "id: hosted-key") || !strings.Contains(out, "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB") {
		t.Errorf("an unrelated trusted key was disturbed:\n%s", out)
	}
}

func TestAppendManifestTrustKeyStillAddsNewIDs(t *testing.T) {
	out := appendManifestTrustKey(trustYAMLOneKey, "brand-new-id", "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD")
	for _, want := range []string{"id: brand-new-id", "id: com.example.app-project-signing", "id: hosted-key"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q to be present:\n%s", want, out)
		}
	}
}

func TestValidateExistingManifestTrustRejectsCollidingKeyIDs(t *testing.T) {
	colliding := []byte(`app_id: com.example.app
manifest_trust:
  keyset_version: 1
  keys:
    - id: com.example.app-project-signing
      public_key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
    - id: com.example.app-project-signing
      public_key: CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC
`)
	trust, err := parseSoroqManifestTrust(colliding)
	if err != nil || trust == nil {
		t.Fatalf("parse setup failed: trust=%v err=%v", trust, err)
	}
	_, err = validateExistingManifestTrust(trust)
	if err == nil {
		t.Fatal("two different public keys under one key id must be rejected, not silently trusted")
	}
	if !strings.Contains(err.Error(), "DIFFERENT public key") {
		t.Errorf("the error should explain the collision, got: %v", err)
	}
}

func TestValidateExistingManifestTrustAcceptsDistinctIDs(t *testing.T) {
	trust, err := parseSoroqManifestTrust([]byte(trustYAMLOneKey))
	if err != nil || trust == nil {
		t.Fatalf("parse setup failed: trust=%v err=%v", trust, err)
	}
	if _, err := validateExistingManifestTrust(trust); err != nil {
		t.Fatalf("a well-formed distinct-id trust block must validate: %v", err)
	}
}
