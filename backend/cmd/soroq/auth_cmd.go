package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
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
	UpdatedAt            time.Time `json:"updated_at"`
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
		fmt.Fprintln(os.Stdout, `usage: soroq login [--hosted-surface https://soroq-hosted-surface.vercel.app] [--config ~/.soroq/config.json] [--json]
       soroq login --email operator@example.com (--token <token> | --token-file ./token.txt | --token-stdin) [--api https://soroq-control-plane.fly.dev] [--config ~/.soroq/config.json] [--skip-verify] [--json]`)
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
		if flagWasSet(fs, "api") {
			resolvedAPIBase = strings.TrimRight(strings.TrimSpace(*apiBase), "/")
		} else {
			resolvedAPIBase = hostedSurfaceAPIBase(resolvedHostedSurface)
		}
		return runBrowserLogin(browserLoginOptions{
			APIBase:          resolvedAPIBase,
			HostedSurfaceURL: resolvedHostedSurface,
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
		fmt.Fprintln(os.Stdout, `usage: soroq whoami [--api https://soroq-control-plane.fly.dev] [--config ~/.soroq/config.json] [--offline] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	creds, err := currentOperatorCredentialsForRequest(*configPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(creds.Token) == "" {
		return errors.New("not logged in; run `soroq login` or set SOROQ_CONTROL_PLANE_OPERATOR_TOKEN")
	}

	resolvedAPIBase := strings.TrimRight(strings.TrimSpace(*apiBase), "/")
	if resolvedAPIBase == "" {
		resolvedAPIBase = strings.TrimRight(strings.TrimSpace(creds.APIBase), "/")
	}
	if resolvedAPIBase == "" {
		resolvedAPIBase = defaultControlPlaneAPI
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

func runLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	configPath := fs.String("config", "", "credential config path")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq logout [--config ~/.soroq/config.json] [--json]`)
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
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(apiBase, "/")+"/v1/apps", nil)
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
	kind := normalizeCredentialKind(config.CredentialKind, firstNonEmpty(config.OperatorToken, config.FirebaseIDToken))
	token := strings.TrimSpace(config.OperatorToken)
	if kind == credentialKindFirebase {
		token = strings.TrimSpace(config.FirebaseIDToken)
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

func currentOperatorCredentialsForRequest(configPath string) (operatorCredentials, error) {
	creds, err := currentOperatorCredentials(configPath)
	if err != nil {
		return operatorCredentials{}, err
	}
	if creds.Source != "config" || normalizeCredentialKind(creds.CredentialKind, creds.Token) != credentialKindFirebase {
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

func normalizeCredentialKind(kind string, token string) string {
	kind = strings.TrimSpace(kind)
	switch kind {
	case credentialKindOperatorToken, credentialKindFirebase:
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
