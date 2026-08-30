package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/external"
	"github.com/infercrane/infercrane/internal/overflow"
)

func valueOrZero(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func (s *Store) SetExternalTargetPolicyForTenant(ctx context.Context, policy domain.ExternalTargetPolicy) (domain.ExternalTargetPolicy, error) {
	if policy.OverflowMode == "" {
		policy.OverflowMode = "health"
	}
	if policy.BreachIntervals == 0 {
		policy.BreachIntervals = 1
	}
	if policy.RecoveryIntervals == 0 {
		policy.RecoveryIntervals = 2
	}
	if policy.CooldownSeconds == 0 {
		policy.CooldownSeconds = 60
	}
	if policy.SignalMaxAgeSeconds == 0 {
		policy.SignalMaxAgeSeconds = 30
	}
	if policy.TenantID == "" || policy.DeploymentID == "" || policy.TargetID == "" || policy.SecretReferenceID == "" || policy.Adapter == "" || policy.RequestLimit < 1 {
		return domain.ExternalTargetPolicy{}, errors.New("tenant, deployment, target, adapter, secret reference, and positive request limit are required")
	}
	if !external.SupportedAdapter(policy.Adapter) {
		return domain.ExternalTargetPolicy{}, fmt.Errorf("unsupported external adapter %q", policy.Adapter)
	}
	if policy.Enabled && !policy.PrivacyAcknowledged {
		return domain.ExternalTargetPolicy{}, errors.New("enabled external capacity requires explicit privacy acknowledgement")
	}
	if policy.CostLimitMicrousd < 1 || policy.MaxRequestCostMicrousd < 1 || policy.MaxRequestCostMicrousd > policy.CostLimitMicrousd {
		return domain.ExternalTargetPolicy{}, errors.New("cost budget and a bounded worst-case per-request reservation are required")
	}
	if err := (overflow.Policy{Mode: policy.OverflowMode, QueueThreshold: valueOrZero(policy.QueueThreshold), BreachIntervals: policy.BreachIntervals, RecoveryIntervals: policy.RecoveryIntervals, Cooldown: time.Duration(policy.CooldownSeconds) * time.Second, SignalMaxAge: time.Duration(policy.SignalMaxAgeSeconds) * time.Second, PrivacyAcknowledged: policy.PrivacyAcknowledged || !policy.Enabled, BudgetAvailable: true}).Validate(); err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	defer tx.Rollback()
	var deploymentExists, targetExists, secretExists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployments WHERE tenant_id=? AND id=?)`, policy.TenantID, policy.DeploymentID).Scan(&deploymentExists); err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM targets WHERE tenant_id=? AND id=? AND provider=?)`, policy.TenantID, policy.TargetID, policy.Adapter).Scan(&targetExists); err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM secret_references WHERE tenant_id=? AND id=?)`, policy.TenantID, policy.SecretReferenceID).Scan(&secretExists); err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	if !deploymentExists || !targetExists || !secretExists {
		return domain.ExternalTargetPolicy{}, ErrNotFound
	}
	if policy.ID == "" {
		policy.ID, err = newID()
		if err != nil {
			return domain.ExternalTargetPolicy{}, err
		}
	}
	stamp := now()
	_, err = tx.ExecContext(ctx, `INSERT INTO external_target_policies(id,tenant_id,deployment_id,target_id,adapter,secret_reference_id,enabled,privacy_acknowledged,request_limit,cost_limit_microusd,max_request_cost_microusd,overflow_mode,queue_threshold,breach_intervals,recovery_intervals,cooldown_seconds,signal_max_age_seconds,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,deployment_id) DO UPDATE SET target_id=EXCLUDED.target_id,adapter=EXCLUDED.adapter,secret_reference_id=EXCLUDED.secret_reference_id,enabled=EXCLUDED.enabled,privacy_acknowledged=EXCLUDED.privacy_acknowledged,request_limit=EXCLUDED.request_limit,cost_limit_microusd=EXCLUDED.cost_limit_microusd,max_request_cost_microusd=EXCLUDED.max_request_cost_microusd,overflow_mode=EXCLUDED.overflow_mode,queue_threshold=EXCLUDED.queue_threshold,breach_intervals=EXCLUDED.breach_intervals,recovery_intervals=EXCLUDED.recovery_intervals,cooldown_seconds=EXCLUDED.cooldown_seconds,signal_max_age_seconds=EXCLUDED.signal_max_age_seconds,updated_at=EXCLUDED.updated_at`, policy.ID, policy.TenantID, policy.DeploymentID, policy.TargetID, policy.Adapter, policy.SecretReferenceID, policy.Enabled, policy.PrivacyAcknowledged, policy.RequestLimit, policy.CostLimitMicrousd, policy.MaxRequestCostMicrousd, policy.OverflowMode, policy.QueueThreshold, policy.BreachIntervals, policy.RecoveryIntervals, policy.CooldownSeconds, policy.SignalMaxAgeSeconds, stamp, stamp)
	if err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	return s.ExternalTargetPolicyForDeployment(ctx, policy.TenantID, policy.DeploymentID)
}

func (s *Store) ExternalTargetPolicyForDeployment(ctx context.Context, tenant, deploymentID string) (domain.ExternalTargetPolicy, error) {
	var out domain.ExternalTargetPolicy
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,deployment_id,target_id,adapter,secret_reference_id,enabled,privacy_acknowledged,request_limit,requests_reserved,cost_limit_microusd,max_request_cost_microusd,cost_reserved_microusd,overflow_mode,queue_threshold,breach_intervals,recovery_intervals,cooldown_seconds,signal_max_age_seconds,created_at,updated_at FROM external_target_policies WHERE tenant_id=? AND deployment_id=?`, tenant, deploymentID).Scan(&out.ID, &out.TenantID, &out.DeploymentID, &out.TargetID, &out.Adapter, &out.SecretReferenceID, &out.Enabled, &out.PrivacyAcknowledged, &out.RequestLimit, &out.RequestsReserved, &out.CostLimitMicrousd, &out.MaxRequestCostMicrousd, &out.CostReservedMicrousd, &out.OverflowMode, &out.QueueThreshold, &out.BreachIntervals, &out.RecoveryIntervals, &out.CooldownSeconds, &out.SignalMaxAgeSeconds, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExternalTargetPolicy{}, ErrNotFound
	}
	if err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	return out, nil
}

// LeaseExternalBudget atomically reserves a bounded request batch. Gateways
// consume leases in memory, preserving the no-PostgreSQL inference-path
// invariant. Unused reservations may reduce availability but can never exceed
// the hard persisted budget.
func (s *Store) LeaseExternalBudget(ctx context.Context, tenant, policyID string, requested int64) (domain.ExternalBudgetLease, error) {
	if tenant == "" || policyID == "" || requested < 1 || requested > 256 {
		return domain.ExternalBudgetLease{}, errors.New("tenant, policy, and lease size 1..256 are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	defer tx.Rollback()
	var enabled, privacy bool
	var limit, used, costLimit, costUsed, perRequest int64
	err = tx.QueryRowContext(ctx, `SELECT enabled,privacy_acknowledged,request_limit,requests_reserved,cost_limit_microusd,cost_reserved_microusd,max_request_cost_microusd FROM external_target_policies WHERE tenant_id=? AND id=? FOR UPDATE`, tenant, policyID).Scan(&enabled, &privacy, &limit, &used, &costLimit, &costUsed, &perRequest)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExternalBudgetLease{}, ErrNotFound
	}
	if err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	if !enabled || !privacy {
		return domain.ExternalBudgetLease{}, fmt.Errorf("%w: external policy is disabled or lacks privacy acknowledgement", ErrConflict)
	}
	available := limit - used
	if requested > available {
		requested = available
	}
	costAvailable := (costLimit - costUsed) / perRequest
	if requested > costAvailable {
		requested = costAvailable
	}
	if requested < 1 {
		return domain.ExternalBudgetLease{}, fmt.Errorf("%w: external hard budget is exhausted", ErrConflict)
	}
	reservedCost := requested * perRequest
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE external_target_policies SET requests_reserved=requests_reserved+?,cost_reserved_microusd=cost_reserved_microusd+?,updated_at=? WHERE tenant_id=? AND id=?`, requested, reservedCost, stamp, tenant, policyID); err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	return domain.ExternalBudgetLease{PolicyID: policyID, Requests: requested, ReservedCostMicrousd: reservedCost, MaxRequestCostMicrousd: perRequest}, nil
}

func (s *Store) EvaluateOverflow(ctx context.Context, tenant, deploymentID string, signal overflow.Signal, budgetAvailable bool, evaluatedAt time.Time) (overflow.Decision, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return overflow.Decision{}, err
	}
	defer tx.Rollback()
	var policy domain.ExternalTargetPolicy
	var queue sql.NullFloat64
	var cooldown, signalAge int
	err = tx.QueryRowContext(ctx, `SELECT enabled,privacy_acknowledged,overflow_mode,queue_threshold,breach_intervals,recovery_intervals,cooldown_seconds,signal_max_age_seconds FROM external_target_policies WHERE tenant_id=? AND deployment_id=? FOR UPDATE`, tenant, deploymentID).Scan(&policy.Enabled, &policy.PrivacyAcknowledged, &policy.OverflowMode, &queue, &policy.BreachIntervals, &policy.RecoveryIntervals, &cooldown, &signalAge)
	if errors.Is(err, sql.ErrNoRows) {
		return overflow.Decision{}, ErrNotFound
	}
	if err != nil {
		return overflow.Decision{}, err
	}
	if !policy.Enabled {
		return overflow.Decision{}, fmt.Errorf("%w: external policy is disabled", ErrConflict)
	}
	if queue.Valid {
		policy.QueueThreshold = &queue.Float64
	}
	state := overflow.State{}
	var changed sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT external_active,consecutive_high,consecutive_low,last_changed_at FROM overflow_states WHERE tenant_id=? AND deployment_id=? FOR UPDATE`, tenant, deploymentID).Scan(&state.External, &state.ConsecutiveHigh, &state.ConsecutiveLow, &changed)
	stateAbsent := errors.Is(err, sql.ErrNoRows)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return overflow.Decision{}, err
	}
	if changed.Valid {
		state.LastChangedAt = parseTime(changed.String)
	}
	enginePolicy := overflow.Policy{Mode: policy.OverflowMode, QueueThreshold: valueOrZero(policy.QueueThreshold), BreachIntervals: policy.BreachIntervals, RecoveryIntervals: policy.RecoveryIntervals, Cooldown: time.Duration(cooldown) * time.Second, SignalMaxAge: time.Duration(signalAge) * time.Second, PrivacyAcknowledged: policy.PrivacyAcknowledged, BudgetAvailable: budgetAvailable}
	decision, err := overflow.Evaluate(enginePolicy, state, signal, evaluatedAt)
	if err != nil {
		return overflow.Decision{}, err
	}
	lastReason := ""
	if latestErr := tx.QueryRowContext(ctx, `SELECT reason FROM overflow_decisions WHERE tenant_id=? AND deployment_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, tenant, deploymentID).Scan(&lastReason); latestErr != nil && !errors.Is(latestErr, sql.ErrNoRows) {
		return overflow.Decision{}, latestErr
	}
	nextExternal := decision.Route == "external"
	lastChanged := state.LastChangedAt
	if nextExternal != state.External {
		lastChanged = evaluatedAt
	}
	stamp := evaluatedAt.UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO overflow_states(deployment_id,tenant_id,external_active,consecutive_high,consecutive_low,last_changed_at,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(deployment_id) DO UPDATE SET external_active=EXCLUDED.external_active,consecutive_high=EXCLUDED.consecutive_high,consecutive_low=EXCLUDED.consecutive_low,last_changed_at=EXCLUDED.last_changed_at,updated_at=EXCLUDED.updated_at WHERE overflow_states.tenant_id=EXCLUDED.tenant_id`, deploymentID, tenant, nextExternal, decision.ConsecutiveHigh, decision.ConsecutiveLow, nullTime(lastChanged), stamp)
	if err != nil {
		return overflow.Decision{}, err
	}
	materialDecision := stateAbsent || decision.Action != "hold" || nextExternal != state.External || decision.ConsecutiveHigh != state.ConsecutiveHigh || decision.ConsecutiveLow != state.ConsecutiveLow || decision.Reason != lastReason
	if materialDecision {
		id, idErr := newID()
		if idErr != nil {
			return overflow.Decision{}, idErr
		}
		signalJSON, _ := json.Marshal(signal)
		policyJSON, _ := json.Marshal(enginePolicy)
		_, err = tx.ExecContext(ctx, `INSERT INTO overflow_decisions(id,tenant_id,deployment_id,route,action,reason,signal_json,policy_json,created_at) VALUES(?,?,?,?,?,?,?::jsonb,?::jsonb,?)`, id, tenant, deploymentID, decision.Route, decision.Action, decision.Reason, string(signalJSON), string(policyJSON), stamp)
		if err != nil {
			return overflow.Decision{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return overflow.Decision{}, err
	}
	return decision, nil
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}
