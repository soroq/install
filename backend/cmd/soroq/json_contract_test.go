package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A COMMAND THAT DECLARES --json MUST EMIT ITS DECLARED SHAPE, INCLUDING WHEN THERE IS NOTHING.
//
// WHY THIS FILE EXISTS. `soroq frontend list --json` and `soroq toolchain list --json` printed `null`
// on a machine with nothing installed, because a nil slice marshals to `null`. A consumer that
// iterates the result fails on exactly the machine a first run happens on, and no test noticed because
// nothing parsed the output -- the same gap that let `soroq support-bundle` ship a document that was
// not JSON at all while 654 lines of substring assertions stayed green.
//
// So the assertion here is the format contract itself: parse it, and require a LIST command to yield a
// list. An empty machine is the fixture on purpose; a warm cache would hide the defect entirely.

func jsonCLI(t *testing.T) string {
	t.Helper()
	built := filepath.Join(t.TempDir(), "soroq")
	build := exec.Command("go", "build", "-o", built, ".")
	if out, err := build.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "no space left on device") {
			t.Fatalf("cannot build the CLI under test: THE DISK IS FULL. Environmental, not a defect "+
				"in the code under test -- nothing was exercised.\n%s", out)
		}
		t.Fatalf("build the CLI under test: %v\n%s", err, out)
	}
	return built
}

func TestListCommandsEmitAJSONArrayFromAnEmptyMachine(t *testing.T) {
	cli := jsonCLI(t)

	for _, args := range [][]string{
		{"frontend", "list", "--json"},
		{"toolchain", "list", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := t.TempDir()
			cmd := exec.Command(cli, args...)
			cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("`soroq %s` failed: %v\n%s", strings.Join(args, " "), err, out)
			}

			// PARSE IT. A substring check cannot tell `[]` from `null` from a truncated document.
			var decoded any
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("`soroq %s` declares JSON and did not emit it (%v):\n%s",
					strings.Join(args, " "), err, out)
			}
			if decoded == nil {
				t.Errorf("`soroq %s` emitted `null` on a machine with nothing installed. A caller "+
					"that iterates the result fails here, and this is the machine a first run "+
					"happens on. An empty list is `[]`.", strings.Join(args, " "))
				return
			}
			if _, ok := decoded.([]any); !ok {
				t.Errorf("`soroq %s` is a LIST command and did not emit a list; got %T:\n%s",
					strings.Join(args, " "), decoded, out)
			}
		})
	}
}

// THE POSITIVE HALF. Without it, a command that emitted `[]` unconditionally -- having read nothing --
// would pass every assertion above, and the suite would be checking a constant.
func TestAListCommandReportsWhatIsActuallyInstalled(t *testing.T) {
	cli := jsonCLI(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".soroq", "frontends", "fe-under-test"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(cli, "frontend", "list", "--json")
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`soroq frontend list --json` failed: %v\n%s", err, out)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("not JSON (%v):\n%s", err, out)
	}
	if len(decoded) != 1 {
		t.Fatalf("expected the one installed frontend, got %d entries:\n%s", len(decoded), out)
	}
	if decoded[0]["version"] != "fe-under-test" {
		t.Errorf("the listed entry is not the installed one: %v", decoded[0])
	}
}

// A COMMAND WHOSE --help LISTS --json MUST EMIT JSON WHEN GIVEN IT.
//
// `soroq init --json --dry-run` printed its prose preview and ignored the flag, while its own help
// listed `[--json]`. That is worse than not offering the mode: a script written against the documented
// surface silently parses nothing and reports success.
//
// The check is derived from `--help` rather than from a hand-written list, so a command that GAINS a
// --json flag without implementing it fails here without anyone remembering to add a row.
func TestEveryCommandThatAdvertisesJSONEmitsIt(t *testing.T) {
	cli := jsonCLI(t)

	// The pairs that can be exercised without a network or a build. Each is a documented read-only
	// form; the point is the FLAG, not the command.
	exercised := 0
	for _, args := range [][]string{
		{"init", "--app-id", "com.example.jsoncheck", "--json", "--dry-run"},
		{"frontend", "list", "--json"},
		{"toolchain", "list", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := t.TempDir()
			project := t.TempDir()
			if err := os.WriteFile(filepath.Join(project, "pubspec.yaml"),
				[]byte("name: x\nenvironment:\n  sdk: '>=3.0.0 <4.0.0'\n"+
					"dependencies:\n  flutter:\n    sdk: flutter\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			// The flag must be one the command actually advertises, or this test would be asserting
			// a contract nobody offered.
			//
			// THE COMMAND PATH IS THE LEADING NON-FLAG WORDS, not a fixed number of arguments. A first
			// version dropped the last two, which turned `frontend list --json` into
			// `soroq frontend --help` -- a help text that lists subcommands and no --json, so both
			// cases SKIPPED. A silent skip is the vacuous shape wearing a different label.
			var path []string
			for _, arg := range args {
				if strings.HasPrefix(arg, "-") {
					break
				}
				path = append(path, arg)
			}
			help := exec.Command(cli, append(append([]string{}, path...), "--help")...)
			help.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}
			helpText, _ := help.CombinedOutput()
			if !strings.Contains(string(helpText), "--json") && !strings.Contains(string(helpText), "-json") {
				t.Skipf("this build's help does not advertise --json for %v", args)
			}

			cmd := exec.Command(cli, args...)
			cmd.Dir = project
			cmd.Env = []string{
				"HOME=" + home,
				"PATH=" + os.Getenv("PATH"),
				"NO_COLOR=1",
				"SOROQ_API=http://127.0.0.1:1",
			}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("`soroq %s` failed: %v", strings.Join(args, " "), err)
			}
			var decoded any
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("`soroq %s` advertises --json and did not emit JSON (%v):\n%s",
					strings.Join(args, " "), err, out)
			}
			exercised++
		})
	}

	// THE POSITIVE HALF. Every case skipping would leave "nothing failed" trivially true -- which is
	// exactly what the first version of this test did, for a reason that had nothing to do with the
	// contract it was checking.
	if exercised == 0 {
		t.Error("no command was actually exercised; every case skipped, so this test proved nothing " +
			"about the --json contract")
	}
}

// AN ALIAS MUST DESCRIBE ITSELF.
//
// `soroq patches` routes to the same function as `soroq patch`, so `soroq patches --help` printed the
// PUBLISHING command's usage: it never named `patches`, and it advertised publish targets while the
// root help describes it as "list your updates, inspect one, promote it, or change its track". A
// reader following --help was told about a different command.
func TestThePatchesAliasDescribesItself(t *testing.T) {
	cli := jsonCLI(t)
	home := t.TempDir()

	cmd := exec.Command(cli, "patches", "--help")
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`soroq patches --help` failed: %v\n%s", err, out)
	}
	help := string(out)

	if !strings.Contains(help, "soroq patches") {
		t.Errorf("`soroq patches --help` never names the command it describes:\n%s", help)
	}
	// The read-side subcommands it actually offers.
	for _, want := range []string{"list", "status", "promote", "set-track"} {
		if !strings.Contains(help, want) {
			t.Errorf("`soroq patches --help` does not mention its %q subcommand:\n%s", want, help)
		}
	}
	// AND IT MUST NOT ADVERTISE PUBLISHING, which is the other command's job and was the whole defect.
	if strings.Contains(help, "--platforms=android,ios") {
		t.Errorf("`soroq patches --help` is still printing `soroq patch`'s publishing usage:\n%s", help)
	}
}
