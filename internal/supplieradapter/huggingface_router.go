package supplieradapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	HuggingFaceRouterAdapterName         = "huggingface-router-openai"
	HuggingFaceSupplier                  = "huggingface"
	HuggingFaceRouterBaseURL             = "https://router.huggingface.co/v1"
	HuggingFaceRouterMaxOutputTokens     = 32_768
	HuggingFaceRouterMaxRequestBytes     = 4 << 20
	HuggingFaceRouterMaxResponseBytes    = 8 << 20
	HuggingFaceRouterMaxStreamEventBytes = 1 << 20
	huggingFaceProbeMaxOutputTokens      = 4
)

var (
	huggingFaceRepositoryPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	huggingFaceProviderPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	huggingFaceBillingPrincipalPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type huggingFaceExpectedModel struct {
	pinned     string
	repository string
}

type huggingFaceExpectedModelKey struct{}

// HuggingFaceRouterAdapter implements a narrow OpenAI-compatible contract.
// Every executable target must bind <repository>:<provider>; automatic routing
// policies are useful for discovery but are deliberately forbidden here. This
// provider binding does not prove an upstream model revision, quantization, or
// runtime. Those remain time-bounded qualification facts.
type HuggingFaceRouterAdapter struct {
	client *http.Client
	now    func() time.Time
}

var _ Adapter = (*HuggingFaceRouterAdapter)(nil)

func NewHuggingFaceRouterAdapter(client *http.Client) *HuggingFaceRouterAdapter {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	strictClient := *client
	strictClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HuggingFaceRouterAdapter{client: &strictClient, now: time.Now}
}

func (*HuggingFaceRouterAdapter) Name() string { return HuggingFaceRouterAdapterName }

func (a *HuggingFaceRouterAdapter) BuildRequest(ctx context.Context, target Target, request Request, credentials CredentialResolver) (*http.Request, error) {
	repository, _, err := validateHuggingFaceTarget(target)
	if err != nil {
		return nil, invalidRequest(err.Error())
	}
	if err = validateHuggingFaceRequest(request); err != nil {
		return nil, invalidRequest(err.Error())
	}
	if credentials == nil {
		return nil, internalBeforeTransmission("supplier credential resolver is unavailable", nil)
	}
	credential, err := credentials.Resolve(ctx, target.CredentialReference)
	if err != nil {
		return nil, internalBeforeTransmission("supplier credential could not be resolved", err)
	}
	defer clear(credential)
	credentialValue := bytes.TrimSpace(credential)
	if len(credentialValue) == 0 || !safeIdentifier(string(credentialValue)) {
		return nil, internalBeforeTransmission("supplier credential is invalid", nil)
	}

	type wireMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	payload := struct {
		Model         string          `json:"model"`
		Messages      []wireMessage   `json:"messages"`
		MaxTokens     int             `json:"max_tokens"`
		Temperature   *float64        `json:"temperature,omitempty"`
		Stream        bool            `json:"stream"`
		StreamOptions map[string]bool `json:"stream_options,omitempty"`
	}{
		Model: target.SupplierModelID, Messages: make([]wireMessage, 0, len(request.Messages)),
		MaxTokens: *request.MaxOutputTokens, Temperature: request.Temperature, Stream: request.Stream,
	}
	if request.Stream {
		payload.StreamOptions = map[string]bool{"include_usage": true}
	}
	for _, message := range request.Messages {
		payload.Messages = append(payload.Messages, wireMessage{Role: message.Role, Content: message.Content[0].Text})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, internalBeforeTransmission("supplier request could not be encoded", err)
	}
	if len(body) > HuggingFaceRouterMaxRequestBytes {
		return nil, invalidRequest("normalized request exceeds the routed inference MVP byte limit")
	}

	expected := huggingFaceExpectedModel{pinned: target.SupplierModelID, repository: repository}
	requestContext := context.WithValue(ctx, huggingFaceExpectedModelKey{}, expected)
	upstream, err := http.NewRequestWithContext(requestContext, http.MethodPost, HuggingFaceRouterBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, internalBeforeTransmission("supplier request could not be constructed", err)
	}
	upstream.Header.Set("Authorization", "Bearer "+string(credentialValue))
	upstream.Header.Set("X-HF-Bill-To", target.BillingPrincipal)
	upstream.Header.Set("Content-Type", "application/json")
	if request.Stream {
		upstream.Header.Set("Accept", "text/event-stream")
	} else {
		upstream.Header.Set("Accept", "application/json")
	}
	upstream.Header.Set("X-Request-ID", request.ID)
	upstream.Header.Set("User-Agent", "InferCrane/1 supplier-adapter")
	return upstream, nil
}

func (a *HuggingFaceRouterAdapter) DecodeResponse(_ context.Context, response *http.Response) (Response, error) {
	if response == nil {
		return Response{}, huggingFaceProtocolFailure("supplier response is missing", "", nil)
	}
	requestID := huggingFaceSupplierRequestID(response.Header)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if response.Body != nil {
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		}
		return Response{}, normalizeHuggingFaceHTTPError(response.StatusCode, response.Header, requestID)
	}
	if response.Body == nil {
		return Response{}, huggingFaceProtocolFailure("supplier response body is missing", requestID, nil)
	}
	expected, ok := expectedHuggingFaceModel(response)
	if !ok {
		response.Body.Close()
		return Response{}, huggingFaceProtocolFailure("supplier response lost its expected model identity", requestID, nil)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		response.Body.Close()
		return Response{}, huggingFaceProtocolFailure("supplier response content type is invalid", requestID, err)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body, HuggingFaceRouterMaxResponseBytes)
	if err != nil {
		return Response{}, huggingFaceProtocolFailure("supplier response exceeded the safe byte limit", requestID, err)
	}
	var raw struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
			Message      struct {
				Role    string  `json:"role"`
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *huggingFaceUsage `json:"usage"`
	}
	if err = json.Unmarshal(body, &raw); err != nil {
		return Response{}, huggingFaceProtocolFailure("supplier returned malformed JSON", requestID, err)
	}
	if strings.TrimSpace(raw.ID) == "" || !expected.accepts(raw.Model) || len(raw.Choices) != 1 || raw.Choices[0].Index != 0 {
		return Response{}, huggingFaceProtocolFailure("supplier response identity or choices did not match the qualified contract", requestID, nil)
	}
	choice := raw.Choices[0]
	if choice.Message.Role != "assistant" || choice.Message.Content == nil || choice.FinishReason == nil || !validHuggingFaceFinishReason(*choice.FinishReason) {
		return Response{}, huggingFaceProtocolFailure("supplier response choice did not match the qualified contract", requestID, nil)
	}
	usage := normalizeHuggingFaceUsage(raw.Usage)
	if err = validateHuggingFaceUsage(usage); err != nil || usage.State != UsageComplete {
		return Response{}, huggingFaceProtocolFailure("supplier response did not include complete valid usage", requestID, err)
	}
	return Response{
		ID: raw.ID, ModelID: expected.pinned, SupplierRequestID: requestID, Usage: usage,
		Choices: []Choice{{Index: choice.Index, Message: &Message{Role: "assistant", Content: []ContentPart{{Type: "text", Text: *choice.Message.Content}}}, FinishReason: *choice.FinishReason}},
	}, nil
}

func (a *HuggingFaceRouterAdapter) OpenStream(_ context.Context, response *http.Response) (Stream, error) {
	if response == nil {
		return nil, huggingFaceProtocolFailure("supplier response is missing", "", nil)
	}
	requestID := huggingFaceSupplierRequestID(response.Header)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if response.Body != nil {
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		}
		return nil, normalizeHuggingFaceHTTPError(response.StatusCode, response.Header, requestID)
	}
	if response.Body == nil {
		return nil, huggingFaceProtocolFailure("supplier response body is missing", requestID, nil)
	}
	expected, ok := expectedHuggingFaceModel(response)
	if !ok {
		response.Body.Close()
		return nil, huggingFaceProtocolFailure("supplier stream lost its expected model identity", requestID, nil)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		response.Body.Close()
		return nil, huggingFaceProtocolFailure("supplier stream content type is invalid", requestID, err)
	}
	return newHuggingFaceRouterStream(response.Body, requestID, expected), nil
}

func (a *HuggingFaceRouterAdapter) Probe(ctx context.Context, target Target, credentials CredentialResolver) (Observation, error) {
	// Probe is a bounded, billable qualification action, not a readiness check.
	// Callers must schedule it explicitly and reconcile its operator-owned cost.
	if _, _, err := validateHuggingFaceTarget(target); err != nil {
		return Observation{}, invalidRequest(err.Error())
	}
	observedAt := time.Now().UTC()
	if a.now != nil {
		observedAt = a.now().UTC()
	}
	temperature := 0.0
	maximum := huggingFaceProbeMaxOutputTokens
	request, err := a.BuildRequest(ctx, target, Request{
		ID:        fmt.Sprintf("infercrane-hf-probe-%d", observedAt.UnixNano()),
		Operation: OperationChatCompletions,
		ModelID:   target.SupplierModelID,
		Messages: []Message{{
			Role:    "user",
			Content: []ContentPart{{Type: "text", Text: "Reply with OK."}},
		}},
		MaxOutputTokens: &maximum,
		Temperature:     &temperature,
		Stream:          false,
	}, credentials)
	if err != nil {
		return Observation{}, err
	}
	response, err := a.client.Do(request)
	if err != nil {
		// A transport failure cannot prove whether the router accepted and billed
		// the request. Keep it ambiguous so callers cannot retry automatically.
		return Observation{}, &Error{Code: ErrorTransport, Message: "supplier completion probe failed", Retry: RetryOtherOffer, Billing: BillingAmbiguous, Cause: err}
	}
	if _, err = a.DecodeResponse(ctx, response); err != nil {
		return Observation{}, err
	}
	// The public /v1/models inventory is discovery metadata and does not
	// authenticate a token. A successful, decoded completion proves the exact
	// credential + provider-bound request contract used for publication.
	return Observation{
		Access: "authorized", Availability: "available", Health: "healthy", ObservedAt: observedAt,
		RateLimit: huggingFaceRateLimit(response.Header, observedAt),
		Inventory: []InventoryItem{{SupplierModelID: target.SupplierModelID, Region: target.Region, Available: true}},
	}, nil
}

type huggingFaceUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func normalizeHuggingFaceUsage(raw *huggingFaceUsage) Usage {
	if raw == nil {
		return Usage{State: UsageAbsent}
	}
	state := UsagePartial
	if raw.PromptTokens != nil && raw.CompletionTokens != nil {
		state = UsageComplete
	}
	var cached, reasoning *int64
	if raw.PromptTokensDetails != nil {
		cached = raw.PromptTokensDetails.CachedTokens
	}
	if raw.CompletionTokensDetails != nil {
		reasoning = raw.CompletionTokensDetails.ReasoningTokens
	}
	return Usage{State: state, InputTokens: raw.PromptTokens, OutputTokens: raw.CompletionTokens, CachedInput: cached, ReasoningTokens: reasoning}
}

func validateHuggingFaceUsage(usage Usage) error {
	if err := usage.Validate(); err != nil {
		return err
	}
	if usage.CachedInput != nil && usage.InputTokens != nil && *usage.CachedInput > *usage.InputTokens {
		return errors.New("cached input tokens exceed input tokens")
	}
	if usage.ReasoningTokens != nil && usage.OutputTokens != nil && *usage.ReasoningTokens > *usage.OutputTokens {
		return errors.New("reasoning tokens exceed output tokens")
	}
	return nil
}

func validateHuggingFaceTarget(target Target) (string, string, error) {
	if target.Supplier != HuggingFaceSupplier {
		return "", "", errors.New("target is not a routed inference contract")
	}
	parsed, err := url.Parse(strings.TrimSpace(target.BaseURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "router.huggingface.co" || parsed.Path != "/v1" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", "", errors.New("target must use the exact qualified routed inference base URL")
	}
	repository, provider, ok := splitPinnedHuggingFaceModel(target.SupplierModelID)
	if !ok {
		return "", "", errors.New("target model must pin one exact repository and provider")
	}
	if target.Region != "global" {
		return "", "", errors.New("routed inference MVP target region must be global")
	}
	if strings.TrimSpace(target.CredentialReference) == "" {
		return "", "", errors.New("target credential reference is required")
	}
	if !huggingFaceBillingPrincipalPattern.MatchString(target.BillingPrincipal) {
		return "", "", errors.New("target must pin one canonical Hugging Face billing principal")
	}
	return repository, provider, nil
}

func splitPinnedHuggingFaceModel(model string) (string, string, bool) {
	if strings.TrimSpace(model) != model || strings.Count(model, ":") != 1 {
		return "", "", false
	}
	repository, provider, _ := strings.Cut(model, ":")
	if !huggingFaceRepositoryPattern.MatchString(repository) || !huggingFaceProviderPattern.MatchString(provider) {
		return "", "", false
	}
	switch provider {
	case "fastest", "cheapest", "preferred", "auto":
		return "", "", false
	}
	return repository, provider, true
}

func validateHuggingFaceRequest(request Request) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if request.Operation != OperationChatCompletions {
		return errors.New("routed inference MVP supports Chat Completions only")
	}
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens > HuggingFaceRouterMaxOutputTokens {
		return fmt.Errorf("max output tokens must be set and cannot exceed %d for the MVP", HuggingFaceRouterMaxOutputTokens)
	}
	if len(request.Messages) > 256 {
		return errors.New("routed inference MVP accepts at most 256 messages")
	}
	if len(request.Tools) != 0 || len(request.ResponseFormat) != 0 {
		return errors.New("routed inference MVP does not yet accept tools or response formats")
	}
	for index, message := range request.Messages {
		if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
			return fmt.Errorf("message %d has an unsupported role", index)
		}
		if message.Name != "" || len(message.Content) != 1 || message.Content[0].Type != "text" || strings.TrimSpace(message.Content[0].Text) == "" || message.Content[0].ImageURL != "" {
			return fmt.Errorf("message %d must contain exactly one non-empty text part and no name", index)
		}
	}
	return nil
}

func validHuggingFaceFinishReason(value string) bool {
	return oneOf(value, "stop", "length", "content_filter")
}

func expectedHuggingFaceModel(response *http.Response) (huggingFaceExpectedModel, bool) {
	if response == nil || response.Request == nil {
		return huggingFaceExpectedModel{}, false
	}
	value, ok := response.Request.Context().Value(huggingFaceExpectedModelKey{}).(huggingFaceExpectedModel)
	if !ok || value.pinned == "" || value.repository == "" {
		return huggingFaceExpectedModel{}, false
	}
	repository, _, valid := splitPinnedHuggingFaceModel(value.pinned)
	return value, valid && repository == value.repository
}

func (model huggingFaceExpectedModel) accepts(value string) bool {
	return value == model.pinned || value == model.repository
}

func huggingFaceSupplierRequestID(header http.Header) string {
	for _, name := range []string{"Inference-Id", "X-Request-ID"} {
		value := strings.TrimSpace(header.Get(name))
		if value != "" && len(value) <= 256 && safeIdentifier(value) {
			return value
		}
	}
	return ""
}

func normalizeHuggingFaceHTTPError(status int, header http.Header, requestID string) *Error {
	code, message, retry := ErrorInternal, "supplier request failed", RetryNever
	billing := BillingAmbiguous
	// Hugging Face's provider contract bills completed requests only when the
	// provider response status is 2xx or 3xx. A received 4xx/5xx therefore
	// proves no charge at this router boundary; transport failures remain
	// ambiguous because no status was received.
	if status >= http.StatusBadRequest && status <= 599 {
		billing = BillingNoChargeConfirmed
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		code, message = ErrorInvalidRequest, "supplier rejected the request"
	case http.StatusUnauthorized:
		code, message = ErrorAuthentication, "supplier authentication failed"
	case http.StatusForbidden:
		code, message = ErrorAuthorization, "supplier authorization failed"
	case http.StatusNotFound:
		code, message = ErrorUnavailable, "qualified supplier route is unavailable"
	case http.StatusTooManyRequests:
		code, message, retry = ErrorRateLimited, "supplier rate limit was reached", RetryOtherOffer
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		code, message, retry = ErrorTimeout, "supplier request timed out", RetryOtherOffer
	default:
		if status >= http.StatusInternalServerError {
			code, message, retry = ErrorUnavailable, "supplier is temporarily unavailable", RetryOtherOffer
		}
	}
	return &Error{
		Code: code, Message: message, HTTPStatus: status, SupplierRequestID: requestID,
		Retry: retry, RetryAfter: parseRetryAfter(header.Get("Retry-After"), time.Now()), Billing: billing,
	}
}

func huggingFaceProtocolFailure(message, requestID string, cause error) *Error {
	return &Error{Code: ErrorProtocol, Message: message, HTTPStatus: http.StatusBadGateway, SupplierRequestID: requestID, Retry: RetryNever, Billing: BillingAmbiguous, Cause: cause}
}

func huggingFaceRateLimit(header http.Header, now time.Time) RateLimit {
	return RateLimit{
		RequestsRemaining: parseOptionalInt64(header.Get("X-RateLimit-Remaining-Requests")),
		TokensRemaining:   parseOptionalInt64(header.Get("X-RateLimit-Remaining-Tokens")),
		ResetAt:           parseOptionalReset(header.Get("X-RateLimit-Reset"), now),
	}
}
