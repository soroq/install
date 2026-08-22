package main

import (
	"strings"
	"testing"
)

// The Judge chose explicit launch-colour configuration, but a decision the zero-touch path cannot
// deliver is not a decision. These prove a developer can declare the colour in soroq.yaml and have it
// reach the generated bootstrap WITHOUT editing or importing anything in main.dart.

func TestLaunchColourAcceptsTheDocumentedForms(t *testing.T) {
	for raw, want := range map[string]string{
		"#102030":    "0xFF102030",
		"102030":     "0xFF102030",
		"0x102030":   "0xFF102030",
		"#CC102030":  "0xCC102030",
		"0XCC102030": "0xCC102030",
		"":           "",
	} {
		got, err := parseSoroqLaunchColor(raw)
		if err != nil {
			t.Errorf("launch_color %q must be accepted: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("launch_color %q -> %q, want %q", raw, got, want)
		}
	}
}

// Malformed values must fail at GENERATION, not compile into Dart as a wrong colour or a syntax error.
func TestLaunchColourRefusesMalformedAndTransparentValues(t *testing.T) {
	for raw, wantSubstring := range map[string]string{
		"red":       "not a hex colour",
		"#12345":    "not a hex colour",
		"#GGGGGG":   "not a hex colour",
		"#00FFFFFF": "fully transparent",
	} {
		if _, err := parseSoroqLaunchColor(raw); err == nil {
			t.Errorf("launch_color %q must be refused", raw)
		} else if !strings.Contains(err.Error(), wantSubstring) {
			t.Errorf("launch_color %q: refusal should mention %q, got %v", raw, wantSubstring, err)
		}
	}
}

func TestGeneratedBootstrapBakesTheDeclaredLaunchColour(t *testing.T) {
	cfg := freehandBootstrapConfig{
		AppID: "com.example.app", RuntimeID: "runtime-1", Channel: "stable",
		ControlPlaneBaseURL: "https://api.example.test", PinnedEnginePubKeyHex: strings.Repeat("a", 64),
		EntrypointImport:            "package:example/main.dart",
		ActivateBeforeDeveloperMain: true,
		LaunchColorARGB:             "0xFF102030",
	}
	src := freehandBootstrapSource(cfg)
	if !strings.Contains(src, "SoroqHandoffFrame(color: Color(0xFF102030))") {
		t.Fatalf("the declared launch colour must be baked into the bootstrap:\n%s", src)
	}
	// Zero-touch contract: nothing about this requires customer code changes.
	if strings.Contains(src, "// TODO") || strings.Contains(src, "edit your main.dart") {
		t.Error("the launch colour must not require developer edits")
	}
}

func TestGeneratedBootstrapOmitsTheArgumentWhenUndeclared(t *testing.T) {
	cfg := freehandBootstrapConfig{
		AppID: "com.example.app", RuntimeID: "runtime-1", Channel: "stable",
		ControlPlaneBaseURL: "https://api.example.test", PinnedEnginePubKeyHex: strings.Repeat("a", 64),
		EntrypointImport:            "package:example/main.dart",
		ActivateBeforeDeveloperMain: true,
	}
	src := freehandBootstrapSource(cfg)
	if strings.Contains(src, "SoroqHandoffFrame(color:") {
		t.Fatal("with no declared colour the bootstrap must omit the argument so the package fallback applies")
	}
	if !strings.Contains(src, "soroqRunAppAfterRestoreActivation(c, app.main)") {
		t.Fatalf("expected the bare helper call:\n%s", src)
	}
}
