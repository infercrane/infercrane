package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
)

const revisionColumns = `r.id,r.deployment_id,r.revision_number,r.status,r.spec_json::text,COALESCE(r.source_revision_id,''),r.reason,r.created_at,r.activated_at,r.completed_at`

func (s *Store) CreateCandidateRevision(ctx context.Context, tenant, deploymentName, specJSON string) (domain.DeploymentRevision, error) {
	if tenant == "" {
		tenant = "global"
	}
	var normalized map[string]any
	if err := json.Unmarshal([]byte(specJSON), &normalized); err != nil || normalized == nil {
		return domain.DeploymentRevision{}, errors.New("revision spec must be a JSON object")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.DeploymentRevision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var deploymentID, activeID string
	var candidate sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT id,active_revision_id,candidate_revision_id FROM deployments WHERE tenant_id=? AND name=? AND desired_state!='deleted' FOR UPDATE`, tenant, deploymentName).Scan(&deploymentID, &activeID, &candidate); errors.Is(err, sql.ErrNoRows) {
		return domain.DeploymentRevision{}, ErrNotFound
	} else if err != nil {
		return domain.DeploymentRevision{}, err
	}
	if candidate.Valid {
		return domain.DeploymentRevision{}, fmt.Errorf("%w: deployment already has a candidate revision", ErrConflict)
	}
	var number int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision_number),0)+1 FROM deployment_revisions WHERE deployment_id=?`, deploymentID).Scan(&number); err != nil {
		return domain.DeploymentRevision{}, err
	}
	id, err := newID()
	if err != nil {
		return domain.DeploymentRevision{}, err
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_revisions(id,deployment_id,revision_number,status,spec_json,source_revision_id,created_at) VALUES(?,?,?,'candidate',?::jsonb,?,?)`, id, deploymentID, number, specJSON, activeID, stamp); err != nil {
		return domain.DeploymentRevision{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployments SET candidate_revision_id=?,updated_at=? WHERE id=?`, id, stamp, deploymentID); err != nil {
		return domain.DeploymentRevision{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.DeploymentRevision{}, err
	}
	return domain.DeploymentRevision{ID: id, DeploymentID: deploymentID, Number: number, Status: "candidate", SpecJSON: specJSON, SourceRevisionID: activeID, CreatedAt: parseTime(stamp)}, nil
}

func (s *Store) Revisions(ctx context.Context, tenant, deploymentName string) ([]domain.DeploymentRevision, error) {
	if tenant == "" {
		tenant = "global"
	}
	rows, err := s.QueryContext(ctx, `SELECT `+revisionColumns+` FROM deployment_revisions r JOIN deployments d ON d.id=r.deployment_id WHERE d.tenant_id=? AND d.name=? ORDER BY r.revision_number DESC`, tenant, deploymentName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []domain.DeploymentRevision
	for rows.Next() {
		revision, scanErr := scanRevision(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

func (s *Store) PromoteCandidateRevision(ctx context.Context, tenant, deploymentName, candidateID string) error {
	return s.transitionRevision(ctx, tenant, deploymentName, candidateID, "active", "")
}

func (s *Store) RejectCandidateRevision(ctx context.Context, tenant, deploymentName, candidateID, reason string) error {
	return s.transitionRevision(ctx, tenant, deploymentName, candidateID, "rejected", reason)
}

func (s *Store) RollbackRevision(ctx context.Context, tenant, deploymentName, revisionID, reason string) error {
	if tenant == "" {
		tenant = "global"
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var deploymentID, activeID, status string
	if err = tx.QueryRowContext(ctx, `SELECT id,active_revision_id FROM deployments WHERE tenant_id=? AND name=? AND desired_state!='deleted' FOR UPDATE`, tenant, deploymentName).Scan(&deploymentID, &activeID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT status FROM deployment_revisions WHERE id=? AND deployment_id=? FOR UPDATE`, revisionID, deploymentID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if revisionID == activeID || status == "candidate" {
		return fmt.Errorf("%w: rollback target must be a non-candidate historical revision", ErrConflict)
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET status='superseded',completed_at=? WHERE id=?`, stamp, activeID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET status='active',reason=?,activated_at=?,completed_at=NULL WHERE id=?`, reason, stamp, revisionID); err != nil {
		return err
	}
	if err = updateDeploymentFromRevision(ctx, tx, deploymentID, revisionID, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) transitionRevision(ctx context.Context, tenant, deploymentName, candidateID, nextStatus, reason string) error {
	if tenant == "" {
		tenant = "global"
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var deploymentID, activeID string
	var candidate sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT id,active_revision_id,candidate_revision_id FROM deployments WHERE tenant_id=? AND name=? AND desired_state!='deleted' FOR UPDATE`, tenant, deploymentName).Scan(&deploymentID, &activeID, &candidate); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if !candidate.Valid || candidate.String != candidateID {
		return fmt.Errorf("%w: revision is not the current candidate", ErrConflict)
	}
	stamp := now()
	if nextStatus == "rejected" {
		if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET status='rejected',reason=?,completed_at=? WHERE id=? AND status='candidate'`, reason, stamp, candidateID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE deployments SET candidate_revision_id=NULL,updated_at=? WHERE id=?`, stamp, deploymentID); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET status='superseded',completed_at=? WHERE id=?`, stamp, activeID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET status='active',activated_at=?,completed_at=NULL WHERE id=? AND status='candidate'`, stamp, candidateID); err != nil {
		return err
	}
	if err = updateDeploymentFromRevision(ctx, tx, deploymentID, candidateID, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func updateDeploymentFromRevision(ctx context.Context, tx *tx, deploymentID, revisionID, stamp string) error {
	_, err := tx.ExecContext(ctx, `UPDATE deployments d SET active_revision_id=?,candidate_revision_id=NULL,model=r.spec_json->>'model',runtime=COALESCE(r.spec_json->>'runtime','vllm'),routing_strategy=COALESCE(r.spec_json->>'routing_strategy','round-robin'),min_replicas=COALESCE((r.spec_json->>'min_replicas')::integer,1),max_replicas=COALESCE((r.spec_json->>'max_replicas')::integer,1),autoscaling_enabled=COALESCE((r.spec_json->>'autoscaling_enabled')::boolean,FALSE),updated_at=? FROM deployment_revisions r WHERE d.id=? AND r.id=? AND r.deployment_id=d.id`, revisionID, stamp, deploymentID, revisionID)
	return err
}

type revisionScanner interface{ Scan(...any) error }

func scanRevision(row revisionScanner) (domain.DeploymentRevision, error) {
	var revision domain.DeploymentRevision
	var created string
	var activated, completed sql.NullTime
	err := row.Scan(&revision.ID, &revision.DeploymentID, &revision.Number, &revision.Status, &revision.SpecJSON, &revision.SourceRevisionID, &revision.Reason, &created, &activated, &completed)
	if err != nil {
		return revision, err
	}
	revision.CreatedAt = parseTime(created)
	if activated.Valid {
		stamp := activated.Time.UTC()
		revision.ActivatedAt = &stamp
	}
	if completed.Valid {
		stamp := completed.Time.UTC()
		revision.CompletedAt = &stamp
	}
	return revision, nil
}
