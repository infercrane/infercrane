package metrics

import (
	"bufio"
	"math"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
)

var metricNames = map[string][]string{
	"running":    {"vllm:num_requests_running"},
	"waiting":    {"vllm:num_requests_waiting"},
	"kv":         {"vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"},
	"queries":    {"vllm:prefix_cache_queries", "vllm:prefix_cache_queries_total", "vllm:cached_request_prefix_tokens"},
	"hits":       {"vllm:prefix_cache_hits", "vllm:prefix_cache_hits_total", "vllm:prefix_cache_hit_tokens"},
	"prompt":     {"vllm:prompt_tokens_total"},
	"generation": {"vllm:generation_tokens_total"},
}

func Parse(text string) domain.Metrics {
	values := make(map[string][]float64)
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.SplitN(fields[0], "{", 2)[0]
		value, err := strconv.ParseFloat(fields[1], 64)
		if err == nil && !math.IsInf(value, 0) && !math.IsNaN(value) {
			values[name] = append(values[name], value)
		}
	}
	return domain.Metrics{
		RequestsRunning: resolve(values, metricNames["running"], true), RequestsWaiting: resolve(values, metricNames["waiting"], true),
		KVCacheUsage: resolve(values, metricNames["kv"], true), PrefixCacheQueries: resolve(values, metricNames["queries"], false),
		PrefixCacheHits: resolve(values, metricNames["hits"], false), PromptTokensTotal: resolve(values, metricNames["prompt"], false),
		GenerationTokensTotal: resolve(values, metricNames["generation"], false), Raw: text,
	}
}

func resolve(values map[string][]float64, names []string, gauge bool) *float64 {
	for _, name := range names {
		found, ok := values[name]
		if !ok {
			continue
		}
		value := found[0]
		for _, item := range found[1:] {
			if gauge && item > value {
				value = item
			} else if !gauge {
				value += item
			}
		}
		return &value
	}
	return nil
}
