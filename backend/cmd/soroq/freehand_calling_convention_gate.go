package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// CALLING-CONVENTION GATE for bases whose engine does not declare soroq_tagged_stack_boundary_v1.
//
// A redirected function tail-enters InterpretCall from its prologue, and InterpretCall reads every
// argument TAGGED from the caller's stack. An engine without the capability compiled a patchable
// function with whatever convention its selector got: register arguments (use_register_cc) and
// TFA-unboxed int/double parameters or result, unless the selector is reachable from outside the
// snapshot -- i.e. declared by a CALLABLE class in the base's dynamic interface, which TFA fully boxes and
// keeps on the stack for the whole selector group. For anything else the patched code receives garbage
// (measured on device: a patched _BenchPageState.runAll ran with `this` = a raw stack address; on the
// host a patched double-returning method returned 1.956e-321 instead of 13.5).
//
// The CLI cannot see the base's selector groups, so this gate allows only what is provably on the tagged
// stack convention on such a base and refuses the rest by name, before anything is synthesized:
//   - a public member of a PUBLIC class, or a public top-level function, in a library the base lists
//     WHOLE under `callable` (the application's own libraries): callable, hence fully boxed;
//   - an override of a Flutter/Object member whose declaring class every Flutter base exposes as
//     callable (State.build, initState, dispose, ..., Object.==/hashCode/toString): the override shares
//     that selector, which is fully boxed.
// A base built by an engine that declares the capability is not gated at all.

// freehandFrameworkBoxedSelectorNames are member names whose selector is declared by a class every Flutter
// base exposes as callable (State / StatelessWidget / StatefulWidget / Widget) or by Object (native, fully
// boxed). A private State's build is the idiomatic hotfix target and has always patched correctly.
var freehandFrameworkBoxedSelectorNames = map[string]bool{
	"build": true, "initState": true, "dispose": true, "didChangeDependencies": true,
	"didUpdateWidget": true, "deactivate": true, "activate": true, "reassemble": true,
	"createState": true, "createElement": true, "debugFillProperties": true,
	"==": true, "hashCode": true, "toString": true, "noSuchMethod": true, "runtimeType": true,
}

var contractWholeLibraryRe = regexp.MustCompile(`^  - library: '([^']+)'$`)

// freehandCallableWholeLibraries reads the `callable:` section of a usage-scoped base contract and
// returns the libraries listed WHOLE (a `- library:` line not followed by a class/member/extension list).
func freehandCallableWholeLibraries(contractYAML string) (map[string]bool, error) {
	f, err := os.Open(contractYAML)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	whole := map[string]bool{}
	inCallable := false
	for i, l := range lines {
		if l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#") {
			inCallable = l == "callable:"
			continue
		}
		if !inCallable {
			continue
		}
		m := contractWholeLibraryRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "    ") {
			continue // a declaration-level entry, not a whole library
		}
		whole[m[1]] = true
	}
	return whole, nil
}

// splitManifestLine splits `<lib-uri>::<class>::<member>` from the right (the URI may contain "::").
func splitManifestLine(line string) (lib, class, member string, ok bool) {
	last := strings.LastIndex(line, "::")
	if last <= 0 {
		return "", "", "", false
	}
	head := line[:last]
	mid := strings.LastIndex(head, "::")
	if mid < 0 {
		return "", "", "", false
	}
	return head[:mid], head[mid+2:], line[last+2:], true
}

func stripAccessorPrefix(member string) string {
	for _, p := range []string{"get:", "set:"} {
		if strings.HasPrefix(member, p) {
			return member[len(p):]
		}
	}
	return member
}

// freehandCallingConventionUnsafe returns the changed identities (manifest lines) whose compiled calling
// convention on a pre-capability base may not be the tagged stack convention. whole == nil means a v1
// base, whose contract lists every package library whole.
func freehandCallingConventionUnsafe(changed []string, whole map[string]bool) []string {
	isWhole := func(lib string) bool {
		if whole == nil {
			return strings.HasPrefix(lib, "package:")
		}
		return whole[lib]
	}
	var unsafe []string
	for _, line := range changed {
		lib, class, member, ok := splitManifestLine(line)
		if !ok {
			unsafe = append(unsafe, line)
			continue
		}
		name := stripAccessorPrefix(member)
		publicMember := name != "" && !strings.HasPrefix(name, "_")
		switch {
		case class == "":
			if !(publicMember && isWhole(lib)) {
				unsafe = append(unsafe, line)
			}
		case !strings.HasPrefix(class, "_") && publicMember && isWhole(lib):
			// callable class member: fully boxed
		case freehandFrameworkBoxedSelectorNames[name]:
			// override of a callable framework / Object selector
		default:
			unsafe = append(unsafe, line)
		}
	}
	sort.Strings(unsafe)
	return unsafe
}

func assertFreehandCallingConventionSafe(base *FreehandBaselineMeta, relDir string, changed []string) error {
	caps, err := baseRedirectCapabilities(base)
	if err != nil {
		return fmt.Errorf("this base's engine capabilities could not be read, so whether it compiled patchable "+
			"functions with the tagged stack calling convention is unknown: %w", err)
	}
	if caps.hasIdentityCapability(freehandTaggedStackBoundaryCapability) {
		return nil
	}
	var whole map[string]bool
	if isScopedContractSchema(base.ContractSchema) {
		if whole, err = freehandCallableWholeLibraries(filepath.Join(relDir, freehandBaseContractFile)); err != nil {
			return fmt.Errorf("read the base contract to check calling conventions: %w", err)
		}
	}
	unsafe := freehandCallingConventionUnsafe(changed, whole)
	if len(unsafe) == 0 {
		return nil
	}
	return fmt.Errorf("%d changed declaration(s) may have been compiled with REGISTER or UNBOXED arguments by "+
		"this base's engine (%s does not declare %s): they are not reachable from the base's callable "+
		"interface, and a redirect hands the interpreter tagged stack arguments only, so the patched code "+
		"would receive garbage (a wrong `this`, corrupt numbers):\n  - %s\n"+
		"Keep them out of the patch, or ship a new base built on a toolchain that declares %s (R9 or later), "+
		"which compiles every patchable boundary with the tagged stack convention.",
		len(unsafe), caps.EngineRevision, freehandTaggedStackBoundaryCapability, joinLines(unsafe),
		freehandTaggedStackBoundaryCapability)
}
