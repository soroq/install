package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"soroq/backend/internal/domain"
)

// doctorCheck is a single environment/project health result. Status is one of
// "ok", "warn", "error", or "skip". Fix is the exact next command/edit to resolve it.
type doctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

type doctorReport struct {
	ProjectDir string        `json:"project_dir"`
	Checks     []doctorCheck `json:"checks"`
	Warnings   int           `json:"warnings"`
	Errors     int           `json:"errors"`
	OK         bool          `json:"ok"`
}

// runDoctor diagnoses the local environment + Flutter project for the iOS dart_eval
// patch-point OTA lane. It does NOT claim Shorebird parity or App-Store safety — it only
// reports whether the toolchain, signing, project config, and control-plane auth are ready.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	projectDir := fs.String("project-dir", ".", "Flutter app directory")
	apiBase := fs.String("api", "", "control plane base URL")
	configPath := fs.String("config", "", "credential config path")
	offline := fs.Bool("offline", false, "skip the network control-plane auth verification")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq doctor [--project-dir .] [--api https://api.soroq.dev] [--config ~/.soroq/config.json] [--offline] [--json]

Checks the local environment + project for the iOS dart_eval patch-point OTA lane:
Flutter project + soroq.yaml, soroq_flutter dependency + version, iOS bundle id, Xcode,
Apple signing team, and control-plane auth. Exits non-zero if any check is an error.
(Patch-point lane only — not Shorebird parity, not an App-Store-safe guarantee.)`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	absDir, err := filepath.Abs(*projectDir)
	if err != nil {
		return err
	}
	report := doctorReport{ProjectDir: absDir}

	// Inspect the project once; reuse for both the project checks and the app_id-scoped
	// release-version check.
	status, projectErr := inspectProject(absDir)
	if projectErr != nil {
		report.Checks = append(report.Checks, doctorCheck{Name: "Project", Status: "error",
			Message: projectErr.Error(),
			Fix:     "run from a Flutter app directory, or pass --project-dir <path>"})
	} else {
		report.Checks = append(report.Checks, doctorProjectChecks(status)...)
	}
	report.Checks = append(report.Checks, doctorSoroqFlutterVersion(absDir))
	report.Checks = append(report.Checks, doctorIOSBundleID(absDir))
	if runtime.GOOS == "darwin" {
		report.Checks = append(report.Checks, doctorXcode())
		report.Checks = append(report.Checks, doctorSigningTeam(absDir))
	} else {
		report.Checks = append(report.Checks,
			doctorCheck{Name: "Xcode", Status: "skip", Message: "not on macOS"},
			doctorCheck{Name: "Apple signing team", Status: "skip", Message: "not on macOS"})
	}
	report.Checks = append(report.Checks, doctorControlPlaneAuth(*apiBase, *configPath, *offline))
	report.Checks = append(report.Checks, doctorReleaseVersion(*apiBase, status.AppID, *offline))
	report.Checks = append(report.Checks, doctorToolchainStatus())

	okCount := 0
	for _, c := range report.Checks {
		switch c.Status {
		case "warn":
			report.Warnings++
		case "error":
			report.Errors++
		case "ok":
			okCount++
		}
	}
	report.OK = report.Errors == 0

	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
		if report.Errors > 0 {
			return errAlreadyPrinted
		}
		return nil
	}

	fmt.Fprintf(os.Stdout, "soroq doctor — %s\n\n", report.ProjectDir)
	for _, c := range report.Checks {
		fmt.Fprintf(os.Stdout, "%s %s", doctorIcon(c.Status), c.Name)
		if c.Message != "" {
			fmt.Fprintf(os.Stdout, ": %s", c.Message)
		}
		fmt.Fprintln(os.Stdout)
		if c.Fix != "" {
			fmt.Fprintf(os.Stdout, "   → %s\n", c.Fix)
		}
	}
	fmt.Fprintf(os.Stdout, "\n%d ok, %d warning(s), %d error(s)\n", okCount, report.Warnings, report.Errors)
	if report.Errors > 0 {
		fmt.Fprintln(os.Stdout, "status: not ready — fix the error(s) above")
		return errAlreadyPrinted
	}
	fmt.Fprintln(os.Stdout, "status: ready")
	return nil
}

func doctorIcon(status string) string {
	switch status {
	case "ok":
		return "✓" // ✓
	case "warn":
		return "!"
	case "error":
		return "✗" // ✗
	default:
		return "–" // – (skip)
	}
}

// doctorProjectChecks maps an inspectProject result to Flutter/soroq.yaml/dependency checks.
func doctorProjectChecks(status projectStatus) []doctorCheck {
	var out []doctorCheck
	if status.HasPubspec {
		out = append(out, doctorCheck{Name: "Flutter project", Status: "ok", Message: "pubspec.yaml found"})
	} else {
		out = append(out, doctorCheck{Name: "Flutter project", Status: "error",
			Message: "no pubspec.yaml in project dir",
			Fix:     "run from a Flutter app directory, or pass --project-dir <path>"})
	}
	if status.HasSoroqConfig {
		msg := "found"
		st := "ok"
		if status.AppID != "" {
			msg = fmt.Sprintf("app_id=%s channel=%s", status.AppID, status.Channel)
			if !status.AppIDLooksValid {
				st, msg = "warn", msg+" (app_id looks invalid)"
			} else if status.Channel != "" && !status.ChannelLooksValid {
				st, msg = "warn", msg+" (channel looks invalid)"
			}
		}
		out = append(out, doctorCheck{Name: "soroq.yaml", Status: st, Message: msg})
	} else {
		out = append(out, doctorCheck{Name: "soroq.yaml", Status: "error",
			Message: "missing", Fix: "run: soroq init"})
	}
	if status.HasSoroqFlutterDependency {
		out = append(out, doctorCheck{Name: "soroq_flutter dependency", Status: "ok"})
	} else {
		out = append(out, doctorCheck{Name: "soroq_flutter dependency", Status: "error",
			Message: "not declared in pubspec.yaml", Fix: "run: flutter pub add soroq_flutter"})
	}
	return out
}

var soroqFlutterVersionPattern = regexp.MustCompile(`(?m)^\s*soroq_flutter:\s*([^\s#{][^\s#]*)`)

func doctorSoroqFlutterVersion(dir string) doctorCheck {
	bytes, err := os.ReadFile(filepath.Join(dir, "pubspec.yaml"))
	if err != nil {
		return doctorCheck{Name: "soroq_flutter version", Status: "warn", Message: "pubspec.yaml not readable"}
	}
	match := soroqFlutterVersionPattern.FindSubmatch(bytes)
	if match == nil {
		return doctorCheck{Name: "soroq_flutter version", Status: "warn",
			Message: "no pinned version (sdk/path/git dependency?)"}
	}
	return doctorCheck{Name: "soroq_flutter version", Status: "ok", Message: strings.TrimSpace(string(match[1]))}
}

func doctorIOSBundleID(dir string) doctorCheck {
	id, err := inferIOSBundleIdentifier(dir)
	if err != nil {
		return doctorCheck{Name: "iOS bundle id", Status: "warn", Message: err.Error(),
			Fix: "set a literal PRODUCT_BUNDLE_IDENTIFIER in ios/Runner.xcodeproj"}
	}
	return doctorCheck{Name: "iOS bundle id", Status: "ok", Message: id}
}

func doctorXcode() doctorCheck {
	out, err := exec.Command("xcodebuild", "-version").Output()
	if err != nil {
		return doctorCheck{Name: "Xcode", Status: "error", Message: "xcodebuild not found",
			Fix: "install Xcode from the App Store, then: sudo xcode-select -s /Applications/Xcode.app"}
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return doctorCheck{Name: "Xcode", Status: "ok", Message: line}
}

var developmentTeamPattern = regexp.MustCompile(`(?m)DEVELOPMENT_TEAM\s*=\s*"?([A-Z0-9]{10})"?`)

func doctorSigningTeam(dir string) doctorCheck {
	// Local.xcconfig (gitignored) is where this lane parameterizes the team; fall back to
	// the committed pbxproj.
	for _, path := range []string{
		filepath.Join(dir, "ios", "Flutter", "Local.xcconfig"),
		filepath.Join(dir, "ios", "Runner.xcodeproj", "project.pbxproj"),
	} {
		bytes, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if match := developmentTeamPattern.FindSubmatch(bytes); match != nil {
			return doctorCheck{Name: "Apple signing team", Status: "ok",
				Message: fmt.Sprintf("%s (%s)", match[1], filepath.Base(path))}
		}
	}
	return doctorCheck{Name: "Apple signing team", Status: "warn", Message: "DEVELOPMENT_TEAM not set",
		Fix: "set DEVELOPMENT_TEAM in ios/Flutter/Local.xcconfig (copy ios/Flutter/Local.xcconfig.example)"}
}

// doctorReleaseVersion reports the latest release registered for the project's app_id in the
// control plane (a public GET /v1/releases). Skips cleanly when offline or when soroq.yaml
// has no app_id; warns (with a fix) when nothing is registered yet.
func doctorReleaseVersion(apiBase, appID string, offline bool) doctorCheck {
	if offline {
		return doctorCheck{Name: "Registered release", Status: "skip", Message: "offline"}
	}
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return doctorCheck{Name: "Registered release", Status: "skip", Message: "no app_id in soroq.yaml"}
	}
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = defaultControlPlaneAPI
	}
	listURL := base + "/v1/releases?app_id=" + url.QueryEscape(appID)
	releases, err := getJSONDecode[[]domain.Release](listURL)
	if err != nil {
		return doctorCheck{Name: "Registered release", Status: "warn",
			Message: "could not query releases: " + err.Error(),
			Fix:     "check --api (and `soroq login` if the control plane requires auth)"}
	}
	if len(releases) == 0 {
		return doctorCheck{Name: "Registered release", Status: "warn",
			Message: "none registered for " + appID, Fix: "run: soroq release ios"}
	}
	latest := releases[0]
	for _, r := range releases[1:] {
		if r.CreatedAt.After(latest.CreatedAt) {
			latest = r
		}
	}
	return doctorCheck{Name: "Registered release", Status: "ok",
		Message: fmt.Sprintf("%s (%s, channel %s; %d total)", latest.Version, latest.ID, latest.Channel, len(releases))}
}

// doctorToolchainStatus summarizes the hosted build-time engine toolchain cache for `soroq doctor`:
// how many toolchains are installed under ~/.soroq/toolchains/ and whether their signature + identity
// check out. Offline + read-only (never touches the network or any unrelated Flutter install).
func doctorToolchainStatus() doctorCheck {
	installed, err := listInstalledToolchains()
	if err != nil {
		return doctorCheck{Name: "Engine toolchain", Status: "warn", Message: err.Error()}
	}
	if len(installed) == 0 {
		return doctorCheck{Name: "Engine toolchain", Status: "warn",
			Message: "none installed", Fix: "run: soroq toolchain install <version> --api <base>"}
	}
	valid, bad := 0, 0
	for _, it := range installed {
		if it.SignatureValid && it.IdentityOK {
			valid++
		} else {
			bad++
		}
	}
	if bad > 0 {
		return doctorCheck{Name: "Engine toolchain", Status: "error",
			Message: fmt.Sprintf("%d installed, %d failing signature/identity", len(installed), bad),
			Fix:     "re-install: soroq toolchain install <version> --api <base> --force"}
	}
	return doctorCheck{Name: "Engine toolchain", Status: "ok",
		Message: fmt.Sprintf("%d installed + signature-valid (run `soroq toolchain list` for detail)", valid)}
}

func doctorControlPlaneAuth(apiBase, configPath string, offline bool) doctorCheck {
	// Scope the credential refresh to the control plane doctor is actually probing
	// so a local/non-prod --api doctor run never rewrites the stored prod credential.
	creds, err := currentOperatorCredentialsForRequest(configPath, strings.TrimRight(strings.TrimSpace(apiBase), "/"))
	if err != nil {
		return doctorCheck{Name: "Control-plane auth", Status: "error", Message: err.Error(),
			Fix: "fix or remove ~/.soroq/config.json, then run: soroq login"}
	}
	if strings.TrimSpace(creds.Token) == "" {
		return doctorCheck{Name: "Control-plane auth", Status: "warn", Message: "not logged in",
			Fix: "run: soroq login   (or set SOROQ_CONTROL_PLANE_OPERATOR_TOKEN)"}
	}
	if offline {
		return doctorCheck{Name: "Control-plane auth", Status: "ok",
			Message: "credentials present (offline — not verified against the control plane)"}
	}
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(creds.APIBase), "/")
	}
	if base == "" {
		base = defaultControlPlaneAPI
	}
	count, err := verifyOperatorCredentials(base, creds)
	if err != nil {
		return doctorCheck{Name: "Control-plane auth", Status: "error",
			Message: "verification failed: " + err.Error(),
			Fix:     "check --api and re-run: soroq login"}
	}
	return doctorCheck{Name: "Control-plane auth", Status: "ok",
		Message: fmt.Sprintf("verified at %s (%d app(s) visible)", base, count)}
}
