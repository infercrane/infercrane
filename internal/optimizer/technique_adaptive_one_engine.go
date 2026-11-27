package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/metrics"
	contract "github.com/infercrane/infercrane/internal/technique"
)

// TechniqueAdaptiveSpeculationOneEngine is speculation that changes with load
// in one engine, whose recurrent state follows the draft length.
const TechniqueAdaptiveSpeculationOneEngine = "adaptive-speculation-one-engine"

// AdaptiveSpeculationOneEngineVersion is the version of the one-engine
// method; a change to how it sets the schedule is a new version.
const AdaptiveSpeculationOneEngineVersion = "0.1.0"

// PrefixCacheHitRateCeiling is the most of the traffic's input that may come
// from the prefix cache for the one-engine technique to apply: it serves with
// prefix caching off, which traffic with shared prefixes needs.
const PrefixCacheHitRateCeiling = 0.02

// scheduleTail closes vLLM's per-batch draft schedule when the setup sets no
// --max-num-seqs; vLLM caps it at the engine's own largest batch.
const scheduleTail = 1024

// bandEdgeShare places the three-draft band's edge below the room three draft
// tokens leave: a request reserves the state of the band it is admitted into,
// so a batch whose requests all reserve one draft length fills at that
// length's room, and an edge at the room is never passed (the gate, one H100,
// 26 November 2026: an edge of 110 against one draft token's room of about 111
// kept the batch at 110). The proofs ran an edge of 48 against three draft
// tokens' room of about 58: this share gives that edge from the same reading.
const bandEdgeShare = 0.83

// ProvenFirstTokenLimitMS is the first-token limit the technique was proven
// at: useful output within one second of first token.
const ProvenFirstTokenLimitMS = 1000.0

// The engine image. Its base is vLLM 0.34.0 for linux/amd64 (the image inside
// the product's pinned manifest list, 20fdb089...), with the patch in
// deploy/vllm-state-by-draft applied at build; its entrypoint
// serve-state-by-draft checks the receipt before it starts the server, so the
// technique's flags never start on another image.
const (
	StateByDraftBaseImage   = "vllm/vllm-openai@sha256:cf5b2fdd087eb4f6d261736967cc5cde974bd22ff4df50365f8fa419b6b4c484"
	StateByDraftPatchID     = "infercrane-state-by-draft-v4"
	StateByDraftPatchSHA256 = "53e1b1eda68b0e4c387002404740b0dffa6488603584d9bd55a26f4643a0db2e"
	StateByDraftCommand     = "/opt/infercrane/serve-state-by-draft"
	// StateByDraftPublishedImage is the image by registry digest once it is
	// published; until then the technique proposes nothing for customers.
	StateByDraftPublishedImage = ""
	// StateByDraftQualificationVariable names, for a qualification run only,
	// an image by digest built from deploy/vllm-state-by-draft before it is
	// published. The engine still checks the receipt at start.
	StateByDraftQualificationVariable = "INFERCRANE_QUALIFY_STATE_BY_DRAFT_IMAGE"
)

// The technique's evidence, which the product reads from the run itself
// (optimizationreport): the engine reporting, in every measured run, that
// it checked the image's receipt and its patched files' sha256 itself; and
// requests reserving three draft tokens and fewer running together.
const (
	EvidenceStateByDraftReceipt = "state-by-draft-receipt"
	EvidenceReservationMix      = "reservation-mix"
)

// The patched engine's gauges on /metrics (patch v3): running requests by
// the draft tokens each reserved, and the smaller of the counts reserving
// three and reserving fewer, above zero only when both ran together.
const StateByDraftMixedGauge = "vllm:infercrane_running_mixed_reservations"

// StateByDraftReceiptGauge is 1 when the engine found the image's receipt
// and its patched files' sha256 as built when its metrics logger started
// (patch v4), else 0.
const StateByDraftReceiptGauge = "vllm:infercrane_state_by_draft_receipt_verified"

// StateByDraftReservingGauges are the gauges of running requests reserving
// 0, 1 and 3 draft tokens.
var StateByDraftReservingGauges = []string{
	"vllm:infercrane_running_reserving_0_drafts",
	"vllm:infercrane_running_reserving_1_drafts",
	"vllm:infercrane_running_reserving_3_drafts",
}

// StateByDraftIdentity says what the engine image is made of.
var StateByDraftIdentity = StateByDraftBaseImage + " + " + StateByDraftPatchID + " (patch_state_by_draft.py sha256:" + StateByDraftPatchSHA256 + ")"

// stateByDraftImage is the image the technique proposes, and whether it may
// propose at all: the published image, else a qualification run's.
var stateByDraftImage = func() (image string, qualification bool) {
	if StateByDraftPublishedImage != "" {
		return StateByDraftPublishedImage, false
	}
	return strings.TrimSpace(os.Getenv(StateByDraftQualificationVariable)), true
}

// stateByDraftDisclosures travel with every claim of the technique. They
// describe the H100 lab runs and quote no figure from them: a claim's numbers
// come from the product's own run.
var stateByDraftDisclosures = []string{
	"All output, not only output within one second of first token, was not shown to improve against the best fixed setup in the H100 lab runs.",
	"At 128 requests at once it served less output within one second of first token than the plain vLLM command on the same H100; the cause is not read.",
	"It runs a patched vLLM 0.34.0 image with prefix caching off, and is refused for traffic that takes more than 2% of its input from the prefix cache.",
	"The H100 lab runs used patch v2 and vLLM's Rust front end; the product runs patch v4 and vLLM's Python front end.",
}

// AdaptiveSpeculationOneEngine is the one-engine form of "speculation that
// changes with load". vLLM's per-batch draft schedule
// (num_speculative_tokens_per_batch_size) lowers the draft length as the batch
// grows, and InferCrane's patch to vLLM 0.34.0 (infercrane-state-by-draft)
// makes each request reserve the recurrent state of the draft length the
// schedule gave the batch it joined, instead of the longest. The patch covers
// vLLM's recurrent-state cache mode "none" only, so the setup turns prefix
// caching off, and the manifest refuses traffic whose prefix cache serves
// more than PrefixCacheHitRateCeiling of its input, or whose hit rate nobody
// read. It never measures.
type AdaptiveSpeculationOneEngine struct{}

var _ contract.Technique = AdaptiveSpeculationOneEngine{}

// Manifest declares the technique. It is checked on vLLM 0.34.0 on one H100
// only, by the gate in tools/fast-track-map
// (rules-h100-state-by-draft-gate-2026-11-26.md).
func (AdaptiveSpeculationOneEngine) Manifest() contract.Manifest {
	return contract.Manifest{
		SchemaVersion: contract.SchemaV1,
		ID:            TechniqueAdaptiveSpeculationOneEngine,
		Version:       AdaptiveSpeculationOneEngineVersion,
		Label:         "Speculation that changes with load, in one engine",
		License:       "Apache-2.0",
		Requires: contract.Requires{
			Engines:       []contract.Engine{{Name: "vllm", Versions: []string{"0.34.0"}}},
			Accelerators:  []string{"H100"},
			ModelFeatures: []string{FeatureNativeMTP},
			// Prefix caching off: never for traffic that shares prefixes,
			// nor for traffic whose sharing nobody read.
			SignalsAtMost: map[string]float64{metrics.MeasurementPrefixCacheHitRate: PrefixCacheHitRateCeiling},
		},
		// Three draft tokens cut decode steps at a light load; at a heavy one
		// a request reserves less state, which frees cache room.
		Relieves: []contract.Limit{contract.LimitDecodeSteps, contract.LimitKVCacheRoom},
		// With prefix caching off a repeated prefix is computed again; the
		// ceiling admits only traffic where little repeats.
		Costs:      []contract.Limit{contract.LimitPrefillCompute},
		Prediction: contract.Method{Metric: "output_tokens_per_second", Basis: "cache-room"},
		// Every result carries the engine's own receipt check and its gauges
		// showing requests reserving three draft tokens and fewer running
		// together, both read from its metrics in every measured run.
		Evidence:       []string{EvidenceStateByDraftReceipt, EvidenceReservationMix},
		Disclosures:    slices.Clone(stateByDraftDisclosures),
		ChangesAnswers: true,
		Runtime:        contract.Runtime{Policy: contract.RuntimeAdaptive},
		Fallback: contract.Fallback{Serves: contract.FallbackStartingSetup,
			Reason: "the patch refuses any vLLM source whose sha256 it does not know, and then the starting setup serves unchanged"},
	}
}

// Candidates proposes the one-engine setup the H100 proofs measured, and only
// that: for a goal of useful output within one second of first token, three
// draft tokens up to the edge below three draft tokens' room and none above
// (on the proofs' model, [[1,48,3],[49,512,0]]), with prefix caching off, on
// the patched image whose entrypoint checks its receipt before the server
// starts. It proposes nothing until the image is published or a qualification
// run names one, for another goal, for a setup with engine environment (the
// patched image takes none), or where every load fits three draft tokens.
func (a AdaptiveSpeculationOneEngine) Candidates(_ context.Context, setup contract.Setup) ([]contract.Candidate, string, error) {
	manifest := a.Manifest()
	if ok, why := manifest.Admits(setup); !ok {
		return nil, why, nil
	}
	image, qualification := stateByDraftImage()
	switch {
	case image == "":
		return nil, "The patched engine image is not published yet; a qualification run names one in " + StateByDraftQualificationVariable + ".", nil
	case setup.Objective != lab.ObjectiveUsefulOutput || setup.FirstTokenLimitMS != ProvenFirstTokenLimitMS:
		return nil, fmt.Sprintf("It was proven for useful output within %s ms of first token only; this run's goal is %q at %s ms.",
			wholeWords(ProvenFirstTokenLimitMS), setup.Objective, wholeWords(setup.FirstTokenLimitMS)), nil
	case setup.Env["VLLM_USE_RUST_FRONTEND"] != "" && setup.Env["VLLM_USE_RUST_FRONTEND"] != "0":
		return nil, fmt.Sprintf("It needs vLLM's Python front end, whose metrics carry the patch's gauges; the starting setup sets VLLM_USE_RUST_FRONTEND=%s.", setup.Env["VLLM_USE_RUST_FRONTEND"]), nil
	case len(setup.Env) > 0:
		return nil, "The starting setup sets engine environment, and the patched image takes none: it runs vLLM's Python front end as built.", nil
	}
	drafts := speculationArgDrafts(setup.Args)
	if drafts < shortSpeculationDrafts {
		return nil, "The starting setup does not speculate, so the room with draft tokens is an upper bound only; speculation is planned on its own first.", nil
	}
	reading, known := readCache(Diagnosis{Signals: setup.Signals}, drafts)
	if !known || !reading.layoutKnown() {
		return nil, "The engine did not report its cache use, running requests and cache layout, so the room per draft length is not known.", nil
	}
	if !reading.recurrentState() {
		return nil, "On a model without recurrent-state layers a draft token reserves no state, so vLLM's own per-batch draft schedule changes the draft length with load without the patch.", nil
	}
	three, _ := reading.capacityWith(speculationDrafts)
	edge := int(math.Floor(three * bandEdgeShare))
	if edge < 1 {
		return nil, "Three draft tokens leave no room for a request, so no schedule starts with them.", nil
	}
	heaviest := reading.target()
	for _, load := range setup.Loads {
		heaviest = math.Max(heaviest, float64(load))
	}
	if heaviest <= three {
		return nil, fmt.Sprintf("Every load judged fits three draft tokens (room for about %s), so one draft length serves them all.", wholeWords(three)), nil
	}
	tail := max(scheduleTail, edge+1)
	if seqs, ok := maxNumSeqs(setup.Args); ok && seqs > edge {
		tail = seqs
	}
	encoded, err := json.Marshal([][3]int{{1, edge, speculationDrafts}, {edge + 1, tail, 0}})
	if err != nil {
		return nil, "", err
	}
	config := fmt.Sprintf(`{"method":"mtp","num_speculative_tokens":%d,"num_speculative_tokens_per_batch_size":%s}`, speculationDrafts, encoded)
	args, _ := withArgs(withoutFlags(setup.Args, []string{"--enable-prefix-caching"}), []runtimeArg{{"--speculative-config", config}, {"--no-enable-prefix-caching", ""}})
	reason := fmt.Sprintf("With 3 draft tokens a running request holds about %s cache blocks, room for about %s requests per server. "+
		"One engine drafts 3 tokens up to %d at once and none above, the setup the H100 proofs measured for useful output within one second; "+
		"with InferCrane's patch to vLLM each request reserves only its own draft length's state. Prefix caching is off, which this traffic barely "+
		"uses (%s of its input from the prefix cache).", oneDecimal(reading.blocksWith(speculationDrafts)), wholeWords(three), edge,
		percentWords(setup.Signals[metrics.MeasurementPrefixCacheHitRate]*100))
	source := contract.ImageSourcePublished
	if qualification {
		reason += " Qualification run: the image is named by " + StateByDraftQualificationVariable + ", not published."
		source = contract.ImageSourceQualification
	}
	candidate := contract.Candidate{
		Technique: manifest.ID, TechniqueVersion: manifest.Version, Label: manifest.Label,
		Args: args, ChangesAnswers: manifest.ChangesAnswers, Reason: reason,
		Bands: []contract.Band{{UpTo: edge, Args: args}, {Args: args}},
		Image: &contract.RuntimeImage{Image: image, Command: []string{StateByDraftCommand}, Identity: StateByDraftIdentity, Source: source},
	}
	return []contract.Candidate{candidate}, "", nil
}

// maxNumSeqs reads --max-num-seqs from engine flags.
func maxNumSeqs(args []string) (int, bool) {
	for index, arg := range args {
		value, found := "", false
		switch {
		case arg == "--max-num-seqs" && index+1 < len(args):
			value, found = args[index+1], true
		case strings.HasPrefix(arg, "--max-num-seqs="):
			value, found = strings.TrimPrefix(arg, "--max-num-seqs="), true
		}
		if found {
			number, err := strconv.Atoi(value)
			return number, err == nil && number > 0
		}
	}
	return 0, false
}
