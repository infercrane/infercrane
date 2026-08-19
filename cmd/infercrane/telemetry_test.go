package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/config"
)

func TestTelemetryCollectDCGMUsesBoundedControlPlaneEvidencePath(t *testing.T) {
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/metrics" {
			t.Fatalf("exporter request=%s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `DCGM_FI_DEV_GPU_UTIL{deployment="coder",gpu="0"} 72
DCGM_FI_DEV_FB_USED{deployment="coder",gpu="0"} 1024`)
	}))
	defer exporter.Close()

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/deployments/coder-production/measurements" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("control request=%s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var body struct {
			Source       string `json:"source"`
			ReplicaID    string `json:"replica_id"`
			Measurements []struct {
				Name string `json:"name"`
			} `json:"measurements"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Source != "dcgm_exporter" || body.ReplicaID != "replica-1" || len(body.Measurements) != 2 {
			t.Fatalf("body=%+v err=%v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"name":"gpu_utilization","value":72,"unit":"percent","sample_count":1}],"content_recorded":false}`)
	}))
	defer control.Close()

	output, err := captureStdout(t, func() error {
		return telemetryCommand(context.Background(), config.Config{ControlURL: control.URL, APIKey: "secret"}, []string{"collect", "dcgm", "coder-production", "--url", exporter.URL + "/metrics", "--selector", "deployment=coder", "--replica", "replica-1", "--ttl", "2m"})
	})
	if err != nil || !strings.Contains(output, "gpu_utilization") || !strings.Contains(output, "prompt/output content not recorded") {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestTelemetryCollectDCGMRejectsUnsafeInputsBeforeControlPlane(t *testing.T) {
	cfg := config.Config{ControlURL: "http://127.0.0.1:1", APIKey: "secret"}
	for name, args := range map[string][]string{
		"credentials": {"collect", "dcgm", "prod", "--url", "http://user:secret@example.test/metrics", "--ttl", time.Minute.String()},
		"selector":    {"collect", "dcgm", "prod", "--selector", "deployment", "--ttl", time.Minute.String()},
		"unit":        {"collect", "dcgm", "prod", "--utilization-unit", "guess", "--file", "missing"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := telemetryCommand(context.Background(), cfg, args); err == nil {
				t.Fatal("unsafe telemetry command was accepted")
			}
		})
	}
}
