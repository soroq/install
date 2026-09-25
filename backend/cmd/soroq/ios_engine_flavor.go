package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// iOS ENGINE-LANE FLAVORS (freehand lane only).
//
// What a flavored Flutter iOS build changes, and how the lane accounts for each:
//
//   - FLUTTER_APP_FLAVOR=<flavor> is appended to the Dart defines of the compiled app
//     (flutter_tools build_system/targets/common.dart). The source-kernel recipe records the flavor and
//     that define, so the source-fidelity kernel -- and every candidate compiled from the recipe --
//     sees the same `appFlavor`. The recipe digest binds both, so a patch for another flavor's base is
//     refused at recipe reproduction.
//   - Xcode builds scheme sentenceCase(flavor), configuration <Mode>-<scheme>, into
//     build/ios/<Config>-iphoneos/, then copies the product to build/ios/iphoneos/. The base identity is
//     written only into THOSE bundles, never into a leftover bundle of another flavor.
//   - The device's app_id/channel/runtime_id are baked into the generated bootstrap from soroq.yaml, and
//     serving is keyed by them. Two flavors on one channel would share a runtime_id, so an iOS flavor must
//     be DECLARED in soroq.yaml `flavors:` with its own channel (flavor_channel.go); the whole freehand
//     release/patch then runs under that channel. An undeclared flavor is refused.
//
// The scaffolded (hand-listed `ios_engine.patchable`) lane keeps refusing flavors: none of the above is
// wired there.

const iosScaffoldedFlavorRefusalReason = `Flavors are supported on the freehand iOS engine lane (no ios_engine.patchable list), where the
source-kernel recipe, the identity asset and the channel are flavor-aware. The scaffolded lane
(hand-listed ios_engine.patchable) is not.`

type iosEngineFlavorRoute struct {
	flavor      string
	channel     string // the declared flavor channel; "" when unflavored
	head        []string
	passthrough []string
}

// resolveIOSEngineFlavorRoute runs before anything mutates the project. It always repairs an interrupted
// flavor-channel swap first: an unflavored build over a swapped soroq.yaml would silently use a flavor's
// channel.
func resolveIOSEngineFlavorRoute(route, projectDir string, head, passthrough []string) (iosEngineFlavorRoute, error) {
	out := iosEngineFlavorRoute{head: head, passthrough: passthrough}
	if err := recoverInterruptedFlavorChannelSwap(projectDir); err != nil {
		return out, err
	}
	flavor, err := flavorFromRouteArgs(projectDir, head, passthrough)
	if err != nil || flavor == "" {
		return out, err
	}
	freehand, err := isFreehandIOSBuild(projectDir)
	if err != nil {
		return out, err
	}
	if !freehand {
		return out, guardUnsupportedFlavorRoute(route, iosScaffoldedFlavorRefusalReason, flavor)
	}
	ch, declared, err := resolveFlavorChannel(projectDir, flavor)
	if err != nil {
		return out, err
	}
	if !declared {
		return out, fmt.Errorf(`refusing --flavor %s on %s: an iOS flavor needs its own channel.

The device's runtime identity is derived from soroq.yaml's app_id + channel + version, so two flavors
on one channel would receive each other's patches. Declare it in soroq.yaml, e.g.

  flavors:
    %s:
      channel: <a channel only this flavor uses>`, flavor, route, flavor)
	}
	if v, ok := flagValue(head, "channel"); ok && strings.TrimSpace(v) != ch {
		return out, fmt.Errorf("--channel %q conflicts with soroq.yaml flavors.%s.channel %q; omit --channel for a declared flavor", strings.TrimSpace(v), flavor, ch)
	}
	// The flavor goes to Flutter (passthrough), never to the soroqctl delegate (head).
	flagVal, _ := flagValue(head, "flavor")
	_, rest, err := resolveCommandFlavor(projectDir, flagVal, passthrough)
	if err != nil {
		return out, err
	}
	out.head = stripFlag(head, "flavor", false)
	if !hasFlag(out.head, "channel") {
		out.head = append(out.head, "--channel", ch)
	}
	out.passthrough = buildArgsWithFlavor(rest, flavor)
	out.flavor, out.channel = flavor, ch
	return out, nil
}

// freehandBuildFlavor is the flavor a (normalized) freehand passthrough builds with.
func freehandBuildFlavor(passthrough []string) string {
	v, _ := flagValue(passthrough, "flavor")
	return strings.TrimSpace(v)
}

// iosBundleBelongsToFlavor reports whether the .app at bundlePath is output of a build of flavor:
// build/ios/iphoneos/ (Flutter's copy of the product just built) or build/ios/<Mode>-<scheme>-iphoneos/
// for this flavor's scheme. Unflavored => every bundle, as before.
func iosBundleBelongsToFlavor(projectDir, bundlePath, flavor string) bool {
	if flavor == "" {
		return true
	}
	rel, err := filepath.Rel(filepath.Join(projectDir, "build", "ios"), bundlePath)
	if err != nil {
		return false
	}
	dir := strings.ToLower(strings.Split(filepath.ToSlash(rel), "/")[0])
	if dir == "iphoneos" {
		return true
	}
	return strings.HasSuffix(dir, "-"+strings.ToLower(flavor)+"-iphoneos")
}

func flavorDartDefines(flavor string) []string {
	if flavor == "" {
		return []string{}
	}
	return []string{"FLUTTER_APP_FLAVOR=" + flavor}
}
