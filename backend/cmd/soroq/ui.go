package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"time"
)

type cliOutputSettings struct {
	Verbose bool
	Quiet   bool
	JSON    bool
}

var (
	cliOutputSettingsMu     sync.RWMutex
	cliOutput               cliOutputSettings
	cliProgressWriter       io.Writer = os.Stderr
	cliHeartbeatInterval              = 10 * time.Second
	cliInterruptGracePeriod           = 3 * time.Second
	cliNow                            = time.Now
)

func configureCLIOutput(verbose, quiet, jsonOutput bool) {
	cliOutputSettingsMu.Lock()
	defer cliOutputSettingsMu.Unlock()
	cliOutput = cliOutputSettings{Verbose: verbose, Quiet: quiet, JSON: jsonOutput}
	cliVerboseRequested = verbose
}

func resetCLIOutput() {
	configureCLIOutput(false, false, false)
}

func currentCLIOutputSettings() cliOutputSettings {
	cliOutputSettingsMu.RLock()
	settings := cliOutput
	cliOutputSettingsMu.RUnlock()
	if !settings.Quiet && !settings.JSON && soroqVerboseBuildOutput() {
		settings.Verbose = true
	}
	return settings
}

func validateCLIOutputFlags(verbose, quiet, jsonOutput bool) error {
	if verbose && quiet {
		return errors.New("--verbose and --quiet cannot be used together")
	}
	if verbose && jsonOutput {
		return errors.New("--verbose cannot be used with --json because raw build output would corrupt machine-readable output")
	}
	return nil
}

type cliProgress struct {
	mu      sync.Mutex
	writer  io.Writer
	label   string
	current string
	started time.Time
	stop    chan struct{}
	done    chan struct{}
	enabled bool
	color   bool
}

func startCLIProgress(label string) *cliProgress {
	settings := currentCLIOutputSettings()
	p := &cliProgress{
		writer:  cliProgressWriter,
		label:   strings.TrimSpace(label),
		current: strings.TrimSpace(label),
		started: cliNow(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		enabled: !settings.Quiet && !settings.JSON,
		color:   cliColorEnabled(cliProgressWriter),
	}
	if !p.enabled {
		close(p.done)
		return p
	}
	p.write("START", p.current, "")
	if settings.Verbose || cliHeartbeatInterval <= 0 {
		close(p.done)
		return p
	}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(cliHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.mu.Lock()
				current := p.current
				p.mu.Unlock()
				p.write("RUNNING", current, "")
			case <-p.stop:
				return
			}
		}
	}()
	return p
}

func (p *cliProgress) Step(label string) {
	if !p.enabled {
		return
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return
	}
	p.mu.Lock()
	if label == p.current {
		p.mu.Unlock()
		return
	}
	p.current = label
	p.mu.Unlock()
	p.write("STEP", label, "")
}

func (p *cliProgress) Finish(success bool, detail string) {
	if !p.enabled {
		return
	}
	select {
	case <-p.done:
	default:
		close(p.stop)
		<-p.done
	}
	status := "DONE"
	if !success {
		status = "FAILED"
	}
	p.write(status, p.label, strings.TrimSpace(detail))
}

func (p *cliProgress) write(status, message, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := cliNow().Sub(p.started).Round(time.Second)
	if elapsed < 0 {
		elapsed = 0
	}
	statusText := status
	if p.color {
		switch status {
		case "DONE":
			statusText = "\x1b[32m" + status + "\x1b[0m"
		case "FAILED":
			statusText = "\x1b[31m" + status + "\x1b[0m"
		case "RUNNING":
			statusText = "\x1b[36m" + status + "\x1b[0m"
		default:
			statusText = "\x1b[34m" + status + "\x1b[0m"
		}
	}
	if detail != "" {
		fmt.Fprintf(p.writer, "[%s] %-7s %s — %s\n", formatCLIElapsed(elapsed), statusText, message, detail)
		return
	}
	fmt.Fprintf(p.writer, "[%s] %-7s %s\n", formatCLIElapsed(elapsed), statusText, message)
}

func formatCLIElapsed(elapsed time.Duration) string {
	totalSeconds := int(elapsed.Seconds())
	if totalSeconds < 0 {
		totalSeconds = 0
	}
	return fmt.Sprintf("%02d:%02d", totalSeconds/60, totalSeconds%60)
}

func cliColorEnabled(writer io.Writer) bool {
	if strings.TrimSpace(os.Getenv("NO_COLOR")) != "" || strings.TrimSpace(os.Getenv("CI")) != "" {
		return false
	}
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

var sensitiveCLIValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*:\s*(?:bearer|basic)\s+)[^\s]+`),
	regexp.MustCompile(`(?i)((?:SOROQ_[A-Z0-9_]*(?:TOKEN|SEED|SECRET|PASSWORD|ACCESS_KEY)[A-Z0-9_]*)|(?:ACCESS_KEY|PASSWORD|SECRET|TOKEN|SEED))=([^\s'";\\]+)`),
	regexp.MustCompile(`(?i)(--[a-z0-9-]*(?:token|seed|secret|password|access-key)[a-z0-9-]*)(?:=|\s+)([^\s'";\\]+)`),
	regexp.MustCompile(`(?i)([?&](?:access_token|token|signature|sig|seed|secret|password|code_verifier)=)[^&\s]+`),
	regexp.MustCompile(`(?i)("(?:access_token|token|signature|seed|secret|password|code_verifier)"\s*:\s*")[^"]*(")`),
}

func redactCLIText(value string) string {
	redacted := value
	for index, pattern := range sensitiveCLIValuePatterns {
		switch index {
		case 0:
			redacted = pattern.ReplaceAllString(redacted, `${1}[REDACTED]`)
		case 1:
			redacted = pattern.ReplaceAllString(redacted, `${1}=[REDACTED]`)
		case 2:
			redacted = pattern.ReplaceAllString(redacted, `${1} [REDACTED]`)
		case 3:
			redacted = pattern.ReplaceAllString(redacted, `${1}[REDACTED]`)
		default:
			redacted = pattern.ReplaceAllString(redacted, `${1}[REDACTED]${2}`)
		}
	}
	return redacted
}

type buildOutputSink struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	log      io.Writer
	logErr   error
	progress *cliProgress
	verbose  bool
}

func (s *buildOutputSink) writeLine(line string, stream io.Writer) {
	line = redactCLIText(strings.TrimRight(line, "\r\n"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if line != "" {
		s.buffer.WriteString(line)
	}
	s.buffer.WriteByte('\n')
	if s.log != nil && s.logErr == nil {
		if line != "" {
			_, s.logErr = io.WriteString(s.log, line)
		}
		if s.logErr == nil {
			_, s.logErr = io.WriteString(s.log, "\n")
		}
	}
	if s.verbose && stream != nil {
		if line != "" {
			_, _ = io.WriteString(stream, line)
		}
		_, _ = io.WriteString(stream, "\n")
	}
	if phase := buildPhaseForLine(line); phase != "" {
		s.progress.Step(phase)
	}
}

func (s *buildOutputSink) output() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buffer.Bytes()...)
}

func (s *buildOutputSink) logError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logErr
}

type redactingLineWriter struct {
	mu      sync.Mutex
	partial []byte
	sink    *buildOutputSink
	stream  io.Writer
}

func (w *redactingLineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		index := bytes.IndexAny(w.partial, "\r\n")
		if index < 0 {
			break
		}
		w.sink.writeLine(string(w.partial[:index]), w.stream)
		w.partial = w.partial[index+1:]
	}
	return len(p), nil
}

func (w *redactingLineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) == 0 {
		return
	}
	w.sink.writeLine(string(w.partial), w.stream)
	w.partial = nil
}

func buildPhaseForLine(line string) string {
	lower := strings.ToLower(strings.TrimSpace(line))
	switch {
	case strings.Contains(lower, "resolving dependencies"):
		return "Resolving Dart dependencies"
	case strings.Contains(lower, "downloading packages"):
		return "Downloading Dart packages"
	case strings.Contains(lower, "running gradle task"):
		return "Compiling Android release with Gradle"
	case strings.Contains(lower, "running xcode build"):
		return "Compiling iOS release with Xcode"
	case strings.Contains(lower, "compiling, linking and signing"):
		return "Linking and signing iOS app"
	case strings.Contains(lower, "built ") && (strings.Contains(lower, ".apk") || strings.Contains(lower, ".aab")):
		return "Android artifact built"
	case strings.Contains(lower, "built ") && (strings.Contains(lower, ".app") || strings.Contains(lower, ".ipa")):
		return "iOS artifact built"
	default:
		return ""
	}
}

func waitForCLICommand(cmd interface {
	Start() error
	Wait() error
}, interrupt func(), kill func()) error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	return waitForCLICommandSignal(cmd, signals, interrupt, kill)
}

func waitForCLICommandSignal(cmd interface {
	Start() error
	Wait() error
}, signals <-chan os.Signal, interrupt func(), kill func()) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-signals:
		interrupt()
		timer := time.NewTimer(cliInterruptGracePeriod)
		defer timer.Stop()
		select {
		case err := <-done:
			return err
		case <-timer.C:
			kill()
			return <-done
		}
	}
}
