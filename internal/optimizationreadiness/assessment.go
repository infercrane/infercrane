// Package optimizationreadiness turns content-free production monitoring into
// a conservative next profiling action. It diagnoses serving-stage symptoms;
// it never claims a GPU operator or kernel bottleneck without profiler data.
package optimizationreadiness

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
)

const (
	SchemaVersion    = "infercrane.optimization-readiness/v1"
	AlgorithmVersion = "monitoring-stage-triage-v1"
)

type Policy struct {
	MinimumRequests int     `json:"minimum_requests"`
	MaxErrorRate    float64 `json:"max_error_rate"`
	QueueShare      float64 `json:"queue_share"`
	TTFTShare       float64 `json:"ttft_share"`
	GenerationShare float64 `json:"generation_share"`
}

type Signal struct {
	Name           string   `json:"name"`
	Availability   string   `json:"availability"`
	Value          *float64 `json:"value,omitempty"`
	Unit           string   `json:"unit,omitempty"`
	Interpretation string   `json:"interpretation"`
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
	Confidence        string     `json:"confidence"`
	Signals           []Signal   `json:"signals"`
	NextAction        string     `json:"next_action"`
	CandidateFamilies []string   `json:"candidate_families"`
	RequiredEvidence  []string   `json:"required_evidence"`
	KernelGate        KernelGate `json:"kernel_gate"`
	ContentRecorded   bool       `json:"content_recorded"`
	ProviderMutation  bool       `json:"provider_mutation"`
}

func DefaultPolicy() Policy {
	return Policy{MinimumRequests: 20, MaxErrorRate: .02, QueueShare: .20, TTFTShare: .40, GenerationShare: .50}
}

func Assess(snapshot domain.EndpointMonitoringSnapshot, policy Policy) Assessment {
	policy = normalizePolicy(policy)
	result := Assessment{
		SchemaVersion: SchemaVersion, AlgorithmVersion: AlgorithmVersion, Endpoint: snapshot.Endpoint,
		State: "collecting", PrimaryBottleneck: "unknown", Confidence: "insufficient",
		NextAction:        "collect fresh request and token telemetry before starting a paid optimization campaign",
		CandidateFamilies: []string{}, RequiredEvidence: []string{"fresh request window", "representative workload distribution", "runtime and accelerator identity"},
		KernelGate:      KernelGate{Eligible: false, Reason: "request-level monitoring cannot establish a GPU operator hotspot; an exact target-GPU profile and Amdahl gate are required"},
		ContentRecorded: false, ProviderMutation: false,
	}
	result.Signals = monitoringSignals(snapshot)
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
	if admissionPressure(snapshot.Admission) || meaningfulShare(snapshot.Summary.P95QueueMS, latency, policy.QueueShare, 25) {
		result.PrimaryBottleneck = "queueing-and-admission"
		result.NextAction = "replay the workload with AIPerf and profile scheduler, batching, replica, and admission settings before kernel work"
		result.CandidateFamilies = []string{"continuous batching", "batch-token budget", "admission policy", "replica scaling"}
		result.RequiredEvidence = []string{"arrival distribution", "concurrency sweep", "queue p95", "SLO-qualified goodput", "sourced cost"}
		return result
	}
	if meaningfulShare(snapshot.Summary.P95TTFTMS, latency, policy.TTFTShare, 50) {
		result.PrimaryBottleneck = "prefill-and-first-token"
		result.NextAction = "capture prefill/decode timing and prefix-reuse evidence, then compare cache, chunked-prefill, runtime, and attention candidates"
		result.CandidateFamilies = []string{"prefix cache", "chunked prefill", "attention backend", "prefill scheduling"}
		result.RequiredEvidence = []string{"input-length distribution", "prefix reuse", "prefill GPU trace", "AIPerf TTFT lanes"}
		return result
	}
	if meaningfulShare(snapshot.Summary.P95GenerationMS, latency, policy.GenerationShare, 50) {
		result.PrimaryBottleneck = "decode-and-generation"
		result.NextAction = "capture decode throughput, KV-cache, and GPU operator profiles, then compare decoding, cache, quantization, and runtime candidates"
		result.CandidateFamilies = []string{"KV cache", "speculative decoding", "quantization", "decode attention", "sampling"}
		result.RequiredEvidence = []string{"output-length distribution", "decode tokens/second", "KV-cache pressure", "target-GPU operator profile"}
		return result
	}
	result.PrimaryBottleneck = "unattributed-serving-time"
	result.NextAction = "run a bounded AIPerf replay with Nsight Systems or runtime profiling to attribute CPU, transfer, prefill, decode, and communication time"
	result.CandidateFamilies = []string{"runtime", "scheduling", "cache", "precision", "hardware"}
	result.RequiredEvidence = []string{"AIPerf replay", "CPU/GPU timeline", "prefill/decode split", "cost per successful output"}
	return result
}

func normalizePolicy(policy Policy) Policy {
	defaults := DefaultPolicy()
	if policy.MinimumRequests <= 0 {
		policy.MinimumRequests = defaults.MinimumRequests
	}
	if policy.MaxErrorRate <= 0 || policy.MaxErrorRate > 1 || math.IsNaN(policy.MaxErrorRate) {
		policy.MaxErrorRate = defaults.MaxErrorRate
	}
	if policy.QueueShare <= 0 || policy.QueueShare >= 1 {
		policy.QueueShare = defaults.QueueShare
	}
	if policy.TTFTShare <= 0 || policy.TTFTShare >= 1 {
		policy.TTFTShare = defaults.TTFTShare
	}
	if policy.GenerationShare <= 0 || policy.GenerationShare >= 1 {
		policy.GenerationShare = defaults.GenerationShare
	}
	return policy
}

func monitoringSignals(snapshot domain.EndpointMonitoringSnapshot) []Signal {
	values := []struct {
		name, unit, interpretation string
		value                      *float64
	}{
		{"error_rate", "ratio", "Reliability failures must be resolved before performance comparisons.", snapshot.Summary.ErrorRate},
		{"p95_latency", "ms", "End-to-end request latency is the denominator for stage-level attribution.", snapshot.Summary.P95LatencyMS},
		{"p95_queue", "ms", "Material queue time points to scheduling, admission, or capacity before kernels.", snapshot.Summary.P95QueueMS},
		{"p95_ttft", "ms", "First-token pressure suggests prefill, batching, cache, or attention investigation.", snapshot.Summary.P95TTFTMS},
		{"p95_generation", "ms", "Generation pressure suggests decode, KV-cache, sampling, or quantization investigation.", snapshot.Summary.P95GenerationMS},
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
