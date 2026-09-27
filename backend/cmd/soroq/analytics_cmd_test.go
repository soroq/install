package main

import (
	"bytes"
	"strings"
	"testing"
)

func analyticsFixture() analyticsView {
	var v analyticsView
	v.AppID = "com.example.app"
	v.Patches = []analyticsPatchRow{
		{PatchNumber: 2, Channel: "stable", ReleaseID: "rel-b", Observed: true, SuccessfulDevices: 9, FailedDevices: 1, FailureClasses: map[string]int{"crash_after_launch": 1}},
		{PatchNumber: 2, Channel: "stable", ReleaseID: "rel-a", Observed: false},
		{PatchNumber: 1, Channel: "stable", ReleaseID: "rel-a", Observed: true, SuccessfulDevices: 0, FailedDevices: 3, RolledBack: true, FailureClasses: map[string]int{"crash_before_first_frame": 3}},
		{PatchNumber: 1, Channel: "beta", ReleaseID: "rel-b", Observed: true, SuccessfulDevices: 4},
	}
	v.Totals.Patches = 4
	return v
}

func TestFilterAnalyticsRecomputesTotals(t *testing.T) {
	got := filterAnalytics(analyticsFixture(), "rel-a", "")
	if got.Totals.Patches != 2 || got.Totals.ObservedPatches != 1 || got.Totals.FailedDevices != 3 || got.Totals.RolledBackPatches != 1 {
		t.Fatalf("totals %+v", got.Totals)
	}
	if got.FailureClasses["crash_before_first_frame"] != 3 || len(got.FailureClasses) != 1 {
		t.Fatalf("classes %v", got.FailureClasses)
	}
	if n := filterAnalytics(analyticsFixture(), "", "beta").Totals.SuccessfulDevices; n != 4 {
		t.Fatalf("channel filter: %d", n)
	}
}

func TestSuccessRate(t *testing.T) {
	if successRate(0, 0) != "" || successRate(9, 1) != "90.0%" || successRate(0, 3) != "0.0%" {
		t.Fatal("successRate")
	}
}

func TestRenderAnalyticsGroupsByReleaseAndNeverShowsAbsentAsZero(t *testing.T) {
	view := analyticsFixture()
	view.FailureClasses = map[string]int{"crash_before_first_frame": 3, "crash_after_launch": 1}
	var buf bytes.Buffer
	renderAnalytics(&buf, view)
	out := buf.String()
	lines := strings.Split(out, "\n")
	var order []string
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			f := strings.Fields(l)
			order = append(order, f[0]+" "+f[len(f)-1])
		}
	}
	want := []string{"#2 rel-a", "#1 rel-a", "#2 rel-b", "#1 rel-b"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("row order %v, want %v\n%s", order, want, out)
	}
	if !strings.Contains(out, "no reports yet") {
		t.Fatalf("an unobserved patch must say so:\n%s", out)
	}
	if !strings.Contains(out, "rolled back") {
		t.Fatalf("rollback state missing:\n%s", out)
	}
	// reasons sorted by count, then name
	i, j := strings.Index(out, "crash_before_first_frame"), strings.Index(out, "crash_after_launch")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("failure reasons not sorted by count:\n%s", out)
	}
}
