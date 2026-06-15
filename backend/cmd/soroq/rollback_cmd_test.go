package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"soroq/backend/internal/domain"
)

func TestRunRollbackPrintsRolledBackPatch(t *testing.T) {
	patch := domain.Patch{
		ID:         "patch-1",
		AppID:      "com.example.app",
		ReleaseID:  "release-1",
		RuntimeID:  "runtime-1",
		Number:     7,
		Channel:    "stable",
		RolledBack: true,
		CreatedAt:  time.Unix(0, 0).UTC(),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/patches/patch-1/rollback" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(patch); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		if err := runRollback([]string{"--api", server.URL, "--patch-id", "patch-1"}); err != nil {
			t.Fatalf("runRollback() error = %v", err)
		}
	})

	if !strings.Contains(stdout, "Rolled back patch patch-1") {
		t.Fatalf("expected rollback headline, got %q", stdout)
	}
	if !strings.Contains(stdout, "rolled_back: yes") {
		t.Fatalf("expected rolled_back flag, got %q", stdout)
	}
}

func TestRunRollbackJSON(t *testing.T) {
	patch := domain.Patch{
		ID:         "patch-2",
		AppID:      "com.example.app",
		ReleaseID:  "release-2",
		RuntimeID:  "runtime-2",
		Number:     8,
		Channel:    "beta",
		RolledBack: true,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(patch); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		if err := runRollback([]string{"--api", server.URL, "--patch-id", "patch-2", "--json"}); err != nil {
			t.Fatalf("runRollback() error = %v", err)
		}
	})

	var payload domain.Patch
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; stdout=%q", err, stdout)
	}
	if payload.ID != "patch-2" || !payload.RolledBack {
		t.Fatalf("unexpected payload %+v", payload)
	}
}
