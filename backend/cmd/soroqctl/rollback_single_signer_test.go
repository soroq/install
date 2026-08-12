package main

// ONE ROLLBACK SIGNER, AND IT VALIDATES BEFORE IT SIGNS.
//
// The tree carried two rollback implementations. The device-proven production path
// (`soroqctl rollback ios-engine`) marshalled `{version:0, patches:[]}` and signed it with no
// assertion; a fully-tested parallel publisher in cmd/soroq owned the strict validator and the
// runtime-id format check and had ZERO call sites. The safety lived in the code nothing ran.
//
// These tests pin both halves of the fix, at the source level, because the property is structural:
// a future edit that reintroduces a second signer, or that removes the pre-sign assertion, is what
// this must catch -- and neither shows up as a behavioural failure in a passing rollback.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// goSources returns every non-test .go file under the backend, so the assertions below cover the
// whole module rather than this package alone.
func goSources(t *testing.T) map[string]string {
	t.Helper()
	root := ".."
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(p)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 20 {
		t.Fatalf("source scan found only %d files; the walk is not covering the module", len(out))
	}
	return out
}

// There must be exactly ONE place that produces a signed version-0 manifest.
func TestOnlyOneRollbackSignerExists(t *testing.T) {
	// A rollback signer is a function that both builds a version-0 manifest and signs it.
	buildsV0 := regexp.MustCompile(`Version:\s*0\b|"version":\s*0\b|reservedRollbackVersion`)
	signs := regexp.MustCompile(`signEngineManifest|signFreehandManifest|ed25519\.Sign|SigningKey`)

	var signers []string
	for path, src := range goSources(t) {
		if strings.Contains(path, "internal/signing/engine_rollback.go") {
			continue // the shared ASSERTION, not a signer -- it contains no signing call
		}
		if buildsV0.MatchString(src) && signs.MatchString(src) {
			signers = append(signers, path)
		}
	}
	if len(signers) != 1 {
		t.Fatalf("expected exactly one rollback signer, found %d: %v\n"+
			"two independently maintained signers for an instruction that reverts a user's app is the "+
			"hazard this test exists to prevent", len(signers), signers)
	}
	if !strings.Contains(signers[0], "soroqctl/") {
		t.Errorf("the surviving rollback signer must be the device-proven soroqctl path, got %s", signers[0])
	}
}

// The deleted parallel publisher must stay deleted.
func TestDeletedRollbackPublisherHasNotReturned(t *testing.T) {
	for path, src := range goSources(t) {
		for _, gone := range []string{
			"func publishFreehandRollback",
			"func ValidateFreehandRollbackManifest",
			"func buildFreehandRollbackManifest",
			"type FreehandRollbackBinding",
		} {
			if strings.Contains(src, gone) {
				t.Errorf("%s reintroduces %q; the rollback signer/validator is shared in internal/signing", path, gone)
			}
		}
	}
}

// The surviving signer must ASSERT before it signs -- not merely somewhere in the function.
func TestProductionRollbackAssertsBeforeSigning(t *testing.T) {
	src, err := os.ReadFile("ios_engine_patch.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	fn := strings.Index(s, "func runRollbackIOSEngine")
	if fn < 0 {
		t.Fatal("the production rollback entry point is gone")
	}
	body := s[fn:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	assertAt := strings.Index(body, "signing.AssertEngineRollbackManifest")
	signAt := strings.Index(body, "signEngineManifest(manifestBytes")
	if assertAt < 0 {
		t.Fatal("the production rollback path no longer asserts the manifest before signing")
	}
	if signAt < 0 {
		t.Fatal("could not locate the signing call")
	}
	if assertAt > signAt {
		t.Fatal("the rollback is signed BEFORE it is asserted; a signature would vouch for an " +
			"unchecked instruction")
	}
	// The binding must be threaded through, or the runtime-id format check is dead weight.
	for _, want := range []string{"AppID:", "Channel:", "ReleaseID:", "RuntimeID:"} {
		if !strings.Contains(body, want) {
			t.Errorf("rollback assertion is missing binding field %s", want)
		}
	}
}
