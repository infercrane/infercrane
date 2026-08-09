package capacity

import "testing"

func TestChoosePrefersWarmThenCost(t *testing.T) {
	cheap, expensive := 1.0, 2.0
	p, err := Choose(Requirement{GPU: "L4", Model: "qwen", Count: 1}, []Candidate{
		{Provider: "cheap", GPU: "L4", Available: 1, HourlyCost: &cheap, WarmModels: map[string]bool{}},
		{Provider: "warm", GPU: "L4", Available: 1, HourlyCost: &expensive, WarmModels: map[string]bool{"qwen": true}},
	})
	if err != nil || p.Candidate.Provider != "warm" {
		t.Fatalf("unexpected placement: %#v %v", p, err)
	}
}

func TestChooseRejectsInsufficientCapacity(t *testing.T) {
	_, err := Choose(Requirement{GPU: "L4", Count: 2}, []Candidate{{GPU: "L4", Available: 1}})
	if err == nil {
		t.Fatal("expected capacity error")
	}
}
