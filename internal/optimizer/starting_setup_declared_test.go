package optimizer

import (
	"strings"
	"testing"
)

// A starting setup is read from a deployment revision, or declared for an
// agent's endpoint, never both and never neither; a revision's setup keeps
// its stored form, so existing proposal digests do not change.
func TestStartingSetupIsARevisionOrADeclarationForAnAgentEndpoint(t *testing.T) {
	declared, err := DeclaredSetup{Engine: " vLLM ", Version: "0.34.0", Args: []string{"--max-num-seqs", "128"}, GPU: "NVIDIA-L4", GPUCount: 1}.StartingSetup("support-bot")
	if err != nil || declared.Endpoint != "support-bot" || declared.Engine != "vllm" || declared.Deployment != "" || declared.Revision != 0 {
		t.Fatalf("declared: %+v %v", declared, err)
	}
	if err = validateStartingSetup(StartingSetup{Deployment: "llama", Revision: 3, Engine: "vllm", Version: "0.34.0"}); err != nil {
		t.Fatalf("revision: %v", err)
	}
	for name, setup := range map[string]StartingSetup{
		"both":       {Deployment: "llama", Revision: 3, Endpoint: "support-bot", Engine: "vllm", Version: "0.34.0"},
		"neither":    {Engine: "vllm", Version: "0.34.0"},
		"no version": {Endpoint: "support-bot", Engine: "vllm"},
	} {
		if validateStartingSetup(setup) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	proposal := Proposal{Input: Request{StartingSetup: &declared}, Candidates: []Candidate{{ID: "control"}}}
	proposal.Candidates[0].Deployment.Runtime.Engine = "vllm"
	proposal.Candidates[0].Deployment.Resources.GPU = "NVIDIA-L4"
	if why := applyStartingSetup(&proposal); why != "" {
		t.Fatal(why)
	}
	control := proposal.Candidates[0]
	if strings.Join(control.Deployment.Runtime.Args, " ") != "--max-num-seqs 128" || control.Features[0].Source != "declared for endpoint support-bot" {
		t.Fatalf("control: %+v", control)
	}
}
