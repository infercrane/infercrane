package managedbilling

import (
	"testing"
	"time"
)

func TestSandboxPolicyBuildsMaximumLifetimeHold(t *testing.T) {
	policy := SandboxPolicy{Enabled: true, SupplierHourlyMicrousd: 180_000, ActiveHourlyMicrousd: 300_000}
	reservation, err := policy.Reservation(90*time.Minute, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if reservation.ResourceType != "sandbox" || reservation.ResourceName != "sandbox-a" || reservation.ReservedMicrousd != 450_000 || reservation.GrossMarginBPS != 4_000 || reservation.RuntimeLimitSeconds != 5_400 {
		t.Fatalf("reservation=%+v", reservation)
	}
}

func TestSandboxPolicyRejectsUnprofitableOrDisabledOffers(t *testing.T) {
	if _, err := (SandboxPolicy{}).Reservation(time.Hour, "sandbox-a"); err == nil {
		t.Fatal("disabled policy was accepted")
	}
	if _, err := (SandboxPolicy{Enabled: true, SupplierHourlyMicrousd: 400_000, ActiveHourlyMicrousd: 300_000}).Reservation(time.Hour, "sandbox-a"); err == nil {
		t.Fatal("retail price below supplier cost was accepted")
	}
}
