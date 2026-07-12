package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"soroq/backend/internal/domain"
)

// isolateRollbackEnv points HOME at a temp dir and clears operator tokens so the /v1/patches list
// (which flows through applyOperatorHeaders) never reads the developer's real ~/.soroq/config.json.
func isolateRollbackEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("SOROQ_API", "")
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "")
	t.Setenv("SOROQ_OPERATOR_TOKEN", "")
}

func writeReadyProject(t *testing.T, dir string) {
	t.Helper()
	writeSoroqFlutterPubspec(t, dir)
	writeFile(t, filepath.Join(dir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))
}

// TestRunRollbackConfigLaneResolvesNewestPatch proves `soroq rollback android` (no --patch-id)
// resolves app_id/channel/release from local state, lists /v1/patches, and rolls back the NEWEST
// non-rolled-back patch (excluding already-rolled-back patches even when they are newer).
func TestRunRollbackConfigLaneResolvesNewestPatch(t *testing.T) {
	isolateRollbackEnv(t)
	dir := t.TempDir()
	writeReadyProject(t, dir)
	if err := saveProjectCLIState(dir, projectCLIState{
		SchemaVersion: 1,
		LastAndroidRelease: &androidReleaseState{
			UpdatedAt: time.Now().UTC(),
			AppID:     "com.example.app",
			Channel:   "stable",
			ReleaseID: "release-1",
		},
	}); err != nil {
		t.Fatalf("saveProjectCLIState() error = %v", err)
	}

	base := time.Unix(1_700_000_000, 0).UTC()
	patches := []domain.Patch{
		{ID: "patch-old", AppID: "com.example.app", ReleaseID: "release-1", Channel: "stable", Number: 1, CreatedAt: base},
		{ID: "patch-new", AppID: "com.example.app", ReleaseID: "release-1", Channel: "stable", Number: 2, CreatedAt: base.Add(2 * time.Hour)},
		// Newer by time, but already rolled back → must be skipped.
		{ID: "patch-rolled", AppID: "com.example.app", ReleaseID: "release-1", Channel: "stable", Number: 3, CreatedAt: base.Add(3 * time.Hour), RolledBack: true},
	}

	var rolledBackID atomic.Value
	var sawList atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/patches":
			sawList.Store(true)
			q := r.URL.Query()
			if got := q.Get("release_id"); got != "release-1" {
				t.Errorf("list release_id = %q, want release-1", got)
			}
			if got := q.Get("app_id"); got != "com.example.app" {
				t.Errorf("list app_id = %q, want com.example.app", got)
			}
			if got := q.Get("channel"); got != "stable" {
				t.Errorf("list channel = %q, want stable", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(patches)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/patches/") && strings.HasSuffix(r.URL.Path, "/rollback"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/patches/"), "/rollback")
			rolledBackID.Store(id)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Patch{ID: id, AppID: "com.example.app", ReleaseID: "release-1", Channel: "stable", Number: 2, RolledBack: true})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		if err := runRollback([]string{"android", "--api", server.URL, "--project-dir", dir}); err != nil {
			t.Fatalf("runRollback() error = %v", err)
		}
	})

	if !sawList.Load() {
		t.Fatalf("expected a GET /v1/patches list request")
	}
	if got, _ := rolledBackID.Load().(string); got != "patch-new" {
		t.Fatalf("rolled back patch id = %q, want patch-new (newest non-rolled-back)", got)
	}
	if !strings.Contains(stdout, "Rolled back patch patch-new") {
		t.Fatalf("expected headline for patch-new, got %q", stdout)
	}
}

// TestRunRollbackConfigLaneReleaseFromLock proves the soroq.lock pin supplies release_id when
// cli-state has none.
func TestRunRollbackConfigLaneReleaseFromLock(t *testing.T) {
	isolateRollbackEnv(t)
	dir := t.TempDir()
	writeReadyProject(t, dir)
	if err := recordSoroqLockPin(dir, "android", soroqLockPin{ReleaseID: "release-lock", Version: "1.0.0+1", ToolchainVersion: "soroq-android-r5"}); err != nil {
		t.Fatalf("recordSoroqLockPin() error = %v", err)
	}

	var listedRelease atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/patches":
			listedRelease.Store(r.URL.Query().Get("release_id"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]domain.Patch{
				{ID: "patch-lock", AppID: "com.example.app", ReleaseID: "release-lock", Channel: "stable", Number: 1, CreatedAt: time.Unix(1, 0).UTC()},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches/patch-lock/rollback":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Patch{ID: "patch-lock", RolledBack: true})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	captureStdout(t, func() {
		if err := runRollback([]string{"android", "--api", server.URL, "--project-dir", dir}); err != nil {
			t.Fatalf("runRollback() error = %v", err)
		}
	})
	if got, _ := listedRelease.Load().(string); got != "release-lock" {
		t.Fatalf("listed release_id = %q, want release-lock (from soroq.lock)", got)
	}
}

// TestRunRollbackConfigLanePatchIDOverride proves an explicit --patch-id is used verbatim and skips
// the list/resolution step.
func TestRunRollbackConfigLanePatchIDOverride(t *testing.T) {
	isolateRollbackEnv(t)
	dir := t.TempDir()
	writeReadyProject(t, dir)

	var listed atomic.Bool
	var rolledBackID atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/patches":
			listed.Store(true)
			t.Fatalf("--patch-id override must NOT list /v1/patches")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/patches/patch-explicit/rollback":
			rolledBackID.Store("patch-explicit")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Patch{ID: "patch-explicit", RolledBack: true})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	captureStdout(t, func() {
		if err := runRollback([]string{"android", "--api", server.URL, "--project-dir", dir, "--patch-id", "patch-explicit"}); err != nil {
			t.Fatalf("runRollback() error = %v", err)
		}
	})
	if listed.Load() {
		t.Fatalf("expected no list request with --patch-id override")
	}
	if got, _ := rolledBackID.Load().(string); got != "patch-explicit" {
		t.Fatalf("rolled back id = %q, want patch-explicit", got)
	}
}

// TestRunRollbackIOSEngineStillDelegates proves `rollback ios-engine` still routes to the soroqctl
// engine-lane delegate (unchanged) rather than the new config-lane wrapper. With no soroqctl on PATH
// the delegate fails with its own distinctive error — proof the delegation branch was reached.
func TestRunRollbackIOSEngineStillDelegates(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no soroqctl discoverable
	err := runRollback([]string{"ios-engine", "--channel", "stable"})
	if err == nil {
		t.Fatalf("expected an error from the engine-lane delegate")
	}
	if !strings.Contains(err.Error(), "soroqctl") {
		t.Fatalf("expected the soroqctl engine-lane delegate error, got %v", err)
	}
}

// TestRunRollbackLegacyPatchIDUnchanged guards the original (no-positional) advanced path.
func TestRunRollbackLegacyPatchIDUnchanged(t *testing.T) {
	isolateRollbackEnv(t)
	var rolledBackID atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/patches/patch-legacy/rollback" {
			rolledBackID.Store("patch-legacy")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.Patch{ID: "patch-legacy", RolledBack: true})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	captureStdout(t, func() {
		if err := runRollback([]string{"--api", server.URL, "--patch-id", "patch-legacy"}); err != nil {
			t.Fatalf("runRollback() error = %v", err)
		}
	})
	if got, _ := rolledBackID.Load().(string); got != "patch-legacy" {
		t.Fatalf("rolled back id = %q, want patch-legacy", got)
	}
}
