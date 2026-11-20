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

// Repeated benchmarks record their measured Student-t intervals and campaign
// control results beside the load inputs. Those are outputs: two runs of the
// same load must share one workload digest, or the most rigorous evidence can
// never be ranked.
func TestEvaluateRanksRepeatedRunsWhoseMeasuredOutputsDiffer(t *testing.T) {
	first := `{"profile":"interactive","profile_version":"benchmark-profile-v1","concurrency":1,"request_count":100,"profile_runs":3,"confidence_95":{"ttft_p95_ms":{"samples":3,"mean":180,"lower_95":170,"upper_95":190}},"measured":{"control":{"available":true,"drift":0.01}}}`
	second := `{"profile":"interactive","profile_version":"benchmark-profile-v1","concurrency":1,"request_count":100,"profile_runs":3,"confidence_95":{"ttft_p95_ms":{"samples":3,"mean":150,"lower_95":146,"upper_95":154}},"measured":{"control":{"available":false}}}`
	changedRuns := `{"profile":"interactive","profile_version":"benchmark-profile-v1","concurrency":1,"request_count":100,"profile_runs":5,"confidence_95":{"ttft_p95_ms":{"samples":5,"mean":140,"lower_95":138,"upper_95":142}}}`
	rows := []domain.BenchmarkResult{
		{ID: "first", ModelIdentity: "model@commit", RequestCount: 300, TTFTP95MS: number(180), WorkloadJSON: first, CostMetadataJSON: `{}`},
		{ID: "second", ModelIdentity: "model@commit", RequestCount: 300, TTFTP95MS: number(150), WorkloadJSON: second, CostMetadataJSON: `{}`},
	}
	if WorkloadDigest(first) != WorkloadDigest(second) {
		t.Fatal("measured outputs changed the workload digest")
	}
	result, err := Evaluate(Input{ModelIdentity: "model@commit", Objective: ObjectiveLatency}, rows)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []Candidate
	if err = json.Unmarshal([]byte(result.ResultsJSON), &candidates); err != nil || len(candidates) != 2 || !candidates[0].Comparable || candidates[0].EvidenceID != "second" || !candidates[0].Selected {
		t.Fatalf("repeated runs were not ranked: candidates=%#v err=%v", candidates, err)
	}
	if WorkloadDigest(changedRuns) == WorkloadDigest(first) {
		t.Fatal("a different run count must remain a different workload")
	}
}

func TestEvaluateComparesOnlyDerivedArtifactsTheCallerAdmits(t *testing.T) {
	row := func(id, model string, ttft float64) domain.BenchmarkResult {
		return domain.BenchmarkResult{ID: id, ModelIdentity: model, RequestCount: 10, TTFTP95MS: number(ttft), WorkloadJSON: `{"profile":"interactive"}`, CostMetadataJSON: `{"available":false}`}
	}
	rows := []domain.BenchmarkResult{row("base", "model@commit", 300), row("fp8", "model-fp8@build", 200), row("other", "other@commit", 100)}
	result, err := Evaluate(Input{ModelIdentity: "model@commit", DerivedModelIdentities: []string{"model-fp8@build"}}, rows)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []Candidate
	if err = json.Unmarshal([]byte(result.ResultsJSON), &candidates); err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].EvidenceID != "fp8" || !candidates[0].Selected || candidates[0].ModelIdentity != "model-fp8@build" || candidates[1].ModelIdentity != "" {
		t.Fatalf("derived artifact was not compared with its base, or an unrelated model was: %#v", candidates)
	}
	if !strings.Contains(result.InputJSON, `"derived_model_identities":["model-fp8@build"]`) {
		t.Fatalf("evaluation input does not record the admitted artifact: %s", result.InputJSON)
	}
	plain, err := Evaluate(Input{ModelIdentity: "model@commit"}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.ResultsJSON, "fp8") || strings.Contains(plain.InputJSON, "derived_model_identities") || strings.Contains(plain.ResultsJSON, "model_identity") {
		t.Fatalf("an evaluation without derived artifacts changed shape: input=%s results=%s", plain.InputJSON, plain.ResultsJSON)
	}
}

func TestObjectiveMetricFollowsTheWorkloadProfile(t *testing.T) {
	for _, test := range []struct {
		objective, profile, metric string
		higher, costScaled, note   bool
	}{
		{objective: ObjectiveInteractive, profile: "interactive", metric: MetricTTFTP95},
		{objective: ObjectiveLatency, profile: "", metric: MetricTTFTP95},
		{objective: ObjectiveLatency, profile: "public-interactive", metric: MetricTTFTP95},
		{objective: ObjectiveInteractive, profile: "buffered", metric: MetricLatencyP95, note: true},
		{objective: ObjectiveLatency, profile: "buffered", metric: MetricLatencyP95, note: true},
		{objective: ObjectiveThroughput, profile: "buffered", metric: MetricOutputThroughput, higher: true},
		{objective: ObjectiveCostEfficiency, profile: "buffered", metric: MetricOutputThroughput, higher: true, costScaled: true},
		{objective: ObjectiveCostEfficiency, profile: "throughput", metric: MetricOutputThroughput, higher: true, costScaled: true},
	} {
		metric, ok := ObjectiveMetric(test.objective, test.profile)
		if !ok || metric.Name != test.metric || metric.HigherIsBetter != test.higher || metric.CostScaled != test.costScaled || (metric.Note != "") != test.note || metric.Label == "" {
			t.Fatalf("%s on %q: %+v ok=%v", test.objective, test.profile, metric, ok)
		}
	}
	if _, ok := ObjectiveMetric("balanced", "buffered"); ok {
		t.Fatal("an unknown objective has no metric")
	}
}

// Buffered runs record request latency and no time to first token. A latency
// objective on them ranks on p95 request latency and says so on every row.
func TestEvaluateRanksBufferedLatencyObjectiveOnRequestLatency(t *testing.T) {
	workload := `{"profile":"buffered","concurrency":8}`
	rows := []domain.BenchmarkResult{
		{ID: "slow", ModelIdentity: "model@commit", RequestCount: 10, LatencyP95MS: number(1200), OutputTokenThroughput: number(90), WorkloadJSON: workload, CostMetadataJSON: `{"available":false}`},
		{ID: "fast", ModelIdentity: "model@commit", RequestCount: 10, LatencyP95MS: number(800), OutputTokenThroughput: number(60), WorkloadJSON: workload, CostMetadataJSON: `{"available":false}`},
	}
	for _, objective := range []string{ObjectiveInteractive, ObjectiveLatency} {
		result, err := Evaluate(Input{ModelIdentity: "model@commit", Objective: objective, WorkloadProfile: "buffered"}, rows)
		if err != nil {
			t.Fatal(err)
		}
		var candidates []Candidate
		if err = json.Unmarshal([]byte(result.ResultsJSON), &candidates); err != nil || len(candidates) != 2 {
			t.Fatalf("candidates=%#v err=%v", candidates, err)
		}
		if candidates[0].EvidenceID != "fast" || !candidates[0].Selected || *candidates[0].ObjectiveValue != 800 || !strings.Contains(candidates[0].SelectionReason, "(p95 request latency)") {
			t.Fatalf("%s: %#v", objective, candidates[0])
		}
		for _, candidate := range candidates {
			if candidate.ObjectiveMetric != MetricLatencyP95 || candidate.LatencyP95MS == nil {
				t.Fatalf("%s: row does not name the ranked metric: %#v", objective, candidate)
			}
		}
	}
}

// Request latency is the metric only for a non-streaming profile. On any
// other profile a missing time to first token stays unknown.
func TestEvaluateNeverRanksStreamingLatencyOnRequestLatency(t *testing.T) {
	for _, profile := range []string{"", "interactive"} {
		workload := `{"profile":"interactive","concurrency":1}`
		rows := []domain.BenchmarkResult{
			{ID: "no-ttft", ModelIdentity: "model@commit", RequestCount: 10, LatencyP95MS: number(100), WorkloadJSON: workload, CostMetadataJSON: `{"available":false}`},
			{ID: "ttft", ModelIdentity: "model@commit", RequestCount: 10, TTFTP95MS: number(300), LatencyP95MS: number(900), WorkloadJSON: workload, CostMetadataJSON: `{"available":false}`},
		}
		result, err := Evaluate(Input{ModelIdentity: "model@commit", Objective: ObjectiveInteractive, WorkloadProfile: profile}, rows)
		if err != nil {
			t.Fatal(err)
		}
		var candidates []Candidate
		if err = json.Unmarshal([]byte(result.ResultsJSON), &candidates); err != nil || len(candidates) != 2 {
			t.Fatalf("candidates=%#v err=%v", candidates, err)
		}
		if candidates[0].EvidenceID != "ttft" || !candidates[0].Selected || candidates[0].ObjectiveMetric != MetricTTFTP95 || candidates[1].ObjectiveValue != nil || candidates[1].Selected {
			t.Fatalf("profile %q: %#v", profile, candidates)
		}
	}
}
