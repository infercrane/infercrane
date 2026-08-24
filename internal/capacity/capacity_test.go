package capacity

import (
	"slices"
	"testing"
	"time"
)

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

func TestChooseEnforcesWarmRequirementAndRejectsExpiredObservation(t *testing.T) {
	now := time.Now().UTC()
	_, err := Choose(Requirement{GPU: "L4", Model: "qwen", Count: 1, Warm: true}, []Candidate{{
		Provider: "stale", GPU: "L4", Available: 1,
		CacheObservations: map[string]CacheEvidence{"qwen": {State: "present", Source: "fixture", Samples: 2, ExpiresAt: now.Add(-time.Minute)}},
	}})
	if err == nil {
		t.Fatal("expired cache observation satisfied a required warm placement")
	}
}

func TestChooseUsesFreshEvidenceAndObjectiveWithoutDependingOnInputOrder(t *testing.T) {
	now := time.Now().UTC()
	cheapCost, reliableCost := 1.0, 1.2
	cheapSuccess, reliableSuccess := 0.85, 0.99
	cheapReady, reliableReady := 40.0, 55.0
	candidates := []Candidate{
		{Provider: "cheap", Region: "eu", GPU: "L4", Pool: "a", Available: 1, HourlyCost: &cheapCost, SuccessRate: &cheapSuccess, ReadyP95Seconds: &cheapReady, CacheObservations: map[string]CacheEvidence{"qwen": {State: "present", Source: "pvc", Samples: 4, ExpiresAt: now.Add(time.Hour)}}},
		{Provider: "reliable", Region: "eu", GPU: "L4", Pool: "b", Available: 1, HourlyCost: &reliableCost, SuccessRate: &reliableSuccess, ReadyP95Seconds: &reliableReady, CacheObservations: map[string]CacheEvidence{"qwen": {State: "present", Source: "snapshot", Samples: 8, ExpiresAt: now.Add(time.Hour)}}},
	}
	first, err := Choose(Requirement{GPU: "L4", Region: "eu", Model: "qwen", Count: 1, Objective: "reliability"}, candidates)
	if err != nil || first.Candidate.Provider != "reliable" || !slices.Contains(first.Evidence, "capacity:success-rate-observed") {
		t.Fatalf("unexpected reliability placement: %#v %v", first, err)
	}
	slices.Reverse(candidates)
	second, err := Choose(Requirement{GPU: "L4", Region: "eu", Model: "qwen", Count: 1, Objective: "reliability"}, candidates)
	if err != nil || second.Candidate.Provider != first.Candidate.Provider {
		t.Fatalf("placement depends on inventory order: first=%#v second=%#v err=%v", first, second, err)
	}
	cost, err := Choose(Requirement{GPU: "L4", Region: "eu", Model: "qwen", Count: 1, Objective: "cost"}, candidates)
	if err != nil || cost.Candidate.Provider != "cheap" {
		t.Fatalf("unexpected cost placement: %#v %v", cost, err)
	}
}

func TestChooseEnforcesMeasuredReadinessSLO(t *testing.T) {
	limit, fast, slow := 60.0, 45.0, 90.0
	placement, err := Choose(Requirement{GPU: "L4", Count: 1, MaxReadyP95Seconds: &limit}, []Candidate{
		{Provider: "unknown", GPU: "L4", Available: 1},
		{Provider: "slow", GPU: "L4", Available: 1, ReadyP95Seconds: &slow},
		{Provider: "fast", GPU: "L4", Available: 1, ReadyP95Seconds: &fast},
	})
	if err != nil || placement.Candidate.Provider != "fast" {
		t.Fatalf("readiness SLO selected %#v: %v", placement, err)
	}
}
