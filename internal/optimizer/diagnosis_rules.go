package optimizer

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
	"github.com/infercrane/infercrane/internal/provideridentity"
)

// DiagnosisRulesVersion identifies the rule set that maps a diagnosis to the
// techniques worth measuring. A change to a rule, priority, gate, or flag
// value changes this version.
const DiagnosisRulesVersion = "diagnosis-technique-rules-v2"

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
	TechniqueFP8KVCache       = "fp8-kv-cache"
	TechniqueKVSequenceCap    = "kv-sequence-cap"
	TechniqueSpeculativeMTP   = "speculative-mtp"
	TechniqueSpeculativeDraft = "speculative-draft-model"
	TechniqueAPIServers       = "api-server-processes"
	TechniqueStreamInterval   = "stream-interval"
	TechniqueCUDAGraphs       = "cuda-graphs"
	TechniqueParallelism      = "parallelism-layout"
	TechniqueSpeculationTune  = "speculation-length"
	TechniqueRightSizing      = "right-sizing"
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
)

type runtimeArg struct{ flag, value string }

// technique is one reviewed change. args holds the exact flags per built-in
// engine and accelerator memory class; an engine without args can still rank
// an existing candidate that applies the technique, but no candidate is
// derived on that engine.
type technique struct {
	id, label string
	args      map[string][]runtimeArg
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
	modelFeature     string
	// outOfScope explains why the technique is never a derived candidate.
	outOfScope string
}

// techniqueOrder breaks priority ties and lists every technique once.
var techniqueOrder = []string{
	TechniqueFP8KVCache, TechniqueBatchBudget, TechniquePrefixCache, TechniquePrefixScheduling, TechniqueSpeculativeMTP,
	TechniqueSpeculativeDraft, TechniqueFP8Weights, TechniqueInt4Weights, TechniqueLargePrefill, TechniqueKVSequenceCap,
	TechniqueAPIServers, TechniqueStreamInterval, TechniqueCUDAGraphs, TechniquePrefixRouting, TechniqueParallelism,
	TechniqueSpeculationTune, TechniqueRightSizing,
}

var techniques = map[string]technique{
	TechniqueBatchBudget: {id: TechniqueBatchBudget, label: "Larger batches",
		sizedArgs: map[string]map[string][]runtimeArg{
			"vllm": {
				memorySmall: {{"--max-num-seqs", "512"}, {"--max-num-batched-tokens", "8192"}},
				memoryLarge: {{"--max-num-seqs", "2048"}, {"--max-num-batched-tokens", "16384"}},
			},
		},
		args: map[string][]runtimeArg{"sglang": {{"--max-running-requests", "1024"}, {"--chunked-prefill-size", "8192"}}}},
	TechniqueLargePrefill: {id: TechniqueLargePrefill, label: "Bigger prompt chunks",
		args: map[string][]runtimeArg{"vllm": {{"--max-num-batched-tokens", "16384"}}, "sglang": {{"--chunked-prefill-size", "16384"}}}},
	TechniquePrefixCache: {id: TechniquePrefixCache, label: "Prefix caching",
		defaultOn: map[string]bool{"vllm": true, "sglang": true},
		disables:  map[string][]string{"vllm": {"--no-enable-prefix-caching"}, "sglang": {"--disable-radix-cache"}}},
	TechniquePrefixScheduling: {id: TechniquePrefixScheduling, label: "Schedule requests that share a prompt together",
		args: map[string][]runtimeArg{"sglang": {{"--schedule-policy", "lpm"}}}},
	TechniquePrefixRouting: {id: TechniquePrefixRouting, label: "Route requests that share a prompt to the same server",
		outOfScope: "Routing is set on the endpoint, not on one server, so it is planned as a separate change."},
	TechniqueFP8Weights: {id: TechniqueFP8Weights, label: "FP8 weights", changesPrecision: true, lowersWeights: true, nativeFP8: true,
		args: map[string][]runtimeArg{"vllm": {{"--quantization", "fp8"}}, "sglang": {{"--quantization", "fp8"}}}},
	TechniqueInt4Weights: {id: TechniqueInt4Weights, label: "4-bit weights", changesPrecision: true, lowersWeights: true,
		outOfScope: "4-bit weights come only from reviewed publisher checkpoints, which serve different model files; a campaign cannot rank those against the starting setup yet."},
	TechniqueFP8KVCache: {id: TechniqueFP8KVCache, label: "FP8 KV cache", changesPrecision: true, nativeFP8: true,
		args: map[string][]runtimeArg{"vllm": {{"--kv-cache-dtype", "fp8"}}, "sglang": {{"--kv-cache-dtype", "fp8_e4m3"}}}},
	TechniqueKVSequenceCap: {id: TechniqueKVSequenceCap, label: "Fewer requests at once",
		args: map[string][]runtimeArg{"vllm": {{"--max-num-seqs", ""}}, "sglang": {{"--max-running-requests", ""}}}},
	TechniqueSpeculativeMTP: {id: TechniqueSpeculativeMTP, label: "Speculative decoding with the model's own prediction head", speculative: true, modelFeature: FeatureNativeMTP,
		args: map[string][]runtimeArg{
			"vllm":   {{"--speculative-config", `{"method":"mtp","num_speculative_tokens":1}`}},
			"sglang": {{"--speculative-algorithm", "NEXTN"}, {"--speculative-num-steps", "1"}, {"--speculative-eagle-topk", "1"}, {"--speculative-num-draft-tokens", "2"}},
		}},
	TechniqueSpeculativeDraft: {id: TechniqueSpeculativeDraft, label: "Speculative decoding with a draft model", speculative: true,
		outOfScope: "InferCrane has no reviewed draft model paired with this model yet, so no setup was added."},
	TechniqueAPIServers: {id: TechniqueAPIServers, label: "More API server processes",
		args: map[string][]runtimeArg{"vllm": {{"--api-server-count", "2"}}}},
	TechniqueStreamInterval: {id: TechniqueStreamInterval, label: "Stream tokens in small groups",
		args: map[string][]runtimeArg{"vllm": {{"--stream-interval", "2"}}}},
	TechniqueCUDAGraphs: {id: TechniqueCUDAGraphs, label: "CUDA graphs sized to your batches",
		outOfScope: "vLLM and SGLang already capture CUDA graphs by default; capture sized to your batches is offered only on reviewed pinned images."},
	TechniqueParallelism: {id: TechniqueParallelism, label: "Fewer GPUs per server",
		outOfScope: "Changing how many GPUs each server uses changes the hardware, so it is planned as a separate run."},
	TechniqueSpeculationTune: {id: TechniqueSpeculationTune, label: "Shorter speculation",
		outOfScope: "Setups start from an untuned server without speculation, so there is no speculation length to shorten here."},
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
}

// plannedChoice is a technique the diagnosis chose, with everything that
// chose it. Priority orders measurement; it is a planning prior, not a gain.
type plannedChoice struct {
	technique technique
	priority  int
	rules     []string
	reason    string
	evidence  []ReasonEvidence
}

// planTechniques applies every rule to the diagnosis and returns the chosen
// techniques by expected benefit. It returns no choice for a window that is
// not diagnosable: an unknown bottleneck is collected, never guessed.
func planTechniques(diagnosis Diagnosis, objective string) []plannedChoice {
	if diagnosis.State != optimizationreadiness.StateProfileReady {
		return nil
	}
	type contribution struct {
		pick     techniquePick
		rule     string
		order    int
		evidence []ReasonEvidence
	}
	byTechnique := map[string][]contribution{}
	order := 0
	for _, outcome := range diagnosisOutcomes(diagnosis, objective) {
		for _, pick := range outcome.picks {
			byTechnique[pick.technique] = append(byTechnique[pick.technique], contribution{pick: pick, rule: outcome.rule, order: order, evidence: outcome.evidence})
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
		choice := plannedChoice{technique: techniques[id], reason: contributions[0].pick.why}
		// Each corroborating rule adds a little, so agreeing evidence wins ties
		// without outranking a stronger direct cause.
		choice.priority = min(100, contributions[0].pick.priority+3*(len(contributions)-1))
		seen := map[string]bool{}
		for _, item := range contributions {
			if !contains(choice.rules, item.rule) {
				choice.rules = append(choice.rules, item.rule)
			}
			for _, evidence := range item.evidence {
				if !seen[evidence.Signal] {
					seen[evidence.Signal] = true
					choice.evidence = append(choice.evidence, evidence)
				}
			}
		}
		choices = append(choices, choice)
	}
	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].priority != choices[j].priority {
			return choices[i].priority > choices[j].priority
		}
		return techniqueRank(choices[i].technique.id) < techniqueRank(choices[j].technique.id)
	})
	return choices
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
func diagnosisOutcomes(d Diagnosis, objective string) []ruleOutcome {
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

	if finding, ok := d.finding(optimizationreadiness.RuleKVPreemption); ok {
		cause := fmt.Sprintf("The engine evicted running requests (%s per completed request, limit %s) because the KV cache was full", ratioWords(finding.Observed), ratioWords(finding.Threshold))
		add(ruleOutcome{rule: finding.Rule, evidence: findingEvidence(finding), picks: []techniquePick{
			{TechniqueFP8KVCache, 100, cause + ", so an FP8 KV cache, which holds about twice the tokens in the same memory, comes first."},
			{TechniqueKVSequenceCap, 80, cause + ", so running fewer requests at once should stop the evictions."},
		}})
	}
	if finding, ok := d.finding(optimizationreadiness.RuleKVSaturation); ok {
		cause := fmt.Sprintf("The KV cache was %s full (limit %s) while requests waited", percentWords(finding.Observed), percentWords(finding.Threshold))
		add(ruleOutcome{rule: finding.Rule, evidence: findingEvidence(finding), picks: []techniquePick{
			{TechniqueFP8KVCache, 95, cause + ", so an FP8 KV cache, which holds about twice the tokens in the same memory, comes first."},
			{TechniqueKVSequenceCap, 60, cause + ", so running fewer requests at once trades some queueing for fewer evictions."},
		}})
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
		picks = append(picks, techniquePick{TechniqueRightSizing, 50, fmt.Sprintf("Only %s ran at once per server, so the GPU may be larger than this traffic needs.", requestWords(finding.Observed))})
		add(ruleOutcome{rule: finding.Rule, evidence: findingEvidence(finding), picks: picks})
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
	if decode := decodeOutcome(d, longContext, throughputGoal); decode != nil {
		add(*decode)
	}
	if prefill := prefillOutcome(d); prefill != nil {
		add(*prefill)
	}
	if share, ok := d.kernelShare(KernelClassGEMM); ok && share >= gemmHeavyShare {
		why := fmt.Sprintf("Matrix-multiply kernels took %s of sampled GPU time in the profile, so smaller weights speed up the largest share of work.", percentWords(share*100))
		add(ruleOutcome{rule: RuleProfileGEMMHeavy, evidence: profileEvidence(KernelClassGEMM, share, gemmHeavyShare), picks: []techniquePick{{TechniqueFP8Weights, 70, why}, {TechniqueInt4Weights, 68, why}}})
	}
	if share, ok := d.kernelShare(KernelClassAttention); ok && share >= attentionHeavyShare && longContext {
		add(ruleOutcome{rule: RuleProfileAttentionHeavy, evidence: profileEvidence(KernelClassAttention, share, attentionHeavyShare), picks: []techniquePick{
			{TechniqueFP8KVCache, 70, fmt.Sprintf("Attention kernels took %s of sampled GPU time in the profile on long contexts, so an FP8 KV cache halves the bytes they read.", percentWords(share*100))},
		}})
	}
	if share, ok := d.kernelShare(KernelClassCommunication); ok && share >= communicationHeavyShare {
		add(ruleOutcome{rule: RuleProfileCommunicationBig, evidence: profileEvidence(KernelClassCommunication, share, communicationHeavyShare), picks: []techniquePick{
			{TechniqueParallelism, 60, fmt.Sprintf("GPU-to-GPU communication took %s of sampled GPU time in the profile.", percentWords(share*100))},
		}})
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
	return out
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
	if waiting, ok := d.signal(metrics.MeasurementRequestsWaiting); ok && waiting > 0 {
		evidence = append(evidence, ReasonEvidence{Signal: metrics.MeasurementRequestsWaiting, Observed: waiting, Unit: "requests"})
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
// Speculation is chosen only at low concurrency, where it pays.
func decodeOutcome(d Diagnosis, longContext, throughputGoal bool) *ruleOutcome {
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
	if known && longContext {
		picks = append(picks, techniquePick{TechniqueFP8KVCache, 70, cause + " on long contexts, so reading each request's KV cache dominates; an FP8 KV cache halves those bytes."})
	}
	if known && throughputGoal {
		picks = append(picks, techniquePick{TechniqueBatchBudget, 60, cause + ", so larger batches share each read of the model across more requests."})
	}
	picks = append(picks, techniquePick{TechniqueFP8Weights, 60, cause + ", so smaller weights move fewer bytes per step."})
	return &ruleOutcome{rule: RuleBottleneckDecode, evidence: evidence, picks: picks}
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
