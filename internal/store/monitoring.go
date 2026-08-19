package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

const monitoringSchema = "https://opentelemetry.io/schemas/gen-ai/1.42.0"

// EndpointMonitoring builds a bounded operational read model from persisted
// request and lifecycle evidence. Raw high-cardinality runtime metrics remain
// behind adapter-specific collectors and never leak into the domain contract.
func (s *Store) EndpointMonitoring(ctx context.Context, tenant, name string, window, bucket time.Duration) (domain.EndpointMonitoringSnapshot, error) {
	if tenant == "" {
		tenant = "global"
	}
	if window < time.Minute || window > 30*24*time.Hour || bucket < time.Minute || bucket > 24*time.Hour || bucket > window || (window+bucket-1)/bucket+1 > 500 {
		return domain.EndpointMonitoringSnapshot{}, errors.New("monitoring window or bucket is outside the bounded range")
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, name)
	if err != nil {
		return domain.EndpointMonitoringSnapshot{}, err
	}
	end := time.Now().UTC()
	start := end.Add(-window)
	out := domain.EndpointMonitoringSnapshot{
		Endpoint: resolved.Endpoint.Name, LogicalModel: resolved.LogicalModel.Name, Environment: resolved.Environment.Name,
		WindowStart: start, WindowEnd: end, BucketSeconds: int(bucket.Seconds()),
		Series: []domain.MonitoringBucket{}, Breakdowns: []domain.MonitoringBreakdown{}, Events: []domain.MonitoringEvent{},
		Evidence: domain.MonitoringEvidence{
			Source: "infercrane_gateway_request_records", SemanticConventionSchema: monitoringSchema,
			Available:    []string{"request_rate", "error_rate", "gateway_request_latency", "gateway_time_to_first_response", "fallback", "retry"},
			Unavailable:  []string{"reported_input_token_usage", "reported_output_token_usage", "runtime_internal_itl", "runtime_internal_tpot", "gpu_utilization", "gpu_memory", "runtime_kv_cache_timeseries"},
			Measurements: []domain.MeasurementEvidence{},
		},
	}
	if err = s.monitoringSummary(ctx, tenant, resolved.Endpoint.ID, start, end, window, &out); err != nil {
		return domain.EndpointMonitoringSnapshot{}, err
	}
	if err = s.monitoringOperationalMeasurements(ctx, tenant, resolved.Endpoint.ID, start, end, &out); err != nil {
		return domain.EndpointMonitoringSnapshot{}, err
	}
	if err = s.monitoringSeries(ctx, tenant, resolved.Endpoint.ID, start, end, bucket, &out); err != nil {
		return domain.EndpointMonitoringSnapshot{}, err
	}
	if err = s.monitoringBreakdowns(ctx, tenant, resolved.Endpoint.ID, start, end, &out); err != nil {
		return domain.EndpointMonitoringSnapshot{}, err
	}
	if err = s.monitoringEvents(ctx, resolved.Endpoint.ID, start, end, &out); err != nil {
		return domain.EndpointMonitoringSnapshot{}, err
	}
	return out, nil
}

func (s *Store) monitoringSummary(ctx context.Context, tenant, endpointID string, start, end time.Time, window time.Duration, out *domain.EndpointMonitoringSnapshot) error {
	var p50Latency, p95Latency, p50TTFT, p95TTFT, p95Queue, p95Generation sql.NullFloat64
	var latest sql.NullTime
	err := s.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(*) FILTER(WHERE error_type IS NOT NULL OR status_code IS NULL OR status_code>=400),COUNT(*) FILTER(WHERE COALESCE(fallback_reason,'')<>''),COUNT(*) FILTER(WHERE retry_count>0),COUNT(*) FILTER(WHERE streaming=TRUE),COUNT(*) FILTER(WHERE input_tokens IS NOT NULL OR output_tokens IS NOT NULL),COUNT(input_tokens),COUNT(output_tokens),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),percentile_cont(0.50) WITHIN GROUP(ORDER BY latency_ms) FILTER(WHERE latency_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP(ORDER BY latency_ms) FILTER(WHERE latency_ms IS NOT NULL),percentile_cont(0.50) WITHIN GROUP(ORDER BY ttft_ms) FILTER(WHERE ttft_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP(ORDER BY ttft_ms) FILTER(WHERE ttft_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP(ORDER BY queue_ms) FILTER(WHERE queue_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP(ORDER BY generation_ms) FILTER(WHERE generation_ms IS NOT NULL),MAX(started_at) FROM request_records WHERE tenant_id=? AND endpoint_id=? AND started_at>=? AND started_at<=?`, tenant, endpointID, start, end).Scan(&out.Summary.Requests, &out.Summary.Errors, &out.Summary.Fallbacks, &out.Summary.Retried, &out.Summary.Streaming, &out.Summary.TokenUsageSamples, &out.Summary.InputTokenSamples, &out.Summary.OutputTokenSamples, &out.Summary.InputTokens, &out.Summary.OutputTokens, &p50Latency, &p95Latency, &p50TTFT, &p95TTFT, &p95Queue, &p95Generation, &latest)
	if err != nil {
		return err
	}
	seconds := window.Seconds()
	out.Summary.RequestsPerSecond = float64(out.Summary.Requests) / seconds
	if out.Summary.InputTokenSamples > 0 {
		out.Summary.InputTokensPerSecond = floatPointer(float64(out.Summary.InputTokens) / seconds)
		out.Evidence.Available = append(out.Evidence.Available, "reported_input_token_usage")
		out.Evidence.Unavailable = removeString(out.Evidence.Unavailable, "reported_input_token_usage")
	}
	if out.Summary.OutputTokenSamples > 0 {
		out.Summary.OutputTokensPerSecond = floatPointer(float64(out.Summary.OutputTokens) / seconds)
		out.Evidence.Available = append(out.Evidence.Available, "reported_output_token_usage")
		out.Evidence.Unavailable = removeString(out.Evidence.Unavailable, "reported_output_token_usage")
	}
	if out.Summary.Requests > 0 {
		out.Summary.ErrorRate = ratio(out.Summary.Errors, out.Summary.Requests)
		out.Summary.FallbackRate = ratio(out.Summary.Fallbacks, out.Summary.Requests)
		out.Summary.RetryRate = ratio(out.Summary.Retried, out.Summary.Requests)
	}
	out.Summary.P50LatencyMS, out.Summary.P95LatencyMS = nullableFloat(p50Latency), nullableFloat(p95Latency)
	out.Summary.P50TTFTMS, out.Summary.P95TTFTMS = nullableFloat(p50TTFT), nullableFloat(p95TTFT)
	out.Summary.P95QueueMS, out.Summary.P95GenerationMS = nullableFloat(p95Queue), nullableFloat(p95Generation)
	out.Evidence.SampleCount = out.Summary.Requests
	if latest.Valid {
		value := latest.Time.UTC()
		out.Evidence.LatestRequestAt = &value
		freshness := 5 * time.Minute
		if candidate := 2 * time.Duration(out.BucketSeconds) * time.Second; candidate > freshness {
			freshness = candidate
		}
		out.Evidence.Fresh = out.WindowEnd.Sub(value) <= freshness
	}
	populateMeasurementEvidence(out)
	return nil
}

func populateMeasurementEvidence(out *domain.EndpointMonitoringSnapshot) {
	observedAt := out.Evidence.LatestRequestAt
	var freshUntil *time.Time
	availability := "not_observed"
	reason := "no request evidence was recorded in the selected window"
	if observedAt != nil {
		freshness := 5 * time.Minute
		if candidate := 2 * time.Duration(out.BucketSeconds) * time.Second; candidate > freshness {
			freshness = candidate
		}
		until := observedAt.Add(freshness)
		freshUntil = &until
		if out.Evidence.Fresh {
			availability, reason = "available", ""
		} else {
			availability = "stale"
			reason = "the latest request sample is older than the monitoring freshness window"
		}
	}
	add := func(name, unit string, value *float64, samples int) {
		state, why := availability, reason
		if observedAt != nil && value == nil {
			state = "not_observed"
			why = "requests were recorded but this measurement was not reported"
		}
		out.Evidence.Measurements = append(out.Evidence.Measurements, domain.MeasurementEvidence{
			Name: name, Value: value, Unit: unit, Availability: state,
			EvidenceClass: "measured", Source: out.Evidence.Source,
			ObservedAt: observedAt, FreshUntil: freshUntil, SampleCount: samples, Reason: why,
		})
	}
	requests := float64(out.Summary.Requests)
	requestValue := &requests
	if out.Summary.Requests == 0 {
		requestValue = nil
	}
	requestRate := out.Summary.RequestsPerSecond
	add("requests", "requests", requestValue, out.Summary.Requests)
	add("request_rate", "requests_per_second", &requestRate, out.Summary.Requests)
	add("error_rate", "ratio", out.Summary.ErrorRate, out.Summary.Requests)
	add("fallback_rate", "ratio", out.Summary.FallbackRate, out.Summary.Requests)
	add("retry_rate", "ratio", out.Summary.RetryRate, out.Summary.Requests)
	add("latency_p95", "milliseconds", out.Summary.P95LatencyMS, out.Summary.Requests)
	add("ttft_p95", "milliseconds", out.Summary.P95TTFTMS, out.Summary.Requests)
	add("queue_p95", "milliseconds", out.Summary.P95QueueMS, out.Summary.Requests)
	add("generation_p95", "milliseconds", out.Summary.P95GenerationMS, out.Summary.Requests)
	add("input_token_throughput", "tokens_per_second", out.Summary.InputTokensPerSecond, out.Summary.InputTokenSamples)
	add("output_token_throughput", "tokens_per_second", out.Summary.OutputTokensPerSecond, out.Summary.OutputTokenSamples)
	for _, unavailable := range []struct{ name, unit, reason string }{
		{"runtime_internal_itl", "milliseconds", "the connected runtime has not supplied qualified ITL telemetry"},
		{"runtime_internal_tpot", "milliseconds", "the connected runtime has not supplied qualified TPOT telemetry"},
		{"gpu_utilization", "percent", "no qualified hardware telemetry collector is connected"},
		{"gpu_memory", "bytes", "no qualified hardware telemetry collector is connected"},
		{"gpu_temperature", "celsius", "no qualified hardware telemetry collector is connected"},
		{"gpu_power", "watts", "no qualified hardware telemetry collector is connected"},
		{"gpu_xid_errors", "count", "no qualified hardware telemetry collector is connected"},
		{"runtime_kv_cache_timeseries", "ratio", "the connected runtime has not supplied qualified KV-cache telemetry"},
	} {
		out.Evidence.Measurements = append(out.Evidence.Measurements, domain.MeasurementEvidence{
			Name: unavailable.name, Unit: unavailable.unit, Availability: "unsupported",
			EvidenceClass: "measured", Source: "runtime_adapter", Reason: unavailable.reason,
		})
	}
}

type operationalMeasurementAggregate struct {
	name, unit, evidenceClass string
	value                     float64
	sampleCount               int
	observedAt, freshUntil    time.Time
	sources                   map[string]struct{}
	fresh                     bool
}

func (s *Store) monitoringOperationalMeasurements(ctx context.Context, tenant, endpointID string, start, end time.Time, out *domain.EndpointMonitoringSnapshot) error {
	rows, err := s.QueryContext(ctx, `SELECT DISTINCT ON (m.deployment_id,m.replica_id,m.name) m.name,m.value,m.unit,m.evidence_class,m.source,m.sample_count,m.observed_at,m.valid_until FROM operational_measurements m JOIN deployments d ON d.id=m.deployment_id AND d.tenant_id=m.tenant_id JOIN backend_bindings b ON b.deployment_id=m.deployment_id AND b.tenant_id=m.tenant_id WHERE m.tenant_id=? AND b.endpoint_id=? AND m.revision_id=d.active_revision_id AND m.observed_at<=? AND m.valid_until>=? ORDER BY m.deployment_id,m.replica_id,m.name,m.observed_at DESC,m.created_at DESC`, tenant, endpointID, end, start)
	if err != nil {
		return err
	}
	defer rows.Close()
	aggregates := map[string]*operationalMeasurementAggregate{}
	for rows.Next() {
		var name, unit, evidenceClass, source string
		var value float64
		var sampleCount int
		var observedAt, validUntil time.Time
		if err = rows.Scan(&name, &value, &unit, &evidenceClass, &source, &sampleCount, &observedAt, &validUntil); err != nil {
			return err
		}
		fresh := validUntil.After(end) || validUntil.Equal(end)
		aggregate, ok := aggregates[name]
		if !ok {
			aggregate = &operationalMeasurementAggregate{name: name, unit: unit, evidenceClass: evidenceClass, sources: map[string]struct{}{}, observedAt: observedAt.UTC(), freshUntil: validUntil.UTC()}
			aggregates[name] = aggregate
		}
		if !fresh {
			if !aggregate.fresh && (aggregate.sampleCount == 0 || observedAt.After(aggregate.observedAt)) {
				aggregate.unit = unit
				aggregate.evidenceClass = evidenceClass
				aggregate.observedAt = observedAt.UTC()
				aggregate.freshUntil = validUntil.UTC()
				aggregate.sampleCount = sampleCount
				aggregate.sources = map[string]struct{}{source: {}}
			}
			continue
		}
		if !aggregate.fresh {
			aggregate.value = value
			aggregate.unit = unit
			aggregate.evidenceClass = evidenceClass
			aggregate.observedAt = observedAt.UTC()
			aggregate.freshUntil = validUntil.UTC()
			aggregate.sampleCount = 0
			aggregate.sources = map[string]struct{}{}
			aggregate.fresh = true
		} else {
			switch name {
			case "gpu_memory", "gpu_power", "gpu_xid_errors":
				aggregate.value += value
			default:
				if value > aggregate.value {
					aggregate.value = value
				}
			}
			if validUntil.Before(aggregate.freshUntil) {
				aggregate.freshUntil = validUntil.UTC()
			}
			if observedAt.After(aggregate.observedAt) {
				aggregate.observedAt = observedAt.UTC()
			}
		}
		aggregate.sources[source] = struct{}{}
		if evidenceClass == "provider_reported" {
			aggregate.evidenceClass = "provider_reported"
		}
		aggregate.sampleCount += sampleCount
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, name := range []string{"gpu_utilization", "gpu_memory", "gpu_temperature", "gpu_power", "gpu_xid_errors"} {
		aggregate, ok := aggregates[name]
		if !ok {
			continue
		}
		sources := make([]string, 0, len(aggregate.sources))
		for source := range aggregate.sources {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		measurement := domain.MeasurementEvidence{Name: name, Unit: aggregate.unit, EvidenceClass: aggregate.evidenceClass, Source: strings.Join(sources, ","), ObservedAt: &aggregate.observedAt, SampleCount: aggregate.sampleCount}
		if aggregate.fresh {
			measurement.Value = floatPointer(aggregate.value)
			measurement.Availability = "available"
			measurement.FreshUntil = &aggregate.freshUntil
			out.Evidence.Available = append(out.Evidence.Available, name)
			out.Evidence.Unavailable = removeString(out.Evidence.Unavailable, name)
		} else {
			measurement.Availability = "stale"
			measurement.Reason = "the latest qualified collector snapshot is outside its declared validity window"
		}
		replaceMeasurementEvidence(&out.Evidence.Measurements, measurement)
	}
	return nil
}

func replaceMeasurementEvidence(values *[]domain.MeasurementEvidence, replacement domain.MeasurementEvidence) {
	for index := range *values {
		if (*values)[index].Name == replacement.Name {
			(*values)[index] = replacement
			return
		}
	}
	*values = append(*values, replacement)
}

func (s *Store) monitoringSeries(ctx context.Context, tenant, endpointID string, start, end time.Time, bucket time.Duration, out *domain.EndpointMonitoringSnapshot) error {
	rows, err := s.QueryContext(ctx, `SELECT date_bin((? * INTERVAL '1 second'),started_at,TIMESTAMPTZ '1970-01-01'),COUNT(*),COUNT(*) FILTER(WHERE error_type IS NOT NULL OR status_code IS NULL OR status_code>=400),COUNT(*) FILTER(WHERE COALESCE(fallback_reason,'')<>''),COUNT(*) FILTER(WHERE retry_count>0),COUNT(*) FILTER(WHERE streaming=TRUE),COUNT(*) FILTER(WHERE input_tokens IS NOT NULL OR output_tokens IS NOT NULL),COUNT(input_tokens),COUNT(output_tokens),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),percentile_cont(0.50) WITHIN GROUP(ORDER BY latency_ms) FILTER(WHERE latency_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP(ORDER BY latency_ms) FILTER(WHERE latency_ms IS NOT NULL),percentile_cont(0.50) WITHIN GROUP(ORDER BY ttft_ms) FILTER(WHERE ttft_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP(ORDER BY ttft_ms) FILTER(WHERE ttft_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP(ORDER BY queue_ms) FILTER(WHERE queue_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP(ORDER BY generation_ms) FILTER(WHERE generation_ms IS NOT NULL) FROM request_records WHERE tenant_id=? AND endpoint_id=? AND started_at>=? AND started_at<=? GROUP BY 1 ORDER BY 1`, int(bucket.Seconds()), tenant, endpointID, start, end)
	if err != nil {
		return err
	}
	defer rows.Close()
	values := make(map[int64]domain.MonitoringBucket)
	for rows.Next() {
		var item domain.MonitoringBucket
		var p50Latency, p95Latency, p50TTFT, p95TTFT, p95Queue, p95Generation sql.NullFloat64
		if err = rows.Scan(&item.StartedAt, &item.Requests, &item.Errors, &item.Fallbacks, &item.Retried, &item.Streaming, &item.TokenUsageSamples, &item.InputTokenSamples, &item.OutputTokenSamples, &item.InputTokens, &item.OutputTokens, &p50Latency, &p95Latency, &p50TTFT, &p95TTFT, &p95Queue, &p95Generation); err != nil {
			return err
		}
		item.StartedAt = item.StartedAt.UTC()
		seconds := bucket.Seconds()
		item.RequestsPerSecond = float64(item.Requests) / seconds
		if item.InputTokenSamples > 0 {
			item.InputTokensPerSecond = floatPointer(float64(item.InputTokens) / seconds)
		}
		if item.OutputTokenSamples > 0 {
			item.OutputTokensPerSecond = floatPointer(float64(item.OutputTokens) / seconds)
		}
		item.ErrorRate, item.FallbackRate = ratio(item.Errors, item.Requests), ratio(item.Fallbacks, item.Requests)
		item.P50LatencyMS, item.P95LatencyMS = nullableFloat(p50Latency), nullableFloat(p95Latency)
		item.P50TTFTMS, item.P95TTFTMS = nullableFloat(p50TTFT), nullableFloat(p95TTFT)
		item.P95QueueMS, item.P95GenerationMS = nullableFloat(p95Queue), nullableFloat(p95Generation)
		values[item.StartedAt.Unix()] = item
	}
	if err = rows.Err(); err != nil {
		return err
	}
	seconds := int64(bucket.Seconds())
	first := time.Unix((start.Unix()/seconds)*seconds, 0).UTC()
	last := time.Unix((end.Unix()/seconds)*seconds, 0).UTC()
	for stamp := first; !stamp.After(last); stamp = stamp.Add(bucket) {
		item, ok := values[stamp.Unix()]
		if !ok {
			item.StartedAt = stamp
		}
		out.Series = append(out.Series, item)
	}
	return nil
}

func (s *Store) monitoringBreakdowns(ctx context.Context, tenant, endpointID string, start, end time.Time, out *domain.EndpointMonitoringSnapshot) error {
	rows, err := s.QueryContext(ctx, `SELECT COALESCE(b.name,'unattributed'),COALESCE(d.name,''),COALESCE(r.revision_id,''),COALESCE(r.provider,''),COALESCE(r.runtime,''),COUNT(*),COUNT(*) FILTER(WHERE r.error_type IS NOT NULL OR r.status_code IS NULL OR r.status_code>=400),COUNT(*) FILTER(WHERE COALESCE(r.fallback_reason,'')<>''),percentile_cont(0.95) WITHIN GROUP(ORDER BY r.latency_ms) FILTER(WHERE r.latency_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP(ORDER BY r.ttft_ms) FILTER(WHERE r.ttft_ms IS NOT NULL),MAX(r.started_at) FROM request_records r LEFT JOIN backend_bindings b ON b.id=r.binding_id LEFT JOIN deployments d ON d.id=r.deployment_id WHERE r.tenant_id=? AND r.endpoint_id=? AND r.started_at>=? AND r.started_at<=? GROUP BY 1,2,3,4,5 ORDER BY COUNT(*) DESC,1,2,3 LIMIT 50`, tenant, endpointID, start, end)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item domain.MonitoringBreakdown
		var latency, ttft sql.NullFloat64
		if err = rows.Scan(&item.Binding, &item.Deployment, &item.Revision, &item.Provider, &item.Runtime, &item.Requests, &item.Errors, &item.Fallbacks, &latency, &ttft, &item.LastSeenAt); err != nil {
			return err
		}
		item.LastSeenAt = item.LastSeenAt.UTC()
		item.ErrorRate, item.P95LatencyMS, item.P95TTFTMS = ratio(item.Errors, item.Requests), nullableFloat(latency), nullableFloat(ttft)
		out.Breakdowns = append(out.Breakdowns, item)
	}
	return rows.Err()
}

func (s *Store) monitoringEvents(ctx context.Context, endpointID string, start, end time.Time, out *domain.EndpointMonitoringSnapshot) error {
	rows, err := s.QueryContext(ctx, `SELECT 'lifecycle',event_type,summary,payload_json::text,created_at FROM deployment_events WHERE deployment_id IN (SELECT DISTINCT deployment_id FROM backend_bindings WHERE endpoint_id=? AND deployment_id IS NOT NULL) AND created_at>=? AND created_at<=? ORDER BY created_at DESC LIMIT 200`, endpointID, start, end)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item domain.MonitoringEvent
		if err = rows.Scan(&item.Kind, &item.Type, &item.Summary, &item.DetailsJSON, &item.OccurredAt); err != nil {
			rows.Close()
			return err
		}
		item.OccurredAt = item.OccurredAt.UTC()
		item.Details = json.RawMessage(item.DetailsJSON)
		out.Events = append(out.Events, item)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	rows, err = s.QueryContext(ctx, `SELECT 'scaling',action,reason,signals_json::text,created_at FROM scaling_decisions WHERE deployment_id IN (SELECT DISTINCT deployment_id FROM backend_bindings WHERE endpoint_id=? AND deployment_id IS NOT NULL) AND created_at>=? AND created_at<=? ORDER BY created_at DESC LIMIT 200`, endpointID, start, end)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item domain.MonitoringEvent
		if err = rows.Scan(&item.Kind, &item.Type, &item.Summary, &item.DetailsJSON, &item.OccurredAt); err != nil {
			rows.Close()
			return err
		}
		item.OccurredAt = item.OccurredAt.UTC()
		item.Details = json.RawMessage(item.DetailsJSON)
		out.Events = append(out.Events, item)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	rows, err = s.QueryContext(ctx, `SELECT 'release_guard',LOWER(decision),'Endpoint Release Guard '||decision,reason_codes_json::text,created_at FROM endpoint_release_guard_evaluations WHERE endpoint_id=? AND created_at>=? AND created_at<=? ORDER BY created_at DESC LIMIT 100`, endpointID, start, end)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item domain.MonitoringEvent
		if err = rows.Scan(&item.Kind, &item.Type, &item.Summary, &item.DetailsJSON, &item.OccurredAt); err != nil {
			return err
		}
		item.OccurredAt = item.OccurredAt.UTC()
		item.Details = json.RawMessage(item.DetailsJSON)
		out.Events = append(out.Events, item)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	sort.SliceStable(out.Events, func(i, j int) bool { return out.Events[i].OccurredAt.After(out.Events[j].OccurredAt) })
	if len(out.Events) > 200 {
		out.Events = out.Events[:200]
	}
	return nil
}

func nullableFloat(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	out := value.Float64
	return &out
}

func ratio(numerator, denominator int) *float64 {
	if denominator == 0 {
		return nil
	}
	value := float64(numerator) / float64(denominator)
	return &value
}

func floatPointer(value float64) *float64 { return &value }

func removeString(values []string, target string) []string {
	out := values[:0]
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}
