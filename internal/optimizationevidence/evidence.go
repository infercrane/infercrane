// Package optimizationevidence defines the immutable, model- and hardware-neutral
// evidence boundary between an optimization worker and InferCrane's control plane.
// It deliberately separates hard qualification from candidate ranking.
package optimizationevidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	Schema           = "infercrane.dev/optimization-evidence/v1"
	EvaluationSchema = "infercrane.dev/optimization-evaluation/v1"
	AlgorithmVersion = "qualified-pareto-v1"
	MaxDocumentBytes = 4 << 20

	// BaselineAlgorithmVersion names the rule that compares candidates with a
	// declared baseline. It is reported only when the policy names a baseline.
	BaselineAlgorithmVersion = "baseline-noise-floor-v1"
	// DefaultMinimumRelativeGain is the smallest relative improvement over the
	// baseline, on the selection objective, that counts as a win. It sits just
	// above the largest spread recorded for one unchanged recipe: 4.9% in mean
	// COGS across six H200 screening receipts. A policy may raise it, never
	// lower it.
	DefaultMinimumRelativeGain = 0.05
)

// Baseline decision outcomes.
const (
	DecisionPromote      = "promote"
	DecisionKeepBaseline = "keep_baseline"
	DecisionRejectAll    = "reject_all"
)

type EvidenceLevel string

const (
	LevelScreening     EvidenceLevel = "screening"
	LevelQualification EvidenceLevel = "qualification"
	LevelPublic        EvidenceLevel = "public"
)

type EvidenceState string

const (
	StateExternalUnverified EvidenceState = "external_unverified"
	StateReproduced         EvidenceState = "reproduced"
	StateQualified          EvidenceState = "qualified"
	StateOptimizedQualified EvidenceState = "optimized_qualified"
	StateServerlessRecipe   EvidenceState = "serverless_recipe"
)

type SLOPolicy struct {
	Scope                      string   `json:"scope"`
	MaxTTFTMS                  float64  `json:"max_ttft_ms"`
	MaxITLMS                   float64  `json:"max_itl_ms"`
	MinSuccessfulFraction      float64  `json:"min_successful_fraction"`
	MaxErrorRate               float64  `json:"max_error_rate"`
	MaxPromptTokenMismatchRate float64  `json:"max_prompt_token_mismatch_rate"`
	RequiredQualityGates       []string `json:"required_quality_gates"`
	MinimumRequestsPerLane     int      `json:"minimum_requests_per_lane"`
	MinimumIndependentRuns     int      `json:"minimum_independent_runs"`
	MinimumMeasurementSeconds  float64  `json:"minimum_measurement_seconds_per_lane,omitempty"`
}

type SelectionPolicy struct {
	ID                         string        `json:"id"`
	Version                    int           `json:"version"`
	Objective                  string        `json:"objective"`
	LaneAggregation            string        `json:"lane_aggregation"`
	EvidenceLevel              EvidenceLevel `json:"evidence_level"`
	QualificationBeforeScoring bool          `json:"qualification_before_scoring"`
	CostTieBreaker             bool          `json:"cost_tie_breaker"`
	// BaselineCandidateID optionally names the candidate that is the current
	// setup. When it is set, another candidate is selected only if it beats the
	// baseline by more than the noise floor; otherwise the baseline is kept.
	BaselineCandidateID string `json:"baseline_candidate_id,omitempty"`
	// MinimumRelativeGain optionally raises the noise floor above
	// DefaultMinimumRelativeGain. Zero means the default.
	MinimumRelativeGain float64 `json:"minimum_relative_gain,omitempty"`
}

// NoiseFloor is the relative gain over the baseline that a challenger must
// exceed to be promoted.
func (p SelectionPolicy) NoiseFloor() float64 {
	return math.Max(p.MinimumRelativeGain, DefaultMinimumRelativeGain)
}

// Minimizes reports whether a lower score is better for the policy objective.
func (p SelectionPolicy) Minimizes() bool {
	return p.Objective == ObjectiveMinimumCOGS || p.Objective == ObjectiveMinimumLatency
}

const (
	ObjectiveQualifiedOutputThroughput = "maximum_slo_qualified_output_throughput"
	ObjectiveSLOGoodput                = "maximum_slo_goodput"
	ObjectiveQualifiedOutputPerDollar  = "maximum_qualified_output_per_dollar"
	ObjectiveMinimumCOGS               = "minimum_cogs_under_slo"
	ObjectiveMinimumLatency            = "minimum_latency_under_slo"
)

type Campaign struct {
	SchemaVersion    string          `json:"schema_version"`
	ModelIdentity    string          `json:"model_identity"`
	HardwareIdentity string          `json:"hardware_identity"`
	WorkloadID       string          `json:"workload_id"`
	WorkloadDigest   string          `json:"workload_digest"`
	RequiredLanes    []int           `json:"required_concurrency_lanes"`
	SLO              SLOPolicy       `json:"slo"`
	Selection        SelectionPolicy `json:"selection_policy"`
	Candidates       []Candidate     `json:"candidates"`
}

type QualityGate struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

type Candidate struct {
	ID                        string         `json:"id"`
	RuntimeID                 string         `json:"runtime_id"`
	Origin                    string         `json:"origin"`
	EvidenceLevel             EvidenceLevel  `json:"evidence_level"`
	EvidenceState             EvidenceState  `json:"evidence_state"`
	SourceRecipeEvidenceState EvidenceState  `json:"source_recipe_evidence_state,omitempty"`
	RecipeDigest              string         `json:"recipe_digest"`
	WorkloadDigest            string         `json:"workload_digest"`
	QualityPassed             bool           `json:"quality_passed"`
	QualityGates              []QualityGate  `json:"quality_gates"`
	ErrorRate                 float64        `json:"error_rate"`
	PromptTokenMismatchRate   float64        `json:"prompt_token_mismatch_rate"`
	Lanes                     []LaneEvidence `json:"lanes"`
}

type LaneEvidence struct {
	Concurrency                          int     `json:"concurrency"`
	Requests                             int     `json:"requests"`
	SuccessfulRequests                   int     `json:"successful_requests"`
	IndependentRuns                      int     `json:"independent_runs"`
	MeasurementSeconds                   float64 `json:"measurement_seconds"`
	SLOAttainment                        float64 `json:"slo_attainment"`
	TTFTP95MS                            float64 `json:"ttft_p95_ms"`
	ITLP95MS                             float64 `json:"itl_p95_ms"`
	AggregateOutputTokensSecond          float64 `json:"aggregate_output_tokens_per_second"`
	RequestThroughput                    float64 `json:"request_throughput_requests_per_second"`
	SLOGoodput                           float64 `json:"slo_goodput_requests_per_second"`
	SLOQualifiedOutputTokensSecond       float64 `json:"slo_qualified_output_tokens_per_second"`
	GPUSecondsPerSuccessfulRequest       float64 `json:"gpu_seconds_per_successful_request"`
	CostPerMillionSuccessfulOutputTokens float64 `json:"cost_per_1m_successful_output_tokens_usd"`
	// InputTokens, OutputTokens, and Economics are producer-recorded context.
	// They are validated but never used for qualification or scoring.
	InputTokens  int64          `json:"input_tokens,omitempty"`
	OutputTokens int64          `json:"output_tokens,omitempty"`
	Economics    *LaneEconomics `json:"economics,omitempty"`
}

// LaneEconomics is a producer's price projection for one measured lane. It
// depends on the producer's price assumptions and is not sourced cost evidence.
type LaneEconomics struct {
	BreakEvenUtilization                         float64 `json:"break_even_utilization"`
	CanReachTargetMarginAtFullUtilization        bool    `json:"can_reach_target_margin_at_full_utilization"`
	InputOutputTokenRatio                        float64 `json:"input_output_token_ratio"`
	InputPriceUSDPerMillion                      float64 `json:"input_price_usd_per_million"`
	MeasuredGPUCostPerMillionOutputEquivalentUSD float64 `json:"measured_gpu_cost_per_million_output_equivalent_usd"`
	MeasuredGPUCostUSD                           float64 `json:"measured_gpu_cost_usd"`
	MeasuredOutputTokensPerSecond                float64 `json:"measured_output_tokens_per_second"`
	MeasuredRevenueUSD                           float64 `json:"measured_revenue_usd"`
	NonGPURevenueFraction                        float64 `json:"non_gpu_revenue_fraction"`
	OutputPriceUSDPerMillion                     float64 `json:"output_price_usd_per_million"`
	PassesTargetMarginAtTargetUtilization        bool    `json:"passes_target_margin_at_target_utilization"`
	ProjectedGrossMargin                         float64 `json:"projected_gross_margin"`
	RequiredOutputTokensPerSecondForTargetMargin float64 `json:"required_output_tokens_per_second_for_target_margin"`
	TargetGrossMargin                            float64 `json:"target_gross_margin"`
	TargetMarginUtilization                      float64 `json:"target_margin_utilization"`
	TargetUtilization                            float64 `json:"target_utilization"`
}

func (e LaneEconomics) validate() error {
	if math.IsNaN(e.ProjectedGrossMargin) || math.IsInf(e.ProjectedGrossMargin, 0) {
		return errors.New("economics projected_gross_margin must be finite")
	}
	for name, value := range map[string]float64{
		"break_even_utilization": e.BreakEvenUtilization, "input_output_token_ratio": e.InputOutputTokenRatio,
		"input_price_usd_per_million": e.InputPriceUSDPerMillion, "measured_gpu_cost_per_million_output_equivalent_usd": e.MeasuredGPUCostPerMillionOutputEquivalentUSD,
		"measured_gpu_cost_usd": e.MeasuredGPUCostUSD, "measured_output_tokens_per_second": e.MeasuredOutputTokensPerSecond,
		"measured_revenue_usd": e.MeasuredRevenueUSD, "non_gpu_revenue_fraction": e.NonGPURevenueFraction,
		"output_price_usd_per_million": e.OutputPriceUSDPerMillion, "required_output_tokens_per_second_for_target_margin": e.RequiredOutputTokensPerSecondForTargetMargin,
		"target_gross_margin": e.TargetGrossMargin, "target_margin_utilization": e.TargetMarginUtilization, "target_utilization": e.TargetUtilization,
	} {
		if !finiteNonnegative(value) {
			return fmt.Errorf("economics %s must be finite and nonnegative", name)
		}
	}
	return nil
}

type Evaluation struct {
	SchemaVersion    string          `json:"schema_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
	InputDigest      string          `json:"input_digest"`
	ModelIdentity    string          `json:"model_identity"`
	HardwareIdentity string          `json:"hardware_identity"`
	WorkloadDigest   string          `json:"workload_digest"`
	SelectionPolicy  SelectionPolicy `json:"selection_policy"`
	// WinnerID is the candidate to serve. With a declared baseline it is the
	// promoted candidate, the kept baseline, or empty when nothing qualified.
	WinnerID           string            `json:"winner_id,omitempty"`
	ParetoCandidateIDs []string          `json:"pareto_candidate_ids"`
	Candidates         []CandidateResult `json:"candidates"`
	// BaselineDecision is present only when the policy names a baseline.
	BaselineDecision *BaselineDecision `json:"baseline_decision,omitempty"`
}

// BaselineDecision records whether any candidate beat the declared baseline by
// more than the noise floor on the selection objective.
type BaselineDecision struct {
	AlgorithmVersion    string  `json:"algorithm_version"`
	BaselineCandidateID string  `json:"baseline_candidate_id"`
	BaselineQualified   bool    `json:"baseline_qualified"`
	NoiseFloor          float64 `json:"noise_floor"`
	Outcome             string  `json:"outcome"`
	// CandidateID is the promoted candidate or the kept baseline.
	CandidateID string `json:"candidate_id,omitempty"`
	// ChallengerID is the best qualified candidate other than the baseline.
	ChallengerID string `json:"challenger_id,omitempty"`
	// RelativeGain is the challenger's gain over the baseline, positive when
	// better. It is absent when the baseline has no positive score.
	RelativeGain *float64 `json:"relative_gain,omitempty"`
	Reason       string   `json:"reason"`
}

type CandidateResult struct {
	CandidateID                              string        `json:"candidate_id"`
	RuntimeID                                string        `json:"runtime_id"`
	Origin                                   string        `json:"origin"`
	EvidenceLevel                            EvidenceLevel `json:"evidence_level"`
	EvidenceState                            EvidenceState `json:"evidence_state"`
	RecipeDigest                             string        `json:"recipe_digest"`
	Qualified                                bool          `json:"qualified"`
	RejectionReasons                         []string      `json:"rejection_reasons,omitempty"`
	Score                                    *float64      `json:"score,omitempty"`
	MeanSLOQualifiedOutputTokensSecond       *float64      `json:"mean_slo_qualified_output_tokens_per_second,omitempty"`
	MeanSLOGoodput                           *float64      `json:"mean_slo_goodput_requests_per_second,omitempty"`
	MeanCostPerMillionSuccessfulOutputTokens *float64      `json:"mean_cost_per_1m_successful_output_tokens_usd,omitempty"`
	WorstTTFTP95MS                           *float64      `json:"worst_ttft_p95_ms,omitempty"`
	WorstITLP95MS                            *float64      `json:"worst_itl_p95_ms,omitempty"`
	Pareto                                   bool          `json:"pareto"`
	Selected                                 bool          `json:"selected"`
	Baseline                                 bool          `json:"baseline,omitempty"`
	RelativeGainOverBaseline                 *float64      `json:"relative_gain_over_baseline,omitempty"`
}

func Decode(body []byte) (Campaign, error) {
	if len(body) == 0 || len(body) > MaxDocumentBytes {
		return Campaign{}, fmt.Errorf("optimization evidence must be between 1 byte and %d bytes", MaxDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var campaign Campaign
	if err := decoder.Decode(&campaign); err != nil {
		return Campaign{}, fmt.Errorf("decode optimization evidence: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Campaign{}, errors.New("optimization evidence contains trailing JSON")
		}
		return Campaign{}, fmt.Errorf("decode trailing optimization evidence: %w", err)
	}
	if err := campaign.Validate(); err != nil {
		return Campaign{}, err
	}
	return campaign, nil
}

func (c Campaign) Validate() error {
	if c.SchemaVersion != Schema || strings.TrimSpace(c.ModelIdentity) == "" || strings.TrimSpace(c.HardwareIdentity) == "" || strings.TrimSpace(c.WorkloadID) == "" || !isSHA256(c.WorkloadDigest) {
		return errors.New("optimization evidence requires the v1 schema, model identity, hardware identity, workload id, and SHA-256 workload digest")
	}
	if len(c.RequiredLanes) == 0 || len(c.RequiredLanes) > 64 || len(c.Candidates) == 0 || len(c.Candidates) > 100 {
		return errors.New("optimization evidence requires 1-64 concurrency lanes and 1-100 candidates")
	}
	if err := c.SLO.validate(); err != nil {
		return fmt.Errorf("SLO policy: %w", err)
	}
	if err := c.Selection.validate(); err != nil {
		return fmt.Errorf("selection policy: %w", err)
	}
	if err := validateEvidenceLevelMinimums(c.Selection.EvidenceLevel, c.SLO); err != nil {
		return fmt.Errorf("evidence level: %w", err)
	}
	seenLanes := map[int]struct{}{}
	for _, lane := range c.RequiredLanes {
		if lane < 1 || lane > 1_000_000 {
			return errors.New("required concurrency lanes must be between 1 and 1000000")
		}
		if _, exists := seenLanes[lane]; exists {
			return errors.New("required concurrency lanes must be unique")
		}
		seenLanes[lane] = struct{}{}
	}
	seenCandidates := map[string]struct{}{}
	for index, candidate := range c.Candidates {
		if err := candidate.validate(c.WorkloadDigest); err != nil {
			return fmt.Errorf("candidate %d: %w", index, err)
		}
		if _, exists := seenCandidates[candidate.ID]; exists {
			return errors.New("candidate ids must be unique")
		}
		seenCandidates[candidate.ID] = struct{}{}
	}
	if baseline := c.Selection.BaselineCandidateID; baseline != "" {
		if _, exists := seenCandidates[baseline]; !exists {
			return fmt.Errorf("selection policy: baseline candidate %q is not a campaign candidate", baseline)
		}
	}
	return nil
}

// validateEvidenceLevelMinimums prevents a producer from labeling a tiny
// screening sample as qualification or public evidence by weakening its own
// policy. These are format floors, not a claim that every workload has enough
// statistical power at the floor; stricter catalog policies remain expected.
func validateEvidenceLevelMinimums(level EvidenceLevel, policy SLOPolicy) error {
	switch level {
	case LevelScreening:
		if policy.MinimumRequestsPerLane < 12 {
			return errors.New("screening requires at least 12 requests per lane")
		}
	case LevelQualification:
		if policy.MinimumRequestsPerLane < 100 || policy.MinimumIndependentRuns < 2 || policy.MinimumMeasurementSeconds < 300 {
			return errors.New("qualification requires at least 100 requests, two independent runs, and 300 seconds per lane")
		}
	case LevelPublic:
		if policy.MinimumRequestsPerLane < 300 || policy.MinimumIndependentRuns < 3 || policy.MinimumMeasurementSeconds < 600 {
			return errors.New("public evidence requires at least 300 requests, three independent runs, and 600 seconds per lane")
		}
	default:
		return errors.New("unsupported evidence level")
	}
	return nil
}

func (p SLOPolicy) validate() error {
	if p.Scope != "each_concurrency_lane" || !finitePositive(p.MaxTTFTMS) || !finitePositive(p.MaxITLMS) || !fraction(p.MinSuccessfulFraction) || !fraction(p.MaxErrorRate) || !fraction(p.MaxPromptTokenMismatchRate) {
		return errors.New("each-lane scope, positive TTFT/ITL limits, and bounded fractions are required")
	}
	if p.MinimumRequestsPerLane < 1 || p.MinimumIndependentRuns < 1 || !finiteNonnegative(p.MinimumMeasurementSeconds) {
		return errors.New("positive request/run minima and a finite measurement duration are required")
	}
	if len(p.RequiredQualityGates) == 0 {
		return errors.New("at least one quality gate is required")
	}
	seen := map[string]struct{}{}
	for _, gate := range p.RequiredQualityGates {
		gate = strings.TrimSpace(gate)
		if gate == "" {
			return errors.New("quality gate names cannot be empty")
		}
		if _, exists := seen[gate]; exists {
			return errors.New("quality gate names must be unique")
		}
		seen[gate] = struct{}{}
	}
	return nil
}

func (p SelectionPolicy) validate() error {
	if strings.TrimSpace(p.ID) == "" || p.Version < 1 || !p.QualificationBeforeScoring || p.LaneAggregation != "arithmetic_mean" {
		return errors.New("identity, positive version, arithmetic-mean lanes, and qualification-before-scoring are required")
	}
	if p.EvidenceLevel != LevelScreening && p.EvidenceLevel != LevelQualification && p.EvidenceLevel != LevelPublic {
		return errors.New("evidence level must be screening, qualification, or public")
	}
	if p.BaselineCandidateID != "" && strings.TrimSpace(p.BaselineCandidateID) != p.BaselineCandidateID {
		return errors.New("baseline candidate id cannot have surrounding whitespace")
	}
	if p.MinimumRelativeGain != 0 {
		if p.BaselineCandidateID == "" {
			return errors.New("minimum relative gain requires a baseline candidate")
		}
		// A producer may demand a larger win, but cannot shrink the floor
		// that keeps measurement noise from being promoted.
		if math.IsNaN(p.MinimumRelativeGain) || p.MinimumRelativeGain < DefaultMinimumRelativeGain || p.MinimumRelativeGain >= 1 {
			return fmt.Errorf("minimum relative gain must be at least %.2f and below 1", DefaultMinimumRelativeGain)
		}
	}
	switch p.Objective {
	case ObjectiveQualifiedOutputThroughput, ObjectiveSLOGoodput, ObjectiveQualifiedOutputPerDollar, ObjectiveMinimumCOGS, ObjectiveMinimumLatency:
		return nil
	default:
		return fmt.Errorf("unsupported objective %q", p.Objective)
	}
}

func (c Candidate) validate(workloadDigest string) error {
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.RuntimeID) == "" || strings.TrimSpace(c.Origin) == "" || !isSHA256(c.RecipeDigest) || c.WorkloadDigest != workloadDigest {
		return errors.New("candidate identity, runtime, origin, recipe digest, and exact workload digest are required")
	}
	switch c.EvidenceState {
	case StateExternalUnverified, StateReproduced, StateQualified, StateOptimizedQualified, StateServerlessRecipe:
	default:
		return errors.New("invalid evidence state")
	}
	if c.EvidenceLevel != LevelScreening && c.EvidenceLevel != LevelQualification && c.EvidenceLevel != LevelPublic {
		return errors.New("candidate evidence level must be screening, qualification, or public")
	}
	if c.SourceRecipeEvidenceState != "" && c.SourceRecipeEvidenceState != StateExternalUnverified && c.SourceRecipeEvidenceState != StateReproduced && c.SourceRecipeEvidenceState != StateQualified && c.SourceRecipeEvidenceState != StateOptimizedQualified && c.SourceRecipeEvidenceState != StateServerlessRecipe {
		return errors.New("invalid source recipe evidence state")
	}
	if !fraction(c.ErrorRate) || !fraction(c.PromptTokenMismatchRate) || len(c.Lanes) == 0 || len(c.Lanes) > 64 {
		return errors.New("bounded error rates and 1-64 lane results are required")
	}
	seenGates := map[string]struct{}{}
	for _, gate := range c.QualityGates {
		gate.Name = strings.TrimSpace(gate.Name)
		if gate.Name == "" {
			return errors.New("quality gate names cannot be empty")
		}
		if _, exists := seenGates[gate.Name]; exists {
			return errors.New("quality gate names must be unique")
		}
		seenGates[gate.Name] = struct{}{}
	}
	seen := map[int]struct{}{}
	for _, lane := range c.Lanes {
		if err := lane.validate(); err != nil {
			return fmt.Errorf("lane %d: %w", lane.Concurrency, err)
		}
		if _, exists := seen[lane.Concurrency]; exists {
			return errors.New("candidate lane concurrency values must be unique")
		}
		seen[lane.Concurrency] = struct{}{}
	}
	return nil
}

func (l LaneEvidence) validate() error {
	if l.Concurrency < 1 || l.Requests < 1 || l.SuccessfulRequests < 0 || l.SuccessfulRequests > l.Requests || l.IndependentRuns < 1 {
		return errors.New("positive concurrency, requests, runs, and bounded successes are required")
	}
	for name, value := range map[string]float64{
		"measurement_seconds": l.MeasurementSeconds, "ttft_p95_ms": l.TTFTP95MS, "itl_p95_ms": l.ITLP95MS,
		"aggregate_output_tokens_per_second": l.AggregateOutputTokensSecond, "request_throughput_requests_per_second": l.RequestThroughput,
		"slo_goodput_requests_per_second": l.SLOGoodput, "slo_qualified_output_tokens_per_second": l.SLOQualifiedOutputTokensSecond,
		"gpu_seconds_per_successful_request": l.GPUSecondsPerSuccessfulRequest, "cost_per_1m_successful_output_tokens_usd": l.CostPerMillionSuccessfulOutputTokens,
	} {
		if !finiteNonnegative(value) {
			return fmt.Errorf("%s must be finite and nonnegative", name)
		}
	}
	if !fraction(l.SLOAttainment) {
		return errors.New("SLO attainment must be between zero and one")
	}
	if l.InputTokens < 0 || l.OutputTokens < 0 {
		return errors.New("input and output tokens must be nonnegative")
	}
	if l.Economics != nil {
		return l.Economics.validate()
	}
	return nil
}

func Evaluate(campaign Campaign) (Evaluation, error) {
	if err := campaign.Validate(); err != nil {
		return Evaluation{}, err
	}
	encoded, err := json.Marshal(campaign)
	if err != nil {
		return Evaluation{}, err
	}
	digest := sha256.Sum256(encoded)
	evaluation := Evaluation{SchemaVersion: EvaluationSchema, AlgorithmVersion: AlgorithmVersion, InputDigest: hex.EncodeToString(digest[:]), ModelIdentity: campaign.ModelIdentity, HardwareIdentity: campaign.HardwareIdentity, WorkloadDigest: campaign.WorkloadDigest, SelectionPolicy: campaign.Selection}
	for _, candidate := range campaign.Candidates {
		evaluation.Candidates = append(evaluation.Candidates, qualifyAndScore(campaign, candidate))
	}
	markPareto(evaluation.Candidates)
	for index := range evaluation.Candidates {
		if evaluation.Candidates[index].Pareto {
			evaluation.ParetoCandidateIDs = append(evaluation.ParetoCandidateIDs, evaluation.Candidates[index].CandidateID)
		}
	}
	sort.Strings(evaluation.ParetoCandidateIDs)
	var winner int
	if campaign.Selection.BaselineCandidateID == "" {
		winner = bestQualified(campaign.Selection, evaluation.Candidates, -1)
	} else {
		var decision BaselineDecision
		decision, winner = decideAgainstBaseline(campaign.Selection, evaluation.Candidates)
		evaluation.BaselineDecision = &decision
	}
	if winner >= 0 {
		evaluation.Candidates[winner].Selected = true
		evaluation.WinnerID = evaluation.Candidates[winner].CandidateID
	}
	sort.Slice(evaluation.Candidates, func(i, j int) bool {
		return evaluation.Candidates[i].CandidateID < evaluation.Candidates[j].CandidateID
	})
	return evaluation, nil
}

func qualifyAndScore(campaign Campaign, candidate Candidate) CandidateResult {
	result := CandidateResult{CandidateID: candidate.ID, RuntimeID: candidate.RuntimeID, Origin: candidate.Origin, EvidenceLevel: candidate.EvidenceLevel, EvidenceState: candidate.EvidenceState, RecipeDigest: candidate.RecipeDigest}
	if candidate.EvidenceLevel != campaign.Selection.EvidenceLevel {
		result.RejectionReasons = append(result.RejectionReasons, "benchmark evidence level does not match selection policy")
	}
	if candidate.EvidenceState == StateExternalUnverified {
		result.RejectionReasons = append(result.RejectionReasons, "external_unverified evidence cannot qualify")
	}
	if candidate.ErrorRate > campaign.SLO.MaxErrorRate {
		result.RejectionReasons = append(result.RejectionReasons, "error rate exceeds policy")
	}
	if candidate.PromptTokenMismatchRate > campaign.SLO.MaxPromptTokenMismatchRate {
		result.RejectionReasons = append(result.RejectionReasons, "prompt token mismatch rate exceeds policy")
	}
	gateResults := map[string]bool{}
	for _, gate := range candidate.QualityGates {
		gateResults[gate.Name] = gate.Passed
	}
	if !candidate.QualityPassed {
		result.RejectionReasons = append(result.RejectionReasons, "quality evaluation failed")
	}
	for _, required := range campaign.SLO.RequiredQualityGates {
		if !gateResults[required] {
			result.RejectionReasons = append(result.RejectionReasons, "required quality gate failed or missing: "+required)
		}
	}
	lanes := map[int]LaneEvidence{}
	for _, lane := range candidate.Lanes {
		lanes[lane.Concurrency] = lane
	}
	var output, goodput, cost, ttft, itl []float64
	for _, concurrency := range campaign.RequiredLanes {
		lane, ok := lanes[concurrency]
		if !ok {
			result.RejectionReasons = append(result.RejectionReasons, "required concurrency lane missing: "+strconv.Itoa(concurrency))
			continue
		}
		prefix := "c" + strconv.Itoa(concurrency) + ": "
		if lane.Requests < campaign.SLO.MinimumRequestsPerLane {
			result.RejectionReasons = append(result.RejectionReasons, prefix+"request count below policy")
		}
		if lane.IndependentRuns < campaign.SLO.MinimumIndependentRuns {
			result.RejectionReasons = append(result.RejectionReasons, prefix+"independent runs below policy")
		}
		if lane.MeasurementSeconds < campaign.SLO.MinimumMeasurementSeconds {
			result.RejectionReasons = append(result.RejectionReasons, prefix+"measurement duration below policy")
		}
		if float64(lane.SuccessfulRequests)/float64(lane.Requests) < campaign.SLO.MinSuccessfulFraction {
			result.RejectionReasons = append(result.RejectionReasons, prefix+"successful fraction below policy")
		}
		if lane.SLOAttainment < campaign.SLO.MinSuccessfulFraction {
			result.RejectionReasons = append(result.RejectionReasons, prefix+"SLO attainment below policy")
		}
		if lane.TTFTP95MS > campaign.SLO.MaxTTFTMS {
			result.RejectionReasons = append(result.RejectionReasons, prefix+"TTFT p95 exceeds policy")
		}
		if lane.ITLP95MS > campaign.SLO.MaxITLMS {
			result.RejectionReasons = append(result.RejectionReasons, prefix+"ITL p95 exceeds policy")
		}
		if campaign.Selection.CostTieBreaker && lane.CostPerMillionSuccessfulOutputTokens <= 0 {
			result.RejectionReasons = append(result.RejectionReasons, prefix+"positive measured COGS required by cost tie-breaker")
		}
		output = append(output, lane.SLOQualifiedOutputTokensSecond)
		goodput = append(goodput, lane.SLOGoodput)
		cost = append(cost, lane.CostPerMillionSuccessfulOutputTokens)
		ttft = append(ttft, lane.TTFTP95MS)
		itl = append(itl, lane.ITLP95MS)
	}
	if len(result.RejectionReasons) != 0 {
		return result
	}
	result.Qualified = true
	meanOutput, meanGoodput, meanCost := mean(output), mean(goodput), mean(cost)
	worstTTFT, worstITL := maximum(ttft), maximum(itl)
	result.MeanSLOQualifiedOutputTokensSecond, result.MeanSLOGoodput, result.MeanCostPerMillionSuccessfulOutputTokens = &meanOutput, &meanGoodput, &meanCost
	result.WorstTTFTP95MS, result.WorstITLP95MS = &worstTTFT, &worstITL
	var score float64
	switch campaign.Selection.Objective {
	case ObjectiveQualifiedOutputThroughput:
		score = meanOutput
	case ObjectiveSLOGoodput:
		score = meanGoodput
	case ObjectiveQualifiedOutputPerDollar:
		if meanCost <= 0 {
			result.Qualified = false
			result.RejectionReasons = append(result.RejectionReasons, "positive measured COGS required for cost-efficiency scoring")
			return result
		}
		score = meanOutput / meanCost
	case ObjectiveMinimumCOGS:
		score = meanCost
	case ObjectiveMinimumLatency:
		score = worstTTFT
	}
	result.Score = &score
	return result
}

// bestQualified returns the index of the best scored candidate, skipping
// the index in except, or -1 when none qualified.
func bestQualified(policy SelectionPolicy, results []CandidateResult, except int) int {
	best := -1
	for index := range results {
		if index == except || !results[index].Qualified || results[index].Score == nil {
			continue
		}
		if best == -1 || better(policy, results[index], results[best]) {
			best = index
		}
	}
	return best
}

// decideAgainstBaseline keeps the declared baseline unless the best other
// qualified candidate beats it by more than the noise floor. A baseline that
// fails qualification cannot be kept, so any qualified challenger replaces it
// with an unknown gain. It returns the decision and the index to serve.
func decideAgainstBaseline(policy SelectionPolicy, results []CandidateResult) (BaselineDecision, int) {
	baseline := -1
	for index := range results {
		if results[index].CandidateID == policy.BaselineCandidateID {
			baseline = index
			results[index].Baseline = true
		}
	}
	decision := BaselineDecision{AlgorithmVersion: BaselineAlgorithmVersion, BaselineCandidateID: policy.BaselineCandidateID, NoiseFloor: policy.NoiseFloor()}
	base := results[baseline]
	decision.BaselineQualified = base.Qualified && base.Score != nil
	challenger := bestQualified(policy, results, baseline)
	if challenger >= 0 {
		decision.ChallengerID = results[challenger].CandidateID
	}
	comparable := decision.BaselineQualified && *base.Score > 0
	if comparable {
		for index := range results {
			if index != baseline && results[index].Qualified && results[index].Score != nil {
				gain := relativeGain(policy, *results[index].Score, *base.Score)
				results[index].RelativeGainOverBaseline = &gain
			}
		}
	}
	objective := policy.Objective
	switch {
	case !decision.BaselineQualified && challenger < 0:
		decision.Outcome = DecisionRejectAll
		decision.Reason = "neither the baseline nor any other candidate qualified"
		return decision, -1
	case !decision.BaselineQualified:
		decision.Outcome, decision.CandidateID = DecisionPromote, decision.ChallengerID
		decision.Reason = fmt.Sprintf("the baseline did not qualify (%s); %s qualified, so its gain over the baseline is unknown", strings.Join(base.RejectionReasons, "; "), decision.ChallengerID)
		return decision, challenger
	case challenger < 0:
		decision.Outcome, decision.CandidateID = DecisionKeepBaseline, base.CandidateID
		decision.Reason = "no other candidate qualified"
		return decision, baseline
	case !comparable:
		decision.Outcome, decision.CandidateID = DecisionKeepBaseline, base.CandidateID
		decision.Reason = fmt.Sprintf("the baseline scored zero on %s, so a relative gain is undefined", objective)
		return decision, baseline
	}
	gain := *results[challenger].RelativeGainOverBaseline
	decision.RelativeGain = &gain
	if gain > decision.NoiseFloor {
		decision.Outcome, decision.CandidateID = DecisionPromote, decision.ChallengerID
		decision.Reason = fmt.Sprintf("%s beat the baseline by %.1f%% on %s, above the %.1f%% noise floor", decision.ChallengerID, gain*100, objective, decision.NoiseFloor*100)
		return decision, challenger
	}
	decision.Outcome, decision.CandidateID = DecisionKeepBaseline, base.CandidateID
	decision.Reason = fmt.Sprintf("the best challenger %s changed %s by %+.1f%%, which does not exceed the %.1f%% noise floor", decision.ChallengerID, objective, gain*100, decision.NoiseFloor*100)
	return decision, baseline
}

// relativeGain is positive when the candidate is better than the baseline.
func relativeGain(policy SelectionPolicy, candidate, baseline float64) float64 {
	if policy.Minimizes() {
		return (baseline - candidate) / baseline
	}
	return (candidate - baseline) / baseline
}

func better(policy SelectionPolicy, candidate, incumbent CandidateResult) bool {
	if candidate.Score == nil || incumbent.Score == nil {
		return candidate.Score != nil
	}
	if *candidate.Score != *incumbent.Score {
		if policy.Minimizes() {
			return *candidate.Score < *incumbent.Score
		}
		return *candidate.Score > *incumbent.Score
	}
	if policy.CostTieBreaker && candidate.MeanCostPerMillionSuccessfulOutputTokens != nil && incumbent.MeanCostPerMillionSuccessfulOutputTokens != nil && *candidate.MeanCostPerMillionSuccessfulOutputTokens != *incumbent.MeanCostPerMillionSuccessfulOutputTokens {
		return *candidate.MeanCostPerMillionSuccessfulOutputTokens < *incumbent.MeanCostPerMillionSuccessfulOutputTokens
	}
	return candidate.CandidateID < incumbent.CandidateID
}

func markPareto(results []CandidateResult) {
	for index := range results {
		if !results[index].Qualified {
			continue
		}
		dominated := false
		for other := range results {
			if other == index || !results[other].Qualified {
				continue
			}
			if dominates(results[other], results[index]) {
				dominated = true
				break
			}
		}
		results[index].Pareto = !dominated
	}
}

func dominates(a, b CandidateResult) bool {
	if a.MeanSLOQualifiedOutputTokensSecond == nil || b.MeanSLOQualifiedOutputTokensSecond == nil || a.MeanCostPerMillionSuccessfulOutputTokens == nil || b.MeanCostPerMillionSuccessfulOutputTokens == nil || a.WorstTTFTP95MS == nil || b.WorstTTFTP95MS == nil || a.WorstITLP95MS == nil || b.WorstITLP95MS == nil {
		return false
	}
	notWorse := *a.MeanSLOQualifiedOutputTokensSecond >= *b.MeanSLOQualifiedOutputTokensSecond && *a.MeanCostPerMillionSuccessfulOutputTokens <= *b.MeanCostPerMillionSuccessfulOutputTokens && *a.WorstTTFTP95MS <= *b.WorstTTFTP95MS && *a.WorstITLP95MS <= *b.WorstITLP95MS
	strict := *a.MeanSLOQualifiedOutputTokensSecond > *b.MeanSLOQualifiedOutputTokensSecond || *a.MeanCostPerMillionSuccessfulOutputTokens < *b.MeanCostPerMillionSuccessfulOutputTokens || *a.WorstTTFTP95MS < *b.WorstTTFTP95MS || *a.WorstITLP95MS < *b.WorstITLP95MS
	return notWorse && strict
}

func mean(values []float64) float64 {
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func maximum(values []float64) float64 {
	value := values[0]
	for _, candidate := range values[1:] {
		value = math.Max(value, candidate)
	}
	return value
}

func finitePositive(value float64) bool { return finiteNonnegative(value) && value > 0 }
func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}
func fraction(value float64) bool { return finiteNonnegative(value) && value <= 1 }

func isSHA256(value string) bool {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
