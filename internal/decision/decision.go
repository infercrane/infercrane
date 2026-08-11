// Package decision evaluates inference evidence without guessing missing facts.
package decision

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

const AlgorithmVersion = "recommendation-v1"

type SLOPolicy struct {
	MaxTTFTP95MS          *float64 `json:"max_ttft_p95_ms,omitempty"`
	MaxLatencyP95MS       *float64 `json:"max_latency_p95_ms,omitempty"`
	MaxErrorRate          *float64 `json:"max_error_rate,omitempty"`
	MinOutputTokensSecond *float64 `json:"min_output_tokens_second,omitempty"`
	MaxHourlyCost         *float64 `json:"max_hourly_cost,omitempty"`
}

func (p SLOPolicy) Validate() error {
	values := []*float64{p.MaxTTFTP95MS, p.MaxLatencyP95MS, p.MaxErrorRate, p.MinOutputTokensSecond, p.MaxHourlyCost}
	set := false
	for _, value := range values {
		if value == nil {
			continue
		}
		set = true
		if math.IsNaN(*value) || math.IsInf(*value, 0) {
			return errors.New("SLO thresholds must be finite")
		}
		if *value < 0 {
			return errors.New("SLO thresholds cannot be negative")
		}
	}
	if !set {
		return errors.New("at least one SLO threshold is required")
	}
	if p.MaxErrorRate != nil && *p.MaxErrorRate > 1 {
		return errors.New("max_error_rate must be between 0 and 1")
	}
	return nil
}

type Evidence struct {
	ID                 string     `json:"id"`
	ModelIdentity      string     `json:"model_identity"`
	Runtime            string     `json:"runtime"`
	RuntimeVersion     string     `json:"runtime_version"`
	Provider           string     `json:"provider"`
	Region             string     `json:"region"`
	GPU                string     `json:"gpu"`
	ComputeMode        string     `json:"compute_mode"`
	QualificationState string     `json:"qualification_state"`
	CapacityState      string     `json:"capacity_state"`
	CapacitySource     string     `json:"capacity_source"`
	Qualified          bool       `json:"qualified"`
	ComparableModel    bool       `json:"comparable_model"`
	ComparableWorkload bool       `json:"comparable_workload"`
	CapacityObservedAt *time.Time `json:"capacity_observed_at,omitempty"`
	CapacityExpiresAt  *time.Time `json:"capacity_expires_at,omitempty"`
	Requests           int        `json:"requests"`
	Failed             int        `json:"failed"`
	TTFTP95MS          *float64   `json:"ttft_p95_ms,omitempty"`
	LatencyP95MS       *float64   `json:"latency_p95_ms,omitempty"`
	OutputTokensSecond *float64   `json:"output_tokens_second,omitempty"`
	HourlyCost         *float64   `json:"hourly_cost,omitempty"`
	CostSource         string     `json:"cost_source,omitempty"`
	CostObservedAt     *time.Time `json:"cost_observed_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
}

type Candidate struct {
	EvidenceID       string     `json:"evidence_id"`
	Configuration    string     `json:"configuration"`
	Qualification    string     `json:"qualification_state,omitempty"`
	CapacityState    string     `json:"capacity_state,omitempty"`
	CapacitySource   string     `json:"capacity_source,omitempty"`
	CapacityObserved *time.Time `json:"capacity_observed_at,omitempty"`
	CapacityExpires  *time.Time `json:"capacity_expires_at,omitempty"`
	Eligible         bool       `json:"eligible"`
	Missing          []string   `json:"missing,omitempty"`
	Violations       []string   `json:"violations,omitempty"`
	Disclosures      []string   `json:"disclosures,omitempty"`
	Score            float64    `json:"score,omitempty"`
}

type Result struct {
	Status           string      `json:"status"`
	AlgorithmVersion string      `json:"algorithm_version"`
	SelectedEvidence string      `json:"selected_evidence_id,omitempty"`
	Reason           string      `json:"reason"`
	Missing          []string    `json:"missing,omitempty"`
	Candidates       []Candidate `json:"candidates"`
}

func Recommend(policy SLOPolicy, evidence []Evidence) Result {
	result := Result{Status: "unknown", AlgorithmVersion: AlgorithmVersion, Reason: "insufficient trustworthy benchmark evidence"}
	if len(evidence) == 0 {
		result.Missing = []string{"benchmark_evidence"}
		return result
	}
	for _, row := range evidence {
		candidate := evaluate(policy, row)
		result.Candidates = append(result.Candidates, candidate)
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		a, b := result.Candidates[i], result.Candidates[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.EvidenceID < b.EvidenceID
	})
	for _, candidate := range result.Candidates {
		if candidate.Eligible {
			result.Status, result.SelectedEvidence = "recommended", candidate.EvidenceID
			result.Reason = "highest deterministic score among candidates satisfying the explicit SLO policy"
			return result
		}
	}
	missing := map[string]struct{}{}
	for _, candidate := range result.Candidates {
		for _, field := range candidate.Missing {
			missing[field] = struct{}{}
		}
	}
	for field := range missing {
		result.Missing = append(result.Missing, field)
	}
	sort.Strings(result.Missing)
	if len(result.Missing) == 0 {
		result.Status, result.Reason = "no_match", "all measured candidates violate the explicit SLO policy"
	}
	return result
}

func evaluate(policy SLOPolicy, row Evidence) Candidate {
	c := Candidate{EvidenceID: row.ID, Configuration: fmt.Sprintf("%s/%s/%s/%s/%s", row.Provider, row.Region, row.GPU, row.Runtime, row.ComputeMode), Qualification: row.QualificationState, CapacityState: row.CapacityState, CapacitySource: row.CapacitySource, CapacityObserved: row.CapacityObservedAt, CapacityExpires: row.CapacityExpiresAt}
	if !row.Qualified {
		c.Missing = append(c.Missing, "qualified_runtime_provider_mode")
	}
	if !row.ComparableModel {
		c.Missing = append(c.Missing, "comparable_model_artifact")
	}
	if !row.ComparableWorkload {
		c.Missing = append(c.Missing, "comparable_benchmark_workload")
	}
	if row.CapacityState == "" || row.CapacityState == "unknown" || row.CapacityObservedAt == nil || row.CapacityExpiresAt == nil {
		c.Disclosures = append(c.Disclosures, "current_capacity_unknown")
	} else if row.CapacityState == "unavailable" {
		c.Violations = append(c.Violations, "current_capacity is unavailable")
	} else if row.CapacityState == "constrained" {
		c.Disclosures = append(c.Disclosures, "current_capacity_constrained")
	}
	if row.Requests <= 0 || row.Failed < 0 || row.Failed > row.Requests {
		c.Missing = append(c.Missing, "valid_request_counts")
	}
	errorRate := 0.0
	if row.Requests > 0 {
		errorRate = float64(row.Failed) / float64(row.Requests)
	}
	checkMax := func(name string, threshold, measured *float64) {
		if threshold == nil {
			return
		}
		if measured == nil || !finite(*measured) {
			c.Missing = append(c.Missing, name)
			return
		}
		if *measured > *threshold {
			c.Violations = append(c.Violations, fmt.Sprintf("%s %.4g exceeds %.4g", name, *measured, *threshold))
		}
	}
	checkMin := func(name string, threshold, measured *float64) {
		if threshold == nil {
			return
		}
		if measured == nil || !finite(*measured) {
			c.Missing = append(c.Missing, name)
			return
		}
		if *measured < *threshold {
			c.Violations = append(c.Violations, fmt.Sprintf("%s %.4g is below %.4g", name, *measured, *threshold))
		}
	}
	checkMax("ttft_p95_ms", policy.MaxTTFTP95MS, row.TTFTP95MS)
	checkMax("latency_p95_ms", policy.MaxLatencyP95MS, row.LatencyP95MS)
	if policy.MaxErrorRate != nil && row.Requests > 0 && errorRate > *policy.MaxErrorRate {
		c.Violations = append(c.Violations, fmt.Sprintf("error_rate %.4g exceeds %.4g", errorRate, *policy.MaxErrorRate))
	}
	checkMin("output_tokens_second", policy.MinOutputTokensSecond, row.OutputTokensSecond)
	if policy.MaxHourlyCost != nil {
		if row.HourlyCost == nil || !finite(*row.HourlyCost) || row.CostSource == "" || row.CostObservedAt == nil {
			c.Missing = append(c.Missing, "trustworthy_hourly_cost")
		} else if *row.HourlyCost > *policy.MaxHourlyCost {
			c.Violations = append(c.Violations, fmt.Sprintf("hourly_cost %.4g exceeds %.4g", *row.HourlyCost, *policy.MaxHourlyCost))
		}
	}
	c.Eligible = len(c.Missing) == 0 && len(c.Violations) == 0
	if c.Eligible {
		if policy.MaxTTFTP95MS != nil && row.TTFTP95MS != nil {
			c.Score += *policy.MaxTTFTP95MS / (1 + *row.TTFTP95MS)
		}
		if policy.MaxLatencyP95MS != nil && row.LatencyP95MS != nil {
			c.Score += *policy.MaxLatencyP95MS / (1 + *row.LatencyP95MS)
		}
		if policy.MaxErrorRate != nil {
			c.Score += (1 - *policy.MaxErrorRate) / (1 + errorRate)
		}
		if policy.MinOutputTokensSecond != nil && row.OutputTokensSecond != nil {
			c.Score += *row.OutputTokensSecond / (1 + *policy.MinOutputTokensSecond)
		}
		if policy.MaxHourlyCost != nil && row.HourlyCost != nil {
			c.Score += *policy.MaxHourlyCost / (1 + *row.HourlyCost)
		}
	}
	return c
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func Snapshot(policy SLOPolicy, evidence []Evidence, result Result) (string, error) {
	evidence = append([]Evidence(nil), evidence...)
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].ID < evidence[j].ID })
	value := struct {
		Policy   SLOPolicy  `json:"policy"`
		Evidence []Evidence `json:"evidence"`
		Result   Result     `json:"result"`
	}{policy, evidence, result}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}
