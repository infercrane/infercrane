package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

func TestReleaseGuardQualityWatchAcceptsAndRecordsFollowUp(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "quality-watch-" + time.Now().UTC().Format("150405.000000000")
	if _, err := s.AddTarget(ctx, domain.Target{Name: name + "-target", URL: "http://" + name, Provider: "existing", Runtime: "vllm", UpstreamModel: "model-v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "model-v1"}, []string{name + "-target"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveForTenant(ctx, "global", name)
	if err != nil {
		t.Fatal(err)
	}
	deployment := resolved.Deployment
	state, _, err := s.QualityVerdicts(ctx, "global", name)
	if err != nil || state != QualityVerdictsNotGated {
		t.Fatalf("ungated state=%q err=%v", state, err)
	}

	limit, watch := 3.0, 1.0
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, MaxTTFTRegressionPercent: 15, MaxLatencyRegressionPercent: 15, MaxErrorRateIncrease: .01, MaxOutputThroughputDropPercent: 20, RequireQualityEvidence: true, MaxQualityRegressionPercent: &limit, QualityWatchRegressionPercent: &watch}
	for _, invalid := range []struct {
		max, watch *float64
	}{{&limit, guardNumber(4)}, {nil, &watch}, {&limit, guardNumber(-1)}} {
		bad := policy
		bad.MaxQualityRegressionPercent, bad.QualityWatchRegressionPercent = invalid.max, invalid.watch
		if _, err = s.SetReleaseGuardPolicy(ctx, "global", name, bad); err == nil {
			t.Fatalf("invalid watch band max=%v watch=%v was accepted", invalid.max, invalid.watch)
		}
	}
	stored, err := s.SetReleaseGuardPolicy(ctx, "global", name, policy)
	if err != nil || stored.QualityWatchRegressionPercent == nil || *stored.QualityWatchRegressionPercent != 1 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	state, _, err = s.QualityVerdicts(ctx, "global", name)
	if err != nil || state != QualityVerdictsNoCandidate {
		t.Fatalf("no-candidate state=%q err=%v", state, err)
	}

	candidate, err := s.CreateCandidateRevision(ctx, "global", name, `{"model":"model-v2","runtime":"vllm","routing_strategy":"round-robin","min_replicas":1,"max_replicas":1}`)
	if err != nil {
		t.Fatal(err)
	}
	replica, _, err := s.EnsureReplicaIntent(ctx, domain.Replica{TenantID: "global", DeploymentID: deployment.ID, RevisionID: candidate.ID, Ordinal: 0, ExternalKey: deployment.ID + "-" + candidate.ID + "-r0", Provider: "existing"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveReplica(ctx, replica.ID, "ready", "http://"+name+"-candidate", "healthy", `{}`, time.Now()); err != nil {
		t.Fatal(err)
	}
	ttft := 100.0
	for revision, request := range map[string]string{deployment.ActiveRevisionID: "active", candidate.ID: "candidate"} {
		if err = s.RecordRequest(ctx, domain.InferenceRecord{RequestID: name + "-" + request, DeploymentID: deployment.ID, RevisionID: revision, OperationName: "chat", StartedAt: time.Now(), StatusCode: 200, LatencyMS: 200, TTFTMS: &ttft}); err != nil {
			t.Fatal(err)
		}
	}

	digest := 0
	attach := func(revision, suite string, score float64, at time.Time) {
		t.Helper()
		digest++
		evidence := domain.QualityEvidence{RevisionID: revision, Suite: suite, SuiteVersion: "git:abc123", Evaluator: "customer-eval", EvaluatorVersion: "1.0.0", Score: score, Passed: true, SampleCount: 200, ArtifactDigest: "sha256:" + strings.Repeat("a", 64), PayloadDigest: fmt.Sprintf("sha256:%064x", digest), Signature: "signature", PublicKey: "public-key", Algorithm: "Ed25519-SHA256", KeyID: "sha256:key", EvaluatedAt: at}
		if _, _, attachErr := s.RecordQualityEvidence(ctx, "global", name, evidence); attachErr != nil {
			t.Fatal(attachErr)
		}
	}
	base := time.Now().UTC().Add(-time.Hour)

	// Missing reference evidence keeps the candidate waiting.
	attach(candidate.ID, "support-answers", .885, base.Add(time.Minute))
	attach(candidate.ID, "math", .90, base.Add(2*time.Minute))
	attach(deployment.ActiveRevisionID, "math", .90, base)
	state, report, err := s.QualityVerdicts(ctx, "global", name)
	if err != nil || state != QualityVerdictsCompared || report.Verdict != releaseguard.QualityUnknown || len(report.Evals) != 2 {
		t.Fatalf("missing-reference state=%q report=%+v err=%v", state, report, err)
	}
	evaluation, err := s.EvaluateReleaseGuard(ctx, "global", name, 5*time.Minute)
	if err != nil || evaluation.Decision != "WAIT" || !strings.Contains(evaluation.ReasonCodesJSON, "quality_evidence_not_comparable") {
		t.Fatalf("missing-reference evaluation=%+v err=%v", evaluation, err)
	}

	// A drop inside the watch band keeps ACCEPT and records a follow-up.
	attach(deployment.ActiveRevisionID, "support-answers", .90, base)
	state, report, err = s.QualityVerdicts(ctx, "global", name)
	if err != nil || state != QualityVerdictsCompared || report.Verdict != releaseguard.QualityWatch || report.Candidate.RevisionID != candidate.ID || report.Candidate.Model != "model-v2" || report.Reference.RevisionID != deployment.ActiveRevisionID {
		t.Fatalf("watch state=%q report=%+v err=%v", state, report, err)
	}
	evaluation, err = s.EvaluateReleaseGuard(ctx, "global", name, 5*time.Minute)
	if err != nil || evaluation.Decision != "ACCEPT" {
		t.Fatalf("watch evaluation=%+v err=%v", evaluation, err)
	}
	var reasons []releaseguard.Reason
	if err = json.Unmarshal([]byte(evaluation.ReasonCodesJSON), &reasons); err != nil || len(reasons) != 2 || reasons[0].Code != "quality_watch" || reasons[0].Eval != "support-answers@git:abc123" || reasons[0].FollowUp == "" || reasons[1].Code != "within_policy" {
		t.Fatalf("watch reasons=%s err=%v", evaluation.ReasonCodesJSON, err)
	}
	var metrics struct {
		Candidate domain.RevisionMetrics      `json:"candidate"`
		Quality   *releaseguard.QualityReport `json:"quality"`
	}
	if err = json.Unmarshal([]byte(evaluation.MetricsJSON), &metrics); err != nil || metrics.Quality == nil || metrics.Quality.Verdict != releaseguard.QualityWatch || len(metrics.Quality.Evals) != 2 || metrics.Candidate.QualityScore == nil {
		t.Fatalf("persisted metrics=%s err=%v", evaluation.MetricsJSON, err)
	}
	accepted, err := s.ReleaseGuardAccepted(ctx, "global", name, candidate.ID)
	if err != nil || !accepted {
		t.Fatalf("WATCH must stay promotable accepted=%t err=%v", accepted, err)
	}
	if count := qualityWatchAudits(t, s, ctx, evaluation.ID); count != 1 {
		t.Fatalf("quality watch follow-ups=%d", count)
	}

	// A newer drop beyond the limit blocks and records no follow-up.
	attach(candidate.ID, "support-answers", .85, base.Add(3*time.Minute))
	evaluation, err = s.EvaluateReleaseGuard(ctx, "global", name, 5*time.Minute)
	if err != nil || evaluation.Decision != "REJECT" || !strings.Contains(evaluation.ReasonCodesJSON, "quality_score_regression") || strings.Contains(evaluation.ReasonCodesJSON, "quality_watch") {
		t.Fatalf("block evaluation=%+v err=%v", evaluation, err)
	}
	if count := qualityWatchAudits(t, s, ctx, evaluation.ID); count != 0 {
		t.Fatalf("BLOCK recorded %d follow-ups", count)
	}
}

func qualityWatchAudits(t *testing.T, s *Store, ctx context.Context, evaluationID string) int {
	t.Helper()
	events, err := s.AuditEventsForTenant(ctx, "global", time.Time{}, 500)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Action == "release_guard.quality_watch" && event.Outcome == "follow_up_required" && event.Actor == "release-guard" && strings.Contains(event.Payload, evaluationID) {
			count++
		}
	}
	return count
}
