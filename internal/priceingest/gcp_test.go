package priceingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

func TestGCPFeedPublishesExactCurrentOnDemandGPUComponents(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Query().Get("currencyCode") != "USD" || r.URL.Query().Get("pageSize") != "5000" {
			t.Errorf("unexpected GCP request: %s %s", r.Method, r.URL.String())
		}
		switch r.URL.Query().Get("pageToken") {
		case "":
			writeGCPPage(t, w, []gcpCatalogSKU{
				gcpSKU("88B8-C3ED-03F0", "Nvidia Tesla T4 GPU running in Americas", []string{"us-central1", "us-east1"}, 0, 350_000_000, now.Add(-12*time.Hour)),
				gcpSKU("1111-2222-3333", "Nvidia L4 GPU running in Netherlands", []string{"europe-west4"}, 0, 70_600_000, now.Add(-24*time.Hour)),
				gcpSKU("1111-2222-3333", "Nvidia L4 GPU running in Netherlands", []string{"europe-west4"}, 0, 672_000_000, now.Add(-time.Hour)),
				gcpSKU("AAAA-BBBB-CCCC", "Nvidia L40 GPU running in Netherlands", []string{"europe-west4"}, 0, 1, now.Add(-time.Hour)),
			}, "page-two")
		case "page-two":
			spot := gcpSKU("4444-5555-6666", "Nvidia Tesla T4 GPU running in Americas", []string{"us-central1"}, 0, 10_000_000, now.Add(-time.Hour))
			spot.Category.UsageType = "Preemptible"
			future := gcpSKU("7777-8888-9999", "Nvidia Tesla V100 GPU running in Americas", []string{"us-central1"}, 1, 0, now.Add(time.Hour))
			foreign := gcpSKU("ABCD-EF01-2345", "Nvidia Tesla V100 GPU running in Americas", []string{"us-central1"}, 1, 0, now.Add(-time.Hour))
			foreign.ServiceProviderName = "Partner"
			writeGCPPage(t, w, []gcpCatalogSKU{spot, future, foreign}, "")
		default:
			t.Errorf("unexpected page token %q", r.URL.Query().Get("pageToken"))
		}
	}))
	defer server.Close()

	catalog := pricing.NewDynamicCatalog(nil)
	feed := GCPFeed{BaseURL: server.URL + "/catalog", Client: server.Client(), Now: func() time.Time { return now }, ValidFor: time.Hour}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	for request, want := range map[pricing.Request]float64{
		{Cloud: "gcp", Region: "us-central1", GPU: "nvidia-tesla-t4", GPUCount: 1, Replicas: 1}: 0.35,
		{Cloud: "gcp", Region: "us-east1", GPU: "nvidia-tesla-t4", GPUCount: 1, Replicas: 1}:    0.35,
		{Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4", GPUCount: 1, Replicas: 1}:      0.672,
	} {
		estimate, err := catalog.Estimate(context.Background(), request)
		if err != nil || estimate.Hourly != want || estimate.Currency != "USD" || estimate.CostScope != pricing.CostScopeAcceleratorOnly || estimate.ObservedAt != now || estimate.StaleAfter != time.Hour || !estimate.GuaranteedUntil.IsZero() {
			t.Fatalf("unexpected GCP estimate for %#v: %#v err=%v", request, estimate, err)
		}
		if !strings.HasPrefix(estimate.Source, defaultGCPCatalogURL+"?currencyCode=USD&pageSize=5000#billing_component=gpu&sku_id=") || strings.Contains(estimate.Source, server.URL) || strings.Contains(estimate.Source, "key=") {
			t.Fatalf("GCP source lacks safe official SKU provenance: %s", estimate.Source)
		}
	}
	if got := len(catalog.Snapshot()); got != 3 {
		t.Fatalf("published %d GCP prices, want only three reviewed region tuples: %#v", got, catalog.Snapshot())
	}
	if calls.Load() != 2 {
		t.Fatalf("GCP pages fetched=%d want 2", calls.Load())
	}
}

func TestGCPFeedPaginationFailurePreservesLastGoodSnapshot(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeGCPPage(t, w, []gcpCatalogSKU{gcpSKU("1111-2222-3333", "Nvidia L4 GPU running in Netherlands", []string{"europe-west4"}, 0, 672_000_000, time.Now().Add(-time.Hour))}, "again")
	}))
	defer server.Close()
	key := pricing.Request{Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4", GPUCount: 1, Replicas: 1}
	catalog := pricing.NewDynamicCatalog(nil)
	catalog.ReplaceProvider("gcp", map[pricing.Request]pricing.Estimate{key: {Currency: "USD", Hourly: 9, Source: "last-good", ObservedAt: time.Now(), StaleAfter: time.Hour}})
	err := (GCPFeed{BaseURL: server.URL, Client: server.Client(), RequestLimit: 2}).Refresh(context.Background(), catalog)
	if err == nil || !strings.Contains(err.Error(), "repeated page token") || calls.Load() != 2 {
		t.Fatalf("unexpected bounded pagination result: calls=%d err=%v", calls.Load(), err)
	}
	estimate, lookupErr := catalog.Estimate(context.Background(), key)
	if lookupErr != nil || estimate.Source != "last-good" {
		t.Fatalf("failed GCP refresh replaced last good: %#v err=%v", estimate, lookupErr)
	}
}

func TestGCPFeedBoundsRequestsAndResponseBodies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, strings.Repeat("x", maximumGCPResponseBytes+1))
	}))
	defer server.Close()
	err := (GCPFeed{BaseURL: server.URL, Client: server.Client()}).Refresh(context.Background(), pricing.NewDynamicCatalog(nil))
	if err == nil || !strings.Contains(err.Error(), "body limit") {
		t.Fatalf("expected bounded GCP body error, got %v", err)
	}

	calls := 0
	paged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		writeGCPPage(t, w, nil, fmt.Sprintf("page-%d", calls))
	}))
	defer paged.Close()
	err = (GCPFeed{BaseURL: paged.URL, Client: paged.Client(), RequestLimit: 2}).Refresh(context.Background(), pricing.NewDynamicCatalog(nil))
	if err == nil || !strings.Contains(err.Error(), "2-request") || calls != 2 {
		t.Fatalf("GCP request budget was not enforced: calls=%d err=%v", calls, err)
	}
}

func TestGCPFeedRequiresAndProtectsOfficialAPIKey(t *testing.T) {
	if err := (GCPFeed{}).Refresh(context.Background(), pricing.NewDynamicCatalog(nil)); err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Fatalf("missing official GCP API key was accepted: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("credential-bearing request reached an untrusted endpoint")
	}))
	defer server.Close()
	if err := (GCPFeed{BaseURL: server.URL, APIKey: "secret", Client: server.Client()}).Refresh(context.Background(), pricing.NewDynamicCatalog(nil)); err == nil || !strings.Contains(err.Error(), "untrusted endpoint") {
		t.Fatalf("GCP API key was allowed for a foreign origin: %v", err)
	}
}

func TestGCPFeedRejectsRedirectsOutsideConfiguredOrigin(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGCPPage(t, w, nil, "")
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	err := (GCPFeed{BaseURL: origin.URL, Client: origin.Client()}).Refresh(context.Background(), pricing.NewDynamicCatalog(nil))
	if err == nil || !strings.Contains(err.Error(), "redirected outside") {
		t.Fatalf("foreign GCP redirect accepted: %v", err)
	}
}

func TestReviewedGCPGPUDescriptionsAreExact(t *testing.T) {
	for description, want := range map[string]string{
		"Nvidia L4 GPU running in Netherlands":                "nvidia-l4",
		"Nvidia Tesla A100 80GB GPU running in Americas":      "nvidia-a100-80gb",
		"Nvidia H100 80GB Mega GPU running in Frankfurt":      "nvidia-h100-mega-80gb",
		"H200 141GB GPU running in Belgium":                   "nvidia-h200-141gb",
		"A4 Nvidia B200 (1 gpu slice) running in Netherlands": "nvidia-b200",
		"RTX 6000 96GB running in Oregon":                     "nvidia-rtx-pro-6000",
	} {
		if got, ok := reviewedGCPGPU(description); !ok || got != want {
			t.Errorf("reviewedGCPGPU(%q)=(%q,%t), want %q", description, got, ok, want)
		}
	}
	for _, description := range []string{
		"Nvidia L40 GPU running in Netherlands",
		"Nvidia L4 Virtual Workstation GPU running in Netherlands",
		"Nvidia L4 GPU attached to Spot Preemptible VMs running in Netherlands",
		"Commitment v1: Nvidia L4 GPU running in Netherlands for 1 Year",
	} {
		if gpu, ok := reviewedGCPGPU(description); ok {
			t.Errorf("unreviewed GCP description %q mapped to %q", description, gpu)
		}
	}
}

func TestGCPPriceDoesNotFallBackPastAnUnsupportedCurrentRate(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	old := gcpSKU("1111-2222-3333", "Nvidia L4 GPU running in Netherlands", []string{"europe-west4"}, 0, 500_000_000, now.Add(-24*time.Hour)).PricingInfo[0]
	current := gcpSKU("1111-2222-3333", "Nvidia L4 GPU running in Netherlands", []string{"europe-west4"}, 0, 600_000_000, now.Add(-time.Hour)).PricingInfo[0]
	current.PricingExpression.UsageUnit = "GiBy.mo"
	if price, effective, ok := currentGCPHourlyPrice([]gcpPricingInfo{old, current}, now); ok {
		t.Fatalf("unsupported latest GCP rate fell back to an old price: price=%f effective=%s", price, effective)
	}
}

func gcpSKU(id, description string, regions []string, units int64, nanos int64, effective time.Time) gcpCatalogSKU {
	sku := gcpCatalogSKU{
		Name: "services/" + gcpComputeEngineServiceID + "/skus/" + id, SKUID: id, Description: description,
		ServiceRegions: regions, ServiceProviderName: "Google",
		Category: gcpSKUCategory{ServiceDisplayName: "Compute Engine", ResourceFamily: "Compute", ResourceGroup: "GPU", UsageType: "OnDemand"},
		PricingInfo: []gcpPricingInfo{{
			EffectiveTime: effective.UTC().Format(time.RFC3339Nano),
			PricingExpression: gcpPricingExpression{UsageUnit: "h", DisplayQuantity: 1, TieredRates: []gcpTierRate{{
				UnitPrice: gcpMoney{CurrencyCode: "USD", Units: fmt.Sprintf("%d", units), Nanos: nanos},
			}}},
		}},
	}
	sku.GeoTaxonomy.Type = "REGIONAL"
	sku.GeoTaxonomy.Regions = append([]string(nil), regions...)
	return sku
}

func writeGCPPage(t *testing.T, w http.ResponseWriter, skus []gcpCatalogSKU, next string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(gcpCatalogPage{SKUs: skus, NextPageToken: next}); err != nil {
		t.Error(err)
	}
}
