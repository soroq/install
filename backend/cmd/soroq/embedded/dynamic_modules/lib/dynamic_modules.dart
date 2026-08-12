// Copyright (c) 2024, the Dart project authors.  Please see the AUTHORS file
// for details. All rights reserved. Use of this source code is governed by a
// BSD-style license that can be found in the LICENSE file.

import 'dart:typed_data' show Uint8List;
import 'dart:_internal' as internal;

/// Load a dynamic module from [uri] and execute its entry point method.
///
/// Entry point method is a no-argument method annotated with
/// `@pragma('dyn-module:entry-point')`.
///
/// This API is experimental, can be changed or removed
/// without a notice.
///
/// Returns a future containing the result of the entry point method.
Future<Object?> loadModuleFromUri(Uri uri) =>
    internal.loadDynamicModule(uri: uri);

/// Load a dynamic module from [bytes] and execute its entry point method.
///
/// Entry point method is a no-argument method annotated with
/// `@pragma('dyn-module:entry-point')`.
///
/// This API is experimental, can be changed or removed
/// without a notice.
///
/// Returns a future containing the result of the entry point method.
Future<Object?> loadModuleFromBytes(Uint8List bytes) =>
    internal.loadDynamicModule(bytes: bytes);

/// [soroq] Path A — transparent OTA function redirection (experimental).
///
/// Redirects the base function identified by the tear-off [baseFunction] to the
/// bytecode of [replacement] (a tear-off from a freshly loaded dynamic module).
/// [baseReceiver] is accepted for symmetry with [soroqRollbackPatch] but unused.
/// This is OTA infrastructure called by the loader/embedder, not by the patched
/// app code.
bool soroqRedirectToPatch(
        Object baseReceiver, Object baseFunction, Object replacement) =>
    internal.soroqRedirectToPatch(baseReceiver, baseFunction, replacement);

/// [soroq] Path A — roll back a redirected function to its original AOT body.
bool soroqRollbackPatch(Object baseReceiver, Object baseFunction) =>
    internal.soroqRollbackPatch(baseReceiver, baseFunction);

/// [soroq] Freehand — apply a transactional desired-state TRANSITION of redirect
/// slots by STABLE IDENTITY (no tear-off, no customer-object construction; instance
/// methods, getters/setters/operators supported).
///
/// [newFlatSpecs] is a fixed-length `List<String>` of N base->patch redirects to SET
/// (8 fields/spec); [staleFlatBaseIds] is a fixed-length `List<String>` of M base
/// slots to CLEAR (4 fields/id). Class fields are empty for a top-level declaration;
/// VM member names are exact (`build`, `get:label`, `+`); kinds are `function` /
/// `static-method` / `instance-member`. All new + stale identities are validated and
/// duplicate/overlapping ids rejected before any slot changes; on any failure it
/// throws (zero slots changed). Clears stale then sets new under one program-lock
/// transaction. Returns the number of new redirects committed. Call after
/// [loadModuleFromBytes] has registered the module's library. Covers activate (empty
/// stale), rollback (empty new), warm v1->v2 (new=v2, stale=v1-v2). OTA infrastructure.
int soroqTransitionBatchByIdentity(
        List<String> newFlatSpecs, List<String> staleFlatBaseIds) =>
    internal.soroqTransitionBatchByIdentity(newFlatSpecs, staleFlatBaseIds);
