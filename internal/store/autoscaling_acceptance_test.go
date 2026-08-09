package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/autoscale"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/router"
	"github.com/infercrane/infercrane/internal/workflows"
)

type acceptanceSignals struct{ waiting, running float64 }

func (s *acceptanceSignals) Signals(context.Context, string) (autoscale.Signals, error) {
	return autoscale.Signals{Waiting: s.waiting, Running: s.running}, nil
}

type acceptanceProvider struct{ existing map[string]bool }

func (p *acceptanceProvider) Handle(key string) provision.ProviderHandle {
	return provision.ProviderHandle{ExternalKey: key, ResourceID: "resource-" + key}
}
func (p *acceptanceProvider) EnsureReplica(_ context.Context, spec provision.ReplicaSpec) (provision.ProviderHandle, error) {
	p.existing[spec.ExternalKey] = true
	return provision.ProviderHandle{ExternalKey: spec.ExternalKey, ResourceID: "resource-" + spec.ExternalKey, RequestID: "request-" + spec.ExternalKey}, nil
}
func (p *acceptanceProvider) ObserveReplica(_ context.Context, handle provision.ProviderHandle, _ int) (provision.Observation, error) {
	if !p.existing[handle.ExternalKey] {
		return provision.Observation{}, nil
	}
	return provision.Observation{Exists: true, State: "ready", Endpoint: "http://runtime.invalid/" + handle.ExternalKey, Details: `{}`}, nil
}
func (p *acceptanceProvider) DeleteReplica(_ context.Context, handle provision.ProviderHandle) error {
	delete(p.existing, handle.ExternalKey)
	return nil
}

type acceptanceRuntime struct{}

func (acceptanceRuntime) Inspect(context.Context, string) (bool, map[string]struct{}) {
	return true, map[string]struct{}{"Qwen/Qwen3-8B": {}}
}

type acceptanceArtifactResolver struct{}

func (acceptanceArtifactResolver) Resolve(context.Context, string, string) (domain.ModelArtifact, error) {
	return domain.ModelArtifact{Source: "huggingface", Repository: "Qwen/Qwen3-8B", RequestedRevision: "main", ImmutableRevision: "0123456789abcdef0123456789abcdef01234567", ModelIdentity: "Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567", CacheState: "unknown", RuntimeCompatibilityJSON: `{}`}, nil
}

func TestDurableAutoscalingAcceptanceOneToTwoToOne(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "autoscale-acceptance"
	requestJSON := fmt.Sprintf(`{"name":%q,"model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global","min_replicas":1,"max_replicas":2}`, name)
	deployment, _, _, err := s.SubmitCloudDeployment(ctx,
		domain.Deployment{Name: name, Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 2, AutoscalingEnabled: true},
		domain.Operation{Kind: workflows.ConvergeKind, IdempotencyKey: "initial", RequestJSON: requestJSON},
	)
	if err != nil {
		t.Fatal(err)
	}
	provider := &acceptanceProvider{existing: map[string]bool{}}
	handlers := workflows.CloudHandlers(s, provider, acceptanceRuntime{}, acceptanceArtifactResolver{})
	worker := operations.Worker{Repository: s, Handlers: handlers, Owner: "acceptance-worker", Lease: time.Second, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Jitter: func(d time.Duration) time.Duration { return d }}
	if worked, workerErr := worker.Once(ctx); workerErr != nil || !worked {
		t.Fatalf("initial converge worked=%t err=%v", worked, workerErr)
	}
	assertReplicaCount(t, s, deployment.ID, 1)
	if err = s.SetDeploymentState(ctx, deployment.ID, "healthy"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetScalingPolicy(ctx, deployment.ID, autoscale.Policy{Enabled: true, MinReplicas: 1, MaxReplicas: 2, QueueThreshold: 1, LowLoadThreshold: 0, ScaleUpIntervals: 1, ScaleDownIntervals: 1}); err != nil {
		t.Fatal(err)
	}
	signals := &acceptanceSignals{waiting: 2}
	controller := autoscale.Controller{Repository: s, Signals: signals, Fleet: s, Now: func() time.Time { return time.Unix(100, 0) }}
	if err = controller.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if worked, workerErr := worker.Once(ctx); workerErr != nil || !worked {
		t.Fatalf("scale up worked=%t err=%v", worked, workerErr)
	}
	assertReplicaCount(t, s, deployment.ID, 2)

	if err = s.SetDeploymentState(ctx, deployment.ID, "healthy"); err != nil {
		t.Fatal(err)
	}
	signals.waiting = 0
	controller.Now = func() time.Time { return time.Unix(200, 0) }
	if err = controller.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if worked, workerErr := worker.Once(ctx); workerErr != nil || !worked {
		t.Fatalf("drain pass worked=%t err=%v", worked, workerErr)
	}
	if !provider.existing[deployment.ID+"-r1"] {
		t.Fatal("provider resource was deleted before router withdrawal was proven")
	}
	resolved, err := s.Resolve(ctx, name)
	if err != nil || len(resolved.Targets) != 1 {
		t.Fatalf("reduced target set=%#v err=%v", resolved.Targets, err)
	}
	hash := router.WorkerSetHash("round-robin", []string{resolved.Targets[0].URL})
	if _, err = s.RecordGeneration(ctx, domain.RouterGeneration{DeploymentID: deployment.ID, OwnerID: "acceptance-router", Generation: 1, Strategy: "round-robin", WorkerSetHash: hash, InternalEndpoint: "http://router.invalid"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE operations SET next_attempt_at=NOW() WHERE kind=$1 AND status='waiting'`, workflows.ScaleKind); err != nil {
		t.Fatal(err)
	}
	if worked, workerErr := worker.Once(ctx); workerErr != nil || !worked {
		t.Fatalf("scale down worked=%t err=%v", worked, workerErr)
	}
	assertReplicaCount(t, s, deployment.ID, 1)
	if provider.existing[deployment.ID+"-r1"] {
		t.Fatal("drained provider resource still exists")
	}
	decisions, err := s.ScalingDecisionsForTenant(ctx, "global", name, 10)
	if err != nil || len(decisions) < 2 || decisions[0].Action != "scale_down" {
		t.Fatalf("decisions=%#v err=%v", decisions, err)
	}
}

func TestGuardedRolloutAcceptanceResumesAfterCutoverRestart(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "rollout-acceptance"
	requestJSON := fmt.Sprintf(`{"name":%q,"model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global","min_replicas":1,"max_replicas":1}`, name)
	deployment, _, _, err := s.SubmitCloudDeployment(ctx, domain.Deployment{Name: name, Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 1}, domain.Operation{Kind: workflows.ConvergeKind, IdempotencyKey: "rollout-initial", RequestJSON: requestJSON})
	if err != nil {
		t.Fatal(err)
	}
	provider := &acceptanceProvider{existing: map[string]bool{}}
	handlers := workflows.RolloutHandlers(s)
	for kind, handler := range workflows.ReleaseGuardHandlers(s) {
		handlers[kind] = handler
	}
	for kind, handler := range workflows.CloudHandlers(s, provider, acceptanceRuntime{}, acceptanceArtifactResolver{}) {
		handlers[kind] = handler
	}
	worker := operations.Worker{Repository: s, Handlers: handlers, Owner: "rollout-worker-before-restart", Lease: time.Second, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Jitter: func(d time.Duration) time.Duration { return d }}
	if worked, workerErr := worker.Once(ctx); workerErr != nil || !worked {
		t.Fatalf("initial converge worked=%t err=%v", worked, workerErr)
	}
	resolved, err := s.Resolve(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	activeRevisionID := resolved.Deployment.ActiveRevisionID
	candidate, err := s.CreateCandidateRevision(ctx, "global", name, `{"model":"Qwen/Qwen3-8B","runtime":"vllm","routing_strategy":"round-robin","min_replicas":1,"max_replicas":1,"compute_mode":"elastic","cloud":"runpod","gpu":"H100"}`)
	if err != nil {
		t.Fatal(err)
	}
	provisionRequest := workflows.RolloutRequest{Name: name, CandidateID: candidate.ID, TenantID: "global", Actor: "acceptance"}
	provisionJSON, _ := json.Marshal(provisionRequest)
	if _, _, err = s.EnqueueOperation(ctx, domain.Operation{TenantID: "global", Kind: workflows.RolloutProvisionKind, ResourceType: "deployment", ResourceName: name, IdempotencyKey: "rollout-provision", RequestJSON: string(provisionJSON), MaxAttempts: 120}); err != nil {
		t.Fatal(err)
	}
	if worked, workerErr := worker.Once(ctx); workerErr != nil || !worked {
		t.Fatalf("candidate provision worked=%t err=%v", worked, workerErr)
	}
	if _, err = s.SetReleaseGuardPolicy(ctx, "global", name, domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, MaxTTFTRegressionPercent: 15, MaxLatencyRegressionPercent: 15, MaxErrorRateIncrease: .01, MaxOutputThroughputDropPercent: 20}); err != nil {
		t.Fatal(err)
	}
	activeTTFT, candidateTTFT := 100.0, 105.0
	for _, record := range []domain.InferenceRecord{
		{RequestID: "rollout-active-request", DeploymentID: deployment.ID, RevisionID: activeRevisionID, OperationName: "chat", StartedAt: time.Now(), StatusCode: 200, LatencyMS: 200, TTFTMS: &activeTTFT},
		{RequestID: "rollout-candidate-request", DeploymentID: deployment.ID, RevisionID: candidate.ID, OperationName: "chat", StartedAt: time.Now(), StatusCode: 200, LatencyMS: 205, TTFTMS: &candidateTTFT},
	} {
		if err = s.RecordRequest(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	evaluation, err := s.EvaluateReleaseGuard(ctx, "global", name, 5*time.Minute)
	if err != nil || evaluation.Decision != "ACCEPT" {
		t.Fatalf("evaluation=%+v err=%v", evaluation, err)
	}
	promoteJSON, _ := json.Marshal(workflows.RolloutRequest{Name: name, CandidateID: candidate.ID, TenantID: "global", Actor: "acceptance"})
	promote, _, err := s.EnqueueOperation(ctx, domain.Operation{TenantID: "global", Kind: workflows.RolloutPromoteKind, ResourceType: "deployment", ResourceName: name, IdempotencyKey: "rollout-promote", RequestJSON: string(promoteJSON), MaxAttempts: 120})
	if err != nil {
		t.Fatal(err)
	}
	if worked, workerErr := worker.Once(ctx); workerErr != nil || !worked {
		t.Fatalf("cutover pass worked=%t err=%v", worked, workerErr)
	}
	current, err := s.Operation(ctx, promote.ID)
	if err != nil || current.Status != "waiting" || current.ErrorCode != "router_cutover_pending" {
		t.Fatalf("promotion operation=%+v err=%v", current, err)
	}
	if !provider.existing[deployment.ID+"-r0"] {
		t.Fatal("old provider resource was deleted before candidate router generation")
	}
	resolved, err = s.Resolve(ctx, name)
	if err != nil || resolved.Deployment.ActiveRevisionID != candidate.ID || len(resolved.Targets) != 1 {
		t.Fatalf("cutover desired state=%+v err=%v", resolved, err)
	}
	hash := router.WorkerSetHash("round-robin", []string{resolved.Targets[0].URL})
	if _, err = s.RecordGeneration(ctx, domain.RouterGeneration{DeploymentID: deployment.ID, OwnerID: "rollout-router", Generation: 1, Strategy: "round-robin", WorkerSetHash: hash, InternalEndpoint: "http://candidate-router.invalid"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE operations SET next_attempt_at=NOW() WHERE id=$1`, promote.ID); err != nil {
		t.Fatal(err)
	}
	restartedWorker := operations.Worker{Repository: s, Handlers: handlers, Owner: "rollout-worker-after-restart", Lease: time.Second, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Jitter: func(d time.Duration) time.Duration { return d }}
	if worked, workerErr := restartedWorker.Once(ctx); workerErr != nil || !worked {
		t.Fatalf("post-restart drain worked=%t err=%v", worked, workerErr)
	}
	current, err = s.Operation(ctx, promote.ID)
	if err != nil || current.Status != "succeeded" {
		t.Fatalf("completed promotion=%+v err=%v", current, err)
	}
	if provider.existing[deployment.ID+"-r0"] || !provider.existing[deployment.ID+"-"+candidate.ID+"-r0"] {
		t.Fatalf("provider resources=%#v", provider.existing)
	}
	assertReplicaCount(t, s, deployment.ID, 1)
}

func assertReplicaCount(t *testing.T, s *Store, deploymentID string, want int) {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM replicas WHERE deployment_id=$1 AND lifecycle_state!='deleted'`, deploymentID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("replica count=%d want=%d", count, want)
	}
}
