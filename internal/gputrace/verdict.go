package gputrace

import (
	"fmt"
	"strings"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

// Rule thresholds of gpu-trace-analysis-v1.
const (
	hostWaitThreshold       = 0.10 // share of GPU time spent idle waiting on the host
	shortKernelThreshold    = 0.50 // share of launches whose GPU work is shorter than the call
	exposedCommThreshold    = 0.10 // share of GPU time in communication not under compute
	memoryBoundThreshold    = 0.60 // share of shaped matrix-multiply time below the ridge
	attentionThreshold      = 0.35 // attention share of kernel time
	idleThreshold           = 0.20 // idle share of GPU time
	longBubbleThresholdUS   = 1000 // p95 idle gap in microseconds
	decodeDominantThreshold = 0.70 // decode share of kernel time
	busyThreshold           = 0.80 // busy share for "no single bottleneck"
)

// judge fires the rules in priority order. The first rule that names a
// bottleneck sets the verdict; every fired rule is a finding that cites the
// measured value and its threshold.
func judge(a gpuprofile.Analysis, hostWaitNS, capacity int64) ([]gpuprofile.Finding, gpuprofile.Verdict) {
	findings := []gpuprofile.Finding{}
	verdict := gpuprofile.Verdict{}
	fire := func(rule string, observed, threshold float64, comparison, unit, sentence, bottleneck, evidence string) {
		findings = append(findings, gpuprofile.Finding{Rule: rule, Observed: round(observed, 4), Comparison: comparison, Threshold: threshold, Unit: unit, Sentence: sentence})
		if bottleneck != "" && verdict.Bottleneck == "" {
			verdict = gpuprofile.Verdict{Bottleneck: bottleneck, Sentence: sentence, Evidence: evidence}
		}
	}
	window := seconds(a.Trace.WindowMS)
	idle := a.Utilization.IdleShare
	if capacity > 0 && a.Idle.HostWaitShare != nil {
		hostWait := float64(hostWaitNS) / float64(capacity)
		if hostWait >= hostWaitThreshold {
			evidence := fmt.Sprintf("idle %s of %s · %s of idle waiting on launches", percent(idle), window, percent(*a.Idle.HostWaitShare))
			if a.Launch.LaunchDelayP50US != nil {
				evidence += fmt.Sprintf(" · launch delay p50 %s", micros(*a.Launch.LaunchDelayP50US))
			}
			fire("gpu.idle_host_bound", hostWait, hostWaitThreshold, ">=", "ratio",
				fmt.Sprintf("The GPU sat idle %s of the capture, mostly waiting for the host to launch work.", percent(idle)), gpuprofile.BottleneckHostLaunch, evidence)
		}
	}
	if short := a.Launch.ShortKernelShare; short != nil && *short >= shortKernelThreshold {
		fire("gpu.launch_bound_kernels", *short, shortKernelThreshold, ">=", "ratio",
			fmt.Sprintf("%s of launches finish on the GPU faster than the host takes to launch them.", capitalize(percent(*short))), gpuprofile.BottleneckHostLaunch,
			fmt.Sprintf("%s of %d launches shorter than their launch call · %.0f launches/s", percent(*short), a.Launch.Launches, a.Launch.PerSecond))
	}
	if overlap := a.Overlap.OverlappedShare; overlap != nil && capacity > 0 {
		exposedMS := a.Overlap.CommunicationMS * (1 - *overlap)
		exposed := exposedMS / (float64(capacity) / 1e6)
		if exposed >= exposedCommThreshold {
			fire("gpu.communication_exposed", exposed, exposedCommThreshold, ">=", "ratio",
				fmt.Sprintf("Collective communication holds the GPU %s of the time without compute running under it.", percent(exposed)), gpuprofile.BottleneckCommunication,
				fmt.Sprintf("communication %.1f ms · %s of it under compute", a.Overlap.CommunicationMS, percent(*overlap)))
		}
	}
	if roof := a.Roofline; roof.Position != "unknown" && roof.MemoryBoundShare != nil && roof.Accelerator != nil && roof.RidgeFLOPPerByte != nil {
		evidence := fmt.Sprintf("%s of matrix-multiply time has shapes · ridge %.0f FLOP/B", percent(value(roof.CoveredShare)), *roof.RidgeFLOPPerByte)
		if share := *roof.MemoryBoundShare; share >= memoryBoundThreshold {
			fire("gpu.memory_bound_gemm", share, memoryBoundThreshold, ">=", "ratio",
				fmt.Sprintf("Matrix multiplies are memory-bound: %s of their time sits below the ridge point of the %s.", percent(share), *roof.Accelerator), gpuprofile.BottleneckMemoryBandwidth, evidence)
		} else if compute := 1 - share; compute >= memoryBoundThreshold {
			fire("gpu.compute_bound_gemm", compute, memoryBoundThreshold, ">=", "ratio",
				fmt.Sprintf("Matrix multiplies are compute-bound: %s of their time sits above the ridge point of the %s.", percent(compute), *roof.Accelerator), gpuprofile.BottleneckCompute, evidence)
		}
	}
	for _, category := range a.Categories {
		if category.Category == gpuprofile.CategoryAttention && category.Share >= attentionThreshold {
			fire("gpu.attention_heavy", category.Share, attentionThreshold, ">=", "ratio",
				fmt.Sprintf("Attention takes %s of kernel time.", percent(category.Share)), gpuprofile.BottleneckAttention,
				fmt.Sprintf("attention %.1f ms of kernel time · %d kernels", category.KernelMS, category.Count))
		}
	}
	if idle >= idleThreshold {
		sentence := fmt.Sprintf("The GPU sat idle %s of the capture.", percent(idle))
		if a.Idle.HostWaitShare == nil {
			sentence = fmt.Sprintf("The GPU sat idle %s of the capture, and the trace has no host launch calls to say why.", percent(idle))
		}
		fire("gpu.idle", idle, idleThreshold, ">=", "ratio", sentence, "", "")
	}
	if p95 := a.Idle.Bubbles.P95US; p95 != nil && *p95 >= longBubbleThresholdUS {
		fire("gpu.long_bubbles", *p95, longBubbleThresholdUS, ">=", "us",
			fmt.Sprintf("One in twenty idle gaps lasts %s or longer.", micros(*p95)), "", "")
	}
	if a.Phases.Source != "unknown" && a.Phases.Decode.Share >= decodeDominantThreshold {
		fire("gpu.decode_dominant", a.Phases.Decode.Share, decodeDominantThreshold, ">=", "ratio",
			fmt.Sprintf("Decode steps take %s of kernel time.", percent(a.Phases.Decode.Share)), "", "")
	}
	if verdict.Bottleneck != "" {
		return findings, verdict
	}
	top := "no kernel category"
	if len(a.Categories) > 0 {
		top = fmt.Sprintf("%s %s of kernel time", a.Categories[0].Category, percent(a.Categories[0].Share))
	}
	evidence := fmt.Sprintf("busy %s of %s · %s", percent(a.Utilization.BusyShare), window, top)
	switch {
	case a.Utilization.BusyShare >= busyThreshold:
		verdict = gpuprofile.Verdict{Bottleneck: gpuprofile.BottleneckBusy, Sentence: fmt.Sprintf("The GPU stayed busy %s of the capture, and no single bottleneck stands out.", percent(a.Utilization.BusyShare)), Evidence: evidence}
	case a.Idle.HostWaitShare == nil:
		verdict = gpuprofile.Verdict{Bottleneck: gpuprofile.BottleneckUnknown, Sentence: fmt.Sprintf("The GPU sat idle %s of the capture, and the trace has no host launch calls to say why.", percent(idle)), Evidence: evidence}
	default:
		verdict = gpuprofile.Verdict{Bottleneck: gpuprofile.BottleneckUnknown, Sentence: fmt.Sprintf("The GPU was busy %s of the capture, and no rule names a single cause.", percent(a.Utilization.BusyShare)), Evidence: evidence}
	}
	return findings, verdict
}

func micros(value float64) string {
	if value >= 1000 {
		return fmt.Sprintf("%.1f ms", value/1000)
	}
	return fmt.Sprintf("%.0f µs", value)
}

func capitalize(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

func value(pointer *float64) float64 {
	if pointer == nil {
		return 0
	}
	return *pointer
}
