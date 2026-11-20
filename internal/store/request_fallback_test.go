package store

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// failoverEndpoint is a primary-fallback endpoint whose two bindings each
// serve their own deployment.
type failoverEndpoint struct {
	primary, fallback               domain.Deployment
	primaryBinding, fallbackBinding domain.BackendBinding
}

func createFailoverEndpoint(t *testing.T, ctx context.Context, s *Store, suffix string) failoverEndpoint {
	t.Helper()
	createDeployment := func(name string) domain.Deployment {
		target, err := s.AddTarget(ctx, domain.Target{Name: name + "-target", URL: "http://" + name, Provider: "existing", Runtime: "vllm", UpstreamModel: "coder"})
		if err != nil {
			t.Fatal(err)
		}
		deployment, err := s.CreateDeployment(ctx, domain.Deployment{Name: name, Model: "coder", Runtime: "vllm"}, []string{target.Name})
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := s.ResolveForTenant(ctx, "global", deployment.Name)
		if err != nil {
			t.Fatal(err)
		}
		return resolved.Deployment
	}
	out := failoverEndpoint{primary: createDeployment("failover-primary-" + suffix), fallback: createDeployment("failover-fallback-" + suffix)}
	environment, err := s.EnvironmentForTenant(ctx, "global", "production")
	if err != nil {
		t.Fatal(err)
	}
	model, err := s.CreateLogicalModel(ctx, "global", domain.LogicalModel{Name: "failover-model-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := s.CreateEndpoint(ctx, "global", domain.Endpoint{Name: "failover-endpoint-" + suffix, LogicalModelID: model.ID, EnvironmentID: environment.ID})
	if err != nil {
		t.Fatal(err)
	}
	if out.primaryBinding, err = s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: endpoint.ID, Name: "primary", Kind: "deployment", OwnershipMode: "lifecycle-managed", DeploymentID: out.primary.ID}); err != nil {
		t.Fatal(err)
	}
	if out.fallbackBinding, err = s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: endpoint.ID, Name: "fallback", Kind: "deployment", OwnershipMode: "lifecycle-managed", DeploymentID: out.fallback.ID}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRevisionMetricsCountRequestsThatFailedOverAwayFromTheRevision(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	e := createFailoverEndpoint(t, ctx, s, time.Now().UTC().Format("150405.000000000"))
	primaryRevision, fallbackRevision := e.primary.ActiveRevisionID, e.fallback.ActiveRevisionID
	ms := func(value float64) *float64 { return &value }
	tokens := func(value int) *int { return &value }
	now := time.Now().UTC()
	servedByPrimary := func(id string) domain.InferenceRecord {
		return domain.InferenceRecord{RequestID: id, DeploymentID: e.primary.ID, RevisionID: primaryRevision, BindingID: e.primaryBinding.ID, OperationName: "chat", StartedAt: now, StatusCode: 200, LatencyMS: 100, TTFTMS: ms(50), OutputTokens: tokens(10)}
	}
	servedByFallback := func(id string, status int, errorType string, leftPrimary bool) domain.InferenceRecord {
		record := domain.InferenceRecord{RequestID: id, DeploymentID: e.fallback.ID, RevisionID: fallbackRevision, BindingID: e.fallbackBinding.ID, OperationName: "chat", StartedAt: now, StatusCode: status, ErrorType: errorType, LatencyMS: 900, TTFTMS: ms(800), OutputTokens: tokens(30)}
		if leftPrimary {
			record.FallbackReason, record.FallbackFromBindingID, record.FallbackFromRevisionID = "upstream_503", e.primaryBinding.ID, primaryRevision
		}
		return record
	}
	stale := servedByFallback("failover-stale", 200, "", true)
	stale.StartedAt = now.Add(-2 * time.Hour)
	for _, record := range []domain.InferenceRecord{
		servedByPrimary("served-by-primary"),
		// The fallback covered the primary's failure: the caller got a 200.
		servedByFallback("covered-by-fallback", 200, "", true),
		// The fallback failed too.
		servedByFallback("fallback-failed-too", 503, "upstream_unavailable", true),
		// Traffic the fallback served without the primary failing first.
		servedByFallback("fallback-only", 200, "", false),
		stale,
	} {
		if err := s.RecordRequest(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	var binding, revision, reason sql.NullString
	if err := s.QueryRowContext(ctx, `SELECT fallback_from_binding_id,fallback_from_revision_id,fallback_reason FROM request_records WHERE request_id=?`, "covered-by-fallback").Scan(&binding, &revision, &reason); err != nil {
		t.Fatal(err)
	}
	if binding.String != e.primaryBinding.ID || revision.String != primaryRevision || reason.String != "upstream_503" {
		t.Fatalf("persisted binding=%v revision=%v reason=%v", binding, revision, reason)
	}
	if err := s.QueryRowContext(ctx, `SELECT fallback_from_binding_id,fallback_from_revision_id FROM request_records WHERE request_id=?`, "fallback-only").Scan(&binding, &revision); err != nil || binding.Valid || revision.Valid {
		t.Fatalf("a request that did not fail over names a binding it left: binding=%v revision=%v err=%v", binding, revision, err)
	}

	approx := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	window := time.Hour
	primary, err := s.RevisionMetrics(ctx, primaryRevision, window)
	if err != nil {
		t.Fatal(err)
	}
	// One request it served and two it failed: both failed-over requests are
	// errors of the primary, whatever the fallback answered.
	if primary.Requests != 3 || !approx(primary.ErrorRate, 2.0/3) {
		t.Fatalf("primary requests=%d error rate=%v", primary.Requests, primary.ErrorRate)
	}
	// TTFT, latency and output speed come only from the request the primary
	// served: 10 tokens in 100 ms.
	if primary.P95TTFTMS == nil || *primary.P95TTFTMS != 50 || primary.P95LatencyMS == nil || *primary.P95LatencyMS != 100 || primary.OutputTokensPerSecond == nil || !approx(*primary.OutputTokensPerSecond, 100) {
		t.Fatalf("primary figures include requests it did not serve: %+v", primary)
	}
	since, err := s.RevisionMetricsSince(ctx, primaryRevision, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if since.Requests != 3 || !approx(since.ErrorRate, 2.0/3) || since.P95TTFTMS == nil || *since.P95TTFTMS != 50 || since.EvidenceSource != "post_promotion_request_telemetry" {
		t.Fatalf("primary since promotion=%+v", since)
	}
	// The fallback keeps every request it served, judged by its own answer.
	fallback, err := s.RevisionMetrics(ctx, fallbackRevision, window)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Requests != 3 || !approx(fallback.ErrorRate, 1.0/3) || fallback.P95TTFTMS == nil || *fallback.P95TTFTMS != 800 || fallback.OutputTokensPerSecond == nil || !approx(*fallback.OutputTokensPerSecond, 30000.0/900) {
		t.Fatalf("fallback=%+v", fallback)
	}
	// Binding metrics, which a staged switch from a closed API reads, count
	// the same way.
	primaryBinding, err := s.BindingMetrics(ctx, "global", e.primaryBinding, window)
	if err != nil {
		t.Fatal(err)
	}
	if primaryBinding.Requests != 3 || !approx(primaryBinding.ErrorRate, 2.0/3) || primaryBinding.P95TTFTMS == nil || *primaryBinding.P95TTFTMS != 50 || primaryBinding.EvidenceSource != "binding_request_telemetry" {
		t.Fatalf("primary binding=%+v", primaryBinding)
	}
	fallbackBinding, err := s.BindingMetrics(ctx, "global", e.fallbackBinding, window)
	if err != nil {
		t.Fatal(err)
	}
	if fallbackBinding.Requests != 3 || !approx(fallbackBinding.ErrorRate, 1.0/3) || fallbackBinding.P95TTFTMS == nil || *fallbackBinding.P95TTFTMS != 800 {
		t.Fatalf("fallback binding=%+v", fallbackBinding)
	}
	if other, err := s.BindingMetrics(ctx, "other-tenant", e.primaryBinding, window); err != nil || other.Requests != 0 {
		t.Fatalf("another tenant sees the binding's requests: %+v err=%v", other, err)
	}
}

func TestRevisionMetricsCountARequestThatLeftAndReturnedToTheRevisionOnce(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	e := createFailoverEndpoint(t, ctx, s, time.Now().UTC().Format("150405.000000000"))
	// Two bindings can serve one deployment. A request that left one of them
	// and was answered by the other is still one request of that revision,
	// and the attempt it left failed.
	revision := e.primary.ActiveRevisionID
	ttft := 40.0
	if err := s.RecordRequest(ctx, domain.InferenceRecord{RequestID: "left-and-returned", DeploymentID: e.primary.ID, RevisionID: revision, BindingID: e.fallbackBinding.ID, OperationName: "chat", StartedAt: time.Now().UTC(), StatusCode: 200, LatencyMS: 80, TTFTMS: &ttft, FallbackReason: "upstream_connection_failed", FallbackFromBindingID: e.primaryBinding.ID, FallbackFromRevisionID: revision}); err != nil {
		t.Fatal(err)
	}
	metrics, err := s.RevisionMetrics(ctx, revision, time.Hour)
	if err != nil || metrics.Requests != 1 || metrics.ErrorRate != 1 || metrics.P95TTFTMS == nil || *metrics.P95TTFTMS != 40 {
		t.Fatalf("metrics=%+v err=%v", metrics, err)
	}
}

func TestRequestMetricsQueriesKeepAnIndexOnEachSide(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	start := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	for _, tc := range []struct {
		subject requestSubject
		scope   string
		args    []any
		indexes []string
	}{
		{revisionRequests, `started_at>=?`, []any{start}, []string{"idx_requests_revision_started", "request_records_fallback_from_revision_started_idx"}},
		{bindingRequests, `tenant_id=? AND started_at>=?`, []any{"global", start}, []string{"request_records_binding_started_idx", "request_records_fallback_from_binding_started_idx"}},
	} {
		plan := explainWithoutSeqScan(t, ctx, s, requestMetricsSQL(tc.subject, tc.scope), append([]any{"id", "id", "id", "id", "id", "id"}, tc.args...)...)
		for _, index := range tc.indexes {
			if !strings.Contains(plan, index) {
				t.Fatalf("%s metrics do not use %s:\n%s", tc.subject.served, index, plan)
			}
		}
	}
}

// explainWithoutSeqScan plans query against a tenant with some traffic, as
// in production: on an empty table any index on tenant_id looks free. The
// rows and their statistics are rolled back.
func explainWithoutSeqScan(t *testing.T, ctx context.Context, s *Store, query string, args ...any) string {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`INSERT INTO request_records(request_id,tenant_id,started_at,status_code,latency_ms) SELECT 'plan-'||g,'global',NOW()-(g||' seconds')::interval,200,10 FROM generate_series(1,5000) g`,
		`ANALYZE request_records`,
		`SET LOCAL enable_seqscan=off`,
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := tx.QueryContext(ctx, `EXPLAIN `+rebind(query), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}
