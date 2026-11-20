package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
)

const (
	// maxParsedRequestBytes bounds how much of a request the gateway reads to
	// learn its model, streaming flag, and system-prompt hash. Larger bodies
	// pass through untouched and are recorded without those fields.
	maxParsedRequestBytes = 16 << 20
	// maxObservedResponseBytes bounds the response bytes kept to read token
	// usage. The bytes stay in memory on this machine and are discarded.
	maxObservedResponseBytes = 2 << 20
	maxBufferedShapes        = 10000
)

// shapeBuffer holds recorded shapes until a sync delivers them. When it is
// full the oldest shapes are dropped and counted, never blocking traffic.
type shapeBuffer struct {
	mu      sync.Mutex
	items   []agentprotocol.TrafficShape
	dropped int
}

func (b *shapeBuffer) add(shape agentprotocol.TrafficShape) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) >= maxBufferedShapes {
		b.items = b.items[1:]
		b.dropped++
	}
	b.items = append(b.items, shape)
}

// take removes up to limit shapes for one sync.
func (b *shapeBuffer) take(limit int) []agentprotocol.TrafficShape {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) < limit {
		limit = len(b.items)
	}
	out := append([]agentprotocol.TrafficShape(nil), b.items[:limit]...)
	b.items = b.items[limit:]
	return out
}

// restore puts undelivered shapes back in front.
func (b *shapeBuffer) restore(shapes []agentprotocol.TrafficShape) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = append(append([]agentprotocol.TrafficShape(nil), shapes...), b.items...)
	if overflow := len(b.items) - maxBufferedShapes; overflow > 0 {
		b.items = b.items[overflow:]
		b.dropped += overflow
	}
}

func (b *shapeBuffer) stats() (pending, dropped int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items), b.dropped
}

// Gateway is the agent's optional local, OpenAI-compatible pass-through. It
// forwards every request unchanged (including the application's own
// Authorization header) to one local server and records a content-free shape
// of each inference request. Prompts, completions, and headers are never
// recorded; the system-prompt and session hashes use a key derived from the
// agent key, which the control plane never sees.
type Gateway struct {
	Upstream  string
	ServerKey string
	HashKey   []byte
	Shapes    *shapeBuffer
	Now       func() time.Time
	Transport http.RoundTripper
}

func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Handler returns the pass-through handler.
func (g *Gateway) Handler() (http.Handler, error) {
	target, err := url.Parse(g.Upstream)
	if err != nil || target.Host == "" {
		return nil, errors.New("gateway upstream must be an absolute URL")
	}
	transport := g.Transport
	if transport == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.Proxy = nil
		transport = base
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = target.Host
		},
		Transport:     transport,
		FlushInterval: -1,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The transport can still be reading the request body after the
		// server's response has started: it sends the body while it waits
		// for the response, and for a body of known length it reads once
		// more past the end. By default an HTTP/1 server closes the request
		// body when the response headers go out, which fails that read and
		// makes the transport close the connection under the response.
		// Full duplex keeps the body readable until this handler returns.
		_ = http.NewResponseController(w).EnableFullDuplex()
		operation := operationFor(r)
		if operation == "" {
			proxy.ServeHTTP(w, r)
			return
		}
		started := g.now()
		info := requestInfo{}
		if r.Body != nil && r.ContentLength <= maxParsedRequestBytes {
			buffered, readErr := io.ReadAll(io.LimitReader(r.Body, maxParsedRequestBytes+1))
			if readErr == nil && len(buffered) <= maxParsedRequestBytes {
				info = g.inspectRequest(buffered, r.Header)
				// The whole body is in memory: forward that copy, so the
				// request sent upstream no longer depends on the server's
				// body at all.
				r.Body = io.NopCloser(bytes.NewReader(buffered))
			} else {
				r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buffered), r.Body))
			}
		}
		observer := &responseObserver{ResponseWriter: w, gateway: g}
		local := *proxy
		local.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, _ error) {
			observer.errorType = "upstream_unreachable"
			rw.WriteHeader(http.StatusBadGateway)
			_, _ = rw.Write([]byte(`{"error":{"message":"the local model server did not answer","type":"server_error"}}`))
		}
		// The proxy aborts with http.ErrAbortHandler when copying a started
		// response fails; the request is still recorded. Only a client that
		// went away is a cancellation: otherwise the server's response broke
		// off, and its token counts stay unknown.
		defer func() {
			recovered := recover()
			if recovered != nil && observer.errorType == "" {
				if observer.clientGone || r.Context().Err() != nil {
					observer.errorType = "client_cancelled"
				} else {
					observer.errorType = "upstream_disconnect"
				}
			}
			g.record(operation, started, info, observer)
			if recovered != nil {
				panic(recovered)
			}
		}()
		local.ServeHTTP(observer, r)
	}), nil
}

type requestInfo struct {
	model, prefixHash, sessionHash string
	streaming                      bool
}

func operationFor(r *http.Request) string {
	if r.Method != http.MethodPost {
		return ""
	}
	switch strings.TrimRight(r.URL.Path, "/") {
	case "/v1/chat/completions":
		return "chat"
	case "/v1/completions":
		return "text_completion"
	case "/v1/embeddings":
		return "embeddings"
	case "/v1/responses":
		return "responses"
	}
	return ""
}

func (g *Gateway) digest(value []byte) string {
	if len(value) == 0 || len(g.HashKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, g.HashKey)
	_, _ = mac.Write(value)
	return hex.EncodeToString(mac.Sum(nil))
}

// inspectRequest reads the model name and streaming flag and hashes the
// leading system messages, using the same rule as the InferCrane gateway. No
// content is kept.
func (g *Gateway) inspectRequest(body []byte, headers http.Header) requestInfo {
	info := requestInfo{sessionHash: g.digest([]byte(headers.Get("X-InferCrane-Session-ID")))}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return info
	}
	if model, ok := payload["model"].(string); ok {
		info.model = agentprotocol.CleanText(model, agentprotocol.MaxText)
	}
	info.streaming, _ = payload["stream"].(bool)
	if messages, ok := payload["messages"].([]any); ok {
		prefix := make([]any, 0)
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok || message["role"] != "system" {
				break
			}
			prefix = append(prefix, message)
		}
		if len(prefix) > 0 {
			encoded, _ := json.Marshal(prefix)
			info.prefixHash = g.digest(encoded)
		}
	}
	return info
}

// responseObserver passes the response through while noting the status, the
// time of the first body byte, and a bounded copy used only to read token
// usage on this machine.
type responseObserver struct {
	http.ResponseWriter
	gateway     *Gateway
	status      int
	firstByteAt time.Time
	body        []byte
	errorType   string
	streaming   bool
	// clientGone is set when writing to the client failed.
	clientGone bool
}

func (o *responseObserver) WriteHeader(status int) {
	if o.status == 0 {
		o.status = status
		o.streaming = strings.HasPrefix(strings.ToLower(o.Header().Get("Content-Type")), "text/event-stream")
	}
	o.ResponseWriter.WriteHeader(status)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.WriteHeader(http.StatusOK)
	}
	if len(p) > 0 && o.firstByteAt.IsZero() {
		o.firstByteAt = o.gateway.now()
	}
	if remaining := maxObservedResponseBytes - len(o.body); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		o.body = append(o.body, p[:remaining]...)
	}
	n, err := o.ResponseWriter.Write(p)
	if err != nil {
		o.clientGone = true
	}
	return n, err
}

func (o *responseObserver) Flush() {
	if flusher, ok := o.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (o *responseObserver) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func (g *Gateway) record(operation string, started time.Time, info requestInfo, observer *responseObserver) {
	if g.Shapes == nil {
		return
	}
	finished := g.now()
	status := observer.status
	switch {
	case status == 0 && observer.errorType == "client_cancelled":
		status = 499
	case status == 0:
		status = http.StatusBadGateway
	}
	streaming := info.streaming || observer.streaming
	shape := agentprotocol.TrafficShape{
		ID: newShapeID(), ServerKey: g.ServerKey, StartedAt: started.UTC(), Operation: operation,
		RequestModel: info.model, StatusCode: status, LatencyMS: milliseconds(finished.Sub(started)),
		Streaming: streaming, SharedPrefixHash: info.prefixHash, SessionIDHash: info.sessionHash, ErrorType: observer.errorType,
	}
	if !observer.firstByteAt.IsZero() {
		ttft := milliseconds(observer.firstByteAt.Sub(started))
		shape.TTFTMS = &ttft
	}
	if shape.ErrorType == "" && status >= 400 {
		shape.ErrorType = "http_" + map[bool]string{true: "server_error", false: "client_error"}[status >= 500]
	}
	shape.InputTokens, shape.OutputTokens = boundedTokens(usage(observer.body))
	observer.body = nil
	if shape.Validate() != nil {
		// Never buffer a shape the control plane would refuse.
		return
	}
	g.Shapes.add(shape)
}

// boundedTokens drops token counts outside what the protocol accepts; an
// implausible count is unknown, not evidence.
func boundedTokens(input, output *int) (*int, *int) {
	valid := func(value *int) *int {
		if value == nil || *value < 0 || *value > 10_000_000 {
			return nil
		}
		return value
	}
	return valid(input), valid(output)
}

func milliseconds(value time.Duration) float64 {
	if value < 0 {
		return 0
	}
	return float64(value.Microseconds()) / 1000
}

func newShapeID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

type usageFields struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	InputTokens      *int `json:"input_tokens"`
	OutputTokens     *int `json:"output_tokens"`
}

// usage reads token counts from a JSON response or the last SSE chunk that
// reports usage. Responses API streams nest usage under "response".
func usage(body []byte) (*int, *int) {
	pick := func(fields usageFields) (*int, *int) {
		input, output := fields.PromptTokens, fields.CompletionTokens
		if input == nil {
			input = fields.InputTokens
		}
		if output == nil {
			output = fields.OutputTokens
		}
		return input, output
	}
	var whole struct {
		Usage *usageFields `json:"usage"`
	}
	if json.Unmarshal(body, &whole) == nil && whole.Usage != nil {
		return pick(*whole.Usage)
	}
	var input, output *int
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		var chunk struct {
			Usage    *usageFields `json:"usage"`
			Response *struct {
				Usage *usageFields `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(value, &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			input, output = pick(*chunk.Usage)
		} else if chunk.Response != nil && chunk.Response.Usage != nil {
			input, output = pick(*chunk.Response.Usage)
		}
	}
	return input, output
}

// ListenGateway opens the gateway's local listener. Loopback is the default;
// any other address is the user's explicit choice.
func ListenGateway(ctx context.Context, address string) (net.Listener, error) {
	var config net.ListenConfig
	return config.Listen(ctx, "tcp", address)
}
