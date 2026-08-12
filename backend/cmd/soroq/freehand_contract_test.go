package main

import (
	"strings"
	"testing"
)

// Real dart2bytecode output from carrying a package whose class extends a dart:collection class.
const realExtendOutput = `
/tmp/x/_pkg/state_notifier/state_notifier.dart:330:13: Error: Cannot extend, implement or mix-in class 'LinkedListEntry' in a dynamic module.
Try removing the reference to class 'LinkedListEntry' or update the dynamic interface to list class 'LinkedListEntry' as extendable.
final class _ListenerEntry extends LinkedListEntry<_ListenerEntry> {
            ^
`

// Real output from the riverpod graph: the diagnostic points at the SDK patch file, so the responsible
// package must be recovered from the nearby carried-file reference.
const realOverrideOutput = `
/tmp/x/_pkg/collection/src/wrappers.dart:120:3: Context: carried library
/Users/x/.soroq/toolchains/t/dart-sdk/lib/_internal/vm/lib/object_patch.dart:21:26: Error: Cannot override member 'List.==' in a dynamic module.
/Users/x/.soroq/toolchains/t/dart-sdk/lib/_internal/vm/lib/object_patch.dart:29:19: Error: Cannot override member 'Iterable.toString' in a dynamic module.
`

func TestParseDynamicModuleViolations_ExtendNamesThePackage(t *testing.T) {
	vs := parseDynamicModuleViolations(realExtendOutput)
	if len(vs) != 1 {
		t.Fatalf("expected 1 violation, got %+v", vs)
	}
	if vs[0].Package != "state_notifier" || vs[0].Member != "LinkedListEntry" {
		t.Fatalf("violation must name the package and member, got %+v", vs[0])
	}
	msg := explainDynamicModuleViolations(vs)
	for _, want := range []string{"state_notifier", "LinkedListEntry", "dynamic interface", "App Store/Play Store release"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal must mention %q, got:\n%s", want, msg)
		}
	}
}

func TestParseDynamicModuleViolations_OverrideAttributedToCarriedPackage(t *testing.T) {
	vs := parseDynamicModuleViolations(realOverrideOutput)
	if len(vs) != 2 {
		t.Fatalf("expected 2 distinct violations, got %+v", vs)
	}
	for _, v := range vs {
		if v.Package != "collection" {
			t.Fatalf("an SDK-file diagnostic must still be attributed to the carried package, got %+v", v)
		}
	}
	msg := explainDynamicModuleViolations(vs)
	if !strings.Contains(msg, "List.==") || !strings.Contains(msg, "Iterable.toString") {
		t.Fatalf("both members must be named, got:\n%s", msg)
	}
	// The message must not tell the user to edit the dependency.
	if strings.Contains(strings.ToLower(msg), "edit the package") || strings.Contains(strings.ToLower(msg), "modify the package") {
		t.Fatalf("must not suggest editing the dependency:\n%s", msg)
	}
}

func TestParseDynamicModuleViolations_UnrelatedFailureNotMisattributed(t *testing.T) {
	if vs := parseDynamicModuleViolations("Error: something else entirely\nBad state: boom"); vs != nil {
		t.Fatalf("an unrelated compiler failure must not be reported as a contract violation: %+v", vs)
	}
}

func TestExplainDynamicModuleViolations_IsPackageAgnostic(t *testing.T) {
	// The same shape from a tiny unrelated package must produce the same class of refusal -- the rule is
	// about what the code does to base types, not which package it is.
	vs := parseDynamicModuleViolations(strings.ReplaceAll(realExtendOutput, "state_notifier", "tiny_pkg"))
	if len(vs) != 1 || vs[0].Package != "tiny_pkg" {
		t.Fatalf("expected the rule to apply identically to any package, got %+v", vs)
	}
}
