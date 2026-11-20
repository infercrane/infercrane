package benchmark

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/infercrane/infercrane/internal/metrics"
)

// Cancel-release probe outcomes.
const (
	CancelReleaseReleased = "released"
	CancelReleaseTimedOut = "timed_out"
	CancelReleaseUnknown  = "unknown"
)

// CancelRelease is how long the serving engine kept a streamed request
// running after its client disconnected. Each probe streams one request,
// disconnects once the engine's running-request gauge shows it, and times
// how long the gauge takes to fall back to idle.
//
// MaxReleaseMS is the slowest measured release. It is nil when no probe
// released, and also when any probe timed out: a timed-out probe was slower
// than TimeoutMS by an unknown amount, so no maximum is known.
type CancelRelease struct {
	Engine       string               `json:"engine,omitempty"`
	Probes       int                  `json:"probes"`
	Released     int                  `json:"released"`
	TimedOut     int                  `json:"timed_out"`
	MaxReleaseMS *float64             `json:"max_release_ms,omitempty"`
	TimeoutMS    float64              `json:"timeout_ms"`
	Samples      []CancelReleaseProbe `json:"samples"`
}

// CancelReleaseProbe is one probe. ReleaseMS is set only when Outcome is
// released; a timed-out or unknown probe has a Reason instead of a number.
type CancelReleaseProbe struct {
	Outcome   string   `json:"outcome"`
	ReleaseMS *float64 `json:"release_ms,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

// cancelReleaseProbe holds the probe's bounds. Every phase is bounded, so a
// broken engine costs at most probes × (settle + rise + release).
type cancelReleaseProbe struct {
	probes    int
	maxTokens int
	// poll is the wait between metrics scrapes. A measured release is late
	// by at most one poll plus one scrape round trip.
	poll, scrapeTimeout time.Duration
	// settle bounds the wait for the gauge to read idle before a probe.
	settle time.Duration
	// rise bounds the wait for the gauge to show the probe's request.
	rise time.Duration
	// release bounds the wait for the gauge to fall back after the disconnect.
	release time.Duration
}

func defaultCancelReleaseProbe() *cancelReleaseProbe {
	return &cancelReleaseProbe{probes: 3, maxTokens: 1024, poll: 25 * time.Millisecond, scrapeTimeout: 2 * time.Second, settle: 5 * time.Second, rise: 30 * time.Second, release: 10 * time.Second}
}

// probePrompt is synthetic. With ignore_eos the engine keeps generating until
// max_tokens, so the request is still running when the probe disconnects.
const probePrompt = "Count upward from 1, one number per line. Do not stop."

// errNotEngineMetrics means the endpoint's /metrics carries no running-request
// gauge, for example a gateway's own metrics. Later probes cannot do better.
var errNotEngineMetrics = errors.New("the metrics endpoint reports no serving-engine running-request gauge")

// run probes the endpoint AIPerf just measured. It reads the same /metrics
// that AIPerf scrapes, derived from Config.Endpoint, and runs only when the
// caller enabled Config.ServerMetrics, which campaigns set only when the
// runtime contract declares that path. It never fails the benchmark: missing
// evidence is recorded as unknown, never as a release time.
func (p *cancelReleaseProbe) run(ctx context.Context, cfg Config) *CancelRelease {
	out := &CancelRelease{TimeoutMS: float64(p.release) / float64(time.Millisecond), Samples: make([]CancelReleaseProbe, 0, p.probes)}
	target, err := newProbeTarget(cfg, p)
	defer target.close()
	stop := ""
	if err != nil {
		stop = err.Error()
	}
	var slowest float64
	for index := 0; index < p.probes; index++ {
		out.Probes++
		if stop == "" && ctx.Err() != nil {
			stop = "the benchmark was cancelled before this probe"
		}
		if stop != "" {
			out.Samples = append(out.Samples, CancelReleaseProbe{Outcome: CancelReleaseUnknown, Reason: stop})
			continue
		}
		sample, engine, err := p.once(ctx, target)
		if engine != "" {
			out.Engine = engine
		}
		if errors.Is(err, errNotEngineMetrics) {
			stop = sample.Reason
		}
		switch sample.Outcome {
		case CancelReleaseReleased:
			out.Released++
			slowest = max(slowest, *sample.ReleaseMS)
		case CancelReleaseTimedOut:
			out.TimedOut++
		}
		out.Samples = append(out.Samples, sample)
	}
	if out.Released > 0 && out.TimedOut == 0 {
		out.MaxReleaseMS = &slowest
	}
	return out
}

// once runs one probe. The returned error is set only when later probes
// cannot succeed either.
func (p *cancelReleaseProbe) once(ctx context.Context, target *probeTarget) (CancelReleaseProbe, string, error) {
	unknown := func(reason string) CancelReleaseProbe {
		return CancelReleaseProbe{Outcome: CancelReleaseUnknown, Reason: reason}
	}
	// 1. Baseline: the engine must be idle, or the gauge cannot attribute a
	// rise or a fall to the probe's request.
	baseline, engine, err := p.waitIdle(ctx, target)
	switch {
	case errors.Is(err, errNotEngineMetrics):
		return unknown(err.Error()), engine, err
	case ctx.Err() != nil:
		return unknown("the benchmark was cancelled during the probe"), engine, nil
	case err != nil:
		return unknown("the engine metrics could not be read: " + err.Error()), engine, nil
	case baseline != 0:
		return unknown(fmt.Sprintf("the engine was not idle: its running-request gauge still read %g after %s", baseline, p.settle)), engine, nil
	}

	// 2. Stream one long request.
	streamCtx, disconnect := context.WithCancel(ctx)
	stream, err := target.stream(streamCtx, p.maxTokens)
	if err != nil {
		disconnect()
		return unknown(err.Error()), engine, nil
	}
	defer func() {
		disconnect()
		stream.wait()
	}()

	// 3. Read chunks until the gauge shows the request running.
	riseDeadline := time.Now().Add(p.rise)
	for {
		if stream.ended() {
			return unknown("the probe request ended before the engine reported it running: " + stream.endReason()), engine, nil
		}
		running, _, err := target.running(ctx)
		if err == nil && running > 1 {
			return unknown(fmt.Sprintf("another request was running during the probe (gauge %g)", running)), engine, nil
		}
		if err == nil && running > baseline && stream.chunks.Load() > 0 {
			break
		}
		if !time.Now().Before(riseDeadline) {
			return unknown(fmt.Sprintf("the engine's running-request gauge did not show the streaming probe request within %s", p.rise)), engine, nil
		}
		if !sleep(ctx, p.poll) {
			return unknown("the benchmark was cancelled during the probe"), engine, nil
		}
	}

	// 4. Disconnect and time the gauge's return to idle.
	started := time.Now()
	disconnect()
	stream.wait()
	if stream.completed.Load() {
		// The engine finished the request on its own before it saw the
		// disconnect, so its release says nothing about cancellation.
		return unknown("the probe request ended before the probe disconnected: " + stream.endReason()), engine, nil
	}
	releaseDeadline := started.Add(p.release)
	for {
		scraped := time.Now()
		running, _, err := target.running(ctx)
		if err == nil && running <= baseline {
			elapsed := float64(time.Since(started)) / float64(time.Millisecond)
			return CancelReleaseProbe{Outcome: CancelReleaseReleased, ReleaseMS: &elapsed}, engine, nil
		}
		if err == nil && running > 1 {
			return unknown(fmt.Sprintf("another request started while the probe waited for the release (gauge %g)", running)), engine, nil
		}
		if !scraped.Before(releaseDeadline) {
			// 5. A timeout needs a scrape taken at the bound that still shows
			// the request. It is not a number: the engine kept the request, or
			// it did not republish the gauge once idle.
			if err != nil {
				return unknown(fmt.Sprintf("the engine metrics could not be read %s after the disconnect: %s", p.release, err)), engine, nil
			}
			return CancelReleaseProbe{Outcome: CancelReleaseTimedOut, Reason: fmt.Sprintf("the running-request gauge did not return to idle within %s of the disconnect; the engine kept the request or did not republish the gauge", p.release)}, engine, nil
		}
		if !sleep(ctx, min(p.poll, max(time.Until(releaseDeadline), 0))) {
			return unknown("the benchmark was cancelled while the probe waited for the release"), engine, nil
		}
	}
}

// waitIdle polls the gauge until it reads zero or the settle bound passes,
// and returns the last reading. A scrape error is returned only when no
// scrape in the window succeeded.
func (p *cancelReleaseProbe) waitIdle(ctx context.Context, target *probeTarget) (float64, string, error) {
	deadline := time.Now().Add(p.settle)
	var last float64
	var lastErr error
	engine, read := "", false
	for {
		running, name, err := target.running(ctx)
		switch {
		case errors.Is(err, errNotEngineMetrics):
			return 0, name, err
		case err == nil:
			last, engine, read = running, name, true
			if running == 0 {
				return 0, engine, nil
			}
		default:
			lastErr = err
		}
		if !time.Now().Before(deadline) || !sleep(ctx, p.poll) {
			if read {
				return last, engine, nil
			}
			if lastErr == nil {
				lastErr = errors.New("no scrape completed")
			}
			return 0, engine, lastErr
		}
	}
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// probeTarget is the benchmarked endpoint. Its URLs are derived from
// Config.Endpoint, the address AIPerf loads, never from separate input.
type probeTarget struct {
	metricsURL, chatURL string
	model, apiKey       string
	scrapeTimeout       time.Duration
	scrapes, streams    *http.Client
}

func newProbeTarget(cfg Config, p *cancelReleaseProbe) (*probeTarget, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return &probeTarget{}, errors.New("the benchmark endpoint is not an http or https base URL")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := func(keepAlive bool, headerTimeout time.Duration) *http.Transport {
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second,
			ResponseHeaderTimeout: headerTimeout, DisableKeepAlives: !keepAlive, MaxIdleConnsPerHost: 1,
			// HTTP/1.1, as OpenAI clients use by default: cancelling the
			// stream closes its TCP connection, which is the disconnect an
			// http-disconnect runtime contract cancels on.
			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		}
	}
	return &probeTarget{
		metricsURL: base + "/metrics", chatURL: base + "/v1/chat/completions", model: cfg.Model, apiKey: cfg.APIKey, scrapeTimeout: p.scrapeTimeout,
		scrapes: &http.Client{Transport: transport(true, p.scrapeTimeout)}, streams: &http.Client{Transport: transport(false, p.rise)},
	}, nil
}

func (t *probeTarget) close() {
	for _, client := range []*http.Client{t.scrapes, t.streams} {
		if client != nil {
			client.CloseIdleConnections()
		}
	}
}

func (t *probeTarget) authorize(request *http.Request) {
	if t.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
}

// running reads the engine's running-request gauge through the same parser
// as every other engine scrape.
func (t *probeTarget) running(ctx context.Context) (float64, string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.scrapeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, t.metricsURL, nil)
	if err != nil {
		return 0, "", err
	}
	t.authorize(request)
	response, err := t.scrapes.Do(request)
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, "", fmt.Errorf("the metrics scrape took longer than %s", t.scrapeTimeout)
	}
	if err != nil {
		return 0, "", errors.New("the metrics endpoint was unreachable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return 0, "", fmt.Errorf("the metrics endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, metrics.MaxEnginePayloadBytes+1))
	if err != nil {
		return 0, "", errors.New("the metrics response could not be read")
	}
	snapshot, err := metrics.ParseEngine(string(body))
	if errors.Is(err, metrics.ErrNoEngineMetrics) {
		return 0, "", errNotEngineMetrics
	}
	if err != nil {
		return 0, "", errors.New("the metrics response was not a readable engine exposition")
	}
	if snapshot.RequestsRunning == nil {
		return 0, snapshot.Engine, errNotEngineMetrics
	}
	return *snapshot.RequestsRunning, snapshot.Engine, nil
}

// probeStream is one streaming chat request whose body is read until the
// connection ends.
type probeStream struct {
	chunks atomic.Int64
	// finished is set when the body ends for any reason; completed only when
	// the engine itself ended the request.
	finished, completed atomic.Bool
	reason              atomic.Value
	done                chan struct{}
}

func (t *probeTarget) stream(ctx context.Context, maxTokens int) (*probeStream, error) {
	body, err := json.Marshal(map[string]any{
		"model": t.model, "messages": []map[string]string{{"role": "user", "content": probePrompt}},
		"max_tokens": maxTokens, "ignore_eos": true, "temperature": 0, "stream": true,
	})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.chatURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	t.authorize(request)
	response, err := t.streams.Do(request)
	if err != nil {
		return nil, errors.New("the probe request could not be sent")
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		return nil, fmt.Errorf("the probe request was refused with HTTP %d", response.StatusCode)
	}
	stream := &probeStream{done: make(chan struct{})}
	go stream.read(response.Body)
	return stream, nil
}

// read consumes server-sent events so the engine is never blocked on a full
// socket, and notes whether the request finished on its own.
func (s *probeStream) read(body io.ReadCloser) {
	defer close(s.done)
	defer body.Close()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 16<<10), 1<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			s.complete("the stream completed")
			return
		}
		s.chunks.Add(1)
		var chunk struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil {
			for _, choice := range chunk.Choices {
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					s.complete("the engine finished it (" + boundedLabel(*choice.FinishReason) + ")")
					return
				}
			}
		}
	}
	s.finish("the stream closed")
}

func (s *probeStream) complete(reason string) {
	s.completed.Store(true)
	s.finish(reason)
}

func (s *probeStream) finish(reason string) {
	if s.finished.CompareAndSwap(false, true) {
		s.reason.Store(reason)
	}
}

// ended reports whether the stream finished before the probe disconnected.
func (s *probeStream) ended() bool { return s.finished.Load() }

func (s *probeStream) endReason() string {
	reason, _ := s.reason.Load().(string)
	return reason
}

func (s *probeStream) wait() { <-s.done }
