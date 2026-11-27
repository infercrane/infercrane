package optimizationreport

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/support"
	"github.com/infercrane/infercrane/internal/technique"
)

const setDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// techniqueCampaign is a run whose second setup kernel tuning made, measured
// as given (nil: not measured yet).
func techniqueCampaign(t *testing.T, measured *optimizationcampaign.MeasuredEvidence) (domain.OptimizationCampaign, domain.OptimizationCandidateRun, domain.OptimizationCandidateRun) {
	t.Helper()
	proposal := optimizer.Proposal{Input: optimizer.Request{ModelIdentity: fixtureModel, Objective: "throughput", WorkloadProfile: "throughput",
		WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: "sha256:" + traceDigest},
		Candidates: []optimizer.Candidate{{ID: "proposal-control"}, {ID: "proposal-kernels", Technique: &optimizer.CandidateTechnique{ID: kerneltuning.TechniqueID, Version: kerneltuning.TechniqueVersion,
			Artifact: &technique.ArtifactRef{ID: "kernel-configs-aaaa", Kind: "kernel_configs", Digest: setDigest}}}}}
	encoded, _ := json.Marshal(proposal)
	campaign := domain.OptimizationCampaign{ID: "campaign-1", Objective: "throughput", ProposalJSON: string(encoded)}
	var draft optimizer.DeploymentDraft
	draft.Name, draft.Model.ID, draft.Model.Revision = "qwen-chat", fixtureModel, fixtureRevision
	draft.Runtime.Engine, draft.Runtime.Version, draft.Runtime.Args = "vllm", support.DefaultRuntimeVersion, []string{"--quantization", "fp8"}
	draft.Resources.GPU, draft.Resources.GPUCount = "H100", 1
	spec, _ := json.Marshal(draft)
	control := domain.OptimizationCandidateRun{ID: "control", ProposalCandidateID: "proposal-control", DeploymentSpecJSON: string(spec)}
	kernels := domain.OptimizationCandidateRun{ID: "kernels", ProposalCandidateID: "proposal-kernels", DeploymentSpecJSON: string(spec)}
	if measured != nil {
		actual, _ := json.Marshal(map[string]any{"workload": json.RawMessage(workloadJSON("customer_replay", measured)), "request_count": 480})
		kernels.ActualEvidenceJSON = string(actual)
	}
	return campaign, control, kernels
}

// confirmed is a measurement whose first window ended firstDecision at
// change first, and whose fresh-server window (when decision is set) ended
// decision at change second.
func confirmed(first float64, firstDecision string, second float64, decision string) *optimizationcampaign.MeasuredEvidence {
	measured := interleaved()
	if decision != "" {
		measured.Confirmation = &optimizationcampaign.ConfirmationEvidence{Comparison: map[string]optimizationcampaign.MetricComparison{
			"output_token_throughput": {HigherIsBetter: true, Delta: second, NoiseFloor: 0.04, Decision: decision}}}
	}
	comparison := measured.Comparison["output_token_throughput"]
	comparison.Delta, comparison.Decision = first, firstDecision
	measured.Comparison["output_token_throughput"] = comparison
	return measured
}

// A setup a technique made shows the manifest's declaration, and before it
// is measured its claim is "not shown" for that reason.
func TestATechniqueSetupShowsItsManifestBeforeItIsMeasured(t *testing.T) {
	campaign, control, kernels := techniqueCampaign(t, nil)
	if view := CandidateTechnique(campaign, control, nil); view != nil {
		t.Fatalf("a setup no technique made shows %+v", view)
	}
	view := CandidateTechnique(campaign, kernels, nil)
	if view == nil || view.ID != kerneltuning.TechniqueID || view.Label == "" || !view.ChangesAnswers || !slices.Equal(view.RequiredEvidence, []string{kerneltuning.EvidenceKernelInvocation}) {
		t.Fatalf("view = %+v", view)
	}
	if view.Artifact == nil || view.Artifact.Kind != "kernel_configs" || view.Artifact.Digest != setDigest || view.Artifact.RunsIn != technique.RunsInDisposableContainer || view.Fallback == nil || view.Fallback.Serves != technique.FallbackEngineDefault {
		t.Fatalf("artifact = %+v fallback = %+v", view.Artifact, view.Fallback)
	}
	if view.Result != nil || view.Claim != technique.ClaimNotShown || !slices.Equal(view.Unknown, []string{unknownMeasurement}) {
		t.Fatalf("claim = %s unknown = %v", view.Claim, view.Unknown)
	}
}

// The result is the campaign's own measurement: a gain counts only when the
// fresh-server window confirmed it, at the smaller change against the
// larger noise; evidence the run did not produce keeps the claim "not
// shown" and names it.
func TestATechniqueResultIsTheCampaignsMeasurementJudgedByItsManifest(t *testing.T) {
	task := &domain.QualityEvidence{ID: "q", Evaluator: qualitycheck.TaskEvaluator, Passed: true}
	campaign, _, kernels := techniqueCampaign(t, confirmed(0.3, optimizationcampaign.DecisionImproved, 0.2, optimizationcampaign.DecisionImproved))
	view := CandidateTechnique(campaign, kernels, task)
	result := view.Result
	if result == nil || result.Measured.Decision != technique.DecisionImproved || result.Measured.Change != 0.2 || result.Measured.NoiseFloor != 0.04 || result.Measured.Metric != "output_token_throughput" {
		t.Fatalf("measured = %+v", result)
	}
	if result.Correctness != (technique.Correctness{Method: "task-score", Verdict: "passed"}) || result.Precision.Weights != "fp8 (at load)" || result.Precision.KVCache != "auto" {
		t.Fatalf("correctness = %+v precision = %+v", result.Correctness, result.Precision)
	}
	if result.Workload.ShapeDigest != "sha256:"+traceDigest || result.Workload.Load != 8 || result.Workload.Requests != 480 || result.Baseline.Basis != "interleaved+fresh-servers" || !strings.HasPrefix(result.Software.ImageDigest, "sha256:") {
		t.Fatalf("workload = %+v baseline = %+v software = %+v", result.Workload, result.Baseline, result.Software)
	}
	// The product records no kernel invocation proof yet, so the claim waits for it.
	if view.Claim != technique.ClaimNotShown || !slices.Contains(view.Unknown, "evidence."+kerneltuning.EvidenceKernelInvocation) {
		t.Fatalf("claim = %s unknown = %v", view.Claim, view.Unknown)
	}

	improved, same, regressed := optimizationcampaign.DecisionImproved, optimizationcampaign.DecisionNoMeasurableDifference, optimizationcampaign.DecisionRegressed
	for name, test := range map[string]struct {
		measured *optimizationcampaign.MeasuredEvidence
		want     string
	}{
		"a first-window gain not measured again":  {confirmed(0.3, improved, 0, ""), technique.DecisionUnknown},
		"a gain the fresh servers did not repeat": {confirmed(0.3, improved, 0.01, same), technique.DecisionSame},
		"a loss": {confirmed(-0.2, regressed, 0, ""), technique.DecisionWorse},
	} {
		campaign, _, kernels := techniqueCampaign(t, test.measured)
		if got := CandidateTechnique(campaign, kernels, task).Result.Measured.Decision; got != test.want {
			t.Errorf("%s: decision = %s, want %s", name, got, test.want)
		}
	}
	if CandidateTechnique(campaign, kernels, nil).Result.Correctness.Verdict != technique.DecisionUnknown {
		t.Fatal("a setup without quality evidence has a verdict")
	}
}

// The report's row and the campaign's candidate show the same view.
func TestTheReportRowCarriesTheSameTechniqueView(t *testing.T) {
	campaign, control, kernels := techniqueCampaign(t, confirmed(0.3, optimizationcampaign.DecisionImproved, 0.2, optimizationcampaign.DecisionImproved))
	campaign.Candidates = []domain.OptimizationCandidateRun{control, kernels}
	report := Assemble(Evidence{Campaign: campaign, Benchmarks: map[string]domain.BenchmarkResult{}, Quality: map[string]domain.QualityEvidence{}})
	var row *CandidateResult
	for index := range report.Candidates {
		if report.Candidates[index].ID == kernels.ID {
			row = &report.Candidates[index]
		}
	}
	direct := CandidateTechnique(campaign, kernels, nil)
	if row == nil || row.Technique == nil || row.Technique.Claim != direct.Claim || !slices.Equal(row.Technique.Unknown, direct.Unknown) {
		t.Fatalf("row = %+v direct = %+v", row, direct)
	}
}

// A campaign whose fresh-server window confirmed the gain carries that
// window as the fresh-machine confirmation, so only the technique's own
// evidence is left and with it the claim reads "shown"; a first-window
// gain names the confirmation it lacks.
func TestACampaignGainIsShownOnlyWithItsFreshServerWindow(t *testing.T) {
	task := &domain.QualityEvidence{ID: "q", Evaluator: qualitycheck.TaskEvaluator, Passed: true}
	manifest, _ := optimizer.TechniqueManifest(kerneltuning.TechniqueID)
	campaign, _, kernels := techniqueCampaign(t, confirmed(0.3, optimizationcampaign.DecisionImproved, 0.2, optimizationcampaign.DecisionImproved))
	view := CandidateTechnique(campaign, kernels, task)
	if slices.Contains(view.Unknown, "evidence."+technique.EvidenceFreshMachineConfirmation) || !slices.Contains(view.Unknown, "evidence."+kerneltuning.EvidenceKernelInvocation) || view.Result.Baseline.Basis != "interleaved+fresh-servers" {
		t.Fatalf("confirmed: unknown = %v basis = %s", view.Unknown, view.Result.Baseline.Basis)
	}
	// A test binary records no build commit; a release build does.
	result := *view.Result
	result.Reproduction.Commit = "0123abc"
	result.Evidence = append(slices.Clone(result.Evidence), technique.EvidenceItem{Name: kerneltuning.EvidenceKernelInvocation, Digest: setDigest})
	if manifest.ClaimOf(result) != technique.ClaimShown {
		t.Fatalf("a confirmed campaign result with its technique's evidence: unknown %v", result.Unknown())
	}

	campaign, _, kernels = techniqueCampaign(t, confirmed(0.3, optimizationcampaign.DecisionImproved, 0, ""))
	view = CandidateTechnique(campaign, kernels, task)
	if view.Claim != technique.ClaimNotShown || !slices.Contains(view.Unknown, "evidence."+technique.EvidenceFreshMachineConfirmation) || view.Result.Baseline.Basis != "interleaved" {
		t.Fatalf("first window only: claim = %s unknown = %v", view.Claim, view.Unknown)
	}

	// An agent's second window on the same node, or on a node it could not
	// name, is not a fresh machine: the claim stays not shown and says why.
	// On another node it counts.
	for _, fresh := range []bool{false, true} {
		measured := confirmed(0.3, optimizationcampaign.DecisionImproved, 0.2, optimizationcampaign.DecisionImproved)
		measured.Confirmation.FreshCandidate = fresh
		campaign, _, kernels = techniqueCampaign(t, measured)
		kernels.ActualEvidenceJSON = measuredByAgent(t, kernels.ActualEvidenceJSON)
		view = CandidateTechnique(campaign, kernels, task)
		missing := slices.Contains(view.Unknown, "evidence."+technique.EvidenceFreshMachineConfirmation)
		if missing == fresh || fresh != (view.Result.Baseline.Basis == "interleaved+fresh-servers") || !fresh && !strings.Contains(view.Result.Baseline.Basis, "second GPU node") || !strings.Contains(view.Result.Baseline.Setup, "declared by the endpoint's operator") {
			t.Fatalf("agent, fresh node %v: unknown = %v basis = %s", fresh, view.Unknown, view.Result.Baseline.Basis)
		}
	}
}

// measuredByAgent marks a candidate's measurement as an agent's.
func measuredByAgent(t *testing.T, actual string) string {
	t.Helper()
	var outer map[string]json.RawMessage
	var workload map[string]any
	if json.Unmarshal([]byte(actual), &outer) != nil || json.Unmarshal(outer["workload"], &workload) != nil {
		t.Fatalf("actual evidence: %s", actual)
	}
	workload["measured_by"], workload["live_setup"] = "agent", optimizationcampaign.LiveSetupDeclared
	outer["workload"], _ = json.Marshal(workload)
	encoded, _ := json.Marshal(outer)
	return string(encoded)
}
