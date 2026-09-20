package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
)

var nativeSandboxStatuses = map[string]bool{
	"creating_workspace": true, "creating": true, "running": true,
	"standby": true, "pausing": true, "resuming": true,
	"deleting": true, "deleted": true, "expired": true,
	"failed": true, "unknown": true, "cleanup_pending": true,
}

func (s *Store) CreateNativeSandbox(ctx context.Context, row domain.NativeSandbox) (domain.NativeSandbox, bool, error) {
	if err := validateNativeSandbox(row); err != nil {
		return row, false, err
	}
	requested := row
	var err error
	if row.ID == "" {
		row.ID, err = newID()
		if err != nil {
			return row, false, err
		}
	}
	stamp := now()
	result, err := s.ExecContext(ctx, `INSERT INTO native_sandboxes(id,tenant_id,created_by,display_name,purpose,source_type,source_reference,template_id,model_endpoint,brezel_project_id,brezel_workspace_id,brezel_sandbox_id,status,failure_code,idempotency_key,input_digest,created_at,updated_at,last_active_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'',?,?,?, ?, ?) ON CONFLICT(tenant_id,idempotency_key) DO NOTHING`, row.ID, row.TenantID, row.CreatedBy, row.DisplayName, row.Purpose, row.SourceType, row.SourceReference, row.TemplateID, row.ModelEndpoint, row.BrezelProjectID, row.BrezelWorkspaceID, row.BrezelSandboxID, row.Status, row.IdempotencyKey, row.InputDigest, stamp, stamp, stamp)
	if err != nil {
		return row, false, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		existing, lookupErr := s.NativeSandboxByIdempotencyKey(ctx, requested.TenantID, requested.IdempotencyKey)
		if lookupErr != nil {
			return row, false, lookupErr
		}
		if existing.InputDigest != requested.InputDigest || existing.DisplayName != requested.DisplayName || existing.Purpose != requested.Purpose || existing.SourceType != requested.SourceType || existing.SourceReference != requested.SourceReference || existing.TemplateID != requested.TemplateID || existing.ModelEndpoint != requested.ModelEndpoint {
			return existing, false, domain.ErrConflict
		}
		return existing, false, nil
	}
	created, err := s.NativeSandbox(ctx, row.TenantID, row.ID)
	return created, true, err
}

func validateNativeSandbox(row domain.NativeSandbox) error {
	if row.TenantID == "" || strings.TrimSpace(row.CreatedBy) == "" || len(row.CreatedBy) > 255 || strings.TrimSpace(row.DisplayName) == "" || len(row.DisplayName) > 120 {
		return errors.New("tenant, creator, and a display name of at most 120 characters are required")
	}
	if row.Purpose != "coding_agent" && row.Purpose != "evaluation" && row.Purpose != "background_task" && row.Purpose != "blank_computer" {
		return errors.New("unsupported sandbox purpose")
	}
	if row.SourceType != "git_repository" && row.SourceType != "upload" && row.SourceType != "empty_workspace" {
		return errors.New("unsupported sandbox source")
	}
	switch row.SourceType {
	case "empty_workspace":
		if row.SourceReference != "" {
			return errors.New("an empty workspace cannot include a source reference")
		}
	case "upload":
		if !integrationNamePattern.MatchString(row.SourceReference) {
			return errors.New("upload source must be an opaque InferCrane reference")
		}
	case "git_repository":
		repository, err := url.Parse(row.SourceReference)
		if err != nil || repository.Scheme != "https" || repository.Host == "" || repository.User != nil || repository.RawQuery != "" || repository.Fragment != "" {
			return errors.New("git source must be an HTTPS URL without credentials, query, or fragment")
		}
	}
	if len(row.SourceReference) > 2048 || len(row.TemplateID) == 0 || len(row.TemplateID) > 128 || len(row.ModelEndpoint) > 255 {
		return errors.New("sandbox source, template, or model endpoint is invalid")
	}
	if row.IdempotencyKey == "" || len(row.IdempotencyKey) > 128 || len(row.InputDigest) != 64 {
		return errors.New("a bounded idempotency key and SHA-256 input digest are required")
	}
	if !nativeSandboxStatuses[row.Status] {
		return errors.New("unsupported sandbox state")
	}
	return nil
}

func (s *Store) NativeSandbox(ctx context.Context, tenant, id string) (domain.NativeSandbox, error) {
	return s.nativeSandbox(ctx, `tenant_id=? AND id=?`, tenant, id)
}

func (s *Store) NativeSandboxByIdempotencyKey(ctx context.Context, tenant, key string) (domain.NativeSandbox, error) {
	return s.nativeSandbox(ctx, `tenant_id=? AND idempotency_key=?`, tenant, key)
}

func (s *Store) nativeSandbox(ctx context.Context, predicate string, values ...any) (domain.NativeSandbox, error) {
	var row domain.NativeSandbox
	var created, updated, active string
	var deleted sql.NullString
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,created_by,display_name,purpose,source_type,source_reference,template_id,model_endpoint,brezel_project_id,brezel_workspace_id,brezel_sandbox_id,status,failure_code,idempotency_key,input_digest,created_at,updated_at,last_active_at,deleted_at FROM native_sandboxes WHERE `+predicate, values...).Scan(&row.ID, &row.TenantID, &row.CreatedBy, &row.DisplayName, &row.Purpose, &row.SourceType, &row.SourceReference, &row.TemplateID, &row.ModelEndpoint, &row.BrezelProjectID, &row.BrezelWorkspaceID, &row.BrezelSandboxID, &row.Status, &row.FailureCode, &row.IdempotencyKey, &row.InputDigest, &created, &updated, &active, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return row, domain.ErrNotFound
	}
	if err != nil {
		return row, err
	}
	row.CreatedAt, row.UpdatedAt, row.LastActiveAt = parseTime(created), parseTime(updated), parseTime(active)
	if deleted.Valid {
		stamp := parseTime(deleted.String)
		row.DeletedAt = &stamp
	}
	return row, nil
}

func (s *Store) NativeSandboxes(ctx context.Context, tenant string, includeTerminal bool) ([]domain.NativeSandbox, error) {
	predicate := `tenant_id=? AND deleted_at IS NULL`
	if includeTerminal {
		predicate = `tenant_id=?`
	}
	rows, err := s.QueryContext(ctx, `SELECT id FROM native_sandboxes WHERE `+predicate+` ORDER BY last_active_at DESC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]domain.NativeSandbox, 0, len(ids))
	for _, id := range ids {
		row, lookupErr := s.NativeSandbox(ctx, tenant, id)
		if lookupErr != nil {
			return nil, lookupErr
		}
		out = append(out, row)
	}
	return out, nil
}

// SetNativeSandboxProviderRefs persists provider identities before a response
// is exposed to the customer, so every later operation can authorize against
// the InferCrane record rather than trusting a provider-supplied ID.
func (s *Store) SetNativeSandboxProviderRefs(ctx context.Context, tenant, id, workspaceID, sandboxID, status, failureCode string) (domain.NativeSandbox, error) {
	if !nativeSandboxStatuses[status] {
		return domain.NativeSandbox{}, errors.New("unsupported sandbox state")
	}
	result, err := s.ExecContext(ctx, `UPDATE native_sandboxes SET brezel_workspace_id=CASE WHEN ?='' THEN brezel_workspace_id ELSE ? END,brezel_sandbox_id=CASE WHEN ?='' THEN brezel_sandbox_id ELSE ? END,status=?,failure_code=?,updated_at=?,last_active_at=? WHERE tenant_id=? AND id=? AND deleted_at IS NULL`, workspaceID, workspaceID, sandboxID, sandboxID, status, failureCode, now(), now(), tenant, id)
	if err != nil {
		return domain.NativeSandbox{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.NativeSandbox{}, domain.ErrNotFound
	}
	return s.NativeSandbox(ctx, tenant, id)
}

func (s *Store) SetNativeSandboxStatus(ctx context.Context, tenant, id, status, failureCode string) (domain.NativeSandbox, error) {
	if !nativeSandboxStatuses[status] {
		return domain.NativeSandbox{}, errors.New("unsupported sandbox state")
	}
	deleted := any(nil)
	if status == "deleted" {
		deleted = now()
	}
	result, err := s.ExecContext(ctx, `UPDATE native_sandboxes SET status=?,failure_code=?,updated_at=?,last_active_at=?,deleted_at=COALESCE(?,deleted_at) WHERE tenant_id=? AND id=?`, status, failureCode, now(), now(), deleted, tenant, id)
	if err != nil {
		return domain.NativeSandbox{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.NativeSandbox{}, domain.ErrNotFound
	}
	return s.NativeSandbox(ctx, tenant, id)
}

func (s *Store) AppendSandboxUsageEvent(ctx context.Context, event domain.SandboxUsageEvent) (bool, error) {
	if event.EventID == "" || event.TenantID == "" || event.SandboxID == "" || event.EventType == "" || event.TemplateID == "" || event.RuntimeClass == "" || event.MetadataVersion < 1 {
		return false, errors.New("complete sandbox usage identity is required")
	}
	if event.RunningMilliseconds < 0 || event.StandbyMilliseconds < 0 || event.CommandDurationMilliseconds < 0 || event.FileIngressBytes < 0 || event.FileEgressBytes < 0 || event.PreviewRequests < 0 {
		return false, errors.New("sandbox usage counters cannot be negative")
	}
	if event.OccurredAt.IsZero() {
		return false, errors.New("sandbox usage occurrence time is required")
	}
	result, err := s.ExecContext(ctx, `INSERT INTO sandbox_usage_events(event_id,tenant_id,sandbox_id,provider_operation_id,event_type,occurred_at,template_id,runtime_class,running_milliseconds,standby_milliseconds,command_duration_milliseconds,command_exit_class,file_ingress_bytes,file_egress_bytes,preview_requests,metadata_version) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM native_sandboxes WHERE tenant_id=? AND id=?) ON CONFLICT DO NOTHING`, event.EventID, event.TenantID, event.SandboxID, event.ProviderOperationID, event.EventType, event.OccurredAt.UTC(), event.TemplateID, event.RuntimeClass, event.RunningMilliseconds, event.StandbyMilliseconds, event.CommandDurationMilliseconds, event.CommandExitClass, event.FileIngressBytes, event.FileEgressBytes, event.PreviewRequests, event.MetadataVersion, event.TenantID, event.SandboxID)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		if _, lookupErr := s.NativeSandbox(ctx, event.TenantID, event.SandboxID); lookupErr != nil {
			return false, fmt.Errorf("%w: sandbox", lookupErr)
		}
	}
	return count == 1, nil
}

func (s *Store) SandboxUsageSummary(ctx context.Context, tenant string) (domain.SandboxUsageSummary, error) {
	var summary domain.SandboxUsageSummary
	err := s.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM native_sandboxes WHERE tenant_id=? AND deleted_at IS NULL AND status NOT IN ('deleted','expired','failed')),COALESCE(SUM(running_milliseconds),0),COALESCE(SUM(standby_milliseconds),0),COALESCE(SUM(CASE WHEN event_type='command.completed' THEN 1 ELSE 0 END),0),COALESCE(SUM(file_ingress_bytes),0),COALESCE(SUM(file_egress_bytes),0),COALESCE(SUM(preview_requests),0) FROM sandbox_usage_events WHERE tenant_id=?`, tenant, tenant).Scan(&summary.ActiveComputers, &summary.RunningMilliseconds, &summary.StandbyMilliseconds, &summary.CommandsExecuted, &summary.FileIngressBytes, &summary.FileEgressBytes, &summary.PreviewRequests)
	return summary, err
}
