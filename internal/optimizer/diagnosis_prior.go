package optimizer

import (
	"math"
	"slices"
	"sort"

	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/resultprior"
	"github.com/infercrane/infercrane/internal/resultstore"
)

// PriorFunc ranks candidate setups by what similar measured cells showed
// (internal/resultprior). The control plane binds it to the caller's results
// store; nil plans from the rules alone.
type PriorFunc func(resultprior.Query) (resultprior.Ranking, error)

// priorRule names the results store's prior in a choice's rules.
const priorRule = "results_prior.similar_cells"

// maxPriorAdded bounds the techniques the prior adds to the rules' choices.
const maxPriorAdded = 2

// applyPrior lets measured cells adjust the rules' choices
// (resultprior.Merge): the rules' first choice keeps its place unless its
// setup lost in similar cells; up to two techniques the rules did not choose,
// whose setups gained in similar cells (lower bound above zero), come right
// after it with the prior's reason; a choice whose setup lost there is
// measured last. Priorities and reasons the rules gave are kept; only the
// order changes, through priorOrder. Without a prior, a starting setup, or a reading of the load,
// the choices are returned as they are.
func applyPrior(request Request, diagnosis Diagnosis, proposal Proposal, choices []plannedChoice) []plannedChoice {
	if request.Prior == nil || len(proposal.Candidates) == 0 {
		return choices
	}
	control := proposal.Candidates[0]
	runtime := control.Deployment.Runtime
	running, _ := diagnosis.signal("runtime_requests_running_per_replica")
	waiting, _ := diagnosis.signal("runtime_requests_waiting_per_replica")
	load := int(math.Round(running + waiting))
	if load < 1 {
		return choices
	}
	input, _ := diagnosis.signal("mean_input_tokens")
	output, _ := diagnosis.signal("mean_output_tokens")
	hit, _ := diagnosis.signal(metrics.MeasurementPrefixCacheHitRate)
	metric := resultstore.MetricOutput
	if request.Objective == lab.ObjectiveUsefulOutput && request.MaxTTFTP95MS != nil {
		metric = resultstore.UsefulOutputMetric(*request.MaxTTFTP95MS)
	}
	query := resultprior.Query{Model: resultstore.Model{ID: request.ModelIdentity, Architecture: resultstore.ArchitectureUnknown, Features: request.ModelFeatures},
		Accelerator: request.GPU, EngineVersion: runtime.Version, Kind: resultprior.KindOf(input, output, hit), Load: load, Metric: metric,
		Current: resultprior.KnobsFromArgs(runtime.Engine, runtime.Args)}
	ids := make([]string, 0, len(techniques))
	for id := range techniques {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		item := techniques[id]
		// A contract technique's setups come from its own package, not from
		// engine flags, so a flag change cannot describe them to the prior.
		if item.env != nil || contractTechniques[id] || techniqueGate(item, control, request) != "" {
			continue
		}
		args, why := deriveArgs(control, item, diagnosis, request)
		if why != "" || slices.Equal(args, runtime.Args) {
			continue
		}
		query.Candidates = append(query.Candidates, resultprior.Candidate{Name: id, Knobs: resultprior.KnobsOfChange(runtime.Engine, runtime.Args, args)})
	}
	if len(query.Candidates) == 0 {
		return choices
	}
	ranking, err := request.Prior(query)
	if err != nil {
		return choices
	}
	var rules, addedIDs []string
	chosen := map[string]bool{}
	for _, choice := range choices {
		rules, chosen[choice.technique.id] = append(rules, choice.technique.id), true
	}
	added := 0
	out := slices.Clone(choices)
	for _, ranked := range ranking.Ranked {
		if chosen[ranked.Name] || !ranked.Known || ranked.Lower <= 0 || added >= maxPriorAdded {
			continue
		}
		added++
		addedIDs = append(addedIDs, ranked.Name)
		out = append(out, plannedChoice{technique: techniques[ranked.Name], rules: []string{priorRule},
			reason:   "The rules did not choose it; similar measured cells did (" + ranking.Algorithm + "): " + ranked.Reason,
			evidence: []ReasonEvidence{{Signal: "results_prior.lower_bound", Observed: ranked.Lower, Unit: "gain"}}})
	}
	// A rules' choice the prior moves last says why.
	for index := range out {
		for _, ranked := range ranking.Ranked {
			if ranked.Name == out[index].technique.id && chosen[ranked.Name] && ranked.Known && ranked.Estimate < 0 && len(out[index].evidence) < maxReasonItems {
				out[index].rules = append(slices.Clone(out[index].rules), priorRule)
				out[index].evidence = append(slices.Clone(out[index].evidence), ReasonEvidence{Signal: "results_prior.estimate", Observed: ranked.Estimate, Unit: "gain"})
			}
		}
	}
	merged := resultprior.Merge(ranking, rules, addedIDs)
	if added == 0 && slices.Equal(merged, rules) {
		// Nothing measured changes the rules' plan: keep it exactly.
		return choices
	}
	position := map[string]int{}
	for index, id := range merged {
		position[id] = index + 1
	}
	for index := range out {
		out[index].priorOrder = position[out[index].technique.id]
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].priorOrder < out[j].priorOrder })
	return out
}
