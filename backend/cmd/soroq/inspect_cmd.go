package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	androidrelease "soroq/backend/internal/androidrelease"
)

type inspectAndroidSummary struct {
	Artifact string                   `json:"artifact"`
	ABIs     []string                 `json:"abis"`
	Snapshot *androidrelease.Snapshot `json:"snapshot"`
}

func runInspect(args []string) error {
	if len(args) == 0 {
		inspectUsage()
		return errAlreadyPrinted
	}

	switch args[0] {
	case "android":
		return runInspectAndroid(args[1:])
	case "-h", "--help", "help":
		inspectUsage()
		return nil
	default:
		inspectUsage()
		return errAlreadyPrinted
	}
}

func inspectUsage() {
	fmt.Fprintln(os.Stdout, `usage: soroq inspect <target> [flags]

targets:
  android  inspect bundled Soroq metadata in an Android APK/AAB`)
}

func runInspectAndroid(args []string) error {
	fs := flag.NewFlagSet("inspect android", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	artifactPath := fs.String("artifact", "", "path to Android APK or AAB")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq inspect android --artifact build/app/outputs/bundle/release/app-release.aab [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(*artifactPath) == "" {
		return errors.New("--artifact is required")
	}

	snapshot, err := inspectAndroidArtifact(*artifactPath)
	if err != nil {
		return err
	}
	abis := androidrelease.DeriveABIs(snapshot)
	summary := inspectAndroidSummary{
		Artifact: snapshot.Artifact.Path,
		ABIs:     abis,
		Snapshot: snapshot,
	}
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	fmt.Fprintf(os.Stdout, "Android artifact: %s\n", summary.Artifact)
	fmt.Fprintf(os.Stdout, "artifact_type: %s\n", snapshot.Artifact.Type)
	fmt.Fprintf(os.Stdout, "app_name: %s\n", snapshot.Metadata.App.Name)
	if snapshot.Metadata.App.Version != nil {
		fmt.Fprintf(os.Stdout, "version: %s\n", *snapshot.Metadata.App.Version)
	}
	fmt.Fprintf(os.Stdout, "app_id: %s\n", snapshot.Metadata.Soroq.AppID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", snapshot.Metadata.Soroq.RuntimeID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", snapshot.Metadata.Soroq.Channel)
	fmt.Fprintf(os.Stdout, "abis: %s\n", strings.Join(abis, ", "))
	fmt.Fprintf(os.Stdout, "bundled_metadata: %s\n", snapshot.Artifact.BundledMetadataZipPath)
	return nil
}
