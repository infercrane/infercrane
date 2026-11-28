package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/infercrane/infercrane/internal/agentprotocol"
)

const probeToken = "probe-secret-token"

// switchableServer answers GET /v1/models with whatever its mode says.
func switchableServer(t *testing.T, mode *atomic.Value, requests *atomic.Int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		switch mode.Load().(string) {
		case "ok":
			_, _ = w.Write([]byte(`{"data":[{"id":"Qwen/Qwen3-8B"}]}`))
		case "not-json":
			_, _ = w.Write([]byte(`<html>sign in</html>`))
		case "401":
			w.WriteHeader(http.StatusUnauthorized)
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) record(event Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

// take returns the probe events since the last call.
func (l *eventLog) take() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Event{}
	for _, event := range l.events {
		if strings.HasPrefix(event.Kind, "server_probe_") {
			out = append(out, event)
		}
	}
	l.events = nil
	return out
}

// A failing health probe is logged once per change, with the server key, the
// probe path, and the reason, and never with the server's token.
func TestProbeFailuresAreLoggedOncePerChangeWithoutTheToken(t *testing.T) {
	ctx := context.Background()
	var mode atomic.Value
	var requests atomic.Int64
	server := switchableServer(t, &mode, &requests)
	key := ServerKey(server.URL)
	log := &eventLog{}
	a := New(Config{Servers: []ServerConfig{{URL: server.URL, AuthHeader: "Bearer " + probeToken}}, Events: log.record})
	pass := func(next string) []Event {
		t.Helper()
		mode.Store(next)
		a.Probe(ctx)
		events := log.take()
		for _, event := range events {
			if strings.Contains(event.Message, probeToken) || strings.Contains(event.Message, "Bearer") {
				t.Fatalf("a probe log line carries the server's credential: %q", event.Message)
			}
		}
		return events
	}
	expect := func(events []Event, kind string, parts ...string) {
		t.Helper()
		if len(events) != 1 || events[0].Kind != kind || events[0].Server == nil || events[0].Server.Key != key {
			t.Fatalf("want one %s event, got %+v", kind, events)
		}
		for _, part := range append([]string{"server " + key, "GET /v1/models"}, parts...) {
			if !strings.Contains(events[0].Message, part) {
				t.Fatalf("%s line lacks %q: %q", kind, part, events[0].Message)
			}
		}
	}

	// A server first seen failing is logged at once: it is never registered.
	expect(pass("401"), "server_probe_failed", "(HTTP 401)", "not registered until the probe passes")
	if events := pass("401"); len(events) != 0 {
		t.Fatalf("the same failure was logged again: %+v", events)
	}
	expect(pass("500"), "server_probe_failed", "(HTTP 500)")
	expect(pass("not-json"), "server_probe_failed", "(not a model list)")
	expect(pass("ok"), "server_probe_recovered", "passes again")
	if events := pass("ok"); len(events) != 0 {
		t.Fatalf("a healthy server was logged again: %+v", events)
	}

	// A registered server that starts failing keeps its endpoint, unhealthy.
	a.mu.Lock()
	a.status[key].Registered = true
	a.mu.Unlock()
	expect(pass("500"), "server_probe_failed", "(HTTP 500)", "its endpoint shows it as unhealthy")
	if status := a.Status().Servers[0]; status.Health != agentprotocol.HealthUnhealthy || status.HealthDetail != "GET /v1/models returned HTTP 500" {
		t.Fatalf("reported status: %+v", status)
	}

	// A pass cut short by shutdown changes nothing and logs nothing.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	mode.Store("ok")
	a.Probe(cancelled)
	if events := log.take(); len(events) != 0 || a.Status().Servers[0].Health != agentprotocol.HealthUnhealthy {
		t.Fatalf("a cancelled pass changed the state: %+v %+v", events, a.Status().Servers[0])
	}
}

// A token with a trailing newline cannot be sent as a header at all. The
// probe says so instead of "could not reach", sends nothing, and keeps the
// token out of what it reports.
func TestProbeNamesAnAuthorizationHeaderHTTPRefuses(t *testing.T) {
	ctx := context.Background()
	var mode atomic.Value
	mode.Store("ok")
	var requests atomic.Int64
	server := switchableServer(t, &mode, &requests)
	config := ServerConfig{URL: server.URL, AuthHeader: "Bearer " + probeToken + "\n"}
	detected, found := Detector{}.Probe(ctx, config, true)
	if !found || detected.Health != agentprotocol.HealthUnhealthy || detected.Failure != (ProbeFailure{Path: "/v1/models", Reason: "invalid Authorization header"}) {
		t.Fatalf("probe: %+v", detected)
	}
	if !strings.Contains(detected.HealthDetail, "trailing newline") || strings.Contains(detected.HealthDetail, probeToken) || detected.Server.Validate() != nil {
		t.Fatalf("health detail: %q", detected.HealthDetail)
	}
	if requests.Load() != 0 {
		t.Fatalf("the server received %d requests with a header HTTP refuses", requests.Load())
	}

	log := &eventLog{}
	a := New(Config{Servers: []ServerConfig{config}, Events: log.record})
	a.Probe(ctx)
	events := log.take()
	if len(events) != 1 || !strings.Contains(events[0].Message, "(invalid Authorization header)") || strings.Contains(events[0].Message, probeToken) {
		t.Fatalf("events: %+v", events)
	}
}

// Each probe error names its class, so the log says where to look.
func TestProbeErrorsNameTheirClass(t *testing.T) {
	ctx := context.Background()
	closed := httptest.NewServer(http.NotFoundHandler())
	address := closed.URL
	closed.Close()
	detected, found := Detector{}.Probe(ctx, ServerConfig{URL: address}, true)
	if !found || detected.Failure.Reason != "connection refused" || !strings.Contains(detected.HealthDetail, "connection refused") {
		t.Fatalf("closed port: %+v", detected)
	}
	var mode atomic.Value
	mode.Store("ok")
	server := switchableServer(t, &mode, &atomic.Int64{})
	healthy, _ := Detector{}.Probe(ctx, ServerConfig{URL: server.URL}, true)
	if healthy.Health != agentprotocol.HealthHealthy || healthy.Failure != (ProbeFailure{}) {
		t.Fatalf("healthy server: %+v", healthy)
	}
	if !validHeaderValue("Bearer a\tb") || validHeaderValue("Bearer a\r") || validHeaderValue("Bearer \x7f") {
		t.Fatal("header value check")
	}
}
