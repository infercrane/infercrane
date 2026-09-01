package priceingest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestVastFeedDoesNotSendCredentialsToCustomEndpoint(t *testing.T) {
	feed := VastFeed{APIKey: "secret", BaseURL: "http://example.test", GPUQueries: []string{"L40S"}}
	if err := feed.Refresh(context.Background(), pricing.NewDynamicCatalog(nil)); err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("credential exfiltration boundary failed: %v", err)
	}
}
