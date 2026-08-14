package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

const WebConsoleAccess = "web_console_access"

var identityProviderPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

func validateExternalIdentity(identity domain.ExternalIdentity) error {
	if !identityProviderPattern.MatchString(identity.Provider) {
		return errors.New("identity provider must be a lowercase identifier")
	}
	if strings.TrimSpace(identity.ExternalUserID) == "" || len(identity.ExternalUserID) > 255 {
		return errors.New("external user ID is required and must not exceed 255 characters")
	}
	if strings.TrimSpace(identity.ExternalOrganizationID) == "" || len(identity.ExternalOrganizationID) > 255 {
		return errors.New("external organization ID is required and must not exceed 255 characters")
	}
	return nil
}

func (s *Store) ProvisionConsoleIdentity(ctx context.Context, tenant string, request domain.ConsoleIdentityProvisioning) (domain.ConsoleIdentity, error) {
	if tenant == "" || strings.TrimSpace(request.DisplayName) == "" || len(request.DisplayName) > 160 {
		return domain.ConsoleIdentity{}, errors.New("tenant and display name are required")
	}
	if err := validateExternalIdentity(request.ExternalIdentity); err != nil {
		return domain.ConsoleIdentity{}, err
	}
	role := authz.Role(request.Role)
	if !validRole(role) {
		return domain.ConsoleIdentity{}, errors.New("valid membership role is required")
	}
	scopes := make([]authz.Action, len(request.Scopes))
	for i, scope := range request.Scopes {
		scopes[i] = authz.Action(scope)
	}
	if len(scopes) == 0 {
		scopes = authz.DefaultScopes(role)
	}
	if err := authz.ValidateScopes(role, scopes); err != nil {
		return domain.ConsoleIdentity{}, err
	}
	scopeNames := make([]string, len(scopes))
	for i, scope := range scopes {
		scopeNames[i] = string(scope)
	}
	encodedScopes, _ := json.Marshal(scopeNames)

	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ConsoleIdentity{}, err
	}
	defer tx.Rollback()

	var userID string
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM external_user_identities WHERE provider=? AND external_user_id=? FOR UPDATE`, request.Provider, request.ExternalUserID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		userID, err = newID()
		if err == nil {
			stamp := now()
			_, err = tx.ExecContext(ctx, `INSERT INTO infercrane_users(id,display_name,created_at,updated_at) VALUES(?,?,?,?)`, userID, strings.TrimSpace(request.DisplayName), stamp, stamp)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO external_user_identities(provider,external_user_id,user_id,created_at) VALUES(?,?,?,?)`, request.Provider, request.ExternalUserID, userID, now())
		}
	} else if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE infercrane_users SET display_name=?,updated_at=? WHERE id=? AND disabled=FALSE`, strings.TrimSpace(request.DisplayName), now(), userID)
	}
	if err != nil {
		return domain.ConsoleIdentity{}, err
	}

	var mappedTenant string
	err = tx.QueryRowContext(ctx, `SELECT tenant_id FROM external_organization_identities WHERE provider=? AND external_organization_id=? FOR UPDATE`, request.Provider, request.ExternalOrganizationID).Scan(&mappedTenant)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO external_organization_identities(provider,external_organization_id,tenant_id,created_at) VALUES(?,?,?,?)`, request.Provider, request.ExternalOrganizationID, tenant, now())
	} else if err == nil && mappedTenant != tenant {
		return domain.ConsoleIdentity{}, fmt.Errorf("%w: external organization is already mapped", ErrConflict)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.ConsoleIdentity{}, err
	}

	stamp := now()
	_, err = tx.ExecContext(ctx, `INSERT INTO organization_memberships(user_id,tenant_id,role,scopes_json,status,created_at,updated_at) VALUES(?,?,?,?::jsonb,'active',?,?) ON CONFLICT(user_id,tenant_id) DO UPDATE SET role=EXCLUDED.role,scopes_json=EXCLUDED.scopes_json,status='active',updated_at=EXCLUDED.updated_at`, userID, tenant, request.Role, string(encodedScopes), stamp, stamp)
	if err != nil {
		return domain.ConsoleIdentity{}, err
	}
	if request.Access {
		entitlementID, idErr := newID()
		if idErr != nil {
			return domain.ConsoleIdentity{}, idErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO entitlements(id,user_id,entitlement_key,created_at,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(user_id,entitlement_key) WHERE user_id IS NOT NULL DO UPDATE SET revoked_at=NULL,updated_at=EXCLUDED.updated_at`, entitlementID, userID, WebConsoleAccess, stamp, stamp)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE entitlements SET revoked_at=?,updated_at=? WHERE user_id=? AND entitlement_key=? AND revoked_at IS NULL`, stamp, stamp, userID, WebConsoleAccess)
	}
	if err != nil {
		return domain.ConsoleIdentity{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ConsoleIdentity{}, err
	}
	return domain.ConsoleIdentity{UserID: userID, TenantID: tenant, DisplayName: strings.TrimSpace(request.DisplayName), Role: request.Role, Scopes: scopeNames, Access: request.Access}, nil
}

func (s *Store) ResolveExternalPrincipal(ctx context.Context, identity domain.ExternalIdentity, entitlement string) (domain.Principal, error) {
	if err := validateExternalIdentity(identity); err != nil || entitlement == "" {
		return domain.Principal{}, ErrNotFound
	}
	var principal domain.Principal
	var scopesJSON string
	err := s.QueryRowContext(ctx, `
		SELECT u.id,o.tenant_id,u.display_name,m.role,m.scopes_json::text
		FROM external_user_identities x
		JOIN infercrane_users u ON u.id=x.user_id AND u.disabled=FALSE
		JOIN external_organization_identities o ON o.provider=x.provider AND o.external_organization_id=?
		JOIN organization_memberships m ON m.user_id=u.id AND m.tenant_id=o.tenant_id AND m.status='active'
		WHERE x.provider=? AND x.external_user_id=?
		AND EXISTS (
			SELECT 1 FROM entitlements e
			WHERE e.entitlement_key=? AND e.revoked_at IS NULL
			AND (e.user_id=u.id OR e.tenant_id=o.tenant_id)
		)
	`, identity.ExternalOrganizationID, identity.Provider, identity.ExternalUserID, entitlement).Scan(&principal.ID, &principal.TenantID, &principal.Name, &principal.Role, &scopesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Principal{}, ErrNotFound
	}
	if err != nil {
		return domain.Principal{}, err
	}
	if err = json.Unmarshal([]byte(scopesJSON), &principal.Scopes); err != nil {
		return domain.Principal{}, fmt.Errorf("decode membership scopes: %w", err)
	}
	principal.Kind = "human"
	return principal, nil
}

func (s *Store) ConsoleIdentitiesForTenant(ctx context.Context, tenant string) ([]domain.ConsoleIdentity, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT u.id,m.tenant_id,u.display_name,m.role,m.scopes_json::text,
		EXISTS (
			SELECT 1 FROM entitlements e
			WHERE e.entitlement_key=? AND e.revoked_at IS NULL
			AND (e.user_id=u.id OR e.tenant_id=m.tenant_id)
		)
		FROM organization_memberships m
		JOIN infercrane_users u ON u.id=m.user_id AND u.disabled=FALSE
		WHERE m.tenant_id=? AND m.status='active'
		ORDER BY u.display_name,u.id
	`, WebConsoleAccess, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.ConsoleIdentity, 0)
	for rows.Next() {
		var identity domain.ConsoleIdentity
		var scopesJSON string
		if err := rows.Scan(&identity.UserID, &identity.TenantID, &identity.DisplayName, &identity.Role, &scopesJSON, &identity.Access); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scopesJSON), &identity.Scopes); err != nil {
			return nil, fmt.Errorf("decode membership scopes: %w", err)
		}
		out = append(out, identity)
	}
	return out, rows.Err()
}
