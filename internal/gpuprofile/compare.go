package gpuprofile

import (
	"errors"
	"math"
	"sort"
	"time"
)

// ErrNotComparable reports that a capture has no analysis to compare.
var ErrNotComparable = errors.New("gpu profile has no analysis to compare")

// MaxCompareKernels bounds the kernel rows of one comparison.
const MaxCompareKernels = 15

// Summary is one retained capture in an endpoint's capture list.
type Summary struct {
	ID            string    `json:"id"`
	Deployment    string    `json:"deployment"`
	Revision      int       `json:"revision"`
	Active        bool      `json:"active"`
	SchemaVersion string    `json:"schema_version"`
	Tool          string    `json:"tool"`
	ToolVersion   string    `json:"tool_version"`
	Accelerator   *string   `json:"accelerator"`
	GPUs          *int      `json:"gpus"`
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at"`
	RecordedAt    time.Time `json:"recorded_at"`
	Verdict       *Verdict  `json:"verdict"`
	BusyShare     *float64  `json:"busy_share"`
}

// SummaryOf lists a profile without its samples.
func SummaryOf(profile Profile, active bool) Summary {
	summary := Summary{
		ID: profile.Capture.ID, Deployment: profile.Deployment, Revision: profile.Revision, Active: active,
		SchemaVersion: profile.SchemaVersion, Tool: profile.Capture.Tool, ToolVersion: profile.Capture.ToolVersion,
		Accelerator: profile.Capture.Accelerator, GPUs: profile.Capture.GPUs,
		StartedAt: profile.Capture.StartedAt, EndedAt: profile.Capture.EndedAt, RecordedAt: profile.RecordedAt,
	}
	if analysis := profile.Analysis; analysis != nil {
		verdict := analysis.Verdict
		busy := analysis.Utilization.BusyShare
		summary.Verdict, summary.BusyShare = &verdict, &busy
	}
	return summary
}

// Comparison is a before-and-after reading of two analyzed captures.
type Comparison struct {
	Base       Summary              `json:"base"`
	Head       Summary              `json:"head"`
	Warnings   []string             `json:"warnings"`
	Metrics    []MetricDelta        `json:"metrics"`
	Categories []CategoryComparison `json:"categories"`
	Kernels    []KernelComparison   `json:"kernels"`
}

// MetricDelta compares one headline value. A side that did not measure it is
// nil, and then so are the delta and the verdict.
type MetricDelta struct {
	Metric   string   `json:"metric"`
	Label    string   `json:"label"`
	Unit     string   `json:"unit"`
	Base     *float64 `json:"base"`
	Head     *float64 `json:"head"`
	Delta    *float64 `json:"delta"`
	Better   string   `json:"better"`
	Improved *bool    `json:"improved"`
}

// CategoryComparison compares kernel time per second of capture, so captures
// of different lengths compare.
type CategoryComparison struct {
	Category   string  `json:"category"`
	BaseMSPerS float64 `json:"base_ms_per_s"`
	HeadMSPerS float64 `json:"head_ms_per_s"`
	BaseShare  float64 `json:"base_share"`
	HeadShare  float64 `json:"head_share"`
}

// KernelComparison compares one top kernel's time per second of capture. A
// kernel that is not among one side's top kernels has nil there.
type KernelComparison struct {
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	BaseMSPerS *float64 `json:"base_ms_per_s"`
	HeadMSPerS *float64 `json:"head_ms_per_s"`
}

type metricSpec struct {
	key, label, unit, better string
	read                     func(Analysis) *float64
}

// A change smaller than this is reported without a better or worse verdict.
// It is a fixed reporting threshold, not a measured capture-to-capture noise.
const compareNoise = 0.02

var compareMetrics = []metricSpec{
	{"busy_share", "GPU busy", "ratio", "higher", func(a Analysis) *float64 { return ptr(a.Utilization.BusyShare) }},
	{"idle_share", "GPU idle", "ratio", "lower", func(a Analysis) *float64 { return ptr(a.Utilization.IdleShare) }},
	{"host_wait_share", "Idle waiting on the host", "ratio", "lower", func(a Analysis) *float64 { return a.Idle.HostWaitShare }},
	{"short_kernel_share", "Kernels shorter than their launch", "ratio", "lower", func(a Analysis) *float64 { return a.Launch.ShortKernelShare }},
	{"launch_delay_p50_us", "Launch delay p50", "us", "lower", func(a Analysis) *float64 { return a.Launch.LaunchDelayP50US }},
	{"overlapped_share", "Communication under compute", "ratio", "higher", func(a Analysis) *float64 { return a.Overlap.OverlappedShare }},
	{"step_p50_ms", "Step time p50", "ms", "lower", func(a Analysis) *float64 { return a.Steps.P50MS }},
	{"step_p95_ms", "Step time p95", "ms", "lower", func(a Analysis) *float64 { return a.Steps.P95MS }},
	{"decode_share", "Decode share of GPU time", "ratio", "", func(a Analysis) *float64 {
		if a.Phases.Source == "unknown" {
			return nil
		}
		return ptr(a.Phases.Decode.Share)
	}},
	// A roofline whose position is unknown covers too little matrix-multiply
	// time to stand for the capture, so it is not compared.
	{"memory_bound_share", "Matrix-multiply time below the memory roof", "ratio", "lower", func(a Analysis) *float64 {
		if a.Roofline.Position == "unknown" {
			return nil
		}
		return a.Roofline.MemoryBoundShare
	}},
}

// Compare reads base and head side by side. Both need an analysis.
func Compare(base, head Profile, baseActive, headActive bool) (Comparison, error) {
	if base.Analysis == nil || head.Analysis == nil {
		return Comparison{}, ErrNotComparable
	}
	comparison := Comparison{Base: SummaryOf(base, baseActive), Head: SummaryOf(head, headActive), Warnings: []string{}, Metrics: []MetricDelta{}, Categories: []CategoryComparison{}, Kernels: []KernelComparison{}}
	if text(base.Capture.Accelerator) != text(head.Capture.Accelerator) {
		comparison.Warnings = append(comparison.Warnings, "The captures ran on different accelerators.")
	}
	if intValue(base.Capture.GPUs) != intValue(head.Capture.GPUs) {
		comparison.Warnings = append(comparison.Warnings, "The captures ran on a different number of GPUs.")
	}
	if base.Analysis.Trace.Format != head.Analysis.Trace.Format {
		comparison.Warnings = append(comparison.Warnings, "The captures came from different profilers.")
	}
	if base.Analysis.Trace.Truncated || head.Analysis.Trace.Truncated {
		comparison.Warnings = append(comparison.Warnings, "At least one capture was larger than one analysis reads, so it covers part of the trace.")
	}
	for _, spec := range compareMetrics {
		row := MetricDelta{Metric: spec.key, Label: spec.label, Unit: spec.unit, Better: spec.better, Base: spec.read(*base.Analysis), Head: spec.read(*head.Analysis)}
		if row.Base != nil && row.Head != nil {
			delta := roundTo(*row.Head-*row.Base, 4)
			row.Delta = &delta
			if spec.better != "" && significant(*row.Base, *row.Head, spec.unit) {
				improved := delta > 0 == (spec.better == "higher")
				row.Improved = &improved
			}
		}
		comparison.Metrics = append(comparison.Metrics, row)
	}
	baseSeconds, headSeconds := base.Analysis.Trace.WindowMS/1000, head.Analysis.Trace.WindowMS/1000
	baseCategories, headCategories := categoryIndex(*base.Analysis), categoryIndex(*head.Analysis)
	for _, category := range Categories {
		left, right := baseCategories[category], headCategories[category]
		if left.KernelMS == 0 && right.KernelMS == 0 {
			continue
		}
		comparison.Categories = append(comparison.Categories, CategoryComparison{
			Category: category, BaseMSPerS: roundTo(left.KernelMS/baseSeconds, 3), HeadMSPerS: roundTo(right.KernelMS/headSeconds, 3),
			BaseShare: left.Share, HeadShare: right.Share,
		})
	}
	kernels := map[string]*KernelComparison{}
	var order []string
	add := func(kernel KernelStat, seconds float64, head bool) {
		row := kernels[kernel.Name]
		if row == nil {
			row = &KernelComparison{Name: kernel.Name, Category: kernel.Category}
			kernels[kernel.Name] = row
			order = append(order, kernel.Name)
		}
		value := roundTo(kernel.TotalMS/seconds, 3)
		if head {
			row.HeadMSPerS = &value
		} else {
			row.BaseMSPerS = &value
		}
	}
	for _, kernel := range base.Analysis.TopKernels {
		add(kernel, baseSeconds, false)
	}
	for _, kernel := range head.Analysis.TopKernels {
		add(kernel, headSeconds, true)
	}
	larger := func(row *KernelComparison) float64 { return math.Max(value(row.BaseMSPerS), value(row.HeadMSPerS)) }
	sort.SliceStable(order, func(i, j int) bool {
		left, right := larger(kernels[order[i]]), larger(kernels[order[j]])
		if left != right {
			return left > right
		}
		return order[i] < order[j]
	})
	for index, name := range order {
		if index == MaxCompareKernels {
			break
		}
		comparison.Kernels = append(comparison.Kernels, *kernels[name])
	}
	return comparison, nil
}

// significant reports whether two values differ by more than the reporting
// threshold: two percentage points for shares, and two percent otherwise.
func significant(base, head float64, unit string) bool {
	if unit == "ratio" {
		return math.Abs(head-base) > compareNoise
	}
	scale := math.Max(math.Abs(base), math.Abs(head))
	return scale > 0 && math.Abs(head-base)/scale > compareNoise
}

func categoryIndex(analysis Analysis) map[string]CategoryTime {
	index := map[string]CategoryTime{}
	for _, category := range analysis.Categories {
		index[category.Category] = category
	}
	return index
}

func ptr(value float64) *float64 { return &value }

func value(pointer *float64) float64 {
	if pointer == nil {
		return 0
	}
	return *pointer
}

func text(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}

func intValue(pointer *int) int {
	if pointer == nil {
		return 0
	}
	return *pointer
}

func roundTo(number float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(number*scale) / scale
}
