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

func telemetryCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 3 || args[0] != "collect" || args[1] != "dcgm" {
		return errors.New("usage: infercrane telemetry collect dcgm DEPLOYMENT [--url URL | --file PATH] [--selector key=value]")
	}
	deployment := args[2]
	fs := flag.NewFlagSet("telemetry collect dcgm", flag.ContinueOnError)
	collectorURL := fs.String("url", "http://127.0.0.1:9400/metrics", "DCGM Prometheus metrics URL")
	file := fs.String("file", "", "saved DCGM Prometheus snapshot")
	selectorText := fs.String("selector", "", "comma-separated exact label selectors")
	replicaID := fs.String("replica", "", "InferCrane replica identity")
	source := fs.String("source", "dcgm_exporter", "evidence source")
	utilizationUnit := fs.String("utilization-unit", "percent", "percent or ratio")
	ttl := fs.Duration("ttl", 2*time.Minute, "evidence freshness TTL")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[3:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || deployment == "" {
		return errors.New("usage: infercrane telemetry collect dcgm DEPLOYMENT [--url URL | --file PATH]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	selector, err := parseMetricSelector(*selectorText)
	if err != nil {
		return err
	}
	payload, err := readCollectorPayload(ctx, *collectorURL, *file)
	if err != nil {
		return err
	}
	observedAt := time.Now().UTC()
	rows, err := metrics.ParseDCGM(string(payload), metrics.DCGMOptions{Selector: selector, ReplicaID: *replicaID, Source: *source, UtilizationUnit: *utilizationUnit, ObservedAt: observedAt, TTL: *ttl})
	if err != nil {
		return err
	}
	values := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		values = append(values, map[string]any{"name": row.Name, "value": row.Value, "unit": row.Unit, "sample_count": row.SampleCount})
	}
	body := map[string]any{"source": *source, "evidence_class": "measured", "replica_id": *replicaID, "observed_at": observedAt, "valid_until": observedAt.Add(*ttl), "measurements": values}
	var response struct {
		Data            []domain.OperationalMeasurement `json:"data"`
		ContentRecorded bool                            `json:"content_recorded"`
	}
	path := "/api/v1/deployments/" + url.PathEscape(deployment) + "/measurements"
	if err = controlJSON(ctx, cfg, http.MethodPost, path, "", body, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Hardware evidence recorded for %s\n\n", deployment)
	for _, row := range response.Data {
		fmt.Printf("  %-20s %10.2f %-10s samples %d\n", row.Name, row.Value, row.Unit, row.SampleCount)
	}
	fmt.Printf("\nFresh until %s · source %s · prompt/output content not recorded\n", observedAt.Add(*ttl).Format(time.RFC3339), *source)
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
		return nil, fmt.Errorf("read DCGM exporter: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DCGM exporter returned HTTP %d", response.StatusCode)
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
