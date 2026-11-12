package metrics

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func fixture(t *testing.T, name string) EngineSnapshot {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ParseEngine(string(raw))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return snapshot
}

func measured(t *testing.T, previous, current EngineSnapshot) map[string]domain.OperationalMeasurement {
	t.Helper()
	observed := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	rows, err := EngineMeasurements(&previous, current, EngineMeasurementOptions{ReplicaID: "target-1", ObservedAt: observed, TTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) > 32 {
		t.Fatalf("one scrape produced %d rows; storage accepts at most 32", len(rows))
	}
	out := map[string]domain.OperationalMeasurement{}
	for _, row := range rows {
		if _, duplicate := out[row.Name]; duplicate {
			t.Fatalf("duplicate measurement %s", row.Name)
		}
		if row.EvidenceClass != "measured" || row.Source != current.Engine+"_metrics" || row.ReplicaID != "target-1" || row.SampleCount < 1 || !row.ValidUntil.Equal(observed.Add(90*time.Second)) || row.Unit != measurementUnit(row.Name) {
			t.Fatalf("row envelope=%+v", row)
		}
		out[row.Name] = row
	}
	return out
}

func expectValues(t *testing.T, rows map[string]domain.OperationalMeasurement, want map[string]float64, absent ...string) {
	t.Helper()
	for name, value := range want {
		row, ok := rows[name]
		if !ok {
			t.Errorf("%s missing", name)
			continue
		}
		if math.Abs(row.Value-value) > 1e-6 {
			t.Errorf("%s = %v, want %v", name, row.Value, value)
		}
	}
	for _, name := range absent {
		if row, ok := rows[name]; ok {
			t.Errorf("%s must stay unavailable, got %+v", name, row)
		}
	}
}

func TestParseEngineVLLMFixtureWindow(t *testing.T) {
	first, second := fixture(t, "vllm_scrape0.prom"), fixture(t, "vllm_scrape1.prom")
	if second.Engine != EngineVLLM || *second.RequestsRunning != 6 || *second.RequestsWaiting != 2 || *second.KVCacheUsage != .93 {
		t.Fatalf("gauges=%+v", second)
	}
	if second.PrefillTime == nil || len(second.PrefillTime.Buckets) != 8 || !math.IsInf(second.PrefillTime.Buckets[7].UpperBound, 1) {
		t.Fatalf("prefill histogram=%+v", second.PrefillTime)
	}
	rows := measured(t, first, second)
	expectValues(t, rows, map[string]float64{
		MeasurementRequestsRunning: 6, MeasurementRequestsWaiting: 2, MeasurementKVCacheUsage: 93,
		MeasurementQueueTimeP95: 2250, MeasurementPrefillTimeP95: 75, MeasurementDecodeTimeP95: 1750,
		MeasurementTTFTP95: 1750, MeasurementITLP95: 93.75, MeasurementTPOTP95: 100, MeasurementTPOTMean: 40,
		MeasurementRequestsCompleted: 100, MeasurementPreemptions: 5,
		MeasurementPrefixCacheHitRate: .1, MeasurementSpecDecodeAccept: .4,
	})
	if rows[MeasurementPrefixCacheHitRate].SampleCount != 20000 || rows[MeasurementSpecDecodeAccept].SampleCount != 2000 || rows[MeasurementITLP95].SampleCount != 5000 {
		t.Fatalf("sample counts must carry the window denominator: %+v", rows)
	}
}

func TestParseEngineSGLangFixtureWindow(t *testing.T) {
	first, second := fixture(t, "sglang_scrape0.prom"), fixture(t, "sglang_scrape1.prom")
	// Two tensor-parallel ranks replicate the scheduler gauges and SGLang adds
	// per-priority breakdowns next to the priority="" total.
	if second.Engine != EngineSGLang || *second.RequestsRunning != 4 || *second.RequestsWaiting != 7 {
		t.Fatalf("queue gauges double counted: running=%v waiting=%v", *second.RequestsRunning, *second.RequestsWaiting)
	}
	rows := measured(t, first, second)
	expectValues(t, rows, map[string]float64{
		MeasurementRequestsRunning: 4, MeasurementRequestsWaiting: 7, MeasurementKVCacheUsage: 88,
		MeasurementQueueTimeP95: 500, MeasurementPrefillTimeP95: 750, MeasurementTTFTP95: 1750,
		MeasurementITLP95: 60, MeasurementTPOTP95: 60, MeasurementTPOTMean: 40,
		MeasurementRequestsCompleted: 100, MeasurementPreemptions: 0,
		MeasurementPrefixCacheHitRate: .6, MeasurementSpecDecodeAccept: .72,
	}, MeasurementDecodeTimeP95)
}

func TestParseEngineLlamaCPPFixtureWindow(t *testing.T) {
	first, second := fixture(t, "llamacpp_scrape0.prom"), fixture(t, "llamacpp_scrape1.prom")
	if second.Engine != EngineLlamaCPP || *second.RequestsRunning != 1 || *second.RequestsWaiting != 0 {
		t.Fatalf("gauges=%+v", second)
	}
	rows := measured(t, first, second)
	// llama.cpp exports zero speculative counters when speculation is off;
	// zero drafts is not a 0% acceptance rate.
	expectValues(t, rows, map[string]float64{MeasurementRequestsRunning: 1, MeasurementRequestsWaiting: 0, MeasurementTPOTMean: 40},
		MeasurementKVCacheUsage, MeasurementSpecDecodeAccept, MeasurementPrefixCacheHitRate, MeasurementTTFTP95,
		MeasurementITLP95, MeasurementTPOTP95, MeasurementRequestsCompleted, MeasurementPreemptions)
	if rows[MeasurementTPOTMean].SampleCount != 400 {
		t.Fatalf("token-weighted TPOT sample count=%d", rows[MeasurementTPOTMean].SampleCount)
	}
}

func TestEngineMeasurementsNeverInventWindowValues(t *testing.T) {
	first, second := fixture(t, "vllm_scrape0.prom"), fixture(t, "vllm_scrape1.prom")
	observed := time.Now().UTC()
	options := EngineMeasurementOptions{ObservedAt: observed, TTL: time.Minute}
	gaugesOnly := []string{MeasurementRequestsRunning, MeasurementRequestsWaiting, MeasurementKVCacheUsage}

	rows, err := EngineMeasurements(nil, second, options)
	if err != nil || len(rows) != len(gaugesOnly) {
		t.Fatalf("first scrape must yield gauges only: rows=%+v err=%v", rows, err)
	}
	// An engine restart moves cumulative values backwards: no window.
	if rows, err = EngineMeasurements(&second, first, options); err != nil || len(rows) != len(gaugesOnly) {
		t.Fatalf("counter reset produced window values: rows=%+v err=%v", rows, err)
	}
	other := fixture(t, "sglang_scrape0.prom")
	if rows, err = EngineMeasurements(&other, second, options); err != nil || len(rows) != len(gaugesOnly) {
		t.Fatalf("engine change produced window values: rows=%+v err=%v", rows, err)
	}
	// No completed requests in the window: latency stays unavailable while
	// counted values are a measured zero.
	rows, err = EngineMeasurements(&second, second, options)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]float64{}
	for _, row := range rows {
		byName[row.Name] = row.Value
	}
	for _, name := range []string{MeasurementQueueTimeP95, MeasurementTTFTP95, MeasurementTPOTMean, MeasurementPrefixCacheHitRate, MeasurementSpecDecodeAccept} {
		if _, ok := byName[name]; ok {
			t.Fatalf("idle window reported %s", name)
		}
	}
	if value, ok := byName[MeasurementPreemptions]; !ok || value != 0 {
		t.Fatalf("idle window preemptions=%v ok=%v", value, ok)
	}
	if _, err = EngineMeasurements(nil, second, EngineMeasurementOptions{ObservedAt: observed}); err == nil {
		t.Fatal("missing TTL accepted")
	}
}

func TestParseEngineRejectsUnsupportedAndAmbiguousPayloads(t *testing.T) {
	if _, err := ParseEngine("tgi_queue_size 3\nprocess_cpu_seconds_total 1\n"); !errors.Is(err, ErrNoEngineMetrics) {
		t.Fatalf("unsupported payload err=%v", err)
	}
	if _, err := ParseEngine("vllm:num_requests_running 1\nsglang:num_running_reqs 1\n"); err == nil {
		t.Fatal("mixed engines accepted")
	}
	if _, err := ParseEngine(strings.Repeat("x", MaxEnginePayloadBytes+1)); err == nil {
		t.Fatal("oversized payload accepted")
	}
	snapshot, err := ParseEngine("vllm:num_requests_running{model_name=\"org/model {v1}\"} 4 1700000000000\nvllm:num_requests_waiting NaN\n")
	if err != nil || snapshot.RequestsRunning == nil || *snapshot.RequestsRunning != 4 {
		t.Fatalf("quoted label or timestamp mishandled: %+v err=%v", snapshot, err)
	}
	if snapshot.RequestsWaiting != nil {
		t.Fatalf("NaN sample became evidence: %v", *snapshot.RequestsWaiting)
	}
	// SGLang's acceptance gauge is meaningless while speculation is off.
	snapshot, err = ParseEngine("sglang:spec_num_draft_tokens 0\nsglang:spec_accept_rate 0\n")
	if err != nil || snapshot.SpecDecodeAcceptRate != nil {
		t.Fatalf("disabled speculation reported acceptance=%v err=%v", snapshot.SpecDecodeAcceptRate, err)
	}
}

func TestHistogramQuantileBoundaries(t *testing.T) {
	inf := math.Inf(1)
	cases := []struct {
		histogram Histogram
		want      float64
		ok        bool
	}{
		{Histogram{Count: 10, Buckets: []HistogramBucket{{.1, 10}, {inf, 10}}}, .095, true},
		{Histogram{Count: 10, Buckets: []HistogramBucket{{.1, 0}, {.2, 0}, {inf, 10}}}, 0, false},
		{Histogram{Count: 10, Buckets: []HistogramBucket{{.1, 5}, {.2, 4}, {inf, 10}}}, 0, false},
		{Histogram{Count: 0, Buckets: []HistogramBucket{{.1, 0}, {inf, 0}}}, 0, false},
		{Histogram{Count: 4, Sum: 1}, 0, false},
	}
	for index, test := range cases {
		got, ok := test.histogram.Quantile(.95)
		if ok != test.ok || math.Abs(got-test.want) > 1e-9 {
			t.Fatalf("case %d: got=%v ok=%v want=%v ok=%v", index, got, ok, test.want, test.ok)
		}
	}
}
