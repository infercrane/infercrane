package modelapirouting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/infercrane/infercrane/internal/supplieradapter"
)

type strictChatPayload struct {
	Model       string              `json:"model"`
	Messages    []strictChatMessage `json:"messages"`
	MaxTokens   *int                `json:"max_tokens"`
	Temperature *float64            `json:"temperature,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
}

type strictChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func normalizeStrictRequest(request ProxyRequest) (supplieradapter.Request, error) {
	body, err := json.Marshal(request.Payload)
	if err != nil {
		return supplieradapter.Request{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload strictChatPayload
	if err = decoder.Decode(&payload); err != nil {
		return supplieradapter.Request{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return supplieradapter.Request{}, errors.New("request must contain one JSON object")
	}
	if request.Operation != "chat" || payload.Model != request.ProductID {
		return supplieradapter.Request{}, errors.New("request operation or public model does not match the strict route")
	}
	normalized := supplieradapter.Request{
		ID: request.RequestID, Operation: supplieradapter.OperationChatCompletions, ModelID: request.ProductID,
		MaxOutputTokens: payload.MaxTokens, Temperature: payload.Temperature, Stream: payload.Stream,
		Messages: make([]supplieradapter.Message, 0, len(payload.Messages)),
	}
	for _, message := range payload.Messages {
		normalized.Messages = append(normalized.Messages, supplieradapter.Message{
			Role: message.Role, Content: []supplieradapter.ContentPart{{Type: "text", Text: message.Content}},
		})
	}
	return normalized, normalized.Validate()
}

type scopedCredentialResolver struct {
	resolver RuntimeCredentialResolver
	operator string
}

func (r scopedCredentialResolver) Resolve(ctx context.Context, reference string) ([]byte, error) {
	if r.resolver == nil {
		return nil, errors.New("hosted supplier credential resolver is unavailable")
	}
	return r.resolver.ResolveHostedModelCredential(ctx, r.operator, reference)
}

func (rt *Runtime) serveStrictCandidate(w http.ResponseWriter, r *http.Request, request ProxyRequest, lease Lease, candidate Candidate, reservation Reservation, adapter supplieradapter.Adapter, normalized supplieradapter.Request, now func() time.Time) {
	target := supplieradapter.Target{
		Supplier: candidate.Supplier, BaseURL: candidate.Endpoint, SupplierModelID: candidate.SupplierModelID,
		Region: "global", CredentialReference: candidate.CredentialReference,
	}
	upstream, err := adapter.BuildRequest(r.Context(), target, normalized, scopedCredentialResolver{resolver: rt.Credentials, operator: lease.Publication.OperatorTenantID})
	if err != nil {
		if tripsCandidateCircuit(err) {
			rt.observeCandidate(candidate.ID, false)
		}
		_ = rt.Billing.ReleaseUnsent(context.WithoutCancel(r.Context()), request.TenantID, reservation.ID, "strict supplier request failed before transmission")
		rt.writeStrictError(w, request, reservation.ID, err)
		return
	}
	upstream.Header.Set("traceparent", request.TraceParent)
	upstream.Header.Set("X-InferCrane-Attempt", "1")

	transmittedAt := now().UTC()
	if err = rt.Billing.MarkTransmitted(r.Context(), request.TenantID, reservation.ID, transmittedAt); err != nil {
		_ = rt.Billing.ReleaseUnsent(context.WithoutCancel(r.Context()), request.TenantID, reservation.ID, "billing transmission fence failed")
		writeError(w, http.StatusServiceUnavailable, "Usage authorization could not be fenced", "billing_error")
		return
	}
	client := rt.Client
	if client == nil {
		client = &http.Client{Transport: &http.Transport{MaxIdleConns: 512, MaxIdleConnsPerHost: 128, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 5 * time.Minute}}
	}
	strictClient := *client
	strictClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := strictClient.Do(upstream)
	if err != nil {
		rt.observeCandidate(candidate.ID, false)
		rt.settleStrict(request, reservation.ID, Usage{})
		if rt.Logger != nil {
			rt.Logger.Error("strict hosted model API supplier result is ambiguous", "request_id", request.RequestID, "reservation_id", reservation.ID, "error", err)
		}
		writeError(w, http.StatusServiceUnavailable, "Inference upstream is unavailable", "server_error")
		return
	}

	if err = rt.Billing.MarkResponseStarted(context.WithoutCancel(r.Context()), request.TenantID, reservation.ID, now().UTC()); err != nil && rt.Logger != nil {
		rt.Logger.Error("mark strict hosted response start", "request_id", request.RequestID, "reservation_id", reservation.ID, "error", err)
	}
	if normalized.Stream {
		rt.serveStrictStream(w, r, request, reservation.ID, candidate.ID, response, adapter)
		return
	}
	rt.serveStrictBuffered(w, r, request, reservation.ID, candidate.ID, response, adapter)
}

func (rt *Runtime) serveStrictBuffered(w http.ResponseWriter, r *http.Request, request ProxyRequest, reservationID, candidateID string, upstream *http.Response, adapter supplieradapter.Adapter) {
	status := upstream.StatusCode
	response, err := adapter.DecodeResponse(r.Context(), upstream)
	if err != nil {
		if tripsCandidateCircuit(err) {
			rt.observeCandidate(candidateID, false)
		}
		rt.settleStrict(request, reservationID, Usage{StatusCode: status})
		rt.writeStrictError(w, request, reservationID, err)
		return
	}
	usage := routingUsage(status, response.Usage)
	body, err := publicBufferedResponse(request.ProductID, response)
	if err != nil {
		rt.observeCandidate(candidateID, false)
		rt.settleStrict(request, reservationID, usage)
		writeError(w, http.StatusBadGateway, "Inference supplier returned an invalid response", "upstream_error")
		return
	}
	rt.observeCandidate(candidateID, true)
	copyResponseHeaders(w.Header(), upstream.Header)
	w.Header().Set("X-Request-ID", request.RequestID)
	w.Header().Set("traceparent", request.TraceParent)
	w.WriteHeader(http.StatusOK)
	_, copyErr := w.Write(body)
	rt.settleStrict(request, reservationID, usage)
	if copyErr != nil && rt.Logger != nil {
		rt.Logger.Error("write strict hosted model API response", "request_id", request.RequestID, "reservation_id", reservationID, "error", copyErr)
	}
}

func (rt *Runtime) serveStrictStream(w http.ResponseWriter, r *http.Request, request ProxyRequest, reservationID, candidateID string, upstream *http.Response, adapter supplieradapter.Adapter) {
	status := upstream.StatusCode
	stream, err := adapter.OpenStream(r.Context(), upstream)
	if err != nil {
		if tripsCandidateCircuit(err) {
			rt.observeCandidate(candidateID, false)
		}
		rt.settleStrict(request, reservationID, Usage{StatusCode: status})
		rt.writeStrictError(w, request, reservationID, err)
		return
	}
	defer stream.Close()
	copyResponseHeaders(w.Header(), upstream.Header)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Request-ID", request.RequestID)
	w.Header().Set("traceparent", request.TraceParent)
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	usage := Usage{StatusCode: status}
	var copyErr error
	for {
		event, nextErr := stream.Next(r.Context())
		if errors.Is(nextErr, io.EOF) {
			_, copyErr = io.WriteString(w, "data: [DONE]\n\n")
			if copyErr == nil {
				copyErr = controller.Flush()
			}
			break
		}
		if nextErr != nil {
			copyErr = nextErr
			break
		}
		if event.Usage != nil {
			usage = routingUsage(status, *event.Usage)
		}
		body, encodeErr := publicStreamEvent(request.RequestID, request.ProductID, event)
		if encodeErr != nil {
			copyErr = encodeErr
			break
		}
		if _, copyErr = fmt.Fprintf(w, "data: %s\n\n", body); copyErr != nil {
			break
		}
		if copyErr = controller.Flush(); copyErr != nil {
			break
		}
	}
	if copyErr == nil {
		rt.observeCandidate(candidateID, true)
	} else if tripsCandidateCircuit(copyErr) {
		rt.observeCandidate(candidateID, false)
	}
	rt.settleStrict(request, reservationID, usage)
	if copyErr != nil && rt.Logger != nil {
		rt.Logger.Error("strict hosted model API stream incomplete", "request_id", request.RequestID, "reservation_id", reservationID, "error", copyErr)
	}
}

func tripsCandidateCircuit(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var supplierErr *supplieradapter.Error
	if !errors.As(err, &supplierErr) {
		return true
	}
	switch supplierErr.Code {
	case supplieradapter.ErrorInvalidRequest, supplieradapter.ErrorCancelled:
		return false
	default:
		return true
	}
}

func (rt *Runtime) settleStrict(request ProxyRequest, reservationID string, usage Usage) {
	settlementContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rt.Billing.Settle(settlementContext, request.TenantID, reservationID, usage)
	if err != nil && rt.Logger != nil {
		rt.Logger.Error("retain strict hosted supplier request for reconciliation", "request_id", request.RequestID, "reservation_id", reservationID, "error", err)
	}
}

func (rt *Runtime) writeStrictError(w http.ResponseWriter, request ProxyRequest, reservationID string, err error) {
	var supplierErr *supplieradapter.Error
	if !errors.As(err, &supplierErr) {
		writeError(w, http.StatusServiceUnavailable, "Inference upstream is unavailable", "server_error")
		return
	}
	status, errorType, message := http.StatusServiceUnavailable, "upstream_error", "Inference upstream is unavailable"
	switch supplierErr.Code {
	case supplieradapter.ErrorInvalidRequest:
		status, errorType, message = http.StatusBadRequest, "invalid_request_error", "Request is outside the qualified model contract"
	case supplieradapter.ErrorRateLimited:
		status, message = http.StatusTooManyRequests, "Inference capacity is temporarily limited"
	case supplieradapter.ErrorProtocol:
		status, message = http.StatusBadGateway, "Inference supplier returned an invalid response"
	case supplieradapter.ErrorTimeout:
		status, message = http.StatusGatewayTimeout, "Inference upstream timed out"
	}
	if rt.Logger != nil {
		rt.Logger.Warn("strict hosted model API request failed", "request_id", request.RequestID, "reservation_id", reservationID, "code", supplierErr.Code, "supplier_request_id", supplierErr.SupplierRequestID)
	}
	writeError(w, status, message, errorType)
}

func routingUsage(status int, usage supplieradapter.Usage) Usage {
	return Usage{StatusCode: status, InputTokens: intToken(usage.InputTokens), CachedInputTokens: intToken(usage.CachedInput), OutputTokens: intToken(usage.OutputTokens)}
}

func intToken(value *int64) *int {
	if value == nil || *value < 0 || uint64(*value) > uint64(math.MaxInt) {
		return nil
	}
	converted := int(*value)
	return &converted
}

func publicBufferedResponse(publicModel string, response supplieradapter.Response) ([]byte, error) {
	choices := make([]map[string]any, 0, len(response.Choices))
	for _, choice := range response.Choices {
		if choice.Message == nil || len(choice.Message.Content) != 1 || choice.Message.Content[0].Type != "text" {
			return nil, errors.New("normalized response choice is not public chat text")
		}
		choices = append(choices, map[string]any{
			"index": choice.Index, "message": map[string]any{"role": choice.Message.Role, "content": choice.Message.Content[0].Text},
			"finish_reason": choice.FinishReason,
		})
	}
	return json.Marshal(map[string]any{
		"id": response.ID, "object": "chat.completion", "model": publicModel, "choices": choices, "usage": publicUsage(response.Usage),
	})
}

func publicStreamEvent(requestID, publicModel string, event supplieradapter.StreamEvent) ([]byte, error) {
	choices := make([]map[string]any, 0, 1)
	if event.Type != supplieradapter.StreamEventUsage {
		delta := map[string]any{}
		finish := any(nil)
		if event.TextDelta != "" {
			delta["content"] = event.TextDelta
		}
		if event.FinishReason != "" {
			finish = event.FinishReason
		}
		choices = append(choices, map[string]any{"index": event.ChoiceIndex, "delta": delta, "finish_reason": finish})
	}
	payload := map[string]any{"id": requestID, "object": "chat.completion.chunk", "model": publicModel, "choices": choices}
	if event.Usage != nil {
		payload["usage"] = publicUsage(*event.Usage)
	}
	return json.Marshal(payload)
}

func publicUsage(usage supplieradapter.Usage) map[string]any {
	prompt, completion := int64(0), int64(0)
	if usage.InputTokens != nil {
		prompt = *usage.InputTokens
	}
	if usage.OutputTokens != nil {
		completion = *usage.OutputTokens
	}
	out := map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion}
	if usage.CachedInput != nil {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": *usage.CachedInput}
	}
	if usage.ReasoningTokens != nil {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": *usage.ReasoningTokens}
	}
	return out
}
