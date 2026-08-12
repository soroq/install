package main

// REQUIRED BASE IDENTITIES — prove a patch is loadable BEFORE it is compiled or uploaded.
//
// A dynamic module's constant pool names base declarations by identity. If any of them was tree-shaken
// out of the shipped AOT snapshot, the VM aborts while READING THE MODULE:
//
//	dart::Class::LookupFunctionAllowPrivate
//	  <- BytecodeReaderHelper::ReadObjectContents / ReadConstantPool / ReadCode
//	  <- BytecodeLoader::LoadBytecode <- DN_Internal_loadDynamicModule
//	runtime/vm/bytecode_reader.cc: FATAL("Unable to find function %s in %s")
//
// That is a SIGABRT on a user's device, recoverable only because crash-loop protection quarantines the
// patch. It must never get that far: every identity a patch requires is declared by the synthesizer and
// checked here against the EXACT retained set of the target base — on the operator's machine, before
// compilation and again before publication.
//
// The check is deliberately independent of the compiler. Compiling proves the module type-checks against
// the base's SOURCE kernel; it says nothing about what survived tree-shaking in the shipped AOT snapshot.
// Those are different questions and only this one predicts load.

import (
	"fmt"
	"sort"
	"strings"
)

// retainedIdentitySet indexes a base's retained identities for lookup.
//
// Membership is EXACT. `LookupFunctionAllowPrivate` takes a `MemberKind`, so `Foo::bar`, `Foo::get:bar`
// and `Foo::set:bar` are three distinct functions that are retained independently — the profile for base
// 5658149d contains `LinkedList::add` and `LinkedList::get:add` as separate entries. Canonicalizing the
// accessor prefix away would let a required getter match a retained setter and ship the very crash this
// gate exists to prevent.
type retainedIdentitySet struct {
	all map[string]bool
	// classes indexes `libUri::class` so a missing member can be reported as "the class retains nothing"
	// (the `_StringBase` case) rather than as an anonymous absent symbol.
	classes map[string]int
}

func newRetainedIdentitySet(ids []string) *retainedIdentitySet {
	s := &retainedIdentitySet{all: make(map[string]bool, len(ids)), classes: map[string]int{}}
	for _, id := range ids {
		s.all[id] = true
		if i := strings.LastIndex(id, "::"); i > 0 {
			s.classes[id[:i]]++
		}
	}
	return s
}

// has reports whether the base retains this exact identity.
func (s *retainedIdentitySet) has(id string) bool { return s.all[id] }

// ownerRetains reports how many functions the owning `lib::class` of an identity retains.
func (s *retainedIdentitySet) ownerRetains(id string) int {
	if i := strings.LastIndex(id, "::"); i > 0 {
		return s.classes[id[:i]]
	}
	return 0
}

// assertRequiredBaseIdentitiesRetained refuses a patch whose module needs a base declaration the target
// base did not retain. The message names every missing identity — that is the whole point: the operator
// learns exactly which declaration the base must retain, instead of a user's app aborting inside the
// bytecode reader.
func assertRequiredBaseIdentitiesRetained(required []string, retained *retainedIdentitySet, surface FreehandBaseSurface) error {
	if len(required) == 0 {
		return nil
	}
	var missing []string
	seen := map[string]bool{}
	for _, id := range required {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if !retained.has(id) {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	var b strings.Builder
	fmt.Fprintf(&b, "freehand patch refused — the module requires %d base declaration(s) that this base did NOT retain, so it would abort inside loadDynamicModule on the device:\n", len(missing))
	for _, m := range cap12(missing) {
		if n := retained.ownerRetains(m); n == 0 {
			fmt.Fprintf(&b, "  - %s   (its class retains NO functions at all)\n", m)
		} else {
			fmt.Fprintf(&b, "  - %s   (its class retains %d other function(s))\n", m, n)
		}
	}
	fmt.Fprintf(&b, "  The base retains %d identities (surface %s). Compiling only proves the module type-checks\n", surface.RetainedCount, short12(surface.SurfaceDigest))
	b.WriteString("  against the base SOURCE kernel; it says nothing about what survived AOT tree-shaking.\n")
	b.WriteString("  Create a new base release whose retention covers these declarations, or change the patch not\n")
	b.WriteString("  to reference them.\n")
	return fmt.Errorf("%s", b.String())
}

func cap12(xs []string) []string {
	if len(xs) <= 12 {
		return xs
	}
	return append(append([]string(nil), xs[:12]...), fmt.Sprintf("… and %d more", len(xs)-12))
}
