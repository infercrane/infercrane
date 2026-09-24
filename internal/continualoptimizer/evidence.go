package continualoptimizer

import (
	"encoding/json"
	"errors"
	"fmt"
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

// FromMonitoring builds the serving half of an evaluation from the normalized
// endpoint read model and externally reconciled economics. Availability and
// contribution are required from a real observation window; zero explicitly
// means unavailable and therefore cannot satisfy promotion policy.
func FromMonitoring(snapshot domain.EndpointMonitoringSnapshot, availability, productiveUtilization, costPerMillionOutputUSD, contributionMargin float64) ServiceEvidence {
	result := ServiceEvidence{
		RequestCount: snapshot.Summary.Requests, ErrorRate: value(snapshot.Summary.ErrorRate), Availability: availability,
		TTFTP95MS: value(snapshot.Summary.P95TTFTMS), ProductiveUtilization: productiveUtilization,
		CostPerMillionOutputUSD: costPerMillionOutputUSD, ContributionMargin: contributionMargin,
		OutputTokensPerSecond: value(snapshot.Summary.OutputTokensPerSecond),
	}
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
