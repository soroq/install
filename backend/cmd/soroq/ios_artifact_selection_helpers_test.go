package main

import (
	"os"
	"strings"
	"testing"
)

// iosReleaseUsageForTest returns the usage text runReleaseIOS prints, read from source so the test
// exercises the real contract rather than a copy of it.
func iosReleaseUsageForTest() string {
	src, err := os.ReadFile("release_cmd.go")
	if err != nil {
		return ""
	}
	body := string(src)
	start := strings.Index(body, "func runReleaseIOS")
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "if err := fs.Parse")
	if end < 0 {
		return body[start:]
	}
	return body[start : start+end]
}

func iosReleaseSourceForTest(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("release_cmd.go")
	if err != nil {
		t.Fatalf("read release_cmd.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func runReleaseIOS")
	if start < 0 {
		t.Fatal("runReleaseIOS not found")
	}
	rest := body[start:]
	if end := strings.Index(rest, "\nfunc "); end > 0 {
		if next := strings.Index(rest[1:], "\nfunc "); next > 0 {
			return rest[:next+1]
		}
	}
	return rest
}
