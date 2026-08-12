package main

// PUBLISH AUTHENTICATION — load-bearing tests.
//
// The existing freehand hosted-publish tests run against a server with operator auth DISABLED, so they
// stay green whether or not a request carries credentials. That is exactly the shape of the bug this
// file guards: registration and bundle upload must send a Bearer, and a credential that cannot be
// recovered must fail BEFORE any request or expensive build.
//
// Nothing here prints, logs or asserts on a token VALUE — only on presence, kind and outcome.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// authEnforcingServer records what each request carried and rejects anything unauthenticated, the way
// production does.
type authEnforcingServer struct {
	srv          *httptest.Server
	sawAuthOn    map[string]bool // path -> Authorization header present
	requestCount int
}

func newAuthEnforcingServer(t *testing.T, wantBearer string) *authEnforcingServer {
	t.Helper()
	a := &authEnforcingServer{sawAuthOn: map[string]bool{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.requestCount++
		got := r.Header.Get("Authorization")
		a.sawAuthOn[r.URL.Path] = strings.TrimSpace(got) != ""
		if got != "Bearer "+wantBearer {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"operator authentication required"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"p1","number":1}`))
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// writeStoredConfig writes a credential config of the given kind. The token value is a test fixture and
// never leaves this process.
func writeStoredConfig(t *testing.T, dir, kind, token, apiBase string, inKeychain bool) string {
	t.Helper()
	p := filepath.Join(dir, "config.json")
	body := `{"schema_version":1,"credential_kind":"` + kind + `","api_base":"` + apiBase + `"`
	switch kind {
	case credentialKindCLIToken:
		if inKeychain {
			body += `,"token_in_keychain":true`
		} else {
			body += `,"cli_token":"` + token + `"`
		}
	case credentialKindOperatorToken:
		body += `,"operator_token":"` + token + `","operator_email":"op@example.test"`
	default:
		body += `,"firebase_id_token":"` + token + `"`
	}
	body += "}"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "SOROQ_OPERATOR_TOKEN", "SOROQ_OPERATOR_EMAIL", "SOROQ_API",
	} {
		old, had := os.LookupEnv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			}
		})
	}
}

// NO ENV TOKEN + STORED cli_token -> a Bearer is sent, and an auth-ENFORCING backend accepts it.
func TestStoredCLITokenAuthenticatesAgainstAnAuthEnforcingBackend(t *testing.T) {
	clearCredentialEnv(t)
	const token = "stored-cli-token-fixture"
	srv := newAuthEnforcingServer(t, token)
	cfg := writeStoredConfig(t, t.TempDir(), credentialKindCLIToken, token, srv.srv.URL, false)

	creds, err := requireOperatorCredentials(cfg, srv.srv.URL, "publishing a hosted patch")
	if err != nil {
		t.Fatalf("stored cli_token was refused: %v", err)
	}
	if strings.TrimSpace(creds.Token) == "" {
		t.Fatal("token_present=false; the request would go out unauthenticated")
	}

	req, _ := http.NewRequest(http.MethodPost, srv.srv.URL+"/v1/patches", strings.NewReader("{}"))
	applyCredentialsHeaders(req, creds)
	if req.Header.Get("Authorization") == "" {
		t.Fatal("Authorization_header_present=false on registration")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth-enforcing backend rejected the stored cli_token: HTTP %d", resp.StatusCode)
	}
	if !srv.sawAuthOn["/v1/patches"] {
		t.Error("registration reached the server WITHOUT an Authorization header")
	}
}

// The bundle upload must carry the SAME credential, not a differently-resolved one.
func TestBundleUploadSendsTheSameAuthenticatedCredential(t *testing.T) {
	clearCredentialEnv(t)
	const token = "stored-cli-token-fixture"
	srv := newAuthEnforcingServer(t, token)
	cfg := writeStoredConfig(t, t.TempDir(), credentialKindCLIToken, token, srv.srv.URL, false)

	creds, err := requireOperatorCredentials(cfg, srv.srv.URL, "uploading a patch bundle")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.srv.URL+"/v1/patches/p1/bundle", strings.NewReader("zip"))
	applyCredentialsHeaders(req, creds)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bundle upload was rejected: HTTP %d", resp.StatusCode)
	}
	if !srv.sawAuthOn["/v1/patches/p1/bundle"] {
		t.Error("bundle upload reached the server WITHOUT an Authorization header")
	}
}

// KEYCHAIN FAILURE -> actionable error and ZERO HTTP requests. Previously it returned "" and an
// unauthenticated request went out, surfacing as a bare 401 after a full build.
func TestKeychainReadFailureIsActionableAndSendsNothing(t *testing.T) {
	clearCredentialEnv(t)
	srv := newAuthEnforcingServer(t, "unused")
	cfg := writeStoredConfig(t, t.TempDir(), credentialKindCLIToken, "", srv.srv.URL, true)

	old := keychainReadFn
	keychainReadFn = func(string) (string, error) { return "", os.ErrPermission }
	t.Cleanup(func() { keychainReadFn = old })

	_, err := requireOperatorCredentials(cfg, srv.srv.URL, "publishing a hosted patch")
	if err == nil {
		t.Fatal("a Keychain read failure was treated as 'no credential' and allowed to proceed")
	}
	for _, want := range []string{"Keychain", "soroq login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is not actionable (missing %q): %v", want, err)
		}
	}
	if srv.requestCount != 0 {
		t.Errorf("made %d HTTP request(s) despite an unreadable credential", srv.requestCount)
	}
}

// An EMPTY stored token must be refused before any request, not sent as an anonymous call.
func TestEmptyStoredCredentialIsRefusedEarly(t *testing.T) {
	clearCredentialEnv(t)
	srv := newAuthEnforcingServer(t, "unused")
	cfg := writeStoredConfig(t, t.TempDir(), credentialKindCLIToken, "", srv.srv.URL, false)

	if _, err := requireOperatorCredentials(cfg, srv.srv.URL, "publishing a hosted patch"); err == nil {
		t.Fatal("an empty stored credential was accepted")
	}
	if srv.requestCount != 0 {
		t.Errorf("made %d HTTP request(s) with no usable credential", srv.requestCount)
	}
}

// A stored credential must never be forwarded to a control plane it was not issued for.
func TestStoredCredentialIsNotLeakedToAnotherHost(t *testing.T) {
	clearCredentialEnv(t)
	cfg := writeStoredConfig(t, t.TempDir(), credentialKindCLIToken, "prod-token-fixture", "https://soroq.dev/api", false)

	_, err := requireOperatorCredentials(cfg, "https://evil.example.test/api", "publishing a hosted patch")
	if err == nil {
		t.Fatal("a production credential was accepted for an unrelated --api host")
	}
	if !strings.Contains(err.Error(), "refusing to send them") {
		t.Errorf("refusal should say the credential is not being sent: %v", err)
	}
	if strings.Contains(err.Error(), "prod-token-fixture") {
		t.Fatal("the token VALUE appeared in the error message")
	}
}

// The other two supported modes stay green.
func TestStaticOperatorTokenStillWorks(t *testing.T) {
	clearCredentialEnv(t)
	const token = "static-operator-token-fixture"
	srv := newAuthEnforcingServer(t, token)
	cfg := writeStoredConfig(t, t.TempDir(), credentialKindOperatorToken, token, srv.srv.URL, false)

	creds, err := requireOperatorCredentials(cfg, srv.srv.URL, "publishing a hosted patch")
	if err != nil {
		t.Fatalf("static operator token was refused: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.srv.URL+"/v1/patches", strings.NewReader("{}"))
	applyCredentialsHeaders(req, creds)
	if req.Header.Get("X-Soroq-Operator-Email") == "" {
		t.Error("operator-token mode must still send the operator email header")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth-enforcing backend rejected the static operator token: HTTP %d", resp.StatusCode)
	}
}

func TestFirebaseStoredCredentialStillAuthenticates(t *testing.T) {
	clearCredentialEnv(t)
	const token = "firebase-id-token-fixture"
	srv := newAuthEnforcingServer(t, token)
	cfg := writeStoredConfig(t, t.TempDir(), credentialKindFirebase, token, srv.srv.URL, false)

	creds, err := requireOperatorCredentials(cfg, srv.srv.URL, "publishing a hosted patch")
	if err != nil {
		t.Fatalf("firebase credential was refused: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.srv.URL+"/v1/patches", strings.NewReader("{}"))
	applyCredentialsHeaders(req, creds)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth-enforcing backend rejected the firebase credential: HTTP %d", resp.StatusCode)
	}
}

// Diagnostics must never carry a token value.
func TestCredentialErrorsNeverContainTokenValues(t *testing.T) {
	clearCredentialEnv(t)
	const secret = "super-secret-token-value"
	cfg := writeStoredConfig(t, t.TempDir(), credentialKindCLIToken, secret, "https://soroq.dev/api", false)

	_, err := requireOperatorCredentials(cfg, "https://other.example.test/api", "publishing a hosted patch")
	if err == nil {
		t.Fatal("expected a host-mismatch refusal")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("the token value leaked into an error message")
	}
}
