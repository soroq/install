package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BARE `soroq rollback` RESOLVES THE PROJECT'S SOLE RELEASED PLATFORM.
//
// WHY THIS FILE EXISTS. An audit deleted the resolution branch from `runRollback` and the ENTIRE Go
// suite stayed green -- twice. The only thing that caught it was the documentation checker, and that
// defence holds only while the documentation happens to write the bare form. A product behaviour whose
// sole guard is a document is a product behaviour one edit away from being unguarded.
//
// The behaviour matters because it is the defect this project already accepted and fixed once:
// `soroq rollback` was documented as the beginner's rollback step while the bare form answered
// "--patch-id is required", handing a beginner a raw patch id to find for themselves.

func writeRollbackProject(t *testing.T, platforms map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "soroq.yaml"),
		[]byte("app_id: com.example.rollback\nchannel: stable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := "# soroq.lock\nplatforms:\n"
	for platform, release := range platforms {
		lock += "  " + platform + ":\n"
		if release != "" {
			lock += "    release_id: " + release + "\n"
		}
		lock += "    version: 1.0.0+1\n    recorded_at: 2026-01-01T00:00:00Z\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "soroq.lock"), []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBareRollbackResolvesTheSoleReleasedPlatform(t *testing.T) {
	dir := writeRollbackProject(t, map[string]string{"android": "rel-android-1"})

	platform, err := soleReleasedPlatform(dir)
	if err != nil {
		t.Fatalf("a project with exactly one released platform must resolve without asking: %v", err)
	}
	if platform != "android" {
		t.Errorf("resolved %q, want android", platform)
	}
}

func TestRollbackNamesBothPlatformsWhenItCannotChoose(t *testing.T) {
	dir := writeRollbackProject(t, map[string]string{
		"android": "rel-android-1",
		"ios":     "rel-ios-1",
	})

	_, err := soleReleasedPlatform(dir)
	if err == nil {
		t.Fatal("two released platforms must NOT resolve to one; guessing which to roll back is not " +
			"a safe default")
	}
	// BOTH names, and the command that disambiguates. An error that only says "ambiguous" leaves the
	// reader to discover the syntax themselves, which is the failure this whole area is about.
	for _, want := range []string{"android", "ios", "soroq rollback android"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestRollbackNamesTheCommandThatCreatesARelease(t *testing.T) {
	dir := writeRollbackProject(t, map[string]string{"android": ""})

	_, err := soleReleasedPlatform(dir)
	if err == nil {
		t.Fatal("a project with no registered release must not resolve a platform to roll back")
	}
	if !strings.Contains(err.Error(), "soroq release") {
		t.Errorf("the refusal does not name the command that creates a release: %v", err)
	}
}

func TestRollbackOutsideAProjectIsDistinguishedFromAnEmptyOne(t *testing.T) {
	// NOT the same condition, and the caller depends on the difference: outside a project the command
	// falls back to requiring --patch-id, while inside one it reports the project's actual state.
	// Collapsing the two would make `soroq rollback` in a Soroq project say "--patch-id is required",
	// which is exactly the defect that was fixed.
	_, err := soleReleasedPlatform(t.TempDir())
	if err != errNotASoroqProject {
		t.Errorf("a directory that is not a Soroq project must report errNotASoroqProject, got %v", err)
	}
}

// A WORD THAT IS NOT A PLATFORM IS REFUSED, NOT DISCARDED.
//
// `soroq release` and `soroq patch` both end their positional switch with a default arm that prints
// usage. `rollback` had no switch: anything that was not android/ios/ios-engine fell through to
// flag.Parse, which leaves a leading non-flag argument in fs.Args() and never reads it. So
// `soroq rollback androd` DISCARDED the word and rolled back whatever platform the lockfile held --
// on an iOS-only project, a mistyped `androd` rolled back iOS.
//
// An audit proved it at the boundary rather than by reading the code: `rollback`, `rollback bogusx`
// and `rollback android` produced byte-identical output. The assertion here is the same shape.
func TestRollbackRefusesAWordThatIsNotAPlatform(t *testing.T) {
	for _, word := range []string{"androd", "iOS", "bogusx", "andriod"} {
		t.Run(word, func(t *testing.T) {
			err := runRollback([]string{word})
			if err == nil {
				t.Fatalf("`soroq rollback %s` did not refuse. A mistyped platform that is silently "+
					"discarded acts on whichever platform the lockfile holds -- a typo rolling back "+
					"a different fleet, with an exit code of zero", word)
			}
			// The message must name the word AND the forms that work, or the reader is left to guess.
			for _, want := range []string{word, "android", "ios"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// THE POSITIVE HALF: the real platform words must still route, or the guard above would be satisfied
// by a command that refused everything.
func TestRollbackStillAcceptsItsRealPlatformWords(t *testing.T) {
	for _, word := range []string{"android", "ios"} {
		err := runRollback([]string{word, "--project-dir", t.TempDir()})
		// It will fail -- there is no project there -- but it must NOT fail with the refusal above.
		if err != nil && strings.Contains(err.Error(), "unknown platform") {
			t.Errorf("`soroq rollback %s` was refused as an unknown platform: %v", word, err)
		}
	}
}
