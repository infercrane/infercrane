package resultprior

import (
	"cmp"
	"slices"
	"strings"

	"github.com/infercrane/infercrane/internal/resultstore"
)

// KnobsFromArgs reads a setup's knobs from its engine flags
// (resultstore.KnobsFromArgs).
func KnobsFromArgs(engine string, args []string) resultstore.Knobs {
	return resultstore.KnobsFromArgs(engine, args)
}

// Merge puts the prior's ranking into the rules' order conservatively. The
// rules' first choice keeps its place unless similar cells measured it
// losing: the rules know limits the prior cannot see, such as the cache room
// that moves a winner between two measured loads. Right after it come the
// added candidates whose lower bound is above zero (gains measured in similar
// cells that the rules did not choose), best first. Then the rules' other
// choices in their order, and last the rules' choices whose estimate is a
// measured loss. An added candidate that is not a measured gain is left out.
func Merge(ranking Ranking, rules, added []string) []string {
	byName := map[string]Ranked{}
	for _, ranked := range ranking.Ranked {
		byName[ranked.Name] = ranked
	}
	gain := func(name string) bool { ranked, ok := byName[name]; return ok && ranked.Known && ranked.Lower > 0 }
	loss := func(name string) bool { ranked, ok := byName[name]; return ok && ranked.Known && ranked.Estimate < 0 }
	var head, rest, losses, gains []string
	for index, name := range rules {
		switch {
		case loss(name):
			losses = append(losses, name)
		case index == 0:
			head = append(head, name)
		default:
			rest = append(rest, name)
		}
	}
	for _, name := range added {
		if gain(name) {
			gains = append(gains, name)
		}
	}
	slices.SortStableFunc(gains, func(a, b string) int { return cmp.Compare(byName[b].Lower, byName[a].Lower) })
	if len(head) == 0 && len(rest) > 0 {
		head, rest = rest[:1], rest[1:]
	}
	out := append(append(append(head, gains...), rest...), losses...)
	return out
}

// Suggest names setups that the nearest measured cells measured beside the
// current setup and that the query's candidates do not include yet, nearest
// cell first, at most limit. Rules propose from what limits a server; this
// proposes from what was measured, so a setup the rules missed can still be
// screened. The planner checks that each can start on this engine, GPU and
// model before it measures it.
func Suggest(records []resultstore.Record, query Query, limit int) []Candidate {
	cells, order, _ := collect(records, query)
	current := normal(query.Current)
	type near struct {
		item     *cell
		distance float64
	}
	var nearest []near
	for _, id := range order {
		item := cells[id]
		if _, ok := item.observed[current]; ok {
			nearest = append(nearest, near{item, Distance(query, item.model, item.accelerator, item.engineVersion, item.kind, item.load)})
		}
	}
	slices.SortStableFunc(nearest, func(a, b near) int { return cmp.Compare(a.distance, b.distance) })
	taken := map[resultstore.Knobs]bool{current: true}
	for _, candidate := range query.Candidates {
		taken[normal(candidate.Knobs)] = true
	}
	var out []Candidate
	for _, entry := range nearest {
		knobs := make([]resultstore.Knobs, 0, len(entry.item.observed))
		for measured := range entry.item.observed {
			knobs = append(knobs, measured)
		}
		slices.SortFunc(knobs, func(a, b resultstore.Knobs) int { return strings.Compare(a.Words(), b.Words()) })
		for _, measured := range knobs {
			if taken[measured] || len(out) >= limit {
				continue
			}
			taken[measured] = true
			out = append(out, Candidate{Name: "measured: " + measured.Words(), Knobs: measured})
		}
	}
	return out
}

// KnobsOfChange is a candidate's knobs against the setup it changes: the knob
// flags it serves with, and every other flag it sets, changes, or removes, as
// "--name=value" ("--name=" when removed). A candidate that only changes knobs
// matches records of those knobs; one that changes anything else matches only
// records that changed the same.
func KnobsOfChange(engine string, control, candidate []string) resultstore.Knobs {
	knobs := KnobsFromArgs(engine, candidate)
	before, after := resultstore.FlagValues(control), resultstore.FlagValues(candidate)
	var changed []string
	for name, value := range after {
		if old, ok := before[name]; !resultstore.KnobFlag(name) && (!ok || old != value) {
			changed = append(changed, name+"="+value)
		}
	}
	for name := range before {
		if _, kept := after[name]; !resultstore.KnobFlag(name) && !kept {
			changed = append(changed, name+"=")
		}
	}
	slices.Sort(changed)
	knobs.Flags = strings.Join(changed, " ")
	return knobs
}

// KindOf names the traffic kind the store's cells use from what a diagnosis
// reads: shared prefix when at least half the prompt tokens hit the prefix
// cache, long when prompts average 2,000 tokens or more, output when answers
// average 1,000 tokens or more, short otherwise. The thresholds sit between
// the kinds of this week's runs (prompts of about 40, 800 and 3,900 tokens;
// answers of about 270 and 1,400).
func KindOf(promptTokens, answerTokens, prefixHitRate float64) string {
	switch {
	case prefixHitRate >= 0.5:
		return "prefix"
	case promptTokens >= 2000:
		return "long"
	case answerTokens >= 1000:
		return "output"
	}
	return "short"
}
