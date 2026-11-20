package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// StayOptimizedActor approves a proposed run when the policy's opt-in
// auto-run does it instead of a person.
const StayOptimizedActor = "stay-optimized"

const (
	maxStayOptimizedObserved = 64 << 10
	maxCheckError            = 512
)

// ClaimDueStayOptimizedChecks leases up to limit endpoints whose Stay
// optimized policy is on and whose next check is due. A lease is held by one
// owner until it completes the check or the lease expires, so instances never
// check one endpoint at the same time.
func (s *Store) ClaimDueStayOptimizedChecks(ctx context.Context, owner string, lease time.Duration, limit int) ([]domain.StayOptimizedCheck, error) {
	if strings.TrimSpace(owner) == "" || lease <= 0 || limit < 1 || limit > 100 {
		return nil, errors.New("claim requires an owner, a positive lease, and a limit of 1..100")
	}
	stamp := time.Now().UTC()
	expires := stamp.Add(lease)
	rows, err := s.QueryContext(ctx, `INSERT INTO continual_optimization_states(tenant_id,deployment_id,generation,created_at,updated_at,lease_owner,lease_expires_at)
SELECT p.tenant_id,p.deployment_id,1,?,?,?,? FROM continual_optimization_policies p
JOIN deployments d ON d.id=p.deployment_id AND d.tenant_id=p.tenant_id AND d.desired_state='running'
LEFT JOIN continual_optimization_states st ON st.deployment_id=p.deployment_id
WHERE COALESCE((p.policy_json->>'enabled')::boolean,false)
AND (st.deployment_id IS NULL OR ((st.next_check_at IS NULL OR st.next_check_at<=?) AND (st.lease_expires_at IS NULL OR st.lease_expires_at<=?)))
ORDER BY st.next_check_at NULLS FIRST LIMIT ?
ON CONFLICT(deployment_id) DO UPDATE SET lease_owner=EXCLUDED.lease_owner,lease_expires_at=EXCLUDED.lease_expires_at,updated_at=EXCLUDED.updated_at
WHERE continual_optimization_states.tenant_id=EXCLUDED.tenant_id
AND (continual_optimization_states.lease_expires_at IS NULL OR continual_optimization_states.lease_expires_at<=?)
AND (continual_optimization_states.next_check_at IS NULL OR continual_optimization_states.next_check_at<=?)
RETURNING tenant_id,deployment_id,observed_json::text`, stamp, stamp, owner, expires, stamp, stamp, limit, stamp, stamp)
	if err != nil {
		return nil, err
	}
	claims, err := scanStayOptimizedClaims(rows, owner, expires)
	if err != nil {
		return nil, err
	}
	return s.nameStayOptimizedClaims(ctx, claims)
}

// ClaimStayOptimizedCheck leases one endpoint's check now, whether or not it
// is due, for a person's Check now. A check another owner holds is a
// conflict.
func (s *Store) ClaimStayOptimizedCheck(ctx context.Context, tenant, deployment, owner string, lease time.Duration) (domain.StayOptimizedCheck, error) {
	if strings.TrimSpace(owner) == "" || lease <= 0 {
		return domain.StayOptimizedCheck{}, errors.New("claim requires an owner and a positive lease")
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deployment)
	if err != nil {
		return domain.StayOptimizedCheck{}, err
	}
	stamp := time.Now().UTC()
	expires := stamp.Add(lease)
	rows, err := s.QueryContext(ctx, `INSERT INTO continual_optimization_states(tenant_id,deployment_id,generation,created_at,updated_at,lease_owner,lease_expires_at) VALUES(?,?,1,?,?,?,?)
ON CONFLICT(deployment_id) DO UPDATE SET lease_owner=EXCLUDED.lease_owner,lease_expires_at=EXCLUDED.lease_expires_at,updated_at=EXCLUDED.updated_at
WHERE continual_optimization_states.tenant_id=EXCLUDED.tenant_id AND (continual_optimization_states.lease_expires_at IS NULL OR continual_optimization_states.lease_expires_at<=?)
RETURNING tenant_id,deployment_id,observed_json::text`, tenant, resolved.Deployment.ID, stamp, stamp, owner, expires, stamp)
	if err != nil {
		return domain.StayOptimizedCheck{}, err
	}
	claims, err := scanStayOptimizedClaims(rows, owner, expires)
	if err != nil {
		return domain.StayOptimizedCheck{}, err
	}
	if len(claims) == 0 {
		return domain.StayOptimizedCheck{}, domain.ErrConflict
	}
	claims[0].DeploymentName = resolved.Deployment.Name
	return claims[0], nil
}

func scanStayOptimizedClaims(rows *sql.Rows, owner string, expires time.Time) ([]domain.StayOptimizedCheck, error) {
	defer rows.Close()
	var claims []domain.StayOptimizedCheck
	for rows.Next() {
		claim := domain.StayOptimizedCheck{LeaseOwner: owner, LeaseExpiresAt: expires}
		if err := rows.Scan(&claim.TenantID, &claim.DeploymentID, &claim.ObservedJSON); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func (s *Store) nameStayOptimizedClaims(ctx context.Context, claims []domain.StayOptimizedCheck) ([]domain.StayOptimizedCheck, error) {
	for index := range claims {
		if err := s.QueryRowContext(ctx, `SELECT name FROM deployments WHERE tenant_id=? AND id=?`, claims[index].TenantID, claims[index].DeploymentID).Scan(&claims[index].DeploymentName); err != nil {
			return nil, err
		}
	}
	return claims, nil
}

// CompleteStayOptimizedCheck records what a check saw, when the next one is
// due, and releases the lease. It changes nothing if the lease was lost.
func (s *Store) CompleteStayOptimizedCheck(ctx context.Context, check domain.StayOptimizedCheck, observedJSON string, nextCheckAt time.Time, checkError string) error {
	if observedJSON == "" {
		observedJSON = "{}"
	}
	if len(observedJSON) > maxStayOptimizedObserved || !json.Valid([]byte(observedJSON)) || nextCheckAt.IsZero() {
		return errors.New("check observation must be bounded JSON with a next check time")
	}
	if len(checkError) > maxCheckError {
		checkError = checkError[:maxCheckError]
	}
	stamp := time.Now().UTC()
	result, err := s.ExecContext(ctx, `UPDATE continual_optimization_states SET lease_owner=NULL,lease_expires_at=NULL,last_check_at=?,next_check_at=?,observed_json=?::jsonb,last_check_error=NULLIF(?,''),updated_at=? WHERE tenant_id=? AND deployment_id=? AND lease_owner=?`, stamp, nextCheckAt.UTC(), observedJSON, checkError, stamp, check.TenantID, check.DeploymentID, check.LeaseOwner)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.ErrConflict
	}
	return nil
}

// StayOptimizedProposals lists the runs Stay optimized proposed for one
// deployment, newest first.
func (s *Store) StayOptimizedProposals(ctx context.Context, tenant, deployment string, limit int) ([]domain.OptimizationCampaign, error) {
	if limit < 1 || limit > 50 {
		limit = 10
	}
	rows, err := s.QueryContext(ctx, `SELECT id FROM optimization_campaigns WHERE tenant_id=? AND target_deployment_name=? AND origin_json IS NOT NULL ORDER BY created_at DESC LIMIT ?`, tenant, deployment, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	out := make([]domain.OptimizationCampaign, 0, len(ids))
	for _, id := range ids {
		campaign, lookupErr := s.OptimizationCampaign(ctx, tenant, id)
		if lookupErr != nil {
			return nil, lookupErr
		}
		out = append(out, campaign)
	}
	return out, nil
}

// SetOptimizationCampaignOrigin replaces a proposed run's origin, so it can
// record what auto-run did after the run was created.
func (s *Store) SetOptimizationCampaignOrigin(ctx context.Context, tenant, id, originJSON string) error {
	if !boundedJSON(originJSON) {
		return errors.New("origin must be bounded JSON")
	}
	result, err := s.ExecContext(ctx, `UPDATE optimization_campaigns SET origin_json=?::jsonb WHERE tenant_id=? AND id=? AND origin_json IS NOT NULL`, originJSON, tenant, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// StayOptimizedAutoRunSpend sums the spend caps auto-run approved for one
// deployment since a time (the start of the month).
func (s *Store) StayOptimizedAutoRunSpend(ctx context.Context, tenant, deployment string, since time.Time) (float64, error) {
	var total float64
	err := s.QueryRowContext(ctx, `SELECT COALESCE(SUM(approved_max_cost_usd),0) FROM optimization_campaigns WHERE tenant_id=? AND target_deployment_name=? AND origin_json IS NOT NULL AND approved_by=? AND approved_at>=?`, tenant, deployment, StayOptimizedActor, since.UTC()).Scan(&total)
	return total, err
}

const endpointCustomKernelColumns = `id,tenant_id,deployment_id,operator,family,engine,engine_version,image_digest,state,state_reason,COALESCE(source_campaign_id,''),created_at,updated_at`

// RecordEndpointCustomKernel records a kernel qualified for or serving on a
// deployment, pinned to one engine release. Recording the same kernel on the
// same engine release again updates its state.
func (s *Store) RecordEndpointCustomKernel(ctx context.Context, kernel domain.EndpointCustomKernel) (domain.EndpointCustomKernel, error) {
	if kernel.TenantID == "" || kernel.DeploymentID == "" || kernel.Operator == "" || kernel.Family == "" || kernel.Engine == "" || kernel.EngineVersion == "" || (kernel.State != "qualified" && kernel.State != "serving") {
		return domain.EndpointCustomKernel{}, errors.New("a kernel record needs its deployment, operator, family, engine release, and a qualified or serving state")
	}
	id, err := newID()
	if err != nil {
		return domain.EndpointCustomKernel{}, err
	}
	stamp := time.Now().UTC()
	row := s.QueryRowContext(ctx, `INSERT INTO endpoint_custom_kernels(id,tenant_id,deployment_id,operator,family,engine,engine_version,image_digest,state,state_reason,source_campaign_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,NULLIF(?,''),?,?)
ON CONFLICT(tenant_id,deployment_id,operator,engine,engine_version) DO UPDATE SET family=EXCLUDED.family,image_digest=EXCLUDED.image_digest,state=EXCLUDED.state,state_reason=EXCLUDED.state_reason,source_campaign_id=EXCLUDED.source_campaign_id,updated_at=EXCLUDED.updated_at
RETURNING `+endpointCustomKernelColumns, id, kernel.TenantID, kernel.DeploymentID, kernel.Operator, kernel.Family, kernel.Engine, kernel.EngineVersion, kernel.ImageDigest, kernel.State, kernel.StateReason, kernel.SourceCampaignID, stamp, stamp)
	return scanEndpointCustomKernel(row)
}

// EndpointCustomKernels lists a deployment's kernels.
func (s *Store) EndpointCustomKernels(ctx context.Context, tenant, deploymentID string) ([]domain.EndpointCustomKernel, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+endpointCustomKernelColumns+` FROM endpoint_custom_kernels WHERE tenant_id=? AND deployment_id=? ORDER BY operator,engine,engine_version`, tenant, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EndpointCustomKernel
	for rows.Next() {
		kernel, scanErr := scanEndpointCustomKernel(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, kernel)
	}
	return out, rows.Err()
}

// SetEndpointCustomKernelState moves one kernel out of qualified or serving.
// A kernel already moved, or moved back by a run that re-qualified it, is
// left as it is.
func (s *Store) SetEndpointCustomKernelState(ctx context.Context, tenant, id, from, state, reason string) error {
	if (from != "qualified" && from != "serving") || (state != "requalification_needed" && state != "off") || len(reason) > 512 {
		return errors.New("a kernel moves only from qualified or serving to requalification_needed or off")
	}
	result, err := s.ExecContext(ctx, `UPDATE endpoint_custom_kernels SET state=?,state_reason=?,updated_at=? WHERE tenant_id=? AND id=? AND state=?`, state, reason, time.Now().UTC(), tenant, id, from)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.ErrConflict
	}
	return nil
}

func scanEndpointCustomKernel(row rowScanner) (domain.EndpointCustomKernel, error) {
	var kernel domain.EndpointCustomKernel
	var created, updated time.Time
	if err := row.Scan(&kernel.ID, &kernel.TenantID, &kernel.DeploymentID, &kernel.Operator, &kernel.Family, &kernel.Engine, &kernel.EngineVersion, &kernel.ImageDigest, &kernel.State, &kernel.StateReason, &kernel.SourceCampaignID, &created, &updated); err != nil {
		return domain.EndpointCustomKernel{}, err
	}
	kernel.CreatedAt, kernel.UpdatedAt = created.UTC(), updated.UTC()
	return kernel, nil
}

func optionalTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	stamp := value.Time.UTC()
	return &stamp
}
