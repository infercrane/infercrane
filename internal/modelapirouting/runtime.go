package modelapirouting

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/openaicompat"
	"github.com/infercrane/infercrane/internal/supplieradapter"
)

type ProxyRequest struct {
	TenantID, ProductID, Operation, Resource, RequestID, TraceParent string
	Payload                                                          map[string]any
}

// Runtime is the supplier-neutral hosted request boundary. It receives a
// tenant already authenticated by Gateway, resolves an in-memory product
// lease, reserves prepaid balance, then makes at most one supplier attempt.
type Runtime struct {
	Routes      *Directory
	Billing     Billing
	Client      *http.Client
	Logger      *slog.Logger
	Adapters    *supplieradapter.Registry
	Credentials RuntimeCredentialResolver
	Circuit     CandidateCircuit
	now         func() time.Time
}

func (rt *Runtime) ServeHTTP(w http.ResponseWriter, r *http.Request, request ProxyRequest) {
	if rt == nil || rt.Routes == nil || rt.Billing == nil {
		writeError(w, http.StatusServiceUnavailable, "Hosted Model API routing is unavailable", "server_error")
		return
	}
	lease, err := rt.Routes.Acquire(request.TenantID, request.ProductID)
	if err != nil {
		status, errorType, message := http.StatusNotFound, "invalid_request_error", "Unknown or unavailable model: "+request.ProductID
		if errors.Is(err, ErrUnauthorized) {
			status, errorType, message = http.StatusForbidden, "permission_error", "API key is not entitled to this model"
		}
		writeError(w, status, message, errorType)
		return
	}
	orderedCandidates := lease.CandidatesForRequest(request.RequestID, request.Operation)
	var candidate *Candidate
	for index := range orderedCandidates {
		if rt.Circuit == nil || rt.Circuit.Allow(orderedCandidates[index].ID, rt.currentTime()) {
			candidate = &orderedCandidates[index]
			break
		}
	}
	if candidate == nil {
		writeError(w, http.StatusUnprocessableEntity, "Model does not support this API operation", "unsupported_protocol")
		return
	}
	var strictAdapter supplieradapter.Adapter
	var strictRequest supplieradapter.Request
	if candidate.Adapter != "" {
		var exists bool
		strictAdapter, exists = rt.Adapters.Lookup(candidate.Adapter)
		if !exists {
			writeError(w, http.StatusServiceUnavailable, "Inference upstream is unavailable", "server_error")
			return
		}
		strictRequest, err = normalizeStrictRequest(request)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Request is outside the qualified model contract", "invalid_request_error")
			return
		}
	}

	now := time.Now
	if rt.now != nil {
		now = rt.now
	}
	reservationRequest := ReservationRequest{
		ID: randomReservationID(), TenantID: request.TenantID, ProductID: request.ProductID,
		EntitlementID: lease.Entitlement.ID, OperatorTenantID: lease.Entitlement.OperatorTenantID,
		ServingPlanID: lease.Entitlement.ServingPlanID, SupplyPlanID: lease.Publication.SupplyPlanID,
		RetailRate: lease.Rate, MaxRequestMicrousd: lease.Entitlement.MaxRequestMicrousd,
		CandidateID: candidate.ID, OfferID: candidate.OfferID, OfferVersion: candidate.OfferVersion, Supplier: candidate.Supplier,
		SupplierModelID: candidate.SupplierModelID, TargetBindingID: candidate.TargetBindingID,
		TargetBindingDigest: candidate.TargetBindingDigest, CreatedAt: now().UTC(),
	}
	reservation, err := rt.Billing.Reserve(r.Context(), reservationRequest)
	if err != nil {
		if errors.Is(err, ErrInsufficientPrepaidBalance) {
			writeError(w, http.StatusPaymentRequired, "Prepaid Model API balance is insufficient", "billing_error")
			return
		}
		if rt.Logger != nil {
			rt.Logger.Error("reserve hosted Model API usage", "request_id", request.RequestID, "error", err)
		}
		writeError(w, http.StatusServiceUnavailable, "Usage authorization is temporarily unavailable", "billing_error")
		return
	}
	if strictAdapter != nil {
		rt.serveStrictCandidate(w, r, request, lease, *candidate, reservation, strictAdapter, strictRequest, now)
		return
	}

	payload := make(map[string]any, len(request.Payload))
	for key, value := range request.Payload {
		payload[key] = value
	}
	payload["model"] = candidate.SupplierModelID
	forceStreamUsage(payload)
	body, err := json.Marshal(payload)
	if err != nil {
		_ = rt.Billing.ReleaseUnsent(context.WithoutCancel(r.Context()), request.TenantID, reservation.ID, "request encoding failed before supplier transmission")
		writeError(w, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error")
		return
	}
	target, err := openaicompat.Endpoint(candidate.Endpoint, request.Resource)
	if err != nil {
		_ = rt.Billing.ReleaseUnsent(context.WithoutCancel(r.Context()), request.TenantID, reservation.ID, "qualified endpoint became invalid before supplier transmission")
		writeError(w, http.StatusServiceUnavailable, "Inference upstream is unavailable", "server_error")
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		_ = rt.Billing.ReleaseUnsent(context.WithoutCancel(r.Context()), request.TenantID, reservation.ID, "request construction failed before supplier transmission")
		writeError(w, http.StatusServiceUnavailable, "Inference upstream is unavailable", "server_error")
		return
	}
	copyRequestHeaders(upstream.Header, r.Header)
	upstream.Header.Set("Accept-Encoding", "identity")
	upstream.Header.Set("Authorization", "Bearer "+candidate.Credential)
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("X-Request-ID", request.RequestID)
	upstream.Header.Set("traceparent", request.TraceParent)
	upstream.Header.Set("X-InferCrane-Attempt", "1")

	// Mark first, then transmit. Any error after this point is ambiguous and
	// must retain the reservation for reconciliation; it is never retried.
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
	response, err := client.Do(upstream)
	if err != nil {
		rt.observeCandidate(candidate.ID, false)
		settlementContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		_, _ = rt.Billing.Settle(settlementContext, request.TenantID, reservation.ID, Usage{})
		cancel()
		if rt.Logger != nil {
			rt.Logger.Error("hosted model API supplier result is ambiguous", "request_id", request.RequestID, "reservation_id", reservation.ID, "error", err)
		}
		writeError(w, http.StatusServiceUnavailable, "Inference upstream is unavailable", "server_error")
		return
	}
	defer response.Body.Close()
	responseStartedAt := now().UTC()
	if err = rt.Billing.MarkResponseStarted(context.WithoutCancel(r.Context()), request.TenantID, reservation.ID, responseStartedAt); err != nil && rt.Logger != nil {
		rt.Logger.Error("mark hosted response start", "request_id", request.RequestID, "reservation_id", reservation.ID, "error", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		rt.observeCandidate(candidate.ID, false)
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		settlementContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		_, settleErr := rt.Billing.Settle(settlementContext, request.TenantID, reservation.ID, Usage{StatusCode: response.StatusCode})
		cancel()
		if settleErr != nil && rt.Logger != nil {
			rt.Logger.Error("retain failed hosted supplier request for reconciliation", "request_id", request.RequestID, "reservation_id", reservation.ID, "error", settleErr)
		}
		writeError(w, response.StatusCode, "Inference supplier could not serve the request", "upstream_error")
		return
	}
	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Set("X-Request-ID", request.RequestID)
	w.Header().Set("traceparent", request.TraceParent)
	w.WriteHeader(response.StatusCode)
	input, output, copyErr := copyPublicResponse(w, response, request.ProductID)
	rt.observeCandidate(candidate.ID, copyErr == nil)
	settlementContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	_, settleErr := rt.Billing.Settle(settlementContext, request.TenantID, reservation.ID, Usage{StatusCode: response.StatusCode, InputTokens: input, OutputTokens: output})
	cancel()
	if (copyErr != nil || settleErr != nil) && rt.Logger != nil {
		rt.Logger.Error("hosted model API response incomplete", "request_id", request.RequestID, "reservation_id", reservation.ID, "copy_error", copyErr, "settle_error", settleErr)
	}
	// An upstream response or partial body is terminal. Never fail over after
	// response start, even when the supplier status is retryable.
}

func (rt *Runtime) currentTime() time.Time {
	now := time.Now
	if rt != nil && rt.now != nil {
		now = rt.now
	}
	return now().UTC()
}

func (rt *Runtime) observeCandidate(candidateID string, success bool) {
	if rt != nil && rt.Circuit != nil {
		rt.Circuit.Observe(candidateID, success, rt.currentTime())
	}
}

func forceStreamUsage(payload map[string]any) {
	stream, _ := payload["stream"].(bool)
	if !stream {
		return
	}
	options := map[string]any{}
	if supplied, ok := payload["stream_options"].(map[string]any); ok {
		for key, value := range supplied {
			options[key] = value
		}
	}
	options["include_usage"] = true
	payload["stream_options"] = options
}

func randomReservationID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "model_usage_" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "model_usage_" + hex.EncodeToString(value)
}

var requestHeaderAllowlist = []string{"Accept", "OpenAI-Beta"}

var responseHeaderAllowlist = []string{"Cache-Control"}

func copyRequestHeaders(destination, source http.Header) {
	copyAllowedHeaders(destination, source, requestHeaderAllowlist)
}

func copyResponseHeaders(destination, source http.Header) {
	copyAllowedHeaders(destination, source, responseHeaderAllowlist)
	// Supplier and proxy fingerprints (Server, Via, X-*), cookies, redirects,
	// and transport-specific headers are deliberately private. Content-Type is
	// normalized because the public response body is parsed and rewritten here.
	if isEventStream(source.Get("Content-Type")) {
		destination.Set("Content-Type", "text/event-stream; charset=utf-8")
	} else {
		destination.Set("Content-Type", "application/json")
	}
}

func copyAllowedHeaders(destination, source http.Header, allowlist []string) {
	const maxForwardedHeaderBytes = 8 << 10
	forwarded := 0
	for _, name := range allowlist {
		for _, value := range source.Values(name) {
			value = strings.TrimSpace(value)
			if value == "" || !safeHeaderValue(value) || forwarded+len(name)+len(value) > maxForwardedHeaderBytes {
				continue
			}
			destination.Add(name, value)
			forwarded += len(name) + len(value)
		}
	}
}

func safeHeaderValue(value string) bool {
	for _, character := range value {
		if (character < 0x20 && character != '\t') || character == 0x7f {
			return false
		}
	}
	return true
}

func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}

type usageObserver struct {
	input, output *int
}

type tokenUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	InputTokens      *int `json:"input_tokens"`
	OutputTokens     *int `json:"output_tokens"`
}

func (o *usageObserver) observeJSON(body []byte) {
	var raw struct {
		Usage    *tokenUsage `json:"usage"`
		Response *struct {
			Usage *tokenUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return
	}
	usage := raw.Usage
	if usage == nil && raw.Response != nil {
		usage = raw.Response.Usage
	}
	if usage == nil {
		return
	}
	input, output := usage.InputTokens, usage.OutputTokens
	if input == nil {
		input = usage.PromptTokens
	}
	if output == nil {
		output = usage.CompletionTokens
	}
	if input != nil {
		value := *input
		o.input = &value
	}
	if output != nil {
		value := *output
		o.output = &value
	}
}

func (o usageObserver) usage() (*int, *int) { return o.input, o.output }

const (
	maxSSELineBytes  = 256 << 10
	maxSSEEventBytes = 2 << 20
	maxBufferedBytes = 32 << 20
)

func copyPublicResponse(w http.ResponseWriter, response *http.Response, publicModelID string) (*int, *int, error) {
	observer := &usageObserver{}
	if isEventStream(response.Header.Get("Content-Type")) {
		return copyPublicSSE(w, response.Body, publicModelID, observer)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBufferedBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > maxBufferedBytes {
		return nil, nil, errors.New("supplier response exceeds the 32 MiB limit")
	}
	observer.observeJSON(body)
	body, err = rewriteJSONModel(body, publicModelID)
	if err != nil {
		input, output := observer.usage()
		return input, output, fmt.Errorf("malformed supplier JSON response: %w", err)
	}
	_, err = w.Write(body)
	input, output := observer.usage()
	return input, output, err
}

func copyPublicSSE(w http.ResponseWriter, body io.Reader, publicModelID string, observer *usageObserver) (*int, *int, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), maxSSELineBytes+1)
	scanner.Split(splitSSELines)
	controller := http.NewResponseController(w)
	var event [][]byte
	eventBytes := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > maxSSELineBytes {
			input, output := observer.usage()
			return input, output, errors.New("supplier SSE line exceeds limit")
		}
		if len(line) != 0 {
			if bytes.IndexByte(line, 0) >= 0 {
				input, output := observer.usage()
				return input, output, errors.New("supplier SSE line contains NUL")
			}
			eventBytes += len(line) + 1
			if eventBytes > maxSSEEventBytes {
				input, output := observer.usage()
				return input, output, errors.New("supplier SSE event exceeds limit")
			}
			event = append(event, bytes.Clone(line))
			continue
		}
		if len(event) == 0 {
			continue
		}
		rewritten, terminal, data, err := rewriteSSEEvent(event, publicModelID)
		if err != nil {
			input, output := observer.usage()
			return input, output, err
		}
		if len(data) != 0 {
			observer.observeJSON(data)
		}
		if _, err = w.Write(rewritten); err != nil {
			input, output := observer.usage()
			return input, output, err
		}
		if err = controller.Flush(); err != nil {
			input, output := observer.usage()
			return input, output, err
		}
		if terminal {
			input, output := observer.usage()
			return input, output, nil
		}
		event, eventBytes = event[:0], 0
	}
	input, output := observer.usage()
	if err := scanner.Err(); err != nil {
		return input, output, fmt.Errorf("read supplier SSE stream: %w", err)
	}
	if len(event) != 0 {
		return input, output, errors.New("supplier SSE stream ended with an unterminated event")
	}
	return input, output, nil
}

func splitSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for index, character := range data {
		switch character {
		case '\n':
			return index + 1, data[:index], nil
		case '\r':
			if index+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			advance = index + 1
			if index+1 < len(data) && data[index+1] == '\n' {
				advance++
			}
			return advance, data[:index], nil
		}
	}
	if atEOF && len(data) != 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func rewriteSSEEvent(lines [][]byte, publicModelID string) (rewritten []byte, terminal bool, observedData []byte, err error) {
	dataValues := make([][]byte, 0, len(lines))
	firstDataLine := -1
	for index, line := range lines {
		field, value := sseField(line)
		if bytes.Equal(field, []byte("data")) {
			if firstDataLine < 0 {
				firstDataLine = index
			}
			dataValues = append(dataValues, value)
		}
	}
	if firstDataLine < 0 {
		for _, line := range lines {
			rewritten = append(rewritten, line...)
			rewritten = append(rewritten, '\n')
		}
		return append(rewritten, '\n'), false, nil, nil
	}
	data := bytes.Join(dataValues, []byte{'\n'})
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		terminal = true
	} else {
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, false, nil, errors.New("supplier SSE event has empty data")
		}
		observedData = bytes.Clone(data)
		data, err = rewriteJSONModel(data, publicModelID)
		if err != nil {
			return nil, false, nil, fmt.Errorf("malformed supplier SSE data: %w", err)
		}
	}
	for index, line := range lines {
		field, _ := sseField(line)
		if bytes.Equal(field, []byte("data")) {
			if index == firstDataLine {
				rewritten = append(rewritten, "data: "...)
				rewritten = append(rewritten, data...)
				rewritten = append(rewritten, '\n')
			}
			continue
		}
		rewritten = append(rewritten, line...)
		rewritten = append(rewritten, '\n')
	}
	return append(rewritten, '\n'), terminal, observedData, nil
}

func sseField(line []byte) (field, value []byte) {
	if len(line) == 0 || line[0] == ':' {
		return nil, nil
	}
	separator := bytes.IndexByte(line, ':')
	if separator < 0 {
		return line, nil
	}
	field, value = line[:separator], line[separator+1:]
	if len(value) != 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}

func rewriteJSONModel(body []byte, publicModelID string) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("expected a JSON object")
	}
	model, err := json.Marshal(publicModelID)
	if err != nil {
		return nil, err
	}
	object["model"] = model
	return json.Marshal(object)
}

func writeError(w http.ResponseWriter, status int, message, errorType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": errorType, "param": nil, "code": nil}})
}
