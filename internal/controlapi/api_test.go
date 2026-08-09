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
func (f *fakeStore) RevokePrincipalForTenant(context.Context, string, string) error { return f.err }
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
