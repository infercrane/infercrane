package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/infercrane/infercrane/internal/domain"
)

// CreateModelImport registers a manifest once per workspace, location, and
// digest. Registering the same manifest again returns the first import.
func (s *Store) CreateModelImport(ctx context.Context, item domain.ModelImport) (domain.ModelImport, error) {
	if item.TenantID == "" || item.URI == "" || item.ManifestDigest == "" || len(item.Files) == 0 {
		return domain.ModelImport{}, errors.New("tenant, location, digest, and files are required")
	}
	manifest, err := json.Marshal(map[string]any{"files": item.Files})
	if err != nil {
		return domain.ModelImport{}, err
	}
	checks, err := json.Marshal(item.Checks)
	if err != nil {
		return domain.ModelImport{}, err
	}
	id, err := newID()
	if err != nil {
		return domain.ModelImport{}, err
	}
	if _, err = s.ExecContext(ctx, `INSERT INTO model_imports(id,tenant_id,kind,source,uri,manifest_digest,manifest_json,checks_json,created_at) VALUES(?,?,?,?,?,?,?::jsonb,?::jsonb,?) ON CONFLICT(tenant_id,uri,manifest_digest) DO NOTHING`, id, item.TenantID, item.Kind, item.Source, item.URI, item.ManifestDigest, string(manifest), string(checks), now()); err != nil {
		return domain.ModelImport{}, err
	}
	return s.ModelImportForTenant(ctx, item.TenantID, item.URI, item.ManifestDigest)
}

// ModelImportForTenant returns the import pinned by one location and digest.
func (s *Store) ModelImportForTenant(ctx context.Context, tenant, uri, digest string) (domain.ModelImport, error) {
	return scanModelImport(s.QueryRowContext(ctx, `SELECT id,tenant_id,kind,source,uri,manifest_digest,manifest_json,checks_json,created_at FROM model_imports WHERE tenant_id=? AND uri=? AND manifest_digest=?`, tenant, uri, digest))
}

// ModelImportsForTenant lists a workspace's imports, newest first.
func (s *Store) ModelImportsForTenant(ctx context.Context, tenant string, limit int) ([]domain.ModelImport, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,kind,source,uri,manifest_digest,manifest_json,checks_json,created_at FROM model_imports WHERE tenant_id=? ORDER BY created_at DESC, id LIMIT ?`, tenant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.ModelImport, 0)
	for rows.Next() {
		item, scanErr := scanModelImport(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type modelImportScanner interface{ Scan(...any) error }

func scanModelImport(row modelImportScanner) (domain.ModelImport, error) {
	var item domain.ModelImport
	var manifest, checks, created string
	err := row.Scan(&item.ID, &item.TenantID, &item.Kind, &item.Source, &item.URI, &item.ManifestDigest, &manifest, &checks, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ModelImport{}, ErrNotFound
	}
	if err != nil {
		return domain.ModelImport{}, err
	}
	var files struct {
		Files []domain.ModelFile `json:"files"`
	}
	if err = json.Unmarshal([]byte(manifest), &files); err != nil {
		return domain.ModelImport{}, err
	}
	if err = json.Unmarshal([]byte(checks), &item.Checks); err != nil {
		return domain.ModelImport{}, err
	}
	item.Files, item.CreatedAt = files.Files, parseTime(created)
	return item, nil
}
