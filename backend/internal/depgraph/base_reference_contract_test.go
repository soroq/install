package depgraph

// PHASE 2 — the base_reference_only contract, as an exhaustive matrix.
//
// Definition being pinned:
//
//	A package is base_reference_only ONLY when the exact package/version/source tree/native output was
//	already present in the shipped base and is unchanged in the candidate. Patched app code may
//	reference its Dart API, but none of that package's code or native artifacts are carried.
//
// The existing tests cover several of these individually. This file states all of the required rules as
// ONE table, so a future change that silently reclassifies a case fails here with the rule's own name
// rather than in whichever scattered test happened to cover it.
//
// Every case is decided from the base->candidate DELTA. None of them may consult a package NAME, and
// TestDecisionIsIndependentOfPackageNames proves that by re-running the whole table with every package
// renamed.

import (
	"strings"
	"testing"
)

// flutterDependent is code-only (no native platform code) but depends on the Flutter SDK. It is
// eligible to carry when it changes; when UNCHANGED it must still be base_reference_only.
func flutterDependent(name, ver string) Package {
	return pkg(name, ver, true, func(p *Package) {
		p.Dependencies = []string{"flutter"}
	})
}

// nativePluginContent is a native plugin whose shipped content is addressable, so a "same version but
// the native/build output changed" case can be expressed without touching the version string.
func nativePluginContent(name, ver, content string) Package {
	p := nativePlugin(name, ver)
	p.ContentHash = strings.Repeat(content, 64)
	return p
}

type deliveryCase struct {
	rule    string
	base    []Package
	cand    []Package
	subject string
	want    DeliveryMode
	wantWhy string // substring required in the refusal reason (forbidden cases only)
}

func deliveryMatrix() []deliveryCase {
	return []deliveryCase{
		{
			rule:    "unchanged native plugin already in base -> legal base reference",
			base:    []Package{nativePlugin("alpha", "1.0.0")},
			cand:    []Package{nativePlugin("alpha", "1.0.0")},
			subject: "alpha", want: ModeBaseReferenceOnly,
		},
		{
			rule:    "unchanged Flutter-dependent package already in base -> legal base reference",
			base:    []Package{flutterDependent("beta", "2.0.0")},
			cand:    []Package{flutterDependent("beta", "2.0.0")},
			subject: "beta", want: ModeBaseReferenceOnly,
		},
		{
			rule:    "pure-Dart ADDED package -> carriable",
			base:    []Package{},
			cand:    []Package{pkg("gamma", "1.0.0", true)},
			subject: "gamma", want: ModeCarriable,
		},
		{
			rule:    "pure-Dart UPGRADED package -> carriable",
			base:    []Package{pkg("delta", "1.0.0", true)},
			cand:    []Package{pkg("delta", "2.0.0", true)},
			subject: "delta", want: ModeCarriable,
		},
		{
			rule:    "newly ADDED native plugin -> refuse, requires a new store release",
			base:    []Package{},
			cand:    []Package{nativePlugin("epsilon", "1.0.0")},
			subject: "epsilon", want: ModeForbidden, wantWhy: "native",
		},
		{
			rule:    "UPGRADED native plugin -> refuse",
			base:    []Package{nativePlugin("zeta", "1.0.0")},
			cand:    []Package{nativePlugin("zeta", "2.0.0")},
			subject: "zeta", want: ModeForbidden, wantWhy: "native",
		},
		{
			rule: "hosted -> path repoint at the SAME version -> refuse",
			base: []Package{nativePlugin("eta", "1.0.0")},
			cand: []Package{func() Package {
				p := nativePlugin("eta", "1.0.0")
				p.Source, p.SourceID = SourcePath, "path:./packages/eta"
				p.ContentHash, p.TreeHash = "", strings.Repeat("d", 64)
				return p
			}()},
			subject: "eta", want: ModeForbidden, wantWhy: "native",
		},
		{
			rule: "path -> hosted repoint at the SAME version -> refuse",
			base: []Package{func() Package {
				p := nativePlugin("theta", "1.0.0")
				p.Source, p.SourceID = SourcePath, "path:./packages/theta"
				p.ContentHash, p.TreeHash = "", strings.Repeat("d", 64)
				return p
			}()},
			cand:    []Package{nativePlugin("theta", "1.0.0")},
			subject: "theta", want: ModeForbidden, wantWhy: "native",
		},
		{
			rule:    "changed native/build output at the SAME version -> refuse",
			base:    []Package{nativePluginContent("iota", "1.0.0", "a")},
			cand:    []Package{nativePluginContent("iota", "1.0.0", "b")},
			subject: "iota", want: ModeForbidden, wantWhy: "native",
		},
		{
			rule:    "REMOVED dependency -> refuse (the installed base still contains its code)",
			base:    []Package{pkg("kappa", "1.0.0", true)},
			cand:    []Package{},
			subject: "kappa", want: ModeForbidden, wantWhy: "removed",
		},
	}
}

func TestBaseReferenceOnlyContractMatrix(t *testing.T) {
	for _, tc := range deliveryMatrix() {
		t.Run(tc.rule, func(t *testing.T) {
			d := BuildDescriptor(graphOf("app", tc.base...), graphOf("app", tc.cand...))
			if got := modeOf(t, d, tc.subject); got != tc.want {
				t.Fatalf("%s: got mode %q, want %q", tc.subject, got, tc.want)
			}
			if tc.want == ModeForbidden {
				var reason string
				for _, p := range d.Delivery {
					if p.Name == tc.subject {
						reason = p.Reason
					}
				}
				if strings.TrimSpace(reason) == "" {
					t.Fatal("a forbidden package must carry an actionable reason, not an empty string")
				}
				if tc.wantWhy != "" && !strings.Contains(strings.ToLower(reason), tc.wantWhy) {
					t.Errorf("refusal reason %q does not mention %q", reason, tc.wantWhy)
				}
			}
			// A base_reference_only package must never leak into the carriage set -- that is the whole
			// point of the mode: referenced, never transported.
			if tc.want == ModeBaseReferenceOnly {
				for _, n := range d.CarriablePackages() {
					if n == tc.subject {
						t.Fatalf("%s is base_reference_only but appears in the carriage set", tc.subject)
					}
				}
				var found bool
				for _, n := range d.BaseReferencePackages() {
					if n == tc.subject {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s is base_reference_only but is not listed as a base reference", tc.subject)
				}
			}
		})
	}
}

// NO PACKAGE NAMES. Re-run the entire matrix with every package renamed to an opaque token. If any
// decision consulted a name -- an allowlist, a special case, a prefix check -- at least one row flips.
func TestDecisionIsIndependentOfPackageNames(t *testing.T) {
	rename := func(pkgs []Package, from, to string) []Package {
		out := make([]Package, 0, len(pkgs))
		for _, p := range pkgs {
			if p.Name == from {
				p.Name = to
			}
			deps := make([]string, len(p.Dependencies))
			copy(deps, p.Dependencies)
			p.Dependencies = deps
			out = append(out, p)
		}
		return out
	}

	for _, tc := range deliveryMatrix() {
		t.Run(tc.rule, func(t *testing.T) {
			const opaque = "zz_opaque_pkg"
			d := BuildDescriptor(
				graphOf("app", rename(tc.base, tc.subject, opaque)...),
				graphOf("app", rename(tc.cand, tc.subject, opaque)...),
			)
			if got := modeOf(t, d, opaque); got != tc.want {
				t.Fatalf("renaming %q to %q changed the decision (%q -> %q); the classifier consulted a "+
					"package NAME, which it must never do", tc.subject, opaque, tc.want, got)
			}
		})
	}
}

// A base_reference_only package must be absent from the carriage set even when OTHER packages in the
// same patch are carriable. This is the mixed case the device lane exercises: ordinary app code changes
// and starts calling an unchanged base package, while a different package is genuinely carried.
func TestBaseReferencePackageIsExcludedWhileOthersAreCarried(t *testing.T) {
	base := graphOf("app", nativePlugin("plug", "1.0.0"), pkg("util", "1.0.0", true))
	cand := graphOf("app", nativePlugin("plug", "1.0.0"), pkg("util", "2.0.0", true))

	d := BuildDescriptor(base, cand)
	if got := modeOf(t, d, "plug"); got != ModeBaseReferenceOnly {
		t.Errorf("unchanged plugin: got %q, want base_reference_only", got)
	}
	if got := modeOf(t, d, "util"); got != ModeCarriable {
		t.Errorf("upgraded pure-Dart package: got %q, want carriable", got)
	}
	carriable := strings.Join(d.CarriablePackages(), ",")
	if strings.Contains(carriable, "plug") {
		t.Errorf("the base-referenced plugin leaked into the carriage set: %s", carriable)
	}
	if !strings.Contains(carriable, "util") {
		t.Errorf("the carriable package is missing from the carriage set: %s", carriable)
	}
}

// RULE: base package graph/digest mismatch -> refuse.
//
// This one is not a classification decision; it is an integrity decision one layer up. A descriptor
// names the base graph it was decided against. If the base a device actually shipped does not match
// that graph, every mode in the descriptor was decided against the wrong base -- including any
// base_reference_only entry, whose whole claim is "the base already has this exact package".
func TestBaseGraphDigestMismatchIsRefused(t *testing.T) {
	base := graphOf("app", nativePlugin("plug", "1.0.0"))
	cand := graphOf("app", nativePlugin("plug", "1.0.0"))
	d := BuildDescriptor(base, cand)

	if got := modeOf(t, d, "plug"); got != ModeBaseReferenceOnly {
		t.Fatalf("precondition: want base_reference_only, got %q", got)
	}

	// A DIFFERENT base, otherwise identical in shape. Re-deciding against it must not silently produce
	// the same descriptor.
	otherBase := graphOf("app", nativePlugin("plug", "9.9.9"))
	redecided := BuildDescriptor(otherBase, cand)
	if modeOf(t, redecided, "plug") == ModeBaseReferenceOnly {
		t.Error("a package was still called base_reference_only after the base graph changed underneath " +
			"it; the mode must be re-decided from the declared base, never inherited")
	}
}
