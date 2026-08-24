// Package optimizer proposes serving configurations without claiming that an
// unmeasured configuration satisfies a performance or cost objective.
//
// Proposal sources are replaceable. The first source composes InferCrane's
// reviewed model catalog with its executable provider/runtime compatibility
// inventory. External estimators such as AIConfigurator or AISimulate can be
// added behind Source without becoming part of the InferCrane domain model.
package optimizer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizationcapability"
	"github.com/infercrane/infercrane/internal/performanceprofile"
	"github.com/infercrane/infercrane/internal/servingcontract"
)

const (
	SchemaVersion    = "infercrane.optimizer.proposal/v1"
	AlgorithmVersion = "catalog-candidate-planner-v1"
)

// EvidenceState makes the optimization proof boundary explicit. A state is
// immutable evidence about one exact candidate identity; changed inputs create
// a new candidate or make existing evidence stale rather than rewriting its
// provenance.
type EvidenceState string

const (
	EvidenceUnmeasured EvidenceState = "unmeasured"
	EvidenceModeled    EvidenceState = "modeled"
	EvidenceMeasured   EvidenceState = "measured"
	EvidenceQualified  EvidenceState = "qualified"
	EvidenceRejected   EvidenceState = "rejected"
	EvidenceStale      EvidenceState = "stale"
)

var objectives = map[string]string{
	"interactive":     "interactive",
	"latency":         "interactive",
	"throughput":      "throughput",
	"cost-efficiency": "throughput",
}

// Request is the provider-neutral optimization intent. SLO and cost fields are
// carried into every candidate as evidence requirements; this planner never
// fabricates values for them.
type Request struct {
	ModelIdentity         string   `json:"model_identity"`
	Provider              string   `json:"provider"`
	Region                string   `json:"region"`
	GPU                   string   `json:"gpu"`
	Runtimes              []string `json:"runtimes,omitempty"`
	Objective             string   `json:"objective"`
	WorkloadProfile       string   `json:"workload_profile"`
	MaxTTFTP95MS          *float64 `json:"max_ttft_p95_ms,omitempty"`
	MaxTPOTP95MS          *float64 `json:"max_tpot_p95_ms,omitempty"`
	MinOutputTokensSecond *float64 `json:"min_output_tokens_second,omitempty"`
	MaxHourlyCost         *float64 `json:"max_hourly_cost,omitempty"`
	IncludeSimulated      bool     `json:"include_simulated,omitempty"`
	WorkloadFingerprint   string   `json:"workload_fingerprint,omitempty"`
	TargetConcurrency     *float64 `json:"target_concurrency,omitempty"`
	MaxCandidates         int      `json:"max_candidates"`
}

type SourceInfo struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	EvidenceClass   string `json:"evidence_class"`
	UpstreamProject string `json:"upstream_project,omitempty"`
}

type Feature struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Source string `json:"source"`
}

type DeploymentDraft struct {
	APIVersion string `json:"api_version" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	Name       string `json:"name" yaml:"name"`
	Model      struct {
		ID       string `json:"id" yaml:"id"`
		Revision string `json:"revision" yaml:"revision"`
	} `json:"model" yaml:"model"`
	Runtime struct {
		Engine  string   `json:"engine" yaml:"engine"`
		Version string   `json:"version" yaml:"version"`
		Args    []string `json:"args" yaml:"args"`
	} `json:"runtime" yaml:"runtime"`
	Compute struct {
		Mode string `json:"mode" yaml:"mode"`
	} `json:"compute" yaml:"compute"`
	Resources struct {
		GPU string `json:"gpu" yaml:"gpu"`
	} `json:"resources" yaml:"resources"`
	Provider struct {
		Cloud   string `json:"cloud" yaml:"cloud"`
		Adapter string `json:"adapter" yaml:"adapter"`
		Region  string `json:"region" yaml:"region"`
	} `json:"provider" yaml:"provider"`
	Scaling struct {
		MinReplicas int `json:"min_replicas" yaml:"min_replicas"`
		MaxReplicas int `json:"max_replicas" yaml:"max_replicas"`
	} `json:"scaling" yaml:"scaling"`
	Routing struct {
		Strategy string `json:"strategy" yaml:"strategy"`
	} `json:"routing" yaml:"routing"`
	Serving servingcontract.Topology `json:"serving,omitzero" yaml:"serving,omitempty"`
}

// ModeledEvidence is an estimator prediction, never a qualification result.
// The raw upstream result stays behind its digest so changes in upstream
// schemas cannot silently become InferCrane's durable domain model.
type ModeledEvidence struct {
	SchemaVersion               string   `json:"schema_version"`
	SourceVersion               string   `json:"source_version"`
	ResultDigest                string   `json:"result_digest"`
	System                      string   `json:"system"`
	Mode                        string   `json:"mode"`
	EstimatedTTFTMS             *float64 `json:"estimated_ttft_ms,omitempty"`
	EstimatedTPOTMS             *float64 `json:"estimated_tpot_ms,omitempty"`
	EstimatedRequestLatencyMS   *float64 `json:"estimated_request_latency_ms,omitempty"`
	EstimatedRequestRate        *float64 `json:"estimated_request_rate,omitempty"`
	EstimatedOutputTokensPerGPU *float64 `json:"estimated_output_tokens_second_per_gpu,omitempty"`
	TotalGPUs                   int      `json:"total_gpus,omitempty"`
	Replicas                    int      `json:"replicas,omitempty"`
	GPUsPerReplica              int      `json:"gpus_per_replica,omitempty"`
	Warnings                    []string `json:"warnings,omitempty"`
}

type Candidate struct {
	ID                    string           `json:"id"`
	Rank                  int              `json:"rank"`
	Status                string           `json:"status"`
	EvidenceState         EvidenceState    `json:"evidence_state"`
	Objective             string           `json:"objective"`
	ConfigurationProfile  string           `json:"configuration_profile"`
	BenchmarkProfile      string           `json:"benchmark_profile"`
	CompatibilityState    string           `json:"compatibility_state"`
	CompatibilityEvidence string           `json:"compatibility_evidence,omitempty"`
	Source                SourceInfo       `json:"source"`
	Features              []Feature        `json:"features"`
	RequiredEvidence      []string         `json:"required_evidence"`
	Limitations           []string         `json:"limitations"`
	ModeledEvidence       *ModeledEvidence `json:"modeled_evidence,omitempty"`
	Deployment            DeploymentDraft  `json:"deployment"`
}

type Proposal struct {
	SchemaVersion     string      `json:"schema_version"`
	AlgorithmVersion  string      `json:"algorithm_version"`
	Input             Request     `json:"input"`
	InputDigest       string      `json:"input_digest"`
	Candidates        []Candidate `json:"candidates"`
	Missing           []string    `json:"missing,omitempty"`
	Warnings          []string    `json:"warnings,omitempty"`
	Mutation          string      `json:"mutation"`
	SelectionBoundary string      `json:"selection_boundary"`
}

// Source creates candidate configurations. It may estimate or discover, but
// it cannot mark a candidate qualified; measured evidence owns that decision.
type Source interface {
	Propose(context.Context, Request) (Proposal, error)
}

// ValidateProposal verifies an untrusted proposal before the control plane
// persists it. It proves identity and shape, not performance.
func ValidateProposal(proposal Proposal) error {
	if proposal.SchemaVersion != SchemaVersion || proposal.AlgorithmVersion == "" || proposal.Mutation != "none" || proposal.SelectionBoundary == "" {
		return errors.New("proposal must use the supported immutable non-mutating schema")
	}
	request := normalizeRequest(proposal.Input)
	if err := validateRequest(request); err != nil {
		return err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	if proposal.InputDigest != hex.EncodeToString(sum[:]) {
		return errors.New("proposal input digest does not match normalized input")
	}
	if len(proposal.Candidates) < 1 || len(proposal.Candidates) > request.MaxCandidates {
		return errors.New("proposal candidate count is empty or exceeds the requested bound")
	}
	seenIDs, seenRanks := map[string]struct{}{}, map[int]struct{}{}
	for _, candidate := range proposal.Candidates {
		if len(candidate.ID) != 64 || candidate.Rank < 1 || candidate.Rank > len(proposal.Candidates) || candidate.Objective != request.Objective || candidate.BenchmarkProfile != request.WorkloadProfile || candidate.Deployment.APIVersion != "infercrane.dev/v1" || candidate.Deployment.Kind != "Deployment" || candidate.Deployment.Name == "" || candidate.Deployment.Model.ID == "" || candidate.Deployment.Model.Revision == "" || candidate.Deployment.Runtime.Engine == "" || candidate.Deployment.Runtime.Version == "" || candidate.Deployment.Resources.GPU != request.GPU {
			return errors.New("proposal contains an incomplete or mismatched candidate")
		}
		if candidate.EvidenceState != EvidenceUnmeasured && candidate.EvidenceState != EvidenceModeled {
			return errors.New("new proposal candidates must be unmeasured or modeled")
		}
		if candidate.EvidenceState == EvidenceModeled && candidate.ModeledEvidence == nil {
			return errors.New("modeled candidate requires modeled evidence provenance")
		}
		if _, duplicate := seenIDs[candidate.ID]; duplicate {
			return errors.New("proposal candidate IDs must be unique")
		}
		if _, duplicate := seenRanks[candidate.Rank]; duplicate {
			return errors.New("proposal candidate ranks must be unique")
		}
		seenIDs[candidate.ID], seenRanks[candidate.Rank] = struct{}{}, struct{}{}
	}
	return nil
}

type CatalogSource struct {
	Recipes      []curatedrecipe.Entry
	Integrations integration.Snapshot
}

func NewCatalogSource(recipes []curatedrecipe.Entry, integrations integration.Snapshot) CatalogSource {
	return CatalogSource{Recipes: append([]curatedrecipe.Entry(nil), recipes...), Integrations: integrations}
}

func (s CatalogSource) Propose(_ context.Context, request Request) (Proposal, error) {
	request = normalizeRequest(request)
	if err := validateRequest(request); err != nil {
		return Proposal{}, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return Proposal{}, err
	}
	sum := sha256.Sum256(input)
	proposal := Proposal{SchemaVersion: SchemaVersion, AlgorithmVersion: AlgorithmVersion, Input: request, InputDigest: hex.EncodeToString(sum[:]), Mutation: "none", SelectionBoundary: "configuration candidates only; benchmark, quality, cost, and Release Guard evidence are required before qualification"}
	entry, ok := findRecipe(s.Recipes, request.ModelIdentity)
	if !ok {
		proposal.Missing = []string{"reviewed_model_recipe"}
		return proposal, nil
	}
	compatibility := matchingCompatibility(s.Integrations.Compatibility, request)
	if len(compatibility) == 0 {
		proposal.Missing = []string{"qualified_provider_runtime_compatibility"}
		return proposal, nil
	}
	profiles := matchingProfiles(entry.Profiles, request)
	if len(profiles) == 0 {
		proposal.Missing = []string{"reviewed_runtime_profile"}
		return proposal, nil
	}
	preferredProfile := objectives[request.Objective]
	for _, profile := range profiles {
		for _, compatible := range compatibility {
			if compatible.Runtime != profile.Runtime || string(compatible.Mode) != profile.ComputeMode {
				continue
			}
			candidate, compileErr := buildCandidate(entry, profile, compatible, runtimeVersion(s.Integrations.Runtimes, profile.Runtime), request, preferredProfile)
			if compileErr != nil {
				return Proposal{}, fmt.Errorf("compile reviewed profile %s: %w", profile.Name, compileErr)
			}
			proposal.Candidates = append(proposal.Candidates, candidate)
		}
	}
	sort.Slice(proposal.Candidates, func(i, j int) bool {
		a, b := proposal.Candidates[i], proposal.Candidates[j]
		if configurationProfileRank(a.ConfigurationProfile, preferredProfile) != configurationProfileRank(b.ConfigurationProfile, preferredProfile) {
			return configurationProfileRank(a.ConfigurationProfile, preferredProfile) < configurationProfileRank(b.ConfigurationProfile, preferredProfile)
		}
		if qualificationRank(a.CompatibilityState) != qualificationRank(b.CompatibilityState) {
			return qualificationRank(a.CompatibilityState) < qualificationRank(b.CompatibilityState)
		}
		return a.ID < b.ID
	})
	if len(proposal.Candidates) > request.MaxCandidates {
		proposal.Candidates = proposal.Candidates[:request.MaxCandidates]
	}
	for index := range proposal.Candidates {
		proposal.Candidates[index].Rank = index + 1
	}
	if len(proposal.Candidates) == 0 {
		proposal.Missing = []string{"compatible_reviewed_candidate"}
	}
	return proposal, nil
}

func configurationProfileRank(name, preferred string) int {
	if name == preferred || strings.HasSuffix(name, "-"+preferred) {
		return 0
	}
	if name == "balanced" || strings.HasSuffix(name, "-balanced") {
		return 1
	}
	return 2
}

func normalizeRequest(request Request) Request {
	request.ModelIdentity = strings.TrimSpace(request.ModelIdentity)
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	request.Region = strings.TrimSpace(request.Region)
	request.GPU = strings.TrimSpace(request.GPU)
	request.Objective = strings.ToLower(strings.TrimSpace(request.Objective))
	if request.Objective == "" {
		request.Objective = "interactive"
	}
	request.WorkloadProfile = strings.ToLower(strings.TrimSpace(request.WorkloadProfile))
	request.WorkloadFingerprint = strings.TrimSpace(request.WorkloadFingerprint)
	if request.WorkloadProfile == "" {
		request.WorkloadProfile = objectives[request.Objective]
	}
	if request.MaxCandidates == 0 {
		request.MaxCandidates = 10
	}
	seen := map[string]struct{}{}
	var runtimes []string
	for _, runtimeName := range request.Runtimes {
		runtimeName = strings.ToLower(strings.TrimSpace(runtimeName))
		if runtimeName == "" {
			continue
		}
		if _, duplicate := seen[runtimeName]; !duplicate {
			seen[runtimeName] = struct{}{}
			runtimes = append(runtimes, runtimeName)
		}
	}
	sort.Strings(runtimes)
	request.Runtimes = runtimes
	return request
}

func validateRequest(request Request) error {
	if request.ModelIdentity == "" || request.Provider == "" || request.GPU == "" {
		return errors.New("model identity, provider, and GPU are required")
	}
	if _, ok := objectives[request.Objective]; !ok {
		return errors.New("objective must be interactive, latency, throughput, or cost-efficiency")
	}
	if _, err := performanceprofile.Get(request.WorkloadProfile); err != nil {
		return err
	}
	if request.MaxCandidates < 1 || request.MaxCandidates > 100 {
		return errors.New("max candidates must be between 1 and 100")
	}
	if len(request.WorkloadFingerprint) > 256 {
		return errors.New("workload fingerprint must be at most 256 characters")
	}
	if request.TargetConcurrency != nil && (*request.TargetConcurrency <= 0 || math.IsNaN(*request.TargetConcurrency) || math.IsInf(*request.TargetConcurrency, 0)) {
		return errors.New("target concurrency must be a finite positive value")
	}
	if (request.Provider == "aws" || request.Provider == "aws-ec2" || request.Provider == "gcp" || request.Provider == "gcp-compute") && request.Region == "" {
		return errors.New("AWS and GCP candidates require an explicit region")
	}
	for name, value := range map[string]*float64{"max_ttft_p95_ms": request.MaxTTFTP95MS, "max_tpot_p95_ms": request.MaxTPOTP95MS, "min_output_tokens_second": request.MinOutputTokensSecond, "max_hourly_cost": request.MaxHourlyCost} {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return fmt.Errorf("%s must be a finite nonnegative value", name)
		}
	}
	return nil
}

func findRecipe(entries []curatedrecipe.Entry, identity string) (curatedrecipe.Entry, bool) {
	for _, entry := range entries {
		exact := entry.Model + "@" + entry.Revision
		if identity == entry.Name || identity == entry.Model || identity == exact {
			return entry, true
		}
	}
	return curatedrecipe.Entry{}, false
}

func matchingCompatibility(entries []integration.RuntimeCompatibility, request Request) []integration.RuntimeCompatibility {
	allowedRuntime := func(value string) bool {
		if len(request.Runtimes) == 0 {
			return true
		}
		for _, candidate := range request.Runtimes {
			if candidate == value {
				return true
			}
		}
		return false
	}
	var out []integration.RuntimeCompatibility
	for _, entry := range entries {
		if request.Provider != entry.Cloud && request.Provider != entry.Adapter || !allowedRuntime(entry.Runtime) {
			continue
		}
		if entry.State != integration.QualificationLocal && entry.State != integration.QualificationReal && !(request.IncludeSimulated && entry.State == integration.QualificationSimulated) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func matchingProfiles(entries []curatedrecipe.ServingProfile, request Request) []curatedrecipe.ServingProfile {
	var out []curatedrecipe.ServingProfile
	for _, entry := range entries {
		if len(request.Runtimes) > 0 {
			matched := false
			for _, runtimeName := range request.Runtimes {
				matched = matched || runtimeName == entry.Runtime
			}
			if !matched {
				continue
			}
		}
		out = append(out, entry)
	}
	return out
}

func buildCandidate(entry curatedrecipe.Entry, profile curatedrecipe.ServingProfile, compatible integration.RuntimeCompatibility, runtimeVersion string, request Request, preferredProfile string) (Candidate, error) {
	draft := DeploymentDraft{APIVersion: "infercrane.dev/v1", Kind: "Deployment"}
	draft.Name = safeName(entry.Name + "-" + profile.Name + "-" + compatible.Cloud)
	draft.Model.ID, draft.Model.Revision = entry.Model, entry.Revision
	draft.Runtime.Engine, draft.Runtime.Version = profile.Runtime, runtimeVersion
	draft.Compute.Mode = profile.ComputeMode
	draft.Resources.GPU = request.GPU
	draft.Provider.Cloud, draft.Provider.Adapter, draft.Provider.Region = compatible.Cloud, compatible.Adapter, request.Region
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = profile.MinReplicas, profile.MaxReplicas
	draft.Routing.Strategy = "round-robin"
	capabilities, err := optimizationcapability.V1()
	if err != nil {
		return Candidate{}, err
	}
	base := optimizationcapability.Request{Runtime: profile.Runtime, RuntimeVersion: runtimeVersion, Model: entry.Model, ArtifactPrecision: "bf16", Accelerator: request.GPU}
	continuous, err := capabilities.Compile(withMechanism(base, optimizationcapability.ContinuousBatching, nil))
	if err != nil {
		return Candidate{}, err
	}
	features := []Feature{{Name: "continuous_batching", State: "runtime-owned", Source: continuous.DescriptorID}}
	if contains(profile.RuntimeArgs, "--enable-prefix-caching") {
		compiled, compileErr := capabilities.Compile(withMechanism(base, optimizationcapability.PrefixCaching, nil))
		if compileErr != nil {
			return Candidate{}, compileErr
		}
		draft.Runtime.Args = append(draft.Runtime.Args, compiled.Arguments...)
		features = append(features, Feature{Name: "prefix_caching", State: "enabled", Source: compiled.DescriptorID})
	}
	if contains(profile.RuntimeArgs, "--max-num-batched-tokens") {
		value, valueErr := argumentValue(profile.RuntimeArgs, "--max-num-batched-tokens")
		if valueErr != nil {
			return Candidate{}, valueErr
		}
		compiled, compileErr := capabilities.Compile(withMechanism(base, optimizationcapability.ChunkedPrefill, map[string]string{"max_num_batched_tokens": value}))
		if compileErr != nil {
			return Candidate{}, compileErr
		}
		draft.Runtime.Args = append(draft.Runtime.Args, compiled.Arguments...)
		features = append(features, Feature{Name: "batch_token_budget", State: "configured", Source: compiled.DescriptorID})
	}
	if len(profile.RuntimeArgs) != len(draft.Runtime.Args) {
		return Candidate{}, errors.New("reviewed profile contains an argument without a capability descriptor")
	}
	required := []string{"runtime readiness and exact served-model identity", "AIPerf " + request.WorkloadProfile + " workload", "fresh provider capacity observation"}
	if request.MaxHourlyCost != nil || request.Objective == "cost-efficiency" {
		required = append(required, "sourced hourly cost")
	}
	if request.MaxTTFTP95MS != nil || request.MaxTPOTP95MS != nil || request.MinOutputTokensSecond != nil {
		required = append(required, "measured SLO metrics")
	}
	required = append(required, "semantic quality evidence when model precision or artifact changes")
	limitations := append([]string(nil), profile.Limitations...)
	limitations = append(limitations, "GPU fit for "+request.GPU+" is not inferred from the reviewed profile hint "+profile.GPUHint+".")
	if profile.Name != preferredProfile && !strings.HasSuffix(profile.Name, "-"+preferredProfile) {
		limitations = append(limitations, "This profile is an alternative candidate; it is not ranked by unmeasured performance.")
	}
	source := SourceInfo{Name: "infercrane-curated-catalog", Version: entry.ReviewedAt, EvidenceClass: profile.EvidenceClass}
	identity, _ := json.Marshal(struct {
		Source        SourceInfo                       `json:"source"`
		Deployment    DeploymentDraft                  `json:"deployment"`
		Profile       string                           `json:"profile"`
		Compatibility integration.RuntimeCompatibility `json:"compatibility"`
	}{source, draft, request.WorkloadProfile, compatible})
	sum := sha256.Sum256(identity)
	return Candidate{ID: hex.EncodeToString(sum[:]), Status: "proposed-unmeasured", EvidenceState: EvidenceUnmeasured, Objective: request.Objective, ConfigurationProfile: profile.Name, BenchmarkProfile: request.WorkloadProfile, CompatibilityState: string(compatible.State), CompatibilityEvidence: compatible.Evidence, Source: source, Features: features, RequiredEvidence: required, Limitations: limitations, Deployment: draft}, nil
}

func withMechanism(request optimizationcapability.Request, mechanism optimizationcapability.Mechanism, parameters map[string]string) optimizationcapability.Request {
	request.Mechanism, request.Parameters = mechanism, parameters
	return request
}

func argumentValue(arguments []string, flagName string) (string, error) {
	for index, argument := range arguments {
		if argument == flagName {
			if index+1 >= len(arguments) || strings.HasPrefix(arguments[index+1], "--") {
				return "", fmt.Errorf("%s requires a value", flagName)
			}
			return arguments[index+1], nil
		}
	}
	return "", fmt.Errorf("%s is not present", flagName)
}

func runtimeVersion(entries []integration.RuntimeProfile, runtimeName string) string {
	for _, entry := range entries {
		if entry.Runtime == runtimeName {
			return entry.EngineVersion
		}
	}
	return ""
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func qualificationRank(state string) int {
	switch state {
	case string(integration.QualificationReal):
		return 0
	case string(integration.QualificationLocal):
		return 1
	case string(integration.QualificationSimulated):
		return 2
	default:
		return 3
	}
}

func safeName(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(strings.TrimSpace(b.String()), "-")
}
