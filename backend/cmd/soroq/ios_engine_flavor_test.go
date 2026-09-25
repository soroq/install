package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantIOSEngineFlavorRefusal is the refusal an UNDECLARED flavor gets on each iOS engine lane: the
// scaffolded lane does not support flavors at all; the freehand lane supports them only with a declared
// per-flavor channel. Both refuse before touching the project.
func wantIOSEngineFlavorRefusal(lane string) string {
	if lane == "scaffolded" {
		return "flavored builds are not supported on this route"
	}
	return "an iOS flavor needs its own channel"
}

const engineFreehandFlavorsConfig = engineFreehandConfig + "flavors:\n  prod:\n    channel: ios-prod\n  dev:\n    channel: ios-dev\n"

// A declared flavor on the freehand lane: the freehand release runs with soroq.yaml on the flavor's
// channel (bootstrap, runtime id and baseline all derive from it), Flutter gets --flavor, the soroqctl
// delegate gets --channel and never --flavor, and the project is byte-identical afterwards.
func TestIOSEngineFreehandReleaseRunsUnderTheFlavorChannel(t *testing.T) {
	for name, extra := range map[string][]string{
		"flag":        {"--flavor", "prod"},
		"passthrough": {"--", "--flavor=prod"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newEngineLaneProjectWithConfig(t, engineOrdinaryPubspec, engineFreehandFlavorsConfig)
			before := p.snapshot(t)
			var gotHead, gotPass []string
			var channelDuring string
			engineLaneFreehandFn = func(head, pass []string, dir, _ string) error {
				gotHead, gotPass = head, pass
				b, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml"))
				channelDuring = parseTopLevelYaml(b)["channel"]
				return nil
			}
			if err := runEngineLaneBuild(p, extra...); err != nil {
				t.Fatalf("flavored freehand release: %v", err)
			}
			if channelDuring != "ios-prod" {
				t.Fatalf("freehand release saw channel %q, want ios-prod", channelDuring)
			}
			if hasFlag(gotHead, "flavor") {
				t.Fatalf("--flavor leaked to the soroqctl delegate args: %v", gotHead)
			}
			if v, _ := flagValue(gotHead, "channel"); v != "ios-prod" {
				t.Fatalf("delegate --channel = %q, want ios-prod (%v)", v, gotHead)
			}
			if freehandBuildFlavor(gotPass) != "prod" {
				t.Fatalf("Flutter passthrough lacks --flavor prod: %v", gotPass)
			}
			p.assertUntouched(t, before)
		})
	}
}

func TestIOSEngineFlavorRouteRefusals(t *testing.T) {
	p := newEngineLaneProjectWithConfig(t, engineOrdinaryPubspec, engineFreehandFlavorsConfig)
	before := p.snapshot(t)
	called := false
	engineLaneFreehandFn = func([]string, []string, string, string) error { called = true; return nil }
	err := runEngineLaneBuild(p, "--channel", "stable", "--", "--flavor", "prod")
	if err == nil || !strings.Contains(err.Error(), "conflicts with soroq.yaml flavors.prod.channel") || called {
		t.Fatalf("conflicting --channel: err=%v called=%v", err, called)
	}
	err = runEngineLaneBuild(p, "--", "--flavor", "staging")
	if err == nil || !strings.Contains(err.Error(), "an iOS flavor needs its own channel") || called {
		t.Fatalf("undeclared flavor: err=%v called=%v", err, called)
	}
	p.assertUntouched(t, before)
}

// pubspec `flutter: default-flavor:` makes a build flavored without --flavor; the lane must treat it so.
func TestIOSEngineFreehandHonoursPubspecDefaultFlavor(t *testing.T) {
	p := newEngineLaneProjectWithConfig(t, engineOrdinaryPubspec+"flutter:\n  default-flavor: dev\n", engineFreehandFlavorsConfig)
	var channelDuring string
	var gotPass []string
	engineLaneFreehandFn = func(_ []string, pass []string, dir, _ string) error {
		gotPass = pass
		b, _ := os.ReadFile(filepath.Join(dir, "soroq.yaml"))
		channelDuring = parseTopLevelYaml(b)["channel"]
		return nil
	}
	if err := runEngineLaneBuild(p); err != nil {
		t.Fatal(err)
	}
	if channelDuring != "ios-dev" || freehandBuildFlavor(gotPass) != "dev" {
		t.Fatalf("default-flavor: channel %q passthrough %v", channelDuring, gotPass)
	}
}

func fakeFlutterRootForRecipe(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{
		"bin/cache/artifacts/engine/common/flutter_patched_sdk_product/platform_strong.dill",
		"bin/cache/dart-sdk/bin/snapshots/gen_kernel_aot.dart.snapshot",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, rel), "bytes of "+rel)
	}
	return root
}

// The recipe carries the flavor and Flutter's FLUTTER_APP_FLAVOR define, and its digest separates
// flavors -- so a candidate for another flavor's base fails reproduction, with a plain message first.
func TestFreehandRecipeBindsTheFlavor(t *testing.T) {
	root := fakeFlutterRootForRecipe(t)
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".dart_tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(proj, ".dart_tool", "package_config.json"), `{"configVersion":2,"packages":[]}`)
	digest := map[string]string{}
	for _, f := range []string{"", "prod", "dev"} {
		r, err := buildFreehandSourceKernelRecipe(proj, root, f)
		if err != nil {
			t.Fatal(err)
		}
		if r.Flavor != f || strings.Join(r.DartDefines, ",") != strings.Join(flavorDartDefines(f), ",") {
			t.Fatalf("recipe for %q: flavor %q defines %v", f, r.Flavor, r.DartDefines)
		}
		if f == "prod" && strings.Join(r.DartDefines, ",") != "FLUTTER_APP_FLAVOR=prod" {
			t.Fatalf("prod defines %v", r.DartDefines)
		}
		d, _ := r.recipeDigest()
		digest[f] = d
	}
	if digest[""] == digest["prod"] || digest["prod"] == digest["dev"] {
		t.Fatalf("recipe digests must separate flavors: %v", digest)
	}
	base, _ := buildFreehandSourceKernelRecipe(proj, root, "prod")
	if err := assertRecipeReproducible(proj, root, base, "prod"); err != nil {
		t.Fatalf("same flavor must reproduce: %v", err)
	}
	err := assertRecipeReproducible(proj, root, base, "dev")
	if err == nil || !strings.Contains(err.Error(), `built as flavor "prod", the patch as flavor "dev"`) {
		t.Fatalf("flavor mismatch: %v", err)
	}
	if err := assertRecipeReproducible(proj, root, base, ""); err == nil {
		t.Fatal("an unflavored patch for a flavored base must be refused")
	}
}

// The identity asset goes only into THIS flavor's bundles: Flutter's product copy (build/ios/iphoneos)
// and the flavor's own configuration dir -- never another flavor's leftover.
func TestFreehandIdentityStampsOnlyTheFlavorsBundles(t *testing.T) {
	proj := t.TempDir()
	for _, d := range []string{"iphoneos", "Profile-Prod-iphoneos", "Release-prod-iphoneos", "Profile-dev-iphoneos", "Profile-production-iphoneos"} {
		if err := os.MkdirAll(filepath.Join(proj, "build", "ios", d, "Runner.app"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := findIOSAppBundles(proj, "prod")
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, g := range got {
		r, _ := filepath.Rel(filepath.Join(proj, "build", "ios"), g)
		rels = append(rels, filepath.ToSlash(r))
	}
	want := "Profile-Prod-iphoneos/Runner.app,Release-prod-iphoneos/Runner.app,iphoneos/Runner.app"
	if strings.Join(rels, ",") != want {
		t.Fatalf("prod bundles = %v, want %s", rels, want)
	}
	all, _ := findIOSAppBundles(proj, "")
	if len(all) != 5 {
		t.Fatalf("unflavored must keep stamping every bundle: %d", len(all))
	}
}
