package resultprior

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/resultstore"
	"github.com/infercrane/infercrane/internal/technique"
)

var (
	plainKnobs = resultstore.Knobs{Engine: "vllm"}.Normal()
	mtpKnobs   = resultstore.Knobs{Engine: "vllm", Speculation: "mtp", Drafts: 3}.Normal()
	fp8KVKnobs = resultstore.Knobs{Engine: "vllm", KVCache: "fp8"}.Normal()
	hybrid27   = resultstore.Model{ID: "org/hybrid-27b", Architecture: resultstore.ArchitectureHybrid, ParamsB: 27, Checkpoint: "fp8", Features: []string{"native_mtp"}}
)

// cellRecords makes the records of one measured cell: each setup's median,
// with a 1% range.
func cellRecords(t *testing.T, set, gpu, kind string, load int, model resultstore.Model, values map[resultstore.Knobs]float64) []resultstore.Record {
	t.Helper()
	var records []resultstore.Record
	for knobs, value := range values {
		record := resultstore.Record{
			Model:    model,
			Setup:    resultstore.Setup{Name: strings.NewReplacer(" ", "-", ",", "").Replace(knobs.Words()), Speculation: knobs.Speculation, Drafts: knobs.Drafts, Quantization: knobs.Quantization},
			Cell:     resultstore.Cell{Set: set},
			Observed: resultstore.Observed{Median: value, Min: value * 0.995, Max: value * 1.005, Runs: 3},
			Result: technique.Result{SchemaVersion: technique.ResultSchemaV1, Technique: "starting-setup", TechniqueVersion: "0.0.0", Candidate: "c",
				Hardware: technique.Hardware{Accelerator: gpu, Count: 1}, Software: technique.Software{Engine: knobs.Engine, EngineVersion: "0.34.0"},
				Workload: technique.Workload{Kind: kind, Load: load}, Precision: technique.Precision{Weights: "fp8", KVCache: knobs.KVCache},
				Measured: technique.Measured{Metric: resultstore.MetricOutput, Decision: technique.DecisionUnknown}},
			Source:     resultstore.Source{Kind: "test", Importer: "test"},
			RecordedAt: time.Date(2001, 1, 2, 12, 0, 0, 0, time.UTC),
		}
		record.Cell.Key = kind + "/" + itoa(load)
		sealed, err := record.Seal()
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, sealed)
	}
	return records
}

func itoa(value int) string {
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func query(kind string, load int) Query {
	return Query{Model: hybrid27, Accelerator: "H100", EngineVersion: "0.34.0", Kind: kind, Load: load, Metric: resultstore.MetricOutput, Current: plainKnobs,
		Candidates: []Candidate{{Name: "mtp", Knobs: mtpKnobs}, {Name: "fp8-kv", Knobs: fp8KVKnobs}, {Name: "draft-model", Knobs: resultstore.Knobs{Engine: "vllm", Speculation: "draft-model", Drafts: 4}}}}
}

func TestRankPrefersWhatWonInTheClosestCells(t *testing.T) {
	var records []resultstore.Record
	// At low load speculation won; at high load the FP8 KV cache did.
	records = append(records, cellRecords(t, "map", "H100", "short", 8, hybrid27, map[resultstore.Knobs]float64{plainKnobs: 100, mtpKnobs: 180, fp8KVKnobs: 102})...)
	records = append(records, cellRecords(t, "map", "H100", "short", 16, hybrid27, map[resultstore.Knobs]float64{plainKnobs: 200, mtpKnobs: 330, fp8KVKnobs: 205})...)
	records = append(records, cellRecords(t, "map", "H100", "short", 128, hybrid27, map[resultstore.Knobs]float64{plainKnobs: 1000, mtpKnobs: 900, fp8KVKnobs: 1150})...)
	records = append(records, cellRecords(t, "map", "H100", "short", 256, hybrid27, map[resultstore.Knobs]float64{plainKnobs: 1100, mtpKnobs: 950, fp8KVKnobs: 1300})...)
	low := Rank(records, query("short", 12), DefaultSettings())
	if low.Ranked[0].Name != "mtp" || low.Algorithm != AlgorithmV1 || low.Cells != 4 {
		t.Fatalf("at 12 at once speculation should lead: %+v", low.Ranked)
	}
	high := Rank(records, query("short", 192), DefaultSettings())
	if high.Ranked[0].Name != "fp8-kv" {
		t.Fatalf("at 192 at once the FP8 KV cache should lead: %+v", high.Ranked)
	}
	// A setup no cell measured is last, unknown, and says so.
	last := high.Ranked[len(high.Ranked)-1]
	if last.Name != "draft-model" || last.Known || !strings.HasPrefix(last.Reason, "unknown") || last.Lower >= 0 {
		t.Fatalf("unmeasured setup: %+v", last)
	}
	if got := Relative(0.5); got != 1 {
		t.Fatalf("a gain of 0.5 is twice the output: %v", got)
	}
	if !strings.Contains(high.Ranked[0].Reason, "similar cell") || len(high.Ranked[0].Evidence) == 0 || high.Ranked[0].Evidence[0].Cell != "map short/256" {
		t.Fatalf("explanation: %+v", high.Ranked[0])
	}
}

func TestRankShrinksThinEvidenceAndReadsTheLowerBound(t *testing.T) {
	// One far cell with a large gain against several close cells with a modest one.
	far := resultstore.Model{ID: "org/dense-8b", Architecture: resultstore.ArchitectureDense, ParamsB: 8, Checkpoint: "bf16"}
	records := cellRecords(t, "other", "L4", "long", 4, far, map[resultstore.Knobs]float64{plainKnobs: 100, fp8KVKnobs: 190})
	for _, load := range []int{16, 32, 64} {
		records = append(records, cellRecords(t, "map", "H100", "short", load, hybrid27, map[resultstore.Knobs]float64{plainKnobs: 100, mtpKnobs: 112})...)
	}
	ranking := Rank(records, query("short", 32), DefaultSettings())
	byName := map[string]Ranked{}
	for _, ranked := range ranking.Ranked {
		byName[ranked.Name] = ranked
	}
	mtp, kv := byName["mtp"], byName["fp8-kv"]
	if kv.Estimate >= 0.1 || kv.Weight >= 0.1 {
		t.Fatalf("one far cell should be shrunk toward no change: %+v", kv)
	}
	if mtp.Lower <= kv.Lower || ranking.Ranked[0].Name != "mtp" {
		t.Fatalf("three close cells should outrank one far one: %+v", ranking.Ranked)
	}
	if mtp.Lower >= mtp.Estimate {
		t.Fatalf("the lower bound is below the estimate: %+v", mtp)
	}
}

func TestDistanceTerms(t *testing.T) {
	q := query("short", 32)
	same := Distance(q, hybrid27, "H100", "0.34.0", "short", 32)
	if same != 0 {
		t.Fatalf("the same cell is at distance %v", same)
	}
	for name, got := range map[string]float64{
		"double the load": Distance(q, hybrid27, "H100", "0.34.0", "short", 64),
		"reasoning on":    Distance(q, hybrid27, "H100", "0.34.0", "short-reasoning", 32),
		"another decode":  Distance(q, hybrid27, "H100", "0.34.0", "output", 32),
		"prompt-heavy":    Distance(q, hybrid27, "H100", "0.34.0", "long", 32),
		"H200":            Distance(q, hybrid27, "H200", "0.34.0", "short", 32),
		"L4":              Distance(q, hybrid27, "L4", "0.34.0", "short", 32),
		"unknown GPU":     Distance(q, hybrid27, "Gaudi 3", "0.34.0", "short", 32),
		"engine version":  Distance(q, hybrid27, "H100", "0.22.1", "short", 32),
		"another model":   Distance(q, resultstore.Model{Architecture: resultstore.ArchitectureDense, ParamsB: 8, Checkpoint: "bf16"}, "H100", "0.34.0", "short", 32),
	} {
		want := map[string][2]float64{
			"double the load": {0.75, 0.75}, "reasoning on": {0.5, 0.5}, "another decode": {1, 1}, "prompt-heavy": {2, 2},
			"H200": {0.3, 0.4}, "L4": {3, 4}, "unknown GPU": {2.5, 2.5}, "engine version": {0.25, 0.25}, "another model": {2.6, 2.62},
		}[name]
		if got < want[0]-1e-9 || got > want[1]+1e-9 {
			t.Errorf("%s: distance %v, want %v to %v", name, got, want[0], want[1])
		}
	}
}

func TestStorePriorReadsSharedNumbersOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	local := resultstore.Local{Path: filepath.Join(t.TempDir(), "results.jsonl")}
	own := cellRecords(t, "map", "H100", "short", 32, hybrid27, map[resultstore.Knobs]float64{plainKnobs: 100, mtpKnobs: 150})
	if _, err := local.PutResultRecords(ctx, resultstore.LocalTenant, own); err != nil {
		t.Fatal(err)
	}
	shared := cellRecords(t, "theirs", "H100", "short", 32, hybrid27, map[resultstore.Knobs]float64{plainKnobs: 100, fp8KVKnobs: 130})
	store := sharedStore{Local: local, shared: shared}
	without, err := StorePrior{Store: store}.Rank(ctx, resultstore.LocalTenant, query("short", 32))
	if err != nil {
		t.Fatal(err)
	}
	with, err := StorePrior{Store: store, UseShared: true}.Rank(ctx, resultstore.LocalTenant, query("short", 32))
	if err != nil {
		t.Fatal(err)
	}
	known := func(ranking Ranking, name string) (Ranked, bool) {
		for _, ranked := range ranking.Ranked {
			if ranked.Name == name {
				return ranked, ranked.Known
			}
		}
		return Ranked{}, false
	}
	if _, ok := known(without, "fp8-kv"); ok {
		t.Fatal("another tenant's numbers were used without being asked for")
	}
	kv, ok := known(with, "fp8-kv")
	if !ok || !kv.Evidence[0].Shared || !strings.HasPrefix(kv.Evidence[0].Cell, "a shared cell (H100, hybrid 27B model") || strings.Contains(kv.Reason, "theirs") {
		t.Fatalf("shared evidence must be named only by its numbers: %+v", kv)
	}
}

// sharedStore is a local store with another tenant's records reduced as the
// control plane's store reduces them.
type sharedStore struct {
	resultstore.Local
	shared []resultstore.Record
}

func (s sharedStore) SharedResultRecords(_ context.Context, _ string, filter resultstore.Filter) ([]resultstore.Record, error) {
	var out []resultstore.Record
	for _, record := range s.shared {
		if filter.Match(record) {
			out = append(out, record.NumbersOnly())
		}
	}
	return out, nil
}

func TestKnobsFromArgsReadsBothEngines(t *testing.T) {
	vllm := KnobsFromArgs("vllm", []string{"--max-num-seqs", "512", "--speculative-config", `{"method":"mtp","num_speculative_tokens":3}`, "--kv-cache-dtype=fp8", "--enforce-eager"})
	if vllm != (resultstore.Knobs{Engine: "vllm", Speculation: "mtp", Drafts: 3, KVCache: "fp8"}) {
		t.Fatalf("vllm: %+v", vllm)
	}
	sglang := KnobsFromArgs("sglang", []string{"--kv-cache-dtype", "fp8_e4m3", "--speculative-algorithm", "NEXTN", "--speculative-num-steps", "3"})
	if sglang != (resultstore.Knobs{Engine: "sglang", Speculation: "mtp", Drafts: 3, KVCache: "fp8"}) {
		t.Fatalf("sglang: %+v", sglang)
	}
	plain := KnobsFromArgs("vllm", []string{"--quantization", "fp8"})
	if plain != (resultstore.Knobs{Engine: "vllm", Speculation: "none", KVCache: "auto", Quantization: "fp8"}) {
		t.Fatalf("plain: %+v", plain)
	}
	if draft := KnobsFromArgs("vllm", []string{"--speculative-config", `{"model":"org/draft","num_speculative_tokens":4}`}); draft.Speculation != "draft-model" || draft.Drafts != 4 {
		t.Fatalf("draft model: %+v", draft)
	}
}

func TestMergeKeepsTheRulesFirstChoiceAndAddsMeasuredGains(t *testing.T) {
	ranking := Ranking{Ranked: []Ranked{
		{Name: "gain-small", Known: true, Estimate: 0.1, Lower: 0.02},
		{Name: "gain-large", Known: true, Estimate: 0.3, Lower: 0.2},
		{Name: "loss", Known: true, Estimate: -0.2, Lower: -0.3},
		{Name: "unsure", Known: true, Estimate: 0.05, Lower: -0.05},
		{Name: "never", Known: false, Lower: -0.5},
		{Name: "added-gain", Known: true, Estimate: 0.4, Lower: 0.3},
		{Name: "added-unsure", Known: true, Estimate: 0.01, Lower: -0.1},
	}}
	got := Merge(ranking, []string{"never", "loss", "gain-small", "unsure", "", "gain-large"}, []string{"added-unsure", "added-gain"})
	want := []string{"never", "added-gain", "gain-small", "unsure", "", "gain-large", "loss"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("merged %v, want %v", got, want)
	}
	// A first choice measured losing gives its place to the next.
	if got := Merge(ranking, []string{"loss", "unsure"}, nil); strings.Join(got, ",") != "unsure,loss" {
		t.Fatalf("merged %v", got)
	}
}

func TestKnobsOfChangeNamesOtherFlags(t *testing.T) {
	control := []string{"--max-num-seqs", "512", "--enforce-eager"}
	if got := KnobsOfChange("vllm", control, append(slices.Clone(control), "--kv-cache-dtype", "fp8")); got != (resultstore.Knobs{Engine: "vllm", Speculation: "none", KVCache: "fp8"}) {
		t.Fatalf("a knob-only change: %+v", got)
	}
	got := KnobsOfChange("vllm", control, []string{"--max-num-seqs", "48"})
	if got.Flags != "--enforce-eager= --max-num-seqs=48" {
		t.Fatalf("other flags: %q", got.Flags)
	}
	for kind, reading := range map[string][3]float64{"short": {40, 272, 0}, "output": {826, 1439, 0}, "long": {3906, 295, 0}, "prefix": {3858, 230, 0.75}} {
		if got := KindOf(reading[0], reading[1], reading[2]); got != kind {
			t.Errorf("%v read as %s, want %s", reading, got, kind)
		}
	}
}
