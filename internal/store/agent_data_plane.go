package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/domain"
)

// Residency values an endpoint may hold.
const (
	ResidencyAny           = "any"
	ResidencyCustomerCloud = "customer-cloud"
)

// setEndpointResidencyTx sets residency on a locked endpoint. customer-cloud
// is refused while any binding is not a server an agent owns; new bindings
// are refused by the residency trigger, which reads the endpoint FOR SHARE,
// so the two cannot interleave.
func setEndpointResidencyTx(ctx context.Context, tx *tx, tenant, endpointID, residency, stamp string) error {
	if residency != ResidencyAny && residency != ResidencyCustomerCloud {
		return errors.New("residency must be any or customer-cloud")
	}
	if residency == ResidencyCustomerCloud {
		var offending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM backend_bindings b LEFT JOIN targets t ON t.id=b.target_id AND t.tenant_id=b.tenant_id WHERE b.tenant_id=? AND b.endpoint_id=? AND (b.kind<>'external' OR t.agent_id IS NULL)`, tenant, endpointID).Scan(&offending); err != nil {
			return err
		}
		if offending > 0 {
			return fmt.Errorf("%w: %d binding(s) are not servers an InferCrane agent owns; remove them first", ErrResidency, offending)
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE endpoints SET residency=?,updated_at=? WHERE tenant_id=? AND id=?`, residency, stamp, tenant, endpointID)
	return err
}

// SetEndpointResidency sets where an endpoint's request content may go.
func (s *Store) SetEndpointResidency(ctx context.Context, tenant, name, residency string) (domain.Endpoint, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.Endpoint{}, err
	}
	defer tx.Rollback()
	var endpointID string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM endpoints WHERE tenant_id=? AND name=? AND desired_state<>'deleted' FOR UPDATE`, tenant, name).Scan(&endpointID); errors.Is(err, sql.ErrNoRows) {
		return domain.Endpoint{}, ErrNotFound
	} else if err != nil {
		return domain.Endpoint{}, err
	}
	if err = setEndpointResidencyTx(ctx, tx, tenant, endpointID, residency, now()); err != nil {
		return domain.Endpoint{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Endpoint{}, err
	}
	return scanEndpoint(s.QueryRowContext(ctx, `SELECT id,tenant_id,logical_model_id,environment_id,name,desired_state,observed_state,COALESCE(active_serving_plan_id,''),COALESCE(candidate_serving_plan_id,''),residency,created_at,updated_at FROM endpoints WHERE tenant_id=? AND id=?`, tenant, endpointID))
}

// EndpointResidency returns the residency of a live endpoint by name.
func (s *Store) EndpointResidency(ctx context.Context, tenant, name string) (string, error) {
	var residency string
	err := s.QueryRowContext(ctx, `SELECT residency FROM endpoints WHERE tenant_id=? AND name=? AND desired_state<>'deleted'`, tenant, name).Scan(&residency)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return residency, err
}

// AgentDataPlaneBindings lists the active-plan bindings an agent serves: the
// agent-managed bindings of serving endpoints whose target this agent owns.
func (s *Store) AgentDataPlaneBindings(ctx context.Context, tenant, agentID string) ([]domain.AgentDataPlaneBinding, error) {
	rows, err := s.QueryContext(ctx, `SELECT e.id,e.name,e.logical_model_id,e.environment_id,p.id,p.routing_policy,b.id,t.id,t.agent_server_key,COALESCE(t.upstream_model_name,''),t.runtime,t.health,spb.priority,spb.weight,(r.id IS NOT NULL)
FROM endpoints e
JOIN serving_plans p ON p.id=e.active_serving_plan_id AND p.tenant_id=e.tenant_id
JOIN serving_plan_bindings spb ON spb.serving_plan_id=p.id
JOIN backend_bindings b ON b.id=spb.binding_id AND b.tenant_id=e.tenant_id AND b.ownership_mode='agent-managed'
JOIN targets t ON t.id=b.target_id AND t.tenant_id=e.tenant_id
LEFT JOIN agent_rollout_servers r ON r.binding_id=b.id
WHERE e.tenant_id=? AND e.desired_state='serving' AND e.residency='customer-cloud' AND t.agent_id=?
ORDER BY e.name,spb.priority LIMIT 512`, tenant, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AgentDataPlaneBinding{}
	for rows.Next() {
		var item domain.AgentDataPlaneBinding
		if err = rows.Scan(&item.EndpointID, &item.EndpointName, &item.LogicalModelID, &item.EnvironmentID, &item.ServingPlanID, &item.RoutingPolicy, &item.BindingID, &item.TargetID, &item.ServerKey, &item.UpstreamModel, &item.Runtime, &item.Health, &item.Priority, &item.Weight, &item.Rollout); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// AgentDataPlaneCredentials returns the tenant's live keys that are not agent
// keys, as stored: SHA-256 digests only. The relay narrows them to keys that
// may invoke the agent's endpoints.
func (s *Store) AgentDataPlaneCredentials(ctx context.Context, tenant string) ([]domain.CredentialRecord, error) {
	rows, err := s.QueryContext(ctx, `SELECT credential_hash,id,tenant_id,name,role,kind,scopes_json::text,endpoint_names_json::text,expires_at FROM principals WHERE tenant_id=? AND disabled=FALSE AND kind<>'agent' AND (expires_at IS NULL OR expires_at>NOW()) ORDER BY id LIMIT 10001`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.CredentialRecord{}
	for rows.Next() {
		var record domain.CredentialRecord
		var scopes, endpoints string
		var expires sql.NullString
		if err = rows.Scan(&record.Hash, &record.Principal.ID, &record.Principal.TenantID, &record.Principal.Name, &record.Principal.Role, &record.Principal.Kind, &scopes, &endpoints, &expires); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(scopes), &record.Principal.Scopes); err != nil {
			return nil, fmt.Errorf("decode principal scopes: %w", err)
		}
		if err = json.Unmarshal([]byte(endpoints), &record.Principal.EndpointNames); err != nil {
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

// AdmissionPoliciesForTenant returns one tenant's endpoint admission policies,
// keyed like AdmissionPolicies.
func (s *Store) AdmissionPoliciesForTenant(ctx context.Context, tenant string) ([]admission.Policy, error) {
	rows, err := s.QueryContext(ctx, `SELECT p.tenant_id,e.name,p.max_concurrency,p.max_queue_depth,p.queue_timeout_ms,p.request_timeout_ms,p.max_request_bytes,p.max_output_tokens,p.allowed_priorities_json::text,p.retry_budget,p.enabled FROM endpoint_admission_policies p JOIN endpoints e ON e.id=p.endpoint_id WHERE e.tenant_id=? AND e.desired_state='serving'`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAdmissionPolicies(rows)
}

// RequestQuotaLimit returns a tenant's hard request rate, if it has one.
func (s *Store) RequestQuotaLimit(ctx context.Context, tenant string) (int, bool, error) {
	var limit int
	err := s.QueryRowContext(ctx, `SELECT max_requests_per_minute FROM tenant_quotas WHERE tenant_id=?`, tenant).Scan(&limit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return limit, err == nil, err
}

const agentCommandColumns = `id,tenant_id,agent_id,kind,COALESCE(idempotency_key,''),state,COALESCE(phase,''),spec_json::text,COALESCE(result_json::text,''),COALESCE(failure_code,''),COALESCE(failure_detail,''),attempts,lease_expires_at,deadline_at,created_by,created_at,updated_at,started_at,finished_at`

func scanAgentCommand(row rowScanner) (domain.AgentCommand, error) {
	var out domain.AgentCommand
	var lease, started, finished sql.NullString
	var deadline, created, updated string
	if err := row.Scan(&out.ID, &out.TenantID, &out.AgentID, &out.Kind, &out.IdempotencyKey, &out.State, &out.Phase, &out.SpecJSON, &out.ResultJSON, &out.FailureCode, &out.FailureDetail, &out.Attempts, &lease, &deadline, &out.CreatedBy, &created, &updated, &started, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AgentCommand{}, ErrNotFound
		}
		return domain.AgentCommand{}, err
	}
	out.DeadlineAt, out.CreatedAt, out.UpdatedAt = parseTime(deadline), parseTime(created), parseTime(updated)
	for target, value := range map[**time.Time]sql.NullString{&out.LeaseExpiresAt: lease, &out.StartedAt: started, &out.FinishedAt: finished} {
		if value.Valid {
			parsed := parseTime(value.String)
			*target = &parsed
		}
	}
	return out, nil
}

// MaxQueuedAgentCommands bounds the commands waiting for or running on one
// agent, so a burst of requests cannot queue unbounded work.
const MaxQueuedAgentCommands = 16

// EnqueueAgentCommand queues one command for an active agent.
func (s *Store) EnqueueAgentCommand(ctx context.Context, command domain.AgentCommand) (domain.AgentCommand, error) {
	if command.TenantID == "" || command.AgentID == "" || command.Kind == "" || command.SpecJSON == "" || command.CreatedBy == "" || command.DeadlineAt.IsZero() {
		return domain.AgentCommand{}, errors.New("tenant, agent, kind, spec, creator, and deadline are required")
	}
	if !validBoundedJSON(command.SpecJSON, 64<<10) {
		return domain.AgentCommand{}, errors.New("command spec must be bounded JSON")
	}
	if len(command.IdempotencyKey) > 128 {
		return domain.AgentCommand{}, errors.New("command idempotency key must be at most 128 bytes")
	}
	id, err := newID()
	if err != nil {
		return domain.AgentCommand{}, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.AgentCommand{}, err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM agents WHERE tenant_id=? AND id=? FOR UPDATE`, command.TenantID, command.AgentID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return domain.AgentCommand{}, ErrNotFound
	} else if err != nil {
		return domain.AgentCommand{}, err
	}
	// Work queued before under the same key is that command, whatever state
	// it is in; the agent row lock serializes two callers with one key.
	if command.IdempotencyKey != "" {
		var existing, agentID, kind string
		err = tx.QueryRowContext(ctx, `SELECT id,agent_id,kind FROM agent_commands WHERE tenant_id=? AND idempotency_key=?`, command.TenantID, command.IdempotencyKey).Scan(&existing, &agentID, &kind)
		if err == nil {
			if agentID != command.AgentID || kind != command.Kind {
				return domain.AgentCommand{}, fmt.Errorf("%w: the idempotency key names another agent's or another kind's command", ErrConflict)
			}
			return s.AgentCommand(ctx, command.TenantID, command.AgentID, existing)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return domain.AgentCommand{}, err
		}
	}
	if state != "active" {
		return domain.AgentCommand{}, fmt.Errorf("%w: the agent is revoked", ErrConflict)
	}
	var open int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_commands WHERE agent_id=? AND state IN ('queued','running')`, command.AgentID).Scan(&open); err != nil {
		return domain.AgentCommand{}, err
	}
	if open >= MaxQueuedAgentCommands {
		return domain.AgentCommand{}, fmt.Errorf("%w: agent already has %d queued or running commands", ErrConflict, open)
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_commands(id,tenant_id,agent_id,kind,state,spec_json,deadline_at,created_by,created_at,updated_at,idempotency_key) VALUES(?,?,?,?,'queued',?::jsonb,?,?,?,?,NULLIF(?,''))`,
		id, command.TenantID, command.AgentID, command.Kind, command.SpecJSON, command.DeadlineAt.UTC(), command.CreatedBy, stamp, stamp, command.IdempotencyKey); err != nil {
		return domain.AgentCommand{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.AgentCommand{}, err
	}
	return s.AgentCommand(ctx, command.TenantID, command.AgentID, id)
}

// AgentCommand reads one command of one agent.
func (s *Store) AgentCommand(ctx context.Context, tenant, agentID, id string) (domain.AgentCommand, error) {
	return scanAgentCommand(s.QueryRowContext(ctx, `SELECT `+agentCommandColumns+` FROM agent_commands WHERE tenant_id=? AND agent_id=? AND id=?`, tenant, agentID, id))
}

// AgentCommands lists an agent's newest commands.
func (s *Store) AgentCommands(ctx context.Context, tenant, agentID string, limit int) ([]domain.AgentCommand, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.QueryContext(ctx, `SELECT `+agentCommandColumns+` FROM agent_commands WHERE tenant_id=? AND agent_id=? ORDER BY created_at DESC LIMIT ?`, tenant, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AgentCommand{}
	for rows.Next() {
		item, scanErr := scanAgentCommand(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// maxCommandAttempts bounds how often a command whose agent stopped
// reporting is handed out again.
const maxCommandAttempts = 3

// LeaseAgentCommands hands an agent its next queued commands, and again a
// running command whose lease expired (the agent restarted mid-run). Work
// past its deadline expires instead. Leases use SKIP LOCKED, so two control
// plane replicas answering the same agent never hand out one command twice.
func (s *Store) LeaseAgentCommands(ctx context.Context, tenant, agentID string, at time.Time, lease time.Duration, limit int) ([]domain.AgentCommand, error) {
	if limit < 1 {
		limit = 1
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current := stamp(at)
	if _, err = tx.ExecContext(ctx, `UPDATE agent_commands SET state='expired',failure_code='deadline_exceeded',failure_detail='the command did not finish before its deadline',lease_expires_at=NULL,finished_at=?,updated_at=? WHERE tenant_id=? AND agent_id=? AND state IN ('queued','running') AND deadline_at<?`, current, current, tenant, agentID, current); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_commands SET state='failed',failure_code='agent_stopped_reporting',failure_detail='the agent stopped reporting on this command too many times',lease_expires_at=NULL,finished_at=?,updated_at=? WHERE tenant_id=? AND agent_id=? AND state='running' AND lease_expires_at<? AND attempts>=?`, current, current, tenant, agentID, current, maxCommandAttempts); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM agent_commands WHERE tenant_id=? AND agent_id=? AND (state='queued' OR (state='running' AND lease_expires_at<?)) ORDER BY created_at LIMIT ? FOR UPDATE SKIP LOCKED`, tenant, agentID, current, limit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	until := stamp(at.Add(lease))
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE agent_commands SET state='running',phase='leased',attempts=attempts+1,lease_expires_at=?,started_at=COALESCE(started_at,?),updated_at=? WHERE id=?`, until, current, current, id); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	out := make([]domain.AgentCommand, 0, len(ids))
	for _, id := range ids {
		item, readErr := s.AgentCommand(ctx, tenant, agentID, id)
		if readErr != nil {
			return nil, readErr
		}
		out = append(out, item)
	}
	return out, nil
}

// UpdateAgentCommand applies an agent's report to a command it leased. A
// command that is no longer running (cancelled, expired, finished) is
// returned unchanged, and the agent stops working on it.
func (s *Store) UpdateAgentCommand(ctx context.Context, tenant, agentID, id string, update domain.AgentCommandUpdate) (domain.AgentCommand, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.AgentCommand{}, err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM agent_commands WHERE tenant_id=? AND agent_id=? AND id=? FOR UPDATE`, tenant, agentID, id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return domain.AgentCommand{}, ErrNotFound
	} else if err != nil {
		return domain.AgentCommand{}, err
	}
	if state == "running" {
		current := now()
		switch update.State {
		case "running":
			lease := update.Lease
			if lease <= 0 {
				lease = 10 * time.Minute
			}
			_, err = tx.ExecContext(ctx, `UPDATE agent_commands SET phase=?,lease_expires_at=?,updated_at=? WHERE id=?`, null(update.Phase), stamp(time.Now().Add(lease)), current, id)
		case "succeeded":
			if !validBoundedJSON(update.ResultJSON, 256<<10) || update.ResultJSON == "" {
				return domain.AgentCommand{}, errors.New("a succeeded command needs a bounded JSON result")
			}
			_, err = tx.ExecContext(ctx, `UPDATE agent_commands SET state='succeeded',phase=?,result_json=?::jsonb,lease_expires_at=NULL,finished_at=?,updated_at=? WHERE id=?`, null(update.Phase), update.ResultJSON, current, current, id)
		case "failed":
			var result any
			if update.ResultJSON != "" {
				if !validBoundedJSON(update.ResultJSON, 256<<10) {
					return domain.AgentCommand{}, errors.New("command result must be bounded JSON")
				}
				result = update.ResultJSON
			}
			_, err = tx.ExecContext(ctx, `UPDATE agent_commands SET state='failed',phase=?,result_json=?::jsonb,failure_code=?,failure_detail=?,lease_expires_at=NULL,finished_at=?,updated_at=? WHERE id=?`, null(update.Phase), result, update.FailureCode, null(update.FailureDetail), current, current, id)
		default:
			return domain.AgentCommand{}, errors.New("command state must be running, succeeded, or failed")
		}
		if err != nil {
			return domain.AgentCommand{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.AgentCommand{}, err
	}
	return s.AgentCommand(ctx, tenant, agentID, id)
}

// CancelAgentCommand cancels a queued or running command. The agent learns
// it on its next report and removes what it started.
func (s *Store) CancelAgentCommand(ctx context.Context, tenant, agentID, id string) (domain.AgentCommand, error) {
	current := now()
	if _, err := s.ExecContext(ctx, `UPDATE agent_commands SET state='cancelled',lease_expires_at=NULL,finished_at=?,updated_at=? WHERE tenant_id=? AND agent_id=? AND id=? AND state IN ('queued','running')`, current, current, tenant, agentID, id); err != nil {
		return domain.AgentCommand{}, err
	}
	return s.AgentCommand(ctx, tenant, agentID, id)
}
