package metrics

import (
	"errors"
	"math"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// Operational measurement names derived from serving-engine metrics. The
// three names that predate engine collection (runtime_internal_itl,
// runtime_internal_tpot, runtime_kv_cache_timeseries) keep their established
// API spelling.
const (
	MeasurementRequestsRunning    = "runtime_requests_running"
	MeasurementRequestsWaiting    = "runtime_requests_waiting"
	MeasurementKVCacheUsage       = "runtime_kv_cache_timeseries"
	MeasurementQueueTimeP95       = "runtime_queue_time_p95"
	MeasurementPrefillTimeP95     = "runtime_prefill_time_p95"
	MeasurementDecodeTimeP95      = "runtime_decode_time_p95"
	MeasurementTTFTP95            = "runtime_ttft_p95"
	MeasurementITLP95             = "runtime_internal_itl"
	MeasurementTPOTP95            = "runtime_internal_tpot"
	MeasurementTPOTMean           = "runtime_tpot_mean"
	MeasurementRequestsCompleted  = "runtime_requests_completed"
	MeasurementPreemptions        = "runtime_preemptions"
	MeasurementPrefixCacheHitRate = "runtime_prefix_cache_hit_rate"
	MeasurementSpecDecodeAccept   = "runtime_spec_decode_acceptance"
)

// Aggregation modes say how monitoring combines one measurement across the
// replicas of a deployment.
const (
	AggregateSum      = "sum"
	AggregateMax      = "max"
	AggregateWeighted = "sample_weighted_mean"
)

// EngineMeasurement describes one normalized engine measurement.
type EngineMeasurement struct {
	Name, Unit, Aggregation string
}

// EngineMeasurementCatalog is the complete, ordered set of engine-derived
// measurements. Monitoring reports every entry, and storage validates units
// against it.
var EngineMeasurementCatalog = []EngineMeasurement{
	{MeasurementRequestsRunning, "count", AggregateSum},
	{MeasurementRequestsWaiting, "count", AggregateSum},
	{MeasurementKVCacheUsage, "percent", AggregateMax},
	{MeasurementQueueTimeP95, "milliseconds", AggregateMax},
	{MeasurementPrefillTimeP95, "milliseconds", AggregateMax},
	{MeasurementDecodeTimeP95, "milliseconds", AggregateMax},
	{MeasurementTTFTP95, "milliseconds", AggregateMax},
	{MeasurementITLP95, "milliseconds", AggregateMax},
	{MeasurementTPOTP95, "milliseconds", AggregateMax},
	{MeasurementTPOTMean, "milliseconds", AggregateMax},
	{MeasurementRequestsCompleted, "count", AggregateSum},
	{MeasurementPreemptions, "count", AggregateSum},
	{MeasurementPrefixCacheHitRate, "ratio", AggregateWeighted},
	{MeasurementSpecDecodeAccept, "ratio", AggregateWeighted},
}

// EngineMeasurementOptions binds one scrape to a replica and a validity
// window.
type EngineMeasurementOptions struct {
	ReplicaID  string
	ObservedAt time.Time
	TTL        time.Duration
}

// EngineMeasurements converts the current scrape into bounded operational
// measurements. Gauges come from the current scrape alone. Window values
// (latency p95s, preemptions, completions, cache and speculative-decoding
// ratios) require the previous scrape of the same engine process; without it,
// or after a counter reset, they are omitted rather than reported as zero.
func EngineMeasurements(previous *EngineSnapshot, current EngineSnapshot, options EngineMeasurementOptions) ([]domain.OperationalMeasurement, error) {
	if current.Engine == "" {
		return nil, errors.New("engine measurements require a detected engine")
	}
	if options.ObservedAt.IsZero() {
		return nil, errors.New("engine measurements require an observation time")
	}
	if options.TTL <= 0 || options.TTL > 24*time.Hour {
		return nil, errors.New("engine evidence TTL must be greater than zero and at most 24 hours")
	}
	observedAt := options.ObservedAt.UTC()
	out := make([]domain.OperationalMeasurement, 0, len(EngineMeasurementCatalog))
	add := func(name string, value float64, samples float64) {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || samples < 1 {
			return
		}
		// Six decimal places keep microsecond and part-per-million precision
		// while removing float noise from persisted evidence.
		value = math.Round(value*1e6) / 1e6
		out = append(out, domain.OperationalMeasurement{
			ReplicaID: options.ReplicaID, Name: name, Value: value, Unit: measurementUnit(name),
			EvidenceClass: "measured", Source: current.Engine + "_metrics",
			SampleCount: boundedCount(samples), ObservedAt: observedAt, ValidUntil: observedAt.Add(options.TTL),
		})
	}
	if current.RequestsRunning != nil {
		add(MeasurementRequestsRunning, *current.RequestsRunning, 1)
	}
	if current.RequestsWaiting != nil {
		add(MeasurementRequestsWaiting, *current.RequestsWaiting, 1)
	}
	if current.KVCacheUsage != nil && *current.KVCacheUsage <= 1 {
		add(MeasurementKVCacheUsage, *current.KVCacheUsage*100, 1)
	}

	window := SameEngineProcess(previous, current)
	if window {
		for _, item := range []struct {
			name           string
			current, older *Histogram
		}{
			{MeasurementQueueTimeP95, current.QueueTime, previous.QueueTime},
			{MeasurementPrefillTimeP95, current.PrefillTime, previous.PrefillTime},
			{MeasurementDecodeTimeP95, current.DecodeTime, previous.DecodeTime},
			{MeasurementTTFTP95, current.TTFT, previous.TTFT},
			{MeasurementITLP95, current.ITL, previous.ITL},
			{MeasurementTPOTP95, current.TPOT, previous.TPOT},
		} {
			delta, ok := histogramDelta(item.current, item.older)
			if !ok || delta.Count <= 0 {
				continue
			}
			if p95, ok := delta.Quantile(.95); ok {
				add(item.name, p95*1000, delta.Count)
			}
		}
		if delta, ok := histogramDelta(current.TPOT, previous.TPOT); ok && delta.Count > 0 {
			add(MeasurementTPOTMean, delta.Sum/delta.Count*1000, delta.Count)
		} else if seconds, ok := counterDelta(current.DecodeSeconds, previous.DecodeSeconds); ok {
			if tokens, ok := counterDelta(current.DecodeTokens, previous.DecodeTokens); ok && tokens > 0 {
				add(MeasurementTPOTMean, seconds/tokens*1000, tokens)
			}
		}
		if delta, ok := histogramDelta(current.E2E, previous.E2E); ok {
			add(MeasurementRequestsCompleted, delta.Count, 1)
		}
		if delta, ok := counterDelta(current.Preemptions, previous.Preemptions); ok {
			add(MeasurementPreemptions, delta, 1)
		}
	}
	if current.PrefixCacheQueries != nil && current.PrefixCacheHits != nil {
		if window {
			queries, queriesOK := counterDelta(current.PrefixCacheQueries, previous.PrefixCacheQueries)
			hits, hitsOK := counterDelta(current.PrefixCacheHits, previous.PrefixCacheHits)
			if queriesOK && hitsOK && queries > 0 && hits <= queries {
				add(MeasurementPrefixCacheHitRate, hits/queries, queries)
			}
		}
	} else if current.PrefixCacheHitRate != nil && *current.PrefixCacheHitRate <= 1 {
		add(MeasurementPrefixCacheHitRate, *current.PrefixCacheHitRate, 1)
	}
	if current.SpecDraftTokens != nil && current.SpecAcceptedTokens != nil {
		if window {
			drafts, draftsOK := counterDelta(current.SpecDraftTokens, previous.SpecDraftTokens)
			accepted, acceptedOK := counterDelta(current.SpecAcceptedTokens, previous.SpecAcceptedTokens)
			if draftsOK && acceptedOK && drafts > 0 && accepted <= drafts {
				add(MeasurementSpecDecodeAccept, accepted/drafts, drafts)
			}
		}
	} else if current.SpecDecodeAcceptRate != nil && *current.SpecDecodeAcceptRate <= 1 {
		add(MeasurementSpecDecodeAccept, *current.SpecDecodeAcceptRate, 1)
	}
	return out, nil
}

// Quantile estimates a quantile by linear interpolation inside the bucket that
// contains it, the same rule Prometheus histogram_quantile uses. A quantile in
// the +Inf bucket has no finite bound and is reported as unavailable.
func (h Histogram) Quantile(q float64) (float64, bool) {
	if h.Count <= 0 || len(h.Buckets) == 0 || q <= 0 || q > 1 {
		return 0, false
	}
	rank := q * h.Count
	lower, below := 0.0, 0.0
	for _, bucket := range h.Buckets {
		if bucket.Count < below {
			return 0, false
		}
		if bucket.Count >= rank {
			if math.IsInf(bucket.UpperBound, 1) {
				return 0, false
			}
			if bucket.Count == below {
				return bucket.UpperBound, true
			}
			if bucket.UpperBound < lower {
				return 0, false
			}
			return lower + (bucket.UpperBound-lower)*(rank-below)/(bucket.Count-below), true
		}
		lower, below = bucket.UpperBound, bucket.Count
	}
	return 0, false
}

func measurementUnit(name string) string {
	for _, item := range EngineMeasurementCatalog {
		if item.Name == name {
			return item.Unit
		}
	}
	return ""
}

func boundedCount(value float64) int {
	if value >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int(value)
}

// SameEngineProcess reports whether two scrapes came from one uninterrupted
// engine process: the same engine and no cumulative value moving backwards.
func SameEngineProcess(previous *EngineSnapshot, current EngineSnapshot) bool {
	if previous == nil || previous.Engine != current.Engine {
		return false
	}
	for _, pair := range [][2]*float64{
		{previous.PromptTokens, current.PromptTokens}, {previous.GenerationTokens, current.GenerationTokens},
		{previous.Preemptions, current.Preemptions}, {previous.PrefixCacheQueries, current.PrefixCacheQueries},
		{previous.PrefixCacheHits, current.PrefixCacheHits}, {previous.SpecDraftTokens, current.SpecDraftTokens},
		{previous.SpecAcceptedTokens, current.SpecAcceptedTokens}, {previous.DecodeSeconds, current.DecodeSeconds},
		{previous.DecodeTokens, current.DecodeTokens},
	} {
		if pair[0] != nil && pair[1] != nil && *pair[1] < *pair[0] {
			return false
		}
	}
	for _, pair := range [][2]*Histogram{
		{previous.QueueTime, current.QueueTime}, {previous.PrefillTime, current.PrefillTime},
		{previous.DecodeTime, current.DecodeTime}, {previous.TTFT, current.TTFT}, {previous.ITL, current.ITL},
		{previous.TPOT, current.TPOT}, {previous.E2E, current.E2E},
	} {
		if pair[0] != nil && pair[1] != nil && pair[1].Count < pair[0].Count {
			return false
		}
	}
	return true
}

func counterDelta(current, previous *float64) (float64, bool) {
	if current == nil || previous == nil || *current < *previous {
		return 0, false
	}
	return *current - *previous, true
}

// histogramDelta subtracts two cumulative readings. Bucket deltas are kept
// only when both readings share one bucket layout.
func histogramDelta(current, previous *Histogram) (Histogram, bool) {
	if current == nil || previous == nil || current.Count < previous.Count {
		return Histogram{}, false
	}
	delta := Histogram{Count: current.Count - previous.Count, Sum: current.Sum - previous.Sum}
	if delta.Sum < 0 {
		return Histogram{}, false
	}
	if len(current.Buckets) == len(previous.Buckets) && len(current.Buckets) > 0 {
		buckets := make([]HistogramBucket, len(current.Buckets))
		for index := range current.Buckets {
			if current.Buckets[index].UpperBound != previous.Buckets[index].UpperBound {
				buckets = nil
				break
			}
			count := current.Buckets[index].Count - previous.Buckets[index].Count
			if count < 0 {
				buckets = nil
				break
			}
			buckets[index] = HistogramBucket{UpperBound: current.Buckets[index].UpperBound, Count: count}
		}
		delta.Buckets = buckets
	}
	return delta, true
}
