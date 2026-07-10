package main

import (
	"reflect"
	"testing"
)

func TestPatchIOSEngineRequested(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"config default", []string{"--config-file", "c.json"}, false},
		{"explicit engine", []string{"--engine"}, true},
		{"toolchain flag", []string{"--toolchain", "soroq-ios-3.44.2-r3"}, true},
		{"toolchain equals", []string{"--toolchain=soroq-ios-r3"}, true},
		{"engine + toolchain", []string{"--engine", "--toolchain", "x"}, true},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		if got := patchIOSEngineRequested(tc.args); got != tc.want {
			t.Errorf("%s: patchIOSEngineRequested(%v) = %v, want %v", tc.name, tc.args, got, tc.want)
		}
	}
}

func TestReleaseIOSEngineRequested(t *testing.T) {
	// --toolchain must NOT route release to the engine delegate (it belongs to --build).
	if releaseIOSEngineRequested([]string{"--build", "--toolchain", "x"}) {
		t.Error("release ios --build --toolchain must stay the build leg, not route to the engine delegate")
	}
	if !releaseIOSEngineRequested([]string{"--engine", "--app-dill", "a.dill"}) {
		t.Error("release ios --engine must route to the engine delegate")
	}
}

func TestStripEngineRoutingFlag(t *testing.T) {
	got := stripEngineRoutingFlag([]string{"--engine", "--toolchain", "r3", "--version", "1"})
	want := []string{"--toolchain", "r3", "--version", "1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stripEngineRoutingFlag dropped/kept wrong flags: got %v want %v", got, want)
	}
}
