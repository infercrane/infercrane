package optimizer

import (
	"context"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/support"
)

func tunedKernelSet(t *testing.T, version, device string) *domain.KernelConfigSet {
	t.Helper()
	shape := kerneltuning.FusedMoEShape{Experts: 256, Intermediate: 512, DType: "fp8_w8a8", BlockN: 128, BlockK: 128}
	file, err := kerneltuning.NewFile(shape, device, map[int]kerneltuning.TileConfig{
		128: {BlockSizeM: 16, BlockSizeN: 64, BlockSizeK: 128, GroupSizeM: 1, NumWarps: 4, NumStages: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	set := kerneltuning.NewSet("vllm", version, device, []domain.KernelConfigFile{file})
	return &set
}

func TestATunedSetAddsTheStartingSetupWithIt(t *testing.T) {
	for name, diagnosis := range map[string]*Diagnosis{"without a diagnosis": nil, "with a diagnosis": decodeDiagnosis()} {
		t.Run(name, func(t *testing.T) {
			request := genericRequest("H100", diagnosis)
			request.MaxCandidates = 10
			control, err := catalogSource(t).Propose(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			version := control.Candidates[0].Deployment.Runtime.Version
			request.KernelConfigs = tunedKernelSet(t, version, "NVIDIA_H100_80GB_HBM3")
			proposal, err := catalogSource(t).Propose(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateProposal(proposal); err != nil {
				t.Fatal(err)
			}
			if len(proposal.Candidates) != len(control.Candidates)+1 {
				t.Fatalf("candidates = %d, want %d", len(proposal.Candidates), len(control.Candidates)+1)
			}
			tuned := proposal.Candidates[len(proposal.Candidates)-1]
			start := proposal.Candidates[0]
			if tuned.Reason == nil || tuned.Reason.Technique != TechniqueTunedKernels || tuned.Reason.Label != "Kernels tuned for your shapes" || tuned.Rank != len(proposal.Candidates) {
				t.Fatalf("tuned candidate = %+v", tuned)
			}
			if tuned.Deployment.Runtime.KernelConfigs == nil || tuned.Deployment.Runtime.KernelConfigs.Digest != request.KernelConfigs.Digest || start.Deployment.Runtime.KernelConfigs != nil {
				t.Fatal("only the tuned candidate carries the set")
			}
			if strings.Join(tuned.Deployment.Runtime.Args, " ") != strings.Join(start.Deployment.Runtime.Args, " ") || tuned.Deployment.Runtime.Version != start.Deployment.Runtime.Version {
				t.Fatal("the tuned candidate changes more than the kernels")
			}
			if !strings.Contains(strings.Join(tuned.RequiredEvidence, "; "), TaskScoreEvidence) {
				t.Fatalf("a kernel change needs a task score: %v", tuned.RequiredEvidence)
			}
		})
	}
}

func TestATunedSetThatDoesNotFitAddsNothing(t *testing.T) {
	request := genericRequest("H100", nil)
	request.MaxCandidates = 10
	// Proposals run the current release; a set for the older one does not fit.
	request.KernelConfigs = tunedKernelSet(t, support.LegacyVLLMRuntimeVersion, "NVIDIA_H100_80GB_HBM3")
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range proposal.Candidates {
		if candidate.Deployment.Runtime.KernelConfigs != nil {
			t.Fatal("a set for another engine version became a candidate")
		}
	}
	if !strings.Contains(strings.Join(proposal.Warnings, " "), "The tuned kernel set was left out") {
		t.Fatalf("warnings = %v", proposal.Warnings)
	}

	// A set for another GPU, or a tampered one, is refused with the request.
	for name, set := range map[string]*domain.KernelConfigSet{
		"another GPU": tunedKernelSet(t, support.DefaultRuntimeVersion, "NVIDIA_H200"),
		"tampered": func() *domain.KernelConfigSet {
			set := tunedKernelSet(t, support.DefaultRuntimeVersion, "NVIDIA_H100_80GB_HBM3")
			set.Files[0].Content = strings.Replace(set.Files[0].Content, `"num_stages":3`, `"num_stages":4`, 1)
			return set
		}(),
	} {
		request.KernelConfigs = set
		if _, err = catalogSource(t).Propose(context.Background(), request); err == nil || !strings.Contains(err.Error(), "kernel_configs") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestProposalValidationRejectsACandidateSetThatDoesNotFit(t *testing.T) {
	request := genericRequest("H100", nil)
	request.MaxCandidates = 10
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	tampered := proposal
	tampered.Candidates = append([]Candidate(nil), proposal.Candidates...)
	tampered.Candidates[0].Deployment.Runtime.KernelConfigs = tunedKernelSet(t, support.LegacyVLLMRuntimeVersion, "NVIDIA_H100_80GB_HBM3")
	if err = ValidateProposal(tampered); err == nil || !strings.Contains(err.Error(), "kernel configs") {
		t.Fatalf("err = %v", err)
	}
}
