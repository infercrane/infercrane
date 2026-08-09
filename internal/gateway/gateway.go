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
	RecordRequest(context.Context, string, string, time.Time, int, float64, string) error
}
type Gateway struct {
	Routes        *routes.Directory
	APIKey        string
	Client        *http.Client
	Recorder      Recorder
	Logger        *slog.Logger
	Ready         func(context.Context) error
	Telemetry     *Telemetry
	Control       http.Handler
	Authenticator interface {
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
		g.record(r.Context(), requestID, route.DeploymentID, started, status, errorType)
		openAIError(w, message, status, "server_error")
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("traceparent", traceParent)
	w.WriteHeader(resp.StatusCode)
	copyErr := copyResponse(w, resp)
	errorType := ""
	if copyErr != nil {
		if r.Context().Err() != nil {
			errorType = "client_cancelled"
		} else {
			errorType = "upstream_disconnect"
		}
	}
	g.record(context.WithoutCancel(r.Context()), requestID, route.DeploymentID, started, resp.StatusCode, errorType)
	if g.Logger != nil {
		g.Logger.Info("inference request", "request_id", requestID, "traceparent", traceParent, "tenant_id", principal.TenantID, "deployment_id", route.DeploymentID, "status", resp.StatusCode, "duration_ms", float64(time.Since(started).Microseconds())/1000)
	}
}
func (g *Gateway) record(ctx context.Context, id, deploymentID string, started time.Time, status int, errorType string) {
	if g.Recorder == nil {
		return
	}
	if err := g.Recorder.RecordRequest(ctx, id, deploymentID, started, status, float64(time.Since(started).Microseconds())/1000, errorType); err != nil && g.Logger != nil {
		g.Logger.Error("record request", "error", err, "request_id", id)
	}
}

var hopHeaders = map[string]struct{}{"Connection": {}, "Keep-Alive": {}, "Proxy-Authenticate": {}, "Proxy-Authorization": {}, "Te": {}, "Trailer": {}, "Transfer-Encoding": {}, "Upgrade": {}, "Content-Length": {}, "Host": {}}
var copyBuffers = sync.Pool{New: func() any { buffer := make([]byte, 32<<10); return &buffer }}

func copyResponse(w http.ResponseWriter, resp *http.Response) error {
	buffer := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(buffer)
	streaming := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	for {
		n, err := resp.Body.Read(*buffer)
		if n > 0 {
			if _, writeErr := w.Write((*buffer)[:n]); writeErr != nil {
				return writeErr
			}
			if streaming {
				if flushErr := http.NewResponseController(w).Flush(); flushErr != nil {
					return flushErr
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
