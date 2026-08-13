package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secrets"
)

func (s *Store) CreateSecretReference(ctx context.Context, tenant, name, resolver, reference string) (domain.SecretReference, error) {
	if tenant == "" || name == "" || resolver == "" || reference == "" {
		return domain.SecretReference{}, errors.New("tenant, name, resolver, and reference are required")
	}
	if resolver != "env" {
		return domain.SecretReference{}, fmt.Errorf("unsupported secret resolver %q", resolver)
	}
	if err := secrets.ValidateReference(domain.SecretReference{Resolver: resolver, Reference: reference}); err != nil {
		return domain.SecretReference{}, err
	}
	id, err := newID()
	if err != nil {
		return domain.SecretReference{}, err
	}
	stamp := now()
	_, err = s.ExecContext(ctx, `INSERT INTO secret_references(id,tenant_id,name,resolver,reference,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, tenant, name, resolver, reference, stamp, stamp)
	if isUniqueViolation(err) {
		return domain.SecretReference{}, fmt.Errorf("%w: secret reference name already exists", ErrConflict)
	}
	if err != nil {
		return domain.SecretReference{}, err
	}
	return domain.SecretReference{ID: id, TenantID: tenant, Name: name, Resolver: resolver, Reference: reference, CreatedAt: parseTime(stamp), UpdatedAt: parseTime(stamp)}, nil
}

func (s *Store) SecretReferenceForTenant(ctx context.Context, tenant, id string) (domain.SecretReference, error) {
	var out domain.SecretReference
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,name,resolver,reference,created_at,updated_at FROM secret_references WHERE tenant_id=? AND id=?`, tenant, id).Scan(&out.ID, &out.TenantID, &out.Name, &out.Resolver, &out.Reference, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SecretReference{}, ErrNotFound
	}
	if err != nil {
		return domain.SecretReference{}, err
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	return out, nil
}

func (s *Store) SecretReferencesForTenant(ctx context.Context, tenant string) ([]domain.SecretReference, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,name,resolver,reference,created_at,updated_at FROM secret_references WHERE tenant_id=? ORDER BY name`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.SecretReference, 0)
	for rows.Next() {
		var item domain.SecretReference
		var created, updated string
		if err := rows.Scan(&item.ID, &item.TenantID, &item.Name, &item.Resolver, &item.Reference, &created, &updated); err != nil {
			return nil, err
		}
		item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSecretReferenceForTenant(ctx context.Context, tenant, id string) error {
	result, err := s.ExecContext(ctx, `DELETE FROM secret_references WHERE tenant_id=? AND id=?`, tenant, id)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}
