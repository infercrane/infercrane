package optimizer

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/speculator"
	"github.com/infercrane/infercrane/internal/support"
)

// DiagnosisRulesVersion identifies the rule set that maps a diagnosis to the
// techniques worth measuring. A change to a rule, priority, gate, or flag
// value changes this version.
const DiagnosisRulesVersion = "diagnosis-technique-rules-v12"

// Techniques the diagnosis rule set can choose. A technique is one change to
// the starting setup; its candidate is measured against that setup.
const (
	TechniqueBatchBudget      = "batch-budget"
	TechniqueLargePrefill     = "prefill-chunk-large"
	TechniquePrefixCache      = "prefix-cache"
	TechniquePrefixScheduling = "prefix-aware-scheduling"
	TechniquePrefixRouting    = "prefix-aware-routing"
	TechniqueFP8Weights       = "fp8-weights"
	TechniqueInt4Weights      = "int4-weights"
	// TechniqueFP8Checkpoint is a published FP8 checkpoint of the starting
	// setup's exact model, from the reviewed catalog. No rule chooses it: it
	// is measured beside FP8 weights quantized at load whenever a rule chose
	// those, because a publisher's calibration may keep answers closer.
	TechniqueFP8Checkpoint = "fp8-checkpoint"
	TechniqueFP8KVCache    = "fp8-kv-cache"
	// TechniqueFP8WeightsKVCache is FP8 weights and an FP8 KV cache together,
	// planned when the rules chose both (combinedPrecision).
	TechniqueFP8WeightsKVCache = "fp8-weights-kv-cache"
	TechniqueKVSequenceCap     = "kv-sequence-cap"
	TechniqueSpeculativeMTP    = "speculative-mtp"
	// TechniqueSpeculativeMTPOne speculates with one draft token from the
	// model's own prediction head instead of three.
	TechniqueSpeculativeMTPOne = "speculative-mtp-one-draft"
	// TechniqueSpeculationOff serves the starting setup without speculation:
	// the other of a publisher's documented setups, when speculation leaves
	// requests waiting for cache room.
	TechniqueSpeculationOff = "speculation-off"
	// TechniqueAdaptiveSpeculation serves one model from one engine per draft
	// length on the same GPU and keeps awake the one whose draft length fits
	// the present load (internal/specswitch).
	TechniqueAdaptiveSpeculation = "adaptive-speculation"
	// TechniqueSpeculativeDraft speculates with a published draft model the
	// plan's input names for this exact model (speculative_drafts).
	TechniqueSpeculativeDraft = "speculative-draft-model"
	// TechniqueSpeculativeTrainedDraft speculates with a draft trained on the
	// customer's own prompts in the customer's own account, named in the
	// plan's input (speculative_drafts, source trained).
	TechniqueSpeculativeTrainedDraft = speculator.TechniqueID
	TechniqueAPIServers              = "api-server-processes"
	TechniqueStreamInterval          = "stream-interval"
	TechniqueCUDAGraphs              = "cuda-graphs"
	TechniqueAsyncScheduling         = "async-scheduling"
	TechniqueParallelism             = "parallelism-layout"
	TechniqueSpeculationTune         = "speculation-length"
	TechniqueRightSizing             = "right-sizing"
	// TechniqueAITER turns on AMD's AITER kernels in vLLM on a reviewed AMD
	// Instinct accelerator through a reviewed engine environment variable.
	TechniqueAITER = "aiter-kernels"
)

// Rule identifiers the planner adds to the readiness finding rules it reuses.
const (
	RuleBottleneckQueueing      = "bottleneck.queueing"
	RuleBottleneckPrefill       = "bottleneck.prefill"
	RuleBottleneckDecode        = "bottleneck.decode"
	RuleSharedPrefix            = "prefix.shared"
	RuleProfileGEMMHeavy        = "gpu_profile.gemm_heavy"
	RuleProfileAttentionHeavy   = "gpu_profile.attention_heavy"
	RuleProfileCommunicationBig = "gpu_profile.communication_heavy"
	// RuleAcceleratorKernels fires when the GPU's work is the bottleneck on a
	// reviewed accelerator whose tuned kernels the pinned engine leaves off.
	RuleAcceleratorKernels = "accelerator.kernels_off_by_default"
)

// Planning thresholds. They choose what to measure, never what to ship.
const (
	sharedPrefixRatio       = 0.5
	specAcceptanceFloor     = 0.5
	gemmHeavyShare          = 0.5
	attentionHeavyShare     = 0.4
	communicationHeavyShare = 0.2
	sequenceCapFraction     = 0.75
	sequenceCapStep         = 8
	// chunkedPromptTokens is the prompt length below which a larger per-step
	// token budget cannot change how a prompt is processed.
	chunkedPromptTokens = 1024
	// longContextTokens is the context below which an FP8 KV cache is not
	// chosen unless the KV cache is what limits the server: at shorter
	// contexts BF16 KV has measured slightly better token latency.
	longContextTokens = 7000
	// Model sizes, in billions of parameters, at or below which speculation
	// and lower weight precision are not proposed.
	speculationMinimumBillions = 1.0
	precisionMinimumBillions   = 1.5
	// rooflineCoverageFloor is the share of matrix-multiply time a trace must
	// have placed on the roofline before its memory- or compute-bound verdict
	// chooses anything.
	rooflineCoverageFloor = 0.5
	// captureHeadroom sizes a larger CUDA graph capture a quarter above the
	// observed concurrency, so batches a little above it still use graphs.
	captureHeadroom = 1.25
)

type runtimeArg struct{ flag, value string }

// engineVariable is one reviewed engine environment switch and its value.
type engineVariable struct{ name, value string }

// technique is one reviewed change. args holds the exact flags per built-in
// engine and accelerator memory class; an engine without args can still rank
// an existing candidate that applies the technique, but no candidate is
// derived on that engine.
type technique struct {
	id, label string
	args      map[string][]runtimeArg
	// env holds the reviewed environment switches per built-in engine
	// (support.EngineEnvironment). A technique sets either args or env.
	env map[string][]engineVariable
	// sizedArgs replaces args where the right value depends on the
	// accelerator's memory class, because the engine's default does.
	sizedArgs map[string]map[string][]runtimeArg
	// defaultOn engines already run the technique unless one of disables is
	// set, so a candidate is derived only by removing those flags.
	defaultOn        map[string]bool
	disables         map[string][]string
	changesPrecision bool
	lowersWeights    bool
	speculative      bool
	nativeFP8        bool
	// changesKernels swaps the GPU kernels that compute each layer, so the
	// answers can differ in the last digits and quality is checked.
	changesKernels bool
	modelFeature   string
	// outOfScope explains why the technique is never a derived candidate.
	outOfScope string
	// draft is the speculator.Source of the draft the technique serves; the
	// plan's input must name one (speculative_drafts).
	draft string
	// parts are the techniques a combined change applies together; each must
	// change the starting setup on its own.
	parts []string
}

// TaskScoreEvidence is the evidence a setup that can change answers needs:
// a task score against the starting setup, item by item, on a task set
// attached to the run (quality_eval_set with grading answer_line).
const TaskScoreEvidence = "task score against the starting setup on a task set: answers held within 1 point (the 95% interval of the paired difference starts above -1 point)"

// changesAnswers is the technique's risk class. Lower numeric precision,
// other kernels, and speculation can change what the model writes, so they
// need a task score; every other change schedules the same computation and
// keeps the short answer check. Speculation is in the first class because
// greedy answers changed with it on real servers.
func (t technique) changesAnswers() bool {
	return t.changesPrecision || t.speculative || t.changesKernels || t.id == TechniqueSpeculationOff
}

// techniqueOrder breaks priority ties and lists every technique once.
var techniqueOrder = []string{
	TechniqueFP8WeightsKVCache, TechniqueFP8KVCache, TechniqueBatchBudget, TechniqueAITER, TechniquePrefixCache, TechniquePrefixScheduling, TechniqueSpeculativeMTP,
	TechniqueSpeculativeMTPOne, TechniqueSpeculationOff, TechniqueAdaptiveSpeculationOneEngine, TechniqueAdaptiveSpeculation, TechniqueSpeculativeTrainedDraft, TechniqueSpeculativeDraft, TechniqueFP8Weights, TechniqueFP8Checkpoint, TechniqueInt4Weights, TechniqueLargePrefill, TechniqueKVSequenceCap,
	TechniqueAPIServers, TechniqueStreamInterval, TechniqueCUDAGraphs, TechniqueAsyncScheduling, TechniquePrefixRouting, TechniqueParallelism,
	TechniqueSpeculationTune, TechniqueRightSizing, TechniqueTunedKernels,
}

var techniques = map[string]technique{
	TechniqueBatchBudget: {id: TechniqueBatchBudget, label: "Larger batches",
		sizedArgs: map[string]map[string][]runtimeArg{
			"vllm": {
				memorySmall: {{"--max-num-seqs", "512"}, {"--max-num-batched-tokens", "8192"}},
				memoryLarge: {{"--max-num-seqs", "2048"}, {"--max-num-batched-tokens", "16384"}},
				// vLLM 0.34.0 already runs 16384 tokens on these; double it.
				memoryHuge: {{"--max-num-seqs", "2048"}, {"--max-num-batched-tokens", "32768"}},
			},
		},
		args: map[string][]runtimeArg{"sglang": {{"--max-running-requests", "1024"}, {"--chunked-prefill-size", "8192"}}}},
	TechniqueLargePrefill: {id: TechniqueLargePrefill, label: "Bigger prompt chunks",
		// vLLM 0.34.0 already runs 16384 tokens on 160 GiB GPUs; double it.
		sizedArgs: map[string]map[string][]runtimeArg{
			"vllm": {
				memorySmall: {{"--max-num-batched-tokens", "16384"}},
				memoryLarge: {{"--max-num-batched-tokens", "16384"}},
				memoryHuge:  {{"--max-num-batched-tokens", "32768"}},
			},
		},
		args: map[string][]runtimeArg{"sglang": {{"--chunked-prefill-size", "16384"}}}},
	TechniquePrefixCache: {id: TechniquePrefixCache, label: "Prefix caching",
		defaultOn: map[string]bool{"vllm": true, "sglang": true},
		disables:  map[string][]string{"vllm": {"--no-enable-prefix-caching"}, "sglang": {"--disable-radix-cache"}}},
	TechniquePrefixScheduling: {id: TechniquePrefixScheduling, label: "Schedule requests that share a prompt together",
		args: map[string][]runtimeArg{"sglang": {{"--schedule-policy", "lpm"}}}},
	TechniquePrefixRouting: {id: TechniquePrefixRouting, label: "Route requests that share a prompt to the same server",
		outOfScope: "Routing is set on the endpoint, not on one server, so it is planned as a separate change."},
	TechniqueFP8Weights: {id: TechniqueFP8Weights, label: "FP8 weights", changesPrecision: true, lowersWeights: true, nativeFP8: true,
		args: map[string][]runtimeArg{"vllm": {{"--quantization", "fp8"}}, "sglang": {{"--quantization", "fp8"}}}},
	TechniqueFP8Checkpoint: {id: TechniqueFP8Checkpoint, label: "A published FP8 checkpoint", changesPrecision: true, lowersWeights: true, nativeFP8: true,
		outOfScope: "A published FP8 checkpoint comes only from the reviewed catalog; it is measured beside FP8 weights when the catalog has one for this exact model."},
	TechniqueInt4Weights: {id: TechniqueInt4Weights, label: "4-bit weights", changesPrecision: true, lowersWeights: true,
		outOfScope: "4-bit weights come only from reviewed publisher checkpoints, which serve different model files; a diagnosis measures only published FP8 checkpoints so far, beside FP8 at load."},
	TechniqueFP8KVCache: {id: TechniqueFP8KVCache, label: "FP8 KV cache", changesPrecision: true, nativeFP8: true,
		args: map[string][]runtimeArg{"vllm": {{"--kv-cache-dtype", "fp8"}}, "sglang": {{"--kv-cache-dtype", "fp8_e4m3"}}}},
	TechniqueFP8WeightsKVCache: {id: TechniqueFP8WeightsKVCache, label: "FP8 weights and an FP8 KV cache", changesPrecision: true, lowersWeights: true, nativeFP8: true,
		parts: []string{TechniqueFP8Weights, TechniqueFP8KVCache},
		args:  map[string][]runtimeArg{"vllm": {{"--quantization", "fp8"}, {"--kv-cache-dtype", "fp8"}}, "sglang": {{"--quantization", "fp8"}, {"--kv-cache-dtype", "fp8_e4m3"}}}},
	TechniqueKVSequenceCap: {id: TechniqueKVSequenceCap, label: "Fewer requests at once",
		args: map[string][]runtimeArg{"vllm": {{"--max-num-seqs", ""}}, "sglang": {{"--max-running-requests", ""}}}},
	TechniqueSpeculativeMTP: {id: TechniqueSpeculativeMTP, label: "Speculative decoding with the model's own prediction head", speculative: true, modelFeature: FeatureNativeMTP,
		args: map[string][]runtimeArg{
			// Three drafted tokens. One drafted token writes at most two tokens a
			// step, and the publishers of models with a prediction head document
			// three or more.
			"vllm":   {{"--speculative-config", `{"method":"mtp","num_speculative_tokens":3}`}},
			"sglang": {{"--speculative-algorithm", "NEXTN"}, {"--speculative-num-steps", "2"}, {"--speculative-eagle-topk", "1"}, {"--speculative-num-draft-tokens", "3"}},
		}},
	// One draft token holds fewer cache blocks than three on a model with
	// recurrent-state layers, so more requests fit at once; the capacity test
	// chooses between them (diagnosis_capacity.go).
	TechniqueSpeculativeMTPOne: {id: TechniqueSpeculativeMTPOne, label: "Speculative decoding with one draft token from the model's own prediction head", speculative: true, modelFeature: FeatureNativeMTP,
		args: map[string][]runtimeArg{"vllm": {{"--speculative-config", `{"method":"mtp","num_speculative_tokens":1}`}}}},
	// Derived by removing the starting setup's speculation flags (deriveArgs).
	TechniqueSpeculationOff: {id: TechniqueSpeculationOff, label: "No speculation"},
	// The engines can lower the draft length with the batch, but keep the
	// cache reserved for the longest; the switch changes the reservation too.
	TechniqueAdaptiveSpeculation: {id: TechniqueAdaptiveSpeculation, label: "Speculation that changes with load", speculative: true, modelFeature: FeatureNativeMTP,
		outOfScope: "Not measured yet: this setup runs one engine per draft length on each server behind InferCrane's speculation switch (infercrane-specswitch), which swaps them as the load moves between bands, and the switch's serving image is not published yet."},
	// The same, in one engine whose requests each reserve their own draft
	// length's state (InferCrane's patch to vLLM); its setups come from the
	// contract (technique_adaptive_one_engine.go), on the patched image only.
	TechniqueAdaptiveSpeculationOneEngine: {id: TechniqueAdaptiveSpeculationOneEngine, label: "Speculation that changes with load, in one engine", speculative: true, modelFeature: FeatureNativeMTP},
	TechniqueSpeculativeDraft:             {id: TechniqueSpeculativeDraft, label: "Speculative decoding with a draft model", speculative: true, draft: speculator.SourcePublished},
	TechniqueSpeculativeTrainedDraft:      {id: TechniqueSpeculativeTrainedDraft, label: "Speculative decoding with a draft trained on your traffic", speculative: true, draft: speculator.SourceTrained},
	TechniqueAPIServers: {id: TechniqueAPIServers, label: "More API server processes",
		args: map[string][]runtimeArg{"vllm": {{"--api-server-count", "2"}}}},
	TechniqueStreamInterval: {id: TechniqueStreamInterval, label: "Stream tokens in small groups",
		args: map[string][]runtimeArg{"vllm": {{"--stream-interval", "2"}}}},
	// CUDA graphs are on by default; a candidate either turns them back on or,
	// on vLLM, captures graphs for the larger batches the server runs.
	TechniqueCUDAGraphs: {id: TechniqueCUDAGraphs, label: "CUDA graphs sized to your batches",
		// SGLang 0.5.23 keeps --disable-cuda-graph as a deprecated spelling and
		// adds --disable-decode-cuda-graph (fields/exec_.py:666).
		disables: map[string][]string{"vllm": {"--enforce-eager"}, "sglang": {"--disable-cuda-graph", "--disable-decode-cuda-graph"}}},
	TechniqueAsyncScheduling: {id: TechniqueAsyncScheduling, label: "Prepare the next step while the GPU runs this one",
		defaultOn: map[string]bool{"vllm": true, "sglang": true},
		disables:  map[string][]string{"vllm": {"--no-async-scheduling"}, "sglang": {"--disable-overlap-schedule"}}},
	TechniqueParallelism: {id: TechniqueParallelism, label: "Fewer GPUs per server",
		outOfScope: "Changing how many GPUs each server uses changes the hardware, so it is planned as a separate run."},
	TechniqueSpeculationTune: {id: TechniqueSpeculationTune, label: "Shorter speculation",
		outOfScope: "The draft length is chosen by the cache room the engine reports (one or three draft tokens, or none); a length tuned by acceptance alone is not planned yet."},
	// No rule picks it: a request carrying a tuned set adds it (kernel_configs.go).
	TechniqueTunedKernels: {id: TechniqueTunedKernels, label: "Kernels tuned for your shapes", changesKernels: true},
	TechniqueAITER: {id: TechniqueAITER, label: "AMD AITER kernels", changesKernels: true,
		env: map[string][]engineVariable{"vllm": {{"VLLM_ROCM_USE_AITER", "1"}}}},
	TechniqueRightSizing: {id: TechniqueRightSizing, label: "A smaller or cheaper GPU",
		outOfScope: "Few requests run at once, so this GPU may be larger than the traffic needs. Comparing GPUs needs capacity measured at your SLO, which is planned as a separate run."},
}

// ReasonEvidence is one measured value that made a rule fire.
type ReasonEvidence struct {
	Signal     string   `json:"signal"`
	Observed   float64  `json:"observed"`
	Comparison string   `json:"comparison,omitempty"`
	Threshold  *float64 `json:"threshold,omitempty"`
	Unit       string   `json:"unit"`
}

type techniquePick struct {
	technique string
	priority  int
	why       string
}

type ruleOutcome struct {
	rule     string
	evidence []ReasonEvidence
	picks    []techniquePick
	// below names, per technique this outcome picks, techniques that must not
	// rank at or above it, whatever other rules chose them for.
	below map[string][]string
	// skips records, per technique this outcome picks, why it is chosen and
	// not measured. The plan lists it with that reason.
	skips map[string]string
}

// plannedChoice is a technique the diagnosis chose, with everything that
// chose it. Priority orders measurement; it is a planning prior, not a gain.
type plannedChoice struct {
	technique technique
	priority  int
	rules     []string
	reason    string
	evidence  []ReasonEvidence
	// skip says why the choice is recorded and not measured.
	skip string
	// priorOrder is the choice's place when the results store's prior
	// ordered the choices (diagnosis_prior.go); 0 without a prior.
	priorOrder int
}

// planTechniques applies every rule to the diagnosis and returns the chosen
// techniques by expected benefit. It returns no choice for a window that is
// not diagnosable: an unknown bottleneck is collected, never guessed.
func planTechniques(diagnosis Diagnosis, facts planContext) []plannedChoice {
	if diagnosis.State != optimizationreadiness.StateProfileReady {
		return nil
	}
	type contribution struct {
		pick     techniquePick
		rule     string
		order    int
		evidence []ReasonEvidence
		skip     string
	}
	byTechnique := map[string][]contribution{}
	below := map[string][]string{}
	order := 0
	for _, outcome := range diagnosisOutcomes(diagnosis, facts) {
		for leader, followers := range outcome.below {
			below[leader] = append(below[leader], followers...)
		}
		for _, pick := range outcome.picks {
			byTechnique[pick.technique] = append(byTechnique[pick.technique], contribution{pick: pick, rule: outcome.rule, order: order, evidence: outcome.evidence, skip: outcome.skips[pick.technique]})
			order++
		}
	}
	var choices []plannedChoice
	for id, contributions := range byTechnique {
		sort.SliceStable(contributions, func(i, j int) bool {
			if contributions[i].pick.priority != contributions[j].pick.priority {
				return contributions[i].pick.priority > contributions[j].pick.priority
			}
			return contributions[i].order < contributions[j].order
		})
		choice := plannedChoice{technique: techniques[id], reason: contributions[0].pick.why, skip: contributions[0].skip}
		// Each corroborating rule adds a little, so agreeing evidence wins ties
		// without outranking a stronger direct cause.
		choice.priority = min(100, contributions[0].pick.priority+3*(len(contributions)-1))
		seen := map[string]bool{}
		for _, item := range contributions {
			if !contains(choice.rules, item.rule) {
				choice.rules = append(choice.rules, item.rule)
			}
			for _, evidence := range item.evidence {
				// A reason carries at most maxReasonItems readings; the strongest
				// rule's come first.
				if !seen[evidence.Signal] && len(choice.evidence) < maxReasonItems {
					seen[evidence.Signal] = true
					choice.evidence = append(choice.evidence, evidence)
				}
			}
		}
		choices = append(choices, choice)
	}
	leaders := make([]string, 0, len(below))
	for leader := range below {
		leaders = append(leaders, leader)
	}
	sort.Strings(leaders)
	for _, leader := range leaders {
		followers := below[leader]
		for _, choice := range choices {
			if choice.technique.id != leader {
				continue
			}
			for index := range choices {
				if contains(followers, choices[index].technique.id) && choices[index].priority >= choice.priority {
					choices[index].priority = choice.priority - 1
				}
			}
		}
	}
	// The pair is planned only with the model's memory facts, which show how
	// much of each step's reads or of the cache each part changes.
	reading, _ := readCache(diagnosis, facts.drafts)
	if combined, ok := combinedPrecision(choices); ok && reading.memoryKnown() {
		choices = append(choices, combined)
	}
	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].priority != choices[j].priority {
			return choices[i].priority > choices[j].priority
		}
		return techniqueRank(choices[i].technique.id) < techniqueRank(choices[j].technique.id)
	})
	return choices
}

// combinedPrecision plans FP8 weights and an FP8 KV cache together when the
// rules chose both and the model's memory facts are known. They change different bytes, the weights every step reads
// once and the cache each running request holds and reads, so neither undoes
// the other: together they halve both, and one task set checks the precision
// of both. The pair ranks just above the stronger of the two, which stay in
// the plan, so a campaign measures each alone too and a gain can be told
// apart.
func combinedPrecision(choices []plannedChoice) (plannedChoice, bool) {
	var weights, cache *plannedChoice
	for index := range choices {
		switch choices[index].technique.id {
		case TechniqueFP8Weights:
			weights = &choices[index]
		case TechniqueFP8KVCache:
			cache = &choices[index]
		}
	}
	if weights == nil || cache == nil {
		return plannedChoice{}, false
	}
	combined := plannedChoice{technique: techniques[TechniqueFP8WeightsKVCache], priority: min(100, max(weights.priority, cache.priority)+1),
		reason: "FP8 weights and an FP8 KV cache were both chosen, and they change different bytes, so they are measured together first and each alone after. FP8 weights: " +
			weights.reason + " FP8 KV cache: " + cache.reason,
		skip: weights.skip}
	if combined.skip == "" {
		combined.skip = cache.skip
	}
	seen := map[string]bool{}
	for _, part := range []*plannedChoice{weights, cache} {
		for _, rule := range part.rules {
			if !contains(combined.rules, rule) {
				combined.rules = append(combined.rules, rule)
			}
		}
		for _, evidence := range part.evidence {
			if !seen[evidence.Signal] && len(combined.evidence) < maxReasonItems {
				seen[evidence.Signal] = true
				combined.evidence = append(combined.evidence, evidence)
			}
		}
	}
	return combined, true
}

func techniqueRank(id string) int {
	for index, candidate := range techniqueOrder {
		if candidate == id {
			return index
		}
	}
	return len(techniqueOrder)
}

func expectedBenefit(priority int) string {
	switch {
	case priority >= 85:
		return "high"
	case priority >= 65:
		return "medium"
	default:
		return "low"
	}
}

// diagnosisOutcomes is the rule table, strongest causes first. Every rule
// reads only values present in the diagnosis; a missing value never fires a
// rule. Each rule chooses at most two changes a setup can carry, so a win
// can be attributed to one change.
func diagnosisOutcomes(d Diagnosis, facts planContext) []ruleOutcome {
	objective := facts.objective
	var out []ruleOutcome
	add := func(outcome ruleOutcome) {
		if len(outcome.picks) > 0 {
			out = append(out, outcome)
		}
	}
	longContext := false
	if mean, ok := d.signal("mean_input_tokens"); ok && mean >= longContextTokens {
		longContext = true
	}
	throughputGoal := objective == "throughput" || objective == "cost-efficiency"
	// A useful-output goal counts only requests whose first token came within
	// the limit, so a change that leaves requests waiting for room loses.
	usefulGoal := objective == lab.ObjectiveUsefulOutput
	cache, cacheKnown := readCache(d, facts.drafts)
	cache.fp8Weights, cache.fp8Cache = facts.fp8Weights, facts.fp8Cache
	// On a model whose full cache is mostly recurrent state, an FP8 KV cache
	// adds no blocks, so the cache rules propose fewer requests at once and
	// say why the FP8 KV cache is left out.
	stateNote, stateEvidence, stateCache := cache.stateCacheNote()
	cacheRule := func(finding DiagnosisFinding, cause string, fp8Priority, capPriority int, capWhy string) ruleOutcome {
		if stateCache {
			return ruleOutcome{rule: finding.Rule, evidence: append(findingEvidence(finding), stateEvidence...), picks: []techniquePick{
				{TechniqueKVSequenceCap, capPriority, cause + capWhy + " " + stateNote},
			}}
		}
		evidence := findingEvidence(finding)
		picks := []techniquePick{{TechniqueFP8KVCache, fp8Priority, cause + ", so an FP8 KV cache, which holds about twice the tokens in the same memory, comes first."}}
		if ordered, ok := cache.precisionPicks(cause, fp8Priority); ok {
			picks, evidence = ordered, append(evidence, cache.memoryEvidence()...)
		}
		return ruleOutcome{rule: finding.Rule, evidence: evidence, picks: append(picks, techniquePick{TechniqueKVSequenceCap, capPriority, cause + capWhy})}
	}

	if finding, ok := d.finding(optimizationreadiness.RuleKVPreemption); ok {
		cause := fmt.Sprintf("The engine evicted running requests (%s per completed request, limit %s) because the KV cache was full", ratioWords(finding.Observed), ratioWords(finding.Threshold))
		add(cacheRule(finding, cause, 100, 80, ", so running fewer requests at once should stop the evictions."))
	}
	if finding, ok := d.finding(optimizationreadiness.RuleKVSaturation); ok {
		cause := fmt.Sprintf("The KV cache was %s full (limit %s) while requests waited", percentWords(finding.Observed), percentWords(finding.Threshold))
		add(cacheRule(finding, cause, 95, 60, ", so running fewer requests at once trades some queueing for fewer evictions."))
	}
	if burst := pastBurstOutcome(d); burst != nil {
		add(*burst)
	}
	speculationFull := speculationCapacityOutcome(cache, cacheKnown, usefulGoal)
	if speculationFull != nil {
		add(*speculationFull)
	}
	if bands := speculationBandsOutcome(cache, cacheKnown, usefulGoal); bands != nil {
		add(*bands)
	}
	if finding, ok := d.finding(optimizationreadiness.RuleHostBound); ok {
		cause := fmt.Sprintf("Requests waited while the GPU had work on only %s of its cycles (limit %s), so the server's CPU side is not keeping it fed", percentWords(finding.Observed), percentWords(finding.Threshold))
		add(ruleOutcome{rule: finding.Rule, evidence: findingEvidence(finding), picks: []techniquePick{
			{TechniqueAPIServers, 85, cause + "; more API server processes take request handling off the engine's path."},
			{TechniqueStreamInterval, 75, cause + "; streaming tokens in small groups cuts per-token work on that side."},
			{TechniqueCUDAGraphs, 70, cause + "; CUDA graphs sized to your batches cut kernel-launch time."},
		}})
	}
	if queue := queueOutcome(d); queue != nil {
		add(*queue)
	}
	if finding, ok := d.finding(optimizationreadiness.RuleLongInputPrefill); ok {
		add(ruleOutcome{rule: finding.Rule, evidence: findingEvidence(finding), picks: []techniquePick{
			{TechniqueLargePrefill, 85, fmt.Sprintf("Prompts average %s tokens (threshold %s), so bigger prompt chunks finish each prompt in fewer engine steps.", numberWords(finding.Observed), numberWords(finding.Threshold))},
		}})
	}
	if finding, ok := d.finding(optimizationreadiness.RulePrefixReuse); ok {
		cause := fmt.Sprintf("Most requests share a system prompt, but only %s of prompt tokens came from the cache (floor %s)", percentWords(finding.Observed*100), percentWords(finding.Threshold*100))
		add(ruleOutcome{rule: finding.Rule, evidence: findingEvidence(finding), picks: []techniquePick{
			{TechniquePrefixCache, 90, cause + ", so prefix caching comes first."},
			{TechniquePrefixScheduling, 80, cause + ", so running requests that share a prompt together keeps it cached."},
			{TechniquePrefixRouting, 70, cause + ", so sending requests that share a prompt to the same server keeps it cached."},
		}})
	} else if shared, ok := d.signal(optimizationreadiness.SignalSharedPrefixRatio); ok && shared >= sharedPrefixRatio && d.PrimaryBottleneck == optimizationreadiness.BottleneckPrefill {
		threshold := sharedPrefixRatio
		add(ruleOutcome{rule: RuleSharedPrefix, evidence: []ReasonEvidence{{Signal: optimizationreadiness.SignalSharedPrefixRatio, Observed: shared, Comparison: ">=", Threshold: &threshold, Unit: "ratio"}}, picks: []techniquePick{
			{TechniquePrefixCache, 80, fmt.Sprintf("First tokens are the bottleneck and %s of requests repeat another request's system prompt, so reusing cached prompt tokens skips that work.", percentWords(shared*100))},
		}})
	}
	if finding, ok := d.finding(optimizationreadiness.RuleComputeBound); ok {
		add(ruleOutcome{rule: finding.Rule, evidence: findingEvidence(finding), picks: []techniquePick{
			{TechniqueFP8Weights, 80, fmt.Sprintf("Tensor cores were busy %s of cycles (threshold %s), so FP8 math, which does more work per cycle, is the lever.", percentWords(finding.Observed), percentWords(finding.Threshold))},
		}})
	}
	if finding, ok := d.finding(optimizationreadiness.RuleLowConcurrency); ok {
		cause := fmt.Sprintf("Only %s ran at once per server (threshold %s), so writing each token waits on reading the model from memory", requestWords(finding.Observed), numberWords(finding.Threshold))
		speculate := cause + "; predicting several tokens per step cuts the number of those reads."
		shrink := cause + "; smaller weights mean fewer bytes read per token."
		picks := []techniquePick{{TechniqueSpeculativeMTP, 90, speculate}, {TechniqueSpeculativeDraft, 88, speculate}, {TechniqueFP8Weights, 85, shrink}, {TechniqueInt4Weights, 82, shrink}}
		if acceptance, ok := d.signal(metrics.MeasurementSpecDecodeAccept); ok && acceptance >= specAcceptanceFloor {
			// Speculation already pays off; weight precision is the next lever.
			picks = []techniquePick{{TechniqueFP8Weights, 90, shrink}, {TechniqueInt4Weights, 88, shrink}, {TechniqueSpeculativeMTP, 70, speculate}, {TechniqueSpeculativeDraft, 68, speculate}}
		}
		evidence := findingEvidence(finding)
		if why, ok := cache.cacheReadWhy(cause); cacheKnown && ok {
			picks, evidence = append(picks, techniquePick{TechniqueFP8KVCache, 70, why}), append(evidence, cache.memoryEvidence()...)
		}
		picks = append(picks, techniquePick{TechniqueRightSizing, 50, fmt.Sprintf("Only %s ran at once per server, so the GPU may be larger than this traffic needs.", requestWords(finding.Observed))})
		add(ruleOutcome{rule: finding.Rule, evidence: evidence, picks: picks})
	}
	if finding, ok := d.finding(optimizationreadiness.RuleMemoryBandwidth); ok {
		cause := fmt.Sprintf("GPU memory was busy %s of cycles (threshold %s) while tensor cores mostly idled", percentWords(finding.Observed), percentWords(finding.Threshold))
		picks := []techniquePick{
			{TechniqueFP8Weights, 85, cause + ", so smaller weights move fewer bytes per token."},
			{TechniqueInt4Weights, 82, cause + ", so smaller weights move fewer bytes per token."},
		}
		if longContext {
			picks = append(picks, techniquePick{TechniqueFP8KVCache, 70, cause + " on long contexts, so an FP8 KV cache halves the bytes attention reads."})
		}
		add(ruleOutcome{rule: finding.Rule, evidence: findingEvidence(finding), picks: picks})
	}
	if decode := decodeOutcome(d, longContext, throughputGoal, usefulGoal, cache, cacheKnown, speculationFull != nil); decode != nil {
		add(*decode)
	}
	if prefill := prefillOutcome(d); prefill != nil {
		add(*prefill)
	}
	if kernels := acceleratorKernelOutcome(d, facts); kernels != nil {
		add(*kernels)
	}
	if share, ok := d.kernelShare(KernelClassGEMM); ok && share >= gemmHeavyShare {
		why := fmt.Sprintf("Matrix-multiply kernels took %s of sampled GPU time in the profile, so smaller weights speed up the largest share of work.", percentWords(share*100))
		add(ruleOutcome{rule: RuleProfileGEMMHeavy, evidence: profileEvidence(KernelClassGEMM, share, gemmHeavyShare), picks: []techniquePick{{TechniqueFP8Weights, 70, why}, {TechniqueInt4Weights, 68, why}}})
	}
	// A trace analysis of the same capture measures attention and exposed
	// communication directly, so its findings replace these share rules.
	_, overlapMeasured := d.traceMeasurement(TraceOverlappedShare)
	if share, ok := d.kernelShare(KernelClassAttention); ok && share >= attentionHeavyShare && longContext && d.trace() == nil {
		add(ruleOutcome{rule: RuleProfileAttentionHeavy, evidence: profileEvidence(KernelClassAttention, share, attentionHeavyShare), picks: []techniquePick{
			{TechniqueFP8KVCache, 70, fmt.Sprintf("Attention kernels took %s of sampled GPU time in the profile on long contexts, so an FP8 KV cache halves the bytes they read.", percentWords(share*100))},
		}})
	}
	if share, ok := d.kernelShare(KernelClassCommunication); ok && share >= communicationHeavyShare && !overlapMeasured {
		add(ruleOutcome{rule: RuleProfileCommunicationBig, evidence: profileEvidence(KernelClassCommunication, share, communicationHeavyShare), picks: []techniquePick{
			{TechniqueParallelism, 60, fmt.Sprintf("GPU-to-GPU communication took %s of sampled GPU time in the profile.", percentWords(share*100))},
		}})
	}
	for _, outcome := range traceOutcomes(d, longContext) {
		add(outcome)
	}
	if finding, ok := d.finding(optimizationreadiness.RuleInterconnectBound); ok {
		add(ruleOutcome{rule: finding.Rule, evidence: findingEvidence(finding), picks: []techniquePick{
			{TechniqueParallelism, 80, fmt.Sprintf("Links between the server's GPUs ran at %s of their peak (threshold %s).", percentWords(finding.Observed), percentWords(finding.Threshold))},
		}})
	}
	if finding, ok := d.finding(optimizationreadiness.RuleSpeculativeAccept); ok {
		add(ruleOutcome{rule: finding.Rule, evidence: findingEvidence(finding), picks: []techniquePick{
			{TechniqueSpeculationTune, 50, fmt.Sprintf("Only %s of drafted tokens were accepted (floor %s), so the current speculation spends more than it saves.", percentWords(finding.Observed*100), percentWords(finding.Threshold*100))},
		}})
	}
	if drafts := suppliedDraftOutcome(out, facts); drafts != nil {
		add(*drafts)
	}
	return out
}

// traceOutcomes reads the findings of the capture's trace analysis. Each
// rule cites the analysis' own measured value and threshold. Findings that
// describe where time went without naming a change (idle time of unknown
// cause, long idle gaps, decode-dominant kernel time) choose nothing.
func traceOutcomes(d Diagnosis, longContext bool) []ruleOutcome {
	var out []ruleOutcome
	if finding, ok := d.traceFinding(TraceRuleIdleHostBound); ok {
		cause := fmt.Sprintf("The GPU trace shows the GPU idle %s of the time waiting for the server's CPU to launch work (limit %s)", percentWords(finding.Observed*100), percentWords(finding.Threshold*100))
		out = append(out, ruleOutcome{rule: finding.Rule, evidence: traceEvidence(d, finding, TraceIdleHostWaitShare, TraceLaunchDelayP50US, TraceGraphLaunchShare), picks: []techniquePick{
			{TechniqueCUDAGraphs, 90, cause + ", so CUDA graphs that cover your batches launch each step's kernels at once."},
			{TechniqueAsyncScheduling, 88, cause + ", so preparing the next step while the GPU runs this one hides that wait."},
		}})
	}
	if finding, ok := d.traceFinding(TraceRuleLaunchBoundKernels); ok {
		out = append(out, ruleOutcome{rule: finding.Rule, evidence: traceEvidence(d, finding, TraceLaunchDelayP50US, TraceGraphLaunchShare), picks: []techniquePick{
			{TechniqueCUDAGraphs, 88, fmt.Sprintf("In the GPU trace, %s of kernels finished faster than the CPU took to launch them (limit %s), so CUDA graphs that cover your batches replace those launches with one.", percentWords(finding.Observed*100), percentWords(finding.Threshold*100))},
		}})
	}
	if finding, ok := d.traceFinding(TraceRuleCommunicationExposed); ok {
		out = append(out, ruleOutcome{rule: finding.Rule, evidence: traceEvidence(d, finding, TraceOverlappedShare), picks: []techniquePick{
			{TechniqueParallelism, 75, fmt.Sprintf("In the GPU trace, communication between GPUs held the GPU %s of the time with no compute running under it (limit %s), so spreading the model over more GPUs would add to it; fewer GPUs per server is the lever.", percentWords(finding.Observed*100), percentWords(finding.Threshold*100))},
		}})
	}
	// A roofline verdict covers only matrix multiplies whose shapes the trace
	// recorded, so it chooses nothing until it covers most of their time.
	if coverage, ok := d.traceMeasurement(TraceRooflineCoveredShare); ok && coverage >= rooflineCoverageFloor {
		if finding, ok := d.traceFinding(TraceRuleMemoryBoundGEMM); ok {
			why := fmt.Sprintf("In the GPU trace, %s of matrix-multiply time waited on memory rather than math (limit %s), so smaller weights move fewer bytes per step.", percentWords(finding.Observed*100), percentWords(finding.Threshold*100))
			out = append(out, ruleOutcome{rule: finding.Rule, evidence: append(traceEvidence(d, finding), coverageEvidence(coverage)), picks: []techniquePick{{TechniqueFP8Weights, 80, why}, {TechniqueInt4Weights, 78, why}}})
		}
		if finding, ok := d.traceFinding(TraceRuleComputeBoundGEMM); ok {
			out = append(out, ruleOutcome{rule: finding.Rule, evidence: append(traceEvidence(d, finding), coverageEvidence(coverage)), picks: []techniquePick{
				{TechniqueFP8Weights, 75, fmt.Sprintf("In the GPU trace, %s of matrix-multiply time was limited by math rather than memory (limit %s), so FP8 math, which does more work per cycle, is the lever.", percentWords(finding.Observed*100), percentWords(finding.Threshold*100))},
			}})
		}
	}
	if finding, ok := d.traceFinding(TraceRuleAttentionHeavy); ok && longContext {
		out = append(out, ruleOutcome{rule: finding.Rule, evidence: traceEvidence(d, finding), picks: []techniquePick{
			{TechniqueFP8KVCache, 70, fmt.Sprintf("In the GPU trace, attention took %s of kernel time (limit %s) on long contexts, so an FP8 KV cache halves the bytes it reads.", percentWords(finding.Observed*100), percentWords(finding.Threshold*100))},
		}})
	}
	return out
}

// traceSignals names the measured quantity behind each trace finding the
// planner reads.
var traceSignals = map[string]string{
	TraceRuleIdleHostBound:        "gpu_trace_host_wait_share",
	TraceRuleLaunchBoundKernels:   "gpu_trace_short_kernel_share",
	TraceRuleCommunicationExposed: "gpu_trace_exposed_communication_share",
	TraceRuleMemoryBoundGEMM:      "gpu_trace_memory_bound_gemm_share",
	TraceRuleComputeBoundGEMM:     "gpu_trace_compute_bound_gemm_share",
	TraceRuleAttentionHeavy:       "gpu_trace_attention_share",
}

// traceEvidence cites a trace finding and the supporting measurements the
// trace recorded; a measurement it did not record is left out.
func traceEvidence(d Diagnosis, finding GPUTraceFinding, supporting ...string) []ReasonEvidence {
	threshold := finding.Threshold
	evidence := []ReasonEvidence{{Signal: traceSignals[finding.Rule], Observed: finding.Observed, Comparison: finding.Comparison, Threshold: &threshold, Unit: finding.Unit}}
	for _, name := range supporting {
		if value, ok := d.traceMeasurement(name); ok {
			unit := "ratio"
			switch {
			case strings.HasSuffix(name, "_us"):
				unit = "us"
			case strings.HasSuffix(name, "_ms"):
				unit = "ms"
			}
			evidence = append(evidence, ReasonEvidence{Signal: "gpu_trace_" + name, Observed: value, Unit: unit})
		}
	}
	return evidence
}

func coverageEvidence(coverage float64) ReasonEvidence {
	threshold := rooflineCoverageFloor
	return ReasonEvidence{Signal: "gpu_trace_" + TraceRooflineCoveredShare, Observed: coverage, Comparison: ">=", Threshold: &threshold, Unit: "ratio"}
}

func queueOutcome(d Diagnosis) *ruleOutcome {
	finding, engineWait := d.finding(optimizationreadiness.RuleEngineQueue)
	if !engineWait && d.PrimaryBottleneck != optimizationreadiness.BottleneckQueueing {
		return nil
	}
	rule, evidence := RuleBottleneckQueueing, []ReasonEvidence{}
	cause := "Requests waited in line before the engine ran them"
	if engineWait {
		rule, evidence = finding.Rule, findingEvidence(finding)
		cause = fmt.Sprintf("Requests waited %s at p95 inside the engine before running (threshold %s)", millisecondsWords(finding.Observed), millisecondsWords(finding.Threshold))
	} else if stage, share := stageEvidence(d, "p95_queue"); share != "" {
		evidence = stage
		cause = "Requests spent " + share + " of p95 request time waiting in line"
	}
	if waiting, ok := waitingNow(d); ok && waiting > 0 {
		evidence = append(evidence, ReasonEvidence{Signal: optimizationreadiness.SignalWaitingPerReplica, Observed: waiting, Unit: "requests"})
	}
	return &ruleOutcome{rule: rule, evidence: evidence, picks: []techniquePick{
		{TechniqueBatchBudget, 90, cause + ", so admitting more requests into each batch comes first."},
	}}
}

// prefillOutcome covers a first-token-bound window. Prompt chunk size only
// matters once prompts approach the engine's per-step token budget, so short
// prompts choose nothing here: their first-token time is not prompt work.
func prefillOutcome(d Diagnosis) *ruleOutcome {
	if d.PrimaryBottleneck != optimizationreadiness.BottleneckPrefill {
		return nil
	}
	mean, ok := d.signal("mean_input_tokens")
	if !ok || mean < chunkedPromptTokens {
		return nil
	}
	evidence, share := stageEvidence(d, "p95_ttft")
	cause := "First tokens take most of the request time"
	if share != "" {
		cause = "First tokens take " + share + " of p95 request time"
	}
	threshold := float64(chunkedPromptTokens)
	evidence = append(evidence, ReasonEvidence{Signal: "mean_input_tokens", Observed: mean, Comparison: ">=", Threshold: &threshold, Unit: "tokens"})
	return &ruleOutcome{rule: RuleBottleneckPrefill, evidence: evidence, picks: []techniquePick{
		{TechniqueLargePrefill, 55, cause + fmt.Sprintf(" and prompts average %s tokens, so bigger prompt chunks finish prompts in fewer engine steps.", numberWords(mean))},
	}}
}

// decodeOutcome covers a decode-bound window the low-concurrency rule does not
// explain: many requests running per server, or concurrency not reported.
// Speculation with the model's own prediction head is chosen here too, with
// the draft length the cache has room for at the load the server is asked to
// hold (decodeSpeculation). When the speculation capacity rule already moved
// a full speculating server to fewer draft tokens, speculation is not chosen
// again here. A model without a prediction head skips it with that reason.
func decodeOutcome(d Diagnosis, longContext, throughputGoal, usefulGoal bool, cache cacheReading, cacheKnown, speculationDecided bool) *ruleOutcome {
	if d.PrimaryBottleneck != optimizationreadiness.BottleneckDecode {
		return nil
	}
	if _, low := d.finding(optimizationreadiness.RuleLowConcurrency); low {
		return nil
	}
	evidence, share := stageEvidence(d, "p95_generation")
	cause := "Writing answers takes most of the request time"
	if share != "" {
		cause = "Writing answers takes " + share + " of p95 request time"
	}
	running, known := d.signal(optimizationreadiness.SignalRunningPerReplica)
	if known {
		evidence = append(evidence, ReasonEvidence{Signal: optimizationreadiness.SignalRunningPerReplica, Observed: running, Unit: "requests"})
		cause += fmt.Sprintf(" with %s running per server", requestWords(running))
	}
	var picks []techniquePick
	var below map[string][]string
	var skips map[string]string
	if !speculationDecided {
		picks, below, skips = decodeSpeculation(cause, cache, cacheKnown, throughputGoal, usefulGoal)
	}
	if cacheKnown {
		evidence = append(evidence, cache.evidence()...)
	}
	if known && longContext {
		picks = append(picks, techniquePick{TechniqueFP8KVCache, 70, cause + " on long contexts, so reading each request's KV cache dominates; an FP8 KV cache halves those bytes."})
	} else if why, ok := cache.cacheReadWhy(cause); cacheKnown && ok {
		picks = append(picks, techniquePick{TechniqueFP8KVCache, 70, why})
		evidence = append(evidence, cache.memoryEvidence()...)
	}
	// Larger batches admit requests that wait for a batch slot; with none
	// waiting there is nobody to admit.
	if waiting, waitingKnown := waitingNow(d); known && throughputGoal && (!waitingKnown || waiting > 0) {
		picks = append(picks, techniquePick{TechniqueBatchBudget, 60, cause + ", so larger batches share each read of the model across more requests."})
	}
	picks = append(picks, techniquePick{TechniqueFP8Weights, 60, cause + ", so smaller weights move fewer bytes per step."})
	return &ruleOutcome{rule: RuleBottleneckDecode, evidence: evidence, picks: picks, below: below, skips: skips}
}

// planContext is what a rule may read beyond the diagnosis: the objective,
// the starting setup's engine and version, the accelerator it runs on, and
// the draft tokens the diagnosed server speculates with.
type planContext struct {
	objective, engine, version, gpu string
	drafts                          int
	// fp8Weights and fp8Cache say the starting setup already serves FP8 (or
	// lower) weights or an FP8 KV cache.
	fp8Weights, fp8Cache bool
	// supplied holds the drafts the plan's input names, by source.
	supplied map[string]speculator.Draft
}

// Planning priors for turning on an accelerator's own kernels: when the GPU's
// work is the measured bottleneck, and when the profile shows the kernels
// AITER replaces dominating GPU time.
const (
	acceleratorKernelBottleneckPriority = 75
	acceleratorKernelProfilePriority    = 70
)

// acceleratorKernelOutcome chooses a reviewed accelerator's own kernels when
// the GPU's work is the bottleneck: writing answers, first tokens of long
// prompts, or attention and matrix-multiply kernels dominating the
// profile. It fires only on a reviewed accelerator whose pinned engine has a
// reviewed switch for those kernels (support.EngineEnvironment); on every
// other GPU there is nothing to switch on. AMD documents AITER as fused
// attention, GEMM, MoE, and RMSNorm kernels for MI300X/MI325X and MI350
// Series GPUs, enabled in vLLM by VLLM_ROCM_USE_AITER=1, and the vLLM recipes'
// AMD preset sets it; vLLM 0.22.1 leaves it off by default.
func acceleratorKernelOutcome(d Diagnosis, facts planContext) *ruleOutcome {
	accelerator, switchable := kernelSwitchAccelerator(facts)
	if !switchable {
		return nil
	}
	var evidence []ReasonEvidence
	var causes []string
	priority := 0
	stage, words := "", ""
	switch d.PrimaryBottleneck {
	case optimizationreadiness.BottleneckDecode:
		stage, words = "p95_generation", "Writing answers takes"
	case optimizationreadiness.BottleneckPrefill:
		// Short prompts' first-token time is not prompt work (prefillOutcome).
		if mean, ok := d.signal("mean_input_tokens"); ok && mean >= chunkedPromptTokens {
			stage, words = "p95_ttft", "First tokens take"
		}
	}
	if stage != "" {
		stageRows, share := stageEvidence(d, stage)
		evidence = append(evidence, stageRows...)
		if share == "" {
			share = "most"
		}
		causes = append(causes, words+" "+share+" of p95 request time")
		priority = acceleratorKernelBottleneckPriority
	}
	for _, kernel := range []struct {
		class, words string
		threshold    float64
	}{{KernelClassAttention, "attention kernels", attentionHeavyShare}, {KernelClassGEMM, "matrix-multiply kernels", gemmHeavyShare}} {
		// A trace analysis of the same capture measures attention directly,
		// so its finding replaces the attention share, as for the FP8 KV
		// cache rule.
		if kernel.class == KernelClassAttention && d.trace() != nil {
			continue
		}
		if share, ok := d.kernelShare(kernel.class); ok && share >= kernel.threshold {
			evidence = append(evidence, profileEvidence(kernel.class, share, kernel.threshold)...)
			causes = append(causes, fmt.Sprintf("%s took %s of sampled GPU time in the profile", kernel.words, percentWords(share*100)))
			priority = max(priority, acceleratorKernelProfilePriority)
		}
	}
	if finding, ok := d.traceFinding(TraceRuleAttentionHeavy); ok {
		evidence = append(evidence, traceEvidence(d, finding)...)
		causes = append(causes, fmt.Sprintf("attention took %s of kernel time in the GPU trace (limit %s)", percentWords(finding.Observed*100), percentWords(finding.Threshold*100)))
		priority = max(priority, acceleratorKernelProfilePriority)
	}
	if priority == 0 {
		return nil
	}
	cause := strings.Join(causes, ", and ")
	why := fmt.Sprintf("%s. On the %s, %s %s runs its own attention and matrix-multiply kernels unless AMD's AITER kernels, built for this GPU, are switched on, so measuring with them on is worth a setup.", strings.ToUpper(cause[:1])+cause[1:], accelerator.Product, engineName(facts.engine), facts.version)
	return &ruleOutcome{rule: RuleAcceleratorKernels, evidence: evidence, picks: []techniquePick{{TechniqueAITER, priority, why}}}
}

// kernelSwitchAccelerator reports the reviewed accelerator when every switch
// of the kernel technique is reviewed for the starting setup's engine,
// version, and the accelerator's platform.
func kernelSwitchAccelerator(facts planContext) (provideridentity.Accelerator, bool) {
	accelerator, reviewed := provideridentity.LookupAccelerator(facts.gpu)
	variables := techniques[TechniqueAITER].env[facts.engine]
	if !reviewed || len(variables) == 0 {
		return provideridentity.Accelerator{}, false
	}
	for _, variable := range variables {
		if _, ok := support.LookupEngineEnvironment(facts.engine, facts.version, accelerator.Platform, variable.name); !ok {
			return provideridentity.Accelerator{}, false
		}
	}
	return accelerator, true
}

// stageEvidence returns a stage's share of p95 latency when both are known.
func stageEvidence(d Diagnosis, stage string) ([]ReasonEvidence, string) {
	value, ok := d.signal(stage)
	latency, latencyOK := d.signal("p95_latency")
	if !ok {
		return []ReasonEvidence{}, ""
	}
	evidence := []ReasonEvidence{{Signal: stage, Observed: value, Unit: "ms"}}
	if !latencyOK || latency <= 0 {
		return evidence, ""
	}
	evidence = append(evidence, ReasonEvidence{Signal: "p95_latency", Observed: latency, Unit: "ms"})
	return evidence, percentWords(math.Min(100, value/latency*100))
}

func profileEvidence(class string, share, threshold float64) []ReasonEvidence {
	return []ReasonEvidence{{Signal: "gpu_profile_" + class + "_share", Observed: share, Comparison: ">=", Threshold: &threshold, Unit: "ratio"}}
}

func findingEvidence(finding DiagnosisFinding) []ReasonEvidence {
	threshold := finding.Threshold
	return []ReasonEvidence{{Signal: finding.Signal, Observed: finding.Observed, Comparison: finding.Comparison, Threshold: &threshold, Unit: finding.Unit}}
}

func percentWords(value float64) string {
	if value >= 10 || value == 0 {
		return strconv.FormatFloat(math.Round(value), 'f', -1, 64) + "%"
	}
	return strconv.FormatFloat(round(value, 1), 'f', -1, 64) + "%"
}

func ratioWords(value float64) string {
	return strconv.FormatFloat(round(value, 3), 'f', -1, 64)
}

func numberWords(value float64) string {
	if value >= 100 {
		return strconv.FormatFloat(math.Round(value), 'f', -1, 64)
	}
	return strconv.FormatFloat(round(value, 1), 'f', -1, 64)
}

func requestWords(value float64) string {
	if value == 1 {
		return "1 request"
	}
	return numberWords(value) + " requests"
}

func millisecondsWords(value float64) string {
	if value >= 1000 {
		return strconv.FormatFloat(round(value/1000, 2), 'f', -1, 64) + " s"
	}
	return strconv.FormatFloat(math.Round(value), 'f', -1, 64) + " ms"
}

// nativeFP8Accelerator reports whether the GPU runs FP8 math in hardware:
// NVIDIA Ada, Hopper, and Blackwell, and the reviewed AMD Instinct CDNA 3 and
// CDNA 4 accelerators, where the pinned vLLM and SGLang images run FP8
// weights and an FP8 KV cache (sources in provideridentity). An unrecognized
// name is treated as not supported, so an FP8 setup is skipped rather than
// guessed.
func nativeFP8Accelerator(gpu string) bool {
	if accelerator, reviewed := provideridentity.LookupAccelerator(gpu); reviewed {
		return accelerator.NativeFP8
	}
	if checkpointcatalog.AcceleratorArchitecture(gpu) != "" {
		return true
	}
	switch provideridentity.NormalizedAccelerator(gpu) {
	case "l4", "l40", "l40s", "rtx4090", "rtx4080", "rtx4070", "rtx4070ti", "rtx6000ada", "rtx5000ada", "rtx4500ada", "rtx4000ada":
		return true
	}
	return false
}

// quantizedModelName reports a repository name that already marks quantized
// weights. It only ever skips an FP8 weight setup; it never adds one.
func quantizedModelName(model string) bool {
	lower := strings.ToLower(model)
	for _, marker := range []string{"fp8", "fp4", "int4", "int8", "awq", "gptq", "w4a16", "w8a8", "w8a16", "bnb", "4bit", "8bit", "gguf", "exl2"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// modelSizeBillions reads a parameter count the repository name states, such
// as 0.6B or 27B. It only ever skips a setup; an unstated size skips nothing.
func modelSizeBillions(model string) (float64, bool) {
	name := strings.ToLower(model)
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	if index := strings.Index(name, "@"); index >= 0 {
		name = name[:index]
	}
	for _, token := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' }) {
		if !strings.HasSuffix(token, "b") || len(token) < 2 {
			continue
		}
		if value, err := strconv.ParseFloat(strings.TrimSuffix(token, "b"), 64); err == nil && value > 0 {
			return value, true
		}
	}
	return 0, false
}

// sequenceCap is the running-request cap that removes about a quarter of the
// observed concurrency, rounded down to a multiple of eight.
func sequenceCap(running float64) (int, bool) {
	limit := int(math.Floor(running*sequenceCapFraction/sequenceCapStep)) * sequenceCapStep
	if limit < sequenceCapStep {
		limit = sequenceCapStep
	}
	return limit, float64(limit) < running
}
