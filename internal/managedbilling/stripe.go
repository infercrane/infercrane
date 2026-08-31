package managedbilling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

const stripeProvider = "stripe"

type stripeCheckoutClient interface {
	Create(context.Context, *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error)
}

type Stripe struct {
	Checkout         stripeCheckoutClient
	WebhookSecret    string
	ReturnURL        string
	PriceIDs         map[int64]string
	ExpectedLivemode bool
}

func NewStripe(secretKey, webhookSecret, returnURL string, priceIDs map[int64]string, expectedLivemode bool) (*Stripe, error) {
	if strings.TrimSpace(secretKey) == "" || strings.TrimSpace(webhookSecret) == "" {
		return nil, errors.New("Stripe secret key and webhook secret are required")
	}
	if _, err := validatedReturnURL(returnURL); err != nil {
		return nil, err
	}
	for _, amount := range checkoutAmounts {
		if strings.TrimSpace(priceIDs[amount]) == "" {
			return nil, fmt.Errorf("Stripe price ID for %d micro-USD is required", amount)
		}
	}
	client := stripe.NewClient(secretKey)
	return &Stripe{Checkout: client.V1CheckoutSessions, WebhookSecret: webhookSecret, ReturnURL: returnURL, PriceIDs: clonePriceIDs(priceIDs), ExpectedLivemode: expectedLivemode}, nil
}

func (s Stripe) CreateCheckoutSession(ctx context.Context, tenant string, amountMicrousd int64) (domain.ManagedCheckoutSession, error) {
	if strings.TrimSpace(tenant) == "" {
		return domain.ManagedCheckoutSession{}, errors.New("tenant is required")
	}
	if !ValidateCheckoutAmount(amountMicrousd) {
		return domain.ManagedCheckoutSession{}, errors.New("amount is not an allowed prepaid top-up")
	}
	priceID := strings.TrimSpace(s.PriceIDs[amountMicrousd])
	if priceID == "" {
		return domain.ManagedCheckoutSession{}, errors.New("prepaid top-up price is not configured")
	}
	base, err := validatedReturnURL(s.ReturnURL)
	if err != nil {
		return domain.ManagedCheckoutSession{}, err
	}
	success := *base
	successQuery := success.Query()
	successQuery.Set("checkout", "success")
	successQuery.Set("session_id", "{CHECKOUT_SESSION_ID}")
	success.RawQuery = successQuery.Encode()
	cancel := *base
	cancelQuery := cancel.Query()
	cancelQuery.Set("checkout", "cancelled")
	cancel.RawQuery = cancelQuery.Encode()

	params := &stripe.CheckoutSessionCreateParams{
		Mode:               stripe.String("payment"),
		SuccessURL:         stripe.String(success.String()),
		CancelURL:          stripe.String(cancel.String()),
		ClientReferenceID:  stripe.String(tenant),
		PaymentMethodTypes: []*string{stripe.String("card")},
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			Quantity: stripe.Int64(1),
			Price:    stripe.String(priceID),
		}},
	}
	params.AddMetadata("infercrane_tenant_id", tenant)
	params.AddMetadata("infercrane_amount_microusd", strconv.FormatInt(amountMicrousd, 10))
	created, err := s.Checkout.Create(ctx, params)
	if err != nil {
		return domain.ManagedCheckoutSession{}, fmt.Errorf("create Stripe Checkout session: %w", err)
	}
	if created == nil || created.ID == "" || created.URL == "" || created.ExpiresAt <= 0 {
		return domain.ManagedCheckoutSession{}, errors.New("Stripe returned an incomplete Checkout session")
	}
	return domain.ManagedCheckoutSession{Provider: stripeProvider, ProviderID: created.ID, URL: created.URL, AmountMicrousd: amountMicrousd, Currency: "USD", ExpiresAt: time.Unix(created.ExpiresAt, 0).UTC()}, nil
}

func (s Stripe) ParseWebhook(payload []byte, signature string) (domain.ManagedPaymentEvent, error) {
	if len(payload) == 0 || strings.TrimSpace(signature) == "" {
		return domain.ManagedPaymentEvent{}, errors.New("Stripe webhook payload and signature are required")
	}
	event, err := webhook.ConstructEventWithOptions(payload, signature, s.WebhookSecret, webhook.ConstructEventOptions{})
	if err != nil {
		return domain.ManagedPaymentEvent{}, fmt.Errorf("verify Stripe webhook: %w", err)
	}
	digest := sha256.Sum256(payload)
	normalized := domain.ManagedPaymentEvent{Provider: stripeProvider, EventID: event.ID, EventType: string(event.Type), PayloadDigest: hex.EncodeToString(digest[:]), IgnoreReason: "event type does not grant balance", MetadataJSON: "{}"}
	if event.ID == "" {
		return domain.ManagedPaymentEvent{}, errors.New("Stripe webhook event ID is missing")
	}
	if event.Type == stripe.EventTypeChargeRefunded {
		return s.parseRefundEvent(event, normalized)
	}
	if event.Type != stripe.EventTypeCheckoutSessionCompleted && event.Type != stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded {
		return normalized, nil
	}
	if event.Data == nil || len(event.Data.Raw) == 0 {
		return domain.ManagedPaymentEvent{}, errors.New("Stripe Checkout event data is missing")
	}
	var session stripe.CheckoutSession
	if err = json.Unmarshal(event.Data.Raw, &session); err != nil {
		return domain.ManagedPaymentEvent{}, fmt.Errorf("decode Stripe Checkout session: %w", err)
	}
	normalized.SessionID = session.ID
	normalized.Operation = "credit"
	if session.PaymentIntent != nil {
		normalized.PaymentIntentID = session.PaymentIntent.ID
	}
	normalized.TenantID = strings.TrimSpace(session.Metadata["infercrane_tenant_id"])
	normalized.Currency = strings.ToUpper(string(session.Currency))
	if session.AmountTotal < 0 || session.AmountTotal > math.MaxInt64/MicrousdPerUSCent {
		return domain.ManagedPaymentEvent{}, errors.New("Stripe Checkout amount is outside the supported range")
	}
	normalized.AmountMicrousd = session.AmountTotal * MicrousdPerUSCent
	metadata, _ := json.Marshal(map[string]any{"provider": stripeProvider, "event_type": event.Type, "checkout_state": session.Status, "payment_status": session.PaymentStatus, "livemode": session.Livemode})
	normalized.MetadataJSON = string(metadata)

	expectedRaw := strings.TrimSpace(session.Metadata["infercrane_amount_microusd"])
	if normalized.TenantID == "" && expectedRaw == "" {
		normalized.IgnoreReason = "checkout is not owned by InferCrane"
		return normalized, nil
	}
	expected, parseErr := strconv.ParseInt(expectedRaw, 10, 64)
	if session.ID == "" || normalized.PaymentIntentID == "" || normalized.TenantID == "" || session.ClientReferenceID != normalized.TenantID || parseErr != nil || !ValidateCheckoutAmount(expected) {
		return domain.ManagedPaymentEvent{}, errors.New("Stripe Checkout metadata is incomplete or invalid")
	}
	if session.Livemode != s.ExpectedLivemode {
		return domain.ManagedPaymentEvent{}, errors.New("Stripe Checkout livemode does not match the configured environment")
	}
	if normalized.Currency != "USD" || session.AmountTotal <= 0 || normalized.AmountMicrousd != expected {
		return domain.ManagedPaymentEvent{}, errors.New("Stripe Checkout amount does not match signed InferCrane metadata")
	}
	if session.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
		normalized.IgnoreReason = "payment is not paid"
		return normalized, nil
	}
	normalized.Apply = true
	normalized.IgnoreReason = ""
	return normalized, nil
}

func (s Stripe) parseRefundEvent(event stripe.Event, normalized domain.ManagedPaymentEvent) (domain.ManagedPaymentEvent, error) {
	if event.Data == nil || len(event.Data.Raw) == 0 {
		return domain.ManagedPaymentEvent{}, errors.New("Stripe refund event data is missing")
	}
	var charge stripe.Charge
	if err := json.Unmarshal(event.Data.Raw, &charge); err != nil {
		return domain.ManagedPaymentEvent{}, fmt.Errorf("decode Stripe refunded charge: %w", err)
	}
	if charge.Livemode != s.ExpectedLivemode {
		return domain.ManagedPaymentEvent{}, errors.New("Stripe refund livemode does not match the configured environment")
	}
	normalized.Operation = "refund"
	if charge.PaymentIntent != nil {
		normalized.PaymentIntentID = strings.TrimSpace(charge.PaymentIntent.ID)
	}
	normalized.Currency = strings.ToUpper(string(charge.Currency))
	if normalized.PaymentIntentID == "" || normalized.Currency != "USD" || charge.AmountRefunded <= 0 || charge.AmountRefunded > math.MaxInt64/MicrousdPerUSCent {
		return domain.ManagedPaymentEvent{}, errors.New("Stripe refund is incomplete or outside the supported USD range")
	}
	normalized.RefundedMicrousd = charge.AmountRefunded * MicrousdPerUSCent
	metadata, _ := json.Marshal(map[string]any{"provider": stripeProvider, "event_type": event.Type, "charge_id": charge.ID, "payment_intent_id": normalized.PaymentIntentID, "livemode": charge.Livemode, "refunded_microusd": normalized.RefundedMicrousd})
	normalized.MetadataJSON = string(metadata)
	normalized.Apply = true
	normalized.IgnoreReason = ""
	return normalized, nil
}

func validatedReturnURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("billing return URL must be an absolute HTTP(S) URL without credentials or fragment")
	}
	return parsed, nil
}

func clonePriceIDs(source map[int64]string) map[int64]string {
	cloned := make(map[int64]string, len(source))
	for amount, id := range source {
		cloned[amount] = id
	}
	return cloned
}
