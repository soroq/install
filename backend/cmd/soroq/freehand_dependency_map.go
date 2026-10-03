package main

// Soroq freehand — the R8 DEPENDENCY MAP.
//
// WHY IT EXISTS. Engines R6/R7 guarantee that a redirect takes effect everywhere with "Path A": the
// compiler never inlines a patchable function and never folds a patchable static callee's inferred
// result into its callers. That keeps every call into app code a real, boxed call, and it costs a lot of
// speed (5x measured on small numeric calls).
//
// R8 lets gen_snapshot optimize like stock Flutter when it is given --soroq_dependency_map=<path>, and
// RECORDS every place it copied a patchable function into other code, one line per edge:
//
//	<why>\t<caller-patchable 0|1>\t<caller identity>\t<callee identity>\n
//
// why is "inline" (the callee's body was inlined into the caller) or "result" (a patchable STATIC
// callee's inferred result was folded into the caller). Identities are the manifest's
// `<library>::<class>::<member>` of the OUTERMOST function; the callee is always patchable.
//
// A redirect on a changed function F changes nothing inside a caller X that absorbed F's body or result:
// X runs its own copy. So a patch must replace X too -- recompiled from UNCHANGED source into module
// bytecode that calls the new F -- and every caller of X that absorbed X, transitively. If any of those
// callers is not patchable, OTA cannot deliver the change at all and the patch is refused.
//
// The map is part of the immutable base: written by the release, bound into baseline.json by digest, and
// required by every patch against a base whose engine declared the capability. A base built WITHOUT the
// capability must carry no map, and patches against it behave exactly as before.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// freehandDependencyMapCapability is the identity capability an engine bundle declares when its
// gen_snapshot understands --soroq_dependency_map. It is closed-set like every other capability.
const freehandDependencyMapCapability = "soroq_dependency_map_v1"

// freehandDependencyMapFile is the canonical map persisted beside the manifest in the immutable baseline.
const freehandDependencyMapFile = "soroq_dependency_map.tsv"

// freehandDependencyMapSchema tags the recorded encoding (canonical TSV: deduplicated, sorted, one edge
// per line, every line newline-terminated).
const freehandDependencyMapSchema = "soroq.freehand.dependency_map.v1"

// freehandDependencyMapFlag is the gen_snapshot flag (runtime/vm/object.cc, R8).
const freehandDependencyMapFlag = "--soroq_dependency_map"

// freehandDependencyEdge is one recorded copy of a patchable callee into a caller.
type freehandDependencyEdge struct {
	Why             string // "inline" | "result"
	CallerPatchable bool
	Caller          string
	Callee          string
}

func (e freehandDependencyEdge) line() string {
	flag := "0"
	if e.CallerPatchable {
		flag = "1"
	}
	return e.Why + "\t" + flag + "\t" + e.Caller + "\t" + e.Callee
}

// validDependencyIdentity checks the `<lib>::<class>::<member>` shape: exactly three segments, a non-empty
// library and member, and no whitespace a line format could be confused by.
func validDependencyIdentity(id string) error {
	if id == "" {
		return errors.New("empty identity")
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return fmt.Errorf("identity %q contains whitespace", id)
	}
	lib, _, member, err := splitIdentity(id)
	if err != nil {
		return err
	}
	if lib == "" || member == "" {
		return fmt.Errorf("identity %q has an empty library or member", id)
	}
	return nil
}

// parseFreehandDependencyMap strictly parses the engine's map (raw or canonical form). ANY malformed line
// is an error: a map this code cannot read completely is a map that could be missing the one edge that
// matters, and treating it as partially valid is how a patch silently misses a caller.
//
// Duplicate lines are legal (the engine writes one line per inlining decision) and are deduplicated. The
// same caller recorded as both patchable and not patchable is NOT legal: the engine would have answered
// SoroqIsPatchable two ways for one identity.
func parseFreehandDependencyMap(raw []byte) ([]freehandDependencyEdge, error) {
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		return nil, errors.New("dependency map does not end with a newline (truncated write?)")
	}
	seen := map[string]bool{}
	callerFlag := map[string]bool{}
	var edges []freehandDependencyEdge
	lineNo := 0
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		if line == "" {
			continue // the empty remainder after the final newline
		}
		lineNo++
		line = strings.TrimSuffix(line, "\n")
		if strings.Contains(line, "\r") {
			return nil, fmt.Errorf("dependency map line %d contains a carriage return", lineNo)
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			return nil, fmt.Errorf("dependency map line %d has %d tab-separated fields, want 4: %q", lineNo, len(f), line)
		}
		why, flag, caller, callee := f[0], f[1], f[2], f[3]
		if why != "inline" && why != "result" {
			return nil, fmt.Errorf("dependency map line %d: why %q is not inline|result", lineNo, why)
		}
		if flag != "0" && flag != "1" {
			return nil, fmt.Errorf("dependency map line %d: caller-patchable %q is not 0|1", lineNo, flag)
		}
		if err := validDependencyIdentity(caller); err != nil {
			return nil, fmt.Errorf("dependency map line %d: caller: %w", lineNo, err)
		}
		if err := validDependencyIdentity(callee); err != nil {
			return nil, fmt.Errorf("dependency map line %d: callee: %w", lineNo, err)
		}
		if caller == callee {
			return nil, fmt.Errorf("dependency map line %d records %s absorbing itself", lineNo, caller)
		}
		patchable := flag == "1"
		if prev, ok := callerFlag[caller]; ok && prev != patchable {
			return nil, fmt.Errorf("dependency map records caller %s as both patchable and not patchable", caller)
		}
		callerFlag[caller] = patchable
		e := freehandDependencyEdge{Why: why, CallerPatchable: patchable, Caller: caller, Callee: callee}
		if seen[e.line()] {
			continue
		}
		seen[e.line()] = true
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].line() < edges[j].line() })
	return edges, nil
}

// renderFreehandDependencyMap is the canonical encoding: sorted, deduplicated, newline-terminated. An
// empty map is zero bytes.
func renderFreehandDependencyMap(edges []freehandDependencyEdge) []byte {
	var b bytes.Buffer
	for _, e := range edges {
		b.WriteString(e.line())
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// manifestLineSet is the set of non-empty manifest lines.
func manifestLineSet(manifest []byte) map[string]bool {
	out := map[string]bool{}
	for _, l := range strings.Split(string(manifest), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out[l] = true
		}
	}
	return out
}

// checkDependencyMapAgainstManifest is the identity sanity check: the engine and the analyzer must agree
// on every identity the map names. A callee is patchable by construction, so it must be a manifest line;
// a caller recorded patchable must be one too; and a caller recorded NOT patchable must not be, because
// then the engine answered SoroqIsPatchable differently from the manifest it was given. Any disagreement
// means the closure computed at patch time would run over names that do not denote the functions the
// engine copied, so it fails closed.
func checkDependencyMapAgainstManifest(edges []freehandDependencyEdge, manifest []byte) error {
	lines := manifestLineSet(manifest)
	var bad []string
	for _, e := range edges {
		if !lines[e.Callee] {
			bad = append(bad, fmt.Sprintf("callee %s (%s into %s) is not in the patchable manifest", e.Callee, e.Why, e.Caller))
		}
		if e.CallerPatchable && !lines[e.Caller] {
			bad = append(bad, fmt.Sprintf("caller %s is recorded patchable but is not in the patchable manifest", e.Caller))
		}
		if !e.CallerPatchable && lines[e.Caller] {
			bad = append(bad, fmt.Sprintf("caller %s is recorded NOT patchable but is in the patchable manifest", e.Caller))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	bad = dedupeStrings(sortedCopy(bad))
	return fmt.Errorf("the engine's dependency map and the analyzer's manifest disagree on identities, so no patch could be expanded over this map correctly:\n  - %s", strings.Join(bad, "\n  - "))
}

func sortedCopy(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------------------------
// RELEASE SIDE
// ---------------------------------------------------------------------------------------------

// freehandDependencyMapCapture is a map collected from THIS build, already parsed and canonicalized.
type freehandDependencyMapCapture struct {
	Canonical []byte
	Edges     int
	// CodeFingerprints is the canonical soroq_code_fingerprints.tsv from the same gen_snapshot run, or nil
	// when the engine does not declare soroq_code_fingerprints_v1.
	CodeFingerprints []byte
}

// toolchainDeclaresDependencyMap reads the resolved toolchain's own engine.json. An absent capability
// block means no; a present but malformed block is an error (the engine tried to say something).
//
// A toolchain that is not named or not installed answers no here: the build itself refuses it a moment
// later with its own, more specific message, and no build means no base.
func toolchainDeclaresDependencyMap(toolchain string) (bool, error) {
	if strings.TrimSpace(toolchain) == "" {
		return false, nil
	}
	bundleDir, err := iosCachedToolchainBundleDir(toolchain)
	if err != nil {
		return false, nil
	}
	enginePath := filepath.Join(bundleDir, "engine.json")
	if _, err := os.Stat(enginePath); os.IsNotExist(err) {
		return false, nil
	}
	return engineBundleDeclaresDependencyMap(enginePath)
}

func engineBundleDeclaresDependencyMap(enginePath string) (bool, error) {
	raw, err := os.ReadFile(enginePath)
	if err != nil {
		return false, fmt.Errorf("read engine bundle %s: %w", enginePath, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, fmt.Errorf("engine bundle %s is not valid JSON: %w", enginePath, err)
	}
	decl, ok := doc[freehandEngineCapabilityKey]
	if !ok {
		return false, nil
	}
	_, caps, err := decodeEngineCapabilityDeclaration(decl)
	if err != nil {
		return false, fmt.Errorf("engine bundle %s declares a malformed %s: %w", enginePath, freehandEngineCapabilityKey, err)
	}
	for _, c := range caps {
		if c == freehandDependencyMapCapability {
			return true, nil
		}
	}
	return false, nil
}

// freehandDependencyMapBuildPath is where gen_snapshot is told to write the map: an ABSOLUTE path in the
// project's .soroq build staging. It goes through Flutter's comma-separated --extra-gen-snapshot-options,
// which cannot carry a comma, so such a path is refused rather than silently truncated.
func freehandDependencyMapBuildPath(projectDir string) (string, error) {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}
	p := filepath.Join(abs, ".soroq", "build", freehandDependencyMapFile)
	if strings.ContainsAny(p, ",") {
		return "", fmt.Errorf("the dependency-map path %s contains a comma, which gen_snapshot's option list cannot carry; move the project to a path without commas", p)
	}
	return p, nil
}

// prepareFreehandDependencyMap creates the staging directory and REMOVES any stale map. A leftover map
// from an earlier build adopted as this build's would describe different code; failing to remove one is
// therefore fatal.
func prepareFreehandDependencyMap(projectDir string) (string, error) {
	p, err := freehandDependencyMapBuildPath(projectDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("create the dependency-map staging directory: %w", err)
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove the stale dependency map %s: %w", p, err)
	}
	if _, err := os.Lstat(p); err == nil {
		return "", fmt.Errorf("a stale dependency map is still present at %s", p)
	}
	return p, nil
}

// freehandUserSetsDependencyMap reports whether the developer's own build arguments already name the
// flag. Two values would leave which file gen_snapshot wrote undefined, and a map written somewhere
// Soroq does not read is a base that inlined without a record.
func freehandUserSetsDependencyMap(passthrough []string) bool {
	return strings.Contains(strings.Join(passthrough, " "), "soroq_dependency_map")
}

// withFreehandDependencyMap is the release-side wiring: when the resolved toolchain's engine declares the
// capability, it clears any stale map and appends --extra-gen-snapshot-options=--soroq_dependency_map=<abs
// path> to the flutter build arguments. A developer-supplied flag is refused on every toolchain.
func withFreehandDependencyMap(projectDir, toolchain string, passthrough []string) ([]string, string, bool, error) {
	if freehandUserSetsDependencyMap(passthrough) {
		return nil, "", false, fmt.Errorf("the build arguments already pass %s; Soroq sets it itself on toolchains that declare %s, and a second value would leave the recorded map undefined", freehandDependencyMapFlag, freehandDependencyMapCapability)
	}
	declared, err := toolchainDeclaresDependencyMap(toolchain)
	if err != nil {
		return nil, "", false, fmt.Errorf("read the toolchain's dependency-map capability: %w", err)
	}
	if !declared {
		return passthrough, "", false, nil
	}
	path, err := prepareFreehandDependencyMap(projectDir)
	if err != nil {
		return nil, "", false, err
	}
	out := append(append([]string(nil), passthrough...), "--extra-gen-snapshot-options="+freehandDependencyMapFlag+"="+path)
	fmt.Fprintf(os.Stderr, "soroq release ios --engine --build (freehand): engine declares %s; dependency map -> %s\n", freehandDependencyMapCapability, path)
	return out, path, true, nil
}

// freehandBuildLogShowsDependencyMapFlag scans the build logs this build wrote (.soroq/logs/*-ios-build.log
// modified at or after `since`) for a gen_snapshot command line carrying --soroq_dependency_map=<mapPath>.
// The log's own header ("$ flutter build ...") and Flutter's argument echo carry the flag inside
// --extra-gen-snapshot-options and are NOT evidence that gen_snapshot received it, so such lines are skipped.
func freehandBuildLogShowsDependencyMapFlag(projectDir, mapPath string, since time.Time) (bool, string, error) {
	logsDir := filepath.Join(projectDir, ".soroq", "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	want := freehandDependencyMapFlag + "=" + mapPath
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "-ios-build.log") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(since) {
			continue
		}
		p := filepath.Join(logsDir, e.Name())
		f, err := os.Open(p)
		if err != nil {
			return false, "", err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		found := false
		for sc.Scan() {
			l := sc.Text()
			if strings.HasPrefix(l, "$ ") || strings.Contains(l, "extra-gen-snapshot-options") {
				continue
			}
			if strings.Contains(l, "gen_snapshot") && containsFlagToken(l, want) {
				found = true
				break
			}
		}
		serr := sc.Err()
		f.Close()
		if serr != nil {
			return false, "", fmt.Errorf("read build log %s: %w", p, serr)
		}
		if found {
			return true, p, nil
		}
	}
	return false, "", nil
}

// containsFlagToken reports whether `want` occurs in l as a whole token (not a prefix of a longer path).
func containsFlagToken(l, want string) bool {
	for i := strings.Index(l, want); i >= 0; {
		end := i + len(want)
		if end == len(l) || strings.ContainsRune(" \t\"',", rune(l[end])) {
			return true
		}
		next := strings.Index(l[end:], want)
		if next < 0 {
			return false
		}
		i = end + next
	}
	return false
}

// collectFreehandDependencyMap reads the map THIS build's gen_snapshot wrote, after a successful build.
//
// The engine writes the file lazily, on the first edge, so a build that inlined nothing leaves no file.
// That is only indistinguishable from "gen_snapshot never received the flag" if nothing else is checked,
// and the second case is a base that may have inlined with no record. So a missing file becomes an EMPTY
// map only when this build's log shows gen_snapshot running with the flag; otherwise the release fails.
func collectFreehandDependencyMap(projectDir, mapPath string, buildStart time.Time) (*freehandDependencyMapCapture, error) {
	fi, err := os.Lstat(mapPath)
	switch {
	case err == nil:
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("the dependency map at %s is not a regular file", mapPath)
		}
		if fi.ModTime().Before(buildStart.Add(-2 * time.Second)) {
			return nil, fmt.Errorf("the dependency map at %s predates this build; refusing to adopt a stale map", mapPath)
		}
		raw, err := os.ReadFile(mapPath)
		if err != nil {
			return nil, err
		}
		edges, err := parseFreehandDependencyMap(raw)
		if err != nil {
			return nil, fmt.Errorf("the dependency map gen_snapshot wrote is malformed: %w", err)
		}
		return &freehandDependencyMapCapture{Canonical: renderFreehandDependencyMap(edges), Edges: len(edges)}, nil
	case os.IsNotExist(err):
		ok, logPath, lerr := freehandBuildLogShowsDependencyMapFlag(projectDir, mapPath, buildStart.Add(-2*time.Second))
		if lerr != nil {
			return nil, fmt.Errorf("the build produced no dependency map and its build log could not be read: %w", lerr)
		}
		if !ok {
			return nil, fmt.Errorf("this toolchain's engine declares %s, but the build produced no dependency map at %s and "+
				"its build log does not show gen_snapshot running with %s=%s. Either gen_snapshot never received the "+
				"flag -- and then this base inlined patchable code with no record, so no patch could be expanded "+
				"over it -- or the app genuinely inlined nothing. Refusing to guess: rebuild with verbose Flutter "+
				"output so the build log records the gen_snapshot command line, or check that nothing strips "+
				"--extra-gen-snapshot-options", freehandDependencyMapCapability, mapPath, freehandDependencyMapFlag, mapPath)
		}
		fmt.Fprintf(os.Stderr, "dependency map: gen_snapshot ran with %s (%s) and recorded no edges; recording an empty map\n",
			freehandDependencyMapFlag, logPath)
		return &freehandDependencyMapCapture{Canonical: []byte{}, Edges: 0}, nil
	default:
		return nil, fmt.Errorf("stat the dependency map %s: %w", mapPath, err)
	}
}

// ---------------------------------------------------------------------------------------------
// BASELINE BINDING
// ---------------------------------------------------------------------------------------------

// baseDeclaresDependencyMap answers from the base's RECORDED capability set. Absent/legacy never
// declares it.
func baseDeclaresDependencyMap(m *FreehandBaselineMeta) (bool, error) {
	caps, err := baseRedirectCapabilities(m)
	if err != nil {
		return false, err
	}
	return caps.hasIdentityCapability(freehandDependencyMapCapability), nil
}

// verifyBaselineDependencyMap checks the map in BOTH directions, like the interface files and the
// obfuscation map: a base whose engine declared the capability must carry a well-formed, canonical map
// hashing to the recorded digest whose identities agree with the manifest; a base whose engine did not
// must record nothing and carry no file (a stray map inside an immutable directory is refused, not read).
func verifyBaselineDependencyMap(relDir string, m *FreehandBaselineMeta, manifest []byte) error {
	_, err := loadVerifiedBaselineDependencyMap(relDir, m, manifest)
	return err
}

// loadVerifiedBaselineDependencyMap returns the base's edges (nil for a base without the capability).
func loadVerifiedBaselineDependencyMap(relDir string, m *FreehandBaselineMeta, manifest []byte) ([]freehandDependencyEdge, error) {
	declared, err := baseDeclaresDependencyMap(m)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(relDir, freehandDependencyMapFile)
	if !declared {
		if m.DependencyMapSchema != "" || m.DependencyMapSHA256 != "" || m.DependencyMapEdges != 0 {
			return nil, fmt.Errorf("baseline %s records a dependency map but its engine does not declare %s", relDir, freehandDependencyMapCapability)
		}
		if _, err := os.Lstat(p); err == nil {
			return nil, fmt.Errorf("baseline %s contains %s but its engine does not declare %s", relDir, freehandDependencyMapFile, freehandDependencyMapCapability)
		}
		return nil, nil
	}
	if m.DependencyMapSchema != freehandDependencyMapSchema {
		return nil, fmt.Errorf("baseline %s was built by an engine declaring %s but records dependency map schema %q, want %q",
			relDir, freehandDependencyMapCapability, m.DependencyMapSchema, freehandDependencyMapSchema)
	}
	if !sha256HexRe.MatchString(m.DependencyMapSHA256) {
		return nil, fmt.Errorf("baseline %s records no valid digest for %s", relDir, freehandDependencyMapFile)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("baseline built with %s is missing %s: %w", freehandDependencyMapCapability, freehandDependencyMapFile, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("baseline %s is not a regular file", freehandDependencyMapFile)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if got := freehandSHA256Bytes(raw); got != m.DependencyMapSHA256 {
		return nil, fmt.Errorf("baseline %s hash mismatch: %s != recorded %s", freehandDependencyMapFile, got, m.DependencyMapSHA256)
	}
	edges, err := parseFreehandDependencyMap(raw)
	if err != nil {
		return nil, fmt.Errorf("baseline %s: %w", freehandDependencyMapFile, err)
	}
	if !bytes.Equal(renderFreehandDependencyMap(edges), raw) {
		return nil, fmt.Errorf("baseline %s is not in canonical form (sorted, deduplicated)", freehandDependencyMapFile)
	}
	if len(edges) != m.DependencyMapEdges {
		return nil, fmt.Errorf("baseline %s has %d edges, recorded %d", freehandDependencyMapFile, len(edges), m.DependencyMapEdges)
	}
	if err := checkDependencyMapAgainstManifest(edges, manifest); err != nil {
		return nil, err
	}
	return edges, nil
}

// ---------------------------------------------------------------------------------------------
// PATCH SIDE: EXPANSION
// ---------------------------------------------------------------------------------------------

// FreehandDependencyMapCaller is one caller the dependency map added to a patch: its identity, the frozen
// key it was compiled under, and the edges that pulled it in. Recorded in the patch plan (and therefore
// bound into the artifact id) so every artifact says why it replaces code whose source did not change.
type FreehandDependencyMapCaller struct {
	Identity string                     `json:"identity"`
	Key      string                     `json:"key"`
	Kind     string                     `json:"kind"`
	Absorbed []FreehandDependencyAbsorb `json:"absorbed"`
}

// FreehandDependencyAbsorb is one edge: this caller holds a copy of Callee (inlined body or folded result).
type FreehandDependencyAbsorb struct {
	Why    string `json:"why"`
	Callee string `json:"callee"`
}

// freehandDependencyClosure computes, over the map, every caller that must be replaced along with the
// changed set: repeatedly add each caller X with an edge X -> c for c already in the set. It returns the
// added callers (sorted, with the edges that reached each) and one refusal per non-patchable caller that
// absorbed something in the set. Cycles terminate because a caller joins the set once.
func freehandDependencyClosure(edges []freehandDependencyEdge, changed []string) ([]FreehandDependencyMapCaller, []string) {
	byCallee := map[string][]freehandDependencyEdge{}
	for _, e := range edges {
		byCallee[e.Callee] = append(byCallee[e.Callee], e)
	}
	inSet := map[string]bool{}
	origin := map[string]string{} // member of the set -> the originally changed identity that reached it
	queue := []string{}
	for _, c := range sortedCopy(changed) {
		if !inSet[c] {
			inSet[c] = true
			origin[c] = c
			queue = append(queue, c)
		}
	}
	added := map[string]*FreehandDependencyMapCaller{}
	refusals := map[string]bool{}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		for _, e := range byCallee[c] {
			if !e.CallerPatchable {
				var msg string
				if e.Why == "result" {
					msg = fmt.Sprintf("%s's result was folded into %s, which OTA cannot replace; ship a store release", e.Callee, e.Caller)
				} else {
					msg = fmt.Sprintf("%s was inlined into %s, which OTA cannot replace; ship a store release", e.Callee, e.Caller)
				}
				if o := origin[c]; o != c {
					msg += fmt.Sprintf(" (reached from the changed %s)", o)
				}
				refusals[msg] = true
				continue
			}
			if a, ok := added[e.Caller]; ok {
				a.Absorbed = append(a.Absorbed, FreehandDependencyAbsorb{Why: e.Why, Callee: e.Callee})
			}
			if inSet[e.Caller] {
				continue
			}
			inSet[e.Caller] = true
			origin[e.Caller] = origin[c]
			added[e.Caller] = &FreehandDependencyMapCaller{
				Identity: e.Caller,
				Absorbed: []FreehandDependencyAbsorb{{Why: e.Why, Callee: e.Callee}},
			}
			queue = append(queue, e.Caller)
		}
	}
	out := make([]FreehandDependencyMapCaller, 0, len(added))
	for _, a := range added {
		sort.Slice(a.Absorbed, func(i, j int) bool {
			if a.Absorbed[i].Callee != a.Absorbed[j].Callee {
				return a.Absorbed[i].Callee < a.Absorbed[j].Callee
			}
			return a.Absorbed[i].Why < a.Absorbed[j].Why
		})
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	msgs := make([]string, 0, len(refusals))
	for m := range refusals {
		msgs = append(msgs, m)
	}
	sort.Strings(msgs)
	return out, msgs
}

// freehandKernelSymbol is one declaration from the analyzer's symbol graph (identity schema v1).
type freehandKernelSymbol struct {
	Key             string `json:"key"`
	LibURI          string `json:"libUri"`
	Class           string `json:"class"`
	Member          string `json:"member"`
	Kind            string `json:"kind"`
	SignatureDigest string `json:"signatureDigest"`
	ManifestLine    string `json:"manifestLine"`
	Patchable       bool   `json:"patchable"`
}

// freehandResolvedIdentity is the frozen v1 key + semantic kind of a manifest identity in the CANDIDATE
// source kernel -- the kernel the module synthesizer extracts from.
type freehandResolvedIdentity struct {
	Key  string
	Kind string
}

// freehandExpansionKernels supplies the symbols of the two SOURCE kernels the expansion needs. Both are
// lazy: a patch that adds no caller analyzes nothing, and the base kernel is analyzed only when an added
// caller runs on a receiver.
type freehandExpansionKernels struct {
	Candidate func() ([]freehandKernelSymbol, error)
	Base      func() ([]freehandKernelSymbol, error)
}

// freehandAnalyzeKernelSymbolsFn is the production analysis; a seam so tests need no Dart.
var freehandAnalyzeKernelSymbolsFn = analyzeFreehandKernelSymbols

// analyzeFreehandKernelSymbols runs the installed analyzer in its normal (manifest) mode over a SOURCE
// kernel and returns every symbol it enumerates.
//
// The caller keys must come from the CANDIDATE source kernel, not from the base's symbol_graph.json: the
// base graph was measured on the AOT app.dill, where type-flow analysis may have shaken a signature, and
// the synthesizer looks declarations up by the candidate kernel's key.
func analyzeFreehandKernelSymbols(flutterRoot, dill, packageConfig string) ([]freehandKernelSymbol, error) {
	dart := filepath.Join(flutterRoot, "bin", "cache", "dart-sdk", "bin", "dart")
	analyzer := filepath.Join(flutterRoot, filepath.FromSlash(freehandAnalyzerRelPath))
	for _, p := range []string{dart, analyzer, dill, packageConfig} {
		if !fileExists(p) {
			return nil, fmt.Errorf("kernel symbol analysis input missing: %s", p)
		}
	}
	out, err := os.MkdirTemp("", "soroq-freehand-symbols-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(out)
	cmd := exec.Command(dart, analyzer, "--dill", dill, "--package-config", packageConfig, "--out", out)
	if log, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("analyze the symbols of %s: %w\n%s", filepath.Base(dill), err, string(log))
	}
	raw, err := os.ReadFile(filepath.Join(out, "symbol_graph.json"))
	if err != nil {
		return nil, fmt.Errorf("read the symbol graph of %s: %w", filepath.Base(dill), err)
	}
	return parseFreehandSymbolGraph(raw)
}

func parseFreehandSymbolGraph(raw []byte) ([]freehandKernelSymbol, error) {
	var g struct {
		Schema  string                 `json:"schema"`
		Symbols []freehandKernelSymbol `json:"symbols"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("parse the symbol graph: %w", err)
	}
	if g.Schema != freehandIdentitySchema {
		return nil, fmt.Errorf("symbol graph schema %q != %q", g.Schema, freehandIdentitySchema)
	}
	return g.Symbols, nil
}

// resolveIdentities maps each manifest line to its unique PATCHABLE symbol.
func resolveIdentities(symbols []freehandKernelSymbol, lines []string) (map[string]freehandResolvedIdentity, error) {
	want := map[string]bool{}
	for _, l := range lines {
		want[l] = true
	}
	found := map[string][]freehandResolvedIdentity{}
	for _, s := range symbols {
		if want[s.ManifestLine] && s.Patchable {
			found[s.ManifestLine] = append(found[s.ManifestLine], freehandResolvedIdentity{Key: s.Key, Kind: s.Kind})
		}
	}
	out := map[string]freehandResolvedIdentity{}
	var bad []string
	for _, l := range sortedCopy(lines) {
		switch n := len(found[l]); n {
		case 1:
			out[l] = found[l][0]
		case 0:
			bad = append(bad, l+": no patchable declaration with this identity in the candidate kernel")
		default:
			bad = append(bad, fmt.Sprintf("%s: %d patchable declarations share this identity in the candidate kernel", l, n))
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("could not resolve the callers the dependency map adds:\n  - %s", strings.Join(bad, "\n  - "))
	}
	return out, nil
}

// freehandReceiverKinds are the semantic kinds whose replacement runs on a BASE receiver.
var freehandReceiverKinds = map[string]bool{"method": true, "getter": true, "setter": true, "operator": true, "constructor": true}

// classFieldLayouts is, per class present in the graph, its declared fields (name + type digest), sorted.
func classFieldLayouts(symbols []freehandKernelSymbol) map[string]string {
	fields := map[string][]string{}
	classes := map[string]bool{}
	for _, s := range symbols {
		if s.Class == "" {
			continue
		}
		ck := s.LibURI + "::" + s.Class
		classes[ck] = true
		if s.Kind == "field-initializer" {
			fields[ck] = append(fields[ck], s.Member+":"+s.SignatureDigest)
		}
	}
	out := map[string]string{}
	for ck := range classes {
		out[ck] = strings.Join(sortedCopy(fields[ck]), ",")
	}
	return out
}

// assertNoClassLayoutChange is the analyzer's class-shape rule, applied to the callers the map adds.
//
// The diff refuses a CHANGED instance member whose class's field layout moved, because the replacement
// runs on a BASE receiver. An added caller runs on a base receiver too, but the diff never looked at it:
// its body did not change, so it is not in the diff at all. Its class, or a superclass of it, may still
// have gained a field this patch declares. The symbol graph does not record supertypes, so the rule is
// deliberately conservative: when any added caller runs on a receiver, EVERY class present in both the
// base and the candidate source kernel must declare exactly the same fields.
func assertNoClassLayoutChange(base, candidate []freehandKernelSymbol, receivers []string) error {
	b, c := classFieldLayouts(base), classFieldLayouts(candidate)
	var moved []string
	for ck, bl := range b {
		if cl, ok := c[ck]; ok && cl != bl {
			moved = append(moved, fmt.Sprintf("%s: [%s] -> [%s]", ck, bl, cl))
		}
	}
	if len(moved) == 0 {
		return nil
	}
	sort.Strings(moved)
	return fmt.Errorf("the dependency map adds caller(s) that run on a base receiver (%s), and this patch changes the declared fields of class(es) that exist in the base:\n  - %s\n"+
		"  A replacement running on a base receiver requires every class layout to be unchanged, exactly as for a changed instance member. "+
		"Keep the field change out of this patch, or ship a store release",
		strings.Join(receivers, ", "), strings.Join(moved, "\n  - "))
}

// expandFreehandChangedSetOverDependencyMap is the patch-side hook. It runs right after the diff and
// BEFORE every gate, so the private-identity gate, the redirect-capability gate, the fold check, module
// synthesis, the ABI bijection check and interface validation all see one consistent changed set.
//
// A base without the capability returns (nil, nil) and touches nothing, so its patches are byte-for-byte
// what they were. A base with a map: the closure over the map is computed from the changed-patchable
// identities; any non-patchable absorber refuses the patch; every added caller is appended to the diff's
// `changed` array as a changed PATCHABLE declaration (its candidate source equals its base source, which
// is the point: its bytecode calls the NEW callee instead of running the old inlined copy), and the diff
// JSON the synthesizer reads is rewritten to match.
func expandFreehandChangedSetOverDependencyMap(base *FreehandBaselineMeta, relDir string, rep *FreehandDiffReport, diffJSONPath string, kernels freehandExpansionKernels) ([]FreehandDependencyMapCaller, error) {
	declared, err := baseDeclaresDependencyMap(base)
	if err != nil {
		return nil, fmt.Errorf("cannot decide whether this base carries a dependency map: %w", err)
	}
	if !declared {
		return nil, nil
	}
	manifest, err := os.ReadFile(filepath.Join(relDir, "soroq_app_manifest.txt"))
	if err != nil {
		return nil, fmt.Errorf("read the base manifest: %w", err)
	}
	edges, err := loadVerifiedBaselineDependencyMap(relDir, base, manifest)
	if err != nil {
		return nil, fmt.Errorf("load the base's dependency map: %w", err)
	}
	decls, err := changedDeclsFromDiff(rep.Changed)
	if err != nil {
		return nil, err
	}
	changed := make([]string, 0, len(decls))
	for _, d := range decls {
		changed = append(changed, d.manifestLine)
	}
	added, refusals := freehandDependencyClosure(edges, changed)
	if len(refusals) > 0 {
		return nil, fmt.Errorf("the base's compiler copied changed code into code a patch cannot replace (dependency map):\n  - %s",
			strings.Join(refusals, "\n  - "))
	}
	if len(added) == 0 {
		return nil, nil
	}
	lines := manifestLineSet(manifest)
	ids := make([]string, 0, len(added))
	for _, a := range added {
		if !lines[a.Identity] {
			return nil, fmt.Errorf("dependency map caller %s is not in the base manifest", a.Identity)
		}
		ids = append(ids, a.Identity)
	}
	if kernels.Candidate == nil || kernels.Base == nil {
		return nil, errors.New("internal: no kernel analysis for the dependency-map expansion")
	}
	candSymbols, err := kernels.Candidate()
	if err != nil {
		return nil, err
	}
	resolved, err := resolveIdentities(candSymbols, ids)
	if err != nil {
		return nil, err
	}
	existingKeys := map[string]bool{}
	for _, c := range append(append([]map[string]any(nil), rep.Changed...), rep.NewCodeClosure...) {
		if k, ok := c["key"].(string); ok {
			existingKeys[k] = true
		}
	}
	for i := range added {
		r, ok := resolved[added[i].Identity]
		if !ok {
			return nil, fmt.Errorf("dependency map caller %s did not resolve to a candidate declaration", added[i].Identity)
		}
		parts, pok := splitFrozenIdentityKey(r.Key)
		lib, cls, _, serr := splitIdentity(added[i].Identity)
		if serr != nil || !pok || parts[1] != lib || parts[3] != cls || parts[2] != r.Kind || !freehandSemanticKinds[r.Kind] {
			return nil, fmt.Errorf("dependency map caller %s resolved to an inconsistent frozen key %q (kind %q)", added[i].Identity, r.Key, r.Kind)
		}
		if existingKeys[r.Key] {
			return nil, fmt.Errorf("dependency map caller %s resolved to key %s, which the diff already carries", added[i].Identity, r.Key)
		}
		existingKeys[r.Key] = true
		added[i].Key = r.Key
		added[i].Kind = r.Kind
	}
	var receivers []string
	for _, a := range added {
		if _, cls, _, _ := splitIdentity(a.Identity); cls != "" && freehandReceiverKinds[a.Kind] {
			receivers = append(receivers, a.Identity)
		}
	}
	if len(receivers) > 0 {
		baseSymbols, err := kernels.Base()
		if err != nil {
			return nil, err
		}
		if err := assertNoClassLayoutChange(baseSymbols, candSymbols, receivers); err != nil {
			return nil, err
		}
	}
	// A caller that absorbed (inlined) a FORCED declaration is forced too: its machine code names the
	// same objects as the base's while what the inlined code must load changed (measured: main inlined
	// bump/peek, was pruned as identical, and kept counting in the base's storage). Transitive.
	forced := map[string]bool{}
	for _, c := range rep.Changed {
		if f, _ := c["forced"].(bool); f {
			if ml, _ := c["manifestLine"].(string); ml != "" {
				forced[ml] = true
			}
		}
	}
	for grew := true; grew; {
		grew = false
		for _, a := range added {
			if forced[a.Identity] {
				continue
			}
			for _, ab := range a.Absorbed {
				if forced[ab.Callee] {
					forced[a.Identity] = true
					grew = true
					break
				}
			}
		}
	}
	for _, a := range added {
		absorbed := make([]any, 0, len(a.Absorbed))
		for _, ab := range a.Absorbed {
			absorbed = append(absorbed, map[string]any{"why": ab.Why, "callee": ab.Callee})
		}
		entry := map[string]any{
			"key":                 a.Key,
			"kind":                a.Kind,
			"manifestLine":        a.Identity,
			"patchable":           true,
			"dependencyMapCaller": true,
			"absorbed":            absorbed,
		}
		if forced[a.Identity] {
			entry["forced"] = true
		}
		rep.Changed = append(rep.Changed, entry)
	}
	sort.SliceStable(rep.Changed, func(i, j int) bool {
		ki, _ := rep.Changed[i]["key"].(string)
		kj, _ := rep.Changed[j]["key"].(string)
		return ki < kj
	})
	// changedPatchable follows the analyzer's own order: the manifest lines of the patchable changed
	// entries, in key order.
	cp := make([]string, 0, len(rep.Changed))
	for _, c := range rep.Changed {
		if p, _ := c["patchable"].(bool); p {
			if ml, ok := c["manifestLine"].(string); ok {
				cp = append(cp, ml)
			}
		}
	}
	rep.ChangedPatchable = cp
	if rep.Counts == nil {
		rep.Counts = map[string]int{}
	}
	rep.Counts["changed"] = len(rep.Changed)
	rep.Counts["changedPatchable"] = len(cp)
	rep.Counts["dependencyMapCallers"] = len(added)
	if err := rewriteFreehandDiffJSON(diffJSONPath, rep); err != nil {
		return nil, fmt.Errorf("rewrite the diff for the dependency-map expansion: %w", err)
	}
	fmt.Fprintf(os.Stdout, "dependency map: %d caller(s) added because they absorbed changed code: %s\n", len(added), strings.Join(ids, ", "))
	return added, nil
}

// rewriteFreehandDiffJSON replaces the three expanded fields of freehand_diff.json IN PLACE, preserving
// every other field the analyzer wrote (including ones this Go struct does not model), so the synthesizer
// reads exactly the set every gate checked.
func rewriteFreehandDiffJSON(path string, rep *FreehandDiffReport) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	set := func(k string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		doc[k] = b
		return nil
	}
	if err := set("changed", rep.Changed); err != nil {
		return err
	}
	if err := set("changedPatchable", rep.ChangedPatchable); err != nil {
		return err
	}
	if err := set("counts", rep.Counts); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSync(path, out, 0o600)
}
