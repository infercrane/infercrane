package gpuprofile_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/gputrace"
	"github.com/infercrane/infercrane/internal/testtools/tracefixture"
)

func sampleRecording(t *testing.T, base time.Time) gpuprofile.Recording {
	t.Helper()
	trace, err := gputrace.ParseKineto(bytes.NewReader(tracefixture.VLLMSingleGPU(base)))
	if err != nil {
		t.Fatal(err)
	}
	recording, err := gputrace.Profile(trace, gputrace.Options{Engine: "vllm"}, gputrace.CaptureFromTrace(trace, "torch-profiler", "vllm-0.22.1", time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	return recording
}

func TestV2RecordingNeedsAValidAnalysis(t *testing.T) {
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	now := base.Add(time.Hour)
	if err := gpuprofile.Validate(sampleRecording(t, base), now); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*gpuprofile.Recording){
		"v2 without analysis":        func(r *gpuprofile.Recording) { r.Analysis = nil },
		"v1 with analysis":           func(r *gpuprofile.Recording) { r.SchemaVersion = gpuprofile.SchemaVersion },
		"unknown algorithm":          func(r *gpuprofile.Recording) { r.Analysis.AlgorithmVersion = "gpu-trace-analysis-v0" },
		"busy above one":             func(r *gpuprofile.Recording) { r.Analysis.Utilization.BusyShare = 1.2 },
		"busy and idle disagree":     func(r *gpuprofile.Recording) { r.Analysis.Utilization.IdleShare = 0.9 },
		"unknown verdict":            func(r *gpuprofile.Recording) { r.Analysis.Verdict.Bottleneck = "vibes" },
		"blank sentence":             func(r *gpuprofile.Recording) { r.Analysis.Verdict.Sentence = "" },
		"half-known idle causes":     func(r *gpuprofile.Recording) { r.Analysis.Idle.OtherShare = nil },
		"unknown without a reason":   func(r *gpuprofile.Recording) { r.Analysis.Launch.ShortKernelShare, r.Analysis.Launch.Reason = nil, "" },
		"overlap without reason":     func(r *gpuprofile.Recording) { r.Analysis.Overlap.Reason = "" },
		"window longer than capture": func(r *gpuprofile.Recording) { r.Analysis.Trace.WindowMS = 10 * 60 * 1000 },
		"more devices than gpus":     func(r *gpuprofile.Recording) { r.Analysis.Trace.Devices = 2 },
		"control characters":         func(r *gpuprofile.Recording) { r.Analysis.TopKernels[0].Name = "gemm\nprompt" },
		"unknown category":           func(r *gpuprofile.Recording) { r.Analysis.Categories[0].Category = "magic" },
		"roofline known without peak": func(r *gpuprofile.Recording) {
			r.Analysis.Roofline.Position, r.Analysis.Roofline.Peak = "memory_bound", nil
		},
		"excerpt event outside": func(r *gpuprofile.Recording) { r.TimelineExcerpt.Events[0][2] = 1e9 },
		"excerpt unknown lane":  func(r *gpuprofile.Recording) { r.TimelineExcerpt.Events[0][0] = 999 },
		"excerpt past window":   func(r *gpuprofile.Recording) { r.TimelineExcerpt.StartMS = r.Analysis.Trace.WindowMS },
		"hardware unknown field": func(r *gpuprofile.Recording) {
			value := 50.0
			r.Analysis.Hardware = &gpuprofile.HardwareSeries{Source: "dcgm-exporter", IntervalMS: 1000, Series: []gpuprofile.FieldSeries{{Field: "gpu_magic", Unit: "percent", Values: []*float64{&value}}}}
		},
		"hardware percent above 100": func(r *gpuprofile.Recording) {
			value := 150.0
			r.Analysis.Hardware = &gpuprofile.HardwareSeries{Source: "dcgm-exporter", IntervalMS: 1000, Series: []gpuprofile.FieldSeries{{Field: "gpu_sm_active", Unit: "percent", Values: []*float64{&value}}}}
		},
	}
	original, err := json.Marshal(sampleRecording(t, base))
	if err != nil {
		t.Fatal(err)
	}
	clone := func() gpuprofile.Recording {
		var copied gpuprofile.Recording
		if err := json.Unmarshal(original, &copied); err != nil {
			t.Fatal(err)
		}
		return copied
	}
	for name, mutate := range cases {
		recording := clone()
		mutate(&recording)
		if err := gpuprofile.Validate(recording, now); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	withHardware := clone()
	value := 61.5
	withHardware.Analysis.Hardware = &gpuprofile.HardwareSeries{Source: "dcgm-exporter", IntervalMS: 1000, Series: []gpuprofile.FieldSeries{{Field: "gpu_sm_active", Unit: "percent", Values: []*float64{&value, nil}}}}
	if err := gpuprofile.Validate(withHardware, now); err != nil {
		t.Fatalf("a DCGM series with a missing sample stays valid: %v", err)
	}
}

func profileOf(recording gpuprofile.Recording, id string, revision int) gpuprofile.Profile {
	profile := gpuprofile.ProfileFromRecording(recording)
	profile.Capture.ID, profile.Revision, profile.Deployment = id, revision, "coder"
	return profile
}

func TestCompareReportsDeltasAndKeepsUnknownsUnknown(t *testing.T) {
	base := time.Now().UTC().Add(-time.Hour)
	before := profileOf(sampleRecording(t, base), "gpc-00000000000000000000000000000001", 3)
	afterRecording := sampleRecording(t, base.Add(time.Minute))
	afterRecording.Analysis.Utilization.BusyShare, afterRecording.Analysis.Utilization.IdleShare = 0.9, 0.1
	afterRecording.Analysis.Steps.P50MS = nil
	after := profileOf(afterRecording, "gpc-00000000000000000000000000000002", 4)
	comparison, err := gpuprofile.Compare(before, after, false, true)
	if err != nil {
		t.Fatal(err)
	}
	metrics := map[string]gpuprofile.MetricDelta{}
	for _, metric := range comparison.Metrics {
		metrics[metric.Metric] = metric
	}
	busy := metrics["busy_share"]
	if busy.Base == nil || busy.Head == nil || busy.Delta == nil || busy.Improved == nil || !*busy.Improved || *busy.Head != 0.9 {
		t.Fatalf("busy share delta %+v", busy)
	}
	if idle := metrics["idle_share"]; idle.Improved == nil || !*idle.Improved {
		t.Fatalf("less idle is better: %+v", idle)
	}
	if step := metrics["step_p50_ms"]; step.Head != nil || step.Delta != nil || step.Improved != nil {
		t.Fatalf("an unknown side keeps the delta unknown: %+v", step)
	}
	if overlap := metrics["overlapped_share"]; overlap.Base != nil || overlap.Improved != nil {
		t.Fatalf("a single-GPU capture has no overlap to compare: %+v", overlap)
	}
	if same := metrics["host_wait_share"]; same.Improved != nil {
		t.Fatalf("an unchanged value is neither better nor worse: %+v", same)
	}
	if !comparison.Head.Active || comparison.Base.Active || comparison.Base.Revision != 3 || len(comparison.Kernels) == 0 || len(comparison.Categories) == 0 {
		t.Fatalf("comparison %+v", comparison)
	}
	if len(comparison.Warnings) != 0 {
		t.Fatalf("same accelerator and profiler: %v", comparison.Warnings)
	}
	other := after
	accelerator := "NVIDIA L40S"
	other.Capture.Accelerator = &accelerator
	warned, _ := gpuprofile.Compare(before, other, false, true)
	if len(warned.Warnings) != 1 || !strings.Contains(warned.Warnings[0], "different accelerators") {
		t.Fatalf("warnings %v", warned.Warnings)
	}
	v1 := before
	v1.Analysis = nil
	if _, err = gpuprofile.Compare(v1, after, false, true); !errors.Is(err, gpuprofile.ErrNotComparable) {
		t.Fatalf("a v1 capture has nothing to compare: %v", err)
	}
}

func TestSummaryCarriesTheVerdictOnlyWhenAnalyzed(t *testing.T) {
	profile := profileOf(sampleRecording(t, time.Now().UTC().Add(-time.Hour)), "gpc-00000000000000000000000000000001", 2)
	summary := gpuprofile.SummaryOf(profile, true)
	if summary.Verdict == nil || summary.BusyShare == nil || !summary.Active || summary.SchemaVersion != gpuprofile.SchemaVersionV2 {
		t.Fatalf("summary %+v", summary)
	}
	profile.Analysis = nil
	if bare := gpuprofile.SummaryOf(profile, false); bare.Verdict != nil || bare.BusyShare != nil {
		t.Fatalf("a v1 summary has no verdict: %+v", bare)
	}
}
