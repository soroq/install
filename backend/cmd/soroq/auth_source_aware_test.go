package main

// SOURCE-AWARE AUTH FAILURE.
//
// A rejected credential used to surface as "control plane returned 401", which is the same message
// whether the developer never logged in, has an expired stored login, or — the case that actually
// happens — has a STALE token exported in their shell. That last one is the worst of the three: the
// environment variable silently overrides the stored login, so `soroq login` appears to succeed and the
// very next command fails again with the identical 401. Diagnosing it means knowing to look at an
// environment variable nothing has mentioned.
//
// So an auth failure names the credential's ORIGIN. It must never name its VALUE.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const staleTokenValue = "stale-token-VALUE-must-never-be-printed"

// A rejected ENVIRONMENT credential must say so, name the variable, and say that it overrides the
// stored login — the fact that makes the failure self-diagnosing.
func TestRejectedEnvironmentCredentialNamesTheVariableAndTheOverride(t *testing.T) {
	t.Setenv("SOROQ_OPERATOR_TOKEN", staleTokenValue)
	creds, err := currentOperatorCredentials("")
	if err != nil {
		t.Fatalf("resolve credentials: %v", err)
	}
	if creds.Source != "environment" {
		t.Fatalf("credential source is %q, want environment", creds.Source)
	}

	authErr := authFailureError(http.StatusUnauthorized, creds, "https://soroq.dev/api", "registering the patch")
	if authErr == nil {
		t.Fatal("a 401 produced no error")
	}
	msg := authErr.Error()
	for _, want := range []string{
		"SOROQ_OPERATOR_TOKEN", // which credential
		"OVERRIDES",            // why `soroq login` will not help
		"unset",                // what to do
		"registering the patch",
		"401",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("auth error does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, staleTokenValue) {
		t.Error("the auth error PRINTED THE TOKEN VALUE; a credential must never be logged")
	}
}

// The other supported environment variable is named correctly when it is the one in use.
func TestControlPlaneOperatorTokenVariableIsNamedWhenUsed(t *testing.T) {
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", staleTokenValue)
	creds, err := currentOperatorCredentials("")
	if err != nil {
		t.Fatalf("resolve credentials: %v", err)
	}
	msg := authFailureError(http.StatusUnauthorized, creds, "https://soroq.dev/api", "uploading").Error()
	if !strings.Contains(msg, "SOROQ_CONTROL_PLANE_OPERATOR_TOKEN") {
		t.Errorf("the variable actually in use is not named:\n%s", msg)
	}
}

// A rejected STORED login points at the config, not at an environment variable that is not set.
func TestRejectedStoredLoginPointsAtTheStoredCredential(t *testing.T) {
	creds := operatorCredentials{
		Token: staleTokenValue, Email: "dev@example.com", CredentialKind: credentialKindCLIToken,
		Source: "config", ConfigPath: "/home/dev/.soroq/config.json",
	}
	msg := authFailureError(http.StatusUnauthorized, creds, "https://soroq.dev/api", "registering the patch").Error()
	if !strings.Contains(msg, "/home/dev/.soroq/config.json") || !strings.Contains(msg, "dev@example.com") {
		t.Errorf("stored-login failure does not identify the credential:\n%s", msg)
	}
	if strings.Contains(msg, "SOROQ_OPERATOR_TOKEN") {
		t.Errorf("blamed an environment variable that is not in use:\n%s", msg)
	}
	if strings.Contains(msg, staleTokenValue) {
		t.Error("the auth error PRINTED THE TOKEN VALUE")
	}
}

// A 403 is a different diagnosis from a 401 and must not be described as a bad credential.
func TestForbiddenIsDescribedAsAnAuthorizationProblem(t *testing.T) {
	creds := operatorCredentials{Token: "t", CredentialKind: credentialKindCLIToken, Source: "config"}
	msg := authFailureError(http.StatusForbidden, creds, "https://soroq.dev/api", "registering the patch").Error()
	if !strings.Contains(msg, "not authorized") {
		t.Errorf("403 is not explained as an authorization failure:\n%s", msg)
	}
}

// Non-auth statuses must be left alone: a 404 or 500 is not an auth problem, and rewriting it as one
// would send the developer chasing credentials over a real server error.
func TestNonAuthStatusesAreNotRewrittenAsAuthFailures(t *testing.T) {
	creds := operatorCredentials{Token: "t", Source: "environment"}
	for _, status := range []int{http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError} {
		if err := authFailureError(status, creds, "https://soroq.dev/api", "x"); err != nil {
			t.Errorf("HTTP %d was rewritten as an auth failure: %v", status, err)
		}
	}
}

// END TO END through the real request path: a control plane that rejects the credential must produce
// the source-aware message, not the bare "control plane returned 401".
func TestPublishPathSurfacesTheSourceAwareErrorNotABare401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	t.Cleanup(srv.Close)

	creds := operatorCredentials{
		Token: staleTokenValue, CredentialKind: credentialKindOperatorToken, Source: "environment",
	}
	t.Setenv("SOROQ_OPERATOR_TOKEN", staleTokenValue)

	_, err := postJSONOperator(srv.URL+"/v1/patches", creds, map[string]string{"id": "p1"})
	if err == nil {
		t.Fatal("a 401 from the control plane was reported as success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "SOROQ_OPERATOR_TOKEN") {
		t.Errorf("the publish path still returns an anonymous 401:\n%s", msg)
	}
	if strings.Contains(msg, staleTokenValue) {
		t.Error("the publish path PRINTED THE TOKEN VALUE")
	}

	uploadErr := uploadEngineBundleOperator(srv.URL+"/v1/engine", creds, []byte("zip"))
	if uploadErr == nil {
		t.Fatal("a 401 on bundle upload was reported as success")
	}
	if !strings.Contains(uploadErr.Error(), "SOROQ_OPERATOR_TOKEN") {
		t.Errorf("bundle upload still returns an anonymous 401:\n%s", uploadErr)
	}
	if strings.Contains(uploadErr.Error(), staleTokenValue) {
		t.Error("bundle upload PRINTED THE TOKEN VALUE")
	}
}
