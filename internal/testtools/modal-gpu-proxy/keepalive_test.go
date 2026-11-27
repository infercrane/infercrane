package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// laggingTunnel relays TCP to target and passes the target's close on only
// after lag, as a tunnel that relays a close a round trip late does. A request
// written in between reaches a closed server and gets no answer.
func laggingTunnel(t *testing.T, target string, lag time.Duration) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				client.Close()
				continue
			}
			go func() {
				_, _ = io.Copy(server, client)
				server.Close()
			}()
			go func() {
				_, _ = io.Copy(client, server)
				time.Sleep(lag)
				client.Close()
			}()
		}
	}()
	return listener.Addr().String()
}

// keepAliveServer answers every request and closes a connection that stayed
// idle for keepAlive, as vLLM's server does after 5 s.
func keepAliveServer(t *testing.T, keepAlive time.Duration, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.IdleTimeout = keepAlive
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func answer(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func leasedProxy(t *testing.T, upstream string, idle time.Duration) *Proxy {
	t.Helper()
	transport := newTransport(idle)
	t.Cleanup(transport.CloseIdleConnections)
	proxy := &Proxy{WorkerKey: workerKey, Client: &http.Client{Transport: transport}}
	if err := proxy.SetLease(Lease{ID: "lease-1", State: LeaseReady, URL: upstream, APIKey: "upstream-key"}); err != nil {
		t.Fatal(err)
	}
	return proxy
}

func postChat(proxy *Proxy, ctx context.Context) int {
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	request.Header.Set("Authorization", "Bearer "+workerKey)
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	return recorder.Code
}

// postAcrossAnIdleGap sends one chat request, waits longer than the server
// keeps the connection, and sends another while the server's close is still
// crossing the tunnel.
func postAcrossAnIdleGap(t *testing.T, idle time.Duration) (int, Stats) {
	t.Helper()
	const keepAlive, lag, gap = 150 * time.Millisecond, time.Second, 400 * time.Millisecond
	server := keepAliveServer(t, keepAlive, answer)
	proxy := leasedProxy(t, "http://"+laggingTunnel(t, server.Listener.Addr().String(), lag), idle)
	if code := postChat(proxy, context.Background()); code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", code)
	}
	time.Sleep(gap)
	return postChat(proxy, context.Background()), proxy.Stats()
}

func TestAPoolThatOutlivesTheServersKeepAliveAnswers502(t *testing.T) {
	// The pool keeps the connection the server already closed, writes the next
	// POST onto it, and Go does not send a POST again.
	code, stats := postAcrossAnIdleGap(t, 10*time.Second)
	if code != http.StatusBadGateway {
		t.Fatalf("second request = %d, want 502", code)
	}
	if stats.FailedRequests != 1 || len(stats.Failures) != 1 || stats.Failures[0].Path != "/v1/chat/completions" || stats.Failures[0].Error == "" {
		t.Fatalf("the failure must be counted with its path and error: %+v", stats)
	}
}

func TestAPoolThatDropsIdleConnectionsFirstAnswers(t *testing.T) {
	code, stats := postAcrossAnIdleGap(t, 75*time.Millisecond)
	if code != http.StatusOK || stats.FailedRequests != 0 {
		t.Fatalf("second request = %d, failed=%d; want 200 and none", code, stats.FailedRequests)
	}
}

func TestTheProxyDropsIdleConnectionsBeforeTheServerDoes(t *testing.T) {
	if upstreamIdleTimeout >= serverKeepAlive {
		t.Fatalf("idle timeout %s must be below the server's keep-alive %s", upstreamIdleTimeout, serverKeepAlive)
	}
	if got := newTransport(upstreamIdleTimeout).IdleConnTimeout; got != upstreamIdleTimeout {
		t.Fatalf("IdleConnTimeout = %s", got)
	}
}

func TestStatsCountTheRemoteSidesServerErrors(t *testing.T) {
	server := keepAliveServer(t, time.Minute, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	})
	proxy := leasedProxy(t, server.URL, upstreamIdleTimeout)
	if code := postChat(proxy, context.Background()); code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want the server's 503 passed on", code)
	}
	stats := proxy.Stats()
	if stats.ServerErrors["503"] != 1 || stats.FailedRequests != 0 {
		t.Fatalf("a server's 503 is the server's, not the network's: %+v", stats)
	}
}

func TestACallerThatLeavesIsNotAFailure(t *testing.T) {
	server := keepAliveServer(t, time.Minute, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	proxy := leasedProxy(t, server.URL, upstreamIdleTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	postChat(proxy, ctx)
	if stats := proxy.Stats(); stats.FailedRequests != 0 || stats.CancelledRequests != 1 {
		t.Fatalf("a caller that left is counted apart: %+v", stats)
	}
}

func TestACheckCutShortByItsCallerIsNotCachedForOthers(t *testing.T) {
	// The kubelet gives up on /health after 2 s; the control plane waits 5 s.
	var checks atomic.Int64
	server := keepAliveServer(t, time.Minute, func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	proxy := leasedProxy(t, server.URL, upstreamIdleTimeout)
	proxy.HealthCache = time.Minute
	impatient, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if proxy.health(impatient) {
		t.Fatal("a caller that stopped waiting cannot have seen the server answer")
	}
	if !proxy.health(context.Background()) {
		t.Fatal("the next caller must ask the server itself, not get the cut-short check")
	}
	if got := checks.Load(); got != 2 {
		t.Fatalf("server checks = %d, want 2", got)
	}
	if !proxy.health(context.Background()) || checks.Load() != 2 {
		t.Fatalf("a completed check is cached: checks = %d", checks.Load())
	}
}
