package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/doctor"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/support"
)

type fakeStore struct {
	operation       domain.Operation
	cancelled       bool
	err             error
	created         bool
	principal       domain.Principal
	targets         []domain.Target
	resolved        domain.ResolvedDeployment
	revisions       []domain.DeploymentRevision
	artifact        domain.ModelArtifact
	benchmarks      []domain.BenchmarkResult
	replicas        []domain.Replica
	activeOperation domain.Operation
}

func (f *fakeStore) AuthenticatePrincipal(context.Context, string) (domain.Principal, error) {
	if f.principal.ID == "" {
		return domain.Principal{}, domain.ErrNotFound
	}
	return f.principal, nil
}

func (f *fakeStore) ActiveOperationForResource(context.Context, string, string, string) (domain.Operation, error) {
	if f.activeOperation.ID == "" {
		return domain.Operation{}, domain.ErrNotFound
	}
	return f.activeOperation, nil
}

func (f *fakeStore) Operation(context.Context, string) (domain.Operation, error) {
	return f.operation, f.err
}
func (f *fakeStore) RequestOperationCancel(context.Context, string) error {
	f.cancelled = true
	return f.err
}
func (f *fakeStore) EnqueueOperation(_ context.Context, operation domain.Operation) (domain.Operation, bool, error) {
	operation.ID = "queued"
	operation.Status = "pending"
	f.operation = operation
	return operation, f.created, f.err
}
func (f *fakeStore) SubmitCloudDeployment(_ context.Context, deployment domain.Deployment, operation domain.Operation) (domain.Deployment, domain.Operation, bool, error) {
	deployment.ID = "deployment"
	operation.ID, operation.Status = "queued", "pending"
	operation.ResourceType, operation.ResourceName = "deployment", deployment.Name
	f.operation = operation
	return deployment, operation, f.created, f.err
}
func (f *fakeStore) SubmitDeploymentDelete(_ context.Context, _, name, _ string, operation domain.Operation) (domain.Operation, bool, error) {
	operation.ID, operation.Status, operation.ResourceName = "queued", "pending", name
	f.operation = operation
	return operation, f.created, f.err
}
func (f *fakeStore) ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error) {
	if f.err != nil {
		return domain.ResolvedDeployment{}, f.err
	}
	if f.resolved.Deployment.ID != "" {
		return f.resolved, nil
	}
	return domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment", Name: "qwen"}, Targets: f.targets}, nil
}
func (f *fakeStore) EventsForTenant(context.Context, string, string) ([]domain.Event, error) {
	return nil, f.err
}
func (f *fakeStore) RequestStats(context.Context, string, time.Duration) (domain.RequestStats, error) {
	return domain.RequestStats{}, f.err
}
func (f *fakeStore) ColdStartStats(context.Context, string, time.Duration) (domain.ColdStartStats, error) {
	return domain.ColdStartStats{}, f.err
}
func (f *fakeStore) ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error) {
	return f.replicas, f.err
}
func (f *fakeStore) Revisions(context.Context, string, string) ([]domain.DeploymentRevision, error) {
	return f.revisions, f.err
}
func (f *fakeStore) OperationEvents(context.Context, string, int) ([]domain.OperationEvent, error) {
	return nil, f.err
}
func (f *fakeStore) ScalingDecisionsForTenant(context.Context, string, string, int) ([]domain.ScalingDecision, error) {
	return nil, f.err
}
func (f *fakeStore) ModelArtifactForRevision(context.Context, string, string) (domain.ModelArtifact, error) {
	if f.artifact.ID != "" {
		return f.artifact, nil
	}
	return domain.ModelArtifact{}, domain.ErrNotFound
}
func (f *fakeStore) ReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.ReleaseGuardEvaluation, error) {
	return nil, f.err
}
func (f *fakeStore) ReleaseGuardPolicy(context.Context, string, string) (domain.ReleaseGuardPolicy, error) {
	return domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 20}, f.err
}
func (f *fakeStore) SetReleaseGuardPolicy(_ context.Context, _, _ string, policy domain.ReleaseGuardPolicy) (domain.ReleaseGuardPolicy, error) {
	return policy, f.err
}
func (f *fakeStore) AddTargetForTenant(_ context.Context, _ string, target domain.Target) (domain.Target, error) {
	target.ID = "target"
	return target, f.err
}
func (f *fakeStore) TargetsForTenant(context.Context, string) ([]domain.Target, error) {
	return nil, f.err
}
func (f *fakeStore) TargetForTenantByName(_ context.Context, _, name string) (domain.Target, error) {
	for _, target := range f.targets {
		if target.Name == name {
			return target, nil
		}
	}
	return domain.Target{}, domain.ErrNotFound
}
func (f *fakeStore) DeploymentsForTenant(context.Context, string) ([]domain.Deployment, error) {
	return nil, f.err
}
func (f *fakeStore) OrphanedTargetsForTenant(context.Context, string) ([]domain.Orphan, error) {
	return nil, f.err
}
func (f *fakeStore) Audit(context.Context, domain.AuditEvent) error { return nil }
func (f *fakeStore) AuditEventsForTenant(context.Context, string, time.Time, int) ([]domain.AuditEvent, error) {
	return nil, f.err
}
func (f *fakeStore) SetTenantQuota(context.Context, string, int, int, int) error { return f.err }
func (f *fakeStore) CreatePrincipalScoped(_ context.Context, tenant, name string, role authz.Role, scopes []authz.Action) (domain.Principal, string, error) {
	names := make([]string, len(scopes))
	for i, scope := range scopes {
		names[i] = string(scope)
	}
	return domain.Principal{ID: "new", TenantID: tenant, Name: name, Role: string(role), Kind: "service_account", Scopes: names}, "ic_token", f.err
}
func (f *fakeStore) RotatePrincipalForTenant(context.Context, string, string) (string, error) {
	return "ic_rotated", f.err
}
func (f *fakeStore) RevokePrincipalForTenant(context.Context, string, string) error { return f.err }
func (f *fakeStore) CreateTenant(context.Context, string, string) error             { return f.err }
func (f *fakeStore) CreateSecretReference(_ context.Context, tenant, name, resolver, reference string) (domain.SecretReference, error) {
	return domain.SecretReference{ID: "secret", TenantID: tenant, Name: name, Resolver: resolver, Reference: reference}, f.err
}
func (f *fakeStore) SecretReferencesForTenant(context.Context, string) ([]domain.SecretReference, error) {
	return []domain.SecretReference{{ID: "secret", Name: "openrouter", Resolver: "env", Reference: "OPENROUTER_API_KEY"}}, f.err
}
func (f *fakeStore) DeleteSecretReferenceForTenant(context.Context, string, string) error {
	return f.err
}
func (f *fakeStore) SetExternalTargetPolicyForTenant(_ context.Context, policy domain.ExternalTargetPolicy) (domain.ExternalTargetPolicy, error) {
	policy.ID = "policy"
	return policy, f.err
}
func (f *fakeStore) ExternalTargetPolicyForDeployment(context.Context, string, string) (domain.ExternalTargetPolicy, error) {
	return domain.ExternalTargetPolicy{ID: "policy", Enabled: true, PrivacyAcknowledged: true}, f.err
}
func (f *fakeStore) SetRouteForTenant(context.Context, string, string, string) error { return f.err }
func (f *fakeStore) RecordBenchmark(_ context.Context, result domain.BenchmarkResult) (domain.BenchmarkResult, error) {
	result.ID = "benchmark"
	f.benchmarks = append(f.benchmarks, result)
	return result, f.err
}
func (f *fakeStore) BenchmarksForDeployment(context.Context, string, string, int) ([]domain.BenchmarkResult, error) {
	return f.benchmarks, f.err
}

type fakeBenchmarkRunner struct{ config benchmark.Config }

func (f *fakeBenchmarkRunner) Run(_ context.Context, cfg benchmark.Config) (benchmark.Result, error) {
	f.config = cfg
	value := 12.5
	return benchmark.Result{Tool: "aiperf", ToolVersion: "0.9.0", Command: "aiperf profile --api-key ${INFERCRANE_API_KEY}", Requests: 10, Succeeded: 10, TTFTP95MS: &value}, nil
}
func TestOperationAPIAuthenticationAndResponse(t *testing.T) {
	store := &fakeStore{operation: domain.Operation{ID: "op", TenantID: "global", Status: "failed", ErrorCode: "provider_denied", MaxAttempts: 5}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/operations/op", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/operations/op", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"max_attempts":5`) || !strings.Contains(response.Body.String(), `"error_code":"provider_denied"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("authenticated control response may be cached: %q", response.Header().Get("Cache-Control"))
	}
}

func TestIntegrationsReturnsVersionedCapabilityEvidence(t *testing.T) {
	registry, err := integration.V02Catalog()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/integrations", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: &fakeStore{}, APIKey: "secret", Integrations: registry.Snapshot()}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, required := range []string{integration.ProviderContractV1, integration.RuntimeContractV1, "runpod-serverless", "real-runpod-serverless", "deferred"} {
		if !strings.Contains(body, required) {
			t.Fatalf("integration response missing %q: %s", required, body)
		}
	}
}

func TestDeploymentReadAPIReturnsDurableState(t *testing.T) {
	store := &fakeStore{}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/qwen", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"deployment"`) || !strings.Contains(response.Body.String(), `"revisions"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestWhoAmIReturnsAuthenticatedIdentity(t *testing.T) {
	store := &fakeStore{}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"bootstrap"`) || !strings.Contains(response.Body.String(), `"role":"admin"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestDeploymentAndOperationEventsAreTenantScoped(t *testing.T) {
	store := &fakeStore{operation: domain.Operation{ID: "op", TenantID: "global"}}
	for _, endpoint := range []string{"/api/v1/deployments/qwen/events", "/api/v1/operations/op/events"} {
		request := httptest.NewRequest(http.MethodGet, endpoint, nil)
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"data"`) {
			t.Fatalf("endpoint=%s response=%d %s", endpoint, response.Code, response.Body.String())
		}
	}
}

func TestScalingDecisionsAreReadThroughTenantAPI(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/qwen/scaling-decisions", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: &fakeStore{}, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"data"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestLifecycleStatusSeparatesServingFromConvergence(t *testing.T) {
	resolved := domain.ResolvedDeployment{
		Deployment: domain.Deployment{ID: "dep", ActiveRevisionID: "rev-1", CandidateRevisionID: "rev-2", MinReplicas: 1},
		Targets: []domain.Target{
			{ID: "ready", Health: "healthy"},
			{ID: "starting", Health: "starting"},
		},
	}
	replicas := []domain.Replica{
		{RevisionID: "rev-1", LifecycleState: "active", Health: "healthy"},
		{RevisionID: "rev-1", LifecycleState: "starting", Health: "starting"},
		{RevisionID: "rev-old", LifecycleState: "draining", Health: "healthy"},
	}
	operation := domain.Operation{ID: "op-scale", Kind: "deployment.scale", RequestJSON: `{"desired_replicas":2}`}
	status := deploymentLifecycleStatus(resolved, replicas, []domain.DeploymentRevision{{ID: "rev-2", Status: "candidate"}}, operation, true)
	if status.ServingState != "serving" || status.ConvergenceState != "converging" || status.ReadyReplicas != 1 || status.DesiredReplicas != 2 || status.ProvisioningReplicas != 1 || status.DrainingReplicas != 1 || status.CandidateState != "candidate" || status.BlockingOperationID != "op-scale" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestLifecycleStatusReportsReadyBeforeRoutePublication(t *testing.T) {
	resolved := domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", ActiveRevisionID: "rev-1", MinReplicas: 1}}
	status := deploymentLifecycleStatus(resolved, []domain.Replica{{RevisionID: "rev-1", LifecycleState: "active", Health: "healthy"}}, nil, domain.Operation{}, false)
	if status.ServingState != "ready" || status.ConvergenceState != "converged" || status.ReadyReplicas != 1 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestLifecycleStatusCountsHealthyExistingTargetsAsReadyCapacity(t *testing.T) {
	resolved := domain.ResolvedDeployment{
		Deployment: domain.Deployment{ID: "dep", MinReplicas: 1},
		Targets: []domain.Target{
			{ID: "ready-a", Health: "healthy"},
			{ID: "ready-b", Health: "healthy"},
		},
	}
	status := deploymentLifecycleStatus(resolved, nil, nil, domain.Operation{}, false)
	if status.ServingState != "serving" || status.ReadyReplicas != 2 || status.DesiredReplicas != 2 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestBenchmarkRunsThroughControlPlaneAndPersistsIdentity(t *testing.T) {
	spec := `{"model":"Qwen/Qwen3-8B","model_revision":"commit","runtime":"vllm","runtime_version":"0.10","compute_mode":"elastic","gpu":"L40S","region":"EU"}`
	store := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", Name: "qwen", ActiveRevisionID: "rev"}, Targets: []domain.Target{{Provider: "runpod"}}}, revisions: []domain.DeploymentRevision{{ID: "rev", SpecJSON: spec}}, artifact: domain.ModelArtifact{ID: "artifact", Repository: "Qwen/Qwen3-8B", ModelIdentity: "Qwen/Qwen3-8B@commit"}}
	runner := &fakeBenchmarkRunner{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/qwen/benchmarks", strings.NewReader(`{"requests":10,"concurrency":2,"random_seed":42}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", BenchmarkRunner: runner, GatewayURL: "http://gateway", AIPerfBinary: "aiperf"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"model_identity":"Qwen/Qwen3-8B@commit"`) || !strings.Contains(response.Body.String(), `"gpu_count":1`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if runner.config.APIKey != "secret" || runner.config.RandomSeed != 42 || runner.config.Model != "qwen" || runner.config.Tokenizer != "Qwen/Qwen3-8B" || len(store.benchmarks) != 1 || store.benchmarks[0].GPU != "L40S" || store.benchmarks[0].GPUCount == nil || *store.benchmarks[0].GPUCount != 1 || !strings.Contains(store.benchmarks[0].CostMetadataJSON, `"available":false`) {
		t.Fatalf("config=%#v benchmarks=%#v", runner.config, store.benchmarks)
	}
}

func TestBenchmarkNormalizesQualifiedRuntimeCloudAndObservedRegion(t *testing.T) {
	spec := `{"model":"Qwen/Qwen3-8B","runtime":"vllm","compute_mode":"elastic","cloud":"runpod","gpu":"H100"}`
	store := &fakeStore{
		resolved:  domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", Name: "qwen", ActiveRevisionID: "rev"}, Targets: []domain.Target{{Provider: "skypilot"}}},
		revisions: []domain.DeploymentRevision{{ID: "rev", SpecJSON: spec}},
		replicas:  []domain.Replica{{RevisionID: "rev", ProviderDetails: `[{"cloud":"RunPod","region":"US-CA-1"}]`}},
		artifact:  domain.ModelArtifact{ID: "artifact", Repository: "Qwen/Qwen3-8B", ModelIdentity: "Qwen/Qwen3-8B@commit"},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/qwen/benchmarks", strings.NewReader(`{"requests":10,"concurrency":2}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", BenchmarkRunner: &fakeBenchmarkRunner{}, GatewayURL: "http://gateway", AIPerfBinary: "aiperf"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(store.benchmarks) != 1 {
		t.Fatalf("response=%d %s benchmarks=%#v", response.Code, response.Body.String(), store.benchmarks)
	}
	result := store.benchmarks[0]
	if result.RuntimeVersion != support.DefaultRuntimeVersion || result.Provider != "runpod" || result.Region != "US-CA-1" {
		t.Fatalf("runtime=%q provider=%q region=%q", result.RuntimeVersion, result.Provider, result.Region)
	}
}

func TestCandidateBenchmarkUsesExplicitHealthyRevisionEndpoint(t *testing.T) {
	spec := `{"model":"Qwen/Qwen3-8B","runtime":"vllm","compute_mode":"elastic","gpu":"L40S"}`
	store := &fakeStore{
		resolved:  domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", Name: "qwen", ActiveRevisionID: "rev-active", CandidateRevisionID: "rev-candidate"}},
		revisions: []domain.DeploymentRevision{{ID: "rev-active", SpecJSON: spec}, {ID: "rev-candidate", SpecJSON: spec}},
		replicas:  []domain.Replica{{RevisionID: "rev-candidate", Ordinal: 0, Provider: "skypilot", LifecycleState: "ready", Health: "healthy", Endpoint: "https://candidate.invalid"}},
		artifact:  domain.ModelArtifact{ID: "artifact", Repository: "Qwen/Qwen3-8B", ModelIdentity: "Qwen/Qwen3-8B@commit"},
	}
	runner := &fakeBenchmarkRunner{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/qwen/benchmarks", strings.NewReader(`{"requests":10,"concurrency":2,"random_seed":42,"revision":"candidate"}`))
	request.Header.Set("Authorization", "Bearer bootstrap")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "bootstrap", BenchmarkRunner: runner, Backends: map[string]BackendMetadata{"skypilot": {APIKey: "worker-secret", APIKeyEnv: "INFERCRANE_WORKER_API_KEY"}}, GatewayURL: "http://gateway", AIPerfBinary: "aiperf"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || runner.config.Endpoint != "https://candidate.invalid" || runner.config.Model != "Qwen/Qwen3-8B" || runner.config.Tokenizer != "Qwen/Qwen3-8B" || runner.config.APIKey != "worker-secret" || runner.config.APIKeyEnv != "INFERCRANE_WORKER_API_KEY" || len(store.benchmarks) != 1 || store.benchmarks[0].RevisionID != "rev-candidate" || !strings.Contains(store.benchmarks[0].WorkloadJSON, `"direct_revision_validation":true`) {
		t.Fatalf("response=%d %s config=%#v benchmarks=%#v", response.Code, response.Body.String(), runner.config, store.benchmarks)
	}
}

func TestRouteAndTenantMutationsUseAuthenticatedAPI(t *testing.T) {
	store := &fakeStore{}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	for _, test := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPut, "/api/v1/deployments/qwen/route", `{"strategy":"round-robin"}`, http.StatusOK},
		{http.MethodPost, "/api/v1/tenants", `{"id":"tenant-a","name":"Tenant A"}`, http.StatusCreated},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("%s %s response=%d %s", test.method, test.path, response.Code, response.Body.String())
		}
	}
}

func TestApplyQueuesIdempotentOperation(t *testing.T) {
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/apply", strings.NewReader(`{"name":"prod","model":"model","targets":["gpu-a"]}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "release-1")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Location") != "/api/v1/operations/queued" || store.operation.Kind != "deployment.apply-existing" {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
}

func TestRolloutTransitionsQueueDurableOperations(t *testing.T) {
	store := &fakeStore{created: true}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	tests := []struct {
		path, body, kind string
	}{
		{"/api/v1/deployments/prod/rollouts", `{"spec":{"model":"Qwen/Qwen3-8B"}}`, "rollout.create-candidate"},
		{"/api/v1/deployments/prod/rollouts/guard/evaluate", ``, "release-guard.evaluate"},
		{"/api/v1/deployments/prod/rollouts/rev-2/promote", ``, "rollout.promote"},
		{"/api/v1/deployments/prod/rollouts/rev-2/provision", ``, "rollout.provision-candidate"},
		{"/api/v1/deployments/prod/rollouts/rev-2/reject", `{"reason":"readiness failed"}`, "rollout.reject"},
		{"/api/v1/deployments/prod/rollback", `{"revision_id":"rev-1","reason":"operator rollback"}`, "rollout.rollback"},
	}
	for i, test := range tests {
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Idempotency-Key", "rollout-"+test.kind)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted || store.operation.Kind != test.kind || store.operation.ResourceName != "prod" {
			t.Fatalf("case %d response=%d %s operation=%#v", i, response.Code, response.Body.String(), store.operation)
		}
	}
}

func TestCloudDeployPersistsAndQueuesConverge(t *testing.T) {
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(`{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","min_replicas":1,"max_replicas":4}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "deploy-qwen")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Location") != "/api/v1/operations/queued" || store.operation.Kind != "deployment.converge" || store.operation.ResourceName != "qwen" {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
	if !strings.Contains(store.operation.RequestJSON, `"tenant_id":"global"`) {
		t.Fatalf("request=%s", store.operation.RequestJSON)
	}
}

func TestPortableRuntimeDeployValidationAndPersistence(t *testing.T) {
	validWorkload := `{"image":"registry.example/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","command":["serve","--model","${MODEL}"],"protocol":"openai","port":8000,"readiness_path":"/health","models_path":"/v1/models","metrics_path":"/metrics","cancellation":"http-disconnect","drain":"connection","shutdown_grace_seconds":30}`
	for _, test := range []struct {
		name, body string
		status     int
		contains   string
	}{
		{"sglang-default", `{"name":"sg","model":"org/model","runtime":"sglang","cloud":"aws","region":"eu-central-1","gpu":"L40S"}`, http.StatusAccepted, `"image":"lmsysorg/sglang:v0.5.12@sha256:`},
		{"custom", `{"name":"custom","model":"org/model","runtime":"custom-oci","cloud":"aws","region":"eu-central-1","gpu":"L40S","workload":` + validWorkload + `}`, http.StatusAccepted, `"runtime":"custom-oci"`},
		{"mutable", `{"name":"bad","model":"org/model","runtime":"custom-oci","cloud":"aws","region":"eu-central-1","gpu":"L40S","workload":` + strings.Replace(validWorkload, "@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ":latest", 1) + `}`, http.StatusUnprocessableEntity, `pinned by @sha256`},
		{"missing", `{"name":"bad","model":"org/model","runtime":"custom-oci","cloud":"aws","region":"eu-central-1","gpu":"L40S"}`, http.StatusUnprocessableEntity, `requires an explicit workload`},
		{"port-conflict", `{"name":"bad","model":"org/model","runtime":"custom-oci","cloud":"aws","region":"eu-central-1","gpu":"L40S","port":9000,"workload":` + validWorkload + `}`, http.StatusUnprocessableEntity, `port conflicts with workload.port`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{created: true}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer secret")
			request.Header.Set("Idempotency-Key", "portable-"+test.name)
			response := httptest.NewRecorder()
			(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
			combined := response.Body.String() + store.operation.RequestJSON
			if response.Code != test.status || !strings.Contains(combined, test.contains) {
				t.Fatalf("status=%d body=%s operation=%s", response.Code, response.Body.String(), store.operation.RequestJSON)
			}
		})
	}
}

func TestServerlessDeployQueuesProviderNativeConverge(t *testing.T) {
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(`{"name":"qwen","model":"Qwen/Qwen3-8B","compute_mode":"serverless","cloud":"runpod","gpu":"L40S","min_replicas":0,"max_replicas":4}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "deploy-qwen-serverless")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.operation.Kind != "deployment.serverless.converge" || !strings.Contains(store.operation.RequestJSON, `"compute_mode":"serverless"`) {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
}

func TestDeleteQueuesDurableCleanup(t *testing.T) {
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/deployments/qwen", nil)
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "delete-qwen")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.operation.Kind != "deployment.delete" || !strings.Contains(store.operation.RequestJSON, `"deployment_id":"deployment"`) {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
}

func TestServerlessDeleteQueuesEndpointCleanup(t *testing.T) {
	store := &fakeStore{created: true, targets: []domain.Target{{Provider: "runpod-serverless"}}}
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/deployments/qwen", nil)
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "delete-qwen-serverless")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", Backends: map[string]BackendMetadata{"runpod-serverless": {Serverless: true}}}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.operation.Kind != "deployment.serverless.delete" {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
}

func TestViewerCannotApply(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "viewer", TenantID: "tenant-a", Name: "reader", Role: "viewer"}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/apply", strings.NewReader(`{"name":"prod","model":"model","targets":["gpu-a"]}`))
	request.Header.Set("Authorization", "Bearer ic_viewer")
	request.Header.Set("Idempotency-Key", "release-1")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestServiceAccountScopeRestrictsRolePermission(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "read-bot", Role: "operator", Scopes: []string{"read"}}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/apply", strings.NewReader(`{"name":"prod","model":"model","targets":["gpu-a"]}`))
	request.Header.Set("Authorization", "Bearer ic_operator")
	request.Header.Set("Idempotency-Key", "release-1")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestServiceAccountCannotRequestScopeAboveRole(t *testing.T) {
	store := &fakeStore{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/principals", strings.NewReader(`{"name":"bad-bot","role":"viewer","scopes":["deploy"]}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "exceeds role") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestSecretAPIAcceptsReferencesButNeverRawValues(t *testing.T) {
	store := &fakeStore{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", strings.NewReader(`{"name":"openrouter","resolver":"env","reference":"OPENROUTER_API_KEY"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), "OPENROUTER_API_KEY") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/secrets", strings.NewReader(`{"name":"unsafe","resolver":"env","reference":"OPENROUTER_API_KEY","value":"must-not-persist"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "must-not-persist") {
		t.Fatalf("raw value was accepted or reflected: %d %s", response.Code, response.Body.String())
	}
}

func TestOperatorCannotManageSecretReferences(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "operator", Role: "operator", Scopes: []string{"read", "deploy"}}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	request.Header.Set("Authorization", "Bearer scoped")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestExternalTargetRegistrationRequiresExternalScopeAndSafeURL(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "deploy-bot", Role: "operator", Scopes: []string{"deploy"}}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/targets", strings.NewReader(`{"name":"external","provider":"openrouter","url":"https://openrouter.ai/api","runtime":"openai","upstream_model":"provider/model"}`))
	request.Header.Set("Authorization", "Bearer scoped")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/targets", strings.NewReader(`{"name":"external","provider":"openrouter","url":"https://openrouter.ai/api?key=unsafe","runtime":"openai","upstream_model":"provider/model"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: &fakeStore{}, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "query parameters") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestExternalPolicyRequiresPrivacyAndHardBudgets(t *testing.T) {
	store := &fakeStore{targets: []domain.Target{{ID: "external", Name: "external", Provider: "openrouter"}}}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/deployments/qwen/external-policy", strings.NewReader(`{"target":"external","adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":false,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "privacy_acknowledgement_required") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPut, "/api/v1/deployments/qwen/external-policy", strings.NewReader(`{"target":"external","adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"policy"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestTargetRegistrationRejectsEmbeddedCredentials(t *testing.T) {
	store := &fakeStore{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/targets", strings.NewReader(`{"name":"gpu-a","url":"https://user:secret@worker.internal/v1"}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "without credentials") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestCancelHidesMissingOperation(t *testing.T) {
	store := &fakeStore{err: domain.ErrNotFound}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/operations/missing/cancel", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "not_found") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestDoctorDiagnosticsRunInsideAuthenticatedControlPlane(t *testing.T) {
	called := false
	handler := (API{Store: &fakeStore{}, APIKey: "secret", Diagnostics: func(_ context.Context, cloud, serverless, aws bool) doctor.Report {
		called = cloud && serverless && aws
		return doctor.Report{Ready: true, Checks: []doctor.Check{{Name: "PostgreSQL", Status: doctor.Pass, Message: "connected"}}}
	}}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/doctor?cloud=true&serverless=true&aws=true", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !called || !strings.Contains(response.Body.String(), `"ready":true`) {
		t.Fatalf("response=%d %s called=%t", response.Code, response.Body.String(), called)
	}
}

func TestErrorEnvelopeCarriesActionableTaxonomy(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/missing", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: &fakeStore{err: domain.ErrNotFound}, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	var envelope struct {
		Error struct {
			Code, Category, Remediation string
			Retryable                   bool
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusNotFound || envelope.Error.Code != "not_found" || envelope.Error.Category != "not_found" || envelope.Error.Retryable || envelope.Error.Remediation == "" {
		t.Fatalf("status=%d envelope=%#v", response.Code, envelope)
	}
}
