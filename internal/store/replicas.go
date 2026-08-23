package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

const replicaColumns = `id,tenant_id,deployment_id,revision_id,ordinal,external_key,lifecycle_state,provider,COALESCE(provider_request_id,''),COALESCE(provider_resource_id,''),COALESCE(endpoint,''),health,provider_details_json::text,last_observed_at,created_at,updated_at`

func (s *Store) EnsureReplicaIntent(ctx context.Context, replica domain.Replica) (domain.Replica, bool, error) {
	if replica.TenantID == "" {
		replica.TenantID = "global"
	}
	if replica.DeploymentID == "" || replica.ExternalKey == "" || replica.Provider == "" || replica.Ordinal < 0 {
		return domain.Replica{}, false, errors.New("deployment, external key, provider, and non-negative ordinal are required")
	}
	if replica.RevisionID == "" {
		if err := s.QueryRowContext(ctx, `SELECT active_revision_id FROM deployments WHERE id=? AND tenant_id=?`, replica.DeploymentID, replica.TenantID).Scan(&replica.RevisionID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.Replica{}, false, ErrNotFound
			}
			return domain.Replica{}, false, err
		}
	}
	existing, err := s.ReplicaByExternalKey(ctx, replica.Provider, replica.ExternalKey)
	if err == nil {
		if existing.TenantID != replica.TenantID || existing.DeploymentID != replica.DeploymentID || existing.RevisionID != replica.RevisionID || existing.Ordinal != replica.Ordinal {
			return domain.Replica{}, false, fmt.Errorf("%w: external key belongs to another replica", ErrConflict)
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return domain.Replica{}, false, err
	}
	id, err := newID()
	if err != nil {
		return domain.Replica{}, false, err
	}
	stamp := now()
	replica.ID, replica.LifecycleState, replica.Health, replica.ProviderDetails = id, "pending", "unknown", "{}"
	replica.CreatedAt, replica.UpdatedAt = parseTime(stamp), parseTime(stamp)
	_, err = s.ExecContext(ctx, `INSERT INTO replicas(id,tenant_id,deployment_id,revision_id,ordinal,external_key,lifecycle_state,provider,health,provider_details_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,?,?)`, replica.ID, replica.TenantID, replica.DeploymentID, replica.RevisionID, replica.Ordinal, replica.ExternalKey, replica.LifecycleState, replica.Provider, replica.Health, replica.ProviderDetails, stamp, stamp)
	if isUniqueViolation(err) {
		existing, lookupErr := s.ReplicaByExternalKey(ctx, replica.Provider, replica.ExternalKey)
		if lookupErr == nil && existing.TenantID == replica.TenantID && existing.DeploymentID == replica.DeploymentID && existing.RevisionID == replica.RevisionID && existing.Ordinal == replica.Ordinal {
			return existing, false, nil
		}
		return domain.Replica{}, false, fmt.Errorf("%w: replica ordinal or external key already exists", ErrConflict)
	}
	return replica, err == nil, err
}

func (s *Store) Replica(ctx context.Context, id string) (domain.Replica, error) {
	return scanReplica(s.QueryRowContext(ctx, `SELECT `+replicaColumns+` FROM replicas WHERE id=?`, id))
}
func (s *Store) ReplicaByExternalKey(ctx context.Context, provider, externalKey string) (domain.Replica, error) {
	return scanReplica(s.QueryRowContext(ctx, `SELECT `+replicaColumns+` FROM replicas WHERE provider=? AND external_key=?`, provider, externalKey))
}
func (s *Store) ReplicasForDeployment(ctx context.Context, tenant, deploymentID string) ([]domain.Replica, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+replicaColumns+` FROM replicas WHERE tenant_id=? AND deployment_id=? ORDER BY ordinal`, tenant, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var replicas []domain.Replica
	for rows.Next() {
		replica, err := scanReplicaValue(rows)
		if err != nil {
			return nil, err
		}
		replicas = append(replicas, replica)
	}
	return replicas, rows.Err()
}

func (s *Store) SetReplicaProviderIdentity(ctx context.Context, id, requestID, resourceID string) error {
	request, resource := null(requestID), null(resourceID)
	result, err := s.ExecContext(ctx, `UPDATE replicas SET provider_request_id=COALESCE(?,provider_request_id),provider_resource_id=COALESCE(provider_resource_id,?),lifecycle_state=CASE WHEN lifecycle_state='pending' THEN 'provisioning' ELSE lifecycle_state END,updated_at=? WHERE id=? AND lifecycle_state!='deleted' AND (? IS NULL OR provider_resource_id IS NULL OR provider_resource_id=?)`, request, resource, now(), id, resource, resource)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: replica identity changed or replica is deleted", ErrConflict)
	}
	return nil
}

func (s *Store) ObserveReplica(ctx context.Context, id, lifecycle, endpoint, health, details string, observed time.Time) error {
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	if details == "" {
		details = "{}"
	}
	result, err := s.ExecContext(ctx, `UPDATE replicas SET lifecycle_state=?,endpoint=?,health=?,provider_details_json=?::jsonb,last_observed_at=?,updated_at=? WHERE id=? AND lifecycle_state!='deleted'`, lifecycle, null(endpoint), health, details, observed.UTC(), now(), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) MarkReplicaDeleted(ctx context.Context, id string) error {
	result, err := s.ExecContext(ctx, `UPDATE replicas SET lifecycle_state='deleted',endpoint=NULL,health='unknown',last_observed_at=?,updated_at=? WHERE id=?`, time.Now().UTC(), now(), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type replicaScanner interface{ Scan(...any) error }

func scanReplica(row *sql.Row) (domain.Replica, error) {
	replica, err := scanReplicaValue(row)
	if errors.Is(err, sql.ErrNoRows) {
		return replica, ErrNotFound
	}
	return replica, err
}
func scanReplicaValue(row replicaScanner) (domain.Replica, error) {
	var replica domain.Replica
	var observed sql.NullTime
	var created, updated string
	err := row.Scan(&replica.ID, &replica.TenantID, &replica.DeploymentID, &replica.RevisionID, &replica.Ordinal, &replica.ExternalKey, &replica.LifecycleState, &replica.Provider, &replica.ProviderRequestID, &replica.ProviderResourceID, &replica.Endpoint, &replica.Health, &replica.ProviderDetails, &observed, &created, &updated)
	if err != nil {
		return replica, err
	}
	replica.CreatedAt, replica.UpdatedAt = parseTime(created), parseTime(updated)
	if observed.Valid {
		stamp := observed.Time.UTC()
		replica.LastObservedAt = &stamp
	}
	return replica, nil
}
