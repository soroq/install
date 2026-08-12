package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSoroqBuildCommandStreamsPhasesAndWritesPrivateRedactedLog(t *testing.T) {
	oldWriter := cliProgressWriter
	defer func() {
		cliProgressWriter = oldWriter
		resetCLIOutput()
	}()

	var progress bytes.Buffer
	cliProgressWriter = &progress
	configureCLIOutput(false, false, false)
	projectDir := t.TempDir()
	cmd := exec.Command("sh", "-c", "printf 'Resolving dependencies...\\nSOROQ_OPERATOR_TOKEN=do-not-leak\\nRunning Gradle task bundleRelease...\\nBuilt build/app-release.aab\\n'")

	if err := runSoroqBuildCommand(cmd, projectDir, "Building Android app bundle", "android"); err != nil {
		t.Fatalf("runSoroqBuildCommand() error = %v", err)
	}
	for _, fragment := range []string{"START", "Resolving Dart dependencies", "Compiling Android release with Gradle", "Android artifact built", "DONE"} {
		if !strings.Contains(progress.String(), fragment) {
			t.Fatalf("progress missing %q: %s", fragment, progress.String())
		}
	}
	logs, err := filepath.Glob(filepath.Join(projectDir, ".soroq", "logs", "*-android-build.log"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("build logs = %v, err = %v, want one", logs, err)
	}
	info, err := os.Stat(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("build log mode = %o, want 600", got)
	}
	contents, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "do-not-leak") {
		t.Fatalf("build log leaked secret: %s", contents)
	}
	if !strings.Contains(string(contents), "SOROQ_OPERATOR_TOKEN=[REDACTED]") {
		t.Fatalf("build log missing redaction marker: %s", contents)
	}
}

func TestRunSoroqBuildCommandFailureNamesDurationLogAndVerboseRecovery(t *testing.T) {
	oldWriter := cliProgressWriter
	defer func() {
		cliProgressWriter = oldWriter
		resetCLIOutput()
	}()

	var progress bytes.Buffer
	cliProgressWriter = &progress
	configureCLIOutput(false, false, false)
	projectDir := t.TempDir()
	cmd := exec.Command("sh", "-c", "printf 'compiler failed\\n' >&2; exit 7")

	err := runSoroqBuildCommand(cmd, projectDir, "Building Android app bundle", "android")
	if err == nil {
		t.Fatal("runSoroqBuildCommand() error = nil, want failure")
	}
	for _, fragment := range []string{"build command failed after", "full log:", "--verbose"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error missing %q: %v", fragment, err)
		}
	}
	if !strings.Contains(progress.String(), "FAILED") {
		t.Fatalf("progress missing FAILED: %s", progress.String())
	}
}
