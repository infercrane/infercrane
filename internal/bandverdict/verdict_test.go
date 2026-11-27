package bandverdict

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func run(output, useful float64) Measure { return Measure{Output: output, UsefulOutput: &useful} }

func runs(values ...float64) []Measure {
	out := make([]Measure, 0, len(values))
	for _, value := range values {
		out = append(out, run(value*1.1, value))
	}
	return out
}

// screen is two bands of short chat; values are useful output tokens per
// second, made up for the test.
func screen() Screen {
	return Screen{SchemaVersion: SchemaV1, LimitMS: 1000, Current: "defaults", Copy: "defaults-copy",
		Rules: Rules{MaxCurrentSpread: 0.05, MinNoiseFloor: 0.01},
		Bands: []Band{
			{Kind: "short", Load: 64, CurrentRuns: runs(1000, 1020), Setups: []Setup{
				{ID: "fp8-weights", Runs: runs(1150)}, {ID: "fp8-weights-kv", Runs: runs(1300)}, {ID: "defaults-copy", Runs: runs(1012)}}},
			{Kind: "short", Load: 16, CurrentRuns: runs(400, 404), Setups: []Setup{
				{ID: "fp8-weights", Runs: runs(470)}, {ID: "fp8-weights-kv", Runs: runs(480)}, {ID: "defaults-copy", Runs: runs(401)}}},
		}}
}

func TestOneSetupWinsEveryBand(t *testing.T) {
	verdict, err := Decide(screen())
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Summary != SummaryOneSetup || verdict.Winners["short/16"] != "fp8-weights-kv" || verdict.Winners["short/64"] != "fp8-weights-kv" || verdict.Metric != MetricUsefulOutput {
		t.Fatalf("verdict = %+v", verdict)
	}
	low := verdict.Bands[0]
	if low.Key != "short/16" || !low.RepeatsSteady || low.CopyInsideNoise == nil || !*low.CopyInsideNoise || low.NoiseFloor < low.Spread || low.NoiseFloor < 0.01 {
		t.Fatalf("checks = %+v", low)
	}
	for _, setup := range low.Setups {
		if setup.ID == "defaults-copy" && setup.Ahead {
			t.Fatal("the copy can never win")
		}
	}
}

func TestDifferentBandsCanHaveDifferentWinners(t *testing.T) {
	s := screen()
	s.Bands[0].Setups[1].Runs = runs(1010) // the pair gains nothing at 64
	verdict, _ := Decide(s)
	if verdict.Summary != SummaryPerBand || verdict.Winners["short/64"] != "fp8-weights" || verdict.Winners["short/16"] != "fp8-weights-kv" {
		t.Fatalf("verdict = %+v", verdict)
	}
}

func TestAnUnsteadyMachineOrAWinningCopyLeavesTheBandUndecided(t *testing.T) {
	unsteady := screen()
	unsteady.Bands[0].CurrentRuns = runs(1000, 1100)
	verdict, _ := Decide(unsteady)
	if verdict.Summary != SummaryIncomplete || verdict.Bands[1].Decision != DecisionUnknown || !strings.Contains(verdict.Bands[1].Reason, "not steady") || verdict.Bands[1].RepeatsSteady {
		t.Fatalf("unsteady: %+v", verdict.Bands[1])
	}
	copied := screen()
	copied.Bands[1].Setups[2].Runs = runs(430)
	verdict, _ = Decide(copied)
	if verdict.Bands[0].Decision != DecisionNoDecision || *verdict.Bands[0].CopyInsideNoise || verdict.Summary != SummaryIncomplete {
		t.Fatalf("copy outside the noise: %+v", verdict.Bands[0])
	}
	single := screen()
	single.Bands[0].CurrentRuns = runs(1000)
	if verdict, _ = Decide(single); verdict.Bands[1].Decision != DecisionUnknown {
		t.Fatalf("one current run: %+v", verdict.Bands[1])
	}
}

func TestAHeldBackSetupNeverWinsAndNoGainKeepsTheCurrentSetup(t *testing.T) {
	s := screen()
	s.Bands[1].Setups[1].HeldBack = "task set worse"
	verdict, _ := Decide(s)
	if verdict.Winners["short/16"] != "fp8-weights" {
		t.Fatalf("held back: %+v", verdict.Bands[0])
	}
	flat := screen()
	for index := range flat.Bands {
		for setup := range flat.Bands[index].Setups {
			flat.Bands[index].Setups[setup].Runs = flat.Bands[index].CurrentRuns[:1]
		}
	}
	if verdict, _ = Decide(flat); verdict.Summary != SummaryOneSetup || verdict.Winners["short/16"] != DecisionKeep {
		t.Fatalf("flat: %+v", verdict)
	}
}

func TestUsefulOutputIsTheDefaultAndMustBeThere(t *testing.T) {
	s := screen()
	s.Bands[0].CurrentRuns = []Measure{{Output: 900}, {Output: 910}}
	verdict, _ := Decide(s)
	if verdict.Bands[1].Decision != DecisionUnknown || !strings.Contains(verdict.Bands[1].Reason, "useful output") {
		t.Fatalf("no useful output: %+v", verdict.Bands[1])
	}
	s.Metric = MetricOutput
	if verdict, _ = Decide(s); verdict.Bands[1].Decision == DecisionUnknown {
		t.Fatalf("judged on all output: %+v", verdict.Bands[1])
	}
}

func TestTheSchemaAndDecodeAgree(t *testing.T) {
	body, err := os.ReadFile("../../schemas/band-screen-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var parsed jsonschema.Schema
	if err = json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	resolved, err := parsed.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(screen())
	var instance any
	_ = json.Unmarshal(encoded, &instance)
	if err = resolved.Validate(instance); err != nil {
		t.Fatalf("the schema refused a valid screen: %v", err)
	}
	if _, err = Decode(encoded); err != nil {
		t.Fatalf("Decode refused a valid screen: %v", err)
	}
	for name, broken := range map[string]func(*Screen){
		"no current setup":       func(s *Screen) { s.Current = "" },
		"a spread of 0":          func(s *Screen) { s.Rules.MaxCurrentSpread = 0 },
		"a band twice":           func(s *Screen) { s.Bands[1] = s.Bands[0] },
		"a load of 0":            func(s *Screen) { s.Bands[0].Load = 0 },
		"a negative run":         func(s *Screen) { s.Bands[0].CurrentRuns[0].Output = -1 },
		"another metric":         func(s *Screen) { s.Metric = "latency" },
		"the current as a setup": func(s *Screen) { s.Bands[0].Setups[0].ID = "defaults" },
	} {
		s := screen()
		broken(&s)
		encoded, _ := json.Marshal(s)
		if _, err := Decode(encoded); err == nil {
			t.Errorf("%s: Decode accepted it", name)
		}
		var instance any
		_ = json.Unmarshal(encoded, &instance)
		if name != "a band twice" && name != "the current as a setup" && resolved.Validate(instance) == nil {
			// Uniqueness across objects is checked in Go only.
			t.Errorf("%s: the schema accepted it", name)
		}
	}
}

// The band table is what a per-server switch serves from: each band's largest
// load and its setup, neighbours with one setup merged, the last band open.
func TestTheVerdictGivesTheBandTableASwitchServesFrom(t *testing.T) {
	s := screen()
	s.Bands[0].Setups[1].Runs = runs(1010) // at 64 FP8 weights alone wins
	verdict, _ := Decide(s)
	if len(verdict.SwitchBands) != 2 || verdict.SwitchBands[0] != (SwitchBand{UpTo: 16, Engine: "fp8-weights-kv"}) || verdict.SwitchBands[1] != (SwitchBand{Engine: "fp8-weights"}) {
		t.Fatalf("table = %+v", verdict.SwitchBands)
	}
	same, _ := Decide(screen())
	if len(same.SwitchBands) != 1 || same.SwitchBands[0] != (SwitchBand{Engine: "fp8-weights-kv"}) {
		t.Fatalf("one setup everywhere: %+v", same.SwitchBands)
	}
	flat := screen()
	for index := range flat.Bands {
		for setup := range flat.Bands[index].Setups {
			flat.Bands[index].Setups[setup].Runs = flat.Bands[index].CurrentRuns[:1]
		}
	}
	if kept, _ := Decide(flat); len(kept.SwitchBands) != 1 || kept.SwitchBands[0].Engine != "defaults" {
		t.Fatalf("kept: %+v", kept.SwitchBands)
	}
	unsteady := screen()
	unsteady.Bands[0].CurrentRuns = runs(1000, 1100)
	if incomplete, _ := Decide(unsteady); incomplete.SwitchBands != nil {
		t.Fatalf("an incomplete verdict gave a table: %+v", incomplete.SwitchBands)
	}
	mixed := screen()
	mixed.Bands[1].Kind = "long"
	if verdict, _ := Decide(mixed); verdict.SwitchBands != nil {
		t.Fatalf("two kinds gave one table: %+v", verdict.SwitchBands)
	}
}
