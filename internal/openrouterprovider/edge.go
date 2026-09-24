package openrouterprovider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxProviderRequestBytes = 16 << 20

const defaultStreamKeepAliveInterval = 10 * time.Second

// Edge is the deliberately small OpenRouter-facing data plane. It keeps the
// public model identity, provider credential, and admission boundary out of
// the model server while preserving streaming and request cancellation.
type Edge struct {
	Catalog                        *Catalog
	PublicModel                    string
	UpstreamModel                  string
	APIKey                         string
	Credentials                    []ChannelCredential
	ChannelPolicies                map[string]ChannelPolicy
	UpstreamURL                    string
	UpstreamKey                    string
	MaxInFlight                    int
	MinInFlight                    int
	InitialInFlight                int
	AdmissionWindow                int
	TargetTTFT                     time.Duration
	QualifiedOutputTokensPerSecond float64
	InputPricePerMillionUSD        float64
	OutputPricePerMillionUSD       float64
	GPUHourlyCostUSD               float64
	MaxPrefillTokensInFlight       int64
	MetricsKey                     string
	// StreamKeepAliveInterval controls SSE comment heartbeats while the model
	// is producing no bytes. Zero selects the production default.
	StreamKeepAliveInterval time.Duration
	Client                  *http.Client
	Logger                  *slog.Logger
	ReceiptRecorder         ReceiptRecorder

	admission        *admissionController
	channelAdmission *channelAdmissionController
}

func (e *Edge) Handler() (http.Handler, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	inputPrice, outputPrice, hasCatalogPrices, err := e.Catalog.EffectiveTokenPrices(e.PublicModel)
	if err != nil {
		return nil, err
	}
	if hasCatalogPrices {
		if e.InputPricePerMillionUSD != 0 && !samePrice(e.InputPricePerMillionUSD, inputPrice) {
			return nil, fmt.Errorf("configured input price %.9g differs from effective catalog price %.9g", e.InputPricePerMillionUSD, inputPrice)
		}
		if e.OutputPricePerMillionUSD != 0 && !samePrice(e.OutputPricePerMillionUSD, outputPrice) {
			return nil, fmt.Errorf("configured output price %.9g differs from effective catalog price %.9g", e.OutputPricePerMillionUSD, outputPrice)
		}
		e.InputPricePerMillionUSD = inputPrice
		e.OutputPricePerMillionUSD = outputPrice
	}
	e.admission, err = newAdmissionController(AdmissionConfig{
		MinLimit: e.MinInFlight, InitialLimit: e.InitialInFlight, MaxLimit: e.MaxInFlight,
		Window: e.AdmissionWindow, TargetTTFT: e.TargetTTFT,
		QualifiedOutputTokensPerSec: e.QualifiedOutputTokensPerSecond,
		InputPricePerMillionUSD:     e.InputPricePerMillionUSD,
		OutputPricePerMillionUSD:    e.OutputPricePerMillionUSD,
		GPUHourlyCostUSD:            e.GPUHourlyCostUSD,
		MaxPrefillTokensInFlight:    e.MaxPrefillTokensInFlight,
	})
	if err != nil {
		return nil, err
	}
	e.channelAdmission = newChannelAdmissionController(e.ChannelPolicies)
	if e.Client == nil {
		e.Client = &http.Client{Transport: &http.Transport{
			MaxIdleConns:          e.MaxInFlight * 2,
			MaxIdleConnsPerHost:   e.MaxInFlight * 2,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
		}}
	}
	if e.Logger == nil {
		e.Logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", e.health)
	mux.HandleFunc("GET /readyz", e.ready)
	mux.HandleFunc("GET /models", e.marketplaceAuth(e.models))
	mux.HandleFunc("GET /v1/models", e.marketplaceAuth(e.models))
	mux.HandleFunc("GET /openrouter/v1/models", e.marketplaceAuth(e.Catalog.ServeHTTP))
	mux.HandleFunc("POST /v1/chat/completions", e.marketplaceAuth(e.completions))
	mux.HandleFunc("POST /v1/billing/requests", e.marketplaceAuth(e.billingRequests))
	if e.MetricsKey != "" {
		mux.HandleFunc("GET /metrics", e.metricsAuth(e.metrics))
	}
	return mux, nil
}

func samePrice(left, right float64) bool {
	delta := math.Abs(left - right)
	return delta <= 1e-9*math.Max(1, math.Max(math.Abs(left), math.Abs(right)))
}

func (e *Edge) metricsAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expected := "Bearer " + e.MetricsKey
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeProviderError(w, http.StatusUnauthorized, "Invalid metrics API key", "authentication_error")
			return
		}
		next(w, r)
	}
}

func (e *Edge) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	e.admission.writePrometheus(w, time.Now())
}

func (e *Edge) validate() error {
	if e.Catalog == nil {
		return errors.New("OpenRouter provider catalog is required")
	}
	if err := e.Catalog.Validate(); err != nil {
		return err
	}
	if e.PublicModel == "" || e.UpstreamModel == "" || e.MaxInFlight < 1 {
		return errors.New("public model, upstream model, and positive admission limit are required")
	}
	if err := validateChannelCredentials(e.marketplaceCredentials()); err != nil {
		return err
	}
	if err := validateChannelPolicies(e.ChannelPolicies); err != nil {
		return err
	}
	found := false
	for _, model := range e.Catalog.Data {
		found = found || model.ID == e.PublicModel
	}
	if !found {
		return fmt.Errorf("public model %q is absent from provider catalog", e.PublicModel)
	}
	parsed, err := url.Parse(e.UpstreamURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("upstream URL must be absolute and contain no credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return errors.New("plaintext upstream is allowed only on loopback")
	}
	e.UpstreamURL = strings.TrimRight(e.UpstreamURL, "/")
	return nil
}

func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func (e *Edge) health(w http.ResponseWriter, _ *http.Request) {
	writeProviderJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (e *Edge) models(w http.ResponseWriter, r *http.Request) {
	identity := identityFromRequest(r)
	if identity.channel == ChannelOpenRouter {
		e.Catalog.ServeHTTP(w, r)
		return
	}
	e.Catalog.ServeOpenAI(w, e.PublicModel, e.channelPolicy(identity.channel))
}

func (e *Edge) billingRequests(w http.ResponseWriter, r *http.Request) {
	identity := identityFromRequest(r)
	policy := e.channelPolicy(identity.channel)
	if !policy.BillingLookupEnabled {
		writeProviderError(w, http.StatusForbidden, "Billing lookup is not enabled for this marketplace channel", "authorization_error")
		return
	}
	ledger, ok := e.ReceiptRecorder.(ReceiptLedger)
	if !ok {
		writeProviderError(w, http.StatusServiceUnavailable, "Billing ledger is unavailable", "server_error")
		return
	}
	var input struct {
		RequestIDs []string `json:"requestIds"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeProviderError(w, http.StatusBadRequest, "Billing request must be valid JSON", "invalid_request_error")
		return
	}
	if len(input.RequestIDs) == 0 || len(input.RequestIDs) > 10_000 {
		writeProviderError(w, http.StatusBadRequest, "Billing request must contain between 1 and 10000 request IDs", "invalid_request_error")
		return
	}
	for _, requestID := range input.RequestIDs {
		if requestID == "" || len(requestID) > 256 {
			writeProviderError(w, http.StatusBadRequest, "Billing request contains an invalid request ID", "invalid_request_error")
			return
		}
	}
	requests, err := ledger.Lookup(identity.channel, input.RequestIDs)
	if err != nil {
		e.Logger.Error("marketplace billing lookup failed", "channel", identity.channel, "request_count", len(input.RequestIDs), "error", err)
		writeProviderError(w, http.StatusServiceUnavailable, "Billing ledger is temporarily unavailable", "server_error")
		return
	}
	var responseRequests any = requests
	if len(requests) == 0 {
		responseRequests = nil
	}
	writeProviderJSON(w, http.StatusOK, map[string]any{"requests": responseRequests})
}

func (e *Edge) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.UpstreamURL+"/health", nil)
	response, err := e.Client.Do(req)
	if err != nil || response.StatusCode != http.StatusOK {
		if response != nil {
			response.Body.Close()
		}
		writeProviderError(w, http.StatusServiceUnavailable, "Model server is not ready", "server_error")
		return
	}
	response.Body.Close()
	writeProviderJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (e *Edge) completions(w http.ResponseWriter, r *http.Request) {
	identity := identityFromRequest(r)
	requestID := identity.requestID
	channel := identity.channel
	if requestID == "" || channel == "" {
		writeProviderError(w, http.StatusInternalServerError, "Marketplace request identity is unavailable", "server_error")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProviderRequestBytes))
	if err != nil {
		writeProviderError(w, http.StatusRequestEntityTooLarge, "Request body is too large", "invalid_request_error")
		return
	}
	var payload map[string]any
	if err = json.Unmarshal(body, &payload); err != nil {
		writeProviderError(w, http.StatusBadRequest, "Request body must be valid JSON", "invalid_request_error")
		return
	}
	model, _ := payload["model"].(string)
	if model != e.PublicModel {
		writeProviderError(w, http.StatusNotFound, "Unknown model", "invalid_request_error")
		return
	}
	if err = validateTextOnlyMessages(payload); err != nil {
		writeProviderError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	stream, _ := payload["stream"].(bool)
	translateReasoningRequest(payload)
	estimatedPrefillTokens := estimatePromptTokens(payload)
	if !e.channelAdmission.tryAcquire(channel) {
		w.Header().Set("Retry-After", "1")
		writeProviderError(w, http.StatusTooManyRequests, "Provider channel capacity is temporarily full", "rate_limit_error")
		e.recordReceipt(RequestReceipt{RequestID: requestID, Channel: channel, Model: e.PublicModel, UpstreamModel: e.UpstreamModel, Stream: stream, StatusCode: http.StatusTooManyRequests, Outcome: "channel_capacity_rejected"})
		return
	}
	defer e.channelAdmission.release(channel)
	decision := e.admission.tryAcquire(estimatedPrefillTokens)
	if decision != admissionAccepted {
		w.Header().Set("Retry-After", "1")
		message := "Provider capacity is temporarily full"
		if decision == admissionPrefillRejected {
			message = "Provider long-context capacity is temporarily full"
		}
		writeProviderError(w, http.StatusTooManyRequests, message, "rate_limit_error")
		e.recordReceipt(RequestReceipt{RequestID: requestID, Channel: channel, Model: e.PublicModel, UpstreamModel: e.UpstreamModel, Stream: stream, StatusCode: http.StatusTooManyRequests, Outcome: string(decision)})
		return
	}
	observation := admissionObservation{StatusCode: http.StatusInternalServerError, Outcome: "edge_error"}
	prefillReleased := false
	releasePrefill := func() {
		if !prefillReleased {
			e.admission.releasePrefill(estimatedPrefillTokens)
			prefillReleased = true
		}
	}
	defer func() {
		releasePrefill()
		e.admission.complete(observation)
	}()
	payload["model"] = e.UpstreamModel
	body, err = json.Marshal(payload)
	if err != nil {
		writeProviderError(w, http.StatusBadRequest, "Request could not be encoded", "invalid_request_error")
		return
	}
	request, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, e.UpstreamURL+"/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", r.Header.Get("Accept"))
	request.Header.Set("X-Request-ID", requestID)
	if e.UpstreamKey != "" {
		request.Header.Set("Authorization", "Bearer "+e.UpstreamKey)
	}
	started := time.Now()
	response, err := e.Client.Do(request)
	if err != nil {
		e.Logger.Error("provider upstream request failed", "request_id", requestID, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		writeProviderError(w, http.StatusBadGateway, "Model server request failed", "server_error")
		e.recordReceipt(RequestReceipt{RequestID: requestID, Channel: channel, Model: e.PublicModel, UpstreamModel: e.UpstreamModel, Stream: stream, StatusCode: http.StatusBadGateway, Outcome: "upstream_error", DurationMS: time.Since(started).Milliseconds()})
		observation = admissionObservation{StatusCode: http.StatusBadGateway, Outcome: "upstream_error", Duration: time.Since(started)}
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		metadata := e.copyEventStream(w, r, response.Body, requestID, started, releasePrefill)
		e.recordResponseReceipt(channel, requestID, stream, response.StatusCode, started, metadata)
		observation = admissionObservation{StatusCode: response.StatusCode, Outcome: metadata.outcome, TTFT: metadata.timeToFirstToken, Duration: time.Since(started), PromptTokens: metadata.promptTokens, CompletionTokens: metadata.completionTokens}
		return
	}
	releasePrefill()
	metadata := e.copyBufferedResponse(w, response.Body, requestID, started)
	e.recordResponseReceipt(channel, requestID, stream, response.StatusCode, started, metadata)
	observation = admissionObservation{StatusCode: response.StatusCode, Outcome: metadata.outcome, TTFT: metadata.timeToFirstToken, Duration: time.Since(started), PromptTokens: metadata.promptTokens, CompletionTokens: metadata.completionTokens}
}

// estimatePromptTokens is deliberately conservative and content-free. The
// edge does not embed a tokenizer or retain prompts; it estimates UTF-8 text
// at three bytes per token solely to protect prefill capacity.
func estimatePromptTokens(payload map[string]any) int64 {
	var textBytes int64
	for _, key := range []string{"messages", "prompt", "input", "tools", "response_format"} {
		textBytes += estimateTextBytes(payload[key])
	}
	if textBytes == 0 {
		return 1
	}
	return (textBytes + 2) / 3
}

func estimateTextBytes(value any) int64 {
	switch typed := value.(type) {
	case string:
		return int64(len([]byte(typed)))
	case []any:
		var total int64
		for _, item := range typed {
			total += estimateTextBytes(item)
		}
		return total
	case map[string]any:
		var total int64
		for _, item := range typed {
			total += estimateTextBytes(item)
		}
		return total
	default:
		return 0
	}
}

// translateReasoningRequest maps OpenRouter's provider-neutral reasoning
// controls onto the Qwen chat-template contract understood by the serving
// runtime. OpenRouter-only controls are removed so strict OpenAI-compatible
// runtimes do not reject an otherwise valid request.
func translateReasoningRequest(payload map[string]any) {
	enabled, configured := false, false
	effort := ""
	if requestedEffort, ok := payload["reasoning_effort"].(string); ok {
		enabled, configured = requestedEffort != "" && requestedEffort != "none", true
		if enabled {
			// Qwen3.8 consumes its native low, medium, and xhigh effort
			// values through the chat template. Preserve the caller's value
			// instead of collapsing every non-none effort into one mode.
			// Unsupported values are deliberately forwarded so the strict
			// model template rejects them rather than silently changing intent.
			effort = requestedEffort
		}
	}
	if include, ok := payload["include_reasoning"].(bool); ok {
		enabled, configured = include, true
		if !include {
			effort = ""
		}
	}
	if reasoning, exists := payload["reasoning"]; exists {
		switch value := reasoning.(type) {
		case bool:
			enabled, configured = value, true
			if !value {
				effort = ""
			}
		case map[string]any:
			if explicit, ok := value["enabled"].(bool); ok {
				enabled, configured = explicit, true
				if !explicit {
					effort = ""
				}
			} else if requestedEffort, ok := value["effort"].(string); ok {
				enabled, configured = requestedEffort != "" && requestedEffort != "none", true
				if enabled {
					effort = requestedEffort
				} else {
					effort = ""
				}
			} else {
				enabled, configured = true, true
			}
		}
	}
	delete(payload, "reasoning")
	delete(payload, "include_reasoning")
	delete(payload, "reasoning_effort")
	if !configured {
		return
	}
	kwargs, _ := payload["chat_template_kwargs"].(map[string]any)
	if kwargs == nil {
		kwargs = map[string]any{}
	}
	kwargs["enable_thinking"] = enabled
	kwargs["preserve_thinking"] = enabled
	if effort != "" {
		kwargs["reasoning_effort"] = effort
	} else if !enabled {
		delete(kwargs, "reasoning_effort")
	}
	payload["chat_template_kwargs"] = kwargs
}

type streamRead struct {
	data []byte
	err  error
}

type responseMetadata struct {
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	finishReason     string
	timeToFirstToken time.Duration
	outcome          string
}

type upstreamUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type upstreamResponseMetadata struct {
	Usage   upstreamUsage `json:"usage"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func (e *Edge) copyBufferedResponse(w http.ResponseWriter, body io.Reader, requestID string, started time.Time) responseMetadata {
	metadata := responseMetadata{outcome: "completed"}
	tee := io.TeeReader(body, flushWriter{writer: w})
	var upstream upstreamResponseMetadata
	if err := json.NewDecoder(tee).Decode(&upstream); err != nil {
		metadata.outcome = "invalid_upstream_response"
		e.Logger.Warn("provider upstream response could not be decoded", "request_id", requestID, "duration_ms", time.Since(started).Milliseconds(), "error", err)
	}
	// The decoder is allowed to stop after the first JSON value. Forward any
	// unread bytes so observation never changes the provider response body.
	if _, err := io.Copy(flushWriter{writer: w}, body); err != nil {
		metadata.outcome = "client_write_error"
		return metadata
	}
	metadata.promptTokens = upstream.Usage.PromptTokens
	metadata.completionTokens = upstream.Usage.CompletionTokens
	metadata.totalTokens = upstream.Usage.TotalTokens
	if len(upstream.Choices) > 0 {
		metadata.finishReason = upstream.Choices[0].FinishReason
	}
	return metadata
}

type flushWriter struct {
	writer io.Writer
}

func (w flushWriter) Write(data []byte) (int, error) {
	count, err := w.writer.Write(data)
	if flusher, ok := w.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return count, err
}

func (e *Edge) copyEventStream(w http.ResponseWriter, r *http.Request, body io.Reader, requestID string, started time.Time, onFirstToken func()) responseMetadata {
	metadata := responseMetadata{outcome: "completed"}
	inspector := newSSEMetadataInspector(started)
	interval := e.StreamKeepAliveInterval
	if interval <= 0 {
		interval = defaultStreamKeepAliveInterval
	}
	reads := make(chan streamRead, 1)
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			count, err := body.Read(buffer)
			chunk := streamRead{err: err}
			if count > 0 {
				chunk.data = append([]byte(nil), buffer[:count]...)
			}
			select {
			case reads <- chunk:
			case <-r.Context().Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	heartbeat := time.NewTimer(interval)
	defer heartbeat.Stop()
	flusher, _ := w.(http.Flusher)
	for {
		select {
		case <-r.Context().Done():
			metadata = inspector.metadata()
			metadata.outcome = "client_canceled"
			return metadata
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				metadata = inspector.metadata()
				metadata.outcome = "client_write_error"
				return metadata
			}
			if flusher != nil {
				flusher.Flush()
			}
			heartbeat.Reset(interval)
		case read := <-reads:
			if len(read.data) > 0 {
				if inspector.observe(read.data) && onFirstToken != nil {
					onFirstToken()
				}
				if _, err := w.Write(read.data); err != nil {
					metadata = inspector.metadata()
					metadata.outcome = "client_write_error"
					return metadata
				}
				if flusher != nil {
					flusher.Flush()
				}
				if !heartbeat.Stop() {
					select {
					case <-heartbeat.C:
					default:
					}
				}
				heartbeat.Reset(interval)
			}
			if read.err != nil {
				if read.err != io.EOF {
					e.Logger.Warn("provider upstream stream ended with error", "request_id", requestID, "duration_ms", time.Since(started).Milliseconds(), "error", read.err)
					metadata = inspector.metadata()
					metadata.outcome = "upstream_stream_error"
					return metadata
				}
				return inspector.metadata()
			}
		}
	}
}

type sseMetadataInspector struct {
	started time.Time
	pending []byte
	result  responseMetadata
}

func newSSEMetadataInspector(started time.Time) *sseMetadataInspector {
	return &sseMetadataInspector{started: started, result: responseMetadata{outcome: "completed"}}
}

func (i *sseMetadataInspector) observe(data []byte) bool {
	firstTokenBefore := i.result.timeToFirstToken
	i.pending = append(i.pending, data...)
	for {
		index := bytes.IndexByte(i.pending, '\n')
		if index < 0 {
			if len(i.pending) > 1<<20 {
				i.pending = nil
				i.result.outcome = "usage_unavailable"
			}
			return firstTokenBefore == 0 && i.result.timeToFirstToken > 0
		}
		line := bytes.TrimSpace(i.pending[:index])
		i.pending = i.pending[index+1:]
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(value, []byte("[DONE]")) || len(value) == 0 {
			continue
		}
		var event upstreamResponseMetadata
		if json.Unmarshal(value, &event) != nil {
			continue
		}
		if len(event.Choices) > 0 && i.result.timeToFirstToken == 0 {
			i.result.timeToFirstToken = time.Since(i.started)
		}
		if event.Usage.TotalTokens > 0 || event.Usage.PromptTokens > 0 || event.Usage.CompletionTokens > 0 {
			i.result.promptTokens = event.Usage.PromptTokens
			i.result.completionTokens = event.Usage.CompletionTokens
			i.result.totalTokens = event.Usage.TotalTokens
		}
		for _, choice := range event.Choices {
			if choice.FinishReason != "" {
				i.result.finishReason = choice.FinishReason
			}
		}
	}
}

func (i *sseMetadataInspector) metadata() responseMetadata {
	return i.result
}

func (e *Edge) recordResponseReceipt(channel, requestID string, stream bool, statusCode int, started time.Time, metadata responseMetadata) {
	receipt := RequestReceipt{
		RequestID:        requestID,
		Channel:          channel,
		Model:            e.PublicModel,
		UpstreamModel:    e.UpstreamModel,
		Stream:           stream,
		StatusCode:       statusCode,
		Outcome:          metadata.outcome,
		DurationMS:       time.Since(started).Milliseconds(),
		PromptTokens:     metadata.promptTokens,
		CompletionTokens: metadata.completionTokens,
		TotalTokens:      metadata.totalTokens,
		FinishReason:     metadata.finishReason,
	}
	if metadata.timeToFirstToken > 0 {
		receipt.TimeToFirstTokenMS = metadata.timeToFirstToken.Milliseconds()
	}
	e.recordReceipt(receipt)
}

// validateTextOnlyMessages keeps the public launch boundary aligned with the
// workload that was qualified and priced. The underlying checkpoint can parse
// visual inputs, but image and video tokenization have a different admission
// and cost profile and must not reach this route until separately qualified.
func validateTextOnlyMessages(payload map[string]any) error {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return nil
	}
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := block["type"].(string)
			if kind != "text" && kind != "input_text" {
				return errors.New("This model endpoint accepts text input only")
			}
		}
	}
	return nil
}

func (e *Edge) recordReceipt(receipt RequestReceipt) {
	if receipt.Channel == "" {
		receipt.Channel = ChannelOpenRouter
	}
	if receipt.StatusCode >= 200 && receipt.StatusCode < 400 && receipt.Outcome == "completed" {
		receipt.CostNanoUSD = requestCostNanoUSD(receipt.PromptTokens, receipt.CompletionTokens, e.channelPolicy(receipt.Channel))
	}
	if e.ReceiptRecorder != nil {
		if err := e.ReceiptRecorder.Record(receipt); err != nil {
			e.Logger.Error("provider receipt recording failed", "request_id", receipt.RequestID, "error", err)
		}
	}
	e.Logger.Info("provider request completed",
		"request_id", receipt.RequestID,
		"channel", receipt.Channel,
		"model", receipt.Model,
		"stream", receipt.Stream,
		"status_code", receipt.StatusCode,
		"outcome", receipt.Outcome,
		"duration_ms", receipt.DurationMS,
		"time_to_first_token_ms", receipt.TimeToFirstTokenMS,
		"prompt_tokens", receipt.PromptTokens,
		"completion_tokens", receipt.CompletionTokens,
		"total_tokens", receipt.TotalTokens,
		"finish_reason", receipt.FinishReason,
	)
}

func randomRequestID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(value)
}

func writeProviderError(w http.ResponseWriter, status int, message, kind string) {
	writeProviderJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind, "code": status}})
}

func writeProviderJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
