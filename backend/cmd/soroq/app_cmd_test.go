package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"soroq/backend/internal/domain"
)

func TestRunAppCreateUsesSoroqYamlAppID(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	var captured domain.CreateAppRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.App{
			ID:          captured.ID,
			DisplayName: captured.DisplayName,
		}); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runAppCreate([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--name", "Example App",
		})
		if err != nil {
			t.Fatalf("runAppCreate() error = %v", err)
		}
	})

	if captured.ID != "com.example.app" {
		t.Fatalf("expected app id from soroq.yaml, got %q", captured.ID)
	}
	if captured.DisplayName != "Example App" {
		t.Fatalf("expected display name, got %q", captured.DisplayName)
	}
	if !strings.Contains(stdout, "Registered Soroq app com.example.app") {
		t.Fatalf("expected registration output, got %q", stdout)
	}
}

func TestRunAppCreateSendsOperatorHeadersFromEnvironment(t *testing.T) {
	t.Setenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN", "cli-secret")
	t.Setenv("SOROQ_OPERATOR_EMAIL", "owner@example.com")

	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cli-secret" {
			t.Fatalf("expected operator Authorization header, got %q", got)
		}
		if got := r.Header.Get("X-Soroq-Operator-Email"); got != "owner@example.com" {
			t.Fatalf("expected operator email header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.App{
			ID:          "com.example.app",
			DisplayName: "Example App",
			OwnerEmail:  "owner@example.com",
		}); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	defer server.Close()

	if err := runAppCreate([]string{
		"--project-dir", projectDir,
		"--api", server.URL,
		"--name", "Example App",
	}); err != nil {
		t.Fatalf("runAppCreate() error = %v", err)
	}
}

func TestRunAppListPrintsApps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]domain.App{
			{ID: "com.example.app", DisplayName: "Example App"},
		}); err != nil {
			t.Fatalf("Encode(app list) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runAppList([]string{
			"--api", server.URL,
		})
		if err != nil {
			t.Fatalf("runAppList() error = %v", err)
		}
	})

	if !strings.Contains(stdout, "Soroq apps: 1") {
		t.Fatalf("expected app count, got %q", stdout)
	}
	if !strings.Contains(stdout, "com.example.app") {
		t.Fatalf("expected app id, got %q", stdout)
	}
}

func TestRunAppCreateIfNotExistsFetchesExistingApp(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	var postCount int
	var getCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps":
			postCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			if err := json.NewEncoder(w).Encode(map[string]string{"error": `app "com.example.app" already exists`}); err != nil {
				t.Fatalf("Encode(error) error = %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/apps/com.example.app":
			getCount++
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(domain.App{
				ID:          "com.example.app",
				DisplayName: "Existing App",
			}); err != nil {
				t.Fatalf("Encode(app) error = %v", err)
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runAppCreate([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
			"--name", "Example App",
			"--if-not-exists",
			"--json",
		})
		if err != nil {
			t.Fatalf("runAppCreate() error = %v", err)
		}
	})

	var summary appCreateSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("Unmarshal(summary) error = %v; stdout=%q", err, stdout)
	}
	if summary.Created {
		t.Fatalf("expected Created=false for existing app")
	}
	if summary.Response.DisplayName != "Existing App" {
		t.Fatalf("expected fetched app response, got %+v", summary.Response)
	}
	if postCount != 1 || getCount != 1 {
		t.Fatalf("expected one create and one get, got post=%d get=%d", postCount, getCount)
	}
}

func TestCreateSoroqAppPostsCreateAndBindRequest(t *testing.T) {
	var captured domain.CreateAppRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.App{ID: captured.ID, DisplayName: captured.DisplayName}); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	defer server.Close()

	app, err := createSoroqApp(server.URL, domain.CreateAppRequest{ID: "com.example.app", DisplayName: "com.example.app"})
	if err != nil {
		t.Fatalf("createSoroqApp() error = %v", err)
	}
	if app.ID != "com.example.app" {
		t.Fatalf("expected created app id, got %q", app.ID)
	}
	if captured.ID != "com.example.app" || captured.DisplayName != "com.example.app" {
		t.Fatalf("expected create request, got %+v", captured)
	}
}

func TestRunAppStatusUsesSoroqYamlAppID(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/com.example.app" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(domain.App{
			ID:          "com.example.app",
			DisplayName: "Example App",
		}); err != nil {
			t.Fatalf("Encode(app) error = %v", err)
		}
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		err := runAppStatus([]string{
			"--project-dir", projectDir,
			"--api", server.URL,
		})
		if err != nil {
			t.Fatalf("runAppStatus() error = %v", err)
		}
	})

	if !strings.Contains(stdout, "Soroq app com.example.app") {
		t.Fatalf("expected status output, got %q", stdout)
	}
	if !strings.Contains(stdout, "display_name: Example App") {
		t.Fatalf("expected display name, got %q", stdout)
	}
}

func TestRunAppCreateRejectsMismatchedAppIDOverride(t *testing.T) {
	projectDir := t.TempDir()
	writeSoroqFlutterPubspec(t, projectDir)
	writeFile(t, filepath.Join(projectDir, "soroq.yaml"), testSoroqYAML("com.example.app", "stable"))

	err := runAppCreate([]string{
		"--project-dir", projectDir,
		"--app-id", "com.example.other",
		"--name", "Example App",
	})
	if err == nil {
		t.Fatalf("expected mismatched app id error")
	}
	if !strings.Contains(err.Error(), "does not match soroq.yaml") {
		t.Fatalf("expected mismatch guidance, got %v", err)
	}
}

func TestRunAppCreateRequiresAppIDWithoutSoroqYaml(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "pubspec.yaml"), "name: demo\n")

	err := runAppCreate([]string{
		"--project-dir", projectDir,
		"--name", "Example App",
	})
	if err == nil {
		t.Fatalf("expected missing app id error")
	}
	if !strings.Contains(err.Error(), "--app-id is required") {
		t.Fatalf("expected missing app id guidance, got %v", err)
	}
}
