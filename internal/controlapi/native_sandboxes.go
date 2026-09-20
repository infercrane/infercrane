package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/sandboxprovider"
)

type sandboxProductStore interface {
	CreateNativeSandbox(context.Context, domain.NativeSandbox) (domain.NativeSandbox, bool, error)
	NativeSandbox(context.Context, string, string) (domain.NativeSandbox, error)
	NativeSandboxes(context.Context, string, bool) ([]domain.NativeSandbox, error)
	SetNativeSandboxProviderRefs(context.Context, string, string, string, string, string, string) (domain.NativeSandbox, error)
	SetNativeSandboxStatus(context.Context, string, string, string, string) (domain.NativeSandbox, error)
	AppendSandboxUsageEvent(context.Context, domain.SandboxUsageEvent) (bool, error)
	SandboxUsageSummary(context.Context, string) (domain.SandboxUsageSummary, error)
}

type createNativeSandboxRequest struct {
	DisplayName         string `json:"display_name"`
	Purpose             string `json:"purpose"`
	SourceType          string `json:"source_type"`
	SourceReference     string `json:"source_reference,omitempty"`
	TemplateID          string `json:"template_id"`
	ModelEndpoint       string `json:"model_endpoint,omitempty"`
	TTLSeconds          int64  `json:"ttl_seconds"`
	StandbyAfterSeconds int64  `json:"standby_after_seconds,omitempty"`
	AutoResume          bool   `json:"auto_resume,omitempty"`
	NetworkMode         string `json:"network_mode,omitempty"`
}

func (a API) sandboxStore(w http.ResponseWriter) (sandboxProductStore, bool) {
	value, ok := a.Store.(sandboxProductStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "sandbox_storage_unavailable", "native sandbox storage is not configured")
	}
	return value, ok
}

func (a API) sandboxCapabilities(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if a.SandboxProvider == nil {
		writeJSON(w, http.StatusOK, sandboxprovider.Capabilities{
			Provider: "brezel", Product: "InferCrane Sandboxes", State: "not_configured",
			Assurance: "private-tenant-preview", Templates: []sandboxprovider.Template{},
			Qualification:     "unavailable",
			QualificationNote: "A private sandbox endpoint and approved environments are not configured.",
		})
		return
	}
	capabilities, err := a.SandboxProvider.Capabilities(r.Context(), actor.TenantID)
	if err != nil {
		a.writeSandboxProviderError(w, err)
		return
	}
	// Brezel is an implementation detail. Keep the stable product identity in
	// the customer contract while retaining evidence about supported features.
	capabilities.Provider = "infercrane"
	writeJSON(w, http.StatusOK, capabilities)
}

func (a API) sandboxes(w http.ResponseWriter, r *http.Request) {
	store, ok := a.sandboxStore(w)
	if !ok {
		return
	}
	includeTerminal := false
	if raw := strings.TrimSpace(r.URL.Query().Get("include_terminal")); raw != "" {
		var err error
		includeTerminal, err = strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "include_terminal must be true or false")
			return
		}
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := store.NativeSandboxes(r.Context(), actor.TenantID, includeTerminal)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "sandboxes could not be read")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, nativeSandboxResponse(row, nil))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "product": "private_computers"})
}

func (a API) sandbox(w http.ResponseWriter, r *http.Request) {
	store, row, ok := a.ownedSandbox(w, r)
	if !ok {
		return
	}
	var observed *sandboxprovider.Sandbox
	if a.SandboxProvider != nil && row.BrezelSandboxID != "" && row.DeletedAt == nil {
		value, err := a.SandboxProvider.Get(r.Context(), row.TenantID, row.BrezelSandboxID)
		if err == nil {
			observed = &value
			status := customerSandboxStatus(value.State)
			failureCode := providerFailureCode(value.Failure)
			if status != row.Status || failureCode != row.FailureCode {
				row, _ = store.SetNativeSandboxStatus(r.Context(), row.TenantID, row.ID, status, failureCode)
			}
		} else {
			// A failed provider observation is uncertainty, never evidence that a
			// previously running computer is still running.
			row, _ = store.SetNativeSandboxStatus(r.Context(), row.TenantID, row.ID, "unknown", "provider_observation_failed")
		}
	}
	writeJSON(w, http.StatusOK, nativeSandboxResponse(row, observed))
}

func (a API) createSandbox(w http.ResponseWriter, r *http.Request) {
	if a.SandboxProvider == nil {
		writeError(w, http.StatusNotImplemented, "sandbox_provider_unavailable", "native private computers are not configured")
		return
	}
	store, ok := a.sandboxStore(w)
	if !ok {
		return
	}
	var request createNativeSandboxRequest
	if !decodeMutationBody(w, r, &request) {
		return
	}
	normalizeNativeSandboxRequest(&request, a.SandboxDefaultTemplate)
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 96 {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_request", "Idempotency-Key is required and must not exceed 96 characters")
		return
	}
	if request.NetworkMode != "offline" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_sandbox_request", "only the offline network policy is available")
		return
	}
	digest, err := sandboxInputDigest(request)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "sandbox request could not be normalized")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	connectorRevision := ""
	if request.ModelEndpoint != "" {
		endpoints, available := a.endpointResources()
		if !available {
			writeError(w, http.StatusNotImplemented, "model_connector_unavailable", "model endpoint access is not configured")
			return
		}
		if _, lookupErr := endpoints.ResolveEndpointForTenant(r.Context(), actor.TenantID, request.ModelEndpoint); lookupErr != nil {
			writeError(w, http.StatusUnprocessableEntity, "model_endpoint_not_found", "the selected InferCrane endpoint was not found")
			return
		}
		connectorRevision = a.SandboxModelConnectors[request.ModelEndpoint]
		if connectorRevision == "" {
			writeError(w, http.StatusUnprocessableEntity, "model_connector_unavailable", "the selected endpoint does not have an approved sandbox connector")
			return
		}
	}
	row, created, err := store.CreateNativeSandbox(r.Context(), domain.NativeSandbox{
		TenantID: actor.TenantID, CreatedBy: actor.Name, DisplayName: request.DisplayName,
		Purpose: request.Purpose, SourceType: request.SourceType, SourceReference: request.SourceReference,
		TemplateID: request.TemplateID, ModelEndpoint: request.ModelEndpoint, BrezelProjectID: a.SandboxProjectID,
		Status: "creating_workspace", IdempotencyKey: key, InputDigest: digest,
	})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "sandbox_conflict", "the idempotency key was already used for a different computer")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_sandbox_request", err.Error())
		return
	}
	if !created && row.BrezelSandboxID != "" {
		writeJSON(w, http.StatusAccepted, nativeSandboxResponse(row, nil))
		return
	}
	if row.BrezelWorkspaceID == "" {
		workspace, workspaceErr := a.SandboxProvider.CreateWorkspace(r.Context(), actor.TenantID, key+".workspace", row.ID)
		if workspaceErr != nil || workspace.Resource.ID == "" {
			_, _ = store.SetNativeSandboxStatus(context.WithoutCancel(r.Context()), actor.TenantID, row.ID, "failed", "workspace_create_failed")
			a.writeSandboxProviderError(w, workspaceErrOrInvalid(workspaceErr))
			return
		}
		row, err = store.SetNativeSandboxProviderRefs(r.Context(), actor.TenantID, row.ID, workspace.Resource.ID, "", "creating", "")
		if err != nil {
			status := "failed"
			if _, cleanupErr := a.SandboxProvider.DeleteWorkspace(context.WithoutCancel(r.Context()), actor.TenantID, workspace.Resource.ID, key+".orphan-workspace"); cleanupErr != nil {
				status = "cleanup_pending"
			}
			_, _ = store.SetNativeSandboxStatus(context.WithoutCancel(r.Context()), actor.TenantID, row.ID, status, "workspace_tracking_failed")
			writeError(w, http.StatusInternalServerError, "sandbox_tracking_failed", "workspace creation could not be recorded")
			return
		}
	}
	mutation, err := a.SandboxProvider.Create(r.Context(), actor.TenantID, key+".computer", sandboxprovider.CreateRequest{
		TemplateID: request.TemplateID, WorkspaceID: row.BrezelWorkspaceID, ConnectorRevision: connectorRevision, ExpiresAfterSeconds: request.TTLSeconds,
		StandbyAfterSeconds: request.StandbyAfterSeconds, AutoResume: request.AutoResume, NetworkMode: request.NetworkMode,
	})
	if err != nil || mutation.Resource.ID == "" {
		cleanupStatus := "failed"
		if _, cleanupErr := a.SandboxProvider.DeleteWorkspace(context.WithoutCancel(r.Context()), actor.TenantID, row.BrezelWorkspaceID, key+".compensate"); cleanupErr != nil {
			cleanupStatus = "cleanup_pending"
		}
		_, _ = store.SetNativeSandboxStatus(context.WithoutCancel(r.Context()), actor.TenantID, row.ID, cleanupStatus, "sandbox_create_failed")
		a.writeSandboxProviderError(w, workspaceErrOrInvalid(err))
		return
	}
	row, err = store.SetNativeSandboxProviderRefs(r.Context(), actor.TenantID, row.ID, row.BrezelWorkspaceID, mutation.Resource.ID, customerSandboxStatus(mutation.Resource.State), providerFailureCode(mutation.Resource.Failure))
	if err != nil {
		status := "failed"
		if _, cleanupErr := a.SandboxProvider.Delete(context.WithoutCancel(r.Context()), actor.TenantID, mutation.Resource.ID, key+".orphan-computer"); cleanupErr != nil && !errors.Is(cleanupErr, sandboxprovider.ErrNotFound) {
			status = "cleanup_pending"
		}
		if _, cleanupErr := a.SandboxProvider.DeleteWorkspace(context.WithoutCancel(r.Context()), actor.TenantID, row.BrezelWorkspaceID, key+".orphan-workspace"); cleanupErr != nil && !errors.Is(cleanupErr, sandboxprovider.ErrNotFound) {
			status = "cleanup_pending"
		}
		_, _ = store.SetNativeSandboxStatus(context.WithoutCancel(r.Context()), actor.TenantID, row.ID, status, "sandbox_tracking_failed")
		writeError(w, http.StatusInternalServerError, "sandbox_tracking_failed", "computer creation could not be recorded")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "sandbox.create", ResourceType: "sandbox", ResourceName: row.ID, Outcome: "accepted"})
	a.appendSandboxUsage(context.WithoutCancel(r.Context()), store, row, mutation.Operation.ID, "computer.created", domain.SandboxUsageEvent{})
	writeJSON(w, http.StatusAccepted, nativeSandboxResponse(row, &mutation.Resource))
}

func normalizeNativeSandboxRequest(request *createNativeSandboxRequest, defaultTemplate string) {
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	if request.DisplayName == "" {
		request.DisplayName = "New computer"
	}
	request.Purpose = strings.TrimSpace(request.Purpose)
	if request.Purpose == "" {
		request.Purpose = "blank_computer"
	}
	request.SourceType = strings.TrimSpace(request.SourceType)
	if request.SourceType == "" {
		request.SourceType = "empty_workspace"
	}
	request.SourceReference = strings.TrimSpace(request.SourceReference)
	request.TemplateID = strings.TrimSpace(request.TemplateID)
	if request.TemplateID == "" {
		request.TemplateID = strings.TrimSpace(defaultTemplate)
	}
	request.ModelEndpoint = strings.TrimSpace(request.ModelEndpoint)
	if request.TTLSeconds == 0 {
		request.TTLSeconds = 3600
	}
	if request.NetworkMode == "" {
		request.NetworkMode = "offline"
	}
}

func sandboxInputDigest(request createNativeSandboxRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (a API) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	a.mutateSandboxLifecycle(w, r, "pause")
}
func (a API) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	a.mutateSandboxLifecycle(w, r, "resume")
}
func (a API) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	a.mutateSandboxLifecycle(w, r, "delete")
}

func (a API) mutateSandboxLifecycle(w http.ResponseWriter, r *http.Request, action string) {
	if a.SandboxProvider == nil {
		writeError(w, http.StatusNotImplemented, "sandbox_provider_unavailable", "native private computers are not configured")
		return
	}
	store, row, ok := a.ownedSandbox(w, r)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 112 {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_request", "Idempotency-Key is required and must not exceed 112 characters")
		return
	}
	if row.DeletedAt != nil || row.Status == "deleted" {
		if action == "delete" {
			writeJSON(w, http.StatusAccepted, nativeSandboxResponse(row, nil))
			return
		}
		writeError(w, http.StatusConflict, "sandbox_deleted", "deleted computers cannot be changed")
		return
	}
	if row.BrezelSandboxID == "" {
		writeError(w, http.StatusConflict, "sandbox_incomplete", "this computer has not finished provisioning")
		return
	}
	var mutation sandboxprovider.Mutation
	var err error
	switch action {
	case "pause":
		mutation, err = a.SandboxProvider.Pause(r.Context(), actor.TenantID, row.BrezelSandboxID, key)
	case "resume":
		mutation, err = a.SandboxProvider.Resume(r.Context(), actor.TenantID, row.BrezelSandboxID, key)
	case "delete":
		mutation, err = a.SandboxProvider.Delete(r.Context(), actor.TenantID, row.BrezelSandboxID, key)
	default:
		err = sandboxprovider.ErrInvalid
	}
	if err != nil && !(action == "delete" && errors.Is(err, sandboxprovider.ErrNotFound)) {
		a.writeSandboxProviderError(w, err)
		return
	}
	status := customerSandboxStatus(mutation.Resource.State)
	if action == "delete" && errors.Is(err, sandboxprovider.ErrNotFound) {
		// A missing provider resource is the terminal success condition for a
		// retryable delete. The InferCrane record still drives workspace cleanup
		// and remains auditable under the stable customer identity.
		status = "deleted"
		mutation.Operation.ID = key + ".provider-already-absent"
	}
	if action == "delete" && status == "deleted" && row.BrezelWorkspaceID != "" {
		if _, cleanupErr := a.SandboxProvider.DeleteWorkspace(r.Context(), actor.TenantID, row.BrezelWorkspaceID, key+".workspace"); cleanupErr != nil && !errors.Is(cleanupErr, sandboxprovider.ErrNotFound) {
			status = "cleanup_pending"
		}
	}
	usage := sandboxLifecycleUsage(row, time.Now().UTC())
	row, err = store.SetNativeSandboxStatus(r.Context(), actor.TenantID, row.ID, status, providerFailureCode(mutation.Resource.Failure))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sandbox_tracking_failed", "computer state could not be recorded")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "sandbox." + action, ResourceType: "sandbox", ResourceName: row.ID, Outcome: "accepted"})
	a.appendSandboxUsage(context.WithoutCancel(r.Context()), store, row, mutation.Operation.ID, "computer."+action, usage)
	writeJSON(w, http.StatusAccepted, nativeSandboxResponse(row, &mutation.Resource))
}

func (a API) ownedSandbox(w http.ResponseWriter, r *http.Request) (sandboxProductStore, domain.NativeSandbox, bool) {
	store, ok := a.sandboxStore(w)
	if !ok {
		return nil, domain.NativeSandbox{}, false
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	row, err := store.NativeSandbox(r.Context(), actor.TenantID, r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "sandbox_not_found", "computer was not found")
		return nil, row, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "computer could not be read")
		return nil, row, false
	}
	return store, row, true
}

func nativeSandboxResponse(row domain.NativeSandbox, observed *sandboxprovider.Sandbox) map[string]any {
	age := int64(0)
	if !row.CreatedAt.IsZero() {
		age = int64(time.Since(row.CreatedAt).Seconds())
		if age < 0 {
			age = 0
		}
	}
	result := map[string]any{
		"id": row.ID, "display_name": row.DisplayName, "purpose": row.Purpose,
		"source_type": row.SourceType, "source_reference": row.SourceReference,
		"template_id": row.TemplateID, "model_endpoint": row.ModelEndpoint,
		"status": row.Status, "failure_code": row.FailureCode,
		"preview_available": row.Status == "running", "runtime_age_seconds": age,
		"created_at": row.CreatedAt, "updated_at": row.UpdatedAt, "last_active_at": row.LastActiveAt,
	}
	if observed != nil {
		result["expires_at"] = observed.ExpiresAt
		result["lifecycle"] = observed.Lifecycle
		result["network_policy"] = "offline"
		result["evidence"] = map[string]any{"environment_revision": observed.EnvironmentRevision, "assurance": "private-computer"}
	}
	return result
}

func customerSandboxStatus(state string) string {
	switch state {
	case "creating", "running", "standby", "pausing", "resuming", "deleting", "deleted", "expired", "failed":
		return state
	default:
		return "unknown"
	}
}

func providerFailureCode(failure *sandboxprovider.Failure) string {
	if failure == nil {
		return ""
	}
	return failure.Code
}

func workspaceErrOrInvalid(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: provider returned no resource identity", sandboxprovider.ErrUpstream)
}

func (a API) appendSandboxUsage(ctx context.Context, store sandboxProductStore, row domain.NativeSandbox, providerOperationID, eventType string, counters domain.SandboxUsageEvent) {
	digest := sha256.Sum256([]byte(row.TenantID + "\x00" + row.ID + "\x00" + providerOperationID + "\x00" + eventType))
	counters.EventID = hex.EncodeToString(digest[:])
	counters.TenantID, counters.SandboxID = row.TenantID, row.ID
	counters.ProviderOperationID, counters.EventType = providerOperationID, eventType
	counters.OccurredAt, counters.TemplateID = time.Now().UTC(), row.TemplateID
	counters.RuntimeClass, counters.MetadataVersion = "firecracker-cpu", 1
	_, _ = store.AppendSandboxUsageEvent(ctx, counters)
}

func sandboxLifecycleUsage(row domain.NativeSandbox, endedAt time.Time) domain.SandboxUsageEvent {
	elapsed := endedAt.Sub(row.LastActiveAt).Milliseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	switch row.Status {
	case "running", "pausing", "resuming":
		return domain.SandboxUsageEvent{RunningMilliseconds: elapsed}
	case "standby":
		return domain.SandboxUsageEvent{StandbyMilliseconds: elapsed}
	default:
		return domain.SandboxUsageEvent{}
	}
}

func (a API) sandboxUsage(w http.ResponseWriter, r *http.Request) {
	store, ok := a.sandboxStore(w)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	summary, err := store.SandboxUsageSummary(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "sandbox usage could not be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage": summary, "billing_status": "not_an_invoice", "content_recorded": false})
}

func (a API) writeSandboxProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sandboxprovider.ErrForbidden):
		writeError(w, http.StatusForbidden, "sandbox_tenant_not_enabled", "private computers are not enabled for this tenant")
	case errors.Is(err, sandboxprovider.ErrNotFound):
		writeError(w, http.StatusNotFound, "sandbox_not_found", "computer was not found")
	case errors.Is(err, sandboxprovider.ErrConflict):
		writeError(w, http.StatusConflict, "sandbox_conflict", err.Error())
	case errors.Is(err, sandboxprovider.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_sandbox_request", err.Error())
	case errors.Is(err, sandboxprovider.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "sandbox_provider_unavailable", "private computer infrastructure is not ready")
	default:
		writeError(w, http.StatusBadGateway, "sandbox_provider_failed", "private computer infrastructure did not confirm the request")
	}
}

func (a API) runSandboxCommand(w http.ResponseWriter, r *http.Request) {
	store, row, ok := a.actionableSandbox(w, r)
	if !ok {
		return
	}
	var request sandboxprovider.CommandRequest
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if len(request.Argv) < 1 || len(request.Argv) > 128 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_command", "a command must contain between 1 and 128 arguments")
		return
	}
	for _, value := range request.Argv {
		if value == "" || len(value) > 4096 {
			writeError(w, http.StatusUnprocessableEntity, "invalid_command", "command arguments must be non-empty and at most 4096 characters")
			return
		}
	}
	if request.Cwd == "" {
		request.Cwd = "/workspace"
	}
	if request.Cwd != "/workspace" && !strings.HasPrefix(request.Cwd, "/workspace/") {
		writeError(w, http.StatusUnprocessableEntity, "invalid_command", "the working directory must be inside /workspace")
		return
	}
	if request.TimeoutSeconds == 0 {
		request.TimeoutSeconds = 300
	}
	if request.TimeoutSeconds < 1 || request.TimeoutSeconds > 3600 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_command", "timeout_seconds must be between 1 and 3600")
		return
	}
	for name, value := range request.Env {
		if name != "INFERCRANE_API_KEY" || value != "brezel://infercrane/model-session" || row.ModelEndpoint == "" {
			writeError(w, http.StatusUnprocessableEntity, "secret_policy", "only the InferCrane model-session placeholder may be passed to a computer")
			return
		}
	}
	started := time.Now().UTC()
	requestID := sandboxEventIdentity(row, "command.request", strconv.FormatInt(started.UnixNano(), 10))
	a.appendSandboxUsage(context.WithoutCancel(r.Context()), store, row, requestID, "command.started", domain.SandboxUsageEvent{})
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	var terminal *sandboxprovider.CommandEvent
	executionID, err := a.SandboxProvider.RunCommand(r.Context(), row.TenantID, row.BrezelSandboxID, request, func(event sandboxprovider.CommandEvent) error {
		if event.Type == "exited" {
			copy := event
			terminal = &copy
		}
		// Provider execution identities stay in the usage ledger. The stream
		// exposes only an InferCrane-scoped request identity.
		event.ExecutionID = requestID
		if encodeErr := encoder.Encode(event); encodeErr != nil {
			return encodeErr
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	duration := time.Since(started).Milliseconds()
	if duration < 0 {
		duration = 0
	}
	operationID, eventType, exitClass := executionID, "command.completed", "success"
	if operationID == "" {
		operationID = requestID
	}
	if err != nil {
		eventType, exitClass = "command.unconfirmed", "unknown"
		status := "unknown"
		if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
			eventType, exitClass, status = "command.cancelled", "cancelled", "cancelled"
		}
		_ = encoder.Encode(sandboxprovider.CommandEvent{ExecutionID: requestID, Type: "error", Exited: true, Status: status, Error: "command completion was not confirmed"})
		if flusher != nil {
			flusher.Flush()
		}
	} else if terminal == nil || terminal.ExitCode == nil {
		eventType, exitClass = "command.unconfirmed", "unknown"
	} else if *terminal.ExitCode != 0 {
		exitClass = "nonzero"
	}
	a.appendSandboxUsage(context.WithoutCancel(r.Context()), store, row, operationID, eventType, domain.SandboxUsageEvent{CommandDurationMilliseconds: duration, CommandExitClass: exitClass})
	_, _ = store.SetNativeSandboxStatus(context.WithoutCancel(r.Context()), row.TenantID, row.ID, row.Status, row.FailureCode)
}

func (a API) writeSandboxFile(w http.ResponseWriter, r *http.Request) {
	store, row, ok := a.actionableSandbox(w, r)
	if !ok {
		return
	}
	filePath := strings.TrimSpace(r.URL.Query().Get("path"))
	if filePath == "" {
		writeError(w, http.StatusBadRequest, "invalid_file_path", "path is required")
		return
	}
	if r.ContentLength < 0 || r.ContentLength > 32<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_file_size", "Content-Length is required and must not exceed 32 MiB")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_file_request", "Idempotency-Key is required and must not exceed 128 characters")
		return
	}
	info, err := a.SandboxProvider.WriteFile(r.Context(), row.TenantID, row.BrezelSandboxID, filePath, io.LimitReader(r.Body, r.ContentLength), r.ContentLength)
	if err != nil {
		a.writeSandboxProviderError(w, err)
		return
	}
	operationID := sandboxEventIdentity(row, "file.write", key)
	a.appendSandboxUsage(context.WithoutCancel(r.Context()), store, row, operationID, "file.ingress", domain.SandboxUsageEvent{FileIngressBytes: info.Size})
	_, _ = store.SetNativeSandboxStatus(context.WithoutCancel(r.Context()), row.TenantID, row.ID, row.Status, row.FailureCode)
	writeJSON(w, http.StatusOK, map[string]any{"size": info.Size, "sha256": info.SHA256})
}

func (a API) readSandboxFile(w http.ResponseWriter, r *http.Request) {
	store, row, ok := a.actionableSandbox(w, r)
	if !ok {
		return
	}
	filePath := strings.TrimSpace(r.URL.Query().Get("path"))
	if filePath == "" {
		writeError(w, http.StatusBadRequest, "invalid_file_path", "path is required")
		return
	}
	download, err := a.SandboxProvider.ReadFile(r.Context(), row.TenantID, row.BrezelSandboxID, filePath)
	if err != nil {
		a.writeSandboxProviderError(w, err)
		return
	}
	defer download.Body.Close()
	if download.ContentType != "" {
		w.Header().Set("Content-Type", download.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if download.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(download.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	written, copyErr := io.Copy(w, download.Body)
	if copyErr == nil {
		operationID := sandboxEventIdentity(row, "file.read", strconv.FormatInt(time.Now().UnixNano(), 10))
		a.appendSandboxUsage(context.WithoutCancel(r.Context()), store, row, operationID, "file.egress", domain.SandboxUsageEvent{FileEgressBytes: written})
		_, _ = store.SetNativeSandboxStatus(context.WithoutCancel(r.Context()), row.TenantID, row.ID, row.Status, row.FailureCode)
	}
}

func (a API) createSandboxPortLease(w http.ResponseWriter, r *http.Request) {
	_, row, ok := a.actionableSandbox(w, r)
	if !ok {
		return
	}
	if a.SandboxPreviews == nil {
		writeError(w, http.StatusNotImplemented, "sandbox_previews_unavailable", "sandbox preview proxy is not configured")
		return
	}
	portValue, err := strconv.ParseUint(r.PathValue("port"), 10, 16)
	if err != nil || portValue == 0 {
		writeError(w, http.StatusBadRequest, "invalid_preview_port", "port must be between 1 and 65535")
		return
	}
	var request struct {
		TTLSeconds int64 `json:"ttl_seconds"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.TTLSeconds == 0 {
		request.TTLSeconds = 600
	}
	if request.TTLSeconds < 30 || request.TTLSeconds > 900 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_preview_ttl", "ttl_seconds must be between 30 and 900")
		return
	}
	lease, err := a.SandboxProvider.CreatePortLease(r.Context(), row.TenantID, row.BrezelSandboxID, uint16(portValue), request.TTLSeconds)
	if err != nil {
		a.writeSandboxProviderError(w, err)
		return
	}
	token, issued := a.SandboxPreviews.issue(row.TenantID, row.ID, lease.Path, lease.ExpiresAt)
	if !issued {
		writeError(w, http.StatusBadGateway, "sandbox_preview_failed", "preview lease could not be brokered")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"preview_url": "/api/v1/sandbox-previews/" + row.ID + "/" + token + "/", "expires_at": lease.ExpiresAt})
}

func (a API) sandboxEvents(w http.ResponseWriter, r *http.Request) {
	_, row, ok := a.ownedSandbox(w, r)
	if !ok {
		return
	}
	events, err := a.SandboxProvider.Events(r.Context(), row.TenantID, row.BrezelSandboxID)
	if err != nil {
		a.writeSandboxProviderError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(events))
	for _, event := range events {
		id := sandboxEventIdentity(row, "activity", event.ID)
		data = append(data, map[string]any{"id": id, "sequence": event.Sequence, "type": event.Type, "state": event.State, "occurred_at": event.At, "details": event.Details})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "content_recorded": false})
}

func (a API) sandboxReceipt(w http.ResponseWriter, r *http.Request) {
	_, row, ok := a.ownedSandbox(w, r)
	if !ok {
		return
	}
	evidence, err := a.SandboxProvider.Receipt(r.Context(), row.TenantID, row.BrezelSandboxID)
	if err != nil {
		a.writeSandboxProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evidence": evidence, "provider_identity_disclosed": false})
}

func (a API) proxySandboxPreview(w http.ResponseWriter, r *http.Request) {
	if a.SandboxProvider == nil || a.SandboxPreviews == nil {
		writeError(w, http.StatusNotImplemented, "sandbox_previews_unavailable", "sandbox preview proxy is not configured")
		return
	}
	store, ok := a.sandboxStore(w)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	sandboxID := r.PathValue("sandbox")
	row, err := store.NativeSandbox(r.Context(), actor.TenantID, sandboxID)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "sandbox_not_found", "computer was not found")
		return
	}
	if err != nil || row.Status != "running" || row.DeletedAt != nil {
		writeError(w, http.StatusConflict, "sandbox_not_running", "preview requires a running computer")
		return
	}
	lease, ok := a.SandboxPreviews.resolve(r.PathValue("token"), actor.TenantID, row.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "preview_not_found", "preview lease was not found or has expired")
		return
	}
	response, err := a.SandboxProvider.ProxyPreview(r.Context(), row.TenantID, lease.ProviderPath, sandboxprovider.PreviewRequest{Method: r.Method, Path: r.PathValue("path"), RawQuery: r.URL.RawQuery})
	if err != nil {
		a.writeSandboxProviderError(w, err)
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'self'")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
	operationID := sandboxEventIdentity(row, "preview.request", strconv.FormatInt(time.Now().UnixNano(), 10))
	a.appendSandboxUsage(context.WithoutCancel(r.Context()), store, row, operationID, "preview.request", domain.SandboxUsageEvent{PreviewRequests: 1})
	_, _ = store.SetNativeSandboxStatus(context.WithoutCancel(r.Context()), row.TenantID, row.ID, row.Status, row.FailureCode)
}

func (a API) actionableSandbox(w http.ResponseWriter, r *http.Request) (sandboxProductStore, domain.NativeSandbox, bool) {
	if a.SandboxProvider == nil {
		writeError(w, http.StatusNotImplemented, "sandbox_provider_unavailable", "native private computers are not configured")
		return nil, domain.NativeSandbox{}, false
	}
	store, row, ok := a.ownedSandbox(w, r)
	if !ok {
		return nil, row, false
	}
	if row.DeletedAt != nil || row.Status != "running" || row.BrezelSandboxID == "" {
		writeError(w, http.StatusConflict, "sandbox_not_running", "this computer cannot execute work in its current state")
		return nil, row, false
	}
	return store, row, true
}

func sandboxEventIdentity(row domain.NativeSandbox, kind, identity string) string {
	digest := sha256.Sum256([]byte(row.TenantID + "\x00" + row.ID + "\x00" + kind + "\x00" + identity))
	return hex.EncodeToString(digest[:])
}
