package optimizationevidence

import (
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
