package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) CreateTenant(ctx context.Context, id, name string) error {
	if id == "" || name == "" {
		return errors.New("tenant ID and name are required")
	}
	_, err := s.ExecContext(ctx, `INSERT INTO tenants(id,name,created_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name`, id, name, now())
	return err
}
func (s *Store) CreatePrincipal(ctx context.Context, tenant, name string, role authz.Role) (domain.Principal, string, error) {
	return s.CreatePrincipalScoped(ctx, tenant, name, role, authz.DefaultScopes(role))
}

func (s *Store) CreatePrincipalScoped(ctx context.Context, tenant, name string, role authz.Role, scopes []authz.Action) (domain.Principal, string, error) {
	if tenant == "" || name == "" || !validRole(role) {
		return domain.Principal{}, "", errors.New("tenant, name, and valid role are required")
	}
	if len(scopes) == 0 {
		return domain.Principal{}, "", errors.New("at least one explicit scope is required")
	}
	if err := authz.ValidateScopes(role, scopes); err != nil {
		return domain.Principal{}, "", err
	}
	id, err := newID()
	if err != nil {
		return domain.Principal{}, "", err
	}
	token, hash, err := newCredential()
	if err != nil {
		return domain.Principal{}, "", err
	}
	stamp := now()
	scopeNames := make([]string, len(scopes))
	for i, scope := range scopes {
		scopeNames[i] = string(scope)
	}
	encodedScopes, _ := json.Marshal(scopeNames)
	_, err = s.ExecContext(ctx, `INSERT INTO principals(id,tenant_id,name,role,credential_hash,kind,scopes_json,created_at,endpoint_names_json) VALUES(?,?,?,?,?,'service_account',?::jsonb,?,'[]'::jsonb)`, id, tenant, name, string(role), hash, string(encodedScopes), stamp)
	if isUniqueViolation(err) {
		return domain.Principal{}, "", fmt.Errorf("%w: principal name or credential already exists", ErrConflict)
	}
	return domain.Principal{ID: id, TenantID: tenant, Name: name, Role: string(role), Kind: "service_account", Scopes: scopeNames, CreatedAt: parseTime(stamp)}, token, err
}
func (s *Store) AuthenticatePrincipal(ctx context.Context, token string) (domain.Principal, error) {
	hash := credentialHash(token)
	var out domain.Principal
	var stamp, scopesJSON, endpointsJSON string
	var expires sql.NullString
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,name,role,kind,scopes_json::text,endpoint_names_json::text,disabled,created_at,expires_at FROM principals WHERE credential_hash=? AND disabled=FALSE AND (expires_at IS NULL OR expires_at>NOW())`, hash).Scan(&out.ID, &out.TenantID, &out.Name, &out.Role, &out.Kind, &scopesJSON, &endpointsJSON, &out.Disabled, &stamp, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.CreatedAt = parseTime(stamp)
	if err := json.Unmarshal([]byte(scopesJSON), &out.Scopes); err != nil {
		return domain.Principal{}, fmt.Errorf("decode principal scopes: %w", err)
	}
	if err := json.Unmarshal([]byte(endpointsJSON), &out.EndpointNames); err != nil {
		return domain.Principal{}, fmt.Errorf("decode principal endpoint restrictions: %w", err)
	}
	if expires.Valid {
		value := parseTime(expires.String)
		out.ExpiresAt = &value
	}
	return out, nil
}
func (s *Store) ActiveCredentials(ctx context.Context) ([]domain.CredentialRecord, error) {
	rows, err := s.QueryContext(ctx, `SELECT credential_hash,id,tenant_id,name,role,kind,scopes_json::text,endpoint_names_json::text,disabled,created_at,expires_at FROM principals WHERE disabled=FALSE AND (expires_at IS NULL OR expires_at>NOW())`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CredentialRecord
	for rows.Next() {
		var record domain.CredentialRecord
		var scopesJSON, endpointsJSON string
		var stamp string
		var expires sql.NullString
		if err := rows.Scan(&record.Hash, &record.Principal.ID, &record.Principal.TenantID, &record.Principal.Name, &record.Principal.Role, &record.Principal.Kind, &scopesJSON, &endpointsJSON, &record.Principal.Disabled, &stamp, &expires); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scopesJSON), &record.Principal.Scopes); err != nil {
			return nil, fmt.Errorf("decode principal scopes: %w", err)
		}
		record.Principal.CreatedAt = parseTime(stamp)
		if err := json.Unmarshal([]byte(endpointsJSON), &record.Principal.EndpointNames); err != nil {
			return nil, fmt.Errorf("decode principal endpoint restrictions: %w", err)
		}
		if expires.Valid {
			value := parseTime(expires.String)
			record.Principal.ExpiresAt = &value
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (s *Store) PrincipalsForTenant(ctx context.Context, tenant string) ([]domain.Principal, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,name,role,kind,scopes_json::text,endpoint_names_json::text,disabled,created_at,expires_at FROM principals WHERE tenant_id=? ORDER BY created_at DESC,id DESC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Principal, 0)
	for rows.Next() {
		var principal domain.Principal
		var scopesJSON, endpointsJSON, stamp string
		var expires sql.NullString
		if err := rows.Scan(&principal.ID, &principal.TenantID, &principal.Name, &principal.Role, &principal.Kind, &scopesJSON, &endpointsJSON, &principal.Disabled, &stamp, &expires); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scopesJSON), &principal.Scopes); err != nil {
			return nil, fmt.Errorf("decode principal scopes: %w", err)
		}
		principal.CreatedAt = parseTime(stamp)
		if err := json.Unmarshal([]byte(endpointsJSON), &principal.EndpointNames); err != nil {
			return nil, fmt.Errorf("decode principal endpoint restrictions: %w", err)
		}
		if expires.Valid {
			value := parseTime(expires.String)
			principal.ExpiresAt = &value
		}
		out = append(out, principal)
	}
	return out, rows.Err()
}
func (s *Store) RotatePrincipal(ctx context.Context, id string) (string, error) {
	return s.RotatePrincipalForTenant(ctx, "", id)
}
func (s *Store) RotatePrincipalForTenant(ctx context.Context, tenant, id string) (string, error) {
	token, hash, err := newCredential()
	if err != nil {
		return "", err
	}
	query := `UPDATE principals SET credential_hash=? WHERE id=? AND disabled=FALSE`
	args := []any{hash, id}
	if tenant != "" {
		query += ` AND tenant_id=?`
		args = append(args, tenant)
	}
	result, err := s.ExecContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return "", ErrNotFound
	}
	return token, nil
}
func (s *Store) RevokePrincipal(ctx context.Context, id string) error {
	return s.RevokePrincipalForTenant(ctx, "", id)
}
func (s *Store) RevokePrincipalForTenant(ctx context.Context, tenant, id string) error {
	query := `UPDATE principals SET disabled=TRUE WHERE id=? AND disabled=FALSE`
	args := []any{id}
	if tenant != "" {
		query += ` AND tenant_id=?`
		args = append(args, tenant)
	}
	result, err := s.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
func newCredential() (string, string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", "", err
	}
	token := "ic_" + hex.EncodeToString(bytes)
	return token, credentialHash(token), nil
}
func credentialHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func validRole(role authz.Role) bool {
	return role == authz.Viewer || role == authz.Operator || role == authz.Admin
}
