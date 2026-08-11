package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/infercrane/infercrane/internal/domain"
	"time"
)

func (s *Store) CreateContextPassport(ctx context.Context, tenant, deploymentName string, row domain.ContextPassport) (domain.ContextPassport, error) {
	if row.ExpiresAt.Before(time.Now().Add(time.Second)) || row.ExpiresAt.After(time.Now().Add(30*24*time.Hour)) || !json.Valid([]byte(row.CacheHintsJSON)) || !json.Valid([]byte(row.MetadataJSON)) {
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
	stamp := now()
	_, err = s.ExecContext(ctx, `INSERT INTO context_passports(id,tenant_id,deployment_id,status,preferred_binding_id,preferred_target_id,cache_hints_json,metadata_json,last_activity,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?::jsonb,?::jsonb,?,?,?,?)`, row.ID, tenant, row.DeploymentID, row.Status, null(row.PreferredBindingID), null(row.PreferredTargetID), row.CacheHintsJSON, row.MetadataJSON, stamp, row.ExpiresAt, stamp, stamp)
	row.LastActivity, row.CreatedAt, row.UpdatedAt = parseTime(stamp), parseTime(stamp), parseTime(stamp)
	return row, err
}
func (s *Store) ContextPassport(ctx context.Context, tenant, id string) (domain.ContextPassport, error) {
	var r domain.ContextPassport
	var activity, expiry, created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,COALESCE(endpoint_id,''),COALESCE(deployment_id,''),status,COALESCE(preferred_binding_id,''),COALESCE(preferred_target_id,''),cache_hints_json::text,metadata_json::text,last_activity,expires_at,created_at,updated_at FROM context_passports WHERE tenant_id=? AND id=?`, tenant, id).Scan(&r.ID, &r.TenantID, &r.EndpointID, &r.DeploymentID, &r.Status, &r.PreferredBindingID, &r.PreferredTargetID, &r.CacheHintsJSON, &r.MetadataJSON, &activity, &expiry, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return r, domain.ErrNotFound
	}
	r.LastActivity, r.ExpiresAt, r.CreatedAt, r.UpdatedAt = parseTime(activity), parseTime(expiry), parseTime(created), parseTime(updated)
	return r, err
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
