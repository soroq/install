package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"soroq/backend/internal/domain"
)

type appCreateSummary struct {
	ProjectDir string                  `json:"project_dir,omitempty"`
	Request    domain.CreateAppRequest `json:"request"`
	Response   domain.App              `json:"response"`
	Created    bool                    `json:"created"`
}

type appStatusSummary struct {
	ProjectDir string     `json:"project_dir,omitempty"`
	AppID      string     `json:"app_id"`
	Response   domain.App `json:"response"`
}

type appListSummary struct {
	Count int          `json:"count"`
	Apps  []domain.App `json:"apps"`
}

func runApp(args []string) error {
	if len(args) == 0 {
		appUsage()
		return errAlreadyPrinted
	}

	switch args[0] {
	case "create":
		return runAppCreate(args[1:])
	case "list":
		return runAppList(args[1:])
	case "status":
		return runAppStatus(args[1:])
	case "-h", "--help", "help":
		appUsage()
		return nil
	default:
		printUnknownSubcommand(os.Stderr, "app", args[0], []string{"create", "list", "status"})
		return errAlreadyPrinted
	}
}

func appUsage() {
	printCommandUsage(os.Stdout,
		"Soroq Apps",
		"Register and inspect apps in the hosted control plane.",
		"soroq app <command> [flags]",
		[]usageSection{{
			Title: "Commands",
			Rows: []usageRow{
				{Name: "create", Description: "Register a Soroq app in the control plane."},
				{Name: "list", Description: "List registered Soroq apps."},
				{Name: "status", Description: "Inspect a registered Soroq app in the control plane."},
			},
		}},
		[]string{
			"soroq app create --name \"My App\" --app-id com.example.app",
			"soroq app status --app-id com.example.app",
		},
	)
}

func addAppCreateHint(err error, appID string) error {
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "unknown app") {
		return err
	}
	appID = strings.TrimSpace(appID)
	if appID == "" {
		appID = "<app-id>"
	}
	return fmt.Errorf("%w\nNext step: register the app first with `soroq app create --name \"Your App\" --app-id %s`.", err, appID)
}

func runAppCreate(args []string) error {
	fs := flag.NewFlagSet("app create", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	appID := fs.String("app-id", "", "app id override (defaults from soroq.yaml)")
	name := fs.String("name", "", "display name")
	ifNotExists := fs.Bool("if-not-exists", false, "succeed when the app is already registered")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq app create --name "My App" [--project-dir .] [--app-id com.example.app] [--api https://soroq-control-plane.fly.dev] [--if-not-exists] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	resolvedName := strings.TrimSpace(*name)
	if resolvedName == "" {
		return errors.New("--name is required")
	}

	status, resolvedAppID, err := resolveAppIDForProject(*projectDir, *appID)
	if err != nil {
		return err
	}

	req := domain.CreateAppRequest{
		ID:          resolvedAppID,
		DisplayName: resolvedName,
	}
	apiBaseURL := strings.TrimRight(*apiBase, "/")
	app, err := postJSONDecode[domain.App](apiBaseURL+"/v1/apps", req)
	created := true
	if err != nil {
		if !*ifNotExists {
			return err
		}
		app, err = getJSONDecode[domain.App](appURL(apiBaseURL, resolvedAppID))
		if err != nil {
			return err
		}
		created = false
	}

	summary := appCreateSummary{
		ProjectDir: status.ProjectDir,
		Request:    req,
		Response:   app,
		Created:    created,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	if created {
		fmt.Fprintf(os.Stdout, "Registered Soroq app %s\n", app.ID)
	} else {
		fmt.Fprintf(os.Stdout, "Soroq app %s already exists\n", app.ID)
	}
	fmt.Fprintf(os.Stdout, "display_name: %s\n", app.DisplayName)
	return nil
}

func runAppStatus(args []string) error {
	fs := flag.NewFlagSet("app status", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	appID := fs.String("app-id", "", "app id override (defaults from soroq.yaml)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq app status [--project-dir .] [--app-id com.example.app] [--api https://soroq-control-plane.fly.dev] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	status, resolvedAppID, err := resolveAppIDForProject(*projectDir, *appID)
	if err != nil {
		return err
	}
	app, err := getJSONDecode[domain.App](appURL(strings.TrimRight(*apiBase, "/"), resolvedAppID))
	if err != nil {
		return err
	}

	summary := appStatusSummary{
		ProjectDir: status.ProjectDir,
		AppID:      resolvedAppID,
		Response:   app,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Soroq app %s\n", app.ID)
	fmt.Fprintf(os.Stdout, "display_name: %s\n", app.DisplayName)
	return nil
}

func runAppList(args []string) error {
	fs := flag.NewFlagSet("app list", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq app list [--api https://soroq-control-plane.fly.dev] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	apps, err := getJSONDecode[[]domain.App](strings.TrimRight(*apiBase, "/") + "/v1/apps")
	if err != nil {
		return err
	}
	summary := appListSummary{
		Count: len(apps),
		Apps:  apps,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Soroq apps: %d\n", len(apps))
	for _, app := range apps {
		fmt.Fprintf(os.Stdout, "- %s\t%s\n", app.ID, app.DisplayName)
	}
	return nil
}

func resolveAppIDForProject(projectDir string, appID string) (projectStatus, string, error) {
	status, err := inspectProject(projectDir)
	if err != nil {
		return projectStatus{}, "", err
	}

	resolvedAppID := strings.TrimSpace(appID)
	if resolvedAppID == "" {
		if !status.HasSoroqConfig {
			return projectStatus{}, "", fmt.Errorf("--app-id is required because soroq.yaml was not found in %s", status.ProjectDir)
		}
		if strings.TrimSpace(status.AppID) == "" {
			return projectStatus{}, "", fmt.Errorf("--app-id is required because soroq.yaml at %s is missing app_id", status.SoroqConfigPath)
		}
		if !status.AppIDLooksValid {
			return projectStatus{}, "", fmt.Errorf("soroq.yaml app_id %q should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens", status.AppID)
		}
		resolvedAppID = status.AppID
	} else if !looksLikeSoroqAppID(resolvedAppID) {
		return projectStatus{}, "", fmt.Errorf("--app-id %q should be a stable Soroq app id using letters, numbers, dots, underscores, or hyphens", resolvedAppID)
	} else if status.HasSoroqConfig && status.AppID != "" && status.AppIDLooksValid && status.AppID != resolvedAppID {
		return projectStatus{}, "", fmt.Errorf("--app-id %q does not match soroq.yaml app_id %q", resolvedAppID, status.AppID)
	}

	return status, resolvedAppID, nil
}

func appURL(apiBase string, appID string) string {
	return strings.TrimRight(apiBase, "/") + "/v1/apps/" + url.PathEscape(appID)
}
