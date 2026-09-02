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

func TestManagedPaymentWebhookIsAtomicAndSessionIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "managed-payment-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Managed Payment"); err != nil {
		t.Fatal(err)
	}
	fundingIntentID, checkoutSessionID := completeFundingIntentForTest(t, s, tenant, suffix, 50_000_000)
	ignored, err := s.ProcessManagedPaymentEvent(ctx, domain.ManagedPaymentEvent{Provider: "stripe", EventID: "evt-unpaid-" + suffix, EventType: "checkout.session.completed", PayloadDigest: strings.Repeat("a", 64), Apply: false, IgnoreReason: "payment is not paid", MetadataJSON: `{}`})
	if err != nil || ignored.Status != "ignored" || ignored.CreditApplied {
		t.Fatalf("ignored=%+v err=%v", ignored, err)
	}
	payment := domain.ManagedPaymentEvent{Provider: "stripe", EventID: "evt-paid-" + suffix, EventType: "checkout.session.async_payment_succeeded", PayloadDigest: strings.Repeat("b", 64), TenantID: tenant, FundingIntentID: fundingIntentID, SessionID: checkoutSessionID, PaymentIntentID: "pi-" + suffix, AmountMicrousd: 50_000_000, Currency: "USD", Operation: "credit", Apply: true, MetadataJSON: `{"livemode":false}`}
	first, err := s.ProcessManagedPaymentEvent(ctx, payment)
	if err != nil || first.Status != "applied" || !first.CreditApplied || first.Wallet == nil || first.Wallet.BalanceMicrousd != 50_000_000 || first.LedgerEntry == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	replayed, err := s.ProcessManagedPaymentEvent(ctx, payment)
	if err != nil || replayed.Status != "applied" || replayed.CreditApplied {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	secondEvent := payment
	secondEvent.EventID = "evt-second-" + suffix
	secondEvent.PayloadDigest = strings.Repeat("c", 64)
	second, err := s.ProcessManagedPaymentEvent(ctx, secondEvent)
	if err != nil || second.Status != "applied" || second.CreditApplied || second.Wallet == nil || second.Wallet.BalanceMicrousd != 50_000_000 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	conflict := payment
	conflict.EventID = "evt-conflict-" + suffix
	conflict.PayloadDigest = strings.Repeat("d", 64)
	conflict.AmountMicrousd = 100_000_000
	if _, err = s.ProcessManagedPaymentEvent(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting checkout intent error=%v", err)
	}
	wallet, err := s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.BalanceMicrousd != 50_000_000 {
		t.Fatalf("wallet=%+v err=%v", wallet, err)
	}
}

func TestManagedPaymentRefundPreservesReservationsAndCreatesDebt(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "managed-refund-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Managed Refund"); err != nil {
		t.Fatal(err)
	}
	fundingIntentID, checkoutSessionID := completeFundingIntentForTest(t, s, tenant, suffix, 50_000_000)
	payment := domain.ManagedPaymentEvent{Provider: "stripe", EventID: "evt-credit-" + suffix, EventType: "checkout.session.completed", PayloadDigest: strings.Repeat("a", 64), TenantID: tenant, FundingIntentID: fundingIntentID, SessionID: checkoutSessionID, PaymentIntentID: "pi-" + suffix, AmountMicrousd: 50_000_000, Currency: "USD", Operation: "credit", Apply: true, MetadataJSON: `{}`}
	if result, err := s.ProcessManagedPaymentEvent(ctx, payment); err != nil || !result.CreditApplied {
		t.Fatalf("credit=%+v err=%v", result, err)
	}
	// Represent $30 already settled and $20 still protected by an active
	// reservation. A provider refund must not steal that reservation.
	if _, err := s.ExecContext(ctx, `UPDATE managed_wallets SET balance_microusd=20000000,reserved_microusd=20000000 WHERE tenant_id=?`, tenant); err != nil {
		t.Fatal(err)
	}
	refund := domain.ManagedPaymentEvent{Provider: "stripe", EventID: "evt-refund-" + suffix, EventType: "charge.refunded", PayloadDigest: strings.Repeat("b", 64), PaymentIntentID: payment.PaymentIntentID, RefundedMicrousd: 50_000_000, Currency: "USD", Operation: "refund", Apply: true, MetadataJSON: `{}`}
	result, err := s.ProcessManagedPaymentEvent(ctx, refund)
	if err != nil || !result.RefundApplied || result.Wallet == nil || result.Wallet.BalanceMicrousd != 20_000_000 || result.Wallet.ReservedMicrousd != 20_000_000 || result.Wallet.DebtMicrousd != 50_000_000 || result.Wallet.AvailableMicrousd != 0 {
		t.Fatalf("refund=%+v err=%v", result, err)
	}
	if replay, replayErr := s.ProcessManagedPaymentEvent(ctx, refund); replayErr != nil || replay.RefundApplied {
		t.Fatalf("replayed refund=%+v err=%v", replay, replayErr)
	}
	sameTotal := refund
	sameTotal.EventID = "evt-refund-retry-" + suffix
	sameTotal.PayloadDigest = strings.Repeat("c", 64)
	if retry, retryErr := s.ProcessManagedPaymentEvent(ctx, sameTotal); retryErr != nil || retry.RefundApplied {
		t.Fatalf("same cumulative refund=%+v err=%v", retry, retryErr)
	}
	tooLarge := refund
	tooLarge.EventID = "evt-refund-too-large-" + suffix
	tooLarge.PayloadDigest = strings.Repeat("d", 64)
	tooLarge.RefundedMicrousd = 50_000_001
	if _, err = s.ProcessManagedPaymentEvent(ctx, tooLarge); !errors.Is(err, ErrConflict) {
		t.Fatalf("refund ceiling error=%v", err)
	}
	// When the reservation finishes, its unused portion automatically pays
	// down the refund debt before any money becomes spendable again.
	if _, err = s.ExecContext(ctx, `UPDATE managed_wallets SET reserved_microusd=0 WHERE tenant_id=?`, tenant); err != nil {
		t.Fatal(err)
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = reconcileManagedDebtTx(ctx, tx, tenant, now()); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	wallet, err := s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.BalanceMicrousd != 0 || wallet.DebtMicrousd != 30_000_000 || wallet.AvailableMicrousd != 0 {
		t.Fatalf("reconciled wallet=%+v err=%v", wallet, err)
	}
	if wallet, err = s.CreditManagedWallet(ctx, tenant, "operator-credit-a-"+suffix, "debt repayment", 25_000_000); err != nil || wallet.BalanceMicrousd != 0 || wallet.DebtMicrousd != 5_000_000 {
		t.Fatalf("partial debt repayment wallet=%+v err=%v", wallet, err)
	}
	if wallet, err = s.CreditManagedWallet(ctx, tenant, "operator-credit-b-"+suffix, "debt repayment and balance", 10_000_000); err != nil || wallet.BalanceMicrousd != 5_000_000 || wallet.DebtMicrousd != 0 || wallet.AvailableMicrousd != 5_000_000 {
		t.Fatalf("complete debt repayment wallet=%+v err=%v", wallet, err)
	}
}

func TestManagedUsageReservationIDIsTenantScoped(t *testing.T) {
	first := managedUsageReservationID("tenant-a", "request-1")
	if first == managedUsageReservationID("tenant-b", "request-1") || first != managedUsageReservationID("tenant-a", "request-1") || !strings.HasPrefix(first, "usage_") {
		t.Fatalf("reservation identity is not deterministic and tenant scoped: %q", first)
	}
}

func TestManagedPaymentLockIDIsStableAndSessionScoped(t *testing.T) {
	first := managedPaymentLockID("stripe", "cs_test_1")
	if first != managedPaymentLockID("stripe", "cs_test_1") {
		t.Fatal("payment lock identity is not deterministic")
	}
	if first == managedPaymentLockID("stripe", "cs_test_2") || first == managedPaymentLockID("other", "cs_test_1") {
		t.Fatal("payment lock identity is not provider/session scoped")
	}
}

func completeFundingIntentForTest(t *testing.T, s *Store, tenant, suffix string, amountMicrousd int64) (string, string) {
	t.Helper()
	key := "checkout-" + suffix
	id := managedbilling.FundingIntentID(tenant, key)
	requested := domain.ManagedFundingIntent{ID: id, TenantID: tenant, Provider: "stripe", IdempotencyKey: key, AmountMicrousd: amountMicrousd, Currency: "USD"}
	_, lease, err := s.PrepareManagedFundingIntent(context.Background(), tenant, requested, time.Minute)
	if err != nil || lease == "" {
		t.Fatalf("prepare funding intent lease=%q err=%v", lease, err)
	}
	sessionID := "cs-" + suffix
	session := domain.ManagedCheckoutSession{FundingIntentID: id, Provider: "stripe", ProviderID: sessionID, URL: "https://checkout.stripe.test/" + sessionID, AmountMicrousd: amountMicrousd, Currency: "USD", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if _, err = s.CompleteManagedFundingIntent(context.Background(), tenant, id, lease, session); err != nil {
		t.Fatalf("complete funding intent: %v", err)
	}
	return id, sessionID
}

func TestManagedWalletAuthorizesAndSettlesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "managed-wallet-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Managed Wallet"); err != nil {
		t.Fatal(err)
	}
	environment, err := s.CreateEnvironment(ctx, tenant, domain.Environment{Name: "production", PolicyJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	model, err := s.CreateLogicalModel(ctx, tenant, domain.LogicalModel{Name: "managed-model"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := s.CreateEndpoint(ctx, tenant, domain.Endpoint{Name: "managed-model-api", LogicalModelID: model.ID, EnvironmentID: environment.ID})
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.AddTargetForTenant(ctx, tenant, domain.Target{Name: "modal-api", URL: "https://supplier.invalid/v1", Provider: "modal", Runtime: "openai-compatible-api", UpstreamModel: "supplier/model"})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := s.CreateSecretReference(ctx, tenant, "modal-api", "env", "MODAL_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"adapter":"modal","secret_reference_id":%q,"enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":10000,"max_request_cost_microusd":1000,"billing_mode":"customer_wallet","input_microusd_per_mtok":100000,"output_microusd_per_mtok":400000,"cost_basis_input_microusd_per_mtok":80000,"cost_basis_output_microusd_per_mtok":320000,"minimum_gross_margin_bps":2000,"cost_basis_provenance":"supplier quote fixture","rate_card_valid_until":"2099-01-01T00:00:00Z"}`, secret.ID)
	binding, err := s.CreateBackendBinding(ctx, tenant, domain.BackendBinding{EndpointID: endpoint.ID, Name: "modal-primary", Kind: "external", OwnershipMode: "traffic-managed", TargetID: target.ID, ConfigJSON: config})
	if err != nil {
		t.Fatal(err)
	}

	wallet, err := s.CreditManagedWallet(ctx, tenant, "credit-payment-1", "prepaid payment", 5000)
	if err != nil || wallet.BalanceMicrousd != 5000 || wallet.AvailableMicrousd != 5000 {
		t.Fatalf("initial credit wallet=%#v err=%v", wallet, err)
	}
	wallet, err = s.CreditManagedWallet(ctx, tenant, "credit-payment-1", "prepaid payment retry", 5000)
	if err != nil || wallet.BalanceMicrousd != 5000 {
		t.Fatalf("idempotent credit wallet=%#v err=%v", wallet, err)
	}

	authorization, err := s.AuthorizeManagedUsage(ctx, tenant, binding.ID, "request-1", "modal", "supplier/model")
	if err != nil || !authorization.Required || authorization.ReservedMicrousd != 1000 {
		t.Fatalf("authorization=%#v err=%v", authorization, err)
	}
	wallet, err = s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.BalanceMicrousd != 5000 || wallet.ReservedMicrousd != 1000 || wallet.AvailableMicrousd != 4000 {
		t.Fatalf("reserved wallet=%#v err=%v", wallet, err)
	}

	inputTokens, outputTokens := 1000, 1000
	settled, err := s.SettleManagedUsage(ctx, tenant, authorization.ReservationID, domain.ManagedUsageSettlement{InputTokens: &inputTokens, OutputTokens: &outputTokens})
	if err != nil || settled.State != "settled" || settled.ActualMicrousd != 500 {
		t.Fatalf("settlement=%#v err=%v", settled, err)
	}
	wallet, err = s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.BalanceMicrousd != 4500 || wallet.ReservedMicrousd != 0 || wallet.AvailableMicrousd != 4500 {
		t.Fatalf("settled wallet=%#v err=%v", wallet, err)
	}
	if _, err = s.SettleManagedUsage(ctx, tenant, authorization.ReservationID, domain.ManagedUsageSettlement{InputTokens: &inputTokens, OutputTokens: &outputTokens}); err != nil {
		t.Fatal(err)
	}
	wallet, err = s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.BalanceMicrousd != 4500 {
		t.Fatalf("duplicate settlement wallet=%#v err=%v", wallet, err)
	}
	expiredConfig := strings.Replace(config, "2099-01-01T00:00:00Z", "2000-01-01T00:00:00Z", 1)
	if _, err = s.ExecContext(ctx, `UPDATE backend_bindings SET config_json=?::jsonb WHERE tenant_id=? AND id=?`, expiredConfig, tenant, binding.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthorizeManagedUsage(ctx, tenant, binding.ID, "request-expired", "modal", "supplier/model"); err == nil || !strings.Contains(err.Error(), "rate card expired") {
		t.Fatalf("expired rate-card authorization error=%v", err)
	}
	wallet, err = s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.BalanceMicrousd != 4500 || wallet.ReservedMicrousd != 0 {
		t.Fatalf("expired authorization mutated wallet=%#v err=%v", wallet, err)
	}
	ledger, err := s.ManagedWalletLedger(ctx, tenant, 10)
	if err != nil || len(ledger) != 2 {
		t.Fatalf("ledger=%#v err=%v", ledger, err)
	}
}

func TestManagedWalletRetainsUnknownUsageForReconciliation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "managed-reconcile-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Managed Reconciliation"); err != nil {
		t.Fatal(err)
	}
	environment, err := s.CreateEnvironment(ctx, tenant, domain.Environment{Name: "production", PolicyJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	model, err := s.CreateLogicalModel(ctx, tenant, domain.LogicalModel{Name: "managed-model"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := s.CreateEndpoint(ctx, tenant, domain.Endpoint{Name: "managed-model-api", LogicalModelID: model.ID, EnvironmentID: environment.ID})
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.AddTargetForTenant(ctx, tenant, domain.Target{Name: "runpod-api", URL: "https://supplier.invalid/v1", Provider: "runpod-serverless-api", Runtime: "openai-compatible-api", UpstreamModel: "supplier/model"})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := s.CreateSecretReference(ctx, tenant, "runpod-api", "env", "RUNPOD_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"adapter":"runpod-serverless-api","secret_reference_id":%q,"enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":10000,"max_request_cost_microusd":1000,"billing_mode":"customer_wallet","input_microusd_per_mtok":100000,"output_microusd_per_mtok":400000,"cost_basis_input_microusd_per_mtok":80000,"cost_basis_output_microusd_per_mtok":320000,"minimum_gross_margin_bps":2000,"cost_basis_provenance":"measured capacity fixture","rate_card_valid_until":"2099-01-01T00:00:00Z"}`, secret.ID)
	binding, err := s.CreateBackendBinding(ctx, tenant, domain.BackendBinding{EndpointID: endpoint.ID, Name: "runpod-primary", Kind: "external", OwnershipMode: "traffic-managed", TargetID: target.ID, ConfigJSON: config})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreditManagedWallet(ctx, tenant, "credit-payment-1", "prepaid payment", 1000); err != nil {
		t.Fatal(err)
	}
	authorization, err := s.AuthorizeManagedUsage(ctx, tenant, binding.ID, "request-unknown", "runpod-serverless-api", "supplier/model")
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := s.SettleManagedUsage(ctx, tenant, authorization.ReservationID, domain.ManagedUsageSettlement{})
	if err != nil || reservation.State != "pending_reconciliation" {
		t.Fatalf("reservation=%#v err=%v", reservation, err)
	}
	wallet, err := s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.BalanceMicrousd != 1000 || wallet.ReservedMicrousd != 1000 || wallet.AvailableMicrousd != 0 {
		t.Fatalf("reconciliation wallet=%#v err=%v", wallet, err)
	}
	pending, err := s.ManagedUsageReservations(ctx, tenant, "pending_reconciliation", 10)
	if err != nil || len(pending) != 1 || pending[0].ID != authorization.ReservationID {
		t.Fatalf("pending reservations=%#v err=%v", pending, err)
	}
	if err = s.ReleaseManagedUsage(ctx, tenant, authorization.ReservationID, "supplier confirmed request was not billed"); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseManagedUsage(ctx, tenant, authorization.ReservationID, "idempotent retry"); err != nil {
		t.Fatal(err)
	}
	wallet, err = s.ManagedWallet(ctx, tenant)
	if err != nil || wallet.BalanceMicrousd != 1000 || wallet.ReservedMicrousd != 0 || wallet.AvailableMicrousd != 1000 {
		t.Fatalf("released reconciliation wallet=%#v err=%v", wallet, err)
	}
	released, err := s.ManagedUsageReservations(ctx, tenant, "released", 10)
	if err != nil || len(released) != 1 || released[0].Resolution != "supplier confirmed request was not billed" {
		t.Fatalf("released reservations=%#v err=%v", released, err)
	}
}
