package priceingest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRunPodFeedUsesExactSecureCloudProviderPrice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "gpuCount: 1, secureCloud: true") {
			t.Fatalf("price query did not match the secure launch lane: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"gpuTypes":[{"id":"NVIDIA L40S","displayName":"L40S","lowestPrice":{"uninterruptablePrice":0.74}},{"id":"NVIDIA H100","displayName":"H100","lowestPrice":{"uninterruptablePrice":1.99}}]}}`))
	}))
	defer server.Close()
	now := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	catalog := pricing.NewDynamicCatalog(nil)
	feed := RunPodFeed{BaseURL: server.URL, Client: server.Client(), ValidFor: 2 * time.Minute, Now: func() time.Time { return now }}
	if err := feed.Refresh(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	l40s, err := catalog.Estimate(context.Background(), pricing.Request{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1})
	if err != nil || l40s.Hourly != 0.74 || l40s.ObservedAt != now {
		t.Fatalf("unexpected live L40S offer: %#v err=%v", l40s, err)
	}
	h100, err := catalog.Estimate(context.Background(), pricing.Request{Cloud: "runpod", Region: "global", GPU: "NVIDIA H100", GPUCount: 1, Replicas: 1})
	if err != nil || h100.Hourly != 1.99 {
		t.Fatalf("unexpected H100 offer: %#v err=%v", h100, err)
	}
}

func TestRunPodFeedDoesNotReplaceLastGoodSnapshotOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	catalog := pricing.NewDynamicCatalog(nil)
	key := pricing.Request{Cloud: "runpod", Region: "global", GPU: "L40S", GPUCount: 1, Replicas: 1}
	catalog.ReplaceProvider("runpod", map[pricing.Request]pricing.Estimate{key: {Hourly: 1, Currency: "USD", Source: "last-good", ObservedAt: time.Now(), StaleAfter: time.Minute}})
	if err := (RunPodFeed{BaseURL: server.URL, Client: server.Client()}).Refresh(context.Background(), catalog); err == nil {
		t.Fatal("expected provider transport error")
	}
	offer, err := catalog.Estimate(context.Background(), key)
	if err != nil || offer.Source != "last-good" {
		t.Fatalf("last good snapshot was lost: %#v err=%v", offer, err)
	}
}

func TestRunPodFeedNeverSendsCredentialToCustomEndpoint(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("untrusted endpoint must not receive a request")
	}))
	defer server.Close()
	err := (RunPodFeed{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).Refresh(context.Background(), pricing.NewDynamicCatalog(nil))
	if err == nil || !strings.Contains(err.Error(), "untrusted endpoint") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe credential-host result: %v", err)
	}
}

func TestRunPodFeedRevalidatesCredentialAgainstTrustedProviderEndpoint(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.String() != defaultRunPodGraphQLURL || request.Header.Get("Authorization") != "Bearer tenant-secret" {
			t.Fatalf("unexpected provider request: url=%s authorization=%q", request.URL, request.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"data":{"gpuTypes":[{"id":"NVIDIA L40S","lowestPrice":{"uninterruptablePrice":0.74}}]}}`))}, nil
	})}
	catalog := pricing.NewDynamicCatalog(nil)
	if err := (RunPodFeed{APIKey: "tenant-secret", Client: client}).Refresh(t.Context(), catalog); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("expected one credential revalidation request, got %d", requests)
	}
}

func TestRunPodFeedRejectsInvalidCredentialWithoutLeakingIt(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`forbidden tenant-secret`))}, nil
	})}
	err := (RunPodFeed{APIKey: "tenant-secret", Client: client}).Refresh(t.Context(), pricing.NewDynamicCatalog(nil))
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), "tenant-secret") {
		t.Fatalf("invalid credential result was not safe: %v", err)
	}
}

func TestProviderFeedJitterStaysWithinTenPercent(t *testing.T) {
	for range 100 {
		got := jittered(time.Minute)
		if got < 54*time.Second || got >= 66*time.Second {
			t.Fatalf("jitter outside expected range: %s", got)
		}
	}
}
