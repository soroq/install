package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"soroq/backend/internal/domain"
)

// writeFirebaseProdCredential writes a stored Firebase operator credential whose
// api_base points at the (fake) prod control plane prodAPIBase. It returns the
// resolved config path. The credential carries a refresh token + api key so the
// command-time refresh path is eligible to fire.
func writeFirebaseProdCredential(t *testing.T, prodAPIBase string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := saveAuthConfig(configPath, authConfig{
		SchemaVersion:        1,
		CredentialKind:       credentialKindFirebase,
		APIBase:              prodAPIBase,
		OperatorEmail:        "owner@example.com",
		FirebaseIDToken:      "prod-id-token-original",
		FirebaseRefreshToken: "prod-refresh-token",
		FirebaseAPIKey:       "prod-api-key",
		FirebaseProjectID:    "prod-project",
	}); err != nil {
		t.Fatalf("saveAuthConfig() error = %v", err)
	}
	return configPath
}

// isolateHome points HOME (and SOROQ_CONFIG via clearOperatorEnv) at temp dirs so
// no code path can read or rewrite the developer's real ~/.soroq/config.json.
func isolateHome(t *testing.T) {
	t.Helper()
	clearOperatorEnv(t)
	t.Setenv("HOME", t.TempDir())
}

// TestLocalAPITargetDoesNotMutateProdCredential is the regression guard for the
// T004 footgun: a command pointed at a LOCAL/non-prod --api must NOT refresh or
// rewrite the stored prod credential. The stored config.json must be byte-for-byte
// unchanged after the local command runs.
func TestLocalAPITargetDoesNotMutateProdCredential(t *testing.T) {
	isolateHome(t)

	// A fake prod control plane host; the credential belongs to this host.
	prodAPIBase := "https://api.soroq.dev"
	configPath := writeFirebaseProdCredential(t, prodAPIBase)
	t.Setenv("SOROQ_CONFIG", configPath)

	// If the refresh path were (wrongly) taken, it would POST here. Fail loudly:
	// a local target must never reach the Firebase secure-token endpoint at all.
	firebase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("local target must NOT refresh the prod credential, but it called the Firebase token endpoint %s %s", r.Method, r.URL.Path)
	}))
	defer firebase.Close()
	t.Setenv("SOROQ_FIREBASE_SECURE_TOKEN_URL", firebase.URL)

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}

	// A LOCAL control plane (different host from the credential's api_base).
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.App{}); err != nil {
			t.Fatalf("Encode(apps) error = %v", err)
		}
	}))
	defer local.Close()

	if err := runAppList([]string{"--api", local.URL, "--json"}); err != nil {
		t.Fatalf("runAppList(local) error = %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("local --api command mutated the stored prod credential.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestLocalWhoamiDoesNotMutateProdCredential covers the whoami path, which calls
// currentOperatorCredentialsForRequest directly (not via applyOperatorHeaders).
func TestLocalWhoamiDoesNotMutateProdCredential(t *testing.T) {
	isolateHome(t)

	prodAPIBase := "https://api.soroq.dev"
	configPath := writeFirebaseProdCredential(t, prodAPIBase)
	t.Setenv("SOROQ_CONFIG", configPath)

	firebase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("local whoami must NOT refresh the prod credential, but it called %s %s", r.Method, r.URL.Path)
	}))
	defer firebase.Close()
	t.Setenv("SOROQ_FIREBASE_SECURE_TOKEN_URL", firebase.URL)

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.App{}); err != nil {
			t.Fatalf("Encode(apps) error = %v", err)
		}
	}))
	defer local.Close()

	if err := runWhoami([]string{"--api", local.URL, "--config", configPath, "--json"}); err != nil {
		t.Fatalf("runWhoami(local) error = %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("local whoami mutated the stored prod credential.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestProdAPITargetStillRefreshesCredential proves the fix is scoped, not a blanket
// disable: when the command actually targets the credential's own (prod) host, the
// short-lived Firebase ID token is still refreshed and the stored credential is
// rewritten as before.
func TestProdAPITargetStillRefreshesCredential(t *testing.T) {
	isolateHome(t)

	// Fake prod control plane. The credential's api_base must match this host so
	// the refresh gate opens.
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Apps verification used by whoami; the refreshed token must be attached.
		if got := r.Header.Get("Authorization"); got != "Bearer prod-id-token-refreshed" {
			t.Fatalf("expected refreshed token on the prod request, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.App{}); err != nil {
			t.Fatalf("Encode(apps) error = %v", err)
		}
	}))
	defer prod.Close()

	configPath := writeFirebaseProdCredential(t, prod.URL)
	t.Setenv("SOROQ_CONFIG", configPath)

	// Fake Firebase secure-token endpoint returns a fresh id token.
	firebase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id_token":      "prod-id-token-refreshed",
			"refresh_token": "prod-refresh-token-rotated",
		})
	}))
	defer firebase.Close()
	t.Setenv("SOROQ_FIREBASE_SECURE_TOKEN_URL", firebase.URL)

	if err := runWhoami([]string{"--api", prod.URL, "--config", configPath, "--json"}); err != nil {
		t.Fatalf("runWhoami(prod) error = %v", err)
	}

	updated, err := loadAuthConfig(configPath)
	if err != nil {
		t.Fatalf("loadAuthConfig(after) error = %v", err)
	}
	if updated.FirebaseIDToken != "prod-id-token-refreshed" {
		t.Fatalf("prod target should refresh the stored id token, got %q", updated.FirebaseIDToken)
	}
	if updated.FirebaseRefreshToken != "prod-refresh-token-rotated" {
		t.Fatalf("prod target should persist the rotated refresh token, got %q", updated.FirebaseRefreshToken)
	}
}

// TestApiTargetMatchesCredential locks the host-match rule the gate depends on.
func TestApiTargetMatchesCredential(t *testing.T) {
	cases := []struct {
		name   string
		target string
		cred   string
		want   bool
	}{
		{"same host scheme/port/trailing-slash insensitive", "https://API.Soroq.dev/", "https://api.soroq.dev/api", true},
		{"different host (local vs prod)", "http://127.0.0.1:8091", "https://api.soroq.dev", false},
		{"localhost vs prod", "http://localhost:8090", "https://api.soroq.dev", false},
		{"empty target means default-prod path", "", "https://api.soroq.dev", true},
		{"empty stored api_base cannot prove mismatch", "http://127.0.0.1:8091", "", true},
		{"bare host without scheme matches", "api.soroq.dev:8443", "https://api.soroq.dev", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := apiTargetMatchesCredential(tc.target, tc.cred); got != tc.want {
				t.Fatalf("apiTargetMatchesCredential(%q, %q) = %v, want %v", tc.target, tc.cred, got, tc.want)
			}
		})
	}
}
