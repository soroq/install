package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

// forceFileKeychainFallback makes storeCLIToken/resolveCLITokenValue use the
// 0600 config.json fallback instead of the developer's real macOS login keychain.
func forceFileKeychainFallback(t *testing.T) {
	t.Helper()
	prev := keychainAvailableFn
	keychainAvailableFn = func() bool { return false }
	t.Cleanup(func() { keychainAvailableFn = prev })
}

// useFakeKeychain injects an in-memory keychain so the keychain-preferred path is
// testable without touching the real login keychain.
func useFakeKeychain(t *testing.T) map[string]string {
	t.Helper()
	store := map[string]string{}
	pa, ps, pr, pd := keychainAvailableFn, keychainStoreFn, keychainReadFn, keychainDeleteFn
	keychainAvailableFn = func() bool { return true }
	keychainStoreFn = func(apiBase, token string) error { store[apiBase] = token; return nil }
	keychainReadFn = func(apiBase string) (string, error) {
		tok, ok := store[apiBase]
		if !ok {
			return "", fmt.Errorf("keychain: not found for %s", apiBase)
		}
		return tok, nil
	}
	keychainDeleteFn = func(apiBase string) error { delete(store, apiBase); return nil }
	t.Cleanup(func() {
		keychainAvailableFn, keychainStoreFn, keychainReadFn, keychainDeleteFn = pa, ps, pr, pd
	})
	return store
}

func TestRunLoginStoresCredentialsAndWhoamiVerifies(t *testing.T) {
	clearOperatorEnv(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cli-secret" {
			t.Fatalf("expected Authorization header from login token, got %q", got)
		}
		if got := r.Header.Get("X-Soroq-Operator-Email"); got != "owner@example.com" {
			t.Fatalf("expected normalized operator email header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.App{
			{ID: "com.example.app", DisplayName: "Example App"},
			{ID: "com.example.other", DisplayName: "Other App"},
		}); err != nil {
			t.Fatalf("Encode(apps) error = %v", err)
		}
	}))
	defer server.Close()

	loginStdout := captureStdout(t, func() {
		err := runLogin([]string{
			"--api", server.URL,
			"--config", configPath,
			"--email", "Owner@Example.com",
			"--token", "cli-secret",
			"--json",
		})
		if err != nil {
			t.Fatalf("runLogin() error = %v", err)
		}
	})
	if strings.Contains(loginStdout, "cli-secret") {
		t.Fatalf("login output leaked token: %q", loginStdout)
	}
	var login authLoginSummary
	if err := json.Unmarshal([]byte(loginStdout), &login); err != nil {
		t.Fatalf("Unmarshal(login) error = %v; stdout=%q", err, loginStdout)
	}
	if !login.Verified || login.AppCount != 2 {
		t.Fatalf("expected verified login with app count, got %+v", login)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat(config) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected config permissions 0600, got %o", got)
	}

	whoamiStdout := captureStdout(t, func() {
		err := runWhoami([]string{
			"--api", server.URL,
			"--config", configPath,
			"--json",
		})
		if err != nil {
			t.Fatalf("runWhoami() error = %v", err)
		}
	})
	var whoami authWhoamiSummary
	if err := json.Unmarshal([]byte(whoamiStdout), &whoami); err != nil {
		t.Fatalf("Unmarshal(whoami) error = %v; stdout=%q", err, whoamiStdout)
	}
	if whoami.Source != "config" || !whoami.Verified || whoami.AppCount != 2 {
		t.Fatalf("expected verified config whoami, got %+v", whoami)
	}
	if requestCount != 2 {
		t.Fatalf("expected login and whoami verification requests, got %d", requestCount)
	}
}

func TestStoredLoginCredentialsAreUsedByControlPlaneCommands(t *testing.T) {
	clearOperatorEnv(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := saveAuthConfig(configPath, authConfig{
		SchemaVersion: 1,
		APIBase:       "https://example.invalid",
		OperatorEmail: "owner@example.com",
		OperatorToken: "stored-secret",
	}); err != nil {
		t.Fatalf("saveAuthConfig() error = %v", err)
	}
	t.Setenv("SOROQ_CONFIG", configPath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer stored-secret" {
			t.Fatalf("expected stored Authorization header, got %q", got)
		}
		if got := r.Header.Get("X-Soroq-Operator-Email"); got != "owner@example.com" {
			t.Fatalf("expected stored operator email, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.App{}); err != nil {
			t.Fatalf("Encode(apps) error = %v", err)
		}
	}))
	defer server.Close()

	if err := runAppList([]string{"--api", server.URL, "--json"}); err != nil {
		t.Fatalf("runAppList() error = %v", err)
	}
}

func TestRunLoginBrowserExchangeStoresCLIToken(t *testing.T) {
	clearOperatorEnv(t)
	forceFileKeychainFallback(t)
	configPath := filepath.Join(t.TempDir(), "config.json")

	var gotCode, gotVerifier string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/cli/auth/exchange" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Code         string `json:"code"`
			CodeVerifier string `json:"code_verifier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode exchange body: %v", err)
		}
		gotCode = body.Code
		gotVerifier = body.CodeVerifier
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(cliExchangeResponse{
			Token: "cli-bearer-token", Email: "Owner@Example.com", TokenType: "bearer",
		}); err != nil {
			t.Fatalf("Encode(exchange) error = %v", err)
		}
	}))
	defer server.Close()

	var gotChallenge string
	oldOpenBrowserURL := openBrowserURL
	openBrowserURL = func(rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Errorf("Parse(loginURL) error = %v", err)
			return err
		}
		if parsed.Path != "/cli/login" {
			t.Errorf("expected /cli/login, got %q", parsed.Path)
		}
		redirect := parsed.Query().Get("redirect_uri")
		state := parsed.Query().Get("state")
		gotChallenge = parsed.Query().Get("code_challenge")
		if redirect == "" || state == "" || gotChallenge == "" {
			t.Errorf("expected redirect_uri, state, code_challenge in %q", rawURL)
			return nil
		}
		go func() {
			resp, err := http.Get(redirect + "?code=one-time-code&state=" + url.QueryEscape(state))
			if err != nil {
				t.Errorf("GET(callback) error = %v", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected callback HTTP 200, got %d", resp.StatusCode)
			}
		}()
		return nil
	}
	defer func() { openBrowserURL = oldOpenBrowserURL }()

	loginStdout := captureStdout(t, func() {
		err := runLogin([]string{
			"--api", server.URL,
			"--hosted-surface", "https://hosted.example",
			"--config", configPath,
			"--email", "Owner@Example.com",
			"--json",
		})
		if err != nil {
			t.Fatalf("runLogin() error = %v", err)
		}
	})
	if strings.Contains(loginStdout, "cli-bearer-token") {
		t.Fatalf("login output leaked token: %q", loginStdout)
	}
	var login authLoginSummary
	if err := json.Unmarshal([]byte(loginStdout), &login); err != nil {
		t.Fatalf("Unmarshal(login) error = %v; stdout=%q", err, loginStdout)
	}
	if login.CredentialKind != credentialKindCLIToken || !login.Verified {
		t.Fatalf("expected verified cli_token login, got %+v", login)
	}
	if login.APIBase != server.URL || login.Email != "owner@example.com" {
		t.Fatalf("unexpected cli_token login summary: %+v", login)
	}
	if gotCode != "one-time-code" {
		t.Fatalf("exchange received code %q, want one-time-code", gotCode)
	}
	if gotVerifier == "" || pkceChallenge(gotVerifier) != gotChallenge {
		t.Fatalf("PKCE mismatch: challenge=%q sha256(verifier)=%q", gotChallenge, pkceChallenge(gotVerifier))
	}

	config, err := loadAuthConfig(configPath)
	if err != nil {
		t.Fatalf("loadAuthConfig() error = %v", err)
	}
	if config.CredentialKind != credentialKindCLIToken || config.CLIToken != "cli-bearer-token" || config.TokenInKeychain {
		t.Fatalf("expected file-fallback cli_token in config, got %+v", config)
	}
	if config.OperatorToken != "" || config.FirebaseIDToken != "" {
		t.Fatalf("cli_token login must not store operator/firebase tokens, got %+v", config)
	}
}

func TestExchangeStoresCLITokenSendsBearerWithoutEmail(t *testing.T) {
	clearOperatorEnv(t)
	forceFileKeychainFallback(t)
	configPath := filepath.Join(t.TempDir(), "config.json")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/cli/auth/exchange" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(cliExchangeResponse{Token: "cli-bearer", Email: "owner@example.com"}); err != nil {
			t.Fatalf("Encode(exchange) error = %v", err)
		}
	}))
	defer server.Close()

	exchanged, err := exchangeCLICode(server.URL, "code123", "verifier123")
	if err != nil {
		t.Fatalf("exchangeCLICode() error = %v", err)
	}
	if exchanged.Token != "cli-bearer" {
		t.Fatalf("unexpected exchange token")
	}
	inKeychain, err := storeCLIToken(configPath, server.URL, exchanged.Email, exchanged.Scopes, exchanged.Token)
	if err != nil {
		t.Fatalf("storeCLIToken() error = %v", err)
	}
	if inKeychain {
		t.Fatalf("expected 0600 file fallback, got keychain")
	}

	creds, err := currentOperatorCredentialsForRequest(configPath, server.URL)
	if err != nil {
		t.Fatalf("currentOperatorCredentialsForRequest() error = %v", err)
	}
	if creds.CredentialKind != credentialKindCLIToken || creds.Token != "cli-bearer" {
		t.Fatalf("expected resolved cli_token, got %+v", creds)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/apps", nil)
	applyCredentialsHeaders(req, creds)
	if got := req.Header.Get("Authorization"); got != "Bearer cli-bearer" {
		t.Fatalf("expected Bearer cli-bearer, got %q", got)
	}
	if got := req.Header.Get("X-Soroq-Operator-Email"); got != "" {
		t.Fatalf("cli_token must not send operator email header, got %q", got)
	}
}

func TestStoreCLITokenPrefersKeychainWithMetadataOnlyConfig(t *testing.T) {
	clearOperatorEnv(t)
	store := useFakeKeychain(t)
	configPath := filepath.Join(t.TempDir(), "config.json")

	inKeychain, err := storeCLIToken(configPath, "https://api.soroq.dev", "owner@example.com", nil, "secret-token")
	if err != nil {
		t.Fatalf("storeCLIToken() error = %v", err)
	}
	if !inKeychain {
		t.Fatalf("expected keychain storage")
	}
	if store["https://api.soroq.dev"] != "secret-token" {
		t.Fatalf("token not written to keychain: %+v", store)
	}
	config, err := loadAuthConfig(configPath)
	if err != nil {
		t.Fatalf("loadAuthConfig() error = %v", err)
	}
	if config.CLIToken != "" {
		t.Fatalf("token must not be in config.json when keychain is used")
	}
	if !config.TokenInKeychain || config.CredentialKind != credentialKindCLIToken {
		t.Fatalf("expected keychain metadata in config, got %+v", config)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat(config) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected config permissions 0600, got %o", got)
	}

	creds, err := currentOperatorCredentials(configPath)
	if err != nil {
		t.Fatalf("currentOperatorCredentials() error = %v", err)
	}
	if creds.Token != "secret-token" || creds.CredentialKind != credentialKindCLIToken {
		t.Fatalf("expected keychain-backed cli_token, got %+v", creds)
	}
}

func TestRunLogoutCLITokenRevokesAndDeletesKeychain(t *testing.T) {
	clearOperatorEnv(t)
	store := useFakeKeychain(t)
	configPath := filepath.Join(t.TempDir(), "config.json")

	var revoked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/cli/auth/revoke" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("expected bearer on revoke, got %q", got)
		}
		revoked = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"revoked":true}`))
	}))
	defer server.Close()

	if _, err := storeCLIToken(configPath, server.URL, "owner@example.com", nil, "secret-token"); err != nil {
		t.Fatalf("storeCLIToken() error = %v", err)
	}

	stdout := captureStdout(t, func() {
		if err := runLogout([]string{"--api", server.URL, "--config", configPath, "--json"}); err != nil {
			t.Fatalf("runLogout() error = %v", err)
		}
	})
	if strings.Contains(stdout, "secret-token") {
		t.Fatalf("logout output leaked token: %q", stdout)
	}
	if !revoked {
		t.Fatalf("expected server-side revoke call")
	}
	if _, ok := store[server.URL]; ok {
		t.Fatalf("expected keychain entry deleted, still present: %+v", store)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("expected config removed, stat error = %v", err)
	}
}

func TestBrowserLoginDefaultSurfaceUsesProductDomain(t *testing.T) {
	rawURL, err := buildBrowserLoginURL("", "http://127.0.0.1:1234/callback", "state-123", "challenge", "", "")
	if err != nil {
		t.Fatalf("buildBrowserLoginURL() error = %v", err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("Parse(loginURL) error = %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "soroq.dev" || parsed.Path != "/cli/login" {
		t.Fatalf("expected default product-domain login URL, got %q", rawURL)
	}
	if got := parsed.Query().Get("api"); got != defaultControlPlaneAPI {
		t.Fatalf("expected default api base %q, got %q", defaultControlPlaneAPI, got)
	}
}

func TestStoredFirebaseCredentialsAreUsedByControlPlaneCommands(t *testing.T) {
	clearOperatorEnv(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := saveAuthConfig(configPath, authConfig{
		SchemaVersion:   1,
		CredentialKind:  credentialKindFirebase,
		APIBase:         "https://hosted.example/api",
		OperatorEmail:   "owner@example.com",
		FirebaseIDToken: "firebase-id-token",
	}); err != nil {
		t.Fatalf("saveAuthConfig() error = %v", err)
	}
	t.Setenv("SOROQ_CONFIG", configPath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer firebase-id-token" {
			t.Fatalf("expected stored Firebase Authorization header, got %q", got)
		}
		if got := r.Header.Get("X-Soroq-Operator-Email"); got != "" {
			t.Fatalf("Firebase credentials should not forward operator email header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.App{}); err != nil {
			t.Fatalf("Encode(apps) error = %v", err)
		}
	}))
	defer server.Close()

	if err := runAppList([]string{"--api", server.URL, "--json"}); err != nil {
		t.Fatalf("runAppList() error = %v", err)
	}
}

func TestEnvironmentCredentialsOverrideStoredLogin(t *testing.T) {
	clearOperatorEnv(t)
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "env-secret")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "env@example.com")
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := saveAuthConfig(configPath, authConfig{
		SchemaVersion: 1,
		OperatorEmail: "stored@example.com",
		OperatorToken: "stored-secret",
	}); err != nil {
		t.Fatalf("saveAuthConfig() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer env-secret" {
			t.Fatalf("expected env Authorization header, got %q", got)
		}
		if got := r.Header.Get("X-Soroq-Operator-Email"); got != "env@example.com" {
			t.Fatalf("expected env operator email, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.App{}); err != nil {
			t.Fatalf("Encode(apps) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runWhoami([]string{
			"--api", server.URL,
			"--config", configPath,
			"--json",
		})
		if err != nil {
			t.Fatalf("runWhoami() error = %v", err)
		}
	})
	var whoami authWhoamiSummary
	if err := json.Unmarshal([]byte(stdout), &whoami); err != nil {
		t.Fatalf("Unmarshal(whoami) error = %v", err)
	}
	if whoami.Source != "environment" || whoami.Email != "env@example.com" {
		t.Fatalf("expected environment credentials, got %+v", whoami)
	}
}

func TestRunLogoutRemovesStoredCredentials(t *testing.T) {
	clearOperatorEnv(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := saveAuthConfig(configPath, authConfig{
		SchemaVersion: 1,
		OperatorEmail: "owner@example.com",
		OperatorToken: "stored-secret",
	}); err != nil {
		t.Fatalf("saveAuthConfig() error = %v", err)
	}

	stdout := captureStdout(t, func() {
		err := runLogout([]string{"--config", configPath, "--json"})
		if err != nil {
			t.Fatalf("runLogout() error = %v", err)
		}
	})
	var logout authLogoutSummary
	if err := json.Unmarshal([]byte(stdout), &logout); err != nil {
		t.Fatalf("Unmarshal(logout) error = %v", err)
	}
	if !logout.Removed {
		t.Fatalf("expected logout to remove config, got %+v", logout)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("expected config to be removed, stat error = %v", err)
	}
	err := runWhoami([]string{"--config", configPath, "--offline"})
	if err == nil {
		t.Fatalf("expected whoami to fail after logout")
	}
	if !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("expected not logged in guidance, got %v", err)
	}
}

func TestRunLoginTokenSourcesAreMutuallyExclusive(t *testing.T) {
	clearOperatorEnv(t)
	err := runLogin([]string{
		"--email", "owner@example.com",
		"--token", "one",
		"--token-file", filepath.Join(t.TempDir(), "token.txt"),
		"--skip-verify",
	})
	if err == nil {
		t.Fatalf("expected mutually-exclusive token source error")
	}
	if !strings.Contains(err.Error(), "only one") {
		t.Fatalf("expected token source guidance, got %v", err)
	}
}

func clearOperatorEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "")
	t.Setenv("SOROQ_OPERATOR_TOKEN", "")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "")
	t.Setenv("SOROQ_API", "")
	t.Setenv("SOROQ_CONFIG", filepath.Join(t.TempDir(), "missing-config.json"))
}
