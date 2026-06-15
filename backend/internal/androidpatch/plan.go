package androidpatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	androidrelease "soroq/backend/internal/androidrelease"
	"soroq/backend/internal/domain"
)

type PlanOptions struct {
	BaseSnapshotPath         string
	CandidateSnapshotPath    string
	CandidateArtifactPath    string
	CandidateSnapshotOutPath string
	ReleaseID                string
	PatchKind                string
	ActivationMode           string
	Strict                   bool
}

type Plan struct {
	SchemaVersion         int                               `json:"schema_version"`
	GeneratedAt           time.Time                         `json:"generated_at"`
	Ready                 bool                              `json:"ready"`
	Strict                bool                              `json:"strict"`
	BaseSnapshotPath      string                            `json:"base_snapshot_path"`
	CandidateSnapshotPath *string                           `json:"candidate_snapshot_path,omitempty"`
	Target                Target                            `json:"target"`
	BaseArtifact          androidrelease.ArtifactDescriptor `json:"base_artifact"`
	CandidateArtifact     androidrelease.ArtifactDescriptor `json:"candidate_artifact"`
	Comparison            androidrelease.ComparisonReport   `json:"comparison"`
	Blockers              []Blocker                         `json:"blockers"`
	Notes                 []string                          `json:"notes,omitempty"`
}

type Target struct {
	Platform                 string   `json:"platform"`
	AppID                    string   `json:"app_id"`
	ReleaseID                *string  `json:"release_id,omitempty"`
	Channel                  string   `json:"channel"`
	RuntimeID                string   `json:"runtime_id"`
	RuntimeIDStrategy        string   `json:"runtime_id_strategy"`
	Version                  *string  `json:"version,omitempty"`
	BuildName                *string  `json:"build_name,omitempty"`
	BuildNumber              *string  `json:"build_number,omitempty"`
	ManifestTrustFingerprint *string  `json:"manifest_trust_fingerprint,omitempty"`
	PatchKind                string   `json:"patch_kind"`
	ActivationMode           string   `json:"activation_mode"`
	ABIs                     []string `json:"abis"`
}

type Blocker struct {
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

func PreparePlan(options PlanOptions) (*Plan, error) {
	if strings.TrimSpace(options.BaseSnapshotPath) == "" {
		return nil, errors.New("--base-snapshot is required")
	}

	hasCandidateSnapshot := strings.TrimSpace(options.CandidateSnapshotPath) != ""
	hasCandidateArtifact := strings.TrimSpace(options.CandidateArtifactPath) != ""
	switch {
	case hasCandidateSnapshot == hasCandidateArtifact:
		return nil, errors.New("exactly one of --candidate-snapshot or --candidate-artifact is required")
	case hasCandidateSnapshot && strings.TrimSpace(options.CandidateSnapshotOutPath) != "":
		return nil, errors.New("--candidate-snapshot-out can only be used with --candidate-artifact")
	}

	baseSnapshotPath := filepath.Clean(options.BaseSnapshotPath)
	baseSnapshot, err := androidrelease.LoadSnapshot(baseSnapshotPath)
	if err != nil {
		return nil, fmt.Errorf("load base snapshot: %w", err)
	}

	var (
		candidateSnapshot     *androidrelease.Snapshot
		candidateSnapshotPath *string
	)
	if hasCandidateSnapshot {
		cleanPath := filepath.Clean(options.CandidateSnapshotPath)
		candidateSnapshot, err = androidrelease.LoadSnapshot(cleanPath)
		if err != nil {
			return nil, fmt.Errorf("load candidate snapshot: %w", err)
		}
		candidateSnapshotPath = &cleanPath
	} else {
		candidateSnapshot, err = androidrelease.CaptureSnapshot(options.CandidateArtifactPath)
		if err != nil {
			return nil, fmt.Errorf("capture candidate snapshot: %w", err)
		}
		if strings.TrimSpace(options.CandidateSnapshotOutPath) != "" {
			cleanPath := filepath.Clean(options.CandidateSnapshotOutPath)
			if err := writeJSONOutput(candidateSnapshot, cleanPath); err != nil {
				return nil, fmt.Errorf("write candidate snapshot: %w", err)
			}
			candidateSnapshotPath = &cleanPath
		}
	}

	comparison := androidrelease.CompareSnapshots(baseSnapshot, candidateSnapshot)
	blockers := make([]Blocker, 0)
	for _, check := range comparison.Checks {
		if !check.Passed {
			blockers = append(blockers, Blocker{
				ID:     check.ID,
				Detail: check.Detail,
			})
		}
	}

	target := Target{
		Platform:                 "android",
		AppID:                    baseSnapshot.Metadata.Soroq.AppID,
		Channel:                  baseSnapshot.Metadata.Soroq.Channel,
		RuntimeID:                baseSnapshot.Metadata.Soroq.RuntimeID,
		RuntimeIDStrategy:        baseSnapshot.Metadata.RuntimeIDStrategy(),
		Version:                  baseSnapshot.Metadata.App.Version,
		BuildName:                baseSnapshot.Metadata.App.BuildName,
		BuildNumber:              baseSnapshot.Metadata.App.BuildNumber,
		ManifestTrustFingerprint: baseSnapshot.Metadata.Soroq.ManifestTrustFingerprint,
		PatchKind:                string(domain.NormalizePatchKind(domain.PatchKind(normalizedDefaultString(options.PatchKind, string(domain.PatchKindExperimentalNativeAOT))))),
		ActivationMode:           normalizedDefaultString(options.ActivationMode, string(domain.ActivationNextColdStart)),
		ABIs:                     androidrelease.DeriveABIs(baseSnapshot),
	}
	if trimmedReleaseID := strings.TrimSpace(options.ReleaseID); trimmedReleaseID != "" {
		target.ReleaseID = &trimmedReleaseID
	}

	notes := make([]string, 0, 3)
	if candidateSnapshotPath != nil {
		notes = append(notes, "candidate snapshot is persisted and can be reused by later patch-generation steps")
	} else {
		notes = append(notes, "candidate snapshot was captured in-memory from the provided Android artifact")
	}
	if comparison.Compatible {
		notes = append(notes, "runtime identity, manifest-trust boundary, and native library digests still match the base release")
	} else {
		notes = append(notes, fmt.Sprintf("patch generation is currently blocked by %d compatibility check(s)", len(blockers)))
	}
	if baseSnapshot.Artifact.Type != candidateSnapshot.Artifact.Type {
		notes = append(notes, fmt.Sprintf("base artifact type %s and candidate artifact type %s differ, but comparison is evaluated on normalized runtime contents", baseSnapshot.Artifact.Type, candidateSnapshot.Artifact.Type))
	}

	return &Plan{
		SchemaVersion:         1,
		GeneratedAt:           time.Now().UTC(),
		Ready:                 comparison.Compatible,
		Strict:                options.Strict,
		BaseSnapshotPath:      baseSnapshotPath,
		CandidateSnapshotPath: candidateSnapshotPath,
		Target:                target,
		BaseArtifact:          baseSnapshot.Artifact,
		CandidateArtifact:     candidateSnapshot.Artifact,
		Comparison:            comparison,
		Blockers:              blockers,
		Notes:                 notes,
	}, nil
}

func LoadPlan(path string) (*Plan, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var plan Plan
	if err := json.Unmarshal(bytes, &plan); err != nil {
		return nil, err
	}
	if plan.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported patch plan schema version %d", plan.SchemaVersion)
	}
	return &plan, nil
}
