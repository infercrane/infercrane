// Package controlapi exposes stable, authenticated control-plane HTTP contracts.
package controlapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/workflows"
)

type Store interface {
	Operation(context.Context, string) (domain.Operation, error)
	RequestOperationCancel(context.Context, string) error
	EnqueueOperation(context.Context, domain.Operation) (domain.Operation, bool, error)
	AddTargetForTenant(context.Context, string, domain.Target) (domain.Target, error)
	TargetsForTenant(context.Context, string) ([]domain.Target, error)
	DeploymentsForTenant(context.Context, string) ([]domain.Deployment, error)
	OrphanedTargetsForTenant(context.Context, string) ([]domain.Orphan, error)
	Audit(context.Context, domain.AuditEvent) error
	AuditEventsForTenant(context.Context, string, time.Time, int) ([]domain.AuditEvent, error)
	SetTenantQuota(context.Context, string, int, int, int) error
	CreatePrincipal(context.Context, string, string, authz.Role) (domain.Principal, string, error)
	RotatePrincipalForTenant(context.Context, string, string) (string, error)
	RevokePrincipalForTenant(context.Context, string, string) error
}
type API struct {
	Store         Store
	APIKey        string
	Authenticator interface {
		AuthenticatePrincipal(context.Context, string) (domain.Principal, error)
	}
}
type identityKey struct{}

func (a API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/operations/{id}", a.auth(authz.Read, a.operation))
	mux.HandleFunc("POST /api/v1/operations/{id}/cancel", a.auth(authz.Deploy, a.cancel))
	mux.HandleFunc("POST /api/v1/deployments/apply", a.auth(authz.Deploy, a.applyDeployment))
	mux.HandleFunc("GET /api/v1/deployments", a.auth(authz.Read, a.deployments))
	mux.HandleFunc("GET /api/v1/targets", a.auth(authz.Read, a.targets))
	mux.HandleFunc("POST /api/v1/targets", a.auth(authz.Deploy, a.addTarget))
	mux.HandleFunc("GET /api/v1/orphans", a.auth(authz.Read, a.orphans))
	mux.HandleFunc("GET /api/v1/audit-events", a.auth(authz.ManageTenant, a.auditEvents))
	mux.HandleFunc("PUT /api/v1/tenant/quota", a.auth(authz.ManageTenant, a.setQuota))
	mux.HandleFunc("POST /api/v1/principals", a.auth(authz.ManageTenant, a.createPrincipal))
	mux.HandleFunc("POST /api/v1/principals/{id}/rotate", a.auth(authz.ManageTenant, a.rotatePrincipal))
	mux.HandleFunc("DELETE /api/v1/principals/{id}", a.auth(authz.ManageTenant, a.revokePrincipal))
	return mux
}

func (a API) auditEvents(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	var before time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, 400, "invalid_cursor", "before must be RFC3339")
			return
		}
		before = parsed
	}
	rows, err := a.Store.AuditEventsForTenant(r.Context(), principal.TenantID, before, limit)
	if err != nil {
		writeError(w, 500, "internal", "audit lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, map[string]any{"id": row.ID, "actor": row.Actor, "action": row.Action, "resource_type": row.ResourceType, "resource_name": row.ResourceName, "outcome": row.Outcome, "request_id": row.RequestID, "payload": json.RawMessage(row.Payload), "created_at": row.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func (a API) setQuota(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	var request struct {
		MaxDeployments       int `json:"max_deployments"`
		MaxReplicas          int `json:"max_replicas"`
		MaxRequestsPerMinute int `json:"max_requests_per_minute"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if err := a.Store.SetTenantQuota(r.Context(), principal.TenantID, request.MaxDeployments, request.MaxReplicas, request.MaxRequestsPerMinute); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "quota.update", ResourceType: "tenant", ResourceName: principal.TenantID, Outcome: "succeeded"})
	w.WriteHeader(http.StatusNoContent)
}
func (a API) createPrincipal(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	var request struct {
		Name string     `json:"name"`
		Role authz.Role `json:"role"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	principal, token, err := a.Store.CreatePrincipal(r.Context(), actor.TenantID, request.Name, request.Role)
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, 409, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "principal.create", ResourceType: "principal", ResourceName: principal.ID, Outcome: "succeeded"})
	writeJSON(w, 201, map[string]any{"principal": map[string]any{"id": principal.ID, "name": principal.Name, "role": principal.Role, "tenant_id": principal.TenantID}, "credential": token})
}
func (a API) rotatePrincipal(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	token, err := a.Store.RotatePrincipalForTenant(r.Context(), actor.TenantID, r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "principal was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "credential rotation failed")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "principal.rotate", ResourceType: "principal", ResourceName: r.PathValue("id"), Outcome: "succeeded"})
	writeJSON(w, 200, map[string]string{"credential": token})
}
func (a API) revokePrincipal(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if err := a.Store.RevokePrincipalForTenant(r.Context(), actor.TenantID, r.PathValue("id")); errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "principal was not found")
		return
	} else if err != nil {
		writeError(w, 500, "internal", "principal revocation failed")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "principal.revoke", ResourceType: "principal", ResourceName: r.PathValue("id"), Outcome: "succeeded"})
	w.WriteHeader(http.StatusNoContent)
}

func (a API) deployments(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := a.Store.DeploymentsForTenant(r.Context(), principal.TenantID)
	if err != nil {
		writeError(w, 500, "internal", "deployment lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, deploymentResponse(row))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) targets(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := a.Store.TargetsForTenant(r.Context(), principal.TenantID)
	if err != nil {
		writeError(w, 500, "internal", "target lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, targetResponse(row))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) orphans(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := a.Store.OrphanedTargetsForTenant(r.Context(), principal.TenantID)
	if err != nil {
		writeError(w, 500, "internal", "orphan lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, map[string]any{"target_id": row.TargetID, "name": row.Name, "provider": row.Provider, "provider_resource_id": row.ProviderResourceID, "created_at": row.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) addTarget(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	var request struct {
		Name          string `json:"name"`
		URL           string `json:"url"`
		Runtime       string `json:"runtime"`
		UpstreamModel string `json:"upstream_model,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if request.Runtime == "" {
		request.Runtime = "vllm"
	}
	parsed, err := url.Parse(request.URL)
	if request.Name == "" || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		writeError(w, 422, "validation_failed", "name and an absolute HTTP(S) URL without credentials or a fragment are required")
		return
	}
	target, err := a.Store.AddTargetForTenant(r.Context(), principal.TenantID, domain.Target{Name: request.Name, URL: request.URL, Provider: "existing", Runtime: request.Runtime, UpstreamModel: request.UpstreamModel})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, 409, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "target could not be registered")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "target.create", ResourceType: "target", ResourceName: request.Name, Outcome: "succeeded"})
	writeJSON(w, 201, map[string]any{"target": targetResponse(target)})
}

func deploymentResponse(row domain.Deployment) map[string]any {
	return map[string]any{"id": row.ID, "tenant_id": row.TenantID, "name": row.Name, "model": row.Model, "runtime": row.Runtime, "routing_strategy": row.RoutingStrategy, "desired_state": row.DesiredState, "observed_state": row.ObservedState, "min_replicas": row.MinReplicas, "max_replicas": row.MaxReplicas, "autoscaling_enabled": row.AutoscalingEnabled, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}
func targetResponse(row domain.Target) map[string]any {
	return map[string]any{"id": row.ID, "name": row.Name, "url": row.URL, "provider": row.Provider, "runtime": row.Runtime, "upstream_model": row.UpstreamModel, "health": row.Health, "provider_resource_id": row.ProviderResourceID, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}

func (a API) applyDeployment(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must not exceed 128 characters")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request workflows.ApplyExistingRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid: "+err.Error())
		return
	}
	if err := request.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	request.Actor = principal.Name
	request.TenantID = principal.TenantID
	encoded, _ := json.Marshal(request)
	operation, created, err := a.Store.EnqueueOperation(r.Context(), domain.Operation{TenantID: principal.TenantID, Kind: workflows.ApplyExistingKind, ResourceType: "deployment", ResourceName: request.Name, IdempotencyKey: key, RequestJSON: string(encoded)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "operation could not be queued")
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	status := http.StatusAccepted
	if !created && operation.Status == "succeeded" {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"operation": operationResponse(operation), "created": created})
}
func (a API) auth(action authz.Action, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token := strings.TrimPrefix(header, "Bearer ")
		var principal domain.Principal
		expected := "Bearer " + a.APIKey
		if a.APIKey != "" && subtle.ConstantTimeCompare([]byte(header), []byte(expected)) == 1 {
			principal = domain.Principal{ID: "bootstrap", TenantID: "global", Name: "bootstrap", Role: string(authz.Admin)}
		} else if a.Authenticator != nil && token != "" {
			resolved, err := a.Authenticator.AuthenticatePrincipal(r.Context(), token)
			if err == nil {
				principal = resolved
			}
		}
		if principal.ID == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "valid bearer authentication is required")
			return
		}
		if !authz.Allowed(authz.Role(principal.Role), action) {
			writeError(w, http.StatusForbidden, "forbidden", "principal is not allowed to perform this action")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, principal)))
	}
}
func (a API) operation(w http.ResponseWriter, r *http.Request) {
	op, err := a.Store.Operation(r.Context(), r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "operation was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "operation lookup failed")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	if op.TenantID != principal.TenantID {
		writeError(w, http.StatusNotFound, "not_found", "operation was not found")
		return
	}
	writeJSON(w, http.StatusOK, operationResponse(op))
}
func (a API) cancel(w http.ResponseWriter, r *http.Request) {
	op, lookupErr := a.Store.Operation(r.Context(), r.PathValue("id"))
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	if lookupErr != nil || op.TenantID != principal.TenantID {
		writeError(w, http.StatusNotFound, "not_found", "operation was not found")
		return
	}
	err := a.Store.RequestOperationCancel(r.Context(), r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusConflict, "not_cancellable", "operation is missing or already terminal")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cancellation request failed")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "operation.cancel", ResourceType: "operation", ResourceName: r.PathValue("id"), Outcome: "accepted"})
	writeJSON(w, http.StatusAccepted, map[string]string{"operation_id": r.PathValue("id"), "status": "cancellation_requested"})
}
func operationResponse(op domain.Operation) map[string]any {
	return map[string]any{"id": op.ID, "tenant_id": op.TenantID, "kind": op.Kind, "resource_type": op.ResourceType, "resource_name": op.ResourceName, "status": op.Status, "progress": op.Progress, "message": op.Message, "attempt": op.Attempt, "max_attempts": op.MaxAttempts, "retryable": op.Retryable, "cancel_requested": op.CancelRequested, "created_at": op.CreatedAt, "updated_at": op.UpdatedAt, "completed_at": op.CompletedAt}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
