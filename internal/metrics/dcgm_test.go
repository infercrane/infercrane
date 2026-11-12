package metrics

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestParseDCGMAggregatesSelectedQualifiedEvidence(t *testing.T) {
	observed := time.Date(2026, 8, 19, 20, 0, 0, 0, time.UTC)
	payload := `
# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization
DCGM_FI_DEV_GPU_UTIL{gpu="0",deployment="coder"} 75
DCGM_FI_DEV_GPU_UTIL{gpu="1",deployment="coder"} 42
DCGM_FI_DEV_GPU_UTIL{gpu="0",deployment="other"} 99
DCGM_FI_DEV_FB_USED{gpu="0",deployment="coder"} 1024
DCGM_FI_DEV_FB_USED{gpu="1",deployment="coder"} 2048
DCGM_FI_DEV_GPU_TEMP{gpu="0",deployment="coder"} 71
DCGM_FI_DEV_GPU_TEMP{gpu="1",deployment="coder"} 68
DCGM_FI_DEV_POWER_USAGE{gpu="0",deployment="coder"} 220.5
DCGM_FI_DEV_POWER_USAGE{gpu="1",deployment="coder"} 210
DCGM_FI_DEV_XID_ERRORS{gpu="0",deployment="coder"} 0
unrelated_metric 17
`
	rows, err := ParseDCGM(payload, DCGMOptions{Selector: map[string]string{"deployment": "coder"}, ReplicaID: "replica-1", ObservedAt: observed, TTL: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("measurements=%+v", rows)
	}
	byName := map[string]float64{}
	for _, row := range rows {
		byName[row.Name] = row.Value
		expectedSamples := 2
		if row.Name == "gpu_xid_errors" {
			expectedSamples = 1
		}
		if row.EvidenceClass != "measured" || row.Source != "dcgm_exporter" || row.ReplicaID != "replica-1" || row.SampleCount != expectedSamples || !row.ValidUntil.Equal(observed.Add(2*time.Minute)) {
			t.Fatalf("row=%+v", row)
		}
	}
	if byName["gpu_utilization"] != 75 || byName["gpu_memory"] != 3*1024*1024*1024 || byName["gpu_temperature"] != 71 || byName["gpu_power"] != 430.5 || byName["gpu_xid_errors"] != 0 || byName["gpu_devices"] != 2 {
		t.Fatalf("aggregates=%+v", byName)
	}
}

func TestParseDCGMRejectsAmbiguousOrHostileEvidence(t *testing.T) {
	options := DCGMOptions{ObservedAt: time.Now().UTC(), TTL: time.Minute}
	for name, input := range map[string]string{
		"nan":        `DCGM_FI_DEV_GPU_UTIL{gpu="0"} NaN`,
		"range":      `DCGM_FI_DEV_GPU_UTIL{gpu="0"} 101`,
		"labels":     `DCGM_FI_DEV_GPU_UTIL{gpu=nope} 10`,
		"no metrics": `other_metric 1`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDCGM(input, options); err == nil {
				t.Fatal("unsafe evidence was accepted")
			}
		})
	}
	if _, err := ParseDCGM(strings.Repeat("x", maxDCGMPayloadBytes+1), options); err == nil {
		t.Fatal("oversized payload was accepted")
	}
	options.UtilizationUnit = "ratio"
	rows, err := ParseDCGM(`DCGM_FI_DEV_GPU_UTIL{gpu="0"} 0.75`, options)
	if err != nil || len(rows) != 2 || rows[0].Value != 75 || rows[1].Name != MeasurementGPUDevices || rows[1].Value != 1 {
		t.Fatalf("explicit ratio conversion rows=%+v err=%v", rows, err)
	}
}

func TestParseDCGMReadsExporter49ProfilingFields(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "dcgm_exporter_4.9.prom"))
	if err != nil {
		t.Fatal(err)
	}
	options := DCGMOptions{Selector: map[string]string{"pod": "coder-0"}, ObservedAt: time.Now().UTC(), TTL: time.Minute}
	rows, err := ParseDCGM(string(raw), options)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]domain.OperationalMeasurement{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	for name, want := range map[string]float64{
		MeasurementGPUUtilization: 100, MeasurementGPUMemory: 2 * 76235669504, MeasurementGPUTemperature: 67, MeasurementGPUPower: 1011,
		MeasurementGPUDevices: 2, MeasurementGPUSMActive: 90, MeasurementGPUSMOccupancy: 20, MeasurementGPUTensorActive: 13,
		MeasurementGPUDRAMActive: 70, MeasurementGPUPCIeThroughput: 3.2e9, MeasurementGPUNVLinkThroughput: 3.15e11,
	} {
		if row, ok := byName[name]; !ok || math.Abs(row.Value-want) > 1e-6 {
			t.Errorf("%s = %+v, want %v", name, row, want)
		}
	}
	if byName[MeasurementGPUSMActive].SampleCount != 2 || byName[MeasurementGPUSMActive].Unit != "percent" || byName[MeasurementGPUNVLinkThroughput].Unit != "bytes_per_second" {
		t.Fatalf("profiling envelope=%+v", byName[MeasurementGPUSMActive])
	}
	// The 4.9 XID field is the last XID code, not a count, so it is not read.
	for _, name := range []string{MeasurementGPUXIDErrors, MeasurementGPUPCIeSaturation, MeasurementGPUNVLinkSaturation} {
		if _, ok := byName[name]; ok {
			t.Fatalf("%s must stay unknown: %+v", name, byName[name])
		}
	}

	options.NVLinkPeakBytesPerSecond, options.PCIePeakBytesPerSecond = 450e9, 2.5e9
	rows, err = ParseDCGM(string(raw), options)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		byName[row.Name] = row
	}
	if got := byName[MeasurementGPUNVLinkSaturation].Value; got != 70 {
		t.Fatalf("NVLink saturation=%v", got)
	}
	// PCIe throughput includes protocol headers and may exceed the payload peak.
	if got := byName[MeasurementGPUPCIeSaturation].Value; got != 100 {
		t.Fatalf("PCIe saturation=%v", got)
	}
	if _, err = ParseDCGM(string(raw), DCGMOptions{TTL: time.Minute, NVLinkPeakBytesPerSecond: -1}); err == nil {
		t.Fatal("negative declared peak accepted")
	}
}

func TestParseDCGMKeepsProfilingFieldsUnknownAndCountsOneSpelling(t *testing.T) {
	options := DCGMOptions{ObservedAt: time.Now().UTC(), TTL: time.Minute}
	rows, err := ParseDCGM("DCGM_FI_DEV_GPU_UTIL{gpu=\"0\"} 80\n", options)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.HasPrefix(row.Name, "gpu_sm") || row.Name == MeasurementGPUTensorActive || row.Name == MeasurementGPUDRAMActive {
			t.Fatalf("absent profiling field reported: %+v", row)
		}
	}
	// One GPU exported under both the legacy and the 4.9 spelling counts once.
	rows, err = ParseDCGM("DCGM_FI_PROF_DRAM_ACTIVE{gpu=\"0\"} 0.4\nDCGM_FI_PROF_DRAM_UTIL_RATIO{gpu=\"0\"} 0.4\nDCGM_FI_DEV_FB_USED{gpu=\"0\"} 1\nDCGM_FI_DEV_FB_USED_BYTES{gpu=\"0\"} 1048576\n", options)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Name == MeasurementGPUDRAMActive && (row.Value != 40 || row.SampleCount != 1) || row.Name == MeasurementGPUMemory && row.Value != 1048576 {
			t.Fatalf("double-counted spelling: %+v", row)
		}
	}
	for name, input := range map[string]string{
		"ratio above one":    "DCGM_FI_PROF_SM_UTIL_RATIO{gpu=\"0\"} 1.2",
		"negative bandwidth": "DCGM_FI_PROF_NVLINK_TX_BYTES{gpu=\"0\"} -5",
		"util ratio above":   "DCGM_FI_DEV_GPU_UTIL_RATIO{gpu=\"0\"} 1.5",
	} {
		if _, err = ParseDCGM(input, options); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// A series without a GPU label leaves the device count unknown.
	rows, err = ParseDCGM("DCGM_FI_DEV_GPU_TEMP 60\n", options)
	if err != nil || len(rows) != 1 || rows[0].Name != MeasurementGPUTemperature {
		t.Fatalf("unlabeled series rows=%+v err=%v", rows, err)
	}
}
