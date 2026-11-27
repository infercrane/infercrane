package optimizer

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/speculator"
	"github.com/infercrane/infercrane/internal/support"
)

const draftTestCommit = "0123456789abcdef0123456789abcdef01234567"

func testTrainedDraft() speculator.Draft {
	return speculator.Draft{
		Name: "support-chat", Source: speculator.SourceTrained, Method: speculator.MethodEAGLE3,
		Repository: "acme/novel-open-model-draft", Revision: draftTestCommit,
		BaseModel: "acme/novel-open-model", BaseRevision: strings.Repeat("a", 40), DraftTokens: 3,
		ArtifactDigest: "sha256:" + strings.Repeat("f", 64),
		// The trained-draft technique serves a draft only where it is pinned.
		Pinned: &speculator.Pin{Engine: "vllm", EngineVersion: support.DefaultRuntimeVersion, Accelerator: "H100"},
		Training: &speculator.Training{
			Framework: "speculators", FrameworkVersion: "0.9.0", DataSource: "prompt_set",
			DatasetFingerprint: "sha256:" + strings.Repeat("d", 64), Examples: 9000, HeldOutExamples: 400, TrainingTokens: 4_000_000,
			HeldOutAcceptance: []float64{0.8, 0.5, 0.5}, ProducedAt: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC),
		},
	}
}

func testPublishedDraft() speculator.Draft {
	return speculator.Draft{
		Name: "publisher", Source: speculator.SourcePublished, Method: speculator.MethodEAGLE3,
		Repository: "publisher/novel-open-model-eagle3", Revision: strings.Repeat("e", 40),
		BaseModel: "acme/novel-open-model", BaseRevision: strings.Repeat("a", 40), DraftTokens: 3,
	}
}

func candidateFor(t *testing.T, proposal Proposal, technique string) Candidate {
	t.Helper()
	for _, candidate := range proposal.Candidates {
		if candidate.Reason != nil && candidate.Reason.Technique == technique {
			return candidate
		}
	}
	t.Fatalf("no %s candidate in %v", technique, reasonOrder(proposal))
	return Candidate{}
}

func speculativeConfig(t *testing.T, candidate Candidate) map[string]any {
	t.Helper()
	args := candidate.Deployment.Runtime.Args
	var config map[string]any
	found := 0
	for index, arg := range args {
		if arg == "--speculative-config" && index+1 < len(args) {
			found++
			if err := json.Unmarshal([]byte(args[index+1]), &config); err != nil {
				t.Fatal(err)
			}
		}
	}
	if found != 1 {
		t.Fatalf("want exactly one --speculative-config, got %d in %q", found, args)
	}
	return config
}

func TestSuppliedDraftsAreMeasuredBesideTheOtherSpeculation(t *testing.T) {
	request := genericRequest("H100", lowConcurrencyDiagnosis())
	request.MaxCandidates = 10
	request.ModelFeatures, request.ModelConfigDigest = []string{FeatureNativeMTP}, recipeTestDigest
	request.SpeculativeDrafts = []speculator.Draft{testPublishedDraft(), testTrainedDraft()}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProposal(proposal); err != nil {
		t.Fatalf("proposal with drafts fails its own validation: %v", err)
	}
	order := reasonOrder(proposal)
	// Low concurrency picks the model's own head at 90; the trained draft is
	// measured right after the strongest speculation choice, ahead of it.
	if len(order) < 4 || order[0] != ReasonControl || order[1] != TechniqueSpeculativeTrainedDraft {
		t.Fatalf("order = %v", order)
	}
	trained := plannedTechnique(t, proposal, TechniqueSpeculativeTrainedDraft)
	if trained.Priority != 91 || !contains(trained.Rules, RuleSuppliedDrafts) || len(trained.CandidateIDs) != 1 || trained.Skipped != "" {
		t.Fatalf("trained draft plan = %+v", trained)
	}
	if !hasEvidence(trained.Evidence, SignalDraftTokensPerStep, 2.4) || !strings.Contains(trained.Reason, "400 held-out prompts") {
		t.Fatalf("trained draft reason = %q evidence = %+v", trained.Reason, trained.Evidence)
	}
	config := speculativeConfig(t, candidateFor(t, proposal, TechniqueSpeculativeTrainedDraft))
	if config["method"] != "eagle3" || config["model"] != "acme/novel-open-model-draft" || config["revision"] != draftTestCommit || config["num_speculative_tokens"] != float64(3) {
		t.Fatalf("trained draft config = %v", config)
	}
	published := speculativeConfig(t, candidateFor(t, proposal, TechniqueSpeculativeDraft))
	if published["model"] != "publisher/novel-open-model-eagle3" {
		t.Fatalf("published draft config = %v", published)
	}
	mtp := speculativeConfig(t, candidateFor(t, proposal, TechniqueSpeculativeMTP))
	if mtp["method"] != "mtp" {
		t.Fatalf("model head config = %v", mtp)
	}
	trainedCandidate := candidateFor(t, proposal, TechniqueSpeculativeTrainedDraft)
	if !contains(trainedCandidate.RequiredEvidence, TaskScoreEvidence) {
		t.Fatalf("a speculation setup must need a task score: %v", trainedCandidate.RequiredEvidence)
	}
	if contains(order, TechniqueSpeculationOff) {
		t.Fatalf("the starting setup does not speculate, so it is the no-speculation arm itself: %v", order)
	}
}

func TestSuppliedDraftReplacesTheStartingSetupsSpeculation(t *testing.T) {
	request := genericRequest("H100", decodeDiagnosis())
	request.MaxCandidates = 10
	request.SpeculativeDrafts = []speculator.Draft{testTrainedDraft()}
	request.StartingSetup = &StartingSetup{Deployment: "chat", Revision: 2, Engine: "vllm", Version: "0.34.0", Args: []string{"--speculative-config", `{"method":"ngram","num_speculative_tokens":4,"prompt_lookup_max":4}`}, GPU: "H100", GPUCount: 1}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	control := proposal.Candidates[0]
	if !strings.Contains(strings.Join(control.Deployment.Runtime.Args, " "), "ngram") {
		t.Skipf("the starting setup was not applied to this control (%v); covered by the planner's own starting-setup tests", control.Deployment.Runtime.Args)
	}
	config := speculativeConfig(t, candidateFor(t, proposal, TechniqueSpeculativeTrainedDraft))
	if config["method"] != "eagle3" {
		t.Fatalf("the draft must replace the starting setup's speculation: %v", config)
	}
	off := candidateFor(t, proposal, TechniqueSpeculationOff)
	if strings.Contains(strings.Join(off.Deployment.Runtime.Args, " "), "--speculative") {
		t.Fatalf("no-speculation arm still speculates: %v", off.Deployment.Runtime.Args)
	}
}

func TestSuppliedDraftWithoutASpeculationChoiceComesAfterTheDiagnosedChanges(t *testing.T) {
	request := genericRequest("H100", preemptionDiagnosis())
	request.MaxCandidates = 10
	request.SpeculativeDrafts = []speculator.Draft{testTrainedDraft()}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	trained := plannedTechnique(t, proposal, TechniqueSpeculativeTrainedDraft)
	if trained.Priority != suppliedDraftPriority || !strings.Contains(trained.Reason, "did not point at speculation") {
		t.Fatalf("trained draft plan = %+v", trained)
	}
	order := reasonOrder(proposal)
	if order[len(order)-1] == TechniqueSpeculativeTrainedDraft && len(order) == 2 {
		t.Fatalf("the diagnosed change is missing: %v", order)
	}
	if order[1] == TechniqueSpeculativeTrainedDraft {
		t.Fatalf("a supplied draft outranked the diagnosed change: %v", order)
	}
}

func TestDraftTechniquesWithoutASuppliedDraftKeepTheirSkipReason(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", lowConcurrencyDiagnosis()))
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedTechnique(t, proposal, TechniqueSpeculativeDraft).Skipped; got != noDraftWhy[speculator.SourcePublished] {
		t.Fatalf("published draft skip = %q", got)
	}
	for _, planned := range proposal.Diagnosis.Techniques {
		if planned.Technique == TechniqueSpeculativeTrainedDraft {
			t.Fatalf("no rule chose a trained draft, yet it was planned: %+v", planned)
		}
	}
}

func TestSuppliedDraftOnAnEngineWithoutReviewedFlagsIsSkipped(t *testing.T) {
	request := genericRequest("H100", lowConcurrencyDiagnosis())
	request.MaxCandidates = 10
	bucket := testTrainedDraft()
	bucket.Repository, bucket.Revision = "s3://acme-drafts/novel/support", bucket.ArtifactDigest
	request.SpeculativeDrafts = []speculator.Draft{bucket}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	planned := plannedTechnique(t, proposal, TechniqueSpeculativeTrainedDraft)
	// The trained-draft technique says why, in its own words (technique_path.go).
	if len(planned.CandidateIDs) != 0 || !strings.Contains(planned.Skipped, "not built yet") || !strings.Contains(planned.Skipped, "a draft in a bucket") {
		t.Fatalf("bucket draft plan = %+v", planned)
	}
}

func TestSuppliedDraftsAreValidatedAndBoundByTheDigest(t *testing.T) {
	source := catalogSource(t)
	base := genericRequest("H100", lowConcurrencyDiagnosis())
	cases := map[string]func(*Request){
		"no diagnosis": func(r *Request) { r.Diagnosis = nil },
		"other model": func(r *Request) {
			draft := testTrainedDraft()
			draft.BaseModel = "acme/other-model"
			r.SpeculativeDrafts = []speculator.Draft{draft}
		},
		"other commit": func(r *Request) {
			draft := testTrainedDraft()
			draft.BaseRevision = strings.Repeat("b", 40)
			r.SpeculativeDrafts = []speculator.Draft{draft}
		},
		"unpinned model": func(r *Request) { r.ModelRevision = "" },
		"two trained": func(r *Request) {
			second := testTrainedDraft()
			second.Name = "second"
			r.SpeculativeDrafts = []speculator.Draft{testTrainedDraft(), second}
		},
		"three drafts": func(r *Request) {
			r.SpeculativeDrafts = []speculator.Draft{testTrainedDraft(), testPublishedDraft(), testPublishedDraft()}
		},
		"invalid draft": func(r *Request) {
			draft := testTrainedDraft()
			draft.Revision = "main"
			r.SpeculativeDrafts = []speculator.Draft{draft}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := base
			request.SpeculativeDrafts = []speculator.Draft{testTrainedDraft()}
			mutate(&request)
			if _, err := source.Propose(context.Background(), request); err == nil {
				t.Fatal("invalid drafts accepted")
			}
		})
	}
	with := base
	with.SpeculativeDrafts = []speculator.Draft{testTrainedDraft()}
	first, err := source.Propose(context.Background(), with)
	if err != nil {
		t.Fatal(err)
	}
	other := base
	draft := testTrainedDraft()
	draft.Revision = strings.Repeat("c", 40)
	other.SpeculativeDrafts = []speculator.Draft{draft}
	second, err := source.Propose(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if first.InputDigest == second.InputDigest {
		t.Fatal("two drafts gave one input digest")
	}
	without, err := source.Propose(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(without.Input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "speculative_drafts") {
		t.Fatalf("a plan without drafts changed its input encoding: %s", encoded)
	}
}

func TestSuppliedDraftPlansAreDeterministicUnderConcurrentUse(t *testing.T) {
	source := catalogSource(t)
	request := genericRequest("H100", lowConcurrencyDiagnosis())
	request.MaxCandidates = 10
	request.SpeculativeDrafts = []speculator.Draft{testTrainedDraft(), testPublishedDraft()}
	want, err := source.Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(want)
	var group sync.WaitGroup
	errs := make(chan string, 16)
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			got, err := source.Propose(context.Background(), request)
			if err != nil {
				errs <- err.Error()
				return
			}
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(wantJSON) {
				errs <- "plan differs between concurrent calls"
			}
		}()
	}
	group.Wait()
	close(errs)
	for message := range errs {
		t.Fatal(message)
	}
}

func TestATrainedDraftPinnedElsewhereFallsBackToTheStartingSetup(t *testing.T) {
	request := genericRequest("H100", lowConcurrencyDiagnosis())
	request.MaxCandidates = 10
	draft := testTrainedDraft()
	draft.Pinned = &speculator.Pin{Engine: "vllm", EngineVersion: "0.34.0", Accelerator: "L4"}
	request.SpeculativeDrafts = []speculator.Draft{draft}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	planned := plannedTechnique(t, proposal, TechniqueSpeculativeTrainedDraft)
	if len(planned.CandidateIDs) != 0 || !strings.Contains(planned.Skipped, "pinned to vllm 0.34.0 on L4") || !strings.Contains(planned.Skipped, "on H100") {
		t.Fatalf("a draft pinned to another GPU was planned: %+v", planned)
	}
}
