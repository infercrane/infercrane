// Command judge reads a trained-draft run (tools/trained-draft) and gives the
// product's own verdicts on it: the campaign judge on each measured window
// (optimizationcampaign.JudgeRecordedWindow), the task-set rule on the answers
// (qualitycheck.Grade and PairedTask), and the core's check of the training
// report (speculator.DecodeReport). For the trained draft at every load it
// also writes the technique contract's record (technique.Result), carrying the
// draft-acceptance evidence its manifest requires, and that record's claim as
// the manifest judges it. It prints one JSON document.
//
//	go run ./tools/trained-draft/judge .infercrane/trained-draft/RUN_ID
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/speculator"
	"github.com/infercrane/infercrane/internal/technique"
)

const recordSchema = "infercrane.trained-draft-measure/v2"

// The measure stage writes these beside measure.json: acceptance read from
// vLLM's speculation counters (the draft-acceptance evidence) and the
// content-free traffic shape (held-out prompt ids and sizes, in order).
const (
	acceptanceFile = "acceptance.json"
	shapeFile      = "traffic-shape.json"
	trainedSetup   = "trained"
	// The confirm stage's record: the trained draft against the control again,
	// timed runs only, in a container other than the measure stage's.
	confirmFile   = "confirm.json"
	confirmSchema = "infercrane.trained-draft-confirm/v1"
)

type recordedRun struct {
	optimizationcampaign.RunSummary
	Setup string `json:"setup"`
}

type acceptance struct {
	Drafts            float64   `json:"drafts"`
	MeanTokensPerStep float64   `json:"mean_tokens_per_step"`
	PerPosition       []float64 `json:"per_position"`
}

type recordedWindow struct {
	Control    string   `json:"control"`
	Candidates []string `json:"candidates"`
	Loads      []struct {
		Concurrency    int           `json:"concurrency"`
		RequestsPerRun int           `json:"requests_per_run"`
		Runs           []recordedRun `json:"runs"`
	} `json:"loads"`
	Acceptance        map[string]map[string]*acceptance `json:"acceptance"`
	HeldOutAcceptance map[string]*acceptance            `json:"held_out_acceptance"`
	Skipped           []string                          `json:"skipped"`
	SameAnswers       struct {
		Prompts int                  `json:"prompts"`
		Answers map[string][]*string `json:"answers"`
	} `json:"same_answers"`
	Tasks *struct {
		Items []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
			Gold string `json:"gold"`
		} `json:"items"`
		Answers map[string][]*string `json:"answers"`
		// Rule is how the comparison is read (qualitycheck.RuleWithinMargin
		// or RuleHeld); Passes are further servers of each setup answering
		// the same items, pooled with the first.
		Rule   string                 `json:"rule"`
		Passes []map[string][]*string `json:"passes"`
	} `json:"tasks"`
}

type record struct {
	Schema  string           `json:"schema"`
	Plan    string           `json:"plan"`
	Rules   string           `json:"rules"`
	Windows []recordedWindow `json:"windows"`
	// What the contract record needs: the engine as "vllm 0.34.0", the image
	// by digest, the GPU, the harness commit (unknown when the tree was
	// dirty), the weights' and cache's precision, and the launch command.
	Engine    string              `json:"engine"`
	Image     string              `json:"image"`
	GPU       string              `json:"gpu"`
	Commit    string              `json:"commit"`
	Dirty     bool                `json:"dirty"`
	Precision technique.Precision `json:"precision"`
	Command   string              `json:"command"`
	Machine   machine             `json:"machine"`
	// Confirms is, on a confirm record, the sha256 of the measure record it
	// measures again.
	Confirms string `json:"confirms,omitempty"`
}

// machine is where a stage ran: the container, the host and the GPU's UUID.
type machine struct {
	TaskID   string `json:"task_id"`
	Hostname string `json:"hostname"`
	GPUUUID  string `json:"gpu_uuid"`
}

type loadVerdict struct {
	Concurrency int                                              `json:"concurrency"`
	Requests    int                                              `json:"requests_per_run,omitempty"`
	Invalid     map[string]string                                `json:"invalid,omitempty"`
	Throughput  map[string]optimizationcampaign.MetricComparison `json:"output_token_throughput,omitempty"`
	TPOT        map[string]optimizationcampaign.MetricComparison `json:"tpot_p95_ms,omitempty"`
	TTFT        map[string]optimizationcampaign.MetricComparison `json:"ttft_p95_ms,omitempty"`
	Acceptance  map[string]*acceptance                           `json:"acceptance,omitempty"`
}

type windowVerdict struct {
	Control           string                                 `json:"control"`
	Candidates        []string                               `json:"candidates"`
	Loads             []loadVerdict                          `json:"loads"`
	HeldOutAcceptance map[string]*acceptance                 `json:"held_out_acceptance,omitempty"`
	SameAnswers       map[string]any                         `json:"same_answers"`
	Tasks             map[string]qualitycheck.TaskComparison `json:"tasks,omitempty"`
	Skipped           []string                               `json:"skipped,omitempty"`
}

type verdict struct {
	Run     string            `json:"run"`
	Plan    string            `json:"plan"`
	Rules   string            `json:"rules"`
	Report  map[string]any    `json:"report"`
	Windows []windowVerdict   `json:"windows"`
	PassBar map[string]string `json:"pass_bar"`
	Results []contractRecord  `json:"results,omitempty"`
	// Confirmation is the confirm stage's window, judged the same way, when
	// the run has one.
	Confirmation *confirmation `json:"confirmation,omitempty"`
}

// confirmation is a confirm record as the judge read it: whether it measures
// this run's measure record again, whether it ran on another GPU, and its
// window's verdict.
type confirmation struct {
	Digest       string          `json:"digest"`
	SameMeasure  bool            `json:"confirms_this_measure"`
	FreshMachine bool            `json:"fresh_machine"`
	Why          string          `json:"why,omitempty"`
	Windows      []windowVerdict `json:"windows"`
}

// contractRecord is the technique contract's record of the trained draft at
// one load, with the claim its manifest gives it (evidence included) and the
// fields it lacks.
type contractRecord struct {
	technique.Result
	Claim   string   `json:"claim"`
	Unknown []string `json:"unknown,omitempty"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: judge RUN_DIR")
		os.Exit(2)
	}
	out, err := judge(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoded, _ := json.MarshalIndent(out, "", " ")
	fmt.Println(string(encoded))
}

func judge(dir string) (verdict, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "measure.json"))
	if err != nil {
		return verdict{}, err
	}
	var measured record
	if err := json.Unmarshal(raw, &measured); err != nil {
		return verdict{}, fmt.Errorf("measure.json: %w", err)
	}
	if measured.Schema != recordSchema {
		return verdict{}, fmt.Errorf("measure.json schema %q is not %s", measured.Schema, recordSchema)
	}
	out := verdict{Run: filepath.Base(dir), Plan: measured.Plan, Rules: measured.Rules, Report: reportCheck(filepath.Join(dir, "report.json")), PassBar: map[string]string{}}
	for _, window := range measured.Windows {
		out.Windows = append(out.Windows, judgeWindow(window))
	}
	confirmed, err := readConfirmation(dir, measured)
	if err != nil {
		return verdict{}, err
	}
	out.Confirmation = confirmed
	out.PassBar = passBar(out)
	for key, line := range rungBar(out, measured) {
		out.PassBar[key] = line
	}
	for key, line := range confirmationBar(out) {
		out.PassBar[key] = line
	}
	out.Results = results(dir, measured, out.Windows, confirmed)
	return out, nil
}

// judgeWindow gives one recorded window the campaign judge's verdict at every
// load, the identical-answer shares and the task rule.
func judgeWindow(window recordedWindow) windowVerdict {
	judged := windowVerdict{Control: window.Control, Candidates: window.Candidates, HeldOutAcceptance: window.HeldOutAcceptance, Skipped: window.Skipped,
		SameAnswers: sameAnswers(window), Tasks: tasks(window)}
	for _, level := range window.Loads {
		loadOut := loadVerdict{Concurrency: level.Concurrency, Requests: level.RequestsPerRun, Invalid: map[string]string{}, Throughput: map[string]optimizationcampaign.MetricComparison{},
			TPOT: map[string]optimizationcampaign.MetricComparison{}, TTFT: map[string]optimizationcampaign.MetricComparison{}, Acceptance: map[string]*acceptance{}}
		for _, candidate := range window.Candidates {
			var runs []optimizationcampaign.RunSummary
			for _, run := range level.Runs {
				if run.Arm == "control" || run.Setup == candidate {
					runs = append(runs, run.RunSummary)
				}
			}
			compared, invalid := optimizationcampaign.JudgeRecordedWindow(runs, len(window.Candidates))
			if invalid != "" {
				loadOut.Invalid[candidate] = invalid
				continue
			}
			loadOut.Throughput[candidate], loadOut.TPOT[candidate], loadOut.TTFT[candidate] = compared["output_token_throughput"], compared["tpot_p95_ms"], compared["ttft_p95_ms"]
		}
		for name, byLoad := range window.Acceptance {
			loadOut.Acceptance[name] = byLoad[fmt.Sprint(level.Concurrency)]
		}
		judged.Loads = append(judged.Loads, loadOut)
	}
	return judged
}

// readConfirmation reads the confirm stage's record when the run has one. It
// counts only when it measured this run's measure record again (by sha256)
// and ran on another GPU than the measure stage, both GPUs named; otherwise
// Why says what is missing and no confirmation is attached.
func readConfirmation(dir string, measured record) (*confirmation, error) {
	raw, err := os.ReadFile(filepath.Join(dir, confirmFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var confirm record
	if err := json.Unmarshal(raw, &confirm); err != nil {
		return nil, fmt.Errorf("%s: %w", confirmFile, err)
	}
	if confirm.Schema != confirmSchema {
		return nil, fmt.Errorf("%s schema %q is not %s", confirmFile, confirm.Schema, confirmSchema)
	}
	out := &confirmation{Digest: fileDigest(filepath.Join(dir, confirmFile)), SameMeasure: confirm.Confirms != "" && confirm.Confirms == fileDigest(filepath.Join(dir, "measure.json"))}
	for _, window := range confirm.Windows {
		out.Windows = append(out.Windows, judgeWindow(window))
	}
	first, second := measured.Machine.GPUUUID, confirm.Machine.GPUUUID
	out.FreshMachine = first != "" && second != "" && first != second
	switch {
	case !out.SameMeasure:
		out.Why = "it does not name this run's measure record"
	case first == "" || second == "":
		out.Why = "a stage did not record its GPU, so a fresh machine is not established"
	case !out.FreshMachine:
		out.Why = "it ran on the measure stage's GPU (" + first + ")"
	}
	return out, nil
}

// confirmedAt is the confirm window's verdict for the trained draft against
// control at a load, when the confirmation counts.
func (c *confirmation) confirmedAt(control string, load int) (optimizationcampaign.MetricComparison, string, bool) {
	if c == nil {
		return optimizationcampaign.MetricComparison{}, "no confirm window", false
	}
	if c.Why != "" {
		return optimizationcampaign.MetricComparison{}, c.Why, false
	}
	for _, window := range c.Windows {
		if window.Control != control {
			continue
		}
		for _, level := range window.Loads {
			if level.Concurrency != load {
				continue
			}
			if reason, invalid := level.Invalid[trainedSetup]; invalid {
				return optimizationcampaign.MetricComparison{}, "the confirm window was void: " + reason, false
			}
			comparison, ok := level.Throughput[trainedSetup]
			if !ok {
				return optimizationcampaign.MetricComparison{}, "the confirm window did not measure the trained draft", false
			}
			if comparison.Decision != optimizationcampaign.DecisionImproved {
				return comparison, "the fresh machine measured " + decisionWords[comparison.Decision], false
			}
			return comparison, "", true
		}
	}
	return optimizationcampaign.MetricComparison{}, fmt.Sprintf("the confirm window has no load %d against %s", load, control), false
}

// confirmationBar states, per load of the trained draft's window, whether a
// fresh machine measured the gain again.
func confirmationBar(out verdict) map[string]string {
	bar := map[string]string{}
	for _, window := range out.Windows {
		if !slices.Contains(window.Candidates, trainedSetup) {
			continue
		}
		for _, level := range window.Loads {
			key := fmt.Sprintf("fresh_machine_at_%d", level.Concurrency)
			comparison, why, ok := out.Confirmation.confirmedAt(window.Control, level.Concurrency)
			if ok {
				bar[key] = fmt.Sprintf("confirmed: a fresh machine measured the trained draft faster than %s (%+.1f%%, noise floor %.1f%%)", window.Control, comparison.Delta*100, comparison.NoiseFloor*100)
				continue
			}
			bar[key] = "not confirmed: " + why
		}
	}
	return bar
}

// results writes the contract's record of the trained draft at every load of
// every window it was measured in. The draft-acceptance item is the sha256 of
// acceptance.json and is attached only where vLLM's counters gave the trained
// draft an acceptance at that load; whatever the run did not record stays
// empty, so Unknown names it and the claim is "not shown".
func results(dir string, measured record, windows []windowVerdict, confirmed *confirmation) []contractRecord {
	manifest := speculator.Technique{}.Manifest()
	acceptanceDigest, shapeDigest := fileDigest(filepath.Join(dir, acceptanceFile)), fileDigest(filepath.Join(dir, shapeFile))
	engine, engineVersion, _ := strings.Cut(measured.Engine, " ")
	_, image, _ := strings.Cut(measured.Image, "@")
	commit := measured.Commit
	if measured.Dirty {
		commit = ""
	}
	shape := ""
	if shapeDigest != "" {
		shape = shapeFile
	}
	var out []contractRecord
	for _, window := range windows {
		if !slices.Contains(window.Candidates, trainedSetup) {
			continue
		}
		for _, level := range window.Loads {
			result := technique.Result{
				SchemaVersion: technique.ResultSchemaV1, Technique: manifest.ID, TechniqueVersion: manifest.Version, Candidate: trainedSetup,
				Hardware:         technique.Hardware{Accelerator: measured.GPU, Count: 1},
				Software:         technique.Software{Engine: engine, EngineVersion: engineVersion, ImageDigest: image},
				Workload:         technique.Workload{ShapeDigest: shapeDigest, Kind: "chat", Load: level.Concurrency, Requests: level.Requests},
				Precision:        measured.Precision,
				Baseline:         technique.Baseline{Setup: window.Control, Basis: "same-machine"},
				Correctness:      correctness(window),
				Measured:         outcome(level),
				Reproduction:     technique.Reproduction{Command: measured.Command, Commit: commit, ImageDigest: image, TrafficShape: shape},
				RequiredEvidence: slices.Clone(manifest.Evidence),
			}
			if accepted := level.Acceptance[trainedSetup]; accepted != nil && acceptanceDigest != "" {
				result.Evidence = []technique.EvidenceItem{{
					Name: speculator.EvidenceDraftAcceptance, Digest: acceptanceDigest,
					Summary: fmt.Sprintf("%.2f tokens a verify step over %.0f drafts at %d at once, from vLLM's speculation counters", accepted.MeanTokensPerStep, accepted.Drafts, level.Concurrency),
				}}
			}
			// A fresh machine's window that measured the gain again is the
			// confirmation; the gain then counts at the smaller of the two
			// changes against the larger noise floor.
			if again, _, ok := confirmed.confirmedAt(window.Control, level.Concurrency); ok && result.Measured.Decision == technique.DecisionImproved {
				result.Measured.Change = min(result.Measured.Change, again.Delta)
				result.Measured.NoiseFloor = max(result.Measured.NoiseFloor, again.NoiseFloor)
				result.Baseline.Basis = "same-machine+fresh-servers"
				result.Evidence = append(result.Evidence, technique.EvidenceItem{
					Name: technique.EvidenceFreshMachineConfirmation, Digest: confirmed.Digest,
					Summary: fmt.Sprintf("another GPU measured %+.1f%% against %s at %d at once (noise floor %.1f%%)", again.Delta*100, window.Control, level.Concurrency, again.NoiseFloor*100),
				})
			}
			out = append(out, contractRecord{Result: result, Claim: manifest.ClaimOf(result), Unknown: result.Unknown()})
		}
	}
	return out
}

// correctness is the task rule when the plan has a task set, else identical
// greedy answers within identityMargin of the control's own second pass.
func correctness(window windowVerdict) technique.Correctness {
	if window.Tasks != nil {
		verdict := technique.DecisionUnknown
		if comparison, ok := window.Tasks[trainedSetup]; ok {
			switch comparison.Verdict {
			case qualitycheck.TaskNoDifference, qualitycheck.TaskBetter, qualitycheck.TaskHeld:
				verdict = "passed"
			case qualitycheck.TaskWorse:
				verdict = "failed"
			}
		}
		return technique.Correctness{Method: "task-score", Verdict: verdict}
	}
	_, floor := window.SameAnswers[window.Control+"_again"].(map[string]any)
	_, mine := window.SameAnswers[trainedSetup].(map[string]any)
	if !floor || !mine {
		return technique.Correctness{Method: "answer-agreement", Verdict: technique.DecisionUnknown}
	}
	return technique.Correctness{Method: "answer-agreement", Verdict: map[bool]string{true: "passed", false: "failed"}[identityHeld(window, trainedSetup)]}
}

// outcome is the campaign judge's throughput decision for the trained draft,
// unknown when its window was void or it was not measured.
func outcome(level loadVerdict) technique.Measured {
	comparison, ok := level.Throughput[trainedSetup]
	if _, invalid := level.Invalid[trainedSetup]; invalid || !ok {
		return technique.Measured{Metric: "output_token_throughput", Decision: technique.DecisionUnknown}
	}
	decision := map[string]string{
		optimizationcampaign.DecisionImproved: technique.DecisionImproved, optimizationcampaign.DecisionRegressed: technique.DecisionWorse,
		optimizationcampaign.DecisionNoMeasurableDifference: technique.DecisionSame,
	}[comparison.Decision]
	if decision == "" {
		decision = technique.DecisionUnknown
	}
	return technique.Measured{Metric: "output_token_throughput", Change: comparison.Delta, NoiseFloor: comparison.NoiseFloor, Decision: decision}
}

// fileDigest is "sha256:<hex>" of a file, or empty when it cannot be read.
func fileDigest(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func reportCheck(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{"valid": false, "error": err.Error()}
	}
	report, err := speculator.DecodeReport(raw)
	if err != nil {
		return map[string]any{"valid": false, "error": err.Error()}
	}
	out := map[string]any{"valid": true, "artifact_digest": report.ArtifactDigest, "examples": report.Examples, "training_tokens": report.TrainingTokens, "dataset_fingerprint": report.DatasetFingerprint}
	if expected, ok := report.Training.ExpectedTokensPerStep(report.DraftTokens); ok {
		out["expected_tokens_per_step"] = expected
	}
	start := speculator.Training{HeldOutAcceptance: report.StartAcceptance}
	if expected, ok := start.ExpectedTokensPerStep(report.DraftTokens); ok {
		out["start_expected_tokens_per_step"] = expected
	}
	return out
}

// sameAnswers is the share of prompts whose answer at temperature 0 is the
// same text as the control's, for every other label (the control's second
// pass is the floor numerics alone give).
func sameAnswers(window recordedWindow) map[string]any {
	reference := window.SameAnswers.Answers[window.Control]
	out := map[string]any{"prompts": window.SameAnswers.Prompts, "reference": window.Control}
	for label, answers := range window.SameAnswers.Answers {
		if label == window.Control {
			continue
		}
		same, compared := 0, 0
		for index := range min(len(reference), len(answers)) {
			if reference[index] == nil || answers[index] == nil {
				continue
			}
			compared++
			if *reference[index] == *answers[index] {
				same++
			}
		}
		if compared > 0 {
			out[label] = map[string]any{"identical": same, "compared": compared, "share": float64(same) / float64(compared)}
		}
	}
	return out
}

// tasks scores every arm's answers with the product's answer_line grading and
// compares each with the control item by item (qualitycheck.PairedTask). A
// failed request scores 0, as a wrong answer would.
func tasks(window recordedWindow) map[string]qualitycheck.TaskComparison {
	if window.Tasks == nil {
		return nil
	}
	score := func(answers []*string) []float64 {
		scores := make([]float64, len(window.Tasks.Items))
		for index, item := range window.Tasks.Items {
			if index < len(answers) && answers[index] != nil {
				scores[index] = qualitycheck.Grade(qualitycheck.GradingAnswerLine, qualitycheck.Prompt{ID: item.ID, Expected: []string{item.Gold}}, *answers[index])
			}
		}
		return scores
	}
	out := map[string]qualitycheck.TaskComparison{}
	for label, answers := range window.Tasks.Answers {
		if label == window.Control {
			continue
		}
		// Each server pair's item-level differences are pooled: the first
		// pass, then every later pass in which both setups answered.
		reference, candidate := score(window.Tasks.Answers[window.Control]), score(answers)
		servers := 1
		for _, pass := range window.Tasks.Passes {
			mine, ok := pass[label]
			theirs, okControl := pass[window.Control]
			if !ok || !okControl {
				continue
			}
			reference, candidate = append(reference, score(theirs)...), append(candidate, score(mine)...)
			servers++
		}
		comparison := qualitycheck.PairedTask(reference, candidate)
		comparison.Items, comparison.ServersPerSide = len(window.Tasks.Items), servers
		if window.Tasks.Rule == qualitycheck.RuleHeld {
			comparison = qualitycheck.RequireHeld(comparison)
		}
		out[label] = comparison
	}
	return out
}

// taskWords is a task comparison in words: its verdict, the rule, the
// interval in points, and how many servers of each setup answered.
func taskWords(task qualitycheck.TaskComparison) string {
	verdict := map[string]string{
		qualitycheck.TaskBetter: "better", qualitycheck.TaskWorse: "worse", qualitycheck.TaskNoDifference: "no measurable difference",
		qualitycheck.TaskNotSettled: "not settled", qualitycheck.TaskHeld: "held within 1 point", qualitycheck.TaskNotShownHeld: "not shown to hold within 1 point",
	}[task.Verdict]
	return fmt.Sprintf("%s, %.3f to %.3f, interval %+.1f to %+.1f points, %d items on %d server(s) each", verdict, task.ReferenceScore, task.CandidateScore, task.Low*100, task.High*100, task.Items, task.ServersPerSide)
}

var decisionWords = map[string]string{
	optimizationcampaign.DecisionImproved: "faster", optimizationcampaign.DecisionRegressed: "slower", optimizationcampaign.DecisionNoMeasurableDifference: "no measurable difference",
}

// identityMargin is how far below the control's own second pass a candidate's
// share of identical greedy answers may fall and still count as answers that
// held, when a plan has no task set (rung 2's rules).
const identityMargin = 0.10

// identityHeld compares a candidate's identical-answer share with the floor
// numerics alone give: the control answering the same prompts twice.
func identityHeld(window windowVerdict, candidate string) bool {
	share := func(label string) (float64, bool) {
		entry, ok := window.SameAnswers[label].(map[string]any)
		if !ok {
			return 0, false
		}
		value, ok := entry["share"].(float64)
		return value, ok
	}
	floor, okFloor := share(window.Control + "_again")
	mine, okMine := share(candidate)
	return okFloor && okMine && mine >= floor-identityMargin
}

// acceptanceMargin is how many more tokens a verify step the trained draft
// must write than the published draft it started from, on the held-out
// prompts, from vLLM's counters (rung 2's rules).
const acceptanceMargin = 0.05

// rungBar states the rung's pass bar as its rules write it, one line per
// check, so a partial pass reads as partial: validity, training ran,
// acceptance moved, faster than the window's control at the lowest load,
// and answers held. "bar_climb" passes only when all five pass.
func rungBar(out verdict, measured record) map[string]string {
	bar := map[string]string{}
	failed, voids := 0, []string{}
	for index, window := range measured.Windows {
		for _, level := range window.Loads {
			for _, run := range level.Runs {
				failed += run.Failed
			}
		}
		for _, level := range out.Windows[index].Loads {
			for candidate, reason := range level.Invalid {
				voids = append(voids, fmt.Sprintf("%s at %d: %s", candidate, level.Concurrency, reason))
			}
		}
		for _, skipped := range window.Skipped {
			voids = append(voids, "skipped "+skipped)
		}
	}
	sort.Strings(voids)
	bar["bar_validity"] = "pass (0 failed requests, no void or skipped measurement)"
	if failed > 0 || len(voids) > 0 {
		bar["bar_validity"] = fmt.Sprintf("fail (%d failed requests; %s)", failed, strings.Join(voids, "; "))
	}
	var drafts *windowVerdict
	for index := range out.Windows {
		if slices.Contains(out.Windows[index].Candidates, trainedSetup) {
			drafts = &out.Windows[index]
			break
		}
	}
	valid, _ := out.Report["valid"].(bool)
	served := drafts != nil && drafts.HeldOutAcceptance[trainedSetup] != nil
	switch {
	case valid && served:
		bar["bar_training_ran"] = "pass (the training report is valid and the trained draft served)"
	case !valid:
		bar["bar_training_ran"] = fmt.Sprint("fail (training report: ", out.Report["error"], ")")
	default:
		bar["bar_training_ran"] = "fail (the trained draft did not serve the held-out prompts)"
	}
	bar["bar_acceptance_moved"] = "unknown (no held-out acceptance for both the trained and the published draft)"
	if drafts != nil {
		trained, published := drafts.HeldOutAcceptance[trainedSetup], drafts.HeldOutAcceptance["published"]
		if trained != nil && published != nil {
			gain := trained.MeanTokensPerStep - published.MeanTokensPerStep
			bar["bar_acceptance_moved"] = fmt.Sprintf("%s (%+.3f tokens a verify step: trained %.3f against published %.3f over %.0f and %.0f drafts, vLLM's counters; bar +%.2f)",
				map[bool]string{true: "pass", false: "fail"}[gain >= acceptanceMargin], gain, trained.MeanTokensPerStep, published.MeanTokensPerStep, trained.Drafts, published.Drafts, acceptanceMargin)
		}
	}
	bar["bar_faster"] = "unknown (the trained draft was not measured)"
	if drafts != nil && len(drafts.Loads) > 0 {
		level := drafts.Loads[0]
		for _, other := range drafts.Loads {
			if other.Concurrency < level.Concurrency {
				level = other
			}
		}
		label := fmt.Sprintf("the trained draft against %s at %d at once", drafts.Control, level.Concurrency)
		comparison, measuredHere := level.Throughput[trainedSetup]
		reason, invalid := level.Invalid[trainedSetup]
		switch {
		case invalid:
			bar["bar_faster"] = fmt.Sprintf("unknown: %s (%s)", label, reason)
		case !measuredHere:
			bar["bar_faster"] = fmt.Sprintf("unknown: %s (not measured)", label)
		default:
			bar["bar_faster"] = fmt.Sprintf("%s: %s: %s (%+.1f%%, noise floor %.1f%%)", map[bool]string{true: "pass", false: "fail"}[comparison.Decision == optimizationcampaign.DecisionImproved],
				label, decisionWords[comparison.Decision], comparison.Delta*100, comparison.NoiseFloor*100)
		}
	}
	bar["bar_answers_held"] = "unknown (no identical-answer pass or task set for the trained draft)"
	if drafts != nil {
		held := correctness(*drafts)
		identical := ""
		if entry, ok := drafts.SameAnswers[trainedSetup].(map[string]any); ok {
			floor, _ := drafts.SameAnswers[drafts.Control+"_again"].(map[string]any)
			identical = fmt.Sprintf("identical greedy answers %v of %v for the trained draft, %v of %v for %s's own second pass", entry["identical"], entry["compared"], floor["identical"], floor["compared"], drafts.Control)
		}
		switch {
		case held.Method == "task-score" && held.Verdict == "passed":
			bar["bar_answers_held"] = "pass (task-score: " + taskWords(drafts.Tasks[trainedSetup]) + " against " + drafts.Control + ")"
			if identical != "" {
				// Quality held by the task rule; whether the greedy text stayed the same is its own fact.
				bar["bar_answers_held"] += "; " + identical
			}
		case held.Method == "task-score" && held.Verdict == "failed":
			bar["bar_answers_held"] = "fail (task-score: worse than " + drafts.Control + ")"
		case held.Method == "task-score":
			bar["bar_answers_held"] = "unknown (task-score: " + taskWords(drafts.Tasks[trainedSetup]) + ")"
		case held.Verdict == "passed":
			// Agreement of greedy text shows no sign of changed answers; it cannot show quality.
			bar["bar_answers_held"] = "pass: no sign of changed answers (" + identical + "); quality not shown, since identical-answer agreement cannot carry a quality claim"
		case held.Verdict == "failed":
			bar["bar_answers_held"] = "fail: answers changed more than numerics alone change them (" + identical + ")"
		}
	}
	climb := true
	for _, key := range []string{"bar_validity", "bar_training_ran", "bar_acceptance_moved", "bar_faster", "bar_answers_held"} {
		climb = climb && strings.HasPrefix(bar[key], "pass")
	}
	bar["bar_climb"] = map[bool]string{true: "pass: every check of the rung's bar passed", false: "not passed: see the bar_ lines"}[climb]
	return bar
}

// passBar states, for every candidate at every load, the judge's decision
// against its window's control in words; quality only when the task rule
// settles it; and for the trained draft a headline that passes only when the
// judge says faster and answers held: by the task rule when the plan has a
// task set, else by identical greedy answers within identityMargin of the
// control's own second pass.
func passBar(out verdict) map[string]string {
	bar := map[string]string{}
	if valid, _ := out.Report["valid"].(bool); valid {
		bar["training_report"] = "valid"
	} else {
		bar["training_report"] = fmt.Sprint("invalid: ", out.Report["error"])
	}
	for _, window := range out.Windows {
		quality := map[string]string{}
		for candidate, comparison := range window.Tasks {
			switch comparison.Verdict {
			case qualitycheck.TaskNoDifference, qualitycheck.TaskBetter, qualitycheck.TaskHeld:
				quality[candidate] = "held"
			case qualitycheck.TaskWorse:
				quality[candidate] = "worse"
			case qualitycheck.TaskNotShownHeld:
				quality[candidate] = "unknown: not shown to hold within 1 point"
			default:
				quality[candidate] = "not settled"
			}
			bar[fmt.Sprintf("quality_%s_vs_%s", candidate, window.Control)] = quality[candidate]
		}
		for _, level := range window.Loads {
			trained, okTrained := level.Throughput["trained"]
			published, okPublished := level.Throughput["published"]
			if okTrained && okPublished {
				// Both are judged against the same control in one window; the trained draft is
				// better only when its gain clears the published draft's by both noise floors.
				margin := trained.Delta - published.Delta
				words := "not shown"
				if margin > trained.NoiseFloor+published.NoiseFloor {
					words = "yes"
				}
				bar[fmt.Sprintf("trained_beats_published_at_%d", level.Concurrency)] = fmt.Sprintf("%s (%+.1f%% against %+.1f%% over %s; floors %.1f%% and %.1f%%)", words, trained.Delta*100, published.Delta*100, window.Control, trained.NoiseFloor*100, published.NoiseFloor*100)
			}
			candidates := append([]string(nil), window.Candidates...)
			sort.Strings(candidates)
			for _, candidate := range candidates {
				key := fmt.Sprintf("%s_vs_%s_at_%d", candidate, window.Control, level.Concurrency)
				if reason, invalid := level.Invalid[candidate]; invalid {
					bar[key] = "unknown: " + reason
					continue
				}
				comparison, ok := level.Throughput[candidate]
				if !ok {
					bar[key] = "unknown: not measured"
					continue
				}
				bar[key] = fmt.Sprintf("%s (%+.1f%%, noise floor %.1f%%)", decisionWords[comparison.Decision], comparison.Delta*100, comparison.NoiseFloor*100)
				if candidate == "trained" {
					held := quality[candidate] == "held"
					if window.Tasks == nil {
						held = identityHeld(window, candidate)
					}
					pass := comparison.Decision == optimizationcampaign.DecisionImproved && held
					basis := "the task score held"
					if window.Tasks == nil {
						basis = "its greedy answers matched as often as the control's own second pass"
					}
					bar[fmt.Sprintf("headline_trained_vs_%s_at_%d", window.Control, level.Concurrency)] = map[bool]string{true: "pass: the trained draft is faster than " + window.Control + " and " + basis, false: "not shown"}[pass]
				}
			}
		}
	}
	return bar
}
