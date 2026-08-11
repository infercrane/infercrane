package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/domain"
)

func normalizePriorities(raw string) (string, map[string]struct{}, error) {
	var values []string
	if json.Unmarshal([]byte(raw), &values) != nil || len(values) == 0 || len(values) > 3 {
		return "", nil, errors.New("allowed priorities must be a non-empty JSON array")
	}
	allowed := map[string]struct{}{}
	for _, value := range values {
		if value != "low" && value != "normal" && value != "high" {
			return "", nil, errors.New("allowed priorities may contain only low, normal, and high")
		}
		allowed[value] = struct{}{}
	}
	values = values[:0]
	for value := range allowed {
		values = append(values, value)
	}
	sort.Strings(values)
	encoded, _ := json.Marshal(values)
	return string(encoded), allowed, nil
}

func (s *Store) SetAdmissionPolicy(ctx context.Context, tenant, endpointName string, policy domain.AdmissionPolicy) (domain.AdmissionPolicy, error) {
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpointName)
	if err != nil {
		return domain.AdmissionPolicy{}, err
	}
	priorities, _, err := normalizePriorities(policy.AllowedPrioritiesJSON)
	if err != nil || policy.MaxConcurrency < 1 || policy.MaxConcurrency > 10000 || policy.MaxQueueDepth < 0 || policy.MaxQueueDepth > 100000 || policy.QueueTimeoutMS < 1 || policy.QueueTimeoutMS > 300000 || policy.MaxRequestBytes < 1024 || policy.MaxRequestBytes > 16<<20 || policy.MaxOutputTokens < 1 || policy.MaxOutputTokens > 1<<20 || policy.RetryBudget < 0 || policy.RetryBudget > 3 {
		if err != nil {
			return domain.AdmissionPolicy{}, err
		}
		return domain.AdmissionPolicy{}, errors.New("admission policy limits are out of bounds")
	}
	stamp := now()
	_, err = s.ExecContext(ctx, `INSERT INTO endpoint_admission_policies(endpoint_id,tenant_id,max_concurrency,max_queue_depth,queue_timeout_ms,max_request_bytes,max_output_tokens,allowed_priorities_json,retry_budget,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?::jsonb,?,?,?,?) ON CONFLICT(endpoint_id) DO UPDATE SET max_concurrency=EXCLUDED.max_concurrency,max_queue_depth=EXCLUDED.max_queue_depth,queue_timeout_ms=EXCLUDED.queue_timeout_ms,max_request_bytes=EXCLUDED.max_request_bytes,max_output_tokens=EXCLUDED.max_output_tokens,allowed_priorities_json=EXCLUDED.allowed_priorities_json,retry_budget=EXCLUDED.retry_budget,enabled=EXCLUDED.enabled,updated_at=EXCLUDED.updated_at WHERE endpoint_admission_policies.tenant_id=EXCLUDED.tenant_id`, resolved.Endpoint.ID, tenant, policy.MaxConcurrency, policy.MaxQueueDepth, policy.QueueTimeoutMS, policy.MaxRequestBytes, policy.MaxOutputTokens, priorities, policy.RetryBudget, policy.Enabled, stamp, stamp)
	if err != nil {
		return domain.AdmissionPolicy{}, err
	}
	return s.AdmissionPolicy(ctx, tenant, endpointName)
}

func (s *Store) AdmissionPolicy(ctx context.Context, tenant, endpointName string) (domain.AdmissionPolicy, error) {
	var policy domain.AdmissionPolicy
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT p.endpoint_id,p.tenant_id,p.max_concurrency,p.max_queue_depth,p.queue_timeout_ms,p.max_request_bytes,p.max_output_tokens,p.allowed_priorities_json::text,p.retry_budget,p.enabled,p.created_at,p.updated_at FROM endpoint_admission_policies p JOIN endpoints e ON e.id=p.endpoint_id WHERE p.tenant_id=? AND e.name=? AND e.desired_state<>'deleted'`, tenant, endpointName).Scan(&policy.EndpointID, &policy.TenantID, &policy.MaxConcurrency, &policy.MaxQueueDepth, &policy.QueueTimeoutMS, &policy.MaxRequestBytes, &policy.MaxOutputTokens, &policy.AllowedPrioritiesJSON, &policy.RetryBudget, &policy.Enabled, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AdmissionPolicy{}, ErrNotFound
		}
		return domain.AdmissionPolicy{}, err
	}
	policy.CreatedAt, policy.UpdatedAt = parseTime(created), parseTime(updated)
	return policy, nil
}

// AdmissionPolicies is the control-plane snapshot source. Gateways publish the
// returned policies into admission.Pool; Acquire itself never calls this method.
func (s *Store) AdmissionPolicies(ctx context.Context) ([]admission.Policy, error) {
	rows, err := s.QueryContext(ctx, `SELECT p.tenant_id,e.name,p.max_concurrency,p.max_queue_depth,p.queue_timeout_ms,p.max_request_bytes,p.max_output_tokens,p.allowed_priorities_json::text,p.retry_budget,p.enabled FROM endpoint_admission_policies p JOIN endpoints e ON e.id=p.endpoint_id WHERE e.desired_state='serving'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []admission.Policy
	for rows.Next() {
		var tenant, endpoint, priorities string
		var policy admission.Policy
		var timeoutMS int
		if err = rows.Scan(&tenant, &endpoint, &policy.MaxConcurrency, &policy.MaxQueueDepth, &timeoutMS, &policy.MaxRequestBytes, &policy.MaxOutputTokens, &priorities, &policy.RetryBudget, &policy.Enabled); err != nil {
			return nil, err
		}
		_, policy.AllowedPriorities, err = normalizePriorities(priorities)
		if err != nil {
			return nil, err
		}
		policy.Key = tenant + "\x00" + endpoint
		policy.QueueTimeout = time.Duration(timeoutMS) * time.Millisecond
		out = append(out, policy)
	}
	return out, rows.Err()
}
