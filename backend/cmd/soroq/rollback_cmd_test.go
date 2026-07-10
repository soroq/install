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

func TestRunRollbackVerifiesPatchCheckSuppression(t *testing.T) {
	var capturedPatchCheck domain.PatchCheckRequest
	patch := domain.Patch{
		ID:             "patch-1",
		AppID:          "com.example.app",
		ReleaseID:      "release-1",
		RuntimeID:      "runtime-1",
		Number:         7,
		Channel:        "stable",
		Kind:           domain.PatchKindExperimentalNativeAOT,
		ActivationMode: domain.ActivationNextColdStart,
		RolledBack:     true,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches/patch-1/rollback":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(patch); err != nil {
				t.Fatalf("Encode(rollback) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patch-check":
			if auth := r.Header.Get("Authorization"); auth != "" {
				t.Fatalf("runtime patch-check must not use operator auth, got %q", auth)
			}
			if err := json.NewDecoder(r.Body).Decode(&capturedPatchCheck); err != nil {
				t.Fatalf("Decode(patch-check) error = %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.PatchCheckResponse{
				PatchAvailable:         false,
				RolledBackPatchNumbers: []int{7},
			}); err != nil {
				t.Fatalf("Encode(patch-check) error = %v", err)
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		if err := runRollback([]string{
			"--api", server.URL,
			"--patch-id", "patch-1",
			"--verify",
			"--verify-client-id", "device-a",
			"--verify-current-patch-number", "0",
		}); err != nil {
			t.Fatalf("runRollback() error = %v", err)
		}
	})

	if capturedPatchCheck.AppID != patch.AppID {
		t.Fatalf("unexpected patch-check app id %q", capturedPatchCheck.AppID)
	}
	if capturedPatchCheck.ReleaseID != patch.ReleaseID {
		t.Fatalf("unexpected patch-check release id %q", capturedPatchCheck.ReleaseID)
	}
	if capturedPatchCheck.RuntimeID != patch.RuntimeID {
		t.Fatalf("unexpected patch-check runtime id %q", capturedPatchCheck.RuntimeID)
	}
	if capturedPatchCheck.Channel != patch.Channel {
		t.Fatalf("unexpected patch-check channel %q", capturedPatchCheck.Channel)
	}
	if capturedPatchCheck.Kind != patch.Kind {
		t.Fatalf("unexpected patch-check kind %q", capturedPatchCheck.Kind)
	}
	if capturedPatchCheck.ClientID != "device-a" {
		t.Fatalf("unexpected patch-check client id %q", capturedPatchCheck.ClientID)
	}
	if !strings.Contains(stdout, "rollback_verified: yes") {
		t.Fatalf("expected verified output, got %q", stdout)
	}
	if !strings.Contains(stdout, "rolled_back_number_present: yes") {
		t.Fatalf("expected rolled-back number output, got %q", stdout)
	}
}

func TestRunRollbackVerifyFailsWhenPatchCheckStillOffersPatch(t *testing.T) {
	patch := domain.Patch{
		ID:             "patch-1",
		AppID:          "com.example.app",
		ReleaseID:      "release-1",
		RuntimeID:      "runtime-1",
		Number:         7,
		Channel:        "stable",
		Kind:           domain.PatchKindAsset,
		ActivationMode: domain.ActivationNextColdStart,
		RolledBack:     true,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches/patch-1/rollback":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(patch); err != nil {
				t.Fatalf("Encode(rollback) error = %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patch-check":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.PatchCheckResponse{
				PatchAvailable: true,
				Patch: &domain.PatchDescriptor{
					ID:             "patch-1",
					Number:         7,
					ManifestURL:    "https://cdn.example.com/patch.json",
					BundleURL:      "https://cdn.example.com/patch.zip",
					ActivationMode: domain.ActivationNextColdStart,
					Kind:           domain.PatchKindAsset,
				},
				RolledBackPatchNumbers: []int{7},
			}); err != nil {
				t.Fatalf("Encode(patch-check) error = %v", err)
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	err := runRollback([]string{
		"--api", server.URL,
		"--patch-id", "patch-1",
		"--verify",
	})
	if err == nil {
		t.Fatalf("expected rollback verification failure")
	}
	if !strings.Contains(err.Error(), "still offers rolled-back patch") {
		t.Fatalf("expected still-offered error, got %v", err)
	}
}
