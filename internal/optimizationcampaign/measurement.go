package optimizationcampaign

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/replay"
)

const (
	// MeasurementMethod identifies how campaign benchmark evidence was
	// produced. It is part of the workload identity, so evidence from an older
	// single-run method can never be ranked against it.
	MeasurementMethod = "campaign-interleaved-control-v1"
	// measurementRunsPerArm is the number of independent AIPerf invocations
	// per arm. Three is the smallest count that yields a Student-t interval
	// with a usable width.
	measurementRunsPerArm = 3
	// maxControlDrift matches kernel qualification: when consecutive control
	// runs differ by more than this in output throughput, the comparison is
	// invalid rather than a measurement of the candidate.
	maxControlDrift   = 0.05
	minWarmupRequests = 16
	maxWarmupRequests = 256
	// measurementSeed is the recorded workload seed. Each invocation's actual
	// seed is drawn per measurement; see invocations.
	measurementSeed = 17
	// replayInvocationTimeout bounds one pass over a replay trace. Traces are
	// capped at replay.MaxAIPerfTraceRequests per pass, and the approval
	// deadline still bounds the whole measurement.
	replayInvocationTimeout = 2 * time.Hour

	ControlRoleCandidate   = "candidate_is_control"
	ControlRoleBracketed   = "interleaved"
	ControlRoleUnavailable = "unavailable"
	ControlRoleInvalid     = "invalid"

	DecisionImproved               = "improved"
	DecisionRegressed              = "regressed"
	DecisionNoMeasurableDifference = "no_measurable_difference"
)

// ReplayTraceSource resolves the content-free trace a customer-observed
// campaign is bound to. Lookups are tenant-scoped.
type ReplayTraceSource interface {
	ReplayTraceByDigest(context.Context, string, string) (domain.ReplayTrace, error)
}

// armTarget is one directly addressed, exact revision endpoint.
type armTarget struct {
	candidateID, revisionID, model string
	endpoint, apiKey, apiKeyEnv    string
	serverMetrics                  bool
}

// loadPlan is the immutable load every arm and run receives.
type loadPlan struct {
	tokenizer                                        string
	requests, concurrency, inputTokens, outputTokens int
	warmup                                           int
	streaming                                        bool
	ttftSLO, tpotSLO                                 float64
	trace                                            *replay.AIPerfTrace
}

// RunSummary is one AIPerf invocation's result, kept for audit.
type RunSummary struct {
	Arm                   string                   `json:"arm"`
	Requests              int                      `json:"requests"`
	Failed                int                      `json:"failed"`
	DurationSeconds       float64                  `json:"duration_seconds"`
	OutputTokenThroughput *float64                 `json:"output_token_throughput,omitempty"`
	RequestThroughput     *float64                 `json:"request_throughput,omitempty"`
	TTFTP95MS             *float64                 `json:"ttft_p95_ms,omitempty"`
	TPOTP95MS             *float64                 `json:"tpot_p95_ms,omitempty"`
	LatencyP95MS          *float64                 `json:"latency_p95_ms,omitempty"`
	Goodput               *float64                 `json:"goodput,omitempty"`
	ServerMetrics         *benchmark.ServerMetrics `json:"server_metrics,omitempty"`
}

// ControlEvidence says whether the campaign control was measured alongside
// the candidate and, if not, why.
type ControlEvidence struct {
	Role          string  `json:"role"`
	CandidateID   string  `json:"candidate_id,omitempty"`
	RevisionID    string  `json:"revision_id,omitempty"`
	Runs          int     `json:"runs"`
	MaxDrift      float64 `json:"max_adjacent_output_throughput_drift,omitempty"`
	DriftLimit    float64 `json:"drift_limit"`
	ServerMetrics bool    `json:"server_metrics_requested"`
	Reason        string  `json:"reason,omitempty"`
}

// MetricComparison is the candidate-versus-control result for one metric.
// The noise floor is the larger of the control-to-control spread and the
// candidate's own 95% interval half-width, both relative to their means. A
// relative delta inside it is no measurable difference.
type MetricComparison struct {
	HigherIsBetter       bool    `json:"higher_is_better"`
	ControlMean          float64 `json:"control_mean"`
	ControlSpread        float64 `json:"control_spread"`
	CandidateMean        float64 `json:"candidate_mean"`
	CandidateUncertainty float64 `json:"candidate_uncertainty"`
	Delta                float64 `json:"delta"`
	NoiseFloor           float64 `json:"noise_floor"`
	Decision             string  `json:"decision"`
}

// MeasuredEvidence is stored under the workload document's "measured" key.
// Lab excludes that key from the workload digest because it holds results.
type MeasuredEvidence struct {
	Method                 string                      `json:"method"`
	InvocationBase         int64                       `json:"invocation_base"`
	Order                  []string                    `json:"order"`
	Runs                   []RunSummary                `json:"runs"`
	Control                ControlEvidence             `json:"control"`
	Comparison             map[string]MetricComparison `json:"comparison,omitempty"`
	ServerMetricsRequested bool                        `json:"server_metrics_requested"`
	GPUTelemetry           string                      `json:"gpu_telemetry"`
}

type measurement struct {
	candidate []benchmark.Result
	evidence  MeasuredEvidence
}

type comparedMetric struct {
	name         string
	higherBetter bool
	value        func(benchmark.Result) *float64
}

var comparedMetrics = []comparedMetric{
	{"output_token_throughput", true, func(r benchmark.Result) *float64 { return r.OutputTokenThroughput }},
	{"request_throughput", true, func(r benchmark.Result) *float64 { return r.RequestThroughput }},
	{"ttft_p95_ms", false, func(r benchmark.Result) *float64 { return r.TTFTP95MS }},
	{"tpot_p95_ms", false, func(r benchmark.Result) *float64 { return r.TPOTP95MS }},
	{"latency_p95_ms", false, func(r benchmark.Result) *float64 { return r.LatencyP95MS }},
	{"goodput", true, func(r benchmark.Result) *float64 { return r.Goodput }},
}

// invocations gives every AIPerf run a distinct seed and, for replay, a
// distinct hash namespace. The base is random per measurement, not a counter
// from zero: the control server is measured again inside every other
// candidate's measurement, and a retried measurement revisits its own
// servers, so counters would resend prompts those servers already cached.
// Each run keeps the trace's own sharing structure.
type invocations struct {
	runner AIPerfRunner
	binary string
	plan   loadPlan
	dir    string
	base   int64
	next   int64
}

// invocationBase draws a base below 2^30, leaving room for the run index in
// AIPerf's 32-bit seed and keeping replay hash IDs below 2^53.
func invocationBase(entropy io.Reader) (int64, error) {
	if entropy == nil {
		entropy = rand.Reader
	}
	var raw [4]byte
	if _, err := io.ReadFull(entropy, raw[:]); err != nil {
		return 0, fmt.Errorf("draw measurement seed base: %w", err)
	}
	return int64(binary.BigEndian.Uint32(raw[:]) >> 2), nil
}

func (i *invocations) run(ctx context.Context, target armTarget, requests int) (benchmark.Result, error) {
	index := i.base + i.next
	i.next++
	streaming := i.plan.streaming
	config := benchmark.Config{
		Binary: i.binary, Endpoint: target.endpoint, APIKey: target.apiKey, APIKeyEnv: target.apiKeyEnv,
		Model: target.model, Tokenizer: i.plan.tokenizer, Requests: requests, Concurrency: i.plan.concurrency,
		RandomSeed: index, Streaming: &streaming, TTFTSLOMS: i.plan.ttftSLO, TPOTSLOMS: i.plan.tpotSLO,
		ServerMetrics: target.serverMetrics,
	}
	if i.plan.trace == nil {
		config.InputTokens, config.OutputTokens = i.plan.inputTokens, i.plan.outputTokens
	} else {
		path := filepath.Join(i.dir, fmt.Sprintf("replay-%02d.jsonl", index))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return benchmark.Result{}, fmt.Errorf("stage replay trace: %w", err)
		}
		writeErr := i.plan.trace.Write(file, int(index))
		closeErr := file.Close()
		if err = errors.Join(writeErr, closeErr); err != nil {
			return benchmark.Result{}, fmt.Errorf("stage replay trace: %w", err)
		}
		config.InputFile, config.DatasetType = path, replay.AIPerfDatasetType
		config.Timeout = replayInvocationTimeout
	}
	result, err := i.runner.Run(ctx, config)
	if err != nil {
		return result, err
	}
	if i.dir != "" {
		result.Command = strings.ReplaceAll(result.Command, i.dir, "./infercrane-replay")
	}
	return result, nil
}

// measureInterleaved warms each server, then runs control and candidate
// alternately (C X C X C X C) so every candidate run is bracketed by control
// runs from the same time window. Without a live control it runs the candidate
// alone and records why the comparison is unavailable. A broken control never
// fails the candidate: it makes the comparison invalid.
func measureInterleaved(ctx context.Context, runs *invocations, candidate armTarget, control *armTarget, controlEvidence ControlEvidence) (measurement, error) {
	result := measurement{evidence: MeasuredEvidence{Method: MeasurementMethod, InvocationBase: runs.base, Control: controlEvidence, ServerMetricsRequested: candidate.serverMetrics, GPUTelemetry: "not_collected"}}
	result.evidence.Control.DriftLimit = maxControlDrift
	var controlRuns []benchmark.Result
	controlFailed := func(reason string) {
		control = nil
		result.evidence.Control.Role, result.evidence.Control.Reason = ControlRoleInvalid, reason
	}
	runControl := func() {
		if control == nil {
			return
		}
		measured, err := runs.run(ctx, *control, runs.plan.requests)
		result.evidence.Order = append(result.evidence.Order, "control")
		if err != nil {
			controlFailed("control run failed: " + boundedReason(err))
			return
		}
		controlRuns = append(controlRuns, measured)
		result.evidence.Runs = append(result.evidence.Runs, summarizeRun("control", measured))
	}
	if control != nil {
		result.evidence.Control.ServerMetrics = control.serverMetrics
		if _, err := runs.run(ctx, *control, runs.plan.warmup); err != nil {
			controlFailed("control warmup failed: " + boundedReason(err))
		} else {
			result.evidence.Order = append(result.evidence.Order, "warmup:control")
		}
	}
	if _, err := runs.run(ctx, candidate, runs.plan.warmup); err != nil {
		return measurement{}, fmt.Errorf("candidate warmup: %w", err)
	}
	result.evidence.Order = append(result.evidence.Order, "warmup:candidate")
	for index := 0; index < measurementRunsPerArm; index++ {
		runControl()
		measured, err := runs.run(ctx, candidate, runs.plan.requests)
		if err != nil {
			return measurement{}, fmt.Errorf("candidate run %d: %w", index+1, err)
		}
		result.evidence.Order = append(result.evidence.Order, "candidate")
		result.candidate = append(result.candidate, measured)
		result.evidence.Runs = append(result.evidence.Runs, summarizeRun("candidate", measured))
	}
	runControl()
	result.evidence.Control.Runs = len(controlRuns)
	if result.evidence.Control.Role == ControlRoleBracketed {
		if reason, drift := validControl(controlRuns); reason != "" {
			result.evidence.Control.Role, result.evidence.Control.Reason, result.evidence.Control.MaxDrift = ControlRoleInvalid, reason, drift
		} else {
			result.evidence.Control.MaxDrift = drift
			result.evidence.Comparison = compareArms(controlRuns, result.candidate)
		}
	}
	return result, nil
}

// validControl applies kernel qualification's control discipline: every
// control run is clean, and consecutive runs agree within the drift limit.
func validControl(runs []benchmark.Result) (string, float64) {
	if len(runs) != measurementRunsPerArm+1 {
		return fmt.Sprintf("control completed %d of %d runs", len(runs), measurementRunsPerArm+1), 0
	}
	drift := 0.0
	for index, run := range runs {
		if run.Failed > 0 || run.Succeeded == 0 || run.OutputTokenThroughput == nil || *run.OutputTokenThroughput <= 0 {
			return "control runs were not clean", 0
		}
		if index > 0 {
			before := *runs[index-1].OutputTokenThroughput
			drift = math.Max(drift, math.Abs(*run.OutputTokenThroughput-before)/before)
		}
	}
	if drift > maxControlDrift {
		return fmt.Sprintf("control output throughput drifted %.1f%% between adjacent runs, above the %.0f%% limit", drift*100, maxControlDrift*100), drift
	}
	return "", drift
}

func compareArms(control, candidate []benchmark.Result) map[string]MetricComparison {
	comparisons := map[string]MetricComparison{}
	for _, metric := range comparedMetrics {
		controlSamples, candidateSamples := samples(control, metric.value), samples(candidate, metric.value)
		if comparison, ok := compareMetric(controlSamples, candidateSamples, metric.higherBetter); ok {
			comparisons[metric.name] = comparison
		}
	}
	if len(comparisons) == 0 {
		return nil
	}
	return comparisons
}

func compareMetric(control, candidate []float64, higherBetter bool) (MetricComparison, bool) {
	if len(control) < 2 || len(candidate) < 2 {
		return MetricComparison{}, false
	}
	controlMean, low, high := 0.0, control[0], control[0]
	for _, value := range control {
		controlMean += value
		low, high = math.Min(low, value), math.Max(high, value)
	}
	controlMean /= float64(len(control))
	interval, ok := benchmark.Confidence95(candidate)
	if !ok || controlMean <= 0 || interval.Mean <= 0 {
		return MetricComparison{}, false
	}
	comparison := MetricComparison{
		HigherIsBetter: higherBetter, ControlMean: controlMean, ControlSpread: (high - low) / controlMean,
		CandidateMean: interval.Mean, CandidateUncertainty: (interval.Upper95 - interval.Mean) / interval.Mean,
		Delta: (interval.Mean - controlMean) / controlMean, Decision: DecisionNoMeasurableDifference,
	}
	comparison.NoiseFloor = math.Max(comparison.ControlSpread, comparison.CandidateUncertainty)
	if math.Abs(comparison.Delta) > comparison.NoiseFloor {
		if (comparison.Delta > 0) == higherBetter {
			comparison.Decision = DecisionImproved
		} else {
			comparison.Decision = DecisionRegressed
		}
	}
	return comparison, true
}

func samples(results []benchmark.Result, value func(benchmark.Result) *float64) []float64 {
	out := make([]float64, 0, len(results))
	for _, result := range results {
		measured := value(result)
		if measured == nil || math.IsNaN(*measured) || math.IsInf(*measured, 0) {
			return nil
		}
		out = append(out, *measured)
	}
	return out
}

// aggregate pools counts and averages per-run metrics. A metric absent from
// any run stays absent rather than averaging a partial set.
func aggregate(results []benchmark.Result) (benchmark.Result, map[string]benchmark.ConfidenceInterval) {
	var pooled benchmark.Result
	if len(results) == 0 {
		return pooled, nil
	}
	pooled.Tool, pooled.ToolVersion, pooled.Command = results[0].Tool, results[0].ToolVersion, results[0].Command
	pooled.ProfileRuns = len(results)
	for _, result := range results {
		pooled.Requests += result.Requests
		pooled.Succeeded += result.Succeeded
		pooled.Failed += result.Failed
		pooled.DurationSeconds += result.DurationSeconds
	}
	mean := func(value func(benchmark.Result) *float64) *float64 {
		values := samples(results, value)
		if len(values) != len(results) {
			return nil
		}
		total := 0.0
		for _, entry := range values {
			total += entry
		}
		total /= float64(len(values))
		return &total
	}
	pooled.RequestThroughput = mean(func(r benchmark.Result) *float64 { return r.RequestThroughput })
	pooled.OutputTokenThroughput = mean(func(r benchmark.Result) *float64 { return r.OutputTokenThroughput })
	pooled.Goodput = mean(func(r benchmark.Result) *float64 { return r.Goodput })
	pooled.TTFTP50MS, pooled.TTFTP95MS = mean(func(r benchmark.Result) *float64 { return r.TTFTP50MS }), mean(func(r benchmark.Result) *float64 { return r.TTFTP95MS })
	pooled.TPOTP50MS, pooled.TPOTP95MS = mean(func(r benchmark.Result) *float64 { return r.TPOTP50MS }), mean(func(r benchmark.Result) *float64 { return r.TPOTP95MS })
	pooled.LatencyP50MS, pooled.LatencyP95MS = mean(func(r benchmark.Result) *float64 { return r.LatencyP50MS }), mean(func(r benchmark.Result) *float64 { return r.LatencyP95MS })
	confidence := map[string]benchmark.ConfidenceInterval{}
	for _, metric := range comparedMetrics {
		if interval, ok := benchmark.Confidence95(samples(results, metric.value)); ok {
			confidence[metric.name] = interval
		}
	}
	if len(confidence) == 0 {
		confidence = nil
	}
	return pooled, confidence
}

func summarizeRun(arm string, result benchmark.Result) RunSummary {
	return RunSummary{Arm: arm, Requests: result.Requests, Failed: result.Failed, DurationSeconds: result.DurationSeconds, OutputTokenThroughput: result.OutputTokenThroughput, RequestThroughput: result.RequestThroughput, TTFTP95MS: result.TTFTP95MS, TPOTP95MS: result.TPOTP95MS, LatencyP95MS: result.LatencyP95MS, Goodput: result.Goodput, ServerMetrics: result.ServerMetrics}
}

func warmupRequests(concurrency int) int {
	return min(max(minWarmupRequests, 2*concurrency), maxWarmupRequests)
}

func boundedReason(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 240 {
		message = message[:240]
	}
	return message
}

// CampaignControl is the candidate every other candidate must measurably
// beat: the proposal's starting point, which recipe search pins to its control
// arm. Its role comes from the immutable proposal, never from measurements.
// Outcome reports use it as the baseline.
func CampaignControl(campaign domain.OptimizationCampaign) (domain.OptimizationCandidateRun, bool) {
	var proposal optimizer.Proposal
	if json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) != nil || proposal.StartingPoint == nil || proposal.StartingPoint.CandidateID == "" {
		return domain.OptimizationCandidateRun{}, false
	}
	for _, candidate := range campaign.Candidates {
		if candidate.ProposalCandidateID == proposal.StartingPoint.CandidateID {
			return candidate, true
		}
	}
	return domain.OptimizationCandidateRun{}, false
}

// resolveBoundReplay returns the verified AIPerf rendering of the Replay trace
// a customer-observed campaign is bound to, or nil when none is bound. Missing,
// malformed, or tampered traces are permanent failures; a store error stays
// retryable.
func resolveBoundReplay(ctx context.Context, source ReplayTraceSource, tenant string, request optimizer.Request) (*replay.AIPerfTrace, error) {
	digest, bound, err := boundReplayDigest(request)
	if err != nil {
		return nil, operations.Permanent("optimization_replay_unavailable", err)
	}
	if !bound {
		return nil, nil
	}
	if source == nil {
		return nil, operations.Permanent("optimization_replay_unavailable", errors.New("campaign replay source is not configured"))
	}
	trace, err := source.ReplayTraceByDigest(ctx, tenant, digest)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, operations.Permanent("optimization_replay_unavailable", errors.New("the campaign's bound Replay trace was not found for this tenant"))
	}
	if err != nil {
		return nil, fmt.Errorf("resolve bound Replay trace: %w", err)
	}
	rendered, err := replay.NewAIPerfTrace(trace)
	if err != nil {
		return nil, operations.Permanent("optimization_replay_invalid", err)
	}
	return &rendered, nil
}

// boundReplayDigest returns the Replay shape digest a customer-observed
// campaign names. Other workload sources replay no customer trace.
func boundReplayDigest(request optimizer.Request) (string, bool, error) {
	if request.WorkloadSource != optimizer.WorkloadSourceCustomerObserved {
		return "", false, nil
	}
	digest := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(request.WorkloadFingerprint), "sha256:"))
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", true, errors.New("customer_observed workload fingerprint does not name a Replay shape digest")
	}
	return digest, true, nil
}
