package optimizer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/metrics"
	contract "github.com/infercrane/infercrane/internal/technique"
)

var qualificationImage = "infercrane/vllm-state-by-draft@sha256:" + strings.Repeat("e", 64)

// withImage runs the test with the technique's image as a qualification run
// names it ("" for none).
func withImage(t *testing.T, image string) {
	t.Helper()
	previous := stateByDraftImage
	stateByDraftImage = func() (string, bool) { return image, true }
	t.Cleanup(func() { stateByDraftImage = previous })
}

// oneEngineSetup is the map's three-draft server at 32 at once, judged at 32
// and 128, for a goal of useful output within one second, whose traffic took
// the given share of its input from the prefix cache.
func oneEngineSetup(hitRate float64, args []string, loads ...int) contract.Setup {
	setup := mapSetup(shortCell(32, 3, 31, 0, 53), args, loads...)
	setup.Signals = maps.Clone(setup.Signals)
	setup.Signals[metrics.MeasurementPrefixCacheHitRate] = hitRate
	setup.Objective, setup.FirstTokenLimitMS = lab.ObjectiveUsefulOutput, lab.DefaultFirstTokenLimitMS
	return setup
}

func scheduleOf(t *testing.T, args []string) [][3]int {
	t.Helper()
	index := slices.Index(args, "--speculative-config")
	if index < 0 || index+1 >= len(args) {
		t.Fatalf("no speculative config in %v", args)
	}
	var config struct {
		Method   string   `json:"method"`
		Drafts   int      `json:"num_speculative_tokens"`
		Schedule [][3]int `json:"num_speculative_tokens_per_batch_size"`
	}
	if err := json.Unmarshal([]byte(args[index+1]), &config); err != nil {
		t.Fatal(err)
	}
	if config.Method != "mtp" || config.Drafts != 3 {
		t.Fatalf("config %+v", config)
	}
	return config.Schedule
}

// On the proofs' model and readings the technique proposes exactly the setup
// the two H100 proofs measured: [[1,48,3],[49,512,0]], prefix caching off, on
// the patched image started by its receipt check.
func TestOneEngineProposesExactlyTheProvenSetup(t *testing.T) {
	withImage(t, qualificationImage)
	registry, err := contract.NewRegistry(AdaptiveSpeculationOneEngine{})
	if err != nil {
		t.Fatal(err)
	}
	candidates, skipped, err := registry.Candidates(context.Background(), oneEngineSetup(0, mapThreeDrafts, 32, 128))
	if err != nil || len(skipped) != 0 || len(candidates) != 1 {
		t.Fatalf("candidates %+v, skipped %+v, err %v", candidates, skipped, err)
	}
	candidate := candidates[0]
	if schedule := scheduleOf(t, candidate.Args); !slices.Equal(schedule, [][3]int{{1, 48, 3}, {49, 512, 0}}) {
		t.Fatalf("schedule %v; want the proven [[1,48,3],[49,512,0]]", schedule)
	}
	if strings.Count(strings.Join(candidate.Args, " "), "--no-enable-prefix-caching") != 1 || len(candidate.Env) != 0 {
		t.Fatalf("args %v env %v", candidate.Args, candidate.Env)
	}
	if len(candidate.Bands) != 2 || candidate.Bands[0].UpTo != 48 || candidate.Bands[1].UpTo != 0 || !slices.Equal(candidate.Bands[0].Args, candidate.Args) {
		t.Fatalf("bands %+v", candidate.Bands)
	}
	if image := candidate.Image; image == nil || image.Image != qualificationImage || !slices.Equal(image.Command, []string{StateByDraftCommand}) ||
		!strings.Contains(image.Identity, StateByDraftPatchSHA256) || !strings.Contains(image.Identity, StateByDraftBaseImage) || image.Source != contract.ImageSourceQualification {
		t.Fatalf("image %+v", candidate.Image)
	}
	if !strings.Contains(candidate.Reason, "Qualification run") {
		t.Fatalf("a qualification run's candidate does not say so: %s", candidate.Reason)
	}
	manifest := AdaptiveSpeculationOneEngine{}.Manifest()
	if manifest.Requires.SignalsAtMost[metrics.MeasurementPrefixCacheHitRate] != PrefixCacheHitRateCeiling || len(manifest.Disclosures) != 4 ||
		!slices.Equal(manifest.Evidence, []string{"state-by-draft-receipt", "reservation-mix"}) || manifest.Runtime.Policy != contract.RuntimeAdaptive {
		t.Fatalf("manifest %+v", manifest)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOneEngineTurnsAnExplicitPrefixCacheOff(t *testing.T) {
	withImage(t, qualificationImage)
	args := append([]string{"--enable-prefix-caching"}, mapThreeDrafts...)
	candidates, why, err := (AdaptiveSpeculationOneEngine{}).Candidates(context.Background(), oneEngineSetup(0.01, args, 32, 128))
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates %+v, why %q, err %v", candidates, why, err)
	}
	if slices.Contains(candidates[0].Args, "--enable-prefix-caching") || !slices.Contains(candidates[0].Args, "--no-enable-prefix-caching") {
		t.Fatalf("args %v", candidates[0].Args)
	}
}

// Prefix caching off is the technique's condition: traffic that shares
// prefixes, or whose sharing nobody read, is never given the setup, through
// the registry or by a direct call.
func TestOneEngineNeverServesSharedPrefixOrUnreadTraffic(t *testing.T) {
	withImage(t, qualificationImage)
	registry, err := contract.NewRegistry(AdaptiveSpeculationOneEngine{})
	if err != nil {
		t.Fatal(err)
	}
	unread := oneEngineSetup(0, mapThreeDrafts, 32, 128)
	delete(unread.Signals, metrics.MeasurementPrefixCacheHitRate)
	for name, setup := range map[string]contract.Setup{
		"shared prefixes":  oneEngineSetup(0.31, mapThreeDrafts, 32, 128),
		"just over":        oneEngineSetup(PrefixCacheHitRateCeiling+0.001, mapThreeDrafts, 32, 128),
		"no reading of it": unread,
	} {
		candidates, skipped, err := registry.Candidates(context.Background(), setup)
		if err != nil || len(candidates) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0].Reason, metrics.MeasurementPrefixCacheHitRate) {
			t.Errorf("%s: registry gave %+v, skipped %+v, err %v", name, candidates, skipped, err)
		}
		direct, why, err := (AdaptiveSpeculationOneEngine{}).Candidates(context.Background(), setup)
		if err != nil || len(direct) != 0 || !strings.Contains(why, metrics.MeasurementPrefixCacheHitRate) {
			t.Errorf("%s: a direct call gave %+v, why %q, err %v", name, direct, why, err)
		}
	}
}

// It proposes nothing it was not proven for: no image yet, another goal or
// first-token limit, engine environment, no recurrent state, no speculation to
// start from, or every load inside three draft tokens' room.
func TestOneEngineProposesNothingItWasNotProvenFor(t *testing.T) {
	noState := oneEngineSetup(0, mapThreeDrafts, 32, 128)
	delete(noState.Signals, metrics.MeasurementStatePageBytes)
	otherGoal := oneEngineSetup(0, mapThreeDrafts, 32, 128)
	otherGoal.Objective, otherGoal.FirstTokenLimitMS = "throughput", 0
	otherLimit := oneEngineSetup(0, mapThreeDrafts, 32, 128)
	otherLimit.FirstTokenLimitMS = 500
	rustFrontEnd := oneEngineSetup(0, mapThreeDrafts, 32, 128)
	rustFrontEnd.Env = map[string]string{"VLLM_USE_RUST_FRONTEND": "1"}
	withEnv := oneEngineSetup(0, mapThreeDrafts, 32, 128)
	withEnv.Env = map[string]string{"VLLM_ATTENTION_BACKEND": "FLASHINFER"}
	plain := mapSetup(shortCell(128, 0, 127, 0, 59), []string{"--max-num-seqs", "512"}, 32, 128)
	plain.Signals = maps.Clone(plain.Signals)
	plain.Signals[metrics.MeasurementPrefixCacheHitRate] = 0
	plain.Objective, plain.FirstTokenLimitMS = lab.ObjectiveUsefulOutput, lab.DefaultFirstTokenLimitMS
	cases := map[string]struct {
		image string
		setup contract.Setup
		says  string
	}{
		"no image published or named":  {"", oneEngineSetup(0, mapThreeDrafts, 32, 128), StateByDraftQualificationVariable},
		"another goal":                 {qualificationImage, otherGoal, "proven for useful output"},
		"another first-token limit":    {qualificationImage, otherLimit, "proven for useful output"},
		"vLLM's Rust front end":        {qualificationImage, rustFrontEnd, "needs vLLM's Python front end"},
		"other engine environment":     {qualificationImage, withEnv, "environment"},
		"no recurrent state":           {qualificationImage, noState, "recurrent-state"},
		"every load fits three drafts": {qualificationImage, oneEngineSetup(0, mapThreeDrafts, 32, 48), "fits three draft tokens"},
		"no speculation to start from": {qualificationImage, plain, "does not speculate"},
	}
	for name, test := range cases {
		withImage(t, test.image)
		candidates, why, err := (AdaptiveSpeculationOneEngine{}).Candidates(context.Background(), test.setup)
		if err != nil || len(candidates) != 0 || !strings.Contains(why, test.says) {
			t.Errorf("%s: candidates %+v, why %q, err %v", name, candidates, why, err)
		}
	}
}

// The setup reaches a deployment as the built-in vLLM workload on the
// patched image, its command the receipt check in place of "vllm serve".
func TestOneEngineSetupRunsOnlyOnItsImage(t *testing.T) {
	workload, err := engineWorkload(contract.RuntimeImage{Image: qualificationImage, Command: []string{StateByDraftCommand}})
	if err != nil {
		t.Fatal(err)
	}
	if workload.Image != qualificationImage || workload.Command[0] != StateByDraftCommand || slices.Contains(workload.Command, "vllm") ||
		workload.Command[1] != "${MODEL}" || workload.MetricsPath != "/metrics" {
		t.Fatalf("workload %+v", workload)
	}
	if _, err := engineWorkload(contract.RuntimeImage{Image: "vllm/vllm-openai:v0.34.0", Command: []string{StateByDraftCommand}}); err == nil {
		t.Fatal("an image by tag became a workload")
	}
}

// The reviewed patch is the one pinned here: an edit to it fails until the pin
// is reviewed and changed, and the proofs ran these bytes.
func TestTheStateByDraftPatchIsTheReviewedOne(t *testing.T) {
	body, err := os.ReadFile("../../deploy/vllm-state-by-draft/patch_state_by_draft.py")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != StateByDraftPatchSHA256 {
		t.Fatalf("patch_state_by_draft.py has sha256 %s, the reviewed one is %s", got, StateByDraftPatchSHA256)
	}
	if !strings.Contains(string(body), `PATCH_ID = "`+StateByDraftPatchID+`"`) {
		t.Fatalf("the patch does not name itself %s", StateByDraftPatchID)
	}
	if StateByDraftPublishedImage != "" {
		t.Fatal("the image is published only by the publish step, with the user's approval")
	}
}

// oneEnginePlan plans for the map's endpoint running three draft tokens at
// 128 at once, whose traffic took the given share of its input from the
// prefix cache.
func oneEnginePlan(t *testing.T, objective string, hitRate float64) (Proposal, *PlannedTechnique) {
	t.Helper()
	return oneEnginePlanWithin(t, objective, hitRate, 6)
}

// oneEnginePlanWithin plans as oneEnginePlan does with a bound on the
// setups (0: the product's default).
func oneEnginePlanWithin(t *testing.T, objective string, hitRate float64, bound int) (Proposal, *PlannedTechnique) {
	t.Helper()
	diagnosis := shortCell(128, 3, 58, 69, 99).diagnosis()
	diagnosis.Signals[metrics.MeasurementPrefixCacheHitRate] = hitRate
	request := genericRequest("H100", diagnosis)
	request.Objective, request.MaxCandidates = objective, bound
	request.ModelFeatures, request.ModelConfigDigest = []string{FeatureNativeMTP}, recipeTestDigest
	request.StartingSetup = &StartingSetup{Deployment: "chat", Revision: 3, Engine: "vllm", Version: "0.34.0", Args: mapThreeDrafts, GPU: "H100", GPUCount: 1}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatal(err)
	}
	for index, planned := range proposal.Diagnosis.Techniques {
		if planned.Technique == TechniqueAdaptiveSpeculation {
			t.Fatalf("with the one-engine image the plan still lists the two-engine switch: %+v", planned)
		}
		if planned.Technique == TechniqueAdaptiveSpeculationOneEngine {
			return proposal, &proposal.Diagnosis.Techniques[index]
		}
	}
	return proposal, nil
}

// With the patched image available, rule kv_cache.speculation_bands chooses
// the one-engine technique, and a useful-output plan past three draft
// tokens' room measures the proven setup on the patched image, its
// disclosures among the setup's limitations.
func TestAPlanPastThreeDraftsRoomMeasuresTheOneEngineSetup(t *testing.T) {
	withImage(t, qualificationImage)
	proposal, planned := oneEnginePlan(t, lab.ObjectiveUsefulOutput, 0)
	if planned == nil || len(planned.CandidateIDs) != 1 || planned.Skipped != "" || !slices.Contains(planned.Rules, RuleSpeculationBands) {
		t.Fatalf("planned %+v", planned)
	}
	var candidate *Candidate
	for index := range proposal.Candidates {
		if proposal.Candidates[index].ID == planned.CandidateIDs[0] {
			candidate = &proposal.Candidates[index]
		}
	}
	if candidate == nil || candidate.Technique == nil || candidate.Technique.ID != TechniqueAdaptiveSpeculationOneEngine {
		t.Fatalf("candidate %+v", candidate)
	}
	runtime := candidate.Deployment.Runtime
	if schedule := scheduleOf(t, runtime.Args); !slices.Equal(schedule, [][3]int{{1, 48, 3}, {49, 512, 0}}) {
		t.Fatalf("schedule %v", schedule)
	}
	if runtime.Workload.Image != qualificationImage || runtime.Workload.Command[0] != StateByDraftCommand || len(runtime.Env) != 0 {
		t.Fatalf("runtime %+v", runtime)
	}
	// The same request without the variable plans differently, so the setup
	// says where its image came from.
	if !slices.ContainsFunc(candidate.Features, func(feature Feature) bool {
		return feature.Name == "engine_image_source" && feature.State == contract.ImageSourceQualification
	}) || !strings.Contains(strings.Join(candidate.Limitations, " "), StateByDraftQualificationVariable) {
		t.Fatalf("features %+v", candidate.Features)
	}
	for _, disclosure := range stateByDraftDisclosures {
		if !slices.Contains(candidate.Limitations, disclosure) {
			t.Fatalf("limitations %v lack %q", candidate.Limitations, disclosure)
		}
	}
	for _, evidence := range []string{EvidenceStateByDraftReceipt, EvidenceReservationMix} {
		if !strings.Contains(strings.Join(candidate.RequiredEvidence, " "), evidenceSentence(evidence)) {
			t.Fatalf("required evidence %v lacks %s", candidate.RequiredEvidence, evidence)
		}
	}
}

// The rule chooses it, and the technique says why it offers nothing: another
// goal, or traffic that shares prefixes.
func TestTheOneEngineRuleRecordsWhyItMeasuredNothing(t *testing.T) {
	withImage(t, qualificationImage)
	for name, test := range map[string]struct {
		objective string
		hitRate   float64
		says      string
	}{
		"a throughput goal":        {"throughput", 0, "proven for useful output"},
		"traffic sharing prefixes": {lab.ObjectiveUsefulOutput, 0.31, metrics.MeasurementPrefixCacheHitRate},
	} {
		_, planned := oneEnginePlan(t, test.objective, test.hitRate)
		if planned == nil || len(planned.CandidateIDs) != 0 || !strings.Contains(planned.Skipped, test.says) {
			t.Errorf("%s: planned %+v", name, planned)
		}
	}
}

// At the product's default bound, with every rule the map's 128 cell fires
// (cache saturation, speculation capacity, the bands), the one-engine setup
// is kept; under a bound the setups ranked higher fill, the plan says the
// limit cut it.
func TestTheOneEngineSetupFitsTheDefaultBoundOrThePlanSaysTheLimitCutIt(t *testing.T) {
	withImage(t, qualificationImage)
	proposal, planned := oneEnginePlanWithin(t, lab.ObjectiveUsefulOutput, 0, 0)
	if proposal.Input.MaxCandidates != 10 || planned == nil || len(planned.CandidateIDs) != 1 || planned.Skipped != "" {
		t.Fatalf("default bound %d: planned %+v", proposal.Input.MaxCandidates, planned)
	}
	ahead := 0
	for _, candidate := range proposal.Candidates {
		if candidate.ID == planned.CandidateIDs[0] {
			break
		}
		ahead++
	}
	_, cut := oneEnginePlanWithin(t, lab.ObjectiveUsefulOutput, 0, ahead)
	if cut == nil || len(cut.CandidateIDs) != 0 || !strings.Contains(cut.Skipped, "limit of") {
		t.Fatalf("bound %d: planned %+v", ahead, cut)
	}
}

// The technique is lab-proven, so its disclosures describe the H100 lab runs
// in words and quote no figure from them; each of the four still says what it
// discloses.
func TestTheDisclosuresQuoteNoLabFigure(t *testing.T) {
	figure := regexp.MustCompile(`[+\-\x{2212}]?\d+\.\d+%|[+\-\x{2212}]\d+%|\d,\d{3}`)
	disclosures := AdaptiveSpeculationOneEngine{}.Manifest().Disclosures
	for _, disclosure := range disclosures {
		if found := figure.FindString(disclosure); found != "" {
			t.Errorf("disclosure %q quotes the lab figure %q", disclosure, found)
		}
	}
	for index, says := range []string{"All output, not only output within one second of first token, was not shown to improve",
		"At 128 requests at once it served less output within one second of first token than the plain vLLM command",
		"prefix caching off", "the product runs patch v4 and vLLM's Python front end"} {
		if index >= len(disclosures) || !strings.Contains(disclosures[index], says) {
			t.Errorf("disclosure %d does not say %q: %v", index+1, says, disclosures)
		}
	}
}
