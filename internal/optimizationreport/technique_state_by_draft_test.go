package optimizationreport

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/support"
	"github.com/infercrane/infercrane/internal/technique"
)

var patchedImage = "registry.example/vllm-state-by-draft@sha256:" + strings.Repeat("e", 64)

// patchedMetrics is a run's server metrics from the patched engine: the
// most requests reserving three and no draft tokens at one scrape, and the
// most of the smaller count.
func patchedMetrics(three, none, mixed float64) *benchmark.ServerMetrics {
	gauge := func(most float64) benchmark.ServerMetricSummary {
		return benchmark.ServerMetricSummary{Type: "gauge", Series: 1, Stats: map[string]float64{"avg": most / 2, "max": most}}
	}
	metrics := serverMetrics(0.6, 0.93)
	metrics.Metrics[optimizer.StateByDraftMixedGauge] = gauge(mixed)
	metrics.Metrics[optimizer.StateByDraftReservingGauges[0]] = gauge(none)
	metrics.Metrics[optimizer.StateByDraftReservingGauges[1]] = gauge(0)
	metrics.Metrics[optimizer.StateByDraftReservingGauges[2]] = gauge(three)
	metrics.Metrics[optimizer.StateByDraftReceiptGauge] = benchmark.ServerMetricSummary{Type: "gauge", Series: 1, Stats: map[string]float64{"avg": 1, "min": 1, "max": 1}}
	return metrics
}

// oneEngineMeasured is a confirmed gain whose candidate runs, in both
// windows, carry the given server metrics (by run, cycling).
func oneEngineMeasured(metrics ...*benchmark.ServerMetrics) *optimizationcampaign.MeasuredEvidence {
	measured := confirmed(0.2, optimizationcampaign.DecisionImproved, 0.15, optimizationcampaign.DecisionImproved)
	next := 0
	take := func() *benchmark.ServerMetrics {
		metric := metrics[next%len(metrics)]
		next++
		return metric
	}
	for index := range measured.Runs {
		if measured.Runs[index].Arm == "candidate" {
			measured.Runs[index].ServerMetrics = take()
		}
	}
	for range 2 {
		measured.Confirmation.Runs = append(measured.Confirmation.Runs, optimizationcampaign.RunSummary{Arm: "control", ServerMetrics: serverMetrics(0.4, 0.7)},
			optimizationcampaign.RunSummary{Arm: "candidate", ServerMetrics: take()})
	}
	return measured
}

// oneEngineCampaign is a run whose second setup the one-engine technique
// made, served by the given command on the given image, with the given
// limitations, measured as given.
func oneEngineCampaign(t *testing.T, image, command string, limitations []string, measured *optimizationcampaign.MeasuredEvidence) (domain.OptimizationCampaign, domain.OptimizationCandidateRun) {
	t.Helper()
	proposal := optimizer.Proposal{Input: optimizer.Request{ModelIdentity: fixtureModel, Objective: "throughput", WorkloadProfile: "throughput",
		WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: "sha256:" + traceDigest},
		Candidates: []optimizer.Candidate{{ID: "proposal-control"}, {ID: "proposal-one-engine", Limitations: limitations,
			Technique: &optimizer.CandidateTechnique{ID: optimizer.TechniqueAdaptiveSpeculationOneEngine, Version: optimizer.AdaptiveSpeculationOneEngineVersion}}}}
	encoded, _ := json.Marshal(proposal)
	campaign := domain.OptimizationCampaign{ID: "campaign-1", Objective: "throughput", ProposalJSON: string(encoded)}
	var draft optimizer.DeploymentDraft
	draft.Name, draft.Model.ID, draft.Model.Revision = "qwen-chat", fixtureModel, fixtureRevision
	draft.Runtime.Engine, draft.Runtime.Version, draft.Runtime.Args = "vllm", support.DefaultRuntimeVersion, []string{"--no-enable-prefix-caching"}
	draft.Runtime.Workload = support.VLLMWorkload()
	draft.Runtime.Workload.Image = image
	draft.Runtime.Workload.Command = append([]string{command}, draft.Runtime.Workload.Command[2:]...)
	draft.Resources.GPU, draft.Resources.GPUCount = "H100", 1
	spec, _ := json.Marshal(draft)
	run := domain.OptimizationCandidateRun{ID: "one-engine", ProposalCandidateID: "proposal-one-engine", DeploymentSpecJSON: string(spec)}
	actual, _ := json.Marshal(map[string]any{"workload": json.RawMessage(workloadJSON("customer_replay", measured)), "request_count": 480})
	run.ActualEvidenceJSON = string(actual)
	return campaign, run
}

// The product collects the one-engine technique's evidence itself: the
// deployment served the patched image through the receipt check, and the
// engine's own gauges, which AIPerf scraped in every measured run, show
// requests reserving three draft tokens and fewer running together. With
// the manifest's disclosures shown on the setup, a confirmed gain is
// "shown".
func TestTheProductCollectsTheOneEngineEvidenceItself(t *testing.T) {
	manifest, _ := optimizer.TechniqueManifest(optimizer.TechniqueAdaptiveSpeculationOneEngine)
	task := &domain.QualityEvidence{ID: "q", Evaluator: qualitycheck.TaskEvaluator, Passed: true}
	mixed := patchedMetrics(48, 80, 12)
	campaign, run := oneEngineCampaign(t, patchedImage, optimizer.StateByDraftCommand, manifest.Disclosures, oneEngineMeasured(mixed))
	view := CandidateTechnique(campaign, run, task)
	if view == nil || view.Result == nil {
		t.Fatalf("view %+v", view)
	}
	names := []string{}
	for _, item := range view.Result.Evidence {
		names = append(names, item.Name)
	}
	if !slices.Equal(names, []string{technique.EvidenceFreshMachineConfirmation, optimizer.EvidenceStateByDraftReceipt, optimizer.EvidenceReservationMix}) {
		t.Fatalf("evidence %v", names)
	}
	if summary := view.Result.Evidence[2].Summary; !strings.Contains(summary, "all 5 measured runs") || !strings.Contains(summary, "up to 12") {
		t.Fatalf("mix summary %q", summary)
	}
	if !slices.Equal(view.Result.Disclosures, manifest.Disclosures) {
		t.Fatalf("disclosures %v", view.Result.Disclosures)
	}
	// A test binary records no build commit; a release build does.
	result := *view.Result
	result.Reproduction.Commit = "0123abc"
	if claim := manifest.ClaimOf(result); claim != technique.ClaimShown {
		t.Fatalf("claim %s, unknown %v", claim, result.Unknown())
	}

	withCommit := func(view *TechniqueView) technique.Result {
		result := *view.Result
		result.Reproduction.Commit = "0123abc"
		return result
	}
	stock := &benchmark.ServerMetrics{Collected: true, Metrics: serverMetrics(0.6, 0.93).Metrics}
	unverified, flapping := patchedMetrics(48, 80, 12), patchedMetrics(48, 80, 12)
	unverified.Metrics[optimizer.StateByDraftReceiptGauge] = benchmark.ServerMetricSummary{Type: "gauge", Series: 1, Stats: map[string]float64{"avg": 0, "min": 0, "max": 0}}
	flapping.Metrics[optimizer.StateByDraftReceiptGauge] = benchmark.ServerMetricSummary{Type: "gauge", Series: 1, Stats: map[string]float64{"avg": 0.5, "min": 0, "max": 1}}

	// Behind poc-modal's proxy the deployment names the proxy, not the
	// engine's image; the engine's own report still carries the evidence.
	proxied, run := oneEngineCampaign(t, "registry.example/modal-gpu-proxy@sha256:"+strings.Repeat("f", 64), "/modal-gpu-proxy", manifest.Disclosures, oneEngineMeasured(mixed))
	if claim := manifest.ClaimOf(withCommit(CandidateTechnique(proxied, run, task))); claim != technique.ClaimShown {
		t.Fatalf("behind a proxy: claim %s", claim)
	}
	for name, test := range map[string]struct {
		image, command string
		limitations    []string
		measured       *optimizationcampaign.MeasuredEvidence
		missing        []string
	}{
		"an engine whose receipt did not hold":         {patchedImage, optimizer.StateByDraftCommand, manifest.Disclosures, oneEngineMeasured(mixed, unverified), []string{optimizer.EvidenceStateByDraftReceipt, optimizer.EvidenceReservationMix}},
		"a receipt that held only at some scrapes":     {patchedImage, optimizer.StateByDraftCommand, manifest.Disclosures, oneEngineMeasured(mixed, flapping), []string{optimizer.EvidenceStateByDraftReceipt, optimizer.EvidenceReservationMix}},
		"a run without the patched gauges":             {patchedImage, optimizer.StateByDraftCommand, manifest.Disclosures, oneEngineMeasured(mixed, stock), []string{optimizer.EvidenceStateByDraftReceipt, optimizer.EvidenceReservationMix}},
		"a run without server metrics":                 {patchedImage, optimizer.StateByDraftCommand, manifest.Disclosures, oneEngineMeasured(mixed, nil), []string{optimizer.EvidenceStateByDraftReceipt, optimizer.EvidenceReservationMix}},
		"a run where the reservations never mixed":     {patchedImage, optimizer.StateByDraftCommand, manifest.Disclosures, oneEngineMeasured(mixed, patchedMetrics(40, 0, 0)), []string{optimizer.EvidenceReservationMix}},
		"a setup shown without one of its disclosures": {patchedImage, optimizer.StateByDraftCommand, manifest.Disclosures[1:], oneEngineMeasured(mixed), nil},
	} {
		campaign, run := oneEngineCampaign(t, test.image, test.command, test.limitations, test.measured)
		view := CandidateTechnique(campaign, run, task)
		result := withCommit(view)
		if claim := manifest.ClaimOf(result); claim != technique.ClaimNotShown {
			t.Errorf("%s: claim %s", name, claim)
		}
		for _, missing := range test.missing {
			if !slices.Contains(result.Unknown(), "evidence."+missing) {
				t.Errorf("%s: unknown %v lacks %s", name, result.Unknown(), missing)
			}
		}
	}
}

// SetupResult, which the Proof Report reads, returns a technique's result with
// the disclosures its setup carried, word for word, and judges it by the
// manifest: a setup shown without one of them is "not shown".
func TestSetupResultCarriesTheTechniqueDisclosures(t *testing.T) {
	manifest, _ := optimizer.TechniqueManifest(optimizer.TechniqueAdaptiveSpeculationOneEngine)
	task := &domain.QualityEvidence{ID: "q", Evaluator: qualitycheck.TaskEvaluator, Passed: true}
	campaign, run := oneEngineCampaign(t, patchedImage, optimizer.StateByDraftCommand, manifest.Disclosures, oneEngineMeasured(patchedMetrics(48, 80, 12)))
	result, _ := SetupResult(campaign, run, task)
	if result == nil || !slices.Equal(result.Disclosures, manifest.Disclosures) {
		t.Fatalf("result %+v", result)
	}
	// A test binary records no build commit; a release build does.
	stamped := *result
	stamped.Reproduction.Commit = "0123abc"
	if claim := manifest.ClaimOf(stamped); claim != technique.ClaimShown {
		t.Fatalf("claim %s, unknown %v", claim, stamped.Unknown())
	}
	campaign, run = oneEngineCampaign(t, patchedImage, optimizer.StateByDraftCommand, manifest.Disclosures[1:], oneEngineMeasured(patchedMetrics(48, 80, 12)))
	result, claim := SetupResult(campaign, run, task)
	if result == nil || claim != technique.ClaimNotShown || slices.Contains(result.Disclosures, manifest.Disclosures[0]) {
		t.Fatalf("without a disclosure: claim %s, result %+v", claim, result)
	}
}

// The record both items digest is the campaign's own: rebuilding it from the
// stored measurement gives the same digest.
func TestTheStateByDraftProofIsRebuiltFromTheStoredRun(t *testing.T) {
	measured := oneEngineMeasured(patchedMetrics(48, 80, 12), patchedMetrics(40, 60, 3))
	var draft optimizer.DeploymentDraft
	draft.Runtime.Workload = runtimecontract.Workload{Image: patchedImage, Command: []string{optimizer.StateByDraftCommand, "${MODEL}"}}
	first, second := stateByDraftEvidence(draft, *measured), stateByDraftEvidence(draft, *measured)
	if len(first) != 2 || first[0].Digest != second[0].Digest || first[0].Digest != first[1].Digest || !strings.HasPrefix(first[0].Digest, "sha256:") {
		t.Fatalf("evidence %+v and %+v", first, second)
	}
	proof := stateByDraftProof(draft, *measured)
	if len(proof.Runs) != 5 || proof.Runs[3].Window != "fresh-servers" || proof.Runs[3].Run != 1 || proof.Runs[1].MostMixed != 3 || proof.Identity != optimizer.StateByDraftIdentity {
		t.Fatalf("proof %+v", proof)
	}
}
