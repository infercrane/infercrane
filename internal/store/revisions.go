package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/support"
)

const revisionColumns = `r.id,r.deployment_id,r.revision_number,r.status,r.spec_json::text,COALESCE(r.source_revision_id,''),r.reason,r.created_at,r.activated_at,r.completed_at`

func (s *Store) CreateCandidateRevision(ctx context.Context, tenant, deploymentName, specJSON string) (domain.DeploymentRevision, error) {
	return s.EnsureCandidateRevision(ctx, tenant, deploymentName, specJSON, "")
}

// EnsureCandidateRevision makes candidate creation replay-safe for a durable
// operation. Replaying the same operation returns the revision it created.
func (s *Store) EnsureCandidateRevision(ctx context.Context, tenant, deploymentName, specJSON, operationID string) (domain.DeploymentRevision, error) {
	if tenant == "" {
		tenant = "global"
	}
	var normalized domain.DeploymentRevisionSpec
	decoder := json.NewDecoder(bytes.NewReader([]byte(specJSON)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&normalized); err != nil || normalized.Model == "" {
		return domain.DeploymentRevision{}, errors.New("revision spec must be a valid deployment spec")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return domain.DeploymentRevision{}, errors.New("revision spec must contain one JSON object")
	}
	if normalized.Runtime == "" {
		normalized.Runtime = "vllm"
	}
	if normalized.RuntimeVersion == "" {
		if normalized.Runtime == support.DefaultRuntime {
			normalized.RuntimeVersion = support.DefaultRuntimeVersion
		} else if normalized.Runtime == "sglang" {
			normalized.RuntimeVersion = support.SGLangRuntimeVersion
		}
	}
	normalized.Workload = support.NormalizeWorkload(normalized.Runtime, normalized.Workload)
	normalized.Serving = normalized.Serving.Normalize()
	if normalized.ComputeMode == "" {
		if normalized.Cloud != "" || normalized.GPU != "" {
			normalized.ComputeMode = "elastic"
		} else {
			normalized.ComputeMode = "existing"
		}
	}
	if normalized.ComputeMode != "existing" && normalized.ComputeMode != "elastic" && normalized.ComputeMode != "serverless" {
		return domain.DeploymentRevision{}, errors.New("revision compute mode is unsupported")
	}
	if normalized.ComputeMode == "elastic" && (normalized.Cloud == "" || normalized.GPU == "") {
		return domain.DeploymentRevision{}, errors.New("elastic revision requires cloud and gpu")
	}
	if normalized.ComputeMode != "existing" {
		if err := support.V1().Validate(normalized.Runtime, normalized.Cloud, normalized.ComputeMode); err != nil {
			return domain.DeploymentRevision{}, fmt.Errorf("support policy: %w", err)
		}
	}
	if normalized.Runtime == "custom-oci" && normalized.Workload.Empty() {
		return domain.DeploymentRevision{}, errors.New("custom-oci revision requires an explicit workload contract")
	}
	if !normalized.Workload.Empty() {
		if err := normalized.Workload.Validate(); err != nil {
			return domain.DeploymentRevision{}, fmt.Errorf("runtime workload: %w", err)
		}
		if normalized.ComputeMode != "elastic" {
			return domain.DeploymentRevision{}, errors.New("custom OCI workload requires elastic compute")
		}
		normalized.Port = normalized.Workload.Port
	}
	if normalized.RoutingStrategy == "" {
		normalized.RoutingStrategy = "round-robin"
	}
	if _, ok := domain.RoutingStrategies[normalized.RoutingStrategy]; !ok {
		return domain.DeploymentRevision{}, errors.New("revision routing strategy is unsupported")
	}
	if normalized.MinReplicas == 0 {
		normalized.MinReplicas = 1
	}
	if normalized.MaxReplicas == 0 {
		normalized.MaxReplicas = normalized.MinReplicas
	}
	if normalized.MinReplicas < 1 || normalized.MaxReplicas < normalized.MinReplicas {
		return domain.DeploymentRevision{}, errors.New("revision replica bounds are invalid")
	}
	if err := normalized.Serving.Validate(normalized.Runtime, normalized.Cloud, normalized.ProviderAdapter, normalized.MinReplicas, normalized.MaxReplicas); err != nil {
		return domain.DeploymentRevision{}, fmt.Errorf("serving topology: %w", err)
	}
	if normalized.Runtime != support.DefaultRuntime && normalized.MaxReplicas > normalized.MinReplicas {
		return domain.DeploymentRevision{}, errors.New("autoscaling is not yet qualified for this runtime; set min and max replicas equal")
	}
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return domain.DeploymentRevision{}, err
	}
	specJSON = string(canonical)
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.DeploymentRevision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if operationID != "" {
		row := tx.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM deployment_revisions r JOIN deployments d ON d.id=r.deployment_id WHERE d.tenant_id=? AND d.name=? AND r.created_by_operation_id=?`, tenant, deploymentName, operationID)
		existing, lookupErr := scanRevision(row)
		if lookupErr == nil {
			if !sameJSON(existing.SpecJSON, specJSON) {
				return domain.DeploymentRevision{}, fmt.Errorf("%w: operation already created a different revision", ErrConflict)
			}
			return existing, nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return domain.DeploymentRevision{}, lookupErr
		}
	}
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_revisions(id,deployment_id,revision_number,status,spec_json,source_revision_id,created_at,created_by_operation_id) VALUES(?,?,?,'candidate',?::jsonb,?,?,?)`, id, deploymentID, number, specJSON, activeID, stamp, null(operationID)); err != nil {
		return domain.DeploymentRevision{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployments SET candidate_revision_id=?,updated_at=? WHERE id=?`, id, stamp, deploymentID); err != nil {
		return domain.DeploymentRevision{}, err
	}
	if err = insertRevisionEvent(ctx, tx, deploymentID, "revision_candidate_created", "Candidate revision created", map[string]any{"revision_id": id, "revision_number": number}, stamp); err != nil {
		return domain.DeploymentRevision{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.DeploymentRevision{}, err
	}
	return domain.DeploymentRevision{ID: id, DeploymentID: deploymentID, Number: number, Status: "candidate", SpecJSON: specJSON, SourceRevisionID: activeID, CreatedAt: parseTime(stamp)}, nil
}

func sameJSON(left, right string) bool {
	var a, b any
	return json.Unmarshal([]byte(left), &a) == nil && json.Unmarshal([]byte(right), &b) == nil && reflect.DeepEqual(a, b)
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

func (s *Store) Revision(ctx context.Context, tenant, deploymentName, revisionID string) (domain.DeploymentRevision, error) {
	if tenant == "" {
		tenant = "global"
	}
	revision, err := scanRevision(s.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM deployment_revisions r JOIN deployments d ON d.id=r.deployment_id WHERE d.tenant_id=? AND d.name=? AND r.id=?`, tenant, deploymentName, revisionID))
	if errors.Is(err, sql.ErrNoRows) {
		return revision, ErrNotFound
	}
	return revision, err
}

func (s *Store) PromoteCandidateRevision(ctx context.Context, tenant, deploymentName, candidateID string) error {
	return s.transitionRevision(ctx, tenant, deploymentName, candidateID, "active", "")
}

func (s *Store) PreviousRevisionID(ctx context.Context, tenant, deploymentName, activeRevisionID string) (string, error) {
	if tenant == "" {
		tenant = "global"
	}
	var id string
	err := s.QueryRowContext(ctx, `SELECT r.id FROM deployment_revisions r JOIN deployments d ON d.id=r.deployment_id WHERE d.tenant_id=? AND d.name=? AND r.id!=? AND r.status='superseded' ORDER BY r.completed_at DESC NULLS LAST,r.revision_number DESC LIMIT 1`, tenant, deploymentName, activeRevisionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// PromoteGuardedCandidate atomically commits a guard-accepted candidate and
// its isolated healthy target set. Provider cleanup happens only after the
// reconciler publishes the resulting router generation.
func (s *Store) PromoteGuardedCandidate(ctx context.Context, tenant, deploymentName, candidateID string, targetNames []string) error {
	if tenant == "" {
		tenant = "global"
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var deploymentID, activeID string
	var currentCandidate sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT id,active_revision_id,candidate_revision_id FROM deployments WHERE tenant_id=? AND name=? AND desired_state!='deleted' FOR UPDATE`, tenant, deploymentName).Scan(&deploymentID, &activeID, &currentCandidate); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if activeID == candidateID && !currentCandidate.Valid {
		return nil
	}
	if !currentCandidate.Valid || currentCandidate.String != candidateID {
		return fmt.Errorf("%w: revision is not the current candidate", ErrConflict)
	}
	var decision, evaluatedActive string
	if err = tx.QueryRowContext(ctx, `SELECT decision,active_revision_id FROM release_guard_evaluations WHERE deployment_id=? AND candidate_revision_id=? ORDER BY created_at DESC LIMIT 1`, deploymentID, candidateID).Scan(&decision, &evaluatedActive); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: candidate has no Release Guard evaluation", ErrConflict)
	} else if err != nil {
		return err
	}
	if decision != "ACCEPT" || evaluatedActive != activeID {
		return fmt.Errorf("%w: Release Guard has not accepted candidate against current active revision", ErrConflict)
	}
	var specJSON string
	if err = tx.QueryRowContext(ctx, `SELECT spec_json::text FROM deployment_revisions WHERE id=? AND deployment_id=? AND status='candidate' FOR UPDATE`, candidateID, deploymentID).Scan(&specJSON); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: candidate revision is unavailable", ErrConflict)
	} else if err != nil {
		return err
	}
	var spec domain.DeploymentRevisionSpec
	if err = json.Unmarshal([]byte(specJSON), &spec); err != nil {
		return err
	}
	deployment := domain.Deployment{Name: deploymentName, Model: spec.Model, Runtime: spec.Runtime, RoutingStrategy: spec.RoutingStrategy, MinReplicas: spec.MinReplicas, MaxReplicas: spec.MaxReplicas, AutoscalingEnabled: spec.AutoscalingEnabled}
	targetIDs, err := validateTargetSet(ctx, tx, tenant, deployment, targetNames)
	if err != nil {
		return err
	}
	if len(targetIDs) < spec.MinReplicas {
		return fmt.Errorf("%w: candidate has fewer targets than minimum replicas", ErrConflict)
	}
	for _, targetID := range targetIDs {
		var ready bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM targets t JOIN replicas r ON r.tenant_id=t.tenant_id AND r.endpoint=t.url WHERE t.id=? AND r.deployment_id=? AND r.revision_id=? AND r.lifecycle_state='ready' AND r.health='healthy')`, targetID, deploymentID, candidateID).Scan(&ready); err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("%w: candidate target is not backed by a healthy ready replica", ErrConflict)
		}
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET status='superseded',completed_at=? WHERE id=?`, stamp, activeID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET status='active',activated_at=?,completed_at=NULL WHERE id=? AND status='candidate'`, stamp, candidateID); err != nil {
		return err
	}
	if err = updateDeploymentFromRevision(ctx, tx, deploymentID, candidateID, stamp); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployments SET observed_state='pending' WHERE id=?`, deploymentID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM deployment_targets WHERE deployment_id=?`, deploymentID); err != nil {
		return err
	}
	for _, targetID := range targetIDs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_targets(deployment_id,target_id) VALUES(?,?)`, deploymentID, targetID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE replicas SET lifecycle_state=CASE WHEN revision_id=? THEN 'active' WHEN lifecycle_state='active' THEN 'draining' ELSE lifecycle_state END,updated_at=? WHERE deployment_id=? AND lifecycle_state!='deleted'`, candidateID, stamp, deploymentID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE scaling_policies SET enabled=?,min_replicas=?,max_replicas=?,updated_at=? WHERE deployment_id=?`, spec.AutoscalingEnabled, spec.MinReplicas, spec.MaxReplicas, stamp, deploymentID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE autoscaling_state SET desired_replicas=?,consecutive_high=0,consecutive_low=0,updated_at=? WHERE deployment_id=?`, spec.MinReplicas, stamp, deploymentID); err != nil {
		return err
	}
	if err = insertRevisionEvent(ctx, tx, deploymentID, "revision_promoted", "Guard-accepted candidate revision committed", map[string]any{"revision_id": candidateID, "previous_revision_id": activeID, "targets": targetNames}, stamp); err != nil {
		return err
	}
	return tx.Commit()
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
	if revisionID == activeID {
		return nil
	}
	if status == "candidate" {
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
	if err = insertRevisionEvent(ctx, tx, deploymentID, "revision_rolled_back", "Deployment rolled back to revision", map[string]any{"revision_id": revisionID, "previous_revision_id": activeID, "reason": reason}, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

// RollbackGuardedPromotion atomically restores a retained historical revision,
// its target set, and replica lifecycle. It is used only by a persisted
// post-promotion monitor while the old capacity is deliberately retained.
func (s *Store) RollbackGuardedPromotion(ctx context.Context, tenant, deploymentName, promotedRevisionID, rollbackRevisionID, reason string, targetNames []string) error {
	if tenant == "" {
		tenant = "global"
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var deploymentID, activeID string
	if err = tx.QueryRowContext(ctx, `SELECT id,active_revision_id FROM deployments WHERE tenant_id=? AND name=? AND desired_state!='deleted' FOR UPDATE`, tenant, deploymentName).Scan(&deploymentID, &activeID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if activeID == rollbackRevisionID {
		return nil
	}
	if activeID != promotedRevisionID {
		return fmt.Errorf("%w: monitored promoted revision is no longer active", ErrConflict)
	}
	var specJSON string
	if err = tx.QueryRowContext(ctx, `SELECT spec_json::text FROM deployment_revisions WHERE id=? AND deployment_id=? AND status='superseded' FOR UPDATE`, rollbackRevisionID, deploymentID).Scan(&specJSON); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: rollback revision is unavailable", ErrConflict)
	} else if err != nil {
		return err
	}
	var spec domain.DeploymentRevisionSpec
	if err = json.Unmarshal([]byte(specJSON), &spec); err != nil {
		return err
	}
	deployment := domain.Deployment{Name: deploymentName, Model: spec.Model, Runtime: spec.Runtime, RoutingStrategy: spec.RoutingStrategy, MinReplicas: spec.MinReplicas, MaxReplicas: spec.MaxReplicas, AutoscalingEnabled: spec.AutoscalingEnabled}
	targetIDs, err := validateTargetSet(ctx, tx, tenant, deployment, targetNames)
	if err != nil {
		return err
	}
	if len(targetIDs) < spec.MinReplicas {
		return fmt.Errorf("%w: rollback revision has fewer retained targets than minimum replicas", ErrConflict)
	}
	for _, targetID := range targetIDs {
		var ready bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM targets t JOIN replicas r ON r.tenant_id=t.tenant_id AND r.endpoint=t.url WHERE t.id=? AND r.deployment_id=? AND r.revision_id=? AND r.lifecycle_state IN ('ready','active','draining') AND r.health='healthy')`, targetID, deploymentID, rollbackRevisionID).Scan(&ready); err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("%w: retained rollback target is not healthy", ErrConflict)
		}
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET status='superseded',completed_at=? WHERE id=?`, stamp, promotedRevisionID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET status='active',reason=?,activated_at=?,completed_at=NULL WHERE id=?`, reason, stamp, rollbackRevisionID); err != nil {
		return err
	}
	if err = updateDeploymentFromRevision(ctx, tx, deploymentID, rollbackRevisionID, stamp); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM deployment_targets WHERE deployment_id=?`, deploymentID); err != nil {
		return err
	}
	for _, targetID := range targetIDs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_targets(deployment_id,target_id) VALUES(?,?)`, deploymentID, targetID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE replicas SET lifecycle_state=CASE WHEN revision_id=? THEN 'active' WHEN revision_id=? AND lifecycle_state!='deleted' THEN 'draining' ELSE lifecycle_state END,updated_at=? WHERE deployment_id=?`, rollbackRevisionID, promotedRevisionID, stamp, deploymentID); err != nil {
		return err
	}
	if err = insertRevisionEvent(ctx, tx, deploymentID, "revision_auto_rolled_back", "Release Guard restored retained revision", map[string]any{"revision_id": rollbackRevisionID, "failed_revision_id": promotedRevisionID, "reason": reason, "targets": targetNames}, stamp); err != nil {
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
		var status string
		if err = tx.QueryRowContext(ctx, `SELECT status FROM deployment_revisions WHERE id=? AND deployment_id=?`, candidateID, deploymentID).Scan(&status); err == nil {
			if (nextStatus == "active" && activeID == candidateID && status == "active") || (nextStatus == "rejected" && status == "rejected") {
				return nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
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
		if err = insertRevisionEvent(ctx, tx, deploymentID, "revision_rejected", "Candidate revision rejected", map[string]any{"revision_id": candidateID, "reason": reason}, stamp); err != nil {
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
	if err = insertRevisionEvent(ctx, tx, deploymentID, "revision_promoted", "Candidate revision promoted", map[string]any{"revision_id": candidateID, "previous_revision_id": activeID}, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func insertRevisionEvent(ctx context.Context, tx *tx, deploymentID, eventType, summary string, payload any, stamp string) error {
	id, err := newID()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO deployment_events(id,deployment_id,event_type,summary,payload_json,created_at) VALUES(?,?,?,?,?::jsonb,?)`, id, deploymentID, eventType, summary, string(encoded), stamp)
	return err
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
