package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/routes"
)

type Recorder interface {
	RecordRequest(context.Context, domain.InferenceRecord) error
}
type CapacityObserver func(context.Context, string) (int, error)
type Gateway struct {
	Routes    *routes.Directory
	APIKey    string
	Client    *http.Client
	Recorder  Recorder
	Logger    *slog.Logger
	Ready     func(context.Context) error
	Telemetry *Telemetry
	Control   http.Handler
	// CapacityObservers provide request-arrival evidence for provider-native
	// serverless routes. The lookup is bounded and best-effort: inference must
	// remain available when telemetry observation fails.
	CapacityObservers map[string]CapacityObserver
	Authenticator     interface {
		AuthenticatePrincipal(context.Context, string) (domain.Principal, error)
	}
}
type principalKey struct{}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", g.health)
	mux.HandleFunc("GET /livez", g.health)
	mux.HandleFunc("GET /readyz", g.ready)
	mux.HandleFunc("GET /v1/models", g.auth(g.models))
	if g.Telemetry == nil {
		g.Telemetry = &Telemetry{}
	}
	mux.Handle("GET /metrics", g.Telemetry)
	if g.Control != nil {
		mux.Handle("/api/v1/", g.Control)
	}
	mux.HandleFunc("POST /v1/chat/completions", g.Telemetry.Observe(g.auth(g.completions)))
	return mux
}
func (g *Gateway) ready(w http.ResponseWriter, r *http.Request) {
	if g.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := g.Ready(ctx); err != nil {
			openAIError(w, "Service is not ready", http.StatusServiceUnavailable, "server_error")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
func (g *Gateway) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actual, expected := r.Header.Get("Authorization"), "Bearer "+g.APIKey
		token := strings.TrimPrefix(actual, "Bearer ")
		var principal domain.Principal
		if g.APIKey != "" && subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1 {
			principal = domain.Principal{ID: "bootstrap", TenantID: "global", Name: "bootstrap", Role: string(authz.Admin)}
		} else if g.Authenticator != nil && token != "" {
			resolved, err := g.Authenticator.AuthenticatePrincipal(r.Context(), token)
			if err == nil {
				principal = resolved
			}
		}
		if principal.ID == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			openAIError(w, "Invalid API key", http.StatusUnauthorized, "authentication_error")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
	}
}
func (g *Gateway) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "deployments": len(g.Routes.List())})
}
func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	data := make([]map[string]string, 0)
	principal := r.Context().Value(principalKey{}).(domain.Principal)
	for _, route := range g.Routes.ListForTenant(principal.TenantID) {
		data = append(data, map[string]string{"id": route.Alias, "object": "model", "owned_by": "deployment"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
func (g *Gateway) completions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		openAIError(w, "Invalid JSON body", http.StatusBadRequest, "invalid_request_error")
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		openAIError(w, "Invalid JSON body", http.StatusBadRequest, "invalid_request_error")
		return
	}
	alias, ok := payload["model"].(string)
	if !ok || alias == "" {
		openAIError(w, "The 'model' field is required", http.StatusBadRequest, "invalid_request_error")
		return
	}
	principal := r.Context().Value(principalKey{}).(domain.Principal)
	route, ok := g.Routes.GetForTenant(principal.TenantID, alias)
	if !ok {
		openAIError(w, "Unknown model alias: "+alias, http.StatusNotFound, "invalid_request_error")
		return
	}
	payload["model"] = route.UpstreamModel
	streaming, _ := payload["stream"].(bool)
	body, err = json.Marshal(payload)
	if err != nil {
		openAIError(w, "Invalid JSON body", http.StatusBadRequest, "invalid_request_error")
		return
	}
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" || len(requestID) > 128 {
		requestID = "req_" + randomID()
	}
	traceParent := r.Header.Get("traceparent")
	if !validTraceParent.MatchString(traceParent) {
		traceParent = "00-" + randomHex(16) + "-" + randomHex(8) + "-01"
	}
	started := time.Now()
	evidenceResult := g.observeCapacity(r.Context(), route, started)
	if route.ComputeMode == "serverless" && g.CapacityObservers[route.Provider] == nil && route.ProviderWorkers != nil && !route.ProviderObservedAt.IsZero() {
		age := started.Sub(route.ProviderObservedAt)
		if age >= 0 && age <= 30*time.Second {
			workers := *route.ProviderWorkers
			cold := workers == 0
			observedAt := route.ProviderObservedAt
			evidenceResult = resolvedCapacityEvidence(capacityEvidence{coldStart: &cold, workers: &workers, observedAt: &observedAt})
			if cold {
				invalidated := route
				invalidated.ProviderWorkers, invalidated.ProviderObservedAt = nil, time.Time{}
				g.Routes.Put(invalidated)
			}
		}
	}
	target, err := url.JoinPath(route.RouterURL, "v1/chat/completions")
	if err != nil {
		openAIError(w, "Inference upstream is unavailable", http.StatusServiceUnavailable, "server_error")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		openAIError(w, "Inference upstream is unavailable", http.StatusServiceUnavailable, "server_error")
		return
	}
	copyHeaders(req.Header, r.Header)
	if route.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+route.UpstreamAPIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("traceparent", traceParent)
	client := g.Client
	if client == nil {
		client = &http.Client{Transport: &http.Transport{MaxIdleConns: 512, MaxIdleConnsPerHost: 128, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 5 * time.Minute}}
	}
	resp, err := client.Do(req)
	if err != nil {
		status, errorType, message := http.StatusServiceUnavailable, "upstream_unavailable", "Inference upstream is unavailable"
		if errors.Is(err, context.DeadlineExceeded) {
			status, errorType, message = http.StatusGatewayTimeout, "timeout", "Inference upstream timed out"
		}
		g.record(r.Context(), requestID, route, alias, started, status, errorType, streaming, responseObservation{}, <-evidenceResult)
		openAIError(w, message, status, "server_error")
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("traceparent", traceParent)
	w.WriteHeader(resp.StatusCode)
	observation := responseObservation{}
	copyErr := copyResponse(w, resp, &observation)
	errorType := ""
	if copyErr != nil {
		if r.Context().Err() != nil {
			errorType = "client_cancelled"
		} else {
			errorType = "upstream_disconnect"
		}
	}
	g.record(context.WithoutCancel(r.Context()), requestID, route, alias, started, resp.StatusCode, errorType, streaming, observation, <-evidenceResult)
	if g.Logger != nil {
		g.Logger.Info("inference request", "request_id", requestID, "traceparent", traceParent, "tenant_id", principal.TenantID, "deployment_id", route.DeploymentID, "status", resp.StatusCode, "duration_ms", float64(time.Since(started).Microseconds())/1000)
	}
}

type capacityEvidence struct {
	coldStart  *bool
	workers    *int
	observedAt *time.Time
}

func resolvedCapacityEvidence(evidence capacityEvidence) <-chan capacityEvidence {
	result := make(chan capacityEvidence, 1)
	result <- evidence
	return result
}

func (g *Gateway) observeCapacity(ctx context.Context, route routes.Snapshot, started time.Time) <-chan capacityEvidence {
	observer := g.CapacityObservers[route.Provider]
	if route.ComputeMode != "serverless" || observer == nil || route.ProviderResourceID == "" {
		return resolvedCapacityEvidence(capacityEvidence{})
	}
	result := make(chan capacityEvidence, 1)
	go func() {
		observeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		workers, err := observer(observeCtx, route.ProviderResourceID)
		if err != nil {
			result <- capacityEvidence{}
			return
		}
		cold := workers == 0
		observedAt := time.Now()
		if observedAt.Before(started) {
			observedAt = started
		}
		result <- capacityEvidence{coldStart: &cold, workers: &workers, observedAt: &observedAt}
	}()
	return result
}

func (g *Gateway) record(ctx context.Context, id string, route routes.Snapshot, requestModel string, started time.Time, status int, errorType string, streaming bool, observation responseObservation, evidence capacityEvidence) {
	if g.Recorder == nil {
		return
	}
	latency := float64(time.Since(started).Microseconds()) / 1000
	var ttft *float64
	if !observation.firstByteAt.IsZero() {
		value := float64(observation.firstByteAt.Sub(started).Microseconds()) / 1000
		ttft = &value
	}
	responseModel, inputTokens, outputTokens := observation.usage()
	record := domain.InferenceRecord{RequestID: id, DeploymentID: route.DeploymentID, RevisionID: route.RevisionID, Provider: route.Provider, Runtime: route.Runtime, ComputeMode: route.ComputeMode, OperationName: "chat", RequestModel: requestModel, ResponseModel: responseModel, SemanticConventionSchema: "https://opentelemetry.io/schemas/gen-ai/1.42.0", StartedAt: started, StatusCode: status, LatencyMS: latency, TTFTMS: ttft, InputTokens: inputTokens, OutputTokens: outputTokens, Streaming: streaming, ErrorType: errorType, ColdStart: evidence.coldStart, ProviderWorkersAtArrival: evidence.workers, ProviderCapacityObservedAt: evidence.observedAt}
	if err := g.Recorder.RecordRequest(ctx, record); err != nil && g.Logger != nil {
		g.Logger.Error("record request", "error", err, "request_id", id)
	}
}

var hopHeaders = map[string]struct{}{"Connection": {}, "Keep-Alive": {}, "Proxy-Authenticate": {}, "Proxy-Authorization": {}, "Te": {}, "Trailer": {}, "Transfer-Encoding": {}, "Upgrade": {}, "Content-Length": {}, "Host": {}}
var copyBuffers = sync.Pool{New: func() any { buffer := make([]byte, 32<<10); return &buffer }}

const observationLimit = 2 << 20

type responseObservation struct {
	firstByteAt time.Time
	body        []byte
}

func (o *responseObservation) observe(body []byte) {
	if len(body) > 0 && o.firstByteAt.IsZero() {
		o.firstByteAt = time.Now()
	}
	remaining := observationLimit - len(o.body)
	if remaining > len(body) {
		remaining = len(body)
	}
	if remaining > 0 {
		o.body = append(o.body, body[:remaining]...)
	}
}

func (o responseObservation) usage() (string, *int, *int) {
	var response struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
		} `json:"usage"`
	}
	decode := func(value []byte) bool { return json.Unmarshal(value, &response) == nil }
	if !decode(o.body) {
		for _, line := range bytes.Split(o.body, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(value, []byte("[DONE]")) {
				continue
			}
			var chunk struct {
				Model string `json:"model"`
				Usage *struct {
					PromptTokens     *int `json:"prompt_tokens"`
					CompletionTokens *int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(value, &chunk) == nil {
				if chunk.Model != "" {
					response.Model = chunk.Model
				}
				if chunk.Usage != nil {
					response.Usage.PromptTokens = chunk.Usage.PromptTokens
					response.Usage.CompletionTokens = chunk.Usage.CompletionTokens
				}
			}
		}
	}
	return response.Model, response.Usage.PromptTokens, response.Usage.CompletionTokens
}

func copyResponse(w http.ResponseWriter, resp *http.Response, observation *responseObservation) error {
	buffer := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(buffer)
	streaming := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	for {
		n, err := resp.Body.Read(*buffer)
		if n > 0 {
			observation.observe((*buffer)[:n])
			if _, writeErr := w.Write((*buffer)[:n]); writeErr != nil {
				return writeErr
			}
			if streaming {
				if flushErr := http.NewResponseController(w).Flush(); flushErr != nil {
					return flushErr
				}
				// Some provider-native OpenAI proxies keep the HTTP body open after
				// the terminal SSE marker. The protocol is complete at [DONE]; do
				// not wait for EOF and misclassify the client's normal exit as a
				// cancellation.
				if hasSSEDone(observation.body) {
					return nil
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func hasSSEDone(body []byte) bool {
	for _, line := range bytes.Split(body, []byte("\n")) {
		if bytes.Equal(bytes.TrimSpace(line), []byte("data: [DONE]")) {
			return true
		}
	}
	return false
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := hopHeaders[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
func randomID() string {
	return randomHex(16)
}
func randomHex(size int) string {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		return hex.EncodeToString(sum[:size])
	}
	return hex.EncodeToString(b)
}

var validTraceParent = regexp.MustCompile(`^[\da-f]{2}-[\da-f]{32}-[\da-f]{16}-[\da-f]{2}$`)

func openAIError(w http.ResponseWriter, message string, status int, errorType string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": errorType, "param": nil, "code": nil}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
