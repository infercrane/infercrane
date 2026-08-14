package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
)

func TestObserveEndpointIncludesBoundedMonitoringEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing control-plane authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/endpoints/coder-production":
			_, _ = io.WriteString(w, `{"endpoint":{"name":"coder-production","observed_state":"serving"},"logical_model":{"name":"coder"},"environment":{"name":"production"},"active_plan":{"id":"plan-1","routing_policy":"primary-fallback"},"bindings":[]}`)
		case "/api/v1/endpoints/coder-production/monitoring":
			if r.URL.Query().Get("window_seconds") != "3600" {
				t.Fatalf("monitoring query=%s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"endpoint":"coder-production","logical_model":"coder","environment":"production","window_start":"2026-08-14T10:00:00Z","window_end":"2026-08-14T11:00:00Z","bucket_seconds":60,"summary":{"requests":120,"errors":2,"fallbacks":4,"requests_per_second":2,"output_tokens_per_second":450,"error_rate":0.0166666667,"fallback_rate":0.0333333333,"p95_latency_ms":810,"p95_ttft_ms":220},"series":[],"breakdowns":[],"events":[],"evidence":{"source":"infercrane_gateway_request_records","semantic_convention_schema":"https://opentelemetry.io/schemas/gen-ai/1.42.0","sample_count":120,"fresh":true,"content_recorded":false,"available":[],"unavailable":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"not configured"}}`)
		}
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return observeCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"coder-production"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{"TRAFFIC", "120 (2.00/s)", "1.67%", "3.33%", "220.0ms", "fresh · 120 samples"} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("output=%q missing %q", output, wanted)
		}
	}
}
