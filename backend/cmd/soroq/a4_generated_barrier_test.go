package main

import (
	"strings"
	"testing"
)

// A4 — THE GENERATED BOOTSTRAP MUST USE THE PRODUCTION RASTER BARRIER.
//
// bootstrap.dart exposes `frameBarrier` for exactly one reason, stated in its own doc comment:
// Flutter's automated test binding never completes `waitUntilFirstFrameRasterized`, so a widget test
// that awaits it hangs forever. Tests inject an analytic future instead.
//
// That escape hatch is safe only while it stays in tests. If the GENERATOR ever emitted a
// `frameBarrier:` argument, every real device would silently run the analytic path — and it would not
// announce itself: the ordering would still look right, the phase record would still be produced, and
// the only visible difference would be a phase named for the injected barrier rather than the
// rasterized one. That is precisely the failure the A4 verdict downgraded a claim over, so it is worth
// a standing assertion rather than a comment.
//
// Host-verifiable, and deliberately narrow: this proves the production wiring SELECTS the real
// barrier. It does not prove a frame rasterized — that needs a device, and the A4 device gate is
// where that claim belongs.

// a4GeneratedBootstrap renders the generated bootstrap for a given ordering.
func a4GeneratedBootstrap(activateFirst bool, launchColor string) string {
	return freehandBootstrapSource(freehandBootstrapConfig{
		ActivateBeforeDeveloperMain: activateFirst,
		LaunchColorARGB:             launchColor,
		AppID:                       "com.example.app",
		RuntimeID:                   strings.Repeat("a", 64),
		Channel:                     "stable",
		ControlPlaneBaseURL:         "https://api.example.test",
		PinnedEnginePubKeyHex:       strings.Repeat("b", 64),
		EntrypointImport:            "package:example_app/main.dart",
	})
}

func TestGeneratedBootstrapNeverInjectsATestBarrier(t *testing.T) {
	for name, src := range map[string]string{
		"default ordering":                    a4GeneratedBootstrap(false, ""),
		"activate-before-main":                a4GeneratedBootstrap(true, ""),
		"activate-before-main, launch colour": a4GeneratedBootstrap(true, "0xFF102030"),
	} {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(src, "frameBarrier") {
				t.Fatalf("the generated bootstrap passes frameBarrier, which is the TEST-ONLY escape " +
					"hatch. On a device that silently substitutes an analytic barrier for the real " +
					"rasterization wait, and nothing about the resulting behaviour looks wrong.")
			}
			// The generator also must not hand-roll its own wait; the ordering helper owns the barrier.
			if strings.Contains(src, "waitUntilFirstFrameRasterized") {
				t.Error("the generated bootstrap should not reference the barrier directly; " +
					"soroqRunAppAfterRestoreActivation owns it, so there is one place to get it right")
			}
		})
	}
}

// The opt-in ordering must remain opt-in. It is a change to initialization SEMANTICS and the A4
// verdict keeps it default-OFF until a physical iPhone cold start proves it; a default flip would
// change every user's startup path with no device evidence behind it.
func TestActivateBeforeMainStaysOptInInGeneratedOutput(t *testing.T) {
	def := a4GeneratedBootstrap(false, "")
	if !strings.Contains(def, "app.main();") {
		t.Fatal("the DEFAULT generated ordering must call app.main() directly")
	}
	if strings.Contains(def, "soroqRunAppAfterRestoreActivation") {
		t.Fatal("the default ordering must NOT use the activate-before-main helper; that path is " +
			"opt-in until the physical-device gate closes")
	}

	optIn := a4GeneratedBootstrap(true, "")
	if !strings.Contains(optIn, "soroqRunAppAfterRestoreActivation") {
		t.Fatal("the opt-in ordering must use the activate-before-main helper")
	}
}

// A declared launch colour must reach the generated handoff frame, and an undeclared one must leave
// the package fallback in charge rather than baking in a guess.
func TestGeneratedLaunchColourIsBakedOnlyWhenDeclared(t *testing.T) {
	withColor := a4GeneratedBootstrap(true, "0xFF102030")
	if !strings.Contains(withColor, "SoroqHandoffFrame(color: Color(0xFF102030))") {
		t.Errorf("a declared launch colour must reach the handoff frame; generated:\n%s", withColor)
	}
	without := a4GeneratedBootstrap(true, "")
	if strings.Contains(without, "SoroqHandoffFrame(color:") {
		t.Error("with no declared colour the generator must omit the argument so the package fallback " +
			"applies, rather than baking in a colour nobody chose")
	}
}
