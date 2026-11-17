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
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Source != "dcgm_exporter" || body.ReplicaID != "replica-1" || len(body.Measurements) != 3 {
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
		"peak":        {"collect", "dcgm", "prod", "--nvlink-peak-gbps", "-1", "--file", "missing"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := telemetryCommand(context.Background(), cfg, args); err == nil {
				t.Fatal("unsafe telemetry command was accepted")
			}
		})
	}
}

func TestTelemetryCollectDCGMDeclaresLinkPeaks(t *testing.T) {
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `DCGM_FI_PROF_NVLINK_TX_BYTES{gpu="0",modelName="NVIDIA H100 80GB HBM3"} 360000000000
DCGM_FI_PROF_NVLINK_RX_BYTES{gpu="0",modelName="NVIDIA H100 80GB HBM3"} 90000000000
DCGM_FI_PROF_NVLINK_TX_BYTES{gpu="1",modelName="NVIDIA H100 80GB HBM3"} 120000000000`)
	}))
	defer exporter.Close()
	var saturation, devices float64
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Measurements []struct {
				Name  string  `json:"name"`
				Value float64 `json:"value"`
			} `json:"measurements"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for _, measurement := range body.Measurements {
			switch measurement.Name {
			case "gpu_nvlink_saturation":
				saturation = measurement.Value
			case "gpu_devices":
				devices = measurement.Value
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[],"content_recorded":false}`)
	}))
	defer control.Close()
	_, err := captureStdout(t, func() error {
		return telemetryCommand(context.Background(), config.Config{ControlURL: control.URL, APIKey: "secret"}, []string{"collect", "dcgm", "coder-production", "--url", exporter.URL + "/metrics", "--nvlink-peak-gbps", "450"})
	})
	if err != nil || saturation != 80 || devices != 2 {
		t.Fatalf("saturation=%v devices=%v err=%v", saturation, devices, err)
	}
}

func TestTelemetryCollectAMDImportsTheExporterSnapshot(t *testing.T) {
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "gpu_gfx_activity{serial_number=\"s1\",pod=\"qwen-0\"} 64\ngpu_package_power{serial_number=\"s1\",pod=\"qwen-0\"} 512\ngpu_gfx_activity{serial_number=\"s2\",pod=\"other\"} 99\n")
	}))
	defer exporter.Close()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Source       string `json:"source"`
			Measurements []struct {
				Name  string  `json:"name"`
				Value float64 `json:"value"`
			} `json:"measurements"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.URL.Path != "/api/v1/deployments/qwen-mi300x/measurements" || body.Source != "amd_device_metrics_exporter" || len(body.Measurements) != 3 {
			t.Fatalf("path=%s body=%+v err=%v", r.URL.Path, body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"name":"gpu_utilization","value":64,"unit":"percent","sample_count":1}],"content_recorded":false}`)
	}))
	defer control.Close()
	output, err := captureStdout(t, func() error {
		return telemetryCommand(context.Background(), config.Config{ControlURL: control.URL, APIKey: "secret"}, []string{"collect", "amd", "qwen-mi300x", "--url", exporter.URL + "/metrics", "--selector", "pod=qwen-0"})
	})
	if err != nil || !strings.Contains(output, "amd_device_metrics_exporter") {
		t.Fatalf("output=%q err=%v", output, err)
	}
	// NVIDIA-only flags are not accepted for the AMD exporter, and an unknown
	// collector is refused.
	cfg := config.Config{ControlURL: "http://127.0.0.1:1", APIKey: "secret"}
	for _, args := range [][]string{
		{"collect", "amd", "prod", "--nvlink-peak-gbps", "450", "--file", "missing"},
		{"collect", "rocm-smi", "prod"},
	} {
		if err := telemetryCommand(context.Background(), cfg, args); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
}

func TestTelemetryCollectDCGMKeepsCollectingAtAnInterval(t *testing.T) {
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `DCGM_FI_PROF_SM_ACTIVE{gpu="0",UUID="GPU-1"} 0.5`)
	}))
	defer exporter.Close()
	var posts []time.Duration
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ObservedAt time.Time `json:"observed_at"`
			ValidUntil time.Time `json:"valid_until"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		posts = append(posts, body.ValidUntil.Sub(body.ObservedAt))
		_, _ = io.WriteString(w, `{"data":[],"content_recorded":false}`)
	}))
	defer control.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previous := telemetrySleep
	defer func() { telemetrySleep = previous }()
	waits := 0
	telemetrySleep = func(ctx context.Context, wait time.Duration) error {
		if wait != 15*time.Second {
			t.Fatalf("waited %s", wait)
		}
		if waits++; waits == 3 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	_, err := captureStdout(t, func() error {
		return telemetryCommand(ctx, config.Config{ControlURL: control.URL, APIKey: "secret"}, []string{"collect", "dcgm", "coder", "--url", exporter.URL + "/metrics", "--interval", "15s", "--output", "json"})
	})
	if err != nil || len(posts) != 3 || posts[0] != 45*time.Second {
		t.Fatalf("posts=%v err=%v", posts, err)
	}
	for name, args := range map[string][]string{
		"too often": {"collect", "dcgm", "coder", "--interval", "1s"},
		"file":      {"collect", "dcgm", "coder", "--interval", "15s", "--file", "dcgm.prom"},
	} {
		if err = telemetryCommand(context.Background(), config.Config{}, args); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
