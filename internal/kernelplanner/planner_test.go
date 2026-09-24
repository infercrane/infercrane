package kernelplanner

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestBuildIsModelAgnosticDeterministicAndAmdahlGated(t *testing.T) {
	request := fixtureRequest()
	first, err := Build(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("kernel plan is not deterministic")
	}
	if first.EvidenceClass != "fixture-unmeasured" || len(first.Candidates) != 2 || first.Candidates[0].OperatorFamily != QuantizedLinear || len(first.Rejected) != 2 {
		t.Fatalf("plan=%+v", first)
	}
	winner := first.Candidates[0]
	if math.Abs(winner.MaxEndToEndSpeedup-1.4705882353) > 1e-6 || winner.RequiredKernelSpeedup <= 1 || winner.Status != "proposed-unmeasured" {
		t.Fatalf("candidate=%+v", winner)
	}
	if first.RegistryVersion != RegistryVersion || len(winner.ExistingImplementations) < 3 || winner.ExistingImplementations[0].ImplementationID != "runtime-vllm" {
		t.Fatalf("kernel search did not preserve existing-first order: %+v", winner.ExistingImplementations)
	}
	if !strings.Contains(first.SelectionBoundary, "AIPerf") {
		t.Fatalf("selection boundary=%q", first.SelectionBoundary)
	}
}

func TestBuildWorksForAnyPinnedModelIdentity(t *testing.T) {
	request := fixtureRequest()
	request.Model.Repository = "example/new-open-weight-architecture"
	request.Model.Revision = strings.Repeat("b", 40)
	plan, err := Build(request)
	if err != nil || len(plan.Candidates) != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
}

func TestBuildPlansHybridRecurrentOperatorsWithoutModelNameBranches(t *testing.T) {
	request := fixtureRequest()
	request.Model.Repository = "example/unseen-hybrid-architecture"
	request.Profile.Hotspots = []Hotspot{
		{ID: "recurrent", Name: "delta_state_update", Family: LinearRecurrence, DeviceTimeFraction: .18, Shapes: []string{"12x48x128x128"}, DTypes: []string{"bf16", "fp32"}},
		{ID: "commit", Name: "recurrent_state_commit", Family: StateCommit, DeviceTimeFraction: .08, Shapes: []string{"48x12x4x48x128x128"}, DTypes: []string{"fp32"}},
	}
	plan, err := Build(request)
	if err != nil || len(plan.Candidates) != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if plan.Candidates[0].OperatorFamily != LinearRecurrence || plan.Candidates[1].OperatorFamily != StateCommit {
		t.Fatalf("unexpected recurrent plan: %+v", plan.Candidates)
	}
	if plan.Candidates[0].Template != "fused-linear-recurrence-update" || plan.Candidates[1].Template != "fused-recurrent-state-commit" {
		t.Fatalf("unexpected templates: %+v", plan.Candidates)
	}
}

func TestBuildSelectsVendorNativeCompilerFamilies(t *testing.T) {
	tests := []struct {
		vendor, accelerator, compiler string
	}{
		{"amd", "MI300X", "aiter"},
		{"google", "TPU-V6E", "pallas"},
		{"aws", "TRN2", "nki"},
	}
	for _, test := range tests {
		request := fixtureRequest()
		request.Hardware = HardwareIdentity{Vendor: test.vendor, Accelerator: test.accelerator}
		request.Runtime.ImageDigest = "sha256:" + strings.Repeat("3", 64)
		plan, err := Build(request)
		if err != nil || len(plan.Candidates) == 0 || len(plan.Candidates[0].CompilerChoices) == 0 || plan.Candidates[0].CompilerChoices[0] != test.compiler {
			t.Fatalf("vendor=%s plan=%+v err=%v", test.vendor, plan, err)
		}
	}
}

func TestBuildRejectsUnknownAcceleratorVendor(t *testing.T) {
	request := fixtureRequest()
	request.Hardware.Vendor = "mystery"
	if _, err := Build(request); err == nil || !strings.Contains(err.Error(), "nvidia, amd, google, or aws") {
		t.Fatalf("unknown vendor was accepted: %v", err)
	}
}

func TestBuildFailsClosedForFixtureWhenMeasuredProfileRequired(t *testing.T) {
	request := fixtureRequest()
	request.Policy.RequireMeasuredProfiler = true
	if _, err := Build(request); err == nil || !strings.Contains(err.Error(), "measured profiler") {
		t.Fatalf("fixture profile passed measured boundary: %v", err)
	}
}

func TestBuildRejectsInvalidIdentityAndFractions(t *testing.T) {
	request := fixtureRequest()
	request.Model.Revision = "main"
	if _, err := Build(request); err == nil {
		t.Fatal("mutable model revision was accepted")
	}
	request = fixtureRequest()
	request.Profile.Hotspots[1].DeviceTimeFraction = .9
	if _, err := Build(request); err == nil || !strings.Contains(err.Error(), "greater than one") {
		t.Fatalf("overlapping profile was accepted: %v", err)
	}
}

func TestBuildRejectsAnEndToEndTargetBeyondAmdahlCeiling(t *testing.T) {
	request := fixtureRequest()
	request.Policy.TargetEndToEndSpeedup = 1.4
	plan, err := Build(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || len(plan.Rejected) != 3 {
		t.Fatalf("plan=%+v", plan)
	}
	found := false
	for _, rejected := range plan.Rejected {
		found = found || rejected.HotspotID == "norm" && strings.Contains(rejected.Reason, "not reachable")
	}
	if !found {
		t.Fatalf("missing Amdahl target rejection: %+v", plan.Rejected)
	}
}

func fixtureRequest() Request {
	return Request{
		SchemaVersion: RequestSchemaV1,
		Model:         ModelIdentity{Repository: "Qwen/Qwen3-0.6B", Revision: strings.Repeat("a", 40)},
		Runtime:       RuntimeIdentity{Name: "vllm", Version: "0.22.1", ImageDigest: "sha256:" + strings.Repeat("1", 64)},
		Hardware:      HardwareIdentity{Vendor: "nvidia", Accelerator: "H100", ComputeCapability: "sm90"},
		Workload:      Workload{Digest: "sha256:" + strings.Repeat("2", 64), Phase: PhaseDecode, BatchSize: 1, Concurrency: 1, InputTokens: 32, OutputTokens: 16},
		Profile: Profile{Tool: "fixture", ToolVersion: "1", EvidenceClass: "fixture", Hotspots: []Hotspot{
			{ID: "linear", Name: "q_proj", Family: QuantizedLinear, DeviceTimeFraction: .32, Shapes: []string{"1x1024x1024"}, DTypes: []string{"int4", "bf16"}},
			{ID: "norm", Name: "residual_rmsnorm", Family: ResidualRMSNorm, DeviceTimeFraction: .12},
			{ID: "rope", Name: "rope", Family: "unknown", DeviceTimeFraction: .04},
			{ID: "tiny", Name: "sampling", Family: Sampling, DeviceTimeFraction: .01},
		}},
		Policy: Policy{MinHotspotFraction: .05, MinMaxEndToEndSpeedup: 1.05, TargetEndToEndSpeedup: 1.02, MaxCandidates: 3},
	}
}
