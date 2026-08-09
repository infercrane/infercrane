package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/workflows"
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

func TestSubmitCloudDeploymentIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "cloud-" + time.Now().UTC().Format("150405.000000000")
	request := `{"name":"` + name + `","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","runtime_version":"0.10.2"}`
	deployment, operation, created, err := s.SubmitCloudDeployment(ctx,
		domain.Deployment{Name: name, Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 4, AutoscalingEnabled: true},
		domain.Operation{Kind: "deployment.converge", IdempotencyKey: "submit-" + name, RequestJSON: request},
	)
	if err != nil || !created {
		t.Fatalf("submit cloud deployment = (%t, %v)", created, err)
	}
	if deployment.ID == "" || operation.ID == "" || operation.Status != "pending" || operation.MaxAttempts != 120 {
		t.Fatalf("submission = %#v %#v", deployment, operation)
	}
	resolved, err := s.Resolve(ctx, name)
	if err != nil || len(resolved.Targets) != 0 {
		t.Fatalf("resolve targetless desired deployment = (%#v, %v)", resolved, err)
	}
	revision, err := s.Revision(ctx, "global", name, resolved.Deployment.ActiveRevisionID)
	if err != nil || !strings.Contains(revision.SpecJSON, `"compute_mode": "elastic"`) || !strings.Contains(revision.SpecJSON, `"cloud": "runpod"`) || !strings.Contains(revision.SpecJSON, `"gpu": "L40S"`) {
		t.Fatalf("active revision spec=%s err=%v", revision.SpecJSON, err)
	}

	againDeployment, againOperation, againCreated, err := s.SubmitCloudDeployment(ctx,
		domain.Deployment{Name: name, Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 4, AutoscalingEnabled: true},
		domain.Operation{Kind: "deployment.converge", IdempotencyKey: "submit-" + name, RequestJSON: request},
	)
	if err != nil || againCreated || againDeployment.ID != deployment.ID || againOperation.ID != operation.ID {
		t.Fatalf("idempotent submission = (%#v, %#v, %t, %v)", againDeployment, againOperation, againCreated, err)
	}
}

func TestSubmitDeploymentDeleteWithdrawsDesiredStateAndQueuesCleanup(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "delete-cloud-" + time.Now().UTC().Format("150405.000000000")
	deployment, _, _, err := s.SubmitCloudDeployment(ctx,
		domain.Deployment{Name: name, Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 1},
		domain.Operation{Kind: "deployment.converge", IdempotencyKey: "create-" + name},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Finish the create transition so serialization permits delete.
	if _, err = s.ExecContext(ctx, `UPDATE operations SET status='succeeded',completed_at=NOW() WHERE resource_name=?`, name); err != nil {
		t.Fatal(err)
	}
	request := `{"deployment_id":"` + deployment.ID + `","name":"` + name + `","tenant_id":"global"}`
	operation, created, err := s.SubmitDeploymentDelete(ctx, "global", name, deployment.ID, domain.Operation{Kind: "deployment.delete", IdempotencyKey: "delete-" + name, RequestJSON: request})
	if err != nil || !created || operation.Status != "pending" {
		t.Fatalf("delete submission=(%#v,%t,%v)", operation, created, err)
	}
	var desired, observed string
	if err = s.QueryRowContext(ctx, `SELECT desired_state,observed_state FROM deployments WHERE id=?`, deployment.ID).Scan(&desired, &observed); err != nil || desired != "deleted" || observed != "deleting" {
		t.Fatalf("state=(%s,%s,%v)", desired, observed, err)
	}
	again, againCreated, err := s.SubmitDeploymentDelete(ctx, "global", name, deployment.ID, domain.Operation{Kind: "deployment.delete", IdempotencyKey: "delete-" + name, RequestJSON: request})
	if err != nil || againCreated || again.ID != operation.ID {
		t.Fatalf("idempotent delete=(%#v,%t,%v)", again, againCreated, err)
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

func TestCancellingWaitingOperationRequiresCleanupClaim(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	queued, _, err := s.EnqueueOperation(ctx, domain.Operation{Kind: "deployment.converge", ResourceType: "deployment", ResourceName: "cancel-waiting", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimOperation(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartClaimedOperation(ctx, claimed.ID, "worker-a", claimed.LeaseGeneration); err != nil {
		t.Fatal(err)
	}
	if err = s.FailClaimedOperation(ctx, claimed.ID, "worker-a", claimed.LeaseGeneration, "starting", "runtime starting", true, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = s.RequestOperationCancel(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	cleanup, err := s.ClaimOperation(ctx, "worker-b", time.Minute)
	if err != nil || !cleanup.CancelRequested {
		t.Fatalf("cleanup claim=%#v err=%v", cleanup, err)
	}
	if err = s.StartClaimedOperation(ctx, cleanup.ID, "worker-b", cleanup.LeaseGeneration); err != nil {
		t.Fatal(err)
	}
	if err = s.FailClaimedOperation(ctx, cleanup.ID, "worker-b", cleanup.LeaseGeneration, "cleanup_failed", "provider unavailable", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	current, err := s.Operation(ctx, queued.ID)
	if err != nil || current.Status != "cancelling" || !current.CancelRequested {
		t.Fatalf("operation=%#v err=%v", current, err)
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

func TestScaleToQueuesExactlyOneDurableOperation(t *testing.T) {
	s := openStore(t, context.Background())
	ctx := context.Background()
	name := "autoscale-operation"
	deployment, converge, _, err := s.SubmitCloudDeployment(ctx,
		domain.Deployment{Name: name, Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 3, AutoscalingEnabled: true},
		domain.Operation{Kind: workflows.ConvergeKind, IdempotencyKey: "converge-autoscale", RequestJSON: `{"name":"` + name + `","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global","min_replicas":1,"max_replicas":3}`},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE operations SET status='succeeded',completed_at=NOW() WHERE id=$1`, converge.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ScaleTo(ctx, deployment.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err = s.ScaleTo(ctx, deployment.ID, 2); err != nil {
		t.Fatal(err)
	}
	var count int
	var desired int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*),MAX((request_json->>'desired_replicas')::integer) FROM operations WHERE tenant_id='global' AND resource_name=$1 AND kind=$2`, name, workflows.ScaleKind).Scan(&count, &desired); err != nil {
		t.Fatal(err)
	}
	if count != 1 || desired != 2 {
		t.Fatalf("count=%d desired=%d", count, desired)
	}
}

func TestModelArtifactIsImmutablePerRevision(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "artifact-target", URL: "http://artifact.invalid", Provider: "existing", Runtime: "vllm"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "artifact-deployment", Model: "Qwen/Qwen3-8B"}, []string{"artifact-target"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(ctx, deployment.Name)
	if err != nil {
		t.Fatal(err)
	}
	deployment = resolved.Deployment
	size := int64(100)
	first, err := s.AttachModelArtifact(ctx, "global", deployment.ActiveRevisionID, domain.ModelArtifact{Source: "huggingface", Repository: "Qwen/Qwen3-8B", RequestedRevision: "main", ImmutableRevision: "0123456789abcdef0123456789abcdef01234567", ModelIdentity: "Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567", ApproximateSizeBytes: &size, CacheState: "unknown", RuntimeCompatibilityJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AttachModelArtifact(ctx, "global", deployment.ActiveRevisionID, domain.ModelArtifact{Source: "huggingface", Repository: "Qwen/Qwen3-8B", RequestedRevision: "main", ImmutableRevision: "ffffffffffffffffffffffffffffffffffffffff", ModelIdentity: "Qwen/Qwen3-8B@ffffffffffffffffffffffffffffffffffffffff", CacheState: "unknown", RuntimeCompatibilityJSON: `{}`})
	if err != nil || first.ID != second.ID || second.ImmutableRevision != first.ImmutableRevision {
		t.Fatalf("first=%#v second=%#v err=%v", first, second, err)
	}
}

func TestRequestTelemetryPersistsMeasurementsAndDimensions(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "telemetry-target", URL: "http://telemetry.invalid", Provider: "runpod", Runtime: "vllm"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "telemetry-deployment", Model: "Qwen/Qwen3-8B"}, []string{"telemetry-target"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(ctx, deployment.Name)
	if err != nil {
		t.Fatal(err)
	}
	ttft := 123.4
	input, output := 17, 23
	cold, workers, capacityObservedAt := true, 0, time.Now().Add(-2*time.Second)
	record := domain.InferenceRecord{RequestID: "req-telemetry", DeploymentID: deployment.ID, RevisionID: resolved.Deployment.ActiveRevisionID, Provider: "runpod", Runtime: "vllm", ComputeMode: "serverless", OperationName: "chat", ResponseModel: "Qwen/Qwen3-8B", StartedAt: time.Now().Add(-time.Second), StatusCode: 200, LatencyMS: 456.7, TTFTMS: &ttft, InputTokens: &input, OutputTokens: &output, Streaming: true, ColdStart: &cold, ProviderWorkersAtArrival: &workers, ProviderCapacityObservedAt: &capacityObservedAt}
	if err = s.RecordRequest(ctx, record); err != nil {
		t.Fatal(err)
	}
	var revision, provider, runtime, mode, operation, model string
	var storedTTFT, latency float64
	var storedInput, storedOutput int
	var streaming, storedCold bool
	var storedWorkers int
	var storedCapacityObservedAt time.Time
	if err = s.db.QueryRowContext(ctx, `SELECT revision_id,provider,runtime,compute_mode,operation_name,response_model,ttft_ms,latency_ms,input_tokens,output_tokens,streaming,cold_start,provider_workers_at_arrival,provider_capacity_observed_at FROM request_records WHERE request_id=$1`, record.RequestID).Scan(&revision, &provider, &runtime, &mode, &operation, &model, &storedTTFT, &latency, &storedInput, &storedOutput, &streaming, &storedCold, &storedWorkers, &storedCapacityObservedAt); err != nil {
		t.Fatal(err)
	}
	if revision != record.RevisionID || provider != "runpod" || runtime != "vllm" || mode != "serverless" || operation != "chat" || model != "Qwen/Qwen3-8B" || storedTTFT != ttft || latency != record.LatencyMS || storedInput != input || storedOutput != output || !streaming || !storedCold || storedWorkers != 0 || storedCapacityObservedAt.IsZero() {
		t.Fatalf("stored telemetry mismatch: revision=%s provider=%s runtime=%s mode=%s operation=%s model=%s ttft=%g latency=%g input=%d output=%d streaming=%t", revision, provider, runtime, mode, operation, model, storedTTFT, latency, storedInput, storedOutput, streaming)
	}
	stats, err := s.RequestStats(ctx, deployment.ID, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if stats.P50TTFTMS == nil || *stats.P50TTFTMS != ttft || stats.P95TTFTMS == nil || *stats.P95TTFTMS != ttft || stats.InputTokensPerSecond <= 0 || stats.OutputTokensPerSecond <= 0 {
		t.Fatalf("request stats=%+v", stats)
	}
	coldStats, err := s.ColdStartStats(ctx, deployment.ID, 5*time.Minute)
	if err != nil || coldStats.ClassifiedRequests != 1 || coldStats.ColdStarts != 1 || coldStats.WarmRequests != 0 || coldStats.ColdTTFTP50MS == nil || *coldStats.ColdTTFTP50MS != ttft || coldStats.ColdTTFTP95MS != nil || coldStats.BottleneckCode != "provider_capacity_or_worker_initialization" {
		t.Fatalf("cold-start stats=%+v err=%v", coldStats, err)
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
	if err = s.SetReplicaProviderIdentity(ctx, first.ID, "request-2", "resource-1"); err != nil {
		t.Fatalf("replace completed provider request: %v", err)
	}
	if err = s.SetReplicaProviderIdentity(ctx, first.ID, "request-2", "resource-2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("resource identity replacement error=%v, want conflict", err)
	}
	if err = s.ObserveReplica(ctx, first.ID, "ready", "https://worker.example", "healthy", `{"gpu":"L40S"}`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ReplicasForDeployment(ctx, "global", deployment.ID)
	if err != nil || len(rows) != 1 || rows[0].ProviderRequestID != "request-2" || rows[0].ProviderResourceID != "resource-1" || rows[0].Endpoint != "https://worker.example" || rows[0].LastObservedAt == nil {
		t.Fatalf("replicas=%#v err=%v", rows, err)
	}
}

func TestDeploymentRevisionsAreImmutableAndPromoteExplicitly(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "revision-seed", URL: "http://revision-seed", Provider: "existing", Runtime: "vllm", UpstreamModel: "model-v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "revision-prod", Model: "model-v1", MinReplicas: 1, MaxReplicas: 1}, []string{"revision-seed"}); err != nil {
		t.Fatal(err)
	}
	revisions, err := s.Revisions(ctx, "global", "revision-prod")
	if err != nil || len(revisions) != 1 || revisions[0].Number != 1 || revisions[0].Status != "active" {
		t.Fatalf("initial revisions=%#v err=%v", revisions, err)
	}
	initialSpec := revisions[0].SpecJSON
	candidate, err := s.CreateCandidateRevision(ctx, "global", "revision-prod", `{"model":"model-v2","runtime":"vllm","routing_strategy":"round-robin","min_replicas":1,"max_replicas":2,"autoscaling_enabled":true}`)
	if err != nil || candidate.Number != 2 || candidate.SourceRevisionID != revisions[0].ID {
		t.Fatalf("candidate=%#v err=%v", candidate, err)
	}
	if _, conflictErr := s.CreateCandidateRevision(ctx, "global", "revision-prod", `{"model":"other"}`); !errors.Is(conflictErr, ErrConflict) {
		t.Fatalf("second candidate error=%v, want conflict", conflictErr)
	}
	if err = s.RejectCandidateRevision(ctx, "global", "revision-prod", candidate.ID, "readiness failed"); err != nil {
		t.Fatal(err)
	}
	candidate, err = s.CreateCandidateRevision(ctx, "global", "revision-prod", `{"model":"model-v2","runtime":"vllm","routing_strategy":"round-robin","min_replicas":1,"max_replicas":2,"autoscaling_enabled":true}`)
	if err != nil || candidate.Number != 3 {
		t.Fatalf("replacement candidate=%#v err=%v", candidate, err)
	}
	if err = s.PromoteCandidateRevision(ctx, "global", "revision-prod", candidate.ID); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(ctx, "revision-prod")
	if err != nil || resolved.Deployment.Model != "model-v2" || resolved.Deployment.MaxReplicas != 2 || !resolved.Deployment.AutoscalingEnabled {
		t.Fatalf("promoted deployment=%#v err=%v", resolved.Deployment, err)
	}
	if err = s.RollbackRevision(ctx, "global", "revision-prod", revisions[0].ID, "operator rollback"); err != nil {
		t.Fatal(err)
	}
	resolved, err = s.Resolve(ctx, "revision-prod")
	if err != nil || resolved.Deployment.Model != "model-v1" {
		t.Fatalf("rolled back deployment=%#v err=%v", resolved.Deployment, err)
	}
	rows, err := s.Revisions(ctx, "global", "revision-prod")
	if err != nil || len(rows) != 3 || rows[2].SpecJSON != initialSpec || rows[2].Status != "active" {
		t.Fatalf("revision history=%#v err=%v", rows, err)
	}
}

func TestRevisionTransitionsAreReplaySafeForDurableOperation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "replay-target", URL: "http://replay-target", Provider: "existing", Runtime: "vllm", UpstreamModel: "model-v1"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "replay-rollout", Model: "model-v1"}, []string{"replay-target"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(ctx, deployment.Name)
	if err != nil {
		t.Fatal(err)
	}
	initialRevisionID := resolved.Deployment.ActiveRevisionID
	operation, _, err := s.EnqueueOperation(ctx, domain.Operation{TenantID: "global", Kind: workflows.RolloutCreateKind, ResourceType: "deployment", ResourceName: deployment.Name, IdempotencyKey: "replay-candidate"})
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"model":"model-v2","runtime":"vllm","routing_strategy":"round-robin","min_replicas":1,"max_replicas":1,"autoscaling_enabled":false}`
	first, err := s.EnsureCandidateRevision(ctx, "global", deployment.Name, spec, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.EnsureCandidateRevision(ctx, "global", deployment.Name, spec, operation.ID)
	if err != nil || again.ID != first.ID {
		t.Fatalf("replayed candidate=%#v first=%#v err=%v", again, first, err)
	}
	if err = s.PromoteCandidateRevision(ctx, "global", deployment.Name, first.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.PromoteCandidateRevision(ctx, "global", deployment.Name, first.ID); err != nil {
		t.Fatalf("replayed promotion: %v", err)
	}
	if err = s.RollbackRevision(ctx, "global", deployment.Name, initialRevisionID, "acceptance rollback"); err != nil {
		t.Fatal(err)
	}
	if err = s.RollbackRevision(ctx, "global", deployment.Name, initialRevisionID, "acceptance rollback"); err != nil {
		t.Fatalf("replayed rollback: %v", err)
	}
	var transitionEvents int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployment_events WHERE deployment_id=$1 AND event_type IN ('revision_candidate_created','revision_promoted','revision_rolled_back')`, deployment.ID).Scan(&transitionEvents); err != nil {
		t.Fatal(err)
	}
	if transitionEvents != 3 {
		t.Fatalf("transition events=%d, want exactly 3 after replays", transitionEvents)
	}
}

func TestReleaseGuardPersistsDeterministicCandidateDecision(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "guard-target", URL: "http://guard-target", Provider: "existing", Runtime: "vllm", UpstreamModel: "model-v1"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "guard-prod", Model: "model-v1"}, []string{"guard-target"})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.CreateCandidateRevision(ctx, "global", deployment.Name, `{"model":"model-v2"}`)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.SetReleaseGuardPolicy(ctx, "global", deployment.Name, domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 5, MaxTTFTRegressionPercent: 10, MaxLatencyRegressionPercent: 12, MaxErrorRateIncrease: .005, MaxOutputThroughputDropPercent: 15})
	if err != nil || policy.MinimumRequests != 5 || policy.MaxTTFTRegressionPercent != 10 {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
	evaluation, err := s.EvaluateReleaseGuard(ctx, "global", deployment.Name, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Decision != "REJECT" || evaluation.CandidateRevisionID != candidate.ID || !strings.Contains(evaluation.ReasonCodesJSON, "candidate_not_ready") || !strings.Contains(evaluation.PolicyJSON, `"minimum_requests":5`) {
		t.Fatalf("evaluation=%+v", evaluation)
	}
	rows, err := s.ReleaseGuardEvaluations(ctx, "global", deployment.Name, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != evaluation.ID || rows[0].PolicyJSON == "" || rows[0].MetricsJSON == "" {
		t.Fatalf("evaluations=%+v err=%v", rows, err)
	}
}

func TestGuardedPromotionAtomicallySwitchesRevisionAndTargets(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "guarded-old", URL: "http://guarded-old", Provider: "existing", Runtime: "vllm", UpstreamModel: "model-v1"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "guarded-prod", Model: "model-v1"}, []string{"guarded-old"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(ctx, deployment.Name)
	if err != nil {
		t.Fatal(err)
	}
	activeRevisionID := resolved.Deployment.ActiveRevisionID
	candidate, err := s.CreateCandidateRevision(ctx, "global", deployment.Name, `{"model":"model-v2","runtime":"vllm","routing_strategy":"round-robin","min_replicas":1,"max_replicas":1,"compute_mode":"elastic","cloud":"runpod","gpu":"L40S"}`)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.AddTarget(ctx, domain.Target{Name: "guarded-prod-" + candidate.ID[:8] + "-r0", URL: "http://guarded-new", Provider: "skypilot", Runtime: "vllm", UpstreamModel: "model-v2"})
	if err != nil {
		t.Fatal(err)
	}
	replica, _, err := s.EnsureReplicaIntent(ctx, domain.Replica{TenantID: "global", DeploymentID: deployment.ID, RevisionID: candidate.ID, Ordinal: 0, ExternalKey: deployment.ID + "-" + candidate.ID + "-r0", Provider: "skypilot"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveReplica(ctx, replica.ID, "ready", target.URL, "healthy", `{}`, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetReleaseGuardPolicy(ctx, "global", deployment.Name, domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, MaxTTFTRegressionPercent: 15, MaxLatencyRegressionPercent: 15, MaxErrorRateIncrease: .01, MaxOutputThroughputDropPercent: 20}); err != nil {
		t.Fatal(err)
	}
	activeTTFT, candidateTTFT := 100.0, 105.0
	if err = s.RecordRequest(ctx, domain.InferenceRecord{RequestID: "guarded-active", DeploymentID: deployment.ID, RevisionID: activeRevisionID, OperationName: "chat", StartedAt: time.Now(), StatusCode: 200, LatencyMS: 200, TTFTMS: &activeTTFT}); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordRequest(ctx, domain.InferenceRecord{RequestID: "guarded-candidate", DeploymentID: deployment.ID, RevisionID: candidate.ID, OperationName: "chat", StartedAt: time.Now(), StatusCode: 200, LatencyMS: 205, TTFTMS: &candidateTTFT}); err != nil {
		t.Fatal(err)
	}
	evaluation, err := s.EvaluateReleaseGuard(ctx, "global", deployment.Name, 5*time.Minute)
	if err != nil || evaluation.Decision != "ACCEPT" {
		t.Fatalf("evaluation=%+v err=%v", evaluation, err)
	}
	if err = s.PromoteGuardedCandidate(ctx, "global", deployment.Name, candidate.ID, []string{target.Name}); err != nil {
		t.Fatal(err)
	}
	if err = s.PromoteGuardedCandidate(ctx, "global", deployment.Name, candidate.ID, []string{target.Name}); err != nil {
		t.Fatalf("replayed promotion: %v", err)
	}
	resolved, err = s.Resolve(ctx, deployment.Name)
	if err != nil || resolved.Deployment.ActiveRevisionID != candidate.ID || resolved.Deployment.CandidateRevisionID != "" || resolved.Deployment.Model != "model-v2" || len(resolved.Targets) != 1 || resolved.Targets[0].ID != target.ID {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
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
	if _, err := s.db.ExecContext(ctx, `TRUNCATE principals,tenant_quotas,audit_events,operations,scaling_decisions,scaling_policies,request_records,deployment_events,router_generations,deployment_targets,replicas,deployments,targets,model_artifacts CASCADE`); err != nil {
		t.Fatalf("reset test database: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}
