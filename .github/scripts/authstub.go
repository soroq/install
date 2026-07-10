// Command authstub is a local, loopback-only HTTP test stub used ONLY by CI (.github/workflows/ci.yml).
//
// It serves the CLI auth exchange/whoami/revoke endpoints so `soroq login`'s loopback callback can
// round-trip WITHOUT contacting any real Google/prod service, and optionally serves static release
// assets (set STUB_ROOT) for the install.ps1 dry test. It binds 127.0.0.1:0 and writes its chosen
// host:port to the file named by STUB_PORTFILE. It never makes an outbound network call.
//
// This file lives under .github/scripts/ (NOT backend/) so it is excluded from the public CLI source
// slice and never trips the public-cli-drift check. stdlib only; no module dependencies.
package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/cli/auth/exchange", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"token": "test-cli-token-abc123", "email": "tester@example.com",
			"scopes": []string{"cli"}, "token_type": "bearer",
		})
	})
	mux.HandleFunc("/v1/cli/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"email": "tester@example.com", "scopes": []string{"cli"}, "kind": "cli_token"})
	})
	mux.HandleFunc("/v1/cli/auth/revoke", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	if root := os.Getenv("STUB_ROOT"); root != "" {
		mux.Handle("/", http.FileServer(http.Dir(root)))
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	if pf := os.Getenv("STUB_PORTFILE"); pf != "" {
		_ = os.WriteFile(pf, []byte(ln.Addr().String()), 0o644)
	}
	_ = http.Serve(ln, mux)
}
