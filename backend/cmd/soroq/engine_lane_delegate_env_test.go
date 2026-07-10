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
	env := engineLaneDelegateEnv([]string{"--api", "https://api.soroq.dev"})
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
	env := engineLaneDelegateEnv([]string{"--api", "https://api.soroq.dev"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "SOROQ_CONTROL_PLANE_OPERATOR_TOKEN=stored-cli-token-abc") {
		t.Fatal("stored cli_token not injected into delegate env")
	}
	// A cli_token's email is bound server-side; the email header must NOT be forwarded for it.
	if strings.Contains(joined, "SOROQ_OPERATOR_EMAIL=") {
		t.Fatal("must not forward SOROQ_OPERATOR_EMAIL for a cli_token")
	}
}
