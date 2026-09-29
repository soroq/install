package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scopedMeta() FreehandBaselineMeta {
	m := fullMeta()
	m.ContractSchema = freehandContractSchemaV2
	m.ContractDigest = strings.Repeat("c", 64)
	return m
}

func testIfaceFiles(spec string) *freehandBaselineInterfaceFiles {
	return &freehandBaselineInterfaceFiles{
		ContractYAML:   []byte("callable:\n  - library: 'dart:core'\n    class: ['Object']\n"),
		ValidationYAML: []byte(spec),
	}
}

func TestBaselineInterface_V2PersistsAndVerifiesBothFiles(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	relDir, err := persistFreehandBaselineWithInterface(proj, scopedMeta(), dill, srcDill, man, graph, testDepGraph(), "", testIfaceFiles("callable: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := verifyExistingBaseline(relDir)
	if err != nil {
		t.Fatalf("fresh v2 baseline must verify: %v", err)
	}
	if !sha256HexRe.MatchString(m.ContractYAMLSHA256) || !sha256HexRe.MatchString(m.InterfaceValidationSHA256) {
		t.Fatalf("digests not recorded: %+v", m)
	}
	if p := freehandBaseInterfaceValidationPath(relDir, m); p != filepath.Join(relDir, freehandInterfaceValidationFile) {
		t.Fatalf("patch side must find the spec: %q", p)
	}
	// Idempotent for identical inputs, refused for a different spec under the same runtime id.
	if _, err := persistFreehandBaselineWithInterface(proj, scopedMeta(), dill, srcDill, man, graph, testDepGraph(), "", testIfaceFiles("callable: []\n")); err != nil {
		t.Fatalf("identical re-persist must be idempotent: %v", err)
	}
	if _, err := persistFreehandBaselineWithInterface(proj, scopedMeta(), dill, srcDill, man, graph, testDepGraph(), "", testIfaceFiles("callable: [x]\n")); err == nil {
		t.Fatal("a different validation spec is a different base")
	}
	// Tamper and removal are both detected.
	spec := filepath.Join(relDir, freehandInterfaceValidationFile)
	os.WriteFile(spec, []byte("callable:\n  - library: 'package:flutter/material.dart'\n"), 0o600)
	if _, err := verifyExistingBaseline(relDir); err == nil {
		t.Fatal("a tampered validation spec must fail verification")
	}
	os.Remove(spec)
	if _, err := verifyExistingBaseline(relDir); err == nil {
		t.Fatal("a v2 baseline without its validation spec must fail verification")
	}
}

func TestBaselineInterface_SchemaAndFilesMustAgree(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	if _, err := persistFreehandBaselineWithInterface(proj, scopedMeta(), dill, srcDill, man, graph, testDepGraph(), "", nil); err == nil {
		t.Fatal("a v2 contract without interface files must be refused")
	}
	v1 := fullMeta()
	v1.ContractSchema = freehandContractSchema
	if _, err := persistFreehandBaselineWithInterface(proj, v1, dill, srcDill, man, graph, testDepGraph(), "", testIfaceFiles("x")); err == nil {
		t.Fatal("a v1 contract must not carry v2 interface files")
	}
	if _, err := persistFreehandBaselineWithInterface(proj, scopedMeta(), dill, srcDill, man, graph, testDepGraph(), "", testIfaceFiles("")); err == nil {
		t.Fatal("an empty validation spec must be refused")
	}
}

func TestBaselineInterface_V1BaselineUnchangedAndStrayFileRefused(t *testing.T) {
	proj, dill, srcDill, man, graph := seedFixture(t)
	v1 := fullMeta()
	v1.ContractSchema = freehandContractSchema
	v1.ContractDigest = strings.Repeat("d", 64)
	relDir, err := persistFreehandBaseline(proj, v1, dill, srcDill, man, graph, testDepGraph(), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(relDir, "baseline.json"))
	var probe map[string]json.RawMessage
	_ = json.Unmarshal(raw, &probe)
	for _, k := range []string{"contract_yaml_sha256", "interface_validation_sha256"} {
		if _, ok := probe[k]; ok {
			t.Fatalf("a v1 baseline.json must not gain %q", k)
		}
	}
	if _, err := verifyExistingBaseline(relDir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(relDir, freehandInterfaceValidationFile), []byte("callable: []\n"), 0o600)
	if _, err := verifyExistingBaseline(relDir); err == nil {
		t.Fatal("a stray validation spec beside a v1 baseline must be refused, not read")
	}
}
