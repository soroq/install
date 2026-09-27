package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUnflavoredIOSEngineReleaseUsesTheProjectChannel(t *testing.T) {
	dir := t.TempDir()
	write := func(yaml string) {
		if err := os.WriteFile(filepath.Join(dir, "soroq.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app_id: dev.example.app\nchannel: analytics-ios\n")
	if got := withProjectReleaseChannel([]string{"--toolchain", "tc"}, dir); !reflect.DeepEqual(got, []string{"--toolchain", "tc", "--channel", "analytics-ios"}) {
		t.Fatalf("soroq.yaml channel not passed: %v", got)
	}
	explicit := []string{"--channel", "beta"}
	if got := withProjectReleaseChannel(explicit, dir); !reflect.DeepEqual(got, explicit) {
		t.Fatalf("an explicit --channel must win: %v", got)
	}
	write("app_id: dev.example.app\n")
	if got := withProjectReleaseChannel([]string{"--toolchain", "tc"}, dir); len(got) != 2 {
		t.Fatalf("no channel in soroq.yaml: args must be unchanged, got %v", got)
	}
}
