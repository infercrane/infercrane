package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/spec"
)

// optimize compare creates a run of exactly two setups from two
// DeploymentSpecs: the current setup, which is the run's control, and the
// proposed change. Both run on fresh servers of one kind, so a pull request
// can prove its change without touching the setup that serves production.
// It creates the run only; optimize approve starts it and optimize gate reads
// its verdict.

const optimizeCompareUsage = "usage: infercrane optimize compare CURRENT.yaml PROPOSED.yaml [--objective OBJECTIVE] [--profile PROFILE] [--workload-fingerprint sha256:DIGEST] [--quality-check builtin|customer_evidence] [--eval-set sha256:DIGEST] [--idempotency-key KEY] [--output human|json]"

func optimizeCompareCommand(ctx context.Context, args []string) error {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return errors.New(optimizeCompareUsage)
	}
	currentPath, proposedPath := args[0], args[1]
	fs := newFlagSet("optimize compare")
	objective := fs.String("objective", "interactive", "interactive, latency, throughput, or cost-efficiency")
	profile := fs.String("profile", "", "benchmark workload profile; defaults from objective")
	fingerprint := fs.String("workload-fingerprint", "", "content-free fingerprint of your captured traffic, so both setups replay it")
	qualityCheck := fs.String("quality-check", "builtin", "who checks answers: builtin (compared with the current setup's) or customer_evidence (your signed eval)")
	evalSet := fs.String("eval-set", "", "digest of an uploaded eval set that replaces the built-in prompts")
	idempotencyKey := fs.String("idempotency-key", "", "stable safe-retry key; defaults to one derived from both setups")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
		return errors.New(optimizeCompareUsage)
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	current, err := spec.Load(currentPath)
	if err != nil {
		return fmt.Errorf("current setup %s: %w", currentPath, err)
	}
	proposed, err := spec.Load(proposedPath)
	if err != nil {
		return fmt.Errorf("proposed setup %s: %w", proposedPath, err)
	}
	proposal, err := optimizer.CompareSetups(optimizer.Comparison{Current: current, Proposed: proposed, Objective: *objective, WorkloadProfile: *profile, WorkloadFingerprint: *fingerprint, QualityCheck: *qualityCheck, QualityEvalSet: *evalSet})
	if err != nil {
		return err
	}
	if *idempotencyKey == "" {
		*idempotencyKey = "compare-" + proposal.Candidates[1].ID[:24]
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	var response struct {
		Campaign optimizationCampaignView `json:"campaign"`
		Created  bool                     `json:"created"`
	}
	if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/optimization/campaigns", *idempotencyKey, map[string]any{"proposal": proposal, "intent": "new_endpoint"}, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Optimization run %s · %s\n", response.Campaign.ID, response.Campaign.State)
	fmt.Printf("Model      %s\nObjective  %s\nCurrent    %s\nProposed   %s\nMutation   none: both setups run on fresh servers\n\n", response.Campaign.ModelIdentity, response.Campaign.Objective, currentPath, proposedPath)
	fmt.Printf("Next: infercrane optimize approve %s --max-cost-usd AMOUNT --expires-in 3h\n", response.Campaign.ID)
	fmt.Printf("Then: infercrane optimize gate %s --wait 3h\n", response.Campaign.ID)
	printResourceLink(cfg, "run", response.Campaign.ID)
	return nil
}
