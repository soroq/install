package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE ISOLATED TEST HOME IS REMOVED ON EVERY EXIT PATH, INCLUDING A PANIC.
//
// WHAT LEAKED AND WHY. TestMain replaces HOME for the whole package, and tests run `go build` as a
// subprocess. That subprocess inherited the temporary HOME, so Go resolved GOMODCACHE to
// $HOME/go/pkg/mod, re-downloaded every module there, and wrote the module cache READ-ONLY --
// `dr-xr-xr-x` on each version directory. os.RemoveAll then failed on those directories, and its error
// was discarded, so every package run left a directory behind. 193 had accumulated before anyone
// looked.
//
// Two fixes, and this file proves both:
//   - the real GOPATH/GOCACHE/GOMODCACHE are captured BEFORE HOME is replaced and exported, so nothing
//     is written into the temporary HOME to begin with;
//   - removal happens in a deferred call inside an inner function, because os.Exit does not run
//     defers, and it defeats read-only directories rather than giving up silently.
//
// THE PANIC CASE CANNOT BE COVERED BY A DEFER AT ALL. An unrecovered panic kills the process, m.Run()
// never returns, and no defer in TestMain fires. The sweep at TestMain start is what covers it, one
// run later. These controls assert the pair.

// countIsolatedHomes reports how many of this package's temporary HOMEs exist right now.
func countIsolatedHomes(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), isolatedHomePrefix) {
			n++
		}
	}
	return n
}

// runNestedSuite compiles and runs a throwaway package whose TestMain is this package's contract, and
// whose single test behaves as the case requires. A SUBPROCESS is the only honest way to observe what
// happens at process exit -- including a panic, which cannot be caught in the process under test.
func runNestedSuite(t *testing.T, behaviour string) (int, string) {
	t.Helper()
	return runNestedSuiteWithSweep(t, behaviour, "")
}

// runNestedSuiteWithSweep is runNestedSuite with an explicit sweep grace period, so a control can
// force the next start to reclaim immediately instead of waiting out the real grace period.
func runNestedSuiteWithSweep(t *testing.T, behaviour, sweepAge string) (int, string) {
	t.Helper()
	dir := t.TempDir()

	source := `package nested

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const isolatedHomePrefix = "soroq-cli-test-home-"

func stableGoEnv() map[string]string {
	captured := map[string]string{}
	for _, name := range []string{"GOPATH", "GOCACHE", "GOMODCACHE"} {
		out, err := exec.Command("go", "env", name).Output()
		if err != nil {
			continue
		}
		if value := strings.TrimSpace(string(out)); value != "" {
			captured[name] = value
		}
	}
	return captured
}

func removeIsolatedHome(home string) error {
	if err := os.RemoveAll(home); err == nil {
		return nil
	}
	_ = filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o755)
		}
		return nil
	})
	return os.RemoveAll(home)
}

func sweepStaleIsolatedHomes(olderThan time.Duration) int {
	if raw := os.Getenv("SOROQ_TEST_HOME_SWEEP_AGE"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			olderThan = parsed
		}
	}
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return 0
	}
	swept := 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), isolatedHomePrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < olderThan {
			continue
		}
		if removeIsolatedHome(filepath.Join(os.TempDir(), entry.Name())) == nil {
			swept++
		}
	}
	return swept
}

func TestMain(m *testing.M) {
	goEnv := stableGoEnv()
	sweepStaleIsolatedHomes(10 * time.Minute)
	home, err := os.MkdirTemp("", isolatedHomePrefix)
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	for name, value := range goEnv {
		os.Setenv(name, value)
	}
	code := func() int {
		defer removeIsolatedHome(home)
		return m.Run()
	}()
	os.Exit(code)
}

func TestBehaviour(t *testing.T) {
	switch os.Getenv("NESTED_BEHAVIOUR") {
	case "fail":
		t.Fatal("deliberate failure")
	case "panic":
		panic("deliberate panic")
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "nested_test.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nested\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "./...", "-count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NESTED_BEHAVIOUR="+behaviour)
	if sweepAge != "" {
		cmd.Env = append(cmd.Env, "SOROQ_TEST_HOME_SWEEP_AGE="+sweepAge)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run nested suite: %v\n%s", err, out)
	}
	return code, string(out)
}

func TestTheIsolatedHomeIsRemovedOnEveryExitPath(t *testing.T) {
	for _, tc := range []struct {
		behaviour string
		wantExit  bool // true when the nested suite must NOT exit 0
	}{
		{"pass", false},
		{"fail", true},
		{"panic", true},
	} {
		t.Run(tc.behaviour, func(t *testing.T) {
			before := countIsolatedHomes(t)
			code, out := runNestedSuite(t, tc.behaviour)
			after := countIsolatedHomes(t)

			if tc.wantExit && code == 0 {
				t.Fatalf("the nested %s suite exited 0; the case did not happen:\n%s", tc.behaviour, out)
			}
			if !tc.wantExit && code != 0 {
				t.Fatalf("the nested passing suite exited %d:\n%s", code, out)
			}

			if tc.behaviour == "panic" {
				// A PANIC RUNS NO DEFER AT ALL. m.Run() never returns, the process dies, and no
				// cleanup in TestMain can fire -- so this case is expected to leave one behind, and
				// pretending otherwise would be the vacuous shape.
				//
				// What must be true is that it does not ACCUMULATE: the next run's sweep reclaims it.
				// That is the claim, and this is the proof.
				if after <= before {
					t.Fatalf("the panic case left nothing behind (%d -> %d). This control assumes a "+
						"panic leaks, and it no longer does -- so it is proving nothing about the "+
						"sweep. Check what changed before deleting this test.", before, after)
				}
				reclaimed, out := runNestedSuiteWithSweep(t, "pass", "0s")
				if reclaimed != 0 {
					t.Fatalf("the follow-up run exited %d:\n%s", reclaimed, out)
				}
				if final := countIsolatedHomes(t); final > before {
					t.Errorf("the sweep did not reclaim the panicked run's HOME: %d before, %d after "+
						"the panic, %d after the next run. A leak that survives the next start is a "+
						"leak.", before, after, final)
				}
				return
			}

			// THE ASSERTION for the paths a defer can reach. A leak shows up as a directory count
			// that did not come back down.
			if after > before {
				t.Errorf("the %s case leaked %d isolated HOME(s) (%d before, %d after). "+
					"os.Exit does not run defers, so removal has to happen in an inner function.",
					tc.behaviour, after-before, before, after)
			}
		})
	}
}

// A READ-ONLY MODULE CACHE MUST NOT DEFEAT REMOVAL.
//
// This is the exact shape that leaked: Go writes module cache directories `dr-xr-xr-x`, and a plain
// os.RemoveAll fails on them. The fix chmods and retries; without it this test fails, which is what
// makes it a control rather than a restatement.
func TestRemovalDefeatsAReadOnlyModuleCache(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	readOnly := filepath.Join(home, "go", "pkg", "mod", "example.com", "thing@v1.0.0")
	if err := os.MkdirAll(readOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readOnly, "file.go"), []byte("package thing\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Read-only from the deepest directory outwards, the way Go leaves it.
	for _, dir := range []string{readOnly, filepath.Dir(readOnly)} {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
	}

	// The unpatched behaviour, kept as the negative half: a plain RemoveAll cannot do this.
	if err := os.RemoveAll(home); err == nil {
		t.Skip("this platform's RemoveAll already handles read-only directories; the guard is moot here")
	}

	if err := removeIsolatedHome(home); err != nil {
		t.Fatalf("removeIsolatedHome could not remove a read-only module cache: %v", err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("the HOME still exists after removeIsolatedHome")
	}
}

// NOTHING IS WRITTEN INTO THE TEMPORARY HOME IN THE FIRST PLACE.
//
// The removal fixes above are the second line of defence. The first is that the real cache locations
// are captured before HOME is replaced, so a `go build` subprocess shares them instead of building a
// duplicate module cache inside the temporary HOME. If this fails, every package run downloads the
// module graph again -- which is what made the leaked directories grow.
func TestTheIsolatedHomeHoldsNoGoCache(t *testing.T) {
	home := os.Getenv("HOME")
	if !strings.Contains(home, isolatedHomePrefix) {
		t.Fatalf("HOME is not the isolated one (%s); this control cannot mean anything", home)
	}
	for _, unwanted := range []string{
		filepath.Join(home, "go", "pkg", "mod"),
		filepath.Join(home, "Library", "Caches", "go-build"),
	} {
		if info, err := os.Stat(unwanted); err == nil && info.IsDir() {
			t.Errorf("a Go cache was created inside the isolated HOME at %s. GOPATH/GOCACHE/GOMODCACHE "+
				"must be captured BEFORE HOME is replaced and exported, or every run duplicates the "+
				"module graph into a temporary directory and writes it read-only.", unwanted)
		}
	}
	// The positive half: the captured values must actually point somewhere outside the temporary HOME.
	for _, name := range []string{"GOCACHE", "GOMODCACHE"} {
		value := os.Getenv(name)
		if value == "" {
			t.Errorf("%s is not exported; a subprocess would fall back to $HOME and duplicate it", name)
			continue
		}
		if strings.HasPrefix(value, home) {
			t.Errorf("%s points inside the isolated HOME (%s)", name, value)
		}
	}
	fmt.Fprintf(os.Stderr, "isolated HOME %s holds no Go cache; GOCACHE=%s\n", home, os.Getenv("GOCACHE"))
}
