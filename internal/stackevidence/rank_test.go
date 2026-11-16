package stackevidence

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func ids(result Result) []string {
	out := make([]string, 0, len(result.Stacks))
	for _, row := range result.Stacks {
		out = append(out, row.ID)
	}
	return out
}

func equal(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

func testPrices(gpu string, count int) (PriceQuote, bool) {
	observed := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	switch {
	case gpu == "H100" && count == 1:
		return PriceQuote{Provider: "runpod", Region: "global", HourlyUSD: 2, Source: "https://prices.example/h100", Authority: "provider_api", ObservedAt: observed}, true
	case gpu == "H200" && count == 2:
		return PriceQuote{Provider: "aws", Region: "us-east-1", HourlyUSD: 8, Source: "https://prices.example/h200", Authority: "provider_api", ObservedAt: observed}, true
	}
	return PriceQuote{}, false
}

func rank(t *testing.T, dataset Dataset, query Query, prices Prices) Result {
	t.Helper()
	result, err := dataset.Rank(query, prices, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRankHighestThroughputOrdersByPerGPUAndPutsStaleLast(t *testing.T) {
	t.Parallel()
	result := rank(t, testDataset(t), Query{Model: "org/model-a", Goal: GoalHighestThroughput}, nil)
	if want := []string{"a-mi300x-vllm-unknown-ttft", "a-h100-vllm", "a-h200-sglang", "a-h100-old"}; !equal(ids(result), want) {
		t.Fatalf("order = %v, want %v", ids(result), want)
	}
	if result.Model.Match != MatchExact || result.Workload.Match != MatchExact || result.Workload.InputTokens != 1024 {
		t.Fatalf("match = %+v workload = %+v", result.Model, result.Workload)
	}
	if !result.Stacks[3].Stale || result.Stacks[0].Stale {
		t.Fatal("stale flags are wrong")
	}
	h200 := result.Stacks[2]
	if h200.OperatingPoint.Concurrency != 64 || h200.Metrics.OutputTokensPerSecond != 3000 || h200.Metrics.OutputTokensPerSecondPerGPU != 1500 {
		t.Fatalf("h200 point = %+v metrics = %+v", h200.OperatingPoint, h200.Metrics)
	}
	if len(h200.Metrics.Derived) != 1 || h200.Metrics.Derived[0] != "output_tokens_per_second" {
		t.Fatalf("derived = %v", h200.Metrics.Derived)
	}
	for index, row := range result.Stacks {
		if row.Rank != index+1 || row.RankBasis != "output_tokens_per_second_per_gpu" || row.Target != TargetNotGiven {
			t.Fatalf("row %d: rank %d basis %s target %s", index, row.Rank, row.RankBasis, row.Target)
		}
		if row.Cost.State != CostUnknown || row.Cost.PerMillionOutputTokensUSD != nil {
			t.Fatalf("cost without prices must stay unknown: %+v", row.Cost)
		}
	}
	if result.Total != 4 || len(result.OtherWorkloads) != 1 || result.OtherWorkloads[0].InputTokens != 8192 {
		t.Fatalf("total %d other %v", result.Total, result.OtherWorkloads)
	}
}

func TestRankLowestCostEstimatesFromCitedThroughputAndCurrentPrice(t *testing.T) {
	t.Parallel()
	result := rank(t, testDataset(t), Query{Model: "org/model-a", Goal: GoalLowestCost}, testPrices)
	if want := []string{"a-h100-vllm", "a-h200-sglang", "a-mi300x-vllm-unknown-ttft", "a-h100-old"}; !equal(ids(result), want) {
		t.Fatalf("order = %v, want %v", ids(result), want)
	}
	h100 := result.Stacks[0]
	want := 2.0 / (2000 * 3600) * 1_000_000
	if h100.Cost.State != CostEstimated || math.Abs(*h100.Cost.PerMillionOutputTokensUSD-want) > 0.0001 {
		t.Fatalf("h100 cost = %+v, want %.4f", h100.Cost, want)
	}
	if h100.Cost.Provider != "runpod" || h100.Cost.PriceSource == "" || h100.Cost.PriceObservedAt == nil || *h100.Cost.HourlyUSD != 2 {
		t.Fatalf("cost provenance missing: %+v", h100.Cost)
	}
	if !strings.Contains(h100.Cost.Basis, "Estimate") || !strings.Contains(h100.Cost.Basis, "concurrency 32") || !strings.Contains(h100.Cost.Basis, "1 × H100") {
		t.Fatalf("basis = %q", h100.Cost.Basis)
	}
	if h100.RankBasis != "cost_per_million_output_tokens" {
		t.Fatalf("rank basis = %s", h100.RankBasis)
	}
	mi300x := result.Stacks[2]
	if mi300x.Cost.State != CostUnknown || !strings.Contains(mi300x.Cost.Basis, "1 × MI300X") || mi300x.RankBasis != "output_tokens_per_second_per_gpu" {
		t.Fatalf("unpriced row = %+v basis %s", mi300x.Cost, mi300x.RankBasis)
	}
}

func TestRankNeverChoosesAPointThatMissesTheTarget(t *testing.T) {
	t.Parallel()
	dataset := testDataset(t)
	result := rank(t, dataset, Query{Model: "org/model-a", Goal: GoalLowestCost, MaxTTFTP95Ms: 1000}, testPrices)
	if want := []string{"a-h100-vllm", "a-h200-sglang", "a-h100-old", "a-mi300x-vllm-unknown-ttft"}; !equal(ids(result), want) {
		t.Fatalf("order = %v, want %v", ids(result), want)
	}
	h200 := result.Stacks[1]
	if h200.OperatingPoint.Concurrency != 4 || h200.Target != TargetMet || *h200.Metrics.TTFTP95Ms != 150 {
		t.Fatalf("h200 must fall back to the point inside the target: %+v", h200.OperatingPoint)
	}
	if mi300x := result.Stacks[3]; mi300x.Target != TargetUnknown {
		t.Fatalf("unpublished p95 must read unknown, got %s", mi300x.Target)
	}

	// The MI300X row published only a 120 ms p50, so its p95 is at least that:
	// a 100 ms target is missed, not unknown.
	tight := rank(t, dataset, Query{Model: "org/model-a", Goal: GoalLowestCost, MaxTTFTP95Ms: 100}, testPrices)
	if want := []string{"a-h100-old"}; !equal(ids(tight), want) {
		t.Fatalf("order = %v, want %v", ids(tight), want)
	}
	if len(tight.Excluded) != 3 || !strings.Contains(tight.Excluded[0].Reason, "Misses the latency target") {
		t.Fatalf("excluded = %+v", tight.Excluded)
	}
	if tight.Target.MaxTTFTP95Ms == nil || *tight.Target.MaxTTFTP95Ms != 100 || !strings.Contains(tight.RankedBy, "at most 100 ms") {
		t.Fatalf("target echo = %+v ranked by %q", tight.Target, tight.RankedBy)
	}

	perUser := rank(t, dataset, Query{Model: "org/model-a", Goal: GoalHighestThroughput, MinPerUserOutputTokensPerSecond: 80}, nil)
	for _, row := range perUser.Stacks {
		if row.ID == "a-h100-vllm" && row.OperatingPoint.Concurrency != 4 {
			t.Fatalf("per-user target must pick concurrency 4, got %d", row.OperatingPoint.Concurrency)
		}
	}
}

func TestRankLowestLatencyPrefersPublishedP95(t *testing.T) {
	t.Parallel()
	result := rank(t, testDataset(t), Query{Model: "org/model-a", Goal: GoalLowestLatency}, nil)
	if want := []string{"a-h200-sglang", "a-h100-vllm", "a-mi300x-vllm-unknown-ttft", "a-h100-old"}; !equal(ids(result), want) {
		t.Fatalf("order = %v, want %v", ids(result), want)
	}
	if result.Stacks[0].OperatingPoint.Concurrency != 4 || result.Stacks[0].RankBasis != "ttft_p95" {
		t.Fatalf("lowest latency picks the fastest first token: %+v", result.Stacks[0])
	}
	if result.Stacks[2].Metrics.TTFTP95Ms != nil || result.Stacks[2].Metrics.TTFTP50Ms == nil {
		t.Fatal("a p50 must never be shown as a p95")
	}
}

func TestRankFiltersByAvailableGPUs(t *testing.T) {
	t.Parallel()
	result := rank(t, testDataset(t), Query{Model: "org/model-a", Goal: GoalHighestThroughput, GPUs: []string{"h100", "NVIDIA H100"}}, nil)
	if want := []string{"a-h100-vllm", "a-h100-old"}; !equal(ids(result), want) {
		t.Fatalf("order = %v, want %v", ids(result), want)
	}
	if len(result.Excluded) != 2 || !strings.Contains(result.Excluded[0].Reason, "not in the GPUs you can use") {
		t.Fatalf("excluded = %+v", result.Excluded)
	}
}

func TestRankComparesOnlyOneWorkloadShape(t *testing.T) {
	t.Parallel()
	dataset := testDataset(t)
	long := rank(t, dataset, Query{Model: "org/model-a", Goal: GoalLowestCost, InputTokens: 8192, OutputTokens: 1024}, nil)
	if want := []string{"a-h100-long"}; !equal(ids(long), want) || long.Workload.Match != MatchExact {
		t.Fatalf("order = %v match %s", ids(long), long.Workload.Match)
	}
	closest := rank(t, dataset, Query{Model: "org/model-a", Goal: GoalLowestCost, InputTokens: 6000, OutputTokens: 1500}, nil)
	if closest.Workload.InputTokens != 8192 || closest.Workload.Match != MatchClosest || closest.Workload.Requested.InputTokens != 6000 {
		t.Fatalf("closest workload = %+v", closest.Workload)
	}
}

func TestRankFallsBackToALabelledFamily(t *testing.T) {
	t.Parallel()
	doc := validDocument(t)
	family := doc["families"].([]any)[0].(map[string]any)
	family["members"] = append(family["members"].([]any), "org/model-c")
	dataset := parseDocument(t, doc)
	result := rank(t, dataset, Query{Model: "org/model-c", Goal: GoalHighestThroughput}, nil)
	if result.Model.Match != MatchFamily || result.Model.Family == nil || result.Model.Family.Basis != "Same architecture." {
		t.Fatalf("model = %+v", result.Model)
	}
	for _, row := range result.Stacks {
		if row.ModelMatch != MatchFamily || row.Model == "org/model-c" {
			t.Fatalf("row %s must name the measured model and the family match", row.ID)
		}
	}
	exact := rank(t, dataset, Query{Model: "org/model-b", Goal: GoalHighestThroughput}, nil)
	if exact.Model.Match != MatchExact || !equal(ids(exact), []string{"b-h100-vllm"}) {
		t.Fatalf("exact evidence must win over the family: %+v %v", exact.Model, ids(exact))
	}
	none := rank(t, dataset, Query{Model: "org/unknown", Goal: GoalLowestCost}, nil)
	if none.Model.Match != MatchNone || len(none.Stacks) != 0 || none.Stacks == nil || none.Workload.Match != MatchNone {
		t.Fatalf("unknown model = %+v", none)
	}
}

func TestRankLabelsEverySource(t *testing.T) {
	t.Parallel()
	result := rank(t, testDataset(t), Query{Model: "org/model-a", Goal: GoalHighestThroughput}, nil)
	want := map[string]string{
		"a-h100-vllm":   "Public benchmark · Bench · 2026-10-01",
		"a-h200-sglang": "Measured by InferCrane · 2026-10-02",
		"a-h100-old":    "Vendor recipe · Vendor · 2024-06-01",
	}
	for _, row := range result.Stacks {
		if label, ok := want[row.ID]; ok && row.Source.Display != label {
			t.Fatalf("%s source = %q, want %q", row.ID, row.Source.Display, label)
		}
		if row.Source.URL == "" || row.Source.License == "" || row.Source.Ref == "" {
			t.Fatalf("%s source is incomplete: %+v", row.ID, row.Source)
		}
		if row.Deploy.State != DeployNotReviewed || row.Deploy.Reason == "" {
			t.Fatalf("%s deploy = %+v", row.ID, row.Deploy)
		}
	}
	if result.DefaultStack != "" {
		t.Fatalf("no stack is deployable, default = %s", result.DefaultStack)
	}
	if !strings.Contains(result.EvidencePolicy, "estimate") {
		t.Fatal("evidence policy must say cost is an estimate")
	}
}

func TestRankLimit(t *testing.T) {
	t.Parallel()
	result := rank(t, testDataset(t), Query{Model: "org/model-a", Goal: GoalHighestThroughput, Limit: 1}, nil)
	if len(result.Stacks) != 1 || result.Total != 4 {
		t.Fatalf("limit: %d rows of %d", len(result.Stacks), result.Total)
	}
}

func TestRankRejectsInvalidQueries(t *testing.T) {
	t.Parallel()
	for name, query := range map[string]Query{
		"loose model":     {Model: "llama 70b"},
		"unknown goal":    {Model: "org/model-a", Goal: "fastest"},
		"limit":           {Model: "org/model-a", Limit: MaxLimit + 1},
		"negative target": {Model: "org/model-a", MaxTTFTP95Ms: -1},
		"too many gpus":   {Model: "org/model-a", GPUs: make([]string, MaxGPUs+1)},
		"blank gpu":       {Model: "org/model-a", GPUs: []string{" "}},
		"huge output":     {Model: "org/model-a", OutputTokens: 300_000},
		"nan target":      {Model: "org/model-a", MinPerUserOutputTokensPerSecond: math.NaN()},
	} {
		if _, err := testDataset(t).Rank(query, nil, testNow); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestEmbeddedDatasetRanksTheMeasuredQwenStack(t *testing.T) {
	t.Parallel()
	result, err := MustDefault().Rank(Query{Model: "qwen/qwen3.8-27b-fp8", Goal: GoalLowestCost}, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if result.Model.Match != MatchExact || result.Workload.Match != MatchClosest || result.Workload.InputTokens != 3919 {
		t.Fatalf("model %+v workload %+v", result.Model, result.Workload)
	}
	top := result.Stacks[0]
	if top.ID != "qwen3.8-27b-fp8-h200x1-sglang-0.5.20-nextn" || top.Deploy.State != DeployReady || result.DefaultStack != top.ID {
		t.Fatalf("top = %s deploy %+v default %s", top.ID, top.Deploy, result.DefaultStack)
	}
	if top.Source.Kind != SourceMeasuredByInferCrane || top.Metrics.TTFTP95Ms == nil {
		t.Fatalf("top source = %+v", top.Source)
	}
	targeted, err := MustDefault().Rank(Query{Model: "Qwen/Qwen3.8-27B-FP8", Goal: GoalLowestCost, InputTokens: 3919, OutputTokens: 1377, MaxTTFTP95Ms: 1000}, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if targeted.Stacks[0].OperatingPoint.Concurrency != 4 || *targeted.Stacks[0].Metrics.TTFTP95Ms > 1000 {
		t.Fatalf("targeted top = %+v", targeted.Stacks[0].OperatingPoint)
	}
}

func parseDocument(t *testing.T, doc map[string]any) Dataset {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	dataset, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return dataset
}

func TestTargetUsesOnlySoundPercentileBounds(t *testing.T) {
	t.Parallel()
	value := func(v float64) *float64 { return &v }
	query := Query{MaxTTFTP95Ms: 1000}
	for name, tc := range map[string]struct {
		ttft Percentiles
		want string
	}{
		"published p95 inside":  {Percentiles{P95: value(900)}, TargetMet},
		"published p95 outside": {Percentiles{P95: value(1100), P99: value(900)}, targetMissed},
		"p99 bounds it inside":  {Percentiles{P50: value(300), P99: value(950)}, TargetMet},
		"p90 already outside":   {Percentiles{P50: value(300), P90: value(1200), P99: value(5000)}, targetMissed},
		"p50 already outside":   {Percentiles{P50: value(1500)}, targetMissed},
		"between bounds":        {Percentiles{P50: value(300), P99: value(5000)}, TargetUnknown},
		"mean says nothing":     {Percentiles{Mean: value(10)}, TargetUnknown},
		"nothing published":     {Percentiles{}, TargetUnknown},
	} {
		if got := meets(Point{TTFTMs: tc.ttft}, query); got != tc.want {
			t.Fatalf("%s: got %s, want %s", name, got, tc.want)
		}
	}
	bounded := Percentiles{P50: value(300), P99: value(950)}
	stack := Stack{GPUCount: 1, Points: []Point{{Concurrency: 8, OutputTokensPerSecond: value(100), TTFTMs: bounded, Ref: "r"}}}
	row, reason := (Dataset{StaleAfterDays: 365}).rankRow(stack, Query{Goal: GoalLowestCost, MaxTTFTP95Ms: 1000}, MatchExact, nil, testNow)
	if reason != "" || row.Metrics.TTFTP95Ms != nil || row.Metrics.TTFTP95AtMostMs == nil || *row.Metrics.TTFTP95AtMostMs != 950 || row.Target != TargetMet {
		t.Fatalf("bounded row = %+v reason %q", row.Metrics, reason)
	}
}
