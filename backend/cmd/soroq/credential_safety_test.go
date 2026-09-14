package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PLANTED CONTROLS for credential safety. Each one is a defect this product must not have, written as a
// test that fails if the defect returns.

// ORIGIN SWAPPING. The defect that was actually present: the loader guarded only the REFRESH of a
// stored credential and returned it anyway on a mismatch, so every command that did not go through
// requireOperatorCredentials (release, doctor, engine-lane, toolchain-publish) could send a production
// token to any --api it was pointed at.
func TestStoredCredentialIsRefusedForAForeignOrigin(t *testing.T) {
	cfg := seedStoredCredential(t, "https://api.soroq.dev", "SECRET-PRODUCTION-TOKEN")
	creds, err := currentOperatorCredentialsForRequest(cfg, "https://evil.example")
	if err == nil {
		t.Fatal("a stored credential was returned for a foreign origin; it can be sent there")
	}
	if !isCredentialOriginMismatch(err) {
		t.Errorf("the refusal must be typed so callers cannot swallow it as 'no credential', got %T", err)
	}
	if strings.Contains(creds.Token, "SECRET-PRODUCTION-TOKEN") {
		t.Error("the refused credential was still returned to the caller")
	}
}

func TestStoredCredentialIsAcceptedForItsOwnOrigin(t *testing.T) {
	// The negative twin. A guard that refuses everything is as useless as one that refuses nothing.
	cfg := seedStoredCredential(t, "https://api.soroq.dev", "SECRET-PRODUCTION-TOKEN")
	creds, err := currentOperatorCredentialsForRequest(cfg, "https://api.soroq.dev")
	if err != nil {
		t.Fatalf("a credential was refused for its OWN origin: %v", err)
	}
	if creds.Token == "" {
		t.Error("no token returned for the matching origin")
	}
}

// Scheme, port, case and trailing slash must not defeat the match, or the guard is trivially bypassed.
func TestOriginMatchIgnoresSchemePortCaseAndTrailingSlash(t *testing.T) {
	for _, tc := range []struct{ cred, target string }{
		{"https://api.soroq.dev", "https://API.SOROQ.DEV"},
		{"https://api.soroq.dev", "https://api.soroq.dev/"},
		{"https://api.soroq.dev", "http://api.soroq.dev"},
	} {
		if !apiTargetMatchesCredential(tc.target, tc.cred) {
			t.Errorf("same host treated as foreign: cred=%s target=%s", tc.cred, tc.target)
		}
	}
	for _, tc := range []struct{ cred, target string }{
		{"https://api.soroq.dev", "https://api.soroq.dev.evil.example"},
		{"https://api.soroq.dev", "https://soroq.dev"},
		{"https://api.soroq.dev", "http://127.0.0.1:8080"},
	} {
		if apiTargetMatchesCredential(tc.target, tc.cred) {
			t.Errorf("a DIFFERENT host was treated as the same: cred=%s target=%s", tc.cred, tc.target)
		}
	}
}

// TOKEN LEAKAGE. A credential must never reach argv, and the refusal message must not quote it.
func TestRefusalMessageDoesNotQuoteTheToken(t *testing.T) {
	err := &credentialOriginMismatchError{CredentialOrigin: "https://api.soroq.dev", TargetOrigin: "https://evil.example"}
	if strings.Contains(err.Error(), "TOKEN") || strings.Contains(err.Error(), "Bearer") {
		t.Error("the refusal message carries token material")
	}
	if !strings.Contains(err.Error(), "api.soroq.dev") || !strings.Contains(err.Error(), "evil.example") {
		t.Error("the refusal message must name both origins so the developer can act on it")
	}
}

// EXPIRED CREDENTIAL. An empty token is not a usable credential and must not read as signed in.
func TestEmptyStoredTokenIsNotTreatedAsAuthenticated(t *testing.T) {
	cfg := seedStoredCredential(t, "https://api.soroq.dev", "")
	creds, err := currentOperatorCredentialsForRequest(cfg, "https://api.soroq.dev")
	if err == nil && strings.TrimSpace(creds.Token) != "" {
		t.Error("an empty stored token was returned as usable")
	}
}

// The isolation this package depends on: no test may read the developer's real credential.
func TestTestHomeIsIsolatedFromTheRealCredential(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	if !strings.Contains(home, "soroq-cli-test-home-") {
		t.Fatalf("tests are running against the real HOME (%s); an authenticated test would read the "+
			"developer's production credential and behave differently in CI", home)
	}
}

func seedStoredCredential(t *testing.T, apiBase, token string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := saveAuthConfig(cfg, authConfig{
		SchemaVersion:  1,
		CredentialKind: credentialKindCLIToken,
		APIBase:        apiBase,
		OperatorEmail:  "owner@example.com",
		CLIToken:       token,
		UpdatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return cfg
}
