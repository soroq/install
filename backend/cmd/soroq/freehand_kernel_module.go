package main

// KERNEL-LEVEL MODULE (analyzer --synthesize --kernel-module).
//
// The source synthesizer recompiles changed declarations from Dart text in a separate library, which the
// host harness measured losing the program's meaning (old const values, copies of the patched class,
// unreachable private members, emitted text that does not compile). The kernel-level builder instead
// moves changed and new code out of the candidate kernel into the module library and links every other
// reference to the BASE declaration by canonical name, then compiles the module itself with
// dart2bytecode's generator. See backend/tools/soroq_kernel_analyze/bin/module_builder.dart.
//
// What the CLI still verifies is unchanged in spirit: the module's bytes are bound by sha into the
// manifest, and the module's VM library URI is bound to the namespace digest the manifest declares.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	freehandKernelModuleFormat = "soroq.freehand.kernel_module.v1"
	freehandKernelModuleFile   = "soroq_freehand_module.dill"
)

// freehandKernelModuleEnabled selects the kernel-level builder. SOROQ_FREEHAND_KERNEL_MODULE=1 forces it
// and =0 forces the source synthesizer. Otherwise it is on whenever the analyzer the patch runs is the
// one bundled beside this CLI (the frontends' own analyzers predate it).
func freehandKernelModuleEnabled() bool {
	switch strings.TrimSpace(os.Getenv("SOROQ_FREEHAND_KERNEL_MODULE")) {
	case "1":
		return true
	case "0":
		return false
	}
	return freehandBundledAnalyzerSelected
}

// freehandBundledAnalyzerSelected is set when the patch analyzer resolved to the CLI-bundled one.
var freehandBundledAnalyzerSelected bool

// verifyFreehandKernelModule checks what the source path checks by recomputing the graph digest from
// emitted sources: that the manifest describes THIS module. The kernel module's URI embeds the
// namespace digest, so the manifest's module_library must be exactly that URI, and the bytecode the
// analyzer produced must exist beside it.
func verifyFreehandKernelModule(synthOut string, manifestBytes []byte, graphDigest string) error {
	var m struct {
		ModuleFormat  string `json:"module_format"`
		ModuleLibrary string `json:"module_library"`
	}
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return fmt.Errorf("decode the kernel module manifest: %w", err)
	}
	if m.ModuleFormat != freehandKernelModuleFormat {
		return fmt.Errorf("module manifest format %q is not %q; the installed analyzer did not build a kernel module", m.ModuleFormat, freehandKernelModuleFormat)
	}
	want := "soroq-freehand:///import/prefix/" + graphDigest + "/soroq_freehand_module.dart"
	if m.ModuleLibrary != want {
		return fmt.Errorf("kernel module library %q is not bound to its namespace digest (want %q)", m.ModuleLibrary, want)
	}
	if !fileExists(filepath.Join(synthOut, "soroq_freehand_module.bytecode")) {
		return fmt.Errorf("the kernel module builder produced no bytecode")
	}
	return nil
}
