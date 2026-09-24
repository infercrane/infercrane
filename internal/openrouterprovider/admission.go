package openrouterprovider

import (
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

// AdmissionConfig controls the provider edge's fail-fast concurrency boundary.
// A zero TargetTTFT disables adaptation and keeps InitialLimit fixed.
type AdmissionConfig struct {
	MinLimit                    int
	InitialLimit                int
	MaxLimit                    int
	Window                      int
	TargetTTFT                  time.Duration
	QualifiedOutputTokensPerSec float64
	InputPricePerMillionUSD     float64
	OutputPricePerMillionUSD    float64
	GPUHourlyCostUSD            float64
	MaxPrefillTokensInFlight    int64
}

type admissionDecision string

const (
	admissionAccepted         admissionDecision = "accepted"
	admissionCapacityRejected admissionDecision = "capacity_rejected"
	admissionPrefillRejected  admissionDecision = "prefill_capacity_rejected"
)

type admissionObservation struct {
	StatusCode       int
	Outcome          string
	TTFT             time.Duration
	Duration         time.Duration
	PromptTokens     int64
	CompletionTokens int64
}

type admissionSnapshot struct {
	startedAt               time.Time
	inFlight                int
	limit                   int
	accepted                uint64
	rejected                uint64
	completed               uint64
	failed                  uint64
	canceled                uint64
	sloMissed               uint64
	promptTokens            uint64
	completionTokens        uint64
	productiveOutputTokens  uint64
	requestSeconds          float64
	ttftSeconds             float64
	ttftSamples             uint64
	revenueUSD              float64
	qualifiedOutputTokensPS float64
	gpuHourlyCostUSD        float64
	prefillTokensInFlight   int64
	prefillRejected         uint64
	maxPrefillTokens        int64
}

// admissionController uses additive increase and multiplicative decrease. It
// never queues at the public edge: requests above the current safe limit get a
// prompt 429 so an upstream router can select another provider.
type admissionController struct {
	mu sync.Mutex

	config    AdmissionConfig
	startedAt time.Time
	inFlight  int
	limit     int

	accepted               uint64
	rejected               uint64
	completed              uint64
	failed                 uint64
	canceled               uint64
	sloMissed              uint64
	promptTokens           uint64
	completionTokens       uint64
	productiveOutputTokens uint64
	requestSeconds         float64
	ttftSeconds            float64
	ttftSamples            uint64
	revenueUSD             float64
	prefillTokensInFlight  int64
	prefillRejected        uint64

	windowCompleted   int
	windowFailures    int
	windowTTFTSamples int
	windowSLOMisses   int
	windowSaturated   bool
}

func newAdmissionController(config AdmissionConfig) (*admissionController, error) {
	if config.MaxLimit < 1 {
		return nil, fmt.Errorf("maximum admission limit must be positive")
	}
	if config.MinLimit == 0 {
		config.MinLimit = config.MaxLimit
	}
	if config.InitialLimit == 0 {
		config.InitialLimit = config.MaxLimit
	}
	if config.Window == 0 {
		config.Window = 32
	}
	if config.MinLimit < 1 || config.MinLimit > config.InitialLimit || config.InitialLimit > config.MaxLimit {
		return nil, fmt.Errorf("admission limits must satisfy 1 <= min <= initial <= max")
	}
	if config.Window < 4 {
		return nil, fmt.Errorf("admission window must contain at least four completions")
	}
	if config.TargetTTFT < 0 || config.QualifiedOutputTokensPerSec < 0 || config.InputPricePerMillionUSD < 0 || config.OutputPricePerMillionUSD < 0 || config.GPUHourlyCostUSD < 0 || config.MaxPrefillTokensInFlight < 0 {
		return nil, fmt.Errorf("admission targets and economic inputs cannot be negative")
	}
	return &admissionController{config: config, startedAt: time.Now(), limit: config.InitialLimit}, nil
}

func (a *admissionController) tryAcquire(estimatedPrefillTokens int64) admissionDecision {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inFlight >= a.limit {
		a.rejected++
		a.windowSaturated = true
		return admissionCapacityRejected
	}
	if a.config.MaxPrefillTokensInFlight > 0 && estimatedPrefillTokens > 0 && a.prefillTokensInFlight+estimatedPrefillTokens > a.config.MaxPrefillTokensInFlight {
		a.rejected++
		a.prefillRejected++
		a.windowSaturated = true
		return admissionPrefillRejected
	}
	a.inFlight++
	a.prefillTokensInFlight += estimatedPrefillTokens
	a.accepted++
	if a.inFlight >= a.limit {
		a.windowSaturated = true
	}
	return admissionAccepted
}

func (a *admissionController) releasePrefill(estimatedPrefillTokens int64) {
	if estimatedPrefillTokens <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prefillTokensInFlight -= estimatedPrefillTokens
	if a.prefillTokensInFlight < 0 {
		a.prefillTokensInFlight = 0
	}
}

func (a *admissionController) complete(observation admissionObservation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inFlight > 0 {
		a.inFlight--
	}
	a.completed++
	a.requestSeconds += observation.Duration.Seconds()

	canceled := observation.Outcome == "client_canceled"
	succeeded := observation.StatusCode >= 200 && observation.StatusCode < 300 && observation.Outcome == "completed"
	if canceled {
		a.canceled++
	} else if !succeeded {
		a.failed++
		a.windowFailures++
	}
	if observation.PromptTokens > 0 {
		a.promptTokens += uint64(observation.PromptTokens)
	}
	if observation.CompletionTokens > 0 {
		a.completionTokens += uint64(observation.CompletionTokens)
	}
	a.revenueUSD += float64(observation.PromptTokens) * a.config.InputPricePerMillionUSD / 1_000_000
	a.revenueUSD += float64(observation.CompletionTokens) * a.config.OutputPricePerMillionUSD / 1_000_000
	// A caller cancellation is neither evidence of an unhealthy model server
	// nor a successful SLO sample. Keep it visible, but exclude it from the
	// adaptive window so user behavior cannot lower fleet capacity.
	if canceled {
		return
	}
	a.windowCompleted++

	qualified := false
	if observation.TTFT > 0 {
		a.ttftSeconds += observation.TTFT.Seconds()
		a.ttftSamples++
		a.windowTTFTSamples++
		qualified = succeeded
		if a.config.TargetTTFT > 0 {
			qualified = qualified && observation.TTFT <= a.config.TargetTTFT
			if succeeded && !qualified {
				a.sloMissed++
				a.windowSLOMisses++
			}
		}
	}
	if a.config.TargetTTFT == 0 {
		qualified = succeeded
	}
	if qualified && observation.CompletionTokens > 0 {
		a.productiveOutputTokens += uint64(observation.CompletionTokens)
	}

	if a.config.TargetTTFT == 0 || a.config.MinLimit == a.config.MaxLimit || a.windowCompleted < a.config.Window {
		return
	}
	bad := a.windowFailures > 0
	if a.windowTTFTSamples > 0 {
		bad = bad || float64(a.windowSLOMisses)/float64(a.windowTTFTSamples) > 0.05
	}
	if bad {
		reduced := int(math.Floor(float64(a.limit) * 0.8))
		if reduced < a.config.MinLimit {
			reduced = a.config.MinLimit
		}
		a.limit = reduced
	} else if a.windowSaturated && a.limit < a.config.MaxLimit {
		a.limit++
	}
	a.windowCompleted = 0
	a.windowFailures = 0
	a.windowTTFTSamples = 0
	a.windowSLOMisses = 0
	a.windowSaturated = false
}

func (a *admissionController) snapshot() admissionSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return admissionSnapshot{
		startedAt: a.startedAt, inFlight: a.inFlight, limit: a.limit,
		accepted: a.accepted, rejected: a.rejected, completed: a.completed,
		failed: a.failed, canceled: a.canceled, sloMissed: a.sloMissed,
		promptTokens: a.promptTokens, completionTokens: a.completionTokens,
		productiveOutputTokens: a.productiveOutputTokens,
		requestSeconds:         a.requestSeconds, ttftSeconds: a.ttftSeconds,
		ttftSamples: a.ttftSamples, revenueUSD: a.revenueUSD,
		qualifiedOutputTokensPS: a.config.QualifiedOutputTokensPerSec,
		gpuHourlyCostUSD:        a.config.GPUHourlyCostUSD,
		prefillTokensInFlight:   a.prefillTokensInFlight,
		prefillRejected:         a.prefillRejected,
		maxPrefillTokens:        a.config.MaxPrefillTokensInFlight,
	}
}

func (a *admissionController) writePrometheus(writer io.Writer, now time.Time) {
	s := a.snapshot()
	uptime := now.Sub(s.startedAt).Seconds()
	if uptime < 0 {
		uptime = 0
	}
	productiveUtilization := 0.0
	if s.qualifiedOutputTokensPS > 0 && uptime > 0 {
		productiveUtilization = float64(s.productiveOutputTokens) / (s.qualifiedOutputTokensPS * uptime)
	}
	gpuCost := uptime / 3600 * s.gpuHourlyCostUSD
	grossMargin := 0.0
	if s.revenueUSD > 0 {
		grossMargin = (s.revenueUSD - gpuCost) / s.revenueUSD
	}
	metrics := []struct {
		name  string
		help  string
		kind  string
		value any
	}{
		{"infercrane_provider_admission_limit", "Current fail-fast request concurrency limit.", "gauge", s.limit},
		{"infercrane_provider_in_flight", "Requests currently executing at the model server.", "gauge", s.inFlight},
		{"infercrane_provider_prefill_tokens_in_flight", "Estimated prompt tokens admitted but not yet producing output.", "gauge", s.prefillTokensInFlight},
		{"infercrane_provider_prefill_token_limit", "Maximum estimated prompt tokens allowed in prefill concurrently; zero disables the boundary.", "gauge", s.maxPrefillTokens},
		{"infercrane_provider_requests_accepted_total", "Requests accepted by the provider edge.", "counter", s.accepted},
		{"infercrane_provider_requests_rejected_total", "Requests rejected before the model server was overloaded.", "counter", s.rejected},
		{"infercrane_provider_prefill_rejected_total", "Requests rejected by the long-prefill token boundary.", "counter", s.prefillRejected},
		{"infercrane_provider_requests_completed_total", "Accepted requests that reached a terminal outcome.", "counter", s.completed},
		{"infercrane_provider_requests_failed_total", "Accepted requests with a non-success terminal outcome.", "counter", s.failed},
		{"infercrane_provider_requests_canceled_total", "Accepted requests canceled by the caller before a terminal model response.", "counter", s.canceled},
		{"infercrane_provider_ttft_slo_missed_total", "Successful requests that missed the configured TTFT objective.", "counter", s.sloMissed},
		{"infercrane_provider_prompt_tokens_total", "Billable prompt tokens reported by the model server.", "counter", s.promptTokens},
		{"infercrane_provider_completion_tokens_total", "Billable completion tokens reported by the model server.", "counter", s.completionTokens},
		{"infercrane_provider_productive_output_tokens_total", "Successful completion tokens delivered within the configured TTFT objective.", "counter", s.productiveOutputTokens},
		{"infercrane_provider_qualified_output_tokens_per_second", "Exact-host SLO-qualified output capacity used as the productive-utilization denominator.", "gauge", s.qualifiedOutputTokensPS},
		{"infercrane_provider_request_seconds_total", "Aggregate accepted request wall time.", "counter", s.requestSeconds},
		{"infercrane_provider_ttft_seconds_total", "Aggregate measured streaming time to first token.", "counter", s.ttftSeconds},
		{"infercrane_provider_ttft_samples_total", "Streaming responses with a measured first token.", "counter", s.ttftSamples},
		{"infercrane_provider_productive_utilization_ratio", "SLO-qualified output tokens divided by qualified output capacity since process start.", "gauge", productiveUtilization},
		{"infercrane_provider_revenue_usd_total", "Revenue implied by reported token usage and configured public prices.", "counter", s.revenueUSD},
		{"infercrane_provider_gpu_cost_usd", "GPU cost implied by process uptime and configured hourly price.", "gauge", gpuCost},
		{"infercrane_provider_gpu_hourly_cost_usd", "Configured all-in GPU cost per hour.", "gauge", s.gpuHourlyCostUSD},
		{"infercrane_provider_gross_margin_ratio", "Implied token revenue minus GPU cost, divided by token revenue.", "gauge", grossMargin},
	}
	for _, metric := range metrics {
		fmt.Fprintf(writer, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", metric.name, metric.help, metric.name, metric.kind, metric.name, metric.value)
	}
}
