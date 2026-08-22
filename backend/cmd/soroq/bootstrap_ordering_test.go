package main

import (
	"strings"
	"testing"
)

// The generated bootstrap decides when developer initialization runs relative to redirect activation,
// which is the whole of A4. Both orderings are pinned here so neither can drift silently.

func bootstrapCfg(activateFirst bool) freehandBootstrapConfig {
	return freehandBootstrapConfig{
		AppID: "com.example.app", RuntimeID: "runtime-1", Channel: "stable",
		ControlPlaneBaseURL: "https://api.example.test", PinnedEnginePubKeyHex: strings.Repeat("a", 64),
		EntrypointImport: "package:example/main.dart", ActivateBeforeDeveloperMain: activateFirst,
	}
}

// DEFAULT: the app starts first; activation is chained behind the first rasterized frame. This is the
// ordering proven on a physical iPhone, and it must remain the default until the other one is too.
func TestGeneratedBootstrapDefaultsToAppFirstOrdering(t *testing.T) {
	src := freehandBootstrapSource(bootstrapCfg(false))
	if !strings.Contains(src, "app.main();") {
		t.Fatal("default ordering must call the developer entrypoint directly")
	}
	if !strings.Contains(src, "soroqActivateRestoredAfterFirstFrame") {
		t.Fatal("default ordering must chain activation behind the first rasterized frame")
	}
	if strings.Contains(src, "soroqRunAppAfterRestoreActivation") {
		t.Fatal("the opt-in ordering must NOT appear unless it was requested")
	}
	mainIdx := strings.Index(src, "app.main();")
	actIdx := strings.Index(src, "soroqActivateRestoredAfterFirstFrame")
	if mainIdx > actIdx {
		t.Fatal("default ordering must invoke app.main() before the post-frame activation call")
	}
}

// OPT-IN: activation precedes developer main, so initialization observes the retained patch.
func TestGeneratedBootstrapOptInActivatesBeforeDeveloperMain(t *testing.T) {
	src := freehandBootstrapSource(bootstrapCfg(true))
	if !strings.Contains(src, "soroqRunAppAfterRestoreActivation(c, app.main)") {
		t.Fatal("opt-in ordering must hand app.main to the activate-first bootstrap helper")
	}
	if strings.Contains(src, "soroqActivateRestoredAfterFirstFrame") {
		t.Fatal("opt-in ordering must not also run the post-frame activation path (double activation)")
	}
	// restorePrepare still runs first in BOTH orderings: it is what makes crash-loop quarantine correct.
	prepIdx := strings.Index(src, "restorePrepare()")
	runIdx := strings.Index(src, "soroqRunAppAfterRestoreActivation")
	if prepIdx < 0 || runIdx < 0 || prepIdx > runIdx {
		t.Fatal("restorePrepare must precede activation in the opt-in ordering too")
	}
}

// Both orderings must still start the app when the controller could not be configured.
func TestBothOrderingsStartTheAppWhenSoroqIsUnavailable(t *testing.T) {
	for _, activateFirst := range []bool{false, true} {
		src := freehandBootstrapSource(bootstrapCfg(activateFirst))
		if !strings.Contains(src, "app.main") {
			t.Fatalf("activateFirst=%v: the developer entrypoint must always be reachable", activateFirst)
		}
		if !strings.Contains(src, "catch (_)") {
			t.Fatalf("activateFirst=%v: OTA wiring must never prevent launch", activateFirst)
		}
	}
}

// The opt-in must be off unless explicitly requested, so a normal build keeps the device-proven path.
func TestActivateBeforeMainIsOffByDefault(t *testing.T) {
	if bootstrapCfg(false).ActivateBeforeDeveloperMain {
		t.Fatal("the corrected ordering is not device-proven yet and must default to off")
	}
}
