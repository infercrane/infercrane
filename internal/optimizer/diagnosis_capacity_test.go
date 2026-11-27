package optimizer

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
)

// mapCell is one cell of the H100 map of 2026-11-23: Qwen3.8-27B-FP8 on vLLM
// 0.34.0, one H100 per server, medians over three servers. The engine's
// gauges were read every five seconds inside each window. The model has
// recurrent-state layers: the engine sizes each cache block to one page of
// the state (832 tokens, a page of 3,407,872 bytes).
type mapCell struct {
	load                     int
	drafts                   int
	running, waiting, usage  float64
	blocks                   float64
	inputTokens, outputTkens float64
	preempted                bool
}

const (
	mapBlockTokens = 832
	mapStatePage   = 3407872
	// Cache blocks each engine reported: the publisher's command, then with
	// 3 and 1 draft tokens from the model's prediction head.
	mapBlocksPlain = 867
	mapBlocksThree = 779
	mapBlocksOne   = 788
)

// Short chat: one short message in (40 tokens on average), at most 295 out
// (269 on average).
func shortCell(load, drafts int, running, waiting, usage float64) mapCell {
	blocks := map[int]float64{0: mapBlocksPlain, 3: mapBlocksThree, 1: mapBlocksOne}[drafts]
	return mapCell{load: load, drafts: drafts, running: running, waiting: waiting, usage: usage, blocks: blocks, inputTokens: 40, outputTkens: 269}
}

// diagnosis is what the endpoint's readiness assessment reports for the cell:
// a full cache with requests waiting is KV-cache pressure, anything else at
// these loads is decode-bound.
func (c mapCell) diagnosis() *Diagnosis {
	signals := map[string]float64{
		optimizationreadiness.SignalRunningPerReplica: c.running, optimizationreadiness.SignalWaitingPerReplica: c.waiting,
		metrics.MeasurementKVCacheUsage: c.usage, optimizationreadiness.SignalCacheBlocksPerReplica: c.blocks,
		metrics.MeasurementKVCacheBlockTokens: mapBlockTokens, metrics.MeasurementStatePageBytes: mapStatePage,
		"mean_input_tokens": c.inputTokens, optimizationreadiness.SignalMeanOutputTokens: c.outputTkens,
		"p95_generation": 5200, "p95_latency": 6100,
	}
	if c.drafts > 0 {
		signals[metrics.MeasurementSpecDraftLength] = float64(c.drafts)
		signals[metrics.MeasurementSpecDecodeAccept] = 0.57
	}
	var findings []DiagnosisFinding
	if c.preempted {
		findings = append(findings, DiagnosisFinding{Rule: optimizationreadiness.RuleKVPreemption, Signal: optimizationreadiness.SignalPreemptionRate, Observed: .05, Comparison: ">=", Threshold: .01, Unit: "ratio"})
	}
	if c.usage >= 90 && c.waiting > 0 {
		findings = append(findings, DiagnosisFinding{Rule: optimizationreadiness.RuleKVSaturation, Signal: metrics.MeasurementKVCacheUsage, Observed: c.usage, Comparison: ">=", Threshold: 90, Unit: "percent"})
	}
	if len(findings) > 0 {
		return testDiagnosis(optimizationreadiness.BottleneckKVCachePressure, signals, findings...)
	}
	return testDiagnosis(optimizationreadiness.BottleneckDecode, signals)
}

func (c mapCell) name() string {
	return fmt.Sprintf("%d drafts at %d (%v running, %v waiting, %v%% cache)", c.drafts, c.load, c.running, c.waiting, c.usage)
}

func mapProposal(t *testing.T, cell mapCell, objective string) Proposal {
	t.Helper()
	return mapProposalWith(t, cell.diagnosis(), objective)
}

// mapProposalFor plans for the map's model on one H100 with a throughput goal.
func mapProposalFor(t *testing.T, diagnosis *Diagnosis) Proposal {
	t.Helper()
	return mapProposalWith(t, diagnosis, "throughput")
}

func mapProposalWith(t *testing.T, diagnosis *Diagnosis, objective string) Proposal {
	t.Helper()
	request := genericRequest("H100", diagnosis)
	request.Objective, request.MaxCandidates = objective, 6
	request.ModelFeatures, request.ModelConfigDigest = []string{FeatureNativeMTP}, recipeTestDigest
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatal(err)
	}
	return proposal
}

// The capacity test reads, from the engine's own gauges, the cache blocks a
// running request holds, and from them how many requests fit at once. On the
// map's short chat it reproduces the measured limits from the readings at
// low load: about 58 requests with 3 draft tokens (measured: never more than
// 58 running) and about 109 with 1 (measured: 111).
func TestCapacityTestReadsTheLimitFromTheEngine(t *testing.T) {
	for _, test := range []struct {
		cell                mapCell
		blocksPerRequest    float64
		capacity, withThree float64
		withOne             float64
	}{
		// Readings at 32 and 48, before anything waits.
		{shortCell(32, 0, 32, 0, 15), 4.06, 213, 65.3, 121.6},
		{shortCell(48, 0, 47, 0, 22), 4.06, 213, 65.4, 121.6},
		{shortCell(32, 3, 31, 0, 53), 13.32, 58.5, 58.5, 109},
		{shortCell(48, 3, 47, 0, 80), 13.26, 58.7, 58.7, 109},
		{shortCell(32, 1, 31, 0, 28), 7.12, 110.7, 59.5, 110.7},
		{shortCell(48, 1, 47, 0, 43), 7.21, 109.3, 58.8, 109.3},
		// With 3 draft tokens the server runs 58 from 64 on; the rest wait.
		{shortCell(96, 3, 58, 37, 99), 13.30, 58.6, 58.6, 109},
	} {
		reading, ok := readCache(*test.cell.diagnosis(), test.cell.drafts)
		if !ok || !reading.recurrentState() {
			t.Fatalf("%s: no reading", test.cell.name())
		}
		three, _ := reading.capacityWith(speculationDrafts)
		one, _ := reading.capacityWith(shortSpeculationDrafts)
		for label, pair := range map[string][2]float64{
			"blocks per running request": {reading.blocksPerRequest(), test.blocksPerRequest},
			"capacity":                   {reading.capacity(), test.capacity},
			"capacity with 3 drafts":     {three, test.withThree},
			"capacity with 1 draft":      {one, test.withOne},
		} {
			if math.Abs(pair[0]-pair[1]) > pair[1]*0.01 {
				t.Errorf("%s: %s = %.2f, want %.2f", test.cell.name(), label, pair[0], pair[1])
			}
		}
	}
	// The prediction from a server without speculation uses that server's
	// blocks (867), not the blocks a speculating server has left once the
	// prediction head is loaded (779), so it is an upper bound: 65 against
	// the 58 measured.
	reading, _ := readCache(*shortCell(48, 0, 47, 0, 22).diagnosis(), 0)
	if three, _ := reading.capacityWith(speculationDrafts); three < 58 || three > 66 {
		t.Fatalf("predicted room with 3 drafts from the plain server = %.1f", three)
	}
}

// Expected outcome on short chat from the publisher's command (the planner's
// current setup runs no speculation): 3 draft tokens first while they fit
// (48, and 64 where the prediction is an upper bound), 1 draft token first
// from 96, never 3 draft tokens at 96 or 128, and no FP8 KV cache ahead of a
// speculation setup that fits.
func TestCapacityTestOnShortChatFromThePublishersCommand(t *testing.T) {
	for _, test := range []struct {
		cell      mapCell
		objective string
		first     string
		priority  int
		reason    string
	}{
		{shortCell(32, 0, 32, 0, 15), "throughput", TechniqueSpeculativeMTP, 80, "room for at most 65 requests per server, above the 32 sent at once"},
		{shortCell(48, 0, 47, 0, 22), "throughput", TechniqueSpeculativeMTP, 80, "room for at most 65 requests per server, above the 47 sent at once"},
		{shortCell(64, 0, 63, 0, 30), "throughput", TechniqueSpeculativeMTP, 80, "room for at most 64 requests per server, above the 63 sent at once"},
		{shortCell(96, 0, 95, 0, 44), "throughput", TechniqueSpeculativeMTPOne, 80, "room for at most 66 requests per server, fewer than the 95 sent at once"},
		{shortCell(128, 0, 127, 0, 59), "throughput", TechniqueSpeculativeMTPOne, 65, "room for at most 122, still fewer than the 127 sent at once"},
	} {
		proposal := mapProposal(t, test.cell, test.objective)
		order := reasonOrder(proposal)
		if len(order) < 2 || order[1] != test.first {
			t.Errorf("%s, %s goal: order=%v, want %s first", test.cell.name(), test.objective, order, test.first)
			continue
		}
		planned := plannedTechnique(t, proposal, test.first)
		if planned.Priority != test.priority || !strings.Contains(planned.Reason, test.reason) {
			t.Errorf("%s: priority %d reason %q", test.cell.name(), planned.Priority, planned.Reason)
		}
		other := map[string]string{TechniqueSpeculativeMTP: TechniqueSpeculativeMTPOne, TechniqueSpeculativeMTPOne: TechniqueSpeculativeMTP}[test.first]
		if contains(order, other) {
			t.Errorf("%s: both draft lengths proposed: %v", test.cell.name(), order)
		}
		args := strings.Join(proposal.Candidates[1].Deployment.Runtime.Args, " ")
		want := map[string]string{TechniqueSpeculativeMTP: `"num_speculative_tokens":3`, TechniqueSpeculativeMTPOne: `"num_speculative_tokens":1`}[test.first]
		if !strings.Contains(args, want) {
			t.Errorf("%s: args %s", test.cell.name(), args)
		}
	}
	// Above the room of both draft lengths, an interactive goal keeps the
	// publisher's command ahead: 1 draft token is still measured, last, because
	// requests would wait for their first token (measured at 128: 1.0 s
	// against 0.1 s).
	interactive := mapProposal(t, shortCell(128, 0, 127, 0, 59), "interactive")
	if planned := plannedTechnique(t, interactive, TechniqueSpeculativeMTPOne); planned.Priority != 45 || planned.ExpectedBenefit != "low" || !strings.Contains(planned.Reason, "some would wait for their first token") {
		t.Errorf("interactive at 128: %+v", planned)
	}
	if order := reasonOrder(interactive); contains(order, TechniqueSpeculativeMTP) || order[len(order)-1] != TechniqueSpeculativeMTPOne {
		t.Errorf("interactive at 128: order=%v", order)
	}
}

// Expected outcome when the current setup already speculates with 3 draft
// tokens: nothing changes while they fit (32, 48); once the cache is full and
// requests wait (64, 96, 128), 1 draft token comes first and the FP8 KV cache
// the cache rule used to rank first is not proposed, because this model's
// full cache is recurrent state. Measured at 128 on three servers, an FP8 KV
// cache on top of 3 draft tokens ran 61 requests against 58: no gain.
func TestCapacityTestWhenThreeDraftTokensRun(t *testing.T) {
	for _, cell := range []mapCell{shortCell(32, 3, 31, 0, 53), shortCell(48, 3, 47, 0, 80)} {
		proposal := mapProposal(t, cell, "throughput")
		if order := reasonOrder(proposal); contains(order, TechniqueSpeculativeMTPOne) || contains(order, TechniqueSpeculativeMTP) {
			t.Errorf("%s: a speculation change proposed while 3 draft tokens fit: %v", cell.name(), order)
		}
	}
	for _, test := range []struct {
		cell     mapCell
		priority int
		reason   string
	}{
		{shortCell(64, 3, 58, 5, 99), 100, "room for about 58 requests, so 5 of the 63 sent at once waited"},
		{shortCell(96, 3, 58, 37, 99), 100, "with 1 draft token a request would hold about 7.1 blocks, room for about 108, so all 95 fit"},
		{shortCell(128, 3, 58, 69, 99), 85, "room for about 108, still fewer than the 127 sent at once"},
	} {
		proposal := mapProposal(t, test.cell, "throughput")
		order := reasonOrder(proposal)
		if strings.Join(order, ",") != "control,"+TechniqueSpeculativeMTPOne+","+TechniqueKVSequenceCap {
			t.Errorf("%s: order=%v", test.cell.name(), order)
			continue
		}
		planned := plannedTechnique(t, proposal, TechniqueSpeculativeMTPOne)
		if planned.Priority != test.priority || !contains(planned.Rules, RuleSpeculationCapacity) || !strings.Contains(planned.Reason, test.reason) {
			t.Errorf("%s: priority %d rules %v reason %q", test.cell.name(), planned.Priority, planned.Rules, planned.Reason)
		}
		if args := strings.Join(proposal.Candidates[1].Deployment.Runtime.Args, " "); args != `--speculative-config {"method":"mtp","num_speculative_tokens":1}` {
			t.Errorf("%s: args %s", test.cell.name(), args)
		}
		for _, technique := range proposal.Diagnosis.Techniques {
			if technique.Technique == TechniqueFP8KVCache {
				t.Errorf("%s: FP8 KV cache planned on a full state cache: %+v", test.cell.name(), technique)
			}
		}
		capReason := plannedTechnique(t, proposal, TechniqueKVSequenceCap).Reason
		if !strings.Contains(capReason, "An FP8 KV cache is not proposed") || !strings.Contains(capReason, "About 92% of the 13.3 blocks") {
			t.Errorf("%s: the cache rule must say why there is no FP8 KV cache: %q", test.cell.name(), capReason)
		}
	}
}

// With 1 draft token running, 3 come back only while they fit: at 32 and 48
// (where 3 measured ahead), not from 64 on (where 1 measured level or ahead).
func TestCapacityTestWhenOneDraftTokenRuns(t *testing.T) {
	for _, test := range []struct {
		cell  mapCell
		three bool
	}{
		{shortCell(32, 1, 31, 0, 28), true},
		{shortCell(48, 1, 47, 0, 43), true},
		{shortCell(64, 1, 63, 0, 57), false},
		{shortCell(96, 1, 95, 0, 85), false},
	} {
		order := reasonOrder(mapProposal(t, test.cell, "throughput"))
		if got := len(order) > 1 && order[1] == TechniqueSpeculativeMTP; got != test.three || contains(order, TechniqueSpeculativeMTPOne) {
			t.Errorf("%s: order=%v, 3 drafts first: %v", test.cell.name(), order, test.three)
		}
	}
}

// Item 3 of the change list on the map's other traffic at 128: the FP8 KV
// cache is left out wherever the full cache is mostly recurrent state, and
// kept where a running request's blocks are mostly its own tokens (the
// publisher's command on 3,900-token prompts, which was not measured with an
// FP8 KV cache on H100).
func TestFP8KVCacheOnlyWhenTheFullCacheIsKV(t *testing.T) {
	longCell := func(drafts int, running, waiting, usage, blocks float64) mapCell {
		return mapCell{load: 128, drafts: drafts, running: running, waiting: waiting, usage: usage, blocks: blocks, inputTokens: 3900, outputTkens: 280, preempted: true}
	}
	outputCell := func(drafts int, running, waiting, usage, blocks float64) mapCell {
		return mapCell{load: 128, drafts: drafts, running: running, waiting: waiting, usage: usage, blocks: blocks, inputTokens: 800, outputTkens: 1377, preempted: drafts > 0}
	}
	for _, test := range []struct {
		name        string
		cell        mapCell
		fp8First    bool
		stateShare  float64
		speculation string
	}{
		// Plain command, long context: 8.3 blocks per request, 6 of them tokens.
		{"long context, publisher's command", longCell(0, 104, 23, 100, mapBlocksPlain), true, 0.28, ""},
		// 3 draft tokens, long context: 17.9 blocks, two thirds state. Measured
		// with an FP8 KV cache on top: +0.9%, inside the spread.
		{"long context, 3 drafts", longCell(3, 43, 83, 99, mapBlocksThree), false, 0.66, TechniqueSpeculativeMTPOne},
		// 3 draft tokens, long output: 14.7 blocks, four fifths state.
		{"long output, 3 drafts", outputCell(3, 53, 74, 100, mapBlocksThree), false, 0.80, TechniqueSpeculativeMTPOne},
	} {
		reading, _ := readCache(*test.cell.diagnosis(), test.cell.drafts)
		if share, ok := reading.stateShare(); !ok || math.Abs(share-test.stateShare) > 0.01 {
			t.Errorf("%s: state share %.3f, want %.2f", test.name, share, test.stateShare)
		}
		order := reasonOrder(mapProposal(t, test.cell, "throughput"))
		if (order[1] == TechniqueFP8KVCache) != test.fp8First || !test.fp8First && contains(order, TechniqueFP8KVCache) {
			t.Errorf("%s: order=%v", test.name, order)
		}
		if test.speculation != "" && order[1] != test.speculation {
			t.Errorf("%s: order=%v, want %s first", test.name, order, test.speculation)
		}
	}
}

// While a speculation setup fits, no FP8 KV cache ranks ahead of it, however
// many rules choose the FP8 KV cache.
func TestFP8KVCacheNeverOutranksSpeculationThatFits(t *testing.T) {
	cell := shortCell(48, 0, 47, 0, 22)
	diagnosis := cell.diagnosis()
	diagnosis.Signals["mean_input_tokens"] = 9000
	diagnosis.Findings = append(diagnosis.Findings,
		DiagnosisFinding{Rule: optimizationreadiness.RuleKVPreemption, Signal: optimizationreadiness.SignalPreemptionRate, Observed: .05, Comparison: ">=", Threshold: .01, Unit: "ratio"},
		DiagnosisFinding{Rule: optimizationreadiness.RuleMemoryBandwidth, Signal: metrics.MeasurementGPUDRAMActive, Observed: 80, Comparison: ">=", Threshold: 60, Unit: "percent"})
	request := genericRequest("H100", diagnosis)
	request.Objective, request.MaxCandidates = "throughput", 6
	request.ModelFeatures, request.ModelConfigDigest = []string{FeatureNativeMTP}, recipeTestDigest
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	speculation, fp8 := plannedTechnique(t, proposal, TechniqueSpeculativeMTP), plannedTechnique(t, proposal, TechniqueFP8KVCache)
	order := reasonOrder(proposal)
	if fp8.Priority != speculation.Priority-1 || indexOf(order, TechniqueFP8KVCache) < indexOf(order, TechniqueSpeculativeMTP) {
		t.Fatalf("FP8 KV cache %d against speculation %d: order=%v", fp8.Priority, speculation.Priority, order)
	}
	// Without room for speculation, the cache rules rank as before.
	full := shortCell(128, 0, 127, 0, 59).diagnosis()
	full.Signals["mean_input_tokens"] = 9000
	full.Signals[optimizationreadiness.SignalCacheBlocksPerReplica] = 0
	full.Findings = append(full.Findings, diagnosis.Findings...)
	request.Diagnosis = full
	if proposal, err = catalogSource(t).Propose(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if planned := plannedTechnique(t, proposal, TechniqueFP8KVCache); planned.Priority < 100 {
		t.Fatalf("FP8 KV cache without a fitting speculation setup: %+v", planned)
	}
}

func indexOf(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return len(values)
}

// The draft length comes from the engine when it reports one, else from the
// starting setup's own flags.
func TestCurrentDraftsReadsTheEngineThenTheFlags(t *testing.T) {
	engine := testDiagnosis(optimizationreadiness.BottleneckDecode, map[string]float64{metrics.MeasurementSpecDraftLength: 3})
	none := testDiagnosis(optimizationreadiness.BottleneckDecode, map[string]float64{})
	for _, test := range []struct {
		diagnosis *Diagnosis
		args      []string
		want      int
	}{
		{engine, nil, 3},
		{engine, []string{"--speculative-config", `{"method":"mtp","num_speculative_tokens":1}`}, 3},
		{none, []string{"--speculative-config", `{"method":"mtp","num_speculative_tokens":1}`}, 1},
		{none, []string{`--speculative-config={"method":"mtp","num_speculative_tokens":2}`}, 2},
		{none, []string{"--speculative-algorithm", "NEXTN", "--speculative-num-steps", "2"}, 2},
		{none, []string{"--max-num-seqs", "512"}, 0},
	} {
		if got := currentDrafts(*test.diagnosis, test.args); got != test.want {
			t.Errorf("args %v: drafts %d, want %d", test.args, got, test.want)
		}
	}
}

// Package 7 of the optimizer plan: the plan for short/96 from three draft
// tokens says how many requests fit with each draft length. Measured on the
// map: 58 with three drafts (the server never ran more), 111 with one.
func TestThePlanSaysHowManyRequestsFitWithEachDraftLength(t *testing.T) {
	plan := mapProposal(t, shortCell(96, 3, 58, 37, 99), "throughput").Diagnosis
	room := plan.CacheRoom
	if room == nil || room.Load != 95 || room.CurrentDrafts != 3 || room.Blocks == nil || *room.Blocks != mapBlocksThree {
		t.Fatalf("room=%+v", room)
	}
	want := map[int]struct {
		room  float64
		basis string
		fits  bool
	}{3: {58, RoomMeasured, false}, 2: {76, RoomPredicted, false}, 1: {108, RoomPredicted, true}, 0: {191, RoomLowerBound, true}}
	for _, entry := range room.DraftLengths {
		expected, ok := want[entry.Drafts]
		if !ok || math.Abs(entry.Room-expected.room) > 2 || entry.Basis != expected.basis || entry.Fits != expected.fits || entry.BlocksPerRequest == nil {
			t.Errorf("%d drafts: %+v, want %+v", entry.Drafts, entry, expected)
		}
		delete(want, entry.Drafts)
	}
	if len(want) != 0 {
		t.Fatalf("missing draft lengths: %v", want)
	}
	if len(room.Bands) != 3 || *room.Bands[0].UpTo != 58 || room.Bands[0].Drafts != 3 || room.Bands[1].Drafts != 1 || room.Bands[2].UpTo != nil || room.Bands[2].Drafts != 0 {
		t.Fatalf("bands=%+v", room.Bands)
	}
	// From the plain command the predictions are upper bounds: a prediction
	// head would take some of the server's blocks.
	plain := mapProposal(t, shortCell(96, 0, 95, 0, 44), "throughput").Diagnosis.CacheRoom
	for _, entry := range plain.DraftLengths {
		if entry.Drafts > 0 && entry.Basis != RoomUpperBound || entry.Drafts == 0 && entry.Basis != RoomMeasured {
			t.Fatalf("plain: %+v", entry)
		}
	}
}

// Package 8 of the optimizer plan, its "done when": a plan for an endpoint
// running three draft tokens proposes one, built from the endpoint's own
// flags. For a goal that counts only requests within the first-token limit,
// the plan proposes no speculation first: at 128 the map measured the plain
// command's useful output at 4,081 tokens a second, one draft token's at
// 2,065, and three draft tokens' at none.
func TestAPlanForAnEndpointRunningThreeDraftTokensProposesOne(t *testing.T) {
	endpoint := []string{"--max-num-seqs", "512", "--speculative-config", `{"method":"mtp","num_speculative_tokens":3}`}
	plan := func(objective string) Proposal {
		request := genericRequest("H100", shortCell(128, 3, 58, 69, 99).diagnosis())
		request.Objective, request.MaxCandidates = objective, 6
		request.ModelFeatures, request.ModelConfigDigest = []string{FeatureNativeMTP}, recipeTestDigest
		request.StartingSetup = &StartingSetup{Deployment: "chat", Revision: 3, Engine: "vllm", Version: "0.34.0", Args: endpoint, GPU: "H100", GPUCount: 1}
		proposal, err := catalogSource(t).Propose(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if err = ValidateProposal(proposal); err != nil {
			t.Fatal(err)
		}
		return proposal
	}
	throughput := plan("throughput")
	if order := reasonOrder(throughput); len(order) < 2 || order[1] != TechniqueSpeculativeMTPOne {
		t.Fatalf("order=%v", order)
	}
	control := throughput.Candidates[0]
	if strings.Join(control.Deployment.Runtime.Args, " ") != strings.Join(endpoint, " ") || control.Deployment.Runtime.Version != "0.34.0" {
		t.Fatalf("control=%+v", control.Deployment.Runtime)
	}
	if args := strings.Join(throughput.Candidates[1].Deployment.Runtime.Args, " "); args != `--max-num-seqs 512 --speculative-config {"method":"mtp","num_speculative_tokens":1}` {
		t.Fatalf("one draft token from the endpoint's flags: %s", args)
	}
	useful := plan("useful-output")
	order := reasonOrder(useful)
	if len(order) < 3 || order[1] != TechniqueSpeculationOff || order[2] != TechniqueSpeculativeMTPOne {
		t.Fatalf("useful output: order=%v", order)
	}
	if args := strings.Join(useful.Candidates[1].Deployment.Runtime.Args, " "); args != "--max-num-seqs 512" {
		t.Fatalf("no speculation: %s", args)
	}
	if !contains(useful.Candidates[1].RequiredEvidence, TaskScoreEvidence) {
		t.Fatalf("turning speculation off changes answers too: %v", useful.Candidates[1].RequiredEvidence)
	}
	// Another engine at the endpoint: the plan starts from its own setup and says why.
	other := genericRequest("H100", shortCell(128, 3, 58, 69, 99).diagnosis())
	other.StartingSetup = &StartingSetup{Deployment: "chat", Revision: 3, Engine: "sglang", Version: "0.5.23", GPU: "H100"}
	proposal, err := catalogSource(t).Propose(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(proposal.Warnings, " "), "The endpoint runs SGLang") {
		t.Fatalf("warnings=%v", proposal.Warnings)
	}
}

// Past the room 3 draft tokens leave, from a server running 3 draft tokens or
// 1, the plan lists speculation that changes with load after the fixed choice
// for this load, with the bands from the engine's own readings (about 58 and
// 111 requests from 3 drafts, the map's measured 58 and 111), and says why it
// is not measured. Below that room nothing calls for it.
func TestAPlanPastThreeDraftsRoomListsSpeculationThatChangesWithLoad(t *testing.T) {
	planned := func(cell mapCell, objective string) *PlannedTechnique {
		proposal := mapProposalWith(t, cell.diagnosis(), objective)
		for index, technique := range proposal.Diagnosis.Techniques {
			if technique.Technique == TechniqueAdaptiveSpeculation {
				return &proposal.Diagnosis.Techniques[index]
			}
		}
		return nil
	}
	for _, cell := range []mapCell{shortCell(96, 3, 58, 37, 99), shortCell(128, 3, 58, 69, 99), shortCell(96, 1, 95, 0, 85)} {
		throughput := planned(cell, "throughput")
		if throughput == nil {
			t.Fatalf("%s: no adaptive speculation in the plan", cell.name())
		}
		if len(throughput.CandidateIDs) != 0 || !strings.Contains(throughput.Skipped, "not published") {
			t.Fatalf("%s: candidates %v, skipped %q; want planned and not measured", cell.name(), throughput.CandidateIDs, throughput.Skipped)
		}
		if !strings.Contains(throughput.Reason, "at once and 1 draft token above it") {
			t.Fatalf("%s: reason %q", cell.name(), throughput.Reason)
		}
		if !contains(throughput.Rules, RuleSpeculationBands) || throughput.Priority >= 80 {
			t.Fatalf("%s: rules %v, priority %d", cell.name(), throughput.Rules, throughput.Priority)
		}
		useful := planned(cell, "useful-output")
		if useful == nil || !strings.Contains(useful.Reason, "and none above") {
			t.Fatalf("%s: useful-output plan %+v", cell.name(), useful)
		}
	}
	// Below the room, or a server that does not speculate (its rooms with
	// draft tokens are upper bounds only).
	for _, cell := range []mapCell{shortCell(32, 3, 31, 0, 53), shortCell(128, 0, 127, 0, 59)} {
		if technique := planned(cell, "throughput"); technique != nil {
			t.Errorf("%s: adaptive speculation planned: %+v", cell.name(), technique)
		}
	}
}
