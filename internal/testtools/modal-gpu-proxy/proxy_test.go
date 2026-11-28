package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const workerKey = "worker-key"

// fakeBroker records calls and serves one lease.
type fakeBroker struct {
	mu         sync.Mutex
	state      string
	url        string
	created    []map[string]any
	heartbeats atomic.Int64
	released   atomic.Int64
	refuse     atomic.Int64 // refuse this many creates first
}

func (b *fakeBroker) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+workerKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	lease := func() map[string]any {
		b.mu.Lock()
		defer b.mu.Unlock()
		return map[string]any{"lease_id": "lease-1", "state": b.state, "url": b.url}
	}
	mux.HandleFunc("POST /v1/leases", auth(func(w http.ResponseWriter, r *http.Request) {
		if b.refuse.Add(-1) >= 0 {
			http.Error(w, "gpu limit reached", http.StatusTooManyRequests)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode create: %v", err)
		}
		b.mu.Lock()
		b.created = append(b.created, body)
		b.mu.Unlock()
		out := lease()
		out["api_key"] = "upstream-key"
		_ = json.NewEncoder(w).Encode(out)
	}))
	mux.HandleFunc("GET /v1/leases/{id}", auth(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(lease()) }))
	mux.HandleFunc("POST /v1/leases/{id}/heartbeat", auth(func(w http.ResponseWriter, _ *http.Request) {
		b.heartbeats.Add(1)
		_ = json.NewEncoder(w).Encode(lease())
	}))
	mux.HandleFunc("DELETE /v1/leases/{id}", auth(func(w http.ResponseWriter, _ *http.Request) {
		b.released.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

func (b *fakeBroker) set(state, url string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state, b.url = state, url
}

// fakeServer stands in for vLLM behind the tunnel.
func fakeServer(t *testing.T, healthy *atomic.Bool, seenAuth *atomic.Value, release chan struct{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "vllm:num_requests_waiting 3")
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release // the second chunk only comes after the test saw the first
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestProxyHealthFollowsTheRemoteServer(t *testing.T) {
	var healthy atomic.Bool
	var auth atomic.Value
	upstream := fakeServer(t, &healthy, &auth, make(chan struct{}))
	proxy := &Proxy{WorkerKey: workerKey, HealthCache: time.Nanosecond}
	pod := httptest.NewServer(proxy)
	defer pod.Close()

	status := func(path string) int {
		response, err := http.Get(pod.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if got := status("/health"); got != http.StatusServiceUnavailable {
		t.Fatalf("health without a lease = %d, want 503", got)
	}
	if got := status("/metrics"); got != http.StatusServiceUnavailable {
		t.Fatalf("requests without a lease = %d, want 503", got)
	}
	if err := proxy.SetLease(Lease{ID: "lease-1", State: LeaseReady, URL: upstream.URL, APIKey: "upstream-key"}); err != nil {
		t.Fatal(err)
	}
	if got := status("/health"); got != http.StatusServiceUnavailable {
		t.Fatalf("health while the remote server is unhealthy = %d, want 503", got)
	}
	healthy.Store(true)
	if got := status("/health"); got != http.StatusOK {
		t.Fatalf("health with a healthy remote server = %d, want 200", got)
	}
	if got := status("/metrics"); got != http.StatusOK {
		t.Fatalf("metrics = %d, want 200 without a key, as vLLM serves them", got)
	}
	if stats := proxy.Stats(); stats.Samples == 0 || stats.RTTp95MS < stats.RTTMinMS {
		t.Fatalf("stats = %+v, want health round trips recorded", stats)
	}
	proxy.End(ErrLeaseEnded)
	if got := status("/health"); got != http.StatusServiceUnavailable {
		t.Fatalf("health after the lease ended = %d, want 503", got)
	}
}

func TestProxyRefusesNotReadyLeases(t *testing.T) {
	proxy := &Proxy{WorkerKey: workerKey}
	for _, lease := range []Lease{
		{ID: "a", State: LeaseStarting, URL: "https://x.modal.host", APIKey: "k"},
		{ID: "b", State: LeaseReady, URL: "", APIKey: "k"},
		{ID: "c", State: LeaseReady, URL: "https://x.modal.host"},
		{ID: "d", State: LeaseReady, URL: "ftp://x", APIKey: "k"},
	} {
		if err := proxy.SetLease(lease); err == nil {
			t.Fatalf("SetLease(%+v) succeeded, want refusal", lease)
		}
	}
}

func TestProxySwapsTheKeyAndStreamsWithoutBuffering(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	var auth atomic.Value
	release := make(chan struct{})
	upstream := fakeServer(t, &healthy, &auth, release)
	proxy := &Proxy{WorkerKey: workerKey}
	if err := proxy.SetLease(Lease{ID: "lease-1", State: LeaseReady, URL: upstream.URL, APIKey: "upstream-key"}); err != nil {
		t.Fatal(err)
	}
	pod := httptest.NewServer(proxy)
	defer pod.Close()

	unauthorized, err := http.Post(pod.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("request without the worker key = %d, want 401", unauthorized.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodPost, pod.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	request.Header.Set("Authorization", "Bearer "+workerKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(first, "first") {
		t.Fatalf("first streamed line = %q, %v; want it before the server finishes", first, err)
	}
	close(release)
	rest, err := reader.ReadString(0)
	if !strings.Contains(rest, "[DONE]") {
		t.Fatalf("stream tail = %q, %v; want [DONE]", rest, err)
	}
	if got := auth.Load(); got != "Bearer upstream-key" {
		t.Fatalf("upstream saw Authorization %q, want the lease key only", got)
	}
}

// A server can answer before it has read the whole request: a streaming
// server, or one that rejects the request early. The proxy must keep
// forwarding the request body after the response has started. Without full
// duplex, Go's HTTP/1 server consumes and closes the unread request body
// before the response begins. A request still arriving then holds the
// response back, and a forwarding read already in flight fails and cuts the
// response mid-stream.
func TestProxyStreamsWhileTheRequestIsStillArriving(t *testing.T) {
	received := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// This server answers before it reads the request, so it needs full
		// duplex itself.
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Errorf("enable full duplex on the remote server: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			received <- "read error: " + err.Error()
			return
		}
		received <- string(body)
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	proxy := &Proxy{WorkerKey: workerKey}
	if err := proxy.SetLease(Lease{ID: "lease-1", State: LeaseReady, URL: upstream.URL, APIKey: "upstream-key"}); err != nil {
		t.Fatal(err)
	}
	pod := httptest.NewServer(proxy)
	defer pod.Close()

	// The client sends the second half of its body once the first event has
	// arrived. The timer only keeps a regression from deadlocking: without
	// full duplex the first event cannot arrive until the request is whole.
	body, send := io.Pipe()
	firstSeen, sentAll := make(chan struct{}), make(chan struct{})
	go func() {
		defer send.Close()
		_, _ = send.Write([]byte(`{"stream":`))
		select {
		case <-firstSeen:
		case <-time.After(5 * time.Second):
		}
		_, _ = send.Write([]byte(`true}`))
		close(sentAll)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, pod.URL+"/v1/chat/completions", body)
	request.Header.Set("Authorization", "Bearer "+workerKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	select {
	case <-sentAll:
		t.Fatalf("first event arrived only after the whole request was sent (%q, %v); the proxy must stream while the request is still arriving", first, err)
	default:
	}
	if err != nil || !strings.Contains(first, "first") {
		t.Fatalf("first streamed line = %q, %v", first, err)
	}
	close(firstSeen)
	if rest, err := reader.ReadString(0); !strings.Contains(rest, "[DONE]") {
		t.Fatalf("stream tail = %q, %v; want [DONE] after the request finished", rest, err)
	}
	select {
	case got := <-received:
		if got != `{"stream":true}` {
			t.Fatalf("upstream received %q, want the whole request body", got)
		}
	case <-ctx.Done():
		t.Fatal("upstream never finished reading the request body")
	}
}

func TestAcquireWaitsForGrantAndReadiness(t *testing.T) {
	var healthy atomic.Bool
	var auth atomic.Value
	upstream := fakeServer(t, &healthy, &auth, make(chan struct{}))
	fake := &fakeBroker{state: LeaseStarting}
	fake.refuse.Store(2)
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()
	broker := Broker{BaseURL: server.URL, Token: workerKey}
	go func() {
		time.Sleep(50 * time.Millisecond)
		fake.set(LeaseReady, upstream.URL)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := acquire(ctx, broker, "pod-a", "Qwen/Qwen3-8B", []string{"--served-model-name", "Qwen/Qwen3-8B", "--kv-cache-dtype", "fp8"}, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if lease.State != LeaseReady || lease.URL != upstream.URL || lease.APIKey != "upstream-key" {
		t.Fatalf("lease = %+v, want ready with the granted key", lease)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.created) != 1 {
		t.Fatalf("creates recorded = %d, want 1 after two refusals", len(fake.created))
	}
	got := fmt.Sprint(fake.created[0]["args"])
	if got != "[--served-model-name Qwen/Qwen3-8B --kv-cache-dtype fp8]" || fake.created[0]["model"] != "Qwen/Qwen3-8B" || fake.created[0]["pod"] != "pod-a" {
		t.Fatalf("create body = %v, want the exact server arguments", fake.created[0])
	}
}

func TestAcquireStopsOnAFailedServer(t *testing.T) {
	fake := &fakeBroker{state: LeaseFailed}
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := acquire(ctx, Broker{BaseURL: server.URL, Token: workerKey}, "pod-a", "m", nil, time.Millisecond)
	if !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("acquire error = %v, want ErrLeaseEnded", err)
	}
}

func TestAcquireReleasesWhenThePodStopsWhileStarting(t *testing.T) {
	fake := &fakeBroker{state: LeaseStarting}
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if _, err := acquire(ctx, Broker{BaseURL: server.URL, Token: workerKey}, "pod-a", "m", nil, time.Millisecond); err == nil {
		t.Fatal("acquire succeeded after cancellation")
	}
	if fake.released.Load() != 1 {
		t.Fatalf("releases = %d, want the starting lease released", fake.released.Load())
	}
	if fake.heartbeats.Load() == 0 {
		t.Fatal("a starting lease was never renewed")
	}
}

func TestKeepAliveEndsWhenTheBrokerEndsTheLease(t *testing.T) {
	fake := &fakeBroker{state: LeaseReady, url: "https://x.modal.host"}
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()
	go func() {
		time.Sleep(20 * time.Millisecond)
		fake.set(LeaseStopped, "")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := keepAlive(ctx, Broker{BaseURL: server.URL, Token: workerKey}, "lease-1", time.Millisecond)
	if !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("keepAlive = %v, want ErrLeaseEnded once the broker stopped the lease", err)
	}
}

func TestKeepAliveGivesUpOnAnUnreachableBroker(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close() // nothing listens any more
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := keepAlive(ctx, Broker{BaseURL: server.URL, Token: workerKey, Client: &http.Client{Timeout: 50 * time.Millisecond}}, "lease-1", time.Millisecond)
	if !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("keepAlive = %v, want ErrLeaseEnded after the broker stayed unreachable", err)
	}
}

func TestParseKeepsServerArgumentsVerbatim(t *testing.T) {
	parsed, err := parse([]string{"--broker", "http://host:1", "--port", "8080", "--model", "Qwen/Qwen3-8B", "--", "--served-model-name", "Qwen/Qwen3-8B", "--quantization", "fp8"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(parsed.serverArgs) != "[--served-model-name Qwen/Qwen3-8B --quantization fp8]" {
		t.Fatalf("server args = %v", parsed.serverArgs)
	}
	if _, err := parse([]string{"--port", "8080"}); err == nil {
		t.Fatal("parse without broker and model succeeded")
	}
}
