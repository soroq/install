package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFrontend writes a frontend into `home`'s store and makes it active, the way
// `soroq frontend install` does.
func installFrontendForResolution(t *testing.T, home, version string, active bool) string {
	t.Helper()
	dir := filepath.Join(home, ".soroq", "frontends", version, defaultFrontendSubdir, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "flutter")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if active {
		raw, err := json.Marshal(map[string]string{"version": version, "flutter_bin": bin})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".soroq", "frontends", "active.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// TWO HOMES. The whole point: a frontend installed for another user must never be selected for this
// one without being named.
func TestFrontendResolution_TwoHomesDoNotCross(t *testing.T) {
	alice, bob := t.TempDir(), t.TempDir()
	aliceBin := installFrontendForResolution(t, alice, "frontend-alice", true)
	bobBin := installFrontendForResolution(t, bob, "frontend-bob", true)

	t.Setenv("SOROQ_FLUTTER_BIN", "")
	t.Setenv("HOME", alice)
	got, err := resolveSoroqFlutterFrontend()
	if err != nil {
		t.Fatal(err)
	}
	if got.Bin != aliceBin {
		t.Fatalf("alice resolved %q, want her own %q", got.Bin, aliceBin)
	}
	if strings.Contains(got.Bin, bob) {
		t.Fatalf("alice reached into bob's home: %q", got.Bin)
	}
	if !strings.Contains(got.Provenance, "frontend-alice") {
		t.Fatalf("provenance does not name the frontend: %q", got.Provenance)
	}

	t.Setenv("HOME", bob)
	got, err = resolveSoroqFlutterFrontend()
	if err != nil {
		t.Fatal(err)
	}
	if got.Bin != bobBin {
		t.Fatalf("bob resolved %q, want his own %q", got.Bin, bobBin)
	}
}

// MULTIPLE FRONTENDS IN ONE HOME. The ACTIVE one wins, not whichever sorts first or was installed
// last — a developer who switches frontends must get the one they switched to.
func TestFrontendResolution_ActiveWinsAmongMany(t *testing.T) {
	home := t.TempDir()
	installFrontendForResolution(t, home, "frontend-aaa-old", false)
	want := installFrontendForResolution(t, home, "frontend-zzz-new", true)
	installFrontendForResolution(t, home, "frontend-mmm-other", false)

	t.Setenv("SOROQ_FLUTTER_BIN", "")
	t.Setenv("HOME", home)
	got, err := resolveSoroqFlutterFrontend()
	if err != nil {
		t.Fatal(err)
	}
	if got.Bin != want {
		t.Fatalf("resolved %q, want the ACTIVE %q", got.Bin, want)
	}
}

// A `soroq-flutter` on PATH used to OUTRANK the installed frontend, silently. PATH is not
// home-relative and is not the developer's selection.
func TestFrontendResolution_PathShimDoesNotWin(t *testing.T) {
	home, shimDir := t.TempDir(), t.TempDir()
	want := installFrontendForResolution(t, home, "frontend-installed", true)
	shim := filepath.Join(shimDir, "soroq-flutter")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOROQ_FLUTTER_BIN", "")
	t.Setenv("HOME", home)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := resolveSoroqFlutterFrontend()
	if err != nil {
		t.Fatal(err)
	}
	if got.Bin == shim {
		t.Fatal("a soroq-flutter on PATH won over the installed frontend")
	}
	if got.Bin != want {
		t.Fatalf("resolved %q, want %q", got.Bin, want)
	}
}

// An active RECORD that points outside this home's store is refused rather than followed. That is the
// shape a copied or stale record takes, and following it is a silent cross-home selection.
func TestFrontendResolution_RecordOutsideTheStoreIsRefused(t *testing.T) {
	home, elsewhere := t.TempDir(), t.TempDir()
	foreign := installFrontendForResolution(t, elsewhere, "frontend-foreign", false)
	store := filepath.Join(home, ".soroq", "frontends")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"version": "frontend-foreign", "flutter_bin": foreign})
	if err := os.WriteFile(filepath.Join(store, "active.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOROQ_FLUTTER_BIN", "")
	t.Setenv("HOME", home)

	_, err := resolveSoroqFlutterFrontend()
	if err == nil {
		t.Fatal("a record pointing outside the store was followed")
	}
	if !strings.Contains(err.Error(), "outside this user's frontend store") {
		t.Fatalf("refusal does not explain itself: %v", err)
	}
	if !strings.Contains(err.Error(), "SOROQ_FLUTTER_BIN") {
		t.Fatalf("refusal does not name the deliberate route: %v", err)
	}
}

// THE DELIBERATE ROUTE still works, and says so. An external frontend must remain reachable — by name.
func TestFrontendResolution_ExplicitOverrideIsAllowedAndNamed(t *testing.T) {
	home, elsewhere := t.TempDir(), t.TempDir()
	installFrontendForResolution(t, home, "frontend-installed", true)
	foreign := installFrontendForResolution(t, elsewhere, "frontend-foreign", false)

	t.Setenv("HOME", home)
	t.Setenv("SOROQ_FLUTTER_BIN", foreign)
	got, err := resolveSoroqFlutterFrontend()
	if err != nil {
		t.Fatal(err)
	}
	if got.Bin != foreign {
		t.Fatalf("the explicit override was ignored: %q", got.Bin)
	}
	if !strings.Contains(got.Provenance, "SOROQ_FLUTTER_BIN") {
		t.Fatalf("provenance hides that an override was used: %q", got.Provenance)
	}
}

// A home with no frontend gets a refusal that names both routes, not a silent reach elsewhere.
func TestFrontendResolution_NoFrontendRefusesWithBothRoutes(t *testing.T) {
	t.Setenv("SOROQ_FLUTTER_BIN", "")
	t.Setenv("HOME", t.TempDir())
	_, err := resolveSoroqFlutterFrontend()
	if err == nil {
		t.Fatal("a home with no frontend resolved one anyway")
	}
	for _, want := range []string{"soroq frontend install", "SOROQ_FLUTTER_BIN"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not mention %q: %v", want, err)
		}
	}
}
