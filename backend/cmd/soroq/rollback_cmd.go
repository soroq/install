package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"soroq/backend/internal/domain"
)

type rollbackSummary struct {
	Patch        domain.Patch                `json:"patch"`
	Verification *rollbackVerificationResult `json:"verification,omitempty"`
}

type rollbackVerificationResult struct {
	ClientID                string                    `json:"client_id"`
	CurrentPatchNumber      int                       `json:"current_patch_number"`
	PatchCheck              domain.PatchCheckResponse `json:"patch_check"`
	RolledBackNumberPresent bool                      `json:"rolled_back_number_present"`
	Verified                bool                      `json:"verified"`
}

func runRollback(args []string) error {
	// engine-lane rollback (version-0 signed manifest) is a distinct target; delegate to soroqctl.
	if len(args) > 0 && args[0] == "ios-engine" {
		return runEngineLaneDelegate("rollback", args[1:])
	}
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	apiBase := fs.String("api", defaultAPIBase(), "control plane base URL")
	patchID := fs.String("patch-id", "", "patch id to roll back")
	verify := fs.Bool("verify", false, "verify runtime patch-check no longer offers the rolled-back patch")
	verifyClientID := fs.String("verify-client-id", "soroq-rollback-verify", "client id to use for rollback patch-check verification")
	verifyCurrentPatchNumber := fs.Int("verify-current-patch-number", 0, "current patch number to report during rollback verification")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stdout, `usage: soroq rollback --patch-id patch-123 [--api https://api.soroq.dev] [--verify] [--verify-client-id device-123] [--json]`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(*patchID) == "" {
		return errors.New("--patch-id is required")
	}
	if *verifyCurrentPatchNumber < 0 {
		return errors.New("--verify-current-patch-number must be zero or greater")
	}

	resolvedAPIBase := strings.TrimRight(*apiBase, "/")
	patch, err := postNoBodyDecode[domain.Patch](http.MethodPost, resolvedAPIBase+"/v1/patches/"+url.PathEscape(strings.TrimSpace(*patchID))+"/rollback")
	if err != nil {
		return err
	}
	var verification *rollbackVerificationResult
	if *verify {
		result, err := verifyRollbackPatchCheck(resolvedAPIBase, patch, strings.TrimSpace(*verifyClientID), *verifyCurrentPatchNumber)
		if err != nil {
			return err
		}
		verification = &result
	}

	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if verification != nil {
			return encoder.Encode(rollbackSummary{Patch: patch, Verification: verification})
		}
		return encoder.Encode(patch)
	}

	fmt.Fprintf(os.Stdout, "Rolled back patch %s\n", patch.ID)
	fmt.Fprintf(os.Stdout, "patch_number: %d\n", patch.Number)
	fmt.Fprintf(os.Stdout, "app_id: %s\n", patch.AppID)
	fmt.Fprintf(os.Stdout, "release_id: %s\n", patch.ReleaseID)
	fmt.Fprintf(os.Stdout, "runtime_id: %s\n", patch.RuntimeID)
	fmt.Fprintf(os.Stdout, "channel: %s\n", patch.Channel)
	fmt.Fprintf(os.Stdout, "rolled_back: %s\n", yesNo(patch.RolledBack))
	if verification != nil {
		fmt.Fprintf(os.Stdout, "rollback_verified: %s\n", yesNo(verification.Verified))
		fmt.Fprintf(os.Stdout, "patch_check_patch_available: %s\n", yesNo(verification.PatchCheck.PatchAvailable && verification.PatchCheck.Patch != nil))
		fmt.Fprintf(os.Stdout, "rolled_back_number_present: %s\n", yesNo(verification.RolledBackNumberPresent))
	}
	return nil
}

func verifyRollbackPatchCheck(apiBase string, patch domain.Patch, clientID string, currentPatchNumber int) (rollbackVerificationResult, error) {
	if strings.TrimSpace(clientID) == "" {
		return rollbackVerificationResult{}, errors.New("--verify-client-id is required when --verify is used")
	}
	response, err := postRuntimeJSONDecode[domain.PatchCheckResponse](strings.TrimRight(apiBase, "/")+"/v1/patch-check", domain.PatchCheckRequest{
		AppID:              patch.AppID,
		ReleaseID:          patch.ReleaseID,
		RuntimeID:          patch.RuntimeID,
		Channel:            patch.Channel,
		Track:              patch.Track,
		CurrentPatchNumber: currentPatchNumber,
		ClientID:           clientID,
		Kind:               patch.Kind,
	})
	if err != nil {
		return rollbackVerificationResult{}, err
	}
	result := rollbackVerificationResult{
		ClientID:                clientID,
		CurrentPatchNumber:      currentPatchNumber,
		PatchCheck:              response,
		RolledBackNumberPresent: containsInt(response.RolledBackPatchNumbers, patch.Number),
	}
	if response.Patch != nil && response.Patch.ID == patch.ID {
		return result, fmt.Errorf("rollback verification failed: patch-check still offers rolled-back patch %s", patch.ID)
	}
	if !result.RolledBackNumberPresent {
		return result, fmt.Errorf("rollback verification failed: patch-check did not report rolled-back patch number %d", patch.Number)
	}
	result.Verified = true
	return result, nil
}

func containsInt(values []int, value int) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
