package main

// LIFECYCLE TRACE — acceptance instrumentation for the cold-start ordering invariant.
//
// The invariant the black-window fix rests on is an ORDERING:
//
//	firstFrameRasterized < loadModule < transitionByIdentity < first patched value observed
//
// Polling the patched function cannot establish it. Activation completes inside the first sampling
// interval, so a poll only ever bounds the ordering by its own period -- it can say "within 25ms",
// never "after the frame". The two engine calls have to timestamp THEMSELVES.
//
// This injects those timestamps into the generated bootstrap and activator, and only when
// SOROQ_FREEHAND_LIFECYCLE_TRACE=1 is set. Default generation is byte-identical to before, so the
// golden test, the shipped template and every production build are untouched.
//
// One monotonic clock. Every event -- including the app's own value samples -- goes through
// package:<app>/soroq_trace.dart, whose Stopwatch starts at the first trace call (bootstrap entry).
// Wall-clock syslog timestamps are only ~1s-granular in practice and are not comparable across
// processes; a single Stopwatch is.

import (
	"fmt"
	"os"
	"strings"
)

const freehandLifecycleTraceEnv = "SOROQ_FREEHAND_LIFECYCLE_TRACE"

// freehandPackageName pulls the app's package name out of its entrypoint import
// ("package:myapp/main.dart" -> "myapp"), so the injected trace import resolves in the app's own
// package config.
func freehandPackageName(entrypointImport string) string {
	s := strings.TrimPrefix(strings.TrimSpace(entrypointImport), "package:")
	if i := strings.Index(s, "/"); i > 0 {
		return s[:i]
	}
	return s
}

// freehandLifecycleTraceEnabled reports whether acceptance tracing was explicitly requested.
func freehandLifecycleTraceEnabled() bool {
	return strings.TrimSpace(os.Getenv(freehandLifecycleTraceEnv)) == "1"
}

// injectActivatorLifecycleTrace timestamps the two engine primitives themselves. These are the only
// points at which a module is actually loaded and redirects are actually installed, so they are the
// only honest source for "when did activation touch the engine".
//
// OUTCOME EVENTS, NOT JUST BRACKETS. The first version emitted only `.start` and a `.end` from a
// `finally`. A `finally` fires on the throwing path too, so `start` + `end` says the call was ATTEMPTED
// and says nothing about whether it worked -- and reading that pair as "loaded successfully" produced
// a wrong device verdict: a candidate that was actually refused INSIDE loadModule was reported as
// having loaded and then been rejected by the identity transaction. Those are different subsystems and
// different guarantees.
//
// So every primitive now emits an explicit success or error event carrying the outcome:
//
//	loadModule.success                          the module really is registered
//	loadModule.error type=<T> msg=<...>         it is not, and the exception type says why
//	transitionByIdentity.success committed=<n>  n slots really were committed
//	transitionByIdentity.error type=<T>         zero slots changed
//
// `.end` is kept, because bracketing timings are still useful for the ordering invariant, but it must
// never be read as an outcome. The rule is: a stage SUCCEEDED only if its own `.success` line is
// present.
func injectActivatorLifecycleTrace(src, pkg string) string {
	if !freehandLifecycleTraceEnabled() {
		return src
	}
	src = strings.Replace(src,
		"import 'dart:typed_data';",
		"import 'dart:typed_data';\n\nimport 'package:"+pkg+"/soroq_trace.dart' as soroqtrace;",
		1)
	src = strings.Replace(src,
		`  @override
  Future<Object?> loadModule(Uint8List bytecode) =>
      loadModuleFromBytes(bytecode);`,
		`  @override
  Future<Object?> loadModule(Uint8List bytecode) async {
    soroqtrace.trace('loadModule.start bytes=${bytecode.length}');
    try {
      final Object? loaded = await loadModuleFromBytes(bytecode);
      // Emitted ONLY on the non-throwing path. This is the single line that entitles a reader to say
      // the module was registered; the .end below cannot, because it also fires when this throws.
      soroqtrace.trace('loadModule.success');
      return loaded;
    } catch (e) {
      soroqtrace.trace('loadModule.error type=${e.runtimeType} msg=$e');
      rethrow;
    } finally {
      soroqtrace.trace('loadModule.end');
    }
  }`, 1)
	src = strings.Replace(src,
		`  @override
  int transitionByIdentity(
    List<String> newFlatSpecs,
    List<String> staleFlatBaseIds,
  ) =>
      soroqTransitionBatchByIdentity(newFlatSpecs, staleFlatBaseIds);`,
		`  @override
  int transitionByIdentity(
    List<String> newFlatSpecs,
    List<String> staleFlatBaseIds,
  ) {
    soroqtrace.trace('transitionByIdentity.start n=${newFlatSpecs.length ~/ 8}');
    try {
      final int committed =
          soroqTransitionBatchByIdentity(newFlatSpecs, staleFlatBaseIds);
      // The committed count is part of the evidence: a transition that commits fewer slots than the
      // ABI declares is a failure the caller turns into a StateError, and that must be visible here
      // rather than inferred from the absence of a later effect.
      soroqtrace.trace('transitionByIdentity.success committed=$committed');
      return committed;
    } catch (e) {
      // The native primitive is transactional: on this path ZERO slots changed.
      soroqtrace.trace('transitionByIdentity.error type=${e.runtimeType} msg=$e');
      rethrow;
    } finally {
      soroqtrace.trace('transitionByIdentity.end');
    }
  }`, 1)
	return src
}

// injectBootstrapLifecycleTrace timestamps the launch sequence around those primitives.
//
// THE BARRIER IS OBSERVED, NOT GATED ON. An earlier version derived a future from
// waitUntilFirstFrameRasterized, attached the trace to it, and passed THAT as `frameBarrier`. That
// made `firstFrameRasterized < loadModule.start` analytic for the chained activation call -- the
// controller resumed only after the trace callback returned, so the inequality could not fail no
// matter how broken the barrier logic was -- and it meant the traced artifact did not execute the
// production call at all (production emits `soroqActivateRestoredAfterFirstFrame(c)` with no
// frameBarrier, and bootstrap.dart documents that parameter as test-only).
//
// So the observer is now a plain listener that gates nothing, and the activation call is left exactly
// as production generates it. If activation ever ran ahead of rasterization -- which is the defect --
// `loadModule.start` would carry a SMALLER timestamp than `firstFrameRasterized` and the validator
// would fail. The inequality is a measurement again.
func injectBootstrapLifecycleTrace(src, pkg string) string {
	if !freehandLifecycleTraceEnabled() {
		return src
	}
	src = strings.Replace(src,
		"import 'package:flutter/widgets.dart';",
		"import 'package:flutter/scheduler.dart' show FrameTiming;\nimport 'package:flutter/widgets.dart';\n\nimport 'package:"+pkg+"/soroq_trace.dart' as soroqtrace;",
		1)
	src = strings.Replace(src,
		"  WidgetsFlutterBinding.ensureInitialized();",
		"  WidgetsFlutterBinding.ensureInitialized();\n  soroqtrace.trace('bootstrap.start');",
		1)
	src = strings.Replace(src,
		"  app.main();",
		"  soroqtrace.trace('app.main.called');\n  app.main();",
		1)
	src = strings.Replace(src,
		"    soroqActivateRestoredAfterFirstFrame(c).then((_) {",
		`    // Passive observer. It gates NOTHING: the controller awaits its own barrier, exactly as in
    // production. If activation were to run ahead of rasterization, loadModule.start would carry a
    // smaller timestamp than this event and the ordering check would fail.
    WidgetsBinding.instance.waitUntilFirstFrameRasterized
        .then((_) => soroqtrace.trace('firstFrameRasterized'));
    soroqtrace.trace('restoreActivate.scheduled');
    soroqActivateRestoredAfterFirstFrame(c).then((_) {
      soroqtrace.trace('restoreActivate.end');
      late final void Function(List<FrameTiming>) onTimings;
      onTimings = (List<FrameTiming> t) {
        if (t.isEmpty) return;
        WidgetsBinding.instance.removeTimingsCallback(onTimings);
        soroqtrace.trace('postRedirectFrameRasterized');
      };
      WidgetsBinding.instance.addTimingsCallback(onTimings);
      WidgetsBinding.instance.ensureVisualUpdate();`, 1)
	// Bracket the hosted check as well, so a checkForUpdate that ever ran early would be visible in
	// the same timeline rather than having to be inferred from its side effects.
	src = strings.Replace(src,
		`      return c.checkForUpdate().then((SoroqOtaStatus st) {`,
		`      soroqtrace.trace('checkForUpdate.start');
      return c.checkForUpdate().then((SoroqOtaStatus st) {
        // The FULL check result, not just active/version. The previous trace showed only the outcome,
        // so a refused patch looked identical to a skipped one and diagnosing a device failure needed
        // guesswork about which stage refused it.
        soroqtrace.trace('checkForUpdate.end active=${st.active} v=${st.activeVersion}'
            ' fetch=${st.fetch} sig=${st.sig} hash=${st.hash}'
            ' quarantined=${st.quarantined} err=${st.error}');`, 1)
	return src
}

// freehandLifecycleTraceLibrary is the single-clock trace library written into the app's lib/ when
// tracing is enabled, so the app's own value samples share the bootstrap's monotonic clock.
func freehandLifecycleTraceLibrary() string {
	return fmt.Sprintf(`// GENERATED (acceptance tracing only, %s=1) — do not ship.
// One monotonic clock for every lifecycle event and value sample in this process. The Stopwatch is
// lazily started by the first trace call, which is the bootstrap entry.
import 'package:flutter/foundation.dart';

final Stopwatch _clock = Stopwatch()..start();

int get micros => _clock.elapsedMicroseconds;

void trace(String event) =>
    debugPrint('SOROQ_TRACE t_us=${_clock.elapsedMicroseconds} $event');
`, freehandLifecycleTraceEnv)
}
