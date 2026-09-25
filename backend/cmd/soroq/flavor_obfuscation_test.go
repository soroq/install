package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Flavor support and Android Option A were built on separate branches; this is the combination. A
// flavored, obfuscated release must build with BOTH the map-capture option and the flavor, discover only
// that flavor's artifact, and record the flavor and the 0600 base map under the same (flavor-qualified)
// release id -- the id every seeded patch for that flavor later loads its map from.
func TestFlavoredObfuscatedReleaseRecordsFlavorAndMapUnderOneRelease(t *testing.T) {
	dir := newFlavorReleaseProject(t)
	installSeedTestToolchain(t, []string{androidObfuscationSeedCapability}, "--load-obfuscation-map")
	plane := newFakeReleasePlane(t)

	prodAAB := filepath.Join(dir, "build", "app", "outputs", "bundle", "prodRelease", "app-prod-release.aab")
	var buildArgs []string
	stubAndroidReleaseBuild(t, func(args []string) error {
		buildArgs = args
		const opt = "--extra-gen-snapshot-options=--save-obfuscation-map="
		for _, a := range args {
			if strings.HasPrefix(a, opt) {
				writeMap(t, strings.TrimPrefix(a, opt), "", "", "Foo", "a", "bar", "b")
			}
		}
		writeFlavorTestArtifact(t, prodAAB, "runtime-prod")
		return nil
	})
	args := []string{"--project-dir", dir, "--api", plane.server.URL, "--arch", "arm64-v8a",
		"--toolchain", seedTestToolchain, "--flavor", "prod", "--"}
	if err := runReleaseAndroid(append(args, seedObfArgs...)); err != nil {
		t.Fatalf("flavored obfuscated release: %v", err)
	}

	joined := strings.Join(buildArgs, " ")
	for _, want := range []string{"--obfuscate", "--split-debug-info=build/symbols", "--save-obfuscation-map=", "--flavor prod"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("flutter build args %q lack %q", joined, want)
		}
	}
	if len(plane.registered) != 1 || plane.registered[0].RuntimeID != "runtime-prod" {
		t.Fatalf("registered %+v; must be exactly the prod artifact", plane.registered)
	}
	id := plane.registered[0].ID
	if f, known, err := knownReleaseFlavor(dir, "android", id); err != nil || !known || f != "prod" {
		t.Fatalf("recorded flavor = %q known=%v err=%v", f, known, err)
	}
	rec, mapPath, err := loadAndroidReleaseObfuscation(dir, id)
	if err != nil || rec == nil || rec.MapEntries != 3 {
		t.Fatalf("obfuscation record for %s: %+v %v", id, rec, err)
	}
	if info, err := os.Stat(mapPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("base map %s: %v %v", mapPath, info, err)
	}
}
