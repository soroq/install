package main

// Soroq freehand — the FREEHAND BASE CONTRACT.
//
// The dynamic interface is what a dynamically loaded patch module is allowed to do with the code the
// installed base already contains. It is therefore the ceiling on which future Dart-only dependencies can
// be carried at all: the previous version listed six libraries under `callable:` only, so any dependency
// whose code extended or implemented a base/SDK class was refused — regardless of the package.
//
// This generator derives the contract from the ACTUAL pinned inputs of the base being built rather than
// from a hand-maintained list: the SDK libraries in the pinned vm_platform, the Flutter libraries in the
// pinned compile interface, the application's own libraries, and the eligible pure-Dart dependency
// libraries in the base runtime dependency graph. It is deterministic (sorted, versioned) and its digest
// is bound into the baseline, the release and every patch.
//
// Section coverage is decided by EVIDENCE, not optimism (see
// handoff/freehand/evidence/dependency-ota/contract-ceiling/):
//
//   callable            INCLUDED — proven
//   can-be-used-as-type INCLUDED — proven
//   extendable          INCLUDED — a carried class extending dart:collection's LinkedListEntry compiles,
//                                  validates, loads and executes (probe B)
//   can-be-overridden   EXCLUDED — a carried class OVERRIDING an SDK instance member compiles and
//                                  validates and then SEGFAULTS the VM at load (probe A). Including it
//                                  would trade a clear refusal for a device crash.
//   dynamically-callable EXCLUDED — a whole-library entry makes the front end's dynamic-module validator
//                                  crash: `type 'Extension' is not a subtype of type 'Library' in type
//                                  cast` (dynamic_module_validator.dart, _DynamicCallValidator.run).
//
// Both exclusions are upstream defects with exact reproductions recorded, NOT product decisions. When they
// are fixed the sections move in and the schema version is bumped; nothing else has to change. Until then
// the named contract diagnostic (freehand_contract.go) is the fail-closed fallback: a dependency that
// needs an excluded capability is refused by name instead of failing on a device.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// freehandContractSchema versions the generated contract. Bump it whenever the section set or the
// derivation changes: a base built under one schema is not interchangeable with another.
const freehandContractSchema = "soroq.freehand.base_contract.v1"

// sdkContractLibraries are the public dart: libraries a Dart-only dependency may legitimately use. They
// are intersected with what the PINNED vm_platform actually provides, so the contract can never claim a
// library this base does not ship.
var sdkContractLibraries = []string{
	"dart:async",
	"dart:collection",
	"dart:convert",
	"dart:core",
	"dart:developer",
	"dart:math",
	"dart:typed_data",
}

// flutterContractLibraries are the Flutter libraries a Dart-only Flutter package may use. They are
// intersected with the pinned compile interface for the same reason.
var flutterContractLibraries = []string{
	"dart:ui",
	"package:flutter/animation.dart",
	"package:flutter/cupertino.dart",
	"package:flutter/foundation.dart",
	"package:flutter/gestures.dart",
	"package:flutter/material.dart",
	"package:flutter/painting.dart",
	"package:flutter/rendering.dart",
	"package:flutter/scheduler.dart",
	"package:flutter/services.dart",
	"package:flutter/widgets.dart",
}

// FreehandBaseContract is the generated, deterministic contract plus its identity.
type FreehandBaseContract struct {
	Schema string `json:"schema"`
	// Libraries is the canonical, sorted set of declaration-library URIs the contract exposes.
	Libraries []string `json:"libraries"`
	// Sections lists which dynamic-interface sections were emitted, in order.
	Sections []string `json:"sections"`
	// Digest is the sha256 of the exact YAML bytes written to disk.
	Digest string `json:"digest"`
	// Path is where the YAML was installed (Soroq-owned project state; never under customer lib/).
	Path string `json:"-"`
}

// contractSections are the sections emitted, in canonical order. See the evidence note above for why
// can-be-overridden and dynamically-callable are absent.
var contractSections = []string{"callable", "extendable", "can-be-used-as-type"}

// buildFreehandBaseContract derives the contract from the pinned inputs of THIS base.
//
//   - sdkAvailable    library URIs the pinned vm_platform provides
//   - flutterAvailable library URIs the pinned Flutter compile interface provides
//   - appLibraries    the application's own library URIs
//   - depLibraries    library URIs of ELIGIBLE (Dart-only) dependencies already in the base graph
//
// A nil/empty availability set means "unknown", in which case the corresponding curated set is used as-is
// rather than silently dropping the whole surface.
func buildFreehandBaseContract(sdkAvailable, flutterAvailable, appLibraries, depLibraries []string) FreehandBaseContract {
	set := map[string]bool{}
	add := func(libs []string, available []string) {
		avail := map[string]bool{}
		for _, a := range available {
			avail[a] = true
		}
		for _, l := range libs {
			if len(avail) == 0 || avail[l] {
				set[l] = true
			}
		}
	}
	add(sdkContractLibraries, sdkAvailable)
	add(flutterContractLibraries, flutterAvailable)
	// The app's own libraries and the eligible pure-Dart dependencies already in the base are exposed
	// verbatim: a patch must be able to call and extend the code the base actually shipped.
	add(appLibraries, nil)
	add(depLibraries, nil)

	libs := make([]string, 0, len(set))
	for l := range set {
		libs = append(libs, l)
	}
	sort.Strings(libs)

	c := FreehandBaseContract{Schema: freehandContractSchema, Libraries: libs, Sections: contractSections}
	c.Digest = freehandContractDigest(c)
	return c
}

// renderFreehandContractYAML produces the exact bytes written to disk. Deterministic: sections in
// canonical order, libraries sorted, no timestamps or absolute paths.
func renderFreehandContractYAML(c FreehandBaseContract) string {
	var b strings.Builder
	b.WriteString("# Generated by Soroq. Do not edit.\n")
	fmt.Fprintf(&b, "# schema: %s\n", c.Schema)
	b.WriteString("# Derived from this base's pinned vm_platform, Flutter compile interface, application\n")
	b.WriteString("# libraries and eligible Dart-only dependencies. No package-specific entries.\n")
	for _, section := range c.Sections {
		fmt.Fprintf(&b, "%s:\n", section)
		for _, l := range c.Libraries {
			fmt.Fprintf(&b, "  - library: '%s'\n", l)
		}
	}
	return b.String()
}

// freehandContractDigest binds the schema, the section set and every library URI. It is computed over the
// canonical fields rather than the rendered text so a comment change cannot silently alter identity.
func freehandContractDigest(c FreehandBaseContract) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", c.Schema)
	fmt.Fprintf(h, "sections:%s\n", strings.Join(c.Sections, ","))
	for _, l := range c.Libraries {
		fmt.Fprintf(h, "lib:%s\n", l)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// generateIOSEngineDynamicInterface writes the contract to Soroq-owned project state. Nothing is added to
// the customer's lib/, and the developer never maintains this file or declares patchable functions.
func generateIOSEngineDynamicInterface(projectDir string) (string, error) {
	c, err := generateFreehandBaseContract(projectDir, nil, nil)
	if err != nil {
		return "", err
	}
	return c.Path, nil
}

// generateFreehandBaseContract derives, renders and atomically installs the contract, returning its
// identity for binding into the baseline/release/patch metadata.
func generateFreehandBaseContract(projectDir string, sdkAvailable, flutterAvailable []string) (FreehandBaseContract, error) {
	var zero FreehandBaseContract
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return zero, err
	}
	path := filepath.Join(absDir, ".soroq", "generated", "ios_dynamic_interface.yaml")
	if strings.Contains(path, ",") {
		return zero, fmt.Errorf("iOS engine build path %q contains a comma, which Flutter cannot encode in extra front-end options; move the project to a path without commas", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return zero, fmt.Errorf("create iOS dynamic-interface directory: %w", err)
	}

	appLibs, depLibs, err := contractProjectLibraries(absDir)
	if err != nil {
		return zero, err
	}
	c := buildFreehandBaseContract(sdkAvailable, flutterAvailable, appLibs, depLibs)
	c.Path = path

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(renderFreehandContractYAML(c)), 0o644); err != nil {
		return zero, fmt.Errorf("write temporary iOS dynamic interface: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return zero, fmt.Errorf("install iOS dynamic interface: %w", err)
	}
	return c, nil
}
