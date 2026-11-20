package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngine is a streaming OpenAI server with a vLLM-style running gauge.
type fakeEngine struct {
	mu sync.Mutex
	// other is traffic that is not the probe's.
	other int
	// probe is how many probe requests the gauge currently shows.
	probe int
	// shows makes the gauge count a streaming request; false models a gauge
	// the engine does not refresh while busy.
	shows bool
	// releaseAfter delays the gauge's fall after a disconnect. releasePlan,
	// when set, gives the delay for each request in order instead.
	releaseAfter time.Duration
	releasePlan  []time.Duration
	// finishAfter ends the stream on its own after that many chunks.
	finishAfter int
	// metrics replaces the engine exposition, as a gateway's /metrics would.
	metrics string
	// metricsDownAfterDisconnect makes /metrics fail once a client left.
	metricsDownAfterDisconnect, disconnected bool
	streams, scrapes                         int
	authorization, request                   string
}

func newFakeEngine(t *testing.T, engine *fakeEngine) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			engine.mu.Lock()
			engine.scrapes++
			if engine.metricsDownAfterDisconnect && engine.disconnected {
				engine.mu.Unlock()
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			body := engine.metrics
			if body == "" {
				body = fmt.Sprintf("# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running{engine=\"0\",model_name=\"m\"} %d\n", engine.other+engine.probe)
			}
			engine.mu.Unlock()
			_, _ = w.Write([]byte(body))
		case "/v1/chat/completions":
			engine.serveStream(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func (e *fakeEngine) serveStream(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil || body["stream"] != true {
		http.Error(w, "streaming chat request required", http.StatusBadRequest)
		return
	}
	encoded, _ := json.Marshal(body)
	e.mu.Lock()
	e.streams++
	e.authorization, e.request = r.Header.Get("Authorization"), string(encoded)
	if e.shows {
		e.probe++
	}
	shows, releaseAfter, finishAfter := e.shows, e.releaseAfter, e.finishAfter
	if e.streams <= len(e.releasePlan) {
		releaseAfter = e.releasePlan[e.streams-1]
	}
	e.mu.Unlock()
	release := func() {
		if !shows {
			return
		}
		e.mu.Lock()
		e.probe--
		e.mu.Unlock()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for chunk := 1; ; chunk++ {
		if finishAfter > 0 && chunk > finishAfter {
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
			flusher.Flush()
			release()
			return
		}
		if _, err := fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%d\\n\"},\"finish_reason\":null}]}\n\n", chunk); err != nil {
			break
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			// The client disconnected: release after the engine's delay.
			e.mu.Lock()
			e.disconnected = true
			e.mu.Unlock()
			time.AfterFunc(releaseAfter, release)
			return
		case <-ticker.C:
		}
	}
	time.AfterFunc(releaseAfter, release)
}

func (e *fakeEngine) counts() (streams, scrapes int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.streams, e.scrapes
}

func (e *fakeEngine) lastRequest() (authorization, body string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.authorization, e.request
}

func testCancelReleaseProbe() *cancelReleaseProbe {
	return &cancelReleaseProbe{probes: 3, maxTokens: 64, poll: 2 * time.Millisecond, scrapeTimeout: time.Second, settle: 500 * time.Millisecond, rise: 2 * time.Second, release: 2 * time.Second}
}

func outcomes(result *CancelRelease) string {
	values := make([]string, 0, len(result.Samples))
	for _, sample := range result.Samples {
		values = append(values, sample.Outcome)
	}
	return strings.Join(values, ",")
}

func TestCancelReleaseMeasuresTheGaugeFallAfterDisconnect(t *testing.T) {
	engine := &fakeEngine{shows: true, releaseAfter: 30 * time.Millisecond}
	endpoint := newFakeEngine(t, engine)

	result := testCancelReleaseProbe().run(context.Background(), Config{Endpoint: endpoint + "/", Model: "m", APIKey: "secret"})

	if result.Engine != "vllm" || result.Probes != 3 || result.Released != 3 || result.TimedOut != 0 || outcomes(result) != "released,released,released" {
		t.Fatalf("unexpected probe summary: %+v", result)
	}
	slowest := 0.0
	for _, sample := range result.Samples {
		// The engine drops the gauge 30 ms after it sees the disconnect, so no
		// measured release can be shorter.
		if sample.ReleaseMS == nil || *sample.ReleaseMS < 30 || *sample.ReleaseMS >= 2000 || sample.Reason != "" {
			t.Fatalf("release was not measured from the disconnect: %+v", sample)
		}
		slowest = max(slowest, *sample.ReleaseMS)
	}
	if result.MaxReleaseMS == nil || *result.MaxReleaseMS != slowest || result.TimeoutMS != 2000 {
		t.Fatalf("max release = %v, want %v; timeout %v", result.MaxReleaseMS, slowest, result.TimeoutMS)
	}
	streams, _ := engine.counts()
	authorization, body := engine.lastRequest()
	if streams != 3 || authorization != "Bearer secret" {
		t.Fatalf("streams = %d, authorization %q", streams, authorization)
	}
	var request struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		IgnoreEOS bool   `json:"ignore_eos"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil || request.Model != "m" || request.MaxTokens != 64 || !request.IgnoreEOS {
		t.Fatalf("probe request = %s", body)
	}
}

func TestCancelReleaseStaleGaugeStaysUnknownNotZero(t *testing.T) {
	engine := &fakeEngine{shows: false}
	endpoint := newFakeEngine(t, engine)
	probe := testCancelReleaseProbe()
	probe.rise = 100 * time.Millisecond

	result := probe.run(context.Background(), Config{Endpoint: endpoint, Model: "m"})

	if result.Released != 0 || result.TimedOut != 0 || result.MaxReleaseMS != nil || outcomes(result) != "unknown,unknown,unknown" {
		t.Fatalf("a gauge that never rose produced a release: %+v", result)
	}
	for _, sample := range result.Samples {
		if sample.ReleaseMS != nil || !strings.Contains(sample.Reason, "did not show the streaming probe request") {
			t.Fatalf("unexpected sample: %+v", sample)
		}
	}
	if streams, _ := engine.counts(); streams != 3 {
		t.Fatalf("streams = %d, want 3", streams)
	}
}

func TestCancelReleaseBusyBaselineIsUnknownWithoutSendingLoad(t *testing.T) {
	engine := &fakeEngine{shows: true, other: 2}
	endpoint := newFakeEngine(t, engine)
	probe := testCancelReleaseProbe()
	probe.settle = 50 * time.Millisecond

	result := probe.run(context.Background(), Config{Endpoint: endpoint, Model: "m"})

	if result.Released != 0 || result.MaxReleaseMS != nil || outcomes(result) != "unknown,unknown,unknown" || !strings.Contains(result.Samples[0].Reason, "was not idle") {
		t.Fatalf("a busy engine produced a release: %+v", result)
	}
	if streams, _ := engine.counts(); streams != 0 {
		t.Fatalf("the probe streamed %d requests to a busy engine", streams)
	}
}

func TestCancelReleaseTimeoutIsCountedWithoutANumber(t *testing.T) {
	// The engine releases 300 ms after the disconnect; the probe waits 60 ms.
	// The settle window lets each next probe start once the engine is idle.
	engine := &fakeEngine{shows: true, releaseAfter: 300 * time.Millisecond}
	endpoint := newFakeEngine(t, engine)
	probe := testCancelReleaseProbe()
	probe.release, probe.settle = 60*time.Millisecond, 2*time.Second

	result := probe.run(context.Background(), Config{Endpoint: endpoint, Model: "m"})

	if result.TimedOut != 3 || result.Released != 0 || result.MaxReleaseMS != nil || result.TimeoutMS != 60 || outcomes(result) != "timed_out,timed_out,timed_out" {
		t.Fatalf("unexpected timeout summary: %+v", result)
	}
	for _, sample := range result.Samples {
		if sample.ReleaseMS != nil || !strings.Contains(sample.Reason, "did not return to idle within 60ms") {
			t.Fatalf("a timed-out probe carried a number or no reason: %+v", sample)
		}
	}
}

func TestCancelReleaseUnreadableMetricsAtTheBoundAreUnknownNotATimeout(t *testing.T) {
	engine := &fakeEngine{shows: true, metricsDownAfterDisconnect: true}
	endpoint := newFakeEngine(t, engine)
	probe := testCancelReleaseProbe()
	probe.probes, probe.release = 1, 60*time.Millisecond

	result := probe.run(context.Background(), Config{Endpoint: endpoint, Model: "m"})

	if result.TimedOut != 0 || result.Released != 0 || outcomes(result) != "unknown" || !strings.Contains(result.Samples[0].Reason, "could not be read 60ms after the disconnect: the metrics endpoint returned HTTP 503") {
		t.Fatalf("unreadable metrics were reported as a timeout: %+v", result)
	}
}

func TestCancelReleaseKeepsNoMaximumWhenAnyProbeTimedOut(t *testing.T) {
	// The first request releases quickly; the next two are slower than the
	// 150 ms bound. The fast release must not stand in for the maximum.
	engine := &fakeEngine{shows: true, releasePlan: []time.Duration{10 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond}}
	endpoint := newFakeEngine(t, engine)
	probe := testCancelReleaseProbe()
	probe.release, probe.settle = 150*time.Millisecond, 2*time.Second

	result := probe.run(context.Background(), Config{Endpoint: endpoint, Model: "m"})

	if result.Released != 1 || result.TimedOut != 2 || result.MaxReleaseMS != nil || outcomes(result) != "released,timed_out,timed_out" {
		t.Fatalf("a maximum was reported although a probe timed out: %+v", result)
	}
	if result.Samples[0].ReleaseMS == nil || *result.Samples[0].ReleaseMS < 10 {
		t.Fatalf("the released probe lost its time: %+v", result.Samples[0])
	}
}

func TestCancelReleaseGatewayMetricsStopAfterOneScrape(t *testing.T) {
	engine := &fakeEngine{shows: true, metrics: "# TYPE gateway_requests_total counter\ngateway_requests_total 5\n"}
	endpoint := newFakeEngine(t, engine)

	result := testCancelReleaseProbe().run(context.Background(), Config{Endpoint: endpoint, Model: "m"})

	if result.Probes != 3 || outcomes(result) != "unknown,unknown,unknown" || result.Engine != "" {
		t.Fatalf("unexpected summary: %+v", result)
	}
	for _, sample := range result.Samples {
		if sample.Reason != errNotEngineMetrics.Error() {
			t.Fatalf("reason = %q", sample.Reason)
		}
	}
	if streams, scrapes := engine.counts(); streams != 0 || scrapes != 1 {
		t.Fatalf("streams = %d, scrapes = %d; want 0 and 1", streams, scrapes)
	}
}

func TestCancelReleaseRequestThatFinishesOnItsOwnIsUnknown(t *testing.T) {
	engine := &fakeEngine{shows: false, finishAfter: 3}
	endpoint := newFakeEngine(t, engine)

	result := testCancelReleaseProbe().run(context.Background(), Config{Endpoint: endpoint, Model: "m"})

	if result.Released != 0 || result.MaxReleaseMS != nil || outcomes(result) != "unknown,unknown,unknown" || !strings.Contains(result.Samples[0].Reason, "ended before the engine reported it running") {
		t.Fatalf("a request the engine finished produced a release: %+v", result)
	}
}

func TestCancelReleaseRefusedOrUnreachableEndpointsAreUnknown(t *testing.T) {
	for name, endpoint := range map[string]string{
		"not a URL":   "example",
		"unreachable": "http://127.0.0.1:1",
	} {
		t.Run(name, func(t *testing.T) {
			probe := testCancelReleaseProbe()
			probe.settle = 20 * time.Millisecond
			result := probe.run(context.Background(), Config{Endpoint: endpoint, Model: "m"})
			if result.Probes != 3 || result.Released != 0 || result.MaxReleaseMS != nil || outcomes(result) != "unknown,unknown,unknown" || result.Samples[0].Reason == "" {
				t.Fatalf("unexpected summary: %+v", result)
			}
		})
	}
}

func TestCancelReleaseStopsWhenTheBenchmarkIsCancelled(t *testing.T) {
	engine := &fakeEngine{shows: true}
	endpoint := newFakeEngine(t, engine)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := testCancelReleaseProbe().run(ctx, Config{Endpoint: endpoint, Model: "m"})

	if outcomes(result) != "unknown,unknown,unknown" || !strings.Contains(result.Samples[0].Reason, "cancelled") {
		t.Fatalf("unexpected summary: %+v", result)
	}
	if streams, _ := engine.counts(); streams != 0 {
		t.Fatalf("a cancelled benchmark streamed %d probe requests", streams)
	}
}

func TestRunProbesCancelReleaseOnlyWithServerMetrics(t *testing.T) {
	engine := &fakeEngine{shows: true, releaseAfter: 5 * time.Millisecond}
	endpoint := newFakeEngine(t, engine)

	result, err := run(context.Background(), Config{Endpoint: endpoint, Model: "m", Requests: 1, Concurrency: 1}, &serverMetricsRunner{}, testCancelReleaseProbe())
	if err != nil {
		t.Fatal(err)
	}
	if streams, scrapes := engine.counts(); result.CancelRelease != nil || streams != 0 || scrapes != 0 {
		t.Fatalf("probed without server metrics: result %+v, streams %d, scrapes %d", result.CancelRelease, streams, scrapes)
	}

	result, err = run(context.Background(), Config{Endpoint: endpoint, Model: "m", Requests: 1, Concurrency: 1, ServerMetrics: true}, &serverMetricsRunner{}, testCancelReleaseProbe())
	if err != nil {
		t.Fatal(err)
	}
	if result.CancelRelease == nil || result.CancelRelease.Released != 3 || result.CancelRelease.MaxReleaseMS == nil {
		t.Fatalf("cancel release = %+v", result.CancelRelease)
	}
}
