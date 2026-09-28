package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

func devicesFixture() analyticsView {
	v := analyticsFixture()
	seen := time.Date(2026, 9, 28, 9, 30, 0, 0, time.UTC)
	v.Devices = []analyticsDeviceRow{
		{ReleaseID: "rel-a", Version: "1.0.41+57", RuntimeID: "a1b2c3d4e5f60718293a4b5c6d7e8f90", Channel: "stable", Devices: 120, Active24h: 80, Active7d: 110, LastSeen: seen},
		{ReleaseID: "rel-b", ReleaseIDs: []string{"rel-b", "rel-b-ios"}, Version: "1.0.40+56", RuntimeID: "ffff", Channel: "beta", Devices: 3, Active24h: 0, Active7d: 1, LastSeen: seen},
		{RuntimeID: "0000", Channel: "stable", Devices: 1, Active24h: 1, Active7d: 1, LastSeen: seen},
	}
	return v
}

func TestRenderAnalyticsShowsDevicesPerRelease(t *testing.T) {
	var buf bytes.Buffer
	renderAnalytics(&buf, devicesFixture())
	out := buf.String()
	for _, want := range []string{
		"devices per release",
		"RELEASE", "ACTIVE 24H", "ACTIVE 7D",
		"rel-a", "1.0.41+57", "120", "80", "110", "2026-09-28 09:30", "a1b2c3d4e5f6\n",
		"rel-b (+1)", "(unregistered)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "a1b2c3d4e5f60718") {
		t.Fatalf("runtime id should be shortened:\n%s", out)
	}
}

func TestRenderAnalyticsDevicesStatesAndOlderServers(t *testing.T) {
	// An older server sends no devices field: no section at all.
	var old bytes.Buffer
	renderAnalytics(&old, analyticsFixture())
	if strings.Contains(old.String(), "devices per release") {
		t.Fatalf("older server output grew a devices section:\n%s", old.String())
	}
	// No devices yet, and no patches either: the section still renders.
	var empty bytes.Buffer
	v := analyticsView{AppID: "com.example.app", Devices: []analyticsDeviceRow{}}
	renderAnalytics(&empty, v)
	if !strings.Contains(empty.String(), "no devices have checked in yet") {
		t.Fatalf("empty devices state:\n%s", empty.String())
	}
	var unavailable bytes.Buffer
	renderAnalytics(&unavailable, analyticsView{AppID: "x", DevicesUnavailable: "device counts could not be read right now"})
	if !strings.Contains(unavailable.String(), "devices per release: unavailable") {
		t.Fatalf("unavailable state:\n%s", unavailable.String())
	}
}

func TestFilterAnalyticsFiltersDevices(t *testing.T) {
	if got := filterAnalytics(devicesFixture(), "", "beta").Devices; len(got) != 1 || got[0].ReleaseID != "rel-b" {
		t.Fatalf("channel filter: %+v", got)
	}
	// A release sharing a runtime with the labelled one still matches.
	if got := filterAnalytics(devicesFixture(), "rel-b-ios", "").Devices; len(got) != 1 || got[0].ReleaseID != "rel-b" {
		t.Fatalf("release filter: %+v", got)
	}
	if got := filterAnalytics(devicesFixture(), "rel-none", "").Devices; got == nil || len(got) != 0 {
		t.Fatalf("filtered-away devices should be empty, not absent: %#v", got)
	}
}

func TestAnalyticsJSONCarriesDevices(t *testing.T) {
	raw := []byte(`{"app_id":"a","totals":{},"patches":[],"devices":[{"release_id":"r","version":"1.0","runtime_id":"rt","channel":"stable","devices":2,"active_24h":1,"active_7d":2,"first_seen":"2026-09-01T00:00:00Z","last_seen":"2026-09-28T00:00:00Z"}]}`)
	var v analyticsView
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"devices":2`, `"active_24h":1`, `"active_7d":2`, `"release_id":"r"`, `"last_seen":"2026-09-28T00:00:00Z"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("--json lost %s: %s", want, out)
		}
	}
}
