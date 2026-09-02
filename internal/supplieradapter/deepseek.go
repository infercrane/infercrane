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
	"strconv"
	"strings"
	"time"
)

const (
	DeepSeekAdapterName        = "deepseek-openai"
	DeepSeekSupplier           = "deepseek"
	DeepSeekBaseURL            = "https://api.deepseek.com"
	DeepSeekV4FlashModelID     = "deepseek-v4-flash"
	DeepSeekV4FlashRevision    = "DeepSeek-V4-Flash-0731"
	DeepSeekMVPMaxOutputTokens = 32_768
	DeepSeekMVPMaxRequestBytes = 4 << 20
)

type DeepSeekAdapter struct {
	client *http.Client
	now    func() time.Time
}

func NewDeepSeekAdapter(client *http.Client) *DeepSeekAdapter {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	strictClient := *client
	strictClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &DeepSeekAdapter{client: &strictClient, now: time.Now}
}

func (*DeepSeekAdapter) Name() string { return DeepSeekAdapterName }

func (a *DeepSeekAdapter) BuildRequest(ctx context.Context, target Target, request Request, credentials CredentialResolver) (*http.Request, error) {
	if err := validateDeepSeekTarget(target); err != nil {
		return nil, invalidRequest(err.Error())
	}
	if err := validateDeepSeekRequest(request); err != nil {
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
	if len(credentialValue) == 0 {
		return nil, internalBeforeTransmission("supplier credential is empty", nil)
	}
	if !safeIdentifier(string(credentialValue)) {
		return nil, internalBeforeTransmission("supplier credential contains invalid characters", nil)
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
		Thinking      struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}{
		Model:       DeepSeekV4FlashModelID,
		MaxTokens:   *request.MaxOutputTokens,
		Temperature: request.Temperature,
		Stream:      request.Stream,
		Messages:    make([]wireMessage, 0, len(request.Messages)),
	}
	payload.Thinking.Type = "disabled"
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
	if len(body) > DeepSeekMVPMaxRequestBytes {
		return nil, invalidRequest("normalized request exceeds the DeepSeek MVP byte limit")
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, DeepSeekBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, internalBeforeTransmission("supplier request could not be constructed", err)
	}
	upstream.Header.Set("Authorization", "Bearer "+string(credentialValue))
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

func (a *DeepSeekAdapter) DecodeResponse(_ context.Context, response *http.Response) (Response, error) {
	if response == nil {
		return Response{}, protocolFailure("supplier response is missing", "", nil)
	}
	if response.Body == nil {
		return Response{}, protocolFailure("supplier response body is missing", supplierRequestID(response.Header), nil)
	}
	requestID := supplierRequestID(response.Header)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return Response{}, normalizeDeepSeekHTTPError(response.StatusCode, response.Header, requestID)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body, DeepSeekMVPMaxRequestBytes)
	if err != nil {
		return Response{}, protocolFailure("supplier response exceeded the safe byte limit", requestID, err)
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
		Usage *deepSeekUsage `json:"usage"`
	}
	if err = json.Unmarshal(body, &raw); err != nil {
		return Response{}, protocolFailure("supplier returned malformed JSON", requestID, err)
	}
	if strings.TrimSpace(raw.ID) == "" || raw.Model != DeepSeekV4FlashModelID || len(raw.Choices) != 1 || raw.Choices[0].Index != 0 {
		return Response{}, protocolFailure("supplier response identity or choices did not match the qualified contract", requestID, nil)
	}
	out := Response{ID: raw.ID, ModelID: raw.Model, SupplierRequestID: requestID, Usage: normalizeDeepSeekUsage(raw.Usage)}
	for _, choice := range raw.Choices {
		if choice.Message.Role != "assistant" || choice.Message.Content == nil || choice.FinishReason == nil || !validDeepSeekFinishReason(*choice.FinishReason) {
			return Response{}, protocolFailure("supplier response choice did not match the qualified contract", requestID, nil)
		}
		out.Choices = append(out.Choices, Choice{
			Index:        choice.Index,
			Message:      &Message{Role: "assistant", Content: []ContentPart{{Type: "text", Text: *choice.Message.Content}}},
			FinishReason: *choice.FinishReason,
		})
	}
	if err = out.Usage.Validate(); err != nil {
		return Response{}, protocolFailure("supplier returned invalid usage", requestID, err)
	}
	return out, nil
}

func (a *DeepSeekAdapter) OpenStream(_ context.Context, response *http.Response) (Stream, error) {
	if response == nil {
		return nil, protocolFailure("supplier response is missing", "", nil)
	}
	if response.Body == nil {
		return nil, protocolFailure("supplier response body is missing", supplierRequestID(response.Header), nil)
	}
	requestID := supplierRequestID(response.Header)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil, normalizeDeepSeekHTTPError(response.StatusCode, response.Header, requestID)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		response.Body.Close()
		return nil, protocolFailure("supplier stream content type is invalid", requestID, err)
	}
	return newDeepSeekStream(response.Body, requestID), nil
}

func (a *DeepSeekAdapter) Probe(ctx context.Context, target Target, credentials CredentialResolver) (Observation, error) {
	if err := validateDeepSeekTarget(target); err != nil {
		return Observation{}, invalidRequest(err.Error())
	}
	if credentials == nil {
		return Observation{}, internalBeforeTransmission("supplier credential resolver is unavailable", nil)
	}
	credential, err := credentials.Resolve(ctx, target.CredentialReference)
	if err != nil {
		return Observation{}, internalBeforeTransmission("supplier credential could not be resolved", err)
	}
	defer clear(credential)
	credentialValue := bytes.TrimSpace(credential)
	if len(credentialValue) == 0 || !safeIdentifier(string(credentialValue)) {
		return Observation{}, internalBeforeTransmission("supplier credential is invalid", nil)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, DeepSeekBaseURL+"/models", nil)
	if err != nil {
		return Observation{}, internalBeforeTransmission("supplier probe could not be constructed", err)
	}
	request.Header.Set("Authorization", "Bearer "+string(credentialValue))
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return Observation{}, &Error{Code: ErrorTransport, Message: "supplier probe transport failed", Retry: RetrySameOffer, Billing: BillingNotTransmitted, Cause: err}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return Observation{}, normalizeDeepSeekHTTPError(response.StatusCode, response.Header, supplierRequestID(response.Header))
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body, 1<<20)
	if err != nil {
		return Observation{}, protocolFailure("supplier inventory response exceeded the safe byte limit", supplierRequestID(response.Header), err)
	}
	var inventory struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.Unmarshal(body, &inventory); err != nil {
		return Observation{}, protocolFailure("supplier inventory response was malformed", supplierRequestID(response.Header), err)
	}
	available := false
	items := make([]InventoryItem, 0, len(inventory.Data))
	for _, item := range inventory.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		isTarget := id == DeepSeekV4FlashModelID
		available = available || isTarget
		items = append(items, InventoryItem{SupplierModelID: id, Region: "global", Available: true})
	}
	availability := "unavailable"
	if available {
		availability = "available"
	}
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	return Observation{Access: "authorized", Availability: availability, Health: "healthy", ObservedAt: now().UTC(), Inventory: items, RateLimit: deepSeekRateLimit(response.Header, now().UTC())}, nil
}

type deepSeekUsage struct {
	PromptTokens            *int64 `json:"prompt_tokens"`
	CompletionTokens        *int64 `json:"completion_tokens"`
	PromptCacheHitTokens    *int64 `json:"prompt_cache_hit_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func normalizeDeepSeekUsage(raw *deepSeekUsage) Usage {
	if raw == nil {
		return Usage{State: UsageAbsent}
	}
	state := UsagePartial
	if raw.PromptTokens != nil && raw.CompletionTokens != nil {
		state = UsageComplete
	}
	var reasoning *int64
	if raw.CompletionTokensDetails != nil {
		reasoning = raw.CompletionTokensDetails.ReasoningTokens
	}
	return Usage{State: state, InputTokens: raw.PromptTokens, OutputTokens: raw.CompletionTokens, CachedInput: raw.PromptCacheHitTokens, ReasoningTokens: reasoning}
}

func validDeepSeekFinishReason(value string) bool {
	return oneOf(value, "stop", "length", "content_filter", "tool_calls", "insufficient_system_resource")
}

func validateDeepSeekTarget(target Target) error {
	if target.Supplier != DeepSeekSupplier || target.SupplierModelID != DeepSeekV4FlashModelID {
		return errors.New("target is not the qualified DeepSeek V4 Flash contract")
	}
	parsed, err := url.Parse(strings.TrimSpace(target.BaseURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "api.deepseek.com" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return errors.New("target must use the exact qualified DeepSeek OpenAI base URL")
	}
	if target.Region != "" && target.Region != "global" {
		return errors.New("DeepSeek MVP target region must be global")
	}
	if strings.TrimSpace(target.CredentialReference) == "" {
		return errors.New("target credential reference is required")
	}
	return nil
}

func validateDeepSeekRequest(request Request) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if request.Operation != OperationChatCompletions {
		return errors.New("DeepSeek MVP supports Chat Completions only")
	}
	if request.ModelID == "" {
		return errors.New("public model id is required")
	}
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens > DeepSeekMVPMaxOutputTokens {
		return fmt.Errorf("max output tokens must be set and cannot exceed %d for the MVP", DeepSeekMVPMaxOutputTokens)
	}
	if len(request.Messages) > 256 {
		return errors.New("DeepSeek MVP accepts at most 256 messages")
	}
	if len(request.Tools) != 0 || len(request.ResponseFormat) != 0 {
		return errors.New("DeepSeek MVP does not yet accept tools or response formats")
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

func invalidRequest(message string) *Error {
	return &Error{Code: ErrorInvalidRequest, Message: message, HTTPStatus: http.StatusBadRequest, Retry: RetryNever, Billing: BillingNotTransmitted}
}

func internalBeforeTransmission(message string, cause error) *Error {
	return &Error{Code: ErrorInternal, Message: message, HTTPStatus: http.StatusServiceUnavailable, Retry: RetryNever, Billing: BillingNotTransmitted, Cause: cause}
}

func protocolFailure(message, requestID string, cause error) *Error {
	return &Error{Code: ErrorProtocol, Message: message, HTTPStatus: http.StatusBadGateway, SupplierRequestID: requestID, Retry: RetryNever, Billing: BillingAmbiguous, Cause: cause}
}

func normalizeDeepSeekHTTPError(status int, header http.Header, requestID string) *Error {
	code, message, retry := ErrorInternal, "supplier request failed", RetryNever
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		code, message = ErrorInvalidRequest, "supplier rejected the request"
	case http.StatusUnauthorized:
		code, message = ErrorAuthentication, "supplier authentication failed"
	case http.StatusForbidden:
		code, message = ErrorAuthorization, "supplier authorization failed"
	case http.StatusTooManyRequests:
		code, message, retry = ErrorRateLimited, "supplier rate limit was reached", RetryOtherOffer
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		code, message, retry = ErrorTimeout, "supplier request timed out", RetryOtherOffer
	default:
		if status >= http.StatusInternalServerError {
			code, message, retry = ErrorUnavailable, "supplier is temporarily unavailable", RetryOtherOffer
		}
	}
	return &Error{Code: code, Message: message, HTTPStatus: status, SupplierRequestID: requestID, Retry: retry, RetryAfter: parseRetryAfter(header.Get("Retry-After"), time.Now()), Billing: BillingAmbiguous}
}

func supplierRequestID(header http.Header) string {
	for _, name := range []string{"X-Request-ID", "X-DS-Request-ID"} {
		value := strings.TrimSpace(header.Get(name))
		if value != "" && len(value) <= 256 && safeIdentifier(value) {
			return value
		}
	}
	return ""
}

func safeIdentifier(value string) bool {
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func deepSeekRateLimit(header http.Header, now time.Time) RateLimit {
	return RateLimit{
		RequestsRemaining: parseOptionalInt64(header.Get("X-RateLimit-Remaining-Requests")),
		TokensRemaining:   parseOptionalInt64(header.Get("X-RateLimit-Remaining-Tokens")),
		ResetAt:           parseOptionalReset(header.Get("X-RateLimit-Reset"), now),
	}
}

func parseOptionalInt64(value string) *int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return nil
	}
	return &parsed
}

func parseOptionalReset(value string, now time.Time) *time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		at := now.Add(time.Duration(seconds) * time.Second).UTC()
		return &at
	}
	if at, err := http.ParseTime(value); err == nil {
		at = at.UTC()
		return &at
	}
	return nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, errors.New("body limit exceeded")
	}
	return body, nil
}
