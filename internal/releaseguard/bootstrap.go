package releaseguard

import (
	"errors"
	"math"
	"math/rand"
	"sort"
)

const bootstrapIterations = 10_000

type BootstrapComparison struct {
	Status                    string   `json:"status"`
	Seed                      int64    `json:"seed"`
	Alpha                     float64  `json:"alpha"`
	Iterations                int      `json:"iterations"`
	ActiveSamples             int      `json:"active_samples"`
	CandidateSamples          int      `json:"candidate_samples"`
	MinimumSamples            int      `json:"minimum_samples"`
	ObservedRegressionPercent *float64 `json:"observed_regression_percent,omitempty"`
	IntervalLowerPercent      *float64 `json:"interval_lower_percent,omitempty"`
	IntervalUpperPercent      *float64 `json:"interval_upper_percent,omitempty"`
	LimitPercent              float64  `json:"limit_percent"`
}

// PairedBootstrap computes a deterministic confidence interval over the
// relative candidate quality regression. Pairing identity is validated by the
// signed evidence layer; this function deliberately contains no I/O or global
// randomness.
func PairedBootstrap(active, candidate []float64, alpha float64, minimum int, seed int64, limit float64) (BootstrapComparison, error) {
	result := BootstrapComparison{Status: "insufficient", Seed: seed, Alpha: alpha, Iterations: bootstrapIterations, ActiveSamples: len(active), CandidateSamples: len(candidate), MinimumSamples: minimum, LimitPercent: limit}
	if alpha <= 0 || alpha >= .5 || minimum < 2 || limit < 0 || limit > 100 {
		return result, errors.New("invalid paired-bootstrap policy")
	}
	if len(active) != len(candidate) || len(active) < minimum {
		return result, nil
	}
	for i := range active {
		if !finiteUnit(active[i]) || !finiteUnit(candidate[i]) {
			return result, errors.New("paired-bootstrap scores must be finite values between zero and one")
		}
	}
	activeMean, candidateMean := mean(active), mean(candidate)
	if activeMean <= 0 {
		return result, nil
	}
	observed := (activeMean - candidateMean) / activeMean * 100
	result.ObservedRegressionPercent = &observed
	rng := rand.New(rand.NewSource(seed)) // #nosec G404 -- deterministic statistical resampling, not security.
	distribution := make([]float64, bootstrapIterations)
	for iteration := range distribution {
		var activeTotal, candidateTotal float64
		for range active {
			index := rng.Intn(len(active))
			activeTotal += active[index]
			candidateTotal += candidate[index]
		}
		bootstrapActive := activeTotal / float64(len(active))
		if bootstrapActive <= 0 {
			return result, nil
		}
		distribution[iteration] = (bootstrapActive - candidateTotal/float64(len(candidate))) / bootstrapActive * 100
	}
	sort.Float64s(distribution)
	lower, upper := percentile(distribution, alpha/2), percentile(distribution, 1-alpha/2)
	result.IntervalLowerPercent, result.IntervalUpperPercent = &lower, &upper
	switch {
	case lower > limit:
		result.Status = "reject"
	case upper <= limit:
		result.Status = "accept"
	default:
		result.Status = "inconclusive"
	}
	return result, nil
}

func finiteUnit(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func mean(values []float64) float64 {
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func percentile(sorted []float64, probability float64) float64 {
	index := int(math.Floor(probability * float64(len(sorted)-1)))
	return sorted[max(0, min(len(sorted)-1, index))]
}
