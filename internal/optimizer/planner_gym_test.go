package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
)

// The planner gym scores the planner on measured cells without a GPU: for
// each cell and current setup it asks the planner as a customer would, takes
// its first proposal this GPU can run, and compares the setup that proposal
// makes with the best setup measured in that cell. The cases come from
// tools/planner-gym/build_cases.py; docs/testing/optimizer-golden-suite.mdx,
// "Planner gym", says how to read the table.
//
//	make planner-gym
//
// INFERCRANE_PLANNER_GYM_OUT=FILE also writes the table as Markdown.
// INFERCRANE_PLANNER_GYM_CASES=FILE scores another cases file (a screen of
// another model); with it, INFERCRANE_PLANNER_GYM_PREDICTIONS=FILE also scores
// a predictions file as written, before that screen ran (TestPlannerPredict).

const gymCasesFile = "testdata/planner-gym/h100-qwen38-27b-fp8-2026-11-23.json"

type gymFile struct {
	Title string `json:"title"`
	Model struct {
		ID       string   `json:"id"`
		Revision string   `json:"revision"`
		Features []string `json:"features"`
	} `json:"model"`
	GPU               string              `json:"gpu"`
	MaxNumSeqs        int                 `json:"max_num_seqs"`
	FirstTokenLimitMS float64             `json:"first_token_limit_ms"`
	Setups            map[string]gymSetup `json:"setups"`
	Cells             []gymCell           `json:"cells"`
	ProductCases      []gymProductCase    `json:"product_cases"`
	// CurrentSetups are the setups the planner is asked from in each cell;
	// the map's file names none and is asked from each of its own three.
	CurrentSetups []string `json:"current_setups"`
	// ModelMemory is what the product's model check reads for the model;
	// absent when the cases do not give it.
	ModelMemory *struct {
		WeightsBytes         float64 `json:"weights_bytes"`
		KVCacheBytesPerToken float64 `json:"kv_cache_bytes_per_token"`
	} `json:"model_memory"`
}

type gymSetup struct {
	Label          string  `json:"label"`
	Drafts         int     `json:"drafts"`
	KVCacheDtype   string  `json:"kv_cache_dtype"`
	Quantization   string  `json:"quantization"`
	Blocks         float64 `json:"blocks"`
	BlockTokens    float64 `json:"block_tokens"`
	StatePageBytes float64 `json:"state_page_bytes"`
}

type gymRange struct {
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

type gymMeasured struct {
	Output       gymRange           `json:"output"`
	UsefulOutput *gymRange          `json:"useful_output"`
	TTFTP50MS    *float64           `json:"ttft_p50_ms"`
	TTFTP95MS    *float64           `json:"ttft_p95_ms"`
	Window       map[string]float64 `json:"window"`
}

type gymCell struct {
	Key            string                 `json:"key"`
	Kind           string                 `json:"kind"`
	RequestsAtOnce int                    `json:"requests_at_once"`
	RulesBuiltFrom string                 `json:"rules_built_from"`
	Measured       map[string]gymMeasured `json:"measured"`
}

type gymProductCase struct {
	Name           string                 `json:"name"`
	Cell           string                 `json:"cell"`
	Current        string                 `json:"current"`
	RequestsAtOnce int                    `json:"requests_at_once"`
	RulesBuiltFrom string                 `json:"rules_built_from"`
	Diagnosis      Diagnosis              `json:"diagnosis"`
	AddedSignals   map[string]float64     `json:"added_signals"`
	Measured       map[string]gymMeasured `json:"measured"`
}

// gymCase is one question to the planner: this cell, this current setup.
type gymCase struct {
	table, name, current, builtFrom string
	load                            int
	diagnosis                       Diagnosis
	measured                        map[string]gymMeasured
}

// gymColumn is one way of counting a setup's output.
type gymColumn struct {
	name, objective string
	request         func(Request) Request
	value           func(gymMeasured) *gymRange
}

type gymOutcome struct {
	proposal, setup, best, result string
	hit, top3, scored             bool
	givenUp                       float64
	// unstartable lists proposals whose arguments the engine refuses.
	unstartable []string
}

func loadGym(t *testing.T) gymFile {
	t.Helper()
	return loadGymFile(t, gymCasesFile)
}

func loadGymFile(t *testing.T, path string) gymFile {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file gymFile
	if err = json.Unmarshal(body, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

// gymCases are every map cell with each setup measured there as the current
// setup (the FP8 KV cache setup is only ever a proposal), and the product's
// own recorded planner inputs.
func gymCases(t *testing.T, file gymFile) []gymCase {
	t.Helper()
	var cases []gymCase
	byKey := map[string]gymCell{}
	for _, cell := range file.Cells {
		byKey[cell.Key] = cell
		currents := file.CurrentSetups
		if len(currents) == 0 {
			currents = []string{"plain", "three-drafts", "one-draft"}
		}
		for _, current := range currents {
			measured, ok := cell.Measured[current]
			if !ok {
				continue
			}
			cases = append(cases, gymCase{table: "map window", name: cell.Key, current: current, builtFrom: cell.RulesBuiltFrom, load: cell.RequestsAtOnce,
				diagnosis: gymDiagnosis(file, file.Setups[current], measured), measured: cell.Measured})
		}
	}
	for _, product := range file.ProductCases {
		diagnosis := product.Diagnosis
		signals := map[string]float64{}
		for name, value := range diagnosis.Signals {
			signals[name] = value
		}
		for name, value := range product.AddedSignals {
			signals[name] = value
		}
		diagnosis.Signals = signals
		measured := product.Measured
		if product.Cell != "" {
			measured = byKey[product.Cell].Measured
		}
		cases = append(cases, gymCase{table: "product reading", name: product.Name, current: product.Current, builtFrom: product.RulesBuiltFrom, load: product.RequestsAtOnce,
			diagnosis: diagnosis, measured: measured})
	}
	return cases
}

// gymDiagnosis is what the product's readiness assessment makes of the map's
// measured window, built from the same readings the agent relays. A reading
// the map did not record is left out: the map stored no queue-time or
// eviction histogram, so no queue or eviction rule can fire from it.
func gymDiagnosis(file gymFile, setup gymSetup, measured gymMeasured) Diagnosis {
	window := measured.Window
	at := time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC)
	requests := int(window["requests"])
	value := func(number float64) *float64 { return &number }
	summary := domain.MonitoringSummary{
		Requests: requests, InputTokenSamples: requests, OutputTokenSamples: requests,
		InputTokens: int64(math.Round(window["prompt_tokens_mean"] * float64(requests))), OutputTokens: int64(math.Round(window["completion_tokens_mean"] * float64(requests))),
		ErrorRate: value(0), P95LatencyMS: value(window["latency_p95_ms"]), P95TTFTMS: measured.TTFTP95MS,
	}
	if generation, ok := window["generation_p95_ms"]; ok {
		summary.P95GenerationMS = value(generation)
	}
	var evidence []domain.MeasurementEvidence
	add := func(name string, number float64, samples int) {
		evidence = append(evidence, domain.MeasurementEvidence{Name: name, Value: value(number), Availability: "available", SampleCount: samples})
	}
	add(metrics.MeasurementRequestsRunning, window["running"], 1)
	add(metrics.MeasurementRequestsWaiting, window["waiting"], 1)
	add(metrics.MeasurementKVCacheUsage, window["cache_use_max"]*100, 1)
	add(metrics.MeasurementKVCacheBlocks, setup.Blocks, 1)
	add(metrics.MeasurementKVCacheBlockTokens, setup.BlockTokens, 1)
	add(metrics.MeasurementStatePageBytes, setup.StatePageBytes, 1)
	if measured.TTFTP95MS != nil {
		add(metrics.MeasurementTTFTP95, *measured.TTFTP95MS, requests)
	}
	if hit, ok := window["prefix_cache_hit_rate"]; ok {
		add(metrics.MeasurementPrefixCacheHitRate, hit, requests)
	}
	if setup.Drafts > 0 {
		add(metrics.MeasurementSpecDraftLength, float64(setup.Drafts), 1)
		if accepted, ok := window["accepted_per_draft"]; ok {
			add(metrics.MeasurementSpecDecodeAccept, accepted/float64(setup.Drafts), requests)
		}
	}
	snapshot := domain.EndpointMonitoringSnapshot{Endpoint: "gym", WindowStart: at.Add(-90 * time.Second), WindowEnd: at, Summary: summary,
		Evidence: domain.MonitoringEvidence{Fresh: true, SampleCount: requests, Measurements: evidence}}
	assessment := optimizationreadiness.Assess(snapshot, optimizationreadiness.DefaultPolicy())
	diagnosis := DiagnosisFromAssessment(assessment, snapshot.WindowStart, snapshot.WindowEnd)
	// The server adds the model check's memory facts to a diagnosis it plans
	// from (controlapi checkUncatalogedProposal); the gym adds the cases'.
	if memory := file.ModelMemory; memory != nil && memory.WeightsBytes > 0 && memory.KVCacheBytesPerToken > 0 {
		diagnosis.Signals[SignalModelWeightsBytes] = memory.WeightsBytes
		diagnosis.Signals[SignalKVCacheBytesPerToken] = memory.KVCacheBytesPerToken
	}
	return diagnosis
}

// gymFeatures is a setup as the gym can name it: its draft tokens and KV
// cache format. Any other change makes a setup no run measured.
type gymFeatures struct {
	drafts       int
	kv           string
	quantization string
}

func (f gymFile) setupNamed(features gymFeatures) string {
	names := make([]string, 0, len(f.Setups))
	for name := range f.Setups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if setup := f.Setups[name]; setup.Drafts == features.drafts && setup.KVCacheDtype == features.kv && setup.Quantization == features.quantization {
			return name
		}
	}
	return ""
}

// gymChange applies a candidate's change from the planner's starting setup to
// the current setup, as a campaign does (tools/poc-modal, with_change). It
// returns the resulting setup, or why the candidate is not measurable here.
func gymChange(file gymFile, current string, control, candidate []string) (string, string) {
	before, after := flagValues(control), flagValues(candidate)
	features := gymFeatures{drafts: file.Setups[current].Drafts, kv: file.Setups[current].KVCacheDtype, quantization: file.Setups[current].Quantization}
	changed := map[string]string{}
	for flag, value := range after {
		if before[flag] != value {
			changed[flag] = value
		}
	}
	for flag := range before {
		if _, kept := after[flag]; !kept {
			changed[flag] = ""
		}
	}
	other := []string{}
	for flag, value := range changed {
		switch flag {
		case "--speculative-config":
			features.drafts = speculationArgDrafts([]string{flag, value})
		case "--kv-cache-dtype":
			features.kv = value
			if value == "" {
				features.kv = "auto"
			}
		case "--quantization":
			features.quantization = value
		case "--max-num-seqs":
			if limit, err := strconv.Atoi(value); err == nil && file.MaxNumSeqs > 0 && limit > file.MaxNumSeqs {
				return "", fmt.Sprintf("cannot start: --max-num-seqs %d, the engine holds at most %d", limit, file.MaxNumSeqs)
			}
			other = append(other, flag+" "+value)
		default:
			other = append(other, strings.TrimSpace(flag+" "+value))
		}
	}
	if len(other) > 0 {
		sort.Strings(other)
		return "", "unmeasured: " + strings.Join(other, ", ")
	}
	if features == (gymFeatures{drafts: file.Setups[current].Drafts, kv: file.Setups[current].KVCacheDtype, quantization: file.Setups[current].Quantization}) {
		return current, "no change"
	}
	if name := file.setupNamed(features); name != "" {
		return name, ""
	}
	return "", fmt.Sprintf("unmeasured: %d draft tokens with a %s KV cache and %s weights", features.drafts, features.kv, map[string]string{"": "the model's own"}[features.quantization]+features.quantization)
}

// gymStartingSetup is the case's current setup as the endpoint's active
// revision: the flags that make its draft tokens, KV cache, and weights.
func gymStartingSetup(file gymFile, current string) *StartingSetup {
	setup := file.Setups[current]
	var args []string
	if setup.Drafts > 0 {
		args = append(args, "--speculative-config", fmt.Sprintf(`{"method":"mtp","num_speculative_tokens":%d}`, setup.Drafts))
	}
	if setup.KVCacheDtype != "" && setup.KVCacheDtype != "auto" {
		args = append(args, "--kv-cache-dtype", setup.KVCacheDtype)
	}
	if setup.Quantization != "" {
		args = append(args, "--quantization", setup.Quantization)
	}
	return &StartingSetup{Deployment: "gym", Revision: 1, Engine: "vllm", Version: "0.34.0", Args: args, GPU: file.GPU, GPUCount: 1}
}

func gymRequest(file gymFile, diagnosis Diagnosis, current string) Request {
	request := genericRequest(file.GPU, &diagnosis)
	request.StartingSetup = gymStartingSetup(file, current)
	request.ModelIdentity, request.ModelRevision = file.Model.ID, file.Model.Revision
	request.Objective, request.MaxCandidates = "throughput", 6
	request.ModelFeatures, request.ModelConfigDigest = nil, ""
	if contains(file.Model.Features, FeatureNativeMTP) {
		request.ModelFeatures, request.ModelConfigDigest = []string{FeatureNativeMTP}, recipeTestDigest
	}
	return request
}

func gymColumns(file gymFile) []gymColumn {
	limit := file.FirstTokenLimitMS
	return []gymColumn{
		{name: "all output", objective: "throughput", request: func(request Request) Request { return request }, value: func(m gymMeasured) *gymRange { return &m.Output }},
		{name: fmt.Sprintf("output within a %.0f s first-token limit", limit/1000), objective: lab.ObjectiveUsefulOutput, request: func(request Request) Request {
			request.Objective, request.MaxTTFTP95MS = lab.ObjectiveUsefulOutput, &limit
			return request
		}, value: func(m gymMeasured) *gymRange { return m.UsefulOutput }},
	}
}

// gymScore asks the planner one case and scores its first runnable proposal
// in one column. The result after the judge is the better of that proposal
// and the current setup, by this column's count.
func gymScore(t *testing.T, file gymFile, item gymCase, column gymColumn) gymOutcome {
	t.Helper()
	proposal, err := catalogSource(t).Propose(context.Background(), column.request(gymRequest(file, item.diagnosis, item.current)))
	if err != nil {
		t.Fatalf("%s: %v", item.name, err)
	}
	control := proposal.Candidates[0].Deployment.Runtime.Args
	// The setups the planner's proposals make, in its order, after leaving out
	// what cannot start and what changes nothing; "" is a setup no run measured.
	var runnable, skipped []string
	outcome, firstWhy := gymOutcome{}, ""
	for _, candidate := range proposal.Candidates[1:] {
		if candidate.Reason == nil || candidate.Reason.Role != ReasonDiagnosis {
			continue
		}
		setup, why := gymChange(file, item.current, control, candidate.Deployment.Runtime.Args)
		if strings.HasPrefix(why, "cannot start") {
			outcome.unstartable = append(outcome.unstartable, candidate.Reason.Label+": "+why)
		}
		if strings.HasPrefix(why, "cannot start") || why == "no change" {
			skipped = append(skipped, candidate.Reason.Label+": "+why)
			continue
		}
		if len(runnable) == 0 {
			outcome.proposal, firstWhy = candidate.Reason.Label, why
		}
		runnable = append(runnable, setup)
	}
	if len(runnable) == 0 {
		outcome.proposal = "nothing this GPU can run"
		if len(skipped) > 0 {
			outcome.proposal += " (" + strings.Join(skipped, "; ") + ")"
		}
		runnable = []string{item.current}
	}
	return gymJudge(item, column, outcome, runnable, firstWhy)
}

// gymJudge scores the setups a first proposal and the ones after it make, in
// the planner's order ("" is a setup no run measured), against the cell.
func gymJudge(item gymCase, column gymColumn, outcome gymOutcome, runnable []string, firstWhy string) gymOutcome {
	outcome.setup = runnable[0]
	best, bestName := gymBest(item.measured, column)
	outcome.best = bestName
	if best == nil {
		outcome.result = "unknown: this cell has no measured value for this column"
		return outcome
	}
	if outcome.setup == "" {
		outcome.result = "unknown, " + firstWhy
		return outcome
	}
	chosen := column.value(item.measured[outcome.setup])
	current := column.value(item.measured[item.current])
	if _, measured := item.measured[outcome.setup]; !measured || chosen == nil || current == nil {
		outcome.result = "unknown: " + outcome.setup + " was not measured in this cell"
		outcome.setup = ""
		return outcome
	}
	outcome.scored = true
	outcome.hit = chosen.Max >= best.Min
	for _, setup := range runnable[:min(3, len(runnable))] {
		if value := column.value(item.measured[setup]); setup != "" && value != nil && value.Max >= best.Min {
			outcome.top3 = true
		}
	}
	kept := math.Max(chosen.Median, current.Median)
	if best.Median > 0 && kept < best.Median && !outcome.hit {
		outcome.givenUp = (best.Median - kept) / best.Median
	}
	switch {
	case outcome.hit:
		outcome.result = "hit"
	default:
		outcome.result = fmt.Sprintf("miss, %s gives up %.1f%%", gymWords(item.measured, outcome.setup, item.current, column), outcome.givenUp*100)
	}
	return outcome
}

func gymWords(measured map[string]gymMeasured, chosen, current string, column gymColumn) string {
	if column.value(measured[current]).Median >= column.value(measured[chosen]).Median {
		return "keeping the current setup"
	}
	return chosen
}

// gymBest is the setup measured best in the cell by this column.
func gymBest(measured map[string]gymMeasured, column gymColumn) (*gymRange, string) {
	names := make([]string, 0, len(measured))
	for name := range measured {
		names = append(names, name)
	}
	sort.Strings(names)
	var best *gymRange
	bestName := ""
	for _, name := range names {
		value := column.value(measured[name])
		if value != nil && (best == nil || value.Median > best.Median) {
			best, bestName = value, name
		}
	}
	return best, bestName
}

// gymSummary is one table's score in one column.
type gymSummary struct {
	scored, hits, top3, unknown int
	givenUp                     []float64
}

func (s gymSummary) medianGivenUp() float64 {
	if len(s.givenUp) == 0 {
		return 0
	}
	values := append([]float64(nil), s.givenUp...)
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func (s gymSummary) words() string {
	return fmt.Sprintf("%d of %d hits, %d of %d with the best in the first three, median given up %.1f%%, %d unknown",
		s.hits, s.scored, s.top3, s.scored, s.medianGivenUp()*100, s.unknown)
}

// gymFloors are the scores the gym must not fall below: the score of the
// planner as committed. Raise a floor when a change lifts the score; never
// lower one to make a change pass.
var gymFloors = map[string]gymSummary{
	"map window/all output":                                 {hits: 37},
	"map window/output within a 1 s first-token limit":      {hits: 40},
	"product reading/all output":                            {hits: 9},
	"product reading/output within a 1 s first-token limit": {hits: 8},
}

func TestPlannerGym(t *testing.T) {
	// INFERCRANE_PLANNER_GYM_CASES scores another cases file, such as a screen of another
	// model; the floors hold for the committed file only.
	other := os.Getenv("INFERCRANE_PLANNER_GYM_CASES")
	file := loadGym(t)
	if other != "" {
		file = loadGymFile(t, other)
	}
	cases := gymCases(t, file)
	var lines []string
	lines = append(lines, "# Planner gym", "", file.Title+".", "Rules "+DiagnosisRulesVersion+". A hit is a first proposal inside the spread of the best measured setup; \"given up\" assumes the judge keeps the better of the proposal and the current setup.", "")
	summaries := map[string]*gymSummary{}
	for _, column := range gymColumns(file) {
		for _, table := range []string{"map window", "product reading"} {
			key := table + "/" + column.name
			summary := &gymSummary{}
			summaries[key] = summary
			builtFrom := &gymSummary{}
			lines = append(lines, "## "+table+", "+column.name, "", "| Case | Current setup | Rules built from it | First proposal it can run | Best measured | Result |", "|---|---|---|---|---|---|")
			for _, item := range cases {
				if item.table != table {
					continue
				}
				outcome := gymScore(t, file, item, column)
				// A setup the engine refuses to start is never proposed (package 4
				// of the optimizer plan: batch sizes come from the engine's blocks).
				for _, refused := range outcome.unstartable {
					t.Errorf("%s from %s: proposed a setup that cannot start: %s", item.name, item.current, refused)
				}
				target := summary
				if item.builtFrom != "" {
					target = builtFrom
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
				built := "no"
				if item.builtFrom != "" {
					built = "yes: " + item.builtFrom
				}
				lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s | %s |", item.name, item.current, built, outcome.proposal, outcome.best, outcome.result))
			}
			lines = append(lines, "", "Cells the rules were not built from: "+summary.words()+".", "Cells the rules were built from: "+builtFrom.words()+".", "")
			// The floor counts every case, built from or not.
			total := gymSummary{scored: summary.scored + builtFrom.scored, hits: summary.hits + builtFrom.hits, top3: summary.top3 + builtFrom.top3, unknown: summary.unknown + builtFrom.unknown,
				givenUp: append(append([]float64(nil), summary.givenUp...), builtFrom.givenUp...)}
			*summary = total
		}
	}
	if predictions := os.Getenv("INFERCRANE_PLANNER_GYM_PREDICTIONS"); predictions != "" {
		if other == "" {
			t.Fatal("INFERCRANE_PLANNER_GYM_PREDICTIONS scores a screen: set INFERCRANE_PLANNER_GYM_CASES to its cases file")
		}
		lines = append(lines, gymPredictionTables(t, file, predictions)...)
	}
	report := strings.Join(lines, "\n")
	t.Log("\n" + report)
	if out := os.Getenv("INFERCRANE_PLANNER_GYM_OUT"); out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, []byte(report+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if other != "" {
		return
	}
	for key, floor := range gymFloors {
		got := summaries[key]
		if got == nil {
			t.Fatalf("no gym table %q", key)
		}
		if got.hits < floor.hits {
			t.Errorf("%s: %d hits, below the floor of %d", key, got.hits, floor.hits)
		}
	}
}

// gymPredictions is the part of a predictions file (TestPlannerPredict) the
// gym scores: per cell and goal, the setups the proposals make, in order.
type gymPredictions struct {
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

// gymPredictionTables scores a predictions file as written against the cells
// of the cases file, by the same judge as the planner's answers now: the
// proposals were made before these cells were measured, and nothing here asks
// the planner again.
func gymPredictionTables(t *testing.T, file gymFile, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var predicted gymPredictions
	if err = json.Unmarshal(body, &predicted); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]gymCell{}
	for _, cell := range file.Cells {
		byKey[cell.Key] = cell
	}
	var lines []string
	for _, column := range gymColumns(file) {
		summary := &gymSummary{}
		lines = append(lines, "## predictions as written ("+predicted.RulesVersion+"), "+column.name, "", "| Case | Current setup | First proposal it can run | Best measured | Result |", "|---|---|---|---|---|")
		for _, cell := range predicted.Cells {
			measuredCell, found := byKey[cell.Key]
			item := gymCase{table: "predictions", name: cell.Key, current: cell.Current, measured: measuredCell.Measured}
			var outcome gymOutcome
			switch {
			case !found:
				outcome = gymOutcome{proposal: "-", result: "unknown: the screen has no such cell"}
			default:
				var runnable []string
				firstWhy := ""
				for _, answer := range cell.Answers {
					if answer.Objective != column.objective {
						continue
					}
					for _, proposal := range answer.Proposals {
						if strings.HasPrefix(proposal.Measured, "cannot start") || proposal.Measured == "no change" {
							continue
						}
						if len(runnable) == 0 {
							outcome.proposal, firstWhy = proposal.Label, proposal.Measured
						}
						runnable = append(runnable, proposal.Setup)
					}
				}
				if len(runnable) == 0 {
					outcome.proposal, runnable = "nothing this GPU can run", []string{cell.Current}
				}
				outcome = gymJudge(item, column, outcome, runnable, firstWhy)
			}
			switch {
			case outcome.scored:
				summary.scored++
				if outcome.hit {
					summary.hits++
				}
				if outcome.top3 {
					summary.top3++
				}
				summary.givenUp = append(summary.givenUp, outcome.givenUp)
			default:
				summary.unknown++
			}
			lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s |", cell.Key, cell.Current, outcome.proposal, outcome.best, outcome.result))
		}
		lines = append(lines, "", "Predictions: "+summary.words()+".", "")
	}
	return lines
}

func TestPlannerGymScoresPredictionsAsWritten(t *testing.T) {
	// A screen's cells as build_cases.py writes them: each range runs from the value up to the
	// value plus the current setup's two-run difference (4% here).
	measured := func(value float64) string {
		return fmt.Sprintf(`{"median": %[1]g, "min": %[1]g, "max": %[2]g}`, value, value*1.04)
	}
	cell := func(key string, setups map[string]float64) string {
		var parts []string
		for _, name := range []string{"defaults", "fp8-weights", "fp8-kv"} {
			if value, ok := setups[name]; ok {
				parts = append(parts, fmt.Sprintf(`%q: {"output": %s, "useful_output": %s}`, name, measured(value), measured(value)))
			}
		}
		return fmt.Sprintf(`{"key": %q, "measured": {%s}}`, key, strings.Join(parts, ", "))
	}
	cases := `{"title": "screen", "gpu": "L4", "first_token_limit_ms": 1000, "current_setups": ["defaults"], "cells": [` +
		cell("short/16", map[string]float64{"defaults": 100, "fp8-weights": 103, "fp8-kv": 101}) + `, ` +
		cell("output/16", map[string]float64{"defaults": 100, "fp8-weights": 110}) + `, ` +
		cell("long/16", map[string]float64{"defaults": 100, "fp8-kv": 120}) + `]}`
	answers := func(proposals string) string {
		return fmt.Sprintf(`[{"objective": "throughput", "proposals": %[1]s}, {"objective": "useful-output", "proposals": %[1]s}]`, proposals)
	}
	predictions := `{"rules_version": "v-test", "cells": [
		{"key": "short/16", "current": "defaults", "answers": ` + answers(`[{"label": "FP8 KV cache", "setup": "fp8-kv"}, {"label": "FP8 weights", "setup": "fp8-weights"}]`) + `},
		{"key": "output/16", "current": "defaults", "answers": ` + answers(`[{"label": "Bigger batch", "setup": "defaults", "measured": "no change"}]`) + `},
		{"key": "long/16", "current": "defaults", "answers": ` + answers(`[{"label": "FP8 weights", "setup": "", "measured": "unmeasured: FP8 weights at long/16"}]`) + `}]}`
	dir := t.TempDir()
	casesPath, predictionsPath := filepath.Join(dir, "cases.json"), filepath.Join(dir, "predictions.json")
	for path, body := range map[string]string{casesPath: cases, predictionsPath: predictions} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	report := strings.Join(gymPredictionTables(t, loadGymFile(t, casesPath), predictionsPath), "\n")
	for _, want := range []string{
		// FP8 KV cache (101) is first: fp8-weights (103) is not ahead of it by more than 4%.
		"| short/16 | defaults | FP8 KV cache | fp8-weights | hit |",
		// Nothing runnable keeps the current setup, 10% behind.
		"| output/16 | defaults | nothing this GPU can run | fp8-weights | miss, keeping the current setup gives up 9.1% |",
		"| long/16 | defaults | FP8 weights | fp8-kv | unknown, unmeasured: FP8 weights at long/16 |",
		"Predictions: 1 of 2 hits, 1 of 2 with the best in the first three, median given up 4.5%, 1 unknown.",
	} {
		if strings.Count(report, want) != 2 {
			t.Errorf("want twice, once per goal: %q\n%s", want, report)
		}
	}
}

// TestPlannerPredict writes the planner's proposals and the cache room it
// predicts for every cell of a cases file, before any other setup is measured
// there (step 2 of the optimizer plan). It runs only when asked:
//
//	INFERCRANE_PLANNER_PREDICT_CASES=cases.json INFERCRANE_PLANNER_PREDICT_OUT=predictions.json \
//	  go test -count=1 -run '^TestPlannerPredict$' ./internal/optimizer
func TestPlannerPredict(t *testing.T) {
	casesPath, out := os.Getenv("INFERCRANE_PLANNER_PREDICT_CASES"), os.Getenv("INFERCRANE_PLANNER_PREDICT_OUT")
	if casesPath == "" || out == "" {
		t.Skip("set INFERCRANE_PLANNER_PREDICT_CASES and INFERCRANE_PLANNER_PREDICT_OUT")
	}
	file := loadGymFile(t, casesPath)
	type proposalView struct {
		Technique string   `json:"technique"`
		Label     string   `json:"label"`
		Priority  int      `json:"priority"`
		Args      []string `json:"args"`
		Setup     string   `json:"setup"`
		Measured  string   `json:"measured,omitempty"`
		Reason    string   `json:"reason"`
	}
	type answer struct {
		Objective         string             `json:"objective"`
		PrimaryBottleneck string             `json:"primary_bottleneck"`
		Summary           string             `json:"summary"`
		Proposals         []proposalView     `json:"proposals"`
		Skipped           map[string]string  `json:"skipped"`
		CacheRoom         *CacheRoom         `json:"cache_room,omitempty"`
		Readings          map[string]float64 `json:"readings"`
	}
	type cellView struct {
		Key     string   `json:"key"`
		Current string   `json:"current"`
		Answers []answer `json:"answers"`
	}
	document := map[string]any{"schema": "infercrane.planner-predictions/v1", "rules_version": DiagnosisRulesVersion, "model": file.Model, "gpu": file.GPU,
		"note": "Written from the current setup's measured server before any other setup was measured. A proposal is what the planner would measure; it is not a result."}
	var cells []cellView
	for _, cell := range file.Cells {
		for current, measured := range cell.Measured {
			diagnosis := gymDiagnosis(file, file.Setups[current], measured)
			view := cellView{Key: cell.Key, Current: current}
			for _, column := range gymColumns(file) {
				request := column.request(gymRequest(file, diagnosis, current))
				proposal, err := catalogSource(t).Propose(context.Background(), request)
				if err != nil {
					t.Fatalf("%s: %v", cell.Key, err)
				}
				given := answer{Objective: request.Objective, PrimaryBottleneck: proposal.Diagnosis.PrimaryBottleneck, Summary: proposal.Diagnosis.Summary,
					CacheRoom: proposal.Diagnosis.CacheRoom, Skipped: map[string]string{}, Readings: diagnosis.Signals}
				control := proposal.Candidates[0].Deployment.Runtime.Args
				for _, candidate := range proposal.Candidates[1:] {
					if candidate.Reason == nil || candidate.Reason.Role != ReasonDiagnosis {
						continue
					}
					setup, why := gymChange(file, current, control, candidate.Deployment.Runtime.Args)
					given.Proposals = append(given.Proposals, proposalView{Technique: candidate.Reason.Technique, Label: candidate.Reason.Label, Priority: candidate.Reason.Priority,
						Args: candidate.Deployment.Runtime.Args, Setup: setup, Measured: why, Reason: candidate.Reason.Summary})
				}
				for _, technique := range proposal.Diagnosis.Techniques {
					if technique.Skipped != "" {
						given.Skipped[technique.Technique] = technique.Skipped
					}
				}
				view.Answers = append(view.Answers, given)
			}
			cells = append(cells, view)
		}
	}
	document["cells"] = cells
	encoded, err := json.MarshalIndent(document, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(out, append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s: %d cells", out, len(cells))
}
