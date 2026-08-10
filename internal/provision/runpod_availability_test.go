package provision

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunPodAvailabilityAggregatesCompatibleGPUStock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.RawQuery != "" {
			t.Fatal("missing API key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"gpuTypes":[{"id":"NVIDIA H100 PCIe","displayName":"H100 PCIe","lowestPrice":{"stockStatus":"Low","availableGpuCounts":[1]}},{"id":"NVIDIA H100 80GB HBM3","displayName":"H100 SXM","lowestPrice":{"stockStatus":"High","availableGpuCounts":[1]}}]}}`))
	}))
	defer server.Close()

	availability, err := (RunPodAvailability{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).Availability(context.Background(), AvailabilityRequest{GPU: "H100", Count: 1})
	if err != nil || availability.State != "available" || !strings.Contains(availability.Message, "high stock") || !strings.Contains(availability.Details, "H100 PCIe:Low") {
		t.Fatalf("availability=%+v err=%v", availability, err)
	}
}

func TestRunPodAvailabilityReportsNoStockAndRegionCaveat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"gpuTypes":[{"id":"NVIDIA L40S","displayName":"L40S","lowestPrice":null}]}}`))
	}))
	defer server.Close()

	availability, err := (RunPodAvailability{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).Availability(context.Background(), AvailabilityRequest{GPU: "L40S", Region: "US", Count: 1})
	if err != nil || availability.State != "unavailable" || !strings.Contains(availability.Message, "does not guarantee region US") {
		t.Fatalf("availability=%+v err=%v", availability, err)
	}
}

func TestRunPodAvailabilityDoesNotExposeCredentialInTransportError(t *testing.T) {
	_, err := (RunPodAvailability{APIKey: "never-persist-this", BaseURL: "://invalid"}).Availability(context.Background(), AvailabilityRequest{GPU: "L40S"})
	if err == nil || strings.Contains(err.Error(), "never-persist-this") {
		t.Fatalf("unsafe error: %v", err)
	}
}
