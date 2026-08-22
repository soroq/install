package main

// [soroq] POST-COMPILE delivery of the rich base identity, as an app-bundled, code-signed asset.
//
// WHY IT CANNOT BE A DART CONSTANT. Two of the four fields are computed FROM the compiled kernel:
// `base_fingerprint` is the app.dill sha and `retention_digest` is the sha of the manifest gen_snapshot
// consumed. The generated bootstrap that would carry them is compiled INTO that kernel, so baking
// either value in changes the source, which changes the kernel, which changes the value. There is no
// fixed point. `prepareFreehandZeroTouch` generates the bootstrap before `buildIOSAppDill`, and
// `persistFreehandBaselineFromBuild` computes both hashes after it.
//
// Emitting only the two pre-build fields was rejected on the same grounds a partial identity always is:
// `SoroqBaseIdentity.isComplete` fails, the controller refuses to act on it, and the app ships the
// appearance of a check with none of its effect.
//
// WHAT THIS DOES INSTEAD. After gen_snapshot has run and the baseline is persisted, the four fields
// exist. They are written as JSON at the ROOT of the built `.app`, beside the executable, where:
//
//   - the code signature covers them, because the file is inside the bundle before signing;
//   - Dart can read them SYNCHRONOUSLY through `dart:io`, with no plugin registration and no binary
//     messenger, from `dirname(Platform.resolvedExecutable)`. That matters: the identity has to be
//     available before `ensureBaseIsolation`, which under activate-before-main runs before the first
//     frame. A MethodChannel round trip could land after the restore it is supposed to guard.
//
// The asset carries the canonical digest over its own fields, so the reader re-derives it and refuses
// an edited file WITHOUT depending on the signature. The signature is the outer defence, not the only
// one.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const (
	freehandBaseIdentityAssetSchema = "soroq.base_identity_asset.v1"
	// Mirrored by soroqBaseIdentityAssetName in packages/soroq_flutter/lib/src/base_identity.dart.
	freehandBaseIdentityAssetName = "soroq_base_identity.json"
)

type freehandBaseIdentityAsset struct {
	Schema          string `json:"schema"`
	RuntimeID       string `json:"runtime_id"`
	BaseFingerprint string `json:"base_fingerprint"`
	ContractDigest  string `json:"contract_digest"`
	RetentionDigest string `json:"retention_digest"`
	Digest          string `json:"digest"`
}

func freehandBaseIdentityAssetBytes(id FreehandRichBaseIdentity) ([]byte, error) {
	if id.Digest == "" {
		return nil, fmt.Errorf("refusing to write an identity asset with no digest")
	}
	// Re-derive rather than trust the struct: the digest is what the device recomputes, and a producer
	// that shipped a digest inconsistent with its own fields would be refused on every launch.
	want := freehandBaseIdentityDigest(id.RuntimeID, id.BaseFingerprint, id.ContractDigest, id.RetentionDigest)
	if id.Digest != want {
		return nil, fmt.Errorf("identity digest does not recompute from its own fields")
	}
	b, err := json.MarshalIndent(freehandBaseIdentityAsset{
		Schema:          freehandBaseIdentityAssetSchema,
		RuntimeID:       id.RuntimeID,
		BaseFingerprint: id.BaseFingerprint,
		ContractDigest:  id.ContractDigest,
		RetentionDigest: id.RetentionDigest,
		Digest:          id.Digest,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// findIOSAppBundles returns every `*.app` directory the iOS build produced under the project.
func findIOSAppBundles(projectDir string) ([]string, error) {
	root := filepath.Join(projectDir, "build", "ios")
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a partial build tree is not this function's problem
		}
		if !d.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".app" {
			out = append(out, path)
			return filepath.SkipDir // never descend into a bundle: nested .app dirs are extensions
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// writeFreehandBaseIdentityAsset writes the identity into every built app bundle.
//
// FAIL CLOSED: finding no bundle is an error. A release that printed success while delivering no
// identity would produce an app that refuses every patch on device, and the operator would learn it
// from a phone rather than from the command that was supposed to do it.
func writeFreehandBaseIdentityAsset(projectDir string, id FreehandRichBaseIdentity) ([]string, error) {
	payload, err := freehandBaseIdentityAssetBytes(id)
	if err != nil {
		return nil, err
	}
	bundles, err := findIOSAppBundles(projectDir)
	if err != nil {
		return nil, err
	}
	if len(bundles) == 0 {
		return nil, fmt.Errorf("no built .app bundle under %s: the base identity has nowhere to go",
			filepath.Join(projectDir, "build", "ios"))
	}
	written := make([]string, 0, len(bundles))
	for _, b := range bundles {
		dst := filepath.Join(b, freehandBaseIdentityAssetName)
		if err := os.WriteFile(dst, payload, 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", dst, err)
		}
		written = append(written, dst)
	}
	return written, nil
}

// deliverFreehandBaseIdentity derives the identity from the persisted baseline and writes it into the
// built bundles.
//
// The baseline is re-read here on purpose and it is NOT a verification re-read. `persistFreehandBaseline`
// either just wrote this file or proved a pre-existing one byte-identical on its idempotent path, and
// nothing below decides whether the baseline is acceptable — it only copies four fields the baseline
// already committed to.
func deliverFreehandBaseIdentity(projectDir, relDir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	if err != nil {
		return nil, fmt.Errorf("read the persisted baseline: %w", err)
	}
	var meta FreehandBaselineMeta
	if err := strictDecodeJSON(raw, &meta); err != nil {
		return nil, fmt.Errorf("decode the persisted baseline: %w", err)
	}
	id, err := richBaseIdentityFromBaseline(&meta)
	if err != nil {
		return nil, err
	}
	return writeFreehandBaseIdentityAsset(projectDir, id)
}
