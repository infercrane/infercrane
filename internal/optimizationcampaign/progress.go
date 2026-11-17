package optimizationcampaign

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"time"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
)

const (
	RunKindWarmup = "warmup"
	RunKindTurn   = "turn"

	ArmControl   = "control"
	ArmCandidate = "candidate"
)

// MeasurementProgress is the live record of one candidate's interleaved
// measurement: every AIPerf invocation so far, in order, with the numbers it
// measured. A turn is one measured run of either arm; warmups are not turns.
// After an interruption the record keeps the failed invocation, and the
// resumed segment's warmups and turns are added to the plan.
type MeasurementProgress struct {
	Method         string           `json:"method"`
	StartedAt      time.Time        `json:"started_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	TurnsPlanned   int              `json:"turns_planned"`
	WarmupsPlanned int              `json:"warmups_planned"`
	Runs           []MeasurementRun `json:"runs"`
}

// MeasurementRun is one AIPerf invocation. EndedAt is nil while it runs.
// Measured values are copied from AIPerf and are absent when it reported none.
type MeasurementRun struct {
	Kind                  string     `json:"kind"`
	Arm                   string     `json:"arm"`
	Turn                  int        `json:"turn,omitempty"`
	StartedAt             time.Time  `json:"started_at"`
	EndedAt               *time.Time `json:"ended_at,omitempty"`
	DurationSeconds       *float64   `json:"duration_seconds,omitempty"`
	Requests              int        `json:"requests"`
	Failed                *int       `json:"failed,omitempty"`
	OutputTokenThroughput *float64   `json:"output_token_throughput,omitempty"`
	TTFTP95MS             *float64   `json:"ttft_p95_ms,omitempty"`
	LatencyP95MS          *float64   `json:"latency_p95_ms,omitempty"`
	Error                 string     `json:"error,omitempty"`
}

// MeasurementProgressSink stores a measuring candidate's live record. A false
// result means the candidate is no longer measuring.
type MeasurementProgressSink interface {
	RecordOptimizationMeasurementProgress(context.Context, string, string, string, string) (bool, error)
}

// progressTracker records each invocation before and after it runs.
// Recording is display evidence only: a failed write is logged and never
// fails or slows the measurement beyond the write itself.
type progressTracker struct {
	sink                            MeasurementProgressSink
	tenant, campaignID, candidateID string
	now                             func() time.Time
	record                          MeasurementProgress
	controlTurns, candidateTurns    int
}

// newProgressTracker starts a record, or continues the stored one when the
// measurement resumes after an interruption.
func newProgressTracker(sink MeasurementProgressSink, candidate domain.OptimizationCandidateRun, now func() time.Time) *progressTracker {
	tracker := &progressTracker{sink: sink, tenant: candidate.TenantID, campaignID: candidate.CampaignID, candidateID: candidate.ID, now: now, record: MeasurementProgress{Method: MeasurementMethod, Runs: []MeasurementRun{}}}
	if existing, ok := DecodeMeasurementProgress(candidate.MeasurementProgressJSON); ok {
		tracker.record = existing
		for _, run := range existing.Runs {
			if run.Kind != RunKindTurn || run.EndedAt == nil || run.Error != "" {
				continue
			}
			if run.Arm == ArmControl {
				tracker.controlTurns++
			} else {
				tracker.candidateTurns++
			}
		}
	}
	return tracker
}

// planSegment sets the invocations still expected. A fresh measurement with a
// live control plans two warmups and C X C X C X C; without one, one warmup
// and the candidate's runs. A resumed segment keeps every recorded invocation
// and adds its own warmups and the turns its remaining pairs need.
func (t *progressTracker) planSegment(withControl bool, remaining int, resumed bool) {
	if t == nil {
		return
	}
	warmups, turns := 1, remaining
	if withControl {
		warmups, turns = 2, 2*remaining+1
	}
	if !resumed || t.record.StartedAt.IsZero() {
		t.record = MeasurementProgress{Method: MeasurementMethod, Runs: []MeasurementRun{}, StartedAt: t.now()}
		t.controlTurns, t.candidateTurns = 0, 0
	}
	recordedTurns, recordedWarmups := 0, 0
	for _, run := range t.record.Runs {
		if run.Kind == RunKindTurn {
			recordedTurns++
		} else {
			recordedWarmups++
		}
	}
	t.record.TurnsPlanned, t.record.WarmupsPlanned = recordedTurns+turns, recordedWarmups+warmups
	t.save()
}

// run records one invocation around measure.
func (t *progressTracker) run(kind, arm string, requests int, measure func() (benchmark.Result, error)) (benchmark.Result, error) {
	if t == nil {
		return measure()
	}
	entry := MeasurementRun{Kind: kind, Arm: arm, StartedAt: t.now(), Requests: requests}
	if kind == RunKindTurn {
		entry.Turn = t.controlTurns + t.candidateTurns + 1
	}
	t.record.Runs = append(t.record.Runs, entry)
	index := len(t.record.Runs) - 1
	t.save()
	result, err := measure()
	ended := t.now()
	duration := ended.Sub(entry.StartedAt).Seconds()
	current := &t.record.Runs[index]
	current.EndedAt, current.DurationSeconds = &ended, &duration
	if err != nil {
		current.Error = boundedReason(err)
	} else {
		failed := result.Failed
		current.Failed = &failed
		current.OutputTokenThroughput, current.TTFTP95MS, current.LatencyP95MS = result.OutputTokenThroughput, result.TTFTP95MS, result.LatencyP95MS
		if kind == RunKindTurn && arm == ArmControl {
			t.controlTurns++
		} else if kind == RunKindTurn {
			t.candidateTurns++
		}
	}
	t.save()
	return result, err
}

func (t *progressTracker) save() {
	if t.sink == nil {
		return
	}
	t.record.UpdatedAt = t.now()
	encoded, err := json.Marshal(t.record)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = t.sink.RecordOptimizationMeasurementProgress(ctx, t.tenant, t.campaignID, t.candidateID, string(encoded)); err != nil {
		slog.Warn("optimization measurement progress was not recorded", "campaign_id", t.campaignID, "candidate_id", t.candidateID, "error", err)
	}
}

// DecodeMeasurementProgress reads a candidate's stored record. It reports
// false when nothing was recorded.
func DecodeMeasurementProgress(raw string) (MeasurementProgress, bool) {
	var progress MeasurementProgress
	if json.Unmarshal([]byte(raw), &progress) != nil || progress.Method == "" || progress.StartedAt.IsZero() {
		return MeasurementProgress{}, false
	}
	return progress, true
}

// TurnsDone counts finished measured turns, failed ones included.
func (p MeasurementProgress) TurnsDone() int {
	done := 0
	for _, run := range p.Runs {
		if run.Kind == RunKindTurn && run.EndedAt != nil {
			done++
		}
	}
	return done
}

// Current returns the invocation still running, if any.
func (p MeasurementProgress) Current() (MeasurementRun, bool) {
	for index := len(p.Runs) - 1; index >= 0; index-- {
		if p.Runs[index].EndedAt == nil {
			return p.Runs[index], true
		}
	}
	return MeasurementRun{}, false
}

// DecodeStateEnteredAt reads when a candidate first entered each state.
func DecodeStateEnteredAt(raw string) map[string]time.Time {
	var values map[string]string
	out := map[string]time.Time{}
	if json.Unmarshal([]byte(raw), &values) != nil {
		return out
	}
	for state, stamp := range values {
		if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			out[state] = parsed.UTC()
		}
	}
	return out
}

const (
	StageAwaitingApproval = "awaiting_approval"
	StageRunning          = "running"
	StageComparing        = "comparing"
	StageDecided          = "decided"
	StageCancelled        = "cancelled"

	ETAEstimated = "estimated"
	ETAUnknown   = "unknown"

	ETABasis = "observed_durations"
)

// RunProgress is where a run is as a whole. Stage comes from candidate truth,
// not the aggregate campaign state: a campaign reads "ranked" as soon as its
// first candidate is ranked, while later candidates are still measuring.
type RunProgress struct {
	Stage          string     `json:"stage"`
	StartedAt      *time.Time `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at"`
	ElapsedSeconds *float64   `json:"elapsed_seconds"`
	SetupsTotal    int        `json:"setups_total"`
	SetupsMeasured int        `json:"setups_measured"`
	TurnsDone      int        `json:"turns_done"`
	TurnsTotal     *int       `json:"turns_total"`
	ETA            ETA        `json:"eta"`
}

// ETA is an estimate of the time left, built only from durations this run
// already observed. It is unknown until every remaining kind of step was
// timed at least once, and it never covers waiting for a person.
type ETA struct {
	State            string         `json:"state"`
	Basis            string         `json:"basis"`
	RemainingSeconds *float64       `json:"remaining_seconds"`
	CompletesAt      *time.Time     `json:"completes_at"`
	Observed         map[string]int `json:"observed"`
	Reason           string         `json:"reason,omitempty"`
}

var decisionStates = []string{CandidateQualified, CandidateGuardPassed, CandidateRejected, CandidateInconclusive, CandidateFailed, CandidateCancelled, CandidatePromoted, CandidateObserved, CandidateCleaned}

// preBarrier states still have provisioning, measurement, or the answer check
// ahead of them.
func preBarrier(state string) bool {
	switch state {
	case CandidateProposed, CandidateProvisioning, CandidateReady, CandidateMeasuring, CandidateValidating:
		return true
	}
	return false
}

// Progress summarizes a run at now from persisted candidate evidence.
func Progress(campaign domain.OptimizationCampaign, now time.Time) RunProgress {
	now = now.UTC()
	result := RunProgress{SetupsTotal: len(campaign.Candidates), ETA: ETA{State: ETAUnknown, Basis: ETABasis, Observed: map[string]int{}}}
	if campaign.ApprovedAt != nil {
		started := campaign.ApprovedAt.UTC()
		result.StartedAt = &started
	}
	control, hasControl := CampaignControl(campaign)
	running, comparing := false, false
	for _, candidate := range campaign.Candidates {
		running = running || preBarrier(candidate.State)
		comparing = comparing || candidate.State == CandidateRanked || candidate.State == CandidateGuarding
		if !preBarrier(candidate.State) && candidate.State != CandidateCancelled && !(candidate.State == CandidateFailed && candidate.BenchmarkID == "") {
			result.SetupsMeasured++
		}
	}
	switch {
	case campaign.ApprovedAt == nil && !campaign.CancelRequested:
		result.Stage = StageAwaitingApproval
	case campaign.CancelRequested:
		result.Stage = StageCancelled
	case running:
		result.Stage = StageRunning
	case comparing:
		result.Stage = StageComparing
	default:
		result.Stage = StageDecided
	}
	if result.Stage == StageDecided || result.Stage == StageCancelled {
		result.EndedAt = decidedAt(campaign)
	}
	if result.StartedAt != nil {
		end := now
		if result.EndedAt != nil {
			end = *result.EndedAt
		} else if result.Stage == StageDecided || result.Stage == StageCancelled {
			end = time.Time{}
		}
		if !end.IsZero() && !end.Before(*result.StartedAt) {
			elapsed := end.Sub(*result.StartedAt).Seconds()
			result.ElapsedSeconds = &elapsed
		}
	}
	durations := observe(campaign)
	result.ETA.Observed = map[string]int{"provisioning": len(durations.provisioning), "warmups": len(durations.warmups), "turns": len(durations.turns), "answer_checks": len(durations.answerChecks)}
	controlLive := !hasControl || liveControlState(control.State) || preBarrier(control.State)
	total, totalKnown := 0, true
	remaining, missing := 0.0, ""
	need := func(values []float64, name string) float64 {
		if len(values) == 0 {
			if missing == "" {
				missing = name
			}
			return 0
		}
		return mean(values)
	}
	for _, candidate := range campaign.Candidates {
		isControl := hasControl && candidate.ID == control.ID
		turns, warmups := measurementRunsPerArm, 1
		if !isControl && hasControl && controlLive {
			turns, warmups = 2*measurementRunsPerArm+1, 2
		}
		progress, recorded := DecodeMeasurementProgress(candidate.MeasurementProgressJSON)
		entered := DecodeStateEnteredAt(candidate.StateEnteredAtJSON)
		switch {
		case recorded && progress.TurnsPlanned > 0:
			result.TurnsDone += progress.TurnsDone()
			total += progress.TurnsPlanned
		case preBarrier(candidate.State):
			total += turns
		case candidate.BenchmarkID != "":
			totalKnown = false
		}
		if result.Stage != StageRunning {
			continue
		}
		since := func(state string) float64 {
			if at, ok := entered[state]; ok && now.After(at) {
				return now.Sub(at).Seconds()
			}
			return 0
		}
		switch candidate.State {
		case CandidateProposed, CandidateProvisioning:
			remaining += math.Max(need(durations.provisioning, "provisioning")-since(CandidateProvisioning), 0)
			remaining += float64(warmups)*need(durations.warmups, "warmups") + float64(turns)*need(durations.turns, "turns") + need(durations.answerChecks, "answer_checks")
		case CandidateReady:
			remaining += float64(warmups)*need(durations.warmups, "warmups") + float64(turns)*need(durations.turns, "turns") + need(durations.answerChecks, "answer_checks")
		case CandidateMeasuring:
			if recorded && progress.TurnsPlanned > 0 {
				warmupsLeft, turnsLeft := progress.WarmupsPlanned, progress.TurnsPlanned
				for _, run := range progress.Runs {
					if run.EndedAt == nil {
						continue
					}
					if run.Kind == RunKindWarmup {
						warmupsLeft--
					} else {
						turnsLeft--
					}
				}
				if current, ok := progress.Current(); ok {
					mean := need(durations.turns, "turns")
					if current.Kind == RunKindWarmup {
						mean = need(durations.warmups, "warmups")
					}
					inFlight := now.Sub(current.StartedAt).Seconds()
					remaining += math.Max(mean-inFlight, 0)
					if current.Kind == RunKindWarmup {
						warmupsLeft--
					} else {
						turnsLeft--
					}
				}
				remaining += float64(max(warmupsLeft, 0))*need(durations.warmups, "warmups") + float64(max(turnsLeft, 0))*need(durations.turns, "turns")
			} else {
				remaining += float64(warmups)*need(durations.warmups, "warmups") + float64(turns)*need(durations.turns, "turns")
			}
			remaining += need(durations.answerChecks, "answer_checks")
		case CandidateValidating:
			remaining += math.Max(need(durations.answerChecks, "answer_checks")-since(CandidateValidating), 0)
		}
	}
	if totalKnown {
		result.TurnsTotal = &total
	}
	if result.Stage != StageRunning {
		result.ETA.Reason = "the run is not measuring"
		return result
	}
	if missing != "" {
		result.ETA.Reason = etaWaiting[missing]
		return result
	}
	completes := now.Add(time.Duration(remaining * float64(time.Second)))
	result.ETA.State, result.ETA.RemainingSeconds, result.ETA.CompletesAt = ETAEstimated, &remaining, &completes
	return result
}

var etaWaiting = map[string]string{
	"provisioning":  "waiting for the first setup to start serving",
	"warmups":       "waiting for the first warmup to finish",
	"turns":         "waiting for the first measured turn to finish",
	"answer_checks": "waiting for the first answer check to finish",
}

type observedDurations struct {
	provisioning, warmups, turns, answerChecks []float64
}

// observe collects the step durations this run already completed.
func observe(campaign domain.OptimizationCampaign) observedDurations {
	var out observedDurations
	for _, candidate := range campaign.Candidates {
		entered := DecodeStateEnteredAt(candidate.StateEnteredAtJSON)
		if start, ok := entered[CandidateProvisioning]; ok {
			if end, done := entered[CandidateReady]; done && end.After(start) {
				out.provisioning = append(out.provisioning, end.Sub(start).Seconds())
			}
		}
		if start, ok := entered[CandidateValidating]; ok {
			if end, done := entered[CandidateRanked]; done && end.After(start) {
				out.answerChecks = append(out.answerChecks, end.Sub(start).Seconds())
			}
		}
		if progress, ok := DecodeMeasurementProgress(candidate.MeasurementProgressJSON); ok {
			for _, run := range progress.Runs {
				if run.EndedAt == nil || run.DurationSeconds == nil || run.Error != "" {
					continue
				}
				if run.Kind == RunKindWarmup {
					out.warmups = append(out.warmups, *run.DurationSeconds)
				} else {
					out.turns = append(out.turns, *run.DurationSeconds)
				}
			}
		}
	}
	return out
}

// decidedAt is when the last candidate reached its decision. Nil when any
// candidate's decision time was never recorded.
func decidedAt(campaign domain.OptimizationCampaign) *time.Time {
	var latest time.Time
	for _, candidate := range campaign.Candidates {
		entered := DecodeStateEnteredAt(candidate.StateEnteredAtJSON)
		var first time.Time
		for _, state := range decisionStates {
			if at, ok := entered[state]; ok && (first.IsZero() || at.Before(first)) {
				first = at
			}
		}
		if first.IsZero() {
			return nil
		}
		if first.After(latest) {
			latest = first
		}
	}
	if latest.IsZero() {
		return nil
	}
	return &latest
}

func mean(values []float64) float64 {
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

// CostQuoteRecord is the sourced quote that authorized a candidate's provider
// mutation, kept to estimate spend.
type CostQuoteRecord struct {
	HourlyUSD   float64   `json:"hourly_usd"`
	MaxReplicas int       `json:"max_replicas"`
	Source      string    `json:"source"`
	ObservedAt  time.Time `json:"observed_at"`
	Locked      bool      `json:"rate_locked"`
}

// CostQuoteRecorder keeps the first quote that authorized a candidate.
type CostQuoteRecorder interface {
	RecordOptimizationCandidateCostQuote(context.Context, string, string, string, string) error
}

func decodeCostQuote(raw string) (CostQuoteRecord, bool) {
	var quote CostQuoteRecord
	if json.Unmarshal([]byte(raw), &quote) != nil || quote.HourlyUSD <= 0 || math.IsNaN(quote.HourlyUSD) || math.IsInf(quote.HourlyUSD, 0) || quote.Source == "" {
		return CostQuoteRecord{}, false
	}
	return quote, true
}

const (
	SpendEstimated = "estimated"
	SpendUnknown   = "unknown"

	SpendBasis = "quoted_rate_x_replica_seconds"
	SpendLabel = "estimated from quoted rate"
)

// RunSpend is the run's spend so far, estimated from each candidate's bound
// hourly quote and how long its replicas existed. It is not an invoice. It is
// unknown when any candidate held capacity without a recorded quote or when a
// candidate's capacity cannot be attributed yet.
type RunSpend struct {
	State          string   `json:"state"`
	Basis          string   `json:"basis"`
	Label          string   `json:"label"`
	EstimatedUSD   *float64 `json:"estimated_usd"`
	ReplicaSeconds *float64 `json:"replica_seconds"`
	CapUSD         *float64 `json:"cap_usd"`
	Reason         string   `json:"reason,omitempty"`
}

// ReplicaUsage is one candidate's replica lifetimes. Known is false when the
// candidate's capacity could not be looked up.
type ReplicaUsage struct {
	Spans []domain.ReplicaSpan
	Known bool
}

// CapacityTarget says where a candidate's replicas are: an exact revision once
// it is known, else a new-endpoint candidate's own deployment. It reports
// false when the candidate's capacity cannot be attributed yet.
func CapacityTarget(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) (deploymentName, revisionID string, ok bool) {
	if candidate.RevisionID != "" {
		return "", candidate.RevisionID, true
	}
	if campaign.Intent == IntentNewEndpoint {
		if candidate.DeploymentName != "" {
			return candidate.DeploymentName, "", true
		}
		if draft, err := candidateDraft(candidate); err == nil {
			return candidateDeploymentName(draft.Name, candidate.ID), "", true
		}
		return "", "", false
	}
	// An evolution candidate's revision is unknown until provisioning returns.
	return "", "", candidate.State == CandidateProposed
}

// EstimateSpend totals quoted rate × replica-seconds at now.
func EstimateSpend(campaign domain.OptimizationCampaign, usage map[string]ReplicaUsage, now time.Time) RunSpend {
	spend := RunSpend{State: SpendUnknown, Basis: SpendBasis, Label: SpendLabel, CapUSD: campaign.ApprovedMaxCostUSD}
	if campaign.ApprovedAt == nil {
		spend.Reason = "the run has not started"
		return spend
	}
	total, seconds := 0.0, 0.0
	for _, candidate := range campaign.Candidates {
		used, ok := usage[candidate.ID]
		if !ok || !used.Known {
			spend.Reason = "a setup's servers cannot be attributed yet"
			return spend
		}
		candidateSeconds := 0.0
		for _, span := range used.Spans {
			end := now.UTC()
			if span.DeletedAt != nil {
				end = span.DeletedAt.UTC()
			}
			if end.After(span.CreatedAt) {
				candidateSeconds += end.Sub(span.CreatedAt).Seconds()
			}
		}
		if candidateSeconds == 0 {
			continue
		}
		quote, quoted := decodeCostQuote(candidate.CostQuoteJSON)
		if !quoted {
			spend.Reason = "a setup ran without a recorded quoted rate"
			return spend
		}
		perReplica := quote.HourlyUSD / float64(max(quote.MaxReplicas, 1))
		total += perReplica * candidateSeconds / 3600
		seconds += candidateSeconds
	}
	spend.State, spend.EstimatedUSD, spend.ReplicaSeconds = SpendEstimated, &total, &seconds
	return spend
}
