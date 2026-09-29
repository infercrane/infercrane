package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/managedbilling"
)

func TestManagedSandboxReservationIsIdempotentAndSettlesActiveTime(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "sandbox-billing-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Sandbox Billing"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditManagedWallet(ctx, tenant, "sandbox-credit-"+suffix, "test credit", 25_000_000); err != nil {
		t.Fatal(err)
	}
	row, _, err := s.CreateNativeSandbox(ctx, domain.NativeSandbox{
		TenantID: tenant, CreatedBy: "tester", DisplayName: "Paid agent", Purpose: "coding_agent",
		SourceType: "empty_workspace", TemplateID: "base", Status: "creating_workspace",
		IdempotencyKey: "sandbox-create-" + suffix, InputDigest: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := managedbilling.SandboxPolicy{Enabled: true, SupplierHourlyMicrousd: 180_000, ActiveHourlyMicrousd: 300_000}
	reservationInput, err := policy.Reservation(time.Hour, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	reservation, created, err := s.ReserveManagedSandboxSpend(ctx, tenant, row.ID, reservationInput)
	if err != nil || !created || reservation.ID == "" || reservation.ReservedMicrousd != 300_000 {
		t.Fatalf("reservation=%+v created=%t err=%v", reservation, created, err)
	}
	replayed, created, err := s.ReserveManagedSandboxSpend(ctx, tenant, row.ID, reservationInput)
	if err != nil || created || replayed.ID != reservation.ID {
		t.Fatalf("replayed=%+v created=%t err=%v", replayed, created, err)
	}
	wallet, err := s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.ReservedMicrousd != 300_000 || wallet.AvailableMicrousd != 24_700_000 {
		t.Fatalf("reserved wallet=%+v err=%v", wallet, err)
	}
	if err = s.ActivateManagedSandboxSpend(ctx, tenant, row.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = s.SettleManagedSandboxSpend(ctx, tenant, row.ID, int64((15*time.Minute)/time.Millisecond), "metered sandbox runtime"); err != nil {
		t.Fatal(err)
	}
	settled, err := s.ManagedSpendReservation(ctx, tenant, "sandbox", row.ID)
	if err != nil || settled.State != "settled" || settled.ActualMicrousd != 75_000 {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	wallet, err = s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.ReservedMicrousd != 0 || wallet.BalanceMicrousd != 24_925_000 {
		t.Fatalf("settled wallet=%+v err=%v", wallet, err)
	}
	if err = s.SettleManagedSandboxSpend(ctx, tenant, row.ID, int64(time.Hour/time.Millisecond), "replay"); err != nil {
		t.Fatal(err)
	}
	walletAfterReplay, err := s.ManagedWallet(ctx, tenant)
	if err != nil || walletAfterReplay != wallet {
		t.Fatalf("settlement replay changed wallet: before=%+v after=%+v err=%v", wallet, walletAfterReplay, err)
	}
}

func TestSameJSONDocumentIgnoresJSONBFormatting(t *testing.T) {
	if !sameJSONDocument(`{"active":300000,"quota":{"active":1,"retained":10}}`, `{ "quota": { "retained": 10, "active": 1 }, "active": 300000 }`) {
		t.Fatal("semantically equivalent pricing documents should match")
	}
	if sameJSONDocument(`{"active":300000}`, `{"active":300001}`) {
		t.Fatal("different pricing documents must not match")
	}
}

func TestManagedSandboxReservationRequiresCreditAndExistingSandbox(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "sandbox-poor-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Sandbox Without Credit"); err != nil {
		t.Fatal(err)
	}
	policy := managedbilling.SandboxPolicy{Enabled: true, SupplierHourlyMicrousd: 180_000, ActiveHourlyMicrousd: 300_000}
	missing, err := policy.Reservation(time.Hour, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReserveManagedSandboxSpend(ctx, tenant, "missing", missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing sandbox error=%v", err)
	}
	row, _, err := s.CreateNativeSandbox(ctx, domain.NativeSandbox{
		TenantID: tenant, CreatedBy: "tester", DisplayName: "Unfunded", Purpose: "blank_computer",
		SourceType: "empty_workspace", TemplateID: "base", Status: "creating_workspace",
		IdempotencyKey: "sandbox-poor-" + suffix, InputDigest: strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := policy.Reservation(time.Hour, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReserveManagedSandboxSpend(ctx, tenant, row.ID, reservation); !errors.Is(err, domain.ErrInsufficientCredits) {
		t.Fatalf("unfunded sandbox error=%v", err)
	}
}

func TestManagedSandboxReservationEnforcesServerOwnedCapacity(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "sandbox-capacity-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Sandbox Capacity"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditManagedWallet(ctx, tenant, "sandbox-capacity-credit-"+suffix, "test credit", 25_000_000); err != nil {
		t.Fatal(err)
	}
	policy := managedbilling.SandboxPolicy{Enabled: true, SupplierHourlyMicrousd: 180_000, ActiveHourlyMicrousd: 300_000, MaxActive: 1, MaxRetained: 10}
	for index := 0; index < 2; index++ {
		row, _, err := s.CreateNativeSandbox(ctx, domain.NativeSandbox{
			TenantID: tenant, CreatedBy: "tester", DisplayName: "Capacity", Purpose: "coding_agent",
			SourceType: "empty_workspace", TemplateID: "base", Status: "creating_workspace",
			IdempotencyKey: fmt.Sprintf("capacity-%s-%d", suffix, index), InputDigest: strings.Repeat(fmt.Sprintf("%x", index+1), 64),
		})
		if err != nil {
			t.Fatal(err)
		}
		reservation, err := policy.Reservation(time.Hour, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = s.ReserveManagedSandboxSpend(ctx, tenant, row.ID, reservation)
		if index == 0 && err != nil {
			t.Fatalf("first reservation: %v", err)
		}
		if index == 1 && !errors.Is(err, ErrConflict) {
			t.Fatalf("second active reservation error=%v", err)
		}
	}
}

func TestNativeSandboxLifecycleTransitionMetersExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "sandbox-transition-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Sandbox Transition"); err != nil {
		t.Fatal(err)
	}
	row, _, err := s.CreateNativeSandbox(ctx, domain.NativeSandbox{
		TenantID: tenant, CreatedBy: "tester", DisplayName: "Transition", Purpose: "coding_agent",
		SourceType: "empty_workspace", TemplateID: "base", Status: "running",
		IdempotencyKey: "transition-" + suffix, InputDigest: strings.Repeat("c", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Add(-10 * time.Minute)
	if _, err = s.ExecContext(ctx, `UPDATE native_sandboxes SET billing_state_since=? WHERE tenant_id=? AND id=?`, startedAt, tenant, row.ID); err != nil {
		t.Fatal(err)
	}
	endedAt := startedAt.Add(10 * time.Minute)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err = s.RecordNativeSandboxTransition(ctx, tenant, row.ID, "deleted", "", "provider-delete", "sandbox.delete", endedAt); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	milliseconds, err := s.SandboxRunningMilliseconds(ctx, tenant, row.ID)
	if err != nil || milliseconds != int64((10*time.Minute)/time.Millisecond) {
		t.Fatalf("running milliseconds=%d err=%v", milliseconds, err)
	}
}
