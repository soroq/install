package main

// THE FREEHAND BASE SURFACE — one immutable artifact describing what a patch may use.
//
// A base exposes its code to patches through two mechanisms that MUST describe the same surface:
//
//	dynamic interface (contract)   what a patch module may reference, extend and use as a type
//	--soroq_manifest (retention)   which identities actually survive AOT tree-shaking in the base
//
// They were produced independently, so they drifted: the contract was widened to 61 libraries while
// retention still covered only the app/package identities discovery happened to find. A module then
// compiled and validated cleanly against the contract, referenced a base member the shipped snapshot had
// shaken away, and the VM aborted at load —
// `bytecode_reader.cc: FATAL("Unable to find function %s in %s")` — which the device could only survive
// because crash-loop protection quarantined the patch.
//
// This type makes the two halves ONE artifact with ONE identity. Both digests are persisted in
// baseline.json and in base_surface.json beside it, and a patch is checked against the retained set
// BEFORE it is ever compiled or uploaded (see freehand_required_identities.go). A mismatch must fail on
// the operator's machine, never inside loadDynamicModule on a user's phone.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const freehandSurfaceSchema = "soroq.freehand.base_surface.v1"

// freehandSurfaceFile is the surface artifact persisted beside the baseline.
const freehandSurfaceFile = "base_surface.json"

// FreehandBaseSurface binds the contract and the retained identity set into a single immutable record.
type FreehandBaseSurface struct {
	Schema string `json:"schema"`

	ContractSchema string   `json:"contract_schema"`
	ContractDigest string   `json:"contract_digest"`
	Libraries      []string `json:"contract_libraries"`
	Sections       []string `json:"contract_sections"`

	// RetainedIdentityDigest is the canonical digest of the EXACT identity set passed to gen_snapshot via
	// --soroq_manifest. RetainedCount is its cardinality (a cheap, human-checkable cross-check).
	RetainedIdentityDigest string `json:"retained_identity_digest"`
	RetainedCount          int    `json:"retained_identity_count"`

	// SurfaceDigest binds both halves. A base whose contract or retention changed at all gets a new
	// surface digest, so the two can never drift without the identity changing.
	SurfaceDigest string `json:"surface_digest"`
}

// canonicalIdentities sorts and dedupes an identity set so its digest is independent of the order the
// profile happened to emit nodes in.
func canonicalIdentities(ids []string) []string {
	seen := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			seen[id] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseIdentityList reads a persisted newline-delimited identity list (retained_identities.txt).
func parseIdentityList(b []byte) []string {
	var ids []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			ids = append(ids, line)
		}
	}
	return canonicalIdentities(ids)
}

// renderIdentityList produces the exact bytes persisted beside the baseline.
func renderIdentityList(ids []string) []byte {
	var b bytes.Buffer
	for _, id := range canonicalIdentities(ids) {
		b.WriteString(id)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// retainedIdentityDigest is the canonical digest of a retained identity set.
func retainedIdentityDigest(ids []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "soroq.freehand.retained_identities.v1\n%d\n", len(ids))
	for _, id := range ids {
		fmt.Fprintf(h, "%s\n", id)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildFreehandBaseSurface derives the surface from the contract that governed the build and the
// AUTHORITATIVE retained identity set resolved from the snapshot profile that same build emitted.
//
// `ids` must come from resolveRetainedIdentitiesFromProfile, NOT from soroq_app_manifest.txt: the
// manifest is an eligibility list whose entries mostly do NOT survive (663 of 2,981 on base 5658149d),
// so hashing it would bind a false claim about what the base retains.
func buildFreehandBaseSurface(c FreehandBaseContract, ids []string) FreehandBaseSurface {
	ids = canonicalIdentities(ids)
	s := FreehandBaseSurface{
		Schema:                 freehandSurfaceSchema,
		ContractSchema:         c.Schema,
		ContractDigest:         c.Digest,
		Libraries:              append([]string(nil), c.Libraries...),
		Sections:               append([]string(nil), c.Sections...),
		RetainedIdentityDigest: retainedIdentityDigest(ids),
		RetainedCount:          len(ids),
	}
	s.SurfaceDigest = s.computeDigest()
	return s
}

// computeDigest binds BOTH halves plus the schema. Explicit canonical serialization, not a struct
// marshal, so a field reordering cannot silently change identity.
func (s FreehandBaseSurface) computeDigest() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n", s.Schema, s.ContractSchema, s.ContractDigest)
	fmt.Fprintf(h, "sections:%s\n", strings.Join(s.Sections, ","))
	for _, l := range s.Libraries {
		fmt.Fprintf(h, "lib:%s\n", l)
	}
	fmt.Fprintf(h, "retained:%s:%d\n", s.RetainedIdentityDigest, s.RetainedCount)
	return hex.EncodeToString(h.Sum(nil))
}

// Validate enforces every internal invariant of a persisted surface.
func (s FreehandBaseSurface) Validate() error {
	if s.Schema != freehandSurfaceSchema {
		return fmt.Errorf("base surface schema %q != %q", s.Schema, freehandSurfaceSchema)
	}
	if s.ContractSchema == "" || !sha256HexRe.MatchString(s.ContractDigest) {
		return errors.New("base surface has a missing/malformed contract identity")
	}
	if !sha256HexRe.MatchString(s.RetainedIdentityDigest) {
		return fmt.Errorf("base surface retained_identity_digest is not a 64-hex sha256: %q", s.RetainedIdentityDigest)
	}
	if s.RetainedCount <= 0 {
		return errors.New("base surface retains ZERO identities; nothing would be patchable")
	}
	if len(s.Libraries) == 0 {
		return errors.New("base surface exposes no contract libraries")
	}
	if !sort.StringsAreSorted(s.Libraries) {
		return errors.New("base surface contract libraries are not canonical (unsorted)")
	}
	if got := s.computeDigest(); got != s.SurfaceDigest {
		return fmt.Errorf("base surface digest mismatch (tampered?): recorded %s != recomputed %s", short12(s.SurfaceDigest), short12(got))
	}
	return nil
}

// AssertMatchesIdentities re-derives the retained digest from the identity list actually on disk and
// requires it to equal the recorded one. This is what catches DRIFT and tampering: the surface record and
// the identity list are stored as separate files, so editing either one independently is detected.
func (s FreehandBaseSurface) AssertMatchesIdentities(ids []string) error {
	ids = canonicalIdentities(ids)
	if len(ids) != s.RetainedCount {
		return fmt.Errorf("base surface retained_identity_count %d != actual %d", s.RetainedCount, len(ids))
	}
	if got := retainedIdentityDigest(ids); got != s.RetainedIdentityDigest {
		return fmt.Errorf("base surface retained_identity_digest %s does not match the persisted identity list (%d identities -> %s): the recorded surface and the base's actual AOT retention have drifted apart",
			short12(s.RetainedIdentityDigest), len(ids), short12(got))
	}
	return nil
}

// AssertMatchesContract requires a freshly derived contract to be the one this surface was built from.
func (s FreehandBaseSurface) AssertMatchesContract(c FreehandBaseContract) error {
	if s.ContractDigest != c.Digest || s.ContractSchema != c.Schema {
		return fmt.Errorf("the freehand base contract changed since this base was built (base %s/%s, current %s/%s); a patch compiled against a different contract than the base retained is not deliverable",
			s.ContractSchema, short12(s.ContractDigest), c.Schema, short12(c.Digest))
	}
	return nil
}

// DecodeFreehandBaseSurfaceStrict is the ONE production parser for a persisted surface.
func DecodeFreehandBaseSurfaceStrict(raw []byte) (FreehandBaseSurface, error) {
	var s FreehandBaseSurface
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return FreehandBaseSurface{}, fmt.Errorf("decode base surface: %w", err)
	}
	if dec.More() {
		return FreehandBaseSurface{}, errors.New("trailing data after base surface JSON")
	}
	if err := s.Validate(); err != nil {
		return FreehandBaseSurface{}, err
	}
	return s, nil
}

func short12(h string) string {
	if len(h) >= 12 {
		return h[:12]
	}
	return h
}
