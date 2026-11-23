package optimizationcampaign

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
)

const (
	measurementCheckpointVersion = 1
	// maxMeasurementInterruptions bounds how often one candidate's
	// measurement may stop and resume, whatever stopped it.
	maxMeasurementInterruptions = 4
	// resumeInvocationGap separates each resumed segment's invocation
	// numbers from every number an earlier segment may have used, including
	// runs whose checkpoint write was lost. A segment makes at most eleven
	// invocations and starts only after its start is stored, so a surviving
	// server never sees a seed or replay prefix namespace twice.
	resumeInvocationGap = 64

	SegmentCompleted   = "completed"
	SegmentInterrupted = "interrupted"
)

// MeasurementCheckpointSink stores a measuring candidate's resumable
// measurement. A false result means the candidate is no longer measuring.
type MeasurementCheckpointSink interface {
	RecordOptimizationMeasurementCheckpoint(context.Context, string, string, string, string) (bool, error)
}

// MeasurementCheckpoint is a candidate's measurement so far: every accepted
// run, every segment of uninterrupted measuring, and every interruption. The
// last interruption is pending until it is resolved or given up; the next
// segment starts only after that. Accepted runs are complete pairs: a candidate run
// counts only when a control run from the same segment directly precedes and
// follows it. A run whose pair the interruption left unfinished is dropped and
// counted in the interruption.
type MeasurementCheckpoint struct {
	Version        int             `json:"version"`
	Method         string          `json:"method"`
	Identity       string          `json:"identity"`
	InvocationBase int64           `json:"invocation_base"`
	NextInvocation int64           `json:"next_invocation"`
	Control        ControlEvidence `json:"control"`
	// Comparisons is how many candidates the campaign compares with its
	// control, fixed when the measurement starts; it widens the interval.
	Comparisons   int                       `json:"comparisons,omitempty"`
	Order         []string                  `json:"order"`
	ControlRuns   []benchmark.Result        `json:"control_runs,omitempty"`
	CandidateRuns []benchmark.Result        `json:"candidate_runs,omitempty"`
	Segments      []MeasurementSegment      `json:"segments"`
	Interruptions []MeasurementInterruption `json:"interruptions,omitempty"`
	// Confirmation is set once the first window ended with a gain on the
	// objective; the candidate is then measured again on fresh servers.
	Confirmation *ConfirmationState `json:"confirmation,omitempty"`
}

// MeasurementSegment is one stretch of measuring between interruptions.
// Confirmation marks a segment of the second window, on fresh servers.
type MeasurementSegment struct {
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	FirstInvocation int64      `json:"first_invocation"`
	ControlRuns     int        `json:"control_runs"`
	CandidateRuns   int        `json:"candidate_runs"`
	Ended           string     `json:"ended,omitempty"`
	Confirmation    bool       `json:"confirmation,omitempty"`
}

// MeasurementInterruption is one stop of a measurement: which arm's
// invocation failed, which candidate owns that machine, and how it was
// recovered. Recovery is machine_serving when the provider and runtime still
// served it, machine_replaced when a lost machine was replaced, and
// provider_managed when the provider replaces workers itself. GaveUp names the
// failure code when the run stopped instead of recovering.
type MeasurementInterruption struct {
	Sequence           int        `json:"sequence"`
	At                 time.Time  `json:"at"`
	Arm                string     `json:"arm"`
	Kind               string     `json:"kind"`
	OwnerCandidateID   string     `json:"owner_candidate_id"`
	ReplicaID          string     `json:"replica_id,omitempty"`
	Reason             string     `json:"reason"`
	DiscardedRuns      int        `json:"discarded_runs"`
	Stage              string     `json:"stage,omitempty"`
	Recovery           string     `json:"recovery,omitempty"`
	ReplacementNumber  int        `json:"replacement_number,omitempty"`
	RemovalOperationID string     `json:"removal_operation_id,omitempty"`
	ReplaceOperationID string     `json:"replace_operation_id,omitempty"`
	GaveUp             string     `json:"gave_up,omitempty"`
	ResolvedAt         *time.Time `json:"resolved_at,omitempty"`
}

// MeasurementInterruptedError reports that a measurement stopped at an
// interruption. Persisted says whether the checkpoint holding it was stored;
// when it was not, the next attempt resumes from the previous checkpoint.
type MeasurementInterruptedError struct {
	Interruption MeasurementInterruption
	Persisted    bool
}

func (e *MeasurementInterruptedError) Error() string {
	return fmt.Sprintf("measurement interrupted by the %s %s: %s", e.Interruption.Arm, e.Interruption.Kind, e.Interruption.Reason)
}

// Pending returns the interruption still being recovered, if any.
func (c *MeasurementCheckpoint) Pending() *MeasurementInterruption {
	if len(c.Interruptions) == 0 {
		return nil
	}
	last := &c.Interruptions[len(c.Interruptions)-1]
	if last.ResolvedAt != nil || last.GaveUp != "" {
		return nil
	}
	return last
}

// DecodeMeasurementCheckpoint reads a stored checkpoint. It reports false when
// nothing usable was stored.
func DecodeMeasurementCheckpoint(raw string) (MeasurementCheckpoint, bool) {
	var checkpoint MeasurementCheckpoint
	if json.Unmarshal([]byte(raw), &checkpoint) != nil || checkpoint.Version != measurementCheckpointVersion || checkpoint.Identity == "" {
		return MeasurementCheckpoint{}, false
	}
	return checkpoint, true
}

// measurementIdentity binds a checkpoint to the exact arms and load. A
// checkpoint from another candidate revision, control, or load is never
// resumed.
func measurementIdentity(candidate armTarget, control ControlEvidence, plan loadPlan, profile, fingerprint string) string {
	identity := map[string]any{
		"method": MeasurementMethod, "candidate": candidate.candidateID, "candidate_revision": candidate.revisionID,
		"control_role": control.Role, "control": control.CandidateID, "control_revision": control.RevisionID,
		"profile": profile, "tokenizer": plan.tokenizer, "requests": plan.requests, "concurrency": plan.concurrency,
		"input_tokens": plan.inputTokens, "output_tokens": plan.outputTokens, "warmup": plan.warmup, "streaming": plan.streaming,
		"ttft_slo": plan.ttftSLO, "tpot_slo": plan.tpotSLO, "replay": fingerprint,
	}
	// Runs on another prompt text are another load. The key is absent
	// without a corpus, so checkpoints stored before corpora existed resume.
	if plan.trace != nil && plan.trace.Properties.PromptText != nil {
		identity["prompt_corpus"] = plan.trace.Properties.PromptText.Corpus.Digest
	}
	encoded, _ := json.Marshal(identity)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// measurementSession runs one segment of a candidate's interleaved
// measurement and records it in the checkpoint as it goes. With a live
// control a segment runs C X C X ... C, so every accepted candidate run is
// bracketed by control runs from the same window; without one it runs the
// candidate alone. A failed invocation ends the segment: the checkpoint keeps
// every complete pair, drops the unfinished one, and names the interruption
// so the driver can recover the machine before the next segment.
type measurementSession struct {
	runs      *invocations
	candidate armTarget
	control   *armTarget
	state     *MeasurementCheckpoint
	sink      MeasurementCheckpointSink
	owner     domain.OptimizationCandidateRun
	tracker   *progressTracker
	now       func() time.Time
	// confirm reports whether a finished first window gained on the
	// objective, so the candidate must be measured again on fresh servers.
	// Nil never confirms.
	confirm func(MeasuredEvidence) bool
	// keepControl keeps the control's server for the confirmation because it
	// serves production.
	keepControl bool
}

// windowRuns counts the accepted candidate runs of the first window and of
// the confirmation window.
func (s *measurementSession) windowRuns() (int, int) {
	first, second := 0, 0
	for _, segment := range s.state.Segments {
		if segment.Confirmation {
			second += segment.CandidateRuns
		} else {
			first += segment.CandidateRuns
		}
	}
	return first, second
}

// retire stops the measurement so the driver retires one arm's server and
// provisions it again; the next attempt continues the confirmation.
func (s *measurementSession) retire(arm string, target armTarget) error {
	state := s.state
	interruption := MeasurementInterruption{Sequence: len(state.Interruptions) + 1, At: s.now(), Arm: arm, Kind: RunKindConfirmation, OwnerCandidateID: target.candidateID, ReplicaID: target.replicaID, Reason: "retired so the gain can be measured again on a fresh server"}
	state.Interruptions = append(state.Interruptions, interruption)
	state.Order = append(state.Order, "retire:"+arm)
	return &MeasurementInterruptedError{Interruption: interruption, Persisted: s.save()}
}

func (s *measurementSession) run(ctx context.Context) (measurement, error) {
	state := s.state
	first, second := s.windowRuns()
	if first >= measurementRunsPerArm && state.Confirmation == nil {
		// Every pair of the first window was measured, possibly before a
		// later step failed; analyze the stored runs instead of measuring
		// again, and confirm a gain on fresh servers before it counts.
		measured := s.finish()
		if s.confirm == nil || s.control == nil || measured.evidence.Control.Role != ControlRoleBracketed || !s.confirm(measured.evidence) {
			return measured, nil
		}
		state.Confirmation = &ConfirmationState{Phase: ConfirmRetireCandidate, KeepControl: s.keepControl}
	}
	if confirmation := state.Confirmation; confirmation != nil {
		if s.control == nil {
			return measurement{}, operations.Permanent("optimization_confirmation_invalid", errors.New("the control is no longer serving, so the gain cannot be measured again"))
		}
		switch confirmation.Phase {
		case ConfirmRetireCandidate:
			return measurement{}, s.retire(ArmCandidate, s.candidate)
		case ConfirmRetireControl:
			return measurement{}, s.retire(ArmControl, *s.control)
		}
		if second >= measurementRunsPerArm {
			return s.finish(), nil
		}
	}
	confirming := state.Confirmation != nil
	done := first
	if confirming {
		done = second
	}
	resumed := len(state.Segments) > 0
	if resumed {
		state.NextInvocation += resumeInvocationGap
		state.Order = append(state.Order, "resume")
	}
	s.runs.base, s.runs.next = state.InvocationBase, state.NextInvocation
	remaining := measurementRunsPerArm - done
	state.Segments = append(state.Segments, MeasurementSegment{StartedAt: s.now(), FirstInvocation: state.InvocationBase + state.NextInvocation, Confirmation: confirming})
	segment := &state.Segments[len(state.Segments)-1]
	// Nothing is sent until the segment's start is stored: a resume adds the
	// invocation gap to the stored counter, so an unstored start could hand a
	// surviving server seeds it has already seen.
	if s.sink != nil && !s.save() {
		return measurement{}, operations.Retryable("optimization_checkpoint_failed", errors.New("the measurement checkpoint could not be stored before measuring"))
	}
	s.tracker.planSegment(s.control != nil, remaining, resumed)

	invoke := func(kind, arm string, target armTarget, requests int) (benchmark.Result, error) {
		result, err := s.tracker.run(kind, arm, requests, func() (benchmark.Result, error) { return s.runs.run(ctx, target, requests) })
		state.NextInvocation = s.runs.next
		return result, err
	}
	interrupt := func(kind string, target armTarget, err error, discarded int) error {
		ended := s.now()
		segment.EndedAt, segment.Ended = &ended, SegmentInterrupted
		arm := ArmCandidate
		if target.candidateID != s.candidate.candidateID {
			arm = ArmControl
		}
		interruption := MeasurementInterruption{Sequence: len(state.Interruptions) + 1, At: ended, Arm: arm, Kind: kind, OwnerCandidateID: target.candidateID, ReplicaID: target.replicaID, Reason: boundedReason(err), DiscardedRuns: discarded}
		state.Interruptions = append(state.Interruptions, interruption)
		state.Order = append(state.Order, "interrupted:"+arm)
		return &MeasurementInterruptedError{Interruption: interruption, Persisted: s.save()}
	}
	accept := func(control, candidate *benchmark.Result) {
		if control != nil {
			state.ControlRuns = append(state.ControlRuns, *control)
			segment.ControlRuns++
		}
		if candidate != nil {
			state.CandidateRuns = append(state.CandidateRuns, *candidate)
			segment.CandidateRuns++
		}
	}

	if s.control != nil {
		if _, err := invoke(RunKindWarmup, ArmControl, *s.control, s.runs.plan.warmup); err != nil {
			return measurement{}, interrupt(RunKindWarmup, *s.control, err, 0)
		}
		state.Order = append(state.Order, "warmup:control")
	}
	if _, err := invoke(RunKindWarmup, ArmCandidate, s.candidate, s.runs.plan.warmup); err != nil {
		return measurement{}, interrupt(RunKindWarmup, s.candidate, err, 0)
	}
	state.Order = append(state.Order, "warmup:candidate")
	s.save()

	if s.control == nil {
		for segment.CandidateRuns < remaining {
			measured, err := invoke(RunKindTurn, ArmCandidate, s.candidate, s.runs.plan.requests)
			if err != nil {
				return measurement{}, interrupt(RunKindTurn, s.candidate, err, 0)
			}
			state.Order = append(state.Order, "candidate")
			accept(nil, &measured)
			s.save()
		}
		return s.finish(), nil
	}

	// The opening control run is accepted together with the first candidate
	// run it brackets; until then an interruption drops it.
	opening, err := invoke(RunKindTurn, ArmControl, *s.control, s.runs.plan.requests)
	if err != nil {
		return measurement{}, interrupt(RunKindTurn, *s.control, err, 0)
	}
	state.Order = append(state.Order, "control")
	leading := &opening
	for segment.CandidateRuns < remaining {
		candidateRun, err := invoke(RunKindTurn, ArmCandidate, s.candidate, s.runs.plan.requests)
		if err != nil {
			return measurement{}, interrupt(RunKindTurn, s.candidate, err, boolCount(leading != nil))
		}
		state.Order = append(state.Order, "candidate")
		closing, err := invoke(RunKindTurn, ArmControl, *s.control, s.runs.plan.requests)
		if err != nil {
			return measurement{}, interrupt(RunKindTurn, *s.control, err, 1+boolCount(leading != nil))
		}
		state.Order = append(state.Order, "control")
		if leading != nil {
			accept(leading, nil)
			leading = nil
		}
		accept(&closing, &candidateRun)
		s.save()
	}
	// The window is complete: analyze it, and start its confirmation when
	// it gained on the objective.
	return s.run(ctx)
}

// finish closes the last segment and analyzes every accepted run. The first
// window and the confirmation window on fresh servers are analyzed apart:
// each has its own control check and paired comparison, because the change
// of servers between them is exactly what the confirmation samples. Within a
// window, drift is checked across all adjacent control runs in order,
// including the pair that spans each restart, so a replaced machine that
// serves differently invalidates the control instead of passing silently.
func (s *measurementSession) finish() measurement {
	state := s.state
	if segment := &state.Segments[len(state.Segments)-1]; segment.Ended == "" {
		ended := s.now()
		segment.EndedAt, segment.Ended = &ended, SegmentCompleted
		s.save()
	}
	evidence := MeasuredEvidence{Method: MeasurementMethod, InvocationBase: state.InvocationBase, Order: state.Order, Control: state.Control, ServerMetricsRequested: s.candidate.serverMetrics, GPUTelemetry: "not_collected"}
	firstWindow := window{control: state.Control}
	var second *window
	if state.Confirmation != nil {
		second = &window{control: state.Control}
	}
	// Runs are listed as they ran: each segment's accepted runs are
	// C X C ... C with a control, or the candidate's runs alone.
	control, candidate := 0, 0
	var firstCandidates []benchmark.Result
	for _, segment := range state.Segments {
		target := &firstWindow
		if segment.Confirmation && second != nil {
			target = second
		}
		if segment.ControlRuns > 0 && control < len(state.ControlRuns) {
			target.addControl(state.ControlRuns[control])
			control++
		}
		for index := 0; index < segment.CandidateRuns && candidate < len(state.CandidateRuns); index++ {
			target.addCandidate(state.CandidateRuns[candidate])
			if target == &firstWindow {
				firstCandidates = append(firstCandidates, state.CandidateRuns[candidate])
			}
			candidate++
			if segment.ControlRuns > 0 && control < len(state.ControlRuns) {
				target.addControl(state.ControlRuns[control])
				control++
			}
		}
		if segment.CandidateRuns > 0 {
			target.segments++
		}
	}
	if len(state.Segments) > 1 || len(state.Interruptions) > 0 {
		evidence.Segments, evidence.Interruptions = state.Segments, state.Interruptions
	}
	evidence.Runs = firstWindow.runs
	evidence.Control = firstWindow.analyze(state.Comparisons, &evidence.Comparison)
	if second != nil {
		confirmation := &ConfirmationEvidence{Runs: second.runs, FreshCandidate: freshServer(state, ArmCandidate), FreshControl: freshServer(state, ArmControl)}
		confirmation.Control = second.analyze(1, &confirmation.Comparison)
		evidence.Confirmation = confirmation
	}
	// The stored candidate row describes the first window, the one its
	// control runs and guard checks come from.
	return measurement{candidate: firstCandidates, evidence: evidence}
}

// window collects one measurement window's runs in order.
type window struct {
	control      ControlEvidence
	runs         []RunSummary
	controlRuns  []benchmark.Result
	restarts     map[int]bool
	segments     int
	lastSegments int
}

func (w *window) addControl(run benchmark.Result) {
	if w.segments != w.lastSegments && len(w.controlRuns) > 0 {
		if w.restarts == nil {
			w.restarts = map[int]bool{}
		}
		w.restarts[len(w.controlRuns)] = true
	}
	w.lastSegments = w.segments
	w.controlRuns = append(w.controlRuns, run)
	w.runs = append(w.runs, summarizeRun("control", run))
}

func (w *window) addCandidate(run benchmark.Result) {
	w.runs = append(w.runs, summarizeRun("candidate", run))
}

// analyze checks the window's control runs and compares the candidate with
// them. comparisons widens the interval for the number of candidates the
// window's result selects among: the first window selects among the
// campaign's candidates, the confirmation tests only the one it confirms.
func (w *window) analyze(comparisons int, into *map[string]MetricComparison) ControlEvidence {
	evidence := w.control
	evidence.DriftLimit = maxControlDrift
	evidence.Runs = len(w.controlRuns)
	if evidence.Role != ControlRoleBracketed {
		return evidence
	}
	reason, drift, limit := validControl(w.controlRuns, measurementRunsPerArm+w.segments, w.restarts)
	evidence.MaxDrift = drift
	if reason != "" {
		evidence.Role, evidence.Reason, evidence.DriftLimit = ControlRoleInvalid, reason, limit
		return evidence
	}
	*into = comparePairedArms(w.runs, comparisons)
	return evidence
}

// freshServer reports whether the confirmation retired and replaced arm's
// server; a serverless provider places workers itself.
func freshServer(state *MeasurementCheckpoint, arm string) bool {
	for _, interruption := range state.Interruptions {
		if interruption.Kind == RunKindConfirmation && interruption.Arm == arm && interruption.Recovery == RecoveryMachineRetired {
			return true
		}
	}
	return false
}

// save stores the checkpoint. A failed write is logged and the measurement
// goes on: every write is a complete snapshot at a pair boundary, so a lost
// write only means a resume repeats more work, and the invocation gap keeps a
// repeated run's seed fresh.
func (s *measurementSession) save() bool {
	if s.sink == nil {
		return false
	}
	encoded, err := json.Marshal(s.state)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	written, err := s.sink.RecordOptimizationMeasurementCheckpoint(ctx, s.owner.TenantID, s.owner.CampaignID, s.owner.ID, string(encoded))
	if err != nil || !written {
		slog.Warn("optimization measurement checkpoint was not recorded", "campaign_id", s.owner.CampaignID, "candidate_id", s.owner.ID, "written", written, "error", err)
		return false
	}
	return true
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

// errCheckpointPending means the checkpoint still names an interruption that
// the driver has not resolved, so measuring must not resume yet.
var errCheckpointPending = errors.New("measurement interruption is still being recovered")
