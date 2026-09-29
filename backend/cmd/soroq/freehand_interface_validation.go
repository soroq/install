package main

// PATCH-SIDE GATE FOR USAGE-SCOPED BASES.
//
// A usage-scoped (v2) base retains only the SDK/Flutter classes its own code used
// (ios_dynamic_interface_scoped.go). A patch module that references any other framework declaration --
// a widget class the app never used, say -- compiles cleanly against the base SOURCE kernel (which has the
// whole framework) and then aborts on a user's phone inside the VM's bytecode reader
// (`FATAL("Unable to find function ...")`), because the shipped AOT snapshot does not contain it.
//
// So before the real compile, the module is run through the front end's own dynamic-module validator
// (`dart2bytecode --validate`) against the base's recorded validation spec. That spec covers every
// non-scoped library whole and every library for overriding, so the ONLY interface diagnostics it can
// produce name a framework/SDK declaration outside what the base exposes. Those are refused here, by
// name, on the operator's machine.
//
// Two other validator outcomes are deliberately NOT refusals, because they are not interface questions
// and refusing them would reject patches this lane has always accepted:
//
//   - "Dynamic calls are not allowed in a dynamic module." The lane has never validated dynamic calls,
//     and a dynamic call reaching a member the base shook out is a NoSuchMethodError (a Dart exception
//     the app can see), not a VM abort.
//   - ordinary compile errors: the real compile that follows reports them exactly as before.
//
// A validator that fails WITHOUT a recognizable diagnostic (a crash) is a refusal: the question could not
// be answered, and an unanswered question must not ship.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// freehandBaseInterfaceValidationPath is the validation spec of a VERIFIED baseline, or "" for a v1 base.
func freehandBaseInterfaceValidationPath(relDir string, base *FreehandBaselineMeta) string {
	if base == nil || base.ContractSchema != freehandContractSchemaV2 {
		return ""
	}
	return filepath.Join(relDir, freehandInterfaceValidationFile)
}

// interfaceDiagnosticRe matches the front end's dynamic-interface diagnostics (pkg/front_end
// messages: ClassShouldBeListedAsCallable / ...AsCanBeUsedAsType / ...AsExtendable,
// ConstructorShouldBeListedAsCallable, MemberShouldBeListedAsCallable,
// ExtensionTypeShouldBeListedAsCanBeUsedAsType, MemberShouldBeListedAsCanBeOverridden).
var interfaceDiagnosticRe = regexp.MustCompile(
	`Cannot (use class|use extension type|extend, implement or mix-in class|invoke constructor|invoke member|override member) '([^']+)'( as a type)? (?:in|from) a dynamic module`)

// diagnosticLocationRe recovers `<uri>:<line>:<col>: Error:` from a front-end diagnostic line.
var diagnosticLocationRe = regexp.MustCompile(`^(\S+?):(\d+):(\d+): Error: `)

const dynamicCallDiagnostic = "Dynamic calls are not allowed in a dynamic module."

// interfaceViolation is one framework/SDK declaration a patch uses that the base does not expose.
type interfaceViolation struct {
	What string // human description ("class CupertinoSlider", "constructor CupertinoSlider")
	Name string
	Line int // module source line, 0 when unknown
}

// interfaceValidationOutcome classifies validator output. It is pure so it can be tested on recorded
// output.
type interfaceValidationOutcome struct {
	Violations   []interfaceViolation
	DynamicCalls int
	OtherErrors  int
}

func classifyInterfaceValidation(output string) interfaceValidationOutcome {
	var o interfaceValidationOutcome
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(output))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "Error: ") {
			continue
		}
		if strings.Contains(line, dynamicCallDiagnostic) {
			o.DynamicCalls++
			continue
		}
		m := interfaceDiagnosticRe.FindStringSubmatch(line)
		if m == nil {
			o.OtherErrors++
			continue
		}
		v := interfaceViolation{Name: m[2]}
		switch {
		case m[1] == "use class" && m[3] != "":
			v.What = "type " + m[2]
		case m[1] == "use class":
			v.What = "class " + m[2]
		case m[1] == "use extension type":
			v.What = "extension type " + m[2]
		case m[1] == "extend, implement or mix-in class":
			v.What = "class " + m[2] + " (as a supertype)"
		case m[1] == "invoke constructor":
			v.What = "constructor " + m[2]
		case m[1] == "invoke member":
			v.What = m[2]
		default:
			v.What = "overriding " + m[2]
		}
		if loc := diagnosticLocationRe.FindStringSubmatch(line); loc != nil {
			v.Line, _ = strconv.Atoi(loc[2])
		}
		key := fmt.Sprintf("%s@%d", v.What, v.Line)
		if !seen[key] {
			seen[key] = true
			o.Violations = append(o.Violations, v)
		}
	}
	sort.SliceStable(o.Violations, func(i, j int) bool {
		if o.Violations[i].Line != o.Violations[j].Line {
			return o.Violations[i].Line < o.Violations[j].Line
		}
		return o.Violations[i].What < o.Violations[j].What
	})
	return o
}

// explainInterfaceViolations renders the refusal. moduleLines (optional) lets it quote the offending
// source line.
func explainInterfaceViolations(vs []interfaceViolation, moduleLines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "freehand patch refused — it uses %d framework/SDK declaration(s) that the installed base does not expose:\n", len(vs))
	for _, v := range cap12Violations(vs) {
		fmt.Fprintf(&b, "  - uses %s which the installed base does not expose; ship a store release", v.What)
		if v.Line > 0 && v.Line <= len(moduleLines) {
			fmt.Fprintf(&b, "\n      at: %s", strings.TrimSpace(moduleLines[v.Line-1]))
		}
		b.WriteString("\n")
	}
	if len(vs) > 12 {
		fmt.Fprintf(&b, "  … and %d more\n", len(vs)-12)
	}
	b.WriteString("  This base was built with a usage-scoped contract: it keeps only the SDK and Flutter classes its own\n")
	b.WriteString("  code used, and a class it never used is not in the shipped app, so this patch would abort when a\n")
	b.WriteString("  device loaded it. Any constructor, method or field of a class the base already uses is patchable.\n")
	b.WriteString("  Either change the patch to use only those, or ship the change in a store release (a new base).")
	return b.String()
}

func cap12Violations(vs []interfaceViolation) []interfaceViolation {
	if len(vs) <= 12 {
		return vs
	}
	return vs[:12]
}

// validateFreehandModuleAgainstBaseInterface runs the front end's validator over the module with the
// base's validation spec. compileArgs are the real compile's arguments up to (not including) the output
// and input, so the validation sees exactly what the compile will see.
func validateFreehandModuleAgainstBaseInterface(dartaot string, compileArgs []string, spec, moduleURI, moduleSrc, projectDir string) error {
	if fi, err := os.Stat(spec); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("freehand patch refused — this base was built with a usage-scoped contract but its interface validation spec %s is unreadable, so whether the patch stays within what the base exposes cannot be checked: %v", spec, err)
	}
	tmp, err := os.MkdirTemp("", "soroq-interface-validate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	args := append(append([]string(nil), compileArgs...), "--validate", spec, "-o", filepath.Join(tmp, "validate.bytecode"), moduleURI)
	cmd := exec.Command(dartaot, args...)
	cmd.Dir = projectDir
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return nil
	}
	o := classifyInterfaceValidation(string(out))
	if len(o.Violations) > 0 {
		var lines []string
		if src, rerr := os.ReadFile(moduleSrc); rerr == nil {
			lines = strings.Split(string(src), "\n")
		}
		return fmt.Errorf("%s", explainInterfaceViolations(o.Violations, lines))
	}
	if o.OtherErrors > 0 {
		// Ordinary compile errors: the real compile reports them, exactly as before this gate existed.
		return nil
	}
	if o.DynamicCalls > 0 {
		fmt.Fprintf(os.Stderr, "note: the patch makes %d dynamic call(s); dynamic calls are not interface-checked (a call to a member the base does not contain raises NoSuchMethodError at runtime)\n", o.DynamicCalls)
		return nil
	}
	return fmt.Errorf("freehand patch refused — the dynamic-module validator failed without a diagnostic, so whether this patch stays within what the usage-scoped base exposes could not be established: %v\n%s", runErr, string(out))
}
