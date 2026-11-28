package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// rawServer listens on a loopback port and hands each connection to handle,
// for servers that do not speak HTTP. Connections still open are closed at
// cleanup.
func rawServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var open []net.Conn
	var handlers sync.WaitGroup
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			mu.Lock()
			open = append(open, conn)
			mu.Unlock()
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				handle(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for _, conn := range open {
			_ = conn.Close()
		}
		mu.Unlock()
		handlers.Wait()
	})
	return "http://" + listener.Addr().String()
}

// readRequest reads the probe's request line and headers.
func readRequest(conn net.Conn) {
	_, _ = http.ReadRequest(bufio.NewReader(conn))
}

// onDefaultPort makes the agent's port scan probe only base's port.
func onDefaultPort(t *testing.T, base string) {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	previous := DefaultPorts
	DefaultPorts = []int{port}
	t.Cleanup(func() { DefaultPorts = previous })
}

// shortTimeout is a probe client that gives up quickly, for a server that
// accepts the connection and never answers.
func shortTimeout() Detector {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return Detector{HTTP: &http.Client{Timeout: 200 * time.Millisecond, Transport: transport}}
}

// A process on a default port that accepted the connection but gave no usable
// reply was dropped like an empty port, so the agent neither reported nor
// logged it. It is now an unhealthy server, logged once like any other.
func TestADefaultPortThatAnswersButFailsTheProbeIsReportedAndLogged(t *testing.T) {
	ctx := context.Background()
	certificate := httptest.NewTLSServer(http.NotFoundHandler())
	pair := certificate.TLS.Certificates[0]
	certificate.Close()
	for name, test := range map[string]struct {
		handle   func(net.Conn)
		detector Detector
		reason   string
		hint     bool
	}{
		"not HTTP": {handle: func(conn net.Conn) {
			readRequest(conn)
			_, _ = conn.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
			_ = conn.Close()
		}, reason: "not an HTTP reply", hint: true},
		"closed": {handle: func(conn net.Conn) {
			readRequest(conn)
			_ = conn.Close()
		}, reason: "closed without an HTTP reply", hint: true},
		// A server that serves HTTPS cannot read a plain HTTP request.
		"serves HTTPS": {handle: func(conn net.Conn) {
			secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{pair}})
			_ = secure.Handshake()
			_ = secure.Close()
		}, reason: "closed without an HTTP reply", hint: true},
		"no reply": {handle: func(conn net.Conn) {
			readRequest(conn)
			_, _ = io.Copy(io.Discard, conn)
		}, detector: shortTimeout(), reason: "timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			base := rawServer(t, test.handle)
			detected, found := test.detector.Probe(ctx, ServerConfig{URL: base}, false)
			if !found || detected.Health != agentprotocol.HealthUnhealthy || detected.Failure != (ProbeFailure{Path: "/v1/models", Reason: test.reason}) || detected.Server.Validate() != nil {
				t.Fatalf("probe: found=%v %+v", found, detected)
			}
			if hint := "name it with --server https://" + strings.TrimPrefix(base, "http://"); strings.Contains(detected.HealthDetail, hint) != test.hint {
				t.Fatalf("health detail %q, HTTPS hint wanted=%v", detected.HealthDetail, test.hint)
			}

			onDefaultPort(t, base)
			log := &eventLog{}
			a := New(Config{AutoDetect: true, Detector: test.detector, Events: log.record})
			a.Probe(ctx)
			events := log.take()
			if len(events) != 1 || events[0].Kind != "server_probe_failed" || !strings.Contains(events[0].Message, "health probe GET /v1/models failed ("+test.reason+"); the server is not registered until the probe passes") {
				t.Fatalf("events: %+v", events)
			}
			if status := a.Status().Servers; len(status) != 1 || status[0].Health != agentprotocol.HealthUnhealthy {
				t.Fatalf("reported servers: %+v", status)
			}
			a.Probe(ctx)
			if events = log.take(); len(events) != 0 {
				t.Fatalf("the same failure was logged again: %+v", events)
			}
		})
	}
}

// Most default ports are empty. A scan that finds nothing listening reports
// and logs nothing, pass after pass.
func TestAnEmptyDefaultPortIsNeitherReportedNorLogged(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	base := closed.URL
	closed.Close()
	onDefaultPort(t, base)
	var events []Event
	var mu sync.Mutex
	a := New(Config{AutoDetect: true, Events: func(event Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}})
	for range 2 {
		a.Probe(context.Background())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 0 || len(a.Status().Servers) != 0 {
		t.Fatalf("an empty port was reported: events=%+v servers=%+v", events, a.Status().Servers)
	}
}

// A server found on a default port that goes away was forgotten without a
// word. Its last line now says it stopped answering, once.
func TestADefaultPortServerThatGoesAwayIsLoggedOnce(t *testing.T) {
	for _, mode := range []string{"ok", "500"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var state atomic.Value
			state.Store(mode)
			server := switchableServer(t, &state, &atomic.Int64{})
			key := ServerKey(server.URL)
			onDefaultPort(t, server.URL)
			log := &eventLog{}
			a := New(Config{AutoDetect: true, Events: log.record})
			a.Probe(ctx)
			_ = log.take()
			server.Close()
			a.Probe(ctx)
			events := log.take()
			if len(events) != 1 || events[0].Kind != "server_probe_failed" || events[0].Server == nil || events[0].Server.Key != key {
				t.Fatalf("events: %+v", events)
			}
			if want := "server " + key + ": health probe GET /v1/models failed (stopped answering); the agent stops reporting it until a server answers on " + server.URL + " again"; events[0].Message != want {
				t.Fatalf("line %q, want %q", events[0].Message, want)
			}
			if servers := a.Status().Servers; len(servers) != 0 {
				t.Fatalf("a server that went away is still reported: %+v", servers)
			}
			a.Probe(ctx)
			if events = log.take(); len(events) != 0 {
				t.Fatalf("an empty port was logged: %+v", events)
			}
		})
	}
}

// An https server that answers in plain HTTP fails the TLS handshake. The
// probe names that, instead of "could not reach".
func TestAnHTTPSProbeOfAPlainServerNamesTheTLSHandshake(t *testing.T) {
	var mode atomic.Value
	mode.Store("ok")
	server := switchableServer(t, &mode, &atomic.Int64{})
	secure := "https://" + strings.TrimPrefix(server.URL, "http://")
	detected, found := Detector{}.Probe(context.Background(), ServerConfig{URL: secure}, true)
	if !found || detected.Failure.Reason != "TLS handshake failed" || !strings.Contains(detected.HealthDetail, "name it with --server "+server.URL) {
		t.Fatalf("probe: %+v", detected)
	}
}
