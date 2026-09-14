package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestUsageListsEveryDispatchedCommand keeps `soroq --help` honest about what the CLI can do.
//
// WHY THIS EXISTS. `setup` dispatched and ran for an entire release while being absent from the help
// text, so it was implemented and undiscoverable at the same time. For a product whose stated goal is
// that a new developer needs no private knowledge, an unlisted command is the same as a missing one.
//
// The test reads the dispatch switch out of main.go rather than a hand-kept list, so adding a command
// without documenting it fails here instead of shipping.
func TestUsageListsEveryDispatchedCommand(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	// Only the top-level switch, which ends at the help case.
	start := strings.Index(body, "switch os.Args[1] {")
	if start < 0 {
		t.Fatal("top-level dispatch switch not found; this test is stale")
	}
	end := strings.Index(body[start:], `case "-h", "--help", "help":`)
	if end < 0 {
		t.Fatal("end of dispatch switch not found; this test is stale")
	}
	dispatch := body[start : start+end]

	// MULTI-VALUE CASES COUNT. `case "version", "-v", "--version":` dispatches a real command, and a
	// parser that only matched single-value cases could not see it -- so an implemented command in that
	// form was invisible to the very check that exists to find undiscoverable commands. The alias forms
	// (-v, --version) are skipped; the word form is a command like any other.
	caseRe := regexp.MustCompile(`(?m)^\tcase ((?:"[^"]+",\s*)*"[^"]+"):`)
	nameRe := regexp.MustCompile(`"([^"]+)"`)
	var commands []string
	for _, m := range caseRe.FindAllStringSubmatch(dispatch, -1) {
		for _, name := range nameRe.FindAllStringSubmatch(m[1], -1) {
			word := name[1]
			if strings.HasPrefix(word, "-") {
				continue // an alias spelling, not a command a reader types by name
			}
			if !regexp.MustCompile(`^[a-z][a-z-]*$`).MatchString(word) {
				continue
			}
			commands = append(commands, word)
		}
	}
	if len(commands) < 10 {
		t.Fatalf("found only %d dispatched commands (%v); the parser is broken, not the help", len(commands), commands)
	}

	help := captureUsage(t)
	for _, c := range commands {
		if !regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(c) + `\s`).MatchString(help) {
			t.Errorf("command %q dispatches but does not appear in `soroq --help`; it is implemented and undiscoverable", c)
		}
	}
}

// TestUsageAdvertisesNoCommandThatDoesNotExist is the other direction: help promising what the CLI
// cannot do is worse than help omitting what it can.
func TestUsageAdvertisesNoCommandThatDoesNotExist(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)
	help := captureUsage(t)

	lineRe := regexp.MustCompile(`(?m)^  ([a-z][a-z-]*)\s{2,}`)
	for _, m := range lineRe.FindAllStringSubmatch(help, -1) {
		// Matches BOTH `case "x":` and `case "x", "-x":`, for the same reason the other direction
		// does: a command dispatched in a multi-value case is dispatched.
		dispatched := regexp.MustCompile(`case (?:"[^"]+", )*"` + regexp.QuoteMeta(m[1]) + `"[,:]`)
		if !dispatched.MatchString(body) {
			t.Errorf("`soroq --help` advertises %q but nothing dispatches it", m[1])
		}
	}
}

// TestUsageDoesNotAskUsersForRawIdentifiers guards the product promise that the ordinary workflow
// never makes a developer handle internal identifiers or reach for soroqctl.
func TestUsageDoesNotAskUsersForRawIdentifiers(t *testing.T) {
	help := captureUsage(t)
	for _, banned := range []string{"patch id", "patch-id", "soroqctl", "runtime id", "runtime-id"} {
		if strings.Contains(strings.ToLower(help), banned) {
			t.Errorf("top-level help mentions %q; the ordinary workflow must not require internal identifiers", banned)
		}
	}
}

func captureUsage(t *testing.T) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	usage()
	w.Close()
	os.Stderr = old
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
