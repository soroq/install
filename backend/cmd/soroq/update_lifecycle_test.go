package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE UPDATE LIFECYCLE, AUDITED RATHER THAN ASSUMED.
//
// installBinaries rolls back on every error it can SEE. It cannot see a process that is KILLED between
// the two renames -- a terminal closing, a laptop sleeping, an out-of-memory kill -- and that window
// leaves one binary replaced and the other in a .bak file. The natural recovery, running `soroq update`
// again, used to short-circuit at "already up to date" and never reach the code that could fix it, so
// the install stayed broken until a new release happened to appear.
//
// These tests simulate the crash by leaving the filesystem in exactly the states those windows produce,
// which is the only way to test a kill that a test process cannot perform on itself.

// crashState writes an install directory in one of the shapes an interrupted update leaves behind.
func crashState(t *testing.T, soroq, soroqctl, bakSoroq, bakCtl, staged string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if content == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("soroq", soroq)
	write("soroqctl", soroqctl)
	write(".soroq.bak", bakSoroq)
	write(".soroqctl.bak", bakCtl)
	write(".soroqctl.new", staged)
	return dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// A KILL BETWEEN THE TWO RENAMES LEAVES NO soroqctl. Recovery restores the previous one.
func TestAnInterruptedUpdateRestoresTheMissingBinary(t *testing.T) {
	dir := crashState(t, "new-soroq", "", "old-soroq", "old-soroqctl", "new-soroqctl")

	lines, interrupted := repairInterruptedUpdate(dir, false)
	if !interrupted {
		t.Fatal("an install dir with a backup and no soroqctl was not recognised as interrupted")
	}
	if got := read(t, filepath.Join(dir, "soroqctl")); got != "old-soroqctl" {
		t.Errorf("soroqctl after repair = %q, want the previous working one restored", got)
	}
	// The NEWER soroq must survive. Restoring it from the backup would be the silent downgrade this
	// product must never perform.
	if got := read(t, filepath.Join(dir, "soroq")); got != "new-soroq" {
		t.Errorf("soroq after repair = %q; the newer binary was replaced by the older one", got)
	}
	for _, leftover := range []string{".soroq.bak", ".soroqctl.bak", ".soroqctl.new"} {
		if _, err := os.Stat(filepath.Join(dir, leftover)); err == nil {
			t.Errorf("repair left %s behind", leftover)
		}
	}
	if len(lines) == 0 {
		t.Error("the repair reported nothing; an operator must be told their install was mid-update")
	}
	if !strings.Contains(strings.Join(lines, "\n"), "soroqctl") {
		t.Errorf("the repair message does not name what was restored: %v", lines)
	}
}

// A KILL AFTER BOTH RENAMES, BEFORE THE BACKUPS ARE CLEARED. Both binaries are the NEW ones, and both
// backups are still on disk. Restoring here would replace two newer binaries with two older ones.
func TestRecoveryNeverReplacesAPresentBinaryWithABackup(t *testing.T) {
	dir := crashState(t, "new-soroq", "new-soroqctl", "old-soroq", "old-soroqctl", "")

	if _, interrupted := repairInterruptedUpdate(dir, false); !interrupted {
		t.Fatal("leftover backups were not recognised as an interrupted update")
	}
	if got := read(t, filepath.Join(dir, "soroq")); got != "new-soroq" {
		t.Errorf("soroq = %q after repair; a NEWER binary was silently replaced by an older one", got)
	}
	if got := read(t, filepath.Join(dir, "soroqctl")); got != "new-soroqctl" {
		t.Errorf("soroqctl = %q after repair; a NEWER binary was silently replaced by an older one", got)
	}
	for _, leftover := range []string{".soroq.bak", ".soroqctl.bak"} {
		if _, err := os.Stat(filepath.Join(dir, leftover)); err == nil {
			t.Errorf("repair left %s behind, so the next run would report an interruption forever", leftover)
		}
	}
}

// A HEALTHY INSTALL IS NOT TOUCHED. The positive control: without it, a repair that deletes everything
// or reports an interruption every time would pass both tests above.
func TestAHealthyInstallIsNotReportedAsInterrupted(t *testing.T) {
	dir := crashState(t, "soroq-binary", "soroqctl-binary", "", "", "")

	lines, interrupted := repairInterruptedUpdate(dir, false)
	if interrupted {
		t.Errorf("a healthy install was reported as interrupted: %v", lines)
	}
	if len(lines) != 0 {
		t.Errorf("a healthy install produced repair output: %v", lines)
	}
	if got := read(t, filepath.Join(dir, "soroq")); got != "soroq-binary" {
		t.Errorf("a healthy soroq was modified: %q", got)
	}
	if got := read(t, filepath.Join(dir, "soroqctl")); got != "soroqctl-binary" {
		t.Errorf("a healthy soroqctl was modified: %q", got)
	}
}

// --check PROMISES ZERO FILESYSTEM CHANGES, INCLUDING WHILE REPAIRING NOTHING.
func TestCheckOnlyReportsAnInterruptionWithoutRepairingIt(t *testing.T) {
	dir := crashState(t, "new-soroq", "", "old-soroq", "old-soroqctl", "new-soroqctl")

	lines, interrupted := repairInterruptedUpdate(dir, true)
	if !interrupted {
		t.Fatal("--check did not notice the interruption")
	}
	if len(lines) == 0 {
		t.Error("--check noticed an interruption and said nothing")
	}
	// NOTHING moved.
	if _, err := os.Stat(filepath.Join(dir, "soroqctl")); err == nil {
		t.Error("--check restored soroqctl; it promises zero filesystem changes")
	}
	if _, err := os.Stat(filepath.Join(dir, ".soroqctl.bak")); err != nil {
		t.Error("--check removed a backup; it promises zero filesystem changes")
	}
}

// A CLEAN INSTALL: no previous binaries at all. The transactional path must handle a directory that
// has never held Soroq, because that is what the installer's first run looks like.
func TestACleanInstallIntoAnEmptyDirectory(t *testing.T) {
	f := setupUpdate(t, defaultUpdOpts())
	if err := os.Remove(f.soroqPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.ctlPath); err != nil {
		t.Fatal(err)
	}

	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("a clean install failed: %v\n%s", err, f.out.String())
	}
	for _, name := range []string{"soroq", "soroqctl"} {
		path := filepath.Join(f.installDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s was not installed: %v", name, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s was installed without an executable bit: %v", name, info.Mode())
		}
	}
	// No transactional leftovers from a run with nothing to back up.
	for _, leftover := range []string{".soroq.new", ".soroqctl.new", ".soroq.bak", ".soroqctl.bak"} {
		if _, err := os.Stat(filepath.Join(f.installDir, leftover)); err == nil {
			t.Errorf("a clean install left %s behind", leftover)
		}
	}
}

// A SAME-VERSION UPDATE IS A VERIFIED NO-OP: not merely a message, but no bytes changed anywhere.
func TestASameVersionUpdateChangesNothingOnDisk(t *testing.T) {
	opts := defaultUpdOpts()
	opts.current = stableVersion // already on the latest stable
	f := setupUpdate(t, opts)

	before, err := os.ReadDir(f.installDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("same-version update returned an error: %v", err)
	}
	if !strings.Contains(f.out.String(), "already up to date") {
		t.Errorf("a same-version update did not say so: %q", f.out.String())
	}
	f.assertOriginal()

	after, err := os.ReadDir(f.installDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Errorf("a same-version update changed the install directory: %d entries became %d",
			len(before), len(after))
	}
}

// A NEWER BINARY IS NEVER REPLACED BY AN OLDER ONE. This is the task's stop_if.
//
// The release feed offering an OLDER tag than what is installed -- a yanked release, a rolled-back
// publish, a mirror serving a stale index -- must leave the newer install exactly as it was.
func TestAnOlderReleaseNeverReplacesANewerInstall(t *testing.T) {
	opts := defaultUpdOpts()
	opts.current = "v9.9.9"        // far newer than anything the feed offers
	opts.stableTag = stableVersion // the feed offers an OLDER release
	f := setupUpdate(t, opts)

	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("update returned an error: %v", err)
	}
	f.assertOriginal()
	if strings.Contains(f.out.String(), "Updated successfully") {
		t.Errorf("an OLDER release was installed over a newer one: %q", f.out.String())
	}
	if !strings.Contains(f.out.String(), "already up to date") {
		t.Errorf("a downgrade attempt was not reported as up to date: %q", f.out.String())
	}
}

// THE POSITIVE TWIN. Without it, a version comparison that refuses EVERY update passes the test above.
func TestANewerReleaseIsStillInstalled(t *testing.T) {
	f := setupUpdate(t, defaultUpdOpts())
	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("update failed: %v\n%s", err, f.out.String())
	}
	if !strings.Contains(f.out.String(), "Updated successfully") {
		t.Errorf("a genuinely newer release was not installed: %q", f.out.String())
	}
	if bytes.Equal(mustRead(t, f.soroqPath), f.origSoroq) {
		t.Error("soroq was not replaced by the newer release")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

// REPAIR MUST RUN EVEN WHEN THERE IS NOTHING TO UPDATE, and that is the whole point.
//
// The tests above call repairInterruptedUpdate directly, so they cannot see WHERE it is called from.
// The original bug was exactly a placement bug: recovery sat after the "already up to date"
// short-circuit, so the one command an operator would reach for returned before repairing anything and
// the install stayed broken until a new release happened to appear. These two drive the whole
// performSelfUpdate path instead.
func TestAnInterruptedInstallIsRepairedEvenWhenAlreadyUpToDate(t *testing.T) {
	opts := defaultUpdOpts()
	opts.current = stableVersion // nothing to update: the short-circuit path
	f := setupUpdate(t, opts)

	// Leave the directory as a kill between the two renames leaves it.
	if err := os.Rename(f.ctlPath, filepath.Join(f.installDir, ".soroqctl.bak")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.installDir, ".soroqctl.new"), []byte("staged"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("update on an interrupted install returned an error: %v", err)
	}
	if _, err := os.Stat(f.ctlPath); err != nil {
		t.Fatalf("soroqctl was NOT restored by the command an operator would run: %v\n%s",
			err, f.out.String())
	}
	if !strings.Contains(f.out.String(), "interrupted") {
		t.Errorf("the repair was silent; an operator must be told: %q", f.out.String())
	}
	if _, err := os.Stat(filepath.Join(f.installDir, ".soroqctl.new")); err == nil {
		t.Error("the staged file from the interrupted run was left behind")
	}
}

func TestCheckOnDeliberatelyRepairsNothingThroughTheWholeCommand(t *testing.T) {
	opts := defaultUpdOpts()
	opts.current = stableVersion
	f := setupUpdate(t, opts)
	f.cfg.checkOnly = true

	if err := os.Rename(f.ctlPath, filepath.Join(f.installDir, ".soroqctl.bak")); err != nil {
		t.Fatal(err)
	}
	if err := performSelfUpdate(f.cfg); err != nil {
		t.Fatalf("--check returned an error: %v", err)
	}
	// It must SAY something, and change nothing.
	if !strings.Contains(f.out.String(), "interrupted") {
		t.Errorf("--check did not report the interrupted install: %q", f.out.String())
	}
	if !strings.Contains(f.out.String(), "Run `soroq update` to repair it.") {
		t.Errorf("--check reported an interruption without saying how to fix it: %q", f.out.String())
	}
	if _, err := os.Stat(f.ctlPath); err == nil {
		t.Error("--check restored soroqctl; it promises zero filesystem changes")
	}
	if _, err := os.Stat(filepath.Join(f.installDir, ".soroqctl.bak")); err != nil {
		t.Error("--check removed the backup; it promises zero filesystem changes")
	}
}
