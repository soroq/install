package main

import (
	"strings"
	"testing"
)

func TestWindowsBuildArtifactGuardFailsBeforeSetupNetwork(t *testing.T) {
	old := currentHostOS
	currentHostOS = "windows"
	t.Cleanup(func() { currentHostOS = old })

	err := runSetup([]string{"android", "--api", "http://127.0.0.1:1/must-not-be-called"})
	if err == nil {
		t.Fatal("expected Windows setup to fail closed")
	}
	for _, want := range []string{"Windows CLI beta", "no download was started", "Windows-host"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestWindowsBuildArtifactGuardCoversDirectInstallCommands(t *testing.T) {
	old := currentHostOS
	currentHostOS = "windows"
	t.Cleanup(func() { currentHostOS = old })

	for name, run := range map[string]func([]string) error{
		"frontend":  runFrontendInstall,
		"toolchain": runToolchainInstall,
	} {
		t.Run(name, func(t *testing.T) {
			err := run([]string{"candidate", "--api", "http://127.0.0.1:1/must-not-be-called"})
			if err == nil || !strings.Contains(err.Error(), "no download was started") {
				t.Fatalf("expected pre-download Windows refusal, got %v", err)
			}
		})
	}
}

func TestBuildArtifactGuardAllowsExistingHosts(t *testing.T) {
	old := currentHostOS
	currentHostOS = "darwin"
	t.Cleanup(func() { currentHostOS = old })
	if err := requireBuildArtifactsForHost(); err != nil {
		t.Fatalf("darwin unexpectedly refused: %v", err)
	}
}
