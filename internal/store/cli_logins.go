package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/clilogin"
	"github.com/infercrane/infercrane/internal/domain"
)

const cliLoginColumns = `id,user_code,device_name,client_os,client_version,status,COALESCE(tenant_id,''),COALESCE(approver_id,''),COALESCE(approver_name,''),created_at,expires_at,decided_at`

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

// CreateCLILogin records a sign-in waiting for approval. Starts are serialized
// so the pending bound holds under concurrency, and old requests are pruned in
// small batches on the way in.
func (s *Store) CreateCLILogin(ctx context.Context, start domain.CLILoginStart, maxPending int) (domain.CLILoginRequest, error) {
	if start.UserCode == "" || start.DeviceCodeHash == "" || !start.ExpiresAt.After(start.CreatedAt) {
		return domain.CLILoginRequest{}, errors.New("user code, device code hash, and a future expiry are required")
	}
	id, err := newID()
	if err != nil {
		return domain.CLILoginRequest{}, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.CLILoginRequest{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('infercrane-cli-login-start',0))`); err != nil {
		return domain.CLILoginRequest{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM cli_login_requests WHERE id IN (SELECT id FROM cli_login_requests WHERE created_at < ? ORDER BY created_at LIMIT 200)`, stamp(start.CreatedAt.Add(-clilogin.Retention))); err != nil {
		return domain.CLILoginRequest{}, err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM cli_login_requests WHERE status='pending' AND expires_at > ?`, stamp(start.CreatedAt)).Scan(&pending); err != nil {
		return domain.CLILoginRequest{}, err
	}
	if pending >= maxPending {
		return domain.CLILoginRequest{}, fmt.Errorf("%w: too many CLI sign-ins are waiting for approval", domain.ErrCapacityExhausted)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO cli_login_requests(id,user_code,device_code_hash,device_name,client_os,client_version,status,created_at,expires_at) VALUES(?,?,?,?,?,?,'pending',?,?)`,
		id, start.UserCode, start.DeviceCodeHash, start.DeviceName, start.ClientOS, start.ClientVersion, stamp(start.CreatedAt), stamp(start.ExpiresAt))
	if isUniqueViolation(err) {
		return domain.CLILoginRequest{}, fmt.Errorf("%w: CLI sign-in code already exists", ErrConflict)
	}
	if err != nil {
		return domain.CLILoginRequest{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.CLILoginRequest{}, err
	}
	return domain.CLILoginRequest{ID: id, UserCode: start.UserCode, DeviceName: start.DeviceName, ClientOS: start.ClientOS, ClientVersion: start.ClientVersion, Status: clilogin.StatusPending, CreatedAt: start.CreatedAt.UTC(), ExpiresAt: start.ExpiresAt.UTC()}, nil
}

func scanCLILogin(row rowScanner, now time.Time) (domain.CLILoginRequest, error) {
	var out domain.CLILoginRequest
	var created, expires string
	var decided sql.NullString
	if err := row.Scan(&out.ID, &out.UserCode, &out.DeviceName, &out.ClientOS, &out.ClientVersion, &out.Status, &out.TenantID, &out.ApproverID, &out.ApproverName, &created, &expires, &decided); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.CLILoginRequest{}, ErrNotFound
		}
		return domain.CLILoginRequest{}, err
	}
	out.CreatedAt, out.ExpiresAt = parseTime(created), parseTime(expires)
	if decided.Valid {
		value := parseTime(decided.String)
		out.DecidedAt = &value
	}
	if out.Status == clilogin.StatusPending && !out.ExpiresAt.After(now) {
		out.Status = clilogin.StatusExpired
	}
	return out, nil
}

// CLILoginByUserCode returns the request a person is about to approve.
func (s *Store) CLILoginByUserCode(ctx context.Context, userCode string, now time.Time) (domain.CLILoginRequest, error) {
	return scanCLILogin(s.QueryRowContext(ctx, `SELECT `+cliLoginColumns+` FROM cli_login_requests WHERE user_code=?`, userCode), now)
}

// DecideCLILogin approves or denies a pending, unexpired request. Approval
// records the approver's tenant, role, and scopes; the key itself is minted
// only when the CLI exchanges its device code.
func (s *Store) DecideCLILogin(ctx context.Context, userCode string, approver domain.Principal, approve bool, scopes []string, now time.Time) (domain.CLILoginRequest, error) {
	if approver.ID == "" || approver.TenantID == "" {
		return domain.CLILoginRequest{}, errors.New("approver identity is required")
	}
	status, role, scopesJSON := clilogin.StatusDenied, any(nil), any(nil)
	if approve {
		encoded, err := json.Marshal(scopes)
		if err != nil {
			return domain.CLILoginRequest{}, err
		}
		status, role, scopesJSON = clilogin.StatusApproved, approver.Role, string(encoded)
	}
	row := s.QueryRowContext(ctx, `UPDATE cli_login_requests SET status=?,tenant_id=?,approver_id=?,approver_name=?,approver_role=?,approver_scopes_json=?::jsonb,decided_at=? WHERE user_code=? AND status='pending' AND expires_at > ? RETURNING `+cliLoginColumns,
		status, approver.TenantID, approver.ID, approver.Name, role, scopesJSON, stamp(now), userCode, stamp(now))
	decided, err := scanCLILogin(row, now)
	if !errors.Is(err, ErrNotFound) {
		return decided, err
	}
	if _, lookupErr := s.CLILoginByUserCode(ctx, userCode, now); lookupErr != nil {
		return domain.CLILoginRequest{}, lookupErr
	}
	return domain.CLILoginRequest{}, fmt.Errorf("%w: CLI sign-in is no longer waiting for approval", ErrConflict)
}

// ExchangeCLILogin is the CLI's poll. It reports pending, slow_down, denied,
// expired, or consumed, and on the first poll after approval it mints the key
// in the same transaction that marks the request consumed, so a device code
// yields at most one credential.
func (s *Store) ExchangeCLILogin(ctx context.Context, deviceCodeHash string, now time.Time, minPollGap, lifetime time.Duration) (domain.CLILoginExchange, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.CLILoginExchange{}, err
	}
	defer tx.Rollback()
	var id, status, tenant, approverID, approverName, role, scopesJSON, deviceName, clientOS, clientVersion, expires string
	var lastPolled sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,status,expires_at,last_polled_at,COALESCE(tenant_id,''),COALESCE(approver_id,''),COALESCE(approver_name,''),COALESCE(approver_role,''),COALESCE(approver_scopes_json::text,'[]'),device_name,client_os,client_version FROM cli_login_requests WHERE device_code_hash=? FOR UPDATE`, deviceCodeHash).
		Scan(&id, &status, &expires, &lastPolled, &tenant, &approverID, &approverName, &role, &scopesJSON, &deviceName, &clientOS, &clientVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CLILoginExchange{}, ErrNotFound
	}
	if err != nil {
		return domain.CLILoginExchange{}, err
	}
	expired := !parseTime(expires).After(now)
	switch {
	case status == clilogin.StatusDenied:
		return domain.CLILoginExchange{Status: clilogin.StatusDenied}, nil
	case status == clilogin.StatusConsumed:
		return domain.CLILoginExchange{Status: clilogin.StatusConsumed}, nil
	case expired:
		return domain.CLILoginExchange{Status: clilogin.StatusExpired}, nil
	case status == clilogin.StatusPending:
		result := clilogin.StatusPending
		if lastPolled.Valid && now.Sub(parseTime(lastPolled.String)) < minPollGap {
			result = clilogin.StatusSlowDown
		}
		if _, err = tx.ExecContext(ctx, `UPDATE cli_login_requests SET last_polled_at=? WHERE id=?`, stamp(now), id); err != nil {
			return domain.CLILoginExchange{}, err
		}
		if err = tx.Commit(); err != nil {
			return domain.CLILoginExchange{}, err
		}
		return domain.CLILoginExchange{Status: result}, nil
	case status != clilogin.StatusApproved:
		return domain.CLILoginExchange{}, fmt.Errorf("unknown CLI sign-in status %q", status)
	}

	var scopes []string
	if err = json.Unmarshal([]byte(scopesJSON), &scopes); err != nil || len(scopes) == 0 {
		return domain.CLILoginExchange{}, errors.New("approved CLI sign-in has no recorded scopes")
	}
	principalID, err := newID()
	if err != nil {
		return domain.CLILoginExchange{}, err
	}
	token, hash, err := newCredential()
	if err != nil {
		return domain.CLILoginExchange{}, err
	}
	created, expiresAt := now.UTC(), now.UTC().Add(lifetime)
	name := clilogin.PrincipalName(deviceName, principalID)
	encodedScopes, _ := json.Marshal(scopes)
	if _, err = tx.ExecContext(ctx, `INSERT INTO principals(id,tenant_id,name,role,credential_hash,kind,scopes_json,created_at,endpoint_names_json,expires_at) VALUES(?,?,?,?,?,?,?::jsonb,?,'[]'::jsonb,?)`,
		principalID, tenant, name, role, hash, clilogin.PrincipalKind, string(encodedScopes), stamp(created), stamp(expiresAt)); err != nil {
		return domain.CLILoginExchange{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO cli_sessions(principal_id,tenant_id,owner_id,owner_name,device_name,client_os,client_version,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		principalID, tenant, approverID, approverName, deviceName, clientOS, clientVersion, stamp(created)); err != nil {
		return domain.CLILoginExchange{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE cli_login_requests SET status='consumed',consumed_at=?,principal_id=? WHERE id=?`, stamp(now), principalID, id); err != nil {
		return domain.CLILoginExchange{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.CLILoginExchange{}, err
	}
	principal := domain.Principal{ID: principalID, TenantID: tenant, Name: name, Role: role, Kind: clilogin.PrincipalKind, Scopes: scopes, CreatedAt: created, ExpiresAt: &expiresAt}
	return domain.CLILoginExchange{Status: clilogin.StatusApproved, Principal: principal, Credential: token, OwnerName: approverName}, nil
}

const cliSessionColumns = `c.principal_id,c.tenant_id,c.owner_id,c.owner_name,c.device_name,c.client_os,c.client_version,c.created_at,c.last_used_at,p.expires_at`

func scanCLISession(row rowScanner) (domain.CLISession, error) {
	var out domain.CLISession
	var created string
	var lastUsed, expires sql.NullString
	if err := row.Scan(&out.ID, &out.TenantID, &out.OwnerID, &out.OwnerName, &out.DeviceName, &out.ClientOS, &out.ClientVersion, &created, &lastUsed, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.CLISession{}, ErrNotFound
		}
		return domain.CLISession{}, err
	}
	out.CreatedAt = parseTime(created)
	if lastUsed.Valid {
		value := parseTime(lastUsed.String)
		out.LastUsedAt = &value
	}
	if expires.Valid {
		value := parseTime(expires.String)
		out.ExpiresAt = &value
	}
	return out, nil
}

// CLISessionsForTenant lists active CLI keys, newest use first. An empty owner
// lists every member's sessions.
func (s *Store) CLISessionsForTenant(ctx context.Context, tenant, owner string, limit int) ([]domain.CLISession, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := s.QueryContext(ctx, `SELECT `+cliSessionColumns+` FROM cli_sessions c JOIN principals p ON p.id=c.principal_id
		WHERE c.tenant_id=? AND (?='' OR c.owner_id=?) AND p.disabled=FALSE AND (p.expires_at IS NULL OR p.expires_at > NOW())
		ORDER BY COALESCE(c.last_used_at,c.created_at) DESC, c.principal_id DESC LIMIT ?`, tenant, owner, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.CLISession, 0)
	for rows.Next() {
		session, scanErr := scanCLISession(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// CLISession returns one active CLI key in a tenant.
func (s *Store) CLISession(ctx context.Context, tenant, id string) (domain.CLISession, error) {
	return scanCLISession(s.QueryRowContext(ctx, `SELECT `+cliSessionColumns+` FROM cli_sessions c JOIN principals p ON p.id=c.principal_id
		WHERE c.tenant_id=? AND c.principal_id=? AND p.disabled=FALSE AND (p.expires_at IS NULL OR p.expires_at > NOW())`, tenant, id))
}

// RevokeCLISession disables the key behind a CLI session.
func (s *Store) RevokeCLISession(ctx context.Context, tenant, id string) error {
	result, err := s.ExecContext(ctx, `UPDATE principals SET disabled=TRUE WHERE id=? AND tenant_id=? AND kind='cli_session' AND disabled=FALSE`, id, tenant)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchCLISession records that a CLI key was used, at most once per gap.
func (s *Store) TouchCLISession(ctx context.Context, id string, at time.Time, gap time.Duration) error {
	_, err := s.ExecContext(ctx, `UPDATE cli_sessions SET last_used_at=? WHERE principal_id=? AND (last_used_at IS NULL OR last_used_at < ?)`, stamp(at), id, stamp(at.Add(-gap)))
	return err
}
