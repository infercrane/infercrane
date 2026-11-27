package kerneltuning

import (
	"errors"
	"fmt"
	"slices"
	"sort"
)

// Load is one band of traffic a set is tuned for, in the terms that decide
// how many tokens one engine step carries.
type Load struct {
	// Running is how many requests the server holds at once in the band.
	Running int `json:"running"`
	// DraftTokens is the tokens each request drafts per step when the setup
	// speculates; a decode step then carries Running × (1 + DraftTokens).
	DraftTokens int `json:"draft_tokens,omitempty"`
	// BatchedTokens is the engine's per-step token budget, which bounds a
	// prefill step.
	BatchedTokens int `json:"batched_tokens,omitempty"`
}

// The step sizes a tuned file names: vLLM's own tuned files use this grid,
// and its CUDA graphs are captured at sizes between its points.
var (
	decodeGrid  = []int{1, 2, 4, 8, 16, 24, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 1536}
	prefillGrid = []int{2048, 3072, 4096, 6144, 8192, 12288, 16384, 24576, 32768}
)

// Plan is the step sizes one file holds. Tune are the sizes the tuner
// measures; Default are sizes that keep the engine's own default: one on each
// side of every step size where the default heuristic changes its tiles, so
// that for any step the nearest key lies on the same side as the step and an
// untuned key reproduces exactly what the engine would have used.
type Plan struct {
	Tune    []int `json:"tune"`
	Default []int `json:"default"`
}

// Keys plans the step sizes for a shape on an engine version from the loads
// the workload runs at. Only the decode sizes the loads reach, and the
// prefill sizes up to the largest step budget, are tuned.
func Keys(runtime, version string, shape FusedMoEShape, loads []Load) (Plan, error) {
	engine, ok := lookupEngine(runtime, version)
	if !ok {
		return Plan{}, fmt.Errorf("kernel tuning is not reviewed for %s %s", runtime, version)
	}
	boundaries, ok := engine.boundaries[shape.dtypeClass()]
	if !ok {
		return Plan{}, fmt.Errorf("kernel tuning on %s %s is reviewed only for %v", runtime, version, reviewedClasses(engine))
	}
	if len(loads) == 0 {
		return Plan{}, errors.New("kernel tuning needs at least one load band")
	}
	decode, batched := 0, 0
	for _, load := range loads {
		if load.Running < 1 || load.Running > 4096 || load.DraftTokens < 0 || load.DraftTokens > 16 || load.BatchedTokens < 0 || load.BatchedTokens > MaxKey {
			return Plan{}, errors.New("a load band needs 1 to 4096 running requests, 0 to 16 draft tokens and a step budget of at most 1,048,576 tokens")
		}
		decode = max(decode, load.Running*(1+load.DraftTokens))
		batched = max(batched, load.BatchedTokens)
	}
	tune := map[int]bool{}
	for _, size := range decodeGrid {
		tune[size] = true
		if size >= decode {
			break
		}
	}
	for _, size := range prefillGrid {
		if size > batched {
			break
		}
		tune[size] = true
	}
	if batched > 0 {
		tune[batched] = true
	}
	defaults := map[int]bool{}
	for _, edge := range boundaries {
		for _, size := range []int{edge, edge + 1} {
			if !tune[size] {
				defaults[size] = true
			}
		}
	}
	plan := Plan{Tune: sortedKeys(tune), Default: sortedKeys(defaults)}
	if len(plan.Tune)+len(plan.Default) > MaxKeys {
		return Plan{}, fmt.Errorf("the loads need %d step sizes, more than a file holds (%d)", len(plan.Tune)+len(plan.Default), MaxKeys)
	}
	return plan, nil
}

// NearestKey is the entry vLLM uses for a step of tokens: the key with the
// smallest distance to it, the smaller key on a tie (min over the keys in
// their file order, which for the files Keys plans is ascending).
func NearestKey(keys []int, tokens int) int {
	ordered := slices.Clone(keys)
	sort.Ints(ordered)
	best := ordered[0]
	for _, key := range ordered[1:] {
		if abs(key-tokens) < abs(best-tokens) {
			best = key
		}
	}
	return best
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func sortedKeys(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Ints(out)
	return out
}

// SameDefaultRegion reports whether two step sizes get the same default
// tiles from the engine version's heuristic for the shape.
func SameDefaultRegion(runtime, version string, shape FusedMoEShape, a, b int) bool {
	engine, ok := lookupEngine(runtime, version)
	if !ok {
		return false
	}
	region := func(tokens int) int {
		count := 0
		for _, edge := range engine.boundaries[shape.dtypeClass()] {
			if tokens > edge {
				count++
			}
		}
		return count
	}
	return region(a) == region(b)
}
