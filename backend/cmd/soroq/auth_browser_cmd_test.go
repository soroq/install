package main

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestLoginCallback() *browserLoginCallback {
	return &browserLoginCallback{
		State:   "test-state",
		result:  make(chan browserLoginResult, 1),
		errs:    make(chan error, 1),
		timeout: time.Second,
	}
}

func TestPKCEChallengeMatchesSHA256(t *testing.T) {
	verifier, err := generatePKCEVerifier()
	if err != nil {
		t.Fatalf("generatePKCEVerifier() error = %v", err)
	}
	if n := len(verifier); n < 43 || n > 128 {
		t.Fatalf("verifier length = %d, want 43..128", n)
	}
	if _, err := base64.RawURLEncoding.DecodeString(verifier); err != nil {
		t.Fatalf("verifier is not base64url (no pad): %v", err)
	}

	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := pkceChallenge(verifier); got != want {
		t.Fatalf("pkceChallenge = %q, want base64url_nopad(sha256(verifier)) = %q", got, want)
	}
}

func TestBuildBrowserLoginURLContainsPKCEAndState(t *testing.T) {
	rawURL, err := buildBrowserLoginURL("https://soroq.dev", "http://127.0.0.1:54321/callback", "state-abc", "challenge-xyz", "https://api.soroq.dev", "owner@example.com")
	if err != nil {
		t.Fatalf("buildBrowserLoginURL() error = %v", err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("Parse(loginURL) error = %v", err)
	}
	if parsed.Path != "/cli/login" {
		t.Fatalf("expected /cli/login path, got %q", parsed.Path)
	}
	q := parsed.Query()
	if q.Get("client") != "soroq-cli" {
		t.Fatalf("expected client=soroq-cli, got %q", q.Get("client"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("expected code_challenge_method=S256, got %q", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") != "challenge-xyz" {
		t.Fatalf("expected code_challenge, got %q", q.Get("code_challenge"))
	}
	if q.Get("state") != "state-abc" {
		t.Fatalf("expected state, got %q", q.Get("state"))
	}
	if q.Get("api") != "https://api.soroq.dev" {
		t.Fatalf("expected api base, got %q", q.Get("api"))
	}
	if q.Get("email_hint") != "owner@example.com" {
		t.Fatalf("expected email_hint, got %q", q.Get("email_hint"))
	}
	redirect := q.Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") || !strings.HasSuffix(redirect, "/callback") {
		t.Fatalf("expected loopback redirect_uri, got %q", redirect)
	}
}

func TestBuildBrowserLoginURLOmitsEmailHintWhenEmpty(t *testing.T) {
	rawURL, err := buildBrowserLoginURL("https://soroq.dev", "http://127.0.0.1:1/callback", "s", "c", "https://api.soroq.dev", "")
	if err != nil {
		t.Fatalf("buildBrowserLoginURL() error = %v", err)
	}
	parsed, _ := url.Parse(rawURL)
	if _, ok := parsed.Query()["email_hint"]; ok {
		t.Fatalf("did not expect email_hint when hint empty: %q", rawURL)
	}
}

func TestBrowserLoginCallbackGETSurfacesCode(t *testing.T) {
	cb := newTestLoginCallback()
	req := httptest.NewRequest(http.MethodGet, "/callback?code=auth-code-1&state=test-state", nil)
	rec := httptest.NewRecorder()

	cb.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want %d", rec.Code, http.StatusOK)
	}
	select {
	case result := <-cb.result:
		if result.Code != "auth-code-1" {
			t.Fatalf("delivered code = %q, want auth-code-1", result.Code)
		}
	default:
		t.Fatal("callback did not deliver the authorization code")
	}
}

func TestBrowserLoginCallbackStateMismatchErrors(t *testing.T) {
	cb := newTestLoginCallback()
	req := httptest.NewRequest(http.MethodGet, "/callback?code=auth-code-1&state=wrong-state", nil)
	rec := httptest.NewRecorder()

	cb.handle(rec, req)

	select {
	case result := <-cb.result:
		t.Fatalf("expected no code delivery on state mismatch, got %q", result.Code)
	default:
	}
	select {
	case err := <-cb.errs:
		if !strings.Contains(err.Error(), "state") {
			t.Fatalf("expected state mismatch error, got %v", err)
		}
	default:
		t.Fatal("expected a state mismatch error")
	}
}

func TestBrowserLoginCallbackAccessDeniedErrors(t *testing.T) {
	cb := newTestLoginCallback()
	req := httptest.NewRequest(http.MethodGet, "/callback?error=access_denied&state=test-state", nil)
	rec := httptest.NewRecorder()

	cb.handle(rec, req)

	select {
	case <-cb.result:
		t.Fatal("expected no code delivery on access_denied")
	default:
	}
	select {
	case err := <-cb.errs:
		if !strings.Contains(err.Error(), "access_denied") {
			t.Fatalf("expected access_denied error, got %v", err)
		}
	default:
		t.Fatal("expected an access_denied error")
	}
}
