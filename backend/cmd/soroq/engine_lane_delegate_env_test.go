package main

import (
	"os"
	"strings"
	"testing"
)

func TestApiFlagFromArgs(t *testing.T) {
	if got := apiFlagFromArgs([]string{"--toolchain", "x", "--api", "https://api.soroq.dev", "--patch-id", "p"}); got != "https://api.soroq.dev" {
		t.Fatalf("--api <val>: got %q", got)
	}
	if got := apiFlagFromArgs([]string{"--api=https://h.example"}); got != "https://h.example" {
		t.Fatalf("--api=<val>: got %q", got)
	}
	if got := apiFlagFromArgs([]string{"--toolchain", "x"}); got != defaultControlPlaneAPI {
		t.Fatalf("default: got %q", got)
	}
}

func TestEngineLaneDelegateEnvExplicitTokenWins(t *testing.T) {
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "explicit-env-token")
	env, err := engineLaneDelegateEnv([]string{"--api", "https://api.soroq.dev"})
	if err != nil {
		t.Fatalf("explicit env token path returned an error: %v", err)
	}
	// An explicit env token must be preserved and NOT overridden by a stored credential.
	count := 0
	for _, e := range env {
		if strings.HasPrefix(e, "SOROQ_CONTROL_PLANE_OPERATOR_TOKEN=") {
			count++
			if e != "SOROQ_CONTROL_PLANE_OPERATOR_TOKEN=explicit-env-token" {
				t.Fatalf("explicit env token overridden: %q", e)
			}
		}
	}
	if count == 0 {
		t.Fatal("explicit env token dropped")
	}
}

func TestEngineLaneDelegateEnvInjectsStoredCLIToken(t *testing.T) {
	os.Unsetenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN")
	os.Unsetenv("SOROQ_OPERATOR_TOKEN")
	dir := t.TempDir()
	cfg := dir + "/config.json"
	t.Setenv("SOROQ_CONFIG", cfg)
	// Store a cli_token credential for the target api (file fallback; no Keychain in tests).
	if err := saveAuthConfig(cfg, authConfig{
		SchemaVersion:  1,
		CredentialKind: credentialKindCLIToken,
		APIBase:        "https://api.soroq.dev",
		OperatorEmail:  "op@example.com",
		CLIToken:       "stored-cli-token-abc",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	env, err := engineLaneDelegateEnv([]string{"--api", "https://api.soroq.dev"})
	if err != nil {
		t.Fatalf("matching host refused: %v", err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "SOROQ_CONTROL_PLANE_OPERATOR_TOKEN=stored-cli-token-abc") {
		t.Fatal("stored cli_token not injected into delegate env")
	}
	// A cli_token's email is bound server-side; the email header must NOT be forwarded for it.
	if strings.Contains(joined, "SOROQ_OPERATOR_EMAIL=") {
		t.Fatal("must not forward SOROQ_OPERATOR_EMAIL for a cli_token")
	}
}

// A stored credential names the control plane it was issued for. requireOperatorCredentials refuses to
// send it anywhere else; delegating to soroqctl must not be a way around that check. This is a real
// escape I walked into: a cli_token issued for api.soroq.dev reached a different control plane, which
// answered with a confusing "Decoding Firebase ID token failed" — by which point the credential had
// already left the host it belonged to.
func TestEngineLaneDelegateEnvRefusesToForwardToAnotherControlPlane(t *testing.T) {
	os.Unsetenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN")
	os.Unsetenv("SOROQ_OPERATOR_TOKEN")
	dir := t.TempDir()
	cfg := dir + "/config.json"
	t.Setenv("SOROQ_CONFIG", cfg)
	if err := saveAuthConfig(cfg, authConfig{
		SchemaVersion:  1,
		CredentialKind: credentialKindCLIToken,
		APIBase:        "https://api.soroq.dev",
		OperatorEmail:  "op@example.com",
		CLIToken:       "stored-cli-token-abc",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	env, err := engineLaneDelegateEnv([]string{"--api", "https://soroq.dev/api"})
	if err == nil {
		t.Fatal("forwarded a stored credential to a control plane it was not issued for")
	}
	// The token must not be in the returned environment either — a caller that ignored the error
	// would otherwise still leak it.
	if strings.Contains(strings.Join(env, "\n"), "stored-cli-token-abc") {
		t.Fatal("credential present in the env despite the refusal")
	}
	for _, want := range []string{"https://api.soroq.dev", "https://soroq.dev/api", "soroq login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%s", want, err)
		}
	}
}

// The refusal keys on the HOST, so a differing path or trailing slash on the same host is still the
// same control plane and must keep working.
func TestEngineLaneDelegateEnvAllowsSameHostDifferentPath(t *testing.T) {
	os.Unsetenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN")
	os.Unsetenv("SOROQ_OPERATOR_TOKEN")
	dir := t.TempDir()
	cfg := dir + "/config.json"
	t.Setenv("SOROQ_CONFIG", cfg)
	if err := saveAuthConfig(cfg, authConfig{
		SchemaVersion:  1,
		CredentialKind: credentialKindCLIToken,
		APIBase:        "https://api.soroq.dev",
		CLIToken:       "stored-cli-token-abc",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	env, err := engineLaneDelegateEnv([]string{"--api", "https://api.soroq.dev/"})
	if err != nil {
		t.Fatalf("same host refused: %v", err)
	}
	if !strings.Contains(strings.Join(env, "\n"), "stored-cli-token-abc") {
		t.Fatal("credential not forwarded to its own control plane")
	}
}
