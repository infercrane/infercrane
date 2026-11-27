package specswitch

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The stand-in replay runs the H100 map's short-chat loads, time-compressed,
// against three stand-in engines whose speeds and cache rooms are the map's
// (fakeEngine with mapSpeed): once through the switch and once against each
// engine alone. A stand-in cannot show a real gain, since its speeds are the
// map's by construction; it shows that the switch reaches the band winner's
// output in every band, swaps inside the warm-up, drops no request and never
// sends one to a sleeping engine.

const (
	replayScale     = 50.0 // stand-in seconds run 50 times faster than real ones
	replayTokens    = 295  // the map's short-chat answer limit
	replayRoomThree = 58   // requests the map's 3-draft server ran at once
	replayRoomOne   = 111  // and the 1-draft server
)

// replayClient keeps a connection per worker, so a run of tens of thousands
// of requests does not exhaust the host's ports.
var replayClient = &http.Client{Transport: &http.Transport{MaxIdleConns: 1024, MaxIdleConnsPerHost: 1024}}

type replayRequest struct {
	start, end time.Time
	// firstToken is when the engine began the request; the stand-in writes
	// the whole answer at its end, so its first token is taken as its start.
	firstToken time.Time
	tokens     int
	ok         bool
}

// replayDriver holds a closed loop: each worker sends its next request when
// its last answer ends.
type replayDriver struct {
	url string

	mu       sync.Mutex
	requests []replayRequest
	workers  []context.CancelFunc
	wg       sync.WaitGroup
}

func (d *replayDriver) setLoad(load int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.workers) > load {
		last := len(d.workers) - 1
		d.workers[last]()
		d.workers = d.workers[:last]
	}
	for len(d.workers) < load {
		ctx, cancel := context.WithCancel(context.Background())
		d.workers = append(d.workers, cancel)
		d.wg.Go(func() { d.loop(ctx) })
	}
}

func (d *replayDriver) loop(ctx context.Context) {
	body := fmt.Sprintf(`{"model":"fake/model","messages":[{"role":"user","content":"hi"}],"max_tokens":%d}`, replayTokens)
	for ctx.Err() == nil {
		start := time.Now()
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.url+"/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response, err := replayClient.Do(request)
		row := replayRequest{start: start, tokens: replayTokens}
		if err == nil {
			var answer struct {
				Started int64 `json:"started_unix_nano"`
				Usage   struct {
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			row.ok = response.StatusCode == http.StatusOK && json.NewDecoder(response.Body).Decode(&answer) == nil
			row.tokens = answer.Usage.CompletionTokens
			row.firstToken = time.Unix(0, answer.Started)
			response.Body.Close()
		}
		row.end = time.Now()
		if ctx.Err() != nil {
			return
		}
		d.mu.Lock()
		d.requests = append(d.requests, row)
		d.mu.Unlock()
	}
}

func (d *replayDriver) stop() {
	d.setLoad(0)
	d.wg.Wait()
}

// window returns output tokens per second inside [from, to], in real-time
// terms, counting each request's tokens by the share of its time inside, and
// the failed requests that ended inside.
func (d *replayDriver) window(from, to time.Time) (float64, int) {
	return d.windowWithin(from, to, 0)
}

// windowWithin is window counting only requests whose first token came
// within limit of their sending (in stand-in time); zero counts every one.
func (d *replayDriver) windowWithin(from, to time.Time, limit time.Duration) (float64, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tokens, failed := 0.0, 0
	for _, row := range d.requests {
		if !row.end.After(from) || !row.start.Before(to) {
			continue
		}
		if !row.ok {
			failed++
			continue
		}
		if limit > 0 && row.firstToken.Sub(row.start) > limit {
			continue
		}
		end, start := row.end, row.start
		if end.After(to) {
			end = to
		}
		if start.Before(from) {
			start = from
		}
		inside := end.Sub(start)
		tokens += float64(row.tokens) * inside.Seconds() / row.end.Sub(row.start).Seconds()
	}
	return tokens / to.Sub(from).Seconds() / replayScale, failed
}

type replayEngines struct{ three, one, none *fakeEngine }

func newReplayEngines(t *testing.T) replayEngines {
	engines := replayEngines{
		three: newFakeEngine(t, "drafts-3", 3, replayRoomThree, mapSpeed(3)),
		one:   newFakeEngine(t, "drafts-1", 1, replayRoomOne, mapSpeed(1)),
		none:  newFakeEngine(t, "drafts-0", 0, 0, mapSpeed(0)),
	}
	for _, engine := range []*fakeEngine{engines.three, engines.one, engines.none} {
		engine.tokens, engine.scale = replayTokens, replayScale
		engine.swapDelay = time.Duration(float64(3*time.Second) / replayScale)
	}
	return engines
}

// replayTiming is DefaultTiming compressed like the stand-in's speeds.
func replayTiming() Timing {
	timing := DefaultTiming()
	for _, value := range []*Duration{&timing.Sample, &timing.Smooth, &timing.RaiseAfter, &timing.LowerAfter, &timing.MinDwell, &timing.DrainTimeout} {
		*value = Duration(float64(*value) / replayScale)
	}
	return timing
}

type replayPhase struct {
	Load   int    `json:"load"`
	Policy string `json:"policy"`
	// Want is the engine the bands give this load.
	Want string `json:"want"`
	// WarmUp overrides the warm-up, in real seconds, where the band lowers:
	// a lower band must hold 2 minutes before the switch moves.
	WarmUp float64 `json:"warm_up,omitempty"`
}

type replayCell struct {
	Load           int                `json:"load"`
	Policy         string             `json:"policy"`
	Awake          string             `json:"awake"`
	Want           string             `json:"want"`
	Adaptive       float64            `json:"adaptive_output_tokens_per_second"`
	Fixed          map[string]float64 `json:"fixed_output_tokens_per_second"`
	Best           string             `json:"best_fixed"`
	ShareOfBest    float64            `json:"share_of_best"`
	Failed         int                `json:"failed"`
	SwapsInWindow  int                `json:"swaps_in_window"`
	SwapsInWarmUp  int                `json:"swaps_in_warm_up"`
	WarmUpSeconds  float64            `json:"warm_up_seconds_real"`
	WindowSeconds  float64            `json:"window_seconds_real"`
	SleepingServed int64              `json:"requests_to_sleeping_engines"`
}

func TestStandInReplayReachesTheBandWinnerInEveryBand(t *testing.T) {
	if testing.Short() {
		t.Skip("the stand-in replay runs about half a minute")
	}
	// Real-time terms: the map's 30 s of warm-up, then a 50 s window per cell.
	real := func(seconds float64) time.Duration {
		return time.Duration(seconds * float64(time.Second) / replayScale)
	}
	warmUp, window := real(30), real(50)
	phases := []replayPhase{
		{Load: 32, Policy: "throughput", Want: "drafts-3"},
		{Load: 64, Policy: "throughput", Want: "drafts-1"},
		{Load: 96, Policy: "throughput", Want: "drafts-1"},
		{Load: 128, Policy: "throughput", Want: "drafts-1"},
		{Load: 128, Policy: "useful-output", Want: "drafts-0"},
		{Load: 32, Policy: "useful-output", Want: "drafts-3", WarmUp: 150},
	}

	// Each engine alone, at every load, for the fixed setups' figures.
	fixed := map[int]map[string]float64{}
	for _, load := range []int{32, 64, 96, 128} {
		fixed[load] = map[string]float64{}
		engines := newReplayEngines(t)
		for _, engine := range []*fakeEngine{engines.three, engines.one, engines.none} {
			driver := &replayDriver{url: engine.server.URL}
			driver.setLoad(load)
			time.Sleep(warmUp)
			from := time.Now()
			time.Sleep(window)
			output, failed := driver.window(from, time.Now())
			driver.stop()
			if failed > 0 {
				t.Fatalf("%s alone at %d: %d failed requests", engine.name, load, failed)
			}
			fixed[load][engine.name] = output
		}
	}

	engines := newReplayEngines(t)
	policies := map[string][]Band{
		"throughput":    {{UpTo: replayRoomThree, Engine: "drafts-3"}, {Engine: "drafts-1"}},
		"useful-output": {{UpTo: replayRoomThree, Engine: "drafts-3"}, {UpTo: replayRoomOne, Engine: "drafts-1"}, {Engine: "drafts-0"}},
	}
	r := newRig(t, []*fakeEngine{engines.three, engines.one, engines.none}, policies, "throughput", replayTiming(), 1024)
	driver := &replayDriver{url: r.server.URL}
	defer driver.stop()
	var cells []replayCell
	for _, phase := range phases {
		if err := r.sw.SetPolicy(phase.Policy); err != nil {
			t.Fatal(err)
		}
		begin := time.Now()
		driver.setLoad(phase.Load)
		phaseWarmUp := warmUp
		if phase.WarmUp > 0 {
			phaseWarmUp = real(phase.WarmUp)
		}
		time.Sleep(phaseWarmUp)
		from := time.Now()
		awakeAtStart := r.sw.State().Awake
		time.Sleep(window)
		to := time.Now()
		output, failed := driver.window(from, to)
		state := r.sw.State()
		cell := replayCell{Load: phase.Load, Policy: phase.Policy, Awake: state.Awake, Want: phase.Want, Adaptive: output, Fixed: fixed[phase.Load], Failed: failed,
			WarmUpSeconds: phaseWarmUp.Seconds() * replayScale, WindowSeconds: window.Seconds() * replayScale,
			SleepingServed: engines.three.sawAsleep.Load() + engines.one.sawAsleep.Load() + engines.none.sawAsleep.Load()}
		for _, swap := range state.Swaps {
			switch {
			case swap.Start.After(from) && swap.Start.Before(to):
				cell.SwapsInWindow++
			case swap.Start.After(begin) && !swap.Start.After(from):
				cell.SwapsInWarmUp++
			}
		}
		best := ""
		for name, value := range fixed[phase.Load] {
			if best == "" || value > fixed[phase.Load][best] {
				best = name
			}
		}
		cell.Best = best
		cell.ShareOfBest = output / fixed[phase.Load][best]
		cells = append(cells, cell)
		t.Logf("%s at %d: awake %s (want %s), adaptive %.0f, fixed %s, best %s, share %.3f, swaps in window %d",
			phase.Policy, phase.Load, cell.Awake, phase.Want, output, formatFixed(fixed[phase.Load]), best, cell.ShareOfBest, cell.SwapsInWindow)
		if awakeAtStart != phase.Want || state.Awake != phase.Want {
			t.Errorf("%s at %d: awake %s at the window's start and %s at its end, want %s", phase.Policy, phase.Load, awakeAtStart, state.Awake, phase.Want)
		}
		if cell.SwapsInWindow > 0 {
			t.Errorf("%s at %d: %d swaps inside the window", phase.Policy, phase.Load, cell.SwapsInWindow)
		}
		if failed > 0 {
			t.Errorf("%s at %d: %d failed requests", phase.Policy, phase.Load, failed)
		}
		// The band winner by the bands the policy uses: the throughput bands
		// stop at one draft, so their bar at 128 is the best fixed output.
		if phase.Policy == "throughput" && cell.ShareOfBest < 0.9 {
			t.Errorf("%s at %d: adaptive %.0f is %.1f%% of the best fixed (%s %.0f)", phase.Policy, phase.Load, output, 100*cell.ShareOfBest, best, fixed[phase.Load][best])
		}
		if share := output / fixed[phase.Load][phase.Want]; share < 0.9 {
			t.Errorf("%s at %d: adaptive %.0f is %.1f%% of %s alone", phase.Policy, phase.Load, output, 100*share, phase.Want)
		}
	}
	if served := engines.three.sawAsleep.Load() + engines.one.sawAsleep.Load() + engines.none.sawAsleep.Load(); served > 0 {
		t.Errorf("%d requests reached a sleeping engine", served)
	}
	if path := os.Getenv("INFERCRANE_SPECSWITCH_REPLAY_RECORD"); path != "" {
		record := map[string]any{
			"schema":      "infercrane.specswitch-stand-in-replay/v1",
			"what":        "Stand-in engines with the H100 map's short-chat speeds and cache rooms (Qwen3.8-27B-FP8, vLLM 0.34.0 medians), time-compressed 50x; the switch against each engine alone. Not a measurement of any GPU.",
			"rooms":       map[string]int{"drafts-3": replayRoomThree, "drafts-1": replayRoomOne},
			"policies":    policies,
			"timing_real": DefaultTiming(),
			"cells":       cells,
			"swaps":       r.sw.State().Swaps,
		}
		encoded, _ := json.MarshalIndent(record, "", "  ")
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func formatFixed(values map[string]float64) string {
	parts := []string{}
	for _, name := range []string{"drafts-3", "drafts-1", "drafts-0"} {
		parts = append(parts, fmt.Sprintf("%s %.0f", name, values[name]))
	}
	return strings.Join(parts, ", ")
}

// traceCell is one run of the trace: one setup over the whole trace.
type traceCell struct {
	Setup         string        `json:"setup"`
	AllOutput     float64       `json:"all_output_tokens_per_second"`
	WithinOne     float64       `json:"within_1s_output_tokens_per_second"`
	Failed        int           `json:"failed"`
	Swaps         []SwapRecord  `json:"swaps,omitempty"`
	FirstTokenP95 []float64     `json:"first_token_p95_ms_after_each_change,omitempty"`
	Phases        []tracePhase  `json:"phases"`
	Elapsed       time.Duration `json:"-"`
}

type tracePhase struct {
	Load      int     `json:"load"`
	AllOutput float64 `json:"all_output_tokens_per_second"`
	WithinOne float64 `json:"within_1s_output_tokens_per_second"`
}

// runTrace holds each phase's load in turn at url and scores the whole trace.
func runTrace(t *testing.T, url string, phases []replayPhase, phaseLength time.Duration) traceCell {
	t.Helper()
	driver := &replayDriver{url: url}
	defer driver.stop()
	limit := time.Duration(float64(time.Second) / replayScale)
	var cell traceCell
	start := time.Now()
	var changes []time.Time
	for _, phase := range phases {
		begin := time.Now()
		changes = append(changes, begin)
		driver.setLoad(phase.Load)
		time.Sleep(phaseLength)
		all, _ := driver.window(begin, time.Now())
		within, _ := driver.windowWithin(begin, time.Now(), limit)
		cell.Phases = append(cell.Phases, tracePhase{Load: phase.Load, AllOutput: all, WithinOne: within})
	}
	end := time.Now()
	cell.AllOutput, cell.Failed = driver.window(start, end)
	cell.WithinOne, _ = driver.windowWithin(start, end, limit)
	// The first token of requests sent in the 30 real seconds after each change.
	after := time.Duration(float64(30*time.Second) / replayScale)
	driver.mu.Lock()
	for _, change := range changes {
		var waits []float64
		for _, row := range driver.requests {
			if row.ok && !row.start.Before(change) && row.start.Before(change.Add(after)) {
				waits = append(waits, float64(row.firstToken.Sub(row.start))*replayScale/float64(time.Millisecond))
			}
		}
		cell.FirstTokenP95 = append(cell.FirstTokenP95, percentile95(waits))
	}
	driver.mu.Unlock()
	cell.Elapsed = end.Sub(start)
	return cell
}

func percentile95(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted[min(len(sorted)-1, int(float64(len(sorted))*0.95))]
}

// traceStudy runs the trace of 32, then 128, then 32 requests at once, each
// phase phaseLength of real time, with every sleep and every wake taking
// swapDelay of real time: each setup alone (one draft token twice, the copy)
// and the switch under both policies, scored over the whole trace by all
// output and by output whose first token, time in the switch's and the
// engine's queues included, came within 1 s. The stand-in's speeds are the
// map's, so what it measures is the switch's own cost: its lag, its drains
// and the time a swap holds new requests.
func traceStudy(t *testing.T, phaseLength, swapDelay time.Duration) map[string]any {
	t.Helper()
	phases := []replayPhase{{Load: 32}, {Load: 128}, {Load: 32}}
	compressed := time.Duration(float64(phaseLength) / replayScale)
	engines := func() replayEngines {
		set := newReplayEngines(t)
		for _, engine := range []*fakeEngine{set.three, set.one, set.none} {
			engine.swapDelay = time.Duration(float64(swapDelay) / replayScale)
		}
		return set
	}
	results := map[string]traceCell{}
	for _, name := range []string{"drafts-3", "drafts-1", "drafts-0", "drafts-1/copy"} {
		set := engines()
		engine := map[string]*fakeEngine{"drafts-3": set.three, "drafts-1": set.one, "drafts-0": set.none, "drafts-1/copy": set.one}[name]
		cell := runTrace(t, engine.server.URL, phases, compressed)
		cell.Setup = name
		results[name] = cell
	}
	for _, policy := range []string{"throughput", "useful-output"} {
		set := engines()
		list := []*fakeEngine{set.three, set.one}
		policies := map[string][]Band{"throughput": {{UpTo: replayRoomThree, Engine: "drafts-3"}, {Engine: "drafts-1"}}}
		if policy == "useful-output" {
			list = append(list, set.none)
			policies = map[string][]Band{"useful-output": {{UpTo: replayRoomThree, Engine: "drafts-3"}, {UpTo: replayRoomOne, Engine: "drafts-1"}, {Engine: "drafts-0"}}}
		}
		r := newRig(t, list, policies, policy, replayTiming(), 1024)
		cell := runTrace(t, r.server.URL, phases, compressed)
		cell.Setup = "switch/" + policy
		cell.Swaps = r.sw.State().Swaps
		results[cell.Setup] = cell
		var served int64
		for _, engine := range list {
			served += engine.sawAsleep.Load()
		}
		if served > 0 {
			t.Errorf("%s: %d requests reached a sleeping engine", policy, served)
		}
		if len(cell.Swaps) != 2 {
			t.Errorf("%s: %d swaps, want 2 (up at 128, down at 32)", policy, len(cell.Swaps))
		}
		for _, swap := range cell.Swaps {
			if swap.Result != SwapDone {
				t.Errorf("%s: swap %s -> %s %s", policy, swap.From, swap.To, swap.Result)
			}
		}
	}
	copyDifference := math.Abs(results["drafts-1"].AllOutput-results["drafts-1/copy"].AllOutput) / math.Max(results["drafts-1"].AllOutput, results["drafts-1/copy"].AllOutput)
	allowance := math.Max(copyDifference, 0.0075)
	best := func(view func(traceCell) float64) (string, float64) {
		name, value := "", 0.0
		for _, setup := range []string{"drafts-3", "drafts-1", "drafts-0"} {
			if view(results[setup]) > value {
				name, value = setup, view(results[setup])
			}
		}
		return name, value
	}
	bestAll, bestAllValue := best(func(cell traceCell) float64 { return cell.AllOutput })
	bestWithin, bestWithinValue := best(func(cell traceCell) float64 { return cell.WithinOne })
	for name, cell := range results {
		t.Logf("%-24s all %6.0f  within 1 s %6.0f  failed %d  swaps %d  first-token p95 after each change (ms) %v", name, cell.AllOutput, cell.WithinOne,
			cell.Failed, len(cell.Swaps), cell.FirstTokenP95)
		if cell.Failed > 0 {
			t.Errorf("%s: %d failed requests", name, cell.Failed)
		}
	}
	throughput, useful := results["switch/throughput"], results["switch/useful-output"]
	allGain, withinGain := throughput.AllOutput/bestAllValue-1, useful.WithinOne/bestWithinValue-1
	t.Logf("phases %s, sleep and wake %s each: all output %+.1f%% against %s, within 1 s %+.1f%% against %s, allowance %.1f%%",
		phaseLength, swapDelay, 100*allGain, bestAll, 100*withinGain, bestWithin, 100*allowance)
	return map[string]any{"phase_seconds": phaseLength.Seconds(), "sleep_and_wake_seconds_each": swapDelay.Seconds(), "allowance": allowance,
		"best_all_output": bestAll, "all_output_gain": allGain, "best_within_1s": bestWithin, "within_1s_gain": withinGain, "results": results}
}

// The trace the coordinator asked for, at one point: five-minute phases, 8 s
// to sleep and 8 s to wake. It checks the switch's behaviour, not its gain,
// which a stand-in cannot give. INFERCRANE_SPECSWITCH_TRACE_GRID runs the
// grid of phase lengths and swap times and writes it to that path.
func TestStandInTraceAcrossBands(t *testing.T) {
	if testing.Short() {
		t.Skip("the stand-in trace runs about two minutes")
	}
	path := os.Getenv("INFERCRANE_SPECSWITCH_TRACE_GRID")
	if path == "" {
		traceStudy(t, 5*time.Minute, 8*time.Second)
		return
	}
	var grid []map[string]any
	for _, point := range []struct{ phase, swap time.Duration }{
		{5 * time.Minute, 2 * time.Second}, {5 * time.Minute, 8 * time.Second}, {15 * time.Minute, 8 * time.Second}, {15 * time.Minute, 20 * time.Second},
	} {
		grid = append(grid, traceStudy(t, point.phase, point.swap))
	}
	record := map[string]any{
		"schema": "infercrane.specswitch-stand-in-trace/v1",
		"what": "Stand-in engines with the H100 map's short-chat speeds and cache rooms, 50x time; a trace of 32, 128, 32 requests at once; the switch's default timing " +
			"(raise after 3 s, lower after 2 min, a minute's dwell); each point a phase length and a time for each sleep and each wake. Not a measurement of any GPU.",
		"grid": grid,
	}
	encoded, _ := json.MarshalIndent(record, "", "  ")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
