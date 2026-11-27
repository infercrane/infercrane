package specswitch

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEngine stands in for one vLLM engine with sleep mode: it answers the
// sleep routes, runs at most room requests at once (the rest wait inside it),
// and takes per request the time its speed at the present batch gives.
type fakeEngine struct {
	t      *testing.T
	name   string
	drafts int
	room   int
	// speed returns tokens per second for one request with batch running.
	speed  func(batch int) float64
	tokens int
	// scale divides every wait, to run a replay faster than real time.
	scale float64

	server *httptest.Server

	mu     sync.Mutex
	asleep bool
	// weightsAsleep and cacheAsleep are the two parts vLLM wakes apart.
	weightsAsleep, cacheAsleep bool
	running                    int
	waiting                    int
	slot                       *sync.Cond
	maxSeen                    int
	served                     int
	tokensW                    int
	refusals                   int
	// sleepErr and wakeErr make the next sleep or wake fail.
	sleepErr, wakeErr error
	sleeps, wakes     int
	release           chan struct{}
	// swapDelay is how long a sleep or a wake takes.
	swapDelay time.Duration
	block     atomic.Bool
	sawAsleep atomic.Int64
}

func newFakeEngine(t *testing.T, name string, drafts, room int, speed func(int) float64) *fakeEngine {
	e := &fakeEngine{t: t, name: name, drafts: drafts, room: room, speed: speed, tokens: 32, scale: 1, release: make(chan struct{})}
	e.slot = sync.NewCond(&e.mu)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "fake/model"}}})
	})
	mux.HandleFunc("GET /is_sleeping", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"is_sleeping": e.asleep})
	})
	mux.HandleFunc("POST /sleep", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(e.swapDelay)
		e.mu.Lock()
		defer e.mu.Unlock()
		if r.URL.Query().Get("level") != "1" || r.URL.Query().Get("mode") != "wait" {
			e.t.Errorf("%s: sleep asked with %s, want level=1&mode=wait", e.name, r.URL.RawQuery)
		}
		e.sleeps++
		if e.sleepErr != nil {
			err := e.sleepErr
			e.sleepErr = nil
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if e.running > 0 || e.waiting > 0 {
			e.t.Errorf("%s: put to sleep with %d running and %d waiting", e.name, e.running, e.waiting)
		}
		e.asleep, e.weightsAsleep, e.cacheAsleep = true, true, true
	})
	mux.HandleFunc("POST /wake_up", func(w http.ResponseWriter, r *http.Request) {
		// Like vLLM: tags=weights wakes the weights (and every part that is
		// not deferrable), tags=kv_cache the cache; no tags wakes both. The
		// engine is awake once both are; each part takes half the delay.
		tags := r.URL.Query()["tags"]
		parts := map[string]bool{"weights": len(tags) == 0, "kv_cache": len(tags) == 0}
		for _, tag := range tags {
			parts[tag] = true
		}
		delay := e.swapDelay
		if len(tags) > 0 {
			delay /= 2
		}
		time.Sleep(delay)
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.wakeErr != nil {
			err := e.wakeErr
			e.wakeErr = nil
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if parts["weights"] {
			e.weightsAsleep = false
			e.wakes++
		}
		if parts["kv_cache"] {
			e.cacheAsleep = false
		}
		e.asleep = e.weightsAsleep || e.cacheAsleep
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		fmt.Fprintf(w, "vllm:num_requests_running %d\nvllm:num_requests_waiting %d\n", e.running, e.waiting)
	})
	serve := func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		if e.asleep {
			e.sawAsleep.Add(1)
			e.mu.Unlock()
			http.Error(w, "asleep", http.StatusServiceUnavailable)
			return
		}
		e.waiting++
		for e.room > 0 && e.running >= e.room {
			e.slot.Wait()
		}
		e.waiting--
		e.running++
		e.maxSeen = max(e.maxSeen, e.running)
		batch := e.running
		started := time.Now()
		e.mu.Unlock()
		defer func() {
			e.mu.Lock()
			e.running--
			e.served++
			e.tokensW += e.tokens
			e.slot.Signal()
			e.mu.Unlock()
		}()
		if e.block.Load() {
			<-e.release
		}
		tokens := e.tokens
		if limit, ok := body["max_tokens"].(float64); ok && int(limit) < tokens {
			tokens = int(limit)
		}
		if e.speed != nil {
			time.Sleep(time.Duration(float64(tokens) / e.speed(batch) / e.scale * float64(time.Second)))
		}
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			for index := range 3 {
				fmt.Fprintf(w, "data: {\"index\":%d}\n\n", index)
				flusher.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		// started is when the engine began the request: everything before it,
		// in the switch's queue or the engine's, is time to first token.
		_ = json.NewEncoder(w).Encode(map[string]any{"engine": e.name, "started_unix_nano": started.UnixNano(), "usage": map[string]any{"completion_tokens": tokens}})
	}
	mux.HandleFunc("POST /v1/completions", serve)
	mux.HandleFunc("POST /v1/chat/completions", serve)
	mux.HandleFunc("POST /collective_rpc", func(http.ResponseWriter, *http.Request) { e.t.Errorf("%s: a development route was reached", e.name) })
	e.server = httptest.NewServer(mux)
	t.Cleanup(e.server.Close)
	return e
}

func (e *fakeEngine) engine() Engine {
	return Engine{Name: e.name, Drafts: e.drafts, URL: e.server.URL, Room: e.room}
}

func (e *fakeEngine) setAsleep(asleep bool) {
	e.mu.Lock()
	e.asleep, e.weightsAsleep, e.cacheAsleep = asleep, asleep, asleep
	e.mu.Unlock()
}

func (e *fakeEngine) isAsleep() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.asleep
}

func (e *fakeEngine) stats() (served, tokens, maxSeen int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.served, e.tokensW, e.maxSeen
}

// mapSpeed is tokens per second for one short-chat request with batch running,
// interpolated from the H100 map's medians (Qwen3.8-27B-FP8, vLLM 0.34.0):
// output per GPU at each load divided by the requests the engine ran.
func mapSpeed(drafts int) func(int) float64 {
	// Points are (requests running, output tokens per second per GPU).
	points := map[int][][2]float64{
		0: {{4, 318}, {32, 1821}, {47, 2199}, {63, 2806}, {95, 3585}, {127, 4081}},
		1: {{4, 488}, {31, 2580}, {47, 3140}, {63, 3727}, {95, 4407}, {111, 4597}},
		3: {{4, 608}, {31, 2870}, {47, 3310}, {58, 3610}},
	}[drafts]
	return func(batch int) float64 {
		b := float64(max(batch, 1))
		if b <= points[0][0] {
			return points[0][1] / points[0][0]
		}
		for index := 1; index < len(points); index++ {
			low, high := points[index-1], points[index]
			if b <= high[0] {
				total := low[1] + (high[1]-low[1])*(b-low[0])/(high[0]-low[0])
				return total / b
			}
		}
		last := points[len(points)-1]
		return last[1] / math.Max(b, last[0])
	}
}

// controllerFor is the VLLM controller with a fast poll for fake engines.
func controllerFor() VLLM {
	return VLLM{Client: &http.Client{Timeout: 10 * time.Second}, Poll: 2 * time.Millisecond}
}
