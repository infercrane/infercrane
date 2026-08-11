package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

func (s *Store) ReleaseGuardPolicy(ctx context.Context, tenant, deploymentName string) (domain.ReleaseGuardPolicy, error) {
	if tenant == "" {
		tenant = "global"
	}
	var policy domain.ReleaseGuardPolicy
	err := s.QueryRowContext(ctx, `SELECT p.deployment_id,p.enabled,p.minimum_requests,p.max_ttft_regression_percent,p.max_latency_regression_percent,p.max_error_rate_increase,p.max_output_throughput_drop_percent,p.require_compatibility_evidence,p.require_synthetic_evidence,p.max_cost_regression_percent,p.auto_rollback_enabled,p.auto_rollback_window_seconds,p.validation_max_requests,p.validation_max_concurrency FROM release_guard_policies p JOIN deployments d ON d.id=p.deployment_id WHERE d.tenant_id=? AND d.name=? AND d.desired_state!='deleted'`, tenant, deploymentName).Scan(&policy.DeploymentID, &policy.Enabled, &policy.MinimumRequests, &policy.MaxTTFTRegressionPercent, &policy.MaxLatencyRegressionPercent, &policy.MaxErrorRateIncrease, &policy.MaxOutputThroughputDropPercent, &policy.RequireCompatibilityEvidence, &policy.RequireSyntheticEvidence, &policy.MaxCostRegressionPercent, &policy.AutoRollbackEnabled, &policy.AutoRollbackWindowSeconds, &policy.ValidationMaxRequests, &policy.ValidationMaxConcurrency)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, ErrNotFound
	}
	return policy, err
}

func (s *Store) SetReleaseGuardPolicy(ctx context.Context, tenant, deploymentName string, policy domain.ReleaseGuardPolicy) (domain.ReleaseGuardPolicy, error) {
	if policy.AutoRollbackWindowSeconds == 0 {
		policy.AutoRollbackWindowSeconds = 300
	}
	if policy.ValidationMaxRequests == 0 {
		policy.ValidationMaxRequests = 100
	}
	if policy.ValidationMaxConcurrency == 0 {
		policy.ValidationMaxConcurrency = 4
	}
	if policy.MinimumRequests < 1 || policy.MaxTTFTRegressionPercent < 0 || policy.MaxLatencyRegressionPercent < 0 || policy.MaxErrorRateIncrease < 0 || policy.MaxErrorRateIncrease > 1 || policy.MaxOutputThroughputDropPercent < 0 || policy.AutoRollbackWindowSeconds < 30 || policy.AutoRollbackWindowSeconds > 3600 || policy.ValidationMaxRequests < 1 || policy.ValidationMaxRequests > 10000 || policy.ValidationMaxConcurrency < 1 || policy.ValidationMaxConcurrency > 128 || (policy.MaxCostRegressionPercent != nil && *policy.MaxCostRegressionPercent < 0) {
		return domain.ReleaseGuardPolicy{}, errors.New("invalid Release Guard policy")
	}
	current, err := s.ReleaseGuardPolicy(ctx, tenant, deploymentName)
	if err != nil {
		return domain.ReleaseGuardPolicy{}, err
	}
	_, err = s.ExecContext(ctx, `UPDATE release_guard_policies SET enabled=?,minimum_requests=?,max_ttft_regression_percent=?,max_latency_regression_percent=?,max_error_rate_increase=?,max_output_throughput_drop_percent=?,require_compatibility_evidence=?,require_synthetic_evidence=?,max_cost_regression_percent=?,auto_rollback_enabled=?,auto_rollback_window_seconds=?,validation_max_requests=?,validation_max_concurrency=?,updated_at=? WHERE deployment_id=?`, policy.Enabled, policy.MinimumRequests, policy.MaxTTFTRegressionPercent, policy.MaxLatencyRegressionPercent, policy.MaxErrorRateIncrease, policy.MaxOutputThroughputDropPercent, policy.RequireCompatibilityEvidence, policy.RequireSyntheticEvidence, policy.MaxCostRegressionPercent, policy.AutoRollbackEnabled, policy.AutoRollbackWindowSeconds, policy.ValidationMaxRequests, policy.ValidationMaxConcurrency, now(), current.DeploymentID)
	if err != nil {
		return domain.ReleaseGuardPolicy{}, err
	}
	return s.ReleaseGuardPolicy(ctx, tenant, deploymentName)
}

func (s *Store) RevisionMetrics(ctx context.Context, revisionID string, window time.Duration) (domain.RevisionMetrics, error) {
	if window <= 0 {
		return domain.RevisionMetrics{}, errors.New("metrics window must be positive")
	}
	var metrics domain.RevisionMetrics
	var ttft, latency, output sql.NullFloat64
	err := s.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(AVG(CASE WHEN error_type IS NOT NULL OR status_code IS NULL OR status_code>=400 THEN 1.0 ELSE 0.0 END),0),percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE latency_ms IS NOT NULL),CASE WHEN COUNT(output_tokens)>0 THEN SUM(output_tokens)::double precision/? ELSE NULL END FROM request_records WHERE revision_id=? AND started_at>=NOW()-(?*INTERVAL '1 second')`, window.Seconds(), revisionID, window.Seconds()).Scan(&metrics.Requests, &metrics.ErrorRate, &ttft, &latency, &output)
	if err != nil {
		return metrics, err
	}
	if ttft.Valid {
		metrics.P95TTFTMS = &ttft.Float64
	}
	if latency.Valid {
		metrics.P95LatencyMS = &latency.Float64
	}
	if output.Valid {
		metrics.OutputTokensPerSecond = &output.Float64
	}
	if err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM replicas WHERE revision_id=? AND lifecycle_state IN ('ready','active') AND health='healthy'`, revisionID).Scan(&metrics.ReadyReplicas); err != nil {
		return domain.RevisionMetrics{}, err
	}
	metrics.EvidenceSource = "request_telemetry"
	return metrics, nil
}

func (s *Store) RevisionMetricsSince(ctx context.Context, revisionID string, since time.Time) (domain.RevisionMetrics, error) {
	if since.IsZero() || since.After(time.Now().UTC()) {
		return domain.RevisionMetrics{}, errors.New("metrics start must be a past timestamp")
	}
	duration := time.Since(since)
	if duration < time.Second {
		duration = time.Second
	}
	var metrics domain.RevisionMetrics
	var ttft, latency, output sql.NullFloat64
	err := s.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(AVG(CASE WHEN error_type IS NOT NULL OR status_code IS NULL OR status_code>=400 THEN 1.0 ELSE 0.0 END),0),percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE latency_ms IS NOT NULL),CASE WHEN COUNT(output_tokens)>0 THEN SUM(output_tokens)::double precision/? ELSE NULL END FROM request_records WHERE revision_id=? AND started_at>=?`, duration.Seconds(), revisionID, since.Format(time.RFC3339Nano)).Scan(&metrics.Requests, &metrics.ErrorRate, &ttft, &latency, &output)
	if err != nil {
		return metrics, err
	}
	if ttft.Valid {
		metrics.P95TTFTMS = &ttft.Float64
	}
	if latency.Valid {
		metrics.P95LatencyMS = &latency.Float64
	}
	if output.Valid {
		metrics.OutputTokensPerSecond = &output.Float64
	}
	if err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM replicas WHERE revision_id=? AND lifecycle_state IN ('ready','active') AND health='healthy'`, revisionID).Scan(&metrics.ReadyReplicas); err != nil {
		return domain.RevisionMetrics{}, err
	}
	metrics.EvidenceSource = "post_promotion_request_telemetry"
	return metrics, nil
}

type guardBenchmarkWorkload struct {
	EndpointType             string `json:"endpoint_type"`
	Streaming                bool   `json:"streaming"`
	RequestCount             int    `json:"request_count"`
	Concurrency              int    `json:"concurrency"`
	RandomSeed               int64  `json:"random_seed"`
	ServerTokenCount         bool   `json:"server_token_count"`
	DirectRevisionValidation bool   `json:"direct_revision_validation"`
}

type guardCostMetadata struct {
	Available bool     `json:"available"`
	Hourly    *float64 `json:"hourly"`
	Source    string   `json:"source"`
}

func benchmarkGuardMetrics(activeReady, candidateReady int, rows []domain.BenchmarkResult, activeRevisionID, candidateRevisionID string) (domain.RevisionMetrics, domain.RevisionMetrics, bool) {
	var active, candidate *domain.BenchmarkResult
	for i := range rows {
		row := &rows[i]
		if active == nil && row.RevisionID == activeRevisionID {
			active = row
		}
		if candidate == nil && row.RevisionID == candidateRevisionID {
			candidate = row
		}
	}
	if active == nil || candidate == nil || active.Tool != candidate.Tool || active.ToolVersion != candidate.ToolVersion {
		return domain.RevisionMetrics{}, domain.RevisionMetrics{}, false
	}
	var activeWorkload, candidateWorkload guardBenchmarkWorkload
	if json.Unmarshal([]byte(active.WorkloadJSON), &activeWorkload) != nil || json.Unmarshal([]byte(candidate.WorkloadJSON), &candidateWorkload) != nil {
		return domain.RevisionMetrics{}, domain.RevisionMetrics{}, false
	}
	directValidation := candidateWorkload.DirectRevisionValidation
	activeWorkload.DirectRevisionValidation, candidateWorkload.DirectRevisionValidation = false, false
	if activeWorkload != candidateWorkload {
		return domain.RevisionMetrics{}, domain.RevisionMetrics{}, false
	}
	convert := func(row *domain.BenchmarkResult, ready int) domain.RevisionMetrics {
		errorRate := 0.0
		if row.RequestCount > 0 {
			errorRate = float64(row.Failed) / float64(row.RequestCount)
		}
		metrics := domain.RevisionMetrics{EvidenceSource: "aiperf_benchmark", EvidenceID: row.ID, Requests: row.RequestCount, ReadyReplicas: ready, ErrorRate: errorRate, P95TTFTMS: row.TTFTP95MS, P95LatencyMS: row.LatencyP95MS, OutputTokensPerSecond: row.OutputTokenThroughput}
		metrics.SyntheticValidation = directValidation
		var cost guardCostMetadata
		if json.Unmarshal([]byte(row.CostMetadataJSON), &cost) == nil && cost.Available && cost.Hourly != nil && cost.Source != "" {
			metrics.SourcedHourlyCost = cost.Hourly
		}
		return metrics
	}
	activeMetrics, candidateMetrics := convert(active, activeReady), convert(candidate, candidateReady)
	compatible := active.ModelIdentity != "" && candidate.ModelIdentity != "" && active.Runtime != "" && active.Runtime == candidate.Runtime && active.RuntimeVersion != "" && active.RuntimeVersion == candidate.RuntimeVersion && active.ComputeMode == candidate.ComputeMode
	activeMetrics.Compatible, candidateMetrics.Compatible = &compatible, &compatible
	activeMetrics.CompatibilityEvidence = "revision-scoped immutable model identities plus matching runtime, runtime version and compute mode"
	candidateMetrics.CompatibilityEvidence = activeMetrics.CompatibilityEvidence
	return activeMetrics, candidateMetrics, true
}

func (s *Store) EvaluateReleaseGuard(ctx context.Context, tenant, deploymentName string, window time.Duration) (domain.ReleaseGuardEvaluation, error) {
	resolved, err := s.ResolveForTenant(ctx, tenant, deploymentName)
	if err != nil {
		return domain.ReleaseGuardEvaluation{}, err
	}
	if resolved.Deployment.CandidateRevisionID == "" {
		return domain.ReleaseGuardEvaluation{}, fmt.Errorf("%w: deployment has no candidate revision", ErrConflict)
	}
	policy, err := s.ReleaseGuardPolicy(ctx, tenant, deploymentName)
	if err != nil {
		return domain.ReleaseGuardEvaluation{}, err
	}
	active, err := s.RevisionMetrics(ctx, resolved.Deployment.ActiveRevisionID, window)
	if err != nil {
		return domain.ReleaseGuardEvaluation{}, err
	}
	candidate, err := s.RevisionMetrics(ctx, resolved.Deployment.CandidateRevisionID, window)
	if err != nil {
		return domain.ReleaseGuardEvaluation{}, err
	}
	if candidate.Requests < policy.MinimumRequests {
		benchmarks, benchmarkErr := s.BenchmarksForDeployment(ctx, tenant, deploymentName, 100)
		if benchmarkErr != nil {
			return domain.ReleaseGuardEvaluation{}, benchmarkErr
		}
		if benchmarkActive, benchmarkCandidate, ok := benchmarkGuardMetrics(active.ReadyReplicas, candidate.ReadyReplicas, benchmarks, resolved.Deployment.ActiveRevisionID, resolved.Deployment.CandidateRevisionID); ok {
			active, candidate = benchmarkActive, benchmarkCandidate
		}
	}
	// Request telemetry alone cannot prove immutable model/runtime compatibility.
	// A comparable AIPerf pair provides the persisted identity and workload proof.
	if policy.RequireCompatibilityEvidence && candidate.Compatible == nil {
		active.CompatibilityEvidence, candidate.CompatibilityEvidence = "comparable benchmark identity unavailable", "comparable benchmark identity unavailable"
	}
	result := releaseguard.Evaluate(releaseguard.Input{Policy: policy, Active: active, Candidate: candidate})
	reasons, _ := json.Marshal(result.Reasons)
	metrics, _ := json.Marshal(map[string]domain.RevisionMetrics{"active": active, "candidate": candidate})
	policyJSON, _ := json.Marshal(policy)
	id, err := newID()
	if err != nil {
		return domain.ReleaseGuardEvaluation{}, err
	}
	evaluation := domain.ReleaseGuardEvaluation{ID: id, DeploymentID: resolved.Deployment.ID, ActiveRevisionID: resolved.Deployment.ActiveRevisionID, CandidateRevisionID: resolved.Deployment.CandidateRevisionID, Decision: result.Decision, ReasonCodesJSON: string(reasons), MetricsJSON: string(metrics), PolicyJSON: string(policyJSON), CreatedAt: time.Now().UTC()}
	_, err = s.ExecContext(ctx, `INSERT INTO release_guard_evaluations(id,deployment_id,active_revision_id,candidate_revision_id,decision,reason_codes_json,metrics_json,policy_json,created_at) VALUES(?,?,?,?,?,?::jsonb,?::jsonb,?::jsonb,?)`, evaluation.ID, evaluation.DeploymentID, evaluation.ActiveRevisionID, evaluation.CandidateRevisionID, evaluation.Decision, evaluation.ReasonCodesJSON, evaluation.MetricsJSON, evaluation.PolicyJSON, evaluation.CreatedAt.Format(time.RFC3339Nano))
	return evaluation, err
}

func (s *Store) ReleaseGuardEvaluations(ctx context.Context, tenant, deploymentName string, limit int) ([]domain.ReleaseGuardEvaluation, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.QueryContext(ctx, `SELECT e.id,e.deployment_id,e.active_revision_id,e.candidate_revision_id,e.decision,e.reason_codes_json::text,e.metrics_json::text,e.policy_json::text,e.created_at FROM release_guard_evaluations e JOIN deployments d ON d.id=e.deployment_id WHERE d.tenant_id=? AND d.name=? ORDER BY e.created_at DESC LIMIT ?`, tenant, deploymentName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evaluations []domain.ReleaseGuardEvaluation
	for rows.Next() {
		var value domain.ReleaseGuardEvaluation
		var created string
		if err = rows.Scan(&value.ID, &value.DeploymentID, &value.ActiveRevisionID, &value.CandidateRevisionID, &value.Decision, &value.ReasonCodesJSON, &value.MetricsJSON, &value.PolicyJSON, &created); err != nil {
			return nil, err
		}
		value.CreatedAt = parseTime(created)
		evaluations = append(evaluations, value)
	}
	return evaluations, rows.Err()
}

func (s *Store) ReleaseGuardAccepted(ctx context.Context, tenant, deploymentName, candidateID string) (bool, error) {
	var decision, currentCandidate, activeID, evaluatedActive string
	err := s.QueryRowContext(ctx, `SELECT e.decision,COALESCE(d.candidate_revision_id,''),d.active_revision_id,e.active_revision_id FROM deployments d JOIN release_guard_evaluations e ON e.deployment_id=d.id AND e.candidate_revision_id=? WHERE d.tenant_id=? AND d.name=? ORDER BY e.created_at DESC LIMIT 1`, candidateID, tenant, deploymentName).Scan(&decision, &currentCandidate, &activeID, &evaluatedActive)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return decision == "ACCEPT" && currentCandidate == candidateID && activeID == evaluatedActive, nil
}
