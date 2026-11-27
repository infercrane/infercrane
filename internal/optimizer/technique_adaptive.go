package optimizer

import (
	"context"
	"fmt"
	"maps"
	"math"

	contract "github.com/infercrane/infercrane/internal/technique"
)

// AdaptiveSpeculationVersion is the version of the adaptive speculation
// technique's method; a change to how it chooses bands is a new version.
const AdaptiveSpeculationVersion = "0.1.0"

// AdaptiveSpeculation is the technique "speculation that changes with load":
// on a model whose draft tokens each keep a copy of its recurrent state, three
// draft tokens up to the room they leave and one draft token above it, each
// band's setup served by its own engine behind infercrane-specswitch. The
// bands come from the engine's own cache readings (cacheReading), the same
// arithmetic as the plan's cache_room. It never measures.
type AdaptiveSpeculation struct{}

var _ contract.Technique = AdaptiveSpeculation{}

// Manifest declares the technique. It was checked on vLLM 0.34.0 on one H100
// only (the map's model); every other engine, version, and GPU is refused
// until measured.
func (AdaptiveSpeculation) Manifest() contract.Manifest {
	return contract.Manifest{
		SchemaVersion: contract.SchemaV1,
		ID:            TechniqueAdaptiveSpeculation,
		Version:       AdaptiveSpeculationVersion,
		Label:         techniques[TechniqueAdaptiveSpeculation].label,
		License:       "Apache-2.0",
		Requires: contract.Requires{
			Engines:       []contract.Engine{{Name: "vllm", Versions: []string{"0.34.0"}}},
			Accelerators:  []string{"H100"},
			ModelFeatures: []string{FeatureNativeMTP},
		},
		// Three draft tokens cut decode steps at a light load; one draft
		// token frees the cache room three reserve at a heavy one.
		Relieves: []contract.Limit{contract.LimitDecodeSteps, contract.LimitKVCacheRoom},
		// A swap holds new requests for its drain, sleep and wake; the
		// sleeping engines keep some GPU memory (a few GiB each on the gate),
		// which the awake engine's cache gives up. Each engine also keeps a
		// pinned host copy of its weights, graphs and buffers from its first
		// sleep on (vLLM 0.34.0): about the weights' size per engine.
		Costs:      []contract.Limit{contract.LimitFirstToken, contract.LimitGPUMemory, contract.LimitHostMemory},
		Prediction: contract.Method{Metric: "output_tokens_per_second", Basis: "cache-room"},
		// Every result carries the switch's record of each swap (drain, sleep,
		// wake of weights, wake of cache, first token): what a swap cost is
		// part of the claim, not a footnote.
		Evidence:       []string{"swap-timings"},
		ChangesAnswers: true,
		Runtime:        contract.Runtime{Policy: contract.RuntimeAdaptive},
		Fallback: contract.Fallback{Serves: contract.FallbackStartingSetup,
			Reason: "each band's engine runs the starting setup's command with only its draft length changed; without the switch the starting setup serves unchanged"},
	}
}

// Candidates proposes the two-band setup when the starting setup speculates
// with the model's prediction head, the engine reported its cache layout,
// the model keeps recurrent state per draft token, and a load the setup is
// judged at (or the diagnosed load) is past the room three draft tokens leave.
func (a AdaptiveSpeculation) Candidates(_ context.Context, setup contract.Setup) ([]contract.Candidate, string, error) {
	drafts := speculationArgDrafts(setup.Args)
	if drafts < shortSpeculationDrafts {
		return nil, "The starting setup does not speculate, so the room with draft tokens is an upper bound only; speculation is planned on its own first.", nil
	}
	reading, known := readCache(Diagnosis{Signals: setup.Signals}, drafts)
	if !known || !reading.layoutKnown() {
		return nil, "The engine did not report its cache use, running requests and cache layout, so the room per draft length is not known.", nil
	}
	if !reading.recurrentState() {
		return nil, "On a model without recurrent-state layers a draft token reserves no state, so the engine's own per-batch draft schedule (vLLM's num_speculative_tokens_per_batch_size) can change the draft length with load without a second engine.", nil
	}
	three, _ := reading.capacityWith(speculationDrafts)
	one, _ := reading.capacityWith(shortSpeculationDrafts)
	edge := int(math.Floor(three))
	if edge < 1 || one <= three {
		return nil, "One draft token leaves no more room than three, so a second engine has nothing to give.", nil
	}
	heaviest := reading.target()
	for _, load := range setup.Loads {
		heaviest = math.Max(heaviest, float64(load))
	}
	if heaviest <= three {
		return nil, fmt.Sprintf("Every load judged fits three draft tokens (room for about %s), so one setup serves them all.", wholeWords(three)), nil
	}
	threeArgs, _ := withArgs(setup.Args, techniques[TechniqueSpeculativeMTP].args["vllm"])
	oneArgs, _ := withArgs(setup.Args, techniques[TechniqueSpeculativeMTPOne].args["vllm"])
	args := oneArgs
	if reading.target() <= three {
		args = threeArgs
	}
	manifest := a.Manifest()
	return []contract.Candidate{{
		Technique: manifest.ID, TechniqueVersion: manifest.Version, Label: manifest.Label,
		Args: args, Env: maps.Clone(setup.Env),
		Bands: []contract.Band{
			{UpTo: edge, Args: threeArgs, Env: maps.Clone(setup.Env)},
			{Args: oneArgs, Env: maps.Clone(setup.Env)},
		},
		ChangesAnswers: manifest.ChangesAnswers,
		Reason: fmt.Sprintf("With 3 draft tokens a running request holds about %s cache blocks, room for about %s requests per server; with 1 about %s, room for about %s. "+
			"Up to %d at once 3 draft tokens serve, above it 1, each from its own engine on the server, swapped when a band has held; a swap holds new requests "+
			"for its drain, sleep and wake, the sleeping engine's GPU memory comes out of the awake one's cache, and each engine keeps a copy "+
			"of its weights in host memory.",
			oneDecimal(reading.blocksWith(speculationDrafts)), wholeWords(three), oneDecimal(reading.blocksWith(shortSpeculationDrafts)), wholeWords(one), edge),
	}}, "", nil
}
