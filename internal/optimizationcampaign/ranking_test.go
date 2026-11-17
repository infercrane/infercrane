package optimizationcampaign

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/optimizer"
)

func rankingFixture(t *testing.T, objective string, maxCost *float64) (domain.OptimizationCampaign, []domain.BenchmarkResult) {
	t.Helper()
	request := optimizer.Request{ModelIdentity: "qwen3-8b", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Runtimes: []string{"vllm"}, Objective: objective, WorkloadProfile: "interactive", MaxHourlyCost: maxCost, MaxCandidates: 2}
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := optimizer.NewCatalogSource(curatedrecipe.All(), registry.Snapshot()).Propose(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	// The catalog fixture can expose one candidate; duplicate the immutable
	// shape with a distinct identity to exercise measured ranking independently
	// from proposal ordering.
	if len(proposal.Candidates) == 1 {
		second := proposal.Candidates[0]
		second.ID = strings.Repeat("b", 64)
		second.Rank = 2
		second.Deployment.Name += "-second"
		proposal.Candidates = append(proposal.Candidates, second)
	}
	encoded, _ := json.Marshal(proposal)
	campaign := domain.OptimizationCampaign{ID: "campaign", TenantID: "tenant", InputDigest: proposal.InputDigest, ModelIdentity: proposal.Input.ModelIdentity, Objective: proposal.Input.Objective, ProposalJSON: string(encoded)}
	for index, candidate := range proposal.Candidates[:2] {
		campaign.Candidates = append(campaign.Candidates, domain.OptimizationCandidateRun{ID: "candidate-" + string(rune('a'+index)), TenantID: campaign.TenantID, CampaignID: campaign.ID, ProposalCandidateID: candidate.ID, State: CandidateRanked, RevisionID: "revision-" + string(rune('a'+index)), BenchmarkID: "benchmark-" + string(rune('a'+index)), QualityEvidenceID: "quality-" + string(rune('a'+index))})
	}
	// Benchmarks record the deployed artifact (repository@immutable revision),
	// not the catalog alias the operator typed when creating the campaign.
	artifactIdentity := proposal.Candidates[0].Deployment.Model.ID + "@" + proposal.Candidates[0].Deployment.Model.Revision
	ttftA, ttftB, throughputA, throughputB := 220.0, 140.0, 60.0, 80.0
	costA, costB := 2.0, 3.0
	workload := `{"profile":"interactive","request_count":100,"random_seed":17}`
	// Candidate A is the proposal's starting point and therefore the control.
	// Candidate B was measured interleaved with it and beat it clearly.
	controlWorkload := measuredWorkload(t, workload, MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleCandidate, CandidateID: "candidate-a"}}, nil)
	candidateWorkload := measuredWorkload(t, workload, MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleBracketed, CandidateID: "candidate-a", Runs: 4}, Comparison: map[string]MetricComparison{
		"ttft_p95_ms":             {ControlMean: 220, CandidateMean: 140, Delta: -80.0 / 220, NoiseFloor: 0.04, Decision: DecisionImproved},
		"output_token_throughput": {HigherIsBetter: true, ControlMean: 60, CandidateMean: 80, Delta: 20.0 / 60, NoiseFloor: 0.03, Decision: DecisionImproved},
	}}, nil)
	benchmarks := []domain.BenchmarkResult{
		{ID: "benchmark-a", RevisionID: "revision-a", ModelIdentity: artifactIdentity, Provider: "aws", Region: "eu-central-1", GPU: "L40S", Runtime: "vllm", ComputeMode: "elastic", RequestCount: 100, TTFTP95MS: &ttftA, OutputTokenThroughput: &throughputA, WorkloadJSON: controlWorkload, CostMetadataJSON: costJSON(costA)},
		{ID: "benchmark-b", RevisionID: "revision-b", ModelIdentity: artifactIdentity, Provider: "aws", Region: "eu-central-1", GPU: "L40S", Runtime: "vllm", ComputeMode: "elastic", RequestCount: 100, TTFTP95MS: &ttftB, OutputTokenThroughput: &throughputB, WorkloadJSON: candidateWorkload, CostMetadataJSON: costJSON(costB)},
	}
	return campaign, benchmarks
}

// measuredWorkload adds measured outputs to a load document the way the
// campaign executor stores them. Lab excludes both keys from the digest.
func measuredWorkload(t *testing.T, base string, evidence MeasuredEvidence, confidence map[string]benchmark.ConfidenceInterval) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(base), &document); err != nil {
		t.Fatal(err)
	}
	document["measured"] = evidence
	if confidence != nil {
		document["confidence_95"] = confidence
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func costJSON(hourly float64) string {
	encoded, _ := json.Marshal(map[string]any{"available": true, "hourly": hourly, "source": "aws-price-list"})
	return string(encoded)
}

func TestRankMeasuredCampaignIgnoresProposalOrder(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-b"] != RankSelect || result.Decisions["candidate-a"] != RankSupersede {
		t.Fatalf("unexpected measured rank: %+v", result.Decisions)
	}
	if result.Evaluation.AlgorithmVersion == "" || result.Evaluation.InputDigest == "" {
		t.Fatalf("ranking did not retain immutable Lab evidence: %+v", result.Evaluation)
	}
}

// TestRankingDecisionsReadBackThePersistedRanking keeps the reader the
// optimization report uses after cleanup equal to what ranking persists, for
// a challenger that won and for a control the gate kept.
func TestRankingDecisionsReadBackThePersistedRanking(t *testing.T) {
	won, wonBenchmarks := rankingFixture(t, "interactive", nil)
	kept, keptBenchmarks := rankingFixture(t, "interactive", nil)
	keptBenchmarks[1].WorkloadJSON = interleaved(t, map[string]MetricComparison{"ttft_p95_ms": {ControlMean: 220, CandidateMean: 214, Delta: -6.0 / 220, NoiseFloor: 0.05, Decision: DecisionNoMeasurableDifference}})
	for name, test := range map[string]struct {
		campaign   domain.OptimizationCampaign
		benchmarks []domain.BenchmarkResult
		selected   string
	}{"challenger-won": {won, wonBenchmarks, "candidate-b"}, "control-kept": {kept, keptBenchmarks, "candidate-a"}} {
		t.Run(name, func(t *testing.T) {
			result, err := RankMeasuredCampaign(test.campaign, test.benchmarks)
			if err != nil {
				t.Fatal(err)
			}
			decisions, err := RankingDecisions(result.Evaluation)
			if err != nil {
				t.Fatal(err)
			}
			if len(decisions) != len(result.Decisions) || decisions[test.selected] != RankSelect {
				t.Fatalf("persisted decisions=%v, ranked %v", decisions, result.Decisions)
			}
			for id, decision := range result.Decisions {
				if decisions[id] != decision {
					t.Fatalf("candidate %s persisted %q, ranked %q", id, decisions[id], decision)
				}
			}
		})
	}
	if _, err := RankingDecisions(domain.LabEvaluation{ResultsJSON: "not json"}); err == nil {
		t.Fatal("an unreadable ranking was decoded")
	}
	unannotated, err := RankingDecisions(domain.LabEvaluation{ResultsJSON: `[{"selected":true}]`})
	if err != nil || len(unannotated) != 0 {
		t.Fatalf("a Lab result without campaign decisions produced %v err=%v", unannotated, err)
	}
}

func TestRankMeasuredCampaignBarrierWaitsForPeersButNotForAnEarlierWinner(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	campaign.Candidates[0].State = CandidateProvisioning
	if _, err := RankMeasuredCampaign(campaign, benchmarks); err == nil || !strings.Contains(err.Error(), "candidate-a has not reached the measured ranking barrier") {
		t.Fatalf("ranking did not wait for an unmeasured peer: %v", err)
	}
	campaign.Candidates[0].State = CandidateRanked
	for _, state := range []string{CandidateQualified, CandidatePromoted, CandidateObserved, CandidateGuarding, CandidateGuardPassed} {
		campaign.Candidates[1].State, campaign.Candidates[1].LabEvaluationID = state, "optlab-earlier"
		result, err := RankMeasuredCampaign(campaign, benchmarks)
		if err != nil || result.Decisions["candidate-b"] != RankSelect || result.Decisions["candidate-a"] != RankSupersede {
			t.Fatalf("%s winner changed or blocked its peer's ranking: decisions=%v err=%v", state, result.Decisions, err)
		}
	}
}

func TestRankMeasuredCampaignFailsClosedForUnlikeWorkload(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	benchmarks[1].WorkloadJSON = measuredWorkload(t, `{"profile":"interactive","request_count":32,"random_seed":17}`, MeasuredEvidence{Method: MeasurementMethod}, nil)
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-a"] != RankInconclusive || result.Decisions["candidate-b"] != RankInconclusive {
		t.Fatalf("unlike workloads were ranked: %+v", result.Decisions)
	}
}

func TestRankMeasuredCampaignRejectsAllCandidatesOutsideCostBoundary(t *testing.T) {
	maxCost := 1.0
	campaign, benchmarks := rankingFixture(t, "cost-efficiency", &maxCost)
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-a"] != RankReject || result.Decisions["candidate-b"] != RankReject {
		t.Fatalf("cost violations were not rejected: %+v", result.Decisions)
	}
}

func TestRankMeasuredCampaignRejectsMismatchedBenchmarkIdentity(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	benchmarks[0].RevisionID = "another-revision"
	if _, err := RankMeasuredCampaign(campaign, benchmarks); err == nil {
		t.Fatal("mismatched benchmark identity was accepted")
	}
}

func TestRankMeasuredCampaignKeepsPublicPriorScreeningInconclusive(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := optimizer.NewCatalogSource(curatedrecipe.All(), registry.Snapshot()).Propose(t.Context(), optimizer.Request{
		ModelIdentity: "qwen3-8b", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Runtimes: []string{"vllm"},
		Objective: "interactive", WorkloadProfile: "public-interactive", MaxCandidates: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(proposal)
	campaign.ProposalJSON = string(encoded)
	campaign.InputDigest = proposal.InputDigest
	campaign.ModelIdentity = proposal.Input.ModelIdentity
	for index := range benchmarks {
		benchmarks[index].WorkloadJSON = `{"profile":"public-interactive","request_count":100,"random_seed":17}`
		campaign.Candidates[index].ProposalCandidateID = proposal.Candidates[index].ID
	}
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range campaign.Candidates {
		if result.Decisions[candidate.ID] != RankInconclusive || !strings.Contains(result.Reasons[candidate.ID], "customer replay") {
			t.Fatalf("public prior crossed the qualification boundary: decisions=%v reasons=%v", result.Decisions, result.Reasons)
		}
	}
}

func interleaved(t *testing.T, comparison map[string]MetricComparison) string {
	t.Helper()
	return measuredWorkload(t, `{"profile":"interactive","request_count":100,"random_seed":17}`, MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleBracketed, CandidateID: "candidate-a", Runs: 4}, Comparison: comparison}, nil)
}

func TestRankMeasuredCampaignKeepsControlWhenWinIsInsideNoiseFloor(t *testing.T) {
	for name, test := range map[string]struct {
		comparison MetricComparison
		code       string
		reason     string
	}{
		"inside-noise":           {comparison: MetricComparison{ControlMean: 220, CandidateMean: 214, Delta: -6.0 / 220, NoiseFloor: 0.05, Decision: DecisionNoMeasurableDifference}, code: FailureNoMeasurableDifference, reason: "no measurable difference from the control"},
		"same-window-regression": {comparison: MetricComparison{ControlMean: 130, CandidateMean: 140, Delta: 10.0 / 130, NoiseFloor: 0.03, Decision: DecisionRegressed}, code: FailureRegressed, reason: "worse than the control"},
	} {
		t.Run(name, func(t *testing.T) {
			campaign, benchmarks := rankingFixture(t, "interactive", nil)
			benchmarks[1].WorkloadJSON = interleaved(t, map[string]MetricComparison{"ttft_p95_ms": test.comparison})
			result, err := RankMeasuredCampaign(campaign, benchmarks)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decisions["candidate-a"] != RankSelect || result.Decisions["candidate-b"] != RankSupersede || result.FailureCodes["candidate-b"] != test.code {
				t.Fatalf("a point-estimate win that did not beat the control displaced it: decisions=%v codes=%v", result.Decisions, result.FailureCodes)
			}
			if !strings.Contains(result.Reasons["candidate-b"], test.reason) || !strings.Contains(result.Reasons["candidate-a"], "control kept") {
				t.Fatalf("reasons=%v", result.Reasons)
			}
		})
	}
}

// threeCandidateFixture adds a third measured candidate. A is the control; B
// and C were each measured interleaved with it.
func threeCandidateFixture(t *testing.T, ttftB, ttftC float64, comparisonB, comparisonC MetricComparison) (domain.OptimizationCampaign, []domain.BenchmarkResult) {
	t.Helper()
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	var proposal optimizer.Proposal
	if err := json.Unmarshal([]byte(campaign.ProposalJSON), &proposal); err != nil {
		t.Fatal(err)
	}
	third := campaign.Candidates[1]
	third.ID, third.ProposalCandidateID, third.RevisionID, third.BenchmarkID, third.QualityEvidenceID = "candidate-c", proposal.Candidates[1].ID, "revision-c", "benchmark-c", "quality-c"
	campaign.Candidates = append(campaign.Candidates, third)
	benchmarks[1].TTFTP95MS = &ttftB
	benchmarks[1].WorkloadJSON = interleaved(t, map[string]MetricComparison{"ttft_p95_ms": comparisonB})
	row := benchmarks[1]
	row.ID, row.RevisionID, row.TTFTP95MS = "benchmark-c", "revision-c", &ttftC
	row.WorkloadJSON = interleaved(t, map[string]MetricComparison{"ttft_p95_ms": comparisonC})
	return campaign, append(benchmarks, row)
}

func TestRankMeasuredCampaignSelectsBestCandidateThatBeatsTheControl(t *testing.T) {
	// B has the best point estimate but a wide interval, so it is inside the
	// noise floor. C is slower than B on the point estimate but beat the
	// control clearly in its own window. C wins; B is not a win.
	campaign, benchmarks := threeCandidateFixture(t, 120, 160,
		MetricComparison{ControlMean: 220, CandidateMean: 120, Delta: -100.0 / 220, NoiseFloor: 0.6, Decision: DecisionNoMeasurableDifference},
		MetricComparison{ControlMean: 220, CandidateMean: 160, Delta: -60.0 / 220, NoiseFloor: 0.05, Decision: DecisionImproved})
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-c"] != RankSelect || result.Decisions["candidate-b"] != RankSupersede || result.Decisions["candidate-a"] != RankSupersede || result.FailureCodes["candidate-b"] != FailureNoMeasurableDifference {
		t.Fatalf("decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
	}
	if !strings.Contains(result.Reasons["candidate-a"], "measurably beat the control") || !strings.Contains(result.Reasons["candidate-c"], "measurably better than the control") {
		t.Fatalf("reasons=%v", result.Reasons)
	}
}

func TestRankMeasuredCampaignLetsInWindowEvidenceOverrideCrossWindowControlLead(t *testing.T) {
	// The control's own earlier run looks fastest, so Lab picks it, but C beat
	// the control when both were measured in the same window.
	campaign, benchmarks := threeCandidateFixture(t, 240, 230,
		MetricComparison{ControlMean: 250, CandidateMean: 240, Delta: -10.0 / 250, NoiseFloor: 0.06, Decision: DecisionNoMeasurableDifference},
		MetricComparison{ControlMean: 280, CandidateMean: 230, Delta: -50.0 / 280, NoiseFloor: 0.04, Decision: DecisionImproved})
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-c"] != RankSelect || result.Decisions["candidate-a"] != RankSupersede {
		t.Fatalf("a cross-window point estimate kept the control over an in-window win: decisions=%v reasons=%v", result.Decisions, result.Reasons)
	}
}

func TestRankMeasuredCampaignNeverWinsOnAnInvalidInterleavedComparison(t *testing.T) {
	// The control drifted during B's window. B's interval is separated from
	// the control's own earlier interval, but that cross-window comparison
	// must not replace the invalid window.
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	base := `{"profile":"interactive","request_count":100,"random_seed":17}`
	interval := func(mean, half float64) map[string]benchmark.ConfidenceInterval {
		return map[string]benchmark.ConfidenceInterval{"ttft_p95_ms": {Samples: 3, Mean: mean, Lower95: mean - half, Upper95: mean + half}}
	}
	benchmarks[0].WorkloadJSON = measuredWorkload(t, base, MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleCandidate, CandidateID: "candidate-a"}}, interval(220, 8))
	benchmarks[1].WorkloadJSON = measuredWorkload(t, base, MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleInvalid, CandidateID: "candidate-a", Reason: "control output throughput drifted 9.0% between adjacent runs, above the 5% limit"}}, interval(140, 6))
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	// Incomparable evidence stays unknown: it is not "no difference".
	if result.Decisions["candidate-a"] != RankSelect || result.FailureCodes["candidate-b"] != FailureComparisonUnavailable || !strings.Contains(result.Reasons["candidate-b"], "interleaved comparison was invalid") {
		t.Fatalf("an invalid window produced a win: decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
	}
}

// The 2026-11-17 RTX 4090 run: n-gram speculation measured 188 tok/s against
// the control's 246 and p95 latency 1153 ms against 744, but the control's
// throughput drifted 6.9% in that window, so the comparison was invalid and
// the setup was labelled "no real difference". The drifted window's runs
// still show it was worse; they never show it was better (its first token was
// faster).
func TestRankMeasuredCampaignNamesRegressionsSeenInADriftedControlWindow(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	base := `{"profile":"interactive","request_count":121,"random_seed":17}`
	run := func(arm string, tps, ttft, latency float64) RunSummary {
		return RunSummary{Arm: arm, Requests: 121, OutputTokenThroughput: &tps, TTFTP95MS: &ttft, LatencyP95MS: &latency}
	}
	measured := MeasuredEvidence{Method: MeasurementMethod,
		Control: ControlEvidence{Role: ControlRoleInvalid, CandidateID: "candidate-a", Runs: 4, MaxDrift: 0.069, DriftLimit: 0.05, Reason: "control output throughput drifted 6.9% between adjacent runs, above the 5% limit"},
		Runs: []RunSummary{
			run("control", 250, 310, 740), run("candidate", 186, 270, 1150),
			run("control", 240, 320, 750), run("candidate", 190, 280, 1160),
			run("control", 256, 312, 745), run("candidate", 188, 275, 1149),
			run("control", 238, 318, 742),
		},
	}
	benchmarks[0].WorkloadJSON = measuredWorkload(t, base, MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleCandidate, CandidateID: "candidate-a"}}, nil)
	benchmarks[1].WorkloadJSON = measuredWorkload(t, base, measured, nil)
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-a"] != RankSelect || result.Decisions["candidate-b"] != RankSupersede || result.FailureCodes["candidate-b"] != FailureRegressed {
		t.Fatalf("decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
	}
	reason := result.Reasons["candidate-b"]
	for _, want := range []string{"worse than the control", "output_token_throughput changed -23.6%", "latency_p95_ms changed +54.9%", "in runs whose control drifted", "interleaved comparison was invalid"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q lacks %q", reason, want)
		}
	}
	if strings.Contains(reason, "ttft_p95_ms changed") {
		t.Fatalf("a drifted window claimed a first-token result: %q", reason)
	}
	regressions := MeasuredRegressions(&measured)
	if got := DescribeRegressions(regressions); !strings.HasPrefix(got, "throughput got worse by 24% (noise ±") || !strings.Contains(got, ", latency got worse by 55% (noise ±") {
		t.Fatalf("customer words = %q", got)
	}
}

// A window invalidated by a broken control, not by drift, holds runs that
// prove nothing, so no regression is read from it.
func TestMeasuredRegressionsIgnoreAControlThatFailed(t *testing.T) {
	tps, slow := 250.0, 100.0
	evidence := MeasuredEvidence{Control: ControlEvidence{Role: ControlRoleInvalid, CandidateID: "control", Runs: 1, DriftLimit: 0.05, Reason: "control run failed: timeout"},
		Runs: []RunSummary{{Arm: "control", OutputTokenThroughput: &tps}, {Arm: "candidate", OutputTokenThroughput: &slow}, {Arm: "candidate", OutputTokenThroughput: &slow}}}
	if regressions := MeasuredRegressions(&evidence); len(regressions) != 0 {
		t.Fatalf("regressions = %+v", regressions)
	}
	if regressions := MeasuredRegressions(nil); regressions != nil {
		t.Fatalf("regressions = %+v", regressions)
	}
}

func TestRankMeasuredCampaignComparesIntervalsWhenControlWasNotInterleaved(t *testing.T) {
	interval := func(mean, half float64) map[string]benchmark.ConfidenceInterval {
		return map[string]benchmark.ConfidenceInterval{"ttft_p95_ms": {Samples: 3, Mean: mean, Lower95: mean - half, Upper95: mean + half}}
	}
	base := `{"profile":"interactive","request_count":100,"random_seed":17}`
	unavailable := MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleUnavailable, CandidateID: "candidate-a", Reason: "the control candidate is not serving (state cleaned)"}}
	control := MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleCandidate, CandidateID: "candidate-a"}}
	for name, test := range map[string]struct {
		candidate map[string]benchmark.ConfidenceInterval
		winner    string
	}{
		"separated":   {candidate: interval(140, 6), winner: "candidate-b"},
		"overlapping": {candidate: interval(140, 75), winner: "candidate-a"},
		"missing":     {winner: "candidate-a"},
	} {
		t.Run(name, func(t *testing.T) {
			campaign, benchmarks := rankingFixture(t, "interactive", nil)
			benchmarks[0].WorkloadJSON = measuredWorkload(t, base, control, interval(220, 8))
			benchmarks[1].WorkloadJSON = measuredWorkload(t, base, unavailable, test.candidate)
			result, err := RankMeasuredCampaign(campaign, benchmarks)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decisions[test.winner] != RankSelect {
				t.Fatalf("decisions=%v reasons=%v", result.Decisions, result.Reasons)
			}
			code := FailureNoMeasurableDifference
			if name == "missing" {
				code = FailureComparisonUnavailable
			}
			if test.winner == "candidate-a" && (result.FailureCodes["candidate-b"] != code || !strings.Contains(result.Reasons["candidate-b"], "not serving")) {
				t.Fatalf("an unproven candidate did not record why the control was kept: codes=%v reasons=%v", result.FailureCodes, result.Reasons)
			}
		})
	}
}

func TestRankMeasuredCampaignSelectsOnConstraintsWhenControlIsIneligible(t *testing.T) {
	// The control costs more than the requested hourly maximum; the candidate
	// does not. The candidate wins on constraints, without a claimed speedup.
	maxCost := 1.5
	campaign, benchmarks := rankingFixture(t, "interactive", &maxCost)
	benchmarks[1].CostMetadataJSON = costJSON(1.0)
	benchmarks[1].WorkloadJSON = interleaved(t, map[string]MetricComparison{"ttft_p95_ms": {ControlMean: 220, CandidateMean: 214, Delta: -6.0 / 220, NoiseFloor: 0.05, Decision: DecisionNoMeasurableDifference}})
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-b"] != RankSelect || !strings.Contains(result.Reasons["candidate-b"], "no performance gain over it is claimed") {
		t.Fatalf("a control outside the constraints blocked the only eligible candidate: decisions=%v reasons=%v", result.Decisions, result.Reasons)
	}
}

func TestRankMeasuredCampaignScalesCostEfficiencyByControlCost(t *testing.T) {
	// Throughput is inside the noise floor, so only a cost difference can
	// make a measurable cost-efficiency gain.
	inside := map[string]MetricComparison{"output_token_throughput": {HigherIsBetter: true, ControlMean: 78, CandidateMean: 80, Delta: 2.0 / 78, NoiseFloor: 0.05, Decision: DecisionNoMeasurableDifference}}
	for name, test := range map[string]struct {
		candidateCost float64
		winner        string
	}{
		"cheaper":   {candidateCost: 1.0, winner: "candidate-b"},
		"same-cost": {candidateCost: 2.0, winner: "candidate-a"},
	} {
		t.Run(name, func(t *testing.T) {
			campaign, benchmarks := rankingFixture(t, "cost-efficiency", nil)
			benchmarks[1].WorkloadJSON = interleaved(t, inside)
			benchmarks[1].CostMetadataJSON = costJSON(test.candidateCost)
			result, err := RankMeasuredCampaign(campaign, benchmarks)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decisions[test.winner] != RankSelect {
				t.Fatalf("decisions=%v reasons=%v", result.Decisions, result.Reasons)
			}
		})
	}
}

func TestRankMeasuredCampaignPersistsControlAssessmentAndFinalSelection(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	benchmarks[1].WorkloadJSON = interleaved(t, map[string]MetricComparison{"ttft_p95_ms": {ControlMean: 220, CandidateMean: 214, Delta: -6.0 / 220, NoiseFloor: 0.05, Decision: DecisionNoMeasurableDifference}})
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Evaluation.AlgorithmVersion != lab.AlgorithmVersion+"+"+RankingAlgorithmVersion {
		t.Fatalf("algorithm=%s", result.Evaluation.AlgorithmVersion)
	}
	var rows []struct {
		CandidateID      string            `json:"candidate_id"`
		Selected         bool              `json:"selected"`
		CampaignDecision string            `json:"campaign_decision"`
		Control          bool              `json:"control"`
		Decision         string            `json:"control_decision"`
		Basis            string            `json:"control_basis"`
		Comparison       *MetricComparison `json:"objective_comparison"`
	}
	if err = json.Unmarshal([]byte(result.Evaluation.ResultsJSON), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		switch row.CandidateID {
		case "candidate-a":
			if !row.Selected || !row.Control || row.Decision != "control" || row.CampaignDecision != RankSelect {
				t.Fatalf("control row=%+v", row)
			}
		case "candidate-b":
			if row.Selected || row.Decision != DecisionNoMeasurableDifference || row.Basis != basisInterleaved || row.Comparison == nil || row.CampaignDecision != RankSupersede {
				t.Fatalf("candidate row=%+v", row)
			}
		default:
			t.Fatalf("unexpected row %+v", row)
		}
	}
	store := &rankingStoreFixture{campaign: campaign, benchmarks: benchmarks}
	ranked, err := (PersistedRanker{Store: store}).Rank(t.Context(), campaign.Candidates[1])
	if err != nil || ranked.Decision != RankSupersede || ranked.FailureCode != FailureNoMeasurableDifference {
		t.Fatalf("persisted ranking=%+v err=%v", ranked, err)
	}
}

func TestRankMeasuredCampaignKeepsConstraintReasonsWhenNothingIsSelected(t *testing.T) {
	maxCost := 1.0
	campaign, benchmarks := rankingFixture(t, "interactive", &maxCost)
	benchmarks[1].WorkloadJSON = interleaved(t, map[string]MetricComparison{"ttft_p95_ms": {ControlMean: 220, CandidateMean: 214, Delta: -6.0 / 220, NoiseFloor: 0.05, Decision: DecisionNoMeasurableDifference}})
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-b"] != RankReject || result.FailureCodes["candidate-b"] != "" || !strings.Contains(result.Reasons["candidate-b"], "hourly cost") {
		t.Fatalf("the noise gate masked a constraint rejection: decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
	}
}

func TestRankMeasuredCampaignMatchesDeployedArtifactNotTypedAlias(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	if campaign.ModelIdentity != "qwen3-8b" || !strings.Contains(benchmarks[0].ModelIdentity, "@") {
		t.Fatalf("fixture no longer exercises an alias campaign: campaign=%q benchmark=%q", campaign.ModelIdentity, benchmarks[0].ModelIdentity)
	}
	if _, err := RankMeasuredCampaign(campaign, benchmarks); err != nil {
		t.Fatalf("benchmarks for the deployed artifact did not rank an alias campaign: %v", err)
	}
	for index := range benchmarks {
		benchmarks[index].ModelIdentity = campaign.ModelIdentity
	}
	if _, err := RankMeasuredCampaign(campaign, benchmarks); err == nil {
		t.Fatal("benchmarks that do not name the deployed artifact were ranked")
	}
}

func TestRankMeasuredCampaignRejectsMixedModelArtifacts(t *testing.T) {
	campaign, _ := rankingFixture(t, "interactive", nil)
	var proposal optimizer.Proposal
	if err := json.Unmarshal([]byte(campaign.ProposalJSON), &proposal); err != nil {
		t.Fatal(err)
	}
	proposal.Candidates[1].Deployment.Model.Revision = strings.Repeat("c", 40)
	encoded, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	campaign.ProposalJSON = string(encoded)
	if _, err = RankMeasuredCampaign(campaign, nil); !errors.Is(err, optimizer.ErrDifferentBaseModel) {
		t.Fatalf("unmodified candidates serving different artifacts were ranked together: %v", err)
	}
}

// Inside the noise on the objective is not "no difference" when the same
// window shows the setup measurably slower on throughput.
func TestRankMeasuredCampaignNamesAHeadlineRegressionOutsideTheObjective(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	benchmarks[1].WorkloadJSON = interleaved(t, map[string]MetricComparison{
		"ttft_p95_ms":             {ControlMean: 315, CandidateMean: 300, Delta: -15.0 / 315, NoiseFloor: 0.08, Decision: DecisionNoMeasurableDifference},
		"output_token_throughput": {HigherIsBetter: true, ControlMean: 246, CandidateMean: 184, Delta: -0.25, NoiseFloor: 0.017, Decision: DecisionRegressed},
	})
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-a"] != RankSelect || result.FailureCodes["candidate-b"] != FailureRegressed || !strings.Contains(result.Reasons["candidate-b"], "output_token_throughput changed -25.0% against a 1.7% noise floor") {
		t.Fatalf("decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
	}
}
