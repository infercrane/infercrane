package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/infercrane/infercrane/internal/autoscale"
)

func (s *Store) SetScalingPolicy(ctx context.Context, deploymentID string, policy autoscale.Policy) error {
	_, err := s.ExecContext(ctx, `INSERT INTO scaling_policies(deployment_id,enabled,min_replicas,max_replicas,queue_threshold,low_load_threshold,scale_up_intervals,scale_down_intervals,cooldown_seconds,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(deployment_id) DO UPDATE SET enabled=EXCLUDED.enabled,min_replicas=EXCLUDED.min_replicas,max_replicas=EXCLUDED.max_replicas,queue_threshold=EXCLUDED.queue_threshold,low_load_threshold=EXCLUDED.low_load_threshold,scale_up_intervals=EXCLUDED.scale_up_intervals,scale_down_intervals=EXCLUDED.scale_down_intervals,cooldown_seconds=EXCLUDED.cooldown_seconds,updated_at=EXCLUDED.updated_at`, deploymentID, policy.Enabled, policy.MinReplicas, policy.MaxReplicas, policy.QueueThreshold, policy.LowLoadThreshold, policy.ScaleUpIntervals, policy.ScaleDownIntervals, int(policy.Cooldown.Seconds()), now())
	return err
}

func (s *Store) AutoscalingDeployments(ctx context.Context) ([]autoscale.Deployment, error) {
	rows, err := s.QueryContext(ctx, `SELECT d.id,p.enabled,p.min_replicas,p.max_replicas,p.queue_threshold,p.low_load_threshold,p.scale_up_intervals,p.scale_down_intervals,p.cooldown_seconds,COUNT(dt.target_id),COALESCE(st.consecutive_high,0),COALESCE(st.consecutive_low,0),st.last_scaled_at FROM deployments d JOIN scaling_policies p ON p.deployment_id=d.id LEFT JOIN deployment_targets dt ON dt.deployment_id=d.id LEFT JOIN autoscaling_state st ON st.deployment_id=d.id WHERE d.desired_state='running' AND p.enabled=TRUE GROUP BY d.id,p.deployment_id,st.deployment_id`)
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

func (s *Store) SaveState(ctx context.Context, deploymentID string, state autoscale.State) error {
	var last any
	if !state.LastScaledAt.IsZero() {
		last = state.LastScaledAt.UTC()
	}
	_, err := s.ExecContext(ctx, `INSERT INTO autoscaling_state(deployment_id,consecutive_high,consecutive_low,last_scaled_at,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(deployment_id) DO UPDATE SET consecutive_high=EXCLUDED.consecutive_high,consecutive_low=EXCLUDED.consecutive_low,last_scaled_at=EXCLUDED.last_scaled_at,updated_at=EXCLUDED.updated_at`, deploymentID, state.ConsecutiveHigh, state.ConsecutiveLow, last, now())
	return err
}
