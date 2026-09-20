package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/kernelplanner"
	"github.com/infercrane/infercrane/internal/optimizationevidence"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/workloadprofile"
	"gopkg.in/yaml.v3"
)

func optimizeCommand(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "kernel-plan" {
		return optimizeKernelPlanCommand(args[1:])
	}
	if len(args) > 0 && args[0] == "workload-profile" {
		return optimizeWorkloadProfileCommand(args[1:])
	}
	if len(args) > 0 && args[0] == "evidence" {
		return optimizeEvidenceCommand(args[1:])
	}
	if len(args) > 0 && args[0] == "doctor" {
		return optimizeDoctorCommand(ctx, args[1:])
	}
	if len(args) > 0 && (args[0] == "list" || args[0] == "inspect" || args[0] == "results" || args[0] == "approve" || args[0] == "activate" || args[0] == "cancel") {
		return optimizeCampaignCommand(ctx, args)
	}
	if len(args) < 2 || (args[0] != "propose" && args[0] != "create") {
		return errors.New("usage: infercrane optimize propose|create MODEL --provider CLOUD_OR_ADAPTER --gpu GPU [flags] | infercrane optimize workload-profile --file PROFILE.json | infercrane optimize kernel-plan --file PROFILE.json | infercrane optimize evidence evaluate|inspect-fastpath --file FILE | infercrane optimize list|inspect|results|approve|activate|cancel | infercrane optimize doctor")
	}
	action := args[0]
	model := args[1]
	fs := flag.NewFlagSet("optimize "+action, flag.ContinueOnError)
	modelRevision := fs.String("model-revision", "", "immutable 40 to 64 character model commit (required for uncataloged models)")
	provider := fs.String("provider", "", "provider cloud or exact adapter")
	region := fs.String("region", "", "exact provider region")
	gpu := fs.String("gpu", "", "exact accelerator identity")
	gpuCount := fs.Int("gpu-count", 1, "GPUs per baseline replica")
	runtimes := fs.String("runtimes", "", "optional comma-separated runtime allowlist")
	objective := fs.String("objective", "interactive", "interactive, latency, throughput, or cost-efficiency")
	profile := fs.String("profile", "", "benchmark workload profile; defaults from objective")
	maxTTFT := fs.String("max-ttft-p95-ms", "", "required measured p95 TTFT")
	maxTPOT := fs.String("max-tpot-p95-ms", "", "required measured p95 TPOT")
	maxErrorRate := fs.String("max-error-rate", "", "maximum measured failed-request ratio")
	minGoodput := fs.String("min-goodput", "", "minimum measured requests per second satisfying the SLO")
	minOutputTPS := fs.String("min-output-tokens-second", "", "required measured output throughput")
	maxHourlyCost := fs.String("max-hourly-cost", "", "required sourced hourly cost")
	includeSimulated := fs.Bool("include-simulated", false, "include locally simulated compatibility candidates")
	maxCandidates := fs.Int("max-candidates", 10, "maximum candidates")
	targetConcurrency := fs.String("target-concurrency", "", "observed or expected concurrent requests; defaults to the workload profile")
	workloadFingerprint := fs.String("workload-fingerprint", "", "content-free observed workload fingerprint")
	sourceName := fs.String("source", "auto", "candidate source: auto, catalog, or aiconfigurator")
	python := fs.String("aiconfigurator-python", "python3", "Python executable containing aiconfigurator 0.11.0")
	allowMetadataNetwork := fs.Bool("allow-model-metadata-network", false, "allow the isolated estimator to fetch public model metadata")
	writeDir := fs.String("write-dir", "", "write candidate DeploymentSpecs into a new or empty directory")
	idempotencyKey := fs.String("idempotency-key", "", "stable safe-retry key for campaign creation")
	targetDeployment := fs.String("target-deployment", "", "existing deployment to evolve; omit when qualifying a new endpoint")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane optimize " + action + " MODEL --provider CLOUD_OR_ADAPTER --gpu GPU [flags]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	request := optimizer.Request{ModelIdentity: model, ModelRevision: *modelRevision, Provider: *provider, Region: *region, GPU: *gpu, GPUCount: *gpuCount, Runtimes: splitList(*runtimes), Objective: *objective, WorkloadProfile: *profile, IncludeSimulated: *includeSimulated, WorkloadFingerprint: *workloadFingerprint, MaxCandidates: *maxCandidates}
	var err error
	if request.MaxTTFTP95MS, err = optionalMilliseconds(*maxTTFT); err != nil {
		return fmt.Errorf("max TTFT p95: %w", err)
	}
	if request.MaxTPOTP95MS, err = optionalMilliseconds(*maxTPOT); err != nil {
		return fmt.Errorf("max TPOT p95: %w", err)
	}
	if request.MaxErrorRate, err = optionalNumber(*maxErrorRate); err != nil || request.MaxErrorRate != nil && *request.MaxErrorRate > 1 {
		return errors.New("maximum error rate must be a number between zero and one")
	}
	if request.MinGoodput, err = optionalNumber(*minGoodput); err != nil {
		return fmt.Errorf("minimum goodput: %w", err)
	}
	if request.MinOutputTokensSecond, err = optionalNumber(*minOutputTPS); err != nil {
		return fmt.Errorf("minimum output tokens per second: %w", err)
	}
	if request.MaxHourlyCost, err = optionalNumber(*maxHourlyCost); err != nil {
		return fmt.Errorf("maximum hourly cost: %w", err)
	}
	if request.TargetConcurrency, err = optionalPositiveNumber(*targetConcurrency); err != nil {
		return fmt.Errorf("target concurrency: %w", err)
	}
	registry, err := integration.V1Catalog()
	if err != nil {
		return fmt.Errorf("load integration inventory: %w", err)
	}
	catalog := optimizer.NewCatalogSource(curatedrecipe.All(), registry.Snapshot())
	proposal, err := optimizationProposal(ctx, request, catalog, *sourceName, *python, *allowMetadataNetwork)
	if err != nil {
		return err
	}
	if *writeDir != "" {
		if err = writeCandidateSpecs(*writeDir, proposal.Candidates); err != nil {
			return err
		}
	}
	if action == "create" {
		cfg, loadErr := config.LoadClient()
		if loadErr != nil {
			return loadErr
		}
		if *idempotencyKey == "" {
			*idempotencyKey = "optimize-" + proposal.InputDigest[:24]
		}
		var response struct {
			Campaign optimizationCampaignView `json:"campaign"`
			Created  bool                     `json:"created"`
		}
		intent := "new_endpoint"
		if strings.TrimSpace(*targetDeployment) != "" {
			intent = "evolve_endpoint"
		}
		if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/optimization/campaigns", *idempotencyKey, map[string]any{"proposal": proposal, "intent": intent, "target_deployment": strings.TrimSpace(*targetDeployment)}, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("Optimization campaign %s · %s\n", response.Campaign.ID, response.Campaign.State)
		fmt.Printf("Model       %s\nObjective   %s\nIntent      %s\nCandidates  %d\nMutation    none\n", response.Campaign.ModelIdentity, response.Campaign.Objective, response.Campaign.Intent, len(response.Campaign.Candidates))
		if response.Campaign.TargetDeployment != "" {
			fmt.Printf("Target      %s\n", response.Campaign.TargetDeployment)
		}
		fmt.Println()
		fmt.Printf("Next: infercrane optimize approve %s --max-cost-usd AMOUNT --expires-in 1h\n", response.Campaign.ID)
		return nil
	}
	if *output == "json" {
		encoded, marshalErr := json.MarshalIndent(proposal, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		fmt.Println(string(encoded))
		return nil
	}
	fmt.Printf("Optimization candidates · %s · %s on %s\n", proposal.Input.ModelIdentity, proposal.Input.Objective, proposal.Input.Provider)
	fmt.Printf("Input evidence %s · mutation %s\n\n", shortID(proposal.InputDigest), proposal.Mutation)
	for _, warning := range proposal.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
	}
	if len(proposal.Candidates) == 0 {
		fmt.Printf("No safe candidate was produced. Missing: %s\n", strings.Join(proposal.Missing, ", "))
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "RANK\tRUNTIME\tPROVIDER\tGPU\tCONFIG\tWORKLOAD\tEVIDENCE\tSTATUS")
	for _, candidate := range proposal.Candidates {
		fmt.Fprintf(w, "%d\t%s %s\t%s\t%s\t%s\t%s\t%s\t%s\n", candidate.Rank, candidate.Deployment.Runtime.Engine, candidate.Deployment.Runtime.Version, candidate.Deployment.Provider.Adapter, candidate.Deployment.Resources.GPU, candidate.ConfigurationProfile, candidate.BenchmarkProfile, candidate.EvidenceState, candidate.Status)
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if proposal.StartingPoint != nil {
		fmt.Printf("\nStarting point: candidate %s · %s\n", shortID(proposal.StartingPoint.CandidateID), proposal.StartingPoint.ClaimBoundary)
	}
	fmt.Println("These are configuration candidates, not performance recommendations.")
	fmt.Printf("Next: deploy the starting point, observe representative traffic, run `infercrane benchmark NAME --profile %s`, profile material bottlenecks, compare with `infercrane lab`, then use Release Guard.\n", proposal.Input.WorkloadProfile)
	if *writeDir != "" {
		fmt.Printf("DeploymentSpecs: %s\n", *writeDir)
	}
	return nil
}

func optimizeWorkloadProfileCommand(args []string) error {
	fs := flag.NewFlagSet("optimize workload-profile", flag.ContinueOnError)
	filePath := fs.String("file", "", "content-free public workload profile JSON")
	profileName := fs.String("profile", "", "optional exact derived benchmark profile")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*filePath) == "" {
		return errors.New("usage: infercrane optimize workload-profile --file PROFILE.json [--profile NAME] [--output human|json]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	body, err := os.ReadFile(*filePath)
	if err != nil {
		return fmt.Errorf("read workload profile: %w", err)
	}
	document, err := workloadprofile.Decode(body)
	if err != nil {
		return err
	}
	digest, err := workloadprofile.Digest(document)
	if err != nil {
		return err
	}
	profiles := document.Profiles
	if strings.TrimSpace(*profileName) != "" {
		profile, lookupErr := workloadprofile.Lookup(document, *profileName)
		if lookupErr != nil {
			return lookupErr
		}
		profiles = []workloadprofile.BenchmarkShape{profile}
	}
	type result struct {
		Digest              string                           `json:"digest"`
		Source              workloadprofile.Source           `json:"source"`
		Sampling            workloadprofile.Sampling         `json:"sampling"`
		Distributions       workloadprofile.Distributions    `json:"distributions"`
		Profiles            []workloadprofile.BenchmarkShape `json:"profiles"`
		EvidenceClass       string                           `json:"evidence_class"`
		MethodologyBoundary string                           `json:"methodology_boundary"`
	}
	view := result{Digest: digest, Source: document.Source, Sampling: document.Sampling, Distributions: document.Distributions, Profiles: profiles, EvidenceClass: document.EvidenceClass, MethodologyBoundary: document.MethodologyBoundary}
	if *output == "json" {
		return printJSON(view)
	}
	fmt.Printf("Workload profile · %s\n", document.Source.Name)
	fmt.Printf("Evidence    %s\n", document.EvidenceClass)
	fmt.Printf("Sample      %d content-free rows · %d/%d row groups · remote only\n", document.Sampling.RowsInspected, len(document.Sampling.RowGroups), document.Sampling.TotalRowGroups)
	fmt.Printf("Digest      %s\n\n", digest)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE\tOBJECTIVE\tINPUT\tOUTPUT\tCONCURRENCY\tREQUESTS\tCONTEXT CLIPPED")
	for _, profile := range profiles {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%t\n", profile.Name, profile.Objective, profile.InputTokens, profile.OutputTokens, profile.Concurrency, profile.Requests, profile.Clipped)
	}
	if err = w.Flush(); err != nil {
		return err
	}
	fmt.Println("\nReproduce against a deployment:")
	for _, profile := range profiles {
		fmt.Printf("  infercrane benchmark DEPLOYMENT --requests %d --concurrency %d --input-tokens %d --output-tokens %d --runs 3 --warmup-requests %d\n", profile.Requests, profile.Concurrency, profile.InputTokens, profile.OutputTokens, profile.Concurrency)
	}
	fmt.Printf("\nBoundary: %s\n", document.MethodologyBoundary)
	return nil
}

func optimizeKernelPlanCommand(args []string) error {
	fs := flag.NewFlagSet("optimize kernel-plan", flag.ContinueOnError)
	filePath := fs.String("file", "", "profile-backed kernel opportunity request JSON")
	requireMeasured := fs.Bool("require-measured", false, "reject fixture or simulated profiler evidence")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*filePath) == "" {
		return errors.New("usage: infercrane optimize kernel-plan --file PROFILE.json [--require-measured] [--output human|json]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	body, err := os.ReadFile(*filePath)
	if err != nil {
		return fmt.Errorf("read kernel opportunity profile: %w", err)
	}
	var request kernelplanner.Request
	if err = json.Unmarshal(body, &request); err != nil {
		return fmt.Errorf("decode kernel opportunity profile: %w", err)
	}
	if *requireMeasured {
		request.Policy.RequireMeasuredProfiler = true
	}
	plan, err := kernelplanner.Build(request)
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(plan)
	}
	fmt.Printf("Kernel opportunities · %s · %s on %s\n", request.Model.Repository, request.Workload.Phase, request.Hardware.Accelerator)
	fmt.Printf("Evidence %s · input %s\n\n", plan.EvidenceClass, shortID(strings.TrimPrefix(plan.InputDigest, "sha256:")))
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "RANK\tOPERATOR\tHOTSPOT\tSHARE\tAMDAHL CEILING\tTARGET\tKERNEL NEEDED\tSEARCH ORDER\tSTATUS")
	for _, candidate := range plan.Candidates {
		projectCounts := make(map[string]int, len(candidate.ExistingImplementations))
		for _, implementation := range candidate.ExistingImplementations {
			projectCounts[implementation.Project]++
		}
		sources := make([]string, 0, len(candidate.ExistingImplementations))
		for _, implementation := range candidate.ExistingImplementations {
			label := implementation.Project
			if projectCounts[label] > 1 {
				label += " (" + implementation.Backend + ")"
			}
			sources = append(sources, label)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%.1f%%\t%.3fx\t%.3fx\t%.3fx\t%s\t%s\n", candidate.Rank, candidate.OperatorFamily, candidate.HotspotID, candidate.DeviceTimeFraction*100, candidate.MaxEndToEndSpeedup, candidate.TargetEndToEndSpeedup, candidate.RequiredKernelSpeedup, strings.Join(sources, " → "), candidate.Status)
	}
	if err = w.Flush(); err != nil {
		return err
	}
	for _, rejected := range plan.Rejected {
		fmt.Printf("Rejected %s · %s\n", rejected.HotspotID, rejected.Reason)
	}
	fmt.Println("\nThese are bounded experiments, not kernel performance claims. Target-GPU and end-to-end AIPerf evidence are still required.")
	return nil
}

func optimizeEvidenceCommand(args []string) error {
	if len(args) < 1 || (args[0] != "evaluate" && args[0] != "inspect-fastpath") {
		return errors.New("usage: infercrane optimize evidence evaluate|inspect-fastpath --file FILE [--output human|json]")
	}
	action := args[0]
	fs := flag.NewFlagSet("optimize evidence "+action, flag.ContinueOnError)
	filePath := fs.String("file", "", "immutable optimization evidence JSON")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*filePath) == "" {
		return errors.New("optimize evidence " + action + " requires --file and no positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	body, err := os.ReadFile(*filePath)
	if err != nil {
		return fmt.Errorf("read optimization evidence: %w", err)
	}
	if action == "inspect-fastpath" {
		imported, decodeErr := optimizationevidence.DecodeFastPathManifest(body)
		if decodeErr != nil {
			return decodeErr
		}
		if *output == "json" {
			return printJSON(imported)
		}
		fmt.Printf("FastPath candidate · %s\n", imported.CandidateID)
		fmt.Printf("Model       %s\nRuntime     %s\nGPU         %d× %s\nManifest    %s\nClaimed     %s / %s\nImported    %s\n\n", imported.ModelIdentity, imported.RuntimeIdentity, imported.GPUCount, imported.GPU, imported.ManifestDigest, imported.ClaimedStatus, imported.ClaimedEvidenceState, imported.ImportedEvidenceState)
		fmt.Println("Qualification remains blocked until InferCrane binds the exact revision and passing signed quality evidence.")
		return nil
	}
	campaign, err := optimizationevidence.Decode(body)
	if err != nil {
		return err
	}
	evaluation, err := optimizationevidence.Evaluate(campaign)
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(evaluation)
	}
	fmt.Printf("Optimization evidence · %s\n", evaluation.ModelIdentity)
	fmt.Printf("Workload    %s\nPolicy      %s@%d\nEvidence    %s\nWinner      %s\n\n", evaluation.WorkloadDigest, evaluation.SelectionPolicy.ID, evaluation.SelectionPolicy.Version, evaluation.InputDigest, emptyAs(evaluation.WinnerID, "none"))
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "CHOICE\tCANDIDATE\tRUNTIME\tQUALIFIED\tSCORE\tPARETO\tREASON")
	for _, candidate := range evaluation.Candidates {
		choice := ""
		if candidate.Selected {
			choice = "SELECTED"
		}
		score := "-"
		if candidate.Score != nil {
			score = strconv.FormatFloat(*candidate.Score, 'f', 4, 64)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\t%t\t%s\n", choice, candidate.CandidateID, candidate.RuntimeID, candidate.Qualified, score, candidate.Pareto, strings.Join(candidate.RejectionReasons, "; "))
	}
	return w.Flush()
}

type optimizationCampaignView struct {
	ID                 string     `json:"id"`
	ModelIdentity      string     `json:"model_identity"`
	Objective          string     `json:"objective"`
	Source             string     `json:"source"`
	Intent             string     `json:"intent"`
	TargetDeployment   string     `json:"target_deployment"`
	State              string     `json:"state"`
	ApprovedMaxCostUSD *float64   `json:"approved_max_cost_usd"`
	ApprovalExpiresAt  *time.Time `json:"approval_expires_at"`
	Candidates         []struct {
		ID                  string `json:"id"`
		ProposalCandidateID string `json:"proposal_candidate_id"`
		State               string `json:"state"`
		EvidenceState       string `json:"evidence_state"`
		TargetEndpoint      string `json:"target_endpoint"`
		DeploymentName      string `json:"deployment_name"`
		RevisionID          string `json:"revision_id"`
		BenchmarkID         string `json:"benchmark_id"`
		FailureCode         string `json:"failure_code"`
		Rank                int    `json:"rank"`
	} `json:"candidates"`
}

func optimizeCampaignCommand(ctx context.Context, args []string) error {
	action := args[0]
	flagArgs := args[1:]
	id := ""
	if action != "list" {
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New("usage: infercrane optimize " + action + " CAMPAIGN [flags]")
		}
		id = args[1]
		flagArgs = args[2:]
	}
	fs := flag.NewFlagSet("optimize "+action, flag.ContinueOnError)
	limit := fs.Int("limit", 20, "maximum campaigns")
	maxCost := fs.Float64("max-cost-usd", 0, "hard maximum campaign spend")
	expiresIn := fs.Duration("expires-in", time.Hour, "approval expiry, from 1m through 24h")
	candidateID := fs.String("candidate", "", "exact qualified candidate ID to activate or promote")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	if action == "list" {
		if fs.NArg() != 0 {
			return errors.New("usage: infercrane optimize list [--limit N]")
		}
		var response struct {
			Data []optimizationCampaignView `json:"data"`
		}
		if err = controlJSON(ctx, cfg, http.MethodGet, "/api/v1/optimization/campaigns?limit="+strconv.Itoa(*limit), "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		for _, campaign := range response.Data {
			fmt.Printf("%-36s  %-18s  %-14s  %s\n", campaign.ID, campaign.State, campaign.Objective, campaign.ModelIdentity)
		}
		if len(response.Data) == 0 {
			fmt.Println("No optimization campaigns.")
		}
		return nil
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane optimize " + action + " CAMPAIGN [flags]")
	}
	path := "/api/v1/optimization/campaigns/" + url.PathEscape(id)
	var response struct {
		Campaign         optimizationCampaignView `json:"campaign"`
		Operation        *domain.Operation        `json:"operation,omitempty"`
		Activation       *domain.Operation        `json:"activation_operation,omitempty"`
		CleanupOperation *domain.Operation        `json:"cleanup_operation,omitempty"`
	}
	switch action {
	case "inspect", "results":
		err = controlJSON(ctx, cfg, http.MethodGet, path, "", nil, &response)
	case "approve":
		if *maxCost <= 0 || *expiresIn < time.Minute || *expiresIn > 24*time.Hour {
			return errors.New("approve requires --max-cost-usd greater than zero and --expires-in between 1m and 24h")
		}
		err = controlJSON(ctx, cfg, http.MethodPost, path+"/approve", "", map[string]any{"max_cost_usd": *maxCost, "expires_in_seconds": int(expiresIn.Seconds())}, &response)
	case "activate":
		if strings.TrimSpace(*candidateID) == "" {
			return errors.New("activate requires --candidate CANDIDATE_ID")
		}
		err = controlJSON(ctx, cfg, http.MethodPost, path+"/activate", "", map[string]any{"candidate_id": strings.TrimSpace(*candidateID)}, &response)
	case "cancel":
		err = controlJSON(ctx, cfg, http.MethodPost, path+"/cancel", "", map[string]any{}, &response)
	default:
		return errors.New("unknown optimization campaign action")
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	view := response.Campaign
	fmt.Printf("Optimization campaign %s · %s\n", view.ID, view.State)
	fmt.Printf("Model      %s\nObjective  %s\nIntent     %s\nSource     %s\n", view.ModelIdentity, view.Objective, view.Intent, view.Source)
	if view.TargetDeployment != "" {
		fmt.Printf("Target     %s\n", view.TargetDeployment)
	}
	if view.ApprovedMaxCostUSD != nil {
		fmt.Printf("Cost cap   $%.2f\n", *view.ApprovedMaxCostUSD)
	}
	if response.Operation != nil {
		fmt.Printf("Execution  %s · %s\n", response.Operation.ID, response.Operation.Status)
	}
	if response.Activation != nil {
		fmt.Printf("Activation %s · %s\n", response.Activation.ID, response.Activation.Status)
	}
	if response.CleanupOperation != nil {
		fmt.Printf("Cleanup    %s · %s\n", response.CleanupOperation.ID, response.CleanupOperation.Status)
	}
	if len(view.Candidates) > 0 {
		fmt.Println("\nCANDIDATES")
		for _, candidate := range view.Candidates {
			fmt.Printf("  %d  %-16s  %-11s  %s", candidate.Rank, candidate.State, candidate.EvidenceState, candidate.ID)
			if candidate.TargetEndpoint != "" {
				fmt.Printf(" · endpoint %s", candidate.TargetEndpoint)
			}
			if candidate.FailureCode != "" {
				fmt.Printf(" · %s", candidate.FailureCode)
			}
			fmt.Println()
		}
	}
	return nil
}

func optimizationProposal(ctx context.Context, request optimizer.Request, catalog optimizer.CatalogSource, sourceName, python string, allowNetwork bool) (optimizer.Proposal, error) {
	sourceName = strings.ToLower(strings.TrimSpace(sourceName))
	if sourceName != "auto" && sourceName != "catalog" && sourceName != "aiconfigurator" {
		return optimizer.Proposal{}, errors.New("source must be auto, catalog, or aiconfigurator")
	}
	if sourceName == "catalog" {
		return catalog.Propose(ctx, request)
	}
	estimator := optimizer.AIConfiguratorSource{Catalog: catalog, Runner: optimizer.PythonEstimatorRunner{Python: python, AllowNetwork: allowNetwork}}
	proposal, err := estimator.Propose(ctx, request)
	if err == nil || sourceName == "aiconfigurator" {
		return proposal, err
	}
	if !errors.Is(err, optimizer.ErrEstimatorUnavailable) {
		return optimizer.Proposal{}, err
	}
	proposal, catalogErr := catalog.Propose(ctx, request)
	if catalogErr != nil {
		return optimizer.Proposal{}, catalogErr
	}
	proposal.Warnings = append(proposal.Warnings, "AIConfigurator was unavailable; generated conservative catalog candidates instead (run `infercrane optimize doctor` for setup guidance)")
	return proposal, nil
}

func optimizeDoctorCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("optimize doctor", flag.ContinueOnError)
	python := fs.String("aiconfigurator-python", "python3", "Python executable containing aiconfigurator 0.11.0")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane optimize doctor [--aiconfigurator-python PATH]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	version, err := (optimizer.PythonEstimatorRunner{Python: *python}).Probe(ctx)
	available := err == nil
	result := map[string]any{"source": "aiconfigurator", "available": available, "required_version": optimizer.AIConfiguratorVersion, "python": *python, "migration_boundary": "optimizer.Source", "qualification_authority": "InferCrane measured evidence"}
	if err != nil {
		result["reason"] = err.Error()
		result["install"] = *python + " -m pip install aiconfigurator==" + optimizer.AIConfiguratorVersion + " plotext==" + optimizer.AIConfiguratorPlotextVersion
	} else {
		result["version"] = version
	}
	if *output == "json" {
		return printJSON(result)
	}
	if available {
		fmt.Printf("AIConfigurator  ready · %s · %s\n", version, *python)
		fmt.Println("Boundary        modeled candidate source; AIPerf, quality, cost, and Release Guard still qualify")
		return nil
	}
	fmt.Printf("AIConfigurator  unavailable · %s\n", err)
	fmt.Printf("Install         %s -m pip install aiconfigurator==%s plotext==%s\n", *python, optimizer.AIConfiguratorVersion, optimizer.AIConfiguratorPlotextVersion)
	fmt.Println("Fallback        `infercrane optimize propose` continues with reviewed catalog candidates")
	return nil
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func optionalMilliseconds(value string) (*float64, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parsed, err := parseMilliseconds(value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func optionalNumber(value string) (*float64, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
		return nil, errors.New("must be a finite nonnegative number")
	}
	return &parsed, nil
}

func optionalPositiveNumber(value string) (*float64, error) {
	result, err := optionalNumber(value)
	if err != nil || result == nil {
		return result, err
	}
	if *result == 0 {
		return nil, errors.New("must be greater than zero")
	}
	return result, nil
}

func writeCandidateSpecs(directory string, candidates []optimizer.Candidate) error {
	info, err := os.Stat(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect candidate directory: %w", err)
	}
	if err == nil && !info.IsDir() {
		return errors.New("candidate output path exists and is not a directory")
	}
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create candidate directory: %w", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read candidate directory: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("candidate output directory must be empty to prevent overwriting prior evidence")
	}
	for _, candidate := range candidates {
		encoded, marshalErr := yaml.Marshal(candidate.Deployment)
		if marshalErr != nil {
			return fmt.Errorf("encode candidate %s: %w", candidate.ID, marshalErr)
		}
		path := filepath.Join(directory, fmt.Sprintf("%02d-%s.yaml", candidate.Rank, candidate.Deployment.Name))
		if writeErr := os.WriteFile(path, encoded, 0o644); writeErr != nil {
			return fmt.Errorf("write candidate %s: %w", candidate.ID, writeErr)
		}
	}
	return nil
}
