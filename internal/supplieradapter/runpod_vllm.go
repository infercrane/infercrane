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
	"strconv"
	"strings"
	"time"
)

const (
	RunPodVLLMAdapterName         = "runpod-vllm-openai"
	RunPodSupplier                = "runpod"
	RunPodVLLMMaxOutputTokens     = 32_768
	RunPodVLLMMaxRequestBytes     = 4 << 20
	RunPodVLLMMaxResponseBytes    = 8 << 20
	RunPodVLLMMaxStreamEventBytes = 1 << 20
)

var runPodEndpointIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,127}$`)

type runPodExpectedModelKey struct{}

// RunPodVLLMAdapter implements the deliberately narrow, qualified subset of
// RunPod's queue-based OpenAI-compatible vLLM endpoint. Endpoint lifecycle and
// health polling remain control-plane responsibilities.
type RunPodVLLMAdapter struct {
	client *http.Client
	now    func() time.Time
}

var _ Adapter = (*RunPodVLLMAdapter)(nil)

func NewRunPodVLLMAdapter(client *http.Client) *RunPodVLLMAdapter {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	strictClient := *client
	strictClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &RunPodVLLMAdapter{client: &strictClient, now: time.Now}
}

func (*RunPodVLLMAdapter) Name() string { return RunPodVLLMAdapterName }

func (a *RunPodVLLMAdapter) BuildRequest(ctx context.Context, target Target, request Request, credentials CredentialResolver) (*http.Request, error) {
	if err := validateRunPodVLLMTarget(target); err != nil {
		return nil, runPodInvalidRequest(err.Error())
	}
	if err := validateRunPodVLLMRequest(request); err != nil {
		return nil, runPodInvalidRequest(err.Error())
	}
	if credentials == nil {
		return nil, runPodInternalBeforeTransmission("supplier credential resolver is unavailable", nil)
	}
	credential, err := credentials.Resolve(ctx, target.CredentialReference)
	if err != nil {
		return nil, runPodInternalBeforeTransmission("supplier credential could not be resolved", err)
	}
	defer clear(credential)
	credentialValue := bytes.TrimSpace(credential)
	if len(credentialValue) == 0 || !runPodSafeHeaderValue(string(credentialValue)) {
		return nil, runPodInternalBeforeTransmission("supplier credential is invalid", nil)
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
		return nil, runPodInternalBeforeTransmission("supplier request could not be encoded", err)
	}
	if len(body) > RunPodVLLMMaxRequestBytes {
		return nil, runPodInvalidRequest("normalized request exceeds the RunPod vLLM MVP byte limit")
	}

	requestContext := context.WithValue(ctx, runPodExpectedModelKey{}, target.SupplierModelID)
	endpoint := strings.TrimRight(target.BaseURL, "/") + "/v1/chat/completions"
	upstream, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, runPodInternalBeforeTransmission("supplier request could not be constructed", err)
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

func (a *RunPodVLLMAdapter) DecodeResponse(_ context.Context, response *http.Response) (Response, error) {
	if response == nil {
		return Response{}, runPodProtocolFailure("supplier response is missing", "", nil)
	}
	requestID := runPodSupplierRequestID(response.Header)
	if response.Body == nil {
		return Response{}, runPodProtocolFailure("supplier response body is missing", requestID, nil)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return Response{}, normalizeRunPodHTTPError(response.StatusCode, response.Header, requestID)
	}
	expectedModel, ok := runPodExpectedModel(response)
	if !ok {
		response.Body.Close()
		return Response{}, runPodProtocolFailure("supplier response lost its expected model identity", requestID, nil)
	}
	defer response.Body.Close()
	body, err := runPodReadBounded(response.Body, RunPodVLLMMaxResponseBytes)
	if err != nil {
		return Response{}, runPodProtocolFailure("supplier response exceeded the safe byte limit", requestID, err)
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
		Usage *runPodVLLMUsage `json:"usage"`
	}
	if err = json.Unmarshal(body, &raw); err != nil {
		return Response{}, runPodProtocolFailure("supplier returned malformed JSON", requestID, err)
	}
	if strings.TrimSpace(raw.ID) == "" || raw.Model != expectedModel || len(raw.Choices) != 1 || raw.Choices[0].Index != 0 {
		return Response{}, runPodProtocolFailure("supplier response identity or choices did not match the qualified contract", requestID, nil)
	}
	choice := raw.Choices[0]
	if choice.Message.Role != "assistant" || choice.Message.Content == nil || choice.FinishReason == nil || !validRunPodFinishReason(*choice.FinishReason) {
		return Response{}, runPodProtocolFailure("supplier response choice did not match the qualified contract", requestID, nil)
	}
	usage := normalizeRunPodVLLMUsage(raw.Usage)
	if err = validateRunPodVLLMUsage(usage); err != nil || usage.State != UsageComplete {
		return Response{}, runPodProtocolFailure("supplier response did not include complete valid usage", requestID, err)
	}
	return Response{
		ID: raw.ID, ModelID: raw.Model, SupplierRequestID: requestID, Usage: usage,
		Choices: []Choice{{Index: choice.Index, Message: &Message{Role: "assistant", Content: []ContentPart{{Type: "text", Text: *choice.Message.Content}}}, FinishReason: *choice.FinishReason}},
	}, nil
}

func (a *RunPodVLLMAdapter) OpenStream(_ context.Context, response *http.Response) (Stream, error) {
	if response == nil {
		return nil, runPodProtocolFailure("supplier response is missing", "", nil)
	}
	requestID := runPodSupplierRequestID(response.Header)
	if response.Body == nil {
		return nil, runPodProtocolFailure("supplier response body is missing", requestID, nil)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil, normalizeRunPodHTTPError(response.StatusCode, response.Header, requestID)
	}
	expectedModel, ok := runPodExpectedModel(response)
	if !ok {
		response.Body.Close()
		return nil, runPodProtocolFailure("supplier stream lost its expected model identity", requestID, nil)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		response.Body.Close()
		return nil, runPodProtocolFailure("supplier stream content type is invalid", requestID, err)
	}
	return newRunPodVLLMStream(response.Body, requestID, expectedModel), nil
}

func (a *RunPodVLLMAdapter) Probe(ctx context.Context, target Target, credentials CredentialResolver) (Observation, error) {
	if err := validateRunPodVLLMTarget(target); err != nil {
		return Observation{}, runPodInvalidRequest(err.Error())
	}
	if credentials == nil {
		return Observation{}, runPodInternalBeforeTransmission("supplier credential resolver is unavailable", nil)
	}
	credential, err := credentials.Resolve(ctx, target.CredentialReference)
	if err != nil {
		return Observation{}, runPodInternalBeforeTransmission("supplier credential could not be resolved", err)
	}
	defer clear(credential)
	credentialValue := bytes.TrimSpace(credential)
	if len(credentialValue) == 0 || !runPodSafeHeaderValue(string(credentialValue)) {
		return Observation{}, runPodInternalBeforeTransmission("supplier credential is invalid", nil)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target.BaseURL, "/")+"/v1/models", nil)
	if err != nil {
		return Observation{}, runPodInternalBeforeTransmission("supplier probe could not be constructed", err)
	}
	request.Header.Set("Authorization", "Bearer "+string(credentialValue))
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return Observation{}, &Error{Code: ErrorTransport, Message: "supplier probe transport failed", Retry: RetrySameOffer, Billing: BillingAmbiguous, Cause: err}
	}
	if response == nil || response.Body == nil {
		return Observation{}, runPodProtocolFailure("supplier inventory response body is missing", "", nil)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return Observation{}, normalizeRunPodHTTPError(response.StatusCode, response.Header, runPodSupplierRequestID(response.Header))
	}
	defer response.Body.Close()
	body, err := runPodReadBounded(response.Body, 1<<20)
	if err != nil {
		return Observation{}, runPodProtocolFailure("supplier inventory response exceeded the safe byte limit", runPodSupplierRequestID(response.Header), err)
	}
	var inventory struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.Unmarshal(body, &inventory); err != nil {
		return Observation{}, runPodProtocolFailure("supplier inventory response was malformed", runPodSupplierRequestID(response.Header), err)
	}
	items := make([]InventoryItem, 0, len(inventory.Data))
	targetCount := 0
	for _, item := range inventory.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || !runPodSafeModelID(id) {
			continue
		}
		if id == target.SupplierModelID {
			targetCount++
		}
		items = append(items, InventoryItem{SupplierModelID: id, Region: target.Region, Available: true})
	}
	availability := "unavailable"
	if targetCount == 1 {
		availability = "available"
	}
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	return Observation{Access: "authorized", Availability: availability, Health: "healthy", ObservedAt: now().UTC(), Inventory: items}, nil
}

type runPodVLLMUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func normalizeRunPodVLLMUsage(raw *runPodVLLMUsage) Usage {
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

func validateRunPodVLLMUsage(usage Usage) error {
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

func validateRunPodVLLMTarget(target Target) error {
	if target.Supplier != RunPodSupplier || strings.TrimSpace(target.SupplierModelID) == "" || !runPodSafeModelID(target.SupplierModelID) {
		return errors.New("target is not a qualified RunPod vLLM contract")
	}
	parsed, err := url.Parse(strings.TrimSpace(target.BaseURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "api.runpod.ai" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return errors.New("target must use an exact RunPod queue-based OpenAI endpoint")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v2" || !runPodEndpointIDPattern.MatchString(parts[1]) || parts[2] != "openai" {
		return errors.New("target must use /v2/{endpoint_id}/openai")
	}
	if strings.TrimSpace(target.Region) == "" || !runPodSafeHeaderValue(target.Region) {
		return errors.New("target region is required")
	}
	if strings.TrimSpace(target.CredentialReference) == "" {
		return errors.New("target credential reference is required")
	}
	return nil
}

func validateRunPodVLLMRequest(request Request) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if request.Operation != OperationChatCompletions {
		return errors.New("RunPod vLLM MVP supports Chat Completions only")
	}
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens > RunPodVLLMMaxOutputTokens {
		return fmt.Errorf("max output tokens must be set and cannot exceed %d for the MVP", RunPodVLLMMaxOutputTokens)
	}
	if len(request.Messages) > 256 {
		return errors.New("RunPod vLLM MVP accepts at most 256 messages")
	}
	if len(request.Tools) != 0 || len(request.ResponseFormat) != 0 {
		return errors.New("RunPod vLLM MVP does not yet accept tools or response formats")
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

func validRunPodFinishReason(value string) bool {
	return oneOf(value, "stop", "length", "content_filter")
}

func runPodExpectedModel(response *http.Response) (string, bool) {
	if response == nil || response.Request == nil {
		return "", false
	}
	value, ok := response.Request.Context().Value(runPodExpectedModelKey{}).(string)
	return value, ok && value != "" && runPodSafeModelID(value)
}

func runPodSupplierRequestID(header http.Header) string {
	for _, name := range []string{"X-Runpod-Request-Id", "X-Runpod-Job-Id"} {
		value := strings.TrimSpace(header.Get(name))
		if value != "" && len(value) <= 256 && runPodSafeHeaderValue(value) {
			return value
		}
	}
	return ""
}

func normalizeRunPodHTTPError(status int, header http.Header, requestID string) *Error {
	code, message, retry := ErrorInternal, "supplier request failed", RetryNever
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		code, message = ErrorInvalidRequest, "supplier rejected the request"
	case http.StatusUnauthorized:
		code, message = ErrorAuthentication, "supplier authentication failed"
	case http.StatusForbidden:
		code, message = ErrorAuthorization, "supplier authorization failed"
	case http.StatusNotFound, http.StatusConflict, http.StatusTooManyRequests:
		code, message, retry = ErrorUnavailable, "supplier capacity is unavailable", RetryOtherOffer
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		code, message, retry = ErrorTimeout, "supplier request timed out", RetryOtherOffer
	default:
		if status >= http.StatusInternalServerError {
			code, message, retry = ErrorUnavailable, "supplier is temporarily unavailable", RetryOtherOffer
		}
	}
	if status == http.StatusTooManyRequests {
		code, message = ErrorRateLimited, "supplier rate limit was reached"
	}
	return &Error{Code: code, Message: message, HTTPStatus: status, SupplierRequestID: requestID, Retry: retry, RetryAfter: runPodParseRetryAfter(header.Get("Retry-After"), time.Now()), Billing: BillingAmbiguous}
}

func runPodInvalidRequest(message string) *Error {
	return &Error{Code: ErrorInvalidRequest, Message: message, HTTPStatus: http.StatusBadRequest, Retry: RetryNever, Billing: BillingNotTransmitted}
}

func runPodInternalBeforeTransmission(message string, cause error) *Error {
	return &Error{Code: ErrorInternal, Message: message, HTTPStatus: http.StatusServiceUnavailable, Retry: RetryNever, Billing: BillingNotTransmitted, Cause: cause}
}

func runPodProtocolFailure(message, requestID string, cause error) *Error {
	return &Error{Code: ErrorProtocol, Message: message, HTTPStatus: http.StatusBadGateway, SupplierRequestID: requestID, Retry: RetryNever, Billing: BillingAmbiguous, Cause: cause}
}

func runPodSafeHeaderValue(value string) bool {
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func runPodSafeModelID(value string) bool {
	return len(value) <= 512 && runPodSafeHeaderValue(value)
}

func runPodParseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func runPodReadBounded(reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, errors.New("body limit exceeded")
	}
	return body, nil
}
