package supplieradapter

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestRequestValidationPreservesNormalizedContract(t *testing.T) {
	maximum := 128
	request := Request{
		ID: "request-1", Operation: OperationChatCompletions, ModelID: "glm-5.3", MaxOutputTokens: &maximum,
		Messages: []Message{{Role: "user", Content: []ContentPart{{Type: "text", Text: "hello"}}}},
		Tools:    []Tool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	request.Tools[0].InputSchema = json.RawMessage(`{`)
	if err := request.Validate(); err == nil {
		t.Fatal("invalid supplier-neutral tool schema was accepted")
	}
}

func TestUsageKeepsAbsentAndPartialDistinct(t *testing.T) {
	input := int64(12)
	if err := (Usage{State: UsagePartial, InputTokens: &input}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Usage{State: UsageComplete, InputTokens: &input}).Validate(); err == nil {
		t.Fatal("complete usage without output tokens was accepted")
	}
	if err := (Usage{State: UsageAbsent, InputTokens: &input}).Validate(); err == nil {
		t.Fatal("absent usage with invented token counts was accepted")
	}
}

func TestSupplierErrorRequiresKnownNoChargeBeforeRetry(t *testing.T) {
	safe := &Error{Code: ErrorUnavailable, Retry: RetryOtherOffer, Billing: BillingNotTransmitted}
	if err := safe.Validate(); err != nil {
		t.Fatal(err)
	}
	if !safe.SafeToRetry() {
		t.Fatal("pre-transmission failure should be safe to retry")
	}
	for name, value := range map[string]*Error{
		"ambiguous billing": {Code: ErrorTimeout, Retry: RetryOtherOffer, Billing: BillingAmbiguous},
		"response started":  {Code: ErrorTransport, Retry: RetryOtherOffer, Billing: BillingNoChargeConfirmed, ResponseStarted: true},
		"retry forbidden":   {Code: ErrorInvalidRequest, Retry: RetryNever, Billing: BillingNotTransmitted},
	} {
		t.Run(name, func(t *testing.T) {
			if value.SafeToRetry() {
				t.Fatal("unsafe supplier failure became retryable")
			}
		})
	}
	cause := errors.New("transport closed")
	normalized := &Error{Code: ErrorTransport, Cause: cause}
	if !errors.Is(normalized, cause) {
		t.Fatal("normalized error did not preserve its internal cause")
	}
	if err := (&Error{Code: ErrorTimeout, Retry: RetryOtherOffer, Billing: "assume_free"}).Validate(); err == nil {
		t.Fatal("unknown billing outcome was accepted")
	}
}
