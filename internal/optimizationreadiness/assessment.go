// Package optimizationreadiness turns content-free production monitoring into
// a conservative next profiling action. It diagnoses serving-stage symptoms
// from gateway request evidence and, when a fresh scrape is available, from
// the serving engine's own metrics. It never claims a GPU operator or kernel
// bottleneck without profiler data, and a technique family it returns is a
// candidate to measure, never a change to apply.
package optimizationreadiness

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/metrics"
)

const (
	SchemaVersion    = "infercrane.optimization-readiness/v1"
	AlgorithmVersion = "monitoring-stage-triage-v2"
)

// Policy holds every diagnosis threshold. The defaults are documented in
// docs/features/telemetry.md; a zero or out-of-range value falls back to the
// default instead of disabling a rule.
type Policy struct {
	MinimumRequests int     `json:"minimum_requests"`
	MaxErrorRate    float64 `json:"max_error_rate"`
	QueueShare      float64 `json:"queue_share"`
	TTFTShare       float64 `json:"ttft_share"`
	GenerationShare float64 `json:"generation_share"`

	// KVCacheUsagePercent is the engine KV-cache occupancy treated as
	// saturated when requests are also waiting.
	KVCacheUsagePercent float64 `json:"kv_cache_usage_percent"`
	// PreemptionRate is preemptions per completed request in the engine
	// window that marks KV-cache pressure on its own.
	PreemptionRate float64 `json:"preemption_rate"`
	// LongInputTokens is the mean reported input length at which prefill
	// pressure points at the batched-token budget and chunked prefill first.
	LongInputTokens float64 `json:"long_input_tokens"`
	// LowConcurrency is the running requests per replica at or below which
	// decode is treated as latency-bound rather than throughput-bound.
	LowConcurrency float64 `json:"low_concurrency"`
	// SharedPrefixRatio is the share of known-shape chat requests that repeat
	// another request's system prefix.
	SharedPrefixRatio float64 `json:"shared_prefix_ratio"`
	// PrefixHitRateFloor is the token-weighted engine prefix-cache hit rate
	// below which shared prefixes are considered poorly reused.
	PrefixHitRateFloor float64 `json:"prefix_hit_rate_floor"`
	// SpecAcceptanceFloor is the draft-token acceptance rate below which the
	// current speculative-decoding setup is unlikely to pay for its drafts.
	SpecAcceptanceFloor float64 `json:"spec_acceptance_floor"`
	// MinimumEngineSamples is the smallest token or draft denominator an
	// engine ratio needs before a rule may use it.
	MinimumEngineSamples int `json:"minimum_engine_samples"`
}

type Signal struct {
	Name           string   `json:"name"`
	Availability   string   `json:"availability"`
	Value          *float64 `json:"value,omitempty"`
	Unit           string   `json:"unit,omitempty"`
	Interpretation string   `json:"interpretation"`
}

// Finding records one engine-signal rule that fired: the observed value, the
// explicit threshold it crossed, and the technique families worth measuring
// because of it.
type Finding struct {
	Rule              string   `json:"rule"`
	Signal            string   `json:"signal"`
	Observed          float64  `json:"observed"`
	Comparison        string   `json:"comparison"`
	Threshold         float64  `json:"threshold"`
	Unit              string   `json:"unit"`
	TechniqueFamilies []string `json:"technique_families"`
	Explanation       string   `json:"explanation"`
}

type KernelGate struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}

type Assessment struct {
	SchemaVersion     string     `json:"schema_version"`
	AlgorithmVersion  string     `json:"algorithm_version"`
	Endpoint          string     `json:"endpoint"`
	State             string     `json:"state"`
	PrimaryBottleneck string     `json:"primary_bottleneck"`
	ProfilePhase      string     `json:"profile_phase,omitempty"`
	Confidence        string     `json:"confidence"`
	Signals           []Signal   `json:"signals"`
	Findings          []Finding  `json:"findings"`
	NextAction        string     `json:"next_action"`
	CandidateFamilies []string   `json:"candidate_families"`
	RequiredEvidence  []string   `json:"required_evidence"`
	KernelGate        KernelGate `json:"kernel_gate"`
	ContentRecorded   bool       `json:"content_recorded"`
	ProviderMutation  bool       `json:"provider_mutation"`
}

// Rule identifiers for engine-signal findings.
const (
	RuleKVPreemption       = "kv_cache.preemption"
	RuleKVSaturation       = "kv_cache.saturation"
	RuleEngineQueue        = "queue.engine_wait"
	RuleLongInputPrefill   = "prefill.long_input"
	RuleLowConcurrency     = "decode.low_concurrency"
	RulePrefixReuse        = "prefix.low_reuse"
	RuleSpeculativeAccept  = "speculative.low_acceptance"
	signalSharedPrefix     = "shared_prefix_request_ratio"
	signalPreemptionRate   = "runtime_preemptions_per_completed_request"
	signalRunningPerWorker = "runtime_requests_running_per_replica"
)

func DefaultPolicy() Policy {
	return Policy{
		MinimumRequests: 20, MaxErrorRate: .02, QueueShare: .20, TTFTShare: .40, GenerationShare: .50,
		KVCacheUsagePercent: 90, PreemptionRate: .01, LongInputTokens: 2048, LowConcurrency: 4,
		SharedPrefixRatio: .5, PrefixHitRateFloor: .25, SpecAcceptanceFloor: .5, MinimumEngineSamples: 100,
	}
}

func Assess(snapshot domain.EndpointMonitoringSnapshot, policy Policy) Assessment {
	policy = normalizePolicy(policy)
	result := Assessment{
		SchemaVersion: SchemaVersion, AlgorithmVersion: AlgorithmVersion, Endpoint: snapshot.Endpoint,
		State: "collecting", PrimaryBottleneck: "unknown", Confidence: "insufficient",
		NextAction:        "collect fresh request and token telemetry before starting a paid optimization campaign",
		CandidateFamilies: []string{}, Findings: []Finding{}, RequiredEvidence: []string{"fresh request window", "representative workload distribution", "runtime and accelerator identity"},
		KernelGate:      KernelGate{Eligible: false, Reason: "request-level monitoring cannot establish a GPU operator hotspot; an exact target-GPU profile and Amdahl gate are required"},
		ContentRecorded: false, ProviderMutation: false,
	}
	engine := engineEvidence(snapshot)
	result.Signals = monitoringSignals(snapshot, engine)
	if !snapshot.Evidence.Fresh {
		result.NextAction = "refresh monitoring evidence; stale observations cannot authorize optimization spend"
		return result
	}
	if snapshot.Summary.Requests < policy.MinimumRequests || snapshot.Evidence.SampleCount < policy.MinimumRequests {
		result.NextAction = "collect at least " + strconv.Itoa(policy.MinimumRequests) + " representative requests before profiling"
		return result
	}

	result.State, result.Confidence = "profile-ready", "stage-level"
	latency := snapshot.Summary.P95LatencyMS
	if snapshot.Summary.ErrorRate != nil && *snapshot.Summary.ErrorRate > policy.MaxErrorRate {
		result.State, result.PrimaryBottleneck = "reliability-blocked", "reliability"
		result.NextAction = "resolve serving errors before performance tuning so failed requests do not bias the benchmark"
		result.CandidateFamilies = []string{"runtime compatibility", "capacity and health", "request validation"}
		result.RequiredEvidence = []string{"error taxonomy", "runtime logs", "provider health", "passing correctness workload"}
		return result
	}
	if engine.available() {
		result.Confidence = "stage-and-engine-level"
	}

	queuePressure := admissionPressure(snapshot.Admission) || meaningfulShare(snapshot.Summary.P95QueueMS, latency, policy.QueueShare, 25)
	engineQueue := engine.value(metrics.MeasurementQueueTimeP95)
	engineQueuePressure := meaningfulShare(engineQueue, latency, policy.QueueShare, 25)
	kv := kvPressure(engine, policy, queuePressure || engineQueuePressure)
	prefix := prefixReuse(snapshot, engine, policy)
	speculative := speculativeAcceptance(engine, policy)
	secondary := append(append([]Finding{}, prefix...), speculative...)
	var missingEvidence []string
	if needsPrefixHitRate(snapshot, engine, policy) {
		missingEvidence = append(missingEvidence, "engine prefix-cache hit rate")
	}

	if len(kv) > 0 {
		result.PrimaryBottleneck = "kv-cache-pressure"
		result.NextAction = "replay the workload against FP8 KV-cache and KV offload candidates and compare preemptions, queue time, and quality before adding replicas"
		result.RequiredEvidence = []string{"KV-cache usage and preemptions on the replayed workload", "output-length distribution", "quality evaluation for reduced-precision KV", "SLO-qualified goodput", "sourced cost"}
		result.finish(kv, secondary, []string{"replica scaling"}, missingEvidence)
		return result
	}
	if queuePressure || engineQueuePressure {
		result.PrimaryBottleneck = "queueing-and-admission"
		result.NextAction = "replay the workload with AIPerf and profile scheduler, batching, replica, and admission settings before kernel work"
		result.RequiredEvidence = []string{"arrival distribution", "concurrency sweep", "queue p95", "SLO-qualified goodput", "sourced cost"}
		var primary []Finding
		if engineQueuePressure && !queuePressure {
			primary = append(primary, Finding{
				Rule: RuleEngineQueue, Signal: metrics.MeasurementQueueTimeP95, Observed: *engineQueue, Comparison: ">=", Threshold: policy.QueueShare * *latency, Unit: "ms",
				TechniqueFamilies: []string{"batch-token budget", "continuous batching", "replica scaling"},
				Explanation:       "requests wait inside the engine scheduler for a material share of gateway p95 latency",
			})
		}
		result.finish(primary, secondary, []string{"continuous batching", "batch-token budget", "admission policy", "replica scaling"}, missingEvidence)
		return result
	}
	ttft := snapshot.Summary.P95TTFTMS
	if ttft == nil {
		ttft = engine.value(metrics.MeasurementTTFTP95)
	}
	if meaningfulShare(ttft, latency, policy.TTFTShare, 50) {
		result.PrimaryBottleneck, result.ProfilePhase = "prefill-and-first-token", "prefill"
		result.NextAction = "capture prefill/decode timing and prefix-reuse evidence, then compare cache, chunked-prefill, runtime, and attention candidates"
		result.RequiredEvidence = []string{"input-length distribution", "prefix reuse", "prefill GPU trace", "AIPerf TTFT lanes"}
		var primary []Finding
		if mean := meanInputTokens(snapshot); mean != nil && *mean >= policy.LongInputTokens {
			primary = append(primary, Finding{
				Rule: RuleLongInputPrefill, Signal: "mean_input_tokens", Observed: *mean, Comparison: ">=", Threshold: policy.LongInputTokens, Unit: "tokens",
				TechniqueFamilies: []string{"chunked prefill", "batch-token budget"},
				Explanation:       "long inputs dominate first-token time; the per-step batched-token budget decides how prefill interleaves with decode",
			})
		}
		// Shared prefixes reduce prefill work directly, so they lead here.
		primary = append(primary, prefix...)
		result.finish(primary, speculative, []string{"prefix cache", "chunked prefill", "attention backend", "prefill scheduling"}, missingEvidence)
		return result
	}
	if meaningfulShare(snapshot.Summary.P95GenerationMS, latency, policy.GenerationShare, 50) {
		result.PrimaryBottleneck, result.ProfilePhase = "decode-and-generation", "decode"
		result.NextAction = "capture decode throughput, KV-cache, and GPU operator profiles, then compare decoding, cache, quantization, and runtime candidates"
		result.RequiredEvidence = []string{"output-length distribution", "decode tokens/second", "KV-cache pressure", "target-GPU operator profile"}
		var primary []Finding
		if running, ok := runningPerReplica(engine); ok && running <= policy.LowConcurrency {
			families := []string{"speculative decoding", "weight quantization"}
			if acceptance := engine.value(metrics.MeasurementSpecDecodeAccept); acceptance != nil && *acceptance >= policy.SpecAcceptanceFloor {
				// Speculation already pays off; weight precision is the next lever.
				families = []string{"weight quantization", "speculative decoding"}
			}
			primary = append(primary, Finding{
				Rule: RuleLowConcurrency, Signal: signalRunningPerWorker, Observed: running, Comparison: "<=", Threshold: policy.LowConcurrency, Unit: "requests",
				TechniqueFamilies: families,
				Explanation:       "few concurrent sequences leave decode memory-bandwidth bound, where fewer target-model steps or smaller weights cut time per token",
			})
		} else if !ok {
			result.RequiredEvidence = append(result.RequiredEvidence, "engine running requests per replica")
		}
		result.finish(primary, secondary, []string{"KV cache", "speculative decoding", "quantization", "decode attention", "sampling"}, missingEvidence)
		return result
	}
	result.PrimaryBottleneck = "unattributed-serving-time"
	result.NextAction = "run a bounded AIPerf replay with Nsight Systems or runtime profiling to attribute CPU, transfer, prefill, decode, and communication time"
	result.RequiredEvidence = []string{"AIPerf replay", "CPU/GPU timeline", "prefill/decode split", "cost per successful output"}
	result.finish(nil, secondary, []string{"runtime", "scheduling", "cache", "precision", "hardware"}, missingEvidence)
	return result
}

// finish orders candidate families: families from findings about the primary
// bottleneck first, then families from independent findings, then the
// stage's default families. Duplicates keep their first position.
func (a *Assessment) finish(primary, secondary []Finding, defaults, missingEvidence []string) {
	a.Findings = append(append([]Finding{}, primary...), secondary...)
	a.RequiredEvidence = append(a.RequiredEvidence, missingEvidence...)
	seen := map[string]struct{}{}
	families := []string{}
	add := func(values ...string) {
		for _, value := range values {
			key := strings.ToLower(value)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			families = append(families, value)
		}
	}
	for _, finding := range primary {
		add(finding.TechniqueFamilies...)
	}
	for _, finding := range secondary {
		add(finding.TechniqueFamilies...)
	}
	add(defaults...)
	a.CandidateFamilies = families
}

func kvPressure(engine engineSignals, policy Policy, waiting bool) []Finding {
	var findings []Finding
	if rate, ok := preemptionRate(engine); ok && rate >= policy.PreemptionRate {
		findings = append(findings, Finding{
			Rule: RuleKVPreemption, Signal: signalPreemptionRate, Observed: rate, Comparison: ">=", Threshold: policy.PreemptionRate, Unit: "ratio",
			TechniqueFamilies: []string{"FP8 KV cache", "KV cache offload", "max-sequence budget"},
			Explanation:       "the engine evicts running sequences because the KV cache is full, which wastes recomputation and inflates latency",
		})
	}
	usage := engine.value(metrics.MeasurementKVCacheUsage)
	engineWaiting := engine.value(metrics.MeasurementRequestsWaiting)
	if usage != nil && *usage >= policy.KVCacheUsagePercent && (waiting || engineWaiting != nil && *engineWaiting > 0) {
		findings = append(findings, Finding{
			Rule: RuleKVSaturation, Signal: metrics.MeasurementKVCacheUsage, Observed: *usage, Comparison: ">=", Threshold: policy.KVCacheUsagePercent, Unit: "percent",
			TechniqueFamilies: []string{"FP8 KV cache", "KV cache offload"},
			Explanation:       "the KV cache is nearly full while requests wait, so admission is limited by KV capacity rather than compute",
		})
	}
	return findings
}

func prefixReuse(snapshot domain.EndpointMonitoringSnapshot, engine engineSignals, policy Policy) []Finding {
	shared := sharedPrefix(snapshot)
	hit, hitSamples := engine.withSamples(metrics.MeasurementPrefixCacheHitRate)
	if shared == nil || *shared < policy.SharedPrefixRatio || hit == nil || hitSamples < policy.MinimumEngineSamples || *hit >= policy.PrefixHitRateFloor {
		return nil
	}
	return []Finding{{
		Rule: RulePrefixReuse, Signal: metrics.MeasurementPrefixCacheHitRate, Observed: *hit, Comparison: "<", Threshold: policy.PrefixHitRateFloor, Unit: "ratio",
		TechniqueFamilies: []string{"prefix cache", "prefix-aware routing"},
		Explanation:       "most requests share a system prefix but the engine rarely reuses cached prefix tokens; the cache may be off, too small, or split across replicas",
	}}
}

func needsPrefixHitRate(snapshot domain.EndpointMonitoringSnapshot, engine engineSignals, policy Policy) bool {
	shared := sharedPrefix(snapshot)
	hit, samples := engine.withSamples(metrics.MeasurementPrefixCacheHitRate)
	return shared != nil && *shared >= policy.SharedPrefixRatio && (hit == nil || samples < policy.MinimumEngineSamples)
}

func speculativeAcceptance(engine engineSignals, policy Policy) []Finding {
	acceptance, samples := engine.withSamples(metrics.MeasurementSpecDecodeAccept)
	if acceptance == nil || samples < policy.MinimumEngineSamples || *acceptance >= policy.SpecAcceptanceFloor {
		return nil
	}
	return []Finding{{
		Rule: RuleSpeculativeAccept, Signal: metrics.MeasurementSpecDecodeAccept, Observed: *acceptance, Comparison: "<", Threshold: policy.SpecAcceptanceFloor, Unit: "ratio",
		TechniqueFamilies: []string{"speculative method or draft model", "speculation length"},
		Explanation:       "most draft tokens are rejected, so speculative decoding spends draft compute without saving target-model steps",
	}}
}

func normalizePolicy(policy Policy) Policy {
	defaults := DefaultPolicy()
	if policy.MinimumRequests <= 0 {
		policy.MinimumRequests = defaults.MinimumRequests
	}
	if policy.MaxErrorRate <= 0 || policy.MaxErrorRate > 1 || math.IsNaN(policy.MaxErrorRate) {
		policy.MaxErrorRate = defaults.MaxErrorRate
	}
	fraction := func(value *float64, fallback float64) {
		if !(*value > 0 && *value < 1) {
			*value = fallback
		}
	}
	fraction(&policy.QueueShare, defaults.QueueShare)
	fraction(&policy.TTFTShare, defaults.TTFTShare)
	fraction(&policy.GenerationShare, defaults.GenerationShare)
	fraction(&policy.PreemptionRate, defaults.PreemptionRate)
	fraction(&policy.SharedPrefixRatio, defaults.SharedPrefixRatio)
	fraction(&policy.PrefixHitRateFloor, defaults.PrefixHitRateFloor)
	fraction(&policy.SpecAcceptanceFloor, defaults.SpecAcceptanceFloor)
	if !(policy.KVCacheUsagePercent > 0 && policy.KVCacheUsagePercent <= 100) {
		policy.KVCacheUsagePercent = defaults.KVCacheUsagePercent
	}
	if !(policy.LongInputTokens > 0) || math.IsInf(policy.LongInputTokens, 0) {
		policy.LongInputTokens = defaults.LongInputTokens
	}
	if !(policy.LowConcurrency > 0) || math.IsInf(policy.LowConcurrency, 0) {
		policy.LowConcurrency = defaults.LowConcurrency
	}
	if policy.MinimumEngineSamples <= 0 {
		policy.MinimumEngineSamples = defaults.MinimumEngineSamples
	}
	return policy
}

// engineSignals holds the fresh engine measurements from the monitoring
// snapshot. Anything not available there is absent here.
type engineSignals map[string]domain.MeasurementEvidence

func engineEvidence(snapshot domain.EndpointMonitoringSnapshot) engineSignals {
	out := engineSignals{}
	for _, measurement := range snapshot.Evidence.Measurements {
		if measurement.Availability != "available" || measurement.Value == nil {
			continue
		}
		for _, item := range metrics.EngineMeasurementCatalog {
			if item.Name == measurement.Name {
				out[measurement.Name] = measurement
			}
		}
	}
	return out
}

func (e engineSignals) available() bool { return len(e) > 0 }

func (e engineSignals) value(name string) *float64 {
	if measurement, ok := e[name]; ok {
		value := *measurement.Value
		return &value
	}
	return nil
}

func (e engineSignals) withSamples(name string) (*float64, int) {
	measurement, ok := e[name]
	if !ok {
		return nil, 0
	}
	value := *measurement.Value
	return &value, measurement.SampleCount
}

// preemptionRate divides window preemptions by completed requests, with at
// least one request in the denominator so preemptions with no completion
// still count as pressure.
func preemptionRate(engine engineSignals) (float64, bool) {
	preemptions := engine.value(metrics.MeasurementPreemptions)
	completed := engine.value(metrics.MeasurementRequestsCompleted)
	if preemptions == nil || completed == nil {
		return 0, false
	}
	return *preemptions / math.Max(*completed, 1), true
}

// runningPerReplica averages the latest running-request gauge over the
// replicas that reported it (one sample per replica).
func runningPerReplica(engine engineSignals) (float64, bool) {
	running, replicas := engine.withSamples(metrics.MeasurementRequestsRunning)
	if running == nil || replicas < 1 {
		return 0, false
	}
	return *running / float64(replicas), true
}

func sharedPrefix(snapshot domain.EndpointMonitoringSnapshot) *float64 {
	for _, measurement := range snapshot.Evidence.Measurements {
		if measurement.Name == signalSharedPrefix && measurement.Availability == "available" && measurement.Value != nil {
			value := *measurement.Value
			return &value
		}
	}
	return nil
}

func meanInputTokens(snapshot domain.EndpointMonitoringSnapshot) *float64 {
	if snapshot.Summary.InputTokenSamples <= 0 {
		return nil
	}
	value := float64(snapshot.Summary.InputTokens) / float64(snapshot.Summary.InputTokenSamples)
	return &value
}

func monitoringSignals(snapshot domain.EndpointMonitoringSnapshot, engine engineSignals) []Signal {
	preemption, preemptionOK := preemptionRate(engine)
	running, runningOK := runningPerReplica(engine)
	optional := func(value float64, ok bool) *float64 {
		if !ok {
			return nil
		}
		return &value
	}
	values := []struct {
		name, unit, interpretation string
		value                      *float64
	}{
		{"error_rate", "ratio", "Reliability failures must be resolved before performance comparisons.", snapshot.Summary.ErrorRate},
		{"p95_latency", "ms", "End-to-end request latency is the denominator for stage-level attribution.", snapshot.Summary.P95LatencyMS},
		{"p95_queue", "ms", "Material queue time points to scheduling, admission, or capacity before kernels.", snapshot.Summary.P95QueueMS},
		{"p95_ttft", "ms", "First-token pressure suggests prefill, batching, cache, or attention investigation.", snapshot.Summary.P95TTFTMS},
		{"p95_generation", "ms", "Generation pressure suggests decode, KV-cache, sampling, or quantization investigation.", snapshot.Summary.P95GenerationMS},
		{"mean_input_tokens", "tokens", "Long inputs make the batched-token budget and chunked prefill the first prefill levers.", meanInputTokens(snapshot)},
		{signalSharedPrefix, "ratio", "Requests that repeat a system prefix can reuse cached prefix tokens.", sharedPrefix(snapshot)},
		{metrics.MeasurementKVCacheUsage, "percent", "A nearly full KV cache with waiting requests limits admission by memory, not compute.", engine.value(metrics.MeasurementKVCacheUsage)},
		{signalPreemptionRate, "ratio", "Preemptions mean the engine evicted running sequences for lack of KV-cache space.", optional(preemption, preemptionOK)},
		{signalRunningPerWorker, "requests", "Few concurrent sequences per replica leave decode memory-bandwidth bound.", optional(running, runningOK)},
		{metrics.MeasurementQueueTimeP95, "ms", "Time requests wait inside the engine scheduler before running.", engine.value(metrics.MeasurementQueueTimeP95)},
		{metrics.MeasurementTTFTP95, "ms", "Engine-measured first-token time, independent of gateway and network overhead.", engine.value(metrics.MeasurementTTFTP95)},
		{metrics.MeasurementTPOTP95, "ms", "Engine-measured time per output token for completed requests.", engine.value(metrics.MeasurementTPOTP95)},
		{metrics.MeasurementPrefixCacheHitRate, "ratio", "Share of input tokens served from the engine prefix cache.", engine.value(metrics.MeasurementPrefixCacheHitRate)},
		{metrics.MeasurementSpecDecodeAccept, "ratio", "Share of speculative draft tokens the target model accepted.", engine.value(metrics.MeasurementSpecDecodeAccept)},
	}
	signals := make([]Signal, 0, len(values))
	for _, item := range values {
		availability := "available"
		if item.value == nil {
			availability = "unavailable"
		}
		signals = append(signals, Signal{Name: item.name, Availability: availability, Value: item.value, Unit: item.unit, Interpretation: item.interpretation})
	}
	sort.Slice(signals, func(i, j int) bool { return signals[i].Name < signals[j].Name })
	return signals
}

func admissionPressure(admission *domain.AdmissionMonitoring) bool {
	if admission == nil {
		return false
	}
	return admission.Waiting > 0 || admission.Rejected > 0 || admission.QueueTimeouts > 0 || strings.EqualFold(admission.CapacityState, "constrained")
}

func meaningfulShare(value, total *float64, share, floor float64) bool {
	return value != nil && total != nil && *value >= floor && *total > 0 && *value / *total >= share
}
