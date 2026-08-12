package main

// DELIVERY MODES MUST BE WIRED, NOT MERELY DEFINED.
//
// The schema shipped with CarriablePackages/BaseReferencePackages having ZERO production callers,
// newPackageNames still returning Added only, and the capability map still derived from raw
// Capability.Eligible. Every unit test passed and the modes affected nothing.
//
// That class of gap is structural: a helper with no call site produces no failing behaviour, so only a
// source-level assertion catches it. These tests are deliberately about WIRING, not behaviour.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func prodSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk("..", func(p string, info os.FileInfo, err error) error {
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
	return out
}

func callers(t *testing.T, needle string) []string {
	t.Helper()
	var hits []string
	for path, src := range prodSources(t) {
		// Skip the file that DEFINES it -- a definition is not a call site.
		if strings.Contains(src, "func (d Descriptor) "+strings.TrimSuffix(needle, "()")) {
			continue
		}
		if strings.Contains(src, needle) {
			hits = append(hits, path)
		}
	}
	return hits
}

// The carriage set must be consumed by production, not just defined.
func TestCarriablePackagesHasAProductionCallSite(t *testing.T) {
	if got := callers(t, "CarriablePackages()"); len(got) == 0 {
		t.Fatal("CarriablePackages() has no production caller; the delivery classification does not " +
			"reach the analyzer, so an upgraded carriable package would silently not be carried")
	}
}

// THE BUG THIS PINS: --new-packages used to be built from Descriptor.Added only, so an eligible
// pure-Dart UPGRADE was never carried and the device kept executing the BASE's older copy. That is
// silent wrong code, not a refusal.
func TestNewPackagesArgumentComesFromTheCarriableSet(t *testing.T) {
	src, err := os.ReadFile("freehand_patch.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if strings.Contains(s, "func newPackageNames(") {
		t.Error("newPackageNames still exists; it returned Added only and missed eligible upgrades")
	}
	i := strings.Index(s, "func carriablePackageNames(")
	if i < 0 {
		t.Fatal("carriablePackageNames is gone; --new-packages would no longer follow the delivery modes")
	}
	body := s[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "CarriablePackages()") {
		t.Error("carriablePackageNames does not use the descriptor's carriable set")
	}
	if strings.Contains(body, "d.Added") {
		t.Error("carriablePackageNames still reads d.Added directly; upgrades would be dropped again")
	}
}

// The analyzer map must express the MODE, not raw eligibility -- raw eligibility cannot represent
// base_reference_only, and feeding the analyzer `eligible:false` for an unchanged plugin would make it
// refuse a reference the base already satisfies.
func TestCapabilityMapExpressesDeliveryMode(t *testing.T) {
	src, err := os.ReadFile("freehand_patch.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func writeCapabilityMap(")
	if i < 0 {
		t.Fatal("writeCapabilityMap is gone")
	}
	body := s[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	for _, want := range []string{"Descriptor", "ModeCarriable", `"mode"`} {
		if !strings.Contains(body, want) {
			t.Errorf("writeCapabilityMap does not carry %s; the analyzer cannot distinguish "+
				"carriable from base_reference_only", want)
		}
	}
}

// EXACTLY ONE authoritative deliverability gate. Two entry points would eventually disagree, and the
// disagreement would be silent.
func TestExactlyOneDeliverabilityGate(t *testing.T) {
	for path, src := range prodSources(t) {
		if strings.Contains(src, "func (d Descriptor) AssessDelivery(") {
			t.Errorf("%s defines a second deliverability gate (AssessDelivery); there must be one", path)
		}
	}
	if got := callers(t, ".Assess()"); len(got) == 0 {
		t.Fatal("Assess() has no production caller; nothing refuses an undeliverable dependency change")
	}
}
