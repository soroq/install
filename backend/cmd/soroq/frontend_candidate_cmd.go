package main

// `soroq frontend use-candidate` — activate a LOCAL, UNSIGNED candidate frontend.
//
// Why this exists as a command rather than a documented file edit: activating a candidate previously
// meant hand-editing ~/.soroq/frontends/active.json. That is not a workflow anyone should be asked to
// perform, and it is dangerous in a specific way -- a half-written or wrong active.json silently
// redirects every subsequent build to a frontend that may not exist, and there is no record that the
// active frontend is unsigned.
//
// This command therefore:
//
//   - VERIFIES the candidate before touching anything (manifest schema, required contents, the
//     analyzer dill, and that the tool snapshot really contains the analysis target);
//   - records the candidate/unsigned status explicitly, so `frontend list`/`doctor` and any later
//     reader can tell this is not a signed production frontend;
//   - activates ATOMICALLY (temp file + rename), so an interrupted run leaves the previous frontend
//     active rather than a truncated pointer;
//   - keeps a restore pointer to the PREVIOUS frontend and offers `--restore` to go back;
//   - REFUSES malformed or incomplete candidates instead of activating something that will fail
//     halfway through a build.
//
// It deliberately cannot publish: a candidate is local-only. Publishing a frontend is a signing
// operation against the production toolchain key and is out of scope here.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// candidateManifest is the subset of soroq.frontend.v1 a candidate must carry.
type candidateManifest struct {
	Schema          string `json:"schema"`
	Version         string `json:"soroq_frontend_version"`
	FlutterRevision string `json:"flutter_revision"`
	PatchsetSHA256  string `json:"patchset_sha256"`
	AnalyzerSHA256  string `json:"analyzer_snapshot_sha256"`
	FrontendSubdir  string `json:"frontend_subdir"`
	Candidate       bool   `json:"candidate"`
	Signed          bool   `json:"signed"`
	// Which toolchains this frontend was built to work with. The canonical zero-touch command uses it
	// to derive --toolchain; without it, a machine with several installed toolchains cannot be paired
	// safely and the command refuses rather than guessing.
	CompatibleToolchainIDs []string `json:"compatible_toolchain_ids,omitempty"`
}

// verifyCandidateFrontend refuses anything that would fail later in a build. Every check names what is
// missing, because "candidate rejected" with no reason is how people go back to editing active.json.
func verifyCandidateFrontend(dir string) (candidateManifest, error) {
	var m candidateManifest

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, fmt.Errorf("candidate has no manifest.json (%s): %w", dir, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("candidate manifest is malformed: %w", err)
	}
	if m.Schema != frontendManifestSchema {
		return m, fmt.Errorf("candidate manifest schema %q is not %q", m.Schema, frontendManifestSchema)
	}
	if !m.Candidate {
		return m, fmt.Errorf("manifest does not declare candidate:true; use `soroq frontend install` for a signed frontend")
	}
	if m.Signed {
		return m, fmt.Errorf("manifest claims signed:true but is being activated as a candidate; refusing the contradiction")
	}
	for name, v := range map[string]string{
		"soroq_frontend_version":   m.Version,
		"flutter_revision":         m.FlutterRevision,
		"patchset_sha256":          m.PatchsetSHA256,
		"analyzer_snapshot_sha256": m.AnalyzerSHA256,
		"frontend_subdir":          m.FrontendSubdir,
	} {
		if strings.TrimSpace(v) == "" {
			return m, fmt.Errorf("candidate manifest field %q is empty", name)
		}
	}

	root := filepath.Join(dir, m.FrontendSubdir)
	flutterBin := filepath.Join(root, "bin", "flutter")
	if _, err := os.Stat(flutterBin); err != nil {
		return m, fmt.Errorf("candidate has no %s: %w", flutterBin, err)
	}

	// The analyzer the frontend will actually invoke, bound by hash to the manifest.
	dill := filepath.Join(root, "bin", "cache", "soroq", "soroq_kernel_analyze.dill")
	if _, err := os.Stat(dill); err != nil {
		return m, fmt.Errorf("candidate does not bundle soroq_kernel_analyze.dill: %w", err)
	}
	sum, err := sha256OfPath(dill)
	if err != nil {
		return m, fmt.Errorf("hash bundled analyzer: %w", err)
	}
	if !strings.EqualFold(sum, m.AnalyzerSHA256) {
		return m, fmt.Errorf("bundled analyzer sha %s does not match the manifest's %s",
			short(sum), short(m.AnalyzerSHA256))
	}

	// THE DECISIVE CHECK. A frontend whose SOURCE has the target but whose executed SNAPSHOT does not
	// is exactly the failure this whole candidate exists to fix: the build succeeds, no analysis is
	// written, and baseline persistence then fails with a confusing content-address error.
	snap := filepath.Join(root, "bin", "cache", "flutter_tools.snapshot")
	if _, err := os.Stat(snap); err != nil {
		return m, fmt.Errorf("candidate has no built flutter_tools.snapshot: %w", err)
	}
	if !fileContainsToken(snap, "SoroqFreehandAnalysis") {
		return m, fmt.Errorf("candidate's flutter_tools.snapshot does not contain SoroqFreehandAnalysis; " +
			"it would build successfully and silently skip the freehand analysis")
	}
	return m, nil
}

// fileContainsToken reports whether a (possibly binary) file contains the literal token.
func fileContainsToken(path, token string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), token)
}

// activeFrontendRestore is written alongside active.json so the previous frontend can be restored
// without the operator having to remember what it was.
type activeFrontendRestore struct {
	Previous  activeFrontend `json:"previous"`
	ReplacedB string         `json:"replaced_by"`
}

func restorePointerPath() (string, error) {
	p, err := activeFrontendPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "active.previous.json"), nil
}

func runFrontendUseCandidate(args []string) error {
	restore := hasFlag(args, "restore")

	activePath, err := activeFrontendPath()
	if err != nil {
		return err
	}
	prevPath, err := restorePointerPath()
	if err != nil {
		return err
	}

	if restore {
		raw, err := os.ReadFile(prevPath)
		if err != nil {
			return fmt.Errorf("no previous frontend recorded to restore (%s): %w", prevPath, err)
		}
		var r activeFrontendRestore
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("restore pointer is malformed: %w", err)
		}
		if strings.TrimSpace(r.Previous.Version) == "" {
			return fmt.Errorf("restore pointer names no previous frontend version")
		}
		if err := recordActiveFrontend(r.Previous); err != nil {
			return fmt.Errorf("restore previous frontend: %w", err)
		}
		_ = os.Remove(prevPath)
		fmt.Fprintf(os.Stdout, "restored signed frontend: %s\n", r.Previous.Version)
		return nil
	}

	var dir string
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			dir = a
			break
		}
		if a == "--dir" && i+1 < len(args) {
			dir = args[i+1]
			break
		}
	}
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("usage: soroq frontend use-candidate <candidate-dir> | --restore")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	m, err := verifyCandidateFrontend(abs)
	if err != nil {
		return fmt.Errorf("candidate refused: %w", err)
	}

	// Remember what we are replacing BEFORE replacing it, so --restore always has a target.
	//
	// Only a SIGNED frontend is ever recorded as the restore target. Activating a candidate while a
	// candidate is already active would otherwise overwrite the pointer with that candidate, and
	// --restore would then "restore" to a candidate — losing the way back to the signed frontend
	// entirely. Re-activating is common (rebuild the candidate, activate again), so this is the normal
	// path, not an edge case.
	if cur, err := os.ReadFile(activePath); err == nil {
		var prev activeFrontend
		if json.Unmarshal(cur, &prev) == nil && strings.TrimSpace(prev.Version) != "" &&
			!isCandidateFrontendRecord(prev) {
			b, _ := json.MarshalIndent(activeFrontendRestore{Previous: prev, ReplacedB: m.Version}, "", "  ")
			if err := writeRestorePointer(prevPath, b); err != nil {
				return fmt.Errorf("record restore pointer: %w", err)
			}
		}
	}

	next := activeFrontend{
		Version:    m.Version,
		FlutterBin: filepath.Join(abs, m.FrontendSubdir, "bin", "flutter"),
		// Deliberately NOT an archive digest: there is no signed archive. Naming it plainly keeps a
		// candidate from ever being mistaken for a verified signed install.
		ArchiveSHA256: "candidate-unsigned:" + m.PatchsetSHA256[:16],
	}
	if err := recordActiveFrontend(next); err != nil {
		return fmt.Errorf("activate candidate: %w", err)
	}

	fmt.Fprintf(os.Stdout, "activated CANDIDATE frontend (UNSIGNED, local only)\n")
	fmt.Fprintf(os.Stdout, "  version      : %s\n", m.Version)
	fmt.Fprintf(os.Stdout, "  flutter      : %s\n", next.FlutterBin)
	fmt.Fprintf(os.Stdout, "  patchset sha : %s\n", m.PatchsetSHA256)
	fmt.Fprintf(os.Stdout, "  analyzer sha : %s\n", m.AnalyzerSHA256)
	fmt.Fprintf(os.Stdout, "  restore with : soroq frontend use-candidate --restore\n")
	return nil
}

// writeRestorePointer persists the restore pointer through the existing atomic temp-file+rename helper,
// so an interrupted write cannot leave a truncated pointer that redirects every later build at nothing.
func writeRestorePointer(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// candidateFlutterVersionOK is a cheap liveness probe used by tests and doctor: the candidate's own
// flutter must at least run.
func candidateFlutterVersionOK(flutterBin string) bool {
	cmd := exec.Command(flutterBin, "--version")
	return cmd.Run() == nil
}

// isCandidateFrontendRecord reports whether an active-frontend record describes an unsigned candidate.
// The marker is written by this command and is deliberately not a valid archive digest, so a candidate
// can never be mistaken for a signature-verified install.
func isCandidateFrontendRecord(a activeFrontend) bool {
	return strings.HasPrefix(a.ArchiveSHA256, "candidate-unsigned:")
}
