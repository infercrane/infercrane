package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

const agentColumns = `a.id,a.tenant_id,a.principal_id,a.name,a.state,a.created_by,COALESCE(a.agent_version,''),COALESCE(a.protocol_version,''),COALESCE(a.os,''),COALESCE(a.arch,''),a.hardware_json::text,a.servers_json::text,COALESCE(a.gateway_server_key,''),a.last_seen_at,a.created_at,a.updated_at,a.revoked_at`

func scanAgent(row rowScanner) (domain.Agent, error) {
	var out domain.Agent
	var created, updated string
	var lastSeen, revoked sql.NullString
	if err := row.Scan(&out.ID, &out.TenantID, &out.PrincipalID, &out.Name, &out.State, &out.CreatedBy, &out.AgentVersion, &out.ProtocolVersion, &out.OS, &out.Arch, &out.HardwareJSON, &out.ServersJSON, &out.GatewayServerKey, &lastSeen, &created, &updated, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Agent{}, ErrNotFound
		}
		return domain.Agent{}, err
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	if lastSeen.Valid {
		value := parseTime(lastSeen.String)
		out.LastSeenAt = &value
	}
	if revoked.Valid {
		value := parseTime(revoked.String)
		out.RevokedAt = &value
	}
	return out, nil
}

// CreateAgent mints one agent key. The key is returned once; only its hash
// is stored, on a principal of kind agent that the control API confines to
// the agent routes.
func (s *Store) CreateAgent(ctx context.Context, tenant, name, createdBy string) (domain.Agent, string, error) {
	if tenant == "" || !validEndpointName(name) || createdBy == "" || len(createdBy) > 255 {
		return domain.Agent{}, "", errors.New("tenant, a lowercase agent name, and the creating principal are required")
	}
	agentID, err := newID()
	if err != nil {
		return domain.Agent{}, "", err
	}
	principalID, err := newID()
	if err != nil {
		return domain.Agent{}, "", err
	}
	token, hash, err := newCredential()
	if err != nil {
		return domain.Agent{}, "", err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.Agent{}, "", err
	}
	defer tx.Rollback()
	created := now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO principals(id,tenant_id,name,role,credential_hash,kind,scopes_json,created_at,endpoint_names_json) VALUES(?,?,?,'viewer',?,?,'[]'::jsonb,?,'[]'::jsonb)`, principalID, tenant, "agent:"+name+":"+agentID[:12], hash, domain.AgentPrincipalKind, created); err != nil {
		return domain.Agent{}, "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agents(id,tenant_id,principal_id,name,state,created_by,created_at,updated_at) VALUES(?,?,?,?,'active',?,?,?)`, agentID, tenant, principalID, name, createdBy, created, created); err != nil {
		if isUniqueViolation(err) {
			return domain.Agent{}, "", fmt.Errorf("%w: an active agent named %s already exists", ErrConflict, name)
		}
		return domain.Agent{}, "", err
	}
	if err = tx.Commit(); err != nil {
		return domain.Agent{}, "", err
	}
	agent, err := s.AgentForTenant(ctx, tenant, agentID)
	return agent, token, err
}

// AgentsForTenant lists agents, active first and newest first.
func (s *Store) AgentsForTenant(ctx context.Context, tenant string) ([]domain.Agent, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+agentColumns+` FROM agents a WHERE a.tenant_id=? ORDER BY a.state='active' DESC,a.created_at DESC LIMIT 500`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Agent{}
	for rows.Next() {
		agent, scanErr := scanAgent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, agent)
	}
	return out, rows.Err()
}

// AgentForTenant finds an agent by ID, or an active agent by name.
func (s *Store) AgentForTenant(ctx context.Context, tenant, idOrName string) (domain.Agent, error) {
	return scanAgent(s.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM agents a WHERE a.tenant_id=? AND (a.id=? OR (a.name=? AND a.state='active')) ORDER BY a.id=? DESC LIMIT 1`, tenant, idOrName, idOrName, idOrName))
}

// AgentForPrincipal resolves the agent that owns an authenticated key.
func (s *Store) AgentForPrincipal(ctx context.Context, principalID string) (domain.Agent, error) {
	return scanAgent(s.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM agents a WHERE a.principal_id=?`, principalID))
}

// RevokeAgent disables the agent's key and marks every server it relayed as
// unhealthy, because nothing reports their health any more. Revoking twice
// is a no-op that returns the revoked agent.
func (s *Store) RevokeAgent(ctx context.Context, tenant, id string) (domain.Agent, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.Agent{}, err
	}
	defer tx.Rollback()
	var principalID, state string
	if err = tx.QueryRowContext(ctx, `SELECT principal_id,state FROM agents WHERE tenant_id=? AND id=? FOR UPDATE`, tenant, id).Scan(&principalID, &state); errors.Is(err, sql.ErrNoRows) {
		return domain.Agent{}, ErrNotFound
	} else if err != nil {
		return domain.Agent{}, err
	}
	if state == "active" {
		stamp := now()
		if _, err = tx.ExecContext(ctx, `UPDATE agents SET state='revoked',revoked_at=?,updated_at=? WHERE id=? AND tenant_id=?`, stamp, stamp, id, tenant); err != nil {
			return domain.Agent{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE principals SET disabled=TRUE WHERE id=? AND tenant_id=?`, principalID, tenant); err != nil {
			return domain.Agent{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE targets SET health='unhealthy',updated_at=? WHERE tenant_id=? AND agent_id=? AND health<>'unhealthy'`, stamp, tenant, id); err != nil {
			return domain.Agent{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.Agent{}, err
	}
	return s.AgentForTenant(ctx, tenant, id)
}

// RecordAgentHeartbeat stores what one sync reported about the agent. It
// refuses a revoked agent so a request that raced a revocation records
// nothing.
func (s *Store) RecordAgentHeartbeat(ctx context.Context, tenant, agentID string, heartbeat domain.AgentHeartbeat) error {
	hardware, servers := heartbeat.HardwareJSON, heartbeat.ServersJSON
	if hardware == "" {
		hardware = "{}"
	}
	if servers == "" {
		servers = "[]"
	}
	result, err := s.ExecContext(ctx, `UPDATE agents SET agent_version=?,protocol_version=?,os=?,arch=?,hardware_json=?::jsonb,servers_json=?::jsonb,gateway_server_key=?,last_seen_at=?,updated_at=? WHERE tenant_id=? AND id=? AND state='active'`,
		heartbeat.AgentVersion, heartbeat.ProtocolVersion, heartbeat.OS, heartbeat.Arch, hardware, servers, null(heartbeat.GatewayServerKey), stamp(heartbeat.SeenAt), now(), tenant, agentID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

const agentServerColumns = `t.agent_id,t.agent_server_key,t.id,t.url,t.runtime,COALESCE(t.upstream_model_name,''),t.health,t.agent_reported_at,e.id,e.name,e.logical_model_id,e.environment_id,b.id,COALESCE(e.active_serving_plan_id,''),a.ownership_mode`

func scanAgentServer(row rowScanner) (domain.AgentServer, error) {
	var out domain.AgentServer
	var reported sql.NullString
	if err := row.Scan(&out.AgentID, &out.Key, &out.TargetID, &out.Address, &out.Runtime, &out.UpstreamModel, &out.Health, &reported, &out.EndpointID, &out.EndpointName, &out.LogicalModelID, &out.EnvironmentID, &out.BindingID, &out.ServingPlanID, &out.OwnershipMode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AgentServer{}, ErrNotFound
		}
		return domain.AgentServer{}, err
	}
	if reported.Valid {
		value := parseTime(reported.String)
		out.ReportedAt = &value
	}
	return out, nil
}

// AgentServers lists the local servers observed through one agent whose
// endpoints still exist.
func (s *Store) AgentServers(ctx context.Context, tenant, agentID string) ([]domain.AgentServer, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+agentServerColumns+` FROM targets t JOIN adopted_workloads a ON a.target_id=t.id AND a.tenant_id=t.tenant_id JOIN endpoints e ON e.id=a.endpoint_id AND e.desired_state<>'deleted' JOIN backend_bindings b ON b.id=a.binding_id WHERE t.tenant_id=? AND t.agent_id=? ORDER BY t.agent_server_key LIMIT 64`, tenant, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AgentServer{}
	for rows.Next() {
		server, scanErr := scanAgentServer(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, server)
	}
	return out, rows.Err()
}

// RegisterAgentServer observes one agent-local server as an observe-only
// endpoint. The target belongs to the agent and is never dialled by the
// control plane. Registering the same server again with the same endpoint
// and model returns the existing registration.
func (s *Store) RegisterAgentServer(ctx context.Context, tenant, agentID string, registration domain.AgentServerRegistration) (domain.AgentServer, bool, error) {
	if tenant == "" || agentID == "" || !validEndpointName(registration.EndpointName) || registration.Key == "" || registration.UpstreamModel == "" || registration.Runtime == "" || registration.Address == "" {
		return domain.AgentServer{}, false, errors.New("tenant, agent, server key, endpoint name, model, runtime, and address are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.AgentServer{}, false, err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM agents WHERE tenant_id=? AND id=? FOR SHARE`, tenant, agentID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return domain.AgentServer{}, false, ErrNotFound
	} else if err != nil {
		return domain.AgentServer{}, false, err
	}
	if state != "active" {
		return domain.AgentServer{}, false, fmt.Errorf("%w: the agent is revoked", ErrConflict)
	}
	existing, err := scanAgentServer(tx.QueryRowContext(ctx, `SELECT `+agentServerColumns+` FROM targets t JOIN adopted_workloads a ON a.target_id=t.id AND a.tenant_id=t.tenant_id JOIN endpoints e ON e.id=a.endpoint_id AND e.desired_state<>'deleted' JOIN backend_bindings b ON b.id=a.binding_id WHERE t.tenant_id=? AND t.agent_id=? AND t.agent_server_key=?`, tenant, agentID, registration.Key))
	if err == nil {
		if existing.EndpointName != registration.EndpointName || existing.UpstreamModel != registration.UpstreamModel {
			return domain.AgentServer{}, false, fmt.Errorf("%w: server %s is already observed as endpoint %s for model %s", ErrConflict, registration.Key, existing.EndpointName, existing.UpstreamModel)
		}
		if err = tx.Commit(); err != nil {
			return domain.AgentServer{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return domain.AgentServer{}, false, err
	}
	stamp := now()
	var targetID string
	switch lookupErr := tx.QueryRowContext(ctx, `SELECT id FROM targets WHERE tenant_id=? AND agent_id=? AND agent_server_key=?`, tenant, agentID, registration.Key).Scan(&targetID); {
	case lookupErr == nil:
		// The endpoint was deleted; reuse the agent's target for the new one.
		if _, err = tx.ExecContext(ctx, `UPDATE targets SET url=?,runtime=?,upstream_model_name=?,updated_at=? WHERE id=?`, registration.Address, registration.Runtime, registration.UpstreamModel, stamp, targetID); err != nil {
			return domain.AgentServer{}, false, err
		}
	case errors.Is(lookupErr, sql.ErrNoRows):
		if targetID, err = newID(); err != nil {
			return domain.AgentServer{}, false, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO targets(id,name,url,provider,runtime,upstream_model_name,health,provider_details_json,tenant_id,agent_id,agent_server_key,created_at,updated_at) VALUES(?,?,?,'external',?,?,'starting','{}'::jsonb,?,?,?,?,?)`,
			targetID, "agent-"+targetID, registration.Address, registration.Runtime, registration.UpstreamModel, tenant, agentID, registration.Key, stamp, stamp); err != nil {
			return domain.AgentServer{}, false, err
		}
	default:
		return domain.AgentServer{}, false, lookupErr
	}
	digest := sha256.Sum256([]byte("agent\x00" + agentID + "\x00" + registration.Key + "\x00" + registration.EndpointName + "\x00" + registration.UpstreamModel))
	if _, err = insertAdoptedEndpointTx(ctx, tx, tenant, registration.EndpointName, registration.EndpointName, targetID, "observe-only", "openai-compatible", "sha256:"+hex.EncodeToString(digest[:]), stamp); err != nil {
		return domain.AgentServer{}, false, err
	}
	created, err := scanAgentServer(tx.QueryRowContext(ctx, `SELECT `+agentServerColumns+` FROM targets t JOIN adopted_workloads a ON a.target_id=t.id AND a.tenant_id=t.tenant_id JOIN endpoints e ON e.id=a.endpoint_id AND e.desired_state<>'deleted' JOIN backend_bindings b ON b.id=a.binding_id WHERE t.id=?`, targetID))
	if err != nil {
		return domain.AgentServer{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.AgentServer{}, false, err
	}
	return created, true, nil
}

// ReportAgentServerHealth records health the agent observed on its machine
// for one of its own servers.
func (s *Store) ReportAgentServerHealth(ctx context.Context, tenant, agentID, key, health string, at time.Time) error {
	if health != "healthy" && health != "unhealthy" {
		return errors.New("agent server health must be healthy or unhealthy")
	}
	_, err := s.ExecContext(ctx, `UPDATE targets SET health=?,agent_reported_at=?,updated_at=? WHERE tenant_id=? AND agent_id=? AND agent_server_key=?`, health, stamp(at), now(), tenant, agentID, key)
	return err
}

// MarkSilentAgentTargets marks agent-relayed targets unhealthy when their
// agent has not reported them since before. A silent agent's server is not
// known to be healthy.
func (s *Store) MarkSilentAgentTargets(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.ExecContext(ctx, `UPDATE targets SET health='unhealthy',updated_at=? WHERE agent_id IS NOT NULL AND health<>'unhealthy' AND (agent_reported_at IS NULL OR agent_reported_at<?)`, now(), stamp(before))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// RecordTargetMeasurements records one bounded engine snapshot for a target
// that no InferCrane deployment owns. Validation matches
// RecordOperationalMeasurements.
func (s *Store) RecordTargetMeasurements(ctx context.Context, tenant, targetID string, rows []domain.OperationalMeasurement) ([]domain.OperationalMeasurement, error) {
	if tenant == "" || targetID == "" {
		return nil, errors.New("tenant and target are required")
	}
	if len(rows) == 0 || len(rows) > 32 {
		return nil, errors.New("operational measurement batch must contain 1..32 rows")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM targets WHERE tenant_id=? AND id=?)`, tenant, targetID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	current := time.Now().UTC().Truncate(time.Microsecond)
	out := make([]domain.OperationalMeasurement, 0, len(rows))
	for _, row := range rows {
		row.ObservedAt = row.ObservedAt.UTC().Truncate(time.Microsecond)
		row.ValidUntil = row.ValidUntil.UTC().Truncate(time.Microsecond)
		row.TenantID = tenant
		if err = validateOperationalMeasurement(row, current); err != nil {
			return nil, err
		}
		if row.ID == "" {
			digest := sha256.Sum256([]byte(fmt.Sprintf("target\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%.17g", tenant, targetID, row.ReplicaID, row.Name, row.Source, row.ObservedAt.Format(time.RFC3339Nano), row.Value)))
			row.ID = hex.EncodeToString(digest[:16])
		}
		row.CreatedAt = current
		if _, err = tx.ExecContext(ctx, `INSERT INTO target_measurements(id,tenant_id,target_id,replica_id,name,value,unit,evidence_class,source,sample_count,observed_at,valid_until,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, row.ID, tenant, targetID, row.ReplicaID, row.Name, row.Value, row.Unit, row.EvidenceClass, row.Source, row.SampleCount, row.ObservedAt, row.ValidUntil, current); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// PurgeTargetMeasurements applies the high-volume evidence retention bound to
// target-scoped measurements.
func (s *Store) PurgeTargetMeasurements(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit < 1 {
		limit = 10000
	}
	result, err := s.ExecContext(ctx, `DELETE FROM target_measurements WHERE ctid IN (SELECT ctid FROM target_measurements WHERE observed_at<? ORDER BY observed_at LIMIT ?)`, before.UTC(), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// AdoptedCredential returns the sealed bearer credential stored for a
// directly connected adopted target, or ErrNotFound when it has none.
func (s *Store) AdoptedCredential(ctx context.Context, tenant, targetID string) (domain.AdoptedCredential, error) {
	var out domain.AdoptedCredential
	err := s.QueryRowContext(ctx, `SELECT a.tenant_id,e.name,a.target_id,a.upstream_credential_ciphertext,a.upstream_credential_nonce FROM adopted_workloads a JOIN endpoints e ON e.id=a.endpoint_id AND e.desired_state<>'deleted' WHERE a.tenant_id=? AND a.target_id=? AND a.upstream_credential_ciphertext IS NOT NULL ORDER BY a.updated_at DESC LIMIT 1`, tenant, targetID).Scan(&out.TenantID, &out.EndpointName, &out.TargetID, &out.Sealed.Ciphertext, &out.Sealed.Nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AdoptedCredential{}, ErrNotFound
	}
	return out, err
}
