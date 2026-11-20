package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// cleanedCampaign is a two-candidate new-endpoint campaign run to the end
// through the store's real transitions.
type cleanedCampaign struct {
	id, control, challenger string
}

// runCleanedCampaign measures a control (246 tok/s, p95 TTFT 315 ms) and a
// challenger (193 tok/s, 339 ms), persists one ranking for both, and moves
// the selected candidate ("control", "challenger", or none) to qualified and
// the other to rejected, or both to inconclusive when none is selected. A
// qualified candidate is then let go the way production lets it go: by an
// operator cancelling the run, or by its approval ending. Cleanup finally
// deletes every candidate deployment and marks every candidate cleaned, so
// only the persisted records say what the campaign decided.
func runCleanedCampaign(t *testing.T, ctx context.Context, s *Store, selected, release string) cleanedCampaign {
	t.Helper()
	controlProposalID, challengerProposalID := strings.Repeat("1", 64), strings.Repeat("2", 64)
	proposal, err := json.Marshal(optimizer.Proposal{SchemaVersion: "infercrane.optimizer.proposal/v1", Input: optimizer.Request{ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive"}, StartingPoint: &optimizer.StartingPoint{CandidateID: controlProposalID}})
	if err != nil {
		t.Fatal(err)
	}
	spec := func(args ...string) string {
		var draft optimizer.DeploymentDraft
		draft.Name, draft.Model.ID, draft.Runtime.Engine, draft.Runtime.Args = "qwen", "Qwen/Qwen3-0.6B", "vllm", args
		draft.Resources.GPU, draft.Resources.GPUCount, draft.Scaling.MaxReplicas = "RTX4090", 1, 1
		encoded, _ := json.Marshal(draft)
		return string(encoded)
	}
	campaign, _, err := s.CreateOptimizationCampaign(ctx, domain.OptimizationCampaign{TenantID: "global", IdempotencyKey: "decision-" + selected + "-" + release, InputDigest: strings.Repeat("e", 64), ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive", Source: "fixture", Intent: optimizationcampaign.IntentNewEndpoint, ProposalJSON: string(proposal)}, []domain.OptimizationCandidateRun{
		{ProposalCandidateID: controlProposalID, Rank: 1, EvidenceState: "unmeasured", DeploymentSpecJSON: spec(), PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
		{ProposalCandidateID: challengerProposalID, Rank: 2, EvidenceState: "unmeasured", DeploymentSpecJSON: spec("--kv-cache-dtype", "fp8"), PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if campaign, err = s.ApproveOptimizationCampaign(ctx, "global", campaign.ID, "operator", 5, time.Now().Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	result := cleanedCampaign{id: campaign.ID}
	for _, candidate := range campaign.Candidates {
		if candidate.ProposalCandidateID == controlProposalID {
			result.control = candidate.ID
		} else {
			result.challenger = candidate.ID
		}
	}
	chosen := map[string]string{"control": result.control, "challenger": result.challenger}[selected]

	// Both candidates are measured and checked before either is ranked.
	states, deployments := map[string]string{}, map[string]string{}
	for _, candidate := range campaign.Candidates {
		outputTPS, ttft, seed := 246.0, 315.0, "c"
		if candidate.ID == result.challenger {
			outputTPS, ttft, seed = 193.0, 339.0, "d"
		}
		measured := recordEvidenceDeployment(t, ctx, s, "qwen-opt-"+candidate.ID[:8], outputTPS, ttft, seed)
		deployments[candidate.ID] = measured.deployment.Name
		state := optimizationcampaign.CandidateProposed
		for _, step := range []struct {
			to      string
			updates domain.OptimizationCandidateRun
		}{
			{optimizationcampaign.CandidateProvisioning, domain.OptimizationCandidateRun{}},
			{optimizationcampaign.CandidateReady, domain.OptimizationCandidateRun{DeploymentName: measured.deployment.Name, RevisionID: measured.deployment.ActiveRevisionID}},
			{optimizationcampaign.CandidateMeasuring, domain.OptimizationCandidateRun{}},
			{optimizationcampaign.CandidateValidating, domain.OptimizationCandidateRun{BenchmarkID: measured.benchmark.ID}},
			{optimizationcampaign.CandidateRanked, domain.OptimizationCandidateRun{BenchmarkID: measured.benchmark.ID, QualityEvidenceID: measured.quality.ID}},
		} {
			if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, candidate.ID, state, step.to, step.updates); err != nil {
				t.Fatalf("transition %s -> %s: %v", state, step.to, err)
			}
			state = step.to
		}
		states[candidate.ID] = state
	}

	// One persisted ranking decides both, in the shape the ranker writes.
	decisions := map[string]string{}
	for _, id := range []string{result.control, result.challenger} {
		switch {
		case chosen == "":
			decisions[id] = optimizationcampaign.RankInconclusive
		case id == chosen:
			decisions[id] = optimizationcampaign.RankSelect
		default:
			decisions[id] = optimizationcampaign.RankSupersede
		}
	}
	rows := []map[string]any{}
	for id, decision := range decisions {
		rows = append(rows, map[string]any{"candidate_id": id, "campaign_decision": decision, "selected": decision == optimizationcampaign.RankSelect})
	}
	results, _ := json.Marshal(rows)
	ranking, err := s.RecordLabEvaluation(ctx, "global", domain.LabEvaluation{ID: "optlab-" + strings.Repeat("7", 64), ModelIdentity: "Qwen/Qwen3-0.6B", AlgorithmVersion: "inference-lab-v4+" + optimizationcampaign.RankingAlgorithmVersion, InputJSON: `{"objective":"interactive"}`, ResultsJSON: string(results), InputDigest: strings.Repeat("7", 64)})
	if err != nil {
		t.Fatal(err)
	}
	for id, decision := range decisions {
		to, updates := optimizationcampaign.CandidateQualified, domain.OptimizationCandidateRun{LabEvaluationID: ranking.ID}
		switch decision {
		case optimizationcampaign.RankSupersede:
			to, updates.FailureCode = optimizationcampaign.CandidateRejected, "measured_candidate_superseded"
		case optimizationcampaign.RankInconclusive:
			to, updates.FailureCode = optimizationcampaign.CandidateInconclusive, "measured_ranking_inconclusive"
		}
		if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, id, states[id], to, updates); err != nil {
			t.Fatalf("rank %s -> %s: %v", states[id], to, err)
		}
		states[id] = to
	}

	switch release {
	case "cancel":
		cancelled, cancelErr := s.CancelOptimizationCampaign(ctx, "global", campaign.ID)
		if cancelErr != nil {
			t.Fatal(cancelErr)
		}
		for _, candidate := range cancelled.Candidates {
			states[candidate.ID] = candidate.State
		}
	case "expire":
		if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, chosen, states[chosen], optimizationcampaign.CandidateFailed, domain.OptimizationCandidateRun{FailureCode: "approval_expired_before_activation"}); err != nil {
			t.Fatal(err)
		}
		states[chosen] = optimizationcampaign.CandidateFailed
	}
	for _, id := range []string{result.control, result.challenger} {
		if err = s.DeleteDeploymentForTenant(ctx, "global", deployments[id]); err != nil {
			t.Fatal(err)
		}
		if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, id, states[id], optimizationcampaign.CandidateCleaned, domain.OptimizationCandidateRun{}); err != nil {
			t.Fatalf("clean %s: %v", states[id], err)
		}
	}
	return result
}

func loadCleanedReport(t *testing.T, ctx context.Context, s *Store, id string) optimizationreport.Report {
	t.Helper()
	report, err := optimizationreport.Load(ctx, s, "global", id)
	if err != nil {
		t.Fatal(err)
	}
	if report.CampaignState != optimizationcampaign.CampaignCleaned {
		t.Fatalf("campaign state=%s, want cleaned", report.CampaignState)
	}
	for _, candidate := range report.Candidates {
		if candidate.State != optimizationcampaign.CandidateCleaned {
			t.Fatalf("candidate %s state=%s, want cleaned", candidate.ID, candidate.State)
		}
	}
	return report
}

func reportMetric(t *testing.T, report optimizationreport.Report, key string) optimizationreport.Metric {
	t.Helper()
	for _, metric := range report.Metrics {
		if metric.Key == key {
			return metric
		}
	}
	t.Fatalf("report has no metric %s", key)
	return optimizationreport.Metric{}
}

// outcomeCards returns the large value of every outcome card on the HTML page.
func outcomeCards(t *testing.T, report optimizationreport.Report) []string {
	t.Helper()
	page, err := optimizationreport.HTML(optimizationreport.Document{Report: report})
	if err != nil {
		t.Fatal(err)
	}
	values := []string{}
	for _, part := range strings.Split(string(page), `<div class="value">`)[1:] {
		values = append(values, part[:strings.Index(part, "</div>")])
	}
	return values
}

// TestCleanedCampaignReportKeepsTheBaselineItKept is the campaign that kept
// vLLM defaults and was then cancelled and cleaned: the report must still say
// the current setup was kept, with its measured numbers unchanged.
func TestCleanedCampaignReportKeepsTheBaselineItKept(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	campaign := runCleanedCampaign(t, ctx, s, "control", "cancel")
	report := loadCleanedReport(t, ctx, s, campaign.id)
	if report.Outcome.Kind != optimizationreport.OutcomeBaselineKept || report.Outcome.WinnerCandidateID != campaign.control || report.Outcome.Activated {
		t.Fatalf("outcome after cleanup=%+v, want the kept baseline", report.Outcome)
	}
	if report.Outcome.Headline != "Keep your current setup: nothing beat it by more than the measurement noise." {
		t.Fatalf("headline=%q", report.Outcome.Headline)
	}
	for key, want := range map[string]float64{optimizationreport.MetricTTFTP95: 315, optimizationreport.MetricTokensPerAccel: 246} {
		metric := reportMetric(t, report, key)
		if metric.Baseline == nil || *metric.Baseline != want || metric.Winner == nil || *metric.Winner != want || metric.Decision != optimizationreport.DecisionUnchanged {
			t.Fatalf("%s after cleanup=%+v, want %v unchanged", key, metric, want)
		}
	}
	if cards := outcomeCards(t, report); cards[1] != "315 ms" || cards[3] != "246" {
		t.Fatalf("outcome cards=%v", cards)
	}
}

// TestCleanedCampaignReportNamesAWinnerThatWasNeverRolledOut: a challenger
// qualified, nobody activated it, and its approval ended. The report names it
// as the best setup found and never claims it went live.
func TestCleanedCampaignReportNamesAWinnerThatWasNeverRolledOut(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	campaign := runCleanedCampaign(t, ctx, s, "challenger", "expire")
	report := loadCleanedReport(t, ctx, s, campaign.id)
	if report.Outcome.Kind != optimizationreport.OutcomeWinnerSelected || report.Outcome.WinnerCandidateID != campaign.challenger || report.Outcome.BaselineCandidateID != campaign.control || report.Outcome.Activated {
		t.Fatalf("outcome after cleanup=%+v, want the challenger as an unreleased winner", report.Outcome)
	}
	headline := report.Outcome.Headline
	if !strings.Contains(headline, "was the best setup found but was not rolled out") || !strings.HasSuffix(headline, "its server was removed because the approval ended.") || strings.Contains(headline, "is live") || strings.HasPrefix(headline, "Roll out") {
		t.Fatalf("headline=%q", headline)
	}
	if metric := reportMetric(t, report, optimizationreport.MetricTTFTP95); metric.Baseline == nil || *metric.Baseline != 315 || metric.Winner == nil || *metric.Winner != 339 {
		t.Fatalf("p95 TTFT after cleanup=%+v", metric)
	}
}

// TestCleanedCampaignReportWithoutAWinnerShowsTheCurrentSetup: nothing
// qualified, so the report has no winner, but the outcome cards lead with the
// current setup's measured values rather than "unknown".
func TestCleanedCampaignReportWithoutAWinnerShowsTheCurrentSetup(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	campaign := runCleanedCampaign(t, ctx, s, "", "")
	report := loadCleanedReport(t, ctx, s, campaign.id)
	if report.Outcome.Kind != optimizationreport.OutcomeNoWinner || report.Outcome.WinnerCandidateID != "" || report.Outcome.Headline != "No setup qualified. Nothing changes in production." {
		t.Fatalf("outcome after cleanup=%+v, want no winner", report.Outcome)
	}
	if metric := reportMetric(t, report, optimizationreport.MetricTTFTP95); metric.Baseline == nil || *metric.Baseline != 315 || metric.Winner != nil {
		t.Fatalf("p95 TTFT after cleanup=%+v", metric)
	}
	if cards := outcomeCards(t, report); cards[1] != "315 ms" || cards[3] != "246" {
		t.Fatalf("outcome cards lead with %v, want the current setup's measured values", cards)
	}
}
