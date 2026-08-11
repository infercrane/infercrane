package decision

import (
	"math"
	"testing"
	"time"
)

func pointer(v float64) *float64 { return &v }

func TestRecommendIsDeterministicAndRejectsSLOViolations(t *testing.T) {
	policy := SLOPolicy{MaxTTFTP95MS: pointer(250), MaxErrorRate: pointer(.01), MinOutputTokensSecond: pointer(20)}
	evidence := []Evidence{
		{ID: "slow", Provider: "a", Runtime: "vllm", Qualified: true, ComparableModel: true, ComparableWorkload: true, Requests: 100, Failed: 0, TTFTP95MS: pointer(400), OutputTokensSecond: pointer(50)},
		{ID: "good", Provider: "b", Runtime: "vllm", Qualified: true, ComparableModel: true, ComparableWorkload: true, Requests: 100, Failed: 0, TTFTP95MS: pointer(200), OutputTokensSecond: pointer(30)},
	}
	result := Recommend(policy, evidence)
	if result.Status != "recommended" || result.SelectedEvidence != "good" || result.AlgorithmVersion != AlgorithmVersion {
		t.Fatalf("unexpected result: %#v", result)
	}
	want, err := Snapshot(policy, evidence, result)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again := Recommend(policy, evidence)
		got, snapshotErr := Snapshot(policy, evidence, again)
		if snapshotErr != nil || got != want {
			t.Fatal("recommendation is not reproducible")
		}
	}
}

func TestRecommendKeepsMissingCostUnknown(t *testing.T) {
	result := Recommend(SLOPolicy{MaxHourlyCost: pointer(2)}, []Evidence{{ID: "bench", Qualified: true, ComparableModel: true, ComparableWorkload: true, Requests: 10, CreatedAt: time.Now()}})
	if result.Status != "unknown" || len(result.Missing) != 1 || result.Missing[0] != "trustworthy_hourly_cost" {
		t.Fatalf("missing cost was fabricated: %#v", result)
	}
}

func TestPolicyValidation(t *testing.T) {
	if (SLOPolicy{}).Validate() == nil {
		t.Fatal("empty policy accepted")
	}
	if (SLOPolicy{MaxErrorRate: pointer(1.1)}).Validate() == nil {
		t.Fatal("invalid error rate accepted")
	}
	if (SLOPolicy{MaxTTFTP95MS: pointer(200)}).Validate() != nil {
		t.Fatal("valid policy rejected")
	}
	nan := pointer(math.NaN())
	if (SLOPolicy{MaxTTFTP95MS: nan}).Validate() == nil {
		t.Fatal("non-finite policy accepted")
	}
}

func TestRecommendationRefusesUnlikeModelOrWorkloadEvidence(t *testing.T) {
	policy := SLOPolicy{MaxTTFTP95MS: pointer(250)}
	result := Recommend(policy, []Evidence{{ID: "wrong-model", Qualified: true, ComparableWorkload: true, Requests: 10, TTFTP95MS: pointer(100)}, {ID: "wrong-workload", Qualified: true, ComparableModel: true, Requests: 10, TTFTP95MS: pointer(100)}})
	if result.Status != "unknown" || len(result.Missing) != 2 {
		t.Fatalf("incomparable evidence selected: %#v", result)
	}
}
