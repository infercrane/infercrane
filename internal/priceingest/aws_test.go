package priceingest

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

var awsTestColumns = []string{
	"SKU", "TermType", "PricePerUnit", "Currency", "Product Family", "Instance Type", "Tenancy",
	"Operating System", "Pre Installed S/W", "operation", "CapacityStatus", "Region Code", "GPU",
}

func awsCSV(t *testing.T, rows ...[]string) string {
	t.Helper()
	var output strings.Builder
	writer := csv.NewWriter(&output)
	_ = writer.Write([]string{"FormatVersion", "v1.0"})
	_ = writer.Write([]string{"Publication Date", "2026-09-01T00:00:00Z"})
	_ = writer.Write(awsTestColumns)
	for _, row := range rows {
		_ = writer.Write(row)
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func awsRow(instanceType, gpuCount, hourly string) []string {
	return []string{"sku", "OnDemand", hourly, "USD", "Compute Instance", instanceType, "Shared", "Linux", "NA", "RunInstances", "Used", "us-east-1", gpuCount}
}

func TestAWSFeedReadsOfficialOnDemandGPUTuples(t *testing.T) {
	var csvRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case awsRegionIndexPath:
			_, _ = fmt.Fprint(w, `{"regions":{"us-east-1":{"currentVersionUrl":"/offers/v1.0/aws/AmazonEC2/20260901000000/us-east-1/index.json"}}}`)
		case "/offers/v1.0/aws/AmazonEC2/20260901000000/us-east-1/index.csv":
			csvRequests.Add(1)
			windows := awsRow("g5.xlarge", "1", "0.50")
			windows[7] = "Windows"
			dedicated := awsRow("g5.xlarge", "1", "0.60")
			dedicated[6] = "Dedicated"
			reserved := awsRow("g5.xlarge", "1", "0.40")
			reserved[1] = "Reserved"
			_, _ = fmt.Fprint(w, awsCSV(t,
				awsRow("g6e.2xlarge", "1", "2.20"),
				awsRow("g6e.xlarge", "1", "2.00"), // cheapest exact GPU/count tuple wins
				awsRow("p5.48xlarge", "8", "98.32"),
				awsRow("g7e.24xlarge", "4", "15.00"),
				awsRow("g6f.large", "0.125", "0.12"), // not representable as an integer GPU count
				windows, dedicated, reserved,
			))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	now := time.Date(2026, 9, 1, 5, 0, 0, 0, time.UTC)
	catalog := pricing.NewDynamicCatalog(nil)
	feed := AWSFeed{BaseURL: server.URL, Regions: []string{"us-east-1"}, Client: server.Client(), MaxBodyBytes: 1 << 20, ValidFor: time.Hour, Now: func() time.Time { return now }}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	source := server.URL + "/offers/v1.0/aws/AmazonEC2/20260901000000/us-east-1/index.csv"
	for request, want := range map[pricing.Request]float64{
		{Cloud: "aws", Region: "us-east-1", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}:                                  2.00,
		{Cloud: "aws", Region: "us-east-1", GPU: "NVIDIA H100 80GB HBM3", GPUCount: 8, Replicas: 1}:                        98.32,
		{Cloud: "aws", Region: "us-east-1", GPU: "NVIDIA RTX PRO 6000 Blackwell Server Edition", GPUCount: 4, Replicas: 1}: 15.00,
	} {
		estimate, err := catalog.Estimate(context.Background(), request)
		if err != nil || estimate.Hourly != want || estimate.Currency != "USD" || estimate.Source != source || estimate.ObservedAt != now || estimate.GuaranteedUntil.IsZero() == false {
			t.Fatalf("unexpected AWS estimate for %#v: %#v err=%v", request, estimate, err)
		}
	}
	if len(catalog.Snapshot()) != 3 {
		t.Fatalf("non-current or fractional offers leaked into the catalog: %#v", catalog.Snapshot())
	}
	if csvRequests.Load() != 1 {
		t.Fatalf("expected one regional price download, got %d", csvRequests.Load())
	}

	// The versioned source is unchanged, so the next refresh verifies the tiny
	// region index and extends freshness without downloading the 300+ MB file.
	later := now.Add(30 * time.Minute)
	feed.Now = func() time.Time { return later }
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if csvRequests.Load() != 1 {
		t.Fatalf("unchanged AWS version was downloaded again: %d", csvRequests.Load())
	}
	refreshed, _ := catalog.Estimate(context.Background(), pricing.Request{Cloud: "aws", Region: "us-east-1", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1})
	if refreshed.ObservedAt != now || refreshed.ObservedAt.Add(refreshed.StaleAfter) != later.Add(time.Hour) {
		t.Fatalf("cached AWS version changed provenance or was not revalidated: %#v", refreshed)
	}
}

func TestAWSFeedFailurePreservesLastGoodSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == awsRegionIndexPath {
			_, _ = fmt.Fprint(w, `{"regions":{"us-east-1":{"currentVersionUrl":"/offers/v1.0/aws/AmazonEC2/next/us-east-1/index.json"}}}`)
			return
		}
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	key := pricing.Request{Cloud: "aws", Region: "us-east-1", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}
	catalog := pricing.NewDynamicCatalog(nil)
	catalog.ReplaceProvider("aws", map[pricing.Request]pricing.Estimate{key: {Currency: "USD", Hourly: 2, Source: "last-good", ObservedAt: time.Now(), StaleAfter: time.Hour}})
	err := (AWSFeed{BaseURL: server.URL, Client: server.Client(), MaxBodyBytes: 1 << 20}).Refresh(context.Background(), catalog)
	if err == nil {
		t.Fatal("expected AWS transport error")
	}
	estimate, estimateErr := catalog.Estimate(context.Background(), key)
	if estimateErr != nil || estimate.Source != "last-good" {
		t.Fatalf("last good AWS snapshot was lost: %#v err=%v refresh=%v", estimate, estimateErr, err)
	}
}

func TestAWSFeedRejectsOversizedRegionWithoutReplacingSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == awsRegionIndexPath {
			_, _ = fmt.Fprint(w, `{"regions":{"us-east-1":{"currentVersionUrl":"/offers/v1.0/aws/AmazonEC2/large/us-east-1/index.json"}}}`)
			return
		}
		_, _ = fmt.Fprint(w, awsCSV(t, awsRow("g6e.xlarge", "1", "2.00"))+strings.Repeat("x", 1024))
	}))
	defer server.Close()

	key := pricing.Request{Cloud: "aws", Region: "us-east-1", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}
	catalog := pricing.NewDynamicCatalog(nil)
	catalog.ReplaceProvider("aws", map[pricing.Request]pricing.Estimate{key: {Currency: "USD", Hourly: 3, Source: "last-good", ObservedAt: time.Now(), StaleAfter: time.Hour}})
	err := (AWSFeed{BaseURL: server.URL, Client: server.Client(), MaxBodyBytes: 128}).Refresh(context.Background(), catalog)
	if err == nil || !strings.Contains(err.Error(), "response limit") {
		t.Fatalf("expected bounded response error, got %v", err)
	}
	estimate, _ := catalog.Estimate(context.Background(), key)
	if estimate.Source != "last-good" {
		t.Fatalf("oversized response replaced the last good snapshot: %#v", estimate)
	}
}

func TestAWSFeedBoundsRegionsAndRejectsForeignCatalogURLs(t *testing.T) {
	if err := (AWSFeed{Regions: []string{"a", "b", "c", "d", "e"}}).Refresh(context.Background(), pricing.NewDynamicCatalog(nil)); err == nil || !strings.Contains(err.Error(), "limited to 4 regions") {
		t.Fatalf("expected region bound before network access, got %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"regions":{"us-east-1":{"currentVersionUrl":"https://example.com/offers/v1.0/aws/AmazonEC2/current/us-east-1/index.json"}}}`)
	}))
	defer server.Close()
	err := (AWSFeed{BaseURL: server.URL, Client: server.Client()}).Refresh(context.Background(), pricing.NewDynamicCatalog(nil))
	if err == nil || !strings.Contains(err.Error(), "outside the expected catalog") {
		t.Fatalf("expected foreign catalog URL rejection, got %v", err)
	}
}

func TestAWSGPUFamilyMappings(t *testing.T) {
	tests := map[string]string{
		"g4dn.xlarge":        "NVIDIA T4",
		"g5.48xlarge":        "NVIDIA A10G",
		"g6.48xlarge":        "NVIDIA L4",
		"g6e.48xlarge":       "NVIDIA L40S",
		"g7.12xlarge":        "NVIDIA RTX PRO 4500 Blackwell Server Edition",
		"p4de.24xlarge":      "NVIDIA A100-SXM4-80GB",
		"p5en.48xlarge":      "NVIDIA H200",
		"p6-b300.48xlarge":   "NVIDIA B300",
		"p6e-gb200.36xlarge": "NVIDIA B200",
		"trn2.48xlarge":      "",
	}
	for instanceType, want := range tests {
		if got := awsGPUModel(instanceType); got != want {
			t.Errorf("awsGPUModel(%q) = %q, want %q", instanceType, got, want)
		}
	}
}
