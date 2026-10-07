package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

const computeConnectionColumns = `id,tenant_id,name,provider,adapter,status,credential_ciphertext,credential_nonce,credential_key_reference,verified_at,created_at,updated_at`

func (s *Store) CreateComputeConnection(ctx context.Context, tenant string, item domain.ComputeConnection) (domain.ComputeConnection, error) {
	if tenant == "" || item.Name == "" || item.Provider == "" || item.Adapter == "" || item.Status != "verified" || len(item.CredentialCiphertext) == 0 || len(item.CredentialNonce) == 0 || item.CredentialKeyReference == "" || item.VerifiedAt.IsZero() {
		return domain.ComputeConnection{}, errors.New("verified compute connection and encrypted credential are required")
	}
	item.ID, item.TenantID = "", tenant
	var err error
	item.ID, err = newID()
	if err != nil {
		return domain.ComputeConnection{}, err
	}
	stamp := now()
	item.CreatedAt, item.UpdatedAt = parseTime(stamp), parseTime(stamp)
	_, err = s.ExecContext(ctx, `INSERT INTO compute_connections(`+computeConnectionColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, item.ID, tenant, item.Name, item.Provider, item.Adapter, item.Status, item.CredentialCiphertext, item.CredentialNonce, item.CredentialKeyReference, item.VerifiedAt.UTC(), stamp, stamp)
	if isUniqueViolation(err) {
		return domain.ComputeConnection{}, fmt.Errorf("%w: compute connection name already exists", ErrConflict)
	}
	if err != nil {
		return domain.ComputeConnection{}, err
	}
	return s.ComputeConnectionForTenant(ctx, tenant, item.ID)
}

func (s *Store) ComputeConnectionForTenant(ctx context.Context, tenant, id string) (domain.ComputeConnection, error) {
	return scanComputeConnection(s.QueryRowContext(ctx, `SELECT `+computeConnectionColumns+` FROM compute_connections WHERE tenant_id=? AND id=?`, tenant, id))
}

func (s *Store) ComputeConnectionsForTenant(ctx context.Context, tenant string) ([]domain.ComputeConnection, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+computeConnectionColumns+` FROM compute_connections WHERE tenant_id=? ORDER BY name`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.ComputeConnection, 0)
	for rows.Next() {
		item, scanErr := scanComputeConnection(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) DeleteComputeConnectionForTenant(ctx context.Context, tenant, id string) error {
	result, err := s.ExecContext(ctx, `DELETE FROM compute_connections WHERE tenant_id=? AND id=?`, tenant, id)
	if isForeignKeyViolation(err) {
		return fmt.Errorf("%w: compute connection is still attached to a deployment", ErrConflict)
	}
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ComputeConnectionForDeployment(ctx context.Context, tenant, deploymentID string) (domain.ComputeConnection, error) {
	return scanComputeConnection(s.QueryRowContext(ctx, `SELECT `+prefixedComputeConnectionColumns("c")+` FROM deployment_compute_connections d JOIN compute_connections c ON c.tenant_id=d.tenant_id AND c.id=d.compute_connection_id WHERE d.tenant_id=? AND d.deployment_id=?`, tenant, deploymentID))
}

func bindComputeConnectionTx(ctx context.Context, tx *tx, tenant, deploymentID, connectionID, stamp string) error {
	if connectionID == "" {
		return nil
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM compute_connections WHERE tenant_id=? AND id=? FOR SHARE`, tenant, connectionID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if status != "verified" {
		return fmt.Errorf("%w: compute connection is not verified", ErrConflict)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO deployment_compute_connections(tenant_id,deployment_id,compute_connection_id,created_at) VALUES(?,?,?,?)`, tenant, deploymentID, connectionID, stamp)
	return err
}

type scanner interface{ Scan(...any) error }

func scanComputeConnection(row scanner) (domain.ComputeConnection, error) {
	var item domain.ComputeConnection
	var verified time.Time
	var created, updated string
	err := row.Scan(&item.ID, &item.TenantID, &item.Name, &item.Provider, &item.Adapter, &item.Status, &item.CredentialCiphertext, &item.CredentialNonce, &item.CredentialKeyReference, &verified, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ComputeConnection{}, ErrNotFound
	}
	if err != nil {
		return domain.ComputeConnection{}, err
	}
	item.VerifiedAt = verified.UTC()
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	return item, nil
}

func prefixedComputeConnectionColumns(alias string) string {
	return alias + `.id,` + alias + `.tenant_id,` + alias + `.name,` + alias + `.provider,` + alias + `.adapter,` + alias + `.status,` + alias + `.credential_ciphertext,` + alias + `.credential_nonce,` + alias + `.credential_key_reference,` + alias + `.verified_at,` + alias + `.created_at,` + alias + `.updated_at`
}
