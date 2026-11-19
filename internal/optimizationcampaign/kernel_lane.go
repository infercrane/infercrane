package optimizationcampaign

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kernelplanner"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/replay"
)

// LaneKernel marks a custom-kernel setup's predicted evidence. Such a setup
// is never measured in the settings race; its own lane runs the Accelerator
// Lab after approval, and the settings result never waits for it.
const LaneKernel = "kernel"

// KernelLaneKind is the durable operation that drives one kernel setup.
const KernelLaneKind = "optimization.campaign.kernel-lane"

// KernelNotServable ends a kernel setup that qualified in the lab. Serving a
// custom kernel needs a packaged runtime image, which InferCrane does not
// build yet, so it is recorded and never offered for roll-out.
const KernelNotServable = "kernel_qualified_not_servable"

type kernelLanePrediction struct {
	Lane       string                `json:"lane"`
	KernelLane *optimizer.KernelLane `json:"kernel_lane"`
}

// IsKernelLane reports whether a setup runs in the kernel lane.
func IsKernelLane(candidate domain.OptimizationCandidateRun) bool {
	var predicted kernelLanePrediction
	return json.Unmarshal([]byte(candidate.PredictedEvidenceJSON), &predicted) == nil && predicted.Lane == LaneKernel
}

// settingsLane keeps the candidate IDs the settings race measures.
func settingsLane(campaign domain.OptimizationCampaign, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if candidate, ok := candidateByID(campaign.Candidates, id); ok && IsKernelLane(candidate) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// KernelLaneRequest drives one kernel setup within its share of the run's
// approved cap.
type KernelLaneRequest struct {
	TenantID    string  `json:"tenant_id"`
	CampaignID  string  `json:"campaign_id"`
	CandidateID string  `json:"candidate_id"`
	MaxCostUSD  float64 `json:"max_cost_usd"`
}

func (r KernelLaneRequest) Validate() error {
	if r.TenantID == "" || r.CampaignID == "" || r.CandidateID == "" || !(r.MaxCostUSD > 0) || math.IsInf(r.MaxCostUSD, 0) {
		return errors.New("a kernel lane needs tenant, run, setup, and a positive share of the cap")
	}
	return nil
}

// KernelLaneStore is what the lane reads and writes. The lab itself runs as
// its own operation (acceleratorlab.ExecuteKind).
type KernelLaneStore interface {
	Repository
	EnqueueOperation(context.Context, domain.Operation) (domain.Operation, bool, error)
	Operation(context.Context, string) (domain.Operation, error)
	ReplayTraceByDigest(context.Context, string, string) (domain.ReplayTrace, error)
}

// ArtifactPublisher puts a file in the lab worker's content-addressed store,
// where the qualifier reads the replay and quality suite by digest.
type ArtifactPublisher interface {
	Publish(context.Context, string, string, io.Reader, int64) (string, error)
}

// KernelLaneHandlers runs a kernel setup through the Accelerator Lab: build
// and correctness checks (provisioning), then the microbenchmark and an
// end-to-end replay (measuring). A kernel that qualifies ends inconclusive
// with KernelNotServable and its measurements recorded; one that does not
// ends rejected with the lab's reasons. The lane never touches the settings
// race, ranking, or roll-out.
func KernelLaneHandlers(store KernelLaneStore, publisher ArtifactPublisher, now func() time.Time) map[string]operations.Handler {
	if now == nil {
		now = time.Now
	}
	return map[string]operations.Handler{KernelLaneKind: func(ctx context.Context, operation domain.Operation) (string, error) {
		var request KernelLaneRequest
		if err := json.Unmarshal([]byte(operation.RequestJSON), &request); err != nil {
			return "", operations.Permanent("invalid_request", fmt.Errorf("decode kernel lane: %w", err))
		}
		if err := request.Validate(); err != nil {
			return "", operations.Permanent("invalid_request", err)
		}
		lane := kernelLane{store: store, publisher: publisher, now: now, request: request}
		return lane.step(ctx)
	}}
}

type kernelLane struct {
	store     KernelLaneStore
	publisher ArtifactPublisher
	now       func() time.Time
	request   KernelLaneRequest
}

func (l kernelLane) step(ctx context.Context) (string, error) {
	campaign, err := l.store.OptimizationCampaign(ctx, l.request.TenantID, l.request.CampaignID)
	if err != nil {
		return "", executionFailure(err)
	}
	candidate, ok := candidateByID(campaign.Candidates, l.request.CandidateID)
	if !ok || !IsKernelLane(candidate) {
		return "", operations.Permanent("kernel_lane_candidate_not_found", domain.ErrNotFound)
	}
	switch candidate.State {
	case CandidateProposed:
		return l.start(ctx, campaign, candidate)
	case CandidateProvisioning, CandidateReady, CandidateMeasuring:
		return l.follow(ctx, campaign, candidate)
	default:
		return l.result(campaign, candidate), nil
	}
}

type laneEvidence struct {
	OperationID string          `json:"operation_id"`
	Stage       string          `json:"stage"`
	Verdict     string          `json:"verdict,omitempty"`
	Kernel      json.RawMessage `json:"kernel,omitempty"`
	Reasons     []string        `json:"reasons,omitempty"`
	CostUSD     *float64        `json:"cost_usd,omitempty"`
}

func readLaneEvidence(candidate domain.OptimizationCandidateRun) laneEvidence {
	var actual struct {
		KernelLane laneEvidence `json:"kernel_lane"`
	}
	_ = json.Unmarshal([]byte(candidate.ActualEvidenceJSON), &actual)
	return actual.KernelLane
}

func encodeLaneEvidence(evidence laneEvidence) string {
	encoded, _ := json.Marshal(map[string]any{"kernel_lane": evidence})
	return string(encoded)
}

// start publishes the run's recorded traffic and the answer-check prompts to
// the lab worker and queues the lab with the setup's pinned identities.
func (l kernelLane) start(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) (string, error) {
	if campaign.ApprovalExpiresAt == nil || !campaign.ApprovalExpiresAt.After(l.now()) {
		return l.end(ctx, campaign, candidate, CandidateFailed, "approval_expired_before_kernel_lane", laneEvidence{Stage: "not_started"})
	}
	labRequest, err := l.labRequest(ctx, campaign, candidate)
	if err != nil {
		return l.end(ctx, campaign, candidate, CandidateFailed, "kernel_lane_request_invalid", laneEvidence{Stage: "not_started", Reasons: []string{err.Error()}})
	}
	validated, digest, err := acceleratorlab.ValidateRequest(labRequest)
	if err != nil {
		return l.end(ctx, campaign, candidate, CandidateFailed, "kernel_lane_request_invalid", laneEvidence{Stage: "not_started", Reasons: []string{err.Error()}})
	}
	encoded, _ := json.Marshal(validated)
	child, _, err := l.store.EnqueueOperation(ctx, domain.Operation{
		TenantID: campaign.TenantID, Kind: acceleratorlab.ExecuteKind, ResourceType: "optimization_campaign", ResourceName: campaign.ID,
		IdempotencyKey: childKey(candidate.ID, "kernel-lab:"+digest), RequestJSON: string(encoded), MaxAttempts: 20,
	})
	if err != nil {
		return "", operations.Retryable("kernel_lane_enqueue_failed", err)
	}
	if _, err = l.store.TransitionOptimizationCandidate(ctx, campaign.TenantID, campaign.ID, candidate.ID, CandidateProposed, CandidateProvisioning,
		domain.OptimizationCandidateRun{ActualEvidenceJSON: encodeLaneEvidence(laneEvidence{OperationID: child.ID, Stage: "building"})}); err != nil {
		return "", executionFailure(err)
	}
	return "", operations.Retryable("kernel_lane_running", errors.New("the kernel lab is building the kernel"))
}

// follow maps the lab's progress onto the setup: building and correctness
// checks are provisioning, the microbenchmark and replay are measuring.
func (l kernelLane) follow(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) (string, error) {
	evidence := readLaneEvidence(candidate)
	if evidence.OperationID == "" {
		return l.end(ctx, campaign, candidate, CandidateFailed, "kernel_lane_lost", evidence)
	}
	child, err := l.store.Operation(ctx, evidence.OperationID)
	if err != nil {
		return "", operations.Retryable("kernel_lane_read_failed", err)
	}
	if child.TenantID != campaign.TenantID || child.Kind != acceleratorlab.ExecuteKind {
		return l.end(ctx, campaign, candidate, CandidateFailed, "kernel_lane_lost", evidence)
	}
	switch child.Status {
	case "succeeded":
		return l.finish(ctx, campaign, candidate, evidence, child)
	case "failed", "cancelled":
		evidence.Stage = "stopped"
		if child.Message != "" {
			evidence.Reasons = []string{child.Message}
		}
		return l.end(ctx, campaign, candidate, CandidateFailed, "kernel_lab_failed", evidence)
	}
	if child.CurrentStep == "measure" || child.CurrentStep == "prove" {
		for _, step := range [][2]string{{CandidateProvisioning, CandidateReady}, {CandidateReady, CandidateMeasuring}} {
			if candidate.State != step[0] {
				continue
			}
			evidence.Stage = "measuring"
			updated, transitionErr := l.store.TransitionOptimizationCandidate(ctx, campaign.TenantID, campaign.ID, candidate.ID, step[0], step[1], domain.OptimizationCandidateRun{ActualEvidenceJSON: encodeLaneEvidence(evidence)})
			if transitionErr != nil {
				return "", executionFailure(transitionErr)
			}
			candidate = updated
		}
	}
	return "", operations.Retryable("kernel_lane_running", fmt.Errorf("the kernel lab is %s", child.Status))
}

// finish records the lab's verdict. Only a qualified result carries the
// measured kernel speedup and end-to-end change; nothing else is a result.
func (l kernelLane) finish(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, evidence laneEvidence, child domain.Operation) (string, error) {
	var result acceleratorlab.Result
	if err := json.Unmarshal([]byte(child.ResultJSON), &result); err != nil || result.SchemaVersion != acceleratorlab.ResultSchemaV1 {
		evidence.Stage = "unreadable"
		return l.end(ctx, campaign, candidate, CandidateFailed, "kernel_lab_result_unreadable", evidence)
	}
	cost := result.CostUSD
	evidence.CostUSD, evidence.Stage = &cost, "done"
	for _, experiment := range result.Experiments {
		if experiment.Candidate.ID == result.WinnerID && experiment.Qualification != nil {
			q := experiment.Qualification
			evidence.Kernel, _ = json.Marshal(map[string]any{
				"operator_family": experiment.Candidate.OperatorFamily, "kernel_speedup": q.KernelSpeedup, "end_to_end_speedup": q.EndToEndSpeedup,
				"correctness_passed": q.CorrectnessPassed, "hidden_shapes_passed": q.HiddenShapesPassed, "quality_passed": q.QualityPassed,
				"serving_replay_measured": q.ServingReplayMeasured, "measured_at": q.MeasuredAt, "runtime_image_digest": q.RuntimeImageDigest,
				"build_artifact_digest": q.BuildArtifactDigest,
			})
		}
		if experiment.State != "qualified" {
			evidence.Reasons = append(evidence.Reasons, experiment.Reasons...)
		}
	}
	from, to := candidate.State, CandidateRejected
	if result.State == "qualified" && evidence.Kernel != nil {
		evidence.Verdict, to = "qualified", CandidateInconclusive
		// Inconclusive is reachable only from measuring.
		for _, step := range [][2]string{{CandidateProvisioning, CandidateReady}, {CandidateReady, CandidateMeasuring}} {
			if from == step[0] {
				updated, err := l.store.TransitionOptimizationCandidate(ctx, campaign.TenantID, campaign.ID, candidate.ID, step[0], step[1], domain.OptimizationCandidateRun{})
				if err != nil {
					return "", executionFailure(err)
				}
				candidate, from = updated, step[1]
			}
		}
		return l.end(ctx, campaign, candidate, to, KernelNotServable, evidence)
	}
	evidence.Verdict = "not_qualified"
	return l.end(ctx, campaign, candidate, to, "kernel_not_qualified", evidence)
}

func (l kernelLane) end(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, to, code string, evidence laneEvidence) (string, error) {
	if len(evidence.Reasons) > 16 {
		evidence.Reasons = evidence.Reasons[:16]
	}
	updated, err := l.store.TransitionOptimizationCandidate(ctx, campaign.TenantID, campaign.ID, candidate.ID, candidate.State, to,
		domain.OptimizationCandidateRun{FailureCode: code, ActualEvidenceJSON: encodeLaneEvidence(evidence)})
	if err != nil {
		return "", executionFailure(err)
	}
	return l.result(campaign, updated), nil
}

func (l kernelLane) result(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) string {
	encoded, _ := json.Marshal(map[string]any{"campaign_id": campaign.ID, "candidate_id": candidate.ID, "state": candidate.State, "failure_code": candidate.FailureCode, "promotion": "not_performed"})
	return string(encoded)
}

// labRequest binds the lab to the setup's pinned engine image, its GPU, and
// the run's recorded traffic and answer check, published to the worker.
func (l kernelLane) labRequest(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) (acceleratorlab.Request, error) {
	var predicted kernelLanePrediction
	if json.Unmarshal([]byte(candidate.PredictedEvidenceJSON), &predicted) != nil || predicted.KernelLane == nil {
		return acceleratorlab.Request{}, errors.New("the setup's kernel lane is unreadable")
	}
	lane := *predicted.KernelLane
	var draft optimizer.DeploymentDraft
	if json.Unmarshal([]byte(candidate.DeploymentSpecJSON), &draft) != nil {
		return acceleratorlab.Request{}, errors.New("the setup's deployment is unreadable")
	}
	if _, reviewed := kernelplanner.ReviewedPackFor(lane.Operator, lane.Vendor, lane.Engine, lane.EngineVersion); !reviewed || draft.Runtime.Engine != lane.Engine || draft.Runtime.Version != lane.EngineVersion {
		return acceleratorlab.Request{}, errors.New("the setup's engine is not the version its kernel pack was reviewed for")
	}
	var proposal optimizer.Proposal
	if json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) != nil || proposal.Input.WorkloadSource != optimizer.WorkloadSourceCustomerObserved {
		return acceleratorlab.Request{}, errors.New("a kernel lane replays recorded traffic, and the run has none")
	}
	trace, err := l.store.ReplayTraceByDigest(ctx, campaign.TenantID, strings.TrimPrefix(proposal.Input.WorkloadFingerprint, "sha256:"))
	if err != nil {
		return acceleratorlab.Request{}, fmt.Errorf("read the run's recorded traffic: %w", err)
	}
	aiperf, err := replay.NewAIPerfTrace(trace)
	if err != nil {
		return acceleratorlab.Request{}, err
	}
	var traffic bytes.Buffer
	if err = aiperf.Write(&traffic, 0); err != nil {
		return acceleratorlab.Request{}, err
	}
	var summary replay.Summary
	_ = json.Unmarshal([]byte(trace.SummaryJSON), &summary)
	suite := qualitycheck.Default()
	prompts := make([]string, 0, len(suite.Prompts))
	for _, prompt := range suite.Prompts {
		prompts = append(prompts, prompt.Prompt)
	}
	answers, _ := json.Marshal(map[string]any{"prompts": prompts, "max_tokens": suite.MaxTokens})
	replayIdentity, err := l.publish(ctx, candidate.ID, "replay", traffic.Bytes())
	if err != nil {
		return acceleratorlab.Request{}, err
	}
	answersIdentity, err := l.publish(ctx, candidate.ID, "answers", answers)
	if err != nil {
		return acceleratorlab.Request{}, err
	}
	gpus := max(draft.Resources.GPUCount, 1)
	concurrency := max(summary.PeakConcurrency, 1)
	return acceleratorlab.Request{
		SchemaVersion: acceleratorlab.RequestSchemaV1, TenantID: campaign.TenantID, CampaignID: campaign.ID, CandidateID: candidate.ID,
		Model:    kernelplanner.ModelIdentity{Repository: draft.Model.ID, Revision: draft.Model.Revision},
		Runtime:  kernelplanner.RuntimeIdentity{Name: lane.Engine, Version: lane.EngineVersion, ImageDigest: lane.ImageDigest},
		Hardware: kernelplanner.HardwareIdentity{Vendor: lane.Vendor, Accelerator: draft.Resources.GPU},
		Topology: acceleratorlab.Topology{Mode: "aggregated", Nodes: 1, Accelerators: gpus, TensorParallel: gpus, PipelineParallel: 1, ExpertParallel: 1},
		Workload: acceleratorlab.Workload{
			Digest: proposal.Input.WorkloadFingerprint, Modality: acceleratorlab.ModalityText, Phase: kernelplanner.PhaseMixed,
			BatchSize: concurrency, Concurrency: concurrency, InputTokens: max(summary.InputTokensMean, 1), OutputTokens: max(summary.OutputTokensMean, 1),
			QualitySuite: answersIdentity, Replay: replayIdentity,
		},
		Policy: acceleratorlab.Policy{
			MaxCandidates: 1, MaxCostUSD: l.request.MaxCostUSD, RequireSanitizer: true, RequireHiddenShapes: true, RequireServingReplay: true, RequireQuality: true,
			AllowGeneratedKernels: true, AuthorizedUntil: campaign.ApprovalExpiresAt.UTC(),
		},
	}, nil
}

// publish stores a file in the worker's artifact store and names it by its
// content digest, the identity the qualifier resolves.
func (l kernelLane) publish(ctx context.Context, candidateID, kind string, body []byte) (string, error) {
	if l.publisher == nil {
		return "", errors.New("no lab worker artifact store is configured")
	}
	if _, err := l.publisher.Publish(ctx, "kernel-lane-"+candidateID, kind, bytes.NewReader(body), int64(len(body))); err != nil {
		return "", fmt.Errorf("publish the %s to the lab worker: %w", kind, err)
	}
	sum := sha256.Sum256(body)
	return kind + "@sha256:" + hex.EncodeToString(sum[:]), nil
}
