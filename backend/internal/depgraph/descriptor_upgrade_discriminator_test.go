package depgraph

// A package whose ONLY change is its pubspec hash or its dependency edges is still an upgrade by
// packageIdentityChanged. Before the Upgrade record carried those two discriminators, BuildDescriptor
// emitted an entry that looked like a no-op and DecodeStrict rejected the entire descriptor — so an
// ordinary `flutter pub get` that shifted one transitive edge blocked EVERY patch for the project.

import (
	"encoding/json"
	"strings"
	"testing"
)

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The strict decoder requires every graph digest field; these are fixtures, not the subject.
func withDigests(d *Descriptor) {
	d.BasePubspecLockSHA, d.CandidatePubspecLockSHA = hex64, hex64
	d.BasePackageConfigSHA, d.CandPackageConfigSHA = hex64, hex64
	d.BaseGraphDigest, d.CandidateGraphDigest = hex64, hex64
	d.DescriptorDigest = d.computeDigest()
}

func upgradeOnlyGraphs(mutate func(p *Package)) (Graph, Graph) {
	base := Graph{RootPackage: "app", PubspecLockSHA: hex64, PackageConfigSHA: hex64, GraphDigest: hex64,
		Packages: map[string]Package{
			"flutter": {Name: "flutter", Version: "0.0.0", Source: "sdk", PubspecSHA: "aaa",
				Dependencies: []string{"collection"}, Capability: Capability{Eligible: true}},
		}}
	cp := base.Packages["flutter"]
	cp.Dependencies = append([]string{}, cp.Dependencies...)
	mutate(&cp)
	cand := Graph{RootPackage: "app", PubspecLockSHA: hex64, PackageConfigSHA: hex64, GraphDigest: hex64,
		Packages: map[string]Package{"flutter": cp}}
	return base, cand
}

func TestUpgradeWithOnlyAPubspecSHAChangeSurvivesStrictDecode(t *testing.T) {
	base, cand := upgradeOnlyGraphs(func(p *Package) { p.PubspecSHA = "bbb" })
	d := BuildDescriptor(base, cand)
	if len(d.Upgraded) != 1 {
		t.Fatalf("expected flutter to be an upgrade, got %+v", d.Upgraded)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeStrict(raw); err != nil {
		t.Fatalf("a real pubspec-hash change was rejected as a no-op: %v", err)
	}
}

func TestUpgradeWithOnlyADependencyEdgeChangeSurvivesStrictDecode(t *testing.T) {
	base, cand := upgradeOnlyGraphs(func(p *Package) { p.Dependencies = []string{"collection", "meta"} })
	d := BuildDescriptor(base, cand)
	if len(d.Upgraded) != 1 {
		t.Fatalf("expected flutter to be an upgrade, got %+v", d.Upgraded)
	}
	raw, _ := json.Marshal(d)
	if _, err := DecodeStrict(raw); err != nil {
		t.Fatalf("a real dependency-edge change was rejected as a no-op: %v", err)
	}
}

// A genuine no-op must STILL be refused: the fix widens what counts as a change, it does not disable
// the check that keeps meaningless entries out of a security record.
func TestATrueNoOpUpgradeIsStillRejected(t *testing.T) {
	d := Descriptor{Schema: DescriptorSchema, GeneratorVersion: GeneratorVersion, RootPackage: "app",
		Upgraded: []Upgrade{{Name: "x", FromVer: "1.0.0", ToVer: "1.0.0"}}}
	withDigests(&d)
	raw, _ := json.Marshal(d)
	_, err := DecodeStrict(raw)
	if err == nil || !strings.Contains(err.Error(), "nothing about it changed") {
		t.Fatalf("a genuine no-op upgrade was accepted: %v", err)
	}
}
