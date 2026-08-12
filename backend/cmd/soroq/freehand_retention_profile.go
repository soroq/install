package main

// PROFILE-DERIVED RETENTION — the authoritative answer to "what did this base actually keep?".
//
// `--soroq_manifest` is an ELIGIBILITY list, not a retention forcer. The flag's own help text says so
// ("build-time eligibility"), and it was measured on base 5658149d: of its 2,981 manifest entries only
// 663 survive into the snapshot, and appending four more entries produced a byte-identical retained set.
// So the manifest cannot answer "will this reference resolve on device?" — and `baseline.json`'s
// `retained_identities: 2981` asserts something false.
//
// The authoritative answer comes from gen_snapshot itself. `--write_v8_snapshot_profile_to` emits a V8
// heap-snapshot profile of the snapshot it just produced; every surviving `Function` object appears as a
// node whose `owner_` chains to a `Class` (or a `PatchClass` wrapping one) and thence to a `Library`.
// Resolving that chain yields exactly the `libraryUri::className::functionName` form the manifest reader
// builds from live `Function` objects (`Function::SoroqIsPatchable`, engine patch 0003) — and therefore
// exactly the form a module's constant pool is resolved against at load.
//
// Because the profile is written by the SAME gen_snapshot invocation that produced the shipped snapshot,
// there is no representativeness gap: it describes the artifact that actually ships, not a reconstruction.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// freehandRetentionProfileFile is the resolved identity list persisted beside the baseline. The raw V8
// profile is ~84 MB and is NOT kept: the resolved list is what every later check needs.
const freehandRetentionProfileFile = "retained_identities.txt"

// freehandRetentionProfilePath is where the build writes the raw V8 profile. It lives in Soroq-owned
// build state, never under the customer's lib/.
func freehandRetentionProfilePath(projectDir string) string {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return ""
	}
	// gen_snapshot receives this through Flutter's comma-separated --extra-gen-snapshot-options, which
	// cannot encode a comma in a value. Refuse rather than emit a silently truncated path.
	p := filepath.Join(abs, ".soroq", "build", "snapshot_profile.heapsnapshot")
	if strings.ContainsAny(p, ",") {
		return ""
	}
	return p
}

// v8Profile is the subset of the V8 heap-snapshot format Dart emits that we need.
type v8Profile struct {
	Snapshot struct {
		Meta struct {
			NodeFields []string   `json:"node_fields"`
			NodeTypes  [][]string `json:"node_types"`
			EdgeFields []string   `json:"edge_fields"`
			EdgeTypes  [][]string `json:"edge_types"`
		} `json:"meta"`
	} `json:"snapshot"`
	Nodes   []int    `json:"nodes"`
	Edges   []int    `json:"edges"`
	Strings []string `json:"strings"`
}

// resolveRetainedIdentitiesFromProfile parses a V8 snapshot profile and returns the canonical, sorted set
// of retained function identities.
func resolveRetainedIdentitiesFromProfile(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read snapshot profile: %w", err)
	}
	var p v8Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode snapshot profile: %w", err)
	}
	return resolveRetainedIdentities(&p)
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

func resolveRetainedIdentities(p *v8Profile) ([]string, error) {
	nf, ef := p.Snapshot.Meta.NodeFields, p.Snapshot.Meta.EdgeFields
	if len(p.Snapshot.Meta.NodeTypes) == 0 || len(p.Snapshot.Meta.EdgeTypes) == 0 {
		return nil, fmt.Errorf("snapshot profile has no node/edge type table")
	}
	nodeTypes, edgeTypes := p.Snapshot.Meta.NodeTypes[0], p.Snapshot.Meta.EdgeTypes[0]
	ti, ni, eci := indexOf(nf, "type"), indexOf(nf, "name"), indexOf(nf, "edge_count")
	eti, eni, eto := indexOf(ef, "type"), indexOf(ef, "name_or_index"), indexOf(ef, "to_node")
	if ti < 0 || ni < 0 || eci < 0 || eti < 0 || eni < 0 || eto < 0 {
		return nil, fmt.Errorf("snapshot profile is missing required node/edge fields")
	}
	k, ek := len(nf), len(ef)
	if k == 0 || ek == 0 || len(p.Nodes)%k != 0 {
		return nil, fmt.Errorf("snapshot profile node array is not a multiple of the field count")
	}
	n := len(p.Nodes) / k

	// Node index -> offset of its first edge. Edges are stored contiguously in node order.
	edgeOff := make([]int, n)
	cur := 0
	for i := 0; i < n; i++ {
		edgeOff[i] = cur
		cur += p.Nodes[i*k+eci]
	}

	str := func(i int) string {
		if i >= 0 && i < len(p.Strings) {
			return p.Strings[i]
		}
		return ""
	}
	typeOf := func(idx int) string {
		if t := p.Nodes[idx*k+ti]; t >= 0 && t < len(nodeTypes) {
			return nodeTypes[t]
		}
		return ""
	}
	nameOf := func(idx int) string { return str(p.Nodes[idx*k+ni]) }

	// props returns the "property"-typed outgoing edges of a node as name -> target node index.
	props := func(idx int) map[string]int {
		out := map[string]int{}
		st := edgeOff[idx]
		for j := st; j < st+p.Nodes[idx*k+eci]; j++ {
			if j*ek+eto >= len(p.Edges) {
				break
			}
			if t := p.Edges[j*ek+eti]; t >= 0 && t < len(edgeTypes) && edgeTypes[t] == "property" {
				out[str(p.Edges[j*ek+eni])] = p.Edges[j*ek+eto] / k
			}
		}
		return out
	}

	// resolveOwner maps a Class/PatchClass node to (libraryUri, className).
	type owner struct{ lib, cls string }
	cache := map[int]*owner{}
	var resolveOwner func(idx, depth int) *owner
	resolveOwner = func(idx, depth int) *owner {
		if o, ok := cache[idx]; ok {
			return o
		}
		if depth > 6 || idx < 0 || idx >= n {
			return nil
		}
		pr := props(idx)
		var res *owner
		switch typeOf(idx) {
		case "PatchClass":
			// A PatchClass wraps the class being patched; follow through to the real Class.
			for _, key := range []string{"wrapped_class_", "patched_class_", "origin_class_"} {
				if t, ok := pr[key]; ok {
					res = resolveOwner(t, depth+1)
					break
				}
			}
		case "Class":
			lib := ""
			if l, ok := pr["library_"]; ok && typeOf(l) == "Library" {
				if u, ok := props(l)["url_"]; ok {
					lib = nameOf(u)
				} else {
					lib = nameOf(l)
				}
			}
			cls := nameOf(idx)
			if c, ok := pr["name_"]; ok {
				cls = nameOf(c)
			}
			// Dart names the top-level container "::"; the manifest reader emits "" for it
			// (`cls.IsTopLevel() ? ""`), so normalize to match.
			if cls == "::" || cls == "<top-level>" {
				cls = ""
			}
			if lib != "" {
				res = &owner{lib: lib, cls: cls}
			}
		}
		cache[idx] = res
		return res
	}

	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if typeOf(i) != "Function" {
			continue
		}
		pr := props(i)
		name := nameOf(i)
		if nm, ok := pr["name_"]; ok {
			name = nameOf(nm)
		}
		// gen_snapshot writes "<optimized out>" for functions whose identity was dropped; they are not
		// addressable by name and must not be presented as retained.
		if name == "" || name == "<optimized out>" {
			continue
		}
		ow, ok := pr["owner_"]
		if !ok {
			continue
		}
		o := resolveOwner(ow, 0)
		if o == nil {
			continue
		}
		seen[o.lib+"::"+o.cls+"::"+name] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("snapshot profile resolved ZERO retained functions; the profile is empty or its schema changed")
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}
