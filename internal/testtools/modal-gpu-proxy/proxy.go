package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Lease is the broker's view of one remote GPU server.
type Lease struct {
	ID     string `json:"lease_id"`
	State  string `json:"state"`
	URL    string `json:"url,omitempty"`
	APIKey string `json:"api_key,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Lease states reported by the broker.
const (
	LeaseStarting = "starting"
	LeaseReady    = "ready"
	LeaseStopped  = "stopped"
	LeaseFailed   = "failed"
)

// ErrLeaseEnded means the remote server stopped or failed; the pod must not
// keep reporting healthy.
var ErrLeaseEnded = errors.New("remote GPU lease ended")

// Broker is the laptop process that holds the Modal login and starts one
// remote GPU server per lease. The proxy never sees a Modal credential.
type Broker struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func (b Broker) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.BaseURL, "/")+path, reader)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+b.Token)
	request.Header.Set("Content-Type", "application/json")
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response.StatusCode, err
	}
	if response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("broker %s %s returned %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return response.StatusCode, fmt.Errorf("decode broker response: %w", err)
		}
	}
	return response.StatusCode, nil
}

// Create asks for a new remote server running exactly args.
func (b Broker) Create(ctx context.Context, pod, model string, args []string) (Lease, error) {
	var lease Lease
	_, err := b.do(ctx, http.MethodPost, "/v1/leases", map[string]any{"pod": pod, "model": model, "args": args}, &lease)
	return lease, err
}

func (b Broker) Get(ctx context.Context, id string) (Lease, error) {
	var lease Lease
	_, err := b.do(ctx, http.MethodGet, "/v1/leases/"+url.PathEscape(id), nil, &lease)
	return lease, err
}

func (b Broker) Heartbeat(ctx context.Context, id string) (Lease, error) {
	var lease Lease
	_, err := b.do(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(id)+"/heartbeat", nil, &lease)
	return lease, err
}

func (b Broker) Release(ctx context.Context, id string) error {
	_, err := b.do(ctx, http.MethodDelete, "/v1/leases/"+url.PathEscape(id), nil, nil)
	return err
}

// Proxy serves the runtime contract InferCrane expects from a model server
// pod and forwards every request to the remote GPU server of its lease.
// Health is the remote server's health: the pod is never ready while the
// remote server is not.
type Proxy struct {
	// WorkerKey is the key InferCrane sends to the model server. Requests to
	// /v1 must carry it; it is replaced by the lease's own upstream key.
	WorkerKey string
	Client    *http.Client
	Now       func() time.Time
	// HealthCache bounds how often /health reaches the remote server.
	HealthCache time.Duration

	mu        sync.RWMutex
	lease     Lease
	upstream  *url.URL
	ended     error
	checkedAt time.Time
	healthy   bool
	rtts      []time.Duration
	// What went wrong between this pod and the remote server on /v1, kept so
	// a run can say which hop a failed request failed on.
	failed       int
	cancelled    int
	serverErrors map[string]int
	failures     []Failure
}

// Failure is one /v1 request this pod answered 502 because the remote
// server could not be reached or closed the connection without an answer.
type Failure struct {
	At    time.Time `json:"at"`
	Path  string    `json:"path"`
	Error string    `json:"error"`
}

const maxFailures = 64

func (p *Proxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// SetLease installs a ready lease. A lease that is not ready is refused.
func (p *Proxy) SetLease(lease Lease) error {
	if lease.State != LeaseReady || lease.URL == "" || lease.APIKey == "" {
		return fmt.Errorf("lease %s is not ready", lease.ID)
	}
	target, err := url.Parse(lease.URL)
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" {
		return fmt.Errorf("lease %s has an invalid server URL", lease.ID)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lease, p.upstream, p.ended, p.checkedAt, p.healthy = lease, target, nil, time.Time{}, false
	return nil
}

// End marks the lease as gone; health turns 503 and requests are refused.
func (p *Proxy) End(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended == nil {
		p.ended = err
	}
}

func (p *Proxy) current() (Lease, *url.URL, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lease, p.upstream, p.ended
}

// Stats is the proxy's own evidence: upstream health round trips measured
// from this pod, which let a report state how much of the measured latency is
// the network path to the remote GPU, and the /v1 requests that failed
// between this pod and the remote server. A request the gateway records as
// failed is the network's when this pod counted it in FailedRequests, the
// remote side's when it is in ServerErrors, and otherwise failed before it
// reached this pod.
type Stats struct {
	LeaseID  string  `json:"lease_id,omitempty"`
	Samples  int     `json:"health_rtt_samples"`
	RTTp50MS float64 `json:"health_rtt_p50_ms"`
	RTTp95MS float64 `json:"health_rtt_p95_ms"`
	RTTMinMS float64 `json:"health_rtt_min_ms"`
	Ended    string  `json:"ended,omitempty"`
	Upstream string  `json:"upstream_host,omitempty"`
	// FailedRequests are answered 502 here: the remote server was not reached
	// or closed the connection before answering.
	FailedRequests int `json:"failed_requests"`
	// CancelledRequests ended because the caller went away first.
	CancelledRequests int `json:"cancelled_requests"`
	// ServerErrors counts the 5xx answers the remote side sent, by status.
	ServerErrors map[string]int `json:"server_errors,omitempty"`
	// Failures are the last failed requests, oldest first.
	Failures []Failure `json:"failures,omitempty"`
}

func (p *Proxy) Stats() Stats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	stats := Stats{LeaseID: p.lease.ID, Samples: len(p.rtts), FailedRequests: p.failed, CancelledRequests: p.cancelled, Failures: slices.Clone(p.failures)}
	if len(p.serverErrors) > 0 {
		stats.ServerErrors = maps.Clone(p.serverErrors)
	}
	if p.upstream != nil {
		stats.Upstream = p.upstream.Host
	}
	if p.ended != nil {
		stats.Ended = p.ended.Error()
	}
	if len(p.rtts) == 0 {
		return stats
	}
	ordered := slices.Clone(p.rtts)
	slices.Sort(ordered)
	pick := func(q float64) float64 {
		index := int(q*float64(len(ordered))+0.999999) - 1
		index = max(0, min(len(ordered)-1, index))
		return float64(ordered[index].Microseconds()) / 1000
	}
	stats.RTTp50MS, stats.RTTp95MS, stats.RTTMinMS = pick(0.50), pick(0.95), float64(ordered[0].Microseconds())/1000
	return stats
}

const maxRTTSamples = 4096

func (p *Proxy) health(ctx context.Context) bool {
	lease, upstream, ended := p.current()
	if ended != nil || upstream == nil {
		return false
	}
	cache := p.HealthCache
	if cache <= 0 {
		cache = 2 * time.Second
	}
	p.mu.RLock()
	fresh, healthy := p.now().Sub(p.checkedAt) < cache, p.healthy
	p.mu.RUnlock()
	if fresh {
		return healthy
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	checkCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(checkCtx, http.MethodGet, upstream.JoinPath("/health").String(), nil)
	if err != nil {
		return false
	}
	started := time.Now()
	response, err := client.Do(request)
	rtt := time.Since(started)
	ok := err == nil && response.StatusCode == http.StatusOK
	if response != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
		response.Body.Close()
	}
	if !ok && ctx.Err() != nil {
		// The caller stopped waiting first: the kubelet's readiness probe allows
		// 2 s, the control plane's 5 s. A check cut short says nothing about the
		// server, and cached it would fail the control plane's probe at once.
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lease.ID != lease.ID {
		return false
	}
	p.checkedAt, p.healthy = p.now(), ok
	if ok && len(p.rtts) < maxRTTSamples {
		p.rtts = append(p.rtts, rtt)
	}
	return ok
}

func (p *Proxy) authorized(request *http.Request) bool {
	if p.WorkerKey == "" {
		return false
	}
	presented, found := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	return found && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(presented)), []byte(p.WorkerKey)) == 1
}

// ServeHTTP implements the pod's runtime contract.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/health":
		if p.health(request.Context()) {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "remote GPU server is not ready", http.StatusServiceUnavailable)
		return
	case "/proxy/stats":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.Stats())
		return
	}
	lease, upstream, ended := p.current()
	if ended != nil || upstream == nil {
		http.Error(w, "remote GPU server is not ready", http.StatusServiceUnavailable)
		return
	}
	// vLLM protects only /v1 with its key; the proxy keeps the same rule.
	if strings.HasPrefix(request.URL.Path, "/v1/") && !p.authorized(request) {
		http.Error(w, "invalid API key", http.StatusUnauthorized)
		return
	}
	reverse := &httputil.ReverseProxy{
		Rewrite: func(out *httputil.ProxyRequest) {
			out.SetURL(upstream)
			out.Out.Host = upstream.Host
			out.Out.Header.Del("Authorization")
			if strings.HasPrefix(request.URL.Path, "/v1/") {
				out.Out.Header.Set("Authorization", "Bearer "+lease.APIKey)
			}
		},
		// Streamed tokens must reach the load generator as they arrive, or
		// time to first token would measure the proxy's buffering.
		FlushInterval: -1,
		Transport:     p.transport(),
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode >= http.StatusInternalServerError && strings.HasPrefix(request.URL.Path, "/v1/") {
				p.noteServerError(response.StatusCode)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, failed *http.Request, err error) {
			if failed.Context().Err() != nil {
				p.noteCancelled(failed.URL.Path)
			} else {
				slog.Warn("remote GPU request failed", "lease_id", lease.ID, "path", failed.URL.Path, "error", err)
				p.noteFailure(failed.URL.Path, err)
			}
			http.Error(w, "remote GPU server unreachable", http.StatusBadGateway)
		},
	}
	// A server may answer before it has read the whole request: it streams,
	// or it rejects the request early. Without full duplex, Go's HTTP/1
	// server consumes and closes the unread request body before the response
	// begins. A request still arriving then holds the response back, and a
	// forwarding read already in flight fails, closes the remote connection
	// and cuts the response mid-stream. HTTP/2 is always full duplex, so the
	// error it returns is expected.
	_ = http.NewResponseController(w).EnableFullDuplex()
	reverse.ServeHTTP(w, request)
}

func (p *Proxy) noteFailure(path string, err error) {
	if !strings.HasPrefix(path, "/v1/") {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed++
	p.failures = append(p.failures, Failure{At: p.now().UTC(), Path: path, Error: err.Error()})
	if len(p.failures) > maxFailures {
		p.failures = slices.Delete(p.failures, 0, len(p.failures)-maxFailures)
	}
}

func (p *Proxy) noteCancelled(path string) {
	if !strings.HasPrefix(path, "/v1/") {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelled++
}

func (p *Proxy) noteServerError(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.serverErrors == nil {
		p.serverErrors = map[string]int{}
	}
	p.serverErrors[fmt.Sprint(status)]++
}

func (p *Proxy) transport() http.RoundTripper {
	if p.Client != nil && p.Client.Transport != nil {
		return p.Client.Transport
	}
	return http.DefaultTransport
}
