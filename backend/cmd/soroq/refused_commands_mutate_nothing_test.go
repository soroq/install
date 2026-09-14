package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// A REFUSED COMMAND MUTATES NOTHING.
//
// WHAT THIS IS FOR, and it is not hypothetical. `soroq update unexpected --check` performed a REAL
// self-update: Go's flag package stops parsing at the first non-flag argument, so `--check` -- whose
// own help says it "makes NO changes" -- never reached the parser, and the command downloaded and
// replaced the installed executable. `soroq cache clean --delete tc-1` ignored `tc-1` and deleted
// every cached version instead of the one named.
//
// Checking the exit code is not enough for either. A command can exit non-zero AFTER downloading, or
// after deleting three of four directories. So this file takes a FINGERPRINT of the whole tree --
// every path, its size, its content hash and its modification time -- runs the refused command, and
// requires the fingerprint to be identical afterwards.

// treeFingerprint describes every file under root: relative path, size, content hash and mtime.
type treeFingerprint map[string]string

func fingerprint(t *testing.T, root string) treeFingerprint {
	t.Helper()
	out := treeFingerprint{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if info.IsDir() {
			out[rel+"/"] = fmt.Sprintf("dir mode=%s", info.Mode().Perm())
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(data)
		// mtime included deliberately: a rewrite with identical bytes is still a mutation.
		out[rel] = fmt.Sprintf("size=%d sha=%s mtime=%d mode=%s",
			info.Size(), hex.EncodeToString(sum[:8]), info.ModTime().UnixNano(), info.Mode().Perm())
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprint %s: %v", root, err)
	}
	return out
}

func diffFingerprints(before, after treeFingerprint) []string {
	var changes []string
	for path, was := range before {
		now, present := after[path]
		if !present {
			changes = append(changes, "REMOVED  "+path)
		} else if now != was {
			changes = append(changes, "CHANGED  "+path+"\n    was: "+was+"\n    now: "+now)
		}
	}
	for path := range after {
		if _, present := before[path]; !present {
			changes = append(changes, "CREATED  "+path)
		}
	}
	sort.Strings(changes)
	return changes
}

// seedCacheHome builds a HOME with four cached toolchain versions and an unrelated sentinel file.
func seedCacheHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, version := range []string{"tc-1", "tc-2", "tc-3", "tc-4"} {
		dir := filepath.Join(home, ".soroq", "toolchains", version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"),
			[]byte(`{"version":"`+version+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A SENTINEL THAT NO COMMAND HAS ANY BUSINESS TOUCHING.
	if err := os.WriteFile(filepath.Join(home, ".soroq", "sentinel.txt"),
		[]byte("this file must survive every refused command\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate everything, so a rewrite with identical content still shows as a changed mtime.
	old := time.Now().Add(-2 * time.Hour)
	_ = filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chtimes(path, old, old)
		}
		return nil
	})
	return home
}

func TestARefusedCommandMutatesNothing(t *testing.T) {
	cli := jsonCLI(t)

	for _, tc := range []struct {
		name string
		args []string
	}{
		// THE TWO DEMONSTRATED DEFECTS, as their own rows.
		{"update with an unexpected word", []string{"update", "unexpected", "--check"}},
		{"cache clean with a version name", []string{"cache", "clean", "--delete", "tc-1"}},
		// And the same shape on other commands, so the class is held rather than the two examples.
		{"cache list with a word", []string{"cache", "list", "tc-1"}},
		{"toolchain list with a word", []string{"toolchain", "list", "tc-1"}},
		{"uninstall with a word", []string{"uninstall", "everything"}},
		{"support-bundle with a word", []string{"support-bundle", "out.json"}},
		{"logout with a word", []string{"logout", "everywhere"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := seedCacheHome(t)
			project := t.TempDir()
			before := fingerprint(t, home)

			cmd := exec.Command(cli, tc.args...)
			cmd.Dir = project
			cmd.Env = []string{
				"HOME=" + home,
				"PATH=" + os.Getenv("PATH"),
				"NO_COLOR=1",
				"SOROQ_API=http://127.0.0.1:1",
			}
			out, err := cmd.CombinedOutput()

			// IT MUST REFUSE. A command that accepts the word and ignores it is the defect.
			if err == nil {
				t.Fatalf("`soroq %s` exited 0. An argument the command never reads must be refused, "+
					"not discarded:\n%s", strings.Join(tc.args, " "), out)
			}
			// AND THE REFUSAL MUST NAME THE WORD, or the reader cannot tell what was wrong.
			if !strings.Contains(string(out), "does not take the argument") {
				t.Errorf("`soroq %s` failed for some other reason:\n%s",
					strings.Join(tc.args, " "), out)
			}

			// NOTHING CHANGED. This is the assertion the exit code cannot make.
			if changes := diffFingerprints(before, fingerprint(t, home)); len(changes) > 0 {
				t.Errorf("`soroq %s` was refused and STILL mutated %d path(s):\n%s",
					strings.Join(tc.args, " "), len(changes), strings.Join(changes, "\n"))
			}
		})
	}
}

// THE POSITIVE HALF: the cache really does hold four versions and a sentinel, and a command that is
// SUPPOSED to act still acts.
//
// Without this, a CLI that refused every invocation -- or a fixture that created nothing -- would
// satisfy every assertion above.
func TestTheCacheFixtureIsRealAndTheDocumentedFormStillWorks(t *testing.T) {
	cli := jsonCLI(t)
	home := seedCacheHome(t)

	for _, version := range []string{"tc-1", "tc-2", "tc-3", "tc-4"} {
		if _, err := os.Stat(filepath.Join(home, ".soroq", "toolchains", version)); err != nil {
			t.Fatalf("the fixture did not create %s: %v", version, err)
		}
	}

	// The DOCUMENTED form -- a dry run, no positional -- must be accepted and must report the versions.
	cmd := exec.Command(cli, "cache", "clean")
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`soroq cache clean` (the documented form) was refused: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "tc-1") {
		t.Errorf("the documented dry run does not mention the cached versions:\n%s", out)
	}
	// A dry run must not delete either -- that is its whole contract.
	for _, version := range []string{"tc-1", "tc-2", "tc-3", "tc-4"} {
		if _, err := os.Stat(filepath.Join(home, ".soroq", "toolchains", version)); err != nil {
			t.Errorf("the DRY RUN removed %s: %v", version, err)
		}
	}
}
