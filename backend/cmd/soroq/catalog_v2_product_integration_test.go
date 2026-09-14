package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- the candidate CLI must actually OFFER the v2 flags ---------------------------------------------

// A matrix nobody can reach from the command line is not shipped. `soroq setup --help` must name both
// flags, because that help text is the only place a user learns the opt-in exists.
func TestSetupHelpExposesTheV2Flags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	runErr := runSetup([]string{"--help"})
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatalf("setup --help returned an error: %v", runErr)
	}
	for _, flag := range []string{"--catalog-v2", "--flutter-revision"} {
		if !strings.Contains(string(out), flag) {
			t.Fatalf("setup --help does not mention %s, so nobody can discover the opt-in:\n%s", flag, out)
		}
	}
}

// --- an unusable revision must leave the machine untouched -----------------------------------------

// countFiles is how "installed nothing" is proven: not by trusting an error string, but by looking at
// what the CLI left behind.
func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func TestUnknownOrInvalidRevisionInstallsNothing(t *testing.T) {
	for name, rev := range map[string]string{
		"unknown but well-formed": "0000000000000000000000000000000000000000",
		"too short":               "deadbeef",
		"not hexadecimal":         "zzzz781f62134f0b4a4a4b4c9b9f7e2a1c9d0e5b",
		"empty":                   "",
	} {
		t.Run(name, func(t *testing.T) {
			signer := setupTestSigner(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			fcalls, tcalls := stubV2Installs(t)
			srv := bothCatalogServer(t, signer, v1CatalogBytes(), v2MatrixBytes())
			defer srv.Close()

			before := countFiles(t, home)
			err := runSetup([]string{"ios", "--api", srv.URL, "--catalog-v2", "--flutter-revision", rev})
			if err == nil {
				t.Fatal("an unusable revision was accepted")
			}
			// No install was even attempted...
			if len(*fcalls) != 0 || len(*tcalls) != 0 {
				t.Fatalf("installs were attempted for an unusable revision: frontend=%v toolchain=%v", *fcalls, *tcalls)
			}
			// ...and nothing was written to the machine.
			if after := countFiles(t, home); after != before {
				t.Fatalf("refused setup still wrote %d file(s) under HOME", after-before)
			}
			// A refusal must never silently become v1's pair.
			if strings.Contains(strings.ToLower(err.Error()), "fallback") {
				t.Fatalf("an unknown revision fell back instead of refusing: %v", err)
			}
		})
	}
}

// The live-catalog check is NOT here. It reaches the production control plane and pins bytes that are
// deliberately mutable, so keeping it in the default suite made every ordinary `go test ./...` -- and
// every exported public run -- require internet access and fail whenever a catalog is legitimately
// republished. It lives behind the `soroq_release_gate` build tag (live_catalog_release_gate_test.go),
// excluded from compilation rather than skipped at runtime, and is invoked deliberately by
// scripts/check-live-catalogs.sh from the release workflow.
