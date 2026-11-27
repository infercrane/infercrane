package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/resultmetrics"
)

// TestScriptedCountersNeverDecrease walks three cycles of the timeline:
// a counter that went down would look like a reset to Prometheus and fire
// the example alerts without a new record.
func TestScriptedCountersNeverDecrease(t *testing.T) {
	started := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	f := newFixture(started, time.Minute)
	last := map[string]float64{}
	sawBlock, sawRollback := false, false
	for step := range 24 {
		var out bytes.Buffer
		if err := resultmetrics.WritePrometheus(&out, f.results(started.Add(time.Duration(step)*time.Minute+time.Second))); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.Contains(line, `verdict="BLOCK"} 1`) {
				sawBlock = true
			}
			if strings.Contains(line, `state="rolled_back"} 1`) {
				sawRollback = true
			}
			if strings.HasPrefix(line, "#") || !strings.Contains(line, "_total{") {
				continue
			}
			cut := strings.LastIndex(line, " ")
			series := line[:cut]
			value, err := strconv.ParseFloat(line[cut+1:], 64)
			if err != nil {
				t.Fatal(err)
			}
			if previous, ok := last[series]; ok && value < previous {
				t.Fatalf("step %d: %s went from %g to %g", step, series, previous, value)
			}
			last[series], seen[series] = value, true
		}
		for series := range last {
			if !seen[series] {
				t.Fatalf("step %d: counter %s disappeared", step, series)
			}
		}
	}
	if !sawBlock || !sawRollback {
		t.Fatalf("timeline lacks a BLOCK (%v) or a rollback (%v)", sawBlock, sawRollback)
	}
}

func TestFixtureServesEachSource(t *testing.T) {
	f := newFixture(time.Now(), time.Minute)
	f.engines["Qwen/Qwen3-8B"].advance(f.random, 4, 1)
	handler := f.handler()
	get := func(path, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := get("/metrics", ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "infercrane_gateway_requests_total") {
		t.Fatalf("gateway status=%d", response.Code)
	}
	if response := get("/engine/metrics", ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `vllm:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen3-8B",le="+Inf"}`) {
		t.Fatalf("engine status=%d body=%s", response.Code, response.Body.String())
	}
	if response := get("/api/v1/metrics", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("results without a key status=%d", response.Code)
	}
	if response := get("/api/v1/metrics", fixtureKey); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `deployment="demo-support-rag"`) {
		t.Fatalf("results status=%d", response.Code)
	}
}
