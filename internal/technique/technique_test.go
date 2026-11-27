package technique

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func trainedDraft() Manifest {
	return Manifest{
		SchemaVersion: SchemaV1, ID: "trained-draft", Version: "0.1.0", Label: "A draft trained on your traffic", License: "Apache-2.0",
		Requires: Requires{Engines: []Engine{{Name: "vllm", Versions: []string{"0.34.0"}}}, Accelerators: []string{"L4", "H100"}},
		Relieves: []Limit{LimitDecodeSteps}, Costs: []Limit{LimitKVCacheRoom, LimitBuildTime},
		Prediction: Method{Metric: "output_tokens_per_second", Basis: "acceptance-offline"},
		Artifact: &Build{Kind: "speculator_checkpoint", RunsIn: RunsInCustomerAccount, UsesCustomerContent: true,
			Produces: "a draft checkpoint pinned to the model revision, engine version, and GPU"},
		ChangesAnswers: true, Runtime: Runtime{Policy: RuntimeStatic},
		Fallback: Fallback{Serves: FallbackStartingSetup, Reason: "without the draft the starting setup serves unchanged"},
	}
}

const suffixFile = `{
  "schema_version": "infercrane.dev/technique-static/v1",
  "manifest": {
    "schema_version": "infercrane.dev/technique/v1",
    "id": "suffix-decoding",
    "version": "0.1.0",
    "label": "Suffix decoding with adaptive verification",
    "license": "Apache-2.0",
    "requires": {"engines": [{"name": "vllm", "versions": ["0.34.0"]}], "accelerators": ["L4", "H100"]},
    "relieves": ["decode-steps"],
    "costs": ["first-token"],
    "prediction": {"metric": "output_tokens_per_second", "basis": "none"},
    "changes_answers": true,
    "runtime": {"policy": "static"},
    "fallback": {"serves": "engine-default", "reason": "without the flag the engine decodes one token a step"}
  },
  "changes": [
    {"label": "Suffix decoding", "reason": "Repeated text in agentic and code traffic drafts itself.",
     "set": [{"name": "--speculative-config", "value": "{\"method\":\"suffix\",\"enable_adaptive_verification\":true}"}]}
  ]
}`

func schema(t *testing.T, file string) *jsonschema.Resolved {
	t.Helper()
	body, err := os.ReadFile("../../schemas/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var parsed jsonschema.Schema
	if err = json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	resolved, err := parsed.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func instance(t *testing.T, value any) any {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err = json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The published schema and the Go validation agree on what a manifest is.
func TestManifestValidationMatchesThePublishedSchema(t *testing.T) {
	published := schema(t, "technique-v1.schema.json")
	valid := trainedDraft()
	if err := valid.Validate(); err != nil {
		t.Fatalf("Go refused a valid manifest: %v", err)
	}
	if err := published.Validate(instance(t, valid)); err != nil {
		t.Fatalf("the schema refused a valid manifest: %v", err)
	}
	declared := trainedDraft()
	declared.Evidence = []string{"draft-acceptance"}
	if err := declared.Validate(); err != nil {
		t.Fatalf("Go refused declared evidence: %v", err)
	}
	if err := published.Validate(instance(t, declared)); err != nil {
		t.Fatalf("the schema refused declared evidence: %v", err)
	}
	ceiled := trainedDraft()
	ceiled.Requires.SignalsAtMost = map[string]float64{"runtime_prefix_cache_hit_rate": 0.02}
	if err := ceiled.Validate(); err != nil {
		t.Fatalf("Go refused a ceiling on a reading: %v", err)
	}
	if err := published.Validate(instance(t, ceiled)); err != nil {
		t.Fatalf("the schema refused a ceiling on a reading: %v", err)
	}
	// Every limit Go knows is one the schema takes, host memory included.
	for _, limit := range limits {
		every := trainedDraft()
		every.Costs = append(every.Costs, limit)
		if err := every.Validate(); err != nil {
			t.Fatalf("Go refused the limit %s: %v", limit, err)
		}
		if err := published.Validate(instance(t, every)); err != nil {
			t.Fatalf("the schema refused the limit %s: %v", limit, err)
		}
	}
	if !slices.Contains(limits, LimitHostMemory) {
		t.Fatal("host memory is not a limit a manifest may name")
	}
	for name, broken := range map[string]func(*Manifest){
		"customer content off the customer's account": func(m *Manifest) { m.Artifact.RunsIn = RunsInDisposableContainer },
		"an artifact that costs no build time":        func(m *Manifest) { m.Costs = []Limit{LimitKVCacheRoom} },
		"no engines":                                  func(m *Manifest) { m.Requires.Engines = nil },
		"a version range":                             func(m *Manifest) { m.Requires.Engines[0].Versions = []string{">=0.34"} },
		"no accelerators":                             func(m *Manifest) { m.Requires.Accelerators = nil },
		"an unknown limit":                            func(m *Manifest) { m.Relieves = []Limit{"speed"} },
		"an unknown runtime policy":                   func(m *Manifest) { m.Runtime.Policy = "sometimes" },
		"no fallback reason":                          func(m *Manifest) { m.Fallback.Reason = "" },
		"an uppercase id":                             func(m *Manifest) { m.ID = "Trained-Draft" },
		"evidence named twice":                        func(m *Manifest) { m.Evidence = []string{"draft-acceptance", "draft-acceptance"} },
		"evidence not a token":                        func(m *Manifest) { m.Evidence = []string{"Draft Acceptance"} },
		"a negative ceiling": func(m *Manifest) {
			m.Requires.SignalsAtMost = map[string]float64{"runtime_prefix_cache_hit_rate": -0.1}
		},
		"a ceiling on a name that is not a token": func(m *Manifest) {
			m.Requires.SignalsAtMost = map[string]float64{"Prefix Hit Rate": 0.02}
		},
		"ceilings on more than eight readings": func(m *Manifest) {
			m.Requires.SignalsAtMost = map[string]float64{}
			for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
				m.Requires.SignalsAtMost[name] = 1
			}
		},
	} {
		manifest := trainedDraft()
		broken(&manifest)
		if manifest.Validate() == nil {
			t.Errorf("%s: Go accepted it", name)
		}
		if published.Validate(instance(t, manifest)) == nil {
			t.Errorf("%s: the schema accepted it", name)
		}
	}
	var static any
	if err := json.Unmarshal([]byte(suffixFile), &static); err != nil {
		t.Fatal(err)
	}
	if err := published.Validate(static); err != nil {
		t.Fatalf("the schema refused a valid static technique: %v", err)
	}
}

func TestAdmitsNamesWhatIsMissing(t *testing.T) {
	manifest := trainedDraft()
	manifest.Requires.ModelFeatures = []string{"native_mtp"}
	setup := Setup{Engine: "vllm", EngineVersion: "0.34.0", Accelerator: "L4", ModelFeatures: []string{"native_mtp"}}
	if ok, why := manifest.Admits(setup); !ok {
		t.Fatalf("refused: %s", why)
	}
	for name, change := range map[string]func(*Setup){
		"sglang":       func(s *Setup) { s.Engine = "sglang" },
		"another vLLM": func(s *Setup) { s.EngineVersion = "0.22.1" },
		"another GPU":  func(s *Setup) { s.Accelerator = "A10G" },
		"no MTP head":  func(s *Setup) { s.ModelFeatures = nil },
	} {
		changed := setup
		change(&changed)
		if ok, why := manifest.Admits(changed); ok || why == "" {
			t.Errorf("%s: admitted (%q)", name, why)
		}
	}
}

// A technique that serves with a cache off is never applied to traffic that
// uses the cache, nor to traffic nobody measured.
func TestAdmitsRefusesTrafficAboveACeilingOrUnread(t *testing.T) {
	manifest := trainedDraft()
	manifest.Requires.SignalsAtMost = map[string]float64{"runtime_prefix_cache_hit_rate": 0.02}
	setup := Setup{Engine: "vllm", EngineVersion: "0.34.0", Accelerator: "H100", Signals: map[string]float64{"runtime_prefix_cache_hit_rate": 0.01}}
	if ok, why := manifest.Admits(setup); !ok {
		t.Fatalf("refused traffic under the ceiling: %s", why)
	}
	setup.Signals = map[string]float64{"runtime_prefix_cache_hit_rate": 0.02}
	if ok, why := manifest.Admits(setup); !ok {
		t.Fatalf("refused traffic at the ceiling: %s", why)
	}
	for name, signals := range map[string]map[string]float64{
		"shared prefixes": {"runtime_prefix_cache_hit_rate": 0.31},
		"no reading":      {"runtime_requests_running_per_replica": 40},
		"no signals":      nil,
		"not a number":    {"runtime_prefix_cache_hit_rate": math.NaN()},
	} {
		changed := setup
		changed.Signals = signals
		ok, why := manifest.Admits(changed)
		if ok || !strings.Contains(why, "runtime_prefix_cache_hit_rate") {
			t.Errorf("%s: admitted or not named (%v, %q)", name, ok, why)
		}
	}
}

func TestAStaticTechniqueProposesItsChangesOnTheStartingSetup(t *testing.T) {
	technique, err := LoadStatic([]byte(suffixFile))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(technique)
	if err != nil {
		t.Fatal(err)
	}
	setup := Setup{Engine: "vllm", EngineVersion: "0.34.0", Accelerator: "L4", Args: []string{"--max-num-seqs", "256", "--speculative-config", `{"method":"mtp","num_speculative_tokens":3}`}}
	candidates, skipped, err := registry.Candidates(context.Background(), setup)
	if err != nil || len(candidates) != 1 || len(skipped) != 0 {
		t.Fatalf("candidates=%+v skipped=%+v err=%v", candidates, skipped, err)
	}
	if got := strings.Join(candidates[0].Args, " "); got != `--max-num-seqs 256 --speculative-config {"method":"suffix","enable_adaptive_verification":true}` {
		t.Fatalf("args = %s", got)
	}
	if !candidates[0].ChangesAnswers || candidates[0].TechniqueVersion != "0.1.0" {
		t.Fatalf("candidate = %+v", candidates[0])
	}
	setup.Accelerator = "A10G"
	if candidates, skipped, _ = registry.Candidates(context.Background(), setup); len(candidates) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "A10G") {
		t.Fatalf("on an unchecked GPU: candidates=%+v skipped=%+v", candidates, skipped)
	}
	for name, body := range map[string]string{
		"an artifact":      strings.Replace(suffixFile, `"changes_answers": true,`, `"changes_answers": true, "artifact": {"kind": "speculator_checkpoint", "runs_in": "disposable-gpu-container", "uses_customer_content": false, "produces": "x"},`, 1),
		"an unknown field": strings.Replace(suffixFile, `"changes": [`, `"extra": 1, "changes": [`, 1),
		"not a flag":       strings.Replace(suffixFile, `"name": "--speculative-config"`, `"name": "speculative-config"`, 1),
	} {
		if _, err = LoadStatic([]byte(body)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestApplyFlagsSetsRemovesAndKeepsTheRest(t *testing.T) {
	args := []string{"--tensor-parallel-size", "1", "--enable-auto-tool-choice", "--kv-cache-dtype=auto", "--max-num-seqs", "512"}
	got, changed := ApplyFlags(args, []Flag{{Name: "--kv-cache-dtype", Value: "fp8"}, {Name: "--enforce-eager"}}, []string{"--max-num-seqs"})
	if !changed || strings.Join(got, " ") != "--tensor-parallel-size 1 --enable-auto-tool-choice --kv-cache-dtype=fp8 --enforce-eager" {
		t.Fatalf("got %q changed=%v", got, changed)
	}
	if _, changed = ApplyFlags(args, []Flag{{Name: "--max-num-seqs", Value: "512"}}, nil); changed {
		t.Fatal("setting a flag to its own value changed the setup")
	}
}

// fixed returns the candidates it was given, to check the registry's checks.
type fixed struct {
	manifest   Manifest
	candidates []Candidate
}

func (f fixed) Manifest() Manifest { return f.manifest }
func (f fixed) Candidates(context.Context, Setup) ([]Candidate, string, error) {
	return f.candidates, "nothing to propose", nil
}

func TestTheRegistryHoldsEveryCandidateToItsManifest(t *testing.T) {
	setup := Setup{Engine: "vllm", EngineVersion: "0.34.0", Accelerator: "L4"}
	good := Candidate{Technique: "trained-draft", TechniqueVersion: "0.1.0", Label: "Draft", ChangesAnswers: true, Reason: "trained on 10K pairs",
		Artifact: &ArtifactRef{ID: "artifact-1", Kind: "speculator_checkpoint", Digest: "sha256:" + strings.Repeat("a", 64)}}
	registry, err := NewRegistry(fixed{manifest: trainedDraft(), candidates: []Candidate{good}})
	if err != nil {
		t.Fatal(err)
	}
	if candidates, _, err := registry.Candidates(context.Background(), setup); err != nil || len(candidates) != 1 {
		t.Fatalf("a candidate that keeps its manifest: %v %v", candidates, err)
	}
	adaptive := trainedDraft()
	adaptive.ID, adaptive.Artifact, adaptive.Costs, adaptive.Runtime.Policy = "adaptive-speculation", nil, []Limit{LimitKVCacheRoom}, RuntimeAdaptive
	banded := Candidate{Technique: "adaptive-speculation", TechniqueVersion: "0.1.0", Label: "Drafts by load", ChangesAnswers: true, Reason: "light load gains, heavy load loses",
		Bands: []Band{{UpTo: 32, Args: []string{"--speculative-config", "{}"}}, {UpTo: 0, Args: []string{}}}}
	for name, test := range map[string]struct {
		manifest  Manifest
		candidate func(Candidate) Candidate
	}{
		"another version":      {trainedDraft(), func(c Candidate) Candidate { c.TechniqueVersion = "0.2.0"; return c }},
		"answers flag dropped": {trainedDraft(), func(c Candidate) Candidate { c.ChangesAnswers = false; return c }},
		"no artifact":          {trainedDraft(), func(c Candidate) Candidate { c.Artifact = nil; return c }},
		"an artifact not by digest": {trainedDraft(), func(c Candidate) Candidate {
			c.Artifact = &ArtifactRef{ID: "a", Kind: "speculator_checkpoint"}
			return c
		}},
		"bands on a static one": {trainedDraft(), func(c Candidate) Candidate { c.Bands = banded.Bands; return c }},
		"one band only":         {adaptive, func(Candidate) Candidate { c := banded; c.Bands = c.Bands[1:]; return c }},
		"bands not increasing":  {adaptive, func(Candidate) Candidate { c := banded; c.Bands = []Band{{UpTo: 64}, {UpTo: 32}, {UpTo: 0}}; return c }},
		"no reason":             {trainedDraft(), func(c Candidate) Candidate { c.Reason = ""; return c }},
		"an engine image by tag": {trainedDraft(), func(c Candidate) Candidate {
			c.Image = &RuntimeImage{Image: "vllm/vllm-openai:v0.34.0", Command: []string{"vllm", "serve"}}
			return c
		}},
		"an engine image with no command": {trainedDraft(), func(c Candidate) Candidate {
			c.Image = &RuntimeImage{Image: "infercrane/vllm@sha256:" + strings.Repeat("b", 64)}
			return c
		}},
	} {
		registry, err := NewRegistry(fixed{manifest: test.manifest, candidates: []Candidate{test.candidate(good)}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, _, err = registry.Candidates(context.Background(), setup); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if registry, err = NewRegistry(fixed{manifest: adaptive, candidates: []Candidate{banded}}); err != nil {
		t.Fatal(err)
	}
	if candidates, _, err := registry.Candidates(context.Background(), setup); err != nil || len(candidates) != 1 {
		t.Fatalf("an adaptive candidate with two bands: %v %v", candidates, err)
	}
	if _, err = NewRegistry(fixed{manifest: trainedDraft()}, fixed{manifest: trainedDraft()}); err == nil {
		t.Fatal("a technique registered twice")
	}
}

// A technique that changes the engine names the image by digest and the
// command that starts it.
func TestACandidateMayNameItsEngineImageByDigest(t *testing.T) {
	setup := Setup{Engine: "vllm", EngineVersion: "0.34.0", Accelerator: "L4"}
	candidate := Candidate{Technique: "trained-draft", TechniqueVersion: "0.1.0", Label: "Draft", ChangesAnswers: true, Reason: "trained on 10K pairs",
		Artifact: &ArtifactRef{ID: "artifact-1", Kind: "speculator_checkpoint", Digest: "sha256:" + strings.Repeat("a", 64)},
		Image: &RuntimeImage{Image: "infercrane/vllm-state-by-draft@sha256:" + strings.Repeat("c", 64), Command: []string{"/opt/infercrane/serve-state-by-draft"},
			Identity: "vllm/vllm-openai@sha256:cf5b… + infercrane-state-by-draft-v2"}}
	registry, err := NewRegistry(fixed{manifest: trainedDraft(), candidates: []Candidate{candidate}})
	if err != nil {
		t.Fatal(err)
	}
	if offered, _, err := registry.Candidates(context.Background(), setup); err != nil || len(offered) != 1 || offered[0].Image.Command[0] != "/opt/infercrane/serve-state-by-draft" {
		t.Fatalf("offered %+v, err %v", offered, err)
	}
}

// Disclosures travel with every claim: a record that drops one is not shown,
// and the manifest bounds them.
func TestARecordThatDropsADisclosureIsNotShown(t *testing.T) {
	manifest := trainedDraft()
	manifest.Disclosures = []string{"All output was not shown to improve.", "At 128 at once it served less than the plain command."}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := schema(t, "technique-v1.schema.json").Validate(instance(t, manifest)); err != nil {
		t.Fatalf("the schema refused disclosures: %v", err)
	}
	record := completeResult()
	if manifest.ClaimOf(record) != ClaimNotShown {
		t.Fatal("a record without the disclosures was shown")
	}
	record.Disclosures = slices.Clone(manifest.Disclosures)
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := schema(t, "technique-result-v1.schema.json").Validate(instance(t, record)); err != nil {
		t.Fatalf("the schema refused a record's disclosures: %v", err)
	}
	if claim := manifest.ClaimOf(record); claim != ClaimShown {
		t.Fatalf("a complete record carrying every disclosure was %q, unknown %v", claim, record.Unknown())
	}
	for index := range manifest.Disclosures {
		dropped := record
		dropped.Disclosures = slices.Delete(slices.Clone(record.Disclosures), index, index+1)
		if manifest.ClaimOf(dropped) != ClaimNotShown {
			t.Fatalf("a record that dropped disclosure %d was shown", index)
		}
	}
	for name, broken := range map[string][]string{
		"empty":    {" "},
		"repeated": {"same", "same"},
		"long":     {strings.Repeat("x", 401)},
		"too many": {"1", "2", "3", "4", "5", "6", "7", "8", "9"},
	} {
		changed := trainedDraft()
		changed.Disclosures = broken
		if changed.Validate() == nil {
			t.Errorf("%s: Go accepted it", name)
		}
		if schema(t, "technique-v1.schema.json").Validate(instance(t, changed)) == nil {
			t.Errorf("%s: the schema accepted it", name)
		}
	}
}

func completeResult() Result {
	digest := "sha256:" + strings.Repeat("b", 64)
	return Result{SchemaVersion: ResultSchemaV1, Technique: "trained-draft", TechniqueVersion: "0.1.0", Candidate: "Draft",
		Hardware: Hardware{Accelerator: "L4", Count: 1, Driver: "580.0"}, Software: Software{Engine: "vllm", EngineVersion: "0.34.0", ImageDigest: digest},
		Workload: Workload{ShapeDigest: digest, Kind: "short", Load: 16, Requests: 400}, Precision: Precision{Weights: "bf16", KVCache: "auto"},
		Baseline: Baseline{Setup: "vLLM defaults", Basis: "same-machine+fresh-servers"}, Correctness: Correctness{Method: "task-score+kl", Verdict: "passed"},
		Measured:     Measured{Metric: "useful_output_tokens_per_second", Change: 0.12, NoiseFloor: 0.03, Decision: DecisionImproved},
		Reproduction: Reproduction{Command: "infercrane optimize screen ...", Commit: "0123abc", ImageDigest: digest, TrafficShape: "short-16.json"},
		Evidence:     []EvidenceItem{{Name: EvidenceFreshMachineConfirmation, Digest: "sha256:" + strings.Repeat("d", 64)}}}
}

// A gain from one machine and one run is never shown, whatever the
// technique's manifest declares: the fresh machine's confirmation is
// required centrally, and Unknown names it so a reader sees what is missing.
func TestNoResultIsShownWithoutAFreshMachineConfirmation(t *testing.T) {
	oneMachine := completeResult()
	oneMachine.Baseline.Basis, oneMachine.Evidence = "same-machine", nil
	manifest := trainedDraft()
	manifest.Evidence = nil
	if oneMachine.Claim() != ClaimNotShown || manifest.ClaimOf(oneMachine) != ClaimNotShown || !slices.Equal(oneMachine.Unknown(), []string{"evidence." + EvidenceFreshMachineConfirmation}) {
		t.Fatalf("one machine: claim=%s manifest=%s unknown=%v", oneMachine.Claim(), manifest.ClaimOf(oneMachine), oneMachine.Unknown())
	}
	// A record that lists the requirement itself is named once.
	oneMachine.RequiredEvidence = []string{EvidenceFreshMachineConfirmation}
	if !slices.Equal(oneMachine.Unknown(), []string{"evidence." + EvidenceFreshMachineConfirmation}) {
		t.Fatalf("unknown = %v", oneMachine.Unknown())
	}
	// A basis that says fresh servers is not the confirmation; the record is.
	oneMachine.Baseline.Basis = "same-machine+fresh-servers"
	if oneMachine.Claim() != ClaimNotShown {
		t.Fatal("a basis alone was taken as the fresh machine's confirmation")
	}
	oneMachine.Evidence = []EvidenceItem{{Name: EvidenceFreshMachineConfirmation, Digest: "confirmed"}}
	if oneMachine.Claim() != ClaimNotShown {
		t.Fatal("a confirmation without a sha256 digest was accepted")
	}
	confirmed := completeResult()
	if confirmed.Claim() != ClaimShown || manifest.ClaimOf(confirmed) != ClaimShown || len(confirmed.Unknown()) != 0 {
		t.Fatalf("confirmed: claim=%s manifest=%s unknown=%v", confirmed.Claim(), manifest.ClaimOf(confirmed), confirmed.Unknown())
	}
}

// A gain is "shown" only with all five fields, a way to reproduce it, answers
// that held, and a change beyond the measured noise.
func TestAResultIsShownOnlyWithEveryFieldAndAGainBeyondTheNoise(t *testing.T) {
	published := schema(t, "technique-result-v1.schema.json")
	result := completeResult()
	if err := result.Validate(); err != nil || result.Claim() != ClaimShown || len(result.Unknown()) != 0 {
		t.Fatalf("validate=%v claim=%s unknown=%v", err, result.Claim(), result.Unknown())
	}
	if err := published.Validate(instance(t, result)); err != nil {
		t.Fatalf("the schema refused a complete result: %v", err)
	}
	for name, test := range map[string]struct {
		change  func(*Result)
		unknown string
	}{
		"no image digest":          {func(r *Result) { r.Software.ImageDigest = "" }, "software.image_digest"},
		"precision unknown":        {func(r *Result) { r.Precision.KVCache = "unknown" }, "precision.kv_cache"},
		"no baseline basis":        {func(r *Result) { r.Baseline.Basis = "" }, "baseline.basis"},
		"no traffic shape":         {func(r *Result) { r.Reproduction.TrafficShape = "" }, "reproduction.traffic_shape"},
		"inside the noise":         {func(r *Result) { r.Measured.Change = 0.02 }, ""},
		"a constant noise of 0":    {func(r *Result) { r.Measured.NoiseFloor = 0 }, ""},
		"answers did not hold":     {func(r *Result) { r.Correctness.Verdict = "blocked" }, ""},
		"no measurable difference": {func(r *Result) { r.Measured.Decision = DecisionSame }, ""},
	} {
		changed := completeResult()
		test.change(&changed)
		if changed.Claim() != ClaimNotShown {
			t.Errorf("%s: claim = %s", name, changed.Claim())
		}
		if test.unknown != "" && !strings.Contains(strings.Join(changed.Unknown(), ","), test.unknown) {
			t.Errorf("%s: unknown = %v", name, changed.Unknown())
		}
	}
	result.Measured.Decision = "better"
	if result.Validate() == nil || published.Validate(instance(t, result)) == nil {
		t.Fatal("a decision outside the four was accepted")
	}
}

// A technique that declares evidence is "not shown" until a record carries
// it, judged against the manifest even when the record left the list out.
func TestARecordWithoutTheEvidenceItsTechniqueDeclaresIsNotShown(t *testing.T) {
	published := schema(t, "technique-result-v1.schema.json")
	manifest := trainedDraft()
	manifest.Evidence = []string{"draft-acceptance"}
	bare := completeResult()
	if manifest.ClaimOf(bare) != ClaimNotShown || bare.Claim() != ClaimShown {
		t.Fatalf("a record that left the requirement out: manifest says %s, record says %s", manifest.ClaimOf(bare), bare.Claim())
	}
	copied := completeResult()
	copied.RequiredEvidence = []string{"draft-acceptance"}
	if copied.Claim() != ClaimNotShown || !slices.Contains(copied.Unknown(), "evidence.draft-acceptance") {
		t.Fatalf("claim=%s unknown=%v", copied.Claim(), copied.Unknown())
	}
	copied.Evidence = append(copied.Evidence, EvidenceItem{Name: "draft-acceptance", Digest: "sha256:" + strings.Repeat("c", 64), Summary: "2.1 tokens accepted a step"})
	if err := copied.Validate(); err != nil || copied.Claim() != ClaimShown || manifest.ClaimOf(copied) != ClaimShown {
		t.Fatalf("validate=%v claim=%s manifest=%s", err, copied.Claim(), manifest.ClaimOf(copied))
	}
	if err := published.Validate(instance(t, copied)); err != nil {
		t.Fatalf("the schema refused a record with evidence: %v", err)
	}
	other := manifest
	other.Version = "0.2.0"
	if other.ClaimOf(copied) != ClaimNotShown {
		t.Fatal("a record of another technique version was judged by this one")
	}
	copied.Evidence[len(copied.Evidence)-1].Digest = "not-a-digest"
	if copied.Validate() == nil || published.Validate(instance(t, copied)) == nil || copied.Claim() != ClaimNotShown {
		t.Fatal("evidence without a sha256 digest was accepted")
	}
}
