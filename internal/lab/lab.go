// Package lab compares persisted benchmark evidence without provisioning infrastructure.
package lab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"

	"github.com/infercrane/infercrane/internal/domain"
)

const AlgorithmVersion = "inference-lab-v2"

const (
	ObjectiveLatency        = "latency"
	ObjectiveThroughput     = "throughput"
	ObjectiveCostEfficiency = "cost-efficiency"
)

type Input struct {
	ModelIdentity   string   `json:"model_identity"`
	Objective       string   `json:"objective"`
	WorkloadProfile string   `json:"workload_profile,omitempty"`
	MaxTTFTP95MS    *float64 `json:"max_ttft_p95_ms,omitempty"`
	WorkloadDigest  string   `json:"workload_digest,omitempty"`
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
	TTFTP95MS          *float64        `json:"ttft_p95_ms,omitempty"`
	OutputTokensSecond *float64        `json:"output_tokens_second,omitempty"`
	ErrorRate          float64         `json:"error_rate"`
	MeetsSLO           *bool           `json:"meets_slo,omitempty"`
	Cost               json.RawMessage `json:"cost_metadata"`
	WorkloadDigest     string          `json:"workload_digest"`
	WorkloadProfile    string          `json:"workload_profile,omitempty"`
	Comparable         bool            `json:"comparable"`
	Selected           bool            `json:"selected"`
	ObjectiveValue     *float64        `json:"objective_value,omitempty"`
	SelectionReason    string          `json:"selection_reason,omitempty"`
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
	candidates := make([]Candidate, 0, len(evidence))
	for _, row := range evidence {
		workloadDigest := digestJSON(row.WorkloadJSON)
		workloadProfile := profileJSON(row.WorkloadJSON)
		if row.ModelIdentity != input.ModelIdentity || input.WorkloadDigest != "" && workloadDigest != input.WorkloadDigest || input.WorkloadProfile != "" && workloadProfile != input.WorkloadProfile {
			continue
		}
		errorRate := 0.0
		if row.RequestCount > 0 {
			errorRate = float64(row.Failed) / float64(row.RequestCount)
		}
		var meets *bool
		if input.MaxTTFTP95MS != nil && row.TTFTP95MS != nil {
			value := *row.TTFTP95MS <= *input.MaxTTFTP95MS
			meets = &value
		}
		cost := json.RawMessage(row.CostMetadataJSON)
		if !json.Valid(cost) {
			cost = json.RawMessage(`{"available":false}`)
		}
		candidate := Candidate{EvidenceClass: "measured", EvidenceID: row.ID, Deployment: row.DeploymentName, Revision: row.RevisionID, Runtime: row.Runtime, RuntimeVersion: row.RuntimeVersion, Provider: row.Provider, Region: row.Region, GPU: row.GPU, GPUCount: row.GPUCount, ComputeMode: row.ComputeMode, TTFTP95MS: row.TTFTP95MS, OutputTokensSecond: row.OutputTokenThroughput, ErrorRate: errorRate, MeetsSLO: meets, Cost: cost, WorkloadDigest: workloadDigest, WorkloadProfile: workloadProfile}
		candidate.ObjectiveValue = objectiveValue(input.Objective, candidate)
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
		if input.Objective == ObjectiveLatency {
			return *a < *b
		}
		return *a > *b
	})
	if comparable {
		for index := range candidates {
			if candidates[index].ObjectiveValue != nil && (candidates[index].MeetsSLO == nil || *candidates[index].MeetsSLO) {
				candidates[index].Selected = true
				candidates[index].SelectionReason = "best measured " + input.Objective + " among exact-workload candidates"
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

func objectiveValue(objective string, candidate Candidate) *float64 {
	switch objective {
	case ObjectiveLatency:
		if validMetric(candidate.TTFTP95MS) {
			return candidate.TTFTP95MS
		}
	case ObjectiveThroughput:
		if validMetric(candidate.OutputTokensSecond) {
			return candidate.OutputTokensSecond
		}
	case ObjectiveCostEfficiency:
		if !validMetric(candidate.OutputTokensSecond) {
			return nil
		}
		var cost struct {
			Available bool     `json:"available"`
			Hourly    *float64 `json:"hourly"`
			Source    string   `json:"source"`
		}
		if json.Unmarshal(candidate.Cost, &cost) != nil || !cost.Available || !validMetric(cost.Hourly) || *cost.Hourly <= 0 || cost.Source == "" {
			return nil
		}
		value := *candidate.OutputTokensSecond / *cost.Hourly
		return &value
	default:
		return nil
	}
	return nil
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

func digestJSON(value string) string {
	var raw any
	if json.Unmarshal([]byte(value), &raw) != nil {
		return ""
	}
	// Target selection is execution provenance, not workload shape. Active and
	// candidate runs remain comparable when every actual load input matches.
	// Preserve all other present and future fields so a new workload dimension
	// cannot silently create a false comparison.
	if object, ok := raw.(map[string]any); ok {
		delete(object, "revision_selector")
		delete(object, "direct_revision_validation")
	}
	canonical, _ := json.Marshal(raw)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
