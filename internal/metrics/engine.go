package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Serving engines whose Prometheus exposition InferCrane normalizes. The
// engine is detected from the payload's metric namespace, never from a
// deployment's configured runtime name.
const (
	EngineVLLM     = "vllm"
	EngineSGLang   = "sglang"
	EngineLlamaCPP = "llamacpp"
)

const (
	// MaxEnginePayloadBytes bounds one scrape of an engine metrics endpoint.
	MaxEnginePayloadBytes = 4 << 20
	maxEngineSamples      = 8192
)

// ErrNoEngineMetrics means the payload contained no metric family from a
// supported engine. Callers must treat that as unavailable evidence, never as
// an idle engine.
var ErrNoEngineMetrics = errors.New("payload contains no supported serving-engine metrics")

// HistogramBucket is one cumulative Prometheus bucket. UpperBound is +Inf for
// the final bucket.
type HistogramBucket struct {
	UpperBound float64
	Count      float64
}

// Histogram is a cumulative Prometheus histogram in its exposition unit
// (seconds for every latency family normalized here). Buckets are sorted by
// upper bound and may be empty when only _sum and _count were exported.
type Histogram struct {
	Count, Sum float64
	Buckets    []HistogramBucket
}

// EngineSnapshot is one bounded, content-free reading of a serving engine.
// Gauges are point-in-time values. Counters and histograms are cumulative
// since the engine process started; EngineMeasurements turns two readings of
// the same process into a window. A nil field means the engine did not
// report that signal, which is different from a reported zero.
type EngineSnapshot struct {
	Engine string

	RequestsRunning, RequestsWaiting *float64
	// KVCacheUsage is the engine's KV-cache occupancy as a 0..1 ratio.
	KVCacheUsage *float64
	// Engine-computed ratios, used only when the matching counters are absent.
	PrefixCacheHitRate, SpecDecodeAcceptRate *float64

	PromptTokens, GenerationTokens, Preemptions *float64
	// PrefixCacheQueries and PrefixCacheHits are token counts.
	PrefixCacheQueries, PrefixCacheHits *float64
	SpecDraftTokens, SpecAcceptedTokens *float64
	// DecodeSeconds and DecodeTokens give a token-weighted mean time per output
	// token for engines that export totals instead of a TPOT histogram.
	DecodeSeconds, DecodeTokens *float64

	QueueTime, PrefillTime, DecodeTime, TTFT, ITL, TPOT, E2E *Histogram
}

type familyKind int

const (
	gaugeSum familyKind = iota
	gaugeMax
	gaugeMean
	counter
	histogram
)

type engineField struct {
	key   string
	kind  familyKind
	names []string
	match map[string]string
	// gate names a gauge that must be reported and positive for this field to
	// be trusted. It keeps an engine's zero-valued "feature off" gauge from
	// becoming a measured 0% ratio.
	gate string
}

type engineSpec struct {
	engine, namespace string
	fields            []engineField
}

// engineSpecs lists the exact metric families InferCrane reads. Earlier names
// in a field's list take precedence over deprecated aliases.
var engineSpecs = []engineSpec{
	{engine: EngineVLLM, namespace: "vllm:", fields: []engineField{
		{key: "running", kind: gaugeSum, names: []string{"vllm:num_requests_running"}},
		{key: "waiting", kind: gaugeSum, names: []string{"vllm:num_requests_waiting"}},
		{key: "kv_usage", kind: gaugeMax, names: []string{"vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"}},
		{key: "prefix_hit_rate", kind: gaugeMean, names: []string{"vllm:gpu_prefix_cache_hit_rate"}},
		{key: "spec_accept_rate", kind: gaugeMean, names: []string{"vllm:spec_decode_draft_acceptance_rate"}},
		{key: "prompt_tokens", kind: counter, names: []string{"vllm:prompt_tokens_total"}},
		{key: "generation_tokens", kind: counter, names: []string{"vllm:generation_tokens_total"}},
		{key: "preemptions", kind: counter, names: []string{"vllm:num_preemptions_total"}},
		{key: "prefix_queries", kind: counter, names: []string{"vllm:prefix_cache_queries_total", "vllm:prefix_cache_queries"}},
		{key: "prefix_hits", kind: counter, names: []string{"vllm:prefix_cache_hits_total", "vllm:prefix_cache_hits"}},
		{key: "spec_draft_tokens", kind: counter, names: []string{"vllm:spec_decode_num_draft_tokens_total"}},
		{key: "spec_accepted_tokens", kind: counter, names: []string{"vllm:spec_decode_num_accepted_tokens_total"}},
		{key: "queue", kind: histogram, names: []string{"vllm:request_queue_time_seconds"}},
		{key: "prefill", kind: histogram, names: []string{"vllm:request_prefill_time_seconds"}},
		{key: "decode", kind: histogram, names: []string{"vllm:request_decode_time_seconds"}},
		{key: "ttft", kind: histogram, names: []string{"vllm:time_to_first_token_seconds"}},
		// vLLM releases before the inter_token_latency rename exported the same
		// per-token histogram as time_per_output_token_seconds.
		{key: "itl", kind: histogram, names: []string{"vllm:inter_token_latency_seconds", "vllm:time_per_output_token_seconds"}},
		{key: "tpot", kind: histogram, names: []string{"vllm:request_time_per_output_token_seconds"}},
		{key: "e2e", kind: histogram, names: []string{"vllm:e2e_request_latency_seconds"}},
	}},
	{engine: EngineSGLang, namespace: "sglang:", fields: []engineField{
		{key: "running", kind: gaugeSum, names: []string{"sglang:num_running_reqs"}},
		{key: "waiting", kind: gaugeSum, names: []string{"sglang:num_queue_reqs"}},
		{key: "kv_usage", kind: gaugeMax, names: []string{"sglang:token_usage"}},
		{key: "prefix_hit_rate", kind: gaugeMean, names: []string{"sglang:cache_hit_rate"}},
		{key: "spec_gate", kind: gaugeMax, names: []string{"sglang:spec_num_draft_tokens"}},
		{key: "spec_accept_rate", kind: gaugeMean, names: []string{"sglang:spec_accept_rate"}, gate: "spec_gate"},
		{key: "prompt_tokens", kind: counter, names: []string{"sglang:prompt_tokens_total"}},
		{key: "generation_tokens", kind: counter, names: []string{"sglang:generation_tokens_total"}},
		{key: "preemptions", kind: counter, names: []string{"sglang:num_retracted_requests_total"}},
		// SGLang reports cached prompt tokens rather than prefix-cache queries;
		// every prompt token is a lookup, so prompt tokens are the denominator.
		{key: "prefix_queries", kind: counter, names: []string{"sglang:prompt_tokens_total"}},
		{key: "prefix_hits", kind: counter, names: []string{"sglang:cached_tokens_total"}},
		{key: "queue", kind: histogram, names: []string{"sglang:queue_time_seconds"}},
		{key: "prefill", kind: histogram, names: []string{"sglang:per_stage_req_latency_seconds"}, match: map[string]string{"stage": "prefill_forward"}},
		{key: "ttft", kind: histogram, names: []string{"sglang:time_to_first_token_seconds"}},
		{key: "itl", kind: histogram, names: []string{"sglang:inter_token_latency_seconds"}},
		{key: "tpot", kind: histogram, names: []string{"sglang:request_time_per_output_token_seconds"}},
		{key: "e2e", kind: histogram, names: []string{"sglang:e2e_request_latency_seconds"}},
	}},
	{engine: EngineLlamaCPP, namespace: "llamacpp:", fields: []engineField{
		{key: "running", kind: gaugeSum, names: []string{"llamacpp:requests_processing"}},
		{key: "waiting", kind: gaugeSum, names: []string{"llamacpp:requests_deferred"}},
		{key: "kv_usage", kind: gaugeMax, names: []string{"llamacpp:kv_cache_usage_ratio"}},
		{key: "prompt_tokens", kind: counter, names: []string{"llamacpp:prompt_tokens_total"}},
		{key: "generation_tokens", kind: counter, names: []string{"llamacpp:tokens_predicted_total"}},
		{key: "decode_seconds", kind: counter, names: []string{"llamacpp:tokens_predicted_seconds_total"}},
		{key: "decode_tokens", kind: counter, names: []string{"llamacpp:tokens_predicted_total"}},
		{key: "spec_draft_tokens", kind: counter, names: []string{"llamacpp:spec_decode_num_draft_tokens_total"}},
		{key: "spec_accepted_tokens", kind: counter, names: []string{"llamacpp:spec_decode_num_accepted_tokens_total"}},
	}},
}

// rankLabels identify replicated reporters inside one engine (for example
// every tensor-parallel rank of one SGLang scheduler). Series that differ only
// by these labels describe the same requests and are not summed.
var rankLabels = map[string]struct{}{"tp_rank": {}, "pp_rank": {}, "moe_ep_rank": {}}

type engineSample struct {
	labels map[string]string
	value  float64
}

// ParseEngine reads the bounded subset of vLLM, SGLang, or llama.cpp server
// metrics that InferCrane uses for autoscaling and bottleneck diagnosis. It
// never exposes arbitrary metric names or labels.
func ParseEngine(text string) (EngineSnapshot, error) {
	if len(text) > MaxEnginePayloadBytes {
		return EngineSnapshot{}, fmt.Errorf("engine metrics payload exceeds %d bytes", MaxEnginePayloadBytes)
	}
	spec, err := detectEngine(text)
	if err != nil {
		return EngineSnapshot{}, err
	}
	wanted := map[string]struct{}{}
	for _, field := range spec.fields {
		for _, name := range field.names {
			if field.kind == histogram {
				for _, suffix := range []string{"_bucket", "_sum", "_count"} {
					wanted[name+suffix] = struct{}{}
				}
				continue
			}
			wanted[name] = struct{}{}
		}
	}
	samples := map[string][]engineSample{}
	total := 0
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 64*1024), MaxEnginePayloadBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rawLabels, rawValue, ok := splitSample(line)
		if !ok {
			continue
		}
		if _, relevant := wanted[name]; !relevant {
			continue
		}
		labels, labelErr := parsePrometheusLabels(rawLabels)
		if labelErr != nil {
			return EngineSnapshot{}, fmt.Errorf("engine metric %s: %w", name, labelErr)
		}
		value, parseErr := strconv.ParseFloat(rawValue, 64)
		if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			// A non-finite sample is not evidence; it leaves the family absent.
			continue
		}
		total++
		if total > maxEngineSamples {
			return EngineSnapshot{}, fmt.Errorf("engine metrics payload contains more than %d relevant samples", maxEngineSamples)
		}
		samples[name] = append(samples[name], engineSample{labels: labels, value: value})
	}
	if err := scanner.Err(); err != nil {
		return EngineSnapshot{}, fmt.Errorf("scan engine metrics payload: %w", err)
	}

	snapshot := EngineSnapshot{Engine: spec.engine}
	gauges := map[string]float64{}
	for _, field := range spec.fields {
		for _, name := range field.names {
			if field.kind == histogram {
				if value, ok := aggregateHistogram(samples, name, field.match); ok {
					snapshot.setHistogram(field.key, value)
					break
				}
				continue
			}
			if value, ok := aggregateScalar(samples[name], field.kind, field.match); ok {
				if field.gate != "" {
					if gate, gated := gauges[field.gate]; !gated || gate <= 0 {
						break
					}
				}
				gauges[field.key] = value
				snapshot.setScalar(field.key, value)
				break
			}
		}
	}
	return snapshot, nil
}

func detectEngine(text string) (engineSpec, error) {
	found := -1
	for index, spec := range engineSpecs {
		if !containsNamespace(text, spec.namespace) {
			continue
		}
		if found >= 0 {
			return engineSpec{}, errors.New("engine metrics payload mixes more than one serving engine")
		}
		found = index
	}
	if found < 0 {
		return engineSpec{}, ErrNoEngineMetrics
	}
	return engineSpecs[found], nil
}

func containsNamespace(text, namespace string) bool {
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], namespace)
		if index < 0 {
			return false
		}
		position := offset + index
		if position == 0 || text[position-1] == '\n' {
			return true
		}
		offset = position + len(namespace)
	}
	return false
}

// splitSample separates a text-format sample into name, label body, and value
// while respecting quoted label values that contain spaces or braces.
func splitSample(line string) (string, string, string, bool) {
	end := strings.IndexAny(line, "{ \t")
	if end <= 0 {
		return "", "", "", false
	}
	name, rest, labels := line[:end], line[end:], ""
	if rest[0] == '{' {
		quoted, escaped, closed := false, false, -1
		for index := 1; index < len(rest); index++ {
			switch {
			case escaped:
				escaped = false
			case rest[index] == '\\' && quoted:
				escaped = true
			case rest[index] == '"':
				quoted = !quoted
			case rest[index] == '}' && !quoted:
				closed = index
			}
			if closed >= 0 {
				break
			}
		}
		if closed < 0 {
			return "", "", "", false
		}
		labels, rest = rest[1:closed], rest[closed+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) < 1 {
		return "", "", "", false
	}
	return name, labels, fields[0], true
}

func matches(labels, selector map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// groupKey identifies one independent reporter: the label set without rank
// labels, the bucket bound, and labels the field selected on.
func groupKey(labels map[string]string, drop ...string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		if _, rank := rankLabels[key]; rank {
			continue
		}
		skip := false
		for _, item := range drop {
			if key == item {
				skip = true
				break
			}
		}
		if !skip {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte(0)
		builder.WriteString(labels[key])
		builder.WriteByte(0)
	}
	return builder.String()
}

func aggregateScalar(samples []engineSample, kind familyKind, selector map[string]string) (float64, bool) {
	selected := make([]engineSample, 0, len(samples))
	totalPriority := false
	for _, sample := range samples {
		if !matches(sample.labels, selector) {
			continue
		}
		if priority, ok := sample.labels["priority"]; ok && priority == "" {
			totalPriority = true
		}
		selected = append(selected, sample)
	}
	if len(selected) == 0 {
		return 0, false
	}
	groups := map[string]float64{}
	order := []string{}
	for _, sample := range selected {
		// SGLang exports a queue total under priority="" next to per-priority
		// breakdowns. Summing both would double count the queue.
		if priority, ok := sample.labels["priority"]; kind != counter && ok && priority != "" && totalPriority {
			continue
		}
		key := groupKey(sample.labels)
		current, seen := groups[key]
		if !seen {
			order = append(order, key)
			groups[key] = sample.value
			continue
		}
		if sample.value > current {
			groups[key] = sample.value
		}
	}
	var result float64
	for index, key := range order {
		value := groups[key]
		switch {
		case index == 0:
			result = value
		case kind == gaugeMax:
			result = math.Max(result, value)
		default:
			result += value
		}
	}
	if kind == gaugeMean {
		result /= float64(len(order))
	}
	return result, true
}

type histogramSeries struct {
	buckets    map[float64]float64
	sum, count float64
	hasSum     bool
	hasCount   bool
}

func aggregateHistogram(samples map[string][]engineSample, name string, selector map[string]string) (Histogram, bool) {
	series := map[string]*histogramSeries{}
	groupOf := map[string]string{}
	get := func(labels map[string]string) *histogramSeries {
		key := seriesKey(labels)
		item, ok := series[key]
		if !ok {
			item = &histogramSeries{buckets: map[float64]float64{}}
			series[key] = item
			groupOf[key] = groupKey(labels, "le")
		}
		return item
	}
	for _, sample := range samples[name+"_bucket"] {
		if !matches(sample.labels, selector) {
			continue
		}
		bound, err := strconv.ParseFloat(sample.labels["le"], 64)
		if err != nil || math.IsNaN(bound) {
			continue
		}
		get(sample.labels).buckets[bound] = sample.value
	}
	for _, sample := range samples[name+"_sum"] {
		if matches(sample.labels, selector) {
			item := get(sample.labels)
			item.sum, item.hasSum = sample.value, true
		}
	}
	for _, sample := range samples[name+"_count"] {
		if matches(sample.labels, selector) {
			item := get(sample.labels)
			item.count, item.hasCount = sample.value, true
		}
	}
	// Within one reporter group keep the series with the most observations;
	// replicated rank series describe the same requests.
	chosen := map[string]*histogramSeries{}
	for key, item := range series {
		if !item.hasCount {
			if top, ok := item.buckets[math.Inf(1)]; ok {
				item.count, item.hasCount = top, true
			}
		}
		if !item.hasCount || !item.hasSum {
			continue
		}
		group := groupOf[key]
		if current, ok := chosen[group]; !ok || item.count > current.count {
			chosen[group] = item
		}
	}
	if len(chosen) == 0 {
		return Histogram{}, false
	}
	result := Histogram{}
	var layout []float64
	bucketsValid := true
	merged := map[float64]float64{}
	for _, item := range chosen {
		result.Count += item.count
		result.Sum += item.sum
		bounds := make([]float64, 0, len(item.buckets))
		for bound := range item.buckets {
			bounds = append(bounds, bound)
		}
		sort.Float64s(bounds)
		if layout == nil {
			layout = bounds
		} else if !sameBounds(layout, bounds) {
			bucketsValid = false
		}
		for bound, count := range item.buckets {
			merged[bound] += count
		}
	}
	if bucketsValid && len(layout) > 0 && math.IsInf(layout[len(layout)-1], 1) {
		result.Buckets = make([]HistogramBucket, 0, len(layout))
		for _, bound := range layout {
			result.Buckets = append(result.Buckets, HistogramBucket{UpperBound: bound, Count: merged[bound]})
		}
	}
	return result, true
}

func seriesKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		if key != "le" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte(0)
		builder.WriteString(labels[key])
		builder.WriteByte(0)
	}
	return builder.String()
}

func sameBounds(left, right []float64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *EngineSnapshot) setScalar(key string, value float64) {
	pointer := &value
	switch key {
	case "running":
		s.RequestsRunning = pointer
	case "waiting":
		s.RequestsWaiting = pointer
	case "kv_usage":
		s.KVCacheUsage = pointer
	case "prefix_hit_rate":
		s.PrefixCacheHitRate = pointer
	case "spec_accept_rate":
		s.SpecDecodeAcceptRate = pointer
	case "prompt_tokens":
		s.PromptTokens = pointer
	case "generation_tokens":
		s.GenerationTokens = pointer
	case "preemptions":
		s.Preemptions = pointer
	case "prefix_queries":
		s.PrefixCacheQueries = pointer
	case "prefix_hits":
		s.PrefixCacheHits = pointer
	case "spec_draft_tokens":
		s.SpecDraftTokens = pointer
	case "spec_accepted_tokens":
		s.SpecAcceptedTokens = pointer
	case "decode_seconds":
		s.DecodeSeconds = pointer
	case "decode_tokens":
		s.DecodeTokens = pointer
	}
}

func (s *EngineSnapshot) setHistogram(key string, value Histogram) {
	pointer := &value
	switch key {
	case "queue":
		s.QueueTime = pointer
	case "prefill":
		s.PrefillTime = pointer
	case "decode":
		s.DecodeTime = pointer
	case "ttft":
		s.TTFT = pointer
	case "itl":
		s.ITL = pointer
	case "tpot":
		s.TPOT = pointer
	case "e2e":
		s.E2E = pointer
	}
}
