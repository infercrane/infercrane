package priceingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

func TestAzureFeedPublishesCurrentLinuxPAYGWithExactGPUCount(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeAzurePage(t, w, []azureRetailItem{
				azureItem("Standard_NV72ads_A10_v5", "westus3", "A10", 3.00, now.Add(-24*time.Hour)),
				azureItem("Standard_ND96isr_H100_v5", "eastus", "H100 newer", 96.00, now.Add(-time.Hour)),
			}, "")
			return
		}
		if r.Method != http.MethodGet || r.URL.Query().Get("currencyCode") != "'USD'" {
			t.Errorf("unexpected Azure request: %s %s", r.Method, r.URL.String())
		}
		filter := r.URL.Query().Get("$filter")
		if !strings.Contains(filter, "priceType eq 'Consumption'") || !strings.Contains(filter, "armSkuName eq 'Standard_N") {
			t.Errorf("Azure query lacks reviewed PAYG provenance: %s", filter)
		}
		items := []azureRetailItem{
			azureItem("Standard_ND96isr_H100_v5", "eastus", "H100", 98.40, now.Add(-48*time.Hour)),
			azureItem("Standard_NC24ads_A100_v4", "westeurope", "A100", 4.20, now.Add(-24*time.Hour)),
			azureItem("Standard_ND96isr_H100_v5", "eastus", "H100 Spot", 9.00, now.Add(-time.Hour)),
			azureItem("Standard_ND96isr_H100_v5", "eastus", "H100 Windows", 100.00, now.Add(-time.Hour)),
			azureItem("Standard_ND96isr_H100_v5", "eastus", "H100 future", 1.00, now.Add(time.Hour)),
			azureItem("Standard_NC24ads_A100_v4", "westeurope", "A100 non-primary", 1.00, now.Add(-time.Hour)),
			azureItem("Standard_Unknown_GPU", "eastus", "unknown", 0.01, now.Add(-time.Hour)),
		}
		items[2].MeterName = "ND96isr H100 v5 Spot"
		items[3].ProductName = "Virtual Machines ND H100 v5 Series Windows"
		items[5].IsPrimaryMeterRegion = false
		writeAzurePage(t, w, items, server.URL+"?page=2")
	}))
	defer server.Close()

	catalog := pricing.NewDynamicCatalog(nil)
	feed := AzureFeed{BaseURL: server.URL, Client: server.Client(), Now: func() time.Time { return now }, ValidFor: 10 * time.Minute}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	h100Key := pricing.Request{Cloud: "azure", Region: "eastus", GPU: "H100-80GB", GPUCount: 8, Replicas: 1}
	h100, err := catalog.Estimate(context.Background(), h100Key)
	if err != nil || h100.Hourly != 96 || h100.Hourly/float64(h100Key.GPUCount) != 12 || h100.ObservedAt != now || h100.StaleAfter != 10*time.Minute {
		t.Fatalf("unexpected Azure H100 VM price: %#v err=%v", h100, err)
	}
	if !strings.HasPrefix(h100.Source, defaultAzureRetailPricesURL+"?") || !strings.Contains(h100.Source, "%24filter=") || !strings.Contains(h100.Source, "arm_sku=Standard_ND96isr_H100_v5") {
		t.Fatalf("Azure source lacks official query and meter provenance: %s", h100.Source)
	}
	a100, err := catalog.Estimate(context.Background(), pricing.Request{Cloud: "azure", Region: "westeurope", GPU: "A100-80GB", GPUCount: 1, Replicas: 1})
	if err != nil || a100.Hourly != 4.2 {
		t.Fatalf("unexpected Azure A100 price: %#v err=%v", a100, err)
	}
	a10, err := catalog.Estimate(context.Background(), pricing.Request{Cloud: "azure", Region: "westus3", GPU: "A10-24GB", GPUCount: 2, Replicas: 1})
	if err != nil || a10.Hourly != 3 {
		t.Fatalf("unexpected Azure A10 price: %#v err=%v", a10, err)
	}
	if got := len(catalog.Snapshot()); got != 3 {
		t.Fatalf("published %d Azure prices, want exactly the three reviewed current Linux rows", got)
	}
}

func TestAzureFeedBoundsPaginationAndPreservesLastGoodSnapshot(t *testing.T) {
	var calls atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeAzurePage(t, w, []azureRetailItem{azureItem("Standard_NC24ads_A100_v4", "eastus", "A100", 2, time.Now().Add(-time.Hour))}, server.URL+"?next=1")
	}))
	defer server.Close()
	key := pricing.Request{Cloud: "azure", Region: "eastus", GPU: "A100-80GB", GPUCount: 1, Replicas: 1}
	catalog := pricing.NewDynamicCatalog(nil)
	catalog.ReplaceProvider("azure", map[pricing.Request]pricing.Estimate{key: {Currency: "USD", Hourly: 9, Source: "last-good", ObservedAt: time.Now(), StaleAfter: time.Hour}})
	err := (AzureFeed{BaseURL: server.URL, Client: server.Client(), RequestLimit: 2}).Refresh(context.Background(), catalog)
	if err == nil || !strings.Contains(err.Error(), "2-request") || calls.Load() != 2 {
		t.Fatalf("unbounded or unreported pagination: calls=%d err=%v", calls.Load(), err)
	}
	offer, lookupErr := catalog.Estimate(context.Background(), key)
	if lookupErr != nil || offer.Source != "last-good" {
		t.Fatalf("failed Azure refresh replaced its last-good snapshot: %#v err=%v", offer, lookupErr)
	}
}

func TestAzureFeedRejectsCrossOriginPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeAzurePage(t, w, nil, "https://example.invalid/steal")
	}))
	defer server.Close()
	err := (AzureFeed{BaseURL: server.URL, Client: server.Client()}).Refresh(context.Background(), pricing.NewDynamicCatalog(nil))
	if err == nil || !strings.Contains(err.Error(), "untrusted pagination") {
		t.Fatalf("cross-origin Azure pagination was not rejected: %v", err)
	}
}

func TestAzureFeedPreservesLastGoodSnapshotOnProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	key := pricing.Request{Cloud: "azure", Region: "eastus", GPU: "H100-80GB", GPUCount: 8, Replicas: 1}
	catalog := pricing.NewDynamicCatalog(nil)
	catalog.ReplaceProvider("azure", map[pricing.Request]pricing.Estimate{key: {Currency: "USD", Hourly: 80, Source: "last-good", ObservedAt: time.Now(), StaleAfter: time.Hour}})
	if err := (AzureFeed{BaseURL: server.URL, Client: server.Client()}).Refresh(context.Background(), catalog); err == nil {
		t.Fatal("expected Azure provider failure")
	}
	offer, err := catalog.Estimate(context.Background(), key)
	if err != nil || offer.Source != "last-good" {
		t.Fatalf("last-good Azure snapshot was lost: %#v err=%v", offer, err)
	}
}

func azureItem(sku, region, meter string, price float64, effective time.Time) azureRetailItem {
	return azureRetailItem{
		CurrencyCode: "USD", RetailPrice: price, UnitPrice: price, ArmRegionName: region,
		EffectiveStartDate: effective.UTC().Format(time.RFC3339), MeterID: strings.ToLower(strings.ReplaceAll(meter, " ", "-")),
		MeterName: meter, ProductName: "Virtual Machines GPU Series Linux", SKUName: meter,
		ServiceName: "Virtual Machines", UnitOfMeasure: "1 Hour", Type: "Consumption", IsPrimaryMeterRegion: true, ArmSKUName: sku,
	}
}

func writeAzurePage(t *testing.T, w http.ResponseWriter, items []azureRetailItem, next string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(azureRetailPage{Items: items, NextPageLink: next}); err != nil {
		t.Error(err)
	}
}

func TestAzureRetailQueryIsStableAndOfficial(t *testing.T) {
	endpoint, err := url.Parse(defaultAzureRetailPricesURL)
	if err != nil {
		t.Fatal(err)
	}
	first, second := azureRetailQueries(endpoint), azureRetailQueries(endpoint)
	if len(first) != 4 || len(second) != 4 {
		t.Fatalf("Azure query shards=%d/%d want four bounded family shards", len(first), len(second))
	}
	for index := range first {
		if first[index] != second[index] || !strings.HasPrefix(first[index], defaultAzureRetailPricesURL+"?") || len(first[index]) > 2_000 {
			t.Fatalf("Azure query %d is unstable, unofficial, or too large: %s", index, first[index])
		}
	}
}

func TestAzurePaginationAcceptsExplicitDefaultPortOnly(t *testing.T) {
	endpoint, _ := url.Parse("https://prices.azure.com/api/retail/prices")
	defaultPort, _ := url.Parse("https://prices.azure.com:443/api/retail/prices?$skip=1000")
	wrongPort, _ := url.Parse("https://prices.azure.com:444/api/retail/prices?$skip=1000")
	foreignHost, _ := url.Parse("https://example.com:443/api/retail/prices?$skip=1000")
	if !sameAzureOrigin(endpoint, defaultPort) {
		t.Fatal("Azure's explicit HTTPS default port was rejected")
	}
	if sameAzureOrigin(endpoint, wrongPort) || sameAzureOrigin(endpoint, foreignHost) {
		t.Fatal("untrusted Azure pagination origin was accepted")
	}
}
