package controlapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

type fakeStore struct {
	operation domain.Operation
	cancelled bool
	err       error
	created   bool
	principal domain.Principal
}

func (f *fakeStore) AuthenticatePrincipal(context.Context, string) (domain.Principal, error) {
	if f.principal.ID == "" {
		return domain.Principal{}, domain.ErrNotFound
	}
	return f.principal, nil
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
	return domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment", Name: "qwen"}}, nil
}
func (f *fakeStore) EventsForTenant(context.Context, string, string) ([]domain.Event, error) {
	return nil, f.err
}
func (f *fakeStore) RequestStats(context.Context, string, time.Duration) (domain.RequestStats, error) {
	return domain.RequestStats{}, f.err
}
func (f *fakeStore) ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error) {
	return nil, f.err
}
func (f *fakeStore) Revisions(context.Context, string, string) ([]domain.DeploymentRevision, error) {
	return nil, f.err
}
func (f *fakeStore) OperationEvents(context.Context, string, int) ([]domain.OperationEvent, error) {
	return nil, f.err
}
func (f *fakeStore) ScalingDecisionsForTenant(context.Context, string, string, int) ([]domain.ScalingDecision, error) {
	return nil, f.err
}
func (f *fakeStore) ModelArtifactForRevision(context.Context, string, string) (domain.ModelArtifact, error) {
	return domain.ModelArtifact{}, domain.ErrNotFound
}
func (f *fakeStore) AddTargetForTenant(_ context.Context, _ string, target domain.Target) (domain.Target, error) {
	target.ID = "target"
	return target, f.err
}
func (f *fakeStore) TargetsForTenant(context.Context, string) ([]domain.Target, error) {
	return nil, f.err
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
func (f *fakeStore) CreatePrincipal(_ context.Context, tenant, name string, role authz.Role) (domain.Principal, string, error) {
	return domain.Principal{ID: "new", TenantID: tenant, Name: name, Role: string(role)}, "ic_token", f.err
}
func (f *fakeStore) RotatePrincipalForTenant(context.Context, string, string) (string, error) {
	return "ic_rotated", f.err
}
func (f *fakeStore) RevokePrincipalForTenant(context.Context, string, string) error  { return f.err }
func (f *fakeStore) CreateTenant(context.Context, string, string) error              { return f.err }
func (f *fakeStore) SetRouteForTenant(context.Context, string, string, string) error { return f.err }
func TestOperationAPIAuthenticationAndResponse(t *testing.T) {
	store := &fakeStore{operation: domain.Operation{ID: "op", TenantID: "global", Status: "running", MaxAttempts: 5}}
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
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"max_attempts":5`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
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
