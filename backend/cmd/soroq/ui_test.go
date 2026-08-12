package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingCLICommand struct {
	done chan error
}

func (c *blockingCLICommand) Start() error { return nil }
func (c *blockingCLICommand) Wait() error  { return <-c.done }

func TestValidateCLIOutputFlagsProtectsMachineOutput(t *testing.T) {
	for _, test := range []struct {
		name    string
		verbose bool
		quiet   bool
		json    bool
		wantErr bool
	}{
		{name: "default"},
		{name: "verbose", verbose: true},
		{name: "quiet", quiet: true},
		{name: "json", json: true},
		{name: "quiet json", quiet: true, json: true},
		{name: "verbose quiet", verbose: true, quiet: true, wantErr: true},
		{name: "verbose json", verbose: true, json: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateCLIOutputFlags(test.verbose, test.quiet, test.json)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateCLIOutputFlags() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestBuildCommandsRejectOutputModesBeforeDoingWork(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func([]string) error
		args []string
		want string
	}{
		{name: "release android json verbose", run: runReleaseAndroid, args: []string{"--json", "--verbose"}, want: "--verbose cannot be used with --json"},
		{name: "release ios quiet verbose", run: runReleaseIOS, args: []string{"--quiet", "--verbose"}, want: "--verbose and --quiet cannot be used together"},
		{name: "patch android json verbose", run: runPatchAndroid, args: []string{"--json", "--verbose"}, want: "--verbose cannot be used with --json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.run(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("command error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestJSONModeSuppressesVerboseEnvironment(t *testing.T) {
	t.Setenv("SOROQ_VERBOSE", "1")
	defer resetCLIOutput()
	configureCLIOutput(false, false, true)
	if settings := currentCLIOutputSettings(); settings.Verbose {
		t.Fatalf("JSON mode inherited verbose environment: %+v", settings)
	}
}

func TestRedactCLITextRemovesCommonSecretShapes(t *testing.T) {
	input := `Authorization: Bearer abc123 SOROQ_OPERATOR_TOKEN=token-value --seed-base64 seed-value ` +
		`https://api.example.test/path?signature=signed-value&ok=1 ` +
		`{"code_verifier":"verifier-value"}`
	redacted := redactCLIText(input)
	for _, secret := range []string{"abc123", "token-value", "seed-value", "signed-value", "verifier-value"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redacted output leaked %q: %s", secret, redacted)
		}
	}
	if count := strings.Count(redacted, "[REDACTED]"); count != 5 {
		t.Fatalf("redaction count = %d, want 5: %s", count, redacted)
	}
}

func TestBuildPhaseForLineRecognizesDisplayOnlyMilestones(t *testing.T) {
	cases := map[string]string{
		"Resolving dependencies...":                      "Resolving Dart dependencies",
		"Downloading packages...":                        "Downloading Dart packages",
		"Running Gradle task 'bundleRelease'...":         "Compiling Android release with Gradle",
		"Running Xcode build...":                         "Compiling iOS release with Xcode",
		"Built build/app/outputs/app-release.aab (73MB)": "Android artifact built",
		"ordinary compiler output":                       "",
	}
	for input, want := range cases {
		if got := buildPhaseForLine(input); got != want {
			t.Errorf("buildPhaseForLine(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCLIProgressEmitsStartHeartbeatStepAndDone(t *testing.T) {
	oldWriter := cliProgressWriter
	oldInterval := cliHeartbeatInterval
	oldNow := cliNow
	defer func() {
		cliProgressWriter = oldWriter
		cliHeartbeatInterval = oldInterval
		cliNow = oldNow
		resetCLIOutput()
	}()

	var output bytes.Buffer
	cliProgressWriter = &output
	cliHeartbeatInterval = 5 * time.Millisecond
	started := time.Unix(0, 0)
	cliNow = func() time.Time { return started.Add(2 * time.Second) }
	configureCLIOutput(false, false, false)

	progress := startCLIProgress("Building Android app bundle")
	time.Sleep(12 * time.Millisecond)
	progress.Step("Compiling Android release with Gradle")
	progress.Finish(true, "log: /tmp/build.log")

	text := output.String()
	for _, fragment := range []string{"START", "RUNNING", "STEP", "DONE", "Compiling Android", "log: /tmp/build.log"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("progress output missing %q: %s", fragment, text)
		}
	}
}

func TestCLIProgressIsSilentForQuietAndJSONModes(t *testing.T) {
	oldWriter := cliProgressWriter
	defer func() {
		cliProgressWriter = oldWriter
		resetCLIOutput()
	}()

	for _, settings := range []cliOutputSettings{{Quiet: true}, {JSON: true}} {
		var output bytes.Buffer
		cliProgressWriter = &output
		configureCLIOutput(settings.Verbose, settings.Quiet, settings.JSON)
		progress := startCLIProgress("Build")
		progress.Step("Step")
		progress.Finish(true, "done")
		if output.Len() != 0 {
			t.Fatalf("settings %+v emitted progress: %q", settings, output.String())
		}
	}
}

func TestRedactingLineWriterHandlesSecretsSplitAcrossWrites(t *testing.T) {
	var log bytes.Buffer
	progress := &cliProgress{enabled: false}
	sink := &buildOutputSink{log: &log, progress: progress}
	writer := &redactingLineWriter{sink: sink}
	_, _ = writer.Write([]byte("SOROQ_OPERATOR_"))
	_, _ = writer.Write([]byte("TOKEN=do-not-leak\nBuilt app-release.aab"))
	writer.Flush()

	if strings.Contains(log.String(), "do-not-leak") || strings.Contains(string(sink.output()), "do-not-leak") {
		t.Fatalf("split write leaked secret: log=%q output=%q", log.String(), sink.output())
	}
}

func TestWaitForCLICommandEscalatesInterruptToKill(t *testing.T) {
	oldGrace := cliInterruptGracePeriod
	cliInterruptGracePeriod = 5 * time.Millisecond
	defer func() { cliInterruptGracePeriod = oldGrace }()

	command := &blockingCLICommand{done: make(chan error, 1)}
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	var mu sync.Mutex
	interrupts := 0
	kills := 0
	wantErr := errors.New("killed")

	err := waitForCLICommandSignal(command, signals, func() {
		mu.Lock()
		interrupts++
		mu.Unlock()
	}, func() {
		mu.Lock()
		kills++
		mu.Unlock()
		command.done <- wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("waitForCLICommandSignal() error = %v, want %v", err, wantErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if interrupts != 1 || kills != 1 {
		t.Fatalf("interrupts = %d, kills = %d, want 1 each", interrupts, kills)
	}
}
