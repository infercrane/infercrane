package intentplan

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/pricing"
)

func testPlanner(t *testing.T) Planner {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	return Planner{
		Compatibility: registry.Snapshot().Compatibility,
		Providers: []Provider{
			{ID: "aws", Label: "AWS", State: "connection-required", Reason: "AWS workload identity is not configured"},
			{ID: "gcp", Label: "Google Cloud", State: "connection-required", Reason: "GCP project and workload identity are not configured"},
			{ID: "runpod", Label: "RunPod", State: "ready"},
		},
		Prices: map[pricing.Request]pricing.Estimate{
			{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}: {Currency: "USD", Hourly: .74, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "https://api.runpod.io/graphql", ObservedAt: now.Add(-time.Minute), StaleAfter: time.Hour},
			{Cloud: "gcp", Region: "us-central1", GPU: "nvidia-l4", GPUCount: 1, Replicas: 1}: {Currency: "USD", Hourly: .67, CostScope: pricing.CostScopeAcceleratorOnly, Authority: pricing.PriceAuthorityProviderAPI, Source: "https://cloudbilling.googleapis.com/v1/services/6F81-5844-456A/skus", ObservedAt: now.Add(-time.Minute), StaleAfter: time.Hour},
		},
		Now: func() time.Time { return now },
	}
}

func TestPlanResolvesSimpleIntentIntoEditableReviewedConfiguration(t *testing.T) {
	plan, err := testPlanner(t).Plan(Request{Intent: "Deploy Qwen/Qwen3-8B for low latency on RunPod"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "ready" || plan.Mutation != "none" || plan.Model == nil || plan.Model.Repository != "Qwen/Qwen3-8B" {
		t.Fatalf("plan identity=%+v", plan)
	}
	if plan.Interpretation.Action != "deploy" || plan.Interpretation.Objective != "latency" {
		t.Fatalf("interpretation=%+v", plan.Interpretation)
	}
	if plan.Configuration == nil || plan.Configuration.Provider != "runpod" || plan.Configuration.ProviderAdapter != "runpod-pods" || plan.Configuration.Profile != "vllm-interactive" || plan.Configuration.GPU != "L40S" {
		t.Fatalf("configuration=%+v", plan.Configuration)
	}
	if plan.Evidence.Performance != "unmeasured" || plan.Evidence.Capacity == "available" || plan.Evidence.Price.HourlyUSDPerReplica == nil || *plan.Evidence.Price.HourlyUSDPerReplica != .74 || !plan.Evidence.Price.DeploymentComparable {
		t.Fatalf("evidence=%+v", plan.Evidence)
	}
	if len(plan.Choices) < 7 || len(plan.Architecture.Nodes) < 5 || len(plan.Architecture.Edges) < 4 {
		t.Fatalf("editable plan is incomplete: choices=%+v architecture=%+v", plan.Choices, plan.Architecture)
	}
}

func TestPlanDoesNotSubstituteUnknownOrAmbiguousModel(t *testing.T) {
	planner := testPlanner(t)
	for _, request := range []Request{
		{Intent: "Plan Qwen/Qwen3.5-35B-A3B on RunPod"},
		{Intent: "Compare Qwen/Qwen3-8B with mistralai/Mistral-7B-Instruct-v0.3"},
	} {
		plan, err := planner.Plan(request)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status != "needs_input" || plan.Model != nil || plan.Configuration != nil || len(plan.Missing) != 1 || plan.Missing[0].Field != "model" || len(plan.Missing[0].Options) == 0 || len(plan.Missing[0].Options) > 8 {
			t.Fatalf("unsupported model was silently resolved: %+v", plan)
		}
	}
}

func TestPlanRequiresProviderChoiceWhenSeveralReadyConnectionsMatch(t *testing.T) {
	planner := testPlanner(t)
	planner.Providers[0].State = "ready"
	plan, err := planner.Plan(Request{Intent: "Deploy Qwen/Qwen3-8B"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "needs_input" || plan.Configuration == nil || plan.Configuration.Provider != "" || !hasMissing(plan, "provider") {
		t.Fatalf("ambiguous provider was silently selected: %+v", plan)
	}
}

func TestPlanSurfacesConnectionAndRegionWithoutTreatingPriceAsCapacity(t *testing.T) {
	plan, err := testPlanner(t).Plan(Request{Intent: "Deploy Qwen/Qwen3-8B on Google Cloud"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "needs_input" || plan.Configuration == nil || plan.Configuration.Provider != "gcp" || !hasMissing(plan, "provider_connection") || !hasMissing(plan, "region") {
		t.Fatalf("provider requirements=%+v", plan)
	}
	if plan.Evidence.Capacity != "unknown until a bounded provider probe or accepted launch" || plan.Evidence.Price.State != "unavailable" {
		t.Fatalf("missing region fabricated deployability or price: %+v", plan.Evidence)
	}
}

func TestPlanFailsClosedWhenRequestedModeHasNoReviewedProfile(t *testing.T) {
	plan, err := testPlanner(t).Plan(Request{Intent: "Deploy Qwen/Qwen3-8B serverless", Provider: "runpod"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "needs_input" || plan.Configuration != nil || !hasMissing(plan, "compute_mode") {
		t.Fatalf("unreviewed serverless profile was emitted: %+v", plan)
	}
}

func TestPlanIsDeterministicAndBounded(t *testing.T) {
	planner := testPlanner(t)
	request := Request{Intent: "Optimize Qwen/Qwen3-8B for throughput", Provider: "runpod"}
	first, err := planner.Plan(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := planner.Plan(request)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("plans differ: first=%+v second=%+v err=%v", first, second, err)
	}
	if first.Interpretation.Action != "optimize" || first.Interpretation.Objective != "throughput" || first.Configuration == nil || first.Configuration.Profile != "vllm-throughput" {
		t.Fatalf("optimization intent=%+v", first)
	}
	if _, err = planner.Plan(Request{Intent: strings.Repeat("x", 2049)}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized intent err=%v", err)
	}
}

func hasMissing(plan Plan, field string) bool {
	for _, missing := range plan.Missing {
		if missing.Field == field {
			return true
		}
	}
	return false
}
