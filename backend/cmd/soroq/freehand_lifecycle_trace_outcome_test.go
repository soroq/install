package main

// OUTCOME TRACING — the injected instrumentation must distinguish "attempted" from "succeeded".
//
// This exists because a device verdict was reported wrongly. An invalid-batch candidate was published
// by reusing the ACTIVE patch's bytecode verbatim and changing only the replacement ABI. The device
// refused it and kept the previous patch, and the trace showed:
//
//	loadModule.start bytes=1201
//	loadModule.end
//	(no transitionByIdentity at all)
//
// That was read as "verified, loaded, then refused at the identity layer". It was not. `loadModule.end`
// came from a `finally`, so it fires when the load THROWS; and because the activator calls
// transitionByIdentity on the line immediately after `await loadModule(...)`, the absence of
// `transitionByIdentity.start` is positive proof that the load itself threw. The real refusal was the
// VM's already-loaded-library check — the candidate's module namespace was identical to the module
// already loaded — which is module loading, not the identity transaction.
//
// Two different subsystems, two different guarantees. These tests pin the instrumentation that makes
// them distinguishable, and pin the reasoning rule that follows from it.

import (
	"os"
	"strings"
	"testing"
)

// withTrace runs f with lifecycle tracing enabled, since the injectors are no-ops otherwise.
func withTrace(t *testing.T, f func()) {
	t.Helper()
	old, had := os.LookupEnv(freehandLifecycleTraceEnv)
	if err := os.Setenv(freehandLifecycleTraceEnv, "1"); err != nil {
		t.Fatalf("set %s: %v", freehandLifecycleTraceEnv, err)
	}
	defer func() {
		if had {
			os.Setenv(freehandLifecycleTraceEnv, old)
		} else {
			os.Unsetenv(freehandLifecycleTraceEnv)
		}
	}()
	f()
}

func TestActivatorTraceEmitsOutcomeEventsNotJustBrackets(t *testing.T) {
	withTrace(t, func() {
		got := injectActivatorLifecycleTrace(freehandActivatorSource, "myapp")

		for _, want := range []string{
			"loadModule.start",
			"loadModule.success",
			"loadModule.error type=${e.runtimeType}",
			"transitionByIdentity.start",
			"transitionByIdentity.success committed=$committed",
			"transitionByIdentity.error type=${e.runtimeType}",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("instrumented activator is missing %q", want)
			}
		}
	})
}

// The success line must be unreachable when the primitive throws. Structurally that means it sits in
// the `try` BEFORE any `catch`/`finally` — never in the `finally`, which is exactly the bug that made
// `.end` meaningless as evidence.
func TestSuccessEventsAreNotEmittedFromFinally(t *testing.T) {
	withTrace(t, func() {
		got := injectActivatorLifecycleTrace(freehandActivatorSource, "myapp")

		for _, probe := range []struct{ success, terminator string }{
			{"loadModule.success", "} catch (e) {"},
			{"transitionByIdentity.success", "} catch (e) {"},
		} {
			si := strings.Index(got, probe.success)
			if si < 0 {
				t.Fatalf("missing %q", probe.success)
			}
			ci := strings.Index(got[si:], probe.terminator)
			if ci < 0 {
				t.Fatalf("%q is not followed by a catch block; it may not be on the success path",
					probe.success)
			}
			// A `finally` must come AFTER the catch, so the success line must precede both.
			fi := strings.Index(got[si:], "} finally {")
			if fi >= 0 && fi < ci {
				t.Errorf("%q appears to be emitted from a finally block, which fires on the throwing "+
					"path too and therefore proves nothing", probe.success)
			}
		}
	})
}

// The `.end` brackets stay — they are useful for the ordering invariant — but they must remain in the
// `finally`, i.e. they are attempt markers and nothing more. This pins the intent so a future edit
// does not quietly promote `.end` into an outcome.
func TestEndEventsRemainAttemptMarkersInFinally(t *testing.T) {
	withTrace(t, func() {
		got := injectActivatorLifecycleTrace(freehandActivatorSource, "myapp")
		for _, end := range []string{"loadModule.end", "transitionByIdentity.end"} {
			ei := strings.Index(got, end)
			if ei < 0 {
				t.Fatalf("missing %q", end)
			}
			// The closest block opener before the .end must be the finally.
			before := got[:ei]
			if strings.LastIndex(before, "} finally {") < strings.LastIndex(before, "} catch (e) {") {
				t.Errorf("%q is not inside the finally block; .end must stay an attempt marker", end)
			}
		}
	})
}

// THE REASONING RULE, encoded so it cannot be forgotten.
//
// Given a trace, a same-bytecode/different-ABI candidate fails at MODULE LOADING and must never be
// counted as evidence about the identity transaction. The discriminator is not the error text; it is
// whether `loadModule.success` and `transitionByIdentity.start` are present.
func TestSameBytecodeDifferentAbiIsNotIdentityTransitionEvidence(t *testing.T) {
	// Exactly what the device emitted for the withdrawn gate-5 candidate (v8), which reused the
	// ACTIVE patch's bytecode and changed only the ABI.
	sameBytecodeDifferentAbi := []string{
		"loadModule.start bytes=1201",
		"loadModule.end",
		"checkForUpdate.end active=patched v=6",
	}
	// What a genuinely fresh module rejected by the identity transaction must look like.
	identityRejection := []string{
		"loadModule.start bytes=1452",
		"loadModule.success",
		"loadModule.end",
		"transitionByIdentity.start n=1",
		"transitionByIdentity.error type=StateError msg=[soroq] transition: new module identity not found",
		"transitionByIdentity.end",
		"checkForUpdate.end active=patched v=6",
	}

	if provesIdentityTransition(sameBytecodeDifferentAbi) {
		t.Error("a same-bytecode/different-ABI refusal was counted as identity-transition evidence; " +
			"it fails at module loading (already-loaded library) and proves nothing about the batch")
	}
	if !provesIdentityTransition(identityRejection) {
		t.Error("a genuine load-then-reject trace was NOT recognised as identity-transition evidence")
	}

	// A load that succeeded but whose transition COMMITTED is not a rejection either.
	committed := []string{
		"loadModule.start bytes=1452", "loadModule.success", "loadModule.end",
		"transitionByIdentity.start n=1", "transitionByIdentity.success committed=1",
	}
	if provesIdentityTransition(committed) {
		t.Error("a successful transition was counted as a rejection")
	}
}

// provesIdentityTransition reports whether a trace entitles a reader to claim the identity transaction
// REJECTED a batch: the module must have genuinely loaded, the transition must have been entered, and
// it must have errored rather than committed.
func provesIdentityTransition(trace []string) bool {
	var loaded, entered, errored, succeeded bool
	for _, line := range trace {
		switch {
		case strings.HasPrefix(line, "loadModule.success"):
			loaded = true
		case strings.HasPrefix(line, "transitionByIdentity.start"):
			entered = true
		case strings.HasPrefix(line, "transitionByIdentity.error"):
			errored = true
		case strings.HasPrefix(line, "transitionByIdentity.success"):
			succeeded = true
		}
	}
	return loaded && entered && errored && !succeeded
}

// Default generation must stay byte-identical, so the golden test, the shipped template and every
// production build are untouched by this instrumentation.
func TestActivatorIsUnchangedWithoutTheTraceFlag(t *testing.T) {
	os.Unsetenv(freehandLifecycleTraceEnv)
	src := freehandActivatorSource
	if got := injectActivatorLifecycleTrace(src, "myapp"); got != src {
		t.Error("activator generation changed while lifecycle tracing was disabled")
	}
}
