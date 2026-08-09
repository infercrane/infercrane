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
	SubmitCloudDeployment(context.Context, domain.Deployment, domain.Operation) (domain.Deployment, domain.Operation, bool, error)
	SubmitDeploymentDelete(context.Context, string, string, string, domain.Operation) (domain.Operation, bool, error)
	ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error)
	EventsForTenant(context.Context, string, string) ([]domain.Event, error)
	RequestStats(context.Context, string, time.Duration) (domain.RequestStats, error)
	ColdStartStats(context.Context, string, time.Duration) (domain.ColdStartStats, error)
	ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error)
	Revisions(context.Context, string, string) ([]domain.DeploymentRevision, error)
	OperationEvents(context.Context, string, int) ([]domain.OperationEvent, error)
	ScalingDecisionsForTenant(context.Context, string, string, int) ([]domain.ScalingDecision, error)
	ModelArtifactForRevision(context.Context, string, string) (domain.ModelArtifact, error)
	ReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.ReleaseGuardEvaluation, error)
	ReleaseGuardPolicy(context.Context, string, string) (domain.ReleaseGuardPolicy, error)
	SetReleaseGuardPolicy(context.Context, string, string, domain.ReleaseGuardPolicy) (domain.ReleaseGuardPolicy, error)
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
	CreateTenant(context.Context, string, string) error
	SetRouteForTenant(context.Context, string, string, string) error
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
	mux.HandleFunc("GET /api/v1/operations/{id}/events", a.auth(authz.Read, a.operationEvents))
	mux.HandleFunc("POST /api/v1/operations/{id}/cancel", a.auth(authz.Deploy, a.cancel))
	mux.HandleFunc("POST /api/v1/deployments/apply", a.auth(authz.Deploy, a.applyDeployment))
	mux.HandleFunc("POST /api/v1/deployments", a.auth(authz.Deploy, a.createCloudDeployment))
	mux.HandleFunc("DELETE /api/v1/deployments/{name}", a.auth(authz.Delete, a.deleteDeployment))
	mux.HandleFunc("GET /api/v1/deployments", a.auth(authz.Read, a.deployments))
	mux.HandleFunc("GET /api/v1/deployments/{name}", a.auth(authz.Read, a.deployment))
	mux.HandleFunc("GET /api/v1/deployments/{name}/events", a.auth(authz.Read, a.deploymentEvents))
	mux.HandleFunc("GET /api/v1/deployments/{name}/revisions", a.auth(authz.Read, a.revisions))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollouts", a.auth(authz.Deploy, a.createRollout))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollouts/guard/evaluate", a.auth(authz.Deploy, a.evaluateReleaseGuard))
	mux.HandleFunc("GET /api/v1/deployments/{name}/release-guard/policy", a.auth(authz.Read, a.releaseGuardPolicy))
	mux.HandleFunc("PUT /api/v1/deployments/{name}/release-guard/policy", a.auth(authz.Deploy, a.setReleaseGuardPolicy))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollouts/{revision}/promote", a.auth(authz.Deploy, a.promoteRollout))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollouts/{revision}/provision", a.auth(authz.Deploy, a.provisionRollout))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollouts/{revision}/reject", a.auth(authz.Deploy, a.rejectRollout))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollback", a.auth(authz.Deploy, a.rollbackDeployment))
	mux.HandleFunc("GET /api/v1/deployments/{name}/scaling-decisions", a.auth(authz.Read, a.scalingDecisions))
	mux.HandleFunc("PUT /api/v1/deployments/{name}/route", a.auth(authz.Deploy, a.setRoute))
	mux.HandleFunc("GET /api/v1/targets", a.auth(authz.Read, a.targets))
	mux.HandleFunc("POST /api/v1/targets", a.auth(authz.Deploy, a.addTarget))
	mux.HandleFunc("GET /api/v1/orphans", a.auth(authz.Read, a.orphans))
	mux.HandleFunc("GET /api/v1/audit-events", a.auth(authz.ManageTenant, a.auditEvents))
	mux.HandleFunc("PUT /api/v1/tenant/quota", a.auth(authz.ManageTenant, a.setQuota))
	mux.HandleFunc("POST /api/v1/tenants", a.auth(authz.ManageTenant, a.createTenant))
	mux.HandleFunc("POST /api/v1/principals", a.auth(authz.ManageTenant, a.createPrincipal))
	mux.HandleFunc("POST /api/v1/principals/{id}/rotate", a.auth(authz.ManageTenant, a.rotatePrincipal))
	mux.HandleFunc("DELETE /api/v1/principals/{id}", a.auth(authz.ManageTenant, a.revokePrincipal))
	return mux
}

func (a API) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must not exceed 128 characters")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	resolved, err := a.Store.ResolveForTenant(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "deployment lookup failed")
		return
	}
	request := workflows.DeleteRequest{DeploymentID: resolved.Deployment.ID, Name: resolved.Deployment.Name, Actor: principal.Name, TenantID: principal.TenantID}
	encoded, _ := json.Marshal(request)
	deleteKind := workflows.DeleteKind
	for _, target := range resolved.Targets {
		if target.Provider == "runpod-serverless" {
			deleteKind = workflows.ServerlessDeleteKind
			break
		}
	}
	operation, created, err := a.Store.SubmitDeploymentDelete(r.Context(), principal.TenantID, resolved.Deployment.Name, resolved.Deployment.ID, domain.Operation{Kind: deleteKind, IdempotencyKey: key, RequestJSON: string(encoded)})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "deletion could not be submitted")
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": operationResponse(operation), "created": created})
}

func (a API) createCloudDeployment(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must not exceed 128 characters")
		return
	}
	var request workflows.CloudRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
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
	request.Actor, request.TenantID = principal.Name, principal.TenantID
	if request.ComputeMode == "" {
		request.ComputeMode = "elastic"
	}
	minReplicas, maxReplicas := 1, 1
	operationKind := workflows.ConvergeKind
	if request.ComputeMode == "serverless" {
		minReplicas, operationKind = 0, workflows.ServerlessConvergeKind
	} else if request.MinReplicas > 0 {
		minReplicas = request.MinReplicas
	}
	if request.MaxReplicas > 0 {
		maxReplicas = request.MaxReplicas
	} else {
		maxReplicas = minReplicas
	}
	encoded, _ := json.Marshal(request)
	autoscalingEnabled := request.ComputeMode != "serverless" && maxReplicas > minReplicas
	deployment, operation, created, err := a.Store.SubmitCloudDeployment(r.Context(), domain.Deployment{TenantID: principal.TenantID, Name: request.Name, Model: request.Model, MinReplicas: minReplicas, MaxReplicas: maxReplicas, AutoscalingEnabled: autoscalingEnabled}, domain.Operation{TenantID: principal.TenantID, Kind: operationKind, IdempotencyKey: key, RequestJSON: string(encoded)})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cloud deployment could not be submitted")
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	status := http.StatusAccepted
	if !created && operation.Status == "succeeded" {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"deployment": deploymentResponse(deployment), "operation": operationResponse(operation), "created": created})
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
func (a API) createTenant(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if actor.ID != "bootstrap" {
		writeError(w, http.StatusForbidden, "forbidden", "only the bootstrap administrator can create tenants")
		return
	}
	var request struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.ID == "" {
		writeError(w, 400, "invalid_request", "tenant id is required")
		return
	}
	if request.Name == "" {
		request.Name = request.ID
	}
	if err := a.Store.CreateTenant(r.Context(), request.ID, request.Name); errors.Is(err, domain.ErrConflict) {
		writeError(w, 409, "conflict", err.Error())
		return
	} else if err != nil {
		writeError(w, 500, "internal", "tenant could not be created")
		return
	}
	writeJSON(w, 201, map[string]any{"tenant": request})
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
func (a API) deployment(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	resolved, err := a.Store.ResolveForTenant(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "deployment lookup failed")
		return
	}
	replicas, err := a.Store.ReplicasForDeployment(r.Context(), principal.TenantID, resolved.Deployment.ID)
	if err != nil {
		writeError(w, 500, "internal", "replica lookup failed")
		return
	}
	revisions, err := a.Store.Revisions(r.Context(), principal.TenantID, resolved.Deployment.Name)
	if err != nil {
		writeError(w, 500, "internal", "revision lookup failed")
		return
	}
	stats, err := a.Store.RequestStats(r.Context(), resolved.Deployment.ID, 5*time.Minute)
	if err != nil {
		writeError(w, 500, "internal", "request statistics lookup failed")
		return
	}
	coldStarts, err := a.Store.ColdStartStats(r.Context(), resolved.Deployment.ID, 24*time.Hour)
	if err != nil {
		writeError(w, 500, "internal", "cold-start statistics lookup failed")
		return
	}
	guardEvaluations, err := a.Store.ReleaseGuardEvaluations(r.Context(), principal.TenantID, resolved.Deployment.Name, 20)
	if err != nil {
		writeError(w, 500, "internal", "Release Guard evaluation lookup failed")
		return
	}
	guardPolicy, err := a.Store.ReleaseGuardPolicy(r.Context(), principal.TenantID, resolved.Deployment.Name)
	if err != nil {
		writeError(w, 500, "internal", "Release Guard policy lookup failed")
		return
	}
	targets := make([]map[string]any, 0, len(resolved.Targets))
	for _, target := range resolved.Targets {
		targets = append(targets, targetResponse(target))
	}
	replicaData := make([]map[string]any, 0, len(replicas))
	for _, replica := range replicas {
		replicaData = append(replicaData, replicaResponse(replica))
	}
	revisionData := make([]map[string]any, 0, len(revisions))
	artifactData := make([]map[string]any, 0, len(revisions))
	for _, revision := range revisions {
		revisionData = append(revisionData, revisionResponse(revision))
		modelArtifact, artifactErr := a.Store.ModelArtifactForRevision(r.Context(), principal.TenantID, revision.ID)
		if artifactErr == nil {
			artifactData = append(artifactData, artifactResponse(revision.ID, modelArtifact))
		} else if !errors.Is(artifactErr, domain.ErrNotFound) {
			writeError(w, 500, "internal", "model artifact lookup failed")
			return
		}
	}
	guardData := make([]map[string]any, 0, len(guardEvaluations))
	for _, evaluation := range guardEvaluations {
		guardData = append(guardData, releaseGuardResponse(evaluation))
	}
	writeJSON(w, 200, map[string]any{"deployment": deploymentResponse(resolved.Deployment), "targets": targets, "replicas": replicaData, "revisions": revisionData, "model_artifacts": artifactData, "request_stats": stats, "cold_start_stats": coldStarts, "release_guard_policy": guardPolicy, "release_guard_evaluations": guardData})
}
func (a API) deploymentEvents(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := a.Store.EventsForTenant(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "deployment event lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, event := range rows {
		data = append(data, map[string]any{"id": event.ID, "type": event.Type, "summary": event.Summary, "payload": json.RawMessage(event.Payload), "target_id": event.TargetID, "created_at": event.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) revisions(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := a.Store.Revisions(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	} else if err != nil {
		writeError(w, 500, "internal", "revision lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, revision := range rows {
		data = append(data, revisionResponse(revision))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func (a API) createRollout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Spec json.RawMessage `json:"spec"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	a.enqueueRollout(w, r, workflows.RolloutCreateKind, workflows.RolloutRequest{Name: r.PathValue("name"), Spec: body.Spec})
}

func (a API) evaluateReleaseGuard(w http.ResponseWriter, r *http.Request) {
	a.enqueueRollout(w, r, workflows.ReleaseGuardEvaluateKind, workflows.RolloutRequest{Name: r.PathValue("name")})
}

func (a API) releaseGuardPolicy(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	policy, err := a.Store.ReleaseGuardPolicy(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Release Guard policy lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (a API) setReleaseGuardPolicy(w http.ResponseWriter, r *http.Request) {
	var policy domain.ReleaseGuardPolicy
	if !decodeMutationBody(w, r, &policy) {
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	updated, err := a.Store.SetReleaseGuardPolicy(r.Context(), principal.TenantID, r.PathValue("name"), policy)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "release_guard.policy.update", ResourceType: "deployment", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, updated)
}

func (a API) promoteRollout(w http.ResponseWriter, r *http.Request) {
	a.enqueueRollout(w, r, workflows.RolloutPromoteKind, workflows.RolloutRequest{Name: r.PathValue("name"), CandidateID: r.PathValue("revision")})
}

func (a API) provisionRollout(w http.ResponseWriter, r *http.Request) {
	a.enqueueRollout(w, r, workflows.RolloutProvisionKind, workflows.RolloutRequest{Name: r.PathValue("name"), CandidateID: r.PathValue("revision")})
}

func (a API) rejectRollout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	a.enqueueRollout(w, r, workflows.RolloutRejectKind, workflows.RolloutRequest{Name: r.PathValue("name"), CandidateID: r.PathValue("revision"), Reason: body.Reason})
}

func (a API) rollbackDeployment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RevisionID string `json:"revision_id"`
		Reason     string `json:"reason"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	a.enqueueRollout(w, r, workflows.RolloutRollbackKind, workflows.RolloutRequest{Name: r.PathValue("name"), RevisionID: body.RevisionID, Reason: body.Reason})
}

func decodeMutationBody(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return false
	}
	return true
}

func (a API) enqueueRollout(w http.ResponseWriter, r *http.Request, kind string, request workflows.RolloutRequest) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must not exceed 128 characters")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	request.Actor, request.TenantID = principal.Name, principal.TenantID
	encoded, _ := json.Marshal(request)
	maxAttempts := 5
	if kind == workflows.RolloutProvisionKind || kind == workflows.RolloutPromoteKind || kind == workflows.RolloutRejectKind {
		maxAttempts = 120
	}
	operation, created, err := a.Store.EnqueueOperation(r.Context(), domain.Operation{TenantID: principal.TenantID, Kind: kind, ResourceType: "deployment", ResourceName: request.Name, IdempotencyKey: key, RequestJSON: string(encoded), MaxAttempts: maxAttempts})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "rollout operation could not be queued")
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	status := http.StatusAccepted
	if !created && operation.Status == "succeeded" {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"operation": operationResponse(operation), "created": created})
}
func (a API) scalingDecisions(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.Store.ScalingDecisionsForTenant(r.Context(), principal.TenantID, r.PathValue("name"), limit)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	} else if err != nil {
		writeError(w, 500, "internal", "scaling decision lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, map[string]any{"id": row.ID, "action": row.Action, "old_replicas": row.OldReplicas, "new_replicas": row.NewReplicas, "reason": row.Reason, "signals": json.RawMessage(row.SignalsJSON), "created_at": row.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) setRoute(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	var request struct {
		Strategy string `json:"strategy"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Strategy == "" {
		writeError(w, 400, "invalid_request", "routing strategy is required")
		return
	}
	if err := a.Store.SetRouteForTenant(r.Context(), principal.TenantID, r.PathValue("name"), request.Strategy); errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	} else if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"deployment": r.PathValue("name"), "routing_strategy": request.Strategy})
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
	return map[string]any{"id": row.ID, "tenant_id": row.TenantID, "name": row.Name, "model": row.Model, "runtime": row.Runtime, "routing_strategy": row.RoutingStrategy, "desired_state": row.DesiredState, "observed_state": row.ObservedState, "min_replicas": row.MinReplicas, "max_replicas": row.MaxReplicas, "autoscaling_enabled": row.AutoscalingEnabled, "active_revision_id": row.ActiveRevisionID, "candidate_revision_id": row.CandidateRevisionID, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}
func targetResponse(row domain.Target) map[string]any {
	return map[string]any{"id": row.ID, "name": row.Name, "url": row.URL, "provider": row.Provider, "runtime": row.Runtime, "upstream_model": row.UpstreamModel, "health": row.Health, "provider_resource_id": row.ProviderResourceID, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}
func replicaResponse(row domain.Replica) map[string]any {
	return map[string]any{"id": row.ID, "revision_id": row.RevisionID, "ordinal": row.Ordinal, "external_key": row.ExternalKey, "lifecycle_state": row.LifecycleState, "provider": row.Provider, "provider_request_id": row.ProviderRequestID, "provider_resource_id": row.ProviderResourceID, "endpoint": row.Endpoint, "health": row.Health, "provider_details": json.RawMessage(row.ProviderDetails), "last_observed_at": row.LastObservedAt, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}
func revisionResponse(row domain.DeploymentRevision) map[string]any {
	return map[string]any{"id": row.ID, "number": row.Number, "status": row.Status, "spec": json.RawMessage(row.SpecJSON), "source_revision_id": row.SourceRevisionID, "reason": row.Reason, "created_at": row.CreatedAt, "activated_at": row.ActivatedAt, "completed_at": row.CompletedAt}
}
func artifactResponse(revisionID string, row domain.ModelArtifact) map[string]any {
	return map[string]any{"id": row.ID, "revision_id": revisionID, "source": row.Source, "repository": row.Repository, "requested_revision": row.RequestedRevision, "immutable_revision": row.ImmutableRevision, "model_identity": row.ModelIdentity, "approximate_size_bytes": row.ApproximateSizeBytes, "cache_state": row.CacheState, "runtime_compatibility": json.RawMessage(row.RuntimeCompatibilityJSON), "resolved_at": row.ResolvedAt}
}

func releaseGuardResponse(row domain.ReleaseGuardEvaluation) map[string]any {
	return map[string]any{"id": row.ID, "active_revision_id": row.ActiveRevisionID, "candidate_revision_id": row.CandidateRevisionID, "decision": row.Decision, "reasons": json.RawMessage(row.ReasonCodesJSON), "metrics": json.RawMessage(row.MetricsJSON), "policy": json.RawMessage(row.PolicyJSON), "created_at": row.CreatedAt}
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
func (a API) operationEvents(w http.ResponseWriter, r *http.Request) {
	op, err := a.Store.Operation(r.Context(), r.PathValue("id"))
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	if err != nil || op.TenantID != principal.TenantID {
		writeError(w, http.StatusNotFound, "not_found", "operation was not found")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.Store.OperationEvents(r.Context(), op.ID, limit)
	if err != nil {
		writeError(w, 500, "internal", "operation event lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, event := range rows {
		data = append(data, map[string]any{"sequence": event.Sequence, "level": event.Level, "type": event.Type, "message": event.Message, "payload": json.RawMessage(event.Payload), "created_at": event.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
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
