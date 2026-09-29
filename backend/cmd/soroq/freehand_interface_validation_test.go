package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Recorded from `dart2bytecode --validate` (3.44.9 private-state toolchain) on a module that constructs
// a Flutter widget the base never used, plus one dynamic call and one ordinary compile error.
const recordedValidatorOutput = `soroq-freehand:///ab12/soroq_freehand_module.dart:41:16: Error: Cannot invoke constructor 'CupertinoSlider' from a dynamic module.
Try removing the call or update the dynamic interface to list constructor 'CupertinoSlider' as callable.
          child: CupertinoSlider(value: 0.5, onChanged: (_) {}),
               ^
soroq-freehand:///ab12/soroq_freehand_module.dart:41:16: Error: Cannot use class 'CupertinoSlider' as a type in a dynamic module.
soroq-freehand:///ab12/soroq_freehand_module.dart:52:9: Error: Dynamic calls are not allowed in a dynamic module.
soroq-freehand:///ab12/soroq_freehand_module.dart:60:3: Error: Cannot invoke member 'Scrollbar.of' from a dynamic module.
`

func TestInterfaceValidation_ClassifiesValidatorOutput(t *testing.T) {
	o := classifyInterfaceValidation(recordedValidatorOutput)
	if o.DynamicCalls != 1 || o.OtherErrors != 0 || len(o.Violations) != 3 {
		t.Fatalf("classification = %+v", o)
	}
	msg := explainInterfaceViolations(o.Violations, nil)
	for _, want := range []string{
		"uses constructor CupertinoSlider which the installed base does not expose; ship a store release",
		"uses type CupertinoSlider which the installed base does not expose; ship a store release",
		"uses Scrollbar.of which the installed base does not expose; ship a store release",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal must say %q:\n%s", want, msg)
		}
	}
	if o.Violations[0].Line != 41 {
		t.Fatalf("location lost: %+v", o.Violations[0])
	}
}

func TestInterfaceValidation_DynamicCallsAndCompileErrorsAreNotInterfaceRefusals(t *testing.T) {
	o := classifyInterfaceValidation("x.dart:3:5: Error: Dynamic calls are not allowed in a dynamic module.\n")
	if len(o.Violations) != 0 || o.DynamicCalls != 1 {
		t.Fatalf("%+v", o)
	}
	o = classifyInterfaceValidation("x.dart:3:5: Error: The getter 'foo' isn't defined for the type 'Bar'.\n")
	if len(o.Violations) != 0 || o.OtherErrors != 1 {
		t.Fatalf("%+v", o)
	}
}

func TestInterfaceValidation_OnlyUsageScopedBasesAreGated(t *testing.T) {
	if p := freehandBaseInterfaceValidationPath("/r", &FreehandBaselineMeta{ContractSchema: freehandContractSchema}); p != "" {
		t.Fatalf("a v1 base must not be validated (its contract never was): %q", p)
	}
	if p := freehandBaseInterfaceValidationPath("/r", &FreehandBaselineMeta{ContractSchema: freehandContractSchemaV2}); p != filepath.Join("/r", freehandInterfaceValidationFile) {
		t.Fatalf("a v2 base must be validated against its recorded spec: %q", p)
	}
}

// A usage-scoped base whose spec is missing must refuse, not skip the gate.
func TestInterfaceValidation_MissingSpecRefuses(t *testing.T) {
	err := validateFreehandModuleAgainstBaseInterface("/bin/false", nil, filepath.Join(t.TempDir(), "missing.yaml"), "m.dart", "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("missing spec must refuse: %v", err)
	}
}

// A validator that dies without a diagnostic must refuse (the question was not answered).
func TestInterfaceValidation_ValidatorCrashRefuses(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "spec.yaml")
	os.WriteFile(spec, []byte("callable: []\n"), 0o644)
	err := validateFreehandModuleAgainstBaseInterface("/bin/sh", []string{"-c", "echo 'Unhandled exception: boom'; exit 255", "--"}, spec, "m.dart", "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "failed without a diagnostic") {
		t.Fatalf("validator crash must refuse: %v", err)
	}
}

// END TO END against the real front end: the toolchain's own dart2bytecode + --validate with a spec in
// exactly the form renderInterfaceValidationYAML emits. A module that stays inside the declared classes
// validates; one that uses an SDK class the "base" never used is refused by name; one whose only problem
// is a dynamic call is let through, as this lane always has.
//
// Set SOROQ_TEST_IOS_TOOLCHAIN_DIR to an installed toolchain's ios/ directory (it carries dartaotruntime,
// dart2bytecode and vm_platform), e.g.
// ~/.soroq/toolchains/soroq-ios-3.44.9-release-6b182d2c_5a2a6a42-private_state-r7_obfuscation/ios
func TestInterfaceValidation_RealFrontEndRefusesUnexposedClass(t *testing.T) {
	tc := os.Getenv("SOROQ_TEST_IOS_TOOLCHAIN_DIR")
	if tc == "" {
		t.Skip("SOROQ_TEST_IOS_TOOLCHAIN_DIR not set")
	}
	dartaot, d2b, platform := filepath.Join(tc, "dartaotruntime"), filepath.Join(tc, "dart2bytecode"), filepath.Join(tc, "vm_platform")
	for _, p := range []string{dartaot, d2b, platform} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("toolchain input missing: %v", err)
		}
	}
	dir := t.TempDir()
	d := newScopedDomain(nil)
	usage := []ContractEntry{
		{Library: "dart:core", Kind: "class", Name: "Object"},
		{Library: "dart:core", Kind: "class", Name: "int"},
		{Library: "dart:core", Kind: "class", Name: "num"},
		{Library: "dart:core", Kind: "class", Name: "String"},
		{Library: "dart:core", Kind: "class", Name: "List"},
		{Library: "dart:core", Kind: "class", Name: "Iterable"},
		{Library: "dart:core", Kind: "class", Name: "bool"},
		{Library: "dart:core", Kind: "member", Name: "print"},
	}
	u, err := parseInterfaceUsage(testUsage(d, usage, nil), d)
	if err != nil {
		t.Fatal(err)
	}
	c, err := buildScopedFreehandBaseContract([]string{"package:app/main.dart"}, nil, d, u)
	if err != nil {
		t.Fatal(err)
	}
	table, _ := parseInterfaceUsage(testUsage(d, nil, []usageLibrary{
		{URI: "dart:core"}, {URI: "dart:collection"}, {URI: "dart:io"},
	}), d)
	spec, err := renderInterfaceValidationYAML(c, table)
	if err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(dir, "base_interface_validation.yaml")
	os.WriteFile(specPath, []byte(spec), 0o600)
	args := []string{d2b, "--target", "vm", "--platform", platform}

	run := func(name, src string) error {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(src), 0o600)
		return validateFreehandModuleAgainstBaseInterface(dartaot, args, specPath, p, p, dir)
	}
	if err := run("inside.dart", "void main() { final l = <int>[1, 2]; print(l.length + l.first); }\n"); err != nil {
		t.Fatalf("a module using only exposed classes must validate: %v", err)
	}
	// dart:io is outside the narrowed domain: whole, exactly as before.
	if err := run("io.dart", "import 'dart:io';\nvoid main() { print(Platform.numberOfProcessors); }\n"); err != nil {
		t.Fatalf("a module using a non-scoped library must validate: %v", err)
	}
	err = run("outside.dart", "import 'dart:collection';\nvoid main() { final m = SplayTreeMap<int, int>(); print(m.length); }\n")
	if err == nil || !strings.Contains(err.Error(), "SplayTreeMap which the installed base does not expose; ship a store release") {
		t.Fatalf("a module using an SDK class the base never used must be refused by name: %v", err)
	}
	if err := run("dyn.dart", "void main() { dynamic x = 1; print(x.isEven); }\n"); err != nil {
		t.Fatalf("a dynamic call alone is not an interface refusal: %v", err)
	}
}
