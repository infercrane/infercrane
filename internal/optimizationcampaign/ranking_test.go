package optimizationcampaign

import (
	"encoding/json"
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
		{ID: "benchmark-a", RevisionID: "revision-a", ModelIdentity: campaign.ModelIdentity, Provider: "aws", Region: "eu-central-1", GPU: "L40S", Runtime: "vllm", ComputeMode: "elastic", RequestCount: 100, TTFTP95MS: &ttftA, OutputTokenThroughput: &throughputA, WorkloadJSON: controlWorkload, CostMetadataJSON: costJSON(costA)},
		{ID: "benchmark-b", RevisionID: "revision-b", ModelIdentity: campaign.ModelIdentity, Provider: "aws", Region: "eu-central-1", GPU: "L40S", Runtime: "vllm", ComputeMode: "elastic", RequestCount: 100, TTFTP95MS: &ttftB, OutputTokenThroughput: &throughputB, WorkloadJSON: candidateWorkload, CostMetadataJSON: costJSON(costB)},
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
		"same-window-regression": {comparison: MetricComparison{ControlMean: 130, CandidateMean: 140, Delta: 10.0 / 130, NoiseFloor: 0.03, Decision: DecisionRegressed}, reason: "worse than the control"},
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
	if result.Decisions["candidate-a"] != RankSelect || result.FailureCodes["candidate-b"] != FailureNoMeasurableDifference || !strings.Contains(result.Reasons["candidate-b"], "interleaved comparison was invalid") {
		t.Fatalf("an invalid window produced a win: decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
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
			if test.winner == "candidate-a" && (result.FailureCodes["candidate-b"] != FailureNoMeasurableDifference || !strings.Contains(result.Reasons["candidate-b"], "not serving")) {
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
