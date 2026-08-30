package managedbilling

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v86"
)

type fakeStripeCheckout struct {
	params *stripe.CheckoutSessionCreateParams
	row    *stripe.CheckoutSession
	err    error
}

func (f *fakeStripeCheckout) Create(_ context.Context, params *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	f.params = params
	return f.row, f.err
}

func TestStripeCheckoutUsesConfiguredFixedPrice(t *testing.T) {
	prices := fixtureStripePrices()
	checkout := &fakeStripeCheckout{row: &stripe.CheckoutSession{ID: "cs_test", URL: "https://checkout.stripe.com/c/pay/test", ExpiresAt: time.Now().Add(time.Hour).Unix()}}
	provider := Stripe{Checkout: checkout, ReturnURL: "https://console.infercrane.com/settings/billing", PriceIDs: prices}
	session, err := provider.CreateCheckoutSession(context.Background(), "tenant-a", 50_000_000)
	if err != nil || session.ProviderID != "cs_test" || session.AmountMicrousd != 50_000_000 {
		t.Fatalf("session=%+v err=%v", session, err)
	}
	if checkout.params == nil || len(checkout.params.LineItems) != 1 || checkout.params.LineItems[0].Price == nil || *checkout.params.LineItems[0].Price != prices[50_000_000] || checkout.params.LineItems[0].PriceData != nil {
		t.Fatalf("checkout did not use the fixed server-side price: %+v", checkout.params)
	}
	if checkout.params.SuccessURL == nil || !strings.Contains(*checkout.params.SuccessURL, "session_id=%7BCHECKOUT_SESSION_ID%7D") || checkout.params.CancelURL == nil || !strings.Contains(*checkout.params.CancelURL, "checkout=cancelled") {
		t.Fatalf("unexpected return URLs: success=%v cancel=%v", checkout.params.SuccessURL, checkout.params.CancelURL)
	}
}

func TestStripeWebhookVerificationIsPaidIdempotencyInput(t *testing.T) {
	secret := "whsec_fixture"
	provider := Stripe{WebhookSecret: secret, ExpectedLivemode: false}
	payload := []byte(fmt.Sprintf(`{"id":"evt_1","object":"event","api_version":%q,"created":1700000000,"livemode":false,"pending_webhooks":1,"type":"checkout.session.completed","data":{"object":{"id":"cs_1","object":"checkout.session","amount_total":5000,"client_reference_id":"tenant-a","currency":"usd","livemode":false,"metadata":{"infercrane_tenant_id":"tenant-a","infercrane_amount_microusd":"50000000"},"payment_intent":"pi_1","payment_status":"paid","status":"complete"}}}`, stripe.APIVersion))
	signature := stripeTestSignature(payload, secret, time.Now().Unix())
	payment, err := provider.ParseWebhook(payload, signature)
	if err != nil || !payment.Apply || payment.Operation != "credit" || payment.EventID != "evt_1" || payment.SessionID != "cs_1" || payment.PaymentIntentID != "pi_1" || payment.TenantID != "tenant-a" || payment.AmountMicrousd != 50_000_000 || payment.Currency != "USD" || len(payment.PayloadDigest) != 64 {
		t.Fatalf("payment=%+v err=%v", payment, err)
	}
	if _, err = provider.ParseWebhook(payload, "t=1,v1=invalid"); err == nil {
		t.Fatal("invalid signature was accepted")
	}
}

func TestStripeWebhookFailsClosedOnModeAndAmountMismatch(t *testing.T) {
	secret := "whsec_fixture"
	payload := []byte(fmt.Sprintf(`{"id":"evt_2","object":"event","api_version":%q,"created":1700000000,"livemode":false,"pending_webhooks":1,"type":"checkout.session.completed","data":{"object":{"id":"cs_2","object":"checkout.session","amount_total":5000,"client_reference_id":"tenant-a","currency":"usd","livemode":false,"metadata":{"infercrane_tenant_id":"tenant-a","infercrane_amount_microusd":"100000000"},"payment_intent":"pi_2","payment_status":"paid","status":"complete"}}}`, stripe.APIVersion))
	signature := stripeTestSignature(payload, secret, time.Now().Unix())
	if _, err := (Stripe{WebhookSecret: secret}).ParseWebhook(payload, signature); err == nil || !strings.Contains(err.Error(), "amount") {
		t.Fatalf("amount mismatch error=%v", err)
	}
	if _, err := (Stripe{WebhookSecret: secret, ExpectedLivemode: true}).ParseWebhook(payload, signature); err == nil || !strings.Contains(err.Error(), "livemode") {
		t.Fatalf("mode mismatch error=%v", err)
	}
}

func TestStripeWebhookFailsClosedOnTenantReferenceMismatch(t *testing.T) {
	secret := "whsec_fixture"
	payload := []byte(fmt.Sprintf(`{"id":"evt_tenant","object":"event","api_version":%q,"created":1700000000,"livemode":false,"pending_webhooks":1,"type":"checkout.session.completed","data":{"object":{"id":"cs_tenant","object":"checkout.session","amount_total":5000,"client_reference_id":"tenant-b","currency":"usd","livemode":false,"metadata":{"infercrane_tenant_id":"tenant-a","infercrane_amount_microusd":"50000000"},"payment_intent":"pi_tenant","payment_status":"paid","status":"complete"}}}`, stripe.APIVersion))
	signature := stripeTestSignature(payload, secret, time.Now().Unix())
	if _, err := (Stripe{WebhookSecret: secret}).ParseWebhook(payload, signature); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("tenant mismatch error=%v", err)
	}
}

func TestStripeRefundWebhookIsCumulativeAndPaymentIntentScoped(t *testing.T) {
	secret := "whsec_fixture"
	provider := Stripe{WebhookSecret: secret, ExpectedLivemode: false}
	payload := []byte(fmt.Sprintf(`{"id":"evt_refund","object":"event","api_version":%q,"created":1700000000,"livemode":false,"pending_webhooks":1,"type":"charge.refunded","data":{"object":{"id":"ch_1","object":"charge","amount":5000,"amount_refunded":2500,"currency":"usd","livemode":false,"payment_intent":"pi_1","refunded":false}}}`, stripe.APIVersion))
	payment, err := provider.ParseWebhook(payload, stripeTestSignature(payload, secret, time.Now().Unix()))
	if err != nil || !payment.Apply || payment.Operation != "refund" || payment.PaymentIntentID != "pi_1" || payment.RefundedMicrousd != 25_000_000 || payment.Currency != "USD" || payment.TenantID != "" {
		t.Fatalf("refund=%+v err=%v", payment, err)
	}
}

func stripeTestSignature(payload []byte, secret string, timestamp int64) string {
	signed := fmt.Sprintf("%d.%s", timestamp, payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signed))
	return fmt.Sprintf("t=%d,v1=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))
}

func fixtureStripePrices() map[int64]string {
	out := map[int64]string{}
	for _, amount := range CheckoutAmounts() {
		out[amount] = fmt.Sprintf("price_%d", amount)
	}
	return out
}
