package resultprior

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/resultstore"
)

// The prior's gym scores the prior on measured cells without a GPU, the way
// the planner gym (internal/optimizer, TestPlannerGym) scores the rules: for
// each cell and current setup it ranks the setups measured there, and the
// planner gym's judge says whether the winner is among the picks. The cell
// asked about is never among the records the prior reads. Run:
//
//	go test -count=1 -run '^TestPriorGym$' -v ./internal/resultprior
//
// The runs it reads are listed in datasets.json of its data directory: by
// default the synthetic runs of ../resultstore/testdata/synthetic, where
// every number is made up and the floors hold. INFERCRANE_PRIOR_GYM_DATA=DIR
// scores another directory with its own datasets.json, such as real runs
// kept outside the repository; the floors then do not apply.
// INFERCRANE_PRIOR_GYM_OUT=FILE also writes the report as Markdown.
// docs/testing/measured-results-ranking.mdx says how to read it.

const syntheticGymData = "../resultstore/testdata/synthetic"

// gymDataset is one measured run as a planner-gym cases file, and where the
// rules' answers for its cells are.
type gymDataset struct {
	name, file string
	options    resultstore.ImportOptions
	// currents are the setups each cell is asked from.
	currents []string
	// rules is TestPlannerPredict's output for these cells (trimmed); empty
	// when the rules could not be asked.
	rules string
	// rulesBuiltFrom says whether the rules were written after these cells
	// were seen.
	rulesBuiltFrom string
}

// gymDataFile is datasets.json.
type gymDataFile struct {
	Datasets []struct {
		Name           string   `json:"name"`
		File           string   `json:"file"`
		Currents       []string `json:"currents"`
		Rules          string   `json:"rules"`
		RulesBuiltFrom string   `json:"rules_built_from"`
		Options        struct {
			Architecture string  `json:"architecture"`
			ParamsB      float64 `json:"params_b"`
			Checkpoint   string  `json:"checkpoint"`
			RecordedAt   string  `json:"recorded_at"`
			ImageDigest  string  `json:"image_digest"`
			Basis        string  `json:"basis"`
			Commit       string  `json:"commit"`
			Set          string  `json:"set"`
		} `json:"options"`
	} `json:"datasets"`
}

// gymData is the directory the gym reads, and whether it is the committed
// synthetic one (the floors hold only there).
func gymData() (string, bool) {
	if dir := os.Getenv("INFERCRANE_PRIOR_GYM_DATA"); dir != "" {
		return dir, false
	}
	return syntheticGymData, true
}

func gymDatasets(t *testing.T) []gymDataset {
	t.Helper()
	dir, _ := gymData()
	body, err := os.ReadFile(filepath.Join(dir, "datasets.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file gymDataFile
	if err = json.Unmarshal(body, &file); err != nil {
		t.Fatal(err)
	}
	var out []gymDataset
	for _, item := range file.Datasets {
		dataset := gymDataset{name: item.Name, file: filepath.Join(dir, item.File), currents: item.Currents, rulesBuiltFrom: item.RulesBuiltFrom}
		if item.Rules != "" {
			dataset.rules = filepath.Join(dir, item.Rules)
		}
		o := item.Options
		dataset.options = resultstore.ImportOptions{Model: resultstore.Model{Architecture: o.Architecture, ParamsB: o.ParamsB, Checkpoint: o.Checkpoint},
			ImageDigest: o.ImageDigest, Basis: o.Basis, Commit: o.Commit, Set: o.Set}
		if o.RecordedAt != "" {
			if dataset.options.RecordedAt, err = time.Parse(time.RFC3339, o.RecordedAt); err != nil {
				t.Fatalf("%s: %v", item.Name, err)
			}
		}
		out = append(out, dataset)
	}
	return out
}

// gymCasesFile is the part of a cases file the gym reads besides the
// records.
type gymCasesFile struct {
	FirstTokenLimitMS float64 `json:"first_token_limit_ms"`
	Cells             []struct {
		Key string `json:"key"`
	} `json:"cells"`
}

// gymCase is one question: this cell, this current setup, this metric.
type gymCase struct {
	dataset         gymDataset
	key, current    string
	metric, column  string
	model           resultstore.Model
	accelerator     string
	kind            string
	load            int
	engineVersion   string
	measured        map[string]resultstore.Record
	currentKnobs    resultstore.Knobs
	rulesProposals  []string
	rulesAsked      bool
	rulesUnmeasured []string
	best            string
	currentIsWinner bool
	winners, pool   int
}

// rulesAnswers is the trimmed TestPlannerPredict output.
type rulesAnswers struct {
	RulesVersion string `json:"rules_version"`
	Cells        []struct {
		Key     string `json:"key"`
		Current string `json:"current"`
		Answers []struct {
			Objective string `json:"objective"`
			Proposals []struct {
				Label    string `json:"label"`
				Setup    string `json:"setup"`
				Measured string `json:"measured"`
			} `json:"proposals"`
		} `json:"answers"`
	} `json:"cells"`
}

func loadGymRecords(t *testing.T) ([]resultstore.Record, []gymCase) {
	t.Helper()
	var records []resultstore.Record
	var cases []gymCase
	for _, dataset := range gymDatasets(t) {
		body, err := os.ReadFile(dataset.file)
		if err != nil {
			t.Fatal(err)
		}
		imported, err := resultstore.Import(filepath.Base(dataset.file), body, dataset.options)
		if err != nil {
			t.Fatalf("%s: %v", dataset.file, err)
		}
		records = append(records, imported...)
		var file gymCasesFile
		if err = json.Unmarshal(body, &file); err != nil {
			t.Fatal(err)
		}
		var rules rulesAnswers
		if dataset.rules != "" {
			rulesBody, err := os.ReadFile(dataset.rules)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(rulesBody, &rules); err != nil {
				t.Fatal(err)
			}
		}
		metrics := []struct{ metric, column, objective string }{{resultstore.MetricOutput, "all output", "throughput"}}
		if file.FirstTokenLimitMS > 0 {
			metrics = append(metrics, struct{ metric, column, objective string }{resultstore.UsefulOutputMetric(file.FirstTokenLimitMS), fmt.Sprintf("output within a %.0f s first-token limit", file.FirstTokenLimitMS/1000), "useful-output"})
		}
		for _, cell := range file.Cells {
			for _, metric := range metrics {
				measured := map[string]resultstore.Record{}
				for _, record := range imported {
					if record.Cell.Key == cell.Key && record.Metric() == metric.metric {
						measured[record.Setup.Name] = record
					}
				}
				for _, current := range dataset.currents {
					base, ok := measured[current]
					if !ok || len(measured) < 2 {
						continue
					}
					item := gymCase{dataset: dataset, key: cell.Key, current: current, metric: metric.metric, column: metric.column, model: base.Model,
						accelerator: base.Accelerator(), kind: base.Kind(), load: base.Load(), engineVersion: base.Result.Software.EngineVersion,
						measured: measured, currentKnobs: base.Knobs()}
					item.judge()
					if dataset.rules != "" {
						item.rulesProposals, item.rulesUnmeasured, item.rulesAsked = rulesPicks(rules, cell.Key, current, metric.objective, measured)
					}
					cases = append(cases, item)
				}
			}
		}
	}
	return records, cases
}

// judge finds the cell's best setup and which setups count as winners: the
// planner gym's rule, a setup whose measured range reaches the best's.
func (c *gymCase) judge() {
	names := make([]string, 0, len(c.measured))
	for name := range c.measured {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if c.best == "" || c.measured[name].Observed.Median > c.measured[c.best].Observed.Median {
			c.best = name
		}
	}
	for _, name := range names {
		if name == c.current {
			c.currentIsWinner = c.winner(name)
			continue
		}
		c.pool++
		if c.winner(name) {
			c.winners++
		}
	}
}

func (c gymCase) winner(name string) bool {
	record, ok := c.measured[name]
	return ok && record.Observed.Max >= c.measured[c.best].Observed.Min
}

// rulesPicks is the rules' runnable proposals in order, as the planner gym
// reads a predictions file: a proposal that cannot start or changes nothing
// is skipped; one that makes a setup no run measured in this cell is kept
// in its place and named.
func rulesPicks(rules rulesAnswers, key, current, objective string, measured map[string]resultstore.Record) (picks, unmeasured []string, asked bool) {
	for _, cell := range rules.Cells {
		if cell.Key != key || cell.Current != current {
			continue
		}
		for _, answer := range cell.Answers {
			if answer.Objective != objective {
				continue
			}
			asked = true
			for _, proposal := range answer.Proposals {
				if strings.HasPrefix(proposal.Measured, "cannot start") || proposal.Measured == "no change" {
					continue
				}
				setup := proposal.Setup
				if _, ok := measured[setup]; !ok || setup == "" {
					unmeasured = append(unmeasured, proposal.Label)
					setup = ""
				}
				picks = append(picks, setup)
			}
		}
	}
	return picks, unmeasured, asked
}

// heldOut says which records a case may not read.
type heldOut struct {
	name, words string
	drop        func(gymCase, resultstore.Record) bool
}

func holdOuts() []heldOut {
	return []heldOut{
		{name: "cell held out", words: "every record of the same model, GPU, traffic kind and load is removed (the screen and the confirmation of one L4 cell go together); the rest of that run stays: the customer's own earlier cells",
			drop: func(c gymCase, r resultstore.Record) bool {
				return r.Model.ID == c.model.ID && r.Accelerator() == c.accelerator && r.Kind() == c.kind && r.Load() == c.load
			}},
		{name: "model and GPU held out", words: "every record of the same model on the same GPU is removed: a new customer, ranked only from other models or other GPUs",
			drop: func(c gymCase, r resultstore.Record) bool {
				return r.Model.ID == c.model.ID && r.Accelerator() == c.accelerator
			}},
	}
}

func keptRecords(records []resultstore.Record, item gymCase, hold heldOut) []resultstore.Record {
	var kept []resultstore.Record
	for _, record := range records {
		if !hold.drop(item, record) {
			kept = append(kept, record)
		}
	}
	return kept
}

func caseQuery(item gymCase) Query {
	return Query{Model: item.model, Accelerator: item.accelerator, EngineVersion: item.engineVersion, Kind: item.kind, Load: item.load, Metric: item.metric, Current: item.currentKnobs}
}

// rulesWithSuggestions is what the planner would screen with the prior: the
// rules' runnable proposals plus up to three setups the nearest measured
// cells suggest (Suggest), merged by Merge. A suggestion this cell did not
// measure is "" (unknown) for the judge.
func rulesWithSuggestions(records []resultstore.Record, item gymCase, hold heldOut) []string {
	kept := keptRecords(records, item, hold)
	query := caseQuery(item)
	for _, name := range item.rulesProposals {
		if name != "" {
			query.Candidates = append(query.Candidates, Candidate{Name: name, Knobs: item.measured[name].Knobs()})
		}
	}
	var added []string
	for _, suggestion := range Suggest(kept, query, 3) {
		name := "unmeasured: " + suggestion.Knobs.Words()
		for measuredName, record := range item.measured {
			if record.Knobs() == suggestion.Knobs {
				name = measuredName
			}
		}
		query.Candidates = append(query.Candidates, Candidate{Name: name, Knobs: suggestion.Knobs})
		added = append(added, name)
	}
	merged := Merge(Rank(kept, query, DefaultSettings()), item.rulesProposals, added)
	for index, name := range merged {
		if strings.HasPrefix(name, "unmeasured: ") {
			merged[index] = ""
		}
	}
	return merged
}

// priorPicks ranks the setups measured in the case's cell, other than the
// current one, from the records the hold-out leaves.
func priorPicks(records []resultstore.Record, item gymCase, hold heldOut) Ranking {
	kept := keptRecords(records, item, hold)
	query := caseQuery(item)
	names := make([]string, 0, len(item.measured))
	for name := range item.measured {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name != item.current {
			query.Candidates = append(query.Candidates, Candidate{Name: name, Knobs: item.measured[name].Knobs()})
		}
	}
	return Rank(kept, query, DefaultSettings())
}

// tally is one picker's score on the cases where something beats the
// current setup.
type tally struct {
	cases, top1, top3, unknown1, unknown3 int
	random1, random3                      float64
}

func (s *tally) add(item gymCase, picks []string) {
	s.cases++
	s.random1 += float64(item.winners) / float64(item.pool)
	s.random3 += randomTop(item.pool, item.winners, 3)
	hit := func(k int) (bool, bool) {
		unknown := false
		for _, pick := range picks[:min(k, len(picks))] {
			if pick == "" {
				unknown = true
				continue
			}
			if item.winner(pick) {
				return true, false
			}
		}
		return false, unknown
	}
	if ok, unknown := hit(1); ok {
		s.top1++
	} else if unknown {
		s.unknown1++
	}
	if ok, unknown := hit(3); ok {
		s.top3++
	} else if unknown {
		s.unknown3++
	}
}

// randomTop is the chance that k setups drawn at random from a pool of n
// include at least one of its w winners.
func randomTop(n, w, k int) float64 {
	if w <= 0 {
		return 0
	}
	if k >= n {
		return 1
	}
	miss := 1.0
	for i := 0; i < k; i++ {
		miss *= float64(n-w-i) / float64(n-i)
	}
	return 1 - math.Max(miss, 0)
}

func (s tally) words(withRandom bool) string {
	if s.cases == 0 {
		return "no cases"
	}
	text := fmt.Sprintf("first pick %d of %d, first three %d of %d", s.top1, s.cases, s.top3, s.cases)
	if s.unknown3 > 0 || s.unknown1 > 0 {
		text += fmt.Sprintf(" (unknown: %d first picks, %d first threes made a setup no run measured)", s.unknown1, s.unknown3)
	}
	if withRandom {
		text += fmt.Sprintf("; a random order would get %.1f and %.1f", s.random1, s.random3)
	}
	return text
}

func share(hits, cases int) string {
	if cases == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", hits, cases, 100*float64(hits)/float64(cases))
}

// priorGymFloors are the prior's hits (first pick, first three) on the
// synthetic runs' cells where something beats the current setup, all runs and
// both columns together, as committed. They guard the code, not a claim about
// real traffic. Raise one when a change lifts it; never lower one to let a change
// pass.
var priorGymFloors = map[string]struct{ first, three int }{
	"cell held out":          {first: 69, three: 77},
	"model and GPU held out": {first: 42, three: 77},
}

// suggestedGymFloors are the same for the rules' list plus the prior's
// suggestions, ordered by the prior, on the cases the rules answered.
var suggestedGymFloors = map[string]struct{ first, three int }{
	"cell held out":          {first: 49, three: 59},
	"model and GPU held out": {first: 35, three: 41},
}

func TestPriorGym(t *testing.T) {
	records, cases := loadGymRecords(t)
	if len(records) < 200 || len(cases) < 60 {
		t.Fatalf("the gym read %d records and %d cases; the committed cells give more", len(records), len(cases))
	}
	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	add("# Prior gym")
	add("")
	add("Prior %s over %d records from %d runs.", AlgorithmV1, len(records), len(gymDatasets(t)))
	add("A case is one cell, one current setup and one way of counting output. A setup wins when its measured range reaches the best setup's (the planner gym's judge). Cases where the current setup already wins are counted apart: the screen measures the current setup anyway, so any picker finds them.")
	add("")
	totals := map[string]*tally{}
	rulesTotal := &tally{}
	// priorOnRulesCases is the prior on exactly the cases the rules answered;
	// reordered is the rules' own list merged with the prior and nothing
	// added; suggested adds the prior's suggestions, as the planner would.
	priorOnRulesCases, reordered, suggested := map[string]*tally{}, map[string]*tally{}, map[string]*tally{}
	for _, hold := range holdOuts() {
		add("## %s", hold.name)
		add("")
		add("%s.", strings.ToUpper(hold.words[:1])+hold.words[1:])
		add("")
		add("| Run | Counting | Cases | Current already wins | To find | Prior: first pick | Prior: first three | Random: first three | Rules: first three |")
		add("|---|---|---|---|---|---|---|---|---|")
		total := &tally{}
		totals[hold.name] = total
		type group struct{ dataset, column string }
		groups := map[group]*struct {
			cases, currentWins int
			prior, rules       tally
			rulesAsked         bool
		}{}
		var order []group
		for _, item := range cases {
			key := group{item.dataset.name, item.column}
			entry, ok := groups[key]
			if !ok {
				entry = &struct {
					cases, currentWins int
					prior, rules       tally
					rulesAsked         bool
				}{}
				groups[key] = entry
				order = append(order, key)
			}
			entry.cases++
			if item.currentIsWinner || item.winners == 0 {
				entry.currentWins++
				continue
			}
			ranking := priorPicks(records, item, hold)
			picks := make([]string, 0, len(ranking.Ranked))
			for _, ranked := range ranking.Ranked {
				picks = append(picks, ranked.Name)
			}
			entry.prior.add(item, picks)
			total.add(item, picks)
			if item.rulesAsked {
				if priorOnRulesCases[hold.name] == nil {
					priorOnRulesCases[hold.name] = &tally{}
				}
				priorOnRulesCases[hold.name].add(item, picks)
				if reordered[hold.name] == nil {
					reordered[hold.name] = &tally{}
				}
				reordered[hold.name].add(item, Merge(ranking, item.rulesProposals, nil))
				if suggested[hold.name] == nil {
					suggested[hold.name] = &tally{}
				}
				suggested[hold.name].add(item, rulesWithSuggestions(records, item, hold))
				entry.rulesAsked = true
				entry.rules.add(item, item.rulesProposals)
				if hold.name == holdOuts()[0].name {
					rulesTotal.add(item, item.rulesProposals)
				}
			}
			if testing.Verbose() && os.Getenv("INFERCRANE_PRIOR_GYM_CASES") != "" {
				t.Logf("%s | %s | %s from %s | best %s | prior %v | rules %v", hold.name, item.dataset.name, item.key, item.current, item.best, picks, item.rulesProposals)
			}
		}
		for _, key := range order {
			entry := groups[key]
			toFind := entry.cases - entry.currentWins
			rules := "not asked"
			if entry.rulesAsked {
				rules = share(entry.rules.top3, entry.rules.cases)
				if entry.rules.unknown3 > 0 {
					rules += fmt.Sprintf(", %d unknown", entry.rules.unknown3)
				}
			}
			random := "-"
			if toFind > 0 {
				random = fmt.Sprintf("%.1f/%d", entry.prior.random3, toFind)
			}
			add("| %s | %s | %d | %d | %d | %s | %s | %s | %s |", key.dataset, key.column, entry.cases, entry.currentWins, toFind,
				share(entry.prior.top1, entry.prior.cases), share(entry.prior.top3, entry.prior.cases), random, rules)
		}
		add("")
		add("All runs: prior %s.", total.words(true))
		add("")
	}
	add("## Against the rules, on the same cases")
	add("")
	add("The rules on the cases they were asked (no hold-out applies: the rules are fixed): %s.", rulesTotal.words(true))
	for _, hold := range holdOuts() {
		if same := priorOnRulesCases[hold.name]; same != nil {
			add("The prior on the same cases, %s: %s.", hold.name, same.words(false))
		}
		if merged := reordered[hold.name]; merged != nil {
			add("The rules' own list merged with the prior (resultprior.Merge, nothing added), %s: %s.", hold.name, merged.words(false))
		}
		if merged := suggested[hold.name]; merged != nil {
			add("The rules' list plus up to three setups the nearest measured cells suggest (resultprior.Suggest), merged by resultprior.Merge as the planner would, %s: %s.", hold.name, merged.words(false))
		}
	}
	add("")
	for _, dataset := range gymDatasets(t) {
		add("- %s: rules built from it: %s.", dataset.name, dataset.rulesBuiltFrom)
	}
	report := strings.Join(lines, "\n")
	t.Log("\n" + report)
	if out := os.Getenv("INFERCRANE_PRIOR_GYM_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(report+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, synthetic := gymData(); !synthetic {
		return
	}
	for name, floor := range suggestedGymFloors {
		if got := suggested[name]; got == nil || got.top1 < floor.first || got.top3 < floor.three {
			t.Errorf("rules plus suggestions, %s: %+v, below the floor of %d and %d", name, got, floor.first, floor.three)
		}
	}
	for name, floor := range priorGymFloors {
		if got := totals[name]; got.top1 < floor.first || got.top3 < floor.three {
			t.Errorf("%s: %d first-pick and %d first-three hits, below the floor of %d and %d", name, got.top1, got.top3, floor.first, floor.three)
		}
	}
}

func TestPriorGymCasesComeFromEveryRun(t *testing.T) {
	_, cases := loadGymRecords(t)
	seen := map[string]int{}
	for _, item := range cases {
		seen[item.dataset.name]++
	}
	for _, dataset := range gymDatasets(t) {
		if seen[dataset.name] == 0 {
			t.Errorf("%s gave no case", dataset.name)
		}
	}
	if got := randomTop(4, 1, 3); math.Abs(got-0.75) > 1e-9 {
		t.Fatalf("random first three of four with one winner: %v", got)
	}
	if got := randomTop(3, 1, 3); got != 1 {
		t.Fatalf("three of three: %v", got)
	}
	if !slices.Contains([]string{"cell held out"}, holdOuts()[0].name) {
		t.Fatal("the first hold-out is the cell")
	}
}
