package specswitch

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// maxEngineMetrics bounds the engine metrics the switch passes on.
const maxEngineMetrics = 8 << 20

// serveMetrics writes the switch's own gauges and counters, then the awake
// engine's metrics as the engine wrote them, so a scrape that reads engine
// gauges (running and waiting requests, cache use) keeps working. Requests
// waiting in the switch are not in the engine's waiting gauge; they are in
// infercrane_specswitch_requests_queued. Engine counters restart when another
// engine wakes: a rate across a swap mixes two engines.
func (s *Switch) serveMetrics(w http.ResponseWriter, r *http.Request) {
	state := s.State()
	s.mu.Lock()
	counts := make([][3]string, 0, len(s.counts))
	for key := range s.counts {
		counts = append(counts, key)
	}
	values := map[[3]string]int{}
	for _, key := range counts {
		values[key] = s.counts[key]
	}
	swapSeconds, swapCount, queuedTotal := s.swapSeconds, s.swapCount, s.queuedTotal
	engines := s.cfg.Engines
	s.mu.Unlock()
	sort.Slice(counts, func(i, j int) bool { return strings.Join(counts[i][:], "\x00") < strings.Join(counts[j][:], "\x00") })

	var b strings.Builder
	b.WriteString("# HELP infercrane_specswitch_engine_awake 1 for the engine that serves, 0 for a sleeping one.\n# TYPE infercrane_specswitch_engine_awake gauge\n")
	for _, engine := range engines {
		awake := 0
		if engine.Name == state.Awake {
			awake = 1
		}
		fmt.Fprintf(&b, "infercrane_specswitch_engine_awake{engine=%q,drafts=\"%d\"} %d\n", engine.Name, engine.Drafts, awake)
	}
	gauges := []struct {
		name, help string
		value      float64
	}{
		{"infercrane_specswitch_awake_drafts", "Draft tokens of the engine that serves.", float64(state.Drafts)},
		{"infercrane_specswitch_requests_in_flight", "Requests the awake engine serves now.", float64(state.InFlight)},
		{"infercrane_specswitch_requests_queued", "Requests waiting in the switch for room or for a swap to end.", float64(state.Queued)},
		{"infercrane_specswitch_swapping", "1 while a swap runs.", boolValue(state.Swapping)},
		{"infercrane_specswitch_healthy", "0 once no engine can serve.", boolValue(state.Healthy)},
	}
	for _, gauge := range gauges {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", gauge.name, gauge.help, gauge.name, gauge.name, gauge.value)
	}
	b.WriteString("# HELP infercrane_specswitch_swaps_total Swaps by engines and result.\n# TYPE infercrane_specswitch_swaps_total counter\n")
	for _, key := range counts {
		fmt.Fprintf(&b, "infercrane_specswitch_swaps_total{from=%q,to=%q,result=%q} %d\n", key[0], key[1], key[2], values[key])
	}
	fmt.Fprintf(&b, "# HELP infercrane_specswitch_swap_seconds Time from a swap's start until requests flow again, swaps that completed.\n# TYPE infercrane_specswitch_swap_seconds summary\ninfercrane_specswitch_swap_seconds_sum %g\ninfercrane_specswitch_swap_seconds_count %d\n", swapSeconds, swapCount)
	fmt.Fprintf(&b, "# HELP infercrane_specswitch_requests_queued_total Requests that waited in the switch.\n# TYPE infercrane_specswitch_requests_queued_total counter\ninfercrane_specswitch_requests_queued_total %d\n", queuedTotal)
	fmt.Fprintf(&b, "# HELP infercrane_specswitch_requests_rejected_total Requests refused because the queue was full.\n# TYPE infercrane_specswitch_requests_rejected_total counter\ninfercrane_specswitch_requests_rejected_total %d\n", state.Rejected)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())

	s.mu.Lock()
	engine := s.cfg.Engines[s.active]
	s.mu.Unlock()
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, engine.URL+"/metrics", nil)
	if err != nil {
		return
	}
	response, err := s.metrics.Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		_, _ = io.Copy(w, io.LimitReader(response.Body, maxEngineMetrics))
	}
}

func boolValue(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
