// Package lab compares persisted benchmark evidence without provisioning infrastructure.
package lab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/infercrane/infercrane/internal/domain"
)

const AlgorithmVersion = "inference-lab-v1"

type Input struct {
	ModelIdentity  string   `json:"model_identity"`
	MaxTTFTP95MS   *float64 `json:"max_ttft_p95_ms,omitempty"`
	WorkloadDigest string   `json:"workload_digest,omitempty"`
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
}

func Evaluate(input Input, evidence []domain.BenchmarkResult) (domain.LabEvaluation, error) {
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return domain.LabEvaluation{}, err
	}
	digest := sha256.Sum256(inputJSON)
	candidates := make([]Candidate, 0, len(evidence))
	for _, row := range evidence {
		if row.ModelIdentity != input.ModelIdentity || input.WorkloadDigest != "" && digestJSON(row.WorkloadJSON) != input.WorkloadDigest {
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
		candidates = append(candidates, Candidate{EvidenceClass: "measured", EvidenceID: row.ID, Deployment: row.DeploymentName, Revision: row.RevisionID, Runtime: row.Runtime, RuntimeVersion: row.RuntimeVersion, Provider: row.Provider, Region: row.Region, GPU: row.GPU, GPUCount: row.GPUCount, ComputeMode: row.ComputeMode, TTFTP95MS: row.TTFTP95MS, OutputTokensSecond: row.OutputTokenThroughput, ErrorRate: errorRate, MeetsSLO: meets, Cost: cost})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].TTFTP95MS == nil {
			return false
		}
		if candidates[j].TTFTP95MS == nil {
			return true
		}
		if *candidates[i].TTFTP95MS == *candidates[j].TTFTP95MS {
			return candidates[i].EvidenceID < candidates[j].EvidenceID
		}
		return *candidates[i].TTFTP95MS < *candidates[j].TTFTP95MS
	})
	resultsJSON, err := json.Marshal(candidates)
	if err != nil {
		return domain.LabEvaluation{}, err
	}
	return domain.LabEvaluation{ModelIdentity: input.ModelIdentity, AlgorithmVersion: AlgorithmVersion, InputJSON: string(inputJSON), ResultsJSON: string(resultsJSON), InputDigest: hex.EncodeToString(digest[:])}, nil
}

func digestJSON(value string) string {
	var raw any
	if json.Unmarshal([]byte(value), &raw) != nil {
		return ""
	}
	canonical, _ := json.Marshal(raw)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
