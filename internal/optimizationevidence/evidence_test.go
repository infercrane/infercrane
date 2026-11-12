package optimizationevidence

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

func qwenCampaign(t *testing.T) Campaign {
	t.Helper()
	body, err := os.ReadFile("testdata/qwen38-modal-screening.json")
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	return campaign
}

func resultByID(t *testing.T, result Evaluation, id string) CandidateResult {
	t.Helper()
	for _, candidate := range result.Candidates {
		if candidate.CandidateID == id {
			return candidate
		}
	}
	t.Fatalf("candidate %q missing from evaluation", id)
	return CandidateResult{}
}

func TestCorrectedQwenFixtureQualifiesBeforeScoring(t *testing.T) {
	result, err := Evaluate(qwenCampaign(t))
	if err != nil {
		t.Fatal(err)
	}
	if result.WinnerID != "sglang-control" || result.AlgorithmVersion != AlgorithmVersion || result.InputDigest == "" {
		t.Fatalf("wrong reproducible winner: %+v", result)
	}
	sglang := resultByID(t, result, "sglang-control")
	wantMean := (78.28892565593024 + 266.27972635035263 + 376.78012546496274) / 3
	if !sglang.Qualified || !sglang.Selected || sglang.Score == nil || math.Abs(*sglang.Score-wantMean) > 1e-12 {
		t.Fatalf("SGLang score=%+v want %.15f", sglang, wantMean)
	}
	vllm := resultByID(t, result, "vllm-control")
	if vllm.Qualified || vllm.Score != nil || !containsReason(vllm.RejectionReasons, "c4: SLO attainment below policy") || !containsReason(vllm.RejectionReasons, "c4: TTFT p95 exceeds policy") {
		t.Fatalf("vLLM c8 throughput was incorrectly allowed to bypass c4 gates: %+v", vllm)
	}
	mtp := resultByID(t, result, "vllm-mtp2")
	if mtp.Qualified || mtp.Score != nil || !containsReason(mtp.RejectionReasons, "c8: SLO attainment below policy") {
		t.Fatalf("native MTP was incorrectly scored: %+v", mtp)
	}
	fp8 := resultByID(t, result, "vllm-mtp2-fp8kv")
	if fp8.Qualified || fp8.Score != nil || !containsReason(fp8.RejectionReasons, "c8: SLO attainment below policy") {
		t.Fatalf("MTP+FP8 KV was incorrectly scored: %+v", fp8)
	}
}

func TestExternalUnverifiedAndMissingLaneNeverReceiveScore(t *testing.T) {
	campaign := qwenCampaign(t)
	campaign.Candidates[0].EvidenceState = StateExternalUnverified
	campaign.Candidates[1].Lanes = campaign.Candidates[1].Lanes[:2]
	result, err := Evaluate(campaign)
	if err != nil {
		t.Fatal(err)
	}
	external := resultByID(t, result, "sglang-control")
	if external.Qualified || external.Score != nil || !containsReason(external.RejectionReasons, "external_unverified") {
		t.Fatalf("external evidence crossed qualification boundary: %+v", external)
	}
	missing := resultByID(t, result, "vllm-control")
	if missing.Qualified || missing.Score != nil || !containsReason(missing.RejectionReasons, "required concurrency lane missing: 8") {
		t.Fatalf("missing lane crossed qualification boundary: %+v", missing)
	}
}

func TestSelectionPolicyCannotDisableQualificationBoundary(t *testing.T) {
	campaign := qwenCampaign(t)
	campaign.Selection.QualificationBeforeScoring = false
	if _, err := Evaluate(campaign); err == nil || !strings.Contains(err.Error(), "qualification-before-scoring") {
		t.Fatalf("unsafe selection policy accepted: %v", err)
	}
}

func TestEvidenceLevelSuccessfulFractionAndCostAreHardGates(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Campaign)
		reason string
	}{
		{name: "evidence level", mutate: func(c *Campaign) { c.Candidates[0].EvidenceLevel = LevelQualification }, reason: "benchmark evidence level does not match selection policy"},
		{name: "successful fraction", mutate: func(c *Campaign) { c.Candidates[0].Lanes[0].SuccessfulRequests = 10 }, reason: "c1: successful fraction below policy"},
		{name: "cost tie breaker", mutate: func(c *Campaign) { c.Candidates[0].Lanes[0].CostPerMillionSuccessfulOutputTokens = 0 }, reason: "c1: positive measured COGS required by cost tie-breaker"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			campaign := qwenCampaign(t)
			test.mutate(&campaign)
			result, err := Evaluate(campaign)
			if err != nil {
				t.Fatal(err)
			}
			candidate := resultByID(t, result, "sglang-control")
			if candidate.Qualified || candidate.Score != nil || !containsReason(candidate.RejectionReasons, test.reason) {
				t.Fatalf("hard gate was bypassed: %+v", candidate)
			}
		})
	}
}

func TestEvidenceLevelLabelsCannotWeakenSampleFloors(t *testing.T) {
	campaign := qwenCampaign(t)
	campaign.Selection.EvidenceLevel = LevelQualification
	for index := range campaign.Candidates {
		campaign.Candidates[index].EvidenceLevel = LevelQualification
	}
	if _, err := Evaluate(campaign); err == nil || !strings.Contains(err.Error(), "qualification requires") {
		t.Fatalf("screening-sized sample was relabeled as qualification: %v", err)
	}

	campaign.Selection.EvidenceLevel = LevelPublic
	for index := range campaign.Candidates {
		campaign.Candidates[index].EvidenceLevel = LevelPublic
	}
	campaign.SLO.MinimumRequestsPerLane = 300
	campaign.SLO.MinimumIndependentRuns = 3
	campaign.SLO.MinimumMeasurementSeconds = 599
	if _, err := Evaluate(campaign); err == nil || !strings.Contains(err.Error(), "public evidence requires") {
		t.Fatalf("short campaign was relabeled as public: %v", err)
	}
}

func TestDecodeRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	body, err := os.ReadFile("testdata/qwen38-modal-screening.json")
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(body), `"workload_id":`, `"unexpected":true,"workload_id":`, 1)
	if _, err = Decode([]byte(unknown)); err == nil {
		t.Fatal("unknown field was accepted")
	}
	if _, err = Decode(append(body, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
}

func containsReason(reasons []string, wanted string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, wanted) {
			return true
		}
	}
	return false
}

const qwen38PairedH200Receipt = "../../docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T044530Z.json"

func TestHarnessReceiptDecodesAndKeepsScreeningBoundary(t *testing.T) {
	body, err := os.ReadFile(qwen38PairedH200Receipt)
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := Decode(body)
	if err != nil {
		t.Fatalf("harness receipt was rejected: %v", err)
	}
	lane := campaign.Candidates[0].Lanes[0]
	if lane.InputTokens == 0 || lane.OutputTokens == 0 || lane.Economics == nil || lane.Economics.TargetUtilization == 0 {
		t.Fatalf("harness context was dropped: %+v", lane)
	}
	result, err := Evaluate(campaign)
	if err != nil {
		t.Fatal(err)
	}
	if result.WinnerID != "sglang-0520-nextn-k4-bounded-graphs" || result.SelectionPolicy.EvidenceLevel != LevelScreening {
		t.Fatalf("receipt changed evidence outcome: %+v", result)
	}
}

func TestLaneContextStaysStrict(t *testing.T) {
	body, err := os.ReadFile(qwen38PairedH200Receipt)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(body), `"target_utilization"`, `"unreviewed_field": 1, "target_utilization"`, 1)
	if _, err = Decode([]byte(unknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown economics field was accepted: %v", err)
	}
	campaign, err := Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	campaign.Candidates[0].Lanes[0].Economics.ProjectedGrossMargin = math.Inf(-1)
	if err = campaign.Validate(); err == nil || !strings.Contains(err.Error(), "projected_gross_margin") {
		t.Fatalf("non-finite margin was accepted: %v", err)
	}
	campaign.Candidates[0].Lanes[0].Economics = nil
	campaign.Candidates[0].Lanes[0].InputTokens = -1
	if err = campaign.Validate(); err == nil || !strings.Contains(err.Error(), "tokens") {
		t.Fatalf("negative tokens were accepted: %v", err)
	}
}

// baselineCampaign has one qualified baseline and one qualified challenger
// whose scores on the objective are exactly base and challenger.
func baselineCampaign(objective string, base, challenger float64) Campaign {
	const workload = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	candidate := func(id string, score float64) Candidate {
		lane := LaneEvidence{Concurrency: 1, Requests: 12, SuccessfulRequests: 12, IndependentRuns: 1, MeasurementSeconds: 60, SLOAttainment: 1,
			TTFTP95MS: 100, ITLP95MS: 10, AggregateOutputTokensSecond: 100, RequestThroughput: 1, SLOGoodput: 1, SLOQualifiedOutputTokensSecond: 100,
			GPUSecondsPerSuccessfulRequest: 5, CostPerMillionSuccessfulOutputTokens: 2}
		if objective == ObjectiveMinimumCOGS {
			lane.CostPerMillionSuccessfulOutputTokens = score
		} else {
			lane.SLOQualifiedOutputTokensSecond = score
		}
		return Candidate{ID: id, RuntimeID: "runtime", Origin: "infercrane_generated", EvidenceLevel: LevelScreening, EvidenceState: StateReproduced,
			RecipeDigest: "sha256:" + strings.Repeat(id[:1], 64), WorkloadDigest: workload, QualityPassed: true,
			QualityGates: []QualityGate{{Name: "parity", Passed: true}}, Lanes: []LaneEvidence{lane}}
	}
	return Campaign{SchemaVersion: Schema, ModelIdentity: "model@rev", HardwareIdentity: "gpu", WorkloadID: "workload", WorkloadDigest: workload, RequiredLanes: []int{1},
		SLO: SLOPolicy{Scope: "each_concurrency_lane", MaxTTFTMS: 1000, MaxITLMS: 50, MinSuccessfulFraction: 0.99, MaxErrorRate: 0.01,
			RequiredQualityGates: []string{"parity"}, MinimumRequestsPerLane: 12, MinimumIndependentRuns: 1},
		Selection: SelectionPolicy{ID: "policy", Version: 1, Objective: objective, LaneAggregation: "arithmetic_mean", EvidenceLevel: LevelScreening,
			QualificationBeforeScoring: true, BaselineCandidateID: "base"},
		Candidates: []Candidate{candidate("base", base), candidate("challenger", challenger)}}
}

func TestBaselineDecisionAppliesNoiseFloorInObjectiveDirection(t *testing.T) {
	tests := []struct {
		name       string
		objective  string
		base, next float64
		floor      float64
		outcome    string
		gain       float64
	}{
		{"throughput lead inside floor", ObjectiveQualifiedOutputThroughput, 100, 104, 0, DecisionKeepBaseline, 0.04},
		{"throughput lead at floor", ObjectiveQualifiedOutputThroughput, 100, 105, 0, DecisionKeepBaseline, 0.05},
		{"throughput lead above floor", ObjectiveQualifiedOutputThroughput, 100, 106, 0, DecisionPromote, 0.06},
		{"throughput loss", ObjectiveQualifiedOutputThroughput, 100, 80, 0, DecisionKeepBaseline, -0.2},
		{"raised floor keeps 8% lead", ObjectiveQualifiedOutputThroughput, 100, 108, 0.1, DecisionKeepBaseline, 0.08},
		{"cogs saving above floor", ObjectiveMinimumCOGS, 2, 1.85, 0, DecisionPromote, 0.075},
		{"cogs saving inside floor", ObjectiveMinimumCOGS, 2, 1.95, 0, DecisionKeepBaseline, 0.025},
		{"cogs increase", ObjectiveMinimumCOGS, 2, 2.5, 0, DecisionKeepBaseline, -0.25},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			campaign := baselineCampaign(test.objective, test.base, test.next)
			campaign.Selection.MinimumRelativeGain = test.floor
			result, err := Evaluate(campaign)
			if err != nil {
				t.Fatal(err)
			}
			decision := result.BaselineDecision
			if decision == nil || decision.Outcome != test.outcome || decision.RelativeGain == nil || math.Abs(*decision.RelativeGain-test.gain) > 1e-12 || decision.ChallengerID != "challenger" || !decision.BaselineQualified {
				t.Fatalf("decision=%+v", decision)
			}
			if decision.NoiseFloor != math.Max(test.floor, DefaultMinimumRelativeGain) || decision.AlgorithmVersion != BaselineAlgorithmVersion {
				t.Fatalf("noise floor or algorithm not recorded: %+v", decision)
			}
			want := "base"
			if test.outcome == DecisionPromote {
				want = "challenger"
			}
			base, challenger := resultByID(t, result, "base"), resultByID(t, result, "challenger")
			if result.WinnerID != want || decision.CandidateID != want || base.Selected != (want == "base") || challenger.Selected != (want == "challenger") {
				t.Fatalf("served candidate %q, want %q: %+v", result.WinnerID, want, result)
			}
			if !base.Baseline || challenger.Baseline || base.RelativeGainOverBaseline != nil || challenger.RelativeGainOverBaseline == nil {
				t.Fatalf("baseline role or per-candidate gain missing: base=%+v challenger=%+v", base, challenger)
			}
		})
	}
}

func TestBaselineDecisionNeverInventsAGain(t *testing.T) {
	failing := baselineCampaign(ObjectiveQualifiedOutputThroughput, 100, 90)
	failing.Candidates[0].QualityGates[0].Passed = false
	result, err := Evaluate(failing)
	if err != nil {
		t.Fatal(err)
	}
	if decision := result.BaselineDecision; decision.Outcome != DecisionPromote || result.WinnerID != "challenger" || decision.RelativeGain != nil || decision.BaselineQualified || !strings.Contains(decision.Reason, "did not qualify") {
		t.Fatalf("unqualified baseline: %+v", decision)
	}
	if resultByID(t, result, "challenger").RelativeGainOverBaseline != nil {
		t.Fatal("a gain was computed against an unqualified baseline")
	}

	failing.Candidates[1].QualityPassed = false
	result, err = Evaluate(failing)
	if err != nil {
		t.Fatal(err)
	}
	if decision := result.BaselineDecision; decision.Outcome != DecisionRejectAll || result.WinnerID != "" || decision.CandidateID != "" || decision.ChallengerID != "" {
		t.Fatalf("nothing qualified: %+v", decision)
	}

	alone := baselineCampaign(ObjectiveQualifiedOutputThroughput, 100, 900)
	alone.Candidates[1].ErrorRate = 0.5
	result, err = Evaluate(alone)
	if err != nil {
		t.Fatal(err)
	}
	if decision := result.BaselineDecision; decision.Outcome != DecisionKeepBaseline || result.WinnerID != "base" || decision.RelativeGain != nil || decision.ChallengerID != "" {
		t.Fatalf("only the baseline qualified: %+v", decision)
	}

	zero := baselineCampaign(ObjectiveMinimumCOGS, 0, 1)
	result, err = Evaluate(zero)
	if err != nil {
		t.Fatal(err)
	}
	if decision := result.BaselineDecision; decision.Outcome != DecisionKeepBaseline || decision.RelativeGain != nil || !strings.Contains(decision.Reason, "undefined") {
		t.Fatalf("zero-score baseline: %+v", decision)
	}
}

func TestBaselinePolicyCannotLowerNoiseFloor(t *testing.T) {
	tests := map[string]func(*Campaign){
		"unknown baseline":       func(c *Campaign) { c.Selection.BaselineCandidateID = "missing" },
		"padded baseline":        func(c *Campaign) { c.Selection.BaselineCandidateID = " base" },
		"floor below default":    func(c *Campaign) { c.Selection.MinimumRelativeGain = 0.03 },
		"floor of one":           func(c *Campaign) { c.Selection.MinimumRelativeGain = 1 },
		"NaN floor":              func(c *Campaign) { c.Selection.MinimumRelativeGain = math.NaN() },
		"floor without baseline": func(c *Campaign) { c.Selection.BaselineCandidateID, c.Selection.MinimumRelativeGain = "", 0.1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			campaign := baselineCampaign(ObjectiveQualifiedOutputThroughput, 100, 110)
			mutate(&campaign)
			if _, err := Evaluate(campaign); err == nil {
				t.Fatalf("unsafe baseline policy accepted: %+v", campaign.Selection)
			}
		})
	}
}

func TestEvidenceWithoutBaselineKeepsAbsoluteSelection(t *testing.T) {
	campaign := qwenCampaign(t)
	encoded, err := json.Marshal(campaign.Selection)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "baseline") || strings.Contains(string(encoded), "minimum_relative_gain") {
		t.Fatalf("absent baseline fields changed the encoded policy and input digest: %s", encoded)
	}
	result, err := Evaluate(campaign)
	if err != nil {
		t.Fatal(err)
	}
	if result.BaselineDecision != nil || result.WinnerID != "sglang-control" {
		t.Fatalf("evidence without a baseline changed behavior: %+v", result)
	}
	for _, candidate := range result.Candidates {
		if candidate.Baseline || candidate.RelativeGainOverBaseline != nil {
			t.Fatalf("baseline fields set without a baseline: %+v", candidate)
		}
	}
	body, err := os.ReadFile("testdata/qwen38-modal-screening.json")
	if err != nil {
		t.Fatal(err)
	}
	declared := strings.Replace(string(body), `"cost_tie_breaker": true`, `"cost_tie_breaker": true, "baseline_candidate_id": "sglang-control", "minimum_relative_gain": 0.08`, 1)
	decoded, err := Decode([]byte(declared))
	if err != nil {
		t.Fatalf("baseline fields were rejected: %v", err)
	}
	if decoded.Selection.BaselineCandidateID != "sglang-control" || decoded.Selection.NoiseFloor() != 0.08 {
		t.Fatalf("baseline fields were not decoded: %+v", decoded.Selection)
	}
}
