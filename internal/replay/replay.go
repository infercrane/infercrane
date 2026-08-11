// Package replay builds privacy-preserving workload-shape traces.
package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type Observation struct {
	StartedAt                                            time.Time
	DurationMS                                           float64
	InputTokens, OutputTokens                            *int
	Operation                                            string
	Streaming                                            bool
	SessionIDHash, ParentSessionIDHash, SharedPrefixHash string
	ToolPauseMS                                          *float64
}
type shape struct {
	ArrivalMS           int64    `json:"arrival_ms"`
	DurationMS          float64  `json:"duration_ms"`
	InputTokens         *int     `json:"input_tokens,omitempty"`
	OutputTokens        *int     `json:"output_tokens,omitempty"`
	Operation           string   `json:"operation"`
	Streaming           bool     `json:"streaming"`
	SessionIDHash       string   `json:"session_id_hash,omitempty"`
	ParentSessionIDHash string   `json:"parent_session_id_hash,omitempty"`
	SharedPrefixHash    string   `json:"shared_prefix_hash,omitempty"`
	ToolPauseMS         *float64 `json:"tool_pause_ms,omitempty"`
}
type Summary struct {
	Requests         int    `json:"requests"`
	InputTokensMean  int    `json:"input_tokens_mean,omitempty"`
	OutputTokensMean int    `json:"output_tokens_mean,omitempty"`
	PeakConcurrency  int    `json:"peak_concurrency"`
	Sessions         int    `json:"sessions"`
	SharedPrefixes   int    `json:"shared_prefixes"`
	ContentStored    bool   `json:"content_stored"`
	EvidenceClass    string `json:"evidence_class"`
}

func Build(deploymentID, deploymentName, revisionID string, windowStart, windowEnd time.Time, observations []Observation) (domain.ReplayTrace, error) {
	sort.SliceStable(observations, func(i, j int) bool { return observations[i].StartedAt.Before(observations[j].StartedAt) })
	rows := make([]shape, 0, len(observations))
	sessions, prefixes := map[string]struct{}{}, map[string]struct{}{}
	inputTotal, inputCount, outputTotal, outputCount := 0, 0, 0, 0
	type event struct {
		at    time.Time
		delta int
	}
	events := make([]event, 0, len(observations)*2)
	for _, o := range observations {
		rows = append(rows, shape{ArrivalMS: o.StartedAt.Sub(windowStart).Milliseconds(), DurationMS: o.DurationMS, InputTokens: o.InputTokens, OutputTokens: o.OutputTokens, Operation: o.Operation, Streaming: o.Streaming, SessionIDHash: o.SessionIDHash, ParentSessionIDHash: o.ParentSessionIDHash, SharedPrefixHash: o.SharedPrefixHash, ToolPauseMS: o.ToolPauseMS})
		if o.InputTokens != nil {
			inputTotal += *o.InputTokens
			inputCount++
		}
		if o.OutputTokens != nil {
			outputTotal += *o.OutputTokens
			outputCount++
		}
		if o.SessionIDHash != "" {
			sessions[o.SessionIDHash] = struct{}{}
		}
		if o.SharedPrefixHash != "" {
			prefixes[o.SharedPrefixHash] = struct{}{}
		}
		events = append(events, event{o.StartedAt, 1}, event{o.StartedAt.Add(time.Duration(o.DurationMS * float64(time.Millisecond))), -1})
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].at.Equal(events[j].at) {
			return events[i].delta < events[j].delta
		}
		return events[i].at.Before(events[j].at)
	})
	current, peak := 0, 0
	for _, e := range events {
		current += e.delta
		if current > peak {
			peak = current
		}
	}
	summary := Summary{Requests: len(rows), PeakConcurrency: peak, Sessions: len(sessions), SharedPrefixes: len(prefixes), ContentStored: false, EvidenceClass: "production_shape"}
	if inputCount > 0 {
		summary.InputTokensMean = inputTotal / inputCount
	}
	if outputCount > 0 {
		summary.OutputTokensMean = outputTotal / outputCount
	}
	shapeJSON, err := json.Marshal(rows)
	if err != nil {
		return domain.ReplayTrace{}, err
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return domain.ReplayTrace{}, err
	}
	envelope, _ := json.Marshal(struct {
		Shape   json.RawMessage `json:"shape"`
		Summary json.RawMessage `json:"summary"`
	}{shapeJSON, summaryJSON})
	digest := sha256.Sum256(envelope)
	return domain.ReplayTrace{DeploymentID: deploymentID, DeploymentName: deploymentName, RevisionID: revisionID, SchemaVersion: "infercrane.replay/v1", WindowStart: windowStart, WindowEnd: windowEnd, RequestCount: len(rows), ShapeJSON: string(shapeJSON), SummaryJSON: string(summaryJSON), ShapeDigest: hex.EncodeToString(digest[:])}, nil
}
