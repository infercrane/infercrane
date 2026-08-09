package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) Targets(ctx context.Context) ([]domain.Target, error) {
	return s.TargetsForTenant(ctx, "global")
}
func (s *Store) TargetsForTenant(ctx context.Context, tenant string) ([]domain.Target, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,name,url,provider,runtime,COALESCE(upstream_model_name,''),health,COALESCE(provider_resource_id,''),COALESCE(provider_details_json::text,''),created_at,updated_at FROM targets WHERE tenant_id=? ORDER BY name`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Target
	for rows.Next() {
		var target domain.Target
		var created, updated string
		if err := rows.Scan(&target.ID, &target.Name, &target.URL, &target.Provider, &target.Runtime, &target.UpstreamModel, &target.Health, &target.ProviderResourceID, &target.ProviderDetails, &created, &updated); err != nil {
			return nil, err
		}
		target.CreatedAt, target.UpdatedAt = parseTime(created), parseTime(updated)
		result = append(result, target)
	}
	return result, rows.Err()
}

func (s *Store) CreateDeployment(ctx context.Context, deployment domain.Deployment, targetNames []string) (domain.Deployment, error) {
	return s.CreateDeploymentForTenant(ctx, "global", deployment, targetNames)
}
func (s *Store) CreateDeploymentForTenant(ctx context.Context, tenant string, deployment domain.Deployment, targetNames []string) (domain.Deployment, error) {
	if len(targetNames) == 0 {
		return domain.Deployment{}, fmt.Errorf("at least one target is required")
	}
	if existing, err := s.ResolveForTenant(ctx, tenant, deployment.Name); err == nil {
		if existing.Deployment.Model == deployment.Model && sameTargets(existing.Targets, targetNames) {
			return existing.Deployment, nil
		}
		return domain.Deployment{}, fmt.Errorf("%w: deployment already exists with different configuration", ErrConflict)
	} else if !errors.Is(err, ErrNotFound) {
		return domain.Deployment{}, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.Deployment{}, err
	}
	defer tx.Rollback()
	id, err := newID()
	if err != nil {
		return domain.Deployment{}, err
	}
	deployment.ID = id
	deployment.TenantID = tenant
	deployment.Runtime = "vllm"
	deployment.DesiredState = "running"
	deployment.ObservedState = "pending"
	if deployment.RoutingStrategy == "" {
		deployment.RoutingStrategy = "round-robin"
	}
	if deployment.MinReplicas == 0 {
		deployment.MinReplicas = len(targetNames)
	}
	if deployment.MaxReplicas == 0 {
		deployment.MaxReplicas = deployment.MinReplicas
	}
	if err = enforceDeploymentQuota(ctx, tx, tenant, "", deployment.MaxReplicas, true); err != nil {
		return domain.Deployment{}, err
	}
	stamp := now()
	deployment.CreatedAt, deployment.UpdatedAt = parseTime(stamp), parseTime(stamp)
	_, err = tx.ExecContext(ctx, `INSERT INTO deployments(id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,tenant_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, deployment.ID, deployment.Name, deployment.Model, deployment.Runtime, deployment.RoutingStrategy, deployment.DesiredState, deployment.ObservedState, deployment.MinReplicas, deployment.MaxReplicas, deployment.AutoscalingEnabled, tenant, stamp, stamp)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.Deployment{}, fmt.Errorf("%w: deployment already exists", ErrConflict)
		}
		return domain.Deployment{}, err
	}
	seen := make(map[string]struct{}, len(targetNames))
	var upstreamModel string
	for _, name := range targetNames {
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		var id, runtime string
		var targetModel sql.NullString
		if err = tx.QueryRowContext(ctx, `SELECT id,runtime,upstream_model_name FROM targets WHERE name=? AND tenant_id=?`, name, tenant).Scan(&id, &runtime, &targetModel); errors.Is(err, sql.ErrNoRows) {
			return domain.Deployment{}, fmt.Errorf("%w: target %s", ErrNotFound, name)
		} else if err != nil {
			return domain.Deployment{}, err
		}
		if runtime != deployment.Runtime {
			return domain.Deployment{}, fmt.Errorf("target %s runtime mismatch", name)
		}
		model := deployment.Model
		if targetModel.Valid {
			model = targetModel.String
		}
		if upstreamModel != "" && upstreamModel != model {
			return domain.Deployment{}, errors.New("all targets must expose one common upstream model")
		}
		upstreamModel = model
		if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_targets(deployment_id,target_id) VALUES(?,?)`, deployment.ID, id); err != nil {
			return domain.Deployment{}, err
		}
	}
	eventID, err := newID()
	if err != nil {
		return domain.Deployment{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO deployment_events(id,deployment_id,event_type,summary,payload_json,created_at) VALUES(?,?,?,?,?::jsonb,?)`, eventID, deployment.ID, "deployment_created", "Deployment "+deployment.Name+" created", "{}", stamp)
	if err != nil {
		return domain.Deployment{}, err
	}
	return deployment, tx.Commit()
}

func sameTargets(targets []domain.Target, names []string) bool {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	if len(targets) != len(set) {
		return false
	}
	for _, t := range targets {
		if _, ok := set[t.Name]; !ok {
			return false
		}
	}
	return true
}

func (s *Store) Deployments(ctx context.Context) ([]domain.Deployment, error) {
	return s.DeploymentsForTenant(ctx, "")
}
func (s *Store) DeploymentsForTenant(ctx context.Context, tenant string) ([]domain.Deployment, error) {
	query := `SELECT id,tenant_id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,created_at,updated_at FROM deployments WHERE desired_state!='deleted'`
	var args []any
	if tenant != "" {
		query += ` AND tenant_id=?`
		args = append(args, tenant)
	}
	query += ` ORDER BY name`
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Deployment
	for rows.Next() {
		var d domain.Deployment
		var created, updated string
		if err := rows.Scan(&d.ID, &d.TenantID, &d.Name, &d.Model, &d.Runtime, &d.RoutingStrategy, &d.DesiredState, &d.ObservedState, &d.MinReplicas, &d.MaxReplicas, &d.AutoscalingEnabled, &created, &updated); err != nil {
			return nil, err
		}
		d.CreatedAt, d.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) Resolve(ctx context.Context, name string) (domain.ResolvedDeployment, error) {
	return s.ResolveForTenant(ctx, "global", name)
}
func (s *Store) ResolveForTenant(ctx context.Context, tenant, name string) (domain.ResolvedDeployment, error) {
	var out domain.ResolvedDeployment
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,created_at,updated_at FROM deployments WHERE tenant_id=? AND name=? AND desired_state='running'`, tenant, name).Scan(&out.Deployment.ID, &out.Deployment.TenantID, &out.Deployment.Name, &out.Deployment.Model, &out.Deployment.Runtime, &out.Deployment.RoutingStrategy, &out.Deployment.DesiredState, &out.Deployment.ObservedState, &out.Deployment.MinReplicas, &out.Deployment.MaxReplicas, &out.Deployment.AutoscalingEnabled, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("%w: deployment %s", ErrNotFound, name)
	}
	if err != nil {
		return out, err
	}
	out.Deployment.CreatedAt, out.Deployment.UpdatedAt = parseTime(created), parseTime(updated)
	rows, err := s.QueryContext(ctx, `SELECT t.id,t.name,t.url,t.provider,t.runtime,COALESCE(t.upstream_model_name,''),t.health,COALESCE(t.provider_resource_id,''),COALESCE(t.provider_details_json::text,''),t.created_at,t.updated_at FROM targets t JOIN deployment_targets dt ON dt.target_id=t.id WHERE dt.deployment_id=? ORDER BY t.name`, out.Deployment.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var t domain.Target
		var targetCreated, targetUpdated string
		if err := rows.Scan(&t.ID, &t.Name, &t.URL, &t.Provider, &t.Runtime, &t.UpstreamModel, &t.Health, &t.ProviderResourceID, &t.ProviderDetails, &targetCreated, &targetUpdated); err != nil {
			return out, err
		}
		t.CreatedAt, t.UpdatedAt = parseTime(targetCreated), parseTime(targetUpdated)
		out.Targets = append(out.Targets, t)
	}
	return out, rows.Err()
}

func (s *Store) SetRoute(ctx context.Context, name, strategy string) error {
	if _, ok := domain.RoutingStrategies[strategy]; !ok {
		return fmt.Errorf("unsupported routing strategy %q", strategy)
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var deploymentID, current string
	if err := tx.QueryRowContext(ctx, `SELECT id,routing_strategy FROM deployments WHERE name=? AND desired_state='running'`, name).Scan(&deploymentID, &current); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if current == strategy {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE deployments SET routing_strategy=?,updated_at=? WHERE id=?`, strategy, now(), deploymentID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	id, err := newID()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_events(id,deployment_id,event_type,summary,payload_json,created_at) VALUES(?,?,?,?,?::jsonb,?)`, id, deploymentID, "routing_changed", "Routing changed to "+strategy, "{}", now()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) DeleteDeployment(ctx context.Context, name string) error {
	return s.DeleteDeploymentForTenant(ctx, "global", name)
}
func (s *Store) DeleteDeploymentForTenant(ctx context.Context, tenant, name string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var id, state string
	if err := tx.QueryRowContext(ctx, `SELECT id,desired_state FROM deployments WHERE tenant_id=? AND name=?`, tenant, name).Scan(&id, &state); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if state == "deleted" {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deployments SET desired_state='deleted',observed_state='deleted',updated_at=? WHERE id=?`, now(), id); err != nil {
		return err
	}
	eventID, err := newID()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deployment_events(id,deployment_id,event_type,summary,payload_json,created_at) VALUES(?,?,?,?,?::jsonb,?)`, eventID, id, "deployment_deleted", "Deployment "+name+" deleted", "{}", now()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) SetTargetHealth(ctx context.Context, id, health string) error {
	_, err := s.ExecContext(ctx, `UPDATE targets SET health=?,updated_at=? WHERE id=?`, health, now(), id)
	return err
}
func (s *Store) SetDeploymentState(ctx context.Context, id, state string) error {
	_, err := s.ExecContext(ctx, `UPDATE deployments SET observed_state=?,updated_at=? WHERE id=?`, state, now(), id)
	return err
}
func (s *Store) Event(ctx context.Context, deploymentID, targetID, eventType, summary, payload string) error {
	if payload == "" {
		payload = "{}"
	}
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = s.ExecContext(ctx, `INSERT INTO deployment_events(id,deployment_id,target_id,event_type,summary,payload_json,created_at) VALUES(?,?,?,?,?,?::jsonb,?)`, id, null(deploymentID), null(targetID), eventType, summary, payload, now())
	return err
}
func (s *Store) Events(ctx context.Context, name string) ([]domain.Event, error) {
	resolved, err := s.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	rows, err := s.QueryContext(ctx, `SELECT id,COALESCE(deployment_id,''),COALESCE(target_id,''),event_type,summary,payload_json::text,created_at FROM deployment_events WHERE deployment_id=? ORDER BY created_at DESC`, resolved.Deployment.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var e domain.Event
		var stamp string
		if err := rows.Scan(&e.ID, &e.DeploymentID, &e.TargetID, &e.Type, &e.Summary, &e.Payload, &stamp); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(stamp)
		out = append(out, e)
	}
	return out, rows.Err()
}
func NormalizeURL(value string) string { return strings.TrimRight(value, "/") }

func (s *Store) UpdateProvisionedTarget(ctx context.Context, id, resourceID, details string) error {
	result, err := s.ExecContext(ctx, `UPDATE targets SET provider_resource_id=?,provider_details_json=?::jsonb,updated_at=? WHERE id=?`, null(resourceID), nullJSON(details), now(), id)
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

func (s *Store) RecordRequest(ctx context.Context, requestID, deploymentID string, started time.Time, status int, latencyMS float64, errorType string) error {
	_, err := s.ExecContext(ctx, `INSERT INTO request_records(request_id,deployment_id,started_at,completed_at,status_code,latency_ms,retry_count,error_type) VALUES(?,?,?,?,?,?,0,?) ON CONFLICT(request_id) DO UPDATE SET completed_at=EXCLUDED.completed_at,status_code=EXCLUDED.status_code,latency_ms=EXCLUDED.latency_ms,error_type=EXCLUDED.error_type`, requestID, deploymentID, started.UTC().Format(time.RFC3339Nano), now(), status, latencyMS, null(errorType))
	return err
}

func (s *Store) PurgeRequests(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit < 1 {
		limit = 10000
	}
	result, err := s.ExecContext(ctx, `DELETE FROM request_records WHERE ctid IN (SELECT ctid FROM request_records WHERE started_at<? ORDER BY started_at LIMIT ?)`, before.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) RequestStats(ctx context.Context, deploymentID string, window time.Duration) (domain.RequestStats, error) {
	if window <= 0 {
		return domain.RequestStats{}, errors.New("stats window must be positive")
	}
	var out domain.RequestStats
	var p50, p95 sql.NullFloat64
	err := s.QueryRowContext(ctx, `SELECT COUNT(*)::double precision/?,COALESCE(AVG(CASE WHEN error_type IS NOT NULL OR status_code IS NULL OR status_code>=400 THEN 1.0 ELSE 0.0 END),0),percentile_cont(0.50) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE latency_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE latency_ms IS NOT NULL) FROM request_records WHERE deployment_id=? AND started_at>=NOW()-(?*INTERVAL '1 second')`, window.Seconds(), deploymentID, window.Seconds()).Scan(&out.RequestsPerSecond, &out.ErrorRate, &p50, &p95)
	if err != nil {
		return domain.RequestStats{}, err
	}
	if p50.Valid {
		out.P50LatencyMS = &p50.Float64
	}
	if p95.Valid {
		out.P95LatencyMS = &p95.Float64
	}
	return out, nil
}

func (s *Store) ActiveGeneration(ctx context.Context, deploymentID, ownerID string) (domain.RouterGeneration, error) {
	var out domain.RouterGeneration
	var created string
	err := s.QueryRowContext(ctx, `SELECT id,deployment_id,owner_id,generation,strategy,worker_set_hash,internal_endpoint,status,created_at FROM router_generations WHERE deployment_id=? AND owner_id=? AND status='active' ORDER BY generation DESC LIMIT 1`, deploymentID, ownerID).Scan(&out.ID, &out.DeploymentID, &out.OwnerID, &out.Generation, &out.Strategy, &out.WorkerSetHash, &out.InternalEndpoint, &out.Status, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.CreatedAt = parseTime(created)
	return out, nil
}

func (s *Store) RecordGeneration(ctx context.Context, generation domain.RouterGeneration) (domain.RouterGeneration, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return generation, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE router_generations SET status='retired' WHERE deployment_id=? AND owner_id=? AND status='active'`, generation.DeploymentID, generation.OwnerID); err != nil {
		return generation, err
	}
	id, err := newID()
	if err != nil {
		return generation, err
	}
	generation.ID = id
	generation.Status = "active"
	stamp := now()
	generation.CreatedAt = parseTime(stamp)
	_, err = tx.ExecContext(ctx, `INSERT INTO router_generations(id,deployment_id,owner_id,generation,strategy,worker_set_hash,internal_endpoint,status,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, generation.ID, generation.DeploymentID, generation.OwnerID, generation.Generation, generation.Strategy, generation.WorkerSetHash, generation.InternalEndpoint, generation.Status, stamp)
	if err != nil {
		return generation, err
	}
	return generation, tx.Commit()
}
