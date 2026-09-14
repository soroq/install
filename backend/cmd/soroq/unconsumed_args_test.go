package main

import (
	"strings"
	"testing"
)

// A WORD A COMMAND NEVER READS IS REFUSED, WHATEVER ORDER IT ARRIVES IN.
//
// This is the control the previous version of the rollback guard did not have. That guard inspected
// args[0], so its test called runRollback([]string{word}) -- word first -- and could not see the case
// that mattered: `soroq rollback --api URL android` put the platform after a flag, where args[0] is
// `--api`, and the word was discarded. On a project whose lockfile held only ios, a correctly spelled
// `android` rolled back the iOS fleet.
//
// So every case here appears in BOTH orders. An order-dependent guard fails the second half.
func TestUnconsumedArgumentsAreRefusedInEitherOrder(t *testing.T) {
	project := t.TempDir()

	for _, tc := range []struct {
		name    string
		run     func(args []string) error
		wordish []string
		flags   []string
	}{
		// NOT "android" HERE. `soroq rollback android --api URL` is the VALID form -- the platform is
		// routed by the first argument -- so it must not be refused, and asserting otherwise would be
		// testing the wrong thing. The order-sensitive case has its own test below.
		{"rollback", runRollback, []string{"androd", "bogusx"},
			[]string{"--api", "http://127.0.0.1:1"}},
		{"status", runStatus, []string{"ios", "bogusx"},
			[]string{"--project-dir", project}},
		{"doctor", runDoctor, []string{"ios", "bogusx"},
			[]string{"--project-dir", project}},
	} {
		for _, word := range tc.wordish {
			// WORD FIRST, then WORD AFTER A FLAG. The second is the one that shipped.
			orders := map[string][]string{
				"word first":       append([]string{word}, tc.flags...),
				"word after flags": append(append([]string{}, tc.flags...), word),
			}
			for order, args := range orders {
				t.Run(tc.name+"/"+word+"/"+order, func(t *testing.T) {
					err := tc.run(args)
					if err == nil {
						t.Fatalf("`soroq %s %s` was accepted; a word the command never reads must be "+
							"refused, not silently discarded", tc.name, strings.Join(args, " "))
					}
					if !strings.Contains(err.Error(), word) {
						t.Errorf("the refusal does not name the word %q: %v", word, err)
					}
				})
			}
		}
	}
}

// THE POSITIVE HALF: the forms that are supposed to work must not be refused, or a command that
// rejected everything would satisfy every assertion above.
func TestTheAcceptedFormsAreNotRefused(t *testing.T) {
	project := t.TempDir()
	for name, run := range map[string]func([]string) error{
		"status": runStatus,
		"doctor": runDoctor,
	} {
		err := run([]string{"--project-dir", project})
		if err != nil && strings.Contains(err.Error(), "does not take the argument") {
			t.Errorf("`soroq %s --project-dir <dir>` was refused as taking an argument: %v", name, err)
		}
	}
	// And the platform-first rollback form still routes rather than being refused.
	if err := runRollback([]string{"android", "--project-dir", project}); err != nil &&
		strings.Contains(err.Error(), "does not take the argument") {
		t.Errorf("`soroq rollback android` was refused as taking an argument: %v", err)
	}
}

// A CORRECTLY SPELLED PLATFORM IN THE WRONG POSITION IS STILL REFUSED, AND THAT IS THE POINT.
//
// This is the exact case the previous guard missed. `soroq rollback android` routes on args[0] and
// works. `soroq rollback --api URL android` puts the platform after a flag, where nothing reads it --
// and the old guard, which inspected args[0], saw `--api` and let it through. On a project whose
// lockfile held only ios, that rolled back the iOS fleet under a command that said android.
//
// Silently doing the right thing for the wrong platform is worse than refusing, so it refuses.
func TestACorrectPlatformAfterAFlagIsRefusedRatherThanIgnored(t *testing.T) {
	for _, platform := range []string{"android", "ios"} {
		t.Run(platform, func(t *testing.T) {
			// `--api` is a flag `rollback` really has; the point is that the platform follows it.
			err := runRollback([]string{"--api", "http://127.0.0.1:1", platform})
			if err == nil {
				t.Fatalf("`soroq rollback --api URL %s` was accepted. The platform sits "+
					"after a flag, so nothing reads it, and the command acts on whatever the "+
					"lockfile holds instead", platform)
			}
			if !strings.Contains(err.Error(), platform) {
				t.Errorf("the refusal does not name %q: %v", platform, err)
			}
			// AND IT SAYS WHERE THE WORD BELONGS. A refusal that does not say what to type instead
			// just moves the guessing.
			if !strings.Contains(err.Error(), "FIRST argument") {
				t.Errorf("the refusal does not say the platform goes first: %v", err)
			}
		})
	}
}
