package optimizer

import (
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
)

// The product's planner check on the map's cell prefix/32 (23 November): 3,900-token
// prompts, 31 running and none waiting, cache 29% full. The engine's queue-time
// histogram over the hour still held the cell's opening burst (p95 7.75 s against
// 14.4 s of request time), so the queue rule fired and only larger batches were
// planned. Speculation measured 44% ahead of the plain command in that cell.
func prefixCheckDiagnosis(waiting float64, withWaiting bool) *Diagnosis {
	signals := map[string]float64{
		"mean_input_tokens": 3860, "p95_generation": 12315, "p95_latency": 14393, "p95_queue": 0.002, "p95_ttft": 6278,
		metrics.MeasurementKVCacheUsage: 29.2, metrics.MeasurementQueueTimeP95: 7750, optimizationreadiness.SignalRunningPerReplica: 31,
		optimizationreadiness.SignalCacheBlocksPerReplica: mapBlocksPlain, metrics.MeasurementKVCacheBlockTokens: mapBlockTokens, metrics.MeasurementStatePageBytes: mapStatePage,
	}
	if withWaiting {
		signals[optimizationreadiness.SignalWaitingPerReplica] = waiting
	}
	return testDiagnosis(optimizationreadiness.BottleneckQueueing, signals,
		DiagnosisFinding{Rule: optimizationreadiness.RuleEngineQueue, Signal: metrics.MeasurementQueueTimeP95, Observed: 7750, Comparison: ">=", Threshold: 2878.67, Unit: "ms"})
}

func TestAQueueReadingWithNobodyWaitingIsReadAsThePresentLoad(t *testing.T) {
	present, note := presentDiagnosis(*prefixCheckDiagnosis(0, true))
	if present.PrimaryBottleneck != optimizationreadiness.BottleneckDecode || len(present.Findings) != 0 || !strings.Contains(note, "no request was waiting") || !strings.Contains(note, "7.75 s") {
		t.Fatalf("present=%+v note=%q", present, note)
	}
	proposal := mapProposalFor(t, prefixCheckDiagnosis(0, true))
	if order := reasonOrder(proposal); len(order) < 2 || order[1] != TechniqueSpeculativeMTP || contains(order, TechniqueBatchBudget) {
		t.Fatalf("order=%v", order)
	}
	if !strings.HasPrefix(proposal.Diagnosis.Summary, "The engine's queue time (7.75 s at p95) is a total over the hour") || proposal.Diagnosis.PrimaryBottleneck != optimizationreadiness.BottleneckDecode {
		t.Fatalf("plan=%+v", proposal.Diagnosis)
	}
	// With requests waiting the queue is real, and with no waiting reading
	// nothing is known about now: both keep the recorded bottleneck.
	for _, diagnosis := range []*Diagnosis{prefixCheckDiagnosis(4, true), prefixCheckDiagnosis(0, false)} {
		if present, note := presentDiagnosis(*diagnosis); present.PrimaryBottleneck != optimizationreadiness.BottleneckQueueing || note != "" {
			t.Fatalf("present=%+v note=%q", present, note)
		}
	}
}

// Campaign 2 (23 November): long shared prefixes at 32. Eleven evictions in the
// opening burst over 80 completed requests made the hour's rate 0.1375, while the
// cache was 54% full when the planner was asked. The FP8 KV cache it ranked first
// measured 6.7% slower.
func TestEvictionsFromABurstThatIsOverChooseNoCacheFormat(t *testing.T) {
	signals := map[string]float64{
		"mean_input_tokens": 9455, "p95_generation": 82491, "p95_latency": 91627, "p95_queue": 0.001, "p95_ttft": 34020,
		metrics.MeasurementKVCacheUsage: 54.3, optimizationreadiness.SignalPreemptionRate: 0.1375, optimizationreadiness.SignalRunningPerReplica: 28,
		optimizationreadiness.SignalCacheBlocksPerReplica: mapBlocksPlain, metrics.MeasurementKVCacheBlockTokens: mapBlockTokens, metrics.MeasurementStatePageBytes: mapStatePage,
	}
	burst := testDiagnosis(optimizationreadiness.BottleneckKVCachePressure, signals,
		DiagnosisFinding{Rule: optimizationreadiness.RuleKVPreemption, Signal: optimizationreadiness.SignalPreemptionRate, Observed: 0.1375, Comparison: ">=", Threshold: 0.01, Unit: "ratio"})
	proposal := mapProposalFor(t, burst)
	order := reasonOrder(proposal)
	if len(order) < 2 || order[1] != TechniqueSpeculativeMTPOne || order[len(order)-1] != TechniqueKVSequenceCap {
		t.Fatalf("order=%v", order)
	}
	for _, technique := range proposal.Diagnosis.Techniques {
		if contains(technique.Rules, optimizationreadiness.RuleKVPreemption) {
			t.Fatalf("the past evictions still chose %s", technique.Technique)
		}
	}
	capped := plannedTechnique(t, proposal, TechniqueKVSequenceCap)
	if capped.Priority != pastBurstPriority || !contains(capped.Rules, RulePastEvictions) || !strings.Contains(capped.Reason, "second replica") {
		t.Fatalf("cap=%+v", capped)
	}
	if !strings.Contains(proposal.Diagnosis.Summary, "a burst that is over") {
		t.Fatalf("summary=%q", proposal.Diagnosis.Summary)
	}
	// A full cache now is not a past burst.
	signals[metrics.MeasurementKVCacheUsage] = 97
	if present, note := presentDiagnosis(*burst); present.PrimaryBottleneck != optimizationreadiness.BottleneckKVCachePressure || note != "" {
		t.Fatalf("full cache read as a burst: %+v", present)
	}
}

// vLLM refuses to start a recurrent-state model with more requests at once than
// its state cache holds (904 for this model on one H100), and "larger batches"
// asked for 2,048. The limit now comes from the blocks the engine reports.
func TestLargerBatchesStayInsideTheCacheTheEngineReports(t *testing.T) {
	waiting := func(blocks float64) *Diagnosis {
		return testDiagnosis(optimizationreadiness.BottleneckQueueing, map[string]float64{
			optimizationreadiness.SignalRunningPerReplica: 512, optimizationreadiness.SignalWaitingPerReplica: 40, optimizationreadiness.SignalCacheBlocksPerReplica: blocks,
			metrics.MeasurementKVCacheUsage: 60, "p95_latency": 4000,
		}, DiagnosisFinding{Rule: optimizationreadiness.RuleEngineQueue, Signal: metrics.MeasurementQueueTimeP95, Observed: 1200, Comparison: ">=", Threshold: 800, Unit: "ms"})
	}
	// 867 blocks hold at most 864 requests: not above vLLM's default of 1,024 on H100.
	proposal := mapProposalFor(t, waiting(mapBlocksPlain))
	planned := plannedTechnique(t, proposal, TechniqueBatchBudget)
	if planned.Skipped == "" || !strings.Contains(planned.Skipped, "at most 864 requests") {
		t.Fatalf("batch budget on 867 blocks: %+v", planned)
	}
	// An attention-only model's blocks are small; the cap does not bind.
	proposal = mapProposalFor(t, waiting(40000))
	if args := strings.Join(proposal.Candidates[1].Deployment.Runtime.Args, " "); !strings.Contains(args, "--max-num-seqs 2048") {
		t.Fatalf("args=%s", args)
	}
	// Between the default and the request, the cap is the blocks.
	proposal = mapProposalFor(t, waiting(1500))
	if args := strings.Join(proposal.Candidates[1].Deployment.Runtime.Args, " "); !strings.Contains(args, "--max-num-seqs 1496") {
		t.Fatalf("args=%s", args)
	}
}
