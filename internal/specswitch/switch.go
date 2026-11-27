package specswitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

var (
	errQueueFull   = errors.New("the switch's queue is full")
	errUnavailable = errors.New("no engine can serve")
)

// maxSwapRecords bounds the swap history kept for the state endpoint.
const maxSwapRecords = 64

// SwapRecord is one swap the switch made or called off.
type SwapRecord struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Load is the load the decision read: the highest reading in the
	// smoothing window, in requests at once.
	Load   int       `json:"load"`
	Policy string    `json:"policy"`
	Result string    `json:"result"`
	Error  string    `json:"error,omitempty"`
	Start  time.Time `json:"start"`
	// DrainSeconds is the wait for requests in flight; SwapSeconds runs from
	// the start until requests flow again.
	DrainSeconds float64 `json:"drain_seconds"`
	SwapSeconds  float64 `json:"swap_seconds"`
	// The parts after the drain: putting the awake engine to sleep, waking
	// the next one (its weights, graphs and buffers copied back, then its
	// cache mapped, when the controller wakes in parts), and its first
	// token: listing the model and writing one token.
	SleepSeconds       float64 `json:"sleep_seconds"`
	WakeSeconds        float64 `json:"wake_seconds"`
	WakeWeightsSeconds float64 `json:"wake_weights_seconds,omitempty"`
	WakeCacheSeconds   float64 `json:"wake_cache_seconds,omitempty"`
	FirstTokenSeconds  float64 `json:"first_token_seconds"`
}

// partedWaker is a controller that wakes an engine's weights and its cache
// apart and times each (VLLM.WakeInParts).
type partedWaker interface {
	WakeInParts(ctx context.Context, engine Engine) (weights, cache time.Duration, err error)
}

// Swap results.
const (
	SwapDone         = "swapped"
	SwapDrainTimeout = "drain_timeout"
	SwapSleepFailed  = "sleep_failed"
	SwapWakeFailed   = "wake_failed"
	SwapFatal        = "fatal"
)

// waiter is a request waiting in the switch for room.
type waiter struct {
	ready      chan int
	dispatched bool
	// size is the request body's bytes, held while the request waits.
	size int
}

// Switch fronts the engines of one server and keeps one of them awake.
type Switch struct {
	cfg     Config
	ctl     Controller
	now     func() time.Time
	log     *slog.Logger
	proxies []*httputil.ReverseProxy
	metrics *http.Client

	decMu sync.Mutex
	dec   *decider

	mu          sync.Mutex
	active      int
	activeSince time.Time
	// retryAfter holds decisions after a swap that failed, for MinDwell.
	retryAfter  time.Time
	inflight    int
	queue       []*waiter
	queuedBytes int
	swapping    bool
	drained     chan struct{}
	failed      error
	policy      string
	swaps       []SwapRecord
	counts      map[[3]string]int
	swapSeconds float64
	swapCount   int
	rejected    int
	queuedTotal int
}

// Option adjusts a new switch.
type Option func(*Switch)

// WithLogger sets the logger.
func WithLogger(logger *slog.Logger) Option { return func(s *Switch) { s.log = logger } }

// New returns a switch whose awake engine is the one serving the lowest band
// of the policy in force. The caller has started every engine and put all but
// that one to sleep.
func New(cfg Config, ctl Controller, options ...Option) (*Switch, error) {
	cfg, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	s := &Switch{cfg: cfg, ctl: ctl, now: time.Now, log: slog.Default(), policy: cfg.Policy, counts: map[[3]string]int{},
		metrics: &http.Client{Timeout: 2 * time.Second}}
	for _, option := range options {
		option(s)
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns:        MaxRoom,
		MaxIdleConnsPerHost: MaxRoom,
		// vLLM 0.34 closes a connection idle for 5 seconds, and Go does not
		// resend a POST on a reused connection the server closed: drop idle
		// connections first.
		IdleConnTimeout: 4 * time.Second,
	}
	for _, engine := range cfg.Engines {
		target, _ := url.Parse(engine.URL)
		s.proxies = append(s.proxies, &httputil.ReverseProxy{
			Rewrite: func(request *httputil.ProxyRequest) {
				request.SetURL(target)
				request.Out.Host = target.Host
			},
			Transport:     transport,
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				writeError(w, http.StatusBadGateway, "upstream_unavailable", "the engine did not answer: "+err.Error())
			},
		})
	}
	s.dec = newDecider(cfg.Timing, cfg.Policies[cfg.Policy])
	s.active = cfg.engineIndex(cfg.Policies[cfg.Policy][0].Engine)
	s.activeSince = s.now()
	return s, nil
}

// InitialEngine is the engine New keeps awake: the lowest band's.
func InitialEngine(cfg Config) string { return cfg.Policies[cfg.Policy][0].Engine }

// Run reads the load every Sample and swaps engines when the policy says so,
// until ctx ends. Only one swap runs at a time.
func (s *Switch) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Timing.Sample.D())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		if s.failed != nil {
			s.mu.Unlock()
			return
		}
		load, awake, since, retryAfter := s.inflight+len(s.queue), s.cfg.Engines[s.active].Name, s.activeSince, s.retryAfter
		s.mu.Unlock()
		if s.now().Before(retryAfter) {
			continue
		}
		s.decMu.Lock()
		target, swap := s.dec.observe(s.now(), load, awake, since)
		// The swap records the load the decision read: the highest reading in
		// the smoothing window.
		smoothed := s.dec.load()
		s.decMu.Unlock()
		if !swap {
			continue
		}
		s.swap(ctx, s.cfg.engineIndex(target), smoothed)
		s.decMu.Lock()
		s.dec.reset()
		s.decMu.Unlock()
	}
}

// SetPolicy puts another policy in force. A pending band change is
// forgotten; the new bands must hold on their own.
func (s *Switch) SetPolicy(name string) error {
	bands, ok := s.cfg.Policies[name]
	if !ok {
		return fmt.Errorf("no policy %q", name)
	}
	s.decMu.Lock()
	s.dec.setBands(bands)
	s.decMu.Unlock()
	s.mu.Lock()
	s.policy = name
	s.mu.Unlock()
	return nil
}

// acquire waits for room on the awake engine and returns its index. Requests
// keep their order: none overtakes one already waiting.
func (s *Switch) acquire(ctx context.Context, size int) (int, error) {
	s.mu.Lock()
	if s.failed != nil {
		s.mu.Unlock()
		return -1, errUnavailable
	}
	if !s.swapping && len(s.queue) == 0 && s.hasRoomLocked() {
		s.inflight++
		index := s.active
		s.mu.Unlock()
		return index, nil
	}
	if len(s.queue) >= s.cfg.QueueLimit || s.queuedBytes+size > MaxQueuedBytes {
		s.rejected++
		s.mu.Unlock()
		return -1, errQueueFull
	}
	w := &waiter{ready: make(chan int, 1), size: size}
	s.queue = append(s.queue, w)
	s.queuedBytes += size
	s.queuedTotal++
	s.mu.Unlock()
	select {
	case index := <-w.ready:
		if index < 0 {
			return -1, errUnavailable
		}
		return index, nil
	case <-ctx.Done():
		s.mu.Lock()
		if w.dispatched {
			s.mu.Unlock()
			if index := <-w.ready; index >= 0 {
				s.release()
			}
			return -1, ctx.Err()
		}
		for position, queued := range s.queue {
			if queued == w {
				s.queue = append(s.queue[:position], s.queue[position+1:]...)
				s.queuedBytes -= w.size
				break
			}
		}
		s.mu.Unlock()
		return -1, ctx.Err()
	}
}

// release ends one request in flight.
func (s *Switch) release() {
	s.mu.Lock()
	s.inflight--
	if s.inflight == 0 && s.drained != nil {
		close(s.drained)
		s.drained = nil
	}
	s.dispatchLocked()
	s.mu.Unlock()
}

func (s *Switch) hasRoomLocked() bool {
	room := s.cfg.Engines[s.active].Room
	return room == 0 || s.inflight < room
}

// dispatchLocked sends waiting requests to the awake engine while it has room.
func (s *Switch) dispatchLocked() {
	if s.failed != nil {
		for _, w := range s.queue {
			w.dispatched = true
			w.ready <- -1
		}
		s.queue, s.queuedBytes = nil, 0
		return
	}
	for !s.swapping && len(s.queue) > 0 && s.hasRoomLocked() {
		w := s.queue[0]
		s.queue = s.queue[1:]
		s.queuedBytes -= w.size
		w.dispatched = true
		s.inflight++
		w.ready <- s.active
	}
}

// swap drains the awake engine, puts it to sleep, wakes target and lets the
// waiting requests flow to it. If the drain does not end in time it is called
// off; if a step fails the switch wakes the engine it started from again; if
// that fails too, no engine serves and the switch reports itself unhealthy.
func (s *Switch) swap(ctx context.Context, target, load int) {
	start := s.now()
	s.mu.Lock()
	from := s.active
	record := SwapRecord{From: s.cfg.Engines[from].Name, To: s.cfg.Engines[target].Name, Load: load, Policy: s.policy, Start: start}
	s.swapping = true
	var drained chan struct{}
	if s.inflight > 0 {
		drained = make(chan struct{})
		s.drained = drained
	}
	s.mu.Unlock()

	resume := func(index int, result string, err error) {
		s.mu.Lock()
		if index != from {
			s.active = index
		}
		s.activeSince = s.now()
		s.swapping = false
		s.drained = nil
		record.Result = result
		if err != nil {
			record.Error = err.Error()
		}
		record.SwapSeconds = s.now().Sub(start).Seconds()
		if result != SwapDone {
			s.retryAfter = s.now().Add(s.cfg.Timing.MinDwell.D())
		}
		s.recordLocked(record)
		s.dispatchLocked()
		s.mu.Unlock()
		level := slog.LevelInfo
		if result != SwapDone {
			level = slog.LevelWarn
		}
		s.log.Log(ctx, level, "speculation switch", "from", record.From, "to", record.To, "result", result, "load", load,
			"drain_seconds", record.DrainSeconds, "sleep_seconds", record.SleepSeconds, "wake_seconds", record.WakeSeconds,
			"wake_weights_seconds", record.WakeWeightsSeconds, "wake_cache_seconds", record.WakeCacheSeconds,
			"first_token_seconds", record.FirstTokenSeconds, "swap_seconds", record.SwapSeconds, "error", record.Error)
	}

	if drained != nil {
		timer := time.NewTimer(s.cfg.Timing.DrainTimeout.D())
		select {
		case <-drained:
			timer.Stop()
		case <-timer.C:
			resume(from, SwapDrainTimeout, errors.New("requests in flight did not finish in time"))
			return
		case <-ctx.Done():
			timer.Stop()
			resume(from, SwapDrainTimeout, ctx.Err())
			return
		}
	}
	record.DrainSeconds = s.now().Sub(start).Seconds()

	stepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Timing.WakeTimeout.D())
	defer cancel()
	engines := s.cfg.Engines
	step := s.now()
	if err := s.ctl.Sleep(stepCtx, engines[from]); err != nil {
		s.fallBack(stepCtx, from, -1, SwapSleepFailed, err, resume)
		return
	}
	record.SleepSeconds = s.now().Sub(step).Seconds()
	step = s.now()
	var err error
	if parted, ok := s.ctl.(partedWaker); ok {
		var weights, cache time.Duration
		weights, cache, err = parted.WakeInParts(stepCtx, engines[target])
		record.WakeWeightsSeconds, record.WakeCacheSeconds = weights.Seconds(), cache.Seconds()
	} else {
		err = s.ctl.Wake(stepCtx, engines[target])
	}
	record.WakeSeconds = s.now().Sub(step).Seconds()
	if err == nil {
		step = s.now()
		err = s.ctl.Ready(stepCtx, engines[target])
		record.FirstTokenSeconds = s.now().Sub(step).Seconds()
	}
	if err != nil {
		s.fallBack(stepCtx, from, target, SwapWakeFailed, err, resume)
		return
	}
	resume(target, SwapDone, nil)
}

// fallBack puts target back to sleep, if it was woken, and wakes from again.
func (s *Switch) fallBack(ctx context.Context, from, target int, result string, cause error, resume func(int, string, error)) {
	if target >= 0 {
		if err := s.ctl.Sleep(ctx, s.cfg.Engines[target]); err != nil {
			cause = errors.Join(cause, err)
		}
	}
	err := s.ctl.Wake(ctx, s.cfg.Engines[from])
	if err == nil {
		err = s.ctl.Ready(ctx, s.cfg.Engines[from])
	}
	if err != nil {
		s.mu.Lock()
		s.failed = errors.Join(cause, err)
		s.mu.Unlock()
		resume(from, SwapFatal, errors.Join(cause, err))
		return
	}
	resume(from, result, cause)
}

func (s *Switch) recordLocked(record SwapRecord) {
	s.swaps = append(s.swaps, record)
	if len(s.swaps) > maxSwapRecords {
		s.swaps = s.swaps[len(s.swaps)-maxSwapRecords:]
	}
	s.counts[[3]string{record.From, record.To, record.Result}]++
	if record.Result == SwapDone {
		s.swapSeconds += record.SwapSeconds
		s.swapCount++
	}
}

// State is what the switch reports about itself.
type State struct {
	Policy    string       `json:"policy"`
	Awake     string       `json:"awake"`
	Drafts    int          `json:"drafts"`
	AwakeFor  float64      `json:"awake_seconds"`
	InFlight  int          `json:"in_flight"`
	Queued    int          `json:"queued"`
	Swapping  bool         `json:"swapping"`
	Healthy   bool         `json:"healthy"`
	Error     string       `json:"error,omitempty"`
	Rejected  int          `json:"rejected"`
	Swaps     []SwapRecord `json:"swaps"`
	Generated time.Time    `json:"generated"`
}

// State returns a copy of the switch's state.
func (s *Switch) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	state := State{Policy: s.policy, Awake: s.cfg.Engines[s.active].Name, Drafts: s.cfg.Engines[s.active].Drafts, AwakeFor: now.Sub(s.activeSince).Seconds(),
		InFlight: s.inflight, Queued: len(s.queue), Swapping: s.swapping, Healthy: s.failed == nil, Rejected: s.rejected,
		Swaps: append([]SwapRecord{}, s.swaps...), Generated: now}
	if s.failed != nil {
		state.Error = s.failed.Error()
	}
	return state
}

// Handler serves inference requests, /health, /metrics and /v1/models. It
// never forwards an engine's control routes (sleep, wake, development
// routes): only /v1/ and /version reach an engine.
func (s *Switch) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.serveHealth)
	mux.HandleFunc("GET /metrics", s.serveMetrics)
	mux.HandleFunc("GET /version", s.servePassThrough)
	mux.HandleFunc("GET /v1/models", s.servePassThrough)
	mux.HandleFunc("/v1/", s.serveInference)
	return mux
}

// AdminHandler serves the switch's state and policy routes; it belongs on a
// loopback listener only.
func (s *Switch) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /switch/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.State())
	})
	mux.HandleFunc("POST /switch/policy", func(w http.ResponseWriter, r *http.Request) {
		if err := s.SetPolicy(r.URL.Query().Get("name")); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_policy", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.State())
	})
	return mux
}

func (s *Switch) serveHealth(w http.ResponseWriter, _ *http.Request) {
	state := s.State()
	w.Header().Set("Content-Type", "application/json")
	if !state.Healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"healthy": state.Healthy, "awake": state.Awake, "error": state.Error})
}

func (s *Switch) awakeProxy() *httputil.ReverseProxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxies[s.active]
}

// servePassThrough answers a read that needs no room from the awake engine.
// Every engine serves the same model, so a sleeping one would answer alike.
func (s *Switch) servePassThrough(w http.ResponseWriter, r *http.Request) {
	s.awakeProxy().ServeHTTP(w, r)
}

func (s *Switch) serveInference(w http.ResponseWriter, r *http.Request) {
	// The body is read before the request waits: the server notices a caller
	// who leaves only once the body has been read, and a request whose caller
	// left must not keep its place in the queue.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "the request body is too large or could not be read")
		return
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	index, err := s.acquire(r.Context(), len(body))
	switch {
	case errors.Is(err, errQueueFull):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "server_overloaded", "too many requests are waiting for room on this server")
		return
	case errors.Is(err, errUnavailable):
		writeError(w, http.StatusServiceUnavailable, "upstream_unavailable", "no engine on this server can serve")
		return
	case err != nil:
		// The caller left while waiting; nothing reached it.
		return
	}
	defer s.release()
	s.proxies[index].ServeHTTP(w, r)
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": kind, "code": status}})
}
