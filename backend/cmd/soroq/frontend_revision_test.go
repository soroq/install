package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestFlutterRevisionOfPrefersGitOverShortVersion locks the doctor false-negative fix: when
// `flutter --version --machine` is unavailable, flutterRevisionOf must read the FULL revision from the
// frontend's bundled .git (git rev-parse HEAD) rather than the SHORTENED revision that plain
// `flutter --version` prints (which caused a one-time first-run `frontend doctor` false negative).
func TestFlutterRevisionOfPrefersGitOverShortVersion(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Stub `flutter`: fails `--machine` (empty JSON), prints a SHORT revision on plain `--version`.
	stub := "#!/bin/sh\nif [ \"$2\" = \"--machine\" ]; then exit 1; fi\necho 'Flutter 3.46.0 • channel master'\necho 'Framework • revision f74781f621 (8 weeks ago)'\n"
	binPath := filepath.Join(binDir, "flutter")
	if err := os.WriteFile(binPath, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real .git at a known full HEAD.
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = root
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f")
	run("commit", "-q", "-m", "c")
	headOut, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	fullHead := string(headOut[:len(headOut)-1])

	got, err := flutterRevisionOf(binPath)
	if err != nil {
		t.Fatalf("flutterRevisionOf: %v", err)
	}
	if got != fullHead {
		t.Fatalf("expected FULL git revision %q, got %q (short-version false negative not fixed)", fullHead, got)
	}
}
