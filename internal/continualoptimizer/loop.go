// Package continualoptimizer owns InferCrane's durable outer optimization
// loop. It does not provision GPUs or mutate traffic. It turns immutable
// workload, SLO, economics, and experiment evidence into a bounded next
// action that the existing optimization-campaign and Release Guard workflows
// can execute.
package continualoptimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	SchemaVersion    = "infercrane.continual-optimizer/v1"
	AlgorithmVersion = "workload-economic-loop-v1"

	SourcePublicPrior      = "public_prior"
	SourceCustomerObserved = "customer_observed"

	ActionWaitForEvidence    = "wait_for_evidence"
	ActionScreenPublicPrior  = "screen_public_prior"
	ActionStartExperiment    = "start_experiment"
	ActionObserveExperiment  = "observe_experiment"
	ActionObserveCanary      = "observe_canary"
	ActionRollbackCanary     = "rollback_canary"
	ActionRecommendPromotion = "recommend_promotion"
	ActionRetainCurrent      = "retain_current"
)

// Policy is durable authority for one optimization loop. Defaults are
// deliberately conservative: public data can screen candidates, customer
// traffic is required for qualification, and production promotion remains an
// explicit operator decision.
type Policy struct {
	Enabled                       bool    `json:"enabled"`
	MinimumObservedRequests       int     `json:"minimum_observed_requests"`
	MinimumCanaryRequests         int     `json:"minimum_canary_requests"`
	DriftThreshold                float64 `json:"drift_threshold"`
	MinimumExpectedGain           float64 `json:"minimum_expected_gain"`
	MinimumMeasuredGain           float64 `json:"minimum_measured_gain"`
	MinimumContributionMargin     float64 `json:"minimum_contribution_margin"`
	MinimumAvailability           float64 `json:"minimum_availability"`
	MaximumErrorRate              float64 `json:"maximum_error_rate"`
	MaximumExperimentCostUSD      float64 `json:"maximum_experiment_cost_usd"`
	MaximumCandidatesPerCampaign  int     `json:"maximum_candidates_per_campaign"`
	MaximumConcurrentCampaigns    int     `json:"maximum_concurrent_campaigns"`
	CooldownSeconds               int     `json:"cooldown_seconds"`
	RejectionMemorySeconds        int     `json:"rejection_memory_seconds"`
	RequireCustomerReplay         bool    `json:"require_customer_replay"`
	RequireManualPromotion        bool    `json:"require_manual_promotion"`
	AllowAutomaticExperimentation bool    `json:"allow_automatic_experimentation"`
}

func DefaultPolicy() Policy {
	return Policy{
		Enabled: true, MinimumObservedRequests: 500, MinimumCanaryRequests: 1000,
		DriftThreshold: .25, MinimumExpectedGain: .03, MinimumMeasuredGain: .03,
		MinimumContributionMargin: .20, MinimumAvailability: .999,
		MaximumErrorRate: .01, MaximumExperimentCostUSD: 100,
		MaximumCandidatesPerCampaign: 4, MaximumConcurrentCampaigns: 1,
		CooldownSeconds: 6 * 60 * 60, RejectionMemorySeconds: 30 * 24 * 60 * 60,
		RequireCustomerReplay: true, RequireManualPromotion: true,
		AllowAutomaticExperimentation: true,
	}
}

// Workload is content-free workload evidence. Ratios are optional: zero means
// unavailable, not an asserted absence of reuse, sessions, or tool pauses.
type Workload struct {
	Source                string    `json:"source"`
	Digest                string    `json:"digest"`
	RequestCount          int       `json:"request_count"`
	WindowStart           time.Time `json:"window_start"`
	WindowEnd             time.Time `json:"window_end"`
	RequestsPerSecond     float64   `json:"requests_per_second"`
	InputTokensMean       float64   `json:"input_tokens_mean"`
	OutputTokensMean      float64   `json:"output_tokens_mean"`
	PeakConcurrency       float64   `json:"peak_concurrency"`
	SharedPrefixRatio     float64   `json:"shared_prefix_ratio,omitempty"`
	SessionReuseRatio     float64   `json:"session_reuse_ratio,omitempty"`
	ToolPauseRequestRatio float64   `json:"tool_pause_request_ratio,omitempty"`
	StreamingRequestRatio float64   `json:"streaming_request_ratio,omitempty"`
}

// ServiceEvidence joins performance with the economics that make optimization
// useful. Productive utilization counts only output delivered within the SLO;
// raw GPU utilization is intentionally not a business objective.
type ServiceEvidence struct {
	RequestCount                   int     `json:"request_count"`
	TTFTP95MS                      float64 `json:"ttft_p95_ms,omitempty"`
	TPOTP95MS                      float64 `json:"tpot_p95_ms,omitempty"`
	Goodput                        float64 `json:"goodput,omitempty"`
	ErrorRate                      float64 `json:"error_rate"`
	Availability                   float64 `json:"availability"`
	OutputTokensPerSecond          float64 `json:"output_tokens_per_second,omitempty"`
	QualifiedOutputTokensPerSecond float64 `json:"qualified_output_tokens_per_second,omitempty"`
	ProductiveUtilization          float64 `json:"productive_utilization,omitempty"`
	CostPerMillionOutputUSD        float64 `json:"cost_per_million_output_usd,omitempty"`
	ContributionMargin             float64 `json:"contribution_margin,omitempty"`
}

// Hypothesis is produced by a model/runtime/hardware planner. The loop ranks
// these proposals but never invents support for an unqualified tuple.
type Hypothesis struct {
	ID                    string  `json:"id"`
	Fingerprint           string  `json:"fingerprint"`
	Layer                 string  `json:"layer"`
	Description           string  `json:"description"`
	ExpectedEndpointGain  float64 `json:"expected_endpoint_gain"`
	Confidence            float64 `json:"confidence"`
	Risk                  float64 `json:"risk"`
	EstimatedCostUSD      float64 `json:"estimated_cost_usd"`
	RequiresProfiler      bool    `json:"requires_profiler,omitempty"`
	ProfilerEvidenceReady bool    `json:"profiler_evidence_ready,omitempty"`
}

type Outcome struct {
	Fingerprint  string    `json:"fingerprint"`
	Decision     string    `json:"decision"`
	MeasuredGain float64   `json:"measured_gain"`
	FailureCode  string    `json:"failure_code,omitempty"`
	CompletedAt  time.Time `json:"completed_at"`
}

type Active struct {
	CampaignID string `json:"campaign_id"`
	Phase      string `json:"phase"`
	Count      int    `json:"count"`
}

type Input struct {
	Now              time.Time        `json:"now"`
	Policy           Policy           `json:"policy"`
	Baseline         *Workload        `json:"baseline,omitempty"`
	Current          Workload         `json:"current"`
	ActiveService    ServiceEvidence  `json:"active_service"`
	CanaryService    *ServiceEvidence `json:"canary_service,omitempty"`
	Hypotheses       []Hypothesis     `json:"hypotheses,omitempty"`
	History          []Outcome        `json:"history,omitempty"`
	ActiveCampaigns  []Active         `json:"active_campaigns,omitempty"`
	LastExperimentAt *time.Time       `json:"last_experiment_at,omitempty"`
}

type RankedHypothesis struct {
	Hypothesis
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
}

type Decision struct {
	SchemaVersion       string             `json:"schema_version"`
	AlgorithmVersion    string             `json:"algorithm_version"`
	InputDigest         string             `json:"input_digest"`
	Action              string             `json:"action"`
	Reasons             []string           `json:"reasons"`
	DriftScore          float64            `json:"drift_score"`
	Selected            []RankedHypothesis `json:"selected,omitempty"`
	WorkloadAuthority   string             `json:"workload_authority"`
	PromotionEligible   bool               `json:"promotion_eligible"`
	AutomaticPromotion  bool               `json:"automatic_promotion"`
	MaximumCostUSD      float64            `json:"maximum_cost_usd,omitempty"`
	BaselineReplacement bool               `json:"baseline_replacement"`
	EvaluatedAt         time.Time          `json:"evaluated_at"`
	DecisionDigest      string             `json:"decision_digest"`
}

func Evaluate(input Input) (Decision, error) {
	input = normalize(input)
	if err := validate(input); err != nil {
		return Decision{}, err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return Decision{}, err
	}
	sum := sha256.Sum256(encoded)
	decision := Decision{
		SchemaVersion: SchemaVersion, AlgorithmVersion: AlgorithmVersion,
		InputDigest: hex.EncodeToString(sum[:]), WorkloadAuthority: input.Current.Source,
		EvaluatedAt: input.Now, AutomaticPromotion: false,
	}

	if !input.Policy.Enabled {
		decision.Action = ActionRetainCurrent
		decision.Reasons = []string{"continual optimization is disabled by policy"}
		return seal(decision), nil
	}
	if input.CanaryService != nil {
		return seal(evaluateCanary(input, decision)), nil
	}
	if len(input.ActiveCampaigns) > 0 {
		decision.Action = ActionObserveExperiment
		decision.Reasons = []string{"a bounded optimization campaign is already active"}
		return seal(decision), nil
	}

	if input.Current.Source == SourcePublicPrior {
		decision.Action = ActionScreenPublicPrior
		decision.Reasons = []string{"public Agentic Systems data is a day-zero screening prior, not production qualification evidence"}
		decision.Selected = rank(input, false)
		decision.MaximumCostUSD = selectedBudget(decision.Selected, input.Policy.MaximumExperimentCostUSD)
		decision.PromotionEligible = false
		return seal(decision), nil
	}

	decision.BaselineReplacement = input.Baseline == nil || input.Baseline.Source != SourceCustomerObserved
	if input.Current.RequestCount < input.Policy.MinimumObservedRequests {
		decision.Action = ActionWaitForEvidence
		decision.Reasons = []string{fmt.Sprintf("customer workload has %d requests; policy requires %d", input.Current.RequestCount, input.Policy.MinimumObservedRequests)}
		decision.PromotionEligible = false
		return seal(decision), nil
	}
	if input.Baseline != nil {
		decision.DriftScore = WorkloadDrift(*input.Baseline, input.Current)
	}

	violations := serviceViolations(input.ActiveService, input.Policy)
	trigger := len(violations) > 0 || decision.DriftScore >= input.Policy.DriftThreshold
	if decision.BaselineReplacement {
		trigger = true
	}
	decision.PromotionEligible = true
	if !trigger {
		decision.Action = ActionRetainCurrent
		decision.Reasons = []string{"customer workload is stable and the active service clears performance, reliability, and margin policy"}
		return seal(decision), nil
	}
	decision.Reasons = append(decision.Reasons, violations...)
	if decision.DriftScore >= input.Policy.DriftThreshold {
		decision.Reasons = append(decision.Reasons, fmt.Sprintf("workload drift %.3f exceeds policy threshold %.3f", decision.DriftScore, input.Policy.DriftThreshold))
	}
	if decision.BaselineReplacement {
		decision.Reasons = append(decision.Reasons, "customer-observed workload replaces the public or stale baseline")
	}
	if input.LastExperimentAt != nil && input.Now.Sub(*input.LastExperimentAt) < time.Duration(input.Policy.CooldownSeconds)*time.Second {
		decision.Action = ActionWaitForEvidence
		decision.Reasons = append(decision.Reasons, "optimization cooldown prevents reacting to short-lived noise")
		return seal(decision), nil
	}
	if !input.Policy.AllowAutomaticExperimentation {
		decision.Action = ActionWaitForEvidence
		decision.Reasons = append(decision.Reasons, "automatic experiment creation is disabled by policy")
		return seal(decision), nil
	}
	decision.Selected = rank(input, true)
	if len(decision.Selected) == 0 {
		decision.Action = ActionWaitForEvidence
		decision.Reasons = append(decision.Reasons, "no novel, qualified, material hypothesis remains inside the experiment budget")
		return seal(decision), nil
	}
	decision.Action = ActionStartExperiment
	decision.MaximumCostUSD = selectedBudget(decision.Selected, input.Policy.MaximumExperimentCostUSD)
	return seal(decision), nil
}

func evaluateCanary(input Input, decision Decision) Decision {
	canary := *input.CanaryService
	decision.WorkloadAuthority = input.Current.Source
	decision.PromotionEligible = input.Current.Source == SourceCustomerObserved && input.Current.RequestCount >= input.Policy.MinimumObservedRequests
	regressions := canaryRegressions(input.ActiveService, canary, input.Policy)
	if len(regressions) > 0 {
		decision.Action = ActionRollbackCanary
		decision.Reasons = regressions
		decision.PromotionEligible = false
		return decision
	}
	if canary.RequestCount < input.Policy.MinimumCanaryRequests {
		decision.Action = ActionObserveCanary
		decision.Reasons = []string{fmt.Sprintf("canary has %d requests; policy requires %d", canary.RequestCount, input.Policy.MinimumCanaryRequests)}
		return decision
	}
	gain := economicUtility(canary) - economicUtility(input.ActiveService)
	if gain < input.Policy.MinimumMeasuredGain {
		decision.Action = ActionRollbackCanary
		decision.Reasons = []string{fmt.Sprintf("canary utility gain %.3f is below required %.3f", gain, input.Policy.MinimumMeasuredGain)}
		decision.PromotionEligible = false
		return decision
	}
	decision.Action = ActionRecommendPromotion
	decision.Reasons = []string{fmt.Sprintf("canary clears correctness, SLO, availability, margin, and %.3f measured utility gain", gain)}
	decision.AutomaticPromotion = decision.PromotionEligible && !input.Policy.RequireManualPromotion
	return decision
}

func rank(input Input, requireMaterial bool) []RankedHypothesis {
	rejected := map[string]Outcome{}
	for _, outcome := range input.History {
		if input.Now.Sub(outcome.CompletedAt) <= time.Duration(input.Policy.RejectionMemorySeconds)*time.Second && (outcome.Decision == "rejected" || outcome.Decision == "failed" || outcome.Decision == "superseded") {
			rejected[outcome.Fingerprint] = outcome
		}
	}
	items := make([]RankedHypothesis, 0, len(input.Hypotheses))
	for _, hypothesis := range input.Hypotheses {
		if _, knownFailure := rejected[hypothesis.Fingerprint]; knownFailure {
			continue
		}
		if hypothesis.EstimatedCostUSD > input.Policy.MaximumExperimentCostUSD || hypothesis.RequiresProfiler && !hypothesis.ProfilerEvidenceReady {
			continue
		}
		if requireMaterial && hypothesis.ExpectedEndpointGain < input.Policy.MinimumExpectedGain {
			continue
		}
		score := hypothesis.ExpectedEndpointGain*hypothesis.Confidence - .35*hypothesis.Risk - .15*(hypothesis.EstimatedCostUSD/input.Policy.MaximumExperimentCostUSD)
		reasons := []string{fmt.Sprintf("expected endpoint gain %.1f%% at %.0f%% confidence", hypothesis.ExpectedEndpointGain*100, hypothesis.Confidence*100)}
		score += workloadFit(hypothesis.Layer, input.Current, &reasons)
		items = append(items, RankedHypothesis{Hypothesis: hypothesis, Score: score, Reasons: reasons})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score == items[j].Score {
			return items[i].Fingerprint < items[j].Fingerprint
		}
		return items[i].Score > items[j].Score
	})
	if len(items) > input.Policy.MaximumCandidatesPerCampaign {
		items = items[:input.Policy.MaximumCandidatesPerCampaign]
	}
	return items
}

func workloadFit(layer string, workload Workload, reasons *[]string) float64 {
	bonus := 0.0
	switch strings.ToLower(layer) {
	case "prefix-cache", "kv-cache":
		if workload.SharedPrefixRatio >= .20 || workload.SessionReuseRatio >= .20 {
			bonus = .08
			*reasons = append(*reasons, "observed prefix or session reuse supports cache optimization")
		}
	case "scheduler", "batching":
		if workload.PeakConcurrency >= 4 || workload.RequestsPerSecond >= 2 {
			bonus = .08
			*reasons = append(*reasons, "observed concurrency supports scheduler or batching optimization")
		}
	case "speculative-decoding", "decode":
		if workload.OutputTokensMean >= 256 {
			bonus = .08
			*reasons = append(*reasons, "decode-heavy workload supports speculative decoding research")
		}
	case "prefill", "chunked-prefill":
		if workload.InputTokensMean >= 4096 {
			bonus = .08
			*reasons = append(*reasons, "long prompts support prefill optimization")
		}
	case "kernel":
		// Kernel work receives no speculative workload bonus. It must enter
		// with profiler evidence and earn its place through Amdahl's law.
	}
	return bonus
}

func serviceViolations(service ServiceEvidence, policy Policy) []string {
	var reasons []string
	if service.RequestCount > 0 && service.ErrorRate > policy.MaximumErrorRate {
		reasons = append(reasons, fmt.Sprintf("error rate %.3f exceeds %.3f", service.ErrorRate, policy.MaximumErrorRate))
	}
	if service.Availability <= 0 {
		reasons = append(reasons, "measured availability evidence is unavailable")
	} else if service.Availability < policy.MinimumAvailability {
		reasons = append(reasons, fmt.Sprintf("availability %.4f is below %.4f", service.Availability, policy.MinimumAvailability))
	}
	if service.CostPerMillionOutputUSD <= 0 {
		reasons = append(reasons, "measured productive-token cost evidence is unavailable")
	}
	if service.QualifiedOutputTokensPerSecond <= 0 {
		reasons = append(reasons, "qualified output-capacity evidence is unavailable")
	}
	if service.ContributionMargin < policy.MinimumContributionMargin {
		reasons = append(reasons, fmt.Sprintf("contribution margin %.3f is below %.3f", service.ContributionMargin, policy.MinimumContributionMargin))
	}
	return reasons
}

func canaryRegressions(active, canary ServiceEvidence, policy Policy) []string {
	reasons := serviceViolations(canary, policy)
	if active.TTFTP95MS > 0 && canary.TTFTP95MS > active.TTFTP95MS*1.10 {
		reasons = append(reasons, "canary p95 TTFT regressed by more than 10%")
	}
	if active.TPOTP95MS > 0 && canary.TPOTP95MS > active.TPOTP95MS*1.10 {
		reasons = append(reasons, "canary p95 TPOT regressed by more than 10%")
	}
	if canary.ErrorRate > active.ErrorRate+.0025 {
		reasons = append(reasons, "canary error rate regressed by more than 0.25 percentage points")
	}
	return reasons
}

// WorkloadDrift is a bounded distance over workload-shape dimensions. Log
// ratios keep a 2x change equally important at small and large token counts.
func WorkloadDrift(baseline, current Workload) float64 {
	weighted := []struct{ weight, value float64 }{
		{.20, logDistance(baseline.InputTokensMean, current.InputTokensMean)},
		{.20, logDistance(baseline.OutputTokensMean, current.OutputTokensMean)},
		{.20, logDistance(baseline.PeakConcurrency, current.PeakConcurrency)},
		{.15, logDistance(baseline.RequestsPerSecond, current.RequestsPerSecond)},
		{.10, ratioDistance(baseline.SharedPrefixRatio, current.SharedPrefixRatio)},
		{.05, ratioDistance(baseline.SessionReuseRatio, current.SessionReuseRatio)},
		{.05, ratioDistance(baseline.ToolPauseRequestRatio, current.ToolPauseRequestRatio)},
		{.05, ratioDistance(baseline.StreamingRequestRatio, current.StreamingRequestRatio)},
	}
	total, weights := 0.0, 0.0
	for _, item := range weighted {
		if item.value < 0 { // unavailable pair
			continue
		}
		total += item.weight * item.value
		weights += item.weight
	}
	if weights == 0 {
		return 0
	}
	return math.Min(1, total/weights)
}

func economicUtility(service ServiceEvidence) float64 {
	throughput := service.QualifiedOutputTokensPerSecond
	if throughput == 0 {
		throughput = service.OutputTokensPerSecond
	}
	costEfficiency := 0.0
	if service.CostPerMillionOutputUSD > 0 {
		costEfficiency = 1 / service.CostPerMillionOutputUSD
	}
	availability := service.Availability
	if availability == 0 {
		availability = 1
	}
	return math.Log1p(throughput)*.35 + math.Log1p(costEfficiency)*.20 + service.ProductiveUtilization*.15 + service.ContributionMargin*.20 + availability*.10
}

func logDistance(left, right float64) float64 {
	if left <= 0 || right <= 0 {
		return -1
	}
	return math.Min(1, math.Abs(math.Log2(right/left))/2)
}

func ratioDistance(left, right float64) float64 {
	if left == 0 && right == 0 {
		return -1
	}
	return math.Min(1, math.Abs(right-left))
}

func selectedBudget(selected []RankedHypothesis, maximum float64) float64 {
	total := 0.0
	for _, item := range selected {
		total += item.EstimatedCostUSD
	}
	return math.Min(maximum, total)
}

func normalize(input Input) Input {
	input.Now = input.Now.UTC()
	input.Current.Source = strings.ToLower(strings.TrimSpace(input.Current.Source))
	input.Current.Digest = strings.ToLower(strings.TrimSpace(input.Current.Digest))
	if input.Baseline != nil {
		copy := *input.Baseline
		copy.Source = strings.ToLower(strings.TrimSpace(copy.Source))
		copy.Digest = strings.ToLower(strings.TrimSpace(copy.Digest))
		input.Baseline = &copy
	}
	for index := range input.Hypotheses {
		item := &input.Hypotheses[index]
		item.ID = strings.TrimSpace(item.ID)
		item.Fingerprint = strings.ToLower(strings.TrimSpace(item.Fingerprint))
		item.Layer = strings.ToLower(strings.TrimSpace(item.Layer))
		item.Description = strings.TrimSpace(item.Description)
	}
	sort.Slice(input.Hypotheses, func(i, j int) bool { return input.Hypotheses[i].Fingerprint < input.Hypotheses[j].Fingerprint })
	sort.Slice(input.History, func(i, j int) bool {
		if input.History[i].CompletedAt.Equal(input.History[j].CompletedAt) {
			return input.History[i].Fingerprint < input.History[j].Fingerprint
		}
		return input.History[i].CompletedAt.Before(input.History[j].CompletedAt)
	})
	return input
}

func validate(input Input) error {
	if input.Now.IsZero() {
		return errors.New("evaluation time is required")
	}
	policy := input.Policy
	if err := ValidatePolicy(policy); err != nil {
		return err
	}
	if err := validateWorkload(input.Current); err != nil {
		return fmt.Errorf("current workload: %w", err)
	}
	if input.Baseline != nil {
		if err := validateWorkload(*input.Baseline); err != nil {
			return fmt.Errorf("baseline workload: %w", err)
		}
	}
	if err := validateServiceEvidence(input.ActiveService); err != nil {
		return fmt.Errorf("active service evidence: %w", err)
	}
	if input.CanaryService != nil {
		if err := validateServiceEvidence(*input.CanaryService); err != nil {
			return fmt.Errorf("canary service evidence: %w", err)
		}
	}
	if len(input.ActiveCampaigns) > policy.MaximumConcurrentCampaigns {
		return errors.New("active campaigns exceed policy authority")
	}
	seen := map[string]struct{}{}
	for _, item := range input.Hypotheses {
		if item.ID == "" || !digest(item.Fingerprint) || item.Layer == "" || item.Description == "" || item.ExpectedEndpointGain <= 0 || item.ExpectedEndpointGain > 10 || item.Confidence <= 0 || item.Confidence > 1 || item.Risk < 0 || item.Risk > 1 || item.EstimatedCostUSD < 0 {
			return errors.New("hypothesis identity, evidence, gain, confidence, risk, or cost is invalid")
		}
		if _, duplicate := seen[item.Fingerprint]; duplicate {
			return errors.New("hypothesis fingerprints must be unique")
		}
		seen[item.Fingerprint] = struct{}{}
	}
	return nil
}

func validateServiceEvidence(evidence ServiceEvidence) error {
	if evidence.RequestCount < 0 {
		return errors.New("request count must be non-negative")
	}
	for _, value := range []float64{evidence.TTFTP95MS, evidence.TPOTP95MS, evidence.Goodput, evidence.OutputTokensPerSecond, evidence.QualifiedOutputTokensPerSecond, evidence.CostPerMillionOutputUSD} {
		if invalidNonNegative(value) {
			return errors.New("performance and cost measurements must be non-negative finite values")
		}
	}
	for _, value := range []float64{evidence.ErrorRate, evidence.Availability, evidence.ProductiveUtilization} {
		if value < 0 || value > 1 || math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("error rate, availability, and productive utilization must be in [0,1]")
		}
	}
	if evidence.ContributionMargin < -1 || evidence.ContributionMargin > 1 || math.IsNaN(evidence.ContributionMargin) || math.IsInf(evidence.ContributionMargin, 0) {
		return errors.New("contribution margin must be a finite value in [-1,1]")
	}
	return nil
}

// ValidatePolicy is the shared persistence and API boundary for durable loop
// authority. It intentionally does not inject defaults: callers must persist
// the exact policy an operator reviewed.
func ValidatePolicy(policy Policy) error {
	if policy.MinimumObservedRequests < 1 || policy.MinimumCanaryRequests < 1 || policy.DriftThreshold <= 0 || policy.DriftThreshold > 1 || policy.MinimumExpectedGain <= 0 || policy.MinimumExpectedGain > 1 || policy.MinimumMeasuredGain <= 0 || policy.MinimumMeasuredGain > 1 || policy.MinimumContributionMargin < -1 || policy.MinimumContributionMargin >= 1 || policy.MinimumAvailability <= 0 || policy.MinimumAvailability > 1 || policy.MaximumErrorRate < 0 || policy.MaximumErrorRate >= 1 || policy.MaximumExperimentCostUSD <= 0 || policy.MaximumCandidatesPerCampaign < 1 || policy.MaximumCandidatesPerCampaign > 100 || policy.MaximumConcurrentCampaigns < 1 || policy.CooldownSeconds < 0 || policy.RejectionMemorySeconds <= 0 || policy.RejectionMemorySeconds > 365*24*60*60 {
		return errors.New("continual optimizer policy is outside safe bounds")
	}
	return nil
}

func validateWorkload(workload Workload) error {
	if workload.Source != SourcePublicPrior && workload.Source != SourceCustomerObserved {
		return errors.New("source must be public_prior or customer_observed")
	}
	if !digest(workload.Digest) || workload.RequestCount < 1 || workload.WindowStart.IsZero() || !workload.WindowEnd.After(workload.WindowStart) || invalidNonNegative(workload.RequestsPerSecond) || invalidNonNegative(workload.InputTokensMean) || invalidNonNegative(workload.OutputTokensMean) || invalidNonNegative(workload.PeakConcurrency) {
		return errors.New("immutable digest, positive request count/window, and finite workload shape are required")
	}
	for _, ratio := range []float64{workload.SharedPrefixRatio, workload.SessionReuseRatio, workload.ToolPauseRequestRatio, workload.StreamingRequestRatio} {
		if ratio < 0 || ratio > 1 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
			return errors.New("workload ratios must be in [0,1]")
		}
	}
	return nil
}

func invalidNonNegative(value float64) bool {
	return value < 0 || math.IsNaN(value) || math.IsInf(value, 0)
}

func digest(value string) bool {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func seal(decision Decision) Decision {
	decision.DecisionDigest = ""
	encoded, _ := json.Marshal(decision)
	sum := sha256.Sum256(encoded)
	decision.DecisionDigest = "sha256:" + hex.EncodeToString(sum[:])
	return decision
}
