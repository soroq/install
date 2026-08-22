package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// ABI selection decides which architecture a release -- and therefore every patch diffed against it --
// is bound to. Getting it silently wrong means devices on the other ABIs sit on a release whose
// patches were built for an architecture they do not run.

func TestSingleABIArtifactResolvesWithoutCeremony(t *testing.T) {
	got, err := resolveReleaseArchForArtifact("apk", []string{"arm64-v8a"}, "")
	if err != nil || got != "arm64-v8a" {
		t.Fatalf("a single-ABI artifact must resolve to that ABI, got %q err=%v", got, err)
	}
}

// A fat APK still defaults to arm64 -- that default is deliberate and right for most shipped Flutter
// apps -- but it must no longer happen SILENTLY. Every ABI inside one APK shares a single runtime_id,
// and patch selection keys on runtime id rather than architecture, so devices on the other ABIs are
// offered code built for the chosen one. The developer has to be told.
func TestMultiABIAPKKeepsArm64DefaultButWarns(t *testing.T) {
	stderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	got, err := resolveReleaseArchForArtifact("apk", []string{"arm64-v8a", "armeabi-v7a"}, "")
	w.Close()
	os.Stderr = stderr
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	warning := buf.String()

	if err != nil || got != "arm64-v8a" {
		t.Fatalf("the deliberate arm64 default must be preserved, got %q err=%v", got, err)
	}
	for _, want := range []string{"armeabi-v7a", "--arch", "aab"} {
		if !strings.Contains(warning, want) {
			t.Errorf("the multi-ABI warning should mention %q:\n%s", want, warning)
		}
	}
	if warning == "" {
		t.Fatal("a multi-ABI APK must not be resolved silently")
	}
}

// Naming the ABI explicitly is the supported path and must work.
func TestMultiABIAPKAcceptsAnExplicitArch(t *testing.T) {
	got, err := resolveReleaseArchForArtifact("apk", []string{"arm64-v8a", "armeabi-v7a"}, "armeabi-v7a")
	if err != nil || got != "armeabi-v7a" {
		t.Fatalf("an explicit --arch must be honoured, got %q err=%v", got, err)
	}
}

// An --arch the artifact does not contain must stay a hard refusal.
func TestArchNotPresentInArtifactIsRefused(t *testing.T) {
	_, err := resolveReleaseArchForArtifact("apk", []string{"arm64-v8a"}, "x86_64")
	if err == nil {
		t.Fatal("requesting an ABI the artifact does not contain must fail")
	}
}

// An AAB keeps universal selection: Play splits per-ABI at delivery, so the bundle legitimately covers
// all of them. This is asserted so a future change cannot quietly alter AAB behaviour while fixing APKs.
func TestMultiABIAABStaysUniversal(t *testing.T) {
	got, err := resolveReleaseArchForArtifact("aab", []string{"arm64-v8a", "armeabi-v7a"}, "")
	if err != nil || got != "universal" {
		t.Fatalf("a multi-ABI AAB must stay universal, got %q err=%v", got, err)
	}
}

func TestUnknownABIsStillAskForArch(t *testing.T) {
	_, err := resolveReleaseArchForArtifact("apk", nil, "")
	if err == nil || !strings.Contains(err.Error(), "--arch") {
		t.Fatalf("an artifact with no detectable ABI must ask for --arch, got %v", err)
	}
}
