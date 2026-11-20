package continualoptimizer

import (
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/support"
)

func TestEngineReleaseFiresOnceForANewPinnedRelease(t *testing.T) {
	setup := Setup{Model: "Qwen/Qwen3-8B", Runtime: "vllm", RuntimeVersion: "0.21.0", GPU: "H100"}
	trigger, fired := DetectEngineRelease(setup, Observed{})
	if !fired || trigger.Kind != TriggerEngineRelease || trigger.Key != "vllm@"+support.DefaultRuntimeVersion || !strings.Contains(trigger.Detail, "runs 0.21.0") {
		t.Fatalf("trigger=%+v fired=%v", trigger, fired)
	}
	if _, again := DetectEngineRelease(setup, Observed{EngineRelease: trigger.Key}); again {
		t.Fatal("a handled release fired again")
	}
	setup.RuntimeVersion = "v" + support.DefaultRuntimeVersion
	if _, current := DetectEngineRelease(setup, Observed{}); current {
		t.Fatal("an endpoint on the pinned release fired")
	}
	if _, unknown := DetectEngineRelease(Setup{Runtime: "vllm", GPU: "H100"}, Observed{}); unknown {
		t.Fatal("an unknown running release must not fire")
	}
	pinned, _ := PinnedEngine(Setup{Runtime: "vllm", GPU: "H100"})
	if _, sameImage := DetectEngineRelease(Setup{Runtime: "vllm", GPU: "H100", RuntimeImage: pinned.Image}, Observed{}); sameImage {
		t.Fatal("the pinned image fired")
	}
}

func TestModelRevisionAndPriceTriggersNeedKnownValues(t *testing.T) {
	setup := Setup{Model: "Qwen/Qwen3-8B", ModelRevision: strings.Repeat("a", 40)}
	trigger, fired := DetectModelRevision(setup, strings.Repeat("b", 40), Observed{})
	if !fired || trigger.Key != "Qwen/Qwen3-8B@"+strings.Repeat("b", 40) {
		t.Fatalf("trigger=%+v", trigger)
	}
	if _, again := DetectModelRevision(setup, strings.Repeat("b", 40), Observed{ModelRevision: trigger.Key}); again {
		t.Fatal("a handled revision fired again")
	}
	if _, unknown := DetectModelRevision(setup, "", Observed{}); unknown {
		t.Fatal("an unknown newest revision fired")
	}
	if _, unpinned := DetectModelRevision(Setup{Model: "Qwen/Qwen3-8B"}, strings.Repeat("b", 40), Observed{}); unpinned {
		t.Fatal("an unpinned model fired")
	}
	if trigger, fired = DetectPriceChange(2.0, 1.5); !fired || !strings.Contains(trigger.Detail, "-25%") {
		t.Fatalf("price trigger=%+v", trigger)
	}
	for _, pair := range [][2]float64{{2, 2.1}, {0, 2}, {2, 0}} {
		if _, moved := DetectPriceChange(pair[0], pair[1]); moved {
			t.Fatalf("price %v fired", pair)
		}
	}
}

func TestScheduleIsWeeklyByDefault(t *testing.T) {
	policy := DefaultPolicy()
	if policy.CheckInterval() != 7*24*time.Hour || !ScheduleDue(Observed{}, policy, testNow) {
		t.Fatal("a never-checked endpoint is due weekly")
	}
	last := testNow.Add(-6 * 24 * time.Hour)
	if ScheduleDue(Observed{ScheduledAt: &last}, policy, testNow) {
		t.Fatal("checked six days ago is not due")
	}
	last = testNow.Add(-7 * 24 * time.Hour)
	if !ScheduleDue(Observed{ScheduledAt: &last}, policy, testNow) {
		t.Fatal("checked a week ago is due")
	}
	if trigger := ScheduleTrigger(policy, testNow); trigger.Kind != TriggerSchedule || !strings.Contains(trigger.Detail, "every week") {
		t.Fatalf("trigger=%+v", trigger)
	}
}

func TestAnEngineReleaseProposesARunWithUnknownGainAndCost(t *testing.T) {
	baseline := workload(SourceCustomerObserved, 1000)
	input := Input{
		Now: testNow, Policy: DefaultPolicy(), Baseline: &baseline, Current: workload(SourceCustomerObserved, 1000),
		// No availability, cost, capacity, or margin: all unknown.
		ActiveService: ServiceEvidence{RequestCount: 1000, ErrorRate: .001},
		Hypotheses:    []Hypothesis{hypothesis("a", "engine", 0, 0)},
		Triggers:      []Trigger{{Kind: TriggerEngineRelease, Key: "vllm@0.23.0", Detail: "vllm 0.23.0 is the new pinned engine release; this endpoint runs 0.22.1"}},
	}
	decision, err := Evaluate(input)
	if err != nil || decision.Action != ActionStartExperiment || len(decision.Selected) != 1 || len(decision.Triggers) != 1 || decision.Triggers[0].Kind != TriggerEngineRelease {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if strings.Join(decision.UnknownEvidence, ",") != "availability,cost_per_million_output_tokens,qualified_output_capacity,contribution_margin" {
		t.Fatalf("unknown=%v", decision.UnknownEvidence)
	}
	if !strings.Contains(decision.Selected[0].Reasons[0], "unknown until measured") {
		t.Fatalf("reasons=%v", decision.Selected[0].Reasons)
	}

	// The same unknown evidence with no trigger changes nothing.
	input.Triggers = nil
	decision, err = Evaluate(input)
	if err != nil || decision.Action != ActionRetainCurrent || len(decision.Triggers) != 0 {
		t.Fatalf("unknown evidence alone started a run: %+v err=%v", decision, err)
	}

	// A proposal already waiting blocks a second one.
	input.Triggers = []Trigger{ScheduleTrigger(input.Policy, testNow)}
	input.PendingProposals = []Active{{CampaignID: "run-1", Phase: "awaiting_approval", Count: 2}}
	decision, err = Evaluate(input)
	if err != nil || decision.Action != ActionObserveExperiment || decision.Reasons[0] != "a proposed run is already waiting for approval" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}

	// Nothing left to try retains the setup instead of waiting forever.
	input.PendingProposals = nil
	input.History = []Outcome{{Fingerprint: input.Hypotheses[0].Fingerprint, Decision: "rejected", CompletedAt: testNow.Add(-time.Hour)}}
	decision, err = Evaluate(input)
	if err != nil || decision.Action != ActionRetainCurrent || !strings.Contains(strings.Join(decision.Reasons, " "), "no setup is left to try") {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestMeasuredBreachTriggersButUnknownMarginDoesNot(t *testing.T) {
	baseline := workload(SourceCustomerObserved, 1000)
	input := Input{Now: testNow, Policy: DefaultPolicy(), Baseline: &baseline, Current: workload(SourceCustomerObserved, 1000), ActiveService: ServiceEvidence{RequestCount: 1000, ErrorRate: .05}, Hypotheses: []Hypothesis{hypothesis("a", "engine", 0, 0)}}
	decision, err := Evaluate(input)
	if err != nil || decision.Action != ActionStartExperiment || decision.Triggers[0].Kind != TriggerServiceLevel {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	for _, reason := range decision.Reasons {
		if strings.Contains(reason, "margin") || strings.Contains(reason, "availability") {
			t.Fatalf("unknown evidence reported as a breach: %v", decision.Reasons)
		}
	}
}

func TestDriftRebasesTheBaseline(t *testing.T) {
	baseline := workload(SourceCustomerObserved, 1000)
	baseline.InputTokensMean, baseline.OutputTokensMean = 400, 64
	input := Input{Now: testNow, Policy: DefaultPolicy(), Baseline: &baseline, Current: workload(SourceCustomerObserved, 1000), ActiveService: ServiceEvidence{RequestCount: 1000}}
	decision, err := Evaluate(input)
	if err != nil || !decision.BaselineReplacement || decision.Triggers[0].Kind != TriggerTrafficDrift {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestPolicyBoundsAutoRunAndInterval(t *testing.T) {
	policy := DefaultPolicy()
	if policy.AutoRun || policy.MonthlyAutoRunCapUSD != 0 || ValidatePolicy(policy) != nil {
		t.Fatalf("default policy must keep auto-run off: %+v", policy)
	}
	policy.AutoRun = true
	if ValidatePolicy(policy) == nil {
		t.Fatal("auto-run without a monthly cap must be refused")
	}
	policy.MonthlyAutoRunCapUSD = 250
	if ValidatePolicy(policy) != nil {
		t.Fatal("auto-run under a monthly cap is valid")
	}
	policy.CheckIntervalSeconds = 60
	if ValidatePolicy(policy) == nil {
		t.Fatal("a check every minute must be refused")
	}
	input := Input{Now: testNow, Policy: DefaultPolicy(), Current: workload(SourceCustomerObserved, 1000), Triggers: []Trigger{{Kind: TriggerTrafficDrift, Detail: "drift"}}}
	if _, err := Evaluate(input); err == nil {
		t.Fatal("Evaluate detects drift itself; an external drift trigger must be refused")
	}
}

func TestNarrowProposalKeepsControlAndSelection(t *testing.T) {
	ids := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	proposal := optimizer.Proposal{StartingPoint: &optimizer.StartingPoint{CandidateID: ids[0]}}
	for index, id := range ids {
		proposal.Candidates = append(proposal.Candidates, optimizer.Candidate{ID: id, Rank: index + 1, ConfigurationProfile: "profile-" + id[:1]})
	}
	hypotheses := HypothesesFromProposal(proposal, map[string]float64{ids[2]: 12})
	if len(hypotheses) != 2 || hypotheses[0].Fingerprint != "sha256:"+ids[1] || hypotheses[1].EstimatedCostUSD != 12 || hypotheses[0].EstimatedCostUSD != 0 {
		t.Fatalf("hypotheses=%+v", hypotheses)
	}
	narrowed, err := NarrowProposal(proposal, []RankedHypothesis{{Hypothesis: hypotheses[1]}})
	if err != nil || len(narrowed.Candidates) != 2 || narrowed.Candidates[0].ID != ids[0] || narrowed.Candidates[1].ID != ids[2] || narrowed.Candidates[1].Rank != 2 {
		t.Fatalf("narrowed=%+v err=%v", narrowed.Candidates, err)
	}
	if _, err = NarrowProposal(proposal, nil); err == nil {
		t.Fatal("a run of only the control must be refused")
	}
}

func TestRunCostAndExpectedChangeStayUnknownWithoutEvidence(t *testing.T) {
	estimate := EstimateRunCost(2, map[string]float64{"a": 2.5}, []string{"a", "b"})
	if estimate.USD != nil || estimate.Basis != "unknown" {
		t.Fatalf("estimate=%+v", estimate)
	}
	estimate = EstimateRunCost(2, map[string]float64{"a": 2.5, "b": 1.25}, []string{"a", "b"})
	// Approval splits the cap evenly, so each share covers the dearest setup.
	if estimate.USD == nil || *estimate.USD != 5 || estimate.Basis != "current_prices" {
		t.Fatalf("estimate=%+v", estimate)
	}
	change := ExpectedChangeFrom(nil)
	if change.Basis != "unknown" || change.Low != nil {
		t.Fatalf("change=%+v", change)
	}
	change = ExpectedChangeFrom([]PastRunChange{{Change: -.3}, {Change: -.05}})
	if change.Basis != "past_runs" || *change.Low != -.3 || *change.High != -.05 || change.Runs != 2 {
		t.Fatalf("change=%+v", change)
	}
	if reason := ProposalReason([]Trigger{{Kind: TriggerEngineRelease, Detail: "vllm 0.23.0 is out"}}, 1); reason != "Stay optimized proposes measuring 1 new setup against your current one because vllm 0.23.0 is out." {
		t.Fatalf("reason=%q", reason)
	}
}

func TestKernelUpkeepSwitchesOffOrRequalifies(t *testing.T) {
	pinned, _ := PinnedEngine(Setup{Runtime: "vllm", GPU: "H100"})
	kernels := []domain.EndpointCustomKernel{
		{ID: "serving-old", Engine: "vllm", EngineVersion: "0.21.0", State: "serving", Operator: "rmsnorm"},
		{ID: "qualified-old", Engine: "vllm", EngineVersion: "0.21.0", State: "qualified", Operator: "rmsnorm"},
		{ID: "qualified-pinned", Engine: "vllm", EngineVersion: pinned.EngineVersion, State: "qualified", Operator: "rmsnorm"},
		{ID: "already-off", Engine: "vllm", EngineVersion: "0.20.0", State: "off", Operator: "rmsnorm"},
		{ID: "other-engine", Engine: "sglang", EngineVersion: "0.1.0", State: "serving", Operator: "rmsnorm"},
	}
	// The endpoint was rolled out to the pinned release.
	changes := KernelUpkeepFor(kernels, Setup{Runtime: "vllm", RuntimeVersion: pinned.EngineVersion, GPU: "H100"})
	got := map[string]string{}
	for _, change := range changes {
		got[change.Kernel.ID] = change.Upkeep.State
	}
	if len(changes) != 2 || got["serving-old"] != "off" || got["qualified-old"] != "requalification_needed" {
		t.Fatalf("changes=%+v", changes)
	}
	// Still on the old release while a new one is pinned: the serving kernel
	// keeps serving with a warning.
	changes = KernelUpkeepFor(kernels[:1], Setup{Runtime: "vllm", RuntimeVersion: "0.21.0", GPU: "H100"})
	if len(changes) != 1 || changes[0].Change || changes[0].Upkeep.State != "serving" {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestPlanningIssueIsNotNothingLeftToTry(t *testing.T) {
	baseline := workload(SourceCustomerObserved, 1000)
	input := Input{Now: testNow, Policy: DefaultPolicy(), Baseline: &baseline, Current: workload(SourceCustomerObserved, 1000), Triggers: []Trigger{ScheduleTrigger(DefaultPolicy(), testNow)}, PlanningIssue: "the compute connection is not verified"}
	decision, err := Evaluate(input)
	if err != nil || decision.Action != ActionWaitForEvidence || !strings.Contains(strings.Join(decision.Reasons, " "), "could not be planned") {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}
