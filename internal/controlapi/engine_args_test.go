package controlapi

import (
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/support"
)

func TestSubmittedProposalSetupsMeetTheEngineArgumentCheck(t *testing.T) {
	candidate := func(engine string, args ...string) optimizer.Candidate {
		var c optimizer.Candidate
		c.ID, c.Deployment.Runtime.Engine, c.Deployment.Runtime.Args = "setup-1", engine, args
		return c
	}
	if err := validateCandidateEngineArgs([]optimizer.Candidate{candidate("vllm", "--max-num-seqs", "256"), candidate("sglang", "--schedule-policy", "lpm")}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		candidate optimizer.Candidate
		want      string
	}{
		{candidate("vllm", "--trust-remote-code"), "setup setup-1: engine argument --trust-remote-code is refused"},
		{candidate("sglang", "--config", "/tmp/c.yaml"), "--config is not reviewed for sglang"},
	} {
		if err := validateCandidateEngineArgs([]optimizer.Candidate{test.candidate}); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("err=%v, want %q", err, test.want)
		}
	}
	custom := candidate("custom-oci", "--anything")
	custom.Deployment.Runtime.Workload = support.SGLangWorkload()
	if err := validateCandidateEngineArgs([]optimizer.Candidate{custom}); err != nil {
		t.Fatalf("custom image: %v", err)
	}
	custom.Deployment.Runtime.Workload.Command = append(custom.Deployment.Runtime.Workload.Command, "--enable-custom-logit-processor")
	if err := validateCandidateEngineArgs([]optimizer.Candidate{custom}); err == nil {
		t.Fatal("a custom command that turns on custom logit processors passed")
	}
}
