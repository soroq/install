package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain isolates HOME for the whole package.
//
// WHY. The credential loader resolves ~/.soroq/config.json through os.UserHomeDir(), so every test that
// exercised an authenticated command was silently reading the DEVELOPER'S REAL CREDENTIAL. That went
// unnoticed while the credential happened to be accepted; it surfaced the moment cross-origin sending
// was refused, because the real credential is issued for api.soroq.dev and the tests point at
// 127.0.0.1 httptest servers.
//
// Two things were wrong with that, and only one of them was the test failure:
//   - a unit test that depends on the machine's ambient login is not hermetic -- it passes or fails
//     according to who is logged in, and it would behave differently in CI;
//   - a test run was reaching for a production credential at all.
//
// Isolating HOME fixes both. Tests that WANT a credential set one explicitly, which is also the only
// way a reader can tell which credential a test is exercising.
// isolatedHomePrefix is the one prefix this package creates. Nothing else in the tree creates it,
// which is what makes a sweep by prefix safe.
const isolatedHomePrefix = "soroq-cli-test-home-"

// stableGoEnv captures the REAL Go cache locations before HOME is replaced.
//
// THE LEAK THIS CLOSES, measured rather than assumed. Tests run `go build` as a subprocess, and that
// subprocess inherits the isolated HOME. Go then resolves GOPATH to $HOME/go and GOMODCACHE to
// $HOME/go/pkg/mod, re-downloads every module into the temporary HOME, and writes the module cache
// READ-ONLY -- `dr-xr-xr-x` on each version directory. os.RemoveAll then fails on those directories,
// and the failure was discarded, so every package run left a directory behind. 193 had accumulated.
//
// Capturing the real locations first and passing them through means nothing is written into the
// temporary HOME at all: no duplicated module cache, no read-only tree, nothing to fail to remove.
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

// removeIsolatedHome removes a temporary HOME, defeating Go's read-only module cache if one is there.
//
// It returns an error rather than discarding one. A cleanup that fails silently is how this leak ran
// for as long as it did.
func removeIsolatedHome(home string) error {
	if err := os.RemoveAll(home); err == nil {
		return nil
	}
	// Go writes module cache directories read-only. Make them writable, then try once more.
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

// sweepStaleIsolatedHomes removes leftovers from earlier runs.
//
// WHY A SWEEP AND NOT ONLY A DEFER. An unrecovered panic in a test kills the process: m.Run() never
// returns and no defer in TestMain runs, so the deferred removal cannot cover that case. The sweep
// does, one run later, and it is also what drains leftovers from every run that leaked before this
// fix existed.
//
// It only removes directories carrying this package's own prefix, and only ones older than a grace
// period, so a concurrent `go test` of another package cannot have its HOME pulled out from under it.
// sweepGracePeriod is how old a leftover must be before the sweep will remove it.
//
// The grace period stops a concurrent `go test` of another package having its HOME pulled out from
// under it. SOROQ_TEST_HOME_SWEEP_AGE overrides it so a control can force an immediate sweep and prove
// the panic case is reclaimed; nothing outside the tests sets it.
func sweepGracePeriod(defaultAge time.Duration) time.Duration {
	if raw := os.Getenv("SOROQ_TEST_HOME_SWEEP_AGE"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			return parsed
		}
	}
	return defaultAge
}

func sweepStaleIsolatedHomes(olderThan time.Duration) int {
	olderThan = sweepGracePeriod(olderThan)
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
	// CAPTURE BEFORE REPLACING. Once HOME points at the temporary directory, `go env` would report
	// the temporary locations and the capture would be worthless.
	goEnv := stableGoEnv()

	sweepStaleIsolatedHomes(10 * time.Minute)

	home, err := os.MkdirTemp("", isolatedHomePrefix)
	if err != nil {
		panic("create isolated test HOME: " + err.Error())
	}

	if err := os.MkdirAll(filepath.Join(home, ".soroq"), 0o700); err != nil {
		panic("create isolated .soroq: " + err.Error())
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home) // windows
	// The captured locations are exported so every `go build` subprocess shares the real caches
	// instead of building a duplicate inside the temporary HOME.
	for name, value := range goEnv {
		os.Setenv(name, value)
	}
	// A stray operator token in the environment would defeat the isolation just as thoroughly.
	os.Unsetenv("SOROQ_CONTROL_PLANE_OPERATOR_TOKEN")
	os.Unsetenv("SOROQ_OPERATOR_TOKEN")
	os.Unsetenv("SOROQ_OPERATOR_EMAIL")
	os.Unsetenv("SOROQ_API")

	// THE INNER FUNCTION IS THE POINT. os.Exit does not run defers, so a defer in TestMain itself
	// would never fire. Running the suite inside a function that returns the code lets the deferred
	// removal happen first, on both the passing and the failing path.
	code := func() int {
		defer func() {
			if err := removeIsolatedHome(home); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: could not remove the isolated test HOME %s: %v\n",
					home, err)
			}
		}()
		return runSuite(m)
	}()

	os.Exit(code)
}

// runSuite runs the tests and applies the package-level checks that need the whole run to be over.
func runSuite(m *testing.M) int {
	code := m.Run()
	if code != 0 {
		return code
	}
	return checkRequiredJourneysRan()
}
