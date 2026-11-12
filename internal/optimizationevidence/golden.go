package optimizationevidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The golden suite is a fixed evaluation of the evaluator itself. Every task
// pairs recorded evidence with the decision a careful reviewer would make, so
// a change to qualification, scoring, or the baseline rule that turns a known
// loss into a promotion fails the suite.
const (
	GoldenManifestSchema = "infercrane.dev/optimizer-golden-suite/v1"
	GoldenReportSchema   = "infercrane.dev/optimizer-golden-report/v1"
	maxGoldenManifest    = 1 << 20
	maxGoldenTasks       = 500
)

// Golden task categories. RequiredGoldenCategories lists the situations the
// checked-in suite must keep covering.
const (
	GoldenClearWin         = "clear_win"
	GoldenInsideNoise      = "inside_noise"
	GoldenSLOGate          = "slo_gate"
	GoldenQualityGate      = "quality_gate"
	GoldenBaselineFailsSLO = "baseline_fails_slo"
	GoldenLaptopRehearsal  = "laptop_rehearsal"
	GoldenRecordedH100     = "recorded_h100"
)

var goldenCategories = map[string]bool{
	GoldenClearWin: true, GoldenInsideNoise: true, GoldenSLOGate: true, GoldenQualityGate: true,
	GoldenBaselineFailsSLO: true, GoldenLaptopRehearsal: true, GoldenRecordedH100: true,
}

// RequiredGoldenCategories must each appear in at least one checked-in task.
var RequiredGoldenCategories = []string{GoldenClearWin, GoldenInsideNoise, GoldenSLOGate, GoldenQualityGate, GoldenLaptopRehearsal, GoldenRecordedH100}

type GoldenManifest struct {
	SchemaVersion string       `json:"schema_version"`
	Tasks         []GoldenTask `json:"tasks"`
}

type GoldenTask struct {
	ID string `json:"id"`
	// Evidence is an optimization-evidence/v1 file. A relative path is
	// resolved from the manifest's directory.
	Evidence            string `json:"evidence"`
	BaselineCandidateID string `json:"baseline_candidate_id"`
	ExpectedDecision    string `json:"expected_decision"`
	// ExpectedCandidateID is required for a promote decision and empty otherwise.
	ExpectedCandidateID string `json:"expected_candidate_id,omitempty"`
	// MustNotWin names candidates whose promotion is a false win even in a
	// task that expects some promotion. In keep_baseline and reject_all tasks
	// every promotion is a false win.
	MustNotWin []string `json:"must_not_win,omitempty"`
	Categories []string `json:"categories"`
	// Why records the reviewer's reason for the expected decision, worked out
	// from the evidence rather than from the evaluator's output.
	Why string `json:"why"`
}

type GoldenReport struct {
	SchemaVersion             string             `json:"schema_version"`
	EvaluatorAlgorithm        string             `json:"evaluator_algorithm"`
	BaselineAlgorithm         string             `json:"baseline_algorithm"`
	Tasks                     int                `json:"tasks"`
	CorrectDecisions          int                `json:"correct_decisions"`
	ExpectedPromotions        int                `json:"expected_promotions"`
	CorrectPromotions         int                `json:"correct_promotions"`
	WinRate                   *float64           `json:"win_rate,omitempty"`
	FalseWins                 int                `json:"false_wins"`
	MissedWins                int                `json:"missed_wins"`
	MedianGainOverBaseline    *float64           `json:"median_gain_over_baseline,omitempty"`
	PromotionsWithKnownGain   int                `json:"promotions_with_known_gain"`
	PromotionsWithUnknownGain int                `json:"promotions_with_unknown_gain"`
	GPUHoursPerAnswer         *float64           `json:"gpu_hours_per_answer,omitempty"`
	AnswersWithUnknownGPUTime int                `json:"answers_with_unknown_gpu_time"`
	Passed                    bool               `json:"passed"`
	Results                   []GoldenTaskResult `json:"results"`
}

type GoldenTaskResult struct {
	TaskID              string   `json:"task_id"`
	Evidence            string   `json:"evidence"`
	HardwareIdentity    string   `json:"hardware_identity"`
	Objective           string   `json:"objective"`
	BaselineCandidateID string   `json:"baseline_candidate_id"`
	ExpectedDecision    string   `json:"expected_decision"`
	ExpectedCandidateID string   `json:"expected_candidate_id,omitempty"`
	Decision            string   `json:"decision"`
	CandidateID         string   `json:"candidate_id,omitempty"`
	Correct             bool     `json:"correct"`
	FalseWin            bool     `json:"false_win"`
	RelativeGain        *float64 `json:"relative_gain,omitempty"`
	MeasuredGPUHours    *float64 `json:"measured_gpu_hours,omitempty"`
	Reason              string   `json:"reason"`
	Categories          []string `json:"categories"`
}

// LoadGoldenManifest strictly decodes and validates a golden manifest.
func LoadGoldenManifest(path string) (GoldenManifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return GoldenManifest{}, fmt.Errorf("read golden manifest: %w", err)
	}
	if len(body) == 0 || len(body) > maxGoldenManifest {
		return GoldenManifest{}, fmt.Errorf("golden manifest must be between 1 byte and %d bytes", maxGoldenManifest)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest GoldenManifest
	if err = decoder.Decode(&manifest); err != nil {
		return GoldenManifest{}, fmt.Errorf("decode golden manifest: %w", err)
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return GoldenManifest{}, errors.New("golden manifest contains trailing data")
	}
	if err = manifest.Validate(); err != nil {
		return GoldenManifest{}, err
	}
	return manifest, nil
}

func (m GoldenManifest) Validate() error {
	if m.SchemaVersion != GoldenManifestSchema {
		return fmt.Errorf("golden manifest requires schema %s", GoldenManifestSchema)
	}
	if len(m.Tasks) == 0 || len(m.Tasks) > maxGoldenTasks {
		return fmt.Errorf("golden manifest requires 1-%d tasks", maxGoldenTasks)
	}
	seen := map[string]bool{}
	for _, task := range m.Tasks {
		if err := task.validate(); err != nil {
			return fmt.Errorf("golden task %q: %w", task.ID, err)
		}
		if seen[task.ID] {
			return fmt.Errorf("golden task %q is duplicated", task.ID)
		}
		seen[task.ID] = true
	}
	return nil
}

func (t GoldenTask) validate() error {
	if strings.TrimSpace(t.ID) == "" || strings.TrimSpace(t.Evidence) == "" || strings.TrimSpace(t.BaselineCandidateID) == "" || strings.TrimSpace(t.Why) == "" {
		return errors.New("id, evidence, baseline_candidate_id, and why are required")
	}
	switch t.ExpectedDecision {
	case DecisionPromote:
		if t.ExpectedCandidateID == "" || t.ExpectedCandidateID == t.BaselineCandidateID {
			return errors.New("a promote decision names a candidate other than the baseline")
		}
	case DecisionKeepBaseline, DecisionRejectAll:
		if t.ExpectedCandidateID != "" {
			return errors.New("only a promote decision names an expected candidate")
		}
	default:
		return fmt.Errorf("expected decision must be %s, %s, or %s", DecisionPromote, DecisionKeepBaseline, DecisionRejectAll)
	}
	for _, id := range t.MustNotWin {
		if id == t.ExpectedCandidateID || id == t.BaselineCandidateID {
			return errors.New("must_not_win cannot name the expected candidate or the baseline")
		}
	}
	if len(t.Categories) == 0 {
		return errors.New("at least one category is required")
	}
	for _, category := range t.Categories {
		if !goldenCategories[category] {
			return fmt.Errorf("unknown category %q", category)
		}
	}
	return nil
}

// RunGoldenSuite evaluates every task in the manifest with Evaluate.
func RunGoldenSuite(manifestPath string) (GoldenReport, error) {
	return runGoldenSuite(manifestPath, Evaluate)
}

func runGoldenSuite(manifestPath string, evaluate func(Campaign) (Evaluation, error)) (GoldenReport, error) {
	manifest, err := LoadGoldenManifest(manifestPath)
	if err != nil {
		return GoldenReport{}, err
	}
	report := GoldenReport{SchemaVersion: GoldenReportSchema, EvaluatorAlgorithm: AlgorithmVersion, BaselineAlgorithm: BaselineAlgorithmVersion, Tasks: len(manifest.Tasks)}
	directory := filepath.Dir(manifestPath)
	var gains []float64
	var gpuHours float64
	for _, task := range manifest.Tasks {
		result, err := runGoldenTask(directory, task, evaluate)
		if err != nil {
			return GoldenReport{}, fmt.Errorf("golden task %q: %w", task.ID, err)
		}
		if result.Correct {
			report.CorrectDecisions++
		}
		if task.ExpectedDecision == DecisionPromote {
			report.ExpectedPromotions++
			if result.Correct {
				report.CorrectPromotions++
			} else if result.Decision != DecisionPromote {
				report.MissedWins++
			}
		}
		if result.FalseWin {
			report.FalseWins++
		}
		if result.Decision == DecisionPromote {
			if result.RelativeGain != nil {
				gains = append(gains, *result.RelativeGain)
			} else {
				report.PromotionsWithUnknownGain++
			}
		}
		if result.MeasuredGPUHours == nil {
			report.AnswersWithUnknownGPUTime++
		} else {
			gpuHours += *result.MeasuredGPUHours
		}
		report.Results = append(report.Results, result)
	}
	if report.ExpectedPromotions > 0 {
		rate := float64(report.CorrectPromotions) / float64(report.ExpectedPromotions)
		report.WinRate = &rate
	}
	report.PromotionsWithKnownGain = len(gains)
	if len(gains) > 0 {
		value := median(gains)
		report.MedianGainOverBaseline = &value
	}
	if known := report.Tasks - report.AnswersWithUnknownGPUTime; known > 0 {
		value := gpuHours / float64(known)
		report.GPUHoursPerAnswer = &value
	}
	report.Passed = report.CorrectDecisions == report.Tasks && report.FalseWins == 0
	return report, nil
}

func runGoldenTask(directory string, task GoldenTask, evaluate func(Campaign) (Evaluation, error)) (GoldenTaskResult, error) {
	path := filepath.FromSlash(task.Evidence)
	if !filepath.IsAbs(path) {
		path = filepath.Join(directory, path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return GoldenTaskResult{}, fmt.Errorf("read evidence: %w", err)
	}
	campaign, err := Decode(body)
	if err != nil {
		return GoldenTaskResult{}, err
	}
	ids := map[string]bool{}
	for _, candidate := range campaign.Candidates {
		ids[candidate.ID] = true
	}
	for _, id := range append([]string{task.BaselineCandidateID, task.ExpectedCandidateID}, task.MustNotWin...) {
		if id != "" && !ids[id] {
			return GoldenTaskResult{}, fmt.Errorf("candidate %q is not in the evidence", id)
		}
	}
	if declared := campaign.Selection.BaselineCandidateID; declared != "" && declared != task.BaselineCandidateID {
		return GoldenTaskResult{}, fmt.Errorf("evidence declares baseline %q, task declares %q", declared, task.BaselineCandidateID)
	}
	campaign.Selection.BaselineCandidateID = task.BaselineCandidateID
	evaluation, err := evaluate(campaign)
	if err != nil {
		return GoldenTaskResult{}, err
	}
	result := GoldenTaskResult{
		TaskID: task.ID, Evidence: task.Evidence, HardwareIdentity: campaign.HardwareIdentity, Objective: campaign.Selection.Objective,
		BaselineCandidateID: task.BaselineCandidateID, ExpectedDecision: task.ExpectedDecision, ExpectedCandidateID: task.ExpectedCandidateID,
		Categories: task.Categories, MeasuredGPUHours: measuredGPUHours(campaign),
	}
	result.Decision, result.CandidateID, result.RelativeGain, result.Reason = decisionOf(evaluation, task.BaselineCandidateID)
	result.Correct = result.Decision == task.ExpectedDecision && (task.ExpectedDecision != DecisionPromote || result.CandidateID == task.ExpectedCandidateID)
	if result.Decision == DecisionPromote {
		result.FalseWin = task.ExpectedDecision != DecisionPromote
		for _, id := range task.MustNotWin {
			result.FalseWin = result.FalseWin || id == result.CandidateID
		}
	}
	return result, nil
}

// decisionOf reads the baseline decision, or derives one from the winner for
// an evaluator that does not report it, so older evaluators can be scored.
func decisionOf(evaluation Evaluation, baseline string) (string, string, *float64, string) {
	if decision := evaluation.BaselineDecision; decision != nil {
		var gain *float64
		if decision.Outcome == DecisionPromote {
			gain = decision.RelativeGain
		}
		return decision.Outcome, decision.CandidateID, gain, decision.Reason
	}
	switch evaluation.WinnerID {
	case "":
		return DecisionRejectAll, "", nil, "no candidate qualified"
	case baseline:
		return DecisionKeepBaseline, baseline, nil, "the baseline scored best"
	default:
		var gain *float64
		for _, candidate := range evaluation.Candidates {
			if candidate.CandidateID == evaluation.WinnerID {
				gain = candidate.RelativeGainOverBaseline
			}
		}
		return DecisionPromote, evaluation.WinnerID, gain, "the winner is not the baseline"
	}
}

// measuredGPUHours is the GPU time inside the measured windows of every lane
// of every candidate. It excludes provisioning, model loading, and warmup, so
// it is a lower bound on what the answer cost. A lane without successful
// requests or GPU seconds makes the total unknown.
func measuredGPUHours(campaign Campaign) *float64 {
	var seconds float64
	for _, candidate := range campaign.Candidates {
		for _, lane := range candidate.Lanes {
			if lane.SuccessfulRequests == 0 || lane.GPUSecondsPerSuccessfulRequest <= 0 {
				return nil
			}
			seconds += lane.GPUSecondsPerSuccessfulRequest * float64(lane.SuccessfulRequests)
		}
	}
	hours := seconds / 3600
	return &hours
}

func median(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}
