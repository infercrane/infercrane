package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/qualitycheck"
)

// RecordQualityEvalSet stores a customer eval set under its content digest.
// Uploading the same bytes again returns the existing record.
func (s *Store) RecordQualityEvalSet(ctx context.Context, tenant, actor string, body []byte) (domain.QualityEvalSet, bool, error) {
	if tenant == "" {
		tenant = "global"
	}
	set, err := qualitycheck.DecodeEvalSet(body)
	if err != nil {
		return domain.QualityEvalSet{}, false, err
	}
	id, err := newID()
	if err != nil {
		return domain.QualityEvalSet{}, false, err
	}
	result, err := s.ExecContext(ctx, `INSERT INTO quality_eval_sets(id,tenant_id,digest,name,version,prompt_count,body,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,digest) DO NOTHING`, id, tenant, set.Digest, set.Name, set.Version, len(set.Prompts), string(body), actor, now())
	if err != nil {
		return domain.QualityEvalSet{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return domain.QualityEvalSet{}, false, err
	}
	row, err := s.QualityEvalSet(ctx, tenant, set.Digest)
	return row, affected == 1, err
}

// QualityEvalSet returns one tenant's eval set, including its body.
func (s *Store) QualityEvalSet(ctx context.Context, tenant, digest string) (domain.QualityEvalSet, error) {
	if tenant == "" {
		tenant = "global"
	}
	var row domain.QualityEvalSet
	var created string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,digest,name,version,prompt_count,body,created_by,created_at FROM quality_eval_sets WHERE tenant_id=? AND digest=?`, tenant, digest).Scan(&row.ID, &row.TenantID, &row.Digest, &row.Name, &row.Version, &row.PromptCount, &row.Body, &row.CreatedBy, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return row, domain.ErrNotFound
	}
	if err != nil {
		return row, err
	}
	row.CreatedAt = parseTime(created)
	return row, nil
}

// DeleteQualityEvalSet removes a tenant's eval set. A campaign that names it
// then waits until it is uploaded again.
func (s *Store) DeleteQualityEvalSet(ctx context.Context, tenant, digest string) error {
	if tenant == "" {
		tenant = "global"
	}
	result, err := s.ExecContext(ctx, `DELETE FROM quality_eval_sets WHERE tenant_id=? AND digest=?`, tenant, digest)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return domain.ErrNotFound
	}
	return nil
}
