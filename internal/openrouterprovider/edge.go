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
	Catalog       *Catalog
	PublicModel   string
	UpstreamModel string
	APIKey        string
	UpstreamURL   string
	UpstreamKey   string
	MaxInFlight   int
	// StreamKeepAliveInterval controls SSE comment heartbeats while the model
	// is producing no bytes. Zero selects the production default.
	StreamKeepAliveInterval time.Duration
	Client                  *http.Client
	Logger                  *slog.Logger

	semaphore chan struct{}
}

func (e *Edge) Handler() (http.Handler, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	e.semaphore = make(chan struct{}, e.MaxInFlight)
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
	mux.HandleFunc("GET /models", e.auth(e.Catalog.ServeHTTP))
	mux.HandleFunc("GET /v1/models", e.auth(e.Catalog.ServeHTTP))
	mux.HandleFunc("GET /openrouter/v1/models", e.auth(e.Catalog.ServeHTTP))
	mux.HandleFunc("POST /v1/chat/completions", e.auth(e.completions))
	return mux, nil
}

func (e *Edge) validate() error {
	if e.Catalog == nil {
		return errors.New("OpenRouter provider catalog is required")
	}
	if err := e.Catalog.Validate(); err != nil {
		return err
	}
	if e.PublicModel == "" || e.UpstreamModel == "" || e.APIKey == "" || e.MaxInFlight < 1 {
		return errors.New("public model, upstream model, API key, and positive admission limit are required")
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

func (e *Edge) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expected := "Bearer " + e.APIKey
		actual := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeProviderError(w, http.StatusUnauthorized, "Invalid provider API key", "authentication_error")
			return
		}
		next(w, r)
	}
}

func (e *Edge) health(w http.ResponseWriter, _ *http.Request) {
	writeProviderJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	select {
	case e.semaphore <- struct{}{}:
		defer func() { <-e.semaphore }()
	default:
		w.Header().Set("Retry-After", "1")
		writeProviderError(w, http.StatusTooManyRequests, "Provider capacity is temporarily full", "rate_limit_error")
		return
	}
	payload["model"] = e.UpstreamModel
	body, err = json.Marshal(payload)
	if err != nil {
		writeProviderError(w, http.StatusBadRequest, "Request could not be encoded", "invalid_request_error")
		return
	}
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = randomRequestID()
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
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(response.StatusCode)
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		e.copyEventStream(w, r, response.Body, requestID, started)
		return
	}
	buffer := make([]byte, 32<<10)
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			if _, err = w.Write(buffer[:count]); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				e.Logger.Warn("provider upstream stream ended with error", "request_id", requestID, "duration_ms", time.Since(started).Milliseconds(), "error", readErr)
			}
			return
		}
	}
}

type streamRead struct {
	data []byte
	err  error
}

func (e *Edge) copyEventStream(w http.ResponseWriter, r *http.Request, body io.Reader, requestID string, started time.Time) {
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
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			heartbeat.Reset(interval)
		case read := <-reads:
			if len(read.data) > 0 {
				if _, err := w.Write(read.data); err != nil {
					return
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
				}
				return
			}
		}
	}
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
