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
	ZAIAdapterName            = "zai-openai"
	ZAISupplier               = "zai"
	ZAIBaseURL                = "https://api.z.ai/api/paas/v4"
	ZAIGLM52ModelID           = "glm-5.2"
	ZAIGLM53ModelID           = "glm-5.3"
	ZAIGLM53FlashModelID      = "glm-5.3-flash"
	ZAIMVPMaxOutputTokens     = 32_768
	ZAIMVPMaxRequestBytes     = 4 << 20
	ZAIMVPMaxResponseBytes    = 8 << 20
	ZAIMVPMaxStreamEventBytes = 1 << 20
)

type zaiExpectedModelKey struct{}

// ZAIAdapter implements the deliberately narrow text-only subset of Z.ai's
// OpenAI-compatible pay-as-you-go API that InferCrane qualifies for its public
// Model API. Supplier-specific identities remain inside this adapter boundary.
type ZAIAdapter struct {
	client *http.Client
	now    func() time.Time
}

var _ Adapter = (*ZAIAdapter)(nil)

func NewZAIAdapter(client *http.Client) *ZAIAdapter {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	strictClient := *client
	strictClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ZAIAdapter{client: &strictClient, now: time.Now}
}

func (*ZAIAdapter) Name() string { return ZAIAdapterName }

func (a *ZAIAdapter) BuildRequest(ctx context.Context, target Target, request Request, credentials CredentialResolver) (*http.Request, error) {
	if err := validateZAITarget(target); err != nil {
		return nil, zaiInvalidRequest(err.Error())
	}
	if err := validateZAIRequest(request); err != nil {
		return nil, zaiInvalidRequest(err.Error())
	}
	if credentials == nil {
		return nil, zaiInternalBeforeTransmission("supplier credential resolver is unavailable", nil)
	}
	credential, err := credentials.Resolve(ctx, target.CredentialReference)
	if err != nil {
		return nil, zaiInternalBeforeTransmission("supplier credential could not be resolved", err)
	}
	defer clear(credential)
	credentialValue := bytes.TrimSpace(credential)
	if len(credentialValue) == 0 || !zaiSafeHeaderValue(string(credentialValue)) {
		return nil, zaiInternalBeforeTransmission("supplier credential is invalid", nil)
	}

	type wireMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	payload := struct {
		Model       string        `json:"model"`
		Messages    []wireMessage `json:"messages"`
		MaxTokens   int           `json:"max_tokens"`
		Temperature *float64      `json:"temperature,omitempty"`
		Stream      bool          `json:"stream"`
		Thinking    struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}{
		Model: target.SupplierModelID, Messages: make([]wireMessage, 0, len(request.Messages)),
		MaxTokens: *request.MaxOutputTokens, Temperature: request.Temperature, Stream: request.Stream,
	}
	// Pin thinking instead of relying on a supplier default that can differ
	// between endpoints or change independently of an InferCrane product.
	payload.Thinking.Type = "enabled"
	for _, message := range request.Messages {
		payload.Messages = append(payload.Messages, wireMessage{Role: message.Role, Content: message.Content[0].Text})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, zaiInternalBeforeTransmission("supplier request could not be encoded", err)
	}
	if len(body) > ZAIMVPMaxRequestBytes {
		return nil, zaiInvalidRequest("normalized request exceeds the Z.ai MVP byte limit")
	}

	requestContext := context.WithValue(ctx, zaiExpectedModelKey{}, target.SupplierModelID)
	upstream, err := http.NewRequestWithContext(requestContext, http.MethodPost, ZAIBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, zaiInternalBeforeTransmission("supplier request could not be constructed", err)
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

func (a *ZAIAdapter) DecodeResponse(_ context.Context, response *http.Response) (Response, error) {
	if response == nil {
		return Response{}, zaiProtocolFailure("supplier response is missing", "", nil)
	}
	requestID := zaiSupplierRequestID(response.Header)
	if response.Body == nil {
		return Response{}, zaiProtocolFailure("supplier response body is missing", requestID, nil)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return Response{}, normalizeZAIHTTPError(response.StatusCode, response.Header, requestID)
	}
	expectedModel, ok := zaiExpectedModel(response)
	if !ok {
		response.Body.Close()
		return Response{}, zaiProtocolFailure("supplier response lost its expected model identity", requestID, nil)
	}
	if !zaiJSONContentType(response.Header.Get("Content-Type")) {
		response.Body.Close()
		return Response{}, zaiProtocolFailure("supplier response content type is invalid", requestID, nil)
	}
	defer response.Body.Close()
	body, err := zaiReadBounded(response.Body, ZAIMVPMaxResponseBytes)
	if err != nil {
		return Response{}, zaiProtocolFailure("supplier response exceeded the safe byte limit", requestID, err)
	}
	var raw struct {
		ID        string `json:"id"`
		RequestID string `json:"request_id"`
		Model     string `json:"model"`
		Choices   []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
			Message      struct {
				Role    string  `json:"role"`
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *zaiUsage `json:"usage"`
	}
	if err = json.Unmarshal(body, &raw); err != nil {
		return Response{}, zaiProtocolFailure("supplier returned malformed JSON", requestID, err)
	}
	requestID, err = mergeZAIRequestID(requestID, raw.RequestID)
	if err != nil {
		return Response{}, zaiProtocolFailure("supplier response request identity did not match", requestID, err)
	}
	if strings.TrimSpace(raw.ID) == "" || raw.Model != expectedModel || len(raw.Choices) != 1 || raw.Choices[0].Index != 0 {
		return Response{}, zaiProtocolFailure("supplier response identity or choices did not match the qualified contract", requestID, nil)
	}
	choice := raw.Choices[0]
	finishReason, valid := normalizeZAIFinishReason(choice.FinishReason)
	if choice.Message.Role != "assistant" || choice.Message.Content == nil || !valid {
		return Response{}, zaiProtocolFailure("supplier response choice did not match the qualified contract", requestID, nil)
	}
	usage := normalizeZAIUsage(raw.Usage)
	if err = validateZAIUsage(usage); err != nil || usage.State != UsageComplete {
		return Response{}, zaiProtocolFailure("supplier response did not include complete valid usage", requestID, err)
	}
	return Response{
		ID: raw.ID, ModelID: raw.Model, SupplierRequestID: requestID, Usage: usage,
		Choices: []Choice{{Index: choice.Index, Message: &Message{Role: "assistant", Content: []ContentPart{{Type: "text", Text: *choice.Message.Content}}}, FinishReason: finishReason}},
	}, nil
}

func (a *ZAIAdapter) OpenStream(_ context.Context, response *http.Response) (Stream, error) {
	if response == nil {
		return nil, zaiProtocolFailure("supplier response is missing", "", nil)
	}
	requestID := zaiSupplierRequestID(response.Header)
	if response.Body == nil {
		return nil, zaiProtocolFailure("supplier response body is missing", requestID, nil)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil, normalizeZAIHTTPError(response.StatusCode, response.Header, requestID)
	}
	expectedModel, ok := zaiExpectedModel(response)
	if !ok {
		response.Body.Close()
		return nil, zaiProtocolFailure("supplier stream lost its expected model identity", requestID, nil)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		response.Body.Close()
		return nil, zaiProtocolFailure("supplier stream content type is invalid", requestID, err)
	}
	return newZAIStream(response.Body, requestID, expectedModel), nil
}

func (a *ZAIAdapter) Probe(ctx context.Context, target Target, credentials CredentialResolver) (Observation, error) {
	if err := validateZAITarget(target); err != nil {
		return Observation{}, zaiInvalidRequest(err.Error())
	}
	maximum := 1
	probeRequest, err := a.BuildRequest(ctx, target, Request{
		ID: "zai-qualification-probe", Operation: OperationChatCompletions, ModelID: "qualification-probe",
		Messages:        []Message{{Role: "user", Content: []ContentPart{{Type: "text", Text: "."}}}},
		MaxOutputTokens: &maximum,
	}, credentials)
	if err != nil {
		return Observation{}, err
	}
	response, err := a.client.Do(probeRequest)
	if err != nil {
		return Observation{}, &Error{Code: ErrorTransport, Message: "supplier probe transport failed", Retry: RetryNever, Billing: BillingAmbiguous, Cause: err}
	}
	decoded, err := a.DecodeResponse(ctx, response)
	if err != nil {
		return Observation{}, err
	}
	if decoded.ModelID != target.SupplierModelID || decoded.Usage.State != UsageComplete {
		return Observation{}, zaiProtocolFailure("supplier probe did not prove the exact model contract", decoded.SupplierRequestID, nil)
	}
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	return Observation{
		Access: "authorized", Availability: "available", Health: "healthy", ObservedAt: now().UTC(),
		Inventory: []InventoryItem{{SupplierModelID: target.SupplierModelID, Region: "global", Available: true}},
	}, nil
}

type zaiUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func normalizeZAIUsage(raw *zaiUsage) Usage {
	if raw == nil {
		return Usage{State: UsageAbsent}
	}
	state := UsagePartial
	if raw.PromptTokens != nil && raw.CompletionTokens != nil {
		state = UsageComplete
	}
	var cached *int64
	if raw.PromptTokensDetails != nil {
		cached = raw.PromptTokensDetails.CachedTokens
	}
	return Usage{State: state, InputTokens: raw.PromptTokens, OutputTokens: raw.CompletionTokens, CachedInput: cached}
}

func validateZAIUsage(usage Usage) error {
	if err := usage.Validate(); err != nil {
		return err
	}
	if usage.CachedInput != nil && usage.InputTokens != nil && *usage.CachedInput > *usage.InputTokens {
		return errors.New("cached input tokens exceed input tokens")
	}
	return nil
}

func validateZAITarget(target Target) error {
	if target.Supplier != ZAISupplier || !zaiSupportedModel(target.SupplierModelID) {
		return errors.New("target is not a qualified Z.ai model contract")
	}
	parsed, err := url.Parse(strings.TrimSpace(target.BaseURL))
	if err != nil || target.BaseURL != ZAIBaseURL || parsed.Scheme != "https" || parsed.Host != "api.z.ai" || parsed.Path != "/api/paas/v4" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return errors.New("target must use the exact qualified Z.ai pay-as-you-go base URL")
	}
	if target.Region != "" && target.Region != "global" {
		return errors.New("Z.ai MVP target region must be global")
	}
	if strings.TrimSpace(target.CredentialReference) == "" {
		return errors.New("target credential reference is required")
	}
	return nil
}

func validateZAIRequest(request Request) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if request.Operation != OperationChatCompletions {
		return errors.New("Z.ai MVP supports Chat Completions only")
	}
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens > ZAIMVPMaxOutputTokens {
		return fmt.Errorf("max output tokens must be set and cannot exceed %d for the MVP", ZAIMVPMaxOutputTokens)
	}
	if request.Temperature != nil && *request.Temperature > 1 {
		return errors.New("temperature cannot exceed 1 for Z.ai")
	}
	if len(request.Messages) > 256 {
		return errors.New("Z.ai MVP accepts at most 256 messages")
	}
	if len(request.Tools) != 0 || len(request.ResponseFormat) != 0 {
		return errors.New("Z.ai MVP does not yet accept tools or response formats")
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

func zaiSupportedModel(value string) bool {
	return oneOf(value, ZAIGLM52ModelID, ZAIGLM53ModelID, ZAIGLM53FlashModelID)
}

func zaiExpectedModel(response *http.Response) (string, bool) {
	if response == nil || response.Request == nil {
		return "", false
	}
	value, ok := response.Request.Context().Value(zaiExpectedModelKey{}).(string)
	return value, ok && zaiSupportedModel(value)
}

func normalizeZAIFinishReason(value *string) (string, bool) {
	if value == nil {
		return "", false
	}
	switch *value {
	case "stop":
		return "stop", true
	case "length", "model_context_window_exceeded":
		return "length", true
	case "sensitive":
		return "content_filter", true
	default:
		return "", false
	}
}

func zaiSupplierRequestID(header http.Header) string {
	for _, name := range []string{"X-Request-ID", "X-ZhipuAI-Request-ID"} {
		value := strings.TrimSpace(header.Get(name))
		if zaiSafeRequestID(value) {
			return value
		}
	}
	return ""
}

func mergeZAIRequestID(headerID, bodyID string) (string, error) {
	bodyID = strings.TrimSpace(bodyID)
	if bodyID == "" {
		return headerID, nil
	}
	if !zaiSafeRequestID(bodyID) {
		return headerID, errors.New("unsafe supplier request id")
	}
	if headerID != "" && headerID != bodyID {
		return headerID, errors.New("conflicting supplier request ids")
	}
	return bodyID, nil
}

func zaiSafeRequestID(value string) bool {
	return value != "" && len(value) <= 256 && zaiSafeHeaderValue(value)
}

func zaiSafeHeaderValue(value string) bool {
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func zaiJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func normalizeZAIHTTPError(status int, header http.Header, requestID string) *Error {
	code, message := ErrorInternal, "supplier request failed"
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		code, message = ErrorInvalidRequest, "supplier rejected the request"
	case http.StatusUnauthorized:
		code, message = ErrorAuthentication, "supplier authentication failed"
	case http.StatusForbidden:
		code, message = ErrorAuthorization, "supplier authorization failed"
	case http.StatusTooManyRequests:
		code, message = ErrorRateLimited, "supplier rate limit was reached"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		code, message = ErrorTimeout, "supplier request timed out"
	default:
		if status == http.StatusNotFound || status == http.StatusConflict || status >= http.StatusInternalServerError {
			code, message = ErrorUnavailable, "supplier is temporarily unavailable"
		}
	}
	// Z.ai does not document a no-charge guarantee for a received error. Fail
	// closed: never retry a transmitted request and reconcile its cost later.
	return &Error{Code: code, Message: message, HTTPStatus: status, SupplierRequestID: requestID, Retry: RetryNever, RetryAfter: zaiParseRetryAfter(header.Get("Retry-After"), time.Now()), Billing: BillingAmbiguous}
}

func zaiInvalidRequest(message string) *Error {
	return &Error{Code: ErrorInvalidRequest, Message: message, HTTPStatus: http.StatusBadRequest, Retry: RetryNever, Billing: BillingNotTransmitted}
}

func zaiInternalBeforeTransmission(message string, cause error) *Error {
	return &Error{Code: ErrorInternal, Message: message, HTTPStatus: http.StatusServiceUnavailable, Retry: RetryNever, Billing: BillingNotTransmitted, Cause: cause}
}

func zaiProtocolFailure(message, requestID string, cause error) *Error {
	return &Error{Code: ErrorProtocol, Message: message, HTTPStatus: http.StatusBadGateway, SupplierRequestID: requestID, Retry: RetryNever, Billing: BillingAmbiguous, Cause: cause}
}

func zaiParseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func zaiReadBounded(reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, errors.New("body limit exceeded")
	}
	return body, nil
}
