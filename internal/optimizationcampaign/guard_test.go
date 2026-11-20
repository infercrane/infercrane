package optimizationcampaign

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
)

func tokensPerRequest(value float64) *float64 { return &value }

// guardWindow is candidate B measured in turns with control A, C X C X C X C,
// 100 requests per run. B beat A clearly on p95 time to first token, so only
// a guard can keep it from winning.
func guardWindow(t *testing.T, controlFailedPerRun, candidateFailedPerRun int, controlTokens, candidateTokens *float64) string {
	t.Helper()
	var runs []RunSummary
	for index := range 7 {
		if index%2 == 0 {
			runs = append(runs, RunSummary{Arm: ArmControl, Requests: 100, Failed: controlFailedPerRun, OutputTokensPerRequest: controlTokens})
		} else {
			runs = append(runs, RunSummary{Arm: ArmCandidate, Requests: 100, Failed: candidateFailedPerRun, OutputTokensPerRequest: candidateTokens})
		}
	}
	return measuredWorkload(t, `{"profile":"interactive","request_count":100,"random_seed":17}`, MeasuredEvidence{
		Method: MeasurementMethod, Runs: runs,
		Control:    ControlEvidence{Role: ControlRoleBracketed, CandidateID: "candidate-a", Runs: 4},
		Comparison: map[string]MetricComparison{"ttft_p95_ms": {ControlMean: 220, CandidateMean: 140, Delta: -80.0 / 220, NoiseFloor: 0.04, Decision: DecisionImproved}},
	}, nil)
}

func guardFixture(t *testing.T, controlFailedPerRun, candidateFailedPerRun int, controlTokens, candidateTokens *float64) (domain.OptimizationCampaign, []domain.BenchmarkResult) {
	t.Helper()
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	benchmarks[1].WorkloadJSON = guardWindow(t, controlFailedPerRun, candidateFailedPerRun, controlTokens, candidateTokens)
	benchmarks[1].RequestCount, benchmarks[1].Failed = 300, 3*candidateFailedPerRun
	return campaign, benchmarks
}

func persistedGuardCheck(t *testing.T, evaluation domain.LabEvaluation, candidateID string) *GuardCheck {
	t.Helper()
	var rows []struct {
		CandidateID string      `json:"candidate_id"`
		GuardCheck  *GuardCheck `json:"guard_check"`
	}
	if err := json.Unmarshal([]byte(evaluation.ResultsJSON), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.CandidateID == candidateID {
			return row.GuardCheck
		}
	}
	t.Fatalf("ranking has no row for %s", candidateID)
	return nil
}

func TestRankMeasuredCampaignHoldsBackACandidateThatFailedMoreRequests(t *testing.T) {
	// B is 36% faster to its first token, but it failed 3% of its requests
	// and the control failed none. Failed requests are left out of p95, so
	// its gain may come from failing slow requests.
	campaign, benchmarks := guardFixture(t, 0, 3, tokensPerRequest(200), tokensPerRequest(200))
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-a"] != RankSelect || result.Decisions["candidate-b"] != RankSupersede {
		t.Fatalf("a candidate that failed more requests won: decisions=%v reasons=%v", result.Decisions, result.Reasons)
	}
	if result.FailureCodes["candidate-b"] != FailureErrorRateRegression || !strings.Contains(result.Reasons["candidate-b"], "failed 3.0% of requests; the current setup failed 0.0%") {
		t.Fatalf("the hold did not name the failed requests: codes=%v reasons=%v", result.FailureCodes, result.Reasons)
	}
	if !strings.Contains(result.Reasons["candidate-a"], "without failing more requests or answering shorter") {
		t.Fatalf("the kept control did not say a better candidate was held back: %q", result.Reasons["candidate-a"])
	}
	check := persistedGuardCheck(t, result.Evaluation, "candidate-b")
	if check == nil || check.Basis != GuardBasisSameWindow || check.Control.Requests != 400 || check.Candidate.Failed != 9 || len(check.Guards) != 1 || check.Guards[0].Code != FailureErrorRateRegression {
		t.Fatalf("the ranking did not persist the guard check: %+v", check)
	}
	if !strings.HasSuffix(result.Evaluation.AlgorithmVersion, "+"+RankingAlgorithmVersion) || RankingAlgorithmVersion != "campaign-paired-control-v4" {
		t.Fatalf("the ranking did not name the guarded algorithm: %q", result.Evaluation.AlgorithmVersion)
	}
}

func TestRankMeasuredCampaignHoldsBackACandidateThatAnsweredShorter(t *testing.T) {
	campaign, benchmarks := guardFixture(t, 0, 0, tokensPerRequest(200), tokensPerRequest(164))
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-a"] != RankSelect || result.Decisions["candidate-b"] != RankSupersede || result.FailureCodes["candidate-b"] != FailureOutputLengthRegression {
		t.Fatalf("a candidate with shorter answers won: decisions=%v codes=%v", result.Decisions, result.FailureCodes)
	}
	if want := "answered 18% shorter than the current setup; a speed or cost gain may come from shorter answers"; !strings.Contains(result.Reasons["candidate-b"], want) {
		t.Fatalf("reason %q does not contain %q", result.Reasons["candidate-b"], want)
	}

	// Both guards fire together; the first one names the code.
	campaign, benchmarks = guardFixture(t, 0, 5, tokensPerRequest(200), tokensPerRequest(150))
	if result, err = RankMeasuredCampaign(campaign, benchmarks); err != nil {
		t.Fatal(err)
	}
	if result.FailureCodes["candidate-b"] != FailureErrorRateRegression || !strings.Contains(result.Reasons["candidate-b"], "failed 5.0% of requests") || !strings.Contains(result.Reasons["candidate-b"], "answered 25% shorter") {
		t.Fatalf("both guards were not named: codes=%v reasons=%v", result.FailureCodes, result.Reasons)
	}
}

func TestRankMeasuredCampaignLeavesEqualArmsToTheNoiseGate(t *testing.T) {
	for name, candidateTokens := range map[string]*float64{"same-length": tokensPerRequest(200), "slightly-shorter": tokensPerRequest(191), "longer": tokensPerRequest(260), "length-unknown": nil} {
		t.Run(name, func(t *testing.T) {
			campaign, benchmarks := guardFixture(t, 0, 0, tokensPerRequest(200), candidateTokens)
			result, err := RankMeasuredCampaign(campaign, benchmarks)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decisions["candidate-b"] != RankSelect || result.FailureCodes["candidate-b"] != "" {
				t.Fatalf("a guard held back a candidate that answered as fully: decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
			}
			if check := persistedGuardCheck(t, result.Evaluation, "candidate-b"); check == nil || len(check.Guards) != 0 {
				t.Fatalf("guard check=%+v", check)
			}
		})
	}
}

// TestRankMeasuredCampaignReadsEvidenceStoredBeforeTheGuards decodes a
// measurement and a ranking exactly as an older control plane stored them:
// runs without an answer length. Failed requests are still compared; the
// unknown length fires nothing and stays unknown.
func TestRankMeasuredCampaignReadsEvidenceStoredBeforeTheGuards(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	benchmarks[1].WorkloadJSON = `{"profile":"interactive","request_count":100,"random_seed":17,"measured":{"method":"campaign-interleaved-control-v1","invocation_base":5,"order":["warmup:control","warmup:candidate","control","candidate","control"],` +
		`"runs":[{"arm":"control","requests":100,"failed":0,"duration_seconds":10,"output_token_throughput":60},{"arm":"candidate","requests":100,"failed":0,"duration_seconds":9,"output_token_throughput":80},{"arm":"control","requests":100,"failed":0,"duration_seconds":10,"output_token_throughput":61}],` +
		`"control":{"role":"interleaved","candidate_id":"candidate-a","runs":2,"drift_limit":0.05},"comparison":{"ttft_p95_ms":{"higher_is_better":false,"control_mean":220,"control_spread":0.01,"candidate_mean":140,"candidate_uncertainty":0.02,"delta":-0.36,"noise_floor":0.04,"decision":"improved"}},"server_metrics_requested":false,"gpu_telemetry":"not_collected"}}`
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-b"] != RankSelect {
		t.Fatalf("older evidence no longer ranks: decisions=%v reasons=%v", result.Decisions, result.Reasons)
	}
	check := persistedGuardCheck(t, result.Evaluation, "candidate-b")
	if check == nil || check.Basis != GuardBasisSameWindow || check.Candidate.Requests != 100 || check.Control.Requests != 200 || check.Candidate.OutputTokensPerRequest != nil || check.Control.OutputTokensPerRequest != nil {
		t.Fatalf("older runs were not read with the length unknown: %+v", check)
	}

	stored := `[{"evidence_id":"benchmark-a","candidate_id":"candidate-a","campaign_decision":"SUPERSEDE","control":true,"control_decision":"control"},{"evidence_id":"benchmark-b","candidate_id":"candidate-b","campaign_decision":"SELECT","control":false,"control_decision":"improved"}]`
	decisions, err := RankingDecisions(domain.LabEvaluation{AlgorithmVersion: "inference-lab-v4+campaign-control-noise-floor-v2", ResultsJSON: stored})
	if err != nil || decisions["candidate-b"] != RankSelect || decisions["candidate-a"] != RankSupersede {
		t.Fatalf("a ranking stored before the guards no longer decodes: %v err=%v", decisions, err)
	}
}

func TestRankMeasuredCampaignComparesWithTheControlsOwnRunsWhenMeasuredApart(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	// A measured itself; B was measured without a live control and its
	// interval cleared A's, so only the guard decides.
	benchmarks[0].WorkloadJSON = measuredWorkload(t, `{"profile":"interactive","request_count":100,"random_seed":17}`, MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleCandidate, CandidateID: "candidate-a"}, Runs: []RunSummary{
		{Arm: ArmCandidate, Requests: 100, OutputTokensPerRequest: tokensPerRequest(200)}, {Arm: ArmCandidate, Requests: 100, OutputTokensPerRequest: tokensPerRequest(210)},
	}}, map[string]benchmark.ConfidenceInterval{"ttft_p95_ms": {Samples: 3, Mean: 220, Lower95: 210, Upper95: 230}})
	benchmarks[1].WorkloadJSON = measuredWorkload(t, `{"profile":"interactive","request_count":100,"random_seed":17}`, MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleUnavailable, CandidateID: "candidate-a", Reason: "the control candidate is not serving (state cleaned)"}, Runs: []RunSummary{
		{Arm: ArmCandidate, Requests: 100, OutputTokensPerRequest: tokensPerRequest(150)}, {Arm: ArmCandidate, Requests: 100, OutputTokensPerRequest: tokensPerRequest(160)},
	}}, map[string]benchmark.ConfidenceInterval{"ttft_p95_ms": {Samples: 3, Mean: 140, Lower95: 130, Upper95: 150}})
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-a"] != RankSelect || result.FailureCodes["candidate-b"] != FailureOutputLengthRegression {
		t.Fatalf("decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
	}
	if check := persistedGuardCheck(t, result.Evaluation, "candidate-b"); check == nil || check.Basis != GuardBasisSeparateRuns || *check.Control.OutputTokensPerRequest != 205 || *check.Candidate.OutputTokensPerRequest != 155 {
		t.Fatalf("guard check=%+v", check)
	}
}

// TestRankMeasuredCampaignHoldsBackLabsChoiceWhenTheControlIsIneligible
// covers a control outside the constraints: Lab's choice is made without a
// gain over the control, but a candidate that failed more requests still
// cannot be it. With nothing else eligible it is rejected under its guard.
func TestRankMeasuredCampaignHoldsBackLabsChoiceWhenTheControlIsIneligible(t *testing.T) {
	maxCost := 1.5
	campaign, benchmarks := guardFixture(t, 0, 2, tokensPerRequest(200), tokensPerRequest(200))
	ranking, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if ranking.Decisions["candidate-b"] != RankSupersede {
		t.Fatalf("decisions=%v", ranking.Decisions)
	}
	campaign, benchmarks = rankingFixture(t, "interactive", &maxCost)
	benchmarks[1].CostMetadataJSON = costJSON(1.0)
	benchmarks[1].WorkloadJSON = guardWindow(t, 0, 2, tokensPerRequest(200), tokensPerRequest(200))
	benchmarks[1].RequestCount, benchmarks[1].Failed = 300, 6
	ranking, err = RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if ranking.Decisions["candidate-a"] != RankReject || ranking.Decisions["candidate-b"] != RankReject || ranking.FailureCodes["candidate-b"] != FailureErrorRateRegression || !strings.Contains(ranking.Reasons["candidate-b"], "failed 2.0% of requests") {
		t.Fatalf("an ineligible control let a failing candidate win: decisions=%v codes=%v reasons=%v", ranking.Decisions, ranking.FailureCodes, ranking.Reasons)
	}
	store := &rankingStoreFixture{campaign: campaign, benchmarks: benchmarks}
	persisted, err := PersistedRanker{Store: store}.Rank(t.Context(), campaign.Candidates[1])
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Decision != RankReject || persisted.FailureCode != FailureErrorRateRegression {
		t.Fatalf("the persisted rejection did not name its guard: %+v", persisted)
	}
}

func TestGuardsApplyTheThresholds(t *testing.T) {
	side := func(requests, failed int, length *float64) ArmOutcome {
		return ArmOutcome{Requests: requests, Failed: failed, OutputTokensPerRequest: length}
	}
	for name, test := range map[string]struct {
		candidate, control ArmOutcome
		codes              []string
	}{
		"clean-equal":                        {side(300, 0, tokensPerRequest(200)), side(400, 0, tokensPerRequest(200)), nil},
		"one-percent-over-a-clean-control":   {side(300, 3, nil), side(400, 0, nil), []string{FailureErrorRateRegression}},
		"under-one-percent-clean-control":    {side(300, 2, nil), side(400, 0, nil), nil},
		"exactly-one-point-more":             {side(300, 9, nil), side(400, 8, nil), nil},
		"over-one-point-more":                {side(300, 10, nil), side(400, 8, nil), []string{FailureErrorRateRegression}},
		"one-percent-over-a-failing-control": {side(300, 3, nil), side(400, 2, nil), nil},
		"fewer-failures-than-the-control":    {side(300, 0, nil), side(400, 40, nil), nil},
		"five-percent-shorter":               {side(300, 0, tokensPerRequest(190)), side(400, 0, tokensPerRequest(200)), nil},
		"over-five-percent-shorter":          {side(300, 0, tokensPerRequest(189)), side(400, 0, tokensPerRequest(200)), []string{FailureOutputLengthRegression}},
		"control-length-unknown":             {side(300, 0, tokensPerRequest(100)), side(400, 0, nil), nil},
		"no-requests":                        {side(0, 0, nil), side(400, 0, nil), nil},
		"both":                               {side(300, 30, tokensPerRequest(100)), side(400, 0, tokensPerRequest(200)), []string{FailureErrorRateRegression, FailureOutputLengthRegression}},
	} {
		t.Run(name, func(t *testing.T) {
			fired := guards(test.candidate, test.control)
			var codes []string
			for _, guard := range fired {
				codes = append(codes, guard.Code)
			}
			if strings.Join(codes, ",") != strings.Join(test.codes, ",") {
				t.Fatalf("fired %v, want %v", codes, test.codes)
			}
		})
	}
}

func TestOwnOutcomeWeightsAnswerLengthBySuccessfulRequests(t *testing.T) {
	row := domain.BenchmarkResult{RequestCount: 200, Failed: 50, WorkloadJSON: measuredWorkload(t, `{}`, MeasuredEvidence{Method: MeasurementMethod, Runs: []RunSummary{
		{Arm: ArmCandidate, Requests: 100, Failed: 50, OutputTokensPerRequest: tokensPerRequest(100)},
		{Arm: ArmCandidate, Requests: 100, Failed: 0, OutputTokensPerRequest: tokensPerRequest(250)},
	}}, nil)}
	outcome, ok := OwnOutcome(row)
	if !ok || outcome.Requests != 200 || outcome.Failed != 50 || outcome.OutputTokensPerRequest == nil || *outcome.OutputTokensPerRequest != 200 || *outcome.ErrorRate() != 0.25 {
		t.Fatalf("outcome=%+v ok=%v", outcome, ok)
	}
	// A run whose successful requests reported no count leaves the length
	// unknown; a run that only failed does not.
	row.WorkloadJSON = measuredWorkload(t, `{}`, MeasuredEvidence{Method: MeasurementMethod, Runs: []RunSummary{
		{Arm: ArmCandidate, Requests: 100, Failed: 100}, {Arm: ArmCandidate, Requests: 100, OutputTokensPerRequest: tokensPerRequest(250)},
	}}, nil)
	if outcome, _ = OwnOutcome(row); outcome.OutputTokensPerRequest == nil || *outcome.OutputTokensPerRequest != 250 {
		t.Fatalf("a run without successes changed the length: %+v", outcome)
	}
	row.WorkloadJSON = measuredWorkload(t, `{}`, MeasuredEvidence{Method: MeasurementMethod, Runs: []RunSummary{
		{Arm: ArmCandidate, Requests: 100}, {Arm: ArmCandidate, Requests: 100, OutputTokensPerRequest: tokensPerRequest(250)},
	}}, nil)
	if outcome, _ = OwnOutcome(row); outcome.OutputTokensPerRequest != nil {
		t.Fatalf("a partial length stood in for the mean: %+v", outcome)
	}
	// Evidence without runs falls back to the row's pooled counts.
	if outcome, ok = OwnOutcome(domain.BenchmarkResult{RequestCount: 300, Failed: 3, WorkloadJSON: `{}`}); !ok || outcome.Requests != 300 || outcome.Failed != 3 || outcome.OutputTokensPerRequest != nil {
		t.Fatalf("row fallback=%+v ok=%v", outcome, ok)
	}
	if _, ok = OwnOutcome(domain.BenchmarkResult{WorkloadJSON: `{}`}); ok {
		t.Fatal("a row without requests produced an outcome")
	}
}
