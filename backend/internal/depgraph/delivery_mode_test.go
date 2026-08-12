package depgraph

// DELIVERY MODE: classification, and resistance to a FULLY REBOUND tamper.
//
// The important cases here are not the ones a hash catches. An attacker who edits a delivery mode and
// then recomputes the descriptor digest (and every outer hash, and the artifact id) produces a document
// whose hashes all agree with each other. Only re-deciding the modes from the delta the descriptor
// itself declares can reject that, which is what validateDelivery does and what these tests pin.

import (
	"strings"
	"testing"
)

func pkg(name, ver string, eligible bool, opts ...func(*Package)) Package {
	p := Package{
		Name: name, Version: ver, Source: SourceHosted,
		SourceID: "hosted:pub.dev", ContentHash: strings.Repeat("a", 64),
		Capability: Capability{Eligible: eligible},
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

func nativePlugin(name, ver string) Package {
	return pkg(name, ver, false, func(p *Package) {
		p.Capability = Capability{
			Eligible: false, HasNativePlugin: true,
			NativeDetail: "ios.pluginClass",
			Reasons:      []string{"declares native platform plugin code (ios.pluginClass)"},
		}
	})
}

// graphOf builds a graph with the identity fields Validate requires, so tests exercise the REAL
// strict validator rather than a relaxed path.
func graphOf(root string, pkgs ...Package) Graph {
	g := Graph{
		RootPackage:      root,
		Packages:         map[string]Package{},
		PubspecLockSHA:   strings.Repeat("1", 64),
		PackageConfigSHA: strings.Repeat("2", 64),
		GraphDigest:      strings.Repeat("3", 64),
	}
	for _, p := range pkgs {
		g.Packages[p.Name] = p
	}
	return g
}

func modeOf(t *testing.T, d Descriptor, name string) DeliveryMode {
	t.Helper()
	for _, p := range d.Delivery {
		if p.Name == name {
			return p.Mode
		}
	}
	t.Fatalf("no delivery entry for %q", name)
	return ""
}

// REQUIRED TEST 1. An unchanged native plugin in the base must not block a Dart-only patch -- and must
// be classified without anything knowing its name.
func TestUnchangedNativePluginIsBaseReferenceOnlyAndDoesNotBlock(t *testing.T) {
	plugin := nativePlugin("some_native_plugin", "0.2.5")
	base := graphOf("app", plugin)
	cand := graphOf("app", plugin,
		pkg("m_core", "1.0.0", true), pkg("m_other", "1.0.0", true),
		pkg("m_multi", "1.0.0", true), pkg("m_gen", "1.0.0", true), pkg("m_util", "1.0.0", true))

	d := BuildDescriptor(base, cand)
	if got := modeOf(t, d, "some_native_plugin"); got != ModeBaseReferenceOnly {
		t.Fatalf("unchanged plugin = %s, want %s", got, ModeBaseReferenceOnly)
	}
	for _, n := range []string{"m_core", "m_other", "m_multi", "m_gen", "m_util"} {
		if got := modeOf(t, d, n); got != ModeCarriable {
			t.Errorf("new pure-Dart %s = %s, want %s", n, got, ModeCarriable)
		}
	}
	if err := d.Assess(); err != nil {
		t.Fatalf("an unchanged plugin must not block a Dart-only patch: %v", err)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("descriptor must validate: %v", err)
	}
}

// REQUIRED TESTS 2-5. Every shape of plugin CHANGE is forbidden.
func TestPluginChangesAreForbidden(t *testing.T) {
	basePlugin := nativePlugin("plug", "1.0.0")
	cases := map[string]struct {
		base, cand Graph
	}{
		"newly added native plugin": {
			base: graphOf("app"),
			cand: graphOf("app", nativePlugin("plug", "1.0.0")),
		},
		"existing plugin upgraded": {
			base: graphOf("app", basePlugin),
			cand: graphOf("app", nativePlugin("plug", "1.1.0")),
		},
		"hosted replaced by path at the SAME version": {
			base: graphOf("app", basePlugin),
			cand: graphOf("app", nativePlugin("plug", "1.0.0"), func() Package {
				p := nativePlugin("plug", "1.0.0")
				p.Source = SourcePath
				p.SourceID = "path:../plug"
				p.ContentHash = ""
				p.TreeHash = strings.Repeat("b", 40)
				return p
			}()),
		},
		"same metadata, changed native build output": {
			base: graphOf("app", basePlugin),
			cand: graphOf("app", func() Package {
				p := nativePlugin("plug", "1.0.0")
				p.ContentHash = strings.Repeat("c", 64) // the shipped bytes differ
				return p
			}()),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := BuildDescriptor(tc.base, tc.cand)
			if got := modeOf(t, d, "plug"); got != ModeForbidden {
				t.Fatalf("%s -> %s, want %s", name, got, ModeForbidden)
			}
			err := d.Assess()
			if err == nil {
				t.Fatal("a plugin change must refuse the patch")
			}
			if !strings.Contains(err.Error(), "App Store") {
				t.Errorf("refusal must name the store-release remedy; got: %v", err)
			}
		})
	}
}

// A removed runtime dependency is forbidden: the base still contains and may reference it.
func TestRemovedDependencyIsForbidden(t *testing.T) {
	base := graphOf("app", pkg("gone", "1.0.0", true))
	d := BuildDescriptor(base, graphOf("app"))
	if got := modeOf(t, d, "gone"); got != ModeForbidden {
		t.Fatalf("removed package = %s, want %s", got, ModeForbidden)
	}
	if err := d.Assess(); err == nil {
		t.Fatal("removal must refuse the patch")
	}
}

// An UPGRADED pure-Dart package is carriable -- upgrades are deliverable, unlike plugin upgrades.
func TestUpgradedPureDartPackageIsCarriable(t *testing.T) {
	base := graphOf("app", pkg("m_core", "1.0.0", true))
	cand := graphOf("app", func() Package {
		p := pkg("m_core", "2.0.0", true)
		p.ContentHash = strings.Repeat("d", 64)
		return p
	}())
	d := BuildDescriptor(base, cand)
	if got := modeOf(t, d, "m_core"); got != ModeCarriable {
		t.Fatalf("upgraded Dart package = %s, want %s", got, ModeCarriable)
	}
	if err := d.Assess(); err != nil {
		t.Fatalf("a Dart-only upgrade must be deliverable: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// REQUIRED TEST: FULLY REBOUND TAMPER. Each case edits a mode AND recomputes the descriptor digest,
// so the hash agrees. Only the semantic re-decision can reject them.
// ---------------------------------------------------------------------------------------------

func reboundDescriptor(t *testing.T, d Descriptor, mutate func(*Descriptor)) Descriptor {
	t.Helper()
	mutate(&d)
	d.DescriptorDigest = d.computeDigest() // <- the tamper is fully rebound
	if got := d.computeDigest(); got != d.DescriptorDigest {
		t.Fatal("rebind failed")
	}
	return d
}

func TestFullyReboundDeliveryTamperIsRejected(t *testing.T) {
	base := graphOf("app", nativePlugin("plug", "1.0.0"), pkg("keep", "1.0.0", true))
	cand := graphOf("app", nativePlugin("plug", "1.0.0"), pkg("keep", "1.0.0", true), pkg("newdep", "1.0.0", true))
	good := BuildDescriptor(base, cand)
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline descriptor must validate: %v", err)
	}

	cases := map[string]func(*Descriptor){
		"carriable changed to base_reference_only": func(d *Descriptor) {
			for i := range d.Delivery {
				if d.Delivery[i].Name == "newdep" {
					d.Delivery[i].Mode = ModeBaseReferenceOnly
				}
			}
		},
		"base_reference_only changed to carriable": func(d *Descriptor) {
			for i := range d.Delivery {
				if d.Delivery[i].Name == "plug" {
					d.Delivery[i].Mode = ModeCarriable
				}
			}
		},
		"swapped identity/mode pair": func(d *Descriptor) {
			for i := range d.Delivery {
				if d.Delivery[i].Name == "newdep" {
					d.Delivery[i].Name = "plug"
				} else if d.Delivery[i].Name == "plug" {
					d.Delivery[i].Name = "newdep"
				}
			}
		},
		"missing delivery entry": func(d *Descriptor) {
			out := d.Delivery[:0]
			for _, p := range d.Delivery {
				if p.Name != "plug" {
					out = append(out, p)
				}
			}
			d.Delivery = out
		},
		"extra delivery entry for a package not in either graph": func(d *Descriptor) {
			d.Delivery = append(d.Delivery, PackageDelivery{
				Name: "ghost", Mode: ModeCarriable, Version: "9.9.9",
				Source: "hosted", SourceID: "hosted:pub.dev",
				IdentityID: strings.Repeat("e", 64), CapabilityID: strings.Repeat("f", 64),
			})
		},
		"duplicate delivery entry": func(d *Descriptor) {
			d.Delivery = append(d.Delivery, d.Delivery[0])
		},
		"unknown mode": func(d *Descriptor) {
			d.Delivery[0].Mode = DeliveryMode("carriable_but_trust_me")
		},
		"forbidden without an actionable reason": func(d *Descriptor) {
			d.Delivery[0].Mode = ModeForbidden
			d.Delivery[0].Reason = ""
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := reboundDescriptor(t, good, mutate)
			err := bad.Validate()
			if err == nil {
				t.Fatalf("fully rebound tamper (%s) was ACCEPTED: every hash agreed and nothing "+
					"re-decided the classification", name)
			}
			if strings.Contains(err.Error(), "digest mismatch") {
				t.Fatalf("rejected only by hash, so the rebind was incomplete and this case proves "+
					"nothing about semantic validation: %v", err)
			}
		})
	}
}

// An ineligible package classified carriable would let a native/asset change ride a code-only patch.
func TestIneligiblePackageCannotBeCarriable(t *testing.T) {
	base := graphOf("app")
	cand := graphOf("app", nativePlugin("plug", "1.0.0"))
	d := BuildDescriptor(base, cand)
	bad := reboundDescriptor(t, d, func(x *Descriptor) {
		for i := range x.Delivery {
			if x.Delivery[i].Name == "plug" {
				x.Delivery[i].Mode = ModeCarriable
				x.Delivery[i].Reason = ""
			}
		}
	})
	if err := bad.Validate(); err == nil {
		t.Fatal("an ineligible package classified carriable was accepted")
	}
}

// Determinism: identical inputs must produce an identical descriptor, including the delivery block.
func TestDeliveryClassificationIsDeterministic(t *testing.T) {
	base := graphOf("app", nativePlugin("plug", "1.0.0"), pkg("keep", "1.0.0", true))
	cand := graphOf("app", nativePlugin("plug", "1.0.0"), pkg("keep", "1.0.0", true),
		pkg("b", "1.0.0", true), pkg("a", "1.0.0", true), pkg("c", "1.0.0", true))
	first := BuildDescriptor(base, cand)
	for i := 0; i < 5; i++ {
		again := BuildDescriptor(base, cand)
		if again.DescriptorDigest != first.DescriptorDigest {
			t.Fatalf("run %d produced a different descriptor digest", i)
		}
		if len(again.Delivery) != len(first.Delivery) {
			t.Fatalf("run %d produced a different delivery length", i)
		}
		for j := range again.Delivery {
			if again.Delivery[j] != first.Delivery[j] {
				t.Fatalf("run %d delivery[%d] differs", i, j)
			}
		}
	}
}

// The carriage set must never include a base_reference_only package: that is the whole point of the
// distinction -- referencing is allowed, copying the source is not.
func TestBaseReferenceOnlyIsNeverCarriable(t *testing.T) {
	base := graphOf("app", nativePlugin("plug", "1.0.0"))
	cand := graphOf("app", nativePlugin("plug", "1.0.0"), pkg("newdep", "1.0.0", true))
	d := BuildDescriptor(base, cand)

	carriable := d.CarriablePackages()
	for _, n := range carriable {
		if n == "plug" {
			t.Fatal("an unchanged base plugin appeared in the carriage set; its source must never be copied")
		}
	}
	if len(carriable) != 1 || carriable[0] != "newdep" {
		t.Fatalf("carriable = %v, want [newdep]", carriable)
	}
	refs := d.BaseReferencePackages()
	if len(refs) != 1 || refs[0] != "plug" {
		t.Fatalf("base references = %v, want [plug]", refs)
	}
}

// FULLY REBOUND IDENTITY-FIELD TAMPER.
//
// The mode tests above prove a rewritten MODE is caught. These prove a rewritten IDENTITY is caught:
// each case edits one field of a delivery entry and recomputes the descriptor digest, so every hash
// agrees. Rejection must come from the field disagreeing with its authoritative delta record, never
// from a digest mismatch -- which the assertion below enforces explicitly.
func TestFullyReboundIdentityFieldTamperIsRejected(t *testing.T) {
	base := graphOf("app",
		nativePlugin("plug", "1.0.0"),
		pkg("keep", "1.0.0", true),
		pkg("oldver", "1.0.0", true),
		pkg("gone", "1.0.0", true))
	cand := graphOf("app",
		nativePlugin("plug", "1.0.0"),
		pkg("keep", "1.0.0", true),
		func() Package { p := pkg("oldver", "2.0.0", true); p.ContentHash = strings.Repeat("9", 64); return p }(),
		pkg("newdep", "1.0.0", true))
	good := BuildDescriptor(base, cand)
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline must validate: %v", err)
	}

	edit := func(name string, f func(*PackageDelivery)) func(*Descriptor) {
		return func(d *Descriptor) {
			for i := range d.Delivery {
				if d.Delivery[i].Name == name {
					f(&d.Delivery[i])
				}
			}
		}
	}

	cases := map[string]func(*Descriptor){
		"added: version rewritten":           edit("newdep", func(p *PackageDelivery) { p.Version = "9.9.9" }),
		"added: source rewritten":            edit("newdep", func(p *PackageDelivery) { p.Source = "path" }),
		"added: source_id rewritten":         edit("newdep", func(p *PackageDelivery) { p.SourceID = "path:../elsewhere" }),
		"added: identity_hash rewritten":     edit("newdep", func(p *PackageDelivery) { p.IdentityID = strings.Repeat("7", 64) }),
		"added: capability_digest rewritten": edit("newdep", func(p *PackageDelivery) { p.CapabilityID = strings.Repeat("8", 64) }),
		"unchanged: identity_hash rewritten": edit("keep", func(p *PackageDelivery) { p.IdentityID = strings.Repeat("6", 64) }),
		"unchanged: capability rewritten":    edit("plug", func(p *PackageDelivery) { p.CapabilityID = strings.Repeat("5", 64) }),
		"upgraded: version rewritten":        edit("oldver", func(p *PackageDelivery) { p.Version = "1.0.0" }),
		"upgraded: identity_hash rewritten":  edit("oldver", func(p *PackageDelivery) { p.IdentityID = strings.Repeat("4", 64) }),
		"removed: version rewritten":         edit("gone", func(p *PackageDelivery) { p.Version = "0.0.1" }),
		// The nastiest shape: take one package's identity and pair it with another package's VALID mode.
		"identity paired with another package's valid mode": func(d *Descriptor) {
			var plugIdent PackageDelivery
			for _, p := range d.Delivery {
				if p.Name == "plug" {
					plugIdent = p
				}
			}
			for i := range d.Delivery {
				if d.Delivery[i].Name == "newdep" {
					d.Delivery[i].Version = plugIdent.Version
					d.Delivery[i].Source = plugIdent.Source
					d.Delivery[i].SourceID = plugIdent.SourceID
					d.Delivery[i].IdentityID = plugIdent.IdentityID
					d.Delivery[i].CapabilityID = plugIdent.CapabilityID
				}
			}
		},
		"unchanged name list edited away from its identity list": func(d *Descriptor) {
			if len(d.Unchanged) > 0 {
				d.Unchanged[0] = "not_a_real_package"
			}
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := reboundDescriptor(t, good, mutate)
			err := bad.Validate()
			if err == nil {
				t.Fatalf("fully rebound identity tamper (%s) was ACCEPTED", name)
			}
			if strings.Contains(err.Error(), "digest mismatch") {
				t.Fatalf("rejected only by hash, so the rebind was incomplete and this proves nothing "+
					"about semantic validation: %v", err)
			}
		})
	}
}
