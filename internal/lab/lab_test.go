package lab

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

func number(v float64) *float64 { return &v }
func TestEvaluateLabelsOnlyMeasuredEvidenceAndNeverInventsCost(t *testing.T) {
	limit := 250.0
	rows := []domain.BenchmarkResult{{ID: "slow", ModelIdentity: "model@commit", DeploymentName: "a", RevisionID: "r1", Runtime: "vllm", RuntimeVersion: "1", Provider: "aws", GPU: "H100", ComputeMode: "elastic", RequestCount: 10, Failed: 1, TTFTP95MS: number(300), WorkloadJSON: `{"requests":10}`, CostMetadataJSON: `{"available":false}`}, {ID: "fast", ModelIdentity: "model@commit", DeploymentName: "b", RevisionID: "r2", Runtime: "sglang", RuntimeVersion: "1", Provider: "gcp", GPU: "H100", ComputeMode: "elastic", RequestCount: 10, TTFTP95MS: number(200), WorkloadJSON: `{"requests":10}`, CostMetadataJSON: `{"available":false}`}, {ID: "other", ModelIdentity: "other", RequestCount: 1, WorkloadJSON: `{}`, CostMetadataJSON: `{}`}}
	result, err := Evaluate(Input{ModelIdentity: "model@commit", MaxTTFTP95MS: &limit}, rows)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []Candidate
	if json.Unmarshal([]byte(result.ResultsJSON), &candidates) != nil || len(candidates) != 2 || candidates[0].EvidenceID != "fast" || candidates[0].EvidenceClass != "measured" || candidates[0].MeetsSLO == nil || !*candidates[0].MeetsSLO || !candidates[0].Selected || candidates[1].ErrorRate != 0.1 {
		t.Fatalf("%#v", candidates)
	}
	if result.InputDigest == "" || result.AlgorithmVersion != AlgorithmVersion {
		t.Fatalf("%#v", result)
	}
}

func TestEvaluateDoesNotRankUnlikeWorkloads(t *testing.T) {
	rows := []domain.BenchmarkResult{
		{ID: "c1", ModelIdentity: "model@commit", RequestCount: 10, TTFTP95MS: number(100), OutputTokenThroughput: number(10), WorkloadJSON: `{"profile":"interactive","concurrency":1}`, CostMetadataJSON: `{"available":false}`},
		{ID: "c32", ModelIdentity: "model@commit", RequestCount: 10, TTFTP95MS: number(500), OutputTokenThroughput: number(500), WorkloadJSON: `{"profile":"throughput","concurrency":32}`, CostMetadataJSON: `{"available":false}`},
	}
	result, err := Evaluate(Input{ModelIdentity: "model@commit", Objective: ObjectiveThroughput}, rows)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []Candidate
	if err = json.Unmarshal([]byte(result.ResultsJSON), &candidates); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.Comparable || candidate.Selected || candidate.SelectionReason == "" {
			t.Fatalf("unlike workload was ranked: %#v", candidates)
		}
	}
}

func TestEvaluateSelectsMeasuredObjectiveWithinExactWorkload(t *testing.T) {
	workload := `{"profile":"throughput","concurrency":32}`
	rows := []domain.BenchmarkResult{
		{ID: "lower", ModelIdentity: "model@commit", RequestCount: 10, OutputTokenThroughput: number(100), WorkloadJSON: workload, CostMetadataJSON: `{"available":true,"hourly":1,"source":"aws-price"}`},
		{ID: "higher", ModelIdentity: "model@commit", RequestCount: 10, OutputTokenThroughput: number(180), WorkloadJSON: workload, CostMetadataJSON: `{"available":true,"hourly":3,"source":"aws-price"}`},
	}
	result, err := Evaluate(Input{ModelIdentity: "model@commit", Objective: ObjectiveThroughput, WorkloadProfile: "throughput"}, rows)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []Candidate
	if err = json.Unmarshal([]byte(result.ResultsJSON), &candidates); err != nil || len(candidates) != 2 || candidates[0].EvidenceID != "higher" || !candidates[0].Selected {
		t.Fatalf("throughput result=%#v err=%v", candidates, err)
	}
	result, err = Evaluate(Input{ModelIdentity: "model@commit", Objective: ObjectiveCostEfficiency, WorkloadProfile: "throughput"}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(result.ResultsJSON), &candidates); err != nil || candidates[0].EvidenceID != "lower" || !candidates[0].Selected {
		t.Fatalf("cost-efficiency result=%#v err=%v", candidates, err)
	}
}

func TestEvaluateComparesActiveAndCandidateWithIdenticalLoad(t *testing.T) {
	active := `{"profile":"interactive","profile_version":"benchmark-profile-v1","concurrency":1,"input_tokens":512,"output_tokens":128,"revision_selector":"active","direct_revision_validation":false}`
	candidate := `{"profile":"interactive","profile_version":"benchmark-profile-v1","concurrency":1,"input_tokens":512,"output_tokens":128,"revision_selector":"candidate","direct_revision_validation":true}`
	changedLoad := `{"profile":"interactive","profile_version":"benchmark-profile-v1","concurrency":2,"input_tokens":512,"output_tokens":128,"revision_selector":"candidate","direct_revision_validation":true}`
	rows := []domain.BenchmarkResult{
		{ID: "active", ModelIdentity: "model@commit", RequestCount: 10, TTFTP95MS: number(180), WorkloadJSON: active, CostMetadataJSON: `{}`},
		{ID: "candidate", ModelIdentity: "model@commit", RequestCount: 10, TTFTP95MS: number(160), WorkloadJSON: candidate, CostMetadataJSON: `{}`},
	}
	result, err := Evaluate(Input{ModelIdentity: "model@commit", Objective: ObjectiveLatency}, rows)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []Candidate
	if err = json.Unmarshal([]byte(result.ResultsJSON), &candidates); err != nil || len(candidates) != 2 || !candidates[0].Comparable || candidates[0].EvidenceID != "candidate" || !candidates[0].Selected || candidates[0].WorkloadDigest != candidates[1].WorkloadDigest {
		t.Fatalf("active/candidate load was not compared: candidates=%#v err=%v", candidates, err)
	}
	rows = append(rows, domain.BenchmarkResult{ID: "changed", ModelIdentity: "model@commit", RequestCount: 10, TTFTP95MS: number(100), WorkloadJSON: changedLoad, CostMetadataJSON: `{}`})
	result, err = Evaluate(Input{ModelIdentity: "model@commit", Objective: ObjectiveLatency}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(result.ResultsJSON), &candidates); err != nil {
		t.Fatal(err)
	}
	for _, row := range candidates {
		if row.Comparable || row.Selected {
			t.Fatalf("changed concurrency was falsely compared: %#v", candidates)
		}
	}
}

func TestEvaluateSelectsOnlyMeasuredCandidateSatisfyingAllConstraints(t *testing.T) {
	workload := `{"profile":"interactive","concurrency":1}`
	maxTTFT, maxTPOT, maxErrors := 200.0, 25.0, 0.01
	minGoodput, minThroughput, maxCost := 4.0, 80.0, 3.0
	maxGPUs := 1
	gpuCount := 1
	rows := []domain.BenchmarkResult{
		{ID: "fast-expensive", ModelIdentity: "model@commit", Provider: "aws", Region: "eu-central-1", GPU: "H100", GPUCount: &gpuCount, RequestCount: 100, TTFTP95MS: number(140), TPOTP95MS: number(18), Goodput: number(5), OutputTokenThroughput: number(100), WorkloadJSON: workload, CostMetadataJSON: `{"available":true,"hourly":4,"source":"aws-price"}`},
		{ID: "qualified", ModelIdentity: "model@commit", Provider: "aws", Region: "eu-central-1", GPU: "L40S", GPUCount: &gpuCount, RequestCount: 100, TTFTP95MS: number(180), TPOTP95MS: number(20), Goodput: number(4.5), OutputTokenThroughput: number(90), WorkloadJSON: workload, CostMetadataJSON: `{"available":true,"hourly":2,"source":"aws-price"}`},
		{ID: "missing-tpot", ModelIdentity: "model@commit", Provider: "aws", Region: "eu-central-1", GPU: "L4", GPUCount: &gpuCount, RequestCount: 100, TTFTP95MS: number(130), Goodput: number(6), OutputTokenThroughput: number(120), WorkloadJSON: workload, CostMetadataJSON: `{"available":true,"hourly":1,"source":"aws-price"}`},
		{ID: "missing-error-sample", ModelIdentity: "model@commit", Provider: "aws", Region: "eu-central-1", GPU: "L4", GPUCount: &gpuCount, RequestCount: 0, TTFTP95MS: number(130), TPOTP95MS: number(19), Goodput: number(6), OutputTokenThroughput: number(120), WorkloadJSON: workload, CostMetadataJSON: `{"available":true,"hourly":1,"source":"aws-price"}`},
	}
	result, err := Evaluate(Input{ModelIdentity: "model@commit", Objective: ObjectiveInteractive, WorkloadProfile: "interactive", MaxTTFTP95MS: &maxTTFT, MaxTPOTP95MS: &maxTPOT, MaxErrorRate: &maxErrors, MinGoodput: &minGoodput, MinOutputTPS: &minThroughput, MaxHourlyCost: &maxCost, Region: "eu-central-1", MaxGPUCount: &maxGPUs}, rows)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []Candidate
	if err = json.Unmarshal([]byte(result.ResultsJSON), &candidates); err != nil || len(candidates) != 4 {
		t.Fatalf("candidates=%#v err=%v", candidates, err)
	}
	selected := 0
	for _, candidate := range candidates {
		if candidate.Selected {
			selected++
			if candidate.EvidenceID != "qualified" || candidate.MeetsSLO == nil || !*candidate.MeetsSLO {
				t.Fatalf("unexpected selected candidate: %#v", candidate)
			}
		}
		if candidate.EvidenceID == "fast-expensive" && (candidate.MeetsSLO == nil || *candidate.MeetsSLO || len(candidate.ConstraintReasons) == 0) {
			t.Fatalf("cost constraint was not enforced: %#v", candidate)
		}
		if candidate.EvidenceID == "missing-tpot" && (candidate.MeetsSLO == nil || *candidate.MeetsSLO || len(candidate.ConstraintReasons) == 0) {
			t.Fatalf("missing evidence did not fail closed: %#v", candidate)
		}
		if candidate.EvidenceID == "missing-error-sample" && (candidate.MeetsSLO == nil || *candidate.MeetsSLO || !strings.Contains(strings.Join(candidate.ConstraintReasons, " "), "error rate evidence unavailable")) {
			t.Fatalf("missing error-rate sample did not fail closed: %#v", candidate)
		}
	}
	if selected != 1 {
		t.Fatalf("selected=%d candidates=%#v", selected, candidates)
	}
}
