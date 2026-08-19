package metrics

import (
	"strings"
	"testing"
	"time"
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
	if len(rows) != 5 {
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
	if byName["gpu_utilization"] != 75 || byName["gpu_memory"] != 3*1024*1024*1024 || byName["gpu_temperature"] != 71 || byName["gpu_power"] != 430.5 || byName["gpu_xid_errors"] != 0 {
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
	if err != nil || len(rows) != 1 || rows[0].Value != 75 {
		t.Fatalf("explicit ratio conversion rows=%+v err=%v", rows, err)
	}
}
