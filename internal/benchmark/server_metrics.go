package benchmark

import (
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	serverMetricsExportName  = "server_metrics_export.json"
	maxServerMetricsBytes    = 16 << 20
	maxServerMetricSummaries = 32
)

// serverMetricKeywords select the runtime signals that explain a serving
// result: cache occupancy and reuse, preemption, queueing, and speculative
// acceptance. They are generic name fragments, so no runtime is special-cased
// and an unknown runtime simply contributes fewer metrics.
var serverMetricKeywords = []string{"cache", "preempt", "spec", "accept", "running", "waiting", "queue", "token_usage", "swap"}

var serverMetricStats = map[string]struct{}{
	"avg": {}, "min": {}, "max": {}, "p50": {}, "p95": {}, "p99": {},
	"total": {}, "rate": {}, "count": {}, "sum": {},
}

type serverMetricsExport struct {
	Metrics map[string]struct {
		Type   string `json:"type"`
		Unit   string `json:"unit"`
		Series []struct {
			Stats map[string]json.RawMessage `json:"stats"`
		} `json:"series"`
	} `json:"metrics"`
}

// readServerMetrics summarizes the export AIPerf writes when server-metrics
// collection is on. It never fails the benchmark: the request records are the
// measurement, and server metrics are supporting evidence.
func readServerMetrics(directory string) *ServerMetrics {
	var paths []string
	_ = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == serverMetricsExportName {
			paths = append(paths, path)
		}
		return nil
	})
	if len(paths) == 0 {
		return &ServerMetrics{Reason: "AIPerf wrote no server-metrics export; the runtime metrics endpoint may be unreachable"}
	}
	sort.Strings(paths)
	file, err := os.Open(paths[0])
	if err != nil {
		return &ServerMetrics{Reason: "server-metrics export is unreadable"}
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxServerMetricsBytes+1))
	if err != nil || len(body) > maxServerMetricsBytes {
		return &ServerMetrics{Reason: "server-metrics export is unreadable or exceeds the size limit"}
	}
	return summarizeServerMetrics(body)
}

func summarizeServerMetrics(body []byte) *ServerMetrics {
	var export serverMetricsExport
	if err := json.Unmarshal(body, &export); err != nil || export.Metrics == nil {
		return &ServerMetrics{Reason: "server-metrics export shape was not recognized"}
	}
	names := make([]string, 0, len(export.Metrics))
	for name := range export.Metrics {
		if len(name) > 128 {
			continue
		}
		lower := strings.ToLower(name)
		for _, keyword := range serverMetricKeywords {
			if strings.Contains(lower, keyword) {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)
	summary := &ServerMetrics{Collected: true, Metrics: map[string]ServerMetricSummary{}}
	for _, name := range names {
		if len(summary.Metrics) == maxServerMetricSummaries {
			break
		}
		metric := export.Metrics[name]
		if len(metric.Series) == 0 {
			continue
		}
		stats := map[string]float64{}
		for key, raw := range metric.Series[0].Stats {
			if _, wanted := serverMetricStats[key]; !wanted {
				continue
			}
			var value float64
			if json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				continue
			}
			stats[key] = value
		}
		if len(stats) == 0 {
			continue
		}
		summary.Metrics[name] = ServerMetricSummary{Type: boundedLabel(metric.Type), Unit: boundedLabel(metric.Unit), Series: len(metric.Series), Stats: stats}
	}
	if len(summary.Metrics) == 0 {
		return &ServerMetrics{Reason: "server-metrics export contained no cache, queue, preemption, or speculation metrics"}
	}
	return summary
}

func boundedLabel(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 {
		return value[:64]
	}
	return value
}
