package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/apibaseline"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapicatalog"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
	"github.com/infercrane/infercrane/internal/replay"
)

type migrationFixture struct {
	endpoint         domain.Endpoint
	closedAPI, owned domain.BackendBinding
	deployment       domain.Deployment
	campaign         domain.OptimizationCampaign
	closedPlan       domain.ServingPlan
	suffix           string
}

// newMigrationFixture builds an endpoint whose active plan routes to a closed
// API, plus an owned deployment on another model and a campaign.
func newMigrationFixture(t *testing.T, ctx context.Context, s *Store) migrationFixture {
	t.Helper()
	f := migrationFixture{suffix: time.Now().UTC().Format("150405.000000000")}
	target, err := s.AddTarget(ctx, domain.Target{Name: "closed-api-" + f.suffix, URL: "https://api.openai.invalid", Provider: "external", Runtime: "openai-compatible", UpstreamModel: "gpt-4.1-mini"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTargetHealth(ctx, target.ID, "healthy"); err != nil {
		t.Fatal(err)
	}
	ownedTarget, err := s.AddTarget(ctx, domain.Target{Name: "owned-" + f.suffix, URL: "http://owned-" + f.suffix, Provider: "existing", Runtime: "llama.cpp", UpstreamModel: "qwen"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTargetHealth(ctx, ownedTarget.ID, "healthy"); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.CreateDeployment(ctx, domain.Deployment{Name: "qwen-owned-" + strings.ReplaceAll(f.suffix, ".", ""), Model: "Qwen/Qwen3-0.6B", Runtime: "llama.cpp"}, []string{ownedTarget.Name})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveForTenant(ctx, "global", deployment.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.deployment = resolved.Deployment
	environment, err := s.EnvironmentForTenant(ctx, "global", "production")
	if err != nil {
		t.Fatal(err)
	}
	model, err := s.CreateLogicalModel(ctx, "global", domain.LogicalModel{Name: "support-" + f.suffix})
	if err != nil {
		t.Fatal(err)
	}
	if f.endpoint, err = s.CreateEndpoint(ctx, "global", domain.Endpoint{Name: "support-" + strings.ReplaceAll(f.suffix, ".", ""), LogicalModelID: model.ID, EnvironmentID: environment.ID}); err != nil {
		t.Fatal(err)
	}
	if f.closedAPI, err = s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: f.endpoint.ID, Name: "primary", Kind: "external", OwnershipMode: "traffic-managed", TargetID: target.ID}); err != nil {
		t.Fatal(err)
	}
	if f.closedPlan, err = s.CreateServingPlan(ctx, "global", domain.ServingPlan{EndpointID: f.endpoint.ID, RoutingPolicy: "manual", Bindings: []domain.ServingPlanBinding{{BindingID: f.closedAPI.ID, Weight: 100}}}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetEndpointPlan(ctx, "global", f.endpoint.Name, f.closedPlan.ID, "active"); err != nil {
		t.Fatal(err)
	}
	f.campaign, _, err = s.CreateOptimizationCampaign(ctx, domain.OptimizationCampaign{TenantID: "global", IdempotencyKey: "migration-" + f.suffix, InputDigest: strings.Repeat("9", 64), ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive", Source: "fixture", Intent: optimizationcampaign.IntentNewEndpoint, ProposalJSON: `{"schema_version":"infercrane.optimizer.proposal/v1"}`}, []domain.OptimizationCandidateRun{{ProposalCandidateID: strings.Repeat("8", 64), Rank: 1, EvidenceState: "unmeasured", DeploymentSpecJSON: `{"name":"qwen"}`, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f migrationFixture) verdict(t *testing.T, ctx context.Context, s *Store, outcome, revision string) domain.MigrationQualityVerdict {
	t.Helper()
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := qualityevidence.MigrationVerdict{Schema: qualityevidence.MigrationVerdictSchema, CampaignID: f.campaign.ID, Endpoint: f.endpoint.Name, BindingID: f.closedAPI.ID, CandidateDeployment: f.deployment.Name, CandidateRevisionID: revision, ReferenceEvidenceDigest: "sha256:" + strings.Repeat("a", 64), CandidateEvidenceDigest: "sha256:" + strings.Repeat("b", 64), Suite: "support", SuiteVersion: "1@sha256:x", Evaluator: "infercrane-graded", EvaluatorVersion: "1", MarginPercent: 5, Verdict: outcome, Code: "fixture", CandidateScore: 0.9, EvaluatedAt: time.Now().UTC().Truncate(time.Second)}
	envelope, err := passport.Sign(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.RecordMigrationVerdict(ctx, "global", f.deployment.Name, domain.MigrationQualityVerdict{CampaignID: f.campaign.ID, CandidateRunID: f.campaign.Candidates[0].ID, EndpointID: f.endpoint.ID, BindingID: f.closedAPI.ID, RevisionID: revision, Verdict: outcome, Code: "fixture", MarginPercent: 5, PayloadJSON: envelope.PayloadJSON, PayloadDigest: envelope.Digest, Signature: envelope.Signature, PublicKey: envelope.PublicKey, Algorithm: envelope.Algorithm, KeyID: envelope.KeyID, EvaluatedAt: payload.EvaluatedAt})
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func (f migrationFixture) record(t *testing.T, ctx context.Context, s *Store, binding domain.BackendBinding, id string, ttft float64, input, cached, output int) {
	t.Helper()
	record := domain.InferenceRecord{RequestID: id + "-" + f.suffix, TenantID: "global", EndpointID: f.endpoint.ID, BindingID: binding.ID, OperationName: "chat", StartedAt: time.Now().UTC(), StatusCode: 200, LatencyMS: ttft * 2, TTFTMS: &ttft, InputTokens: &input, OutputTokens: &output, CachedInputTokens: &cached}
	if binding.Kind == "external" {
		record.TargetID, record.ComputeMode = binding.TargetID, "external"
	} else {
		record.DeploymentID, record.RevisionID = f.deployment.ID, f.deployment.ActiveRevisionID
	}
	if err := s.RecordRequest(ctx, record); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationSwitchStagesCanaryPrimaryAndRollback(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	f := newMigrationFixture(t, ctx, s)

	// A weighted plan between a closed API and another model, without a
	// verdict, stays refused.
	owned, err := s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: f.endpoint.ID, Name: "owned", Kind: "deployment", OwnershipMode: "traffic-managed", DeploymentID: f.deployment.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateServingPlan(ctx, "global", domain.ServingPlan{EndpointID: f.endpoint.ID, RoutingPolicy: "weighted", Bindings: []domain.ServingPlanBinding{{BindingID: owned.ID, Priority: 0, Weight: 500}, {BindingID: f.closedAPI.ID, Priority: 1, Weight: 9500}}}); err == nil {
		t.Fatal("a weighted plan across two models was created without a verdict")
	}
	// A BLOCK verdict, or a PASS for another revision, admits no plan.
	blocked := f.verdict(t, ctx, s, releaseguard.QualityBlock, f.deployment.ActiveRevisionID)
	if _, err = s.StageMigration(ctx, "global", f.endpoint.Name, MigrationStepCanary, blocked.ID, 0, time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("BLOCK verdict canary error=%v", err)
	}
	stale := f.verdict(t, ctx, s, releaseguard.QualityPass, "another-revision")
	if _, err = s.StageMigration(ctx, "global", f.endpoint.Name, MigrationStepCanary, stale.ID, 0, time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision canary error=%v", err)
	}
	if _, err = s.StageMigration(ctx, "global", f.endpoint.Name, MigrationStepCanary, stale.ID, 2000, time.Hour); err == nil {
		t.Fatal("a 20% canary was accepted")
	}

	pass := f.verdict(t, ctx, s, releaseguard.QualityPass, f.deployment.ActiveRevisionID)
	canary, err := s.StageMigration(ctx, "global", f.endpoint.Name, MigrationStepCanary, pass.ID, 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !canary.Activated || canary.Evaluation.Decision != "PASS" || !strings.Contains(canary.Evaluation.ReasonCodesJSON, "migration_canary_admitted_on_quality_verdict") {
		t.Fatalf("canary=%+v", canary)
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, "global", f.endpoint.Name)
	if err != nil {
		t.Fatal(err)
	}
	// 5% as the smallest equal ratio, so the canary is interleaved one in 20.
	if resolved.ActivePlan.RoutingPolicy != "weighted" || resolved.ActivePlan.MigrationVerdictID != pass.ID || resolved.ActivePlan.Bindings[0].BindingID != owned.ID || resolved.ActivePlan.Bindings[0].Weight != 1 || resolved.ActivePlan.Bindings[1].Weight != 19 {
		t.Fatalf("canary plan=%+v", resolved.ActivePlan)
	}
	// A migration plan stays on its endpoint.
	if _, _, err = s.StageEnvironmentPromotion(ctx, "global", f.endpoint.Name, f.endpoint.Name+"-x", "k"); err == nil {
		t.Fatal("a migration plan was promoted")
	}

	// The next step is measured per binding on the canary's traffic.
	policy, err := s.EndpointReleaseGuardPolicy(ctx, "global", f.endpoint.Name)
	if err != nil {
		t.Fatal(err)
	}
	policy.Enabled, policy.MinimumRequests, policy.MaxTTFTRegressionPercent, policy.MaxLatencyRegressionPercent = true, 2, 20, 20
	if _, err = s.SetEndpointReleaseGuardPolicy(ctx, "global", f.endpoint.Name, policy); err != nil {
		t.Fatal(err)
	}
	f.record(t, ctx, s, f.closedAPI, "closed-1", 300, 100, 60, 20)
	f.record(t, ctx, s, f.closedAPI, "closed-2", 320, 100, 60, 20)
	f.record(t, ctx, s, owned, "owned-1", 900, 100, 0, 20)
	f.record(t, ctx, s, owned, "owned-2", 950, 100, 0, 20)
	slow, err := s.StageMigration(ctx, "global", f.endpoint.Name, MigrationStepPrimary, "", 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if slow.Activated || slow.Evaluation.Decision != "REJECT" || !strings.Contains(slow.Evaluation.MetricsJSON, "binding_request_telemetry") {
		t.Fatalf("a slower owned binding was promoted: %+v", slow.Evaluation)
	}
	f.record(t, ctx, s, owned, "owned-3", 250, 100, 0, 20)
	f.record(t, ctx, s, owned, "owned-4", 260, 100, 0, 20)
	f.record(t, ctx, s, owned, "owned-5", 270, 100, 0, 20)
	f.record(t, ctx, s, owned, "owned-6", 280, 100, 0, 20)
	f.record(t, ctx, s, owned, "owned-7", 290, 100, 0, 20)
	f.record(t, ctx, s, owned, "owned-8", 300, 100, 0, 20)
	policy.MaxTTFTRegressionPercent, policy.MaxLatencyRegressionPercent = 300, 300
	if _, err = s.SetEndpointReleaseGuardPolicy(ctx, "global", f.endpoint.Name, policy); err != nil {
		t.Fatal(err)
	}
	primary, err := s.StageMigration(ctx, "global", f.endpoint.Name, MigrationStepPrimary, "", 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !primary.Activated || primary.Evaluation.Decision != "PASS" {
		t.Fatalf("primary=%+v", primary.Evaluation)
	}
	resolved, _ = s.ResolveEndpointForTenant(ctx, "global", f.endpoint.Name)
	if resolved.ActivePlan.RoutingPolicy != "primary-fallback" || resolved.ActivePlan.Bindings[0].BindingID != owned.ID || resolved.ActivePlan.Bindings[1].BindingID != f.closedAPI.ID {
		t.Fatalf("primary plan=%+v", resolved.ActivePlan)
	}

	rollback, err := s.StageMigration(ctx, "global", f.endpoint.Name, MigrationStepRollback, "", 0, time.Hour)
	if err != nil || !rollback.Activated || rollback.Plan.ID != f.closedPlan.ID {
		t.Fatalf("rollback=%+v err=%v", rollback, err)
	}
}

func TestEndpointReplayRateCardAndBindingEvidence(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	f := newMigrationFixture(t, ctx, s)
	f.record(t, ctx, s, f.closedAPI, "replay-1", 300, 1000, 400, 200)
	f.record(t, ctx, s, f.closedAPI, "replay-2", 310, 1000, 0, 300)
	trace, err := s.CaptureEndpointReplayTrace(ctx, "global", f.endpoint.Name, "primary", time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if trace.EndpointID != f.endpoint.ID || trace.BindingID != f.closedAPI.ID || trace.DeploymentID != "" || trace.RequestCount != 2 {
		t.Fatalf("trace=%+v", trace)
	}
	volume, err := replay.TraceTokenVolume(trace)
	if err != nil || volume.InputTokens != 2000 || volume.CachedInputTokens != 400 || volume.OutputTokens != 500 {
		t.Fatalf("volume=%+v err=%v", volume, err)
	}
	read, err := s.ReplayTrace(ctx, "global", trace.ID)
	if err != nil || read.EndpointName != f.endpoint.Name || read.BindingID != f.closedAPI.ID || read.ShapeDigest != trace.ShapeDigest {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	listed, err := s.EndpointReplayTraces(ctx, "global", f.endpoint.Name, 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if _, err = s.CaptureEndpointReplayTrace(ctx, "global", f.endpoint.Name, "missing", time.Hour, 100); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown binding error=%v", err)
	}

	if card, err := s.BindingRateCard(ctx, "global", f.closedAPI.ID); err != nil || card != nil {
		t.Fatalf("rate card before any: %+v %v", card, err)
	}
	input, cached, output := int64(400_000), int64(100_000), int64(1_600_000)
	recorded, err := s.RecordBindingRateCard(ctx, "global", f.endpoint.Name, "primary", apibaseline.RateCard{Authority: apibaseline.AuthorityListPrice, Pricing: modelapicatalog.Pricing{Currency: "USD", InputMicrousdPerMillion: &input, CachedInputMicrousdPerMillion: &cached, OutputMicrousdPerMillion: &output}, Source: "https://openai.com/api/pricing", ValidUntil: time.Now().Add(30 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	card, err := s.BindingRateCard(ctx, "global", f.closedAPI.ID)
	if err != nil || card == nil || card.ID != recorded.ID || *card.Pricing.CachedInputMicrousdPerMillion != cached || card.Authority != apibaseline.AuthorityListPrice {
		t.Fatalf("card=%+v err=%v", card, err)
	}
	cost := apibaseline.Price(card, volume, time.Hour, time.Now())
	if cost.Unknown != "" || cost.WindowUSD == nil {
		t.Fatalf("cost=%+v", cost)
	}
	if _, err = s.RecordBindingRateCard(ctx, "global", f.endpoint.Name, "primary", apibaseline.RateCard{Authority: "guess", Pricing: card.Pricing, ValidUntil: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("rate card without authority recorded")
	}
	ownedBinding, err := s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: f.endpoint.ID, Name: "owned", Kind: "deployment", OwnershipMode: "traffic-managed", DeploymentID: f.deployment.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordBindingRateCard(ctx, "global", f.endpoint.Name, ownedBinding.Name, *card); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rate card on a deployment binding error=%v", err)
	}

	evidence := domain.BindingQualityEvidence{Source: domain.QualityEvidenceSourceInferCrane, Suite: "support", SuiteVersion: "1@x", Evaluator: "infercrane-graded", EvaluatorVersion: "1", Score: 0.9, SampleCount: 40, PayloadJSON: `{"score":0.9}`, PayloadDigest: "sha256:" + strings.Repeat("c", 64), Signature: "s", PublicKey: "k", Algorithm: "ed25519", KeyID: "id", EvaluatedAt: time.Now().UTC()}
	first, created, err := s.RecordBindingQualityEvidence(ctx, "global", f.endpoint.Name, "primary", evidence)
	if err != nil || !created {
		t.Fatalf("first=%+v created=%v err=%v", first, created, err)
	}
	again, created, err := s.RecordBindingQualityEvidence(ctx, "global", f.endpoint.Name, "primary", evidence)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("again=%+v created=%v err=%v", again, created, err)
	}
	rows, err := s.BindingQualityEvidence(ctx, "global", f.closedAPI.ID, 10)
	if err != nil || len(rows) != 1 || rows[0].Score != 0.9 || rows[0].BindingID != f.closedAPI.ID {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}
