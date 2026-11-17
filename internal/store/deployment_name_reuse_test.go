package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/workflows"
)

// startingProvider keeps every replica allocating, like a GPU host that never
// finishes booting, so a first deploy stays in progress until it is cancelled.
type startingProvider struct {
	mu       sync.Mutex
	existing map[string]bool
}

func (p *startingProvider) Handle(key string) provision.ProviderHandle {
	return provision.ProviderHandle{ExternalKey: key, ResourceID: "resource-" + key}
}
func (p *startingProvider) EnsureReplica(_ context.Context, spec provision.ReplicaSpec) (provision.ProviderHandle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.existing[spec.ExternalKey] = true
	return provision.ProviderHandle{ExternalKey: spec.ExternalKey, ResourceID: "resource-" + spec.ExternalKey}, nil
}
func (p *startingProvider) ObserveReplica(_ context.Context, handle provision.ProviderHandle, _ int) (provision.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.existing[handle.ExternalKey] {
		return provision.Observation{}, nil
	}
	return provision.Observation{Exists: true, State: "starting", Details: `{}`}, nil
}
func (p *startingProvider) DeleteReplica(_ context.Context, handle provision.ProviderHandle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.existing, handle.ExternalKey)
	return nil
}

func nameReuseRequest(name string) string {
	return fmt.Sprintf(`{"name":%q,"model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global","min_replicas":1,"max_replicas":1}`, name)
}

func submitNameReuse(ctx context.Context, s *Store, name, key string) (domain.Deployment, domain.Operation, bool, error) {
	return s.SubmitCloudDeployment(ctx,
		domain.Deployment{Name: name, Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 1},
		domain.Operation{Kind: workflows.ConvergeKind, IdempotencyKey: key, RequestJSON: nameReuseRequest(name)},
	)
}

func nameReuseWorker(s *Store, provider workflows.ReplicaProvider) operations.Worker {
	handlers := workflows.QualifiedCloudHandlers(s, provider, acceptanceRuntime{}, acceptanceArtifactResolver{})
	return operations.Worker{Repository: s, Handlers: handlers, Owner: "name-reuse-worker", Lease: 30 * time.Second, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Jitter: func(d time.Duration) time.Duration { return d }}
}

func runOperation(t *testing.T, ctx context.Context, s *Store, worker operations.Worker, id, want string) {
	t.Helper()
	if worked, err := worker.Once(ctx); err != nil || !worked {
		operation, _ := s.Operation(ctx, id)
		t.Fatalf("worker worked=%t err=%v operation=%+v", worked, err, operation)
	}
	operation, err := s.Operation(ctx, id)
	if err != nil || operation.Status != want {
		t.Fatalf("operation %s status=%q message=%q err=%v, want %s", id, operation.Status, operation.Message, err, want)
	}
}

func deleteNameReuse(t *testing.T, ctx context.Context, s *Store, worker operations.Worker, deployment domain.Deployment, key string) {
	t.Helper()
	request, _ := json.Marshal(workflows.DeleteRequest{DeploymentID: deployment.ID, Name: deployment.Name, TenantID: "global"})
	operation, created, err := s.SubmitDeploymentDelete(ctx, "global", deployment.Name, deployment.ID, domain.Operation{Kind: workflows.DeleteKind, IdempotencyKey: key, RequestJSON: string(request)})
	if err != nil || !created {
		t.Fatalf("delete submission=(%#v,%t,%v)", operation, created, err)
	}
	runOperation(t, ctx, s, worker, operation.ID, "succeeded")
}

func endpointRow(t *testing.T, ctx context.Context, s *Store, id string) (desired, observed, owner string) {
	t.Helper()
	var ownerID sql.NullString
	if err := s.QueryRowContext(ctx, `SELECT desired_state,observed_state,owner_deployment_id FROM endpoints WHERE id=?`, id).Scan(&desired, &observed, &ownerID); err != nil {
		t.Fatalf("endpoint %s: %v", id, err)
	}
	return desired, observed, ownerID.String
}

func liveEndpointNames(t *testing.T, ctx context.Context, s *Store) []string {
	t.Helper()
	endpoints, err := s.EndpointsForTenant(ctx, "global")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		names = append(names, endpoint.Name)
	}
	return names
}

func TestDeletedDeploymentNameCanBeDeployedAgain(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	worker := nameReuseWorker(s, &acceptanceProvider{existing: map[string]bool{}})
	const name = "qwen-again"

	first, converge, _, err := submitNameReuse(ctx, s, name, "first-deploy")
	if err != nil {
		t.Fatal(err)
	}
	runOperation(t, ctx, s, worker, converge.ID, "succeeded")
	firstEndpoint, err := s.ResolveEndpointForTenant(ctx, "global", name)
	if err != nil || len(firstEndpoint.Bindings) != 1 || firstEndpoint.Bindings[0].DeploymentID != first.ID {
		t.Fatalf("first endpoint=%#v err=%v", firstEndpoint, err)
	}
	if _, _, owner := endpointRow(t, ctx, s, firstEndpoint.Endpoint.ID); owner != first.ID {
		t.Fatalf("first endpoint owner=%q, want %q", owner, first.ID)
	}

	deleteNameReuse(t, ctx, s, worker, first, "first-delete")
	if _, err = s.ResolveForTenant(ctx, "global", name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted deployment still resolves: %v", err)
	}
	if desired, observed, _ := endpointRow(t, ctx, s, firstEndpoint.Endpoint.ID); desired != "deleted" || observed != "deleted" {
		t.Fatalf("owned endpoint after delete=(%s,%s), want deleted", desired, observed)
	}
	if names := liveEndpointNames(t, ctx, s); len(names) != 0 {
		t.Fatalf("endpoints left after delete=%v", names)
	}
	var payload string
	if err = s.QueryRowContext(ctx, `SELECT payload_json::text FROM deployment_events WHERE deployment_id=? AND event_type='deployment_deleted'`, first.ID).Scan(&payload); err != nil || !strings.Contains(payload, `"retired_endpoints": ["`+name+`"]`) {
		t.Fatalf("deletion event payload=%s err=%v", payload, err)
	}
	var audited int
	if err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id='global' AND action='endpoint.delete' AND resource_name=? AND payload_json->>'deployment_id'=?`, name, first.ID).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("endpoint retirement audit=%d err=%v", audited, err)
	}

	second, converge, created, err := submitNameReuse(ctx, s, name, "second-deploy")
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("redeploy=(%#v,%t,%v)", second, created, err)
	}
	runOperation(t, ctx, s, worker, converge.ID, "succeeded")
	resolved, err := s.ResolveForTenant(ctx, "global", name)
	if err != nil || resolved.Deployment.ID != second.ID || len(resolved.Targets) != 1 {
		t.Fatalf("redeployed deployment=%#v err=%v", resolved, err)
	}
	secondEndpoint, err := s.ResolveEndpointForTenant(ctx, "global", name)
	if err != nil || secondEndpoint.Endpoint.ID == firstEndpoint.Endpoint.ID || len(secondEndpoint.Bindings) != 1 || secondEndpoint.Bindings[0].DeploymentID != second.ID {
		t.Fatalf("redeployed endpoint=%#v err=%v", secondEndpoint, err)
	}
	if secondEndpoint.LogicalModel.ID != firstEndpoint.LogicalModel.ID {
		t.Fatalf("redeploy created logical model %q instead of reusing %q", secondEndpoint.LogicalModel.ID, firstEndpoint.LogicalModel.ID)
	}

	// The first deployment's history stays attached to its own identity.
	var desired, observed string
	if err = s.QueryRowContext(ctx, `SELECT desired_state,observed_state FROM deployments WHERE id=?`, first.ID).Scan(&desired, &observed); err != nil || desired != "deleted" || observed != "deleted" {
		t.Fatalf("first deployment history=(%s,%s,%v)", desired, observed, err)
	}
	var firstEvents int
	if err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployment_events WHERE deployment_id=?`, first.ID).Scan(&firstEvents); err != nil || firstEvents < 2 {
		t.Fatalf("first deployment events=%d err=%v", firstEvents, err)
	}
	currentEvents, err := s.EventsForTenant(ctx, "global", name)
	if err != nil || len(currentEvents) == 0 {
		t.Fatalf("current events=%d err=%v", len(currentEvents), err)
	}
	for _, event := range currentEvents {
		if event.DeploymentID != second.ID {
			t.Fatalf("current events include another deployment's event %#v", event)
		}
	}
	revisions, err := s.Revisions(ctx, "global", name)
	if err != nil || len(revisions) != 1 || revisions[0].DeploymentID != second.ID {
		t.Fatalf("revisions mixed deployments: %#v err=%v", revisions, err)
	}

	// A third round proves lookups stay unambiguous with several deleted rows.
	deleteNameReuse(t, ctx, s, worker, second, "second-delete")
	third, converge, created, err := submitNameReuse(ctx, s, name, "third-deploy")
	if err != nil || !created || third.ID == first.ID || third.ID == second.ID {
		t.Fatalf("third deploy=(%#v,%t,%v)", third, created, err)
	}
	runOperation(t, ctx, s, worker, converge.ID, "succeeded")
	if names := liveEndpointNames(t, ctx, s); len(names) != 1 || names[0] != name {
		t.Fatalf("live endpoints=%v, want [%s]", names, name)
	}
	deployments, err := s.DeploymentsForTenant(ctx, "global")
	if err != nil || len(deployments) != 1 || deployments[0].ID != third.ID {
		t.Fatalf("live deployments=%#v err=%v", deployments, err)
	}
	var rows int
	if err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments WHERE tenant_id='global' AND name=?`, name).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("deployment rows for name=%d err=%v, want 3", rows, err)
	}

	// Replaying the first submission still reports that submission's operation.
	replayed, replayedOperation, replayCreated, err := submitNameReuse(ctx, s, name, "first-deploy")
	if err != nil || replayCreated || replayedOperation.ID == "" || replayed.Name != name {
		t.Fatalf("replay=(%#v,%#v,%t,%v)", replayed, replayedOperation, replayCreated, err)
	}
}

func TestCancelledFirstDeployReleasesNameAndEndpoint(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	const name = "qwen-cancelled"
	stuck := nameReuseWorker(s, &startingProvider{existing: map[string]bool{}})

	first, converge, _, err := submitNameReuse(ctx, s, name, "stuck-deploy")
	if err != nil {
		t.Fatal(err)
	}
	firstEndpoint, err := s.ResolveEndpointForTenant(ctx, "global", name)
	if err != nil {
		t.Fatal(err)
	}
	runOperation(t, ctx, s, stuck, converge.ID, "waiting")
	if err = s.RequestOperationCancel(ctx, converge.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE operations SET lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=?`, converge.ID); err != nil {
		t.Fatal(err)
	}
	runOperation(t, ctx, s, stuck, converge.ID, "cancelled")

	if _, err = s.ResolveForTenant(ctx, "global", name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancelled first deploy still resolves: %v", err)
	}
	if desired, observed, _ := endpointRow(t, ctx, s, firstEndpoint.Endpoint.ID); desired != "deleted" || observed != "deleted" {
		t.Fatalf("cancelled deploy left its endpoint (%s,%s)", desired, observed)
	}
	if names := liveEndpointNames(t, ctx, s); len(names) != 0 {
		t.Fatalf("endpoints left after cancel=%v", names)
	}
	replicas, err := s.ReplicasForDeployment(ctx, "global", first.ID)
	if err != nil || len(replicas) != 1 || replicas[0].LifecycleState != "deleted" {
		t.Fatalf("cancelled replicas=%#v err=%v", replicas, err)
	}

	ready := nameReuseWorker(s, &acceptanceProvider{existing: map[string]bool{}})
	second, converge, created, err := submitNameReuse(ctx, s, name, "deploy-after-cancel")
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("deploy after cancel=(%#v,%t,%v)", second, created, err)
	}
	runOperation(t, ctx, s, ready, converge.ID, "succeeded")
	endpoint, err := s.ResolveEndpointForTenant(ctx, "global", name)
	if err != nil || endpoint.Endpoint.DesiredState != "serving" || len(endpoint.Bindings) != 1 || endpoint.Bindings[0].DeploymentID != second.ID {
		t.Fatalf("endpoint after redeploy=%#v err=%v", endpoint, err)
	}
}

func TestLiveDeploymentNameStillConflicts(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	const name = "qwen-live"
	live, converge, _, err := submitNameReuse(ctx, s, name, "live-deploy")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = submitNameReuse(ctx, s, name, "other-deploy"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("live name submission error=%v", err)
	}

	// Concurrent first deploys of one name create exactly one deployment.
	const contenders = 8
	var wg sync.WaitGroup
	results := make(chan error, contenders)
	for i := range contenders {
		wg.Go(func() {
			_, _, _, submitErr := submitNameReuse(ctx, s, "qwen-race", fmt.Sprintf("race-%d", i))
			results <- submitErr
		})
	}
	wg.Wait()
	close(results)
	createdCount := 0
	for submitErr := range results {
		switch {
		case submitErr == nil:
			createdCount++
		case !errors.Is(submitErr, ErrConflict):
			t.Fatalf("concurrent submission error=%v", submitErr)
		}
	}
	if createdCount != 1 {
		t.Fatalf("concurrent submissions created %d deployments, want 1", createdCount)
	}

	// A name whose deletion has not finished still belongs to that deployment.
	if _, err = s.ExecContext(ctx, `UPDATE operations SET status='succeeded',completed_at=NOW() WHERE id=?`, converge.ID); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(workflows.DeleteRequest{DeploymentID: live.ID, Name: name, TenantID: "global"})
	deleteOperation, _, err := s.SubmitDeploymentDelete(ctx, "global", name, live.ID, domain.Operation{Kind: workflows.DeleteKind, IdempotencyKey: "live-delete", RequestJSON: string(request)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = submitNameReuse(ctx, s, name, "during-delete"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "still being deleted") {
		t.Fatalf("submission during deletion error=%v", err)
	}
	// A health observation racing the deletion must not take the name back.
	if err = s.DeleteDeploymentForTenant(ctx, "global", name); err != nil {
		t.Fatal(err)
	}
	if err = s.SetDeploymentState(ctx, live.ID, "healthy"); err != nil {
		t.Fatal(err)
	}
	var observed string
	if err = s.QueryRowContext(ctx, `SELECT observed_state FROM deployments WHERE id=?`, live.ID).Scan(&observed); err != nil || observed != "deleted" {
		t.Fatalf("deleted deployment observed=%q err=%v", observed, err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE operations SET status='succeeded',completed_at=NOW() WHERE id=?`, deleteOperation.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, created, submitErr := submitNameReuse(ctx, s, name, "after-delete"); submitErr != nil || !created {
		t.Fatalf("submission after deletion=(%t,%v)", created, submitErr)
	}
}

func TestOperatorEndpointsSurviveDeploymentDeletion(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	target, err := s.AddTargetForTenant(ctx, "global", domain.Target{Name: "fallback", URL: "http://fallback.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "Qwen/Qwen3-8B"})
	if err != nil {
		t.Fatal(err)
	}

	// An operator endpoint that already uses the name blocks the deployment
	// with a precise reason instead of a misleading deployment conflict.
	model, err := s.CreateLogicalModel(ctx, "global", domain.LogicalModel{Name: "coder"})
	if err != nil {
		t.Fatal(err)
	}
	var environmentID string
	if _, err = s.ExecContext(ctx, `INSERT INTO environments(id,tenant_id,name,created_at,updated_at) VALUES('env-production','global','production',NOW(),NOW()) ON CONFLICT(tenant_id,name) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if err = s.QueryRowContext(ctx, `SELECT id FROM environments WHERE tenant_id='global' AND name='production'`).Scan(&environmentID); err != nil {
		t.Fatal(err)
	}
	operatorEndpoint, err := s.CreateEndpoint(ctx, "global", domain.Endpoint{Name: "taken", LogicalModelID: model.ID, EnvironmentID: environmentID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = submitNameReuse(ctx, s, "taken", "taken-deploy"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "endpoint taken already exists") {
		t.Fatalf("deploy over operator endpoint error=%v", err)
	}

	// Operator bindings to a deployment, and owned endpoints the operator gave
	// another backend, are left for the operator when the deployment goes.
	deployment, converge, _, err := submitNameReuse(ctx, s, "qwen-shared", "shared-deploy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE operations SET status='succeeded',completed_at=NOW() WHERE id=?`, converge.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: operatorEndpoint.ID, Name: "primary", Kind: "deployment", OwnershipMode: "lifecycle-managed", DeploymentID: deployment.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: deployment.ID + "-endpoint", Name: "fallback", Kind: "external", OwnershipMode: "traffic-managed", TargetID: target.ID}); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteDeploymentForTenant(ctx, "global", "qwen-shared"); err != nil {
		t.Fatal(err)
	}
	if desired, _, _ := endpointRow(t, ctx, s, operatorEndpoint.ID); desired != "serving" {
		t.Fatalf("operator endpoint desired=%s after deployment deletion", desired)
	}
	if desired, _, _ := endpointRow(t, ctx, s, deployment.ID+"-endpoint"); desired != "serving" {
		t.Fatalf("owned endpoint with another backend desired=%s after deployment deletion", desired)
	}
	if _, _, _, err = submitNameReuse(ctx, s, "qwen-shared", "shared-again"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "endpoint qwen-shared already exists") {
		t.Fatalf("deploy over kept endpoint error=%v", err)
	}
}

func TestExistingTargetDeploymentIsRecreatedAfterDeletion(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "a", URL: "http://a.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "model"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "prod", Model: "model"}, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteDeployment(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	second, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "prod", Model: "model"}, []string{"a"})
	if err != nil || second.ID == first.ID || second.DesiredState != "running" {
		t.Fatalf("apply after delete=(%#v,%v), want a new deployment", second, err)
	}
	endpoint, err := s.ResolveEndpointForTenant(ctx, "global", "prod")
	if err != nil || len(endpoint.Bindings) != 1 || endpoint.Bindings[0].DeploymentID != second.ID {
		t.Fatalf("endpoint after apply=%#v err=%v", endpoint, err)
	}
}

func TestManagedDeploymentNameGetsANewHoldAfterDeletion(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	tenant, name := "managed-name-reuse", "qwen-managed"
	if err := s.CreateTenant(ctx, tenant, "Managed Name Reuse"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditManagedWallet(ctx, tenant, "managed-name-reuse-credit", "test credit", 5_000_000); err != nil {
		t.Fatal(err)
	}
	hold := domain.ManagedSpendReservation{
		Provider: "runpod", SupplierHourlyMicrousd: 600_000, RetailHourlyMicrousd: 1_000_000,
		ReservedMicrousd: 1_166_667, GrossMarginBPS: 4_000, RuntimeLimitSeconds: 3_600,
		CleanupAllowanceSeconds: 600, PricingJSON: `{"source":"test provider quote"}`,
	}
	submit := func(key string) (domain.Deployment, domain.ManagedSpendReservation, bool, error) {
		deployment, _, reservation, created, err := s.SubmitManagedCloudDeployment(ctx,
			domain.Deployment{TenantID: tenant, Name: name, Model: "Qwen/Qwen3-8B", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1},
			domain.Operation{TenantID: tenant, Kind: workflows.ConvergeKind, IdempotencyKey: key, RequestJSON: `{"cloud":"runpod","gpu":"L40S"}`},
			hold)
		return deployment, reservation, created, err
	}
	first, firstHold, created, err := submit("managed-first")
	if err != nil || !created {
		t.Fatalf("first managed deploy=(%t,%v)", created, err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE operations SET status='succeeded',completed_at=NOW() WHERE tenant_id=?`, tenant); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteDeploymentForTenant(ctx, tenant, name); err != nil {
		t.Fatal(err)
	}
	released, err := s.ManagedSpendReservation(ctx, tenant, "deployment", name)
	if err != nil || released.ID != firstHold.ID || released.State != "released" {
		t.Fatalf("first hold after delete=%+v err=%v", released, err)
	}

	second, secondHold, created, err := submit("managed-second")
	if err != nil || !created || second.ID == first.ID || secondHold.ID == firstHold.ID {
		t.Fatalf("managed redeploy=(%#v,%+v,%t,%v)", second, secondHold, created, err)
	}
	current, err := s.ManagedSpendReservation(ctx, tenant, "deployment", name)
	if err != nil || current.ID != secondHold.ID || current.State != "reserved" {
		t.Fatalf("current hold=%+v err=%v", current, err)
	}
	wallet, err := s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.ReservedMicrousd != hold.ReservedMicrousd {
		t.Fatalf("wallet after redeploy=%+v err=%v", wallet, err)
	}
	if _, _, _, err = submit("managed-third"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second live managed deploy error=%v", err)
	}
}

func TestReusableNameMigrationRetiresEndpointsOfDeletedDeployments(t *testing.T) {
	ctx, databaseURL, _ := isolatedMigrationDatabase(t)
	previous, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()
	if _, err = previous.ExecContext(ctx, `CREATE TABLE schema_migrations(version TEXT PRIMARY KEY, checksum TEXT, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range embeddedMigrations(t) {
		if migration.name >= "083_" {
			break
		}
		if _, err = previous.ExecContext(ctx, string(migration.body)); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
		if _, err = previous.ExecContext(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, migration.name, migration.checksum); err != nil {
			t.Fatal(err)
		}
	}
	// Before 083 a cancelled or deleted deployment left its endpoint serving.
	for _, statement := range []string{
		`INSERT INTO deployments(id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,tenant_id,created_at,updated_at) VALUES('stuck','vast-qwen','Qwen/Qwen3-8B','vllm','round-robin','deleted','deleted',1,1,false,'global',NOW(),NOW())`,
		`UPDATE endpoints SET observed_state='degraded' WHERE id='stuck-endpoint'`,
		`INSERT INTO deployments(id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,tenant_id,created_at,updated_at) VALUES('kept','shared','Qwen/Qwen3-8B','vllm','round-robin','deleted','deleted',1,1,false,'global',NOW(),NOW())`,
		`INSERT INTO targets(id,name,url,provider,runtime,health,tenant_id,created_at,updated_at) VALUES('fallback','fallback','http://fallback.invalid','existing','vllm','healthy','global',NOW(),NOW())`,
		`INSERT INTO backend_bindings(id,tenant_id,endpoint_id,name,kind,ownership_mode,target_id,created_at,updated_at) VALUES('kept-fallback','global','kept-endpoint','fallback','external','traffic-managed','fallback',NOW(),NOW())`,
		`INSERT INTO deployments(id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,tenant_id,created_at,updated_at) VALUES('live','live','Qwen/Qwen3-8B','vllm','round-robin','running','healthy',1,1,false,'global',NOW(),NOW())`,
	} {
		if _, err = previous.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err = previous.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(ctx, databaseURL, Options{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	for id, want := range map[string][3]string{
		"stuck-endpoint": {"deleted", "deleted", "stuck"},
		"kept-endpoint":  {"serving", "pending", "kept"},
		"live-endpoint":  {"serving", "pending", "live"},
	} {
		var desired, observed string
		var owner sql.NullString
		if err = upgraded.QueryRowContext(ctx, `SELECT desired_state,observed_state,owner_deployment_id FROM endpoints WHERE id=?`, id).Scan(&desired, &observed, &owner); err != nil {
			t.Fatal(err)
		}
		if got := [3]string{desired, observed, owner.String}; got != want {
			t.Fatalf("%s after upgrade=%v, want %v", id, got, want)
		}
	}
	deployment, _, created, err := submitNameReuse(ctx, upgraded, "vast-qwen", "vast-qwen-again")
	if err != nil || !created || deployment.ID == "stuck" {
		t.Fatalf("redeploy stuck name=(%#v,%t,%v)", deployment, created, err)
	}
}
