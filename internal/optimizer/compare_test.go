package optimizer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/spec"
)

const comparisonSpec = `apiVersion: infercrane.dev/v1
kind: Deployment
name: qwen-chat
model:
  id: Qwen/Qwen3-8B
  revision: b968826d9c46dd6066d109eabc6255188de91218
runtime:
  engine: vllm
  version: 0.22.1
  args: [%s]
resources:
  gpu: L40S
provider:
  cloud: runpod
`

func loadSpec(t *testing.T, body string) spec.Deployment {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := spec.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func specWithArgs(t *testing.T, args string) spec.Deployment {
	t.Helper()
	return loadSpec(t, strings.Replace(comparisonSpec, "%s", args, 1))
}

func TestCompareSetupsMeasuresExactlyTheCurrentAndTheProposedSetup(t *testing.T) {
	t.Parallel()
	current := specWithArgs(t, "--enable-prefix-caching")
	proposed := specWithArgs(t, "--enable-prefix-caching, --max-num-batched-tokens, '8192'")
	proposal, err := CompareSetups(Comparison{Current: current, Proposed: proposed, Objective: "latency"})
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatalf("the control plane would refuse it: %v", err)
	}
	if len(proposal.Candidates) != 2 || proposal.AlgorithmVersion != ComparisonAlgorithmVersion || proposal.Mutation != "none" {
		t.Fatalf("proposal %+v", proposal)
	}
	if proposal.StartingPoint == nil || proposal.StartingPoint.CandidateID != proposal.Candidates[0].ID {
		t.Fatal("the current setup must be the run's control")
	}
	if got := proposal.Candidates[1].Deployment.Runtime.Args; strings.Join(got, " ") != "--enable-prefix-caching --max-num-batched-tokens 8192" {
		t.Fatalf("the proposed setup must be measured exactly as written: %v", got)
	}
	if proposal.Input.Objective != "latency" || proposal.Input.WorkloadProfile != "interactive" || proposal.Input.MaxCandidates != 2 {
		t.Fatalf("input %+v", proposal.Input)
	}
	again, err := CompareSetups(Comparison{Current: current, Proposed: proposed, Objective: "latency"})
	if err != nil || again.Candidates[1].ID != proposal.Candidates[1].ID || again.InputDigest != proposal.InputDigest {
		t.Fatal("the same two setups must give the same proposal, so a retried CI job reuses its run")
	}
}

func TestCompareSetupsRefusesWhatOneRunCannotProve(t *testing.T) {
	t.Parallel()
	current := specWithArgs(t, "--enable-prefix-caching")
	otherGPU := strings.Replace(strings.Replace(comparisonSpec, "%s", "--enable-prefix-caching", 1), "gpu: L40S", "gpu: NVIDIA H100 80GB HBM3", 1)
	otherModel := strings.Replace(strings.Replace(comparisonSpec, "%s", "", 1), "Qwen/Qwen3-8B", "Qwen/Qwen3-14B", 1)
	floating := strings.Replace(comparisonSpec, "b968826d9c46dd6066d109eabc6255188de91218", "main", 1)
	cases := map[string]struct {
		current, proposed spec.Deployment
		want              string
	}{
		"hardware change":   {current, loadSpec(t, otherGPU), "changes resources.gpu"},
		"model change":      {current, loadSpec(t, otherModel), "migration Proof"},
		"nothing changed":   {current, specWithArgs(t, "--enable-prefix-caching"), "changes nothing a run measures"},
		"floating revision": {loadSpec(t, strings.Replace(floating, "%s", "", 1)), loadSpec(t, strings.Replace(floating, "%s", "--enable-prefix-caching", 1)), "immutable"},
	}
	for name, tc := range cases {
		if _, err := CompareSetups(Comparison{Current: tc.current, Proposed: tc.proposed}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %v, want %q", name, err, tc.want)
		}
	}
	renamed := current
	renamed.Name = "qwen-chat-renamed"
	if _, err := CompareSetups(Comparison{Current: current, Proposed: renamed}); err == nil {
		t.Fatal("a rename alone changes nothing a run measures")
	}
}
