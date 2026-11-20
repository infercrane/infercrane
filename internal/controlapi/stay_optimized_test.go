package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/continualoptimizer"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/notification"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/support"
)

// stayOptimizedFake is one endpoint, "chat", running vLLM 0.21.0 on an AWS
// L40S with a week of traffic, and a Stay optimized policy.
type stayOptimizedFake struct {
	*fakeOptimizationCampaignStore
	policy      continualoptimizer.Policy
	policyJSON  string
	state       domain.ContinualOptimizationState
	stateFound  bool
	leaseOwner  string
	decisions   []continualoptimizer.Decision
	claims      int
	completions int
	kernels     []domain.EndpointCustomKernel
	kernelMoves map[string]string
	autoSpent   float64
	trace       domain.ReplayTrace
}

func newStayOptimizedFake(t *testing.T, policy continualoptimizer.Policy) *stayOptimizedFake {
	t.Helper()
	spec, err := json.Marshal(domain.DeploymentRevisionSpec{Model: "qwen3-8b", Runtime: "vllm", RuntimeVersion: "0.21.0", Cloud: "aws", Region: "eu-central-1", GPU: "L40S", GPUCount: 1, MinReplicas: 1, MaxReplicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	base := &fakeStore{
		resolved:  domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment-chat", TenantID: "default", Name: "chat", Model: "qwen3-8b", Runtime: "vllm", ActiveRevisionID: "revision-1", DesiredState: "running"}, EndpointNames: []string{"chat"}},
		revisions: []domain.DeploymentRevision{{ID: "revision-1", DeploymentID: "deployment-chat", Status: "active", SpecJSON: string(spec)}},
	}
	now := time.Now().UTC()
	return &stayOptimizedFake{
		fakeOptimizationCampaignStore: &fakeOptimizationCampaignStore{fakeStore: base}, policy: policy, kernelMoves: map[string]string{},
		trace: domain.ReplayTrace{ID: "replay-1", DeploymentName: "chat", SchemaVersion: "infercrane.replay/v1", ShapeDigest: strings.Repeat("b", 64), RequestCount: 900, WindowStart: now.Add(-24 * time.Hour), WindowEnd: now},
	}
}

func (f *stayOptimizedFake) Revision(_ context.Context, _, _, id string) (domain.DeploymentRevision, error) {
	for _, revision := range f.revisions {
		if revision.ID == id {
			return revision, nil
		}
	}
	return domain.DeploymentRevision{}, domain.ErrNotFound
}

func (f *stayOptimizedFake) SetContinualOptimizationPolicy(_ context.Context, _, name string, policy continualoptimizer.Policy) (domain.ContinualOptimizationPolicyRecord, error) {
	f.policy = policy
	return f.ContinualOptimizationPolicy(context.Background(), "", name)
}

func (f *stayOptimizedFake) ContinualOptimizationPolicy(_ context.Context, _, name string) (domain.ContinualOptimizationPolicyRecord, error) {
	if f.policyJSON != "" {
		return domain.ContinualOptimizationPolicyRecord{DeploymentName: name, PolicyJSON: f.policyJSON, Generation: 1}, nil
	}
	encoded, _ := json.Marshal(f.policy)
	return domain.ContinualOptimizationPolicyRecord{DeploymentName: name, PolicyJSON: string(encoded), Generation: 1}, nil
}

func (f *stayOptimizedFake) ContinualOptimizationState(context.Context, string, string) (domain.ContinualOptimizationState, error) {
	if !f.stateFound {
		return domain.ContinualOptimizationState{}, domain.ErrNotFound
	}
	return f.state, nil
}

func (f *stayOptimizedFake) RecordContinualOptimizationDecision(_ context.Context, _, _ string, current continualoptimizer.Workload, decision continualoptimizer.Decision) (domain.ContinualOptimizationDecisionRecord, bool, error) {
	f.decisions = append(f.decisions, decision)
	f.stateFound = true
	if decision.BaselineReplacement {
		encoded, _ := json.Marshal(current)
		f.state.BaselineJSON = string(encoded)
	}
	if decision.Action == continualoptimizer.ActionStartExperiment {
		at := decision.EvaluatedAt
		f.state.LastExperimentAt = &at
	}
	return domain.ContinualOptimizationDecisionRecord{ID: "decision-" + string(rune('0'+len(f.decisions)))}, true, nil
}

func (f *stayOptimizedFake) ContinualOptimizationDecisions(context.Context, string, string, int) ([]domain.ContinualOptimizationDecisionRecord, error) {
	return nil, nil
}

func (f *stayOptimizedFake) ClaimDueStayOptimizedChecks(_ context.Context, owner string, lease time.Duration, _ int) ([]domain.StayOptimizedCheck, error) {
	now := time.Now().UTC()
	if !f.policy.Enabled || f.leaseOwner != "" || f.state.NextCheckAt != nil && f.state.NextCheckAt.After(now) {
		return nil, nil
	}
	return f.claim(owner, lease), nil
}

func (f *stayOptimizedFake) ClaimStayOptimizedCheck(_ context.Context, _, _, owner string, lease time.Duration) (domain.StayOptimizedCheck, error) {
	if f.leaseOwner != "" {
		return domain.StayOptimizedCheck{}, domain.ErrConflict
	}
	return f.claim(owner, lease)[0], nil
}

func (f *stayOptimizedFake) claim(owner string, lease time.Duration) []domain.StayOptimizedCheck {
	f.claims++
	f.leaseOwner = owner
	observed := f.state.ObservedJSON
	if observed == "" {
		observed = "{}"
	}
	return []domain.StayOptimizedCheck{{TenantID: "default", DeploymentID: "deployment-chat", DeploymentName: "chat", ObservedJSON: observed, LeaseOwner: owner, LeaseExpiresAt: time.Now().Add(lease)}}
}

func (f *stayOptimizedFake) CompleteStayOptimizedCheck(_ context.Context, check domain.StayOptimizedCheck, observed string, next time.Time, checkError string) error {
	if check.LeaseOwner != f.leaseOwner {
		return domain.ErrConflict
	}
	f.completions++
	f.leaseOwner, f.stateFound = "", true
	now := time.Now().UTC()
	f.state.ObservedJSON, f.state.NextCheckAt, f.state.LastCheckAt, f.state.LastCheckError = observed, &next, &now, checkError
	return nil
}

func (f *stayOptimizedFake) StayOptimizedProposals(context.Context, string, string, int) ([]domain.OptimizationCampaign, error) {
	if f.campaign.ID == "" || f.campaign.OriginJSON == "" {
		return nil, nil
	}
	return []domain.OptimizationCampaign{f.campaign}, nil
}

func (f *stayOptimizedFake) SetOptimizationCampaignOrigin(_ context.Context, _, _, origin string) error {
	f.campaign.OriginJSON = origin
	return nil
}

func (f *stayOptimizedFake) StayOptimizedAutoRunSpend(context.Context, string, string, time.Time) (float64, error) {
	return f.autoSpent, nil
}

func (f *stayOptimizedFake) EndpointCustomKernels(context.Context, string, string) ([]domain.EndpointCustomKernel, error) {
	return f.kernels, nil
}

func (f *stayOptimizedFake) SetEndpointCustomKernelState(_ context.Context, _, id, from, state, _ string) error {
	for index := range f.kernels {
		if f.kernels[index].ID == id && f.kernels[index].State == from {
			f.kernels[index].State, f.kernelMoves[id] = state, state
			return nil
		}
	}
	return domain.ErrConflict
}

func (f *stayOptimizedFake) EndpointMonitoring(_ context.Context, _, endpoint string, window, bucket time.Duration) (domain.EndpointMonitoringSnapshot, error) {
	end := time.Now().UTC()
	errorRate, ttft, output := .001, 180.0, 40.0
	return domain.EndpointMonitoringSnapshot{Endpoint: endpoint, WindowStart: end.Add(-window), WindowEnd: end, BucketSeconds: int(bucket.Seconds()), Summary: domain.MonitoringSummary{
		Requests: 6000, Streaming: 3000, InputTokens: 6_000_000, InputTokenSamples: 6000, OutputTokens: 1_200_000, OutputTokenSamples: 6000,
		RequestsPerSecond: 6000 / window.Seconds(), ErrorRate: &errorRate, P95TTFTMS: &ttft, OutputTokensPerSecond: &output,
	}}, nil
}

// One replica ran the whole week.
func (f *stayOptimizedFake) OptimizationReplicaSpans(context.Context, string, string, string, time.Time) ([]domain.ReplicaSpan, error) {
	return []domain.ReplicaSpan{{ReplicaID: "replica-1", CreatedAt: time.Now().Add(-30 * 24 * time.Hour)}}, nil
}

func (f *stayOptimizedFake) CaptureReplayTrace(context.Context, string, string, time.Duration, int) (domain.ReplayTrace, error) {
	return f.trace, nil
}
func (f *stayOptimizedFake) ReplayTrace(context.Context, string, string) (domain.ReplayTrace, error) {
	return f.trace, nil
}
func (f *stayOptimizedFake) ReplayTraces(context.Context, string, string, int) ([]domain.ReplayTrace, error) {
	return []domain.ReplayTrace{f.trace}, nil
}
func (f *stayOptimizedFake) RecordArtifactCacheObservation(_ context.Context, _ string, row domain.ArtifactCacheObservation) (domain.ArtifactCacheObservation, error) {
	return row, nil
}
func (f *stayOptimizedFake) RequestArtifactPrefetch(_ context.Context, _ string, row domain.ArtifactPrefetch) (domain.ArtifactPrefetch, bool, error) {
	return row, true, nil
}
func (f *stayOptimizedFake) CapacityIntelligence(context.Context, string, time.Duration) ([]domain.CapacitySummary, error) {
	return nil, nil
}

func stayOptimizedAPI(t *testing.T, store *stayOptimizedFake) API {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	// The workspace runs on the operator's own AWS account, so an approval
	// needs no tenant compute connection.
	return API{Store: store, APIKey: "secret", OptimizationCosts: fakeOptimizationCosts{}, Integrations: registry.Snapshot(), ComputeProviders: []ComputeProvider{{ID: "aws", Mode: "byoc", State: "ready", BillingModes: []string{"provider_account"}}}, OperatorComputeTenants: []string{"default"}}
}

func decodeOrigin(t *testing.T, campaign domain.OptimizationCampaign) continualoptimizer.Origin {
	t.Helper()
	origin, ok := continualoptimizer.DecodeOrigin(campaign.OriginJSON)
	if !ok {
		t.Fatalf("campaign has no Stay optimized origin: %s", campaign.OriginJSON)
	}
	return origin
}

// An engine release in the pinned catalog makes the background check propose
// a run with its reason and cost. The run waits for approval: nothing is
// approved, queued, or deployed, and later passes add nothing while it waits.
func TestStayOptimizedEngineReleaseProposesARunThatWaitsForApproval(t *testing.T) {
	store := newStayOptimizedFake(t, continualoptimizer.DefaultPolicy())
	loop := StayOptimized{API: stayOptimizedAPI(t, store), Owner: "instance-a"}

	ran, err := loop.RunOnce(context.Background())
	if err != nil || ran != 1 || store.completions != 1 || store.leaseOwner != "" {
		t.Fatalf("ran=%d err=%v completions=%d lease=%q", ran, err, store.completions, store.leaseOwner)
	}
	if len(store.decisions) != 1 || store.decisions[0].Action != continualoptimizer.ActionStartExperiment {
		t.Fatalf("decisions=%+v", store.decisions)
	}
	kinds := map[string]bool{}
	for _, trigger := range store.decisions[0].Triggers {
		kinds[trigger.Kind] = true
	}
	if !kinds[continualoptimizer.TriggerEngineRelease] || !kinds[continualoptimizer.TriggerSchedule] {
		t.Fatalf("triggers=%+v", store.decisions[0].Triggers)
	}
	campaign := store.campaign
	if campaign.ID == "" || campaign.State != optimizationcampaign.CampaignAwaitingApproval || campaign.Intent != optimizationcampaign.IntentEvolveEndpoint || campaign.TargetDeployment != "chat" || campaign.ApprovedBy != "" {
		t.Fatalf("campaign=%+v", campaign)
	}
	if store.operation.ID != "" {
		t.Fatalf("a proposal queued work: %+v", store.operation)
	}
	origin := decodeOrigin(t, campaign)
	if !strings.Contains(origin.Reason, "vllm "+support.DefaultRuntimeVersion+" is the new pinned engine release; this endpoint runs 0.21.0") || origin.AutoRun.Approved {
		t.Fatalf("origin=%+v", origin)
	}
	// Two setups and the control at $2/h for one hour each.
	if origin.RunCost.USD == nil || *origin.RunCost.USD != 2*float64(len(campaign.Candidates)) || origin.RunCost.Basis != "current_prices" {
		t.Fatalf("run cost=%+v candidates=%d", origin.RunCost, len(campaign.Candidates))
	}
	if origin.ExpectedChange.Basis != "unknown" || origin.ExpectedChange.Low != nil {
		t.Fatalf("expected change invented: %+v", origin.ExpectedChange)
	}
	// Cost per output token comes from the price, the replica that ran, and
	// the week's output tokens; availability and margin stay unknown.
	if strings.Join(origin.UnknownEvidence, ",") != "availability,qualified_output_capacity,contribution_margin" {
		t.Fatalf("unknown evidence=%v", origin.UnknownEvidence)
	}
	var proposal struct {
		Input struct {
			WorkloadSource      string `json:"workload_source"`
			WorkloadFingerprint string `json:"workload_fingerprint"`
		} `json:"input"`
		Candidates []struct {
			Deployment struct {
				Runtime struct{ Version string } `json:"runtime"`
			} `json:"deployment"`
		} `json:"candidates"`
	}
	if err = json.Unmarshal([]byte(campaign.ProposalJSON), &proposal); err != nil || proposal.Input.WorkloadSource != "customer_observed" || proposal.Input.WorkloadFingerprint != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("proposal input=%+v err=%v", proposal.Input, err)
	}
	for _, candidate := range proposal.Candidates {
		if candidate.Deployment.Runtime.Version != support.DefaultRuntimeVersion {
			t.Fatalf("setup is not on the pinned release: %+v", proposal.Candidates)
		}
	}
	var observed continualoptimizer.Observed
	if json.Unmarshal([]byte(store.state.ObservedJSON), &observed) != nil || observed.EngineRelease != "vllm@"+support.DefaultRuntimeVersion || observed.ScheduledAt == nil || observed.HourlyUSDPerReplica != 2 {
		t.Fatalf("observed=%s", store.state.ObservedJSON)
	}

	// The next pass is due an hour later. When it runs, the proposal is still
	// waiting, so it records nothing and proposes nothing.
	if store.state.NextCheckAt == nil || store.state.NextCheckAt.Sub(time.Now()) < 59*time.Minute {
		t.Fatalf("next check=%v", store.state.NextCheckAt)
	}
	past := time.Now().Add(-time.Minute)
	store.state.NextCheckAt = &past
	if _, err = loop.RunOnce(context.Background()); err != nil || len(store.decisions) != 1 || store.completions != 2 {
		t.Fatalf("second pass err=%v decisions=%d completions=%d", err, len(store.decisions), store.completions)
	}

	// The approval-needed notification carries the reason and the cost.
	body := notification.CampaignBody("default", notification.Notice{Type: notification.EventApprovalNeeded, ID: campaign.ID + ":approval_needed", CampaignID: campaign.ID, OccurredAt: campaign.CreatedAt}, store.campaign, nil, notification.Links{})
	if body.Proposal == nil || !strings.Contains(body.Summary, "Nothing changes until you approve") || !strings.Contains(body.Summary, "0.21.0") {
		t.Fatalf("notification=%+v", body)
	}
}

// The weekly check runs on its own: with nothing else changed, an endpoint
// already on the pinned release is checked once the week has passed, and
// not before.
func TestStayOptimizedWeeklyCheckRunsOnItsOwn(t *testing.T) {
	store := newStayOptimizedFake(t, continualoptimizer.DefaultPolicy())
	var spec domain.DeploymentRevisionSpec
	_ = json.Unmarshal([]byte(store.revisions[0].SpecJSON), &spec)
	spec.RuntimeVersion = support.DefaultRuntimeVersion
	encoded, _ := json.Marshal(spec)
	store.revisions[0].SpecJSON = string(encoded)
	loop := StayOptimized{API: stayOptimizedAPI(t, store), Owner: "instance-a"}

	sixDaysAgo := time.Now().UTC().Add(-6 * 24 * time.Hour)
	observed, _ := json.Marshal(continualoptimizer.Observed{ScheduledAt: &sixDaysAgo, HourlyUSDPerReplica: 2, EngineRelease: "vllm@" + support.DefaultRuntimeVersion})
	store.state.ObservedJSON = string(observed)
	if _, err := loop.RunOnce(context.Background()); err != nil || len(store.decisions) != 0 || store.campaign.ID != "" {
		t.Fatalf("checked before the week passed: err=%v decisions=%+v", err, store.decisions)
	}

	weekAgo := time.Now().UTC().Add(-7*24*time.Hour - time.Minute)
	observed, _ = json.Marshal(continualoptimizer.Observed{ScheduledAt: &weekAgo, HourlyUSDPerReplica: 2, EngineRelease: "vllm@" + support.DefaultRuntimeVersion})
	store.state.ObservedJSON = string(observed)
	past := time.Now().Add(-time.Minute)
	store.state.NextCheckAt = &past
	if _, err := loop.RunOnce(context.Background()); err != nil || len(store.decisions) != 1 {
		t.Fatalf("weekly check did not run: err=%v decisions=%+v", err, store.decisions)
	}
	if triggers := store.decisions[0].Triggers; triggers[0].Kind != continualoptimizer.TriggerSchedule {
		t.Fatalf("triggers=%+v", triggers)
	}
	if store.campaign.ID == "" || store.campaign.State != optimizationcampaign.CampaignAwaitingApproval {
		t.Fatalf("weekly check proposed nothing: %+v", store.campaign)
	}
}

// Auto-run is off by default. Turned on, it approves a proposed run only
// under what is left of the month's cap, and the run's execution is queued
// like a person's approval. A run over the cap waits for a person.
func TestStayOptimizedAutoRunStaysUnderTheMonthlyCap(t *testing.T) {
	for _, test := range []struct {
		name     string
		spent    float64
		approved bool
	}{
		{"fits the cap", 10, true},
		{"over the cap", 45, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := continualoptimizer.DefaultPolicy()
			policy.AutoRun, policy.MonthlyAutoRunCapUSD = true, 50
			store := newStayOptimizedFake(t, policy)
			store.autoSpent = test.spent
			if _, err := (StayOptimized{API: stayOptimizedAPI(t, store), Owner: "instance-a"}).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			origin := decodeOrigin(t, store.campaign)
			if origin.AutoRun.Approved != test.approved {
				t.Fatalf("auto-run=%+v", origin.AutoRun)
			}
			if test.approved {
				if store.campaign.ApprovedBy != "stay-optimized" || store.campaign.ApprovedMaxCostUSD == nil || *store.campaign.ApprovedMaxCostUSD != *origin.RunCost.USD || store.operation.Kind != optimizationcampaign.ExecuteKind {
					t.Fatalf("campaign=%+v operation=%+v", store.campaign, store.operation)
				}
				return
			}
			if store.campaign.ApprovedBy != "" || store.operation.ID != "" || !strings.Contains(origin.AutoRun.Reason, "would pass this month's auto-run cap") {
				t.Fatalf("over-cap run approved: campaign=%+v origin=%+v", store.campaign, origin.AutoRun)
			}
		})
	}
}

// Check now runs one check from the control plane's own records, even with
// Stay optimized off, and the suggestions read shows what was proposed.
func TestStayOptimizedCheckNowAndSuggestions(t *testing.T) {
	store := newStayOptimizedFake(t, continualoptimizer.DefaultPolicy())
	store.kernels = []domain.EndpointCustomKernel{{ID: "kernel-1", Operator: "residual-rmsnorm", Family: "norm", Engine: "vllm", EngineVersion: "0.21.0", State: "qualified", ImageDigest: "sha256:" + strings.Repeat("c", 64)}}
	handler := stayOptimizedAPI(t, store).Handler()
	call := func(method, path string, body []byte) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// A dashboard that still sends the old evidence fields is accepted; the
	// fields are ignored.
	response := call(http.MethodPost, "/api/v1/deployments/chat/continual-optimization/evaluate", []byte(`{"public_profile":"public-interactive"}`))
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var checked struct {
		Decision continualoptimizer.Decision `json:"decision"`
		Mutation string                      `json:"mutation"`
		Proposal map[string]any              `json:"proposal"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &checked); err != nil || checked.Mutation != "proposed_run_created" || checked.Proposal["campaign_id"] != store.campaign.ID || checked.Decision.Triggers[0].Kind != continualoptimizer.TriggerManual {
		t.Fatalf("checked=%+v err=%v", checked, err)
	}
	if store.kernelMoves["kernel-1"] != "requalification_needed" {
		t.Fatalf("kernel pinned to the old engine was not marked: %+v", store.kernelMoves)
	}

	response = call(http.MethodGet, "/api/v1/deployments/chat/continual-optimization/suggestions", nil)
	var listed struct {
		Data []struct {
			CampaignID string                             `json:"campaign_id"`
			State      string                             `json:"state"`
			Reason     string                             `json:"reason"`
			RunCost    continualoptimizer.RunCostEstimate `json:"run_cost"`
			Kernels    []continualoptimizer.KernelUpkeep  `json:"kernels"`
		} `json:"data"`
		Check struct {
			NextCheckAt           *time.Time `json:"next_check_at"`
			ScheduledEverySeconds int        `json:"scheduled_every_seconds"`
		} `json:"check"`
		Kernels []map[string]any `json:"kernels"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &listed) != nil {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(listed.Data) != 1 || listed.Data[0].State != optimizationcampaign.CampaignAwaitingApproval || listed.Data[0].Reason == "" || listed.Data[0].RunCost.USD == nil || len(listed.Data[0].Kernels) != 1 {
		t.Fatalf("suggestions=%+v", listed.Data)
	}
	if listed.Check.NextCheckAt == nil || listed.Check.ScheduledEverySeconds != 7*24*60*60 || listed.Kernels[0]["state"] != "requalification_needed" {
		t.Fatalf("check=%+v kernels=%+v", listed.Check, listed.Kernels)
	}

	// The run's own read says it was proposed and why.
	response = call(http.MethodGet, "/api/v1/optimization/campaigns/"+store.campaign.ID, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"origin":{"kind":"stay_optimized"`) {
		t.Fatalf("campaign read status=%d body=%s", response.Code, response.Body.String())
	}

	// A second Check now while one is running is refused.
	store.leaseOwner = "instance-b"
	if response = call(http.MethodPost, "/api/v1/deployments/chat/continual-optimization/evaluate", nil); response.Code != http.StatusConflict {
		t.Fatalf("concurrent check status=%d", response.Code)
	}
}
