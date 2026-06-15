package main

import (
	"context"
	"crypto/rand"
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
	ConfigPath       string
	NoOpen           bool
	Timeout          time.Duration
	JSONOut          bool
}

type browserLoginPayload struct {
	State        string `json:"state"`
	IDToken      string `json:"idToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	Email        string `json:"email,omitempty"`
	APIKey       string `json:"apiKey,omitempty"`
	ProjectID    string `json:"projectId,omitempty"`
}

type firebaseRefreshResult struct {
	IDToken      string
	RefreshToken string
}

var openBrowserURL = openBrowserWithOS

func runBrowserLogin(options browserLoginOptions) error {
	hostedSurfaceURL := strings.TrimRight(strings.TrimSpace(options.HostedSurfaceURL), "/")
	if hostedSurfaceURL == "" {
		hostedSurfaceURL = defaultHostedSurfaceURL
	}
	apiBase := strings.TrimRight(strings.TrimSpace(options.APIBase), "/")
	if apiBase == "" {
		apiBase = hostedSurfaceAPIBase(hostedSurfaceURL)
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}

	callback, err := startBrowserLoginCallback(timeout)
	if err != nil {
		return err
	}
	defer callback.Close()

	loginURL, err := buildBrowserLoginURL(hostedSurfaceURL, callback.URL, callback.State)
	if err != nil {
		return err
	}

	if options.NoOpen {
		fmt.Fprintf(os.Stderr, "Open this URL to finish Soroq login:\n%s\n", loginURL)
	} else if err := openBrowserURL(loginURL); err != nil {
		fmt.Fprintf(os.Stderr, "Open this URL to finish Soroq login:\n%s\n", loginURL)
		fmt.Fprintf(os.Stderr, "Could not open the browser automatically: %v\n", err)
	}

	payload, err := callback.Wait()
	if err != nil {
		return err
	}
	token := strings.TrimSpace(payload.IDToken)
	if token == "" {
		return errors.New("browser login callback did not include a Firebase ID token")
	}
	email := normalizeOperatorEmail(payload.Email)

	summary := authLoginSummary{
		APIBase:          apiBase,
		HostedSurfaceURL: hostedSurfaceURL,
		Email:            email,
		ConfigPath:       options.ConfigPath,
		CredentialKind:   credentialKindFirebase,
		TokenStored:      true,
	}
	appCount, err := verifyOperatorCredentials(apiBase, operatorCredentials{
		Token:          token,
		Email:          email,
		CredentialKind: credentialKindFirebase,
	})
	if err != nil {
		return fmt.Errorf("login verification failed: %w", err)
	}
	summary.Verified = true
	summary.AppCount = appCount

	if err := saveAuthConfig(options.ConfigPath, authConfig{
		SchemaVersion:        1,
		CredentialKind:       credentialKindFirebase,
		APIBase:              apiBase,
		HostedSurfaceURL:     hostedSurfaceURL,
		OperatorEmail:        email,
		FirebaseIDToken:      token,
		FirebaseRefreshToken: strings.TrimSpace(payload.RefreshToken),
		FirebaseAPIKey:       strings.TrimSpace(payload.APIKey),
		FirebaseProjectID:    strings.TrimSpace(payload.ProjectID),
		UpdatedAt:            time.Now().UTC(),
	}); err != nil {
		return err
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
	fmt.Fprintf(os.Stdout, "Verified access to %d app(s).\n", summary.AppCount)
	fmt.Fprintf(os.Stdout, "Hosted surface: %s\n", summary.HostedSurfaceURL)
	fmt.Fprintf(os.Stdout, "Config: %s\n", summary.ConfigPath)
	return nil
}

type browserLoginCallback struct {
	URL     string
	State   string
	server  *http.Server
	result  chan browserLoginPayload
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
		result:  make(chan browserLoginPayload, 1),
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

func (callback *browserLoginCallback) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Use POST for the Soroq CLI login callback.", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		callback.errs <- err
		http.Error(w, "Could not read callback body.", http.StatusBadRequest)
		return
	}

	var payload browserLoginPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		callback.errs <- err
		http.Error(w, "Could not decode callback body.", http.StatusBadRequest)
		return
	}
	if payload.State != callback.State {
		err := errors.New("browser login callback state did not match")
		callback.errs <- err
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	select {
	case callback.result <- payload:
	default:
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "<!doctype html><title>Soroq login complete</title><p>Soroq login complete. You can return to the terminal.</p>")
}

func (callback *browserLoginCallback) Wait() (browserLoginPayload, error) {
	timer := time.NewTimer(callback.timeout)
	defer timer.Stop()

	select {
	case payload := <-callback.result:
		return payload, nil
	case err := <-callback.errs:
		return browserLoginPayload{}, err
	case <-timer.C:
		return browserLoginPayload{}, fmt.Errorf("browser login timed out after %s", callback.timeout)
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

func buildBrowserLoginURL(hostedSurfaceURL string, callbackURL string, state string) (string, error) {
	hostedSurfaceURL = strings.TrimRight(strings.TrimSpace(hostedSurfaceURL), "/")
	if hostedSurfaceURL == "" {
		hostedSurfaceURL = defaultHostedSurfaceURL
	}
	parsed, err := url.Parse(hostedSurfaceURL + "/operator.html")
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("cli_login_callback", callbackURL)
	query.Set("cli_login_state", state)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func hostedSurfaceAPIBase(hostedSurfaceURL string) string {
	hostedSurfaceURL = strings.TrimRight(strings.TrimSpace(hostedSurfaceURL), "/")
	if hostedSurfaceURL == "" {
		hostedSurfaceURL = defaultHostedSurfaceURL
	}
	return hostedSurfaceURL + "/api"
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
