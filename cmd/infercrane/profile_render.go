package main

import (
	"fmt"
	"strings"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

// renderProfile writes the verdict first, then the numbers it rests on.
// Every unknown value says why instead of printing a zero.
func renderProfile(result ProfileResult) string {
	var out strings.Builder
	header := []string{"GPU profile", result.Endpoint, result.Capture.Tool + " " + result.Capture.ToolVersion}
	if result.Capture.Accelerator != nil {
		accelerator := *result.Capture.Accelerator
		if result.Capture.GPUs != nil && *result.Capture.GPUs > 1 {
			accelerator = fmt.Sprintf("%d × %s", *result.Capture.GPUs, accelerator)
		}
		header = append(header, accelerator)
	}
	fmt.Fprintln(&out, strings.Join(header, " · "))
	analysis := result.Analysis
	if analysis == nil {
		fmt.Fprintf(&out, "\n%s\n", result.Reason)
		writeFooter(&out, result)
		return out.String()
	}
	fmt.Fprintf(&out, "\n%s\n%s\n\n", analysis.Verdict.Sentence, analysis.Verdict.Evidence)
	row := func(label, value string) { fmt.Fprintf(&out, "  %-20s %s\n", label, value) }
	utilization := analysis.Utilization
	row("Window", fmt.Sprintf("%s between the first and last GPU activity · %d kernels on %d GPU(s), %d stream(s)", duration(analysis.Trace.WindowMS), analysis.Trace.Kernels, analysis.Trace.Devices, analysis.Trace.Streams))
	row("GPU busy", fmt.Sprintf("%s (compute %s · memory and communication only %s)", pct(utilization.BusyShare), pct(utilization.ComputeShare), pct(utilization.NonComputeShare)))
	idle := analysis.Idle
	if idle.HostWaitShare != nil {
		row("GPU idle", fmt.Sprintf("%s · waiting on the host %s · launch latency %s · other %s", pct(utilization.IdleShare), pct(*idle.HostWaitShare), pct(*idle.LaunchGapShare), pct(*idle.OtherShare)))
	} else {
		row("GPU idle", fmt.Sprintf("%s · cause unknown: %s", pct(utilization.IdleShare), idle.Reason))
	}
	if idle.Bubbles.P95US != nil {
		row("Idle gaps", fmt.Sprintf("%d · p50 %s · p95 %s · longest %s", idle.Bubbles.Count, micro(*idle.Bubbles.P50US), micro(*idle.Bubbles.P95US), micro(*idle.Bubbles.MaxUS)))
	}
	launch := analysis.Launch
	if launch.ShortKernelShare != nil {
		row("Launches", fmt.Sprintf("%d (%d graph) · %.0f/s · %s shorter on the GPU than their launch call", launch.Launches, launch.GraphLaunches, launch.PerSecond, pct(*launch.ShortKernelShare)))
		row("Launch delay", fmt.Sprintf("p50 %s · p95 %s · busiest host thread in launch calls %s of the time", micro(*launch.LaunchDelayP50US), micro(*launch.LaunchDelayP95US), pct(*launch.HostLaunchShare)))
	} else {
		row("Launches", "unknown: "+launch.Reason)
	}
	if overlap := analysis.Overlap; overlap.OverlappedShare != nil {
		row("Communication", fmt.Sprintf("%s · %s of it under compute", duration(overlap.CommunicationMS), pct(*overlap.OverlappedShare)))
	} else {
		row("Communication", "not applicable: "+overlap.Reason)
	}
	if steps := analysis.Steps; steps.P50MS != nil {
		source := "from GPU idle gaps"
		if steps.Marker != nil {
			source = "from " + *steps.Marker + " annotations"
		}
		row("Steps", fmt.Sprintf("%d · p50 %s · p95 %s (%s)", steps.Count, duration(*steps.P50MS), duration(*steps.P95MS), source))
	} else {
		row("Steps", "unknown: "+steps.Reason)
	}
	if phases := analysis.Phases; phases.Source != "unknown" {
		source := map[string]string{"annotations": "step annotations", "kernel_names": "attention kernel names"}[phases.Source]
		row("Prefill / decode", fmt.Sprintf("%s / %s of kernel time · mixed %s · unknown %s (from %s)", pct(phases.Prefill.Share), pct(phases.Decode.Share), pct(phases.Mixed.Share), pct(phases.Unknown.Share), source))
	} else {
		row("Prefill / decode", "unknown: no step names a phase")
	}
	if roof := analysis.Roofline; roof.Position != "unknown" {
		row("Roofline", fmt.Sprintf("%s · %s of shaped matrix-multiply time below the ridge (%.0f FLOP/B) · %s of matrix-multiply time has shapes", strings.ReplaceAll(roof.Position, "_", "-"), pct(*roof.MemoryBoundShare), *roof.RidgeFLOPPerByte, pct(*roof.CoveredShare)))
	} else {
		row("Roofline", "unknown: "+roof.Reason)
	}
	if hardware := analysis.Hardware; hardware != nil {
		for _, series := range hardware.Series {
			row(series.Field, seriesSummary(series))
		}
	}
	if len(analysis.TopKernels) > 0 {
		fmt.Fprintf(&out, "\nTop kernels\n")
		for index, kernel := range analysis.TopKernels {
			if index == 8 {
				break
			}
			fmt.Fprintf(&out, "  %5s  %-13s %7d × %-9s %s\n", pct(kernel.Share), kernel.Category, kernel.Count, micro(kernel.MeanUS), truncate(kernel.Name, 72))
		}
	}
	if len(analysis.Findings) > 0 {
		fmt.Fprintf(&out, "\nFindings\n")
		for _, finding := range analysis.Findings {
			fmt.Fprintf(&out, "  %-28s %s\n", finding.Rule, finding.Sentence)
		}
	}
	if analysis.Trace.Truncated {
		fmt.Fprintf(&out, "\nThe trace held more events than one analysis reads; these numbers cover the part that was read.\n")
	}
	writeFooter(&out, result)
	return out.String()
}

func writeFooter(out *strings.Builder, result ProfileResult) {
	fmt.Fprintln(out)
	switch {
	case result.Uploaded && result.Source != "stored":
		fmt.Fprintf(out, "Recorded %s against %s revision %d · prompt and output content not recorded\n", *result.CaptureID, *result.Deployment, *result.Revision)
	case result.Source == "stored":
		fmt.Fprintf(out, "Capture %s of %s revision %d\n", *result.CaptureID, *result.Deployment, *result.Revision)
	default:
		fmt.Fprintln(out, "Analyzed locally; not recorded (--no-upload).")
	}
	if len(result.TraceFiles) > 0 {
		fmt.Fprintf(out, "Trace: %s (open it in https://ui.perfetto.dev)\n", strings.Join(result.TraceFiles, ", "))
	}
	if result.DashboardURL != nil {
		printDashboardLink(out, *result.DashboardURL)
	}
}

func renderComparison(endpoint string, comparison gpuprofile.Comparison) string {
	var out strings.Builder
	fmt.Fprintf(&out, "GPU profile comparison · %s\n", endpoint)
	fmt.Fprintf(&out, "  before  %s · revision %d · %s\n", comparison.Base.ID, comparison.Base.Revision, comparison.Base.EndedAt.Format("2006-01-02 15:04"))
	fmt.Fprintf(&out, "  after   %s · revision %d · %s\n\n", comparison.Head.ID, comparison.Head.Revision, comparison.Head.EndedAt.Format("2006-01-02 15:04"))
	for _, warning := range comparison.Warnings {
		fmt.Fprintf(&out, "  ! %s\n", warning)
	}
	if len(comparison.Warnings) > 0 {
		fmt.Fprintln(&out)
	}
	for _, metric := range comparison.Metrics {
		before, after := metricValue(metric.Base, metric.Unit), metricValue(metric.Head, metric.Unit)
		verdict := ""
		switch {
		case metric.Delta == nil:
			verdict = "unknown on one side"
		case metric.Improved == nil:
			verdict = "no clear change"
		case *metric.Improved:
			verdict = "better"
		default:
			verdict = "worse"
		}
		fmt.Fprintf(&out, "  %-44s %10s → %-10s %s\n", metric.Label, before, after, verdict)
	}
	if len(comparison.Categories) > 0 {
		fmt.Fprintf(&out, "\nKernel time per second of capture\n")
		for _, category := range comparison.Categories {
			fmt.Fprintf(&out, "  %-14s %8.1f ms → %-8.1f ms\n", category.Category, category.BaseMSPerS, category.HeadMSPerS)
		}
	}
	return out.String()
}

func metricValue(value *float64, unit string) string {
	if value == nil {
		return "unknown"
	}
	switch unit {
	case "ratio":
		return pct(*value)
	case "us":
		return micro(*value)
	case "ms":
		return duration(*value)
	}
	return fmt.Sprintf("%.2f", *value)
}

func seriesSummary(series gpuprofile.FieldSeries) string {
	var sum, peak float64
	count := 0
	for _, value := range series.Values {
		if value != nil {
			sum += *value
			peak = max(peak, *value)
			count++
		}
	}
	if count == 0 {
		return "not sent"
	}
	if series.Unit == "percent" {
		return fmt.Sprintf("mean %.0f%% · peak %.0f%% (%d of %d samples)", sum/float64(count), peak, count, len(series.Values))
	}
	return fmt.Sprintf("mean %.2f GB/s · peak %.2f GB/s (%d of %d samples)", sum/float64(count)/1e9, peak/1e9, count, len(series.Values))
}

func pct(share float64) string {
	value := share * 100
	if value > 0 && value < 10 {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0") + "%"
	}
	return fmt.Sprintf("%.0f%%", value)
}

func micro(value float64) string {
	if value >= 1000 {
		return duration(value / 1000)
	}
	return fmt.Sprintf("%.0f µs", value)
}

func duration(milliseconds float64) string {
	if milliseconds >= 1000 {
		return fmt.Sprintf("%.1f s", milliseconds/1000)
	}
	if milliseconds >= 10 {
		return fmt.Sprintf("%.0f ms", milliseconds)
	}
	return fmt.Sprintf("%.2f ms", milliseconds)
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}
