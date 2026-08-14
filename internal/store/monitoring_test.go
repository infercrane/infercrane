package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestEndpointMonitoringAggregatesBoundedTenantEvidenceAndLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	name := "monitoring-" + suffix
	targetName := name + "-target"
	if _, err := s.AddTarget(ctx, domain.Target{Name: targetName, URL: "http://monitoring.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 2}, []string{targetName})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := s.ResolveEndpointForTenant(ctx, "global", name)
	if err != nil {
		t.Fatal(err)
	}
	ttft, queue, generation := 120.0, 30.0, 450.0
	input, output := 100, 25
	for index, record := range []domain.InferenceRecord{
		{RequestID: "monitor-success-" + suffix, StatusCode: 200, LatencyMS: 600, TTFTMS: &ttft, QueueMS: &queue, GenerationMS: &generation, InputTokens: &input, OutputTokens: &output, Streaming: true},
		{RequestID: "monitor-fallback-" + suffix, StatusCode: 503, LatencyMS: 900, TTFTMS: &ttft, InputTokens: &input, OutputTokens: &output, RetryCount: 1, ErrorType: "upstream_unavailable", FallbackReason: "primary_unhealthy"},
	} {
		record.TenantID = "global"
		record.DeploymentID = deployment.ID
		record.RevisionID = deployment.ActiveRevisionID
		record.LogicalModelID = endpoint.LogicalModel.ID
		record.EnvironmentID = endpoint.Environment.ID
		record.EndpointID = endpoint.Endpoint.ID
		record.ServingPlanID = endpoint.ActivePlan.ID
		record.BindingID = endpoint.Bindings[0].ID
		record.Provider = "existing"
		record.Runtime = "vllm"
		record.OperationName = "chat"
		record.StartedAt = time.Now().UTC().Add(-time.Duration(index+1) * time.Minute)
		if err = s.RecordRequest(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Event(ctx, deployment.ID, "", "replica_healthy", "Replica became healthy", `{}`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.EndpointMonitoring(ctx, "global", name, time.Hour, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Endpoint != name || snapshot.Summary.Requests != 2 || snapshot.Summary.Errors != 1 || snapshot.Summary.Fallbacks != 1 || snapshot.Summary.Retried != 1 || snapshot.Summary.Streaming != 1 {
		t.Fatalf("snapshot summary=%+v", snapshot)
	}
	if snapshot.Summary.ErrorRate == nil || *snapshot.Summary.ErrorRate != .5 || snapshot.Summary.FallbackRate == nil || *snapshot.Summary.FallbackRate != .5 || snapshot.Summary.P95TTFTMS == nil {
		t.Fatalf("snapshot rates=%+v", snapshot.Summary)
	}
	if snapshot.Summary.TokenUsageSamples != 2 || snapshot.Summary.InputTokenSamples != 2 || snapshot.Summary.OutputTokenSamples != 2 || snapshot.Summary.OutputTokensPerSecond == nil {
		t.Fatalf("reported token evidence=%+v", snapshot.Summary)
	}
	if snapshot.Evidence.SampleCount != 2 || !snapshot.Evidence.Fresh || snapshot.Evidence.ContentRecorded || snapshot.Evidence.LatestRequestAt == nil {
		t.Fatalf("snapshot evidence=%+v", snapshot.Evidence)
	}
	if len(snapshot.Series) < 2 || len(snapshot.Series) > 14 || len(snapshot.Breakdowns) != 1 || len(snapshot.Events) == 0 || snapshot.Events[0].Type != "replica_healthy" {
		t.Fatalf("series=%d breakdowns=%+v events=%+v", len(snapshot.Series), snapshot.Breakdowns, snapshot.Events)
	}
	if _, err = s.EndpointMonitoring(ctx, "other", name, time.Hour, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant monitoring err=%v", err)
	}
	if _, err = s.EndpointMonitoring(ctx, "global", name, 31*24*time.Hour, time.Hour); err == nil {
		t.Fatal("unbounded monitoring window was accepted")
	}
	if _, err = s.EndpointMonitoring(ctx, "global", name, 500*time.Minute, time.Minute); err == nil {
		t.Fatal("a window capable of returning 500+ aligned buckets was accepted")
	}

	unknownTokenName := name + "-unknown-tokens"
	unknownTokenTarget := targetName + "-unknown-tokens"
	if _, err = s.AddTarget(ctx, domain.Target{Name: unknownTokenTarget, URL: "http://monitoring-unknown.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
		t.Fatal(err)
	}
	unknownDeployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: unknownTokenName, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1}, []string{unknownTokenTarget})
	if err != nil {
		t.Fatal(err)
	}
	unknownEndpoint, err := s.ResolveEndpointForTenant(ctx, "global", unknownTokenName)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordRequest(ctx, domain.InferenceRecord{
		RequestID: "monitor-unknown-tokens-" + suffix, TenantID: "global", DeploymentID: unknownDeployment.ID,
		RevisionID: unknownDeployment.ActiveRevisionID, LogicalModelID: unknownEndpoint.LogicalModel.ID,
		EnvironmentID: unknownEndpoint.Environment.ID, EndpointID: unknownEndpoint.Endpoint.ID,
		ServingPlanID: unknownEndpoint.ActivePlan.ID, BindingID: unknownEndpoint.Bindings[0].ID,
		Provider: "existing", Runtime: "vllm", OperationName: "chat", StatusCode: 200,
		LatencyMS: 75, InputTokens: &input, StartedAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	unknownTokenSnapshot, err := s.EndpointMonitoring(ctx, "global", unknownTokenName, time.Hour, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if unknownTokenSnapshot.Summary.TokenUsageSamples != 1 || unknownTokenSnapshot.Summary.InputTokenSamples != 1 || unknownTokenSnapshot.Summary.OutputTokenSamples != 0 || unknownTokenSnapshot.Summary.InputTokensPerSecond == nil || unknownTokenSnapshot.Summary.OutputTokensPerSecond != nil {
		t.Fatalf("partially reported token usage was rendered as measured output zero: %+v", unknownTokenSnapshot.Summary)
	}
	if containsString(unknownTokenSnapshot.Evidence.Unavailable, "reported_input_token_usage") || !containsString(unknownTokenSnapshot.Evidence.Unavailable, "reported_output_token_usage") || !containsString(unknownTokenSnapshot.Evidence.Available, "reported_input_token_usage") || containsString(unknownTokenSnapshot.Evidence.Available, "reported_output_token_usage") {
		t.Fatalf("unreported token evidence boundary=%+v", unknownTokenSnapshot.Evidence)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
