package optimizer

import (
	"context"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
)

// An 8B-class model in 16 bits on a 24 GB GPU: 16 GB of weights, 147,456 KV
// bytes per token, 400 cache blocks of 64 tokens (9.4 MB each).
const (
	testWeightsBytes = 16e9
	testKVPerToken   = 147456
)

func memorySignals(signals map[string]float64) map[string]float64 {
	signals[SignalModelWeightsBytes] = testWeightsBytes
	signals[SignalKVCacheBytesPerToken] = testKVPerToken
	return signals
}

func precisionProposal(t *testing.T, diagnosis *Diagnosis, mutate ...func(*Request)) Proposal {
	t.Helper()
	request := genericRequest("L4", diagnosis)
	request.Objective, request.MaxCandidates = "throughput", 6
	for _, change := range mutate {
		change(&request)
	}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatal(err)
	}
	return proposal
}

// fullCache is 6 long requests running and 10 waiting on a full cache.
func fullCache(memory bool) *Diagnosis {
	signals := map[string]float64{
		optimizationreadiness.SignalRunningPerReplica: 6, optimizationreadiness.SignalWaitingPerReplica: 10, metrics.MeasurementKVCacheUsage: 95,
		optimizationreadiness.SignalCacheBlocksPerReplica: 400, metrics.MeasurementKVCacheBlockTokens: 64,
		"mean_input_tokens": 3900, optimizationreadiness.SignalMeanOutputTokens: 295,
	}
	if memory {
		memorySignals(signals)
	}
	return testDiagnosis(optimizationreadiness.BottleneckKVCachePressure, signals,
		DiagnosisFinding{Rule: optimizationreadiness.RuleKVSaturation, Signal: metrics.MeasurementKVCacheUsage, Observed: 95, Comparison: ">=", Threshold: 90, Unit: "percent"})
}

func TestFullCacheRanksFP8ByPredictedRoomAndPlansThePair(t *testing.T) {
	proposal := precisionProposal(t, fullCache(true))
	// FP8 weights free 8 GB, about 848 blocks of 9.4 MB: up to 1,248 blocks,
	// room for 19 at 2.4 blocks a request; an FP8 KV cache doubles the 400
	// blocks, room for 12. The pair ranks above both, which follow.
	order := strings.Join(reasonOrder(proposal), ",")
	if want := "control," + TechniqueFP8WeightsKVCache + "," + TechniqueFP8Weights + "," + TechniqueFP8KVCache; !strings.HasPrefix(order, want) {
		t.Fatalf("order=%s, want it to start %s", order, want)
	}
	pair := proposal.Candidates[1]
	if args := strings.Join(pair.Deployment.Runtime.Args, " "); args != "--quantization fp8 --kv-cache-dtype fp8" {
		t.Fatalf("the pair must carry both flags and nothing else: %s", args)
	}
	weights := plannedTechnique(t, proposal, TechniqueFP8Weights)
	cache := plannedTechnique(t, proposal, TechniqueFP8KVCache)
	if weights.Priority != 95 || cache.Priority != 90 || plannedTechnique(t, proposal, TechniqueFP8WeightsKVCache).Priority != 96 {
		t.Fatalf("priorities weights=%d cache=%d", weights.Priority, cache.Priority)
	}
	if !strings.Contains(weights.Reason, "room for at most 19 requests, above the 16 sent at once") || !strings.Contains(cache.Reason, "room for at most 12 requests, fewer than the 16 sent at once") {
		t.Fatalf("reasons must give the predicted rooms: %q / %q", weights.Reason, cache.Reason)
	}
	room := proposal.Diagnosis.CacheRoom
	if room == nil || len(room.Precision) != 3 {
		t.Fatalf("cache_room.precision=%+v", room)
	}
	for _, entry := range room.Precision {
		if entry.Basis != RoomUpperBound {
			t.Fatalf("a precision room is an upper bound: %+v", entry)
		}
	}
	if got := [3]float64{room.Precision[0].Room, room.Precision[1].Room, room.Precision[2].Room}; got != [3]float64{12, 19, 39} {
		t.Fatalf("rooms (FP8 KV cache, FP8 weights, both)=%v", got)
	}
}

func TestWithoutTheModelsMemoryTheFullCacheKeepsItsFixedOrder(t *testing.T) {
	proposal := precisionProposal(t, fullCache(false))
	if order := strings.Join(reasonOrder(proposal), ","); strings.Contains(order, TechniqueFP8WeightsKVCache) || !strings.HasPrefix(order, "control,"+TechniqueFP8KVCache) {
		t.Fatalf("order=%s", order)
	}
	// An FP8 KV cache's room needs only the engine's blocks; FP8 weights' needs
	// the model's memory.
	if room := proposal.Diagnosis.CacheRoom; room == nil || len(room.Precision) != 1 || room.Precision[0].Technique != TechniqueFP8KVCache || room.Precision[0].Room != 12 {
		t.Fatalf("without memory facts only the FP8 KV cache's room: %+v", room)
	}
}

// decodeBound is a decode-bound window with `running` short requests and a
// cache with room.
func decodeBound(running float64) *Diagnosis {
	return testDiagnosis(optimizationreadiness.BottleneckDecode, memorySignals(map[string]float64{
		optimizationreadiness.SignalRunningPerReplica: running, optimizationreadiness.SignalWaitingPerReplica: 0, metrics.MeasurementKVCacheUsage: 50,
		optimizationreadiness.SignalCacheBlocksPerReplica: 400, metrics.MeasurementKVCacheBlockTokens: 64,
		"p95_generation": 19000, "p95_latency": 19200, "mean_input_tokens": 45, optimizationreadiness.SignalMeanOutputTokens: 265,
	}))
}

func TestDecodeChoosesAnFP8KVCacheWhenTheCacheIsAShareOfEachStepsReads(t *testing.T) {
	// 64 requests read about 1.65 GB of cache a step beside 16 GB of weights:
	// 9% now, 17% with FP8 weights, so an FP8 KV cache is chosen and paired.
	proposal := precisionProposal(t, decodeBound(64))
	if order := strings.Join(reasonOrder(proposal), ","); !strings.HasPrefix(order, "control,"+TechniqueFP8WeightsKVCache+","+TechniqueFP8KVCache+","+TechniqueFP8Weights) {
		t.Fatalf("64 running: order=%s", order)
	}
	if reason := plannedTechnique(t, proposal, TechniqueFP8KVCache).Reason; !strings.Contains(reason, "17% with FP8 weights") {
		t.Fatalf("the reason must give the share of each step's reads: %q", reason)
	}
	// 16 requests read about 0.41 GB: 5% with FP8 weights, under 10%.
	small := precisionProposal(t, decodeBound(16))
	if order := strings.Join(reasonOrder(small), ","); order != "control,"+TechniqueFP8Weights {
		t.Fatalf("16 running: order=%s", order)
	}
}

func TestThePairIsNotPlannedWhenTheSetupAlreadyRunsOneHalf(t *testing.T) {
	proposal := precisionProposal(t, decodeBound(64), func(request *Request) {
		request.StartingSetup = &StartingSetup{Deployment: "chat", Revision: 1, Engine: "vllm", Version: "0.34.0", Args: []string{"--kv-cache-dtype", "fp8"}, GPU: "L4", GPUCount: 1}
	})
	if order := strings.Join(reasonOrder(proposal), ","); strings.Contains(order, TechniqueFP8WeightsKVCache) || strings.Contains(order, TechniqueFP8KVCache+",") {
		t.Fatalf("a setup already on an FP8 KV cache gets FP8 weights alone: order=%s", order)
	}
	quantized := precisionProposal(t, decodeBound(64), func(request *Request) { request.ModelIdentity = "acme/novel-open-model-FP8" })
	if order := strings.Join(reasonOrder(quantized), ","); strings.Contains(order, TechniqueFP8Weights) {
		t.Fatalf("a model whose weights are already FP8 gets neither FP8 weights nor the pair: order=%s", order)
	}
}

func TestPrecisionOfReadsTheStartingSetup(t *testing.T) {
	for _, item := range []struct {
		model         string
		args          []string
		weights, kvFP bool
	}{
		{"acme/model", nil, false, false},
		{"acme/model-FP8", nil, true, false},
		{"acme/model", []string{"--quantization", "fp8"}, true, false},
		{"acme/model", []string{"--kv-cache-dtype=fp8_e4m3"}, false, true},
		{"acme/model", []string{"--kv-cache-dtype", "auto"}, false, false},
	} {
		if weights, cache := precisionOf(item.model, item.args); weights != item.weights || cache != item.kvFP {
			t.Fatalf("%s %v: weights=%t cache=%t", item.model, item.args, weights, cache)
		}
	}
}
