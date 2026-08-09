package store

import (
	"context"
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
