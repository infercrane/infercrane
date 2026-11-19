package optimizer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
)

// kernelDiagnosis is a trace whose norm kernels dominate and run far below
// what the GPU allows: the one case the reviewed lab pack covers.
func kernelDiagnosis(lab bool, operators ...TraceOperator) *Diagnosis {
	trace := traceSummary(gpuprofile.BottleneckMemoryBandwidth, nil)
	trace.Operators = operators
	diagnosis := traceDiagnosis(optimizationreadiness.BottleneckUnattributed, map[string]float64{optimizationreadiness.SignalRunningPerReplica: 12}, trace)
	diagnosis.KernelLab = lab
	return diagnosis
}

// kernelRequest is a run on recorded traffic, which a kernel lane replays.
func kernelRequest(gpu string, diagnosis *Diagnosis) Request {
	request := genericRequest(gpu, diagnosis)
	request.WorkloadSource, request.WorkloadFingerprint = WorkloadSourceCustomerObserved, "sha256:"+strings.Repeat("b", 64)
	return request
}

func operatorTime(class string, share float64, efficiency *float64) TraceOperator {
	return TraceOperator{Operator: class, Share: share, CoveredShare: .8, Efficiency: efficiency}
}

func TestKernelSetupIsPlannedLastOnlyWhenEveryGateHolds(t *testing.T) {
	weak := floatPointer(.32)
	proposal, err := catalogSource(t).Propose(context.Background(), kernelRequest("H100", kernelDiagnosis(true, operatorTime(gpuprofile.OperatorNorm, .41, weak), operatorTime(gpuprofile.OperatorAttention, .2, nil))))
	if err != nil {
		t.Fatal(err)
	}
	order := reasonOrder(proposal)
	if order[len(order)-1] != TechniqueCustomKernel {
		t.Fatalf("a custom kernel must come last: %v", order)
	}
	kernel := proposal.Candidates[len(proposal.Candidates)-1]
	lane := kernel.KernelLane
	if lane == nil || lane.Operator != gpuprofile.OperatorNorm || lane.Family != "residual-rmsnorm" || lane.Vendor != "nvidia" || lane.Engine != "vllm" || lane.EngineVersion != "0.22.1" ||
		!strings.HasPrefix(lane.ImageDigest, "sha256:") || lane.Share != .41 || lane.Efficiency != .32 || lane.LabGPUHours != KernelLabGPUHours {
		t.Fatalf("lane = %+v", lane)
	}
	// It keeps the starting setup's flags: the kernel is the only change.
	if strings.Join(kernel.Deployment.Runtime.Args, " ") != strings.Join(proposal.Candidates[0].Deployment.Runtime.Args, " ") {
		t.Fatalf("kernel setup changed flags: %v", kernel.Deployment.Runtime.Args)
	}
	planned := plannedTechnique(t, proposal, TechniqueCustomKernel)
	if planned.Priority != kernelLanePriority || !strings.Contains(planned.Reason, "41%") || !strings.Contains(planned.Reason, "32%") || !strings.Contains(planned.Reason, "never wait") {
		t.Fatalf("plan = %+v", planned)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatalf("the planner's own proposal is invalid: %v", err)
	}
}

func TestKernelSetupNeedsEveryGate(t *testing.T) {
	weak, strong := floatPointer(.32), floatPointer(.7)
	for name, test := range map[string]struct {
		gpu       string
		diagnosis *Diagnosis
	}{
		"no lab on this control plane": {"H100", kernelDiagnosis(false, operatorTime(gpuprofile.OperatorNorm, .41, weak))},
		"not dominant":                 {"H100", kernelDiagnosis(true, operatorTime(gpuprofile.OperatorNorm, .2, weak))},
		"engine kernel not weak":       {"H100", kernelDiagnosis(true, operatorTime(gpuprofile.OperatorNorm, .41, strong))},
		"efficiency not measured":      {"H100", kernelDiagnosis(true, operatorTime(gpuprofile.OperatorNorm, .41, nil))},
		"no reviewed pack for experts": {"H100", kernelDiagnosis(true, operatorTime(gpuprofile.OperatorMoEExperts, .9, weak))},
		"no reviewed pack on AMD":      {"MI300X", kernelDiagnosis(true, operatorTime(gpuprofile.OperatorNorm, .41, weak))},
		"another operator dominates": {"H100", kernelDiagnosis(true, operatorTime(gpuprofile.OperatorNorm, .32, weak),
			operatorTime(gpuprofile.OperatorAttention, .5, nil))},
	} {
		proposal, err := catalogSource(t).Propose(context.Background(), kernelRequest(test.gpu, test.diagnosis))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, candidate := range proposal.Candidates {
			if candidate.KernelLane != nil || candidate.Reason != nil && candidate.Reason.Technique == TechniqueCustomKernel {
				t.Errorf("%s: planned a kernel: %v", name, reasonOrder(proposal))
			}
		}
	}
}

func TestKernelSetupWaitsForASettingChangeOnTheSameOperator(t *testing.T) {
	fp8 := plannedChoice{technique: techniques[TechniqueFP8Weights]}
	kernel := plannedChoice{technique: techniques[TechniqueCustomKernel]}
	if !contains(fp8.technique.targets, gpuprofile.OperatorMoEExperts) || contains(fp8.technique.targets, gpuprofile.OperatorNorm) {
		t.Fatalf("FP8 weights targets %v", fp8.technique.targets)
	}
	if !contains(techniques[TechniqueAITER].targets, gpuprofile.OperatorNorm) || len(kernel.technique.targets) != 0 {
		t.Fatal("AITER replaces norm kernels; a custom kernel targets nothing a setting changes")
	}
}

func TestProposalValidationRefusesAForgedKernelLane(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), kernelRequest("H100", kernelDiagnosis(true, operatorTime(gpuprofile.OperatorNorm, .41, floatPointer(.32)))))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(proposal)
	for name, mutate := range map[string]func(*KernelLane, *Candidate){
		"another image":          func(lane *KernelLane, _ *Candidate) { lane.ImageDigest = "sha256:" + strings.Repeat("0", 64) },
		"unreviewed engine":      func(lane *KernelLane, _ *Candidate) { lane.EngineVersion = "0.23.0" },
		"more lab time":          func(lane *KernelLane, _ *Candidate) { lane.LabGPUHours = 40 },
		"operator not dominant":  func(lane *KernelLane, _ *Candidate) { lane.Share = .1 },
		"engine kernel not weak": func(lane *KernelLane, _ *Candidate) { lane.Efficiency = .9 },
		"another family":         func(lane *KernelLane, _ *Candidate) { lane.Family = "attention-decode" },
		"lane without its reason": func(_ *KernelLane, candidate *Candidate) {
			candidate.Reason.Technique = TechniqueFP8Weights
		},
		"reason without its lane": func(_ *KernelLane, candidate *Candidate) { candidate.KernelLane = nil },
	} {
		var copied Proposal
		if err = json.Unmarshal(original, &copied); err != nil {
			t.Fatal(err)
		}
		candidate := &copied.Candidates[len(copied.Candidates)-1]
		mutate(candidate.KernelLane, candidate)
		if ValidateProposal(copied) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestKernelSetupNeedsRecordedTraffic(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", kernelDiagnosis(true, operatorTime(gpuprofile.OperatorNorm, .41, floatPointer(.32)))))
	if err != nil {
		t.Fatal(err)
	}
	if order := reasonOrder(proposal); order[len(order)-1] == TechniqueCustomKernel {
		t.Fatalf("a run on a public sample planned a kernel: %v", order)
	}
	if skipped := plannedTechnique(t, proposal, TechniqueCustomKernel).Skipped; !strings.Contains(skipped, "recorded traffic") {
		t.Fatalf("skip reason = %q", skipped)
	}
}
