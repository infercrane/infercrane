package optimizer

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/performanceprofile"
	"github.com/infercrane/infercrane/internal/servingcontract"
)

const (
	AIConfiguratorVersion        = "0.11.0"
	AIConfiguratorPlotextVersion = "5.3.2"
	estimatorInputSchema         = "infercrane.optimizer.estimator-input/v1"
	estimatorOutputSchema        = "infercrane.optimizer.estimator-output/v1"
	maxEstimatorOutputBytes      = 4 << 20
	defaultEstimatorTimeout      = 2 * time.Minute
)

//go:embed aiconfigurator_adapter.py
var aiConfiguratorAdapter []byte

var ErrEstimatorUnavailable = errors.New("optimization estimator unavailable")

type estimatorInput struct {
	SchemaVersion          string  `json:"schema_version"`
	RequiredVersion        string  `json:"required_version"`
	RequiredPlotextVersion string  `json:"required_plotext_version"`
	ModelPath              string  `json:"model_path"`
	System                 string  `json:"system"`
	Backend                string  `json:"backend"`
	DatabaseMode           string  `json:"database_mode"`
	TargetConcurrency      float64 `json:"target_concurrency"`
	InputTokens            int     `json:"input_tokens"`
	OutputTokens           int     `json:"output_tokens"`
	TTFTMS                 float64 `json:"ttft_ms"`
	TPOTMS                 float64 `json:"tpot_ms"`
	PrefixTokens           int     `json:"prefix_tokens"`
	EnableChunkedPrefill   bool    `json:"enable_chunked_prefill"`
	TopN                   int     `json:"top_n"`
}

type estimatorPool struct {
	Replicas          int `json:"replicas"`
	TensorParallelism int `json:"tensor_parallelism"`
}

type estimatorCandidate struct {
	Mode                              string        `json:"mode"`
	Backend                           string        `json:"backend"`
	TotalGPUs                         int           `json:"total_gpus"`
	Replicas                          int           `json:"replicas"`
	GPUsPerReplica                    int           `json:"gpus_per_replica"`
	TensorParallelism                 int           `json:"tensor_parallelism"`
	EstimatedTTFTMS                   *float64      `json:"estimated_ttft_ms"`
	EstimatedTPOTMS                   *float64      `json:"estimated_tpot_ms"`
	EstimatedRequestLatencyMS         *float64      `json:"estimated_request_latency_ms"`
	EstimatedRequestRate              *float64      `json:"estimated_request_rate"`
	EstimatedOutputTokensSecondPerGPU *float64      `json:"estimated_output_tokens_second_per_gpu"`
	Prefill                           estimatorPool `json:"prefill"`
	Decode                            estimatorPool `json:"decode"`
}

type estimatorOutput struct {
	SchemaVersion string               `json:"schema_version"`
	Source        string               `json:"source"`
	SourceVersion string               `json:"source_version"`
	EvidenceClass string               `json:"evidence_class"`
	ModelPath     string               `json:"model_path"`
	System        string               `json:"system"`
	Backend       string               `json:"backend"`
	ResultDigest  string               `json:"result_digest"`
	Candidates    []estimatorCandidate `json:"candidates"`
	Error         string               `json:"error,omitempty"`
	Message       string               `json:"message,omitempty"`
}

// EstimatorRunner is the replaceable process boundary shared by today's
// AIConfigurator adapter and the future AISimulate adapter.
type EstimatorRunner interface {
	Estimate(context.Context, estimatorInput) (estimatorOutput, error)
}

type PythonEstimatorRunner struct {
	Python       string
	Timeout      time.Duration
	AllowNetwork bool
}

func (r PythonEstimatorRunner) Probe(ctx context.Context) (string, error) {
	python := strings.TrimSpace(r.Python)
	if python == "" {
		python = "python3"
	}
	temp, err := os.MkdirTemp("", "infercrane-estimator-probe-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "-c", "import aiconfigurator, importlib.metadata; print(aiconfigurator.__version__ + ' ' + importlib.metadata.version('plotext'))")
	command.Dir = temp
	command.Env = estimatorEnvironment(temp, false)
	var stdout, stderr boundedBuffer
	stdout.limit, stderr.limit = 1024, 4096
	command.Stdout, command.Stderr = &stdout, &stderr
	if err = command.Run(); err != nil {
		return "", fmt.Errorf("%w: %s cannot load aiconfigurator: %s", ErrEstimatorUnavailable, python, boundedDiagnostic(stderr.String()))
	}
	versions := strings.Fields(stdout.String())
	if len(versions) != 2 || versions[0] != AIConfiguratorVersion || versions[1] != AIConfiguratorPlotextVersion {
		return "", fmt.Errorf("%w: installed tool tuple %q does not match required aiconfigurator %s + plotext %s", ErrEstimatorUnavailable, strings.TrimSpace(stdout.String()), AIConfiguratorVersion, AIConfiguratorPlotextVersion)
	}
	return versions[0], nil
}

func (r PythonEstimatorRunner) Estimate(ctx context.Context, input estimatorInput) (estimatorOutput, error) {
	python := strings.TrimSpace(r.Python)
	if python == "" {
		python = "python3"
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultEstimatorTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	temp, err := os.MkdirTemp("", "infercrane-estimator-")
	if err != nil {
		return estimatorOutput{}, fmt.Errorf("create isolated estimator directory: %w", err)
	}
	defer os.RemoveAll(temp)
	script := filepath.Join(temp, "adapter.py")
	if err = os.WriteFile(script, aiConfiguratorAdapter, 0o600); err != nil {
		return estimatorOutput{}, fmt.Errorf("write estimator adapter: %w", err)
	}
	body, err := json.Marshal(input)
	if err != nil {
		return estimatorOutput{}, fmt.Errorf("encode estimator request: %w", err)
	}
	command := exec.CommandContext(ctx, python, script)
	command.Dir = temp
	command.Stdin = bytes.NewReader(body)
	command.Env = estimatorEnvironment(temp, r.AllowNetwork)
	var stdout, stderr boundedBuffer
	stdout.limit, stderr.limit = maxEstimatorOutputBytes, maxEstimatorOutputBytes
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr := command.Run()
	var response estimatorOutput
	decodeErr := json.Unmarshal(stdout.Bytes(), &response)
	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return estimatorOutput{}, fmt.Errorf("%w: AIConfigurator exceeded %s", ErrEstimatorUnavailable, timeout)
		}
		if decodeErr == nil && response.Message != "" {
			return estimatorOutput{}, fmt.Errorf("%w: %s: %s", ErrEstimatorUnavailable, response.Error, response.Message)
		}
		return estimatorOutput{}, fmt.Errorf("%w: AIConfigurator process failed: %v: %s", ErrEstimatorUnavailable, runErr, boundedDiagnostic(stderr.String()))
	}
	if stdout.overflow || stderr.overflow {
		return estimatorOutput{}, fmt.Errorf("%w: AIConfigurator output exceeded %d bytes", ErrEstimatorUnavailable, maxEstimatorOutputBytes)
	}
	if decodeErr != nil {
		return estimatorOutput{}, fmt.Errorf("%w: decode AIConfigurator output: %v", ErrEstimatorUnavailable, decodeErr)
	}
	if err = validateEstimatorOutput(response, input); err != nil {
		return estimatorOutput{}, fmt.Errorf("%w: %v", ErrEstimatorUnavailable, err)
	}
	return response, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return original, nil
	}
	if len(value) > remaining {
		b.overflow = true
		value = value[:remaining]
	}
	_, _ = b.Buffer.Write(value)
	return original, nil
}

func estimatorEnvironment(temp string, allowNetwork bool) []string {
	allowed := map[string]bool{"PATH": true, "LANG": true, "LC_ALL": true, "SSL_CERT_FILE": true, "REQUESTS_CA_BUNDLE": true}
	environment := []string{"HOME=" + temp, "HF_HOME=" + filepath.Join(temp, "huggingface"), "PYTHONNOUSERSITE=1", "AICONFIGURATOR_LOG_LEVEL=ERROR"}
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if allowed[name] {
			environment = append(environment, item)
		}
	}
	if !allowNetwork {
		environment = append(environment, "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1")
	}
	return environment
}

func boundedDiagnostic(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1000 {
		return value[:1000] + "…"
	}
	return value
}

func validateEstimatorOutput(output estimatorOutput, input estimatorInput) error {
	if output.SchemaVersion != estimatorOutputSchema || output.Source != "aiconfigurator" || output.SourceVersion != AIConfiguratorVersion || output.EvidenceClass != "modeled" {
		return errors.New("AIConfigurator returned an incompatible identity or schema")
	}
	if output.ModelPath != input.ModelPath || output.System != input.System || output.Backend != input.Backend {
		return errors.New("AIConfigurator output does not match the requested model, system, and backend")
	}
	if !strings.HasPrefix(output.ResultDigest, "sha256:") || len(output.ResultDigest) != 71 {
		return errors.New("AIConfigurator output lacks a valid result digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(output.ResultDigest, "sha256:")); err != nil {
		return errors.New("AIConfigurator output lacks a valid result digest")
	}
	if len(output.Candidates) > 100 {
		return errors.New("AIConfigurator returned too many candidates")
	}
	for _, candidate := range output.Candidates {
		if candidate.Backend != input.Backend || candidate.TotalGPUs < 0 || candidate.Replicas < 0 || candidate.GPUsPerReplica < 0 || candidate.TensorParallelism < 0 {
			return errors.New("AIConfigurator returned an invalid candidate boundary")
		}
		for _, estimate := range []*float64{candidate.EstimatedTTFTMS, candidate.EstimatedTPOTMS, candidate.EstimatedRequestLatencyMS, candidate.EstimatedRequestRate, candidate.EstimatedOutputTokensSecondPerGPU} {
			if estimate != nil && (*estimate < 0 || math.IsNaN(*estimate) || math.IsInf(*estimate, 0)) {
				return errors.New("AIConfigurator returned an invalid modeled metric")
			}
		}
		if candidate.Mode != servingcontract.ModeAggregated && candidate.Mode != servingcontract.ModeDisaggregated {
			return errors.New("AIConfigurator returned an unsupported serving mode")
		}
	}
	return nil
}

type AIConfiguratorSource struct {
	Catalog CatalogSource
	Runner  EstimatorRunner
}

func (s AIConfiguratorSource) Propose(ctx context.Context, request Request) (Proposal, error) {
	base, err := s.Catalog.Propose(ctx, request)
	if err != nil || len(base.Candidates) == 0 {
		return base, err
	}
	if s.Runner == nil {
		return Proposal{}, fmt.Errorf("%w: runner is not configured", ErrEstimatorUnavailable)
	}
	entry, ok := findRecipe(s.Catalog.Recipes, base.Input.ModelIdentity)
	if !ok {
		return base, nil
	}
	profile, err := performanceprofile.Get(base.Input.WorkloadProfile)
	if err != nil {
		return Proposal{}, err
	}
	targetConcurrency := float64(profile.Concurrency)
	if base.Input.TargetConcurrency != nil {
		targetConcurrency = *base.Input.TargetConcurrency
	}
	ttft, tpot := 2000.0, 30.0
	if base.Input.MaxTTFTP95MS != nil {
		ttft = *base.Input.MaxTTFTP95MS
	}
	if base.Input.MaxTPOTP95MS != nil {
		tpot = *base.Input.MaxTPOTP95MS
	}

	byRuntime := map[string]Candidate{}
	for _, candidate := range base.Candidates {
		if _, exists := byRuntime[candidate.Deployment.Runtime.Engine]; !exists {
			byRuntime[candidate.Deployment.Runtime.Engine] = candidate
		}
	}
	var modeled []Candidate
	var failures []string
	for _, runtimeName := range sortedRuntimeNames(byRuntime) {
		input := estimatorInput{SchemaVersion: estimatorInputSchema, RequiredVersion: AIConfiguratorVersion, RequiredPlotextVersion: AIConfiguratorPlotextVersion, ModelPath: entry.Model, System: aiConfiguratorSystem(base.Input.GPU), Backend: runtimeName, DatabaseMode: "HYBRID", TargetConcurrency: targetConcurrency, InputTokens: profile.InputTokens, OutputTokens: profile.OutputTokens, TTFTMS: ttft, TPOTMS: tpot, TopN: base.Input.MaxCandidates, EnableChunkedPrefill: true}
		output, estimateErr := s.Runner.Estimate(ctx, input)
		if estimateErr != nil {
			failures = append(failures, runtimeName+": "+estimateErr.Error())
			continue
		}
		for _, estimate := range output.Candidates {
			candidate, executable := modeledCandidate(byRuntime[runtimeName], base.Input, output, estimate)
			if executable {
				modeled = append(modeled, candidate)
			}
		}
	}
	if len(modeled) == 0 {
		if len(failures) > 0 {
			return Proposal{}, fmt.Errorf("%w: %s", ErrEstimatorUnavailable, strings.Join(failures, "; "))
		}
		return Proposal{}, fmt.Errorf("%w: AIConfigurator returned no executable candidates for this InferCrane provider boundary", ErrEstimatorUnavailable)
	}
	sort.Slice(modeled, func(i, j int) bool {
		if modeled[i].ModeledEvidence.TotalGPUs != modeled[j].ModeledEvidence.TotalGPUs {
			return modeled[i].ModeledEvidence.TotalGPUs < modeled[j].ModeledEvidence.TotalGPUs
		}
		return modeled[i].ID < modeled[j].ID
	})
	if len(modeled) > base.Input.MaxCandidates {
		modeled = modeled[:base.Input.MaxCandidates]
	}
	for index := range modeled {
		modeled[index].Rank = index + 1
	}
	base.AlgorithmVersion = "aiconfigurator-adapter-v1"
	base.Candidates = modeled
	base.SelectionBoundary = "AIConfigurator modeled candidates only; exact AIPerf, replay, semantic quality, sourced cost, and Release Guard evidence are required before qualification"
	return base, nil
}

func sortedRuntimeNames(values map[string]Candidate) []string {
	result := make([]string, 0, len(values))
	for name := range values {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func aiConfiguratorSystem(gpu string) string {
	normalized := strings.ToLower(strings.NewReplacer("nvidia-", "", "nvidia ", "", "_", "", "-", "", " ", "").Replace(gpu))
	switch normalized {
	case "h100sxm":
		return "h100_sxm"
	case "h100pcie", "h100":
		return "h100_pcie"
	case "h200", "h200sxm":
		return "h200_sxm"
	case "b200", "b200sxm":
		return "b200_sxm"
	case "l40s", "l4", "a30", "gb200", "gb300":
		return normalized
	default:
		return strings.ToLower(strings.TrimSpace(gpu))
	}
}

func modeledCandidate(base Candidate, request Request, output estimatorOutput, estimate estimatorCandidate) (Candidate, bool) {
	candidate := base
	candidate.Source = SourceInfo{Name: "aiconfigurator", Version: output.SourceVersion, EvidenceClass: "modeled", UpstreamProject: "https://github.com/ai-dynamo/aiconfigurator"}
	candidate.Status, candidate.EvidenceState = "proposed-modeled-unqualified", EvidenceModeled
	candidate.ConfigurationProfile = "aiconfigurator-" + estimate.Mode
	candidate.Features = append([]Feature(nil), base.Features...)
	candidate.RequiredEvidence = append([]string(nil), base.RequiredEvidence...)
	candidate.Limitations = append([]string(nil), base.Limitations...)
	candidate.Limitations = append(candidate.Limitations, "AIConfigurator output is a modeled estimate, not measured performance or a production recommendation.")
	candidate.ModeledEvidence = &ModeledEvidence{SchemaVersion: estimatorOutputSchema, SourceVersion: output.SourceVersion, ResultDigest: output.ResultDigest, System: output.System, Mode: estimate.Mode, EstimatedTTFTMS: estimate.EstimatedTTFTMS, EstimatedTPOTMS: estimate.EstimatedTPOTMS, EstimatedRequestLatencyMS: estimate.EstimatedRequestLatencyMS, EstimatedRequestRate: estimate.EstimatedRequestRate, EstimatedOutputTokensPerGPU: estimate.EstimatedOutputTokensSecondPerGPU, TotalGPUs: estimate.TotalGPUs, Replicas: estimate.Replicas, GPUsPerReplica: estimate.GPUsPerReplica}

	if base.Deployment.Provider.Adapter == "kubernetes-dynamo" {
		candidate.Deployment.Scaling.MinReplicas, candidate.Deployment.Scaling.MaxReplicas = 1, 1
		topology := servingcontract.Topology{Backend: servingcontract.BackendDynamo, Profile: "custom", Mode: estimate.Mode, Routing: servingcontract.RoutingDirect, Autoscaling: servingcontract.Autoscaling{Owner: servingcontract.AutoscalingDisabled}, Cache: servingcontract.Cache{Backend: servingcontract.CacheNone}}
		if estimate.Mode == servingcontract.ModeAggregated {
			replicas, tp := estimate.Replicas, estimate.TensorParallelism
			if replicas < 1 {
				replicas = 1
			}
			if tp < 1 {
				tp = estimate.GPUsPerReplica
			}
			if tp < 1 {
				return Candidate{}, false
			}
			topology.Worker = servingcontract.Pool{Replicas: replicas, TensorParallelism: tp}
		} else {
			if estimate.Prefill.Replicas < 1 || estimate.Prefill.TensorParallelism < 1 || estimate.Decode.Replicas < 1 || estimate.Decode.TensorParallelism < 1 {
				return Candidate{}, false
			}
			topology.Prefill = servingcontract.Pool{Replicas: estimate.Prefill.Replicas, TensorParallelism: estimate.Prefill.TensorParallelism}
			topology.Decode = servingcontract.Pool{Replicas: estimate.Decode.Replicas, TensorParallelism: estimate.Decode.TensorParallelism}
		}
		candidate.Deployment.Serving = topology.Normalize()
	} else {
		if estimate.Mode != servingcontract.ModeAggregated || estimate.GPUsPerReplica > 1 || estimate.TensorParallelism > 1 {
			return Candidate{}, false
		}
		replicas := estimate.Replicas
		if replicas < 1 {
			replicas = estimate.TotalGPUs
		}
		if replicas < 1 || replicas > 10000 {
			return Candidate{}, false
		}
		candidate.Deployment.Scaling.MinReplicas, candidate.Deployment.Scaling.MaxReplicas = replicas, replicas
	}
	identity, _ := json.Marshal(struct {
		Source     SourceInfo         `json:"source"`
		Input      Request            `json:"input"`
		Estimate   estimatorCandidate `json:"estimate"`
		Deployment DeploymentDraft    `json:"deployment"`
	}{candidate.Source, request, estimate, candidate.Deployment})
	digest := sha256.Sum256(identity)
	candidate.ID = hex.EncodeToString(digest[:])
	candidate.Deployment.Name = safeName(base.Deployment.Name + "-aic-" + candidate.ID[:8])
	return candidate, true
}
