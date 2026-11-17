package metrics

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseAMDDeviceMetricsReadsOnlyConfirmedFields(t *testing.T) {
	payload, err := os.ReadFile("testdata/amd_device_metrics_exporter_1.5.3.prom")
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	rows, err := ParseAMDDeviceMetrics(string(payload), AMDDeviceMetricsOptions{Selector: map[string]string{"pod": "qwen-0"}, ReplicaID: "replica-1", ObservedAt: observed, TTL: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, row := range rows {
		got[row.Name] = row.Value
		if row.Source != "amd_device_metrics_exporter" || row.EvidenceClass != "measured" || row.ReplicaID != "replica-1" || !row.ValidUntil.Equal(observed.Add(2*time.Minute)) {
			t.Fatalf("row=%+v", row)
		}
	}
	// Utilization is the busiest selected GPU; power counts only the first
	// spelling seen (package power), so GPU 0 is not summed twice; another
	// pod's GPU is never read.
	want := map[string]float64{MeasurementGPUUtilization: 81, MeasurementGPUPower: 1116, MeasurementGPUTemperature: 76, MeasurementGPUDevices: 2}
	if len(got) != len(want) {
		t.Fatalf("measurements=%v", got)
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s=%v, want %v (all: %v)", name, got[name], value, got)
		}
	}
	for _, unknown := range []string{MeasurementGPUMemory, MeasurementGPUDRAMActive, MeasurementGPUSMActive, MeasurementGPUSMOccupancy, MeasurementGPUTensorActive} {
		if _, present := got[unknown]; present {
			t.Fatalf("%s was mapped from an unconfirmed AMD field", unknown)
		}
	}
}

func TestParseAMDDeviceMetricsHonorsThePrefixAndRejectsHostileInput(t *testing.T) {
	rows, err := ParseAMDDeviceMetrics("amd_gpu_gfx_activity{serial_number=\"a\"} 12\ngpu_gfx_activity{serial_number=\"b\"} 99\n", AMDDeviceMetricsOptions{MetricPrefix: "amd_", TTL: time.Minute})
	if err != nil || len(rows) != 2 || rows[0].Value != 12 {
		t.Fatalf("prefixed rows=%+v err=%v", rows, err)
	}
	for name, payload := range map[string]string{
		"out of range":  "gpu_gfx_activity{serial_number=\"a\"} 140\n",
		"negative":      "gpu_package_power{serial_number=\"a\"} -1\n",
		"non-finite":    "gpu_package_power{serial_number=\"a\"} NaN\n",
		"bad label":     "gpu_gfx_activity{serial_number=a} 1\n",
		"no value":      "gpu_gfx_activity\n",
		"nothing known": "gpu_used_vram{serial_number=\"a\"} 1\n",
		"too large":     strings.Repeat("#", maxDCGMPayloadBytes+1),
	} {
		if _, err := ParseAMDDeviceMetrics(payload, AMDDeviceMetricsOptions{TTL: time.Minute}); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if _, err := ParseAMDDeviceMetrics("gpu_gfx_activity{serial_number=\"a\"} 1\n", AMDDeviceMetricsOptions{}); err == nil {
		t.Fatal("missing TTL was accepted")
	}
	// Series without a GPU identity leave the device count unknown.
	rows, err = ParseAMDDeviceMetrics("gpu_gfx_activity{pod=\"x\"} 5\n", AMDDeviceMetricsOptions{TTL: time.Minute})
	if err != nil || len(rows) != 1 || rows[0].Name != MeasurementGPUUtilization {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}
