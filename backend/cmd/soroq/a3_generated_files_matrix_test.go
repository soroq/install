package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A3 — GENERATED FILES AND `part` FILES, against the real analyzer.
//
// This was the last UNTESTED row in the T011 Dart-language section, and "untested" was the dangerous
// state: the shape is ACCEPTED, so a wrong answer here does not refuse — it ships a patch bound to
// the wrong library. Code generation (json_serializable, freezed, build_runner) is close to universal
// in real Flutter apps, so this is not an exotic corner.
//
// The specific hazard is `part`. A part file is not a library: it has no package: URI of its own and
// its declarations belong to the library that declares the `part` directive. An implementation that
// keyed changes off the FILE a declaration was written in would attribute a change in models.g.dart
// to "models.g.dart", which no library imports, and the redirect would bind to nothing.
//
// These drive the PRODUCTION path — generateFreehandSourceKernel and runFreehandAnalyzerDiff, the same
// functions `soroq patch ios --engine` calls — against real compiled kernels. Nothing is mocked.
//
// REQUIRES an installed Soroq frontend, so it is skipped by default and `go test ./...` stays
// hermetic. Run it with:
//
//	SOROQ_FREEHAND_FRONTEND=~/.soroq/frontends/<id>/flutter-sdk-src go test ./cmd/soroq -run TestA3 -v

const a3FrontendEnv = "SOROQ_FREEHAND_FRONTEND"

func a3FlutterRoot(t *testing.T) string {
	t.Helper()
	root := strings.TrimSpace(os.Getenv(a3FrontendEnv))
	if root == "" {
		t.Skipf("set %s to an installed frontend (…/flutter-sdk-src) to run the A3 matrix", a3FrontendEnv)
	}
	for _, rel := range []string{
		filepath.Join("bin", "cache", "soroq", "soroq_kernel_analyze.dill"),
		filepath.Join("bin", "cache", "dart-sdk", "bin", "dart"),
		filepath.Join("bin", "cache", "dart-sdk", "bin", "dartaotruntime"),
	} {
		if !fileExists(filepath.Join(root, rel)) {
			t.Fatalf("%s=%s is not a usable frontend: missing %s", a3FrontendEnv, root, rel)
		}
	}
	return root
}

// a3Recipe is the minimal source-kernel recipe: the same compiler, platform dill and flags the
// freehand release path uses, so the kernels the analyzer sees are the kernels production produces.
func a3Recipe() FreehandSourceKernelRecipe {
	return FreehandSourceKernelRecipe{
		Schema:          freehandRecipeSchemaV2,
		Entrypoint:      "lib/main.dart",
		Target:          "flutter",
		BuildMode:       "profile",
		PlatformDillRel: filepath.Join("bin", "cache", "artifacts", "engine", "common", "flutter_patched_sdk_product", "platform_strong.dill"),
	}
}

// a3Project writes a fixture package. Files are relpaths under the project.
func a3Project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"lib", ".dart_tool"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	writeFile(t, filepath.Join(dir, "pubspec.yaml"),
		"name: a3fix\nversion: 1.0.0+1\nenvironment:\n  sdk: \">=3.0.0 <4.0.0\"\n")
	writeFile(t, filepath.Join(dir, ".dart_tool", "package_config.json"),
		`{"configVersion":2,"packages":[{"name":"a3fix","rootUri":"../","packageUri":"lib/","languageVersion":"3.0"}]}`)
	for rel, body := range files {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), body)
	}
	return dir
}

// a3Run is one complete base->candidate run: the project, the compiled kernels, the diff report and
// the paths the production synthesis step consumes.
type a3Run struct {
	dir      string
	candDill string
	diffJSON string
	rep      *FreehandDiffReport
}

// a3Diff keeps the original signature for the cases that only need the report.
func a3Diff(t *testing.T, root string, base map[string]string, mutate map[string]string) *FreehandDiffReport {
	t.Helper()
	return a3DiffRun(t, root, base, mutate).rep
}

// a3DiffRun compiles base, applies the mutation, compiles the candidate, and returns the production diff.
func a3DiffRun(t *testing.T, root string, base map[string]string, mutate map[string]string) *a3Run {
	t.Helper()
	dir := a3Project(t, base)
	baseDill := filepath.Join(dir, "base.dill")
	if _, err := generateFreehandSourceKernel(dir, root, a3Recipe(), baseDill); err != nil {
		t.Fatalf("compile base source kernel: %v", err)
	}
	for rel, body := range mutate {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), body)
	}
	candDill := filepath.Join(dir, "cand.dill")
	if _, err := generateFreehandSourceKernel(dir, root, a3Recipe(), candDill); err != nil {
		t.Fatalf("compile candidate source kernel: %v", err)
	}
	outDir := filepath.Join(dir, "out")
	rep, err := runFreehandAnalyzerDiff(root, baseDill, candDill,
		filepath.Join(dir, ".dart_tool", "package_config.json"), outDir, "")
	if err != nil {
		t.Fatalf("analyzer diff: %v", err)
	}
	return &a3Run{dir: dir, candDill: candDill, diffJSON: filepath.Join(outDir, "freehand_diff.json"), rep: rep}
}

// a3ModuleManifest is the subset of the synthesized ABI manifest these assertions read.
type a3ModuleManifest struct {
	Schema         string   `json:"schema"`
	ModuleLibrary  string   `json:"module_library"`
	Imports        []string `json:"imports"`
	ReplacementABI []struct {
		BaseIdentity   string `json:"base_identity"`
		StableIdentity string `json:"stable_identity"`
		ModuleLibrary  string `json:"module_library"`
		ModuleMember   string `json:"module_member"`
		Kind           string `json:"kind"`
		HostInvocable  bool   `json:"host_invocable"`
	} `json:"replacement_abi"`
	ValueExercised     []string `json:"value_exercised"`
	NeedsFlutterTarget bool     `json:"needs_flutter_target"`
}

// a3Synthesize runs the PRODUCTION synthesis step with the same analyzer invocation
// generateAndPersistFreehandModule uses, and returns the module source plus its ABI manifest.
func a3Synthesize(t *testing.T, root string, run *a3Run) (string, a3ModuleManifest) {
	t.Helper()
	out := filepath.Join(run.dir, "synth")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir synth: %v", err)
	}
	dart := filepath.Join(root, "bin", "cache", "dart-sdk", "bin", "dart")
	analyzer := filepath.Join(root, "bin", "cache", "soroq", "soroq_kernel_analyze.dill")
	cmd := exec.Command(dart, analyzer, "--synthesize",
		"--diff-json", run.diffJSON,
		"--dill", run.candDill,
		"--package-config", filepath.Join(run.dir, ".dart_tool", "package_config.json"),
		"--out", out)
	if log, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("module synthesis failed: %v\n%s", err, log)
	}
	raw, err := os.ReadFile(filepath.Join(out, "soroq_freehand_module_manifest.json"))
	if err != nil {
		t.Fatalf("read synth manifest: %v", err)
	}
	var man a3ModuleManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("parse synth manifest: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(out, "soroq_freehand_module.dart"))
	if err != nil {
		t.Fatalf("read synth module: %v", err)
	}
	return string(src), man
}

// a3ExecuteModule compiles and RUNS the synthesized module on this host and returns what its
// dyn-module entrypoint produced. This is the value-level proof: the analyzer agreeing with itself is
// not evidence that the extracted code computes the new answer.
func a3ExecuteModule(t *testing.T, root string, run *a3Run, moduleSrc string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(run.dir, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeFile(t, filepath.Join(run.dir, "lib", "soroq_module_probe.dart"), moduleSrc)
	writeFile(t, filepath.Join(run.dir, "bin", "probe.dart"),
		"import 'package:a3fix/soroq_module_probe.dart' as m;\n"+
			"void main() { print('PROBE:' + m.dynamicModuleEntrypoint().toString()); }\n")
	cmd := exec.Command(filepath.Join(root, "bin", "cache", "dart-sdk", "bin", "dart"),
		"run", "--packages=.dart_tool/package_config.json", "bin/probe.dart")
	cmd.Dir = run.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("executing the synthesized module failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "PROBE:") {
			return strings.TrimPrefix(line, "PROBE:")
		}
	}
	t.Fatalf("the module produced no probe output:\n%s", out)
	return ""
}

// a3ChangedLibraries returns the set of library URIs the diff attributed changes to.
func a3ChangedLibraries(rep *FreehandDiffReport) map[string]bool {
	out := map[string]bool{}
	for _, c := range rep.Changed {
		line, _ := c["manifestLine"].(string)
		if i := strings.Index(line, "::"); i > 0 {
			out[line[:i]] = true
		}
	}
	return out
}

func a3ChangedNames(rep *FreehandDiffReport) []string {
	var out []string
	for _, c := range rep.Changed {
		line, _ := c["manifestLine"].(string)
		if i := strings.LastIndex(line, "::"); i >= 0 {
			out = append(out, line[i+2:])
		}
	}
	return out
}

func a3RequireLibraries(t *testing.T, rep *FreehandDiffReport, want ...string) {
	t.Helper()
	got := a3ChangedLibraries(rep)
	for _, w := range want {
		if !got[w] {
			t.Errorf("expected a change attributed to %s; changed libraries were %v (names %v)",
				w, a3KeysOf(got), a3ChangedNames(rep))
		}
	}
	if len(got) != len(want) {
		t.Errorf("expected exactly %d changed libraries %v, got %v", len(want), want, a3KeysOf(got))
	}
}

func a3KeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------

const a3MainUsesModel = "import 'models.dart';\nvoid main() { print(Model().value); }\n"

// THE core case: a change in a generated `part` must be attributed to the OWNING library. A part file
// has no package: URI, so attributing it to itself would produce a redirect bound to nothing.
func TestA3ChangedPartFileIsAttributedToItsOwningLibrary(t *testing.T) {
	root := a3FlutterRoot(t)
	rep := a3Diff(t, root, map[string]string{
		"lib/main.dart":     a3MainUsesModel,
		"lib/models.dart":   "part 'models.g.dart';\nclass Model { int get value => generatedValue(); }\n",
		"lib/models.g.dart": "part of 'models.dart';\nint generatedValue() => 1;\n",
	}, map[string]string{
		"lib/models.g.dart": "part of 'models.dart';\nint generatedValue() => 2;\n",
	})

	if !rep.Supported {
		t.Fatalf("a body change in a generated part must be patchable; blockers: %v", rep.Blockers)
	}
	a3RequireLibraries(t, rep, "package:a3fix/models.dart")
	for _, c := range rep.Changed {
		if line, _ := c["manifestLine"].(string); strings.Contains(line, "models.g.dart") {
			t.Errorf("a part file must never appear as a library identity: %s", line)
		}
	}
}

// A generated LIBRARY (import, not part) changing alone must not drag its importers in.
func TestA3ChangedGeneratedLibraryDoesNotPullInTheUnchangedImporter(t *testing.T) {
	root := a3FlutterRoot(t)
	rep := a3Diff(t, root, map[string]string{
		"lib/main.dart":    "import 'codegen.dart';\nvoid main() { print(generated()); }\n",
		"lib/codegen.dart": "int generated() => 1;\n",
	}, map[string]string{
		"lib/codegen.dart": "int generated() => 2;\n",
	})

	if !rep.Supported {
		t.Fatalf("a generated-library body change must be patchable; blockers: %v", rep.Blockers)
	}
	a3RequireLibraries(t, rep, "package:a3fix/codegen.dart")
	if a3ChangedLibraries(rep)["package:a3fix/main.dart"] {
		t.Error("an UNCHANGED importer must not be reported as changed; that would patch code the " +
			"developer did not touch")
	}
}

// Generated code and the app code that imports it changing together: both must be reported.
func TestA3GeneratedLibraryAndItsImporterChangingTogetherAreBothReported(t *testing.T) {
	root := a3FlutterRoot(t)
	rep := a3Diff(t, root, map[string]string{
		"lib/main.dart":    "import 'codegen.dart';\nint appValue() => generated();\nvoid main() { print(appValue()); }\n",
		"lib/codegen.dart": "int generated() => 1;\n",
	}, map[string]string{
		"lib/main.dart":    "import 'codegen.dart';\nint appValue() => generated() + 1;\nvoid main() { print(appValue()); }\n",
		"lib/codegen.dart": "int generated() => 2;\n",
	})

	if !rep.Supported {
		t.Fatalf("both changes are ordinary body changes; blockers: %v", rep.Blockers)
	}
	a3RequireLibraries(t, rep, "package:a3fix/codegen.dart", "package:a3fix/main.dart")
}

// PRIVATE DECLARATIONS IN GENERATED CODE — the real boundary, measured.
//
// This test originally asserted that changing a private helper's body is patchable, because
// MATRIX.md section 5 lists "private functions" under supported. Running it against the real analyzer
// showed that is NOT what the product does, and the product is the authority: a change whose changed
// DECLARATION is private is refused with "private/constructor/extension/abstract/external changes are
// unsupported". Generated code is full of private helpers, so the distinction below is the one a
// developer using build_runner will actually hit.
func TestA3PrivateDeclarationBoundaryInGeneratedCode(t *testing.T) {
	root := a3FlutterRoot(t)

	t.Run("changing a private declaration directly is refused", func(t *testing.T) {
		rep := a3Diff(t, root, map[string]string{
			"lib/main.dart":    "import 'codegen.dart';\nvoid main() { print(generated()); }\n",
			"lib/codegen.dart": "int _helper() => 1;\nint generated() => _helper();\n",
		}, map[string]string{
			"lib/codegen.dart": "int _helper() => 2;\nint generated() => _helper();\n",
		})
		if rep.Supported {
			t.Fatal("a directly-changed PRIVATE declaration is refused by the analyzer; if this now " +
				"passes the boundary moved and MATRIX.md section 5 must be re-measured, not assumed")
		}
		if !strings.Contains(strings.Join(rep.Blockers, " "), "private") {
			t.Errorf("the refusal should name the private-declaration rule, got: %v", rep.Blockers)
		}
	})

	t.Run("a private helper reached from a changed PUBLIC declaration is carried", func(t *testing.T) {
		rep := a3Diff(t, root, map[string]string{
			"lib/main.dart":    "import 'codegen.dart';\nvoid main() { print(generated()); }\n",
			"lib/codegen.dart": "int generated() => 1;\n",
		}, map[string]string{
			"lib/codegen.dart": "int _newHelper() => 7;\nint generated() => _newHelper();\n",
		})
		if !rep.Supported {
			t.Fatalf("a PUBLIC body change that introduces a new private helper must be patchable; "+
				"blockers: %v", rep.Blockers)
		}
		a3RequireLibraries(t, rep, "package:a3fix/codegen.dart")
		if len(rep.NewCodeClosure) == 0 {
			t.Error("the newly introduced private helper must appear in the new-code closure; without " +
				"it the patched public function would call a symbol the device does not have")
		}
	})
}

// A generated type referenced ONLY as a type annotation: changing one of its method bodies is a body
// change, and the type-only referrer must not be dragged in.
func TestA3GeneratedTypeOnlyDependencyDoesNotWidenTheChangeSet(t *testing.T) {
	root := a3FlutterRoot(t)
	rep := a3Diff(t, root, map[string]string{
		"lib/main.dart": "import 'codegen.dart';\nint describe(Generated g) => g.value();\n" +
			"void main() { print(describe(Generated())); }\n",
		"lib/codegen.dart": "class Generated { int value() => 1; }\n",
	}, map[string]string{
		"lib/codegen.dart": "class Generated { int value() => 2; }\n",
	})

	if !rep.Supported {
		t.Fatalf("a method body change must be patchable; blockers: %v", rep.Blockers)
	}
	a3RequireLibraries(t, rep, "package:a3fix/codegen.dart")
	if a3ChangedLibraries(rep)["package:a3fix/main.dart"] {
		t.Error("a type-ONLY reference must not make the referrer a changed library")
	}
}

// A multi-library generated closure: only the library that actually changed is reported, no matter how
// deep the import chain is.
func TestA3MultiLibraryGeneratedClosureReportsOnlyTheChangedLibrary(t *testing.T) {
	root := a3FlutterRoot(t)
	rep := a3Diff(t, root, map[string]string{
		"lib/main.dart": "import 'a.dart';\nvoid main() { print(fromA()); }\n",
		"lib/a.dart":    "import 'b.dart';\nint fromA() => fromB();\n",
		"lib/b.dart":    "import 'c.dart';\nint fromB() => fromC();\n",
		"lib/c.dart":    "int fromC() => 1;\n",
	}, map[string]string{
		"lib/c.dart": "int fromC() => 2;\n",
	})

	if !rep.Supported {
		t.Fatalf("a leaf body change must be patchable; blockers: %v", rep.Blockers)
	}
	a3RequireLibraries(t, rep, "package:a3fix/c.dart")
}

// Stale generated output must be refused by the COMPILER, before any patch exists. This is the case
// where silence would be worst: a stale .g.dart that no longer matches its library is the normal
// failure mode of a forgotten `build_runner`, and shipping a patch from it would bind against code
// that does not exist.
func TestA3StaleGeneratedOutputFailsTheCandidateCompile(t *testing.T) {
	root := a3FlutterRoot(t)
	dir := a3Project(t, map[string]string{
		"lib/main.dart":     a3MainUsesModel,
		"lib/models.dart":   "part 'models.g.dart';\nclass Model { int get value => generatedValue(); }\n",
		"lib/models.g.dart": "part of 'models.dart';\nint generatedValue() => 1;\n",
	})
	if _, err := generateFreehandSourceKernel(dir, root, a3Recipe(), filepath.Join(dir, "base.dill")); err != nil {
		t.Fatalf("compile base source kernel: %v", err)
	}
	// The library now expects a symbol the stale generated part no longer provides.
	writeFile(t, filepath.Join(dir, "lib", "models.dart"),
		"part 'models.g.dart';\nclass Model { int get value => regeneratedValue(); }\n")

	if _, err := generateFreehandSourceKernel(dir, root, a3Recipe(), filepath.Join(dir, "cand.dill")); err == nil {
		t.Fatal("a candidate whose generated output is stale must FAIL to compile; producing a patch " +
			"from it would bind a redirect to a symbol that does not exist")
	}
}

// Determinism: the same base and candidate must produce the same diff, twice. Without this the patch
// identity derived from the diff would not be reproducible.
func TestA3DiffIsDeterministicAcrossRuns(t *testing.T) {
	root := a3FlutterRoot(t)
	base := map[string]string{
		"lib/main.dart":     a3MainUsesModel,
		"lib/models.dart":   "part 'models.g.dart';\nclass Model { int get value => generatedValue(); }\n",
		"lib/models.g.dart": "part of 'models.dart';\nint generatedValue() => 1;\n",
	}
	mutate := map[string]string{"lib/models.g.dart": "part of 'models.dart';\nint generatedValue() => 2;\n"}

	first, _ := json.Marshal(a3Diff(t, root, base, mutate))
	second, _ := json.Marshal(a3Diff(t, root, base, mutate))
	if string(first) != string(second) {
		t.Fatalf("the diff is not reproducible across runs:\n first: %s\nsecond: %s", first, second)
	}
}

// ---------------------------------------------------------------------------
// PRODUCTION SYNTHESIS for the central changed-part case.
//
// The diff agreeing with itself is not enough. The diff decides WHAT changed; synthesis decides what
// the device is actually handed, and that is where a part-file mistake would finally land: the module
// declares which library each replacement belongs to, and a redirect naming `models.g.dart` would
// resolve to nothing on the device — after the patch had already shipped.

func TestA3ChangedPartSynthesisBindsToOwningLibraryAndExecutesTheNewValue(t *testing.T) {
	root := a3FlutterRoot(t)
	run := a3DiffRun(t, root, map[string]string{
		"lib/main.dart":     a3MainUsesModel,
		"lib/models.dart":   "part 'models.g.dart';\nclass Model { int get value => generatedValue(); }\n",
		"lib/models.g.dart": "part of 'models.dart';\nint generatedValue() => 1;\n",
	}, map[string]string{
		"lib/models.g.dart": "part of 'models.dart';\nint generatedValue() => 2;\n",
	})
	if !run.rep.Supported {
		t.Fatalf("precondition: the diff must be patchable; blockers: %v", run.rep.Blockers)
	}
	moduleSrc, man := a3Synthesize(t, root, run)

	// 1. The ABI must name the OWNING library. This is the assertion the whole A3 case exists for.
	if len(man.ReplacementABI) != 1 {
		t.Fatalf("expected exactly one replacement, got %d: %+v", len(man.ReplacementABI), man.ReplacementABI)
	}
	abi := man.ReplacementABI[0]
	if abi.BaseIdentity != "package:a3fix/models.dart::::generatedValue" {
		t.Errorf("the replacement must be bound to the owning library, got base_identity %q", abi.BaseIdentity)
	}
	if abi.ModuleMember != "generatedValue" || abi.Kind != "function" {
		t.Errorf("unexpected replacement shape: %+v", abi)
	}

	// 2. The part FILENAME must appear nowhere the device could try to resolve. A part has no
	//    package: URI, so any occurrence here is a redirect that binds to nothing.
	for what, body := range map[string]string{"module source": moduleSrc, "module library": man.ModuleLibrary,
		"imports": strings.Join(man.Imports, " "), "base identity": abi.BaseIdentity,
		"stable identity": abi.StableIdentity, "abi module library": abi.ModuleLibrary} {
		if strings.Contains(body, "models.g.dart") {
			t.Errorf("the %s references the part file, which is not a resolvable library: %s", what, body)
		}
	}
	if !strings.Contains(strings.Join(man.Imports, " "), "package:a3fix/models.dart") {
		t.Errorf("the module must import the owning library, got imports %v", man.Imports)
	}

	// 3. VALUE LEVEL. Run the synthesized module on this host and require the CHANGED value. Base was
	//    1; a module that still computes 1 would pass every structural check above and ship a patch
	//    that changes nothing.
	got := a3ExecuteModule(t, root, run, moduleSrc)
	if !strings.Contains(got, "=2") {
		t.Fatalf("the synthesized module did not compute the changed value (base was 1): %s", got)
	}
	if !strings.Contains(got, "package:a3fix/models.dart") {
		t.Errorf("the executed value must be reported against the owning library: %s", got)
	}
	if len(man.ValueExercised) == 0 {
		t.Error("the synthesizer must record that it exercised the value")
	}
}

// Every refused or failed case must leave NOTHING publishable behind. A refusal that still wrote a
// module, a manifest or a bundle would leave an artifact that a later step could pick up.
func TestA3RefusedAndFailedCasesProduceNoPublishableArtifacts(t *testing.T) {
	root := a3FlutterRoot(t)

	t.Run("refused private-declaration change", func(t *testing.T) {
		run := a3DiffRun(t, root, map[string]string{
			"lib/main.dart":    "import 'codegen.dart';\nvoid main() { print(generated()); }\n",
			"lib/codegen.dart": "int _helper() => 1;\nint generated() => _helper();\n",
		}, map[string]string{
			"lib/codegen.dart": "int _helper() => 2;\nint generated() => _helper();\n",
		})
		if run.rep.Supported {
			t.Fatal("precondition: this change must be refused")
		}
		a3RequireNoPublishableArtifacts(t, run.dir)
	})

	t.Run("stale generated output fails the candidate compile", func(t *testing.T) {
		dir := a3Project(t, map[string]string{
			"lib/main.dart":     a3MainUsesModel,
			"lib/models.dart":   "part 'models.g.dart';\nclass Model { int get value => generatedValue(); }\n",
			"lib/models.g.dart": "part of 'models.dart';\nint generatedValue() => 1;\n",
		})
		if _, err := generateFreehandSourceKernel(dir, root, a3Recipe(), filepath.Join(dir, "base.dill")); err != nil {
			t.Fatalf("compile base: %v", err)
		}
		writeFile(t, filepath.Join(dir, "lib", "models.dart"),
			"part 'models.g.dart';\nclass Model { int get value => regeneratedValue(); }\n")
		if _, err := generateFreehandSourceKernel(dir, root, a3Recipe(), filepath.Join(dir, "cand.dill")); err == nil {
			t.Fatal("precondition: a stale generated part must fail the candidate compile")
		}
		a3RequireNoPublishableArtifacts(t, dir)
	})
}

// a3RequireNoPublishableArtifacts fails if anything a publish step could consume exists under dir.
func a3RequireNoPublishableArtifacts(t *testing.T, dir string) {
	t.Helper()
	var found []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		switch {
		case strings.HasPrefix(base, "soroq_freehand_module"),
			base == "manifest.json", base == "manifest.sig",
			strings.HasSuffix(base, ".patch"), strings.HasSuffix(base, ".zip"),
			strings.Contains(path, string(filepath.Separator)+"releases"+string(filepath.Separator)),
			strings.Contains(path, string(filepath.Separator)+"patches"+string(filepath.Separator)):
			rel, _ := filepath.Rel(dir, path)
			found = append(found, rel)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("a refused/failed case left publishable artifacts behind: %v", found)
	}
}
