package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

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

func TestRunLoginBrowserCallbackStoresFirebaseCredentials(t *testing.T) {
	clearOperatorEnv(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer firebase-id-token" {
			t.Fatalf("expected Firebase Authorization header, got %q", got)
		}
		if got := r.Header.Get("X-Soroq-Operator-Email"); got != "" {
			t.Fatalf("Firebase credentials should not forward operator email header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.App{
			{ID: "com.example.app", DisplayName: "Example App"},
		}); err != nil {
			t.Fatalf("Encode(apps) error = %v", err)
		}
	}))
	defer server.Close()

	oldOpenBrowserURL := openBrowserURL
	openBrowserURL = func(rawURL string) error {
		t.Helper()
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Errorf("Parse(loginURL) error = %v", err)
			return err
		}
		callbackURL := parsed.Query().Get("cli_login_callback")
		state := parsed.Query().Get("cli_login_state")
		if callbackURL == "" || state == "" {
			t.Errorf("expected callback URL and state in %q", rawURL)
			return nil
		}
		go func() {
			body := `{"state":` + quoteJSONString(state) + `,"idToken":"firebase-id-token","refreshToken":"refresh-token","email":"Owner@Example.com","apiKey":"firebase-api-key","projectId":"project-123"}`
			resp, err := http.Post(callbackURL, "application/json", strings.NewReader(body))
			if err != nil {
				t.Errorf("POST(callback) error = %v", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected callback HTTP 200, got %d", resp.StatusCode)
			}
		}()
		return nil
	}
	defer func() {
		openBrowserURL = oldOpenBrowserURL
	}()

	loginStdout := captureStdout(t, func() {
		err := runLogin([]string{
			"--api", server.URL,
			"--hosted-surface", "https://hosted.example",
			"--config", configPath,
			"--json",
		})
		if err != nil {
			t.Fatalf("runLogin() error = %v", err)
		}
	})
	var login authLoginSummary
	if err := json.Unmarshal([]byte(loginStdout), &login); err != nil {
		t.Fatalf("Unmarshal(login) error = %v; stdout=%q", err, loginStdout)
	}
	if login.CredentialKind != credentialKindFirebase || !login.Verified || login.AppCount != 1 {
		t.Fatalf("expected verified Firebase browser login, got %+v", login)
	}
	if login.APIBase != server.URL || login.HostedSurfaceURL != "https://hosted.example" {
		t.Fatalf("unexpected browser login endpoints: %+v", login)
	}

	config, err := loadAuthConfig(configPath)
	if err != nil {
		t.Fatalf("loadAuthConfig() error = %v", err)
	}
	if config.CredentialKind != credentialKindFirebase || config.FirebaseIDToken != "firebase-id-token" || config.FirebaseRefreshToken != "refresh-token" {
		t.Fatalf("expected Firebase credentials in config, got %+v", config)
	}
	if config.OperatorToken != "" {
		t.Fatalf("browser login must not store operator token, got %+v", config)
	}
	if requestCount != 1 {
		t.Fatalf("expected one verification request, got %d", requestCount)
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
	if !strings.Contains(err.Error(), "not logged in") {
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

func quoteJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
