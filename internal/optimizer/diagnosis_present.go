package optimizer

import (
	"fmt"
	"math"

	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
)

// The planner plans for the load the server holds when it is diagnosed. Two
// readings of the assessment are totals over its whole window (an hour): the
// engine's queue-time histogram and its evictions per completed request. A
// burst at the start of the window can leave both high while nothing waits
// and the cache has room now. The planner then reads the bottleneck again
// from the readings of the present load, and says why in the plan.
const (
	// presentQueueShare and presentQueueFloorMS are the readiness policy's
	// queue thresholds: a queue reading counts when it is at least this share
	// of p95 request time and this long.
	presentQueueShare   = 0.20
	presentQueueFloorMS = 25.0
	// presentTTFTShare and presentGenerationShare are the readiness policy's
	// stage shares for first tokens and for writing answers.
	presentTTFTShare       = 0.40
	presentGenerationShare = 0.50
	// presentFullCache is the cache use, in percent, at or above which the
	// cache counts as full now (the readiness policy's saturation limit).
	presentFullCache = 90.0
)

// waitingNow is the number of requests waiting per server when the
// diagnosis was read, from the engine's waiting gauge.
func waitingNow(d Diagnosis) (float64, bool) {
	if value, ok := d.signal(optimizationreadiness.SignalWaitingPerReplica); ok {
		return value, true
	}
	return d.signal(metrics.MeasurementRequestsWaiting)
}

// staleQueue reports an engine queue time that is long against p95 request
// time while the engine's gauge shows no request waiting, and no queue at the
// gateway. It returns the queue time.
func staleQueue(d Diagnosis) (float64, bool) {
	waiting, known := waitingNow(d)
	queue, queueKnown := d.signal(metrics.MeasurementQueueTimeP95)
	latency, latencyKnown := d.signal("p95_latency")
	if !known || waiting > 0 || !queueKnown || !latencyKnown || latency <= 0 || queue < presentQueueFloorMS || queue/latency < presentQueueShare {
		return 0, false
	}
	if gateway, ok := d.signal("p95_queue"); ok && gateway >= presentQueueFloorMS && gateway/latency >= presentQueueShare {
		return 0, false
	}
	return queue, true
}

// pastEvictions reports evictions over the window while the cache has room
// now and no request is known to wait: a burst that is over. It needs the
// cache use; without it nothing is known about now.
func pastEvictions(d Diagnosis) (DiagnosisFinding, float64, bool) {
	finding, evicted := d.finding(optimizationreadiness.RuleKVPreemption)
	usage, usageKnown := d.signal(metrics.MeasurementKVCacheUsage)
	if !evicted || !usageKnown || usage >= presentFullCache {
		return DiagnosisFinding{}, 0, false
	}
	if _, saturated := d.finding(optimizationreadiness.RuleKVSaturation); saturated {
		return DiagnosisFinding{}, 0, false
	}
	if waiting, known := waitingNow(d); known && waiting > 0 {
		return DiagnosisFinding{}, 0, false
	}
	return finding, usage, true
}

// presentStage is the stage that holds the larger share of p95 request time:
// writing answers or first tokens. First-token times over the window hold the
// same past wait that made the queue reading long, so neither share alone
// decides.
func presentStage(d Diagnosis) string {
	latency, ok := d.signal("p95_latency")
	if !ok || latency <= 0 {
		return optimizationreadiness.BottleneckUnattributed
	}
	generation, generationKnown := d.signal("p95_generation")
	ttft, ttftKnown := d.signal("p95_ttft")
	generationShare, ttftShare := 0.0, 0.0
	if generationKnown {
		generationShare = generation / latency
	}
	if ttftKnown {
		ttftShare = ttft / latency
	}
	switch {
	case generationShare >= presentGenerationShare && generationShare >= ttftShare:
		return optimizationreadiness.BottleneckDecode
	case ttftShare >= presentTTFTShare:
		return optimizationreadiness.BottleneckPrefill
	case generationShare >= presentGenerationShare:
		return optimizationreadiness.BottleneckDecode
	default:
		return optimizationreadiness.BottleneckUnattributed
	}
}

// presentDiagnosis returns the diagnosis the planner plans from: the recorded
// one, or, when its bottleneck rests on waiting the present load does not
// show, the same readings with that bottleneck read again. The note says what
// was read again and why; it is empty when nothing was.
func presentDiagnosis(d Diagnosis) (Diagnosis, string) {
	queue, queueStale := staleQueue(d)
	preemption, usage, burst := pastEvictions(d)
	reread := false
	switch d.PrimaryBottleneck {
	case optimizationreadiness.BottleneckQueueing, optimizationreadiness.BottleneckPrefill:
		reread = queueStale
	case optimizationreadiness.BottleneckKVCachePressure:
		reread = burst
	}
	if !reread {
		return d, ""
	}
	present := d
	present.PrimaryBottleneck = presentStage(d)
	present.Findings = []DiagnosisFinding{}
	for _, finding := range d.Findings {
		if queueStale && finding.Rule == optimizationreadiness.RuleEngineQueue || burst && finding.Rule == optimizationreadiness.RuleKVPreemption {
			continue
		}
		present.Findings = append(present.Findings, finding)
	}
	if burst {
		present.Findings = append(present.Findings, DiagnosisFinding{Rule: RulePastEvictions, Signal: preemption.Signal, Observed: preemption.Observed, Comparison: preemption.Comparison, Threshold: preemption.Threshold, Unit: preemption.Unit})
	}
	words := bottleneckWords[present.PrimaryBottleneck]
	switch {
	case queueStale && burst:
		return present, fmt.Sprintf("The engine's queue time (%s at p95) and its evictions (%s per completed request) are totals over the hour, but no request was waiting and the cache was %s full when the diagnosis was read, so both came from an earlier burst; the plan reads the present load as %s.",
			millisecondsWords(queue), ratioWords(preemption.Observed), percentWords(usage), words)
	case queueStale:
		return present, fmt.Sprintf("The engine's queue time (%s at p95) is a total over the hour, but no request was waiting when the diagnosis was read, so it came from an earlier burst or from reading prompts, not from a queue now; the plan reads the present load as %s.",
			millisecondsWords(queue), words)
	default:
		return present, fmt.Sprintf("The engine evicted requests (%s per completed request over the hour), but the cache was %s full with no request waiting when the diagnosis was read, so the evictions came from a burst that is over; the plan reads the present load as %s.",
			ratioWords(preemption.Observed), percentWords(usage), words)
	}
}

// RulePastEvictions fires on evictions over the diagnosis window while the
// cache has room now: a burst that is over. Its answer is fewer requests at
// once per server, so the next burst waits instead of evicting, or a second
// replica for the burst; never a cache format.
const RulePastEvictions = "kv_cache.past_burst"

// pastBurstPriority ranks fewer requests at once for a burst that is over
// behind any change the present load chose.
const pastBurstPriority = 40

func pastBurstOutcome(d Diagnosis) *ruleOutcome {
	finding, ok := d.finding(RulePastEvictions)
	if !ok {
		return nil
	}
	why := fmt.Sprintf("The engine evicted requests in a burst earlier in the window (%s per completed request); running fewer requests at once per server makes the next burst wait instead of evicting, and a second replica would absorb it.", ratioWords(finding.Observed))
	return &ruleOutcome{rule: RulePastEvictions, evidence: findingEvidence(finding), picks: []techniquePick{{TechniqueKVSequenceCap, pastBurstPriority, why}}}
}

// cacheBlockLimit is the most requests a server can run at once by its own
// cache: each running request holds at least one cache block. It is a
// multiple of eight, and known only when the engine reported its blocks.
func cacheBlockLimit(d Diagnosis) (int, bool) {
	blocks, ok := d.signal(optimizationreadiness.SignalCacheBlocksPerReplica)
	if !ok || blocks < sequenceCapStep {
		return 0, false
	}
	return int(math.Floor(blocks/sequenceCapStep)) * sequenceCapStep, true
}
