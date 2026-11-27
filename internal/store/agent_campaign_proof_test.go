package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// An agent's measured setups have no deployment or revision: their benchmark
// and quality rows are scoped to the agent and the setup's own identity, are
// read back by ID and by model like any other, and a row with both scopes or
// neither is refused by the database itself.
func TestAgentMeasuredEvidenceIsScopedToTheAgentsSetup(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.CreateAgent(ctx, "tenant-a", "cluster-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	throughput := 812.5
	setup := "agentsetup-0123456789abcdef01234567"
	recorded, err := s.RecordBenchmark(ctx, domain.BenchmarkResult{
		TenantID: "tenant-a", AgentID: agent.ID, DeploymentName: "support-bot", RevisionID: setup, ModelIdentity: "Qwen/Qwen3-8B",
		Runtime: "vllm", RuntimeVersion: "0.34.0", Provider: "kubernetes", GPU: "NVIDIA-L4", ComputeMode: "agent-cluster", Tool: "aiperf", ToolVersion: "0.12.0",
		WorkloadJSON: `{"measurement_method":"campaign-interleaved-control-v3"}`, ReproductionCommand: "infercrane agent window", RequestCount: 256, Succeeded: 256,
		DurationSeconds: 120, OutputTokenThroughput: &throughput,
	})
	if err != nil {
		t.Fatal(err)
	}
	byID, err := s.BenchmarksByIDs(ctx, "tenant-a", []string{recorded.ID})
	if err != nil || byID[recorded.ID].RevisionID != setup || byID[recorded.ID].AgentID != agent.ID || byID[recorded.ID].DeploymentID != "" {
		t.Fatalf("by ID: %+v err=%v", byID, err)
	}
	byModel, err := s.BenchmarksForModel(ctx, "tenant-a", "Qwen/Qwen3-8B", 10)
	if err != nil || len(byModel) != 1 || byModel[0].RevisionID != setup {
		t.Fatalf("by model: %+v err=%v", byModel, err)
	}
	if _, err = s.RecordBenchmark(ctx, domain.BenchmarkResult{TenantID: "tenant-a", AgentID: agent.ID, DeploymentName: "support-bot", RevisionID: "rev-1", ModelIdentity: "m", RequestCount: 1, Succeeded: 1, WorkloadJSON: "{}"}); err == nil {
		t.Fatal("an agent-measured row named a revision instead of its setup")
	}
	if _, err = s.ExecContext(ctx, `UPDATE benchmark_results SET revision_id='x' WHERE id=?`, recorded.ID); err == nil {
		t.Fatal("the database accepted a row with both scopes")
	}

	evidence := domain.QualityEvidence{
		RevisionID: setup, Suite: "support", SuiteVersion: "3@sha256:" + strings.Repeat("a", 64), Evaluator: "infercrane-task", EvaluatorVersion: "1",
		Score: 0.95, Passed: true, SampleCount: 40, DistributionJSON: `{"schema":"x"}`, ArtifactDigest: "sha256:" + strings.Repeat("b", 64),
		PayloadDigest: "sha256:" + strings.Repeat("c", 64), Signature: "sig", PublicKey: "key", Algorithm: "Ed25519-SHA256", KeyID: "kid",
		Source: domain.QualityEvidenceSourceInferCrane, ReferenceRevisionID: "agentserver-89abcdef0123456789abcdef", EvaluatedAt: time.Now().UTC().Truncate(time.Second),
	}
	stored, created, err := s.RecordAgentQualityEvidence(ctx, "tenant-a", agent.ID, evidence)
	if err != nil || !created {
		t.Fatalf("record: %+v created=%v err=%v", stored, created, err)
	}
	again, created, err := s.RecordAgentQualityEvidence(ctx, "tenant-a", agent.ID, evidence)
	if err != nil || created || again.ID != stored.ID {
		t.Fatalf("a repeated payload: %+v created=%v err=%v", again, created, err)
	}
	read, err := s.QualityEvidenceByIDs(ctx, "tenant-a", []string{stored.ID})
	if err != nil || read[stored.ID].RevisionID != setup || read[stored.ID].AgentID != agent.ID {
		t.Fatalf("by ID: %+v err=%v", read, err)
	}
	if _, _, err = s.RecordAgentQualityEvidence(ctx, "tenant-b", agent.ID, evidence); err == nil {
		t.Fatal("another workspace recorded evidence for this workspace's agent")
	}
}

// A campaign step that queues a window and is retried finds the window it
// queued; one key cannot name another agent's work.
func TestAgentCommandIdempotencyKeyReturnsTheQueuedCommand(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.CreateAgent(ctx, "tenant-a", "cluster-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.CreateAgent(ctx, "tenant-a", "cluster-b", "owner")
	if err != nil {
		t.Fatal(err)
	}
	command := domain.AgentCommand{TenantID: "tenant-a", AgentID: agent.ID, Kind: "measure_window", SpecJSON: `{"measure_window":{}}`, CreatedBy: "optimization-campaign", DeadlineAt: time.Now().Add(time.Hour), IdempotencyKey: "optimization:c1:window:1"}
	first, err := s.EnqueueAgentCommand(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnqueueAgentCommand(ctx, command)
	if err != nil || second.ID != first.ID || second.IdempotencyKey != command.IdempotencyKey {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	command.AgentID = other.ID
	if _, err = s.EnqueueAgentCommand(ctx, command); !errors.Is(err, ErrConflict) {
		t.Fatalf("another agent's key: %v", err)
	}
	listed, err := s.AgentCommands(ctx, "tenant-a", agent.ID, 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

// A run on an agent's endpoint names the endpoint and the agent, never a
// deployment; the database refuses a run that names both or half.
func TestAgentEndpointCampaignNamesTheEndpointAndAgent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.CreateAgent(ctx, "tenant-a", "cluster-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	campaign := domain.OptimizationCampaign{TenantID: "tenant-a", IdempotencyKey: "agent-run-1", InputDigest: strings.Repeat("d", 64), ModelIdentity: "Qwen/Qwen3-8B", Objective: "throughput", Source: "test", Intent: "evolve_endpoint", TargetEndpoint: "support-bot", AgentID: agent.ID, ProposalJSON: `{}`}
	candidates := []domain.OptimizationCandidateRun{{ProposalCandidateID: "current", Rank: 1, EvidenceState: "modeled", DeploymentSpecJSON: `{}`, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`}}
	created, _, err := s.CreateOptimizationCampaign(ctx, campaign, candidates)
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.OptimizationCampaign(ctx, "tenant-a", created.ID)
	if err != nil || read.TargetEndpoint != "support-bot" || read.AgentID != agent.ID || read.TargetDeployment != "" {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	half := campaign
	half.IdempotencyKey, half.AgentID = "agent-run-2", ""
	if _, _, err = s.CreateOptimizationCampaign(ctx, half, candidates); err == nil {
		t.Fatal("a run on an agent's endpoint without its agent was stored")
	}
	both := campaign
	both.IdempotencyKey, both.TargetDeployment = "agent-run-3", "support-bot"
	if _, _, err = s.CreateOptimizationCampaign(ctx, both, candidates); err == nil {
		t.Fatal("a run naming a deployment and an agent's endpoint was stored")
	}
	fresh := campaign
	fresh.IdempotencyKey, fresh.Intent = "agent-run-4", "new_endpoint"
	if _, _, err = s.CreateOptimizationCampaign(ctx, fresh, candidates); err == nil {
		t.Fatal("a new endpoint naming an agent's endpoint was stored")
	}
	// The same key for another endpoint is a conflict, not the first run.
	moved := campaign
	moved.TargetEndpoint = "other-bot"
	if _, _, err = s.CreateOptimizationCampaign(ctx, moved, candidates); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key, other endpoint: %v", err)
	}
	// Another tenant's agent is not found.
	if err = s.CreateTenant(ctx, "tenant-b", "Tenant B"); err != nil {
		t.Fatal(err)
	}
	foreign := campaign
	foreign.TenantID, foreign.IdempotencyKey = "tenant-b", "agent-run-5"
	if _, _, err = s.CreateOptimizationCampaign(ctx, foreign, candidates); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant's agent: %v", err)
	}
}

// A server a rollout started joins its agent's agent-managed endpoint as a
// second binding, outside the active plan; it is listed as a rollout server,
// and another agent or an observe-only endpoint cannot get one.
func TestAgentRolloutServerJoinsItsAgentsEndpoint(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.CreateAgent(ctx, "tenant-a", "cluster-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.RegisterAgentServer(ctx, "tenant-a", agent.ID, domain.AgentServerRegistration{Key: "qwen3-models-svc-8000", EndpointName: "support-bot", UpstreamModel: "Qwen/Qwen3-8B", Runtime: "vllm", Address: "http://qwen3.models.svc:8000"}); err != nil {
		t.Fatal(err)
	}
	rollout := domain.AgentServerRegistration{Key: "infercrane-abc-models-svc-8000", EndpointName: "support-bot", UpstreamModel: "Qwen/Qwen3-8B", Runtime: "vllm", Address: "http://infercrane-abc.models.svc:8000"}
	if _, _, err = s.RegisterAgentRolloutServer(ctx, "tenant-a", agent.ID, rollout); !errors.Is(err, ErrConflict) {
		t.Fatalf("an observe-only endpoint took a rollout server: %v", err)
	}
	if _, err = s.PromoteAdoptionOwnership(ctx, "tenant-a", "support-bot", "agent-managed"); err != nil {
		t.Fatal(err)
	}
	other, _, err := s.CreateAgent(ctx, "tenant-a", "cluster-b", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.RegisterAgentRolloutServer(ctx, "tenant-a", other.ID, rollout); !errors.Is(err, ErrConflict) {
		t.Fatalf("another agent added a rollout server: %v", err)
	}
	server, created, err := s.RegisterAgentRolloutServer(ctx, "tenant-a", agent.ID, rollout)
	if err != nil || !created || !server.Rollout || server.OwnershipMode != "agent-managed" || server.EndpointName != "support-bot" {
		t.Fatalf("rollout server: %+v created=%v err=%v", server, created, err)
	}
	again, created, err := s.RegisterAgentRolloutServer(ctx, "tenant-a", agent.ID, rollout)
	if err != nil || created || again.BindingID != server.BindingID {
		t.Fatalf("again: %+v created=%v err=%v", again, created, err)
	}
	servers, err := s.AgentServers(ctx, "tenant-a", agent.ID)
	if err != nil || len(servers) != 2 {
		t.Fatalf("servers: %+v err=%v", servers, err)
	}
	rollouts := 0
	for _, listed := range servers {
		if listed.Rollout {
			rollouts++
		}
	}
	if rollouts != 1 {
		t.Fatalf("listed rollout servers: %+v", servers)
	}
	// The new binding is not routed until a rollout switches the plan.
	bindings, err := s.AgentDataPlaneBindings(ctx, "tenant-a", agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings {
		if binding.BindingID == server.BindingID {
			t.Fatalf("the rollout server is routed before any switch: %+v", bindings)
		}
	}

	// The guarded switch routes to the rollout's server first, the
	// endpoint's own server as its fallback; a rollback returns to the
	// plan that served before, and the own server is never removed.
	if _, err = s.SwitchAgentRollout(ctx, "tenant-a", "support-bot", server.BindingID, "REJECT", nil, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("a switch without PASS: %v", err)
	}
	switched, err := s.SwitchAgentRollout(ctx, "tenant-a", "support-bot", server.BindingID, "PASS", []string{"agent_rollout_measured"}, map[string]string{"benchmark_id": "b1"})
	if err != nil || switched.PlanID == "" || switched.PreviousPlanID == "" || switched.EvaluationID == "" {
		t.Fatalf("switch: %+v %v", switched, err)
	}
	if again, againErr := s.SwitchAgentRollout(ctx, "tenant-a", "support-bot", server.BindingID, "PASS", nil, nil); againErr != nil || again.PlanID != switched.PlanID {
		t.Fatalf("switch again: %+v %v", again, againErr)
	}
	bindings, err = s.AgentDataPlaneBindings(ctx, "tenant-a", agent.ID)
	if err != nil || len(bindings) != 2 || bindings[0].BindingID != server.BindingID || bindings[0].RoutingPolicy != "primary-fallback" || !bindings[0].Rollout || bindings[1].Rollout {
		t.Fatalf("routes after the switch (the rollout's server first and marked, the own server not): %+v %v", bindings, err)
	}
	if err = s.RollBackAgentRollout(ctx, "tenant-a", "support-bot", switched.PlanID, switched.PreviousPlanID, []string{"release_guard_rejected"}, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.RollBackAgentRollout(ctx, "tenant-a", "support-bot", switched.PlanID, switched.PreviousPlanID, nil, nil); err != nil {
		t.Fatalf("rolling back twice: %v", err)
	}
	bindings, err = s.AgentDataPlaneBindings(ctx, "tenant-a", agent.ID)
	if err != nil || len(bindings) != 1 || bindings[0].BindingID == server.BindingID {
		t.Fatalf("routes after the rollback: %+v %v", bindings, err)
	}
}
