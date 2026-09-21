package managedbilling

import (
	"testing"
	"time"
)

func TestQuoteDeploymentPreservesMarginAndHoldsCleanupAllowance(t *testing.T) {
	now := time.Now().UTC()
	quote, err := QuoteDeployment(DeploymentPolicy{Enabled: true}, "runpod", "EU", "L40S", "runpod.gpuTypes.lowestPrice", 1, 1.09, time.Hour, now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if quote.SupplierHourlyMicrousd != 1_090_000 || quote.RetailHourlyMicrousd != 1_816_667 {
		t.Fatalf("unexpected quote: %+v", quote)
	}
	want, err := DurationCostMicrousd(quote.RetailHourlyMicrousd, time.Hour+10*time.Minute)
	if err != nil || quote.ReservedMicrousd != want {
		t.Fatalf("reserve=%d want=%d err=%v", quote.ReservedMicrousd, want, err)
	}
}

func TestQuoteDeploymentFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	policy := DeploymentPolicy{Enabled: true}
	for name, mutate := range map[string]func(*DeploymentPolicy, *time.Time, *time.Duration){
		"disabled": func(policy *DeploymentPolicy, _ *time.Time, _ *time.Duration) { policy.Enabled = false },
		"stale":    func(_ *DeploymentPolicy, valid *time.Time, _ *time.Duration) { *valid = now.Add(-time.Second) },
		"too long": func(_ *DeploymentPolicy, _ *time.Time, runtime *time.Duration) { *runtime = 25 * time.Hour },
	} {
		t.Run(name, func(t *testing.T) {
			candidate, valid, runtime := policy, now.Add(time.Minute), time.Hour
			mutate(&candidate, &valid, &runtime)
			if _, err := QuoteDeployment(candidate, "runpod", "", "L40S", "provider", 1, 1, runtime, now, valid); err == nil {
				t.Fatal("expected closed quote")
			}
		})
	}
}
