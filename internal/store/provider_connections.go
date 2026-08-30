package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/external"
)

// CreateProviderConnection records reusable provider configuration without
// copying credential values into PostgreSQL. Endpoint bindings snapshot their
// own immutable privacy acknowledgement and hard budget policy separately.
func (s *Store) CreateProviderConnection(ctx context.Context, tenant string, item domain.ProviderConnection) (domain.ProviderConnection, error) {
	if tenant == "" || item.Name == "" || item.Adapter == "" || item.TargetID == "" || item.SecretReferenceID == "" {
		return domain.ProviderConnection{}, errors.New("tenant, name, adapter, target, and secret reference are required")
	}
	if !external.SupportedAdapter(item.Adapter) {
		return domain.ProviderConnection{}, fmt.Errorf("unsupported provider adapter %q", item.Adapter)
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ProviderConnection{}, err
	}
	defer tx.Rollback()

	var targetProvider string
	if err = tx.QueryRowContext(ctx, `SELECT provider FROM targets WHERE tenant_id=? AND id=?`, tenant, item.TargetID).Scan(&targetProvider); errors.Is(err, sql.ErrNoRows) {
		return domain.ProviderConnection{}, ErrNotFound
	} else if err != nil {
		return domain.ProviderConnection{}, err
	}
	if targetProvider != item.Adapter {
		return domain.ProviderConnection{}, errors.New("provider connection adapter must match its target provider")
	}
	var secretExists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM secret_references WHERE tenant_id=? AND id=?)`, tenant, item.SecretReferenceID).Scan(&secretExists); err != nil {
		return domain.ProviderConnection{}, err
	}
	if !secretExists {
		return domain.ProviderConnection{}, ErrNotFound
	}

	var existing domain.ProviderConnection
	var created, updated string
	err = tx.QueryRowContext(ctx, `SELECT id,tenant_id,name,adapter,target_id,secret_reference_id,created_at,updated_at FROM provider_connections WHERE tenant_id=? AND name=?`, tenant, item.Name).Scan(
		&existing.ID, &existing.TenantID, &existing.Name, &existing.Adapter, &existing.TargetID, &existing.SecretReferenceID, &created, &updated,
	)
	if err == nil {
		existing.CreatedAt, existing.UpdatedAt = parseTime(created), parseTime(updated)
		if existing.Adapter == item.Adapter && existing.TargetID == item.TargetID && existing.SecretReferenceID == item.SecretReferenceID {
			if err = tx.Commit(); err != nil {
				return domain.ProviderConnection{}, err
			}
			return s.ProviderConnectionForTenant(ctx, tenant, existing.Name)
		}
		return domain.ProviderConnection{}, fmt.Errorf("%w: provider connection name already exists with different configuration", ErrConflict)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.ProviderConnection{}, err
	}
	item.ID, err = newID()
	if err != nil {
		return domain.ProviderConnection{}, err
	}
	stamp := now()
	_, err = tx.ExecContext(ctx, `INSERT INTO provider_connections(id,tenant_id,name,adapter,target_id,secret_reference_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, item.ID, tenant, item.Name, item.Adapter, item.TargetID, item.SecretReferenceID, stamp, stamp)
	if isUniqueViolation(err) {
		return domain.ProviderConnection{}, fmt.Errorf("%w: provider connection already exists", ErrConflict)
	}
	if err != nil {
		return domain.ProviderConnection{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ProviderConnection{}, err
	}
	return s.ProviderConnectionForTenant(ctx, tenant, item.Name)
}

func (s *Store) ProviderConnectionForTenant(ctx context.Context, tenant, name string) (domain.ProviderConnection, error) {
	var item domain.ProviderConnection
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT pc.id,pc.tenant_id,pc.name,pc.adapter,pc.target_id,t.name,pc.secret_reference_id,s.name,pc.created_at,pc.updated_at FROM provider_connections pc JOIN targets t ON t.id=pc.target_id AND t.tenant_id=pc.tenant_id JOIN secret_references s ON s.id=pc.secret_reference_id AND s.tenant_id=pc.tenant_id WHERE pc.tenant_id=? AND pc.name=?`, tenant, name).Scan(
		&item.ID, &item.TenantID, &item.Name, &item.Adapter, &item.TargetID, &item.TargetName, &item.SecretReferenceID, &item.SecretReferenceName, &created, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProviderConnection{}, ErrNotFound
	}
	if err != nil {
		return domain.ProviderConnection{}, err
	}
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	return item, nil
}

func (s *Store) ProviderConnectionsForTenant(ctx context.Context, tenant string) ([]domain.ProviderConnection, error) {
	rows, err := s.QueryContext(ctx, `SELECT pc.id,pc.tenant_id,pc.name,pc.adapter,pc.target_id,t.name,pc.secret_reference_id,s.name,pc.created_at,pc.updated_at FROM provider_connections pc JOIN targets t ON t.id=pc.target_id AND t.tenant_id=pc.tenant_id JOIN secret_references s ON s.id=pc.secret_reference_id AND s.tenant_id=pc.tenant_id WHERE pc.tenant_id=? ORDER BY pc.name`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.ProviderConnection, 0)
	for rows.Next() {
		var item domain.ProviderConnection
		var created, updated string
		if err = rows.Scan(&item.ID, &item.TenantID, &item.Name, &item.Adapter, &item.TargetID, &item.TargetName, &item.SecretReferenceID, &item.SecretReferenceName, &created, &updated); err != nil {
			return nil, err
		}
		item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) DeleteProviderConnectionForTenant(ctx context.Context, tenant, name string) error {
	result, err := s.ExecContext(ctx, `DELETE FROM provider_connections WHERE tenant_id=? AND name=?`, tenant, name)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}
