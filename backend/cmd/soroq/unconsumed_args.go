package main

import (
	"fmt"
	"strings"
)

// refuseUnconsumedArguments reports the words a command parsed but never read.
//
// WHY THIS IS A SHARED HELPER AND NOT THREE LOCAL CHECKS.
//
// Go's `flag` package stops at the first non-flag argument and leaves the rest in fs.Args(). A command
// that never inspects fs.Args() therefore accepts any number of words and silently ignores them. Three
// commands did: `soroq status ios`, `soroq doctor ios` and `soroq rollback ... android` all behaved
// exactly as if the word were absent, and exited 0.
//
// THE ROLLBACK CASE IS THE ONE THAT MATTERS, and it is why this check lives after the parse rather
// than before it. An earlier fix inspected args[0] and refused an unknown platform there, which
// covered `soroq rollback androd`. It did NOT cover `soroq rollback --api URL androd`: the word sits
// after a flag, args[0] is `--api`, and the guard never sees it. Worse, `--api URL android` was
// discarded too -- so on a project whose lockfile holds only ios, a correctly spelled `android` rolled
// back the iOS fleet. Checking args[0] guards one argument ORDER. Checking fs.Args() guards the
// command.
//
// The message names the words and, when the command has a fixed set, what was allowed -- a refusal
// that does not say what to type instead just moves the guessing.
func refuseUnconsumedArguments(command string, leftover []string, accepted []string) error {
	if len(leftover) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(leftover))
	for _, word := range leftover {
		quoted = append(quoted, fmt.Sprintf("%q", word))
	}
	subject := "argument"
	if len(quoted) > 1 {
		subject = "arguments"
	}
	message := fmt.Sprintf("`soroq %s` does not take the %s %s",
		command, subject, strings.Join(quoted, ", "))
	if len(accepted) > 0 {
		message += fmt.Sprintf("; it accepts %s", strings.Join(accepted, ", "))
	}
	// SAY WHY A FLAG IS IN THIS LIST, because otherwise the message reads as nonsense: the reader can
	// see that `--check` IS a real flag of the command they typed.
	//
	// Go's flag package stops parsing at the first non-flag argument, so everything after an
	// unexpected word -- flags included -- is left unread. That is exactly how
	// `soroq update unexpected --check` performed a real self-update: `--check` never reached the
	// parser, so the command believed it had not been asked to check.
	for _, word := range leftover {
		if strings.HasPrefix(word, "-") {
			message += ". A flag listed here was NOT applied: argument parsing stops at the first " +
				"unexpected word, so every flag after one is ignored"
			break
		}
	}
	return fmt.Errorf("%s", message)
}
