package priceingest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

func TestVastFeedPublishesCheapestVerifiedRentableOffersAtomically(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"verified":{"eq":true}`) || !strings.Contains(string(body), `"rentable":{"eq":true}`) || !strings.Contains(string(body), `"rented":{"eq":false}`) {
			t.Fatalf("unsafe Vast market query: %s", body)
		}
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		gpu := request["gpu_name"].(map[string]any)["eq"].(string)
		_, _ = w.Write([]byte(`{"offers":[` +
			`{"id":11,"gpu_name":"` + gpu + `","num_gpus":1,"dph_total":1.3,"geolocation":"Texas, US","rentable":true,"rented":false,"verification":"verified"},` +
			`{"id":12,"gpu_name":"` + gpu + `","num_gpus":1,"dph_total":0.9,"geolocation":"Virginia, US","rentable":true,"rented":false,"verification":"verified"},` +
			`{"id":13,"gpu_name":"` + gpu + `","num_gpus":1,"dph_total":0.4,"geolocation":"Frankfurt, DE","rentable":false,"rented":false,"verification":"verified"},` +
			`{"id":14,"gpu_name":"` + gpu + `","num_gpus":2,"dph_total":1.7,"geolocation":"Frankfurt, DE","rentable":true,"rented":false,"verification":"verified"}]}`))
	}))
	defer server.Close()
	now := time.Date(2026, 9, 1, 5, 0, 0, 0, time.UTC)
	catalog := pricing.NewDynamicCatalog(nil)
	feed := VastFeed{BaseURL: server.URL, Client: server.Client(), GPUQueries: []string{"H100 SXM", "L40S"}, Workers: 2, Now: func() time.Time { return now }}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	for _, gpu := range []string{"H100 SXM", "L40S"} {
		single, err := catalog.Estimate(context.Background(), pricing.Request{Cloud: "vast", Region: "US", GPU: gpu, GPUCount: 1, Replicas: 1})
		if err != nil || single.Hourly != 0.9 || !strings.HasSuffix(single.Source, "#offer-12") || single.ObservedAt != now {
			t.Fatalf("unexpected cheapest %s offer: %#v err=%v", gpu, single, err)
		}
		multi, err := catalog.Estimate(context.Background(), pricing.Request{Cloud: "vast", Region: "DE", GPU: gpu, GPUCount: 2, Replicas: 1})
		if err != nil || multi.Hourly != 1.7 {
			t.Fatalf("unexpected multi-GPU %s offer: %#v err=%v", gpu, multi, err)
		}
	}
}

func TestVastFeedKeepsLastGoodSnapshotWhenSweepIsPartial(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 2 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"offers":[{"id":1,"gpu_name":"H100 SXM","num_gpus":1,"dph_total":2,"geolocation":"US","rentable":true,"rented":false,"verification":"verified"}]}`))
	}))
	defer server.Close()
	key := pricing.Request{Cloud: "vast", Region: "US", GPU: "H100 SXM", GPUCount: 1, Replicas: 1}
	catalog := pricing.NewDynamicCatalog(nil)
	catalog.ReplaceProvider("vast", map[pricing.Request]pricing.Estimate{key: {Currency: "USD", Source: "last-good", Hourly: 3, ObservedAt: time.Now(), StaleAfter: time.Hour}})
	feed := VastFeed{BaseURL: server.URL, Client: server.Client(), GPUQueries: []string{"H100 SXM", "L40S"}, Workers: 1}
	if err := feed.Refresh(context.Background(), catalog); err == nil {
		t.Fatal("expected partial provider sweep to fail")
	}
	offer, err := catalog.Estimate(context.Background(), key)
	if err != nil || offer.Source != "last-good" {
		t.Fatalf("partial sweep replaced the last-good snapshot: %#v err=%v", offer, err)
	}
}

func TestDefaultVastFeedUsesFiveGroupedMarketplaceQueries(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			GPUName struct {
				In []string `json:"in"`
			} `json:"gpu_name"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if len(request.GPUName.In) < 2 {
			t.Fatalf("default query was not grouped: %s", body)
		}
		calls.Add(1)
		gpu := request.GPUName.In[0]
		_, _ = w.Write([]byte(`{"offers":[{"id":1,"gpu_name":"` + gpu + `","num_gpus":1,"dph_total":1,"geolocation":"US","rentable":true,"rented":false,"verification":"verified"}]}`))
	}))
	defer server.Close()

	catalog := pricing.NewDynamicCatalog(nil)
	feed := VastFeed{BaseURL: server.URL, Client: server.Client()}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 5 {
		t.Fatalf("default Vast refresh made %d requests; public limit is five", got)
	}
	if got := len(catalog.Snapshot()); got != 5 {
		t.Fatalf("grouped refresh published %d offers, want 5", got)
	}
}

func TestVastRegionUsesCountrySuffix(t *testing.T) {
	for input, expected := range map[string]string{"Frankfurt, DE": "DE", ", US": "US", "": "global"} {
		if got := vastRegion(input); got != expected {
			t.Fatalf("vastRegion(%q)=%q want %q", input, got, expected)
		}
	}
}
