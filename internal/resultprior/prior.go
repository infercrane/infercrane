// Package resultprior ranks candidate setups by what similar measured cells
// showed. It is the "prune" half of "prune with a model, then measure the top
// few": it orders the list the screen measures, and it never decides or
// claims anything. Measurement decides.
//
// The method is kept simple enough to explain in one sentence per candidate:
// find the measured cells most like the one asked about (same kind of
// traffic, a nearby load, a similar GPU and model), read how much better or
// worse the candidate did than the current setup in each, average those with
// more weight on closer cells, shrink the average toward "no change" when
// there are few of them, and rank by the lower end of that estimate. A
// candidate no similar cell measured is ranked last and named unknown.
//
// It reads records from internal/resultstore: the tenant's own, and other
// tenants' only as resultstore.Record.NumbersOnly reduces them.
package resultprior

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/infercrane/infercrane/internal/gputrace"
	"github.com/infercrane/infercrane/internal/resultstore"
)

// AlgorithmV1 names the method; a change to it, a weight, or a setting is a
// new version.
const AlgorithmV1 = "nearest-cells-lower-bound-v1"

// Query is the cell the planner is asked about and the candidates it would
// measure there.
type Query struct {
	Model       resultstore.Model `json:"model"`
	Accelerator string            `json:"accelerator"`
	// EngineVersion is the current setup's engine version.
	EngineVersion string `json:"engine_version"`
	Kind          string `json:"kind"`
	Load          int    `json:"load"`
	Metric        string `json:"metric"`
	// Current is how the endpoint serves now; every gain is against it.
	Current    resultstore.Knobs `json:"current"`
	Candidates []Candidate       `json:"candidates"`
}

// Candidate is one setup the planner could measure.
type Candidate struct {
	Name  string            `json:"name"`
	Knobs resultstore.Knobs `json:"knobs"`
}

// Settings are the method's constants. They are part of the algorithm
// version.
type Settings struct {
	// Neighbours is how many of the closest cells with evidence count per
	// candidate.
	Neighbours int
	// Scale turns a distance into a weight: exp(-distance / Scale).
	Scale float64
	// Shrink is how many perfectly similar cells' worth of "no change" every
	// estimate starts from.
	Shrink float64
	// PriorSpread is the spread of a gain no cell measured.
	PriorSpread float64
	// Z is how many standard errors below the estimate the ranking reads.
	Z float64
}

// DefaultSettings are the settings of AlgorithmV1.
func DefaultSettings() Settings {
	return Settings{Neighbours: 6, Scale: 1, Shrink: 0.25, PriorSpread: 0.25, Z: 1}
}

// Evidence is one cell's measurement of a candidate against the current
// setup.
type Evidence struct {
	Cell string `json:"cell"`
	// Gain is the candidate's output over the current setup's in that cell,
	// as (candidate - current) / max(candidate, current): -1 to 1, close to
	// the relative change for small changes, and defined when either is 0.
	Gain     float64 `json:"gain"`
	Spread   float64 `json:"spread"`
	Distance float64 `json:"distance"`
	Weight   float64 `json:"weight"`
	Shared   bool    `json:"shared,omitempty"`
}

// Ranked is one candidate's place in the ranking.
type Ranked struct {
	Name  string `json:"name"`
	Setup string `json:"setup"`
	// Estimate is the weighted, shrunk mean gain; Lower is Estimate less Z
	// standard errors. Both are on Evidence.Gain's scale.
	Estimate float64    `json:"estimate"`
	Lower    float64    `json:"lower"`
	Cells    int        `json:"cells"`
	Weight   float64    `json:"weight"`
	Known    bool       `json:"known"`
	Evidence []Evidence `json:"evidence,omitempty"`
	Reason   string     `json:"reason"`
}

// Ranking is the ordered candidates and what they were ranked from.
type Ranking struct {
	Algorithm string   `json:"algorithm"`
	Metric    string   `json:"metric"`
	Records   int      `json:"records"`
	Cells     int      `json:"cells"`
	Ranked    []Ranked `json:"ranked"`
}

// Prior ranks candidates for a tenant. The planner calls it; it reads the
// tenant's records and, when the store has them, other tenants' shared
// numbers.
type Prior interface {
	Rank(ctx context.Context, tenant string, query Query) (Ranking, error)
}

// StorePrior is a Prior over a results store.
type StorePrior struct {
	Store    resultstore.Store
	Settings Settings
	// UseShared adds other tenants' records that opted in, numbers only.
	UseShared bool
}

// Rank reads the records for the query's metric and ranks its candidates.
func (p StorePrior) Rank(ctx context.Context, tenant string, query Query) (Ranking, error) {
	filter := resultstore.Filter{Metric: query.Metric, Limit: resultstore.MaxListLimit}
	records, err := p.Store.ListResultRecords(ctx, tenant, filter)
	if err != nil {
		return Ranking{}, err
	}
	if p.UseShared {
		shared, err := p.Store.SharedResultRecords(ctx, tenant, filter)
		if err != nil {
			return Ranking{}, err
		}
		records = append(records, shared...)
	}
	settings := p.Settings
	if settings == (Settings{}) {
		settings = DefaultSettings()
	}
	return Rank(records, query, settings), nil
}

// cell is one measurement: the setups measured in it, by knobs.
type cell struct {
	words, kind, accelerator, engineVersion string
	load                                    int
	model                                   resultstore.Model
	shared                                  bool
	observed                                map[resultstore.Knobs]resultstore.Observed
}

// collect groups the records of the query's metric into cells, in the order
// they were read, and counts the records read.
func collect(records []resultstore.Record, query Query) (map[string]*cell, []string, int) {
	cells := map[string]*cell{}
	var order []string
	read := 0
	for _, record := range records {
		if record.Metric() != query.Metric {
			continue
		}
		read++
		item, ok := cells[record.Cell.ID]
		if !ok {
			item = &cell{words: record.CellWords(), kind: record.Kind(), load: record.Load(), accelerator: record.Accelerator(), model: record.Model,
				shared: record.Shared(), observed: map[resultstore.Knobs]resultstore.Observed{}}
			cells[record.Cell.ID] = item
			order = append(order, record.Cell.ID)
		}
		knobs := record.Knobs()
		if _, seen := item.observed[knobs]; !seen {
			item.observed[knobs] = record.Observed
		}
		if knobs.Engine == normal(query.Current).Engine && item.engineVersion == "" {
			item.engineVersion = record.Result.Software.EngineVersion
		}
	}
	return cells, order, read
}

// Rank orders the query's candidates by the lower bound of their estimated
// gain over the current setup in similar cells.
func Rank(records []resultstore.Record, query Query, settings Settings) Ranking {
	cells, order, read := collect(records, query)
	ranking := Ranking{Algorithm: AlgorithmV1, Metric: query.Metric, Records: read, Cells: len(cells)}
	current := normal(query.Current)
	for _, candidate := range query.Candidates {
		knobs := normal(candidate.Knobs)
		var evidence []Evidence
		for _, id := range order {
			item := cells[id]
			mine, measured := item.observed[knobs]
			base, based := item.observed[current]
			if !measured || !based || (mine.Median <= 0 && base.Median <= 0) {
				continue
			}
			top := math.Max(mine.Median, base.Median)
			distance := Distance(query, item.model, item.accelerator, item.engineVersion, item.kind, item.load)
			// The spread is on the gain's scale and capped there: a range of
			// servers wider than the larger median says the cell knows little,
			// not that the gain could be beyond -1 to 1.
			spread := math.Min(math.Max(mine.Max-mine.Min, base.Max-base.Min)/top, 1)
			evidence = append(evidence, Evidence{Cell: item.words, Gain: round4((mine.Median - base.Median) / top),
				Spread: round4(spread), Distance: distance, Weight: round4(math.Exp(-distance / settings.Scale)), Shared: item.shared})
		}
		slices.SortStableFunc(evidence, func(a, b Evidence) int { return cmp.Compare(b.Weight, a.Weight) })
		if len(evidence) > settings.Neighbours {
			evidence = evidence[:settings.Neighbours]
		}
		ranking.Ranked = append(ranking.Ranked, estimate(candidate.Name, knobs, evidence, settings))
	}
	slices.SortStableFunc(ranking.Ranked, func(a, b Ranked) int {
		if c := cmp.Compare(b.Lower, a.Lower); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Estimate, a.Estimate); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return ranking
}

// estimate is the shrunk weighted mean of a candidate's gains and its lower
// bound. With weights w, gains g and spreads s:
//
//	mean     = Σ w·g / (Σ w + Shrink)
//	variance = (Σ w·(g - mean)² + Shrink·PriorSpread²) / (Σ w + Shrink) + Σ w·s² / Σ w
//	n        = (Σ w)² / Σ w²
//	lower    = mean - Z·sqrt(variance / (n + Shrink))
//
// Without evidence the mean is 0 and the lower bound PriorSpread/√Shrink
// below it, under every candidate with some.
func estimate(name string, knobs resultstore.Knobs, evidence []Evidence, settings Settings) Ranked {
	ranked := Ranked{Name: name, Setup: knobs.Words(), Cells: len(evidence), Evidence: evidence}
	var weights, weighted, squares, spreads float64
	for _, item := range evidence {
		weights += item.Weight
		weighted += item.Weight * item.Gain
		squares += item.Weight * item.Weight
		spreads += item.Weight * item.Spread * item.Spread
	}
	mean := weighted / (weights + settings.Shrink)
	deviation := settings.Shrink * settings.PriorSpread * settings.PriorSpread
	for _, item := range evidence {
		deviation += item.Weight * (item.Gain - mean) * (item.Gain - mean)
	}
	variance := deviation / (weights + settings.Shrink)
	effective := 0.0
	if weights > 0 {
		variance += spreads / weights
		effective = weights * weights / squares
	}
	ranked.Estimate = round4(mean)
	ranked.Lower = round4(mean - settings.Z*math.Sqrt(variance/(effective+settings.Shrink)))
	ranked.Weight = round4(weights)
	ranked.Known = len(evidence) > 0
	if !ranked.Known {
		ranked.Reason = "unknown: no similar measured cell has this setup beside the current one"
		return ranked
	}
	closest := evidence[0]
	ranked.Reason = fmt.Sprintf("about %s over the current setup from %d similar cell(s), at least %s; closest: %s, %s", percent(Relative(ranked.Estimate)), len(evidence),
		percent(Relative(ranked.Lower)), closest.Cell, percent(Relative(closest.Gain)))
	return ranked
}

// Relative turns a gain on Evidence.Gain's scale into the plain relative
// change over the current setup: g/(1-g) above zero, g itself below. A gain
// of 1 (the current setup served nothing) has no relative change and reads as
// +Inf.
func Relative(gain float64) float64 {
	if gain <= 0 {
		return gain
	}
	if gain >= 1 {
		return math.Inf(1)
	}
	return gain / (1 - gain)
}

// Distance is how unlike the query a measured cell is. Each term is in the
// same unit: about 1 for a difference that, on this week's runs, changed
// which setup won.
//
//   - traffic kind: 0 the same; 0.5 the same kind with reasoning switched;
//     1 two decode-heavy kinds (short, output) or two prompt-heavy kinds
//     (long, prefix); 2 otherwise.
//   - load: 0.75 per doubling of requests at once.
//   - GPU: |ln| of the ratio of datasheet memory bandwidths plus half that
//     of compute (gputrace.LookupPeak); 2.5 for a GPU without datasheet
//     numbers that is not the same name.
//   - model: 1 for another architecture (0.5 if either is unknown); 0.5·|ln|
//     of the ratio of parameter counts (0.5 if unknown); 0.5 for another
//     checkpoint precision; 0.5 per feature one has and the other not.
//   - engine version of the current setup: 0.25 when it differs.
func Distance(query Query, model resultstore.Model, accelerator, engineVersion, kind string, load int) float64 {
	distance := kindDistance(query.Kind, kind)
	if query.Load > 0 && load > 0 {
		distance += 0.75 * math.Abs(math.Log2(float64(query.Load)/float64(load)))
	}
	distance += acceleratorDistance(query.Accelerator, accelerator)
	switch {
	case query.Model.Architecture == model.Architecture && model.Architecture != resultstore.ArchitectureUnknown:
	case query.Model.Architecture == resultstore.ArchitectureUnknown || model.Architecture == resultstore.ArchitectureUnknown || query.Model.Architecture == "" || model.Architecture == "":
		distance += 0.5
	default:
		distance += 1
	}
	if query.Model.ParamsB > 0 && model.ParamsB > 0 {
		distance += 0.5 * math.Abs(math.Log(query.Model.ParamsB/model.ParamsB))
	} else {
		distance += 0.5
	}
	if !strings.EqualFold(query.Model.Checkpoint, model.Checkpoint) {
		distance += 0.5
	}
	for _, feature := range query.Model.Features {
		if !slices.Contains(model.Features, feature) {
			distance += 0.5
		}
	}
	for _, feature := range model.Features {
		if !slices.Contains(query.Model.Features, feature) {
			distance += 0.5
		}
	}
	if query.EngineVersion != "" && engineVersion != "" && query.EngineVersion != engineVersion {
		distance += 0.25
	}
	return round4(distance)
}

func kindDistance(a, b string) float64 {
	if a == b {
		return 0
	}
	baseA, baseB := strings.TrimSuffix(a, "-reasoning"), strings.TrimSuffix(b, "-reasoning")
	if baseA == baseB {
		return 0.5
	}
	group := func(kind string) string {
		switch kind {
		case "short", "output", "chat":
			return "decode"
		case "long", "prefix", "rag":
			return "prompt"
		}
		return kind
	}
	if group(baseA) == group(baseB) {
		return 1
	}
	return 2
}

func acceleratorDistance(a, b string) float64 {
	if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) {
		return 0
	}
	peakA, okA := lookupPeak(a)
	peakB, okB := lookupPeak(b)
	if !okA || !okB || peakA.MemoryTBps <= 0 || peakB.MemoryTBps <= 0 || peakA.ComputeTFLOPS <= 0 || peakB.ComputeTFLOPS <= 0 {
		return 2.5
	}
	return math.Abs(math.Log(peakA.MemoryTBps/peakB.MemoryTBps)) + 0.5*math.Abs(math.Log(peakA.ComputeTFLOPS/peakB.ComputeTFLOPS))
}

// lookupPeak reads the datasheet; a bare H100 is the SXM part, as InferCrane
// plans it.
func lookupPeak(accelerator string) (peak struct{ MemoryTBps, ComputeTFLOPS float64 }, ok bool) {
	for _, name := range []string{accelerator, accelerator + " SXM"} {
		if found, known := gputrace.LookupPeak(name); known {
			return struct{ MemoryTBps, ComputeTFLOPS float64 }{found.MemoryTBps, found.ComputeTFLOPS}, true
		}
	}
	return peak, false
}

func normal(knobs resultstore.Knobs) resultstore.Knobs { return knobs.Normal() }

func round4(value float64) float64 { return math.Round(value*1e4) / 1e4 }

func percent(change float64) string {
	if math.IsInf(change, 1) {
		return "output where the current setup had none"
	}
	return fmt.Sprintf("%+.1f%%", 100*change)
}
