package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ENGINE R12: FRAMEWORK CODE A PATCH INVALIDATES SHIPS WITH THE PATCH.
//
// The base's framework code was compiled with type-flow analysis's facts about the whole BASE program (a
// field that only ever held one value is a constant in every reader; a callback field that held one
// tear-off is called directly). A patch that changes the program can make those facts false, and the base's
// compiled code keeps acting on them. Shorebird recompiles the whole program and ships the machine code
// that changed. Soroq does the equivalent with the candidate program it already builds at patch time:
//
//   - release: gen_snapshot (--soroq_callgraph, engine capability soroq_callgraph_v1) records every call
//     edge of the code it compiled -- build metadata only, the snapshot is byte-identical with or without
//     it -- and the baseline binds the canonical file by hash;
//   - plan: the analyzer (--r12-plan) compares the type-flow facts of the base's and the candidate's AOT
//     kernels and climbs the call graph from every member that compiled differently. App members on the
//     way are redirected like changed declarations (added to the diff here, forced so machine-code pruning
//     keeps them); framework members are carried into the module; the framework members the base reaches
//     only through calls the engine resolves at run time are SWAPPED (`swap:` ABI entries, engine
//     capability soroq_entry_swap_v1: the engine points the base function at the module's copy).
//
// A base without the call graph keeps R11's rules: the analyzer refuses a write the base's facts forbid.
const (
	freehandCallGraphCapability = "soroq_callgraph_v1"
	freehandCallGraphFlag       = "--soroq_callgraph"
	freehandCallGraphFile       = "soroq_callgraph.tsv"
	freehandCallGraphSchema     = "soroq.callgraph.v1"
	freehandEntrySwapCapability = "soroq_entry_swap_v1"
	// R13: dispatch-table calls on classes a patch defines are resolved by name (engine dispatch fallback).
	freehandDispatchFallbackCapability = "soroq_dispatch_fallback_v1"
	// R14: dart:_internal.soroqSentinel / soroqIsSentinel (module-side late fields keep late semantics).
	freehandLateSentinelCapability = "soroq_late_sentinel_v1"
	// R15: the bytecode loader resolves a library the precompiler dropped from the library list through the
	// class table (a patch may name classes of libraries nothing named through the embedder API).
	freehandLibraryLookupCapability = "soroq_library_lookup_v1"
	// R16: the bytecode loader resolves a direct-call target the precompiler pruned from its class's
	// function array through the compiled code it kept.
	freehandCompiledFunctionLookupCapability = "soroq_compiled_function_lookup_v1"
	// R17: the compiled-function index covers every function the program snapshot compiled (heap walk).
	freehandCompiledFunctionLookupV2Capability = "soroq_compiled_function_lookup_v2"
	// R18: bytecode calls compiled functions using the register calling convention (adapter stub).
	freehandRegisterCallAdapterCapability = "soroq_register_call_adapter_v1"
	// R19: the interpreter calls a patch's new FFI natives (and the FFI intrinsics AOT code inlines) on the
	// AOT runtime: `soroq-ffi|...` / `soroq-ffi-op|...` externals.
	freehandBytecodeFfiCapability = "soroq_bytecode_ffi_v1"
	// R20: the interpreter runs the VM intrinsics AOT code inlines (`soroq-op|...`) and resolves a platform
	// native whose function the precompiler pruned (`soroq-native|...`).
	freehandIntrinsicOpsCapability = "soroq_intrinsic_ops_v1"
	// R23: the dispatch fallback takes the arguments a table call passes in registers (the base call graph's
	// selector lines carry the count).
	freehandDispatchRegistersCapability = "soroq_dispatch_registers_v1"
	freehandR12PlanSchema               = "soroq.r12_plan.v1"
	freehandSwapKindPrefix              = "swap:"
)

var (
	callGraphWrittenRe = regexp.MustCompile(`^# written=([0-9]+)$`)
	// R49: a capability line of the engine that wrote the graph.
	callGraphCapabilityPrefix = "# soroq.capability "
	callGraphKinds            = map[string]bool{"direct": true, "table": true, "ic": true, "closure": true, "inline": true}
	// R13: `selector  <offset>|<argc or -1>|<unboxed 0/1>  <interface target>` (the dispatch fallback);
	// R23 bases add `|<registers>` (leading arguments the call passes in registers).
	// R23 adds `|<registers>`; R29 adds `|<param reps>:<return rep>|<registers uncapped>` to an unboxed selector.
	callGraphSelectorRe = regexp.MustCompile(`^[0-9]+\|-?[0-9]+\|[01](\|[0-9]+(\|[tid]*:[tid]\|[0-9]+)?)?$`)
)

// parseFreehandCallGraph reads gen_snapshot's file or its canonical rendering: the schema line, edges
// `<kind>\t<lib>|<class>|<name>\t<lib>|<class>|<name>`, and a `# written=N` trailer equal to the number of
// edge lines (a truncated file never passes for a complete one). It returns the distinct edges, sorted.
func parseFreehandCallGraph(raw []byte) ([]string, error) {
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) < 2 || lines[0] != "# "+freehandCallGraphSchema {
		return nil, fmt.Errorf("the call graph does not start with the %s schema line", freehandCallGraphSchema)
	}
	m := callGraphWrittenRe.FindStringSubmatch(lines[len(lines)-1])
	if m == nil {
		return nil, errors.New("the call graph has no written= trailer (truncated?)")
	}
	written, _ := strconv.Atoi(m[1])
	body := lines[1 : len(lines)-1]
	// R49: `# soroq.capability <name>` lines declare what the engine that wrote the graph can do; kept (they
	// sort first), never counted as edges.
	edges := 0
	for _, l := range body {
		if !strings.HasPrefix(l, callGraphCapabilityPrefix) {
			edges++
		}
	}
	if written != edges {
		return nil, fmt.Errorf("the call graph trailer says %d edges but %d were read", written, edges)
	}
	seen := make(map[string]bool, len(body))
	for i, l := range body {
		if strings.HasPrefix(l, callGraphCapabilityPrefix) {
			seen[l] = true
			continue
		}
		p := strings.Split(l, "\t")
		if len(p) == 3 && p[0] == "selector" && callGraphSelectorRe.MatchString(p[1]) && strings.Count(p[2], "|") >= 2 {
			seen[l] = true
			continue
		}
		if len(p) != 3 || !callGraphKinds[p[0]] || strings.Count(p[1], "|") < 2 || strings.Count(p[2], "|") < 2 {
			return nil, fmt.Errorf("call graph line %d is malformed: %q", i+2, l)
		}
		seen[l] = true
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out, nil
}

// renderFreehandCallGraph is the canonical form persisted in a baseline (distinct edges, sorted).
func renderFreehandCallGraph(edges []string) []byte {
	var b bytes.Buffer
	b.WriteString("# " + freehandCallGraphSchema + "\n")
	n := 0
	for _, e := range edges {
		b.WriteString(e + "\n")
		if !strings.HasPrefix(e, callGraphCapabilityPrefix) {
			n++
		}
	}
	fmt.Fprintf(&b, "# written=%d\n", n)
	return b.Bytes()
}

func toolchainDeclaresCallGraph(toolchain string) (bool, error) {
	if strings.TrimSpace(toolchain) == "" {
		return false, nil
	}
	bundleDir, err := iosCachedToolchainBundleDir(toolchain)
	if err != nil {
		return false, nil
	}
	enginePath := filepath.Join(bundleDir, "engine.json")
	if _, err := os.Stat(enginePath); os.IsNotExist(err) {
		return false, nil
	}
	return engineBundleDeclaresIdentityCapability(enginePath, freehandCallGraphCapability)
}

// withFreehandCallGraph is the release-side wiring: on a toolchain that declares the capability it appends
// --extra-gen-snapshot-options=--soroq_callgraph=<abs path>.
func withFreehandCallGraph(projectDir, toolchain string, passthrough []string) ([]string, string, bool, error) {
	if strings.Contains(strings.Join(passthrough, " "), "soroq_callgraph") {
		return nil, "", false, fmt.Errorf("the build arguments already pass %s; Soroq sets it itself", freehandCallGraphFlag)
	}
	declared, err := toolchainDeclaresCallGraph(toolchain)
	if err != nil {
		return nil, "", false, fmt.Errorf("read the toolchain's call-graph capability: %w", err)
	}
	if !declared {
		return passthrough, "", false, nil
	}
	path, err := freehandCodeFingerprintsStagingPath(projectDir, freehandCallGraphFile)
	if err != nil {
		return nil, "", false, err
	}
	out := append(append([]string(nil), passthrough...), "--extra-gen-snapshot-options="+freehandCallGraphFlag+"="+path)
	return out, path, true, nil
}

// collectFreehandCallGraph reads the file THIS build's gen_snapshot wrote and returns its canonical form.
func collectFreehandCallGraph(path string, buildStart time.Time) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("the build produced no call graph at %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("the call graph at %s is not a regular file", path)
	}
	if fi.ModTime().Before(buildStart.Add(-2 * time.Second)) {
		return nil, fmt.Errorf("the call graph at %s predates this build; refusing to adopt a stale file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	edges, err := parseFreehandCallGraph(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(edges) == 0 {
		return nil, fmt.Errorf("the call graph at %s lists no edges", path)
	}
	return renderFreehandCallGraph(edges), nil
}

// bindFreehandCallGraph records the canonical call graph onto meta: required iff the engine declares the
// capability, forbidden otherwise (the same both-directions rule as the field layout).
func bindFreehandCallGraph(meta *FreehandBaselineMeta, capabilities *FreehandRedirectCapabilities, graph []byte) error {
	meta.CallGraphSchema, meta.CallGraphSHA256, meta.CallGraphEdges = "", "", 0
	want := capabilities.hasIdentityCapability(freehandCallGraphCapability)
	have := graph != nil
	if want && !have {
		return fmt.Errorf("refusing to persist a baseline built by engine %s, which declares %s, without the call graph its build recorded", meta.EngineRev, freehandCallGraphCapability)
	}
	if !want && have {
		return fmt.Errorf("refusing to persist a call graph into a baseline whose engine %s does not declare %s", meta.EngineRev, freehandCallGraphCapability)
	}
	if !have {
		return nil
	}
	edges, err := parseFreehandCallGraph(graph)
	if err != nil {
		return fmt.Errorf("refusing to persist a malformed call graph: %w", err)
	}
	if !bytes.Equal(renderFreehandCallGraph(edges), graph) {
		return errors.New("refusing to persist a call graph that is not in canonical form")
	}
	meta.CallGraphSchema = freehandCallGraphSchema
	meta.CallGraphSHA256 = freehandSHA256Bytes(graph)
	meta.CallGraphEdges = len(edges)
	return nil
}

// verifiedBaselineCallGraphPath checks the file in BOTH directions and returns its path, or "" for a base
// whose engine does not declare the capability.
func verifiedBaselineCallGraphPath(relDir string, m *FreehandBaselineMeta) (string, error) {
	caps, err := baseRedirectCapabilities(m)
	if err != nil {
		return "", err
	}
	p := filepath.Join(relDir, freehandCallGraphFile)
	if !caps.hasIdentityCapability(freehandCallGraphCapability) {
		if m.CallGraphSchema != "" || m.CallGraphSHA256 != "" || m.CallGraphEdges != 0 {
			return "", fmt.Errorf("baseline %s records a call graph but its engine does not declare %s", relDir, freehandCallGraphCapability)
		}
		if _, err := os.Lstat(p); err == nil {
			return "", fmt.Errorf("baseline %s contains %s but its engine does not declare %s", relDir, freehandCallGraphFile, freehandCallGraphCapability)
		}
		return "", nil
	}
	if m.CallGraphSchema != freehandCallGraphSchema || !sha256HexRe.MatchString(m.CallGraphSHA256) {
		return "", fmt.Errorf("baseline %s was built by an engine declaring %s but records no valid call graph", relDir, freehandCallGraphCapability)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("baseline built with %s is missing %s: %w", freehandCallGraphCapability, freehandCallGraphFile, err)
	}
	if got := freehandSHA256Bytes(raw); got != m.CallGraphSHA256 {
		return "", fmt.Errorf("baseline %s hash mismatch: %s != recorded %s", freehandCallGraphFile, got, m.CallGraphSHA256)
	}
	edges, err := parseFreehandCallGraph(raw)
	if err != nil {
		return "", fmt.Errorf("baseline %s: %w", freehandCallGraphFile, err)
	}
	if !bytes.Equal(renderFreehandCallGraph(edges), raw) || len(edges) != m.CallGraphEdges {
		return "", fmt.Errorf("baseline %s is not in canonical form or does not match its recorded count", freehandCallGraphFile)
	}
	return p, nil
}

// ---------------------------------------------------------------------------------------------
// PATCH SIDE
// ---------------------------------------------------------------------------------------------

// freehandR12 is the R12 state of the patch being built (package state, like freehandFieldLayoutPath):
// the verified base call graph, the candidate's AOT kernel, and the plan's swap set the ABI is checked
// against.
var freehandR12 struct {
	CallGraphPath string
	CandidateAOT  string
	Active        bool
	Swaps         map[string]bool // base identities (manifest lines) the module swaps
	EntrySwap     bool            // the base's engine declares soroq_entry_swap_v1
	// Replan runs the plan again with the classes the synthesis found the module copies (its manifest's
	// r12_copied_classes): the plan the module was built with, whose swaps the ABI check then holds it to.
	Replan func(copied []string) error
}

func resetFreehandR12() {
	freehandR12.CallGraphPath, freehandR12.CandidateAOT = "", ""
	freehandR12.Active, freehandR12.EntrySwap = false, false
	freehandR12.Swaps = nil
	freehandR12.Replan = nil
}

// stageFreehandCandidateAOTKernel keeps the candidate build's AOT kernel (the build directory is reused by
// later builds) for the R12 plan and synthesis.
func stageFreehandCandidateAOTKernel(projectDir, appDill string) error {
	if appDill == "" {
		return nil
	}
	dst, err := freehandCodeFingerprintsStagingPath(projectDir, "candidate_app.dill")
	if err != nil {
		return err
	}
	in, err := os.Open(appDill)
	if err != nil {
		return fmt.Errorf("keep the candidate AOT kernel: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	freehandR12.CandidateAOT = dst
	return nil
}

type freehandR12Plan struct {
	Schema        string           `json:"schema"`
	Active        bool             `json:"active"`
	Seeds         int              `json:"seeds"`
	Carried       int              `json:"carried"`
	Swapped       int              `json:"swapped"`
	ChangedFields int              `json:"changed_fields"`
	Redirect      []map[string]any `json:"redirect"`
	Swap          []string         `json:"swap"`
	Refusals      []string         `json:"refusals"`
}

// applyFreehandR12Plan runs the analyzer's R12 plan for a base with a call graph and folds its result
// into the diff: app members the ripple reaches are added to the changed set (forced: their machine code
// may be identical while the framework code they call is not), and the swap set is kept for the ABI check.
// A base without the capability changes nothing.
// extraArgs are the analyzer inputs synthesis also gets (--new-packages, --capability-map): they decide which
// declarations are app code, so the plan and the module builder must see the same ones.
func applyFreehandR12Plan(dart, analyzer, candSourceDill, packageConfig, relDir string, rep *FreehandDiffReport, diffJSONPath string,
	extraArgs ...string) (*freehandR12Plan, error) {
	if freehandR12.CallGraphPath == "" {
		return nil, nil
	}
	if freehandR12.CandidateAOT == "" {
		return nil, errors.New("the base records a call graph (engine R12) but the candidate was not built, so the framework " +
			"code this patch invalidates cannot be determined")
	}
	plan, err := runFreehandR12Plan(dart, analyzer, candSourceDill, packageConfig, relDir, diffJSONPath, extraArgs...)
	if err != nil {
		return plan, err
	}
	have := make(map[string]bool, len(rep.Changed))
	for _, c := range rep.Changed {
		if ml, _ := c["manifestLine"].(string); ml != "" {
			have[ml] = true
		}
	}
	added := []string{}
	for _, d := range plan.Redirect {
		ml, _ := d["manifestLine"].(string)
		if ml == "" {
			return plan, errors.New("the R12 plan lists a redirect without a manifest line")
		}
		if have[ml] {
			continue
		}
		have[ml] = true
		rep.Changed = append(rep.Changed, d)
		rep.ChangedPatchable = append(rep.ChangedPatchable, ml)
		added = append(added, ml)
	}
	freehandR12.Active = true
	freehandR12.Swaps = make(map[string]bool, len(plan.Swap))
	for _, s := range plan.Swap {
		freehandR12.Swaps[s] = true
	}
	freehandR12.Replan = func(copied []string) error {
		again, err := runFreehandR12Plan(dart, analyzer, candSourceDill, packageConfig, relDir, diffJSONPath,
			append(append([]string{}, extraArgs...), "--r12-copied", strings.Join(copied, ","))...)
		if err != nil {
			return err
		}
		// The diff the module was synthesized from is final: a redirect only the copies call for cannot be added.
		for _, d := range again.Redirect {
			if ml, _ := d["manifestLine"].(string); !have[ml] {
				return fmt.Errorf("the classes the module copies call for redirecting %s, which the patch's diff does not", ml)
			}
		}
		freehandR12.Swaps = make(map[string]bool, len(again.Swap))
		for _, s := range again.Swap {
			freehandR12.Swaps[s] = true
		}
		fmt.Fprintf(os.Stdout, "framework ripple (R12): the module copies %d class(es) base code reads the fields of; %d carried, "+
			"%d swapped\n", len(copied), again.Carried, again.Swapped)
		return nil
	}
	if len(added) > 0 {
		sort.Strings(rep.ChangedPatchable)
		if rep.Counts == nil {
			rep.Counts = map[string]int{}
		}
		rep.Counts["changed"] = len(rep.Changed)
		rep.Counts["changedPatchable"] = len(rep.ChangedPatchable)
		rep.Counts["r12Redirected"] = len(added)
		if err := rewriteFreehandDiffJSON(diffJSONPath, rep); err != nil {
			return plan, fmt.Errorf("rewrite the diff after the R12 plan: %w", err)
		}
	}
	fmt.Fprintf(os.Stdout, "framework ripple (R12): %d member(s) compiled differently; %d carried, %d swapped, %d app member(s) redirected\n",
		plan.Seeds, plan.Carried, plan.Swapped, len(added))
	return plan, nil
}

// runFreehandR12Plan runs the analyzer's R12 plan (--r12-plan) and checks what it says it cannot deliver.
func runFreehandR12Plan(dart, analyzer, candSourceDill, packageConfig, relDir, diffJSONPath string,
	extraArgs ...string) (*freehandR12Plan, error) {
	out, err := os.CreateTemp("", "soroq-r12-plan-*.json")
	if err != nil {
		return nil, err
	}
	out.Close()
	defer os.Remove(out.Name())
	args := []string{analyzer, "--r12-plan", "--dill", candSourceDill, "--package-config", packageConfig,
		"--aot-dill", filepath.Join(relDir, "app.dill"), "--cand-aot-dill", freehandR12.CandidateAOT,
		"--callgraph", freehandR12.CallGraphPath, "--diff-json", diffJSONPath, "--r12-out", out.Name()}
	if graph := filepath.Join(relDir, freehandObjectGraphName); fileExists(graph) {
		args = append(args, "--aot-graph", graph)
	}
	args = append(args, extraArgs...)
	cmd := exec.Command(dart, args...)
	log, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("the R12 plan failed: %w\n%s", err, string(log))
	}
	if os.Getenv("SOROQ_KMB_TRACE") == "1" {
		// The analyzer's trace (seeds, the facts that changed) is kept for diagnosis.
		_ = os.WriteFile(filepath.Join(os.TempDir(), "soroq-r12-plan-trace.log"), log, 0o600)
	}
	raw, err := os.ReadFile(out.Name())
	if err != nil {
		return nil, err
	}
	var plan freehandR12Plan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("the R12 plan is malformed: %w", err)
	}
	if plan.Schema != freehandR12PlanSchema {
		return nil, fmt.Errorf("the R12 plan has schema %q, want %q", plan.Schema, freehandR12PlanSchema)
	}
	if !plan.Active {
		return nil, errors.New("the analyzer could not compute the R12 plan (base or candidate AOT kernel, or call graph, missing)")
	}
	if len(plan.Refusals) > 0 {
		return &plan, fmt.Errorf("the patch changes what framework code the base compiled, in a way that cannot be delivered yet:\n  - %s",
			strings.Join(plan.Refusals, "\n  - "))
	}
	if len(plan.Swap) > 0 && !freehandR12.EntrySwap {
		return &plan, fmt.Errorf("the patch must replace %d framework function(s) the base reaches at run time, and the base's "+
			"engine does not declare %s:\n  - %s", len(plan.Swap), freehandEntrySwapCapability, strings.Join(plan.Swap, "\n  - "))
	}
	return &plan, nil
}

// withFreehandR12 appends the synthesis flags when the plan ran.
func withFreehandR12(args []string) []string {
	if !freehandR12.Active {
		return args
	}
	return append(args, "--callgraph", freehandR12.CallGraphPath, "--cand-aot-dill", freehandR12.CandidateAOT)
}

// validateFreehandSwapEntry checks one `swap:` ABI entry: the base engine swaps entries, the plan named
// this identity, and the kind after the prefix is one the runtime knows.
func validateFreehandSwapEntry(e freehandABIEntryView) error {
	if !freehandR12.Active || !freehandR12.EntrySwap {
		return fmt.Errorf("replacement_abi entry %s swaps a base function, which this base cannot do", e.BaseIdentity)
	}
	if !freehandR12.Swaps[e.BaseIdentity] {
		return fmt.Errorf("replacement_abi entry %s swaps a function the R12 plan did not name", e.BaseIdentity)
	}
	if !abiKinds[strings.TrimPrefix(e.Kind, freehandSwapKindPrefix)] {
		return fmt.Errorf("replacement_abi entry %s has unknown kind %q", e.BaseIdentity, e.Kind)
	}
	return nil
}

// freehandABIEntryView is the part of an ABI entry the swap check reads.
type freehandABIEntryView struct {
	BaseIdentity string
	Kind         string
}
