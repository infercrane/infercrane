package optimizationcampaign

import (
	"encoding/json"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
)

func labelCandidate(id, engine string, args []string, features ...optimizer.Feature) optimizer.Candidate {
	candidate := optimizer.Candidate{ID: id, Features: features}
	candidate.Deployment.Runtime.Engine, candidate.Deployment.Runtime.Args = engine, args
	return candidate
}

func labelCampaign(t *testing.T, candidates ...optimizer.Candidate) domain.OptimizationCampaign {
	t.Helper()
	proposal, err := json.Marshal(optimizer.Proposal{Candidates: candidates})
	if err != nil {
		t.Fatal(err)
	}
	campaign := domain.OptimizationCampaign{ProposalJSON: string(proposal)}
	for index, candidate := range candidates {
		spec, _ := json.Marshal(candidate.Deployment)
		campaign.Candidates = append(campaign.Candidates, domain.OptimizationCandidateRun{ID: "run-" + candidate.ID, ProposalCandidateID: candidate.ID, Rank: index + 1, DeploymentSpecJSON: string(spec)})
	}
	return campaign
}

// The proposal of the 2026-11-17 Qwen3-0.6B run on an RTX 4090 described
// every setup in features[].source. The run page showed "vllm setup" twice.
func TestCandidateLabelsUseTheDescriptionTheProposalRecorded(t *testing.T) {
	campaign := labelCampaign(t,
		labelCandidate("defaults", "vllm", nil, optimizer.Feature{Name: "runtime_args", State: "baseline", Source: "Your current setup (vLLM defaults)"}),
		labelCandidate("ngram", "vllm", []string{"--speculative-config", `{"method": "ngram", "num_speculative_tokens": 4, "prompt_lookup_max": 4, "prompt_lookup_min": 2}`}, optimizer.Feature{Name: "runtime_args", State: "candidate", Source: "Speculative decoding (n-gram)"}),
		labelCandidate("fp8", "vllm", []string{"--kv-cache-dtype", "fp8", "--max-num-batched-tokens", "2048"}, optimizer.Feature{Name: "runtime_args", State: "candidate", Source: "FP8 KV cache, smaller prefill chunks"}),
	)
	labels := CandidateLabels(campaign)
	want := map[string]string{"run-defaults": "Your current setup (vLLM defaults)", "run-ngram": "Speculative decoding (n-gram)", "run-fp8": "FP8 KV cache, smaller prefill chunks"}
	for id, label := range want {
		if labels[id] != label {
			t.Fatalf("label[%s] = %q, want %q (all: %v)", id, labels[id], label, labels)
		}
	}
}

func TestCandidateLabelsNameRecognizedFlagsAndNeverGuessUnknownOnes(t *testing.T) {
	cases := []struct {
		name      string
		candidate optimizer.Candidate
		want      string
	}{
		{"n-gram speculation", labelCandidate("a", "vllm", []string{"--speculative-config", `{"method":"ngram","num_speculative_tokens":4}`}), "Speculative decoding (n-gram)"},
		{"fp8 kv and chunk", labelCandidate("a", "vllm", []string{"--kv-cache-dtype", "fp8", "--max-num-batched-tokens", "2048"}), "FP8 KV cache, prefill chunks of 2,048 tokens"},
		{"sglang nextn", labelCandidate("a", "sglang", []string{"--speculative-algorithm", "NEXTN", "--speculative-num-steps", "3", "--speculative-num-draft-tokens", "4", "--chunked-prefill-size=16384"}), "Speculative decoding (MTP, 4 draft tokens), prefill chunks of 16,384 tokens"},
		{"defaults", labelCandidate("a", "vllm", nil), "vLLM defaults"},
		{"custom image defaults", labelCandidate("a", "custom-oci", nil), "Custom image defaults"},
		{"unknown flag", labelCandidate("a", "vllm", []string{"--some-new-flag", "3"}), "vLLM --some-new-flag 3"},
		{"publisher checkpoint", labelCandidate("a", "vllm", nil, optimizer.Feature{Name: "quantization", State: "nvfp4", Source: "modelopt@0.39.0"}), "NVFP4 checkpoint"},
		{"digest is not a description", labelCandidate("a", "vllm", []string{"--enable-prefix-caching"}, optimizer.Feature{Name: "runtime_args", State: "pinned", Source: "sha256:0123 abc"}), "Prefix caching"},
		{"identifier is not a description", labelCandidate("a", "vllm", nil, optimizer.Feature{Name: "runtime_args", State: "pinned", Source: "vllm-0.22.1-prefix-cache-v1"}), "vLLM defaults"},
		{"other features never name a setup", labelCandidate("a", "vllm", nil, optimizer.Feature{Name: "flags_exercised_on_image", State: "not-performance-evidence", Source: "exercised on another model"}), "vLLM defaults"},
	}
	for _, test := range cases {
		if got := CandidateLabels(labelCampaign(t, test.candidate))["run-a"]; got != test.want {
			t.Fatalf("%s: label = %q, want %q", test.name, got, test.want)
		}
	}
	// Flags in the portable workload command count too.
	workload := labelCandidate("a", "custom-oci", nil)
	workload.Deployment.Runtime.Workload.Command = []string{"vllm", "serve", "${MODEL}", "--kv-cache-dtype", "fp8"}
	if got := CandidateLabels(labelCampaign(t, workload))["run-a"]; got != "FP8 KV cache" {
		t.Fatalf("workload command label = %q", got)
	}
}

func TestCandidateLabelsTellEqualSetupsApartAndSurviveAnUnreadableProposal(t *testing.T) {
	campaign := labelCampaign(t, labelCandidate("a", "vllm", nil), labelCandidate("b", "vllm", nil))
	labels := CandidateLabels(campaign)
	if labels["run-a"] != "vLLM defaults (setup 1)" || labels["run-b"] != "vLLM defaults (setup 2)" {
		t.Fatalf("equal labels were not told apart: %v", labels)
	}
	campaign = labelCampaign(t, labelCandidate("a", "vllm", []string{"--kv-cache-dtype", "fp8_e4m3"}))
	campaign.ProposalJSON = "not json"
	if got := CandidateLabels(campaign)["run-a"]; got != "FP8 KV cache" {
		t.Fatalf("label from the stored deployment spec = %q", got)
	}
	campaign.Candidates[0].DeploymentSpecJSON = "{}"
	if got := CandidateLabels(campaign)["run-a"]; got != "Setup 1" {
		t.Fatalf("label without any spec = %q", got)
	}
}
