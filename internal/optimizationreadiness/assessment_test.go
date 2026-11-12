package optimizationreadiness

import (
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/metrics"
)

func TestAssessmentRequiresFreshRepresentativeEvidence(t *testing.T) {
	snapshot := fixtureSnapshot()
	snapshot.Evidence.Fresh = false
	assessment := Assess(snapshot, Policy{})
	if assessment.State != "collecting" || assessment.PrimaryBottleneck != "unknown" || assessment.KernelGate.Eligible || assessment.ProviderMutation {
		t.Fatalf("stale evidence crossed readiness boundary: %+v", assessment)
	}
	snapshot.Evidence.Fresh = true
	snapshot.Summary.Requests, snapshot.Evidence.SampleCount = 5, 5
	assessment = Assess(snapshot, Policy{})
	if assessment.State != "collecting" || assessment.Confidence != "insufficient" {
		t.Fatalf("small sample crossed readiness boundary: %+v", assessment)
	}
}

func TestAssessmentDiagnosesServingStageWithoutClaimingKernel(t *testing.T) {
	tests := []struct {
		name, expected string
		mutate         func(*domain.EndpointMonitoringSnapshot)
	}{
		{"reliability", "reliability", func(s *domain.EndpointMonitoringSnapshot) { value := .05; s.Summary.ErrorRate = &value }},
		{"queue", "queueing-and-admission", func(s *domain.EndpointMonitoringSnapshot) { value := 250.0; s.Summary.P95QueueMS = &value }},
		{"prefill", "prefill-and-first-token", func(s *domain.EndpointMonitoringSnapshot) { value := 500.0; s.Summary.P95TTFTMS = &value }},
		{"decode", "decode-and-generation", func(s *domain.EndpointMonitoringSnapshot) { value := 700.0; s.Summary.P95GenerationMS = &value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := fixtureSnapshot()
			test.mutate(&snapshot)
			assessment := Assess(snapshot, Policy{})
			if assessment.PrimaryBottleneck != test.expected || assessment.KernelGate.Eligible || assessment.ContentRecorded {
				t.Fatalf("assessment=%+v", assessment)
			}
			wantPhase := map[string]string{"prefill": "prefill", "decode": "decode"}[test.name]
			if assessment.ProfilePhase != wantPhase {
				t.Fatalf("phase=%q want %q", assessment.ProfilePhase, wantPhase)
			}
		})
	}
}

func fixtureSnapshot() domain.EndpointMonitoringSnapshot {
	now := time.Now().UTC()
	zero, latency := 0.0, 1000.0
	return domain.EndpointMonitoringSnapshot{
		Endpoint: "coder-production", WindowStart: now.Add(-time.Hour), WindowEnd: now, BucketSeconds: 60,
		Summary:  domain.MonitoringSummary{Requests: 100, ErrorRate: &zero, P95LatencyMS: &latency},
		Evidence: domain.MonitoringEvidence{Source: "infercrane_gateway_request_records", SampleCount: 100, Fresh: true, ContentRecorded: false},
	}
}

type engineValue struct {
	value   float64
	samples int
}

func withEngine(snapshot domain.EndpointMonitoringSnapshot, values map[string]engineValue) domain.EndpointMonitoringSnapshot {
	for name, item := range values {
		value := item.value
		samples := item.samples
		if samples == 0 {
			samples = 1
		}
		source := "vllm_metrics"
		if name == "shared_prefix_request_ratio" {
			source = "infercrane_gateway_request_records"
		}
		snapshot.Evidence.Measurements = append(snapshot.Evidence.Measurements, domain.MeasurementEvidence{Name: name, Value: &value, Availability: "available", EvidenceClass: "measured", Source: source, SampleCount: samples})
	}
	return snapshot
}

func findingRules(assessment Assessment) []string {
	rules := make([]string, 0, len(assessment.Findings))
	for _, finding := range assessment.Findings {
		rules = append(rules, finding.Rule)
	}
	return rules
}

func leadingFamilies(t *testing.T, assessment Assessment, want ...string) {
	t.Helper()
	if len(assessment.CandidateFamilies) < len(want) {
		t.Fatalf("families=%v want prefix %v", assessment.CandidateFamilies, want)
	}
	for index, family := range want {
		if assessment.CandidateFamilies[index] != family {
			t.Fatalf("families=%v want prefix %v", assessment.CandidateFamilies, want)
		}
	}
	seen := map[string]bool{}
	for _, family := range assessment.CandidateFamilies {
		if seen[family] {
			t.Fatalf("duplicate family %q in %v", family, assessment.CandidateFamilies)
		}
		seen[family] = true
	}
}

func TestKVCachePressureRules(t *testing.T) {
	tests := []struct {
		name   string
		engine map[string]engineValue
		queue  *float64
		rules  []string
	}{
		{"preemptions above one percent", map[string]engineValue{metrics.MeasurementPreemptions: {value: 5}, metrics.MeasurementRequestsCompleted: {value: 100}}, nil, []string{RuleKVPreemption}},
		{"preemptions without completions", map[string]engineValue{metrics.MeasurementPreemptions: {value: 2}, metrics.MeasurementRequestsCompleted: {value: 0}}, nil, []string{RuleKVPreemption}},
		{"saturated cache with engine waiting", map[string]engineValue{metrics.MeasurementKVCacheUsage: {value: 95}, metrics.MeasurementRequestsWaiting: {value: 3}}, nil, []string{RuleKVSaturation}},
		{"saturated cache with gateway queueing", map[string]engineValue{metrics.MeasurementKVCacheUsage: {value: 91}}, floatPointer(300), []string{RuleKVSaturation}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := withEngine(fixtureSnapshot(), test.engine)
			snapshot.Summary.P95QueueMS = test.queue
			assessment := Assess(snapshot, Policy{})
			if assessment.PrimaryBottleneck != "kv-cache-pressure" || assessment.Confidence != "stage-and-engine-level" || assessment.KernelGate.Eligible {
				t.Fatalf("assessment=%+v", assessment)
			}
			if got := findingRules(assessment); len(got) != len(test.rules) || got[0] != test.rules[0] {
				t.Fatalf("rules=%v want %v", got, test.rules)
			}
			leadingFamilies(t, assessment, "FP8 KV cache", "KV cache offload")
		})
	}
	for name, engine := range map[string]map[string]engineValue{
		"preemptions below one percent": {metrics.MeasurementPreemptions: {value: 1}, metrics.MeasurementRequestsCompleted: {value: 500}},
		"full cache without waiting":    {metrics.MeasurementKVCacheUsage: {value: 97}, metrics.MeasurementRequestsWaiting: {value: 0}},
		"waiting below saturation":      {metrics.MeasurementKVCacheUsage: {value: 80}, metrics.MeasurementRequestsWaiting: {value: 4}},
		"preemptions without window":    {metrics.MeasurementPreemptions: {value: 9}},
	} {
		t.Run("no "+name, func(t *testing.T) {
			assessment := Assess(withEngine(fixtureSnapshot(), engine), Policy{})
			if assessment.PrimaryBottleneck == "kv-cache-pressure" || len(assessment.Findings) != 0 {
				t.Fatalf("false KV diagnosis: %+v", assessment)
			}
		})
	}
}

func TestEngineQueueAndTTFTSignalsFillGatewayGaps(t *testing.T) {
	queued := Assess(withEngine(fixtureSnapshot(), map[string]engineValue{metrics.MeasurementQueueTimeP95: {value: 300, samples: 100}}), Policy{})
	if queued.PrimaryBottleneck != "queueing-and-admission" || len(queued.Findings) != 1 || queued.Findings[0].Rule != RuleEngineQueue || queued.Findings[0].Threshold != 200 {
		t.Fatalf("engine queue=%+v", queued)
	}
	leadingFamilies(t, queued, "batch-token budget", "continuous batching", "replica scaling")
	prefill := Assess(withEngine(fixtureSnapshot(), map[string]engineValue{metrics.MeasurementTTFTP95: {value: 600, samples: 100}}), Policy{})
	if prefill.PrimaryBottleneck != "prefill-and-first-token" || prefill.ProfilePhase != "prefill" {
		t.Fatalf("engine TTFT fallback=%+v", prefill)
	}
}

func TestLongInputPrefillRule(t *testing.T) {
	snapshot := fixtureSnapshot()
	ttft := 500.0
	snapshot.Summary.P95TTFTMS = &ttft
	snapshot.Summary.InputTokens, snapshot.Summary.InputTokenSamples = 300000, 100
	assessment := Assess(snapshot, Policy{})
	if assessment.PrimaryBottleneck != "prefill-and-first-token" || len(assessment.Findings) != 1 || assessment.Findings[0].Rule != RuleLongInputPrefill || assessment.Findings[0].Observed != 3000 {
		t.Fatalf("long input=%+v", assessment)
	}
	leadingFamilies(t, assessment, "chunked prefill", "batch-token budget", "prefix cache")

	snapshot.Summary.InputTokens = 50000
	short := Assess(snapshot, Policy{})
	if len(short.Findings) != 0 {
		t.Fatalf("short inputs fired %v", findingRules(short))
	}
	leadingFamilies(t, short, "prefix cache", "chunked prefill")
}

func TestLowConcurrencyDecodeRule(t *testing.T) {
	decode := func(values map[string]engineValue) Assessment {
		snapshot := withEngine(fixtureSnapshot(), values)
		generation := 700.0
		snapshot.Summary.P95GenerationMS = &generation
		return Assess(snapshot, Policy{})
	}
	low := decode(map[string]engineValue{metrics.MeasurementRequestsRunning: {value: 6, samples: 2}})
	if low.PrimaryBottleneck != "decode-and-generation" || len(low.Findings) != 1 || low.Findings[0].Rule != RuleLowConcurrency || low.Findings[0].Observed != 3 {
		t.Fatalf("low concurrency=%+v", low)
	}
	leadingFamilies(t, low, "speculative decoding", "weight quantization")

	accepted := decode(map[string]engineValue{metrics.MeasurementRequestsRunning: {value: 2, samples: 1}, metrics.MeasurementSpecDecodeAccept: {value: .8, samples: 4000}})
	leadingFamilies(t, accepted, "weight quantization", "speculative decoding")

	busy := decode(map[string]engineValue{metrics.MeasurementRequestsRunning: {value: 80, samples: 2}})
	if len(busy.Findings) != 0 {
		t.Fatalf("busy decode fired %v", findingRules(busy))
	}
	leadingFamilies(t, busy, "KV cache")

	unknown := decode(nil)
	if len(unknown.Findings) != 0 || !containsString(unknown.RequiredEvidence, "engine running requests per replica") {
		t.Fatalf("missing concurrency must be required evidence, not assumed: %+v", unknown)
	}
}

func TestPrefixReuseRule(t *testing.T) {
	prefill := func(values map[string]engineValue) Assessment {
		snapshot := withEngine(fixtureSnapshot(), values)
		ttft := 500.0
		snapshot.Summary.P95TTFTMS = &ttft
		return Assess(snapshot, Policy{})
	}
	poor := prefill(map[string]engineValue{"shared_prefix_request_ratio": {value: .7, samples: 100}, metrics.MeasurementPrefixCacheHitRate: {value: .05, samples: 50000}})
	if len(poor.Findings) != 1 || poor.Findings[0].Rule != RulePrefixReuse {
		t.Fatalf("poor reuse=%+v", poor)
	}
	leadingFamilies(t, poor, "prefix cache", "prefix-aware routing", "chunked prefill")

	for name, values := range map[string]map[string]engineValue{
		"good hit rate":    {"shared_prefix_request_ratio": {value: .7, samples: 100}, metrics.MeasurementPrefixCacheHitRate: {value: .6, samples: 50000}},
		"unique prompts":   {"shared_prefix_request_ratio": {value: .1, samples: 100}, metrics.MeasurementPrefixCacheHitRate: {value: .01, samples: 50000}},
		"too few tokens":   {"shared_prefix_request_ratio": {value: .7, samples: 100}, metrics.MeasurementPrefixCacheHitRate: {value: .01, samples: 50}},
		"hit rate unknown": {"shared_prefix_request_ratio": {value: .7, samples: 100}},
	} {
		t.Run(name, func(t *testing.T) {
			assessment := prefill(values)
			if len(assessment.Findings) != 0 {
				t.Fatalf("fired %v", findingRules(assessment))
			}
			wantEvidence := name == "too few tokens" || name == "hit rate unknown"
			if containsString(assessment.RequiredEvidence, "engine prefix-cache hit rate") != wantEvidence {
				t.Fatalf("required evidence=%v", assessment.RequiredEvidence)
			}
		})
	}

	// The reuse finding is independent of the stage. Measured findings lead
	// the generic queue families, which follow unchanged.
	snapshot := withEngine(fixtureSnapshot(), map[string]engineValue{"shared_prefix_request_ratio": {value: .9, samples: 100}, metrics.MeasurementPrefixCacheHitRate: {value: .1, samples: 50000}})
	queue := 300.0
	snapshot.Summary.P95QueueMS = &queue
	queued := Assess(snapshot, Policy{})
	leadingFamilies(t, queued, "prefix cache", "prefix-aware routing", "continuous batching")
}

func TestSpeculativeAcceptanceRule(t *testing.T) {
	low := Assess(withEngine(fixtureSnapshot(), map[string]engineValue{metrics.MeasurementSpecDecodeAccept: {value: .3, samples: 2000}}), Policy{})
	if low.PrimaryBottleneck != "unattributed-serving-time" || len(low.Findings) != 1 || low.Findings[0].Rule != RuleSpeculativeAccept {
		t.Fatalf("low acceptance=%+v", low)
	}
	leadingFamilies(t, low, "speculative method or draft model", "speculation length", "runtime")
	for _, value := range []engineValue{{value: .3, samples: 20}, {value: .7, samples: 2000}} {
		if assessment := Assess(withEngine(fixtureSnapshot(), map[string]engineValue{metrics.MeasurementSpecDecodeAccept: value}), Policy{}); len(assessment.Findings) != 0 {
			t.Fatalf("acceptance %+v fired %v", value, findingRules(assessment))
		}
	}
}

func TestEngineFindingsRespectEvidenceGates(t *testing.T) {
	pressure := map[string]engineValue{metrics.MeasurementPreemptions: {value: 50}, metrics.MeasurementRequestsCompleted: {value: 100}}
	failing := withEngine(fixtureSnapshot(), pressure)
	errorRate := .1
	failing.Summary.ErrorRate = &errorRate
	if assessment := Assess(failing, Policy{}); assessment.PrimaryBottleneck != "reliability" || len(assessment.Findings) != 0 {
		t.Fatalf("errors must block engine tuning: %+v", assessment)
	}
	stale := withEngine(fixtureSnapshot(), pressure)
	stale.Evidence.Fresh = false
	if assessment := Assess(stale, Policy{}); assessment.State != "collecting" || len(assessment.Findings) != 0 {
		t.Fatalf("stale request evidence must not authorize engine findings: %+v", assessment)
	}
	// Unavailable engine evidence is never read as zero.
	unavailable := fixtureSnapshot()
	unavailable.Evidence.Measurements = append(unavailable.Evidence.Measurements, domain.MeasurementEvidence{Name: metrics.MeasurementKVCacheUsage, Availability: "not_observed"})
	assessment := Assess(unavailable, Policy{})
	if assessment.Confidence != "stage-level" {
		t.Fatalf("confidence=%s", assessment.Confidence)
	}
	for _, signal := range assessment.Signals {
		if strings.HasPrefix(signal.Name, "runtime_") && (signal.Availability != "unavailable" || signal.Value != nil) {
			t.Fatalf("absent engine signal reported a value: %+v", signal)
		}
	}
}

func TestPolicyDefaultsAreExplicit(t *testing.T) {
	if got := normalizePolicy(Policy{}); got != DefaultPolicy() {
		t.Fatalf("zero policy=%+v", got)
	}
	invalid := Policy{KVCacheUsagePercent: 150, PreemptionRate: 2, SharedPrefixRatio: -1, PrefixHitRateFloor: 1, SpecAcceptanceFloor: 3, LowConcurrency: -2, LongInputTokens: -1, MinimumEngineSamples: -5}
	if got := normalizePolicy(invalid); got != DefaultPolicy() {
		t.Fatalf("invalid thresholds were kept: %+v", got)
	}
	custom := DefaultPolicy()
	custom.KVCacheUsagePercent, custom.LowConcurrency = 80, 8
	if got := normalizePolicy(custom); got != custom {
		t.Fatalf("valid custom policy changed: %+v", got)
	}
}

func floatPointer(value float64) *float64 { return &value }

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
