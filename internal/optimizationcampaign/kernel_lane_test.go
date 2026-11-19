package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kernelplanner"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/replay"
)

type laneStore struct {
	*coordinatorRepository
	enqueued   []domain.Operation
	operations map[string]domain.Operation
	trace      domain.ReplayTrace
}

func (s *laneStore) EnqueueOperation(_ context.Context, operation domain.Operation) (domain.Operation, bool, error) {
	for _, existing := range s.enqueued {
		if existing.IdempotencyKey == operation.IdempotencyKey {
			return existing, false, nil
		}
	}
	operation.ID, operation.Status = "op-lab-1", "pending"
	s.enqueued = append(s.enqueued, operation)
	s.operations[operation.ID] = operation
	return operation, true, nil
}

func (s *laneStore) Operation(_ context.Context, id string) (domain.Operation, error) {
	operation, ok := s.operations[id]
	if !ok {
		return domain.Operation{}, domain.ErrNotFound
	}
	return operation, nil
}

func (s *laneStore) ReplayTraceByDigest(_ context.Context, tenant, digest string) (domain.ReplayTrace, error) {
	if tenant != "tenant" || digest != s.trace.ShapeDigest {
		return domain.ReplayTrace{}, domain.ErrNotFound
	}
	return s.trace, nil
}

type recordingPublisher struct{ kinds []string }

func (p *recordingPublisher) Publish(_ context.Context, job, kind string, reader io.Reader, size int64) (string, error) {
	body, _ := io.ReadAll(reader)
	if int64(len(body)) != size || !strings.HasPrefix(job, "kernel-lane-") {
		return "", errors.New("bad publication")
	}
	p.kinds = append(p.kinds, kind)
	return "https://worker.example/v1/artifacts/sha256/x", nil
}

func sampleReplayTrace(t *testing.T) domain.ReplayTrace {
	t.Helper()
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	observations := make([]replay.Observation, 0, 20)
	input, output := 900, 120
	for index := 0; index < 20; index++ {
		observations = append(observations, replay.Observation{StartedAt: start.Add(time.Duration(index) * time.Second), DurationMS: 400, Operation: "chat", InputTokens: &input, OutputTokens: &output})
	}
	trace, err := replay.Build("dep-1", "coder-runtime", "rev-1", start, start.Add(time.Minute), observations)
	if err != nil {
		t.Fatal(err)
	}
	return trace
}

func kernelLaneFixture(t *testing.T, now time.Time) (*laneStore, *recordingPublisher) {
	t.Helper()
	trace := sampleReplayTrace(t)
	image := "sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"
	lane := optimizer.KernelLane{Operator: "norm", Family: string(kernelplanner.ResidualRMSNorm), Vendor: "nvidia", Engine: "vllm", EngineVersion: "0.22.1", ImageDigest: image, Share: .41, Efficiency: .32, LabGPUHours: 2}
	predicted, _ := json.Marshal(map[string]any{"technique": "custom_kernel", "lane": LaneKernel, "kernel_lane": lane})
	spec, _ := json.Marshal(map[string]any{"name": "coder-custom-kernel", "model": map[string]any{"id": "meta-llama/Llama-3.1-8B-Instruct", "revision": strings.Repeat("a", 40)},
		"runtime": map[string]any{"engine": "vllm", "version": "0.22.1"}, "resources": map[string]any{"gpu": "H100", "gpu_count": 1}})
	proposal, _ := json.Marshal(optimizer.Proposal{Input: optimizer.Request{WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: "sha256:" + trace.ShapeDigest}})
	expiry, cost := now.Add(4*time.Hour), 40.0
	campaign := domain.OptimizationCampaign{
		ID: "campaign", TenantID: "tenant", State: CampaignApproved, ApprovalExpiresAt: &expiry, ApprovedMaxCostUSD: &cost, ProposalJSON: string(proposal),
		Candidates: []domain.OptimizationCandidateRun{
			{ID: "control", State: CandidateProposed, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
			{ID: "kernel", State: CandidateProposed, PredictedEvidenceJSON: string(predicted), DeploymentSpecJSON: string(spec), ActualEvidenceJSON: `{}`},
		},
	}
	return &laneStore{coordinatorRepository: &coordinatorRepository{campaign: campaign}, operations: map[string]domain.Operation{}, trace: trace}, &recordingPublisher{}
}

func runLane(t *testing.T, store *laneStore, publisher *recordingPublisher, now time.Time) (string, error) {
	t.Helper()
	request, _ := json.Marshal(KernelLaneRequest{TenantID: "tenant", CampaignID: "campaign", CandidateID: "kernel", MaxCostUSD: 20})
	return KernelLaneHandlers(store, publisher, func() time.Time { return now })[KernelLaneKind](t.Context(), domain.Operation{RequestJSON: string(request)})
}

func laneState(store *laneStore) domain.OptimizationCandidateRun {
	candidate, _ := candidateByID(store.campaign.Candidates, "kernel")
	return candidate
}

func waiting(t *testing.T, err error) {
	t.Helper()
	var failure operations.Failure
	if !errors.As(err, &failure) || !failure.Retryable || failure.Code != "kernel_lane_running" {
		t.Fatalf("err = %v, want the lane to wait on the lab", err)
	}
}

func TestKernelLaneQueuesTheLabBoundToTheSetupAndTheRunsTraffic(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	store, publisher := kernelLaneFixture(t, now)
	_, err := runLane(t, store, publisher, now)
	waiting(t, err)
	if strings.Join(publisher.kinds, ",") != "replay,answers" || len(store.enqueued) != 1 || laneState(store).State != CandidateProvisioning {
		t.Fatalf("published=%v enqueued=%d state=%s", publisher.kinds, len(store.enqueued), laneState(store).State)
	}
	var request acceleratorlab.Request
	if err = json.Unmarshal([]byte(store.enqueued[0].RequestJSON), &request); err != nil {
		t.Fatal(err)
	}
	if store.enqueued[0].Kind != acceleratorlab.ExecuteKind || request.CampaignID != "campaign" || request.CandidateID != "kernel" ||
		request.Runtime.ImageDigest != "sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7" || request.Runtime.Version != "0.22.1" ||
		request.Hardware.Vendor != "nvidia" || request.Hardware.Accelerator != "H100" || request.Workload.Digest != "sha256:"+store.trace.ShapeDigest ||
		!strings.HasPrefix(request.Workload.Replay, "replay@sha256:") || !strings.HasPrefix(request.Workload.QualitySuite, "answers@sha256:") ||
		request.Policy.MaxCostUSD != 20 || !request.Policy.RequireHiddenShapes || !request.Policy.RequireQuality || !request.Policy.RequireServingReplay ||
		!request.Policy.AuthorizedUntil.Equal(*store.campaign.ApprovalExpiresAt) || request.Workload.InputTokens != 900 || request.Workload.OutputTokens != 120 {
		t.Fatalf("lab request = %+v", request)
	}
	// A retry does not queue a second lab run.
	_, err = runLane(t, store, publisher, now)
	waiting(t, err)
	if len(store.enqueued) != 1 {
		t.Fatalf("enqueued %d lab runs", len(store.enqueued))
	}
}

func TestKernelLaneFollowsTheLabStages(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	store, publisher := kernelLaneFixture(t, now)
	_, err := runLane(t, store, publisher, now)
	waiting(t, err)
	lab := store.operations["op-lab-1"]
	lab.Status, lab.CurrentStep = "running", "build"
	store.operations[lab.ID] = lab
	_, err = runLane(t, store, publisher, now)
	waiting(t, err)
	if laneState(store).State != CandidateProvisioning {
		t.Fatalf("building is provisioning, got %s", laneState(store).State)
	}
	lab.CurrentStep = "measure"
	store.operations[lab.ID] = lab
	_, err = runLane(t, store, publisher, now)
	waiting(t, err)
	if laneState(store).State != CandidateMeasuring || readLaneEvidence(laneState(store)).Stage != "measuring" {
		t.Fatalf("measuring: %+v", laneState(store))
	}
}

func labResult(state string, qualified bool) string {
	experiment := acceleratorlab.ExperimentResult{Candidate: kernelplanner.Candidate{ID: "exp-1", OperatorFamily: kernelplanner.ResidualRMSNorm}, State: "rejected", Reasons: []string{"end-to-end speedup target was not reached"}}
	if qualified {
		experiment.State, experiment.Reasons = "qualified", nil
		experiment.Qualification = &acceleratorlab.QualificationEvidence{CorrectnessPassed: true, HiddenShapesPassed: true, QualityPassed: true, ServingReplayMeasured: true, KernelSpeedup: 1.4, EndToEndSpeedup: 1.031}
	}
	result := acceleratorlab.Result{SchemaVersion: acceleratorlab.ResultSchemaV1, State: state, Experiments: []acceleratorlab.ExperimentResult{experiment}, CostUSD: 6.5}
	if qualified {
		result.WinnerID = "exp-1"
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}

func TestKernelLaneRecordsTheVerdictAndNeverOffersARollOut(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		status, result, state, code, verdict string
	}{
		"qualified":     {"succeeded", labResult("qualified", true), CandidateInconclusive, KernelNotServable, "qualified"},
		"not qualified": {"succeeded", labResult("inconclusive", false), CandidateRejected, "kernel_not_qualified", "not_qualified"},
		"lab failed":    {"failed", "", CandidateFailed, "kernel_lab_failed", ""},
	} {
		store, publisher := kernelLaneFixture(t, now)
		_, err := runLane(t, store, publisher, now)
		waiting(t, err)
		lab := store.operations["op-lab-1"]
		lab.Status, lab.ResultJSON, lab.Message = test.status, test.result, "worker stopped"
		store.operations[lab.ID] = lab
		if _, err = runLane(t, store, publisher, now); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		candidate := laneState(store)
		evidence := readLaneEvidence(candidate)
		if candidate.State != test.state || candidate.FailureCode != test.code || evidence.Verdict != test.verdict {
			t.Fatalf("%s: state=%s code=%s evidence=%+v", name, candidate.State, candidate.FailureCode, evidence)
		}
		if name == "qualified" {
			var kernel map[string]any
			_ = json.Unmarshal(evidence.Kernel, &kernel)
			if kernel["end_to_end_speedup"] != 1.031 || kernel["kernel_speedup"] != 1.4 || evidence.CostUSD == nil || *evidence.CostUSD != 6.5 {
				t.Fatalf("qualified kernel lost its measurements: %+v", evidence)
			}
		}
		if name == "not qualified" && (evidence.Kernel != nil || !strings.Contains(strings.Join(evidence.Reasons, " "), "end-to-end")) {
			t.Fatalf("a rejected kernel carries a result or loses its reason: %+v", evidence)
		}
	}
}

func TestKernelLaneDoesNotStartAfterTheApprovalEnds(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	store, publisher := kernelLaneFixture(t, now)
	if _, err := runLane(t, store, publisher, now.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if candidate := laneState(store); candidate.State != CandidateFailed || candidate.FailureCode != "approval_expired_before_kernel_lane" || len(store.enqueued) != 0 || len(publisher.kinds) != 0 {
		t.Fatalf("expired lane: %+v enqueued=%d", candidate, len(store.enqueued))
	}
}

func TestSettingsRaceNeverMeasuresAKernelSetup(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	store, _ := kernelLaneFixture(t, now)
	if got := settingsLane(store.campaign, []string{"control", "kernel"}); strings.Join(got, ",") != "control" {
		t.Fatalf("settings lane = %v", got)
	}
	if !IsKernelLane(store.campaign.Candidates[1]) || IsKernelLane(store.campaign.Candidates[0]) {
		t.Fatal("lane marker misread")
	}
}
