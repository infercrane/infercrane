package optimizer

import (
	"context"
	"strings"
	"testing"
)

func float(value float64) *float64 { return &value }

// A latency objective on the non-streaming buffered profile ranks on p95
// request latency. The proposal says so before anything is measured.
func TestProposalWarnsWhenLatencyObjectiveRanksOnRequestLatency(t *testing.T) {
	for _, objective := range []string{"interactive", "latency"} {
		proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: objective, WorkloadProfile: "buffered"})
		if err != nil {
			t.Fatal(err)
		}
		if !hasWarning(proposal.Warnings, "Objective "+objective+" ranks setups on p95 request latency on the buffered profile") {
			t.Fatalf("%s: warnings=%v", objective, proposal.Warnings)
		}
		if err = ValidateProposal(proposal); err != nil {
			t.Fatalf("%s: the warned proposal must stay valid: %v", objective, err)
		}
	}
	for _, request := range []Request{
		{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive"},
		{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "cost-efficiency", WorkloadProfile: "buffered"},
	} {
		proposal, err := catalogSource(t).Propose(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if hasWarning(proposal.Warnings, "ranks setups on") {
			t.Fatalf("%s on %q ranks on its usual metric: warnings=%v", request.Objective, request.WorkloadProfile, proposal.Warnings)
		}
	}
}

// A buffered run never measures time to first token or time per output
// token, so a limit on either would reject every setup after the whole
// measurement. It is refused before anything is proposed.
func TestProposeRejectsLimitsTheProfileCannotMeasure(t *testing.T) {
	base := Request{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive", WorkloadProfile: "buffered"}
	for name, test := range map[string]struct {
		ttft, tpot *float64
		named      string
	}{
		"ttft": {ttft: float(200), named: "max_ttft_p95_ms cannot be measured on the buffered profile"},
		"tpot": {tpot: float(25), named: "max_tpot_p95_ms cannot be measured on the buffered profile"},
		"both": {ttft: float(200), tpot: float(25), named: "max_ttft_p95_ms and max_tpot_p95_ms cannot be measured"},
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			request.MaxTTFTP95MS, request.MaxTPOTP95MS = test.ttft, test.tpot
			_, err := catalogSource(t).Propose(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), test.named) || !strings.Contains(err.Error(), "use a streaming profile such as interactive") {
				t.Fatalf("err=%v", err)
			}
			if err = ValidateMeasurable(request); err == nil {
				t.Fatal("ValidateMeasurable accepted an unmeasurable limit")
			}
		})
	}
	streaming := base
	streaming.WorkloadProfile, streaming.MaxTTFTP95MS, streaming.MaxTPOTP95MS = "interactive", float(200), float(25)
	if err := ValidateMeasurable(streaming); err != nil {
		t.Fatalf("a streaming profile measures both limits: %v", err)
	}
	// Without --profile, the interactive objective uses the streaming
	// interactive profile.
	streaming.WorkloadProfile = ""
	if _, err := catalogSource(t).Propose(context.Background(), streaming); err != nil {
		t.Fatalf("default profile: %v", err)
	}
	buffered := base
	buffered.MaxErrorRate, buffered.MinOutputTokensSecond = float(0.01), float(5)
	if err := ValidateMeasurable(buffered); err != nil {
		t.Fatalf("buffered runs measure error rate and throughput: %v", err)
	}
}

func hasWarning(warnings []string, fragment string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, fragment) {
			return true
		}
	}
	return false
}
