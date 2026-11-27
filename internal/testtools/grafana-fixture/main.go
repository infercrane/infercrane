// Command grafana-fixture serves sample series for previewing the InferCrane
// Grafana dashboard without a GPU, a database, or a control plane. It is a
// development fake: every value it serves is invented, and names start with
// "demo-" so a screenshot cannot pass for a measurement.
//
//	go run ./internal/testtools/grafana-fixture -listen 127.0.0.1:19480
//	docker compose -f internal/testtools/grafana-fixture/compose.yaml up
//
// /metrics is a real gateway Telemetry fed by simulated requests,
// /engine/metrics is a vLLM-shaped exposition, and /api/v1/metrics is the
// results exposition written by internal/resultmetrics from a scripted
// timeline that cycles every eight minutes. /api/v1/metrics needs the bearer
// token "fixture-key", like the authenticated control-plane route.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"math/rand/v2"
	"net/http"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/infercrane/infercrane/internal/gateway"
	"github.com/infercrane/infercrane/internal/resultmetrics"
)

const fixtureKey = "fixture-key"

func main() {
	listen := flag.String("listen", "127.0.0.1:19480", "address to serve on")
	phase := flag.Duration("phase", time.Minute, "length of one step of the scripted results timeline")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	f := newFixture(time.Now(), *phase)
	go f.simulate(ctx)
	server := &http.Server{Addr: *listen, Handler: f.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	log.Printf("grafana fixture on http://%s (sample data only)", *listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type fixture struct {
	started  time.Time
	phase    time.Duration
	gateway  *gateway.Telemetry
	mu       sync.Mutex
	engines  map[string]*engine
	random   *rand.Rand
	accepted uint64
}

func newFixture(started time.Time, phase time.Duration) *fixture {
	f := &fixture{started: started, phase: phase, random: rand.New(rand.NewPCG(7, 11)), engines: map[string]*engine{
		"Qwen/Qwen3-8B": newEngine(0.09, 0.022),
		"deepseek-ai/DeepSeek-R1-Distill-Qwen-7B": newEngine(0.14, 0.028),
	}}
	f.gateway = &gateway.Telemetry{Extra: f.writeGatewayExtra}
	return f
}

func (f *fixture) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", f.gateway)
	mux.HandleFunc("GET /engine/metrics", f.serveEngine)
	mux.HandleFunc("GET /api/v1/metrics", f.serveResults)
	return mux
}

// simulate sends a varying stream of fake requests through the gateway's
// real Telemetry and advances the engine counters.
func (f *fixture) simulate(ctx context.Context) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			elapsed := now.Sub(f.started).Seconds()
			// Two to six requests per tick, rising and falling over ten minutes.
			load := 4 + 2*sine(elapsed/600)
			f.mu.Lock()
			count := int(load + f.random.Float64())
			durations := make([]time.Duration, count)
			statuses := make([]int, count)
			for i := range count {
				durations[i] = time.Duration((0.15 + f.random.ExpFloat64()*0.6) * float64(time.Second))
				statuses[i] = http.StatusOK
				if f.random.Float64() < 0.004+0.01*max(0, sine(elapsed/300)) {
					statuses[i] = http.StatusBadGateway
				}
			}
			for name, e := range f.engines {
				share := 0.6
				if strings.HasPrefix(name, "deepseek") {
					share = 0.4
				}
				e.advance(f.random, load*share, elapsed)
			}
			f.mu.Unlock()
			for i := range count {
				go f.request(ctx, durations[i], statuses[i])
			}
		}
	}
}

func (f *fixture) request(ctx context.Context, duration time.Duration, status int) {
	handler := f.gateway.Observe(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(duration):
		case <-r.Context().Done():
		}
		w.WriteHeader(status)
	})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", nil)
	handler(discard{header: http.Header{}}, request)
	f.mu.Lock()
	f.accepted++
	f.mu.Unlock()
}

type discard struct{ header http.Header }

func (d discard) Header() http.Header         { return d.header }
func (d discard) Write(b []byte) (int, error) { return len(b), nil }
func (d discard) WriteHeader(int)             {}

func (f *fixture) writeGatewayExtra(w io.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprintf(w, "# TYPE infercrane_accounting_records_persisted_total counter\ninfercrane_accounting_records_persisted_total %d\n", f.accepted)
	fmt.Fprintf(w, "# TYPE infercrane_accounting_records_dropped_total counter\ninfercrane_accounting_records_dropped_total 0\n")
	fmt.Fprintf(w, "# TYPE infercrane_admission_waiting gauge\ninfercrane_admission_waiting %d\n", int(max(0, 3*sine(time.Since(f.started).Seconds()/300))))
	fmt.Fprintf(w, "# TYPE infercrane_admission_rejected_total counter\ninfercrane_admission_rejected_total 0\n")
}

// engine is a vLLM-shaped set of counters and histograms for one model.
type engine struct {
	ttftBase, itlBase              float64
	running, waiting, kv           float64
	prompt, generated, preemptions float64
	prefixQueries, prefixHits      float64
	drafts, accepted               float64
	ttft, itl, queue               histogram
}

type histogram struct {
	counts []float64
	sum    float64
	count  float64
}

var bounds = []float64{0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.5, 0.75, 1, 2.5, 5}

func newEngine(ttft, itl float64) *engine {
	e := &engine{ttftBase: ttft, itlBase: itl}
	for _, h := range []*histogram{&e.ttft, &e.itl, &e.queue} {
		h.counts = make([]float64, len(bounds))
	}
	return e
}

func (h *histogram) observe(value float64) {
	for i, bound := range bounds {
		if value <= bound {
			h.counts[i]++
		}
	}
	h.sum += value
	h.count++
}

func (e *engine) advance(random *rand.Rand, load, elapsed float64) {
	e.running = load * 4
	e.waiting = max(0, load*2*sine(elapsed/300)-1)
	e.kv = min(0.97, 0.35+0.1*load+0.1*sine(elapsed/240))
	requests := int(load*0.4 + random.Float64())
	for range requests {
		input, output := 600+random.Float64()*1400, 120+random.Float64()*380
		e.prompt += input
		e.generated += output
		e.prefixQueries += input
		e.prefixHits += input * (0.45 + 0.2*sine(elapsed/420))
		e.drafts += output * 0.6
		e.accepted += output * 0.6 * (0.62 + 0.08*sine(elapsed/360))
		e.ttft.observe(e.ttftBase * (1 + 0.6*random.ExpFloat64()) * (1 + e.waiting/10))
		e.itl.observe(e.itlBase * (1 + 0.3*random.ExpFloat64()))
		e.queue.observe(0.005 * (1 + e.waiting) * random.ExpFloat64())
	}
	if e.kv > 0.9 && random.Float64() < 0.2 {
		e.preemptions++
	}
}

func (f *fixture) serveEngine(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder
	families := []struct {
		name, kind string
		value      func(*engine) float64
	}{
		{"vllm:num_requests_running", "gauge", func(e *engine) float64 { return e.running }},
		{"vllm:num_requests_waiting", "gauge", func(e *engine) float64 { return e.waiting }},
		{"vllm:kv_cache_usage_perc", "gauge", func(e *engine) float64 { return e.kv }},
		{"vllm:prompt_tokens_total", "counter", func(e *engine) float64 { return e.prompt }},
		{"vllm:generation_tokens_total", "counter", func(e *engine) float64 { return e.generated }},
		{"vllm:num_preemptions_total", "counter", func(e *engine) float64 { return e.preemptions }},
		{"vllm:prefix_cache_queries_total", "counter", func(e *engine) float64 { return e.prefixQueries }},
		{"vllm:prefix_cache_hits_total", "counter", func(e *engine) float64 { return e.prefixHits }},
		{"vllm:spec_decode_num_draft_tokens_total", "counter", func(e *engine) float64 { return e.drafts }},
		{"vllm:spec_decode_num_accepted_tokens_total", "counter", func(e *engine) float64 { return e.accepted }},
	}
	for _, family := range families {
		fmt.Fprintf(&b, "# TYPE %s %s\n", family.name, family.kind)
		for _, name := range sortedModels(f.engines) {
			fmt.Fprintf(&b, "%s{model_name=%q} %g\n", family.name, name, family.value(f.engines[name]))
		}
	}
	for _, h := range []struct {
		name string
		get  func(*engine) *histogram
	}{
		{"vllm:time_to_first_token_seconds", func(e *engine) *histogram { return &e.ttft }},
		{"vllm:inter_token_latency_seconds", func(e *engine) *histogram { return &e.itl }},
		{"vllm:request_queue_time_seconds", func(e *engine) *histogram { return &e.queue }},
	} {
		fmt.Fprintf(&b, "# TYPE %s histogram\n", h.name)
		for _, name := range sortedModels(f.engines) {
			series := h.get(f.engines[name])
			for i, bound := range bounds {
				fmt.Fprintf(&b, "%s_bucket{model_name=%q,le=\"%g\"} %g\n", h.name, name, bound, series.counts[i])
			}
			fmt.Fprintf(&b, "%s_bucket{model_name=%q,le=\"+Inf\"} %g\n%s_sum{model_name=%q} %g\n%s_count{model_name=%q} %g\n", h.name, name, series.count, h.name, name, series.sum, h.name, name, series.count)
		}
	}
	_, _ = io.WriteString(w, b.String())
}

func (f *fixture) serveResults(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+fixtureKey {
		http.Error(w, "valid bearer authentication is required", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = resultmetrics.WritePrometheus(w, f.results(time.Now()))
}

// results is the scripted timeline: eight steps per cycle. Counters carry
// over from earlier cycles so increase() sees each new record.
func (f *fixture) results(now time.Time) resultmetrics.Snapshot {
	steps := int(now.Sub(f.started) / f.phase)
	cycle, step := steps/8, steps%8
	at := func(s int) time.Time { return f.started.Add(time.Duration(cycle*8+s) * f.phase) }
	verdict := func(v string) *string { return &v }
	cost := func(v float64) *float64 { return &v }

	// demo-qwen3-chat: a candidate waits for evidence, passes, is promoted,
	// a second candidate passes on WATCH, a third is blocked on quality.
	qwen := resultmetrics.Deployment{Name: "demo-qwen3-chat",
		GuardDecisions:  map[string]int{"ACCEPT": cycle * 2, "WAIT": cycle, "REJECT": cycle},
		QualityVerdicts: map[string]int{"PASS": cycle, "WATCH": cycle, "BLOCK": cycle, "UNKNOWN": cycle + 1},
		Rollbacks:       resultmetrics.Counts(resultmetrics.RollbackTriggers)}
	switch {
	case step < 2:
		qwen.LatestGuard = &resultmetrics.GuardResult{Decision: "WAIT", QualityVerdict: verdict("UNKNOWN"), EvaluatedAt: at(0)}
		qwen.GuardDecisions["WAIT"]++
	case step < 4:
		qwen.LatestGuard = &resultmetrics.GuardResult{Decision: "ACCEPT", QualityVerdict: verdict("PASS"), EvaluatedAt: at(2)}
		qwen.GuardDecisions["WAIT"]++
		qwen.GuardDecisions["ACCEPT"]++
		qwen.QualityVerdicts["PASS"]++
		qwen.Observation = &resultmetrics.Observation{State: "observing", UpdatedAt: at(2)}
		if step == 3 {
			qwen.Observation = &resultmetrics.Observation{State: "accepted", UpdatedAt: at(3)}
		}
	case step < 6:
		qwen.LatestGuard = &resultmetrics.GuardResult{Decision: "ACCEPT", QualityVerdict: verdict("WATCH"), EvaluatedAt: at(4)}
		qwen.GuardDecisions["WAIT"]++
		qwen.GuardDecisions["ACCEPT"] += 2
		qwen.QualityVerdicts["PASS"]++
		qwen.QualityVerdicts["WATCH"]++
		qwen.Observation = &resultmetrics.Observation{State: "accepted", UpdatedAt: at(3)}
	default:
		qwen.LatestGuard = &resultmetrics.GuardResult{Decision: "REJECT", QualityVerdict: verdict("BLOCK"), EvaluatedAt: at(6)}
		qwen.GuardDecisions["WAIT"]++
		qwen.GuardDecisions["ACCEPT"] += 2
		qwen.GuardDecisions["REJECT"]++
		qwen.QualityVerdicts["PASS"]++
		qwen.QualityVerdicts["WATCH"]++
		qwen.QualityVerdicts["BLOCK"]++
		qwen.Observation = &resultmetrics.Observation{State: "accepted", UpdatedAt: at(3)}
	}

	// demo-glm-coder: a promoted revision regresses and rolls back.
	glm := resultmetrics.Deployment{Name: "demo-glm-coder",
		GuardDecisions:  map[string]int{"ACCEPT": cycle + 1},
		QualityVerdicts: map[string]int{"PASS": cycle + 1},
		Rollbacks:       map[string]int{"automatic": cycle},
		LatestGuard:     &resultmetrics.GuardResult{Decision: "ACCEPT", QualityVerdict: verdict("PASS"), EvaluatedAt: at(0)}}
	glm.Observation = &resultmetrics.Observation{State: "observing", UpdatedAt: at(1)}
	if step >= 5 {
		last := at(5)
		glm.Observation = &resultmetrics.Observation{State: "rolled_back", UpdatedAt: last}
		glm.Rollbacks["automatic"]++
		glm.LastRollbackAt = &last
	} else if cycle > 0 {
		last := at(-3)
		glm.LastRollbackAt = &last
	}

	// demo-support-rag: never evaluated, so every gauge is unknown.
	rag := resultmetrics.Deployment{Name: "demo-support-rag", GuardDecisions: resultmetrics.Counts(resultmetrics.GuardDecisions), QualityVerdicts: resultmetrics.Counts(resultmetrics.QualityVerdicts), Rollbacks: resultmetrics.Counts(resultmetrics.RollbackTriggers)}

	// demo-support-chat: a staged switch from a closed API. A 5% canary is
	// admitted, the owned deployment becomes primary with the closed API as
	// fallback, the endpoint guard rejects the next candidate plan, and the
	// endpoint rolls back to the closed API.
	chat := resultmetrics.Endpoint{Name: "demo-support-chat", GuardDecisions: map[string]int{"PASS": cycle * 3, "REJECT": cycle}}
	closed := resultmetrics.PlanBinding{Name: "closed-api", Kind: "external", Priority: 0, Weight: 95}
	owned := resultmetrics.PlanBinding{Name: "owned-qwen3", Kind: "deployment", Priority: 1, Weight: 5}
	policy, plan := "weighted", []resultmetrics.PlanBinding{closed, owned}
	switch {
	case step < 2:
		chat.GuardDecisions["PASS"]++
		chat.LatestGuard = &resultmetrics.EndpointGuardResult{Decision: "PASS", EvaluatedAt: at(0)}
		chat.CandidateStaged = step == 1
	case step < 4:
		owned.Priority, closed.Priority = 0, 1
		policy, plan = "primary-fallback", []resultmetrics.PlanBinding{closed, owned}
		chat.GuardDecisions["PASS"] += 2
		chat.LatestGuard = &resultmetrics.EndpointGuardResult{Decision: "PASS", EvaluatedAt: at(2)}
	case step < 6:
		owned.Priority, closed.Priority = 0, 1
		policy, plan = "primary-fallback", []resultmetrics.PlanBinding{closed, owned}
		chat.GuardDecisions["PASS"] += 2
		chat.GuardDecisions["REJECT"]++
		chat.LatestGuard = &resultmetrics.EndpointGuardResult{Decision: "REJECT", EvaluatedAt: at(4)}
		chat.CandidateStaged = true
	default:
		policy, plan = "manual", []resultmetrics.PlanBinding{closed}
		chat.GuardDecisions["PASS"] += 3
		chat.GuardDecisions["REJECT"]++
		chat.LatestGuard = &resultmetrics.EndpointGuardResult{Decision: "PASS", EvaluatedAt: at(6)}
	}
	chat.RoutingPolicy, chat.Bindings = policy, resultmetrics.Shares(policy, plan)
	single := resultmetrics.Endpoint{Name: "demo-qwen3-chat", RoutingPolicy: "manual", Bindings: resultmetrics.Shares("manual", []resultmetrics.PlanBinding{{Name: "primary", Kind: "deployment", Weight: 100}}), GuardDecisions: resultmetrics.Counts(resultmetrics.EndpointGuardDecisions)}

	// Optimization runs: one finds a cheaper setup, one keeps the current
	// setup, one is still running with unknown cost.
	runState, outcome := "running", "in_progress"
	var chosen *float64
	if step >= 3 {
		runState, outcome, chosen = "promoted", "winner_selected", cost(0.31)
	}
	campaigns := []resultmetrics.Campaign{
		{Endpoint: "demo-qwen3-chat", ID: "demo-1", State: runState, UpdatedAt: at(min(step, 3)), Outcome: verdict(outcome), BaselineCostPerMillionOutputTokensUSD: cost(0.42), ChosenCostPerMillionOutputTokensUSD: chosen},
		{Endpoint: "demo-glm-coder", ID: "demo-2", State: "cleaned", UpdatedAt: at(0), Outcome: verdict("baseline_kept"), BaselineCostPerMillionOutputTokensUSD: cost(1.1), ChosenCostPerMillionOutputTokensUSD: cost(1.1)},
		{Endpoint: "demo-support-chat", ID: "demo-3", State: "running", UpdatedAt: at(0), Outcome: verdict("in_progress")},
	}
	states := resultmetrics.Counts(resultmetrics.CampaignStates)
	for _, c := range campaigns {
		states[c.State]++
	}
	states["cleaned"] += cycle * 2
	return resultmetrics.Snapshot{SchemaVersion: resultmetrics.SchemaVersion, GeneratedAt: now.UTC(),
		Deployments: []resultmetrics.Deployment{qwen, glm, rag}, Endpoints: []resultmetrics.Endpoint{chat, single},
		Campaigns: campaigns, CampaignStates: states}
}

func sortedModels(engines map[string]*engine) []string {
	return slices.Sorted(maps.Keys(engines))
}

func sine(x float64) float64 {
	// A cheap smooth wave in [-1, 1] with period 1.
	x -= float64(int(x))
	if x < 0.5 {
		return 1 - 16*(x-0.25)*(x-0.25)
	}
	return -1 + 16*(x-0.75)*(x-0.75)
}
