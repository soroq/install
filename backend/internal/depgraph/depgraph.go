// Package depgraph resolves a Flutter/Dart project's RUNTIME dependency graph — the transitive closure
// of the application's `dependencies:` only, walked recursively through each package's own
// `dependencies:` — classifies each package's OTA capability (Dart-only vs native vs asset-bearing) from
// its real pubspec.yaml AND its on-disk contents, and produces a strict dependency descriptor comparing a
// base graph to a candidate graph. It is the single source of truth for "is this dependency change
// deliverable via a code-only OTA patch?".
//
// Runtime vs dev. `dev_dependencies` are NOT runtime packages: build_runner, analyzer, test and friends
// never reach the shipped app, so they are excluded from both eligibility and carriage. Code they
// GENERATE is ordinary application Dart source and remains fully patchable — the generator is excluded,
// its output is not.
//
// Machine independence. Nothing serialized here may contain a developer-local absolute path: a path or
// git package's identity is a deterministic source-tree hash (plus a project-relative path when the
// package lives inside the project), never `/Users/<someone>/...`.
package depgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Schema / generator identities. Bump GeneratorVersion whenever the capability policy, the runtime-graph
// walk, or a digest computation changes — a graph digest is only comparable within one generator version.
const (
	GraphSchema      = "soroq.freehand.dependency_graph.v1"
	DescriptorSchema = "soroq.freehand.dependency_descriptor.v3"
	GeneratorVersion = "soroq-depgraph/4"
)

// SourceType is a resolved package's pub source kind.
type SourceType string

const (
	SourceHosted  SourceType = "hosted"
	SourcePath    SourceType = "path"
	SourceGit     SourceType = "git"
	SourceSDK     SourceType = "sdk"
	SourceUnknown SourceType = "unknown"
)

// Package is one resolved RUNTIME dependency.
type Package struct {
	Name    string     `json:"name"`
	Version string     `json:"version"`
	Source  SourceType `json:"source"`
	// SourceID is a STABLE, machine-independent source identity:
	//   hosted  "hosted:<host>/<name>"          (+ ContentHash = the archive sha256 from pubspec.lock)
	//   git     "git:<url>@<resolved-commit>"   (+ GitCommit, + TreeHash of the checkout)
	//   path    "path:./<project-relative>" or "path:external"  (identity carried by TreeHash)
	//   sdk     "sdk:<flutter|dart>"
	// It NEVER contains a developer-local absolute path.
	SourceID string `json:"source_id"`
	// ContentHash is the hosted archive sha256 recorded in pubspec.lock (deterministic across machines).
	ContentHash string `json:"content_hash,omitempty"`
	// GitCommit is the resolved commit for a git source (not a mutable ref).
	GitCommit string `json:"git_commit,omitempty"`
	// TreeHash is a deterministic hash of the package's shipped source tree. Computed for path/git
	// sources, whose contents are not otherwise pinned by a checksum.
	TreeHash string `json:"tree_hash,omitempty"`
	// PubspecSHA is the sha256 of the package's own pubspec.yaml.
	PubspecSHA string `json:"pubspec_sha256,omitempty"`
	// Dependencies are this package's canonical (sorted, deduped) RUNTIME dependency edges.
	Dependencies []string   `json:"dependencies"`
	Capability   Capability `json:"capability"`

	rootDir string // resolved absolute package dir — NEVER serialized
}

// RootDir exposes the resolved package directory to in-process callers (never serialized).
func (p Package) RootDir() string { return p.rootDir }

// Graph is a project's fully-resolved RUNTIME dependency graph plus provenance hashes.
type Graph struct {
	Schema           string `json:"schema"`
	GeneratorVersion string `json:"generator_version"`
	PubspecLockSHA   string `json:"pubspec_lock_sha256"`
	PackageConfigSHA string `json:"package_config_sha256"`
	// RootPackage is the application package name; Roots are its direct runtime dependency edges.
	RootPackage string             `json:"root_package"`
	Roots       []string           `json:"roots"`
	Packages    map[string]Package `json:"packages"`
	GraphDigest string             `json:"graph_digest"`
}

// PackageNames returns every runtime package name, sorted.
func (g Graph) PackageNames() []string { return sortedKeys(g.Packages) }

// ---- Resolution ----

type lockFile struct {
	Packages map[string]lockPackage `yaml:"packages"`
}
type lockPackage struct {
	Dependency  string    `yaml:"dependency"`
	Source      string    `yaml:"source"`
	Version     string    `yaml:"version"`
	Description yaml.Node `yaml:"description"`
}

type packageConfig struct {
	Packages []struct {
		Name    string `json:"name"`
		RootURI string `json:"rootUri"`
	} `json:"packages"`
}

// depsSection models just the dependency maps of a pubspec.yaml. dev_dependencies is parsed ONLY so it
// can be deliberately excluded from the runtime walk.
type depsSection struct {
	Name            string               `yaml:"name"`
	Dependencies    map[string]yaml.Node `yaml:"dependencies"`
	DevDependencies map[string]yaml.Node `yaml:"dev_dependencies"`
}

// Resolve reads projectDir's pubspec.yaml, pubspec.lock and .dart_tool/package_config.json and returns
// the RUNTIME dependency graph: the transitive closure of the app's `dependencies:`, walked recursively
// through each package's own `dependencies:`. dev_dependencies are excluded at every level. Cycles are
// handled by a visited set. Any runtime edge that cannot be resolved to a locked, on-disk package fails
// closed with an actionable error.
func Resolve(projectDir string) (Graph, error) {
	return ResolveAt(projectDir,
		filepath.Join(projectDir, "pubspec.lock"),
		filepath.Join(projectDir, ".dart_tool", "package_config.json"))
}

// ResolveAt resolves the graph for projectDir using an EXPLICIT lock and package_config rather than the
// ones sitting in the project.
//
// The base graph of a release is resolved by the pinned Soroq toolchain's Flutter, whose SDK pulls in
// packages a developer's own Flutter may not (objective_c, code_assets, hooks, record_use among them).
// Reading the developer's lock for the candidate side therefore compares two graphs produced by two
// different SDKs, and an ordinary `flutter pub get` shows up as a large spurious diff -- packages
// "removed", plugins "introduced" -- which the eligibility gate then correctly refuses. The developer's
// only visible mistake was running a completely normal command.
//
// Resolving BOTH sides through the same pinned toolchain removes that whole class of false diff, and
// leaves the developer's lock as something Soroq only ever reads.
func ResolveAt(projectDir, lockPath, cfgPath string) (Graph, error) {
	rootPubspecPath := filepath.Join(projectDir, "pubspec.yaml")

	rootPubspecBytes, err := os.ReadFile(rootPubspecPath)
	if err != nil {
		return Graph{}, fmt.Errorf("read pubspec.yaml: %w", err)
	}
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		return Graph{}, fmt.Errorf("read pubspec.lock: %w (run `flutter pub get` first)", err)
	}
	cfgBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		return Graph{}, fmt.Errorf("read package_config.json: %w (run `flutter pub get` first)", err)
	}

	var lf lockFile
	if err := yaml.Unmarshal(lockBytes, &lf); err != nil {
		return Graph{}, fmt.Errorf("parse pubspec.lock: %w", err)
	}
	var cfg packageConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return Graph{}, fmt.Errorf("parse package_config.json: %w", err)
	}
	pkgRoots := map[string]string{}
	for _, p := range cfg.Packages {
		pkgRoots[p.Name] = resolveRoot(cfgPath, p.RootURI)
	}

	var rootDeps depsSection
	if err := yaml.Unmarshal(rootPubspecBytes, &rootDeps); err != nil {
		return Graph{}, fmt.Errorf("parse pubspec.yaml: %w", err)
	}
	if strings.TrimSpace(rootDeps.Name) == "" {
		return Graph{}, fmt.Errorf("pubspec.yaml has no package `name:`")
	}

	absProject, err := filepath.Abs(projectDir)
	if err != nil {
		return Graph{}, err
	}

	g := Graph{
		Schema:           GraphSchema,
		GeneratorVersion: GeneratorVersion,
		PubspecLockSHA:   sha256Hex(lockBytes),
		PackageConfigSHA: sha256Hex(cfgBytes),
		RootPackage:      rootDeps.Name,
		Roots:            sortedKeys(rootDeps.Dependencies),
		Packages:         map[string]Package{},
	}

	// Breadth-first walk over RUNTIME edges only. `visited` both terminates cycles (a -> b -> a) and
	// dedupes diamonds (a -> b, a -> c, b -> d, c -> d).
	visited := map[string]bool{rootDeps.Name: true}
	queue := append([]string(nil), g.Roots...)
	// `via` records who first required each package, so an unresolved edge names its requester.
	via := map[string]string{}
	for _, r := range g.Roots {
		via[r] = rootDeps.Name
	}

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if visited[name] {
			continue
		}
		visited[name] = true

		lp, ok := lf.Packages[name]
		if !ok {
			return Graph{}, fmt.Errorf("unresolved runtime dependency %q (required by %q) is absent from pubspec.lock — run `flutter pub get`; refusing to classify an incomplete runtime graph", name, via[name])
		}
		dir := pkgRoots[name]
		if dir == "" {
			return Graph{}, fmt.Errorf("unresolved runtime dependency %q (required by %q) has no entry in .dart_tool/package_config.json — run `flutter pub get`; refusing to classify an incomplete runtime graph", name, via[name])
		}
		pkgPubspecBytes, err := os.ReadFile(filepath.Join(dir, "pubspec.yaml"))
		if err != nil {
			return Graph{}, fmt.Errorf("runtime dependency %q (required by %q): cannot read its pubspec.yaml: %w — refusing to classify a package whose contents cannot be verified", name, via[name], err)
		}
		var pkgDeps depsSection
		if err := yaml.Unmarshal(pkgPubspecBytes, &pkgDeps); err != nil {
			return Graph{}, fmt.Errorf("runtime dependency %q: parse pubspec.yaml: %w", name, err)
		}

		edges := sortedKeys(pkgDeps.Dependencies) // RUNTIME edges only — dev_dependencies dropped here
		pkg := Package{
			Name:         name,
			Version:      lp.Version,
			Source:       normalizeSource(lp.Source),
			PubspecSHA:   sha256Hex(pkgPubspecBytes),
			Dependencies: edges,
			rootDir:      dir,
		}
		pkg.SourceID, pkg.ContentHash, pkg.GitCommit, err = describeSource(name, lp, dir, absProject)
		if err != nil {
			return Graph{}, fmt.Errorf("runtime dependency %q: %w", name, err)
		}
		// path/git sources carry no checksum in the lock — pin them with a deterministic tree hash.
		if pkg.Source == SourcePath || pkg.Source == SourceGit {
			th, err := hashSourceTree(dir)
			if err != nil {
				return Graph{}, fmt.Errorf("runtime dependency %q: hash source tree: %w", name, err)
			}
			pkg.TreeHash = th
		}
		pkg.Capability = classifyPackage(pkgPubspecBytes, dir, pkg.Source)
		g.Packages[name] = pkg

		for _, e := range edges {
			if !visited[e] {
				if _, seen := via[e]; !seen {
					via[e] = name
				}
				queue = append(queue, e)
			}
		}
	}

	g.GraphDigest = graphDigest(g)
	return g, nil
}

func normalizeSource(s string) SourceType {
	switch st := SourceType(strings.TrimSpace(s)); st {
	case SourceHosted, SourcePath, SourceGit, SourceSDK:
		return st
	default:
		return SourceUnknown
	}
}

func resolveRoot(cfgPath, rootURI string) string {
	if rootURI == "" {
		return ""
	}
	if strings.HasPrefix(rootURI, "file://") {
		return filepath.Clean(strings.TrimPrefix(rootURI, "file://"))
	}
	return filepath.Clean(filepath.Join(filepath.Dir(cfgPath), rootURI))
}

// describeSource derives the STABLE, machine-independent source identity. It deliberately never returns a
// developer-local absolute path: a path package inside the project is identified by its project-relative
// path, and one outside the project only as "path:external" (its real identity is the tree hash).
func describeSource(name string, lp lockPackage, dir, absProject string) (sourceID, contentHash, gitCommit string, err error) {
	var desc map[string]any
	if lp.Description.Kind != 0 {
		_ = lp.Description.Decode(&desc)
	}
	switch normalizeSource(lp.Source) {
	case SourceHosted:
		host, _ := desc["url"].(string)
		hosted, _ := desc["name"].(string)
		sha, _ := desc["sha256"].(string)
		if hosted == "" {
			hosted = name
		}
		host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
		host = strings.TrimSuffix(host, "/")
		if host == "" {
			host = "pub.dev"
		}
		if sha == "" {
			return "", "", "", fmt.Errorf("hosted package has no sha256 in pubspec.lock; refusing an unpinned hosted dependency")
		}
		return "hosted:" + host + "/" + hosted, sha, "", nil
	case SourceGit:
		url, _ := desc["url"].(string)
		ref, _ := desc["resolved-ref"].(string)
		if ref == "" {
			return "", "", "", fmt.Errorf("git package has no resolved-ref in pubspec.lock; refusing a dependency pinned only to a mutable ref")
		}
		return "git:" + url + "@" + ref, "", ref, nil
	case SourcePath:
		rel, rerr := filepath.Rel(absProject, dir)
		if rerr == nil && !strings.HasPrefix(rel, "..") {
			return "path:./" + filepath.ToSlash(rel), "", "", nil
		}
		// Outside the project: the absolute path is developer-local and must not be serialized.
		return "path:external", "", "", nil
	case SourceSDK:
		var sdk string
		if lp.Description.Kind != 0 {
			_ = lp.Description.Decode(&sdk)
		}
		if sdk == "" {
			sdk = "flutter"
		}
		return "sdk:" + sdk, "", "", nil
	default:
		return "", "", "", fmt.Errorf("unknown pub source %q; refusing to classify a dependency whose provenance cannot be pinned", lp.Source)
	}
}

// skipTreeDirs are directories excluded from a package's deterministic source-tree hash: VCS/build state
// and generated caches that are not part of the shipped package and differ per machine.
var skipTreeDirs = map[string]bool{
	".git": true, ".dart_tool": true, "build": true, ".idea": true, ".vscode": true,
	".github": true, "node_modules": true, ".pub-cache": true, ".pub": true,
}

type treeEntry struct {
	rel    string
	kind   string
	digest string
	size   int64
}

// hashSourceTree computes a deterministic hash over a package's source tree: every regular file's
// package-relative slash path, its size, and its content hash, in sorted path order. Symlinks are
// recorded by their target text rather than followed (a symlink escaping the tree must not silently
// contribute foreign content, and repointing it must still change the hash).
func hashSourceTree(dir string) (string, error) {
	var entries []treeEntry
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if skipTreeDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			target, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			entries = append(entries, treeEntry{rel: relSlash, kind: "symlink", digest: sha256Hex([]byte(filepath.ToSlash(target)))})
			return nil
		}
		if !d.Type().IsRegular() {
			// Sockets/devices/fifos are not shippable package content; record their presence explicitly
			// rather than silently ignoring them.
			entries = append(entries, treeEntry{rel: relSlash, kind: "irregular"})
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		fh, ferr := sha256File(p)
		if ferr != nil {
			return ferr
		}
		entries = append(entries, treeEntry{rel: relSlash, kind: "file", digest: fh, size: info.Size()})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	fmt.Fprint(h, "soroq.depgraph.tree.v1\n")
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s\n", e.rel, e.kind, e.size, e.digest)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- Digests ----

// graphDigest is the canonical graph identity. It binds, for every runtime package: name, version,
// source kind, stable source identity, hosted archive checksum, git commit, source-tree hash, pubspec
// sha, the FULL capability classification (not just the eligible bit), and the canonical runtime
// dependency edges — plus the root package and its direct edges. Two graphs with the same digest are
// interchangeable for OTA purposes; any change to any of those inputs changes the digest.
func graphDigest(g Graph) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n", GraphSchema, GeneratorVersion, g.RootPackage)
	// The resolution inputs are part of the identity: a graph is only meaningful for the exact
	// pubspec.lock + package_config it was resolved from, and editing either in a persisted graph must
	// invalidate its digest.
	fmt.Fprintf(h, "inputs\x00%s\x00%s\n", g.PubspecLockSHA, g.PackageConfigSHA)
	fmt.Fprintf(h, "roots:%s\n", strings.Join(g.Roots, ","))
	for _, n := range sortedKeys(g.Packages) {
		p := g.Packages[n]
		fmt.Fprintf(h, "pkg\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\n",
			p.Name, p.Version, p.Source, p.SourceID, p.ContentHash, p.GitCommit, p.TreeHash, p.PubspecSHA,
			strings.Join(p.Dependencies, ","), p.Capability.digestString())
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RecomputeDigest re-derives the canonical graph digest from the graph's own records. The strict decoder
// uses it to reject a graph whose recorded digest does not match its contents.
func (g Graph) RecomputeDigest() string { return graphDigest(g) }

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
