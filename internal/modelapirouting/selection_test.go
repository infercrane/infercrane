package modelapirouting

import (
	"fmt"
	"testing"
	"time"
)

func TestCandidatesForRequestUsesStableWeightsAndKeepsFallbackOrder(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	route := routeFixture(now, "customer")
	runpod := route.Candidates[0]
	runpod.ID, runpod.TrafficWeightBPS = "runpod", 1_000
	upstream := route.Candidates[0]
	upstream.ID, upstream.OfferID, upstream.TrafficWeightBPS = "upstream", "upstream-offer", 9_000
	route.Candidates = []Candidate{runpod, upstream}
	lease := Lease{Entitlement: route.Entitlement, Publication: route.Publication, Rate: route.Rate, Candidates: route.Candidates}

	selected := map[string]int{}
	for index := 0; index < 10_000; index++ {
		ordered := lease.CandidatesForRequest(fmt.Sprintf("request-%d", index), "chat")
		if len(ordered) != 2 || ordered[0].ID == ordered[1].ID {
			t.Fatalf("invalid candidate order %#v", ordered)
		}
		selected[ordered[0].ID]++
	}
	if selected["runpod"] < 850 || selected["runpod"] > 1_150 {
		t.Fatalf("10%% canary selected %d times", selected["runpod"])
	}
	if first, second := lease.CandidatesForRequest("stable-id", "chat"), lease.CandidatesForRequest("stable-id", "chat"); first[0].ID != second[0].ID {
		t.Fatalf("selection was not stable: %s then %s", first[0].ID, second[0].ID)
	}
}

func TestPublishedRouteRejectsPartialWeightsAndAcceptsLegacyOrder(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	route := routeFixture(now, "customer")
	second := route.Candidates[0]
	second.ID, second.OfferID = "second", "second-offer"
	route.Candidates = append(route.Candidates, second)
	if err := validatePublishedRoute(route); err != nil {
		t.Fatalf("legacy unweighted route rejected: %v", err)
	}
	route.Candidates[0].TrafficWeightBPS = 500
	if err := validatePublishedRoute(route); err == nil {
		t.Fatal("partial weighted route was accepted")
	}
	route.Candidates[1].TrafficWeightBPS = 9_500
	if err := validatePublishedRoute(route); err != nil {
		t.Fatalf("complete weighted route rejected: %v", err)
	}
}

func TestCircuitBreakerOpensThenRecovers(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	breaker := NewCircuitBreaker(2, time.Minute)
	breaker.Observe("runpod", false, now)
	if !breaker.Allow("runpod", now) {
		t.Fatal("circuit opened before its threshold")
	}
	breaker.Observe("runpod", false, now)
	if breaker.Allow("runpod", now.Add(30*time.Second)) {
		t.Fatal("open circuit admitted traffic")
	}
	if !breaker.Allow("runpod", now.Add(time.Minute)) {
		t.Fatal("circuit did not admit a recovery request")
	}
	breaker.Observe("runpod", true, now.Add(time.Minute))
	if !breaker.Allow("runpod", now.Add(time.Minute)) {
		t.Fatal("successful recovery did not close the circuit")
	}
}
