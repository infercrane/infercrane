package optimizer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// The planning golden suite fixes what the planner proposes for a recorded
// diagnosis. Each task pairs a request (with its diagnosis) with the order a
// careful reviewer would measure setups in, so a rule change that puts a
// known loser first, or drops the change most likely to win, fails the suite.
const (
	PlanningGoldenSchema       = "infercrane.dev/optimizer-planning-golden/v1"
	PlanningGoldenReportSchema = "infercrane.dev/optimizer-planning-golden-report/v1"
	maxPlanningGoldenBytes     = 1 << 20
	maxPlanningGoldenTasks     = 200
)

// GoldenOrderControl is the order entry for the starting setup.
const GoldenOrderControl = "control"

// goldenNeededEvidence names the evidence a plan can ask for, by key.
var goldenNeededEvidence = map[string]string{"fresh_traffic": NeededFreshTraffic, "engine_metrics": NeededEngineMetrics, "fewer_errors": NeededFewerErrors}

// Planning golden categories. RequiredPlanningCategories must each appear in
// the checked-in suite.
const (
	PlanningKVPressure            = "kv_pressure"
	PlanningQueueing              = "queueing"
	PlanningHostBound             = "host_bound"
	PlanningPrefill               = "prefill"
	PlanningPrefixReuse           = "prefix_reuse"
	PlanningDecodeLowConcurrency  = "decode_low_concurrency"
	PlanningDecodeHighConcurrency = "decode_high_concurrency"
	PlanningGPUProfile            = "gpu_profile"
	PlanningNotReady              = "not_ready"
	PlanningReliability           = "reliability"
	PlanningRecordedGPURun        = "recorded_gpu_run"
	PlanningPublisherCheckpoint   = "publisher_checkpoint"
	PlanningPinnedImageSetups     = "pinned_image_setups"
)

// RequiredPlanningCategories lists every situation the suite must cover.
var RequiredPlanningCategories = []string{
	PlanningKVPressure, PlanningQueueing, PlanningHostBound, PlanningPrefill, PlanningPrefixReuse,
	PlanningDecodeLowConcurrency, PlanningDecodeHighConcurrency, PlanningGPUProfile, PlanningNotReady,
	PlanningReliability, PlanningRecordedGPURun, PlanningPublisherCheckpoint, PlanningPinnedImageSetups,
}

type PlanningGoldenManifest struct {
	SchemaVersion string               `json:"schema_version"`
	Tasks         []PlanningGoldenTask `json:"tasks"`
}

type PlanningGoldenTask struct {
	ID      string  `json:"id"`
	Request Request `json:"request"`
	// ExpectedState is the diagnosis plan state.
	ExpectedState string `json:"expected_state"`
	// ExpectedOrder lists every proposed candidate by rank: "control", then
	// technique identifiers.
	ExpectedOrder []string `json:"expected_order"`
	// MustNotPropose names techniques no candidate may apply.
	MustNotPropose []string `json:"must_not_propose,omitempty"`
	// MustNotUseArgs are argument fragments no candidate other than the
	// control may carry, such as a method that lost on recorded hardware.
	MustNotUseArgs []string `json:"must_not_use_args,omitempty"`
	// ExpectedSkipped names chosen techniques the plan must record as skipped.
	ExpectedSkipped []string `json:"expected_skipped,omitempty"`
	// ExpectedNeededEvidence names, by key, exactly the evidence the plan
	// must ask for: fresh_traffic, engine_metrics, or fewer_errors.
	ExpectedNeededEvidence []string `json:"expected_needed_evidence,omitempty"`
	Categories             []string `json:"categories"`
	// Why records the reviewer's reason for the expected order, worked out
	// from the diagnosis rather than from the planner's output.
	Why string `json:"why"`
}

type PlanningGoldenReport struct {
	SchemaVersion    string                 `json:"schema_version"`
	PlannerAlgorithm string                 `json:"planner_algorithm"`
	RulesVersion     string                 `json:"rules_version"`
	Tasks            int                    `json:"tasks"`
	Correct          int                    `json:"correct"`
	Passed           bool                   `json:"passed"`
	Results          []PlanningGoldenResult `json:"results"`
}

type PlanningGoldenResult struct {
	TaskID        string   `json:"task_id"`
	ExpectedState string   `json:"expected_state"`
	State         string   `json:"state"`
	ExpectedOrder []string `json:"expected_order"`
	Order         []string `json:"order"`
	Correct       bool     `json:"correct"`
	Problems      []string `json:"problems"`
	Categories    []string `json:"categories"`
}

// IsPlanningGoldenManifest reports whether a manifest declares the planning
// schema, so one command can run either golden suite.
func IsPlanningGoldenManifest(body []byte) bool {
	var header struct {
		SchemaVersion string `json:"schema_version"`
	}
	return json.Unmarshal(body, &header) == nil && header.SchemaVersion == PlanningGoldenSchema
}

// LoadPlanningGoldenManifest strictly decodes and validates a manifest.
func LoadPlanningGoldenManifest(path string) (PlanningGoldenManifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return PlanningGoldenManifest{}, fmt.Errorf("read planning golden manifest: %w", err)
	}
	if len(body) == 0 || len(body) > maxPlanningGoldenBytes {
		return PlanningGoldenManifest{}, fmt.Errorf("planning golden manifest must be between 1 byte and %d bytes", maxPlanningGoldenBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest PlanningGoldenManifest
	if err = decoder.Decode(&manifest); err != nil {
		return PlanningGoldenManifest{}, fmt.Errorf("decode planning golden manifest: %w", err)
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return PlanningGoldenManifest{}, errors.New("planning golden manifest contains trailing data")
	}
	return manifest, manifest.Validate()
}

func (m PlanningGoldenManifest) Validate() error {
	if m.SchemaVersion != PlanningGoldenSchema {
		return fmt.Errorf("planning golden manifest requires schema %s", PlanningGoldenSchema)
	}
	if len(m.Tasks) == 0 || len(m.Tasks) > maxPlanningGoldenTasks {
		return fmt.Errorf("planning golden manifest requires 1-%d tasks", maxPlanningGoldenTasks)
	}
	seen := map[string]bool{}
	for _, task := range m.Tasks {
		if err := task.validate(); err != nil {
			return fmt.Errorf("planning golden task %q: %w", task.ID, err)
		}
		if seen[task.ID] {
			return fmt.Errorf("planning golden task %q is duplicated", task.ID)
		}
		seen[task.ID] = true
	}
	return nil
}

func (t PlanningGoldenTask) validate() error {
	if strings.TrimSpace(t.ID) == "" || strings.TrimSpace(t.Why) == "" || t.Request.Diagnosis == nil {
		return errors.New("id, why, and a request with a diagnosis are required")
	}
	if len(t.ExpectedOrder) == 0 || t.ExpectedOrder[0] != GoldenOrderControl {
		return errors.New("expected order must start with the control")
	}
	for _, entry := range append(append(append([]string{}, t.ExpectedOrder[1:]...), t.MustNotPropose...), t.ExpectedSkipped...) {
		if _, known := techniques[entry]; !known {
			return fmt.Errorf("unknown technique %q", entry)
		}
	}
	for _, key := range t.ExpectedNeededEvidence {
		if goldenNeededEvidence[key] == "" {
			return fmt.Errorf("unknown needed evidence %q", key)
		}
	}
	for _, forbidden := range t.MustNotPropose {
		if contains(t.ExpectedOrder, forbidden) {
			return fmt.Errorf("technique %q is both expected and forbidden", forbidden)
		}
	}
	switch t.ExpectedState {
	case DiagnosisApplied, DiagnosisNotReady, DiagnosisReliabilityBlocked, DiagnosisNoActionableFinding, DiagnosisNoStartingSetup:
	default:
		return fmt.Errorf("unknown expected state %q", t.ExpectedState)
	}
	if len(t.Categories) == 0 {
		return errors.New("at least one category is required")
	}
	for _, category := range t.Categories {
		if !contains(RequiredPlanningCategories, category) {
			return fmt.Errorf("unknown category %q", category)
		}
	}
	return nil
}

// RunPlanningGolden proposes every task with the source and compares the
// candidate order, plan state, skipped techniques, and forbidden changes.
func RunPlanningGolden(ctx context.Context, path string, source CatalogSource) (PlanningGoldenReport, error) {
	manifest, err := LoadPlanningGoldenManifest(path)
	if err != nil {
		return PlanningGoldenReport{}, err
	}
	report := PlanningGoldenReport{SchemaVersion: PlanningGoldenReportSchema, PlannerAlgorithm: AlgorithmVersion, RulesVersion: DiagnosisRulesVersion, Tasks: len(manifest.Tasks)}
	for _, task := range manifest.Tasks {
		proposal, proposeErr := source.Propose(ctx, task.Request)
		if proposeErr != nil {
			return PlanningGoldenReport{}, fmt.Errorf("planning golden task %q: %w", task.ID, proposeErr)
		}
		if validateErr := ValidateProposal(proposal); validateErr != nil && len(proposal.Candidates) > 0 {
			return PlanningGoldenReport{}, fmt.Errorf("planning golden task %q: proposal failed validation: %w", task.ID, validateErr)
		}
		result := scorePlanningTask(task, proposal)
		if result.Correct {
			report.Correct++
		}
		report.Results = append(report.Results, result)
	}
	report.Passed = report.Correct == report.Tasks
	return report, nil
}

func scorePlanningTask(task PlanningGoldenTask, proposal Proposal) PlanningGoldenResult {
	result := PlanningGoldenResult{TaskID: task.ID, ExpectedState: task.ExpectedState, ExpectedOrder: task.ExpectedOrder, Order: []string{}, Problems: []string{}, Categories: task.Categories}
	if proposal.Diagnosis != nil {
		result.State = proposal.Diagnosis.State
	}
	if result.State != task.ExpectedState {
		result.Problems = append(result.Problems, fmt.Sprintf("plan state %q, expected %q", result.State, task.ExpectedState))
	}
	applied := map[string]bool{}
	for index, candidate := range proposal.Candidates {
		entry := "unreasoned"
		if reason := candidate.Reason; reason != nil {
			entry = reason.Technique
			if reason.Role == ReasonControl {
				entry = GoldenOrderControl
			}
		}
		result.Order = append(result.Order, entry)
		for _, id := range classifyCandidate(candidate) {
			applied[id] = true
		}
		if index == 0 {
			continue
		}
		args := strings.ToLower(strings.Join(append(append([]string{}, candidate.Deployment.Runtime.Args...), candidate.Deployment.Runtime.Workload.Command...), " "))
		for _, fragment := range task.MustNotUseArgs {
			if strings.Contains(args, strings.ToLower(fragment)) {
				result.Problems = append(result.Problems, fmt.Sprintf("candidate %d carries forbidden argument %q", index+1, fragment))
			}
		}
	}
	if strings.Join(result.Order, ",") != strings.Join(task.ExpectedOrder, ",") {
		result.Problems = append(result.Problems, fmt.Sprintf("order %v, expected %v", result.Order, task.ExpectedOrder))
	}
	for _, forbidden := range task.MustNotPropose {
		if applied[forbidden] {
			result.Problems = append(result.Problems, fmt.Sprintf("proposed forbidden technique %q", forbidden))
		}
	}
	for _, expected := range task.ExpectedSkipped {
		skipped := false
		if proposal.Diagnosis != nil {
			for _, planned := range proposal.Diagnosis.Techniques {
				skipped = skipped || planned.Technique == expected && planned.Skipped != "" && len(planned.CandidateIDs) == 0
			}
		}
		if !skipped {
			result.Problems = append(result.Problems, fmt.Sprintf("technique %q was not recorded as skipped", expected))
		}
	}
	var needed []string
	if proposal.Diagnosis != nil {
		needed = proposal.Diagnosis.NeededEvidence
	}
	want := make([]string, 0, len(task.ExpectedNeededEvidence))
	for _, key := range task.ExpectedNeededEvidence {
		want = append(want, goldenNeededEvidence[key])
	}
	if strings.Join(needed, "|") != strings.Join(want, "|") {
		result.Problems = append(result.Problems, fmt.Sprintf("needed evidence %q, expected %q", needed, want))
	}
	result.Correct = len(result.Problems) == 0
	return result
}
