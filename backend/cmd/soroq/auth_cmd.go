package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"soroq/backend/internal/domain"
)

type authConfig struct {
	SchemaVersion        int       `json:"schema_version"`
	CredentialKind       string    `json:"credential_kind,omitempty"`
	APIBase              string    `json:"api_base,omitempty"`
	HostedSurfaceURL     string    `json:"hosted_surface_url,omitempty"`
	OperatorEmail        string    `json:"operator_email,omitempty"`
	OperatorToken        string    `json:"operator_token,omitempty"`
	FirebaseIDToken      string    `json:"firebase_id_token,omitempty"`
	FirebaseRefreshToken string    `json:"firebase_refresh_token,omitempty"`
	FirebaseAPIKey       string    `json:"firebase_api_key,omitempty"`
	FirebaseProjectID    string    `json:"firebase_project_id,omitempty"`
	// CLIToken holds the browser-login (OAuth+PKCE) bearer token when the macOS
	// Keychain is unavailable; when TokenInKeychain is true the raw token lives in
	// the Keychain (service "soroq-cli-token:<api_base>") and this stays empty.
	CLIToken        string    `json:"cli_token,omitempty"`
	TokenInKeychain bool      `json:"token_in_keychain,omitempty"`
	Scopes          []string  `json:"scopes,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type operatorCredentials struct {
	Token                string
	Email                string
	APIBase              string
	HostedSurfaceURL     string
	CredentialKind       string
	FirebaseRefreshToken string
	FirebaseAPIKey       string
	FirebaseProjectID    string
	Source               string
	ConfigPath           string
}

type authLoginSummary struct {
	APIBase          string `json:"api_base"`
	HostedSurfaceURL string `json:"hosted_surface_url,omitempty"`
	Email            string `json:"email,omitempty"`
	ConfigPath       string `json:"config_path"`
	CredentialKind   string `json:"credential_kind,omitempty"`
	TokenStored      bool   `json:"token_stored"`
	Verified         bool   `json:"verified"`
	AppCount         int    `json:"app_count,omitempty"`
}

type authWhoamiSummary struct {
	APIBase          string `json:"api_base"`
	HostedSurfaceURL string `json:"hosted_surface_url,omitempty"`
	Email            string `json:"email,omitempty"`
	Source           string `json:"source"`
	ConfigPath       string `json:"config_path,omitempty"`
	CredentialKind   string `json:"credential_kind,omitempty"`
	TokenPresent     bool   `json:"token_present"`
	Verified         bool   `json:"verified"`
	AppCount         int    `json:"app_count,omitempty"`
}

type authLogoutSummary struct {
	ConfigPath                    string `json:"config_path"`
	Removed                       bool   `json:"removed"`
	EnvironmentCredentialsPresent bool   `json:"environment_credentials_present"`
}

const (
	credentialKindOperatorToken = "operator_token"
	credentialKindFirebase      = "firebase_id_token"
	credentialKindCLIToken      = "cli_token"
)

func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	hostedSurface := fs.String("hosted-surface", defaultHostedSurfaceURL, "hosted operator surface URL for browser login")
	email := fs.String("email", "", "operator email")
	token := fs.String("token", "", "operator token")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	tokenStdin := fs.Bool("token-stdin", false, "read the operator token from stdin")
	configPath := fs.String("config", "", "credential config path")
	skipVerify := fs.Bool("skip-verify", false, "store credentials without calling the control plane")
	noOpen := fs.Bool("no-open", false, "print the browser login URL without opening it")
	callbackTimeout := fs.Duration("callback-timeout", 3*time.Minute, "browser login callback timeout")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq login [--hosted-surface https://soroq.dev] [--config ~/.soroq/config.json] [--json]
       soroq login --email operator@example.com (--token <token> | --token-file ./token.txt | --token-stdin) [--api https://api.soroq.dev] [--config ~/.soroq/config.json] [--skip-verify] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	resolvedConfigPath, err := resolveSoroqConfigPath(*configPath)
	if err != nil {
		return err
	}
	resolvedEmail := normalizeOperatorEmail(*email)
	resolvedToken, err := readLoginToken(*token, *tokenFile, *tokenStdin)
	if err != nil {
		return err
	}

	resolvedHostedSurface := strings.TrimRight(strings.TrimSpace(*hostedSurface), "/")
	if resolvedHostedSurface == "" {
		resolvedHostedSurface = defaultHostedSurfaceURL
	}
	resolvedAPIBase := strings.TrimRight(strings.TrimSpace(*apiBase), "/")
	if resolvedAPIBase == "" {
		resolvedAPIBase = defaultControlPlaneAPI
	}

	if resolvedToken == "" {
		if *skipVerify {
			return errors.New("--skip-verify is only supported with token login; browser login must complete the hosted callback")
		}
		// The OAuth+PKCE browser flow targets the control-plane API directly
		// (defaultControlPlaneAPI when --api is not set); the hosted surface only
		// hosts the authorize page.
		return runBrowserLogin(browserLoginOptions{
			APIBase:          resolvedAPIBase,
			HostedSurfaceURL: resolvedHostedSurface,
			EmailHint:        resolvedEmail,
			ConfigPath:       resolvedConfigPath,
			NoOpen:           *noOpen,
			Timeout:          *callbackTimeout,
			JSONOut:          *jsonOut,
		})
	}

	summary := authLoginSummary{
		APIBase:        resolvedAPIBase,
		Email:          resolvedEmail,
		ConfigPath:     resolvedConfigPath,
		CredentialKind: credentialKindOperatorToken,
		TokenStored:    true,
	}
	if !*skipVerify {
		appCount, err := verifyOperatorCredentials(resolvedAPIBase, operatorCredentials{
			Token:          resolvedToken,
			Email:          resolvedEmail,
			CredentialKind: credentialKindOperatorToken,
		})
		if err != nil {
			return fmt.Errorf("login verification failed: %w", err)
		}
		summary.Verified = true
		summary.AppCount = appCount
	}

	if err := saveAuthConfig(resolvedConfigPath, authConfig{
		SchemaVersion:  1,
		CredentialKind: credentialKindOperatorToken,
		APIBase:        resolvedAPIBase,
		OperatorEmail:  resolvedEmail,
		OperatorToken:  resolvedToken,
		UpdatedAt:      time.Now().UTC(),
	}); err != nil {
		return err
	}

	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	if summary.Verified {
		fmt.Fprintf(os.Stdout, "Logged in to %s", summary.APIBase)
		if summary.Email != "" {
			fmt.Fprintf(os.Stdout, " as %s", summary.Email)
		}
		fmt.Fprintln(os.Stdout)
		fmt.Fprintf(os.Stdout, "Verified access to %d app(s).\n", summary.AppCount)
	} else {
		fmt.Fprintf(os.Stdout, "Stored Soroq credentials for %s\n", summary.APIBase)
		fmt.Fprintln(os.Stdout, "Verification skipped.")
	}
	fmt.Fprintf(os.Stdout, "Config: %s\n", summary.ConfigPath)
	return nil
}

func runWhoami(args []string) error {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", "", "control plane base URL")
	configPath := fs.String("config", "", "credential config path")
	offline := fs.Bool("offline", false, "only inspect local credentials without calling the control plane")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq whoami [--api https://api.soroq.dev] [--config ~/.soroq/config.json] [--offline] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	// The effective control-plane target. An explicit --api scopes the credential
	// refresh so a local/non-prod whoami never rewrites the stored prod credential.
	targetAPIBase := strings.TrimRight(strings.TrimSpace(*apiBase), "/")
	creds, err := currentOperatorCredentialsForRequest(*configPath, targetAPIBase)
	if err != nil {
		return err
	}
	if strings.TrimSpace(creds.Token) == "" {
		// Primary guidance is browser login; SOROQ_CONTROL_PLANE_OPERATOR_TOKEN
		// remains a supported fallback (still honored by the credential resolver).
		base := targetAPIBase
		if base == "" {
			base = defaultControlPlaneAPI
		}
		return fmt.Errorf("Not logged in. Run: soroq login --api %s", base)
	}

	resolvedAPIBase := targetAPIBase
	if resolvedAPIBase == "" {
		resolvedAPIBase = strings.TrimRight(strings.TrimSpace(creds.APIBase), "/")
	}
	if resolvedAPIBase == "" {
		resolvedAPIBase = defaultControlPlaneAPI
	}

	// Browser-login (cli_token) credentials verify against the dedicated CLI auth
	// endpoint, not /v1/apps. Handle them in their own branch so the operator_token
	// and firebase whoami behavior below stays byte-for-byte unchanged.
	if normalizeCredentialKind(creds.CredentialKind, creds.Token) == credentialKindCLIToken {
		return runWhoamiCLIToken(resolvedAPIBase, creds, *offline, *jsonOut)
	}

	summary := authWhoamiSummary{
		APIBase:          resolvedAPIBase,
		HostedSurfaceURL: creds.HostedSurfaceURL,
		Email:            creds.Email,
		Source:           creds.Source,
		ConfigPath:       creds.ConfigPath,
		CredentialKind:   normalizeCredentialKind(creds.CredentialKind, creds.Token),
		TokenPresent:     creds.Token != "",
	}
	if !*offline {
		appCount, err := verifyOperatorCredentials(resolvedAPIBase, creds)
		if err != nil {
			return fmt.Errorf("whoami verification failed: %w", err)
		}
		summary.Verified = true
		summary.AppCount = appCount
	}

	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "API: %s\n", summary.APIBase)
	if summary.Email != "" {
		fmt.Fprintf(os.Stdout, "operator: %s\n", summary.Email)
	} else {
		fmt.Fprintln(os.Stdout, "operator: token-only")
	}
	fmt.Fprintf(os.Stdout, "source: %s\n", summary.Source)
	if summary.CredentialKind != "" {
		fmt.Fprintf(os.Stdout, "credential: %s\n", summary.CredentialKind)
	}
	if summary.HostedSurfaceURL != "" {
		fmt.Fprintf(os.Stdout, "hosted_surface: %s\n", summary.HostedSurfaceURL)
	}
	if summary.ConfigPath != "" {
		fmt.Fprintf(os.Stdout, "config: %s\n", summary.ConfigPath)
	}
	if summary.Verified {
		fmt.Fprintf(os.Stdout, "verified: yes (%d app(s) visible)\n", summary.AppCount)
	} else {
		fmt.Fprintln(os.Stdout, "verified: no (offline)")
	}
	return nil
}

// runWhoamiCLIToken verifies a browser-login (cli_token) credential against
// GET /v1/cli/auth/whoami and prints the email + credential kind. The bearer
// token is never printed.
func runWhoamiCLIToken(apiBase string, creds operatorCredentials, offline bool, jsonOut bool) error {
	summary := authWhoamiSummary{
		APIBase:          apiBase,
		HostedSurfaceURL: creds.HostedSurfaceURL,
		Email:            creds.Email,
		Source:           creds.Source,
		ConfigPath:       creds.ConfigPath,
		CredentialKind:   credentialKindCLIToken,
		TokenPresent:     creds.Token != "",
	}
	if !offline {
		who, err := fetchCLIWhoami(apiBase, creds.Token)
		if err != nil {
			return fmt.Errorf("whoami verification failed: %w", err)
		}
		if strings.TrimSpace(who.Email) != "" {
			summary.Email = normalizeOperatorEmail(who.Email)
		}
		if strings.TrimSpace(who.Kind) != "" {
			summary.CredentialKind = strings.TrimSpace(who.Kind)
		}
		summary.Verified = true
	}

	if jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "API: %s\n", summary.APIBase)
	if summary.Email != "" {
		fmt.Fprintf(os.Stdout, "operator: %s\n", summary.Email)
	} else {
		fmt.Fprintln(os.Stdout, "operator: token-only")
	}
	fmt.Fprintf(os.Stdout, "source: %s\n", summary.Source)
	fmt.Fprintf(os.Stdout, "credential: %s\n", summary.CredentialKind)
	if summary.ConfigPath != "" {
		fmt.Fprintf(os.Stdout, "config: %s\n", summary.ConfigPath)
	}
	if summary.Verified {
		fmt.Fprintln(os.Stdout, "verified: yes")
	} else {
		fmt.Fprintln(os.Stdout, "verified: no (offline)")
	}
	return nil
}

func runLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", "", "control plane base URL for cli_token revocation")
	configPath := fs.String("config", "", "credential config path")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq logout [--api https://api.soroq.dev] [--config ~/.soroq/config.json] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	resolvedConfigPath, err := resolveSoroqConfigPath(*configPath)
	if err != nil {
		return err
	}

	// For a browser-login (cli_token) credential, best-effort revoke it server-side
	// and delete it from the Keychain before removing the config entry. Operator and
	// firebase credentials keep the historical "just remove the config file" behavior.
	if config, loadErr := loadAuthConfig(resolvedConfigPath); loadErr == nil {
		if normalizeCredentialKind(config.CredentialKind, config.CLIToken) == credentialKindCLIToken {
			revokeAPIBase := strings.TrimRight(strings.TrimSpace(*apiBase), "/")
			if revokeAPIBase == "" {
				revokeAPIBase = strings.TrimRight(strings.TrimSpace(config.APIBase), "/")
			}
			if token := resolveCLITokenValue(config); token != "" {
				_ = revokeCLIToken(revokeAPIBase, token)
			}
			if config.TokenInKeychain {
				_ = keychainDeleteFn(strings.TrimRight(strings.TrimSpace(config.APIBase), "/"))
			}
		}
	}

	err = os.Remove(resolvedConfigPath)
	removed := true
	if errors.Is(err, os.ErrNotExist) {
		removed = false
	} else if err != nil {
		return err
	}
	summary := authLogoutSummary{
		ConfigPath:                    resolvedConfigPath,
		Removed:                       removed,
		EnvironmentCredentialsPresent: envOperatorToken() != "",
	}

	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	if removed {
		fmt.Fprintf(os.Stdout, "Removed Soroq credentials from %s\n", summary.ConfigPath)
	} else {
		fmt.Fprintf(os.Stdout, "No stored Soroq credentials found at %s\n", summary.ConfigPath)
	}
	if summary.EnvironmentCredentialsPresent {
		fmt.Fprintln(os.Stdout, "Environment credentials are still active for this shell.")
	}
	return nil
}

func readLoginToken(token string, tokenFile string, tokenStdin bool) (string, error) {
	sources := 0
	if strings.TrimSpace(token) != "" {
		sources++
	}
	if strings.TrimSpace(tokenFile) != "" {
		sources++
	}
	if tokenStdin {
		sources++
	}
	if sources > 1 {
		return "", errors.New("pass only one of --token, --token-file, or --token-stdin")
	}
	switch {
	case strings.TrimSpace(token) != "":
		return strings.TrimSpace(token), nil
	case strings.TrimSpace(tokenFile) != "":
		bytes, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(bytes)), nil
	case tokenStdin:
		bytes, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(bytes)), nil
	default:
		return "", nil
	}
}

func verifyOperatorCredentials(apiBase string, creds operatorCredentials) (int, error) {
	req, err := http.NewRequest(http.MethodGet, verificationAppsURL(apiBase, creds), nil)
	if err != nil {
		return 0, err
	}
	applyCredentialsHeaders(req, creds)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(respBody))
		if message == "" {
			message = resp.Status
		}
		return 0, fmt.Errorf("request failed: %s", message)
	}
	var apps []domain.App
	if err := json.Unmarshal(respBody, &apps); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}
	return len(apps), nil
}

func verificationAppsURL(apiBase string, creds operatorCredentials) string {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}
	if normalizeCredentialKind(creds.CredentialKind, creds.Token) == credentialKindFirebase &&
		strings.HasSuffix(base, "/api") {
		return base + "/operator/apps"
	}
	return base + "/v1/apps"
}

func applyCredentialsHeaders(req *http.Request, creds operatorCredentials) {
	if strings.TrimSpace(creds.Token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(creds.Token))
	}
	if normalizeCredentialKind(creds.CredentialKind, creds.Token) == credentialKindOperatorToken && strings.TrimSpace(creds.Email) != "" {
		req.Header.Set("X-Soroq-Operator-Email", strings.TrimSpace(creds.Email))
	}
}

func currentOperatorCredentials(configPath string) (operatorCredentials, error) {
	if token := envOperatorToken(); token != "" {
		return operatorCredentials{
			Token:          token,
			Email:          strings.TrimSpace(os.Getenv("SOROQ_OPERATOR_EMAIL")),
			APIBase:        strings.TrimRight(strings.TrimSpace(os.Getenv("SOROQ_API")), "/"),
			CredentialKind: credentialKindOperatorToken,
			Source:         "environment",
		}, nil
	}

	resolvedConfigPath, err := resolveSoroqConfigPath(configPath)
	if err != nil {
		return operatorCredentials{}, err
	}
	config, err := loadAuthConfig(resolvedConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return operatorCredentials{ConfigPath: resolvedConfigPath, Source: "none"}, nil
	}
	if err != nil {
		return operatorCredentials{}, err
	}
	kind := normalizeCredentialKind(config.CredentialKind, firstNonEmpty(config.OperatorToken, config.FirebaseIDToken, config.CLIToken))
	token := strings.TrimSpace(config.OperatorToken)
	switch kind {
	case credentialKindFirebase:
		token = strings.TrimSpace(config.FirebaseIDToken)
	case credentialKindCLIToken:
		token = resolveCLITokenValue(config)
	}
	return operatorCredentials{
		Token:                token,
		Email:                strings.TrimSpace(config.OperatorEmail),
		APIBase:              strings.TrimRight(strings.TrimSpace(config.APIBase), "/"),
		HostedSurfaceURL:     strings.TrimRight(strings.TrimSpace(config.HostedSurfaceURL), "/"),
		CredentialKind:       kind,
		FirebaseRefreshToken: strings.TrimSpace(config.FirebaseRefreshToken),
		FirebaseAPIKey:       strings.TrimSpace(config.FirebaseAPIKey),
		FirebaseProjectID:    strings.TrimSpace(config.FirebaseProjectID),
		Source:               "config",
		ConfigPath:           resolvedConfigPath,
	}, nil
}

// currentOperatorCredentialsForRequest loads the stored operator credentials and,
// for Firebase credentials, may refresh the short-lived ID token (rewriting the
// config file) BEFORE a request is made. targetAPIBase is the control-plane base
// URL the command is actually about to hit. The refresh+persist is gated on that
// target matching the host the stored credential belongs to (its api_base): a
// command pointed at a different/local control plane must NOT refresh or rewrite
// the stored credential. Pass "" to mean "the credential's own/default host"
// (preserves the historical default-prod behavior).
func currentOperatorCredentialsForRequest(configPath string, targetAPIBase string) (operatorCredentials, error) {
	creds, err := currentOperatorCredentials(configPath)
	if err != nil {
		return operatorCredentials{}, err
	}
	if creds.Source != "config" || normalizeCredentialKind(creds.CredentialKind, creds.Token) != credentialKindFirebase {
		return creds, nil
	}
	// Only refresh/rewrite the stored credential when the request targets the
	// control plane the credential was issued for. A local/non-prod --api (or
	// SOROQ_API) target must leave the prod credential byte-for-byte intact.
	if !apiTargetMatchesCredential(targetAPIBase, creds.APIBase) {
		return creds, nil
	}
	if creds.FirebaseRefreshToken == "" || creds.FirebaseAPIKey == "" {
		return creds, nil
	}

	refreshed, err := refreshFirebaseIDToken(creds.FirebaseAPIKey, creds.FirebaseRefreshToken)
	if err != nil {
		if strings.TrimSpace(creds.Token) != "" {
			return creds, nil
		}
		return operatorCredentials{}, err
	}
	if strings.TrimSpace(refreshed.IDToken) == "" {
		return creds, nil
	}

	creds.Token = strings.TrimSpace(refreshed.IDToken)
	if strings.TrimSpace(refreshed.RefreshToken) != "" {
		creds.FirebaseRefreshToken = strings.TrimSpace(refreshed.RefreshToken)
	}

	if creds.ConfigPath != "" {
		config, err := loadAuthConfig(creds.ConfigPath)
		if err == nil {
			config.CredentialKind = credentialKindFirebase
			config.FirebaseIDToken = creds.Token
			config.FirebaseRefreshToken = creds.FirebaseRefreshToken
			config.UpdatedAt = time.Now().UTC()
			_ = saveAuthConfig(creds.ConfigPath, config)
		}
	}

	return creds, nil
}

// apiTargetMatchesCredential reports whether a command's effective control-plane
// target (targetAPIBase) belongs to the same host as the stored credential's
// api_base (credAPIBase). It is the guard that prevents a local/non-prod --api
// command from refreshing or rewriting a prod credential.
//
// Matching rule (host-only, scheme/port/case/trailing-slash insensitive):
//   - empty target  -> match (no explicit target = use the credential's own host,
//     i.e. the default-prod path; preserve historical refresh behavior).
//   - empty stored api_base -> match (cannot prove a mismatch; do not silently
//     regress refresh for credentials that predate api_base being recorded).
//   - otherwise -> match only when the parsed hostnames are equal.
func apiTargetMatchesCredential(targetAPIBase string, credAPIBase string) bool {
	targetHost := apiHost(targetAPIBase)
	credHost := apiHost(credAPIBase)
	if targetHost == "" || credHost == "" {
		return true
	}
	return strings.EqualFold(targetHost, credHost)
}

// apiHost extracts a normalized (lowercased) hostname from a control-plane base
// URL. It tolerates bare host[:port] values that lack a scheme.
func apiHost(rawAPIBase string) string {
	raw := strings.TrimSpace(rawAPIBase)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func normalizeCredentialKind(kind string, token string) string {
	kind = strings.TrimSpace(kind)
	switch kind {
	case credentialKindOperatorToken, credentialKindFirebase, credentialKindCLIToken:
		return kind
	case "":
		if strings.TrimSpace(token) != "" {
			return credentialKindOperatorToken
		}
	}
	return kind
}

func envOperatorToken() string {
	return firstNonEmptyEnv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "SOROQ_OPERATOR_TOKEN")
}

func normalizeOperatorEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func resolveSoroqConfigPath(configPath string) (string, error) {
	path := strings.TrimSpace(configPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("SOROQ_CONFIG"))
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, ".soroq", "config.json")
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Abs(path)
}

// resolveCLITokenValue returns the raw cli_token for a stored config, reading it
// from the macOS Keychain when the token was stored there, or from the config
// file in the plaintext (0600) fallback. Returns "" when it cannot be recovered.
func resolveCLITokenValue(config authConfig) string {
	if config.TokenInKeychain {
		apiBase := strings.TrimRight(strings.TrimSpace(config.APIBase), "/")
		token, err := keychainReadFn(apiBase)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(token)
	}
	return strings.TrimSpace(config.CLIToken)
}

func loadAuthConfig(path string) (authConfig, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return authConfig{}, err
	}
	var config authConfig
	if err := json.Unmarshal(bytes, &config); err != nil {
		return authConfig{}, err
	}
	return config, nil
}

func saveAuthConfig(path string, config authConfig) error {
	if config.SchemaVersion == 0 {
		config.SchemaVersion = 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	bytes, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	bytes = append(bytes, '\n')
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, bytes, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
