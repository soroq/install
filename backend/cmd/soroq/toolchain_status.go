package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"soroq/backend/internal/signing"
)

// installedToolchain summarizes one cached toolchain version under ~/.soroq/toolchains/.
type installedToolchain struct {
	Version         string `json:"version"`
	Dir             string `json:"dir"`
	Platform        string `json:"platform,omitempty"`
	Arch            string `json:"arch,omitempty"`
	BuildMode       string `json:"build_mode,omitempty"`
	Tier            string `json:"tier,omitempty"`
	FlutterRevision string `json:"flutter_revision,omitempty"`
	DartRevision    string `json:"dart_revision,omitempty"`
	SignatureValid  bool   `json:"signature_valid"`
	IdentityOK      bool   `json:"identity_ok"`
	Note            string `json:"note,omitempty"`
}

// listInstalledToolchains scans the cache dir and validates each entry's cached manifest signature +
// identity (no network). It NEVER touches any unrelated Flutter install.
func listInstalledToolchains() ([]installedToolchain, error) {
	root, err := toolchainsRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []installedToolchain
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		versionDir := filepath.Join(root, e.Name())
		it := installedToolchain{Version: e.Name(), Dir: versionDir}
		manifestBytes, mErr := os.ReadFile(filepath.Join(versionDir, "manifest.json"))
		sigBytes, sErr := os.ReadFile(filepath.Join(versionDir, "manifest.sig"))
		if mErr != nil || sErr != nil {
			it.Note = "incomplete cache entry (missing manifest.json/manifest.sig)"
			out = append(out, it)
			continue
		}
		if err := signing.VerifyToolchainManifestSignature(manifestBytes, strings.TrimSpace(string(sigBytes)), pinnedToolchainPublicKeyHex()); err == nil {
			it.SignatureValid = true
		} else {
			it.Note = "signature invalid: " + err.Error()
		}
		if m, err := parseCLIManifest(manifestBytes); err == nil {
			it.Platform, it.Arch = m.Platform, m.Arch
			it.BuildMode, it.Tier = m.BuildMode, m.Tier
			it.FlutterRevision, it.DartRevision = m.FlutterRevision, m.DartRevision
			it.IdentityOK = checkToolchainIdentity(m) == nil
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func runToolchainList(args []string) error {
	fs := flag.NewFlagSet("toolchain list", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() { fmt.Fprintln(os.Stdout, `usage: soroq toolchain list [--json]`) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	installed, err := listInstalledToolchains()
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(installed)
	}
	root, _ := toolchainsRoot()
	if len(installed) == 0 {
		fmt.Fprintf(os.Stdout, "No toolchains installed under %s\n", root)
		fmt.Fprintln(os.Stdout, "  install one: soroq toolchain install <version> --api <base>")
		return nil
	}
	fmt.Fprintf(os.Stdout, "Installed toolchains (%s):\n", root)
	for _, it := range installed {
		marker := "✓"
		if !it.SignatureValid || !it.IdentityOK {
			marker = "✗"
		}
		fmt.Fprintf(os.Stdout, "  %s %s", marker, it.Version)
		if it.Platform != "" {
			fmt.Fprintf(os.Stdout, "  (%s/%s %s, tier=%s, flutter=%s)", it.Platform, it.Arch, it.BuildMode, it.Tier, short(it.FlutterRevision))
		}
		fmt.Fprintln(os.Stdout)
		if it.Note != "" {
			fmt.Fprintf(os.Stdout, "      note: %s\n", it.Note)
		}
	}
	return nil
}

// runToolchainDoctor reports toolchain availability + package/CLI-version compatibility:
//   - the CLI-pinned toolchain trust domain (key id) and the build-time identity it is wired for,
//   - locally installed toolchains and whether their signature + identity check out,
//   - (online) whether a requested --version is available in the registry and compatible,
//   - whether soroqctl (the UNCHANGED verifyEngineBundle gate) is resolvable.
func runToolchainDoctor(args []string) error {
	fs := flag.NewFlagSet("toolchain doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", "", "control plane base URL (registry); when set, probe --version availability")
	version := fs.String("version", "", "a specific toolchain version to probe for availability/compatibility")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq toolchain doctor [--api https://api.soroq.dev] [--version <v>] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	report := doctorReport{}
	report.Checks = append(report.Checks, toolchainTrustDomainCheck())
	report.Checks = append(report.Checks, toolchainSoroqFlutterFrontendCheck())
	report.Checks = append(report.Checks, toolchainSoroqctlCheck())
	report.Checks = append(report.Checks, toolchainInstalledChecks()...)
	if base := strings.TrimRight(strings.TrimSpace(*apiBase), "/"); base != "" && strings.TrimSpace(*version) != "" {
		report.Checks = append(report.Checks, toolchainAvailabilityCheck(base, strings.TrimSpace(*version)))
	} else if strings.TrimSpace(*version) != "" {
		report.Checks = append(report.Checks, doctorCheck{Name: "Toolchain availability", Status: "skip",
			Message: "pass --api to probe registry availability for " + strings.TrimSpace(*version)})
	}

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
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
		if report.Errors > 0 {
			return errAlreadyPrinted
		}
		return nil
	}
	fmt.Fprintln(os.Stdout, "soroq toolchain doctor")
	fmt.Fprintln(os.Stdout)
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
		return errAlreadyPrinted
	}
	return nil
}

func toolchainTrustDomainCheck() doctorCheck {
	return doctorCheck{Name: "Toolchain trust domain", Status: "ok",
		Message: fmt.Sprintf("pinned key %s; wired for ios(flutter=%s dart=%s) + android-candidate(flutter=%s dart=%s)",
			toolchainPinnedKeyID,
			short(expectedFlutterRevision), short(expectedDartRevision),
			short(expectedAndroidFlutterRevision), expectedAndroidDartRevision)}
}

// toolchainSoroqFlutterFrontendCheck HARD-FAILS when the Soroq Flutter frontend (the fork whose asset
// bundler is CANONICAL for soroq/soroq_metadata.json + runtime_id — see bundled_metadata.go) cannot be
// resolved. Every real Android/iOS build shells out to this binary; without it there is no Soroq build.
//
// D1.2 closed the distribution gap: resolveSoroqFlutterBin() now finds the frontend via (1)
// $SOROQ_FLUTTER_BIN, (2) a `soroq-flutter` on PATH, (3) a recorded `soroq frontend install` under
// ~/.soroq/frontends/, or (4) the legacy ~/development/soroq-forks checkout. The normal fresh-user path is
// (3) — no manual SOROQ_FLUTTER_BIN. doctor still fails loudly (with the install command) when none resolve,
// instead of letting a build die with a cryptic error deep in the toolchain.
func toolchainSoroqFlutterFrontendCheck() doctorCheck {
	bin, err := resolveSoroqFlutterBin()
	if err != nil {
		return doctorCheck{
			Name:    "Soroq Flutter frontend",
			Status:  "error",
			Message: "not found — the Soroq Flutter fork provides the canonical asset bundler (soroq_metadata.json / runtime_id)",
			Fix:     "soroq frontend install <version> --api " + defaultControlPlaneAPI,
		}
	}
	return doctorCheck{Name: "Soroq Flutter frontend", Status: "ok", Message: bin}
}

func toolchainSoroqctlCheck() doctorCheck {
	if bin, err := resolveSoroqctl(); err == nil {
		return doctorCheck{Name: "verifyEngineBundle gate (soroqctl)", Status: "ok", Message: bin}
	}
	return doctorCheck{Name: "verifyEngineBundle gate (soroqctl)", Status: "warn",
		Message: "soroqctl not found; install will refuse without it (or use --skip-bundle-verify)",
		Fix:     "build it: go build ./backend/cmd/soroqctl, then place it next to soroq or on PATH"}
}

func toolchainInstalledChecks() []doctorCheck {
	installed, err := listInstalledToolchains()
	if err != nil {
		return []doctorCheck{{Name: "Installed toolchains", Status: "error", Message: err.Error()}}
	}
	if len(installed) == 0 {
		root, _ := toolchainsRoot()
		return []doctorCheck{{Name: "Installed toolchains", Status: "warn",
			Message: "none under " + root, Fix: "soroq toolchain install <version> --api <base>"}}
	}
	var checks []doctorCheck
	for _, it := range installed {
		status := "ok"
		msg := fmt.Sprintf("%s/%s %s tier=%s flutter=%s", it.Platform, it.Arch, it.BuildMode, it.Tier, short(it.FlutterRevision))
		if !it.SignatureValid {
			status, msg = "error", "signature invalid — "+it.Note
		} else if !it.IdentityOK {
			status, msg = "warn", "identity incompatible with this CLI — "+msg
		}
		checks = append(checks, doctorCheck{Name: "Toolchain " + it.Version, Status: status, Message: msg})
	}
	return checks
}

func toolchainAvailabilityCheck(base, version string) doctorCheck {
	manifestBytes, err := httpGetBytes(base + "/v1/toolchains/" + url.PathEscape(version))
	if err != nil {
		return doctorCheck{Name: "Toolchain availability", Status: "warn",
			Message: fmt.Sprintf("%s not available at %s: %v", version, base, err)}
	}
	sigBytes, err := httpGetBytes(base + "/v1/toolchains/" + url.PathEscape(version) + "/manifest.sig")
	if err != nil {
		return doctorCheck{Name: "Toolchain availability", Status: "warn",
			Message: fmt.Sprintf("%s manifest present but signature missing: %v", version, err)}
	}
	if err := signing.VerifyToolchainManifestSignature(manifestBytes, strings.TrimSpace(string(sigBytes)), pinnedToolchainPublicKeyHex()); err != nil {
		return doctorCheck{Name: "Toolchain availability", Status: "error",
			Message: fmt.Sprintf("%s signature does not verify against the pinned key: %v", version, err)}
	}
	m, err := parseCLIManifest(manifestBytes)
	if err != nil {
		return doctorCheck{Name: "Toolchain availability", Status: "error", Message: err.Error()}
	}
	if err := checkToolchainIdentity(m); err != nil {
		return doctorCheck{Name: "Toolchain availability", Status: "warn",
			Message: fmt.Sprintf("%s available but incompatible: %v", version, err)}
	}
	return doctorCheck{Name: "Toolchain availability", Status: "ok",
		Message: fmt.Sprintf("%s available + signature-valid + compatible (%s/%s tier=%s)", version, m.Platform, m.Arch, m.Tier)}
}
