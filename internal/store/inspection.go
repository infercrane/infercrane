package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) RequestInspectionForTenant(ctx context.Context, tenant, requestID string) (domain.RequestInspection, error) {
	var out domain.RequestInspection
	var started string
	var ttft, queue, generation sql.NullFloat64
	var input, output sql.NullInt64
	err := s.QueryRowContext(ctx, `SELECT r.request_id,r.tenant_id,COALESCE(r.deployment_id,''),COALESCE(r.revision_id,''),COALESCE(r.target_id,''),COALESCE(r.logical_model_id,''),COALESCE(r.environment_id,''),COALESCE(r.endpoint_id,''),COALESCE(r.serving_plan_id,''),COALESCE(r.binding_id,''),COALESCE(r.provider,''),COALESCE(r.runtime,''),COALESCE(r.compute_mode,''),r.operation_name,COALESCE(r.request_model,''),COALESCE(r.response_model,''),COALESCE(r.error_type,''),COALESCE(r.semantic_convention_schema,''),r.started_at,COALESCE(r.status_code,0),COALESCE(r.latency_ms,0),r.ttft_ms,r.input_tokens,r.output_tokens,r.streaming,r.retry_count,r.queue_ms,r.generation_ms,COALESCE(r.fallback_reason,''),COALESCE(m.name,''),COALESCE(v.name,''),COALESCE(e.name,''),COALESCE(p.spec_digest,''),COALESCE(b.name,''),COALESCE(d.name,''),COALESCE(rv.id,''),COALESCE(t.name,'') FROM request_records r LEFT JOIN logical_models m ON m.id=r.logical_model_id LEFT JOIN environments v ON v.id=r.environment_id LEFT JOIN endpoints e ON e.id=r.endpoint_id LEFT JOIN serving_plans p ON p.id=r.serving_plan_id LEFT JOIN backend_bindings b ON b.id=r.binding_id LEFT JOIN deployments d ON d.id=r.deployment_id LEFT JOIN deployment_revisions rv ON rv.id=r.revision_id LEFT JOIN targets t ON t.id=r.target_id WHERE r.tenant_id=? AND r.request_id=?`, tenant, requestID).Scan(&out.RequestID, &out.TenantID, &out.DeploymentID, &out.RevisionID, &out.TargetID, &out.LogicalModelID, &out.EnvironmentID, &out.EndpointID, &out.ServingPlanID, &out.BindingID, &out.Provider, &out.Runtime, &out.ComputeMode, &out.OperationName, &out.RequestModel, &out.ResponseModel, &out.ErrorType, &out.SemanticConventionSchema, &started, &out.StatusCode, &out.LatencyMS, &ttft, &input, &output, &out.Streaming, &out.RetryCount, &queue, &generation, &out.FallbackReason, &out.LogicalModel, &out.Environment, &out.Endpoint, &out.ServingPlan, &out.Binding, &out.Deployment, &out.Revision, &out.Target)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.StartedAt = parseTime(started)
	if ttft.Valid {
		out.TTFTMS = &ttft.Float64
	}
	if queue.Valid {
		out.QueueMS = &queue.Float64
	}
	if generation.Valid {
		out.GenerationMS = &generation.Float64
	}
	if input.Valid {
		value := int(input.Int64)
		out.InputTokens = &value
	}
	if output.Valid {
		value := int(output.Int64)
		out.OutputTokens = &value
	}
	return out, nil
}

type diagnosticEvidence struct {
	EndpointState string   `json:"endpoint_state"`
	Requests      int      `json:"requests"`
	Errors        int      `json:"errors"`
	ErrorRate     float64  `json:"error_rate"`
	P95LatencyMS  *float64 `json:"p95_latency_ms"`
	P95QueueMS    *float64 `json:"p95_queue_ms"`
	LatestRequest string   `json:"latest_request,omitempty"`
}

func (s *Store) DiagnoseEndpoint(ctx context.Context, tenant, name string, window time.Duration) ([]domain.DiagnosticFinding, error) {
	if window <= 0 || window > 30*24*time.Hour {
		return nil, errors.New("diagnostic window must be between zero and 30 days")
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, name)
	if err != nil {
		return nil, err
	}
	var evidence diagnosticEvidence
	evidence.EndpointState = resolved.Endpoint.ObservedState
	var p95Latency, p95Queue sql.NullFloat64
	var latest sql.NullTime
	err = s.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(*) FILTER(WHERE error_type IS NOT NULL OR status_code IS NULL OR status_code>=400),COALESCE(AVG(CASE WHEN error_type IS NOT NULL OR status_code IS NULL OR status_code>=400 THEN 1.0 ELSE 0.0 END),0),percentile_cont(0.95) WITHIN GROUP(ORDER BY latency_ms) FILTER(WHERE latency_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP(ORDER BY queue_ms) FILTER(WHERE queue_ms IS NOT NULL),MAX(started_at) FROM request_records WHERE tenant_id=? AND endpoint_id=? AND started_at>=NOW()-(?*INTERVAL '1 second')`, tenant, resolved.Endpoint.ID, window.Seconds()).Scan(&evidence.Requests, &evidence.Errors, &evidence.ErrorRate, &p95Latency, &p95Queue, &latest)
	if err != nil {
		return nil, err
	}
	if p95Latency.Valid {
		evidence.P95LatencyMS = &p95Latency.Float64
	}
	if p95Queue.Valid {
		evidence.P95QueueMS = &p95Queue.Float64
	}
	if latest.Valid {
		evidence.LatestRequest = latest.Time.UTC().Format(time.RFC3339Nano)
	}
	type rule struct{ code, severity, confidence, summary string }
	var rules []rule
	if evidence.EndpointState != "serving" {
		rules = append(rules, rule{"endpoint_not_serving", "critical", "high", "Endpoint is not serving because its persisted route state is " + evidence.EndpointState + "."})
	}
	if evidence.Requests >= 20 && evidence.ErrorRate >= 0.05 {
		rules = append(rules, rule{"elevated_error_rate", "critical", "high", "At least 5% of recent requests failed."})
	}
	if evidence.Requests >= 20 && evidence.P95QueueMS != nil && evidence.P95LatencyMS != nil && *evidence.P95QueueMS >= 100 && *evidence.P95QueueMS >= *evidence.P95LatencyMS*0.30 {
		rules = append(rules, rule{"queue_dominates_latency", "warning", "high", "Queueing accounts for at least 30% of recent p95 latency."})
	}
	if len(rules) == 0 {
		rules = append(rules, rule{"no_active_finding", "info", "high", "No deterministic issue is active in the selected evidence window."})
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		return nil, err
	}
	digestBytes := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	observed := time.Now().UTC()
	if evidence.LatestRequest != "" {
		observed = latest.Time.UTC()
	}
	findings := make([]domain.DiagnosticFinding, 0, len(rules))
	for _, item := range rules {
		id, idErr := newID()
		if idErr != nil {
			return nil, idErr
		}
		finding := domain.DiagnosticFinding{ID: id, TenantID: tenant, EndpointID: resolved.Endpoint.ID, Code: item.code, Severity: item.severity, Confidence: item.confidence, Summary: item.summary, EvidenceJSON: string(body), EvidenceDigest: digest, ObservedAt: observed, CreatedAt: time.Now().UTC()}
		_, err = s.ExecContext(ctx, `INSERT INTO diagnostic_findings(id,tenant_id,endpoint_id,code,severity,confidence,summary,evidence_json,evidence_digest,observed_at,created_at) VALUES(?,?,?,?,?,?,?,?::jsonb,?,?,?) ON CONFLICT(tenant_id,endpoint_id,code,evidence_digest) DO UPDATE SET observed_at=EXCLUDED.observed_at RETURNING id,created_at`, finding.ID, tenant, finding.EndpointID, finding.Code, finding.Severity, finding.Confidence, finding.Summary, finding.EvidenceJSON, finding.EvidenceDigest, finding.ObservedAt, finding.CreatedAt)
		if err != nil {
			return nil, err
		}
		// Read canonical persisted identity for idempotent evaluations.
		if err = s.QueryRowContext(ctx, `SELECT id,created_at FROM diagnostic_findings WHERE tenant_id=? AND endpoint_id=? AND code=? AND evidence_digest=?`, tenant, finding.EndpointID, finding.Code, finding.EvidenceDigest).Scan(&finding.ID, &finding.CreatedAt); err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}
	return findings, nil
}
