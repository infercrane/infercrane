package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

func TestTargetAndDeploymentLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)

	first, err := s.AddTarget(ctx, domain.Target{
		Name: "worker-a", URL: "http://127.0.0.1:8001/", Provider: "existing",
		Runtime: "vllm", UpstreamModel: "Qwen/Qwen2.5-7B",
	})
	if err != nil {
		t.Fatalf("add target: %v", err)
	}
	if first.URL != "http://127.0.0.1:8001" {
		t.Fatalf("URL = %q, want normalized URL", first.URL)
	}
	if first.ID == "" || first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Fatal("target identity and timestamps must be populated")
	}

	again, err := s.AddTarget(ctx, domain.Target{
		Name: "worker-a", URL: "http://127.0.0.1:8001", Provider: "existing",
		Runtime: "vllm", UpstreamModel: "Qwen/Qwen2.5-7B",
	})
	if err != nil || again.ID != first.ID {
		t.Fatalf("idempotent add = (%q, %v), want %q", again.ID, err, first.ID)
	}

	deployment, err := s.CreateDeployment(ctx, domain.Deployment{
		Name: "qwen-prod", Model: "qwen", RoutingStrategy: "round-robin",
	}, []string{"worker-a", "worker-a"})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if deployment.ID == "" || deployment.CreatedAt.IsZero() {
		t.Fatal("deployment identity and timestamps must be populated")
	}

	resolved, err := s.Resolve(ctx, "qwen-prod")
	if err != nil {
		t.Fatalf("resolve deployment: %v", err)
	}
	if len(resolved.Targets) != 1 || resolved.Targets[0].ID != first.ID {
		t.Fatalf("resolved targets = %#v", resolved.Targets)
	}

	events, err := s.Events(ctx, "qwen-prod")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "deployment_created" || events[0].CreatedAt.IsZero() {
		t.Fatalf("events = %#v", events)
	}
}

func TestCreateDeploymentRejectsIncompatibleTargets(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	for _, target := range []domain.Target{
		{Name: "a", URL: "http://a", Provider: "existing", Runtime: "vllm", UpstreamModel: "model-a"},
		{Name: "b", URL: "http://b", Provider: "existing", Runtime: "vllm", UpstreamModel: "model-b"},
	} {
		if _, err := s.AddTarget(ctx, target); err != nil {
			t.Fatalf("add target %s: %v", target.Name, err)
		}
	}

	_, err := s.CreateDeployment(ctx, domain.Deployment{Name: "bad", Model: "alias"}, []string{"a", "b"})
	if err == nil {
		t.Fatal("expected incompatible upstream models to be rejected")
	}
	if _, resolveErr := s.Resolve(ctx, "bad"); !errors.Is(resolveErr, ErrNotFound) {
		t.Fatalf("rolled-back deployment resolve error = %v", resolveErr)
	}
}

func TestApplyDeploymentConvergesTargetMembership(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	for _, name := range []string{"a", "b"} {
		if _, err := s.AddTarget(ctx, domain.Target{Name: name, URL: "http://" + name + ":8000", Provider: "existing", Runtime: "vllm", UpstreamModel: "model"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "prod", Model: "model", RoutingStrategy: "round-robin"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	applied, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "prod", Model: "model", RoutingStrategy: "power-of-two"}, []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(ctx, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if applied.RoutingStrategy != "power-of-two" || len(resolved.Targets) != 1 || resolved.Targets[0].Name != "b" {
		t.Fatalf("deployment not converged: %#v %#v", applied, resolved.Targets)
	}
}

func TestTargetConflict(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "a", URL: "http://worker", Runtime: "vllm"}); err != nil {
		t.Fatalf("add target: %v", err)
	}
	_, err := s.AddTarget(ctx, domain.Target{Name: "b", URL: "http://worker/", Runtime: "vllm"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
}

func TestOperationLifecycleIsIdempotentAndRetryable(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	request := domain.Operation{Kind: "apply", ResourceType: "deployment", ResourceName: "qwen", IdempotencyKey: "request-1", RequestJSON: `{"model":"qwen"}`}
	first, created, err := s.StartOperation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first operation should be created")
	}
	again, created, err := s.StartOperation(ctx, request)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("idempotent operation = %#v, %v", again, err)
	}
	if err := s.FailOperation(ctx, first.ID, "provider_busy", "retry later", true); err != nil {
		t.Fatal(err)
	}
	retried, err := s.RetryOperation(ctx, first.ID)
	if err != nil || retried.Attempt != 2 || retried.Status != "running" {
		t.Fatalf("retry = %#v, %v", retried, err)
	}
	if err := s.FinishOperation(ctx, first.ID, `{"deployment":"qwen"}`); err != nil {
		t.Fatal(err)
	}
	finished, err := s.Operation(ctx, first.ID)
	if err != nil || finished.Status != "succeeded" || finished.Progress != 100 {
		t.Fatalf("finished = %#v, %v", finished, err)
	}
}

func TestOrphanedTargetsOnlyReturnsProvisionedUnusedTargets(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	target, err := s.AddTarget(ctx, domain.Target{Name: "orphan", URL: "http://orphan:8000", Provider: "skypilot", Runtime: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProvisionedTarget(ctx, target.ID, "cluster-orphan", `{}`); err != nil {
		t.Fatal(err)
	}
	orphans, err := s.OrphanedTargets(ctx)
	if err != nil || len(orphans) != 1 || orphans[0].ProviderResourceID != "cluster-orphan" {
		t.Fatalf("orphans = %#v, %v", orphans, err)
	}
}

func TestTenantQuotaRejectsDeploymentBeyondReplicaLimit(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.SetTenantQuota(ctx, "global", 10, 1, 1000); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := s.AddTarget(ctx, domain.Target{Name: name, URL: "http://quota-" + name, Provider: "existing", Runtime: "vllm"}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "too-large", Model: "model"}, []string{"a", "b"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error=%v, want quota conflict", err)
	}
}

func TestOperationQueueLeasesAndRecoversExpiredWork(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	queued, created, err := s.EnqueueOperation(ctx, domain.Operation{Kind: "apply", ResourceType: "deployment", ResourceName: "leased", IdempotencyKey: "lease-1", MaxAttempts: 3})
	if err != nil || !created {
		t.Fatalf("enqueue=%#v created=%t err=%v", queued, created, err)
	}
	claimed, err := s.ClaimOperation(ctx, "worker-a", time.Minute)
	if err != nil || claimed.ID != queued.ID || claimed.Attempt != 1 || claimed.LeaseOwner != "worker-a" {
		t.Fatalf("claim=%#v err=%v", claimed, err)
	}
	if _, err = s.ClaimOperation(ctx, "worker-b", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active lease was claimed twice: %v", err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE operations SET lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=?`, queued.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.ClaimOperation(ctx, "worker-b", time.Minute)
	if err != nil || recovered.Attempt != 2 || recovered.LeaseOwner != "worker-b" {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	if err = s.StartClaimedOperation(ctx, recovered.ID, "worker-b", recovered.LeaseGeneration); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteClaimedOperation(ctx, recovered.ID, "worker-b", recovered.LeaseGeneration, `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
}

func TestStaleLeaseCannotCheckpointOrFinish(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	queued, _, err := s.EnqueueOperation(ctx, domain.Operation{Kind: "apply", ResourceType: "deployment", ResourceName: "fenced"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimOperation(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE operations SET lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=?`, queued.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckpointClaimedOperation(ctx, first.ID, "worker-a", first.LeaseGeneration, "provision", "running", `{}`, 25, "expired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired checkpoint error=%v, want ErrNotFound", err)
	}
	second, err := s.ClaimOperation(ctx, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.LeaseGeneration <= first.LeaseGeneration {
		t.Fatalf("lease generation did not advance: first=%d second=%d", first.LeaseGeneration, second.LeaseGeneration)
	}
	if err = s.CheckpointClaimedOperation(ctx, first.ID, "worker-a", first.LeaseGeneration, "provision", "running", `{}`, 50, "stale"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale checkpoint error=%v, want ErrNotFound", err)
	}
	if err = s.CompleteClaimedOperation(ctx, first.ID, "worker-a", first.LeaseGeneration, `{}`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale completion error=%v, want ErrNotFound", err)
	}
	if err = s.StartClaimedOperation(ctx, second.ID, "worker-b", second.LeaseGeneration); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckpointClaimedOperation(ctx, second.ID, "worker-b", second.LeaseGeneration, "provision", "succeeded", `{"resource_id":"gpu-1"}`, 50, "GPU provisioned"); err != nil {
		t.Fatal(err)
	}
	events, err := s.OperationEvents(ctx, second.ID, 10)
	if err != nil || len(events) != 1 || events[0].Type != "step.succeeded" || events[0].Message != "GPU provisioned" {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestCancellingQueuedOperationPreventsClaim(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	queued, _, err := s.EnqueueOperation(ctx, domain.Operation{Kind: "apply", ResourceType: "deployment", ResourceName: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RequestOperationCancel(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.Operation(ctx, queued.ID)
	if err != nil || current.Status != "cancelled" {
		t.Fatalf("operation=%#v err=%v", current, err)
	}
	if _, err = s.ClaimOperation(ctx, "worker", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancelled operation was claimable: %v", err)
	}
}

func TestDeploymentLifecycleMutationsAreSerialized(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	type result struct {
		op      domain.Operation
		created bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, key := range []string{"release-a", "release-b"} {
		go func() {
			<-start
			op, created, err := s.EnqueueOperation(ctx, domain.Operation{Kind: "deployment.converge", ResourceType: "deployment", ResourceName: "prod", IdempotencyKey: key})
			results <- result{op: op, created: created, err: err}
		}()
	}
	close(start)
	var createdCount, conflicts int
	var winner domain.Operation
	for range 2 {
		item := <-results
		if item.created && item.err == nil {
			createdCount++
			winner = item.op
		}
		if errors.Is(item.err, ErrConflict) {
			conflicts++
		}
	}
	if createdCount != 1 || conflicts != 1 {
		t.Fatalf("created=%d conflicts=%d", createdCount, conflicts)
	}
	claimed, err := s.ClaimOperation(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartClaimedOperation(ctx, claimed.ID, "worker", claimed.LeaseGeneration); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteClaimedOperation(ctx, winner.ID, "worker", claimed.LeaseGeneration, `{}`); err != nil {
		t.Fatal(err)
	}
	if _, nextCreated, err := s.EnqueueOperation(ctx, domain.Operation{Kind: "deployment.delete", ResourceType: "deployment", ResourceName: "prod", IdempotencyKey: "delete-a"}); err != nil || !nextCreated {
		t.Fatalf("new transition after completion: created=%t err=%v", nextCreated, err)
	}
}

func TestPrincipalCredentialRotationAndRevocation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	principal, token, err := s.CreatePrincipal(ctx, "tenant-a", "operator", authz.Operator)
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := s.AuthenticatePrincipal(ctx, token)
	if err != nil || authenticated.ID != principal.ID || authenticated.TenantID != "tenant-a" {
		t.Fatalf("authenticated=%#v err=%v", authenticated, err)
	}
	rotated, err := s.RotatePrincipal(ctx, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticatePrincipal(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old credential remained valid: %v", err)
	}
	if _, err = s.AuthenticatePrincipal(ctx, rotated); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokePrincipal(ctx, principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticatePrincipal(ctx, rotated); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked credential remained valid: %v", err)
	}
}

func TestReplicaIntentAndProviderIdentityAreIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "seed", URL: "http://seed", Provider: "existing", Runtime: "vllm", UpstreamModel: "model"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "replicas", Model: "model"}, []string{"seed"})
	if err != nil {
		t.Fatal(err)
	}
	intent := domain.Replica{TenantID: "global", DeploymentID: deployment.ID, Ordinal: 0, ExternalKey: "infercrane-replicas-r0", Provider: "skypilot"}
	first, created, err := s.EnsureReplicaIntent(ctx, intent)
	if err != nil || !created || first.LifecycleState != "pending" {
		t.Fatalf("first=%#v created=%t err=%v", first, created, err)
	}
	again, created, err := s.EnsureReplicaIntent(ctx, intent)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("again=%#v created=%t err=%v", again, created, err)
	}
	if err = s.SetReplicaProviderIdentity(ctx, first.ID, "request-1", "resource-1"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetReplicaProviderIdentity(ctx, first.ID, "request-1", "resource-1"); err != nil {
		t.Fatalf("repeating identical identity: %v", err)
	}
	if err = s.SetReplicaProviderIdentity(ctx, first.ID, "request-2", "resource-2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("identity replacement error=%v, want conflict", err)
	}
	if err = s.ObserveReplica(ctx, first.ID, "ready", "https://worker.example", "healthy", `{"gpu":"L40S"}`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ReplicasForDeployment(ctx, "global", deployment.ID)
	if err != nil || len(rows) != 1 || rows[0].ProviderResourceID != "resource-1" || rows[0].Endpoint != "https://worker.example" || rows[0].LastObservedAt == nil {
		t.Fatalf("replicas=%#v err=%v", rows, err)
	}
}

func TestTenantResourcesCanReuseNamesWithoutCrossTenantVisibility(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := s.CreateTenant(ctx, tenant, tenant); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddTargetForTenant(ctx, tenant, domain.Target{Name: "gpu", URL: "http://gpu:8000", Provider: "existing", Runtime: "vllm", UpstreamModel: "model"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyDeploymentForTenant(ctx, tenant, domain.Deployment{Name: "prod", Model: "model"}, []string{"gpu"}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.DeploymentsForTenant(ctx, "tenant-a")
	if err != nil || len(a) != 1 || a[0].TenantID != "tenant-a" {
		t.Fatalf("tenant a deployments=%#v err=%v", a, err)
	}
	b, err := s.ResolveForTenant(ctx, "tenant-b", "prod")
	if err != nil || b.Deployment.TenantID != "tenant-b" {
		t.Fatalf("tenant b deployment=%#v err=%v", b, err)
	}
}

func openStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	url := os.Getenv("INFERCRANE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("INFERCRANE_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	s, err := Open(ctx, url, Options{MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `TRUNCATE principals,tenant_quotas,audit_events,operations,scaling_decisions,scaling_policies,request_records,deployment_events,router_generations,deployment_targets,replicas,deployments,targets CASCADE`); err != nil {
		t.Fatalf("reset test database: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}
