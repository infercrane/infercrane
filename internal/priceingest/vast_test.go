package priceingest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

func TestVastFeedPublishesExactOneGPUGlobalFloor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gpu := request["gpu_name"].(map[string]any)["eq"].(string)
		if request["limit"].(float64) != 1 || request["allocated_storage"].(float64) != 5 || request["external"].(map[string]any)["eq"] != false || request["num_gpus"].(map[string]any)["eq"].(float64) != 1 {
			t.Errorf("unsafe or ambiguous Vast query: %s", body)
		}
		_, _ = w.Write([]byte(`{"offers":[` +
			`{"id":12,"gpu_name":"` + gpu + `","num_gpus":1,"dph_total":0.9,"rentable":true,"rented":false,"verification":"verified"},` +
			`{"id":13,"gpu_name":"` + gpu + `","num_gpus":2,"dph_total":0.4,"rentable":true,"rented":false,"verification":"verified"}]}`))
	}))
	defer server.Close()
	now := time.Date(2026, 9, 1, 5, 0, 0, 0, time.UTC)
	catalog := pricing.NewDynamicCatalog(nil)
	feed := VastFeed{BaseURL: server.URL, Client: server.Client(), GPUQueries: []string{"H100 SXM", "L40S"}, Now: func() time.Time { return now }}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	for _, gpu := range []string{"H100 SXM", "L40S"} {
		key := pricing.Request{Cloud: "vast", Region: "global", GPU: gpu, GPUCount: 1, Replicas: 1}
		offer, err := catalog.Estimate(context.Background(), key)
		if err != nil || offer.Hourly != 0.9 || !strings.HasSuffix(offer.Source, "#offer-12") || offer.ObservedAt != now {
			t.Fatalf("unexpected %s floor: %#v err=%v", gpu, offer, err)
		}
	}
}

func TestVastFeedUpdatesCompletedShardsAndRetainsFailedShard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		gpu := request["gpu_name"].(map[string]any)["eq"].(string)
		if gpu == "L40S" {
			http.Error(w, "unavailable", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"offers":[{"id":1,"gpu_name":"` + gpu + `","num_gpus":1,"dph_total":2,"rentable":true,"rented":false,"verification":"verified"}]}`))
	}))
	defer server.Close()
	h100 := pricing.Request{Cloud: "vast", Region: "global", GPU: "H100 SXM", GPUCount: 1, Replicas: 1}
	l40s := pricing.Request{Cloud: "vast", Region: "global", GPU: "L40S", GPUCount: 1, Replicas: 1}
	catalog := pricing.NewDynamicCatalog(nil)
	catalog.ReplaceProvider("vast", map[pricing.Request]pricing.Estimate{
		h100: {Currency: "USD", Source: "old-h100", Hourly: 3, ObservedAt: time.Now(), StaleAfter: time.Hour},
		l40s: {Currency: "USD", Source: "old-l40s", Hourly: 1, ObservedAt: time.Now(), StaleAfter: time.Hour},
	})
	feed := VastFeed{BaseURL: server.URL, Client: server.Client(), GPUQueries: []string{"H100 SXM", "L40S"}}
	if err := feed.Refresh(context.Background(), catalog); err == nil {
		t.Fatal("expected failed L40S shard")
	}
	snapshot := catalog.Snapshot()
	if snapshot[h100].Hourly != 2 || snapshot[l40s].Source != "old-l40s" {
		t.Fatalf("unexpected staged snapshot: %#v", snapshot)
	}
}

func TestDefaultVastFeedLeavesRateLimitHeadroom(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		gpu := request["gpu_name"].(map[string]any)["eq"].(string)
		calls.Add(1)
		_, _ = w.Write([]byte(`{"offers":[{"id":1,"gpu_name":"` + gpu + `","num_gpus":1,"dph_total":1,"rentable":true,"rented":false,"verification":"verified"}]}`))
	}))
	defer server.Close()

	catalog := pricing.NewDynamicCatalog(nil)
	feed := VastFeed{BaseURL: server.URL, Client: server.Client()}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("default Vast sweep made %d calls; want four plus one request of headroom", got)
	}
	if got := len(catalog.Snapshot()); got != 4 {
		t.Fatalf("staged sweep published %d exact GPU floors, want 4", got)
	}
}

func TestVastFeedRetries429AtStatedReset(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"offers":[{"id":1,"gpu_name":"L40S","num_gpus":1,"dph_total":1,"rentable":true,"rented":false,"verification":"verified"}]}`))
	}))
	defer server.Close()
	catalog := pricing.NewDynamicCatalog(nil)
	feed := VastFeed{BaseURL: server.URL, Client: server.Client(), GPUQueries: []string{"L40S"}}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("429 retry calls=%d want 2", calls.Load())
	}
}

func TestVastFeedRetriesWithinSharedAttemptBudget(t *testing.T) {
	var calls atomic.Int64
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		gpu := request["gpu_name"].(map[string]any)["eq"].(string)
		seen[gpu]++
		call := calls.Add(1)
		if call <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"offers":[{"id":1,"gpu_name":"` + gpu + `","num_gpus":1,"dph_total":1,"rentable":true,"rented":false,"verification":"verified"}]}`))
	}))
	defer server.Close()
	catalog := pricing.NewDynamicCatalog(nil)
	feed := VastFeed{BaseURL: server.URL, Client: server.Client(), GPUQueries: []string{"B200", "H200", "H100 SXM", "L40S"}}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("retrying sweep made %d calls, want hard budget of 4", got)
	}
	if seen["B200"] != 3 || seen["H200"] != 1 || seen["H100 SXM"] != 0 {
		t.Fatalf("retry budget did not shorten the sweep: %#v", seen)
	}
}

func TestVastFeedAdvancesPastPoisonedShard(t *testing.T) {
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		gpu := request["gpu_name"].(map[string]any)["eq"].(string)
		seen[gpu]++
		if gpu == "poison" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"offers":[{"id":1,"gpu_name":"` + gpu + `","num_gpus":1,"dph_total":1,"rentable":true,"rented":false,"verification":"verified"}]}`))
	}))
	defer server.Close()
	catalog := pricing.NewDynamicCatalog(nil)
	feed := VastFeed{BaseURL: server.URL, Client: server.Client(), GPUQueries: []string{"first", "poison", "after"}, ShardsPerRefresh: 2}
	if err := feed.Refresh(context.Background(), catalog); err == nil {
		t.Fatal("expected poisoned shard to be reported")
	}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if seen["after"] == 0 {
		t.Fatalf("shard after persistent failure was never reached: %#v", seen)
	}
}

func TestVastFeedDoesNotSendCredentialsToCustomEndpoint(t *testing.T) {
	feed := VastFeed{APIKey: "secret", BaseURL: "http://example.test", GPUQueries: []string{"L40S"}}
	if err := feed.Refresh(context.Background(), pricing.NewDynamicCatalog(nil)); err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("credential exfiltration boundary failed: %v", err)
	}
}

func TestVastFeedWarmFetchesOneRequestedGPUWithinTheRequestBudget(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var query struct {
			GPU struct {
				Eq string `json:"eq"`
			} `json:"gpu_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&query)
		_, _ = w.Write([]byte(`{"offers":[{"id":7,"gpu_name":"` + query.GPU.Eq + `","num_gpus":1,"dph_total":0.33,"rentable":true,"rented":false,"verification":"verified"}]}`))
	}))
	defer server.Close()
	now := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
	catalog := pricing.NewDynamicCatalog(nil)
	feed := &VastFeed{BaseURL: server.URL, Client: server.Client(), Interval: 5 * time.Minute, Now: func() time.Time { return now }}
	row := func(gpu string) pricing.Request {
		return pricing.Request{Cloud: "vast", Region: "global", GPU: gpu, GPUCount: 1, Replicas: 1}
	}

	// Before any sweep the schedule is unknown, but the row is fetched.
	result := feed.Warm(t.Context(), catalog, row("rtx 4090"))
	if !result.Fetched || !result.NextRefreshAt.IsZero() || requests.Load() != 1 {
		t.Fatalf("result=%+v requests=%d", result, requests.Load())
	}
	if offer, err := catalog.Estimate(t.Context(), row("RTX 4090")); err != nil || offer.Hourly != 0.33 {
		t.Fatalf("warmed row=%+v err=%v", offer, err)
	}
	// Within one interval a second warm spends nothing.
	if result := feed.Warm(t.Context(), catalog, row("L4")); result.Fetched || requests.Load() != 1 || !strings.Contains(result.Detail, "already ran") {
		t.Fatalf("result=%+v requests=%d", result, requests.Load())
	}
	// Rows the feed never publishes are not fetched.
	for _, request := range []pricing.Request{row("Imaginary GPU"), {Cloud: "vast", Region: "global", GPU: "H100 SXM", GPUCount: 2, Replicas: 1}, {Cloud: "vast", Region: "global", GPU: "H100 SXM", GPUCount: 1, Replicas: 3}} {
		if result := feed.Warm(t.Context(), catalog, request); result.Fetched || result.Feed != "" || result.Detail == "" {
			t.Fatalf("%+v: result=%+v", request, result)
		}
	}
	// A warm never waits behind a running sweep.
	feed.mu.Lock()
	result = feed.Warm(t.Context(), catalog, row("L4"))
	feed.mu.Unlock()
	if result.Fetched || !strings.Contains(result.Detail, "refresh is running now") || requests.Load() != 1 {
		t.Fatalf("result=%+v requests=%d", result, requests.Load())
	}
}

func TestVastFeedDatesTheSweepThatReachesAGPU(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"offers":[]}`))
	}))
	defer server.Close()
	swept := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
	feed := &VastFeed{BaseURL: server.URL, Client: server.Client(), Interval: 5 * time.Minute, Now: func() time.Time { return swept }}
	if err := feed.Refresh(t.Context(), pricing.NewDynamicCatalog(nil)); err != nil {
		t.Fatal(err)
	}
	// The first sweep covered indexes 0-3; the cursor is at 4 (A100 SXM4).
	for gpu, want := range map[string]time.Duration{"A100 SXM4": 5 * time.Minute, "RTX 4090": 5 * time.Minute, "H200 NVL": 10 * time.Minute, "B200": 45 * time.Minute} {
		index := slices.Index(defaultVastGPUs, gpu)
		if got := feed.nextScheduledFetch(index, len(defaultVastGPUs)); !got.Equal(swept.Add(want)) {
			t.Fatalf("%s: next=%s want %s", gpu, got, swept.Add(want))
		}
	}
}
