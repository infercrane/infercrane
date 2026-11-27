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
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/modelimport"
	"github.com/infercrane/infercrane/internal/optimizationcapability"
	"github.com/infercrane/infercrane/internal/performanceprofile"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/servingcontract"
	"github.com/infercrane/infercrane/internal/speculator"
	"github.com/infercrane/infercrane/internal/support"
	"github.com/infercrane/infercrane/internal/workloadprofile"
)

const (
	SchemaVersion    = "infercrane.optimizer.proposal/v1"
	AlgorithmVersion = "catalog-candidate-planner-v2"

	WorkloadSourceReviewedDefault  = "reviewed_default"
	WorkloadSourcePublicPrior      = "public_prior"
	WorkloadSourceCustomerObserved = "customer_observed"
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
	// A useful-output goal counts only requests within the first-token limit,
	// so it plans like an interactive one.
	lab.ObjectiveUsefulOutput: "interactive",
}

// Request is the provider-neutral optimization intent. SLO and cost fields are
// carried into every candidate as evidence requirements; this planner never
// fabricates values for them.
type Request struct {
	ModelIdentity         string   `json:"model_identity"`
	ModelRevision         string   `json:"model_revision,omitempty"`
	Provider              string   `json:"provider"`
	ComputeConnectionID   string   `json:"compute_connection_id,omitempty"`
	ProviderAdapter       string   `json:"provider_adapter,omitempty"`
	Region                string   `json:"region"`
	GPU                   string   `json:"gpu"`
	GPUCount              int      `json:"gpu_count,omitempty"`
	Runtimes              []string `json:"runtimes,omitempty"`
	Objective             string   `json:"objective"`
	WorkloadProfile       string   `json:"workload_profile"`
	MaxTTFTP95MS          *float64 `json:"max_ttft_p95_ms,omitempty"`
	MaxTPOTP95MS          *float64 `json:"max_tpot_p95_ms,omitempty"`
	MaxErrorRate          *float64 `json:"max_error_rate,omitempty"`
	MinGoodput            *float64 `json:"min_goodput,omitempty"`
	MinOutputTokensSecond *float64 `json:"min_output_tokens_second,omitempty"`
	MaxHourlyCost         *float64 `json:"max_hourly_cost,omitempty"`
	IncludeSimulated      bool     `json:"include_simulated,omitempty"`
	WorkloadSource        string   `json:"workload_source,omitempty"`
	WorkloadFingerprint   string   `json:"workload_fingerprint,omitempty"`
	TargetConcurrency     *float64 `json:"target_concurrency,omitempty"`
	MaxCandidates         int      `json:"max_candidates"`
	Search                string   `json:"search,omitempty"`
	ModelFeatures         []string `json:"model_features,omitempty"`
	// Prior, when set, lets the results store's measured cells order the
	// diagnosis's choices and add up to two it missed (diagnosis_prior.go).
	// It is bound by the server per call and is not part of the input.
	Prior             PriorFunc `json:"-"`
	ModelConfigDigest string    `json:"model_config_digest,omitempty"`
	// ModelTokenizers is the tokenizer digest (modelcheck's TokenizerDigest)
	// of each model a setup serves, keyed "ID@REVISION", recorded by the
	// control plane when the proposal names more than one model. A divergence
	// between two models is read only when both digests are recorded and
	// equal (qualitycheck.ErrDifferentTokenizers); the planner never reads
	// the network.
	ModelTokenizers map[string]string `json:"model_tokenizers,omitempty"`
	// MaxModelLen bounds the context of untuned baselines for models outside
	// the reviewed catalog, so the runtime does not reserve KV cache for a
	// longer context than the GPU was sized for. Reviewed profiles keep their
	// own arguments.
	MaxModelLen int `json:"max_model_len,omitempty"`
	// ModelSecretReferenceID names the workspace credential every candidate's
	// worker uses for a private model. It is a reference, never the token.
	ModelSecretReferenceID string `json:"model_secret_reference_id,omitempty"`
	// ModelAdapters is the LoRA adapter every candidate serves on the model,
	// pinned by commit or manifest digest.
	ModelAdapters []domain.ModelAdapter `json:"model_adapters,omitempty"`
	// SpeculativeDrafts names drafts for exactly this model and commit: at
	// most one trained on the customer's own traffic in their own account and
	// one published. With a diagnosis, each becomes a setup measured against
	// the starting setup, beside the model's own prediction head and no
	// speculation. The records are content-free; the files stay where the
	// customer put them.
	SpeculativeDrafts []speculator.Draft `json:"speculative_drafts,omitempty"`
	// QualityCheck chooses who decides whether a candidate's answers are still
	// good enough. QualityCheckBuiltin (the default) runs InferCrane's
	// built-in answer check against the campaign control; QualityCheckCustomer
	// waits for evidence the customer signs and attaches. Customer evidence
	// attached for a candidate wins in both modes.
	QualityCheck string `json:"quality_check,omitempty"`
	// QualityEvalSet optionally names an uploaded customer eval set by its
	// sha256 digest. It replaces the built-in prompts in the answer check.
	QualityEvalSet string `json:"quality_eval_set,omitempty"`
	// Diagnosis is the endpoint's measured bottleneck evidence. When present,
	// the planner derives candidates from the starting setup and orders them
	// by what the diagnosis says is most likely to help. The control API
	// reads it server-side; it is bound by the input digest.
	Diagnosis *Diagnosis `json:"diagnosis,omitempty"`
	// StartingSetup is what the diagnosed endpoint runs now. The control API
	// reads it with the diagnosis; it is bound by the input digest, and the
	// plan starts from it when it can.
	StartingSetup *StartingSetup `json:"starting_setup,omitempty"`
	// KernelConfigs is a tuned kernel set for this GPU and engine
	// (internal/kerneltuning). The proposal adds the starting setup with it
	// as one candidate (TechniqueTunedKernels).
	KernelConfigs *domain.KernelConfigSet `json:"kernel_configs,omitempty"`
	// Migration makes the campaign a migration Proof: the closed API behind
	// an external binding of an existing endpoint is the baseline the
	// candidates are compared with on cost, speed, and quality. The closed
	// API is never a candidate, so lineage still binds the candidates to one
	// open base model.
	Migration *MigrationBaseline `json:"migration,omitempty"`
}

// MigrationBaseline names the closed API a migration campaign replaces.
type MigrationBaseline struct {
	// Endpoint is the customer's endpoint; Binding names its external
	// binding to the closed API.
	Endpoint string `json:"endpoint"`
	Binding  string `json:"binding"`
	// Dialect is how the closed API reads a chat request:
	// MigrationDialectOpenAI or MigrationDialectOpenAICompatible.
	Dialect string `json:"dialect"`
	// MaxQualityRegressionPercent is the largest drop in graded quality, at
	// the upper bound of the paired-bootstrap interval, a candidate may show
	// against the closed API and still pass.
	MaxQualityRegressionPercent float64 `json:"max_quality_regression_percent"`
}

const (
	MigrationDialectOpenAI           = "openai"
	MigrationDialectOpenAICompatible = "openai-compatible"
	// DefaultMigrationQualityMargin is the regression margin when a request
	// names none.
	DefaultMigrationQualityMargin = 5.0
)

var migrationName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validateMigration(request Request) error {
	migration := request.Migration
	if !migrationName.MatchString(migration.Endpoint) || !migrationName.MatchString(migration.Binding) {
		return errors.New("migration.endpoint and migration.binding must name an endpoint and its external binding")
	}
	if migration.Dialect != MigrationDialectOpenAI && migration.Dialect != MigrationDialectOpenAICompatible {
		return errors.New("migration.dialect must be openai or openai-compatible")
	}
	if math.IsNaN(migration.MaxQualityRegressionPercent) || migration.MaxQualityRegressionPercent <= 0 || migration.MaxQualityRegressionPercent > 50 {
		return errors.New("migration.max_quality_regression_percent must be greater than 0 and at most 50")
	}
	if request.WorkloadSource != WorkloadSourceCustomerObserved || request.WorkloadFingerprint == "" {
		return errors.New("a migration measures the closed API's own traffic: capture it from the endpoint and pass workload_fingerprint")
	}
	if request.QualityCheck != QualityCheckCustomer && request.QualityEvalSet == "" {
		return errors.New("a migration compares answers with expected answers: name a graded quality_eval_set, or choose customer_evidence")
	}
	return nil
}

const (
	QualityCheckBuiltin  = "builtin"
	QualityCheckCustomer = "customer_evidence"
)

var qualityEvalSetDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

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
	APIVersion          string `json:"api_version" yaml:"apiVersion"`
	Kind                string `json:"kind" yaml:"kind"`
	Name                string `json:"name" yaml:"name"`
	ComputeConnectionID string `json:"compute_connection_id,omitempty" yaml:"computeConnectionID,omitempty"`
	// ModelSecretReferenceID names the workspace credential the worker uses
	// for a private model; the provider delivers it natively.
	ModelSecretReferenceID string `json:"model_secret_reference_id,omitempty" yaml:"modelSecretReferenceID,omitempty"`
	Model                  struct {
		ID       string `json:"id" yaml:"id"`
		Revision string `json:"revision" yaml:"revision"`
		// Adapters are LoRA adapters served on the model, pinned by commit
		// or manifest digest. Omitted when empty so candidate identities of
		// existing proposals hold.
		Adapters []domain.ModelAdapter `json:"adapters,omitempty" yaml:"adapters,omitempty"`
	} `json:"model" yaml:"model"`
	Runtime struct {
		Engine  string   `json:"engine" yaml:"engine"`
		Version string   `json:"version" yaml:"version"`
		Args    []string `json:"args" yaml:"args"`
		// Env holds reviewed engine switches (support.EngineEnvironment). It is
		// omitted when empty so existing candidate identities do not change.
		Env      map[string]string        `json:"env,omitempty" yaml:"env,omitempty"`
		Workload runtimecontract.Workload `json:"workload,omitzero" yaml:"workload,omitempty"`
		// KernelConfigs is the engine's own kernels tuned for this GPU and
		// workload (internal/kerneltuning). It is omitted when empty so
		// existing candidate identities do not change.
		KernelConfigs *domain.KernelConfigSet `json:"kernel_configs,omitempty" yaml:"kernel_configs,omitempty"`
	} `json:"runtime" yaml:"runtime"`
	Compute struct {
		Mode string `json:"mode" yaml:"mode"`
	} `json:"compute" yaml:"compute"`
	Resources struct {
		GPU      string `json:"gpu" yaml:"gpu"`
		GPUCount int    `json:"gpu_count" yaml:"gpu_count"`
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

// ArtifactSeed preserves the immutable publisher provenance of an optimized
// checkpoint considered by the planner. It is not measured evidence and does
// not become qualified merely because the publisher marks it ready to deploy.
type ArtifactSeed struct {
	CatalogID      string `json:"catalog_id"`
	Publisher      string `json:"publisher"`
	Tool           string `json:"tool"`
	ToolVersion    string `json:"tool_version"`
	Algorithm      string `json:"algorithm"`
	Format         string `json:"format"`
	BaseRepository string `json:"base_repository"`
	BaseRevision   string `json:"base_revision,omitempty"`
	// BaseScreeningRevision is the base revision the catalog screened a
	// publisher-declared build against (the base head when it was reviewed).
	BaseScreeningRevision    string   `json:"base_screening_revision,omitempty"`
	LineageState             string   `json:"lineage_state"`
	OutputRepository         string   `json:"output_repository"`
	OutputRevision           string   `json:"output_revision"`
	ManifestDigest           string   `json:"manifest_digest"`
	ModelCardDigest          string   `json:"model_card_digest"`
	LicenseSPDX              string   `json:"license_spdx"`
	LicenseDigest            string   `json:"license_digest,omitempty"`
	AcceleratorArchitectures []string `json:"accelerator_architectures"`
	SourceURL                string   `json:"source_url"`
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
	ArtifactSeed          *ArtifactSeed    `json:"artifact_seed,omitempty"`
	// Technique names the technique-contract technique that made this
	// setup, its version, and the artifact it serves (technique_path.go).
	Technique *CandidateTechnique `json:"technique,omitempty"`
	// Reason is set on every candidate of a diagnosis-ordered proposal.
	Reason     *CandidateReason `json:"reason,omitempty"`
	Deployment DeploymentDraft  `json:"deployment"`
}

// StartingPoint identifies the first deployable configuration selected from
// compatibility and objective fit. It is a safe baseline, not a claim that the
// candidate is fastest or cheapest for production traffic.
type StartingPoint struct {
	CandidateID   string        `json:"candidate_id"`
	EvidenceState EvidenceState `json:"evidence_state"`
	Basis         string        `json:"basis"`
	ClaimBoundary string        `json:"claim_boundary"`
}

type Proposal struct {
	SchemaVersion      string         `json:"schema_version"`
	AlgorithmVersion   string         `json:"algorithm_version"`
	Input              Request        `json:"input"`
	InputDigest        string         `json:"input_digest"`
	Candidates         []Candidate    `json:"candidates"`
	Missing            []string       `json:"missing,omitempty"`
	Warnings           []string       `json:"warnings,omitempty"`
	StartingPoint      *StartingPoint `json:"starting_point,omitempty"`
	PostDeployWorkflow []string       `json:"post_deploy_workflow,omitempty"`
	// Diagnosis is present when the input carried a diagnosis.
	Diagnosis         *DiagnosisPlan `json:"diagnosis,omitempty"`
	Mutation          string         `json:"mutation"`
	SelectionBoundary string         `json:"selection_boundary"`
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
		if len(candidate.ID) != 64 || candidate.Rank < 1 || candidate.Rank > len(proposal.Candidates) || candidate.Objective != request.Objective || candidate.BenchmarkProfile != request.WorkloadProfile || candidate.Deployment.APIVersion != "infercrane.dev/v1" || candidate.Deployment.Kind != "Deployment" || candidate.Deployment.Name == "" || candidate.Deployment.Model.ID == "" || candidate.Deployment.Model.Revision == "" || candidate.Deployment.Runtime.Engine == "" || candidate.Deployment.Runtime.Version == "" || candidate.Deployment.Resources.GPU != request.GPU || candidate.Deployment.Resources.GPUCount < 1 || candidate.Deployment.Resources.GPUCount > 1024 {
			return errors.New("proposal contains an incomplete or mismatched candidate")
		}
		if candidate.Deployment.ComputeConnectionID != request.ComputeConnectionID {
			return errors.New("proposal candidate compute connection does not match the requested boundary")
		}
		providerMatches := candidate.Deployment.Provider.Cloud == request.Provider || candidate.Deployment.Provider.Adapter == request.Provider
		if !providerMatches {
			return errors.New("proposal candidate provider does not match the requested boundary")
		}
		if request.ProviderAdapter != "" && candidate.Deployment.Provider.Adapter != request.ProviderAdapter {
			return errors.New("proposal candidate provider adapter does not match the requested compute connection")
		}
		if candidate.EvidenceState != EvidenceUnmeasured && candidate.EvidenceState != EvidenceModeled {
			return errors.New("new proposal candidates must be unmeasured or modeled")
		}
		if candidate.EvidenceState == EvidenceModeled && candidate.ModeledEvidence == nil {
			return errors.New("modeled candidate requires modeled evidence provenance")
		}
		if candidate.Technique != nil {
			if err := validateCandidateTechnique(*candidate.Technique); err != nil {
				return err
			}
		}
		if candidate.ArtifactSeed != nil {
			if err := validateArtifactSeed(*candidate.ArtifactSeed); err != nil {
				return err
			}
			if candidate.Deployment.Model.ID != candidate.ArtifactSeed.OutputRepository || candidate.Deployment.Model.Revision != candidate.ArtifactSeed.OutputRevision || candidate.EvidenceState != EvidenceUnmeasured {
				return errors.New("publisher artifact candidate must deploy the exact unmeasured output identity")
			}
		}
		if err := support.ValidateRuntimeEnv(candidate.Deployment.Runtime.Engine, candidate.Deployment.Runtime.Version, candidate.Deployment.Resources.GPU, candidate.Deployment.Runtime.Workload, candidate.Deployment.Runtime.Env); err != nil {
			return fmt.Errorf("proposal candidate runtime env: %w", err)
		}
		if err := kerneltuning.ValidateForDeployment(candidate.Deployment.Runtime.KernelConfigs, candidate.Deployment.Runtime.Engine, candidate.Deployment.Runtime.Version, candidate.Deployment.Resources.GPU, candidate.Deployment.Runtime.Workload.Empty()); err != nil {
			return fmt.Errorf("proposal candidate kernel configs: %w", err)
		}
		if candidate.Reason != nil {
			if err := validateCandidateReason(*candidate.Reason); err != nil {
				return err
			}
		}
		if _, duplicate := seenIDs[candidate.ID]; duplicate {
			return errors.New("proposal candidate IDs must be unique")
		}
		if _, duplicate := seenRanks[candidate.Rank]; duplicate {
			return errors.New("proposal candidate ranks must be unique")
		}
		seenIDs[candidate.ID], seenRanks[candidate.Rank] = struct{}{}, struct{}{}
	}
	if proposal.StartingPoint != nil {
		if _, found := seenIDs[proposal.StartingPoint.CandidateID]; !found || proposal.StartingPoint.EvidenceState != EvidenceUnmeasured && proposal.StartingPoint.EvidenceState != EvidenceModeled || proposal.StartingPoint.Basis == "" || proposal.StartingPoint.ClaimBoundary == "" {
			return errors.New("proposal starting point is incomplete or does not reference a candidate")
		}
	}
	// A campaign measures every candidate against one control, so the
	// candidates must serve one base model or artifacts built from it.
	if _, err := ProposalLineage(proposal); err != nil {
		return err
	}
	return nil
}

type CatalogSource struct {
	Recipes      []curatedrecipe.Entry
	Integrations integration.Snapshot
	// Checkpoints is the reviewed, committed publisher checkpoint catalog. It
	// is read from the binary; proposals never fetch upstream data.
	Checkpoints checkpointcatalog.Catalog
}

func NewCatalogSource(recipes []curatedrecipe.Entry, integrations integration.Snapshot) CatalogSource {
	return CatalogSource{Recipes: append([]curatedrecipe.Entry(nil), recipes...), Integrations: integrations, Checkpoints: checkpointcatalog.MustDefault()}
}

func (s CatalogSource) Propose(_ context.Context, request Request) (Proposal, error) {
	request = normalizeRequest(request)
	if err := validateRequest(request); err != nil {
		return Proposal{}, err
	}
	if err := ValidateMeasurable(request); err != nil {
		return Proposal{}, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return Proposal{}, err
	}
	sum := sha256.Sum256(input)
	proposal := Proposal{SchemaVersion: SchemaVersion, AlgorithmVersion: AlgorithmVersion, Input: request, InputDigest: hex.EncodeToString(sum[:]), Mutation: "none", SelectionBoundary: "configuration candidates only; benchmark, quality, cost, and Release Guard evidence are required before qualification"}
	applyWorkloadBoundary(&proposal)
	applyObjectiveMetric(&proposal)
	if request.Diagnosis != nil {
		// Plan every candidate first so the diagnosis ranks the whole set
		// before the requested bound cuts it.
		proposal.Input.MaxCandidates = maxProposalCandidates
	}
	proposal, err = s.proposeReviewed(proposal)
	if err == nil && searchesSetups(request) {
		proposal, err = s.expandRecipeSearch(proposal)
	}
	if err != nil {
		return proposal, err
	}
	if request.Diagnosis != nil {
		proposal.Input = request
		if proposal, err = applyDiagnosis(proposal); err != nil {
			return proposal, err
		}
	}
	return applyUnruledTechniques(proposal)
}

const (
	minMaxModelLen            = 256
	maxMaxModelLen            = 1 << 20
	maxProposalCandidates     = 100
	genericBaselineLimitation = "This baseline has no model-specific runtime arguments or kernel claims."
)

func (s CatalogSource) proposeReviewed(proposal Proposal) (Proposal, error) {
	var err error
	request := proposal.Input
	entry, ok := findRecipe(s.Recipes, request.ModelIdentity)
	if ok && request.ModelRevision != "" && request.ModelRevision != entry.Revision {
		ok = false
	}
	if !ok {
		return s.proposeGeneric(proposal)
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
			version := profile.RuntimeVersion
			if version == "" {
				version = runtimeVersion(s.Integrations.Runtimes, profile.Runtime)
			}
			candidate, compileErr := buildCandidate(entry, profile, compatible, version, request, preferredProfile)
			if compileErr != nil {
				return Proposal{}, fmt.Errorf("compile reviewed profile %s: %w", profile.Name, compileErr)
			}
			proposal.Candidates = append(proposal.Candidates, candidate)
		}
	}
	proposal.Candidates, err = s.expandCheckpointCandidates(proposal.Candidates, request)
	if err != nil {
		return Proposal{}, err
	}
	sort.Slice(proposal.Candidates, func(i, j int) bool {
		a, b := proposal.Candidates[i], proposal.Candidates[j]
		if configurationProfileRank(a.ConfigurationProfile, preferredProfile) != configurationProfileRank(b.ConfigurationProfile, preferredProfile) {
			return configurationProfileRank(a.ConfigurationProfile, preferredProfile) < configurationProfileRank(b.ConfigurationProfile, preferredProfile)
		}
		if qualificationRank(a.CompatibilityState) != qualificationRank(b.CompatibilityState) {
			return qualificationRank(a.CompatibilityState) < qualificationRank(b.CompatibilityState)
		}
		if adapterRank(a, request) != adapterRank(b, request) {
			return adapterRank(a, request) < adapterRank(b, request)
		}
		if artifactRank(a) != artifactRank(b) {
			return artifactRank(a) < artifactRank(b)
		}
		if a.Deployment.Provider.Adapter != b.Deployment.Provider.Adapter {
			return a.Deployment.Provider.Adapter < b.Deployment.Provider.Adapter
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
	} else {
		attachStartingPoint(&proposal)
	}
	return proposal, nil
}

func configurationProfileRank(name, preferred string) int {
	for _, tool := range checkpointcatalog.QuantizationTools() {
		if index := strings.Index(name, "-"+tool+"-"); index > 0 {
			name = name[:index]
		}
	}
	if name == preferred || strings.HasSuffix(name, "-"+preferred) {
		return 0
	}
	if name == "balanced" || strings.HasSuffix(name, "-balanced") {
		return 1
	}
	return 2
}

func artifactRank(candidate Candidate) int {
	if candidate.ArtifactSeed == nil {
		return 0
	}
	return 1
}

func normalizeRequest(request Request) Request {
	request.ModelIdentity = strings.TrimSpace(request.ModelIdentity)
	request.ModelRevision = strings.ToLower(strings.TrimSpace(request.ModelRevision))
	if repository, revision, found := splitPinnedModel(request.ModelIdentity); found && request.ModelRevision == "" {
		request.ModelIdentity = repository + "@" + revision
		request.ModelRevision = revision
	}
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	request.ComputeConnectionID = strings.TrimSpace(request.ComputeConnectionID)
	request.ModelSecretReferenceID = strings.TrimSpace(request.ModelSecretReferenceID)
	request.ProviderAdapter = strings.ToLower(strings.TrimSpace(request.ProviderAdapter))
	request.Region = strings.TrimSpace(request.Region)
	request.GPU = strings.TrimSpace(request.GPU)
	request.Search = strings.ToLower(strings.TrimSpace(request.Search))
	request.ModelConfigDigest = strings.ToLower(strings.TrimSpace(request.ModelConfigDigest))
	request.ModelFeatures = normalizedFeatures(request.ModelFeatures)
	request.Objective = strings.ToLower(strings.TrimSpace(request.Objective))
	if request.Objective == "" {
		request.Objective = "interactive"
	}
	request.WorkloadProfile = strings.ToLower(strings.TrimSpace(request.WorkloadProfile))
	request.WorkloadSource = strings.ToLower(strings.TrimSpace(request.WorkloadSource))
	request.WorkloadFingerprint = strings.TrimSpace(request.WorkloadFingerprint)
	request.QualityCheck = strings.ToLower(strings.TrimSpace(request.QualityCheck))
	request.QualityEvalSet = strings.ToLower(strings.TrimSpace(request.QualityEvalSet))
	request.SpeculativeDrafts = normalizeDrafts(request.SpeculativeDrafts)
	if request.Diagnosis != nil {
		diagnosis := normalizeDiagnosis(*request.Diagnosis)
		request.Diagnosis = &diagnosis
	}
	if request.Migration != nil {
		migration := *request.Migration
		migration.Endpoint = strings.TrimSpace(migration.Endpoint)
		migration.Binding = strings.TrimSpace(migration.Binding)
		migration.Dialect = strings.ToLower(strings.TrimSpace(migration.Dialect))
		if migration.Dialect == "" {
			migration.Dialect = MigrationDialectOpenAICompatible
		}
		if migration.MaxQualityRegressionPercent == 0 {
			migration.MaxQualityRegressionPercent = DefaultMigrationQualityMargin
		}
		request.Migration = &migration
	}
	if request.QualityCheck == QualityCheckBuiltin {
		// The default is stored as empty so existing proposal digests stay stable.
		request.QualityCheck = ""
	}
	if request.WorkloadProfile == "" {
		request.WorkloadProfile = objectives[request.Objective]
	}
	if performanceprofile.IsPublicPrior(request.WorkloadProfile) {
		if request.WorkloadSource == "" {
			request.WorkloadSource = WorkloadSourcePublicPrior
		}
		if request.WorkloadFingerprint == "" {
			request.WorkloadFingerprint = workloadprofile.PublicPriorDigest
		}
	} else if request.WorkloadSource == "" {
		if request.WorkloadFingerprint == "" {
			request.WorkloadSource = WorkloadSourceReviewedDefault
		} else {
			request.WorkloadSource = WorkloadSourceCustomerObserved
		}
	}
	if request.MaxCandidates == 0 {
		request.MaxCandidates = 10
	}
	if request.GPUCount == 0 {
		request.GPUCount = 1
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
	if len(request.ModelSecretReferenceID) > 255 {
		return errors.New("model_secret_reference_id must not exceed 255 characters")
	}
	if err := validateKernelConfigs(request); err != nil {
		return err
	}
	if err := modelimport.ValidateDeployment(request.ModelIdentity, request.ModelRevision, "", "elastic", request.ModelAdapters, nil); err != nil {
		return err
	}
	if err := validateSearch(request); err != nil {
		return err
	}
	if err := validateModelTokenizers(request.ModelTokenizers); err != nil {
		return err
	}
	if err := validateDrafts(request); err != nil {
		return err
	}
	if _, ok := objectives[request.Objective]; !ok {
		return errors.New("objective must be interactive, latency, throughput, cost-efficiency, or useful-output")
	}
	if _, err := performanceprofile.Get(request.WorkloadProfile); err != nil {
		return err
	}
	switch request.WorkloadSource {
	case WorkloadSourceReviewedDefault:
		if performanceprofile.IsPublicPrior(request.WorkloadProfile) {
			return errors.New("public workload profiles require workload_source public_prior")
		}
	case WorkloadSourcePublicPrior:
		if !performanceprofile.IsPublicPrior(request.WorkloadProfile) {
			return errors.New("workload_source public_prior requires a public workload profile")
		}
		if request.WorkloadFingerprint != workloadprofile.PublicPriorDigest {
			return errors.New("public workload prior fingerprint does not match the product evidence artifact")
		}
	case WorkloadSourceCustomerObserved:
		if performanceprofile.IsPublicPrior(request.WorkloadProfile) {
			return errors.New("customer_observed workloads cannot use a public workload profile")
		}
		if request.WorkloadFingerprint == "" {
			return errors.New("customer_observed workloads require a workload fingerprint")
		}
	default:
		return errors.New("workload_source must be reviewed_default, public_prior, or customer_observed")
	}
	if request.MaxCandidates < 1 || request.MaxCandidates > maxProposalCandidates {
		return errors.New("max candidates must be between 1 and 100")
	}
	if request.GPUCount < 1 || request.GPUCount > 1024 {
		return errors.New("GPU count must be between 1 and 1024")
	}
	if _, embeddedRevision, found := splitPinnedModel(request.ModelIdentity); found && embeddedRevision != request.ModelRevision {
		return errors.New("model identity revision and model_revision must match")
	}
	if request.ModelRevision != "" && !immutableModelRevision(request.ModelRevision) && !(modelimport.IsObjectModel(request.ModelIdentity) && modelimport.ValidDigest(request.ModelRevision)) {
		return errors.New("model revision must be an immutable 40 to 64 character hexadecimal commit")
	}
	if len(request.WorkloadFingerprint) > 256 {
		return errors.New("workload fingerprint must be at most 256 characters")
	}
	if len(request.ComputeConnectionID) > 255 {
		return errors.New("compute connection ID must be at most 255 characters")
	}
	if len(request.ProviderAdapter) > 255 {
		return errors.New("provider adapter must be at most 255 characters")
	}
	if request.ProviderAdapter != "" && request.ComputeConnectionID == "" {
		return errors.New("provider adapter requires a compute connection boundary")
	}
	if request.TargetConcurrency != nil && (*request.TargetConcurrency <= 0 || math.IsNaN(*request.TargetConcurrency) || math.IsInf(*request.TargetConcurrency, 0)) {
		return errors.New("target concurrency must be a finite positive value")
	}
	if (request.Provider == "aws" || request.Provider == "aws-ec2" || request.Provider == "gcp" || request.Provider == "gcp-compute") && request.Region == "" {
		return errors.New("AWS and GCP candidates require an explicit region")
	}
	for name, value := range map[string]*float64{"max_ttft_p95_ms": request.MaxTTFTP95MS, "max_tpot_p95_ms": request.MaxTPOTP95MS, "max_error_rate": request.MaxErrorRate, "min_goodput": request.MinGoodput, "min_output_tokens_second": request.MinOutputTokensSecond, "max_hourly_cost": request.MaxHourlyCost} {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return fmt.Errorf("%s must be a finite nonnegative value", name)
		}
	}
	if request.MaxErrorRate != nil && *request.MaxErrorRate > 1 {
		return errors.New("max_error_rate must be between zero and one")
	}
	if request.QualityCheck != "" && request.QualityCheck != QualityCheckCustomer {
		return errors.New("quality_check must be builtin or customer_evidence")
	}
	if request.MaxModelLen != 0 && (request.MaxModelLen < minMaxModelLen || request.MaxModelLen > maxMaxModelLen) {
		return fmt.Errorf("max_model_len must be between %d and %d", minMaxModelLen, maxMaxModelLen)
	}
	if request.QualityEvalSet != "" && !qualityEvalSetDigest.MatchString(request.QualityEvalSet) {
		return errors.New("quality_eval_set must be sha256:<64 lowercase hex characters>")
	}
	if request.QualityEvalSet != "" && request.QualityCheck == QualityCheckCustomer {
		return errors.New("quality_eval_set applies to the built-in answer check, not to customer_evidence")
	}
	if request.Diagnosis != nil {
		if err := validateDiagnosis(*request.Diagnosis); err != nil {
			return err
		}
	}
	if request.StartingSetup != nil {
		if request.Diagnosis == nil {
			return errors.New("a starting setup comes with the endpoint's diagnosis")
		}
		if err := validateStartingSetup(*request.StartingSetup); err != nil {
			return err
		}
	}
	if request.Migration != nil {
		if err := validateMigration(request); err != nil {
			return err
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

func splitPinnedModel(identity string) (string, string, bool) {
	index := strings.LastIndex(strings.TrimSpace(identity), "@")
	if index <= 0 || index == len(identity)-1 {
		return strings.TrimSpace(identity), "", false
	}
	return strings.TrimSpace(identity[:index]), strings.ToLower(strings.TrimSpace(identity[index+1:])), true
}

func immutableModelRevision(revision string) bool {
	if len(revision) < 40 || len(revision) > 64 {
		return false
	}
	_, err := hex.DecodeString(revision)
	return err == nil
}

// proposeGeneric creates deliberately untuned starting points for any pinned
// open-weight model. It reuses only executable provider/runtime compatibility;
// model fit, licensing, quality, and performance remain explicit evidence gaps.
func (s CatalogSource) proposeGeneric(proposal Proposal) (Proposal, error) {
	request := proposal.Input
	model, embeddedRevision, _ := splitPinnedModel(request.ModelIdentity)
	revision := request.ModelRevision
	if revision == "" {
		revision = embeddedRevision
	}
	if revision == "" {
		proposal.Missing = []string{"immutable_model_revision"}
		proposal.Warnings = append(proposal.Warnings, "Uncataloged models require an exact 40 to 64 character hexadecimal commit before InferCrane can create a candidate.")
		return proposal, nil
	}
	if model == request.ModelIdentity {
		model = strings.TrimSpace(request.ModelIdentity)
	}
	if entry, found := findRecipe(s.Recipes, model); found {
		model = entry.Model
	}
	compatibility := matchingCompatibility(s.Integrations.Compatibility, request)
	if len(compatibility) == 0 {
		proposal.Missing = []string{"qualified_provider_runtime_compatibility"}
		return proposal, nil
	}
	for _, compatible := range compatibility {
		if compatible.Runtime != "vllm" && compatible.Runtime != "sglang" {
			continue
		}
		version := runtimeVersion(s.Integrations.Runtimes, compatible.Runtime)
		if version == "" {
			continue
		}
		candidate := buildGenericCandidate(model, revision, compatible, version, request)
		proposal.Candidates = append(proposal.Candidates, candidate)
	}
	var err error
	proposal.Candidates, err = s.expandCheckpointCandidates(proposal.Candidates, request)
	if err != nil {
		return Proposal{}, err
	}
	sort.Slice(proposal.Candidates, func(i, j int) bool {
		if qualificationRank(proposal.Candidates[i].CompatibilityState) != qualificationRank(proposal.Candidates[j].CompatibilityState) {
			return qualificationRank(proposal.Candidates[i].CompatibilityState) < qualificationRank(proposal.Candidates[j].CompatibilityState)
		}
		if adapterRank(proposal.Candidates[i], request) != adapterRank(proposal.Candidates[j], request) {
			return adapterRank(proposal.Candidates[i], request) < adapterRank(proposal.Candidates[j], request)
		}
		if artifactRank(proposal.Candidates[i]) != artifactRank(proposal.Candidates[j]) {
			return artifactRank(proposal.Candidates[i]) < artifactRank(proposal.Candidates[j])
		}
		if proposal.Candidates[i].Deployment.Provider.Adapter != proposal.Candidates[j].Deployment.Provider.Adapter {
			return proposal.Candidates[i].Deployment.Provider.Adapter < proposal.Candidates[j].Deployment.Provider.Adapter
		}
		return proposal.Candidates[i].ID < proposal.Candidates[j].ID
	})
	if len(proposal.Candidates) > request.MaxCandidates {
		proposal.Candidates = proposal.Candidates[:request.MaxCandidates]
	}
	for index := range proposal.Candidates {
		proposal.Candidates[index].Rank = index + 1
	}
	if len(proposal.Candidates) == 0 {
		proposal.Missing = []string{"generic_open_weight_runtime"}
		return proposal, nil
	}
	attachStartingPoint(&proposal)
	proposal.Warnings = append(proposal.Warnings, "Generic candidates are safe, untuned baselines. InferCrane has not inferred model architecture, GPU memory fit, license, tokenizer behavior, runtime compatibility, or quality.")
	return proposal, nil
}

func attachStartingPoint(proposal *Proposal) {
	if proposal == nil || len(proposal.Candidates) == 0 {
		return
	}
	first := proposal.Candidates[0]
	proposal.StartingPoint = &StartingPoint{
		CandidateID: first.ID, EvidenceState: first.EvidenceState,
		Basis:         "highest-ranked compatible configuration for the declared objective and reviewed capability boundary",
		ClaimBoundary: "a deployment starting point only; production performance, quality, cost, and capacity remain unqualified until measured",
	}
	firstStep := "observe representative traffic"
	if proposal.Input.WorkloadSource == WorkloadSourcePublicPrior {
		firstStep = "replace the public prior with representative customer replay"
	}
	proposal.PostDeployWorkflow = []string{firstStep, "assess serving-stage bottlenecks", "profile the exact runtime and target GPU", "search existing implementations before custom code", "qualify candidates with workload, quality, cost, and Release Guard evidence"}
}

// applyModelInputs gives a candidate the request's model credential and LoRA
// adapter, so every setup serves what the customer serves.
func applyModelInputs(draft *DeploymentDraft, request Request) {
	draft.ModelSecretReferenceID = request.ModelSecretReferenceID
	draft.Model.Adapters = append([]domain.ModelAdapter(nil), request.ModelAdapters...)
}

// ServedModel is the model name a candidate answers to: its LoRA adapter when
// it serves one, otherwise the model.
func (d DeploymentDraft) ServedModel() string {
	if len(d.Model.Adapters) > 0 {
		return d.Model.Adapters[0].Name
	}
	return d.Model.ID
}

func (s CatalogSource) expandCheckpointCandidates(baselines []Candidate, request Request) ([]Candidate, error) {
	// A LoRA adapter is trained on one base checkpoint; serving it on another
	// would change what the customer's endpoint answers.
	if len(s.Checkpoints.Candidates()) == 0 || len(request.ModelAdapters) > 0 {
		return baselines, nil
	}
	out := append([]Candidate(nil), baselines...)
	seen := map[string]struct{}{}
	for _, candidate := range out {
		seen[candidate.ID] = struct{}{}
	}
	for _, baseline := range baselines {
		matches := s.Checkpoints.Match(baseline.Deployment.Model.ID, baseline.Deployment.Model.Revision, baseline.Deployment.Runtime.Engine, baseline.Deployment.Runtime.Version, request.GPU)
		for _, entry := range matches {
			derived, err := checkpointCandidate(baseline, entry, s.Checkpoints.PublisherName(entry.Publisher), request)
			if err != nil {
				return nil, err
			}
			if _, duplicate := seen[derived.ID]; !duplicate {
				out = append(out, derived)
				seen[derived.ID] = struct{}{}
			}
		}
	}
	model, revision := request.ModelIdentity, request.ModelRevision
	if embeddedModel, embeddedRevision, found := splitPinnedModel(model); found {
		model = embeddedModel
		if revision == "" {
			revision = embeddedRevision
		}
	}
	// A pinned revision is the base model every candidate serves. The
	// reviewed recipe's revision applies only when the request pins none, as
	// in proposeReviewed; otherwise the proposal would mix two base revisions.
	if entry, found := findRecipe(s.Recipes, model); found {
		model = entry.Model
		if revision == "" {
			revision = entry.Revision
		}
	}
	for _, compatible := range matchingCompatibility(s.Integrations.Compatibility, request) {
		if compatible.Runtime != "vllm" && compatible.Runtime != "sglang" {
			continue
		}
		version := runtimeVersion(s.Integrations.Runtimes, compatible.Runtime)
		if version == "" {
			continue
		}
		matches := s.Checkpoints.Match(model, revision, compatible.Runtime, version, request.GPU)
		if len(matches) == 0 {
			continue
		}
		baseline := buildGenericCandidate(model, revision, compatible, version, request)
		if _, duplicate := seen[baseline.ID]; !duplicate {
			out = append(out, baseline)
			seen[baseline.ID] = struct{}{}
		}
		for _, entry := range matches {
			derived, err := checkpointCandidate(baseline, entry, s.Checkpoints.PublisherName(entry.Publisher), request)
			if err != nil {
				return nil, err
			}
			if _, duplicate := seen[derived.ID]; !duplicate {
				out = append(out, derived)
				seen[derived.ID] = struct{}{}
			}
		}
	}
	return out, nil
}

var checkpointUpstreams = map[string]string{"modelopt": "NVIDIA/Model-Optimizer", "llm-compressor": "vllm-project/llm-compressor"}

func checkpointCandidate(baseline Candidate, entry checkpointcatalog.Entry, publisher string, request Request) (Candidate, error) {
	encoded, err := json.Marshal(baseline)
	if err != nil {
		return Candidate{}, err
	}
	var candidate Candidate
	if err = json.Unmarshal(encoded, &candidate); err != nil {
		return Candidate{}, err
	}
	quantization := entry.Quantization
	if quantization == nil {
		return Candidate{}, fmt.Errorf("checkpoint %s is not a quantized checkpoint", entry.ID)
	}
	artifact := ArtifactSeed{
		CatalogID: entry.ID, Publisher: publisher, Tool: quantization.Tool,
		ToolVersion: quantization.ToolVersion, Algorithm: quantization.Scheme, Format: entry.Artifact.Format,
		BaseRepository: entry.Base.Repository, BaseRevision: entry.Base.DeclaredRevision, BaseScreeningRevision: entry.Base.ScreeningRevision,
		LineageState: entry.Base.Lineage, OutputRepository: entry.Repository,
		OutputRevision: entry.Revision, ManifestDigest: entry.Artifact.ManifestDigest,
		ModelCardDigest: entry.Artifact.ModelCardDigest, LicenseSPDX: entry.License.SPDX,
		LicenseDigest:            entry.License.Digest,
		AcceleratorArchitectures: append([]string(nil), entry.Hardware.Architectures...),
		SourceURL:                entry.SourceURL,
	}
	candidate.ID = ""
	candidate.Rank = 0
	candidate.Status = "proposed-unmeasured"
	candidate.EvidenceState = EvidenceUnmeasured
	candidate.ModeledEvidence = nil
	candidate.ArtifactSeed = &artifact
	candidate.ConfigurationProfile += "-" + quantization.Tool + "-" + strings.ToLower(quantization.Scheme)
	candidate.Source = SourceInfo{
		Name: "publisher-checkpoint-catalog", Version: entry.Revision,
		EvidenceClass: "publisher-provided-unmeasured", UpstreamProject: checkpointUpstreams[quantization.Tool],
	}
	candidate.Deployment.Name = safeName(candidate.Deployment.Name + "-" + quantization.Scheme)
	candidate.Deployment.Model.ID = entry.Repository
	candidate.Deployment.Model.Revision = entry.Revision
	candidate.Features = append(candidate.Features,
		Feature{Name: "optimized_checkpoint", State: "publisher-provided", Source: entry.Artifact.ManifestDigest},
		Feature{Name: "quantization", State: quantization.Scheme, Source: quantization.Tool + "@" + quantization.ToolVersion},
		Feature{Name: "artifact_lineage", State: entry.Base.Lineage, Source: entry.SourceURL},
	)
	candidate.RequiredEvidence = append(candidate.RequiredEvidence,
		"artifact manifest verification before download",
		"runtime load and exact served-model identity for the pinned checkpoint",
		"paired semantic quality comparison against the exact source checkpoint",
		"workload latency, throughput, memory, and cost comparison against the unmodified baseline",
		"license review for the model and every builder/runtime dependency",
	)
	candidate.Limitations = append(candidate.Limitations, entry.Limitations...)
	candidate.Limitations = append(candidate.Limitations,
		"Publisher benchmark or model-card results are discovery evidence only and cannot satisfy InferCrane qualification gates.",
		fmt.Sprintf("This checkpoint is restricted to the publisher-declared accelerator architectures (%s).", strings.Join(entry.Hardware.Architectures, ", ")),
	)
	identity, err := json.Marshal(struct {
		Source     SourceInfo      `json:"source"`
		Deployment DeploymentDraft `json:"deployment"`
		Profile    string          `json:"profile"`
		Artifact   ArtifactSeed    `json:"artifact"`
		Workload   string          `json:"workload"`
	}{candidate.Source, candidate.Deployment, candidate.ConfigurationProfile, artifact, request.WorkloadProfile})
	if err != nil {
		return Candidate{}, err
	}
	sum := sha256.Sum256(identity)
	candidate.ID = hex.EncodeToString(sum[:])
	return candidate, nil
}

func validateArtifactSeed(seed ArtifactSeed) error {
	if seed.CatalogID == "" || seed.Publisher == "" || !slices.Contains(checkpointcatalog.QuantizationTools(), seed.Tool) || seed.ToolVersion == "" || seed.Algorithm == "" || seed.BaseRepository == "" || seed.OutputRepository == "" || seed.LicenseSPDX == "" || len(seed.AcceleratorArchitectures) == 0 || !strings.HasPrefix(seed.SourceURL, "https://huggingface.co/") {
		return errors.New("publisher artifact candidate has incomplete provenance")
	}
	if seed.LineageState != LineageExact && seed.LineageState != LineagePublisherDeclared {
		return errors.New("publisher artifact candidate has an unsupported lineage state")
	}
	if seed.LineageState == LineageExact && !immutableModelRevision(seed.BaseRevision) {
		return errors.New("publisher artifact exact lineage requires an immutable base revision")
	}
	if seed.BaseScreeningRevision != "" && !immutableModelRevision(seed.BaseScreeningRevision) {
		return errors.New("publisher artifact screening revision must be immutable")
	}
	if !immutableModelRevision(seed.OutputRevision) || !sha256Digest(seed.ManifestDigest) || !sha256Digest(seed.ModelCardDigest) || seed.LicenseDigest != "" && !sha256Digest(seed.LicenseDigest) {
		return errors.New("publisher artifact candidate requires immutable output and manifest identity")
	}
	return nil
}

func sha256Digest(value string) bool {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func applyWorkloadBoundary(proposal *Proposal) {
	if proposal == nil || proposal.Input.WorkloadSource != WorkloadSourcePublicPrior {
		return
	}
	proposal.Warnings = append(proposal.Warnings, "The public workload prior is day-zero screening evidence. Replace it with representative customer replay before qualification or promotion.")
	proposal.SelectionBoundary += "; public workload priors cannot qualify or promote a production candidate"
}

// applyObjectiveMetric warns when the objective ranks on another metric than
// its usual one, so the substitution is visible before anything is measured.
func applyObjectiveMetric(proposal *Proposal) {
	metric, ok := lab.ObjectiveMetric(proposal.Input.Objective, proposal.Input.WorkloadProfile)
	if !ok || metric.Note == "" {
		return
	}
	proposal.Warnings = append(proposal.Warnings, fmt.Sprintf("Objective %s ranks setups on %s on the %s profile: %s.", proposal.Input.Objective, metric.Label, proposal.Input.WorkloadProfile, metric.Note))
}

// ValidateMeasurable rejects a limit the request's workload profile can never
// measure. A non-streaming (buffered) run receives each answer whole, so it
// measures neither time to first token nor time per output token, and a TTFT
// or TPOT limit would reject every setup only after the run spent its whole
// measurement time. It is checked when a proposal is made and when a run is
// created, not when a stored run is ranked.
func ValidateMeasurable(request Request) error {
	request = normalizeRequest(request)
	profile, err := performanceprofile.Get(request.WorkloadProfile)
	if err != nil || profile.Streaming {
		return nil
	}
	var limits []string
	if request.MaxTTFTP95MS != nil {
		limits = append(limits, "max_ttft_p95_ms")
	}
	if request.MaxTPOTP95MS != nil {
		limits = append(limits, "max_tpot_p95_ms")
	}
	if len(limits) == 0 {
		return nil
	}
	return fmt.Errorf("%s cannot be measured on the %s profile: it receives each answer whole, so no run measures time to first token or time per output token and every setup would miss the limit; use a streaming profile such as interactive, or drop the limit (latency objectives on %s rank on p95 request latency)", strings.Join(limits, " and "), profile.Name, profile.Name)
}

func requireCustomerReplay(request Request, required, limitations []string) ([]string, []string) {
	if request.WorkloadSource != WorkloadSourcePublicPrior {
		return required, limitations
	}
	required = append(required, "representative customer replay replacing the public workload prior")
	limitations = append(limitations, "The public workload prior is a screening input and cannot qualify this candidate for production.")
	return required, limitations
}

func buildGenericCandidate(model, revision string, compatible integration.RuntimeCompatibility, runtimeVersion string, request Request) Candidate {
	draft := DeploymentDraft{APIVersion: "infercrane.dev/v1", Kind: "Deployment"}
	draft.Name = safeName(model + "-" + compatible.Runtime + "-baseline-" + compatible.Adapter)
	draft.ComputeConnectionID = request.ComputeConnectionID
	draft.Model.ID, draft.Model.Revision = model, revision
	applyModelInputs(&draft, request)
	draft.Runtime.Engine, draft.Runtime.Version = compatible.Runtime, runtimeVersion
	draft.Runtime.Args = genericRuntimeArgs(compatible, request)
	draft.Compute.Mode = string(compatible.Mode)
	draft.Resources.GPU, draft.Resources.GPUCount = request.GPU, request.GPUCount
	draft.Provider.Cloud, draft.Provider.Adapter, draft.Provider.Region = compatible.Cloud, compatible.Adapter, request.Region
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, 1
	draft.Routing.Strategy = "round-robin"
	if compatible.Adapter == "kubernetes-dynamo" {
		draft.Serving = servingcontract.Topology{Backend: servingcontract.BackendDynamo, Profile: "baseline", Mode: servingcontract.ModeAggregated, Routing: servingcontract.RoutingDirect, Worker: servingcontract.Pool{Replicas: 1, TensorParallelism: request.GPUCount}}
	}
	source := SourceInfo{Name: "infercrane-generic-open-weight", Version: "generic-open-weight-v1", EvidenceClass: "configuration-unverified"}
	features := []Feature{{Name: "runtime_baseline", State: "untuned", Source: compatible.Evidence}, {Name: "immutable_model_revision", State: "pinned", Source: revision}}
	if compatible.Adapter == "kubernetes-dynamo" {
		features = append(features, Feature{Name: "dynamo_graph", State: "single-mutation-owner", Source: compatible.Evidence})
	}
	required := []string{"runtime readiness and exact served-model identity", "model architecture and accelerator memory fit", "license and access review", "AIPerf " + request.WorkloadProfile + " workload", "semantic quality evidence", "fresh provider capacity observation"}
	if request.MaxHourlyCost != nil || request.Objective == "cost-efficiency" {
		required = append(required, "sourced hourly cost")
	}
	if request.MaxTTFTP95MS != nil || request.MaxTPOTP95MS != nil || request.MaxErrorRate != nil || request.MinGoodput != nil || request.MinOutputTokensSecond != nil {
		required = append(required, "measured SLO metrics")
	}
	limitations := []string{
		genericBaselineLimitation,
		"The exact model, revision, tokenizer, runtime, GPU count, and workload must pass real qualification before production use.",
		"GPU memory fit and model license/access were supplied by the operator and have not been inferred.",
	}
	required, limitations = requireCustomerReplay(request, required, limitations)
	identity, _ := json.Marshal(struct {
		Source        SourceInfo                       `json:"source"`
		Deployment    DeploymentDraft                  `json:"deployment"`
		Profile       string                           `json:"profile"`
		Compatibility integration.RuntimeCompatibility `json:"compatibility"`
	}{source, draft, request.WorkloadProfile, compatible})
	sum := sha256.Sum256(identity)
	return Candidate{ID: hex.EncodeToString(sum[:]), Status: "proposed-unmeasured", EvidenceState: EvidenceUnmeasured, Objective: request.Objective, ConfigurationProfile: compatible.Runtime + "-generic-baseline", BenchmarkProfile: request.WorkloadProfile, CompatibilityState: string(compatible.State), CompatibilityEvidence: compatible.Evidence, Source: source, Features: features, RequiredEvidence: required, Limitations: limitations, Deployment: draft}
}

// genericRuntimeArgs are the only arguments an untuned baseline carries: the
// context bound the caller sized the GPU for, and tensor parallelism across
// the replica's GPUs. A Dynamo graph sets tensor parallelism itself.
func genericRuntimeArgs(compatible integration.RuntimeCompatibility, request Request) []string {
	args := []string{}
	sglang := compatible.Runtime == "sglang"
	if request.MaxModelLen > 0 {
		flag := "--max-model-len"
		if sglang {
			flag = "--context-length"
		}
		args = append(args, flag, strconv.Itoa(request.MaxModelLen))
	}
	if request.GPUCount > 1 && compatible.Adapter != "kubernetes-dynamo" {
		flag := "--tensor-parallel-size"
		if sglang {
			flag = "--tp-size"
		}
		args = append(args, flag, strconv.Itoa(request.GPUCount))
	}
	return args
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
		if request.ProviderAdapter != "" && request.ProviderAdapter != entry.Adapter {
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
		if len(entry.CompatibleGPUs) > 0 && !contains(entry.CompatibleGPUs, request.GPU) {
			continue
		}
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
	draft.ComputeConnectionID = request.ComputeConnectionID
	draft.Model.ID, draft.Model.Revision = entry.Model, entry.Revision
	applyModelInputs(&draft, request)
	draft.Runtime.Engine, draft.Runtime.Version = profile.Runtime, runtimeVersion
	draft.Runtime.Workload = profile.Workload
	draft.Compute.Mode = profile.ComputeMode
	draft.Resources.GPU = request.GPU
	draft.Resources.GPUCount = profile.GPUCount
	if draft.Resources.GPUCount == 0 {
		draft.Resources.GPUCount = 1
	}
	draft.Provider.Cloud, draft.Provider.Adapter, draft.Provider.Region = compatible.Cloud, compatible.Adapter, request.Region
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = profile.MinReplicas, profile.MaxReplicas
	draft.Routing.Strategy = "round-robin"
	if compatible.Adapter == "kubernetes-dynamo" {
		// InferCrane owns exactly one DGD parent. Dynamo owns the worker graph;
		// an outer replica autoscaler would create two mutation owners.
		draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, 1
		draft.Serving = servingcontract.Topology{
			Backend: servingcontract.BackendDynamo,
			Profile: "baseline",
			Mode:    servingcontract.ModeAggregated,
			Routing: servingcontract.RoutingDirect,
			Worker:  servingcontract.Pool{Replicas: 1, TensorParallelism: 1},
		}
	}
	features := []Feature{}
	base := optimizationcapability.Request{Runtime: profile.Runtime, RuntimeVersion: runtimeVersion, Model: entry.Model, ArtifactPrecision: "bf16", Accelerator: request.GPU}
	var capabilities optimizationcapability.Registry
	if profile.Workload.Empty() {
		var err error
		capabilities, err = optimizationcapability.V1()
		if err != nil {
			return Candidate{}, err
		}
		continuous, compileErr := capabilities.Compile(withMechanism(base, optimizationcapability.ContinuousBatching, nil))
		if compileErr != nil {
			return Candidate{}, compileErr
		}
		features = append(features, Feature{Name: "continuous_batching", State: "runtime-owned", Source: continuous.DescriptorID})
	} else {
		if err := profile.Workload.Validate(); err != nil {
			return Candidate{}, fmt.Errorf("portable runtime profile: %w", err)
		}
		features = append(features, Feature{Name: "immutable_runtime_artifact", State: "pinned", Source: profile.Workload.Image})
	}
	if compatible.Adapter == "kubernetes-dynamo" {
		features = append(features, Feature{Name: "dynamo_graph", State: "single-mutation-owner", Source: compatible.Evidence})
	}
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
	if request.MaxTTFTP95MS != nil || request.MaxTPOTP95MS != nil || request.MaxErrorRate != nil || request.MinGoodput != nil || request.MinOutputTokensSecond != nil {
		required = append(required, "measured SLO metrics")
	}
	required = append(required, "semantic quality evidence when model precision or artifact changes")
	limitations := append([]string(nil), profile.Limitations...)
	if compatible.Adapter == "kubernetes-dynamo" {
		limitations = append(limitations, "InferCrane owns one DGD parent and Dynamo owns the internal worker graph; outer replica autoscaling is disabled until an exact Dynamo scaling owner is qualified.")
	}
	limitations = append(limitations, "GPU fit for "+request.GPU+" is not inferred from the reviewed profile hint "+profile.GPUHint+".")
	if profile.Name != preferredProfile && !strings.HasSuffix(profile.Name, "-"+preferredProfile) {
		limitations = append(limitations, "This profile is an alternative candidate; it is not ranked by unmeasured performance.")
	}
	required, limitations = requireCustomerReplay(request, required, limitations)
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

// adapterRank puts a candidate on the adapter the request names ahead of
// candidates on another adapter of the same cloud. A provider such as
// "kubernetes" names both a cloud and its plain adapter, and the catalog also
// offers that cloud through the Dynamo serving graph. With equal evidence the
// order between the two used to fall to their content hashes, which change
// with the workload profile. The first candidate is the starting setup a
// diagnosis plans from, so the profile decided whether engine-flag techniques
// became setups or were all refused for a serving graph nobody asked for.
//
// A provider that names only a cloud, such as "runpod", names no adapter. Its
// adapters are then taken in name order after the artifact rank, so the
// starting setup is still the same for every profile. Neither rule reads what
// a diagnosed endpoint actually runs: the request says which kind of server
// the plan starts from.
func adapterRank(candidate Candidate, request Request) int {
	if candidate.Deployment.Provider.Adapter == request.Provider {
		return 0
	}
	return 1
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

// maxModelTokenizers bounds model_tokenizers: the starting setup's model and
// the published checkpoints a proposal may add.
const maxModelTokenizers = 16

var tokenizerDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// validateModelTokenizers checks each entry names one model at one revision
// and carries a sha256 tokenizer digest.
func validateModelTokenizers(tokenizers map[string]string) error {
	if len(tokenizers) > maxModelTokenizers {
		return fmt.Errorf("model_tokenizers names at most %d models", maxModelTokenizers)
	}
	for key, digest := range tokenizers {
		model, revision, found := strings.Cut(key, "@")
		if !found || model == "" || revision == "" || len(key) > 512 {
			return fmt.Errorf("model_tokenizers key %q must be MODEL@REVISION", key)
		}
		if !tokenizerDigestPattern.MatchString(digest) {
			return fmt.Errorf("model_tokenizers %q must be a sha256 digest", key)
		}
	}
	return nil
}

// TokenizerKey is a model's key in model_tokenizers.
func TokenizerKey(model, revision string) string {
	return model + "@" + revision
}
