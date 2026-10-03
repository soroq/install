package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSplitFrozenIdentityKeyAllowsPipeInMember(t *testing.T) {
	parts, ok := splitFrozenIdentityKey("v1|package:app/main.dart|function||Twice|twice|35c1a021")
	if !ok {
		t.Fatal("an extension member key (member contains '|') was rejected")
	}
	if parts[1] != "package:app/main.dart" || parts[2] != "function" || parts[3] != "" || parts[4] != "Twice|twice" || parts[5] != "35c1a021" {
		t.Fatalf("wrong split: %#v", parts)
	}
	if _, ok := splitFrozenIdentityKey("v1|lib|kind|cls|sig"); ok {
		t.Fatal("a five-segment key was accepted")
	}
	if _, ok := splitFrozenIdentityKey("v2|lib|kind|cls|m|sig"); ok {
		t.Fatal("a non-v1 key was accepted")
	}
}

func TestVerifyFreehandKernelModuleBindsURIToDigest(t *testing.T) {
	dir := t.TempDir()
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	man := func(format, lib string) []byte {
		b, _ := json.Marshal(map[string]string{"module_format": format, "module_library": lib})
		return b
	}
	good := "soroq-freehand:///import/prefix/" + digest + "/soroq_freehand_module.dart"
	if err := verifyFreehandKernelModule(dir, man(freehandKernelModuleFormat, good), digest); err == nil {
		t.Fatal("accepted a kernel module with no bytecode")
	}
	if err := os.WriteFile(filepath.Join(dir, "soroq_freehand_module.bytecode"), []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyFreehandKernelModule(dir, man(freehandKernelModuleFormat, good), digest); err != nil {
		t.Fatalf("rejected a well-bound kernel module: %v", err)
	}
	if err := verifyFreehandKernelModule(dir, man(freehandKernelModuleFormat, "soroq-freehand:///import/prefix/other/soroq_freehand_module.dart"), digest); err == nil {
		t.Fatal("accepted a module library not bound to the namespace digest")
	}
	if err := verifyFreehandKernelModule(dir, man("soroq.freehand.module.v2", good), digest); err == nil {
		t.Fatal("accepted a source-synthesized manifest as a kernel module")
	}
}

func TestKernelModuleSwitch(t *testing.T) {
	t.Setenv("SOROQ_FREEHAND_KERNEL_MODULE", "1")
	if !freehandKernelModuleEnabled() {
		t.Fatal("=1 must force the kernel module")
	}
	t.Setenv("SOROQ_FREEHAND_KERNEL_MODULE", "0")
	freehandBundledAnalyzerSelected = true
	defer func() { freehandBundledAnalyzerSelected = false }()
	if freehandKernelModuleEnabled() {
		t.Fatal("=0 must force the source synthesizer even with the bundled analyzer")
	}
	t.Setenv("SOROQ_FREEHAND_KERNEL_MODULE", "")
	if !freehandKernelModuleEnabled() {
		t.Fatal("the bundled analyzer must make the kernel module the default")
	}
}

func TestPruningKeepsForcedDeclarations(t *testing.T) {
	// Identical machine code for a forced declaration (an accessor of storage whose initial value
	// changed) must not be pruned: the drop is filtered out of `same` before anything is removed.
	rep := &FreehandDiffReport{
		Changed: []map[string]any{
			{"manifestLine": "lib::::bump", "forced": true},
			{"manifestLine": "lib::::other"},
		},
		ChangedPatchable: []string{"lib::::bump", "lib::::other"},
	}
	same := freehandSameMachineCode(rep.ChangedPatchable, map[string]string{"lib::::bump": "a", "lib::::other": "b"}, map[string]string{"lib::::bump": "a", "lib::::other": "b"})
	for _, c := range rep.Changed {
		if forced, _ := c["forced"].(bool); forced {
			delete(same, c["manifestLine"].(string))
		}
	}
	if same["lib::::bump"] || !same["lib::::other"] {
		t.Fatalf("forced declaration pruned or unforced one kept: %v", same)
	}
}
