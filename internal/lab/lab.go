// Package lab compares persisted benchmark evidence without provisioning infrastructure.
package lab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/performanceprofile"
)

// AlgorithmVersion v5 ranks latency objectives on a non-streaming workload
// profile by p95 request latency and names the ranked metric on every row.
const AlgorithmVersion = "inference-lab-v5"

const (
	ObjectiveLatency        = "latency"
	ObjectiveInteractive    = "interactive"
	ObjectiveThroughput     = "throughput"
	ObjectiveCostEfficiency = "cost-efficiency"
	// ObjectiveUsefulOutput ranks on the output of requests that met the
	// customer's limits: a request whose first token came too late counts
	// for nothing, however fast the server writes.
	ObjectiveUsefulOutput = "useful-output"
)

// DefaultFirstTokenLimitMS is the first-token limit a useful-output objective
// holds each request to when the customer states none: one second, the limit
// chat users notice.
const DefaultFirstTokenLimitMS = 1000.0

// FirstTokenLimitMS is the first-token limit a campaign holds each request
// to: the customer's own, else the default for a useful-output objective,
// else none (zero).
func FirstTokenLimitMS(objective string, limit *float64) float64 {
	switch {
	case limit != nil && *limit > 0:
		return *limit
	case objective == ObjectiveUsefulOutput:
		return DefaultFirstTokenLimitMS
	default:
		return 0
	}
}

// Metric keys an objective can rank on. They are the benchmark metric names
// that campaign measurements compare between interleaved runs.
const (
	MetricTTFTP95          = "ttft_p95_ms"
	MetricLatencyP95       = "latency_p95_ms"
	MetricOutputThroughput = "output_token_throughput"
	// MetricUsefulOutput is the output tokens per second of requests that met
	// every limit set.
	MetricUsefulOutput = "useful_output_throughput"
)

// BufferedLatencyNote explains why a latency objective on a non-streaming
// profile ranks on p95 request latency.
const BufferedLatencyNote = "buffered answers arrive whole, so time to first token is not measured; p95 request latency is the wait a buffered client sees"

// Metric is the measured value an objective ranks on.
type Metric struct {
	Name           string
	Label          string
	HigherIsBetter bool
	// CostScaled divides the metric by the sourced hourly cost.
	CostScaled bool
	// Note says why the objective ranks on this metric instead of its usual
	// one. It is empty when the usual metric applies.
	Note string
}

// ObjectiveMetric returns the metric an objective ranks on for a workload
// profile. Latency and interactive rank on p95 time to first token, except on
// a non-streaming profile such as buffered: AIPerf measures no first token
// there, and the time a buffered client waits is the whole request, so they
// rank on p95 request latency. ok is false for an unknown objective.
func ObjectiveMetric(objective, workloadProfile string) (Metric, bool) {
	switch objective {
	case ObjectiveLatency, ObjectiveInteractive:
		if profile, err := performanceprofile.Get(workloadProfile); err == nil && !profile.Streaming {
			return Metric{Name: MetricLatencyP95, Label: "p95 request latency", Note: BufferedLatencyNote}, true
		}
		return Metric{Name: MetricTTFTP95, Label: "p95 time to first token"}, true
	case ObjectiveThroughput:
		return Metric{Name: MetricOutputThroughput, Label: "output tokens per second", HigherIsBetter: true}, true
	case ObjectiveCostEfficiency:
		return Metric{Name: MetricOutputThroughput, Label: "output tokens per second per sourced hourly cost", HigherIsBetter: true, CostScaled: true}, true
	case ObjectiveUsefulOutput:
		return Metric{Name: MetricUsefulOutput, Label: "output tokens per second of requests within the limits", HigherIsBetter: true}, true
	}
	return Metric{}, false
}

type Input struct {
	ModelIdentity   string   `json:"model_identity"`
	Objective       string   `json:"objective"`
	WorkloadProfile string   `json:"workload_profile,omitempty"`
	MaxTTFTP95MS    *float64 `json:"max_ttft_p95_ms,omitempty"`
	MaxTPOTP95MS    *float64 `json:"max_tpot_p95_ms,omitempty"`
	MaxErrorRate    *float64 `json:"max_error_rate,omitempty"`
	MinGoodput      *float64 `json:"min_goodput,omitempty"`
	MinOutputTPS    *float64 `json:"min_output_tokens_second,omitempty"`
	MaxHourlyCost   *float64 `json:"max_hourly_cost,omitempty"`
	Region          string   `json:"region,omitempty"`
	MaxGPUCount     *int     `json:"max_gpu_count,omitempty"`
	WorkloadDigest  string   `json:"workload_digest,omitempty"`
	// DerivedModelIdentities are artifacts, such as quantized checkpoints, that
	// the caller proved were built from ModelIdentity. Their rows compete with
	// the base model's on the same workload. Lineage is not quality evidence:
	// the caller must admit only rows whose answers already passed a check
	// against the base model.
	DerivedModelIdentities []string `json:"derived_model_identities,omitempty"`
}

type Candidate struct {
	EvidenceClass      string          `json:"evidence_class"`
	EvidenceID         string          `json:"evidence_id"`
	Deployment         string          `json:"deployment"`
	Revision           string          `json:"revision"`
	Runtime            string          `json:"runtime"`
	RuntimeVersion     string          `json:"runtime_version"`
	Provider           string          `json:"provider"`
	Region             string          `json:"region,omitempty"`
	GPU                string          `json:"gpu"`
	GPUCount           *int            `json:"gpu_count,omitempty"`
	ComputeMode        string          `json:"compute_mode"`
	RequestCount       int             `json:"request_count"`
	TTFTP95MS          *float64        `json:"ttft_p95_ms,omitempty"`
	TPOTP95MS          *float64        `json:"tpot_p95_ms,omitempty"`
	LatencyP95MS       *float64        `json:"latency_p95_ms,omitempty"`
	OutputTokensSecond *float64        `json:"output_tokens_second,omitempty"`
	Goodput            *float64        `json:"goodput,omitempty"`
	ErrorRate          float64         `json:"error_rate"`
	MeetsSLO           *bool           `json:"meets_slo,omitempty"`
	ConstraintReasons  []string        `json:"constraint_reasons,omitempty"`
	Cost               json.RawMessage `json:"cost_metadata"`
	WorkloadDigest     string          `json:"workload_digest"`
	WorkloadProfile    string          `json:"workload_profile,omitempty"`
	Comparable         bool            `json:"comparable"`
	Selected           bool            `json:"selected"`
	ObjectiveValue     *float64        `json:"objective_value,omitempty"`
	// ObjectiveMetric names the measured metric ObjectiveValue comes from.
	ObjectiveMetric string `json:"objective_metric,omitempty"`
	SelectionReason string `json:"selection_reason,omitempty"`
	// ModelIdentity is set only on a row that served one of the input's
	// derived artifacts instead of its base model.
	ModelIdentity string `json:"model_identity,omitempty"`
}

func Evaluate(input Input, evidence []domain.BenchmarkResult) (domain.LabEvaluation, error) {
	if input.Objective == "" {
		input.Objective = ObjectiveLatency
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return domain.LabEvaluation{}, err
	}
	digest := sha256.Sum256(inputJSON)
	metric, knownObjective := ObjectiveMetric(input.Objective, input.WorkloadProfile)
	admitted := map[string]struct{}{input.ModelIdentity: {}}
	for _, identity := range input.DerivedModelIdentities {
		admitted[identity] = struct{}{}
	}
	candidates := make([]Candidate, 0, len(evidence))
	for _, row := range evidence {
		workloadDigest := digestJSON(row.WorkloadJSON)
		workloadProfile := profileJSON(row.WorkloadJSON)
		if _, ok := admitted[row.ModelIdentity]; !ok || input.WorkloadDigest != "" && workloadDigest != input.WorkloadDigest || input.WorkloadProfile != "" && workloadProfile != input.WorkloadProfile {
			continue
		}
		errorRate := 0.0
		if row.RequestCount > 0 {
			errorRate = float64(row.Failed) / float64(row.RequestCount)
		}
		cost := json.RawMessage(row.CostMetadataJSON)
		if !json.Valid(cost) {
			cost = json.RawMessage(`{"available":false}`)
		}
		candidate := Candidate{EvidenceClass: "measured", EvidenceID: row.ID, Deployment: row.DeploymentName, Revision: row.RevisionID, Runtime: row.Runtime, RuntimeVersion: row.RuntimeVersion, Provider: row.Provider, Region: row.Region, GPU: row.GPU, GPUCount: row.GPUCount, ComputeMode: row.ComputeMode, RequestCount: row.RequestCount, TTFTP95MS: row.TTFTP95MS, TPOTP95MS: row.TPOTP95MS, LatencyP95MS: row.LatencyP95MS, OutputTokensSecond: row.OutputTokenThroughput, Goodput: row.Goodput, ErrorRate: errorRate, Cost: cost, WorkloadDigest: workloadDigest, WorkloadProfile: workloadProfile}
		if row.ModelIdentity != input.ModelIdentity {
			candidate.ModelIdentity = row.ModelIdentity
		}
		candidate.MeetsSLO, candidate.ConstraintReasons = evaluateConstraints(input, candidate)
		if knownObjective {
			candidate.ObjectiveValue, candidate.ObjectiveMetric = objectiveValue(metric, candidate), metric.Name
		}
		candidates = append(candidates, candidate)
	}
	digests := map[string]struct{}{}
	for _, candidate := range candidates {
		if candidate.WorkloadDigest != "" {
			digests[candidate.WorkloadDigest] = struct{}{}
		}
	}
	comparable := len(candidates) > 0 && len(digests) == 1
	for index := range candidates {
		if candidates[index].WorkloadDigest == "" {
			comparable = false
		}
	}
	for index := range candidates {
		candidates[index].Comparable = comparable
		if !comparable {
			candidates[index].SelectionReason = "exact workload digest required before ranking"
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].WorkloadDigest != candidates[j].WorkloadDigest {
			return candidates[i].WorkloadDigest < candidates[j].WorkloadDigest
		}
		a, b := candidates[i].ObjectiveValue, candidates[j].ObjectiveValue
		if a == nil || b == nil {
			if a == nil && b != nil {
				return false
			}
			if a != nil && b == nil {
				return true
			}
			return candidates[i].EvidenceID < candidates[j].EvidenceID
		}
		if *a == *b {
			return candidates[i].EvidenceID < candidates[j].EvidenceID
		}
		if metric.HigherIsBetter {
			return *a > *b
		}
		return *a < *b
	})
	if comparable {
		for index := range candidates {
			if candidates[index].ObjectiveValue != nil && (candidates[index].MeetsSLO == nil || *candidates[index].MeetsSLO) {
				candidates[index].Selected = true
				candidates[index].SelectionReason = "best measured " + input.Objective + " (" + metric.Label + ") among exact-workload candidates"
				break
			}
		}
	}
	resultsJSON, err := json.Marshal(candidates)
	if err != nil {
		return domain.LabEvaluation{}, err
	}
	return domain.LabEvaluation{ModelIdentity: input.ModelIdentity, AlgorithmVersion: AlgorithmVersion, InputJSON: string(inputJSON), ResultsJSON: string(resultsJSON), InputDigest: hex.EncodeToString(digest[:])}, nil
}

func evaluateConstraints(input Input, candidate Candidate) (*bool, []string) {
	hasConstraints := input.MaxTTFTP95MS != nil || input.MaxTPOTP95MS != nil || input.MaxErrorRate != nil || input.MinGoodput != nil || input.MinOutputTPS != nil || input.MaxHourlyCost != nil || input.Region != "" || input.MaxGPUCount != nil
	if !hasConstraints {
		return nil, nil
	}
	var reasons []string
	requireMaximum := func(name string, actual, maximum *float64) {
		if maximum == nil {
			return
		}
		if !validMetric(actual) {
			reasons = append(reasons, name+" evidence unavailable")
			return
		}
		if *actual > *maximum {
			reasons = append(reasons, name+" exceeds constraint")
		}
	}
	requireMinimum := func(name string, actual, minimum *float64) {
		if minimum == nil {
			return
		}
		if !validMetric(actual) {
			reasons = append(reasons, name+" evidence unavailable")
			return
		}
		if *actual < *minimum {
			reasons = append(reasons, name+" is below constraint")
		}
	}
	requireMaximum("TTFT p95", candidate.TTFTP95MS, input.MaxTTFTP95MS)
	requireMaximum("TPOT p95", candidate.TPOTP95MS, input.MaxTPOTP95MS)
	if input.MaxErrorRate != nil {
		if candidate.RequestCount <= 0 {
			reasons = append(reasons, "error rate evidence unavailable")
		} else if candidate.ErrorRate > *input.MaxErrorRate {
			reasons = append(reasons, "error rate exceeds constraint")
		}
	}
	requireMinimum("goodput", candidate.Goodput, input.MinGoodput)
	requireMinimum("output throughput", candidate.OutputTokensSecond, input.MinOutputTPS)
	if input.MaxHourlyCost != nil {
		hourly := hourlyCost(candidate.Cost)
		requireMaximum("hourly cost", hourly, input.MaxHourlyCost)
	}
	if input.Region != "" && candidate.Region != input.Region {
		reasons = append(reasons, "region does not match constraint")
	}
	if input.MaxGPUCount != nil {
		if candidate.GPUCount == nil || *candidate.GPUCount <= 0 {
			reasons = append(reasons, "GPU count evidence unavailable")
		} else if *candidate.GPUCount > *input.MaxGPUCount {
			reasons = append(reasons, "GPU count exceeds constraint")
		}
	}
	meets := len(reasons) == 0
	return &meets, reasons
}

func objectiveValue(metric Metric, candidate Candidate) *float64 {
	var value *float64
	switch metric.Name {
	case MetricTTFTP95:
		value = candidate.TTFTP95MS
	case MetricLatencyP95:
		value = candidate.LatencyP95MS
	case MetricOutputThroughput:
		value = candidate.OutputTokensSecond
	}
	if !validMetric(value) {
		return nil
	}
	if !metric.CostScaled {
		return value
	}
	hourly := hourlyCost(candidate.Cost)
	if !validMetric(hourly) || *hourly <= 0 {
		return nil
	}
	scaled := *value / *hourly
	return &scaled
}

func hourlyCost(raw json.RawMessage) *float64 {
	var cost struct {
		Available bool     `json:"available"`
		Hourly    *float64 `json:"hourly"`
		Source    string   `json:"source"`
	}
	if json.Unmarshal(raw, &cost) != nil || !cost.Available || !validMetric(cost.Hourly) || cost.Source == "" {
		return nil
	}
	return cost.Hourly
}

func validMetric(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0
}

func profileJSON(value string) string {
	var workload struct {
		Profile string `json:"profile"`
	}
	if json.Unmarshal([]byte(value), &workload) != nil {
		return ""
	}
	return workload.Profile
}

// nonWorkloadKeys are the workload-evidence keys that never describe the load
// that was sent. "confidence_95" holds per-run Student-t intervals and
// "measured" holds campaign control, noise-floor, and server-metric results.
// Keeping either in the digest gave every repeated benchmark a unique identity,
// which excluded the most rigorous evidence from ranking. A setup an InferCrane
// agent measured also names its windows' agent commands and nodes
// ("agent_windows") and whether it ran on another node ("fresh_machine"):
// execution provenance that differs for every setup of one run.
var nonWorkloadKeys = []string{"revision_selector", "direct_revision_validation", "confidence_95", "measured", "agent_windows", "fresh_machine"}

// WorkloadDigest returns the comparability identity Lab uses for one stored
// workload document. Callers that pre-check comparability must use it rather
// than hashing the raw document.
func WorkloadDigest(value string) string { return digestJSON(value) }

func digestJSON(value string) string {
	var raw any
	if json.Unmarshal([]byte(value), &raw) != nil {
		return ""
	}
	// Target selection is execution provenance, not workload shape, and the
	// measured outputs that benchmark writers keep beside the load inputs are
	// results, not inputs. Active and candidate runs remain comparable when
	// every actual load input matches. Preserve all other present and future
	// fields so a new workload dimension cannot silently create a false
	// comparison.
	if object, ok := raw.(map[string]any); ok {
		for _, key := range nonWorkloadKeys {
			delete(object, key)
		}
	}
	canonical, _ := json.Marshal(raw)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
