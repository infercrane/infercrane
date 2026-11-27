package optimizer

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
)

// RuleSpeculationCapacity fires when a server that speculates with more than
// one draft token has run out of cache: requests wait, and each draft token
// keeps another copy of the model's recurrent state, so fewer draft tokens
// make room for more requests.
const RuleSpeculationCapacity = "kv_cache.speculation_capacity"

// RuleSpeculationBands fires when the draft length that fits changes with the
// load: 3 draft tokens fit below a load the server is asked to pass, 1 above
// it. One server can then keep both, one engine awake at a time.
const RuleSpeculationBands = "kv_cache.speculation_bands"

// Draft lengths proposed with the model's own prediction head.
const (
	speculationDrafts      = 3
	shortSpeculationDrafts = 1
)

// stateCacheShare is the share of a running request's cache blocks holding
// recurrent state at or above which an FP8 KV cache is not proposed to make
// room in a full cache. On such a model the engine sizes each block to one
// page of the state, so an FP8 KV cache makes a block hold more tokens and
// leaves the number of blocks where it was.
const stateCacheShare = 0.5

// Evidence names for values the capacity test derives from the engine's own
// readings. They are computed, never reported by an engine.
const (
	signalBlocksPerRunning = "kv_cache_blocks_per_running_request"
	signalRequestCapacity  = "kv_cache_request_capacity"
	signalRequestsAtOnce   = "requests_at_once_per_replica"
	signalStateShare       = "kv_cache_state_share"
)

// Model memory the server adds to a diagnosis when it read the model's pinned
// config and its safetensors summary (modelcheck): the bytes the served
// weights take on one server and the KV cache bytes one token takes at 16
// bits. They come from the checkpoint, never from an engine, and are absent
// when the model was not checked.
const (
	SignalModelWeightsBytes    = "model_weights_bytes"
	SignalKVCacheBytesPerToken = "kv_cache_bytes_per_token"
)

// kvReadShare is the share of the bytes one decode step reads that, when it is
// the running requests' KV cache, makes an FP8 KV cache worth measuring for
// speed: halving it saves at least half that share of every step's reads.
const kvReadShare = 0.10

// cacheReading is one server's cache as the capacity test sees it, from the
// engine's own metrics at the diagnosed load. A request a server runs holds
// cache blocks; the server runs as many requests at once as its blocks
// allow, and the rest wait.
type cacheReading struct {
	// running and waiting are requests per server; waitingKnown says whether
	// the engine reported waiting requests at all.
	running, waiting float64
	waitingKnown     bool
	// usage is the highest cache use, 0..1.
	usage float64
	// blocks and blockTokens are the engine's cache blocks per server and the
	// tokens one block holds; both are zero when the engine did not report
	// its cache layout.
	blocks, blockTokens float64
	// statePageBytes is set only for a model with recurrent-state layers.
	statePageBytes float64
	// contextTokens is the mean prompt plus answer length per request, an
	// upper bound on the tokens a running request holds; zero when unknown.
	contextTokens float64
	// drafts is the number of draft tokens the server speculates with now.
	drafts int
	// weightsBytes and kvBytesPerToken are the model's memory facts; zero
	// when the model was not checked.
	weightsBytes, kvBytesPerToken float64
	// inputTokens and outputTokens are the mean prompt and answer lengths.
	inputTokens, outputTokens float64
	// fp8Weights and fp8Cache say the starting setup already runs FP8
	// weights or an FP8 KV cache.
	fp8Weights, fp8Cache bool
}

// readCache reads the capacity test's inputs. It needs the running requests
// and the cache use; without either, nothing is known about room.
func readCache(d Diagnosis, drafts int) (cacheReading, bool) {
	running, runningOK := d.signal(optimizationreadiness.SignalRunningPerReplica)
	usage, usageOK := d.signal(metrics.MeasurementKVCacheUsage)
	if !runningOK || !usageOK || running <= 0 || usage <= 0 {
		return cacheReading{drafts: drafts}, false
	}
	reading := cacheReading{running: running, usage: math.Min(usage, 100) / 100, drafts: drafts}
	if waiting, ok := d.signal(optimizationreadiness.SignalWaitingPerReplica); ok && waiting >= 0 {
		reading.waiting, reading.waitingKnown = waiting, true
	}
	blocks, blocksOK := d.signal(optimizationreadiness.SignalCacheBlocksPerReplica)
	tokens, tokensOK := d.signal(metrics.MeasurementKVCacheBlockTokens)
	if blocksOK && tokensOK && blocks > 0 && tokens > 0 {
		reading.blocks, reading.blockTokens = blocks, tokens
		if page, ok := d.signal(metrics.MeasurementStatePageBytes); ok && page > 0 {
			reading.statePageBytes = page
		}
	}
	for _, name := range []string{"mean_input_tokens", optimizationreadiness.SignalMeanOutputTokens} {
		if value, ok := d.signal(name); ok && value > 0 {
			reading.contextTokens += value
		}
	}
	reading.inputTokens, _ = d.signal("mean_input_tokens")
	reading.outputTokens, _ = d.signal(optimizationreadiness.SignalMeanOutputTokens)
	if weights, ok := d.signal(SignalModelWeightsBytes); ok && weights > 0 {
		if perToken, ok := d.signal(SignalKVCacheBytesPerToken); ok && perToken > 0 {
			reading.weightsBytes, reading.kvBytesPerToken = weights, perToken
		}
	}
	return reading, true
}

func (r cacheReading) memoryKnown() bool { return r.weightsBytes > 0 && r.kvBytesPerToken > 0 }

// kvStepShare is the share of the bytes one decode step reads that is the
// running requests' KV cache: each running request reads its cache, on
// average its prompt and half its answer, and every request shares one read
// of the weights. With fp8Weights the weights are taken at half their bytes,
// as FP8 weights would serve them. It needs the model's memory facts.
func (r cacheReading) kvStepShare(fp8Weights bool) (float64, bool) {
	if !r.memoryKnown() || r.running <= 0 || r.inputTokens+r.outputTokens <= 0 {
		return 0, false
	}
	cache := r.running * (r.inputTokens + r.outputTokens/2) * r.kvBytesPerToken
	if r.fp8Cache {
		cache /= 2
	}
	weights := r.weightsBytes
	if fp8Weights && !r.fp8Weights {
		weights /= 2
	}
	return cache / (weights + cache), true
}

// memoryEvidence cites the model's memory facts a prediction used.
func (r cacheReading) memoryEvidence() []ReasonEvidence {
	if !r.memoryKnown() {
		return nil
	}
	return []ReasonEvidence{
		{Signal: SignalModelWeightsBytes, Observed: r.weightsBytes, Unit: "bytes"},
		{Signal: SignalKVCacheBytesPerToken, Observed: r.kvBytesPerToken, Unit: "bytes"},
	}
}

// cacheReadWhy says why an FP8 KV cache is worth measuring on a server that
// is busy reading memory: the running requests' KV cache is a share of the
// bytes each step reads, now or once FP8 weights halve the weights. It is
// false when that share is under kvReadShare or a fact is missing.
func (r cacheReading) cacheReadWhy(cause string) (string, bool) {
	if r.fp8Cache {
		return "", false
	}
	now, ok := r.kvStepShare(false)
	if !ok {
		return "", false
	}
	withWeights, _ := r.kvStepShare(true)
	if withWeights < kvReadShare {
		return "", false
	}
	why := fmt.Sprintf("%s. Each step reads the weights (%s) once and every running request's KV cache, about %s here: %s of the bytes a step reads",
		cause, gigabytesWords(r.weightsBytes), gigabytesWords(r.running*(r.inputTokens+r.outputTokens/2)*r.kvBytesPerToken), percentWords(now*100))
	if withWeights > now {
		why += fmt.Sprintf(", %s with FP8 weights", percentWords(withWeights*100))
	}
	return why + ". An FP8 KV cache halves those bytes.", true
}

// precisionPicks orders an FP8 KV cache and FP8 weights for a full cache by
// the room each is predicted to make (blocksWithPrecision): the one with more
// room comes first at the rule's priority, the other five below. It is false
// when either prediction cannot be made, and the rule keeps its fixed order.
func (r cacheReading) precisionPicks(cause string, priority int) ([]techniquePick, bool) {
	cacheRoom, cacheOK := r.capacityWithPrecision(TechniqueFP8KVCache)
	weightsRoom, weightsOK := r.capacityWithPrecision(TechniqueFP8Weights)
	if !cacheOK || !weightsOK {
		return nil, false
	}
	target := r.target()
	room := func(capacity float64) string {
		if capacity > target {
			return fmt.Sprintf("room for at most %s requests, above the %s sent at once", wholeWords(capacity), numberWords(target))
		}
		return fmt.Sprintf("room for at most %s requests, fewer than the %s sent at once", wholeWords(capacity), numberWords(target))
	}
	cacheWhy := fmt.Sprintf("%s, so an FP8 KV cache, which holds a block's tokens in half the bytes, would leave %s (about twice the %s blocks).", cause, room(cacheRoom), numberWords(r.blocks))
	weightsBlocks, _ := r.blocksWithPrecision(TechniqueFP8Weights)
	weightsWhy := fmt.Sprintf("%s, so FP8 weights, which free up to %s of the weights' memory for the cache, would leave %s (up to %s blocks).",
		cause, gigabytesWords(r.weightsBytes/2), room(weightsRoom), wholeWords(weightsBlocks))
	if weightsRoom > cacheRoom {
		return []techniquePick{{TechniqueFP8Weights, priority, weightsWhy}, {TechniqueFP8KVCache, priority - 5, cacheWhy}}, true
	}
	return []techniquePick{{TechniqueFP8KVCache, priority, cacheWhy}, {TechniqueFP8Weights, priority - 5, weightsWhy}}, true
}

func gigabytesWords(bytes float64) string {
	return oneDecimal(bytes/1e9) + " GB"
}

// Precision changes whose cache room the capacity test predicts.
var roomTechniques = []string{TechniqueFP8KVCache, TechniqueFP8Weights, TechniqueFP8WeightsKVCache}

// blocksWithPrecision predicts the cache blocks a server would have with a
// precision change, from the engine's own block count: an FP8 KV cache holds
// a block's tokens in half the bytes, so about twice the blocks fit; FP8
// weights free half the weights' bytes, which become blocks of blockTokens
// tokens at kvBytesPerToken each. Both are upper bounds: the embeddings and
// some layers stay at 16 bits, and the engine sizes its own buffers anew. It
// is false when the change does not apply or a fact is missing.
func (r cacheReading) blocksWithPrecision(technique string) (float64, bool) {
	if !r.layoutKnown() || r.recurrentState() {
		return 0, false
	}
	blocks := r.blocks
	weights := technique == TechniqueFP8Weights || technique == TechniqueFP8WeightsKVCache
	cache := technique == TechniqueFP8KVCache || technique == TechniqueFP8WeightsKVCache
	if weights {
		if r.fp8Weights || !r.memoryKnown() {
			return 0, false
		}
		perBlock := r.kvBytesPerToken * r.blockTokens
		if r.fp8Cache {
			perBlock /= 2
		}
		blocks += r.weightsBytes / 2 / perBlock
	}
	if cache {
		if r.fp8Cache {
			return 0, false
		}
		blocks *= 2
	}
	return blocks, true
}

// capacityWithPrecision is how many requests would fit at once with a
// precision change: the predicted blocks over the blocks a running request
// holds now. A precision change does not change how many tokens a block
// holds, so a request holds as many blocks as before.
func (r cacheReading) capacityWithPrecision(technique string) (float64, bool) {
	blocks, ok := r.blocksWithPrecision(technique)
	perRequest := r.blocksPerRequest()
	if !ok || perRequest <= 0 {
		return 0, false
	}
	return blocks / perRequest, true
}

func (r cacheReading) layoutKnown() bool { return r.blocks > 0 && r.blockTokens > 0 }

func (r cacheReading) recurrentState() bool { return r.layoutKnown() && r.statePageBytes > 0 }

// target is the load the server is asked to hold: the requests it runs plus
// those waiting for room.
func (r cacheReading) target() float64 { return r.running + r.waiting }

// capacity is how many requests fit at once: the blocks divided by the blocks
// one running request holds, which is the running requests over the cache use.
func (r cacheReading) capacity() float64 { return r.running / r.usage }

// blocksPerRequest is the highest cache use times the blocks, divided by the
// running requests. It needs the cache layout.
func (r cacheReading) blocksPerRequest() float64 { return r.usage * r.blocks / r.running }

// kvBlocks estimates the blocks a running request holds for its tokens: its
// prompt and answer in blocks of blockTokens, at least one. The rest of its
// blocks hold recurrent state.
func (r cacheReading) kvBlocks() float64 {
	kv := 1.0
	if r.contextTokens > 0 {
		kv = math.Max(1, math.Ceil(r.contextTokens/r.blockTokens))
	}
	return math.Min(kv, r.blocksPerRequest())
}

// blocksWith predicts the blocks one running request holds with the given
// draft tokens. With recurrent-state layers a request holds one copy of the
// state per state layer group, plus one more per draft token; without them
// draft tokens hold no state, and the blocks stay where they are.
func (r cacheReading) blocksWith(drafts int) float64 {
	perRequest := r.blocksPerRequest()
	if !r.recurrentState() || drafts == r.drafts {
		return perRequest
	}
	kv := r.kvBlocks()
	perCopy := math.Max(0, perRequest-kv) / float64(1+r.drafts)
	return kv + perCopy*float64(1+drafts)
}

// capacityWith predicts how many requests fit at once with the given draft
// tokens. For the server's own draft length it is the reading; for another,
// it needs the cache layout. The blocks are the server's own: a setup with
// a different draft length is not running, so its block count is not known,
// and adding a prediction head takes memory, so the prediction from a server
// without speculation is an upper bound.
func (r cacheReading) capacityWith(drafts int) (float64, bool) {
	if drafts == r.drafts {
		return r.capacity(), true
	}
	if !r.layoutKnown() {
		return 0, false
	}
	perRequest := r.blocksWith(drafts)
	if perRequest <= 0 {
		return 0, false
	}
	return r.blocks / perRequest, true
}

// stateShare is the share of a running request's blocks that hold recurrent
// state rather than its tokens. It is known only for a model with
// recurrent-state layers whose engine reported its cache layout.
func (r cacheReading) stateShare() (float64, bool) {
	if !r.recurrentState() {
		return 0, false
	}
	perRequest := r.blocksPerRequest()
	if perRequest <= 0 {
		return 0, false
	}
	return math.Max(0, perRequest-r.kvBlocks()) / perRequest, true
}

// evidence cites the readings the capacity test used and what it computed.
func (r cacheReading) evidence() []ReasonEvidence {
	out := []ReasonEvidence{
		{Signal: optimizationreadiness.SignalRunningPerReplica, Observed: r.running, Unit: "requests"},
	}
	if r.waitingKnown {
		out = append(out, ReasonEvidence{Signal: optimizationreadiness.SignalWaitingPerReplica, Observed: r.waiting, Unit: "requests"})
	}
	out = append(out, ReasonEvidence{Signal: metrics.MeasurementKVCacheUsage, Observed: round(r.usage*100, 2), Unit: "percent"})
	if r.layoutKnown() {
		out = append(out,
			ReasonEvidence{Signal: optimizationreadiness.SignalCacheBlocksPerReplica, Observed: r.blocks, Unit: "blocks"},
			ReasonEvidence{Signal: signalBlocksPerRunning, Observed: round(r.blocksPerRequest(), 2), Unit: "blocks"})
	}
	target := r.target()
	return append(out, ReasonEvidence{Signal: signalRequestCapacity, Observed: round(r.capacity(), 1), Comparison: ">", Threshold: &target, Unit: "requests"})
}

// stateCacheNote says why an FP8 KV cache is not proposed to make room on a
// model whose cache is mostly recurrent state, with its evidence. It returns
// false when the cache is not known to be mostly state.
func (r cacheReading) stateCacheNote() (string, []ReasonEvidence, bool) {
	share, known := r.stateShare()
	if !known || share < stateCacheShare {
		return "", nil, false
	}
	threshold := stateCacheShare
	why := fmt.Sprintf("An FP8 KV cache is not proposed: this model keeps a recurrent state, and the engine sizes each cache block to one page of that state, so an FP8 KV cache would make each block hold more tokens without adding blocks. About %s of the %s blocks a running request holds are that state.",
		percentWords(share*100), oneDecimal(r.blocksPerRequest()))
	return why, []ReasonEvidence{
		{Signal: metrics.MeasurementStatePageBytes, Observed: r.statePageBytes, Unit: "bytes"},
		{Signal: metrics.MeasurementKVCacheBlockTokens, Observed: r.blockTokens, Unit: "tokens"},
		{Signal: signalStateShare, Observed: round(share, 3), Comparison: ">=", Threshold: &threshold, Unit: "ratio"},
	}, true
}

// speculationCapacityOutcome moves a server that speculates with more than one
// draft token to one when its cache is full: requests wait, and one draft
// token makes room for more of them. It fires only for a model with
// recurrent-state layers, where each draft token holds blocks of state.
func speculationCapacityOutcome(r cacheReading, known, usefulGoal bool) *ruleOutcome {
	if !known || r.drafts < shortSpeculationDrafts || !r.recurrentState() {
		return nil
	}
	current, target := r.capacity(), r.target()
	if target < current {
		return nil
	}
	if usefulGoal {
		if off := speculationOffOutcome(r, current, target); off != nil {
			return off
		}
	}
	if r.drafts == shortSpeculationDrafts {
		return nil
	}
	fewer, ok := r.capacityWith(shortSpeculationDrafts)
	if !ok || fewer <= current {
		return nil
	}
	why := fmt.Sprintf("With %d draft tokens each running request holds %s of the server's %s cache blocks, room for about %s requests, so %s of the %s sent at once waited. Each draft token keeps another copy of the model's recurrent state; with 1 draft token a request would hold about %s blocks, room for about %s",
		r.drafts, oneDecimal(r.blocksPerRequest()), numberWords(r.blocks), wholeWords(current), numberWords(r.waiting), numberWords(target),
		oneDecimal(r.blocksWith(shortSpeculationDrafts)), wholeWords(fewer))
	priority := 100
	if target < fewer {
		why += fmt.Sprintf(", so all %s fit.", numberWords(target))
	} else {
		priority = 85
		why += fmt.Sprintf(", still fewer than the %s sent at once, so some would still wait; it is measured at your load.", numberWords(target))
	}
	return &ruleOutcome{rule: RuleSpeculationCapacity, evidence: r.evidence(), picks: []techniquePick{{TechniqueSpeculativeMTPOne, priority, why}}}
}

// speculationBandsOutcome plans speculation that changes with load on a
// speculating server of a model with recurrent-state layers whose load is
// past the room 3 draft tokens leave: below that room 3 draft tokens fit, and
// a fixed setup chosen for this load gives up their speed whenever the load
// falls back. It comes after the fixed choice for the present load.
func speculationBandsOutcome(r cacheReading, known, usefulGoal bool) *ruleOutcome {
	if !known || r.drafts < shortSpeculationDrafts || !r.recurrentState() {
		return nil
	}
	three, threeOK := r.capacityWith(speculationDrafts)
	one, oneOK := r.capacityWith(shortSpeculationDrafts)
	if !threeOK || !oneOK || three >= one || r.target() < three {
		return nil
	}
	top := "1 draft token above it"
	if usefulGoal {
		none, noneOK := r.capacityWith(0)
		if noneOK && none > one {
			top = fmt.Sprintf("1 up to %s, and none above, where requests would wait for room and miss the first-token limit", wholeWords(one))
		}
	}
	fits := fmt.Sprintf("The draft length that fits depends on the load: with 3 draft tokens about %s requests fit per server, with 1 about %s. The %s sent at once are past the first, so a fixed setup for this load gives up 3 draft tokens' speed whenever the load falls back below %s.",
		wholeWords(three), wholeWords(one), numberWords(r.target()), wholeWords(three))
	// With the patched engine image available, one engine changes the draft
	// length with the batch; the technique itself decides whether this run's
	// goal and traffic are the ones it was proven for, and says why not.
	if image, _ := stateByDraftImage(); image != "" {
		why := fits + " Speculation that changes with load in one engine drafts 3 tokens while the batch is below that room and none above it, and each request reserves only its own draft length's state."
		return &ruleOutcome{rule: RuleSpeculationBands, evidence: r.evidence(), picks: []techniquePick{{TechniqueAdaptiveSpeculationOneEngine, 40, why}}}
	}
	why := fits + fmt.Sprintf(" Speculation that changes with load keeps 3 draft tokens up to %s at once and %s, swapping engines on the server when a band has held.", wholeWords(three), top)
	return &ruleOutcome{rule: RuleSpeculationBands, evidence: r.evidence(), picks: []techniquePick{{TechniqueAdaptiveSpeculation, 40, why}}}
}

// decodeSpeculation chooses speculation with the model's own prediction head
// for a decode-bound window by the capacity test: 3 draft tokens while they
// fit at the load the server is asked to hold, 1 draft token while only that
// fits, and behind other changes when neither fits or room is not known.
// While a speculation setup fits, no FP8 KV cache ranks ahead of it.
func decodeSpeculation(cause string, r cacheReading, known, throughputGoal, usefulGoal bool) ([]techniquePick, map[string][]string, map[string]string) {
	measured := cause + ", so predicting several tokens per step cuts the steps each answer takes."
	if !known && r.drafts == speculationDrafts {
		return nil, nil, nil
	}
	if !known {
		return []techniquePick{{TechniqueSpeculativeMTP, 65, measured + " The engine reported no cache use or no running requests, so whether its draft tokens fit in the cache at this load is not known, and it is measured at your load."}}, nil, nil
	}
	target := r.target()
	three, threeKnown := r.capacityWith(speculationDrafts)
	if !threeKnown && r.drafts == speculationDrafts {
		return nil, nil, nil
	}
	if !threeKnown {
		return []techniquePick{{TechniqueSpeculativeMTP, 65, measured + " The engine did not report its cache layout, so whether 3 draft tokens fit in the cache at this load is not known, and it is measured at your load."}}, nil, nil
	}
	ahead := func(technique string) map[string][]string {
		return map[string][]string{technique: {TechniqueFP8KVCache}}
	}
	threeWords := fmt.Sprintf(" With 3 draft tokens a running request would hold about %s cache blocks, %s requests per server", oneDecimal(r.blocksWith(speculationDrafts)), r.roomWords(speculationDrafts, three))
	if !r.layoutKnown() {
		threeWords = fmt.Sprintf(" The cache has room for about %s requests per server", wholeWords(three))
	}
	if target < three {
		if r.drafts == speculationDrafts {
			return nil, nil, nil
		}
		return []techniquePick{{TechniqueSpeculativeMTP, 80, measured + threeWords + fmt.Sprintf(", above the %s sent at once, so it comes first.", numberWords(target))}}, ahead(TechniqueSpeculativeMTP), nil
	}
	if !r.recurrentState() {
		// Draft tokens hold no state here, so fewer of them make no room.
		return []techniquePick{{TechniqueSpeculativeMTP, 65, measured + threeWords + fmt.Sprintf(", not above the %s sent at once, so it is measured at your load rather than first.", numberWords(target))}}, nil, nil
	}
	if r.drafts == shortSpeculationDrafts {
		return nil, nil, nil
	}
	one, _ := r.capacityWith(shortSpeculationDrafts)
	threeWords += fmt.Sprintf(", fewer than the %s sent at once: each draft token keeps another copy of the model's recurrent state.", numberWords(target))
	oneWords := fmt.Sprintf(" With 1 draft token a request would hold about %s, %s", oneDecimal(r.blocksWith(shortSpeculationDrafts)), r.roomWords(shortSpeculationDrafts, one))
	if target < one {
		return []techniquePick{{TechniqueSpeculativeMTPOne, 80, measured + threeWords + oneWords + ", so 1 draft token comes first."}}, ahead(TechniqueSpeculativeMTPOne), nil
	}
	if usefulGoal {
		// Requests that wait for room get their first token late and count
		// for nothing under a first-token limit.
		why := fmt.Sprintf(", still fewer than the %s sent at once, so some would wait for room and get their first token late.", numberWords(target))
		return []techniquePick{{TechniqueSpeculativeMTPOne, 45, measured + threeWords + oneWords + why}}, nil,
			map[string]string{TechniqueSpeculativeMTPOne: "Not measured: your goal counts only requests within the first-token limit, and with either draft length some requests would wait for cache room and miss it."}
	}
	priority, why := 45, fmt.Sprintf(", still fewer than the %s sent at once, so some would wait for their first token; it is measured at your load.", numberWords(target))
	if throughputGoal {
		priority = 65
	}
	return []techniquePick{{TechniqueSpeculativeMTPOne, priority, measured + threeWords + oneWords + why}}, nil, nil
}

// roomWords says how many requests fit with the given draft tokens. A
// prediction from a server that does not speculate is an upper bound, because
// the prediction head it would load takes memory the cache then lacks.
func (r cacheReading) roomWords(drafts int, capacity float64) string {
	if r.drafts == 0 && drafts > 0 && r.layoutKnown() {
		return "room for at most " + wholeWords(capacity)
	}
	return "room for about " + wholeWords(capacity)
}

// currentDrafts is the number of draft tokens the diagnosed server speculates
// with: the engine's own reading, else the starting setup's flags, else none.
func currentDrafts(d Diagnosis, args []string) int {
	if value, ok := d.signal(metrics.MeasurementSpecDraftLength); ok && value >= 1 {
		return int(math.Round(value))
	}
	return speculationArgDrafts(args)
}

// speculationArgDrafts reads the draft tokens from vLLM's speculative config
// or SGLang's step count; it is zero when the flags set none.
func speculationArgDrafts(args []string) int {
	for index, arg := range args {
		flag, value, inline := strings.Cut(arg, "=")
		if !inline && index+1 < len(args) {
			value = args[index+1]
		}
		switch flag {
		case "--speculative-config":
			var config struct {
				Tokens int `json:"num_speculative_tokens"`
			}
			if json.Unmarshal([]byte(value), &config) == nil && config.Tokens > 0 {
				return config.Tokens
			}
		case "--speculative-num-steps":
			if steps, err := strconv.Atoi(value); err == nil && steps > 0 {
				return steps
			}
		}
	}
	return 0
}

func oneDecimal(value float64) string {
	return strconv.FormatFloat(round(value, 1), 'f', 1, 64)
}

// wholeWords is a count of requests that fit, rounded down.
func wholeWords(value float64) string {
	return strconv.FormatFloat(math.Floor(value), 'f', 0, 64)
}

// Basis of a room figure in a plan.
const (
	RoomMeasured   = "measured"
	RoomPredicted  = "predicted"
	RoomUpperBound = "upper_bound"
	RoomLowerBound = "lower_bound"
)

// CacheRoom is what the capacity test read and predicted for the diagnosed
// server: the load it is asked to hold and how many requests fit at once
// with each draft length of the model's own prediction head. It is a
// prediction a customer can check against a run, not a measured result.
type CacheRoom struct {
	// Load is requests running plus waiting per server.
	Load            float64 `json:"load"`
	Running         float64 `json:"running"`
	CacheUsePercent float64 `json:"cache_use_percent"`
	// Blocks is the engine's cache blocks per server, when it reported them.
	Blocks        *float64    `json:"blocks,omitempty"`
	CurrentDrafts int         `json:"current_drafts"`
	DraftLengths  []DraftRoom `json:"draft_lengths"`
	// Bands is the predicted setup per load band: the most draft tokens that
	// still fit. It is predicted from this one window, never measured per
	// band.
	Bands []LoadBand `json:"bands,omitempty"`
	// Precision is the room predicted with each precision change, from the
	// engine's blocks and the model's memory facts. Absent when the model was
	// not checked or the engine did not report its cache layout.
	Precision []PrecisionRoom `json:"precision,omitempty"`
}

// PrecisionRoom is the room predicted with one precision change.
type PrecisionRoom struct {
	Technique string  `json:"technique"`
	Blocks    float64 `json:"blocks"`
	Room      float64 `json:"room"`
	// Basis is always upper_bound: the embeddings and some layers stay at 16
	// bits, and the engine sizes its own buffers anew.
	Basis string `json:"basis"`
	Fits  bool   `json:"fits"`
}

// DraftRoom is the room for one draft length.
type DraftRoom struct {
	Drafts           int      `json:"drafts"`
	BlocksPerRequest *float64 `json:"blocks_per_request,omitempty"`
	Room             float64  `json:"room"`
	// Basis is measured for the server's own draft length, predicted for
	// another, upper_bound when predicted from a server without a prediction
	// head loaded, whose blocks a head would take some of, and lower_bound
	// for no speculation predicted from a server with one, whose memory the
	// head would free.
	Basis string `json:"basis"`
	Fits  bool   `json:"fits"`
}

// LoadBand is one band of load and the draft length predicted best for it.
type LoadBand struct {
	// UpTo is the largest load of the band, in requests at once per server;
	// absent for the band above every room.
	UpTo   *float64 `json:"up_to,omitempty"`
	Drafts int      `json:"drafts"`
}

// cacheRoom reports the room per draft length and per precision change, or
// nil when the engine did not report cache use and running requests.
func cacheRoom(d Diagnosis, drafts int, fp8Weights, fp8Cache bool) *CacheRoom {
	reading, known := readCache(d, drafts)
	if !known {
		return nil
	}
	reading.fp8Weights, reading.fp8Cache = fp8Weights, fp8Cache
	room := &CacheRoom{Load: round(reading.target(), 1), Running: reading.running, CacheUsePercent: round(reading.usage*100, 1), CurrentDrafts: drafts}
	if reading.layoutKnown() {
		blocks := reading.blocks
		room.Blocks = &blocks
	}
	lengths := []int{0, shortSpeculationDrafts, 2, speculationDrafts}
	for _, length := range lengths {
		capacity, ok := reading.capacityWith(length)
		if !ok || length != drafts && !reading.recurrentState() {
			continue
		}
		entry := DraftRoom{Drafts: length, Room: math.Floor(capacity), Basis: RoomMeasured}
		switch {
		case length == drafts:
		case drafts == 0:
			entry.Basis = RoomUpperBound
		case length == 0:
			entry.Basis = RoomLowerBound
		default:
			entry.Basis = RoomPredicted
		}
		if reading.layoutKnown() {
			perRequest := round(reading.blocksWith(length), 2)
			entry.BlocksPerRequest = &perRequest
		}
		entry.Fits = capacity > reading.target()
		room.DraftLengths = append(room.DraftLengths, entry)
	}
	for _, technique := range roomTechniques {
		blocks, ok := reading.blocksWithPrecision(technique)
		capacity, capacityOK := reading.capacityWithPrecision(technique)
		if ok && capacityOK {
			room.Precision = append(room.Precision, PrecisionRoom{Technique: technique, Blocks: math.Floor(blocks), Room: math.Floor(capacity), Basis: RoomUpperBound, Fits: capacity > reading.target()})
		}
	}
	if len(room.DraftLengths) == 0 && len(room.Precision) == 0 {
		return nil
	}
	// The most draft tokens that fit, band by band, over the lengths the
	// planner proposes: three, then one, then none above both.
	var previous float64
	for _, length := range []int{speculationDrafts, shortSpeculationDrafts} {
		for _, entry := range room.DraftLengths {
			if entry.Drafts == length && entry.Room > previous {
				upTo := entry.Room
				room.Bands = append(room.Bands, LoadBand{UpTo: &upTo, Drafts: length})
				previous = upTo
			}
		}
	}
	if len(room.Bands) > 0 {
		room.Bands = append(room.Bands, LoadBand{Drafts: 0})
	}
	return room
}

// speculationOffOutcome serves a speculating server without speculation when
// the goal counts only requests within the first-token limit: requests wait
// for cache room with every draft length, and without speculation they all
// fit. One draft token, which still leaves some waiting, comes second when
// the server runs more than one.
func speculationOffOutcome(r cacheReading, current, target float64) *ruleOutcome {
	one, oneKnown := r.capacityWith(shortSpeculationDrafts)
	none, noneKnown := r.capacityWith(0)
	if !oneKnown || !noneKnown || target < one || target >= none {
		return nil
	}
	why := fmt.Sprintf("With %d draft tokens each running request holds %s cache blocks, room for about %s requests, so %s of the %s sent at once waited for room; with 1 draft token there would be room for about %s, still fewer. Your goal counts only requests within the first-token limit, and a request that waits for room gets its first token late. Without speculation a request would hold about %s blocks, room for at least %s, so all would fit.",
		r.drafts, oneDecimal(r.blocksPerRequest()), wholeWords(current), numberWords(r.waiting), numberWords(target), wholeWords(one), oneDecimal(r.blocksWith(0)), wholeWords(none))
	picks := []techniquePick{{TechniqueSpeculationOff, 95, why}}
	if r.drafts > shortSpeculationDrafts {
		picks = append(picks, techniquePick{TechniqueSpeculativeMTPOne, 60, why + " One draft token is measured second: it writes faster per request and leaves fewer waiting than " + fmt.Sprint(r.drafts) + "."})
	}
	return &ruleOutcome{rule: RuleSpeculationCapacity, evidence: r.evidence(), picks: picks}
}
