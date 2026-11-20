package continualoptimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/workloadprofile"
)

// FromPublicProfile converts one immutable Agentic Systems-derived screening
// lane into the loop's workload contract. Public data remains explicitly
// non-promotable regardless of sample size.
func FromPublicProfile(document workloadprofile.Document, shape workloadprofile.BenchmarkShape) (Workload, error) {
	if err := workloadprofile.Validate(document); err != nil {
		return Workload{}, err
	}
	if shape.Requests < 1 || shape.InputTokens < 1 || shape.OutputTokens < 1 || shape.Concurrency < 1 {
		return Workload{}, errors.New("public workload shape is incomplete")
	}
	end, err := time.Parse(time.RFC3339, document.GeneratedAt)
	if err != nil {
		return Workload{}, err
	}
	window := time.Minute
	rate := shape.RequestRate
	if rate > 0 {
		window = time.Duration(float64(shape.Requests) / rate * float64(time.Second))
	}
	return Workload{
		Source: SourcePublicPrior, Digest: document.Digest, RequestCount: shape.Requests,
		WindowStart: end.Add(-window), WindowEnd: end, RequestsPerSecond: rate,
		InputTokensMean: float64(shape.InputTokens), OutputTokensMean: float64(shape.OutputTokens),
		PeakConcurrency: float64(shape.Concurrency), StreamingRequestRatio: boolRatio(shape.Streaming),
	}, nil
}

type replayShape struct {
	Streaming        bool     `json:"streaming"`
	SessionIDHash    string   `json:"session_id_hash"`
	SharedPrefixHash string   `json:"shared_prefix_hash"`
	ToolPauseMS      *float64 `json:"tool_pause_ms"`
}

// FromReplayTrace upgrades the loop from its public prior to privacy-preserving
// customer evidence. It derives reuse ratios from repeated hashes, never from
// prompt, completion, session, or prefix content.
func FromReplayTrace(trace domain.ReplayTrace) (Workload, error) {
	if trace.SchemaVersion != "infercrane.replay/v1" || trace.RequestCount < 1 || trace.ShapeDigest == "" || !trace.WindowEnd.After(trace.WindowStart) {
		return Workload{}, errors.New("replay trace identity, request count, and window are required")
	}
	var summary struct {
		Requests         int  `json:"requests"`
		InputTokensMean  int  `json:"input_tokens_mean"`
		OutputTokensMean int  `json:"output_tokens_mean"`
		PeakConcurrency  int  `json:"peak_concurrency"`
		ContentStored    bool `json:"content_stored"`
	}
	if err := json.Unmarshal([]byte(trace.SummaryJSON), &summary); err != nil {
		return Workload{}, fmt.Errorf("decode replay summary: %w", err)
	}
	if summary.ContentStored || summary.Requests != trace.RequestCount || summary.PeakConcurrency < 1 {
		return Workload{}, errors.New("replay summary is inconsistent or contains content")
	}
	var shapes []replayShape
	if err := json.Unmarshal([]byte(trace.ShapeJSON), &shapes); err != nil {
		return Workload{}, fmt.Errorf("decode replay shapes: %w", err)
	}
	if len(shapes) != trace.RequestCount {
		return Workload{}, errors.New("replay shape count does not match trace identity")
	}
	sessions, prefixes := map[string]int{}, map[string]int{}
	streaming, toolPause := 0, 0
	for _, row := range shapes {
		if row.Streaming {
			streaming++
		}
		if row.ToolPauseMS != nil {
			toolPause++
		}
		if row.SessionIDHash != "" {
			sessions[row.SessionIDHash]++
		}
		if row.SharedPrefixHash != "" {
			prefixes[row.SharedPrefixHash]++
		}
	}
	return Workload{
		Source: SourceCustomerObserved, Digest: "sha256:" + trace.ShapeDigest,
		RequestCount: trace.RequestCount, WindowStart: trace.WindowStart.UTC(), WindowEnd: trace.WindowEnd.UTC(),
		RequestsPerSecond: float64(trace.RequestCount) / trace.WindowEnd.Sub(trace.WindowStart).Seconds(),
		InputTokensMean:   float64(summary.InputTokensMean), OutputTokensMean: float64(summary.OutputTokensMean), PeakConcurrency: float64(summary.PeakConcurrency),
		SharedPrefixRatio: repeatedRatio(prefixes, trace.RequestCount), SessionReuseRatio: repeatedRatio(sessions, trace.RequestCount),
		ToolPauseRequestRatio: float64(toolPause) / float64(trace.RequestCount), StreamingRequestRatio: float64(streaming) / float64(trace.RequestCount),
	}, nil
}

// WorkloadFromMonitoring builds customer-observed workload evidence from the
// gateway's own content-free request records for one window. Token means
// come from the requests that reported usage; peak concurrency and reuse
// ratios are not in these records and stay unknown (zero).
func WorkloadFromMonitoring(snapshot domain.EndpointMonitoringSnapshot) (Workload, error) {
	summary := snapshot.Summary
	if summary.Requests < 1 || snapshot.WindowStart.IsZero() || !snapshot.WindowEnd.After(snapshot.WindowStart) {
		return Workload{}, errors.New("monitoring window has no requests")
	}
	identity, err := json.Marshal(map[string]any{
		"schema": "infercrane.monitoring-workload/v1", "endpoint": snapshot.Endpoint,
		"window_start": snapshot.WindowStart.UTC(), "window_end": snapshot.WindowEnd.UTC(),
		"requests": summary.Requests, "streaming": summary.Streaming,
		"input_tokens": summary.InputTokens, "input_token_samples": summary.InputTokenSamples,
		"output_tokens": summary.OutputTokens, "output_token_samples": summary.OutputTokenSamples,
	})
	if err != nil {
		return Workload{}, err
	}
	sum := sha256.Sum256(identity)
	return Workload{
		Source: SourceCustomerObserved, Digest: "sha256:" + hex.EncodeToString(sum[:]), RequestCount: summary.Requests,
		WindowStart: snapshot.WindowStart.UTC(), WindowEnd: snapshot.WindowEnd.UTC(),
		RequestsPerSecond: float64(summary.Requests) / snapshot.WindowEnd.Sub(snapshot.WindowStart).Seconds(),
		InputTokensMean:   mean(summary.InputTokens, summary.InputTokenSamples), OutputTokensMean: mean(summary.OutputTokens, summary.OutputTokenSamples),
		StreamingRequestRatio: float64(summary.Streaming) / float64(summary.Requests),
	}, nil
}

// CostEvidence is the price side of a check: the sourced hourly price of one
// replica of the running setup, and the average replicas that ran in the
// window. Zero means unknown.
type CostEvidence struct {
	HourlyUSDPerReplica float64 `json:"hourly_usd_per_replica,omitempty"`
	AverageReplicas     float64 `json:"average_replicas,omitempty"`
	PriceSource         string  `json:"price_source,omitempty"`
}

// ServiceFromMonitoring builds the serving half of a check from the same
// window. Availability and contribution margin are not in the control
// plane's records, so they stay unknown. Cost per million output tokens is
// set only when the price, the replicas, and the output tokens are all known.
func ServiceFromMonitoring(snapshot domain.EndpointMonitoringSnapshot, cost CostEvidence) ServiceEvidence {
	result := ServiceEvidence{
		RequestCount: snapshot.Summary.Requests, ErrorRate: value(snapshot.Summary.ErrorRate),
		TTFTP95MS: value(snapshot.Summary.P95TTFTMS), OutputTokensPerSecond: value(snapshot.Summary.OutputTokensPerSecond),
	}
	result.CostPerMillionOutputUSD = CostPerMillionOutput(snapshot, cost)
	// Goodput and runtime-internal TPOT are available only when a qualified
	// collector reports them. Missing values remain missing rather than being
	// inferred from gateway latency.
	for _, measurement := range snapshot.Evidence.Measurements {
		if measurement.Availability != "available" || measurement.Value == nil {
			continue
		}
		switch measurement.Name {
		case "runtime_internal_tpot":
			result.TPOTP95MS = *measurement.Value
		case "goodput":
			result.Goodput = *measurement.Value
		case "qualified_output_throughput":
			result.QualifiedOutputTokensPerSecond = *measurement.Value
		}
	}
	return result
}

// CostPerMillionOutput is what one million output tokens cost in the window:
// the hourly price of the replicas that ran, over the output tokens served
// per hour. Output tokens are the per-request mean of the requests that
// reported usage, times every request. Zero means unknown.
func CostPerMillionOutput(snapshot domain.EndpointMonitoringSnapshot, cost CostEvidence) float64 {
	summary := snapshot.Summary
	hours := snapshot.WindowEnd.Sub(snapshot.WindowStart).Hours()
	if cost.HourlyUSDPerReplica <= 0 || cost.AverageReplicas <= 0 || hours <= 0 || summary.OutputTokenSamples < 1 || summary.OutputTokens < 1 || summary.Requests < 1 {
		return 0
	}
	tokensPerHour := mean(summary.OutputTokens, summary.OutputTokenSamples) * float64(summary.Requests) / hours
	result := cost.HourlyUSDPerReplica * cost.AverageReplicas / (tokensPerHour / 1e6)
	if math.IsNaN(result) || math.IsInf(result, 0) || result <= 0 {
		return 0
	}
	return result
}

// AverageReplicas is the time-weighted number of replicas alive in the
// window, from replica creation and deletion times.
func AverageReplicas(spans []domain.ReplicaSpan, start, end time.Time) float64 {
	if !end.After(start) {
		return 0
	}
	alive := 0.0
	for _, span := range spans {
		from, to := span.CreatedAt, end
		if span.DeletedAt != nil {
			to = *span.DeletedAt
		}
		if from.Before(start) {
			from = start
		}
		if to.After(end) {
			to = end
		}
		if to.After(from) {
			alive += to.Sub(from).Seconds()
		}
	}
	return alive / end.Sub(start).Seconds()
}

func mean(total int64, samples int) float64 {
	if samples < 1 || total < 0 {
		return 0
	}
	return float64(total) / float64(samples)
}

func repeatedRatio(values map[string]int, requests int) float64 {
	repeated := 0
	for _, count := range values {
		if count > 1 {
			repeated += count
		}
	}
	if requests == 0 {
		return 0
	}
	return float64(repeated) / float64(requests)
}

func boolRatio(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func value(number *float64) float64 {
	if number == nil {
		return 0
	}
	return *number
}
