package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) CreateContextPassport(ctx context.Context, tenant, deploymentName string, row domain.ContextPassport) (domain.ContextPassport, error) {
	stampTime := time.Now().UTC()
	if row.ExpiresAt.Before(stampTime.Add(time.Second)) || row.ExpiresAt.After(stampTime.Add(30*24*time.Hour)) || !json.Valid([]byte(row.CacheHintsJSON)) || !json.Valid([]byte(row.MetadataJSON)) {
		return row, errors.New("bounded expiry and valid metadata are required")
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deploymentName)
	if err != nil {
		return row, err
	}
	row.ID, err = newID()
	if err != nil {
		return row, err
	}
	row.TenantID, row.DeploymentID, row.Status = tenant, resolved.Deployment.ID, "active"
	row.DeploymentName = resolved.Deployment.Name
	stamp := stampTime.Format(time.RFC3339Nano)
	expires := row.ExpiresAt.UTC().Format(time.RFC3339Nano)
	_, err = s.ExecContext(ctx, `INSERT INTO context_passports(id,tenant_id,deployment_id,status,preferred_binding_id,preferred_target_id,cache_hints_json,metadata_json,last_activity,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?::jsonb,?::jsonb,?,?,?,?)`, row.ID, tenant, row.DeploymentID, row.Status, null(row.PreferredBindingID), null(row.PreferredTargetID), row.CacheHintsJSON, row.MetadataJSON, stamp, expires, stamp, stamp)
	if err != nil {
		return row, fmt.Errorf("persist context passport with expiry %s after creation %s: %w", expires, stamp, err)
	}
	row.LastActivity, row.CreatedAt, row.UpdatedAt = parseTime(stamp), parseTime(stamp), parseTime(stamp)
	return row, nil
}
func (s *Store) ContextPassport(ctx context.Context, tenant, id string) (domain.ContextPassport, error) {
	var r domain.ContextPassport
	var activity, expiry, created, updated string
	err := s.QueryRowContext(ctx, `SELECT c.id,c.tenant_id,COALESCE(c.endpoint_id,''),COALESCE(c.deployment_id,''),COALESCE(d.name,''),c.status,COALESCE(c.preferred_binding_id,''),COALESCE(c.preferred_target_id,''),c.cache_hints_json::text,c.metadata_json::text,c.last_activity,c.expires_at,c.created_at,c.updated_at FROM context_passports c LEFT JOIN deployments d ON d.id=c.deployment_id WHERE c.tenant_id=? AND c.id=?`, tenant, id).Scan(&r.ID, &r.TenantID, &r.EndpointID, &r.DeploymentID, &r.DeploymentName, &r.Status, &r.PreferredBindingID, &r.PreferredTargetID, &r.CacheHintsJSON, &r.MetadataJSON, &activity, &expiry, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return r, domain.ErrNotFound
	}
	r.LastActivity, r.ExpiresAt, r.CreatedAt, r.UpdatedAt = parseTime(activity), parseTime(expiry), parseTime(created), parseTime(updated)
	return r, err
}

func (s *Store) ActiveContextPassports(ctx context.Context, at time.Time, limit int) ([]domain.ContextPassport, error) {
	if limit < 1 || limit > 10000 {
		limit = 10000
	}
	rows, err := s.QueryContext(ctx, `SELECT c.id,c.tenant_id,COALESCE(c.endpoint_id,''),COALESCE(c.deployment_id,''),COALESCE(d.name,''),c.status,COALESCE(c.preferred_binding_id,''),COALESCE(c.preferred_target_id,''),c.cache_hints_json::text,c.metadata_json::text,c.last_activity,c.expires_at,c.created_at,c.updated_at FROM context_passports c LEFT JOIN deployments d ON d.id=c.deployment_id WHERE c.status='active' AND c.expires_at>? ORDER BY c.expires_at LIMIT ?`, at, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ContextPassport
	for rows.Next() {
		var r domain.ContextPassport
		var activity, expiry, created, updated string
		if err = rows.Scan(&r.ID, &r.TenantID, &r.EndpointID, &r.DeploymentID, &r.DeploymentName, &r.Status, &r.PreferredBindingID, &r.PreferredTargetID, &r.CacheHintsJSON, &r.MetadataJSON, &activity, &expiry, &created, &updated); err != nil {
			return nil, err
		}
		r.LastActivity, r.ExpiresAt, r.CreatedAt, r.UpdatedAt = parseTime(activity), parseTime(expiry), parseTime(created), parseTime(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) SetBurstGuardPolicy(ctx context.Context, tenant, name string, p domain.BurstGuardPolicy) (domain.BurstGuardPolicy, error) {
	resolved, err := s.ResolveForTenant(ctx, tenant, name)
	if err != nil {
		return p, err
	}
	external, err := s.ExternalTargetPolicyForDeployment(ctx, tenant, resolved.Deployment.ID)
	if err != nil {
		return p, errors.New("governed external fallback policy is required")
	}
	if p.QueueThreshold < 0 || p.BreachIntervals < 1 || p.RecoveryIntervals < 1 || p.CooldownSeconds < 1 || p.SignalMaxAgeSeconds < 1 || p.MaxIncrementalCostMicrousdHour < 1 {
		return p, errors.New("complete bounded burst policy is required")
	}
	p.ID, err = newID()
	if err != nil {
		return p, err
	}
	p.TenantID, p.DeploymentID, p.ExternalPolicyID = tenant, resolved.Deployment.ID, external.ID
	stamp := now()
	var created, updated string
	err = s.QueryRowContext(ctx, `INSERT INTO burst_guard_policies(id,tenant_id,deployment_id,external_policy_id,enabled,queue_threshold,breach_intervals,recovery_intervals,cooldown_seconds,signal_max_age_seconds,max_incremental_cost_microusd_hour,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(deployment_id) DO UPDATE SET external_policy_id=EXCLUDED.external_policy_id,enabled=EXCLUDED.enabled,queue_threshold=EXCLUDED.queue_threshold,breach_intervals=EXCLUDED.breach_intervals,recovery_intervals=EXCLUDED.recovery_intervals,cooldown_seconds=EXCLUDED.cooldown_seconds,signal_max_age_seconds=EXCLUDED.signal_max_age_seconds,max_incremental_cost_microusd_hour=EXCLUDED.max_incremental_cost_microusd_hour,updated_at=EXCLUDED.updated_at RETURNING id,created_at,updated_at`, p.ID, tenant, p.DeploymentID, p.ExternalPolicyID, p.Enabled, p.QueueThreshold, p.BreachIntervals, p.RecoveryIntervals, p.CooldownSeconds, p.SignalMaxAgeSeconds, p.MaxIncrementalCostMicrousdHour, stamp, stamp).Scan(&p.ID, &created, &updated)
	p.CreatedAt, p.UpdatedAt = parseTime(created), parseTime(updated)
	return p, err
}
func (s *Store) RecordBurstGuardDecision(ctx context.Context, row domain.BurstGuardDecision) (domain.BurstGuardDecision, error) {
	if row.TenantID == "" || row.DeploymentID == "" || row.PolicyID == "" || row.Reason == "" || !json.Valid([]byte(row.EvidenceJSON)) {
		return row, errors.New("complete burst decision is required")
	}
	row.ID, _ = newID()
	stamp := now()
	_, err := s.ExecContext(ctx, `INSERT INTO burst_guard_decisions(id,tenant_id,deployment_id,policy_id,decision,reason,incremental_cost_microusd_hour,evidence_json,created_at) VALUES(?,?,?,?,?,?,?,?::jsonb,?)`, row.ID, row.TenantID, row.DeploymentID, row.PolicyID, row.Decision, row.Reason, row.IncrementalCostMicrousdHour, row.EvidenceJSON, stamp)
	row.CreatedAt = parseTime(stamp)
	return row, err
}
