package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/metrics"
)

const maxCollectorPayload = 8 << 20

const telemetryUsage = "usage: infercrane telemetry collect dcgm|amd DEPLOYMENT [--url URL | --file PATH] [--selector key=value] [--interval 15s]"

// telemetryCommand imports bounded snapshots from a GPU metrics exporter:
// NVIDIA's dcgm-exporter (dcgm) or the AMD Device Metrics Exporter (amd).
func telemetryCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 3 || args[0] != "collect" || (args[1] != "dcgm" && args[1] != "amd") {
		return errors.New(telemetryUsage)
	}
	collector, deployment := args[1], args[2]
	fs := newFlagSet("telemetry collect " + collector)
	defaultURL, defaultSource := "http://127.0.0.1:9400/metrics", "dcgm_exporter"
	if collector == "amd" {
		defaultURL, defaultSource = "http://127.0.0.1:5000/metrics", "amd_device_metrics_exporter"
	}
	collectorURL := fs.String("url", defaultURL, "exporter Prometheus metrics URL")
	file := fs.String("file", "", "saved exporter Prometheus snapshot")
	selectorText := fs.String("selector", "", "comma-separated exact label selectors")
	replicaID := fs.String("replica", "", "InferCrane replica identity")
	source := fs.String("source", defaultSource, "evidence source")
	ttl := fs.Duration("ttl", 2*time.Minute, "evidence freshness TTL")
	interval := fs.Duration("interval", 0, "keep collecting at this interval (5s to 10m) until stopped; the TTL defaults to three intervals")
	output := fs.String("output", "human", "human or json")
	var utilizationUnit, metricPrefix *string
	var nvlinkPeak, pciePeak *float64
	if collector == "dcgm" {
		utilizationUnit = fs.String("utilization-unit", "percent", "percent or ratio")
		nvlinkPeak = fs.Float64("nvlink-peak-gbps", 0, "declared per-GPU, per-direction NVLink peak in GB/s")
		pciePeak = fs.Float64("pcie-peak-gbps", 0, "declared per-GPU, per-direction PCIe peak in GB/s")
	} else {
		metricPrefix = fs.String("metric-prefix", "", "the exporter's MetricsFieldPrefix, if it sets one")
	}
	if err := fs.Parse(args[3:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || deployment == "" {
		return errors.New(telemetryUsage)
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	selector, err := parseMetricSelector(*selectorText)
	if err != nil {
		return err
	}
	if collector == "dcgm" && (*nvlinkPeak < 0 || *pciePeak < 0) {
		return errors.New("--nvlink-peak-gbps and --pcie-peak-gbps must not be negative")
	}
	if *interval != 0 {
		if *interval < 5*time.Second || *interval > 10*time.Minute {
			return errors.New("--interval must be 5s to 10m")
		}
		if *file != "" {
			return errors.New("--interval reads --url; a saved --file does not change")
		}
		ttlSet := false
		fs.Visit(func(f *flag.Flag) { ttlSet = ttlSet || f.Name == "ttl" })
		if !ttlSet {
			*ttl = 3 * *interval
		}
	}
	parse := func(payload string, observedAt time.Time) ([]domain.OperationalMeasurement, error) {
		if collector == "amd" {
			return metrics.ParseAMDDeviceMetrics(payload, metrics.AMDDeviceMetricsOptions{Selector: selector, ReplicaID: *replicaID, Source: *source, ObservedAt: observedAt, TTL: *ttl, MetricPrefix: *metricPrefix})
		}
		return metrics.ParseDCGM(payload, metrics.DCGMOptions{Selector: selector, ReplicaID: *replicaID, Source: *source, UtilizationUnit: *utilizationUnit, ObservedAt: observedAt, TTL: *ttl, NVLinkPeakBytesPerSecond: *nvlinkPeak * 1e9, PCIePeakBytesPerSecond: *pciePeak * 1e9})
	}
	collect := func() error {
		return collectHardwareOnce(ctx, cfg, deployment, *collectorURL, *file, *source, *replicaID, *output, *ttl, parse)
	}
	if *interval == 0 {
		return collect()
	}
	// Continuous collection: a failed scrape or post is reported and retried
	// at the next interval; the evidence goes stale on its own after its TTL.
	for {
		if err = collect(); err != nil {
			fmt.Fprintf(os.Stderr, "%s collection failed: %v\n", time.Now().UTC().Format(time.RFC3339), err)
		}
		if err = telemetrySleep(ctx, *interval); err != nil {
			return nil
		}
	}
}

// telemetrySleep waits between continuous collections. Tests replace it.
var telemetrySleep = func(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// collectHardwareOnce reads one exporter snapshot, parses it with the
// collector's bounded parser, and records it against the deployment.
func collectHardwareOnce(ctx context.Context, cfg config.Config, deployment, collectorURL, file, source, replicaID, output string, ttl time.Duration, parse func(string, time.Time) ([]domain.OperationalMeasurement, error)) error {
	payload, err := readCollectorPayload(ctx, collectorURL, file)
	if err != nil {
		return err
	}
	observedAt := time.Now().UTC()
	rows, err := parse(string(payload), observedAt)
	if err != nil {
		return err
	}
	values := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		values = append(values, map[string]any{"name": row.Name, "value": row.Value, "unit": row.Unit, "sample_count": row.SampleCount})
	}
	body := map[string]any{"source": source, "evidence_class": "measured", "replica_id": replicaID, "observed_at": observedAt, "valid_until": observedAt.Add(ttl), "measurements": values}
	var response struct {
		Data            []domain.OperationalMeasurement `json:"data"`
		ContentRecorded bool                            `json:"content_recorded"`
	}
	path := "/api/v1/deployments/" + url.PathEscape(deployment) + "/measurements"
	if err = controlJSON(ctx, cfg, http.MethodPost, path, "", body, &response); err != nil {
		return err
	}
	if output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Hardware evidence recorded for %s\n\n", deployment)
	for _, row := range response.Data {
		fmt.Printf("  %-20s %10.2f %-10s samples %d\n", row.Name, row.Value, row.Unit, row.SampleCount)
	}
	fmt.Printf("\nFresh until %s · source %s · prompt/output content not recorded\n", observedAt.Add(ttl).Format(time.RFC3339), source)
	return nil
}

func parseMetricSelector(raw string) (map[string]string, error) {
	result := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return result, nil
	}
	for _, part := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" || len(key) > 64 || len(value) > 256 {
			return nil, errors.New("--selector must be comma-separated key=value pairs")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("--selector repeats %q", key)
		}
		result[key] = value
	}
	return result, nil
}

func readCollectorPayload(ctx context.Context, rawURL, file string) ([]byte, error) {
	if file != "" {
		info, err := os.Stat(file)
		if err != nil {
			return nil, err
		}
		if info.Size() > maxCollectorPayload {
			return nil, errors.New("collector snapshot exceeds 8 MiB")
		}
		return os.ReadFile(file)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("--url must be an HTTP(S) URL without embedded credentials")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read GPU metrics exporter: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GPU metrics exporter returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxCollectorPayload+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCollectorPayload {
		return nil, errors.New("collector snapshot exceeds 8 MiB")
	}
	return data, nil
}
