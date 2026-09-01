package provision

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunPodAvailabilityUsesExactReviewedGPUIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.RawQuery != "" {
			t.Fatal("missing API key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"gpuTypes":[{"id":"NVIDIA H100 PCIe","displayName":"H100 PCIe","lowestPrice":{"stockStatus":"High","availableGpuCounts":[1]}},{"id":"NVIDIA H100 80GB HBM3","displayName":"H100 SXM","lowestPrice":{"stockStatus":"Low","availableGpuCounts":[1]}}]}}`))
	}))
	defer server.Close()

	availability, err := (RunPodAvailability{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).Availability(context.Background(), AvailabilityRequest{GPU: "H100", Count: 1})
	if err != nil || availability.State != "constrained" || !strings.Contains(availability.Message, "low current secure capacity") || !strings.Contains(availability.Details, "H100 SXM:Low") || strings.Contains(availability.Details, "H100 PCIe") {
		t.Fatalf("availability=%+v err=%v", availability, err)
	}
}

func TestRunPodAvailabilityDoesNotUseDifferentGPUVariant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"gpuTypes":[{"id":"NVIDIA H100 PCIe","displayName":"H100 PCIe","lowestPrice":{"stockStatus":"High","availableGpuCounts":[1]}}]}}`))
	}))
	defer server.Close()

	availability, err := (RunPodAvailability{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).Availability(context.Background(), AvailabilityRequest{GPU: "H100", Count: 1})
	if err != nil || availability.State != "unknown" || availability.Details != "" {
		t.Fatalf("different GPU variant affected availability: availability=%+v err=%v", availability, err)
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

func TestRunPodAvailabilityRedactsAndBoundsGraphQLError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"credential secret ` + strings.Repeat("x", 16<<10) + `"}]}`))
	}))
	defer server.Close()
	_, err := (RunPodAvailability{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).Availability(context.Background(), AvailabilityRequest{GPU: "L40S"})
	if err == nil || strings.Contains(err.Error(), "secret") || len(err.Error()) > 5000 {
		t.Fatalf("unsafe GraphQL error length=%d err=%v", len(err.Error()), err)
	}
}

func TestRunPodLaunchProbeKeepsDeployabilityUnknownWithoutQuota(t *testing.T) {
	for _, test := range []struct {
		stock             string
		wantAvailability  string
		wantDeployability string
	}{
		{stock: "High", wantAvailability: "available", wantDeployability: "unknown"},
		{stock: "Low", wantAvailability: "constrained", wantDeployability: "unknown"},
		{stock: "None", wantAvailability: "unavailable", wantDeployability: "unavailable"},
	} {
		t.Run(test.stock, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"gpuTypes":[{"id":"NVIDIA L40S","displayName":"L40S","lowestPrice":{"stockStatus":"` + test.stock + `","availableGpuCounts":[1]}}]}}`))
			}))
			defer server.Close()

			evidence, err := (RunPodAvailability{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).ProbeLaunch(context.Background(), LaunchProbeRequest{Provider: "runpod", Region: "EU-RO-1", GPU: "L40S", GPUCount: 1})
			if err != nil || evidence.ConnectionState != "configured" || evidence.AvailabilityState != test.wantAvailability || evidence.QuotaState != "unknown" || evidence.Deployability != test.wantDeployability || evidence.Source == "" || evidence.ExpiresAt.IsZero() {
				t.Fatalf("evidence=%+v err=%v", evidence, err)
			}
		})
	}
}

func TestRunPodLaunchProbeFailsClosedWithoutCredentials(t *testing.T) {
	evidence, err := (RunPodAvailability{}).ProbeLaunch(context.Background(), LaunchProbeRequest{Provider: "runpod", GPU: "H100", GPUCount: 1})
	if err != nil || evidence.ConnectionState != "connection-required" || evidence.AvailabilityState != "unknown" || evidence.Deployability != "unknown" {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
}
