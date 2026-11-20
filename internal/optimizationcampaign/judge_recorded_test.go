package optimizationcampaign

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// TestRecordedH100CampaignKeepsItsDecisions re-judges the real interleaved
// runs of the first datacenter-GPU campaign (Qwen3-8B on Modal H100s,
// 2026-11-20, measured with v1's three rounds) with the paired comparison.
// Both candidates beat vLLM defaults far outside the noise in v1; the paired
// judge must agree, at the campaign's two comparisons, and report gains
// close to the ones the run published.
func TestRecordedH100CampaignKeepsItsDecisions(t *testing.T) {
	raw, err := os.ReadFile("../../docs/testing/evidence/qwen3-8b-campaign-modal-h100-2026-11-20.json")
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		CampaignRecord struct {
			Candidates []struct {
				Label       string `json:"label"`
				Measurement struct {
					Runs []struct {
						Kind                  string   `json:"kind"`
						Arm                   string   `json:"arm"`
						OutputTokenThroughput *float64 `json:"output_token_throughput"`
						TTFTP95MS             *float64 `json:"ttft_p95_ms"`
					} `json:"runs"`
				} `json:"measurement"`
			} `json:"candidates"`
		} `json:"campaign_record"`
	}
	if err = json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"FP8 weights (quantized by vLLM at load)": 0.187, "Compact FP8 KV cache": 0.093}
	judged := 0
	for _, candidate := range evidence.CampaignRecord.Candidates {
		var runs []RunSummary
		for _, run := range candidate.Measurement.Runs {
			if run.Kind == RunKindTurn {
				runs = append(runs, RunSummary{Arm: run.Arm, OutputTokenThroughput: run.OutputTokenThroughput, TTFTP95MS: run.TTFTP95MS})
			}
		}
		if len(runs) == 0 || runs[0].Arm != "control" {
			continue // the control measured alone
		}
		gain, known := want[candidate.Label]
		if !known {
			t.Fatalf("unexpected candidate %q in the recorded campaign", candidate.Label)
		}
		comparison, ok := comparePaired(runs, func(r RunSummary) *float64 { return r.OutputTokenThroughput }, true, 2)
		if !ok || comparison.Decision != DecisionImproved || math.Abs(comparison.Delta-gain) > 0.01 || comparison.Pairs != 3 {
			t.Errorf("%s: %+v, want improved by about %.1f%%", candidate.Label, comparison, gain*100)
		}
		judged++
	}
	if judged != 2 {
		t.Fatalf("judged %d recorded candidates, want 2", judged)
	}
}
