package depgraph

// Strict production decoding for a persisted runtime dependency GRAPH (the immutable base graph stored in
// a release baseline). The descriptor's decoder lives in descriptor.go; both are the only sanctioned way
// to read these structures back, and neither exists solely inside a test.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DecodeGraphStrict parses a persisted dependency graph, rejecting unknown fields, trailing JSON, a wrong
// schema or generator version, malformed hashes, an inconsistent map (a key that disagrees with the record
// it holds), dangling runtime edges, and a graph digest that does not match the recomputed content.
func DecodeGraphStrict(raw []byte) (Graph, error) {
	var g Graph
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		return Graph{}, fmt.Errorf("decode dependency graph: %w", err)
	}
	if dec.More() {
		return Graph{}, errors.New("trailing data after dependency graph JSON")
	}
	if err := g.Validate(); err != nil {
		return Graph{}, err
	}
	return g, nil
}

// Validate enforces every internal invariant of a dependency graph.
func (g Graph) Validate() error {
	if g.Schema != GraphSchema {
		return fmt.Errorf("dependency graph schema %q != %q", g.Schema, GraphSchema)
	}
	if g.GeneratorVersion != GeneratorVersion {
		return fmt.Errorf("dependency graph generator_version %q != %q — this base was recorded by a different dependency-graph generator and its digest is not comparable; create a new base release", g.GeneratorVersion, GeneratorVersion)
	}
	if strings.TrimSpace(g.RootPackage) == "" {
		return errors.New("dependency graph missing root_package")
	}
	for name, h := range map[string]string{
		"pubspec_lock_sha256":   g.PubspecLockSHA,
		"package_config_sha256": g.PackageConfigSHA,
		"graph_digest":          g.GraphDigest,
	} {
		if !sha256Re.MatchString(h) {
			return fmt.Errorf("dependency graph field %s is not a 64-hex sha256: %q", name, h)
		}
	}
	if g.Packages == nil {
		return errors.New("dependency graph has no packages map")
	}
	for key, p := range g.Packages {
		if key != p.Name {
			return fmt.Errorf("dependency graph map key %q disagrees with the package record it holds (%q) — swapped or forged record", key, p.Name)
		}
		if strings.TrimSpace(p.Version) == "" {
			return fmt.Errorf("dependency graph package %q has no version", key)
		}
		if strings.TrimSpace(p.SourceID) == "" {
			return fmt.Errorf("dependency graph package %q has no source_id", key)
		}
		if strings.Contains(p.SourceID, "/Users/") || strings.Contains(p.SourceID, "/home/") || strings.Contains(p.SourceID, `C:\`) {
			return fmt.Errorf("dependency graph package %q embeds a developer-local absolute path in source_id (%q)", key, p.SourceID)
		}
		for label, h := range map[string]string{"content_hash": p.ContentHash, "tree_hash": p.TreeHash, "pubspec_sha256": p.PubspecSHA} {
			if h != "" && !sha256Re.MatchString(h) {
				return fmt.Errorf("dependency graph package %q has a malformed %s: %q", key, label, h)
			}
		}
		if p.Source != SourceSDK && p.identityHash() == "" {
			return fmt.Errorf("dependency graph package %q has no content pin (neither a hosted content_hash nor a tree_hash)", key)
		}
		// Every runtime edge must resolve inside the graph: a dangling edge means the recorded closure is
		// incomplete and a delta computed against it would be wrong.
		for _, e := range p.Dependencies {
			if _, ok := g.Packages[e]; !ok {
				return fmt.Errorf("dependency graph package %q has a dangling runtime edge to %q (not present in the graph)", key, e)
			}
		}
	}
	for _, r := range g.Roots {
		if _, ok := g.Packages[r]; !ok {
			return fmt.Errorf("dependency graph root edge %q is not present in the graph", r)
		}
	}
	if got := g.RecomputeDigest(); got != g.GraphDigest {
		return fmt.Errorf("dependency graph digest mismatch (tampered?): recorded %s != recomputed %s", short(g.GraphDigest), short(got))
	}
	return nil
}

// MarshalCanonical serializes a graph for persistence. It validates first, so a malformed graph can never
// be written into an immutable baseline.
func (g Graph) MarshalCanonical() ([]byte, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(g, "", "  ")
}
