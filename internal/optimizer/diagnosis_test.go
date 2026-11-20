package optimizer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
	"github.com/infercrane/infercrane/internal/spec"
	"gopkg.in/yaml.v3"
)

var diagnosisWindow = time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)

func testDiagnosis(bottleneck string, signals map[string]float64, findings ...DiagnosisFinding) *Diagnosis {
	return &Diagnosis{
		Source: DiagnosisSourceEndpointReadiness, Endpoint: "chat-production", AlgorithmVersion: optimizationreadiness.AlgorithmVersion,
		WindowStart: diagnosisWindow, WindowEnd: diagnosisWindow.Add(time.Hour), State: optimizationreadiness.StateProfileReady,
		PrimaryBottleneck: bottleneck, Confidence: "stage-and-engine-level", Findings: findings, Signals: signals,
	}
}

func preemptionDiagnosis() *Diagnosis {
	return testDiagnosis(optimizationreadiness.BottleneckKVCachePressure,
		map[string]float64{optimizationreadiness.SignalRunningPerReplica: 48},
		DiagnosisFinding{Rule: optimizationreadiness.RuleKVPreemption, Signal: optimizationreadiness.SignalPreemptionRate, Observed: .08, Comparison: ">=", Threshold: .01, Unit: "ratio"})
}

func lowConcurrencyDiagnosis() *Diagnosis {
	return testDiagnosis(optimizationreadiness.BottleneckDecode,
		map[string]float64{optimizationreadiness.SignalRunningPerReplica: 1},
		DiagnosisFinding{Rule: optimizationreadiness.RuleLowConcurrency, Signal: optimizationreadiness.SignalRunningPerReplica, Observed: 1, Comparison: "<=", Threshold: 4, Unit: "requests"})
}

func genericRequest(gpu string, diagnosis *Diagnosis) Request {
	return Request{ModelIdentity: "acme/novel-open-model", ModelRevision: strings.Repeat("a", 40), Provider: "runpod-pods", GPU: gpu, Runtimes: []string{"vllm"}, MaxCandidates: 4, Diagnosis: diagnosis}
}

func reasonOrder(proposal Proposal) []string {
	var out []string
	for _, candidate := range proposal.Candidates {
		switch {
		case candidate.Reason == nil:
			out = append(out, "none")
		case candidate.Reason.Role == ReasonDiagnosis:
			out = append(out, candidate.Reason.Technique)
		default:
			out = append(out, candidate.Reason.Role)
		}
	}
	return out
}

func plannedTechnique(t *testing.T, proposal Proposal, id string) PlannedTechnique {
	t.Helper()
	for _, planned := range proposal.Diagnosis.Techniques {
		if planned.Technique == id {
			return planned
		}
	}
	t.Fatalf("technique %s not planned: %+v", id, proposal.Diagnosis)
	return PlannedTechnique{}
}

func TestDiagnosisDerivesRankedCandidatesWithReasonsAndKeepsGates(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", preemptionDiagnosis()))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reasonOrder(proposal), ","); got != "control,fp8-kv-cache,kv-sequence-cap" {
		t.Fatalf("order=%s", got)
	}
	control := proposal.Candidates[0]
	if proposal.StartingPoint == nil || proposal.StartingPoint.CandidateID != control.ID || len(control.Deployment.Runtime.Args) != 0 {
		t.Fatalf("the untuned starting setup must stay the control: %+v", proposal.StartingPoint)
	}
	wantArgs := map[string]string{TechniqueFP8KVCache: "--kv-cache-dtype fp8", TechniqueKVSequenceCap: "--max-num-seqs 32"}
	for index, candidate := range proposal.Candidates[1:] {
		reason := candidate.Reason
		if candidate.Rank != index+2 || candidate.EvidenceState != EvidenceUnmeasured || candidate.Status != "proposed-unmeasured" || candidate.Source.Name != diagnosisPlannerSource || candidate.Source.Version != DiagnosisRulesVersion {
			t.Fatalf("derived candidate crossed its proof boundary: %+v", candidate)
		}
		if got := strings.Join(candidate.Deployment.Runtime.Args, " "); got != wantArgs[reason.Technique] {
			t.Fatalf("%s args=%q", reason.Technique, got)
		}
		if reason.Priority == 0 || reason.ExpectedBenefit == "" || len(reason.Rules) == 0 || reason.Rules[0] != optimizationreadiness.RuleKVPreemption || !strings.Contains(reason.Summary, "0.08 per completed request") || len(reason.Evidence) == 0 {
			t.Fatalf("reason does not cite the diagnosis: %+v", reason)
		}
		for _, required := range control.RequiredEvidence {
			if !contains(candidate.RequiredEvidence, required) {
				t.Fatalf("derived candidate dropped required evidence %q", required)
			}
		}
		if index > 0 && reason.Priority > proposal.Candidates[index].Reason.Priority {
			t.Fatalf("candidates are not ordered by expected benefit: %v", reasonOrder(proposal))
		}
	}
	for _, candidate := range proposal.Candidates[1:] {
		if hasFeature(candidate.Features, "runtime_baseline", "untuned") || contains(candidate.Limitations, genericBaselineLimitation) || !hasFeature(candidate.Features, "diagnosis_technique", candidate.Reason.Technique) {
			t.Fatalf("a changed setup kept the untuned baseline's claims: %+v", candidate)
		}
	}
	fp8 := proposal.Candidates[1]
	if fp8.Reason.ExpectedBenefit != "high" || !contains(fp8.RequiredEvidence, "answer quality check against the starting setup because this setup changes numeric precision") {
		t.Fatalf("precision change lost its quality gate: %+v", fp8)
	}
	if plan := proposal.Diagnosis; plan.State != DiagnosisApplied || plan.RulesVersion != DiagnosisRulesVersion || plan.Endpoint != "chat-production" || !strings.Contains(plan.Summary, "FP8 KV cache, fewer requests at once") || len(plan.NeededEvidence) != 0 {
		t.Fatalf("plan=%+v", plan)
	}
	if planned := plannedTechnique(t, proposal, TechniqueFP8KVCache); len(planned.CandidateIDs) != 1 || planned.CandidateIDs[0] != fp8.ID || planned.Skipped != "" {
		t.Fatalf("plan does not link its candidate: %+v", planned)
	}
	if !contains(proposal.Warnings, diagnosisOrderWarning) {
		t.Fatalf("warnings=%v", proposal.Warnings)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosisIsBoundByTheInputDigestAndOptional(t *testing.T) {
	request := genericRequest("H100", preemptionDiagnosis())
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(proposal)
	var tampered Proposal
	if err = json.Unmarshal(encoded, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.Input.Diagnosis.Signals[optimizationreadiness.SignalRunningPerReplica] = 4
	if err = ValidateProposal(tampered); err == nil {
		t.Fatal("a changed diagnosis kept the input digest")
	}
	if err = json.Unmarshal(encoded, &tampered); err != nil {
		t.Fatal(err)
	}
	if err = ValidateProposal(tampered); err != nil {
		t.Fatalf("a round-tripped diagnosis proposal failed validation: %v", err)
	}

	request.Diagnosis = nil
	plain, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Diagnosis != nil || plain.Input.Diagnosis != nil || plain.InputDigest == proposal.InputDigest {
		t.Fatalf("a request without diagnosis changed: %+v", plain.Diagnosis)
	}
	raw, _ := json.Marshal(plain)
	if strings.Contains(string(raw), `"diagnosis"`) || strings.Contains(string(raw), `"reason"`) {
		t.Fatal("a proposal without diagnosis must serialize exactly as before")
	}
}

func TestDiagnosisWithoutActionableEvidenceMeasuresOnlyTheControl(t *testing.T) {
	base := Request{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", MaxCandidates: 3}
	plain, err := catalogSource(t).Propose(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	collecting := testDiagnosis(optimizationreadiness.BottleneckUnknown, nil)
	collecting.State = optimizationreadiness.StateCollecting
	blocked := testDiagnosis(optimizationreadiness.BottleneckReliability, map[string]float64{"error_rate": .07, "runtime_requests_running_per_replica": 2})
	blocked.State = optimizationreadiness.StateReliabilityBlocked
	unattributed := testDiagnosis(optimizationreadiness.BottleneckUnattributed, map[string]float64{"p95_latency": 900})
	for state, test := range map[string]struct {
		diagnosis *Diagnosis
		needed    []string
	}{
		DiagnosisNotReady:            {collecting, []string{NeededFreshTraffic, NeededEngineMetrics}},
		DiagnosisReliabilityBlocked:  {blocked, []string{NeededFewerErrors}},
		DiagnosisNoActionableFinding: {unattributed, []string{NeededEngineMetrics}},
	} {
		request := base
		request.Diagnosis = test.diagnosis
		proposal, err := catalogSource(t).Propose(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if proposal.Diagnosis.State != state || len(proposal.Diagnosis.Techniques) != 0 || !contains(proposal.Warnings, proposal.Diagnosis.Summary) || strings.Join(proposal.Diagnosis.NeededEvidence, "|") != strings.Join(test.needed, "|") {
			t.Fatalf("%s: plan=%+v warnings=%v", state, proposal.Diagnosis, proposal.Warnings)
		}
		if len(proposal.Candidates) != 1 || proposal.Candidates[0].ConfigurationProfile != plain.Candidates[0].ConfigurationProfile || proposal.Candidates[0].Reason.Role != ReasonControl {
			t.Fatalf("%s: a plan without a change must measure only the starting setup, got %v", state, reasonOrder(proposal))
		}
		for _, needed := range test.needed {
			if !strings.Contains(proposal.Diagnosis.Summary, needed) {
				t.Fatalf("%s: summary does not ask for %q: %s", state, needed, proposal.Diagnosis.Summary)
			}
		}
		if err = ValidateProposal(proposal); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiagnosisRecordsWhyAChosenTechniqueWasNotAdded(t *testing.T) {
	ampere, err := catalogSource(t).Propose(context.Background(), genericRequest("A100-80GB", lowConcurrencyDiagnosis()))
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedTechnique(t, ampere, TechniqueFP8Weights).Skipped; !strings.Contains(got, "FP8 in hardware") {
		t.Fatalf("FP8 on Ampere skipped=%q", got)
	}
	if got := plannedTechnique(t, ampere, TechniqueSpeculativeMTP).Skipped; !strings.Contains(got, "prediction head") {
		t.Fatalf("MTP without a declared head skipped=%q", got)
	}
	if ampere.Diagnosis.State != DiagnosisNoActionableFinding || len(ampere.Candidates) != 1 || !strings.Contains(ampere.Diagnosis.Summary, "none of the changes it chose applies") {
		t.Fatalf("plan=%+v candidates=%v", ampere.Diagnosis, reasonOrder(ampere))
	}

	declared := genericRequest("H100", lowConcurrencyDiagnosis())
	declared.ModelFeatures, declared.ModelConfigDigest = []string{FeatureNativeMTP}, recipeTestDigest
	withHead, err := catalogSource(t).Propose(context.Background(), declared)
	if err != nil {
		t.Fatal(err)
	}
	mtp := withHead.Candidates[1]
	if mtp.Reason.Technique != TechniqueSpeculativeMTP || !strings.Contains(strings.Join(mtp.Deployment.Runtime.Args, " "), `"method":"mtp"`) || !contains(mtp.RequiredEvidence, "output parity and speculative acceptance evidence") {
		t.Fatalf("declared prediction head did not lead: %+v", mtp)
	}

	quantized := genericRequest("H100", lowConcurrencyDiagnosis())
	quantized.ModelIdentity = "acme/novel-open-model-FP8"
	alreadyQuantized, err := catalogSource(t).Propose(context.Background(), quantized)
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedTechnique(t, alreadyQuantized, TechniqueFP8Weights).Skipped; !strings.Contains(got, "already look quantized") {
		t.Fatalf("quantized model skipped=%q", got)
	}

	prefix := testDiagnosis(optimizationreadiness.BottleneckPrefill, map[string]float64{optimizationreadiness.SignalSharedPrefixRatio: .9, "mean_input_tokens": 300})
	defaults, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", prefix))
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedTechnique(t, defaults, TechniquePrefixCache).Skipped; !strings.Contains(got, "on by default") {
		t.Fatalf("default-on prefix cache skipped=%q", got)
	}

	small := genericRequest("H100", lowConcurrencyDiagnosis())
	small.ModelIdentity, small.ModelFeatures, small.ModelConfigDigest = "acme/Tiny-0.6B-Instruct", []string{FeatureNativeMTP}, recipeTestDigest
	tiny, err := catalogSource(t).Propose(context.Background(), small)
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedTechnique(t, tiny, TechniqueSpeculativeMTP).Skipped; !strings.Contains(got, "0.6B parameters") {
		t.Fatalf("speculation on a 0.6B model skipped=%q", got)
	}
	if got := plannedTechnique(t, tiny, TechniqueFP8Weights).Skipped; !strings.Contains(got, "1.5B or fewer") {
		t.Fatalf("lower precision on a 0.6B model skipped=%q", got)
	}
	if got := plannedTechnique(t, tiny, TechniqueRightSizing).Skipped; !strings.Contains(got, "separate run") || len(tiny.Candidates) != 1 {
		t.Fatalf("right-sizing must be flagged, never added: %q %v", got, reasonOrder(tiny))
	}

	queue := testDiagnosis(optimizationreadiness.BottleneckQueueing, map[string]float64{"p95_queue": 400, "p95_latency": 1000})
	unknownGPU, err := catalogSource(t).Propose(context.Background(), genericRequest("Mystery-GPU", queue))
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedTechnique(t, unknownGPU, TechniqueBatchBudget).Skipped; !strings.Contains(got, "does not know Mystery-GPU's memory") {
		t.Fatalf("batch limits on an unknown GPU skipped=%q", got)
	}

	running := testDiagnosis(optimizationreadiness.BottleneckKVCachePressure, nil, preemptionDiagnosis().Findings...)
	unknownConcurrency, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", running))
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedTechnique(t, unknownConcurrency, TechniqueKVSequenceCap).Skipped; !strings.Contains(got, "running requests per server") {
		t.Fatalf("sequence cap without concurrency skipped=%q", got)
	}
}

func TestDiagnosisReEnablesATechniqueTheStartingSetupTurnedOff(t *testing.T) {
	control := Candidate{ID: strings.Repeat("c", 64), ConfigurationProfile: "vllm-generic-baseline"}
	control.Deployment.Runtime.Engine, control.Deployment.Runtime.Version = "vllm", "0.22.1"
	control.Deployment.Runtime.Args = []string{"--no-enable-prefix-caching", "--max-num-seqs", "64"}
	args, skipped := deriveArgs(control, techniques[TechniquePrefixCache], *testDiagnosis(optimizationreadiness.BottleneckPrefill, nil), Request{GPU: "H100"})
	if skipped != "" || strings.Join(args, " ") != "--max-num-seqs 64" {
		t.Fatalf("args=%v skipped=%q", args, skipped)
	}
	args, changed := withArgs([]string{"--max-num-seqs", "64", "--enforce-eager"}, []runtimeArg{{"--max-num-seqs", "1024"}, {"--max-num-batched-tokens", "8192"}})
	if !changed || strings.Join(args, " ") != "--max-num-seqs 1024 --enforce-eager --max-num-batched-tokens 8192" {
		t.Fatalf("args=%v", args)
	}
	if _, changed = withArgs([]string{"--kv-cache-dtype", "fp8"}, []runtimeArg{{"--kv-cache-dtype", "fp8"}}); changed {
		t.Fatal("an unchanged flag reported a change")
	}
	for name, test := range map[string]struct {
		gpu  string
		args []runtimeArg
		kept string
		why  string
	}{
		"default on 80 GB":    {"H100", []runtimeArg{{"--max-num-batched-tokens", "8192"}}, "", "already vLLM 0.22.1's defaults"},
		"default under 70 GB": {"RTX4090", []runtimeArg{{"--max-num-batched-tokens", "2048"}, {"--kv-cache-dtype", "fp8"}}, "--kv-cache-dtype fp8", ""},
		"A100 keeps small":    {"A100-80GB", []runtimeArg{{"--max-num-seqs", "256"}}, "", "already vLLM 0.22.1's defaults"},
		"unknown memory":      {"Mystery-GPU", []runtimeArg{{"--max-num-seqs", "512"}}, "", "does not know Mystery-GPU's memory"},
		"unrecorded flag":     {"H100", []runtimeArg{{"--block-size", "32"}}, "", "has not recorded vLLM 0.22.1's default for --block-size"},
	} {
		kept, why := withoutNoOps("vllm", "0.22.1", test.gpu, test.args)
		var flags []string
		for _, arg := range kept {
			flags = append(flags, arg.flag, arg.value)
		}
		if strings.Join(flags, " ") != test.kept || test.why != "" && !strings.Contains(why, test.why) || test.why == "" && why != "" {
			t.Errorf("%s: kept=%v why=%q", name, flags, why)
		}
	}
	if _, why := withoutNoOps("sglang", "0.5.12", "H100", []runtimeArg{{"--chunked-prefill-size", "8192"}}); !strings.Contains(why, "sizes --chunked-prefill-size from GPU memory") {
		t.Fatalf("SGLang sized default why=%q", why)
	}
	if _, why := withoutNoOps("vllm", "0.99.0", "H100", []runtimeArg{{"--kv-cache-dtype", "fp8"}}); !strings.Contains(why, "has not recorded vLLM 0.99.0's defaults") {
		t.Fatalf("unreviewed version why=%q", why)
	}
	for model, want := range map[string]float64{"Qwen/Qwen3-0.6B": .6, "meta-llama/Llama-3.1-8B-Instruct": 8, "Qwen/Qwen3-30B-A3B": 30, "openai/gpt-oss-120b@" + strings.Repeat("a", 40): 120} {
		if got, ok := modelSizeBillions(model); !ok || got != want {
			t.Errorf("%s size=%v ok=%t", model, got, ok)
		}
	}
	for _, model := range []string{"mistralai/Mixtral-8x7B", "deepseek-ai/DeepSeek-V3", "acme/novel-open-model"} {
		if _, ok := modelSizeBillions(model); ok {
			t.Errorf("%s has no stated size", model)
		}
	}
	if limit, lowers := sequenceCap(48); limit != 32 || !lowers {
		t.Fatalf("cap=%d lowers=%t", limit, lowers)
	}
	if _, lowers := sequenceCap(6); lowers {
		t.Fatal("a cap at or above the observed concurrency changes nothing")
	}
}

func TestDiagnosisCandidatesPassDeploymentSpecValidation(t *testing.T) {
	directory := t.TempDir()
	high := testDiagnosis(optimizationreadiness.BottleneckDecode, map[string]float64{optimizationreadiness.SignalRunningPerReplica: 64})
	for name, request := range map[string]Request{"kv": genericRequest("H100", preemptionDiagnosis()), "kv-mi300x": genericRequest("MI300X", preemptionDiagnosis()), "decode": genericRequest("H100", high), "host": genericRequest("H100", testDiagnosis(optimizationreadiness.BottleneckHostScheduler, nil, DiagnosisFinding{Rule: optimizationreadiness.RuleHostBound, Signal: metrics.MeasurementGPUSMActive, Observed: 30, Comparison: "<", Threshold: 50, Unit: "percent"}))} {
		proposal, err := catalogSource(t).Propose(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range proposal.Candidates[1:] {
			encoded, err := yaml.Marshal(candidate.Deployment)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, name+"-"+candidate.Deployment.Name+".yaml")
			if err = os.WriteFile(path, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err = spec.Load(path); err != nil {
				t.Fatalf("%s %s failed spec validation: %v", name, candidate.Deployment.Name, err)
			}
		}
	}
}

func TestDiagnosisPlansAMDInstinctGPUsFromTheirReviewedIdentity(t *testing.T) {
	// Every spelling of an MI300X resolves to 192 GB, so vLLM 0.22.1's large
	// batch defaults apply and restating them is a no-op, as on an H100.
	for _, gpu := range []string{"MI300X", "AMD Instinct MI300X OAM", "AMD_Instinct_MI300X_OAM", "MI325X", "MI355X"} {
		if class, known := acceleratorMemoryClass(gpu); !known || class != memoryLarge {
			t.Fatalf("%s memory class=%q known=%t", gpu, class, known)
		}
		if _, why := withoutNoOps("vllm", "0.22.1", gpu, []runtimeArg{{"--max-num-seqs", "1024"}, {"--max-num-batched-tokens", "8192"}}); !strings.Contains(why, "already vLLM 0.22.1's defaults") {
			t.Fatalf("%s kept a default: %q", gpu, why)
		}
		if !nativeFP8Accelerator(gpu) {
			t.Fatalf("%s is not treated as running FP8 in hardware", gpu)
		}
	}
	for _, gpu := range []string{"MI250X", "MI300A"} {
		if nativeFP8Accelerator(gpu) {
			t.Fatalf("unreviewed %s was treated as running FP8", gpu)
		}
		if _, known := acceleratorMemoryClass(gpu); known {
			t.Fatalf("unreviewed %s has a memory class", gpu)
		}
	}
	proposal, err := catalogSource(t).Propose(context.Background(), genericRequest("AMD Instinct MI300X OAM", preemptionDiagnosis()))
	if err != nil {
		t.Fatal(err)
	}
	var kv []string
	for _, candidate := range proposal.Candidates {
		if candidate.Reason != nil && candidate.Reason.Technique == TechniqueFP8KVCache {
			kv = candidate.Deployment.Runtime.Args
		}
	}
	if !strings.Contains(strings.Join(kv, " "), "--kv-cache-dtype fp8") {
		t.Fatalf("no FP8 KV cache setup on MI300X: %+v", proposal.Diagnosis)
	}
	if proposal.Diagnosis == nil || proposal.Diagnosis.RulesVersion != DiagnosisRulesVersion {
		t.Fatalf("rules version=%+v", proposal.Diagnosis)
	}
}

func TestDiagnosisCustomerTextOmitsInternalWording(t *testing.T) {
	manifest, err := LoadPlanningGoldenManifest(planningGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	internal := regexp.MustCompile(`(?i)recipe search|search control|\barms?\b|readiness|_|kv_cache\.|gpu\.`)
	for _, task := range manifest.Tasks {
		proposal, err := catalogSource(t).Propose(context.Background(), task.Request)
		if err != nil {
			t.Fatal(err)
		}
		text := append([]string{proposal.Diagnosis.Summary}, proposal.Warnings...)
		for _, planned := range proposal.Diagnosis.Techniques {
			text = append(text, planned.Label, planned.Reason, planned.Skipped)
		}
		for _, candidate := range proposal.Candidates {
			text = append(text, candidate.Reason.Label, candidate.Reason.Summary)
		}
		for _, line := range text {
			if internal.MatchString(line) {
				t.Errorf("%s shows internal wording: %q", task.ID, line)
			}
		}
	}
}

func TestDiagnosisPlansAreDeterministicUnderConcurrentUse(t *testing.T) {
	source := catalogSource(t)
	request := genericRequest("H100", preemptionDiagnosis())
	first, err := source.Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(first)
	var group sync.WaitGroup
	failures := make(chan string, 16)
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			proposal, err := source.Propose(context.Background(), request)
			got, _ := json.Marshal(proposal)
			if err != nil || string(got) != string(want) {
				failures <- "concurrent proposal differed"
			}
		}()
	}
	group.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
	if request.Diagnosis.Signals[optimizationreadiness.SignalRunningPerReplica] != 48 {
		t.Fatal("planning mutated the caller's diagnosis")
	}
}

func TestValidateDiagnosisRejectsMalformedEvidence(t *testing.T) {
	mutations := map[string]func(*Diagnosis){
		"source":     func(d *Diagnosis) { d.Source = "client" },
		"endpoint":   func(d *Diagnosis) { d.Endpoint = "" },
		"state":      func(d *Diagnosis) { d.State = "ready" },
		"bottleneck": func(d *Diagnosis) { d.PrimaryBottleneck = "KV Cache!" },
		"window":     func(d *Diagnosis) { d.WindowEnd = d.WindowStart.Add(-time.Minute) },
		"finding":    func(d *Diagnosis) { d.Findings[0].Comparison = "~" },
		"signal":     func(d *Diagnosis) { d.Signals["Bad Name"] = 1 },
		"profile": func(d *Diagnosis) {
			d.GPUProfile = &GPUProfileSummary{CaptureID: "c", CapturedAt: diagnosisWindow, KernelSeconds: 1, Shares: map[string]float64{"attention": 1.5}}
		},
	}
	for name, mutate := range mutations {
		diagnosis := preemptionDiagnosis()
		diagnosis.Signals = map[string]float64{}
		mutate(diagnosis)
		if _, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", diagnosis)); err == nil {
			t.Errorf("%s: malformed diagnosis accepted", name)
		}
	}
	reason := CandidateReason{Role: ReasonDiagnosis, Technique: "made-up", Label: "x"}
	if err := validateCandidateReason(reason); err == nil {
		t.Fatal("an unknown technique reason was accepted")
	}
}

func TestEstimatorRefusesADiagnosisOrderedRequest(t *testing.T) {
	source := AIConfiguratorSource{Catalog: catalogSource(t)}
	if _, err := source.Propose(context.Background(), genericRequest("H100", preemptionDiagnosis())); !errors.Is(err, ErrDiagnosisNeedsCatalog) {
		t.Fatalf("err=%v", err)
	}
}

func TestDiagnosisFromAssessmentFeedsThePlanner(t *testing.T) {
	now := time.Now().UTC()
	zero, latency := 0.0, 4000.0
	value := func(v float64) *float64 { return &v }
	snapshot := domain.EndpointMonitoringSnapshot{
		Endpoint: "chat-production", WindowStart: now.Add(-time.Hour), WindowEnd: now, BucketSeconds: 60,
		Summary:  domain.MonitoringSummary{Requests: 200, ErrorRate: &zero, P95LatencyMS: &latency},
		Evidence: domain.MonitoringEvidence{Source: "infercrane_gateway_request_records", SampleCount: 200, Fresh: true},
	}
	for name, observed := range map[string]float64{metrics.MeasurementPreemptions: 20, metrics.MeasurementRequestsCompleted: 200, metrics.MeasurementRequestsRunning: 48, metrics.MeasurementKVCacheUsage: 98} {
		snapshot.Evidence.Measurements = append(snapshot.Evidence.Measurements, domain.MeasurementEvidence{Name: name, Value: value(observed), Availability: "available", EvidenceClass: "measured", Source: "vllm_metrics", SampleCount: 1})
	}
	assessment := optimizationreadiness.Assess(snapshot, optimizationreadiness.DefaultPolicy())
	diagnosis := DiagnosisFromAssessment(assessment, snapshot.WindowStart, snapshot.WindowEnd)
	if diagnosis.PrimaryBottleneck != optimizationreadiness.BottleneckKVCachePressure || len(diagnosis.Findings) == 0 || diagnosis.Signals[optimizationreadiness.SignalRunningPerReplica] != 48 {
		t.Fatalf("diagnosis=%+v", diagnosis)
	}
	if _, present := diagnosis.Signals["p95_queue"]; present {
		t.Fatal("an unavailable signal became a value")
	}
	proposal, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", &diagnosis))
	if err != nil {
		t.Fatal(err)
	}
	if order := reasonOrder(proposal); len(order) < 2 || order[1] != TechniqueFP8KVCache {
		t.Fatalf("order=%v", order)
	}
}

func TestSummarizeGPUProfileGroupsKernelTime(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	profile := gpuprofile.Profile{
		Revision: 3,
		Capture:  gpuprofile.Capture{ID: "cap-1", CaptureMetadata: gpuprofile.CaptureMetadata{Tool: "cupti", EndedAt: now.Add(-time.Hour)}},
		Frames: []gpuprofile.Frame{
			{Kind: gpuprofile.KindPython, Name: "forward"},
			{Kind: gpuprofile.KindKernel, Name: "flash_fwd_splitkv_kernel"},
			{Kind: gpuprofile.KindKernel, Name: "sm90_xmma_gemm_bf16bf16_bf16f32"},
			{Kind: gpuprofile.KindKernel, Name: "ncclDevKernel_AllReduce_Sum_bf16_RING_LL"},
			{Kind: gpuprofile.KindKernel, Name: "rms_norm_kernel"},
			{Kind: gpuprofile.KindSASS, Name: "LDG.E.128"},
		},
		Stacks: []gpuprofile.Stack{
			{Frames: []int{0, 1, 5}, GPUNS: 5e9},
			{Frames: []int{0, 2}, GPUNS: 3e9},
			{Frames: []int{0, 3}, GPUNS: 1e9},
			{Frames: []int{0, 4}, GPUNS: 1e9},
			{Frames: []int{0}, GPUNS: 7e9},
		},
	}
	summary := SummarizeGPUProfile(profile, now, GPUProfileMaxAge)
	if summary == nil || summary.KernelSeconds != 10 || summary.Shares[KernelClassAttention] != .5 || summary.Shares[KernelClassGEMM] != .3 || summary.Shares[KernelClassCommunication] != .1 || summary.Shares[KernelClassOther] != .1 || summary.CaptureID != "cap-1" || summary.Revision != 3 {
		t.Fatalf("summary=%+v", summary)
	}
	if SummarizeGPUProfile(profile, now.Add(8*24*time.Hour), GPUProfileMaxAge) != nil {
		t.Fatal("a stale capture was summarized")
	}
	profile.Stacks = []gpuprofile.Stack{{Frames: []int{0}, GPUNS: 1e9}}
	if SummarizeGPUProfile(profile, now, GPUProfileMaxAge) != nil {
		t.Fatal("a capture without kernel time was summarized")
	}
	for name, class := range map[string]string{"BatchDecodeWithPagedKVCacheKernel": KernelClassAttention, "fused_moe_kernel": KernelClassGEMM, "Marlin<256>": KernelClassGEMM, "cross_device_reduce_1stage": KernelClassCommunication, "silu_and_mul_kernel": KernelClassOther} {
		if got := KernelClass(name); got != class {
			t.Errorf("%s classified %s, want %s", name, got, class)
		}
	}
}

func decodeDiagnosis() *Diagnosis {
	return testDiagnosis(optimizationreadiness.BottleneckDecode, map[string]float64{optimizationreadiness.SignalRunningPerReplica: 64, "p95_generation": 2600, "p95_latency": 3000, "mean_input_tokens": 800})
}

func TestDiagnosisPlansAITERKernelsOnlyOnReviewedAMDAccelerators(t *testing.T) {
	source := catalogSource(t)
	for _, gpu := range []string{"MI300X", "AMD Instinct MI300X OAM", "MI355X"} {
		proposal, err := source.Propose(context.Background(), genericRequest(gpu, decodeDiagnosis()))
		if err != nil {
			t.Fatal(err)
		}
		if order := reasonOrder(proposal); strings.Join(order, ",") != "control,aiter-kernels,fp8-weights" {
			t.Fatalf("%s order=%v plan=%+v", gpu, order, proposal.Diagnosis)
		}
		if err := ValidateProposal(proposal); err != nil {
			t.Fatalf("%s: %v", gpu, err)
		}
		control, aiter := proposal.Candidates[0], proposal.Candidates[1]
		if len(control.Deployment.Runtime.Env) != 0 || aiter.Deployment.Runtime.Env["VLLM_ROCM_USE_AITER"] != "1" || len(aiter.Deployment.Runtime.Env) != 1 {
			t.Fatalf("%s env control=%v aiter=%v", gpu, control.Deployment.Runtime.Env, aiter.Deployment.Runtime.Env)
		}
		// Only the kernels change: same flags, model, image, and GPU.
		if strings.Join(aiter.Deployment.Runtime.Args, " ") != strings.Join(control.Deployment.Runtime.Args, " ") || !reflect.DeepEqual(aiter.Deployment.Model, control.Deployment.Model) || aiter.Deployment.Resources != control.Deployment.Resources {
			t.Fatalf("%s AITER setup changed more than the kernels: %+v", gpu, aiter.Deployment)
		}
		if !contains(aiter.RequiredEvidence, "answer quality check against the starting setup because this setup swaps the GPU kernels that compute each layer") {
			t.Fatalf("%s AITER setup skips the quality check: %v", gpu, aiter.RequiredEvidence)
		}
		planned := plannedTechnique(t, proposal, TechniqueAITER)
		if planned.Priority != acceleratorKernelBottleneckPriority || !contains(planned.Rules, RuleAcceleratorKernels) || !strings.Contains(planned.Reason, "87% of p95 request time") || !strings.Contains(planned.Reason, "AMD's AITER kernels") {
			t.Fatalf("%s planned=%+v", gpu, planned)
		}
		if classified := classifyCandidate(aiter); !contains(classified, TechniqueAITER) {
			t.Fatalf("%s AITER setup classified as %v", gpu, classified)
		}
	}
	// The same diagnosis on NVIDIA, on an unreviewed AMD GPU, or on SGLang,
	// whose ROCm image already turns AITER on, never names AITER.
	for _, request := range []Request{genericRequest("H100", decodeDiagnosis()), genericRequest("MI250X", decodeDiagnosis())} {
		proposal, err := source.Propose(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		for _, planned := range proposal.Diagnosis.Techniques {
			if planned.Technique == TechniqueAITER {
				t.Fatalf("%s planned AITER: %+v", request.GPU, proposal.Diagnosis)
			}
		}
	}
	// A full KV cache or a queue is not kernel time, so AITER is not chosen.
	for _, diagnosis := range []*Diagnosis{preemptionDiagnosis(), testDiagnosis(optimizationreadiness.BottleneckQueueing, nil)} {
		proposal, err := source.Propose(context.Background(), genericRequest("MI300X", diagnosis))
		if err != nil {
			t.Fatal(err)
		}
		if contains(reasonOrder(proposal), TechniqueAITER) {
			t.Fatalf("%s chose AITER: %v", diagnosis.PrimaryBottleneck, reasonOrder(proposal))
		}
	}
}

func TestDiagnosisAITERFromKernelProfileAndLongPromptsOnly(t *testing.T) {
	source := catalogSource(t)
	profiled := testDiagnosis(optimizationreadiness.BottleneckUnattributed, nil)
	profiled.GPUProfile = &GPUProfileSummary{CaptureID: "capture-mi300x", Tool: "torch-profiler", Revision: 1, CapturedAt: diagnosisWindow, KernelSeconds: 12, Shares: map[string]float64{KernelClassGEMM: 0.62, KernelClassAttention: 0.2, KernelClassOther: 0.18}}
	proposal, err := source.Propose(context.Background(), genericRequest("MI300X", profiled))
	if err != nil {
		t.Fatal(err)
	}
	planned := plannedTechnique(t, proposal, TechniqueAITER)
	if planned.Priority != acceleratorKernelProfilePriority || !strings.Contains(planned.Reason, "Matrix-multiply kernels took 62% of sampled GPU time") || len(planned.CandidateIDs) != 1 {
		t.Fatalf("profile planned=%+v", planned)
	}
	// A trace analysis of the capture replaces the attention share with its
	// own finding, as it does for the FP8 KV cache rule.
	attention := GPUTraceFinding{Rule: TraceRuleAttentionHeavy, Observed: .41, Comparison: ">=", Threshold: .35, Unit: "ratio"}
	traced := traceDiagnosis(optimizationreadiness.BottleneckUnattributed, nil, traceSummary("attention", nil, attention))
	traced.GPUProfile.Shares = map[string]float64{KernelClassAttention: .5, KernelClassGEMM: .3, KernelClassOther: .2}
	proposal, err = source.Propose(context.Background(), genericRequest("MI300X", traced))
	if err != nil {
		t.Fatal(err)
	}
	planned = plannedTechnique(t, proposal, TechniqueAITER)
	if planned.Priority != acceleratorKernelProfilePriority || !strings.HasPrefix(planned.Reason, "Attention took 41% of kernel time in the GPU trace (limit 35%).") || strings.Contains(planned.Reason, "sampled GPU time") {
		t.Fatalf("trace planned=%+v", planned)
	}
	traced.GPUProfile.Trace.Findings = nil
	proposal, err = source.Propose(context.Background(), genericRequest("MI300X", traced))
	if err != nil {
		t.Fatal(err)
	}
	if contains(reasonOrder(proposal), TechniqueAITER) {
		t.Fatalf("a trace without an attention finding chose AITER from the profile share: %v", reasonOrder(proposal))
	}
	// Short prompts' first-token time is not prompt work, so it chooses nothing.
	short := testDiagnosis(optimizationreadiness.BottleneckPrefill, map[string]float64{"mean_input_tokens": 300, "p95_ttft": 900, "p95_latency": 1000})
	proposal, err = source.Propose(context.Background(), genericRequest("MI300X", short))
	if err != nil {
		t.Fatal(err)
	}
	if contains(reasonOrder(proposal), TechniqueAITER) {
		t.Fatalf("short prompts chose AITER: %v", reasonOrder(proposal))
	}
	long := testDiagnosis(optimizationreadiness.BottleneckPrefill, map[string]float64{"mean_input_tokens": 6000, "p95_ttft": 900, "p95_latency": 1000})
	proposal, err = source.Propose(context.Background(), genericRequest("MI300X", long))
	if err != nil {
		t.Fatal(err)
	}
	if order := reasonOrder(proposal); strings.Join(order, ",") != "control,aiter-kernels,prefill-chunk-large" {
		t.Fatalf("long prompts order=%v", order)
	}
}

func TestDeriveEnvSkipsNoOpsAgainstTheRecordedDefault(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), genericRequest("MI300X", nil))
	if err != nil {
		t.Fatal(err)
	}
	control := proposal.Candidates[0]
	request := Request{GPU: "MI300X"}
	env, why := deriveEnv(control, techniques[TechniqueAITER], request)
	if why != "" || env["VLLM_ROCM_USE_AITER"] != "1" {
		t.Fatalf("env=%v why=%q", env, why)
	}
	// The derived map is a copy; the starting setup is unchanged.
	if control.Deployment.Runtime.Env != nil {
		t.Fatalf("starting setup env changed: %v", control.Deployment.Runtime.Env)
	}
	on := control
	on.Deployment.Runtime.Env = map[string]string{"VLLM_ROCM_USE_AITER": "1"}
	if _, why = deriveEnv(on, techniques[TechniqueAITER], request); why != "The starting setup already uses this change." {
		t.Fatalf("already on: %q", why)
	}
	// Setting the switch to the engine's recorded default (off) is a no-op.
	off := technique{id: "aiter-off", env: map[string][]engineVariable{"vllm": {{"VLLM_ROCM_USE_AITER", "0"}}}}
	if _, why = deriveEnv(control, off, request); !strings.Contains(why, "already vLLM 0.22.1's default on MI300X") {
		t.Fatalf("default: %q", why)
	}
	for name, test := range map[string]struct {
		mutate func(*Candidate)
		gpu    string
		want   string
	}{
		"NVIDIA":         {func(*Candidate) {}, "H100", "not an accelerator InferCrane has reviewed"},
		"engine version": {func(c *Candidate) { c.Deployment.Runtime.Version = "0.23.0" }, "MI300X", "not reviewed for vLLM 0.23.0 on MI300X"},
		"custom image": {func(c *Candidate) {
			c.Deployment.Runtime.Workload.Image, c.Deployment.Runtime.Workload.Command = "registry.internal:5000/team/vllm@sha256:"+strings.Repeat("c", 64), []string{"vllm", "serve"}
		}, "MI300X", "pinned custom image"},
	} {
		candidate := control
		test.mutate(&candidate)
		if _, why := deriveEnv(candidate, techniques[TechniqueAITER], Request{GPU: test.gpu}); !strings.Contains(why, test.want) {
			t.Fatalf("%s: %q", name, why)
		}
	}
}

func TestProposalValidationRejectsUnreviewedEngineEnvironment(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), genericRequest("MI300X", decodeDiagnosis()))
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []map[string]string{{"HF_TOKEN": "hf_x"}, {"VLLM_ROCM_USE_AITER": "true"}} {
		tampered := proposal
		tampered.Candidates = append([]Candidate(nil), proposal.Candidates...)
		tampered.Candidates[1].Deployment.Runtime.Env = env
		if err := ValidateProposal(tampered); err == nil || !strings.Contains(err.Error(), "runtime env") {
			t.Fatalf("env %v accepted: %v", env, err)
		}
	}
}
