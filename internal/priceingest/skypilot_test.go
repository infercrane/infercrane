package priceingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

func TestRefreshImportsCheapestExactGPUPricesWithoutReplacingManualEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("InstanceType,AcceleratorName,AcceleratorCount,vCPUs,MemoryGiB,Region,SpotPrice,Price,AvailabilityZone,GpuInfo\n" +
			"expensive,L40S,1.0,8,64,EU,0.8,0.9,EU-1,{}\n" +
			"cheap,L40S,1.0,8,64,EU,0.7,0.4,EU-2,{}\n" +
			"cpu,,0,8,64,EU,0,0.1,EU-3,{}\n"))
	}))
	defer server.Close()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	manualKey := pricing.Request{Cloud: "runpod", Region: "EU", GPU: "L40S", GPUCount: 1, Replicas: 1}
	catalog := pricing.NewDynamicCatalog(map[pricing.Request]pricing.Estimate{manualKey: {Currency: "USD", Source: "operator", Hourly: 0.3, ObservedAt: now, StaleAfter: time.Hour}})
	feed := Feed{Client: server.Client(), Sources: map[string]string{"runpod": server.URL}, ValidFor: 2 * time.Hour, Now: func() time.Time { return now }}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	estimate, err := catalog.Estimate(context.Background(), manualKey)
	if err != nil || estimate.Hourly != 0.3 || estimate.Source != "operator" {
		t.Fatalf("manual evidence must win: %#v, %v", estimate, err)
	}
	if len(catalog.Snapshot()) != 1 {
		t.Fatalf("expected one deduplicated exact tuple, got %#v", catalog.Snapshot())
	}
}

func TestRefreshKeepsSuccessfulProvidersWhenAnotherSourceFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("AcceleratorName,AcceleratorCount,Region,Price\nA100,1,global,1.2\n"))
	}))
	defer server.Close()
	catalog := pricing.NewDynamicCatalog(nil)
	feed := Feed{Client: server.Client(), Sources: map[string]string{"lambda": server.URL, "runpod": server.URL + "/missing"}}
	if err := feed.Refresh(context.Background(), catalog); err == nil {
		t.Fatal("expected partial refresh error")
	}
	prices := catalog.Snapshot()
	if len(prices) != 1 {
		t.Fatalf("expected successful provider to remain, got %#v", prices)
	}
	for request := range prices {
		if request.Cloud != "lambda" {
			t.Fatalf("unexpected provider survived partial refresh: %#v", request)
		}
	}
}
