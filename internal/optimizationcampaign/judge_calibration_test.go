package optimizationcampaign

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/benchmark"
)

// judgeScenario is one measurement condition for the calibration test.
// Noise is the run-to-run coefficient of variation. HostBias is the standard
// deviation of a fixed speed difference between the candidate's machine and
// the control's, drawn once per candidate window, because the arms run on
// separate servers. Trend is a slow change per run that both arms share.
type judgeScenario struct {
	Noise, HostBias, Trend float64
}

// judgeWindow measures one candidate exactly as a campaign records it: the
// session's run order (control, then candidate and control in turn), its
// checkpoint, and finish, which applies the broken-window check and the
// paired comparison. Only AIPerf is replaced, by draws from the scenario.
func judgeWindow(rng *rand.Rand, scenario judgeScenario, effect float64, comparisons int) string {
	bias := rng.NormFloat64() * scenario.HostBias
	step := 0
	run := func(candidate bool) benchmark.Result {
		value := 1000 * (1 + scenario.Trend*float64(step)) * (1 + rng.NormFloat64()*scenario.Noise)
		if candidate {
			value *= (1 + effect) * (1 + bias)
		}
		step++
		return benchmark.Result{Requests: 200, Succeeded: 200, OutputTokenThroughput: &value}
	}
	state := MeasurementCheckpoint{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleBracketed, CandidateID: "control"}, Comparisons: comparisons}
	state.ControlRuns = append(state.ControlRuns, run(false))
	for range measurementRunsPerArm {
		state.CandidateRuns = append(state.CandidateRuns, run(true))
		state.ControlRuns = append(state.ControlRuns, run(false))
	}
	state.Segments = []MeasurementSegment{{ControlRuns: len(state.ControlRuns), CandidateRuns: len(state.CandidateRuns), Ended: SegmentCompleted}}
	session := measurementSession{state: &state, now: time.Now}
	evidence := session.finish().evidence
	if evidence.Control.Role != ControlRoleBracketed {
		return "invalid"
	}
	comparison, found := evidence.Comparison["output_token_throughput"]
	if !found {
		return "invalid"
	}
	return comparison.Decision
}

// judgeRates runs trials campaigns whose candidates have the given true
// gains (zero is a copy of the control). A campaign "wins" when any
// candidate is judged improved, which is when the control gate replaces the
// control. It also returns the share of windows found broken.
func judgeRates(seed uint64, trials int, scenario judgeScenario, effects []float64) (win, broken float64) {
	rng := rand.New(rand.NewPCG(seed, 0x1f2e3d4c))
	wins, brokenWindows := 0, 0
	for range trials {
		won := false
		for _, effect := range effects {
			switch judgeWindow(rng, scenario, effect, len(effects)) {
			case DecisionImproved:
				won = true
			case "invalid":
				brokenWindows++
			}
		}
		if won {
			wins++
		}
	}
	return float64(wins) / float64(trials), float64(brokenWindows) / float64(trials*len(effects))
}

// TestJudgeCalibration measures the campaign judge itself: how often it
// declares a win when nothing changed (false wins), and how often it finds a
// real gain (power). Noise levels follow real runs: the H100 campaign of
// 2026-11-20 had a 1.2% noise floor and 0.65% control drift; the RTX 4090
// run of 2026-11-17 had about 7% noise. Run with -v for the table.
//
// Same-machine false wins are bounded by design (familyAlpha, split over
// the candidates). Machine-to-machine differences are not: pairing in time
// cannot remove a fixed difference between two servers, and the table shows
// how much that matters until the winner is re-measured on a fresh server.
func TestJudgeCalibration(t *testing.T) {
	const trials = 2000
	rows := []struct {
		name     string
		scenario judgeScenario
	}{
		{"same machine, 1% noise", judgeScenario{Noise: 0.01}},
		{"same machine, 2% noise", judgeScenario{Noise: 0.02}},
		{"same machine, 4% noise", judgeScenario{Noise: 0.04}},
		{"2% noise, 0.5% drift per run", judgeScenario{Noise: 0.02, Trend: 0.005}},
		{"2% noise, machines differ 1%", judgeScenario{Noise: 0.02, HostBias: 0.01}},
		{"2% noise, machines differ 2%", judgeScenario{Noise: 0.02, HostBias: 0.02}},
		{"2% noise, machines differ 4%", judgeScenario{Noise: 0.02, HostBias: 0.04}},
	}
	type result struct{ aa1, aa4, p3, p5, p10, broken float64 }
	results := map[string]result{}
	var table strings.Builder
	fmt.Fprintf(&table, "\n%-31s %8s %8s %7s %7s %7s %8s\n", "condition", "AA x1", "AA x4", "+3%", "+5%", "+10%", "broken")
	for index, row := range rows {
		seed := uint64(index+1) * 1000
		var r result
		r.aa1, r.broken = judgeRates(seed, trials, row.scenario, []float64{0})
		r.aa4, _ = judgeRates(seed+1, trials, row.scenario, []float64{0, 0, 0, 0})
		r.p3, _ = judgeRates(seed+2, trials, row.scenario, []float64{0.03})
		r.p5, _ = judgeRates(seed+3, trials, row.scenario, []float64{0.05})
		r.p10, _ = judgeRates(seed+4, trials, row.scenario, []float64{0.10})
		results[row.name] = r
		fmt.Fprintf(&table, "%-31s %7.1f%% %7.1f%% %6.1f%% %6.1f%% %6.1f%% %7.1f%%\n", row.name, r.aa1*100, r.aa4*100, r.p3*100, r.p5*100, r.p10*100, r.broken*100)
	}
	t.Log(table.String())

	// One-sided wins are at most familyAlpha/2 by construction; the margin
	// covers sampling error over 2,000 trials.
	for _, name := range []string{"same machine, 1% noise", "same machine, 2% noise", "same machine, 4% noise", "2% noise, 0.5% drift per run"} {
		if r := results[name]; r.aa1 > 0.035 || r.aa4 > 0.035 {
			t.Errorf("%s: false wins %.1f%% (1 candidate), %.1f%% (4 candidates), want at most 3.5%%", name, r.aa1*100, r.aa4*100)
		}
	}
	if r := results["same machine, 2% noise"]; r.p10 < 0.95 || r.p5 < 0.5 {
		t.Errorf("same machine, 2%% noise: found +5%% %.1f%%, +10%% %.1f%% of the time, want at least 50%% and 95%%", r.p5*100, r.p10*100)
	}
	if r := results["same machine, 2% noise"]; r.broken > 0.01 {
		t.Errorf("same machine, 2%% noise: %.1f%% of windows broken, want at most 1%%", r.broken*100)
	}
}

func TestJudgeWindowUsesTheCampaignRules(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	if got := judgeWindow(rng, judgeScenario{Noise: 0.005}, 0.20, 1); got != DecisionImproved {
		t.Fatalf("a 20%% gain at 0.5%% noise was judged %q", got)
	}
	if got := judgeWindow(rng, judgeScenario{Noise: 0.005}, -0.20, 1); got != DecisionRegressed {
		t.Fatalf("a 20%% loss at 0.5%% noise was judged %q", got)
	}
	if got := judgeWindow(rng, judgeScenario{Noise: 0.30}, 0, 1); got != "invalid" {
		t.Fatalf("30%% noise should break the window, got %q", got)
	}
}

// judgeConfirmed measures one candidate the way a campaign now decides it:
// a first window, and when that gains, a second window after both servers
// were retired and provisioned again. Each machine gets its own speed,
// drawn so that two machines differ by about scenario.HostBias. The decision
// is the ranking's own confirmedDecision on what finish recorded.
func judgeConfirmed(rng *rand.Rand, scenario judgeScenario, effect float64, comparisons int) string {
	machine := func() float64 { return 1 + rng.NormFloat64()*scenario.HostBias/math.Sqrt2 }
	state := MeasurementCheckpoint{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleBracketed, CandidateID: "control"}, Comparisons: comparisons}
	window := func(controlSpeed, candidateSpeed float64, confirmation bool) {
		step := 0
		run := func(speed float64) benchmark.Result {
			value := 1000 * speed * (1 + scenario.Trend*float64(step)) * (1 + rng.NormFloat64()*scenario.Noise)
			step++
			return benchmark.Result{Requests: 200, Succeeded: 200, OutputTokenThroughput: &value}
		}
		state.ControlRuns = append(state.ControlRuns, run(controlSpeed))
		for range measurementRunsPerArm {
			state.CandidateRuns = append(state.CandidateRuns, run(candidateSpeed*(1+effect)))
			state.ControlRuns = append(state.ControlRuns, run(controlSpeed))
		}
		state.Segments = append(state.Segments, MeasurementSegment{ControlRuns: measurementRunsPerArm + 1, CandidateRuns: measurementRunsPerArm, Ended: SegmentCompleted, Confirmation: confirmation})
	}
	window(machine(), machine(), false)
	session := measurementSession{state: &state, now: time.Now}
	first := session.finish().evidence
	comparison, found := first.Comparison["output_token_throughput"]
	if first.Control.Role != ControlRoleBracketed || !found {
		return "invalid"
	}
	if comparison.Decision == DecisionImproved {
		state.Confirmation = &ConfirmationState{Phase: ConfirmMeasure}
		state.Interruptions = []MeasurementInterruption{{Kind: RunKindConfirmation, Arm: ArmCandidate, Recovery: RecoveryMachineRetired}, {Kind: RunKindConfirmation, Arm: ArmControl, Recovery: RecoveryMachineRetired}}
		window(machine(), machine(), true)
	}
	evidence := session.finish().evidence
	decision, _, _ := confirmedDecision(&evidence, comparison, "output_token_throughput", "throughput", false, 1)
	return decision
}

func confirmedRate(seed uint64, trials int, scenario judgeScenario, effects []float64) float64 {
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	wins := 0
	for range trials {
		for _, effect := range effects {
			if judgeConfirmed(rng, scenario, effect, len(effects)) == DecisionImproved {
				wins++
				break
			}
		}
	}
	return float64(wins) / float64(trials)
}

// TestJudgeCalibrationWithConfirmation measures the full rule: a gain counts
// only when it holds again on fresh servers. Real GPU runs are quiet (the
// H100 control of 2026-11-20 varied about 0.3% between runs), so machine
// differences, not run noise, decide false wins there; the 0.5% rows show it.
// "First window" is the decision without the confirmation.
func TestJudgeCalibrationWithConfirmation(t *testing.T) {
	const trials = 2000
	rows := []struct {
		name     string
		scenario judgeScenario
	}{
		{"0.5% noise, same machine", judgeScenario{Noise: 0.005}},
		{"0.5% noise, machines differ 1%", judgeScenario{Noise: 0.005, HostBias: 0.01}},
		{"0.5% noise, machines differ 2%", judgeScenario{Noise: 0.005, HostBias: 0.02}},
		{"2% noise, same machine", judgeScenario{Noise: 0.02}},
		{"2% noise, machines differ 2%", judgeScenario{Noise: 0.02, HostBias: 0.02}},
	}
	type result struct{ first4, aa1, aa4, p3, p5, p10 float64 }
	results := map[string]result{}
	var table strings.Builder
	fmt.Fprintf(&table, "\n%-31s %13s %8s %8s %7s %7s %7s\n", "condition", "first window", "AA x1", "AA x4", "+3%", "+5%", "+10%")
	for index, row := range rows {
		seed := uint64(index+1) * 7000
		var r result
		r.first4, _ = judgeRates(seed, trials, judgeScenario{Noise: row.scenario.Noise, HostBias: row.scenario.HostBias}, []float64{0, 0, 0, 0})
		r.aa1 = confirmedRate(seed+1, trials, row.scenario, []float64{0})
		r.aa4 = confirmedRate(seed+2, trials, row.scenario, []float64{0, 0, 0, 0})
		r.p3 = confirmedRate(seed+3, trials, row.scenario, []float64{0.03})
		r.p5 = confirmedRate(seed+4, trials, row.scenario, []float64{0.05})
		r.p10 = confirmedRate(seed+5, trials, row.scenario, []float64{0.10})
		results[row.name] = r
		fmt.Fprintf(&table, "%-31s %12.1f%% %7.1f%% %7.1f%% %6.1f%% %6.1f%% %6.1f%%\n", row.name, r.first4*100, r.aa1*100, r.aa4*100, r.p3*100, r.p5*100, r.p10*100)
	}
	t.Log(table.String())
	if r := results["0.5% noise, same machine"]; r.aa4 > 0.01 || r.p3 < 0.9 {
		t.Errorf("same machine: false wins %.1f%%, +3%% found %.1f%%", r.aa4*100, r.p3*100)
	}
	if r := results["0.5% noise, machines differ 1%"]; r.aa4 > 0.07 || r.aa4 >= r.first4/3 || r.p5 < 0.95 {
		t.Errorf("machines differ 1%%: false wins %.1f%% (first window %.1f%%), +5%% found %.1f%%", r.aa4*100, r.first4*100, r.p5*100)
	}
	if r := results["2% noise, same machine"]; r.aa4 > 0.01 || r.p10 < 0.95 {
		t.Errorf("2%% noise: false wins %.1f%%, +10%% found %.1f%%", r.aa4*100, r.p10*100)
	}
}
