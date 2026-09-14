package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// EXECUTING THE DOCUMENTED COMMANDS, FROM A CLEAN HOME.
//
// verify_documented_commands.py proves a documented command EXISTS. That is the cheap half. This is the
// other one: the command runs from a HOME that holds no credential and no cache, and it produces the
// state the documentation says it produces.
//
// WHY EXIT 0 IS NOT THE ASSERTION. A command that prints a warning and returns 0 without writing
// soroq.yaml satisfies "the quickstart works" and leaves the reader with nothing. Every case below
// names a FILE, a piece of OUTPUT or a REFUSAL it requires, so a command that succeeds vacuously fails
// here. TestACommandThatOnlyExitsZeroIsNotEnough is the control that proves that distinction is real.
//
// ISOLATION. Every case gets its own t.TempDir() for HOME and its own for the project. The real HOME is
// never assigned, read or removed; nothing here deletes a variable-expanded path.

// docCLI builds the CLI once per test binary and returns its path.
func docCLI(t *testing.T) string {
	t.Helper()
	built := filepath.Join(t.TempDir(), "soroq")
	build := exec.Command("go", "build", "-o", built, ".")
	if out, err := build.CombinedOutput(); err != nil {
		// SAY WHEN THE MACHINE IS THE PROBLEM, NOT THE CODE.
		//
		// An audit reported this test as an intermittent failure and said it "would have been read as
		// a catch". It was neither flaky nor a catch: the host disk was full, the in-test `go build`
		// could not write its objects, and the failure surfaced as a bare `exit status 1` under the
		// name of a test about documentation. A run that could not build the thing it tests has not
		// tested anything, and it must not look like it did.
		if strings.Contains(string(out), "no space left on device") {
			t.Fatalf("cannot build the CLI under test: THE DISK IS FULL. This is an environmental "+
				"failure, not a defect in the code under test and not a flaky assertion -- nothing "+
				"was exercised. Free space and run again.\n%s", out)
		}
		t.Fatalf("build the CLI under test: %v\n%s", err, out)
	}
	return built
}

type docRun struct {
	stdout   string
	exitCode int
}

// runDocumented runs one documented invocation in an isolated HOME and project directory.
//
// The environment is built from nothing rather than inherited. An operator token, an API override or a
// PATH entry pointing at an already-configured install would each make a first-run test pass for a
// reason a first-time reader does not have.
func runDocumented(t *testing.T, cli, home, projectDir string, args ...string) docRun {
	t.Helper()
	cmd := exec.Command(cli, args...)
	cmd.Dir = projectDir
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"NO_COLOR=1",
	}
	out, err := cmd.CombinedOutput()
	run := docRun{stdout: string(out)}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			run.exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return run
}

// cleanProject makes an empty directory that looks like a fresh Flutter app: a pubspec and nothing
// else. A documented command must work from here, not only from a repository checkout.
func cleanProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pubspec := "name: example_app\nenvironment:\n  sdk: '>=3.0.0 <4.0.0'\ndependencies:\n  flutter:\n    sdk: flutter\n"
	if err := os.WriteFile(filepath.Join(dir, "pubspec.yaml"), []byte(pubspec), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// --- the documented happy path, each step asserted on the state it leaves ---

func TestDocumentedVersionPrintsAVersion(t *testing.T) {
	cli := docCLI(t)
	run := runDocumented(t, cli, t.TempDir(), cleanProject(t), "version")
	if run.exitCode != 0 {
		t.Fatalf("`soroq version` exited %d: %s", run.exitCode, run.stdout)
	}
	if !strings.Contains(run.stdout, "soroq ") {
		t.Errorf("`soroq version` printed %q, which does not name the product and a version", run.stdout)
	}
}

func TestDocumentedHelpListsTheCommandsTheDocsUse(t *testing.T) {
	cli := docCLI(t)
	run := runDocumented(t, cli, t.TempDir(), cleanProject(t), "--help")
	if run.exitCode != 0 {
		t.Fatalf("`soroq --help` exited %d", run.exitCode)
	}
	// The commands the quickstart walks a beginner through, in order. If help stops listing one of
	// these, the quickstart has become undiscoverable even though every command still works.
	for _, command := range []string{
		"init", "setup", "login", "doctor", "flutter", "release", "patch", "rollback", "status",
		"version", "update", "uninstall",
	} {
		if !strings.Contains(run.stdout, "\n  "+command+" ") {
			t.Errorf("`soroq --help` does not list %q, which the quickstart tells a reader to run", command)
		}
	}
}

// `soroq init` is the first command that WRITES anything. The documentation says it creates
// soroq.yaml; this requires that file to exist and to carry the app id that was asked for.

// requiredJourney names a test that MUST run whenever its external dependency is installed, and the
// tool that dependency is. The registry is the contract; the check in TestMain reads it.
//
// EACH ENTRY CARRIES ITS OWN TOOL. An earlier version routed a git-dependent test through the Flutter
// predicate, which is the "one dependency over" defect this project keeps repeating: without git those
// tests failed instead of skipping, while the comment claimed the class was handled.
type requiredJourney struct {
	test string
	tool string
}

var requiredJourneys = []requiredJourney{
	{test: "TestDocumentedInitCreatesTheProjectFile", tool: "flutter"},
	{test: "TestExitCodeAndDocumentedBehaviourAreDifferentMeasurements", tool: "flutter"},
	{test: "TestFlutterRevisionOfPrefersGitOverShortVersion", tool: "git"},
}

// completedJourneys records journeys that ran to the END of their body. Completion, not entry, is what
// the check reads.
//
// WHY COMPLETION AND WHY IT IS NOT A COUNTER. The control this replaces incremented a "skips" counter
// on the skip path and a "runs" counter otherwise, then asserted runs==0 && skips>0 while flutter was
// installed. Both halves tested the same predicate, so it could not fire: an audit planted
// `t.Skip("temporarily disabled")` in both journeys with Flutter present, neither counter moved, and
// the suite reported success -- the exact scenario the comment said it prevented.
//
// A completion marker is independent of every skip path. It is written at the end of the body, so a
// skip anywhere before it -- for any reason, planted or genuine -- leaves the marker absent, and the
// check fails when the tool that would have allowed the run is installed.
var completedJourneys sync.Map

// requireTool skips a test whose SUBJECT needs an external tool, and says exactly what went unrun.
//
// A skip is right here rather than a failure: `soroq init` refusing without Flutter is an unmet
// PRECONDITION, and failing there blames the code for the machine.
func requireTool(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s is not on PATH, so this journey cannot run. NOT EXERCISED: %s. "+
			"CI installs it, so this does run there.", tool, t.Name())
	}
}

// journeyCompleted marks a journey as having reached the end of its body.
func journeyCompleted(t *testing.T) {
	t.Helper()
	completedJourneys.Store(t.Name(), true)
}

// checkRequiredJourneysRan fails the package when a journey did not complete although its dependency
// is installed. It runs from TestMain, after the whole suite, where the answer is knowable.
func checkRequiredJourneysRan() int {
	// ONLY WHEN THE WHOLE SUITE RAN. Under `-run SomethingElse` these journeys are filtered out by the
	// caller's own request, and reporting that as a failure would make every targeted invocation red
	// -- which teaches people to ignore the check, the opposite of what it is for.
	if filter := flag.Lookup("test.run"); filter != nil {
		if value := filter.Value.String(); value != "" && value != ".*" {
			return 0
		}
	}

	code := 0
	for _, journey := range requiredJourneys {
		if _, err := exec.LookPath(journey.tool); err != nil {
			continue // the dependency is genuinely absent; skipping was correct
		}
		if _, done := completedJourneys.Load(journey.test); !done {
			fmt.Fprintf(os.Stderr,
				"FAIL: %s is installed, and yet %s did not run to completion. A documentation suite "+
					"that exercises nothing must not report success.\n", journey.tool, journey.test)
			code = 1
		}
	}
	return code
}

func TestDocumentedInitCreatesTheProjectFile(t *testing.T) {
	requireTool(t, "flutter")
	cli := docCLI(t)
	project := cleanProject(t)
	run := runDocumented(t, cli, t.TempDir(), project,
		"init", "--app-id", "com.example.docjourney", "--channel", "stable")
	if run.exitCode != 0 {
		t.Fatalf("`soroq init` exited %d: %s", run.exitCode, run.stdout)
	}

	configPath := filepath.Join(project, "soroq.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("`soroq init` exited 0 but wrote no soroq.yaml: %v\n%s", err, run.stdout)
	}
	config := string(raw)
	if !strings.Contains(config, "com.example.docjourney") {
		t.Errorf("soroq.yaml does not record the app id that was asked for:\n%s", config)
	}
	if !strings.Contains(config, "stable") {
		t.Errorf("soroq.yaml does not record the channel that was asked for:\n%s", config)
	}
	journeyCompleted(t)
}

// `soroq doctor` on a fresh project must say what is MISSING. A doctor that reports a healthy project
// before anything is configured is the readiness-theatre defect in command form.
func TestDocumentedDoctorNamesWhatIsMissingOnAFreshProject(t *testing.T) {
	cli := docCLI(t)
	home := t.TempDir()
	project := cleanProject(t)
	runDocumented(t, cli, home, project, "init", "--app-id", "com.example.docjourney")

	run := runDocumented(t, cli, home, project, "doctor")
	lowered := strings.ToLower(run.stdout)
	// It must name at least one thing that is not ready, and tell the reader what to run next.
	if !strings.Contains(lowered, "todo") && !strings.Contains(lowered, "next") &&
		!strings.Contains(lowered, "not") {
		t.Errorf("`soroq doctor` on a project with no toolchain, no login and no release reported "+
			"nothing to fix:\n%s", run.stdout)
	}
	if !strings.Contains(lowered, "soroq ") {
		t.Errorf("`soroq doctor` did not name a command to run next:\n%s", run.stdout)
	}
}

func TestDocumentedStatusRunsFromACleanHomeWithoutCredentials(t *testing.T) {
	cli := docCLI(t)
	home := t.TempDir()
	project := cleanProject(t)
	runDocumented(t, cli, home, project, "init", "--app-id", "com.example.docjourney")

	run := runDocumented(t, cli, home, project, "status", "--local")
	if strings.Contains(strings.ToLower(run.stdout), "panic") {
		t.Fatalf("`soroq status --local` panicked from a clean HOME:\n%s", run.stdout)
	}
	if !strings.Contains(run.stdout, "com.example.docjourney") {
		t.Errorf("`soroq status --local` does not report the project it is standing in:\n%s", run.stdout)
	}
}

// --- the documented FAILURE paths, which are the ones a beginner actually hits ---

func TestDocumentedFailurePathsAreActionable(t *testing.T) {
	cli := docCLI(t)

	for _, tc := range []struct {
		name     string
		args     []string
		inited   bool
		seedHome bool   // put something in the HOME so there is something to act on
		wantExit bool   // true when the command must NOT exit 0
		wants    string // a phrase the message must contain, lowercased
	}{
		{
			name:     "a command that does not exist",
			args:     []string{"deploy"},
			wantExit: true,
			wants:    "unknown command",
		},
		{
			// It may infer an app id from the directory or refuse; either is defensible. What is NOT
			// defensible is saying nothing a reader can act on, so the assertion is on the message.
			// The earlier version asserted neither an exit code nor a phrase, which left it checking
			// only that the word "panic:" was absent.
			name:     "init with no app id",
			args:     []string{"init"},
			wantExit: true,
			// The message must name the FLAG that fixes it. Asserting the word "soroq" would pass on
			// almost any output from a binary called soroq, which is how the earlier version of this
			// case checked nothing.
			wants: "--app-id",
		},
		{
			// "soroq" appears in almost any output from a binary called soroq, so asserting it proves
			// close to nothing. The message must name what is missing AND how to fix it -- and it must
			// say so itself, not by echoing a directory path that happens to contain the word.
			name:     "patch before there is a project",
			args:     []string{"patch", "android"},
			wantExit: true,
			wants:    "soroq.yaml not found",
		},
		{
			// Seeded on purpose. With a genuinely empty HOME the honest answer is "nothing to
			// uninstall", and asserting the refusal there would be scoring a message that cannot
			// appear -- the vacuous-pass shape. The refusal is only meaningful when there IS
			// something to remove.
			name:     "uninstall without the confirmation the docs require",
			args:     []string{"uninstall"},
			seedHome: true,
			wantExit: true,
			wants:    "would remove",
		},
		{
			name:     "uninstall with nothing installed says so, and does not pretend to refuse",
			args:     []string{"uninstall"},
			wantExit: false,
			wants:    "nothing to uninstall",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			project := cleanProject(t)
			if tc.inited {
				runDocumented(t, cli, home, project, "init", "--app-id", "com.example.docjourney")
			}
			if tc.seedHome {
				if err := os.MkdirAll(filepath.Join(home, ".soroq", "frontends"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			run := runDocumented(t, cli, home, project, tc.args...)

			// THE PATHS COME OUT BEFORE THE PHRASE GOES IN.
			//
			// t.TempDir() names its directory after the SUBTEST, and the commands echo that path in
			// their errors. So a case asserting `wants: "project"` could match the word inside
			// `.../..._patch_before_there_is_a_project/001` rather than in anything the command said.
			// That is not what was happening here -- the message does say "project", on its second
			// line -- but nothing prevented it, and an assertion that COULD pass on its own fixture's
			// directory name is one bad rename away from proving nothing.
			//
			// Removing both spellings of each path first means the phrase has to come from the
			// message. Both spellings, because on macOS t.TempDir() returns `/var/folders/...` while
			// the command reports the symlink-resolved `/private/var/folders/...`.
			// BOTH SPELLINGS OF EACH PATH. On macOS t.TempDir() returns `/var/folders/...` while the
			// command reports the symlink-resolved `/private/var/folders/...`, so replacing only the
			// literal misses the one that actually appears -- and the assertion goes on matching the
			// path it was supposed to stop matching.
			message := strings.ToLower(run.stdout)
			for _, path := range []string{project, home} {
				candidates := []string{path}
				if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
					candidates = append(candidates, resolved)
				}
				for _, candidate := range candidates {
					message = strings.ReplaceAll(message, strings.ToLower(candidate), "<path>")
				}
			}

			if tc.wantExit && run.exitCode == 0 {
				t.Errorf("`soroq %s` exited 0; a documented failure path that succeeds silently leaves "+
					"the reader with nothing:\n%s", strings.Join(tc.args, " "), run.stdout)
			}
			if strings.Contains(message, "panic:") {
				t.Errorf("`soroq %s` panicked instead of explaining:\n%s",
					strings.Join(tc.args, " "), run.stdout)
			}
			if tc.wants != "" && !strings.Contains(message, tc.wants) {
				t.Errorf("`soroq %s` did not say %q (paths removed before matching, so the phrase "+
					"has to come from the message):\n%s",
					strings.Join(tc.args, " "), tc.wants, run.stdout)
			}
		})
	}
}

// --- the controls this file needs in order to mean anything ---

// A CLEAN HOME IS ACTUALLY CLEAN. Without this, every test above could be passing because it quietly
// read the developer's own credentials, and the suite would go green on a machine where nothing works
// for anybody else.
func TestTheJourneyHomeHoldsNoInheritedCredential(t *testing.T) {
	cli := docCLI(t)
	home := t.TempDir()
	project := cleanProject(t)

	run := runDocumented(t, cli, home, project, "whoami")
	lowered := strings.ToLower(run.stdout)
	if !strings.Contains(lowered, "not signed in") && !strings.Contains(lowered, "log in") &&
		!strings.Contains(lowered, "login") && run.exitCode == 0 {
		t.Errorf("`soroq whoami` from a clean HOME reported an identity; the journey is inheriting "+
			"credentials and proves nothing about a first run:\n%s", run.stdout)
	}

	// And nothing from the real HOME appeared in it.
	for _, leaked := range []string{".soroq/config.json", ".soroq/toolchains", ".soroq/frontends"} {
		if _, err := os.Stat(filepath.Join(home, leaked)); err == nil {
			t.Errorf("the isolated HOME contains %s, which it never created", leaked)
		}
	}
}

// NO WARM CACHE. A journey that only passes because a toolchain was already downloaded is not a
// journey a new developer can repeat.
func TestTheJourneyDoesNotDependOnAWarmCache(t *testing.T) {
	cli := docCLI(t)
	home := t.TempDir()
	project := cleanProject(t)
	runDocumented(t, cli, home, project, "init", "--app-id", "com.example.docjourney")

	// doctor must run and REPORT the missing toolchain rather than failing to start without one.
	run := runDocumented(t, cli, home, project, "doctor")
	if strings.Contains(strings.ToLower(run.stdout), "panic") {
		t.Fatalf("`soroq doctor` cannot run without a cache:\n%s", run.stdout)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".soroq"))
	if err == nil {
		for _, entry := range entries {
			if entry.Name() == "toolchains" || entry.Name() == "frontends" {
				contents, _ := os.ReadDir(filepath.Join(home, ".soroq", entry.Name()))
				if len(contents) > 0 {
					t.Errorf("the isolated HOME has a populated %s cache it never downloaded",
						entry.Name())
				}
			}
		}
	}
}

// THE CONTROL FOR THIS WHOLE FILE: exit codes and documented behaviour are different measurements.
//
// The first version of this test asserted the point with a hardcoded `stateAssertionCatchesIt := true`,
// so its self-check was dead code and what remained was an absence that was always true. An independent
// audit called it out, and it was exactly the "checkers that cannot fail" shape this file exists to
// guard against.
//
// It now RUNS the two commands and compares them. One exits 0 and writes the file the documentation
// promises; the other ALSO exits 0 and writes nothing. An exit-code check cannot tell them apart, and
// the state assertion can -- and the test fails if that stops being true.
func TestExitCodeAndDocumentedBehaviourAreDifferentMeasurements(t *testing.T) {
	requireTool(t, "flutter")
	cli := docCLI(t)

	// A command that exits 0 AND does what the documentation says.
	doesWhatItSays := cleanProject(t)
	honest := runDocumented(t, cli, t.TempDir(), doesWhatItSays,
		"init", "--app-id", "com.example.control")
	_, honestWroteFile := os.Stat(filepath.Join(doesWhatItSays, "soroq.yaml"))

	// A command that exits 0 and writes NO project file. `status --local` in a directory that is not a
	// Soroq project reports what it found and creates nothing.
	doesNothing := t.TempDir()
	vacuous := runDocumented(t, cli, t.TempDir(), doesNothing, "status", "--local")
	_, vacuousWroteFile := os.Stat(filepath.Join(doesNothing, "soroq.yaml"))

	// THE PREMISE. Both must exit 0, or an exit-code check would already separate them and this
	// control would be proving nothing.
	if honest.exitCode != 0 || vacuous.exitCode != 0 {
		t.Fatalf("this control needs two commands that BOTH exit 0; got %d and %d. Pick a different "+
			"pair, or the distinction it demonstrates is not the one being made.",
			honest.exitCode, vacuous.exitCode)
	}

	// THE DISTINCTION. One left the documented file, the other did not.
	if honestWroteFile != nil {
		t.Errorf("`soroq init` exited 0 and wrote no soroq.yaml:\n%s", honest.stdout)
	}
	if vacuousWroteFile == nil {
		t.Fatal("`soroq status --local` created a project file; this control assumed it does not, " +
			"so the pair no longer demonstrates the difference")
	}
	journeyCompleted(t)
}
