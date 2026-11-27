package optimizer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"context"

	"github.com/infercrane/infercrane/internal/resultprior"
	"github.com/infercrane/infercrane/internal/resultstore"
)

// TestPlannerGymWithPrior asks the planner every gym case with the results
// store's prior bound to this week's records, never reading the cell asked
// about (same model, GPU, traffic kind and load), and scores it by the gym's
// judge beside the rules alone. INFERCRANE_PLANNER_GYM_CASES scores another
// cases file.
func TestPlannerGymWithPrior(t *testing.T) {
	vllm := "sha256:cf5b2fdd087eb4f6d261736967cc5cde974bd22ff4df50365f8fa419b6b4c484"
	imports := []struct {
		file    string
		options resultstore.ImportOptions
	}{
		{gymCasesFile, resultstore.ImportOptions{Model: resultstore.Model{Architecture: resultstore.ArchitectureHybrid, ParamsB: 27, Checkpoint: "fp8"}, RecordedAt: time.Date(2026, 11, 23, 16, 17, 0, 0, time.UTC), ImageDigest: vllm}},
		// Synthetic runs (internal/resultstore/testdata/synthetic/generate.py): every number is made up.
		{"../resultstore/testdata/synthetic/screen-dense.json", resultstore.ImportOptions{Model: resultstore.Model{Architecture: resultstore.ArchitectureDense, ParamsB: 8.2, Checkpoint: "bf16"}, RecordedAt: time.Date(2001, 1, 3, 3, 0, 0, 0, time.UTC), Set: "synthetic-screen"}},
		{"../resultstore/testdata/synthetic/confirm-dense.json", resultstore.ImportOptions{}},
		{"../resultstore/testdata/synthetic/two-engines-hybrid.json", resultstore.ImportOptions{}},
	}
	var records []resultstore.Record
	for _, item := range imports {
		body, err := os.ReadFile(item.file)
		if err != nil {
			t.Fatal(err)
		}
		imported, err := resultstore.Import(filepath.Base(item.file), body, item.options)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, imported...)
	}
	file := loadGym(t)
	if other := os.Getenv("INFERCRANE_PLANNER_GYM_CASES"); other != "" {
		file = loadGymFile(t, other)
	}
	var lines []string
	for _, column := range gymColumns(file) {
		rules, prior := gymSummary{}, gymSummary{}
		for _, item := range gymCases(t, file) {
			if item.table != "map window" {
				continue
			}
			kind := strings.Split(item.name, "/")[0]
			if strings.HasSuffix(item.name, "/reasoning") {
				kind += "-reasoning"
			}
			kept := make([]resultstore.Record, 0, len(records))
			for _, record := range records {
				if !(record.Model.ID == file.Model.ID && record.Accelerator() == file.GPU && record.Kind() == kind && record.Load() == item.load) {
					kept = append(kept, record)
				}
			}
			for name, bound := range map[string]PriorFunc{"rules": nil, "prior": func(query resultprior.Query) (resultprior.Ranking, error) {
				return resultprior.Rank(kept, query, resultprior.DefaultSettings()), nil
			}} {
				request := column.request(gymRequest(file, item.diagnosis, item.current))
				request.Prior = bound
				proposal, err := catalogSource(t).Propose(context.Background(), request)
				if err != nil {
					t.Fatalf("%s: %v", item.name, err)
				}
				control := proposal.Candidates[0].Deployment.Runtime.Args
				var runnable []string
				outcome, firstWhy := gymOutcome{}, ""
				for _, candidate := range proposal.Candidates[1:] {
					if candidate.Reason == nil || candidate.Reason.Role != ReasonDiagnosis {
						continue
					}
					setup, why := gymChange(file, item.current, control, candidate.Deployment.Runtime.Args)
					if strings.HasPrefix(why, "cannot start") || why == "no change" {
						continue
					}
					if len(runnable) == 0 {
						outcome.proposal, firstWhy = candidate.Reason.Label, why
					}
					runnable = append(runnable, setup)
				}
				if len(runnable) == 0 {
					runnable = []string{item.current}
				}
				outcome = gymJudge(item, column, outcome, runnable, firstWhy)
				target := &rules
				if name == "prior" {
					target = &prior
				}
				switch {
				case outcome.scored:
					target.scored++
					if outcome.hit {
						target.hits++
					}
					if outcome.top3 {
						target.top3++
					}
					target.givenUp = append(target.givenUp, outcome.givenUp)
				default:
					target.unknown++
				}
				if testing.Verbose() && name == "prior" {
					t.Logf("%s | %s from %s | %s | %s", column.name, item.name, item.current, outcome.proposal, outcome.result)
				}
			}
		}
		lines = append(lines, fmt.Sprintf("%s, rules alone: %s.", column.name, rules.words()), fmt.Sprintf("%s, rules with the prior: %s.", column.name, prior.words()))
		// The prior may only add: with it, the planner keeps every hit it had.
		if os.Getenv("INFERCRANE_PLANNER_GYM_CASES") == "" && (prior.hits < rules.hits || prior.top3 < rules.top3) {
			t.Errorf("%s: the prior lost hits: %s against %s", column.name, prior.words(), rules.words())
		}
	}
	t.Log("\n" + strings.Join(lines, "\n"))
}

// An empty results store leaves every plan exactly as the rules made it.
func TestAnEmptyPriorChangesNoPlan(t *testing.T) {
	file := loadGym(t)
	empty := func(query resultprior.Query) (resultprior.Ranking, error) {
		return resultprior.Rank(nil, query, resultprior.DefaultSettings()), nil
	}
	for _, item := range gymCases(t, file) {
		for _, column := range gymColumns(file) {
			request := column.request(gymRequest(file, item.diagnosis, item.current))
			without, err := catalogSource(t).Propose(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			request.Prior = empty
			with, err := catalogSource(t).Propose(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if len(with.Candidates) != len(without.Candidates) {
				t.Fatalf("%s: %d candidates against %d", item.name, len(with.Candidates), len(without.Candidates))
			}
			for index := range with.Candidates {
				if with.Candidates[index].ID != without.Candidates[index].ID {
					t.Fatalf("%s from %s: candidate %d is %s, without a prior %s", item.name, item.current, index, with.Candidates[index].ID, without.Candidates[index].ID)
				}
			}
		}
	}
}
