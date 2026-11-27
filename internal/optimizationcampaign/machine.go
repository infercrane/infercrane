package optimizationcampaign

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// Two servers of one setup do not run at one speed. The judge allows a fixed
// machineDifference for that; on short prompts at high load servers of one
// setup differed by 3% to 15%, and in each case the slower server's GPU was
// the least busy. So a campaign measures the spread where it can: a copy of
// the control, run as a candidate, differs from the control only by the
// machine, and a server whose GPU was markedly less busy than its peers' is
// named.

// lessBusyPoints is how many points of GPU busy share below the busiest
// server of the same setup at the same load mark a server as slower by its
// machine. On the map's crossover run every slowest server of a setup was at
// least this far below the busiest.
const lessBusyPoints = 5.0

// gpuBusyMetric is the DCGM gauge of the share of time the GPU was busy, in
// percent. A server's metrics carry it only when the server exports it.
const gpuBusyMetric = "DCGM_FI_DEV_GPU_UTIL"

// ServerBusy is how busy one server's GPU was while it was measured.
type ServerBusy struct {
	Setup, Load, Server string
	GPUBusyPercent      float64
}

// LessBusyServers names each server whose GPU was busy at least
// lessBusyPoints less than the busiest server of the same setup at the same
// load. A setup with one server at a load names nothing.
func LessBusyServers(servers []ServerBusy) []ServerBusy {
	busiest := map[string]float64{}
	count := map[string]int{}
	key := func(server ServerBusy) string { return server.Setup + "\x00" + server.Load }
	for _, server := range servers {
		count[key(server)]++
		busiest[key(server)] = math.Max(busiest[key(server)], server.GPUBusyPercent)
	}
	var out []ServerBusy
	for _, server := range servers {
		if count[key(server)] > 1 && busiest[key(server)]-server.GPUBusyPercent >= lessBusyPoints {
			out = append(out, server)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return key(out[i]) < key(out[j]) || key(out[i]) == key(out[j]) && out[i].Server < out[j].Server
	})
	return out
}

// runGPUBusy is the mean GPU busy share a run's server reported, when it
// exported the DCGM gauge.
func runGPUBusy(metrics *benchmark.ServerMetrics) (float64, bool) {
	if metrics == nil || !metrics.Collected {
		return 0, false
	}
	summary, ok := metrics.Metrics[gpuBusyMetric]
	if !ok {
		return 0, false
	}
	value, ok := summary.Stats["avg"]
	return value, ok && !math.IsNaN(value)
}

// MachineWarnings compares the GPU busy share of the two servers in each
// window of a measurement: the control's and the candidate's in the first
// window, and the fresh ones in the second. A server whose GPU was markedly
// less busy than the other's under the same load may have made the
// difference, and the warning names it. Nothing is said when either server
// did not report the share.
func MachineWarnings(measured *MeasuredEvidence) []string {
	if measured == nil {
		return nil
	}
	var out []string
	check := func(window string, runs []RunSummary) {
		sums, counts := map[string]float64{}, map[string]int{}
		for _, run := range runs {
			if busy, ok := runGPUBusy(run.ServerMetrics); ok {
				sums[run.Arm] += busy
				counts[run.Arm]++
			}
		}
		if counts["control"] == 0 || counts["candidate"] == 0 {
			return
		}
		control, candidate := sums["control"]/float64(counts["control"]), sums["candidate"]/float64(counts["candidate"])
		switch {
		case control-candidate >= lessBusyPoints:
			out = append(out, fmt.Sprintf("In the %s the candidate's server kept its GPU busy %.0f%% of the time against %.0f%% for the current setup's under the same load; a slower machine may explain part of a loss.", window, candidate, control))
		case candidate-control >= lessBusyPoints:
			out = append(out, fmt.Sprintf("In the %s the current setup's server kept its GPU busy %.0f%% of the time against %.0f%% for the candidate's under the same load; a slower machine may explain part of a gain.", window, control, candidate))
		}
	}
	check("first window", measured.Runs)
	if measured.Confirmation != nil {
		check("window on fresh servers", measured.Confirmation.Runs)
	}
	return out
}

// runningMetric is the engine's gauge of the requests it was running.
const runningMetric = "vllm:num_requests_running"

const (
	// slowerServerShare is how much longer each output token must take on
	// one server of a setup than on another, at about the same number of
	// running requests, before the report names the slower server.
	slowerServerShare = 0.10
	// sameRunningShare is how close the two servers' running requests must
	// be for their time per token to be compared: more requests in a batch
	// make each step slower on any machine.
	sameRunningShare = 0.10
	// slowRunShare is how much longer per token one run must take than the
	// same server's other runs in the window before it is named.
	slowRunShare = 0.25
)

// runSpeed is one run's p95 time per output token as the benchmark measured
// it, and the engine's mean running requests when the server reported them.
type runSpeed struct {
	tpot, running float64
	runningKnown  bool
}

func speedOf(run RunSummary) (runSpeed, bool) {
	if run.TPOTP95MS == nil || math.IsNaN(*run.TPOTP95MS) || *run.TPOTP95MS <= 0 {
		return runSpeed{}, false
	}
	speed := runSpeed{tpot: *run.TPOTP95MS}
	if run.ServerMetrics != nil && run.ServerMetrics.Collected {
		if summary, ok := run.ServerMetrics.Metrics[runningMetric]; ok {
			if value, ok := summary.Stats["avg"]; ok && !math.IsNaN(value) && value > 0 {
				speed.running, speed.runningKnown = value, true
			}
		}
	}
	return speed, true
}

// armSpeeds lists every run of arm in the window that has a time per token.
func armSpeeds(runs []RunSummary, arm string) []runSpeed {
	var out []runSpeed
	for _, run := range runs {
		if run.Arm != arm {
			continue
		}
		if speed, ok := speedOf(run); ok {
			out = append(out, speed)
		}
	}
	return out
}

// serverSpeed is a server's typical run: the median time per token and the
// median running requests, so one slow run (named on its own) does not make
// the server look slow. Running is known only when every run reported it.
func serverSpeed(speeds []runSpeed) (runSpeed, bool) {
	if len(speeds) == 0 {
		return runSpeed{}, false
	}
	tpots, running := make([]float64, 0, len(speeds)), make([]float64, 0, len(speeds))
	out := runSpeed{runningKnown: true}
	for _, speed := range speeds {
		tpots, running = append(tpots, speed.tpot), append(running, speed.running)
		out.runningKnown = out.runningKnown && speed.runningKnown
	}
	out.tpot, out.running = median(tpots), median(running)
	return out, true
}

func median(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[middle-1] + sorted[middle]) / 2
	}
	return sorted[middle]
}

// ServerSpeedWarnings names, from the measured runs alone, a server that
// served each output token markedly slower than another server of the same
// setup at about the same number of running requests: each arm's server in
// the first window against its server in the window on fresh servers, and a
// run that took much longer per token than the same server's other runs in
// its window. Time per token is the benchmark's p95, so it includes the
// network path to the server; the running requests are the engine's own
// gauge. It needs no GPU telemetry, and says nothing when the engine did not
// report its running requests. label names the setup the measurement
// belongs to.
func ServerSpeedWarnings(label string, measured *MeasuredEvidence) []string {
	if measured == nil {
		return nil
	}
	names := map[string]string{"candidate": "“" + label + "”", "control": "the current setup"}
	var out []string
	for _, arm := range []string{"candidate", "control"} {
		check := func(window string, runs []RunSummary) {
			speeds := armSpeeds(runs, arm)
			if len(speeds) < 3 {
				return
			}
			for index, speed := range speeds {
				others := make([]float64, 0, len(speeds)-1)
				running, runningKnown := make([]float64, 0, len(speeds)-1), speed.runningKnown
				for other, value := range speeds {
					if other != index {
						others = append(others, value.tpot)
						running = append(running, value.running)
						runningKnown = runningKnown && value.runningKnown
					}
				}
				typical := median(others)
				if speed.tpot < (1+slowRunShare)*typical {
					continue
				}
				detail := fmt.Sprintf("p95 %.1f ms against %.1f ms", speed.tpot, typical)
				if runningKnown {
					detail += fmt.Sprintf(", with %.0f requests running against %.0f", speed.running, median(running))
				}
				out = append(out, fmt.Sprintf("In the measurement of “%s”, %s on %s took %.0f%% longer per output token in its run %d of %d there than in its other runs (%s); a server that is slow in some runs and not others makes its window drift.",
					label, names[arm], window, 100*(speed.tpot/typical-1), index+1, len(speeds), detail))
			}
		}
		check("its first server", measured.Runs)
		if measured.Confirmation == nil {
			continue
		}
		check("its fresh server", measured.Confirmation.Runs)
		first, firstOK := serverSpeed(armSpeeds(measured.Runs, arm))
		fresh, freshOK := serverSpeed(armSpeeds(measured.Confirmation.Runs, arm))
		if !firstOK || !freshOK || !first.runningKnown || !fresh.runningKnown || math.Abs(fresh.running/first.running-1) > sameRunningShare {
			continue
		}
		slower, faster, which := fresh, first, "fresh"
		if first.tpot > fresh.tpot {
			slower, faster, which = first, fresh, "first"
		}
		if slower.tpot < (1+slowerServerShare)*faster.tpot {
			continue
		}
		out = append(out, fmt.Sprintf("In the measurement of “%s”, %s took %.0f%% longer per output token on its %s server than on its other one (p95 %.1f ms against %.1f ms) with about as many requests running (%.0f against %.0f): two servers of one setup differed by more than the %.2g%% the judge allows for machines.",
			label, names[arm], 100*(slower.tpot/faster.tpot-1), which, slower.tpot, faster.tpot, slower.running, faster.running, 100*machineDifference))
	}
	return out
}

// WidenForMachines widens a paired comparison's interval when two machines
// were measured further apart than the judge's fixed allowance: the
// statistical part of the interval stays, and the machine part uses the
// measured difference. The decision is taken again on the wider interval.
func WidenForMachines(comparison MetricComparison, difference float64) MetricComparison {
	difference = math.Abs(difference)
	if comparison.Method != ComparisonPaired || comparison.Confidence <= 0 || comparison.Confidence >= 1 || difference <= comparison.MachineDifference {
		return comparison
	}
	quantile := math.Sqrt2 * math.Erfinv(comparison.Confidence)
	half := math.Log1p(comparison.NoiseFloor)
	statistical := math.Sqrt(math.Max(0, half*half-math.Pow(quantile*comparison.MachineDifference, 2)))
	half = math.Hypot(statistical, quantile*difference)
	mean := math.Log1p(comparison.Delta)
	comparison.NoiseFloor, comparison.MachineDifference = math.Expm1(half), difference
	comparison.Decision = DecisionNoMeasurableDifference
	switch {
	case mean-half > 0:
		comparison.Decision = DecisionRegressed
		if comparison.HigherIsBetter {
			comparison.Decision = DecisionImproved
		}
	case mean+half < 0:
		comparison.Decision = DecisionImproved
		if comparison.HigherIsBetter {
			comparison.Decision = DecisionRegressed
		}
	}
	return comparison
}

// copyOfControl reports whether a candidate is the control again: the same
// draft except for the sampling seed, which gives it its own identity and
// does not change its speed.
func copyOfControl(control, candidate optimizer.DeploymentDraft) bool {
	withoutSeed := func(args []string) []string {
		out := make([]string, 0, len(args))
		for index := 0; index < len(args); index++ {
			if args[index] == "--seed" {
				index++
				continue
			}
			if strings.HasPrefix(args[index], "--seed=") {
				continue
			}
			out = append(out, args[index])
		}
		return out
	}
	if control.Model.ID != candidate.Model.ID || control.Model.Revision != candidate.Model.Revision || control.Runtime.Engine != candidate.Runtime.Engine || control.Runtime.Version != candidate.Runtime.Version ||
		control.Resources.GPU != candidate.Resources.GPU || control.Resources.GPUCount != candidate.Resources.GPUCount || fmt.Sprint(control.Runtime.Env) != fmt.Sprint(candidate.Runtime.Env) {
		return false
	}
	before, after := withoutSeed(control.Runtime.Args), withoutSeed(candidate.Runtime.Args)
	return strings.Join(before, "\x00") == strings.Join(after, "\x00") && len(before) < len(candidate.Runtime.Args)
}
