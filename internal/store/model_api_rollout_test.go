package store

import (
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/modelapisupply"
)

func TestValidateModelAPIRolloutWeights(t *testing.T) {
	plan := modelapisupply.Plan{
		Primary:   &modelapisupply.Selection{CandidateID: "upstream"},
		Fallbacks: []modelapisupply.Selection{{CandidateID: "runpod"}},
	}
	tests := map[string]struct {
		references []SupplyCandidateReference
		want       string
	}{
		"legacy order":        {references: []SupplyCandidateReference{{CandidateID: "upstream"}, {CandidateID: "runpod"}}},
		"ten percent canary":  {references: []SupplyCandidateReference{{CandidateID: "upstream", TrafficWeightBPS: 9_000}, {CandidateID: "runpod", TrafficWeightBPS: 1_000}}},
		"partial allocation":  {references: []SupplyCandidateReference{{CandidateID: "upstream", TrafficWeightBPS: 9_000}, {CandidateID: "runpod"}}, want: "total 10000"},
		"rejected allocation": {references: []SupplyCandidateReference{{CandidateID: "upstream", TrafficWeightBPS: 9_000}, {CandidateID: "rejected", TrafficWeightBPS: 1_000}}, want: "rejected"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateModelAPIRolloutWeights(plan, test.references)
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
		})
	}
}
