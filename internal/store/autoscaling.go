package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/autoscale"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/workflows"
)

func (s *Store) SetScalingPolicy(ctx context.Context, deploymentID string, policy autoscale.Policy) error {
	if _, err := s.ExecContext(ctx, `INSERT INTO scaling_policies(deployment_id,enabled,min_replicas,max_replicas,queue_threshold,low_load_threshold,scale_up_intervals,scale_down_intervals,cooldown_seconds,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(deployment_id) DO UPDATE SET enabled=EXCLUDED.enabled,min_replicas=EXCLUDED.min_replicas,max_replicas=EXCLUDED.max_replicas,queue_threshold=EXCLUDED.queue_threshold,low_load_threshold=EXCLUDED.low_load_threshold,scale_up_intervals=EXCLUDED.scale_up_intervals,scale_down_intervals=EXCLUDED.scale_down_intervals,cooldown_seconds=EXCLUDED.cooldown_seconds,updated_at=EXCLUDED.updated_at`, deploymentID, policy.Enabled, policy.MinReplicas, policy.MaxReplicas, policy.QueueThreshold, policy.LowLoadThreshold, policy.ScaleUpIntervals, policy.ScaleDownIntervals, int(policy.Cooldown.Seconds()), now()); err != nil {
		return err
	}
	_, err := s.ExecContext(ctx, `INSERT INTO autoscaling_state(deployment_id,consecutive_high,consecutive_low,desired_replicas,updated_at) VALUES(?,0,0,?,?) ON CONFLICT(deployment_id) DO NOTHING`, deploymentID, policy.MinReplicas, now())
	return err
}

// ScaleTo atomically records desired capacity and queues one durable scale
// operation. An unresolved operation suppresses duplicate provider work.
func (s *Store) ScaleTo(ctx context.Context, deploymentID string, desired int) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var tenant, name, model, sourceJSON string
	var minReplicas, maxReplicas, current int
	err = tx.QueryRowContext(ctx, `SELECT tenant_id,name,model,min_replicas,max_replicas,(SELECT COUNT(*) FROM replicas r WHERE r.deployment_id=d.id AND r.lifecycle_state!='deleted') FROM deployments d WHERE id=? AND desired_state='running' FOR UPDATE`, deploymentID).Scan(&tenant, &name, &model, &minReplicas, &maxReplicas, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if desired < minReplicas || desired > maxReplicas {
		return fmt.Errorf("desired replicas %d outside %d..%d", desired, minReplicas, maxReplicas)
	}
	var existingDesired int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE((request_json->>'desired_replicas')::integer,0) FROM operations WHERE resource_type='deployment' AND resource_name=? AND tenant_id=? AND kind=? AND status IN ('pending','running','retry_wait') ORDER BY created_at DESC LIMIT 1`, name, tenant, workflows.ScaleKind).Scan(&existingDesired)
	if err == nil {
		if existingDesired == desired {
			return tx.Commit()
		}
		return fmt.Errorf("%w: another scaling operation is in progress", ErrConflict)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT request_json::text FROM operations WHERE resource_type='deployment' AND resource_name=? AND tenant_id=? AND kind IN (?,?) ORDER BY created_at DESC LIMIT 1`, name, tenant, workflows.ScaleKind, workflows.ConvergeKind).Scan(&sourceJSON); errors.Is(err, sql.ErrNoRows) {
		return errors.New("cloud provisioning configuration is unavailable")
	} else if err != nil {
		return err
	}
	var request workflows.CloudRequest
	if err = json.Unmarshal([]byte(sourceJSON), &request); err != nil {
		return fmt.Errorf("decode cloud configuration: %w", err)
	}
	request.DeploymentID, request.TenantID, request.Name, request.Model = deploymentID, tenant, name, model
	request.DesiredReplicas, request.PreviousReplicas = desired, current
	request.MinReplicas, request.MaxReplicas = minReplicas, maxReplicas
	encoded, _ := json.Marshal(request)
	var generation int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO autoscaling_state(deployment_id,consecutive_high,consecutive_low,desired_replicas,scale_generation,updated_at) VALUES(?,0,0,?,1,?) ON CONFLICT(deployment_id) DO UPDATE SET desired_replicas=EXCLUDED.desired_replicas,scale_generation=autoscaling_state.scale_generation+1,updated_at=EXCLUDED.updated_at RETURNING scale_generation`, deploymentID, desired, now()).Scan(&generation); err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	stamp := now()
	key := fmt.Sprintf("autoscale-%s-%d", deploymentID, generation)
	if _, err = tx.ExecContext(ctx, `INSERT INTO operations(id,tenant_id,kind,resource_type,resource_name,idempotency_key,status,progress,message,request_json,result_json,attempt,max_attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,'pending',0,'queued',?::jsonb,'{}'::jsonb,0,120,?,?,?)`, id, tenant, workflows.ScaleKind, "deployment", name, key, string(encoded), stamp, stamp, stamp); err != nil {
		return err
	}
	eventID, err := newID()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]int{"old_replicas": current, "new_replicas": desired})
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_events(id,deployment_id,event_type,summary,payload_json,created_at) VALUES(?,?, 'scaling_queued', ?, ?::jsonb,?)`, eventID, deploymentID, fmt.Sprintf("Scaling queued %d -> %d", current, desired), string(payload), stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AutoscalingDeployments(ctx context.Context) ([]autoscale.Deployment, error) {
	rows, err := s.QueryContext(ctx, `SELECT d.id,p.enabled,p.min_replicas,p.max_replicas,p.queue_threshold,p.low_load_threshold,p.scale_up_intervals,p.scale_down_intervals,p.cooldown_seconds,COUNT(dt.target_id),COALESCE(st.consecutive_high,0),COALESCE(st.consecutive_low,0),st.last_scaled_at FROM deployments d JOIN scaling_policies p ON p.deployment_id=d.id LEFT JOIN deployment_targets dt ON dt.deployment_id=d.id LEFT JOIN autoscaling_state st ON st.deployment_id=d.id WHERE d.desired_state='running' AND d.observed_state IN ('healthy','degraded') AND p.enabled=TRUE GROUP BY d.id,p.deployment_id,st.deployment_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []autoscale.Deployment
	for rows.Next() {
		var d autoscale.Deployment
		var cooldown int
		var last sql.NullTime
		if err := rows.Scan(&d.ID, &d.Policy.Enabled, &d.Policy.MinReplicas, &d.Policy.MaxReplicas, &d.Policy.QueueThreshold, &d.Policy.LowLoadThreshold, &d.Policy.ScaleUpIntervals, &d.Policy.ScaleDownIntervals, &cooldown, &d.State.Replicas, &d.State.ConsecutiveHigh, &d.State.ConsecutiveLow, &last); err != nil {
			return nil, err
		}
		d.Policy.Cooldown = time.Duration(cooldown) * time.Second
		if last.Valid {
			d.State.LastScaledAt = last.Time
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) AutoscalingTargetURLs(ctx context.Context, deploymentID string) ([]string, error) {
	rows, err := s.QueryContext(ctx, `SELECT t.url FROM targets t JOIN deployment_targets dt ON dt.target_id=t.id WHERE dt.deployment_id=? AND t.health='healthy' ORDER BY t.name`, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var urls []string
	for rows.Next() {
		var targetURL string
		if err = rows.Scan(&targetURL); err != nil {
			return nil, err
		}
		urls = append(urls, targetURL)
	}
	return urls, rows.Err()
}

func (s *Store) RecordDecision(ctx context.Context, deploymentID string, decision autoscale.Decision, signals string) error {
	id, err := newID()
	if err != nil {
		return err
	}
	if !json.Valid([]byte(signals)) {
		signals = "{}"
	}
	_, err = s.ExecContext(ctx, `INSERT INTO scaling_decisions(id,deployment_id,action,old_replicas,new_replicas,reason,signals_json,created_at) VALUES(?,?,?,?,?,?,?::jsonb,?)`, id, deploymentID, decision.Action, decision.OldReplicas, decision.NewReplicas, decision.Reason, signals, now())
	return err
}

func (s *Store) ScalingDecisionsForTenant(ctx context.Context, tenant, deploymentName string, limit int) ([]domain.ScalingDecision, error) {
	resolved, err := s.ResolveForTenant(ctx, tenant, deploymentName)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx, `SELECT id,deployment_id,action,old_replicas,new_replicas,reason,signals_json::text,created_at FROM scaling_decisions WHERE deployment_id=? ORDER BY created_at DESC LIMIT ?`, resolved.Deployment.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ScalingDecision
	for rows.Next() {
		var decision domain.ScalingDecision
		if err = rows.Scan(&decision.ID, &decision.DeploymentID, &decision.Action, &decision.OldReplicas, &decision.NewReplicas, &decision.Reason, &decision.SignalsJSON, &decision.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, decision)
	}
	return out, rows.Err()
}

func (s *Store) SaveState(ctx context.Context, deploymentID string, state autoscale.State) error {
	var last any
	if !state.LastScaledAt.IsZero() {
		last = state.LastScaledAt.UTC()
	}
	_, err := s.ExecContext(ctx, `INSERT INTO autoscaling_state(deployment_id,consecutive_high,consecutive_low,last_scaled_at,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(deployment_id) DO UPDATE SET consecutive_high=EXCLUDED.consecutive_high,consecutive_low=EXCLUDED.consecutive_low,last_scaled_at=EXCLUDED.last_scaled_at,updated_at=EXCLUDED.updated_at`, deploymentID, state.ConsecutiveHigh, state.ConsecutiveLow, last, now())
	return err
}
