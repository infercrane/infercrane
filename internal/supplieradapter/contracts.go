// Package supplieradapter defines the private boundary between InferCrane and
// an inference supplier. It owns provider-specific wire translation while the
// gateway, billing, and public model contracts remain supplier-neutral.
package supplieradapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	OperationChatCompletions = "chat.completions"
	OperationResponses       = "responses"

	UsageComplete = "complete"
	UsagePartial  = "partial"
	UsageAbsent   = "absent"

	BillingNotTransmitted    = "not_transmitted"
	BillingNoChargeConfirmed = "no_charge_confirmed"
	BillingChargeKnown       = "charge_known"
	BillingAmbiguous         = "ambiguous"

	RetryNever      = "never"
	RetrySameOffer  = "same_offer"
	RetryOtherOffer = "other_offer"

	ErrorInvalidRequest = "invalid_request"
	ErrorAuthentication = "authentication_failed"
	ErrorAuthorization  = "authorization_failed"
	ErrorRateLimited    = "rate_limited"
	ErrorUnavailable    = "unavailable"
	ErrorTimeout        = "timeout"
	ErrorTransport      = "transport"
	ErrorProtocol       = "protocol_error"
	ErrorCancelled      = "cancelled"
	ErrorInternal       = "internal"
)

// Target is an operator-owned supplier route. CredentialReference names a
// secret-manager entry; it must never contain the secret itself.
// BillingPrincipal pins the non-secret supplier account charged by adapters
// whose credentials may be rotated independently of their billing owner.
type Target struct {
	Supplier            string
	BaseURL             string
	SupplierModelID     string
	Region              string
	CredentialReference string
	BillingPrincipal    string
}

// CredentialResolver resolves a reference only inside the trusted adapter
// boundary. The returned bytes must not be persisted or included in errors.
type CredentialResolver interface {
	Resolve(ctx context.Context, reference string) ([]byte, error)
}

type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type Message struct {
	Role    string        `json:"role"`
	Name    string        `json:"name,omitempty"`
	Content []ContentPart `json:"content"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Request is the normalized inference request. Supplier-specific parameters
// belong in an adapter implementation, not in the public or planner contract.
type Request struct {
	ID              string
	Operation       string
	ModelID         string
	Messages        []Message
	Tools           []Tool
	ResponseFormat  json.RawMessage
	MaxOutputTokens *int
	Temperature     *float64
	Stream          bool
}

func (r Request) Validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.ModelID) == "" {
		return errors.New("request id and model id are required")
	}
	if r.Operation != OperationChatCompletions && r.Operation != OperationResponses {
		return fmt.Errorf("unsupported normalized operation %q", r.Operation)
	}
	if len(r.Messages) == 0 {
		return errors.New("at least one message is required")
	}
	if r.MaxOutputTokens != nil && *r.MaxOutputTokens <= 0 {
		return errors.New("max output tokens must be positive")
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return errors.New("temperature must be between 0 and 2")
	}
	for index, message := range r.Messages {
		if strings.TrimSpace(message.Role) == "" || len(message.Content) == 0 {
			return fmt.Errorf("message %d requires a role and content", index)
		}
	}
	for index, tool := range r.Tools {
		if strings.TrimSpace(tool.Name) == "" || len(tool.InputSchema) == 0 || !json.Valid(tool.InputSchema) {
			return fmt.Errorf("tool %d requires a name and valid input schema", index)
		}
	}
	if len(r.ResponseFormat) > 0 && !json.Valid(r.ResponseFormat) {
		return errors.New("response format must be valid JSON")
	}
	return nil
}

type Usage struct {
	State           string
	InputTokens     *int64
	OutputTokens    *int64
	CachedInput     *int64
	ReasoningTokens *int64
}

func (u Usage) Validate() error {
	if u.State != UsageComplete && u.State != UsagePartial && u.State != UsageAbsent {
		return fmt.Errorf("unknown usage state %q", u.State)
	}
	values := []*int64{u.InputTokens, u.OutputTokens, u.CachedInput, u.ReasoningTokens}
	for _, value := range values {
		if value != nil && *value < 0 {
			return errors.New("usage token counts cannot be negative")
		}
	}
	if u.State == UsageComplete && (u.InputTokens == nil || u.OutputTokens == nil) {
		return errors.New("complete usage requires input and output tokens")
	}
	if u.State == UsageAbsent && (u.InputTokens != nil || u.OutputTokens != nil || u.CachedInput != nil || u.ReasoningTokens != nil) {
		return errors.New("absent usage cannot carry token counts")
	}
	return nil
}

type Choice struct {
	Index        int
	Message      *Message
	Text         string
	FinishReason string
}

type Response struct {
	ID                string
	ModelID           string
	SupplierRequestID string
	Choices           []Choice
	Usage             Usage
}

type StreamEvent struct {
	Type              string
	ChoiceIndex       int
	TextDelta         string
	FinishReason      string
	Usage             *Usage
	SupplierRequestID string
}

type Stream interface {
	Next(ctx context.Context) (StreamEvent, error)
	Close() error
}

// Error is safe to retain in operational records. Message must be sanitized;
// raw upstream bodies and credentials never belong here.
type Error struct {
	Code              string
	Message           string
	HTTPStatus        int
	SupplierRequestID string
	Retry             string
	RetryAfter        time.Duration
	Billing           string
	ResponseStarted   bool
	Cause             error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *Error) Validate() error {
	if e == nil {
		return errors.New("supplier error is required")
	}
	if !oneOf(e.Code, ErrorInvalidRequest, ErrorAuthentication, ErrorAuthorization, ErrorRateLimited, ErrorUnavailable, ErrorTimeout, ErrorTransport, ErrorProtocol, ErrorCancelled, ErrorInternal) {
		return fmt.Errorf("unknown supplier error code %q", e.Code)
	}
	if !oneOf(e.Retry, RetryNever, RetrySameOffer, RetryOtherOffer) {
		return fmt.Errorf("unknown retry hint %q", e.Retry)
	}
	if !oneOf(e.Billing, BillingNotTransmitted, BillingNoChargeConfirmed, BillingChargeKnown, BillingAmbiguous) {
		return fmt.Errorf("unknown billing outcome %q", e.Billing)
	}
	if e.RetryAfter < 0 {
		return errors.New("retry delay cannot be negative")
	}
	return nil
}

// SafeToRetry is intentionally stricter than the supplier's retry hint. A
// request is retryable only before response bytes and when double billing has
// been ruled out.
func (e *Error) SafeToRetry() bool {
	if e == nil || e.ResponseStarted || (e.Retry != RetrySameOffer && e.Retry != RetryOtherOffer) {
		return false
	}
	return e.Billing == BillingNotTransmitted || e.Billing == BillingNoChargeConfirmed
}

type RateLimit struct {
	RequestsRemaining *int64
	TokensRemaining   *int64
	ResetAt           *time.Time
}

type InventoryItem struct {
	SupplierModelID string
	Region          string
	Available       bool
}

// Observation normalizes health, access, inventory, and rate-limit signals.
// It is evidence at ObservedAt, not a durable availability promise.
type Observation struct {
	Access       string
	Availability string
	Health       string
	ObservedAt   time.Time
	RateLimit    RateLimit
	Inventory    []InventoryItem
}

// Adapter owns provider-specific authentication and wire translation. It must
// return *Error for supplier failures so billing ambiguity and retry safety are
// never inferred from an HTTP status alone.
type Adapter interface {
	Name() string
	BuildRequest(ctx context.Context, target Target, request Request, credentials CredentialResolver) (*http.Request, error)
	DecodeResponse(ctx context.Context, response *http.Response) (Response, error)
	OpenStream(ctx context.Context, response *http.Response) (Stream, error)
	Probe(ctx context.Context, target Target, credentials CredentialResolver) (Observation, error)
}

func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}
