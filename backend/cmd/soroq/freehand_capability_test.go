package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE DEFECT THESE TESTS EXIST FOR. A freehand patch carrying three constructor identities was
// published, verified, staged and committed on a real iPhone — requested=4, committed=4, active
// state=patched — and none of the three redirects changed anything observable. Every producer- and
// transport-side guard passed, and each was right to pass. Nothing asked whether the base's ENGINE
// could honour a redirect on that KIND of identity.
//
// The guard under test answers that question from DATA RECORDED ON THE BASE. So the suite has to prove
// three separable things, and the controls matter more than the count:
//
//   - a green control, or a guard that refused everything would look identical to one that works;
//   - planted failures whose REFUSAL TEXT names the kind and the identity, or a refusal for an
//     unrelated reason would be scored as coverage;
//   - the same planted patch ACCEPTED against a base whose engine declares the kind, which is the only
//     evidence that the guard is data and not a hard-coded block.

// capDecls turns frozen stable identities into changedDecls through the REAL diff parser, so a test can
// never assert against a decl shape the production path would not produce.
func capDecls(t *testing.T, entries ...[2]string) []changedDecl {
	t.Helper()
	raw := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		raw = append(raw, map[string]any{"manifestLine": e[0], "key": e[1], "patchable": true})
	}
	decls, err := changedDeclsFromDiff(raw)
	if err != nil {
		t.Fatalf("fixture decls are malformed: %v", err)
	}
	if len(decls) != len(entries) {
		t.Fatalf("fixture produced %d decls, want %d", len(decls), len(entries))
	}
	return decls
}

const capLib = "package:app/main.dart"

// The four probe identities from the device transcript, exactly as the analyzer emits them.
var (
	capMethodDecl      = [2]string{capLib + "::_DailySummary::build", "v1|" + capLib + "|method|_DailySummary|build|797de60e"}
	capFunctionDecl    = [2]string{capLib + "::::otaValue", "v1|" + capLib + "|function||otaValue|c55a32db"}
	capConstructorDecl = [2]string{capLib + "::EagerProviderProbe::EagerProviderProbe.", "v1|" + capLib + "|constructor|EagerProviderProbe||9e7dc67f"}
	capFactoryDecl     = [2]string{capLib + "::FactoryCtorProbe::FactoryCtorProbe.make", "v1|" + capLib + "|factory|FactoryCtorProbe|make|27480176"}
	capFieldInitDecl   = [2]string{capLib + "::StaticInitProbe::init:value", "v1|" + capLib + "|field-initializer|StaticInitProbe|value|ce90b6f0"}
)

// legacyBase is a base with NO capability record — every base built before this guard existed.
func legacyBase() *FreehandBaselineMeta {
	m := fullMeta()
	m.RedirectCapabilities = nil
	return &m
}

// engineDeclaringBase is a base whose recorded capabilities were declared by its engine.
func engineDeclaringBase(kinds ...string) *FreehandBaselineMeta {
	m := fullMeta()
	m.RedirectCapabilities = &FreehandRedirectCapabilities{
		Schema:         freehandRedirectCapabilitiesSchema,
		Source:         freehandCapabilitySourceEngine,
		EngineRevision: m.EngineRev,
		HonouredKinds:  append([]string(nil), kinds...),
		Note:           "declared by the engine bundle itself (test fixture)",
	}
	return &m
}

// GREEN CONTROL. Without this, every refusal below could be produced by a guard that refuses
// everything — which would look like coverage and be none. A method- and function-kind change is what
// the overwhelming majority of real patches are, and it must still plan against a legacy base.
func TestRedirectCapability_MethodAndFunctionOnLegacyBaseAreAccepted(t *testing.T) {
	if err := assertFreehandRedirectCapability(legacyBase(), capDecls(t, capMethodDecl, capFunctionDecl)); err != nil {
		t.Fatalf("a method+function patch against a legacy-default base was REFUSED: %v", err)
	}
}

// PLANTED FAILURES. Each must be refused, and the refusal must NAME the kind and the identity: an error
// that merely occurred is not evidence the guard understood what it was refusing.
func TestRedirectCapability_PlantedUnhonouredKindsAreRefused(t *testing.T) {
	cases := []struct {
		name string
		decl [2]string
		kind string
	}{
		{"constructor", capConstructorDecl, "constructor"},
		{"factory", capFactoryDecl, "factory"},
		{"field-initializer", capFieldInitDecl, "field-initializer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := assertFreehandRedirectCapability(legacyBase(), capDecls(t, tc.decl))
			if err == nil {
				t.Fatalf("a %s identity was ACCEPTED against a legacy-default base; that is the silent no-op this guard exists to stop", tc.kind)
			}
			msg := err.Error()
			for _, want := range []string{tc.kind, tc.decl[0]} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal does not name %q:\n%s", want, msg)
				}
			}
			// The refusal must also say WHY, and what the base does honour — otherwise the developer
			// learns only that something was rejected.
			for _, want := range []string{"change NOTHING on device", "honours:", legacyDefaultRedirectKinds[0]} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal does not explain %q:\n%s", want, msg)
				}
			}
			t.Logf("REFUSAL TEXT (%s):\n%s", tc.kind, msg)
		})
	}
}

// A MIXED patch is refused whole: one unhonoured identity among honoured ones would otherwise ship as a
// patch that half works, which is the same silence with extra steps.
func TestRedirectCapability_MixedPatchIsRefusedAndNamesEveryOffender(t *testing.T) {
	err := assertFreehandRedirectCapability(legacyBase(), capDecls(t, capMethodDecl, capConstructorDecl, capFieldInitDecl))
	if err == nil {
		t.Fatal("a patch mixing honoured and unhonoured kinds was accepted")
	}
	for _, want := range []string{"constructor", "field-initializer", capConstructorDecl[0], capFieldInitDecl[0]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q:\n%s", want, err.Error())
		}
	}
	if strings.Contains(err.Error(), capMethodDecl[0]) {
		t.Errorf("refusal names the HONOURED method identity as an offender:\n%s", err.Error())
	}
}

// THE DATA-DRIVEN PROOF. The SAME constructor patch that was refused above is ACCEPTED against a base
// whose recorded capabilities declare `constructor`. This is the test that separates a guard driven by
// the base's data from a hard-coded block on a kind: only the base changed.
func TestRedirectCapability_DeclaredConstructorIsAccepted(t *testing.T) {
	decls := capDecls(t, capConstructorDecl)
	if err := assertFreehandRedirectCapability(legacyBase(), decls); err == nil {
		t.Fatal("control failed: the constructor patch must be refused against a legacy-default base")
	}
	declared := engineDeclaringBase(append(append([]string(nil), legacyDefaultRedirectKinds...), "constructor")...)
	if err := assertFreehandRedirectCapability(declared, decls); err != nil {
		t.Fatalf("the SAME constructor patch was refused against a base whose engine DECLARES constructor support — the guard is a block, not data: %v", err)
	}
	// ...and declaring constructor must not smuggle in the other two unmeasured kinds.
	if err := assertFreehandRedirectCapability(declared, capDecls(t, capFieldInitDecl)); err == nil {
		t.Fatal("a base declaring only `constructor` also accepted a field-initializer identity")
	}
}

// ABSENT IS LEGACY-DEFAULT, NEVER ALLOW-ALL. Every base that already shipped has no capability field;
// reading absence as permissive would reinstate the exact defect for exactly those bases.
func TestRedirectCapability_AbsentRecordIsLegacyDefaultNotAllowAll(t *testing.T) {
	caps, err := baseRedirectCapabilities(legacyBase())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Source != freehandCapabilitySourceLegacy {
		t.Errorf("absent record resolved to source %q, want %q", caps.Source, freehandCapabilitySourceLegacy)
	}
	if len(caps.HonouredKinds) != len(legacyDefaultRedirectKinds) {
		t.Errorf("absent record resolved to %v, want the legacy default %v", caps.HonouredKinds, legacyDefaultRedirectKinds)
	}
	if strings.TrimSpace(caps.Note) == "" {
		t.Error("the recorded value does not say WHY it is what it is; a future reader cannot tell 'nobody measured this' from 'an engine measured exactly this'")
	}
	for _, k := range []string{"constructor", "factory", "field-initializer"} {
		if caps.kindSet()[k] {
			t.Errorf("the legacy default honours %q, which no engine has been shown to honour", k)
		}
	}
}

// A MALFORMED RECORD FAILS CLOSED, WITH A REASON. A capability record nobody can parse describes a base
// nobody has measured; the one thing it must never do is read as permissive.
func TestRedirectCapability_MalformedRecordFailsClosed(t *testing.T) {
	good := func() *FreehandRedirectCapabilities {
		return &FreehandRedirectCapabilities{
			Schema: freehandRedirectCapabilitiesSchema, Source: freehandCapabilitySourceEngine,
			EngineRevision: "eng-1", HonouredKinds: []string{"method"}, Note: "n",
		}
	}
	// Control: the well-formed record this table mutates is itself accepted.
	if err := validateFreehandRedirectCapabilities(good()); err != nil {
		t.Fatalf("the well-formed control record was rejected, so every case below proves nothing: %v", err)
	}
	cases := []struct {
		name    string
		wantMsg string
		mutate  func(c *FreehandRedirectCapabilities)
	}{
		{"unknown-schema", "schema", func(c *FreehandRedirectCapabilities) { c.Schema = "soroq.freehand.redirect_capabilities.v9" }},
		{"empty-schema", "schema", func(c *FreehandRedirectCapabilities) { c.Schema = "" }},
		{"unknown-source", "source", func(c *FreehandRedirectCapabilities) { c.Source = "trust-me" }},
		{"no-engine-revision", "engine_revision", func(c *FreehandRedirectCapabilities) { c.EngineRevision = " " }},
		{"zero-kinds", "ZERO honoured kinds", func(c *FreehandRedirectCapabilities) { c.HonouredKinds = nil }},
		{"not-a-semantic-kind", "not a frozen semantic identity kind", func(c *FreehandRedirectCapabilities) {
			c.HonouredKinds = []string{"method", "instance-member"} // an ABI shape label, not a semantic kind
		}},
		{"duplicate-kind", "twice", func(c *FreehandRedirectCapabilities) { c.HonouredKinds = []string{"method", "method"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good()
			tc.mutate(c)
			err := validateFreehandRedirectCapabilities(c)
			if err == nil {
				t.Fatalf("a malformed capability record (%s) was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refusal does not give the reason %q:\n%s", tc.wantMsg, err.Error())
			}
			// And the guard itself must refuse rather than fall back to any default.
			m := fullMeta()
			m.RedirectCapabilities = c
			gerr := assertFreehandRedirectCapability(&m, capDecls(t, capMethodDecl))
			if gerr == nil {
				t.Fatalf("the publish guard ACCEPTED a patch against a base with a malformed (%s) capability record", tc.name)
			}
			if !strings.Contains(gerr.Error(), "cannot decide what this base's engine can honour") {
				t.Errorf("the guard's refusal does not say it could not decide:\n%s", gerr.Error())
			}
		})
	}
}

// writeEngineBundle writes a fixture ~/.soroq/toolchains/<dir>/ios/engine.json. declaration is the raw
// JSON for the capability key, or "" for an engine that declares nothing (every engine shipping today).
func writeEngineBundle(t *testing.T, root, dir, engineRev, declaration string) {
	t.Helper()
	iosDir := filepath.Join(root, dir, "ios")
	if err := os.MkdirAll(iosDir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := `{"schema":"soroq.ios_engine.v2","soroq_engine_revision":` + mustJSON(t, engineRev)
	if declaration != "" {
		doc += `,"` + freehandEngineCapabilityKey + `":` + declaration
	}
	doc += "}"
	if err := os.WriteFile(filepath.Join(iosDir, "engine.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The capability is SOURCED FROM THE ENGINE, not from a constant. Today no engine declares anything, so
// resolution must land on legacy-default and SAY SO; an engine that does declare must be believed.
func TestRedirectCapability_ResolvedFromTheEngineBundle(t *testing.T) {
	t.Run("no-matching-engine-is-legacy-default", func(t *testing.T) {
		root := t.TempDir()
		writeEngineBundle(t, root, "some-other-toolchain", "eng-OTHER", "")
		caps, err := resolveRedirectCapabilitiesFromToolchains(root, "eng-1")
		if err != nil {
			t.Fatal(err)
		}
		if caps.Source != freehandCapabilitySourceLegacy || !strings.Contains(caps.Note, "eng-1") {
			t.Errorf("want a legacy-default record naming the unmatched revision, got %+v", caps)
		}
	})
	t.Run("engine-declaring-nothing-is-legacy-default", func(t *testing.T) {
		root := t.TempDir()
		writeEngineBundle(t, root, "tc-a", "eng-1", "")
		caps, err := resolveRedirectCapabilitiesFromToolchains(root, "eng-1")
		if err != nil {
			t.Fatal(err)
		}
		if caps.Source != freehandCapabilitySourceLegacy {
			t.Errorf("an engine that declares nothing must resolve to legacy-default, got %+v", caps)
		}
		if !strings.Contains(caps.Note, freehandEngineCapabilityKey) {
			t.Errorf("the recorded note does not say what was missing: %q", caps.Note)
		}
	})
	t.Run("engine-declaring-constructor-is-believed", func(t *testing.T) {
		root := t.TempDir()
		writeEngineBundle(t, root, "tc-future", "eng-1", `{"honoured_kinds":["method","function","constructor"]}`)
		caps, err := resolveRedirectCapabilitiesFromToolchains(root, "eng-1")
		if err != nil {
			t.Fatal(err)
		}
		if caps.Source != freehandCapabilitySourceEngine || !caps.kindSet()["constructor"] {
			t.Fatalf("the engine's own declaration was not carried through: %+v", caps)
		}
		if caps.kindSet()["factory"] {
			t.Error("a kind the engine did not declare was added to the set")
		}
	})
	t.Run("malformed-declaration-is-an-error-not-a-downgrade", func(t *testing.T) {
		for _, decl := range []string{
			`["constructor"]`,                        // a bare list is a different contract
			`{"honoured_kinds":[]}`,                  // honours nothing
			`{"honoured_kinds":["instance-member"]}`, // an ABI shape label, not a semantic kind
			`{"kinds":["constructor"]}`,              // unknown field
			`{"honoured_kinds":["method","method"]}`, // duplicate
			`"constructor"`,                          // a stray string
		} {
			root := t.TempDir()
			writeEngineBundle(t, root, "tc-bad", "eng-1", decl)
			caps, err := resolveRedirectCapabilitiesFromToolchains(root, "eng-1")
			if err == nil {
				t.Fatalf("a malformed engine declaration %s silently resolved to %+v", decl, caps)
			}
			if !strings.Contains(err.Error(), freehandEngineCapabilityKey) {
				t.Errorf("the error does not name the malformed key:\n%s", err.Error())
			}
		}
	})
	t.Run("disagreeing-bundles-fail-closed", func(t *testing.T) {
		root := t.TempDir()
		writeEngineBundle(t, root, "tc-a", "eng-1", `{"honoured_kinds":["method"]}`)
		writeEngineBundle(t, root, "tc-b", "eng-1", `{"honoured_kinds":["method","constructor"]}`)
		if caps, err := resolveRedirectCapabilitiesFromToolchains(root, "eng-1"); err == nil {
			t.Fatalf("two bundles claiming the same engine revision but declaring different kinds resolved to %+v", caps)
		}
	})
	t.Run("no-engine-revision-fails-closed", func(t *testing.T) {
		if _, err := resolveRedirectCapabilitiesFromToolchains(t.TempDir(), "  "); err == nil {
			t.Fatal("a baseline with no engine_revision resolved a capability set anyway")
		}
	})
}

// THE WHOLE POINT, END TO END: a FUTURE engine unlocks constructor identities with NO producer change.
// Only the engine bundle differs between the two halves; the producer code, the patch and the base
// fixtures are identical. If this ever needs a code edit to pass, the capability has stopped being data.
func TestRedirectCapability_FutureEngineUnlocksConstructorsWithNoProducerChange(t *testing.T) {
	decls := capDecls(t, capConstructorDecl)

	persistWithEngine := func(t *testing.T, declaration string) *FreehandBaselineMeta {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeEngineBundle(t, filepath.Join(home, ".soroq", "toolchains"), "tc-under-test", fullMeta().EngineRev, declaration)
		proj, dill, srcDill, man, graph := seedFixture(t)
		relDir, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
		if err != nil {
			t.Fatal(err)
		}
		// Re-read through the STRICT verifier, so what is asserted is what a patch run would load.
		base, err := verifyExistingBaseline(relDir)
		if err != nil {
			t.Fatalf("the persisted baseline failed strict verification: %v", err)
		}
		if base.RedirectCapabilities == nil {
			t.Fatal("the baseline recorded no capability set at all")
		}
		return base
	}

	t.Run("todays-engine-declares-nothing-so-constructors-are-refused", func(t *testing.T) {
		base := persistWithEngine(t, "")
		if base.RedirectCapabilities.Source != freehandCapabilitySourceLegacy {
			t.Fatalf("recorded source %q, want %q", base.RedirectCapabilities.Source, freehandCapabilitySourceLegacy)
		}
		if err := assertFreehandRedirectCapability(base, decls); err == nil {
			t.Fatal("constructors were accepted against a base whose engine declares nothing")
		}
	})

	t.Run("an-engine-that-declares-constructor-unlocks-it", func(t *testing.T) {
		base := persistWithEngine(t, `{"honoured_kinds":["function","method","static-method","getter","setter","operator","constructor"]}`)
		if base.RedirectCapabilities.Source != freehandCapabilitySourceEngine {
			t.Fatalf("recorded source %q, want %q", base.RedirectCapabilities.Source, freehandCapabilitySourceEngine)
		}
		if err := assertFreehandRedirectCapability(base, decls); err != nil {
			t.Fatalf("a base built on an engine DECLARING constructor support still refused a constructor patch: %v", err)
		}
	})
}

// The capability record must not be able to break a base that already exists: it is not a required
// field and not an immutable input, so an unchanged base still re-registers idempotently.
func TestRedirectCapability_DoesNotMutateOrBreakExistingBaselines(t *testing.T) {
	for _, f := range requiredBaselineFields {
		if f == "redirect_capabilities" {
			t.Fatal("redirect_capabilities became a REQUIRED baseline field; every base written before this guard would stop verifying")
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj, dill, srcDill, man, graph := seedFixture(t)
	d1, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatal(err)
	}
	// A second run with an engine that now declares MORE must still be idempotent, not a refusal to
	// overwrite an immutable baseline: the capability is derived, and re-deriving it is not a change to
	// an immutable input.
	writeEngineBundle(t, filepath.Join(home, ".soroq", "toolchains"), "tc-new", fullMeta().EngineRev, `{"honoured_kinds":["method","constructor"]}`)
	d2, err := persistFreehandBaseline(proj, fullMeta(), dill, srcDill, man, graph, testDepGraph())
	if err != nil {
		t.Fatalf("re-registering an unchanged base after the engine gained a declaration was refused: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("idempotent re-run returned a different dir: %s != %s", d1, d2)
	}
	// The ORIGINAL record is still on disk: nothing rewrote an immutable baseline underneath it.
	base, err := verifyExistingBaseline(d1)
	if err != nil {
		t.Fatal(err)
	}
	if base.RedirectCapabilities.Source != freehandCapabilitySourceLegacy {
		t.Errorf("the immutable baseline was mutated: recorded source is now %q", base.RedirectCapabilities.Source)
	}
}

// ANTI-DRIFT. The capability record can only name kinds from this universe, and expectABI is the
// authority on what a kind means. If a future kind is added to expectABI and not here, a base could
// never declare it and the guard would refuse it forever — a hard-coded limit by omission.
func TestSemanticKindUniverseMatchesExpectABI(t *testing.T) {
	for k := range freehandSemanticKinds {
		modelled := false
		for _, class := range []string{"", "C"} {
			if _, err := expectABI(changedDecl{keyKind: k, keyClass: class, keyMember: "m"}); err == nil {
				modelled = true
			}
		}
		if !modelled {
			t.Errorf("semantic kind %q is declarable in a capability record but expectABI models no VM shape for it", k)
		}
	}
	// The mirror direction: every kind expectABI models must be declarable.
	src, err := os.ReadFile("freehand_patch.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func expectABI(")
	if i < 0 {
		t.Fatal("expectABI is gone")
	}
	body = body[i:]
	if end := strings.Index(body, "\n\n// splitIdentity"); end > 0 {
		body = body[:end]
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "case \"") {
			continue
		}
		for _, part := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "case "), ":"), ",") {
			kind := strings.Trim(strings.TrimSpace(part), `"`)
			if !freehandSemanticKinds[kind] {
				t.Errorf("expectABI models kind %q but no base could ever declare it (missing from freehandSemanticKinds)", kind)
			}
		}
	}
}

// WIRING. The guard is tested at the function level because computeFreehandPatchPlan needs a real
// Flutter toolchain, a base release and a candidate compile. A guard that is never reached from the
// real path is the exact failure mode this tranche is about, so the call site is pinned here: present,
// after the diff has produced changed declarations, and BEFORE anything is synthesised or returned.
func TestRedirectCapabilityGuardIsWiredIntoThePatchPlan(t *testing.T) {
	src, err := os.ReadFile("freehand_patch.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func computeFreehandPatchPlan(")
	if i < 0 {
		t.Fatal("computeFreehandPatchPlan is gone")
	}
	body := s[i:]
	if end := strings.Index(body, "\n// errFreehandNoOp"); end > 0 {
		body = body[:end]
	}
	call := strings.Index(body, "assertFreehandRedirectCapability(base,")
	if call < 0 {
		t.Fatal("computeFreehandPatchPlan does not call assertFreehandRedirectCapability; a patch on an unhonoured kind would publish and silently do nothing")
	}
	diffDone := strings.Index(body, "if !rep.Supported {")
	if diffDone < 0 || diffDone > call {
		t.Error("the capability guard does not run AFTER the diff has produced changed-patchable declarations")
	}
	ret := strings.Index(body, "return &FreehandPatchPlan{")
	if ret < 0 || call > ret {
		t.Error("the capability guard does not run BEFORE the plan is returned for module synthesis")
	}
}
