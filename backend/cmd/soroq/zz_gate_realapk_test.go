package main

import (
	"os"
	"strings"
	"testing"
)

// TestAndroidGate_RealAPKs runs the production gate against the REAL base/candidate release APKs and the
// REAL resolved project. Skipped unless SOROQ_REAL_APK_BASE/CAND/PROJECT are set.
func TestAndroidGate_RealAPKs(t *testing.T) {
	base, cand, proj := os.Getenv("SOROQ_REAL_APK_BASE"), os.Getenv("SOROQ_REAL_APK_CAND"), os.Getenv("SOROQ_REAL_APK_PROJECT")
	if base == "" || cand == "" || proj == "" {
		t.Skip("set SOROQ_REAL_APK_BASE/CAND/PROJECT to run against real artifacts")
	}
	delta, err := assertAndroidDependencyDeliverable(proj, base, cand)
	if err != nil {
		t.Fatalf("the real riverpod dependency add must be ACCEPTED by the gate: %v", err)
	}
	if !delta.Changed || len(delta.Paths) != 1 || !strings.Contains(delta.Paths[0], "NOTICES.Z") {
		t.Fatalf("expected exactly the NOTICES.Z license delta, got %+v", delta)
	}
	t.Logf("ACCEPTED. license delta: %v\n%s", delta.Paths, delta.Warning())
}
