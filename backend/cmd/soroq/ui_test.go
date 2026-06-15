package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintRootUsageIncludesProfessionalWorkflow(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer

	printRootUsage(&out)
	got := out.String()

	for _, want := range []string{
		"Soroq CLI",
		"Workflow",
		"soroq login",
		"soroq release android --artifact",
		"SOROQ_COLOR=auto|always|never",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("root usage missing %q:\n%s", want, got)
		}
	}
}

func TestPrintUnknownCommandSuggestsClosestCommand(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer

	printUnknownCommand(&out, "relese")
	got := out.String()

	for _, want := range []string{
		"[ERROR] unknown command \"relese\"",
		"did you mean `release`?",
		"run `soroq --help`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("unknown command output missing %q:\n%s", want, got)
		}
	}
}

func TestSuggestionsForErrorAddsActionableFixes(t *testing.T) {
	got := strings.Join(suggestionsForError("pubspec.yaml not found and adb device missing"), "\n")

	for _, want := range []string{
		"Run the command from a Flutter project root",
		"adb devices",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("suggestions missing %q:\n%s", want, got)
		}
	}
}

func TestClosestRootCommandRejectsWeakMatches(t *testing.T) {
	if got, ok := closestRootCommand("banana"); ok {
		t.Fatalf("closestRootCommand returned weak match %q", got)
	}
}

func TestPrintUnknownSubcommandSuggestsClosestChoice(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer

	printUnknownSubcommand(&out, "patch", "androd", []string{"android", "config", "ios"})
	got := out.String()

	for _, want := range []string{
		"[ERROR] unknown subcommand `patch androd`.",
		"did you mean `patch android`?",
		"run `soroq patch --help`.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("unknown subcommand output missing %q:\n%s", want, got)
		}
	}
}
