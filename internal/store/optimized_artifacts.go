package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizedartifact"
)

func (s *Store) CreateOptimizedArtifact(ctx context.Context, tenant, idempotencyKey string, plan optimizedartifact.Plan) (domain.OptimizedArtifact, bool, error) {
	if tenant == "" || idempotencyKey == "" || len(idempotencyKey) > 128 {
		return domain.OptimizedArtifact{}, false, errors.New("tenant and bounded idempotency key are required")
	}
	inputDigest, err := optimizedartifact.InputDigest(plan)
	if err != nil {
		return domain.OptimizedArtifact{}, false, err
	}
	configuration, _ := json.Marshal(plan.Configuration)
	hardware, _ := json.Marshal(plan.HardwareConstraints)
	row := domain.OptimizedArtifact{TenantID: tenant, IdempotencyKey: idempotencyKey, InputDigest: inputDigest, BaseModelArtifactID: plan.BaseModelArtifactID, Kind: plan.Kind, Format: plan.Format, Tool: plan.Tool, ToolVersion: plan.ToolVersion, Algorithm: plan.Algorithm, BuilderImageDigest: plan.BuilderImageDigest, CalibrationDigest: plan.CalibrationDigest, LicenseSPDX: plan.LicenseSPDX, ConfigurationJSON: string(configuration), HardwareConstraintsJSON: string(hardware), RequiresQualityReview: plan.RequiresQualityReview, State: optimizedartifact.StatePlanned, EvidenceState: "unmeasured", BuildEvidenceJSON: `{}`}
	row.ID, err = newID()
	if err != nil {
		return row, false, err
	}
	stamp := now()
	result, err := s.ExecContext(ctx, `INSERT INTO optimized_artifacts(id,tenant_id,idempotency_key,input_digest,base_model_artifact_id,kind,format,tool,tool_version,algorithm,builder_image_digest,calibration_digest,license_spdx,configuration_json,hardware_constraints_json,requires_quality_review,state,evidence_state,build_evidence_json,created_at,updated_at) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?::jsonb,?::jsonb,?,?,?,?::jsonb,?,? WHERE EXISTS(SELECT 1 FROM model_artifacts WHERE tenant_id=? AND id=?) ON CONFLICT(tenant_id,idempotency_key) DO NOTHING`, row.ID, tenant, idempotencyKey, inputDigest, plan.BaseModelArtifactID, plan.Kind, plan.Format, plan.Tool, plan.ToolVersion, plan.Algorithm, plan.BuilderImageDigest, null(plan.CalibrationDigest), plan.LicenseSPDX, row.ConfigurationJSON, row.HardwareConstraintsJSON, plan.RequiresQualityReview, row.State, row.EvidenceState, row.BuildEvidenceJSON, stamp, stamp, tenant, plan.BaseModelArtifactID)
	if err != nil {
		return row, false, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		existing, lookupErr := s.OptimizedArtifactByIdempotencyKey(ctx, tenant, idempotencyKey)
		if errors.Is(lookupErr, domain.ErrNotFound) {
			return row, false, fmt.Errorf("%w: base model artifact", domain.ErrNotFound)
		}
		if lookupErr != nil {
			return row, false, lookupErr
		}
		if existing.InputDigest != inputDigest {
			return existing, false, domain.ErrConflict
		}
		return existing, false, nil
	}
	return s.OptimizedArtifact(ctx, tenant, row.ID)
}

func (s *Store) OptimizedArtifact(ctx context.Context, tenant, id string) (domain.OptimizedArtifact, bool, error) {
	row, err := s.optimizedArtifact(ctx, `tenant_id=? AND id=?`, tenant, id)
	return row, err == nil, err
}

func (s *Store) OptimizedArtifactByIdempotencyKey(ctx context.Context, tenant, key string) (domain.OptimizedArtifact, error) {
	return s.optimizedArtifact(ctx, `tenant_id=? AND idempotency_key=?`, tenant, key)
}

func (s *Store) optimizedArtifact(ctx context.Context, predicate string, args ...any) (domain.OptimizedArtifact, error) {
	var row domain.OptimizedArtifact
	var calibration, outputRepository, outputRevision, outputDigest, qualityEvidenceID, failure sql.NullString
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,idempotency_key,input_digest,base_model_artifact_id,kind,format,tool,tool_version,algorithm,builder_image_digest,calibration_digest,license_spdx,configuration_json::text,hardware_constraints_json::text,requires_quality_review,state,evidence_state,output_repository,output_immutable_revision,output_digest,build_evidence_json::text,quality_evidence_id,failure_code,created_at,updated_at FROM optimized_artifacts WHERE `+predicate, args...).Scan(&row.ID, &row.TenantID, &row.IdempotencyKey, &row.InputDigest, &row.BaseModelArtifactID, &row.Kind, &row.Format, &row.Tool, &row.ToolVersion, &row.Algorithm, &row.BuilderImageDigest, &calibration, &row.LicenseSPDX, &row.ConfigurationJSON, &row.HardwareConstraintsJSON, &row.RequiresQualityReview, &row.State, &row.EvidenceState, &outputRepository, &outputRevision, &outputDigest, &row.BuildEvidenceJSON, &qualityEvidenceID, &failure, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return row, domain.ErrNotFound
	}
	if err != nil {
		return row, err
	}
	row.CalibrationDigest, row.OutputRepository, row.OutputImmutableRevision, row.OutputDigest = calibration.String, outputRepository.String, outputRevision.String, outputDigest.String
	row.QualityEvidenceID, row.FailureCode = qualityEvidenceID.String, failure.String
	row.CreatedAt, row.UpdatedAt = parseTime(created), parseTime(updated)
	return row, nil
}

func (s *Store) OptimizedArtifacts(ctx context.Context, tenant string, limit int) ([]domain.OptimizedArtifact, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.QueryContext(ctx, `SELECT id FROM optimized_artifacts WHERE tenant_id=? ORDER BY created_at DESC LIMIT ?`, tenant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.OptimizedArtifact
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		row, _, lookupErr := s.OptimizedArtifact(ctx, tenant, id)
		if lookupErr != nil {
			return nil, lookupErr
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Store) BeginOptimizedArtifactBuild(ctx context.Context, tenant, id string) (domain.OptimizedArtifact, error) {
	result, err := s.ExecContext(ctx, `UPDATE optimized_artifacts SET state=?,updated_at=? WHERE tenant_id=? AND id=? AND state=?`, optimizedartifact.StateBuilding, now(), tenant, id, optimizedartifact.StatePlanned)
	if err != nil {
		return domain.OptimizedArtifact{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		row, _, lookupErr := s.OptimizedArtifact(ctx, tenant, id)
		if lookupErr != nil {
			return row, lookupErr
		}
		if row.State == optimizedartifact.StateBuilding {
			return row, nil
		}
		return row, domain.ErrConflict
	}
	row, _, err := s.OptimizedArtifact(ctx, tenant, id)
	return row, err
}

func (s *Store) AttestOptimizedArtifact(ctx context.Context, tenant, id, state string, attestation optimizedartifact.Attestation) (domain.OptimizedArtifact, error) {
	if err := optimizedartifact.ValidateAttestation(state, attestation); err != nil {
		return domain.OptimizedArtifact{}, err
	}
	result, err := s.ExecContext(ctx, `UPDATE optimized_artifacts SET state=?,evidence_state=?,output_repository=?,output_immutable_revision=?,output_digest=?,build_evidence_json=?::jsonb,failure_code=?,updated_at=? WHERE tenant_id=? AND id=? AND state='building'`, state, map[string]string{optimizedartifact.StateReady: "measured", optimizedartifact.StateFailed: "rejected"}[state], null(attestation.OutputRepository), null(attestation.OutputImmutableRevision), null(attestation.OutputDigest), string(attestation.BuildEvidence), null(attestation.FailureCode), now(), tenant, id)
	if err != nil {
		return domain.OptimizedArtifact{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		row, _, lookupErr := s.OptimizedArtifact(ctx, tenant, id)
		if lookupErr != nil {
			return row, lookupErr
		}
		if row.State == state && row.OutputDigest == attestation.OutputDigest && row.FailureCode == attestation.FailureCode && semanticJSONEqual(row.BuildEvidenceJSON, string(attestation.BuildEvidence)) {
			return row, nil
		}
		return row, domain.ErrConflict
	}
	row, _, err := s.OptimizedArtifact(ctx, tenant, id)
	return row, err
}

func (s *Store) QualifyOptimizedArtifact(ctx context.Context, tenant, id, candidateRunID, qualityEvidenceID string) (domain.OptimizedArtifact, error) {
	if candidateRunID == "" || qualityEvidenceID == "" {
		return domain.OptimizedArtifact{}, errors.New("exact candidate run and passing signed quality evidence are required")
	}
	result, err := s.ExecContext(ctx, `UPDATE optimized_artifacts artifact SET evidence_state='qualified',quality_evidence_id=?,updated_at=? FROM revision_quality_evidence evidence, optimization_candidate_runs candidate WHERE artifact.tenant_id=? AND artifact.id=? AND artifact.state='ready' AND artifact.evidence_state='measured' AND evidence.id=? AND evidence.tenant_id=artifact.tenant_id AND evidence.passed=TRUE AND candidate.id=? AND candidate.tenant_id=artifact.tenant_id AND candidate.optimized_artifact_id=artifact.id AND candidate.revision_id=evidence.revision_id`, qualityEvidenceID, now(), tenant, id, qualityEvidenceID, candidateRunID)
	if err != nil {
		return domain.OptimizedArtifact{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		row, _, lookupErr := s.OptimizedArtifact(ctx, tenant, id)
		if lookupErr != nil {
			return row, lookupErr
		}
		if row.EvidenceState == "qualified" && row.QualityEvidenceID == qualityEvidenceID {
			return row, nil
		}
		return row, domain.ErrConflict
	}
	row, _, err := s.OptimizedArtifact(ctx, tenant, id)
	return row, err
}
