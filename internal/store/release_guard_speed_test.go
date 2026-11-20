package store

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// A canary serves a small share of traffic beside the active revision.
// Release Guard must compare how fast each revision serves a request, not
// how many tokens each served: 19 requests and 1 request at the same speed
// are equally fast. Failed requests and requests without a token count do
// not set the speed.
func TestRevisionMetricsComparesSpeedNotTrafficShare(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	revision := func(name string) string {
		deployment, _, _, err := s.SubmitCloudDeployment(ctx,
			domain.Deployment{Name: name + "-" + suffix, Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 1},
			domain.Operation{Kind: "deployment.converge", IdempotencyKey: "create-" + name + "-" + suffix, RequestJSON: `{"name":"` + name + `","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S"}`},
		)
		if err != nil {
			t.Fatal(err)
		}
		return deployment.ActiveRevisionID
	}
	record := func(revisionID string, index, status int, latency float64, output *int) {
		t.Helper()
		input := 512
		if err := s.RecordRequest(ctx, domain.InferenceRecord{RequestID: fmt.Sprintf("speed-%s-%s-%d", revisionID, suffix, index), TenantID: "global", RevisionID: revisionID, StartedAt: time.Now().UTC().Add(-10 * time.Second), StatusCode: status, LatencyMS: latency, InputTokens: &input, OutputTokens: output, OperationName: "chat"}); err != nil {
			t.Fatal(err)
		}
	}
	tokens := func(value int) *int { return &value }
	active, canary := revision("guard-speed-active"), revision("guard-speed-canary")
	for index := range 19 {
		record(active, index, 200, 2000, tokens(200)) // 100 tokens/s each
	}
	record(active, 19, 500, 10, tokens(200))  // failed: not a speed
	record(active, 20, 200, 1000, nil)        // no token count: not a speed
	record(canary, 0, 200, 1000, tokens(100)) // 100 tokens/s

	activeMetrics, err := s.RevisionMetrics(ctx, active, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	canaryMetrics, err := s.RevisionMetrics(ctx, canary, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if activeMetrics.OutputTokensPerSecond == nil || canaryMetrics.OutputTokensPerSecond == nil || math.Abs(*activeMetrics.OutputTokensPerSecond-100) > 1e-9 || math.Abs(*canaryMetrics.OutputTokensPerSecond-100) > 1e-9 {
		t.Fatalf("active %v tokens/s over %d requests, canary %v tokens/s over %d, want 100 and 100", activeMetrics.OutputTokensPerSecond, activeMetrics.Requests, canaryMetrics.OutputTokensPerSecond, canaryMetrics.Requests)
	}
	since, err := s.RevisionMetricsSince(ctx, canary, time.Now().UTC().Add(-time.Minute))
	if err != nil || since.OutputTokensPerSecond == nil || math.Abs(*since.OutputTokensPerSecond-100) > 1e-9 {
		t.Fatalf("post-promotion speed = %v, err %v", since.OutputTokensPerSecond, err)
	}
}
