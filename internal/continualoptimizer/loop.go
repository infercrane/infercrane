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
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	SchemaVersion = "infercrane.continual-optimizer/v1"
	// AlgorithmVersion v2 acts on named triggers. Evidence the control plane
	// does not have stays unknown and never starts a run by itself.
	AlgorithmVersion = "workload-economic-loop-v2"

	SourcePublicPrior      = "public_prior"
	SourceCustomerObserved = "customer_observed"

	ActionWaitForEvidence   = "wait_for_evidence"
	ActionScreenPublicPrior = "screen_public_prior"
	ActionStartExperiment   = "start_experiment"
	ActionObserveExperiment = "observe_experiment"
	ActionRetainCurrent     = "retain_current"
)

// Trigger kinds. Schedule, manual, engine release, model revision, and price
// change are detected outside Evaluate from the control plane's own records;
// traffic drift, service level, and first customer traffic are detected by
// Evaluate itself.
const (
	TriggerSchedule      = "schedule"
	TriggerManual        = "manual"
	TriggerEngineRelease = "engine_release"
	TriggerModelRevision = "model_revision"
	TriggerPriceChange   = "price_change"
	TriggerTrafficDrift  = "traffic_drift"
	TriggerServiceLevel  = "service_level"
	TriggerFirstTraffic  = "first_customer_traffic"

	// DefaultCheckInterval is the scheduled check when a policy names none.
	DefaultCheckInterval = 7 * 24 * time.Hour
	maxTriggers          = 16
)

var externalTriggers = map[string]struct{}{TriggerSchedule: {}, TriggerManual: {}, TriggerEngineRelease: {}, TriggerModelRevision: {}, TriggerPriceChange: {}}

// Trigger is one reason to look for a better setup. Key identifies the change
// it reacts to, so the same engine release or model revision is acted on once.
type Trigger struct {
	Kind   string `json:"kind"`
	Key    string `json:"key,omitempty"`
	Detail string `json:"detail"`
}

// Policy is durable authority for one optimization loop. Defaults are
// deliberately conservative: public data can screen candidates and customer
// traffic is required for qualification. The loop never judges or promotes a
// canary; Release Guard owns that decision.
type Policy struct {
	Enabled                      bool    `json:"enabled"`
	MinimumObservedRequests      int     `json:"minimum_observed_requests"`
	DriftThreshold               float64 `json:"drift_threshold"`
	MinimumExpectedGain          float64 `json:"minimum_expected_gain"`
	MinimumContributionMargin    float64 `json:"minimum_contribution_margin"`
	MinimumAvailability          float64 `json:"minimum_availability"`
	MaximumErrorRate             float64 `json:"maximum_error_rate"`
	MaximumExperimentCostUSD     float64 `json:"maximum_experiment_cost_usd"`
	MaximumCandidatesPerCampaign int     `json:"maximum_candidates_per_campaign"`
	MaximumConcurrentCampaigns   int     `json:"maximum_concurrent_campaigns"`
	CooldownSeconds              int     `json:"cooldown_seconds"`
	RejectionMemorySeconds       int     `json:"rejection_memory_seconds"`
	RequireCustomerReplay        bool    `json:"require_customer_replay"`
	// AllowAutomaticExperimentation lets a check propose a run. A proposed
	// run waits for a person's approval and spend cap unless AutoRun is on.
	AllowAutomaticExperimentation bool `json:"allow_automatic_experimentation"`
	// CheckIntervalSeconds is how often the scheduled check runs. Zero means
	// DefaultCheckInterval (weekly).
	CheckIntervalSeconds int `json:"check_interval_seconds,omitempty"`
	// AutoRun approves a proposed run without a person, only when the run's
	// cost is known and fits what is left of MonthlyAutoRunCapUSD this
	// calendar month (UTC). Off by default. Rollout still needs Release Guard
	// and a person.
	AutoRun              bool    `json:"auto_run,omitempty"`
	MonthlyAutoRunCapUSD float64 `json:"monthly_auto_run_cap_usd,omitempty"`
	// QualityEvalSet names an uploaded eval set (sha256:DIGEST) whose cases
	// replace the built-in prompts in the answer check of every proposed run.
	// Empty uses the built-in prompts.
	QualityEvalSet string `json:"quality_eval_set,omitempty"`
}

// CheckInterval is the scheduled check period.
func (p Policy) CheckInterval() time.Duration {
	if p.CheckIntervalSeconds <= 0 {
		return DefaultCheckInterval
	}
	return time.Duration(p.CheckIntervalSeconds) * time.Second
}

func DefaultPolicy() Policy {
	return Policy{
		Enabled: true, MinimumObservedRequests: 500,
		DriftThreshold: .25, MinimumExpectedGain: .03,
		MinimumContributionMargin: .20, MinimumAvailability: .999,
		MaximumErrorRate: .01, MaximumExperimentCostUSD: 100,
		MaximumCandidatesPerCampaign: 4, MaximumConcurrentCampaigns: 1,
		CooldownSeconds: 6 * 60 * 60, RejectionMemorySeconds: 30 * 24 * 60 * 60,
		RequireCustomerReplay: true, AllowAutomaticExperimentation: true,
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
	// ContributionMargin needs the customer's revenue, which the control
	// plane does not record. Nil means unknown, never zero.
	ContributionMargin *float64 `json:"contribution_margin,omitempty"`
}

// Hypothesis is produced by a model/runtime/hardware planner. The loop ranks
// these proposals but never invents support for an unqualified tuple. A zero
// ExpectedEndpointGain or EstimatedCostUSD means unknown: the planner had no
// measured or sourced number, so the run measures it.
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
	Now             time.Time       `json:"now"`
	Policy          Policy          `json:"policy"`
	Baseline        *Workload       `json:"baseline,omitempty"`
	Current         Workload        `json:"current"`
	ActiveService   ServiceEvidence `json:"active_service"`
	Hypotheses      []Hypothesis    `json:"hypotheses,omitempty"`
	History         []Outcome       `json:"history,omitempty"`
	ActiveCampaigns []Active        `json:"active_campaigns,omitempty"`
	// PendingProposals are runs this loop proposed that still wait for
	// approval. One waiting proposal is enough; a check never stacks more.
	PendingProposals []Active   `json:"pending_proposals,omitempty"`
	LastExperimentAt *time.Time `json:"last_experiment_at,omitempty"`
	// Triggers detected outside Evaluate for this check.
	Triggers []Trigger `json:"triggers,omitempty"`
	// PlanningIssue says why the planner produced no hypotheses, so a check
	// does not report "nothing left to try" when it could not plan at all.
	PlanningIssue string `json:"planning_issue,omitempty"`
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
	MaximumCostUSD      float64            `json:"maximum_cost_usd,omitempty"`
	BaselineReplacement bool               `json:"baseline_replacement"`
	// Triggers are every reason this check looked for a better setup.
	Triggers []Trigger `json:"triggers,omitempty"`
	// UnknownEvidence names measurements the control plane did not have.
	// Unknown evidence is shown as unknown; it never starts a run.
	UnknownEvidence []string  `json:"unknown_evidence,omitempty"`
	EvaluatedAt     time.Time `json:"evaluated_at"`
	DecisionDigest  string    `json:"decision_digest"`
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
		EvaluatedAt: input.Now,
	}

	if !input.Policy.Enabled {
		decision.Action = ActionRetainCurrent
		decision.Reasons = []string{"continual optimization is disabled by policy"}
		return seal(decision), nil
	}
	if len(input.ActiveCampaigns) > 0 {
		decision.Action = ActionObserveExperiment
		decision.Reasons = []string{"a bounded optimization campaign is already active"}
		return seal(decision), nil
	}
	if len(input.PendingProposals) > 0 {
		decision.Action = ActionObserveExperiment
		decision.Reasons = []string{"a proposed run is already waiting for approval"}
		return seal(decision), nil
	}
	decision.Triggers = append([]Trigger(nil), input.Triggers...)
	decision.UnknownEvidence = unknownEvidence(input.ActiveService)

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
	for _, violation := range serviceViolations(input.ActiveService, input.Policy) {
		decision.Triggers = append(decision.Triggers, Trigger{Kind: TriggerServiceLevel, Detail: violation})
	}
	if decision.DriftScore >= input.Policy.DriftThreshold {
		decision.Triggers = append(decision.Triggers, Trigger{Kind: TriggerTrafficDrift, Key: input.Current.Digest, Detail: fmt.Sprintf("workload drift %.3f exceeds policy threshold %.3f", decision.DriftScore, input.Policy.DriftThreshold)})
		// The run is planned for the new shape, so later drift is measured
		// from it rather than firing again on every check.
		decision.BaselineReplacement = true
	}
	if decision.BaselineReplacement {
		decision.Triggers = append(decision.Triggers, Trigger{Kind: TriggerFirstTraffic, Key: input.Current.Digest, Detail: "customer-observed workload replaces the public or stale baseline"})
	}
	decision.PromotionEligible = true
	if len(decision.Triggers) == 0 {
		decision.Action = ActionRetainCurrent
		decision.Reasons = []string{"nothing changed: no engine release, model revision, price change, traffic shift, or broken limit"}
		return seal(decision), nil
	}
	for _, trigger := range decision.Triggers {
		decision.Reasons = append(decision.Reasons, trigger.Detail)
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
	if len(decision.Selected) == 0 && len(input.Hypotheses) == 0 && input.PlanningIssue != "" {
		decision.Action = ActionWaitForEvidence
		decision.Reasons = append(decision.Reasons, "setups could not be planned: "+input.PlanningIssue)
		return seal(decision), nil
	}
	if len(decision.Selected) == 0 {
		decision.Action = ActionRetainCurrent
		decision.Reasons = append(decision.Reasons, "no setup is left to try: every planned setup was measured, rejected, or dismissed recently, or exceeds the experiment budget")
		return seal(decision), nil
	}
	decision.Action = ActionStartExperiment
	decision.MaximumCostUSD = selectedBudget(decision.Selected, input.Policy.MaximumExperimentCostUSD)
	return seal(decision), nil
}

// unknownEvidence names the service measurements the control plane did not
// have for this check.
func unknownEvidence(service ServiceEvidence) []string {
	var unknown []string
	if service.Availability <= 0 {
		unknown = append(unknown, "availability")
	}
	if service.CostPerMillionOutputUSD <= 0 {
		unknown = append(unknown, "cost_per_million_output_tokens")
	}
	if service.QualifiedOutputTokensPerSecond <= 0 {
		unknown = append(unknown, "qualified_output_capacity")
	}
	if service.ContributionMargin == nil {
		unknown = append(unknown, "contribution_margin")
	}
	return unknown
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
		// An unknown gain (zero) is not refused as immaterial: the run is
		// what measures it.
		if requireMaterial && hypothesis.ExpectedEndpointGain > 0 && hypothesis.ExpectedEndpointGain < input.Policy.MinimumExpectedGain {
			continue
		}
		score := hypothesis.ExpectedEndpointGain*hypothesis.Confidence - .35*hypothesis.Risk - .15*(hypothesis.EstimatedCostUSD/input.Policy.MaximumExperimentCostUSD)
		reasons := []string{fmt.Sprintf("expected endpoint gain %.1f%% at %.0f%% confidence", hypothesis.ExpectedEndpointGain*100, hypothesis.Confidence*100)}
		if hypothesis.ExpectedEndpointGain == 0 {
			reasons = []string{fmt.Sprintf("expected gain unknown until measured; planner confidence %.0f%%", hypothesis.Confidence*100)}
		}
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

// serviceViolations lists measured limit breaches only. Missing evidence is
// reported by unknownEvidence and never counts as a breach.
func serviceViolations(service ServiceEvidence, policy Policy) []string {
	var reasons []string
	if service.RequestCount > 0 && service.ErrorRate > policy.MaximumErrorRate {
		reasons = append(reasons, fmt.Sprintf("error rate %.3f exceeds %.3f", service.ErrorRate, policy.MaximumErrorRate))
	}
	if service.Availability > 0 && service.Availability < policy.MinimumAvailability {
		reasons = append(reasons, fmt.Sprintf("availability %.4f is below %.4f", service.Availability, policy.MinimumAvailability))
	}
	if service.ContributionMargin != nil && *service.ContributionMargin < policy.MinimumContributionMargin {
		reasons = append(reasons, fmt.Sprintf("contribution margin %.3f is below %.3f", *service.ContributionMargin, policy.MinimumContributionMargin))
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
	if len(input.ActiveCampaigns) > policy.MaximumConcurrentCampaigns {
		return errors.New("active campaigns exceed policy authority")
	}
	if len(input.Triggers) > maxTriggers {
		return errors.New("too many triggers")
	}
	if len(input.PlanningIssue) > 512 {
		return errors.New("planning issue is too long")
	}
	for _, trigger := range input.Triggers {
		if _, known := externalTriggers[trigger.Kind]; !known || trigger.Detail == "" || len(trigger.Detail) > 512 || len(trigger.Key) > 256 {
			return errors.New("trigger kind, key, or detail is invalid")
		}
	}
	seen := map[string]struct{}{}
	for _, item := range input.Hypotheses {
		if item.ID == "" || !digest(item.Fingerprint) || item.Layer == "" || item.Description == "" || item.ExpectedEndpointGain < 0 || item.ExpectedEndpointGain > 10 || math.IsNaN(item.ExpectedEndpointGain) || math.IsNaN(item.EstimatedCostUSD) || math.IsInf(item.EstimatedCostUSD, 0) || item.Confidence <= 0 || item.Confidence > 1 || item.Risk < 0 || item.Risk > 1 || item.EstimatedCostUSD < 0 {
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
	if margin := evidence.ContributionMargin; margin != nil && (*margin < -1 || *margin > 1 || math.IsNaN(*margin) || math.IsInf(*margin, 0)) {
		return errors.New("contribution margin must be a finite value in [-1,1]")
	}
	return nil
}

// ValidatePolicy is the shared persistence and API boundary for durable loop
// authority. It intentionally does not inject defaults: callers must persist
// the exact policy an operator reviewed.
func ValidatePolicy(policy Policy) error {
	if policy.MinimumObservedRequests < 1 || policy.DriftThreshold <= 0 || policy.DriftThreshold > 1 || policy.MinimumExpectedGain <= 0 || policy.MinimumExpectedGain > 1 || policy.MinimumContributionMargin < -1 || policy.MinimumContributionMargin >= 1 || policy.MinimumAvailability <= 0 || policy.MinimumAvailability > 1 || policy.MaximumErrorRate < 0 || policy.MaximumErrorRate >= 1 || policy.MaximumExperimentCostUSD <= 0 || policy.MaximumCandidatesPerCampaign < 1 || policy.MaximumCandidatesPerCampaign > 100 || policy.MaximumConcurrentCampaigns < 1 || policy.CooldownSeconds < 0 || policy.RejectionMemorySeconds <= 0 || policy.RejectionMemorySeconds > 365*24*60*60 {
		return errors.New("continual optimizer policy is outside safe bounds")
	}
	if policy.CheckIntervalSeconds != 0 && (policy.CheckIntervalSeconds < 60*60 || policy.CheckIntervalSeconds > 30*24*60*60) {
		return errors.New("check interval must be between one hour and 30 days")
	}
	if policy.MonthlyAutoRunCapUSD < 0 || policy.MonthlyAutoRunCapUSD > 1_000_000 || math.IsNaN(policy.MonthlyAutoRunCapUSD) || math.IsInf(policy.MonthlyAutoRunCapUSD, 0) {
		return errors.New("monthly auto-run cap must be between 0 and 1,000,000 USD")
	}
	if policy.AutoRun && policy.MonthlyAutoRunCapUSD <= 0 {
		return errors.New("auto-run needs a monthly spend cap above zero")
	}
	if policy.QualityEvalSet != "" && !evalSetDigest.MatchString(policy.QualityEvalSet) {
		return errors.New("quality eval set must be the sha256:<64 lowercase hex> digest of an uploaded eval set")
	}
	return nil
}

var evalSetDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

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
