package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestReplayCacheCapacityFinOpsAndAutopilotPersistence(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	name := "intelligence-" + suffix
	fixtureProvider := "fixture-" + suffix
	target := name + "-target"
	if _, err := s.AddTarget(ctx, domain.Target{Name: target, URL: "http://intelligence.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "model"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "org/model", MinReplicas: 1, MaxReplicas: 1}, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	deployment = resolved.Deployment
	artifact, err := s.AttachModelArtifact(ctx, "global", deployment.ActiveRevisionID, domain.ModelArtifact{Source: "huggingface", Repository: "org/model", RequestedRevision: "main", ImmutableRevision: strings.Repeat("a", 40), ModelIdentity: "org/model@" + strings.Repeat("a", 40), CacheState: "unknown", RuntimeCompatibilityJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	in, out := 128, 32
	started := time.Now().UTC().Add(-time.Minute)
	if err = s.RecordRequest(ctx, domain.InferenceRecord{RequestID: "request-" + suffix, TenantID: "global", DeploymentID: deployment.ID, RevisionID: deployment.ActiveRevisionID, StartedAt: started, StatusCode: 200, LatencyMS: 100, InputTokens: &in, OutputTokens: &out, OperationName: "chat", Streaming: true, SessionIDHash: "session-hash", SharedPrefixHash: "prefix-hash"}); err != nil {
		t.Fatal(err)
	}
	trace, err := s.CaptureReplayTrace(ctx, "global", name, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if trace.RequestCount != 1 || strings.Contains(trace.ShapeJSON, "prompt") || !strings.Contains(trace.ShapeJSON, "session-hash") {
		t.Fatalf("trace=%#v", trace)
	}
	loaded, err := s.ReplayTrace(ctx, "global", trace.ID)
	if err != nil || loaded.ShapeDigest != trace.ShapeDigest {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	if _, err = s.ReplayTrace(ctx, "other", trace.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross tenant replay err=%v", err)
	}
	now := time.Now().UTC()
	observation, err := s.RecordArtifactCacheObservation(ctx, "global", domain.ArtifactCacheObservation{ModelArtifactID: artifact.ID, Provider: fixtureProvider, Location: "zone/cache", State: "present", Source: "fixture-api", EvidenceJSON: `{"present":true}`, ObservedAt: now, ExpiresAt: now.Add(time.Minute)})
	if err != nil || observation.ID == "" {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	if _, err = s.RecordArtifactCacheObservation(ctx, "other", domain.ArtifactCacheObservation{ModelArtifactID: artifact.ID, Provider: fixtureProvider, Location: "zone/cache", State: "present", Source: "fixture-api", EvidenceJSON: `{}`, ObservedAt: now, ExpiresAt: now.Add(time.Minute)}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross tenant cache err=%v", err)
	}
	prefetch, created, err := s.RequestArtifactPrefetch(ctx, "global", domain.ArtifactPrefetch{ModelArtifactID: artifact.ID, Provider: fixtureProvider, Region: "zone", Location: "zone/cache", IdempotencyKey: "prefetch-" + suffix})
	if err != nil || !created {
		t.Fatalf("prefetch=%#v created=%v err=%v", prefetch, created, err)
	}
	again, created, err := s.RequestArtifactPrefetch(ctx, "global", domain.ArtifactPrefetch{ModelArtifactID: artifact.ID, Provider: fixtureProvider, Region: "zone", Location: "zone/cache", IdempotencyKey: "prefetch-" + suffix})
	if err != nil || created || again.ID != prefetch.ID {
		t.Fatalf("retry=%#v created=%v err=%v", again, created, err)
	}
	if _, _, err = s.RequestArtifactPrefetch(ctx, "global", domain.ArtifactPrefetch{ModelArtifactID: artifact.ID, Provider: fixtureProvider, Region: "other", Location: "zone/cache", IdempotencyKey: "prefetch-" + suffix}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("prefetch conflict err=%v", err)
	}
	loadedArtifact, err := s.ModelArtifactForTenantByID(ctx, "global", artifact.ID)
	if err != nil || loadedArtifact.ModelIdentity != artifact.ModelIdentity {
		t.Fatalf("artifact lookup=%#v err=%v", loadedArtifact, err)
	}
	updated, err := s.UpdateArtifactPrefetch(ctx, "global", prefetch.ID, "running", "provider-operation-1", "")
	if err != nil || updated.Status != "running" || updated.ProviderOperationID != "provider-operation-1" {
		t.Fatalf("updated prefetch=%#v err=%v", updated, err)
	}
	clocked, err := s.RecordCapacityOperation(ctx, domain.CapacityOperation{TenantID: "global", Provider: fixtureProvider + "-clock", Runtime: "vllm", ComputeMode: "elastic", Region: "zone", GPU: "GPU", Operation: "ensure", ResourceKey: name + "-clock", Outcome: "succeeded", StartedAt: time.Now().UTC().Add(-3 * time.Second)})
	if err != nil || clocked.CompletedAt.IsZero() || clocked.DurationSeconds < 2 || clocked.DurationSeconds > 10 {
		t.Fatalf("database-clock capacity operation=%#v err=%v", clocked, err)
	}
	if _, err = s.RecordCapacityOperation(ctx, domain.CapacityOperation{TenantID: "global", Provider: fixtureProvider, Runtime: "vllm", ComputeMode: "elastic", Region: "zone", GPU: "GPU", Operation: "ensure", ResourceKey: name + "a", Outcome: "pending", StartedAt: now.Add(-20 * time.Second), CompletedAt: now.Add(-15 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	for index, outcome := range []string{"succeeded", "succeeded", "capacity_unavailable"} {
		begin := now.Add(-10*time.Second + time.Duration(index)*time.Second)
		if _, err = s.RecordCapacityOperation(ctx, domain.CapacityOperation{TenantID: "global", Provider: fixtureProvider, Runtime: "vllm", ComputeMode: "elastic", Region: "zone", GPU: "GPU", Operation: "ensure", ResourceKey: name + string(rune('a'+index)), Outcome: outcome, StartedAt: begin, CompletedAt: begin.Add(time.Duration(index+1) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	summaries, err := s.CapacityIntelligence(ctx, "global", time.Hour)
	var summary *domain.CapacitySummary
	for index := range summaries {
		if summaries[index].Provider == fixtureProvider && summaries[index].Region == "zone" && summaries[index].GPU == "GPU" {
			summary = &summaries[index]
			break
		}
	}
	if err != nil || summary == nil || summary.Attempts != 3 || summary.Succeeded != 2 || summary.CapacityFailures != 1 {
		t.Fatalf("capacity=%#v err=%v", summaries, err)
	}
	if summary.Pending != 0 || summary.ProviderFailures != 0 || summary.DurationP50Seconds != nil || summary.DurationP95Seconds != nil {
		t.Fatalf("capacity must deduplicate polling and withhold statistically weak predictions: %#v", summary)
	}
	stableProvider := fixtureProvider + "-stable"
	for index := 1; index <= 20; index++ {
		begin := now.Add(-time.Duration(100-index) * time.Second)
		if _, err = s.RecordCapacityOperation(ctx, domain.CapacityOperation{TenantID: "global", Provider: stableProvider, Runtime: "vllm", ComputeMode: "elastic", Region: "zone", GPU: "GPU", Operation: "ensure", ResourceKey: fmt.Sprintf("%s-stable-%d", name, index), Outcome: "succeeded", StartedAt: begin, CompletedAt: begin.Add(time.Duration(index) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	summaries, err = s.CapacityIntelligence(ctx, "global", time.Hour)
	var stable *domain.CapacitySummary
	for index := range summaries {
		if summaries[index].Provider == stableProvider {
			stable = &summaries[index]
			break
		}
	}
	if err != nil || stable == nil || stable.DurationP50Seconds == nil || stable.DurationP95Seconds == nil || *stable.DurationP50Seconds != 10.5 || *stable.DurationP95Seconds < 19 || *stable.DurationP95Seconds > 19.1 {
		t.Fatalf("stable readiness evidence=%#v err=%v", stable, err)
	}
	report, err := s.RecordFinOpsReport(ctx, domain.FinOpsReport{TenantID: "global", DeploymentID: deployment.ID, DeploymentName: name, WindowStart: now.Add(-time.Hour), WindowEnd: now, Currency: "USD", Status: "unavailable", SummaryJSON: `{"status":"unavailable"}`, EvidenceJSON: `[]`, InputDigest: strings.Repeat("b", 64)})
	if err != nil || report.ID == "" {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	reports, err := s.FinOpsReports(ctx, "global", name, 10)
	if err != nil || len(reports) != 1 || reports[0].ID != report.ID {
		t.Fatalf("reports=%#v err=%v", reports, err)
	}
	recommendation, err := s.RecordInferenceRecommendation(ctx, domain.InferenceRecommendation{TenantID: "global", DeploymentID: deployment.ID, Status: "recommended", AlgorithmVersion: "recommendation-v1", SelectedEvidenceID: "benchmark", Reason: "qualified", MissingJSON: `[]`, CandidatesJSON: `[{"evidence_id":"benchmark"}]`, InputSnapshotJSON: `{"evidence":["benchmark"]}`})
	if err != nil {
		t.Fatal(err)
	}
	plan, created, err := s.CreateAutopilotPlan(ctx, domain.AutopilotPlan{TenantID: "global", DeploymentID: deployment.ID, DeploymentName: name, RecommendationID: recommendation.ID, Objective: "minimize_cost", CandidateJSON: recommendation.CandidatesJSON, EvidenceJSON: recommendation.InputSnapshotJSON, InputDigest: recommendation.InputDigest})
	if err != nil || !created {
		t.Fatalf("plan=%#v created=%v err=%v", plan, created, err)
	}
	againPlan, created, err := s.CreateAutopilotPlan(ctx, domain.AutopilotPlan{TenantID: "global", DeploymentID: deployment.ID, DeploymentName: name, RecommendationID: recommendation.ID, Objective: "minimize_cost", CandidateJSON: recommendation.CandidatesJSON, EvidenceJSON: recommendation.InputSnapshotJSON, InputDigest: recommendation.InputDigest})
	if err != nil || created || againPlan.ID != plan.ID {
		t.Fatalf("plan retry=%#v created=%v err=%v", againPlan, created, err)
	}
	approved, err := s.ApproveAutopilotPlan(ctx, "global", plan.ID, "operator")
	if err != nil || approved.Status != "approved" || approved.ApprovedBy != "operator" {
		t.Fatalf("approved=%#v err=%v", approved, err)
	}
	approvedAgain, err := s.ApproveAutopilotPlan(ctx, "global", plan.ID, "operator")
	if err != nil || approvedAgain.ID != plan.ID {
		t.Fatalf("approval retry=%#v err=%v", approvedAgain, err)
	}
}
