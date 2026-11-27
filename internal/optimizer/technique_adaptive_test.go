package optimizer

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/metrics"
	contract "github.com/infercrane/infercrane/internal/technique"
)

func mapSetup(cell mapCell, args []string, loads ...int) contract.Setup {
	return contract.Setup{Model: "Qwen/Qwen3.8-27B-FP8", ModelRevision: "c3a462cc3d38", ModelFeatures: []string{FeatureNativeMTP}, Engine: "vllm",
		EngineVersion: "0.34.0", Accelerator: "H100", GPUCount: 1, Args: args, Signals: cell.diagnosis().Signals, Loads: loads}
}

var mapThreeDrafts = []string{"--max-num-seqs", "512", "--speculative-config", `{"method":"mtp","num_speculative_tokens":3}`}

func TestAdaptiveSpeculationRegistersAndProposesTwoBands(t *testing.T) {
	registry, err := contract.NewRegistry(AdaptiveSpeculation{})
	if err != nil {
		t.Fatal(err)
	}
	// The map's server with 3 draft tokens at 32 at once, judged at 32 and 128.
	candidates, skipped, err := registry.Candidates(context.Background(), mapSetup(shortCell(32, 3, 31, 0, 53), mapThreeDrafts, 32, 128))
	if err != nil || len(skipped) != 0 || len(candidates) != 1 {
		t.Fatalf("candidates %+v, skipped %+v, err %v", candidates, skipped, err)
	}
	candidate := candidates[0]
	if len(candidate.Bands) != 2 || candidate.Bands[1].UpTo != 0 || candidate.Bands[0].UpTo < 55 || candidate.Bands[0].UpTo > 60 {
		t.Fatalf("bands %+v; want three drafts up to the map's room (about 58), one draft above", candidate.Bands)
	}
	if got := strings.Join(candidate.Bands[0].Args, " "); got != strings.Join(mapThreeDrafts, " ") {
		t.Fatalf("first band %s", got)
	}
	if got := strings.Join(candidate.Bands[1].Args, " "); got != `--max-num-seqs 512 --speculative-config {"method":"mtp","num_speculative_tokens":1}` {
		t.Fatalf("second band %s", got)
	}
	// At 32 the setup for the present load is the first band's.
	if strings.Join(candidate.Args, " ") != strings.Join(mapThreeDrafts, " ") || !candidate.ChangesAnswers {
		t.Fatalf("candidate %+v", candidate)
	}
	manifest := AdaptiveSpeculation{}.Manifest()
	if manifest.Runtime.Policy != contract.RuntimeAdaptive || manifest.Fallback.Serves != contract.FallbackStartingSetup ||
		!slices.Contains(manifest.Costs, contract.LimitHostMemory) || !slices.Equal(manifest.Evidence, []string{"swap-timings"}) {
		t.Fatalf("manifest %+v", manifest)
	}
}

func TestAdaptiveSpeculationProposesNothingWithoutABandToGain(t *testing.T) {
	cases := map[string]contract.Setup{
		"every load fits three drafts": mapSetup(shortCell(32, 3, 31, 0, 53), mapThreeDrafts, 32, 48),
		"no speculation to start from": mapSetup(shortCell(128, 0, 127, 0, 59), []string{"--max-num-seqs", "512"}, 32, 128),
	}
	noState := mapSetup(shortCell(32, 3, 31, 0, 53), mapThreeDrafts, 32, 128)
	noState.Signals = map[string]float64{}
	for key, value := range shortCell(32, 3, 31, 0, 53).diagnosis().Signals {
		noState.Signals[key] = value
	}
	delete(noState.Signals, metrics.MeasurementStatePageBytes)
	cases["no recurrent state"] = noState
	for name, setup := range cases {
		candidates, why, err := (AdaptiveSpeculation{}).Candidates(context.Background(), setup)
		if err != nil || len(candidates) != 0 || why == "" {
			t.Errorf("%s: candidates %+v, why %q, err %v", name, candidates, why, err)
		}
	}
	other := mapSetup(shortCell(32, 3, 31, 0, 53), mapThreeDrafts, 32, 128)
	other.Accelerator = "L4"
	if ok, why := (AdaptiveSpeculation{}).Manifest().Admits(other); ok || !strings.Contains(why, "L4") {
		t.Fatalf("admitted an unchecked GPU: %v %q", ok, why)
	}
}
