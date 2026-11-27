package specswitch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func fastTiming() Timing {
	return Timing{Sample: Duration(5 * time.Millisecond), Smooth: Duration(20 * time.Millisecond), RaiseAfter: Duration(30 * time.Millisecond),
		LowerAfter: Duration(150 * time.Millisecond), MinDwell: Duration(50 * time.Millisecond), DrainTimeout: Duration(2 * time.Second), WakeTimeout: Duration(5 * time.Second)}
}

type rig struct {
	sw      *Switch
	server  *httptest.Server
	engines []*fakeEngine
	cancel  context.CancelFunc
	done    chan struct{}
}

// newRig starts a switch over the given fake engines; the first band's engine
// is awake and the others asleep, as the command leaves them.
func newRig(t *testing.T, engines []*fakeEngine, policies map[string][]Band, policy string, timing Timing, queue int) *rig {
	t.Helper()
	cfg := Config{Listen: ":0", Policies: policies, Policy: policy, Timing: timing, QueueLimit: queue}
	for _, engine := range engines {
		cfg.Engines = append(cfg.Engines, engine.engine())
	}
	for _, engine := range engines {
		engine.setAsleep(engine.name != InitialEngine(cfg))
	}
	sw, err := New(cfg, controllerFor())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{sw: sw, server: httptest.NewServer(sw.Handler()), engines: engines, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		sw.Run(ctx)
	}()
	t.Cleanup(func() {
		r.cancel()
		<-r.done
		r.server.Close()
	})
	return r
}

func (r *rig) complete(t *testing.T, ctx context.Context) (int, string) {
	t.Helper()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.server.URL+"/v1/completions", strings.NewReader(`{"model":"fake/model","prompt":"hi","max_tokens":8}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSwitchQueuesRequestsBeyondTheAwakeEnginesRoom(t *testing.T) {
	three := newFakeEngine(t, "drafts-3", 3, 2, nil)
	one := newFakeEngine(t, "drafts-1", 1, 4, nil)
	three.block.Store(true)
	timing := fastTiming()
	timing.RaiseAfter, timing.LowerAfter = Duration(time.Hour), Duration(time.Hour)
	r := newRig(t, []*fakeEngine{three, one}, map[string][]Band{"p": {{UpTo: 100, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, "p", timing, 10)

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if status, body := r.complete(t, context.Background()); status != http.StatusOK {
				t.Errorf("request failed: %d %s", status, body)
			}
		})
	}
	waitFor(t, "two in flight and three queued", func() bool { s := r.sw.State(); return s.InFlight == 2 && s.Queued == 3 })
	waitFor(t, "two running in the engine", func() bool {
		three.mu.Lock()
		defer three.mu.Unlock()
		return three.running == 2
	})
	close(three.release)
	wg.Wait()
	if _, _, maxSeen := three.stats(); maxSeen != 2 {
		t.Fatalf("the engine ran %d at once, want its room of 2", maxSeen)
	}
}

func TestSwitchRefusesWhenTheQueueIsFull(t *testing.T) {
	three := newFakeEngine(t, "drafts-3", 3, 1, nil)
	one := newFakeEngine(t, "drafts-1", 1, 1, nil)
	three.block.Store(true)
	timing := fastTiming()
	timing.RaiseAfter, timing.LowerAfter = Duration(time.Hour), Duration(time.Hour)
	r := newRig(t, []*fakeEngine{three, one}, map[string][]Band{"p": {{UpTo: 100, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, "p", timing, 1)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { r.complete(t, context.Background()) })
	}
	waitFor(t, "one in flight and one queued", func() bool { s := r.sw.State(); return s.InFlight == 1 && s.Queued == 1 })
	status, body := r.complete(t, context.Background())
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "server_overloaded") {
		t.Fatalf("got %d %s, want 503 server_overloaded", status, body)
	}
	close(three.release)
	wg.Wait()
}

func TestSwitchForgetsARequestWhoseCallerLeftWhileQueued(t *testing.T) {
	three := newFakeEngine(t, "drafts-3", 3, 1, nil)
	one := newFakeEngine(t, "drafts-1", 1, 1, nil)
	three.block.Store(true)
	timing := fastTiming()
	timing.RaiseAfter, timing.LowerAfter = Duration(time.Hour), Duration(time.Hour)
	r := newRig(t, []*fakeEngine{three, one}, map[string][]Band{"p": {{UpTo: 100, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, "p", timing, 4)
	go r.complete(t, context.Background())
	waitFor(t, "one in flight", func() bool { return r.sw.State().InFlight == 1 })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.complete(t, ctx) }()
	waitFor(t, "one queued", func() bool { return r.sw.State().Queued == 1 })
	cancel()
	<-done
	waitFor(t, "the queue to empty", func() bool { return r.sw.State().Queued == 0 })
	close(three.release)
	waitFor(t, "nothing in flight", func() bool { return r.sw.State().InFlight == 0 })
	if served, _, _ := three.stats(); served != 1 {
		t.Fatalf("the engine served %d requests, want only the one whose caller stayed", served)
	}
}

func TestSwitchSwapsAfterDrainingAndNeverSendsToASleepingEngine(t *testing.T) {
	speed := func(int) float64 { return 2000 }
	three := newFakeEngine(t, "drafts-3", 3, 4, speed)
	one := newFakeEngine(t, "drafts-1", 1, 16, speed)
	three.swapDelay, one.swapDelay = 20*time.Millisecond, 40*time.Millisecond
	r := newRig(t, []*fakeEngine{three, one}, map[string][]Band{"p": {{UpTo: 4, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, "p", fastTiming(), 64)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	failures := make(chan string, 1024)
	for range 12 {
		wg.Go(func() {
			for ctx.Err() == nil {
				if status, body := r.complete(t, ctx); status != http.StatusOK && ctx.Err() == nil {
					failures <- body
				}
			}
		})
	}
	waitFor(t, "drafts-1 awake", func() bool { return r.sw.State().Awake == "drafts-1" })
	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Errorf("request failed during the swap: %s", failure)
	}
	if !three.isAsleep() || one.isAsleep() {
		t.Fatalf("after the swap drafts-3 asleep=%t, drafts-1 asleep=%t", three.isAsleep(), one.isAsleep())
	}
	if three.sawAsleep.Load()+one.sawAsleep.Load() > 0 {
		t.Fatal("a request reached a sleeping engine")
	}
	swaps := r.sw.State().Swaps
	if len(swaps) != 1 || swaps[0].Result != SwapDone || swaps[0].From != "drafts-3" || swaps[0].To != "drafts-1" {
		t.Fatalf("swaps %+v, want one from drafts-3 to drafts-1", swaps)
	}
	if served, _, _ := one.stats(); served == 0 {
		t.Fatal("drafts-1 served nothing after the swap")
	}
	// Each part is timed: 20 ms to sleep drafts-3, 20 ms each for drafts-1's
	// weights and cache, and the parts fit inside the whole.
	swap := swaps[0]
	if swap.SleepSeconds < 0.02 || swap.WakeWeightsSeconds < 0.02 || swap.WakeCacheSeconds < 0.02 || swap.FirstTokenSeconds <= 0 ||
		swap.WakeSeconds < swap.WakeWeightsSeconds+swap.WakeCacheSeconds ||
		swap.DrainSeconds+swap.SleepSeconds+swap.WakeSeconds+swap.FirstTokenSeconds > swap.SwapSeconds+0.001 {
		t.Fatalf("swap parts %+v", swap)
	}
}

func TestSwitchCallsOffASwapWhoseDrainDoesNotEnd(t *testing.T) {
	three := newFakeEngine(t, "drafts-3", 3, 4, nil)
	one := newFakeEngine(t, "drafts-1", 1, 16, nil)
	three.block.Store(true)
	timing := fastTiming()
	timing.DrainTimeout = Duration(100 * time.Millisecond)
	timing.MinDwell = Duration(time.Hour)
	r := newRig(t, []*fakeEngine{three, one}, map[string][]Band{"p": {{UpTo: 2, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, "p", timing, 64)
	// The swap's dwell runs from the switch's start; let it pass once.
	r.sw.mu.Lock()
	r.sw.activeSince = time.Now().Add(-2 * time.Hour)
	r.sw.mu.Unlock()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { r.complete(t, context.Background()) })
	}
	waitFor(t, "a called-off swap", func() bool {
		swaps := r.sw.State().Swaps
		return len(swaps) == 1 && swaps[0].Result == SwapDrainTimeout
	})
	if r.sw.State().Awake != "drafts-3" || three.isAsleep() {
		t.Fatal("drafts-3 should still serve after the swap was called off")
	}
	close(three.release)
	wg.Wait()
}

func TestSwitchWakesItsOwnEngineAgainWhenTheOtherFailsToWake(t *testing.T) {
	speed := func(int) float64 { return 2000 }
	three := newFakeEngine(t, "drafts-3", 3, 4, speed)
	one := newFakeEngine(t, "drafts-1", 1, 16, speed)
	one.mu.Lock()
	one.wakeErr = errors.New("out of memory")
	one.mu.Unlock()
	timing := fastTiming()
	timing.MinDwell = Duration(time.Hour)
	r := newRig(t, []*fakeEngine{three, one}, map[string][]Band{"p": {{UpTo: 2, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, "p", timing, 64)
	r.sw.mu.Lock()
	r.sw.activeSince = time.Now().Add(-2 * time.Hour)
	r.sw.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			for ctx.Err() == nil {
				if status, body := r.complete(t, ctx); status != http.StatusOK && ctx.Err() == nil {
					t.Errorf("request failed: %d %s", status, body)
				}
			}
		})
	}
	waitFor(t, "a failed wake", func() bool {
		swaps := r.sw.State().Swaps
		return len(swaps) == 1 && swaps[0].Result == SwapWakeFailed
	})
	time.Sleep(30 * time.Millisecond)
	cancel()
	wg.Wait()
	state := r.sw.State()
	if state.Awake != "drafts-3" || !state.Healthy || three.isAsleep() || !one.isAsleep() {
		t.Fatalf("after a failed wake: %+v, drafts-3 asleep=%t, drafts-1 asleep=%t", state, three.isAsleep(), one.isAsleep())
	}
}

func TestSwitchReportsUnhealthyWhenNoEngineWakes(t *testing.T) {
	three := newFakeEngine(t, "drafts-3", 3, 4, nil)
	one := newFakeEngine(t, "drafts-1", 1, 16, nil)
	one.mu.Lock()
	one.wakeErr = errors.New("out of memory")
	one.mu.Unlock()
	three.mu.Lock()
	three.wakeErr = errors.New("host memory exhausted")
	three.mu.Unlock()
	timing := fastTiming()
	timing.MinDwell = Duration(time.Millisecond)
	r := newRig(t, []*fakeEngine{three, one}, map[string][]Band{"p": {{UpTo: 1, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, "p", timing, 64)
	three.block.Store(true)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() { r.complete(t, context.Background()) })
	}
	waitFor(t, "a swap to begin", func() bool { return r.sw.State().Swapping })
	close(three.release)
	wg.Wait()
	waitFor(t, "the switch to give up", func() bool { return !r.sw.State().Healthy })
	response, err := http.Get(r.server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/health answered %d, want 503", response.StatusCode)
	}
	if status, _ := r.complete(t, context.Background()); status != http.StatusServiceUnavailable {
		t.Fatalf("a request answered %d, want 503", status)
	}
}

func TestSwitchServesOnlyInferenceRoutesAndStreams(t *testing.T) {
	three := newFakeEngine(t, "drafts-3", 3, 4, nil)
	one := newFakeEngine(t, "drafts-1", 1, 4, nil)
	timing := fastTiming()
	timing.RaiseAfter, timing.LowerAfter = Duration(time.Hour), Duration(time.Hour)
	r := newRig(t, []*fakeEngine{three, one}, map[string][]Band{"p": {{UpTo: 8, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, "p", timing, 4)
	for _, path := range []string{"/sleep?level=1", "/wake_up", "/collective_rpc", "/is_sleeping"} {
		response, err := http.Post(r.server.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s reached the engine: %d", path, response.StatusCode)
		}
	}
	if three.isAsleep() {
		t.Fatal("a control route put the engine to sleep")
	}
	response, err := http.Post(r.server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !bytes.Contains(body, []byte("data: [DONE]")) || bytes.Count(body, []byte("data: ")) != 4 {
		t.Fatalf("stream did not pass through whole: %q", body)
	}
	metrics, err := http.Get(r.server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(metrics.Body)
	metrics.Body.Close()
	for _, want := range []string{`infercrane_specswitch_engine_awake{engine="drafts-3",drafts="3"} 1`, "infercrane_specswitch_requests_queued 0", "vllm:num_requests_running"} {
		if !bytes.Contains(text, []byte(want)) {
			t.Errorf("metrics lack %q:\n%s", want, text)
		}
	}
}

func TestSwitchPolicyChangeMovesToTheNewBands(t *testing.T) {
	speed := func(int) float64 { return 4000 }
	three := newFakeEngine(t, "drafts-3", 3, 4, speed)
	one := newFakeEngine(t, "drafts-1", 1, 8, speed)
	none := newFakeEngine(t, "drafts-0", 0, 0, speed)
	policies := map[string][]Band{
		"throughput":    {{UpTo: 4, Engine: "drafts-3"}, {Engine: "drafts-1"}},
		"useful-output": {{UpTo: 4, Engine: "drafts-3"}, {UpTo: 8, Engine: "drafts-1"}, {Engine: "drafts-0"}},
	}
	r := newRig(t, []*fakeEngine{three, one, none}, policies, "throughput", fastTiming(), 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			for ctx.Err() == nil {
				r.complete(t, ctx)
			}
		})
	}
	waitFor(t, "drafts-1 at 12 under throughput", func() bool { return r.sw.State().Awake == "drafts-1" })
	admin := httptest.NewServer(r.sw.AdminHandler())
	defer admin.Close()
	response, err := http.Post(admin.URL+"/switch/policy?name=useful-output", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	waitFor(t, "drafts-0 at 12 under useful-output", func() bool { return r.sw.State().Awake == "drafts-0" })
	cancel()
	wg.Wait()
	if three.sawAsleep.Load()+one.sawAsleep.Load()+none.sawAsleep.Load() > 0 {
		t.Fatal("a request reached a sleeping engine")
	}
}
