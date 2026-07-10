package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type browserLoginOptions struct {
	APIBase          string
	HostedSurfaceURL string
	EmailHint        string
	ConfigPath       string
	NoOpen           bool
	Timeout          time.Duration
	JSONOut          bool
}

// browserLoginResult is the OAuth authorization-code redirect the hosted login
// surface sends back to the CLI loopback callback.
type browserLoginResult struct {
	Code  string
	State string
}

type firebaseRefreshResult struct {
	IDToken      string
	RefreshToken string
}

// cliExchangeResponse is the body of POST /v1/cli/auth/exchange.
type cliExchangeResponse struct {
	Token     string   `json:"token"`
	Email     string   `json:"email"`
	Scopes    []string `json:"scopes"`
	TokenType string   `json:"token_type"`
}

// cliWhoamiResponse is the body of GET /v1/cli/auth/whoami.
type cliWhoamiResponse struct {
	Email  string   `json:"email"`
	Scopes []string `json:"scopes"`
	Kind   string   `json:"kind"`
}

var openBrowserURL = openBrowserWithOS

// runBrowserLogin performs the OAuth Authorization-Code + PKCE browser flow:
// it opens the hosted login surface, receives a one-time code on a loopback
// redirect, exchanges it for a bearer token, and stores it as a cli_token
// credential (Keychain preferred, 0600 file fallback). The token is never
// printed or logged.
func runBrowserLogin(options browserLoginOptions) error {
	hostedSurfaceURL := strings.TrimRight(strings.TrimSpace(options.HostedSurfaceURL), "/")
	if hostedSurfaceURL == "" {
		hostedSurfaceURL = defaultHostedSurfaceURL
	}
	apiBase := strings.TrimRight(strings.TrimSpace(options.APIBase), "/")
	if apiBase == "" {
		apiBase = defaultControlPlaneAPI
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}

	verifier, err := generatePKCEVerifier()
	if err != nil {
		return err
	}
	challenge := pkceChallenge(verifier)

	callback, err := startBrowserLoginCallback(timeout)
	if err != nil {
		return err
	}
	defer callback.Close()

	loginURL, err := buildBrowserLoginURL(hostedSurfaceURL, callback.URL, callback.State, challenge, apiBase, options.EmailHint)
	if err != nil {
		return err
	}

	if options.NoOpen {
		fmt.Fprintf(os.Stderr, "Open this URL to finish Soroq login:\n%s\n", loginURL)
	} else if err := openBrowserURL(loginURL); err != nil {
		fmt.Fprintf(os.Stderr, "Open this URL to finish Soroq login:\n%s\n", loginURL)
		fmt.Fprintf(os.Stderr, "Could not open the browser automatically: %v\n", err)
	}

	result, err := callback.Wait()
	if err != nil {
		return err
	}
	code := strings.TrimSpace(result.Code)
	if code == "" {
		return errors.New("browser login callback did not include an authorization code")
	}

	exchanged, err := exchangeCLICode(apiBase, code, verifier)
	if err != nil {
		return fmt.Errorf("login exchange failed: %w", err)
	}
	if strings.TrimSpace(exchanged.Token) == "" {
		return errors.New("login exchange did not return a token")
	}
	email := normalizeOperatorEmail(exchanged.Email)

	inKeychain, err := storeCLIToken(options.ConfigPath, apiBase, email, exchanged.Scopes, exchanged.Token)
	if err != nil {
		return err
	}

	summary := authLoginSummary{
		APIBase:          apiBase,
		HostedSurfaceURL: hostedSurfaceURL,
		Email:            email,
		ConfigPath:       options.ConfigPath,
		CredentialKind:   credentialKindCLIToken,
		TokenStored:      true,
		Verified:         true,
	}

	if options.JSONOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Logged in to %s", summary.APIBase)
	if summary.Email != "" {
		fmt.Fprintf(os.Stdout, " as %s", summary.Email)
	}
	fmt.Fprintln(os.Stdout)
	if inKeychain {
		fmt.Fprintln(os.Stdout, "Token stored in the macOS Keychain.")
	} else {
		fmt.Fprintf(os.Stdout, "Token stored (0600) in %s\n", summary.ConfigPath)
	}
	fmt.Fprintf(os.Stdout, "Config: %s\n", summary.ConfigPath)
	return nil
}

type browserLoginCallback struct {
	URL     string
	State   string
	server  *http.Server
	result  chan browserLoginResult
	errs    chan error
	timeout time.Duration
}

func startBrowserLoginCallback(timeout time.Duration) (*browserLoginCallback, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	state, err := randomLoginState()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	callback := &browserLoginCallback{
		URL:     fmt.Sprintf("http://%s/callback", listener.Addr().String()),
		State:   state,
		result:  make(chan browserLoginResult, 1),
		errs:    make(chan error, 1),
		timeout: timeout,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", callback.handle)
	callback.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: minDuration(timeout, 10*time.Second),
	}

	go func() {
		if err := callback.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			callback.errs <- err
		}
	}()

	return callback, nil
}

// handle receives the browser's GET redirect carrying ?code=&state= (or
// ?error=&state=). The callback binds 127.0.0.1 only, so it is loopback-scoped.
func (callback *browserLoginCallback) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Use a GET redirect for the Soroq CLI login callback.", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	state := strings.TrimSpace(query.Get("state"))
	if state != callback.State {
		err := errors.New("browser login callback state did not match")
		callback.deliverErr(err)
		writeCallbackHTML(w, http.StatusBadRequest, "Soroq login failed", "Login could not be verified (state mismatch). Return to the terminal and try again.")
		return
	}
	if errParam := strings.TrimSpace(query.Get("error")); errParam != "" {
		err := fmt.Errorf("browser login was denied: %s", errParam)
		callback.deliverErr(err)
		writeCallbackHTML(w, http.StatusOK, "Soroq login failed", "Login was denied. Return to the terminal.")
		return
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		err := errors.New("browser login callback did not include an authorization code")
		callback.deliverErr(err)
		writeCallbackHTML(w, http.StatusBadRequest, "Soroq login failed", "Login response was missing its code. Return to the terminal and try again.")
		return
	}

	select {
	case callback.result <- browserLoginResult{Code: code, State: state}:
	default:
	}
	writeCallbackHTML(w, http.StatusOK, "Soroq login complete", "Soroq login complete. You can return to the terminal.")
}

func (callback *browserLoginCallback) deliverErr(err error) {
	select {
	case callback.errs <- err:
	default:
	}
}

func writeCallbackHTML(w http.ResponseWriter, status int, title string, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "<!doctype html><title>"+title+"</title><p>"+message+"</p>")
}

func (callback *browserLoginCallback) Wait() (browserLoginResult, error) {
	timer := time.NewTimer(callback.timeout)
	defer timer.Stop()

	select {
	case result := <-callback.result:
		return result, nil
	case err := <-callback.errs:
		return browserLoginResult{}, err
	case <-timer.C:
		return browserLoginResult{}, fmt.Errorf("browser login timed out after %s", callback.timeout)
	}
}

func (callback *browserLoginCallback) Close() {
	if callback == nil || callback.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = callback.server.Shutdown(ctx)
}

// buildBrowserLoginURL constructs the hosted OAuth+PKCE login URL. The website
// authorizes the operator, then redirects the browser to redirect_uri with a
// one-time code + state.
func buildBrowserLoginURL(hostedSurfaceURL string, callbackURL string, state string, codeChallenge string, apiBase string, emailHint string) (string, error) {
	hostedSurfaceURL = strings.TrimRight(strings.TrimSpace(hostedSurfaceURL), "/")
	if hostedSurfaceURL == "" {
		hostedSurfaceURL = defaultHostedSurfaceURL
	}
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if apiBase == "" {
		apiBase = defaultControlPlaneAPI
	}
	parsed, err := url.Parse(hostedSurfaceURL + "/cli/login")
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("client", "soroq-cli")
	query.Set("redirect_uri", callbackURL)
	query.Set("state", state)
	query.Set("code_challenge", codeChallenge)
	query.Set("code_challenge_method", "S256")
	query.Set("api", apiBase)
	if hint := normalizeOperatorEmail(emailHint); hint != "" {
		query.Set("email_hint", hint)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// generatePKCEVerifier returns a 43-char base64url (no padding) code_verifier
// from 32 crypto/rand bytes (within the RFC 7636 43-128 char range).
func generatePKCEVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// pkceChallenge returns base64url_nopad(sha256(verifier)) (the S256 method).
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// exchangeCLICode redeems the one-time code + PKCE verifier for a bearer token
// via POST <apiBase>/v1/cli/auth/exchange.
func exchangeCLICode(apiBase string, code string, codeVerifier string) (cliExchangeResponse, error) {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}
	body, err := json.Marshal(map[string]string{"code": code, "code_verifier": codeVerifier})
	if err != nil {
		return cliExchangeResponse{}, err
	}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/cli/auth/exchange", bytes.NewReader(body))
	if err != nil {
		return cliExchangeResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cliExchangeResponse{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return cliExchangeResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(respBody))
		if message == "" {
			message = resp.Status
		}
		return cliExchangeResponse{}, fmt.Errorf("request failed: %s", message)
	}
	var decoded cliExchangeResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return cliExchangeResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return decoded, nil
}

// fetchCLIWhoami calls GET <apiBase>/v1/cli/auth/whoami with the bearer token.
func fetchCLIWhoami(apiBase string, token string) (cliWhoamiResponse, error) {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}
	req, err := http.NewRequest(http.MethodGet, base+"/v1/cli/auth/whoami", nil)
	if err != nil {
		return cliWhoamiResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cliWhoamiResponse{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return cliWhoamiResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(respBody))
		if message == "" {
			message = resp.Status
		}
		return cliWhoamiResponse{}, fmt.Errorf("request failed: %s", message)
	}
	var decoded cliWhoamiResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return cliWhoamiResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return decoded, nil
}

// revokeCLIToken best-effort revokes the bearer token via
// POST <apiBase>/v1/cli/auth/revoke. Errors are surfaced to callers that may
// choose to ignore them (logout is best-effort).
func revokeCLIToken(apiBase string, token string) error {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/cli/auth/revoke", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("revoke failed: %s", resp.Status)
	}
	return nil
}

// storeCLIToken persists a browser-login bearer token. It prefers the macOS
// Keychain (service "soroq-cli-token:<apiBase>"); when the Keychain is
// unavailable/fails it falls back to storing the raw token in config.json,
// which saveAuthConfig writes mode 0600. Only metadata is written to config.json
// when the Keychain succeeds. Returns whether the token landed in the Keychain.
func storeCLIToken(configPath string, apiBase string, email string, scopes []string, token string) (bool, error) {
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")

	inKeychain := false
	if keychainAvailableFn() {
		if err := keychainStoreFn(apiBase, token); err == nil {
			inKeychain = true
		}
	}

	config := authConfig{
		SchemaVersion:   1,
		CredentialKind:  credentialKindCLIToken,
		APIBase:         apiBase,
		OperatorEmail:   normalizeOperatorEmail(email),
		Scopes:          scopes,
		TokenInKeychain: inKeychain,
		UpdatedAt:       time.Now().UTC(),
	}
	if !inKeychain {
		config.CLIToken = token
	}
	if err := saveAuthConfig(configPath, config); err != nil {
		return inKeychain, err
	}
	return inKeychain, nil
}

// --- macOS Keychain storage for cli_token (scoped to the cli_token kind only) ---
//
// These are function vars so tests can inject a fake keychain (or force the
// 0600 file fallback) without touching the developer's real login keychain.
var (
	keychainAvailableFn = defaultKeychainAvailable
	keychainStoreFn     = defaultKeychainStoreToken
	keychainReadFn      = defaultKeychainReadToken
	keychainDeleteFn    = defaultKeychainDeleteToken
)

const cliTokenKeychainAccount = "soroq"

func cliTokenKeychainService(apiBase string) string {
	return "soroq-cli-token:" + strings.TrimRight(strings.TrimSpace(apiBase), "/")
}

func defaultKeychainAvailable() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath("security")
	return err == nil
}

func defaultKeychainStoreToken(apiBase string, token string) error {
	cmd := exec.Command("security", "add-generic-password", "-U",
		"-a", cliTokenKeychainAccount, "-s", cliTokenKeychainService(apiBase), "-w", token)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("keychain store failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func defaultKeychainReadToken(apiBase string) (string, error) {
	cmd := exec.Command("security", "find-generic-password",
		"-a", cliTokenKeychainAccount, "-s", cliTokenKeychainService(apiBase), "-w")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func defaultKeychainDeleteToken(apiBase string) error {
	cmd := exec.Command("security", "delete-generic-password",
		"-a", cliTokenKeychainAccount, "-s", cliTokenKeychainService(apiBase))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("keychain delete failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func randomLoginState() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func minDuration(a time.Duration, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func openBrowserWithOS(rawURL string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", rawURL).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	default:
		return exec.Command("xdg-open", rawURL).Start()
	}
}

func refreshFirebaseIDToken(apiKey string, refreshToken string) (firebaseRefreshResult, error) {
	apiKey = strings.TrimSpace(apiKey)
	refreshToken = strings.TrimSpace(refreshToken)
	if apiKey == "" || refreshToken == "" {
		return firebaseRefreshResult{}, errors.New("Firebase API key and refresh token are required")
	}

	baseURL := strings.TrimSpace(os.Getenv("SOROQ_FIREBASE_SECURE_TOKEN_URL"))
	if baseURL == "" {
		baseURL = "https://securetoken.googleapis.com/v1/token"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return firebaseRefreshResult{}, err
	}
	query := parsed.Query()
	query.Set("key", apiKey)
	parsed.RawQuery = query.Encode()

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	req, err := http.NewRequest(http.MethodPost, parsed.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return firebaseRefreshResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return firebaseRefreshResult{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return firebaseRefreshResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(respBody))
		if message == "" {
			message = resp.Status
		}
		return firebaseRefreshResult{}, fmt.Errorf("Firebase token refresh failed: %s", message)
	}

	var decoded struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return firebaseRefreshResult{}, err
	}
	return firebaseRefreshResult{
		IDToken:      decoded.IDToken,
		RefreshToken: decoded.RefreshToken,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
